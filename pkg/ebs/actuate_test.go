package ebs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// stepFor builds the step a plan would carry for converting a gp2 volume.
func stepFor(volumeID string, size int64, iops, tput int32) domain.Step {
	ref := domain.TargetRef{Domain: Kind, Scope: "acct/us-east-1", ID: volumeID}
	from := SpecOf(gp2Volume(volumeID, size))
	to := from.
		WithAttr(AttrVolumeType, VolumeTypeGP3).
		WithAttr(AttrIOPS, itoa(int64(iops))).
		WithAttr(AttrThroughputMBps, itoa(int64(tput)))
	return domain.Step{
		Seq: 1, Key: domain.StepKey(ref, from, to), Target: ref,
		Action: domain.ActionInPlace, From: from, To: to, Risk: "low",
	}
}

// newActuator wires an actuator over a fixture with a fake clock whose sleeps
// advance time instead of spending it.
func newActuator(t *testing.T, f *Fixture, clock *fakeClock, mode Mode, tune func(*ActuatorConfig)) *Actuator {
	t.Helper()
	cfg := ActuatorConfig{
		Mode:         mode,
		Now:          clock.Now,
		Logger:       slog.New(slog.DiscardHandler),
		PollInterval: time.Second,
		PollTimeout:  10 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			clock.Advance(d)
			return ctx.Err()
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	a, err := NewActuator(f, cfg)
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	return a
}

func TestActuatorWiring(t *testing.T) {
	clock := newClock(base)
	f := &Fixture{Now: clock.Now}
	if _, err := NewActuator(nil, ActuatorConfig{Now: clock.Now}); err == nil {
		t.Error("nil seam accepted")
	}
	if _, err := NewActuator(f, ActuatorConfig{}); err == nil {
		t.Error("missing clock accepted: this package must have no clock of its own")
	}
	if _, err := NewActuator(f, ActuatorConfig{Now: clock.Now, Mode: "apply-ish"}); err == nil {
		t.Error("an unknown mode was defaulted instead of rejected")
	}
	a, err := NewActuator(f, ActuatorConfig{Now: clock.Now})
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	if a.Mode() != ModeDryRun {
		t.Errorf("default mode = %q, want %q", a.Mode(), ModeDryRun)
	}
	if a.Domain() != Kind {
		t.Errorf("actuator domain = %q", a.Domain())
	}
}

// TestExecuteAppliesAndPolls is the whole point of the unit: an asynchronous
// modification is issued, polled to a terminal state, and recorded.
func TestExecuteAppliesAndPolls(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	f.PollsToOptimizing = 2
	a := newActuator(t, f, clock, ModeApply, nil)
	step := stepFor("vol-a", 4000, 5200, 130)

	if err := a.Execute(t.Context(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := f.ModifyCallCount(); n != 1 {
		t.Fatalf("issued %d modifications, want exactly 1", n)
	}
	req := f.ModifyRequests[0]
	if req.VolumeID != "vol-a" || req.VolumeType != VolumeTypeGP3 || req.IOPS != 5200 || req.ThroughputMBps != 130 {
		t.Errorf("ModifyVolume request = %+v", req)
	}
	if req.SizeGiB != 0 {
		t.Errorf("the request carries a size (%d): this unit never resizes a volume", req.SizeGiB)
	}
	live, _ := f.VolumeByID("vol-a")
	if live.VolumeType != VolumeTypeGP3 || live.IOPS != 5200 || live.ThroughputMBps != 130 {
		t.Errorf("live volume after apply = %+v", live)
	}
	e, ok := a.Entry(step.Key)
	if !ok {
		t.Fatal("no ledger entry for the executed step")
	}
	if e.Status != StatusDone || e.Attempts != 1 || e.Polls < 2 {
		t.Errorf("ledger entry = %+v, want done after 1 attempt and ≥2 polls", e)
	}
	if e.From.Attr(AttrVolumeType) != VolumeTypeGP2 {
		t.Errorf("the ledger did not record the original state: %+v", e.From.Attrs)
	}
	if e.FinishedAt.IsZero() || e.StartedAt.IsZero() {
		t.Error("the ledger entry has no timestamps")
	}
}

// TestPollingAcrossIncompleteModification: a modification that has not landed
// when the poll budget runs out is IN FLIGHT — not done, not failed — and the
// next execution resumes it instead of issuing a second one.
func TestPollingAcrossIncompleteModification(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	f.PollsToOptimizing = 4
	a := newActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
		c.PollInterval = time.Second
		c.PollTimeout = 2 * time.Second // two polls, then give up waiting
	})
	step := stepFor("vol-a", 4000, 5200, 130)

	err := a.Execute(t.Context(), step)
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("Execute: err = %v, want ErrPollTimeout", err)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusInFlight {
		t.Fatalf("ledger status = %q, want %q", e.Status, StatusInFlight)
	}
	if e.Terminal() {
		t.Fatal("an in-flight step reads as terminal: a later run would skip it")
	}
	live, _ := f.VolumeByID("vol-a")
	if live.VolumeType != VolumeTypeGP2 {
		t.Fatalf("the volume changed type while still modifying: %s", live.VolumeType)
	}

	// Resume: same step, same actuator. It must NOT issue a second
	// modification — AWS would reject it, and a second one is a second change.
	if err := a.Execute(t.Context(), step); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := f.ModifyCallCount(); n != 1 {
		t.Fatalf("issued %d modifications across two executions, want 1", n)
	}
	e, _ = a.Entry(step.Key)
	if e.Status != StatusDone || e.Attempts != 2 {
		t.Errorf("after resume: %+v, want done with 2 attempts", e)
	}
	live, _ = f.VolumeByID("vol-a")
	if live.VolumeType != VolumeTypeGP3 || live.IOPS != 5200 {
		t.Errorf("resumed modification did not land: %+v", live)
	}
}

// TestIdempotency: re-executing a completed step is a no-op, with and without
// the ledger that remembers it. Both paths matter — the first is a re-run in
// the same process, the second is a controller restart.
func TestIdempotency(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	a := newActuator(t, f, clock, ModeApply, nil)
	step := stepFor("vol-a", 4000, 5200, 130)

	if err := a.Execute(t.Context(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls, mods := f.ModifyCallCount(), len(f.ModificationRequests)
	if err := a.Execute(t.Context(), step); err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	if f.ModifyCallCount() != calls {
		t.Errorf("re-executing issued another modification (%d → %d)", calls, f.ModifyCallCount())
	}
	if len(f.ModificationRequests) != mods {
		t.Error("re-executing a completed step still called the cloud")
	}

	// A restarted controller: fresh actuator, empty ledger, converted volume.
	fresh := newActuator(t, f, clock, ModeApply, nil)
	if err := fresh.Execute(t.Context(), step); err != nil {
		t.Fatalf("fresh actuator: %v", err)
	}
	if f.ModifyCallCount() != calls {
		t.Errorf("a fresh actuator re-issued a completed modification")
	}
	e, _ := fresh.Entry(step.Key)
	if e.Status != StatusNoop {
		t.Errorf("fresh actuator recorded %q, want %q", e.Status, StatusNoop)
	}

	// And a ledger reloaded from bytes remembers just as well.
	blob, err := a.LedgerJSON()
	if err != nil {
		t.Fatalf("LedgerJSON: %v", err)
	}
	restored := newActuator(t, f, clock, ModeApply, nil)
	if err := restored.RestoreLedger(blob); err != nil {
		t.Fatalf("RestoreLedger: %v", err)
	}
	if e, ok := restored.Entry(step.Key); !ok || !e.Terminal() {
		t.Errorf("restored ledger lost the completed step: %+v", e)
	}
	if err := restored.Execute(t.Context(), step); err != nil {
		t.Fatalf("restored actuator: %v", err)
	}
	if f.ModifyCallCount() != calls {
		t.Error("a restored ledger did not prevent a repeat modification")
	}
}

// TestRevertRestoresRecordedFrom: undo goes back to what was actually there,
// and for this unit that is always an upward move.
func TestRevertRestoresRecordedFrom(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	a := newActuator(t, f, clock, ModeApply, nil)
	step := stepFor("vol-a", 4000, 5200, 130)

	if err := a.Execute(t.Context(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Inside the cooldown, the revert is refused honestly rather than
	// attempted and lost: AWS enforces the same wait.
	if err := a.Revert(t.Context(), step); !errors.Is(err, ErrCooldown) {
		t.Fatalf("revert inside the cooldown: err = %v, want ErrCooldown", err)
	}
	clock.Advance(DefaultCooldown + time.Minute)

	if err := a.Revert(t.Context(), step); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	live, _ := f.VolumeByID("vol-a")
	if live.VolumeType != VolumeTypeGP2 {
		t.Fatalf("after revert the volume is %s, want gp2", live.VolumeType)
	}
	if live.ThroughputMBps != 0 {
		t.Errorf("reverted gp2 volume carries provisioned throughput %d", live.ThroughputMBps)
	}
	// Upward: the restored gp2 sustains 12,000 IOPS where the gp3 was
	// provisioned for 5,200.
	if live.IOPS != 12000 {
		t.Errorf("restored gp2 IOPS = %d, want 12000", live.IOPS)
	}
	if req := f.ModifyRequests[len(f.ModifyRequests)-1]; req.IOPS != 0 || req.ThroughputMBps != 0 {
		t.Errorf("the revert request provisioned gp2 IOPS/throughput: %+v", req)
	}
	var reverts int
	for _, e := range a.Ledger() {
		if e.Revert {
			reverts++
			if e.Status != StatusDone {
				t.Errorf("revert entry status = %q", e.Status)
			}
		}
	}
	if reverts != 1 {
		t.Errorf("%d revert entries in the ledger, want 1", reverts)
	}
}

// TestRevertRefusesToDegrade: an undo that lowers delivered performance is not
// an undo. It can only happen when the live volume is not what the step left
// behind, and quietly degrading somebody else's volume is worse than refusing.
func TestRevertRefusesToDegrade(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp3Volume("vol-a", 4000, 8000, 200)})
	a := newActuator(t, f, clock, ModeApply, nil)

	ref := domain.TargetRef{Domain: Kind, Scope: "acct/us-east-1", ID: "vol-a"}
	weak := SpecOf(gp3Volume("vol-a", 4000, 3000, 125))
	strong := SpecOf(gp3Volume("vol-a", 4000, 8000, 200))
	step := domain.Step{
		Key: domain.StepKey(ref, weak, strong), Target: ref, Action: domain.ActionInPlace,
		From: weak, To: strong,
	}
	err := a.Revert(t.Context(), step)
	if !errors.Is(err, ErrRevertWouldDegrade) {
		t.Fatalf("err = %v, want ErrRevertWouldDegrade", err)
	}
	if f.ModifyCallCount() != 0 {
		t.Error("a degrading revert reached the cloud")
	}
	// A non-in-place step is honestly irreversible here.
	step.Action = domain.ActionStopStart
	if err := a.Revert(t.Context(), step); !errors.Is(err, domain.ErrIrreversible) {
		t.Errorf("err = %v, want ErrIrreversible", err)
	}
}

// preflightCase is one scenario both modes must judge identically.
type preflightCase struct {
	name  string
	build func(*fakeClock) (*Fixture, domain.Step)
	want  error // nil means "both modes accept"
}

func preflightCases() []preflightCase {
	good := func(c *fakeClock) (*Fixture, domain.Step) {
		return newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)}), stepFor("vol-a", 4000, 5200, 130)
	}
	return []preflightCase{
		{"accepted", good, nil},
		{"missing volume", func(c *fakeClock) (*Fixture, domain.Step) {
			return newFixture(c, nil), stepFor("vol-a", 4000, 5200, 130)
		}, ErrVolumeMissing},
		{"drifted", func(c *fakeClock) (*Fixture, domain.Step) {
			return newFixture(c, []VolumeRecord{gp2Volume("vol-a", 2000)}), stepFor("vol-a", 4000, 5200, 130)
		}, ErrDrift},
		{"deleting", func(c *fakeClock) (*Fixture, domain.Step) {
			v := gp2Volume("vol-a", 4000)
			v.State = VolumeStateDeleting
			return newFixture(c, []VolumeRecord{v}), stepFor("vol-a", 4000, 5200, 130)
		}, ErrVolumeState},
		{"multi-attach", func(c *fakeClock) (*Fixture, domain.Step) {
			v := gp2Volume("vol-a", 4000)
			v.MultiAttachEnabled = true
			return newFixture(c, []VolumeRecord{v}), stepFor("vol-a", 4000, 5200, 130)
		}, ErrVolumeState},
		{"attaching", func(c *fakeClock) (*Fixture, domain.Step) {
			v := gp2Volume("vol-a", 4000)
			v.Attachments = []VolumeAttachment{{InstanceID: "i-x", State: AttachmentAttaching}}
			return newFixture(c, []VolumeRecord{v}), stepFor("vol-a", 4000, 5200, 130)
		}, ErrVolumeState},
		{"another modification in progress", func(c *fakeClock) (*Fixture, domain.Step) {
			f := newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)})
			f.ModificationPages = []DescribeVolumesModificationsOutput{{Modifications: []VolumeModification{{
				VolumeID: "vol-a", ModificationState: ModificationModifying,
				TargetVolumeType: VolumeTypeGP3, TargetSizeGiB: 4000, TargetIOPS: 9000,
				TargetThroughputMBps: 250, StartTime: c.Now(),
			}}}}
			return f, stepFor("vol-a", 4000, 5200, 130)
		}, ErrModificationInProgress},
		{"cooldown", func(c *fakeClock) (*Fixture, domain.Step) {
			f := newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)})
			f.ModificationPages = []DescribeVolumesModificationsOutput{{Modifications: []VolumeModification{{
				VolumeID: "vol-a", ModificationState: ModificationCompleted,
				StartTime: c.Now().Add(-2 * time.Hour), EndTime: c.Now().Add(-time.Hour),
			}}}}
			return f, stepFor("vol-a", 4000, 5200, 130)
		}, ErrCooldown},
		{"wrong action", func(c *fakeClock) (*Fixture, domain.Step) {
			s := stepFor("vol-a", 4000, 5200, 130)
			s.Action = domain.ActionRolling
			return newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)}), s
		}, ErrWrongAction},
		{"tampered key", func(c *fakeClock) (*Fixture, domain.Step) {
			s := stepFor("vol-a", 4000, 5200, 130)
			s.Key = "0000000000000000"
			return newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)}), s
		}, ErrStepKeyMismatch},
		{"resize attempt", func(c *fakeClock) (*Fixture, domain.Step) {
			s := stepFor("vol-a", 4000, 5200, 130)
			s.To = s.To.WithAttr(AttrSizeGiB, "8000")
			s.Key = domain.StepKey(s.Target, s.From, s.To)
			return newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)}), s
		}, ErrBadStep},
		{"invalid gp3 target", func(c *fakeClock) (*Fixture, domain.Step) {
			s := stepFor("vol-a", 4000, 2000, 130) // below gp3's 3,000 IOPS floor
			return newFixture(c, []VolumeRecord{gp2Volume("vol-a", 4000)}), s
		}, ErrBadStep},
		{"gp2 target carrying invented numbers", func(c *fakeClock) (*Fixture, domain.Step) {
			// A revert's target is a gp2 spec, which legitimately carries the
			// size-derived baseline. A DIFFERENT number was invented.
			s := stepFor("vol-a", 4000, 5200, 130)
			s.From, s.To = s.To, s.From.WithAttr(AttrIOPS, "9999")
			s.Key = domain.StepKey(s.Target, s.From, s.To)
			return newFixture(c, []VolumeRecord{gp3Volume("vol-a", 4000, 5200, 130)}), s
		}, ErrBadStep},
		{"revert to the recorded gp2 baseline", func(c *fakeClock) (*Fixture, domain.Step) {
			s := stepFor("vol-a", 4000, 5200, 130)
			s.From, s.To = s.To, s.From
			s.Key = domain.StepKey(s.Target, s.From, s.To)
			return newFixture(c, []VolumeRecord{gp3Volume("vol-a", 4000, 5200, 130)}), s
		}, nil},
	}
}

