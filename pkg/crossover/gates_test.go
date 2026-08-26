package crossover

import (
	"reflect"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// sparseOpts is the golden scenario that wins on Fargate (see
// TestGoldenSparseWinsFargate). Every gate test starts from it, so a block can
// only come from the gate under test — never from the fixture being unwinnable.
func sparseOpts() Options {
	return Options{Candidates: []pricing.InstanceType{m5large, m5xlarge}}
}

func TestControlCaseWinsFargateSoGateTestsMeanSomething(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)}, sparseOpts())
	if rep.Verdict != VerdictFargate || len(rep.Blocks) != 0 {
		t.Fatalf("the gate tests' control case must be an unblocked Fargate win, got %q %+v",
			rep.Verdict, rep.Blocks)
	}
}

// TestEachGateBlocksIndependently walks every §4.3 gate. For each, exactly one
// property of exactly one pod is changed on a pod set that otherwise wins on
// Fargate, and the report must come back blocked — by that gate, naming that
// pod, claiming no savings, and never with a Fargate verdict.
func TestEachGateBlocksIndependently(t *testing.T) {
	cases := []struct {
		gate    Gate
		present func(*Facts)
		unknown func(*Facts)
	}{
		{GateDaemonSet,
			func(f *Facts) { f.DaemonSet = Present }, func(f *Facts) { f.DaemonSet = Unknown }},
		{GateExtendedResource,
			func(f *Facts) { f.ExtendedResource = Present }, func(f *Facts) { f.ExtendedResource = Unknown }},
		{GateEBSVolume,
			func(f *Facts) { f.EBSVolume = Present }, func(f *Facts) { f.EBSVolume = Unknown }},
		{GatePrivileged,
			func(f *Facts) { f.Privileged = Present }, func(f *Facts) { f.Privileged = Unknown }},
		{GateHostPath,
			func(f *Facts) { f.HostPath = Present }, func(f *Facts) { f.HostPath = Unknown }},
		{GateHostNetwork,
			func(f *Facts) { f.HostNetwork = Present }, func(f *Facts) { f.HostNetwork = Unknown }},
		{GateHostPort,
			func(f *Facts) { f.HostPort = Present }, func(f *Facts) { f.HostPort = Unknown }},
		{GatePrivateSubnet,
			func(f *Facts) { f.NoPrivateSubnet = Present }, func(f *Facts) { f.NoPrivateSubnet = Unknown }},
		{GateEvictionIntolerant,
			func(f *Facts) { f.EvictionIntolerant = Present }, func(f *Facts) { f.EvictionIntolerant = Unknown }},
	}
	if len(cases) != len(AllGates())-1 {
		t.Fatalf("gate table drift: %d fact gates covered, %d gates declared (minus size-ceiling)",
			len(cases), len(AllGates())-1)
	}

	for _, tc := range cases {
		for _, variant := range []struct {
			name  string
			apply func(*Facts)
			kind  BlockKind
		}{
			{"present", tc.present, BlockViolation},
			{"unobserved", tc.unknown, BlockUnverified},
		} {
			t.Run(string(tc.gate)+"/"+variant.name, func(t *testing.T) {
				pods := mkPods(6, 200, 512)
				variant.apply(&pods[2].Facts)
				rep := Analyze(testNow, PodSet{Pods: pods}, sparseOpts())

				if rep.Verdict != VerdictFargateBlocked {
					t.Fatalf("verdict = %q, want %q\n%s", rep.Verdict, VerdictFargateBlocked, rep.Summary())
				}
				if rep.Fargate.Eligible {
					t.Errorf("a blocked pod set must not be eligible")
				}
				if rep.MonthlySavingsUSD != 0 || rep.SavingsFraction != 0 {
					t.Errorf("a blocked report claimed savings: %v (%v)", rep.MonthlySavingsUSD, rep.SavingsFraction)
				}
				if len(rep.Blocks) != 1 {
					t.Fatalf("want exactly one block, got %+v", rep.Blocks)
				}
				got := rep.Blocks[0]
				if got.Gate != tc.gate || got.Kind != variant.kind {
					t.Fatalf("block = %s/%s, want %s/%s", got.Gate, got.Kind, tc.gate, variant.kind)
				}
				if !reflect.DeepEqual(got.Pods, []string{"default/p02"}) {
					t.Errorf("block names %v, want just the pod that tripped it", got.Pods)
				}
				if got.Reason == "" || !strings.HasPrefix(got.Reason, string(tc.gate)+":") {
					t.Errorf("block reason %q must lead with the gate name", got.Reason)
				}
				// The headline must never read as a price recommendation.
				if h := rep.Headline(); !strings.Contains(h, "not an option") || strings.Contains(h, "cheaper") {
					t.Errorf("blocked headline reads as advice: %q", h)
				}
				// The Fargate bill is still arithmetic — but it is not advice.
				if rep.Fargate.HourlyUSD <= 0 {
					t.Errorf("the Fargate side should still be priced for reference")
				}
			})
		}
	}
}

