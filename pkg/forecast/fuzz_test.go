package forecast

import (
	"encoding/binary"
	"math"
	"testing"
)

// floatsFrom decodes the fuzz payload into a float64 stream so the fuzzer can
// synthesize arbitrary bit patterns: NaN, ±Inf, subnormals, huge magnitudes.
func floatsFrom(data []byte) []float64 {
	vs := make([]float64, 0, len(data)/8)
	for len(data) >= 8 {
		vs = append(vs, math.Float64frombits(binary.LittleEndian.Uint64(data)))
		data = data[8:]
	}
	return vs
}

func adversarialSeeds(f *testing.F) {
	f.Helper()
	seed := func(vals ...float64) {
		b := make([]byte, 0, 8*len(vals))
		for _, v := range vals {
			b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
		}
		f.Add(b)
	}
	seed(1e308, -1e308, 1e308, -1e308)
	seed(math.NaN(), math.Inf(1), math.Inf(-1), 0)
	seed(1e150, -1e150, 1e150)
	seed(0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	seed(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)
	seed(math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64)
}

func FuzzEWMA(f *testing.F) {
	adversarialSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, alpha := range []float64{0.01, 0.5, 1} {
			e, err := NewEWMA(alpha)
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range floatsFrom(data) {
				e.Add(v)
				if m := e.Mean(); math.IsNaN(m) || math.IsInf(m, 0) {
					t.Fatalf("alpha=%v: mean %v not finite after sample %d (%v)", alpha, m, i, v)
				}
				if s := e.StdDev(); math.IsNaN(s) || math.IsInf(s, 0) || s < 0 {
					t.Fatalf("alpha=%v: stddev %v invalid after sample %d (%v)", alpha, s, i, v)
				}
				if b := e.UpperBound(3); b < e.Mean() {
					t.Fatalf("alpha=%v: UpperBound(3)=%v below mean %v", alpha, b, e.Mean())
				}
				if e.N() > i+1 {
					t.Fatalf("alpha=%v: N=%d exceeds samples fed %d", alpha, e.N(), i+1)
				}
			}
		}
	})
}

func FuzzHoltWinters(f *testing.F) {
	adversarialSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		trend, _ := NewHoltWinters(0.5, 0.2, 0, 0)
		seasonal, _ := NewHoltWinters(0.5, 0.1, 0.3, 3)
		for _, hw := range []*HoltWinters{trend, seasonal} {
			for i, v := range floatsFrom(data) {
				hw.Add(v)
				for _, h := range []int{1, 7, math.MaxInt} {
					got := hw.Forecast(h)
					if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
						t.Fatalf("Forecast(%d) = %v after sample %d (%v), want finite >= 0", h, got, i, v)
					}
				}
			}
		}
	})
}

func FuzzSpikeDetector(f *testing.F) {
	adversarialSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := NewSpikeDetector(0.2, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range floatsFrom(data) {
			s.Observe(v)
			if r := s.SpikeRate(); math.IsNaN(r) || r < 0 || r > 1 {
				t.Fatalf("SpikeRate %v out of [0,1]", r)
			}
		}
	})
}
