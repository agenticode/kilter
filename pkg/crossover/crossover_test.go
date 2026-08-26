package crossover

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// testNow is the fixed clock every test passes in. This package never reads
// one of its own.
var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

const mib = int64(1) << 20

// Catalog rows used by the golden scenarios, quoted from pkg/pricing/catalog.json
// so the hand arithmetic below can be checked against a fixed price.
func it(name string, mcpu int64, memGiB float64, hourly float64, arch string) pricing.InstanceType {
	if arch == "" {
		arch = "amd64"
	}
	return pricing.InstanceType{
		Provider: "aws", Name: name, Family: strings.Split(name, ".")[0], Arch: arch,
		MilliCPU: mcpu, MemoryBytes: int64(memGiB * float64(1<<30)), HourlyUSD: hourly,
	}
}

var (
	m5large    = it("m5.large", 2000, 8, 0.096, "")
	m5xlarge   = it("m5.xlarge", 4000, 16, 0.192, "")
	m52xlarge  = it("m5.2xlarge", 8000, 32, 0.384, "")
	c5large    = it("c5.large", 2000, 4, 0.085, "")
	m7g2xlarge = it("m7g.2xlarge", 8000, 32, 0.3264, "arm64")
)

// mkPod builds a workload pod whose gate facts were all observed absent — the
// only state in which Fargate may be recommended. Gate tests override them.
func mkPod(uid string, mcpu, memMiB int64) Pod {
	return Pod{
		Spec: model.PodSpec{
			UID: uid, Name: uid, Namespace: "default",
			Workload:   model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: "web"},
			Containers: []model.ContainerSpec{{Name: "app", Requests: model.Resources{MilliCPU: mcpu, MemoryBytes: memMiB * mib}}},
		},
		Facts: CompatibleFacts(),
	}
}

func mkPods(n int, mcpu, memMiB int64) []Pod {
	out := make([]Pod, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, mkPod(fmt.Sprintf("p%02d", i), mcpu, memMiB))
	}
	return out
}

func closeTo(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (±%g)", what, got, want, tol)
	}
}

// ---------------------------------------------------------------------------
// Golden scenario 1: sparse workloads win on Fargate.
// ---------------------------------------------------------------------------

// TestGoldenSparseWinsFargate is §4.3's sparse reading, computed rather than
// asserted. Arithmetic, by hand:
//
//	6 pods × (200 m, 512 Mi).
//	Fargate: effective request 200 m / 512 MiB; +256 MiB overhead = 768 MiB.
//	         Smallest tier that holds it: 0.25 vCPU / 1 GB.
//	         P(0.25, 1) = 0.25×0.04048 + 1×0.004445
//	                    = 0.010120 + 0.004445 = $0.014565/h.
//	         F = 6 × 0.014565                  = $0.087390/h  ($63.79/mo)
//	EC2:     demand 1200 m / 3 GiB. One m5.large: capacity 2000 m / 8 GiB,
//	         allocatable after the 8 % system reserve 1840 m / 7.36 GiB —
//	         all six pods fit, and the node is billed whole.
//	         E = 1 × $0.096/h                  = $0.096000/h  ($70.08/mo)
//	Fargate wins by 0.096 − 0.08739 = $0.00861/h = $6.2853/mo = 8.97 %.
func TestGoldenSparseWinsFargate(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)}, Options{
		Candidates: []pricing.InstanceType{m5large, m5xlarge},
	})

	if rep.Verdict != VerdictFargate {
		t.Fatalf("verdict = %q, want %q\n%s", rep.Verdict, VerdictFargate, rep.Summary())
	}
	closeTo(t, "F(P) hourly", rep.Fargate.HourlyUSD, 0.087390, 1e-9)
	closeTo(t, "F(P) monthly", rep.Fargate.MonthlyUSD, 63.7947, 1e-6)
	closeTo(t, "E(P) hourly", rep.EC2.HourlyUSD, 0.096, 1e-9)
	closeTo(t, "monthly savings", rep.MonthlySavingsUSD, 0.00861*730, 1e-6)
	closeTo(t, "savings fraction", rep.SavingsFraction, 0.00861/0.096, 1e-9)
	if rep.Close {
		t.Errorf("a 9 %% gap should not be marked close")
	}
	if rep.EC2.Nodes != 1 || rep.EC2.NodeTypes[0].Name != "m5.large" {
		t.Errorf("EC2 side = %d× %v, want 1× m5.large", rep.EC2.Nodes, rep.EC2.NodeTypes)
	}
	if len(rep.Fargate.Configs) != 1 ||
		rep.Fargate.Configs[0].Config != (pricing.FargateConfig{MilliCPU: 250, MemoryMiB: 1024}) ||
		rep.Fargate.Configs[0].Pods != 6 {
		t.Errorf("tier histogram = %+v, want 6 × 0.25 vCPU / 1 GB", rep.Fargate.Configs)
	}
	// Break-even: u = value(1200 m, 3 GiB)/value(2000 m, 8 GiB)
	//               = (1.2×0.033 + 3×0.0044)/(2×0.033 + 8×0.0044)
	//               = 0.0528/0.1012 = 0.521739
	//           u* = u × E/F = 0.521739 × 0.096/0.087390 = 0.573138
	// u < u* — below the break-even density, which is why Fargate wins.
	closeTo(t, "achieved density", rep.Density.Achieved, 0.0528/0.1012, 1e-9)
	closeTo(t, "break-even density", rep.Density.BreakEven, (0.0528/0.1012)*(0.096/0.087390), 1e-9)
	if !(rep.Density.Achieved < rep.Density.BreakEven) {
		t.Errorf("sparse pack should sit below the break-even density: u=%v u*=%v",
			rep.Density.Achieved, rep.Density.BreakEven)
	}
	closeTo(t, "system reserved fraction", rep.Density.SystemReservedFraction, 0.08, 1e-3)
}