// TestSizeCeilingGateBlocksAlone covers the one gate that is computed rather
// than observed: a pod with no valid Fargate configuration at all.
func TestSizeCeilingGateBlocksAlone(t *testing.T) {
	pods := mkPods(6, 200, 512)
	// 16 vCPU / 120 GB is the ceiling; 120 GiB of request plus the 256 MiB
	// overhead has nowhere to go.
	pods = append(pods, mkPod("toobig", 16000, 120<<10))
	// A node big enough to hold the oversize pod, so the EC2 side stays
	// feasible and the only thing blocking is the Fargate ceiling itself.
	rep := Analyze(testNow, PodSet{Pods: pods}, Options{
		Candidates: []pricing.InstanceType{m5large, it("m5.24xlarge", 96000, 384, 4.608, "")},
	})
	if rep.Verdict != VerdictFargateBlocked {
		t.Fatalf("verdict = %q, want %q\n%s", rep.Verdict, VerdictFargateBlocked, rep.Summary())
	}
	if len(rep.Blocks) != 1 || rep.Blocks[0].Gate != GateSizeCeiling {
		t.Fatalf("blocks = %+v, want exactly the size-ceiling gate", rep.Blocks)
	}
	if !reflect.DeepEqual(rep.Fargate.Unpriced, []string{"default/toobig"}) {
		t.Errorf("unpriced = %v, want the oversize pod", rep.Fargate.Unpriced)
	}
	// The other six pods are still priced: an unpriceable pod is excluded from
	// the sum, never clamped to the ceiling and billed.
	closeTo(t, "F(P) over the priceable pods", rep.Fargate.HourlyUSD, 6*0.014565, 1e-9)
}

// TestDaemonSetTemplatesBlockAtSetLevel: the DaemonSets that make E(P) honest
// are themselves a reason Fargate cannot run the workload, and that block is a
// property of the set, so it names no pod.
func TestDaemonSetTemplatesBlockAtSetLevel(t *testing.T) {
	ds := model.PodSpec{
		UID: "ds", Name: "fluentbit", Namespace: "logging",
		Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "logging", Name: "fluentbit"},
		Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: 50, MemoryBytes: 64 * mib}}},
	}
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512), DaemonSets: []model.PodSpec{ds}}, sparseOpts())
	if rep.Verdict != VerdictFargateBlocked {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictFargateBlocked)
	}
	if len(rep.Blocks) != 1 || rep.Blocks[0].Gate != GateDaemonSet || len(rep.Blocks[0].Pods) != 0 {
		t.Fatalf("blocks = %+v, want one set-level daemonset block", rep.Blocks)
	}
	if rep.EC2.DaemonSetTemplates != 1 {
		t.Errorf("DaemonSet templates = %d, want 1", rep.EC2.DaemonSetTemplates)
	}
}

// TestZeroFactsBlocksOnEveryGate: the zero value is "nobody looked", and
// nobody-looked blocks — on all nine observable gates at once, each named.
func TestZeroFactsBlocksOnEveryGate(t *testing.T) {
	pods := mkPods(2, 200, 512)
	pods[0].Facts = Facts{}
	pods[1].Facts = Facts{}
	rep := Analyze(testNow, PodSet{Pods: pods}, sparseOpts())
	if rep.Verdict != VerdictFargateBlocked {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictFargateBlocked)
	}
	if len(rep.Blocks) != len(AllGates())-1 {
		t.Fatalf("got %d blocks, want one per observable gate (%d)", len(rep.Blocks), len(AllGates())-1)
	}
	order := AllGates()
	for i, b := range rep.Blocks {
		if b.Gate != order[i] {
			t.Errorf("block %d gate = %s, want %s (blocks must follow AllGates order)", i, b.Gate, order[i])
		}
		if b.Kind != BlockUnverified {
			t.Errorf("block %d kind = %s, want %s", i, b.Kind, BlockUnverified)
		}
	}
}

