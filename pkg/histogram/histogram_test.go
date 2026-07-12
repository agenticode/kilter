package histogram

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newCPU(t *testing.T) *Histogram {
	t.Helper()
	h, err := New(DefaultCPUOptions())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEmpty(t *testing.T) {
	h := newCPU(t)
	if !h.IsEmpty() {
		t.Fatal("new histogram must be empty")
	}
	if got := h.Percentile(0.95); got != 0 {
		t.Fatalf("empty percentile = %v", got)
	}
}

func TestInvalidOptions(t *testing.T) {
	bad := []Options{
		{},
		{FirstBucketSize: 10, Ratio: 1.0, NumBuckets: 10, HalfLife: time.Hour, Epsilon: 1e-4},
		{FirstBucketSize: -1, Ratio: 1.05, NumBuckets: 10, HalfLife: time.Hour, Epsilon: 1e-4},
		{FirstBucketSize: 10, Ratio: 1.05, NumBuckets: 1, HalfLife: time.Hour, Epsilon: 1e-4},
	}
	for i, o := range bad {
		if _, err := New(o); err == nil {
			t.Errorf("case %d: expected error for %+v", i, o)
		}
	}
}

func TestBucketBoundaryConsistency(t *testing.T) {
	o := DefaultCPUOptions()
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 100000; i++ {
		v := math.Exp(rng.Float64()*18 - 2) // ~0.13 .. ~8.8e6
		b := o.findBucket(v)
		if b < 0 || b >= o.NumBuckets {
			t.Fatalf("bucket %d out of range for %v", b, v)
		}
		if o.bucketStart(b) > v {
			t.Fatalf("bucketStart(%d)=%v > value %v", b, o.bucketStart(b), v)
		}
		if b < o.NumBuckets-1 && v >= o.bucketStart(b+1) {
			t.Fatalf("value %v >= next bucket start %v (bucket %d)", v, o.bucketStart(b+1), b)
		}
	}
}

func TestPercentileConservative(t *testing.T) {
	h := newCPU(t)
	h.AddSample(250, 1, t0)
	p := h.Percentile(0.5)
	if p < 250 {
		t.Fatalf("percentile %v must be >= sample 250 (conservative)", p)
	}
	if p > 250*1.10+10 {
		t.Fatalf("percentile %v too far above sample 250", p)
	}
}

func TestPercentileOrdering(t *testing.T) {
	h := newCPU(t)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 10000; i++ {
		h.AddSample(rng.Float64()*2000, 1, t0)
	}
	p50, p90, p99, max := h.Percentile(0.5), h.Percentile(0.9), h.Percentile(0.99), h.Max()
	if !(p50 <= p90 && p90 <= p99 && p99 <= max) {
		t.Fatalf("ordering violated: p50=%v p90=%v p99=%v max=%v", p50, p90, p99, max)
	}
	// Uniform [0,2000): p50 should land near 1000 (within bucket resolution).
	if p50 < 800 || p50 > 1250 {
		t.Fatalf("p50=%v implausible for uniform [0,2000)", p50)
	}
}

func TestDecayShiftsTowardRecent(t *testing.T) {
	h := newCPU(t)
	// Old plateau at ~1000m, then workload shrinks to ~100m.
	for i := 0; i < 1000; i++ {
		h.AddSample(1000, 1, t0.Add(time.Duration(i)*time.Minute))
	}
	late := t0.Add(10 * 24 * time.Hour) // 10 half-lives later: old weight ~1/1000
	for i := 0; i < 1000; i++ {
		h.AddSample(100, 1, late.Add(time.Duration(i)*time.Minute))
	}
	p90 := h.Percentile(0.90)
	if p90 > 200 {
		t.Fatalf("p90=%v — old plateau should have decayed away", p90)
	}
	// But the max should still remember the old peak (>= negligible threshold check).
	if h.Percentile(0.999) > 1200 {
		t.Fatalf("p999=%v unexpectedly high", h.Percentile(0.999))
	}
}

func TestLongHorizonNoOverflow(t *testing.T) {
	h := newCPU(t)
	// Span 300 half-lives (~300 days at 24h): must trigger internal renormalization.
	for d := 0; d < 300; d++ {
		h.AddSample(500, 1, t0.Add(time.Duration(d)*24*time.Hour))
	}
	if h.IsEmpty() {
		t.Fatal("histogram lost data")
	}
	p := h.Percentile(0.9)
	if math.IsInf(p, 0) || math.IsNaN(p) || p < 500 || p > 600 {
		t.Fatalf("p90=%v after long horizon", p)
	}
}

func TestMergeEquivalence(t *testing.T) {
	a, b, all := newCPU(t), newCPU(t), newCPU(t)
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 5000; i++ {
		v := rng.Float64() * 3000
		ts := t0.Add(time.Duration(rng.Intn(72)) * time.Hour)
		all.AddSample(v, 1, ts)
		if i%2 == 0 {
			a.AddSample(v, 1, ts)
		} else {
			b.AddSample(v, 1, ts)
		}
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	for _, p := range []float64{0.5, 0.9, 0.95, 0.99} {
		pa, pall := a.Percentile(p), all.Percentile(p)
		if math.Abs(pa-pall) > pall*0.06+10 {
			t.Fatalf("p%v: merged=%v combined=%v", p, pa, pall)
		}
	}
}

func TestMergeIncompatible(t *testing.T) {
	a := newCPU(t)
	b := MustNew(DefaultMemoryOptions())
	if err := a.Merge(b); err == nil {
		t.Fatal("merging different options must fail")
	}
}

func TestCheckpointRoundtrip(t *testing.T) {
	h := newCPU(t)
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 2000; i++ {
		h.AddSample(rng.Float64()*1500, 1, t0.Add(time.Duration(i)*time.Minute))
	}
	c := h.Checkpoint()
	h2, err := FromCheckpoint(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []float64{0.5, 0.9, 0.99} {
		if h.Percentile(p) != h2.Percentile(p) {
			t.Fatalf("p%v mismatch: %v vs %v", p, h.Percentile(p), h2.Percentile(p))
		}
	}
}

func TestCheckpointCorrupt(t *testing.T) {
	h := newCPU(t)
	h.AddSample(100, 1, t0)
	c := h.Checkpoint()
	c.Buckets[9999] = 1 // out of range
	if _, err := FromCheckpoint(c); err == nil {
		t.Fatal("corrupt checkpoint must be rejected")
	}
	c2 := h.Checkpoint()
	c2.Buckets[3] = math.Inf(1)
	if _, err := FromCheckpoint(c2); err == nil {
		t.Fatal("inf weight must be rejected")
	}
}

func TestGarbageSamplesIgnored(t *testing.T) {
	h := newCPU(t)
	h.AddSample(math.NaN(), 1, t0)
	h.AddSample(math.Inf(1), 1, t0)
	h.AddSample(100, -5, t0)
	h.AddSample(100, 0, t0)
	if !h.IsEmpty() {
		t.Fatal("garbage samples must be ignored")
	}
	h.AddSample(-50, 1, t0) // negative value clamps to 0 but counts
	if h.IsEmpty() {
		t.Fatal("clamped sample should count")
	}
}

func BenchmarkAddSample(b *testing.B) {
	h := MustNew(DefaultCPUOptions())
	ts := t0
	for i := 0; i < b.N; i++ {
		ts = ts.Add(time.Second)
		h.AddSample(float64(i%2000), 1, ts)
	}
}

func BenchmarkPercentile(b *testing.B) {
	h := MustNew(DefaultCPUOptions())
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100000; i++ {
		h.AddSample(rng.Float64()*2000, 1, t0.Add(time.Duration(i)*time.Second))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Percentile(0.95)
	}
}
