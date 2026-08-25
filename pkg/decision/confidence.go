package decision

import (
	"fmt"
	"math"
	"time"
)

// Confidence is a score with a basis: every number can say why it is
// believed. Score keeps the semantics of the recommender's historical single
// float (0..1, rounded to 2 decimals, thresholded downstream); Basis lists
// the named terms it was composed from.
type Confidence struct {
	Score float64          `json:"score"` // 0..1
	Basis []ConfidenceTerm `json:"basis,omitempty"`
}

// ConfidenceTerm is one named, individually testable evidence term.
// Value is a goodness in [0,1] (1 = full confidence contribution) and
// Weight is its exponent in the composition (default 1).
type ConfidenceTerm struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Weight float64 `json:"weight"`
	Note   string  `json:"note,omitempty"`
}

// maxTermWeight caps a term's exponent. Weights are expected to be ~1; the
// cap only exists so a corrupt weight cannot produce a meaninglessly
// hyper-penalized score. At weight 8 a single 0.9-valued term already drags
// the product below 0.43, which is as much influence as any one term should
// ever have.
const maxTermWeight = 8

// Compose combines terms into a Confidence via a weighted product:
//
//	Score = ∏ Value_i ^ Weight_i, rounded to 2 decimals.
//
// With the three legacy terms (history-depth, window-span, volatility) at
// weight 1 this reproduces the recommender's historical confidence formula
// exactly — back-compat by construction. Each additional term can only
// reduce the score (multiplicative penalty), which is the conservative
// direction for a gate that only acts above a threshold. A zero-valued term
// vetoes confidence entirely.
//
// Garbage never inflates: a non-finite or negative term Value is clamped to
// 0 (killing the score, loudly visible in Basis), Value > 1 clamps to 1, and
// a non-positive or NaN Weight clamps to 0 (the term is recorded but
// contributes nothing). No terms — or only weight-0 terms — means no
// evidence, and no evidence is zero confidence, not full confidence.
func Compose(terms ...ConfidenceTerm) Confidence {
	c := Confidence{Basis: make([]ConfidenceTerm, 0, len(terms))}
	score := 1.0
	effective := 0
	for _, t := range terms {
		if !(t.Value > 0) { // catches NaN, -Inf, negatives, zero
			t.Value = 0
		} else if t.Value > 1 { // catches +Inf too
			t.Value = 1
		}
		if !(t.Weight > 0) { // catches NaN, negatives, zero
			t.Weight = 0
		} else if t.Weight > maxTermWeight {
			t.Weight = maxTermWeight
		}
		c.Basis = append(c.Basis, t)
		if t.Weight > 0 {
			score *= math.Pow(t.Value, t.Weight)
			effective++
		}
	}
	if effective == 0 {
		return c // Score 0: no effective evidence terms
	}
	c.Score = math.Round(score*100) / 100
	return c
}

// TermHistoryDepth scores sample coverage: samples/saturation clamped to
// [0,1]. Pass saturation = 4×MinSamples to reproduce the recommender's
// historical curve (full credit at 4× the minimum history). Non-positive
// saturation is invalid input and scores 0 — garbage config must lower
// confidence, never raise it.
func TermHistoryDepth(samples, saturation int) ConfidenceTerm {
	t := ConfidenceTerm{Name: "history-depth", Weight: 1}
	if saturation <= 0 {
		t.Note = "invalid saturation"
		return t
	}
	if samples < 0 {
		samples = 0
	}
	t.Value = math.Min(1, float64(samples)/float64(saturation))
	t.Note = fmt.Sprintf("%d/%d samples", samples, saturation)
	return t
}

// TermWindowSpan scores observation span: window/saturation clamped to
// [0,1]. Pass saturation = 2×MinWindow to reproduce the recommender's
// historical curve. Non-positive saturation scores 0.
func TermWindowSpan(window, saturation time.Duration) ConfidenceTerm {
	t := ConfidenceTerm{Name: "window-span", Weight: 1}
	if saturation <= 0 {
		t.Note = "invalid saturation"
		return t
	}
	if window < 0 {
		window = 0
	}
	t.Value = math.Min(1, float64(window)/float64(saturation))
	t.Note = fmt.Sprintf("%.1fh/%.1fh", window.Hours(), saturation.Hours())
	return t
}

