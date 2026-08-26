package ecs

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

const eps = 1e-9

func closeTo(a, b float64) bool { return math.Abs(a-b) < eps }

// TestRoundUpTierIsQuantizeMinusOverhead is the load-bearing test for the
// quantizer reuse: RoundUpTier must equal "round up to the next valid Fargate
// task size" at every tier and at every one-byte boundary, while
// pricing.Quantize keeps adding the 256 MiB Kubernetes overhead for EKS.
//
// If someone ever re-derives the tier table inside this package, or if
// pricing.Quantize stops adding the overhead, this fails.
func TestRoundUpTierIsQuantizeMinusOverhead(t *testing.T) {
	configs := pricing.FargateConfigs()
	if len(configs) == 0 {
		t.Fatal("pricing exposes no Fargate configurations")
	}
	for _, c := range configs {
		// A task definition that already names a tier rounds to itself: ECS
		// bills exactly what the task definition declares.
		got, err := RoundUpTier(c.Resources())
		if err != nil {
			t.Fatalf("RoundUpTier(%s): %v", c, err)
		}
		if got != c {
			t.Errorf("RoundUpTier(%s) = %s, want the tier itself", c, got)
		}

		// One byte below the tier still rounds up to it.
		below := model.Resources{MilliCPU: c.MilliCPU, MemoryBytes: c.MemoryBytes() - 1}
		if got, err := RoundUpTier(below); err != nil || got != c {
			t.Errorf("RoundUpTier(%s) = %s, %v; want %s", below, got, err, c)
		}

		// One byte above must never round back to the same tier.
		above := model.Resources{MilliCPU: c.MilliCPU, MemoryBytes: c.MemoryBytes() + 1}
		got, err = RoundUpTier(above)
		if c == pricing.FargateMaxConfig {
			if !errors.Is(err, pricing.ErrFargateTooLarge) {
				t.Errorf("RoundUpTier past the ceiling = %s, %v; want ErrFargateTooLarge", got, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("RoundUpTier(%s): %v", above, err)
		}
		if got == c {
			t.Errorf("RoundUpTier(%s) stayed on %s: the boundary is not being crossed", above, c)
		}
	}

	// The sharp version: 1 vCPU / 8 GB. EKS adds 256 MiB and cannot fit 8.25 GB
	// in the 1-vCPU row, so it bills 2 vCPU / 9 GB (§4.1.1, AWS's own worked
	// example). ECS bills exactly 1 vCPU / 8 GB.
	req := model.Resources{MilliCPU: 1000, MemoryBytes: 8 << 30}
	eks, err := pricing.Quantize(req, model.Resources{})
	if err != nil {
		t.Fatalf("pricing.Quantize: %v", err)
	}
	if eks.MilliCPU != 2000 || eks.MemoryMiB != 9*1024 {
		t.Fatalf("EKS quantizer changed: 1 vCPU/8 GB → %s, want 2vCPU 9GB (§4.1.1)", eks)
	}
	ecs, err := RoundUpTier(req)
	if err != nil {
		t.Fatalf("RoundUpTier: %v", err)
	}
	if ecs.MilliCPU != 1000 || ecs.MemoryMiB != 8*1024 {
		t.Fatalf("ECS rounding of 1 vCPU/8 GB = %s, want 1vCPU 8GB: ECS has no Kubernetes overhead", ecs)
	}
}

// TestQuantizerReuseAgainstAWSDocumentedTiers pins the shared tier table
// against the values AWS documents, priced with the published rates.
func TestQuantizerReuseAgainstAWSDocumentedTiers(t *testing.T) {
	r := DefaultRates()
	cases := []struct {
		name        string
		need        model.Resources
		wantTier    string
		wantHourly  float64
		wantARMRate float64
	}{{
		// §4.1.1: the overhead cliff, as ECS does not have it.
		name: "1vCPU/8GB", need: model.Resources{MilliCPU: 1000, MemoryBytes: 8 << 30},
		wantTier: "1vCPU 8GB", wantHourly: 0.04048 + 8*0.004445,
		wantARMRate: 0.03238 + 8*0.00356,
	}, {
		// §4.1.3: 200 m / 512 MiB. On EKS the overhead pushes it to 1 GB; on
		// ECS 0.25 vCPU / 0.5 GB is a valid task size and is what is billed.
		name: "small", need: model.Resources{MilliCPU: 200, MemoryBytes: 512 << 20},
		wantTier: "0.25vCPU 0.5GB", wantHourly: 0.25*0.04048 + 0.5*0.004445,
		wantARMRate: 0.25*0.03238 + 0.5*0.00356,
	}, {
		// The 8-vCPU row steps by 4 GB: 17 GB is not a task size, 20 GB is.
		name: "8vCPU-step", need: model.Resources{MilliCPU: 8000, MemoryBytes: 17 << 30},
		wantTier: "8vCPU 20GB", wantHourly: 8*0.04048 + 20*0.004445,
		wantARMRate: 8*0.03238 + 20*0.00356,
	}, {
		// The 16-vCPU row steps by 8 GB.
		name: "16vCPU-step", need: model.Resources{MilliCPU: 16000, MemoryBytes: 33 << 30},
		wantTier: "16vCPU 40GB", wantHourly: 16*0.04048 + 40*0.004445,
		wantARMRate: 16*0.03238 + 40*0.00356,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RoundUpTier(tc.need)
			if err != nil {
				t.Fatalf("RoundUpTier(%s): %v", tc.need, err)
			}
			if got.String() != tc.wantTier {
				t.Fatalf("RoundUpTier(%s) = %s, want %s", tc.need, got, tc.wantTier)
			}
			if h := r.Cost(got, PlatformX86OnDemand); !closeTo(h, tc.wantHourly) {
				t.Errorf("x86 on-demand cost = %.8f, want %.8f", h, tc.wantHourly)
			}
			arm := r.Cost(got, Platform{Arch: ArchARM64, Market: MarketOnDemand})
			if !closeTo(arm, tc.wantARMRate) {
				t.Errorf("arm on-demand cost = %.8f, want %.8f", arm, tc.wantARMRate)
			}
			// ARM is ~20 % cheaper across both dimensions (§4.5).
			if ratio := arm / tc.wantHourly; ratio > 0.81 || ratio < 0.79 {
				t.Errorf("arm/x86 = %.4f, want ≈0.80", ratio)
			}
		})
	}
}

// TestX86OnDemandCostMatchesTheEKSEngine proves the shared price function:
// the same tier, at the same x86 on-demand rates, costs the same in both
// domains. The economics engine is reused, not forked.
func TestX86OnDemandCostMatchesTheEKSEngine(t *testing.T) {
	eks := pricing.DefaultFargateRates()
	ecs := DefaultRates()
	for _, c := range pricing.FargateConfigs() {
		want := eks.Cost(c)
		got := ecs.Cost(c, PlatformX86OnDemand)
		if !closeTo(got, want) {
			t.Fatalf("tier %s: ecs x86 on-demand $%.8f, eks $%.8f", c, got, want)
		}
	}
}

// TestEKSFargateStillRefusesSpotAndARM is the §7 trap 3 pin on the OTHER side
// of the seam. ECS gaining Spot and ARM must not have leaked either into the
// EKS Fargate domain, where AWS supports neither. The refusal is structural —
// the types cannot express them — so this test reads the types.
func TestEKSFargateStillRefusesSpotAndARM(t *testing.T) {
	// 1. pricing.Platform has exactly one inhabitant.
	if !pricing.EKSLinuxX86.Valid() {
		t.Fatal("pricing.EKSLinuxX86 is not valid")
	}
	if (pricing.Platform{}).Valid() {
		t.Error("the zero pricing.Platform is valid: EKS Fargate would accept an unknown platform")
	}
	if s := pricing.EKSLinuxX86.String(); !strings.Contains(s, "x86_64") || !strings.Contains(s, "on-demand") {
		t.Errorf("pricing.EKSLinuxX86 = %q, want an x86 on-demand identifier", s)
	}

	// 2. pricing.FargateRates has no ARM rate and no spot discount to set.
	rt := reflect.TypeOf(pricing.FargateRates{})
	for i := range rt.NumField() {
		n := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(n, "arm") || strings.Contains(n, "spot") || strings.Contains(n, "graviton") {
			t.Errorf("pricing.FargateRates gained field %q: EKS Fargate has no ARM and no Spot",
				rt.Field(i).Name)
		}
	}

	// 3. A rate-override file that tries to introduce either is rejected
	//    loudly, not ignored.
	for _, body := range []string{
		`{"vcpuHourlyUSD":0.04,"gbHourlyUSD":0.004,"spotDiscount":0.7}`,
		`{"vcpuHourlyUSD":0.04,"gbHourlyUSD":0.004,"armVCPUHourlyUSD":0.032}`,
	} {
		if _, err := pricing.LoadFargateRates(strings.NewReader(body)); err == nil {
			t.Errorf("pricing.LoadFargateRates accepted an ECS-only dimension: %s", body)
		}
	}

	// 4. And this package's Platform is not convertible to pricing's, so an
	//    ECS advisory cannot be fed to the EKS pricer by accident.
	if reflect.TypeOf(PlatformX86OnDemand).ConvertibleTo(reflect.TypeOf(pricing.EKSLinuxX86)) {
		t.Error("ecs.Platform converts to pricing.Platform: the two domains can borrow each other's levers")
	}
}

// TestSpotAndARMArePriceableHere is the ECS half of the same pin: both
// dimensions exist, both are cheaper, and both are reachable from this
// package's types.
func TestSpotAndARMArePriceableHere(t *testing.T) {
	r := DefaultRates()
	tier, err := RoundUpTier(model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30})
	if err != nil {
		t.Fatal(err)
	}
	od := r.Cost(tier, PlatformX86OnDemand)
	arm := r.Cost(tier, Platform{Arch: ArchARM64, Market: MarketOnDemand})
	spot := r.Cost(tier, Platform{Arch: ArchX86, Market: MarketSpot})
	armSpot := r.Cost(tier, Platform{Arch: ArchARM64, Market: MarketSpot})

	if !(arm < od) {
		t.Errorf("arm $%.6f is not cheaper than x86 $%.6f", arm, od)
	}
	if !(spot < od) {
		t.Errorf("spot $%.6f is not cheaper than on-demand $%.6f", spot, od)
	}
	// ARM + Spot compose: AWS added Fargate Spot for Graviton in 2024-09.
	if !closeTo(armSpot, arm*(1-DefaultSpotDiscount)) {
		t.Errorf("arm spot $%.6f, want arm × (1−discount) = $%.6f", armSpot, arm*(1-DefaultSpotDiscount))
	}
	if !(armSpot < spot && armSpot < arm) {
		t.Errorf("arm+spot $%.6f is not the cheapest of $%.6f/$%.6f", armSpot, spot, arm)
	}
}

