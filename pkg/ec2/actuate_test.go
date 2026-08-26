package ec2

// The happy path, the ledger, and the determinism guarantees.

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

func TestActuatorWiring(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)

	if _, err := NewActuator(nil, nil, ActuatorConfig{Now: clock.Now}); err == nil {
		t.Error("a nil instance seam was accepted")
	}
	if _, err := NewActuator(f, nil, ActuatorConfig{}); err == nil {
		t.Error("a missing clock was accepted: this package must have no clock of its own")
	}
	if _, err := NewActuator(f, nil, ActuatorConfig{Now: clock.Now, Mode: "apply-ish"}); err == nil {
		t.Error("an unknown mode was defaulted instead of rejected")
	}
	if _, err := NewActuator(f, nil, ActuatorConfig{Now: clock.Now, MinHealthyPercentage: 140}); err == nil {
		t.Error("a min-healthy percentage above 100 was accepted")
	}
	a, err := NewActuator(f, nil, ActuatorConfig{Now: clock.Now})
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	if a.Mode() != ModeDryRun {
		t.Errorf("default mode = %q, want %q", a.Mode(), ModeDryRun)
	}
	if a.Domain() != Kind {
		t.Errorf("actuator domain = %q", a.Domain())
	}
	if a.CanActuateASG() {
		t.Error("an actuator wired without an Auto Scaling seam claims it can act on one")
	}
	step := actASGStep(actStepOpts{})
	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); !errors.Is(err, ErrNoAutoScalingSeam) {
		t.Errorf("ASG step without a seam: err = %v, want ErrNoAutoScalingSeam", err)
	}
}

// The whole point of the unit: stop, modify, start, in that order, exactly
// once each, with the instance running as the new type at the end.
func TestStopStartResizeEndToEnd(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	f.SettleAfter = 2 // two polls in each transient state
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})

	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	runningAs(t, f, "i-app", "m6i.2xlarge")

	for op, want := range map[string]int{
		OpStopInstances: 1, OpModifyInstanceAttribute: 1, OpStartInstances: 1,
	} {
		if got := f.Count(op); got != want {
			t.Errorf("%s called %d times, want %d (ops: %v)", op, got, want, f.Ops())
		}
	}
	// Order matters: a modify before the stop is rejected by AWS, and a start
	// before the modify silently resizes nothing.
	var seen []string
	for _, op := range f.Ops() {
		switch op {
		case OpStopInstances, OpModifyInstanceAttribute, OpStartInstances:
			seen = append(seen, op)
		}
	}
	want := []string{OpStopInstances, OpModifyInstanceAttribute, OpStartInstances}
	if len(seen) != len(want) {
		t.Fatalf("mutation sequence = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("mutation sequence = %v, want %v", seen, want)
		}
	}

	e, ok := a.Entry(step.Key)
	if !ok {
		t.Fatal("no ledger entry for the executed step")
	}
	if e.Status != StatusDone || e.Stage != StageRunning {
		t.Errorf("ledger entry = %s/%s, want %s/%s", e.Status, e.Stage, StatusDone, StageRunning)
	}
	if e.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (one per mutation)", e.Attempts)
	}
	if e.From.Attr(AttrInstanceType) != "m5.2xlarge" {
		t.Errorf("the ledger did not record the original state: %v", e.From.Attrs)
	}
	if e.Downtime <= 0 || e.StoppedAt.IsZero() || e.RunningAt.IsZero() {
		t.Errorf("downtime was not measured: stopped %v, running %v, downtime %v",
			e.StoppedAt, e.RunningAt, e.Downtime)
	}
	if e.Fingerprint == "" || e.ApprovedBy == "" {
		t.Errorf("the ledger does not record who approved this stop: %+v", e)
	}
	if !e.Terminal() || !e.Settled() {
		t.Error("a completed resize must read as terminal and settled")
	}
}

// Dry-run runs every gate and issues nothing, and records exactly the calls
// apply would make.
func TestDryRunTouchesNothing(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeDryRun, nil)
	step := actStep(actStepOpts{})

	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("dry-run Execute: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("a dry-run issued %d mutating call(s): %v", n, f.Ops())
	}
	runningAs(t, f, "i-app", "m5.2xlarge")
	e, _ := a.Entry(step.Key)
	if e.Status != StatusDryRun {
		t.Fatalf("status = %q, want %q", e.Status, StatusDryRun)
	}
	for _, want := range []string{"StopInstances", "ModifyInstanceAttribute", "StartInstances", "m6i.2xlarge"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("dry-run detail %q does not mention %q", e.Detail, want)
		}
	}
	if e.Terminal() {
		t.Error("a dry-run must not be terminal: previewing then applying is the normal sequence")
	}
}

