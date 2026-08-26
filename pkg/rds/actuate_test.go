package rds

// The execute path: what is sent, what is not, and what happens when a
// controller dies halfway.

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// --- FINDINGS.md §5.5: the mutate input has three fields and no more --------

// The test §5.5 names. The struct is inspected by REFLECTION rather than by
// reading the source, so a field added tomorrow fails this test rather than
// slipping past a reviewer who was looking at the doc comment.
func TestMutateInputCannotChangeClassStorageOrAZ(t *testing.T) {
	got := structFieldNames(t, ModifyStorageInput{})
	want := []string{"DBInstanceIdentifier", "ClientToken", "StorageType", "IOPS", "StorageThroughputMBps"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ModifyStorageInput fields = %v, want exactly %v.\n"+
			"Three of those change the instance (StorageType, IOPS, StorageThroughputMBps); the other two "+
			"name the target and the attempt. Adding a sixth field is adding a way for this unit to change "+
			"something FINDINGS.md §5.5 says it never changes.", got, want)
	}
	// And the things it must never be able to express, named one by one so a
	// failure says which one came back.
	banned := []string{
		"class", "instanceclass", "multiaz", "availabilityzone", "az", "engineversion", "engine",
		"allocatedstorage", "storagesize", "size", "masteruserpassword", "password", "applyimmediately",
		"deletionprotection", "parametergroup", "backupretention", "maintenancewindow", "subnetgroup",
		"securitygroup", "publiclyaccessible", "port", "cacertificate",
	}
	for _, f := range got {
		lower := strings.ToLower(f)
		for _, b := range banned {
			if strings.Contains(lower, b) {
				t.Errorf("ModifyStorageInput has field %q, which can express %q", f, b)
			}
		}
	}
}

// --- the approval gate, structurally ----------------------------------------

// The zero ApprovedStep is the only one a foreign package can build, and it
// cannot act. This is the runtime half of the "unapproved is unrepresentable"
// claim; the compile-time half is that ApprovedStep's fields are unexported.
func TestZeroApprovedStepCannotAct(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeApply)
	for name, err := range map[string]error{
		"Execute": a.Execute(context.Background(), ApprovedStep{}),
		"Revert":  a.Revert(context.Background(), ApprovedStep{}),
	} {
		if !errors.Is(err, ErrNotApproved) {
			t.Errorf("%s with a zero ApprovedStep: err = %v, want ErrNotApproved", name, err)
		}
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%d modification(s) were issued without an approval", n)
	}
	// And a step carrying a step but no approval is equally inert — the
	// `authorized` bit is separate from the step for exactly this reason.
	step := actDefaultStep()
	if err := a.Execute(context.Background(), ApprovedStep{step: step}); !errors.Is(err, ErrNotApproved) {
		t.Errorf("a hand-built ApprovedStep acted: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%d modification(s) were issued by a hand-built ApprovedStep", n)
	}
}

// *Actuator must NOT satisfy domain.Actuator: a registry cannot be handed one
// and driven. Only *BoundActuator — an actuator with an approval attached —
// can be registered.
func TestActuatorIsNotRegistrableWithoutApproval(t *testing.T) {
	a := actActuator(t, actFixture(t), ModeApply)
	if _, ok := any(a).(domain.Actuator); ok {
		t.Fatal("*rds.Actuator satisfies domain.Actuator; a registry could then execute steps with no " +
			"approval anywhere in the picture")
	}
	if _, err := a.Bind(Approval{}); !errors.Is(err, ErrNotApproved) {
		t.Errorf("Bind accepted the zero Approval: %v", err)
	}
	b, err := a.Bind(actApproval(t, actDefaultStep()))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, ok := any(b).(domain.Actuator); !ok {
		t.Fatal("*rds.BoundActuator does not satisfy domain.Actuator; nothing is registrable at all")
	}
}

