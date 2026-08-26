package backtest

import (
	"math"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

func TestDefaultCostModelMatchesItsStatedDerivation(t *testing.T) {
	// The doc comment claims the rates come from m5.large — $0.096/h for
	// 2 vCPU and 8 GiB — split evenly. If the embedded catalog ever moves,
	// this fails and the comment gets fixed rather than quietly becoming a lie.
	it, ok := pricing.Embedded().Lookup("aws", "m5.large")
	if !ok {
		t.Fatal("m5.large is no longer in the embedded catalog")
	}
	cores := float64(it.MilliCPU) / 1000
	gib := float64(it.MemoryBytes) / bytesPerGiB
	wantCPU := 0.5 * it.HourlyUSD / cores
	wantMem := 0.5 * it.HourlyUSD / gib

	d := DefaultCostModel()
	if math.Abs(d.CPUUSDPerCoreHour-wantCPU) > 1e-9 {
		t.Errorf("CPU rate = %v, m5.large implies %v", d.CPUUSDPerCoreHour, wantCPU)
	}
	if math.Abs(d.MemUSDPerGiBHour-wantMem) > 1e-9 {
		t.Errorf("memory rate = %v, m5.large implies %v", d.MemUSDPerGiBHour, wantMem)
	}
}

func TestCostModelPricing(t *testing.T) {
	c := CostModel{CPUUSDPerCoreHour: 0.02, MemUSDPerGiBHour: 0.01, IncidentUSD: 1}
	tests := []struct {
		name string
		r    model.Resources
		want float64
	}{
		{"zero is free", model.Resources{}, 0},
		{"one core", model.Resources{MilliCPU: 1000}, 0.02},
		{"one GiB", model.Resources{MemoryBytes: 1 << 30}, 0.01},
		{"both", model.Resources{MilliCPU: 500, MemoryBytes: 2 << 30}, 0.03},
		{"negatives clamp rather than mint credit", model.Resources{MilliCPU: -5000, MemoryBytes: -(4 << 30)}, 0},
		{"one negative dimension does not subsidise the other",
			model.Resources{MilliCPU: -5000, MemoryBytes: 1 << 30}, 0.01},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.HourlyUSD(tc.r); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("HourlyUSD(%v) = %v, want %v", tc.r, got, tc.want)
			}
		})
	}
}

func TestCostModelValidation(t *testing.T) {
	nan := zeroDiv()
	tests := []struct {
		name string
		c    CostModel
		ok   bool
	}{
		{"defaults", DefaultCostModel(), true},
		{"zero incident is a legitimate risk price", CostModel{1, 1, 0}, true},
		{"zero cpu rate", CostModel{0, 1, 1}, false},
		{"zero memory rate", CostModel{1, 0, 1}, false},
		{"negative incident", CostModel{1, 1, -1}, false},
		{"NaN cpu rate", CostModel{nan, 1, 1}, false},
		{"NaN memory rate", CostModel{1, nan, 1}, false},
		{"NaN incident", CostModel{1, 1, nan}, false},
		{"Inf cpu rate", CostModel{math.Inf(1), 1, 1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestCostModelDefaultsFillOnlyUnsetFields(t *testing.T) {
	got := CostModel{CPUUSDPerCoreHour: 0.5}.withDefaults()
	if got.CPUUSDPerCoreHour != 0.5 {
		t.Fatalf("an explicit rate was overwritten: %v", got.CPUUSDPerCoreHour)
	}
	d := DefaultCostModel()
	if got.MemUSDPerGiBHour != d.MemUSDPerGiBHour || got.IncidentUSD != d.IncidentUSD {
		t.Fatalf("unset fields did not take defaults: %+v", got)
	}
}

func TestCostModelFromCatalog(t *testing.T) {
	cat := pricing.Embedded()
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 1, Nodes: 2})
	snap := tr.Snapshots[0]

	got, err := CostModelFromCatalog(cat, snap, CostModel{IncidentUSD: 7})
	if err != nil {
		t.Fatal(err)
	}
	// Two m5.2xlarge at $0.384/h over 2 × 7.8 cores and 2 × 30 GiB allocatable.
	it, _ := cat.Lookup("aws", "m5.2xlarge")
	total := 2 * it.HourlyUSD
	wantCPU := 0.5 * total / (2 * 7.8)
	wantMem := 0.5 * total / (2 * 30)
	if math.Abs(got.CPUUSDPerCoreHour-wantCPU) > 1e-9 || math.Abs(got.MemUSDPerGiBHour-wantMem) > 1e-9 {
		t.Fatalf("derived rates %+v, want cpu=%v mem=%v", got, wantCPU, wantMem)
	}
	if got.IncidentUSD != 7 {
		t.Fatalf("IncidentUSD is not derivable from a price list; it must carry through, got %v", got.IncidentUSD)
	}
}

func TestCostModelFromCatalogRefusesToInventRates(t *testing.T) {
	cat := pricing.Embedded()
	tests := []struct {
		name string
		snap *model.ClusterSnapshot
	}{
		{"no nodes", &model.ClusterSnapshot{ClusterID: "c"}},
		{"unpriced nodes", &model.ClusterSnapshot{ClusterID: "c", Nodes: []model.NodeSpec{{
			Name: "n", Allocatable: model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30}}}}},
		{"no capacity", &model.ClusterSnapshot{ClusterID: "c", Nodes: []model.NodeSpec{{
			Name: "n", InstanceType: "m5.large", Provider: "aws"}}}},
		{"nil snapshot", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CostModelFromCatalog(cat, tc.snap, CostModel{})
			if err == nil {
				t.Fatalf("want an error, got rates %+v", got)
			}
			if got != DefaultCostModel() {
				t.Fatalf("the fallback must be the documented default, got %+v", got)
			}
		})
	}
	if _, err := CostModelFromCatalog(nil, &model.ClusterSnapshot{}, CostModel{}); err == nil {
		t.Fatal("a nil catalog must error")
	}
}

func TestMeanSortedAndRound6(t *testing.T) {
	if got := meanSorted(nil); got != 0 {
		t.Fatalf("mean of nothing = %v, want 0 (a scorecard field is always a number)", got)
	}
	if got := meanSorted([]float64{1, 2, 3}); got != 2 {
		t.Fatalf("mean = %v, want 2", got)
	}
	if got := round6(zeroDiv()); got != 0 {
		t.Fatalf("round6(NaN) = %v, want 0", got)
	}
	if got := round6(math.Inf(-1)); got != 0 {
		t.Fatalf("round6(-Inf) = %v, want 0", got)
	}
	if got := round6(1.00000049); got != 1 {
		t.Fatalf("round6 = %v, want 1", got)
	}
}
