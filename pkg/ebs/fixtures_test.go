package ebs

import (
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Test scaffolding. Every helper here builds RECORDED data and drives the
// production paths with it; no test reaches into unexported decision logic.

// fakeClock is the caller-supplied clock this package refuses to live without.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// base is the instant every test builds from. Fixed, so nothing depends on
// when the suite runs.
var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// gp2Volume builds an in-use gp2 volume with its size-derived IOPS, the way
// DescribeVolumes reports one.
func gp2Volume(id string, sizeGiB int64, tags ...Tag) VolumeRecord {
	return VolumeRecord{
		VolumeID:         id,
		VolumeType:       VolumeTypeGP2,
		SizeGiB:          sizeGiB,
		IOPS:             GP2PerformanceFor(sizeGiB).BaselineIOPS,
		State:            VolumeStateInUse,
		AvailabilityZone: "us-east-1a",
		Attachments: []VolumeAttachment{
			{InstanceID: "i-" + id, Device: "/dev/xvdb", State: AttachmentAttached},
		},
		Tags: tags,
	}
}

// gp3Volume builds an in-use gp3 volume.
func gp3Volume(id string, sizeGiB int64, iops, tput int32) VolumeRecord {
	v := gp2Volume(id, sizeGiB)
	v.VolumeType, v.IOPS, v.ThroughputMBps = VolumeTypeGP3, iops, tput
	return v
}

// countSeries records a CloudWatch counter series: n datapoints, one per
// publication period, each holding the SUM over that period. perSecond is the
// rate the series should decode to.
func countSeries(volumeID, metric string, start time.Time, n int, perSecond float64) RecordedSeries {
	out := RecordedSeries{VolumeID: volumeID, Metric: metric}
	for i := 0; i < n; i++ {
		out.Points = append(out.Points, Point{
			At:    start.Add(time.Duration(i) * time.Duration(PeriodSeconds) * time.Second),
			Value: perSecond * float64(PeriodSeconds),
		})
	}
	return out
}

// measured records the four counter series that decode to iops and mbps, split
// between reads and writes the way a real volume reports them.
func measured(volumeID string, start time.Time, n int, iops, mbps float64) []RecordedSeries {
	return []RecordedSeries{
		countSeries(volumeID, MetricVolumeReadOps, start, n, iops*0.6),
		countSeries(volumeID, MetricVolumeWriteOps, start, n, iops*0.4),
		countSeries(volumeID, MetricVolumeReadBytes, start, n, mbps*MiB*0.7),
		countSeries(volumeID, MetricVolumeWriteBytes, start, n, mbps*MiB*0.3),
	}
}

// newFixture assembles a one-page fixture.
func newFixture(clock *fakeClock, vols []VolumeRecord, metrics ...[]RecordedSeries) *Fixture {
	f := &Fixture{
		InventoryPages: []DescribeVolumesOutput{{Volumes: vols}},
		Now:            clock.Now,
	}
	for _, m := range metrics {
		f.Metrics = append(f.Metrics, m...)
	}
	return f
}

// collectInto runs the production collector over a fixture and folds the
// snapshot into a domain, which is what every domain-level test starts from.
func collectInto(t *testing.T, d *Domain, f *Fixture, now time.Time) {
	t.Helper()
	c, err := NewCollector(f, f, CollectorConfig{Scope: "123456789012/us-east-1", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	snap, err := c.Collect(t.Context(), now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := d.Learn(snap); err != nil {
		t.Fatalf("Learn: %v", err)
	}
}

// testConfig is DefaultConfig with the evidence gates lowered so a test does
// not have to record a week of datapoints to exercise anything else. The gates
// themselves are tested with the shipped defaults.
func testConfig() Config {
	c := DefaultConfig()
	c.Scope = "123456789012/us-east-1"
	c.Region = "us-east-1"
	c.MinSamples = 12
	c.MinWindow = time.Hour
	c.ActuationAvailable = true
	return c
}

func newDomain(t *testing.T, cfg Config) *Domain {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// only returns the single assessment for a volume, failing if absent.
func only(t *testing.T, rep Report, volumeID string) Assessment {
	t.Helper()
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invalid: %v", err)
	}
	for _, a := range rep.Assessments {
		if a.Ref.ID == volumeID {
			return a
		}
	}
	t.Fatalf("no assessment for %s in %d assessment(s)", volumeID, len(rep.Assessments))
	return Assessment{}
}

// wantRefusal asserts a volume was refused with a specific code.
func wantRefusal(t *testing.T, rep Report, volumeID, code string) Assessment {
	t.Helper()
	a := only(t, rep, volumeID)
	switch {
	case a.Refusal == nil:
		t.Fatalf("%s: expected refusal %q, got a proposal: %s", volumeID, code, a.Recommendation.Reason)
	case a.Refusal.Code != code:
		t.Fatalf("%s: refusal code %q, want %q (%s)", volumeID, a.Refusal.Code, code, a.Refusal.Reason)
	}
	return a
}

// wantProposal asserts a volume was proposed for conversion.
func wantProposal(t *testing.T, rep Report, volumeID string) (Assessment, domain.Recommendation) {
	t.Helper()
	a := only(t, rep, volumeID)
	if a.Recommendation == nil {
		t.Fatalf("%s: expected a proposal, got refusal %v", volumeID, a.Refusal)
	}
	return a, *a.Recommendation
}
