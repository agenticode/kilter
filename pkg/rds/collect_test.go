package rds

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The four CloudWatch truths every domain in this tree has had to re-derive
// (docs/design/rds-batch-assessment.md §1.4), asserted here for the fifth
// time. FINDINGS.md §5 records them as the specification a shared
// pkg/cloudwatch would have to satisfy.

// Truth 4, and the acceptance test the design doc names: clamp the window to
// CloudWatch retention. "Data points with a period of 60 seconds (1 minute)
// are available for 15 days."
func TestCollectorClampsWindowTo15DayRetention(t *testing.T) {
	long := Window{Start: testEnd.Add(-45 * 24 * time.Hour), End: testEnd}
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("old", "db.r6i.xlarge", "postgres")},
		Metrics: map[string][]Point{
			// One datapoint 40 days back, one inside the retention window. A
			// collector that did not clamp would ask for both.
			"old/" + MetricDatabaseConns: {
				{At: testEnd.Add(-40 * 24 * time.Hour), Value: 99},
				{At: testEnd.Add(-2 * 24 * time.Hour), Value: 0},
			},
		},
	}
	cfg := DefaultCollectorConfig(long)
	c, err := NewCollector(f, f, f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Window().Duration(); got != RetentionAtOneMinute {
		t.Fatalf("collector window = %s, want the %s retention limit", got, RetentionAtOneMinute)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Window.Duration() != RetentionAtOneMinute {
		t.Fatalf("snapshot window = %s, want %s. A snapshot that CLAIMS 45 days and contains 15 is a lie "+
			"told by omission, and every downstream window gate reads the claim",
			snap.Window.Duration(), RetentionAtOneMinute)
	}
	conns, ok := snap.Targets[0].SeriesFor(MetricDatabaseConns)
	if !ok {
		t.Fatal("no connections series")
	}
	if len(conns.Points) != 1 || conns.Points[0].Value != 0 {
		t.Fatalf("the out-of-retention datapoint survived the clamp: %+v", conns.Points)
	}
	var warned bool
	for _, w := range snap.Warnings {
		if strings.Contains(w, "shortened") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the window was clamped silently; warnings were %v", snap.Warnings)
	}

	// The credit metrics publish at 5 minutes only, so their queries go out at
	// 300 s even when the collector was configured for 60 s.
	credit, ok := snap.Targets[0].SeriesFor(MetricCPUCreditBalance)
	if !ok {
		t.Fatal("no credit series")
	}
	if credit.PeriodSeconds != CreditPeriodSeconds {
		t.Errorf("credit metric period = %d, want %d: \"CPU credit metrics are available at a "+
			"five-minute frequency only\"", credit.PeriodSeconds, CreditPeriodSeconds)
	}
	cpu, _ := snap.Targets[0].SeriesFor(MetricCPUUtilization)
	if cpu.PeriodSeconds != PublicationPeriodSeconds {
		t.Errorf("CPU period = %d, want %d", cpu.PeriodSeconds, PublicationPeriodSeconds)
	}
}

// Truth 2: route results by query ID, never by position. Truth 3: a missing
// result is truncation, not "no usage".
func TestCollectorRoutesByQueryIDAndMarksMissingResultsTruncated(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("one", "db.r6i.large", "postgres"),
			rec("two", "db.r6i.large", "postgres"),
		},
		Metrics: mergeMetrics(
			metricsFor("one", 11, 1, 8<<30, 40*GiB),
			metricsFor("two", 22, 2, 8<<30, 40*GiB),
		),
	}
	snap := collect(t, f)
	for _, tgt := range snap.Targets {
		cpu, ok := tgt.SeriesFor(MetricCPUUtilization)
		if !ok || len(cpu.Points) == 0 {
			t.Fatalf("%s: no CPU series", tgt.Instance.Identifier)
		}
		want := map[string]float64{"one": 11, "two": 22}[tgt.Instance.Identifier]
		if cpu.Points[0].Value != want {
			t.Errorf("%s got CPU %v, want %v: results were routed by position, not by query ID",
				tgt.Instance.Identifier, cpu.Points[0].Value, want)
		}
	}

	// Drop the first three answers: those series must come back PARTIAL with
	// no points, not complete-and-empty.
	f.DropResults = 3
	snap = collect(t, f)
	var partial, complete int
	for _, tgt := range snap.Targets {
		for _, s := range tgt.Series {
			if s.Partial {
				partial++
				if s.Status != StatusTruncated {
					t.Errorf("a missing result was marked %q, want %q", s.Status, StatusTruncated)
				}
				if len(s.Points) != 0 {
					t.Error("a truncated series carries points")
				}
				continue
			}
			complete++
		}
	}
	if partial != 3 {
		t.Errorf("%d series marked partial, want 3", partial)
	}
	if complete == 0 {
		t.Error("every series was marked partial; the distinction is not being made")
	}
}

