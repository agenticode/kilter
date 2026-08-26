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

// anchorID is the citation every fixture charge carries: the timeline point
// the charge was read from, which is exactly the standard the node terms hold
// themselves to.
func anchorID(at time.Time) ID { return TimelineID(testCluster, point(at, 0, 0)) }

// charge builds a citable fixture charge, failing the test rather than
// returning an uncitable one — the constructor is the contract under test in
// TestChargeCannotBeConstructedUncited, not a convenience here.
func charge(t *testing.T, at time.Time, kind, class string, units int64, rate float64) Charge {
	t.Helper()
	c, err := NewCharge(kind, class, units, rate, anchorID(at))
	if err != nil {
		t.Fatalf("NewCharge(%s/%s): %v", kind, class, err)
	}
	return c
}

// fargateInput is the worked example this file's arithmetic follows, and it
// is deliberately node-flat: the fleet costs $1.00/h at both edges, so every
// dollar of the observed move is a charge move and nothing can hide in a node
// term.
//
//	start: 8 pods on 0.25vCPU/0.5GB at $0.012500/h  = $0.100000/h
//	       2 pods on 1vCPU/2GB      at $0.050000/h  = $0.100000/h
//	                                        10 units, $0.200000/h
//	end:   4 pods on 0.25vCPU/0.5GB at $0.012500/h  = $0.050000/h
//	       8 pods on 1vCPU/2GB      at $0.055000/h  = $0.440000/h
//	                                        12 units, $0.490000/h
//
// Observed cost therefore moves $1.20/h → $1.49/h.
func fargateInput(t *testing.T) Input {
	t.Helper()
	from, to := t0, t0.Add(hours(24))
	end := to.Add(-time.Minute)
	return Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.20, 10), point(end, 1.49, 10)},
		Start: &CostBasis{
			At:           from,
			Groups:       []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}},
			ChargesKnown: true,
			Charges: []Charge{
				charge(t, from, ChargeKindFargate, "0.25vCPU/0.5GB", 8, 0.0125),
				charge(t, from, ChargeKindFargate, "1vCPU/2GB", 2, 0.05),
			},
		},
		End: &CostBasis{
			At:           end,
			Groups:       []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}},
			ChargesKnown: true,
			Charges: []Charge{
				charge(t, end, ChargeKindFargate, "0.25vCPU/0.5GB", 4, 0.0125),
				charge(t, end, ChargeKindFargate, "1vCPU/2GB", 8, 0.055),
			},
		},
	}
}

