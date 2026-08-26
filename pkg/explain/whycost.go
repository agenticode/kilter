package explain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// Term kinds. The first five are the top-level decomposition; the rest are
// sub-attributions of node-count (see the package doc for why they are not
// siblings of it).
const (
	TermNodeCount      = "node-count"
	TermSpotRatio      = "spot-ratio"
	TermInstanceMix    = "instance-mix"
	TermPricingCatalog = "pricing-catalog"
	TermResidual       = "residual"

	TermKilterAction    = "kilter-action"
	TermWorkloadSet     = "workload-set"
	TermWorkloadScaling = "workload-scaling"
	TermUnattributed    = "unattributed"
)

// chainOrder is the order the top-level terms are peeled off in, and it is
// part of the package's contract — see the "Attribution order" section of
// the package doc for why it is this order and not another.
var chainOrder = []string{TermNodeCount, TermSpotRatio, TermInstanceMix, TermPricingCatalog}

// Fact is one deterministic key/value detail behind a term. A slice, not a
// map, because the rendering order is part of the output and map iteration
// order is not a thing this package is allowed to have.
type Fact struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Term is one additive component of ΔCost, in µUSD per hour.
//
// Micro is authoritative. USDPerHour and MonthlyUSD are conveniences derived
// from it by division and multiplication respectively; they are never summed
// and never fed back into the arithmetic.
type Term struct {
	Kind       string  `json:"kind"`
	Label      string  `json:"label"`
	Micro      Micro   `json:"microUSDPerHour"`
	USDPerHour float64 `json:"usdPerHour"`
	MonthlyUSD float64 `json:"monthlyUSD"`
	Facts      []Fact  `json:"facts,omitempty"`
	Evidence   []ID    `json:"evidence"`
	// EvidenceTruncated counts ids the per-term cap dropped. Nothing is ever
	// dropped silently: a reader can always tell whether the citation list is
	// the whole set or a bounded sample of it.
	EvidenceTruncated int `json:"evidenceTruncated,omitempty"`
	// Of are sub-attributions that sum exactly to this term. Exactly one
	// level deep, always; a tree of unbounded depth is a tree nobody audits.
	Of []Term `json:"of,omitempty"`
}

func newTerm(kind, label string, m Micro, facts []Fact, ev []ID) Term {
	ids, dropped := dedupeIDs(ev, maxEvidencePerTerm)
	return Term{
		Kind: kind, Label: label, Micro: m,
		USDPerHour: m.USD(), MonthlyUSD: m.MonthlyUSD(),
		Facts: facts, Evidence: ids, EvidenceTruncated: dropped,
	}
}

// NodeGroup is one priced slice of the fleet: every node of one instance
// type on one capacity lifecycle, at one unit price.
type NodeGroup struct {
	InstanceType string `json:"instanceType"`
	Spot         bool   `json:"spot,omitempty"`
	Nodes        int64  `json:"nodes"`
	// UnitUSDPerHour is the price of ONE node in this group, not the group
	// total. Groups with the same (InstanceType, Spot) are merged by
	// node-weighted average price before any term is computed.
	UnitUSDPerHour float64 `json:"unitUSDPerHour"`
}

// NamespaceDemand is one namespace's requested capacity — the driver behind
// node count, and the only thing that lets the node-count term be split into
// "you added workloads" versus "your workloads grew".
type NamespaceDemand struct {
	Namespace   string `json:"namespace"`
	MilliCPU    int64  `json:"milliCPU"`
	MemoryBytes int64  `json:"memoryBytes"`
	Pods        int64  `json:"pods,omitempty"`
}

// CostBasis is the priced composition of one cluster at one instant: what
// the fleet was made of, and what it was being asked to run.
//
// nil means "not supplied"; a non-nil basis with no groups means "the fleet
// was empty at this edge", which is a different and perfectly real claim (a
// cluster created or torn down inside the window). Conflating the two would
// silently downgrade a complete answer into a residual, so WhyCost keys off
// nil-ness alone.
//
// Both edges are optional. Without them the answer degrades honestly — the
// node-count term still comes from the timeline, and everything else lands in
// the residual with a note saying why — rather than inventing a composition
// nobody observed.
type CostBasis struct {
	At         time.Time         `json:"at"`
	Groups     []NodeGroup       `json:"groups,omitempty"`
	Namespaces []NamespaceDemand `json:"namespaces,omitempty"`

	// Charges is the second dimension: cost billed per line item rather than
	// per node — Fargate pods above all, and anything else with the same
	// `Σ units × unit rate` shape. See charges.go for the decomposition and
	// its order. Groups deliberately excludes Fargate "nodes", so without
	// this dimension their whole cost lands in the residual.
	Charges []Charge `json:"charges,omitempty"`
	// ChargesKnown states that Charges is the complete non-node charge set at
	// this edge. It is a flag rather than a nil check because an empty slice
	// has to be able to mean "we looked, there were none" — the same
	// empty-is-not-missing distinction the nil-ness of *CostBasis draws for
	// the fleet. Charges are attributed only when BOTH edges set it: a
	// missing edge is not a zero, and treating it as one would report a
	// cluster's entire Fargate bill as having appeared or vanished.
	ChargesKnown bool `json:"chargesKnown,omitempty"`
}

func (b *CostBasis) supplied() bool { return b != nil }

// LedgerAction is one entry of Kilter's own audit ledger, projected into the
// shape the attribution needs.
//
// It is declared here rather than imported from pkg/api because pkg/api sits
// above every decision package; importing it would invert the dependency
// direction and drag the HTTP surface into the decomposition. FINDINGS.md
// specifies the field-by-field mapping the wiring must perform.
type LedgerAction struct {
	At          time.Time `json:"at"`
	Finished    time.Time `json:"finished,omitzero"`
	Cluster     string    `json:"cluster"`
	Fingerprint string    `json:"fingerprint"`
	Mode        string    `json:"mode"` // "apply" | "dry-run"
	Risk        string    `json:"risk,omitempty"`
	// Applied marks an entry that actually changed the cluster. Only applied
	// entries are attributed; a dry-run moved no money.
	Applied bool `json:"applied"`
	// NodesRemoved / NodesAdded count *confirmed* node lifecycle steps.
	NodesRemoved int64 `json:"nodesRemoved,omitempty"`
	NodesAdded   int64 `json:"nodesAdded,omitempty"`
	Resizes      int64 `json:"resizes,omitempty"`
	// Claimed money at execution time, straight from the ledger.
	CostBeforeHourlyUSD float64 `json:"costBeforeHourlyUSD,omitempty"`
	ProjectedHourlyUSD  float64 `json:"projectedHourlyUSD,omitempty"`
}

