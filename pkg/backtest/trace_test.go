package backtest

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/model"
)

// TestTraceCompositionIsWhatTheOracleClaims verifies the generator's side of
// the analytic contract: the closed-form oracles in TestAnalyticOracles are
// only valid if each 288-sample day really holds the stated mix of levels.
func TestTraceCompositionIsWhatTheOracleClaims(t *testing.T) {
	const perDay = 288
	tests := []struct {
		kind      TraceKind
		wantPeaks int // peak-level samples in the first full day after day 0
	}{
		{TraceSteady, 0},
		{TraceDiurnal, 144},    // 12 of 24 hours
		{TraceBursty, 12},      // one sample every two hours
		{TraceRegimeChange, 0}, // the first day is entirely pre-shift
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{Kind: tc.kind, Start: propStart, Days: 7, Workloads: 1})
			if len(tr.Snapshots) != 7*perDay {
				t.Fatalf("got %d snapshots, want %d", len(tr.Snapshots), 7*perDay)
			}
			peaks := 0
			for i := 1; i <= perDay; i++ { // window (day0, day1]
				if tr.Snapshots[i].Usage[0].MilliCPU == tr.Spec.PeakMilliCPU {
					peaks++
				}
			}
			if peaks != tc.wantPeaks {
				t.Fatalf("day 1 holds %d peak samples, want %d", peaks, tc.wantPeaks)
			}
			// The peak share must sit on the correct side of the 5% tail the
			// CPU oracle ignores — that is the whole point of the archetype.
			share := float64(peaks) / perDay
			if tc.kind == TraceBursty && share >= 0.05 {
				t.Fatalf("bursty spikes are %.2f%% of the window; they must stay under the 5%% tail", share*100)
			}
			if tc.kind == TraceDiurnal && share <= 0.05 {
				t.Fatalf("the diurnal peak is only %.2f%% of the window; p95 would miss it", share*100)
			}
		})
	}
}

func TestTraceRegimeShiftLandsOnADecisionInstant(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceRegimeChange, Start: propStart, Days: 7, Workloads: 1})
	want := propStart.Add(3 * 24 * time.Hour)
	if !tr.ShiftAt.Equal(want) {
		t.Fatalf("shift at %s, want %s", tr.ShiftAt, want)
	}
	before, after := false, false
	for _, s := range tr.Snapshots {
		u := s.Usage[0]
		if u.Timestamp.Before(tr.ShiftAt) && u.MilliCPU == tr.Spec.BaseMilliCPU {
			before = true
		}
		if !u.Timestamp.Before(tr.ShiftAt) && u.MilliCPU == tr.Spec.PeakMilliCPU {
			after = true
		}
	}
	if !before || !after {
		t.Fatalf("the shift is not visible in the samples (before=%v after=%v)", before, after)
	}
	// It also emits the events the decision layer needs to react to.
	kinds := map[string]int{}
	for _, ev := range tr.Events {
		kinds[ev.Kind]++
		if !ev.At.Equal(tr.ShiftAt) {
			t.Fatalf("unexpected event at %s", ev.At)
		}
	}
	if kinds[evidence.EventDeploy] != 1 || kinds[evidence.EventRegimeChange] != 1 {
		t.Fatalf("shift events = %v, want one deploy and one regime-change", kinds)
	}
}

func TestTraceIsAPureFunctionOfItsSpec(t *testing.T) {
	spec := TraceSpec{Kind: TraceBursty, Start: propStart, Days: 4, Workloads: 3,
		NoisePct: 0.2, NoiseSeed: 1234}
	a := mustTrace(t, spec)
	b := mustTrace(t, spec)
	if len(a.Snapshots) != len(b.Snapshots) {
		t.Fatalf("snapshot counts differ")
	}
	for i := range a.Snapshots {
		for j := range a.Snapshots[i].Usage {
			if a.Snapshots[i].Usage[j] != b.Snapshots[i].Usage[j] {
				t.Fatalf("snapshot %d usage %d differs: %+v vs %+v",
					i, j, a.Snapshots[i].Usage[j], b.Snapshots[i].Usage[j])
			}
		}
	}
	// A different seed must actually change the samples, or the noise knob
	// is decorative and the "noisy" fixtures are not noisy.
	spec.NoiseSeed = 4321
	c := mustTrace(t, spec)
	differs := false
	for i := range a.Snapshots {
		if a.Snapshots[i].Usage[0] != c.Snapshots[i].Usage[0] {
			differs = true
			break
		}
	}
	if !differs {
		t.Fatal("changing NoiseSeed changed nothing")
	}
}

