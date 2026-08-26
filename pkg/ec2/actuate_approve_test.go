package ec2

// Approval enforcement.
//
// The claim under test is that an unapproved step is UNREPRESENTABLE, not
// merely rejected. Go has no way to forbid a zero value, so the claim is
// discharged in two halves:
//
//   - Compile-time: no exported constructor for [ApprovedStep] other than
//     [Approval.Authorize], all fields unexported, and `*Actuator` does not
//     satisfy [domain.Actuator] so the registry cannot drive it. A test cannot
//     assert a compile error, so those are pinned by the runtime tests below
//     plus the assertions in TestActuatorIsNotRegistrableWithoutApproval —
//     which fail the moment somebody adds an Execute(ctx, Step) method to
//     *Actuator to "make it easier to call".
//   - Runtime: everything a determined caller CAN construct — the zero value,
//     a token for another plan, an expired token, a step appended after
//     approval — is refused here, with the account untouched.

import (
	"errors"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// The one a caller can build without this package's help. It must not act.
func TestZeroApprovedStepCannotAct(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeApply, nil)

	var zero ApprovedStep
	if zero.Approved() {
		t.Fatal("the zero ApprovedStep reports itself approved")
	}
	if err := a.Execute(t.Context(), zero); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Execute(zero): err = %v, want ErrNotApproved", err)
	}
	if err := a.Revert(t.Context(), zero); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Revert(zero): err = %v, want ErrNotApproved", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("an unapproved step issued %d mutating call(s): %v", n, f.Ops())
	}
	runningAs(t, f, "i-app", "m5.2xlarge")

	// A hand-built ApprovedStep carrying a real step but no approval is the
	// obvious way to try to slip past the gate from inside the package. It
	// must not work either.
	forged := ApprovedStep{step: actStep(actStepOpts{})}
	if err := a.Execute(t.Context(), forged); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Execute(forged): err = %v, want ErrNotApproved", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("a forged approval issued %d mutating call(s)", n)
	}
}

// The bare actuator must not satisfy domain.Actuator: the registry's execution
// path takes a plain Step, and a type that accepts one has no gate left.
func TestActuatorIsNotRegistrableWithoutApproval(t *testing.T) {
	clock := newActClock(actBase)
	a := newActActuator(t, newActFixture(clock), clock, ModeApply, nil)

	if _, ok := any(a).(domain.Actuator); ok {
		t.Fatal("*Actuator satisfies domain.Actuator: a registry could execute an unapproved step through it")
	}
	reg := domain.NewRegistry()
	// The bound form is what may be registered, and only after an approval.
	if _, err := a.Bind(Approval{}); err == nil {
		t.Fatal("Bind accepted a zero Approval")
	}
	step := actStep(actStepOpts{})
	b, err := a.Bind(actApprove(t, actBase, step))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := reg.RegisterActuator(b); err != nil {
		t.Fatalf("RegisterActuator: %v", err)
	}
	if !reg.CanActuate(Kind) {
		t.Fatal("the bound actuator did not register")
	}
	if b.Fingerprint() != PlanFingerprint([]domain.Step{step}) {
		t.Errorf("bound fingerprint = %q", b.Fingerprint())
	}
	if b.Domain() != Kind {
		t.Errorf("bound domain = %q", b.Domain())
	}
}

// A bound actuator authorizes only the plan it was bound to.
func TestBoundActuatorRefusesForeignSteps(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock, actInstance("i-app"), actInstance("i-other"))
	a := newActActuator(t, f, clock, ModeApply, nil)

	approved := actStep(actStepOpts{id: "i-app"})
	other := actStep(actStepOpts{id: "i-other"})
	b, err := a.Bind(actApprove(t, actBase, approved))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := b.Execute(t.Context(), other); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("Execute(foreign): err = %v, want ErrStepNotInPlan", err)
	}
	if err := b.Revert(t.Context(), other); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("Revert(foreign): err = %v, want ErrStepNotInPlan", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("a foreign step issued %d mutating call(s): %v", n, f.Ops())
	}
	// And the refusal is recorded, so the attempt is auditable.
	if e, ok := a.Entry(other.Key); !ok || e.Status != StatusRefused {
		t.Errorf("a refused foreign step left ledger entry %+v", e)
	}
	if err := b.Execute(t.Context(), approved); err != nil {
		t.Fatalf("the approved step must still run: %v", err)
	}
	runningAs(t, f, "i-app", "m6i.2xlarge")
	runningAs(t, f, "i-other", "m5.2xlarge")
}

