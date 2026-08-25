package forecast

import (
	"math"
	"math/rand"
	"testing"
)

// --- constructor validation against non-finite parameters ---

func TestNewHoltWintersRejectsNaNParams(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name    string
		a, b, g float64
		L       int
	}{
		{"nan alpha", nan, 0.1, 0.1, 0},
		{"nan beta", 0.5, nan, 0.1, 0},
		{"nan gamma", 0.5, 0.1, nan, 12},
		{"inf alpha", math.Inf(1), 0.1, 0.1, 0},
		{"neg inf beta", 0.5, math.Inf(-1), 0.1, 0},
	}
	for _, c := range cases {
		if _, err := NewHoltWinters(c.a, c.b, c.g, c.L); err == nil {
			t.Errorf("%s: NewHoltWinters(%v,%v,%v,%d) should be rejected", c.name, c.a, c.b, c.g, c.L)
		}
	}
}

func TestNewSpikeDetectorRejectsBadK(t *testing.T) {
	for _, k := range []float64{0, -3, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := NewSpikeDetector(0.1, k); err == nil {
			t.Errorf("k=%v should be rejected", k)
		}
	}
}

// --- overflow / poisoning resistance ---

func TestEWMAPoisonResistance(t *testing.T) {
	e, _ := NewEWMA(0.5)
	// Finite but absurd magnitudes: delta*delta would overflow float64 and
	// permanently poison mean/variance with ±Inf/NaN.
	e.Add(1e308)
	e.Add(-1e308)
	if math.IsNaN(e.Mean()) || math.IsInf(e.Mean(), 0) {
		t.Fatalf("mean poisoned to %v by huge finite inputs", e.Mean())
	}
	if math.IsNaN(e.StdDev()) || math.IsInf(e.StdDev(), 0) {
		t.Fatalf("stddev poisoned to %v by huge finite inputs", e.StdDev())
	}
	if e.N() != 0 {
		t.Fatalf("garbage-magnitude samples must be ignored, N = %d", e.N())
	}
}

func TestEWMAAcceptsBoundaryMagnitude(t *testing.T) {
	// 1e150 is the largest accepted magnitude; state must stay finite even for
	// the worst-case swing between the two extremes.
	e, _ := NewEWMA(0.3)
	e.Add(1e150)
	e.Add(-1e150)
	if e.N() != 2 {
		t.Fatalf("boundary-magnitude samples must be accepted, N = %d", e.N())
	}
	if math.IsNaN(e.Mean()) || math.IsInf(e.Mean(), 0) || math.IsNaN(e.StdDev()) || math.IsInf(e.StdDev(), 0) {
		t.Fatalf("state not finite: mean=%v stddev=%v", e.Mean(), e.StdDev())
	}
}

func TestHoltWintersPoisonResistance(t *testing.T) {
	hw, _ := NewHoltWinters(0.5, 0.3, 0, 0)
	// Before hardening this sequence drove trend to NaN via Inf-Inf and
	// Forecast returned NaN (the v<0 clamp does not catch NaN).
	hw.Add(1e308)
	hw.Add(-1e308)
	hw.Add(1e308)
	got := hw.Forecast(1)
	if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
		t.Fatalf("Forecast(1) = %v after adversarial inputs, want finite >= 0", got)
	}
	if hw.N() != 0 {
		t.Fatalf("garbage-magnitude samples must be ignored, N = %d", hw.N())
	}
}

func TestHoltWintersGarbageDoesNotShiftState(t *testing.T) {
	// A model fed garbage interleaved with a clean series must end up in
	// exactly the state of a model fed only the clean series: garbage must
	// neither perturb level/trend/seasonal nor shift the seasonal phase.
	const L = 12
	clean, _ := NewHoltWinters(0.3, 0.05, 0.2, L)
	dirty, _ := NewHoltWinters(0.3, 0.05, 0.2, L)
	garbage := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e200, -1e200}
	for i := 0; i < 5*L; i++ {
		v := 500 + 200*math.Sin(2*math.Pi*float64(i%L)/L)
		clean.Add(v)
		dirty.Add(v)
		dirty.Add(garbage[i%len(garbage)])
	}
	if clean.N() != dirty.N() {
		t.Fatalf("sample counts diverged: clean %d dirty %d", clean.N(), dirty.N())
	}
	for h := 1; h <= L; h++ {
		if c, d := clean.Forecast(h), dirty.Forecast(h); c != d {
			t.Fatalf("Forecast(%d): clean %v != dirty %v", h, c, d)
		}
	}
}

// --- Forecast horizon edge cases ---

func TestForecastHugeHorizonNoPanic(t *testing.T) {
	const L = 4
	hw, _ := NewHoltWinters(0.3, 0.1, 0.2, L)
	for i := 0; i < 3*L; i++ {
		hw.Add(100 + 10*float64(i%L))
	}
	if !hw.Ready() {
		t.Fatal("should be ready")
	}
	// (n-1+h) overflows int here; the seasonal index must not go negative.
	got := hw.Forecast(math.MaxInt)
	if math.IsNaN(got) || got < 0 {
		t.Fatalf("Forecast(MaxInt) = %v, want finite >= 0", got)
	}
	// Modular equivalence: MaxInt and MaxInt%L land on the same seasonal
	// phase, so the forecasts may differ only by the trend contribution.
	same := hw.Forecast(math.MaxInt % L)
	wantDelta := (float64(math.MaxInt) - float64(math.MaxInt%L)) * hw.trend
	if math.Abs((got-same)-wantDelta) > math.Abs(wantDelta)*1e-9+1e-9 {
		t.Fatalf("seasonal phase inconsistent: Forecast(MaxInt)=%v Forecast(%d)=%v trendDelta=%v",
			got, math.MaxInt%L, same, wantDelta)
	}
}

