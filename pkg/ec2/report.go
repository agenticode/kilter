package ec2

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

// Totals summarize a report. Savings totals are net-of-commitment and
// advisory savings are counted separately, because an advisory is not a plan
// and adding the two would present unactuatable money as a forecast.
type Totals struct {
	Instances        int `json:"instances"`
	Excluded         int `json:"excluded"`
	Proposals        int `json:"proposals"`
	Refusals         int `json:"refusals"`
	Advisories       int `json:"advisories"`
	MemoryBlind      int `json:"memoryBlind"`
	CoarseResolution int `json:"coarseResolution"`

	CurrentMonthlyUSD      float64 `json:"currentMonthlyUSD"`
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	NetSavingsMonthlyUSD   float64 `json:"netSavingsMonthlyUSD"`
	// AdvisoryNetSavingsMonthlyUSD is money that would require a human
	// decision this domain cannot make (an architecture port, a credit-mode
	// change). It is reported, never claimed.
	AdvisoryNetSavingsMonthlyUSD float64 `json:"advisoryNetSavingsMonthlyUSD"`
	// SuppressedByCode counts refusal reasons, so the UI can show what the
	// domain declined to do and why without walking every assessment.
	SuppressedByCode map[string]int `json:"suppressedByCode,omitempty"`
}

