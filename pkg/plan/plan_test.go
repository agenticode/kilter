package plan

import (
	"fmt"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

var t0 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func m5xlarge(name string) model.NodeSpec {
	return model.NodeSpec{
		Name:         name,
		Labels:       map[string]string{"kubernetes.io/hostname": name, "kubernetes.io/arch": "amd64"},
		Ready:        true,
		Capacity:     model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		Allocatable:  model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		InstanceType: "m5.xlarge", Provider: "aws",
	}
}

func runningPod(uid, node, wl string, cpu, memMiB int64) model.PodSpec {
	return model.PodSpec{
		UID: uid, Name: uid, Namespace: "default", NodeName: node, Phase: "Running",
		Labels:   map[string]string{"app": wl},
		Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: wl},
		Containers: []model.ContainerSpec{{Name: "app",
			Requests: model.Resources{MilliCPU: cpu, MemoryBytes: memMiB << 20}}},
	}
}

func snapshot(nodes []model.NodeSpec, pods []model.PodSpec) *model.ClusterSnapshot {
	return &model.ClusterSnapshot{ClusterID: "test", Timestamp: t0, Nodes: nodes, Pods: pods}
}

func TestConsolidatesUnderutilizedNode(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b"), m5xlarge("node-c")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072),
			runningPod("b1", "node-b", "wb", 2000, 3072),
			runningPod("c1", "node-c", "wc", 400, 1024),
			runningPod("c2", "node-c", "wd", 400, 1024),
		},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 1 || p.Removals[0].Node != "node-c" {
		t.Fatalf("expected node-c removal, got %+v", p.Removals)
	}
	if p.Removals[0].EvictedPods != 2 {
		t.Fatalf("evicted pods = %d", p.Removals[0].EvictedPods)
	}
	// Step ordering: cordon → evict×2 → delete.
	var types []StepType
	for _, s := range p.Steps {
		types = append(types, s.Type)
	}
	want := []StepType{StepCordonNode, StepEvictPod, StepEvictPod, StepDeleteNode}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("step order %v, want %v", types, want)
	}
	// Money: 3×0.192 → 2×0.192.
	if p.CurrentHourlyUSD < 0.575 || p.CurrentHourlyUSD > 0.577 {
		t.Fatalf("current cost %v", p.CurrentHourlyUSD)
	}
	wantProj := p.CurrentHourlyUSD - 0.192
	if diff := p.ProjectedHourlyUSD - wantProj; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("projected %v want %v", p.ProjectedHourlyUSD, wantProj)
	}
	if p.SavingsMonthlyUSD < 140 || p.SavingsMonthlyUSD > 141 {
		t.Fatalf("savings %v, want ~140.16", p.SavingsMonthlyUSD)
	}
	// Sim targets recorded for evictions.
	for _, s := range p.Steps {
		if s.Type == StepEvictPod && s.TargetNode == "" {
			t.Fatal("evict step missing sim target")
		}
	}
}

func TestNoRemovalWhenClusterTight(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 3500, 4096),
			runningPod("b1", "node-b", "wb", 3500, 4096),
		},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("tight cluster must yield empty plan, got %d steps", len(p.Steps))
	}
	if len(p.Notes) == 0 {
		t.Fatal("empty plan should carry an explanatory note")
	}
}

func TestHeadroomGuardBlocksAggressivePacking(t *testing.T) {
	// Two nodes at 46% each: candidates (<50%), but merging them leaves only
	// 7.5% free CPU on the survivor (<10% headroom) → guard must refuse.
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 1850, 2048),
			runningPod("b1", "node-b", "wb", 1850, 2048),
		},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 0 {
		t.Fatalf("headroom guard should block: %+v", p.Removals)
	}
}

func TestPDBBlocksRemoval(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072),
			runningPod("b1", "node-b", "web", 400, 1024),
		},
	)
	snap.PDBs = []model.PDB{{
		Namespace: "default", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 0,
	}}
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 0 {
		t.Fatalf("PDB must block node-b removal: %+v", p.Removals)
	}
}

func TestDoNotEvictPinsNode(t *testing.T) {
	pinned := runningPod("b1", "node-b", "wb", 400, 1024)
	pinned.DoNotEvict = true
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{runningPod("a1", "node-a", "wa", 2400, 3072), pinned},
	)
	p, _ := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if len(p.Removals) != 0 {
		t.Fatalf("do-not-evict must pin the node: %+v", p.Removals)
	}
}

func TestControlPlaneNeverRemoved(t *testing.T) {
	cp := m5xlarge("cp-1")
	cp.Labels["node-role.kubernetes.io/control-plane"] = ""
	snap := snapshot(
		[]model.NodeSpec{cp, m5xlarge("node-a")},
		[]model.PodSpec{runningPod("a1", "node-a", "wa", 2400, 3072)},
	)
	p, _ := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	for _, r := range p.Removals {
		if r.Node == "cp-1" {
			t.Fatal("control-plane node removed")
		}
	}
}

