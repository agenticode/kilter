package pricing

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

// The contract of the consolidated tables: a rate table is region-keyed and
// says which region it came from; a fact that does not vary by region has no
// region to say. These tests are the teeth on that distinction, because it is
// the one a future sync pass is most likely to blur — the cheapest way to
// "support eu-west-1" is to return us-east-1 numbers with a eu-west-1 label,
// and that is exactly the silent mispricing this package exists to refuse.

// ratesAccessor is one region-keyed table, addressed generically so a new one
// cannot be added without the contract tests seeing it.
type ratesAccessor struct {
	name     string
	lookup   func(Region) (any, bool)
	baseline func() any
	regionOf func(any) Region
}

func allRateAccessors() []ratesAccessor {
	return []ratesAccessor{
		{
			name:     "LambdaRates",
			lookup:   func(r Region) (any, bool) { v, ok := LambdaRatesFor(r); return v, ok },
			baseline: func() any { return DefaultLambdaRates() },
			regionOf: func(v any) Region { return v.(LambdaRates).Region },
		},
		{
			name:     "EBSRates",
			lookup:   func(r Region) (any, bool) { v, ok := EBSRatesFor(r); return v, ok },
			baseline: func() any { return DefaultEBSRates() },
			regionOf: func(v any) Region { return v.(EBSRates).Region },
		},
		{
			name:     "FargateRates",
			lookup:   func(r Region) (any, bool) { v, ok := FargateRatesFor(r); return v, ok },
			baseline: func() any { return DefaultFargateRates() },
			regionOf: func(v any) Region { return v.(FargateRates).Region },
		},
		{
			name:     "FargateARMRates",
			lookup:   func(r Region) (any, bool) { v, ok := FargateARMRatesFor(r); return v, ok },
			baseline: func() any { return DefaultFargateARMRates() },
			regionOf: func(v any) Region { return v.(FargateARMRates).Region },
		},
		{
			name:     "EC2CreditRates",
			lookup:   func(r Region) (any, bool) { v, ok := EC2CreditRatesFor(r); return v, ok },
			baseline: func() any { return DefaultEC2CreditRates() },
			regionOf: func(v any) Region { return v.(EC2CreditRates).Region },
		},
	}
}

// A region this package has no rates for must be told so, and must never be
// handed a table that claims to be its own. The Region field always names the
// region the NUMBERS came from, not the one that was asked for.
func TestUnknownRegionIsReportedNotFabricated(t *testing.T) {
	unknown := []Region{"eu-west-1", "ap-northeast-1", "us-gov-west-1", "", "US-EAST-1", " us-east-1"}
	for _, a := range allRateAccessors() {
		got, ok := a.lookup(DefaultRegion)
		if !ok {
			t.Errorf("%s: the baseline region %q is not in the table", a.name, DefaultRegion)
		}
		if r := a.regionOf(got); r != DefaultRegion {
			t.Errorf("%s: baseline lookup returned Region %q", a.name, r)
		}
		if !reflect.DeepEqual(got, a.baseline()) {
			t.Errorf("%s: lookup of the baseline region disagrees with the Default accessor", a.name)
		}
		for _, region := range unknown {
			got, ok := a.lookup(region)
			if ok {
				t.Errorf("%s: claims verified rates for %q", a.name, region)
			}
			if r := a.regionOf(got); r != DefaultRegion {
				t.Errorf("%s: asked for %q, got a table labelled %q — it must be labelled with the "+
					"region the numbers are from, so a report cannot imply it priced %q",
					a.name, region, r, region)
			}
			if !reflect.DeepEqual(got, a.baseline()) {
				t.Errorf("%s: the fallback for %q is not the baseline", a.name, region)
			}
		}
	}
}

// Every rate type in this package is region-keyed, in the type. A new one that
// forgot its Region field would be a table nobody could label — the shape this
// test exists to prevent.
func TestEveryRateTypeCarriesItsRegion(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(LambdaRates{}),
		reflect.TypeOf(EBSRates{}),
		reflect.TypeOf(FargateRates{}),
		reflect.TypeOf(FargateARMRates{}),
		reflect.TypeOf(EC2CreditRates{}),
	}
	if len(types) != len(allRateAccessors()) {
		t.Fatalf("%d rate types but %d accessors: one of them is unreachable or unchecked",
			len(types), len(allRateAccessors()))
	}
	for _, rt := range types {
		f, ok := rt.FieldByName("Region")
		if !ok {
			t.Errorf("%s has no Region field: its numbers cannot be attributed to a region", rt.Name())
			continue
		}
		if f.Type != reflect.TypeOf(Region("")) {
			t.Errorf("%s.Region is %s, want pricing.Region", rt.Name(), f.Type)
		}
	}
}

