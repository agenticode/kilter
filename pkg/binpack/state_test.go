package binpack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

func node(name string, cpu, memGiB int64, labels map[string]string) model.NodeSpec {
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels["kubernetes.io/hostname"]; !ok {
		labels["kubernetes.io/hostname"] = name
	}
	return model.NodeSpec{
		Name: name, Labels: labels, Ready: true,
		Capacity:    model.Resources{MilliCPU: cpu, MemoryBytes: memGiB << 30},
		Allocatable: model.Resources{MilliCPU: cpu, MemoryBytes: memGiB << 30},
	}
}

func pod(uid string, cpu, memMiB int64, wl string) *model.PodSpec {
	return &model.PodSpec{
		UID: uid, Name: uid, Namespace: "default",
		Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: wl},
		Containers: []model.ContainerSpec{{Name: "app",
			Requests: model.Resources{MilliCPU: cpu, MemoryBytes: memMiB << 20}}},
	}
}

func TestFitsResources(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{node("n1", 1000, 2, nil)}, nil)
	if err := cs.Fits(pod("p1", 500, 1024, "w"), "n1"); err != nil {
		t.Fatalf("should fit: %v", err)
	}
	if err := cs.Fits(pod("p2", 1500, 512, "w"), "n1"); err == nil {
		t.Fatal("cpu overflow should not fit")
	}
	if err := cs.Fits(pod("p3", 100, 4096, "w"), "n1"); err == nil {
		t.Fatal("memory overflow should not fit")
	}
	// After placing, free shrinks.
	if err := cs.Place(pod("p1", 800, 1024, "w"), "n1"); err != nil {
		t.Fatal(err)
	}
	if err := cs.Fits(pod("p4", 500, 512, "w"), "n1"); err == nil {
		t.Fatal("should no longer fit after placement")
	}
}

func TestFitsNodeConditions(t *testing.T) {
	notReady := node("nr", 4000, 8, nil)
	notReady.Ready = false
	cordoned := node("cd", 4000, 8, nil)
	cordoned.Unschedulable = true
	cs := NewClusterState([]model.NodeSpec{notReady, cordoned}, nil)
	if err := cs.Fits(pod("p", 100, 128, "w"), "nr"); err == nil {
		t.Fatal("not-ready node must reject")
	}
	if err := cs.Fits(pod("p", 100, 128, "w"), "cd"); err == nil {
		t.Fatal("cordoned node must reject")
	}
	if err := cs.Fits(pod("p", 100, 128, "w"), "nope"); err == nil {
		t.Fatal("unknown node must error")
	}
}

func TestPodLimit(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{node("n1", 100000, 100, nil)}, nil)
	ns, _ := cs.Node("n1")
	ns.MaxPods = 3
	for i := 0; i < 3; i++ {
		p := pod(fmt.Sprintf("p%d", i), 10, 16, "w")
		if err := cs.Fits(p, "n1"); err != nil {
			t.Fatal(err)
		}
		cs.Place(p, "n1")
	}
	if err := cs.Fits(pod("p9", 10, 16, "w"), "n1"); err == nil {
		t.Fatal("pod limit must reject")
	}
}

