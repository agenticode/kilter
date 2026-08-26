package ebs

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

func sampleValues(snap *domain.Snapshot, volumeID, metric string) []float64 {
	var out []float64
	for _, s := range snap.Samples {
		if s.Ref.ID == volumeID && s.Metric == metric {
			out = append(out, s.Value)
		}
	}
	return out
}

func newTestCollector(t *testing.T, f *Fixture, cfg CollectorConfig) *Collector {
	t.Helper()
	c, err := NewCollector(f, f, cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c
}

// TestCollectorDecodesCounters pins the one arithmetic the collector does:
// CloudWatch publishes SUMS per period, and a brain that has to know a period
// to read a sample will one day forget.
func TestCollectorDecodesCounters(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	start := base
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-1", 4000)},
		measured("vol-1", start, 20, 4000, 100))

	c := newTestCollector(t, f, CollectorConfig{Scope: "acct/us-east-1", CollectBurstBalance: true})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap.Domain != Kind || snap.Scope != "acct/us-east-1" {
		t.Errorf("snapshot header = %s/%s", snap.Domain, snap.Scope)
	}
	iops := sampleValues(snap, "vol-1", SampleIOPS)
	if len(iops) != 20 {
		t.Fatalf("got %d IOPS samples, want 20", len(iops))
	}
	for _, v := range iops {
		if math.Abs(v-4000) > 1e-6 {
			t.Fatalf("IOPS sample = %v, want 4000 (read+write ops ÷ period)", v)
		}
	}
	tput := sampleValues(snap, "vol-1", SampleThroughputMBps)
	for _, v := range tput {
		if math.Abs(v-100) > 1e-6 {
			t.Fatalf("throughput sample = %v MiB/s, want 100", v)
		}
	}
	for _, s := range snap.Samples {
		if s.WindowSeconds != PeriodSeconds {
			t.Fatalf("sample window = %ds, want %d", s.WindowSeconds, PeriodSeconds)
		}
	}

	// The request contract: EBS namespace, VolumeId dimension, 5-minute
	// period, Sum statistic, and a window ending at now.
	if len(f.MetricRequests) != 1 {
		t.Fatalf("got %d metric calls, want 1", len(f.MetricRequests))
	}
	req := f.MetricRequests[0]
	if !req.EndTime.Equal(clock.Now()) || req.StartTime != clock.Now().Add(-DefaultWindow) {
		t.Errorf("metric window %s..%s, want a %s lookback ending at now",
			req.StartTime, req.EndTime, DefaultWindow)
	}
	if len(req.Queries) != 5 {
		t.Fatalf("got %d queries, want 5 (4 counters + burst balance)", len(req.Queries))
	}
	for _, q := range req.Queries {
		if q.Namespace != NamespaceEBS || q.Dimensions["VolumeId"] != "vol-1" || q.PeriodSeconds != PeriodSeconds {
			t.Errorf("query %+v is not an AWS/EBS 5-minute VolumeId query", q)
		}
		if q.MetricName == MetricBurstBalance && q.Stat != StatAverage {
			t.Errorf("burst balance requested as %q, want %q", q.Stat, StatAverage)
		}
		if strings.HasSuffix(q.MetricName, "Ops") && q.Stat != StatSum {
			t.Errorf("%s requested as %q, want %q", q.MetricName, q.Stat, StatSum)
		}
	}

	// The spec the domain reads volume identity from.
	tgt := snap.Targets[0]
	if tgt.Spec.Attr(AttrVolumeType) != VolumeTypeGP2 || tgt.Spec.Attr(AttrSizeGiB) != "4000" {
		t.Errorf("target spec = %+v", tgt.Spec.Attrs)
	}
	if tgt.Spec.Attr(AttrAttachmentState) != AttachmentAttached {
		t.Errorf("attachment state = %q", tgt.Spec.Attr(AttrAttachmentState))
	}
}

