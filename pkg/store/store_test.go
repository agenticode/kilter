package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/histogram"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

var t0 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kilter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecommenderStateRoundtrip(t *testing.T) {
	s := open(t)
	h := histogram.MustNew(histogram.DefaultCPUOptions())
	h.AddSample(250, 1, t0)
	states := []recommend.CheckpointState{{
		Key: model.ContainerKey{
			Workload:  model.WorkloadRef{Kind: model.KindDeployment, Namespace: "d", Name: "w"},
			Container: "app",
		},
		CPU: h.Checkpoint(), Memory: histogram.MustNew(histogram.DefaultMemoryOptions()).Checkpoint(),
		Samples: 42,
	}}
	if err := s.SaveRecommenderState("c1", states); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRecommenderState("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Samples != 42 || got[0].Key.Container != "app" {
		t.Fatalf("roundtrip lost data: %+v", got)
	}
	// Restoring into a recommender works end to end.
	r, _ := recommend.New(recommend.DefaultConfig())
	if n := r.Restore(got); n != 1 {
		t.Fatalf("restore count %d", n)
	}
	// Unknown cluster: nil, nil.
	if got, err := s.LoadRecommenderState("nope"); err != nil || got != nil {
		t.Fatalf("missing cluster should be nil,nil: %v %v", got, err)
	}
}

func TestSnapshotRoundtrip(t *testing.T) {
	s := open(t)
	snap := &model.ClusterSnapshot{
		ClusterID: "prod-east", Timestamp: t0,
		Nodes: []model.NodeSpec{{Name: "n1", Ready: true}},
		Pods:  []model.PodSpec{{UID: "u1", Name: "p1"}},
	}
	if err := s.SaveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSnapshot("prod-east")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterID != "prod-east" || len(got.Nodes) != 1 || len(got.Pods) != 1 {
		t.Fatalf("snapshot lost: %+v", got)
	}
	clusters, _ := s.Clusters()
	if len(clusters) != 1 || clusters[0] != "prod-east" {
		t.Fatalf("clusters: %v", clusters)
	}
	if err := s.SaveSnapshot(&model.ClusterSnapshot{}); err == nil {
		t.Fatal("empty cluster id must be rejected")
	}
}

func TestPlanHistoryPrunes(t *testing.T) {
	s := open(t)
	for i := 0; i < PlanHistoryLimit+10; i++ {
		p := &plan.Plan{ClusterID: "c1", CreatedAt: t0.Add(time.Duration(i) * time.Minute),
			CurrentHourlyUSD: float64(i)}
		if err := s.SavePlan(p); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := s.PlanCount("c1")
	if n != PlanHistoryLimit {
		t.Fatalf("history not pruned: %d", n)
	}
	latest, err := s.LatestPlan("c1")
	if err != nil {
		t.Fatal(err)
	}
	if latest.CurrentHourlyUSD != float64(PlanHistoryLimit+9) {
		t.Fatalf("latest plan wrong: %v", latest.CurrentHourlyUSD)
	}
	if p, err := s.LatestPlan("unknown"); err != nil || p != nil {
		t.Fatal("unknown cluster should be nil,nil")
	}
}

func TestPlanHistoryClusterIsolation(t *testing.T) {
	s := open(t)
	// A cluster id that is a prefix of another must not leak history.
	s.SavePlan(&plan.Plan{ClusterID: "prod", CreatedAt: t0, CurrentHourlyUSD: 1})
	s.SavePlan(&plan.Plan{ClusterID: "prod-east", CreatedAt: t0, CurrentHourlyUSD: 2})
	p, _ := s.LatestPlan("prod")
	if p.CurrentHourlyUSD != 1 {
		t.Fatalf("prefix isolation broken: %v", p.CurrentHourlyUSD)
	}
	n, _ := s.PlanCount("prod")
	if n != 1 {
		t.Fatalf("prefix count leak: %d", n)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cluster := fmt.Sprintf("c%d", i%2)
			for j := 0; j < 20; j++ {
				s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: cluster, Timestamp: t0})
				s.LoadSnapshot(cluster)
				s.SavePlan(&plan.Plan{ClusterID: cluster, CreatedAt: t0.Add(time.Duration(j) * time.Second)})
				s.LatestPlan(cluster)
			}
		}(i)
	}
	wg.Wait()
}