func TestNodeSelectorAndAffinity(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{
		node("gpu-1", 8000, 32, map[string]string{"accel": "gpu", "disk": "ssd", "cpus": "8"}),
		node("std-1", 8000, 32, map[string]string{"disk": "hdd", "cpus": "4"}),
	}, nil)

	sel := pod("s1", 100, 128, "w")
	sel.NodeSelector = map[string]string{"accel": "gpu"}
	if err := cs.Fits(sel, "gpu-1"); err != nil {
		t.Fatal(err)
	}
	if err := cs.Fits(sel, "std-1"); err == nil {
		t.Fatal("selector must reject std-1")
	}

	cases := []struct {
		req      model.NodeSelectorRequirement
		gpu, std bool
	}{
		{model.NodeSelectorRequirement{Key: "disk", Operator: "In", Values: []string{"ssd", "nvme"}}, true, false},
		{model.NodeSelectorRequirement{Key: "disk", Operator: "NotIn", Values: []string{"hdd"}}, true, false},
		{model.NodeSelectorRequirement{Key: "accel", Operator: "Exists"}, true, false},
		{model.NodeSelectorRequirement{Key: "accel", Operator: "DoesNotExist"}, false, true},
		{model.NodeSelectorRequirement{Key: "cpus", Operator: "Gt", Values: []string{"6"}}, true, false},
		{model.NodeSelectorRequirement{Key: "cpus", Operator: "Lt", Values: []string{"6"}}, false, true},
	}
	for i, c := range cases {
		p := pod(fmt.Sprintf("a%d", i), 100, 128, "w")
		p.RequiredAffinity = []model.NodeSelectorTerm{{MatchExpressions: []model.NodeSelectorRequirement{c.req}}}
		if got := cs.Fits(p, "gpu-1") == nil; got != c.gpu {
			t.Errorf("case %d gpu: got %v want %v", i, got, c.gpu)
		}
		if got := cs.Fits(p, "std-1") == nil; got != c.std {
			t.Errorf("case %d std: got %v want %v", i, got, c.std)
		}
	}

	// ORed terms: matches if either term matches.
	p := pod("or1", 100, 128, "w")
	p.RequiredAffinity = []model.NodeSelectorTerm{
		{MatchExpressions: []model.NodeSelectorRequirement{{Key: "accel", Operator: "Exists"}}},
		{MatchExpressions: []model.NodeSelectorRequirement{{Key: "disk", Operator: "In", Values: []string{"hdd"}}}},
	}
	if err := cs.Fits(p, "std-1"); err != nil {
		t.Fatalf("ORed terms should match std-1: %v", err)
	}
}

func TestTaints(t *testing.T) {
	tainted := node("t1", 8000, 32, nil)
	tainted.Taints = []model.Taint{
		{Key: "dedicated", Value: "infra", Effect: "NoSchedule"},
		{Key: "maint", Effect: "PreferNoSchedule"}, // must not block
	}
	cs := NewClusterState([]model.NodeSpec{tainted}, nil)

	plain := pod("p1", 100, 128, "w")
	if err := cs.Fits(plain, "t1"); err == nil {
		t.Fatal("untolerated NoSchedule must reject")
	}
	tol := pod("p2", 100, 128, "w")
	tol.Tolerations = []model.Toleration{{Key: "dedicated", Operator: "Equal", Value: "infra", Effect: "NoSchedule"}}
	if err := cs.Fits(tol, "t1"); err != nil {
		t.Fatalf("tolerated pod should fit: %v", err)
	}
}

func TestAntiAffinity(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{
		node("n1", 8000, 32, nil), node("n2", 8000, 32, nil),
	}, nil)
	mk := func(uid string) *model.PodSpec {
		p := pod(uid, 100, 128, "ha-app")
		p.AntiAffinityKeys = []string{"kubernetes.io/hostname"}
		return p
	}
	p1, p2, p3 := mk("r1"), mk("r2"), mk("r3")
	assign, failed := cs.Schedule([]*model.PodSpec{p1, p2, p3})
	if len(failed) != 1 {
		t.Fatalf("2 nodes, 3 anti-affine replicas: want 1 unschedulable, got %d (assign=%v)", len(failed), assign)
	}
	if assign[p1.UID] == assign[p2.UID] {
		t.Fatal("anti-affine replicas co-located")
	}
	if !strings.Contains(strings.Join(failed[0].Reasons, " "), "anti-affinity") {
		t.Fatalf("reason should mention anti-affinity: %v", failed[0].Reasons)
	}
	// Removing one frees the domain.
	if err := cs.Remove(p1.UID, assign[p1.UID]); err != nil {
		t.Fatal(err)
	}
	if err := cs.Fits(p3, assign[p1.UID]); err != nil {
		t.Fatalf("after removal, replica should fit: %v", err)
	}
}

func TestTopologySpread(t *testing.T) {
	mkNode := func(name, zone string) model.NodeSpec {
		return node(name, 16000, 64, map[string]string{"topology.kubernetes.io/zone": zone})
	}
	cs := NewClusterState([]model.NodeSpec{
		mkNode("a1", "za"), mkNode("b1", "zb"), mkNode("c1", "zc"),
	}, nil)
	var pods []*model.PodSpec
	for i := 0; i < 6; i++ {
		p := pod(fmt.Sprintf("sp%d", i), 100, 128, "spread-app")
		p.TopologySpread = []model.TopologySpreadConstraint{{
			MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: "DoNotSchedule",
		}}
		pods = append(pods, p)
	}
	assign, failed := cs.Schedule(pods)
	if len(failed) != 0 {
		t.Fatalf("all 6 should schedule: %+v", failed)
	}
	perZone := map[string]int{}
	for _, n := range assign {
		perZone[n[:1]]++ // a1→a etc.
	}
	if perZone["a"] != 2 || perZone["b"] != 2 || perZone["c"] != 2 {
		t.Fatalf("expected 2/2/2 spread, got %v", perZone)
	}
}

