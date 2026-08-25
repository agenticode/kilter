package awssync

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	kpricing "github.com/agenticode/kilter/pkg/pricing"
)

// pagedEC2 serves spot history one page per call, like the real API.
type pagedEC2 struct{ pages [][]ec2types.SpotPrice }

func (f *pagedEC2) DescribeSpotPriceHistory(ctx context.Context, in *ec2.DescribeSpotPriceHistoryInput,
	_ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	if len(f.pages) == 0 {
		return &ec2.DescribeSpotPriceHistoryOutput{}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	out := &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: page}
	if len(f.pages) > 0 {
		tok := "more"
		out.NextToken = &tok
	}
	return out, nil
}

// TestFetchSpotLatestAcrossPages pins the documented contract — newest price
// per AZ, then averaged — when one AZ's history spans page boundaries. A
// per-page "latest" would average the stale page-2 quote in.
func TestFetchSpotLatestAcrossPages(t *testing.T) {
	now := time.Now()
	e := &pagedEC2{pages: [][]ec2types.SpotPrice{
		{spotEntry("m5.xlarge", "us-east-1a", "0.10", now)},
		{
			spotEntry("m5.xlarge", "us-east-1a", "0.50", now.Add(-time.Hour)), // stale, same AZ
			spotEntry("m5.xlarge", "us-east-1b", "0.20", now),
		},
	}}
	s := newWithClients("us-east-1", nil, nil, e)
	got, err := s.fetchSpot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := (0.10 + 0.20) / 2
	if diff := got["m5.xlarge"] - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("spot avg %v, want %v (stale cross-page price leaked in)", got["m5.xlarge"], want)
	}
}

// TestFetchSpotNewerOnLaterPage is the mirror case: a later page can also
// carry the NEWER quote, which must win over an earlier page's stale one.
func TestFetchSpotNewerOnLaterPage(t *testing.T) {
	now := time.Now()
	e := &pagedEC2{pages: [][]ec2types.SpotPrice{
		{spotEntry("c5.large", "us-east-1a", "0.90", now.Add(-time.Hour))},
		{spotEntry("c5.large", "us-east-1a", "0.03", now)},
	}}
	s := newWithClients("us-east-1", nil, nil, e)
	got, err := s.fetchSpot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff := got["c5.large"] - 0.03; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("spot %v, want newest 0.03", got["c5.large"])
	}
}

func TestArchOfFamilies(t *testing.T) {
	cases := map[string]string{
		"a1":     "arm64", // first-gen Graviton: no "g" modifier
		"m7g":    "arm64",
		"c6gd":   "arm64",
		"c7gn":   "arm64",
		"t4g":    "arm64",
		"g5g":    "arm64", // Graviton GPU host
		"im4gn":  "arm64",
		"x2gd":   "arm64",
		"hpc7g":  "arm64",
		"m5":     "amd64",
		"c5":     "amd64",
		"g4dn":   "amd64", // GPU family, "g" is the family letter not a modifier
		"g5":     "amd64",
		"i4i":    "amd64",
		"d3en":   "amd64",
		"u-6tb1": "amd64", // no generation digit, regex intentionally skips
		"inf2":   "amd64",
		"trn1":   "amd64",
	}
	for fam, want := range cases {
		if got := archOf(fam); got != want {
			t.Errorf("archOf(%q) = %q, want %q", fam, got, want)
		}
	}
}

func TestBurstableClassification(t *testing.T) {
	cases := map[string]bool{
		"t2.micro":      true,
		"t3.medium":     true,
		"t3a.large":     true,
		"t4g.small":     true,
		"trn1.2xlarge":  false, // Trainium, not credit-based
		"trn2.48xlarge": false,
		"m5.large":      false,
	}
	for name, want := range cases {
		it, ok := ParsePriceListEntry(priceDoc(name, "2", "4 GiB", "0.05"))
		if !ok {
			t.Fatalf("%s: entry rejected", name)
		}
		if it.Burstable != want {
			t.Errorf("%s: burstable=%v, want %v", name, it.Burstable, want)
		}
	}
}

func TestParseMemoryAdversarial(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"16 GiB", 16 << 30, true},
		{"16GiB", 16 << 30, true},
		{"  16 GiB  ", 16 << 30, true},
		{"512 TiB", 512 << 40, true},
		{"8388607 TiB", (1<<23 - 1) << 40, true}, // largest whole-TiB value below 2^63
		{"8388608 TiB", 0, false},                // exactly 2^63 bytes: int64 overflow
		{"99999999999 TiB", 0, false},            // far past overflow
		{"-16 GiB", 0, false},
		{"0 GiB", 0, false},
		{"16 KiB", 0, false},
		{"16", 0, false},
		{"GiB", 0, false},
		{"1e3 GiB", 0, false},
		{"1.5.5 GiB", 0, false},
		{"", 0, false},
		{"sixteen GiB", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseMemory(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
		if ok && got <= 0 {
			t.Errorf("parseMemory(%q) accepted non-positive %d", tc.in, got)
		}
	}
}

