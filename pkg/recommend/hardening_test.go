package recommend

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/histogram"
	"github.com/agenticode/kilter/pkg/model"
)

func TestConfigValidate(t *testing.T) {
	mut := func(f func(*Config)) Config {
		c := DefaultConfig()
		f(&c)
		return c
	}
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"default", DefaultConfig(), false},
		{"cpu percentile 1.0 ok", mut(func(c *Config) { c.CPUPercentile = 1.0 }), false},
		{"min change ratio 0 ok", mut(func(c *Config) { c.MinChangeRatio = 0 }), false},
		{"zero floors ok", mut(func(c *Config) { c.MinMilliCPU = 0; c.MinMemoryBytes = 0 }), false},
		{"zero value config", Config{}, true},
		{"cpu percentile zero", mut(func(c *Config) { c.CPUPercentile = 0 }), true},
		{"cpu percentile negative", mut(func(c *Config) { c.CPUPercentile = -0.1 }), true},
		{"cpu percentile above one", mut(func(c *Config) { c.CPUPercentile = 1.01 }), true},
		{"mem percentile above one", mut(func(c *Config) { c.MemoryPercentile = 1.5 }), true},
		{"cpu headroom below one", mut(func(c *Config) { c.CPUHeadroom = 0.99 }), true},
		{"mem headroom below one", mut(func(c *Config) { c.MemoryHeadroom = 0.5 }), true},
		{"oom bump below one", mut(func(c *Config) { c.OOMBumpRatio = 0.9 }), true},
		{"change ratio negative", mut(func(c *Config) { c.MinChangeRatio = -0.01 }), true},
		{"change ratio one", mut(func(c *Config) { c.MinChangeRatio = 1.0 }), true},
		{"min samples zero", mut(func(c *Config) { c.MinSamples = 0 }), true},
		{"min samples negative", mut(func(c *Config) { c.MinSamples = -5 }), true},
		{"min window zero", mut(func(c *Config) { c.MinWindow = 0 }), true},
		{"min window negative", mut(func(c *Config) { c.MinWindow = -time.Hour }), true},
		{"negative cpu floor", mut(func(c *Config) { c.MinMilliCPU = -1 }), true},
		{"negative mem floor", mut(func(c *Config) { c.MinMemoryBytes = -1 }), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New(%+v) err = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestCeilInt64(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want int64
	}{
		{"zero", 0, 0},
		{"negative", -5.5, 0},
		{"negative inf", math.Inf(-1), 0},
		{"nan", math.NaN(), 0},
		{"fraction rounds up", 0.1, 1},
		{"exact int", 1.0, 1},
		{"one point five", 1.5, 2},
		{"large exact", 1e18, 1_000_000_000_000_000_000},
		{"max int64 float", float64(math.MaxInt64), math.MaxInt64},
		{"just above max", 9.3e18, math.MaxInt64},
		{"way above max", 1e300, math.MaxInt64},
		{"positive inf", math.Inf(1), math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ceilInt64(tt.in); got != tt.want {
				t.Fatalf("ceilInt64(%v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestSignificantBoundaries(t *testing.T) {
	r := newRec(t) // MinChangeRatio = 0.10
	tests := []struct {
		cur, target int64
		want        bool
	}{
		{0, 0, false},
		{0, 1, true}, // anything appearing from zero is significant
		{100, 100, false},
		{100, 110, true}, // exactly at the 10% boundary counts
		{100, 109, false},
		{100, 90, true},
		{100, 91, false},
		{100, 0, true},
		{1_000_000, 1_099_999, false},
		{1_000_000, 1_100_000, true},
	}
	for _, tt := range tests {
		if got := r.significant(tt.cur, tt.target); got != tt.want {
			t.Errorf("significant(%d, %d) = %v, want %v", tt.cur, tt.target, got, tt.want)
		}
	}
}

func TestNilSnapshotSafe(t *testing.T) {
	r := newRec(t)
	r.ObserveSnapshot(nil) // must not panic
	if recs := r.Recommendations(nil); recs != nil {
		t.Fatalf("Recommendations(nil) = %v, want nil", recs)
	}
	if ins := r.Insights(nil); ins != nil {
		t.Fatalf("Insights(nil) = %v, want nil", ins)
	}
}

func TestGarbageUsageSkipped(t *testing.T) {
	r := newRec(t)
	ref := deployRef("garbage")
	key := model.ContainerKey{Workload: ref, Container: "app"}
	hours := 12
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
		model.Resources{}, hours,
		func(i int) int64 { return 150 },
		func(i int) int64 { return 300 << 20 },
	)
	good := len(snap.Usage)
	// Adversarial garbage: negative usage and zero timestamps must be dropped,
	// not learned. A zero timestamp would otherwise anchor firstSample at
	// year 1 and blow the observation window (and confidence) wide open.
	snap.Usage = append(snap.Usage,
		model.Usage{Key: key, PodUID: "pod-1", Timestamp: t0.Add(time.Hour), MilliCPU: -5, MemoryBytes: 100 << 20},
		model.Usage{Key: key, PodUID: "pod-1", Timestamp: t0.Add(time.Hour), MilliCPU: 100, MemoryBytes: -1},
		model.Usage{Key: key, PodUID: "pod-1", Timestamp: time.Time{}, MilliCPU: 100, MemoryBytes: 100 << 20},
	)
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	if recs[0].Samples != good {
		t.Fatalf("Samples = %d, want %d (garbage must not be counted)", recs[0].Samples, good)
	}
	if recs[0].WindowHours > float64(hours)+1 {
		t.Fatalf("WindowHours = %v, zero-time sample poisoned the window", recs[0].WindowHours)
	}
}

func TestHugeWindowSecondsCapped(t *testing.T) {
	r := newRec(t)
	ref := deployRef("wincap")
	key := model.ContainerKey{Workload: ref, Container: "app"}
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
		model.Resources{}, 24,
		func(i int) int64 { return 400 },
		func(i int) int64 { return 300 << 20 },
	)
	// One mis-reported sample claiming a ~68-year averaging window. Uncapped
	// its weight (~35M) would drown the real distribution and shrink the
	// target toward the garbage value.
	snap.Usage = append(snap.Usage, model.Usage{
		Key: key, PodUID: "pod-1", Timestamp: t0.Add(time.Hour),
		MilliCPU: 1, MemoryBytes: 10 << 20, WindowSeconds: math.MaxInt32,
	})
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	if recs[0].TargetRequest.MemoryBytes < 300<<20 {
		t.Fatalf("mem target %dMi below the real 300Mi peak — huge-window sample dominated",
			recs[0].TargetRequest.MemoryBytes>>20)
	}
	if recs[0].TargetRequest.MilliCPU < 400 {
		t.Fatalf("cpu target %dm below the real 400m usage — huge-window sample dominated",
			recs[0].TargetRequest.MilliCPU)
	}
}

func TestLimitPreservedWhenRequestUnset(t *testing.T) {
	r := newRec(t)
	ref := deployRef("limit-only")
	// Limit set, request unset: the ratio is undefined, but the limit must be
	// carried forward — dropping it would silently change QoS class.
	curLim := model.Resources{MilliCPU: 500, MemoryBytes: 512 << 20}
	snap := mkSnap(ref, model.Resources{}, curLim, 24,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 100 << 20 },
	)
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	rec := recs[0]
	if rec.TargetLimit.MilliCPU == 0 || rec.TargetLimit.MemoryBytes == 0 {
		t.Fatalf("existing limit dropped: targetLim=%+v", rec.TargetLimit)
	}
	if rec.TargetLimit.MilliCPU < rec.TargetRequest.MilliCPU ||
		rec.TargetLimit.MemoryBytes < rec.TargetRequest.MemoryBytes {
		t.Fatalf("limit below request: lim=%+v req=%+v", rec.TargetLimit, rec.TargetRequest)
	}
}

func TestLimitNeverBelowRequest(t *testing.T) {
	r := newRec(t)
	ref := deployRef("inverted")
	// Garbage snapshot: limit below request (ratio < 1). The emitted limit
	// must still be >= the emitted request or the API server rejects it.
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30},
		model.Resources{MilliCPU: 500, MemoryBytes: 512 << 20},
		24,
		func(i int) int64 { return 800 },
		func(i int) int64 { return 700 << 20 },
	)
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	rec := recs[0]
	if rec.TargetLimit.MilliCPU < rec.TargetRequest.MilliCPU {
		t.Fatalf("cpu limit %dm below request %dm", rec.TargetLimit.MilliCPU, rec.TargetRequest.MilliCPU)
	}
	if rec.TargetLimit.MemoryBytes < rec.TargetRequest.MemoryBytes {
		t.Fatalf("mem limit %dMi below request %dMi",
			rec.TargetLimit.MemoryBytes>>20, rec.TargetRequest.MemoryBytes>>20)
	}
}

