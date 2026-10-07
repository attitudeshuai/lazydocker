package commands

import (
	"math"
	"reflect"
	"time"

	"github.com/jesseduffield/lazydocker/pkg/config"
)

// nowFunc is the clock used when trimming history; tests override it.
var nowFunc = time.Now

// recordStats appends a freshly received sample to the container's history and
// applies retention.
//
// When tiers is empty this is the original behaviour: fine-grained samples are
// kept for maxFineDuration and anything older is dropped. When tiers are
// configured, samples ageing out of the fine window are instead rolled into
// the first (finest) tier by averaging them into interval-sized buckets, and
// samples ageing out of each tier cascade into the next coarser one. Samples
// that age out of the last tier are dropped.
//
// The caller does NOT hold StatsMutex.
func (c *Container) recordStats(stats *RecordedStats, maxFineDuration time.Duration, tiers []config.RetentionTier) {
	c.StatsMutex.Lock()
	defer c.StatsMutex.Unlock()

	c.StatHistory = append(c.StatHistory, stats)

	if len(tiers) == 0 {
		c.eraseOldHistory(maxFineDuration)
		return
	}

	now := nowFunc()
	c.ensureTierSlices(len(tiers))

	if maxFineDuration > 0 {
		kept, aged := splitByAge(c.StatHistory, now, maxFineDuration)
		c.StatHistory = kept
		if len(aged) > 0 {
			c.ensureTierSlices(len(tiers))
			c.CoarseHistory[0], c.coarseWeight[0] = rollup(
				aged, repeatOne(len(aged)),
				c.CoarseHistory[0], c.coarseWeight[0],
				tiers[0].Interval,
			)
		}
	}

	// Age every tier, cascading expired buckets into the next coarser tier.
	for i := range tiers {
		points := c.CoarseHistory[i]
		weights := c.coarseWeight[i]

		kept, agedPts, agedWeights := splitByAgeWeighted(points, weights, now, tiers[i].Duration)
		c.CoarseHistory[i] = kept
		c.coarseWeight[i] = weights[:len(kept)]

		if len(agedPts) == 0 {
			continue
		}

		if i+1 < len(tiers) {
			c.ensureTierSlices(len(tiers))
			c.CoarseHistory[i+1], c.coarseWeight[i+1] = rollup(
				agedPts, agedWeights,
				c.CoarseHistory[i+1], c.coarseWeight[i+1],
				tiers[i+1].Interval,
			)
		}
		// Samples ageing out of the final tier are simply dropped.
	}
}

// ensureTierSlices grows the tier storage to hold tierCount tiers. Existing
// tiers are preserved; the caller holds StatsMutex.
func (c *Container) ensureTierSlices(tierCount int) {
	for len(c.CoarseHistory) < tierCount {
		c.CoarseHistory = append(c.CoarseHistory, []*RecordedStats{})
		c.coarseWeight = append(c.coarseWeight, []int{})
	}
}

// splitByAge partitions an ascending, time-ordered sample slice into the
// samples still inside the window (now-recordedAt < maxAge) and the samples
// that have aged out. Samples whose age equals the boundary age out, matching
// the original "< maxDuration" retention rule.
func splitByAge(samples []*RecordedStats, now time.Time, maxAge time.Duration) (kept []*RecordedStats, aged []*RecordedStats) {
	for i, sample := range samples {
		if now.Sub(sample.RecordedAt) < maxAge {
			return samples[i:], samples[:i]
		}
	}
	return nil, samples
}

// splitByAgeWeighted is splitByAge for a tier slice that carries the number of
// fine samples each point represents.
func splitByAgeWeighted(points []*RecordedStats, weights []int, now time.Time, maxAge time.Duration) (kept []*RecordedStats, aged []*RecordedStats, agedWeights []int) {
	for i, point := range points {
		if now.Sub(point.RecordedAt) < maxAge {
			return points[i:], points[:i], weights[:i]
		}
	}
	return nil, points, weights
}

// rollup merges points (each representing weight fine-grained samples) into
// existing tier points by averaging them into fixed, interval-sized time
// buckets. Both inputs must be ascending by time; new points are expected to
// be newer than existing points, so only the trailing bucket can need merging.
func rollup(points []*RecordedStats, weights []int, existing []*RecordedStats, existingWeights []int, interval time.Duration) ([]*RecordedStats, []int) {
	for i, point := range points {
		bucketStart := bucketTime(point.RecordedAt, interval)

		if n := len(existing); n > 0 && existing[n-1].RecordedAt.Equal(bucketStart) {
			existing[n-1] = mergeRecorded(existing[n-1], point, existingWeights[n-1], weights[i])
			existingWeights[n-1] += weights[i]
			continue
		}

		representative := *point
		representative.RecordedAt = bucketStart
		existing = append(existing, &representative)
		existingWeights = append(existingWeights, weights[i])
	}

	return existing, existingWeights
}

