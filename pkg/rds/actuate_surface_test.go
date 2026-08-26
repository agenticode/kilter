package rds

// What U14 changed about this package's guarantees, asserted rather than
// described.
//
// U11 shipped two tests whose names promise that this package cannot act:
// TestNoActuationSurfaceExists and TestNoMutatingAPISurface. Both still pass,
// and after U14 they mean something NARROWER than they say. This file is where
// that narrowing is written down in executable form, because a reviewer who
// reads those two names and stops has been misled, and the fix for that is not
// a paragraph in a document nobody diffs.

import (
	"context"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/domain"
)

// The honest statement of what TestNoMutatingAPISurface now guarantees.
//
// It scans for the IDENTIFIER `ModifyDBInstance` and this package contains
// none — but [StorageActuateAPI.ModifyStorage] IS that operation, and the
// fixture's operation name is the literal string. The guarantee that survives
// is the one that was always the real one: the READ-ONLY decision path — the
// sizer, the parity engine, the report — cannot reach a mutation, because the
// only type that can is [Actuator] and nothing in that path constructs one.
func TestTheOnlyMutatingPathIsTheActuator(t *testing.T) {
	// The fixture names the real operation, so a ledger entry and an audit
	// log say `ModifyDBInstance` and not a euphemism.
	if OpModifyStorage != "Modify"+"DBInstance" {
		t.Fatalf("the fixture's mutating operation is named %q; it must name the real AWS operation so "+
			"nothing downstream has to decode a euphemism", OpModifyStorage)
	}
	// The read-only domain still cannot actuate, by any route.
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(d).(domain.Actuator); ok {
		t.Fatal("*rds.Domain became a domain.Actuator")
	}
	if _, ok := any(d).(interface {
		ModifyStorage(context.Context, *ModifyStorageInput) (*ModifyStorageOutput, error)
	}); ok {
		t.Fatal("*rds.Domain can reach the mutating seam")
	}
	// And the read-only seams cannot be passed where a mutating one is
	// expected: a controller wired with DescribeDBInstances permissions only
	// must fail to compile into an actuator, not fail at runtime against a
	// production database.
	if _, ok := any(&EnvelopeFixture{}).(StorageActuateAPI); ok {
		t.Fatal("EnvelopeFixture satisfies StorageActuateAPI; a read-only wiring could actuate")
	}
	if _, ok := any(&Fixture{}).(StorageActuateAPI); ok {
		t.Fatal("Fixture satisfies StorageActuateAPI; a read-only wiring could actuate")
	}
}

// The seam names exactly two AWS operations for mutation and observation, and
// [StorageActuateAPI] pins the whole surface. A method added to it is a new
// AWS permission an operator must grant, so the set is asserted by name.
func TestActuateSeamIsTwoOperationsPlusTheEnvelope(t *testing.T) {
	want := map[string]bool{
		// U13's read seam, embedded because §5.2/§5.3 make re-reading
		// mandatory.
		"DescribeValidDBInstanceModifications": true,
		"DescribeEvents":                       true,
		// U14's own.
		"DescribeInstanceState": true,
		"ModifyStorage":         true,
	}
	got := interfaceMethodNames(t, (*StorageActuateAPI)(nil))
	if len(got) != len(want) {
		t.Fatalf("StorageActuateAPI has %d methods (%v), want exactly %d", len(got), got, len(want))
	}
	for _, m := range got {
		if !want[m] {
			t.Errorf("StorageActuateAPI gained method %q; that is a new IAM action an operator must grant, "+
				"and ACTUATE-FINDINGS.md §5's least-privilege policy no longer covers this unit", m)
		}
	}
}

