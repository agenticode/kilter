package ec2

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// The confidence model moved to pkg/confidence. This file is the proof that
// the move changed nothing: it carries the pre-lift implementation verbatim
// and asserts that the shipped path reproduces it BIT for bit — Score, every
// factor field, and the weakestFactor prose an operator reads out of a
// low-confidence refusal.
//
// The copies below are frozen. They are not maintained alongside the real
// implementation and must not be "fixed"; the moment a deliberate change to
// the model lands, this file's job is to fail, and the new behaviour is then
// re-pinned by editing the copies in the same commit that changes the model.

type legacyFactor struct {
	Name   string
	Weight float64
	Earned float64
	Why    string
}

type legacyConfidence struct {
	Score   float64
	Factors []legacyFactor
}

// legacyAdd is pkg/ec2's pre-lift add, character for character. Note what it
// does NOT do: NaN is neither < 0 nor > 1, so it survives both comparisons and
// poisons Score. That is the shipped behaviour, and the table below pins it.
func (c *legacyConfidence) add(name string, weight, earned float64, why string) {
	if earned < 0 {
		earned = 0
	}
	if earned > 1 {
		earned = 1
	}
	c.Factors = append(c.Factors, legacyFactor{Name: name, Weight: weight, Earned: earned, Why: why})
	c.Score += weight * earned
}

// legacyConfidenceOf is the pre-lift (*Sizer).confidence, verbatim.
func legacyConfidenceOf(cfg Config, obs Observation) legacyConfidence {
	var c legacyConfidence
	c.add("sample-coverage", weightCoverage, obs.Coverage,
		fmt.Sprintf("%d of ~%d expected datapoints", obs.Samples, obs.ExpectedSamples))

	windowEarned := 0.0
	if cfg.MinWindow > 0 {
		windowEarned = obs.Window.Duration().Seconds() / cfg.MinWindow.Seconds()
	}
	c.add("window", weightWindow, windowEarned,
		fmt.Sprintf("observed %s against a %s minimum", obs.Window.String(), cfg.MinWindow.Round(time.Hour)))

	memEarned, memWhy := 1.0, "memory observed via the CloudWatch agent"
	if obs.MemoryBlind {
		memEarned, memWhy = 0, "memory-blind: no CloudWatch agent, so no memory metric exists"
	}
	c.add("memory-signal", weightMemory, memEarned, memWhy)

	resEarned, resWhy := 1.0, fmt.Sprintf("%d-second datapoints", obs.PeriodSeconds)
	if obs.PeriodSeconds > PeriodDetailedSeconds {
		resEarned = 0.4
		resWhy = fmt.Sprintf("%d-second datapoints hide shorter peaks", obs.PeriodSeconds)
	}
	c.add("metric-resolution", weightResolution, resEarned, resWhy)

	burstEarned, burstWhy := 1.0, "not a credit-based instance type"
	switch obs.Burst.Class {
	case BurstUnknown:
		burstEarned, burstWhy = 0, "burstable with no usable credit evidence"
	case BurstThrottled:
		burstEarned, burstWhy = 0, "credit-depleted: observed CPU is a throttling ceiling"
	case BurstHealthy, BurstSurplus:
		burstWhy = "credit metrics present and classified"
	}
	c.add("burst-evidence", weightBurst, burstEarned, burstWhy)
	return c
}