// TestSpotDiscountUnderPromises pins the deliberate conservatism: the shipped
// default must stay well under AWS's advertised "up to 70 %", because an
// advisory that over-promises is worse than one that under-promises.
func TestSpotDiscountUnderPromises(t *testing.T) {
	if DefaultSpotDiscount >= 0.70 {
		t.Errorf("DefaultSpotDiscount = %v: never assume AWS's advertised ceiling", DefaultSpotDiscount)
	}
	if DefaultSpotDiscount <= 0 || DefaultSpotDiscount >= 1 {
		t.Errorf("DefaultSpotDiscount = %v is out of range", DefaultSpotDiscount)
	}
}

// TestRatesGarbageFallsBackPerDimension: a zero, negative or non-finite rate
// must fall back to the embedded baseline rather than price a tier at $0 and
// mint an infinite saving.
func TestRatesGarbageFallsBackPerDimension(t *testing.T) {
	d := DefaultRates()
	for _, r := range []Rates{
		{},
		{VCPUHourlyUSD: -1, GBHourlyUSD: math.NaN(), ArmVCPUHourlyUSD: math.Inf(1), SpotDiscount: math.NaN()},
		{VCPUHourlyUSD: 0.04, SpotDiscount: 1.5},
	} {
		got := r.withDefaults()
		if !positiveFinite(got.VCPUHourlyUSD) || !positiveFinite(got.GBHourlyUSD) ||
			!positiveFinite(got.ArmVCPUHourlyUSD) || !positiveFinite(got.ArmGBHourlyUSD) {
			t.Fatalf("withDefaults(%+v) = %+v: a rate is not positive-finite", r, got)
		}
		if got.SpotDiscount < 0 || got.SpotDiscount >= 1 {
			t.Fatalf("withDefaults(%+v) spot discount %v out of range", r, got.SpotDiscount)
		}
		if r.VCPUHourlyUSD <= 0 && got.VCPUHourlyUSD != d.VCPUHourlyUSD {
			t.Fatalf("garbage vcpu rate did not fall back to the baseline")
		}
	}
	// An invalid platform prices as the most expensive option, so a wiring bug
	// over-states the current bill instead of inventing a saving.
	tier := pricing.FargateMinConfig
	if got, want := d.Cost(tier, Platform{}), d.Cost(tier, PlatformX86OnDemand); !closeTo(got, want) {
		t.Errorf("invalid platform priced $%.8f, want the x86 on-demand $%.8f", got, want)
	}
}

