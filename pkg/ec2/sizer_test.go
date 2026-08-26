package ec2

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// testCatalogJSON is a small, explicit catalog. Prices are the us-east-1
// Linux on-demand figures docs/design/compute-domains.md §4.4–§4.6 cite, so
// the arithmetic in these tests is the arithmetic in the design document.
// It is local rather than pkg/pricing's embedded catalog so that a catalog
// refresh cannot silently change what a suppression test proves.
const testCatalogJSON = `{"instances":[
 {"provider":"aws","name":"c5.large","family":"c5","arch":"amd64","milliCPU":2000,"memoryBytes":4294967296,"hourlyUSD":0.085},
 {"provider":"aws","name":"c5.xlarge","family":"c5","arch":"amd64","milliCPU":4000,"memoryBytes":8589934592,"hourlyUSD":0.17},
 {"provider":"aws","name":"m5.large","family":"m5","arch":"amd64","milliCPU":2000,"memoryBytes":8589934592,"hourlyUSD":0.096},
 {"provider":"aws","name":"m5.xlarge","family":"m5","arch":"amd64","milliCPU":4000,"memoryBytes":17179869184,"hourlyUSD":0.192},
 {"provider":"aws","name":"m5.2xlarge","family":"m5","arch":"amd64","milliCPU":8000,"memoryBytes":34359738368,"hourlyUSD":0.384},
 {"provider":"aws","name":"r5.large","family":"r5","arch":"amd64","milliCPU":2000,"memoryBytes":17179869184,"hourlyUSD":0.126},
 {"provider":"aws","name":"r5.xlarge","family":"r5","arch":"amd64","milliCPU":4000,"memoryBytes":34359738368,"hourlyUSD":0.252},
 {"provider":"aws","name":"m7g.large","family":"m7g","arch":"arm64","milliCPU":2000,"memoryBytes":8589934592,"hourlyUSD":0.0816},
 {"provider":"aws","name":"m7g.xlarge","family":"m7g","arch":"arm64","milliCPU":4000,"memoryBytes":17179869184,"hourlyUSD":0.1632},
 {"provider":"aws","name":"t3.medium","family":"t3","arch":"amd64","milliCPU":2000,"memoryBytes":4294967296,"hourlyUSD":0.0416,"burstable":true},
 {"provider":"aws","name":"t3.large","family":"t3","arch":"amd64","milliCPU":2000,"memoryBytes":8589934592,"hourlyUSD":0.0832,"burstable":true},
 {"provider":"aws","name":"t3.xlarge","family":"t3","arch":"amd64","milliCPU":4000,"memoryBytes":17179869184,"hourlyUSD":0.1664,"burstable":true}
]}`

func testCatalog(t *testing.T) *pricing.Catalog {
	t.Helper()
	cat, err := pricing.Load(strings.NewReader(testCatalogJSON))
	if err != nil {
		t.Fatalf("load test catalog: %v", err)
	}
	return cat
}

// windowDays is the observation span every sizer test uses: long enough to
// clear the 7-day MinWindow without inflating the fixtures.
const windowDays = 10

