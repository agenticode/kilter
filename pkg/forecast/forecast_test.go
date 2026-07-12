package forecast

import (
	"math"
	"math/rand"
	"testing"
)

func TestEWMAConvergence(t *testing.T) {
	e, err := NewEWMA(0.1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		e.Add(100)
	}
	if math.Abs(e.Mean()-100) > 1e-6 {
		t.Fatalf("mean = %v, want 100", e.Mean())
	}
	if e.StdDev() > 1e-3 {
		t.Fatalf("stddev = %v for constant series", e.StdDev())
	}
}

func TestEWMATracksShift(t *testing.T) {
	e, _ := NewEWMA(0.2)
	for i := 0; i < 100; i++ {
		e.Add(50)
	}
	for i := 0; i < 100; i++ {
		e.Add(200)
	}
	if math.Abs(e.Mean()-200) > 1 {
		t.Fatalf("mean = %v after level shift, want ~200", e.Mean())
	}
}

func TestEWMAInvalid(t *testing.T) {
	for _, a := range []float64{0, -0.5, 1.5, math.NaN()} {
		if _, err := NewEWMA(a); err == nil {
			t.Errorf("alpha %v should be rejected", a)
		}
	}
	e, _ := NewEWMA(0.5)
	e.Add(math.NaN())
	e.Add(math.Inf(1))
	if e.N() != 0 {
		t.Fatal("garbage must be ignored")
	}
}

func TestHoltWintersTrendOnly(t *testing.T) {
	hw, err := NewHoltWinters(0.5, 0.3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Linear ramp: y = 10 + 2t
	for i := 0; i < 200; i++ {
		hw.Add(10 + 2*float64(i))
	}
	if !hw.Ready() {
		t.Fatal("should be ready")
	}
	got := hw.Forecast(10)
	want := 10 + 2*float64(199+10)
	if math.Abs(got-want) > want*0.05 {
		t.Fatalf("forecast(10) = %v, want ~%v", got, want)
	}
}

func TestHoltWintersSeasonal(t *testing.T) {
	const L = 24
	hw, err := NewHoltWinters(0.3, 0.02, 0.3, L)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(11))
	series := func(i int) float64 {
		return 500 + 200*math.Sin(2*math.Pi*float64(i%L)/L)
	}
	n := 0
	for ; n < 20*L; n++ {
		hw.Add(series(n) + rng.NormFloat64()*10)
	}
	// Forecast one full season ahead; compare each step to ground truth.
	var maxErr float64
	for h := 1; h <= L; h++ {
		got := hw.Forecast(h)
		want := series(n - 1 + h)
		if e := math.Abs(got - want); e > maxErr {
			maxErr = e
		}
	}
	// Amplitude is 200; allow 15% of peak-to-peak as max pointwise error.
	if maxErr > 60 {
		t.Fatalf("max seasonal forecast error %v too high", maxErr)
	}
}

func TestHoltWintersNonNegative(t *testing.T) {
	hw, _ := NewHoltWinters(0.5, 0.5, 0, 0)
	hw.Add(100)
	hw.Add(50) // steep downtrend
	hw.Add(10)
	if got := hw.Forecast(50); got < 0 {
		t.Fatalf("forecast must clamp at 0, got %v", got)
	}
}

func TestHoltWintersValidation(t *testing.T) {
	cases := []struct {
		a, b, g float64
		L       int
	}{
		{0, 0.1, 0.1, 12},   // alpha 0
		{0.5, -1, 0.1, 12},  // beta negative
		{0.5, 0.1, 0.1, 1},  // season of 1
		{0.5, 0.1, 0.1, -3}, // negative season
		{0.5, 0.1, 0, 12},   // seasonal without gamma
	}
	for i, c := range cases {
		if _, err := NewHoltWinters(c.a, c.b, c.g, c.L); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}

func TestHoltWintersNotReady(t *testing.T) {
	hw, _ := NewHoltWinters(0.3, 0.1, 0.1, 12)
	for i := 0; i < 23; i++ { // needs 24
		hw.Add(float64(i))
	}
	if hw.Ready() {
		t.Fatal("should not be ready before two full seasons")
	}
	if hw.Forecast(1) != 0 {
		t.Fatal("not-ready forecast must be 0")
	}
}

func TestSpikeDetector(t *testing.T) {
	s, err := NewSpikeDetector(0.1, 3)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 300; i++ {
		if s.Observe(100 + rng.NormFloat64()*5) {
			// occasional statistical spikes are fine, but rate is checked below
			continue
		}
	}
	if s.SpikeRate() > 0.02 {
		t.Fatalf("false positive rate %v too high for steady series", s.SpikeRate())
	}
	if !s.Observe(100000) {
		t.Fatal("obvious spike not detected")
	}
}

func TestSpikeDetectorWarmup(t *testing.T) {
	s, _ := NewSpikeDetector(0.2, 3)
	for i := 0; i < 9; i++ {
		if s.Observe(float64(i * 1000)) {
			t.Fatal("warm-up must not report spikes")
		}
	}
}

func BenchmarkHoltWintersAdd(b *testing.B) {
	hw := DefaultDemand()
	for i := 0; i < b.N; i++ {
		hw.Add(float64(i % 1000))
	}
}