// An approval expires while a plan runs, and the expiry is re-read at every
// step rather than once at the top.
func TestExpiredApprovalCannotAct(t *testing.T) {
	step := actDefaultStep()
	f := actFixture(t)
	a, err := NewActuator(f, ActuatorConfig{Mode: ModeApply,
		Now:   func() time.Time { return actNow().Add(2 * time.Hour) }, // past ExpiresAt
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	as, err := actApproval(t, step).Authorize(step)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Execute(context.Background(), as); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("an expired approval acted: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%d modification(s) were issued under an expired approval", n)
	}
}

// A token approved for one plan does not authorize a step from another.
func TestApprovalDoesNotCoverAnotherPlan(t *testing.T) {
	planned := actDefaultStep()
	other := actStep(
		actSpec(actEngine, StorageGP2, actSize, -1, -1),
		actSpec(actEngine, StorageGP3, actSize, 64000, 4000),
	)
	ap := actApproval(t, planned)
	if _, err := ap.Authorize(other); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("an approval covered a step it never saw: %v", err)
	}
	// Through the registrable form, the refusal is recorded rather than
	// silently dropped.
	a := actActuator(t, actFixture(t), ModeApply)
	b, err := a.Bind(ap)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Execute(context.Background(), other); !errors.Is(err, ErrStepNotInPlan) {
		t.Fatalf("BoundActuator executed an uncovered step: %v", err)
	}
	if e, ok := a.Entry(other.Key); !ok || e.Status != StatusRefused {
		t.Errorf("the uncovered step left no refusal in the ledger (%+v)", e)
	}
}

// --- dry-run is the default, and it is symmetric with apply -----------------

func TestDryRunIsTheDefaultAndIssuesNothing(t *testing.T) {
	f := actFixture(t)
	a, err := NewActuator(f, ActuatorConfig{Now: actClock()}) // no Mode
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode() != ModeDryRun {
		t.Fatalf("default mode = %q, want %q", a.Mode(), ModeDryRun)
	}
	step := actDefaultStep()
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("dry-run issued %d modification(s)", n)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusDryRun {
		t.Errorf("status = %q, want %q", e.Status, StatusDryRun)
	}
	// A dry-run records the EXACT call an apply would make, which is what
	// makes it a preview rather than a promise.
	if e.Sent.DBInstanceIdentifier != actID || e.Sent.StorageType != StorageGP3 {
		t.Errorf("dry-run recorded no call: %+v", e.Sent)
	}
	if e.Sent.StorageThroughputMBps != 1000 {
		t.Errorf("dry-run recorded --storage-throughput %d, want 1000", e.Sent.StorageThroughputMBps)
	}
	// An unknown mode is rejected at the constructor, never defaulted.
	if _, err := NewActuator(f, ActuatorConfig{Mode: "aply", Now: actClock()}); err == nil {
		t.Error("a typo'd mode was accepted; everything past the constructor trusts Mode")
	}
}

// --- FINDINGS.md §5.1: which arguments are SENT -----------------------------

// 12,000 IOPS is exactly the striped baseline and must NOT be sent; 1,000
// MiB/s is above the 500 MiB/s baseline and must be. One step, both halves.
func TestActuateSendsOnlyProvisionedArguments(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	call, err := a.PlannedCall(context.Background(), step)
	if err != nil {
		t.Fatalf("PlannedCall: %v", err)
	}
	if call.IOPS != 0 {
		t.Errorf("--iops %d would be sent for a value equal to the free 12,000 IOPS baseline", call.IOPS)
	}
	if call.StorageThroughputMBps != 1000 {
		t.Errorf("--storage-throughput = %d, want 1000", call.StorageThroughputMBps)
	}
	if call.StorageType != StorageGP3 {
		t.Errorf("--storage-type = %q", call.StorageType)
	}
	if call.ClientToken == "" {
		t.Error("the call carries no idempotency identity")
	}
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("apply: %v", err)
	}
	e, _ := a.Entry(step.Key)
	if !strings.Contains(e.Detail, "--iops omitted") {
		t.Errorf("the ledger does not say the baseline was NOT bought: %q", e.Detail)
	}
}

