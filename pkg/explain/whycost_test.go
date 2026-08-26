package explain

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// mustWhy runs WhyCost and fails the test on error.
func mustWhy(t *testing.T, in Input) *Attribution {
	t.Helper()
	a, err := WhyCost(in)
	if err != nil {
		t.Fatalf("WhyCost: %v", err)
	}
	return a
}

// checkSums is the package's central invariant, asserted on every attribution
// any test in this file produces: the terms plus the residual reconstruct
// ΔCost exactly, and every sub-attribution reconstructs its parent exactly.
func checkSums(t *testing.T, a *Attribution) {
	t.Helper()
	got, err := a.Sum()
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got != a.DeltaMicro {
		t.Errorf("sum(terms)+residual = %d µUSD/h, want ΔCost = %d µUSD/h (off by %d)",
			got, a.DeltaMicro, got-a.DeltaMicro)
	}
	for _, term := range a.Terms {
		if len(term.Of) == 0 {
			continue
		}
		var sum Micro
		for _, s := range term.Of {
			sum += s.Micro
		}
		if sum != term.Micro {
			t.Errorf("sub-terms of %q sum to %d µUSD/h, parent is %d", term.Kind, sum, term.Micro)
		}
	}
}

// checkCitable asserts the rule that makes this package safe to narrate from:
// no term, sub-term or residual is emitted without at least one evidence id.
func checkCitable(t *testing.T, a *Attribution) {
	t.Helper()
	for _, term := range append(append([]Term(nil), a.Terms...), a.Residual) {
		if len(term.Evidence) == 0 {
			t.Errorf("term %q carries no evidence id", term.Kind)
		}
		for _, s := range term.Of {
			if len(s.Evidence) == 0 {
				t.Errorf("sub-term %q of %q carries no evidence id", s.Kind, term.Kind)
			}
		}
	}
}

func termByKind(a *Attribution, kind string) (Term, bool) {
	for _, term := range a.Terms {
		if term.Kind == kind {
			return term, true
		}
		for _, s := range term.Of {
			if s.Kind == kind {
				return s, true
			}
		}
	}
	return Term{}, false
}

func mustTerm(t *testing.T, a *Attribution, kind string) Term {
	t.Helper()
	term, ok := termByKind(a, kind)
	if !ok {
		t.Fatalf("no %q term in attribution", kind)
	}
	return term
}

// TestWhyCostWorkedExample pins the arithmetic the package doc walks through.
// Every number below is derivable by hand from baseInput, which is the point:
// a decomposition nobody can check by hand is not an audit artifact.
func TestWhyCostWorkedExample(t *testing.T) {
	a := mustWhy(t, baseInput())
	checkSums(t, a)
	checkCitable(t, a)

	if a.FromMicro != 1_000_000 || a.ToMicro != 1_120_000 {
		t.Fatalf("endpoints: from=%d to=%d µUSD/h, want 1000000 and 1120000", a.FromMicro, a.ToMicro)
	}
	if a.DeltaMicro != 120_000 {
		t.Fatalf("ΔCost = %d µUSD/h, want 120000", a.DeltaMicro)
	}
	want := map[string]Micro{
		// (12−10) nodes × $0.100000 start average.
		TermNodeCount: 200_000,
		// 12 × [(8/12−1)×100000 + (4/12−0)×40000] µUSD.
		TermSpotRatio: -240_000,
		// Within each lifecycle the instance type never changed.
		TermInstanceMix: 0,
		// 8 surviving on-demand nodes × ($0.12−$0.10).
		TermPricingCatalog: 160_000,
	}
	for kind, v := range want {
		if got := mustTerm(t, a, kind); got.Micro != v {
			t.Errorf("term %s = %d µUSD/h, want %d", kind, got.Micro, v)
		}
	}
	if a.Residual.Micro != 0 {
		t.Errorf("residual = %d µUSD/h, want 0: the composition fully prices the observed cost", a.Residual.Micro)
	}
}

// TestWhyCostGolden freezes the whole payload, prose included. A change to
// any label, fact, ordering or citation is a deliberate act after this.
func TestWhyCostGolden(t *testing.T) {
	a := mustWhy(t, baseInput())
	goldenJSON(t, "whycost_base", a)
	goldenText(t, "whycost_base_prose", a.Prose())
}

