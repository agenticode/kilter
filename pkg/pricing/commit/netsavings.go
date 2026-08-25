package commit

import (
	"fmt"
	"math"
	"time"
)

// Reason codes for a suppressed recommendation. They are stable strings meant
// to be stored and matched on, unlike [Assessment.Reason] which is prose.
const (
	ReasonCommitmentNegative = "commitment-negative"
	ReasonCommitmentNeutral  = "commitment-neutral"
)

// Assessment is the verdict on one proposed change: what the bill actually
// does, and whether the recommendation survives it.
//
// Callers must claim [Assessment.ClaimableHourlyUSD], never GrossHourlyUSD.
// Gross is carried only so a UI can show the list-price fantasy beside the
// fact, and so the two can be compared in an audit.
type Assessment struct {
	Before Cost `json:"-"`
	After  Cost `json:"-"`

	// NetHourlyUSD is signed: positive is a saving, negative is a cost
	// increase. This is Bill(before) − Bill(after).
	NetHourlyUSD  float64 `json:"netHourlyUSD"`
	NetMonthlyUSD float64 `json:"netMonthlyUSD"`
	// GrossHourlyUSD is the on-demand list-price delta — the number a naive
	// optimizer reports. Net ≤ Gross whenever commitments are in play.
	GrossHourlyUSD  float64 `json:"grossHourlyUSD"`
	GrossMonthlyUSD float64 `json:"grossMonthlyUSD"`
	// StrandedHourlyUSD is the committed spend this change would newly waste.
	StrandedHourlyUSD float64 `json:"strandedHourlyUSD"`

	Suppressed bool   `json:"suppressed,omitempty"`
	ReasonCode string `json:"reasonCode,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// ValidFrom is the earliest date on which the blocking commitment set
	// changes — the first expiry among the commitments this change would
	// newly strand. A suppression lapses on its own: re-run NetSavings against
	// Inventory.Active(now) on or after this date and re-evaluate. It is zero
	// when nothing dated is doing the blocking.
	ValidFrom time.Time `json:"validFrom,omitempty"`
	// Conservative is true when either bill used the no-SP-rate fallback. The
	// net is then a floor: the real saving is this or better, never worse.
	Conservative bool `json:"conservative,omitempty"`
}

// ClaimableHourlyUSD is the only number a plan, an API response or a ledger
// entry may present as a saving. A suppressed recommendation claims nothing;
// so does a change whose net is negative.
func (a Assessment) ClaimableHourlyUSD() float64 {
	if a.Suppressed || a.NetHourlyUSD <= Eps {
		return 0
	}
	return a.NetHourlyUSD
}

// ClaimableMonthlyUSD projects ClaimableHourlyUSD onto a billing-average month.
func (a Assessment) ClaimableMonthlyUSD() float64 { return a.ClaimableHourlyUSD() * HoursPerMonth }

// NetSavings evaluates a proposed change as a bill delta.
//
// before and after are the SAME account-wide usage, once as observed and once
// with the recommendation applied. Passing only the affected lines is a
// correctness bug, not an optimization: Compute Savings Plans absorb usage
// account-wide, so a partial view understates absorption and overstates the
// saving.
//
// Suppression:
//   - net < 0 — the change raises the bill. ReasonCommitmentNegative (§4.4
//     ex.1, the +135 % case).
//   - net ≈ 0 while the list price promised a saving — the freed capacity is
//     entirely stranded, so the change buys risk for nothing.
//     ReasonCommitmentNeutral (§4.4 ex.2 and ex.3).
//
// Rate-availability harmonization: if either side priced a line under the
// conservative no-SP-rate fallback, BOTH sides re-price every such line that
// way. Otherwise a line with a known rate before and an unknown rate after
// would appear to drop to zero cost and manufacture a saving — the one way the
// fallback could over-claim. Lines are matched by [UsageLine.ID]; lines with
// an empty ID share one bucket, which is coarse but never optimistic.
func (inv *Inventory) NetSavings(before, after Usage) Assessment {
	b, a := inv.bill(before, nil), inv.bill(after, nil)
	if force := fallbackIDs(b, a); len(force) > 0 {
		b, a = inv.bill(before, force), inv.bill(after, force)
	}

	net := b.HourlyUSD - a.HourlyUSD
	gross := before.OnDemandHourlyUSD() - after.OnDemandHourlyUSD()
	as := Assessment{
		Before:            b,
		After:             a,
		NetHourlyUSD:      net,
		NetMonthlyUSD:     net * HoursPerMonth,
		GrossHourlyUSD:    gross,
		GrossMonthlyUSD:   gross * HoursPerMonth,
		StrandedHourlyUSD: math.Max(0, a.StrandedUSD-b.StrandedUSD),
		Conservative:      b.Fallback || a.Fallback,
	}
	as.ValidFrom = newlyStrandedExpiry(b, a)

	switch {
	case net < -Eps:
		as.Suppressed, as.ReasonCode = true, ReasonCommitmentNegative
		as.Reason = fmt.Sprintf(
			"commitment stranding: applying this raises the bill by $%.4f/h (list price suggested $%.4f/h of savings) "+
				"because $%.4f/h of committed capacity would no longer be absorbed",
			-net, gross, as.StrandedHourlyUSD)
	case net <= Eps && gross > Eps:
		as.Suppressed, as.ReasonCode = true, ReasonCommitmentNeutral
		as.Reason = fmt.Sprintf(
			"commitment stranding: the $%.4f/h list-price saving is fully absorbed by committed capacity "+
				"($%.4f/h newly stranded), so the realized saving is $%.4f/h",
			gross, as.StrandedHourlyUSD, math.Max(0, net))
	}
	if as.Suppressed {
		if as.ValidFrom.IsZero() {
			as.Reason += "; no expiry is known for the blocking commitment, so this will not lapse on its own"
		} else {
			as.Reason += fmt.Sprintf("; re-evaluate on %s, when the blocking commitment expires",
				as.ValidFrom.UTC().Format("2006-01-02"))
		}
	}
	if as.Conservative {
		as.Reason += conservativeNote(as.Suppressed)
	}
	return as
}

func conservativeNote(suppressed bool) string {
	s := "savings-plan rates were unavailable for part of this usage, so the net is a conservative floor"
	if suppressed {
		return " (" + s + ")"
	}
	return s
}

// fallbackIDs is the union of line IDs either bill priced under the
// conservative fallback.
func fallbackIDs(bills ...Cost) map[string]bool {
	var out map[string]bool
	for _, c := range bills {
		for _, cov := range c.Coverage {
			if !cov.Fallback {
				continue
			}
			if out == nil {
				out = map[string]bool{}
			}
			out[cov.LineID] = true
		}
	}
	return out
}

// newlyStrandedExpiry returns the earliest expiry among commitments the change
// strands more than before — the first date the arithmetic changes, and so the
// right date to re-evaluate on. Commitments with no expiry are ignored here;
// the caller reports that case in prose instead of inventing a date.
func newlyStrandedExpiry(before, after Cost) time.Time {
	was := make(map[string]float64, len(before.Commitments))
	for _, c := range before.Commitments {
		was[c.Kind+"/"+c.ID] += c.StrandedUSD()
	}
	var out time.Time
	for _, c := range after.Commitments { // slice iteration: order-independent min
		if c.Expires.IsZero() || c.StrandedUSD() <= was[c.Kind+"/"+c.ID]+Eps {
			continue
		}
		if out.IsZero() || c.Expires.Before(out) {
			out = c.Expires
		}
	}
	return out
}
