package provider

// As in rdsapi_test.go: a fake SDK client, no credential, no socket.

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	krds "github.com/agenticode/kilter/pkg/rds"
)

// ---- the fake cloudwatch: client ----

type fakeCW struct {
	// results is what to answer, keyed by "<DBInstanceIdentifier>/<MetricName>".
	results map[string][]krds.Point
	// status overrides the status code on every result; the zero value means
	// "CloudWatch said nothing", which is the case this adapter resolves.
	status cwtypes.StatusCode
	// dropIDs omits the Id from every result.
	dropIDs bool
	// pages, when > 1, answers with that many pages of the same results.
	pages int
	// messages is CloudWatch's out-of-band complaint channel.
	messages []cwtypes.MessageData
	err      error

	mu       sync.Mutex
	requests []*cloudwatch.GetMetricDataInput
	deadline []bool
}

func (f *fakeCW) GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput,
	_ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {

	f.mu.Lock()
	_, ok := ctx.Deadline()
	f.deadline = append(f.deadline, ok)
	f.requests = append(f.requests, in)
	page := len(f.requests)
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	out := &cloudwatch.GetMetricDataOutput{Messages: f.messages}
	if f.pages > 1 && page < f.pages {
		tok := "page"
		out.NextToken = &tok
	}
	for _, q := range in.MetricDataQueries {
		r := cwtypes.MetricDataResult{StatusCode: f.status}
		if !f.dropIDs {
			r.Id = q.Id
		}
		key := ""
		if q.MetricStat != nil && q.MetricStat.Metric != nil {
			for _, d := range q.MetricStat.Metric.Dimensions {
				if str(d.Name) == "DBInstanceIdentifier" {
					key = str(d.Value)
				}
			}
			key += "/" + str(q.MetricStat.Metric.MetricName)
		}
		for _, p := range f.results[key] {
			r.Timestamps = append(r.Timestamps, p.At)
			r.Values = append(r.Values, p.Value)
		}
		out.MetricDataResults = append(out.MetricDataResults, r)
	}
	return out, nil
}

func (f *fakeCW) lastRequest() *cloudwatch.GetMetricDataInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// ---- query construction ----

func TestGetMetricDataQueryIsBuiltFromTheSeamVerbatim(t *testing.T) {
	f := &fakeCW{status: cwtypes.StatusCodeComplete}
	a := newCloudWatchAPI(f, "us-east-1")
	start, end := refNow.Add(-time.Hour), refNow
	_, err := a.GetMetricData(context.Background(), &krds.GetMetricDataInput{
		Queries: []krds.MetricDataQuery{{
			ID:            "q0_0",
			Namespace:     krds.NamespaceRDS,
			MetricName:    krds.MetricCPUUtilization,
			Dimensions:    map[string]string{"DBInstanceIdentifier": "db-1"},
			PeriodSeconds: 60,
			Stat:          "Average",
		}},
		StartTime: start, EndTime: end,
	})
	if err != nil {
		t.Fatal(err)
	}
	in := f.lastRequest()
	if len(in.MetricDataQueries) != 1 {
		t.Fatalf("want 1 query, got %d", len(in.MetricDataQueries))
	}
	q := in.MetricDataQueries[0]
	if str(q.Id) != "q0_0" {
		t.Fatalf("Id = %q; pkg/rds routes results BY ID, not by label or position", str(q.Id))
	}
	if q.MetricStat == nil || q.MetricStat.Metric == nil {
		t.Fatal("a MetricStat query is required")
	}
	if got := str(q.MetricStat.Metric.Namespace); got != "AWS/RDS" {
		t.Fatalf("Namespace = %q", got)
	}
	if got := str(q.MetricStat.Metric.MetricName); got != krds.MetricCPUUtilization {
		t.Fatalf("MetricName = %q", got)
	}
	if got := i32(q.MetricStat.Period); got != 60 {
		t.Fatalf("Period = %d", got)
	}
	if got := str(q.MetricStat.Stat); got != "Average" {
		t.Fatalf("Stat = %q", got)
	}
	// Naming a unit FILTERS datapoints to the ones published with it, so a
	// wrong guess returns an empty series that reads as a quiet database.
	if q.MetricStat.Unit != "" {
		t.Fatalf("Unit must stay unset, got %q", q.MetricStat.Unit)
	}
	if q.ReturnData == nil || !*q.ReturnData {
		t.Fatal("ReturnData must be explicitly true")
	}
	if q.Expression != nil {
		t.Fatal("no metric math is issued by this adapter")
	}
	if in.StartTime == nil || !in.StartTime.Equal(start) || in.EndTime == nil || !in.EndTime.Equal(end) {
		t.Fatalf("window not propagated: %v..%v", in.StartTime, in.EndTime)
	}
	if in.NextToken != nil {
		t.Fatalf("an empty NextToken must be sent as unset, got %q", *in.NextToken)
	}
}

