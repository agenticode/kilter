package whatif

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/model"
)

// proposalFrom runs a real what-if over a trace and files the result, which is
// the full production path: replay → gate → proposal.
func proposalFrom(t *testing.T, tr *backtest.Trace, cand Policy, clock Clock) *Record {
	t.Helper()
	res, err := scenarioOver(t, tr, DefaultPolicy(), cand).Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	spec, err := res.Spec(Target{Cluster: tr.Cluster}, DefaultEnvelope(), DefaultTolerance(),
		"determinism fixture", []string{"ev:b", "ev:a"})
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	rec, err := NewStore().Create(Actor{Kind: ActorTuner, ID: "nightly"}, spec, clock)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return rec
}

func noisyTrace(t *testing.T) *backtest.Trace {
	t.Helper()
	return trace(t, backtest.TraceSpec{
		Kind: backtest.TraceRegimeChange, Days: 6, Workloads: 4,
		NoisePct: 0.09, NoiseSeed: 1234,
		OOMAt:    []time.Duration{20 * time.Hour, 70 * time.Hour},
		DeployAt: []time.Duration{10 * time.Hour, 60 * time.Hour},
	})
}

func TestProposalIsByteIdenticalForTheSameHistoryAndCandidate(t *testing.T) {
	tr := noisyTrace(t)
	cand := DefaultPolicy()
	cand.Rec.CPUHeadroom = 1.20

	first, err := proposalFrom(t, tr, cand, fixedClock()).Proposal().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got, err := proposalFrom(t, tr, cand, fixedClock()).Proposal().Encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("run %d produced different proposal bytes:\n%s\n---\n%s", i, first, got)
		}
	}
}

// TestShufflingHistoryDoesNotChangeTheProposal is the shuffle test the brief
// asks for. PR#27 shipped a real bug where float addition's non-associativity
// made totals depend on the order data arrived in; a proposal is a pile of
// money totals over a replay, so the same history in a different slice order
// must produce the same bytes.
func TestShufflingHistoryDoesNotChangeTheProposal(t *testing.T) {
	tr := noisyTrace(t)
	cand := DefaultPolicy()
	cand.Rec.MemoryHeadroom = 1.25

	want, err := proposalFrom(t, tr, cand, fixedClock()).Proposal().Encode()
	if err != nil {
		t.Fatal(err)
	}

	// Deterministic shuffles — a fixed permutation per round, no global RNG,
	// so a failure is reproducible.
	for round := 1; round <= 6; round++ {
		shuffled := permute(tr.Snapshots, round)
		if sameOrder(shuffled, tr.Snapshots) {
			t.Fatalf("round %d did not actually reorder anything", round)
		}
		alt := *tr
		alt.Snapshots = shuffled
		got, err := proposalFrom(t, &alt, cand, fixedClock()).Proposal().Encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("shuffle round %d changed the proposal:\n%s\n---\n%s", round, want, got)
		}
	}
}

// permute reorders a slice by a fixed stride, producing a different order per
// round without any randomness.
func permute(in []*model.ClusterSnapshot, round int) []*model.ClusterSnapshot {
	out := make([]*model.ClusterSnapshot, 0, len(in))
	stride := 2*round + 1
	for off := 0; off < stride && len(out) < len(in); off++ {
		for i := off; i < len(in); i += stride {
			out = append(out, in[i])
		}
	}
	return out
}

