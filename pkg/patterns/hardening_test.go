package patterns

import (
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// ---- regression: p95 == 0 idle-fraction collapse ----

// A cron-style workload with exact zeros between rare spikes (>95% zeros)
// drives p95 to 0, which used to collapse the idle threshold to v < 0 and
// misclassify the idlest possible series as diurnal/bursty — the policy then
// added headroom instead of sizing for active windows.
func TestZeroIdleBatchRegression(t *testing.T) {
	d := &Detector{}
	feed(d, 48, func(i int, _ time.Time) float64 {
		if i%24 == 0 { // one 5-minute spike every 2 hours
			return 1000
		}
		return 0
	})
	c, f := d.Analyze()
	if c != ClassBatch {
		t.Fatalf("class = %s (%s), want batch", c, f)
	}
	if f.IdleFrac < 0.9 {
		t.Fatalf("idleFrac = %v for a series that is 95.8%% exact zeros", f.IdleFrac)
	}
}

// ---- Add input validation ----

func TestAddSampleValidation(t *testing.T) {
	cases := []struct {
		name   string
		v      float64
		accept bool
	}{
		{"zero", 0, true},
		{"negative zero", math.Copysign(0, -1), true},
		{"normal", 250, true},
		{"tiny subnormal", 5e-324, true},
		{"large but plausible", 1e17, true},
		{"at cap", maxSample, true},
		{"just above cap", math.Nextafter(maxSample, math.Inf(1)), false},
		{"absurd", 1e19, false},
		{"max float", math.MaxFloat64, false},
		{"NaN", math.NaN(), false},
		{"+Inf", math.Inf(1), false},
		{"-Inf", math.Inf(-1), false},
		{"negative", -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Detector{}
			d.Add(t0, tc.v)
			if got := d.size() == 1; got != tc.accept {
				t.Fatalf("Add(%v): accepted=%v, want %v", tc.v, got, tc.accept)
			}
		})
	}
}

// ---- sample-count boundaries ----

func TestAnalyzeSampleBoundaries(t *testing.T) {
	t.Run("empty detector", func(t *testing.T) {
		d := &Detector{}
		c, f := d.Analyze()
		if c != ClassUnknown || f.Samples != 0 {
			t.Fatalf("empty: class=%s samples=%d", c, f.Samples)
		}
	})
	t.Run("one below threshold", func(t *testing.T) {
		d := &Detector{}
		for i := 0; i < minClassifySamples-1; i++ {
			d.Add(t0.Add(time.Duration(i*5)*time.Minute), 100)
		}
		if c, _ := d.Analyze(); c != ClassUnknown {
			t.Fatalf("class = %s with %d samples", c, minClassifySamples-1)
		}
	})
	t.Run("at threshold", func(t *testing.T) {
		d := &Detector{}
		for i := 0; i < minClassifySamples; i++ {
			d.Add(t0.Add(time.Duration(i*5)*time.Minute), 100)
		}
		c, f := d.Analyze()
		if c != ClassSteady {
			t.Fatalf("constant series at threshold: class=%s (%s)", c, f)
		}
	})
}

// ---- ring buffer wrap ----

func TestRingWrapKeepsNewestInOrder(t *testing.T) {
	d := &Detector{}
	total := 60 * 12 // 60h at 5-min cadence, > ringCap
	feed(d, 60, func(i int, _ time.Time) float64 { return float64(i) })
	if got := d.size(); got != ringCap {
		t.Fatalf("size = %d, want %d", got, ringCap)
	}
	vals, times := d.series()
	// Oldest surviving sample is total-ringCap; newest is total-1.
	if vals[0] != float64(total-ringCap) || vals[len(vals)-1] != float64(total-1) {
		t.Fatalf("ring window = [%v, %v], want [%v, %v]",
			vals[0], vals[len(vals)-1], total-ringCap, total-1)
	}
	for i := 1; i < len(times); i++ {
		if times[i] <= times[i-1] {
			t.Fatalf("times not strictly increasing at %d: %d then %d", i, times[i-1], times[i])
		}
	}
}

// ---- all-zero series ----

func TestAllZeroSeriesIsBatch(t *testing.T) {
	d := &Detector{}
	feed(d, 48, func(int, time.Time) float64 { return 0 })
	c, f := d.Analyze()
	if c != ClassBatch {
		t.Fatalf("all-zero series: class=%s (%s)", c, f)
	}
	assertFeatureInvariants(t, c, f)
}