func TestOOMBumpSaturatesInsteadOfOverflowing(t *testing.T) {
	r := newRec(t)
	ref := deployRef("giant")
	// A limit near MaxInt64: bumping by 1.5× overflows int64. Unclamped, the
	// float→int conversion is platform-defined (negative on amd64), silently
	// discarding the OOM floor. It must saturate instead.
	req := model.Resources{MilliCPU: 100, MemoryBytes: 1 << 40}
	lim := model.Resources{MemoryBytes: math.MaxInt64 - 100}
	snap := mkSnap(ref, req, lim, 12,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 200 << 20 },
	)
	r.ObserveSnapshot(snap)
	snap2 := mkSnap(ref, req, lim, 12,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 200 << 20 },
	)
	snap2.Pods[0].Containers[0].RestartCount = 1
	snap2.Pods[0].Containers[0].LastOOMKilled = true
	r.ObserveSnapshot(snap2)

	recs := r.Recommendations(snap2)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	rec := recs[0]
	if rec.OOMCount != 1 {
		t.Fatalf("OOMCount = %d, want 1", rec.OOMCount)
	}
	if rec.TargetRequest.MemoryBytes != math.MaxInt64 {
		t.Fatalf("mem target = %d, want saturated MaxInt64 (floor lost to overflow?)",
			rec.TargetRequest.MemoryBytes)
	}
	if rec.TargetLimit.MemoryBytes < rec.TargetRequest.MemoryBytes {
		t.Fatalf("mem limit %d below request %d", rec.TargetLimit.MemoryBytes, rec.TargetRequest.MemoryBytes)
	}
}

