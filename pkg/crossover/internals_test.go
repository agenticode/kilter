package crossover

import (
	"math"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

func TestPodKeyFallbacks(t *testing.T) {
	cases := []struct {
		spec model.PodSpec
		want string
	}{
		{model.PodSpec{Namespace: "ns", Name: "n"}, "ns/n"},
		{model.PodSpec{Name: "n"}, "/n"},
		{model.PodSpec{UID: "u"}, "u"},
		{model.PodSpec{}, "<unnamed pod>"},
	}
	for _, tc := range cases {
		if got := podKey(&tc.spec); got != tc.want {
			t.Errorf("podKey(%+v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// TestMoneyIsNeverComparedWithEquals covers the comparison convention: an
// absolute floor for near-zero amounts, a relative band for real ones, and NaN
// never equal to anything (including itself).
func TestMoneyIsNeverComparedWithEquals(t *testing.T) {
	cases := []struct {
		a, b float64
		want bool
	}{
		{0, 0, true},
		{1e-12, 0, true},                  // below the absolute floor
		{0.096, 0.096, true},              // identical
		{0.096, 0.096 + 1e-11, true},      // within the relative band
		{0.096, 0.0961, false},            // a real difference
		{1e9, 1e9 + 0.5, true},            // relative band: 1e-9 of $1e9/h is $1
		{1e9, 1e9 + 2, false},             // …but $2 on $1e9/h is a real difference
		{math.NaN(), math.NaN(), false},   // NaN is never equal
		{math.NaN(), 1, false},            //
		{math.Inf(1), math.Inf(1), false}, // Inf-Inf is NaN, so: not equal
	}
	for _, tc := range cases {
		if got := moneyEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("moneyEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestLessPodOrdersByEveryKey(t *testing.T) {
	mk := func(uid, ns, name string, mcpu, mem int64) model.PodSpec {
		return model.PodSpec{UID: uid, Namespace: ns, Name: name,
			Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: mcpu, MemoryBytes: mem}}}}
	}
	cases := []struct {
		name string
		a, b model.PodSpec
	}{
		{"uid", mk("a", "z", "z", 9, 9), mk("b", "a", "a", 1, 1)},
		{"namespace", mk("x", "a", "z", 9, 9), mk("x", "b", "a", 1, 1)},
		{"name", mk("x", "n", "a", 9, 9), mk("x", "n", "b", 1, 1)},
		{"cpu", mk("x", "n", "p", 1, 9), mk("x", "n", "p", 2, 1)},
		{"memory", mk("x", "n", "p", 1, 1), mk("x", "n", "p", 1, 2)},
	}
	for _, tc := range cases {
		if !lessPod(tc.a, tc.b) || lessPod(tc.b, tc.a) {
			t.Errorf("%s: ordering is not strict", tc.name)
		}
	}
	same := mk("x", "n", "p", 1, 1)
	if lessPod(same, same) {
		t.Errorf("identical pods must not order before themselves")
	}
}

func TestCanonicalDaemonSetsEdgeCases(t *testing.T) {
	done := model.PodSpec{UID: "t", Phase: "Failed",
		Workload: model.WorkloadRef{Kind: model.KindDaemonSet, Name: "gone"}}
	bare := model.PodSpec{UID: "b1"} // no owning controller at all
	bare2 := model.PodSpec{UID: "b2"}
	out, notes := canonicalDaemonSets([]model.PodSpec{done, bare, bare2})
	if len(out) != 2 {
		t.Fatalf("templates = %d, want both unowned pods kept (they cannot be deduped by workload)", len(out))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "terminal DaemonSet") {
		t.Errorf("notes = %v, want the terminal exclusion reported", notes)
	}
	if out[0].UID != "b1" || out[1].UID != "b2" {
		t.Errorf("templates are not in canonical order: %v %v", out[0].UID, out[1].UID)
	}
}

// TestDecideOrdersBlocksBeforePrices attacks decide() directly with the state
// Analyze cannot build: a block standing beside an "eligible", far cheaper
// Fargate side. The switch must still decide on the block, because it is
// ordered to — that ordering is the only thing standing between a $9/h price
// gap and a recommendation that would break the cluster.
func TestDecideOrdersBlocksBeforePrices(t *testing.T) {
	rep := Report{
		Blocks:  []Block{{Gate: GateEBSVolume, Kind: BlockViolation, Reason: "x"}},
		Fargate: FargateSide{Eligible: true, HourlyUSD: 1, Pods: 1},
		EC2:     EC2Side{Feasible: true, HourlyUSD: 10, Nodes: 1},
	}
	decide(&rep, 1)
	if rep.Verdict != VerdictFargateBlocked {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictFargateBlocked)
	}
	if rep.MonthlySavingsUSD != 0 || rep.SavingsFraction != 0 || rep.Close {
		t.Errorf("blocked report claimed savings %v (%v, close=%v)",
			rep.MonthlySavingsUSD, rep.SavingsFraction, rep.Close)
	}

	// And with no EC2 side to fall back to, the answer is "undecided", still
	// never "fargate".
	rep = Report{
		Blocks:  []Block{{Gate: GateEBSVolume, Kind: BlockViolation, Reason: "x"}},
		Fargate: FargateSide{Eligible: true, HourlyUSD: 1, Pods: 1},
	}
	decide(&rep, 1)
	if rep.Verdict != VerdictUndecided {
		t.Fatalf("verdict = %q, want %q", rep.Verdict, VerdictUndecided)
	}
	if !hasWarning(rep, "neither side can run this pod set") {
		t.Errorf("a doubly-impossible pod set must say so: %v", rep.Warnings)
	}
}

func TestTieHeadlineAndCloseNote(t *testing.T) {
	rep := Report{Verdict: VerdictTie, Fargate: FargateSide{Pods: 3, MonthlyUSD: 10}, EC2: EC2Side{MonthlyUSD: 10}}
	if h := rep.Headline(); !strings.Contains(h, "cost the same") {
		t.Errorf("tie headline = %q", h)
	}
	rep.Verdict, rep.Close = VerdictFargate, true
	if h := rep.Headline(); !strings.Contains(h, "error bars") {
		t.Errorf("close win must be flagged: %q", h)
	}
	if s := rep.Summary(); !strings.Contains(s, "error bars") {
		t.Errorf("summary must carry the close-call note")
	}
}

func TestNonZeroGuardsTheDivision(t *testing.T) {
	for _, v := range []float64{0, -1} {
		if nonZero(v) != 1 {
			t.Errorf("nonZero(%v) must guard the division", v)
		}
	}
	if nonZero(0.5) != 0.5 {
		t.Errorf("nonZero must pass positive values through")
	}
}

// TestDensityUndefinedWhenASideIsUnpriceable: a break-even is not invented out
// of a zero.
func TestDensityUndefinedWhenASideIsUnpriceable(t *testing.T) {
	if d := density(EC2Side{}, FargateSide{HourlyUSD: 1}); d.Defined {
		t.Errorf("no nodes ⇒ no density, got %+v", d)
	}
	d := density(EC2Side{Nodes: 1, HourlyUSD: 1,
		Purchased:   model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30},
		Allocatable: model.Resources{MilliCPU: 920, MemoryBytes: 1 << 30}},
		FargateSide{HourlyUSD: 0})
	if !d.Defined || d.BreakEven != 0 {
		t.Errorf("an unpriced Fargate side leaves the break-even at zero, got %+v", d)
	}
}
