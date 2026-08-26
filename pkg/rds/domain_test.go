package rds

import (
	"strings"
	"testing"
)

// The seam claim of this unit, asserted end to end: Recommend returns nothing
// and Refusals returns everything.
//
// A caller that rendered only domain.Recommendations would show a user an
// empty RDS section and let them conclude the tool found nothing — a different
// claim from "the tool declined to guess, here is what it needs". That is
// exactly the case pkg/domain/refusal.go exists for, so this domain routes its
// entire output through it.
func TestRecommendIsEmptyAndRefusalsAreTheOutput(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Observe(collect(t, mixedFixture())); err != nil {
		t.Fatal(err)
	}

	recs := d.Recommend(testNow, nil)
	if len(recs) != 0 {
		t.Fatalf("Recommend returned %d recommendations; this domain proposes nothing", len(recs))
	}
	if recs == nil {
		t.Error("Recommend returned nil rather than an empty slice; a caller ranging over it should see " +
			"\"asked and answered\", not \"never ran\"")
	}

	refs := d.Refusals(testNow, nil)
	if len(refs) < len(d.Report(testNow, nil).Assessments) {
		t.Fatalf("Refusals returned %d entries for %d instances; every instance states at least one reason",
			len(refs), len(d.Report(testNow, nil).Assessments))
	}
	for i, r := range refs {
		if r.Code == "" || r.Reason == "" || r.Target.ID == "" {
			t.Errorf("refusal %d is incomplete: %+v", i, r)
		}
		if r.Target.Domain != Kind {
			t.Errorf("refusal %d carries domain %q", i, r.Target.Domain)
		}
		if i > 0 {
			prev := refs[i-1]
			if c := prev.Target.Compare(r.Target); c > 0 {
				t.Errorf("refusals are not sorted: %s after %s", r.Target, prev.Target)
			}
		}
	}

	// Every distinct refusal code the report produced survives the projection.
	rep := d.Report(testNow, nil)
	got := map[string]bool{}
	for _, r := range refs {
		got[r.Code] = true
	}
	for code := range rep.Totals.RefusedByCode {
		if !got[code] {
			t.Errorf("refusal code %q is in the report totals but not in the projected refusals", code)
		}
	}
}

// Health degrades on a stale snapshot and says which condition it is in, but
// never stops being report-only.
func TestHealthDegradesButStaysReportOnly(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if h := d.Health(testNow); h.Ready || !h.ReportOnly ||
		!strings.Contains(h.Reason, "no RDS DB instances") {
		t.Fatalf("empty-domain health = %+v", h)
	}

	snap := collect(t, mixedFixture())
	if err := d.Observe(snap); err != nil {
		t.Fatal(err)
	}
	if h := d.Health(testNow); !h.Ready || !h.ReportOnly || h.Targets != len(snap.Targets) {
		t.Fatalf("loaded-domain health = %+v", h)
	}

	stale := *snap
	stale.Stale = true
	if err := d.Observe(&stale); err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if h.Ready {
		t.Error("a stale snapshot left the domain ready")
	}
	if !h.ReportOnly {
		t.Error("a stale domain stopped being report-only")
	}
	if !strings.Contains(h.Reason, "incomplete") {
		t.Errorf("a stale domain does not say why: %s", h.Reason)
	}
}

// The rendered report leads with what it refuses, not with a dollar figure.
//
// This is the opposite order from every other domain's report, and it is
// deliberate: in RDS the refusal IS the finding, and a reader who sees money
// first reads the refusal as a caveat on a recommendation that does not exist.
func TestWriteTextLeadsWithTheRefusal(t *testing.T) {
	rep := assess(t, collect(t, mixedFixture()), nil)
	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	proposesNothing := strings.Index(out, "THIS DOMAIN PROPOSES NOTHING")
	spend := strings.Index(out, "observed instance spend")
	if proposesNothing < 0 {
		t.Fatal("the report does not say that it proposes nothing")
	}
	if spend < 0 {
		t.Fatal("the report does not state the observed spend")
	}
	if proposesNothing > spend {
		t.Error("the report leads with a dollar figure; the refusal is the finding here")
	}
	for _, want := range []string{
		"failovers", "allocated storage cannot shrink", "FreeableMemory is engine-dependent",
		"UNVERIFIED", "unrecoverable by any API",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report omits %q:\n%s", want, out)
		}
	}
	// A reader can see the refusal roll-up without walking every instance.
	if !strings.Contains(out, "refused [") {
		t.Error("the report has no \"what we declined to do, and why\" roll-up")
	}
}

// Report.For is a binary search over a sorted slice; a mis-sorted slice would
// silently return the wrong assessment.
func TestReportForFindsEveryAssessment(t *testing.T) {
	rep := assess(t, collect(t, mixedFixture()), nil)
	for _, a := range rep.Assessments {
		got, ok := rep.For(a.Target.ID)
		if !ok {
			t.Fatalf("For(%q) found nothing in a report that contains it", a.Target.ID)
		}
		if got.Target.ID != a.Target.ID {
			t.Fatalf("For(%q) returned %q", a.Target.ID, got.Target.ID)
		}
	}
	if _, ok := rep.For("arn:aws:rds:us-east-1:1234:db:nonesuch"); ok {
		t.Error("For invented an assessment")
	}
	if _, ok := rep.For(""); ok {
		t.Error("For(\"\") matched something")
	}
}
