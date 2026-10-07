package gui

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"time"

	"github.com/boz/go-throttle"
	"github.com/docker/docker/api/types/events"
	"github.com/jesseduffield/gocui"
	"github.com/sirupsen/logrus"
)

const (
	// reconcileThrottlePeriod coalesces bursts of events for one panel, just
	// like the old global refresh throttle, but the panel is marked stale
	// before throttling so an event lost to the throttle is still accounted
	// for by the dirty check below.
	reconcileThrottlePeriod = 50 * time.Millisecond

	// reconcileRetryInterval is the delay before a panel whose fetch failed
	// tries again, so that a temporarily unreachable docker daemon doesn't
	// leave the panel stale forever.
	reconcileRetryInterval = 2 * time.Second

	// fallbackFullRefreshInterval drives a full refresh while the docker
	// event subscription is unavailable, matching the old "refetch
	// everything" behaviour on a timer.
	fallbackFullRefreshInterval = 5 * time.Second
)

// reconcileKey identifies one independently reconciled fetch unit. Containers
// and services share a unit because they come from a single docker call;
// projects are a separate unit that depends on containers because projects
// are derived from container labels.
type reconcileKey string

const (
	reconcileContainers reconcileKey = "containers"
	reconcileProjects   reconcileKey = "projects"
	reconcileImages     reconcileKey = "images"
	reconcileVolumes    reconcileKey = "volumes"
	reconcileNetworks   reconcileKey = "networks"
)

type reconcileState int

const (
	// stateStale means the panel may be out of date: either it has never
	// been fetched, events marked it dirty, or the last fetch failed/was
	// superseded. A reconciliation is pending.
	stateStale reconcileState = iota
	// stateReconciling means a fetch for this panel is currently in flight.
	stateReconciling
	// stateFresh means the view reflects the last successfully completed
	// fetch and no event has invalidated it since.
	stateFresh
)

// errStaleSnapshot is returned by the apply callback when the fetch it
// belongs to has been superseded by a newer reconciliation request. It
// signals that the snapshot must never touch the view.
var errStaleSnapshot = stderrors.New("reconciliation snapshot is stale")

// applyFunc runs the given view mutation synchronously, on the gui main
// loop. It must not return until the mutation has finished (or failed).
type applyFunc func(mutate func() error) error

// reconcileJob performs a single reconciliation for a unit: fetch the
// freshest data from docker, then invoke apply with the view mutation. If
// apply refuses the mutation (errStaleSnapshot) the fetched snapshot is
// discarded without touching the view.
type reconcileJob func(apply func(mutate func() error) error) error

type reconcileUnitDef struct {
	key   reconcileKey
	views []*gocui.View
	job   reconcileJob
	// after a successful reconciliation of this unit, those units are
	// requested too (e.g. projects are derived from containers).
	triggers []reconcileKey
}

type unitStatus struct {
	key         reconcileKey
	views       []*gocui.View
	state       reconcileState
	lastSuccess time.Time
	lastError   error
}

type reconcileUnit struct {
	def    reconcileUnitDef
	job    reconcileJob
	signal chan struct{}

	throttler throttle.ThrottleDriver

	mu          sync.Mutex
	state       reconcileState
	epoch       uint64
	dirty       bool
	lastSuccess time.Time
	lastError   error
	waiters     []chan error
}

