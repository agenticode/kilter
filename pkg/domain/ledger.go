package domain

import (
	"sort"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Netter turns a proposed change into a bill delta.
//
// # Why this seam exists
//
// pkg/pricing/commit already implements the waterfall correctly, but its entry
// point — Inventory.NetSavings(before, after Usage) — requires the ACCOUNT-WIDE
// usage on both sides, and documents that passing only the affected lines is a
// correctness bug: Compute Savings Plans absorb usage account-wide, so a
// partial view understates absorption and overstates the saving.
//
// A domain knows only its own targets. It cannot construct account-wide usage,
// and it must not be tempted to try. So the domain hands over just the lines it
// changes, and whoever owns the account-wide view — the brain, which sees every
// registered domain's usage plus whatever `kilter pricing sync-commitments`
// collected — splices them into the whole picture. [Ledger] is that splice.
//
// A nil Netter means "no commitment information available". Net then equals
// gross, which is correct: with no known commitment, nothing can be stranded.
type Netter interface {
	// Net assesses replacing the `before` usage lines with the `after` lines.
	// Lines are matched to the account-wide baseline by UsageLine.ID.
	Net(before, after []commit.UsageLine) commit.Assessment
}

// Ledger nets a domain's proposed change against an account-wide usage baseline
// and a commitment inventory. It is pure: no clock, no I/O.
//
// The baseline is the account's observed hourly usage, ideally every line every
// domain reports. A missing or partial baseline is safe but pessimistic: less
// usage to absorb a commitment means more apparent stranding, so the assessment
// under-claims savings. It can never over-claim.
type Ledger struct {
	inv      *commit.Inventory
	baseline []commit.UsageLine
}

// NewLedger builds a ledger over an inventory and the account-wide baseline
// usage. A nil inventory is legal and yields on-demand billing throughout, i.e.
// net == gross.
func NewLedger(inv *commit.Inventory, baseline commit.Usage) *Ledger {
	lines := make([]commit.UsageLine, len(baseline.Lines))
	copy(lines, baseline.Lines)
	sortLines(lines)
	return &Ledger{inv: inv, baseline: lines}
}

// sortLines puts usage lines in a canonical order so a splice — and therefore
// every bill derived from it — is independent of the order lines were collected
// in. commit.Bill is already order-independent; this keeps the *inputs*
// reproducible too, which is what makes a checkpointed assessment comparable.
func sortLines(l []commit.UsageLine) {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].ID != l[j].ID {
			return l[i].ID < l[j].ID
		}
		if l[i].Kind != l[j].Kind {
			return l[i].Kind < l[j].Kind
		}
		return l[i].InstanceType < l[j].InstanceType
	})
}

// Net implements [Netter].
func (l *Ledger) Net(before, after []commit.UsageLine) commit.Assessment {
	var inv *commit.Inventory
	var base []commit.UsageLine
	if l != nil {
		inv, base = l.inv, l.baseline
	}
	if inv == nil {
		inv = &commit.Inventory{} // no commitments: bill everything on-demand
	}
	beforeUsage := commit.Usage{Lines: splice(base, before)}
	afterUsage := commit.Usage{Lines: splice(beforeUsage.Lines, after)}
	return inv.NetSavings(beforeUsage, afterUsage)
}

// splice overlays lines onto a baseline, matching by ID: a line whose ID is
// already present replaces it, a line whose ID is not present is appended.
//
// Lines with an empty ID never match and are always appended. That is coarse —
// two anonymous lines for the same resource would double-count — which is why
// every producer in this package stamps an ID.
func splice(base, over []commit.UsageLine) []commit.UsageLine {
	out := make([]commit.UsageLine, len(base), len(base)+len(over))
	copy(out, base)
	idx := make(map[string]int, len(base))
	for i, l := range base {
		if l.ID != "" {
			if _, dup := idx[l.ID]; !dup {
				idx[l.ID] = i
			}
		}
	}
	for _, l := range over {
		if l.ID != "" {
			if i, ok := idx[l.ID]; ok {
				out[i] = l
				continue
			}
			idx[l.ID] = len(out)
		}
		out = append(out, l)
	}
	return out
}
