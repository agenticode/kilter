package ec2

import (
	"context"
	"strings"
	"testing"
	"time"
)

// now is the caller-supplied clock every test uses. This package has none.
var testNow = time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC)

func mustFixture(t *testing.T, path string) *Fixture {
	t.Helper()
	f, err := LoadFixtureFile(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return f
}

func mustCollect(t *testing.T, f *Fixture, cfg CollectorConfig) *Snapshot {
	t.Helper()
	c, err := NewCollector(f, f, cfg)
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	snap, err := c.Collect(context.Background(), testNow)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap == nil {
		t.Fatal("collect returned a nil snapshot and a nil error")
	}
	return snap
}

// An empty account is a normal answer, not an error and not a nil snapshot.
func TestCollectEmptyAccount(t *testing.T) {
	f := mustFixture(t, "testdata/account-empty.json")
	snap := mustCollect(t, f, CollectorConfig{Scope: "1234/us-east-1", Region: "us-east-1", Window: 2 * time.Hour})

	if len(snap.Targets) != 0 {
		t.Fatalf("empty account produced %d targets", len(snap.Targets))
	}
	if snap.Stale {
		t.Error("an empty account is complete, not stale")
	}
	if len(snap.Warnings) != 0 {
		t.Errorf("empty account warned: %v", snap.Warnings)
	}
	if len(f.MetricRequests) != 0 {
		t.Errorf("no instances should mean no GetMetricData calls, got %d", len(f.MetricRequests))
	}
	if snap.Domain != Domain || snap.Timestamp != testNow {
		t.Errorf("snapshot header wrong: %+v", snap)
	}
}

func TestCollectPaginatedInventoryAndMetrics(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	snap := mustCollect(t, f, CollectorConfig{
		Scope: "1234/us-east-1", Region: "us-east-1", Window: 2 * time.Hour,
		PreferredPeriodSeconds: PeriodDetailedSeconds,
	})

	if len(f.InventoryRequests) != 3 {
		t.Fatalf("expected 3 inventory pages, got %d", len(f.InventoryRequests))
	}
	if len(f.MetricRequests) < 2 {
		t.Fatalf("metricPageSize=3 should force metric pagination, got %d calls", len(f.MetricRequests))
	}

	// The stopped instance is filtered; the rest arrive sorted by ID.
	var got []string
	for _, tg := range snap.Targets {
		got = append(got, tg.Ref.ID)
	}
	want := []string{
		"i-0a00000000000000a", "i-0b00000000000000b", "i-0d00000000000000d", "i-0e00000000000000e",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("targets = %v, want %v (stopped instances excluded, sorted by ID)", got, want)
	}

	a := snap.Targets[0]
	if a.Ref.Name != "billing-api" || a.Instance.Tags["env"] != "prod" {
		t.Errorf("tags not normalized: %+v", a.Instance)
	}
	if a.Instance.Architecture != "amd64" || a.Instance.Platform != "Linux/UNIX" {
		t.Errorf("arch/platform not normalized: %+v", a.Instance)
	}
	cpu, ok := a.SeriesFor(MetricCPUUtilization)
	if !ok || len(cpu.Points) != 6 {
		t.Fatalf("CPU series for %s: ok=%v points=%d", a.Ref.ID, ok, len(cpu.Points))
	}
	if peak, _ := cpu.Max(); peak != 11.4 {
		t.Errorf("reassembled peak = %v, want 11.4", peak)
	}
	for i := 1; i < len(cpu.Points); i++ {
		if !cpu.Points[i-1].At.Before(cpu.Points[i].At) {
			t.Fatalf("points are not time-ordered after paging: %v", cpu.Points)
		}
	}

	// Blind spots are declared, not implied.
	if strings.Join(a.Blind, ",") != "disk-space,memory" {
		t.Errorf("blind spots = %v, want disk-space,memory", a.Blind)
	}
}

// Detailed monitoring is the only thing that buys 1-minute datapoints; asking
// for 60 s where CloudWatch publishes at 300 s must not be recorded as if it
// had been granted.
func TestCollectClampsPeriodToPublicationGranularity(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	snap := mustCollect(t, f, CollectorConfig{
		Window: 2 * time.Hour, PreferredPeriodSeconds: PeriodDetailedSeconds,
	})

	periods := map[string]int32{}
	for _, req := range f.MetricRequests {
		for _, q := range req.Queries {
			if q.MetricName == MetricCPUUtilization {
				periods[q.Dimensions["InstanceId"]] = q.PeriodSeconds
			}
		}
	}
	if got := periods["i-0a00000000000000a"]; got != PeriodBasicSeconds {
		t.Errorf("basic-monitoring instance requested at %ds, want %ds", got, PeriodBasicSeconds)
	}
	if got := periods["i-0b00000000000000b"]; got != PeriodDetailedSeconds {
		t.Errorf("detailed-monitoring instance requested at %ds, want %ds", got, PeriodDetailedSeconds)
	}
	for _, tg := range snap.Targets {
		s, _ := tg.SeriesFor(MetricCPUUtilization)
		if tg.Instance.DetailedMonitoring != (s.PeriodSeconds == PeriodDetailedSeconds) {
			t.Errorf("%s: monitoring=%v but series period=%ds",
				tg.Ref.ID, tg.Instance.DetailedMonitoring, s.PeriodSeconds)
		}
	}
}

// Credit metrics are requested only where they exist.
func TestCollectRequestsCreditMetricsOnlyForBurstable(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	mustCollect(t, f, CollectorConfig{Window: 2 * time.Hour, CollectMemory: true})

	byInstance := map[string]map[string]bool{}
	for _, req := range f.MetricRequests {
		for _, q := range req.Queries {
			id := q.Dimensions["InstanceId"]
			if byInstance[id] == nil {
				byInstance[id] = map[string]bool{}
			}
			byInstance[id][q.MetricName] = true
		}
	}
	if !byInstance["i-0b00000000000000b"][MetricCPUCreditBalance] {
		t.Error("t3.large did not get a CPUCreditBalance query")
	}
	if byInstance["i-0a00000000000000a"][MetricCPUCreditBalance] {
		t.Error("m5.xlarge got a credit query it has no metric for")
	}
	for id, m := range byInstance {
		if !m[MetricMemUsedPercent] {
			t.Errorf("%s: CollectMemory was on but no %s query was issued", id, MetricMemUsedPercent)
		}
	}
}

// A response that answers fewer queries than it was asked is truncation, and
// truncation must never read as "this metric has no data".
func TestCollectDetectsTruncatedMetricResponse(t *testing.T) {
	f := mustFixture(t, "testdata/account-truncated.json")
	snap := mustCollect(t, f, CollectorConfig{Window: 2 * time.Hour})

	if !snap.Stale {
		t.Error("a truncated metric response must mark the snapshot stale")
	}
	joined := strings.Join(snap.Warnings, " | ")
	if !strings.Contains(joined, "truncated") {
		t.Errorf("warnings do not name truncation: %q", joined)
	}
	for _, tg := range snap.Targets {
		s, ok := tg.SeriesFor(MetricCPUUtilization)
		if !ok {
			t.Fatalf("%s has no CPU series at all", tg.Ref.ID)
		}
		if !s.Partial {
			t.Errorf("%s CPU series is not marked partial after truncation", tg.Ref.ID)
		}
	}
	// The recorded PartialData status is preserved distinctly from our own
	// truncation marker.
	first, _ := snap.Targets[0].SeriesFor(MetricCPUUtilization)
	if first.Status != StatusPartialData && first.Status != StatusTruncated {
		t.Errorf("status = %q, want a partial/truncated marker", first.Status)
	}
}

// A CloudWatch status other than Complete propagates as a partial series even
// when every query was answered.
func TestCollectPropagatesPartialDataStatus(t *testing.T) {
	f := &Fixture{
		InventoryPages: []DescribeInstancesOutput{{Reservations: []Reservation{{Instances: []InstanceRecord{
			{InstanceID: "i-1", InstanceType: "m5.large", Architecture: "x86_64", State: "running"},
		}}}}},
		Metrics: []RecordedSeries{{
			InstanceID: "i-1", Metric: MetricCPUUtilization, Status: StatusPartialData,
			Points: []Point{{At: testNow.Add(-time.Hour), Value: 5}, {At: testNow, Value: 6}},
		}},
	}
	snap := mustCollect(t, f, CollectorConfig{Window: 2 * time.Hour})
	s, _ := snap.Targets[0].SeriesFor(MetricCPUUtilization)
	if !s.Partial || s.Status != StatusPartialData {
		t.Fatalf("partial=%v status=%q, want a partial series carrying PartialData", s.Partial, s.Status)
	}
}

// A pager that never advances is a bug, and must be loud rather than infinite.
func TestCollectRejectsNonAdvancingPager(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	f.RepeatInventoryToken = true
	c, _ := NewCollector(f, f, CollectorConfig{Window: time.Hour})
	if _, err := c.Collect(context.Background(), testNow); err == nil ||
		!strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("err = %v, want a non-advancing-token error", err)
	}
}