// TestSumInvariantAcrossShapes runs the invariant over deliberately awkward
// inputs — empty fleets, single groups, price-only moves, everything shrinking.
func TestSumInvariantAcrossShapes(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	tl := func(a, b float64, na, nb int) []evidence.TimelinePoint {
		return []evidence.TimelinePoint{point(from, a, na), point(to.Add(-time.Minute), b, nb)}
	}
	cases := []struct {
		name string
		in   Input
	}{
		{"no composition at all", Input{
			Cluster: testCluster, From: from, To: to, Timeline: tl(2, 3, 20, 30),
		}},
		{"grows from an empty fleet", Input{
			Cluster: testCluster, From: from, To: to, Timeline: tl(0, 0.5, 0, 5),
			Start: &CostBasis{At: from},
			End:   &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "c6i.xlarge", Nodes: 5, UnitUSDPerHour: 0.10}}},
		}},
		{"shrinks to an empty fleet", Input{
			Cluster: testCluster, From: from, To: to, Timeline: tl(0.5, 0, 5, 0),
			Start: &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "c6i.xlarge", Nodes: 5, UnitUSDPerHour: 0.10}}},
			End:   &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "c6i.xlarge", Nodes: 0, UnitUSDPerHour: 0.10}}},
		}},
		{"price only", Input{
			Cluster: testCluster, From: from, To: to, Timeline: tl(1.0, 1.5, 10, 10),
			Start: &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
			End:   &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.15}}},
		}},
		{"whole fleet swaps type and lifecycle", Input{
			Cluster: testCluster, From: from, To: to, Timeline: tl(1.0, 0.36, 10, 12),
			Start: &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
			End: &CostBasis{At: to, Groups: []NodeGroup{
				{InstanceType: "c6i.large", Spot: true, Nodes: 12, UnitUSDPerHour: 0.03},
			}},
		}},
		{"observed cost the composition cannot explain", Input{
			Cluster: testCluster, From: from, To: to, Timeline: tl(1.0, 2.0, 10, 10),
			Start: &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
			End:   &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := mustWhy(t, tc.in)
			checkSums(t, a)
			checkCitable(t, a)
		})
	}
}

// TestResidualIsReportedNotAbsorbed is the honesty test. The composition says
// the fleet costs $1.00/h at both edges; the meter says the bill doubled.
// The extra dollar is unexplained, and it must appear as residual — not
// quietly enlarge whichever term happened to be biggest.
func TestResidualIsReportedNotAbsorbed(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.0, 10), point(to.Add(-time.Minute), 2.0, 10)},
		Start:    &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
		End:      &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	if a.Residual.Micro != 1_000_000 {
		t.Fatalf("residual = %d µUSD/h, want the whole unexplained 1000000", a.Residual.Micro)
	}
	for _, term := range a.Terms {
		if term.Micro != 0 {
			t.Errorf("term %s = %d µUSD/h; nothing in the composition moved, so every term must be zero",
				term.Kind, term.Micro)
		}
	}
	if len(a.Notes) == 0 || !strings.Contains(strings.Join(a.Notes, " "), "unpriced") {
		t.Errorf("expected a note naming the unpriced gap, got %v", a.Notes)
	}
}

// TestAttributionOrderIsTheDocumentedOne pins the choice the package doc
// argues for. Node-count is measured at the START mix; the equally defensible
// "mix first" convention would measure it at the END mix and produce a
// different number. An undocumented order is a bug waiting to be "fixed"
// into a different set of numbers, so the convention is asserted, not assumed.
func TestAttributionOrderIsTheDocumentedOne(t *testing.T) {
	a := mustWhy(t, baseInput())
	node := mustTerm(t, a, TermNodeCount)

	// Documented: ΔN × (start cost / start nodes) = 2 × $0.100000.
	const startMixAnswer = Micro(200_000)
	// The alternative: ΔN × (end mix priced at start prices / end nodes)
	//   = 2 × ((8×$0.10 + 4×$0.04)/12) = 2 × $0.08.
	const endMixAnswer = Micro(160_000)

	if node.Micro != startMixAnswer {
		t.Fatalf("node-count = %d µUSD/h, want %d (measured at the START mix, per the package doc)",
			node.Micro, startMixAnswer)
	}
	if startMixAnswer == endMixAnswer {
		t.Fatal("fixture is useless: both orders agree, so it proves nothing about the order")
	}
	if want := []string{TermNodeCount, TermSpotRatio, TermInstanceMix, TermPricingCatalog}; !equalStrings(a.Order, want) {
		t.Errorf("Order = %v, want %v", a.Order, want)
	}
	// The emitted term order is the chain order, so a reader of the JSON sees
	// the convention in the sequence as well as in the Order field.
	var kinds []string
	for _, term := range a.Terms {
		kinds = append(kinds, term.Kind)
	}
	if !equalStrings(kinds, a.Order) {
		t.Errorf("terms are emitted as %v, want chain order %v", kinds, a.Order)
	}
}

