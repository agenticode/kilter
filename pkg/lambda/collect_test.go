package lambda

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func testWindow() Window { return Window{Start: testStart(), End: testNow} }

func fixtureWithTwoFunctions() *Fixture {
	fx := &Fixture{
		Functions: []FunctionRecord{
			{FunctionArn: "arn:...:function:beta", FunctionName: "beta", MemorySize: 512, Timeout: 10,
				Architectures: []string{ArchARM}, Runtime: "nodejs22.x", PackageType: PackageZip},
			{FunctionArn: "arn:...:function:alpha", FunctionName: "alpha", MemorySize: 1024, Timeout: 30,
				Architectures: []string{ArchX86}, Runtime: "python3.13", PackageType: PackageZip,
				Tags: map[string]string{"team": "payments"}, ProvisionedConcurrency: 0},
		},
		Events:  map[string][]LogEvent{},
		Metrics: map[string][]Point{},
	}
	fx.Events[LogGroupPrefix+"alpha"] = SyntheticReports("a", testStart(), testSpan, 300, 1024, 400, 100, 300, 0)
	fx.Events[LogGroupPrefix+"beta"] = SyntheticReports("b", testStart(), testSpan, 120, 512, 200, 50, 250, 10)
	fx.Metrics["alpha/"+MetricInvocations] = SyntheticMetric(testStart(), time.Hour, 48, 100)
	fx.Metrics["beta/"+MetricInvocations] = SyntheticMetric(testStart(), time.Hour, 48, 5)
	return fx
}

