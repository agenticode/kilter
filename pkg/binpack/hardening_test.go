package binpack

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// testInstance builds a minimal valid candidate for plan tests that need
// precise control over the shape (the embedded catalog is too coarse).
func testInstance(name string, milliCPU, memGiB int64, hourly float64) pricing.InstanceType {
	return pricing.InstanceType{
		Provider: "test", Name: name, Family: "test", Arch: "amd64",
		MilliCPU: milliCPU, MemoryBytes: memGiB << 30, HourlyUSD: hourly,
	}
}

func TestGarbageRequestsClamped(t *testing.T) {
	alloc := model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30}
	cases := []struct {
		name     string
		cpu, mem int64 // raw request values (mem in MiB, via pod helper)
	}{
		{"negative cpu", -5000, 128},
		{"negative mem", 200, -1024},
		{"both negative", -5000, -1024},
		{"hugely negative", -1 << 60, -1 << 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewClusterState([]model.NodeSpec{node("n1", 1000, 2, nil)}, nil)
			ns, _ := cs.Node("n1")
			evil := pod("evil", tc.cpu, tc.mem, "w")
			if err := cs.Fits(evil, "n1"); err != nil {
				t.Fatalf("negative requests should be treated as zero: %v", err)
			}
			if err := cs.Place(evil, "n1"); err != nil {
				t.Fatal(err)
			}
			// The critical invariant: garbage must never inflate free capacity,
			// and only the legitimately positive dimensions are consumed.
			if want := alloc.Sub(requestsOf(evil)); ns.Free != want {
				t.Fatalf("free capacity after garbage pod: got %s want %s", ns.Free, want)
			}
			if ns.Free.MilliCPU > alloc.MilliCPU || ns.Free.MemoryBytes > alloc.MemoryBytes {
				t.Fatalf("negative requests inflated free capacity: %s > %s", ns.Free, alloc)
			}
			// When every dimension was garbage the pod consumed nothing, so a
			// pod sized exactly to the node must still fit.
			if tc.cpu < 0 && tc.mem < 0 {
				if err := cs.Fits(pod("full", 1000, 2<<10, "w2"), "n1"); err != nil {
					t.Fatalf("full-size pod should still fit: %v", err)
				}
			}
			// Remove restores exactly what place consumed.
			if err := cs.Remove("evil", "n1"); err != nil {
				t.Fatal(err)
			}
			if ns.Free != alloc {
				t.Fatalf("remove asymmetric: free %s want %s", ns.Free, alloc)
			}
		})
	}
}

func TestNegativeExtendedRequestsDoNotMintResources(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{node("n1", 4000, 8, nil)}, nil)
	evil := pod("evil", 100, 128, "w")
	evil.Containers[0].Extended = map[string]int64{"nvidia.com/gpu": -2}
	if err := cs.Place(evil, "n1"); err != nil {
		t.Fatal(err)
	}
	gp := pod("g", 100, 128, "w2")
	gp.Containers[0].Extended = map[string]int64{"nvidia.com/gpu": 1}
	if err := cs.Fits(gp, "n1"); err == nil {
		t.Fatal("negative extended request minted GPUs on a GPU-less node")
	}
}

func TestPlaceDuplicatePodRejected(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{node("n1", 1000, 2, nil)}, nil)
	ns, _ := cs.Node("n1")
	p := pod("p1", 400, 512, "w")
	if err := cs.Place(p, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := cs.Place(p, "n1"); err == nil {
		t.Fatal("double placement of the same UID must error")
	}
	if ns.PodCount != 1 || ns.Free.MilliCPU != 600 {
		t.Fatalf("failed double placement corrupted state: count=%d free=%s", ns.PodCount, ns.Free)
	}
	// One remove fully restores — the second place really was a no-op.
	if err := cs.Remove("p1", "n1"); err != nil {
		t.Fatal(err)
	}
	if ns.PodCount != 0 || ns.Free != ns.Spec.Allocatable {
		t.Fatalf("state not restored: count=%d free=%s", ns.PodCount, ns.Free)
	}
}

func TestAddNodeDuplicateNameReplaces(t *testing.T) {
	old := pod("old", 500, 512, "ha-app")
	old.AntiAffinityKeys = []string{"kubernetes.io/hostname"}
	old.NodeName = "n1"
	cs := NewClusterState([]model.NodeSpec{node("n1", 4000, 16, nil)}, []model.PodSpec{*old})

	cs.AddNode(node("n1", 8000, 32, nil))
	if got := len(cs.Nodes()); got != 1 {
		t.Fatalf("duplicate AddNode left %d entries for one name", got)
	}
	ns, ok := cs.Node("n1")
	if !ok || ns.Free.MilliCPU != 8000 || ns.PodCount != 0 {
		t.Fatalf("replacement node not fresh: free=%s count=%d", ns.Free, ns.PodCount)
	}
	// The replaced node's pods left the simulation — including their
	// anti-affinity topology counts, so the domain is free again.
	rep := pod("rep", 500, 512, "ha-app")
	rep.AntiAffinityKeys = []string{"kubernetes.io/hostname"}
	if err := cs.Fits(rep, "n1"); err != nil {
		t.Fatalf("stale topology count survived node replacement: %v", err)
	}
	// Scheduling must land on the live node state, not a phantom.
	assign, failed := cs.Schedule([]*model.PodSpec{pod("s", 6000, 1024, "w")})
	if len(failed) != 0 || assign["s"] != "n1" {
		t.Fatalf("schedule after replacement: assign=%v failed=%v", assign, failed)
	}
	if ns.Free.MilliCPU != 2000 {
		t.Fatalf("placement hit a phantom node state: free=%s", ns.Free)
	}
}

