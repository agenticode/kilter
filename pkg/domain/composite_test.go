package domain

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// partDomain is a scriptable half of a composite: it owns targets whose ID
// carries its prefix, and nothing else.
type partDomain struct {
	kind     Kind
	prefix   string
	health   Health
	recs     []Recommendation
	refs     []Refusal
	learned  int
	learnErr error
	planned  [][]Recommendation
	planErr  error
	state    []byte
}

func (p *partDomain) Kind() Kind { return p.kind }
func (p *partDomain) Learn(s *Snapshot) error {
	p.learned++
	return p.learnErr
}
func (p *partDomain) Recommend(time.Time, Netter) []Recommendation {
	out := make([]Recommendation, len(p.recs))
	copy(out, p.recs)
	return out
}
func (p *partDomain) Refusals(time.Time, Netter) []Refusal {
	out := make([]Refusal, len(p.refs))
	copy(out, p.refs)
	return out
}
func (p *partDomain) PlanSteps(recs []Recommendation, g Guard) ([]Step, error) {
	p.planned = append(p.planned, recs)
	if p.planErr != nil {
		return nil, p.planErr
	}
	out := make([]Step, 0, len(recs))
	for i, r := range recs {
		out = append(out, Step{
			Seq: i + 1, Key: StepKey(r.Target, r.Current, r.Proposed),
			Target: r.Target, Action: r.Action, From: r.Current, To: r.Proposed,
		})
	}
	return out, nil
}
func (p *partDomain) Health(time.Time) Health     { return p.health }
func (p *partDomain) Checkpoint() ([]byte, error) { return p.state, nil }
func (p *partDomain) Restore(b []byte) error      { p.state = b; return nil }

func ownsPrefix(prefix string) func(Recommendation) bool {
	return func(r Recommendation) bool { return strings.HasPrefix(r.Target.ID, prefix) }
}

func recFor(k Kind, id string, gross, net float64) Recommendation {
	r := Recommendation{
		Target:   TargetRef{Domain: k, Scope: "acct/us-east-1", ID: id},
		Current:  Spec{Attrs: map[string]string{"size": "big"}},
		Proposed: Spec{Attrs: map[string]string{"size": "small"}},
		Action:   ActionInPlace,
		Evidence: []Evidence{{Metric: "cpu-p95", Value: "9%", Source: "cloudwatch"}},
		Reason:   "smaller is cheaper",
	}
	r.SetSavings(gross, net)
	return r
}

func newTestComposite(t *testing.T, a, b *partDomain) *Composite {
	t.Helper()
	c, err := NewComposite(EC2,
		Part{Name: "instances", Domain: a, Owns: ownsPrefix(a.prefix)},
		Part{Name: "volumes", Domain: b, Owns: ownsPrefix(b.prefix)},
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCompositeIsTheKindCollisionResolution: two domains claiming one Kind
// cannot both be registered — that is the registry rule and it is correct —
// but a composite that IS the Kind can be, and it routes.
func TestCompositeIsTheKindCollisionResolution(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	inst := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true}}
	vols := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true}}

	// The collision itself: registering both halves is refused.
	r := NewRegistry()
	if err := r.Register(inst); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(vols); err == nil {
		t.Fatal("the registry accepted two domains for one kind")
	}

	// The resolution: one composite.
	inst.recs = []Recommendation{recFor(EC2, "i-1", 100, 100)}
	vols.recs = []Recommendation{recFor(EC2, "vol-1", 10, 10)}
	c := newTestComposite(t, inst, vols)
	r2 := NewRegistry()
	if err := r2.Register(c); err != nil {
		t.Fatal(err)
	}
	if got := r2.Kinds(); len(got) != 1 || got[0] != EC2 {
		t.Fatalf("Kinds = %v, want [ec2]", got)
	}
	recs := r2.Recommend(now, nil)
	if len(recs) != 2 {
		t.Fatalf("Recommend returned %d recommendations, want 2", len(recs))
	}
	for _, rec := range recs {
		if rec.Target.Domain != EC2 {
			t.Errorf("%s is attributed to %q", rec.Target.ID, rec.Target.Domain)
		}
	}
}

