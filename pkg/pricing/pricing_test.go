package pricing

import (
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

func TestEmbeddedCatalogLoads(t *testing.T) {
	c := Embedded()
	if c.Len() < 30 {
		t.Fatalf("embedded catalog suspiciously small: %d", c.Len())
	}
	it, ok := c.Lookup("aws", "m5.xlarge")
	if !ok {
		t.Fatal("m5.xlarge missing")
	}
	if it.MilliCPU != 4000 || it.MemoryBytes != 16<<30 {
		t.Fatalf("m5.xlarge shape wrong: %+v", it)
	}
	if it.Price(false) != 0.192 {
		t.Fatalf("m5.xlarge on-demand price %v", it.Price(false))
	}
	if it.Price(true) >= it.Price(false) {
		t.Fatal("spot must be cheaper than on-demand")
	}
}

func TestAllEmbeddedSpotCheaper(t *testing.T) {
	for _, it := range Embedded().Candidates("", "") {
		if it.SpotHourlyUSD > 0 && it.SpotHourlyUSD >= it.HourlyUSD {
			t.Errorf("%s/%s: spot %v >= ondemand %v", it.Provider, it.Name, it.SpotHourlyUSD, it.HourlyUSD)
		}
	}
}

func TestCandidatesSortedAndFiltered(t *testing.T) {
	c := Embedded()
	arm := c.Candidates("aws", "arm64")
	if len(arm) == 0 {
		t.Fatal("no arm64 aws candidates")
	}
	for _, it := range arm {
		if it.Arch != "arm64" || it.Provider != "aws" {
			t.Fatalf("filter leak: %+v", it)
		}
	}
	all := c.Candidates("gcp", "")
	for i := 1; i < len(all); i++ {
		if all[i].HourlyUSD < all[i-1].HourlyUSD {
			t.Fatal("candidates not sorted by price")
		}
	}
}

func TestNodeCostResolutionOrder(t *testing.T) {
	c := Embedded()

	annotated := &model.NodeSpec{Name: "n1", HourlyCost: 0.5, InstanceType: "m5.large", Provider: "aws"}
	if cost, src := c.NodeHourlyCost(annotated); cost != 0.5 || src != SourceAnnotation {
		t.Fatalf("annotation should win: %v %v", cost, src)
	}

	catalog := &model.NodeSpec{Name: "n2", InstanceType: "m5.large", Provider: "aws"}
	if cost, src := c.NodeHourlyCost(catalog); cost != 0.096 || src != SourceCatalog {
		t.Fatalf("catalog lookup failed: %v %v", cost, src)
	}

	spot := &model.NodeSpec{Name: "n3", InstanceType: "m5.large", Provider: "aws", Spot: true}
	if cost, _ := c.NodeHourlyCost(spot); cost != 0.035 {
		t.Fatalf("spot price wrong: %v", cost)
	}

	unknown := &model.NodeSpec{Name: "n4", InstanceType: "weird.9xlarge", Provider: "onprem",
		Capacity: model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30}}
	cost, src := c.NodeHourlyCost(unknown)
	if src != SourceFallback {
		t.Fatalf("expected fallback, got %v", src)
	}
	want := 8*FallbackCPUHourlyUSD + 32*FallbackGiBHourlyUSD
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("fallback cost %v, want %v", cost, want)
	}
}

func TestSnapshotCost(t *testing.T) {
	c := Embedded()
	snap := &model.ClusterSnapshot{Nodes: []model.NodeSpec{
		{Name: "a", InstanceType: "m5.xlarge", Provider: "aws"},
		{Name: "b", InstanceType: "m5.xlarge", Provider: "aws"},
		{Name: "c", InstanceType: "m5.xlarge", Provider: "aws", Spot: true},
	}}
	cc := c.SnapshotCost(snap)
	wantHourly := 0.192 + 0.192 + 0.070
	if diff := cc.HourlyUSD - wantHourly; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cluster hourly %v, want %v", cc.HourlyUSD, wantHourly)
	}
	if cc.MonthlyUSD != cc.HourlyUSD*HoursPerMonth {
		t.Fatal("monthly math broken")
	}
	if len(cc.Nodes) != 3 {
		t.Fatalf("want 3 node costs, got %d", len(cc.Nodes))
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	cases := []string{
		`{}`,
		`{"instances": []}`,
		`{"instances": [{"provider":"aws","name":"x","milliCPU":0,"memoryBytes":1,"hourlyUSD":1}]}`,
		`{"instances": [{"provider":"aws","name":"x","milliCPU":1000,"memoryBytes":1,"hourlyUSD":0}]}`,
		`{"nope": true, "instances": [{"provider":"aws","name":"x","milliCPU":1000,"memoryBytes":1,"hourlyUSD":1}]}`,
		`not json`,
	}
	for i, s := range cases {
		if _, err := Load(strings.NewReader(s)); err == nil {
			t.Errorf("case %d should fail: %s", i, s)
		}
	}
}

func TestCustomCatalogOverride(t *testing.T) {
	custom := `{"instances": [
		{"provider":"onprem","name":"rack-std","milliCPU":16000,"memoryBytes":68719476736,"hourlyUSD":0.20}
	]}`
	c, err := Load(strings.NewReader(custom))
	if err != nil {
		t.Fatal(err)
	}
	it, ok := c.Lookup("onprem", "rack-std")
	if !ok || it.Arch != "amd64" {
		t.Fatalf("custom lookup failed: %+v ok=%v", it, ok)
	}
}