// …and the facts that do NOT vary by region say so by having no Region to set.
// If GlobalFacts ever grows one, something region-dependent has been filed as
// a universal truth, and every region will quietly be billed us-east-1's
// number for it.
func TestGlobalFactsIsNotRegionKeyed(t *testing.T) {
	rt := reflect.TypeOf(GlobalFacts{})
	for i := range rt.NumField() {
		if strings.Contains(strings.ToLower(rt.Field(i).Name), "region") {
			t.Fatalf("GlobalFacts.%s: this type is the things that do not vary by region", rt.Field(i).Name)
		}
	}
	g := Global()
	if g.FargateOverheadBytes != FargateOverheadBytes ||
		g.LambdaFreeEphemeralStorageMB != LambdaFreeEphemeralStorageMB ||
		g.HoursPerMonth != HoursPerMonth {
		t.Fatalf("Global() = %+v does not report the constants it names", g)
	}
	if g.FargateOverheadBytes != 256<<20 {
		t.Errorf("Fargate overhead = %d bytes, want 256 MiB", g.FargateOverheadBytes)
	}
}

// The baseline accessors must return the constants, not a copy that has drifted
// from them. Every number in this file is reachable two ways — as a constant
// and through a table — and both must be the same number.
func TestBaselineTablesReturnTheDeclaredConstants(t *testing.T) {
	l := DefaultLambdaRates()
	if l.RequestUSDPerMillion != LambdaRequestUSDPerMillion ||
		l.X86GBSecondUSD != LambdaX86GBSecondUSD || l.ARMGBSecondUSD != LambdaARMGBSecondUSD {
		t.Errorf("DefaultLambdaRates() = %+v", l)
	}
	// The exact constant and the runtime division are the same price, one ULP
	// apart. Money is compared through a tolerance; the constant is the one
	// that is exact, and callers with a choice should take it.
	if LambdaRequestUSD != 2e-07 {
		t.Errorf("LambdaRequestUSD = %v, want exactly 2e-07", LambdaRequestUSD)
	}
	if got := l.RequestUSD(); math.Abs(got-LambdaRequestUSD) > 1e-15 {
		t.Errorf("RequestUSD() = %v, LambdaRequestUSD = %v", got, LambdaRequestUSD)
	}
	e := DefaultEBSRates()
	if e.GP2GBMonthUSD != EBSGP2GBMonthUSD || e.GP3GBMonthUSD != EBSGP3GBMonthUSD ||
		e.GP3IOPSMonthUSD != EBSGP3IOPSMonthUSD || e.GP3ThroughputMonthUSD != EBSGP3ThroughputMonthUSD ||
		e.IO1GBMonthUSD != EBSIO1GBMonthUSD || e.IO2GBMonthUSD != EBSIO2GBMonthUSD {
		t.Errorf("DefaultEBSRates() = %+v", e)
	}
	a := DefaultFargateARMRates()
	if a.VCPUHourlyUSD != FargateARMVCPUHourlyUSD || a.GBHourlyUSD != FargateARMGBHourlyUSD {
		t.Errorf("DefaultFargateARMRates() = %+v", a)
	}
	c := DefaultEC2CreditRates()
	if c.SurplusCreditUSDPerVCPUHour != EC2SurplusCreditUSDPerVCPUHour {
		t.Errorf("DefaultEC2CreditRates() = %+v", c)
	}
	if got, want := c.SurplusUSDPerCredit(), EC2SurplusCreditUSDPerVCPUHour/CreditsPerVCPUHour; got != want {
		t.Errorf("SurplusUSDPerCredit() = %v, want %v", got, want)
	}
	f := DefaultFargateRates()
	if f.VCPUHourlyUSD != FargateVCPUHourlyUSD || f.GBHourlyUSD != FargateGBHourlyUSD {
		t.Errorf("DefaultFargateRates() = %+v", f)
	}
	if f.Region != DefaultRegion || !f.Platform.Valid() {
		t.Errorf("DefaultFargateRates() = %+v, want the us-east-1 EKS platform", f)
	}
}

