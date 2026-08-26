package domain

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Report is the cross-domain answer: every registered domain's health, its
// recommendations, its refusals, and one set of totals.
//
// # The one number this type exists to protect
//
// Kilter prints exactly one figure as a saving, and it is
// [Report.Totals].ClaimableMonthlyUSD: the sum of
// [Recommendation.ClaimableMonthlyUSD] over recommendations that are actually
// applicable. Every term in that sum came out of a domain's commitment
// waterfall — a Bill(before) − Bill(after) delta through pkg/pricing/commit —
// because [Recommendation.SetSavings] is the only supported way to populate
// the field and it clamps net to gross (§7 trap 1).
//
// GrossMonthlyUSD is carried beside it as the list-price fantasy, and the
// aggregate keeps the same discipline the per-recommendation type does:
// Claimable ≤ Gross at every level, asserted by [Report.Validate].
//
// # Why gross is clamped per recommendation
//
// Gross sums max(0, gross) rather than raw gross. A domain may legitimately
// recommend a CHANGE THAT COSTS MORE — a safety-driven growth, an
// under-provisioned Lambda — and such a recommendation has a negative gross
// and a zero claim. Summing raw gross would let one negative recommendation
// drag the total below a positive claimable sum and manufacture a net > gross
// violation out of two individually honest numbers. The clamp keeps the
// aggregate invariant a consequence of the per-recommendation one.
// GrossIncreaseMonthlyUSD carries the other side, so nothing is hidden by the
// clamp.
type Report struct {
	At      time.Time      `json:"at"`
	Domains []DomainReport `json:"domains,omitempty"`
	Totals  Totals         `json:"totals"`

	// Recommendations is every domain's output, sorted. Suppressed
	// recommendations are INCLUDED — §5.7 requires a commitment-blocked
	// recommendation to stay visible with its reason.
	Recommendations []Recommendation `json:"recommendations,omitempty"`
	// Refusals is every target a domain declined to change, with its reason.
	Refusals []Refusal `json:"refusals,omitempty"`
}

// DomainReport is one domain's slice of the whole.
type DomainReport struct {
	Kind   Kind   `json:"kind"`
	Health Health `json:"health"`
	// Actuatable reports whether an actuator is wired (see actuation.go).
	// A domain can be ready, willing, and still unable to act.
	Actuatable bool `json:"actuatable"`

	Recommendations int `json:"recommendations"`
	// Applicable is how many may be planned; Suppressed how many must not be.
	Applicable int `json:"applicable"`
	Suppressed int `json:"suppressed"`
	// Refused is how many targets produced no recommendation at all.
	Refused int `json:"refused"`

	ClaimableMonthlyUSD     float64 `json:"claimableMonthlyUSD"`
	GrossMonthlyUSD         float64 `json:"grossMonthlyUSD"`
	GrossIncreaseMonthlyUSD float64 `json:"grossIncreaseMonthlyUSD,omitempty"`

	// SuppressedByCode and RefusedByCode drive the "what we declined to do,
	// and why" panel, most frequent first.
	SuppressedByCode []CodeCount `json:"suppressedByCode,omitempty"`
	RefusedByCode    []CodeCount `json:"refusedByCode,omitempty"`
}

// Totals is the cross-domain roll-up.
type Totals struct {
	Domains    int `json:"domains"`
	ReportOnly int `json:"reportOnly"`
	Actuatable int `json:"actuatable"`

	Recommendations int `json:"recommendations"`
	Applicable      int `json:"applicable"`
	Suppressed      int `json:"suppressed"`
	Refused         int `json:"refused"`

	ClaimableMonthlyUSD     float64 `json:"claimableMonthlyUSD"`
	GrossMonthlyUSD         float64 `json:"grossMonthlyUSD"`
	GrossIncreaseMonthlyUSD float64 `json:"grossIncreaseMonthlyUSD,omitempty"`

	SuppressedByCode []CodeCount `json:"suppressedByCode,omitempty"`
	RefusedByCode    []CodeCount `json:"refusedByCode,omitempty"`
}

// Summarize runs the read half of the loop across every registered domain:
// recommend, collect refusals, roll up.
//
// Determinism: domains are visited in canonical kind order, recommendations
// are sorted before anything is added up, and every float sum accumulates in
// that canonical order — so two runs over the same state produce the same
// bytes, down to the last bit of float64 addition.
//
// A nil registry summarizes to an empty report with zero totals, which is the
// "organ, not heart" rule at the report level: a core with no domains attached
// reports nothing and fails at nothing.
func Summarize(now time.Time, reg *Registry, ledger Netter) *Report {
	rep := &Report{At: now}
	if reg == nil || reg.Len() == 0 {
		return rep
	}
	rep.Recommendations = reg.Recommend(now, ledger)

	for _, k := range reg.Kinds() {
		d, ok := reg.Get(k)
		if !ok {
			continue
		}
		dr := DomainReport{
			Kind:       k,
			Health:     reg.healthOf(k, d, now),
			Actuatable: reg.CanActuate(k),
		}
		refusals := reg.Refusals(k, now, ledger)
		rep.Refusals = append(rep.Refusals, refusals...)

		var suppressCodes, refuseCodes []string
		for _, rec := range rep.Recommendations {
			if rec.Target.Domain != k {
				continue
			}
			dr.Recommendations++
			gross := finite(rec.GrossSavingsMonthlyUSD)
			if gross > 0 {
				dr.GrossMonthlyUSD += gross
			} else {
				dr.GrossIncreaseMonthlyUSD += -gross
			}
			if rec.Suppressed {
				dr.Suppressed++
				suppressCodes = append(suppressCodes, rec.SuppressCode)
				continue
			}
			dr.Applicable++
			dr.ClaimableMonthlyUSD += rec.ClaimableMonthlyUSD()
		}
		for _, ref := range refusals {
			dr.Refused++
			refuseCodes = append(refuseCodes, ref.Code)
		}
		dr.SuppressedByCode = countCodes(suppressCodes)
		dr.RefusedByCode = countCodes(refuseCodes)
		rep.Domains = append(rep.Domains, dr)
	}
	SortRefusals(rep.Refusals)
	rep.Totals = totalsOf(rep.Domains)
	return rep
}

