package explain

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/agenticode/kilter/pkg/evidence"
)

// The charges dimension: cost that is billed per *line item* rather than per
// node, decomposed by the same chain the node composition uses.
//
// # Why a second dimension at all
//
// A cluster's observed hourly cost is not only `Σ nodes × price`. EKS Fargate
// pods are billed by their quantized vCPU/memory configuration and have no
// shareable machine behind them (pkg/pricing/fargate.go); EBS, Lambda and
// friends have the same shape. `pkg/pricing.SnapshotCost` already includes
// them in the number the timeline stores, while [CostBasis.Groups]
// deliberately excludes Fargate "nodes" — pricing a single-pod VM by its
// reported shape is a silent overcharge. The gap therefore landed in
// [Attribution.Residual] with a note: honest, and coarse.
//
// This file makes it decomposable *without making it less honest*. Every
// dollar that leaves the residual lands in a term that names a specific
// line-item move, cites the charges it was computed from, and satisfies an
// arithmetic identity checked in [Attribution.check].
//
// # The identity, and where it is enforced
//
//	charges.Micro       == ChargeDeltaMicro == Σ(end u·r) − Σ(start u·r)
//	sum(charges.Of)     == charges.Micro
//
// both re-verified in [Attribution.check] alongside `sum(Terms)+Residual ==
// Delta`, which is unchanged: the charges term is an ordinary member of
// [Attribution.Terms], so the residual is still computed last, as the
// remainder, and can still grow.
//
// The children are the same chain as the node side, one level deep, so the
// existing machinery — `check`, [Attribution.Verify], [Attribution.Citations],
// [Attribution.Sum], [Attribution.Prose] — covers the new dimension with no
// change. Duplicating that machinery for a parallel top-level list would have
// meant duplicating the invariant enforcement too, and an invariant enforced
// in two places is an invariant enforced in neither.
//
// # Attribution order — and why it is this one
//
//	charge-volume → charge-kind-mix → charge-class-mix → charge-rate
//
//   - **charge-volume first**, at the window's starting mix and starting
//     rates. It is the term an operator can check independently: the unit
//     count (Fargate pods) is a number they already know, and the reference
//     per-unit price is printed in the term's facts. Measured at the *end*
//     mix instead, it would be priced against a configuration mix nobody
//     remembers — see TestChargeAttributionOrderIsTheDocumentedOne, which
//     pins a fixture where the two conventions disagree.
//
//   - **kind before class.** A charge *kind* ("fargate", "ebs") is a
//     placement policy someone chose. A charge *class* (which Fargate
//     configuration a pod quantizes onto) is largely a consequence of the
//     requests plus AWS's rounding rule. Giving the interaction to the
//     consequence keeps the policy term equal to "what moving that workload
//     off Fargate would have cost at the old shape mix", which is the
//     counterfactual a human is actually checking.
//
//   - **charge-rate last**, weighted at end-of-window units (a Paasche price
//     index), for the same reason pricing-catalog is: a published rate change
//     you did not cause belongs against the units you actually run now. A
//     line that appeared mid-window borrows the other edge's rate, so
//     "rate" always means *rates moved*, never *the line-item set moved*.
//
// # What still lands in the residual
//
// Everything it did before except the charge move itself. Charges supplied at
// only one edge are NOT attributed — a missing edge is not a zero — and the
// whole move stays in the residual under a note. Cost from a kind nobody
// supplied a line for is invisible here and stays in the residual by
// construction.

// Charge kinds. The set is open — Kind is a caller-chosen label and the
// decomposition treats any string as a policy grouping — but the one this
// unit was built for has a name so the wiring and the tests agree on it.
const (
	// ChargeKindFargate is EKS Fargate pod capacity, priced per quantized
	// vCPU/memory configuration (pkg/pricing §4.1). Class is the
	// configuration, e.g. "0.25vCPU/0.5GB"; Units is the number of pods on it.
	ChargeKindFargate = "fargate"
)

// Term kinds for the charges dimension. `charges` is the top-level term; the
// rest are its sub-attributions, exactly one level deep.
const (
	TermCharges            = "charges"
	TermChargeVolume       = "charge-volume"
	TermChargeKindMix      = "charge-kind-mix"
	TermChargeClassMix     = "charge-class-mix"
	TermChargeRate         = "charge-rate"
	TermChargeUnattributed = "charge-unattributed"
)

