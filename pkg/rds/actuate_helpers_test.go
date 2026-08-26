package rds

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// The scenario every test in this unit starts from: a 500 GiB MySQL instance,
// which is ABOVE the 400 GiB striping threshold and therefore in the striped
// gp3 regime — 12,000 IOPS / 500 MiB/s free, provisioning permitted. That is
// the only regime where this actuator can do anything at all, so it is the one
// worth making the default.
const (
	actID     = "prod-orders"
	actScope  = "123456789012/us-east-1"
	actSize   = int64(500)
	actEngine = "mysql"
)

func actNow() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }

func actClock() func() time.Time { return actNow }

// actGP3Envelope is the envelope a striped 500 GiB MySQL instance reports.
// Every ceiling in it comes from here rather than from a constant in the
// package, which is TestProvisioningEnvelopeIsReadNeverHardcoded's whole point.
func actGP3Envelope() ValidStorageOptionRecord {
	return ValidStorageOptionRecord{
		StorageType: StorageGP3, MinIOPS: 12000, MaxIOPS: 64000,
		MinStorageThroughputMBps: 500, MaxStorageThroughputMBps: 4000,
		MinAllocatedStorageGiB: 20, MaxAllocatedStorageGiB: 65536,
	}
}

// actLive builds the live record for the default scenario: gp2, available,
// tagged, nothing pending.
func actLive(opts ...func(*InstanceStateRecord)) InstanceStateRecord {
	r := InstanceStateRecord{
		Identifier: actID, ARN: "arn:aws:rds:us-east-1:123456789012:db:" + actID,
		Engine: actEngine, LicenseModel: "general-public-license", Status: StatusAvailable,
		AllocatedStorageGiB: actSize, StorageType: StorageGP2,
		Tags: map[string]string{"env": "prod"}, TagsKnown: true,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

// actFixture wires the default scenario: one instance, a known envelope, and
// an empty (but KNOWN) modification history.
func actFixture(t *testing.T, opts ...func(*InstanceStateRecord)) *StorageActuateFixture {
	t.Helper()
	f := NewStorageActuateFixture(actClock(), actLive(opts...))
	f.WithEnvelope(actID, actGP3Envelope())
	f.WithEvents(actID)
	return f
}

func actRef() domain.TargetRef {
	return domain.TargetRef{Domain: Kind, Scope: actScope, ID: actID}
}

// actSpec builds one side of a step.
func actSpec(engine, storageType string, sizeGiB int64, iops, tput int32) domain.Spec {
	s := domain.Spec{Attrs: map[string]string{
		AttrEngine:              engine,
		AttrLicenseModel:        "general-public-license",
		AttrStorageType:         storageType,
		AttrAllocatedStorageGiB: strconv.FormatInt(sizeGiB, 10),
	}}
	if iops >= 0 {
		s.Attrs[AttrIOPS] = strconv.FormatInt(int64(iops), 10)
	}
	if tput >= 0 {
		s.Attrs[AttrStorageThroughput] = strconv.FormatInt(int64(tput), 10)
	}
	return s
}

// actStep is the default step: convert the 500 GiB gp2 volume to gp3 at
// 12,000 IOPS / 1,000 MiB/s.
//
// Read the numbers: 12,000 IOPS is EXACTLY the striped regime's free baseline,
// so the call must NOT send --iops. 1,000 MiB/s is above the 500 MiB/s
// baseline, so it must. That asymmetry inside one step is FINDINGS.md §5.1 in
// its smallest form, and TestActuateSendsOnlyProvisionedArguments turns on it.
func actStep(from, to domain.Spec) domain.Step {
	s := domain.Step{
		Seq: 1, Target: actRef(), Action: domain.ActionInPlace,
		From: from, To: to, Risk: RiskLow,
		Detail: "gp2 → gp3 at measured parity",
	}
	s.Key = domain.StepKey(s.Target, s.From, s.To)
	return s
}

func actDefaultStep() domain.Step {
	return actStep(
		actSpec(actEngine, StorageGP2, actSize, -1, -1),
		actSpec(actEngine, StorageGP3, actSize, 12000, 1000),
	)
}

// actApproval builds a real approval over the given steps. There is no way to
// obtain one that skips a check, which is the point.
func actApproval(t *testing.T, steps ...domain.Step) Approval {
	t.Helper()
	tok := ApprovalToken{
		Fingerprint: PlanFingerprint(steps), Scope: actScope, ApprovedBy: "alan",
		ApprovedAt: actNow().Add(-time.Minute), ExpiresAt: actNow().Add(time.Hour),
	}
	ap, err := NewApproval(steps, tok, actNow())
	if err != nil {
		t.Fatalf("NewApproval: %v", err)
	}
	return ap
}

func actApproved(t *testing.T, step domain.Step) ApprovedStep {
	t.Helper()
	as, err := actApproval(t, step).Authorize(step)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return as
}

// actActuator builds an actuator whose Sleep does not spend real time.
func actActuator(t *testing.T, f *StorageActuateFixture, mode Mode) *Actuator {
	t.Helper()
	a, err := NewActuator(f, ActuatorConfig{
		Mode: mode, Now: actClock(),
		PollInterval: time.Second, PollTimeout: time.Minute,
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	})
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	return a
}

// actExecute runs one step and returns the error, whatever it is.
func actExecute(t *testing.T, a *Actuator, step domain.Step) error {
	t.Helper()
	return a.Execute(context.Background(), actApproved(t, step))
}

// wantRefusal asserts that err is a refusal carrying exactly the given code.
// Every refusal test in this unit ends in this call, so a refusal that changes
// its code silently is impossible.
func wantRefusal(t *testing.T, err error, code string) *RefusalError {
	t.Helper()
	if err == nil {
		t.Fatalf("want refusal %q, got nil: THE ACTUATOR WOULD HAVE MODIFIED A DATABASE", code)
	}
	if !IsRefusal(err) {
		t.Fatalf("want refusal %q, got a non-refusal error: %v", code, err)
	}
	if got := RefusalCode(err); got != code {
		t.Fatalf("refusal code = %q, want %q (%v)", got, code, err)
	}
	var r *RefusalError
	if !asRefusal(err, &r) {
		t.Fatalf("refusal %v is not a *RefusalError", err)
	}
	if r.Reason == "" {
		t.Errorf("refusal %q carries no reason; a refusal a human cannot act on is a bug", code)
	}
	return r
}

func asRefusal(err error, out **RefusalError) bool {
	for e := err; e != nil; {
		if r, ok := e.(*RefusalError); ok {
			*out = r
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// interfaceMethodNames returns an interface's method names in sorted order,
// for the "this seam has exactly these operations" assertions.
func interfaceMethodNames(t *testing.T, ptr any) []string {
	t.Helper()
	rt := reflect.TypeOf(ptr)
	if rt == nil || rt.Kind() != reflect.Pointer || rt.Elem().Kind() != reflect.Interface {
		t.Fatalf("interfaceMethodNames: %T is not a pointer to an interface", ptr)
	}
	it := rt.Elem()
	out := make([]string, 0, it.NumMethod())
	for i := range it.NumMethod() {
		out = append(out, it.Method(i).Name)
	}
	sortStrings(out)
	return out
}

// renderCall serializes an issued call so a test can assert on the whole of
// what would go over the wire rather than on the fields it remembered to look
// at.
func renderCall(t *testing.T, in ModifyStorageInput) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