// TestSubAttributionOrderTakesFactsBeforeInferences pins the second documented
// order: Kilter's counted node actions are removed from the node-count term
// before the proportional workload-demand split runs on what is left.
func TestSubAttributionOrderTakesFactsBeforeInferences(t *testing.T) {
	a := mustWhy(t, baseInput())
	node := mustTerm(t, a, TermNodeCount)
	if len(node.Of) == 0 {
		t.Fatal("node-count carries no sub-attribution")
	}
	if node.Of[0].Kind != TermKilterAction {
		t.Errorf("first sub-attribution is %q, want %q: counted facts precede proportional inference",
			node.Of[0].Kind, TermKilterAction)
	}
	if last := node.Of[len(node.Of)-1]; last.Kind != TermUnattributed {
		t.Errorf("last sub-attribution is %q, want %q", last.Kind, TermUnattributed)
	}
	// Two nodes removed at the $0.10 start average = −$0.20/h.
	kilter := mustTerm(t, a, TermKilterAction)
	if kilter.Micro != -200_000 {
		t.Errorf("kilter-action = %d µUSD/h, want -200000", kilter.Micro)
	}
}

// TestShuffleIsIdentical is the determinism test the float-associativity trap
// demands. Nothing about the answer may depend on the order the caller
// happened to assemble its slices in.
func TestShuffleIsIdentical(t *testing.T) {
	base := mustWhy(t, baseInput())
	want, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rng := rand.New(rand.NewSource(20260301))
	for i := 0; i < 64; i++ {
		in := baseInput()
		rng.Shuffle(len(in.Timeline), func(a, b int) { in.Timeline[a], in.Timeline[b] = in.Timeline[b], in.Timeline[a] })
		rng.Shuffle(len(in.Events), func(a, b int) { in.Events[a], in.Events[b] = in.Events[b], in.Events[a] })
		rng.Shuffle(len(in.End.Groups), func(a, b int) { in.End.Groups[a], in.End.Groups[b] = in.End.Groups[b], in.End.Groups[a] })
		rng.Shuffle(len(in.End.Namespaces), func(a, b int) {
			in.End.Namespaces[a], in.End.Namespaces[b] = in.End.Namespaces[b], in.End.Namespaces[a]
		})
		rng.Shuffle(len(in.Start.Namespaces), func(a, b int) {
			in.Start.Namespaces[a], in.Start.Namespaces[b] = in.Start.Namespaces[b], in.Start.Namespaces[a]
		})
		got, err := json.Marshal(mustWhy(t, in))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("shuffle %d changed the answer\n got: %s\nwant: %s", i, got, want)
		}
	}
}

// TestSplitGroupRowsMergeNotDouble: a collector that reports one group across
// two rows must produce the same answer as one that reports it once.
func TestSplitGroupRowsMergeNotDouble(t *testing.T) {
	one := baseInput()
	split := baseInput()
	split.End.Groups = []NodeGroup{
		{InstanceType: "m5.large", Nodes: 5, UnitUSDPerHour: 0.12},
		{InstanceType: "m5.large", Spot: true, Nodes: 4, UnitUSDPerHour: 0.04},
		{InstanceType: "m5.large", Nodes: 3, UnitUSDPerHour: 0.12},
	}
	a, b := mustWhy(t, one), mustWhy(t, split)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("split group rows changed the answer\n one: %s\nsplit: %s", ja, jb)
	}
}