// TestDryRunAndApplyAgree is the anti-regression for the bug class that bit
// pkg/actuate: an apply path that can do something dry-run never showed.
//
// Both modes run the identical pre-flight, so for every scenario that ends
// before a modification is issued, the verdict must be identical. Outcomes
// that only exist AFTER issuing (poll timeout, AWS-side failure) are excluded
// by construction — dry-run does not wait for a change it did not make — and
// are covered by their own tests.
func TestDryRunAndApplyAgree(t *testing.T) {
	for _, c := range preflightCases() {
		t.Run(c.name, func(t *testing.T) {
			dryClock := newClock(base)
			dryF, dryStep := c.build(dryClock)
			dry := newActuator(t, dryF, dryClock, ModeDryRun, nil)
			dryErr := dry.Execute(t.Context(), dryStep)

			applyClock := newClock(base)
			applyF, applyStep := c.build(applyClock)
			app := newActuator(t, applyF, applyClock, ModeApply, nil)
			applyErr := app.Execute(t.Context(), applyStep)

			switch {
			case c.want == nil && (dryErr != nil || applyErr != nil):
				t.Fatalf("expected acceptance: dry-run %v, apply %v", dryErr, applyErr)
			case c.want != nil && !errors.Is(dryErr, c.want):
				t.Fatalf("dry-run err = %v, want %v", dryErr, c.want)
			case c.want != nil && !errors.Is(applyErr, c.want):
				t.Fatalf("apply err = %v, want %v", applyErr, c.want)
			}
			if n := dryF.ModifyCallCount(); n != 0 {
				t.Fatalf("dry-run issued %d modification(s)", n)
			}
			// The ledger tells the same story in both modes.
			dryEntry, _ := dry.Entry(dryStep.Key)
			applyEntry, _ := app.Entry(applyStep.Key)
			if (dryEntry.Error == "") != (applyEntry.Error == "") {
				t.Fatalf("ledger disagrees: dry-run %q vs apply %q", dryEntry.Error, applyEntry.Error)
			}
			if c.want == nil && dryEntry.Status != StatusDryRun {
				t.Fatalf("dry-run recorded %q, want %q", dryEntry.Status, StatusDryRun)
			}
			if c.want == nil && !strings.Contains(dryEntry.Detail, "ModifyVolume") {
				t.Fatalf("dry-run did not record the call it would make: %q", dryEntry.Detail)
			}
		})
	}
}

