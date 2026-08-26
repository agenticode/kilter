package backtest

import (
	"bytes"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

var update = flag.Bool("update", false, "rewrite the golden scorecards in testdata/")

// TestSumSortedIsOrderIndependent is the unit-level guard for the failure
// mode pkg/ecs shipped this week: float addition is not associative, so the
// same values added in a different sequence can produce a different result.
// The test also demonstrates that a naive sum really does differ, so it is
// checking a live hazard rather than a hypothetical one.
func TestSumSortedIsOrderIndependent(t *testing.T) {
	// Classic catastrophic-cancellation set: the big terms annihilate, and
	// whether the small ones survive depends entirely on the order.
	vals := []float64{1e16, 1, 1, 1, -1e16, 0.5, -0.5, 1e16, -1e16}

	naive := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s
	}

	rng := rand.New(rand.NewSource(20260826))
	want := sumSorted(vals)
	naiveDiffers := false
	for i := 0; i < 200; i++ {
		perm := append([]float64(nil), vals...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		if got := sumSorted(perm); got != want {
			t.Fatalf("sumSorted changed with input order: %v vs %v (permutation %v)", got, want, perm)
		}
		if naive(perm) != naive(vals) {
			naiveDiffers = true
		}
	}
	if !naiveDiffers {
		t.Fatalf("the test data no longer exposes order-dependent summation; pick harder values")
	}
}

// TestScorecardIsByteIdenticalAcrossRuns: Go randomizes map iteration order
// on every range, so two runs in one process are a real test of whether any
// map order leaks into the output.
func TestScorecardIsByteIdenticalAcrossRuns(t *testing.T) {
	for _, kind := range allKinds {
		t.Run(string(kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 5,
				NoisePct: 0.12, NoiseSeed: 3, DeployAt: []time.Duration{0, 70 * time.Hour},
				OOMAt: []time.Duration{55 * time.Hour}})
			store := mustStore(t, tr)
			var first []byte
			for i := 0; i < 8; i++ {
				h := defaultPolicy().harness(tr)
				h.Evidence = store
				b, err := mustRun(t, h, tr).Encode()
				if err != nil {
					t.Fatal(err)
				}
				if first == nil {
					first = b
					continue
				}
				if !bytes.Equal(first, b) {
					t.Fatalf("run %d differs from run 0:\n--- run 0 ---\n%s\n--- run %d ---\n%s",
						i, first, i, b)
				}
			}
		})
	}
}

// TestScorecardIsByteIdenticalUnderShuffledHistory is the shuffle test the
// determinism requirement asks for: the same history presented in a
// different order — snapshots out of sequence, pods and usage entries
// permuted within each snapshot — must score identically. Anything that
// depends on enumeration order rather than on content fails here.
func TestScorecardIsByteIdenticalUnderShuffledHistory(t *testing.T) {
	for _, kind := range allKinds {
		t.Run(string(kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 5,
				NoisePct: 0.12, NoiseSeed: 11})
			store := mustStore(t, tr)

			h := defaultPolicy().harness(tr)
			h.Evidence = store
			want, err := mustRun(t, h, tr).Encode()
			if err != nil {
				t.Fatal(err)
			}

			for seed := int64(1); seed <= 5; seed++ {
				shuffled := shuffleTrace(tr, seed)
				sh := defaultPolicy().harness(shuffled)
				sh.Evidence = store
				got, err := mustRun(t, sh, shuffled).Encode()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(want, got) {
					t.Fatalf("shuffle seed %d changed the scorecard:\n--- ordered ---\n%s\n--- shuffled ---\n%s",
						seed, want, got)
				}
			}
		})
	}
}