// TestWorkloadSetSplit checks the demand attribution on a fixture with a
// clean answer: ml-batch arrives with 6000 millicores while the surviving
// namespaces add 1000, so the split is 6:1 of whatever Kilter did not do.
func TestWorkloadSetSplit(t *testing.T) {
	a := mustWhy(t, baseInput())
	node := mustTerm(t, a, TermNodeCount)
	kilter := mustTerm(t, a, TermKilterAction)
	ws := mustTerm(t, a, TermWorkloadSet)
	scale := mustTerm(t, a, TermWorkloadScaling)
	unattr := mustTerm(t, a, TermUnattributed)

	// Memory moves 26/48 over the window against CPU's 7/20, so memory is
	// the binding dimension and the split is taken on it: ml-batch arrives
	// with 24 GiB while the surviving namespaces add 2 GiB.
	if got := factValue(ws.Facts, "demandDimension"); got != "memory" {
		t.Fatalf("demandDimension = %q, want %q", got, "memory")
	}
	rest := node.Micro - kilter.Micro
	if want := Micro(math.Round(float64(rest) * 24.0 / 26.0)); ws.Micro != want {
		t.Errorf("workload-set = %d µUSD/h, want %d (24/26 of the %d left after Kilter's actions)", ws.Micro, want, rest)
	}
	if want := Micro(math.Round(float64(rest) * 2.0 / 26.0)); scale.Micro != want {
		t.Errorf("workload-scaling = %d µUSD/h, want %d", scale.Micro, want)
	}
	if unattr.Micro > 1 || unattr.Micro < -1 {
		t.Errorf("unattributed = %d µUSD/h; a fully-attributed split should leave only quantization", unattr.Micro)
	}
	if got := factValue(ws.Facts, "added"); got != "ml-batch" {
		t.Errorf("workload-set added fact = %q, want %q", got, "ml-batch")
	}
}

// TestWorkloadSplitRefusedOnCancellation: when added and surviving demand
// nearly cancel, dividing by the net turns rounding noise into a confident
// enormous number. The split must be refused and said out loud.
func TestWorkloadSplitRefusedOnCancellation(t *testing.T) {
	in := baseInput()
	// Memory is flat across the window so CPU is the binding dimension, and
	// in CPU the departing namespace (−100k) and the surviving one (+99k)
	// very nearly cancel: a net of 1000 against parts of 199000.
	in.Start.Namespaces = []NamespaceDemand{
		{Namespace: "a", MilliCPU: 100_000, MemoryBytes: 1 << 30},
		{Namespace: "doomed", MilliCPU: 100_000},
	}
	in.End.Namespaces = []NamespaceDemand{
		{Namespace: "a", MilliCPU: 199_000, MemoryBytes: 1 << 30},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	if _, ok := termByKind(a, TermWorkloadSet); ok {
		t.Error("workload-set was emitted despite near-total cancellation")
	}
	if !strings.Contains(strings.Join(a.Notes, " "), "cancel") {
		t.Errorf("expected a note explaining the refused split, got %v", a.Notes)
	}
	unattr := mustTerm(t, a, TermUnattributed)
	node := mustTerm(t, a, TermNodeCount)
	kilter := mustTerm(t, a, TermKilterAction)
	if unattr.Micro != node.Micro-kilter.Micro {
		t.Errorf("unattributed = %d, want the whole %d left after Kilter's actions", unattr.Micro, node.Micro-kilter.Micro)
	}
}

// TestEmptyBasisIsSuppliedNotMissing is the regression for the bug
// FuzzWhyCostInvariants found on its first run: a CostBasis describing an
// *empty* fleet ("the cluster did not exist yet at the window start") was
// treated as "no composition supplied", which silently dumped the entire
// $23/h a new cluster cost into the residual. nil means unsupplied; an empty
// group list is a claim, and a claim this package can fully explain.
func TestEmptyBasisIsSuppliedNotMissing(t *testing.T) {
	from, to := t0, t0.Add(hours(49))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 0, 0), point(to.Add(-time.Minute), 23.04, 48)},
		Start:    &CostBasis{At: from},
		End: &CostBasis{At: to.Add(-time.Minute), Groups: []NodeGroup{
			{InstanceType: "m5.large", Nodes: 48, UnitUSDPerHour: 0.48},
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkCitable(t, a)
	if a.Residual.Micro != 0 {
		t.Errorf("residual = %d µUSD/h, want 0: a fleet appearing from nothing is fully explained", a.Residual.Micro)
	}
	node := mustTerm(t, a, TermNodeCount)
	if node.Micro != 23_040_000 {
		t.Errorf("node-count = %d µUSD/h, want the whole 23040000", node.Micro)
	}
	if !strings.Contains(strings.Join(a.Notes, " "), "zero priced nodes") {
		t.Errorf("expected a note about the missing start mix, got %v", a.Notes)
	}
	// The distinct case — genuinely no composition — still degrades to the
	// timeline-only answer, so the two are not being conflated in reverse.
	missing := in
	missing.Start = nil
	b := mustWhy(t, missing)
	if !strings.Contains(strings.Join(b.Notes, " "), "no fleet composition") {
		t.Errorf("a nil basis must still report the degradation, got %v", b.Notes)
	}
}

// TestNoCompositionDegradesHonestly: with only a timeline, node-count is
// still computable and everything else is residual, said in a note. The
// alternative — inventing a composition — is what this package exists to
// avoid.
func TestNoCompositionDegradesHonestly(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.0, 10), point(to.Add(-time.Minute), 1.4, 14)},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkCitable(t, a)
	node := mustTerm(t, a, TermNodeCount)
	if node.Micro != 400_000 { // 4 nodes × $0.10 observed average
		t.Errorf("node-count = %d µUSD/h, want 400000", node.Micro)
	}
	if a.Residual.Micro != 0 {
		t.Errorf("residual = %d, want 0 for a pure node-count move", a.Residual.Micro)
	}
	if !strings.Contains(strings.Join(a.Notes, " "), "no fleet composition") {
		t.Errorf("expected a note about the missing composition, got %v", a.Notes)
	}
	if _, ok := termByKind(a, TermSpotRatio); ok {
		t.Error("spot-ratio emitted with no composition to compute it from")
	}
}