// The page budget turns an unbounded pager into a stale snapshot, not a hang.
func TestCollectPageBudgetDegradesToStale(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	c, _ := NewCollector(f, f, CollectorConfig{Window: time.Hour, MaxPages: 2})
	snap, err := c.Collect(context.Background(), testNow)
	if err != nil {
		t.Fatalf("page budget should degrade, not fail: %v", err)
	}
	if !snap.Stale {
		t.Error("exhausting the page budget must mark the snapshot stale")
	}
	if !strings.Contains(strings.Join(snap.Warnings, " "), "pagination stopped") {
		t.Errorf("warnings do not explain the truncation: %v", snap.Warnings)
	}
}

// A transport failure is not evidence of an empty account.
func TestCollectPropagatesTransportErrors(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	f.InventoryFailAt = 2
	c, _ := NewCollector(f, f, CollectorConfig{Window: time.Hour})
	snap, err := c.Collect(context.Background(), testNow)
	if err == nil {
		t.Fatal("a failed DescribeInstances call must return an error")
	}
	if snap != nil {
		t.Error("a failed collect must not also return a snapshot")
	}

	g := mustFixture(t, "testdata/account-paginated.json")
	g.MetricsFailAt = 1
	c2, _ := NewCollector(g, g, CollectorConfig{Window: time.Hour})
	if _, err := c2.Collect(context.Background(), testNow); err == nil {
		t.Fatal("a failed GetMetricData call must return an error")
	}
}

