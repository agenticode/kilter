package decision

import (
	"fmt"
	"math"
	"time"

	"github.com/agenticode/kilter/pkg/patterns"
)

// RefusalCode is the machine-readable reason class of a refusal.
type RefusalCode string

const (
	CodeInsufficientHistory RefusalCode = "insufficient-history"
	CodePostChangeSoak      RefusalCode = "post-change-soak"
	CodeClassUnstable       RefusalCode = "class-unstable"
	CodeSignalConflict      RefusalCode = "signal-conflict"
	CodeRegimeChangePending RefusalCode = "regime-change-pending"
	CodeForecastDivergence  RefusalCode = "forecast-divergence"
	CodeSLADegraded         RefusalCode = "sla-degraded"
	CodeQuarantined         RefusalCode = "quarantined"
)

// Refusal is a first-class "we refuse to recommend, because X, until Y".
// Code is machine-readable, Detail is a deterministic human sentence, and
// Until — when the clearing time is computable — says when the condition
// likely clears (zero when unknown).
type Refusal struct {
	Code   RefusalCode `json:"code"`
	Detail string      `json:"detail"`
	Until  time.Time   `json:"until,omitzero"`
}

// Evidence is the per-subject fact sheet the refusal predicates and Decide
// evaluate. Callers (the wiring in pkg/recommend, fed by the evidence
// substrate) fill what they know; zero values mean "signal absent", which
// every predicate treats as "no grounds to refuse" — except where a
// documented fail-safe applies (see each predicate).
type Evidence struct {
	// History.
	Samples    int           // learned samples for this subject
	Window     time.Duration // span from first to last sample
	LastSample time.Time     // newest sample timestamp (zero = none)

	// Classification.
	Class patterns.Class // current behavior class (empty/unknown OK)
	// ClassStabilityKnown gates the ClassStability check: only consult the
	// fraction when the caller actually tracked it. An explicit flag beats
	// a NaN sentinel — absence of tracking is a fact, not a garbage value.
	ClassStabilityKnown bool
	ClassStability      float64   // fraction of recent re-classifications agreeing, 0..1
	LastClassFlip       time.Time // most recent class change (zero = never observed)

	// Change events (deploys, resizes, HPA min/max changes).
	LastChange time.Time // most recent change event (zero = none)

	// Conflicting signals.
	ShrinkIndicated   bool    // the sizing math wants to shrink this subject
	OOMsInWindow      int     // OOMKills observed inside the learning window
	ThrottledInWindow bool    // sustained CFS throttling inside the window
	HPAThrashPerHour  float64 // HPA direction-flip EWMA, flips/hour

	// Regime.
	LastChangepoint time.Time // most recent CUSUM regime-change (zero = none)

	// Forecasts on the horizon that matters; <= 0 or non-finite means the
	// forecaster is unavailable (the remote one is an optional organ).
	BuiltinForecast float64
	RemoteForecast  float64

	// Optional SLO signal and regression quarantine.
	SLODegraded      bool
	Quarantined      bool
	QuarantineReason string
}

// Class soak multipliers. The soak must span the workload's characteristic
// period so post-change history covers representative behavior:
//
//	steady/unknown/growing ×1 — flat (or trend-dominated) series
//	  re-establish a representative level within the base soak; growth is
//	  handled by the trend machinery, the soak only guards the level shift.
//	bursty ×2 — heavy tails need roughly twice the coverage before the
//	  post-change tail is believable.
//	diurnal ×4 — one full daily cycle (24h at the 6h default base) must
//	  pass before the distribution covers both peak and trough.
//	batch ×4 — active windows are sparse; a day typically covers at least
//	  one run of a nightly job.
const (
	soakSteadyMul  = 1
	soakGrowingMul = 1
	soakBurstyMul  = 2
	soakDiurnalMul = 4
	soakBatchMul   = 4
)

// maxSoak bounds any class-scaled soak so a pathological base config cannot
// produce an effectively-permanent refusal window.
const maxSoak = 7 * 24 * time.Hour

// SoakFor returns the class-scaled post-change soak. A non-positive base
// falls back to the package default (garbage config must not disable the
// soak), and the result is capped at maxSoak.
func SoakFor(class patterns.Class, base time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultConfig().BaseSoak
	}
	mul := time.Duration(soakSteadyMul)
	switch class {
	case patterns.ClassBursty:
		mul = soakBurstyMul
	case patterns.ClassDiurnal:
		mul = soakDiurnalMul
	case patterns.ClassBatch:
		mul = soakBatchMul
	case patterns.ClassGrowing:
		mul = soakGrowingMul
	}
	// Clamp the base to the cap *before* multiplying: base*mul on an
	// absurd (but Validate-passing) base overflows int64 nanoseconds and
	// wraps negative, and a negative soak makes every soak comparison
	// (now.Sub(event) >= soak) trivially true — the refusal would silently
	// disappear. Clamping first keeps the product bounded by 4*maxSoak,
	// which is ~5e15ns, nowhere near overflow.
	if base > maxSoak {
		base = maxSoak
	}
	soak := base * mul
	if soak > maxSoak {
		soak = maxSoak
	}
	return soak
}