// TestObservedActionWindowIsContextNotAttribution: the realized move across a
// Kilter action's window is reported as a fact and never becomes the term's
// value. Blaming whatever ran concurrently is the classic attribution lie.
func TestObservedActionWindowIsContextNotAttribution(t *testing.T) {
	a := mustWhy(t, baseInput())
	kilter := mustTerm(t, a, TermKilterAction)
	obs := factValue(kilter.Facts, "observedAcrossActionWindowsUSDPerHour")
	if obs == "" {
		t.Fatal("expected the observed action-window move to be reported as a fact")
	}
	if obs == formatUSD(kilter.Micro.USD()) {
		t.Errorf("the observed move %s equals the attributed value; the fixture cannot tell them apart", obs)
	}
	if !strings.Contains(factValue(kilter.Facts, "observedCaveat"), "not an attribution") {
		t.Error("the observed fact must carry its caveat")
	}
}

// TestDryRunActionsAreNotAttributed: a plan that only previewed moved no money.
func TestDryRunActionsAreNotAttributed(t *testing.T) {
	in := baseInput()
	in.Actions[0].Mode = "dry-run"
	in.Actions[0].Applied = false
	a := mustWhy(t, in)
	checkSums(t, a)
	if _, ok := termByKind(a, TermKilterAction); ok {
		t.Error("a dry-run plan was attributed a cost effect")
	}
}

// TestActionsOutsideWindowIgnored keeps the window an argument in practice,
// not just in the signature.
func TestActionsOutsideWindowIgnored(t *testing.T) {
	in := baseInput()
	in.Actions = append(in.Actions, LedgerAction{
		At: t0.Add(-hours(5)), Cluster: testCluster, Fingerprint: "before", Mode: "apply",
		Applied: true, NodesRemoved: 99,
	}, LedgerAction{
		At: t0.Add(hours(400)), Cluster: testCluster, Fingerprint: "after", Mode: "apply",
		Applied: true, NodesRemoved: 99,
	}, LedgerAction{
		At: t0.Add(hours(60)), Cluster: "some-other-cluster", Fingerprint: "elsewhere", Mode: "apply",
		Applied: true, NodesRemoved: 99,
	})
	a := mustWhy(t, in)
	kilter := mustTerm(t, a, TermKilterAction)
	if kilter.Micro != -200_000 {
		t.Errorf("kilter-action = %d µUSD/h, want -200000: out-of-window and other-cluster entries must not count", kilter.Micro)
	}
}

