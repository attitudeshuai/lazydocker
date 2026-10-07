package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
)

// statsStreamer is the slice of the docker client that the stats manager
// depends on. *client.Client satisfies it, and tests substitute a fake.
type statsStreamer interface {
	ContainerStats(ctx context.Context, containerID string, stream bool) (container.StatsResponseReader, error)
}

// StatsState is the observable lifecycle state of a single container's stats
// collection.
type StatsState string

const (
	// StatsStateIdle means collection has never been started for the container.
	StatsStateIdle StatsState = "idle"
	// StatsStateConnecting means a connection to the daemon stats stream is
	// being established.
	StatsStateConnecting StatsState = "connecting"
	// StatsStateRunning means the stream is connected and samples arrive.
	StatsStateRunning StatsState = "running"
	// StatsStateRetrying means the stream was interrupted (or could not be
	// established) and the collector is waiting to reconnect with backoff.
	StatsStateRetrying StatsState = "retrying"
	// StatsStatePaused means collection was manually paused. No goroutine and
	// no daemon connection remain; collected history is retained.
	StatsStatePaused StatsState = "paused"
	// StatsStateStopped means collection was manually stopped (or the manager
	// was closed). No goroutine and no daemon connection remain; collected
	// history is retained.
	StatsStateStopped StatsState = "stopped"
)

// StatsStatus is an observable snapshot of one collector's state.
type StatsStatus struct {
	ContainerID    string
	State          StatsState
	Attempts       int
	LastError      string
	NextRetryAt    time.Time
	LastReceivedAt time.Time
	StartedAt      time.Time
}

// StatsScopeKind selects the granularity at which a stats lifecycle operation
// or aggregation runs.
type StatsScopeKind int

const (
	// ScopeContainer targets a single container by ID.
	ScopeContainer StatsScopeKind = iota
	// ScopeService targets every non-one-off container of one service.
	ScopeService
	// ScopeProject targets every container of one compose project.
	ScopeProject
	// ScopeAll targets every known container.
	ScopeAll
)

// StatsScope describes a set of containers for stats operations.
type StatsScope struct {
	Kind        StatsScopeKind
	ContainerID string
	ProjectName string
	ServiceName string
}

// ContainerStatsScope scopes lifecycle operations to a single container.
func ContainerStatsScope(containerID string) StatsScope {
	return StatsScope{Kind: ScopeContainer, ContainerID: containerID}
}

// ServiceStatsScope scopes lifecycle operations to all containers of a service.
func ServiceStatsScope(projectName, serviceName string) StatsScope {
	return StatsScope{Kind: ScopeService, ProjectName: projectName, ServiceName: serviceName}
}

// ProjectStatsScope scopes lifecycle operations to all containers of a project.
func ProjectStatsScope(projectName string) StatsScope {
	return StatsScope{Kind: ScopeProject, ProjectName: projectName}
}

// AllStatsScope scopes lifecycle operations to every container.
func AllStatsScope() StatsScope {
	return StatsScope{Kind: ScopeAll}
}

// MatchStatsScope reports whether the container belongs to the scope.
func MatchStatsScope(c *Container, scope StatsScope) bool {
	switch scope.Kind {
	case ScopeContainer:
		return c.ID == scope.ContainerID
	case ScopeService:
		return !c.OneOff && c.ProjectName == scope.ProjectName && c.ServiceName == scope.ServiceName
	case ScopeProject:
		return c.ProjectName == scope.ProjectName
	case ScopeAll:
		return true
	default:
		return false
	}
}

// StatsAggregate is the summary of the latest stats across a set of containers
// (a service or a project). Percentages are summed and averaged across the
// containers currently reporting.
type StatsAggregate struct {
	// Members is the number of containers in the scope.
	Members int
	// Reporting is the number of members that have produced at least one sample.
	Reporting int
	// CollectorStates counts members by current collection state.
	CollectorStates map[StatsState]int

	CPUPercentageSum    float64
	CPUPercentageAvg    float64
	MemoryPercentageSum float64
	MemoryPercentageAvg float64

	MemoryUsage uint64
	MemoryLimit uint64
	NetworkRx   uint64
	NetworkTx   uint64
}

