package commit

import "testing"

// TestNormalizationUnitsDocumentedTable is exhaustive over the normalization
// factor table in apply_ri.html. These numbers decide how much of a regional
// reservation a given instance absorbs, so a single wrong row silently
// mis-prices every recommendation in that family.
func TestNormalizationUnitsDocumentedTable(t *testing.T) {
	table := map[string]float64{
		"nano": 0.25, "micro": 0.5, "small": 1, "medium": 2, "large": 4,
		"xlarge": 8, "2xlarge": 16, "3xlarge": 24, "4xlarge": 32, "6xlarge": 48,
		"8xlarge": 64, "9xlarge": 72, "10xlarge": 80, "12xlarge": 96,
		"16xlarge": 128, "18xlarge": 144, "24xlarge": 192, "32xlarge": 256,
		"48xlarge": 384, "56xlarge": 448, "96xlarge": 768, "112xlarge": 896,
	}
	if len(sizeUnits) != len(table) {
		t.Fatalf("sizeUnits has %d rows, the documented table has %d", len(sizeUnits), len(table))
	}
	for size, want := range table {
		got, ok := NormalizationUnits(size)
		if !ok {
			t.Errorf("NormalizationUnits(%q): not found", size)
			continue
		}
		near(t, got, want, 1e-12, "NormalizationUnits("+size+")")
		// Case and whitespace must not change a billing decision.
		if up, ok := NormalizationUnits(" " + size + " "); !ok || up != got {
			t.Errorf("NormalizationUnits(%q) is not whitespace-insensitive", size)
		}
	}
}

func TestNormalizationUnitsUnknownAndExtrapolated(t *testing.T) {
	// Undocumented <N>xlarge sizes extrapolate as 8×N, which reproduces every
	// documented row; a newly launched size must not silently lose coverage.
	for size, want := range map[string]float64{"5xlarge": 40, "64xlarge": 512, "128xlarge": 1024} {
		got, ok := NormalizationUnits(size)
		if !ok {
			t.Errorf("NormalizationUnits(%q): want extrapolation, got not-found", size)
			continue
		}
		near(t, got, want, 1e-12, "NormalizationUnits("+size+")")
	}
	// Anything else is unknown, which costs only flexibility, never accuracy.
	for _, size := range []string{"", "metal", "huge", "xxlarge", "0xlarge", "-2xlarge", "xlargexlarge"} {
		if u, ok := NormalizationUnits(size); ok {
			t.Errorf("NormalizationUnits(%q) = %v, want not-found", size, u)
		}
	}
}

// TestInstanceUnitsMetalTable covers the bare-metal table: `metal` has no
// single factor, it inherits the equivalent virtualized size in its family.
func TestInstanceUnitsMetalTable(t *testing.T) {
	for _, tc := range []struct {
		instanceType string
		want         float64
	}{
		{"a1.metal", 32},
		{"z1d.metal", 96}, {"m5zn.metal", 96}, {"x2iezn.metal", 96},
		{"i3.metal", 128}, {"c6g.metal", 128}, {"x2gd.metal", 128},
		{"c5n.metal", 144},
		{"m5.metal", 192}, {"r5b.metal", 192}, {"i3en.metal", 192},
		{"c6i.metal", 256}, {"m6id.metal", 256},
		{"u-18tb1.metal", 448}, {"u-24tb1.metal", 448},
		{"u-6tb1.metal", 896}, {"u-12tb1.metal", 896},
		// Virtualized equivalents from the doc's own worked example.
		{"i3.16xlarge", 128}, {"i3.8xlarge", 64}, {"i3.4xlarge", 32},
		{"m5.xlarge", 8}, {"m3.2xlarge", 16}, {"t2.medium", 2}, {"t2.small", 1},
	} {
		got, ok := InstanceUnits(tc.instanceType)
		if !ok {
			t.Errorf("InstanceUnits(%q): not found", tc.instanceType)
			continue
		}
		near(t, got, tc.want, 1e-12, "InstanceUnits("+tc.instanceType+")")
	}
	// An unlisted bare-metal type resolves to unknown: the reservation then
	// applies on exact match only, which is safe, not wrong.
	for _, unknown := range []string{"zz9.metal", "metal", "m5", "", "m5."} {
		if u, ok := InstanceUnits(unknown); ok {
			t.Errorf("InstanceUnits(%q) = %v, want not-found", unknown, u)
		}
	}
}