// --- apply, and observing rather than assuming ------------------------------

func TestApplyIssuesExactlyOneModificationAndObservesIt(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := f.Mutations(); n != 1 {
		t.Fatalf("issued %d modification(s), want exactly 1", n)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusDone {
		t.Fatalf("status = %q (%s), want %q", e.Status, e.Error, StatusDone)
	}
	if e.Stage != StageDone {
		t.Errorf("stage = %q, want %q", e.Stage, StageDone)
	}
	if e.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", e.Attempts)
	}
	if e.Polls == 0 {
		t.Error("the actuator reported success without observing the instance")
	}
	if e.IssuedAt.IsZero() {
		t.Error("IssuedAt is zero; an operator cannot tell when the 24-hour window started")
	}
	// The instance really got there, and by the documented route: the values
	// land at storage-optimization, not at `available`.
	live, _ := f.Instance(actID)
	if live.StorageType != StorageGP3 || live.StorageThroughputMBps != 1000 {
		t.Errorf("the fixture instance is %s at %d MiB/s", live.StorageType, live.StorageThroughputMBps)
	}
	if live.Status != StatusAvailable {
		t.Errorf("the instance settled at %q", live.Status)
	}
	// And the modification is now in the instance's own 24-hour history.
	if got := len(f.Events[actID]); got != 1 {
		t.Errorf("the modification left %d events; the cooldown cannot see it", got)
	}
}

// A poll budget that runs out is IN-FLIGHT, not failure and not success.
// storage-optimization routinely outlives any sane budget, so this is the
// EXPECTED outcome of a successful apply against a real database.
func TestPollTimeoutIsInFlightNotFailure(t *testing.T) {
	f := actFixture(t)
	f.SettleAfter = 1000 // never settles inside the budget
	a, err := NewActuator(f, ActuatorConfig{Mode: ModeApply, Now: actClock(),
		PollInterval: time.Second, PollTimeout: 0,
		Sleep: func(ctx context.Context, d time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	step := actDefaultStep()
	err = a.Execute(context.Background(), actApproved(t, step))
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout", err)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusInFlight {
		t.Fatalf("status = %q, want %q", e.Status, StatusInFlight)
	}
	if e.Terminal() {
		t.Error("an in-flight entry is terminal; a resume would skip a running modification")
	}
	if e.Settled() {
		t.Error("an in-flight entry reads as settled; Unsettled() would not return it")
	}
	if len(a.Unsettled()) != 1 {
		t.Error("Unsettled() does not report the running modification")
	}
}

// --- idempotency -------------------------------------------------------------

// Re-executing a completed step is a no-op with NO cloud call at all.
func TestReExecutingACompletedStepIsANoop(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	callsBefore := len(f.Ops())
	for range 3 {
		if err := actExecute(t, a, step); err != nil {
			t.Fatalf("re-execute: %v", err)
		}
	}
	if n := f.Mutations(); n != 1 {
		t.Fatalf("re-executing issued %d modification(s), want 1", n)
	}
	if got := len(f.Ops()); got != callsBefore {
		t.Errorf("re-executing a completed step made %d cloud call(s); it must make none", got-callsBefore)
	}
}

// A step whose instance ALREADY reads as the target is a no-op, even from a
// cold ledger: doing nothing is always allowed, including inside a cooldown.
func TestAlreadyAtTargetIsANoopEvenInCooldown(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) {
		r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 12000, 1000
	})
	for i := range MaxStorageModificationsPer24h {
		f.Events[actID] = append(f.Events[actID], EventRecord{
			SourceIdentifier: actID, SourceType: EventSourceDBInstance,
			Message:    "Finished applying modification to storage throughput",
			Categories: []string{EventCategoryConfigurationChange},
			Date:       actNow().Add(-time.Duration(i+1) * time.Hour),
		})
	}
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	err := actExecute(t, a, step)
	// The cooldown gate runs first and refuses — which is correct and is the
	// conservative order. What must NOT happen is a modification.
	if err != nil && RefusalCode(err) != RefuseCooldown {
		t.Fatalf("err = %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%d modification(s) were issued for an instance already at the target", n)
	}
}