// TestPricingCatalogTermMeansPricesMoved: a group that appears mid-window
// borrows the other edge's price, so its arrival is a mix change and not a
// phantom price change.
func TestPricingCatalogTermMeansPricesMoved(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.0, 10), point(to.Add(-time.Minute), 1.2, 12)},
		Start:    &CostBasis{At: from, Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}}},
		End: &CostBasis{At: to, Groups: []NodeGroup{
			{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10},
			{InstanceType: "r6i.large", Nodes: 2, UnitUSDPerHour: 0.10},
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	cat := mustTerm(t, a, TermPricingCatalog)
	if cat.Micro != 0 {
		t.Errorf("pricing-catalog = %d µUSD/h, want 0: no price moved, a new group merely appeared", cat.Micro)
	}
	if got := factValue(cat.Facts, "groupsRepriced"); got != "0" {
		t.Errorf("groupsRepriced = %q, want %q", got, "0")
	}
}

// TestWindowNeedsTwoObservations: ΔCost is a measurement, and one point is
// not a change.
func TestWindowNeedsTwoObservations(t *testing.T) {
	in := baseInput()
	in.Timeline = in.Timeline[:1]
	if _, err := WhyCost(in); err == nil {
		t.Fatal("expected an error for a single timeline point")
	} else if !strings.Contains(err.Error(), "two observations") {
		t.Errorf("error %q should explain why one point is not enough", err)
	}
}

func TestWhyCostValidation(t *testing.T) {
	good := baseInput()
	cases := []struct {
		name string
		mut  func(*Input)
		want string
	}{
		{"no cluster", func(in *Input) { in.Cluster = "  " }, "needs a cluster"},
		{"zero from", func(in *Input) { in.From = time.Time{} }, "bounded window"},
		{"zero to", func(in *Input) { in.To = time.Time{} }, "bounded window"},
		{"inverted", func(in *Input) { in.From, in.To = in.To, in.From }, "empty or inverted"},
		{"negative nodes", func(in *Input) { in.Start.Groups[0].Nodes = -1 }, "node count"},
		{"absurd node count", func(in *Input) { in.Start.Groups[0].Nodes = maxNodes + 1 }, "node count"},
		{"NaN price", func(in *Input) { in.Start.Groups[0].UnitUSDPerHour = math.NaN() }, "not a usable price"},
		{"infinite price", func(in *Input) { in.End.Groups[0].UnitUSDPerHour = math.Inf(1) }, "not a usable price"},
		{"negative price", func(in *Input) { in.End.Groups[0].UnitUSDPerHour = -0.01 }, "not a usable price"},
		{"unnamed group", func(in *Input) { in.Start.Groups[0].InstanceType = "" }, "no instance type"},
		{"unnamed namespace", func(in *Input) { in.End.Namespaces[0].Namespace = "" }, "no namespace"},
		{"negative demand", func(in *Input) { in.End.Namespaces[0].MilliCPU = -5 }, "negative demand"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mut(&in)
			_, err := WhyCost(in)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := WhyCost(good); err != nil {
		t.Fatalf("the unmutated fixture must still pass: %v", err)
	}
}

// TestExtremePricesRejectedNotSaturated: the money range is enforced with an
// error. A saturated total is a wrong answer wearing the costume of a right
// one.
func TestExtremePricesRejectedNotSaturated(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.0, 10), point(to.Add(-time.Minute), 1.0, 10)},
		Start: &CostBasis{At: from, Groups: []NodeGroup{
			{InstanceType: "absurd", Nodes: 1_000_000, UnitUSDPerHour: MaxUSD},
		}},
		End: &CostBasis{At: to, Groups: []NodeGroup{{InstanceType: "absurd", Nodes: 1, UnitUSDPerHour: 1}}},
	}
	_, err := WhyCost(in)
	if err == nil {
		t.Fatal("expected a range error rather than a saturated total")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("error %q, want a money range error", err)
	}
}

// TestNodeCountMismatchIsNoted: when the composition disagrees with the
// observed node count, the payload says so instead of picking a winner.
func TestNodeCountMismatchIsNoted(t *testing.T) {
	in := baseInput()
	in.Timeline[0].Nodes = 99
	a := mustWhy(t, in)
	checkSums(t, a)
	joined := strings.Join(a.Notes, " ")
	if !strings.Contains(joined, "99") {
		t.Errorf("expected a note about the node-count disagreement, got %v", a.Notes)
	}
}