// Predicate is one refusal check. Each returns nil (no grounds) or a fully
// populated Refusal. Predicates are pure; now is the decision instant.
type Predicate func(ev Evidence, cfg Config, now time.Time) *Refusal

// untilProjectionCap bounds the insufficient-history clearing estimate:
// projecting more than 30 days out from sparse-sample arithmetic is noise,
// and an absurd cfg.MinSamples must not overflow time arithmetic.
const untilProjectionCap = 30 * 24 * time.Hour

// RefuseInsufficientHistory refuses when samples < MinSamples or the
// observed window < MinWindow — the condition the recommender used to skip
// silently. Until is estimated from the observed sampling rate when
// computable (capped at 30 days out), zero otherwise.
func RefuseInsufficientHistory(ev Evidence, cfg Config, now time.Time) *Refusal {
	if ev.Samples >= cfg.MinSamples && ev.Window >= cfg.MinWindow {
		return nil
	}
	r := &Refusal{
		Code: CodeInsufficientHistory,
		Detail: fmt.Sprintf("history is insufficient: %d samples over %.1fh; sizing needs at least %d samples spanning %.1fh",
			ev.Samples, ev.Window.Hours(), cfg.MinSamples, cfg.MinWindow.Hours()),
	}
	if ev.LastSample.IsZero() {
		return r
	}
	var wait time.Duration
	if ev.Window < cfg.MinWindow {
		wait = cfg.MinWindow - ev.Window
	}
	if ev.Samples < cfg.MinSamples && ev.Samples >= 2 && ev.Window > 0 {
		rate := ev.Window / time.Duration(ev.Samples-1)
		if byRate := rate * time.Duration(cfg.MinSamples-ev.Samples); byRate > wait {
			wait = byRate
		}
	}
	if wait <= 0 {
		return r // samples short with no usable rate estimate
	}
	if wait > untilProjectionCap {
		wait = untilProjectionCap
	}
	r.Until = ev.LastSample.Add(wait)
	return r
}

// RefusePostChangeSoak refuses while a deploy/resize/scale event is inside
// the class-scaled soak window. A change timestamped in the future (clock
// skew) still refuses — the change is at most brand new. Until is the end
// of the soak.
func RefusePostChangeSoak(ev Evidence, cfg Config, now time.Time) *Refusal {
	if ev.LastChange.IsZero() {
		return nil
	}
	soak := SoakFor(ev.Class, cfg.BaseSoak)
	if now.Sub(ev.LastChange) >= soak {
		return nil
	}
	return &Refusal{
		Code: CodePostChangeSoak,
		Detail: fmt.Sprintf("a change event at %s is inside the %.1fh post-change soak for class %q; post-change behavior is not yet representative",
			ev.LastChange.UTC().Format(time.RFC3339), soak.Hours(), string(ev.Class)),
		Until: ev.LastChange.Add(soak),
	}
}

// RefuseClassUnstable refuses when the classifier flipped within
// ClassFlipWindow, or when tracked class stability is below
// MinClassStability. The stability comparison is positive-form, so a
// tracked-but-NaN stability refuses (fail-safe): claiming to know the
// stability and producing garbage is grounds for distrust, not a pass.
func RefuseClassUnstable(ev Evidence, cfg Config, now time.Time) *Refusal {
	if !ev.LastClassFlip.IsZero() && now.Sub(ev.LastClassFlip) < cfg.ClassFlipWindow {
		return &Refusal{
			Code: CodeClassUnstable,
			Detail: fmt.Sprintf("behavior class flipped at %s, within the last %.1fh; class-adaptive policy is unreliable until the class settles",
				ev.LastClassFlip.UTC().Format(time.RFC3339), cfg.ClassFlipWindow.Hours()),
			Until: ev.LastClassFlip.Add(cfg.ClassFlipWindow),
		}
	}
	if ev.ClassStabilityKnown {
		// A tracked-but-unusable fraction (NaN, negative, > 1) is grounds
		// for distrust, but it must not be formatted into the sentence:
		// "class stability NaN is below the required 0.70" tells an
		// operator nothing. Say what actually happened instead.
		if !(ev.ClassStability >= 0 && ev.ClassStability <= 1) {
			return &Refusal{
				Code:   CodeClassUnstable,
				Detail: "class stability was tracked but did not yield a usable fraction; a classifier that cannot report its own agreement is no basis for class-adaptive policy",
			}
		}
		// A garbage threshold must not disable the check either: fall back
		// to the documented default rather than comparing against NaN.
		minStability := cfg.MinClassStability
		if !(minStability >= 0 && minStability <= 1) {
			minStability = DefaultConfig().MinClassStability
		}
		if !(ev.ClassStability >= minStability) {
			return &Refusal{
				Code: CodeClassUnstable,
				Detail: fmt.Sprintf("class stability %.2f is below the required %.2f; recent re-classifications disagree too often to trust class-adaptive policy",
					ev.ClassStability, minStability),
			}
		}
	}
	return nil
}