// TestBlockPodListIsCappedAndCounted: a big cluster does not produce a report
// with one line per pod, and the count it drops is never lost.
func TestBlockPodListIsCappedAndCounted(t *testing.T) {
	pods := mkPods(40, 100, 128)
	for i := range pods {
		pods[i].Facts.HostNetwork = Present
	}
	rep := Analyze(testNow, PodSet{Pods: pods}, sparseOpts())
	if len(rep.Blocks) != 1 {
		t.Fatalf("blocks = %+v", rep.Blocks)
	}
	list := rep.Blocks[0].Pods
	if len(list) != maxListedPods+1 {
		t.Fatalf("pod list length = %d, want %d + an overflow line", len(list), maxListedPods)
	}
	if want := "… and 30 more"; list[len(list)-1] != want {
		t.Errorf("overflow line = %q, want %q", list[len(list)-1], want)
	}
}

// TestFactsFromPodSpec pins exactly which properties a snapshot can already
// answer and which stay Unknown. The Unknown set is the collector's to-do list
// (FINDINGS.md §6): if a future collector fills one, this test must change.
func TestFactsFromPodSpec(t *testing.T) {
	base := model.PodSpec{
		Workload:   model.WorkloadRef{Kind: model.KindDeployment},
		Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: 100}}},
	}
	f := FactsFromPodSpec(&base)
	want := Facts{DaemonSet: Absent, ExtendedResource: Absent, HostPath: Absent, EvictionIntolerant: Absent}
	if f != want {
		t.Fatalf("plain pod facts = %+v, want %+v (five properties Unknown)", f, want)
	}

	ds := base
	ds.Workload.Kind = model.KindDaemonSet
	if FactsFromPodSpec(&ds).DaemonSet != Present {
		t.Errorf("a DaemonSet pod must be observed as such")
	}
	gpu := base
	gpu.Containers = []model.ContainerSpec{{Extended: map[string]int64{"nvidia.com/gpu": 1}}}
	if FactsFromPodSpec(&gpu).ExtendedResource != Present {
		t.Errorf("a GPU request must be observed as an extended resource")
	}
	local := base
	local.HasLocalStorage = true
	if FactsFromPodSpec(&local).HostPath != Present {
		t.Errorf("node-local storage must be observed as a host-path block")
	}
	pinned := base
	pinned.DoNotEvict = true
	if FactsFromPodSpec(&pinned).EvictionIntolerant != Present {
		t.Errorf("do-not-evict must be observed as eviction intolerance")
	}
	if FactsFromPodSpec(nil) != (Facts{}) {
		t.Errorf("a nil pod must be all-Unknown, i.e. fully blocked")
	}
}

func TestCompatibleFactsPassesEveryGate(t *testing.T) {
	if got := evaluateGates([]Pod{{Facts: CompatibleFacts()}}); len(got) != 0 {
		t.Fatalf("CompatibleFacts blocked on %+v", got)
	}
	// And it really is every field, not a subset that happens to be checked.
	v := reflect.ValueOf(CompatibleFacts())
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Interface().(Fact) != Absent {
			t.Errorf("field %s = %v, want Absent", v.Type().Field(i).Name, v.Field(i))
		}
	}
}

// TestAllGatesIsACopy: the gate table is a function, not a package var, so a
// caller cannot reorder everyone else's reports.
func TestAllGatesIsACopy(t *testing.T) {
	a := AllGates()
	a[0] = "tampered"
	if AllGates()[0] == "tampered" {
		t.Fatal("AllGates handed out its backing array")
	}
}

// ---------------------------------------------------------------------------
// Snapshot wiring.
// ---------------------------------------------------------------------------