func sameOrder(a, b []*model.ClusterSnapshot) bool {
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

// TestTheFingerprintIgnoresTheClockButNothingElse: filing the same proposal a
// day later is the same proposal, so an approval bound to the fingerprint
// stays meaningful — but any change to the content is a different proposal.
func TestTheFingerprintIgnoresTheClockButNothingElse(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 5, Workloads: 2})
	cand := DefaultPolicy()
	cand.Rec.CPUHeadroom = 1.20

	a := proposalFrom(t, tr, cand, fixedClock()).Proposal()
	b := proposalFrom(t, tr, cand, FixedClock(testNow.Add(72*time.Hour))).Proposal()
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("the clock changed the proposal's identity")
	}
	if a.CreatedAt.Equal(b.CreatedAt) {
		t.Fatal("fixture is wrong: the two clocks must differ")
	}

	for _, tc := range []struct {
		name string
		mut  func(*Proposal)
	}{
		{"author", func(p *Proposal) { p.Author = Actor{Kind: ActorAgent, ID: "mallory"} }},
		{"target", func(p *Proposal) { p.Target.Namespace = "elsewhere" }},
		{"candidate policy", func(p *Proposal) { p.Candidate.Rec.CPUHeadroom = 1.30 }},
		{"gate verdict", func(p *Proposal) { p.Gate.Passed = !p.Gate.Passed }},
		{"scorecard hash", func(p *Proposal) { p.CandidateScore = "0000000000000000" }},
		{"regret", func(p *Proposal) { p.CandidateRegret = 0 }},
		{"rationale", func(p *Proposal) { p.Rationale = "trust me" }},
		{"evidence", func(p *Proposal) { p.EvidenceIDs = append(p.EvidenceIDs, "ev:z") }},
		{"window", func(p *Proposal) { p.Window[1] = p.Window[1].Add(time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := a.clone()
			tc.mut(&mutated)
			if mutated.Fingerprint() == a.Fingerprint() {
				t.Fatalf("changing the %s did not change the fingerprint", tc.name)
			}
		})
	}
}

// TestNoAmbientInputsInThePureLogic scans this package's non-test sources for
// the three ways a deterministic artifact stops being one: reading the clock,
// reading randomness, reading the environment. The clock is an argument
// (Clock), and this is what keeps it one.
func TestNoAmbientInputsInThePureLogic(t *testing.T) {
	banned := map[string]map[string]bool{
		"time": {"Now": true, "Since": true, "Until": true, "Tick": true, "After": true},
		"rand": {"": true}, // any use at all
		"os":   {"Getenv": true, "LookupEnv": true, "Environ": true, "ReadFile": true, "Open": true},
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing package: %v", err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				fns, watched := banned[id.Name]
				if !watched {
					return true
				}
				if fns[""] || fns[sel.Sel.Name] {
					t.Errorf("%s: %s.%s is an ambient input; take it as an argument instead",
						name, id.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}

// FuzzSumUSDIsOrderIndependent: the total is a function of the multiset.
func FuzzSumUSDIsOrderIndependent(f *testing.F) {
	f.Add(1.0, 2.0, 3.0, 4.0)
	f.Add(1e16, 1.0, -1e16, 1.0)
	f.Add(math.MaxFloat64, -math.MaxFloat64, 1.0, -1.0)
	f.Add(0.1, 0.2, 0.3, -0.6)

	f.Fuzz(func(t *testing.T, a, b, c, d float64) {
		orders := [][]float64{
			{a, b, c, d}, {d, c, b, a}, {b, d, a, c}, {c, a, d, b},
		}
		want := sumUSD(orders[0])
		for i, o := range orders[1:] {
			got := sumUSD(o)
			if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
				t.Fatalf("order %d summed to %v, want %v (inputs %v %v %v %v)",
					i+1, got, want, a, b, c, d)
			}
		}
		if math.IsNaN(want) || math.IsInf(want, 0) {
			t.Fatalf("sumUSD produced %v from %v %v %v %v; the total must always be "+
				"a finite, JSON-encodable number", want, a, b, c, d)
		}
	})
}

func TestSumUSDIsTotal(t *testing.T) {
	if got := sumUSD([]float64{1, math.NaN(), 2}); got != 3 {
		t.Fatalf("sumUSD dropped the wrong things: %v", got)
	}
	if got := sumUSD(nil); got != 0 {
		t.Fatalf("sumUSD(nil) = %v", got)
	}
	// The case the fuzzer found: +Inf and −Inf in one multiset sum to NaN,
	// which is neither order-stable nor JSON-encodable. Both are dropped.
	if got := sumUSD([]float64{math.Inf(1), 1, math.Inf(-1), 2}); got != 3 {
		t.Fatalf("mixed infinities leaked into the total: %v", got)
	}
	for _, in := range [][]float64{
		{math.Inf(1)}, {math.Inf(-1)}, {math.NaN()},
		{math.MaxFloat64, math.MaxFloat64},
	} {
		got := sumUSD(in)
		if math.IsNaN(got) {
			t.Fatalf("sumUSD(%v) = NaN", in)
		}
		if _, err := json.Marshal(map[string]float64{"total": got}); err != nil {
			t.Fatalf("sumUSD(%v) = %v, which cannot be encoded: %v", in, got, err)
		}
	}
}

func TestRound6AndPctChangeNeverProduceNonFinite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := round6(v); math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("round6(%v) = %v", v, got)
		}
		if got := pctChange(0, v); math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("pctChange(0,%v) = %v", v, got)
		}
		if got := pctChange(v, 1); math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("pctChange(%v,1) = %v", v, got)
		}
	}
	if got := pctChange(0, 5); got != 0 {
		t.Fatalf("pctChange from a zero baseline should be 0, got %v", got)
	}
	if got := pctChange(30, 20); got != round6(-100.0/3) {
		t.Fatalf("pctChange(30,20) = %v", got)
	}
}

