package plan

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

// TestPDBReservationRollback: an aborted removal must not leak PDB
// reservations into the shared guard. node-b hosts two pods sharing a
// one-disruption budget (its removal must abort at the second Reserve);
// node-c hosts one pod under the same budget and must still be removable
// afterwards. Before the rollback fix, node-b's abort consumed the budget
// and the whole plan came out empty.
func TestPDBReservationRollback(t *testing.T) {
	web := func(uid, node string) model.PodSpec {
		p := runningPod(uid, node, "web", 100, 128)
		return p
	}
	solo := web("c1", "node-c")
	solo.Containers[0].Requests.MilliCPU = 300
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b"), m5xlarge("node-c")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072), // anchor: not a candidate
			web("b1", "node-b"), web("b2", "node-b"),     // 5% util → tried first
			solo, // 7.5% util → tried second
		},
	)
	snap.PDBs = []model.PDB{{
		Namespace: "default", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 1,
	}}
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 1 || p.Removals[0].Node != "node-c" {
		t.Fatalf("node-c must be removable after node-b's aborted removal: %+v", p.Removals)
	}
}

// TestPDBGroupBudgetRespected: two pods of one node sharing a single-
// disruption budget must block that node's removal (individual CanEvict
// checks would pass; only group reservation catches it).
func TestPDBGroupBudgetRespected(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072),
			runningPod("b1", "node-b", "web", 100, 128),
			runningPod("b2", "node-b", "web", 100, 128),
		},
	)
	snap.PDBs = []model.PDB{{
		Namespace: "default", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 1,
	}}
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 0 {
		t.Fatalf("budget of 1 must block evicting 2 covered pods: %+v", p.Removals)
	}
}

// terminalPod builds a finished pod still present in the snapshot
// (completed Job, failed pod pending GC).
func terminalPod(uid, node, wl string, kind model.WorkloadKind, phase string, cpu int64) model.PodSpec {
	p := runningPod(uid, node, wl, cpu, 256)
	p.Workload.Kind = kind
	p.Phase = phase
	return p
}

// TestTerminalPodsDoNotDistortPlan: Succeeded/Failed pods hold no node
// resources and cannot be disrupted. They must not inflate utilization,
// consume simulated capacity, or receive evict steps.
func TestTerminalPodsDoNotDistortPlan(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072),
			runningPod("b1", "node-b", "wb", 300, 512),
			// A fat completed Job pod: counted, it would push node-b to ~100%
			// utilization and hide the consolidation win entirely.
			terminalPod("done1", "node-b", "batch", model.KindJob, "Succeeded", 3600),
			// A failed pod: same story.
			terminalPod("dead1", "node-b", "oops", model.KindDeployment, "Failed", 100),
		},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 1 || p.Removals[0].Node != "node-b" {
		t.Fatalf("terminal pods must not block consolidation: %+v", p.Removals)
	}
	if p.Removals[0].EvictedPods != 1 {
		t.Fatalf("only the running pod counts as evicted: %+v", p.Removals[0])
	}
	for _, s := range p.Steps {
		if s.Type == StepEvictPod && (s.PodUID == "done1" || s.PodUID == "dead1") {
			t.Fatalf("terminal pod scheduled for eviction: %+v", s)
		}
	}
}

// TestTerminalBarePodDoesNotPinNode: a completed bare pod (kubectl run
// leftovers) is not evictable, but it is also not *there* anymore — it must
// not block its node's drain forever.
func TestTerminalBarePodDoesNotPinNode(t *testing.T) {
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072),
			runningPod("b1", "node-b", "wb", 300, 512),
			terminalPod("bare1", "node-b", "debug", model.KindBarePod, "Succeeded", 100),
		},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 1 || p.Removals[0].Node != "node-b" {
		t.Fatalf("completed bare pod must not pin node-b: %+v", p.Removals)
	}
}