// Report is one region's read-only verdict.
type Report struct {
	Domain      string    `json:"domain"`
	Scope       string    `json:"scope,omitempty"`
	Region      string    `json:"region,omitempty"`
	GeneratedAt time.Time `json:"generatedAt"`
	Window      Window    `json:"window"`
	// Stale marks a report built on an incomplete snapshot.
	Stale bool `json:"stale,omitempty"`
	// Config is echoed so a stored report explains its own thresholds.
	Config Config `json:"config"`
	// Assessments is sorted by instance ID.
	Assessments []Assessment `json:"assessments,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
	Totals      Totals       `json:"totals"`
}

// Proposals returns the assessments that carry a proposal, in report order.
func (r *Report) Proposals() []Assessment {
	var out []Assessment
	for _, a := range r.Assessments {
		if a.Proposal != nil {
			out = append(out, a)
		}
	}
	return out
}

// For returns the assessment for an instance ID.
func (r *Report) For(id string) (Assessment, bool) {
	i := sort.Search(len(r.Assessments), func(i int) bool { return r.Assessments[i].Target.ID >= id })
	if i < len(r.Assessments) && r.Assessments[i].Target.ID == id {
		return r.Assessments[i], true
	}
	return Assessment{}, false
}

func (r *Report) computeTotals() Totals {
	t := Totals{SuppressedByCode: map[string]int{}}
	for _, a := range r.Assessments {
		t.Instances++
		if a.Excluded() {
			t.Excluded++
		}
		t.CurrentMonthlyUSD += a.CurrentMonthlyUSD
		t.Advisories += len(a.Advisories)
		for _, s := range a.Suppressions {
			t.SuppressedByCode[s.Code]++
		}
		for _, ad := range a.Advisories {
			if ad.NetSavingsMonthlyUSD > 0 {
				t.AdvisoryNetSavingsMonthlyUSD += ad.NetSavingsMonthlyUSD
			}
		}
		if a.Observation.MemoryBlind {
			t.MemoryBlind++
		}
		if a.Observation.PeriodSeconds > PeriodDetailedSeconds {
			t.CoarseResolution++
		}
		if a.Proposal == nil {
			t.Refusals++
			continue
		}
		t.Proposals++
		t.GrossSavingsMonthlyUSD += a.Proposal.GrossSavingsMonthlyUSD
		t.NetSavingsMonthlyUSD += a.Proposal.NetSavingsMonthlyUSD
	}
	if len(t.SuppressedByCode) == 0 {
		t.SuppressedByCode = nil
	}
	return t
}

// Validate asserts this package's invariants over a finished report. It is
// exported and cheap on purpose: it is called by the tests, by the fuzz
// target, and is the right thing for a later unit to call before persisting a
// report or serving it. A violation is a bug in this package, not bad input.
func (r *Report) Validate() error {
	if r.Domain != Domain {
		return fmt.Errorf("ec2: report domain %q, want %q", r.Domain, Domain)
	}
	var lastID string
	for i, a := range r.Assessments {
		if i > 0 && a.Target.ID < lastID {
			return fmt.Errorf("ec2: assessments are not sorted by instance ID (%q after %q)", a.Target.ID, lastID)
		}
		lastID = a.Target.ID

		if len(a.Evidence) == 0 {
			return fmt.Errorf("ec2: %s has no evidence; every assessment must state what it saw", a.Target.ID)
		}
		if a.Proposal == nil && len(a.Suppressions) == 0 {
			return fmt.Errorf("ec2: %s proposes nothing and gives no reason; silence is not an output", a.Target.ID)
		}
		for _, ad := range a.Advisories {
			if ad.Caveat == "" {
				return fmt.Errorf("ec2: %s advisory %q has no caveat; an advisory without its caveat reads as an "+
					"actionable saving", a.Target.ID, ad.Code)
			}
			if ad.Actuatable() {
				return fmt.Errorf("ec2: %s advisory %q claims to be actuatable", a.Target.ID, ad.Code)
			}
		}
		if a.Excluded() {
			if a.Proposal != nil {
				return fmt.Errorf("ec2: %s is excluded from this domain but carries a proposal", a.Target.ID)
			}
			if len(a.Advisories) > 0 {
				return fmt.Errorf("ec2: %s is excluded from this domain but carries advisories", a.Target.ID)
			}
		}
		if err := validateProposal(a); err != nil {
			return err
		}
	}
	want := r.computeTotals()
	if want.Proposals != r.Totals.Proposals || want.Instances != r.Totals.Instances ||
		!nearly(want.NetSavingsMonthlyUSD, r.Totals.NetSavingsMonthlyUSD) {
		return fmt.Errorf("ec2: totals do not match the assessments they summarize")
	}
	return nil
}

func validateProposal(a Assessment) error {
	p := a.Proposal
	if p == nil {
		return nil
	}
	obs := a.Observation
	// The invariant this package exists for.
	if obs.MemoryBlind && p.Spec.Resources.MemoryBytes < a.Current.Resources.MemoryBytes {
		return fmt.Errorf("ec2: %s is memory-blind but the proposal cuts memory from %s to %s",
			a.Target.ID, gib(a.Current.Resources.MemoryBytes), gib(p.Spec.Resources.MemoryBytes))
	}
	if !obs.MemoryBlind && p.Spec.Resources.MemoryBytes < obs.PeakMemoryBytes {
		return fmt.Errorf("ec2: %s proposal memory %s is below the observed peak %s",
			a.Target.ID, gib(p.Spec.Resources.MemoryBytes), gib(obs.PeakMemoryBytes))
	}
	if p.Spec.Resources.MilliCPU < obs.PeakCPUMilli {
		return fmt.Errorf("ec2: %s proposal CPU %d mCPU is below the observed peak %d mCPU",
			a.Target.ID, p.Spec.Resources.MilliCPU, obs.PeakCPUMilli)
	}
	if p.Spec.Resources.MilliCPU < obs.DemandCPUMilli {
		return fmt.Errorf("ec2: %s proposal CPU %d mCPU is below computed demand %d mCPU",
			a.Target.ID, p.Spec.Resources.MilliCPU, obs.DemandCPUMilli)
	}
	if p.NetSavingsMonthlyUSD <= 0 {
		return fmt.Errorf("ec2: %s proposes a change claiming %s/mo; a proposal must be net-positive or be a "+
			"suppression", a.Target.ID, fmtUSD(p.NetSavingsMonthlyUSD))
	}
	if p.NetSavingsMonthlyUSD > p.GrossSavingsMonthlyUSD+1e-6 {
		return fmt.Errorf("ec2: %s claims net %s/mo above gross %s/mo; net can never exceed the list-price delta",
			a.Target.ID, fmtUSD(p.NetSavingsMonthlyUSD), fmtUSD(p.GrossSavingsMonthlyUSD))
	}
	if obs.Burst.Class.Throttled() {
		return fmt.Errorf("ec2: %s is credit-throttled but carries a proposal", a.Target.ID)
	}
	if p.Confidence <= 0 {
		return fmt.Errorf("ec2: %s proposal has no confidence score", a.Target.ID)
	}
	return nil
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// WriteText renders a deterministic human summary. It is the shape
// `cloud-agent --domain ec2` prints and the UI card mirrors.
func (r *Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "kilter ec2 domain (read-only) — scope %s region %s\n",
		orNone(r.Scope), orNone(r.Region))
	fmt.Fprintf(tw, "generated %s over a %s window\n",
		r.GeneratedAt.UTC().Format(time.RFC3339), r.Window.String())
	if r.Stale {
		fmt.Fprintf(tw, "SNAPSHOT IS INCOMPLETE — affected instances are refused, not guessed\n")
	}
	t := r.Totals
	fmt.Fprintf(tw, "%d instances\t%d proposals\t%d refusals\t%d excluded\t%d advisories\n",
		t.Instances, t.Proposals, t.Refusals, t.Excluded, t.Advisories)
	fmt.Fprintf(tw, "current %s/mo\tnet savings %s/mo\t(list price would claim %s/mo)\n",
		fmtUSD(t.CurrentMonthlyUSD), fmtUSD(t.NetSavingsMonthlyUSD), fmtUSD(t.GrossSavingsMonthlyUSD))
	fmt.Fprintf(tw, "advisory-only (needs a human decision) %s/mo\n", fmtUSD(t.AdvisoryNetSavingsMonthlyUSD))
	fmt.Fprintf(tw, "%d of %d instances are memory-blind; %d are on 5-minute basic monitoring\n",
		t.MemoryBlind, t.Instances, t.CoarseResolution)
	fmt.Fprintln(tw)

	for _, a := range r.Assessments {
		fmt.Fprintf(tw, "%s\t%s\t%s/mo\n", a.Target.ID, a.Current.Attrs[AttrInstanceType],
			fmtUSD(a.CurrentMonthlyUSD))
		if a.Proposal != nil {
			fmt.Fprintf(tw, "  PROPOSE\t%s\tnet %s/mo\tconfidence %.2f\trisk %s (%s)\n",
				a.Proposal.InstanceType, fmtUSD(a.Proposal.NetSavingsMonthlyUSD),
				a.Proposal.Confidence, a.Proposal.Risk, a.Proposal.Action)
		}
		for _, s := range a.Suppressions {
			line := fmt.Sprintf("  REFUSE [%s]\t%s\n", s.Code, s.Reason)
			if !s.ValidFrom.IsZero() {
				line = fmt.Sprintf("  REFUSE [%s]\t%s\t(re-evaluate %s)\n",
					s.Code, s.Reason, s.ValidFrom.UTC().Format("2006-01-02"))
			}
			fmt.Fprint(tw, line)
		}
		for _, ad := range a.Advisories {
			fmt.Fprintf(tw, "  ADVISE [%s]\t%s\n", ad.Code, ad.Message)
			fmt.Fprintf(tw, "         caveat\t%s\n", ad.Caveat)
		}
		if a.Observation.ResolutionNote != "" {
			fmt.Fprintf(tw, "  observed\t%s\n", a.Observation.ResolutionNote)
		}
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(tw, "warning: %s\n", warn)
	}
	return tw.Flush()
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