func TestScheduleNilAndDuplicatePods(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{node("n1", 4000, 16, nil)}, nil)
	p1 := pod("dup", 500, 512, "w")
	p2 := pod("dup", 500, 512, "w") // same UID, distinct object
	assign, failed := cs.Schedule([]*model.PodSpec{nil, p1, nil, p2})
	if len(assign) != 1 || assign["dup"] != "n1" {
		t.Fatalf("want exactly one placement, got %v", assign)
	}
	if len(failed) != 1 || !strings.Contains(failed[0].Reasons[0], "duplicate pod UID") {
		t.Fatalf("duplicate must be reported, got %+v", failed)
	}
	ns, _ := cs.Node("n1")
	if ns.PodCount != 1 || ns.Free.MilliCPU != 3500 {
		t.Fatalf("duplicate was double-booked: count=%d free=%s", ns.PodCount, ns.Free)
	}
}

func TestPlanNilAndDuplicatePods(t *testing.T) {
	p1 := pod("dup", 500, 512, "w")
	p2 := pod("dup", 500, 512, "w")
	plan := PlanNodes([]*model.PodSpec{nil, p1, p2, nil}, awsCandidates(t), PlanOptions{})
	placed := 0
	for _, n := range plan.Nodes {
		placed += len(n.PodUIDs)
	}
	if placed != 1 {
		t.Fatalf("want 1 placed pod, got %d", placed)
	}
	if len(plan.Unschedulable) != 1 || !strings.Contains(plan.Unschedulable[0].Reasons[0], "duplicate pod UID") {
		t.Fatalf("duplicate must be reported, got %+v", plan.Unschedulable)
	}
	// Nil-heavy input with no candidates must not panic or emit nil pods.
	empty := PlanNodes([]*model.PodSpec{nil, nil}, nil, PlanOptions{})
	if len(empty.Unschedulable) != 0 || len(empty.Nodes) != 0 {
		t.Fatalf("nil pods leaked into the plan: %+v", empty)
	}
}

func TestPlanUnschedulableReasonIncludesDaemonSetOverhead(t *testing.T) {
	// The pod fits an EMPTY node of the only candidate (3680m allocatable after
	// the 8% system reservation), but not once the DaemonSet overhead lands.
	// The reported reason must be the resource shortfall — previously this
	// misreported as "node cap reached (MaxNodes)".
	cand := []pricing.InstanceType{testInstance("t.large", 4000, 16, 0.10)}
	ds := pod("ds", 1000, 512, "logger")
	ds.Workload.Kind = model.KindDaemonSet
	plan := PlanNodes([]*model.PodSpec{pod("big", 3400, 1024, "app")}, cand, PlanOptions{
		DaemonSetPods: []model.PodSpec{*ds},
	})
	if len(plan.Unschedulable) != 1 || len(plan.Nodes) != 0 {
		t.Fatalf("pod must be unschedulable under DS overhead: %+v", plan)
	}
	reasons := strings.Join(plan.Unschedulable[0].Reasons, " ")
	if !strings.Contains(reasons, "insufficient free resources") {
		t.Fatalf("reason must name the resource shortfall, got %q", reasons)
	}
	if strings.Contains(reasons, "node cap reached") {
		t.Fatalf("misleading MaxNodes reason for a DS-overhead failure: %q", reasons)
	}
}

func TestPlanMaxNodesCapReason(t *testing.T) {
	// Two pods that cannot share one 3680m node, capped at MaxNodes=1: the
	// leftover genuinely fits a fresh node, so the cap reason is the truth.
	cand := []pricing.InstanceType{testInstance("t.large", 4000, 16, 0.10)}
	pods := []*model.PodSpec{pod("a", 2000, 512, "w"), pod("b", 2000, 512, "w")}
	plan := PlanNodes(pods, cand, PlanOptions{MaxNodes: 1})
	if len(plan.Nodes) != 1 || len(plan.Unschedulable) != 1 {
		t.Fatalf("want 1 node + 1 unschedulable, got %+v", plan)
	}
	reasons := strings.Join(plan.Unschedulable[0].Reasons, " ")
	if !strings.Contains(reasons, "node cap reached (MaxNodes)") {
		t.Fatalf("want MaxNodes cap reason, got %q", reasons)
	}
}

