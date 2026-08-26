package rds

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// The invariants below are properties of the SOURCE, not of any single
// function, so they are asserted against the source. A reviewer should not
// have to grep to be sure this package cannot actuate, cannot dial out and
// cannot read the clock — and neither should the next person to edit it.
//
// This is the pkg/lambda pattern, kept deliberately identical so a reviewer
// who has read one has read both.

func packageFiles(t *testing.T) (map[string]*ast.File, map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	src := map[string]string{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, b, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files[name], src[name] = f, string(b)
	}
	if len(files) == 0 {
		t.Fatal("no package source found")
	}
	return files, src
}

// Air-gapped: standard library plus this repository, and nothing else. A new
// module dependency here would break the single-static-binary promise, and an
// AWS SDK import would put a network client in the decision path.
func TestNoForeignImports(t *testing.T) {
	files, _ := packageFiles(t)
	for name, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s", name, imp.Path.Value)
			}
			if strings.HasPrefix(path, "github.com/agenticode/kilter/") {
				continue
			}
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q: this package is stdlib + intra-repo only", name, path)
			}
			for _, banned := range []string{"net/http", "net/rpc", "os/exec", "net"} {
				if path == banned {
					t.Errorf("%s imports %q: nothing here does I/O to the outside world", name, banned)
				}
			}
		}
	}
}

// No clock. Callers pass `now`; a package that reads time.Now cannot be
// replayed, checkpointed or compared against yesterday's report.
func TestNoClockReads(t *testing.T) {
	_, src := packageFiles(t)
	for name, body := range src {
		for _, banned := range []string{"time.Now(", "time.Since(", "time.Until("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s calls %s: this package reads no clock", name, banned)
			}
		}
	}
}

// Advisory only. None of the mutating RDS APIs may be named anywhere in the
// package's CODE — not in a seam, not in a constant, not behind a flag.
//
// This is the acceptance test the design doc calls TestNoActuationSurfaceExists,
// split in two: the identifier scan here, and the interface check in
// TestNoActuationSurfaceExists below.
func TestNoMutatingAPISurface(t *testing.T) {
	_, src := packageFiles(t)
	mutating := []string{
		"ModifyDBInstance", "ModifyDBCluster", "DeleteDBInstance", "DeleteDBCluster",
		"RebootDBInstance", "StopDBInstance", "StartDBInstance", "FailoverDBCluster",
		"PromoteReadReplica", "CreateDBInstance", "RestoreDBInstance", "AddTagsToResource",
		"RemoveTagsFromResource", "ModifyDBParameterGroup", "PurchaseReservedDBInstancesOffering",
		"CreateBlueGreenDeployment", "SwitchoverBlueGreenDeployment",
	}
	for name, body := range src {
		// Prose — a doc comment or the text of a refusal — is allowed to NAME
		// the API this package will not call; that naming is the point. What
		// must not exist is an IDENTIFIER: a seam method, a function, a field.
		code := identifiers(t, name, body)
		for _, m := range mutating {
			if strings.Contains(code, m) {
				t.Errorf("%s references the mutating API %q outside a comment", name, m)
			}
		}
	}
}

// The interface half of TestNoActuationSurfaceExists: *Domain must NOT satisfy
// domain.Actuator, so a registry cannot be talked into executing a step for
// this domain even by a caller that holds the concrete type.
func TestNoActuationSurfaceExists(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(d).(domain.Actuator); ok {
		t.Fatal("*rds.Domain satisfies domain.Actuator; this domain has no actuator and must not be " +
			"able to acquire one by accident")
	}
	// It IS a domain.Domain and a domain.Refuser — the read-only halves.
	if _, ok := any(d).(domain.Domain); !ok {
		t.Error("*rds.Domain does not satisfy domain.Domain")
	}
	if _, ok := any(d).(domain.Refuser); !ok {
		t.Error("*rds.Domain does not satisfy domain.Refuser; the refusals ARE this domain's output")
	}

	// PlanSteps refuses unconditionally, including for an empty input.
	for _, recs := range [][]domain.Recommendation{nil, {}, {{Target: domain.TargetRef{ID: "x"}}}} {
		steps, err := d.PlanSteps(recs, domain.Guard{Now: testNow})
		if len(steps) != 0 {
			t.Errorf("PlanSteps returned %d steps", len(steps))
		}
		if err == nil || !strings.Contains(err.Error(), "report-only") {
			t.Errorf("PlanSteps error = %v, want domain.ErrReportOnly", err)
		}
	}
	// And Health says so, in every state.
	if h := d.Health(testNow); !h.ReportOnly {
		t.Error("Health.ReportOnly is false on an empty domain")
	}
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("a", "db.r6i.large", "postgres")},
		Metrics:   metricsFor("a", 30, 12, 8<<30, 40*GiB),
	}
	if err := d.Observe(collect(t, f)); err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if !h.ReportOnly {
		t.Error("Health.ReportOnly is false on a fully-loaded domain")
	}
	if !h.Ready {
		t.Errorf("Health.Ready is false after a clean snapshot: %s", h.Reason)
	}
	if h.Reason == "" {
		t.Error("a report-only domain must say why in Health.Reason")
	}
}