// TestTierForRejectsNonTiers: a service whose task definition does not name a
// valid Fargate task size has no honest denominator and no honest bill.
func TestTierForRejectsNonTiers(t *testing.T) {
	if _, err := TierFor(model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30}); err != nil {
		t.Fatalf("1 vCPU / 2 GB is a valid task size: %v", err)
	}
	for _, bad := range []model.Resources{
		{MilliCPU: 1000, MemoryBytes: 1500 << 20}, // 1.46 GB is not a step
		{MilliCPU: 750, MemoryBytes: 2 << 30},     // 0.75 vCPU is not a row
		{},
	} {
		if _, err := TierFor(bad); err == nil {
			t.Errorf("TierFor(%s) accepted a non-tier", bad)
		}
	}
}

// TestParseTaskSizes covers both spellings RegisterTaskDefinition accepts, and
// pins that unreadable ones are errors rather than guesses — a guessed
// denominator poisons every percentage in the window.
func TestParseTaskSizes(t *testing.T) {
	cpu := map[string]int64{
		"256": 250, "1024": 1000, "4096": 4000, "16384": 16000,
		"0.25 vCPU": 250, "1vCPU": 1000, "  2 VCPU ": 2000,
	}
	for in, want := range cpu {
		got, err := ParseTaskCPU(in)
		if err != nil || got != want {
			t.Errorf("ParseTaskCPU(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	mem := map[string]int64{
		"512": 512 << 20, "2048": 2 << 30, "30720": 30 << 30,
		"0.5GB": 512 << 20, "2 GB": 2 << 30, "8gb": 8 << 30, "1024 MB": 1 << 30,
	}
	for in, want := range mem {
		got, err := ParseTaskMemory(in)
		if err != nil || got != want {
			t.Errorf("ParseTaskMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "lots", "-1", "0", "1e400", "NaN", "vCPU"} {
		if _, err := ParseTaskCPU(bad); !errors.Is(err, ErrTaskSize) {
			t.Errorf("ParseTaskCPU(%q) = %v, want ErrTaskSize", bad, err)
		}
		if _, err := ParseTaskMemory(bad); !errors.Is(err, ErrTaskSize) {
			t.Errorf("ParseTaskMemory(%q) = %v, want ErrTaskSize", bad, err)
		}
	}
}

// TestFormatTaskSizeRoundTrips: whatever the sizer proposes must parse back to
// the same tier, because those strings are what RegisterTaskDefinition sends.
func TestFormatTaskSizeRoundTrips(t *testing.T) {
	for _, c := range pricing.FargateConfigs() {
		cpu, err := ParseTaskCPU(FormatTaskCPU(c))
		if err != nil {
			t.Fatalf("tier %s: %v", c, err)
		}
		mem, err := ParseTaskMemory(FormatTaskMemory(c))
		if err != nil {
			t.Fatalf("tier %s: %v", c, err)
		}
		if got := (model.Resources{MilliCPU: cpu, MemoryBytes: mem}); got != c.Resources() {
			t.Errorf("tier %s round-tripped to %s", c, got)
		}
	}
}

// TestModeVocabularyMatchesGuard keeps this package's tag semantics identical
// to the Kubernetes annotation's. Two vocabularies for one guardrail would be
// worse than either.
func TestModeVocabularyMatchesGuard(t *testing.T) {
	if modeOff != guard.ModeOff || modeRecommend != guard.ModeRecommend || modeApply != guard.ModeApply {
		t.Fatalf("mode vocabulary drifted from pkg/guard: %q/%q/%q", modeOff, modeRecommend, modeApply)
	}
	if got := modeFor(nil, ""); got != modeApply {
		t.Errorf("untagged service with no default = %q, want %q (opt-out semantics)", got, modeApply)
	}
	if got := modeFor(map[string]string{TagKilterMode: "nonsense"}, modeRecommend); got != modeRecommend {
		t.Errorf("unrecognized tag value = %q, want the configured default", got)
	}
	if got := modeFor(map[string]string{TagKilterMode: modeOff}, modeApply); got != modeOff {
		t.Errorf("mode=off tag = %q, want off", got)
	}
}

// TestPlatformVersionGate: LATEST and empty are current; anything older than
// 1.4.0 blocks the 8- and 16-vCPU task sizes, and an unreadable version reads
// as too old.
func TestPlatformVersionGate(t *testing.T) {
	ok := []string{"", "LATEST", "latest", "1.4.0", "1.10.0", "2.0.0"}
	bad := []string{"1.3.0", "1.0.0", "0.9", "garbage", "1"}
	for _, v := range ok {
		if !platformVersionAtLeast(v, 1, 4) {
			t.Errorf("platformVersionAtLeast(%q) = false, want true", v)
		}
	}
	for _, v := range bad {
		if platformVersionAtLeast(v, 1, 4) {
			t.Errorf("platformVersionAtLeast(%q) = true, want false", v)
		}
	}
}
