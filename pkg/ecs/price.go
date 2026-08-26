package ecs

import (
	"fmt"
	"math"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// --- The ECS-only dimensions ----------------------------------------------
//
// pkg/pricing.Platform has exactly one value, EKSLinuxX86, and no way to make
// another: on EKS Fargate there is no Spot and no Graviton, so "move these pods
// to Fargate Spot/ARM" — the most common unactionable Fargate advice — is
// unrepresentable there rather than merely discouraged (§7 trap 3).
//
// ECS Fargate has both, so both exist here, as types. An ECS [Platform] cannot
// be converted to a pricing.Platform and vice versa; the two domains cannot
// borrow each other's levers by accident.

// Arch is the CPU architecture a Fargate task runs under
// (`runtimePlatform.cpuArchitecture` in the task definition).
type Arch string

const (
	// ArchX86 is X86_64, the default.
	ArchX86 Arch = "X86_64"
	// ArchARM64 is Graviton. ECS only [verified: fargate/pricing +
	// whats-new 2024/09]; EKS Fargate cannot run it at all.
	ArchARM64 Arch = "ARM64"
)

// Market is the capacity market a task is billed under.
type Market string

const (
	// MarketOnDemand is the FARGATE capacity provider.
	MarketOnDemand Market = "on-demand"
	// MarketSpot is the FARGATE_SPOT capacity provider — interruptible, ECS
	// only ("Amazon EKS doesn't support Fargate Spot" [verified]).
	MarketSpot Market = "spot"
)

// OSFamily is the task definition's `runtimePlatform.operatingSystemFamily`.
// It gates both ECS-only levers: Windows tasks have neither ARM64 nor Spot,
// and they bill on a 5-minute minimum with license fees on top (§4.1).
type OSFamily string

const (
	OSLinux   OSFamily = "LINUX"
	OSWindows OSFamily = "WINDOWS"
)

// Platform is the billing platform of an ECS Fargate task: an architecture
// crossed with a market. The zero value is invalid.
type Platform struct {
	Arch   Arch   `json:"arch"`
	Market Market `json:"market"`
}

// PlatformX86OnDemand is the default platform, and the only one whose price
// equals what the same tier costs on EKS Fargate.
var PlatformX86OnDemand = Platform{Arch: ArchX86, Market: MarketOnDemand}

// Valid reports whether p names a platform this package can price.
func (p Platform) Valid() bool {
	return (p.Arch == ArchX86 || p.Arch == ArchARM64) &&
		(p.Market == MarketOnDemand || p.Market == MarketSpot)
}

// String renders the platform for reports and spec attributes.
func (p Platform) String() string { return string(p.Arch) + "/" + string(p.Market) }

// ARM Fargate rates, us-east-1, Linux, on-demand: −20 % vCPU and −19.9 % GB
// against x86.
//
// The numbers live in pkg/pricing beside the x86 Fargate rates, as
// [pricing.FargateARMRates] — a type EKS code cannot reach, so consolidating
// them did not merge the products: pricing.FargateRates still has no ARM field
// and TestEKSFargateStillRefusesSpotAndARM still fails if it grows one. These
// names are kept, and kept constant, so nothing that referenced them changes.
const (
	ARMVCPUHourlyUSD = pricing.FargateARMVCPUHourlyUSD
	ARMGBHourlyUSD   = pricing.FargateARMGBHourlyUSD
)

// DefaultSpotDiscount is the Fargate Spot discount this package assumes.
//
// AWS advertises "up to 70 %" [verified: fargate/pricing], and "up to" is not a
// number anyone may bill against: the realized Spot discount varies by region,
// architecture and moment. So the shipped default is deliberately well under
// the advertised ceiling. An advisory that under-promises and is beaten is a
// good surprise; one that over-promises is the reason nobody trusts optimizers.
// Operators who have measured their own rate set [Rates.SpotDiscount].
const DefaultSpotDiscount = 0.50

// Rates prices ECS Fargate tiers. Unlike pkg/pricing.FargateRates — which has
// no ARM rate and no spot field because EKS has neither — this struct carries
// all three dimensions, and that difference is the type-level statement of
// §7 trap 3.
type Rates struct {
	// VCPUHourlyUSD and GBHourlyUSD are the x86 on-demand rates. They default
	// to pkg/pricing's embedded values, so ECS and EKS can never disagree about
	// what an x86 on-demand tier costs.
	VCPUHourlyUSD float64 `json:"vcpuHourlyUSD"`
	GBHourlyUSD   float64 `json:"gbHourlyUSD"`
	// ArmVCPUHourlyUSD and ArmGBHourlyUSD price Graviton tasks.
	ArmVCPUHourlyUSD float64 `json:"armVCPUHourlyUSD"`
	ArmGBHourlyUSD   float64 `json:"armGBHourlyUSD"`
	// SpotDiscount is the fraction taken off the on-demand price, in [0,1).
	SpotDiscount float64 `json:"spotDiscount"`
}

// DefaultRates returns the embedded baseline. Every rate comes from
// pkg/pricing — the x86 pair from [pricing.DefaultFargateRates], the ARM pair
// from [pricing.DefaultFargateARMRates]: one rate table, two domains. Only
// SpotDiscount originates here, because it is not an AWS rate (see
// [DefaultSpotDiscount]).
func DefaultRates() Rates {
	base := pricing.DefaultFargateRates()
	arm := pricing.DefaultFargateARMRates()
	return Rates{
		VCPUHourlyUSD:    base.VCPUHourlyUSD,
		GBHourlyUSD:      base.GBHourlyUSD,
		ArmVCPUHourlyUSD: arm.VCPUHourlyUSD,
		ArmGBHourlyUSD:   arm.GBHourlyUSD,
		SpotDiscount:     DefaultSpotDiscount,
	}
}

// withDefaults substitutes the embedded baseline for unset or nonsensical
// fields, one dimension at a time. A garbage rate must never silently price a
// tier at zero and mint a saving.
func (r Rates) withDefaults() Rates {
	d := DefaultRates()
	if !positiveFinite(r.VCPUHourlyUSD) {
		r.VCPUHourlyUSD = d.VCPUHourlyUSD
	}
	if !positiveFinite(r.GBHourlyUSD) {
		r.GBHourlyUSD = d.GBHourlyUSD
	}
	if !positiveFinite(r.ArmVCPUHourlyUSD) {
		r.ArmVCPUHourlyUSD = d.ArmVCPUHourlyUSD
	}
	if !positiveFinite(r.ArmGBHourlyUSD) {
		r.ArmGBHourlyUSD = d.ArmGBHourlyUSD
	}
	if !(r.SpotDiscount >= 0) || r.SpotDiscount >= 1 { // catches NaN
		r.SpotDiscount = d.SpotDiscount
	}
	return r
}

func positiveFinite(f float64) bool { return f > 0 && !math.IsInf(f, 0) && !math.IsNaN(f) }

// Cost returns the hourly USD price of a tier on a platform.
//
// The price function itself — P(v,g) = v·rate_vcpu + g·rate_gb (§4.1) — is
// pkg/pricing's, evaluated through pricing.FargateRates.Cost, so an ECS bill
// and an EKS bill can never disagree about the arithmetic. Only the rates
// differ, and only ECS has more than one set of them.
//
// An invalid platform prices as x86 on-demand: the most expensive of the four,
// so a wiring bug over-states the current bill and under-states the saving.
func (r Rates) Cost(c pricing.FargateConfig, p Platform) float64 {
	r = r.withDefaults()
	vcpu, gb := r.VCPUHourlyUSD, r.GBHourlyUSD
	if p.Arch == ArchARM64 {
		vcpu, gb = r.ArmVCPUHourlyUSD, r.ArmGBHourlyUSD
	}
	// The Platform stamp below is required by the struct and ignored by Cost,
	// which reads the two rates and nothing else. It is emphatically NOT a
	// claim that ARM or Spot exist on EKS: those live in this package's
	// [Platform], which has no pricing.Platform equivalent by design.
	base := pricing.FargateRates{
		Platform:      pricing.EKSLinuxX86,
		VCPUHourlyUSD: vcpu,
		GBHourlyUSD:   gb,
	}.Cost(c)
	if p.Market == MarketSpot {
		base *= 1 - r.SpotDiscount
	}
	return base
}

// MonthlyCost is Cost scaled by the billing-average month, using pkg/pricing's
// constant so every domain's "monthly" means the same thing.
func (r Rates) MonthlyCost(c pricing.FargateConfig, p Platform) float64 {
	return r.Cost(c, p) * pricing.HoursPerMonth
}

// --- Quantizer reuse -------------------------------------------------------

// RoundUpTier maps an ECS task's requested cpu/memory to the task size AWS
// bills it at.
//
// ECS and EKS share one tier table — AWS ships the EKS pod-configuration table
// as the ECS task-size table, down to the 4-GB steps at 8 vCPU and the 8-GB
// steps at 16 vCPU (§4.1) — but they do NOT share the +256 MiB overhead. That
// memory pays for kubelet, kube-proxy and containerd; an ECS task has none of
// them, so an ECS task definition asking for exactly 1 vCPU / 8 GB bills
// 1 vCPU / 8 GB, where the identical EKS pod bills 2 vCPU / 9 GB (§4.1.1).
//
// The rounding is [pricing.Quantize] — the shipped, exhaustively table-tested
// U1 quantizer — with the Kubernetes overhead cancelled out of the input:
//
//	Quantize(need − FargateOverheadBytes) ≡ roundUpToTier(need)
//
// exactly, for need ≥ FargateOverheadBytes, and for smaller needs both sides
// land on the smallest tier. TestRoundUpTierIsQuantizeMinusOverhead pins the
// identity at every tier and at every ±1-byte boundary.
//
// Cancelling one constant rather than re-deriving the table is deliberate. A
// second copy of the tiers would be a second thing to get wrong, and the two
// tables have to move together anyway: they are one AWS table.
//
// A need above 16 vCPU / 120 GB returns [pricing.ErrFargateTooLarge]. Pricing
// such a task at the ceiling would invent a bill for a task that will never
// start.
func RoundUpTier(need model.Resources) (pricing.FargateConfig, error) {
	return pricing.Quantize(model.Resources{
		MilliCPU: max(need.MilliCPU, 0),
		// Saturating floor at 0 happens inside pricing.Quantize; subtracting
		// below zero here is safe and lands on the smallest tier, which is
		// also what rounding a sub-overhead need up produces.
		MemoryBytes: max(need.MemoryBytes, 0) - pricing.FargateOverheadBytes,
	}, model.Resources{})
}

// TierFor returns the valid tier exactly matching a task definition's declared
// cpu/memory. ECS validates task size at RegisterTaskDefinition time, so a
// running Fargate service's reservation IS a tier; anything else means the
// service is not on Fargate, or the collector read the wrong fields, and
// guessing which would put a fabricated denominator under every percentage in
// the window.
func TierFor(r model.Resources) (pricing.FargateConfig, error) {
	c, ok := pricing.FargateConfigFor(r)
	if !ok {
		return pricing.FargateConfig{}, fmt.Errorf("%w: %s is not a valid Fargate task size", ErrTaskSize, r)
	}
	return c, nil
}