// TestScoringIsByteIdenticalUnderShuffledRecords isolates the aggregation:
// given the same multiset of scored decisions in any order, score() must
// produce the same bytes. This is where an unordered float sum would show up
// directly, without the replay masking it.
func TestScoringIsByteIdenticalUnderShuffledRecords(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceRegimeChange, Start: propStart, Days: 9, Workloads: 6,
		NoisePct: 0.2, NoiseSeed: 42})
	h := defaultPolicy().harness(tr)
	h.Evidence = mustStore(t, tr)
	recs, err := h.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 40 {
		t.Fatalf("only %d records; the shuffle has too little to reorder", len(recs))
	}

	want, err := score(recs, DefaultCostModel(), 24*time.Hour, 7*24*time.Hour).Encode()
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(20260826))
	for i := 0; i < 40; i++ {
		perm := append([]record(nil), recs...)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		got, err := score(perm, DefaultCostModel(), 24*time.Hour, 7*24*time.Hour).Encode()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("permutation %d changed the scorecard:\n--- sorted ---\n%s\n--- shuffled ---\n%s",
				i, want, got)
		}
	}
}

// TestNoAmbientInputsInTheScoringPath enforces purity mechanically rather
// than by review: time in a backtest comes from the replayed history, so a
// call to time.Now() anywhere in the non-test sources would make a scorecard
// depend on when it was computed. The same goes for a random source or the
// environment. The check parses the sources and inspects call expressions, so
// naming a banned function in a comment (as the paragraph above does) is not
// a false positive.
func TestNoAmbientInputsInTheScoringPath(t *testing.T) {
	banned := map[string]map[string]bool{
		"time": {"Now": true, "Since": true, "Until": true},
		"rand": nil, // any selector: no ambient randomness at all
		"os":   {"Getenv": true, "LookupEnv": true, "Environ": true},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			fns, watched := banned[pkg.Name]
			if !watched {
				return true
			}
			if fns == nil || fns[sel.Sel.Name] {
				t.Errorf("%s calls %s.%s: a scorecard must be a pure function of the replayed history",
					fset.Position(call.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no sources were checked; the scan is broken")
	}
}

// TestGoldenScorecards pins the full output of every archetype. Run with
// -update to regenerate after a deliberate change; an accidental one shows up
// as a diff in review, which is the point.
func TestGoldenScorecards(t *testing.T) {
	for _, kind := range allKinds {
		t.Run(string(kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 2,
				DeployAt: []time.Duration{0}, OOMAt: []time.Duration{55 * time.Hour}})
			h := defaultPolicy().harness(tr)
			h.Evidence = mustStore(t, tr)
			got, err := mustRun(t, h, tr).Encode()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "scorecard_"+string(kind)+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `go test ./pkg/backtest -update` to create it)", err)
			}
			if !bytes.Equal(want, got) {
				t.Fatalf("golden mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
			}
		})
	}
}

// shuffleTrace deep-copies a trace and permutes every enumeration order the
// harness could accidentally depend on: the snapshot sequence, the pod list
// and the usage list inside each snapshot.
func shuffleTrace(tr *Trace, seed int64) *Trace {
	rng := rand.New(rand.NewSource(seed))
	snaps := make([]*model.ClusterSnapshot, 0, len(tr.Snapshots))
	for _, src := range tr.Snapshots {
		cp := *src
		cp.Pods = append([]model.PodSpec(nil), src.Pods...)
		cp.Usage = append([]model.Usage(nil), src.Usage...)
		cp.Workloads = append([]model.WorkloadInfo(nil), src.Workloads...)
		rng.Shuffle(len(cp.Pods), func(a, b int) { cp.Pods[a], cp.Pods[b] = cp.Pods[b], cp.Pods[a] })
		rng.Shuffle(len(cp.Usage), func(a, b int) { cp.Usage[a], cp.Usage[b] = cp.Usage[b], cp.Usage[a] })
		rng.Shuffle(len(cp.Workloads), func(a, b int) {
			cp.Workloads[a], cp.Workloads[b] = cp.Workloads[b], cp.Workloads[a]
		})
		snaps = append(snaps, &cp)
	}
	rng.Shuffle(len(snaps), func(a, b int) { snaps[a], snaps[b] = snaps[b], snaps[a] })

	out := *tr
	out.Snapshots = snaps
	return &out
}