// TestTermsCarryTheRightEvidence: a spot-ratio term cites spot interruptions,
// a catalog term cites pricing changes, and workload-set cites the deploys in
// the namespaces that came and went.
func TestTermsCarryTheRightEvidence(t *testing.T) {
	a := mustWhy(t, baseInput())
	cases := []struct {
		kind, want string
	}{
		{TermSpotRatio, "/spot-interrupt@"},
		{TermPricingCatalog, "/pricing-change@"},
		{TermWorkloadSet, "ml-batch"},
		{TermWorkloadScaling, "payments"},
	}
	for _, tc := range cases {
		term := mustTerm(t, a, tc.kind)
		if !anyIDContains(term.Evidence, tc.want) {
			t.Errorf("term %s cites %v, none containing %q", tc.kind, term.Evidence, tc.want)
		}
	}
}

// TestCitationsResolveAgainstTheStore is the §5.7 contract: the store that
// produced the answer must be able to re-serve every id in it.
func TestCitationsResolveAgainstTheStore(t *testing.T) {
	in := baseInput()
	mem := memWithFixtures(t, in)
	a := mustWhy(t, in)
	r := Resolver{Store: mem, Actions: in.Actions}
	if err := a.Verify(r); err != nil {
		t.Fatalf("attribution does not verify: %v", err)
	}
	ids := a.Citations()
	if len(ids) == 0 {
		t.Fatal("attribution cites nothing")
	}
	cits, err := r.ResolveAll(ids)
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if len(cits) != len(ids) {
		t.Fatalf("resolved %d of %d citations", len(cits), len(ids))
	}
}

// TestVerifyFailsAgainstAnEmptyStore proves Verify is load-bearing rather
// than decorative.
func TestVerifyFailsAgainstAnEmptyStore(t *testing.T) {
	in := baseInput()
	a := mustWhy(t, in)
	empty, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	if err := a.Verify(Resolver{Store: empty}); err == nil {
		t.Fatal("Verify passed against a store holding none of the cited evidence")
	}
}

// TestEvidenceIDsAreCapped keeps the citation list bounded — a list nobody
// can check is not evidence.
func TestEvidenceIDsAreCapped(t *testing.T) {
	in := baseInput()
	for i := 0; i < 200; i++ {
		in.Events = append(in.Events, ev(t0.Add(time.Duration(i)*time.Minute+time.Hour),
			evidence.EventPricingChange, evidence.SeverityInfo,
			evidence.NodeSubject(testCluster, "node-"+string(rune('a'+i%26))), nil))
	}
	a := mustWhy(t, in)
	cat := mustTerm(t, a, TermPricingCatalog)
	if len(cat.Evidence) > maxEvidencePerTerm {
		t.Errorf("pricing-catalog cites %d ids, cap is %d", len(cat.Evidence), maxEvidencePerTerm)
	}
	if cat.EvidenceTruncated == 0 {
		t.Error("the cap dropped citations without reporting it; nothing may be dropped silently")
	}
	if want := 203 - maxEvidencePerTerm; cat.EvidenceTruncated != want {
		t.Errorf("EvidenceTruncated = %d, want %d (200 synthetic + 1 fixture pricing-change + 2 anchors, less the cap)",
			cat.EvidenceTruncated, want)
	}
}

func TestProseQuotesOnlyComputedNumbers(t *testing.T) {
	a := mustWhy(t, baseInput())
	p := a.Prose()
	for _, want := range []string{
		signedUSD(a.DeltaMicro),
		signedUSD(mustTerm(t, a, TermNodeCount).Micro),
		signedUSD(a.Residual.Micro),
		"node-count → spot-ratio → instance-mix → pricing-catalog",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prose is missing %q\n%s", want, p)
		}
	}
}

