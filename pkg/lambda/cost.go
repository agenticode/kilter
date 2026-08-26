package lambda

import (
	"fmt"
	"math"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Embedded Lambda rates, us-east-1, on-demand: $0.20 per 1M requests;
// $0.0000166667 per GB-second on x86_64 and $0.0000133334 per GB-second on
// arm64 — a 20 % lower RATE, not a 20 % lower bill (see [Rates.ArmRateDelta]).
//
// The numbers themselves now live in pkg/pricing beside the Fargate and
// instance rate tables (FINDINGS.md §10 asked for this move; U9 could not make
// it because it may not edit that package). These names are kept, and kept
// constant, so nothing that referenced them has to change; they are one
// definition away from the single source of truth, and
// pricing.TestNoRateLiteralsInDomainPackages fails if a literal reappears here.
//
// Like pkg/pricing's instance catalog these are a baseline for relative math;
// exact billing belongs to the invoice. Region-aware lookup is
// [pricing.LambdaRatesFor]; [DefaultRates] is the us-east-1 baseline.
const (
	RequestUSDPerMillion   = pricing.LambdaRequestUSDPerMillion
	X86GBSecondUSD         = pricing.LambdaX86GBSecondUSD
	ARMGBSecondUSD         = pricing.LambdaARMGBSecondUSD
	FreeEphemeralStorageMB = pricing.LambdaFreeEphemeralStorageMB
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

// DefaultRates returns the embedded baseline rates, read from pkg/pricing's
// us-east-1 Lambda table.
func DefaultRates() Rates {
	p := pricing.DefaultLambdaRates()
	return Rates{
		// The exact constant, not p.RequestUSD(): the two differ by one ULP
		// (see pricing.LambdaRequestUSD) and the baseline should be the exact
		// one wherever it is available.
		RequestUSD:     pricing.LambdaRequestUSD,
		GBSecondUSD:    p.X86GBSecondUSD,
		ArmGBSecondUSD: p.ARMGBSecondUSD,
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
			// From r, NOT from the RequestUSDPerMillion constant. The
			// duration line above already prices from r, so quoting the
			// constant here made an operator-supplied Config.Rates.RequestUSD
			// honoured by [Rates.InvocationUSD] and ignored by the commitment
			// waterfall — the same requests priced two ways in one report.
			// Pinned by TestLambdaRequestLineHonoursOverriddenRates.
			ODRate: r.RequestUSD * 1e6,
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
