package decision

import "fmt"

// ShiftKind classifies an exceedance — a stretch of samples far from
// baseline — into the three hypotheses of the design (§4.3) plus the two
// honest fallbacks.
type ShiftKind string

const (
	// ShiftSpike: isolated excursion that reverted within a few samples.
	// Excluded from the memory-peak term; never triggers re-learning.
	ShiftSpike ShiftKind = "spike"
	// ShiftSeasonal: recurs at the daily lag; the decaying histogram
	// already covers it, no action needed.
	ShiftSeasonal ShiftKind = "seasonal"
	// ShiftTrend: gradual sustained drift; the domain of the trend
	// machinery (predictive headroom), not regime handling.
	ShiftTrend ShiftKind = "trend"
	// ShiftRegime: the level moved and holds. Triggers fast-forward decay
	// and the regime-change-pending refusal.
	ShiftRegime ShiftKind = "regime"
	// ShiftIndeterminate: not yet resolvable — the excursion has neither
	// reverted nor held long enough. Callers treat it as pending.
	ShiftIndeterminate ShiftKind = "indeterminate"
)

// spikeRevertMaxSamples: an excursion that reverts within this many samples
// (15 minutes at 5-min cadence) is an isolated anomaly regardless of what
// the CUSUM said — one balloon must not be promoted to a regime. Counted
// from 1, so the spike window is samples 1..3 inclusive.
const spikeRevertMaxSamples = 3

// regimeSustainMinSamples: the new level must hold at least this many
// consecutive samples (30 minutes at 5-min cadence) before a CUSUM firing
// is confirmed as a regime change rather than a fat burst.
const regimeSustainMinSamples = 6

// seasonalAutoCorrMin matches the diurnal classification threshold in
// pkg/patterns: recurrence with lag-24h autocorrelation above 0.5 is a
// season, and the decaying histogram already covers seasons.
const seasonalAutoCorrMin = 0.5

// trendPerDayMin matches the ClassGrowing threshold in pkg/patterns:
// a sustained drift above 10%/day is a trend.
const trendPerDayMin = 0.10

// ShiftFacts are the cheap statistics an exceedance is scored against.
// All fields are computable online by the caller.
type ShiftFacts struct {
	// CUSUMFired reports whether the changepoint detector fired for this
	// excursion.
	CUSUMFired bool
	// Direction of the excursion: +1 up, -1 down.
	Direction int
	// RevertedWithin is the number of samples the excursion lasted before
	// the series re-entered its pre-excursion baseline band. It is only
	// meaningful when >= 1: an excursion cannot revert in zero samples.
	// Zero (the struct's zero value) and negatives both mean "has not
	// reverted", so a caller that forgets to populate this field gets
	// "nothing is resolved yet", never "spike". That default matters:
	// spike is the one verdict with a shrinking consequence (the samples
	// are excluded from the memory peak term), so it must be earned by
	// positive evidence, never fallen into.
	RevertedWithin int
	// SustainedFor is the number of consecutive samples the new level has
	// held so far.
	SustainedFor int
	// AutoCorr24h is the lag-24h autocorrelation from patterns.Features.
	AutoCorr24h float64
	// TrendPerDay is the normalized slope from patterns.Features.
	TrendPerDay float64
}

// ClassifyShift scores an exceedance against the spike / seasonal / regime /
// trend hypotheses, in that documented precedence, and returns the kind with
// a deterministic human-readable reason. Rules (first match wins):
//
//  1. reverted after 1..spikeRevertMaxSamples samples → spike (even if
//     CUSUM fired);
//  2. lag-24h autocorrelation ≥ 0.5 → seasonal;
//  3. CUSUM fired and the level held ≥ regimeSustainMinSamples → regime;
//  4. |TrendPerDay| ≥ 0.10 → trend;
//  5. otherwise indeterminate (treat as pending).
//
// Every comparison is bounded on both sides and written in positive form,
// so NaN and ±Inf in AutoCorr24h or TrendPerDay fall through to
// indeterminate: garbage statistics can never manufacture a season or a
// trend, both of which mean "no regime handling needed".
func ClassifyShift(f ShiftFacts) (ShiftKind, string) {
	if f.RevertedWithin > 0 && f.RevertedWithin <= spikeRevertMaxSamples {
		return ShiftSpike, fmt.Sprintf("reverted within %d sample(s) (spike threshold %d)", f.RevertedWithin, spikeRevertMaxSamples)
	}
	// Autocorrelation is mathematically confined to [-1,1] (see
	// patterns.Features); anything outside that band is a broken producer,
	// not a season. Bounding the comparison keeps ±Inf from manufacturing
	// the "no action needed" verdict.
	if f.AutoCorr24h >= seasonalAutoCorrMin && f.AutoCorr24h <= 1 {
		return ShiftSeasonal, fmt.Sprintf("recurs at the 24h lag (autocorrelation %.2f ≥ %.2f)", f.AutoCorr24h, seasonalAutoCorrMin)
	}
	if f.CUSUMFired && f.SustainedFor >= regimeSustainMinSamples {
		dir := "up"
		if f.Direction < 0 {
			dir = "down"
		}
		return ShiftRegime, fmt.Sprintf("changepoint fired and the level held %s for %d samples (≥ %d)", dir, f.SustainedFor, regimeSustainMinSamples)
	}
	if (f.TrendPerDay >= trendPerDayMin && f.TrendPerDay <= maxAbsSample) ||
		(f.TrendPerDay <= -trendPerDayMin && f.TrendPerDay >= -maxAbsSample) {
		return ShiftTrend, fmt.Sprintf("sustained drift of %+.0f%%/day (threshold %.0f%%)", f.TrendPerDay*100, trendPerDayMin*100)
	}
	return ShiftIndeterminate, "neither reverted, recurring, held, nor trending yet"
}