// Idempotency, the two ways it can be true: the ledger already knows, and the
// account already reads as the target.
func TestReExecutingACompletedStepIsANoop(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)

	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	mutations, reads := f.Mutations(), f.Count(OpDescribeInstance)

	for i := 0; i < 3; i++ {
		if err := a.Execute(t.Context(), as); err != nil {
			t.Fatalf("re-execute %d: %v", i, err)
		}
	}
	if got := f.Mutations(); got != mutations {
		t.Errorf("re-executing a completed step issued %d extra mutation(s)", got-mutations)
	}
	if got := f.Count(OpDescribeInstance); got != reads {
		t.Errorf("re-executing a completed step read the account %d extra time(s): a finished step must cost nothing",
			got-reads)
	}
}

func TestAlreadyAtTargetIsANoopWithoutALedger(t *testing.T) {
	clock := newActClock(actBase)
	inst := actInstance("i-app")
	inst.InstanceType = "m6i.2xlarge" // a previous process finished the job
	f := newActFixture(clock, inst)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})

	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("an already-resized instance was touched %d time(s): %v", n, f.Ops())
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusNoop || !e.Terminal() {
		t.Errorf("status = %q, want a terminal %q", e.Status, StatusNoop)
	}
}

// The ledger must survive a process boundary: that is what makes a restart
// able to tell "finished" from "never started".
func TestLedgerRoundTripsThroughJSON(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	b, err := a.LedgerJSON()
	if err != nil {
		t.Fatalf("LedgerJSON: %v", err)
	}

	restored := newActActuator(t, newActFixture(newActClock(actBase)), clock, ModeApply, nil)
	if err := restored.RestoreLedger(b); err != nil {
		t.Fatalf("RestoreLedger: %v", err)
	}
	e, ok := restored.Entry(step.Key)
	if !ok || e.Status != StatusDone || !e.Terminal() {
		t.Fatalf("restored entry = %+v", e)
	}
	if err := restored.RestoreLedger(nil); err != nil {
		t.Errorf("restoring an empty ledger must be a no-op, got %v", err)
	}
	if err := restored.RestoreLedger([]byte("{")); err == nil {
		t.Error("a corrupt ledger was accepted")
	}
	b2, err := restored.LedgerJSON()
	if err != nil {
		t.Fatalf("re-serialize: %v", err)
	}
	if string(b) != string(b2) {
		t.Errorf("ledger bytes changed across a round trip:\n%s\n%s", b, b2)
	}
}

// PR#27 shipped a real bug because totals were summed in arrival order. Both
// aggregates in this unit are proved order-independent by shuffling.
func TestSummaryIsShuffleInvariant(t *testing.T) {
	entries := make([]LedgerEntry, 0, 24)
	for i := 0; i < 24; i++ {
		e := LedgerEntry{
			Key:      string(rune('a'+i%13)) + string(rune('a'+i%7)) + string(rune('0'+i%10)),
			Status:   []string{StatusDone, StatusRefused, StatusRolledBack, StatusInFlight}[i%4],
			Downtime: time.Duration(i*i*7919) * time.Millisecond,
		}
		if e.Status == StatusRefused {
			e.RefusalCode = []string{RefuseInstanceStore, RefuseMemoryBlind, RefuseCommitmentNegative}[i%3]
		}
		if e.Downtime > 0 {
			e.StoppedAt = actBase
			if i%5 != 0 {
				e.RunningAt = actBase.Add(e.Downtime)
			}
		}
		entries = append(entries, e)
	}
	want, err := json.Marshal(Summarize(entries))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if Summarize(entries).Entries != len(entries) {
		t.Fatal("the summary lost entries")
	}
	for seed := int64(0); seed < 50; seed++ {
		shuffled := make([]LedgerEntry, len(entries))
		copy(shuffled, entries)
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := json.Marshal(Summarize(shuffled))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("seed %d changed the summary:\n want %s\n  got %s", seed, want, got)
		}
	}
	if Summarize(nil).Entries != 0 {
		t.Error("an empty ledger must summarize to an empty summary")
	}
}