func TestDrainSimulation(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{
		node("keep-1", 4000, 16, nil), node("keep-2", 4000, 16, nil), node("drain-me", 4000, 16, nil),
	}, []model.PodSpec{
		*podOn("k1", "keep-1", 1000, 2048), *podOn("k2", "keep-2", 1000, 2048),
		*podOn("d1", "drain-me", 1000, 2048), *podOn("d2", "drain-me", 1500, 4096),
	})
	moved, err := cs.RemoveNode("drain-me")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 2 {
		t.Fatalf("expected 2 pods to move, got %d", len(moved))
	}
	_, failed := cs.Schedule(moved)
	if len(failed) != 0 {
		t.Fatalf("drained pods should fit on remaining nodes: %+v", failed)
	}
	// Now overload: drain a node whose pods cannot fit.
	cs2 := NewClusterState([]model.NodeSpec{
		node("small", 1000, 2, nil), node("big", 8000, 32, nil),
	}, []model.PodSpec{
		*podOn("h1", "big", 7000, 28<<10),
	})
	moved2, _ := cs2.RemoveNode("big")
	_, failed2 := cs2.Schedule(moved2)
	if len(failed2) != 1 {
		t.Fatal("huge pod must be unschedulable on small node")
	}
}

func podOn(uid, nodeName string, cpu, memMiB int64) *model.PodSpec {
	p := pod(uid, cpu, memMiB, "wl-"+uid)
	p.NodeName = nodeName
	return p
}

func TestScheduleDeterminism(t *testing.T) {
	build := func() (map[string]string, []Unschedulable) {
		cs := NewClusterState([]model.NodeSpec{
			node("n1", 4000, 16, nil), node("n2", 4000, 16, nil), node("n3", 4000, 16, nil),
		}, nil)
		var pods []*model.PodSpec
		for i := 0; i < 30; i++ {
			pods = append(pods, pod(fmt.Sprintf("p%02d", i), int64(100+i*37%700), int64(128+i*97%2048), fmt.Sprintf("w%d", i%5)))
		}
		return cs.Schedule(pods)
	}
	a1, f1 := build()
	a2, f2 := build()
	if len(f1) != len(f2) {
		t.Fatal("determinism: failure count differs")
	}
	for k, v := range a1 {
		if a2[k] != v {
			t.Fatalf("determinism: pod %s → %s vs %s", k, v, a2[k])
		}
	}
}

func TestBestFitPacksTightly(t *testing.T) {
	// One almost-full node and one empty node: a small pod should land on the
	// almost-full one (tightest fit), keeping the empty node free.
	cs := NewClusterState([]model.NodeSpec{
		node("full-ish", 4000, 16, nil), node("empty", 4000, 16, nil),
	}, []model.PodSpec{*podOn("base", "full-ish", 3000, 12<<10)})
	assign, failed := cs.Schedule([]*model.PodSpec{pod("tiny", 200, 256, "w")})
	if len(failed) != 0 {
		t.Fatal("tiny pod must schedule")
	}
	if assign["tiny"] != "full-ish" {
		t.Fatalf("best-fit should pick full-ish, got %s", assign["tiny"])
	}
}

func BenchmarkDrainSimulation1kNodes(b *testing.B) {
	nodes := make([]model.NodeSpec, 1000)
	pods := make([]model.PodSpec, 0, 10000)
	for i := 0; i < 1000; i++ {
		nodes[i] = node(fmt.Sprintf("n%04d", i), 16000, 64, nil)
	}
	for i := 0; i < 10000; i++ {
		p := podOn(fmt.Sprintf("p%05d", i), fmt.Sprintf("n%04d", i%1000), 400, 1024)
		pods = append(pods, *p)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs := NewClusterState(nodes, pods)
		moved, _ := cs.RemoveNode("n0007")
		_, failed := cs.Schedule(moved)
		if len(failed) != 0 {
			b.Fatal("unexpected unschedulable pods")
		}
	}
}
