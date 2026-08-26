// Package confidence holds the "earned, not lost" confidence model shared by
// the cloud sizing domains.
//
// A score starts at zero and adds only what the evidence demonstrates, so a
// missing signal cannot be mistaken for a present one. Each domain still owns
// its own factors, weights and prose — those are claims about that domain's
// evidence and do not transfer. What lives here is the machinery underneath
// them: the factor record, the accumulator with its clamp, the reporting
// projection [WeakestFactor], and the one factor whose arithmetic is genuinely
// identical across domains ([WindowFactor]).
//
// This is deliberately NOT the only confidence model in the repo.
// [github.com/agenticode/kilter/pkg/decision.Compose] is a multiplicative one
// with different semantics (a zero term vetoes the score; the result is
// rounded to two decimals), and pkg/ecs and pkg/domain/fargate use it because
// they inherit the recommender's historical formula. The two models are not
// interchangeable and merging them would move shipped numbers. See
// FINDINGS.md.
package confidence

import (
	"fmt"
	"math"
	"time"
)

// Factor is one earned component of a confidence score.
//
// The JSON shape is load-bearing: it is what a report serializes and what an
// operator reads to know which measurement to go fix.
type Factor struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Earned float64 `json:"earned"` // 0..1
	Why    string  `json:"why"`
}

// Confidence is a score built from nothing. It starts at zero and adds only
// what the evidence earns, so a missing signal cannot be mistaken for a
// present one; a domain's own MinConfidence is the bar it has to clear.
//
// Factors are appended in call order and never sorted, so the slice — and
// therefore [WeakestFactor] and the serialized report — is deterministic.
type Confidence struct {
	Score   float64  `json:"score"`
	Factors []Factor `json:"factors,omitempty"`
}

// Add appends a factor and credits weight×earned to the score.
//
// earned is clamped to [0,1], and a non-finite earned — NaN or ±Inf — is not
// evidence of anything and earns 0. That last clause is the difference from
// [Confidence.AddBounded]; prefer this one.
func (c *Confidence) Add(name string, weight, earned float64, why string) {
	if !finite(earned) || earned < 0 {
		earned = 0
	}
	if earned > 1 {
		earned = 1
	}
	c.append(name, weight, earned, why)
}

// AddBounded is [Confidence.Add] with the non-finite guard omitted: earned is
// clamped by comparison alone. NaN is neither < 0 nor > 1, so it passes
// through and poisons Score; +Inf clamps to 1 rather than earning 0.
//
// This exists because pkg/ec2 shipped with exactly this clamp and its scores
// are its shipped output. Adopting Add there is a one-word diff and a
// deliberate behaviour change, not a refactor — which is the point of keeping
// the two spellings distinguishable rather than averaging them.
func (c *Confidence) AddBounded(name string, weight, earned float64, why string) {
	if earned < 0 {
		earned = 0
	}
	if earned > 1 {
		earned = 1
	}
	c.append(name, weight, earned, why)
}

func (c *Confidence) append(name string, weight, earned float64, why string) {
	c.Factors = append(c.Factors, Factor{Name: name, Weight: weight, Earned: earned, Why: why})
	c.Score += weight * earned
}

// finite is pkg/lambda's guard, character for character, so the lift cannot
// have quietly changed which values count as evidence.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// WeakestFactor names the factor that cost the most confidence, so a
// low-confidence refusal says what would fix it rather than only that it
// failed.
//
// This is a reporting surface, not an internal detail: an operator reads it to
// know what to go measure. Loss is Weight×(1−Earned) and ties resolve to the
// earliest factor, so the answer depends only on the order factors were added.
func WeakestFactor(c Confidence) string {
	worst, lost := "", -1.0
	for _, f := range c.Factors {
		if l := f.Weight * (1 - f.Earned); l > lost {
			worst, lost = f.Name+": "+f.Why, l
		}
	}
	if worst == "" {
		return "no single dominant factor"
	}
	return worst
}

// FactorWindow is the shared name of the observation-span factor. It is a
// constant because it is printed by [WeakestFactor] into a refusal an operator
// reads and a test asserts on.
const FactorWindow = "window"

// WindowFactor is the observation-span factor: the observed span as a fraction
// of the minimum the domain requires, and the prose saying so.
//
// The returned fraction is uncapped — clamping is [Confidence.Add]'s job, and
// routing it through there is what keeps a domain's non-finite policy in one
// place. A non-positive minimum earns nothing: a domain that cannot say how
// much window it needs has not been shown that it got enough.
//
// round controls only the prose. It is a parameter rather than a constant
// because it is a domain fact: an EC2 window is quoted against a multi-day
// minimum and rounds to the hour, a Lambda window against a minimum of hours
// and rounds to the minute. Rounding a 6-hour Lambda minimum to the hour is
// not a formatting preference, it is a wrong number.
//
// The division is deliberately Seconds()/Seconds() and not
// float64(observed)/float64(minimum): the two disagree in the last bit for
// some inputs, and this factor's value is shipped output.
func WindowFactor(observed, minimum time.Duration, observedText string, round time.Duration) (earned float64, why string) {
	if minimum > 0 {
		earned = observed.Seconds() / minimum.Seconds()
	}
	return earned, fmt.Sprintf("observed %s against a %s minimum", observedText, minimum.Round(round))
}
