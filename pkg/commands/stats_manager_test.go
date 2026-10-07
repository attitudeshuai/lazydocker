package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/stretchr/testify/assert"
)

// fakeStatsStreamer simulates the daemon stats endpoint. It tracks how many
// stream connections are currently open (and the high-water mark), can fail
// the first N dial attempts to exercise backoff, and writes a scripted JSON
// payload per connection. Open streams unblock and end as soon as the context
// is cancelled, exactly like a real docker connection.
type fakeStatsStreamer struct {
	dialAttempts int32
	failDials    int32
	bodies       int32

	open     int32
	openPeak int32
	closedOK int32

	script func(containerID string, connection int32) []string
}

func (f *fakeStatsStreamer) noteOpen() {
	open := atomic.AddInt32(&f.open, 1)
	for {
		peak := atomic.LoadInt32(&f.openPeak)
		if open <= peak || atomic.CompareAndSwapInt32(&f.openPeak, peak, open) {
			return
		}
	}
}

type trackedBody struct {
	io.Reader
	once    sync.Once
	onClose func()
}

func (b *trackedBody) Close() error {
	b.once.Do(b.onClose)
	return nil
}

func (f *fakeStatsStreamer) ContainerStats(ctx context.Context, containerID string, _ bool) (container.StatsResponseReader, error) {
	attempt := atomic.AddInt32(&f.dialAttempts, 1)
	if attempt <= atomic.LoadInt32(&f.failDials) {
		return container.StatsResponseReader{}, errors.New("daemon unavailable")
	}

	pr, pw := io.Pipe()
	f.noteOpen()
	connection := atomic.AddInt32(&f.bodies, 1)

	var lines []string
	if f.script != nil {
		lines = f.script(containerID, connection)
	}

	go func() {
		for _, line := range lines {
			select {
			case <-ctx.Done():
				_ = pw.CloseWithError(ctx.Err())
				return
			case <-time.After(time.Millisecond):
				if _, err := pw.Write([]byte(line + "\n")); err != nil {
					return
				}
			}
		}
		<-ctx.Done()
		_ = pw.CloseWithError(ctx.Err())
	}()

	body := &trackedBody{
		Reader: pr,
		onClose: func() {
			_ = pr.Close()
			atomic.AddInt32(&f.open, -1)
			atomic.AddInt32(&f.closedOK, 1)
		},
	}
	return container.StatsResponseReader{Body: body}, nil
}

// statsJSON builds a single-line stats payload with the given memory usage
// and CPU counters.
func statsJSON(cpuTotal, preCPUTotal, systemTotal, preSystemTotal int64, memoryUsage int) string {
	return fmt.Sprintf(
		`{"read":"2020-01-01T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d},"precpu_stats":{"cpu_usage":{"total_usage":%d},"system_cpu_usage":%d},"memory_stats":{"usage":%d,"limit":1000},"networks":{"eth0":{"rx_bytes":100,"tx_bytes":200}}}`,
		cpuTotal, systemTotal, preCPUTotal, preSystemTotal, memoryUsage,
	)
}

func newTestManager(t *testing.T, streamer *fakeStatsStreamer) *StatsManager {
	t.Helper()
	appCfg := &config.AppConfig{UserConfig: &config.UserConfig{}}
	appCfg.UserConfig.Stats.MaxDuration = 3 * time.Minute

	manager := newStatsManager(streamer, NewDummyLog(), appCfg, statsManagerOptions{
		retryInitial: time.Millisecond,
		retryMax:     5 * time.Millisecond,
		retryFactor:  2,
		jitter:       0,
	})
	t.Cleanup(manager.Close)
	return manager
}

func testContainer(id, project, service string, oneOff ...bool) *Container {
	return &Container{ID: id, Name: id, ProjectName: project, ServiceName: service, OneOff: len(oneOff) > 0 && oneOff[0]}
}

// waitFor polls until condition holds or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timed out waiting for: %s", message)
	}
}

func historyLen(c *Container) int {
	c.StatsMutex.Lock()
	defer c.StatsMutex.Unlock()
	return len(c.StatHistory)
}