// Duplicate records across pages would double-count the account's usage in the
// commitment waterfall.
func TestCollectDeduplicatesAcrossPages(t *testing.T) {
	rec := InstanceRecord{InstanceID: "i-dup", InstanceType: "m5.large", Architecture: "x86_64", State: "running"}
	f := &Fixture{InventoryPages: []DescribeInstancesOutput{
		{Reservations: []Reservation{{Instances: []InstanceRecord{rec}}}},
		{Reservations: []Reservation{{Instances: []InstanceRecord{rec}}}},
	}}
	snap := mustCollect(t, f, CollectorConfig{Window: time.Hour})
	if len(snap.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(snap.Targets))
	}
	if !strings.Contains(strings.Join(snap.Warnings, " "), "more than one page") {
		t.Errorf("deduplication was silent: %v", snap.Warnings)
	}
}

// GetMetricData accepts at most 500 queries; the collector must batch, and the
// fixture rejects an over-large call so a regression cannot pass quietly.
func TestCollectBatchesWithinTheGetMetricDataLimit(t *testing.T) {
	var instances []InstanceRecord
	for i := 0; i < 620; i++ {
		instances = append(instances, InstanceRecord{
			InstanceID: idFor(i), InstanceType: "m5.large", Architecture: "x86_64", State: "running",
		})
	}
	f := &Fixture{InventoryPages: []DescribeInstancesOutput{
		{Reservations: []Reservation{{Instances: instances}}},
	}}
	snap := mustCollect(t, f, CollectorConfig{Window: time.Hour, CollectMemory: true})
	if len(snap.Targets) != 620 {
		t.Fatalf("got %d targets", len(snap.Targets))
	}
	// 620 instances × 2 metrics = 1240 queries ⇒ at least 3 calls of ≤ 500.
	if len(f.MetricRequests) < 3 {
		t.Fatalf("expected batching into >=3 calls, got %d", len(f.MetricRequests))
	}
	for i, req := range f.MetricRequests {
		if len(req.Queries) > MaxSeriesPerCall {
			t.Fatalf("call %d carried %d queries, over the %d limit", i, len(req.Queries), MaxSeriesPerCall)
		}
	}
}

func idFor(i int) string {
	const hex = "0123456789abcdef"
	b := []byte("i-00000000000000000")
	for j := len(b) - 1; j >= 2 && i > 0; j-- {
		b[j] = hex[i%16]
		i /= 16
	}
	return string(b)
}

