package ecs

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func collect(t *testing.T, api *fakeAPI, met *fakeMetrics, mods ...func(*CollectorConfig)) *Snapshot {
	t.Helper()
	cfg := CollectorConfig{Cluster: testCluster, Scope: testScope}
	for _, m := range mods {
		m(&cfg)
	}
	c, err := NewCollector(api, met, cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	snap, err := c.Collect(context.Background(), testNow)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap == nil {
		t.Fatal("Collect returned a nil snapshot with a nil error")
	}
	return snap
}

// TestCollectorBuildsObservations walks the whole read path and checks that the
// denominator — the task definition's reservation — arrives with the
// percentages it belongs to.
func TestCollectorBuildsObservations(t *testing.T) {
	f := newFixture()
	snap := collect(t, newFakeAPI(f), newFakeMetrics(f))

	if len(snap.Services) != 1 {
		t.Fatalf("collected %d services, want 1", len(snap.Services))
	}
	o := snap.Services[0]
	if o.Ref.ID != TargetID(testCluster, testService) {
		t.Errorf("target ID %q", o.Ref.ID)
	}
	if o.Reserved.Reserved != (model.Resources{MilliCPU: 4000, MemoryBytes: 8 << 30}) {
		t.Fatalf("reservation %s, want 4000m/8GiB", o.Reserved.Reserved)
	}
	if o.Reserved.Revision != 7 || o.Reserved.ARN != testTDARN {
		t.Errorf("reservation carries revision %d / %q, want 7 / %q",
			o.Reserved.Revision, o.Reserved.ARN, testTDARN)
	}
	if o.CPUPercent.Len() != f.samples || o.MemPercent.Len() != f.samples {
		t.Fatalf("series lengths %d/%d, want %d", o.CPUPercent.Len(), o.MemPercent.Len(), f.samples)
	}
	if !o.CPUPercent.Complete() || !o.MemPercent.Complete() {
		t.Error("a complete response was recorded as partial")
	}
	if snap.Window.Span() != DefaultWindow {
		t.Errorf("window span %s, want %s", snap.Window.Span(), DefaultWindow)
	}
	if snap.Stale {
		t.Errorf("healthy collection marked stale: %s", snap.StaleReason)
	}

	// And the collected snapshot assesses exactly like a hand-built one.
	a := NewSizer(testConfig()).Assess(o, testNow, nil)
	if a.Proposal == nil || a.Proposal.Tier.String() != "2vCPU 4GB" {
		t.Fatalf("collected observation assessed to %v", a.Proposal)
	}
}

// TestCollectorRoutesMetricsByQueryID: results are matched by query ID, never by
// position, so a response that reorders them cannot cross-wire two services'
// utilization — which would put one service's percentages over another's
// reservation.
func TestCollectorRoutesMetricsByQueryID(t *testing.T) {
	a := newFixture(func(f *fixture) {
		f.service, f.tdARN = "alpha", "arn:aws:ecs:us-east-1:111122223333:task-definition/alpha:1"
		f.constCPUPct, f.constMem = 10, 10
	})
	b := newFixture(func(f *fixture) {
		f.service, f.tdARN = "beta", "arn:aws:ecs:us-east-1:111122223333:task-definition/beta:1"
		f.constCPUPct, f.constMem = 70, 70
	})
	met := newFakeMetrics(a, b)
	met.reverse = true // CloudWatch owes us no ordering
	snap := collect(t, newFakeAPI(a, b), met)

	if len(snap.Services) != 2 {
		t.Fatalf("collected %d services", len(snap.Services))
	}
	for _, o := range snap.Services {
		want := 10.0
		if o.Service.ServiceName == "beta" {
			want = 70.0
		}
		if got := o.CPUPercent.Max(); got != want {
			t.Errorf("%s got %v%% CPU, want %v%%: the results were cross-wired",
				o.Service.ServiceName, got, want)
		}
		if got := o.MemPercent.Max(); got != want {
			t.Errorf("%s got %v%% memory, want %v%%", o.Service.ServiceName, got, want)
		}
	}
	// Observations come out sorted by target ID regardless of API order.
	if snap.Services[0].Ref.ID > snap.Services[1].Ref.ID {
		t.Error("observations are not in canonical order")
	}
}

// TestCollectorMarksAMissingResultTruncated: GetMetricData returns one result
// per query even when the metric has no data, so a MISSING result is a fact
// about the response, not about the service — and the sizer refuses on it
// rather than reading it as "no usage".
func TestCollectorMarksAMissingResultTruncated(t *testing.T) {
	f := newFixture()
	met := newFakeMetrics(f)
	met.drop = map[string]bool{"mem0": true}
	snap := collect(t, newFakeAPI(f), met)

	o := snap.Services[0]
	if o.MemPercent.StatusCode != StatusTruncated {
		t.Fatalf("memory series status %q, want %q", o.MemPercent.StatusCode, StatusTruncated)
	}
	if o.CPUPercent.StatusCode != StatusComplete {
		t.Errorf("cpu series status %q, want %q", o.CPUPercent.StatusCode, StatusComplete)
	}
	if len(o.Notes) == 0 {
		t.Error("a truncated series was not noted")
	}
	a := NewSizer(testConfig()).Assess(o, testNow, nil)
	if a.Proposal != nil {
		t.Fatal("sized a service whose memory series never arrived")
	}
	if !hasSuppression(a, ReasonPartialMetrics) {
		t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonPartialMetrics)
	}
}