// chargeChainOrder is the order the charge sub-terms are peeled off in, and
// it is part of the package's contract — see the file doc above for why it is
// this order and not another. It is echoed into every payload as
// [Attribution.ChargeOrder].
var chargeChainOrder = []string{TermChargeVolume, TermChargeKindMix, TermChargeClassMix, TermChargeRate}

// Charge bounds. Like the node bounds they exist so the money arithmetic
// cannot overflow, and each is far above any real cluster: 4096 distinct
// (kind, class) lines, and 2^32 units — four billion Fargate pods, or four
// billion GiB of block storage.
const (
	maxCharges     = 4096
	maxChargeUnits = int64(1) << 32
)

// ErrUncitableCharge reports a [Charge] with no parseable evidence id.
//
// It is a construction error, not a warning: [NewCharge] refuses to return
// such a charge and [CostBasis.validate] refuses the whole input, so a charge
// that could not be cited never reaches a term. An uncitable term looks like
// knowledge while being unfalsifiable, which is worse than the residual it
// would have been part of.
var ErrUncitableCharge = errors.New("explain: charge with no citable evidence")

// Charge is one non-node line item at one window edge: Units of one
// (Kind, Class) at one unit rate.
//
// It is deliberately the same shape as [NodeGroup] — a count and a unit price
// — because that is what lets the same chain decompose it. `Cost = Σ u·r` is
// the identity on both sides; only the names of the grouping dimensions
// differ (lifecycle/instance-type there, kind/class here).
//
// UnitUSDPerHour is the price of ONE unit, not the line total. Lines with the
// same (Kind, Class) are merged by unit-weighted average rate before any term
// is computed, so a collector that reports a line twice does not become two
// terms.
//
// Evidence is required and must parse. Prefer [NewCharge], which enforces
// that at construction; a hand-built Charge with no evidence is rejected by
// [WhyCost] with [ErrUncitableCharge] rather than dropped, because silently
// dropping a line would understate the charge total and hand the difference
// to a term that did not earn it.
type Charge struct {
	Kind           string  `json:"kind"`
	Class          string  `json:"class,omitempty"`
	Units          int64   `json:"units"`
	UnitUSDPerHour float64 `json:"unitUSDPerHour"`
	// Evidence cites the observation this line was read from — at minimum the
	// timeline point for the window edge, which is exactly the standard the
	// node terms already hold themselves to.
	Evidence []ID `json:"evidence"`
}

// NewCharge builds a validated, citable charge. It is the only constructor
// that can fail, and it fails on precisely the things [WhyCost] would refuse:
// an empty kind, a negative or out-of-range unit count, an unusable rate, and
// — the one this unit exists to make impossible — no parseable citation.
func NewCharge(kind, class string, units int64, unitUSDPerHour float64, evidence ...ID) (Charge, error) {
	c := Charge{Kind: kind, Class: class, Units: units, UnitUSDPerHour: unitUSDPerHour}
	if len(evidence) > 0 {
		c.Evidence = append([]ID(nil), evidence...)
	}
	if err := c.validate(); err != nil {
		return Charge{}, err
	}
	return c, nil
}

func (c Charge) key() ckey { return ckey{Kind: c.Kind, Class: c.Class} }

func (c Charge) validate() error {
	if strings.TrimSpace(c.Kind) == "" {
		return fmt.Errorf("explain: charge with no kind")
	}
	if c.Units < 0 || c.Units > maxChargeUnits {
		return fmt.Errorf("explain: charge %q unit count %d outside [0, %d]", c.key(), c.Units, maxChargeUnits)
	}
	if math.IsNaN(c.UnitUSDPerHour) || math.IsInf(c.UnitUSDPerHour, 0) || c.UnitUSDPerHour < 0 {
		return fmt.Errorf("explain: charge %q unit rate %v is not a usable price", c.key(), c.UnitUSDPerHour)
	}
	if _, err := MicroFromUSD(c.UnitUSDPerHour); err != nil {
		return err
	}
	if len(c.Evidence) == 0 {
		return fmt.Errorf("%w: %s", ErrUncitableCharge, c.key())
	}
	for _, id := range c.Evidence {
		if _, err := Parse(id); err != nil {
			return fmt.Errorf("%w: %s cites %q: %v", ErrUncitableCharge, c.key(), id, err)
		}
	}
	return nil
}