// TestDimensionsAreOrderedSoTwoIdenticalCollectionsIssueIdenticalRequests:
// Go randomizes map iteration, and an unordered dimension list makes two runs
// over the same account send two different requests, which defeats replay and
// diff.
func TestDimensionsAreOrderedSoTwoIdenticalCollectionsIssueIdenticalRequests(t *testing.T) {
	dims := map[string]string{
		"DBInstanceIdentifier": "db-1",
		"EngineName":           "postgres",
		"Az":                   "us-east-1a",
		"":                     "dropped",
	}
	var first []string
	for i := 0; i < 25; i++ {
		f := &fakeCW{status: cwtypes.StatusCodeComplete}
		a := newCloudWatchAPI(f, "us-east-1")
		if _, err := a.GetMetricData(context.Background(), &krds.GetMetricDataInput{
			Queries: []krds.MetricDataQuery{{
				ID: "q0_0", Namespace: krds.NamespaceRDS, MetricName: "CPUUtilization",
				Dimensions: dims, PeriodSeconds: 60, Stat: "Average",
			}},
			StartTime: refNow.Add(-time.Hour), EndTime: refNow,
		}); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, d := range f.lastRequest().MetricDataQueries[0].MetricStat.Metric.Dimensions {
			names = append(names, str(d.Name)+"="+str(d.Value))
		}
		if i == 0 {
			first = names
			continue
		}
		if !reflect.DeepEqual(names, first) {
			t.Fatalf("dimension order is not stable: %v vs %v", names, first)
		}
	}
	want := []string{"Az=us-east-1a", "DBInstanceIdentifier=db-1", "EngineName=postgres"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("dimensions = %v, want %v (a blank name is dropped, not sent)", first, want)
	}
}

// ---- the status decision ----

// TestUnsetStatusBecomesPartialNotComplete is the load-bearing test for this
// seam. pkg/rds reads an empty status as Complete:
//
//	ser.Status = r.StatusCode
//	if ser.Status == "" { ser.Status = StatusComplete }
//
// so passing "" through would turn "CloudWatch did not vouch for this series"
// into "this series is whole evidence" — which is what an idle verdict is made
// of.
func TestUnsetStatusBecomesPartialNotComplete(t *testing.T) {
	f := &fakeCW{status: ""} // CloudWatch said nothing
	a := newCloudWatchAPI(f, "us-east-1")
	out, err := a.GetMetricData(context.Background(), oneQuery())
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Results[0].StatusCode; got != krds.StatusPartialData {
		t.Fatalf("StatusCode = %q, want %q", got, krds.StatusPartialData)
	}
	if !hasNote(a.Notes(), "no status code") {
		t.Fatalf("the decision must be visible: %v", a.Notes())
	}

	// End to end: the series pkg/rds builds from it must be marked partial.
	series := collectOneSeries(t, "")
	if !series.Partial {
		t.Fatal("a series CloudWatch did not vouch for must be partial in the snapshot")
	}
}