// TestApplyRecordsFailures: a failure fails loudly and is recorded; nothing is
// assumed.
func TestApplyRecordsFailures(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	f.FailModification = true
	a := newActuator(t, f, clock, ModeApply, nil)
	step := stepFor("vol-a", 4000, 5200, 130)

	err := a.Execute(t.Context(), step)
	if !errors.Is(err, ErrModificationFailed) {
		t.Fatalf("err = %v, want ErrModificationFailed", err)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusFailed || e.Error == "" {
		t.Errorf("ledger entry = %+v, want a failed entry carrying the error", e)
	}
	if e.Terminal() {
		t.Error("a failed step reads as terminal, so a retry would be skipped")
	}

	// A transport failure on the mutation itself is recorded the same way.
	clock2 := newClock(base)
	f2 := newFixture(clock2, []VolumeRecord{gp2Volume("vol-b", 4000)})
	f2.ModifyFailAt = 1
	a2 := newActuator(t, f2, clock2, ModeApply, nil)
	step2 := stepFor("vol-b", 4000, 5200, 130)
	if err := a2.Execute(t.Context(), step2); err == nil {
		t.Fatal("a failed ModifyVolume call returned success")
	}
	if e, _ := a2.Entry(step2.Key); e.Status != StatusFailed {
		t.Errorf("ledger status = %q, want %q", e.Status, StatusFailed)
	}
}

// TestResumeOfMatchingInFlightModification: a modification toward the same
// target that is already running is resumed, not refused and not duplicated.
func TestResumeOfMatchingInFlightModification(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	f.ModificationPages = []DescribeVolumesModificationsOutput{{Modifications: []VolumeModification{{
		VolumeID: "vol-a", ModificationState: ModificationOptimizing,
		TargetVolumeType: VolumeTypeGP3, TargetSizeGiB: 4000,
		TargetIOPS: 5200, TargetThroughputMBps: 130, StartTime: base,
	}}}}
	a := newActuator(t, f, clock, ModeApply, nil)
	step := stepFor("vol-a", 4000, 5200, 130)

	if err := a.Execute(t.Context(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := f.ModifyCallCount(); n != 0 {
		t.Errorf("issued %d modification(s) while one was already running toward the same target", n)
	}
	if e, _ := a.Entry(step.Key); e.Status != StatusDone {
		t.Errorf("status = %q, want %q", e.Status, StatusDone)
	}
}

// TestContextCancellation: every call is bounded by the caller's context.
func TestContextCancellation(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)})
	f.PollsToOptimizing = 50
	ctx, cancel := context.WithCancel(t.Context())
	// A poll wait that outlives the caller's patience: the cancellation, not
	// the poll budget, must be what ends this.
	a := newActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
		c.PollTimeout = time.Hour
		c.Sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}
	})
	step := stepFor("vol-a", 4000, 5200, 130)

	err := a.Execute(ctx, step)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if e, _ := a.Entry(step.Key); e.Terminal() {
		t.Error("a cancelled step reads as terminal")
	}
}

func TestLedgerOrderingAndSerialization(t *testing.T) {
	clock := newClock(base)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000), gp2Volume("vol-b", 2000)})
	a := newActuator(t, f, clock, ModeApply, nil)

	for _, s := range []domain.Step{stepFor("vol-a", 4000, 5200, 130), stepFor("vol-b", 2000, 3000, 125)} {
		if err := a.Execute(t.Context(), s); err != nil {
			t.Fatalf("Execute %s: %v", s.Target.ID, err)
		}
	}
	entries := a.Ledger()
	if len(entries) != 2 || entries[0].Target.ID != "vol-a" || entries[1].Target.ID != "vol-b" {
		t.Fatalf("ledger = %+v, want insertion order", entries)
	}
	blob, err := a.LedgerJSON()
	if err != nil {
		t.Fatalf("LedgerJSON: %v", err)
	}
	again, err := a.LedgerJSON()
	if err != nil || string(blob) != string(again) {
		t.Error("LedgerJSON is not deterministic")
	}
	if err := a.RestoreLedger([]byte("{")); err == nil {
		t.Error("malformed ledger bytes accepted")
	}
	if err := a.RestoreLedger(nil); err != nil {
		t.Errorf("RestoreLedger(nil): %v", err)
	}
}
