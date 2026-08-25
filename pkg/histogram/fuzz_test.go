package histogram

import (
	"math"
	"testing"
	"time"
)

// FuzzFindBucket: for any finite value, the bucket must contain it.
func FuzzFindBucket(f *testing.F) {
	f.Add(0.0)
	f.Add(10.0)
	f.Add(999999.9)
	f.Add(0.0001)
	o := DefaultCPUOptions()
	f.Fuzz(func(t *testing.T, v float64) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Skip()
		}
		if v < 0 {
			v = -v
		}
		b := o.findBucket(v)
		if b < 0 || b >= o.NumBuckets {
			t.Fatalf("bucket %d out of range for %v", b, v)
		}
		if o.bucketStart(b) > v {
			t.Fatalf("bucketStart(%d)=%v > %v", b, o.bucketStart(b), v)
		}
		if b < o.NumBuckets-1 && v >= o.bucketStart(b+1) {
			t.Fatalf("%v belongs to a later bucket than %d", v, b)
		}
	})
}

// FuzzAddSample: any pair of samples — garbage or not, in any time order —
// must leave the histogram with a finite non-negative total consistent with
// its buckets, and finite, monotone percentiles.
func FuzzAddSample(f *testing.F) {
	f.Add(100.0, 1.0, 200.0, 2.0, int64(0), int64(3600))
	f.Add(math.NaN(), math.Inf(1), -5.0, 0.0, int64(-1000), int64(1000))
	f.Add(math.MaxFloat64, 1.0, 1e-300, math.MaxFloat64, int64(1e9), int64(-1e9))
	f.Fuzz(func(t *testing.T, v1, w1, v2, w2 float64, dt1, dt2 int64) {
		h := MustNew(DefaultCPUOptions())
		// Bound offsets so time.Duration(dt)*time.Second cannot overflow.
		dt1 %= 3_000_000_000
		dt2 %= 3_000_000_000
		h.AddSample(v1, w1, t0.Add(time.Duration(dt1)*time.Second))
		h.AddSample(v2, w2, t0.Add(time.Duration(dt2)*time.Second))
		if math.IsNaN(h.total) || math.IsInf(h.total, 0) || h.total < 0 {
			t.Fatalf("total corrupted: %v", h.total)
		}
		sum := 0.0
		for _, w := range h.weights {
			sum += w
		}
		if math.Abs(sum-h.total) > 1e-9*math.Max(sum, h.total) {
			t.Fatalf("total %v drifted from bucket sum %v", h.total, sum)
		}
		prev := -1.0
		for _, p := range []float64{0, 0.5, 0.9, 0.99, 1} {
			got := h.Percentile(p)
			if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
				t.Fatalf("Percentile(%v) = %v", p, got)
			}
			if got < prev {
				t.Fatalf("Percentile(%v)=%v below previous %v", p, got, prev)
			}
			prev = got
		}
	})
}

// FuzzCheckpoint: arbitrary checkpoint data must never panic — it either
// restores to a usable histogram or returns an error.
func FuzzCheckpoint(f *testing.F) {
	f.Add(3, 12.5, int64(1000))
	f.Add(-1, math.Inf(1), int64(-5))
	f.Add(9999, math.NaN(), int64(0))
	f.Fuzz(func(t *testing.T, bucket int, weight float64, unixSec int64) {
		c := Checkpoint{
			Options: DefaultCPUOptions(),
			RefTime: time.Unix(unixSec, 0),
			Buckets: map[int]float64{bucket: weight},
		}
		h, err := FromCheckpoint(c)
		if err != nil {
			return // rejected: fine
		}
		// Restored histograms must behave sanely.
		p := h.Percentile(0.95)
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			t.Fatalf("restored histogram produced %v", p)
		}
		h.AddSample(100, 1, time.Unix(unixSec, 0).Add(time.Hour))
		if h.IsEmpty() {
			t.Fatal("histogram lost the added sample")
		}
	})
}
