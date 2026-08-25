package evidence

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Series tiers (§3.3b). Tier 0 is the raw sample ring; tiers 1 and 2 are
// stored digests; tier 3 is the cluster timeline (see Timeline).
const (
	TierRaw    = 0
	TierHourly = 1
	TierDaily  = 2
)

// DigestStats are order statistics over one resource dimension within a
// digest window. For coalesced and daily digests the percentiles are
// element-wise maxima of the merged windows — a conservative upper bound
// (never an underestimate), which is the safe direction for sizing; Max is
// always exact.
type DigestStats struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// ordered reports p50 ≤ p95 ≤ p99 ≤ max with all values finite and
// non-negative — the invariant every stored digest satisfies (element-wise
// max merging preserves it).
func (d DigestStats) ordered() bool {
	vals := [4]float64{d.P50, d.P95, d.P99, d.Max}
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return d.P50 <= d.P95 && d.P95 <= d.P99 && d.P99 <= d.Max
}

// merge folds o in element-wise (max).
func (d *DigestStats) merge(o DigestStats) {
	d.P50 = math.Max(d.P50, o.P50)
	d.P95 = math.Max(d.P95, o.P95)
	d.P99 = math.Max(d.P99, o.P99)
	d.Max = math.Max(d.Max, o.Max)
}

// Digest is one aggregated window of usage history: [Start, End).
type Digest struct {
	Start   time.Time   `json:"start"`
	End     time.Time   `json:"end"`
	Tier    int         `json:"tier"`
	Samples int64       `json:"samples"`
	CPU     DigestStats `json:"cpu"` // milliCPU
	Mem     DigestStats `json:"mem"` // bytes
	// ThrottleRatio is the sample-weighted mean CFS throttle ratio.
	ThrottleRatio float64 `json:"throttleRatio"`
	Restarts      int64   `json:"restarts"`
	OOMs          int64   `json:"ooms"`
	// Coalesced counts additional windows run-length-folded into this digest
	// beyond the first (0 = a single window).
	Coalesced int64 `json:"coalesced,omitempty"`
}

// hourAcc accumulates the current, not-yet-finalized hour of samples.
// Bounded by Config.MaxSamplesPerHour; overflow samples are counted in
// Samples (and Stats) but excluded from the percentile arrays.
type hourAcc struct {
	hour     time.Time // hour start, UTC
	cpu      []int64
	mem      []int64
	throttle []float64
	restarts int64
	ooms     int64
	dropped  int64
}

func (a *hourAcc) bytes() int64 { return int64(len(a.cpu)) * accSampleBytes }

// series is the per-subject tiered history.
type series struct {
	ref     SubjectRef
	raw     ring[Sample]
	hourly  ring[Digest]
	daily   ring[Digest]
	acc     *hourAcc
	day     *Digest // accumulating daily rollup (Tier 2, partial day)
	lastAt  time.Time
	lastSeq uint64 // seq of the last sample — the coldness key for eviction
	bytes   int64
	hIdx    int
}

// sanitizeSample validates and normalizes a sample. Garbage — zero time,
// negative or absurd magnitudes, NaN/Inf throttle — is rejected, never
// stored (the pkg/patterns posture).
func sanitizeSample(smp Sample) (Sample, error) {
	if smp.At.IsZero() {
		return smp, fmt.Errorf("evidence: sample needs a timestamp")
	}
	smp.At = utcTime(smp.At)
	if smp.MilliCPU < 0 || smp.MilliCPU > maxSampleValue ||
		smp.MemoryBytes < 0 || smp.MemoryBytes > maxSampleValue ||
		smp.Restarts < 0 || smp.Restarts > maxSampleValue ||
		smp.OOMs < 0 || smp.OOMs > maxSampleValue {
		return smp, fmt.Errorf("evidence: sample magnitudes outside [0, 1e18]")
	}
	if math.IsNaN(smp.ThrottleRatio) || math.IsInf(smp.ThrottleRatio, 0) {
		return smp, fmt.Errorf("evidence: sample throttle ratio is not finite")
	}
	if smp.ThrottleRatio < 0 {
		smp.ThrottleRatio = 0
	}
	if smp.ThrottleRatio > 1 {
		smp.ThrottleRatio = 1
	}
	return smp, nil
}

// validateSample is the restore-side dual: checks without transforming.
func validateSample(smp Sample) error {
	s, err := sanitizeSample(smp)
	if err != nil {
		return err
	}
	if !timeStoredOK(smp.At) || s.ThrottleRatio != smp.ThrottleRatio {
		return fmt.Errorf("evidence: sample not storage-normal")
	}
	return nil
}

