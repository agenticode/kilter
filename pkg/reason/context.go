package reason

import (
	"encoding/json"
	"math"

	"github.com/agenticode/kilter/pkg/evidence"
)

// Context assembly (§5.4). The model never sees the cluster; it sees case
// files. At 50k workloads that stops being a nicety and becomes a
// correctness property: a seed built by sampling gives a different answer
// every run, and a seed built by taking the first N gives an answer about
// whichever namespace sorts first.
//
// So the seed is computed by RANKING, with a total order, from a candidate
// list the deterministic engine supplies. This package does not compute
// savings, risk or anomaly scores — those are L1/L2's numbers, and inventing
// them here would be the exact inversion §1.2 forbids. It ranks what it is
// given and stops.

// Seed sizing. §5.4's default is 24 stubs at roughly 12 KiB.
const (
	DefaultSeedStubs = 24
	maxSeedStubs     = 64
	defaultSeedBytes = 12 << 10
	maxSeedNote      = 96
)

// Candidate is one ranked subject offered as seed context.
type Candidate struct {
	Subject evidence.SubjectRef
	// Score is the caller's relevance number — §5.4's
	// max(savingsUSD, riskScore, anomalyScore), or whatever the question
	// kind makes relevant. Higher is more relevant. Non-finite scores are
	// treated as the lowest possible, never as an error: a garbage score
	// must not be able to promote a subject.
	Score float64
	// Note is a one-line, already-deterministic summary from the engine. It
	// is cluster-adjacent text and is scrubbed and capped like everything
	// else that came from below.
	Note string
}

// seedStub is one line of the seed context.
type seedStub struct {
	Kind  string  `json:"kind"`
	Key   string  `json:"key"`
	Score float64 `json:"score"`
	Note  string  `json:"note,omitempty"`
}

// seedContext is the deterministic case-file index the first user message
// carries.
type seedContext struct {
	Cluster string `json:"cluster"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject,omitempty"`

	Considered int        `json:"considered"`
	Included   int        `json:"included"`
	Stubs      []seedStub `json:"subjects"`
	// Note is a constant explaining what the list is and is not.
	Note string `json:"note"`
}

const seedNote = "the highest-ranked subjects in scope, not all of them; use list_subjects and get_dossier to reach anything else"

// buildSeed ranks candidates and returns at most k stubs fitting maxBytes.
//
// The order is score descending, then (cluster, kind, key) ascending. That is
// total: no two distinct candidates compare equal, so the result is a
// function of the candidate SET and not of the order it arrived in — which is
// what makes the seed replayable when the caller's ranking pass changes its
// traversal.
//
// Selection is a bounded insert into a k-sized slice: O(n·k) comparisons with
// k ≤ 64, which at 50k subjects is a few million comparisons and no
// allocation, versus sorting 50k elements to keep 24.
func buildSeed(scope Scope, cands []Candidate, k, maxBytes int) (seedContext, error) {
	if k <= 0 {
		k = DefaultSeedStubs
	}
	if k > maxSeedStubs {
		k = maxSeedStubs
	}
	if maxBytes <= 0 {
		maxBytes = defaultSeedBytes
	}

	top := make([]Candidate, 0, k)
	considered := 0
	for _, c := range cands {
		if c.Subject.Cluster != "" && c.Subject.Cluster != scope.Cluster {
			continue
		}
		if c.Subject.Kind == "" || c.Subject.Key == "" {
			continue
		}
		considered++
		if math.IsNaN(c.Score) {
			c.Score = math.Inf(-1)
		}
		top = insertRanked(top, c, k)
	}

	sc := seedContext{
		Cluster:    scope.Cluster,
		From:       scope.From.UTC().Format(rfc3339),
		To:         scope.To.UTC().Format(rfc3339),
		Considered: considered,
		Note:       seedNote,
		Stubs:      []seedStub{},
	}
	if scope.Subject.Kind != "" {
		safe, _ := scrubText(scope.Subject.Kind+"/"+scope.Subject.Key, maxDisplayIdent)
		sc.Subject = safe
	}
	for _, c := range top {
		key, _ := scrubText(c.Subject.Key, maxDisplayIdent)
		note, _ := scrubText(c.Note, maxSeedNote)
		score := c.Score
		if math.IsInf(score, 0) || math.IsNaN(score) {
			score = 0
		}
		sc.Stubs = append(sc.Stubs, seedStub{Kind: c.Subject.Kind, Key: key, Score: score, Note: note})
	}

	// Byte cap: drop from the bottom, which is the least relevant end, and
	// report the count rather than the fact.
	for {
		sc.Included = len(sc.Stubs)
		b, err := json.Marshal(sc)
		if err != nil {
			return seedContext{}, err
		}
		if len(b) <= maxBytes || len(sc.Stubs) == 0 {
			return sc, nil
		}
		sc.Stubs = sc.Stubs[:len(sc.Stubs)-1]
	}
}

// insertRanked keeps the top k candidates in rank order.
func insertRanked(top []Candidate, c Candidate, k int) []Candidate {
	pos := len(top)
	for i := range top {
		if rankLess(c, top[i]) {
			pos = i
			break
		}
	}
	if pos >= k {
		return top
	}
	if len(top) < k {
		top = append(top, Candidate{})
	}
	copy(top[pos+1:], top[pos:])
	top[pos] = c
	return top
}

// rankLess is the total order: higher score first, then subject order.
func rankLess(a, b Candidate) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Subject.Cluster != b.Subject.Cluster {
		return a.Subject.Cluster < b.Subject.Cluster
	}
	if a.Subject.Kind != b.Subject.Kind {
		return a.Subject.Kind < b.Subject.Kind
	}
	return a.Subject.Key < b.Subject.Key
}

// rfc3339 is the one timestamp format this package emits.
const rfc3339 = "2006-01-02T15:04:05Z07:00"