// TestCollectorPropagatesCloudWatchStatus.
func TestCollectorPropagatesCloudWatchStatus(t *testing.T) {
	f := newFixture()
	met := newFakeMetrics(f)
	met.status = map[string]string{"cpu0": StatusPartialData}
	snap := collect(t, newFakeAPI(f), met)
	if got := snap.Services[0].CPUPercent.StatusCode; got != StatusPartialData {
		t.Fatalf("status %q, want %q", got, StatusPartialData)
	}
}

// TestCollectorSkipsNonFargateServices: an EC2-launch-type service belongs to
// the node domain, and pricing it as a Fargate tier would be nonsense.
func TestCollectorSkipsNonFargateServices(t *testing.T) {
	ec2Svc := newFixture(func(f *fixture) { f.service, f.launchType = "onnodes", "EC2" })
	fg := newFixture()
	snap := collect(t, newFakeAPI(ec2Svc, fg), newFakeMetrics(ec2Svc, fg))
	if len(snap.Services) != 1 || snap.Services[0].Service.ServiceName != testService {
		t.Fatalf("collected %d services, want only the Fargate one", len(snap.Services))
	}

	// Unless asked for explicitly, in which case the sizer refuses it by name.
	snap = collect(t, newFakeAPI(ec2Svc, fg), newFakeMetrics(ec2Svc, fg),
		func(c *CollectorConfig) { c.IncludeNonFargate = true })
	if len(snap.Services) != 2 {
		t.Fatalf("collected %d services with IncludeNonFargate", len(snap.Services))
	}
}

// TestCollectorPaginatesAndBudgets.
func TestCollectorPaginatesAndBudgets(t *testing.T) {
	fs := []*fixture{
		newFixture(func(f *fixture) { f.service, f.tdARN = "s1", "td/s1:1" }),
		newFixture(func(f *fixture) { f.service, f.tdARN = "s2", "td/s2:1" }),
	}
	api := newFakeAPI(fs...)
	api.listPages = [][]string{{api.serviceARNs[0]}, {api.serviceARNs[1]}}
	snap := collect(t, api, newFakeMetrics(fs...))
	if len(snap.Services) != 2 {
		t.Fatalf("paginated listing produced %d services", len(snap.Services))
	}

	// A server that keeps handing back a token yields a stale snapshot, not an
	// unbounded loop.
	api = newFakeAPI(fs...)
	api.listPages = [][]string{{api.serviceARNs[0]}, {api.serviceARNs[1]}}
	snap = collect(t, api, newFakeMetrics(fs...), func(c *CollectorConfig) { c.MaxPages = 1 })
	if !snap.Stale || snap.StaleReason == "" {
		t.Fatal("an exhausted page budget did not mark the snapshot stale")
	}
}

