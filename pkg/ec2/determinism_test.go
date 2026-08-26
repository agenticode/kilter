package ec2

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// A report must be a function of what was observed, not of the order it
// arrived in. This shuffles every input ordering the package touches —
// targets, series within a target, tags, catalog rows and commitment
// inventory — and requires the serialized report to be byte-identical.
func TestReportIsShuffleInvariant(t *testing.T) {
	insts := []InstanceRecord{
		rec("i-0001", "m5.xlarge"),
		rec("i-0002", "r5.large"),
		rec("i-0003", "t3.large", standard),
		rec("i-0004", "m5.2xlarge", tag(TagK8sClusterPrefix+"prod", "owned"), tag("Name", "node")),
		rec("i-0005", "c5.large", tag(TagKilterMode, "off"), tag("team", "infra")),
		rec("i-0006", "m5.large", detailed, withStore),
	}
	metrics := []RecordedSeries{
		series("i-0001", MetricCPUUtilization, basic, 12, 18, 15, 21),
		series("i-0001", memAgent, basic, 41, 45, 43),
		series("i-0002", MetricCPUUtilization, basic, 4, 6, 5),
		series("i-0003", MetricCPUUtilization, basic, 9, 11, 10),
		series("i-0003", MetricCPUCreditBalance, basic, 700, 720, 710),
		series("i-0003", MetricCPUSurplusCreditsCharged, basic, 0),
		series("i-0003", MetricCPUSurplusCreditBalance, basic, 0),
		series("i-0003", MetricCPUCreditUsage, basic, 0.3),
		series("i-0004", MetricCPUUtilization, basic, 55, 60, 58),
		series("i-0005", MetricCPUUtilization, basic, 30, 33, 31),
		series("i-0006", MetricCPUUtilization, detailP, 7, 9, 8),
		series("i-0006", memAgent, detailP, 22, 25, 24),
	}
	inv := &commit.Inventory{
		RIs: []commit.ReservedInstance{
			{ID: "ri-a", Count: 1, InstanceType: "m5.large", Region: "us-east-1",
				Platform: commit.PlatformLinux, Tenancy: commit.TenancyDefault, EffectiveHourlyUSD: 0.061,
				Expires: testNow.AddDate(0, 4, 0)},
			{ID: "ri-b", Count: 1, InstanceType: "r5.large", Region: "us-east-1",
				Platform: commit.PlatformLinux, Tenancy: commit.TenancyDefault, EffectiveHourlyUSD: 0.08,
				Expires: testNow.AddDate(0, 11, 0)},
		},
		SavingsPlans: []commit.SavingsPlan{
			{ID: "sp-1", Type: commit.SPCompute, CommitmentUSDPerHour: 0.05, Expires: testNow.AddDate(1, 0, 0)},
		},
	}

	want := renderReport(t, insts, metrics, inv, testCatalogJSON)

	// Reverse every ordering the package could accidentally depend on.
	revInsts := reverseInstances(insts)
	for i := range revInsts {
		revInsts[i].Tags = reverseTags(revInsts[i].Tags)
	}
	revInv := &commit.Inventory{
		RIs:          []commit.ReservedInstance{inv.RIs[1], inv.RIs[0]},
		SavingsPlans: inv.SavingsPlans,
	}
	got := renderReport(t, revInsts, reverseSeries(metrics), revInv, reverseCatalog(t, testCatalogJSON))

	if got != want {
		t.Fatalf("report changed when inputs were reordered\n--- ordered ---\n%s\n--- shuffled ---\n%s",
			want, got)
	}
}

// renderReport runs the whole path — collect, size, serialize — and returns
// canonical JSON.
func renderReport(t *testing.T, insts []InstanceRecord, metrics []RecordedSeries,
	inv *commit.Inventory, catalogJSON string) string {
	t.Helper()
	snap := collectFor(t, insts, metrics)
	cat, err := pricing.Load(strings.NewReader(catalogJSON))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	s, err := NewSizer(cat, DefaultConfig())
	if err != nil {
		t.Fatalf("sizer: %v", err)
	}
	rep := s.Assess(testNow, snap, inv)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invalid: %v", err)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func reverseInstances(in []InstanceRecord) []InstanceRecord {
	out := make([]InstanceRecord, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func reverseTags(in []Tag) []Tag {
	out := make([]Tag, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

func reverseSeries(in []RecordedSeries) []RecordedSeries {
	out := make([]RecordedSeries, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}

// reverseCatalog re-serializes the catalog with its rows in the opposite
// order, so a price tie broken by slice position would show up.
func reverseCatalog(t *testing.T, catalogJSON string) string {
	t.Helper()
	var f struct {
		Instances []json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal([]byte(catalogJSON), &f); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for i, j := 0, len(f.Instances)-1; i < j; i, j = i+1, j-1 {
		f.Instances[i], f.Instances[j] = f.Instances[j], f.Instances[i]
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return string(b)
}

// Running the same sizer twice over the same snapshot must not drift, which
// also proves Assess keeps no state between calls.
func TestAssessIsRepeatable(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-1", "m5.xlarge"), rec("i-2", "r5.large")},
		[]RecordedSeries{
			series("i-1", MetricCPUUtilization, basic, 11, 13, 12),
			series("i-1", memAgent, basic, 30, 32),
			series("i-2", MetricCPUUtilization, basic, 4, 5),
		},
	)
	s, err := NewSizer(testCatalog(t), DefaultConfig())
	if err != nil {
		t.Fatalf("sizer: %v", err)
	}
	first, _ := json.Marshal(s.Assess(testNow, snap, nil))
	second, _ := json.Marshal(s.Assess(testNow, snap, nil))
	if string(first) != string(second) {
		t.Fatal("two identical calls produced different reports")
	}
}

// The collector must be order-independent too: pages may arrive grouped any
// way at all.
func TestCollectorIsPageGroupingInvariant(t *testing.T) {
	insts := []InstanceRecord{rec("i-a", "m5.large"), rec("i-b", "c5.large"), rec("i-c", "r5.large")}
	metrics := []RecordedSeries{
		series("i-a", MetricCPUUtilization, basic, 5),
		series("i-b", MetricCPUUtilization, basic, 6),
		series("i-c", MetricCPUUtilization, basic, 7),
	}
	one := collectFor(t, insts, metrics)

	f := &Fixture{
		InventoryPages: []DescribeInstancesOutput{
			{Reservations: []Reservation{{Instances: []InstanceRecord{insts[2]}}}},
			{Reservations: []Reservation{{Instances: []InstanceRecord{insts[0], insts[1]}}}},
		},
		Metrics:        reverseSeries(metrics),
		MetricPageSize: 1,
	}
	c, err := NewCollector(f, f, CollectorConfig{
		Scope: "1234/us-east-1", Region: "us-east-1",
		Window: (windowDays + 1) * 24 * 3600 * 1e9, CollectMemory: true,
	})
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	many, err := c.Collect(t.Context(), testNow)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	a, _ := json.Marshal(one)
	b, _ := json.Marshal(many)
	if string(a) != string(b) {
		t.Fatalf("snapshot depends on page grouping\n%s\n%s", a, b)
	}
}
