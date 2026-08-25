package decision

import (
	"math"
	"time"
)

// FloorConfig tunes the OOM-floor time decay (design §4.3.4).
type FloorConfig struct {
	// Hold is how long the floor stays at full strength after the last
	// OOM. Default 14 days: two weekly business cycles must pass OOM-free
	// before an OOM-derived constraint may start to relax.
	Hold time.Duration
	// DecayPerWeek is the fraction of the floor-above-observed gap shed
	// per week after Hold. Default 0.10: the floor halves its excess in
	// ~6.6 weeks, slow enough that a monthly OOM pattern re-arms it long
	// before it fades. Must be in (0,1).
	DecayPerWeek float64
}

// DefaultFloorConfig returns the documented defaults.
func DefaultFloorConfig() FloorConfig {
	return FloorConfig{Hold: 14 * 24 * time.Hour, DecayPerWeek: 0.10}
}

// withDefaults substitutes documented defaults for out-of-range or NaN
// fields, mirroring pkg/guard's BreakerConfig behavior: a floor guards
// against OOMs, so a garbage config must yield the conservative default,
// never a disabled floor.
func (c FloorConfig) withDefaults() FloorConfig {
	d := DefaultFloorConfig()
	if c.Hold <= 0 {
		c.Hold = d.Hold
	}
	if !(c.DecayPerWeek > 0) || !(c.DecayPerWeek < 1) { // catches NaN
		c.DecayPerWeek = d.DecayPerWeek
	}
	return c
}

// EffectiveOOMFloor returns the memory floor (bytes) to apply at `now`,
// given the armed floor (OOMBumpRatio × the level that OOMed), the time of
// the last OOM, and the observed-peak sizing term the floor relaxes toward.
//
// Semantics (pure, deterministic):
//
//   - no floor armed (floorBytes <= 0) or no OOM ever (lastOOM zero) → 0;
//   - within Hold of the last OOM → the full floor (an OOM is an OOM);
//   - after Hold, the floor-above-observed gap decays geometrically at
//     DecayPerWeek, continuously:
//     floor(t) = observed + ⌈gap·(1−rate)^weeks⌉, and relaxes to exactly
//     observed once the remaining excess falls below one byte;
//   - the result never drops below observedBytes and never exceeds
//     floorBytes; if the floor is already at or below the observed term it
//     is returned unchanged (the max() in sizing makes it inert anyway);
//   - now before lastOOM (clock skew) counts as age 0 → full floor;
//   - negative observedBytes clamps to 0.
//
// A new OOM re-arms the floor simply by the caller passing the new
// (floorBytes, lastOOM) pair. Rounding is upward (ceil) — conservative for
// memory.
func EffectiveOOMFloor(floorBytes int64, lastOOM, now time.Time, observedBytes int64, cfg FloorConfig) int64 {
	if floorBytes <= 0 || lastOOM.IsZero() {
		return 0
	}
	if observedBytes < 0 {
		observedBytes = 0
	}
	if floorBytes <= observedBytes {
		return floorBytes
	}
	cfg = cfg.withDefaults()
	age := now.Sub(lastOOM)
	if age <= cfg.Hold {
		return floorBytes
	}
	weeks := float64(age-cfg.Hold) / float64(7*24*time.Hour)
	factor := math.Pow(1-cfg.DecayPerWeek, weeks) // in [0,1]: DecayPerWeek is in (0,1) and weeks > 0

	// Decay the *gap* and add it back in integer space rather than
	// computing the absolute floor as a float64. Converting an
	// out-of-range float64 to int64 is implementation-defined in Go
	// (arm64 saturates to MaxInt64, amd64 yields MinInt64), and
	// float64(floorBytes) rounds *up* past MaxInt64 for a floor near the
	// int64 ceiling. On amd64 the wrapped value would then be clamped
	// down to observedBytes — a silently *lower* memory floor than the OOM
	// evidence supports, which is the one direction a floor must never
	// fail. gap is exact (0 < gap <= MaxInt64, no overflow: floorBytes >
	// observedBytes >= 0) and observedBytes+add can never exceed
	// floorBytes, so no conversion here can leave the int64 domain.
	gap := floorBytes - observedBytes
	raw := float64(gap) * factor
	var add int64
	switch {
	case math.IsNaN(raw):
		// Un-computable decay: keep the whole floor. Memory fails high.
		add = gap
	case !(raw >= 1):
		// Less than a byte of excess left: fully relaxed to the observed
		// term, which is what "relaxes toward the observed peak" means.
		add = 0
	case !(raw < float64(math.MaxInt64)):
		add = gap // not representable as int64: treat the gap as undecayed
	default:
		add = int64(math.Ceil(raw)) // round up: conservative for memory
	}
	out := observedBytes + add
	// Belt and braces: the result must always land in [observed, floor].
	if out < observedBytes {
		out = observedBytes
	}
	if out > floorBytes {
		out = floorBytes
	}
	return out
}

// SustainedPeak returns the highest level the series held for at least
// `run` consecutive samples: the maximum over all length-`run` windows of
// the window minimum. This is the "verified sustained peak" of the
// spike-robust sizing formula — a one-sample balloon can never set it,
// while any plateau of `run` samples does.
//
// Input hygiene: NaN, ±Inf and absurd magnitudes (> maxAbsSample) are
// dropped before windowing (pipeline garbage must not set a peak);
// remaining neighbors are treated as adjacent, which can only join
// excursions and raise the estimate — the conservative (bigger-memory)
// direction. Negative samples clamp to 0.
//
// Degenerate inputs: run < 1 is treated as 1 (plain max); fewer valid
// samples than `run` shrinks the window to what exists (the min of the
// whole series — the only level provably sustained throughout); no valid
// samples → 0.
func SustainedPeak(vals []float64, run int) float64 {
	clean := make([]float64, 0, len(vals))
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) || v > maxAbsSample {
			continue // garbage is dropped, never stored
		}
		if v < 0 {
			v = 0 // finite negatives clamp, matching histogram.AddSample
		}
		clean = append(clean, v)
	}
	if len(clean) == 0 {
		return 0
	}
	if run < 1 {
		run = 1
	}
	if run > len(clean) {
		run = len(clean)
	}
	best := 0.0
	for i := 0; i+run <= len(clean); i++ {
		windowMin := clean[i]
		for j := i + 1; j < i+run; j++ {
			if clean[j] < windowMin {
				windowMin = clean[j]
			}
		}
		if windowMin > best {
			best = windowMin
		}
	}
	return best
}

// RobustPeak combines the two spike-robust peak terms of the design:
// max(high-percentile-of-decayed-histogram, verified sustained peak).
// Garbage in either term (NaN, ±Inf, negative) contributes 0 rather than
// poisoning the max.
func RobustPeak(histHighPercentile, sustainedPeak float64) float64 {
	a, b := histHighPercentile, sustainedPeak
	if !(a >= 0 && a <= maxAbsSample) {
		a = 0
	}
	if !(b >= 0 && b <= maxAbsSample) {
		b = 0
	}
	return math.Max(a, b)
}