func factValue(facts []Fact, key string) string {
	for _, f := range facts {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

func anyIDContains(ids []ID, sub string) bool {
	for _, id := range ids {
		if strings.Contains(string(id), sub) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCheckCatchesEveryWayTheInvariantCanBreak exercises the internal guard
// directly. It is the net under a future refactor: if someone changes how
// terms are built and the sum stops closing, WhyCost must return an error
// rather than ship a wrong audit record.
func TestCheckCatchesEveryWayTheInvariantCanBreak(t *testing.T) {
	anchor := []ID{"tl/c@1"}
	ok := func() *Attribution {
		return &Attribution{
			DeltaMicro: 100,
			Terms:      []Term{newTerm(TermNodeCount, "l", 100, nil, anchor)},
			Residual:   newTerm(TermResidual, "l", 0, nil, anchor),
		}
	}
	if err := ok().check(); err != nil {
		t.Fatalf("a well-formed attribution must pass: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Attribution)
		want string
	}{
		{"terms do not sum", func(a *Attribution) { a.Terms[0].Micro = 99 }, "but ΔCost is"},
		{"term with no citation", func(a *Attribution) { a.Terms[0].Evidence = nil }, "no evidence id"},
		{"residual with no citation", func(a *Attribution) { a.Residual.Evidence = nil }, "no evidence id"},
		{"sub-terms do not sum", func(a *Attribution) {
			a.Terms[0].Of = []Term{newTerm(TermKilterAction, "l", 1, nil, anchor)}
		}, "sub-terms of"},
		{"sub-term with no citation", func(a *Attribution) {
			a.Terms[0].Of = []Term{{Kind: TermKilterAction, Micro: 100}}
		}, "carries no evidence id"},
		{"sub-term nests further", func(a *Attribution) {
			a.Terms[0].Of = []Term{newTerm(TermKilterAction, "l", 100, nil, anchor)}
			a.Terms[0].Of[0].Of = []Term{newTerm(TermUnattributed, "l", 100, nil, anchor)}
		}, "nests further"},
		{"overflow", func(a *Attribution) {
			a.Terms = []Term{newTerm(TermNodeCount, "l", maxMicro, nil, anchor)}
			a.Residual = newTerm(TermResidual, "l", maxMicro, nil, anchor)
		}, "out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := ok()
			tc.mut(a)
			err := a.check()
			if err == nil {
				t.Fatalf("check passed; expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestAttributionVerifyChecksSubTermsToo: a dangling citation on a nested
// term must fail the publish gate as loudly as one on a top-level term.
func TestAttributionVerifyChecksSubTermsToo(t *testing.T) {
	in := baseInput()
	mem := memWithFixtures(t, in)
	a := mustWhy(t, in)
	r := Resolver{Store: mem, Actions: in.Actions}
	if err := a.Verify(r); err != nil {
		t.Fatalf("baseline must verify: %v", err)
	}
	for i := range a.Terms {
		if len(a.Terms[i].Of) > 0 {
			a.Terms[i].Of[0].Evidence = append(a.Terms[i].Of[0].Evidence, "tl/prod-eks-1@1")
			break
		}
	}
	if err := a.Verify(r); err == nil {
		t.Fatal("a dangling sub-term citation passed Verify")
	}
}

// TestZeroNodeGroupHasNoObservedPrice: a group row with no nodes states a
// price nobody was paying. It must not create a catalog term — that term
// means "prices moved", and a price you ran zero nodes at did not move your
// bill.
func TestZeroNodeGroupHasNoObservedPrice(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.0, 10), point(to.Add(-time.Minute), 1.75, 15)},
		Start: &CostBasis{At: from, Groups: []NodeGroup{
			{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10},
			{InstanceType: "r6i.large", Nodes: 0, UnitUSDPerHour: 0.10},
		}},
		End: &CostBasis{At: to, Groups: []NodeGroup{
			{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10},
			{InstanceType: "r6i.large", Nodes: 5, UnitUSDPerHour: 0.15},
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkCitable(t, a)
	if got := mustTerm(t, a, TermPricingCatalog); got.Micro != 0 {
		t.Errorf("pricing-catalog = %d µUSD/h, want 0: r6i was never run at the start price", got.Micro)
	}
	if a.Residual.Micro != 0 {
		t.Errorf("residual = %d µUSD/h, want 0", a.Residual.Micro)
	}
	// The whole $0.75/h is volume plus mix: 5 more nodes at the $0.10 start
	// average is $0.50, and those nodes being $0.15 r6i rather than the start
	// mix is the remaining $0.25.
	if got := mustTerm(t, a, TermNodeCount); got.Micro != 500_000 {
		t.Errorf("node-count = %d µUSD/h, want 500000", got.Micro)
	}
	if got := mustTerm(t, a, TermInstanceMix); got.Micro != 250_000 {
		t.Errorf("instance-mix = %d µUSD/h, want 250000", got.Micro)
	}
}
