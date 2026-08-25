package pricing

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

// entry builds one catalog JSON instance object for table tests.
func entry(provider, name, arch string, milliCPU, memBytes int64, hourly, spot float64) string {
	s := fmt.Sprintf(`{"provider":%q,"name":%q,"milliCPU":%d,"memoryBytes":%d,"hourlyUSD":%v`,
		provider, name, milliCPU, memBytes, hourly)
	if arch != "" {
		s += fmt.Sprintf(`,"arch":%q`, arch)
	}
	if spot != 0 {
		s += fmt.Sprintf(`,"spotHourlyUSD":%v`, spot)
	}
	return s + "}"
}

func catalogJSON(entries ...string) string {
	return `{"instances":[` + strings.Join(entries, ",") + `]}`
}

func TestLoadValidationBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string // substring; empty = must load
	}{
		{"valid minimal", catalogJSON(entry("aws", "x.large", "", 1000, 1<<30, 0.01, 0)), ""},
		{"valid arm64", catalogJSON(entry("aws", "x.large", "arm64", 1000, 1<<30, 0.01, 0)), ""},
		{"valid spot", catalogJSON(entry("aws", "x.large", "", 1000, 1<<30, 0.10, 0.03)), ""},
		{"spot equal to on-demand accepted", catalogJSON(entry("aws", "x.large", "", 1000, 1<<30, 0.10, 0.10)), ""},
		{"huge but positive accepted", catalogJSON(entry("aws", "x.large", "", math.MaxInt64, math.MaxInt64, 1e12, 0)), ""},
		{"negative spot", catalogJSON(entry("aws", "x.large", "", 1000, 1<<30, 0.10, -0.01)), "negative spot"},
		{"unknown arch", catalogJSON(entry("aws", "x.large", "x86_64", 1000, 1<<30, 0.01, 0)), "unknown arch"},
		{"negative milliCPU", catalogJSON(entry("aws", "x.large", "", -1000, 1<<30, 0.01, 0)), "invalid instance"},
		{"negative memory", catalogJSON(entry("aws", "x.large", "", 1000, -1, 0.01, 0)), "invalid instance"},
		{"negative price", catalogJSON(entry("aws", "x.large", "", 1000, 1<<30, -0.01, 0)), "invalid instance"},
		{"duplicate provider/name", catalogJSON(
			entry("aws", "x.large", "", 1000, 1<<30, 0.01, 0),
			entry("aws", "x.large", "", 2000, 2<<30, 0.02, 0)), "duplicate"},
		{"same name different provider ok", catalogJSON(
			entry("aws", "x.large", "", 1000, 1<<30, 0.01, 0),
			entry("gcp", "x.large", "", 1000, 1<<30, 0.01, 0)), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(strings.NewReader(tc.json))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("should load: %v", err)
				}
				if c.Len() == 0 {
					t.Fatal("loaded catalog is empty")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPriceMissingSpotFallsBackToOnDemand(t *testing.T) {
	// Constructed directly (bypassing Load) to probe the Price guard itself.
	for _, spot := range []float64{0, -1, math.Inf(-1)} {
		it := InstanceType{HourlyUSD: 0.5, SpotHourlyUSD: spot}
		if got := it.Price(true); got != 0.5 {
			t.Errorf("spot=%v: Price(true)=%v, want on-demand 0.5", spot, got)
		}
	}
}

func TestNodeHourlyCostAdversarial(t *testing.T) {
	c := Embedded()
	cases := []struct {
		name       string
		node       model.NodeSpec
		wantSource CostSource
		wantCost   float64 // -1 = only check ≥0 and finite
	}{
		{"negative capacity clamps to zero", model.NodeSpec{
			Capacity: model.Resources{MilliCPU: -8000, MemoryBytes: -1}},
			SourceFallback, 0},
		{"negative cpu positive mem", model.NodeSpec{
			Capacity: model.Resources{MilliCPU: -8000, MemoryBytes: 32 << 30}},
			SourceFallback, 32 * FallbackGiBHourlyUSD},
		{"zero capacity", model.NodeSpec{}, SourceFallback, 0},
		{"NaN annotation ignored", model.NodeSpec{HourlyCost: math.NaN(),
			InstanceType: "m5.large", Provider: "aws"},
			SourceCatalog, 0.096},
		{"+Inf annotation ignored", model.NodeSpec{HourlyCost: math.Inf(1),
			InstanceType: "m5.large", Provider: "aws"},
			SourceCatalog, 0.096},
		{"negative annotation ignored", model.NodeSpec{HourlyCost: -5,
			InstanceType: "m5.large", Provider: "aws"},
			SourceCatalog, 0.096},
		{"annotation wins over catalog", model.NodeSpec{HourlyCost: 0.5,
			InstanceType: "m5.large", Provider: "aws"},
			SourceAnnotation, 0.5},
		{"unknown type falls back", model.NodeSpec{InstanceType: "weird.9xl", Provider: "onprem",
			Capacity: model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30}},
			SourceFallback, FallbackCPUHourlyUSD + FallbackGiBHourlyUSD},
		{"spot fallback multiplier", model.NodeSpec{Spot: true,
			Capacity: model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30}},
			SourceFallback, (FallbackCPUHourlyUSD + FallbackGiBHourlyUSD) * 0.35},
		{"huge capacity stays finite", model.NodeSpec{
			Capacity: model.Resources{MilliCPU: math.MaxInt64, MemoryBytes: math.MaxInt64}},
			SourceFallback, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cost, src := c.NodeHourlyCost(&tc.node)
			if src != tc.wantSource {
				t.Fatalf("source %v, want %v", src, tc.wantSource)
			}
			if cost < 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
				t.Fatalf("cost %v violates ≥0/finite invariant", cost)
			}
			if tc.wantCost >= 0 {
				if diff := cost - tc.wantCost; diff > 1e-9 || diff < -1e-9 {
					t.Fatalf("cost %v, want %v", cost, tc.wantCost)
				}
			}
		})
	}
}

