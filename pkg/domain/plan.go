package domain

import (
	"errors"
	"time"
)

// Refusal codes carried by [Plan.RefusalCode]. Like every other code in this
// package they are stable strings meant to be stored and matched on, unlike
// the prose beside them.
const (
	// RefuseNotRegistered: nobody wired a domain for this kind.
	RefuseNotRegistered = "not-registered"
	// RefuseReportOnly: the domain may recommend but must not act.
	RefuseReportOnly = "report-only"
	// RefuseGuardrail: freeze, the circuit breaker or the change window said
	// no. The prose names which.
	RefuseGuardrail = "guardrail"
	// RefuseDomainError: the domain failed to plan. This is a bug, not a
	// policy, and it is reported as one.
	RefuseDomainError = "domain-error"
	// RefuseNothingToDo: no applicable recommendation. Recorded rather than
	// left blank, because "no steps" and "no answer" must not look the same.
	RefuseNothingToDo = "nothing-to-do"
)

// Plan is one domain's executable output plus, when there is none, the reason.
//
// A plan with no steps is a normal answer and carries a code saying which kind
// of normal it is. Callers render RefusalCode/Refusal; they do not treat an
// empty Steps slice as silence.
type Plan struct {
	Kind  Kind   `json:"kind"`
	Scope string `json:"scope,omitempty"`

	Steps []Step `json:"steps,omitempty"`
	// Fingerprint is the deterministic content hash of Steps — the same
	// approach `kilter approve` uses for node plans. Empty when there are no
	// steps, so an empty plan can never be approved by accident.
	Fingerprint string `json:"fingerprint,omitempty"`

	// Actuatable reports whether an actuator is wired for this kind. It is
	// NOT derived from the domain: see actuation.go. A plan with steps and
	// Actuatable false is a rehearsal — reviewable, never runnable, and the
	// renderer says so.
	Actuatable bool `json:"actuatable"`

	RefusalCode string `json:"refusalCode,omitempty"`
	Refusal     string `json:"refusal,omitempty"`

	// Considered is how many applicable (non-suppressed, in-domain)
	// recommendations were offered to the domain.
	Considered int `json:"considered"`
	// Suppressed is how many were withheld because they must never be applied.
	Suppressed int `json:"suppressed"`
}

// Disruptive counts the steps that restart or stop their target.
func (p Plan) Disruptive() int {
	n := 0
	for _, s := range p.Steps {
		if s.Action.Disruptive() {
			n++
		}
	}
	return n
}

// BuildPlan turns one domain's recommendations into a reviewable plan.
//
// It never returns an error: a refusal is the product. Every way this can
// decline — unregistered, report-only, frozen, outside the window, nothing
// applicable, or a domain that failed outright — comes back as a code and
// prose on the Plan, because a caller that has to distinguish "the domain
// refused" from "the call failed" will eventually get it wrong in the
// direction of acting.
//
// It routes through [Registry.PlanSteps] and nowhere else, so the core's
// report-only gate, its suppressed/foreign filter and its validation of what
// the domain handed back all apply. This method adds no capability; it adds a
// shape.
//
// Building a plan is NOT permission to run it. Steps are produced for review
// whether or not an actuator exists; Actuatable records which case this is,
// and [Registry.Execute] is where the difference bites.
func (r *Registry) BuildPlan(k Kind, recs []Recommendation, g Guard) Plan {
	p := Plan{Kind: k, Actuatable: r.CanActuate(k)}
	for _, rec := range recs {
		if rec.Target.Domain != k {
			continue
		}
		if p.Scope == "" {
			p.Scope = rec.Target.Scope
		}
		if rec.Suppressed {
			p.Suppressed++
			continue
		}
		p.Considered++
	}

	steps, err := r.PlanSteps(k, recs, g)
	switch {
	case errors.Is(err, ErrNotRegistered):
		p.RefusalCode, p.Refusal = RefuseNotRegistered, err.Error()
		return p
	case errors.Is(err, ErrReportOnly):
		p.RefusalCode, p.Refusal = RefuseReportOnly, err.Error()
		return p
	case errors.Is(err, ErrFrozen), errors.Is(err, ErrBreakerOpen), errors.Is(err, ErrOutsideWindow):
		p.RefusalCode, p.Refusal = RefuseGuardrail, err.Error()
		return p
	case err != nil:
		p.RefusalCode, p.Refusal = RefuseDomainError, err.Error()
		return p
	case len(steps) == 0:
		p.RefusalCode = RefuseNothingToDo
		p.Refusal = nothingToDoReason(p.Considered, p.Suppressed)
		return p
	}
	p.Steps = steps
	p.Fingerprint = Fingerprint(steps)
	return p
}

// nothingToDoReason explains an empty step list in the terms the reader cares
// about: was there nothing to do, or was everything withheld?
func nothingToDoReason(considered, suppressed int) string {
	switch {
	case considered == 0 && suppressed > 0:
		return "every recommendation for this domain is suppressed and must not be applied"
	case considered == 0:
		return "no applicable recommendations"
	default:
		return "the domain produced no executable step for its applicable recommendations"
	}
}

// BuildPlans builds one plan per registered kind, in canonical order. Domains
// that refuse are present in the result with their reason, never omitted.
func (r *Registry) BuildPlans(recs []Recommendation, g Guard) []Plan {
	kinds := r.Kinds()
	if len(kinds) == 0 {
		return nil
	}
	out := make([]Plan, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, r.BuildPlan(k, recs, g))
	}
	return out
}

// PlanAt is a convenience for callers that hold only a clock.
func (r *Registry) PlanAt(k Kind, now time.Time, recs []Recommendation) Plan {
	return r.BuildPlan(k, recs, Guard{Now: now})
}