// reconcileManager owns per-panel fetch serialization. Each unit has a
// single worker so fetches for the same panel can never overlap; an epoch
// counter makes sure a fetch that was overtaken by newer events cannot
// overwrite newer data; the dirty flag makes sure every event is eventually
// reconciled exactly once in order.
type reconcileManager struct {
	log            *logrus.Entry
	apply          applyFunc
	onStatusChange func()
	retryInterval  time.Duration
	throttlePeriod time.Duration
	units          map[reconcileKey]*reconcileUnit
	orderedUnits   []reconcileKey
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

func newReconcileManager(log *logrus.Entry, defs []reconcileUnitDef) *reconcileManager {
	ctx, cancel := context.WithCancel(context.Background())

	m := &reconcileManager{
		log:            log,
		onStatusChange: func() {},
		retryInterval:  reconcileRetryInterval,
		throttlePeriod: reconcileThrottlePeriod,
		units:          make(map[reconcileKey]*reconcileUnit, len(defs)),
		orderedUnits:   make([]reconcileKey, 0, len(defs)),
		ctx:            ctx,
		cancel:         cancel,
	}

	for _, def := range defs {
		u := &reconcileUnit{
			def:    def,
			job:    def.job,
			signal: make(chan struct{}, 1),
			state:  stateStale,
		}
		u.throttler = throttle.ThrottleFunc(m.throttlePeriod, true, func() {
			m.kick(u)
		})
		m.units[def.key] = u
		m.orderedUnits = append(m.orderedUnits, def.key)
	}

	return m
}

func (m *reconcileManager) start() {
	for _, key := range m.orderedUnits {
		m.wg.Add(1)
		go func(u *reconcileUnit) {
			defer m.wg.Done()
			m.runWorker(u)
		}(m.units[key])
	}
}

func (m *reconcileManager) stop() {
	for _, key := range m.orderedUnits {
		m.units[key].throttler.Stop()
	}
	m.cancel()

	// Don't hang shutdown on a docker call that's blocked on a dead daemon:
	// wait briefly, then let the process exit; cancelled workers bail out of
	// their next wait and in-flight applies are no-ops once the gui is gone.
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
}

func (m *reconcileManager) kick(u *reconcileUnit) {
	select {
	case u.signal <- struct{}{}:
	default:
	}
}

// Request marks the unit stale (its data must be reconciled before it can be
// considered fresh) and schedules one coalesced fetch. Safe to call from any
// goroutine, including in tight bursts from the event stream.
func (m *reconcileManager) Request(key reconcileKey) {
	u, ok := m.units[key]
	if !ok {
		return
	}

	u.mu.Lock()
	previousState := u.state
	u.epoch++
	u.dirty = true
	if u.state == stateFresh {
		u.state = stateStale
	}
	changed := u.state != previousState
	u.mu.Unlock()

	// Only repaint on a state transition; while a panel is already stale a
	// burst of events must not produce a burst of ui updates (the periodic
	// status render keeps timestamps/spinners moving).
	if changed {
		m.onStatusChange()
	}
	u.throttler.Trigger()
}

// RequestAll marks every unit stale and refetches everything. Used for the
// initial load, subscription recovery (events during the gap were missed)
// and the fallback timer while the subscription is down.
func (m *reconcileManager) RequestAll() {
	for _, key := range m.orderedUnits {
		m.Request(key)
	}
}

// RequestSync behaves like Request but blocks until the requested
// reconciliation has been applied (or the context is cancelled). Callers
// that mutate docker state (e.g. pruning) use it to keep their
// refresh-and-continue behaviour.
func (m *reconcileManager) RequestSync(ctx context.Context, key reconcileKey) error {
	u, ok := m.units[key]
	if !ok {
		return nil
	}

	done := make(chan error, 1)

	u.mu.Lock()
	previousState := u.state
	u.epoch++
	u.dirty = true
	if u.state == stateFresh {
		u.state = stateStale
	}
	changed := u.state != previousState
	u.waiters = append(u.waiters, done)
	u.mu.Unlock()

	if changed {
		m.onStatusChange()
	}
	u.throttler.Trigger()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// MarkAllStale flags every unit as needing reconciliation without
// scheduling a fetch by itself. Used the moment the event subscription is
// lost: the panels can show that their data may be out of date before the
// recovery/full refresh actually runs.
func (m *reconcileManager) MarkAllStale() {
	changed := false
	for _, key := range m.orderedUnits {
		u := m.units[key]
		u.mu.Lock()
		if u.state == stateFresh {
			u.state = stateStale
			changed = true
		}
		u.dirty = true
		u.mu.Unlock()
	}
	if changed {
		m.onStatusChange()
	}
}

func (m *reconcileManager) Statuses() []unitStatus {
	statuses := make([]unitStatus, 0, len(m.orderedUnits))
	for _, key := range m.orderedUnits {
		u := m.units[key]
		u.mu.Lock()
		statuses = append(statuses, unitStatus{
			key:         key,
			views:       u.def.views,
			state:       u.state,
			lastSuccess: u.lastSuccess,
			lastError:   u.lastError,
		})
		u.mu.Unlock()
	}
	return statuses
}

func (m *reconcileManager) runWorker(u *reconcileUnit) {
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-u.signal:
		}

		for {
			u.mu.Lock()
			if !u.dirty {
				u.mu.Unlock()
				break
			}
			u.dirty = false
			requestEpoch := u.epoch
			u.state = stateReconciling
			u.mu.Unlock()
			m.onStatusChange()

			var applyErr error
			fetchErr := u.job(func(mutate func() error) error {
				// Epoch guard: a newer request arrived while we were
				// fetching, so this snapshot is older than what the panel
				// is already waiting for. Never let it touch the view.
				u.mu.Lock()
				currentEpoch := u.epoch
				u.mu.Unlock()
				if currentEpoch != requestEpoch {
					applyErr = errStaleSnapshot
					return errStaleSnapshot
				}

				applyErr = m.apply(mutate)
				return applyErr
			})
			if fetchErr == nil {
				fetchErr = applyErr
			}

			u.mu.Lock()
			superseded := u.epoch != requestEpoch
			stale := isStaleSnapshot(fetchErr)

			// On a stale-snapshot skip the request is served by the next
			// round, so waiters stay attached until an applied round.
			var resolveWaiters []chan error
			if !stale {
				resolveWaiters = u.waiters
				u.waiters = nil
			}

			retry := false
			switch {
			case stale:
				// Fetch was overtaken by newer events: discard it and
				// refetch in order. The view keeps its previous complete
				// snapshot.
				u.state = stateStale
				u.dirty = true
			case fetchErr != nil:
				// Fetch failed: the panel is not fresh and must be retried.
				u.state = stateStale
				u.lastError = fetchErr
				u.dirty = true
				retry = true
				m.log.Warnf("reconciliation of %s failed: %s", u.def.key, fetchErr.Error())
			case superseded:
				// A newer request slipped in while the snapshot was being
				// applied. The applied snapshot was complete, but it can't
				// be considered fresh: run another round immediately.
				u.state = stateStale
				u.dirty = true
			default:
				u.state = stateFresh
				u.lastSuccess = time.Now()
				u.lastError = nil
			}
			triggers := u.def.triggers
			stillDirty := u.dirty
			u.mu.Unlock()

			for _, waiter := range resolveWaiters {
				waiter <- fetchErr
			}

			if retry {
				time.AfterFunc(m.retryInterval, func() { m.kick(u) })
			}

			// Dependent units (projects) only get re-read once their source
			// unit (containers) has applied fresh data.
			if fetchErr == nil && !stale && !superseded {
				for _, triggered := range triggers {
					m.Request(triggered)
				}
			}

			m.onStatusChange()

			// On fetch error we don't immediately loop: the retry timer is
			// the only thing allowed to kick the next round, otherwise a
			// dead daemon would be hit in a tight loop.
			if !stillDirty || retry {
				break
			}
		}
	}
}