// healthOf reads a domain's health and stamps its kind, the way
// [Registry.Health] does.
func (r *Registry) healthOf(k Kind, d Domain, now time.Time) Health {
	h := d.Health(now)
	h.Kind = k
	return h
}

// totalsOf folds the per-domain rows, in the order they are already in
// (canonical kind order).
func totalsOf(rows []DomainReport) Totals {
	var t Totals
	suppressed := make([][]CodeCount, 0, len(rows))
	refused := make([][]CodeCount, 0, len(rows))
	for _, d := range rows {
		t.Domains++
		if d.Health.ReportOnly {
			t.ReportOnly++
		}
		if d.Actuatable {
			t.Actuatable++
		}
		t.Recommendations += d.Recommendations
		t.Applicable += d.Applicable
		t.Suppressed += d.Suppressed
		t.Refused += d.Refused
		t.ClaimableMonthlyUSD += d.ClaimableMonthlyUSD
		t.GrossMonthlyUSD += d.GrossMonthlyUSD
		t.GrossIncreaseMonthlyUSD += d.GrossIncreaseMonthlyUSD
		suppressed = append(suppressed, d.SuppressedByCode)
		refused = append(refused, d.RefusedByCode)
	}
	t.SuppressedByCode = mergeCodeCounts(suppressed...)
	t.RefusedByCode = mergeCodeCounts(refused...)
	return t
}

// For returns one domain's row.
func (r *Report) For(k Kind) (DomainReport, bool) {
	for _, d := range r.Domains {
		if d.Kind == k {
			return d, true
		}
	}
	return DomainReport{}, false
}

// Validate is the invariant checker for the aggregate. It is cheap, and it is
// the difference between a bug and a wrong invoice — call it before printing,
// persisting or serving a report.
func (r *Report) Validate() error {
	if r == nil {
		return fmt.Errorf("domain: nil report")
	}
	seen := make(map[Kind]bool, len(r.Domains))
	for _, rec := range r.Recommendations {
		if err := rec.Validate(); err != nil {
			return err
		}
	}
	for _, ref := range r.Refusals {
		switch {
		case ref.Target.ID == "":
			return fmt.Errorf("domain: refusal has no target ID")
		case ref.Code == "":
			return fmt.Errorf("domain: refusal for %s has no code", ref.Target)
		case ref.Reason == "":
			return fmt.Errorf("domain: refusal %s/%s has no reason", ref.Target, ref.Code)
		}
	}
	for _, d := range r.Domains {
		if seen[d.Kind] {
			return fmt.Errorf("domain: report lists %q twice", d.Kind)
		}
		seen[d.Kind] = true
		if !d.Kind.Valid() {
			return fmt.Errorf("domain: report lists unknown kind %q", d.Kind)
		}
		if d.Applicable+d.Suppressed != d.Recommendations {
			return fmt.Errorf("domain: %s counts %d applicable + %d suppressed ≠ %d recommendations",
				d.Kind, d.Applicable, d.Suppressed, d.Recommendations)
		}
		if err := checkMoney(string(d.Kind), d.ClaimableMonthlyUSD, d.GrossMonthlyUSD, d.GrossIncreaseMonthlyUSD); err != nil {
			return err
		}
	}
	t := r.Totals
	if t.Domains != len(r.Domains) {
		return fmt.Errorf("domain: totals count %d domains, report lists %d", t.Domains, len(r.Domains))
	}
	if t.Applicable+t.Suppressed != t.Recommendations {
		return fmt.Errorf("domain: totals count %d applicable + %d suppressed ≠ %d recommendations",
			t.Applicable, t.Suppressed, t.Recommendations)
	}
	if t.Recommendations != len(r.Recommendations) {
		return fmt.Errorf("domain: totals count %d recommendations, report carries %d",
			t.Recommendations, len(r.Recommendations))
	}
	if t.Refused != len(r.Refusals) {
		return fmt.Errorf("domain: totals count %d refusals, report carries %d",
			t.Refused, len(r.Refusals))
	}
	return checkMoney("totals", t.ClaimableMonthlyUSD, t.GrossMonthlyUSD, t.GrossIncreaseMonthlyUSD)
}

