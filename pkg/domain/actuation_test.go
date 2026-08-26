package domain

import (
	"context"
	"errors"
	"testing"
	"time"
)

// hostileDomain is a domain that lies.
//
// It is the threat model this unit's wiring has to survive: a [Domain] is
// ordinary Go code somebody else wrote, and nothing stops it from claiming to
// be healthy, claiming to be actuatable, and handing back steps for targets it
// was never given — including targets belonging to ANOTHER domain, whose
// actuator would then be the one holding the credentials.
//
// Every field below is a separate lie, so each can be tested alone.
type hostileDomain struct {
	kind Kind
	// claimsActuatable makes Health report ReportOnly:false regardless of
	// whether anything was ever collected.
	claimsActuatable bool
	// planCalls counts how often the core let it near a plan.
	planCalls int
	// steps is what it hands back, unconditionally — including for
	// recommendations it was never given.
	steps []Step
}

func (h *hostileDomain) Kind() Kind            { return h.kind }
func (h *hostileDomain) Learn(*Snapshot) error { return nil }
func (h *hostileDomain) Recommend(time.Time, Netter) []Recommendation {
	return nil
}
func (h *hostileDomain) PlanSteps(recs []Recommendation, g Guard) ([]Step, error) {
	h.planCalls++
	return h.steps, nil
}
func (h *hostileDomain) Health(time.Time) Health {
	return Health{Kind: h.kind, Ready: true, ReportOnly: !h.claimsActuatable}
}
func (h *hostileDomain) Checkpoint() ([]byte, error) { return nil, nil }
func (h *hostileDomain) Restore([]byte) error        { return nil }

// recordingActuator is an actuator that records rather than acts, so a test
// can assert that nothing reached it.
type recordingActuator struct {
	kind     Kind
	executed []Step
	reverted []Step
	err      error
}

func (a *recordingActuator) Domain() Kind { return a.kind }
func (a *recordingActuator) Execute(_ context.Context, s Step) error {
	a.executed = append(a.executed, s)
	return a.err
}
func (a *recordingActuator) Revert(_ context.Context, s Step) error {
	a.reverted = append(a.reverted, s)
	return a.err
}

func stepFor(k Kind, id string) Step {
	t := TargetRef{Domain: k, Scope: "acct/us-east-1", ID: id}
	from := Spec{Attrs: map[string]string{"size": "big"}}
	to := Spec{Attrs: map[string]string{"size": "small"}}
	return Step{Seq: 1, Key: StepKey(t, from, to), Target: t,
		Action: ActionInPlace, From: from, To: to}
}

// TestHostileDomainClaimingActuatabilityIsBlockedByTheCore is requirement 3.
//
// The domain says it is ready and NOT report-only. The core believes that far
// enough to build a plan — a plan is for review, and refusing to even show one
// would hide the problem rather than contain it. What it does not do is let
// the plan run: execution routes through the actuator table, which only cmd/
// writes to, and the hostile domain has no entry in it.
func TestHostileDomainClaimingActuatabilityIsBlockedByTheCore(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	h := &hostileDomain{kind: ECSFargate, claimsActuatable: true}
	r := NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatal(err)
	}

	if r.CanActuate(ECSFargate) {
		t.Fatal("a domain granted itself actuation capability")
	}
	step := stepFor(ECSFargate, "prod/web")
	err := r.Execute(context.Background(), Guard{Now: now}, step)
	if !errors.Is(err, ErrReportOnly) {
		t.Fatalf("Execute on a domain with no actuator = %v, want ErrReportOnly", err)
	}
	if err := r.Revert(context.Background(), Guard{Now: now}, step); !errors.Is(err, ErrReportOnly) {
		t.Fatalf("Revert on a domain with no actuator = %v, want ErrReportOnly", err)
	}

	// And the plan says so, rather than looking like a runnable one.
	rec := validRec()
	rec.Target.Domain = ECSFargate
	h.steps = []Step{step}
	p := r.BuildPlan(ECSFargate, []Recommendation{rec}, Guard{Now: now})
	if p.Actuatable {
		t.Error("plan claims to be actuatable with no actuator wired")
	}
	if len(p.Steps) == 0 {
		t.Error("a plan should still be reviewable; refusing to show it hides the problem")
	}
}

