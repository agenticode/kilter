package decision

import (
	"math"
	"strings"
	"testing"
)

// sustainedFacts is an exceedance that has NOT reverted and HAS held long
// enough to qualify as a regime, with neutral season/trend statistics.
// Each test breaks exactly one thing.
func sustainedFacts() ShiftFacts {
	return ShiftFacts{
		CUSUMFired:     true,
		Direction:      +1,
		RevertedWithin: -1,
		SustainedFor:   regimeSustainMinSamples,
		AutoCorr24h:    0,
		TrendPerDay:    0,
	}
}

func TestClassifyShift(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ShiftFacts)
		want   ShiftKind
	}{
		// --- spike: rule 1, highest precedence ---
		{"reverted after one sample", func(f *ShiftFacts) { f.RevertedWithin = 1 }, ShiftSpike},
		{"reverted after two samples", func(f *ShiftFacts) { f.RevertedWithin = 2 }, ShiftSpike},
		// Zero is the struct zero value and must mean "not resolved",
		// never "reverted instantly": spike is the verdict that shrinks
		// memory, so it must never be the default a caller falls into.
		{"zero means not reverted", func(f *ShiftFacts) { f.RevertedWithin = 0 }, ShiftRegime},
		{"reverted within threshold", func(f *ShiftFacts) { f.RevertedWithin = spikeRevertMaxSamples }, ShiftSpike},
		{"reverted one past threshold is not a spike", func(f *ShiftFacts) {
			f.RevertedWithin = spikeRevertMaxSamples + 1
		}, ShiftRegime},
		{"never reverted is not a spike", func(f *ShiftFacts) { f.RevertedWithin = -1 }, ShiftRegime},
		{"any negative means not reverted", func(f *ShiftFacts) { f.RevertedWithin = -99 }, ShiftRegime},
		// A quick revert wins even when CUSUM fired and the season and
		// trend statistics both scream: one balloon is one balloon.
		{"spike beats CUSUM, season and trend", func(f *ShiftFacts) {
			f.RevertedWithin, f.AutoCorr24h, f.TrendPerDay = 1, 0.99, 5
		}, ShiftSpike},
		{"unpopulated facts never yield spike", func(f *ShiftFacts) {
			*f = ShiftFacts{CUSUMFired: true, Direction: 1, SustainedFor: 100}
		}, ShiftRegime},

		// --- seasonal: rule 2 ---
		{"seasonal at threshold", func(f *ShiftFacts) { f.AutoCorr24h = seasonalAutoCorrMin }, ShiftSeasonal},
		{"seasonal well above threshold", func(f *ShiftFacts) { f.AutoCorr24h = 0.95 }, ShiftSeasonal},
		{"just below threshold is not seasonal", func(f *ShiftFacts) {
			f.AutoCorr24h = seasonalAutoCorrMin - 0.01
		}, ShiftRegime},
		// A diurnal workload fires the CUSUM on its own daily ramp every
		// day; the decaying histogram already covers that, so season must
		// outrank regime or the engine re-learns once per day forever.
		{"season beats regime", func(f *ShiftFacts) { f.AutoCorr24h = 0.9 }, ShiftSeasonal},
		{"season beats trend", func(f *ShiftFacts) {
			f.CUSUMFired, f.AutoCorr24h, f.TrendPerDay = false, 0.9, 5
		}, ShiftSeasonal},

		// --- regime: rule 3 ---
		{"cusum fired and held", func(f *ShiftFacts) {}, ShiftRegime},
		{"cusum fired but held one sample short", func(f *ShiftFacts) {
			f.SustainedFor = regimeSustainMinSamples - 1
		}, ShiftIndeterminate},
		{"held long but cusum never fired", func(f *ShiftFacts) {
			f.CUSUMFired, f.SustainedFor = false, 1000
		}, ShiftIndeterminate},
		{"downward regime", func(f *ShiftFacts) { f.Direction = -1 }, ShiftRegime},
		{"regime beats trend", func(f *ShiftFacts) { f.TrendPerDay = 5 }, ShiftRegime},

		// --- trend: rule 4 ---
		{"upward trend at threshold", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, trendPerDayMin
		}, ShiftTrend},
		{"downward trend at threshold", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, -trendPerDayMin
		}, ShiftTrend},
		{"trend just below threshold", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, trendPerDayMin-0.001
		}, ShiftIndeterminate},

		// --- indeterminate: rule 5 ---
		{"nothing resolved", func(f *ShiftFacts) { f.CUSUMFired = false }, ShiftIndeterminate},
		{"zero value facts", func(f *ShiftFacts) { *f = ShiftFacts{} }, ShiftIndeterminate},

		// --- garbage statistics may never manufacture a verdict ---
		{"NaN autocorr is not a season", func(f *ShiftFacts) {
			f.CUSUMFired, f.AutoCorr24h = false, math.NaN()
		}, ShiftIndeterminate},
		{"NaN trend is not a trend", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, math.NaN()
		}, ShiftIndeterminate},
		{"NaN both", func(f *ShiftFacts) {
			f.CUSUMFired, f.AutoCorr24h, f.TrendPerDay = false, math.NaN(), math.NaN()
		}, ShiftIndeterminate},
		// Autocorrelation is bounded to [-1,1] by definition, so an
		// out-of-band value is a broken producer, not a season. "Seasonal"
		// and "trend" both mean "no regime handling needed", so garbage
		// must not be able to reach either.
		{"+Inf autocorr is not a season", func(f *ShiftFacts) {
			f.CUSUMFired, f.AutoCorr24h = false, math.Inf(1)
		}, ShiftIndeterminate},
		{"autocorr above 1 is not a season", func(f *ShiftFacts) {
			f.CUSUMFired, f.AutoCorr24h = false, 42
		}, ShiftIndeterminate},
		{"-Inf trend is not a trend", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, math.Inf(-1)
		}, ShiftIndeterminate},
		{"+Inf trend is not a trend", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, math.Inf(1)
		}, ShiftIndeterminate},
		{"large but finite trend is still a trend", func(f *ShiftFacts) {
			f.CUSUMFired, f.TrendPerDay = false, 1e6
		}, ShiftTrend},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := sustainedFacts()
			tc.mutate(&f)
			got, reason := ClassifyShift(f)
			if got != tc.want {
				t.Fatalf("ClassifyShift(%+v) = %q (%s), want %q", f, got, reason, tc.want)
			}
			if strings.TrimSpace(reason) == "" {
				t.Fatal("every classification must carry a human reason")
			}
			if strings.Contains(reason, "%!") {
				t.Fatalf("reason has a formatting artifact: %q", reason)
			}
		})
	}
}

