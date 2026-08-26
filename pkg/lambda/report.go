package lambda

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Totals summarize a report. Advisory money is counted separately from claimed
// money, because an advisory is not a plan: adding an unmeasured ARM rate delta
// to a measured saving would present a guess as a forecast.
type Totals struct {
	Functions  int `json:"functions"`
	Excluded   int `json:"excluded"`
	Proposals  int `json:"proposals"`
	Refusals   int `json:"refusals"`
	Advisories int `json:"advisories"`
	// SinglePoint counts functions whose cost effect is unknowable from the
	// evidence collected — the headline number of this domain.
	SinglePoint int `json:"singlePoint"`
	// AtCeiling counts functions whose memory measurement may be truncated.
	AtCeiling int `json:"atCeiling"`
	// CostKnown counts functions whose CURRENT bill is measured.
	CostKnown int `json:"costKnown"`

	CurrentMonthlyUSD      float64 `json:"currentMonthlyUSD"`
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	NetSavingsMonthlyUSD   float64 `json:"netSavingsMonthlyUSD"`
	// AdvisoryRateDeltaMonthlyUSD is money that would require a human decision
	// and a measurement this domain does not have (an architecture port). It is
	// reported, never claimed, and never summed with NetSavingsMonthlyUSD.
	AdvisoryRateDeltaMonthlyUSD float64 `json:"advisoryRateDeltaMonthlyUSD"`
	// SuppressedByCode counts refusal reasons so a UI can show what the domain
	// declined to do, and why, without walking every assessment.
	SuppressedByCode map[string]int `json:"suppressedByCode,omitempty"`
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
	// Assessments is sorted by function ARN.
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

// For returns the assessment for a function ARN.
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
		t.Functions++
		if a.Excluded() {
			t.Excluded++
		}
		if a.CostKnown {
			t.CostKnown++
			t.CurrentMonthlyUSD += a.CurrentMonthlyUSD
		}
		t.Advisories += len(a.Advisories)
		for _, s := range a.Suppressions {
			t.SuppressedByCode[s.Code]++
		}
		for _, ad := range a.Advisories {
			if ad.RateDeltaMonthlyUSD > 0 {
				t.AdvisoryRateDeltaMonthlyUSD += ad.RateDeltaMonthlyUSD
			}
		}
		if a.Suppressed(ReasonSingleMemoryPoint) {
			t.SinglePoint++
		}
		if a.Observation.AtCeiling {
			t.AtCeiling++
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
// exported and cheap on purpose: the tests call it, the fuzz target calls it,
// and it is the right thing for a later unit to call before persisting a report
// or serving it. A violation is a bug in this package, not bad input.
func (r *Report) Validate() error {
	if r.Domain != Kind {
		return fmt.Errorf("lambda: report domain %q, want %q", r.Domain, Kind)
	}
	var lastID string
	for i, a := range r.Assessments {
		if i > 0 && a.Target.ID < lastID {
			return fmt.Errorf("lambda: assessments are not sorted by ARN (%q after %q)", a.Target.ID, lastID)
		}
		lastID = a.Target.ID

		if len(a.Evidence) == 0 {
			return fmt.Errorf("lambda: %s has no evidence; every assessment must state what it saw", a.Target.ID)
		}
		if a.Proposal == nil && len(a.Suppressions) == 0 {
			return fmt.Errorf("lambda: %s proposes nothing and gives no reason; silence is not an output",
				a.Target.ID)
		}
		for _, ad := range a.Advisories {
			if ad.Caveat == "" {
				return fmt.Errorf("lambda: %s advisory %q has no caveat; an advisory without its caveat reads "+
					"as an actionable saving", a.Target.ID, ad.Code)
			}
			if ad.Actuatable() || ad.Action() != domain.ActionAdvisory {
				return fmt.Errorf("lambda: %s advisory %q claims to be actuatable", a.Target.ID, ad.Code)
			}
		}
		if a.Excluded() && a.Proposal != nil {
			return fmt.Errorf("lambda: %s is excluded from this domain but carries a proposal", a.Target.ID)
		}
		if err := validateProposal(a); err != nil {
			return err
		}
	}
	want := r.computeTotals()
	if want.Proposals != r.Totals.Proposals || want.Functions != r.Totals.Functions ||
		!nearly(want.NetSavingsMonthlyUSD, r.Totals.NetSavingsMonthlyUSD) {
		return fmt.Errorf("lambda: totals do not match the assessments they summarize")
	}
	return nil
}

// validateProposal encodes the rule this package exists for. Read it as the
// specification: a proposal is only well-formed when a measurement exists AT
// the memory setting it proposes.
func validateProposal(a Assessment) error {
	p := a.Proposal
	if p == nil {
		return nil
	}
	obs := a.Observation
	id := a.Target.ID

	// THE RULE. No cost claim without a measured duration at the proposed
	// setting — enforced structurally, not by convention.
	point, measured := obs.PointAt(p.MemoryMB)
	if !measured {
		return fmt.Errorf("lambda: %s proposes %s with no measurement at that setting; a saving without a "+
			"measured duration at the proposed memory is a guess", id, fmtMB(p.MemoryMB))
	}
	if p.MeasuredSamples <= 0 || point.Warm != p.MeasuredSamples {
		return fmt.Errorf("lambda: %s proposal at %s claims %d warm samples but the measurement has %d",
			id, fmtMB(p.MemoryMB), p.MeasuredSamples, point.Warm)
	}
	if !nearly(point.MeanBilledMS, p.MeasuredBilledMS) {
		return fmt.Errorf("lambda: %s proposal at %s carries a billed duration measured somewhere else "+
			"(%v vs %v)", id, fmtMB(p.MemoryMB), p.MeasuredBilledMS, point.MeanBilledMS)
	}
	if !a.CostKnown {
		return fmt.Errorf("lambda: %s proposes a change without a measured current bill", id)
	}
	if _, ok := obs.Current(); !ok {
		return fmt.Errorf("lambda: %s proposes a change with no measurement at the current setting", id)
	}

	// Safety: never below the observed memory floor, never onto a setting whose
	// own memory measurement was truncated.
	if p.MemoryMB < obs.MemoryFloorMB {
		return fmt.Errorf("lambda: %s proposes %s, below the %s memory floor",
			id, fmtMB(p.MemoryMB), fmtMB(obs.MemoryFloorMB))
	}
	if point.AtCeiling {
		return fmt.Errorf("lambda: %s proposes %s, a setting whose own memory measurement was truncated",
			id, fmtMB(p.MemoryMB))
	}
	if obs.AtCeiling {
		return fmt.Errorf("lambda: %s is at its memory ceiling but carries a proposal", id)
	}
	if p.MemoryMB < MinMemoryMB || p.MemoryMB > MaxMemoryMB {
		return fmt.Errorf("lambda: %s proposes %s, outside the platform range", id, fmtMB(p.MemoryMB))
	}

	// Read-only, structurally: the only action class this domain may emit.
	if p.Action != domain.ActionAdvisory {
		return fmt.Errorf("lambda: %s proposal has action %q; this domain is advisory only", id, p.Action)
	}

	// Money.
	if p.NetSavingsMonthlyUSD <= 0 {
		return fmt.Errorf("lambda: %s proposes a change claiming %s/mo; a proposal must be net-positive or be "+
			"a suppression", id, fmtUSD(p.NetSavingsMonthlyUSD))
	}
	if p.NetSavingsMonthlyUSD > p.GrossSavingsMonthlyUSD+1e-6 {
		return fmt.Errorf("lambda: %s claims net %s/mo above gross %s/mo; net can never exceed the list-price "+
			"delta", id, fmtUSD(p.NetSavingsMonthlyUSD), fmtUSD(p.GrossSavingsMonthlyUSD))
	}
	if p.ProposedHourlyUSD >= a.CurrentHourlyUSD-eps {
		return fmt.Errorf("lambda: %s proposes a setting that measured no cheaper (%s/h vs %s/h)",
			id, fmtUSD(p.ProposedHourlyUSD), fmtUSD(a.CurrentHourlyUSD))
	}
	if p.Confidence <= 0 {
		return fmt.Errorf("lambda: %s proposal has no confidence score", id)
	}
	return nil
}

func nearly(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// WriteText renders a deterministic human summary. It is the shape
// `cloud-agent --domain lambda` prints and the UI card mirrors.
func (r *Report) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "kilter lambda domain (advisory only, read-only) — scope %s region %s\n",
		orNone(r.Scope), orNone(r.Region))
	fmt.Fprintf(tw, "generated %s over a %s window\n",
		r.GeneratedAt.UTC().Format(time.RFC3339), r.Window.String())
	if r.Stale {
		fmt.Fprintf(tw, "SNAPSHOT IS INCOMPLETE — affected functions are refused, not guessed\n")
	}
	t := r.Totals
	fmt.Fprintf(tw, "%d functions\t%d proposals\t%d refusals\t%d excluded\t%d advisories\n",
		t.Functions, t.Proposals, t.Refusals, t.Excluded, t.Advisories)
	fmt.Fprintf(tw, "current %s/mo across %d functions whose bill is measured\tnet savings %s/mo\t"+
		"(list price would claim %s/mo)\n",
		fmtUSD(t.CurrentMonthlyUSD), t.CostKnown, fmtUSD(t.NetSavingsMonthlyUSD),
		fmtUSD(t.GrossSavingsMonthlyUSD))
	fmt.Fprintf(tw, "advisory-only rate deltas (unmeasured, never claimed) %s/mo\n",
		fmtUSD(t.AdvisoryRateDeltaMonthlyUSD))
	fmt.Fprintf(tw, "%d of %d functions have only one measured memory setting: their cost effect is UNKNOWN\n",
		t.SinglePoint, t.Functions)
	fmt.Fprintf(tw, "%d functions sit at their memory ceiling (possible truncation)\n", t.AtCeiling)
	fmt.Fprintln(tw)

	for _, a := range r.Assessments {
		cost := "cost unmeasured"
		if a.CostKnown {
			cost = fmtUSD(a.CurrentMonthlyUSD) + "/mo"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Function.DisplayName(), fmtMB(a.Function.MemoryMB),
			a.Function.Arch(), cost)
		if p := a.Proposal; p != nil {
			fmt.Fprintf(tw, "  ADVISE\t%s\tnet %s/mo\tconfidence %.2f\trisk %s (%s)\n",
				fmtMB(p.MemoryMB), fmtUSD(p.NetSavingsMonthlyUSD), p.Confidence, p.Risk, p.Action)
			fmt.Fprintf(tw, "         measured\t%s over %d warm invocations at %s\n",
				fmtMS(p.MeasuredBilledMS), p.MeasuredSamples, fmtMB(p.MemoryMB))
		}
		for _, s := range a.Suppressions {
			if s.ValidFrom.IsZero() {
				fmt.Fprintf(tw, "  REFUSE [%s]\t%s\n", s.Code, s.Reason)
				continue
			}
			fmt.Fprintf(tw, "  REFUSE [%s]\t%s\t(re-evaluate %s)\n",
				s.Code, s.Reason, s.ValidFrom.UTC().Format("2006-01-02"))
		}
		for _, ad := range a.Advisories {
			fmt.Fprintf(tw, "  ADVISE [%s]\t%s\n", ad.Code, ad.Message)
			fmt.Fprintf(tw, "         caveat\t%s\n", ad.Caveat)
		}
		if len(a.Observation.Points) > 0 {
			fmt.Fprintf(tw, "  measured settings\t%s\n", memoryPointsValue(a.Observation.Points))
		}
		for _, d := range a.Observation.dropLines() {
			fmt.Fprintf(tw, "  dropped\t%s\n", d)
		}
	}
	for _, warn := range r.Warnings {
		fmt.Fprintf(tw, "warning: %s\n", warn)
	}
	return tw.Flush()
}

// dropLines renders the parse-drop summary, or nothing when the log parsed
// cleanly.
func (o Observation) dropLines() []string {
	if o.Dropped == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d REPORT line(s) did not parse and were excluded from every number above",
		o.Dropped)}
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