func TestPlanOptionsWithDefaults(t *testing.T) {
	cases := []struct {
		name         string
		in           PlanOptions
		wantFrac     float64
		wantMaxPods  int
		wantMaxNodes int
	}{
		{"zero value", PlanOptions{}, 0.08, 110, 5000},
		{"NaN fraction", PlanOptions{SystemReservedFraction: math.NaN()}, 0.08, 110, 5000},
		{"+Inf fraction", PlanOptions{SystemReservedFraction: math.Inf(1)}, 0.08, 110, 5000},
		{"negative fraction", PlanOptions{SystemReservedFraction: -0.3}, 0.08, 110, 5000},
		{"absurd fraction", PlanOptions{SystemReservedFraction: 0.6}, 0.08, 110, 5000},
		{"negative caps", PlanOptions{MaxPodsPerNode: -1, MaxNodes: -1}, 0.08, 110, 5000},
		{"valid kept", PlanOptions{SystemReservedFraction: 0.2, MaxPodsPerNode: 50, MaxNodes: 7}, 0.2, 50, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.SystemReservedFraction != tc.wantFrac {
				t.Errorf("SystemReservedFraction=%v want %v", got.SystemReservedFraction, tc.wantFrac)
			}
			if got.MaxPodsPerNode != tc.wantMaxPods || got.MaxNodes != tc.wantMaxNodes {
				t.Errorf("MaxPods=%d MaxNodes=%d want %d/%d",
					got.MaxPodsPerNode, got.MaxNodes, tc.wantMaxPods, tc.wantMaxNodes)
			}
		})
	}
}

func TestTopologySpreadGarbageMaxSkew(t *testing.T) {
	// Kubernetes validation requires maxSkew >= 1; garbage 0 or negative must
	// behave like the strictest valid constraint, not brick the workload.
	for _, skew := range []int32{0, -3} {
		t.Run(fmt.Sprintf("maxSkew=%d", skew), func(t *testing.T) {
			cs := NewClusterState([]model.NodeSpec{
				node("a1", 16000, 64, map[string]string{"topology.kubernetes.io/zone": "za"}),
				node("b1", 16000, 64, map[string]string{"topology.kubernetes.io/zone": "zb"}),
			}, nil)
			var pods []*model.PodSpec
			for i := 0; i < 4; i++ {
				p := pod(fmt.Sprintf("sp%d", i), 100, 128, "spread-app")
				p.TopologySpread = []model.TopologySpreadConstraint{{
					MaxSkew: skew, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: "DoNotSchedule",
				}}
				pods = append(pods, p)
			}
			assign, failed := cs.Schedule(pods)
			if len(failed) != 0 {
				t.Fatalf("garbage maxSkew bricked scheduling: %+v", failed)
			}
			perZone := map[string]int{}
			for _, n := range assign {
				perZone[n[:1]]++
			}
			if perZone["a"] != 2 || perZone["b"] != 2 {
				t.Fatalf("expected 2/2 spread, got %v", perZone)
			}
		})
	}
}

func TestPlanRejectsInvalidPriceCandidates(t *testing.T) {
	good := testInstance("good.large", 4000, 16, 0.10)
	neg := testInstance("neg.large", 8000, 32, -0.50)
	nan := testInstance("nan.large", 8000, 32, math.NaN())
	inf := testInstance("inf.large", 8000, 32, math.Inf(1))
	pods := []*model.PodSpec{pod("a", 1000, 1024, "w"), pod("b", 1000, 1024, "w")}

	plan := PlanNodes(pods, []pricing.InstanceType{neg, nan, inf, good}, PlanOptions{})
	if len(plan.Unschedulable) != 0 {
		t.Fatalf("unschedulable: %+v", plan.Unschedulable)
	}
	for _, n := range plan.Nodes {
		if n.Type.Name != "good.large" {
			t.Fatalf("plan used invalid-price candidate %s", n.Type.Name)
		}
	}
	if plan.TotalHourlyUSD <= 0 {
		t.Fatalf("cost corrupted: %v", plan.TotalHourlyUSD)
	}
	// Only invalid candidates: an honest empty plan, never a negative-cost one.
	broken := PlanNodes(pods, []pricing.InstanceType{neg, nan}, PlanOptions{})
	if len(broken.Nodes) != 0 || broken.TotalHourlyUSD != 0 {
		t.Fatalf("invalid-price candidates produced a plan: %+v", broken)
	}
	if len(broken.Unschedulable) != 2 ||
		!strings.Contains(broken.Unschedulable[0].Reasons[0], "no candidate instance types") {
		t.Fatalf("want no-candidates reasons, got %+v", broken.Unschedulable)
	}
}

func TestRemoveErrors(t *testing.T) {
	cs := NewClusterState([]model.NodeSpec{node("n1", 1000, 2, nil)}, nil)
	if err := cs.Remove("x", "nope"); err == nil {
		t.Fatal("unknown node must error")
	}
	if err := cs.Remove("ghost", "n1"); err == nil {
		t.Fatal("unknown pod must error")
	}
	if _, err := cs.RemoveNode("nope"); err == nil {
		t.Fatal("removing unknown node must error")
	}
}