// ---------------------------------------------------------------------------
// Golden scenario 2: dense workloads win on EC2.
// ---------------------------------------------------------------------------

// TestGoldenDenseWinsEC2 is §4.3's worked dense example, corrected for the
// quantizer. Arithmetic, by hand:
//
//	40 pods × (1 vCPU, 4 GiB).
//	Fargate: 4 GiB + 256 MiB = 4.25 GiB; the 1-vCPU row steps 2…8 GB by 1 GB,
//	         so the pod lands on 1 vCPU / 5 GB — a whole gigabyte bought for
//	         256 MiB of kubelet overhead.
//	         P(1, 5) = 0.04048 + 5×0.004445 = $0.062705/h
//	         F = 40 × 0.062705              = $2.508200/h
//	EC2:     m5.2xlarge allocatable = 7360 m / 29.44 GiB ⇒ 7 pods/node
//	         (memory binds: 8×4 = 32 GiB > 29.44).
//	         5 full m5.2xlarge = 35 pods; the 5 left are cheaper on
//	         m5.xlarge (3680 m / 14.72 GiB ⇒ 3 pods) — 3 + 2.
//	         E = 5×0.384 + 2×0.192 = 1.920 + 0.384 = $2.304000/h
//	EC2 wins by 2.5082 − 2.304 = $0.2042/h = $149.066/mo = 8.14 %.
func TestGoldenDenseWinsEC2(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(40, 1000, 4096)}, Options{
		Candidates: []pricing.InstanceType{m5xlarge, m52xlarge},
	})

	if rep.Verdict != VerdictEC2 {
		t.Fatalf("verdict = %q, want %q\n%s", rep.Verdict, VerdictEC2, rep.Summary())
	}
	closeTo(t, "F(P) hourly", rep.Fargate.HourlyUSD, 2.5082, 1e-9)
	closeTo(t, "E(P) hourly", rep.EC2.HourlyUSD, 2.304, 1e-9)
	closeTo(t, "monthly savings", rep.MonthlySavingsUSD, (2.5082-2.304)*730, 1e-6)
	closeTo(t, "savings fraction", rep.SavingsFraction, (2.5082-2.304)/2.5082, 1e-9)
	if rep.EC2.Nodes != 7 {
		t.Errorf("nodes = %d, want 7 (5× m5.2xlarge + 2× m5.xlarge)\n%s", rep.EC2.Nodes, rep.Summary())
	}
	// u = value(40 000 m, 160 GiB)/value(48 000 m, 192 GiB)
	//   = (40×0.033 + 160×0.0044)/(48×0.033 + 192×0.0044) = 2.024/2.4288 = 0.833333
	// u* = 0.833333 × 2.304/2.5082 = 0.765489 — above it, so the node set wins.
	closeTo(t, "achieved density", rep.Density.Achieved, 2.024/2.4288, 1e-9)
	closeTo(t, "break-even density", rep.Density.BreakEven, (2.024/2.4288)*(2.304/2.5082), 1e-9)
	if !(rep.Density.Achieved > rep.Density.BreakEven) {
		t.Errorf("dense pack should sit above the break-even density: u=%v u*=%v",
			rep.Density.Achieved, rep.Density.BreakEven)
	}
}