// TestCollectorReportsDescribeFailures: a per-service failure degrades the
// snapshot and is named, never silently dropped.
func TestCollectorReportsDescribeFailures(t *testing.T) {
	f := newFixture()
	api := newFakeAPI(f)
	api.failures = []Failure{{ARN: "arn:...:service/prod/gone", Reason: "MISSING"}}
	snap := collect(t, api, newFakeMetrics(f))
	if !snap.Stale {
		t.Fatal("a describe failure did not mark the snapshot stale")
	}
	var named bool
	for _, w := range snap.Warnings {
		if strings.Contains(w, "MISSING") {
			named = true
		}
	}
	if !named {
		t.Fatalf("warnings %v do not name the failure", snap.Warnings)
	}
}

// TestCollectorSurfacesTransportErrors: a failed call is not evidence of an
// empty cluster.
func TestCollectorSurfacesTransportErrors(t *testing.T) {
	boom := errors.New("throttled")
	for _, tc := range []struct {
		name string
		mod  func(*fakeAPI, *fakeMetrics)
	}{
		{"list", func(a *fakeAPI, _ *fakeMetrics) { a.listErr = boom }},
		{"describe", func(a *fakeAPI, _ *fakeMetrics) { a.describeErr = boom }},
		{"task-definition", func(a *fakeAPI, _ *fakeMetrics) { a.tdErr = boom }},
		{"metrics", func(_ *fakeAPI, m *fakeMetrics) { m.err = boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			api, met := newFakeAPI(f), newFakeMetrics(f)
			tc.mod(api, met)
			c, err := NewCollector(api, met, CollectorConfig{Cluster: testCluster})
			if err != nil {
				t.Fatal(err)
			}
			snap, err := c.Collect(context.Background(), testNow)
			if err == nil {
				t.Fatalf("Collect swallowed a transport error and returned %d services", len(snap.Services))
			}
			if snap != nil {
				t.Error("Collect returned both a snapshot and an error")
			}
		})
	}
}

// TestCollectorClampsTheWindowToCloudWatchRetention: asking for more than 15
// days silently changes the resolution from 1-minute to coarser aggregates, so
// the window is clamped and the clamp is reported.
func TestCollectorClampsTheWindowToCloudWatchRetention(t *testing.T) {
	f := newFixture()
	snap := collect(t, newFakeAPI(f), newFakeMetrics(f),
		func(c *CollectorConfig) { c.Window = 90 * 24 * time.Hour })
	if snap.Window.Span() != MaxWindow {
		t.Fatalf("window span %s, want the %s clamp", snap.Window.Span(), MaxWindow)
	}
	if len(snap.Warnings) == 0 || !strings.Contains(snap.Warnings[0], "clamped") {
		t.Fatalf("warnings %v do not report the clamp", snap.Warnings)
	}
}

// TestCollectorRequiresSeamsAndAClock.
func TestCollectorRequiresSeamsAndAClock(t *testing.T) {
	f := newFixture()
	api, met := newFakeAPI(f), newFakeMetrics(f)
	cfg := CollectorConfig{Cluster: testCluster}
	if _, err := NewCollector(nil, met, cfg); err == nil {
		t.Error("built a collector with no inventory seam")
	}
	if _, err := NewCollector(api, nil, cfg); err == nil {
		t.Error("built a collector with no metrics seam")
	}
	if _, err := NewCollector(api, met, CollectorConfig{}); err == nil {
		t.Error("built a collector with no cluster")
	}
	if _, err := NewCollector(api, met, CollectorConfig{Cluster: testCluster, Window: -time.Hour}); err == nil {
		t.Error("built a collector with a negative window")
	}
	if _, err := NewCollector(api, met, CollectorConfig{Cluster: testCluster,
		MaxSeriesPerCall: MaxSeriesPerCall + 1}); err == nil {
		t.Error("built a collector over the GetMetricData query limit")
	}
	c, err := NewCollector(api, met, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background(), time.Time{}); err == nil {
		t.Error("collected without a caller-supplied now: this package has no clock")
	}
	if c.Domain() != Kind {
		t.Errorf("Domain() = %q, want %q", c.Domain(), Kind)
	}
}