// TestReportOnlyDomainNeverReachesItsActuator closes the other half: even
// WITH an actuator wired, a domain whose health says report-only cannot
// execute. Actuation needs both — a wired path and a willing domain — and
// either one alone is not enough.
func TestReportOnlyDomainNeverReachesItsActuator(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	h := &hostileDomain{kind: ECSFargate, claimsActuatable: false} // honest, for once
	act := &recordingActuator{kind: ECSFargate}
	r := NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterActuator(act); err != nil {
		t.Fatal(err)
	}
	if !r.CanActuate(ECSFargate) {
		t.Fatal("CanActuate = false after RegisterActuator")
	}
	err := r.Execute(context.Background(), Guard{Now: now}, stepFor(ECSFargate, "prod/web"))
	if !errors.Is(err, ErrReportOnly) {
		t.Fatalf("Execute on a report-only domain = %v, want ErrReportOnly", err)
	}
	if len(act.executed) != 0 {
		t.Fatalf("the actuator was reached %d time(s) despite report-only", len(act.executed))
	}
}

// TestDomainCannotEmitAStepForAnotherDomain is the escalation the core has to
// refuse. A step is routed for execution by Step.Target.Domain, so a domain
// that returns a step naming a DIFFERENT domain would be borrowing that
// domain's actuator — and its credentials. Filtering the input is not enough;
// the output has to be checked too.
func TestDomainCannotEmitAStepForAnotherDomain(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	h := &hostileDomain{
		kind:             ECSFargate,
		claimsActuatable: true,
		steps:            []Step{stepFor(EC2, "i-victim")}, // another domain's target
	}
	victim := &recordingActuator{kind: EC2}
	r := NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterActuator(victim); err != nil {
		t.Fatal(err)
	}
	rec := validRec()
	rec.Target.Domain = ECSFargate

	steps, err := r.PlanSteps(ECSFargate, []Recommendation{rec}, Guard{Now: now})
	if !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("PlanSteps = (%v, %v), want ErrWrongDomain", steps, err)
	}
	if len(steps) != 0 {
		t.Fatal("a cross-domain step escaped the core")
	}
	p := r.BuildPlan(ECSFargate, []Recommendation{rec}, Guard{Now: now})
	if p.RefusalCode != RefuseDomainError {
		t.Errorf("BuildPlan refusal = %q, want %q", p.RefusalCode, RefuseDomainError)
	}
	if len(victim.executed) != 0 {
		t.Fatal("the victim domain's actuator was reached")
	}
}

// TestStepsWithoutAnIdempotencyKeyAreRefused: a step with no key cannot be
// resumed, so re-running a partly-applied plan would re-apply it. The core
// refuses rather than generating one, because a key it invented would not
// match the one the domain's actuator computes.
func TestStepsWithoutAnIdempotencyKeyAreRefused(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	bad := stepFor(ECSFargate, "prod/web")
	bad.Key = ""
	h := &hostileDomain{kind: ECSFargate, claimsActuatable: true, steps: []Step{bad}}
	r := NewRegistry()
	if err := r.Register(h); err != nil {
		t.Fatal(err)
	}
	rec := validRec()
	rec.Target.Domain = ECSFargate
	if _, err := r.PlanSteps(ECSFargate, []Recommendation{rec}, Guard{Now: now}); err == nil {
		t.Fatal("a step with no idempotency key was accepted")
	}

	worse := stepFor(ECSFargate, "prod/web")
	worse.Action = "sneaky-in-place"
	h.steps = []Step{worse}
	if _, err := r.PlanSteps(ECSFargate, []Recommendation{rec}, Guard{Now: now}); err == nil {
		t.Fatal("a step with an invalid action class was accepted")
	}
}