func rec(id, itype string, opts ...func(*InstanceRecord)) InstanceRecord {
	r := InstanceRecord{
		InstanceID: id, InstanceType: itype, Architecture: "x86_64", State: "running",
		AvailabilityZone: "us-east-1a", Tenancy: "default", MonitoringState: "disabled",
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func detailed(r *InstanceRecord)  { r.MonitoringState = "enabled" }
func unlimited(r *InstanceRecord) { r.CPUCredits = CreditModeUnlimited }
func standard(r *InstanceRecord)  { r.CPUCredits = CreditModeStandard }
func withStore(r *InstanceRecord) { r.InstanceStoreVolumes = 1 }
func tag(k, v string) func(*InstanceRecord) {
	return func(r *InstanceRecord) { r.Tags = append(r.Tags, Tag{Key: k, Value: v}) }
}

// series builds a full-window series at the resolution the instance's
// monitoring state implies.
func series(id, metric string, period time.Duration, vals ...float64) RecordedSeries {
	count := int((time.Duration(windowDays) * 24 * time.Hour) / period)
	return SyntheticSeries(id, metric, testNow, period, count, vals)
}

const (
	basic    = 5 * time.Minute
	detailP  = time.Minute
	memAgent = MetricMemUsedPercent
)

// collectFor runs the production collector over a fixture, so every sizer test
// is also a collector test.
func collectFor(t *testing.T, insts []InstanceRecord, metrics []RecordedSeries) *Snapshot {
	t.Helper()
	f := &Fixture{
		InventoryPages: []DescribeInstancesOutput{{Reservations: []Reservation{{Instances: insts}}}},
		Metrics:        metrics,
	}
	c, err := NewCollector(f, f, CollectorConfig{
		Scope: "1234/us-east-1", Region: "us-east-1",
		Window: time.Duration(windowDays+1) * 24 * time.Hour, CollectMemory: true,
	})
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	snap, err := c.Collect(context.Background(), testNow)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return snap
}

func assess(t *testing.T, snap *Snapshot, inv *commit.Inventory, tweak ...func(*Config)) *Report {
	t.Helper()
	cfg := DefaultConfig()
	for _, fn := range tweak {
		fn(&cfg)
	}
	s, err := NewSizer(testCatalog(t), cfg)
	if err != nil {
		t.Fatalf("new sizer: %v", err)
	}
	rep := s.Assess(testNow, snap, inv)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report violates its own invariants: %v", err)
	}
	return rep
}

func only(t *testing.T, a Assessment, code string) Suppression {
	t.Helper()
	if len(a.Suppressions) != 1 {
		var got []string
		for _, s := range a.Suppressions {
			got = append(got, s.Code)
		}
		t.Fatalf("%s: expected exactly one suppression (%s), got %v", a.Target.ID, code, got)
	}
	if a.Suppressions[0].Code != code {
		t.Fatalf("%s: suppression = %q, want %q (%s)", a.Target.ID, a.Suppressions[0].Code, code,
			a.Suppressions[0].Reason)
	}
	if a.Proposal != nil {
		t.Fatalf("%s: %s must be a refusal, but a proposal was made (%s)", a.Target.ID, code,
			a.Proposal.InstanceType)
	}
	return a.Suppressions[0]
}

func single(t *testing.T, rep *Report) Assessment {
	t.Helper()
	if len(rep.Assessments) != 1 {
		t.Fatalf("expected 1 assessment, got %d", len(rep.Assessments))
	}
	return rep.Assessments[0]
}

// --- the memory-blind rule -------------------------------------------------

// The headline invariant: with no memory metric, a cheaper shape that would
// have taken memory away is REFUSED, and the refusal names what it declined.
// It does not degrade into a smaller recommendation.
func TestMemoryBlindRefusesRatherThanShrinks(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-blind", "r5.large")},
		[]RecordedSeries{series("i-blind", MetricCPUUtilization, basic, 4, 6, 5, 7)},
	)
	a := single(t, assess(t, snap, nil))

	if !a.Observation.MemoryBlind {
		t.Fatal("an instance with no CWAgent series must be memory-blind")
	}
	s := only(t, a, ReasonMemoryBlind)
	// c5.large is cheaper than r5.large and clears the CPU demand — and takes
	// 12 GiB of memory away on no evidence at all.
	for _, want := range []string{"c5.large", "NO memory metric", "current 16.0GiB", "CloudWatch agent"} {
		if !strings.Contains(s.Reason, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, s.Reason)
		}
	}
	if a.Observation.MemoryFloorBytes != a.Current.Resources.MemoryBytes {
		t.Errorf("memory floor = %d, want the current instance's memory %d",
			a.Observation.MemoryFloorBytes, a.Current.Resources.MemoryBytes)
	}
}

