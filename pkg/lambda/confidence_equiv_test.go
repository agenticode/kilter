package lambda

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
// The copies below are frozen; see the same file in pkg/ec2 for why.

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

// legacyAdd is pkg/lambda's pre-lift add, character for character. Unlike
// pkg/ec2's it zeroes a non-finite earned value rather than letting NaN
// through — the one factor-level disagreement between the two domains.
func (c *legacyConfidence) add(name string, weight, earned float64, why string) {
	if !finite(earned) || earned < 0 {
		earned = 0
	}
	if earned > 1 {
		earned = 1
	}
	c.Factors = append(c.Factors, legacyFactor{Name: name, Weight: weight, Earned: earned, Why: why})
	c.Score += weight * earned
}

// legacyConfidenceOf is the pre-lift (*Sizer).confidence, verbatim.
func legacyConfidenceOf(cfg Config, obs Observation, usable []MemoryPoint) legacyConfidence {
	var c legacyConfidence

	pointsEarned, pointsWhy := 0.0, fmt.Sprintf("%d memory setting(s) measured with >= %d warm invocations",
		len(usable), cfg.MinSamplesPerPoint)
	switch {
	case len(usable) >= 3:
		pointsEarned = 1
	case len(usable) == 2:
		pointsEarned = 0.8
	}
	c.add("measured-points", weightMeasuredPoints, pointsEarned, pointsWhy)

	c.add("report-coverage", weightReportCoverage, obs.ReportCoverage,
		fmt.Sprintf("%d REPORT lines parsed for %.0f invocations (source: %s)",
			obs.Records, obs.Invocations, obs.InvocationSource))

	warmEarned, warmWhy := 0.0, "no invocations observed"
	if obs.Warm+obs.Cold > 0 {
		warmEarned = 1 - obs.ColdShare
		warmWhy = fmt.Sprintf("%.1f%% of invocations were cold starts", obs.ColdShare*100)
	}
	c.add("warm-share", weightWarmShare, warmEarned, warmWhy)

	windowEarned := 0.0
	if cfg.MinWindow > 0 {
		windowEarned = obs.Window.Duration().Seconds() / cfg.MinWindow.Seconds()
	}
	c.add("window", weightWindow, windowEarned,
		fmt.Sprintf("observed %s against a %s minimum", obs.Window.String(), cfg.MinWindow.Round(time.Minute)))

	headEarned, headWhy := 0.0, "max memory used is at the configured ceiling: possibly truncated"
	if cur, ok := obs.Current(); ok && cur.MemoryMB > 0 && !cur.AtCeiling {
		margin := 1 - float64(cur.MaxMemoryUsedMB)/float64(cur.MemoryMB)
		headEarned = math.Min(1, margin/0.25)
		headWhy = fmt.Sprintf("%s used of %s configured (%.0f%% margin)",
			fmtMB(cur.MaxMemoryUsedMB), fmtMB(cur.MemoryMB), margin*100)
	}
	c.add("memory-headroom", weightHeadroom, headEarned, headWhy)
	return c
}

// legacyWeakestFactor is pkg/lambda's pre-lift weakestFactor, verbatim.
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

// equivWindow builds a window of exactly d from a fixed instant. No clock is
// read: confidence is pure and this test must be reproducible forever.
func equivWindow(d time.Duration) Window {
	start := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	return Window{Start: start, End: start.Add(d)}
}