func TestRestoreRejectsCorruptEntries(t *testing.T) {
	key := model.ContainerKey{Workload: deployRef("ckpt"), Container: "app"}
	valid := func() CheckpointState {
		return CheckpointState{
			Key:         key,
			CPU:         histogram.MustNew(histogram.DefaultCPUOptions()).Checkpoint(),
			Memory:      histogram.MustNew(histogram.DefaultMemoryOptions()).Checkpoint(),
			FirstSample: t0,
			LastSample:  t0.Add(24 * time.Hour),
			Samples:     100,
		}
	}
	tests := []struct {
		name string
		mut  func(*CheckpointState)
		want int
	}{
		{"valid", func(cs *CheckpointState) {}, 1},
		{"negative samples", func(cs *CheckpointState) { cs.Samples = -1 }, 0},
		{"negative oom count", func(cs *CheckpointState) { cs.OOMCount = -1 }, 0},
		{"negative oom floor", func(cs *CheckpointState) { cs.OOMFloor = -1 }, 0},
		{"time range backwards", func(cs *CheckpointState) {
			cs.FirstSample, cs.LastSample = cs.LastSample, cs.FirstSample
		}, 0},
		{"zero first with last set", func(cs *CheckpointState) { cs.FirstSample = time.Time{} }, 0},
		{"zero last with first set", func(cs *CheckpointState) { cs.LastSample = time.Time{} }, 0},
		{"corrupt cpu options", func(cs *CheckpointState) { cs.CPU.Options.Ratio = 0 }, 0},
		{"negative bucket weight", func(cs *CheckpointState) {
			cs.Memory.Buckets = map[int]float64{3: -1}
		}, 0},
		{"nan bucket weight", func(cs *CheckpointState) {
			cs.CPU.Buckets = map[int]float64{0: math.NaN()}
		}, 0},
		{"bucket index out of range", func(cs *CheckpointState) {
			cs.CPU.Buckets = map[int]float64{100000: 1}
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRec(t)
			cs := valid()
			tt.mut(&cs)
			if got := r.Restore([]CheckpointState{cs}); got != tt.want {
				t.Fatalf("Restore = %d, want %d", got, tt.want)
			}
			if r.StateCount() != tt.want {
				t.Fatalf("StateCount = %d, want %d", r.StateCount(), tt.want)
			}
		})
	}

	// A corrupt entry must not block valid siblings.
	t.Run("mixed batch", func(t *testing.T) {
		r := newRec(t)
		bad := valid()
		bad.Samples = -1
		if got := r.Restore([]CheckpointState{bad, valid()}); got != 1 {
			t.Fatalf("Restore = %d, want 1 (valid entry alongside corrupt)", got)
		}
	})
}

