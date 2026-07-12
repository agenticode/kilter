// Package scale holds soak-scale tests proving the decision engine's latency
// budget at big-tech cluster sizes. They run in normal CI (no build tags):
// if these get slow, that IS the regression we want to catch.
package scale

import (
	"fmt"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

// bigCluster synthesizes nodes×podsPerNode with a realistic mix: 70% steady
// small pods, 20% medium, 10% large, plus per-node DaemonSet pods and a
// fraction of underutilized nodes for the consolidator to find.
func bigCluster(nodes, podsPerNode int) *model.ClusterSnapshot {
	snap := &model.ClusterSnapshot{ClusterID: "scale", Timestamp: time.Unix(1750000000, 0)}
	for n := 0; n < nodes; n++ {
		name := fmt.Sprintf("node-%04d", n)
		snap.Nodes = append(snap.Nodes, model.NodeSpec{
			Name:         name,
			Labels:       map[string]string{"kubernetes.io/hostname": name, "kubernetes.io/arch": "amd64"},
			Ready:        true,
			Capacity:     model.Resources{MilliCPU: 16000, MemoryBytes: 64 << 30},
			Allocatable:  model.Resources{MilliCPU: 15200, MemoryBytes: 60 << 30},
			InstanceType: "m5.4xlarge", Provider: "aws",
		})
		// Every 10th node is nearly empty → consolidation candidates.
		limit := podsPerNode
		if n%10 == 0 {
			limit = 1
		}
		for p := 0; p < limit; p++ {
			var cpu, mem int64
			switch p % 10 {
			case 0:
				cpu, mem = 1000, 4<<30 // large
			case 1, 2:
				cpu, mem = 500, 2<<30 // medium
			default:
				cpu, mem = 100, 256<<20 // small
			}
			uid := fmt.Sprintf("p-%04d-%03d", n, p)
			snap.Pods = append(snap.Pods, model.PodSpec{
				UID: uid, Name: uid, Namespace: fmt.Sprintf("ns-%d", p%20),
				NodeName: name, Phase: "Running",
				Workload: model.WorkloadRef{Kind: model.KindDeployment,
					Namespace: fmt.Sprintf("ns-%d", p%20), Name: fmt.Sprintf("app-%d", p%50)},
				Containers: []model.ContainerSpec{{Name: "app",
					Requests: model.Resources{MilliCPU: cpu, MemoryBytes: mem}}},
			})
		}
	}
	return snap
}

func TestPlanBuild5kNodes50kPods(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short")
	}
	snap := bigCluster(5000, 10) // ≈45.5k pods (every 10th node near-empty)
	start := time.Now()
	p, err := plan.Build(snap, nil, pricing.Embedded(), plan.DefaultConfig())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("5k nodes / %dk pods: plan in %s — %d removals, $%.0f/月 savings",
		len(snap.Pods)/1000, elapsed, len(p.Removals), p.SavingsMonthlyUSD)
	if len(p.Removals) == 0 {
		t.Fatal("near-empty nodes must be found at scale")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("plan took %s — over the 30s scale budget", elapsed)
	}
}

func TestRecommenderIngest50kContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short")
	}
	snap := bigCluster(5000, 10)
	for i := range snap.Pods {
		p := &snap.Pods[i]
		snap.Usage = append(snap.Usage, model.Usage{
			Key:       model.ContainerKey{Workload: p.Workload, Container: "app"},
			PodUID:    p.UID,
			Timestamp: snap.Timestamp,
			MilliCPU:  50, MemoryBytes: 128 << 20,
		})
	}
	r, err := recommend.New(recommend.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r.ObserveSnapshot(snap)
	elapsed := time.Since(start)
	t.Logf("ingest %dk usage samples: %s", len(snap.Usage)/1000, elapsed)
	if elapsed > 10*time.Second {
		t.Fatalf("ingest took %s — over the 10s scale budget", elapsed)
	}
}