// TestParsePriceDeterministicAcrossDimensions: a SKU with several hourly
// dimensions must always resolve to the same (lowest) price — Go map
// iteration order would otherwise make repeated syncs disagree.
func TestParsePriceDeterministicAcrossDimensions(t *testing.T) {
	doc := `{
	  "product": {"attributes": {"instanceType": "m5.xlarge", "vcpu": "4", "memory": "16 GiB"}},
	  "terms": {"OnDemand": {"X": {"priceDimensions": {
	    "A": {"unit": "Hrs", "pricePerUnit": {"USD": "0.20"}},
	    "B": {"unit": "Hrs", "pricePerUnit": {"USD": "0.10"}},
	    "C": {"unit": "Quantity", "pricePerUnit": {"USD": "0.01"}},
	    "D": {"unit": "Hrs", "pricePerUnit": {"USD": "Infinity"}},
	    "E": {"unit": "Hrs", "pricePerUnit": {"USD": "NaN"}},
	    "F": {"unit": "Hrs", "pricePerUnit": {"USD": "-3"}}
	  }}}}
	}`
	for i := 0; i < 50; i++ { // map order reshuffles every Unmarshal
		it, ok := ParsePriceListEntry(doc)
		if !ok {
			t.Fatal("entry rejected")
		}
		if it.HourlyUSD != 0.10 {
			t.Fatalf("iteration %d: price %v, want lowest hourly 0.10", i, it.HourlyUSD)
		}
	}
}

func TestSyncDedupesDuplicateSKUs(t *testing.T) {
	p := &fakePricing{pages: [][]string{
		{priceDoc("m5.xlarge", "4", "16 GiB", "0.30")},
		{priceDoc("m5.xlarge", "4", "16 GiB", "0.192"), priceDoc("c5.large", "2", "4 GiB", "0.085")},
	}}
	s := newWithClients("us-east-1", nil, p, &fakeEC2{})
	raw, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cat, err := kpricing.Load(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("catalog with duplicate SKUs must dedupe before render: %v", err)
	}
	if cat.Len() != 2 {
		t.Fatalf("catalog size %d, want 2", cat.Len())
	}
	m5, _ := cat.Lookup("aws", "m5.xlarge")
	if m5.HourlyUSD != 0.192 {
		t.Fatalf("dedupe must keep the cheapest quote, got %v", m5.HourlyUSD)
	}
}

// TestSyncInstancesDeterministic: two identical syncs must render identical
// instance lists (the comment carries a timestamp, so compare instances only).
func TestSyncInstancesDeterministic(t *testing.T) {
	build := func() []byte {
		p := &fakePricing{pages: [][]string{{
			priceDoc("m5.xlarge", "4", "16 GiB", "0.192"),
			priceDoc("c5.large", "2", "4 GiB", "0.085"),
			priceDoc("t3.medium", "2", "4 GiB", "0.0416"),
		}}}
		e := &fakeEC2{history: []ec2types.SpotPrice{
			spotEntry("m5.xlarge", "us-east-1a", "0.07", time.Unix(1700000000, 0)),
		}}
		raw, err := newWithClients("us-east-1", nil, p, e).Sync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Instances json.RawMessage `json:"instances"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Instances
	}
	a, b := build(), build()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("instances differ between identical syncs:\n%s\n---\n%s", a, b)
	}
}

// FuzzParsePriceListEntry: whatever the Pricing API (or an attacker-supplied
// price file) throws at the parser, an accepted entry must satisfy every
// invariant the catalog loader and planners rely on.
func FuzzParsePriceListEntry(f *testing.F) {
	f.Add(priceDoc("m5.xlarge", "4", "16 GiB", "0.192"))
	f.Add(priceDoc("t4g.small", "2", "2 GiB", "0.0168"))
	f.Add(priceDoc("m5.metal", "96", "384 GiB", "4.6"))
	f.Add(priceDoc("x.large", "-4", "16 GiB", "0.192"))
	f.Add(priceDoc("x.large", "4", "99999999999 TiB", "0.192"))
	f.Add(priceDoc("x.large", "4", "16 GiB", "Infinity"))
	f.Add(`{"terms":{"OnDemand":{"X":{"priceDimensions":{"Y":{"unit":"Hrs","pricePerUnit":{"USD":"1"}}}}}}}`)
	f.Add(`not json`)
	f.Fuzz(func(t *testing.T, raw string) {
		it, ok := ParsePriceListEntry(raw)
		if !ok {
			return
		}
		if it.Name == "" || strings.Contains(it.Name, "metal") {
			t.Fatalf("bad name accepted: %+v", it)
		}
		if it.Provider != "aws" {
			t.Fatalf("provider %q", it.Provider)
		}
		if it.MilliCPU <= 0 || it.MilliCPU%1000 != 0 {
			t.Fatalf("milliCPU %d not a positive whole-vCPU count", it.MilliCPU)
		}
		if it.MemoryBytes <= 0 {
			t.Fatalf("memoryBytes %d", it.MemoryBytes)
		}
		if it.HourlyUSD <= 0 || math.IsNaN(it.HourlyUSD) || math.IsInf(it.HourlyUSD, 0) {
			t.Fatalf("hourlyUSD %v", it.HourlyUSD)
		}
		if it.Arch != "amd64" && it.Arch != "arm64" {
			t.Fatalf("arch %q", it.Arch)
		}
		if want := strings.SplitN(it.Name, ".", 2)[0]; it.Family != want {
			t.Fatalf("family %q, want %q", it.Family, want)
		}
	})
}

func FuzzParseMemory(f *testing.F) {
	f.Add("16 GiB")
	f.Add("0.5 GiB")
	f.Add("512 MiB")
	f.Add("1.5 TiB")
	f.Add("99999999999 TiB")
	f.Add("-1 GiB")
	f.Add("garbage")
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := parseMemory(s)
		if ok && got <= 0 {
			t.Fatalf("parseMemory(%q) accepted non-positive %d", s, got)
		}
	})
}
