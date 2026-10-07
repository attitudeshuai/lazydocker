package gui

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

func newTestReconciler(t *testing.T, defs ...reconcileUnitDef) *reconcileManager {
	t.Helper()
	m := newReconcileManager(logrus.NewEntry(logrus.New()), defs)
	// run view mutations inline: the "gui main loop" is just the calling
	// goroutine in tests
	m.apply = func(mutate func() error) error { return mutate() }
	m.retryInterval = 10 * time.Millisecond
	m.start()
	t.Cleanup(m.stop)
	return m
}

func waitForState(t *testing.T, m *reconcileManager, key reconcileKey, want reconcileState) {
	t.Helper()
	if !assert.Eventually(t, func() bool {
		for _, s := range m.Statuses() {
			if s.key == key {
				return s.state == want
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "unit %s never reached state %d", key, want) {
		t.FailNow()
	}
}

func TestReconcileKeysForEvent(t *testing.T) {
	tests := []struct {
		name     string
		msg      events.Message
		wantKeys []reconcileKey
	}{
		{
			name:     "container stop only touches containers",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionStop},
			wantKeys: []reconcileKey{reconcileContainers},
		},
		{
			name:     "container die",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionDie},
			wantKeys: []reconcileKey{reconcileContainers},
		},
		{
			name:     "container start does not refetch images or networks",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionStart},
			wantKeys: []reconcileKey{reconcileContainers},
		},
		{
			name:     "container commit creates an image too",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionCommit},
			wantKeys: []reconcileKey{reconcileContainers, reconcileImages},
		},
		{
			name:     "exec events change nothing",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionExecCreate},
			wantKeys: nil,
		},
		{
			name:     "exec command variants change nothing",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.Action("exec_start: /bin/sh")},
			wantKeys: nil,
		},
		{
			name:     "attach changes nothing",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionAttach},
			wantKeys: nil,
		},
		{
			name:     "health status changes nothing",
			msg:      events.Message{Type: events.ContainerEventType, Action: events.ActionHealthStatusHealthy},
			wantKeys: nil,
		},
		{
			name:     "image pull touches images only",
			msg:      events.Message{Type: events.ImageEventType, Action: events.ActionPull},
			wantKeys: []reconcileKey{reconcileImages},
		},
		{
			name:     "image delete",
			msg:      events.Message{Type: events.ImageEventType, Action: events.ActionDelete},
			wantKeys: []reconcileKey{reconcileImages},
		},
		{
			name:     "volume create touches volumes only",
			msg:      events.Message{Type: events.VolumeEventType, Action: events.ActionCreate},
			wantKeys: []reconcileKey{reconcileVolumes},
		},
		{
			name:     "network connect touches networks only",
			msg:      events.Message{Type: events.NetworkEventType, Action: events.ActionConnect},
			wantKeys: []reconcileKey{reconcileNetworks},
		},
		{
			name:     "network remove",
			msg:      events.Message{Type: events.NetworkEventType, Action: events.ActionRemove},
			wantKeys: []reconcileKey{reconcileNetworks},
		},
		{
			name:     "daemon reload reconciles everything",
			msg:      events.Message{Type: events.DaemonEventType, Action: events.ActionReload},
			wantKeys: allReconcileKeys(),
		},
		{
			name:     "unknown event type reconciles everything to be safe",
			msg:      events.Message{Type: events.Type("mystery"), Action: events.Action("boop")},
			wantKeys: allReconcileKeys(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reconcileKeysForEvent(tt.msg)
			assert.ElementsMatch(t, tt.wantKeys, got)
		})
	}
}

func TestInitialRequestAllReconcilesEveryUnit(t *testing.T) {
	applied := atomic.Int64{}
	job := func(apply func(func() error) error) error {
		return apply(func() error { applied.Add(1); return nil })
	}
	m := newTestReconciler(t,
		reconcileUnitDef{key: reconcileContainers, job: job, triggers: []reconcileKey{reconcileProjects}},
		reconcileUnitDef{key: reconcileProjects, job: job},
		reconcileUnitDef{key: reconcileImages, job: job},
		reconcileUnitDef{key: reconcileVolumes, job: job},
		reconcileUnitDef{key: reconcileNetworks, job: job},
	)

	m.RequestAll()

	for _, key := range m.orderedUnits {
		waitForState(t, m, key, stateFresh)
	}
	for _, s := range m.Statuses() {
		assert.False(t, s.lastSuccess.IsZero(), "unit %s should record last success", s.key)
	}
}