// validateCharges checks the charges dimension of a basis. It is called from
// [CostBasis.validate], so an uncitable charge fails the whole answer.
func (b *CostBasis) validateCharges() error {
	if len(b.Charges) > maxCharges {
		return fmt.Errorf("explain: %d charges exceeds the %d cap", len(b.Charges), maxCharges)
	}
	if len(b.Charges) > 0 && !b.ChargesKnown {
		// Rows without the flag would be silently ignored, and a silently
		// ignored line item is money that vanishes from both the terms and
		// the operator's attention.
		return fmt.Errorf("explain: %d charges supplied with ChargesKnown false; set the flag or drop the rows", len(b.Charges))
	}
	var total int64
	for _, c := range b.Charges {
		if err := c.validate(); err != nil {
			return err
		}
		total += c.Units
		if total > maxChargeUnits {
			return fmt.Errorf("explain: total charge units exceeds the %d cap", maxChargeUnits)
		}
	}
	return nil
}

// chargesKnown reports whether this edge stated its non-node charges. A nil
// basis states nothing; `ChargesKnown` with no rows states "there were none",
// which is a different and perfectly real claim (bug 1's lesson, applied to
// the second dimension).
func (b *CostBasis) chargesKnown() bool { return b != nil && b.ChargesKnown }

// chargeEdges folds both window edges' charges into canonical form and
// reports whether the dimension is attributable at all.
//
// It is attributable only when BOTH edges claim to know their charges. One
// edge alone is refused, loudly in Notes, because the alternative — reading
// the silent edge as zero — would report a cluster's entire Fargate bill as
// having appeared out of nothing or vanished into it. That is the exact shape
// of the lie this package is built to avoid: a large, confident, wrong number
// where an admission belonged. The whole charge move stays in the residual.
func chargeEdges(in Input, att *Attribution) (a, b *chargeComp, known bool, err error) {
	if a, err = newChargeComp(in.Start); err != nil {
		return nil, nil, false, err
	}
	if b, err = newChargeComp(in.End); err != nil {
		return nil, nil, false, err
	}
	switch {
	case in.Start.chargesKnown() && in.End.chargesKnown():
		return a, b, true, nil
	case in.Start.chargesKnown() || in.End.chargesKnown():
		edge := "start"
		if in.End.chargesKnown() {
			edge = "end"
		}
		att.Notes = append(att.Notes, fmt.Sprintf(
			"non-node charges are stated at the %s of the window only; a missing edge is not a zero, so no charge term is computed and the whole charge move stays in the residual", edge))
	}
	// Not attributable: hand back empty dimensions so nothing downstream can
	// accidentally price a half-known edge.
	empty, err := newChargeComp(nil)
	if err != nil {
		return nil, nil, false, err
	}
	return empty, empty, false, nil
}

// sum is the dimension's total, nil-safe so callers can add it to a fleet
// total without branching.
func (c *chargeComp) sum() Micro {
	if c == nil {
		return 0
	}
	return c.total
}

// modelSubject names what a composition note is comparing against the meter,
// so the note does not claim to have priced charges it never saw.
func modelSubject(edge string, chargesKnown bool) string {
	if chargesKnown {
		return edge + " composition and charges"
	}
	return edge + " composition"
}

// ckey identifies a charge line. Comparable, so it is a safe map key; the
// canonical order is (Kind, then Class).
type ckey struct {
	Kind  string
	Class string
}

func (k ckey) less(o ckey) bool {
	if k.Kind != o.Kind {
		return k.Kind < o.Kind
	}
	return k.Class < o.Class
}

func (k ckey) String() string {
	if k.Class == "" {
		return k.Kind
	}
	return k.Kind + "/" + k.Class
}

// chargeComp is a validated, canonically-ordered charges dimension — the
// exact analogue of comp, and deliberately built the same way so the two
// dimensions cannot drift apart.
type chargeComp struct {
	keys  []ckey // sorted; the ONLY order anything iterates in
	kinds []string
	u     map[ckey]int64
	r     map[ckey]Micro
	ev    map[ckey][]ID
	units int64
	total Micro // Σ u·r, computed in exact integer arithmetic
}