// The lost-response case: the modification LANDED and the call reported an
// error. A retry must observe the pending change and resume, never re-issue.
func TestALostResponseDoesNotIssueASecondModification(t *testing.T) {
	f := actFixture(t)
	f.SettleAfter = 1
	f.FailAfter = func(op string, n int) error {
		if op == OpModifyStorage && n == 1 {
			return errors.New("RequestTimeout: the response never arrived")
		}
		return nil
	}
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	if err := actExecute(t, a, step); err == nil {
		t.Fatal("the lost response was reported as success")
	}
	e, _ := a.Entry(step.Key)
	if e.Terminal() {
		t.Fatal("a failed-but-landed modification is terminal; the retry would never look")
	}
	// The retry: the pending change is observed, the machine resumes.
	f.FailAfter = nil
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("resume after a lost response: %v", err)
	}
	// The retry issued NOTHING: the pending change was observed and resumed.
	if n := f.Mutations(); n != 1 {
		t.Fatalf("the retry issued a second modification (%d seam calls total)", n)
	}
	if got := len(f.Events[actID]); got != 1 {
		t.Fatalf("the instance recorded %d storage modifications, want 1: a duplicate spends one of four",
			got)
	}
}

// --- resumability at EVERY stage boundary ------------------------------------

// A controller dies at each stage in turn, restarts with only the persisted
// ledger, and resumes. The AWS-side state is RE-OBSERVED, never assumed: the
// resumed actuator is a brand-new one that has seen nothing.
func TestResumeAtEveryStageBoundary(t *testing.T) {
	stages := []struct {
		name  string
		phase modPhase
		set   func(*InstanceStateRecord)
	}{
		{"accepted", phaseAccepted, func(r *InstanceStateRecord) {
			r.Status = StatusAvailable
			r.PendingStorageType, r.PendingStorageThroughputMBps = StorageGP3, 1000
		}},
		{"modifying", phaseModifying, func(r *InstanceStateRecord) {
			r.Status = StatusModifying
			r.PendingStorageType, r.PendingStorageThroughputMBps = StorageGP3, 1000
		}},
		{"storage-optimization", phaseOptimizing, func(r *InstanceStateRecord) {
			// The values have LANDED and the instance is still optimizing.
			r.Status = StatusStorageOptimization
			r.StorageType, r.StorageThroughputMBps = StorageGP3, 1000
		}},
		{"available-at-target", phaseNone, func(r *InstanceStateRecord) {
			r.Status = StatusAvailable
			r.StorageType, r.StorageThroughputMBps = StorageGP3, 1000
		}},
	}
	step := actDefaultStep()
	for _, s := range stages {
		t.Run(s.name, func(t *testing.T) {
			f := actFixture(t, s.set)
			f.phase[actID] = s.phase
			// The crashed controller's ledger: the step was issued and not
			// finished. This is exactly what Persist would have flushed.
			crashed := []LedgerEntry{{
				Key: step.Key, Target: step.Target, Action: step.Action,
				From: step.From, To: step.To, Mode: ModeApply,
				Status: StatusInFlight, Stage: StageAccepted, Attempts: 1,
				StartedAt: actNow().Add(-time.Hour), IssuedAt: actNow().Add(-time.Hour),
			}}
			blob, err := json.Marshal(crashed)
			if err != nil {
				t.Fatal(err)
			}
			// A BRAND NEW actuator. It knows nothing except the ledger.
			a := actActuator(t, f, ModeApply)
			if err := a.RestoreLedger(blob); err != nil {
				t.Fatalf("RestoreLedger: %v", err)
			}
			if got := a.Unsettled(); len(got) != 1 {
				t.Fatalf("Unsettled() = %d entries, want 1: a restarted controller would not resume", len(got))
			}
			if err := actExecute(t, a, step); err != nil {
				t.Fatalf("resume from %s: %v", s.name, err)
			}
			// Nothing new was issued: every one of these stages is a
			// modification RDS has already taken.
			if n := f.Mutations(); n != 0 {
				t.Fatalf("resuming from %s issued %d modification(s); RDS had already accepted one",
					s.name, n)
			}
			e, _ := a.Entry(step.Key)
			if e.Status != StatusDone && e.Status != StatusNoop {
				t.Fatalf("resume from %s ended at %q (%s)", s.name, e.Status, e.Error)
			}
			live, _ := f.Instance(actID)
			if live.StorageType != StorageGP3 || live.StorageThroughputMBps != 1000 {
				t.Errorf("resume from %s left the instance at %s / %d MiB/s",
					s.name, live.StorageType, live.StorageThroughputMBps)
			}
		})
	}
}