// ---- percentile ----

func TestPercentile(t *testing.T) {
	asc := make([]float64, 100)
	for i := range asc {
		asc[i] = float64(i + 1) // 1..100
	}
	cases := []struct {
		name string
		vals []float64
		p    float64
		want float64
	}{
		{"empty", nil, 0.95, 0},
		{"single p0", []float64{42}, 0, 42},
		{"single p1", []float64{42}, 1, 42},
		{"two p50 takes lower", []float64{1, 2}, 0.5, 1},
		{"two p1 takes max", []float64{1, 2}, 1, 2},
		{"hundred p95", asc, 0.95, 95}, // ⌊0.95·99⌋ = 94 → value 95
		{"hundred p0", asc, 0, 1},
		{"hundred p1", asc, 1, 100},
		{"unsorted input", []float64{5, 1, 4, 2, 3}, 1, 5},
		{"p above 1 clamps to max", []float64{1, 2, 3}, 1.5, 3},
		{"p below 0 clamps to min", []float64{1, 2, 3}, -0.5, 1},
		{"NaN p clamps to min", []float64{1, 2, 3}, math.NaN(), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.vals, tc.p); got != tc.want {
				t.Fatalf("percentile(%v, %v) = %v, want %v", tc.vals, tc.p, got, tc.want)
			}
		})
	}

	t.Run("does not mutate input", func(t *testing.T) {
		in := []float64{5, 1, 4}
		percentile(in, 0.95)
		if in[0] != 5 || in[1] != 1 || in[2] != 4 {
			t.Fatalf("input mutated: %v", in)
		}
	})
}

func TestSortFloatsMatchesStdlib(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	sizes := []int{0, 1, 2, 3, 17, ringCap}
	for _, n := range sizes {
		a := make([]float64, n)
		for i := range a {
			a[i] = math.Floor(rng.Float64()*10) / 3 // force duplicates
		}
		want := append([]float64(nil), a...)
		sort.Float64s(want)
		sortFloats(a)
		for i := range a {
			if a[i] != want[i] {
				t.Fatalf("n=%d: sortFloats diverges from stdlib at %d: %v vs %v", n, i, a[i], want[i])
			}
		}
	}
	// Reverse-sorted worst case.
	a := make([]float64, 200)
	for i := range a {
		a[i] = float64(len(a) - i)
	}
	sortFloats(a)
	for i := 1; i < len(a); i++ {
		if a[i] < a[i-1] {
			t.Fatalf("reverse input not sorted at %d", i)
		}
	}
}

// ---- classify rule table ----

// classify is a priority-ordered rule chain; this pins the intended
// precedence so a future reorder is a conscious decision, not an accident.
func TestClassifyRuleTable(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Features)
		want Class
	}{
		{"zero mean is batch", func(f *Features) { f.Mean = 0 }, ClassBatch},
		{"growth wins over idle and cycles",
			func(f *Features) { f.TrendPerDay = 0.2; f.IdleFrac = 0.9; f.AutoCorr24h = 0.9; f.CV = 3 }, ClassGrowing},
		{"downtrend is not growing", func(f *Features) { f.TrendPerDay = -0.5 }, ClassSteady},
		{"trend exactly at threshold is not growing", func(f *Features) { f.TrendPerDay = 0.10 }, ClassSteady},
		{"idle with near-zero median is batch even with daily cycle",
			func(f *Features) { f.IdleFrac = 0.7; f.MedianRatio = 0.01; f.AutoCorr24h = 0.9 }, ClassBatch},
		{"idle with a serving baseline is bursty, not batch",
			func(f *Features) { f.IdleFrac = 0.7; f.MedianRatio = 0.05; f.CV = 2 }, ClassBursty},
		{"daily cycle beats burstiness", func(f *Features) { f.AutoCorr24h = 0.6; f.CV = 2 }, ClassDiurnal},
		{"high CV alone is bursty", func(f *Features) { f.CV = 1.5 }, ClassBursty},
		{"spike rate alone is bursty", func(f *Features) { f.SpikeRate = 0.06 }, ClassBursty},
		{"quiet flat series is steady", func(*Features) {}, ClassSteady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Features{Samples: ringCap, Mean: 100}
			tc.mut(&f)
			if got := classify(f); got != tc.want {
				t.Fatalf("classify(%+v) = %s, want %s", f, got, tc.want)
			}
		})
	}
}

