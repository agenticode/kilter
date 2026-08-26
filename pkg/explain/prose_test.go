package explain

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/evidence"
)

// Template prose is the only thing a no-model deployment renders (§5.9) and
// the only wording a narrating model is allowed to quote, so every branch of
// it is pinned rather than left to whichever fixture happens to reach it.

func TestFormatters(t *testing.T) {
	t.Run("usd", func(t *testing.T) {
		cases := []struct {
			in   float64
			want string
		}{
			{0, "$0.000000"},
			{math.Copysign(0, -1), "$0.000000"}, // negative zero must not render as -$0
			{0.1, "$0.100000"},
			{-2.5, "$-2.500000"},
			{math.NaN(), "n/a"},
			{math.Inf(1), "n/a"},
		}
		for _, tc := range cases {
			if got := formatUSD(tc.in); got != tc.want {
				t.Errorf("formatUSD(%v) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})
	t.Run("signed", func(t *testing.T) {
		if got := signedUSD(-1_500_000); got != "-$1.500000/h" {
			t.Errorf("signedUSD = %q", got)
		}
		if got := signedUSD(0); got != "+$0.000000/h" {
			t.Errorf("signedUSD(0) = %q", got)
		}
		if got := signedMonthly(-1_000_000); got != "-$730.00/mo" {
			t.Errorf("signedMonthly = %q", got)
		}
	})
	t.Run("ratio", func(t *testing.T) {
		for in, want := range map[float64]string{0: "0.0%", 0.125: "12.5%", 1: "100.0%"} {
			if got := formatRatio(in); got != want {
				t.Errorf("formatRatio(%v) = %q, want %q", in, got, want)
			}
		}
		if got := formatRatio(math.NaN()); got != "n/a" {
			t.Errorf("formatRatio(NaN) = %q", got)
		}
	})
}

func TestLabelsCoverEveryDirection(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"nodes up", nodeCountLabel(3, 100_000), "3 more nodes"},
		{"nodes down", nodeCountLabel(-1, 100_000), "1 fewer node"},
		{"nodes flat", nodeCountLabel(0, 100_000), "node count unchanged"},
		{"spot cheaper", spotLabel(-1), "larger share"},
		{"spot dearer", spotLabel(1), "smaller share"},
		{"spot flat", spotLabel(0), "did not move the bill"},
		{"mix cheaper", mixLabel(-1), "cheaper instance types"},
		{"mix dearer", mixLabel(1), "more expensive instance types"},
		{"mix flat", mixLabel(0), "did not move the bill"},
		{"catalog none", catalogLabel(0, 0), "no catalog price changed"},
		{"catalog up", catalogLabel(1, 2), "rose for 2 node groups"},
		{"catalog down", catalogLabel(-1, 1), "fell for 1 node group"},
		{"catalog net zero", catalogLabel(0, 3), "moved, netting out"},
		{"kilter removed", kilterActionLabel(-2, 1), "removed 2 nodes across 1 applied plan"},
		{"kilter added", kilterActionLabel(1, 2), "added 1 node across 2 applied plans"},
		{"kilter neutral", kilterActionLabel(0, 3), "changed no node count"},
		{"scaling up", workloadScalingLabel(1), "requested more capacity"},
		{"scaling down", workloadScalingLabel(-1), "requested less capacity"},
		{"scaling flat", workloadScalingLabel(0), "requested the same capacity"},
		{"unattributed zero", unattributedLabel(0), "every node added or removed is accounted for"},
		{"unattributed nonzero", unattributedLabel(5), "no supplied driver behind it"},
		{"residual zero", residualLabel(0), "nothing unexplained"},
		{"residual nonzero", residualLabel(-5), "unexplained"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.got, tc.want) {
				t.Errorf("label %q does not contain %q", tc.got, tc.want)
			}
		})
	}
}

func TestWorkloadSetLabel(t *testing.T) {
	a := []NamespaceDemand{{Namespace: "keep"}, {Namespace: "gone"}}
	b := []NamespaceDemand{{Namespace: "keep"}, {Namespace: "new"}}
	cases := []struct {
		name     string
		from, to []NamespaceDemand
		want     string
	}{
		{"both", a, b, "1 new and 1 departed namespaces"},
		{"added only", a[:1], b, "1 new namespace"},
		{"removed only", a, a[:1], "1 departed namespace"},
		{"unchanged", a, a, "did not change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workloadSetLabel(0, tc.from, tc.to); !strings.Contains(got, tc.want) {
				t.Errorf("workloadSetLabel = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestProseOrdersByMagnitudeThenChain pins the tie-break: equal-magnitude
// terms fall back to the documented chain order, so two zero terms never
// swap places between runs.
func TestProseOrdersByMagnitudeThenChain(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.0, 10), point(to.Add(-time.Minute), 1.0, 10)},
		Start:    &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
		End:      &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
	}
	a := mustWhy(t, in)
	for _, term := range a.Terms {
		if term.Micro != 0 {
			t.Fatalf("fixture is not all-zero: %s = %d", term.Kind, term.Micro)
		}
	}
	lines := strings.Split(strings.TrimSpace(a.Prose()), "\n")
	var order []string
	for _, l := range lines[2:] {
		f := strings.Fields(l)
		if len(f) > 0 && chainIndex(f[0]) < len(chainOrder) {
			order = append(order, f[0])
		}
	}
	if !equalStrings(order, chainOrder) {
		t.Errorf("all-zero terms rendered as %v, want chain order %v", order, chainOrder)
	}
	if chainIndex(TermResidual) != len(chainOrder) {
		t.Error("chainIndex must sort unknown kinds last")
	}
}

func TestNamespaceOf(t *testing.T) {
	cases := []struct {
		name string
		ev   evidence.EvidenceEvent
		want string
	}{
		{"explicit attr wins", ev(t0, evidence.EventDeploy, evidence.SeverityInfo,
			workloadSubject("parsed", "app"), map[string]string{"namespace": "declared"}), "declared"},
		{"workload key", ev(t0, evidence.EventDeploy, evidence.SeverityInfo, workloadSubject("parsed", "app"), nil), "parsed"},
		{"container key", ev(t0, evidence.EventOOMKill, evidence.SeverityCritical,
			evidence.ContainerSubject(testCluster, containerKey("cns", "app", "c")), nil), "cns"},
		{"cluster subject", ev(t0, evidence.EventPricingChange, evidence.SeverityInfo,
			evidence.ClusterSubject(testCluster), nil), ""},
		{"node subject", ev(t0, evidence.EventNodePressure, evidence.SeverityWarning,
			evidence.NodeSubject(testCluster, "n1"), nil), ""},
		{"malformed workload key", evidence.EvidenceEvent{At: t0, Kind: evidence.EventDeploy,
			Subject: evidence.SubjectRef{Cluster: testCluster, Kind: evidence.SubjectWorkload, Key: "flat"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespaceOf(tc.ev); got != tc.want {
				t.Errorf("namespaceOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestJoinCapped(t *testing.T) {
	items := []string{"a", "b", "c", "d"}
	if got := joinCapped(items, 4); got != "a, b, c, d" {
		t.Errorf("joinCapped = %q", got)
	}
	if got := joinCapped(items, 2); got != "a, b (+2 more)" {
		t.Errorf("joinCapped = %q", got)
	}
}

func TestRelMoveFloorsAtOne(t *testing.T) {
	// A dimension that starts at zero must not divide by zero; it is treated
	// as starting at one unit so the relative move stays finite and the
	// dominant-dimension choice stays deterministic.
	if got := relMove(0, 100); got != 100 {
		t.Errorf("relMove(0,100) = %v, want 100", got)
	}
	if got := relMove(200, 100); got != 0.5 {
		t.Errorf("relMove(200,100) = %v, want 0.5", got)
	}
	if got := relMove(0, 0); got != 0 {
		t.Errorf("relMove(0,0) = %v, want 0", got)
	}
}

func TestRefusalCitationsPickTheRightPool(t *testing.T) {
	digests := []ID{"dig/1/c/k/s@1"}
	changes := []ID{"evt/c/workload/w/deploy@1"}
	ooms := []ID{"evt/c/container/k/oomkill@1"}
	throttles := []ID{"evt/c/container/k/throttle-high@1"}
	decisions := []ID{"dec/c/container/k@1"}
	cases := []struct {
		code decision.RefusalCode
		want ID
	}{
		{decision.CodeInsufficientHistory, digests[0]},
		{decision.CodePostChangeSoak, changes[0]},
		{decision.CodeRegimeChangePending, changes[0]},
		{decision.CodeClassUnstable, changes[0]},
		{decision.CodeSignalConflict, ooms[0]},
		{decision.CodeQuarantined, decisions[0]},
		{decision.CodeSLADegraded, decisions[0]},
		{decision.RefusalCode("something-new"), digests[0]},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			got := refusalCitations(tc.code, digests, changes, ooms, throttles, decisions)
			if len(got) == 0 || got[0] != tc.want {
				t.Errorf("refusalCitations(%s) = %v, want it to lead with %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestConfidenceCitationsPickTheRightPool(t *testing.T) {
	digests := []ID{"dig/1/c/k/s@1"}
	changes := []ID{"evt/c/workload/w/deploy@1"}
	decisions := []ID{"dec/c/container/k@1"}
	cases := map[string]ID{
		"post-change-soak": changes[0],
		"class-stability":  changes[0],
		"signal-agreement": decisions[0],
		"history-depth":    digests[0],
		"volatility":       digests[0],
	}
	for name, want := range cases {
		got := confidenceCitations(name, digests, changes, decisions)
		if len(got) == 0 || got[0] != want {
			t.Errorf("confidenceCitations(%q) = %v, want it to lead with %q", name, got, want)
		}
	}
}

func TestParentWorkload(t *testing.T) {
	c := evidence.ContainerSubject(testCluster, containerKey("ns", "app", "web"))
	got, ok := parentWorkload(c)
	if !ok {
		t.Fatal("container subject has no parent")
	}
	if got.Kind != evidence.SubjectWorkload || got.Key != "Deployment/ns/app" || got.Cluster != testCluster {
		t.Errorf("parentWorkload = %+v", got)
	}
	for _, s := range []evidence.SubjectRef{
		evidence.ClusterSubject(testCluster),
		evidence.NodeSubject(testCluster, "n1"),
		{Cluster: testCluster, Kind: evidence.SubjectContainer, Key: "flat"},
		{Cluster: testCluster, Kind: evidence.SubjectContainer, Key: "/leading"},
	} {
		if _, ok := parentWorkload(s); ok {
			t.Errorf("parentWorkload(%+v) claimed a parent", s)
		}
	}
}

func TestMergeEventsIsBoundedAndOrdered(t *testing.T) {
	own := []evidence.EvidenceEvent{
		ev(t0.Add(2*time.Hour), evidence.EventOOMKill, evidence.SeverityCritical,
			evidence.ContainerSubject(testCluster, containerKey("ns", "app", "web")), nil),
	}
	var parent []evidence.EvidenceEvent
	for i := 0; i < 10; i++ {
		parent = append(parent, ev(t0.Add(time.Duration(i)*time.Hour), evidence.EventDeploy,
			evidence.SeverityInfo, workloadSubject("ns", "app"), map[string]string{"n": string(rune('a' + i))}))
	}
	got := mergeEvents(own, parent, 3)
	if len(got) != 4 {
		t.Fatalf("merged %d events, want 1 own + 3 parent", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.After(got[i-1].At) {
			t.Fatalf("merged events are not newest-first: %v then %v", got[i-1].At, got[i].At)
		}
	}
	if merged := mergeEvents(own, nil, 3); len(merged) != 1 {
		t.Errorf("merging no parent events changed the list: %d", len(merged))
	}
}

// TestRealizedDeltaNeedsBracketingObservations: an action with no timeline
// point on both sides of it produces no realized figure rather than a
// half-measured one.
func TestRealizedDeltaNeedsBracketingObservations(t *testing.T) {
	in := baseInput()
	in.Actions[0].At = t0.Add(hours(167))
	in.Actions[0].Finished = t0.Add(hours(167) + time.Hour)
	a := mustWhy(t, in)
	kilter := mustTerm(t, a, TermKilterAction)
	if got := factValue(kilter.Facts, "observedAcrossActionWindowsUSDPerHour"); got != "" {
		t.Errorf("an unbracketed action reported a realized move of %s", got)
	}
	// The attributed value is unaffected: it never depended on the timeline.
	if kilter.Micro != -200_000 {
		t.Errorf("kilter-action = %d µUSD/h, want -200000", kilter.Micro)
	}
}

func TestZeroFinishedTimeFallsBackToStart(t *testing.T) {
	in := baseInput()
	in.Actions[0].Finished = time.Time{}
	a := mustWhy(t, in)
	kilter := mustTerm(t, a, TermKilterAction)
	if got := factValue(kilter.Facts, "observedAcrossActionWindowsUSDPerHour"); got == "" {
		t.Error("an action with no finish time must still be measurable from its start")
	}
}
