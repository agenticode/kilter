package reason

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/evidence"
)

// fiftyThousand builds §5.4's stated scale: 50k subjects with scores from a
// deterministic generator, so the fixture is a function of nothing but this
// file.
func fiftyThousand(n int) []Candidate {
	out := make([]Candidate, 0, n)
	seed := uint64(0x2545F4914F6CDD1D)
	for i := 0; i < n; i++ {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		out = append(out, Candidate{
			Subject: evidence.SubjectRef{
				Cluster: cluster,
				Kind:    evidence.SubjectContainer,
				Key:     "ns" + itoa(i%400) + "/Deployment/app" + itoa(i) + "/main",
			},
			Score: float64(seed%100_000) / 100,
			Note:  "class=bursty cv=1.8",
		})
	}
	return out
}

// permute reorders deterministically without touching the multiset.
func permute(in []Candidate) []Candidate {
	out := make([]Candidate, len(in))
	copy(out, in)
	for i := len(out) - 1; i > 0; i-- {
		j := (i*7919 + 104729) % (i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// TestTheSeedIsAFunctionOfTheCandidateSetAtFiftyThousandSubjects.
//
// At 50k workloads, context selection stops being a nicety. A seed built by
// sampling answers differently every run; a seed built by taking the first N
// answers about whichever namespace sorts first. This is the property that
// makes it neither: the ranking is a total order, so the same SET produces the
// same seed regardless of the order the caller's ranking pass emitted it in.
func TestTheSeedIsAFunctionOfTheCandidateSetAtFiftyThousandSubjects(t *testing.T) {
	cands := fiftyThousand(50_000)
	a, err := buildSeed(testScope(), cands, DefaultSeedStubs, defaultSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildSeed(testScope(), permute(cands), DefaultSeedStubs, defaultSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ab) != string(bb) {
		t.Fatalf("the arrival order of candidates leaked into the seed:\n%s\n%s", ab, bb)
	}
	if a.Considered != 50_000 {
		t.Fatalf("considered %d candidates", a.Considered)
	}
	if len(a.Stubs) != DefaultSeedStubs {
		t.Fatalf("the seed carries %d stubs, want %d", len(a.Stubs), DefaultSeedStubs)
	}
	if len(ab) > defaultSeedBytes {
		t.Fatalf("the seed is %d bytes, over the %d-byte context budget", len(ab), defaultSeedBytes)
	}
	for i := 1; i < len(a.Stubs); i++ {
		if a.Stubs[i-1].Score < a.Stubs[i].Score {
			t.Fatalf("stub %d outranks stub %d", i, i-1)
		}
	}
}

// TestTiedScoresAreBrokenBySubjectOrder. Without a tie-break the ranking is a
// partial order, and a partial order over 50k elements is a coin flip about
// which of two equally interesting workloads the model is told about.
func TestTiedScoresAreBrokenBySubjectOrder(t *testing.T) {
	var cands []Candidate
	for i := 0; i < 200; i++ {
		cands = append(cands, Candidate{
			Subject: evidence.SubjectRef{Cluster: cluster, Kind: evidence.SubjectContainer,
				Key: "ns/Deployment/app" + itoa(1000+i) + "/main"},
			Score: 7, // every score identical
		})
	}
	a, err := buildSeed(testScope(), cands, 5, defaultSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildSeed(testScope(), permute(cands), 5, defaultSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.Stubs {
		if a.Stubs[i] != b.Stubs[i] {
			t.Fatalf("stub %d differs under permutation: %+v vs %+v", i, a.Stubs[i], b.Stubs[i])
		}
	}
	if a.Stubs[0].Key != "ns/Deployment/app1000/main" {
		t.Fatalf("the tie-break is not subject order: first stub is %q", a.Stubs[0].Key)
	}
}

// TestAGarbageScoreCannotPromoteASubject. A NaN compares false against
// everything, which in a naive comparator sorts it wherever the algorithm
// happens to put it — including first.
func TestAGarbageScoreCannotPromoteASubject(t *testing.T) {
	cands := []Candidate{
		{Subject: evidence.SubjectRef{Cluster: cluster, Kind: "container", Key: "real"}, Score: 1},
		{Subject: evidence.SubjectRef{Cluster: cluster, Kind: "container", Key: "nan"}, Score: math.NaN()},
		{Subject: evidence.SubjectRef{Cluster: cluster, Kind: "container", Key: "inf"}, Score: math.Inf(1)},
	}
	got, err := buildSeed(testScope(), cands, 3, defaultSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stubs[len(got.Stubs)-1].Key != "nan" {
		t.Fatalf("a NaN-scored subject did not sort last: %+v", got.Stubs)
	}
	for _, s := range got.Stubs {
		if math.IsNaN(s.Score) || math.IsInf(s.Score, 0) {
			t.Fatalf("a non-finite score reached the seed: %+v", s)
		}
	}
}

// TestTheSeedIsCappedInBytesAndSaysHowMuchItCarries.
func TestTheSeedIsCappedInBytesAndSaysHowMuchItCarries(t *testing.T) {
	cands := fiftyThousand(1000)
	for i := range cands {
		cands[i].Note = strings.Repeat("n", 400) // longer than maxSeedNote
	}
	got, err := buildSeed(testScope(), cands, maxSeedStubs, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 2048 {
		t.Fatalf("the seed is %d bytes against a 2048-byte cap", len(b))
	}
	if got.Included != len(got.Stubs) {
		t.Fatalf("the seed reports %d included and carries %d", got.Included, len(got.Stubs))
	}
	if got.Included >= got.Considered {
		t.Fatal("the byte cap dropped nothing, so this fixture proves nothing")
	}
	for _, s := range got.Stubs {
		if len(s.Note) > maxSeedNote {
			t.Fatalf("a note of %d bytes survived the display cap", len(s.Note))
		}
	}
}

// TestCandidatesForOtherClustersAreNotSeeded.
func TestCandidatesForOtherClustersAreNotSeeded(t *testing.T) {
	cands := []Candidate{
		{Subject: evidence.SubjectRef{Cluster: "staging", Kind: "container", Key: "elsewhere"}, Score: 999},
		{Subject: evidence.SubjectRef{Cluster: cluster, Kind: "container", Key: "here"}, Score: 1},
		{Subject: evidence.SubjectRef{Kind: "", Key: ""}, Score: 500}, // not a subject at all
	}
	got, err := buildSeed(testScope(), cands, 10, defaultSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.Considered != 1 || len(got.Stubs) != 1 || got.Stubs[0].Key != "here" {
		t.Fatalf("the seed reached outside its scope: %+v", got)
	}
}

func BenchmarkSeedAtFiftyThousand(b *testing.B) {
	cands := fiftyThousand(50_000)
	sc := Scope{Cluster: cluster, From: t0, To: tEnd}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildSeed(sc, cands, DefaultSeedStubs, defaultSeedBytes); err != nil {
			b.Fatal(err)
		}
	}
}
