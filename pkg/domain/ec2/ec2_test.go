package ec2

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	kec2 "github.com/agenticode/kilter/pkg/ec2"
	"github.com/agenticode/kilter/pkg/pricing"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{Scope: "000000000000/us-east-1", Region: "us-east-1", Catalog: pricing.Embedded()}
}

// instanceSnapshot builds a minimal pkg/ec2 snapshot wrapped in the generic
// envelope's Payload.
func instanceSnapshot(t *testing.T, ids ...string) *domain.Snapshot {
	t.Helper()
	native := kec2.Snapshot{
		Domain: kec2.Domain, Scope: "000000000000/us-east-1", Region: "us-east-1",
		Timestamp: testNow,
		Window:    kec2.Window{Start: testNow.Add(-14 * 24 * time.Hour), End: testNow},
	}
	for _, id := range ids {
		native.Targets = append(native.Targets, kec2.Target{
			Ref: kec2.TargetRef{Domain: kec2.Domain, Scope: native.Scope, ID: id},
			Instance: kec2.Instance{
				ID: id, InstanceType: "m5.2xlarge", Architecture: "amd64",
				State: "running", AvailabilityZone: "us-east-1a", Tenancy: "default",
			},
		})
	}
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	return &domain.Snapshot{Domain: Kind, Scope: native.Scope, Timestamp: testNow, Payload: raw}
}

// TestCompositeRegistersUnderTheOneECKind is the Kind-collision resolution:
// both halves report `ec2`, and the composite is what a single-domain-per-kind
// registry can hold.
func TestCompositeRegistersUnderTheOneECKind(t *testing.T) {
	c, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind() != domain.EC2 {
		t.Fatalf("Kind = %q", c.Kind())
	}
	names := c.PartNames()
	if len(names) != 2 || names[0] != PartInstances || names[1] != PartVolumes {
		t.Fatalf("PartNames = %v", names)
	}
	for _, p := range c.Parts() {
		if p.Domain.Kind() != domain.EC2 {
			t.Errorf("part %q reports kind %q", p.Name, p.Domain.Kind())
		}
	}
	r := domain.NewRegistry()
	if err := r.Register(c); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Fatalf("registry holds %d domains", r.Len())
	}
}

// TestRoutingSeparatesVolumesFromInstances: the predicate the composite routes
// on must send volumes to pkg/ebs and everything else to pkg/ec2, and the two
// ID spaces must not overlap.
func TestRoutingSeparatesVolumesFromInstances(t *testing.T) {
	vol := domain.Recommendation{
		Target:  domain.TargetRef{Domain: domain.EC2, ID: "vol-0123"},
		Current: domain.Spec{Attrs: map[string]string{"volumeType": "gp2"}},
	}
	inst := domain.Recommendation{
		Target:  domain.TargetRef{Domain: domain.EC2, ID: "i-0123"},
		Current: domain.Spec{Attrs: map[string]string{"instanceType": "m5.large"}},
	}
	// A volume whose attributes were lost in a round trip still routes by ID.
	bareVol := domain.Recommendation{Target: domain.TargetRef{Domain: domain.EC2, ID: "vol-9999"}}

	for _, tc := range []struct {
		name     string
		rec      domain.Recommendation
		isVolume bool
	}{
		{"volume by attribute", vol, true},
		{"volume by id prefix", bareVol, true},
		{"instance", inst, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ownsVolume(tc.rec) != tc.isVolume {
				t.Errorf("ownsVolume = %v, want %v", ownsVolume(tc.rec), tc.isVolume)
			}
			if ownsInstance(tc.rec) == tc.isVolume {
				t.Error("ownsInstance and ownsVolume both claimed, or both declined, the same target")
			}
		})
	}
}

