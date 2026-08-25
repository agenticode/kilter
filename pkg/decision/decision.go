// Package decision implements the deterministic decision-quality core of the
// reasoning engine: structured confidence composed from named evidence terms,
// refusal as a first-class output, online regime-change detection (two-sided
// winsorized CUSUM), and evidence-backed floors — an OOM memory floor with
// time decay and a spike-robust peak estimator.
//
// Everything here is pure and deterministic: no clocks (callers pass now), no
// I/O, no hidden global state, and no map iteration in any output path.
// Garbage inputs — NaN, ±Inf, negative counts, zero windows — are rejected or
// clamped explicitly, and every such path degrades toward the safe side:
// lower confidence, refusal rather than action, higher memory floors.
// Comparisons are written in positive form (`!(x > 0)`) so NaN can never
// slip through a threshold.
//
// Dependency direction is strictly downward: pkg/recommend imports this
// package; this package never imports pkg/recommend.
package decision

import (
	"fmt"
	"math"
	"time"
)

// maxAbsSample bounds accepted observation magnitudes, matching the rationale
// in pkg/patterns: 1e18 is far beyond any real usage telemetry (an exabyte of
// memory bytes, a quadrillion millicores), so anything above it is pipeline
// garbage. Keeping every accepted value ≤ 1e18 also keeps all downstream
// arithmetic (deltas, squared deviations, EWMA products) comfortably finite.
const maxAbsSample = 1e18

// maxConfigWindow bounds the duration knobs. 30 days is already an extreme
// operating point — a month of clean history required before kilter will
// size anything, or a month of suppression after a class flip — so beyond
// it the value is a typo, not an intent. The bound is not cosmetic:
// unbounded durations overflow the class-scaled soak arithmetic in SoakFor
// (base*4 wraps negative past ~73 years), and a negative soak makes every
// soak comparison trivially true, silently deleting two refusals.
const maxConfigWindow = 30 * 24 * time.Hour

// Config tunes the refusal predicates and the act threshold. Every value has
// a documented rationale; Validate rejects garbage (including NaN) so a bad
// config fails at startup, not silently at decision time. If a garbage
// config does reach Evaluate/Decide anyway, the positive-form comparisons
// there degrade toward refusal / recommend-only, never toward act.
type Config struct {
	// MinSamples is the sample count below which history is insufficient.
	// Default 30, matching recommend.Config.MinSamples so the refusal
	// surfaces exactly where the recommender silently skipped before.
	MinSamples int
	// MinWindow is the observation span below which history is
	// insufficient. Default 6h, matching recommend.Config.MinWindow.
	MinWindow time.Duration
	// BaseSoak is the post-change soak for steady workloads; other classes
	// scale it (see SoakFor). Default 6h per the design (§4.2): long enough
	// to cover a deploy's warm-up transient (JVM heap growth, cache fill)
	// at 5-minute sampling, short enough not to stall a healthy fleet.
	BaseSoak time.Duration
	// ClassFlipWindow refuses while a classifier flip is recent. Default
	// 24h per the design: one full daily cycle must pass with a stable
	// class before class-adaptive policy is trusted again.
	ClassFlipWindow time.Duration
	// MinClassStability is the minimum fraction of recent re-classifications
	// that must agree. Default 0.7 per the design (§4.2): with the ~5
	// classes available, 0.7 is far above chance agreement while tolerating
	// occasional flicker between adjacent classes (steady↔diurnal).
	MinClassStability float64
	// MaxHPAThrashPerHour refuses when the HPA direction-flip EWMA exceeds
	// it. Default 2.0: a healthy diurnal HPA reverses direction about twice
	// a day (~0.1/h); a sustained 2/h means replicas oscillate every ≤30min,
	// so per-replica usage mixes regimes and sizing math is unreliable.
	MaxHPAThrashPerHour float64
	// MaxForecastDivergence refuses when built-in and remote forecasts
	// disagree by more than this relative fraction. Default 0.35: larger
	// than MemoryHeadroom−1 (0.20) plus MinChangeRatio (0.10) with margin,
	// i.e. a disagreement too large for standard headroom to absorb if the
	// engine follows the wrong model.
	MaxForecastDivergence float64
	// ActConfidence is the minimum Confidence.Score for ActionAct.
	// Default 0.6, matching plan.Config.MinConfidence so a Verdict that
	// says "act" is exactly one the planner would accept today.
	ActConfidence float64
}

