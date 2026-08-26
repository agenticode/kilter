package ec2

// Resumability, rollback and undo.
//
// A stop-resize is three cloud transitions with two crash windows between
// them. The property this file exists to prove is the one that matters at 3am:
//
//	after ANY interruption, the instance is either running, or the ledger
//	says it is not — and re-running the same step brings it back.
//
// Every stage boundary is interrupted separately. "One test covers the crash
// case" is exactly how the untested boundary becomes the one that fires.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

var errInjected = errors.New("injected: the API call did not complete")

// interruption is one stage boundary and how to break it.
type interruption struct {
	name string
	// arm breaks the fixture and returns the function that repairs it.
	arm func(f *ActuateFixture) func()
	// tune adjusts the actuator used for the interrupted attempt.
	tune func(*ActuatorConfig)
	// wantTerminal is true for boundaries where the machine resolves the
	// instance itself instead of leaving work to resume.
	wantTerminal bool
}

// failFirst breaks the first call of an operation, before its effect.
func failFirst(op string) func(*ActuateFixture) func() {
	return func(f *ActuateFixture) func() {
		armed := true
		f.Fail = func(o string, _ int) error {
			if armed && o == op {
				armed = false
				return errInjected
			}
			return nil
		}
		return func() { f.Fail = nil }
	}
}

// loseFirstResponse lets the first call of an operation take effect and then
// fails it — the lost-response case, and the one that leaves the account and
// the controller disagreeing.
func loseFirstResponse(op string) func(*ActuateFixture) func() {
	return func(f *ActuateFixture) func() {
		armed := true
		f.FailAfter = func(o string, _ int) error {
			if armed && o == op {
				armed = false
				return errInjected
			}
			return nil
		}
		return func() { f.FailAfter = nil }
	}
}

// oneShotPoll makes the poll budget one attempt and stalls a transition, so
// the machine gives up waiting with the instance mid-transition.
func stallFrom(op string) func(*ActuateFixture) func() {
	return func(f *ActuateFixture) func() {
		f.Fail = func(o string, _ int) error {
			if o == op {
				f.SettleAfter = 50 // nothing settles from here on
			}
			return nil
		}
		return func() { f.Fail = nil; f.SettleAfter = 0 }
	}
}

var stageBoundaries = []interruption{
	{name: "before the stop is issued", arm: failFirst(OpStopInstances)},
	{name: "the stop's response is lost", arm: loseFirstResponse(OpStopInstances)},
	{
		name: "while it is stopping",
		arm:  stallFrom(OpStopInstances),
		tune: func(c *ActuatorConfig) { c.PollTimeout = c.PollInterval },
	},
	// A modify that cannot run leaves the instance STOPPED. The machine is
	// required to resolve that itself rather than leave it for a resume, so
	// this boundary is terminal — rolled back, running, and not retried.
	{name: "before the modify is issued", arm: failFirst(OpModifyInstanceAttribute), wantTerminal: true},
	{name: "the modify's response is lost", arm: loseFirstResponse(OpModifyInstanceAttribute), wantTerminal: true},
	{name: "before the start is issued", arm: failFirst(OpStartInstances)},
	{name: "the start's response is lost", arm: loseFirstResponse(OpStartInstances)},
	{
		name: "while it is starting",
		arm:  stallFrom(OpStartInstances),
		tune: func(c *ActuatorConfig) { c.PollTimeout = c.PollInterval },
	},
	{name: "the describe that drives the machine fails", arm: failFirst(OpDescribeInstance)},
}

// TestResumesFromEveryStageBoundary interrupts each boundary, then resumes
// with a FRESH actuator — no ledger at all, the harshest restart there is —
// and requires the instance to end up running as the target.
func TestResumesFromEveryStageBoundary(t *testing.T) {
	for _, tc := range stageBoundaries {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newActFixture(clock)
			repair := tc.arm(f)

			step := actStep(actStepOpts{})
			as := actAuthorized(t, actBase, step)
			a := newActActuator(t, f, clock, ModeApply, tc.tune)
			err := a.Execute(t.Context(), as)

			// The invariant. Whatever happened, an instance that is not
			// running must be recorded as unfinished business.
			assertNeverStoppedAndForgotten(t, f, a, step.Key)

			e, ok := a.Entry(step.Key)
			if !ok {
				t.Fatalf("interrupted step left no ledger entry (err %v)", err)
			}
			if tc.wantTerminal != e.Terminal() {
				t.Fatalf("entry terminal = %v (status %q), want %v", e.Terminal(), e.Status, tc.wantTerminal)
			}

			repair()

			// A restarted controller that lost its ledger entirely.
			for i := 0; i < 4; i++ {
				fresh := newActActuator(t, f, clock, ModeApply, nil)
				if err := fresh.Execute(t.Context(), as); err == nil {
					break
				} else if i == 3 {
					t.Fatalf("resume did not converge: %v", err)
				}
				assertNeverStoppedAndForgotten(t, f, fresh, step.Key)
			}
			runningAs(t, f, "i-app", "m6i.2xlarge")
		})
	}
}

