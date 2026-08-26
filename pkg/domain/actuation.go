package domain

import (
	"context"
	"fmt"
	"sort"
)

// Actuation capability is a property of the WIRING, never of the domain.
//
// [Registry.PlanSteps] already refuses to plan for a domain whose own Health
// says it is report-only, and it refuses in the core rather than trusting the
// domain's PlanSteps to police itself (U2). That closes the honest failure:
// a domain that knows its collector is gone.
//
// It does not close the dishonest one. A [Domain] is ordinary Go code; its
// Health method can return whatever it likes, including
// `Health{ReportOnly: false}` on a domain with no collector, no credentials
// and no actuator. Nothing the domain says about itself can be the last word
// on whether it may touch a cloud account.
//
// So the last word is the actuator table below. An [Actuator] gets into it
// exactly one way: someone in cmd/ wrote the line that put it there, having
// first built the SDK client it needs. A domain cannot register one for
// itself — it never sees the [Registry] — and [Registry.Execute] refuses a
// step whose kind has none. A hostile domain can therefore fabricate
// recommendations, lie about its Health, and even hand back steps; the steps
// go nowhere, because execution routes through a table it cannot write to.

// RegisterActuator wires an actuator for one kind. Registering two actuators
// for the same kind, a nil actuator, or an actuator reporting an unknown kind
// is a wiring bug and is reported, never silently resolved.
//
// Registration order does not matter: an actuator may be registered before or
// after its domain, and an actuator for a kind with no domain is legal but
// inert (nothing can produce steps for it).
func (r *Registry) RegisterActuator(a Actuator) error {
	if r == nil {
		return fmt.Errorf("domain: RegisterActuator on a nil registry")
	}
	if a == nil {
		return fmt.Errorf("domain: RegisterActuator(nil)")
	}
	k := a.Domain()
	if !k.Valid() {
		return fmt.Errorf("domain: RegisterActuator: unknown kind %q", k)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actuators[k]; dup {
		return fmt.Errorf("domain: RegisterActuator: %q already has an actuator", k)
	}
	if r.actuators == nil {
		r.actuators = map[Kind]Actuator{}
	}
	r.actuators[k] = a
	return nil
}

// Actuator returns the actuator wired for a kind.
func (r *Registry) Actuator(k Kind) (Actuator, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actuators[k]
	return a, ok
}

// CanActuate reports whether a kind has an actuator wired. It says nothing
// about whether the domain is currently willing to act — that is
// [Domain.Health] — only whether an execution path physically exists.
func (r *Registry) CanActuate(k Kind) bool {
	_, ok := r.Actuator(k)
	return ok
}

// ActuatableKinds returns the kinds with an actuator wired, in canonical
// order.
func (r *Registry) ActuatableKinds() []Kind {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.actuators) == 0 {
		return nil
	}
	out := make([]Kind, 0, len(r.actuators))
	for k := range r.actuators {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Execute runs one step through its kind's actuator, re-checking every gate at
// execution time rather than trusting the plan's beliefs:
//
//  1. the step's kind must be registered — a step for a domain nobody wired is
//     not a step this process may run;
//  2. the domain must not be report-only as of g.Now — a collector that died
//     between planning and execution revokes permission to act;
//  3. an actuator must be wired for the kind — see the note above: this is the
//     gate a domain cannot talk its way past;
//  4. the guardrails must still allow actuation — freeze, breaker and change
//     window are re-evaluated, so a plan that was legal when built is refused
//     when executed late.
//
// Actuators are idempotent per [Step.Key], so a refused-then-retried step is
// safe to re-execute.
func (r *Registry) Execute(ctx context.Context, g Guard, step Step) error {
	a, err := r.actuatorFor(g, step)
	if err != nil {
		return err
	}
	if err := g.Allow(); err != nil {
		return err
	}
	return a.Execute(ctx, step)
}

// Revert undoes a step through its kind's actuator, from the step's recorded
// From spec.
//
// It applies gates 1-3 of [Registry.Execute] but NOT the change window: a
// revert exists to undo a change Kilter already made, and refusing to undo it
// because the window closed would leave a workload stranded at the size that
// broke it. Freeze and the circuit breaker are likewise not applied here for
// the same reason — both are usually turned on *because* something is wrong,
// which is when a revert is most needed. Actuators that disagree may enforce
// their own policy; this one is the core's.
func (r *Registry) Revert(ctx context.Context, g Guard, step Step) error {
	a, err := r.actuatorFor(g, step)
	if err != nil {
		return err
	}
	return a.Revert(ctx, step)
}

// actuatorFor resolves the actuator for a step and applies the registration
// and report-only gates.
func (r *Registry) actuatorFor(g Guard, step Step) (Actuator, error) {
	k := step.Target.Domain
	d, ok := r.Get(k)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotRegistered, k)
	}
	if h := d.Health(g.Now); h.ReportOnly {
		return nil, fmt.Errorf("%w: %s: %s", ErrReportOnly, k, h.Reason)
	}
	a, ok := r.Actuator(k)
	if !ok {
		return nil, fmt.Errorf("%w: %s: no actuator is wired for this domain", ErrReportOnly, k)
	}
	return a, nil
}