func TestDaemonSetPodsDoNotBlock(t *testing.T) {
	ds := runningPod("ds1", "node-b", "fluentd", 100, 128)
	ds.Workload.Kind = model.KindDaemonSet
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2000, 3072),
			runningPod("b1", "node-b", "wb", 400, 1024),
			ds,
		},
	)
	p, _ := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if len(p.Removals) != 1 || p.Removals[0].Node != "node-b" {
		t.Fatalf("node-b should be removable despite DS pod: %+v", p.Removals)
	}
	if p.Removals[0].DaemonSetPods != 1 || p.Removals[0].EvictedPods != 1 {
		t.Fatalf("DS accounting wrong: %+v", p.Removals[0])
	}
	// DS pod must not appear as an evict step.
	for _, s := range p.Steps {
		if s.Type == StepEvictPod && s.PodUID == "ds1" {
			t.Fatal("DS pod scheduled for eviction")
		}
	}
}

func rec(wl string, curCPU, curMemMiB, tgtCPU, tgtMemMiB int64, conf float64) recommend.Recommendation {
	return recommend.Recommendation{
		Key: model.ContainerKey{
			Workload:  model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: wl},
			Container: "app",
		},
		CurrentRequest: model.Resources{MilliCPU: curCPU, MemoryBytes: curMemMiB << 20},
		TargetRequest:  model.Resources{MilliCPU: tgtCPU, MemoryBytes: tgtMemMiB << 20},
		Confidence:     conf,
	}
}

func TestRightsizingUnlocksConsolidation(t *testing.T) {
	nodes := []model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")}
	pods := []model.PodSpec{
		runningPod("a1", "node-a", "wa", 3000, 4096),
		runningPod("b1", "node-b", "wb", 3000, 4096),
	}
	// Without recommendations: both nodes 75% utilized → nothing to do.
	bare, _ := Build(snapshot(nodes, pods), nil, pricing.Embedded(), DefaultConfig())
	if len(bare.Removals) != 0 {
		t.Fatalf("no recs: expected no removals, got %+v", bare.Removals)
	}
	// With high-confidence shrink recs, one node becomes removable.
	recs := []recommend.Recommendation{
		rec("wa", 3000, 4096, 300, 512, 0.9),
		rec("wb", 3000, 4096, 300, 512, 0.9),
	}
	p, _ := Build(snapshot(nodes, pods), recs, pricing.Embedded(), DefaultConfig())
	if len(p.Removals) != 1 {
		t.Fatalf("rightsizing should unlock 1 removal, got %+v", p.Removals)
	}
	if len(p.Rightsizing) != 2 {
		t.Fatalf("expected 2 accepted recs, got %d", len(p.Rightsizing))
	}
	// Reclaimed: 2 × (2700m, 3584Mi).
	if p.ReclaimedRequests.MilliCPU != 5400 {
		t.Fatalf("reclaimed cpu %d", p.ReclaimedRequests.MilliCPU)
	}
	// Resize steps come before node steps.
	if p.Steps[0].Type != StepResizeWorkload || p.Steps[1].Type != StepResizeWorkload {
		t.Fatalf("resize steps must lead the plan: %v", p.Steps[0].Type)
	}
	if p.SavingsMonthlyUSD <= 0 {
		t.Fatal("consolidation savings must be positive")
	}
}

func TestLowConfidenceRecommendationIgnored(t *testing.T) {
	nodes := []model.NodeSpec{m5xlarge("node-a")}
	pods := []model.PodSpec{runningPod("a1", "node-a", "wa", 3000, 4096)}
	recs := []recommend.Recommendation{rec("wa", 3000, 4096, 300, 512, 0.3)}
	p, _ := Build(snapshot(nodes, pods), recs, pricing.Embedded(), DefaultConfig())
	if len(p.Rightsizing) != 0 || !p.Empty() {
		t.Fatalf("low-confidence rec must be ignored: %+v", p.Rightsizing)
	}
}

func TestMaxRemovalsBound(t *testing.T) {
	var nodes []model.NodeSpec
	var pods []model.PodSpec
	for i := 0; i < 8; i++ {
		n := fmt.Sprintf("node-%d", i)
		nodes = append(nodes, m5xlarge(n))
		pods = append(pods, runningPod(fmt.Sprintf("p%d", i), n, fmt.Sprintf("w%d", i), 300, 512))
	}
	cfg := DefaultConfig()
	cfg.MaxNodeRemovals = 2
	p, _ := Build(snapshot(nodes, pods), nil, pricing.Embedded(), cfg)
	if len(p.Removals) != 2 {
		t.Fatalf("removals must respect bound: got %d", len(p.Removals))
	}
}