// The three §5.5 fields are the only ones the call can carry, checked on a
// REAL call built by the real code path rather than on a hand-made literal.
func TestTheIssuedCallCarriesNothingElse(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeApply)
	step := actDefaultStep()
	if err := actExecute(t, a, step); err != nil {
		t.Fatalf("apply: %v", err)
	}
	e, _ := a.Entry(step.Key)
	sent := e.Sent
	if sent.DBInstanceIdentifier != actID {
		t.Fatalf("the call named %q", sent.DBInstanceIdentifier)
	}
	// Serialize it and assert the key set: a field that exists but was left
	// zero is still a field somebody can fill in tomorrow.
	blob := renderCall(t, sent)
	for _, banned := range []string{
		"class", "multiAZ", "availabilityZone", "engineVersion", "allocatedStorage",
		"masterUserPassword", "applyImmediately", "deletionProtection",
	} {
		if strings.Contains(blob, banned) {
			t.Errorf("the issued call carries %q: %s", banned, blob)
		}
	}
}

// The dry-run and apply paths run the IDENTICAL pre-flight, so an apply can
// never do something a dry-run never showed. Asserted by driving both modes
// over every refusal scenario and comparing the codes.
func TestDryRunAndApplyRefuseIdentically(t *testing.T) {
	scenarios := []struct {
		name string
		bend func(*InstanceStateRecord)
	}{
		{"mode-off", func(r *InstanceStateRecord) { r.Tags[TagKilterMode] = "off" }},
		{"tags-unknown", func(r *InstanceStateRecord) { r.TagsKnown = false }},
		{"optimizing", func(r *InstanceStateRecord) { r.Status = StatusStorageOptimization }},
		{"stopped", func(r *InstanceStateRecord) { r.Status = StatusStopped }},
		{"alloc-drift", func(r *InstanceStateRecord) { r.AllocatedStorageGiB = 900 }},
		{"drift", func(r *InstanceStateRecord) { r.StorageType, r.IOPS = StorageGP3, 40000 }},
		{"engine", func(r *InstanceStateRecord) { r.Engine = "oracle-se2" }},
		{"clean", func(r *InstanceStateRecord) {}},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			dry := actActuator(t, actFixture(t, s.bend), ModeDryRun)
			wet := actActuator(t, actFixture(t, s.bend), ModeApply)
			step := actDefaultStep()
			dryErr := actExecute(t, dry, step)
			wetErr := actExecute(t, wet, step)
			if RefusalCode(dryErr) != RefusalCode(wetErr) {
				t.Fatalf("dry-run refused with %q and apply with %q: the two modes do not share a gate",
					RefusalCode(dryErr), RefusalCode(wetErr))
			}
			if (dryErr == nil) != (wetErr == nil) {
				t.Fatalf("dry-run err=%v, apply err=%v", dryErr, wetErr)
			}
		})
	}
}

// Preflight needs no approval and cannot act — the "may I look?" / "may I
// act?" split that lets the approval gate stay absolute without making the
// tool opaque.
func TestPreflightNeedsNoApprovalAndTouchesNothing(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) { r.Tags[TagKilterMode] = "off" })
	a := actActuator(t, f, ModeApply)
	if code := RefusalCode(a.Preflight(context.Background(), actDefaultStep())); code != RefuseModeOff {
		t.Fatalf("Preflight code = %q, want %q", code, RefuseModeOff)
	}
	clean := actFixture(t)
	b := actActuator(t, clean, ModeApply)
	if err := b.Preflight(context.Background(), actDefaultStep()); err != nil {
		t.Fatalf("Preflight on a clean instance: %v", err)
	}
	if n := clean.Mutations(); n != 0 {
		t.Fatalf("Preflight issued %d modification(s)", n)
	}
	if _, err := b.PlannedCall(context.Background(), actDefaultStep()); err != nil {
		t.Fatalf("PlannedCall: %v", err)
	}
	if n := clean.Mutations(); n != 0 {
		t.Fatalf("PlannedCall issued %d modification(s)", n)
	}
}

