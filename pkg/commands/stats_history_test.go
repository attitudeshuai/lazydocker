package commands

import (
	"testing"
	"time"

	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/stretchr/testify/assert"
)

func sampleAt(at time.Time, cpu float64, memUsage int) *RecordedStats {
	stats := &RecordedStats{RecordedAt: at}
	stats.DerivedStats.CPUPercentage = cpu
	stats.DerivedStats.MemoryPercentage = cpu
	stats.ClientStats.MemoryStats.Usage = memUsage
	stats.ClientStats.MemoryStats.Limit = 1000
	return stats
}

// Without tiers configured retention must behave exactly like the original
// implementation: samples inside the window stay, older samples are dropped
// outright and no coarse tiers are allocated.
func TestRecordStatsNoTiersMatchesOldBehaviour(t *testing.T) {
	c := &Container{}
	now := time.Now()

	c.recordStats(sampleAt(now.Add(-10*time.Minute), 1, 10), 3*time.Minute, nil)
	c.recordStats(sampleAt(now.Add(-2*time.Minute), 2, 20), 3*time.Minute, nil)
	c.recordStats(sampleAt(now.Add(-30*time.Second), 3, 30), 3*time.Minute, nil)

	assert.Len(t, c.StatHistory, 2)
	assert.Equal(t, float64(2), c.StatHistory[0].DerivedStats.CPUPercentage)
	assert.Equal(t, float64(3), c.StatHistory[1].DerivedStats.CPUPercentage)
	assert.Empty(t, c.CoarseHistory)
	assert.Equal(t, 0, c.TierCount())
}

// The original eraseOldHistory keeps the whole slice when every sample is too
// old; the no-tiers path preserves that quirk.
func TestRecordStatsNoTiersKeepsAllOldSliceQuirk(t *testing.T) {
	c := &Container{}
	now := time.Now()

	c.recordStats(sampleAt(now.Add(-10*time.Minute), 1, 10), 3*time.Minute, nil)
	assert.Len(t, c.StatHistory, 1)
}

// A maxDuration of zero disables trimming entirely, with or without tiers.
func TestRecordStatsNoTiersZeroDurationKeepsEverything(t *testing.T) {
	c := &Container{}
	now := time.Now()

	c.recordStats(sampleAt(now.Add(-10*time.Hour), 1, 10), 0, nil)
	c.recordStats(sampleAt(now, 2, 20), 0, nil)

	assert.Len(t, c.StatHistory, 2)
	assert.Empty(t, c.CoarseHistory)
}

func TestMergeRecordedWeightedMean(t *testing.T) {
	a := sampleAt(time.Unix(100, 0), 10, 100)
	b := sampleAt(time.Unix(100, 0), 40, 800)

	merged := mergeRecorded(a, b, 2, 6)

	assert.InDelta(t, 32.5, merged.DerivedStats.CPUPercentage, 0.0001)
	assert.InDelta(t, 32.5, merged.DerivedStats.MemoryPercentage, 0.0001)
	// int fields are the weighted mean as well (200 + 4800)/8.
	assert.Equal(t, 625, merged.ClientStats.MemoryStats.Usage)
	assert.Equal(t, int64(1000), merged.ClientStats.MemoryStats.Limit)
}

func TestRollupBucketsAndMergesByInterval(t *testing.T) {
	base := time.Unix(600, 0)
	points := []*RecordedStats{
		sampleAt(base, 10, 100),
		sampleAt(base.Add(1*time.Second), 20, 200),
		sampleAt(base.Add(2*time.Second), 30, 300),
	}

	got, weights := rollup(points, []int{1, 1, 1}, nil, nil, 2*time.Second)

	assert.Len(t, got, 2)
	assert.Equal(t, base, got[0].RecordedAt)
	assert.Equal(t, base.Add(2*time.Second), got[1].RecordedAt)
	assert.InDelta(t, 15, got[0].DerivedStats.CPUPercentage, 0.0001)
	assert.InDelta(t, 30, got[1].DerivedStats.CPUPercentage, 0.0001)
	assert.Equal(t, []int{2, 1}, weights)

	// Cascade-style merge: an already-weighted bucket (two fine samples,
	// mean 15) receives a third sample in the same ascending bucket; the
	// carried weight keeps the result the true mean of all three samples.
	existing := []*RecordedStats{sampleAt(base, 15, 150)}
	got, weights = rollup(
		[]*RecordedStats{sampleAt(base.Add(1*time.Second), 45, 450)},
		[]int{1},
		existing, []int{2}, 2*time.Second,
	)
	assert.Len(t, got, 1)
	assert.InDelta(t, 25, got[0].DerivedStats.CPUPercentage, 0.0001) // (15*2 + 45)/3
	assert.Equal(t, []int{3}, weights)
}

// Full tier cascade: samples from the last 24 seconds. Fine window keeps the
// last 2 seconds; tier 0 (4s buckets, 8s retention) keeps the bucket whose
// start is 4s ago; tier 1 (12s buckets, 24s retention) holds the cascaded
// buckets covering 5-12s ago. Samples whose coarse bucket starts beyond the
// last tier's duration are dropped.
func TestRecordStatsCascadesThroughTiers(t *testing.T) {
	anchor := time.Unix(600, 0) // divisible by 2s, 4s and 12s
	oldNow := nowFunc
	nowFunc = func() time.Time { return anchor }
	defer func() { nowFunc = oldNow }()

	tiers := []config.RetentionTier{
		{Interval: 4 * time.Second, Duration: 8 * time.Second},
		{Interval: 12 * time.Second, Duration: 24 * time.Second},
	}

	c := &Container{}
	// Insert oldest first so history stays time-ordered.
	for age := 23; age >= 0; age-- {
		c.recordStats(sampleAt(anchor.Add(-time.Duration(age)*time.Second), float64(age), age*10), 2*time.Second, tiers)
	}

	// Fine tier: ages 1 then 0 (inserted oldest first, so the newer sample
	// lands last).
	assert.Len(t, c.StatHistory, 2)
	assert.InDelta(t, 1, c.StatHistory[0].DerivedStats.CPUPercentage, 0.0001)
	assert.InDelta(t, 0, c.StatHistory[1].DerivedStats.CPUPercentage, 0.0001)

	assert.Equal(t, 2, c.TierCount())

	// Tier 0: the bucket covering ages 2-4 (bucket start 4s ago) remains;
	// buckets starting 8s ago and older cascade into tier 1.
	tier0 := c.GetCoarseHistory(0)
	assert.Len(t, tier0, 1)
	assert.Equal(t, anchor.Add(-4*time.Second), tier0[0].RecordedAt)
	assert.InDelta(t, 3.0, tier0[0].DerivedStats.CPUPercentage, 0.0001)
	assert.Equal(t, 30, tier0[0].ClientStats.MemoryStats.Usage)

	// Tier 1: the bucket covering ages 5-12, i.e. the true mean of those
	// samples (weighted mean of the cascaded bucket means).
	tier1 := c.GetCoarseHistory(1)
	assert.Len(t, tier1, 1)
	assert.Equal(t, anchor.Add(-12*time.Second), tier1[0].RecordedAt)
	assert.InDelta(t, 8.5, tier1[0].DerivedStats.CPUPercentage, 0.0001)
	assert.Equal(t, 85, tier1[0].ClientStats.MemoryStats.Usage)
}