// Input is everything WhyCost is allowed to look at. There is no clock here
// on purpose: the window is an argument, so the same inputs give the same
// answer forever.
type Input struct {
	Cluster  string
	From, To time.Time
	// Timeline is the cluster's observed cost/node history, normally
	// evidence.Store.Timeline(cluster, from, to). It defines ΔCost: the
	// decomposition explains a measurement, it does not replace it.
	Timeline []evidence.TimelinePoint
	// Start and End describe the fleet composition at the window edges.
	Start, End *CostBasis
	// Events grounds terms in specific observations (pricing-change,
	// spot-interrupt, deploy). Events carried on Timeline points as overlay
	// are picked up automatically; this field is for anything else.
	Events []evidence.EvidenceEvent
	// Actions is the ledger, already filtered to this cluster or not — the
	// window and cluster filter is applied here.
	Actions []LedgerAction
}

// Attribution is the deterministic answer to "why did my cluster cost
// change". Terms and Residual are µUSD-per-hour amounts that satisfy
//
//	sum(Terms) + Residual == Delta
//
// exactly, in integer arithmetic, always.
type Attribution struct {
	Cluster     string    `json:"cluster"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	WindowHours float64   `json:"windowHours"`

	FromMicro  Micro `json:"fromMicroUSDPerHour"`
	ToMicro    Micro `json:"toMicroUSDPerHour"`
	DeltaMicro Micro `json:"deltaMicroUSDPerHour"`

	FromUSDPerHour  float64 `json:"fromUSDPerHour"`
	ToUSDPerHour    float64 `json:"toUSDPerHour"`
	DeltaUSDPerHour float64 `json:"deltaUSDPerHour"`
	DeltaMonthlyUSD float64 `json:"deltaMonthlyUSD"`

	FromNodes int64 `json:"fromNodes"`
	ToNodes   int64 `json:"toNodes"`

	// ChargesKnown reports that both window edges stated their non-node
	// charges, so the `charges` term below is a decomposition rather than a
	// gap in the residual. When it is false the three Charge* amounts are
	// meaningless and are omitted.
	ChargesKnown     bool  `json:"chargesKnown,omitempty"`
	ChargeFromMicro  Micro `json:"chargeFromMicroUSDPerHour,omitempty"`
	ChargeToMicro    Micro `json:"chargeToMicroUSDPerHour,omitempty"`
	ChargeDeltaMicro Micro `json:"chargeDeltaMicroUSDPerHour,omitempty"`

	Terms    []Term `json:"terms"`
	Residual Term   `json:"residual"`

	// Order is the attribution order actually used, echoed into the payload
	// so a stored answer states the convention it was computed under.
	Order []string `json:"order"`
	// ChargeOrder is the order inside the charges dimension, echoed for the
	// same reason. Empty when no charges were supplied at both edges.
	ChargeOrder []string `json:"chargeOrder,omitempty"`
	// Notes record every degradation, guard trip and disagreement between
	// the observed timeline and the supplied composition. An empty Notes is
	// a claim that nothing was approximated.
	Notes []string `json:"notes,omitempty"`
}

// Sum returns the sum of the top-level terms plus the residual. It must
// equal DeltaMicro; the property tests assert exactly that and nothing in
// the package is allowed to make it true by adjusting a term.
func (a *Attribution) Sum() (Micro, error) {
	vals := make([]Micro, 0, len(a.Terms)+1)
	for _, t := range a.Terms {
		vals = append(vals, t.Micro)
	}
	vals = append(vals, a.Residual.Micro)
	return sumMicro(vals)
}

// Input bounds. Every one of them exists so the money arithmetic below
// cannot overflow, and each is far above any real cluster.
const (
	maxGroups     = 4096
	maxNamespaces = 4096
	maxNodes      = 1 << 20 // per group and in total
	maxActions    = 4096
	maxPoints     = 1 << 20
)

// cancelGuard bounds how much cancellation the workload-set split tolerates.
// When added and surviving demand move in opposite directions their sum can
// be near zero while the parts are huge, and dividing by it turns rounding
// noise into a confident, enormous, wrong attribution. Above this ratio the
// split is refused and the whole node-count term stays unattributed — a
// visible "we do not know" instead of an invisible fabrication.
const cancelGuard = 4.0

func (in *Input) validate() error {
	if strings.TrimSpace(in.Cluster) == "" {
		return fmt.Errorf("explain: why-cost needs a cluster")
	}
	if in.From.IsZero() || in.To.IsZero() {
		return fmt.Errorf("explain: why-cost needs a bounded window (the window is an argument, never a clock)")
	}
	if !in.To.After(in.From) {
		return fmt.Errorf("explain: window [%v, %v) is empty or inverted", in.From, in.To)
	}
	if len(in.Timeline) > maxPoints {
		return fmt.Errorf("explain: %d timeline points exceeds the %d cap", len(in.Timeline), maxPoints)
	}
	if len(in.Actions) > maxActions {
		return fmt.Errorf("explain: %d ledger actions exceeds the %d cap", len(in.Actions), maxActions)
	}
	for _, b := range []*CostBasis{in.Start, in.End} {
		if b == nil {
			continue
		}
		if err := b.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (b *CostBasis) validate() error {
	if len(b.Groups) > maxGroups {
		return fmt.Errorf("explain: %d node groups exceeds the %d cap", len(b.Groups), maxGroups)
	}
	if len(b.Namespaces) > maxNamespaces {
		return fmt.Errorf("explain: %d namespaces exceeds the %d cap", len(b.Namespaces), maxNamespaces)
	}
	var total int64
	for _, g := range b.Groups {
		if g.InstanceType == "" {
			return fmt.Errorf("explain: node group with no instance type")
		}
		if g.Nodes < 0 || g.Nodes > maxNodes {
			return fmt.Errorf("explain: group %q node count %d outside [0, %d]", g.InstanceType, g.Nodes, maxNodes)
		}
		total += g.Nodes
		if total > maxNodes {
			return fmt.Errorf("explain: total node count exceeds the %d cap", maxNodes)
		}
		if math.IsNaN(g.UnitUSDPerHour) || math.IsInf(g.UnitUSDPerHour, 0) || g.UnitUSDPerHour < 0 {
			return fmt.Errorf("explain: group %q unit price %v is not a usable price", g.InstanceType, g.UnitUSDPerHour)
		}
		if _, err := MicroFromUSD(g.UnitUSDPerHour); err != nil {
			return err
		}
	}
	for _, ns := range b.Namespaces {
		if ns.Namespace == "" {
			return fmt.Errorf("explain: namespace demand with no namespace")
		}
		if ns.MilliCPU < 0 || ns.MemoryBytes < 0 || ns.Pods < 0 {
			return fmt.Errorf("explain: namespace %q has negative demand", ns.Namespace)
		}
	}
	return b.validateCharges()
}

// gkey identifies a node group. Comparable, so it is a safe map key; the
// canonical order is (InstanceType, then on-demand before spot).
type gkey struct {
	Type string
	Spot bool
}

func (k gkey) less(o gkey) bool {
	if k.Type != o.Type {
		return k.Type < o.Type
	}
	return !k.Spot && o.Spot
}

func (k gkey) String() string {
	if k.Spot {
		return k.Type + "/spot"
	}
	return k.Type + "/on-demand"
}

// comp is a validated, canonically-ordered composition.
type comp struct {
	keys  []gkey // sorted; the ONLY order anything iterates in
	n     map[gkey]int64
	p     map[gkey]Micro
	nodes int64
	total Micro // Σ n·p, computed in exact integer arithmetic
}

// newComp folds a basis into canonical form. Duplicate (type, lifecycle)
// groups are merged with a node-weighted average price, because two rows for
// one group is a collector detail and must not become two terms.
func newComp(b *CostBasis) (*comp, error) {
	c := &comp{n: map[gkey]int64{}, p: map[gkey]Micro{}}
	weighted := map[gkey]Micro{} // Σ n·p per group, exact
	for _, g := range b.Groups {
		k := gkey{Type: g.InstanceType, Spot: g.Spot}
		unit, err := MicroFromUSD(g.UnitUSDPerHour)
		if err != nil {
			return nil, err
		}
		cost, err := mul(unit, g.Nodes)
		if err != nil {
			return nil, err
		}
		if _, seen := c.n[k]; !seen {
			c.keys = append(c.keys, k)
		}
		c.n[k] += g.Nodes
		if weighted[k], err = add(weighted[k], cost); err != nil {
			return nil, err
		}
	}
	sort.Slice(c.keys, func(i, j int) bool { return c.keys[i].less(c.keys[j]) })
	for _, k := range c.keys {
		n := c.n[k]
		if n > 0 {
			// Integer division truncates; the ≤1 µUSD it can shave off a
			// merged unit price is recovered because `total` below is
			// recomputed from the recovered unit price, so the identity the
			// terms satisfy is the one the terms were built from.
			c.p[k] = weighted[k] / Micro(n)
		}
		cost, err := mul(c.p[k], n)
		if err != nil {
			return nil, err
		}
		if c.total, err = add(c.total, cost); err != nil {
			return nil, err
		}
		c.nodes += n
	}
	return c, nil
}

// priceAt returns this composition's unit price for k, falling back to the
// other side when the group did not exist here. A group that appears mid
// window has no price change to report — the whole of its cost is a mix
// change — so borrowing the other side's price is what makes the catalog
// term mean "prices moved" rather than "the fleet moved".
//
// "Did not exist here" includes a supplied row with zero nodes: a
// node-weighted average price over zero nodes is not a price, and treating a
// stated-but-unrun price as observed would let the catalog term charge for a
// move nobody was paying either side of.
func (c *comp) priceAt(k gkey, other *comp) Micro {
	if p, ok := c.p[k]; ok {
		return p
	}
	return other.p[k]
}

// unionKeys returns every group present in either composition, in canonical
// order.
func unionKeys(a, b *comp) []gkey {
	seen := map[gkey]bool{}
	out := make([]gkey, 0, len(a.keys)+len(b.keys))
	for _, k := range a.keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, k := range b.keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}

// lifecycleCost returns (nodes, Σ n·p) for one lifecycle under this
// composition's own prices, iterating in canonical key order.
func lifecycleCost(c *comp, spot bool) (int64, float64) {
	var nodes int64
	var parts []float64
	for _, k := range c.keys {
		if k.Spot != spot {
			continue
		}
		nodes += c.n[k]
		parts = append(parts, float64(c.n[k])*float64(c.p[k]))
	}
	return nodes, sumSorted(parts)
}

// WhyCost decomposes the cluster's hourly cost change over [From, To) into
// additive, individually-citable terms plus an honest residual.
//
// The unit is USD per hour — the run rate — not integrated spend over the
// window. See the package doc for why.
func WhyCost(in Input) (*Attribution, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	pts := pointsInWindow(in.Timeline, in.From, in.To)
	if len(pts) < 2 {
		return nil, fmt.Errorf("explain: window [%v, %v) holds %d timeline points; ΔCost needs two observations to be a measurement",
			in.From, in.To, len(pts))
	}
	start, end := pts[0], pts[len(pts)-1]
	startID := TimelineID(in.Cluster, start)
	endID := TimelineID(in.Cluster, end)
	anchors := []ID{startID, endID}

	cFrom, err := MicroFromUSD(start.CostUSDPerHour)
	if err != nil {
		return nil, err
	}
	cTo, err := MicroFromUSD(end.CostUSDPerHour)
	if err != nil {
		return nil, err
	}
	delta, err := sub(cTo, cFrom)
	if err != nil {
		return nil, err
	}

	att := &Attribution{
		Cluster:     in.Cluster,
		From:        in.From.UTC(),
		To:          in.To.UTC(),
		WindowHours: in.To.Sub(in.From).Hours(),
		FromMicro:   cFrom, ToMicro: cTo, DeltaMicro: delta,
		FromUSDPerHour: cFrom.USD(), ToUSDPerHour: cTo.USD(),
		DeltaUSDPerHour: delta.USD(), DeltaMonthlyUSD: delta.MonthlyUSD(),
		FromNodes: int64(start.Nodes), ToNodes: int64(end.Nodes),
		Order: append([]string(nil), chainOrder...),
	}

	events := gatherEvents(pts, in.Events, in.From, in.To)
	actions := actionsInWindow(in.Actions, in.Cluster, in.From, in.To)

	// The charges dimension is folded in first only so the composition notes
	// below can compare the *whole* modelled cost — nodes plus charges —
	// against the observed one. Its term is appended last, in chain order.
	chA, chB, chargesKnown, err := chargeEdges(in, att)
	if err != nil {
		return nil, err
	}

	var terms []Term
	var nodeTerm *Term
	// pbarA is the reference unit price (µUSD per node-hour) the node-count
	// term and every one of its sub-attributions are priced at.
	var pbarA float64
	var deltaNodes int64

	switch {
	case in.Start.supplied() && in.End.supplied():
		a, err := newComp(in.Start)
		if err != nil {
			return nil, err
		}
		b, err := newComp(in.End)
		if err != nil {
			return nil, err
		}
		if a.nodes != int64(start.Nodes) {
			att.Notes = append(att.Notes, fmt.Sprintf(
				"start composition holds %d nodes but the observed timeline point holds %d; the difference lands in the residual", a.nodes, start.Nodes))
		}
		if b.nodes != int64(end.Nodes) {
			att.Notes = append(att.Notes, fmt.Sprintf(
				"end composition holds %d nodes but the observed timeline point holds %d; the difference lands in the residual", b.nodes, end.Nodes))
		}
		// The modelled cost is the fleet plus whatever charges were supplied.
		// Comparing the fleet alone against an observed cost that includes
		// Fargate would report an unpriced gap that the charges dimension has
		// in fact just priced.
		modelA, err := add(a.total, chA.sum())
		if err != nil {
			return nil, err
		}
		modelB, err := add(b.total, chB.sum())
		if err != nil {
			return nil, err
		}
		if modelA != cFrom {
			att.Notes = append(att.Notes, fmt.Sprintf(
				"%s prices at %.6f USD/h but the observed cost is %.6f USD/h; the unpriced difference lands in the residual",
				modelSubject("start", chargesKnown), modelA.USD(), cFrom.USD()))
		}
		if modelB != cTo {
			att.Notes = append(att.Notes, fmt.Sprintf(
				"%s prices at %.6f USD/h but the observed cost is %.6f USD/h; the unpriced difference lands in the residual",
				modelSubject("end", chargesKnown), modelB.USD(), cTo.USD()))
		}
		composed, ref, err := decomposeComposition(a, b, anchors, events, att)
		if err != nil {
			return nil, err
		}
		terms = composed
		pbarA = ref
		deltaNodes = b.nodes - a.nodes
	default:
		att.Notes = append(att.Notes,
			"no fleet composition supplied for both window edges; only the node-count term is computable and the instance-mix, spot-ratio and pricing-catalog effects are inside the residual")
		if start.Nodes > 0 {
			pbarA = float64(cFrom) / float64(start.Nodes)
		}
		deltaNodes = int64(end.Nodes) - int64(start.Nodes)
		m, err := quantize(float64(deltaNodes) * pbarA)
		if err != nil {
			return nil, err
		}
		terms = []Term{newTerm(TermNodeCount, nodeCountLabel(deltaNodes, pbarA), m,
			[]Fact{
				{"fromNodes", strconv.Itoa(start.Nodes)},
				{"toNodes", strconv.Itoa(end.Nodes)},
				{"referenceUnitUSDPerHour", formatUSD(pbarA / MicroPerUSD)},
				{"source", "observed timeline (no composition supplied)"},
			}, anchors)}
	}

	for i := range terms {
		if terms[i].Kind == TermNodeCount {
			nodeTerm = &terms[i]
		}
	}
	if nodeTerm != nil {
		if err := attributeNodeCount(nodeTerm, pbarA, in, actions, events, anchors, att); err != nil {
			return nil, err
		}
	}

	// The charges dimension is the last link of the top-level chain, and it
	// is appended after attributeNodeCount so no pointer into `terms` is
	// invalidated. Its own chain lives in Term.Of; see charges.go.
	if chargesKnown {
		chargeTerm, err := decomposeCharges(chA, chB, anchors, events, att)
		if err != nil {
			return nil, err
		}
		// Recomputed here rather than read off the term: check() compares the
		// two, so a future change to how the parent is built has something to
		// disagree with.
		chargeDelta, err := sub(chB.total, chA.total)
		if err != nil {
			return nil, err
		}
		terms = append(terms, chargeTerm)
		att.Order = append(att.Order, TermCharges)
		att.ChargeOrder = append([]string(nil), chargeChainOrder...)
		att.ChargesKnown = true
		att.ChargeFromMicro, att.ChargeToMicro, att.ChargeDeltaMicro = chA.total, chB.total, chargeDelta
	}

	// Terms are emitted in chain order, and only when they carry a number or
	// a citation worth showing.
	att.Terms = terms
	sum, err := sumMicro(termMicros(terms))
	if err != nil {
		return nil, err
	}
	res, err := sub(delta, sum)
	if err != nil {
		return nil, err
	}
	att.Residual = newTerm(TermResidual, residualLabel(res), res,
		[]Fact{{"method", "ΔCost minus the sum of the terms above; never folded into a term"}}, anchors)

	if err := att.check(); err != nil {
		return nil, err
	}
	return att, nil
}

func termMicros(terms []Term) []Micro {
	out := make([]Micro, 0, len(terms))
	for _, t := range terms {
		out = append(out, t.Micro)
	}
	return out
}

// check re-verifies the package's central invariant on the finished payload,
// and that every emitted term is citable. A violation is a bug in this
// file, and it fails loudly here rather than shipping a wrong audit record.
func (a *Attribution) check() error {
	got, err := a.Sum()
	if err != nil {
		return err
	}
	if got != a.DeltaMicro {
		return fmt.Errorf("explain: BUG: terms sum to %d µUSD/h but ΔCost is %d µUSD/h", got, a.DeltaMicro)
	}
	for _, t := range append(append([]Term(nil), a.Terms...), a.Residual) {
		if len(t.Evidence) == 0 {
			return fmt.Errorf("explain: BUG: term %q carries no evidence id", t.Kind)
		}
		if len(t.Of) == 0 {
			continue
		}
		subs := make([]Micro, 0, len(t.Of))
		for _, s := range t.Of {
			if len(s.Evidence) == 0 {
				return fmt.Errorf("explain: BUG: sub-term %q of %q carries no evidence id", s.Kind, t.Kind)
			}
			if len(s.Of) != 0 {
				return fmt.Errorf("explain: BUG: sub-term %q of %q nests further", s.Kind, t.Kind)
			}
			subs = append(subs, s.Micro)
		}
		sum, err := sumMicro(subs)
		if err != nil {
			return err
		}
		if sum != t.Micro {
			return fmt.Errorf("explain: BUG: sub-terms of %q sum to %d µUSD/h, parent is %d µUSD/h", t.Kind, sum, t.Micro)
		}
	}
	return a.checkCharges()
}

// checkCharges enforces the charges dimension's own arithmetic identity, in
// the same place and the same way as the central one:
//
//	charges.Micro == ChargeDeltaMicro   (the exact repriced difference)
//
// The companion identity, sum(charges.Of) == charges.Micro, is enforced by
// the sub-term loop above — the charges chain is deliberately a
// sub-attribution so that it inherits it rather than reimplementing it.
//
// The first identity is what stops the second from being vacuous. Without it
// the parent could be any number at all and charge-unattributed would dutifully
// absorb the difference, which is precisely the failure this package exists to
// refuse: error hidden inside a confidently-labelled term instead of reported.
func (a *Attribution) checkCharges() error {
	var charges *Term
	for i := range a.Terms {
		if a.Terms[i].Kind == TermCharges {
			if charges != nil {
				return fmt.Errorf("explain: BUG: two %q terms in one attribution", TermCharges)
			}
			charges = &a.Terms[i]
		}
	}
	if !a.ChargesKnown {
		if charges != nil {
			return fmt.Errorf("explain: BUG: a %q term was emitted without a charges dimension at both window edges", TermCharges)
		}
		if a.ChargeDeltaMicro != 0 || a.ChargeFromMicro != 0 || a.ChargeToMicro != 0 {
			return fmt.Errorf("explain: BUG: charge amounts reported without a charges dimension at both window edges")
		}
		return nil
	}
	if charges == nil {
		return fmt.Errorf("explain: BUG: charges are known at both edges but no %q term was emitted", TermCharges)
	}
	stated, err := sub(a.ChargeToMicro, a.ChargeFromMicro)
	if err != nil {
		return err
	}
	if stated != a.ChargeDeltaMicro {
		return fmt.Errorf("explain: BUG: charge endpoints differ by %d µUSD/h but ChargeDelta is %d µUSD/h", stated, a.ChargeDeltaMicro)
	}
	if charges.Micro != a.ChargeDeltaMicro {
		return fmt.Errorf("explain: BUG: the %q term is %d µUSD/h but the supplied charges differ by %d µUSD/h",
			TermCharges, charges.Micro, a.ChargeDeltaMicro)
	}
	return nil
}

// decomposeComposition peels node-count, spot-ratio, instance-mix and
// pricing-catalog off the composition change, in that order. It returns the
// terms and the reference unit price (µUSD per node-hour) that node-count
// was measured at.
func decomposeComposition(a, b *comp, anchors []ID, events []evidence.EvidenceEvent, att *Attribution) ([]Term, float64, error) {
	keys := unionKeys(a, b)
	NA, NB := float64(a.nodes), float64(b.nodes)

	// pricing-catalog is exact integer arithmetic: node counts at the end of
	// the window times the per-group price move.
	var catalog Micro
	var moved []string
	for _, k := range keys {
		pa, pb := a.priceAt(k, b), b.priceAt(k, a)
		if pa == pb {
			continue
		}
		d, err := sub(pb, pa)
		if err != nil {
			return nil, 0, err
		}
		part, err := mul(d, b.n[k])
		if err != nil {
			return nil, 0, err
		}
		if catalog, err = add(catalog, part); err != nil {
			return nil, 0, err
		}
		if b.n[k] > 0 {
			moved = append(moved, fmt.Sprintf("%s %s→%s", k, formatUSD(pa.USD()), formatUSD(pb.USD())))
		}
	}

	var pbarA float64
	var nodeMicro, spotMicro, mixMicro Micro
	var nodeFacts []Fact

	switch {
	case a.nodes == 0:
		// No previous mix to hold constant. Everything in the volume+mix
		// bracket is measured at the end mix and start prices, and is
		// reported as node-count growth from nothing — the alternative would
		// be a spot/instance-mix term describing a change from a fleet that
		// did not exist.
		att.Notes = append(att.Notes,
			"the window opens with zero priced nodes, so there is no prior mix to hold constant: the whole volume change is reported as node-count and the mix terms are zero by construction")
		var parts []float64
		for _, k := range keys {
			parts = append(parts, float64(b.n[k])*float64(a.priceAt(k, b)))
		}
		m, err := quantize(sumSorted(parts))
		if err != nil {
			return nil, 0, err
		}
		nodeMicro = m
		if b.nodes > 0 {
			pbarA = sumSorted(parts) / NB
		}
		nodeFacts = []Fact{
			{"fromNodes", "0"},
			{"toNodes", strconv.FormatInt(b.nodes, 10)},
			{"referenceUnitUSDPerHour", formatUSD(pbarA / MicroPerUSD)},
			{"note", "priced at the end-of-window mix; no start mix existed"},
		}
	default:
		pbarA = float64(a.total) / NA

		// 1. node-count: the volume change at the START mix and START prices.
		m, err := quantize((NB - NA) * pbarA)
		if err != nil {
			return nil, 0, err
		}
		nodeMicro = m

		// Lifecycle aggregates, all at START prices.
		var sigmaA, sigmaB, qA, qBA [2]float64
		for i, spot := range [2]bool{false, true} {
			nA, costA := lifecycleCost(a, spot)
			if NA > 0 {
				sigmaA[i] = float64(nA) / NA
			}
			if nA > 0 {
				qA[i] = costA / float64(nA)
			}
			var nB int64
			var partsB []float64
			for _, k := range keys {
				if k.Spot != spot {
					continue
				}
				nB += b.n[k]
				partsB = append(partsB, float64(b.n[k])*float64(a.priceAt(k, b)))
			}
			if NB > 0 {
				sigmaB[i] = float64(nB) / NB
			}
			if nB > 0 {
				qBA[i] = sumSorted(partsB) / float64(nB)
			}
			// A lifecycle absent at the window start has no start-mix
			// average price. The identity below holds for ANY finite value
			// here — qA cancels between the two terms — so the choice is
			// purely about which term tells the truth. Borrowing the end
			// mix's average (still at START prices) keeps "you moved a third
			// of the fleet onto spot" inside spot-ratio; leaving it at zero
			// would credit spot-ratio with the full on-demand price of the
			// migrated nodes and then hand instance-mix the offsetting
			// "spot nodes cost something", which is not a mix change at all.
			if nA == 0 {
				qA[i] = qBA[i]
			}
			if nB == 0 {
				qBA[i] = qA[i]
			}
		}
		// 2. spot-ratio: lifecycle shares move, within-lifecycle mix held at
		//    the start, prices held at the start.
		spotParts := []float64{
			NB * (sigmaB[0] - sigmaA[0]) * qA[0],
			NB * (sigmaB[1] - sigmaA[1]) * qA[1],
		}
		if spotMicro, err = quantize(sumSorted(spotParts)); err != nil {
			return nil, 0, err
		}
		// 3. instance-mix: within-lifecycle mix moves, at the end lifecycle
		//    shares and start prices.
		mixParts := []float64{
			NB * sigmaB[0] * (qBA[0] - qA[0]),
			NB * sigmaB[1] * (qBA[1] - qA[1]),
		}
		if mixMicro, err = quantize(sumSorted(mixParts)); err != nil {
			return nil, 0, err
		}
		nodeFacts = []Fact{
			{"fromNodes", strconv.FormatInt(a.nodes, 10)},
			{"toNodes", strconv.FormatInt(b.nodes, 10)},
			{"referenceUnitUSDPerHour", formatUSD(pbarA / MicroPerUSD)},
			{"source", "start composition (mix and prices held at the window start)"},
		}
	}

	spotFacts := []Fact{
		{"fromSpotNodes", strconv.FormatInt(spotNodes(a), 10)},
		{"toSpotNodes", strconv.FormatInt(spotNodes(b), 10)},
		{"fromSpotShare", formatRatio(share(spotNodes(a), a.nodes))},
		{"toSpotShare", formatRatio(share(spotNodes(b), b.nodes))},
	}
	mixFacts := mixFactsFor(a, b, keys)
	catalogFacts := []Fact{{"groupsRepriced", strconv.Itoa(len(moved))}}
	if len(moved) > 0 {
		sort.Strings(moved)
		if len(moved) > 8 {
			catalogFacts = append(catalogFacts, Fact{"moved", strings.Join(moved[:8], ", ") + fmt.Sprintf(" (+%d more)", len(moved)-8)})
		} else {
			catalogFacts = append(catalogFacts, Fact{"moved", strings.Join(moved, ", ")})
		}
	}

	terms := []Term{
		newTerm(TermNodeCount, nodeCountLabel(b.nodes-a.nodes, pbarA), nodeMicro, nodeFacts, anchors),
		newTerm(TermSpotRatio, spotLabel(spotMicro), spotMicro, spotFacts,
			append(anchors, eventIDs(events, evidence.EventSpotInterrupt)...)),
		newTerm(TermInstanceMix, mixLabel(mixMicro), mixMicro, mixFacts, anchors),
		newTerm(TermPricingCatalog, catalogLabel(catalog, len(moved)), catalog, catalogFacts,
			append(anchors, eventIDs(events, evidence.EventPricingChange)...)),
	}
	return terms, pbarA, nil
}

func spotNodes(c *comp) int64 {
	n, _ := lifecycleCost(c, true)
	return n
}

func share(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// mixFactsFor names the biggest movers in the instance-type mix, in a
// deterministic order: largest absolute node-count change first, ties broken
// by canonical group order.
func mixFactsFor(a, b *comp, keys []gkey) []Fact {
	type mover struct {
		k gkey
		d int64
	}
	var ms []mover
	for _, k := range keys {
		if d := b.n[k] - a.n[k]; d != 0 {
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
		facts = append(facts, Fact{m.k.String(), fmt.Sprintf("%+d nodes", m.d)})
	}
	return facts
}

// attributeNodeCount splits the node-count term into the drivers that moved
// it: Kilter's own confirmed node actions first (they are counted, not
// inferred), then the demand split between namespaces that came and went and
// namespaces that grew, then whatever is left.
//
// The order matters and is documented in the package doc: a node Kilter is
// recorded as having deleted is a fact from the ledger, while the demand
// split is a proportional inference, so the fact is taken out first.
func attributeNodeCount(t *Term, pbarA float64, in Input,
	actions []LedgerAction, events []evidence.EvidenceEvent, anchors []ID, att *Attribution) error {

	var subs []Term

	var kilterNodes int64
	var actionIDs []ID
	var applied int
	for _, a := range actions {
		if !a.Applied {
			continue
		}
		applied++
		kilterNodes += a.NodesAdded - a.NodesRemoved
		actionIDs = append(actionIDs, ActionID(a))
	}
	actionIDs = append(actionIDs, eventIDs(events, evidence.EventKilterAction, evidence.EventActuationStep)...)
	if applied > 0 && len(actionIDs) > 0 {
		m, err := quantize(float64(kilterNodes) * pbarA)
		if err != nil {
			return err
		}
		facts := []Fact{
			{"appliedPlans", strconv.Itoa(applied)},
			{"nodesRemoved", strconv.FormatInt(sumRemoved(actions), 10)},
			{"nodesAdded", strconv.FormatInt(sumAdded(actions), 10)},
			{"resizes", strconv.FormatInt(sumResizes(actions), 10)},
		}
		if realized, ok := realizedDelta(in, actions); ok {
			facts = append(facts,
				Fact{"observedAcrossActionWindowsUSDPerHour", formatUSD(realized.USD())},
				Fact{"observedCaveat", "the observed move across the action windows, not an attribution: anything else that happened at the same time is inside it"})
		}
		subs = append(subs, newTerm(TermKilterAction, kilterActionLabel(kilterNodes, applied), m, facts, actionIDs))
	} else if applied > 0 {
		att.Notes = append(att.Notes, "applied ledger actions were supplied without citable identity; their effect stays unattributed")
	}

	// Demand split over what Kilter did not do.
	rest, err := sub(t.Micro, subMicros(subs))
	if err != nil {
		return err
	}
	if in.Start != nil && in.End != nil && len(in.Start.Namespaces) > 0 && len(in.End.Namespaces) > 0 {
		wsMicro, scaleMicro, dim, split, note := splitDemand(in.Start.Namespaces, in.End.Namespaces, rest)
		if note != "" {
			att.Notes = append(att.Notes, note)
		}
		if split {
			subs = append(subs,
				newTerm(TermWorkloadSet, workloadSetLabel(wsMicro, in.Start.Namespaces, in.End.Namespaces), wsMicro,
					append(namespaceFacts(in.Start.Namespaces, in.End.Namespaces), Fact{"demandDimension", dim}),
					append(anchors, deployIDsFor(events, changedNamespaces(in.Start.Namespaces, in.End.Namespaces))...)),
				newTerm(TermWorkloadScaling, workloadScalingLabel(scaleMicro), scaleMicro, nil,
					append(anchors, deployIDsFor(events, survivingNamespaces(in.Start.Namespaces, in.End.Namespaces))...)),
			)
		}
	} else if t.Micro != 0 {
		att.Notes = append(att.Notes, "no per-namespace demand supplied for both window edges; the node-count term is not split into workload-set and workload-scaling")
	}

	left, err := sub(t.Micro, subMicros(subs))
	if err != nil {
		return err
	}
	if len(subs) > 0 || left != 0 {
		subs = append(subs, newTerm(TermUnattributed, unattributedLabel(left), left,
			[]Fact{{"method", "the part of the node-count change no supplied driver accounts for"}}, anchors))
	}
	if len(subs) > 0 {
		t.Of = subs
	}
	return nil
}

func subMicros(subs []Term) Micro {
	var total Micro
	for _, s := range subs {
		total += s.Micro
	}
	return total
}

func sumRemoved(as []LedgerAction) int64 {
	var n int64
	for _, a := range as {
		if a.Applied {
			n += a.NodesRemoved
		}
	}
	return n
}

func sumAdded(as []LedgerAction) int64 {
	var n int64
	for _, a := range as {
		if a.Applied {
			n += a.NodesAdded
		}
	}
	return n
}

func sumResizes(as []LedgerAction) int64 {
	var n int64
	for _, a := range as {
		if a.Applied {
			n += a.Resizes
		}
	}
	return n
}

// realizedDelta measures what the cluster's observed cost actually did
// across the action windows. It is reported as context, never attributed:
// correlating a cost move with an action that happened at the same time is
// exactly the failure mode this design enumerates candidates to avoid.
func realizedDelta(in Input, actions []LedgerAction) (Micro, bool) {
	var total Micro
	var any bool
	for _, a := range actions {
		if !a.Applied {
			continue
		}
		fin := a.Finished
		if fin.IsZero() {
			fin = a.At
		}
		before, okB := pointAtOrBefore(in.Timeline, a.At)
		after, okA := pointAtOrAfter(in.Timeline, fin)
		if !okB || !okA || !after.At.After(before.At) {
			continue
		}
		b, err1 := MicroFromUSD(before.CostUSDPerHour)
		c, err2 := MicroFromUSD(after.CostUSDPerHour)
		if err1 != nil || err2 != nil {
			continue
		}
		d, err := sub(c, b)
		if err != nil {
			continue
		}
		if total, err = add(total, d); err != nil {
			continue
		}
		any = true
	}
	return total, any
}

// splitDemand apportions the node-count change Kilter did not cause between
// namespaces that appeared/disappeared and namespaces that grew/shrank.
//
// The measure is one dimension, not two: node count is driven by whichever
// of CPU or memory is binding, and adding millicores to bytes is not a
// quantity. The dimension with the larger *relative* total move over the
// window is chosen, ties going to CPU — a documented, deterministic rule
// rather than a per-cluster heuristic.
func splitDemand(from, to []NamespaceDemand, rest Micro) (ws, scale Micro, dim string, ok bool, note string) {
	a := demandByNamespace(from)
	b := demandByNamespace(to)

	cpuA, memA := totals(from)
	cpuB, memB := totals(to)
	relCPU := relMove(cpuA, cpuB)
	relMem := relMove(memA, memB)
	useCPU := relCPU >= relMem
	pick := func(d NamespaceDemand) float64 {
		if useCPU {
			return float64(d.MilliCPU)
		}
		return float64(d.MemoryBytes)
	}

	var addedParts, removedParts, survParts []float64
	for _, ns := range sortedNamespaces(b) {
		if _, in := a[ns]; !in {
			addedParts = append(addedParts, pick(b[ns]))
		}
	}
	for _, ns := range sortedNamespaces(a) {
		if _, in := b[ns]; !in {
			removedParts = append(removedParts, pick(a[ns]))
		}
	}
	for _, ns := range sortedNamespaces(a) {
		if _, in := b[ns]; in {
			survParts = append(survParts, pick(b[ns])-pick(a[ns]))
		}
	}
	dWS := sumSorted(addedParts) - sumSorted(removedParts)
	dSurv := sumSorted(survParts)
	dTotal := dWS + dSurv

	dim = "memory"
	if useCPU {
		dim = "cpu"
	}
	if dTotal == 0 {
		return 0, 0, dim, false, "total " + dim + " demand did not move across the window, so the node-count change cannot be apportioned between workload-set and workload-scaling"
	}
	if math.Abs(dWS)+math.Abs(dSurv) > cancelGuard*math.Abs(dTotal) {
		return 0, 0, dim, false, fmt.Sprintf(
			"workload-set and workload-scaling %s demand move in opposite directions and nearly cancel (|%.0f|+|%.0f| against a net of |%.0f|), so the split is refused rather than amplified",
			dim, dWS, dSurv, dTotal)
	}
	wsF := float64(rest) * (dWS / dTotal)
	scaleF := float64(rest) * (dSurv / dTotal)
	w, err1 := quantize(wsF)
	sc, err2 := quantize(scaleF)
	if err1 != nil || err2 != nil {
		return 0, 0, dim, false, "the workload demand split overflowed the money range and was refused"
	}
	return w, sc, dim, true, ""
}

func totals(ds []NamespaceDemand) (cpu, mem int64) {
	for _, d := range ds {
		cpu += d.MilliCPU
		mem += d.MemoryBytes
	}
	return cpu, mem
}

// relMove is |Δ| relative to the starting level, with a floor of 1 so a
// cluster that starts at zero in one dimension does not divide by zero.
func relMove(from, to int64) float64 {
	base := float64(from)
	if base < 1 {
		base = 1
	}
	return math.Abs(float64(to-from)) / base
}

func demandByNamespace(ds []NamespaceDemand) map[string]NamespaceDemand {
	m := make(map[string]NamespaceDemand, len(ds))
	for _, d := range ds {
		// Duplicate rows are summed, not last-wins: a collector that reports
		// a namespace twice must not silently halve the cluster.
		cur := m[d.Namespace]
		cur.Namespace = d.Namespace
		cur.MilliCPU += d.MilliCPU
		cur.MemoryBytes += d.MemoryBytes
		cur.Pods += d.Pods
		m[d.Namespace] = cur
	}
	return m
}

func sortedNamespaces(m map[string]NamespaceDemand) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func changedNamespaces(from, to []NamespaceDemand) map[string]bool {
	a, b := demandByNamespace(from), demandByNamespace(to)
	out := map[string]bool{}
	for ns := range a {
		if _, in := b[ns]; !in {
			out[ns] = true
		}
	}
	for ns := range b {
		if _, in := a[ns]; !in {
			out[ns] = true
		}
	}
	return out
}

func survivingNamespaces(from, to []NamespaceDemand) map[string]bool {
	a, b := demandByNamespace(from), demandByNamespace(to)
	out := map[string]bool{}
	for ns := range a {
		if _, in := b[ns]; in {
			out[ns] = true
		}
	}
	return out
}

func namespaceFacts(from, to []NamespaceDemand) []Fact {
	a, b := demandByNamespace(from), demandByNamespace(to)
	var added, removed []string
	for _, ns := range sortedNamespaces(b) {
		if _, in := a[ns]; !in {
			added = append(added, ns)
		}
	}
	for _, ns := range sortedNamespaces(a) {
		if _, in := b[ns]; !in {
			removed = append(removed, ns)
		}
	}
	facts := []Fact{
		{"namespacesAdded", strconv.Itoa(len(added))},
		{"namespacesRemoved", strconv.Itoa(len(removed))},
	}
	if len(added) > 0 {
		facts = append(facts, Fact{"added", joinCapped(added, 8)})
	}
	if len(removed) > 0 {
		facts = append(facts, Fact{"removed", joinCapped(removed, 8)})
	}
	return facts
}

func joinCapped(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" (+%d more)", len(items)-max)
}

// pointsInWindow filters and sorts the timeline. Sorting is not paranoia:
// the answer must not depend on the order the caller assembled its slice in.
func pointsInWindow(pts []evidence.TimelinePoint, from, to time.Time) []evidence.TimelinePoint {
	out := make([]evidence.TimelinePoint, 0, len(pts))
	for _, p := range pts {
		if p.At.Before(from) || !p.At.Before(to) {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func pointAtOrBefore(pts []evidence.TimelinePoint, t time.Time) (evidence.TimelinePoint, bool) {
	var best evidence.TimelinePoint
	var ok bool
	for _, p := range pts {
		if p.At.After(t) {
			continue
		}
		if !ok || p.At.After(best.At) {
			best, ok = p, true
		}
	}
	return best, ok
}

func pointAtOrAfter(pts []evidence.TimelinePoint, t time.Time) (evidence.TimelinePoint, bool) {
	var best evidence.TimelinePoint
	var ok bool
	for _, p := range pts {
		if p.At.Before(t) {
			continue
		}
		if !ok || p.At.Before(best.At) {
			best, ok = p, true
		}
	}
	return best, ok
}

// gatherEvents merges the timeline's own event overlay with any extra events
// the caller supplied, de-duplicated by citation id and returned in a total
// order.
func gatherEvents(pts []evidence.TimelinePoint, extra []evidence.EvidenceEvent, from, to time.Time) []evidence.EvidenceEvent {
	seen := map[ID]bool{}
	var out []evidence.EvidenceEvent
	add := func(ev evidence.EvidenceEvent) {
		if ev.At.Before(from) || !ev.At.Before(to) {
			return
		}
		id := EventID(ev)
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, ev)
	}
	for _, p := range pts {
		for _, ev := range p.Events {
			add(ev)
		}
	}
	for _, ev := range extra {
		add(ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return EventID(out[i]) < EventID(out[j]) })
	return out
}

func eventIDs(events []evidence.EvidenceEvent, kinds ...string) []ID {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	var out []ID
	for _, ev := range events {
		if want[ev.Kind] {
			out = append(out, EventID(ev))
		}
	}
	return out
}

// namespaceOf extracts the namespace an event's subject lives in. The
// explicit attr wins when a collector set one; otherwise it is the second
// segment of a WorkloadRef/ContainerKey string ("Kind/namespace/name[...]").
func namespaceOf(ev evidence.EvidenceEvent) string {
	if ns := ev.Attrs["namespace"]; ns != "" {
		return ns
	}
	switch ev.Subject.Kind {
	case evidence.SubjectWorkload, evidence.SubjectContainer:
		parts := strings.Split(ev.Subject.Key, "/")
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return ""
}

func deployIDsFor(events []evidence.EvidenceEvent, namespaces map[string]bool) []ID {
	var out []ID
	for _, ev := range events {
		if ev.Kind != evidence.EventDeploy {
			continue
		}
		if namespaces[namespaceOf(ev)] {
			out = append(out, EventID(ev))
		}
	}
	return out
}

func actionsInWindow(as []LedgerAction, cluster string, from, to time.Time) []LedgerAction {
	out := make([]LedgerAction, 0, len(as))
	for _, a := range as {
		if a.Cluster != cluster {
			continue
		}
		if a.At.Before(from) || !a.At.Before(to) {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out
}
