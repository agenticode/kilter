package recommend

import (
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

func TestInsightOOMImminent(t *testing.T) {
	r := newRec(t)
	ref := deployRef("hot")
	lim := model.Resources{MemoryBytes: 512 << 20}
	// Peak memory at 500Mi against a 512Mi limit → within 5% → critical.
	snap := mkSnap(ref, model.Resources{MilliCPU: 100, MemoryBytes: 256 << 20}, lim, 12,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 500 << 20 },
	)
	r.ObserveSnapshot(snap)
	ins := r.Insights(snap)
	if len(ins) == 0 || ins[0].Kind != "oom-risk" || ins[0].Severity != "critical" {
		t.Fatalf("expected critical oom-risk first, got %+v", ins)
	}
}

func TestInsightOOMPredicted(t *testing.T) {
	r := newRec(t)
	ref := deployRef("leaker")
	lim := model.Resources{MemoryBytes: 1 << 30}
	// Memory grows from 600Mi toward the 1Gi limit: +25%/day trend.
	snap := mkSnap(ref, model.Resources{MilliCPU: 100, MemoryBytes: 512 << 20}, lim, 48,
		func(i int) int64 { return 50 },
		func(i int) int64 { return 600<<20 + int64(i)*(300<<20)/(48*12) },
	)
	r.ObserveSnapshot(snap)
	var found *model.Insight
	for i := range r.Insights(snap) {
		ins := r.Insights(snap)[i]
		if ins.Kind == "oom-risk" {
			found = &ins
			break
		}
	}
	if found == nil {
		t.Fatal("growing memory near limit must produce oom-risk insight")
	}
	if found.Severity == "critical" {
		// fine too if peak already within 5%, but horizon expected otherwise
		return
	}
	if found.HorizonHours <= 0 || found.HorizonHours > 200 {
		t.Fatalf("implausible horizon %v", found.HorizonHours)
	}
}

func TestInsightCPUSaturation(t *testing.T) {
	r := newRec(t)
	ref := deployRef("throttled")
	lim := model.Resources{MilliCPU: 1000}
	snap := mkSnap(ref, model.Resources{MilliCPU: 500, MemoryBytes: 256 << 20}, lim, 12,
		func(i int) int64 { return 950 }, // p95 ≥ 90% of limit
		func(i int) int64 { return 100 << 20 },
	)
	r.ObserveSnapshot(snap)
	found := false
	for _, ins := range r.Insights(snap) {
		if ins.Kind == "cpu-saturation" {
			found = true
		}
	}
	if !found {
		t.Fatal("sustained cpu near limit must produce cpu-saturation insight")
	}
}

func TestNoInsightsForHealthyWorkload(t *testing.T) {
	r := newRec(t)
	ref := deployRef("healthy")
	snap := mkSnap(ref,
		model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30},
		model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30}, 24,
		func(i int) int64 { return 200 },
		func(i int) int64 { return 400 << 20 },
	)
	r.ObserveSnapshot(snap)
	for _, ins := range r.Insights(snap) {
		if ins.Severity != "info" {
			t.Fatalf("healthy workload produced %+v", ins)
		}
	}
}