// TestBreakEvenReducesToTheScreeningRatio pins the claim Density's doc comment
// makes: u* is §4.3's screening ratio P_ec2_bundle / P_fargate_bundle, and the
// gap between the doc's 82.4 % and Kilter's 76.5 % is exactly the +256 MiB
// overhead the screening formula omits.
//
//	§4.3, no overhead: u* = 0.0480 / 0.05826 = 0.823893
//	                        (m5.xlarge $0.192 ÷ 4 bundles; P(1,4) Fargate)
//	with the overhead:  u* = 0.833333 × 2.304 / 2.5082 = 0.765489
func TestBreakEvenReducesToTheScreeningRatio(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(40, 1000, 4096)}, Options{
		Candidates: []pricing.InstanceType{m5xlarge, m52xlarge},
	})
	closeTo(t, "u* with the quantizer's overhead", rep.Density.BreakEven, 0.765489, 1e-6)

	// Recompute u* against the screening formula's overhead-free bundle price
	// to show the two agree once the overhead is removed.
	bundleFargate := 0.04048 + 4*0.004445 // P(1,4) = 0.058260
	screening := 0.048 / bundleFargate    // 0.823893
	noOverhead := rep.Density.Achieved * rep.EC2.HourlyUSD / (40 * bundleFargate)
	closeTo(t, "u* without the overhead", noOverhead, screening, 1e-6)
	if !(rep.Density.BreakEven < screening) {
		t.Errorf("the overhead must push the crossover down, not up: %v vs %v",
			rep.Density.BreakEven, screening)
	}

	// u/u* == F/E is the identity that makes the density framing exact rather
	// than decorative; it must not depend on the scalarizing exchange rate.
	closeTo(t, "u/u*", rep.Density.Achieved/rep.Density.BreakEven,
		rep.Fargate.HourlyUSD/rep.EC2.HourlyUSD, 1e-9)
}

// ---------------------------------------------------------------------------
// DaemonSet overhead.
// ---------------------------------------------------------------------------

