package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// FuzzScoreInvariants hammers the aggregation with arbitrary decisions. A
// scorecard is an artifact operators and CI compare mechanically, so it must
// never contain a NaN, never lose a decision, and never disagree with its own
// definitions — whatever the replay hands it.
func FuzzScoreInvariants(f *testing.F) {
	f.Add(int64(3), int64(1000), int64(1<<30), int64(800), int64(1<<29), uint8(0), int64(24))
	f.Add(int64(0), int64(0), int64(0), int64(0), int64(0), uint8(255), int64(1))
	f.Add(int64(7), int64(-5), int64(-5), int64(math.MaxInt64), int64(math.MaxInt64), uint8(9), int64(-3))

	f.Fuzz(func(t *testing.T, n, cpu, mem, oracleCPU, oracleMem int64, flags uint8, hours int64) {
		count := int(n % 32)
		if count < 0 {
			count = -count
		}
		horizon := time.Duration(hours%72) * time.Hour
		flipWindow := 7 * 24 * time.Hour

		recs := make([]record, 0, count)
		for i := 0; i < count; i++ {
			bit := func(k uint) bool { return flags&(1<<(k%8)) != 0 }
			r := record{
				Key: model.ContainerKey{
					Workload:  model.WorkloadRef{Kind: model.KindDeployment, Namespace: "ns", Name: "w"},
					Container: "app",
				},
				At:           propStart.Add(time.Duration(i) * time.Hour),
				Applied:      bit(uint(i)),
				Current:      model.Resources{MilliCPU: cpu, MemoryBytes: mem},
				Target:       model.Resources{MilliCPU: cpu / 2, MemoryBytes: mem / 2},
				Chosen:       model.Resources{MilliCPU: cpu, MemoryBytes: mem},
				Oracle:       model.Resources{MilliCPU: oracleCPU, MemoryBytes: oracleMem},
				MemViolation: bit(uint(i + 1)),
				CPUStarved:   bit(uint(i + 2)),
				OOMKills:     i % 3,
				Adverse:      bit(uint(i + 3)),
				Samples:      i,
			}
			if !r.Applied {
				r.Code = CodeBelowChangeThreshold
			}
			recs = append(recs, r)
		}

		sc := score(recs, DefaultCostModel(), horizon, flipWindow)

		if sc.Scored != count {
			t.Fatalf("scored %d of %d records", sc.Scored, count)
		}
		refusals := 0
		for _, v := range sc.Refusals {
			refusals += v
		}
		if sc.Decisions+refusals != sc.Scored {
			t.Fatalf("decisions(%d) + refusals(%d) != scored(%d)", sc.Decisions, refusals, sc.Scored)
		}
		if sc.RefusalsGood+sc.RefusalsIdle != refusals {
			t.Fatalf("refusal quality %d+%d != %d", sc.RefusalsGood, sc.RefusalsIdle, refusals)
		}
		for name, v := range map[string]float64{
			"OracleGapPct": sc.OracleGapPct, "OracleGapPctApplied": sc.OracleGapPctApplied,
			"PolicyCostUSD": sc.PolicyCostUSD, "OracleCostUSD": sc.OracleCostUSD,
			"ClaimedSavingsUSD": sc.ClaimedSavingsUSD, "RealizedSavingsUSD": sc.RealizedSavingsUSD,
			"ClaimedVsRealized": sc.ClaimedVsRealized, "ForgoneSavingsUSD": sc.ForgoneSavingsUSD,
			"FlipRate": sc.FlipRate, "ResourceRegretUSD": sc.ResourceRegretUSD,
			"RiskRegretUSD": sc.RiskRegretUSD, "RegretUSD": sc.RegretUSD,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s is not a number: %v", name, v)
			}
		}
		if sc.RiskRegretUSD < 0 {
			t.Fatalf("risk regret is negative: %v", sc.RiskRegretUSD)
		}
		if sc.ForgoneSavingsUSD < 0 {
			t.Fatalf("forgone savings is negative: %v", sc.ForgoneSavingsUSD)
		}
		if sc.Flips > sc.Decisions {
			t.Fatalf("%d flips across %d decisions", sc.Flips, sc.Decisions)
		}
		if _, err := sc.Encode(); err != nil {
			t.Fatalf("encoding: %v", err)
		}
	})
}

// FuzzTraceSpecBuild asserts the generator is total: every spec either builds
// a usable trace or returns an error. A panic here would be a fixture that
// only works for the parameters someone happened to try.
func FuzzTraceSpecBuild(f *testing.F) {
	f.Add(0, 7, 1, int64(200), int64(600), int64(1<<29), int64(1<<30), 3.0, 0.0, uint64(1), 2)
	f.Add(3, 2, 4, int64(1), int64(1), int64(1), int64(1), 1.0, 0.49, uint64(9), 1)
	f.Add(1, -5, 0, int64(0), int64(-1), int64(0), int64(0), 0.0, -1.0, uint64(0), 0)

	f.Fuzz(func(t *testing.T, kind, days, workloads int, baseCPU, peakCPU, baseMem, peakMem int64,
		oversize, noise float64, seed uint64, nodes int) {

		kinds := []TraceKind{TraceSteady, TraceDiurnal, TraceBursty, TraceRegimeChange, "bogus"}
		if kind < 0 {
			kind = -kind
		}
		spec := TraceSpec{
			Kind: kinds[kind%len(kinds)], Start: propStart, Days: days, Workloads: workloads,
			BaseMilliCPU: baseCPU, PeakMilliCPU: peakCPU, BaseMemBytes: baseMem, PeakMemBytes: peakMem,
			OversizeFactor: oversize, NoisePct: noise, NoiseSeed: seed, Nodes: nodes,
			Interval: time.Hour, // keep fuzz traces cheap
		}
		tr, err := spec.Build()
		if err != nil {
			return
		}
		if tr.Cluster == "" || len(tr.Snapshots) == 0 || len(tr.Keys) == 0 {
			t.Fatalf("a successful build produced an unusable trace: %d snapshots, %d keys",
				len(tr.Snapshots), len(tr.Keys))
		}
		for i := 1; i < len(tr.Snapshots); i++ {
			if !tr.Snapshots[i-1].Timestamp.Before(tr.Snapshots[i].Timestamp) {
				t.Fatalf("snapshots are not strictly increasing at %d", i)
			}
		}
		for _, s := range tr.Snapshots {
			for _, u := range s.Usage {
				if u.MilliCPU < 0 || u.MemoryBytes < 0 || u.Timestamp.IsZero() {
					t.Fatalf("generated an unusable sample: %+v", u)
				}
			}
			req := s.Pods[0].Containers[0].Requests
			if req.MilliCPU <= 0 || req.MemoryBytes <= 0 {
				t.Fatalf("generated pods with no request: %s", req)
			}
		}
		if _, err := tr.Store(); err != nil {
			t.Fatalf("seeding evidence from a valid trace: %v", err)
		}
	})
}
