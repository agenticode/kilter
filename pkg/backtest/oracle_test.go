package backtest

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func TestPercentileInt64(t *testing.T) {
	tests := []struct {
		name   string
		sorted []int64
		p      float64
		want   int64
	}{
		{"empty", nil, 0.95, 0},
		{"single", []int64{7}, 0.95, 7},
		{"p95 of 288 with top 144 high", append(rep(144, 200), rep(144, 600)...), 0.95, 600},
		{"p95 of 288 with top 12 high", append(rep(276, 200), rep(12, 600)...), 0.95, 200},
		{"p95 of 288 with top 15 high", append(rep(273, 200), rep(15, 600)...), 0.95, 600},
		{"p100 is the max", []int64{1, 2, 3}, 1, 3},
		{"p0 clamps to the minimum", []int64{1, 2, 3}, 0, 1},
		{"negative p clamps to the minimum", []int64{1, 2, 3}, -1, 1},
		{"p above 1 clamps to the max", []int64{1, 2, 3}, 2, 3},
		{"exact rank boundary", []int64{10, 20, 30, 40}, 0.5, 20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentileInt64(tc.sorted, tc.p); got != tc.want {
				t.Fatalf("percentileInt64(p=%v) = %d, want %d", tc.p, got, tc.want)
			}
		})
	}
}

// TestPercentileInt64NaN pins the NaN branch separately: a NaN percentile
// must not slip through a threshold into an out-of-range index.
func TestPercentileInt64NaN(t *testing.T) {
	nan := zeroDiv()
	if got := percentileInt64([]int64{5, 9}, nan); got != 5 {
		t.Fatalf("NaN percentile = %d, want the conservative low end 5", got)
	}
}

func TestOracleRequestInvertsThePredicates(t *testing.T) {
	ws := windowStats{Samples: 10, CPUP95: 350, MemMax: 1 << 30}
	tests := []struct {
		name       string
		starvation float64
		wantCPU    int64
	}{
		{"factor 1 needs the whole p95", 1, 350},
		{"factor 2 tolerates burst", 2, 175},
		{"factor 0.5 demands headroom", 0.5, 700},
		{"non-integer factor rounds up", 3, 117}, // ceil(350/3) = 117
		{"zero factor falls back to 1", 0, 350},
		{"negative factor falls back to 1", -4, 350},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := oracleRequest(ws, tc.starvation)
			if o.MilliCPU != tc.wantCPU {
				t.Fatalf("oracle CPU = %dm, want %dm", o.MilliCPU, tc.wantCPU)
			}
			if o.MemoryBytes != ws.MemMax {
				t.Fatalf("oracle memory = %d, want the observed peak %d", o.MemoryBytes, ws.MemMax)
			}
			// The defining property: the oracle is exactly on the boundary of
			// the safe set — safe itself, and one milliCPU cheaper is not.
			if mem, cpu := violates(ws, o, tc.starvation); mem || cpu {
				t.Fatalf("the oracle itself violates (mem=%v cpu=%v)", mem, cpu)
			}
			cheaper := model.Resources{MilliCPU: o.MilliCPU - 1, MemoryBytes: o.MemoryBytes - 1}
			mem, cpu := violates(ws, cheaper, tc.starvation)
			if !mem {
				t.Fatalf("one byte under the oracle should violate memory")
			}
			if !cpu {
				t.Fatalf("one milliCPU under the oracle should starve CPU")
			}
		})
	}
}

func TestOracleOfAnEmptyWindowIsZero(t *testing.T) {
	o := oracleRequest(windowStats{}, 1)
	if !o.IsZero() {
		t.Fatalf("empty window oracle = %v, want zero", o)
	}
	if mem, cpu := violates(windowStats{}, model.Resources{}, 1); mem || cpu {
		t.Fatalf("an empty window cannot prove a violation (mem=%v cpu=%v)", mem, cpu)
	}
}