func TestGreenfieldFloorReported(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b"), m5xlarge("node-c")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 500, 1024),
			runningPod("b1", "node-b", "wb", 500, 1024),
			runningPod("c1", "node-c", "wc", 500, 1024),
		},
	)
	p, _ := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if p.GreenfieldHourlyUSD <= 0 {
		t.Fatal("greenfield floor should be computed")
	}
	if p.GreenfieldHourlyUSD >= p.CurrentHourlyUSD {
		t.Fatalf("floor $%.3f should undercut current $%.3f", p.GreenfieldHourlyUSD, p.CurrentHourlyUSD)
	}
}

func TestPlanDeterminism(t *testing.T) {
	mk := func() *Plan {
		var nodes []model.NodeSpec
		var pods []model.PodSpec
		for i := 0; i < 6; i++ {
			n := fmt.Sprintf("node-%d", i)
			nodes = append(nodes, m5xlarge(n))
			pods = append(pods, runningPod(fmt.Sprintf("p%d", i), n, fmt.Sprintf("w%d", i%3), int64(200+i*100), 512))
		}
		p, _ := Build(snapshot(nodes, pods), nil, pricing.Embedded(), DefaultConfig())
		return p
	}
	a, b := mk(), mk()
	if fmt.Sprintf("%+v", a.Removals) != fmt.Sprintf("%+v", b.Removals) {
		t.Fatalf("plans differ:\n%+v\n%+v", a.Removals, b.Removals)
	}
	if len(a.Steps) != len(b.Steps) {
		t.Fatal("step counts differ")
	}
}

func TestKarpenterNodesRespected(t *testing.T) {
	karp := m5xlarge("karp-1")
	karp.ManagedBy = "karpenter"
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), karp},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2000, 3072),
			runningPod("k1", "karp-1", "wk", 200, 512), // 5% utilized — prime candidate
		},
	)
	p, _ := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if len(p.Removals) != 0 {
		t.Fatalf("karpenter node must not be consolidated by default: %+v", p.Removals)
	}
	noteFound := false
	for _, n := range p.Notes {
		if len(n) > 0 && (n[0] == '1') {
			noteFound = true
		}
	}
	if !noteFound {
		t.Fatalf("plan should note the deferred karpenter node: %v", p.Notes)
	}
	// Override allows consolidation.
	cfg := DefaultConfig()
	cfg.RespectManagedNodes = false
	p2, _ := Build(snap, nil, pricing.Embedded(), cfg)
	if len(p2.Removals) != 1 || p2.Removals[0].Node != "karp-1" {
		t.Fatalf("override should consolidate karp-1: %+v", p2.Removals)
	}
}

func TestGuardrailModesRespected(t *testing.T) {
	nodes := []model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")}
	pods := []model.PodSpec{
		runningPod("a1", "node-a", "wa", 2400, 3072),
		runningPod("b1", "node-b", "wb", 400, 1024), // node-b would be consolidated
	}
	recs := []recommend.Recommendation{rec("wb", 400, 1024, 100, 256, 0.9)}

	// Workload-level off: no resize step, pod pins its node.
	snap := snapshot(nodes, pods)
	snap.Workloads = []model.WorkloadInfo{{
		Ref: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: "wb"}, Mode: "off",
	}}
	p, _ := Build(snap, recs, pricing.Embedded(), DefaultConfig())
	if len(p.Rightsizing) != 0 || len(p.Removals) != 0 {
		t.Fatalf("mode=off must block resize and pin node: %+v %+v", p.Rightsizing, p.Removals)
	}

	// Namespace-level recommend: same protection via inheritance.
	snap2 := snapshot(nodes, pods)
	snap2.NamespaceModes = map[string]string{"default": "recommend"}
	p2, _ := Build(snap2, recs, pricing.Embedded(), DefaultConfig())
	if len(p2.Rightsizing) != 0 || len(p2.Removals) != 0 {
		t.Fatalf("namespace recommend must inherit: %+v", p2.Steps)
	}

	// Default apply: automation proceeds.
	p3, _ := Build(snapshot(nodes, pods), recs, pricing.Embedded(), DefaultConfig())
	if len(p3.Rightsizing) != 1 || len(p3.Removals) != 1 {
		t.Fatalf("default apply should act: rs=%d rm=%d", len(p3.Rightsizing), len(p3.Removals))
	}
}

func TestBuildDoesNotMutateSnapshot(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{runningPod("a1", "node-a", "wa", 3000, 4096)},
	)
	recs := []recommend.Recommendation{rec("wa", 3000, 4096, 300, 512, 0.9)}
	before := snap.Pods[0].Containers[0].Requests
	p1, _ := Build(snap, recs, pricing.Embedded(), DefaultConfig())
	if snap.Pods[0].Containers[0].Requests != before {
		t.Fatalf("Build mutated the input snapshot: %+v", snap.Pods[0].Containers[0].Requests)
	}
	p2, _ := Build(snap, recs, pricing.Embedded(), DefaultConfig())
	if p1.Fingerprint != p2.Fingerprint {
		t.Fatalf("rebuild must be identical: %s vs %s", p1.Fingerprint, p2.Fingerprint)
	}
}