// The stage is derived from the LIVE instance and never from the ledger. A
// ledger that lies about the stage changes nothing.
func TestStageIsDerivedFromAWSNotFromTheLedger(t *testing.T) {
	step := actDefaultStep()
	f := actFixture(t) // gp2, available, nothing pending: really StageReady
	lying := []LedgerEntry{{
		Key: step.Key, Target: step.Target, Action: step.Action, From: step.From, To: step.To,
		Mode: ModeApply, Status: StatusInFlight, Stage: StageOptimizing, Attempts: 1,
	}}
	blob, _ := json.Marshal(lying)
	a := actActuator(t, f, ModeApply)
	if err := a.RestoreLedger(blob); err != nil {
		t.Fatal(err)
	}
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if n := f.Mutations(); n != 1 {
		t.Fatalf("the actuator believed the ledger's %q and issued %d modification(s), want 1",
			StageOptimizing, n)
	}
}

// --- persist-before-mutate ----------------------------------------------------

// A Persist that fails ABORTS the modification. A storage change nobody wrote
// down is the state this unit must never reach.
func TestPersistFailureAbortsTheMutation(t *testing.T) {
	f := actFixture(t)
	a, err := NewActuator(f, ActuatorConfig{Mode: ModeApply, Now: actClock(),
		Sleep:   func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		Persist: func(ctx context.Context, b []byte) error { return errors.New("disk full") }})
	if err != nil {
		t.Fatal(err)
	}
	step := actDefaultStep()
	if err := a.Execute(context.Background(), actApproved(t, step)); err == nil {
		t.Fatal("a Persist failure did not abort the modification")
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%d modification(s) were issued after the ledger failed to persist", n)
	}
}