// Every embedded rate is a positive finite number. A zero rate mints savings
// out of nothing; a negative or non-finite one poisons every total downstream.
func TestEveryEmbeddedRateIsPositiveAndFinite(t *testing.T) {
	rates := map[string]float64{
		"LambdaRequestUSDPerMillion":     LambdaRequestUSDPerMillion,
		"LambdaX86GBSecondUSD":           LambdaX86GBSecondUSD,
		"LambdaARMGBSecondUSD":           LambdaARMGBSecondUSD,
		"FargateARMVCPUHourlyUSD":        FargateARMVCPUHourlyUSD,
		"FargateARMGBHourlyUSD":          FargateARMGBHourlyUSD,
		"EBSGP2GBMonthUSD":               EBSGP2GBMonthUSD,
		"EBSGP3GBMonthUSD":               EBSGP3GBMonthUSD,
		"EBSGP3IOPSMonthUSD":             EBSGP3IOPSMonthUSD,
		"EBSGP3ThroughputMonthUSD":       EBSGP3ThroughputMonthUSD,
		"EBSIO1GBMonthUSD":               EBSIO1GBMonthUSD,
		"EBSIO2GBMonthUSD":               EBSIO2GBMonthUSD,
		"EC2SurplusCreditUSDPerVCPUHour": EC2SurplusCreditUSDPerVCPUHour,
	}
	for name, v := range rates {
		if !(v > 0) || math.IsInf(v, 0) {
			t.Errorf("%s = %v: not a positive finite price", name, v)
		}
	}
	// Internal consistency AWS's own rate card has: gp3 capacity undercuts
	// gp2, io1/io2 exceed both, and arm64 undercuts x86 on both dimensions.
	if !(EBSGP3GBMonthUSD < EBSGP2GBMonthUSD) {
		t.Error("gp3 capacity is not cheaper than gp2: the gp2→gp3 case inverts")
	}
	if !(EBSIO1GBMonthUSD > EBSGP2GBMonthUSD) || !(EBSIO2GBMonthUSD > EBSGP2GBMonthUSD) {
		t.Error("io1/io2 capacity is not dearer than gp2")
	}
	if !(LambdaARMGBSecondUSD < LambdaX86GBSecondUSD) {
		t.Error("the arm64 Lambda rate is not below x86_64")
	}
	if !(FargateARMVCPUHourlyUSD < FargateVCPUHourlyUSD) || !(FargateARMGBHourlyUSD < FargateGBHourlyUSD) {
		t.Error("the ARM Fargate rates are not below x86")
	}
}

// AWS publishes the Fargate rates twice — per second and per hour — and
// docs/design/compute-domains.md §4.1 records both. This package embeds the
// per-hour pair; the per-second pair must still be the same price, to the
// precision AWS quotes it in.
//
// It is not exact, and that is the point of pinning it: $0.000001235/GB-s ×
// 3600 = $0.004446, while the per-hour quote is $0.004445. The per-hour figure
// is authoritative (0.004445/3600 = 0.00000123472…, which rounds to the quoted
// 0.000001235); the reverse direction is rounding noise. A future sync that
// swapped a per-second quote in as if it were per-hour would land outside this
// tolerance.
func TestFargatePerSecondAndPerHourQuotesAreOnePrice(t *testing.T) {
	for _, c := range []struct {
		name         string
		perHour      float64
		perSecond    float64
		quotedDigits float64 // 0.5 ulp of the per-second quote's last digit
	}{
		{"x86 vCPU", FargateVCPUHourlyUSD, 0.000011244, 5e-10},
		{"x86 GB", FargateGBHourlyUSD, 0.000001235, 5e-10},
		{"arm vCPU", FargateARMVCPUHourlyUSD, 0.0000089944, 5e-11},
		{"arm GB", FargateARMGBHourlyUSD, 0.0000009889, 5e-11},
	} {
		diff := math.Abs(c.perHour/3600 - c.perSecond)
		if diff > c.quotedDigits {
			t.Errorf("%s: %v/h is %v/s, but AWS quotes %v/s (off by %v, tolerance %v)",
				c.name, c.perHour, c.perHour/3600, c.perSecond, diff, c.quotedDigits)
		}
	}
}

// The rate tables serialize. A rate carried into a report or a checkpoint must
// round-trip, including the region label — a table that lost its Region on the
// way out becomes an unattributable number at the other end.
func TestRateTablesRoundTripThroughJSON(t *testing.T) {
	for _, v := range []any{DefaultLambdaRates(), DefaultEBSRates(), DefaultFargateARMRates(), DefaultEC2CreditRates()} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		out := reflect.New(reflect.TypeOf(v))
		if err := json.Unmarshal(b, out.Interface()); err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		if got := out.Elem().Interface(); !reflect.DeepEqual(got, v) {
			t.Errorf("%T round-tripped to %+v, want %+v", v, got, v)
		}
		if !strings.Contains(string(b), string(DefaultRegion)) {
			t.Errorf("%T serialized without its region: %s", v, b)
		}
	}
}

// A Fargate override file states rates, not a region. Loading one must not
// leave it labelled us-east-1 — that would attribute an operator's own numbers
// to a region they never named.
func TestLoadedFargateRatesCarryNoRegionClaim(t *testing.T) {
	r, err := LoadFargateRates(strings.NewReader(`{"vcpuHourlyUSD":0.05,"gbHourlyUSD":0.006}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Region != "" {
		t.Errorf("override rates claim region %q; the file never said one", r.Region)
	}
	if !r.Platform.Valid() || r.VCPUHourlyUSD != 0.05 || r.GBHourlyUSD != 0.006 {
		t.Errorf("override = %+v", r)
	}
	// And a file that tries to name one is rejected, like every other unknown
	// field: rates are data, provenance is not the file's to assert.
	if _, err := LoadFargateRates(strings.NewReader(
		`{"vcpuHourlyUSD":0.05,"gbHourlyUSD":0.006,"region":"eu-west-1"}`)); err == nil {
		t.Error("an override file was allowed to declare its own region")
	}
}
