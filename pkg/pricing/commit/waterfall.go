package commit

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LineCoverage records how one usage line was paid for. The quantity fields
// partition Quantity: RIQty + EC2SPQty + ComputeSPQty + OnDemandQty == Quantity.
type LineCoverage struct {
	LineID       string  `json:"lineID,omitempty"`
	Quantity     float64 `json:"quantity"`
	RIQty        float64 `json:"riQty,omitempty"`
	EC2SPQty     float64 `json:"ec2spQty,omitempty"`
	ComputeSPQty float64 `json:"computeSPQty,omitempty"`
	OnDemandQty  float64 `json:"onDemandQty"`
	OnDemandUSD  float64 `json:"onDemandUSD"`
	// Fallback marks a line covered under the conservative no-rate rule
	// documented on [Inventory.Bill]: billed at zero marginal cost because its
	// Savings Plans rate was unknown.
	Fallback bool `json:"fallback,omitempty"`
}

// CommitmentUse is one commitment's utilization for the billed hour.
// CommittedUSD is charged regardless; CommittedUSD − UsedUSD is stranded.
type CommitmentUse struct {
	ID           string    `json:"id,omitempty"`
	Kind         string    `json:"kind"` // "reserved-instance" | "savings-plan"
	CommittedUSD float64   `json:"committedUSD"`
	UsedUSD      float64   `json:"usedUSD"`
	Expires      time.Time `json:"expires,omitempty"`
}

// StrandedUSD is the committed spend this hour bought nothing with.
func (c CommitmentUse) StrandedUSD() float64 { return math.Max(0, c.CommittedUSD-c.UsedUSD) }

// Cost is the outcome of the waterfall for one hour of usage: what the invoice
// says, not what a list-price subtraction wishes it said.
//
// Invariant: HourlyUSD == RICommittedUSD + SPCommittedUSD + OnDemandUSD.
// Invariant: HourlyUSD >= RICommittedUSD + SPCommittedUSD (the committed
// floor) — you cannot bill your way below what you already promised to pay.
type Cost struct {
	HourlyUSD float64 `json:"hourlyUSD"`

	RICommittedUSD float64 `json:"riCommittedUSD"` // charged whether used or not
	RIUsedUSD      float64 `json:"riUsedUSD"`      // the part usage actually absorbed
	SPCommittedUSD float64 `json:"spCommittedUSD"` // use-it-or-lose-it, this hour
	SPConsumedUSD  float64 `json:"spConsumedUSD"`
	OnDemandUSD    float64 `json:"onDemandUSD"`
	StrandedUSD    float64 `json:"strandedUSD"` // committed spend nothing absorbed

	// Fallback is true when any line was priced under the conservative
	// no-SP-rate rule. When it is set, HourlyUSD is a LOWER BOUND on the real
	// bill and must not be shown as an absolute cost — only NetSavings deltas
	// remain meaningful, and they under-claim by construction.
	Fallback bool `json:"fallback,omitempty"`

	// Coverage is in canonical order, not input order. Match by LineID.
	Coverage    []LineCoverage  `json:"coverage,omitempty"`
	Commitments []CommitmentUse `json:"commitments,omitempty"`
}

// MonthlyUSD projects the hourly bill onto a billing-average month.
func (c Cost) MonthlyUSD() float64 { return c.HourlyUSD * HoursPerMonth }

// lineState is a usage line mid-waterfall.
type lineState struct {
	line      UsageLine
	key       string
	unitsEach float64 // normalization units per instance-hour; 0 if unknown/non-EC2
	remaining float64 // quantity not yet covered
	cov       LineCoverage
}

