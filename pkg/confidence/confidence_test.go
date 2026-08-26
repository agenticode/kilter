package confidence

import (
	"math"
	"testing"
	"time"
)

// The domain-level proofs live in pkg/ec2 and pkg/lambda, because that is
// where the shipped numbers are. These tests pin this package's own contract:
// the two clamps, the ordering WeakestFactor depends on, and the one theorem
// that lets a domain choose either clamp for the window factor.

func TestAddClampsIntoTheUnitInterval(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0, 0}, {0.5, 0.5}, {1, 1},
		{-0.0, 0}, {-1, 0}, {-math.MaxFloat64, 0},
		{2, 1}, {math.MaxFloat64, 1},
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
	} {
		var c Confidence
		c.Add("f", 1, tc.in, "why")
		if c.Factors[0].Earned != tc.want || c.Score != tc.want {
			t.Errorf("Add(%v): earned %v score %v, want %v", tc.in, c.Factors[0].Earned, c.Score, tc.want)
		}
	}
}

// AddBounded differs from Add on exactly two inputs. Anything else diverging
// means the two spellings have drifted apart for a reason nobody recorded.
func TestAddBoundedDiffersFromAddOnlyOnNonFiniteInput(t *testing.T) {
	inputs := []float64{
		0, 0.25, 0.5, 1, -0.0, -1, -1e300, 2, 1e300, math.MaxFloat64,
		math.SmallestNonzeroFloat64, 0.9999999999999999,
		math.NaN(), math.Inf(1), math.Inf(-1),
	}
	for _, in := range inputs {
		var a, b Confidence
		a.Add("f", 1, in, "why")
		b.AddBounded("f", 1, in, "why")
		same := math.Float64bits(a.Factors[0].Earned) == math.Float64bits(b.Factors[0].Earned)
		wantDiffer := math.IsNaN(in) || math.IsInf(in, 1)
		if same == wantDiffer {
			t.Errorf("Add(%v)=%v AddBounded(%v)=%v: differ=%v, want differ=%v",
				in, a.Factors[0].Earned, in, b.Factors[0].Earned, !same, wantDiffer)
		}
	}
	// -Inf agrees because both clamps floor it: only NaN and +Inf diverge.
	var a, b Confidence
	a.Add("f", 1, math.Inf(-1), "why")
	b.AddBounded("f", 1, math.Inf(-1), "why")
	if a.Factors[0].Earned != 0 || b.Factors[0].Earned != 0 {
		t.Errorf("-Inf: Add %v, AddBounded %v, want both 0", a.Factors[0].Earned, b.Factors[0].Earned)
	}
}

func TestWeakestFactorNamesTheLargestLossAndBreaksTiesByOrder(t *testing.T) {
	var c Confidence
	c.Add("first", 0.3, 0.5, "a")  // loses 0.15
	c.Add("second", 0.3, 0.5, "b") // loses 0.15 — a tie
	c.Add("third", 0.4, 1, "c")    // loses 0
	if got, want := WeakestFactor(c), "first: a"; got != want {
		t.Errorf("WeakestFactor = %q, want %q: ties resolve to the earliest factor", got, want)
	}

	var big Confidence
	big.Add("small", 0.9, 0.9, "x") // loses 0.09
	big.Add("large", 0.2, 0, "y")   // loses 0.20
	if got, want := WeakestFactor(big), "large: y"; got != want {
		t.Errorf("WeakestFactor = %q, want %q", got, want)
	}
}

// A perfect score still has to answer, because callers print the result
// unconditionally into a refusal.
func TestWeakestFactorOnPerfectAndEmptyScores(t *testing.T) {
	if got := WeakestFactor(Confidence{}); got != "no single dominant factor" {
		t.Errorf("empty: %q", got)
	}
	var c Confidence
	c.Add("all", 1, 1, "everything measured")
	if got, want := WeakestFactor(c), "all: everything measured"; got != want {
		t.Errorf("perfect: %q, want %q — zero loss is still the weakest of one", got, want)
	}
}

// Factors are never sorted, so the slice, WeakestFactor and any serialized
// report are functions of call order alone.
func TestFactorsKeepCallOrder(t *testing.T) {
	var c Confidence
	names := []string{"z", "a", "m", "b"}
	for _, n := range names {
		c.Add(n, 0.25, 1, "why")
	}
	for i, n := range names {
		if c.Factors[i].Name != n {
			t.Fatalf("factor %d = %q, want %q", i, c.Factors[i].Name, n)
		}
	}
}