// StatsManager supervises stats collection for all containers. It owns every
// collector goroutine and guarantees at most one live collection per
// container, explicit lifecycle transitions (start/pause/stop/restart at
// container and scope granularity), backoff retries on interruptions and
// clean shutdown with no goroutines or daemon connections left behind.
type StatsManager struct {
	client statsStreamer
	log    *logrus.Entry
	appCfg *config.AppConfig

	rootCtx    context.Context
	cancelRoot context.CancelFunc

	retryInitial time.Duration
	retryMax     time.Duration
	retryFactor  float64
	jitter       float64
	now          func() time.Time
	rng          *rand.Rand

	mu     deadlock.Mutex
	cols   map[string]*statsCollector
	closed bool
	wg     sync.WaitGroup
}

type statsManagerOptions struct {
	retryInitial time.Duration
	retryMax     time.Duration
	retryFactor  float64
	jitter       float64
	now          func() time.Time
}

// newStatsManager builds a manager. The root context is created internally so
// that Close is the single, deterministic shutdown path.
func newStatsManager(client statsStreamer, log *logrus.Entry, appCfg *config.AppConfig, options ...statsManagerOptions) *StatsManager {
	opts := statsManagerOptions{
		retryInitial: time.Second,
		retryMax:     30 * time.Second,
		retryFactor:  2,
		jitter:       0.25,
		now:          time.Now,
	}
	if len(options) > 0 {
		option := options[0]
		if option.retryInitial > 0 {
			opts.retryInitial = option.retryInitial
		}
		if option.retryMax > 0 {
			opts.retryMax = option.retryMax
		}
		if option.retryFactor > 0 {
			opts.retryFactor = option.retryFactor
		}
		if option.jitter >= 0 {
			opts.jitter = option.jitter
		}
		if option.now != nil {
			opts.now = option.now
		}
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())

	return &StatsManager{
		client:       client,
		log:          log,
		appCfg:       appCfg,
		rootCtx:      rootCtx,
		cancelRoot:   cancelRoot,
		retryInitial: opts.retryInitial,
		retryMax:     opts.retryMax,
		retryFactor:  opts.retryFactor,
		jitter:       opts.jitter,
		now:          opts.now,
		rng:          rand.New(rand.NewSource(opts.now().UnixNano())),
		cols:         map[string]*statsCollector{},
	}
}

// Close stops every collector, cancels in-flight connections and waits for all
// collector goroutines to exit. It is safe to call multiple times.
func (m *StatsManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cols := make([]*statsCollector, 0, len(m.cols))
	for _, col := range m.cols {
		cols = append(cols, col)
	}
	m.mu.Unlock()

	m.cancelRoot()

	for _, col := range cols {
		col.mu.Lock()
		col.intent = false
		col.state = StatsStateStopped
		if col.cancel != nil {
			col.cancel()
		}
		col.signalLocked()
		col.mu.Unlock()
	}

	m.wg.Wait()
}

// Ensure starts collection for a container that has never been collected. It
// is driven by the periodic container scan and never overrides a manual pause
// or stop, and never starts a second collection for a container that is
// already collecting.
func (m *StatsManager) Ensure(c *Container) {
	if m == nil || m.isClosed() {
		return
	}

	col := m.getOrCreate(c)

	col.mu.Lock()
	defer col.mu.Unlock()
	if !col.alive && col.state == StatsStateIdle {
		col.intent = true
		col.spawnLocked()
	}
}

// StartContainer starts (or resumes) collection for a single container.
func (m *StatsManager) StartContainer(c *Container) {
	if m.isClosed() {
		return
	}
	m.getOrCreate(c).Start()
}

// PauseContainer pauses collection for a single container.
func (m *StatsManager) PauseContainer(c *Container) {
	if m.isClosed() {
		return
	}
	if col := m.lookup(c.ID); col != nil {
		col.Pause()
	}
}