// No package-level mutable state. Every package-level var is a fixed table;
// anything new needs a deliberate decision, which is what this allowlist
// forces.
func TestNoUnexpectedPackageState(t *testing.T) {
	files, _ := packageFiles(t)
	allowed := map[string]bool{
		"shippedClassRates":   true,
		"shippedStorageRates": true,
		"classShapes":         true,
		"memorySemantics":     true,
		"collectedMetrics":    true,
		"exclusionCodes":      true,
		"rdsSizeFlexible":     true,
	}
	for name, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for _, id := range vs.Names {
					if !allowed[id.Name] {
						t.Errorf("%s declares package-level var %q; this package holds no mutable state "+
							"(add it to the allowlist only if it is an immutable table)", name, id.Name)
					}
				}
			}
		}
	}
}

// identifiers re-parses the file and returns every identifier in it, with
// comments and string literals excluded, so prose naming an API this package
// refuses to call does not trip the mutating-surface check.
func identifiers(t *testing.T, name, body string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, body, 0) // no ParseComments ⇒ comments dropped
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			b.WriteString(id.Name)
			b.WriteByte('\n')
		}
		return true
	})
	return b.String()
}

// structFieldNames returns a struct's exported and unexported field names, for
// the "this field must not exist" assertions.
func structFieldNames(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	if rt.Kind() != reflect.Struct {
		t.Fatalf("structFieldNames: %T is not a struct", v)
	}
	out := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out = append(out, rt.Field(i).Name)
	}
	return out
}

// Kind is honest about registration.
//
// pkg/domain's kind set is closed and does not contain RDS today, so
// domain.Registry.Register rejects this domain — a one-line core change U11
// may not make (FINDINGS.md §6). This test passes in BOTH worlds: it asserts
// the property that must hold either way, rather than pinning today's gap and
// breaking the moment someone closes it.
func TestKindIsHonestAboutRegistration(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	reg := domain.NewRegistry()
	regErr := reg.Register(d)

	if Kind.Valid() {
		if regErr != nil {
			t.Fatalf("domain.Kind(%q) is in pkg/domain's closed set but Register failed: %v", Kind, regErr)
		}
		// Registered: the core must still refuse to plan steps for it.
		if _, err := reg.PlanSteps(Kind, nil, domain.Guard{Now: testNow}); err == nil {
			t.Error("the registry planned steps for a report-only domain")
		}
		return
	}
	if regErr == nil {
		t.Fatalf("domain.Kind(%q) is not in pkg/domain's closed set, yet Register accepted it", Kind)
	}
	if !strings.Contains(regErr.Error(), "unknown kind") {
		t.Errorf("Register refused for an unexpected reason: %v", regErr)
	}
	// The consequence a reader needs: the domain still works standalone, and
	// cmd/ wires it through Report/Refusals until pkg/domain adds the kind.
	if got := d.Report(testNow, nil); got == nil || got.Domain != Kind {
		t.Error("the standalone Report path does not work without registration")
	}
}