func TestGCBoundary(t *testing.T) {
	r := newRec(t)
	ref := deployRef("edge")
	snap := mkSnap(ref, model.Resources{MilliCPU: 100, MemoryBytes: 1 << 30},
		model.Resources{}, 8,
		func(i int) int64 { return 10 },
		func(i int) int64 { return 10 << 20 },
	)
	r.ObserveSnapshot(snap)
	last := snap.Usage[len(snap.Usage)-1].Timestamp
	// Cutoff exactly at the last sample keeps the state (Before is strict).
	if n := r.GC(last); n != 0 {
		t.Fatalf("GC at exact lastSample removed %d, want 0", n)
	}
	if n := r.GC(last.Add(time.Nanosecond)); n != 1 {
		t.Fatalf("GC just past lastSample removed %d, want 1", n)
	}
}

func TestHPAVariants(t *testing.T) {
	req := model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30}
	build := func(t *testing.T, cfg Config, info model.WorkloadInfo) []Recommendation {
		t.Helper()
		r, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ref := deployRef("hpa-x")
		info.Ref = ref
		snap := mkSnap(ref, req, model.Resources{}, 24,
			func(i int) int64 { return 100 },
			func(i int) int64 { return 256 << 20 },
		)
		snap.Workloads = []model.WorkloadInfo{info}
		r.ObserveSnapshot(snap)
		return r.Recommendations(snap)
	}

	t.Run("keda-owned HPA names its owner", func(t *testing.T) {
		recs := build(t, DefaultConfig(), model.WorkloadInfo{HasHPA: true, HPATargetsCPU: true, HPAOwner: "keda"})
		if len(recs) != 1 || !recs[0].CPUSkipped {
			t.Fatalf("want 1 cpu-skipped rec, got %+v", recs)
		}
		if !strings.Contains(recs[0].Reason, "keda") {
			t.Fatalf("reason should name the HPA owner, got %q", recs[0].Reason)
		}
	})

	t.Run("HPA not on CPU does not skip", func(t *testing.T) {
		recs := build(t, DefaultConfig(), model.WorkloadInfo{HasHPA: true, HPATargetsCPU: false})
		if len(recs) != 1 || recs[0].CPUSkipped {
			t.Fatalf("memory-based HPA must not skip cpu, got %+v", recs)
		}
		if recs[0].TargetRequest.MilliCPU >= 1000 {
			t.Fatalf("cpu should shrink, got %dm", recs[0].TargetRequest.MilliCPU)
		}
	})

	t.Run("SkipCPUForHPA disabled resizes CPU", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.SkipCPUForHPA = false
		recs := build(t, cfg, model.WorkloadInfo{HasHPA: true, HPATargetsCPU: true})
		if len(recs) != 1 || recs[0].CPUSkipped {
			t.Fatalf("skip disabled: CPUSkipped must be false, got %+v", recs)
		}
		if recs[0].TargetRequest.MilliCPU >= 1000 {
			t.Fatalf("cpu should shrink when skip is disabled, got %dm", recs[0].TargetRequest.MilliCPU)
		}
	})
}