func TestCandidatesDeterministicTieBreak(t *testing.T) {
	// Same price everywhere; file order is deliberately scrambled. The sort
	// must land on (price, provider, name) regardless.
	c, err := Load(strings.NewReader(catalogJSON(
		entry("gcp", "b-type", "", 1000, 1<<30, 0.10, 0),
		entry("aws", "z.large", "", 1000, 1<<30, 0.10, 0),
		entry("gcp", "a-type", "", 1000, 1<<30, 0.10, 0),
		entry("aws", "a.large", "", 1000, 1<<30, 0.10, 0),
		entry("aws", "cheap.large", "", 1000, 1<<30, 0.05, 0),
	)))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range c.Candidates("", "") {
		got = append(got, it.Provider+"/"+it.Name)
	}
	want := []string{"aws/cheap.large", "aws/a.large", "aws/z.large", "gcp/a-type", "gcp/b-type"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestSnapshotCostNilAndSumInvariant(t *testing.T) {
	c := Embedded()
	nilCost := c.SnapshotCost(nil)
	if nilCost.HourlyUSD != 0 || nilCost.MonthlyUSD != 0 || len(nilCost.Nodes) != 0 {
		t.Fatalf("nil snapshot must price as empty: %+v", nilCost)
	}
	empty := c.SnapshotCost(&model.ClusterSnapshot{})
	if empty.HourlyUSD != 0 || len(empty.Nodes) != 0 {
		t.Fatalf("empty snapshot: %+v", empty)
	}

	// Mixed sources, including garbage: total must equal the sum of parts and
	// stay ≥0/finite.
	snap := &model.ClusterSnapshot{Nodes: []model.NodeSpec{
		{Name: "cat", InstanceType: "m5.xlarge", Provider: "aws"},
		{Name: "spot", InstanceType: "m5.xlarge", Provider: "aws", Spot: true},
		{Name: "anno", HourlyCost: 1.25},
		{Name: "garbage", Capacity: model.Resources{MilliCPU: -1, MemoryBytes: -1}},
		{Name: "fallback", Capacity: model.Resources{MilliCPU: 4000, MemoryBytes: 8 << 30}},
	}}
	cc := c.SnapshotCost(snap)
	if len(cc.Nodes) != len(snap.Nodes) {
		t.Fatalf("want %d node entries, got %d", len(snap.Nodes), len(cc.Nodes))
	}
	sum := 0.0
	for _, nc := range cc.Nodes {
		if nc.HourlyUSD < 0 || math.IsNaN(nc.HourlyUSD) || math.IsInf(nc.HourlyUSD, 0) {
			t.Fatalf("node %s cost %v violates ≥0/finite invariant", nc.Node, nc.HourlyUSD)
		}
		if nc.MonthlyUSD != nc.HourlyUSD*HoursPerMonth {
			t.Fatalf("node %s monthly math broken", nc.Node)
		}
		sum += nc.HourlyUSD
	}
	if diff := cc.HourlyUSD - sum; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("total %v != sum of parts %v", cc.HourlyUSD, sum)
	}
}

// FuzzLoad asserts that any catalog Load accepts satisfies the invariants the
// decision path relies on: positive shapes and prices, non-negative spot, a
// known arch, and an index that agrees with the instance list.
func FuzzLoad(f *testing.F) {
	f.Add([]byte(catalogJSON(entry("aws", "m5.large", "amd64", 2000, 8<<30, 0.096, 0.035))))
	f.Add([]byte(catalogJSON(entry("gcp", "n2-standard-2", "", 2000, 8<<30, 0.097, 0))))
	f.Add([]byte(`{"instances":[]}`))
	f.Add([]byte(`{"instances":[{"provider":"aws","name":"x","milliCPU":-1,"memoryBytes":1,"hourlyUSD":1}]}`))
	f.Add([]byte(`{"instances":[{"provider":"a","name":"x","milliCPU":1,"memoryBytes":1,"hourlyUSD":1,"spotHourlyUSD":-1}]}`))
	f.Add([]byte(`not json`))
	f.Add(embeddedCatalog)
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Load(strings.NewReader(string(data)))
		if err != nil {
			return
		}
		if c.Len() == 0 {
			t.Fatal("Load succeeded on empty catalog")
		}
		seen := map[string]bool{}
		for _, it := range c.Candidates("", "") {
			if it.Name == "" || it.Provider == "" {
				t.Fatalf("unnamed entry survived validation: %+v", it)
			}
			if it.MilliCPU <= 0 || it.MemoryBytes <= 0 || it.HourlyUSD <= 0 || it.SpotHourlyUSD < 0 {
				t.Fatalf("non-positive shape/price survived validation: %+v", it)
			}
			if it.Arch != "amd64" && it.Arch != "arm64" {
				t.Fatalf("unknown arch survived validation: %+v", it)
			}
			key := it.Provider + "/" + it.Name
			if seen[key] {
				t.Fatalf("duplicate %s survived validation", key)
			}
			seen[key] = true
			got, ok := c.Lookup(it.Provider, it.Name)
			if !ok || got != it {
				t.Fatalf("index disagrees with instance list for %s", key)
			}
		}
	})
}