// TestEveryRegisteredDomainsKindIsInTheClosedSet is the check the unit asks
// for: extend the Kind set consistently, and prove membership rather than
// assume it. It also proves Register is the enforcement point — a domain with
// a kind outside the set cannot get in at all.
func TestEveryRegisteredDomainsKindIsInTheClosedSet(t *testing.T) {
	known := map[Kind]bool{}
	for _, k := range Kinds() {
		known[k] = true
	}
	if len(known) != len(kinds) {
		t.Fatalf("Kinds() has %d entries, the closed set has %d", len(known), len(kinds))
	}

	r := NewRegistry()
	for _, k := range Kinds() {
		if err := r.Register(ready(k)); err != nil {
			t.Fatalf("Register(%s): %v", k, err)
		}
	}
	for _, k := range r.Kinds() {
		if !known[k] {
			t.Errorf("registered domain reports kind %q, which is not in the closed set", k)
		}
		d, ok := r.Get(k)
		if !ok {
			t.Fatalf("Get(%s) missing", k)
		}
		if !d.Kind().Valid() {
			t.Errorf("domain %s reports an invalid kind", k)
		}
	}
	// And the negative: an unknown kind is refused, so the set stays closed.
	if err := r.Register(&partDomain{kind: "quantum-annealer"}); err == nil {
		t.Error("a domain with an unknown kind was registered")
	}
	if _, err := NewComposite("quantum-annealer", Part{Name: "x", Domain: &partDomain{kind: "quantum-annealer"},
		Owns: func(Recommendation) bool { return true }}); err == nil {
		t.Error("a composite for an unknown kind was built")
	}
}

// TestCompositeReportOnlyHalfNeverGetsAStep is the sharp edge of the design.
//
// The composite is actuatable when ANY half is, so the volume half can act.
// The instance half cannot — plain-EC2 actuation does not exist. If the
// composite's permissive merge leaked, the instance half's recommendations
// would become steps for a domain that has no way to execute them.
func TestCompositeReportOnlyHalfNeverGetsAStep(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	inst := &partDomain{kind: EC2, prefix: "i-",
		health: Health{Ready: true, ReportOnly: true, Reason: "no actuator (U7)"}}
	vols := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true}}
	c := newTestComposite(t, inst, vols)

	h := c.Health(now)
	if h.ReportOnly {
		t.Fatal("composite is report-only although one half can act")
	}
	if !strings.Contains(h.Reason, "instances") || !strings.Contains(h.Reason, "no actuator") {
		t.Errorf("health reason does not name the degraded half: %q", h.Reason)
	}

	steps, err := c.PlanSteps([]Recommendation{
		recFor(EC2, "i-1", 100, 100),
		recFor(EC2, "vol-1", 10, 10),
	}, Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("PlanSteps produced %d steps, want 1 (the volume only)", len(steps))
	}
	if steps[0].Target.ID != "vol-1" {
		t.Fatalf("planned %q; the report-only half's target must never become a step", steps[0].Target.ID)
	}
	if len(inst.planned) != 0 {
		t.Fatal("the report-only half's PlanSteps was called")
	}
}

// TestCompositePlanStepsRoutesAndResequences: each half sees only what it
// owns, suppressed recommendations reach nobody, and the merged plan is
// densely sequenced and capped ONCE.
func TestCompositePlanStepsRoutesAndResequences(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	inst := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true}}
	vols := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true}}
	c := newTestComposite(t, inst, vols)

	suppressed := recFor(EC2, "i-9", 100, 0)
	suppressed.Suppressed, suppressed.SuppressCode = true, SuppressCommitmentNegative

	recs := []Recommendation{
		recFor(EC2, "vol-2", 1, 1),
		recFor(EC2, "i-1", 2, 2),
		recFor(EC2, "vol-1", 3, 3),
		recFor(EC2, "i-2", 4, 4),
		suppressed,
	}
	steps, err := c.PlanSteps(recs, Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 {
		t.Fatalf("PlanSteps produced %d steps, want 4", len(steps))
	}
	wantOrder := []string{"i-1", "i-2", "vol-1", "vol-2"}
	for i, s := range steps {
		if s.Target.ID != wantOrder[i] {
			t.Errorf("step %d is %q, want %q", i, s.Target.ID, wantOrder[i])
		}
		if s.Seq != i+1 {
			t.Errorf("step %d has Seq %d", i, s.Seq)
		}
	}
	for _, given := range inst.planned {
		for _, r := range given {
			if !strings.HasPrefix(r.Target.ID, "i-") {
				t.Errorf("the instance half was given %q", r.Target.ID)
			}
			if r.Suppressed {
				t.Errorf("a suppressed recommendation reached a half: %q", r.Target.ID)
			}
		}
	}

	// MaxSteps is applied to the MERGE. A cap applied per half would let a
	// two-part domain execute twice the plan the operator authorized.
	capped, err := c.PlanSteps(recs, Guard{Now: now, MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 {
		t.Fatalf("MaxSteps=2 produced %d steps", len(capped))
	}
}

// TestCompositeGuardrailsRefuseBeforeAnyHalfIsAsked.
func TestCompositeGuardrailsRefuseBeforeAnyHalfIsAsked(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	inst := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true}}
	vols := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true}}
	c := newTestComposite(t, inst, vols)
	rec := []Recommendation{recFor(EC2, "i-1", 1, 1)}

	if _, err := c.PlanSteps(rec, Guard{Now: now, Freeze: true}); !errors.Is(err, ErrFrozen) {
		t.Errorf("frozen PlanSteps = %v", err)
	}
	if _, err := c.PlanSteps(rec, Guard{Now: now, BreakerOpen: true}); !errors.Is(err, ErrBreakerOpen) {
		t.Errorf("breaker PlanSteps = %v", err)
	}
	if len(inst.planned) != 0 {
		t.Fatal("a half was asked to plan under a refusing guardrail")
	}
}