func TestTraceNoiseStaysInsideItsBand(t *testing.T) {
	const pct = 0.2
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 2, Workloads: 4,
		NoisePct: pct, NoiseSeed: 5})
	lo := float64(tr.Spec.BaseMilliCPU) * (1 - pct)
	hi := float64(tr.Spec.BaseMilliCPU) * (1 + pct)
	sawLow, sawHigh := false, false
	for _, s := range tr.Snapshots {
		for _, u := range s.Usage {
			v := float64(u.MilliCPU)
			if v < lo-1 || v > hi+1 {
				t.Fatalf("sample %v outside the ±%.0f%% band [%v, %v]", v, pct*100, lo, hi)
			}
			if v < float64(tr.Spec.BaseMilliCPU) {
				sawLow = true
			}
			if v > float64(tr.Spec.BaseMilliCPU) {
				sawHigh = true
			}
		}
	}
	if !sawLow || !sawHigh {
		t.Fatalf("noise is one-sided (low=%v high=%v)", sawLow, sawHigh)
	}
	// Over-provisioned requests must still clear the noisy peak, so the
	// unchanged sizing stays safe by construction.
	req := tr.Snapshots[0].Pods[0].Containers[0].Requests
	if req.MilliCPU <= int64(hi) {
		t.Fatalf("requests %s do not cover the noisy peak %v", req, hi)
	}
}

func TestTraceSpecValidation(t *testing.T) {
	ok := TraceSpec{Start: propStart}
	tests := []struct {
		name string
		mut  func(*TraceSpec)
	}{
		{"no start", func(s *TraceSpec) { s.Start = time.Time{} }},
		{"negative days", func(s *TraceSpec) { s.Days = -1 }},
		{"absurd days", func(s *TraceSpec) { s.Days = 500 }},
		{"negative interval", func(s *TraceSpec) { s.Interval = -time.Minute }},
		{"huge interval", func(s *TraceSpec) { s.Interval = 48 * time.Hour }},
		{"no workloads", func(s *TraceSpec) { s.Workloads = -1 }},
		{"negative base cpu", func(s *TraceSpec) { s.BaseMilliCPU = -1 }},
		{"peak below base", func(s *TraceSpec) { s.BaseMilliCPU = 500; s.PeakMilliCPU = 100 }},
		{"memory peak below base", func(s *TraceSpec) { s.BaseMemBytes = 1 << 30; s.PeakMemBytes = 1 << 20 }},
		{"undersize factor", func(s *TraceSpec) { s.OversizeFactor = 0.5 }},
		{"NaN oversize factor", func(s *TraceSpec) { s.OversizeFactor = zeroDiv() }},
		{"negative noise", func(s *TraceSpec) { s.NoisePct = -0.1 }},
		{"absurd noise", func(s *TraceSpec) { s.NoisePct = 0.9 }},
		{"no nodes", func(s *TraceSpec) { s.Nodes = -1 }},
		{"unknown kind", func(s *TraceSpec) { s.Kind = "chaotic" }},
		{"one-day regime change has no shift", func(s *TraceSpec) { s.Kind = TraceRegimeChange; s.Days = 1 }},
	}
	if _, err := ok.Build(); err != nil {
		t.Fatalf("the baseline spec should build: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := ok
			tc.mut(&spec)
			if _, err := spec.Build(); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestTraceStoreHoldsTheSeededEvents(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 3, Workloads: 2,
		DeployAt: []time.Duration{0, 30 * time.Hour}, OOMAt: []time.Duration{10 * time.Hour}})
	st := mustStore(t, tr)
	for _, key := range tr.Keys {
		evs, err := st.Events(evidence.ContainerSubject(tr.Cluster, key), time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != 3 {
			t.Fatalf("container %s has %d events, want 3", key, len(evs))
		}
		for i := 1; i < len(evs); i++ {
			if evs[i].At.Before(evs[i-1].At) {
				t.Fatalf("events came back out of order: %v", evs)
			}
		}
	}
}

func TestTraceStartIsTruncatedToTheSampleGrid(t *testing.T) {
	odd := propStart.Add(3*time.Minute + 17*time.Second)
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: odd, Days: 1})
	if !tr.Start.Equal(propStart) {
		t.Fatalf("start = %s, want it truncated to the 5-minute grid at %s", tr.Start, propStart)
	}
	if !tr.End.Equal(propStart.Add(24 * time.Hour)) {
		t.Fatalf("end = %s, want start + 1 day", tr.End)
	}
}

func TestTracePodsAreGuaranteedAndOversized(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceDiurnal, Start: propStart, Days: 2, Workloads: 2,
		OversizeFactor: 4})
	c := tr.Snapshots[0].Pods[0].Containers[0]
	if c.Requests != c.Limits {
		t.Fatalf("trace pods should be Guaranteed: requests %s, limits %s", c.Requests, c.Limits)
	}
	want := model.Resources{
		MilliCPU:    4 * tr.Spec.PeakMilliCPU,
		MemoryBytes: 4 * tr.Spec.PeakMemBytes,
	}
	if c.Requests != want {
		t.Fatalf("requests %s, want %s (4× the trace peak)", c.Requests, want)
	}
}
