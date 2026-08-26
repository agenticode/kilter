package lambda

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	klambda "github.com/agenticode/kilter/pkg/lambda"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

const fnARN = "arn:aws:lambda:us-east-1:000000000000:function:thumbnailer"

// nativeSnapshot builds a Lambda snapshot carrying REPORT records at two
// memory settings — the only evidence shape that can support a cost claim.
func nativeSnapshot(t *testing.T) *klambda.Snapshot {
	t.Helper()
	win := klambda.Window{Start: testNow.Add(-14 * 24 * time.Hour), End: testNow}
	tgt := klambda.Target{
		Ref: domain.TargetRef{Domain: Kind, Scope: "000000000000/us-east-1",
			ID: fnARN, Name: "thumbnailer"},
		Function: klambda.Function{
			ARN: fnARN, Name: "thumbnailer", MemoryMB: 1024,
			Architecture: klambda.ArchX86, PackageType: klambda.PackageZip,
		},
	}
	tgt.Reports = append(tgt.Reports,
		parse(t, "hi", testNow.Add(-7*24*time.Hour), 7*24*time.Hour, 600, 1024, 300, 42)...)
	tgt.Reports = append(tgt.Reports,
		parse(t, "lo", win.Start, 7*24*time.Hour, 600, 512, 300, 68)...)
	// The Invocations metric is sized to the REPORT lines that survived
	// parsing: their ratio IS the report-coverage confidence factor, and it
	// must clear Config.MinInvocations (1000) or the fixture fails on evidence
	// volume rather than on the arithmetic under test.
	tgt.Series = []klambda.Series{{
		Metric: klambda.MetricInvocations,
		Points: klambda.SyntheticMetric(win.Start, time.Hour, 336, 4.0),
	}}
	return &klambda.Snapshot{
		Domain: Kind, Scope: "000000000000/us-east-1", Region: "us-east-1",
		Timestamp: testNow, Window: win, Targets: []klambda.Target{tgt},
	}
}

func parse(t *testing.T, prefix string, start time.Time, span time.Duration,
	n int, memoryMB, maxUsedMB int64, billedMS float64) []klambda.ReportRecord {
	t.Helper()
	recs, drops := klambda.ParseEvents(
		klambda.SyntheticReports(prefix, start, span, n, memoryMB, maxUsedMB, billedMS, 0, 0))
	if len(drops) > 0 || len(recs) != n {
		t.Fatalf("synthetic REPORT lines did not parse: %d records, %v", len(recs), drops)
	}
	return recs
}

// TestPayloadPreservesREPORTEvidence is the fix for pkg/lambda/FINDINGS.md §8.
//
// The same snapshot goes in two ways: through the opaque Payload (which the
// adapter routes to the native path) and through the generic seam (which
// cannot carry a REPORT record). The first can make a measured cost claim; the
// second honestly refuses. Proving BOTH is the point — the lossy path is not a
// bug to be hidden, it is the correct answer to lossy input.
func TestPayloadPreservesREPORTEvidence(t *testing.T) {
	native := nativeSnapshot(t)
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}

	viaPayload, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := viaPayload.Learn(&domain.Snapshot{
		Domain: Kind, Scope: native.Scope, Timestamp: testNow, Payload: raw,
	}); err != nil {
		t.Fatal(err)
	}
	recs := viaPayload.Recommend(testNow, nil)
	if len(recs) != 1 {
		t.Fatalf("the payload path produced %d recommendations, want 1 (refusals: %+v)",
			len(recs), viaPayload.Refusals(testNow, nil))
	}
	if recs[0].Suppressed {
		t.Errorf("a measured two-point comparison was suppressed: %s", recs[0].Reason)
	}
	if len(viaPayload.Refusals(testNow, nil)) != 0 {
		t.Errorf("the payload path refused: %v", viaPayload.Refusals(testNow, nil))
	}

	viaGeneric, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := viaGeneric.Learn(native.Generic()); err != nil {
		t.Fatal(err)
	}
	refs := viaGeneric.Refusals(testNow, nil)
	if len(refs) != 1 {
		t.Fatalf("the generic path produced %d refusals, want 1", len(refs))
	}
	if refs[0].Code != klambda.ReasonNoReportEvidence {
		t.Errorf("the generic path refused as %q, want %q",
			refs[0].Code, klambda.ReasonNoReportEvidence)
	}
	if refs[0].Reason == "" {
		t.Error("the refusal carries no prose")
	}
}

// TestDomainRemainsAdvisoryForever. There is no Lambda actuator anywhere in
// the codebase, and this adapter must not become the place one appears.
func TestDomainRemainsAdvisoryForever(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(d).(domain.Actuator); ok {
		t.Fatal("the Lambda domain satisfies domain.Actuator")
	}
	for _, g := range []domain.Guard{{Now: testNow}, {}} {
		if _, err := d.PlanSteps(nil, g); !errors.Is(err, domain.ErrReportOnly) {
			t.Fatalf("PlanSteps = %v, want ErrReportOnly", err)
		}
	}
	if !d.Health(testNow).ReportOnly {
		t.Fatal("the Lambda domain does not report itself report-only")
	}
	// And through the core, which refuses first.
	r := domain.NewRegistry()
	if err := r.Register(d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PlanSteps(Kind, nil, domain.Guard{Now: testNow}); !errors.Is(err, domain.ErrReportOnly) {
		t.Fatalf("registry PlanSteps = %v, want ErrReportOnly", err)
	}
	// Every recommendation it can make is advisory.
	native := nativeSnapshot(t)
	if err := d.Observe(native); err != nil {
		t.Fatal(err)
	}
	for _, rec := range d.Recommend(testNow, nil) {
		if rec.Action != domain.ActionAdvisory {
			t.Errorf("%s has action %q, want advisory", rec.Target, rec.Action)
		}
	}
}

// TestLearnInputHandling.
func TestLearnInputHandling(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(nil); err != nil {
		t.Errorf("Learn(nil) = %v", err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: domain.EC2}); !errors.Is(err, domain.ErrWrongDomain) {
		t.Errorf("Learn(foreign) = %v, want ErrWrongDomain", err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Payload: []byte("]")}); err == nil {
		t.Error("a malformed payload was accepted as an empty account")
	}
	// A payload naming another domain is ignored, not decoded into ours.
	other, _ := json.Marshal(map[string]any{"domain": "ec2"})
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Payload: other}); err != nil {
		t.Errorf("Learn(foreign payload) = %v", err)
	}
	if d.Health(testNow).Targets != 0 {
		t.Error("a foreign payload was ingested")
	}
}

// TestNativeEvidenceSurvivesALaterGenericSnapshot: the generic path must never
// erase what the native path delivered.
func TestNativeEvidenceSurvivesALaterGenericSnapshot(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	native := nativeSnapshot(t)
	if err := d.Observe(native); err != nil {
		t.Fatal(err)
	}
	before := len(d.Recommend(testNow, nil))
	if before == 0 {
		t.Fatal("the native path produced nothing; the fixture proves nothing")
	}
	if err := d.Learn(native.Generic()); err != nil {
		t.Fatal(err)
	}
	if after := len(d.Recommend(testNow, nil)); after != before {
		t.Fatalf("recommendations went from %d to %d after a generic snapshot: "+
			"the lossy path erased REPORT evidence", before, after)
	}
}