// TestCompositeLearnFansOutAndJoinsErrors: one bad half does not stop the
// other from learning, and the failure is reported rather than swallowed.
func TestCompositeLearnFansOutAndJoinsErrors(t *testing.T) {
	inst := &partDomain{kind: EC2, prefix: "i-", learnErr: errors.New("boom")}
	vols := &partDomain{kind: EC2, prefix: "vol-"}
	c := newTestComposite(t, inst, vols)

	err := c.Learn(&Snapshot{Domain: EC2, Scope: "acct/us-east-1"})
	if err == nil || !strings.Contains(err.Error(), "instances: boom") {
		t.Fatalf("Learn error = %v, want it to name the failing half", err)
	}
	if vols.learned != 1 {
		t.Fatal("the healthy half did not learn")
	}
	if err := c.Learn(nil); err != nil {
		t.Errorf("Learn(nil) = %v", err)
	}
	if err := c.Learn(&Snapshot{Domain: Lambda}); !errors.Is(err, ErrWrongDomain) {
		t.Errorf("Learn(foreign) = %v, want ErrWrongDomain", err)
	}
}

// TestCompositeCheckpointIsDeterministicAndRoundTrips.
func TestCompositeCheckpointIsDeterministicAndRoundTrips(t *testing.T) {
	inst := &partDomain{kind: EC2, prefix: "i-", state: []byte(`{"a":1}`)}
	vols := &partDomain{kind: EC2, prefix: "vol-", state: []byte(`{"b":2}`)}
	c := newTestComposite(t, inst, vols)

	first, err := c.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := c.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatal("Checkpoint is not byte-stable")
		}
	}
	// Registration order must not change the bytes.
	swapped, err := NewComposite(EC2,
		Part{Name: "volumes", Domain: vols, Owns: ownsPrefix("vol-")},
		Part{Name: "instances", Domain: inst, Owns: ownsPrefix("i-")},
	)
	if err != nil {
		t.Fatal(err)
	}
	other, err := swapped.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(other) != string(first) {
		t.Fatal("Checkpoint depends on part registration order")
	}

	inst.state, vols.state = nil, nil
	if err := c.Restore(first); err != nil {
		t.Fatal(err)
	}
	if string(inst.state) != `{"a":1}` || string(vols.state) != `{"b":2}` {
		t.Fatalf("Restore did not route: inst=%q vols=%q", inst.state, vols.state)
	}
	if err := c.Restore(nil); err != nil {
		t.Errorf("Restore(nil) = %v", err)
	}
	// A checkpoint from another wiring is reported, not silently half-applied:
	// losing a half's learned state is how a resized volume loses its cooldown.
	if err := c.Restore([]byte(`{"version":1,"kind":"ec2","parts":[{"name":"snapshots"}]}`)); err == nil {
		t.Error("a checkpoint naming an unknown part was accepted")
	}
	if err := c.Restore([]byte(`{"version":1,"kind":"lambda"}`)); !errors.Is(err, ErrWrongDomain) {
		t.Errorf("a checkpoint for another kind = %v, want ErrWrongDomain", err)
	}
}

// TestNewCompositeRejectsWiringBugs.
func TestNewCompositeRejectsWiringBugs(t *testing.T) {
	good := &partDomain{kind: EC2, prefix: "i-"}
	all := func(Recommendation) bool { return true }
	for _, tc := range []struct {
		name  string
		parts []Part
	}{
		{"no parts", nil},
		{"unnamed", []Part{{Domain: good, Owns: all}}},
		{"nil domain", []Part{{Name: "a", Owns: all}}},
		{"no predicate", []Part{{Name: "a", Domain: good}}},
		{"duplicate name", []Part{
			{Name: "a", Domain: good, Owns: all}, {Name: "a", Domain: good, Owns: all}}},
		{"wrong kind", []Part{{Name: "a", Domain: &partDomain{kind: Lambda}, Owns: all}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewComposite(EC2, tc.parts...); err == nil {
				t.Error("wiring bug was accepted")
			}
		})
	}
}