// The same boundaries, resumed through a PERSISTED ledger rather than a fresh
// actuator: the path a real controller takes.
func TestResumesThroughAPersistedLedger(t *testing.T) {
	for _, tc := range stageBoundaries {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newActFixture(clock)
			repair := tc.arm(f)

			var saved []byte
			persist := func(_ context.Context, b []byte) error {
				saved = append([]byte(nil), b...)
				return nil
			}
			step := actStep(actStepOpts{})
			as := actAuthorized(t, actBase, step)
			a := newActActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
				persistTune(c, persist)
				if tc.tune != nil {
					tc.tune(c)
				}
			})
			_ = a.Execute(t.Context(), as)
			if b, err := a.LedgerJSON(); err == nil {
				saved = b
			}
			repair()

			for i := 0; i < 4; i++ {
				next := newActActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) { persistTune(c, persist) })
				if err := next.RestoreLedger(saved); err != nil {
					t.Fatalf("RestoreLedger: %v", err)
				}
				// Unsettled is what a controller reads on startup to find the
				// instances it may have left down.
				if live, _ := f.Instance("i-app"); live.State != StateRunning {
					if len(next.Unsettled()) == 0 {
						t.Fatalf("instance is %s but the restored ledger reports nothing unsettled", live.State)
					}
				}
				err := next.Execute(t.Context(), as)
				if b, jerr := next.LedgerJSON(); jerr == nil {
					saved = b
				}
				if err == nil {
					break
				}
				if i == 3 {
					t.Fatalf("resume did not converge: %v", err)
				}
			}
			live, _ := f.Instance("i-app")
			if live.State != StateRunning {
				t.Fatalf("after resuming, %s is %s", live.InstanceID, live.State)
			}
			if tc.wantTerminal {
				// A resume that inherits a terminal entry must NOT stop the
				// instance again. The rolled-back case therefore ends on the
				// ORIGINAL type, and that is the correct outcome: a human
				// decides whether to try the resize a second time.
				if n := f.Count(OpStopInstances); n != 1 {
					t.Errorf("a terminal entry was retried: %d stop(s) issued", n)
				}
			} else if live.InstanceType != "m6i.2xlarge" {
				t.Fatalf("after resuming, %s is %s, want m6i.2xlarge", live.InstanceID, live.InstanceType)
			}
		})
	}
}

func persistTune(c *ActuatorConfig, p func(context.Context, []byte) error) { c.Persist = p }

// assertNeverStoppedAndForgotten is the safety property, stated once and
// reused: a non-running instance always has a non-terminal ledger entry
// naming it, so something will come back for it.
func assertNeverStoppedAndForgotten(t *testing.T, f *ActuateFixture, a *Actuator, key string) {
	t.Helper()
	live, ok := f.Instance("i-app")
	if !ok {
		t.Fatal("the instance vanished")
	}
	if live.State == StateRunning {
		return
	}
	e, ok := a.Entry(key)
	if !ok {
		t.Fatalf("instance is %s and the ledger has no entry for it: stopped and forgotten", live.State)
	}
	if e.Terminal() {
		t.Fatalf("instance is %s but its ledger entry is terminal (%q): stopped and forgotten",
			live.State, e.Status)
	}
}

// A modify that can never succeed must not leave the instance down. The
// machine starts it again at its ORIGINAL type and says so.
func TestRollbackRestoresServiceWhenTheModifyCannotSucceed(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	f.SettleAfter = 1 // one poll per transition, so the fake clock advances
	f.Fail = func(op string, _ int) error {
		if op == OpModifyInstanceAttribute {
			return errInjected
		}
		return nil
	}
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)

	err := a.Execute(t.Context(), as)
	if err == nil {
		t.Fatal("a permanently failing modify reported success")
	}
	runningAs(t, f, "i-app", "m5.2xlarge")

	e, _ := a.Entry(step.Key)
	if e.Status != StatusRolledBack {
		t.Fatalf("status = %q, want %q", e.Status, StatusRolledBack)
	}
	if !e.Terminal() {
		t.Error("a rolled-back step must be terminal: retrying it would stop the instance again for work that just failed")
	}
	if !e.Settled() {
		t.Error("a rolled-back step describes an instance at rest")
	}
	if e.Downtime <= 0 {
		t.Errorf("the outage was not recorded: %+v", e)
	}
	// And it is not retried automatically.
	before := f.Mutations()
	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("re-executing a rolled-back step: %v", err)
	}
	if f.Mutations() != before {
		t.Error("a rolled-back step was retried automatically")
	}
}

// --- undo -------------------------------------------------------------------