// TermVolatility scores behavioral stability from the spike rate:
// 1 − min(0.5, 5×spikeRate), the recommender's historical volatility
// penalty (a 10% spike rate already costs the maximum 0.5). A garbage rate
// (NaN, negative) scores 0 — an unknowable volatility is no basis for
// confidence.
func TermVolatility(spikeRate float64) ConfidenceTerm {
	t := ConfidenceTerm{Name: "volatility", Weight: 1}
	if !(spikeRate >= 0) { // catches NaN and negatives
		t.Note = "invalid spike rate"
		return t
	}
	t.Value = 1 - math.Min(0.5, spikeRate*5)
	t.Note = fmt.Sprintf("spike rate %.3f", spikeRate)
	return t
}

// TermClassStability scores classifier agreement: the fraction of recent
// re-classifications agreeing with the current class, clamped to [0,1].
// NaN scores 0.
func TermClassStability(agreeFrac float64) ConfidenceTerm {
	t := ConfidenceTerm{Name: "class-stability", Weight: 1}
	if !(agreeFrac > 0) {
		t.Note = fmt.Sprintf("agreement %.2f", 0.0)
		return t
	}
	t.Value = math.Min(1, agreeFrac)
	t.Note = fmt.Sprintf("agreement %.2f", t.Value)
	return t
}

// TermPostChangeSoak scores time since the last deploy/resize/scale event
// against the required soak: sinceChange/requiredSoak clamped to [0,1].
// requiredSoak <= 0 means no soak is required and scores neutral 1;
// a negative sinceChange (clock skew: change timestamped in the future)
// scores 0 — the change has effectively just happened.
func TermPostChangeSoak(sinceChange, requiredSoak time.Duration) ConfidenceTerm {
	t := ConfidenceTerm{Name: "post-change-soak", Weight: 1}
	if requiredSoak <= 0 {
		t.Value = 1
		t.Note = "no soak required"
		return t
	}
	if sinceChange < 0 {
		sinceChange = 0
	}
	t.Value = math.Min(1, float64(sinceChange)/float64(requiredSoak))
	t.Note = fmt.Sprintf("%.1fh/%.1fh since change", sinceChange.Hours(), requiredSoak.Hours())
	return t
}

// TermFreshness scores staleness of the newest sample: 1 at age 0 decaying
// linearly to 0 at maxAge. A non-positive maxAge is invalid and scores 0.
// Negative age (sample timestamped in the future — clock skew) clamps to 0,
// i.e. fully fresh; skew must not zero out confidence.
func TermFreshness(sinceLastSample, maxAge time.Duration) ConfidenceTerm {
	t := ConfidenceTerm{Name: "freshness", Weight: 1}
	if maxAge <= 0 {
		t.Note = "invalid max age"
		return t
	}
	if sinceLastSample < 0 {
		sinceLastSample = 0
	}
	t.Value = 1 - math.Min(1, float64(sinceLastSample)/float64(maxAge))
	t.Note = fmt.Sprintf("last sample %.1fh ago", sinceLastSample.Hours())
	return t
}

// TermSignalAgreement scores corroboration across independent signals:
// agreeing/total clamped to [0,1]. total <= 0 means no signals were
// consulted and scores neutral 1 (multiplicative identity: absence of
// signals is not evidence against).
func TermSignalAgreement(agreeing, total int) ConfidenceTerm {
	t := ConfidenceTerm{Name: "signal-agreement", Weight: 1}
	if total <= 0 {
		t.Value = 1
		t.Note = "no signals"
		return t
	}
	if agreeing < 0 {
		agreeing = 0
	}
	if agreeing > total {
		agreeing = total
	}
	t.Value = float64(agreeing) / float64(total)
	t.Note = fmt.Sprintf("%d/%d signals agree", agreeing, total)
	return t
}

// TermForecastAgreement scores built-in vs remote forecaster agreement on
// the horizon that matters: 1 − relative divergence. Either forecast
// unavailable (non-finite, non-positive, or beyond maxAbsSample) scores
// neutral 1 — the remote forecaster is an optional organ and its absence
// must not depress confidence.
func TermForecastAgreement(builtin, remote float64) ConfidenceTerm {
	t := ConfidenceTerm{Name: "forecast-agreement", Weight: 1}
	if !(builtin > 0 && builtin <= maxAbsSample) || !(remote > 0 && remote <= maxAbsSample) {
		t.Value = 1
		t.Note = "forecast unavailable"
		return t
	}
	div := math.Abs(builtin-remote) / math.Max(builtin, remote)
	t.Value = 1 - math.Min(1, div)
	t.Note = fmt.Sprintf("divergence %.2f", div)
	return t
}