// RefuseSignalConflict refuses when the evidence contradicts itself: a
// shrink is indicated while OOM or throttling events sit in the same
// window, or the HPA is thrashing so replica-level usage mixes regimes.
// The thrash comparison is positive-form, so a NaN score (or NaN config
// threshold) refuses rather than silently passing.
func RefuseSignalConflict(ev Evidence, cfg Config, now time.Time) *Refusal {
	if ev.ShrinkIndicated && (ev.OOMsInWindow > 0 || ev.ThrottledInWindow) {
		what := "CFS throttling"
		if ev.OOMsInWindow > 0 {
			what = fmt.Sprintf("%d OOMKill(s)", ev.OOMsInWindow)
		}
		return &Refusal{
			Code: CodeSignalConflict,
			Detail: fmt.Sprintf("shrink is indicated but %s occurred in the same window; the usage history and the failure signals disagree",
				what),
		}
	}
	if !(ev.HPAThrashPerHour < cfg.MaxHPAThrashPerHour) {
		return &Refusal{
			Code: CodeSignalConflict,
			Detail: fmt.Sprintf("HPA is thrashing (%.1f direction flips/hour, threshold %.1f); per-replica usage mixes scaling regimes and sizing is unreliable",
				ev.HPAThrashPerHour, cfg.MaxHPAThrashPerHour),
		}
	}
	return nil
}

// RefuseRegimeChangePending refuses while the post-changepoint window is
// still shorter than the class-scaled soak: the distribution has shifted
// and the engine has not yet re-learned it. Until is the end of the soak.
func RefuseRegimeChangePending(ev Evidence, cfg Config, now time.Time) *Refusal {
	if ev.LastChangepoint.IsZero() {
		return nil
	}
	soak := SoakFor(ev.Class, cfg.BaseSoak)
	if now.Sub(ev.LastChangepoint) >= soak {
		return nil
	}
	return &Refusal{
		Code: CodeRegimeChangePending,
		Detail: fmt.Sprintf("a regime change was detected at %s and the post-changepoint window is still inside the %.1fh soak; the engine is re-learning the new level",
			ev.LastChangepoint.UTC().Format(time.RFC3339), soak.Hours()),
		Until: ev.LastChangepoint.Add(soak),
	}
}

// RefuseForecastDivergence refuses when both forecasters are available and
// disagree beyond tolerance. Either forecast unavailable (non-positive,
// non-finite, or beyond maxAbsSample) is not grounds to refuse — the remote
// forecaster is optional. The tolerance comparison is positive-form, so a
// NaN config threshold refuses when both forecasts are present.
func RefuseForecastDivergence(ev Evidence, cfg Config, now time.Time) *Refusal {
	a, b := ev.BuiltinForecast, ev.RemoteForecast
	if !(a > 0 && a <= maxAbsSample) || !(b > 0 && b <= maxAbsSample) {
		return nil
	}
	div := math.Abs(a-b) / math.Max(a, b)
	if div <= cfg.MaxForecastDivergence {
		return nil
	}
	return &Refusal{
		Code: CodeForecastDivergence,
		Detail: fmt.Sprintf("built-in and remote forecasts diverge by %.0f%% (%.0f vs %.0f, tolerance %.0f%%); at least one model is wrong about the horizon that matters",
			div*100, a, b, cfg.MaxForecastDivergence*100),
	}
}

// RefuseSLADegraded refuses when the operator-declared SLO signal has
// degraded since kilter's last change.
func RefuseSLADegraded(ev Evidence, cfg Config, now time.Time) *Refusal {
	if !ev.SLODegraded {
		return nil
	}
	return &Refusal{
		Code:   CodeSLADegraded,
		Detail: "the declared SLO signal has degraded since the last change; sizing pauses until the SLO recovers",
	}
}

// RefuseQuarantined surfaces the regression quarantine as a refusal rather
// than silence.
func RefuseQuarantined(ev Evidence, cfg Config, now time.Time) *Refusal {
	if !ev.Quarantined {
		return nil
	}
	detail := "the subject is quarantined by the regression detector; no recommendations until the quarantine lifts"
	if ev.QuarantineReason != "" {
		detail = fmt.Sprintf("the subject is quarantined by the regression detector (%s); no recommendations until the quarantine lifts", ev.QuarantineReason)
	}
	return &Refusal{Code: CodeQuarantined, Detail: detail}
}

// predicates is the documented evaluation order (design §4.2, table order):
// first match wins. A slice, not a map — the order is part of the contract
// and must never depend on iteration order.
var predicates = []Predicate{
	RefuseInsufficientHistory,
	RefusePostChangeSoak,
	RefuseClassUnstable,
	RefuseSignalConflict,
	RefuseRegimeChangePending,
	RefuseForecastDivergence,
	RefuseSLADegraded,
	RefuseQuarantined,
}

// Evaluate runs every refusal predicate in the documented order and returns
// the first match, or nil when there are no grounds to refuse. Deterministic:
// identical inputs produce identical output.
func Evaluate(ev Evidence, cfg Config, now time.Time) *Refusal {
	for _, p := range predicates {
		if r := p(ev, cfg, now); r != nil {
			return r
		}
	}
	return nil
}
