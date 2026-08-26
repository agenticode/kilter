package explain

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// bits is a deterministic decoder from a fuzz corpus entry to a structured
// input. It never fails: exhausted input yields zeros, which is itself an
// interesting shape (empty fleets, flat timelines).
type bits struct {
	b []byte
	i int
}

func (r *bits) byte() int {
	if r.i >= len(r.b) {
		return 0
	}
	v := int(r.b[r.i])
	r.i++
	return v
}

func (r *bits) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return r.byte() % n
}

func (r *bits) bool() bool { return r.byte()%2 == 1 }

// cents keeps generated prices exactly representable in µUSD, so the
// closed-basis assertion below measures the decomposition's own error and
// not the fixture's.
func (r *bits) cents(max int) float64 { return float64(r.intn(max)) / 100 }

var fuzzTypes = []string{"m5.large", "c6i.xlarge", "r6i.2xlarge", "t3.medium"}
var fuzzNamespaces = []string{"alpha", "beta", "gamma", "delta", "epsilon"}

func (r *bits) basis(at time.Time) *CostBasis {
	b := &CostBasis{At: at}
	seen := map[gkey]bool{}
	for n := r.intn(6); n > 0; n-- {
		k := gkey{Type: fuzzTypes[r.intn(len(fuzzTypes))], Spot: r.bool()}
		if seen[k] {
			continue // duplicate rows are covered by their own unit test
		}
		seen[k] = true
		b.Groups = append(b.Groups, NodeGroup{
			InstanceType: k.Type, Spot: k.Spot,
			Nodes:          int64(r.intn(400)),
			UnitUSDPerHour: r.cents(1000),
		})
	}
	seenNS := map[string]bool{}
	for n := r.intn(5); n > 0; n-- {
		ns := fuzzNamespaces[r.intn(len(fuzzNamespaces))]
		if seenNS[ns] {
			continue
		}
		seenNS[ns] = true
		b.Namespaces = append(b.Namespaces, NamespaceDemand{
			Namespace:   ns,
			MilliCPU:    int64(r.intn(200)) * 1000,
			MemoryBytes: int64(r.intn(200)) << 30,
			Pods:        int64(r.intn(50)),
		})
	}
	return b
}

