package explain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

var fuzzChargeKinds = []string{"fargate", "ebs", "lambda"}

// The empty class is deliberate: a kind whose charge has no sub-shape (a flat
// line item) must decompose as cleanly as Fargate's configuration tiers.
var fuzzChargeClasses = []string{"0.25vCPU/0.5GB", "1vCPU/2GB", "4vCPU/8GB", ""}

// charges generates one edge's charges dimension. Unlike the node generator
// it emits duplicate (kind, class) rows freely: the charge identity is
// computed from the *merged* dimension, so duplicates exercise the merge rule
// from inside the invariant rather than having to be excluded from it.
func (r *bits) charges(at time.Time) ([]Charge, bool) {
	if !r.bool() {
		return nil, false // this edge does not know its charges
	}
	cite := TimelineID(testCluster, evidence.TimelinePoint{At: at})
	var out []Charge
	for n := r.intn(7); n > 0; n-- {
		out = append(out, Charge{
			Kind:           fuzzChargeKinds[r.intn(len(fuzzChargeKinds))],
			Class:          fuzzChargeClasses[r.intn(len(fuzzChargeClasses))],
			Units:          int64(r.intn(500)),
			UnitUSDPerHour: r.cents(1000),
			Evidence:       []ID{cite},
		})
	}
	return out, true
}

// modelledCost reprices a whole basis — fleet plus charges — the way WhyCost
// does, by folding it through the same canonical forms. Used to build the
// *closed* case: an input the supplied evidence fully accounts for, where the
// residual must therefore be quantization and nothing else.
func modelledCost(t *testing.T, b *CostBasis) (Micro, int64) {
	t.Helper()
	c, err := newComp(b)
	if err != nil {
		t.Fatalf("newComp: %v", err)
	}
	ch, err := newChargeComp(b)
	if err != nil {
		t.Fatalf("newChargeComp: %v", err)
	}
	total, err := add(c.total, ch.total)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return total, c.nodes
}

