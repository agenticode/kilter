package patterns

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

// feed generates a series at 5-minute cadence for the given hours.
func feed(d *Detector, hours int, fn func(i int, t time.Time) float64) {
	for i := 0; i < hours*12; i++ {
		ts := t0.Add(time.Duration(i*5) * time.Minute)
		d.Add(ts, fn(i, ts))
	}
}

func TestUnknownUntilEnoughSamples(t *testing.T) {
	d := &Detector{}
	feed(d, 3, func(i int, _ time.Time) float64 { return 100 }) // 36 < 48 samples
	c, f := d.Analyze()
	if c != ClassUnknown {
		t.Fatalf("class = %s with %d samples", c, f.Samples)
	}
}

func TestSteady(t *testing.T) {
	d := &Detector{}
	rng := rand.New(rand.NewSource(1))
	feed(d, 24, func(i int, _ time.Time) float64 { return 200 + rng.NormFloat64()*6 })
	c, f := d.Analyze()
	if c != ClassSteady {
		t.Fatalf("class = %s (%s)", c, f)
	}
	if f.CV > 0.1 {
		t.Fatalf("cv = %v for near-constant series", f.CV)
	}
}

func TestDiurnal(t *testing.T) {
	d := &Detector{}
	rng := rand.New(rand.NewSource(2))
	feed(d, 48, func(i int, _ time.Time) float64 {
		phase := 2 * math.Pi * float64(i%288) / 288
		return 500 + 300*math.Sin(phase) + rng.NormFloat64()*20
	})
	c, f := d.Analyze()
	if c != ClassDiurnal {
		t.Fatalf("class = %s (%s)", c, f)
	}
	if f.AutoCorr24h < 0.5 {
		t.Fatalf("ac24 = %v for daily sine", f.AutoCorr24h)
	}
}

func TestBursty(t *testing.T) {
	d := &Detector{}
	rng := rand.New(rand.NewSource(3))
	feed(d, 24, func(i int, _ time.Time) float64 {
		if rng.Float64() < 0.08 {
			return 2000 + rng.Float64()*1000 // spike
		}
		return 100 + rng.NormFloat64()*10
	})
	c, f := d.Analyze()
	if c != ClassBursty {
		t.Fatalf("class = %s (%s)", c, f)
	}
}

func TestBatch(t *testing.T) {
	d := &Detector{}
	feed(d, 24, func(i int, _ time.Time) float64 {
		if i%12 == 0 { // 5 minutes of work per hour
			return 1000
		}
		return 2 // idle
	})
	c, f := d.Analyze()
	if c != ClassBatch {
		t.Fatalf("class = %s (%s)", c, f)
	}
	if f.IdleFrac < 0.6 {
		t.Fatalf("idleFrac = %v", f.IdleFrac)
	}
}

func TestGrowing(t *testing.T) {
	d := &Detector{}
	rng := rand.New(rand.NewSource(4))
	// +30%/day linear growth over 2 days.
	feed(d, 48, func(i int, _ time.Time) float64 {
		day := float64(i) / 288.0
		return 400 * (1 + 0.3*day) * (1 + rng.NormFloat64()*0.02)
	})
	c, f := d.Analyze()
	if c != ClassGrowing {
		t.Fatalf("class = %s (%s)", c, f)
	}
	if f.TrendPerDay < 0.15 {
		t.Fatalf("trend = %v/day, expected ~0.28", f.TrendPerDay)
	}
}

func TestClassMigration(t *testing.T) {
	// A workload that was steady turns bursty; the ring must forget and the
	// class must follow. 48h steady then 48h bursty (ring holds 48h).
	d := &Detector{}
	rng := rand.New(rand.NewSource(5))
	feed(d, 48, func(i int, _ time.Time) float64 { return 300 + rng.NormFloat64()*5 })
	c1, _ := d.Analyze()
	if c1 != ClassSteady {
		t.Fatalf("phase 1 class = %s", c1)
	}
	for i := 0; i < 48*12; i++ {
		ts := t0.Add(48*time.Hour + time.Duration(i*5)*time.Minute)
		v := 100.0
		if rng.Float64() < 0.10 {
			v = 3000
		}
		d.Add(ts, v)
	}
	c2, f := d.Analyze()
	if c2 != ClassBursty {
		t.Fatalf("phase 2 class = %s (%s) — detector did not adapt", c2, f)
	}
}

func TestPolicyDifferentiation(t *testing.T) {
	base := func(c Class) Policy { return PolicyFor(c, 0.95, 1.15, 1.20) }
	steady, bursty, growing := base(ClassSteady), base(ClassBursty), base(ClassGrowing)
	if steady.CPUHeadroom >= bursty.CPUHeadroom {
		t.Fatal("bursty must get more cpu headroom than steady")
	}
	if bursty.CPUPercentile <= 0.95 {
		t.Fatal("bursty must raise the percentile")
	}
	if growing.MemoryHeadroom <= 1.20 {
		t.Fatal("growing must raise memory headroom")
	}
	if base(ClassUnknown).CPUPercentile != 0.95 {
		t.Fatal("unknown must keep operator defaults")
	}
	for _, c := range []Class{ClassSteady, ClassDiurnal, ClassBursty, ClassBatch, ClassGrowing, ClassUnknown} {
		if p := base(c); p.Note == "" || p.CPUPercentile <= 0 || p.CPUPercentile > 1 {
			t.Fatalf("bad policy for %s: %+v", c, p)
		}
	}
}

func TestPriors(t *testing.T) {
	low, high, src := PriorWasteEstimate()
	if !(low > 0 && low < high && high < 1) {
		t.Fatalf("prior range %v..%v", low, high)
	}
	if src == "" {
		t.Fatal("priors must cite their source")
	}
}

func TestGarbageIgnored(t *testing.T) {
	d := &Detector{}
	d.Add(t0, math.NaN())
	d.Add(t0, math.Inf(1))
	d.Add(t0, -5)
	if d.n != 0 {
		t.Fatal("garbage samples must be ignored")
	}
}

func BenchmarkAnalyze(b *testing.B) {
	d := &Detector{}
	rng := rand.New(rand.NewSource(9))
	feed(d, 48, func(i int, _ time.Time) float64 { return 500 + rng.NormFloat64()*100 })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Analyze()
	}
}