// TestCollectorQueriesTheDefaultECSMetrics pins what is actually asked for:
// AWS/ECS CPUUtilization and MemoryUtilization, dimensioned by cluster and
// service, at ECS's free 1-minute publication period.
func TestCollectorQueriesTheDefaultECSMetrics(t *testing.T) {
	f := newFixture()
	var seen []MetricDataQuery
	met := &recordingMetrics{inner: newFakeMetrics(f), seen: &seen}
	c, err := NewCollector(newFakeAPI(f), met, CollectorConfig{Cluster: testCluster})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background(), testNow); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("issued %d queries for one service, want 2", len(seen))
	}
	names := map[string]bool{}
	for _, q := range seen {
		names[q.MetricName] = true
		if q.Namespace != Namespace {
			t.Errorf("namespace %q, want %q", q.Namespace, Namespace)
		}
		if q.PeriodSeconds != PeriodSeconds {
			t.Errorf("period %d, want %d", q.PeriodSeconds, PeriodSeconds)
		}
		if q.Dimensions[DimClusterName] != testCluster || q.Dimensions[DimServiceName] != testService {
			t.Errorf("dimensions %v", q.Dimensions)
		}
		if q.Stat != "Average" {
			t.Errorf("stat %q", q.Stat)
		}
	}
	if !names[MetricCPUUtilization] || !names[MetricMemoryUtilization] {
		t.Fatalf("queried %v, want ECS's two default service metrics", names)
	}
}

// recordingMetrics captures the queries the collector builds.
type recordingMetrics struct {
	inner *fakeMetrics
	seen  *[]MetricDataQuery
}

func (r *recordingMetrics) GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	*r.seen = append(*r.seen, in.Queries...)
	return r.inner.GetMetricData(ctx, in)
}

// TestTargetIDRoundTrips.
func TestTargetIDRoundTrips(t *testing.T) {
	c, s, err := ParseTargetID(TargetID("prod", "web"))
	if err != nil || c != "prod" || s != "web" {
		t.Fatalf("ParseTargetID = %q, %q, %v", c, s, err)
	}
	for _, bad := range []string{"", "prod", "/web", "prod/"} {
		if _, _, err := ParseTargetID(bad); err == nil {
			t.Errorf("ParseTargetID(%q) accepted a malformed ID", bad)
		}
	}
	if got := arnTail("arn:aws:ecs:us-east-1:1:cluster/prod"); got != "prod" {
		t.Errorf("arnTail = %q", got)
	}
	if got := arnTail("prod"); got != "prod" {
		t.Errorf("arnTail of a bare name = %q", got)
	}
}

// TestSeriesDropsGarbageDatapoints: NaN, absurd values and unstamped datapoints
// are not evidence, and a percentile taken over them would not be either.
func TestSeriesDropsGarbageDatapoints(t *testing.T) {
	s := Series{
		Timestamps: []time.Time{testNow.Add(-2 * time.Minute), {}, testNow.Add(-time.Minute), testNow},
		Values:     []float64{10, 20, math.NaN(), 30},
	}
	s.sortAscending()
	if s.Len() != 2 || s.Values[0] != 10 || s.Values[1] != 30 {
		t.Fatalf("sortAscending kept %v", s.Values)
	}
	if len(s.Timestamps) != len(s.Values) {
		t.Fatal("timestamps and values are no longer parallel")
	}
}