func TestForecastInvalidHorizon(t *testing.T) {
	hw, _ := NewHoltWinters(0.5, 0.2, 0, 0)
	hw.Add(10)
	hw.Add(20)
	for _, h := range []int{0, -1, math.MinInt} {
		if got := hw.Forecast(h); got != 0 {
			t.Errorf("Forecast(%d) = %v, want 0", h, got)
		}
	}
}

func TestHoltWintersReadyBoundary(t *testing.T) {
	const L = 6
	hw, _ := NewHoltWinters(0.3, 0.1, 0.2, L)
	for i := 0; i < 2*L-1; i++ {
		hw.Add(100)
	}
	if hw.Ready() {
		t.Fatal("ready one sample early")
	}
	if got := hw.Forecast(1); got != 0 {
		t.Fatalf("not-ready forecast = %v, want 0", got)
	}
	hw.Add(100) // sample 2L triggers initialization
	if !hw.Ready() {
		t.Fatal("should be ready at exactly two full seasons")
	}
	if got := hw.Forecast(1); math.Abs(got-100) > 1e-6 {
		t.Fatalf("Forecast(1) = %v for constant 100 series", got)
	}
}

// --- EWMA basics and properties ---

func TestEWMASingleSample(t *testing.T) {
	e, _ := NewEWMA(0.3)
	e.Add(42)
	if e.Mean() != 42 || e.StdDev() != 0 || e.N() != 1 {
		t.Fatalf("after one sample: mean=%v stddev=%v n=%d", e.Mean(), e.StdDev(), e.N())
	}
}

func TestEWMAAlphaOne(t *testing.T) {
	e, _ := NewEWMA(1)
	for _, v := range []float64{10, 200, 3} {
		e.Add(v)
		if e.Mean() != v {
			t.Fatalf("alpha=1 must track last value exactly: mean=%v want %v", e.Mean(), v)
		}
	}
	if e.StdDev() != 0 {
		t.Fatalf("alpha=1 variance must stay 0, got %v", e.StdDev())
	}
}

func TestEWMAUpperBoundFloor(t *testing.T) {
	e, _ := NewEWMA(0.5)
	for _, v := range []float64{10, 30, 20, 40} {
		e.Add(v)
	}
	if e.StdDev() <= 0 {
		t.Fatal("test needs positive stddev")
	}
	for _, k := range []float64{-1, -100, math.NaN()} {
		if got := e.UpperBound(k); got < e.Mean() || math.IsNaN(got) {
			t.Errorf("UpperBound(%v) = %v, must be floored at mean %v", k, got, e.Mean())
		}
	}
	if got := e.UpperBound(2); got != e.Mean()+2*e.StdDev() {
		t.Errorf("UpperBound(2) = %v, want mean+2*stddev", got)
	}
}

func TestEWMAMeanBounded(t *testing.T) {
	// Property: the smoothed mean is a convex combination of inputs, so it can
	// never leave the [min, max] envelope of the observed samples.
	rng := rand.New(rand.NewSource(42))
	e, _ := NewEWMA(0.25)
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := 0; i < 5000; i++ {
		v := rng.NormFloat64()*1e6 - 500
		e.Add(v)
		lo, hi = math.Min(lo, v), math.Max(hi, v)
		if m := e.Mean(); m < lo || m > hi {
			t.Fatalf("mean %v escaped input envelope [%v, %v] at i=%d", m, lo, hi, i)
		}
		if s := e.StdDev(); math.IsNaN(s) || math.IsInf(s, 0) {
			t.Fatalf("stddev not finite at i=%d: %v", i, s)
		}
	}
}

// --- SpikeDetector ---

func TestSpikeDetectorIgnoresGarbage(t *testing.T) {
	s, _ := NewSpikeDetector(0.1, 3)
	for i := 0; i < 15; i++ {
		s.Observe(100)
	}
	if !s.Observe(1e6) {
		t.Fatal("real spike must be detected")
	}
	rate := s.SpikeRate()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300} {
		if s.Observe(v) {
			t.Errorf("garbage %v must not report a spike", v)
		}
	}
	if got := s.SpikeRate(); got != rate {
		t.Fatalf("garbage diluted SpikeRate: %v -> %v", rate, got)
	}
}

func TestSpikeDetectorWarmupBoundary(t *testing.T) {
	// The first 10 observations never spike; the 11th is the first that can.
	s, _ := NewSpikeDetector(0.5, 3)
	for i := 0; i < 9; i++ {
		s.Observe(100)
	}
	if s.Observe(1e6) {
		t.Fatal("10th observation is still warm-up")
	}
	s2, _ := NewSpikeDetector(0.5, 3)
	for i := 0; i < 10; i++ {
		s2.Observe(100)
	}
	if !s2.Observe(1e6) {
		t.Fatal("11th observation must be spike-checked")
	}
}

func TestSpikeRateEmpty(t *testing.T) {
	s, _ := NewSpikeDetector(0.1, 3)
	if got := s.SpikeRate(); got != 0 {
		t.Fatalf("empty detector SpikeRate = %v, want 0", got)
	}
}

func TestDefaultDemand(t *testing.T) {
	hw := DefaultDemand()
	if hw == nil {
		t.Fatal("nil model")
	}
	for i := 0; i < 2*288; i++ {
		hw.Add(100)
	}
	if !hw.Ready() {
		t.Fatal("should be ready after two days of samples")
	}
	if got := hw.Forecast(12); math.Abs(got-100) > 1e-6 {
		t.Fatalf("Forecast(12) = %v for constant demand", got)
	}
}