func collect(t *testing.T, fx *Fixture, mut func(*CollectorConfig)) *Snapshot {
	t.Helper()
	cfg := DefaultCollectorConfig(testWindow())
	cfg.Scope, cfg.Region = testScope, testRegion
	if mut != nil {
		mut(&cfg)
	}
	c, err := NewCollector(fx, fx, fx, cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return snap
}

func TestCollectorPaginatesAndSorts(t *testing.T) {
	fx := fixtureWithTwoFunctions()
	fx.PageSize = 1 // force multi-page inventory and log reads

	snap := collect(t, fx, func(c *CollectorConfig) { c.MaxPages = 400 })
	if snap.Domain != Kind || snap.Scope != testScope || snap.Region != testRegion {
		t.Fatalf("snapshot identity is wrong: %+v", snap.Domain)
	}
	if len(snap.Targets) != 2 {
		t.Fatalf("collected %d targets, want 2", len(snap.Targets))
	}
	// Sorted by ARN, not by the order AWS paginated them in.
	if snap.Targets[0].Function.Name != "alpha" || snap.Targets[1].Function.Name != "beta" {
		t.Fatalf("targets are not sorted by ARN: %s, %s",
			snap.Targets[0].Function.Name, snap.Targets[1].Function.Name)
	}
	if fx.Calls.ListFunctions < 2 || fx.Calls.FilterLogEvents < 2 {
		t.Errorf("pagination did not happen: %+v", fx.Calls)
	}
	alpha := snap.Targets[0]
	if len(alpha.Reports) != 300 {
		t.Errorf("alpha has %d REPORT records, want 300", len(alpha.Reports))
	}
	if alpha.Function.Arch() != ArchX86 || alpha.Function.MemoryMB != 1024 {
		t.Errorf("alpha configuration did not survive collection: %+v", alpha.Function)
	}
	if snap.Targets[1].Function.Arch() != ArchARM {
		t.Errorf("beta is an arm64 function")
	}
	// Series arrive sorted by metric name and every collected metric is present.
	if len(alpha.Series) != len(collectedMetrics) {
		t.Fatalf("alpha has %d series, want %d", len(alpha.Series), len(collectedMetrics))
	}
	for i := 1; i < len(alpha.Series); i++ {
		if alpha.Series[i-1].Metric > alpha.Series[i].Metric {
			t.Fatalf("series are not sorted: %v", alpha.Series)
		}
	}
	inv, ok := alpha.SeriesFor(MetricInvocations)
	if !ok || inv.Sum() != 4800 {
		t.Errorf("invocations series = %v (found %v)", inv.Sum(), ok)
	}
}

// A missing or forbidden log group is an operational condition, not a reason to
// fail the whole collection: the function is reported WITHOUT evidence, which
// is exactly what happened.
func TestCollectorDegradesWhenLogsAreUnreadable(t *testing.T) {
	fx := fixtureWithTwoFunctions()
	fx.LogsErr = map[string]error{LogGroupPrefix + "alpha": errors.New("AccessDeniedException")}

	snap := collect(t, fx, nil)
	if !snap.Stale {
		t.Errorf("a partial collection must mark the snapshot stale")
	}
	if len(snap.Warnings) == 0 {
		t.Fatalf("a degraded collection must say what it could not read")
	}
	if len(snap.Targets[0].Reports) != 0 {
		t.Errorf("alpha should have no REPORT evidence")
	}
	if len(snap.Targets[1].Reports) == 0 {
		t.Errorf("beta's evidence must survive alpha's failure")
	}
	// And the sizer refuses on it rather than guessing.
	s, _ := NewSizer(DefaultConfig())
	rep := s.Assess(testNow, snap, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invariants violated: %v", err)
	}
	a, _ := rep.For(snap.Targets[0].Ref.ID)
	if !a.Suppressed(ReasonNoReportEvidence) {
		t.Errorf("a function with unreadable logs must refuse, got %v", codes(a))
	}
}

// A GetMetricData response that omits a query's result means "we were not
// told", not "the metric is empty".
func TestCollectorTreatsAMissingMetricResultAsPartial(t *testing.T) {
	fx := fixtureWithTwoFunctions()
	fx.DropResults = 1

	snap := collect(t, fx, nil)
	var partial int
	for _, tg := range snap.Targets {
		for _, s := range tg.Series {
			if s.Partial && s.Status == StatusTruncated {
				partial++
			}
		}
	}
	if partial == 0 {
		t.Fatalf("a truncated metric response must mark a series partial")
	}
}

func TestCollectorHonorsItsBudgets(t *testing.T) {
	fx := fixtureWithTwoFunctions()
	snap := collect(t, fx, func(c *CollectorConfig) { c.MaxEventsPerFunction = 10 })
	if !snap.Stale {
		t.Errorf("hitting the event cap must mark the snapshot stale")
	}
	for _, tg := range snap.Targets {
		if len(tg.Reports) > 10 {
			t.Errorf("%s returned %d reports past the 10-event cap", tg.Function.Name, len(tg.Reports))
		}
	}
	found := false
	for _, w := range snap.Warnings {
		if strings.Contains(w, "PREFIX of the window") {
			found = true
		}
	}
	if !found {
		t.Errorf("the cap warning must say the evidence is a prefix, got %v", snap.Warnings)
	}
}

func TestCollectorRespectsTheIncludeFilterAndFunctionCap(t *testing.T) {
	fx := fixtureWithTwoFunctions()
	snap := collect(t, fx, func(c *CollectorConfig) { c.Include = []string{"beta"} })
	if len(snap.Targets) != 1 || snap.Targets[0].Function.Name != "beta" {
		t.Fatalf("include filter ignored: %d targets", len(snap.Targets))
	}

	fx2 := fixtureWithTwoFunctions()
	snap2 := collect(t, fx2, func(c *CollectorConfig) { c.MaxFunctions = 1 })
	if len(snap2.Targets) != 1 {
		t.Fatalf("function cap ignored: %d targets", len(snap2.Targets))
	}
	if len(snap2.Warnings) == 0 {
		t.Errorf("a capped inventory must say functions are missing, not that they had no findings")
	}
}

func TestCollectorHonorsContextCancellation(t *testing.T) {
	fx := fixtureWithTwoFunctions()
	cfg := DefaultCollectorConfig(testWindow())
	c, err := NewCollector(fx, fx, fx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestCollectorRequiresAnInventoryAndAWindow(t *testing.T) {
	if _, err := NewCollector(nil, nil, nil, DefaultCollectorConfig(testWindow())); err == nil {
		t.Errorf("a collector with no inventory seam must be rejected")
	}
	fx := fixtureWithTwoFunctions()
	if _, err := NewCollector(fx, nil, nil, CollectorConfig{}); err == nil {
		t.Errorf("a collector with no window must be rejected")
	}
	// Logs and metrics are optional: an account with no logs:FilterLogEvents
	// permission still gets an inventory, and every function honestly refuses.
	c, err := NewCollector(fx, nil, nil, DefaultCollectorConfig(testWindow()))
	if err != nil {
		t.Fatalf("logs and metrics must be optional: %v", err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Targets) != 2 || len(snap.Targets[0].Reports) != 0 {
		t.Errorf("inventory-only collection should yield targets with no evidence")
	}
}