// TestFromSnapshotSplitsAndClassifies checks the three jobs FromSnapshot does:
// Fargate pods are separated (never fed to node math), DaemonSet pods become
// one overhead template each, and node-side pods carry honestly-derived facts.
func TestFromSnapshotSplitsAndClassifies(t *testing.T) {
	req := func(m, mem int64) []model.ContainerSpec {
		return []model.ContainerSpec{{Name: "app", Requests: model.Resources{MilliCPU: m, MemoryBytes: mem * mib}}}
	}
	snap := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{
			{Name: "ec2-1", Ready: true, InstanceType: "m5.large", Provider: "aws",
				Capacity: model.Resources{MilliCPU: 2000, MemoryBytes: 8 << 30}},
			{Name: "fargate-1", Ready: true, ManagedBy: model.ManagedByFargate,
				Labels:   map[string]string{model.LabelComputeType: "fargate"},
				Capacity: model.Resources{MilliCPU: 96000, MemoryBytes: 384 << 30}},
		},
		Pods: []model.PodSpec{
			{UID: "w1", Name: "web-1", Namespace: "default", NodeName: "ec2-1",
				Workload:   model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: "web"},
				Containers: req(200, 512)},
			{UID: "d1", Name: "agent-1", Namespace: "kube-system", NodeName: "ec2-1",
				Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "agent"},
				Containers: req(50, 64)},
			{UID: "d2", Name: "agent-2", Namespace: "kube-system", NodeName: "ec2-1",
				Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "agent"},
				Containers: req(50, 64)},
			{UID: "f1", Name: "batch-1", Namespace: "jobs", NodeName: "fargate-1",
				Workload:   model.WorkloadRef{Kind: model.KindJob, Namespace: "jobs", Name: "batch"},
				Containers: req(200, 512)},
		},
	}
	ps := FromSnapshot(snap)

	if len(ps.Pods) != 2 {
		t.Fatalf("workload pods = %d, want 2 (one EC2-hosted, one Fargate-hosted)", len(ps.Pods))
	}
	if len(ps.DaemonSets) != 1 || ps.DaemonSets[0].Workload.Name != "agent" {
		t.Fatalf("DaemonSet templates = %+v, want one per DaemonSet", ps.DaemonSets)
	}
	byUID := map[string]Pod{}
	for _, p := range ps.Pods {
		byUID[p.Spec.UID] = p
	}
	if got := byUID["f1"].Facts; got != CompatibleFacts() {
		t.Errorf("a pod AWS already runs on Fargate must be fully compatible, got %+v", got)
	}
	if got := byUID["w1"].Facts; got.EBSVolume != Unknown || got.HostNetwork != Unknown {
		t.Errorf("an EC2-hosted pod's unobservable properties must stay Unknown, got %+v", got)
	}

	// End to end: the Fargate node's 96 vCPU / 384 GB shell must never reach
	// node math, and the mixed set blocks because the EC2-hosted pod is
	// unverified — not because it looked expensive.
	rep := Analyze(testNow, ps, sparseOpts())
	if rep.Verdict != VerdictFargateBlocked {
		t.Fatalf("verdict = %q, want %q\n%s", rep.Verdict, VerdictFargateBlocked, rep.Summary())
	}
	for _, b := range rep.Blocks {
		if b.Kind == BlockUnverified && len(b.Pods) > 0 && b.Pods[0] != "default/web-1" {
			t.Errorf("unverified block names %v, want only the EC2-hosted pod", b.Pods)
		}
	}
	if rep.EC2.Purchased.MilliCPU >= 96000 {
		t.Errorf("the Fargate VM's shape leaked into the EC2 side: %v", rep.EC2.Purchased)
	}
}

func TestFromSnapshotNil(t *testing.T) {
	if ps := FromSnapshot(nil); len(ps.Pods) != 0 || len(ps.DaemonSets) != 0 {
		t.Fatalf("nil snapshot = %+v, want empty", ps)
	}
}

func TestFactString(t *testing.T) {
	for f, want := range map[Fact]string{Unknown: "unknown", Absent: "absent", Present: "present", Fact(9): "unknown"} {
		if got := f.String(); got != want {
			t.Errorf("Fact(%d).String() = %q, want %q", f, got, want)
		}
	}
}
