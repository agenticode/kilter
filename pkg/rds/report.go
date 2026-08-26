package rds

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Totals summarize a report.
//
// The shape of this struct is the argument of the whole unit. There is a
// `CurrentMonthlyUSD` and there are `RefusedByCode` counts, and the only
// savings fields are `Gross`/`Net`, which are zero in every report this unit
// produces. Money that is REPORTED (an idle instance's cost, an unused storage
// floor's cost) is counted in AdvisoryMonthlyUSD and is never summed with a
// saving — an advisory is not a plan, and adding a magnitude to a forecast
// presents a question as an answer.
type Totals struct {
	Instances  int `json:"instances"`
	Excluded   int `json:"excluded"`
	Proposals  int `json:"proposals"`
	Refusals   int `json:"refusals"`
	Advisories int `json:"advisories"`

	// CostKnown counts instances whose instance line could be priced at all.
	CostKnown int `json:"costKnown"`
	// Unverified counts instances ANY dollar of which came from an unverified
	// rate — the headline caveat of this domain today. Any, not every: a
	// target priced from a verified class rate and an unverified $/GiB-month
	// still has an unverified number in it.
	Unverified int `json:"unverified"`
	// Idle counts instances with zero database connections across the window,
	// split out because it is the finding with a dollar figure and a question
	// attached.
	Idle int `json:"idle"`
	// IdleReplicas is the subset of Idle that are read replicas.
	IdleReplicas int `json:"idleReplicas"`
	// StorageFloored counts instances whose allocated storage is materially
	// above what was ever used — money that cannot be recovered by any API.
	StorageFloored int `json:"storageFloored"`
	// MultiAZ counts instances whose instance line is doubled.
	MultiAZ int `json:"multiAZ"`

	CurrentMonthlyUSD      float64 `json:"currentMonthlyUSD"`
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	NetSavingsMonthlyUSD   float64 `json:"netSavingsMonthlyUSD"`
	// UnrecoverableStorageMonthlyUSD is the allocated-but-unused storage cost
	// across the fleet. It is the single largest honest number this domain
	// produces and it is NOT a saving: no RDS API reduces allocated storage.
	UnrecoverableStorageMonthlyUSD float64 `json:"unrecoverableStorageMonthlyUSD"`
	// IdleMonthlyUSD is the cost of instances carrying no connections.
	IdleMonthlyUSD float64 `json:"idleMonthlyUSD"`
	// StrandedIfDownsizedMonthlyUSD is the Reserved DB Instance commitment a
	// hypothetical one-size-down move would waste. Reported so the arithmetic
	// trap 13 describes is visible even though the move is not proposed.
	StrandedIfDownsizedMonthlyUSD float64 `json:"strandedIfDownsizedMonthlyUSD"`

	// RefusedByCode counts reason codes so a UI can render "what we declined
	// to do, and why" without walking every assessment.
	RefusedByCode map[string]int `json:"refusedByCode,omitempty"`
	// AdvisedByCode does the same for advisories.
	AdvisedByCode map[string]int `json:"advisedByCode,omitempty"`
}