// TestDaemonSetOverheadMovesTheAnswer asserts both halves of what a DaemonSet
// does, which point in opposite directions and are routinely confused.
//
// Arithmetic, by hand. 6 pods × (300 m, 512 Mi), candidates {m5.large}:
//
//	Fargate: 512 MiB + 256 MiB = 768 MiB, but 300 m needs the 0.5-vCPU row,
//	         whose smallest memory option is 1 GB ⇒ 0.5 vCPU / 1 GB.
//	         P(0.5, 1) = 0.5×0.04048 + 0.004445 = $0.024685/h
//	         F = 6 × 0.024685 = $0.148110/h — unchanged by DaemonSets, which
//	         cannot exist on Fargate at all.
//	EC2 without DaemonSets: allocatable 1840 m / 7.36 GiB, demand 1800 m ⇒
//	         all six fit on one node.  E = $0.096/h  → EC2 wins (35 % cheaper).
//	EC2 with one DaemonSet of (200 m, 256 Mi): 1840 − 200 = 1640 m of room,
//	         so only 5 pods fit per node and a second node must be bought.
//	         E = 2 × 0.096 = $0.192/h → now 30 % *more* than Fargate.
//
// Direction, stated: the DaemonSet moves the economics toward Fargate — and
// simultaneously makes Fargate impossible. The cheaper-looking option is the
// unavailable one, so the verdict goes to fargate-blocked, never to fargate.
func TestDaemonSetOverheadMovesTheAnswer(t *testing.T) {
	pods := mkPods(6, 300, 512)
	opts := Options{Candidates: []pricing.InstanceType{m5large}}

	bare := Analyze(testNow, PodSet{Pods: pods}, opts)
	ds := model.PodSpec{
		UID: "ds-0", Name: "node-agent", Namespace: "kube-system",
		Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "node-agent"},
		Containers: []model.ContainerSpec{{Name: "agent", Requests: model.Resources{MilliCPU: 200, MemoryBytes: 256 * mib}}},
	}
	with := Analyze(testNow, PodSet{Pods: pods, DaemonSets: []model.PodSpec{ds}}, opts)

	closeTo(t, "F(P) without DaemonSets", bare.Fargate.HourlyUSD, 0.148110, 1e-9)
	closeTo(t, "F(P) with DaemonSets", with.Fargate.HourlyUSD, 0.148110, 1e-9)
	closeTo(t, "E(P) without DaemonSets", bare.EC2.HourlyUSD, 0.096, 1e-9)
	closeTo(t, "E(P) with DaemonSets", with.EC2.HourlyUSD, 0.192, 1e-9)
	if bare.EC2.Nodes != 1 || with.EC2.Nodes != 2 {
		t.Fatalf("nodes: bare=%d with=%d, want 1 and 2", bare.EC2.Nodes, with.EC2.Nodes)
	}

	if bare.Verdict != VerdictEC2 {
		t.Errorf("without DaemonSets verdict = %q, want %q", bare.Verdict, VerdictEC2)
	}
	if with.Verdict != VerdictFargateBlocked {
		t.Errorf("with DaemonSets verdict = %q, want %q", with.Verdict, VerdictFargateBlocked)
	}
	// The economics moved toward Fargate even though the verdict cannot.
	if !(with.Fargate.HourlyUSD < with.EC2.HourlyUSD) {
		t.Errorf("DaemonSet overhead should make the node set the pricier side: F=%v E=%v",
			with.Fargate.HourlyUSD, with.EC2.HourlyUSD)
	}
	if with.MonthlySavingsUSD != 0 {
		t.Errorf("a blocked report must claim no savings, got %v", with.MonthlySavingsUSD)
	}

	// Achieved density halves (twice the capacity for the same demand)…
	closeTo(t, "u without DaemonSets", bare.Density.Achieved, 0.0726/0.1012, 1e-9)
	closeTo(t, "u with DaemonSets", with.Density.Achieved, 0.0726/0.2024, 1e-9)
	if !(with.Density.Achieved < bare.Density.Achieved) {
		t.Errorf("DaemonSet overhead must lower achieved density: %v → %v",
			bare.Density.Achieved, with.Density.Achieved)
	}
	// …while the break-even line itself does not move, because the winning
	// instance type — and therefore the price per unit of capacity — did not.
	// u* = value(W) × (E/value(C)) / F, and E/value(C) is a property of the
	// shape you buy. DaemonSets move you across the line; they do not move it.
	closeTo(t, "u* is unmoved when the winning shape is unchanged",
		with.Density.BreakEven, bare.Density.BreakEven, 1e-9)
}

// TestDaemonSetOverheadCanMoveTheBreakEvenLine is the other half: when the
// overhead changes *which* shape wins the pack, the price per unit of capacity
// changes and the break-even density moves with it.
//
// Arithmetic, by hand. 12 pods × (300 m, 512 Mi); DaemonSet (0 m, 2 GiB);
// candidates {c5.large $0.085 = 2 vCPU/4 GiB, m5.large $0.096 = 2 vCPU/8 GiB}:
//
//	Fargate: 12 × P(0.5, 1) = 12 × 0.024685 = $0.296220/h (unchanged).
//	No DaemonSet: c5.large allocatable 1840 m / 3.68 GiB fits 6 pods
//	  (CPU binds), m5.large also fits 6 (CPU binds) — c5.large packs the same
//	  six pods for less, so it wins. E = 2 × 0.085 = $0.170/h.
//	  u  = value(3600 m, 6 GiB)/value(2 × c5.large) = 0.1452/0.1672 = 0.868421
//	  u* = 0.868421 × 0.170/0.296220 = 0.498385
//	With the DaemonSet: c5.large has 1.68 GiB left ⇒ 3 pods; m5.large has
//	  5.36 GiB left ⇒ still 6. The pack switches to m5.large.
//	  E = 2 × 0.096 = $0.192/h.
//	  u  = 0.1452/0.2024 = 0.717391
//	  u* = 0.717391 × 0.192/0.296220 = 0.464989
//
// Direction, stated: the break-even density *falls* (0.49839 → 0.46499), because
// the shape the overhead forced you onto is cheaper per unit of capacity —
// m5.large at $0.94862 per capacity-dollar against c5.large's $1.01675. A
// cheaper shape needs less density to beat Fargate.
func TestDaemonSetOverheadCanMoveTheBreakEvenLine(t *testing.T) {
	pods := mkPods(12, 300, 512)
	opts := Options{Candidates: []pricing.InstanceType{c5large, m5large}}
	bare := Analyze(testNow, PodSet{Pods: pods}, opts)
	fat := model.PodSpec{
		UID: "ds-mem", Name: "mem-agent", Namespace: "kube-system",
		Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "mem-agent"},
		Containers: []model.ContainerSpec{{Name: "agent", Requests: model.Resources{MemoryBytes: 2 << 30}}},
	}
	with := Analyze(testNow, PodSet{Pods: pods, DaemonSets: []model.PodSpec{fat}}, opts)

	if bare.EC2.NodeTypes[0].Name != "c5.large" || bare.EC2.Nodes != 2 {
		t.Fatalf("without the DaemonSet the pack should be 2× c5.large, got %d× %+v",
			bare.EC2.Nodes, bare.EC2.NodeTypes)
	}
	if with.EC2.NodeTypes[0].Name != "m5.large" || with.EC2.Nodes != 2 {
		t.Fatalf("with the DaemonSet the pack should switch to 2× m5.large, got %d× %+v",
			with.EC2.Nodes, with.EC2.NodeTypes)
	}
	closeTo(t, "u* without the DaemonSet", bare.Density.BreakEven, 0.498385, 1e-5)
	closeTo(t, "u* with the DaemonSet", with.Density.BreakEven, 0.464989, 1e-5)
	if !(with.Density.BreakEven < bare.Density.BreakEven) {
		t.Errorf("switching to a cheaper-per-capacity shape must lower the break-even density: %v → %v",
			bare.Density.BreakEven, with.Density.BreakEven)
	}
}