// TestExecuteRefusesGuardrailsAtExecutionTime: a plan that was legal when it
// was built is refused when it is executed late. The guard is re-evaluated,
// not remembered.
func TestExecuteRefusesGuardrailsAtExecutionTime(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	d := ready(ECSFargate)
	act := &recordingActuator{kind: ECSFargate}
	r := NewRegistry()
	if err := r.Register(d); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterActuator(act); err != nil {
		t.Fatal(err)
	}
	step := stepFor(ECSFargate, "prod/web")

	for _, tc := range []struct {
		name string
		g    Guard
		want error
	}{
		{"freeze", Guard{Now: now, Freeze: true}, ErrFrozen},
		{"breaker", Guard{Now: now, BreakerOpen: true}, ErrBreakerOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Execute(context.Background(), tc.g, step); !errors.Is(err, tc.want) {
				t.Fatalf("Execute = %v, want %v", err, tc.want)
			}
			if len(act.executed) != 0 {
				t.Fatal("the actuator ran under a refusing guardrail")
			}
		})
	}

	// Clean guard: it goes through.
	if err := r.Execute(context.Background(), Guard{Now: now}, step); err != nil {
		t.Fatalf("Execute under a clean guard = %v", err)
	}
	if len(act.executed) != 1 {
		t.Fatalf("actuator executed %d steps, want 1", len(act.executed))
	}

	// Revert is NOT gated by freeze: it is the safety path, and freeze is
	// usually on BECAUSE something is wrong.
	if err := r.Revert(context.Background(), Guard{Now: now, Freeze: true}, step); err != nil {
		t.Fatalf("Revert under freeze = %v, want it to be allowed", err)
	}
	if len(act.reverted) != 1 {
		t.Fatalf("actuator reverted %d steps, want 1", len(act.reverted))
	}
}

// TestRegisterActuatorRejectsWiringBugs mirrors Register's contract.
func TestRegisterActuatorRejectsWiringBugs(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterActuator(nil); err == nil {
		t.Error("RegisterActuator(nil) was accepted")
	}
	if err := r.RegisterActuator(&recordingActuator{kind: "made-up"}); err == nil {
		t.Error("an actuator with an unknown kind was accepted")
	}
	if err := r.RegisterActuator(&recordingActuator{kind: EC2}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterActuator(&recordingActuator{kind: EC2}); err == nil {
		t.Error("a second actuator for one kind was accepted")
	}
	var nilReg *Registry
	if err := nilReg.RegisterActuator(&recordingActuator{kind: EC2}); err == nil {
		t.Error("RegisterActuator on a nil registry was accepted")
	}
	if nilReg.CanActuate(EC2) {
		t.Error("a nil registry claims actuation capability")
	}
	if got := nilReg.ActuatableKinds(); got != nil {
		t.Errorf("nil registry ActuatableKinds = %v", got)
	}
}

// TestExecuteRefusesUnregisteredDomains: a step for a domain nobody wired is
// reported, never silently skipped.
func TestExecuteRefusesUnregisteredDomains(t *testing.T) {
	r := NewRegistry()
	err := r.Execute(context.Background(), Guard{}, stepFor(Lambda, "fn"))
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("Execute = %v, want ErrNotRegistered", err)
	}
}

// TestActuatableKindsIsCanonicallyOrdered pins the determinism rule at the
// only place this file introduces a map.
func TestActuatableKindsIsCanonicallyOrdered(t *testing.T) {
	r := NewRegistry()
	for _, k := range []Kind{Lambda, EC2, K8sFargate} {
		if err := r.RegisterActuator(&recordingActuator{kind: k}); err != nil {
			t.Fatal(err)
		}
	}
	want := []Kind{EC2, K8sFargate, Lambda}
	for i := 0; i < 50; i++ {
		got := r.ActuatableKinds()
		if len(got) != len(want) {
			t.Fatalf("ActuatableKinds = %v", got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("ActuatableKinds = %v, want %v", got, want)
			}
		}
	}
}