// ---- PolicyFor bounds ----

func TestPolicyForBounds(t *testing.T) {
	cases := []struct {
		name  string
		class Class
		check func(t *testing.T, p Policy)
	}{
		{"bursty percentile capped at 0.99", ClassBursty, func(t *testing.T, p Policy) {
			if p.CPUPercentile != 0.99 {
				t.Fatalf("percentile = %v", p.CPUPercentile)
			}
		}},
		{"batch lowers percentile but not below 0.85", ClassBatch, func(t *testing.T, p Policy) {
			if !(p.CPUPercentile >= 0.85 && p.CPUPercentile < 0.98) {
				t.Fatalf("percentile = %v", p.CPUPercentile)
			}
		}},
		{"steady headroom never below floors", ClassSteady, func(t *testing.T, p Policy) {
			if p.CPUHeadroom < 1.05 || p.MemoryHeadroom < 1.10 {
				t.Fatalf("headroom %v/%v below floors", p.CPUHeadroom, p.MemoryHeadroom)
			}
		}},
		{"diurnal keeps operator base", ClassDiurnal, func(t *testing.T, p Policy) {
			if p.CPUPercentile != 0.98 || p.CPUHeadroom != 1.06 || p.MemoryHeadroom != 1.12 {
				t.Fatalf("diurnal changed base: %+v", p)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Base values chosen so every cap/floor in the table is exercised:
			// percentile 0.98 (+0.03 would exceed 0.99; −0.05 would pass 0.85
			// only for floors ≥ 0.90 — covered below), headroom near floors.
			tc.check(t, PolicyFor(tc.class, 0.98, 1.06, 1.12))
		})
	}
	// Batch floor engages when base−0.05 would undershoot 0.85.
	if p := PolicyFor(ClassBatch, 0.87, 1.15, 1.20); p.CPUPercentile != 0.85 {
		t.Fatalf("batch floor: %v", p.CPUPercentile)
	}
	// All classes with sane base yield a usable percentile and headroom.
	for _, c := range []Class{ClassUnknown, ClassSteady, ClassDiurnal, ClassBursty, ClassBatch, ClassGrowing} {
		p := PolicyFor(c, 0.95, 1.15, 1.20)
		if !(p.CPUPercentile > 0 && p.CPUPercentile <= 1) {
			t.Fatalf("%s: percentile %v out of (0,1]", c, p.CPUPercentile)
		}
		if p.CPUHeadroom < 1 || p.MemoryHeadroom < 1 {
			t.Fatalf("%s: headroom %v/%v below 1", c, p.CPUHeadroom, p.MemoryHeadroom)
		}
	}
}

// ---- feature invariants under adversarial input ----

var validClasses = map[Class]bool{
	ClassUnknown: true, ClassSteady: true, ClassDiurnal: true,
	ClassBursty: true, ClassBatch: true, ClassGrowing: true,
}

func assertFeatureInvariants(t *testing.T, c Class, f Features) {
	t.Helper()
	if !validClasses[c] {
		t.Errorf("unknown class %q", c)
	}
	finite := func(name string, v float64) {
		t.Helper()
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("%s = %v, must be finite (%s)", name, v, f)
		}
	}
	finite("Mean", f.Mean)
	finite("CV", f.CV)
	finite("AutoCorr24h", f.AutoCorr24h)
	finite("TrendPerDay", f.TrendPerDay)
	finite("SpikeRate", f.SpikeRate)
	finite("IdleFrac", f.IdleFrac)
	finite("MedianRatio", f.MedianRatio)
	if f.Mean < 0 || f.CV < 0 {
		t.Errorf("negative Mean/CV: %v / %v", f.Mean, f.CV)
	}
	if f.AutoCorr24h < -1 || f.AutoCorr24h > 1 {
		t.Errorf("AutoCorr24h = %v out of [-1,1]", f.AutoCorr24h)
	}
	unit := func(name string, v float64) {
		t.Helper()
		if v < 0 || v > 1 {
			t.Errorf("%s = %v out of [0,1]", name, v)
		}
	}
	unit("SpikeRate", f.SpikeRate)
	unit("IdleFrac", f.IdleFrac)
	unit("MedianRatio", f.MedianRatio)
}