func TestFamilyAndSizeOf(t *testing.T) {
	for _, tc := range []struct{ in, family, size string }{
		{"m5.xlarge", "m5", "xlarge"},
		{"M5.XLarge", "m5", "xlarge"},
		{"u7i-6tb.112xlarge", "u7i-6tb", "112xlarge"},
		{"i3.metal", "i3", "metal"},
		{"m5", "", ""},
		{"", "", ""},
		{".large", "", "large"},
	} {
		if got := FamilyOf(tc.in); got != tc.family {
			t.Errorf("FamilyOf(%q) = %q, want %q", tc.in, got, tc.family)
		}
		if got := SizeOf(tc.in); got != tc.size {
			t.Errorf("SizeOf(%q) = %q, want %q", tc.in, got, tc.size)
		}
	}
}

// TestSizeFlexibilityLimitations is the limitations list from apply_ri.html.
// Getting any of these wrong means claiming a reservation floats across sizes
// when it does not — which manufactures absorption that the invoice denies.
func TestSizeFlexibilityLimitations(t *testing.T) {
	base := ReservedInstance{ID: "ri", Count: 1, InstanceType: "m5.xlarge",
		Region: "us-east-1", Platform: "Linux/UNIX", Tenancy: "default"}
	if !base.SizeFlexible() {
		t.Fatal("regional Linux default-tenancy m5 must be size-flexible")
	}
	for _, tc := range []struct {
		name  string
		muted func(ReservedInstance) ReservedInstance
	}{
		{"zonal", func(r ReservedInstance) ReservedInstance { r.AZ = "us-east-1a"; return r }},
		{"windows", func(r ReservedInstance) ReservedInstance { r.Platform = "Windows"; return r }},
		{"rhel", func(r ReservedInstance) ReservedInstance { r.Platform = "RHEL"; return r }},
		{"suse", func(r ReservedInstance) ReservedInstance { r.Platform = "SUSE Linux"; return r }},
		{"dedicated", func(r ReservedInstance) ReservedInstance { r.Tenancy = "dedicated"; return r }},
		{"host", func(r ReservedInstance) ReservedInstance { r.Tenancy = "host"; return r }},
		{"g4dn", func(r ReservedInstance) ReservedInstance { r.InstanceType = "g4dn.xlarge"; return r }},
		{"g5g", func(r ReservedInstance) ReservedInstance { r.InstanceType = "g5g.xlarge"; return r }},
		{"p5", func(r ReservedInstance) ReservedInstance { r.InstanceType = "p5.48xlarge"; return r }},
		{"inf2", func(r ReservedInstance) ReservedInstance { r.InstanceType = "inf2.xlarge"; return r }},
		{"u7i-6tb", func(r ReservedInstance) ReservedInstance { r.InstanceType = "u7i-6tb.112xlarge"; return r }},
		{"unknown-size", func(r ReservedInstance) ReservedInstance { r.InstanceType = "zz9.plural"; return r }},
	} {
		if tc.muted(base).SizeFlexible() {
			t.Errorf("%s: must NOT be size-flexible", tc.name)
		}
	}
	// Not excluded despite looking similar: m6g is Graviton, not a G-family
	// GPU instance; g4dn is excluded but g4dn's neighbours in other families
	// are not.
	for _, ok := range []string{"m6g.xlarge", "c6gd.xlarge", "r6g.xlarge", "x2gd.xlarge"} {
		r := base
		r.InstanceType = ok
		if !r.SizeFlexible() {
			t.Errorf("%s: must be size-flexible", ok)
		}
	}
}