// TestConfidenceEquivalenceAfterLift is the acceptance criterion for moving
// this model into pkg/confidence: every input produces the identical score,
// the identical factor list and the identical weakest-factor prose.
func TestConfidenceEquivalenceAfterLift(t *testing.T) {
	windows := []time.Duration{
		0, time.Second, 30 * time.Minute, 6 * time.Hour, 24 * time.Hour, 30 * 24 * time.Hour,
		// A span whose Seconds() is not exactly representable, so a lift that
		// swapped Seconds()/Seconds() for float64/float64 would show up.
		3*time.Hour + 7*time.Minute + 13*time.Second + 456789123*time.Nanosecond,
	}
	minWindows := []time.Duration{0, -time.Hour, time.Nanosecond, 6 * time.Hour, 3*time.Hour + 20*time.Minute}
	coverages := []float64{
		0, 0.25, 1, 0.9999999999999999,
		// Out of band and non-finite: this domain zeroes what pkg/ec2 keeps.
		-1, 2, math.NaN(), math.Inf(1), math.Inf(-1),
	}
	coldShares := []float64{0, 0.5, 1, 1.5, -0.5, math.NaN(), math.Inf(1)}
	// Points exercise every branch of measured-points (0/1/2/3+) and of
	// memory-headroom (no current point, zero memory, at ceiling, margins
	// above and below the 25% saturation, and a negative margin).
	pointSets := [][]MemoryPoint{
		nil,
		{{MemoryMB: 512, MaxMemoryUsedMB: 100, Warm: 10}},
		{{MemoryMB: 512, MaxMemoryUsedMB: 500, Warm: 10}, {MemoryMB: 1024, MaxMemoryUsedMB: 500, Warm: 10}},
		{{MemoryMB: 512}, {MemoryMB: 1024}, {MemoryMB: 2048}},
		{{MemoryMB: 0, MaxMemoryUsedMB: 0}},
		{{MemoryMB: 512, MaxMemoryUsedMB: 512, AtCeiling: true}},
		{{MemoryMB: 512, MaxMemoryUsedMB: 600}},
		{{MemoryMB: 1024, MaxMemoryUsedMB: 1000}},
		{{MemoryMB: 1769, MaxMemoryUsedMB: 3}},
	}

	cases := 0
	for _, mw := range minWindows {
		for _, w := range windows {
			for _, cov := range coverages {
				for _, cs := range coldShares {
					for pi, pts := range pointSets {
						for _, idx := range []int{-1, 0, len(pts) - 1, len(pts)} {
							for _, warm := range []int{0, 700} {
								cfg := Config{MinWindow: mw, MinSamplesPerPoint: 50}
								obs := Observation{
									Window:           equivWindow(w),
									Records:          1400,
									Warm:             warm,
									Cold:             0,
									ColdShare:        cs,
									Invocations:      2_000_000,
									InvocationSource: SourceCloudWatch,
									ReportCoverage:   cov,
									Points:           pts,
									CurrentIndex:     idx,
								}
								// usable is passed in by the caller, so vary it
								// independently of Points to cover every branch.
								usable := pts
								if pi%2 == 1 {
									usable = nil
								}
								s := &Sizer{cfg: cfg}
								cases++
								assertSameConfidence(t,
									legacyConfidenceOf(cfg, obs, usable), s.confidence(obs, usable))
								if t.Failed() {
									t.Fatalf("first divergence at MinWindow=%v Window=%v Coverage=%v ColdShare=%v points=%d idx=%d warm=%d",
										mw, w, cov, cs, pi, idx, warm)
								}
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

// TestConfidenceZeroesNonFiniteEvidence pins the other half of the divergence
// pkg/ec2 preserves: here a NaN or infinite earned value is no evidence.
func TestConfidenceZeroesNonFiniteEvidence(t *testing.T) {
	s := &Sizer{cfg: Config{MinWindow: 6 * time.Hour, MinSamplesPerPoint: 50}}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		c := s.confidence(Observation{Window: equivWindow(6 * time.Hour), ReportCoverage: v, CurrentIndex: -1}, nil)
		if c.Factors[1].Earned != 0 {
			t.Errorf("report-coverage %v earned %v, want 0", v, c.Factors[1].Earned)
		}
		if math.IsNaN(c.Score) {
			t.Errorf("report-coverage %v poisoned the score", v)
		}
	}
}