// The same instance, same CPU, with a memory signal: now the move is decidable
// and it is made. Memory evidence is what unlocks memory reduction — nothing
// else does.
func TestMemorySignalUnlocksTheDownsize(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-seen", "r5.large")},
		[]RecordedSeries{
			series("i-seen", MetricCPUUtilization, basic, 4, 6, 5, 7),
			series("i-seen", memAgent, basic, 18, 20, 17, 19),
		},
	)
	a := single(t, assess(t, snap, nil))

	if a.Observation.MemoryBlind {
		t.Fatal("a CWAgent series must clear memory-blind")
	}
	if a.Proposal == nil {
		t.Fatalf("expected a proposal, got refusals: %+v", a.Suppressions)
	}
	if a.Proposal.InstanceType != "c5.large" {
		t.Errorf("proposed %s, want c5.large", a.Proposal.InstanceType)
	}
	if a.Proposal.NetSavingsMonthlyUSD <= 0 {
		t.Errorf("net savings = %v, want positive", a.Proposal.NetSavingsMonthlyUSD)
	}
	// Peak 20 % of 16 GiB with 25 % headroom = 4 GiB, exactly c5.large.
	if got := a.Observation.MemoryFloorBytes; got > 4*(1<<30) {
		t.Errorf("memory floor = %s, want <= 4GiB", gib(got))
	}
	if a.Proposal.Action != ActionStopStart {
		t.Errorf("action = %q, want %q", a.Proposal.Action, ActionStopStart)
	}
}

// Memory-blind never fires when the floor changed nothing; it is a statement
// about a blocked decision, not a permanent banner.
func TestMemoryBlindDoesNotFireWhenItChangesNothing(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-cheap", "c5.large")},
		[]RecordedSeries{series("i-cheap", MetricCPUUtilization, basic, 4, 6, 5, 7)},
	)
	a := single(t, assess(t, snap, nil))
	if !a.Observation.MemoryBlind {
		t.Fatal("expected memory-blind observation")
	}
	only(t, a, ReasonNoCheaperCandidate)
}

// --- suppressions, each firing alone --------------------------------------

func TestSuppressionK8sTaggedFiresAlone(t *testing.T) {
	for _, key := range []string{TagK8sClusterPrefix + "prod-eks", TagEKSCluster, TagAWSEKSCluster} {
		snap := collectFor(t,
			[]InstanceRecord{rec("i-node", "m5.2xlarge", tag(key, "prod-eks"))},
			[]RecordedSeries{series("i-node", MetricCPUUtilization, basic, 3, 4, 2)},
		)
		a := single(t, assess(t, snap, nil))
		s := only(t, a, ReasonK8sTagged)
		if !strings.Contains(s.Reason, "k8s-nodes") {
			t.Errorf("%s: refusal must hand the instance to the k8s-nodes pipeline: %s", key, s.Reason)
		}
		if len(a.Advisories) != 0 {
			t.Errorf("%s: an excluded instance must not carry advisories", key)
		}
		if !a.Excluded() {
			t.Errorf("%s: assessment should report itself excluded", key)
		}
	}
}

func TestSuppressionModeOffFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-off", "m5.xlarge", tag(TagKilterMode, "off"))},
		[]RecordedSeries{series("i-off", MetricCPUUtilization, basic, 3, 4, 2)},
	)
	a := single(t, assess(t, snap, nil))
	s := only(t, a, ReasonModeOff)
	if !strings.Contains(s.Reason, TagKilterMode) {
		t.Errorf("refusal must name the tag: %s", s.Reason)
	}
}

// §4.4 ex.1: rightsizing off an RI can raise the bill. The list price says
// "save"; the invoice says otherwise, and the suppression carries the date the
// block lapses.
func TestSuppressionCommitmentNegativeFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-ri", "m5.xlarge")},
		[]RecordedSeries{
			series("i-ri", MetricCPUUtilization, basic, 4, 6, 5, 7),
			series("i-ri", memAgent, basic, 10, 12, 11),
		},
	)
	expiry := testNow.AddDate(0, 7, 0)
	inv := &commit.Inventory{RIs: []commit.ReservedInstance{{
		ID: "ri-1", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
		Platform: commit.PlatformLinux, Tenancy: commit.TenancyDefault,
		OfferingClass: "standard", EffectiveHourlyUSD: 0.121, Expires: expiry,
	}}}
	a := single(t, assess(t, snap, inv))

	s := only(t, a, commit.ReasonCommitmentNegative)
	if !strings.Contains(s.Reason, "m5.xlarge") || !strings.Contains(s.Reason, "stranding") {
		t.Errorf("refusal does not explain stranding: %s", s.Reason)
	}
	if !s.ValidFrom.Equal(expiry) {
		t.Errorf("ValidFrom = %v, want the RI expiry %v", s.ValidFrom, expiry)
	}

	// Same instance, same metrics, no commitments ⇒ the move is real.
	b := single(t, assess(t, collectFor(t,
		[]InstanceRecord{rec("i-ri", "m5.xlarge")},
		[]RecordedSeries{
			series("i-ri", MetricCPUUtilization, basic, 4, 6, 5, 7),
			series("i-ri", memAgent, basic, 10, 12, 11),
		}), nil))
	if b.Proposal == nil {
		t.Fatalf("without commitments the same change must be proposed: %+v", b.Suppressions)
	}
}