// StopContainer stops collection for a single container, retaining history.
func (m *StatsManager) StopContainer(c *Container) {
	if m.isClosed() {
		return
	}
	if col := m.lookup(c.ID); col != nil {
		col.Stop()
	}
}

// RestartContainer stops any in-flight collection and starts it afresh.
// History is retained, so the chart continues from where it left off.
func (m *StatsManager) RestartContainer(c *Container) {
	if m.isClosed() {
		return
	}
	m.getOrCreate(c).Restart()
}

func (m *StatsManager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Start starts collection for every container matching the scope. It returns
// the number of targeted containers.
func (m *StatsManager) Start(scope StatsScope, all []*Container) int {
	targets := m.resolve(scope, all)
	for _, c := range targets {
		m.StartContainer(c)
	}
	return len(targets)
}

// Pause pauses collection for every container matching the scope.
func (m *StatsManager) Pause(scope StatsScope, all []*Container) int {
	targets := m.resolve(scope, all)
	for _, c := range targets {
		m.PauseContainer(c)
	}
	return len(targets)
}

// Stop stops collection for every container matching the scope.
func (m *StatsManager) Stop(scope StatsScope, all []*Container) int {
	targets := m.resolve(scope, all)
	for _, c := range targets {
		m.StopContainer(c)
	}
	return len(targets)
}

// Restart restarts collection for every container matching the scope.
func (m *StatsManager) Restart(scope StatsScope, all []*Container) int {
	targets := m.resolve(scope, all)
	for _, c := range targets {
		m.RestartContainer(c)
	}
	return len(targets)
}

// Reconcile stops and forgets collectors whose containers are no longer in the
// active list, i.e. containers that have been removed.
func (m *StatsManager) Reconcile(active []*Container) {
	if m.isClosed() {
		return
	}

	live := make(map[string]struct{}, len(active))
	for _, c := range active {
		live[c.ID] = struct{}{}
	}

	m.mu.Lock()
	stale := []*statsCollector{}
	for id, col := range m.cols {
		if _, ok := live[id]; !ok {
			stale = append(stale, col)
			delete(m.cols, id)
		}
	}
	m.mu.Unlock()

	for _, col := range stale {
		col.Stop()
	}
}

// Status returns the observable state of one container's collector.
func (m *StatsManager) Status(containerID string) (StatsStatus, bool) {
	col := m.lookup(containerID)
	if col == nil {
		return StatsStatus{}, false
	}
	return col.snapshot(), true
}

// Statuses returns snapshots of every collector, ordered by container ID.
func (m *StatsManager) Statuses() []StatsStatus {
	m.mu.Lock()
	cols := make([]*statsCollector, 0, len(m.cols))
	for _, col := range m.cols {
		cols = append(cols, col)
	}
	m.mu.Unlock()

	sort.Slice(cols, func(i, j int) bool { return cols[i].ctr.ID < cols[j].ctr.ID })

	statuses := make([]StatsStatus, len(cols))
	for i, col := range cols {
		statuses[i] = col.snapshot()
	}
	return statuses
}

// Aggregate summarises the latest stats across the containers in a scope.
// Each per-container read is individually consistent, and only existing
// samples are summed, so a half-updated history can never be observed.
func (m *StatsManager) Aggregate(scope StatsScope, all []*Container) StatsAggregate {
	aggregate := StatsAggregate{CollectorStates: map[StatsState]int{}}

	for _, c := range all {
		if !MatchStatsScope(c, scope) {
			continue
		}

		aggregate.Members++
		if status, ok := m.Status(c.ID); ok {
			aggregate.CollectorStates[status.State]++
		} else {
			aggregate.CollectorStates[StatsStateIdle]++
		}

		stats, ok := c.GetLastStats()
		if !ok {
			continue
		}

		aggregate.Reporting++
		aggregate.CPUPercentageSum += stats.DerivedStats.CPUPercentage
		aggregate.MemoryPercentageSum += stats.DerivedStats.MemoryPercentage
		aggregate.MemoryUsage += uint64(stats.ClientStats.MemoryStats.Usage)
		if stats.ClientStats.MemoryStats.Limit > 0 {
			aggregate.MemoryLimit += uint64(stats.ClientStats.MemoryStats.Limit)
		}
		aggregate.NetworkRx += uint64(stats.ClientStats.Networks.Eth0.RxBytes)
		aggregate.NetworkTx += uint64(stats.ClientStats.Networks.Eth0.TxBytes)
	}

	if aggregate.Reporting > 0 {
		aggregate.CPUPercentageAvg = aggregate.CPUPercentageSum / float64(aggregate.Reporting)
		aggregate.MemoryPercentageAvg = aggregate.MemoryPercentageSum / float64(aggregate.Reporting)
	}

	return aggregate
}

func (m *StatsManager) resolve(scope StatsScope, all []*Container) []*Container {
	if scope.Kind == ScopeContainer {
		for _, c := range all {
			if c.ID == scope.ContainerID {
				return []*Container{c}
			}
		}
		// The container may have been removed from the list already; still
		// target a known collector so stop/restart keep working.
		if col := m.lookup(scope.ContainerID); col != nil {
			return []*Container{col.ctr}
		}
		return nil
	}

	targets := []*Container{}
	for _, c := range all {
		if MatchStatsScope(c, scope) {
			targets = append(targets, c)
		}
	}
	return targets
}

func (m *StatsManager) lookup(id string) *statsCollector {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols[id]
}

func (m *StatsManager) getOrCreate(c *Container) *statsCollector {
	m.mu.Lock()
	defer m.mu.Unlock()

	col, ok := m.cols[c.ID]
	if !ok {
		col = newStatsCollector(m, c)
		m.cols[c.ID] = col
	}
	return col
}

func (m *StatsManager) retentionSettings() (time.Duration, []config.RetentionTier) {
	if m.appCfg == nil || m.appCfg.UserConfig == nil {
		return 0, nil
	}

	statsCfg := m.appCfg.UserConfig.Stats
	if statsCfg.Retention == nil {
		return statsCfg.MaxDuration, nil
	}
	return statsCfg.MaxDuration, statsCfg.Retention.Tiers
}

// record turns one raw daemon payload into a RecordedStats and hands it to the
// container's (possibly tiered) history.
func (m *StatsManager) record(c *Container, raw ContainerStats, at time.Time) {
	recorded := &RecordedStats{
		ClientStats: raw,
		DerivedStats: DerivedStats{
			CPUPercentage:    raw.CalculateContainerCPUPercentage(),
			MemoryPercentage: raw.CalculateContainerMemoryUsage(),
		},
		RecordedAt: at,
	}

	maxFineDuration, tiers := m.retentionSettings()
	c.recordStats(recorded, maxFineDuration, tiers)
}

// statsCollector owns the single goroutine that streams stats for one
// container. All state transitions are serialised through its mutex; the
// boolean intent distinguishes "should be running" from "is running", which is
// what lets pause/stop/restart race safely with reconnect attempts.
type statsCollector struct {
	mgr *StatsManager
	ctr *Container

	mu     deadlock.Mutex
	intent bool
	alive  bool
	state  StatsState

	cancel context.CancelFunc
	done   chan struct{} // closed while no goroutine is alive
	wake   chan struct{} // closed to interrupt a backoff sleep; nil otherwise

	attempts   int
	lastErr    error
	nextTry    time.Time
	startedAt  time.Time
	lastRecvAt time.Time

	// immediateReconnect is set by Restart so that a failure/retry cycle
	// already in progress reconnects straight away instead of backing off.
	immediateReconnect bool
}

func newStatsCollector(m *StatsManager, c *Container) *statsCollector {
	return &statsCollector{
		mgr:   m,
		ctr:   c,
		state: StatsStateIdle,
		done:  closedChannel(),
	}
}

func closedChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Start ensures the collector is running, regardless of whether it was idle,
// paused or stopped before.
func (col *statsCollector) Start() {
	col.mu.Lock()
	defer col.mu.Unlock()

	col.intent = true
	if col.alive {
		return
	}
	col.spawnLocked()
}

// Pause halts the collector into the paused state and waits for its goroutine
// and daemon connection to be gone.
func (col *statsCollector) Pause() {
	col.halt(StatsStatePaused)
}

// Stop halts the collector into the stopped state and waits for its goroutine
// and daemon connection to be gone. History is retained.
func (col *statsCollector) Stop() {
	col.halt(StatsStateStopped)
}

func (col *statsCollector) halt(state StatsState) {
	col.mu.Lock()
	col.intent = false
	col.state = state
	col.immediateReconnect = false
	alive := col.alive
	done := col.done
	if col.cancel != nil {
		col.cancel()
	}
	col.signalLocked()
	col.mu.Unlock()

	if alive {
		<-done
	}
}

// Restart reconnects immediately: it cancels the current stream (if any) and
// either kicks the live goroutine into reconnecting or spawns a fresh one. At
// no point do two goroutines exist for the same container.
func (col *statsCollector) Restart() {
	col.mu.Lock()
	defer col.mu.Unlock()

	col.intent = true
	if !col.alive {
		col.spawnLocked()
		return
	}

	col.state = StatsStateConnecting
	col.attempts = 0
	col.lastErr = nil
	col.immediateReconnect = true
	if col.cancel != nil {
		col.cancel()
	}
	col.signalLocked()
}

// spawnLocked starts the single collector goroutine. Caller holds col.mu.
func (col *statsCollector) spawnLocked() {
	col.alive = true
	col.done = make(chan struct{})
	col.state = StatsStateConnecting
	col.attempts = 0
	col.lastErr = nil
	col.immediateReconnect = false
	col.startedAt = col.mgr.now()

	col.mgr.wg.Add(1)
	go col.run()
}

// signalLocked interrupts any in-progress backoff wait. Caller holds col.mu.
func (col *statsCollector) signalLocked() {
	if col.wake != nil {
		close(col.wake)
		col.wake = nil
	}
}

func (col *statsCollector) run() {
	defer col.mgr.wg.Done()

	defer func() {
		if recovered := recover(); recovered != nil {
			col.mgr.log.Errorf("stats collector for container %q panicked: %v", col.ctr.Name, recovered)
			col.mu.Lock()
			col.intent = false
			col.state = StatsStateStopped
			col.mu.Unlock()
		}
		col.finish()
	}()

	backoff := col.mgr.retryInitial

	for {
		ctx, cancel := context.WithCancel(col.mgr.rootCtx)

		col.mu.Lock()
		if !col.intent || col.mgr.rootCtx.Err() != nil {
			cancel()
			col.mu.Unlock()
			return
		}
		col.state = StatsStateConnecting
		col.cancel = cancel
		col.mu.Unlock()

		stream, err := col.mgr.client.ContainerStats(ctx, col.ctr.ID, true)
		if err != nil {
			cancel()
			if !col.waitForRetry(err, &backoff) {
				return
			}
			continue
		}

		// A pause/stop/close may have landed while the dial was in flight.
		col.mu.Lock()
		if !col.intent || col.mgr.rootCtx.Err() != nil {
			col.mu.Unlock()
			cancel()
			_ = stream.Body.Close()
			return
		}
		col.state = StatsStateRunning
		col.attempts = 0
		col.lastErr = nil
		col.mu.Unlock()

		received, readErr := col.readStream(stream.Body)
		_ = stream.Body.Close()
		cancel()

		// A connection that streamed at least one sample was healthy, so the
		// next interruption starts the backoff ladder from the bottom again.
		if received > 0 {
			backoff = col.mgr.retryInitial
		}

		if !col.waitForRetry(readErr, &backoff) {
			return
		}
	}
}

// readStream consumes one daemon stats stream until it ends or the context is
// cancelled. It returns the number of samples recorded and the scanner's
// error (nil on a clean EOF).
func (col *statsCollector) readStream(body io.Reader) (int, error) {
	scanner := bufio.NewScanner(body)
	// Stats payloads can be large on multi-core machines (per-CPU counters),
	// so allow lines well beyond the scanner's 64KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	received := 0
	for scanner.Scan() {
		var raw ContainerStats
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			// A single malformed payload must not kill the stream; behave like
			// the original implementation and skip it.
			col.mgr.log.Debugf("could not parse stats payload for container %q: %v", col.ctr.Name, err)
			continue
		}

		at := col.mgr.now()
		col.mgr.record(col.ctr, raw, at)
		received++

		col.mu.Lock()
		col.lastRecvAt = at
		col.mu.Unlock()
	}

	return received, scanner.Err()
}