// TestDeltaIsAFunctionOfTheScorecardsAlone: Diff must read nothing but its
// two arguments, so the same pair always yields the same delta.
func TestDeltaIsAFunctionOfTheScorecardsAlone(t *testing.T) {
	base := scoreFor(baselinePolicy(), nil)
	cand := scoreFor(candidatePolicy(), better)
	want := Diff(base, cand)
	for i := 0; i < 20; i++ {
		if got := Diff(base, cand); got != want {
			t.Fatalf("iteration %d: %+v != %+v", i, got, want)
		}
	}
	if Diff(nil, cand) != (Delta{}) || Diff(base, nil) != (Delta{}) {
		t.Fatal("a nil scorecard must yield the zero delta, not a panic or a lie")
	}
	if want.RegretUSD != -10 {
		t.Fatalf("RegretUSD delta = %v, want -10", want.RegretUSD)
	}
	if want.WindowHours != 168 {
		t.Fatalf("WindowHours = %v, want 168", want.WindowHours)
	}
	// 7 days of window, $10 of regret improvement → about $43.45/month.
	if wantMonthly := round6(-10 * (hoursPerMonth / 168)); want.ProjectedMonthlyUSD != wantMonthly {
		t.Fatalf("ProjectedMonthlyUSD = %v, want %v", want.ProjectedMonthlyUSD, wantMonthly)
	}
}

func TestRefusalCountingDoesNotDependOnMapOrder(t *testing.T) {
	sc := scoreFor(baselinePolicy(), func(s *backtest.Scorecard) {
		s.Refusals = map[string]int{
			backtest.CodeBelowChangeThreshold: 10,
			backtest.CodeBelowConfidence:      7,
			backtest.CodeModeGuarded:          3,
			backtest.CodePlanDropped:          1,
		}
	})
	want := countRefusals(sc)
	if want != 21 {
		t.Fatalf("countRefusals = %d, want 21", want)
	}
	for i := 0; i < 100; i++ {
		if got := countRefusals(sc); got != want {
			t.Fatalf("iteration %d: %d != %d", i, got, want)
		}
	}
	codes := sortedRefusalCodes(sc)
	for i := 1; i < len(codes); i++ {
		if codes[i] < codes[i-1] {
			t.Fatalf("refusal codes are not sorted: %v", codes)
		}
	}
}