// TestNaNConfigFallsBackToDefaults: float comparisons involving NaN are
// always false, so a NaN threshold silently disables the guard it tunes —
// most dangerously MinClusterHeadroom, whose check reads `free/alloc < cfg`.
// NaN must fall back to the defaults, exactly like zero and negatives do.
func TestNaNConfigFallsBackToDefaults(t *testing.T) {
	// Same shape as TestHeadroomGuardBlocksAggressivePacking: merging the two
	// nodes leaves 7.5% free CPU, under the default 10% headroom.
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 1850, 2048),
			runningPod("b1", "node-b", "wb", 1850, 2048),
		},
	)
	cfg := DefaultConfig()
	cfg.MinNodeUtilization = math.NaN()
	cfg.MinConfidence = math.NaN()
	cfg.MinClusterHeadroom = math.NaN()
	p, err := Build(snap, nil, pricing.Embedded(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 0 {
		t.Fatalf("NaN headroom must not disable the headroom guard: %+v", p.Removals)
	}

	// NaN MinConfidence must fall back to 0.6, not reject everything.
	snap2 := snapshot(
		[]model.NodeSpec{m5xlarge("node-a")},
		[]model.PodSpec{runningPod("a1", "node-a", "wa", 3000, 4096)},
	)
	p2, err := Build(snap2, []recommend.Recommendation{rec("wa", 3000, 4096, 300, 512, 0.9)}, pricing.Embedded(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Rightsizing) != 1 {
		t.Fatalf("NaN MinConfidence must fall back to default, got %d accepted", len(p2.Rightsizing))
	}
}

// TestNegativeRequestsDoNotFakeCandidates: a buggy collector emitting
// negative requests must not deflate a node's utilization below the
// consolidation threshold (mirrors pkg/binpack's clamping).
func TestNegativeRequestsDoNotFakeCandidates(t *testing.T) {
	poison := runningPod("b2", "node-b", "wx", -50000, 512)
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), m5xlarge("node-b")},
		[]model.PodSpec{
			runningPod("a1", "node-a", "wa", 2400, 3072),
			runningPod("b1", "node-b", "wb", 3000, 4096), // 75%: not a candidate
			poison,
		},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) != 0 {
		t.Fatalf("negative requests must not unlock removals: %+v", p.Removals)
	}
}

// TestZeroAllocatableNodeNeverCandidate: nodes reporting zero or negative
// allocatable are broken, not empty; they must read as fully utilized.
func TestZeroAllocatableNodeNeverCandidate(t *testing.T) {
	broken := m5xlarge("node-broken")
	broken.Allocatable = model.Resources{MilliCPU: -1, MemoryBytes: -1}
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("node-a"), broken},
		[]model.PodSpec{runningPod("a1", "node-a", "wa", 2400, 3072)},
	)
	p, err := Build(snap, nil, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range p.Removals {
		if r.Node == "node-broken" {
			t.Fatalf("broken-allocatable node removed: %+v", r)
		}
	}
}

func TestBuildNilArgs(t *testing.T) {
	snap := snapshot([]model.NodeSpec{m5xlarge("node-a")}, nil)
	if _, err := Build(nil, nil, pricing.Embedded(), DefaultConfig()); err == nil {
		t.Fatal("nil snapshot must error")
	}
	if _, err := Build(snap, nil, nil, DefaultConfig()); err == nil {
		t.Fatal("nil catalog must error")
	}
}

// TestStaleRecommendationDropped: a recommendation whose workload has
// vanished from the snapshot (no pods, no workload record) must not emit a
// resize step the actuator can only fail on. A scaled-to-zero workload that
// still has a WorkloadInfo record keeps its step.
func TestStaleRecommendationDropped(t *testing.T) {
	nodes := []model.NodeSpec{m5xlarge("node-a")}
	pods := []model.PodSpec{runningPod("a1", "node-a", "wa", 3000, 4096)}
	recs := []recommend.Recommendation{
		rec("wa", 3000, 4096, 300, 512, 0.9),    // live: applies to a1
		rec("ghost", 1000, 1024, 100, 128, 0.9), // deleted workload: must drop
		rec("wz", 1000, 1024, 100, 128, 0.9),    // scaled to zero, still declared
	}
	snap := snapshot(nodes, pods)
	snap.Workloads = []model.WorkloadInfo{{
		Ref: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: "wz"},
	}}
	p, err := Build(snap, recs, pricing.Embedded(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var resized []string
	for _, s := range p.Steps {
		if s.Type == StepResizeWorkload {
			resized = append(resized, s.Workload.Name)
		}
	}
	if fmt.Sprint(resized) != fmt.Sprint([]string{"wa", "wz"}) {
		t.Fatalf("resize steps %v, want [wa wz] (ghost dropped)", resized)
	}
	if len(p.Rightsizing) != 2 {
		t.Fatalf("rightsizing list must mirror emitted steps, got %d", len(p.Rightsizing))
	}
	noted := false
	for _, n := range p.Notes {
		if strings.Contains(n, "stale") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("dropped stale rec must be noted: %v", p.Notes)
	}
	assertSeqContiguous(t, p)
}

// assertSeqContiguous checks the plan's step-sequence invariant: Seq is
// 1..len(steps) in slice order.
func assertSeqContiguous(t *testing.T, p *Plan) {
	t.Helper()
	for i, s := range p.Steps {
		if s.Seq != i+1 {
			t.Fatalf("step %d has seq %d; want contiguous 1..%d", i, s.Seq, len(p.Steps))
		}
	}
}

// TestFingerprintContract pins what the fingerprint does and does not see:
// it identifies the plan's actions and targets, ignores the from-values
// (approval means "you may set X to T", however X drifted), and is
// order-sensitive (steps are an ordered program).
func TestFingerprintContract(t *testing.T) {
	base := []Step{
		{Type: StepResizeWorkload, Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "d", Name: "w"},
			Container: "app", FromReq: model.Resources{MilliCPU: 1000}, ToReq: model.Resources{MilliCPU: 300}},
		{Type: StepCordonNode, Node: "n1"},
	}
	same := []Step{
		{Type: StepResizeWorkload, Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "d", Name: "w"},
			Container: "app", FromReq: model.Resources{MilliCPU: 4000}, ToReq: model.Resources{MilliCPU: 300}},
		{Type: StepCordonNode, Node: "n1"},
	}
	if fingerprint(base) != fingerprint(same) {
		t.Fatal("from-values must not change the fingerprint")
	}
	differentTarget := []Step{
		{Type: StepResizeWorkload, Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "d", Name: "w"},
			Container: "app", ToReq: model.Resources{MilliCPU: 400}},
		{Type: StepCordonNode, Node: "n1"},
	}
	if fingerprint(base) == fingerprint(differentTarget) {
		t.Fatal("a different resize target must change the fingerprint")
	}
	reordered := []Step{base[1], base[0]}
	if fingerprint(base) == fingerprint(reordered) {
		t.Fatal("step order is part of the plan's identity")
	}
	if fingerprint(nil) != fingerprint([]Step{}) {
		t.Fatal("empty fingerprints must agree")
	}
}