func TestKnownStatusCodesPassThroughVerbatim(t *testing.T) {
	for _, sc := range []cwtypes.StatusCode{
		cwtypes.StatusCodeComplete,
		cwtypes.StatusCodePartialData,
		cwtypes.StatusCodeInternalError,
		cwtypes.StatusCodeForbidden,
	} {
		t.Run(string(sc), func(t *testing.T) {
			a := newCloudWatchAPI(&fakeCW{status: sc}, "us-east-1")
			out, err := a.GetMetricData(context.Background(), oneQuery())
			if err != nil {
				t.Fatal(err)
			}
			if got := out.Results[0].StatusCode; got != string(sc) {
				t.Fatalf("StatusCode = %q, want %q", got, sc)
			}
			if len(a.Notes()) != 0 {
				t.Fatalf("a vouched-for status needs no note: %v", a.Notes())
			}
			// Everything but Complete makes the series partial in pkg/rds.
			series := collectOneSeries(t, sc)
			if want := sc != cwtypes.StatusCodeComplete; series.Partial != want {
				t.Fatalf("Partial = %v for status %q, want %v", series.Partial, sc, want)
			}
		})
	}
}

func TestResultWithNoIDIsNotedAndLeavesTheQueryTruncated(t *testing.T) {
	a := newCloudWatchAPI(&fakeCW{status: cwtypes.StatusCodeComplete, dropIDs: true}, "us-east-1")
	out, err := a.GetMetricData(context.Background(), oneQuery())
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].ID != "" {
		t.Fatal("an unidentified result must not be given an ID")
	}
	if !hasNote(a.Notes(), "result with no Id") {
		t.Fatalf("notes = %v", a.Notes())
	}
	// pkg/rds iterates SLOTS, not results, so the query that was never
	// answered becomes a truncated series rather than an empty metric.
	snap := collectSnapshot(t, &fakeCW{status: cwtypes.StatusCodeComplete, dropIDs: true})
	for _, s := range snap.Targets[0].Series {
		if !s.Partial || s.Status != krds.StatusTruncated {
			t.Fatalf("%s: an unanswered query must be truncated, got %+v", s.Metric, s)
		}
	}
}

// ---- window, queries, tokens ----

func TestEmptyQueryListMakesNoCall(t *testing.T) {
	f := &fakeCW{}
	a := newCloudWatchAPI(f, "us-east-1")
	out, err := a.GetMetricData(context.Background(), &krds.GetMetricDataInput{
		StartTime: refNow.Add(-time.Hour), EndTime: refNow,
	})
	if err != nil || out == nil || len(out.Results) != 0 {
		t.Fatalf("an empty query list must answer empty without a call: %v %v", out, err)
	}
	if len(f.requests) != 0 {
		t.Fatal("CloudWatch rejects an empty query list; no call should have been made")
	}
	if _, err := a.GetMetricData(context.Background(), nil); err != nil {
		t.Fatalf("a nil input must be handled, got %v", err)
	}
}

func TestInvalidWindowAndPeriodAreRefusedClientSide(t *testing.T) {
	f := &fakeCW{}
	a := newCloudWatchAPI(f, "us-east-1")
	q := []krds.MetricDataQuery{{ID: "q0_0", Namespace: krds.NamespaceRDS,
		MetricName: "CPUUtilization", PeriodSeconds: 60, Stat: "Average"}}
	for _, tc := range []struct {
		name string
		in   *krds.GetMetricDataInput
	}{
		{"zero start", &krds.GetMetricDataInput{Queries: q, EndTime: refNow}},
		{"zero end", &krds.GetMetricDataInput{Queries: q, StartTime: refNow}},
		{"inverted", &krds.GetMetricDataInput{Queries: q, StartTime: refNow, EndTime: refNow.Add(-time.Hour)}},
		{"empty window", &krds.GetMetricDataInput{Queries: q, StartTime: refNow, EndTime: refNow}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.GetMetricData(context.Background(), tc.in); err == nil {
				t.Fatal("want a client-side refusal")
			}
		})
	}
	bad := &krds.GetMetricDataInput{
		Queries:   []krds.MetricDataQuery{{ID: "q0_0", Namespace: krds.NamespaceRDS, MetricName: "X", Stat: "Average"}},
		StartTime: refNow.Add(-time.Hour), EndTime: refNow,
	}
	if _, err := a.GetMetricData(context.Background(), bad); err == nil {
		t.Fatal("a zero period must be refused")
	}
	if len(f.requests) != 0 {
		t.Fatalf("nothing should have reached AWS, got %d requests", len(f.requests))
	}
}