// TestNonFlexibleRegionalRIStillAppliesOnExactMatch: losing size flexibility
// is not losing the reservation. A Windows regional RI is AZ-flexible and
// size-locked.
func TestNonFlexibleRegionalRIStillAppliesOnExactMatch(t *testing.T) {
	inv := &Inventory{RIs: []ReservedInstance{{
		ID: "ri-win", Count: 2, InstanceType: "m5.large", Region: "us-east-1",
		Platform: "Windows", Tenancy: "default", EffectiveHourlyUSD: 0.10,
	}}}
	u := Usage{Lines: []UsageLine{
		{ID: "exact-other-az", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1c",
			InstanceType: "m5.large", Platform: "Windows", Quantity: 2, ODRate: 0.192},
		{ID: "bigger-same-family", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
			InstanceType: "m5.xlarge", Platform: "Windows", Quantity: 1, ODRate: 0.384},
	}}
	c := inv.Bill(u)
	assertPartition(t, c)
	near(t, covOf(t, c, "exact-other-az").RIQty, 2, 1e-9, "AZ flexibility applies")
	near(t, covOf(t, c, "bigger-same-family").RIQty, 0, 1e-9, "size flexibility does not")
	near(t, c.OnDemandUSD, 0.384, 1e-9, "the xlarge bills on-demand")
}

// TestZonalRIRequiresExactAvailabilityZoneAndSize.
func TestZonalRIRequiresExactAvailabilityZoneAndSize(t *testing.T) {
	inv := &Inventory{RIs: []ReservedInstance{{
		ID: "zonal", Count: 2, InstanceType: "c4.xlarge", Region: "us-east-1",
		AZ: "us-east-1a", Platform: "Linux/UNIX", EffectiveHourlyUSD: 0.12,
	}}}
	u := Usage{Lines: []UsageLine{
		{ID: "match", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
			InstanceType: "c4.xlarge", Quantity: 1, ODRate: 0.199},
		{ID: "other-az", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1b",
			InstanceType: "c4.xlarge", Quantity: 1, ODRate: 0.199},
		{ID: "other-size-same-az", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
			InstanceType: "c4.large", Quantity: 1, ODRate: 0.10},
	}}
	c := inv.Bill(u)
	assertPartition(t, c)
	near(t, covOf(t, c, "match").RIQty, 1, 1e-9, "exact match covered")
	near(t, covOf(t, c, "other-az").RIQty, 0, 1e-9, "zonal RI is not AZ-flexible")
	near(t, covOf(t, c, "other-size-same-az").RIQty, 0, 1e-9, "zonal RI is not size-flexible")
	// One of the two reservations goes unused, and the bill says so.
	near(t, c.StrandedUSD, 0.12, 1e-9, "half the zonal reservation is stranded")
}

func TestNormalizePlatformAndTenancy(t *testing.T) {
	for _, in := range []string{"", "Linux", "linux", "LINUX/UNIX", "Amazon Linux",
		"Amazon Linux 2", " amazon linux 2023 ", "Linux/UNIX (Amazon VPC)"} {
		if got := NormalizePlatform(in); got != PlatformLinux {
			t.Errorf("NormalizePlatform(%q) = %q, want %q", in, got, PlatformLinux)
		}
	}
	for _, in := range []string{"Windows", "RHEL", "SUSE Linux Enterprise Server"} {
		if got := NormalizePlatform(in); got == PlatformLinux {
			t.Errorf("NormalizePlatform(%q) must not fold to Linux/UNIX", in)
		}
	}
	for _, in := range []string{"", "default", "Default", "shared", " SHARED "} {
		if got := NormalizeTenancy(in); got != TenancyDefault {
			t.Errorf("NormalizeTenancy(%q) = %q, want %q", in, got, TenancyDefault)
		}
	}
	for _, in := range []string{"dedicated", "host"} {
		if got := NormalizeTenancy(in); got == TenancyDefault {
			t.Errorf("NormalizeTenancy(%q) must not fold to default", in)
		}
	}
}