// Truth 1: batch at the GetMetricData limit. With more series than fit in one
// call, the collector must issue more than one call and lose nothing.
func TestCollectorBatchesWithinTheGetMetricDataLimit(t *testing.T) {
	const n = 60 // 60 instances × 11 metrics = 660 queries > MaxQueriesPerCall
	f := &Fixture{Metrics: map[string][]Point{}}
	for i := 0; i < n; i++ {
		id := string(rune('a'+i/26)) + string(rune('a'+i%26))
		f.Instances = append(f.Instances, rec(id, "db.r6i.large", "postgres"))
		for k, v := range metricsFor(id, float64(i), 1, 8<<30, 40*GiB) {
			f.Metrics[k] = v
		}
	}
	snap := collect(t, f)
	if len(snap.Targets) != n {
		t.Fatalf("collected %d instances, want %d", len(snap.Targets), n)
	}
	wantCalls := (n*len(collectedMetrics) + MaxQueriesPerCall - 1) / MaxQueriesPerCall
	if f.Calls.GetMetricData != wantCalls {
		t.Errorf("GetMetricData called %d times, want %d (%d queries at %d per call)",
			f.Calls.GetMetricData, wantCalls, n*len(collectedMetrics), MaxQueriesPerCall)
	}
	for _, tgt := range snap.Targets {
		if s, ok := tgt.SeriesFor(MetricCPUUtilization); !ok || len(s.Points) == 0 {
			t.Fatalf("%s lost its CPU series across the batch boundary", tgt.Instance.Identifier)
		}
	}
}

// Pagination is real, and a page cap is a stated incompleteness rather than a
// silent truncation of the fleet.
func TestCollectorPaginatesAndSaysWhenItStopped(t *testing.T) {
	f := &Fixture{PageSize: 2, Metrics: map[string][]Point{}}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		f.Instances = append(f.Instances, rec(id, "db.r6i.large", "postgres"))
	}
	snap := collect(t, f)
	if len(snap.Targets) != 7 {
		t.Fatalf("paginated collection returned %d instances, want 7", len(snap.Targets))
	}
	if f.Calls.DescribeDBInstances < 4 {
		t.Errorf("DescribeDBInstances called %d times for 7 instances at 2 per page",
			f.Calls.DescribeDBInstances)
	}

	// A page budget of 1 truncates the inventory, loudly.
	f2 := &Fixture{PageSize: 2, Instances: f.Instances}
	snap2 := collect(t, f2, func(c *CollectorConfig) { c.MaxPages = 1 })
	if !snap2.Stale {
		t.Error("a truncated inventory did not mark the snapshot stale")
	}
	var said bool
	for _, w := range snap2.Warnings {
		if strings.Contains(w, "ABSENT from this report") {
			said = true
		}
	}
	if !said {
		t.Errorf("a truncated inventory did not say the missing instances are absent rather than clean: %v",
			snap2.Warnings)
	}
}

// The metrics and commitment seams are optional. A caller with fewer IAM
// permissions gets a smaller report, never no report.
func TestOptionalSeamsDegradeRatherThanBreak(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("a", "db.r6i.xlarge", "postgres")},
		Metrics:   metricsFor("a", 30, 12, 24<<30, 40*GiB),
	}
	cfg := DefaultCollectorConfig(testWindow())
	c, err := NewCollector(f, nil, nil, cfg) // no metrics, no reservations
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("a collector with no metrics API failed outright: %v", err)
	}
	rep := assess(t, snap, nil)
	a := must(t, rep, "a")
	wantCode(t, a, ReasonNoMetricEvidence)
	if a.Idle.Idle {
		t.Fatal("an instance with no metrics API was reported as idle")
	}

	// The inventory API is the one hard dependency.
	if _, err := NewCollector(nil, f, f, cfg); err == nil {
		t.Error("NewCollector accepted a nil inventory API")
	}
	if _, err := NewCollector(f, f, f, CollectorConfig{}); err == nil {
		t.Error("NewCollector accepted a zero-length window")
	}

	// A cluster listing failure degrades: the member is still excluded, and
	// the report says it could not tell which kind of cluster it was.
	f3 := &Fixture{
		Instances:   []DBInstanceRecord{rec("m", "db.r6i.large", "mysql", withCluster("c"))},
		ClustersErr: errors.New("AccessDenied"),
		Metrics:     metricsFor("m", 30, 12, 8<<30, 40*GiB),
	}
	rep3 := assess(t, collect(t, f3), nil)
	wantCode(t, must(t, rep3, "m"), ReasonClusterMemberNotSupported)

	// A reservation listing failure degrades to net == gross, and says so.
	f4 := &Fixture{
		Instances:       []DBInstanceRecord{rec("r", "db.r6i.large", "postgres")},
		Metrics:         metricsFor("r", 30, 12, 8<<30, 40*GiB),
		ReservationsErr: errors.New("AccessDenied"),
	}
	snap4 := collect(t, f4)
	if len(snap4.Reservations) != 0 {
		t.Error("a failed reservation listing produced reservations")
	}
	var warned bool
	for _, w := range snap4.Warnings {
		if strings.Contains(w, "under-claims") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a missing reservation inventory did not say which direction it errs: %v", snap4.Warnings)
	}
}