// checkMoney enforces the money discipline at one level of the report.
func checkMoney(what string, claimable, gross, increase float64) error {
	for _, f := range []struct {
		name string
		v    float64
	}{{"claimable", claimable}, {"gross", gross}, {"gross increase", increase}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			return fmt.Errorf("domain: %s has non-finite %s savings", what, f.name)
		}
		if f.v < 0 {
			return fmt.Errorf("domain: %s has negative %s savings $%v", what, f.name, f.v)
		}
	}
	// The invariant the whole package exists to protect, at the aggregate:
	// what may be claimed can never exceed the list-price fantasy.
	if claimable > gross+moneyEps {
		return fmt.Errorf("domain: %s claims $%v > gross $%v", what, claimable, gross)
	}
	return nil
}

// moneyEps matches commit.Eps: float64 summation over thousands of targets
// must not be able to trip an invariant check by its last bits.
const moneyEps = 1e-9

// WriteText renders the report for a terminal. Deterministic: no map is
// iterated, every list is already sorted, and money is formatted at a fixed
// precision.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "kilter domains — %s\n\n", r.At.UTC().Format(time.RFC3339))
	if len(r.Domains) == 0 {
		b.WriteString("  no domains registered\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	fmt.Fprintf(&b, "  %-14s %-9s %-11s %7s %7s %7s %12s %12s\n",
		"DOMAIN", "STATE", "ACTUATION", "TARGETS", "RECS", "REFUSED", "CLAIMABLE", "GROSS")
	for _, d := range r.Domains {
		fmt.Fprintf(&b, "  %-14s %-9s %-11s %7d %7d %7d %12s %12s\n",
			d.Kind, healthState(d.Health), actuationState(d),
			d.Health.Targets, d.Recommendations, d.Refused,
			usd(d.ClaimableMonthlyUSD), usd(d.GrossMonthlyUSD))
	}
	fmt.Fprintf(&b, "  %-14s %-9s %-11s %7s %7d %7d %12s %12s\n",
		"TOTAL", "", "", "", r.Totals.Recommendations, r.Totals.Refused,
		usd(r.Totals.ClaimableMonthlyUSD), usd(r.Totals.GrossMonthlyUSD))

	b.WriteString("\n  claimable = net of the commitment waterfall, the only number that is a saving\n")
	b.WriteString("  gross     = on-demand list price, shown so the fantasy sits beside the fact\n")
	if r.Totals.GrossIncreaseMonthlyUSD > 0 {
		fmt.Fprintf(&b, "  %s/mo of recommended changes would COST more (safety-driven growth)\n",
			usd(r.Totals.GrossIncreaseMonthlyUSD))
	}

	// Degraded domains, named. A report-only domain that is not labelled as
	// such reads as a domain with nothing to say.
	var degraded []DomainReport
	for _, d := range r.Domains {
		if d.Health.ReportOnly || !d.Health.Ready {
			degraded = append(degraded, d)
		}
	}
	if len(degraded) > 0 {
		b.WriteString("\n  Degraded domains\n")
		for _, d := range degraded {
			fmt.Fprintf(&b, "    %-14s %s\n", d.Kind, describeHealth(d.Health))
		}
	}

	if len(r.Totals.SuppressedByCode) > 0 || len(r.Totals.RefusedByCode) > 0 {
		b.WriteString("\n  What kilter declined to do, and why\n")
		for _, c := range r.Totals.SuppressedByCode {
			fmt.Fprintf(&b, "    %-32s %4d suppressed (reported, never applied)\n", c.Code, c.Count)
		}
		for _, c := range r.Totals.RefusedByCode {
			fmt.Fprintf(&b, "    %-32s %4d refused\n", c.Code, c.Count)
		}
	}

	if r.Totals.Applicable > 0 {
		b.WriteString("\n  Applicable recommendations\n")
		for _, rec := range r.Recommendations {
			if rec.Suppressed {
				continue
			}
			fmt.Fprintf(&b, "    %-10s %-40s %-10s %s/mo  %s\n",
				rec.Target.Domain, truncate(rec.Target.ID, 40), rec.Action,
				usd(rec.ClaimableMonthlyUSD()), rec.Reason)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func healthState(h Health) string {
	switch {
	case !h.Ready:
		return "degraded"
	case h.ReportOnly:
		return "ready"
	default:
		return "ready"
	}
}

func actuationState(d DomainReport) string {
	switch {
	case d.Health.ReportOnly:
		return "report-only"
	case !d.Actuatable:
		return "no actuator"
	default:
		return "wired"
	}
}

func usd(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "$?"
	}
	return fmt.Sprintf("$%.2f", v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// KindsOf returns the kinds present in a recommendation slice, in canonical
// order. Used to notice a recommendation attributed to a domain nobody
// registered — which is a bug worth reporting, not dropping.
func KindsOf(recs []Recommendation) []Kind {
	seen := make(map[Kind]bool, len(recs))
	for _, r := range recs {
		seen[r.Target.Domain] = true
	}
	out := make([]Kind, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
