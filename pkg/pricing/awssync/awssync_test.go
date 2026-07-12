package awssync

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"

	kpricing "github.com/agenticode/kilter/pkg/pricing"
)

func priceDoc(instanceType, vcpu, memory, usd string) string {
	return fmt.Sprintf(`{
	  "product": {"attributes": {"instanceType": %q, "vcpu": %q, "memory": %q}},
	  "terms": {"OnDemand": {"X": {"priceDimensions": {"Y": {"unit": "Hrs", "pricePerUnit": {"USD": %q}}}}}}
	}`, instanceType, vcpu, memory, usd)
}

func TestParsePriceListEntry(t *testing.T) {
	it, ok := ParsePriceListEntry(priceDoc("m5.xlarge", "4", "16 GiB", "0.192"))
	if !ok {
		t.Fatal("valid entry rejected")
	}
	if it.MilliCPU != 4000 || it.MemoryBytes != 16<<30 || it.HourlyUSD != 0.192 {
		t.Fatalf("parsed wrong: %+v", it)
	}
	if it.Family != "m5" || it.Arch != "amd64" || it.Burstable {
		t.Fatalf("classification wrong: %+v", it)
	}

	arm, _ := ParsePriceListEntry(priceDoc("m7g.large", "2", "8 GiB", "0.0816"))
	if arm.Arch != "arm64" {
		t.Fatalf("graviton arch: %+v", arm)
	}
	gd, _ := ParsePriceListEntry(priceDoc("c6gd.large", "2", "4 GiB", "0.0768"))
	if gd.Arch != "arm64" {
		t.Fatalf("c6gd should be arm64: %+v", gd)
	}
	gpu, _ := ParsePriceListEntry(priceDoc("g4dn.xlarge", "4", "16 GiB", "0.526"))
	if gpu.Arch != "amd64" {
		t.Fatalf("g4dn is x86: %+v", gpu)
	}
	burst, _ := ParsePriceListEntry(priceDoc("t3.medium", "2", "4 GiB", "0.0416"))
	if !burst.Burstable {
		t.Fatal("t3 must be burstable")
	}

	for _, bad := range []string{
		priceDoc("m5.metal", "96", "384 GiB", "4.6"),
		priceDoc("m5.xlarge", "0", "16 GiB", "0.192"),
		priceDoc("m5.xlarge", "4", "sixteen", "0.192"),
		priceDoc("m5.xlarge", "4", "16 GiB", "0"),
		"not json",
	} {
		if _, ok := ParsePriceListEntry(bad); ok {
			t.Fatalf("should reject: %.60s", bad)
		}
	}
}

func TestParseMemoryUnits(t *testing.T) {
	cases := map[string]int64{
		"16 GiB":  16 << 30,
		"512 MiB": 512 << 20,
		"1.5 TiB": 3 << 39,
		"0.5 GiB": 1 << 29,
	}
	for in, want := range cases {
		got, ok := parseMemory(in)
		if !ok || got != want {
			t.Fatalf("%q → %d ok=%v, want %d", in, got, ok, want)
		}
	}
}

type fakePricing struct{ pages [][]string }

func (f *fakePricing) GetProducts(ctx context.Context, in *pricing.GetProductsInput,
	_ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	if len(f.pages) == 0 {
		return &pricing.GetProductsOutput{}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	out := &pricing.GetProductsOutput{PriceList: page}
	if len(f.pages) > 0 {
		tok := "more"
		out.NextToken = &tok
	}
	return out, nil
}

type fakeEC2 struct{ history []ec2types.SpotPrice }

func (f *fakeEC2) DescribeSpotPriceHistory(ctx context.Context, in *ec2.DescribeSpotPriceHistoryInput,
	_ ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error) {
	return &ec2.DescribeSpotPriceHistoryOutput{SpotPriceHistory: f.history}, nil
}

func spotEntry(itype, az, price string, at time.Time) ec2types.SpotPrice {
	return ec2types.SpotPrice{
		InstanceType: ec2types.InstanceType(itype), AvailabilityZone: &az,
		SpotPrice: &price, Timestamp: &at,
	}
}

func TestSyncEndToEnd(t *testing.T) {
	now := time.Now()
	p := &fakePricing{pages: [][]string{
		{priceDoc("m5.xlarge", "4", "16 GiB", "0.192"), priceDoc("t3.medium", "2", "4 GiB", "0.0416")},
		{priceDoc("c5.large", "2", "4 GiB", "0.085")},
	}}
	e := &fakeEC2{history: []ec2types.SpotPrice{
		spotEntry("m5.xlarge", "us-east-1a", "0.0700", now),
		spotEntry("m5.xlarge", "us-east-1a", "0.0900", now.Add(-time.Hour)), // stale, ignored
		spotEntry("m5.xlarge", "us-east-1b", "0.0740", now),
		spotEntry("weird.type", "us-east-1a", "not-a-number", now),
	}}
	s := newWithClients("us-east-1", nil, p, e)
	raw, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The output must load through the standard catalog loader.
	cat, err := kpricing.Load(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("generated catalog does not load: %v\n%s", err, raw)
	}
	if cat.Len() != 3 {
		t.Fatalf("catalog size %d", cat.Len())
	}
	m5, ok := cat.Lookup("aws", "m5.xlarge")
	if !ok || m5.HourlyUSD != 0.192 {
		t.Fatalf("m5: %+v", m5)
	}
	// Spot = average of the two AZs' latest (0.070, 0.074).
	if diff := m5.SpotHourlyUSD - 0.072; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("spot avg: %v", m5.SpotHourlyUSD)
	}
	t3, _ := cat.Lookup("aws", "t3.medium")
	if !t3.Burstable {
		t.Fatal("burstable flag lost through sync")
	}
}

func TestSyncFamilyFilter(t *testing.T) {
	p := &fakePricing{pages: [][]string{{
		priceDoc("m5.xlarge", "4", "16 GiB", "0.192"),
		priceDoc("c5.large", "2", "4 GiB", "0.085"),
	}}}
	s := newWithClients("us-east-1", []string{"c5"}, p, &fakeEC2{})
	raw, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := kpricing.Load(bytes.NewReader(raw))
	if cat.Len() != 1 {
		t.Fatalf("family filter failed: %d entries", cat.Len())
	}
}

func TestSyncEmptyFails(t *testing.T) {
	s := newWithClients("us-east-1", nil, &fakePricing{}, &fakeEC2{})
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("empty result must fail loudly")
	}
}
