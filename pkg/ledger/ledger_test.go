package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func mustLen[T any](t *testing.T, got []T, want int) []T {
	t.Helper()
	if len(got) != want {
		t.Fatalf("expected %d entries, got %d: %#v", want, len(got), got)
	}
	return got
}

func newTestLedger(t *testing.T, enabled bool, opts ...func(*Config)) *Ledger {
	t.Helper()
	cfg := Config{
		Enabled: enabled,
		Dir:     t.TempDir(),
		MaxSize: DefaultMaxSize,
		MaxAge:  DefaultMaxAge,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	l, err := New(cfg)
	must(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func withClock(clock *fakeClock) func(*Config) {
	return func(c *Config) { c.Now = clock.Now }
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestDisabledLedgerTouchesNothingAndIsNoOp(t *testing.T) {
	dir := t.TempDir()
	l, err := New(Config{Enabled: false, Dir: dir})
	must(t, err)
	assert.False(t, l.Enabled())
	assert.Equal(t, "", l.NewBatchID())

	// no file created
	_, err = os.Stat(filepath.Join(dir, FileName))
	assert.True(t, os.IsNotExist(err))

	target := Target{Kind: ObjectContainer, ID: "abc", Project: "proj", Service: "web"}
	apiErr := l.Start(PathAPI, "container.remove").For(target).Run(func() error {
		return errors.New("boom")
	})
	// operation error passes through untouched, no ledger error joins in
	assert.EqualError(t, apiErr, "boom")

	procErr := l.Start(PathProcess, "custom-command").For(target).
		FinishProcess("docker stop abc", 1, errors.New("exit status 1"))
	assert.EqualError(t, procErr, "exit status 1")

	assert.Nil(t, l.Query(Filter{}))
	assert.NoError(t, l.Close())
}

func TestNilLedgerReceiversAreSafe(t *testing.T) {
	var l *Ledger
	assert.False(t, l.Enabled())
	assert.Equal(t, "", l.NewBatchID())
	l.SetConnection("ssh://host")
	assert.NoError(t, l.Close())
	assert.NoError(t, l.Start(PathAPI, "x").Run(func() error { return nil }))
	assert.Nil(t, l.Query(Filter{}))
}

func TestAPIRecordCapturesAllFields(t *testing.T) {
	clock := newFakeClock()
	l := newTestLedger(t, true, withClock(clock))
	l.SetConnection("unix:///var/run/docker.sock")

	target := Target{Kind: ObjectContainer, ID: "deadbeef", Name: "web-1", Project: "shop", Service: "web"}

	// success
	clock.Advance(time.Second)
	err := l.Start(PathAPI, "container.stop").For(target).Run(func() error {
		clock.Advance(250 * time.Millisecond)
		return nil
	})
	must(t, err)

	// failure
	clock.Advance(time.Second)
	err = l.Start(PathAPI, "container.remove").For(target).Run(func() error {
		clock.Advance(50 * time.Millisecond)
		return errors.New("must stop container first")
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must stop container first")

	entries := l.Query(Filter{})
	mustLen(t, entries, 2)

	first := entries[0]
	assert.Equal(t, int64(1), first.Seq)
	assert.Equal(t, PathAPI, first.Path)
	assert.Equal(t, "container.stop", first.Action)
	assert.Equal(t, target, first.Target)
	assert.Equal(t, "unix:///var/run/docker.sock", first.Connection)
	assert.True(t, first.Success)
	assert.Empty(t, first.Error)
	assert.Nil(t, first.ExitCode, "API records carry no exit code")
	assert.False(t, first.StartedAt.IsZero())
	assert.True(t, first.EndedAt.After(first.StartedAt))
	assert.Equal(t, int64(250), first.DurationMS)
	assert.Empty(t, first.Command)
	assert.Empty(t, first.BatchID)

	second := entries[1]
	assert.Equal(t, int64(2), second.Seq)
	assert.False(t, second.Success)
	assert.Equal(t, "must stop container first", second.Error)
	assert.Nil(t, second.ExitCode)
}

func TestProcessRecordCapturesCommandAndExitCode(t *testing.T) {
	l := newTestLedger(t, true)
	l.SetConnection("ssh://docker@remote")
	target := Target{Kind: ObjectService, ID: "shop-web", Name: "web", Project: "shop", Service: "web"}

	op := l.Start(PathProcess, "service.restart").For(target)
	err := op.FinishProcess("docker compose -p shop restart web", 0, nil)
	must(t, err)

	op = l.Start(PathProcess, "bulk-command").For(Target{Kind: ObjectProject, ID: "shop", Name: "shop"})
	err = op.FinishProcess("docker compose -p shop down", 1, errors.New("exit status 1"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 1")

	entries := l.Query(Filter{})
	mustLen(t, entries, 2)
	assert.Equal(t, "docker compose -p shop restart web", entries[0].Command)
	if assert.NotNil(t, entries[0].ExitCode) {
		assert.Equal(t, 0, *entries[0].ExitCode)
	}
	assert.True(t, entries[0].Success)
	assert.Equal(t, PathProcess, entries[0].Path)
	assert.Equal(t, "ssh://docker@remote", entries[0].Connection)

	if assert.NotNil(t, entries[1].ExitCode) {
		assert.Equal(t, 1, *entries[1].ExitCode)
	}
	assert.False(t, entries[1].Success)
	assert.Contains(t, entries[1].Error, "exit status 1")
}

func TestBatchGroupsPerItemResults(t *testing.T) {
	l := newTestLedger(t, true)
	batchID := l.NewBatchID()
	assert.NotEmpty(t, batchID)

	ids := []string{"c1", "c2", "c3"}
	for i, id := range ids {
		err := l.Start(PathAPI, "container.stop").
			For(Target{Kind: ObjectContainer, ID: id}).
			Batch(batchID, i, len(ids)).
			Run(func() error {
				if id == "c2" {
					return errors.New("no such container")
				}
				return nil
			})
		if id == "c2" {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
		}
	}

	group := l.Query(Filter{BatchID: batchID})
	mustLen(t, group, 3)
	for i, e := range group {
		assert.Equal(t, batchID, e.BatchID)
		assert.Equal(t, i, e.BatchIndex)
		assert.Equal(t, 3, e.BatchTotal)
		assert.Equal(t, ids[i], e.Target.ID)
	}
	assert.True(t, group[0].Success)
	assert.False(t, group[1].Success)
	assert.True(t, group[2].Success)
	assert.NotEqual(t, l.NewBatchID(), batchID)
}

func TestConcurrentWritesStayOrderedAndNothingIsLost(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Enabled: true, Dir: dir}
	l, err := New(cfg)
	must(t, err)

	const goroutines = 16
	const perRoutine = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perRoutine; i++ {
				id := "g" + strconv.Itoa(g) + "-" + strconv.Itoa(i)
				_ = l.Start(PathAPI, "container.start").
					For(Target{Kind: ObjectContainer, ID: id, Project: "p"}).
					Run(func() error { return nil })
			}
		}(g)
	}
	wg.Wait()
	must(t, l.Close())

	// reopen from disk and verify
	l2, err := New(cfg)
	must(t, err)
	defer l2.Close()

	entries := l2.Query(Filter{})
	mustLen(t, entries, goroutines*perRoutine)

	seen := make(map[string]bool, len(entries))
	var prevSeq int64
	for _, e := range entries {
		// gap-free ascending seq: nothing was lost or interleaved
		if e.Seq != prevSeq+1 {
			t.Fatalf("sequence gap at %#v: want seq %d, got %d", e, prevSeq+1, e.Seq)
		}
		prevSeq = e.Seq
		if seen[e.Target.ID] {
			t.Fatalf("duplicate record for %s", e.Target.ID)
		}
		seen[e.Target.ID] = true
		assert.True(t, e.Success)
		assert.Equal(t, "p", e.Target.Project)
	}
}

func TestQueriesByObjectProjectAndTimeRange(t *testing.T) {
	clock := newFakeClock()
	l := newTestLedger(t, true, withClock(clock))

	record := func(action string, target Target, startedAt time.Time) {
		clock.t = startedAt
		must(t, l.Start(PathAPI, action).For(target).Run(func() error { return nil }))
	}

	t0 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	record("container.start", Target{Kind: ObjectContainer, ID: "c1", Project: "alpha", Service: "web"}, t0)
	record("container.stop", Target{Kind: ObjectContainer, ID: "c1", Project: "alpha", Service: "web"}, t0.Add(time.Hour))
	record("image.remove", Target{Kind: ObjectImage, ID: "img1", Project: "alpha"}, t0.Add(2*time.Hour))
	record("volume.remove", Target{Kind: ObjectVolume, ID: "v1", Project: "beta"}, t0.Add(3*time.Hour))
	record("network.remove", Target{Kind: ObjectNetwork, ID: "n1", Project: "beta", Service: "db"}, t0.Add(4*time.Hour))

	// by object kind + id
	byObject := l.Query(Filter{ObjectKind: ObjectContainer, ObjectID: "c1"})
	mustLen(t, byObject, 2)
	assert.Equal(t, "c1", byObject[0].Target.ID)

	// by project
	assert.Len(t, l.Query(Filter{Project: "alpha"}), 3)
	assert.Len(t, l.Query(Filter{Project: "beta"}), 2)

	// by project + service
	assert.Len(t, l.Query(Filter{Project: "alpha", Service: "web"}), 2)

	// by kind only
	assert.Len(t, l.Query(Filter{ObjectKind: ObjectVolume}), 1)

	// time range (bounds inclusive)
	inRange := l.Query(Filter{Since: t0.Add(90 * time.Minute), Until: t0.Add(3 * time.Hour)})
	mustLen(t, inRange, 2)
	assert.Equal(t, "image.remove", inRange[0].Action)
	assert.Equal(t, "volume.remove", inRange[1].Action)

	// since only
	assert.Len(t, l.Query(Filter{Since: t0.Add(2 * time.Hour)}), 3)
}

func TestQueryReturnsSnapshotWithoutHalfWrittenRecords(t *testing.T) {
	l := newTestLedger(t, true)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = l.Start(PathAPI, "container.restart").
					For(Target{Kind: ObjectContainer, ID: "x"}).
					Run(func() error { return nil })
			}
		}()
	}

	// readers must never observe partially committed entries
	stop := make(chan struct{})
	readerWG := sync.WaitGroup{}
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				for _, e := range l.Query(Filter{}) {
					assert.NotEmpty(t, e.Action)
					assert.Greater(t, e.Seq, int64(0))
					assert.False(t, e.EndedAt.Before(e.StartedAt))
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	readerWG.Wait()
	assert.Len(t, l.Query(Filter{}), 800)
}

func TestRotationBySizeEvictsOldest(t *testing.T) {
	l := newTestLedger(t, true, func(c *Config) { c.MaxSize = 450 })

	record := func(id string) int64 {
		err := l.Start(PathProcess, "custom-command").
			For(Target{Kind: ObjectContainer, ID: id}).
			FinishProcess("docker inspect --format '{{ .Config.Labels }}' "+id, 0, nil)
		must(t, err)
		found := l.Query(Filter{ObjectKind: ObjectContainer, ObjectID: id})
		mustLen(t, found, 1)
		return found[0].Seq
	}

	firstSeq := record("container-one-with-a-rather-long-identifier")
	entries := l.Query(Filter{})
	mustLen(t, entries, 1)
	secondSeq := record("container-two-with-a-rather-long-identifier")
	entries = l.Query(Filter{})
	mustLen(t, entries, 1)
	assert.Equal(t, secondSeq, entries[0].Seq)
	assert.NotEqual(t, firstSeq, entries[0].Seq)

	thirdSeq := record("container-three-with-a-rather-long-identifier")
	entries = l.Query(Filter{})
	mustLen(t, entries, 1)
	assert.Equal(t, thirdSeq, entries[0].Seq)

	// survivors actually fit on disk
	info, err := os.Stat(filepath.Join(l.cfg.Dir, FileName))
	must(t, err)
	assert.LessOrEqual(t, info.Size(), int64(450))

	// and survive a restart
	must(t, l.Close())
	reopened, err := New(Config{Enabled: true, Dir: l.cfg.Dir, MaxSize: 450, MaxAge: DefaultMaxAge})
	must(t, err)
	defer reopened.Close()
	entries = reopened.Query(Filter{})
	mustLen(t, entries, 1)
	assert.Equal(t, thirdSeq, entries[0].Seq)
}

func TestEvictionByAge(t *testing.T) {
	clock := newFakeClock()
	l := newTestLedger(t, true, withClock(clock), func(c *Config) { c.MaxAge = 30 * 24 * time.Hour })

	commit := func(action string) {
		must(t, l.Start(PathAPI, action).
			For(Target{Kind: ObjectContainer, ID: "c1"}).
			Run(func() error { return nil }))
	}

	commit("container.create")
	clock.Advance(31 * 24 * time.Hour)
	commit("container.start")

	entries := l.Query(Filter{})
	mustLen(t, entries, 1)
	assert.Equal(t, "container.start", entries[0].Action)

	// expired records are gone from disk after a restart too
	must(t, l.Close())
	reopened, err := New(Config{Enabled: true, Dir: l.cfg.Dir, MaxSize: DefaultMaxSize, MaxAge: 30 * 24 * time.Hour, Now: clock.Now})
	must(t, err)
	defer reopened.Close()
	entries = reopened.Query(Filter{})
	mustLen(t, entries, 1)
	assert.Equal(t, "container.start", entries[0].Action)
}

func TestStartupRepairsTornLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	l, err := New(Config{Enabled: true, Dir: dir})
	must(t, err)
	must(t, l.Start(PathAPI, "container.stop").For(Target{Kind: ObjectContainer, ID: "good"}).
		Run(func() error { return nil }))
	must(t, l.Close())

	// simulate a crash mid-write: a partial JSON line at the tail
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	must(t, err)
	_, err = f.WriteString(`{"seq":99,"action":"container.start","tar`)
	must(t, err)
	must(t, f.Close())

	l2, err := New(Config{Enabled: true, Dir: dir})
	must(t, err)
	defer l2.Close()

	entries := l2.Query(Filter{})
	mustLen(t, entries, 1)
	assert.Equal(t, "good", entries[0].Target.ID)

	// appending still works and the repaired file replays cleanly
	must(t, l2.Start(PathAPI, "container.start").For(Target{Kind: ObjectContainer, ID: "good2"}).
		Run(func() error { return nil }))

	l3, err := New(Config{Enabled: true, Dir: dir})
	must(t, err)
	defer l3.Close()
	entries = l3.Query(Filter{})
	mustLen(t, entries, 2)
	assert.Equal(t, "good", entries[0].Target.ID)
	assert.Equal(t, "good2", entries[1].Target.ID)
}

func TestWriteFailureIsReportedNotSwallowed(t *testing.T) {
	l := newTestLedger(t, true)
	must(t, l.Close())

	// writes after close must return an explicit error rather than vanishing
	err := l.Start(PathAPI, "container.stop").Run(func() error { return nil })
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "closed")

	// RecordProcessResult surfaces the ledger failure but never hides the
	// command error inside it (interactive callers must be able to report the
	// two independently).
	cmdErr := errors.New("exit status 1")
	recErr := l.Start(PathProcess, "container.attach").
		RecordProcessResult("docker attach c1", 1, cmdErr)
	assert.Error(t, recErr)
	assert.Contains(t, recErr.Error(), "closed")
	assert.NotContains(t, recErr.Error(), "exit status 1")

	// and direct Append of invalid entries is rejected too
	l2 := newTestLedger(t, true)
	err = l2.Append(Entry{Target: Target{Kind: ObjectContainer}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "action")
}

func TestNewFailsWhenDirectoryCannotBeCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a-file")
	must(t, os.WriteFile(dir, []byte("x"), 0o600))
	_, err := New(Config{Enabled: true, Dir: filepath.Join(dir, "sub")})
	assert.Error(t, err)
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"512":     512,
		"512B":    512,
		"1KB":     1024,
		"2K":      2048,
		"1MB":     1 << 20,
		"1.5MB":   int64(1.5 * float64(1<<20)),
		"2GB":     2 << 30,
		" 100KB ": 100 * 1024,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		must(t, err)
		assert.Equal(t, want, got, "input %q", in)
	}

	for _, bad := range []string{"", "abc", "1XB"} {
		_, err := ParseSize(bad)
		assert.Error(t, err)
	}
}

func TestStartRequiresAction(t *testing.T) {
	l := newTestLedger(t, true)
	err := l.Start(PathAPI, "").Run(func() error { return nil })
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "action")
}