// §4.4 ex.2: downsizing inside an RI family claims 50 % and realizes 0 %.
func TestSuppressionCommitmentNeutralFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-flex", "m5.xlarge")},
		[]RecordedSeries{
			series("i-flex", MetricCPUUtilization, basic, 4, 6, 5, 7),
			series("i-flex", memAgent, basic, 40, 44, 42),
		},
	)
	inv := &commit.Inventory{RIs: []commit.ReservedInstance{{
		ID: "ri-flex", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
		Platform: commit.PlatformLinux, Tenancy: commit.TenancyDefault,
		EffectiveHourlyUSD: 0.121, Expires: testNow.AddDate(1, 0, 0),
	}}}
	a := single(t, assess(t, snap, inv))
	if !a.Suppressed(commit.ReasonCommitmentNeutral) && !a.Suppressed(commit.ReasonCommitmentNegative) {
		t.Fatalf("expected a commitment suppression, got %+v", a.Suppressions)
	}
	if a.Proposal != nil {
		t.Fatal("an RI-absorbed downsize must not be proposed")
	}
}

func TestSuppressionUnknownInstanceTypeFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-exotic", "x8g.48xlarge")},
		[]RecordedSeries{series("i-exotic", MetricCPUUtilization, basic, 5, 6)},
	)
	a := single(t, assess(t, snap, nil))
	s := only(t, a, ReasonUnknownInstanceType)
	if !strings.Contains(s.Reason, "x8g.48xlarge") {
		t.Errorf("refusal must name the type: %s", s.Reason)
	}
}

func TestSuppressionNoMetricsFiresAlone(t *testing.T) {
	snap := collectFor(t, []InstanceRecord{rec("i-quiet", "m5.xlarge")}, nil)
	a := single(t, assess(t, snap, nil))
	only(t, a, ReasonNoMetrics)
}

func TestSuppressionPartialMetricsFiresAlone(t *testing.T) {
	s := series("i-partial", MetricCPUUtilization, basic, 4, 6, 5)
	s.Status = StatusPartialData
	snap := collectFor(t, []InstanceRecord{rec("i-partial", "m5.xlarge")}, []RecordedSeries{s})
	a := single(t, assess(t, snap, nil))
	sup := only(t, a, ReasonPartialMetrics)
	if !strings.Contains(sup.Reason, "partial window is not a window") {
		t.Errorf("refusal prose lost its point: %s", sup.Reason)
	}
}

func TestSuppressionInsufficientWindowFiresAlone(t *testing.T) {
	// Two days of datapoints against a seven-day minimum.
	short := SyntheticSeries("i-new", MetricCPUUtilization, testNow, basic, 576, []float64{4, 6, 5})
	snap := collectFor(t, []InstanceRecord{rec("i-new", "m5.xlarge")}, []RecordedSeries{short})
	a := single(t, assess(t, snap, nil))
	sup := only(t, a, ReasonInsufficientWindow)
	if !strings.Contains(sup.Reason, "minimum") {
		t.Errorf("refusal must state the minimum: %s", sup.Reason)
	}
}

