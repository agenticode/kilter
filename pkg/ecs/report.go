package ecs

import (
	"fmt"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Report is one pass of the sizer over one snapshot: every observed service,
// assessed, in a deterministic order.
type Report struct {
	Domain      domain.Kind  `json:"domain"`
	Scope       string       `json:"scope"`
	Cluster     string       `json:"cluster"`
	GeneratedAt time.Time    `json:"generatedAt"`
	Window      Window       `json:"window"`
	Assessments []Assessment `json:"assessments,omitempty"`
	// ClaimableMonthlyUSD is the sum of net savings from proposals that are
	// actually applicable. Advisory estimates are excluded by construction.
	ClaimableMonthlyUSD float64 `json:"claimableMonthlyUSD"`
	// AdvisoryMonthlyUSD is what the ECS-only levers would save if their
	// unverifiable preconditions hold. It is reported separately from the
	// claimable total and never added to it.
	AdvisoryMonthlyUSD float64  `json:"advisoryMonthlyUSD"`
	Stale              bool     `json:"stale,omitempty"`
	StaleReason        string   `json:"staleReason,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
}

// Assess turns a snapshot into a report. Assessments come out sorted by target
// ID, so shuffling the snapshot's services, tags or metric results cannot
// change a byte of the output — pinned by TestReportIsShuffleInvariant.
func (s *Sizer) Report(snap *Snapshot, now time.Time, ledger domain.Netter) *Report {
	r := &Report{Domain: Kind, GeneratedAt: now}
	if snap == nil {
		r.Stale, r.StaleReason = true, "no snapshot: the ECS collector is absent or has not reported"
		return r
	}
	r.Scope, r.Cluster, r.Window = snap.Scope, snap.Cluster, snap.Window
	r.Stale, r.StaleReason = snap.Stale, snap.StaleReason
	r.Warnings = append(r.Warnings, snap.Warnings...)

	r.Assessments = make([]Assessment, 0, len(snap.Services))
	for _, o := range snap.Services {
		r.Assessments = append(r.Assessments, s.Assess(o, now, ledger))
	}
	sort.SliceStable(r.Assessments, func(i, j int) bool {
		return r.Assessments[i].Ref.Compare(r.Assessments[j].Ref) < 0
	})
	// The totals are summed AFTER sorting, not during collection. Floating-point
	// addition is not associative, so a sum accumulated in arrival order would
	// differ in the last bits between two runs over the same data — enough to
	// change a rendered report and to break a byte-comparing store.
	for i := range r.Assessments {
		a := &r.Assessments[i]
		r.ClaimableMonthlyUSD += a.ClaimableMonthlyUSD()
		for _, ad := range a.Advisories {
			if ad.EstimatedMonthlyUSD > 0 {
				r.AdvisoryMonthlyUSD += ad.EstimatedMonthlyUSD
			}
		}
	}
	return r
}

// Validate reports the first structural problem with a report. It is the
// contract the package promises: every assessment either proposes a change or
// says why it does not, every assessment carries evidence, and no advisory
// estimate leaks into a claimable saving.
func (r *Report) Validate() error {
	if r == nil {
		return fmt.Errorf("ecs: nil report")
	}
	for i := range r.Assessments {
		a := &r.Assessments[i]
		switch {
		case a.Ref.ID == "":
			return fmt.Errorf("ecs: assessment %d has no target ID", i)
		case len(a.Evidence) == 0:
			return fmt.Errorf("ecs: assessment %s has no evidence", a.Ref)
		case a.Proposal == nil && len(a.Suppressions) == 0:
			return fmt.Errorf("ecs: assessment %s proposes nothing and gives no reason", a.Ref)
		case a.Proposal != nil && a.Proposal.NetMonthlyUSD > a.Proposal.GrossMonthlyUSD:
			return fmt.Errorf("ecs: assessment %s claims net $%v > gross $%v",
				a.Ref, a.Proposal.NetMonthlyUSD, a.Proposal.GrossMonthlyUSD)
		case a.Proposal != nil && a.Proposal.Tier == a.CurrentTier:
			return fmt.Errorf("ecs: assessment %s proposes the tier it is already on", a.Ref)
		}
		for _, s := range a.Suppressions {
			if s.Code == "" {
				return fmt.Errorf("ecs: assessment %s is suppressed without a code", a.Ref)
			}
		}
		for _, ad := range a.Advisories {
			if ad.Code == "" || ad.Caveat == "" {
				return fmt.Errorf("ecs: assessment %s has an advisory without a code or caveat", a.Ref)
			}
		}
	}
	return nil
}

// Recommendations projects the report into the domain-generic shape, so a later
// unit can hand ECS recommendations to the same registry, plan, approval and
// ledger machinery every other domain uses.
//
// Both suppressed proposals and advisories are included, because both must stay
// visible: a recommendation that vanishes is indistinguishable from a bug.
// Suppressed ones carry their code, and advisories are [domain.ActionAdvisory],
// which no executor will ever plan.
func (r *Report) Recommendations() []domain.Recommendation {
	if r == nil {
		return nil
	}
	var out []domain.Recommendation
	for i := range r.Assessments {
		a := &r.Assessments[i]
		if a.Proposal != nil {
			rec := domain.Recommendation{
				Target:            a.Ref,
				Current:           a.Current,
				Proposed:          a.Proposal.Spec,
				CurrentHourlyUSD:  a.CurrentHourlyUSD,
				ProposedHourlyUSD: a.Proposal.HourlyUSD,
				Action:            a.Proposal.Action,
				Risk:              a.Proposal.Risk,
				Confidence:        a.Confidence,
				Evidence:          a.Evidence,
				Reason:            a.Proposal.Reason,
			}
			rec.SetSavings(a.Proposal.GrossMonthlyUSD, a.Proposal.NetMonthlyUSD)
			if len(a.Suppressions) > 0 {
				s := a.Suppressions[0]
				rec.Suppressed, rec.SuppressCode = true, s.Code
				rec.ValidFrom = s.ValidFrom
				rec.Reason = s.Reason + "; " + rec.Reason
			}
			out = append(out, rec)
		}
		for _, ad := range a.Advisories {
			rec := domain.Recommendation{
				Target:            a.Ref,
				Current:           a.Current,
				Proposed:          ad.Proposed,
				CurrentHourlyUSD:  a.CurrentHourlyUSD,
				ProposedHourlyUSD: a.CurrentHourlyUSD,
				Action:            domain.ActionAdvisory,
				Risk:              "medium",
				Confidence:        a.Confidence,
				Evidence:          advisoryEvidence(a, ad),
				Reason:            ad.Code + ": " + ad.Detail + " — " + ad.Caveat,
				// An advisory is always suppressed. Its precondition is not
				// observable, so it may never become a step, and its estimate
				// may never become a claim: SetSavings(0, 0) below is what
				// keeps ClaimableMonthlyUSD honest.
				Suppressed:   true,
				SuppressCode: ad.Code,
			}
			rec.SetSavings(0, 0)
			out = append(out, rec)
		}
	}
	domain.SortRecommendations(out)
	return out
}

// advisoryEvidence guarantees an advisory recommendation has evidence, which
// domain.Recommendation.Validate requires of everything.
func advisoryEvidence(a *Assessment, ad Advisory) []domain.Evidence {
	ev := append([]domain.Evidence(nil), ad.Evidence...)
	ev = append(ev, domain.Evidence{
		Metric: "advisory-estimate",
		Value:  fmt.Sprintf("$%.2f/mo if the caveat holds", ad.EstimatedMonthlyUSD),
		Source: SourceQuantizer,
	})
	if len(a.Evidence) > 0 {
		ev = append(ev, a.Evidence[0])
	}
	return ev
}

// PlanSteps orders applicable recommendations into executable steps.
//
// Three rules have teeth here:
//
//   - an advisory never becomes a step. Fargate Spot and ARM64 are legal on ECS
//     and Kilter prices them, but their preconditions live outside CloudWatch,
//     so they stay advice;
//   - a suppressed recommendation never becomes a step;
//   - every ECS task-size change is [domain.ActionRolling]. There is no
//     in-place resize: a new revision means a new deployment, which means new
//     tasks. A step claiming otherwise would understate disruption to whatever
//     executes it.
func PlanSteps(recs []domain.Recommendation, g domain.Guard) ([]domain.Step, error) {
	if err := g.Allow(); err != nil {
		return nil, err
	}
	applicable := make([]domain.Recommendation, 0, len(recs))
	for _, r := range recs {
		if r.Suppressed || r.Action == domain.ActionAdvisory {
			continue
		}
		if r.Target.Domain != "" && r.Target.Domain != Kind {
			return nil, fmt.Errorf("ecs: recommendation for %q handed to the ecs-fargate domain", r.Target.Domain)
		}
		if r.Action != domain.ActionRolling {
			return nil, fmt.Errorf("ecs: recommendation for %s has action %q; every ECS task-size change is %q",
				r.Target, r.Action, domain.ActionRolling)
		}
		applicable = append(applicable, r)
	}
	if len(applicable) == 0 {
		return nil, nil
	}
	domain.SortRecommendations(applicable)

	out := make([]domain.Step, 0, len(applicable))
	for _, r := range applicable {
		if g.MaxSteps > 0 && len(out) >= g.MaxSteps {
			break
		}
		out = append(out, domain.Step{
			Seq:    len(out) + 1,
			Key:    domain.StepKey(r.Target, r.Current, r.Proposed),
			Target: r.Target,
			Action: r.Action,
			// From carries the current task-definition ARN including its
			// revision. That is the rollback target: reverting is an
			// UpdateService back to this exact revision, never a new one.
			From:   r.Current,
			To:     r.Proposed,
			Risk:   r.Risk,
			Detail: fmt.Sprintf("%s → %s (%s)", r.Current.Attr(AttrTaskSize), r.Proposed.Attr(AttrTaskSize), r.Reason),
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