func TestUsageWithoutPodTracksButNeverRecommends(t *testing.T) {
	r := newRec(t)
	key := model.ContainerKey{Workload: deployRef("ghost"), Container: "app"}
	snap := &model.ClusterSnapshot{ClusterID: "test", Timestamp: t0.Add(24 * time.Hour)}
	for i := 0; i < 24*12; i++ {
		snap.Usage = append(snap.Usage, model.Usage{
			Key: key, PodUID: "gone",
			Timestamp: t0.Add(time.Duration(i*5) * time.Minute),
			MilliCPU:  100, MemoryBytes: 100 << 20,
		})
	}
	r.ObserveSnapshot(snap)
	if r.StateCount() != 1 {
		t.Fatalf("usage-only key should be tracked, StateCount = %d", r.StateCount())
	}
	// No pod in the snapshot → no current sizing → no recommendation.
	if recs := r.Recommendations(snap); len(recs) != 0 {
		t.Fatalf("no rec expected without a live pod, got %d", len(recs))
	}
}

func TestZeroRequestGetsFloors(t *testing.T) {
	r := newRec(t)
	ref := deployRef("unset")
	// No requests, no limits, tiny usage: the recommendation must land on the
	// configured floors, never zero (a zero target would erase the request).
	snap := mkSnap(ref, model.Resources{}, model.Resources{}, 24,
		func(i int) int64 { return 1 },
		func(i int) int64 { return 1 << 20 },
	)
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	cfg := DefaultConfig()
	if recs[0].TargetRequest.MilliCPU < cfg.MinMilliCPU {
		t.Fatalf("cpu target %dm below floor %dm", recs[0].TargetRequest.MilliCPU, cfg.MinMilliCPU)
	}
	if recs[0].TargetRequest.MemoryBytes < cfg.MinMemoryBytes {
		t.Fatalf("mem target %d below floor %d", recs[0].TargetRequest.MemoryBytes, cfg.MinMemoryBytes)
	}
	if recs[0].TargetLimit != (model.Resources{}) {
		t.Fatalf("no limit must stay no limit, got %+v", recs[0].TargetLimit)
	}
}

func TestInsightSeverityOrdering(t *testing.T) {
	r := newRec(t)
	// Workload A: warning only (cpu p95 near limit).
	snapA := mkSnap(deployRef("a-warn"),
		model.Resources{MilliCPU: 500, MemoryBytes: 256 << 20},
		model.Resources{MilliCPU: 1000}, 12,
		func(i int) int64 { return 950 },
		func(i int) int64 { return 50 << 20 },
	)
	// Workload B: critical (memory peak within 5% of limit).
	snapB := mkSnap(deployRef("b-crit"),
		model.Resources{MilliCPU: 100, MemoryBytes: 256 << 20},
		model.Resources{MemoryBytes: 512 << 20}, 12,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 500 << 20 },
	)
	snapA.Pods = append(snapA.Pods, snapB.Pods...)
	snapA.Usage = append(snapA.Usage, snapB.Usage...)
	r.ObserveSnapshot(snapA)
	ins := r.Insights(snapA)
	if len(ins) < 2 {
		t.Fatalf("want at least critical+warning, got %+v", ins)
	}
	if ins[0].Severity != "critical" {
		t.Fatalf("critical must sort first, got %q", ins[0].Severity)
	}
	for i := 1; i < len(ins); i++ {
		if sevRank(ins[i-1].Severity) < sevRank(ins[i].Severity) {
			t.Fatalf("severity order violated at %d: %q after %q",
				i, ins[i].Severity, ins[i-1].Severity)
		}
	}
}