func TestNextTokenIsPropagatedInBothDirections(t *testing.T) {
	f := &fakeCW{status: cwtypes.StatusCodeComplete, pages: 2}
	a := newCloudWatchAPI(f, "us-east-1")
	out, err := a.GetMetricData(context.Background(), oneQuery())
	if err != nil {
		t.Fatal(err)
	}
	if out.NextToken == "" {
		t.Fatal("CloudWatch said there is more; the adapter must say so or the series truncates silently")
	}
	in := oneQuery()
	in.NextToken = out.NextToken
	if _, err := a.GetMetricData(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := f.lastRequest().NextToken; got == nil || *got != out.NextToken {
		t.Fatalf("inbound NextToken not sent: %v", got)
	}
}

func TestCloudWatchMessagesBecomeNotes(t *testing.T) {
	f := &fakeCW{
		status:   cwtypes.StatusCodeComplete,
		messages: []cwtypes.MessageData{{Code: sp("MaxMetricsExceeded"), Value: sp("too many metrics")}},
	}
	a := newCloudWatchAPI(f, "us-east-1")
	if _, err := a.GetMetricData(context.Background(), oneQuery()); err != nil {
		t.Fatal(err)
	}
	if !hasNote(a.Notes(), "MaxMetricsExceeded") {
		t.Fatalf("CloudWatch's out-of-band complaints have nowhere to go in the seam; "+
			"they must reach Notes(): %v", a.Notes())
	}
}

func TestMetricsCallCarriesADeadlineAndHonoursCancellation(t *testing.T) {
	f := &fakeCW{status: cwtypes.StatusCodeComplete}
	a := newCloudWatchAPI(f, "us-east-1")
	if _, err := a.GetMetricData(context.Background(), oneQuery()); err != nil {
		t.Fatal(err)
	}
	if len(f.deadline) != 1 || !f.deadline[0] {
		t.Fatal("GetMetricData was issued with no deadline")
	}
	a.SetCallTimeout(0)
	if a.callTimeout() != DefaultMetricsCallTimeout {
		t.Fatalf("timeout = %s, want the default", a.callTimeout())
	}

	blocked := newCloudWatchAPI(blockingCW{}, "us-east-1")
	blocked.SetCallTimeout(20 * time.Millisecond)
	if _, err := blocked.GetMetricData(context.Background(), oneQuery()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
}

type blockingCW struct{}

func (blockingCW) GetMetricData(ctx context.Context, _ *cloudwatch.GetMetricDataInput,
	_ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// ---- end to end, through the real collector ----

// TestLiveAdaptersDriveTheRealCollector is the whole point of this unit: the
// same collector cmd/ drives against --rds-fixture, driven instead by the two
// SDK adapters, producing a snapshot and a report.
func TestLiveAdaptersDriveTheRealCollector(t *testing.T) {
	inst := fullInstance()
	inst.DBInstanceIdentifier = sp("db-1")
	inst.DBInstanceArn = sp("arn:aws:rds:us-east-1:1:db:db-1")
	inst.TagList = nil // force the ListTagsForResource fallback

	rf := &fakeRDS{
		instances: []rdstypes.DBInstance{inst},
		tags: map[string][]rdstypes.Tag{
			"arn:aws:rds:us-east-1:1:db:db-1": {{Key: sp(krds.TagKilterMode), Value: sp("off")}},
		},
	}
	cw := &fakeCW{status: cwtypes.StatusCodeComplete, results: map[string][]krds.Point{
		"db-1/CPUUtilization": krds.SyntheticMetric(refNow.Add(-time.Hour), time.Minute, 60, 4),
	}}

	ra := newRDSAPI(rf, "us-east-1")
	ca := newCloudWatchAPI(cw, "us-east-1")
	cfg := krds.DefaultCollectorConfig(krds.Window{Start: refNow.Add(-time.Hour), End: refNow})
	cfg.Scope, cfg.Region = "123456789012/us-east-1", ra.Region()
	c, err := krds.NewCollector(ra, ca, ra, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(snap.Targets))
	}
	tgt := snap.Targets[0]
	if tgt.Instance.Identifier != "db-1" || tgt.Instance.Class != "db.r6i.xlarge" {
		t.Fatalf("instance did not survive the adapter: %+v", tgt.Instance)
	}
	if tgt.Instance.Region != "us-east-1" {
		t.Fatalf("Region = %q; it must be the adapter's region", tgt.Instance.Region)
	}
	// The tag fallback ran, so the guardrail is reachable.
	if tgt.Instance.Tags[krds.TagKilterMode] != "off" {
		t.Fatalf("tags = %v; the kilter.dev/mode guardrail is unreachable without them", tgt.Instance.Tags)
	}
	// Every collected metric got a series, and the one with data has it.
	if len(tgt.Series) != len(krds.CollectedMetrics()) {
		t.Fatalf("want %d series, got %d", len(krds.CollectedMetrics()), len(tgt.Series))
	}
	cpu, ok := tgt.SeriesFor(krds.MetricCPUUtilization)
	if !ok || len(cpu.Points) != 60 {
		t.Fatalf("CPU series did not survive: %+v", cpu)
	}
	if cpu.Partial {
		t.Fatal("a Complete series must not be marked partial")
	}

	// And the brain accepts it.
	d, err := krds.NewDomain(krds.Config{Scope: cfg.Scope, Region: cfg.Region, Rates: krds.DefaultRates()})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Observe(snap); err != nil {
		t.Fatal(err)
	}
	rep := d.Report(refNow, nil)
	if rep == nil || len(rep.Refusals()) == 0 {
		t.Fatal("the report's refusals are the product; there must be at least one")
	}
	if len(d.Recommend(refNow, nil)) != 0 {
		t.Fatal("this domain proposes nothing, always")
	}
}

// ---- helpers ----

func oneQuery() *krds.GetMetricDataInput {
	return &krds.GetMetricDataInput{
		Queries: []krds.MetricDataQuery{{
			ID: "q0_0", Namespace: krds.NamespaceRDS, MetricName: krds.MetricCPUUtilization,
			Dimensions:    map[string]string{"DBInstanceIdentifier": "db-1"},
			PeriodSeconds: 60, Stat: "Average",
		}},
		StartTime: refNow.Add(-time.Hour), EndTime: refNow,
	}
}

// collectSnapshot runs the real collector over one instance and the given
// CloudWatch fake.
func collectSnapshot(t *testing.T, cw *fakeCW) *krds.Snapshot {
	t.Helper()
	inst := fullInstance()
	inst.DBInstanceIdentifier = sp("db-1")
	inst.DBInstanceArn = sp("arn:aws:rds:us-east-1:1:db:db-1")
	ra := newRDSAPI(&fakeRDS{instances: []rdstypes.DBInstance{inst}}, "us-east-1")
	ca := newCloudWatchAPI(cw, "us-east-1")
	cfg := krds.DefaultCollectorConfig(krds.Window{Start: refNow.Add(-time.Hour), End: refNow})
	cfg.Region = "us-east-1"
	c, err := krds.NewCollector(ra, ca, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// collectOneSeries returns the CPU series pkg/rds builds when CloudWatch
// answers with the given status.
func collectOneSeries(t *testing.T, sc cwtypes.StatusCode) krds.Series {
	t.Helper()
	snap := collectSnapshot(t, &fakeCW{status: sc, results: map[string][]krds.Point{
		"db-1/CPUUtilization": krds.SyntheticMetric(refNow.Add(-time.Hour), time.Minute, 60, 4),
	}})
	s, ok := snap.Targets[0].SeriesFor(krds.MetricCPUUtilization)
	if !ok {
		t.Fatal("no CPU series")
	}
	return s
}