// Revert restores the state the step recorded in From, using the same machine
// and leaving its own ledger entry.
func TestRevertRestoresTheRecordedFromState(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	f.SettleAfter = 1
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)

	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	runningAs(t, f, "i-app", "m6i.2xlarge")

	if err := a.Revert(t.Context(), as); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	runningAs(t, f, "i-app", "m5.2xlarge")

	inverseKey := domain.StepKey(step.Target, step.To, step.From)
	rev, ok := a.Entry(inverseKey)
	if !ok {
		t.Fatal("the undo left no ledger entry of its own")
	}
	if !rev.Revert || rev.Status != StatusDone {
		t.Fatalf("undo entry = %+v, want a completed revert", rev)
	}
	if rev.To.Attr(AttrInstanceType) != "m5.2xlarge" || rev.From.Attr(AttrInstanceType) != "m6i.2xlarge" {
		t.Fatalf("the undo did not invert the step: %v → %v", rev.From.Attrs, rev.To.Attrs)
	}
	// The forward entry is untouched: an audit sees both halves.
	fwd, _ := a.Entry(step.Key)
	if fwd.Status != StatusDone || fwd.Revert {
		t.Fatalf("the forward entry was overwritten by the undo: %+v", fwd)
	}
	// Reverting twice is a no-op, not a second stop.
	before := f.Mutations()
	if err := a.Revert(t.Context(), as); err != nil {
		t.Fatalf("second Revert: %v", err)
	}
	if f.Mutations() != before {
		t.Error("reverting an already-reverted step touched the account again")
	}
}

// An undo is not judged on economics: it restores a shape the workload
// demonstrably ran on, and demanding a saving would make every bad change
// permanent. Safety predicates still apply.
func TestRevertIsNotBlockedByEconomics(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeApply, nil)
	// The forward step cuts memory with a real signal, so its inverse GROWS
	// memory and carries no savings attestation at all on the side it is
	// moving toward.
	step := actStep(actStepOpts{toType: "m6i.xlarge", toCPU: 4000, toMem: actMem(16)})
	as := actAuthorized(t, actBase, step)
	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	runningAs(t, f, "i-app", "m6i.xlarge")
	if err := a.Revert(t.Context(), as); err != nil {
		t.Fatalf("Revert must not be blocked by the missing savings attestation: %v", err)
	}
	runningAs(t, f, "i-app", "m5.2xlarge")
}

// An undo still refuses to destroy data.
func TestRevertStillRefusesUnsafeInstances(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)
	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Somebody attaches ephemeral storage before the undo runs.
	live, _ := f.Instance("i-app")
	live.InstanceStoreVolumes = 1
	f.SetInstance(live)

	before := f.Mutations()
	err := a.Revert(t.Context(), as)
	if RefusalCode(err) != RefuseInstanceStore {
		t.Fatalf("Revert: err = %v, want an %s refusal", err, RefuseInstanceStore)
	}
	if f.Mutations() != before {
		t.Error("a refused undo touched the account")
	}
}

func TestRevertOfAnUnrevertibleActionIsHonest(t *testing.T) {
	clock := newActClock(actBase)
	a := newActActuator(t, newActFixture(clock), clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	step.Action = domain.ActionAdvisory
	step.Key = domain.StepKey(step.Target, step.From, step.To)
	as := actAuthorized(t, actBase, step)
	if err := a.Revert(t.Context(), as); !errors.Is(err, ErrIrreversible) {
		t.Fatalf("Revert(advisory): err = %v, want ErrIrreversible", err)
	}
}

// A machine that cannot make progress reports it rather than looping against a
// billed API forever.
func TestMachineGivesUpRatherThanLooping(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	// Every stop is quietly undone by "somebody else" restarting the instance.
	f.FailAfter = func(op string, _ int) error {
		if op == OpStopInstances {
			live, _ := f.Instance("i-app")
			live.State = StateRunning
			live.InstanceType = "m5.2xlarge"
			f.SetInstance(live)
		}
		return nil
	}
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actStep(actStepOpts{})
	err := a.Execute(t.Context(), actAuthorized(t, actBase, step))
	if !errors.Is(err, ErrStuck) {
		t.Fatalf("Execute: err = %v, want ErrStuck", err)
	}
	if n := f.Count(OpStopInstances); n > maxStageVisits+1 {
		t.Errorf("issued %d stops before giving up", n)
	}
	runningAs(t, f, "i-app", "m5.2xlarge")
}

// A poll budget that runs out is IN FLIGHT — not done, not failed — and the
// entry stays resumable.
func TestPollTimeoutIsInFlightNotFailure(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	f.SettleAfter = 10
	a := newActActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
		c.PollInterval = time.Second
		c.PollTimeout = 2 * time.Second
	})
	step := actStep(actStepOpts{})
	err := a.Execute(t.Context(), actAuthorized(t, actBase, step))
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("Execute: err = %v, want ErrPollTimeout", err)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusInFlight || e.Terminal() {
		t.Fatalf("entry = %s (terminal %v), want a resumable in-flight entry", e.Status, e.Terminal())
	}
	if got := a.Unsettled(); len(got) != 1 || got[0].Key != step.Key {
		t.Fatalf("Unsettled() = %v, want the in-flight step", got)
	}
}