// nearestRank returns the nearest-rank percentile of sorted (ascending)
// values: the smallest value with at least ceil(p*n) values ≤ it.
func nearestRank(sorted []int64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return float64(sorted[idx])
}

func statsFromValues(vals []int64) DigestStats {
	if len(vals) == 0 {
		return DigestStats{}
	}
	sorted := make([]int64, len(vals))
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return DigestStats{
		P50: nearestRank(sorted, 0.50),
		P95: nearestRank(sorted, 0.95),
		P99: nearestRank(sorted, 0.99),
		Max: float64(sorted[len(sorted)-1]),
	}
}

// finalize turns the accumulator into a tier-1 digest. The accumulator
// always holds at least one sample (it is created on first ingest and the
// first sample always fits the per-hour cap).
func (a *hourAcc) finalize() Digest {
	n := len(a.throttle)
	sum := 0.0
	for _, t := range a.throttle {
		sum += t
	}
	mean := 0.0
	if n > 0 {
		mean = sum / float64(n)
	}
	return Digest{
		Start:         a.hour,
		End:           a.hour.Add(time.Hour),
		Tier:          TierHourly,
		Samples:       int64(n) + a.dropped,
		CPU:           statsFromValues(a.cpu),
		Mem:           statsFromValues(a.mem),
		ThrottleRatio: mean,
		Restarts:      a.restarts,
		OOMs:          a.ooms,
	}
}

// withinTol reports whether b is within relative tolerance tol of a on
// every order statistic, with an absolute floor so near-zero series
// coalesce despite large relative wiggle.
func withinTol(a, b DigestStats, tol, floor float64) bool {
	pairs := [4][2]float64{{a.P50, b.P50}, {a.P95, b.P95}, {a.P99, b.P99}, {a.Max, b.Max}}
	for _, p := range pairs {
		diff := math.Abs(p[0] - p[1])
		if diff <= floor {
			continue
		}
		if diff > tol*math.Max(p[0], p[1]) {
			return false
		}
	}
	return true
}

// Absolute coalescing floors: differences below these are always boring.
const (
	coalesceCPUFloor      = 5.0      // milliCPU
	coalesceMemFloor      = 16 << 20 // bytes
	coalesceThrottleDelta = 0.05     // absolute throttle-ratio difference
)

// canCoalesce reports whether next can be run-length-folded into prev:
// windows must be adjacent (no gap — a gap is information), both must be
// boring (no restarts, no OOMs — those are evidence, never coalesced), and
// every statistic must be within tolerance.
func canCoalesce(prev, next *Digest, tol float64) bool {
	return prev.End.Equal(next.Start) &&
		prev.Restarts == 0 && prev.OOMs == 0 &&
		next.Restarts == 0 && next.OOMs == 0 &&
		math.Abs(prev.ThrottleRatio-next.ThrottleRatio) <= coalesceThrottleDelta &&
		withinTol(prev.CPU, next.CPU, tol, coalesceCPUFloor) &&
		withinTol(prev.Mem, next.Mem, tol, coalesceMemFloor)
}

// foldInto merges next into prev (coalescing or daily rollup): the window
// extends, percentiles take the element-wise max (conservative), the
// throttle ratio stays a sample-weighted mean, counts add.
func foldInto(prev, next *Digest) {
	total := prev.Samples + next.Samples
	if total > 0 {
		prev.ThrottleRatio = (prev.ThrottleRatio*float64(prev.Samples) + next.ThrottleRatio*float64(next.Samples)) / float64(total)
	}
	prev.End = next.End
	prev.Samples = total
	prev.CPU.merge(next.CPU)
	prev.Mem.merge(next.Mem)
	prev.Restarts += next.Restarts
	prev.OOMs += next.OOMs
	prev.Coalesced += next.Coalesced + 1
}

// validateDigest checks a checkpointed digest without transforming it.
func validateDigest(d Digest, wantTier int) error {
	if d.Tier != wantTier {
		return fmt.Errorf("evidence: digest tier %d, want %d", d.Tier, wantTier)
	}
	if !timeStoredOK(d.Start) || !timeStoredOK(d.End) || !d.End.After(d.Start) {
		return fmt.Errorf("evidence: digest window [%v, %v) invalid", d.Start, d.End)
	}
	if d.Samples < 1 || d.Restarts < 0 || d.OOMs < 0 || d.Coalesced < 0 {
		return fmt.Errorf("evidence: digest counts invalid")
	}
	if !d.CPU.ordered() || !d.Mem.ordered() {
		return fmt.Errorf("evidence: digest stats out of order or not finite")
	}
	if math.IsNaN(d.ThrottleRatio) || d.ThrottleRatio < 0 || d.ThrottleRatio > 1 {
		return fmt.Errorf("evidence: digest throttle ratio outside [0, 1]")
	}
	return nil
}