// Ensure must never start two concurrent collectors; a stopped collector must
// not be resurrected by the scan, but an explicit start works. Stop drains the
// goroutine and closes its connection.
func TestStatsManagerEnsureUniquenessAndStop(t *testing.T) {
	streamer := &fakeStatsStreamer{
		script: func(string, int32) []string {
			return []string{statsJSON(10, 0, 100, 0, 100)}
		},
	}
	manager := newTestManager(t, streamer)
	c := testContainer("c1", "", "")

	// Hammer Ensure concurrently: exactly one connection may ever be open.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.Ensure(c)
		}()
	}
	wg.Wait()

	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.open) == 1 }, "one open connection")
	waitFor(t, time.Second, func() bool {
		status, ok := manager.Status(c.ID)
		return ok && status.State == StatsStateRunning
	}, "running state")
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.bodies))
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.openPeak))

	manager.StopContainer(c)

	status, ok := manager.Status(c.ID)
	assert.True(t, ok)
	assert.Equal(t, StatsStateStopped, status.State)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.open) == 0 }, "connection closed after stop")

	// The periodic scan must not resurrect a manually stopped collector.
	manager.Ensure(c)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.bodies))
	status, _ = manager.Status(c.ID)
	assert.Equal(t, StatsStateStopped, status.State)

	// ... but an explicit start reconnects, retaining the previous history.
	manager.StartContainer(c)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.bodies) == 2 }, "explicit restart opens new connection")
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.openPeak))
}

// Pause tears down the goroutine and connection; resume reconnects and the
// history continues instead of starting from zero.
func TestStatsManagerPauseResumeKeepsHistory(t *testing.T) {
	streamer := &fakeStatsStreamer{}
	streamer.script = func(_ string, connection int32) []string {
		if connection == 1 {
			return []string{statsJSON(10, 0, 100, 0, 111)}
		}
		return []string{statsJSON(20, 10, 300, 100, 222)}
	}
	manager := newTestManager(t, streamer)
	c := testContainer("c1", "", "")

	manager.StartContainer(c)
	waitFor(t, time.Second, func() bool { return historyLen(c) >= 1 }, "first sample")

	manager.PauseContainer(c)
	status, _ := manager.Status(c.ID)
	assert.Equal(t, StatsStatePaused, status.State)
	assert.Equal(t, int32(0), atomic.LoadInt32(&streamer.open))

	manager.StartContainer(c)
	waitFor(t, time.Second, func() bool { return historyLen(c) >= 2 }, "second sample appended to existing history")

	c.StatsMutex.Lock()
	defer c.StatsMutex.Unlock()
	if !assert.Len(t, c.StatHistory, 2) {
		return
	}
	assert.Equal(t, 111, c.StatHistory[0].ClientStats.MemoryStats.Usage)
	assert.Equal(t, 222, c.StatHistory[1].ClientStats.MemoryStats.Usage)

	status, _ = manager.Status(c.ID)
	assert.Equal(t, StatsStateRunning, status.State)
}

// Failed dials and interrupted streams are retried with backoff; the retrying
// state is observable (attempt count + scheduled time); once connected again
// history simply continues.
func TestStatsManagerRetryWithObservableBackoff(t *testing.T) {
	streamer := &fakeStatsStreamer{
		failDials: 3,
		script: func(string, int32) []string {
			return []string{statsJSON(10, 0, 100, 0, 100)}
		},
	}
	manager := newTestManager(t, streamer)
	c := testContainer("c1", "", "")

	sawRetrying := false
	manager.StartContainer(c)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := manager.Status(c.ID)
		if ok && status.State == StatsStateRetrying {
			sawRetrying = true
			assert.Greater(t, status.Attempts, 0)
			assert.False(t, status.NextRetryAt.IsZero())
			assert.NotEmpty(t, status.LastError)
		}
		if ok && status.State == StatsStateRunning {
			break
		}
		time.Sleep(time.Millisecond)
	}

	assert.True(t, sawRetrying, "expected to observe the retrying state")
	waitFor(t, time.Second, func() bool { return historyLen(c) >= 1 }, "sample after reconnect")
	waitFor(t, time.Second, func() bool {
		status, ok := manager.Status(c.ID)
		return ok && status.State == StatsStateRunning && status.Attempts == 0
	}, "attempts reset on successful stream")
}