// bucketTime returns the start of the interval-sized bucket that t falls in.
func bucketTime(t time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		return t
	}
	nanos := t.UnixNano()
	step := interval.Nanoseconds()
	return time.Unix(0, nanos-(nanos%step)).In(t.Location())
}

func repeatOne(n int) []int {
	weights := make([]int, n)
	for i := range weights {
		weights[i] = 1
	}
	return weights
}

// mergeRecorded returns a new RecordedStats that is the weighted mean of a and
// b, where wa and wb are the numbers of fine-grained samples each argument
// represents. Numeric leaf fields (including DerivedStats, which powers the
// graphs) are averaged; non-numeric fields are taken from a. Averaging with
// the carried sample weights means cascading an averaged bucket into a coarser
// bucket stays accurate.
func mergeRecorded(a, b *RecordedStats, wa, wb int) *RecordedStats {
	merged := *a
	mergeValue(reflect.ValueOf(&merged).Elem(), reflect.ValueOf(b).Elem(), wa, wb)
	// RecordedAt is assigned to the bucket time by the caller; make sure we
	// never corrupt it while traversing the struct.
	merged.RecordedAt = a.RecordedAt
	return &merged
}

var timeType = reflect.TypeOf(time.Time{})

func mergeValue(dst, src reflect.Value, wa, wb int) {
	if !dst.CanSet() {
		return
	}

	switch dst.Kind() {
	case reflect.Struct:
		// time.Time contains unexported fields; treat it as an opaque leaf.
		if dst.Type() == timeType {
			return
		}
		for i := 0; i < dst.NumField(); i++ {
			mergeValue(dst.Field(i), src.Field(i), wa, wb)
		}
	case reflect.Float32, reflect.Float64:
		dst.SetFloat(weightedMean(dst.Float(), src.Float(), wa, wb))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		dst.SetInt(int64(math.Round(weightedMean(float64(dst.Int()), float64(src.Int()), wa, wb))))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		dst.SetUint(uint64(math.Round(weightedMean(float64(dst.Uint()), float64(src.Uint()), wa, wb))))
	case reflect.Slice:
		mergeNumericSlice(dst, src, wa, wb)
	default:
		// Strings, bools, pointers and anything else keep the existing value.
	}
}

// mergeNumericSlice averages equally-sized numeric slices (e.g. per-CPU usage)
// element by element; mismatched or non-numeric slices are left untouched.
func mergeNumericSlice(dst, src reflect.Value, wa, wb int) {
	if dst.Len() != src.Len() || dst.Len() == 0 {
		return
	}

	switch dst.Index(0).Kind() {
	case reflect.Float32, reflect.Float64:
		for i := 0; i < dst.Len(); i++ {
			dst.Index(i).SetFloat(weightedMean(dst.Index(i).Float(), src.Index(i).Float(), wa, wb))
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		for i := 0; i < dst.Len(); i++ {
			dst.Index(i).SetInt(int64(math.Round(weightedMean(float64(dst.Index(i).Int()), float64(src.Index(i).Int()), wa, wb))))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		for i := 0; i < dst.Len(); i++ {
			dst.Index(i).SetUint(uint64(math.Round(weightedMean(float64(dst.Index(i).Uint()), float64(src.Index(i).Uint()), wa, wb))))
		}
	}
}

func weightedMean(a, b float64, wa, wb int) float64 {
	return (a*float64(wa) + b*float64(wb)) / float64(wa+wb)
}

// TierCount returns the number of retention tiers currently held by the
// container. This is zero when no tiered retention is configured.
func (c *Container) TierCount() int {
	c.StatsMutex.Lock()
	defer c.StatsMutex.Unlock()
	return len(c.CoarseHistory)
}

// GetCoarseHistory returns the downsampled samples stored for the given
// retention tier (0 is the finest tier beyond raw history). The returned slice
// must be treated as read-only and must not outlive the lock-protected reads
// performed while rendering.
func (c *Container) GetCoarseHistory(level int) []*RecordedStats {
	c.StatsMutex.Lock()
	defer c.StatsMutex.Unlock()
	if level < 0 || level >= len(c.CoarseHistory) {
		return nil
	}
	return c.CoarseHistory[level]
}