// ---------------------------------------------------------------------------
// Determinism.
// ---------------------------------------------------------------------------

// TestReportIsShuffleInvariant pins the determinism rule: the report is a pure
// function of the pod *set*, so no map iteration order and no input order can
// change a byte of it.
func TestReportIsShuffleInvariant(t *testing.T) {
	base := mkPods(23, 300, 700)
	for i := range base {
		base[i].Spec.Namespace = fmt.Sprintf("ns%d", i%4)
		// Trip a couple of gates so block aggregation, pod-list sorting and
		// the "… and N more" truncation are shuffled too.
		if i%3 == 0 {
			base[i].Facts.EBSVolume = Present
		}
		if i%7 == 0 {
			base[i].Facts.Privileged = Unknown
		}
	}
	ds := []model.PodSpec{
		{UID: "ds-a", Name: "a", Namespace: "kube-system",
			Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "a"},
			Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: 50, MemoryBytes: 64 * mib}}}},
		{UID: "ds-b", Name: "b", Namespace: "kube-system",
			Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "b"},
			Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: 30, MemoryBytes: 32 * mib}}}},
	}
	opts := Options{Candidates: []pricing.InstanceType{m5large, m5xlarge, m52xlarge, c5large, m7g2xlarge}}
	want := Analyze(testNow, PodSet{Pods: base, DaemonSets: ds}, opts)
	wantText := want.Summary()

	rng := rand.New(rand.NewSource(20260826))
	for trial := 0; trial < 25; trial++ {
		pods := append([]Pod(nil), base...)
		rng.Shuffle(len(pods), func(i, j int) { pods[i], pods[j] = pods[j], pods[i] })
		dsShuf := append([]model.PodSpec(nil), ds...)
		rng.Shuffle(len(dsShuf), func(i, j int) { dsShuf[i], dsShuf[j] = dsShuf[j], dsShuf[i] })
		got := Analyze(testNow, PodSet{Pods: pods, DaemonSets: dsShuf}, opts)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: shuffling the input changed the report\n got: %s\nwant: %s",
				trial, got.Summary(), wantText)
		}
	}
}

// TestShuffleInvariantWithUnnamedPods covers the degenerate identity case:
// pods carrying no UID still produce a stable report, because canonicalization
// orders by (UID, namespace, name, requests) before synthesizing identities.
func TestShuffleInvariantWithUnnamedPods(t *testing.T) {
	var base []Pod
	for i := 0; i < 9; i++ {
		p := mkPod("", int64(100+10*i), 256)
		p.Spec.Name = fmt.Sprintf("n%d", i)
		base = append(base, p)
	}
	opts := Options{Candidates: []pricing.InstanceType{m5large}}
	want := Analyze(testNow, PodSet{Pods: base}, opts)
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 10; trial++ {
		pods := append([]Pod(nil), base...)
		rng.Shuffle(len(pods), func(i, j int) { pods[i], pods[j] = pods[j], pods[i] })
		if got := Analyze(testNow, PodSet{Pods: pods}, opts); !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: unnamed pods produced an order-dependent report", trial)
		}
	}
}