// TestCollectorMetersOnlyActionableTypes: a query for a volume nothing can be
// done about is a bill for nothing, and the volume is still reported — with a
// declared blind spot, so the domain refuses for a stated reason.
func TestCollectorMetersOnlyActionableTypes(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{
		gp2Volume("vol-gp2", 500),
		gp3Volume("vol-gp3", 500, 3000, 125),
		{VolumeID: "vol-io2", VolumeType: VolumeTypeIO2, SizeGiB: 500, State: VolumeStateInUse},
	}, measured("vol-gp2", base, 20, 500, 50))

	c := newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(snap.Targets))
	}
	for _, q := range f.MetricRequests[0].Queries {
		if id := q.Dimensions["VolumeId"]; id != "vol-gp2" {
			t.Errorf("metrics requested for %s, which this unit cannot act on", id)
		}
	}
	for _, tg := range snap.Targets {
		blind := strings.Join(tg.Blind, ",")
		if tg.Ref.ID == "vol-gp2" && blind != "" {
			t.Errorf("metered volume declared blind: %q", blind)
		}
		if tg.Ref.ID != "vol-gp2" && !strings.Contains(blind, SampleIOPS) {
			t.Errorf("%s: blind = %q, want an iops blind spot", tg.Ref.ID, blind)
		}
	}
}

func TestCollectorPagination(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := &Fixture{
		Now: clock.Now,
		InventoryPages: []DescribeVolumesOutput{
			{Volumes: []VolumeRecord{gp2Volume("vol-b", 100)}},
			{Volumes: []VolumeRecord{gp2Volume("vol-a", 200)}},
		},
	}
	c := newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(snap.Targets))
	}
	// Sorted by volume ID, not by page order: query IDs are positional, so a
	// stable target order is what makes the request reproducible.
	if snap.Targets[0].Ref.ID != "vol-a" || snap.Targets[1].Ref.ID != "vol-b" {
		t.Errorf("targets not in ID order: %s, %s", snap.Targets[0].Ref.ID, snap.Targets[1].Ref.ID)
	}
	if snap.Stale {
		t.Errorf("clean pagination marked stale: %s", snap.StaleReason)
	}
}

func TestCollectorSurvivesBrokenPager(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := &Fixture{
		Now:                  clock.Now,
		RepeatInventoryToken: true,
		InventoryPages: []DescribeVolumesOutput{
			{Volumes: []VolumeRecord{gp2Volume("vol-a", 100)}},
		},
	}
	c := newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !snap.Stale || !strings.Contains(snap.StaleReason, "repeating") {
		t.Errorf("broken pager not reported: stale=%v reason=%q", snap.Stale, snap.StaleReason)
	}
}

func TestCollectorFailureModes(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))

	// A failure on the FIRST inventory call is an error: a failed call is not
	// evidence of an empty account.
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 100)})
	f.InventoryFailAt = 1
	c := newTestCollector(t, f, CollectorConfig{})
	if _, err := c.Collect(t.Context(), clock.Now()); err == nil {
		t.Fatal("first-call inventory failure did not error")
	}

	// A metrics failure is not: the volumes are still reported, blind and
	// stale, and the domain refuses on the blindness rather than acting.
	f = newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 100)}, measured("vol-a", base, 20, 100, 10))
	f.MetricsFailAt = 1
	c = newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("metrics failure broke the collection: %v", err)
	}
	if !snap.Stale || len(snap.Samples) != 0 {
		t.Errorf("metrics failure: stale=%v samples=%d, want stale with no samples", snap.Stale, len(snap.Samples))
	}
	if !strings.Contains(snap.Targets[0].Blind[0], SampleIOPS) {
		t.Errorf("blind spots = %v, want an iops entry", snap.Targets[0].Blind)
	}
}

// TestCollectorTruncatedResponse: a query CloudWatch did not answer is a fact
// about the response, not about the metric. Reading silence as "no I/O" is how
// a busy volume gets converted to a baseline gp3.
func TestCollectorTruncatedResponse(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 100)}, measured("vol-a", base, 20, 100, 10))
	f.TruncateResultsAt = 2 // answers read ops and write ops, drops the byte counters
	c := newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !snap.Stale {
		t.Error("truncated response did not mark the snapshot stale")
	}
	if got := sampleValues(snap, "vol-a", SampleThroughputMBps); len(got) != 0 {
		t.Errorf("got %d throughput samples from a truncated response, want 0", len(got))
	}
	if len(sampleValues(snap, "vol-a", SampleIOPS)) == 0 {
		t.Error("the answered half of the response was discarded too")
	}
}