// TestVolumeSnapshotPredicate pins the ordering fix: a snapshot whose only
// content is the instance half's payload is not addressed to the volume half,
// but an empty account still is.
func TestVolumeSnapshotPredicate(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap *domain.Snapshot
		want bool
	}{
		{"nil", nil, false},
		{"payload only", &domain.Snapshot{Payload: []byte(`{}`)}, false},
		{"payload plus targets", &domain.Snapshot{
			Payload: []byte(`{}`),
			Targets: []domain.Target{{Ref: domain.TargetRef{ID: "vol-1"}}}}, true},
		{"targets only", &domain.Snapshot{
			Targets: []domain.Target{{Ref: domain.TargetRef{ID: "vol-1"}}}}, true},
		{"empty account", &domain.Snapshot{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeSnapshot(tc.snap); got != tc.want {
				t.Errorf("volumeSnapshot = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInstanceHalfIsStructurallyReportOnly. pkg/ec2 has no actuation surface
// at all — U7 was never built — so this is not a stub that a config flag could
// flip.
func TestInstanceHalfIsStructurallyReportOnly(t *testing.T) {
	d, err := NewInstances(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(instanceSnapshot(t, "i-1")); err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if !h.ReportOnly {
		t.Fatal("the instance half claims it can act")
	}
	if !h.Ready {
		t.Fatalf("the instance half is not ready after learning: %s", h.Reason)
	}
	for _, g := range []domain.Guard{
		{Now: testNow},
		{Now: testNow, MaxSteps: 100},
		{}, // zero guard
	} {
		steps, err := d.PlanSteps(nil, g)
		if !errors.Is(err, domain.ErrReportOnly) {
			t.Fatalf("PlanSteps = (%v, %v), want ErrReportOnly", steps, err)
		}
	}
	// And there is no actuator to register: *Instances must not satisfy the
	// interface, so no wiring can accidentally hand it credentials.
	if _, ok := any(d).(domain.Actuator); ok {
		t.Fatal("the instance half satisfies domain.Actuator")
	}
	// The core refuses too, before the domain is even asked.
	c, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := domain.NewRegistry()
	if err := r.Register(c); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PlanSteps(domain.EC2, nil, domain.Guard{Now: testNow}); !errors.Is(err, domain.ErrReportOnly) {
		t.Fatalf("registry PlanSteps = %v, want ErrReportOnly", err)
	}
}

// TestNoCatalogIsReportedNotPricedAtZero. An instance the catalog cannot price
// is already a refusal inside pkg/ec2; a whole FLEET nobody can price is a
// wiring mistake, and it says so instead of reporting $0 everywhere.
func TestNoCatalogIsReportedNotPricedAtZero(t *testing.T) {
	cfg := testConfig()
	cfg.Catalog = nil
	d, err := NewInstances(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(instanceSnapshot(t, "i-1")); err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if h.Ready {
		t.Error("a domain with no pricing catalog reports itself ready")
	}
	if !strings.Contains(h.Reason, "catalog") {
		t.Errorf("health reason does not name the missing catalog: %q", h.Reason)
	}
	if got := d.Recommend(testNow, nil); got != nil {
		t.Errorf("Recommend returned %v with no catalog", got)
	}
	if got := d.UsageLines(testNow, nil); got != nil {
		t.Errorf("UsageLines returned %v with no catalog", got)
	}
	// The composite still builds: a volumes-only wiring is legal.
	if _, err := New(cfg); err != nil {
		t.Fatalf("New with no catalog: %v", err)
	}
}

// TestLearnInputHandling: an unfed domain is report-only, a payload-less
// snapshot is ignored without disturbing what was already learned, a foreign
// snapshot is an error, and a malformed payload is an error rather than an
// empty account.
func TestLearnInputHandling(t *testing.T) {
	d, err := NewInstances(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Health(testNow); h.Ready || !strings.Contains(h.Reason, "no snapshot") {
		t.Errorf("an unfed domain reports %+v", h)
	}
	if err := d.Learn(nil); err != nil {
		t.Errorf("Learn(nil) = %v", err)
	}
	if err := d.Learn(instanceSnapshot(t, "i-1", "i-2")); err != nil {
		t.Fatal(err)
	}
	if got := d.Health(testNow).Targets; got != 2 {
		t.Fatalf("Targets = %d, want 2", got)
	}

	// A shared ec2-kind snapshot carrying only volume targets must not make
	// this half forget its instances — the mirror image of pkg/ebs's own rule.
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Timestamp: testNow,
		Targets: []domain.Target{{Ref: domain.TargetRef{Domain: Kind, ID: "vol-1"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := d.Health(testNow).Targets; got != 2 {
		t.Fatalf("Targets = %d after a volume snapshot; the half forgot its instances", got)
	}

	if err := d.Learn(&domain.Snapshot{Domain: domain.Lambda}); !errors.Is(err, domain.ErrWrongDomain) {
		t.Errorf("Learn(foreign) = %v, want ErrWrongDomain", err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Payload: []byte("not json")}); err == nil {
		t.Error("a malformed payload was accepted as an empty account")
	}
}

// TestStaleSnapshotDegradesTheHalf.
func TestStaleSnapshotDegradesTheHalf(t *testing.T) {
	d, err := NewInstances(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	old := instanceSnapshot(t, "i-1")
	old.Timestamp = testNow.Add(-72 * time.Hour)
	var native kec2.Snapshot
	if err := json.Unmarshal(old.Payload, &native); err != nil {
		t.Fatal(err)
	}
	native.Timestamp = old.Timestamp
	raw, _ := json.Marshal(native)
	old.Payload = raw

	if err := d.Learn(old); err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if h.Ready {
		t.Error("a three-day-old snapshot still reads as ready")
	}
	if !strings.Contains(h.Reason, "old") {
		t.Errorf("health reason = %q", h.Reason)
	}
}

// TestUsageLinesFeedTheAccountWideBaseline. Without them, every domain's net
// saving would be computed against a baseline containing only its own
// targets, which overstates the saving (§4.4 ex.3).
func TestUsageLinesFeedTheAccountWideBaseline(t *testing.T) {
	d, err := NewInstances(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(instanceSnapshot(t, "i-b", "i-a")); err != nil {
		t.Fatal(err)
	}
	lines := d.UsageLines(testNow, nil)
	if len(lines) != 2 {
		t.Fatalf("UsageLines = %v, want 2", lines)
	}
	if lines[0].ID != "i-a" || lines[1].ID != "i-b" {
		t.Errorf("UsageLines is not sorted by ID: %v", lines)
	}
	for _, l := range lines {
		if l.ODRate <= 0 {
			t.Errorf("%s has no on-demand rate; an unpriced line must be omitted, not zeroed", l.ID)
		}
		if l.InstanceType == "" || l.Quantity != 1 {
			t.Errorf("line = %+v", l)
		}
	}
}

// TestCheckpointRoundTripIsDeterministic.
func TestCheckpointRoundTripIsDeterministic(t *testing.T) {
	c, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Learn(instanceSnapshot(t, "i-1")); err != nil {
		t.Fatal(err)
	}
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
	restored, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(first); err != nil {
		t.Fatal(err)
	}
	after, err := restored.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(first) {
		t.Fatal("restore-then-checkpoint is not identical to the original")
	}
	if got := restored.Health(testNow).Targets; got != 1 {
		t.Errorf("restored domain tracks %d targets, want 1", got)
	}
}

// TestBothHalvesExplainTheirRefusals: [domain.Refuser] is implemented on both
// sides, so a target that produced no recommendation is still accounted for.
func TestBothHalvesExplainTheirRefusals(t *testing.T) {
	inst, err := NewInstances(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	vols, err := NewVolumes(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(inst).(domain.Refuser); !ok {
		t.Error("the instance half cannot explain its refusals")
	}
	if _, ok := any(vols).(domain.Refuser); !ok {
		t.Error("the volume half cannot explain its refusals")
	}

	// Two instances with no metrics at all: pkg/ec2 refuses both, and every
	// refusal carries a code and prose.
	if err := inst.Learn(instanceSnapshot(t, "i-1", "i-2")); err != nil {
		t.Fatal(err)
	}
	refs := inst.Refusals(testNow, nil)
	if len(refs) != 2 {
		t.Fatalf("Refusals = %v, want one per instance", refs)
	}
	for i, r := range refs {
		if r.Code == "" || r.Reason == "" {
			t.Errorf("refusal %d has no code or reason: %+v", i, r)
		}
		if r.Target.Domain != domain.EC2 {
			t.Errorf("refusal %d is attributed to %q", i, r.Target.Domain)
		}
	}
	if refs[0].Target.ID > refs[1].Target.ID {
		t.Error("refusals are not canonically ordered")
	}
}