// ---------------------------------------------------------------------------
// Degenerate inputs.
// ---------------------------------------------------------------------------

func TestDegenerateInputs(t *testing.T) {
	huge := mkPod("huge", 32000, 200<<10) // 32 vCPU / 200 GiB: above every Fargate tier
	// Fits every Fargate tier, fits no m5.large: 8 vCPU / 16 GiB.
	unpackable := mkPod("unpackable", 8000, 16<<10)
	neg := mkPod("neg", -500, -1024)
	sat := mkPod("sat", math.MaxInt64, math.MaxInt64)
	terminal := mkPod("done", 200, 512)
	terminal.Spec.Phase = "Succeeded"
	dupA, dupB := mkPod("same", 200, 512), mkPod("same", 4000, 8192)

	cases := []struct {
		name    string
		set     PodSet
		opts    Options
		verdict Verdict
		check   func(*testing.T, Report)
	}{
		{
			name: "empty pod set", set: PodSet{}, verdict: VerdictUndecided,
			check: func(t *testing.T, r Report) {
				if r.Fargate.Pods != 0 || r.EC2.Nodes != 0 || r.Density.Defined {
					t.Errorf("empty set produced content: %+v", r)
				}
				if r.MonthlySavingsUSD != 0 {
					t.Errorf("empty set claimed savings %v", r.MonthlySavingsUSD)
				}
			},
		},
		{
			name: "nil pods slice", set: PodSet{Pods: nil}, verdict: VerdictUndecided,
		},
		{
			name: "one pod above every Fargate tier and every node",
			set:  PodSet{Pods: []Pod{huge}}, verdict: VerdictUndecided,
			check: func(t *testing.T, r Report) {
				if len(r.Fargate.Unpriced) != 1 {
					t.Errorf("huge pod should be unpriced on Fargate: %+v", r.Fargate)
				}
				if !hasGate(r.Blocks, GateSizeCeiling) {
					t.Errorf("huge pod should raise the size-ceiling gate: %+v", r.Blocks)
				}
				if r.EC2.Feasible {
					t.Errorf("huge pod cannot be packed onto m5.large")
				}
			},
		},
		{
			name: "one huge pod that Fargate can hold but no node can",
			// 8 vCPU / 16 GiB is a valid Fargate tier and fits no candidate here.
			set: PodSet{Pods: []Pod{mkPod("big", 8000, 16<<10)}}, verdict: VerdictFargate,
			check: func(t *testing.T, r Report) {
				if r.MonthlySavingsUSD != 0 {
					t.Errorf("a feasibility-only win must claim no savings, got %v", r.MonthlySavingsUSD)
				}
				if !hasWarning(r, "feasibility") {
					t.Errorf("a feasibility-only win must say so: %v", r.Warnings)
				}
			},
		},
		{
			name: "one pod in the set fits no instance type",
			set:  PodSet{Pods: append(mkPods(3, 200, 512), unpackable)}, verdict: VerdictFargate,
			check: func(t *testing.T, r Report) {
				if len(r.EC2.Unschedulable) != 1 || !strings.Contains(r.EC2.Unschedulable[0], "unpackable") {
					t.Errorf("the pod that fits nowhere must be named: %v", r.EC2.Unschedulable)
				}
				if r.EC2.Feasible {
					t.Errorf("a partial pack is not feasible")
				}
				if r.MonthlySavingsUSD != 0 {
					t.Errorf("no savings may be claimed against a partial pack, got %v", r.MonthlySavingsUSD)
				}
			},
		},
		{
			name: "no candidate instance types at all",
			set:  PodSet{Pods: mkPods(3, 200, 512)},
			opts: Options{Candidates: []pricing.InstanceType{}}, verdict: VerdictFargate,
		},
		{
			name: "catalog has no instances for the requested provider",
			set:  PodSet{Pods: mkPods(3, 200, 512)},
			opts: Options{Provider: "nowhere"}, verdict: VerdictFargate,
			check: func(t *testing.T, r Report) {
				if !hasWarning(r, "no nowhere instance types") {
					t.Errorf("an empty candidate set must be reported: %v", r.Warnings)
				}
			},
		},
		{
			name: "negative requests clamp rather than mint capacity",
			set:  PodSet{Pods: []Pod{neg}}, verdict: VerdictFargate,
			check: func(t *testing.T, r Report) {
				// Clamped to zero, the pod lands on the smallest tier.
				if got := r.Fargate.Configs[0].Config; got != pricing.FargateMinConfig {
					t.Errorf("negative requests → %v, want the smallest tier", got)
				}
			},
		},
		{
			name: "saturating requests are rejected, not wrapped",
			set:  PodSet{Pods: []Pod{sat}}, verdict: VerdictUndecided,
			check: func(t *testing.T, r Report) {
				if len(r.Fargate.Unpriced) != 1 || r.Fargate.HourlyUSD != 0 {
					t.Errorf("a MaxInt64 pod must be unpriced, got %+v", r.Fargate)
				}
			},
		},
		{
			name: "only terminal pods", set: PodSet{Pods: []Pod{terminal}}, verdict: VerdictUndecided,
			check: func(t *testing.T, r Report) {
				if !hasWarning(r, "terminal pod") {
					t.Errorf("terminal exclusions must be reported: %v", r.Warnings)
				}
			},
		},
		{
			name: "duplicate UIDs are dropped once, deterministically",
			set:  PodSet{Pods: []Pod{dupA, dupB}}, verdict: VerdictFargate,
			check: func(t *testing.T, r Report) {
				if r.Fargate.Pods != 1 {
					t.Errorf("duplicate UID should leave one pod, got %d", r.Fargate.Pods)
				}
				if !hasWarning(r, "duplicate UID") {
					t.Errorf("dropping a pod must be reported: %v", r.Warnings)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts.Candidates == nil && opts.Provider == "" {
				opts.Candidates = []pricing.InstanceType{m5large}
			}
			rep := Analyze(testNow, tc.set, opts)
			if rep.Verdict != tc.verdict {
				t.Errorf("verdict = %q, want %q\n%s", rep.Verdict, tc.verdict, rep.Summary())
			}
			if rep.At != testNow {
				t.Errorf("report clock = %v, want the caller's %v", rep.At, testNow)
			}
			if rep.Verdict == VerdictFargate && len(rep.Blocks) > 0 {
				t.Fatalf("a blocked report recommended Fargate: %+v", rep.Blocks)
			}
			// Summary must render without panicking on any of these.
			if s := rep.Summary(); s == "" {
				t.Errorf("empty summary")
			}
			if tc.check != nil {
				tc.check(t, rep)
			}
		})
	}
}

// TestNilCatalogUsesEmbedded checks the zero-Options path end to end.
func TestNilCatalogUsesEmbedded(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)}, Options{})
	if rep.Fargate.HourlyUSD <= 0 || rep.EC2.HourlyUSD <= 0 {
		t.Fatalf("zero Options should still price both sides: %+v", rep)
	}
	if rep.EC2.NodeTypes[0].Provider != DefaultProvider {
		t.Errorf("default provider = %q, want %q", rep.EC2.NodeTypes[0].Provider, DefaultProvider)
	}
}