func TestNewApprovalRefusesEverythingItCannotVerify(t *testing.T) {
	step := actStep(actStepOpts{})
	good := actToken([]domain.Step{step}, actBase)

	for _, tc := range []struct {
		name  string
		steps []domain.Step
		tok   ApprovalToken
		now   time.Time
		want  error
	}{
		{"no fingerprint", []domain.Step{step}, ApprovalToken{ApprovedBy: "x", ExpiresAt: actBase.Add(time.Hour)}, actBase, ErrNotApproved},
		{"no approver", []domain.Step{step},
			ApprovalToken{Fingerprint: good.Fingerprint, ExpiresAt: actBase.Add(time.Hour)}, actBase, ErrNotApproved},
		{"no expiry", []domain.Step{step},
			ApprovalToken{Fingerprint: good.Fingerprint, ApprovedBy: "x"}, actBase, ErrNotApproved},
		{"empty plan", nil, good, actBase, ErrNotApproved},
		{"already expired", []domain.Step{step}, good, actBase.Add(25 * time.Hour), ErrApprovalExpired},
		{"fingerprint is for another plan", []domain.Step{step},
			ApprovalToken{Fingerprint: "0000000000000000", Scope: actScope, ApprovedBy: "x", ExpiresAt: actBase.Add(time.Hour)},
			actBase, ErrFingerprintMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewApproval(tc.steps, tc.tok, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("NewApproval: err = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("a step edited after the plan was hashed", func(t *testing.T) {
		edited := step
		edited.To = edited.To.WithAttr(AttrNetSavingsMonthlyUSD, "99999")
		// The key still claims the old contents.
		if _, err := NewApproval([]domain.Step{edited}, good, actBase); !errors.Is(err, ErrStepKeyMismatch) {
			t.Fatalf("NewApproval: err = %v, want ErrStepKeyMismatch", err)
		}
	})
	t.Run("a step from another domain", func(t *testing.T) {
		foreign := step
		foreign.Target.Domain = domain.Lambda
		foreign.Key = domain.StepKey(foreign.Target, foreign.From, foreign.To)
		tok := actToken([]domain.Step{foreign}, actBase)
		if _, err := NewApproval([]domain.Step{foreign}, tok, actBase); !errors.Is(err, ErrNotApproved) {
			t.Fatalf("NewApproval: err = %v, want ErrNotApproved", err)
		}
	})
	t.Run("a step outside the token's account", func(t *testing.T) {
		elsewhere := actStep(actStepOpts{scope: "9999/eu-west-1"})
		tok := actToken([]domain.Step{elsewhere}, actBase)
		tok.Scope = actScope
		if _, err := NewApproval([]domain.Step{elsewhere}, tok, actBase); !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("NewApproval: err = %v, want ErrScopeMismatch", err)
		}
	})
	t.Run("duplicate keys", func(t *testing.T) {
		dup := []domain.Step{step, step}
		if _, err := NewApproval(dup, actToken(dup, actBase), actBase); !errors.Is(err, ErrNotApproved) {
			t.Fatalf("NewApproval: err = %v, want ErrNotApproved", err)
		}
	})
}

// Approving a plan does not approve a step somebody appended to it.
func TestAuthorizeRefusesStepsOutsideThePlan(t *testing.T) {
	a := actStep(actStepOpts{id: "i-a", seq: 1})
	b := actStep(actStepOpts{id: "i-b", seq: 2})
	ap := actApprove(t, actBase, a)

	if _, err := ap.Authorize(b); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("Authorize(unapproved): err = %v, want ErrStepNotInPlan", err)
	}
	if _, err := ap.Authorize(a); err != nil {
		t.Fatalf("Authorize(approved): %v", err)
	}
	tampered := a
	tampered.To = tampered.To.WithAttr(AttrInstanceType, "m5.metal")
	if _, err := ap.Authorize(tampered); !errors.Is(err, ErrStepKeyMismatch) {
		t.Fatalf("Authorize(tampered): err = %v, want ErrStepKeyMismatch", err)
	}
	// Re-keying the tampered step makes it self-consistent — and still not in
	// the plan, which is the whole point of hashing the plan separately.
	tampered.Key = domain.StepKey(tampered.Target, tampered.From, tampered.To)
	if _, err := ap.Authorize(tampered); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("Authorize(re-keyed): err = %v, want ErrStepNotInPlan", err)
	}
	if _, err := (Approval{}).Authorize(a); !errors.Is(err, ErrNotApproved) {
		t.Fatal("the zero Approval authorized a step")
	}

	plan := []domain.Step{a, b}
	all, err := actApprove(t, actBase, plan...).AuthorizeAll(plan)
	if err != nil || len(all) != 2 {
		t.Fatalf("AuthorizeAll: %v (%d)", err, len(all))
	}
	if _, err := actApprove(t, actBase, a).AuthorizeAll(plan); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("AuthorizeAll over a wider plan: err = %v", err)
	}
}