// FuzzChargeInvariants is the acceptance property for the charges dimension.
// It asserts, over arbitrary fleets, charge sets, ledgers and timelines, that
// BOTH arithmetic identities hold exactly and that no rounding path can make
// either one fail:
//
//	sum(Terms) + Residual == Delta                              (unchanged)
//	charges.Micro == ChargeDelta == Σ(end u·r) − Σ(start u·r)    (new)
//	sum(charges.Of) == charges.Micro                            (new)
//
// plus the assertion that carries the actual weight, because the first three
// are true by construction in isolation: when the supplied evidence fully
// prices the observed cost, the charge chain's own remainder is quantization
// only. A decomposition that failed to explain a charge move would still
// satisfy the identities — charge-unattributed would silently swallow the
// difference — and this is what catches that.
func FuzzChargeInvariants(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{1, 1, 3, 0, 0, 40, 12, 1, 1, 5, 2, 2, 8, 60, 1, 3, 9, 4, 4, 4, 7, 7})
	f.Add([]byte{3, 1, 200, 99, 1, 6, 2, 1, 90, 0, 0, 12, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	f.Add([]byte{0, 0, 1, 0, 0, 1, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1})
	f.Add(make([]byte, 96))

	f.Fuzz(func(t *testing.T, data []byte) {
		r := &bits{b: data}
		from := t0
		to := t0.Add(hours(1 + r.intn(400)))
		endAt := to.Add(-time.Minute)
		closed := r.bool()

		start, end := r.basis(from), r.basis(endAt)
		start.Charges, start.ChargesKnown = r.charges(from)
		end.Charges, end.ChargesKnown = r.charges(endAt)

		var costA, costB float64
		var nodesA, nodesB int
		if closed {
			// Fleet AND charges price the observed cost exactly: the model is
			// complete in both dimensions, so nothing but quantization may be
			// left anywhere.
			//
			// Both edges must state their charges for that to be true, and
			// the fuzzer found out why: a dimension stated at one edge only
			// is deliberately NOT supplied evidence — WhyCost refuses it and
			// leaves the move in the residual — so a "closed" case built on
			// one is asserting that the package attributed something it is
			// designed to decline. The asymmetric shape is still generated
			// and still asserted, in the open branch below.
			start.ChargesKnown, end.ChargesKnown = true, true
			var ma, mb Micro
			var na, nb int64
			ma, na = modelledCost(t, start)
			mb, nb = modelledCost(t, end)
			costA, costB = ma.USD(), mb.USD()
			nodesA, nodesB = int(na), int(nb)
		} else {
			costA, costB = r.cents(20000), r.cents(20000)
			nodesA, nodesB = r.intn(500), r.intn(500)
		}

		in := Input{
			Cluster: testCluster, From: from, To: to,
			Timeline: []evidence.TimelinePoint{
				point(from, costA, nodesA),
				point(from.Add(to.Sub(from)/2), (costA+costB)/2, (nodesA+nodesB)/2),
				point(endAt, costB, nodesB),
			},
			Start: start, End: end,
		}
		for n := r.intn(3); n > 0; n-- {
			in.Actions = append(in.Actions, LedgerAction{
				At:           from.Add(time.Duration(r.intn(int(to.Sub(from)/time.Hour)+1)) * time.Hour),
				Cluster:      testCluster,
				Fingerprint:  string(rune('a' + r.intn(26))),
				Mode:         "apply",
				Applied:      true,
				NodesRemoved: int64(r.intn(20)),
			})
		}
		for n := r.intn(3); n > 0; n-- {
			in.Events = append(in.Events, ev(
				from.Add(time.Duration(r.intn(24))*time.Hour),
				[]string{evidence.EventPricingChange, evidence.EventSpotInterrupt, evidence.EventDeploy}[r.intn(3)],
				evidence.SeverityInfo,
				workloadSubject(fuzzNamespaces[r.intn(len(fuzzNamespaces))], "app"), nil))
		}

		a, err := WhyCost(in)
		if err != nil {
			// Every generated input is inside the documented bounds and every
			// charge is citable, so there is no acceptable failure here.
			t.Fatalf("WhyCost: %v", err)
		}

		// 1. The central invariant, exactly. Adding a dimension may not cost
		//    the package the identity it was built around.
		sum, err := a.Sum()
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if sum != a.DeltaMicro {
			t.Fatalf("sum(terms)+residual = %d, ΔCost = %d (off by %d)", sum, a.DeltaMicro, sum-a.DeltaMicro)
		}

		bothKnown := start.ChargesKnown && end.ChargesKnown
		if a.ChargesKnown != bothKnown {
			t.Fatalf("ChargesKnown = %v, want %v (start %v, end %v)",
				a.ChargesKnown, bothKnown, start.ChargesKnown, end.ChargesKnown)
		}
		parent, hasCharges := termByKind(a, TermCharges)
		if hasCharges != bothKnown {
			t.Fatalf("a %q term is present = %v but the dimension is known at both edges = %v",
				TermCharges, hasCharges, bothKnown)
		}

		if bothKnown {
			// 2. The charges term is the exact repriced difference of the two
			//    supplied dimensions — the identity that stops the third from
			//    being vacuous.
			chA, err := newChargeComp(start)
			if err != nil {
				t.Fatalf("newChargeComp(start): %v", err)
			}
			chB, err := newChargeComp(end)
			if err != nil {
				t.Fatalf("newChargeComp(end): %v", err)
			}
			want, err := sub(chB.total, chA.total)
			if err != nil {
				t.Fatalf("sub: %v", err)
			}
			if a.ChargeFromMicro != chA.total || a.ChargeToMicro != chB.total {
				t.Fatalf("charge endpoints are %d → %d µUSD/h, want %d → %d",
					a.ChargeFromMicro, a.ChargeToMicro, chA.total, chB.total)
			}
			if a.ChargeDeltaMicro != want || parent.Micro != want {
				t.Fatalf("charges term = %d and ChargeDelta = %d, want %d", parent.Micro, a.ChargeDeltaMicro, want)
			}

			// 3. The chain reconstructs its parent exactly, one level deep.
			if len(parent.Of) > 0 {
				var subSum Micro
				var left Micro
				var sawLeft bool
				for _, s := range parent.Of {
					if len(s.Of) != 0 {
						t.Fatalf("charge sub-term %q nests further", s.Kind)
					}
					subSum += s.Micro
					if s.Kind == TermChargeUnattributed {
						left, sawLeft = s.Micro, true
					}
				}
				if subSum != parent.Micro {
					t.Fatalf("charge sub-terms sum to %d, parent is %d (off by %d)", subSum, parent.Micro, subSum-parent.Micro)
				}
				if !sawLeft {
					t.Fatalf("the charge chain shipped without an explicit %q remainder", TermChargeUnattributed)
				}
				// 4. No rounding path may push the remainder past what
				//    quantization can explain. This is the assertion that
				//    would catch an incomplete chain.
				if left > maxChargeQuantizationMicro || left < -maxChargeQuantizationMicro {
					t.Fatalf("%s holds %d µUSD/h; three quantizations cannot exceed %d, so the charge chain is incomplete, not merely rounded",
						TermChargeUnattributed, left, maxChargeQuantizationMicro)
				}
			}

			// 5. The stated order is the emitted order, and charges is its
			//    last link.
			var kinds []string
			for _, term := range a.Terms {
				kinds = append(kinds, term.Kind)
			}
			if !equalStrings(kinds, a.Order) {
				t.Fatalf("terms are emitted as %v but Order states %v", kinds, a.Order)
			}
			if a.Order[len(a.Order)-1] != TermCharges {
				t.Fatalf("Order ends with %q, want %q", a.Order[len(a.Order)-1], TermCharges)
			}
			if !equalStrings(a.ChargeOrder, chargeChainOrder) {
				t.Fatalf("ChargeOrder = %v, want %v", a.ChargeOrder, chargeChainOrder)
			}
		} else if a.ChargeDeltaMicro != 0 || a.ChargeFromMicro != 0 || a.ChargeToMicro != 0 || len(a.ChargeOrder) != 0 {
			// A half-known dimension must leave no trace but a note: reading
			// the silent edge as zero is the failure this refuses.
			t.Fatalf("charge fields are populated without both edges: from=%d to=%d delta=%d order=%v",
				a.ChargeFromMicro, a.ChargeToMicro, a.ChargeDeltaMicro, a.ChargeOrder)
		}

		// 6. Nothing uncitable ships, charges included.
		for _, term := range append(append([]Term(nil), a.Terms...), a.Residual) {
			for _, s := range append([]Term{term}, term.Of...) {
				if len(s.Evidence) == 0 {
					t.Fatalf("term %q carries no evidence id", s.Kind)
				}
				for _, id := range s.Evidence {
					if _, err := Parse(id); err != nil {
						t.Fatalf("term %q emitted an unparseable id %q: %v", s.Kind, id, err)
					}
				}
			}
		}

		// 7. A complete model — both dimensions — leaves only rounding behind.
		if closed && (a.Residual.Micro > maxQuantizationMicro || a.Residual.Micro < -maxQuantizationMicro) {
			t.Fatalf("a fleet and a charge set that fully price the observed cost left a residual of %d µUSD/h; the decomposition is incomplete, not merely rounded",
				a.Residual.Micro)
		}

		// 8. Determinism: reversing every input slice, charge rows included,
		//    changes nothing.
		want, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rev := in
		rev.Timeline = reversedPoints(in.Timeline)
		rev.Events = reversedEvents(in.Events)
		rev.Actions = reversedActions(in.Actions)
		rev.Start = reversedChargeBasis(in.Start)
		rev.End = reversedChargeBasis(in.End)
		b, err := WhyCost(rev)
		if err != nil {
			t.Fatalf("WhyCost(reversed): %v", err)
		}
		got, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("reversing the inputs changed the answer\n got: %s\nwant: %s", got, want)
		}
	})
}

// reversedChargeBasis is reversedBasis with the charges dimension reversed
// too, so the shuffle covers both dimensions of one basis.
func reversedChargeBasis(in *CostBasis) *CostBasis {
	out := reversedBasis(in)
	if out == nil {
		return nil
	}
	out.ChargesKnown = in.ChargesKnown
	for i := len(in.Charges) - 1; i >= 0; i-- {
		out.Charges = append(out.Charges, in.Charges[i])
	}
	return out
}