// modelledTotal reprices a basis the way newComp does, for the closed-basis
// assertion. It only holds because the generator emits no duplicate group
// rows.
func modelledTotal(t *testing.T, b *CostBasis) Micro {
	t.Helper()
	var total Micro
	for _, g := range b.Groups {
		unit, err := MicroFromUSD(g.UnitUSDPerHour)
		if err != nil {
			t.Fatalf("MicroFromUSD: %v", err)
		}
		part, err := mul(unit, g.Nodes)
		if err != nil {
			t.Fatalf("mul: %v", err)
		}
		if total, err = add(total, part); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	return total
}

// maxQuantizationMicro bounds the residual a *closed* decomposition may show.
// Three of the four terms are quantized from float share arithmetic, each
// losing under half a µUSD, so the honest ceiling is 2 µUSD — a hundredth of
// a cent per month. Anything larger is a modelling error, not rounding, and
// this is the assertion that would catch it.
const maxQuantizationMicro = Micro(2)

// FuzzWhyCostInvariants is the acceptance property for this package: over
// arbitrary fleets, demands, ledgers and timelines, the terms plus the
// residual reconstruct ΔCost exactly, every sub-attribution reconstructs its
// parent exactly, every emitted term is citable, and the answer does not
// depend on the order anything arrived in.
func FuzzWhyCostInvariants(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{2, 0, 10, 3, 1, 0, 5, 40, 2, 1, 3, 9, 7, 1, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{3, 1, 200, 99, 0, 2, 100, 50, 1, 3, 40, 12, 4, 4, 4, 4, 9, 9, 9, 9})
	f.Add(make([]byte, 64))

	f.Fuzz(func(t *testing.T, data []byte) {
		r := &bits{b: data}
		from := t0
		to := t0.Add(hours(1 + r.intn(400)))
		closed := r.bool()
		start := r.basis(from)
		end := r.basis(to.Add(-time.Minute))

		var costA, costB float64
		var nodesA, nodesB int
		if closed {
			// The composition prices the observed cost exactly: the model is
			// complete, so the residual must be quantization only.
			ma, mb := modelledTotal(t, start), modelledTotal(t, end)
			costA, costB = ma.USD(), mb.USD()
			for _, g := range start.Groups {
				nodesA += int(g.Nodes)
			}
			for _, g := range end.Groups {
				nodesB += int(g.Nodes)
			}
		} else {
			costA, costB = r.cents(20000), r.cents(20000)
			nodesA, nodesB = r.intn(500), r.intn(500)
		}

		in := Input{
			Cluster: testCluster, From: from, To: to,
			Timeline: []evidence.TimelinePoint{
				point(from, costA, nodesA),
				point(from.Add(to.Sub(from)/2), (costA+costB)/2, (nodesA+nodesB)/2),
				point(to.Add(-time.Minute), costB, nodesB),
			},
			Start: start, End: end,
		}
		for n := r.intn(4); n > 0; n-- {
			in.Actions = append(in.Actions, LedgerAction{
				At:           from.Add(time.Duration(r.intn(int(to.Sub(from)/time.Hour)+1)) * time.Hour),
				Cluster:      testCluster,
				Fingerprint:  string(rune('a' + r.intn(26))),
				Mode:         "apply",
				Applied:      true,
				NodesRemoved: int64(r.intn(20)),
				NodesAdded:   int64(r.intn(5)),
			})
		}
		for n := r.intn(4); n > 0; n-- {
			in.Events = append(in.Events, ev(
				from.Add(time.Duration(r.intn(24))*time.Hour),
				[]string{evidence.EventPricingChange, evidence.EventSpotInterrupt, evidence.EventDeploy}[r.intn(3)],
				evidence.SeverityInfo,
				workloadSubject(fuzzNamespaces[r.intn(len(fuzzNamespaces))], "app"), nil))
		}

		a, err := WhyCost(in)
		if err != nil {
			// Every generated input is inside the documented bounds, so the
			// only acceptable failure is the deliberate "one point is not a
			// change" refusal — which this generator never produces.
			t.Fatalf("WhyCost: %v", err)
		}

		// 1. The invariant, exactly.
		sum, err := a.Sum()
		if err != nil {
			t.Fatalf("Sum: %v", err)
		}
		if sum != a.DeltaMicro {
			t.Fatalf("sum(terms)+residual = %d, ΔCost = %d (off by %d)", sum, a.DeltaMicro, sum-a.DeltaMicro)
		}
		// 2. Sub-attributions reconstruct their parent, exactly, one level deep.
		for _, term := range a.Terms {
			if len(term.Of) == 0 {
				continue
			}
			var subSum Micro
			for _, s := range term.Of {
				if len(s.Of) != 0 {
					t.Fatalf("sub-term %q nests further", s.Kind)
				}
				subSum += s.Micro
			}
			if subSum != term.Micro {
				t.Fatalf("sub-terms of %q sum to %d, parent is %d", term.Kind, subSum, term.Micro)
			}
		}
		// 3. Nothing uncitable ships.
		for _, term := range append(append([]Term(nil), a.Terms...), a.Residual) {
			if len(term.Evidence) == 0 {
				t.Fatalf("term %q carries no evidence id", term.Kind)
			}
			for _, s := range term.Of {
				if len(s.Evidence) == 0 {
					t.Fatalf("sub-term %q carries no evidence id", s.Kind)
				}
			}
			for _, id := range term.Evidence {
				if _, err := Parse(id); err != nil {
					t.Fatalf("term %q emitted an unparseable id %q: %v", term.Kind, id, err)
				}
			}
		}
		// 4. A complete model leaves only rounding behind.
		if closed && (a.Residual.Micro > maxQuantizationMicro || a.Residual.Micro < -maxQuantizationMicro) {
			t.Fatalf("closed basis left a residual of %d µUSD/h; the decomposition is incomplete, not merely rounded", a.Residual.Micro)
		}
		// 5. Determinism: reversing every input slice changes nothing.
		want, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rev := in
		rev.Timeline = reversedPoints(in.Timeline)
		rev.Events = reversedEvents(in.Events)
		rev.Actions = reversedActions(in.Actions)
		rev.Start = reversedBasis(in.Start)
		rev.End = reversedBasis(in.End)
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

func reversedPoints(in []evidence.TimelinePoint) []evidence.TimelinePoint {
	out := make([]evidence.TimelinePoint, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func reversedEvents(in []evidence.EvidenceEvent) []evidence.EvidenceEvent {
	out := make([]evidence.EvidenceEvent, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func reversedActions(in []LedgerAction) []LedgerAction {
	out := make([]LedgerAction, len(in))
	for i := range in {
		out[len(in)-1-i] = in[i]
	}
	return out
}

func reversedBasis(in *CostBasis) *CostBasis {
	if in == nil {
		return nil
	}
	out := &CostBasis{At: in.At}
	for i := len(in.Groups) - 1; i >= 0; i-- {
		out.Groups = append(out.Groups, in.Groups[i])
	}
	for i := len(in.Namespaces) - 1; i >= 0; i-- {
		out.Namespaces = append(out.Namespaces, in.Namespaces[i])
	}
	return out
}

// FuzzParseID asserts that ids are a closed, exactly-invertible language:
// nothing parses that this package could not have emitted, and everything it
// emits parses back to the coordinate it was built from.
func FuzzParseID(f *testing.F) {
	f.Add("evt/c/container/Deployment%2Fns%2Fapp%2Fweb/oomkill@1772323200000000000")
	f.Add("tl/c@0")
	f.Add("dig/1/c/container/k@1")
	f.Add("act/c/fp@1")
	f.Add("dec//workload/k@-1")
	f.Add("")
	f.Add("@")
	f.Add("%%%@1")

	f.Fuzz(func(t *testing.T, s string) {
		ref, err := Parse(ID(s))
		if err != nil {
			return
		}
		// A parseable id must be reconstructible from what it parsed to.
		var round ID
		switch ref.Kind {
		case KindEvent:
			round = EventID(evidence.EvidenceEvent{At: ref.At, Kind: ref.EventKind, Subject: ref.Subject})
		case KindDecision:
			round = DecisionID(evidence.DecisionRecord{At: ref.At, Subject: ref.Subject})
		case KindDigest:
			round = DigestID(ref.Subject, evidence.Digest{Start: ref.At, Tier: ref.Tier})
		case KindTimeline:
			round = TimelineID(ref.Cluster, evidence.TimelinePoint{At: ref.At})
		case KindAction:
			round = ActionID(LedgerAction{At: ref.At, Cluster: ref.Cluster, Fingerprint: ref.Token})
		default:
			t.Fatalf("Parse(%q) produced kind %q", s, ref.Kind)
		}
		again, err := Parse(round)
		if err != nil {
			t.Fatalf("re-parse of %q (from %q) failed: %v", round, s, err)
		}
		if again != ref {
			t.Fatalf("parse is not a fixed point: %q → %+v → %q → %+v", s, ref, round, again)
		}
	})
}

// FuzzMoney asserts the fixed-point boundary: conversions either produce an
// in-range Micro or an error, never a silently saturated or wrapped amount.
func FuzzMoney(f *testing.F) {
	f.Add(0.0, int64(0))
	f.Add(0.0928, int64(12))
	f.Add(-1.5, int64(-3))
	f.Add(math.MaxFloat64, int64(math.MaxInt64))
	f.Add(math.SmallestNonzeroFloat64, int64(1))

	f.Fuzz(func(t *testing.T, usd float64, n int64) {
		m, err := MicroFromUSD(usd)
		if err != nil {
			if m != 0 {
				t.Fatalf("MicroFromUSD(%v) returned %d alongside an error", usd, m)
			}
			return
		}
		if m > maxMicro || m < -maxMicro {
			t.Fatalf("MicroFromUSD(%v) = %d, outside ±%d", usd, m, maxMicro)
		}
		// Round-half-away-from-zero bounds the drift at exactly half a µUSD
		// (5e-7 USD). The magnitude-scaled slack covers the one ulp that
		// dividing by 1e6 costs on the way back, which is what inputs
		// landing precisely on the tie expose (-21.7078125 → -21707812.5
		// µUSD). Above ~$5e7 the slack exceeds half a µUSD, which is honest:
		// float64 cannot represent µUSD precision up there at all.
		tol := 5e-7 + math.Abs(usd)*1e-14
		if got := m.USD(); math.Abs(got-usd) > tol {
			t.Fatalf("MicroFromUSD(%v).USD() = %v, drifted by %v, tolerance %v", usd, got, math.Abs(got-usd), tol)
		}
		p, err := mul(m, n)
		if err != nil {
			return
		}
		if p > maxMicro || p < -maxMicro {
			t.Fatalf("mul(%d, %d) = %d, outside ±%d", m, n, p, maxMicro)
		}
		if n != 0 && p/Micro(n) != m {
			t.Fatalf("mul(%d, %d) = %d, which does not divide back", m, n, p)
		}
		q, err := quantize(float64(m))
		if err != nil {
			t.Fatalf("quantize of an in-range Micro failed: %v", err)
		}
		if q != m {
			t.Fatalf("quantize(%d) = %d", m, q)
		}
	})
}