func TestFetchesForOneUnitNeverOverlapAndCoalesce(t *testing.T) {
	var inFlight, maxInFlight atomic.Int64
	var rounds atomic.Int64

	job := func(apply func(func() error) error) error {
		cur := inFlight.Add(1)
		for {
			max := maxInFlight.Load()
			if cur <= max || maxInFlight.CompareAndSwap(max, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		rounds.Add(1)
		return apply(func() error { return nil })
	}
	m := newTestReconciler(t, reconcileUnitDef{key: reconcileImages, job: job})

	// burst of requests, much denser than the fetch time
	for i := 0; i < 30; i++ {
		m.Request(reconcileImages)
		time.Sleep(2 * time.Millisecond)
	}

	waitForState(t, m, reconcileImages, stateFresh)

	assert.Equal(t, int64(1), maxInFlight.Load(), "fetches for one unit must never overlap")
	// 30 events must not cause 30 fetches; coalescing keeps it well below
	assert.Less(t, rounds.Load(), int64(15), "burst should be coalesced into a few fetches")
}

func TestStaleFetchCannotOverwriteNewerSnapshot(t *testing.T) {
	var mutateCallsMu sync.Mutex
	mutateCalls := []int{}

	round1Started := make(chan struct{})
	releaseRound1 := make(chan struct{})

	roundMu := sync.Mutex{}
	round := 0
	controllableJob := func(apply func(func() error) error) error {
		roundMu.Lock()
		round++
		myRound := round
		roundMu.Unlock()

		if myRound == 1 {
			close(round1Started)
			<-releaseRound1
		}
		return apply(func() error {
			mutateCallsMu.Lock()
			mutateCalls = append(mutateCalls, myRound)
			mutateCallsMu.Unlock()
			return nil
		})
	}

	m := newTestReconciler(t, reconcileUnitDef{key: reconcileImages, job: controllableJob})

	m.Request(reconcileImages)
	<-round1Started

	// newer event arrives while round 1 is still fetching
	m.Request(reconcileImages)

	// let the overtaken round 1 finish: its snapshot must be discarded
	close(releaseRound1)

	waitForState(t, m, reconcileImages, stateFresh)

	assert.Eventually(t, func() bool {
		mutateCallsMu.Lock()
		defer mutateCallsMu.Unlock()
		// only round 2's mutation may have touched the view; round 1's
		// stale snapshot was never applied
		return len(mutateCalls) == 1 && mutateCalls[0] == 2
	}, time.Second, 5*time.Millisecond, "unexpected applied rounds: %v", mutateCalls)
}

func TestFailedFetchKeepsPanelStaleAndRetriesUntilFresh(t *testing.T) {
	var attempts atomic.Int64

	job := func(apply func(func() error) error) error {
		if attempts.Add(1) == 1 {
			return assertError("daemon is down")
		}
		return apply(func() error { return nil })
	}
	m := newTestReconciler(t, reconcileUnitDef{key: reconcileVolumes, job: job})

	m.Request(reconcileVolumes)

	waitForState(t, m, reconcileVolumes, stateFresh)

	// after the first failure it must have retried and eventually succeeded
	assert.GreaterOrEqual(t, attempts.Load(), int64(2))
	for _, s := range m.Statuses() {
		if s.key == reconcileVolumes {
			assert.Nil(t, s.lastError)
			assert.False(t, s.lastSuccess.IsZero())
		}
	}
}

func TestRequestSyncReportsFetchError(t *testing.T) {
	var attempts atomic.Int64
	job := func(apply func(func() error) error) error {
		if attempts.Add(1) == 1 {
			return assertError("boom")
		}
		return apply(func() error { return nil })
	}
	m := newTestReconciler(t, reconcileUnitDef{key: reconcileNetworks, job: job})

	err := m.RequestSync(context.Background(), reconcileNetworks)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "boom")

	// the automatic retry still brings the panel fresh
	waitForState(t, m, reconcileNetworks, stateFresh)
}

func TestContainersSuccessTriggersProjectsReconciliation(t *testing.T) {
	containerFetches := atomic.Int64{}
	projectFetches := atomic.Int64{}

	m := newTestReconciler(t,
		reconcileUnitDef{
			key: reconcileContainers,
			job: func(apply func(func() error) error) error {
				containerFetches.Add(1)
				return apply(func() error { return nil })
			},
			triggers: []reconcileKey{reconcileProjects},
		},
		reconcileUnitDef{
			key: reconcileProjects,
			job: func(apply func(func() error) error) error {
				projectFetches.Add(1)
				return apply(func() error { return nil })
			},
		},
	)

	m.Request(reconcileContainers)

	waitForState(t, m, reconcileContainers, stateFresh)
	waitForState(t, m, reconcileProjects, stateFresh)
	assert.GreaterOrEqual(t, projectFetches.Load(), int64(1))
}

func TestMarkAllStaleFlagsFreshPanels(t *testing.T) {
	job := func(apply func(func() error) error) error {
		return apply(func() error { return nil })
	}
	m := newTestReconciler(t, reconcileUnitDef{key: reconcileImages, job: job})

	m.Request(reconcileImages)
	waitForState(t, m, reconcileImages, stateFresh)

	// simulate subscription loss: no fetch is scheduled by MarkAllStale on
	// its own, but every panel must immediately report it may be outdated
	m.MarkAllStale()

	for _, s := range m.Statuses() {
		if s.key == reconcileImages {
			assert.Equal(t, stateStale, s.state)
		}
	}
}

type staticError string

func (e staticError) Error() string { return string(e) }

func assertError(msg string) error { return staticError(msg) }