// DefaultConfig returns production-grade defaults (rationale on each field).
func DefaultConfig() Config {
	return Config{
		MinSamples:            30,
		MinWindow:             6 * time.Hour,
		BaseSoak:              6 * time.Hour,
		ClassFlipWindow:       24 * time.Hour,
		MinClassStability:     0.7,
		MaxHPAThrashPerHour:   2.0,
		MaxForecastDivergence: 0.35,
		ActConfidence:         0.6,
	}
}

// Validate rejects out-of-range and NaN configuration. Positive-form
// comparisons make NaN in any float field fail validation.
func (c Config) Validate() error {
	if c.MinSamples < 1 {
		return fmt.Errorf("decision: MinSamples %d must be >= 1", c.MinSamples)
	}
	if c.MinWindow <= 0 || c.MinWindow > maxConfigWindow {
		return fmt.Errorf("decision: MinWindow %v out of (0,%v]", c.MinWindow, maxConfigWindow)
	}
	// BaseSoak is bounded by the same cap SoakFor enforces, so a config
	// that validates cannot mean something different from what SoakFor
	// will actually do with it.
	if c.BaseSoak <= 0 || c.BaseSoak > maxSoak {
		return fmt.Errorf("decision: BaseSoak %v out of (0,%v]", c.BaseSoak, maxSoak)
	}
	if c.ClassFlipWindow <= 0 || c.ClassFlipWindow > maxConfigWindow {
		return fmt.Errorf("decision: ClassFlipWindow %v out of (0,%v]", c.ClassFlipWindow, maxConfigWindow)
	}
	if !(c.MinClassStability >= 0) || !(c.MinClassStability <= 1) {
		return fmt.Errorf("decision: MinClassStability %v out of [0,1]", c.MinClassStability)
	}
	if !(c.MaxHPAThrashPerHour > 0) || math.IsInf(c.MaxHPAThrashPerHour, 0) {
		return fmt.Errorf("decision: MaxHPAThrashPerHour %v must be finite and > 0", c.MaxHPAThrashPerHour)
	}
	if !(c.MaxForecastDivergence > 0) || !(c.MaxForecastDivergence < 1) {
		return fmt.Errorf("decision: MaxForecastDivergence %v out of (0,1)", c.MaxForecastDivergence)
	}
	if !(c.ActConfidence > 0) || !(c.ActConfidence <= 1) {
		return fmt.Errorf("decision: ActConfidence %v out of (0,1]", c.ActConfidence)
	}
	return nil
}

// Action is the disposition of a Verdict.
type Action string

const (
	// ActionAct: confidence clears the act threshold and no refusal fired;
	// the recommendation may be applied (subject to guard/plan gates).
	ActionAct Action = "act"
	// ActionRecommendOnly: no refusal, but confidence is below the act
	// threshold; surface the recommendation, do not apply it.
	ActionRecommendOnly Action = "recommend-only"
	// ActionRefuse: a refusal predicate fired; no recommendation should be
	// surfaced as actionable. The Refusal says why and until when.
	ActionRefuse Action = "refuse"
)

// Verdict is the decision-quality envelope around one sizing decision.
// The recommendation itself is attached by the caller (pkg/recommend);
// carrying it here would invert the dependency direction.
type Verdict struct {
	Action     Action     `json:"action"`
	Confidence Confidence `json:"confidence"`
	Refusal    *Refusal   `json:"refusal,omitempty"`
}

// Decide runs the refusal predicates in their documented order, then maps
// confidence onto act vs recommend-only. Fail-safe by construction: a NaN
// score, or an ActConfidence that is not a usable probability, yields
// recommend-only — never act.
func Decide(ev Evidence, conf Confidence, cfg Config, now time.Time) Verdict {
	if ref := Evaluate(ev, cfg, now); ref != nil {
		return Verdict{Action: ActionRefuse, Confidence: conf, Refusal: ref}
	}
	// Guard the threshold as well as the score. An unvalidated config can
	// carry ActConfidence = 0 (the zero value) or NaN, and `score >= 0`
	// would then license action on a subject with no evidence at all. A
	// threshold that is not a usable probability means the operator's
	// intent is unknown, and unknown intent authorizes nothing. Validate
	// is where a bad threshold is meant to be caught; this is the net.
	if !(cfg.ActConfidence > 0 && cfg.ActConfidence <= 1) {
		return Verdict{Action: ActionRecommendOnly, Confidence: conf}
	}
	if conf.Score >= cfg.ActConfidence {
		return Verdict{Action: ActionAct, Confidence: conf}
	}
	return Verdict{Action: ActionRecommendOnly, Confidence: conf}
}