// ingest advances the series with one sanitized sample. seq is the global
// arrival sequence. Returns the accounted byte delta.
func (sr *series) ingest(smp Sample, seq uint64, cfg *Config, st *Stats) int64 {
	var delta int64
	hour := smp.At.Truncate(time.Hour)
	if sr.acc != nil && hour.After(sr.acc.hour) {
		delta += sr.rollHour(cfg, st)
	}
	if sr.acc == nil {
		sr.acc = &hourAcc{hour: hour}
	}
	if len(sr.acc.cpu) < cfg.MaxSamplesPerHour {
		sr.acc.cpu = append(sr.acc.cpu, smp.MilliCPU)
		sr.acc.mem = append(sr.acc.mem, smp.MemoryBytes)
		sr.acc.throttle = append(sr.acc.throttle, smp.ThrottleRatio)
		delta += accSampleBytes
	} else {
		sr.acc.dropped++
		st.DroppedSamples++
	}
	sr.acc.restarts += smp.Restarts
	sr.acc.ooms += smp.OOMs

	if _, evicted := sr.raw.push(smp, cfg.RawSampleCap); evicted {
		st.EvictedSeriesItems++
	} else {
		delta += sampleBytes
	}
	sr.lastAt = smp.At
	sr.lastSeq = seq
	sr.bytes += delta
	return delta
}

// rollHour finalizes the pending hour into the hourly tier (and the daily
// rollup). Returns the accounted byte delta; the caller applies it to
// sr.bytes exactly once (rollHour must not, or ingest would count it twice).
func (sr *series) rollHour(cfg *Config, st *Stats) int64 {
	var delta int64
	d := sr.acc.finalize()
	delta -= sr.acc.bytes()
	sr.acc = nil

	// Daily rollup: flush the pending day when this hour belongs to a later day.
	dayStart := d.Start.Truncate(24 * time.Hour)
	if sr.day != nil && dayStart.After(sr.day.Start.Truncate(24*time.Hour)) {
		delta += sr.flushDay(cfg, st)
	}
	if sr.day == nil {
		day := d
		day.Tier = TierDaily
		day.Coalesced = 0
		sr.day = &day
		delta += digestBytes
	} else {
		fold := d
		fold.Tier = TierDaily
		foldInto(sr.day, &fold)
		sr.day.Coalesced = 0 // Coalesced tracks run-length folds, not day membership
	}

	delta += sr.appendDigest(&sr.hourly, d, cfg.HourlyCap, cfg, &st.CoalescedHourly, st)
	return delta
}

// flushDay moves the pending daily rollup into the daily tier.
func (sr *series) flushDay(cfg *Config, st *Stats) int64 {
	if sr.day == nil {
		return 0
	}
	d := *sr.day
	sr.day = nil
	delta := int64(-digestBytes)
	delta += sr.appendDigest(&sr.daily, d, cfg.DailyCap, cfg, &st.CoalescedDaily, st)
	return delta
}

// appendDigest run-length-coalesces d into r's newest digest when boring,
// else pushes it (evicting oldest at cap). Returns the byte delta.
func (sr *series) appendDigest(r *ring[Digest], d Digest, capN int, cfg *Config, coalesced *uint64, st *Stats) int64 {
	if r.len() > 0 {
		prev := r.at(r.len() - 1)
		if canCoalesce(prev, &d, cfg.CoalesceTolerance) {
			foldInto(prev, &d)
			*coalesced++
			return 0
		}
	}
	if _, evicted := r.push(d, capN); evicted {
		st.EvictedSeriesItems++
		return 0
	}
	return digestBytes
}

// evictOne sheds the least valuable item from this (coldest) series:
// raw samples first (largest, shortest-lived), then hourly digests, then
// daily, then pending accumulators; reports the byte delta and whether the
// series is now completely empty.
func (sr *series) evictOne(st *Stats) (delta int64, empty bool) {
	switch {
	case sr.raw.len() > 0:
		sr.raw.dropFront()
		delta = -sampleBytes
	case sr.hourly.len() > 0:
		sr.hourly.dropFront()
		delta = -digestBytes
	case sr.daily.len() > 0:
		sr.daily.dropFront()
		delta = -digestBytes
	case sr.acc != nil:
		delta = -sr.acc.bytes()
		sr.acc = nil
	case sr.day != nil:
		delta = -digestBytes
		sr.day = nil
	default:
		return 0, true
	}
	st.EvictedSeriesItems++
	sr.bytes += delta
	return delta, sr.raw.len() == 0 && sr.hourly.len() == 0 && sr.daily.len() == 0 && sr.acc == nil && sr.day == nil
}