// waitForRetry records an interruption, applies exponential backoff (with
// jitter) and waits. It returns false when the collector should exit instead
// of reconnecting (paused, stopped or manager closed); history is untouched
// across retries so a successful reconnect continues where it left off.
func (col *statsCollector) waitForRetry(err error, backoff *time.Duration) bool {
	col.mu.Lock()
	if !col.intent || col.mgr.rootCtx.Err() != nil {
		col.mu.Unlock()
		return false
	}

	// An explicit restart interrupted the stream: reconnect immediately and
	// restart the backoff ladder.
	if col.immediateReconnect {
		col.immediateReconnect = false
		col.state = StatsStateConnecting
		col.attempts = 0
		col.lastErr = nil
		col.nextTry = time.Time{}
		col.wake = nil
		*backoff = col.mgr.retryInitial
		col.mu.Unlock()
		return true
	}

	col.state = StatsStateRetrying
	col.attempts++
	col.lastErr = err

	delay := *backoff
	if col.mgr.jitter > 0 && delay > 0 {
		factor := 1 + (2*col.mgr.rng.Float64()-1)*col.mgr.jitter
		delay = time.Duration(float64(delay) * factor)
	}
	col.nextTry = col.mgr.now().Add(delay)

	wake := make(chan struct{})
	col.wake = wake
	col.mu.Unlock()

	// Grow the backoff for the next failed attempt, capped at retryMax.
	next := time.Duration(float64(*backoff) * col.mgr.retryFactor)
	if next > col.mgr.retryMax || next <= 0 {
		next = col.mgr.retryMax
	}
	*backoff = next

	timer := time.NewTimer(delay)
	select {
	case <-col.mgr.rootCtx.Done():
		timer.Stop()
		return false
	case <-wake:
		timer.Stop()
	case <-timer.C:
	}

	col.mu.Lock()
	defer col.mu.Unlock()
	if !col.intent || col.mgr.rootCtx.Err() != nil {
		return false
	}
	col.wake = nil

	// Woken by an explicit restart while sleeping: reconnect straight away.
	if col.immediateReconnect {
		col.immediateReconnect = false
		col.state = StatsStateConnecting
		col.nextTry = time.Time{}
		*backoff = col.mgr.retryInitial
	}
	return true
}

// finish marks the goroutine as gone. Callers that requested pause/stop/close
// have already set the target state; finish must not overwrite it.
func (col *statsCollector) finish() {
	col.mu.Lock()
	defer col.mu.Unlock()

	if col.alive {
		close(col.done)
		col.alive = false
	}
	col.cancel = nil
	col.wake = nil
}

func (col *statsCollector) snapshot() StatsStatus {
	col.mu.Lock()
	defer col.mu.Unlock()

	status := StatsStatus{
		ContainerID:    col.ctr.ID,
		State:          col.state,
		Attempts:       col.attempts,
		NextRetryAt:    col.nextTry,
		LastReceivedAt: col.lastRecvAt,
		StartedAt:      col.startedAt,
	}
	if col.lastErr != nil {
		status.LastError = col.lastErr.Error()
	}
	return status
}