func TestSuppressionInsufficientSamplesFiresAlone(t *testing.T) {
	// A full ten-day span, but only a third of the datapoints it implies:
	// the gaps are unobserved time, not idle time.
	// 15-minute spacing against a 5-minute publication period: one datapoint
	// where three were expected.
	sparse := SyntheticSeries("i-gappy", MetricCPUUtilization, testNow,
		15*time.Minute, int((time.Duration(windowDays)*24*time.Hour)/(15*time.Minute)), []float64{4, 6, 5})
	snap := collectFor(t, []InstanceRecord{rec("i-gappy", "m5.xlarge")}, []RecordedSeries{sparse})
	a := single(t, assess(t, snap, nil))
	only(t, a, ReasonInsufficientSamples)
}

func TestSuppressionLowConfidenceFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-meh", "m5.xlarge")},
		[]RecordedSeries{
			series("i-meh", MetricCPUUtilization, basic, 4, 6, 5, 7),
			series("i-meh", memAgent, basic, 60, 62, 61),
		},
	)
	a := single(t, assess(t, snap, nil, func(c *Config) { c.MinConfidence = 0.95 }))
	sup := only(t, a, ReasonLowConfidence)
	if !strings.Contains(sup.Reason, "metric-resolution") {
		t.Errorf("a low-confidence refusal must name what would fix it: %s", sup.Reason)
	}
}

// A sizer that can only shrink is a downsizer. Demand above the current shape
// is reported and never proposed, because spending money is not this unit's
// call.
func TestSuppressionUndersizedFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-hot", "c5.large")},
		[]RecordedSeries{series("i-hot", MetricCPUUtilization, basic, 88, 92, 90)},
	)
	a := single(t, assess(t, snap, nil))
	only(t, a, ReasonUndersized)
	ad, ok := a.AdvisoryFor(AdvisoryUndersized)
	if !ok {
		t.Fatal("an undersized instance must produce an advisory")
	}
	if ad.GrossSavingsMonthlyUSD != 0 || ad.NetSavingsMonthlyUSD != 0 {
		t.Error("growing an instance is not a saving and must not claim one")
	}
}

// --- graviton advisory -----------------------------------------------------

func TestGravitonIsAdvisoryOnlyAndAlwaysCaveated(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-x86", "m5.xlarge")},
		[]RecordedSeries{
			series("i-x86", MetricCPUUtilization, basic, 55, 60, 58),
			series("i-x86", memAgent, basic, 70, 74, 72),
		},
	)
	a := single(t, assess(t, snap, nil))

	ad, ok := a.AdvisoryFor(AdvisoryGraviton)
	if !ok {
		t.Fatalf("expected a graviton advisory; advisories = %+v", a.Advisories)
	}
	if ad.ProposedType != "m7g.xlarge" {
		t.Errorf("advisory target = %q, want m7g.xlarge", ad.ProposedType)
	}
	if ad.Actuatable() || ad.Action() != ActionAdvisory {
		t.Error("a graviton advisory must never be actuatable")
	}
	for _, want := range []string{"portability", "arm64", "advisory only"} {
		if !strings.Contains(strings.ToLower(ad.Caveat), want) {
			t.Errorf("caveat does not state %q:\n%s", want, ad.Caveat)
		}
	}
	// It is never promoted into the proposal: proposals stay same-architecture.
	if a.Proposal != nil && a.Proposal.Spec.Attrs[AttrArch] == "arm64" {
		t.Error("an architecture migration was proposed as actuatable")
	}
	// Its money is reported separately from claimed savings.
	if a.Proposal == nil && ad.NetSavingsMonthlyUSD > 0 &&
		nearly(a.CurrentMonthlyUSD, ad.NetSavingsMonthlyUSD) {
		t.Error("advisory savings must not be folded into the report's claim")
	}
}

