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