// TestTieIsExactNotApproximate: a tie means the two bills are equal, not close.
func TestTieIsExactNotApproximate(t *testing.T) {
	pods := mkPods(6, 200, 512) // F = $0.087390/h
	tie := it("tie.node", 4000, 16, 0.087390, "")
	rep := Analyze(testNow, PodSet{Pods: pods}, Options{Candidates: []pricing.InstanceType{tie}})
	if rep.Verdict != VerdictTie {
		t.Fatalf("verdict = %q, want %q (F=%v E=%v)", rep.Verdict, VerdictTie,
			rep.Fargate.HourlyUSD, rep.EC2.HourlyUSD)
	}
	near := it("near.node", 4000, 16, 0.0874, "") // 0.0001 dearer: a win, not a tie
	rep = Analyze(testNow, PodSet{Pods: pods}, Options{Candidates: []pricing.InstanceType{near}})
	if rep.Verdict != VerdictFargate || !rep.Close {
		t.Fatalf("a 0.01 %% gap should be a close Fargate win, got %q close=%v", rep.Verdict, rep.Close)
	}
}

func hasGate(blocks []Block, g Gate) bool {
	for _, b := range blocks {
		if b.Gate == g {
			return true
		}
	}
	return false
}

func hasWarning(r Report, substr string) bool {
	for _, w := range r.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
