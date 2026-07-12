package recommend

import (
	"math/rand"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func deployRef(name string) model.WorkloadRef {
	return model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: name}
}

// mkSnap builds a snapshot with one deployment pod and a usage series.
func mkSnap(ref model.WorkloadRef, req, lim model.Resources, hours int, cpuFn, memFn func(i int) int64) *model.ClusterSnapshot {
	key := model.ContainerKey{Workload: ref, Container: "app"}
	snap := &model.ClusterSnapshot{
		ClusterID: "test",
		Timestamp: t0.Add(time.Duration(hours) * time.Hour),
		Pods: []model.PodSpec{{
			UID: "pod-1", Name: ref.Name + "-abc", Namespace: ref.Namespace,
			Workload: ref, Phase: "Running",
			Containers: []model.ContainerSpec{{Name: "app", Requests: req, Limits: lim}},
		}},
	}
	samplesPerHour := 12 // every 5 min
	for i := 0; i < hours*samplesPerHour; i++ {
		snap.Usage = append(snap.Usage, model.Usage{
			Key: key, PodUID: "pod-1",
			Timestamp:   t0.Add(time.Duration(i*5) * time.Minute),
			MilliCPU:    cpuFn(i),
			MemoryBytes: memFn(i),
		})
	}
	return snap
}

func newRec(t *testing.T) *Recommender {
	t.Helper()
	r, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestShrinkOversizedWorkload(t *testing.T) {
	r := newRec(t)
	ref := deployRef("web")
	// Requested 2000m/4Gi, actually uses ~150m/300Mi.
	rng := rand.New(rand.NewSource(1))
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
		model.Resources{},
		24,
		func(i int) int64 { return 140 + rng.Int63n(20) },
		func(i int) int64 { return 300<<20 + rng.Int63n(10<<20) },
	)
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 recommendation, got %d", len(recs))
	}
	rec := recs[0]
	if rec.TargetRequest.MilliCPU >= 400 || rec.TargetRequest.MilliCPU < 150 {
		t.Fatalf("cpu target %dm implausible for ~150m usage", rec.TargetRequest.MilliCPU)
	}
	memMi := rec.TargetRequest.MemoryBytes >> 20
	if memMi < 300 || memMi > 500 {
		t.Fatalf("mem target %dMi implausible for ~310Mi peak", memMi)
	}
	if rec.Confidence <= 0.5 {
		t.Fatalf("confidence %v too low for 24h steady history", rec.Confidence)
	}
	if d := rec.Delta(); d.MilliCPU <= 0 || d.MemoryBytes <= 0 {
		t.Fatalf("expected positive savings, delta=%+v", d)
	}
}

func TestOOMBumpsMemoryFloor(t *testing.T) {
	r := newRec(t)
	ref := deployRef("leaky")
	req := model.Resources{MilliCPU: 100, MemoryBytes: 512 << 20}
	lim := model.Resources{MilliCPU: 0, MemoryBytes: 512 << 20}
	snap := mkSnap(ref, req, lim, 12,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 200 << 20 },
	)
	r.ObserveSnapshot(snap)

	// Second snapshot: container restarted due to OOMKill.
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
	wantFloor := int64(float64(512<<20) * 1.5)
	if rec.TargetRequest.MemoryBytes < wantFloor {
		t.Fatalf("mem target %dMi below OOM floor %dMi",
			rec.TargetRequest.MemoryBytes>>20, wantFloor>>20)
	}
	if rec.OOMCount != 1 {
		t.Fatalf("OOMCount = %d, want 1", rec.OOMCount)
	}
	if rec.TargetLimit.MemoryBytes < wantFloor {
		t.Fatalf("mem limit %dMi below OOM floor", rec.TargetLimit.MemoryBytes>>20)
	}
}

func TestHPAOnCPUKeepsCPURequest(t *testing.T) {
	r := newRec(t)
	ref := deployRef("hpa-web")
	req := model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30}
	snap := mkSnap(ref, req, model.Resources{}, 24,
		func(i int) int64 { return 100 },
		func(i int) int64 { return 256 << 20 },
	)
	snap.Workloads = []model.WorkloadInfo{{Ref: ref, HasHPA: true, HPATargetsCPU: true}}
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec (memory still shrinks), got %d", len(recs))
	}
	if !recs[0].CPUSkipped {
		t.Fatal("CPUSkipped should be set")
	}
	if recs[0].TargetRequest.MilliCPU != 1000 {
		t.Fatalf("cpu request must stay 1000m, got %dm", recs[0].TargetRequest.MilliCPU)
	}
	if recs[0].TargetRequest.MemoryBytes >= 2<<30 {
		t.Fatal("memory should still shrink")
	}
}

func TestRightSizedWorkloadSuppressed(t *testing.T) {
	r := newRec(t)
	ref := deployRef("tuned")
	// Steady class tightens headroom: usage ~850m → target ≈ 901m/376Mi,
	// within 10% of the current 950m/400Mi → suppressed.
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 950, MemoryBytes: 400 << 20},
		model.Resources{}, 24,
		func(i int) int64 { return 850 },
		func(i int) int64 { return 310 << 20 },
	)
	r.ObserveSnapshot(snap)
	if recs := r.Recommendations(snap); len(recs) != 0 {
		t.Fatalf("well-tuned workload should get no recommendation, got %+v", recs)
	}
}