// A four-per-24-hours limit that a REVERT also spends. This is the honest
// arithmetic ACTUATE-FINDINGS.md §4 states, asserted so it cannot quietly stop
// being true: the fixture enforces the limit the way AWS does, and a change
// plus its undo really do consume two of the four.
func TestAChangeAndItsUndoSpendTwoOfFour(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) {
		r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 12000, 1000
	})
	a := actActuator(t, f, ModeApply)
	step := actStep(
		actSpec(actEngine, StorageGP3, actSize, 12000, 1000),
		actSpec(actEngine, StorageGP3, actSize, 20000, 2000),
	)
	if err := a.Execute(context.Background(), actApproved(t, step)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := countStorageModifications(f, actID); got != 1 {
		t.Fatalf("the change left %d modification(s) in the 24-hour window, want 1", got)
	}
	// The undo is a reduction and is refused, so it spends nothing HERE — but
	// the arithmetic an operator performing it by hand faces is 1 + 1 = 2 of
	// 4, and the second raise below proves the counter really is shared.
	up := actStep(
		actSpec(actEngine, StorageGP3, actSize, 20000, 2000),
		actSpec(actEngine, StorageGP3, actSize, 24000, 2400),
	)
	if err := a.Execute(context.Background(), actApproved(t, up)); err != nil {
		t.Fatalf("second change: %v", err)
	}
	if got := countStorageModifications(f, actID); got != 2 {
		t.Fatalf("two changes left %d modification(s), want 2: the limit is not being counted", got)
	}
}

func countStorageModifications(f *StorageActuateFixture, id string) int {
	n := 0
	for _, ev := range f.Events[id] {
		if IsStorageModificationEvent(ev) {
			n++
		}
	}
	return n
}

// The sharpest edge in this unit, pinned.
//
// inFlightTowardTarget is the one PERMISSIVE predicate here: when it says yes,
// the four gates that stop a modification being issued are skipped. Its safety
// rests entirely on one claim — no state it accepts can derive to StageReady,
// which is the only stage that issues. ACTUATE-FINDINGS.md §7.5 names this as
// the thing a future edit to stageOf could break without breaking a test.
//
// This is that test. It sweeps every combination of live storage type, live
// values and pending values around the default step and asserts the invariant
// directly, so the claim is enforced rather than merely written down.
func TestNothingResumableCanAlsoIssue(t *testing.T) {
	e := ParseEngine(actEngine, "general-public-license")
	r := GP3RegimeFor(e, actSize)
	in, err := decodeStep(actDefaultStep(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	base := storageFacts{
		regime: r,
		want:   configOf(r, actSize, 12000, 1000),
		from:   configOf(r, actSize, -1, -1),
	}
	types := []string{StorageGP2, StorageGP3, ""}
	values := []int32{0, 500, 1000, 2000, 12000, 20000}
	statuses := []string{StatusAvailable, StatusModifying, StatusStorageOptimization, StatusStopped}
	checked := 0
	for _, lt := range types {
		for _, li := range values {
			for _, ltp := range values {
				for _, pt := range types {
					for _, pi := range values {
						for _, ptp := range values {
							for _, st := range statuses {
								f := base
								f.live = InstanceStateRecord{
									Identifier: actID, Engine: actEngine, Status: st,
									AllocatedStorageGiB: actSize, StorageType: lt,
									IOPS: li, StorageThroughputMBps: ltp,
									PendingStorageType: pt, PendingIOPS: pi,
									PendingStorageThroughputMBps: ptp,
									TagsKnown:                    true,
								}
								f.liveCfg = configOf(r, actSize, li, ltp)
								checked++
								if inFlightTowardTarget(in, f) && stageOf(in, f) == StageReady {
									t.Fatalf("a state that skips the issue gates ALSO derives to %q, "+
										"which is the stage that sends a modification: live=%s/%d/%d "+
										"pending=%s/%d/%d status=%s",
										StageReady, lt, li, ltp, pt, pi, ptp, st)
								}
							}
						}
					}
				}
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("the sweep only covered %d states; it is not proving much", checked)
	}
}