// An approval that expires while a plan is running stops authorizing the rest
// of it. A stop, a modify and a start take minutes; a 24-hour token that was
// minted 24 hours ago is not consent.
func TestExpiredApprovalStopsExecutionMidPlan(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock, actInstance("i-a"), actInstance("i-b"))
	a := newActActuator(t, f, clock, ModeApply, nil)

	first := actStep(actStepOpts{id: "i-a", seq: 1})
	second := actStep(actStepOpts{id: "i-b", seq: 2})
	ap := actApprove(t, actBase, first, second)
	steps, err := ap.AuthorizeAll([]domain.Step{first, second})
	if err != nil {
		t.Fatalf("AuthorizeAll: %v", err)
	}
	if err := a.Execute(t.Context(), steps[0]); err != nil {
		t.Fatalf("first step: %v", err)
	}
	clock.Advance(25 * time.Hour)
	err = a.Execute(t.Context(), steps[1])
	if !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("second step: err = %v, want ErrApprovalExpired", err)
	}
	runningAs(t, f, "i-a", "m6i.2xlarge")
	runningAs(t, f, "i-b", "m5.2xlarge")
	if e, _ := a.Entry(second.Key); e.Status != StatusRefused {
		t.Errorf("the expired step recorded status %q", e.Status)
	}
}

// The approval is recorded on the ledger and its steps are listed in plan
// order, never map order.
func TestApprovalIsAuditable(t *testing.T) {
	steps := []domain.Step{
		actStep(actStepOpts{id: "i-c", seq: 3}),
		actStep(actStepOpts{id: "i-a", seq: 1}),
		actStep(actStepOpts{id: "i-b", seq: 2}),
	}
	ap := actApprove(t, actBase, steps...)
	want := []string{steps[1].Key, steps[2].Key, steps[0].Key} // seq 1, 2, 3
	for i := 0; i < 20; i++ {
		got := ap.Steps()
		if len(got) != len(want) {
			t.Fatalf("Steps() = %v", got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("Steps() = %v, want sequence order %v", got, want)
			}
		}
	}
	if ap.Token().ApprovedBy == "" || ap.Fingerprint() == "" || !ap.Valid() {
		t.Errorf("approval does not carry its own provenance: %+v", ap.Token())
	}
	if (Approval{}).Valid() || (Approval{}).Fingerprint() != "" || (Approval{}).Steps() != nil {
		t.Error("the zero Approval reports itself usable")
	}
}