func TestCollectorRejectsBadConfiguration(t *testing.T) {
	f := &Fixture{}
	if _, err := NewCollector(nil, f, CollectorConfig{}); err == nil {
		t.Error("a nil inventory seam must be rejected")
	}
	if _, err := NewCollector(f, nil, CollectorConfig{}); err == nil {
		t.Error("a nil metrics seam must be rejected")
	}
	if _, err := NewCollector(f, f, CollectorConfig{MaxSeriesPerCall: MaxSeriesPerCall + 1}); err == nil {
		t.Error("an over-large batch size must be rejected")
	}
	c, _ := NewCollector(f, f, CollectorConfig{})
	if _, err := c.Collect(context.Background(), time.Time{}); err == nil {
		t.Error("a zero `now` must be rejected: this package has no clock")
	}
	if c.Domain() != Domain {
		t.Errorf("domain = %q", c.Domain())
	}
}

func TestCollectHonorsContextCancellation(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	c, _ := NewCollector(f, f, CollectorConfig{Window: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Collect(ctx, testNow); err == nil {
		t.Fatal("a cancelled context must abort collection")
	}
}

// Query IDs must satisfy CloudWatch's grammar; the fixture enforces it, so
// this test is really about the collector's generator.
func TestCollectGeneratesValidQueryIDs(t *testing.T) {
	f := mustFixture(t, "testdata/account-paginated.json")
	mustCollect(t, f, CollectorConfig{Window: 2 * time.Hour, CollectMemory: true})
	for i, req := range f.MetricRequests {
		seen := map[string]bool{}
		for _, q := range req.Queries {
			if err := validQueryID(q.ID); err != nil {
				t.Fatalf("generated ID rejected: %v", err)
			}
			if seen[q.ID] {
				t.Fatalf("call %d repeats query ID %q, which would merge two series", i, q.ID)
			}
			seen[q.ID] = true
		}
	}
}

// A result for a query nobody asked is discarded loudly, never routed by
// position.
func TestCollectDiscardsUnknownQueryIDs(t *testing.T) {
	f := &Fixture{
		InventoryPages: []DescribeInstancesOutput{{Reservations: []Reservation{{Instances: []InstanceRecord{
			{InstanceID: "i-1", InstanceType: "m5.large", Architecture: "x86_64", State: "running"},
		}}}}},
		Metrics: []RecordedSeries{{InstanceID: "i-1", Metric: MetricCPUUtilization,
			Points: []Point{{At: testNow, Value: 5}}}},
	}
	stub := &renamingMetrics{inner: f}
	c, _ := NewCollector(f, stub, CollectorConfig{Window: time.Hour})
	snap, err := c.Collect(context.Background(), testNow)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !strings.Contains(strings.Join(snap.Warnings, " "), "unknown query id") {
		t.Errorf("an unroutable result was dropped silently: %v", snap.Warnings)
	}
	if !snap.Stale {
		t.Error("dropping every answer should leave the snapshot stale")
	}
}

// renamingMetrics rewrites result IDs, simulating an API (or a bug) that
// returns something the collector cannot route.
type renamingMetrics struct{ inner MetricsAPI }

func (r *renamingMetrics) GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	out, err := r.inner.GetMetricData(ctx, in)
	if err != nil || out == nil {
		return out, err
	}
	for i := range out.Results {
		out.Results[i].ID = "zzz" + out.Results[i].ID
	}
	return out, nil
}

// Mismatched timestamp/value lengths are corruption, not data.
func TestCollectRejectsMalformedResults(t *testing.T) {
	f := &Fixture{InventoryPages: []DescribeInstancesOutput{{Reservations: []Reservation{{
		Instances: []InstanceRecord{{InstanceID: "i-1", InstanceType: "m5.large", State: "running"}}}}}}}
	c, _ := NewCollector(f, &malformedMetrics{inner: f}, CollectorConfig{Window: time.Hour})
	if _, err := c.Collect(context.Background(), testNow); err == nil {
		t.Fatal("a result with mismatched timestamps and values must fail loudly")
	}
}

type malformedMetrics struct{ inner MetricsAPI }

func (m *malformedMetrics) GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	out, err := m.inner.GetMetricData(ctx, in)
	if err != nil || out == nil {
		return out, err
	}
	for i := range out.Results {
		out.Results[i].Timestamps = []time.Time{testNow}
		out.Results[i].Values = nil
	}
	return out, nil
}