// Report is one region's read-only verdict.
type Report struct {
	Domain      domain.Kind `json:"domain"`
	Scope       string      `json:"scope,omitempty"`
	Region      string      `json:"region,omitempty"`
	GeneratedAt time.Time   `json:"generatedAt"`
	Window      Window      `json:"window"`
	Stale       bool        `json:"stale,omitempty"`
	// Config is echoed so a stored report explains its own thresholds.
	Config Config `json:"config"`
	// Reservations is how many Reserved DB Instances were in the inventory the
	// snapshot carried. Zero with a nil ledger is a different fact from zero
	// with a ledger, and the report says which.
	Reservations int `json:"reservations,omitempty"`
	// Assessments is sorted by instance ARN.
	Assessments []Assessment `json:"assessments,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
	Totals      Totals       `json:"totals"`
}

// For returns the assessment for an instance ARN or identifier.
func (r *Report) For(id string) (Assessment, bool) {
	i := sort.Search(len(r.Assessments), func(i int) bool { return r.Assessments[i].Target.ID >= id })
	if i < len(r.Assessments) && r.Assessments[i].Target.ID == id {
		return r.Assessments[i], true
	}
	return Assessment{}, false
}

// Refusals projects the report onto the generic seam so the cross-domain
// report can render RDS beside every other domain.
//
// This is the ONLY projection that carries this domain's output, because
// [domain.Recommendation] cannot express a refusal — Validate rejects one
// whose Proposed equals its Current — and this domain proposes nothing. A
// fleet that renders only recommendations would show a user an empty RDS
// section and let them conclude the tool found nothing, which is a different
// claim from "the tool declined to guess, here is what it needs".
func (r *Report) Refusals() []domain.Refusal {
	var out []domain.Refusal
	for _, a := range r.Assessments {
		for _, s := range a.Suppressions {
			out = append(out, domain.Refusal{
				Target: a.Target, Code: s.Code, Reason: s.Reason, ValidFrom: s.ValidFrom,
			})
		}
	}
	domain.SortRefusals(out)
	return out
}

func (r *Report) computeTotals() Totals {
	t := Totals{RefusedByCode: map[string]int{}, AdvisedByCode: map[string]int{}}
	for _, a := range r.Assessments {
		t.Instances++
		if a.Excluded() {
			t.Excluded++
		}
		for _, s := range a.Suppressions {
			t.RefusedByCode[s.Code]++
		}
		for _, ad := range a.Advisories {
			t.AdvisedByCode[ad.Code]++
		}
		t.Advisories += len(a.Advisories)
		if a.CostKnown {
			t.CostKnown++
			t.CurrentMonthlyUSD += a.CurrentMonthlyUSD
		}
		// Counted off the WORST rate behind any dollar, not off the instance
		// line alone: a target priced from a verified class rate and an
		// unverified $/GiB-month still has an unverified dollar in it.
		if p := a.WorstRateProvenance(); p != "" && !p.Claimable() {
			t.Unverified++
		}
		if a.Deployment == "multi-az-instance" {
			t.MultiAZ++
		}
		if a.Idle.Known && a.Idle.Idle {
			t.Idle++
			t.IdleMonthlyUSD += a.CurrentMonthlyUSD
			if a.Idle.IsReplica {
				t.IdleReplicas++
			}
		}
		if a.Storage.Overprovisioned(r.Config.StorageOverprovisionThreshold) {
			t.StorageFloored++
			t.UnrecoverableStorageMonthlyUSD += a.Storage.UnusedMonthlyUSD
		}
		if a.Commitment.StrandedMonthlyUSD > 0 {
			t.StrandedIfDownsizedMonthlyUSD += a.Commitment.StrandedMonthlyUSD
		}
		if a.Proposal == nil {
			t.Refusals++
			continue
		}
		t.Proposals++
		t.GrossSavingsMonthlyUSD += a.Proposal.GrossSavingsMonthlyUSD
		t.NetSavingsMonthlyUSD += a.Proposal.NetSavingsMonthlyUSD
	}
	if len(t.RefusedByCode) == 0 {
		t.RefusedByCode = nil
	}
	if len(t.AdvisedByCode) == 0 {
		t.AdvisedByCode = nil
	}
	return t
}

// Validate asserts this package's invariants over a finished report. It is
// exported and cheap on purpose: the tests call it, the fuzz targets call it,
// and it is the right thing for a later unit to call before persisting a
// report or serving it. A violation is a BUG IN THIS PACKAGE, not bad input.
//
// The clauses in [validateProposal] are the specification of what this domain
// may ever propose. Read them as the list of things RDS cannot do.
func (r *Report) Validate() error {
	if r.Domain != Kind {
		return fmt.Errorf("rds: report domain %q, want %q", r.Domain, Kind)
	}
	var lastID string
	for i, a := range r.Assessments {
		if i > 0 && a.Target.ID < lastID {
			return fmt.Errorf("rds: assessments are not sorted by ARN (%q after %q)", a.Target.ID, lastID)
		}
		lastID = a.Target.ID

		if len(a.Evidence) == 0 {
			return fmt.Errorf("rds: %s has no evidence; every assessment must state what it saw", a.Target.ID)
		}
		if len(a.Suppressions) == 0 {
			return fmt.Errorf("rds: %s gives no reason for anything; silence is not an output", a.Target.ID)
		}
		for _, s := range a.Suppressions {
			if s.Code == "" || s.Reason == "" {
				return fmt.Errorf("rds: %s has a suppression with no %s", a.Target.ID,
					map[bool]string{true: "code", false: "reason"}[s.Code == ""])
			}
		}
		// Exclusions fire alone. An instance this domain does not model must
		// not also be told things in a vocabulary that does not describe it.
		if a.Excluded() {
			if len(a.Suppressions) != 1 {
				return fmt.Errorf("rds: %s is excluded (%s) but carries %d suppressions; an exclusion fires "+
					"alone", a.Target.ID, a.Suppressions[0].Code, len(a.Suppressions))
			}
			if len(a.Advisories) > 0 {
				return fmt.Errorf("rds: %s is excluded but carries %d advisories", a.Target.ID, len(a.Advisories))
			}
			if a.Proposal != nil {
				return fmt.Errorf("rds: %s is excluded from this domain but carries a proposal", a.Target.ID)
			}
		}
		for _, ad := range a.Advisories {
			if ad.Caveat == "" {
				return fmt.Errorf("rds: %s advisory %q has no caveat; an advisory without its caveat reads "+
					"as an actionable saving", a.Target.ID, ad.Code)
			}
			if ad.Actuatable() || ad.Action() != domain.ActionAdvisory {
				return fmt.Errorf("rds: %s advisory %q claims to be actuatable", a.Target.ID, ad.Code)
			}
			if ad.MonthlyUSD < 0 {
				return fmt.Errorf("rds: %s advisory %q carries a negative magnitude %v; an advisory reports "+
					"a cost, never a signed delta", a.Target.ID, ad.Code, ad.MonthlyUSD)
			}
		}
		if err := validateProposal(a); err != nil {
			return err
		}
	}
	want := r.computeTotals()
	switch {
	case want.Instances != r.Totals.Instances,
		want.Proposals != r.Totals.Proposals,
		want.Refusals != r.Totals.Refusals,
		!nearly(want.NetSavingsMonthlyUSD, r.Totals.NetSavingsMonthlyUSD),
		!nearly(want.CurrentMonthlyUSD, r.Totals.CurrentMonthlyUSD):
		return fmt.Errorf("rds: totals do not match the assessments they summarize")
	}
	return nil
}

// validateProposal is the gate in front of everything U13 and U14 will ever
// want to do. It is written now, while there are no proposals, precisely
// because a gate written after the thing it guards is a gate someone argued
// their way past.
func validateProposal(a Assessment) error {
	p := a.Proposal
	if p == nil {
		return nil
	}
	id := a.Target.ID

	// TRAP 8. Allocated storage is a monotone ratchet: "You can't reduce the
	// amount of storage for a DB instance after storage has been allocated."
	// A proposal below the observed allocation is not merely unwise, it is
	// unexecutable, and it would put a saving in a report that no API could
	// realize.
	if p.AllocatedStorageGiB != 0 && p.AllocatedStorageGiB < a.Instance.AllocatedStorageGiB {
		return fmt.Errorf("rds: %s proposes %d GiB of allocated storage against an observed %d GiB; "+
			"allocated storage can only ever grow, and no RDS API reduces it",
			id, p.AllocatedStorageGiB, a.Instance.AllocatedStorageGiB)
	}

	// Read-only, structurally: the only action class this domain may emit.
	// U13 and U14 relax this deliberately and with their own justification;
	// until then, a non-advisory action here is a bug.
	if p.Action != domain.ActionAdvisory {
		return fmt.Errorf("rds: %s proposal has action %q; this domain is advisory only", id, p.Action)
	}
	if p.Action.Disruptive() {
		return fmt.Errorf("rds: %s proposal claims a disruptive action class; nothing this domain "+
			"represents requires downtime", id)
	}

	// An excluded instance has no verdict of any kind.
	if a.Excluded() {
		return fmt.Errorf("rds: %s is excluded but carries a proposal", id)
	}

	// Money. An unverified rate may size a fact and may never become a claim.
	if p.NetSavingsMonthlyUSD > 0 && !p.RateProvenance.Claimable() {
		return fmt.Errorf("rds: %s claims %s/mo from a %q rate; §7 marks RDS rates unverified and an "+
			"unverified rate is a magnitude, not a saving", id, fmtUSD(p.NetSavingsMonthlyUSD),
			p.RateProvenance)
	}
	if p.NetSavingsMonthlyUSD > p.GrossSavingsMonthlyUSD+1e-6 {
		return fmt.Errorf("rds: %s claims net %s/mo above gross %s/mo; net can never exceed the list-price "+
			"delta", id, fmtUSD(p.NetSavingsMonthlyUSD), fmtUSD(p.GrossSavingsMonthlyUSD))
	}
	if p.NetSavingsMonthlyUSD <= 0 {
		return fmt.Errorf("rds: %s proposes a change claiming %s/mo; a proposal must be net-positive or be "+
			"a refusal", id, fmtUSD(p.NetSavingsMonthlyUSD))
	}
	if p.Confidence <= 0 || p.Confidence > 1 {
		return fmt.Errorf("rds: %s proposal has out-of-range confidence %v", id, p.Confidence)
	}
	if p.Reason == "" {
		return fmt.Errorf("rds: %s proposal states no reason", id)
	}
	return nil
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// WriteText renders a deterministic human summary. It is the shape
// `kilter --domain rds` prints and the UI card mirrors.
//
// The layout puts the refusals first and the money second, which is the
// opposite of every other domain's report and is deliberate: in RDS the
// refusal IS the finding, and a reader who sees a dollar figure first will
// read the refusal as a caveat on a recommendation that does not exist.
func (r *Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "kilter rds domain (advisory only, read-only) — scope %s region %s\n",
		orNone(r.Scope), orNone(r.Region))
	fmt.Fprintf(tw, "generated %s over a %s window\n",
		r.GeneratedAt.UTC().Format(time.RFC3339), r.Window.String())
	if r.Stale {
		fmt.Fprintf(tw, "SNAPSHOT IS INCOMPLETE — affected instances are refused, not guessed\n")
	}
	t := r.Totals
	fmt.Fprintf(tw, "%d DB instances\t%d excluded\t%d with nothing proposed\t%d advisories\n",
		t.Instances, t.Excluded, t.Refusals, t.Advisories)
	fmt.Fprintf(tw, "THIS DOMAIN PROPOSES NOTHING. Instance-class changes are failovers; allocated storage "+
		"cannot shrink; FreeableMemory is engine-dependent.\n")
	fmt.Fprintf(tw, "observed instance spend %s/mo across %d priced instances\n",
		fmtUSD(t.CurrentMonthlyUSD), t.CostKnown)
	// Counted independently of CostKnown, and phrased so: an instance whose
	// class this package cannot price can still carry an unverified STORAGE
	// dollar, so this number is legitimately allowed to exceed the priced
	// count and must not read as a subset of it.
	fmt.Fprintf(tw, "%d instances carry at least one UNVERIFIED dollar figure\t"+
		"(no RDS rate here came from AWS; supply your own with rds.LoadRates)\n", t.Unverified)
	fmt.Fprintf(tw, "%d instances carried zero connections (%s/mo)\tof those, %d are read replicas\n",
		t.Idle, fmtUSD(t.IdleMonthlyUSD), t.IdleReplicas)
	fmt.Fprintf(tw, "%d instances are paying for storage they never used (%s/mo, unrecoverable by any API)\n",
		t.StorageFloored, fmtUSD(t.UnrecoverableStorageMonthlyUSD))
	if t.StrandedIfDownsizedMonthlyUSD > 0 {
		fmt.Fprintf(tw, "a one-size-down move across the fleet would strand %s/mo of Reserved DB Instance "+
			"commitment\n", fmtUSD(t.StrandedIfDownsizedMonthlyUSD))
	}
	fmt.Fprintln(tw)

	for _, code := range sortedCodes(t.RefusedByCode) {
		fmt.Fprintf(tw, "  refused [%s]\t×%d\n", code, t.RefusedByCode[code])
	}
	fmt.Fprintln(tw)

	for _, a := range r.Assessments {
		cost := "unpriced"
		if a.CostKnown {
			cost = fmtUSD(a.CurrentMonthlyUSD) + "/mo"
			if !a.RateProvenance.Claimable() {
				cost += " (" + string(a.RateProvenance) + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Instance.DisplayName(), orNone(a.Instance.Class),
			a.Engine.String(), orNone(string(a.Deployment)), cost)
		for _, ad := range a.Advisories {
			fmt.Fprintf(tw, "  REPORT [%s]\t%s\n", ad.Code, ad.Message)
			fmt.Fprintf(tw, "         caveat\t%s\n", ad.Caveat)
		}
		for _, s := range a.Suppressions {
			if s.ValidFrom.IsZero() {
				fmt.Fprintf(tw, "  REFUSE [%s]\t%s\n", s.Code, s.Reason)
				continue
			}
			fmt.Fprintf(tw, "  REFUSE [%s]\t%s\t(re-evaluate %s)\n",
				s.Code, s.Reason, s.ValidFrom.UTC().Format("2006-01-02"))
		}
		if p := a.Proposal; p != nil {
			fmt.Fprintf(tw, "  ADVISE\tstorage %s iops %d throughput %d\tnet %s/mo\tconfidence %.2f (%s)\n",
				orNone(p.StorageType), p.IOPS, p.StorageThroughputMBps,
				fmtUSD(p.NetSavingsMonthlyUSD), p.Confidence, p.Action)
		}
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(tw, "warning: %s\n", warn)
	}
	return tw.Flush()
}

// sortedCodes orders a code tally by descending count then code, so no output
// depends on Go's randomized map order.
func sortedCodes(m map[string]int) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
