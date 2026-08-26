package ec2

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Platform normalization feeds the commitment waterfall: Reserved Instance
// size flexibility applies only to Linux/UNIX on default tenancy, so a
// mislabelled platform changes the bill, not just a string in a report.
func TestPlatformAndArchNormalization(t *testing.T) {
	for _, tc := range []struct{ platform, details, want string }{
		{"", "", "Linux/UNIX"},
		{"", "Linux/UNIX", "Linux/UNIX"},
		{"windows", "", "Windows"},
		{"", "Windows with SQL Server Standard", "Windows"},
		{"", "Red Hat Enterprise Linux", "Red Hat Enterprise Linux"},
		{"", "RHEL with HA", "Red Hat Enterprise Linux"},
		{"", "SUSE Linux", "SUSE Linux"},
	} {
		if got := normalizePlatform(tc.platform, tc.details); got != tc.want {
			t.Errorf("normalizePlatform(%q,%q) = %q, want %q", tc.platform, tc.details, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"x86_64", "amd64"}, {"arm64", "arm64"}, {"aarch64", "arm64"},
		{"x86_64_mac", "amd64"}, {"", ""}, {"sparc", ""},
	} {
		if got := normalizeArch(tc.in); got != tc.want {
			t.Errorf("normalizeArch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A stopped instance bills no instance-hour and has no current CPU; collecting
// one is opt-in.
func TestCollectorStateFilter(t *testing.T) {
	insts := []InstanceRecord{
		rec("i-run", "m5.large"),
		func() InstanceRecord { r := rec("i-stop", "m5.large"); r.State = "stopped"; return r }(),
	}
	f := &Fixture{InventoryPages: []DescribeInstancesOutput{
		{Reservations: []Reservation{{Instances: insts}}}}}
	snap := mustCollect(t, f, CollectorConfig{Window: time.Hour})
	if len(snap.Targets) != 1 || snap.Targets[0].Ref.ID != "i-run" {
		t.Fatalf("default state filter kept %d targets", len(snap.Targets))
	}

	g := &Fixture{InventoryPages: f.InventoryPages}
	snap2 := mustCollect(t, g, CollectorConfig{Window: time.Hour, IncludeStates: []string{"Running", "stopped"}})
	if len(snap2.Targets) != 2 {
		t.Fatalf("explicit state filter kept %d targets, want 2", len(snap2.Targets))
	}
}

// A fixture must survive being written out and read back, so a recording
// captured from a real account can be committed as testdata.
func TestFixtureRoundTrip(t *testing.T) {
	src := mustFixture(t, "testdata/account-paginated.json")
	var buf bytes.Buffer
	if err := WriteFixture(&buf, src); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := LoadFixture(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	a := mustCollect(t, src, CollectorConfig{Window: 2 * time.Hour})
	b := mustCollect(t, back, CollectorConfig{Window: 2 * time.Hour})
	if len(a.Targets) != len(b.Targets) {
		t.Fatalf("round trip changed the account: %d vs %d targets", len(a.Targets), len(b.Targets))
	}

	SortMetrics(back.Metrics)
	for i := 1; i < len(back.Metrics); i++ {
		p, q := back.Metrics[i-1], back.Metrics[i]
		if p.InstanceID > q.InstanceID || (p.InstanceID == q.InstanceID && p.Metric > q.Metric) {
			t.Fatalf("SortMetrics left %v before %v", p.InstanceID+"/"+p.Metric, q.InstanceID+"/"+q.Metric)
		}
	}
}

func TestFixtureRejectsMalformedRecordings(t *testing.T) {
	for _, bad := range []string{
		`{"metrics":[{"metric":"CPUUtilization"}]}`,
		`{"metrics":[{"instanceId":"i-1","metric":"m"},{"instanceId":"i-1","metric":"m"}]}`,
		`{"metricPageSize":-1}`,
		`{"unknownField":1}`,
		`{"metrics":[{"instanceId":"i-1","metric":"m","points":[{"at":"2026-08-20T01:00:00Z","value":1},` +
			`{"at":"2026-08-20T00:00:00Z","value":2}]}]}`,
	} {
		if _, err := LoadFixture(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted a malformed fixture: %s", bad)
		}
	}
	if _, err := LoadFixtureFile("testdata/does-not-exist.json"); err == nil {
		t.Error("a missing fixture file must fail")
	}
}

func TestReportAndAssessmentAccessors(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-1", "r5.large"), rec("i-2", "m5.2xlarge", tag(TagKilterMode, "off"))},
		[]RecordedSeries{
			series("i-1", MetricCPUUtilization, basic, 4, 6, 5),
			series("i-1", memAgent, basic, 15, 17),
			series("i-2", MetricCPUUtilization, basic, 4, 6, 5),
		},
	)
	rep := assess(t, snap, nil)

	if got := len(rep.Proposals()); got != 1 {
		t.Fatalf("Proposals() returned %d, want 1", got)
	}
	a, ok := rep.For("i-1")
	if !ok || a.Refused() {
		t.Fatalf("For(i-1): ok=%v refused=%v", ok, a.Refused())
	}
	if _, ok := rep.For("i-nope"); ok {
		t.Error("For returned an assessment for an unknown instance")
	}
	b, _ := rep.For("i-2")
	if !b.Refused() {
		t.Error("the opted-out instance must be a refusal")
	}
	if s, ok := b.SuppressionFor(ReasonModeOff); !ok || s.Code != ReasonModeOff {
		t.Errorf("SuppressionFor: %+v %v", s, ok)
	}
	if _, ok := b.SuppressionFor(ReasonMemoryBlind); ok {
		t.Error("SuppressionFor invented a suppression")
	}
	if _, ok := b.AdvisoryFor(AdvisoryGraviton); ok {
		t.Error("AdvisoryFor invented an advisory")
	}
	if rep.Totals.SuppressedByCode[ReasonModeOff] != 1 {
		t.Errorf("totals do not count refusal reasons: %v", rep.Totals.SuppressedByCode)
	}
	if got := a.Target.String(); got != "ec2/1234/us-east-1/i-1" {
		t.Errorf("TargetRef.String() = %q", got)
	}
}

func TestSizerEchoesItsConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinConfidence = 0.8
	s, err := NewSizer(testCatalog(t), cfg)
	if err != nil {
		t.Fatalf("sizer: %v", err)
	}
	if s.Config().MinConfidence != 0.8 {
		t.Errorf("Config() = %+v", s.Config())
	}
	if s.Config().Provider != DefaultProvider {
		t.Errorf("an empty provider must default to %q", DefaultProvider)
	}
	rep := s.Assess(testNow, &Snapshot{Domain: Domain}, nil)
	if rep.Config.MinConfidence != 0.8 {
		t.Error("a stored report must explain its own thresholds")
	}
}