func TestCollectorEmptyAccount(t *testing.T) {
	clock := newClock(base)
	f := &Fixture{Now: clock.Now}
	c := newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if snap == nil || len(snap.Targets) != 0 || snap.Stale {
		t.Errorf("empty account produced %+v", snap)
	}
	if len(f.MetricRequests) != 0 {
		t.Errorf("empty account still called GetMetricData %d time(s)", len(f.MetricRequests))
	}
}

func TestCollectorNeedsCallerClock(t *testing.T) {
	f := &Fixture{}
	c := newTestCollector(t, f, CollectorConfig{})
	if _, err := c.Collect(t.Context(), time.Time{}); err == nil {
		t.Fatal("a zero now was accepted; this package must have no clock of its own")
	}
	if _, err := NewDomainCollector(c, nil); err == nil {
		t.Fatal("NewDomainCollector accepted a nil clock")
	}
	dc, err := NewDomainCollector(c, func() time.Time { return base })
	if err != nil {
		t.Fatalf("NewDomainCollector: %v", err)
	}
	if dc.Domain() != Kind {
		t.Errorf("adapter domain = %q, want %q", dc.Domain(), Kind)
	}
	if _, err := dc.Collect(t.Context()); err != nil {
		t.Fatalf("adapter Collect: %v", err)
	}
}

func TestCollectorRejectsBadWiring(t *testing.T) {
	f := &Fixture{}
	if _, err := NewCollector(nil, f, CollectorConfig{}); err == nil {
		t.Error("nil inventory seam accepted")
	}
	if _, err := NewCollector(f, nil, CollectorConfig{}); err == nil {
		t.Error("nil metrics seam accepted")
	}
	if _, err := NewCollector(f, f, CollectorConfig{Window: -time.Hour}); err == nil {
		t.Error("negative window accepted")
	}
	if _, err := NewCollector(f, f, CollectorConfig{MaxSeriesPerCall: MaxSeriesPerCall + 1}); err == nil {
		t.Error("over-limit batch size accepted")
	}
}

// TestCollectorReportsModifications: the in-progress and cooldown refusals are
// only as good as the evidence behind them.
func TestCollectorReportsModifications(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 500)}, measured("vol-a", base, 20, 100, 10))
	f.ModificationPages = []DescribeVolumesModificationsOutput{{Modifications: []VolumeModification{
		{VolumeID: "vol-a", ModificationState: ModificationCompleted,
			StartTime: base.Add(40 * time.Hour), EndTime: base.Add(44 * time.Hour)},
		{VolumeID: "vol-a", ModificationState: ModificationOptimizing, StartTime: base.Add(46 * time.Hour)},
	}}}
	c := newTestCollector(t, f, CollectorConfig{})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	spec := snap.Targets[0].Spec
	// The in-flight record wins over the newer completed one: "a change is
	// happening now" outranks "a change happened".
	if got := spec.Attr(AttrModificationState); got != ModificationOptimizing {
		t.Errorf("modification state = %q, want %q", got, ModificationOptimizing)
	}
}

// TestCollectorBatchesQueries pins that the batch size is honoured, because
// GetMetricData rejects an over-sized request outright.
func TestCollectorBatchesQueries(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	var vols []VolumeRecord
	var series []RecordedSeries
	for _, id := range []string{"vol-1", "vol-2", "vol-3"} {
		vols = append(vols, gp2Volume(id, 500))
		series = append(series, measured(id, base, 20, 100, 10)...)
	}
	f := newFixture(clock, vols, series)
	c := newTestCollector(t, f, CollectorConfig{MaxSeriesPerCall: 4})
	snap, err := c.Collect(t.Context(), clock.Now())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(f.MetricRequests) != 3 {
		t.Fatalf("12 queries at 4 per call took %d calls, want 3", len(f.MetricRequests))
	}
	for _, r := range f.MetricRequests {
		if len(r.Queries) > 4 {
			t.Fatalf("batch of %d exceeds the configured 4", len(r.Queries))
		}
	}
	if len(sampleValues(snap, "vol-3", SampleIOPS)) != 20 {
		t.Error("the last batch's samples went missing")
	}
}