func isStaleSnapshot(err error) bool {
	return stderrors.Is(err, errStaleSnapshot)
}

// reconcileKeysForEvent classifies a docker event and returns the fetch
// units whose data it can change. Services and projects are derived from
// containers so they move with the containers unit. A nil result means the
// event changes nothing shown in the panels.
func reconcileKeysForEvent(msg events.Message) []reconcileKey {
	switch msg.Type {
	case events.ContainerEventType:
		return keysForContainerAction(msg.Action)
	case events.ImageEventType:
		return []reconcileKey{reconcileImages}
	case events.VolumeEventType:
		return []reconcileKey{reconcileVolumes}
	case events.NetworkEventType:
		return []reconcileKey{reconcileNetworks}
	case events.DaemonEventType:
		// a daemon reload can change anything, so reconcile everything
		return allReconcileKeys()
	default:
		// unknown/unmapped event types (builder, swarm objects, configs,
		// plugins, ...): prefer correctness over saving fetches
		return allReconcileKeys()
	}
}

func keysForContainerAction(action events.Action) []reconcileKey {
	actionStr := string(action)

	switch {
	case strings.HasPrefix(actionStr, "exec_"):
		return nil
	case strings.HasPrefix(actionStr, string(events.ActionHealthStatus)):
		return nil
	}

	switch action {
	// lifecycle / inventory changes
	case events.ActionCreate,
		events.ActionStart,
		events.ActionStop,
		events.ActionRestart,
		events.ActionPause,
		events.ActionUnPause,
		events.ActionKill,
		events.ActionDie,
		events.ActionOOM,
		events.ActionDestroy,
		events.ActionRemove,
		events.ActionRename,
		events.ActionUpdate:
		return []reconcileKey{reconcileContainers}
	case events.ActionCommit:
		// committing a container also creates a new image
		return []reconcileKey{reconcileContainers, reconcileImages}
	default:
		// attach/detach/resize/top/copy/export/checkpoint/unknown: no list
		// inventory change
		return nil
	}
}

func allReconcileKeys() []reconcileKey {
	return []reconcileKey{
		reconcileContainers,
		reconcileProjects,
		reconcileImages,
		reconcileVolumes,
		reconcileNetworks,
	}
}