// A tag read that fails leaves the guardrail unevaluable, and the report says
// so rather than treating the instance as un-tagged.
func TestTagFailureIsStatedNotAssumed(t *testing.T) {
	arn := "arn:aws:rds:us-east-1:1234:db:t"
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("t", "db.r6i.large", "postgres")},
		TagsErr:   map[string]error{arn: errors.New("AccessDenied")},
		Metrics:   metricsFor("t", 30, 12, 8<<30, 40*GiB),
	}
	snap := collect(t, f)
	var said bool
	for _, w := range snap.Warnings {
		if strings.Contains(w, TagKilterMode) {
			said = true
		}
	}
	if !said {
		t.Errorf("a failed tag read did not warn that the opt-out tag cannot be honoured: %v", snap.Warnings)
	}
}

// Context cancellation is honoured at every seam, and never produces a partial
// snapshot that looks complete.
func TestContextCancellation(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("a", "db.r6i.large", "postgres")},
		Metrics:   metricsFor("a", 30, 12, 8<<30, 40*GiB),
	}
	cfg := DefaultCollectorConfig(testWindow())
	c, err := NewCollector(f, f, f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect on a cancelled context returned %v, want context.Canceled", err)
	}
	if f.Calls.GetMetricData != 0 {
		t.Error("a cancelled collection still queried CloudWatch")
	}
}

// Reservation amortization: EffectiveHourly = UsagePrice + FixedPrice ÷ term
// hours, and only reservations that actually bill are returned.
func TestReservationsAreAmortizedAndFiltered(t *testing.T) {
	const year = int64(365 * 24 * 3600)
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("a", "db.r6i.large", "postgres")},
		Reservations: []ReservedDBInstanceRecord{
			{ReservedDBInstanceId: "partial", DBInstanceClass: "db.r6i.large", DBInstanceCount: 1,
				ProductDescription: "postgresql", State: "active",
				UsagePrice: 0.10, FixedPrice: 876, Duration: year, StartTime: testStart},
			{ReservedDBInstanceId: "retired", DBInstanceClass: "db.r6i.large", DBInstanceCount: 1,
				ProductDescription: "postgresql", State: "retired", UsagePrice: 0.20, Duration: year},
			{ReservedDBInstanceId: "zero-count", DBInstanceClass: "db.r6i.large", DBInstanceCount: 0,
				ProductDescription: "postgresql", State: "active", Duration: year},
			{ReservedDBInstanceId: "multiaz", DBInstanceClass: "db.r6i.large", DBInstanceCount: 1,
				ProductDescription: "postgresql", State: "active", MultiAZ: true,
				UsagePrice: 0.20, Duration: year, StartTime: testStart},
		},
	}
	snap := collect(t, f)
	if len(snap.Reservations) != 2 {
		t.Fatalf("collected %d reservations, want 2 (retired and zero-count are dropped): %+v",
			len(snap.Reservations), snap.Reservations)
	}
	byID := map[string]int{}
	for i, r := range snap.Reservations {
		byID[r.ID] = i
	}
	p := snap.Reservations[byID["partial"]]
	// 876 upfront over 8760 hours = $0.10/h, plus the $0.10/h recurring.
	if got, want := p.EffectiveHourlyUSD, 0.20; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("amortized rate = %v, want %v (usage + upfront ÷ term hours)", got, want)
	}
	if p.Expires.IsZero() {
		t.Error("a reservation with a start time and a duration has no expiry")
	}
	m := snap.Reservations[byID["multiaz"]]
	if u, ok := m.Units(); !ok || u != 8 {
		t.Errorf("a Multi-AZ db.r6i.large supplies %v units, want 8 (4 Single-AZ × 2)", u)
	}
}

// The collector reads only what it says it reads, so an IAM policy sized from
// CollectedMetrics is the right size.
func TestCollectedMetricsIsTheWholeQueryList(t *testing.T) {
	names := CollectedMetrics()
	if len(names) != len(collectedMetrics) {
		t.Fatalf("CollectedMetrics returned %d names for %d queries", len(names), len(collectedMetrics))
	}
	seen := map[string]bool{}
	for i, n := range names {
		if n == "" {
			t.Errorf("metric %d has no name", i)
		}
		if seen[n] {
			t.Errorf("metric %q is queried twice", n)
		}
		seen[n] = true
		if i > 0 && names[i-1] > n {
			t.Errorf("the metric list is not in canonical order (%q after %q)", n, names[i-1])
		}
	}
	// The four metrics every verdict in this package rests on.
	for _, want := range []string{
		MetricCPUUtilization, MetricDatabaseConns, MetricFreeableMemory, MetricFreeStorageSpace,
	} {
		if !seen[want] {
			t.Errorf("the collector does not read %q, which a verdict depends on", want)
		}
	}
}