func TestPlanFingerprintIsShuffleInvariant(t *testing.T) {
	steps := []domain.Step{
		actStep(actStepOpts{id: "i-a", seq: 1}),
		actStep(actStepOpts{id: "i-b", seq: 2, toType: "m6i.xlarge", toCPU: 4000, toMem: actMem(16)}),
		actASGStep(actStepOpts{id: "asg-x", seq: 3}),
		actStep(actStepOpts{id: "i-c", seq: 4}),
	}
	want := PlanFingerprint(steps)
	if want == "" {
		t.Fatal("a non-empty plan hashed to nothing")
	}
	for seed := int64(0); seed < 40; seed++ {
		shuffled := make([]domain.Step, len(steps))
		copy(shuffled, steps)
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := PlanFingerprint(shuffled); got != want {
			t.Fatalf("seed %d: fingerprint %q != %q — a human's approval would stop covering their own plan",
				seed, got, want)
		}
	}
	if PlanFingerprint(nil) != "" {
		t.Error("an empty plan must have no fingerprint, so it can never be approved by accident")
	}
	// Editing any step must change the fingerprint.
	edited := make([]domain.Step, len(steps))
	copy(edited, steps)
	edited[1] = actStep(actStepOpts{id: "i-b", seq: 2, toType: "m6i.2xlarge"})
	if PlanFingerprint(edited) == want {
		t.Fatal("editing a step left the fingerprint unchanged")
	}
}

// A step whose Persist hook fails must not reach the cloud at all: an
// unrecorded stop is the state this unit exists to prevent.
func TestPersistFailureAbortsBeforeTheStop(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	boom := errors.New("disk full")
	a := newActActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
		c.Persist = func(context.Context, []byte) error { return boom }
	})
	step := actStep(actStepOpts{})
	err := a.Execute(t.Context(), actAuthorized(t, actBase, step))
	if !errors.Is(err, boom) {
		t.Fatalf("Execute: err = %v, want the persist failure", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("the instance was touched %d time(s) despite an unwritable ledger: %v", n, f.Ops())
	}
	runningAs(t, f, "i-app", "m5.2xlarge")
	e, _ := a.Entry(step.Key)
	if e.Terminal() {
		t.Error("a step that failed to record must stay resumable")
	}
}

// The persist hook is called before every mutation, in order, and what it
// receives is a ledger a restart can act on.
func TestPersistRecordsIntentBeforeEachMutation(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	var snapshots [][]byte
	a := newActActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
		c.Persist = func(_ context.Context, b []byte) error {
			snapshots = append(snapshots, append([]byte(nil), b...))
			return nil
		}
	})
	step := actStep(actStepOpts{})
	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(snapshots) < 3 {
		t.Fatalf("persisted %d times, want at least one per mutation", len(snapshots))
	}
	var first []LedgerEntry
	if err := json.Unmarshal(snapshots[0], &first); err != nil {
		t.Fatalf("the first snapshot is not a ledger: %v", err)
	}
	if len(first) != 1 || first[0].Stage != StageStopping || first[0].Key != step.Key {
		t.Fatalf("the first persisted snapshot = %+v, want the stop recorded before it was issued", first)
	}
}

// The production sleep is the one path tests normally replace, so it gets its
// own coverage: it must honour cancellation and never spin on a zero delay.
func TestActuateSleepHonoursItsContext(t *testing.T) {
	if err := actuateSleep(t.Context(), 0); err != nil {
		t.Errorf("a zero sleep on a live context returned %v", err)
	}
	if err := actuateSleep(t.Context(), time.Microsecond); err != nil {
		t.Errorf("a short sleep returned %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := actuateSleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled sleep returned %v, want context.Canceled", err)
	}
	if err := actuateSleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled zero sleep returned %v", err)
	}
}

// The wiring surface cmd/ uses: an approved step names its own approver, and a
// bound actuator exposes the ledger for the trust report.
func TestBoundActuatorExposesProvenance(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})

	as := actAuthorized(t, actBase, step)
	if as.Token().ApprovedBy != "operator@example.com" || as.Token().Fingerprint == "" {
		t.Fatalf("ApprovedStep.Token() = %+v", as.Token())
	}
	if (ApprovedStep{}).Token().ApprovedBy != "" {
		t.Error("the zero ApprovedStep claims an approver")
	}
	if as.Step().Key != step.Key {
		t.Error("ApprovedStep.Step() did not round-trip the step")
	}

	b, err := a.Bind(actApprove(t, actBase, step))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := b.Execute(t.Context(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	led := b.Ledger()
	if len(led) != 1 || led[0].Key != step.Key || led[0].ApprovedBy == "" {
		t.Fatalf("BoundActuator.Ledger() = %+v", led)
	}
	sum := Summarize(led)
	if sum.Entries != 1 || len(sum.ByStatus) != 1 || sum.ByStatus[0].Code != StatusDone {
		t.Fatalf("Summarize = %+v", sum)
	}
}