func TestInsufficientHistory(t *testing.T) {
	r := newRec(t)
	ref := deployRef("young")
	snap := mkSnap(ref, model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30},
		model.Resources{}, 1, // 1 hour < MinWindow 6h
		func(i int) int64 { return 50 },
		func(i int) int64 { return 100 << 20 },
	)
	r.ObserveSnapshot(snap)
	if recs := r.Recommendations(snap); len(recs) != 0 {
		t.Fatalf("insufficient window must yield no recs, got %d", len(recs))
	}
}

func TestJobsAndBarePodsExcluded(t *testing.T) {
	r := newRec(t)
	for _, kind := range []model.WorkloadKind{model.KindJob, model.KindCronJob, model.KindBarePod} {
		ref := model.WorkloadRef{Kind: kind, Namespace: "default", Name: "j"}
		snap := mkSnap(ref, model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30},
			model.Resources{}, 24,
			func(i int) int64 { return 10 },
			func(i int) int64 { return 10 << 20 },
		)
		r.ObserveSnapshot(snap)
		if recs := r.Recommendations(snap); len(recs) != 0 {
			t.Fatalf("%s must be excluded, got %d recs", kind, len(recs))
		}
	}
}

func TestLimitRatioPreserved(t *testing.T) {
	r := newRec(t)
	ref := deployRef("limited")
	// limit = 2x request
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30},
		model.Resources{MilliCPU: 2000, MemoryBytes: 2 << 30},
		24,
		func(i int) int64 { return 100 },
		func(i int) int64 { return 128 << 20 },
	)
	r.ObserveSnapshot(snap)
	recs := r.Recommendations(snap)
	if len(recs) != 1 {
		t.Fatalf("want 1 rec, got %d", len(recs))
	}
	rec := recs[0]
	gotRatio := float64(rec.TargetLimit.MilliCPU) / float64(rec.TargetRequest.MilliCPU)
	if gotRatio < 1.9 || gotRatio > 2.1 {
		t.Fatalf("cpu limit ratio %v, want ~2.0", gotRatio)
	}
}

func TestCheckpointRestoreRoundtrip(t *testing.T) {
	r := newRec(t)
	ref := deployRef("persist")
	snap := mkSnap(ref, model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
		model.Resources{}, 24,
		func(i int) int64 { return 200 },
		func(i int) int64 { return 500 << 20 },
	)
	r.ObserveSnapshot(snap)
	want := r.Recommendations(snap)

	r2 := newRec(t)
	if n := r2.Restore(r.Checkpoint()); n != 1 {
		t.Fatalf("restored %d states, want 1", n)
	}
	got := r2.Recommendations(snap)
	if len(got) != len(want) || len(got) != 1 {
		t.Fatalf("rec count mismatch: %d vs %d", len(got), len(want))
	}
	if got[0].TargetRequest != want[0].TargetRequest {
		t.Fatalf("targets differ after restore: %+v vs %+v", got[0].TargetRequest, want[0].TargetRequest)
	}
}

func TestGC(t *testing.T) {
	r := newRec(t)
	ref := deployRef("old")
	snap := mkSnap(ref, model.Resources{MilliCPU: 100, MemoryBytes: 1 << 30},
		model.Resources{}, 8,
		func(i int) int64 { return 10 },
		func(i int) int64 { return 10 << 20 },
	)
	r.ObserveSnapshot(snap)
	if r.StateCount() != 1 {
		t.Fatal("state should exist")
	}
	if n := r.GC(t0.Add(1000 * time.Hour)); n != 1 {
		t.Fatalf("GC removed %d, want 1", n)
	}
	if r.StateCount() != 0 {
		t.Fatal("state should be gone")
	}
}

func BenchmarkObserveAndRecommend10kContainers(b *testing.B) {
	r, _ := New(DefaultConfig())
	// Build one large snapshot: 10k containers, 1 sample each.
	snap := &model.ClusterSnapshot{ClusterID: "bench", Timestamp: t0.Add(24 * time.Hour)}
	for i := 0; i < 10000; i++ {
		ref := model.WorkloadRef{Kind: model.KindDeployment, Namespace: "ns", Name: "w" + string(rune('a'+i%26)) + itoa(i)}
		key := model.ContainerKey{Workload: ref, Container: "app"}
		snap.Pods = append(snap.Pods, model.PodSpec{
			UID: "u" + itoa(i), Name: "p" + itoa(i), Workload: ref, Phase: "Running",
			Containers: []model.ContainerSpec{{Name: "app",
				Requests: model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30}}},
		})
		for s := 0; s < 40; s++ {
			snap.Usage = append(snap.Usage, model.Usage{
				Key: key, PodUID: "u" + itoa(i),
				Timestamp: t0.Add(time.Duration(s) * 20 * time.Minute),
				MilliCPU:  100, MemoryBytes: 200 << 20,
			})
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ObserveSnapshot(snap)
		r.Recommendations(snap)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