// newStates sanitizes the lines and puts them in canonical order, which is
// what makes every downstream sum independent of input order.
func newStates(lines []UsageLine) []*lineState {
	out := make([]*lineState, 0, len(lines))
	for _, l := range lines {
		l.Quantity, l.ODRate = sane(l.Quantity), sane(l.ODRate)
		l.ComputeSPRate, l.EC2SPRate = sane(l.ComputeSPRate), sane(l.EC2SPRate)
		s := &lineState{line: l, key: l.canonicalKey(), remaining: l.Quantity}
		if l.Kind == KindEC2 {
			if u, ok := InstanceUnits(l.InstanceType); ok {
				s.unitsEach = u
			}
		}
		s.cov = LineCoverage{LineID: l.ID, Quantity: l.Quantity}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// canonicalRIs orders reservations soonest-expiring first.
//
// AWS does not document a consumption order within a pool of interchangeable
// reservations, and the order cannot change the bill (every reservation is
// charged in full either way). It does decide which reservation is reported as
// stranded, and therefore the ValidFrom date on a suppression. Consuming the
// soonest-expiring first leaves the longest-lived reservation holding the
// stranded units, so a suppression is dated by the blocker that actually
// outlasts the others — the conservative direction.
func (inv *Inventory) canonicalRIs() []ReservedInstance {
	if inv == nil {
		return nil
	}
	out := append([]ReservedInstance(nil), inv.RIs...)
	sort.SliceStable(out, func(i, j int) bool { return riKey(out[i]) < riKey(out[j]) })
	return out
}

func riKey(r ReservedInstance) string {
	exp := "9999-99-99"
	if !r.Expires.IsZero() {
		exp = r.Expires.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{exp, r.ID, r.InstanceType, r.Region, r.AZ,
		NormalizePlatform(r.Platform), NormalizeTenancy(r.Tenancy),
		strconv.Itoa(r.Count), strconv.FormatFloat(r.EffectiveHourlyUSD, 'g', 17, 64)}, "\x00")
}

func spKey(s SavingsPlan) string {
	exp := "9999-99-99"
	if !s.Expires.IsZero() {
		exp = s.Expires.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{exp, s.ID, string(s.Type), s.Region, s.Family,
		strconv.FormatFloat(s.CommitmentUSDPerHour, 'g', 17, 64)}, "\x00")
}

func (inv *Inventory) canonicalSPs(t SavingsPlanType) []SavingsPlan {
	if inv == nil {
		return nil
	}
	var out []SavingsPlan
	for _, s := range inv.SavingsPlans {
		if s.Type == t {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return spKey(out[i]) < spKey(out[j]) })
	return out
}

// Bill prices one hour of usage through the commitment waterfall and returns
// what the invoice would say. This is the only honest way to price usage in
// Kilter, and every savings claim must eventually be a difference of two
// Bill results — see [Inventory.NetSavings].
//
// Order (verified against apply_ri.html and sp-applying.html):
//
//  1. zonal RIs, exact match on AZ + instance type + platform + tenancy;
//  2. regional RIs — AZ-flexible; size-flexible within the family via
//     normalization units, applied smallest instance first (a regional RI on a
//     non-flexible platform, dedicated tenancy or an excluded family still
//     applies, but only on an exact size match);
//  3. EC2 Instance Savings Plans, pooled per (region, family);
//  4. Compute Savings Plans, pooled, applied to the highest savings percentage
//     first, ties broken by the lower Savings Plans rate;
//  5. whatever is left, at on-demand rates.
//
// # Conservative fallback when Savings Plans rates are unavailable
//
// SP rates per usage type come from an AWS API Kilter may not be able to call
// (§4.4). When a line is Savings-Plan-eligible, a plan exists that could cover
// it, and its rate for that plan type is unknown (≤ 0), the line is billed at
// ZERO MARGINAL COST and consumes NONE of the commitment — i.e. the commitment
// is assumed fully stranded. Both the before-bill and the after-bill treat it
// that way, so a change confined to such lines nets exactly $0. The rule
// under-claims savings by construction and can never invent them. The price of
// that safety is that [Cost.HourlyUSD] is then only a lower bound on the real
// bill; [Cost.Fallback] says so.
//
// Bill never fails. Non-finite or negative inputs clamp to zero rather than
// poisoning a total; call [Usage.Validate] and [Inventory.Validate] at the
// boundary if you want to hear about bad data.
func (inv *Inventory) Bill(u Usage) Cost { return inv.bill(u, nil) }

// bill is Bill with an optional set of line IDs forced onto the conservative
// fallback path. NetSavings uses it to harmonize rate availability across a
// before/after pair.
func (inv *Inventory) bill(u Usage, force map[string]bool) Cost {
	states := newStates(u.Lines)
	var cost Cost

	// (1) zonal RIs, then (2) regional RIs. Both loops walk a sorted slice.
	ris := inv.canonicalRIs()
	for _, zonal := range []bool{true, false} {
		for _, ri := range ris {
			if ri.Zonal() != zonal {
				continue
			}
			capUnits, usedUnits := applyRI(ri, states)
			committed := float64(ri.Count) * sane(ri.EffectiveHourlyUSD)
			used := 0.0
			if capUnits > Eps {
				used = committed * (usedUnits / capUnits)
			}
			cost.RICommittedUSD += committed
			cost.RIUsedUSD += used
			cost.Commitments = append(cost.Commitments, CommitmentUse{
				ID: ri.ID, Kind: "reserved-instance",
				CommittedUSD: committed, UsedUSD: used, Expires: ri.Expires,
			})
		}
	}

	// (3) EC2 Instance SPs, pooled per (region, family) — AWS applies current
	// plans "grouped together". Disjoint scopes make the group order
	// irrelevant to totals; it is still fixed, for attribution.
	ec2SPs := inv.canonicalSPs(SPEC2Instance)
	for _, group := range groupEC2SPs(ec2SPs) {
		region, family := group[0].Region, group[0].Family
		eligible := func(s *lineState) bool {
			return s.line.Kind == KindEC2 && !s.line.SPIneligible &&
				strings.EqualFold(s.line.Region, region) &&
				strings.EqualFold(s.line.Family(), family)
		}
		rate := func(l UsageLine) float64 { return l.EC2SPRate }
		consumed, fb := applySPPool(states, group, force, eligible, rate, coverEC2SP)
		cost.Fallback = cost.Fallback || fb
		for i, sp := range group {
			cost.SPCommittedUSD += sane(sp.CommitmentUSDPerHour)
			cost.SPConsumedUSD += consumed[i]
			cost.Commitments = append(cost.Commitments, CommitmentUse{
				ID: sp.ID, Kind: "savings-plan",
				CommittedUSD: sane(sp.CommitmentUSDPerHour), UsedUSD: consumed[i], Expires: sp.Expires,
			})
		}
	}

	// (4) Compute SPs, pooled account-wide across EC2, Fargate and Lambda.
	if compute := inv.canonicalSPs(SPCompute); len(compute) > 0 {
		eligible := func(s *lineState) bool {
			return !s.line.SPIneligible &&
				(s.line.Kind == KindEC2 || s.line.Kind == KindFargate || s.line.Kind == KindLambda)
		}
		rate := func(l UsageLine) float64 { return l.ComputeSPRate }
		consumed, fb := applySPPool(states, compute, force, eligible, rate, coverComputeSP)
		cost.Fallback = cost.Fallback || fb
		for i, sp := range compute {
			cost.SPCommittedUSD += sane(sp.CommitmentUSDPerHour)
			cost.SPConsumedUSD += consumed[i]
			cost.Commitments = append(cost.Commitments, CommitmentUse{
				ID: sp.ID, Kind: "savings-plan",
				CommittedUSD: sane(sp.CommitmentUSDPerHour), UsedUSD: consumed[i], Expires: sp.Expires,
			})
		}
	}

	// (5) the remainder, at on-demand rates. states is in canonical order, so
	// this sum — and therefore HourlyUSD — is bit-identical under any
	// permutation of the input lines.
	cost.Coverage = make([]LineCoverage, 0, len(states))
	for _, s := range states {
		if s.remaining < 0 {
			s.remaining = 0
		}
		s.cov.OnDemandQty = s.remaining
		s.cov.OnDemandUSD = s.remaining * s.line.ODRate
		cost.OnDemandUSD += s.cov.OnDemandUSD
		cost.Coverage = append(cost.Coverage, s.cov)
	}

	cost.HourlyUSD = cost.RICommittedUSD + cost.SPCommittedUSD + cost.OnDemandUSD
	cost.StrandedUSD = math.Max(0, cost.RICommittedUSD-cost.RIUsedUSD) +
		math.Max(0, cost.SPCommittedUSD-cost.SPConsumedUSD)
	return cost
}

// applyRI applies one reservation and reports its capacity and consumption in
// normalization units.
func applyRI(ri ReservedInstance, states []*lineState) (capUnits, usedUnits float64) {
	riUnits, unitsOK := InstanceUnits(ri.InstanceType)
	flexible := ri.SizeFlexible()
	perRI := riUnits
	if !unitsOK {
		// Unknown type: fall back to counting whole instances. Matching is
		// exact-type-only in that case, so the scale cancels out.
		perRI = 1
	}
	capUnits = float64(max(ri.Count, 0)) * perRI
	if capUnits <= Eps {
		return capUnits, 0
	}

	riFamily, riPlatform, riTenancy := ri.Family(), NormalizePlatform(ri.Platform), NormalizeTenancy(ri.Tenancy)
	var elig []*lineState
	for _, s := range states {
		l := s.line
		if s.remaining <= Eps || l.Kind != KindEC2 ||
			!strings.EqualFold(l.Region, ri.Region) ||
			NormalizePlatform(l.Platform) != riPlatform ||
			NormalizeTenancy(l.Tenancy) != riTenancy {
			continue
		}
		switch {
		case ri.Zonal():
			// Zonal: exact AZ and instance size, per apply_ri.html.
			if !strings.EqualFold(l.AZ, ri.AZ) || !strings.EqualFold(l.InstanceType, ri.InstanceType) {
				continue
			}
		case flexible:
			// Regional + size-flexible: same family, any size, any AZ. A line
			// whose size has no known normalization factor cannot be scored
			// against the pool, so it is left for an exact-match reservation
			// or on-demand rather than guessed at.
			if !strings.EqualFold(l.Family(), riFamily) || s.unitsEach <= 0 {
				continue
			}
		default:
			// Regional but not size-flexible: AZ-flexible, size-locked.
			if !strings.EqualFold(l.InstanceType, ri.InstanceType) {
				continue
			}
		}
		elig = append(elig, s)
	}
	// "applied from the smallest to the largest instance size within the
	// instance family based on the normalization factor" — apply_ri.html.
	sort.SliceStable(elig, func(i, j int) bool {
		if flexible {
			if d := elig[i].unitsEach - elig[j].unitsEach; d < -Eps || d > Eps {
				return elig[i].unitsEach < elig[j].unitsEach
			}
		}
		return elig[i].key < elig[j].key
	})

	budget := capUnits
	for _, s := range elig {
		if budget <= Eps {
			break
		}
		ue := s.unitsEach
		if ue <= 0 {
			ue = perRI // exact-match path with an unrecognized type
		}
		if ue <= 0 {
			continue
		}
		takeUnits := math.Min(budget, s.remaining*ue)
		if takeUnits <= 0 {
			continue
		}
		qty := takeUnits / ue
		if qty > s.remaining {
			qty = s.remaining
		}
		s.remaining -= qty
		s.cov.RIQty += qty
		budget -= qty * ue
	}
	return capUnits, capUnits - budget
}

func coverEC2SP(s *lineState, qty float64)     { s.cov.EC2SPQty += qty }
func coverComputeSP(s *lineState, qty float64) { s.cov.ComputeSPQty += qty }

// groupEC2SPs buckets EC2 Instance Savings Plans by their (region, family)
// scope, preserving the canonical order of the plans within each group and
// ordering the groups by scope. No map iteration: the caller's determinism
// depends on it.
func groupEC2SPs(plans []SavingsPlan) [][]SavingsPlan {
	var groups [][]SavingsPlan
	index := map[string]int{}
	for _, p := range plans {
		k := strings.ToLower(p.Region) + "\x00" + strings.ToLower(p.Family)
		if i, ok := index[k]; ok {
			groups[i] = append(groups[i], p)
			continue
		}
		index[k] = len(groups)
		groups = append(groups, []SavingsPlan{p})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i][0], groups[j][0]
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.Family < b.Family
	})
	return groups
}