// Every exported reason and advisory code is a stable, non-empty,
// kebab-case-ish string, and no two share a value. Codes are stored, grouped
// and filtered on; two codes with one value silently merge two findings.
func TestReasonCodesAreDistinct(t *testing.T) {
	codes := map[string]string{}
	for name, code := range map[string]string{
		"ReasonAuroraNotSupported":           ReasonAuroraNotSupported,
		"ReasonClusterMemberNotSupported":    ReasonClusterMemberNotSupported,
		"ReasonModeOff":                      ReasonModeOff,
		"ReasonUnknownEngine":                ReasonUnknownEngine,
		"ReasonUnknownInstanceClass":         ReasonUnknownInstanceClass,
		"ReasonEngineNotPriced":              ReasonEngineNotPriced,
		"ReasonUnknownDeployment":            ReasonUnknownDeployment,
		"ReasonUnverifiedRate":               ReasonUnverifiedRate,
		"ReasonInstanceClassIsAFailover":     ReasonInstanceClassIsAFailover,
		"ReasonFreeableMemoryIsPageCache":    ReasonFreeableMemoryIsPageCache,
		"ReasonBufferPoolScalesWithClass":    ReasonBufferPoolScalesWithClass,
		"ReasonMemorySemanticsUnencoded":     ReasonMemorySemanticsUnencoded,
		"ReasonStorageCannotShrink":          ReasonStorageCannotShrink,
		"ReasonStorageAutoscalingRatchet":    ReasonStorageAutoscalingRatchet,
		"ReasonReplicaIsFailoverCapacity":    ReasonReplicaIsFailoverCapacity,
		"ReasonMultiAZIsAvailabilityPosture": ReasonMultiAZIsAvailabilityPosture,
		"ReasonInsufficientWindow":           ReasonInsufficientWindow,
		"ReasonNoMetricEvidence":             ReasonNoMetricEvidence,
		"ReasonTruncatedMetrics":             ReasonTruncatedMetrics,
		"ReasonSizeFlexibilityExcluded":      ReasonSizeFlexibilityExcluded,
		"ReasonInstanceStateUnstable":        ReasonInstanceStateUnstable,
		"ReasonNoStoragePerformanceModel":    ReasonNoStoragePerformanceModel,
		"AdvisoryIdleInstance":               AdvisoryIdleInstance,
		"AdvisoryIdleReadReplica":            AdvisoryIdleReadReplica,
		"AdvisoryStorageFloor":               AdvisoryStorageFloor,
		"AdvisoryStorageAutoscaling":         AdvisoryStorageAutoscaling,
		"AdvisoryMultiAZMultiplier":          AdvisoryMultiAZMultiplier,
		"AdvisoryUnverifiedRate":             AdvisoryUnverifiedRate,
		"AdvisoryReservationStranding":       AdvisoryReservationStranding,
	} {
		if code == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if code != strings.ToLower(code) || strings.ContainsAny(code, " _") {
			t.Errorf("%s = %q: codes are lower-case and hyphenated so they are safe to store and group on",
				name, code)
		}
		if prev, dup := codes[code]; dup {
			t.Errorf("%s and %s share the value %q; two codes with one value silently merge two findings",
				prev, name, code)
		}
		codes[code] = name
	}
}

// A zero Window, a nil snapshot and an empty account all produce a valid,
// empty report rather than a panic or an error. "The core runs with zero
// domains registered" has a per-domain analogue: the domain runs with zero
// targets.
func TestEmptyAccountProducesAValidReport(t *testing.T) {
	s, err := NewSizer(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, snap := range []*Snapshot{nil, {Domain: Kind}, {Domain: Kind, Window: testWindow()}} {
		rep := s.Assess(testNow, snap, nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("empty report is invalid: %v", err)
		}
		if len(rep.Assessments) != 0 || rep.Totals.Instances != 0 {
			t.Error("an empty snapshot produced assessments")
		}
		var b strings.Builder
		if err := rep.WriteText(&b); err != nil {
			t.Fatalf("WriteText on an empty report: %v", err)
		}
		if !strings.Contains(b.String(), "PROPOSES NOTHING") {
			t.Error("the report does not say what it is")
		}
	}
}

// ClampWindow refuses to let a 1-minute series pretend to cover more than
// CloudWatch retains.
func TestClampWindowIsRetentionAware(t *testing.T) {
	end := testEnd
	long := Window{Start: end.Add(-30 * 24 * time.Hour), End: end}
	got, clamped := ClampWindow(long, PublicationPeriodSeconds)
	if !clamped {
		t.Fatal("a 30-day window at 60 s was not clamped")
	}
	if got.Duration() != RetentionAtOneMinute {
		t.Errorf("clamped window = %s, want %s", got.Duration(), RetentionAtOneMinute)
	}
	if !got.End.Equal(end) {
		t.Error("clamping moved the END of the window; the recent end is the part that exists")
	}
	// A short window is untouched.
	short := Window{Start: end.Add(-3 * 24 * time.Hour), End: end}
	if got, clamped := ClampWindow(short, PublicationPeriodSeconds); clamped || got != short {
		t.Error("a 3-day window was clamped")
	}
	// A coarser period reaches further back, so it is not clamped here.
	if _, clamped := ClampWindow(long, CreditPeriodSeconds); clamped {
		t.Error("a 5-minute series was clamped to the 1-minute retention limit")
	}
}