// Restart cancels the live connection and reconnects immediately without ever
// running two goroutines/connections for the same container.
func TestStatsManagerRestartIsSeamless(t *testing.T) {
	streamer := &fakeStatsStreamer{
		script: func(string, int32) []string {
			return []string{statsJSON(10, 0, 100, 0, 100)}
		},
	}
	manager := newTestManager(t, streamer)
	c := testContainer("c1", "", "")

	manager.StartContainer(c)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.bodies) == 1 }, "first connection")

	manager.RestartContainer(c)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.bodies) == 2 }, "second connection")

	status, _ := manager.Status(c.ID)
	assert.Equal(t, StatsStateRunning, status.State)
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.openPeak), "never two concurrent connections")
	assert.Equal(t, int32(1), atomic.LoadInt32(&streamer.open))
}

// Restart must reconnect immediately even when the normal backoff would
// otherwise be long, both while a stream is live and while sleeping between
// failed dials.
func TestStatsManagerRestartBypassesBackoff(t *testing.T) {
	// Case 1: stream went healthy (one sample) then restart hits while idle in
	// the read loop; backoff had reset to the large initial delay.
	streamer := &fakeStatsStreamer{
		script: func(string, int32) []string {
			return []string{statsJSON(10, 0, 100, 0, 100)}
		},
	}
	appCfg := &config.AppConfig{UserConfig: &config.UserConfig{}}
	manager := newStatsManager(streamer, NewDummyLog(), appCfg, statsManagerOptions{
		retryInitial: 30 * time.Second,
		retryMax:     30 * time.Second,
		jitter:       0,
	})
	t.Cleanup(manager.Close)
	c := testContainer("c1", "", "")

	manager.StartContainer(c)
	waitFor(t, time.Second, func() bool { return historyLen(c) >= 1 }, "first connection streaming")

	manager.RestartContainer(c)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.bodies) == 2 }, "immediate reconnect from live stream")

	// Case 2: restart wakes a collector that is sleeping through a long backoff
	// after a failed dial.
	streamer2 := &fakeStatsStreamer{
		failDials: 1,
		script: func(string, int32) []string {
			return []string{statsJSON(10, 0, 100, 0, 100)}
		},
	}
	manager2 := newStatsManager(streamer2, NewDummyLog(), appCfg, statsManagerOptions{
		retryInitial: 30 * time.Second,
		retryMax:     30 * time.Second,
		jitter:       0,
	})
	t.Cleanup(manager2.Close)
	c2 := testContainer("c2", "", "")

	manager2.StartContainer(c2)
	waitFor(t, time.Second, func() bool {
		status, ok := manager2.Status(c2.ID)
		return ok && status.State == StatsStateRetrying
	}, "sleeping in long backoff")

	manager2.RestartContainer(c2)
	// The first dial failed (so no body opened); restart must immediately make
	// the second dial attempt, which succeeds and opens the first stream.
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer2.bodies) == 1 }, "immediate reconnect from backoff sleep")
	waitFor(t, time.Second, func() bool {
		status, ok := manager2.Status(c2.ID)
		return ok && status.State == StatsStateRunning
	}, "running again")
}