// Family-scoped commitments do not follow an instance across families, so a
// Graviton advisory under an m5 RI must say the delta is not a saving.
func TestGravitonAdvisoryStatesCommitmentCaveat(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-x86ri", "m5.xlarge")},
		[]RecordedSeries{
			series("i-x86ri", MetricCPUUtilization, basic, 55, 60, 58),
			series("i-x86ri", memAgent, basic, 70, 74, 72),
		},
	)
	inv := &commit.Inventory{RIs: []commit.ReservedInstance{{
		ID: "ri-m5", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
		Platform: commit.PlatformLinux, Tenancy: commit.TenancyDefault,
		EffectiveHourlyUSD: 0.121, Expires: testNow.AddDate(0, 9, 0),
	}}}
	a := single(t, assess(t, snap, inv))
	ad, ok := a.AdvisoryFor(AdvisoryGraviton)
	if !ok {
		t.Fatal("expected a graviton advisory")
	}
	if ad.NetSavingsMonthlyUSD != 0 {
		t.Errorf("net = %v: an m7g move away from an m5 RI is not a saving", ad.NetSavingsMonthlyUSD)
	}
	if !strings.Contains(ad.Caveat, "NOT a saving") {
		t.Errorf("caveat must refuse the claim outright:\n%s", ad.Caveat)
	}
	if ad.GrossSavingsMonthlyUSD <= 0 {
		t.Error("the list-price delta should still be shown beside the fact")
	}
}

// --- risk and read-only ----------------------------------------------------

func TestInstanceStoreRaisesRiskWithoutBlockingTheReport(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-eph", "r5.large", withStore)},
		[]RecordedSeries{
			series("i-eph", MetricCPUUtilization, basic, 4, 6, 5),
			series("i-eph", memAgent, basic, 18, 20, 19),
		},
	)
	a := single(t, assess(t, snap, nil))
	if a.Proposal == nil {
		t.Fatalf("expected a proposal: %+v", a.Suppressions)
	}
	if a.Proposal.Risk != RiskHigh {
		t.Errorf("risk = %q, want %q for an instance-store instance", a.Proposal.Risk, RiskHigh)
	}
	if !strings.Contains(a.Proposal.Reason, "instance-store") {
		t.Errorf("the reason must warn about instance-store data loss: %s", a.Proposal.Reason)
	}
}

func TestReportTextIsDeterministicAndSaysWhatItRefused(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{
			rec("i-blind", "r5.large"),
			rec("i-node", "m5.2xlarge", tag(TagK8sClusterPrefix+"prod", "owned")),
		},
		[]RecordedSeries{
			series("i-blind", MetricCPUUtilization, basic, 4, 6, 5),
			series("i-node", MetricCPUUtilization, basic, 40, 44, 42),
		},
	)
	rep := assess(t, snap, nil)
	var a, b bytes.Buffer
	if err := rep.WriteText(&a); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := rep.WriteText(&b); err != nil {
		t.Fatalf("write: %v", err)
	}
	if a.String() != b.String() {
		t.Fatal("text output is not deterministic")
	}
	for _, want := range []string{"REFUSE [memory-blind]", "REFUSE [k8s-tagged]", "read-only", "basic monitoring"} {
		if !strings.Contains(a.String(), want) {
			t.Errorf("report text is missing %q:\n%s", want, a.String())
		}
	}
}

func TestAssessHandlesNilSnapshotAndNilInventory(t *testing.T) {
	s, err := NewSizer(testCatalog(t), DefaultConfig())
	if err != nil {
		t.Fatalf("new sizer: %v", err)
	}
	rep := s.Assess(testNow, nil, nil)
	if rep == nil || len(rep.Assessments) != 0 || len(rep.Warnings) == 0 {
		t.Fatalf("a nil snapshot must yield an empty, explained report: %+v", rep)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("empty report must still validate: %v", err)
	}
}

func TestSizerRejectsBadConfiguration(t *testing.T) {
	if _, err := NewSizer(nil, DefaultConfig()); err == nil {
		t.Error("a nil catalog must be rejected")
	}
	bad := []func(*Config){
		func(c *Config) { c.CPUPercentile = 0 },
		func(c *Config) { c.CPUHeadroom = 0.9 },
		func(c *Config) { c.MemHeadroom = 0 },
		func(c *Config) { c.CoarseResolutionHeadroom = 0.5 },
		func(c *Config) { c.MinWindow = 0 },
		func(c *Config) { c.MinSampleCoverage = 2 },
		func(c *Config) { c.MinConfidence = -1 },
	}
	for i, mut := range bad {
		cfg := DefaultConfig()
		mut(&cfg)
		if _, err := NewSizer(testCatalog(t), cfg); err == nil {
			t.Errorf("config mutation %d was accepted", i)
		}
	}
}