// Persist is called BEFORE the mutating call, so the crash window contains
// "we may have modified it" and never "we definitely did and nobody knows".
func TestPersistHappensBeforeTheCall(t *testing.T) {
	f := actFixture(t)
	var order []string
	f.Fail = func(op string, n int) error {
		if op == OpModifyStorage {
			order = append(order, "modify")
		}
		return nil
	}
	a, err := NewActuator(f, ActuatorConfig{Mode: ModeApply, Now: actClock(),
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		Persist: func(ctx context.Context, b []byte) error {
			order = append(order, "persist")
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Execute(context.Background(), actApproved(t, actDefaultStep())); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "persist" || order[1] != "modify" {
		t.Fatalf("call order = %v, want persist before modify", order)
	}
}

// --- revert -------------------------------------------------------------------

// FINDINGS.md §5.6: a revert restores the recorded From exactly.
func TestRevertRestoresTheRecordedFrom(t *testing.T) {
	// A raise, so its undo is a reduction — which the ratchet refuses. That
	// is the honest behaviour and it is asserted below; the reversible case
	// is a storage-type conversion whose undo is caught by the same rule.
	f := actFixture(t, func(r *InstanceStateRecord) {
		r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 12000, 1000
	})
	step := actStep(
		actSpec(actEngine, StorageGP3, actSize, 12000, 1000),
		actSpec(actEngine, StorageGP3, actSize, 20000, 2000),
	)
	a := actActuator(t, f, ModeApply)
	as := actApproved(t, step)
	if err := a.Execute(context.Background(), as); err != nil {
		t.Fatalf("apply: %v", err)
	}
	live, _ := f.Instance(actID)
	if live.IOPS != 20000 || live.StorageThroughputMBps != 2000 {
		t.Fatalf("the raise did not land: %+v", live)
	}
	// The undo restores exactly what was recorded as From — and the ratchet
	// refuses it, because a revert of a RAISE is a reduction. The refusal is
	// the product: an operator is told the undo is a reduction rather than
	// having one performed at 3 a.m.
	err := a.Revert(context.Background(), as)
	r := wantRefusal(t, err, RefuseRatchet)
	if !strings.Contains(r.Reason, "never down") {
		t.Errorf("the revert refusal does not state the rule: %q", r.Reason)
	}
	if n := f.Mutations(); n != 1 {
		t.Fatalf("the refused revert issued a modification (%d total)", n)
	}
	// The inverse step has its OWN key and its own ledger entry, so the undo
	// is auditable separately from the change.
	inv := domain.StepKey(step.Target, step.To, step.From)
	e, ok := a.Entry(inv)
	if !ok {
		t.Fatal("the revert left no ledger entry")
	}
	if !e.Revert || e.Origin != step.Key {
		t.Errorf("the revert entry does not point at the step it undoes: revert=%v origin=%q",
			e.Revert, e.Origin)
	}
}

// A revert can never be talked below the regime baseline (FINDINGS.md §5.6).
// The recorded From is floored at the baseline by configOf before anything
// looks at it, and the live envelope still gates the result.
func TestRevertCannotGoBelowTheRegimeBaseline(t *testing.T) {
	e := ParseEngine(actEngine, "general-public-license")
	r := GP3RegimeFor(e, actSize)
	// A From that names an impossible configuration — 500 IOPS on a striped
	// volume whose floor is 12,000.
	for _, iops := range []int32{-1, 0, 100, 500, r.BaselineIOPS - 1} {
		got := configOf(r, actSize, iops, 0)
		if got.IOPS < r.BaselineIOPS {
			t.Fatalf("configOf(%d IOPS) = %d, below the non-reducible %d baseline",
				iops, got.IOPS, r.BaselineIOPS)
		}
		if got.ThroughputMBps < r.BaselineThroughputMBps {
			t.Fatalf("configOf produced %d MiB/s, below the %d baseline",
				got.ThroughputMBps, r.BaselineThroughputMBps)
		}
		if got.ProvisionedIOPS && got.IOPS == r.BaselineIOPS {
			t.Fatalf("configOf claims to provision the free baseline")
		}
	}
	// End to end: a revert whose From names 500 IOPS restores the BASELINE,
	// not 500, and is refused as a reduction rather than performed.
	f := actFixture(t, func(rec *InstanceStateRecord) {
		rec.StorageType, rec.IOPS, rec.StorageThroughputMBps = StorageGP3, 12000, 1000
	})
	step := actStep(
		actSpec(actEngine, StorageGP3, actSize, 500, 100), // an impossible From
		actSpec(actEngine, StorageGP3, actSize, 12000, 1000),
	)
	a := actActuator(t, f, ModeApply)
	as := actApproved(t, step)
	// The forward step is a no-op: the live volume already reads as To.
	if err := a.Execute(context.Background(), as); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := a.Revert(context.Background(), as); err == nil {
		t.Fatal("a revert to an impossible configuration was performed")
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%d modification(s) were issued restoring a sub-baseline configuration", n)
	}
}

// A non-in-place action has no undo here, and says so honestly.
func TestRevertRefusesAnIrreversibleAction(t *testing.T) {
	step := actDefaultStep()
	step.Action = domain.ActionStopStart
	step.Key = domain.StepKey(step.Target, step.From, step.To)
	a := actActuator(t, actFixture(t), ModeApply)
	as := ApprovedStep{step: step, approval: actApproval(t, step), authorized: true}
	if err := a.Revert(context.Background(), as); !errors.Is(err, domain.ErrIrreversible) {
		t.Fatalf("err = %v, want domain.ErrIrreversible", err)
	}
	if !errors.Is(ErrIrreversible(), domain.ErrIrreversible) {
		t.Error("ErrIrreversible() is not domain.ErrIrreversible")
	}
}

// --- determinism ---------------------------------------------------------------

// Money is summed through SumUSD — sorted by name, then added — so the total
// cannot depend on the order entries arrived in. The premise is asserted too:
// naive accumulation really does produce different totals over these values.
func TestActuateLedgerSummaryIsShuffleInvariant(t *testing.T) {
	entries := []LedgerEntry{
		{Key: "a", Status: StatusDone, Claimed: true, ClaimedMonthlyUSD: 0.1},
		{Key: "b", Status: StatusDone, Claimed: true, ClaimedMonthlyUSD: 0.2},
		{Key: "c", Status: StatusDone, Claimed: true, ClaimedMonthlyUSD: 0.3},
		{Key: "d", Status: StatusDone, Claimed: true, ClaimedMonthlyUSD: 1e17},
		{Key: "e", Status: StatusDone, Claimed: true, ClaimedMonthlyUSD: -1e17},
		{Key: "f", Status: StatusDone},
		{Key: "g", Status: StatusRefused, RefusalCode: RefuseCooldown, ValidFrom: actNow().Add(time.Hour)},
		{Key: "h", Status: StatusRefused, RefusalCode: RefuseCooldown, ValidFrom: actNow().Add(2 * time.Hour)},
		{Key: "i", Status: StatusInFlight},
	}
	want, err := json.Marshal(Summarize(entries))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(11))
	naive := map[float64]bool{}
	for range 200 {
		shuffled := append([]LedgerEntry(nil), entries...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := json.Marshal(Summarize(shuffled))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("Summarize is order-dependent:\n want %s\n  got %s", want, got)
		}
		var sum float64
		for _, e := range shuffled {
			if e.Claimed {
				sum += e.ClaimedMonthlyUSD
			}
		}
		naive[sum] = true
	}
	if len(naive) < 2 {
		t.Fatalf("the premise does not hold: naive accumulation produced %d distinct total(s), so this "+
			"test would pass even if SumUSD were removed", len(naive))
	}
	s := Summarize(entries)
	if s.Unclaimed != 1 {
		t.Errorf("Unclaimed = %d, want 1: a total must never quietly mean everything", s.Unclaimed)
	}
	if s.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1", s.InFlight)
	}
	if !s.NextClears.Equal(actNow().Add(time.Hour)) {
		t.Errorf("NextClears = %s, want the EARLIEST dated refusal", s.NextClears)
	}
}

// The ledger round-trips, and the bytes are stable across processes.
func TestLedgerJSONIsStable(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	if err := actExecute(t, a, step); err != nil {
		t.Fatal(err)
	}
	first, err := a.LedgerJSON()
	if err != nil {
		t.Fatal(err)
	}
	b := actActuator(t, actFixture(t), ModeApply)
	if err := b.RestoreLedger(first); err != nil {
		t.Fatal(err)
	}
	second, err := b.LedgerJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("ledger did not round-trip:\n %s\n %s", first, second)
	}
}