// TestAnalyticOracles is the acceptance evidence for the harness itself: on
// each synthetic archetype the oracle is known in closed form from the
// generator's parameters, and the harness must recover exactly that number.
//
// The closed forms (5-minute cadence, 24h horizon → 288 samples per window):
//
//	steady        every sample Base          → p95 Base,  peak BaseMem
//	diurnal       144 Peak / 144 Base        → p95 Peak,  peak PeakMem
//	bursty        12 Peak / 276 Base (4.2%)  → p95 Base,  peak PeakMem
//	regime-change per window, one level each — except the window straddling
//	              the shift, which holds exactly one Peak sample: too few to
//	              move p95, enough to set the memory peak.
func TestAnalyticOracles(t *testing.T) {
	const (
		baseCPU  = int64(200)
		peakCPU  = int64(600)
		baseMem  = int64(512) << 20
		peakMem  = int64(768) << 20
		days     = 7
		shiftDay = days / 2 // TraceSpec puts the regime shift here
	)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// want[i] is the expected oracle at decision instant start+i*24h.
	tests := []struct {
		kind TraceKind
		want []model.Resources
	}{
		{TraceSteady, repeatRes(days, model.Resources{MilliCPU: baseCPU, MemoryBytes: baseMem})},
		{TraceDiurnal, repeatRes(days, model.Resources{MilliCPU: peakCPU, MemoryBytes: peakMem})},
		{TraceBursty, repeatRes(days, model.Resources{MilliCPU: baseCPU, MemoryBytes: peakMem})},
		{TraceRegimeChange, func() []model.Resources {
			out := make([]model.Resources, days)
			for i := range out {
				switch {
				case i < shiftDay-1:
					// Window entirely before the shift.
					out[i] = model.Resources{MilliCPU: baseCPU, MemoryBytes: baseMem}
				case i == shiftDay-1:
					// Window ends exactly on the shift: one Peak sample.
					// 1 of 288 cannot reach the 95th percentile, but it is
					// the peak, so memory must already cover it.
					out[i] = model.Resources{MilliCPU: baseCPU, MemoryBytes: peakMem}
				default:
					out[i] = model.Resources{MilliCPU: peakCPU, MemoryBytes: peakMem}
				}
			}
			return out
		}()},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{
				Kind: tc.kind, Start: start, Days: days, Workloads: 1,
				BaseMilliCPU: baseCPU, PeakMilliCPU: peakCPU,
				BaseMemBytes: baseMem, PeakMemBytes: peakMem,
			})
			h := &Harness{History: tr.Source()}
			recs, err := h.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != days {
				t.Fatalf("got %d scored decisions, want one per day (%d)", len(recs), days)
			}
			for i, r := range recs {
				wantAt := start.Add(time.Duration(i) * 24 * time.Hour)
				if !r.At.Equal(wantAt) {
					t.Fatalf("decision %d at %s, want %s", i, r.At, wantAt)
				}
				if r.Oracle != tc.want[i] {
					t.Errorf("day %d oracle = %s, want the analytic %s", i, r.Oracle, tc.want[i])
				}
				// A day holds 288 five-minute samples, except the last:
				// the trace is generated over the half-open [Start, End),
				// so no sample sits exactly on the closing edge.
				wantSamples := 288
				if i == days-1 {
					wantSamples = 287
				}
				if r.Samples != wantSamples {
					t.Errorf("day %d scored on %d samples, want %d", i, r.Samples, wantSamples)
				}
			}
		})
	}
}

// TestOracleIsALowerBound states the property the whole scorecard rests on:
// nothing that avoids a violation can be cheaper than the oracle. If this
// ever fails, every regret number in the package is meaningless.
func TestOracleIsALowerBound(t *testing.T) {
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	cost := DefaultCostModel()
	for _, kind := range allKinds {
		for _, noise := range []float64{0, 0.15} {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: start, Days: 7, Workloads: 3,
				NoisePct: noise, NoiseSeed: 99})
			for _, policy := range namedPolicies() {
				h := policy.harness(tr)
				recs, err := h.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range recs {
					if r.MemViolation || r.CPUStarved {
						continue // an unsafe sizing is allowed to be cheaper
					}
					pc, oc := cost.HourlyUSD(r.Chosen), cost.HourlyUSD(r.Oracle)
					if pc < oc {
						t.Fatalf("%s/%s noise=%v: safe sizing %s costs $%.6f/h, under the oracle %s at $%.6f/h",
							kind, policy.name, noise, r.Chosen, pc, r.Oracle, oc)
					}
				}
			}
		}
	}
}

func rep(n int, v int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func repeatRes(n int, r model.Resources) []model.Resources {
	out := make([]model.Resources, n)
	for i := range out {
		out[i] = r
	}
	return out
}

// zeroDiv produces a NaN without importing math into the test's expectations.
func zeroDiv() float64 {
	z := 0.0
	return z / z
}