// TestCompositeOutputIsIndependentOfInputOrder.
func TestCompositeOutputIsIndependentOfInputOrder(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	inst := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true}}
	vols := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true}}
	inst.recs = []Recommendation{recFor(EC2, "i-1", 1, 1), recFor(EC2, "i-2", 2, 2)}
	vols.recs = []Recommendation{recFor(EC2, "vol-1", 3, 3), recFor(EC2, "vol-2", 4, 4)}
	inst.refs = []Refusal{{Target: TargetRef{ID: "i-3"}, Code: "memory-blind", Reason: "no agent"}}
	vols.refs = []Refusal{{Target: TargetRef{ID: "vol-3"}, Code: "not-gp2", Reason: "io2"}}
	c := newTestComposite(t, inst, vols)

	want := c.Recommend(now, nil)
	wantRefs := c.Refusals(now, nil)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		rng.Shuffle(len(inst.recs), func(a, b int) { inst.recs[a], inst.recs[b] = inst.recs[b], inst.recs[a] })
		rng.Shuffle(len(vols.recs), func(a, b int) { vols.recs[a], vols.recs[b] = vols.recs[b], vols.recs[a] })
		got := c.Recommend(now, nil)
		if len(got) != len(want) {
			t.Fatalf("Recommend length changed: %d vs %d", len(got), len(want))
		}
		for j := range want {
			if got[j].Target.ID != want[j].Target.ID {
				t.Fatalf("Recommend order changed under shuffle: %v", got)
			}
		}
	}
	if len(wantRefs) != 2 || wantRefs[0].Target.ID != "i-3" {
		t.Fatalf("Refusals = %v", wantRefs)
	}
	for _, r := range wantRefs {
		if r.Target.Domain != EC2 {
			t.Errorf("refusal %q is attributed to %q", r.Target.ID, r.Target.Domain)
		}
	}
}

// TestPartAcceptsKeepsOneHalfsSnapshotAwayFromTheOther pins the fix for a real
// ordering bug (cmd/FINDINGS.md §5.1).
//
// Several collectors feed one Kind. A half handed its sibling's snapshot may
// be unable to tell "this is not for me" from "my collector found nothing",
// and a half that draws the second conclusion degrades itself — so the
// domain's health came to depend on which snapshot happened to arrive last.
// Accepts is how the wiring says which snapshot is whose.
func TestPartAcceptsKeepsOneHalfsSnapshotAwayFromTheOther(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	// A half that degrades when it is fed a snapshot with no targets — which
	// is exactly what pkg/ebs does, correctly, for its own contract.
	touchy := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true}}
	other := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true}}

	c, err := NewComposite(EC2,
		Part{Name: "instances", Domain: other, Owns: ownsPrefix("i-")},
		Part{Name: "volumes", Domain: touchy, Owns: ownsPrefix("vol-"),
			Accepts: func(s *Snapshot) bool { return len(s.Payload) == 0 || len(s.Targets) > 0 }},
	)
	if err != nil {
		t.Fatal(err)
	}

	payloadOnly := &Snapshot{Domain: EC2, Timestamp: now, Payload: []byte(`{"domain":"ec2"}`)}
	withTargets := &Snapshot{Domain: EC2, Timestamp: now,
		Targets: []Target{{Ref: TargetRef{Domain: EC2, ID: "vol-1"}}}}
	empty := &Snapshot{Domain: EC2, Timestamp: now}

	if err := c.Learn(payloadOnly); err != nil {
		t.Fatal(err)
	}
	if touchy.learned != 0 {
		t.Error("a half was handed a snapshot whose only content was its sibling's payload")
	}
	if other.learned != 1 {
		t.Error("the payload's owner did not receive it")
	}

	if err := c.Learn(withTargets); err != nil {
		t.Fatal(err)
	}
	if touchy.learned != 1 {
		t.Error("a half did not receive its own snapshot")
	}

	// An empty account is a real answer: "we looked and found none" must stay
	// distinguishable from "we never looked", so a target-less, payload-less
	// snapshot still reaches the half.
	if err := c.Learn(empty); err != nil {
		t.Fatal(err)
	}
	if touchy.learned != 2 {
		t.Error("an empty-account snapshot was withheld; that erases a real observation")
	}
}