// newChargeComp folds a basis's charges into canonical form. Duplicate
// (kind, class) rows are merged with a unit-weighted average rate, for the
// same reason newComp merges duplicate node groups: two rows for one line is
// a collector detail and must not become two terms.
func newChargeComp(b *CostBasis) (*chargeComp, error) {
	c := &chargeComp{u: map[ckey]int64{}, r: map[ckey]Micro{}, ev: map[ckey][]ID{}}
	if b == nil {
		return c, nil
	}
	weighted := map[ckey]Micro{} // Σ u·r per line, exact
	for _, ch := range b.Charges {
		k := ch.key()
		unit, err := MicroFromUSD(ch.UnitUSDPerHour)
		if err != nil {
			return nil, err
		}
		cost, err := mul(unit, ch.Units)
		if err != nil {
			return nil, err
		}
		if _, seen := c.u[k]; !seen {
			c.keys = append(c.keys, k)
		}
		c.u[k] += ch.Units
		c.ev[k] = append(c.ev[k], ch.Evidence...)
		if weighted[k], err = add(weighted[k], cost); err != nil {
			return nil, err
		}
	}
	sort.Slice(c.keys, func(i, j int) bool { return c.keys[i].less(c.keys[j]) })
	for _, k := range c.keys {
		u := c.u[k]
		if u > 0 {
			// Integer division truncates; total below is recomputed from the
			// recovered rate, so the identity the terms satisfy is the one
			// the terms were built from (newComp does the same).
			c.r[k] = weighted[k] / Micro(u)
		}
		cost, err := mul(c.r[k], u)
		if err != nil {
			return nil, err
		}
		if c.total, err = add(c.total, cost); err != nil {
			return nil, err
		}
		c.units += u
		if n := len(c.kinds); n == 0 || c.kinds[n-1] != k.Kind {
			c.kinds = append(c.kinds, k.Kind)
		}
	}
	return c, nil
}

// rateAt returns this dimension's unit rate for k, falling back to the other
// edge when the line did not exist here. A line that appears mid-window has
// no rate change to report — the whole of its cost is a volume or mix change
// — so borrowing the other edge's rate is what makes charge-rate mean "rates
// moved" rather than "the line-item set moved".
//
// "Did not exist here" includes a supplied row with zero units: a
// unit-weighted average over zero units is not a rate, and treating a
// stated-but-unrun rate as observed would let charge-rate charge for a move
// nobody was paying either side of.
func (c *chargeComp) rateAt(k ckey, other *chargeComp) Micro {
	if r, ok := c.r[k]; ok {
		return r
	}
	return other.r[k]
}