// TestWindowFactorIsAlwaysFiniteAndNonNegative is the theorem that makes the
// clamp choice irrelevant for THIS factor: a domain may add it with Add or
// with AddBounded and get the same number either way.
//
// It holds because a time.Duration is an int64 of nanoseconds, so a positive
// minimum is at least 1ns (1e-9 s) and any observed span is at most ~292
// years (~9.2e9 s). The largest possible quotient is ~9.2e18 — large, but
// finite, and clamped to 1 by both spellings. Observed spans are non-negative
// because a domain's Window.Duration() floors at zero.
//
// This is why the pkg/lambda equivalence proof does not — and cannot —
// distinguish Add from AddBounded on the window factor. Recorded so that
// absence of a failing case reads as a proof rather than as a gap.
func TestWindowFactorIsAlwaysFiniteAndNonNegative(t *testing.T) {
	spans := []time.Duration{
		0, 1, time.Second, time.Hour, 365 * 24 * time.Hour, math.MaxInt64,
	}
	minimums := []time.Duration{
		1, 2, time.Nanosecond, time.Second, 7 * 24 * time.Hour, math.MaxInt64,
	}
	for _, s := range spans {
		for _, m := range minimums {
			earned, _ := WindowFactor(s, m, "text", time.Hour)
			if math.IsNaN(earned) || math.IsInf(earned, 0) || earned < 0 {
				t.Fatalf("WindowFactor(%v, %v) = %v, want finite and non-negative", s, m, earned)
			}
			var a, b Confidence
			a.Add(FactorWindow, 0.2, earned, "w")
			b.AddBounded(FactorWindow, 0.2, earned, "w")
			if math.Float64bits(a.Score) != math.Float64bits(b.Score) {
				t.Fatalf("WindowFactor(%v, %v): Add %v != AddBounded %v", s, m, a.Score, b.Score)
			}
		}
	}
}

// A non-positive minimum earns nothing but still prints, because the prose is
// what tells an operator the config is the problem.
func TestWindowFactorWithNoStatedMinimumEarnsNothing(t *testing.T) {
	for _, m := range []time.Duration{0, -time.Hour} {
		earned, why := WindowFactor(24*time.Hour, m, "24h0m0s", time.Hour)
		if earned != 0 {
			t.Errorf("minimum %v earned %v, want 0", m, earned)
		}
		if why == "" {
			t.Errorf("minimum %v produced no prose", m)
		}
	}
}

// The rounding argument is a domain fact, not a formatting preference: the
// same span reads differently for a domain whose minimum is days and one
// whose minimum is hours.
func TestWindowFactorRoundingIsCallerChosen(t *testing.T) {
	hourly, _ := WindowFactor(time.Hour, 6*time.Hour+20*time.Minute, "1h0m0s", time.Hour)
	minutely, _ := WindowFactor(time.Hour, 6*time.Hour+20*time.Minute, "1h0m0s", time.Minute)
	if math.Float64bits(hourly) != math.Float64bits(minutely) {
		t.Fatal("rounding must not touch the earned value")
	}
	_, hourWhy := WindowFactor(time.Hour, 6*time.Hour+20*time.Minute, "1h0m0s", time.Hour)
	_, minWhy := WindowFactor(time.Hour, 6*time.Hour+20*time.Minute, "1h0m0s", time.Minute)
	if hourWhy == minWhy {
		t.Fatalf("rounding did not change the prose: both %q", hourWhy)
	}
	if want := "observed 1h0m0s against a 6h20m0s minimum"; minWhy != want {
		t.Errorf("minute rounding: %q, want %q", minWhy, want)
	}
	if want := "observed 1h0m0s against a 6h0m0s minimum"; hourWhy != want {
		t.Errorf("hour rounding: %q, want %q", hourWhy, want)
	}
}

// Score is a plain sum in call order, so it is reproducible; nothing here
// iterates a map or reads a clock.
func TestScoreIsTheWeightedSumInCallOrder(t *testing.T) {
	var c Confidence
	c.Add("a", 0.3, 1, "")
	c.Add("b", 0.2, 0.5, "")
	c.Add("c", 0.5, 0, "")
	want := 0.3*1 + 0.2*0.5 + 0.5*0
	if math.Float64bits(c.Score) != math.Float64bits(want) {
		t.Errorf("score %v, want %v", c.Score, want)
	}
}