func TestFeatureInvariantsAdversarial(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	cases := []struct {
		name string
		add  func(d *Detector)
	}{
		{"constant at cap", func(d *Detector) {
			feed(d, 48, func(int, time.Time) float64 { return maxSample })
		}},
		{"alternating cap and zero", func(d *Detector) {
			feed(d, 48, func(i int, _ time.Time) float64 {
				if i%2 == 0 {
					return maxSample
				}
				return 0
			})
		}},
		{"one cap spike among subnormals", func(d *Detector) {
			feed(d, 48, func(i int, _ time.Time) float64 {
				if i == 300 {
					return maxSample
				}
				return 5e-324
			})
		}},
		{"all samples share one timestamp", func(d *Detector) {
			for i := 0; i < 576; i++ {
				d.Add(t0, float64(i))
			}
		}},
		{"zero-value timestamps", func(d *Detector) {
			for i := 0; i < 576; i++ {
				d.Add(time.Time{}, rng.Float64()*1000)
			}
		}},
		{"one-second cadence", func(d *Detector) {
			for i := 0; i < 576; i++ {
				d.Add(t0.Add(time.Duration(i)*time.Second), float64(i)*1e15)
			}
		}},
		{"out-of-order timestamps", func(d *Detector) {
			for i := 0; i < 576; i++ {
				ts := t0.Add(time.Duration(rng.Intn(48*3600)) * time.Second)
				d.Add(ts, rng.Float64()*1e6)
			}
		}},
		{"far-future timestamps", func(d *Detector) {
			base := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := 0; i < 576; i++ {
				d.Add(base.Add(time.Duration(i*5)*time.Minute), rng.Float64()*100)
			}
		}},
		{"random walk", func(d *Detector) {
			v := 1000.0
			feed(d, 48, func(int, time.Time) float64 {
				v = math.Max(0, v+rng.NormFloat64()*50)
				return v
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Detector{}
			tc.add(d)
			c, f := d.Analyze()
			assertFeatureInvariants(t, c, f)
		})
	}
}

// ---- autocorrelation sign ----

// A 48h-period signal is anti-phase at 24h lag: AutoCorr24h must come out
// strongly negative (exercising the r < -1 clamp side) and the workload
// must not be called diurnal.
func TestAntiPhaseAutocorrelation(t *testing.T) {
	d := &Detector{}
	rng := rand.New(rand.NewSource(11))
	feed(d, 48, func(i int, _ time.Time) float64 {
		phase := 2 * math.Pi * float64(i) / 576 // one full cycle over 48h
		return 500 + 300*math.Sin(phase) + rng.NormFloat64()*10
	})
	c, f := d.Analyze()
	if f.AutoCorr24h > -0.5 {
		t.Fatalf("ac24 = %v for anti-phase signal, want strongly negative (%s)", f.AutoCorr24h, f)
	}
	if c == ClassDiurnal {
		t.Fatalf("anti-phase signal classified diurnal (%s)", f)
	}
}

// ---- fuzz ----

// FuzzAnalyze decodes the input as (Δminutes, float64-bits) pairs, feeds
// them through Add (which must reject garbage), and asserts the Analyze
// finiteness/range invariants hold for whatever survives.
func FuzzAnalyze(f *testing.F) {
	mk := func(pairs ...[2]uint64) []byte {
		var b []byte
		for _, p := range pairs {
			b = append(b, byte(p[0]))
			b = binary.LittleEndian.AppendUint64(b, p[1])
		}
		return b
	}
	f.Add(mk([2]uint64{5, math.Float64bits(100)}, [2]uint64{5, math.Float64bits(0)}))
	f.Add(mk([2]uint64{0, math.Float64bits(maxSample)}, [2]uint64{255, math.Float64bits(5e-324)}))
	f.Add(mk([2]uint64{1, math.Float64bits(math.NaN())}, [2]uint64{1, math.Float64bits(-1)}))
	f.Fuzz(func(t *testing.T, data []byte) {
		d := &Detector{}
		ts := t0
		for len(data) >= 9 {
			ts = ts.Add(time.Duration(data[0]) * time.Minute)
			d.Add(ts, math.Float64frombits(binary.LittleEndian.Uint64(data[1:9])))
			data = data[9:]
		}
		c, feats := d.Analyze()
		assertFeatureInvariants(t, c, feats)
		if feats.Samples < minClassifySamples && c != ClassUnknown {
			t.Errorf("classified %s with only %d samples", c, feats.Samples)
		}
	})
}