// unionCKeys returns every line present in either dimension, canonically
// ordered.
func unionCKeys(a, b *chargeComp) []ckey {
	seen := map[ckey]bool{}
	out := make([]ckey, 0, len(a.keys)+len(b.keys))
	for _, src := range [][]ckey{a.keys, b.keys} {
		for _, k := range src {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

// unionKinds returns every charge kind present in either dimension, sorted.
func unionKinds(keys []ckey) []string {
	var out []string
	for _, k := range keys {
		if n := len(out); n == 0 || out[n-1] != k.Kind {
			out = append(out, k.Kind)
		}
	}
	return out
}

// kindAt returns (units, Σ u·rate) for one kind of composition c, priced at
// `rates`' rates, iterating in canonical key order.
func kindAt(c *chargeComp, keys []ckey, kind string, rates, other *chargeComp) (int64, float64) {
	var units int64
	var parts []float64
	for _, k := range keys {
		if k.Kind != kind {
			continue
		}
		units += c.u[k]
		parts = append(parts, float64(c.u[k])*float64(rates.rateAt(k, other)))
	}
	return units, sumSorted(parts)
}

// decomposeCharges peels charge-volume, charge-kind-mix, charge-class-mix and
// charge-rate off the charges change, in that order, and returns them as the
// sub-attributions of a single `charges` term whose value is the exact
// integer difference of the two supplied dimensions.
//
// The remainder — parent minus the four — is emitted as charge-unattributed
// so the identity `sum(Of) == parent` is exact by construction rather than by
// luck. In a correct decomposition it holds nothing but the three
// quantizations, and a value larger than that raises a note.
func decomposeCharges(a, b *chargeComp, anchors []ID, events []evidence.EvidenceEvent, att *Attribution) (Term, error) {
	keys := unionCKeys(a, b)
	kinds := unionKinds(keys)
	UA, UB := float64(a.units), float64(b.units)

	parent, err := sub(b.total, a.total)
	if err != nil {
		return Term{}, err
	}

	// charge-rate is exact integer arithmetic: end-of-window units times the
	// per-line rate move (Paasche).
	var rate Micro
	var moved []string
	rateMoved := map[ckey]bool{}
	for _, k := range keys {
		ra, rb := a.rateAt(k, b), b.rateAt(k, a)
		if ra == rb {
			continue
		}
		rateMoved[k] = true
		d, err := sub(rb, ra)
		if err != nil {
			return Term{}, err
		}
		part, err := mul(d, b.u[k])
		if err != nil {
			return Term{}, err
		}
		if rate, err = add(rate, part); err != nil {
			return Term{}, err
		}
		if b.u[k] > 0 {
			moved = append(moved, fmt.Sprintf("%s %s→%s", k, formatUSD(ra.USD()), formatUSD(rb.USD())))
		}
	}

	var rbarA float64
	var volume, kindMix, classMix Micro
	var volumeFacts []Fact
	kindMoved := map[string]bool{}
	classMoved := map[ckey]bool{}

	switch {
	case a.units == 0:
		// No prior mix to hold constant. Everything in the volume+mix bracket
		// is measured at the end mix and start rates and reported as volume
		// growth from nothing — the alternative would be a kind/class-mix
		// term describing a change from a line-item set that did not exist.
		var parts []float64
		for _, k := range keys {
			parts = append(parts, float64(b.u[k])*float64(a.rateAt(k, b)))
		}
		startPriced := sumSorted(parts)
		if volume, err = quantize(startPriced); err != nil {
			return Term{}, err
		}
		if b.units > 0 {
			rbarA = startPriced / UB
			att.Notes = append(att.Notes,
				"the window opens with no non-node charges, so there is no prior charge mix to hold constant: the whole charge change is reported as charge-volume and the mix terms are zero by construction")
		}
		volumeFacts = []Fact{
			{"fromUnits", "0"},
			{"toUnits", strconv.FormatInt(b.units, 10)},
			{"referenceUnitUSDPerHour", formatUSD(rbarA / MicroPerUSD)},
			{"note", "priced at the end-of-window charge mix; no start mix existed"},
		}
	default:
		rbarA = float64(a.total) / UA

		// 1. charge-volume: the unit change at the START mix and START rates.
		if volume, err = quantize((UB - UA) * rbarA); err != nil {
			return Term{}, err
		}

		var kindParts, classParts []float64
		for _, j := range kinds {
			uA, costA := kindAt(a, keys, j, a, b)
			uB, costBA := kindAt(b, keys, j, a, b)

			var sigA, sigB, qA, qBA float64
			if UA > 0 {
				sigA = float64(uA) / UA
			}
			if UB > 0 {
				sigB = float64(uB) / UB
			}
			if uA > 0 {
				qA = costA / float64(uA)
			}
			if uB > 0 {
				qBA = costBA / float64(uB)
			}
			// A kind absent at one edge has no average rate on that side.
			// The identity below holds for ANY finite value here — qA
			// cancels between the two terms — so the choice is purely about
			// which term tells the truth. Borrowing the other edge's average
			// (still at START rates) keeps "you moved a third of the fleet
			// onto Fargate" inside charge-kind-mix rather than splitting it
			// across kind-mix and an offsetting class-mix that describes no
			// class change at all.
			if uA == 0 {
				qA = qBA
			}
			if uB == 0 {
				qBA = qA
			}
			if sigA != sigB {
				kindMoved[j] = true
			}
			// 2. charge-kind-mix: kind shares move, within-kind class mix
			//    held at the start, rates held at the start.
			kindParts = append(kindParts, UB*(sigB-sigA)*qA)
			// 3. charge-class-mix: within-kind class mix moves, at the end
			//    kind shares and start rates.
			classParts = append(classParts, UB*sigB*(qBA-qA))

			for _, k := range keys {
				if k.Kind != j {
					continue
				}
				var wA, wB float64
				if uA > 0 {
					wA = float64(a.u[k]) / float64(uA)
				}
				if uB > 0 {
					wB = float64(b.u[k]) / float64(uB)
				}
				if wA != wB {
					classMoved[k] = true
				}
			}
		}
		if kindMix, err = quantize(sumSorted(kindParts)); err != nil {
			return Term{}, err
		}
		if classMix, err = quantize(sumSorted(classParts)); err != nil {
			return Term{}, err
		}
		volumeFacts = []Fact{
			{"fromUnits", strconv.FormatInt(a.units, 10)},
			{"toUnits", strconv.FormatInt(b.units, 10)},
			{"referenceUnitUSDPerHour", formatUSD(rbarA / MicroPerUSD)},
			{"source", "start charges (mix and rates held at the window start)"},
		}
	}

	chained, err := sumMicro([]Micro{volume, kindMix, classMix, rate})
	if err != nil {
		return Term{}, err
	}
	left, err := sub(parent, chained)
	if err != nil {
		return Term{}, err
	}
	if left > maxChargeQuantizationMicro || left < -maxChargeQuantizationMicro {
		// Three quantizations cannot exceed 1 µUSD between them, so anything
		// larger is a modelling error and must be visible rather than sitting
		// mute inside a sub-term nobody reads.
		att.Notes = append(att.Notes, fmt.Sprintf(
			"the charge chain leaves %d µUSD/h unattributed, more than the %d µUSD/h that quantization can explain; treat the charge terms as approximate",
			left, maxChargeQuantizationMicro))
	}

	unitsMoved := func(k ckey) bool { return a.u[k] != b.u[k] }
	subs := []Term{
		newTerm(TermChargeVolume, chargeVolumeLabel(b.units-a.units, rbarA), volume, volumeFacts,
			append(anchors, chargeEvidence(a, b, keys, unitsMoved)...)),
		newTerm(TermChargeKindMix, chargeKindMixLabel(kindMix), kindMix, chargeKindFacts(a, b, keys, kinds),
			append(anchors, chargeEvidence(a, b, keys, func(k ckey) bool { return kindMoved[k.Kind] })...)),
		newTerm(TermChargeClassMix, chargeClassMixLabel(classMix), classMix, chargeClassFacts(a, b, keys),
			append(anchors, chargeEvidence(a, b, keys, func(k ckey) bool { return classMoved[k] })...)),
		newTerm(TermChargeRate, chargeRateLabel(rate, len(moved)), rate, chargeRateFacts(moved),
			concatIDs(anchors, chargeEvidence(a, b, keys, func(k ckey) bool { return rateMoved[k] }),
				eventIDs(events, evidence.EventPricingChange))),
		newTerm(TermChargeUnattributed, chargeUnattributedLabel(left), left,
			[]Fact{{"method", "the charge change the chain above does not account for; in a correct decomposition this is the three quantizations and nothing else"}},
			anchors),
	}

	parentFacts := []Fact{
		{"fromUSDPerHour", formatUSD(a.total.USD())},
		{"toUSDPerHour", formatUSD(b.total.USD())},
		{"fromUnits", strconv.FormatInt(a.units, 10)},
		{"toUnits", strconv.FormatInt(b.units, 10)},
		{"fromLines", strconv.Itoa(len(a.keys))},
		{"toLines", strconv.Itoa(len(b.keys))},
		{"kinds", joinCapped(kinds, 8)},
		{"order", strings.Join(chargeChainOrder, " → ")},
	}
	parentTerm := newTerm(TermCharges, chargesLabel(parent, kinds), parent, parentFacts,
		concatIDs(anchors, chargeEvidence(a, b, keys, func(ckey) bool { return true })))
	// A charges dimension with no line at either edge is a real claim — "we
	// looked, there was nothing" — but it has no chain to show. Emitting four
	// zero sub-terms and a zero remainder would be five rows of ceremony
	// around a number that is zero for a stated reason.
	if len(keys) > 0 {
		parentTerm.Of = subs
	}
	return parentTerm, nil
}

// maxChargeQuantizationMicro bounds what charge-unattributed may hold in a
// correct decomposition. charge-volume, charge-kind-mix and charge-class-mix
// are each quantized once, losing under half a µUSD; charge-rate and the
// parent are exact integer arithmetic. Three half-µUSD errors sum to under
// 1.5, and the remainder is an integer, so 1 is the true ceiling — 2 is the
// same slack the node side allows itself (TestChargeQuantizationBoundMatches
// TheNodeSide fails if the two ever drift).
const maxChargeQuantizationMicro = Micro(2)

// chargeEvidence collects the citations of every line matching pred, from
// both edges, in canonical key order. newTerm sorts, de-duplicates and caps
// what it returns, so the order here is about determinism, not presentation.
func chargeEvidence(a, b *chargeComp, keys []ckey, pred func(ckey) bool) []ID {
	var out []ID
	for _, k := range keys {
		if !pred(k) {
			continue
		}
		out = append(out, a.ev[k]...)
		out = append(out, b.ev[k]...)
	}
	return out
}

// chargeKindFacts names each kind's unit share at both edges, in canonical
// order — the numbers the kind-mix term is computed from.
func chargeKindFacts(a, b *chargeComp, keys []ckey, kinds []string) []Fact {
	facts := make([]Fact, 0, len(kinds))
	for _, j := range kinds {
		uA, _ := kindAt(a, keys, j, a, b)
		uB, _ := kindAt(b, keys, j, a, b)
		facts = append(facts, Fact{j, fmt.Sprintf("%s → %s of units (%d → %d)",
			formatRatio(share(uA, a.units)), formatRatio(share(uB, b.units)), uA, uB)})
		if len(facts) == 8 {
			break
		}
	}
	return facts
}

// chargeClassFacts names the biggest movers in the class mix, largest
// absolute unit change first, ties broken by canonical key order.
func chargeClassFacts(a, b *chargeComp, keys []ckey) []Fact {
	type mover struct {
		k ckey
		d int64
	}
	var ms []mover
	for _, k := range keys {
		if d := b.u[k] - a.u[k]; d != 0 {
			ms = append(ms, mover{k, d})
		}
	}
	sort.SliceStable(ms, func(i, j int) bool {
		ai, aj := abs64(ms[i].d), abs64(ms[j].d)
		if ai != aj {
			return ai > aj
		}
		return ms[i].k.less(ms[j].k)
	})
	if len(ms) > 6 {
		ms = ms[:6]
	}
	facts := make([]Fact, 0, len(ms))
	for _, m := range ms {
		facts = append(facts, Fact{m.k.String(), fmt.Sprintf("%+d units", m.d)})
	}
	return facts
}

func chargeRateFacts(moved []string) []Fact {
	facts := []Fact{{"linesRepriced", strconv.Itoa(len(moved))}}
	if len(moved) > 0 {
		sort.Strings(moved)
		facts = append(facts, Fact{"moved", joinCapped(moved, 8)})
	}
	return facts
}

// --- labels. Template text over numbers computed above; every quantity in a
// label is already a field of the term it labels (see prose.go).

func chargesLabel(m Micro, kinds []string) string {
	if len(kinds) == 0 {
		return "no non-node charges at either window edge"
	}
	list := joinCapped(kinds, 3)
	switch {
	case m > 0:
		return "non-node charges (" + list + ") cost more"
	case m < 0:
		return "non-node charges (" + list + ") cost less"
	default:
		return "non-node charges (" + list + ") did not move the bill"
	}
}

func chargeVolumeLabel(deltaUnits int64, rbarMicro float64) string {
	switch {
	case deltaUnits > 0:
		return fmt.Sprintf("%d more charged %s at the window's starting average rate of %s/unit-hour",
			deltaUnits, plural(deltaUnits, "unit", "units"), formatUSD(rbarMicro/MicroPerUSD))
	case deltaUnits < 0:
		return fmt.Sprintf("%d fewer charged %s at the window's starting average rate of %s/unit-hour",
			-deltaUnits, plural(deltaUnits, "unit", "units"), formatUSD(rbarMicro/MicroPerUSD))
	default:
		return "charged unit count unchanged"
	}
}

func chargeKindMixLabel(m Micro) string {
	switch {
	case m < 0:
		return "the charge mix shifted toward cheaper kinds of charge"
	case m > 0:
		return "the charge mix shifted toward more expensive kinds of charge"
	default:
		return "the mix of charge kinds did not move the bill"
	}
}

func chargeClassMixLabel(m Micro) string {
	switch {
	case m < 0:
		return "charged units shifted toward cheaper configurations"
	case m > 0:
		return "charged units shifted toward more expensive configurations"
	default:
		return "the configuration mix did not move the bill"
	}
}

func chargeRateLabel(m Micro, lines int) string {
	if lines == 0 {
		return "no rate changed for any charge line still running"
	}
	dir := "rose"
	if m < 0 {
		dir = "fell"
	}
	if m == 0 {
		dir = "moved, netting out"
	}
	return fmt.Sprintf("charge rates %s for %d %s", dir, lines, plural(int64(lines), "line", "lines"))
}

func chargeUnattributedLabel(m Micro) string {
	if m == 0 {
		return "the charge chain accounts for every µUSD of the charge change"
	}
	if m <= maxChargeQuantizationMicro && m >= -maxChargeQuantizationMicro {
		return "rounding left over by the charge share arithmetic"
	}
	return "charge change the chain does not account for"
}