// legacyWeakestFactor is pkg/ec2's pre-lift weakestFactor, verbatim.
func legacyWeakestFactor(c legacyConfidence) string {
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

// win builds a window of exactly d from a fixed instant. No clock is read:
// confidence is pure and this test must be reproducible forever.
func win(d time.Duration) Window {
	start := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	return Window{Start: start, End: start.Add(d)}
}

// TestConfidenceEquivalenceAfterLift is the acceptance criterion for moving
// this model into pkg/confidence: every input produces the identical score,
// the identical factor list and the identical weakest-factor prose.
func TestConfidenceEquivalenceAfterLift(t *testing.T) {
	windows := []time.Duration{
		0, time.Minute, 90 * time.Minute, 24 * time.Hour,
		7 * 24 * time.Hour, 14 * 24 * time.Hour, 400 * 24 * time.Hour,
		// A span whose Seconds() is not exactly representable, so a lift
		// that swapped Seconds()/Seconds() for float64/float64 would show up.
		3*time.Hour + 7*time.Minute + 13*time.Second + 456789123*time.Nanosecond,
	}
	minWindows := []time.Duration{
		0, -time.Hour, time.Nanosecond, 45 * time.Minute,
		7 * 24 * time.Hour, 3*time.Hour + 20*time.Minute,
	}
	coverages := []float64{
		0, 0.5, 1, 0.9999999999999999, 1e-300,
		// Out of band and non-finite: the clamp is the one place ec2 and
		// lambda genuinely disagree, so it is pinned hardest.
		-1, -0.0, 2, math.MaxFloat64,
		math.NaN(), math.Inf(1), math.Inf(-1),
	}
	periods := []int32{0, -60, 60, PeriodDetailedSeconds, PeriodDetailedSeconds + 1, 300, 3600}
	classes := []BurstClass{
		BurstNotApplicable, BurstUnknown, BurstHealthy, BurstThrottled, BurstSurplus,
		"", "some-future-class",
	}

	cases := 0
	for _, mw := range minWindows {
		for _, w := range windows {
			for _, cov := range coverages {
				for _, p := range periods {
					for _, cl := range classes {
						for _, blind := range []bool{false, true} {
							cfg := Config{MinWindow: mw}
							obs := Observation{
								Window:          win(w),
								PeriodSeconds:   p,
								Samples:         int(cov * 1000),
								ExpectedSamples: 1000,
								Coverage:        cov,
								MemoryBlind:     blind,
								Burst:           BurstState{Class: cl},
							}
							s := &Sizer{cfg: cfg}
							cases++
							assertSameConfidence(t, legacyConfidenceOf(cfg, obs), s.confidence(obs))
							if t.Failed() {
								t.Fatalf("first divergence at MinWindow=%v Window=%v Coverage=%v Period=%d Burst=%q Blind=%v",
									mw, w, cov, p, cl, blind)
							}
						}
					}
				}
			}
		}
	}
	if cases < 10000 {
		t.Fatalf("only %d cases exercised; the proof is meant to be dense", cases)
	}
	t.Logf("%d input combinations produce bit-identical confidence", cases)
}

// assertSameConfidence compares by raw bits, not by tolerance. A confidence
// that moves in the last bit is still a confidence that moved.
func assertSameConfidence(t *testing.T, want legacyConfidence, got Confidence) {
	t.Helper()
	if math.Float64bits(want.Score) != math.Float64bits(got.Score) {
		t.Errorf("score: legacy %v (%#x) != lifted %v (%#x)",
			want.Score, math.Float64bits(want.Score), got.Score, math.Float64bits(got.Score))
	}
	if len(want.Factors) != len(got.Factors) {
		t.Fatalf("factor count: legacy %d != lifted %d", len(want.Factors), len(got.Factors))
	}
	for i, wf := range want.Factors {
		gf := got.Factors[i]
		if wf.Name != gf.Name {
			t.Errorf("factor %d name: %q != %q", i, wf.Name, gf.Name)
		}
		if math.Float64bits(wf.Weight) != math.Float64bits(gf.Weight) {
			t.Errorf("factor %d (%s) weight: %v != %v", i, wf.Name, wf.Weight, gf.Weight)
		}
		if math.Float64bits(wf.Earned) != math.Float64bits(gf.Earned) {
			t.Errorf("factor %d (%s) earned: %v (%#x) != %v (%#x)", i, wf.Name,
				wf.Earned, math.Float64bits(wf.Earned), gf.Earned, math.Float64bits(gf.Earned))
		}
		if wf.Why != gf.Why {
			t.Errorf("factor %d (%s) why:\n legacy %q\n lifted %q", i, wf.Name, wf.Why, gf.Why)
		}
	}
	// weakestFactor is a reporting surface: an operator reads it to know what
	// to go measure, so a change in WHICH factor is named is a behaviour
	// change even when the number is unchanged.
	if w, g := legacyWeakestFactor(want), weakestFactor(got); w != g {
		t.Errorf("weakestFactor:\n legacy %q\n lifted %q", w, g)
	}
}

// TestConfidenceKeepsItsNaNPropagation pins the divergence between this
// domain's clamp and pkg/lambda's, so that closing it can only ever be a
// deliberate, visible change rather than a side effect of a refactor.
func TestConfidenceKeepsItsNaNPropagation(t *testing.T) {
	s := &Sizer{cfg: Config{MinWindow: 7 * 24 * time.Hour}}
	c := s.confidence(Observation{Window: win(7 * 24 * time.Hour), Coverage: math.NaN()})
	if !math.IsNaN(c.Factors[0].Earned) {
		t.Errorf("sample-coverage earned = %v, want NaN: pkg/ec2 clamps by comparison alone", c.Factors[0].Earned)
	}
	if !math.IsNaN(c.Score) {
		t.Errorf("score = %v, want NaN", c.Score)
	}
	inf := s.confidence(Observation{Window: win(7 * 24 * time.Hour), Coverage: math.Inf(1)})
	if inf.Factors[0].Earned != 1 {
		t.Errorf("+Inf coverage earned = %v, want 1 (clamped up, not zeroed)", inf.Factors[0].Earned)
	}
}