// TestClassifyShiftRegimeReasonNamesDirection: the sentence is surfaced to
// operators, so an up-shift and a down-shift must not read identically.
func TestClassifyShiftRegimeReasonNamesDirection(t *testing.T) {
	up := sustainedFacts()
	up.Direction = +1
	kUp, rUp := ClassifyShift(up)

	down := sustainedFacts()
	down.Direction = -1
	kDown, rDown := ClassifyShift(down)

	if kUp != ShiftRegime || kDown != ShiftRegime {
		t.Fatalf("both must be regime: %q / %q", kUp, kDown)
	}
	if !strings.Contains(rUp, "up") {
		t.Fatalf("up reason %q does not say up", rUp)
	}
	if !strings.Contains(rDown, "down") {
		t.Fatalf("down reason %q does not say down", rDown)
	}
	// Direction 0 (unknown) must not silently read as "up".
	zero := sustainedFacts()
	zero.Direction = 0
	if _, r := ClassifyShift(zero); strings.TrimSpace(r) == "" {
		t.Fatal("zero direction produced an empty reason")
	}
}

// TestClassifyShiftIsTotalAndDeterministic: every input maps to one of the
// five documented kinds, with a reason, and repeated calls agree.
func TestClassifyShiftIsTotalAndDeterministic(t *testing.T) {
	known := map[ShiftKind]bool{
		ShiftSpike: true, ShiftSeasonal: true, ShiftTrend: true,
		ShiftRegime: true, ShiftIndeterminate: true,
	}
	nums := []float64{math.NaN(), math.Inf(-1), -1e9, -1, -0.5, -0.1, 0, 0.1, 0.5, 0.99, 1, 1e9, math.Inf(1)}
	ints := []int{-1000, -2, -1, 0, 1, 3, 4, 5, 6, 7, 1000}

	for _, fired := range []bool{false, true} {
		for _, dir := range []int{-1, 0, 1, 7} {
			for _, rev := range ints {
				for _, sus := range ints {
					for _, ac := range nums {
						for _, tp := range nums {
							f := ShiftFacts{
								CUSUMFired: fired, Direction: dir,
								RevertedWithin: rev, SustainedFor: sus,
								AutoCorr24h: ac, TrendPerDay: tp,
							}
							k, r := ClassifyShift(f)
							if !known[k] {
								t.Fatalf("unknown kind %q for %+v", k, f)
							}
							if strings.TrimSpace(r) == "" {
								t.Fatalf("empty reason for %+v", f)
							}
							if k2, r2 := ClassifyShift(f); k2 != k || r2 != r {
								t.Fatalf("non-deterministic for %+v", f)
							}
						}
					}
				}
			}
		}
	}
}

// TestShiftThresholdsMatchPatterns documents the coupling: these constants
// exist to agree with pkg/patterns' diurnal and growing classification
// thresholds. If patterns changes and this package does not, an exceedance
// can be called seasonal here while the classifier calls the workload
// steady — a contradiction the operator would see.
func TestShiftThresholdsMatchPatterns(t *testing.T) {
	if seasonalAutoCorrMin != 0.5 {
		t.Fatalf("seasonalAutoCorrMin = %v; patterns.classify uses AutoCorr24h > 0.5 for diurnal", seasonalAutoCorrMin)
	}
	if trendPerDayMin != 0.10 {
		t.Fatalf("trendPerDayMin = %v; patterns.classify uses TrendPerDay > 0.10 for growing", trendPerDayMin)
	}
	// A spike must be shorter than a confirmed regime, or the two rules
	// overlap and precedence decides everything.
	if !(spikeRevertMaxSamples < regimeSustainMinSamples) {
		t.Fatalf("spike window %d must be shorter than the regime hold %d",
			spikeRevertMaxSamples, regimeSustainMinSamples)
	}
}