// mustCatalog builds a pricing catalog from raw JSON for adversarial-catalog
// tests.
func mustCatalog(t *testing.T, instancesJSON string) *pricing.Catalog {
	t.Helper()
	c, err := pricing.Load(strings.NewReader(`{"instances":[` + instancesJSON + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// spotTestSnap: two safe on-demand "web" replicas on an aws node.
func spotTestSnap() *model.ClusterSnapshot {
	return snapshot(
		[]model.NodeSpec{m5xlarge("od-1")},
		[]model.PodSpec{
			runningPod("w1", "od-1", "web", 500, 512),
			runningPod("w2", "od-1", "web", 500, 512),
		},
	)
}

// TestSpotDiscountIgnoresGarbageCatalog: a catalog row whose spot price is at
// or above on-demand is bad data, not a discount. Before the guard it dragged
// the averaged discount negative (1 − 0.5/0.1 = −4) and the "savings"
// estimate below zero — a nonsense number surfaced to operators.
func TestSpotDiscountIgnoresGarbageCatalog(t *testing.T) {
	garbage := mustCatalog(t,
		`{"provider":"aws","name":"weird.large","milliCPU":2000,"memoryBytes":8589934592,"hourlyUSD":0.1,"spotHourlyUSD":0.5}`)
	rep := BuildSpotReport(spotTestSnap(), garbage, 2)
	if rep.DiscountApplied != 0.65 {
		t.Fatalf("garbage-only catalog must fall back to 0.65, got %v", rep.DiscountApplied)
	}
	if rep.EstMonthlySavingsUSD <= 0 {
		t.Fatalf("savings estimate must stay positive, got %v", rep.EstMonthlySavingsUSD)
	}

	// A garbage row next to a real one must simply be ignored.
	mixed := mustCatalog(t,
		`{"provider":"aws","name":"weird.large","milliCPU":2000,"memoryBytes":8589934592,"hourlyUSD":0.1,"spotHourlyUSD":0.5},
		 {"provider":"aws","name":"fine.large","milliCPU":2000,"memoryBytes":8589934592,"hourlyUSD":0.1,"spotHourlyUSD":0.03}`)
	rep2 := BuildSpotReport(spotTestSnap(), mixed, 2)
	if rep2.DiscountApplied < 0.699 || rep2.DiscountApplied > 0.701 {
		t.Fatalf("mixed catalog must average only real discounts (0.70), got %v", rep2.DiscountApplied)
	}
}

// TestSpotReportNilTolerant: report builders are called from CLI paths where
// snapshot or catalog may be absent; they must degrade, not panic.
func TestSpotReportNilTolerant(t *testing.T) {
	if rep := BuildSpotReport(nil, pricing.Embedded(), 2); len(rep.Workloads) != 0 || rep.EstMonthlySavingsUSD != 0 {
		t.Fatalf("nil snapshot must yield an empty report: %+v", rep)
	}
	rep := BuildSpotReport(spotTestSnap(), nil, 2)
	if rep.DiscountApplied != 0.65 {
		t.Fatalf("nil catalog must fall back to 0.65, got %v", rep.DiscountApplied)
	}
	if len(rep.Workloads) != 1 || !rep.Workloads[0].Safe {
		t.Fatalf("nil catalog must not change safety scoring: %+v", rep.Workloads)
	}
	if got := InterruptedSpotNodes(nil); got != nil {
		t.Fatalf("nil snapshot: want nil, got %v", got)
	}
	if p := EmergencyDrainPlan(nil, "gone"); p == nil || len(p.Steps) != 0 {
		t.Fatalf("nil snapshot must yield an empty non-nil plan: %+v", p)
	}
}

// TestSpotReportNegativeRequestsClamped: one poisoned replica must not
// cancel out its siblings' real savings.
func TestSpotReportNegativeRequestsClamped(t *testing.T) {
	snap := spotTestSnap()
	snap.Pods[1].Containers[0].Requests = model.Resources{MilliCPU: -8000, MemoryBytes: -1 << 40}
	rep := BuildSpotReport(snap, pricing.Embedded(), 2)
	if rep.SafeRequests.MilliCPU != 500 || rep.SafeRequests.MemoryBytes != 512<<20 {
		t.Fatalf("negative requests must clamp to zero, got %+v", rep.SafeRequests)
	}
	if rep.EstMonthlySavingsUSD < 0 {
		t.Fatalf("savings estimate went negative: %v", rep.EstMonthlySavingsUSD)
	}
}

// TestEmergencyDrainSkipsTerminalPods: evicting an already-finished pod
// wastes the seconds that matter most during a spot interruption.
func TestEmergencyDrainSkipsTerminalPods(t *testing.T) {
	done := runningPod("done1", "dying", "batch", 100, 128)
	done.Phase = "Succeeded"
	snap := snapshot(
		[]model.NodeSpec{m5xlarge("dying")},
		[]model.PodSpec{runningPod("w1", "dying", "web", 500, 512), done},
	)
	p := EmergencyDrainPlan(snap, "dying")
	for _, s := range p.Steps {
		if s.Type == StepEvictPod && s.PodUID == "done1" {
			t.Fatalf("terminal pod must not be evicted: %+v", s)
		}
	}
	if p.Fingerprint == "" {
		t.Fatal("emergency plans must be fingerprinted for the audit ledger")
	}
	assertSeqContiguous(t, p)
}

// TestEvictionTargetsSurvive: across a multi-round consolidation, a node an
// earlier step moved pods onto must never itself be removed by a later step —
// otherwise the plan is self-contradictory (it would drain replacement pods
// it has no steps for) — and no pod is evicted twice.
func TestEvictionTargetsSurvive(t *testing.T) {
	var nodes []model.NodeSpec
	var pods []model.PodSpec
	for i := 0; i < 5; i++ {
		n := fmt.Sprintf("node-%d", i)
		nodes = append(nodes, m5xlarge(n))
		pods = append(pods, runningPod(fmt.Sprintf("p%d", i), n, fmt.Sprintf("w%d", i), 150, 256))
	}
	cfg := DefaultConfig()
	cfg.MaxNodeRemovals = 4
	p, err := Build(snapshot(nodes, pods), nil, pricing.Embedded(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Removals) == 0 {
		t.Fatal("scenario should consolidate at least one node")
	}
	removed := map[string]bool{}
	for _, r := range p.Removals {
		removed[r.Node] = true
	}
	seen := map[string]bool{}
	for _, s := range p.Steps {
		if s.Type != StepEvictPod {
			continue
		}
		if removed[s.TargetNode] {
			t.Fatalf("evict target %s is removed later in the same plan", s.TargetNode)
		}
		if seen[s.PodUID] {
			t.Fatalf("pod %s evicted twice", s.PodUID)
		}
		seen[s.PodUID] = true
	}
	assertSeqContiguous(t, p)
}

// TestMaxKeyDeterministic: ties must break lexicographically regardless of
// map iteration order, or dominantProviderArch flaps between builds.
func TestMaxKeyDeterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := maxKey(map[string]int{"gcp": 2, "aws": 2, "azure": 1}); got != "aws" {
			t.Fatalf("tie must break to lexicographic min, got %q", got)
		}
	}
	if got := maxKey(nil); got != "" {
		t.Fatalf("empty map must yield empty key, got %q", got)
	}
}