// chargeSub returns one sub-attribution of the charges term.
func chargeSub(t *testing.T, a *Attribution, kind string) Term {
	t.Helper()
	parent := mustTerm(t, a, TermCharges)
	for _, s := range parent.Of {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("no %q sub-attribution of %q; got %d children", kind, TermCharges, len(parent.Of))
	return Term{}
}

// checkChargeSums is the charges dimension's identity, asserted on every
// attribution this file produces. It is the same shape as checkSums and is
// deliberately independent of Attribution.check, so a bug in check cannot
// make the tests agree with it.
func checkChargeSums(t *testing.T, a *Attribution) {
	t.Helper()
	parent, ok := termByKind(a, TermCharges)
	if !a.ChargesKnown {
		if ok {
			t.Errorf("a %q term shipped without charges at both edges", TermCharges)
		}
		return
	}
	if !ok {
		t.Fatalf("charges are known but no %q term was emitted", TermCharges)
	}
	if want := a.ChargeToMicro - a.ChargeFromMicro; parent.Micro != want {
		t.Errorf("%q = %d µUSD/h, want the exact repriced difference %d", TermCharges, parent.Micro, want)
	}
	if parent.Micro != a.ChargeDeltaMicro {
		t.Errorf("%q = %d µUSD/h but ChargeDeltaMicro is %d", TermCharges, parent.Micro, a.ChargeDeltaMicro)
	}
	if len(parent.Of) == 0 {
		return
	}
	var sum Micro
	for _, s := range parent.Of {
		sum += s.Micro
	}
	if sum != parent.Micro {
		t.Errorf("charge sub-terms sum to %d µUSD/h, parent is %d", sum, parent.Micro)
	}
	if left := chargeSub(t, a, TermChargeUnattributed); left.Micro > maxChargeQuantizationMicro || left.Micro < -maxChargeQuantizationMicro {
		t.Errorf("%s = %d µUSD/h, above the %d µUSD/h quantization can explain: the chain is incomplete, not merely rounded",
			TermChargeUnattributed, left.Micro, maxChargeQuantizationMicro)
	}
}

// TestChargesWorkedExample pins the arithmetic by hand. A decomposition
// nobody can check by hand is not an audit artifact.
func TestChargesWorkedExample(t *testing.T) {
	a := mustWhy(t, fargateInput(t))
	checkSums(t, a)
	checkChargeSums(t, a)
	checkCitable(t, a)

	if a.ChargeFromMicro != 200_000 || a.ChargeToMicro != 490_000 {
		t.Fatalf("charge endpoints: from=%d to=%d µUSD/h, want 200000 and 490000", a.ChargeFromMicro, a.ChargeToMicro)
	}
	if a.ChargeDeltaMicro != 290_000 {
		t.Fatalf("Δcharges = %d µUSD/h, want 290000", a.ChargeDeltaMicro)
	}
	want := map[string]Micro{
		// (12−10) units × $0.020000 start average rate.
		TermChargeVolume: 40_000,
		// Only one kind exists, so no kind share can move.
		TermChargeKindMix: 0,
		// 12 × [(0.25·12500 + 0.75·50000) − 20000] µUSD: the mix moved from
		// 80/20 onto 33/67 at START rates.
		TermChargeClassMix: 210_000,
		// 8 surviving 1vCPU/2GB units × ($0.055−$0.050).
		TermChargeRate: 40_000,
		// 40000 + 0 + 210000 + 40000 = 290000, exactly.
		TermChargeUnattributed: 0,
	}
	for kind, v := range want {
		if got := chargeSub(t, a, kind); got.Micro != v {
			t.Errorf("charge sub-term %s = %d µUSD/h, want %d", kind, got.Micro, v)
		}
	}
	// The fleet is flat and fully priced, so the whole ΔCost is the charge
	// move and nothing is left over.
	if a.Residual.Micro != 0 {
		t.Errorf("residual = %d µUSD/h, want 0: composition plus charges price the observed cost exactly", a.Residual.Micro)
	}
	for _, kind := range []string{TermNodeCount, TermSpotRatio, TermInstanceMix, TermPricingCatalog} {
		if got := mustTerm(t, a, kind); got.Micro != 0 {
			t.Errorf("term %s = %d µUSD/h; the fleet did not move, so it must be zero", kind, got.Micro)
		}
	}
}

// TestChargesShrinkTheResidualTheyUsedToBe is the whole point of the unit,
// stated as a before/after: the same cluster, the same meter, with and
// without the charges dimension supplied. Every µUSD that leaves the residual
// must land in the charges term — not merely leave.
func TestChargesShrinkTheResidualTheyUsedToBe(t *testing.T) {
	with := fargateInput(t)
	without := fargateInput(t)
	without.Start.Charges, without.Start.ChargesKnown = nil, false
	without.End.Charges, without.End.ChargesKnown = nil, false

	before := mustWhy(t, without)
	after := mustWhy(t, with)
	checkSums(t, before)
	checkSums(t, after)
	checkChargeSums(t, after)

	if before.Residual.Micro != 290_000 {
		t.Fatalf("without charges the residual is %d µUSD/h, want the whole unattributed 290000", before.Residual.Micro)
	}
	if after.Residual.Micro != 0 {
		t.Fatalf("with charges the residual is %d µUSD/h, want 0", after.Residual.Micro)
	}
	moved := before.Residual.Micro - after.Residual.Micro
	if got := mustTerm(t, after, TermCharges); got.Micro != moved {
		t.Errorf("%d µUSD/h left the residual but the %q term is %d; a dollar may only leave the residual by landing in a term that claims it",
			moved, TermCharges, got.Micro)
	}
	// Nothing else may have moved to make room.
	for _, kind := range []string{TermNodeCount, TermSpotRatio, TermInstanceMix, TermPricingCatalog} {
		b, a2 := mustTerm(t, before, kind), mustTerm(t, after, kind)
		if b.Micro != a2.Micro {
			t.Errorf("term %s changed from %d to %d µUSD/h when charges were supplied; the node chain must be untouched",
				kind, b.Micro, a2.Micro)
		}
	}
}

// TestResidualStillGrowsWhenChargesCannotExplainIt is the complement of the
// test above, and the one that keeps the dimension honest: a decomposition
// that can only ever shrink the residual is not measuring, it is asserting.
//
// Here the supplied charges are flat — Fargate genuinely did not move — and
// the meter says the bill rose by $0.29/h anyway. The charges term must be
// zero and the residual must hold the whole unexplained amount.
func TestResidualStillGrowsWhenChargesCannotExplainIt(t *testing.T) {
	in := fargateInput(t)
	in.End.Charges = []Charge{
		charge(t, in.End.At, ChargeKindFargate, "0.25vCPU/0.5GB", 8, 0.0125),
		charge(t, in.End.At, ChargeKindFargate, "1vCPU/2GB", 2, 0.05),
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkChargeSums(t, a)

	if got := mustTerm(t, a, TermCharges); got.Micro != 0 {
		t.Fatalf("%q = %d µUSD/h, want 0: no charge line moved", TermCharges, got.Micro)
	}
	for _, s := range mustTerm(t, a, TermCharges).Of {
		if s.Micro != 0 {
			t.Errorf("charge sub-term %s = %d µUSD/h; nothing moved, so every one must be zero", s.Kind, s.Micro)
		}
	}
	if a.Residual.Micro != 290_000 {
		t.Fatalf("residual = %d µUSD/h, want the whole unexplained 290000", a.Residual.Micro)
	}
	if !strings.Contains(strings.Join(a.Notes, " "), "unpriced") {
		t.Errorf("expected a note naming the unpriced gap, got %v", a.Notes)
	}
}

// TestChargesAtOneEdgeAreRefusedNotZeroed: a missing edge is not a zero.
// Reading it as one would report an entire Fargate bill as having appeared
// out of nothing — a large, confident, wrong number where an admission
// belonged.
func TestChargesAtOneEdgeAreRefusedNotZeroed(t *testing.T) {
	in := fargateInput(t)
	in.Start.Charges, in.Start.ChargesKnown = nil, false

	a := mustWhy(t, in)
	checkSums(t, a)
	checkCitable(t, a)

	if a.ChargesKnown {
		t.Error("ChargesKnown is set although only one edge stated its charges")
	}
	if _, ok := termByKind(a, TermCharges); ok {
		t.Fatalf("a %q term was emitted from a single edge", TermCharges)
	}
	if len(a.ChargeOrder) != 0 || a.ChargeDeltaMicro != 0 {
		t.Errorf("charge fields are populated without a two-edge dimension: order=%v delta=%d", a.ChargeOrder, a.ChargeDeltaMicro)
	}
	// $1.20 observed against $1.00 of fleet at the start, $1.49 against $1.00
	// at the end: the whole $0.29 charge move stays unattributed.
	if a.Residual.Micro != 290_000 {
		t.Fatalf("residual = %d µUSD/h, want 290000", a.Residual.Micro)
	}
	joined := strings.Join(a.Notes, " ")
	if !strings.Contains(joined, "a missing edge is not a zero") {
		t.Errorf("expected a note refusing the one-sided dimension, got %v", a.Notes)
	}
}

// TestChargeAttributionOrderIsTheDocumentedOne pins the choice charges.go
// argues for. charge-volume is measured at the START class mix; the equally
// defensible "class mix first" convention would measure it at the END mix and
// produce a different number.
func TestChargeAttributionOrderIsTheDocumentedOne(t *testing.T) {
	a := mustWhy(t, fargateInput(t))
	vol := chargeSub(t, a, TermChargeVolume)

	// Documented: ΔU × (start charge cost / start units) = 2 × $0.020000.
	const startMixAnswer = Micro(40_000)
	// The alternative: ΔU × (end mix priced at start rates / end units)
	//   = 2 × ((4×$0.0125 + 8×$0.05)/12) = 2 × $0.0375.
	const endMixAnswer = Micro(75_000)

	if vol.Micro != startMixAnswer {
		t.Fatalf("charge-volume = %d µUSD/h, want %d (measured at the START charge mix, per charges.go)",
			vol.Micro, startMixAnswer)
	}
	if startMixAnswer == endMixAnswer {
		t.Fatal("fixture is useless: both conventions agree, so it proves nothing about the order")
	}
	if want := chargeChainOrder; !equalStrings(a.ChargeOrder, want) {
		t.Errorf("ChargeOrder = %v, want %v", a.ChargeOrder, want)
	}
	// The emitted sub-term order is the chain order, so a reader of the JSON
	// sees the convention in the sequence as well as in the field.
	var kinds []string
	for _, s := range mustTerm(t, a, TermCharges).Of {
		kinds = append(kinds, s.Kind)
	}
	if want := append(append([]string(nil), chargeChainOrder...), TermChargeUnattributed); !equalStrings(kinds, want) {
		t.Errorf("charge sub-terms are emitted as %v, want %v", kinds, want)
	}
	// And charges is the last link of the top-level chain, so Order still
	// describes the terms in the order they appear.
	var top []string
	for _, term := range a.Terms {
		top = append(top, term.Kind)
	}
	if !equalStrings(top, a.Order) {
		t.Errorf("terms are emitted as %v, want the stated order %v", top, a.Order)
	}
	if last := a.Order[len(a.Order)-1]; last != TermCharges {
		t.Errorf("Order ends with %q, want %q", last, TermCharges)
	}
}

// TestChargeKindMixIsThePolicyTerm pins the second half of the order
// argument: moving units between *kinds* is a placement policy and belongs in
// charge-kind-mix, priced at the old within-kind class mix. Half the Fargate
// pods move to a cheaper kind at the same unit count, so volume is zero and
// the whole move is the policy term.
func TestChargeKindMixIsThePolicyTerm(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	end := to.Add(-time.Minute)
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 0.40, 0), point(end, 0.30, 0)},
		Start: &CostBasis{At: from, ChargesKnown: true, Charges: []Charge{
			charge(t, from, ChargeKindFargate, "1vCPU/2GB", 8, 0.05),
		}},
		End: &CostBasis{At: end, ChargesKnown: true, Charges: []Charge{
			charge(t, end, ChargeKindFargate, "1vCPU/2GB", 4, 0.05),
			charge(t, end, "ebs", "gp3", 4, 0.025),
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkChargeSums(t, a)

	if got := chargeSub(t, a, TermChargeVolume).Micro; got != 0 {
		t.Errorf("charge-volume = %d µUSD/h, want 0: the unit count did not move", got)
	}
	// 8 × [(0.5−1)×50000 + (0.5−0)×25000] = −100000.
	if got := chargeSub(t, a, TermChargeKindMix).Micro; got != -100_000 {
		t.Errorf("charge-kind-mix = %d µUSD/h, want -100000", got)
	}
	if got := chargeSub(t, a, TermChargeClassMix).Micro; got != 0 {
		t.Errorf("charge-class-mix = %d µUSD/h, want 0: no class share moved inside a kind", got)
	}
	if got := chargeSub(t, a, TermChargeRate).Micro; got != 0 {
		t.Errorf("charge-rate = %d µUSD/h, want 0: no rate moved", got)
	}
}

// TestChargeRateMeansRatesMoved: a line that appears mid-window borrows the
// other edge's rate, so it contributes a volume or mix change and never a
// rate change. Otherwise "rates rose" would mean "you bought something".
func TestChargeRateMeansRatesMoved(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	end := to.Add(-time.Minute)
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 0.10, 0), point(end, 0.30, 0)},
		Start: &CostBasis{At: from, ChargesKnown: true, Charges: []Charge{
			charge(t, from, ChargeKindFargate, "0.25vCPU/0.5GB", 8, 0.0125),
		}},
		End: &CostBasis{At: end, ChargesKnown: true, Charges: []Charge{
			charge(t, end, ChargeKindFargate, "0.25vCPU/0.5GB", 8, 0.0125),
			charge(t, end, ChargeKindFargate, "1vCPU/2GB", 4, 0.05),
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkChargeSums(t, a)
	if got := chargeSub(t, a, TermChargeRate).Micro; got != 0 {
		t.Errorf("charge-rate = %d µUSD/h, want 0: no rate moved, a new line merely appeared", got)
	}
}

// TestZeroUnitChargeLineHasNoObservedRate mirrors the node side: a line with
// no units states a rate nobody was paying, and must not create a rate term.
func TestZeroUnitChargeLineHasNoObservedRate(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	end := to.Add(-time.Minute)
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 0.10, 0), point(end, 0.30, 0)},
		Start: &CostBasis{At: from, ChargesKnown: true, Charges: []Charge{
			charge(t, from, ChargeKindFargate, "0.25vCPU/0.5GB", 8, 0.0125),
			charge(t, from, ChargeKindFargate, "1vCPU/2GB", 0, 0.04),
		}},
		End: &CostBasis{At: end, ChargesKnown: true, Charges: []Charge{
			charge(t, end, ChargeKindFargate, "0.25vCPU/0.5GB", 8, 0.0125),
			charge(t, end, ChargeKindFargate, "1vCPU/2GB", 4, 0.05),
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkChargeSums(t, a)
	if got := chargeSub(t, a, TermChargeRate).Micro; got != 0 {
		t.Errorf("charge-rate = %d µUSD/h, want 0: the 1vCPU/2GB line was never run at the start rate", got)
	}
}

// TestDuplicateChargeRowsMergeNotDouble: two rows for one line is a collector
// detail, not two terms.
func TestDuplicateChargeRowsMergeNotDouble(t *testing.T) {
	split := fargateInput(t)
	split.End.Charges = []Charge{
		charge(t, split.End.At, ChargeKindFargate, "0.25vCPU/0.5GB", 4, 0.0125),
		charge(t, split.End.At, ChargeKindFargate, "1vCPU/2GB", 5, 0.055),
		charge(t, split.End.At, ChargeKindFargate, "1vCPU/2GB", 3, 0.055),
	}
	merged := mustWhy(t, fargateInput(t))
	got := mustWhy(t, split)
	checkChargeSums(t, got)
	if got.ChargeToMicro != merged.ChargeToMicro {
		t.Fatalf("split rows price at %d µUSD/h, merged at %d", got.ChargeToMicro, merged.ChargeToMicro)
	}
	for _, kind := range chargeChainOrder {
		if a, b := chargeSub(t, got, kind).Micro, chargeSub(t, merged, kind).Micro; a != b {
			t.Errorf("sub-term %s is %d for split rows and %d for merged", kind, a, b)
		}
	}
}

// TestKnownButEmptyChargesAreAClaim: "we looked, there was nothing" is a
// different and more useful answer than "we did not look". It is stated as a
// zero charges term with no chain, not as silence.
func TestKnownButEmptyChargesAreAClaim(t *testing.T) {
	in := fargateInput(t)
	in.Start.Charges, in.End.Charges = nil, nil
	in.Timeline = []evidence.TimelinePoint{point(in.From, 1.00, 10), point(in.End.At, 1.00, 10)}

	a := mustWhy(t, in)
	checkSums(t, a)
	checkChargeSums(t, a)
	checkCitable(t, a)

	parent := mustTerm(t, a, TermCharges)
	if parent.Micro != 0 {
		t.Errorf("%q = %d µUSD/h, want 0", TermCharges, parent.Micro)
	}
	if len(parent.Of) != 0 {
		t.Errorf("%q carries %d sub-terms; an empty dimension has no chain to show", TermCharges, len(parent.Of))
	}
	if !strings.Contains(parent.Label, "no non-node charges") {
		t.Errorf("label %q does not state that the dimension was checked and empty", parent.Label)
	}
	if !a.ChargesKnown {
		t.Error("ChargesKnown is false although both edges stated an empty charge set")
	}
}

// TestChargeCannotBeConstructedUncited is the citation contract for the new
// dimension: NewCharge refuses, and a hand-built uncitable charge fails the
// whole answer rather than being silently dropped. Dropping it would
// understate the charge total and hand the difference to a term that did not
// earn it.
func TestChargeCannotBeConstructedUncited(t *testing.T) {
	if _, err := NewCharge(ChargeKindFargate, "1vCPU/2GB", 4, 0.05); err == nil {
		t.Fatal("NewCharge returned a charge with no citation")
	} else if !strings.Contains(err.Error(), "citable") {
		t.Errorf("error %q does not name the missing citation", err)
	}
	if _, err := NewCharge(ChargeKindFargate, "1vCPU/2GB", 4, 0.05, "not-an-id"); err == nil {
		t.Fatal("NewCharge accepted an unparseable citation")
	}

	in := fargateInput(t)
	in.End.Charges = append(in.End.Charges, Charge{Kind: ChargeKindFargate, Class: "2vCPU/4GB", Units: 1, UnitUSDPerHour: 0.1})
	if _, err := WhyCost(in); err == nil {
		t.Fatal("WhyCost accepted a charge with no citation")
	}

	dangling := fargateInput(t)
	dangling.End.Charges[0].Evidence = []ID{"tl/prod%eks@notanumber"}
	if _, err := WhyCost(dangling); err == nil {
		t.Fatal("WhyCost accepted a charge citing an unparseable id")
	}
}

// TestChargeValidationRefusesRatherThanGuesses.
func TestChargeValidationRefusesRatherThanGuesses(t *testing.T) {
	id := anchorID(t0)
	cases := []struct {
		name string
		mut  func(*CostBasis)
	}{
		{"no kind", func(b *CostBasis) { b.Charges = []Charge{{Units: 1, UnitUSDPerHour: 0.1, Evidence: []ID{id}}} }},
		{"negative units", func(b *CostBasis) {
			b.Charges = []Charge{{Kind: "fargate", Units: -1, UnitUSDPerHour: 0.1, Evidence: []ID{id}}}
		}},
		{"units over cap", func(b *CostBasis) {
			b.Charges = []Charge{{Kind: "fargate", Units: maxChargeUnits + 1, UnitUSDPerHour: 0.1, Evidence: []ID{id}}}
		}},
		{"negative rate", func(b *CostBasis) {
			b.Charges = []Charge{{Kind: "fargate", Units: 1, UnitUSDPerHour: -0.1, Evidence: []ID{id}}}
		}},
		{"NaN rate", func(b *CostBasis) {
			b.Charges = []Charge{{Kind: "fargate", Units: 1, UnitUSDPerHour: math.NaN(), Evidence: []ID{id}}}
		}},
		{"absurd rate", func(b *CostBasis) {
			b.Charges = []Charge{{Kind: "fargate", Units: 1, UnitUSDPerHour: MaxUSD * 10, Evidence: []ID{id}}}
		}},
		{"rows without the flag", func(b *CostBasis) {
			b.ChargesKnown = false
			b.Charges = []Charge{{Kind: "fargate", Units: 1, UnitUSDPerHour: 0.1, Evidence: []ID{id}}}
		}},
		{"too many lines", func(b *CostBasis) {
			b.Charges = make([]Charge, maxCharges+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fargateInput(t)
			in.End.Charges = nil
			tc.mut(in.End)
			if _, err := WhyCost(in); err == nil {
				t.Fatal("WhyCost accepted an input it cannot price honestly")
			}
		})
	}
}

// TestChargeTermsCarryTheEvidenceOfTheLinesThatMoved: a term must cite the
// charges it was computed from, not merely something that resolves.
func TestChargeTermsCarryTheEvidenceOfTheLinesThatMoved(t *testing.T) {
	in := fargateInput(t)
	in.Events = append(in.Events, ev(t0.Add(time.Hour), evidence.EventPricingChange, evidence.SeverityInfo,
		evidence.ClusterSubject(testCluster), map[string]string{"kind": "fargate"}))
	a := mustWhy(t, in)

	startCite, endCite := string(anchorID(in.From)), string(anchorID(in.End.At))
	for _, tc := range []struct{ kind, want string }{
		{TermCharges, startCite},
		{TermChargeVolume, endCite},
		{TermChargeClassMix, endCite},
		{TermChargeRate, "/pricing-change@"},
	} {
		term := mustTerm(t, a, tc.kind)
		if !anyIDContains(term.Evidence, tc.want) {
			t.Errorf("term %s cites %v, none containing %q", tc.kind, term.Evidence, tc.want)
		}
	}
	// Every emitted id must parse — Verify then requires it to resolve.
	for _, term := range append([]Term{mustTerm(t, a, TermCharges)}, mustTerm(t, a, TermCharges).Of...) {
		if len(term.Evidence) == 0 {
			t.Fatalf("charge term %q carries no evidence id", term.Kind)
		}
		for _, id := range term.Evidence {
			if _, err := Parse(id); err != nil {
				t.Errorf("charge term %q emitted an unparseable id %q: %v", term.Kind, id, err)
			}
		}
	}
}

// TestVerifyCoversChargeCitations is requirement 5: a payload whose charge
// citations no longer resolve fails the publish gate exactly as a node term's
// do. It works without a line of new Verify code because the charges chain is
// a sub-attribution — which is most of the argument for nesting it.
func TestVerifyCoversChargeCitations(t *testing.T) {
	in := fargateInput(t)
	mem := memWithFixtures(t, in)
	a := mustWhy(t, in)
	r := Resolver{Store: mem}
	if err := a.Verify(r); err != nil {
		t.Fatalf("baseline must verify: %v", err)
	}
	// Every charge citation is in the set a narrating session may quote.
	cited := map[ID]bool{}
	for _, id := range a.Citations() {
		cited[id] = true
	}
	for _, s := range mustTerm(t, a, TermCharges).Of {
		for _, id := range s.Evidence {
			if !cited[id] {
				t.Errorf("charge sub-term %q cites %q, which Citations() omits", s.Kind, id)
			}
		}
	}

	for _, tc := range []struct {
		name string
		mut  func(*Attribution)
	}{
		{"on the charges term", func(a *Attribution) {
			for i := range a.Terms {
				if a.Terms[i].Kind == TermCharges {
					a.Terms[i].Evidence = append(a.Terms[i].Evidence, "tl/prod-eks-1@1")
				}
			}
		}},
		{"on a charge sub-term", func(a *Attribution) {
			for i := range a.Terms {
				if a.Terms[i].Kind == TermCharges {
					a.Terms[i].Of[0].Evidence = append(a.Terms[i].Of[0].Evidence, "tl/prod-eks-1@1")
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := mustWhy(t, fargateInput(t))
			tc.mut(broken)
			if err := broken.Verify(r); err == nil {
				t.Fatal("a dangling charge citation passed Verify")
			}
		})
	}
}

// TestCheckCatchesEveryWayTheChargeIdentityCanBreak exercises the new guard
// directly, the way TestCheckCatchesEveryWayTheInvariantCanBreak does the old
// one. It is the net under a future refactor.
func TestCheckCatchesEveryWayTheChargeIdentityCanBreak(t *testing.T) {
	anchor := []ID{"tl/c@1"}
	ok := func() *Attribution {
		charges := newTerm(TermCharges, "l", 100, nil, anchor)
		charges.Of = []Term{
			newTerm(TermChargeVolume, "l", 60, nil, anchor),
			newTerm(TermChargeUnattributed, "l", 40, nil, anchor),
		}
		return &Attribution{
			DeltaMicro:      100,
			ChargesKnown:    true,
			ChargeFromMicro: 10, ChargeToMicro: 110, ChargeDeltaMicro: 100,
			Terms:    []Term{charges},
			Residual: newTerm(TermResidual, "l", 0, nil, anchor),
		}
	}
	if err := ok().check(); err != nil {
		t.Fatalf("a well-formed charges attribution must pass: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Attribution)
		want string
	}{
		{"charges term disagrees with the supplied dimension", func(a *Attribution) {
			a.Terms[0].Micro, a.Terms[0].Of[1].Micro = 99, 39
			a.DeltaMicro = 99
		}, "supplied charges differ by"},
		{"endpoints disagree with the stated delta", func(a *Attribution) { a.ChargeToMicro = 111 }, "ChargeDelta is"},
		{"charge sub-terms do not sum", func(a *Attribution) { a.Terms[0].Of[0].Micro = 61 }, "sub-terms of"},
		{"charge sub-term with no citation", func(a *Attribution) { a.Terms[0].Of[0].Evidence = nil }, "no evidence id"},
		{"a charges term without a two-edge dimension", func(a *Attribution) {
			a.ChargesKnown, a.ChargeFromMicro, a.ChargeToMicro, a.ChargeDeltaMicro = false, 0, 0, 0
		}, "without a charges dimension"},
		{"charge amounts without a charges term", func(a *Attribution) {
			a.Terms, a.ChargesKnown = nil, false
			a.Residual = newTerm(TermResidual, "l", 100, nil, anchor)
		}, "charge amounts reported"},
		{"a known dimension with no charges term", func(a *Attribution) {
			a.Terms = []Term{newTerm(TermNodeCount, "l", 100, nil, anchor)}
		}, "no \"charges\" term was emitted"},
		{"two charges terms", func(a *Attribution) {
			a.Terms = append(a.Terms, newTerm(TermCharges, "l", 0, nil, anchor))
		}, "two \"charges\" terms"},
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

// TestChargeShuffleIsIdentical is the determinism test the float-associativity
// trap demands, applied to the new dimension: nothing about the answer may
// depend on the order the caller assembled its charge rows in.
func TestChargeShuffleIsIdentical(t *testing.T) {
	base := mustWhy(t, fargateInput(t))
	want, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rng := rand.New(rand.NewSource(20260826))
	for i := 0; i < 64; i++ {
		in := fargateInput(t)
		// Split every line into single-unit rows first, so the shuffle has
		// something to permute and the merge rule is exercised at the same
		// time.
		for _, b := range []*CostBasis{in.Start, in.End} {
			var rows []Charge
			for _, c := range b.Charges {
				for u := int64(0); u < c.Units; u++ {
					one := c
					one.Units = 1
					rows = append(rows, one)
				}
			}
			rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
			b.Charges = rows
		}
		got, err := json.Marshal(mustWhy(t, in))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("shuffle %d changed the answer\n got: %s\nwant: %s", i, got, want)
		}
	}
}

// TestChargeProseStatesItsConvention: an answer that states one attribution
// convention and hides the other is half an audit record.
func TestChargeProseStatesItsConvention(t *testing.T) {
	a := mustWhy(t, fargateInput(t))
	p := a.Prose()
	for _, want := range []string{
		"charge-volume → charge-kind-mix → charge-class-mix → charge-rate",
		"node-count → spot-ratio → instance-mix → pricing-catalog → charges",
		signedUSD(mustTerm(t, a, TermCharges).Micro),
		signedUSD(chargeSub(t, a, TermChargeClassMix).Micro),
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prose is missing %q\n%s", want, p)
		}
	}
	// Prose over an attribution with no charges must be unchanged.
	if strings.Contains(mustWhy(t, baseInput()).Prose(), "Charge attribution order") {
		t.Error("prose announces a charge order for an attribution that has none")
	}
}

// TestChargeQuantizationBoundMatchesTheNodeSide keeps the two dimensions'
// honesty thresholds from drifting apart.
func TestChargeQuantizationBoundMatchesTheNodeSide(t *testing.T) {
	if maxChargeQuantizationMicro != maxQuantizationMicro {
		t.Fatalf("charge quantization bound is %d µUSD/h but the node side allows %d",
			maxChargeQuantizationMicro, maxQuantizationMicro)
	}
}

// TestChargesWithoutAComposition: the two dimensions are independent. A
// caller that knows its Fargate bill but not its fleet still gets a charge
// decomposition, and the fleet stays in the residual with the existing note.
func TestChargesWithoutAComposition(t *testing.T) {
	from, to := t0, t0.Add(hours(24))
	end := to.Add(-time.Minute)
	in := Input{
		Cluster: testCluster, From: from, To: to,
		Timeline: []evidence.TimelinePoint{point(from, 1.20, 10), point(end, 1.30, 10)},
		Start: &CostBasis{At: from, ChargesKnown: true, Charges: []Charge{
			charge(t, from, ChargeKindFargate, "1vCPU/2GB", 4, 0.05),
		}},
		End: &CostBasis{At: end, ChargesKnown: true, Charges: []Charge{
			charge(t, end, ChargeKindFargate, "1vCPU/2GB", 6, 0.05),
		}},
	}
	a := mustWhy(t, in)
	checkSums(t, a)
	checkChargeSums(t, a)
	checkCitable(t, a)

	if got := mustTerm(t, a, TermCharges); got.Micro != 100_000 {
		t.Errorf("%q = %d µUSD/h, want 100000", TermCharges, got.Micro)
	}
	if got := mustTerm(t, a, TermNodeCount).Micro; got != 0 {
		t.Errorf("node-count = %d µUSD/h, want 0: no node group was supplied", got)
	}
	// The unattributed $1.20/h of fleet is a *level*, not a move: it is
	// identical at both edges, so it cancels out of ΔCost and the residual is
	// zero even though most of the bill is unmodelled. The note is what keeps
	// that from reading as "we priced everything".
	if a.Residual.Micro != 0 {
		t.Errorf("residual = %d µUSD/h, want 0: the unpriced fleet is the same at both edges, so it moved nothing", a.Residual.Micro)
	}
	if !strings.Contains(strings.Join(a.Notes, " "), "unpriced") {
		t.Errorf("expected a note naming the unpriced fleet, got %v", a.Notes)
	}
}
