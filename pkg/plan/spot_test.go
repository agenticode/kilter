package plan

import (
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

func TestSpotReportScoring(t *testing.T) {
	spotNode := m5xlarge("spot-1")
	spotNode.Spot = true
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("od-1"), spotNode},
		[]model.PodSpec{
			// web: 3 replicas, evictable → safe. One already on spot.
			runningPod("w1", "od-1", "web", 500, 512),
			runningPod("w2", "od-1", "web", 500, 512),
			runningPod("w3", "spot-1", "web", 500, 512),
			// single: 1 replica → unsafe.
			runningPod("s1", "od-1", "single", 200, 256),
			// pinned: replicas ok but do-not-evict → unsafe.
			runningPod("p1", "od-1", "pinned", 200, 256),
			runningPod("p2", "od-1", "pinned", 200, 256),
		},
	)
	snap.Pods[4].DoNotEvict = true

	rep := BuildSpotReport(snap, pricing.Embedded(), 2)
	byName := map[string]SpotSafety{}
	for _, w := range rep.Workloads {
		byName[w.Workload.Name] = w
	}
	if !byName["web"].Safe || byName["web"].OnSpot != 1 {
		t.Fatalf("web should be spot-safe with 1 on spot: %+v", byName["web"])
	}
	if byName["single"].Safe || byName["pinned"].Safe {
		t.Fatalf("single/pinned must be unsafe: %+v %+v", byName["single"], byName["pinned"])
	}
	if len(byName["single"].Reasons) == 0 {
		t.Fatal("unsafe workloads must carry reasons")
	}
	// Safe requests = web's two on-demand replicas only (1000m/1Gi).
	if rep.SafeRequests.MilliCPU != 1000 {
		t.Fatalf("safe requests = %+v", rep.SafeRequests)
	}
	if rep.EstMonthlySavingsUSD <= 0 || rep.DiscountApplied <= 0.3 || rep.DiscountApplied >= 0.9 {
		t.Fatalf("estimate implausible: $%v at %.0f%%", rep.EstMonthlySavingsUSD, rep.DiscountApplied*100)
	}
}

func TestSpotReportStatefulSetUnsafe(t *testing.T) {
	pods := []model.PodSpec{runningPod("db1", "node-a", "db", 500, 512), runningPod("db2", "node-a", "db", 500, 512)}
	for i := range pods {
		pods[i].Workload.Kind = model.KindStatefulSet
	}
	snap := snapshot([]model.NodeSpec{m5xlarge("node-a")}, pods)
	rep := BuildSpotReport(snap, pricing.Embedded(), 2)
	if len(rep.Workloads) != 1 || rep.Workloads[0].Safe {
		t.Fatalf("statefulset must be spot-unsafe: %+v", rep.Workloads)
	}
}

func TestSpotReportPDBExhaustedUnsafe(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a")},
		[]model.PodSpec{
			runningPod("w1", "node-a", "web", 500, 512),
			runningPod("w2", "node-a", "web", 500, 512),
		},
	)
	snap.PDBs = []model.PDB{{
		Namespace: "default", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 0,
	}}
	rep := BuildSpotReport(snap, pricing.Embedded(), 2)
	if rep.Workloads[0].Safe {
		t.Fatal("exhausted PDB must make workload spot-unsafe")
	}
}

func TestInterruptedSpotNodes(t *testing.T) {
	n1 := m5xlarge("dying")
	n1.Taints = []model.Taint{{Key: "aws-node-termination-handler/spot-itn", Effect: "NoSchedule"}}
	n2 := m5xlarge("karp")
	n2.Taints = []model.Taint{{Key: "karpenter.sh/disrupted", Effect: "NoSchedule"}}
	snap := snapshot([]model.NodeSpec{m5xlarge("healthy"), n1, n2}, nil)
	got := InterruptedSpotNodes(snap)
	if len(got) != 2 || got[0] != "dying" || got[1] != "karp" {
		t.Fatalf("interrupted = %v", got)
	}
}

func TestEmergencyDrainPlan(t *testing.T) {
	ds := runningPod("ds1", "dying", "logger", 100, 128)
	ds.Workload.Kind = model.KindDaemonSet
	pinned := runningPod("pin1", "dying", "pinned", 100, 128)
	pinned.DoNotEvict = true
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("dying"), m5xlarge("safe")},
		[]model.PodSpec{
			runningPod("w1", "dying", "web", 500, 512),
			ds, pinned,
		},
	)
	p := EmergencyDrainPlan(snap, "dying")
	if p.Steps[0].Type != StepCordonNode {
		t.Fatal("must cordon first")
	}
	evicts := 0
	for _, s := range p.Steps {
		if s.Type == StepEvictPod {
			evicts++
			if s.PodUID == "ds1" || s.PodUID == "pin1" {
				t.Fatalf("must not evict DS or opted-out pods: %+v", s)
			}
		}
		if s.Type == StepDeleteNode {
			t.Fatal("emergency drain must not delete the node (cloud reclaims it)")
		}
	}
	if evicts != 1 {
		t.Fatalf("evictions = %d, want 1", evicts)
	}
}
