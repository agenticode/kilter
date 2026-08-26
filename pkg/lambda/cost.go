package lambda

import (
	"fmt"
	"math"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Embedded Lambda rates, us-east-1, on-demand
// [verified: https://aws.amazon.com/lambda/pricing/ fetched 2026-08-25]:
// $0.20 per 1M requests; $0.0000166667 per GB-second on x86_64 and
// $0.0000133334 per GB-second on arm64 — a 20 % lower RATE, not a 20 % lower
// bill (see [Rates.ArmRateDelta]).
//
// Like pkg/pricing's instance catalog these are a baseline for relative math;
// exact billing belongs to the invoice. They live here rather than in
// pkg/pricing/catalog.json only because this unit may not edit that package;
// FINDINGS.md records the move a later unit should make.
const (
	RequestUSDPerMillion   = 0.20
	X86GBSecondUSD         = 0.0000166667
	ARMGBSecondUSD         = 0.0000133334
	FreeEphemeralStorageMB = 512
)

// Rates prices Lambda usage. There is no spot rate and no reserved rate:
// Lambda has neither.
type Rates struct {
	// RequestUSD is the charge for ONE request.
	RequestUSD float64 `json:"requestUSD"`
	// GBSecondUSD is the x86_64 duration rate.
	GBSecondUSD float64 `json:"gbSecondUSD"`
	// ArmGBSecondUSD is the arm64 duration rate.
	ArmGBSecondUSD float64 `json:"armGBSecondUSD"`
}

// DefaultRates returns the embedded baseline rates.
func DefaultRates() Rates {
	return Rates{
		RequestUSD:     RequestUSDPerMillion / 1e6,
		GBSecondUSD:    X86GBSecondUSD,
		ArmGBSecondUSD: ARMGBSecondUSD,
	}
}

func (r Rates) validate() error {
	switch {
	case !finite(r.RequestUSD) || r.RequestUSD < 0:
		return fmt.Errorf("lambda: RequestUSD must be a non-negative number, got %v", r.RequestUSD)
	case !finite(r.GBSecondUSD) || r.GBSecondUSD <= 0:
		return fmt.Errorf("lambda: GBSecondUSD must be positive, got %v", r.GBSecondUSD)
	case !finite(r.ArmGBSecondUSD) || r.ArmGBSecondUSD <= 0:
		return fmt.Errorf("lambda: ArmGBSecondUSD must be positive, got %v", r.ArmGBSecondUSD)
	}
	return nil
}

// GBSecond returns the duration rate for an architecture. An unknown
// architecture is priced as x86_64 — the more expensive of the two, so an
// unrecognized value can never manufacture a discount.
func (r Rates) GBSecond(arch string) float64 {
	if arch == ArchARM {
		return r.ArmGBSecondUSD
	}
	return r.GBSecondUSD
}

// ArmRateDelta is the fraction by which the arm64 rate is below x86_64. It is a
// RATE ratio and nothing more: the duration a function runs at on Graviton is
// not this number, is not observable from x86 metrics, and is what decides
// whether the bill actually falls.
func (r Rates) ArmRateDelta() float64 {
	if r.GBSecondUSD <= 0 {
		return 0
	}
	return (r.GBSecondUSD - r.ArmGBSecondUSD) / r.GBSecondUSD
}

// BillableMS rounds a measured duration up to Lambda's 1 ms billing
// granularity, with a 1 ms minimum. Only ever applied to a duration we do not
// have a billed value for; when the REPORT line carries Billed Duration, that
// is the authority.
func BillableMS(ms float64) float64 {
	if !finite(ms) || ms <= MinBilledMS {
		return MinBilledMS
	}
	return math.Ceil(ms)
}

// GBSeconds is the billable GB-second quantity of one invocation.
func GBSeconds(memoryMB int64, billedMS float64) float64 {
	if memoryMB <= 0 || !finite(billedMS) || billedMS <= 0 {
		return 0
	}
	return (float64(memoryMB) / 1024) * (billedMS / 1000)
}

// InvocationUSD is the cost of one invocation: the request charge plus
// memory × duration at the architecture's GB-second rate.
//
// This is the whole billing model, and the reason this package refuses so
// much: the only free variable a recommender controls is memoryMB, and
// billedMS is a FUNCTION of it that no metric reveals.
func (r Rates) InvocationUSD(arch string, memoryMB int64, billedMS float64) float64 {
	return r.RequestUSD + GBSeconds(memoryMB, billedMS)*r.GBSecond(arch)
}

// HourlyUSD prices a rate of invocations at one operating point.
func (r Rates) HourlyUSD(invocationsPerHour float64, arch string, memoryMB int64, billedMS float64) float64 {
	if !finite(invocationsPerHour) || invocationsPerHour <= 0 {
		return 0
	}
	return invocationsPerHour * r.InvocationUSD(arch, memoryMB, billedMS)
}

// MonthlyUSD projects an hourly cost onto a billing-average month.
func MonthlyUSD(hourly float64) float64 { return hourly * HoursPerMonth }

// MemoryFloorMB is the smallest configurable memory setting that clears an
// observed max-memory-used with headroom, rounded UP to stepMB and clamped to
// the platform range.
//
// The floor is a lower bound on safety, never a recommendation on its own: the
// setting that does not OOM and the setting that costs least are different
// questions, and this package answers only the first one from a single
// operating point.
func MemoryFloorMB(maxUsedMB int64, headroom float64, stepMB int64) int64 {
	if maxUsedMB <= 0 {
		return MinMemoryMB
	}
	if !finite(headroom) || headroom < 1 {
		headroom = 1
	}
	if stepMB < 1 {
		stepMB = 1
	}
	need := int64(math.Ceil(float64(maxUsedMB) * headroom))
	if r := need % stepMB; r != 0 {
		need += stepMB - r
	}
	return ClampMemoryMB(need)
}

// usageLines renders one function's hourly usage as commitment usage lines: a
// GB-second duration line and a requests line, matching the units
// pkg/pricing/commit documents for KindLambda.
//
// Requests are included even though no memory change moves them, because the
// waterfall bills the whole picture: Compute Savings Plans absorb Lambda
// duration account-wide (§4.4), and a line omitted from `before` is a line the
// commitment appears not to have covered.
func usageLines(ref domain.TargetRef, region, arch string, r Rates,
	invocationsPerHour, gbSecondsPerHour float64) []commit.UsageLine {

	if !finite(invocationsPerHour) || invocationsPerHour < 0 {
		invocationsPerHour = 0
	}
	if !finite(gbSecondsPerHour) || gbSecondsPerHour < 0 {
		gbSecondsPerHour = 0
	}
	return []commit.UsageLine{
		{
			ID:       ref.ID + "/duration",
			Kind:     commit.KindLambda,
			Region:   region,
			Unit:     "GB-Seconds",
			Quantity: gbSecondsPerHour,
			ODRate:   r.GBSecond(arch),
		},
		{
			ID:       ref.ID + "/requests",
			Kind:     commit.KindLambda,
			Region:   region,
			Unit:     "Requests-Millions",
			Quantity: invocationsPerHour / 1e6,
			ODRate:   RequestUSDPerMillion,
		},
	}
}

// finite maps NaN and ±Inf to false. Garbage arithmetic must not be able to
// travel into a savings claim.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// zeroIfNotFinite is the value-returning form.
func zeroIfNotFinite(f float64) float64 {
	if !finite(f) {
		return 0
	}
	return f
}