// Scope operations target exactly the matching containers, and aggregates sum
// the latest per-container stats into service/project level summaries.
func TestStatsManagerScopesAndAggregate(t *testing.T) {
	streamer := &fakeStatsStreamer{
		script: func(id string, _ int32) []string {
			usage := 100
			if id == "b" {
				usage = 300
			}
			return []string{statsJSON(10, 0, 100, 0, usage)}
		},
	}
	manager := newTestManager(t, streamer)

	a := testContainer("a", "p1", "svcA", false)
	b := testContainer("b", "p1", "svcA", false)
	cc := testContainer("c", "p1", "svcB", false)
	d := testContainer("d", "p2", "svcC", false)
	oneOff := testContainer("e", "p1", "svcA", true)
	all := []*Container{a, b, cc, d, oneOff}

	started := manager.Start(ServiceStatsScope("p1", "svcA"), all)
	assert.Equal(t, 2, started) // one-off containers are not part of the service

	waitFor(t, time.Second, func() bool {
		return historyLen(a) >= 1 && historyLen(b) >= 1
	}, "service members collecting")

	// Other scopes are untouched.
	_, exists := manager.Status(cc.ID)
	assert.False(t, exists)
	_, exists = manager.Status(d.ID)
	assert.False(t, exists)
	_, exists = manager.Status(oneOff.ID)
	assert.False(t, exists)

	serviceAggregate := manager.Aggregate(ServiceStatsScope("p1", "svcA"), all)
	assert.Equal(t, 2, serviceAggregate.Members)
	assert.Equal(t, 2, serviceAggregate.Reporting)
	assert.Equal(t, 2, serviceAggregate.CollectorStates[StatsStateRunning])
	assert.Equal(t, uint64(400), serviceAggregate.MemoryUsage)
	assert.Equal(t, uint64(2000), serviceAggregate.MemoryLimit)

	projectAggregate := manager.Aggregate(ProjectStatsScope("p1"), all)
	assert.Equal(t, 4, projectAggregate.Members) // includes one-off and svcB, excludes p2
	assert.Equal(t, 2, projectAggregate.Reporting)
	assert.Equal(t, 2, projectAggregate.CollectorStates[StatsStateIdle])

	// Pausing the project tears down every running collector in it.
	manager.Pause(ProjectStatsScope("p1"), all)
	for _, c := range []*Container{a, b} {
		status, ok := manager.Status(c.ID)
		assert.True(t, ok)
		assert.Equal(t, StatsStatePaused, status.State)
	}
	assert.Equal(t, int32(0), atomic.LoadInt32(&streamer.open))

	// Restarting the project starts paused members AND previously idle members.
	manager.Restart(ProjectStatsScope("p1"), all)
	waitFor(t, time.Second, func() bool {
		return atomic.LoadInt32(&streamer.open) == 4
	}, "all four project containers collecting")

	// Stop-all reaches every active collector across scopes.
	manager.Stop(AllStatsScope(), all)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.open) == 0 }, "all connections closed")
	for _, c := range all {
		if status, ok := manager.Status(c.ID); ok {
			assert.NotEqual(t, StatsStateRunning, status.State)
		}
	}
}

// Reconcile stops and forgets collectors whose containers disappeared.
func TestStatsManagerReconcile(t *testing.T) {
	streamer := &fakeStatsStreamer{
		script: func(string, int32) []string {
			return []string{statsJSON(10, 0, 100, 0, 100)}
		},
	}
	manager := newTestManager(t, streamer)
	a := testContainer("a", "", "")
	b := testContainer("b", "", "")

	manager.Ensure(a)
	manager.Ensure(b)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.open) == 2 }, "both open")

	manager.Reconcile([]*Container{a})

	_, exists := manager.Status(b.ID)
	assert.False(t, exists)
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&streamer.open) == 1 }, "removed collector closed")
	status, ok := manager.Status(a.ID)
	assert.True(t, ok)
	assert.Equal(t, StatsStateRunning, status.State)
}

// Close cancels even connections that are mid-backoff and waits for every
// goroutine; calling it twice is safe.
func TestStatsManagerCloseDrainsEverything(t *testing.T) {
	streamer := &fakeStatsStreamer{failDials: 1_000_000}
	appCfg := &config.AppConfig{UserConfig: &config.UserConfig{}}
	manager := newStatsManager(streamer, NewDummyLog(), appCfg, statsManagerOptions{
		retryInitial: time.Millisecond,
		retryMax:     5 * time.Millisecond,
		jitter:       0,
	})

	for _, id := range []string{"a", "b", "c"} {
		manager.StartContainer(testContainer(id, "", ""))
	}
	waitFor(t, time.Second, func() bool {
		statuses := manager.Statuses()
		retrying := 0
		for _, status := range statuses {
			if status.State == StatsStateRetrying {
				retrying++
			}
		}
		return retrying == 3
	}, "all three retrying")

	manager.Close()
	manager.Close()

	assert.Equal(t, int32(0), atomic.LoadInt32(&streamer.open))
}