// applySPPool applies one pool of interchangeable Savings Plans.
//
// "We calculate your potential savings percentages of each combination of
// eligible usage… Your Savings Plans are applied to your highest savings
// percentage first. If there are multiple usages with equal savings
// percentages, Savings Plans are applied to the first usage with the lowest
// Savings Plans rate." — sp-applying.html. The final tie-break is the line's
// canonical key, so the order is total and input-order-independent.
func applySPPool(
	states []*lineState,
	plans []SavingsPlan,
	force map[string]bool,
	eligible func(*lineState) bool,
	rateOf func(UsageLine) float64,
	cover func(*lineState, float64),
) (consumed []float64, fallback bool) {
	consumed = make([]float64, len(plans))
	var budget float64
	for _, p := range plans {
		budget += sane(p.CommitmentUSDPerHour)
	}
	if budget <= Eps {
		return consumed, false
	}
	start := budget

	var elig []*lineState
	for _, s := range states {
		if s.remaining > Eps && eligible(s) {
			elig = append(elig, s)
		}
	}
	savings := func(s *lineState) float64 {
		od, r := s.line.ODRate, rateOf(s.line)
		if od <= Eps || r <= 0 {
			return 0
		}
		return (od - r) / od
	}
	sort.SliceStable(elig, func(i, j int) bool {
		si, sj := savings(elig[i]), savings(elig[j])
		if d := si - sj; d < -Eps || d > Eps {
			return si > sj
		}
		ri, rj := rateOf(elig[i].line), rateOf(elig[j].line)
		if d := ri - rj; d < -Eps || d > Eps {
			return ri < rj
		}
		return elig[i].key < elig[j].key
	})

	for _, s := range elig {
		rate := rateOf(s.line)
		if force[s.line.ID] || rate <= 0 {
			// Conservative fallback: unknown rate ⇒ assume the commitment is
			// stranded and the usage is free at the margin. Consumes no
			// budget, so a shrink here can never be claimed as a saving.
			cover(s, s.remaining)
			s.cov.Fallback = true
			s.remaining = 0
			fallback = true
			continue
		}
		if budget <= Eps {
			continue
		}
		qty := math.Min(s.remaining, budget/rate)
		if qty <= 0 {
			continue
		}
		cover(s, qty)
		s.remaining -= qty
		budget -= qty * rate
	}

	// Attribute the pool's consumption to individual plans in canonical order
	// so per-plan stranding — and the ValidFrom it dates — is deterministic.
	rem := start - budget
	if rem < 0 {
		rem = 0
	}
	for i, p := range plans {
		c := math.Min(rem, sane(p.CommitmentUSDPerHour))
		consumed[i] = c
		rem -= c
	}
	return consumed, fallback
}
