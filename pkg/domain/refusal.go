package domain

import (
	"sort"
	"strings"
	"time"
)

// Refusal is a target this domain declined to change, and why.
//
// # Why refusals need their own type
//
// A [Recommendation] cannot express one. [Recommendation.Validate] rejects a
// recommendation whose Proposed equals its Current — correctly, because a
// recommendation to change nothing is not a recommendation — so a domain that
// looked at a volume and concluded "gp3 would cost more here" has nothing to
// return. Every domain built so far discovered this independently and put its
// refusals in a package-local report type: pkg/ec2's Suppression, pkg/ebs's
// Refusal, pkg/ecs's Suppression, pkg/lambda's Suppression.
//
// Those reports are the most useful thing those domains produce. A fleet that
// has never been power-tuned yields nothing but `single-memory-point`
// refusals; an account with a Compute Savings Plan yields nothing but
// `commitment-neutral`. Rendering only [Recommendation]s would show a user an
// empty report and let them conclude the tool found nothing — which is a
// different claim from "the tool declined to guess, here is what it needs".
//
// So the seam carries them, and the aggregate report renders them beside the
// recommendations rather than under a debug flag.
//
// Refusals are distinct from a suppressed [Recommendation]: a suppression has
// a concrete alternative on the table that must not be applied, a refusal has
// no alternative at all.
type Refusal struct {
	Target TargetRef `json:"target"`
	// Code is a stable string safe to store, group and filter on — the
	// producing package's own reason code, forwarded unchanged.
	Code string `json:"code"`
	// Reason is the prose to show a human. Never empty.
	Reason string `json:"reason"`
	// ValidFrom dates a refusal that lapses on its own — a commitment expiry,
	// most often. Zero when nothing dated is doing the blocking.
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

// Refuser is the optional half of [Domain] for domains that can explain the
// targets they declined to touch. A domain that does not implement it is not
// broken — it simply has nothing to add beyond its recommendations.
type Refuser interface {
	// Refusals lists targets with no recommendation and the reason for each,
	// as of now. It is pure, like the rest of the seam.
	Refusals(now time.Time, ledger Netter) []Refusal
}

// SortRefusals orders refusals totally and deterministically.
func SortRefusals(rs []Refusal) {
	sort.SliceStable(rs, func(i, j int) bool {
		if c := rs[i].Target.Compare(rs[j].Target); c != 0 {
			return c < 0
		}
		if rs[i].Code != rs[j].Code {
			return rs[i].Code < rs[j].Code
		}
		return rs[i].Reason < rs[j].Reason
	})
}

// CodeCount is one reason code and how often it fired. Used for the
// "what we declined to do, and why" roll-up.
type CodeCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

// countCodes tallies codes into a canonically ordered slice: by descending
// count, then by code, so the output never depends on map iteration order.
func countCodes(codes []string) []CodeCount {
	if len(codes) == 0 {
		return nil
	}
	tally := make(map[string]int, len(codes))
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			tally[c]++
		}
	}
	if len(tally) == 0 {
		return nil
	}
	out := make([]CodeCount, 0, len(tally))
	for c, n := range tally {
		out = append(out, CodeCount{Code: c, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// mergeCodeCounts folds several tallies into one, canonically ordered.
func mergeCodeCounts(in ...[]CodeCount) []CodeCount {
	var flat []string
	for _, cs := range in {
		for _, c := range cs {
			for i := 0; i < c.Count; i++ {
				flat = append(flat, c.Code)
			}
		}
	}
	return countCodes(flat)
}

// Refusals asks a domain for its target-level refusals, returning nil when it
// does not implement [Refuser].
func (r *Registry) Refusals(k Kind, now time.Time, ledger Netter) []Refusal {
	d, ok := r.Get(k)
	if !ok {
		return nil
	}
	ref, ok := d.(Refuser)
	if !ok {
		return nil
	}
	out := ref.Refusals(now, ledger)
	for i := range out {
		out[i].Target.Domain = k
	}
	SortRefusals(out)
	return out
}
