// Rate tables for the products that are priced per-unit rather than per-node:
// Lambda duration and requests, EBS capacity/IOPS/throughput, EC2 burstable
// surplus credits, and the ARM Fargate rates that exist on ECS only.
//
// # Why these live here
//
// Four domain packages (pkg/lambda, pkg/ecs, pkg/ebs, pkg/ec2) each needed
// rates before this package could be edited, so each inlined its own copy of
// the numbers. Same numbers, four files, no single source of truth — which is
// how a pricing engine starts disagreeing with itself: one table gets a region
// added and the others do not, and two domains then report different costs for
// the same resource. The numbers now live in this file exactly once; the domain
// packages keep their exported constant names but define them from these, so
// callers see no API change and `grep` sees one definition.
// TestNoRateLiteralsInDomainPackages fails if a rate literal reappears.
//
// # Region
//
// Every AWS price in this file varies by region, so every rate type here
// carries a [Region] and is looked up with a ...RatesFor(Region) accessor that
// reports whether the region was actually found. The embedded tables hold
// [DefaultRegion] only: that is a data gap (this package is air-gapped and
// ships what was verified), not a design one, and the accessors are the seam
// `kilter pricing sync-aws` fills. A caller asking for a region that is not in
// the table is TOLD so and gets a baseline whose Region field says us-east-1,
// rather than a fabricated regional price.
//
// The billing inputs that genuinely do not vary by region — the Fargate
// memory overhead, Lambda's free ephemeral storage, the billing-average month
// — are [GlobalFacts], which has no Region field and no per-region accessor.
// That asymmetry is the documentation.

package pricing

// Region is an AWS region identifier, e.g. "us-east-1".
type Region string

// DefaultRegion is the region every embedded rate in this package is quoted
// in. Like the instance catalog, the embedded numbers are a baseline good for
// relative savings math; exact billing belongs to your invoice.
const DefaultRegion Region = "us-east-1"

// --- Lambda ----------------------------------------------------------------

// Embedded Lambda rates, us-east-1, on-demand
// [verified: https://aws.amazon.com/lambda/pricing/ fetched 2026-08-25]:
// $0.20 per 1M requests; $0.0000166667 per GB-second on x86_64 and
// $0.0000133334 per GB-second on arm64 — a 20 % lower RATE, not a 20 % lower
// bill, because the duration a function runs at on Graviton is not observable
// from x86 metrics (see lambda.Rates.ArmRateDelta).
//
// Lambda has no spot rate and no reserved rate: the product has neither.
const (
	LambdaRequestUSDPerMillion = 0.20
	LambdaX86GBSecondUSD       = 0.0000166667
	LambdaARMGBSecondUSD       = 0.0000133334
)

// LambdaRates is the Lambda rate table for one region.
type LambdaRates struct {
	// Region is the region these numbers are quoted in — always the region
	// the rates actually came from, never the region that was asked for.
	Region Region `json:"region"`
	// RequestUSDPerMillion is the charge per million requests. It is held per
	// million, not per request, because that is the unit AWS quotes and the
	// unit pkg/pricing/commit bills a KindLambda requests line in; dividing
	// once at the edge beats rounding a 2e-7 constant in four places.
	RequestUSDPerMillion float64 `json:"requestUSDPerMillion"`
	// X86GBSecondUSD and ARMGBSecondUSD are the duration rates.
	X86GBSecondUSD float64 `json:"x86GBSecondUSD"`
	ARMGBSecondUSD float64 `json:"armGBSecondUSD"`
}

// LambdaRequestUSD is the charge for ONE request: the same price as
// [LambdaRequestUSDPerMillion], in the unit one invocation is priced in.
//
// It is a constant expression, not a runtime division, and that is deliberate.
// Go evaluates constant arithmetic at arbitrary precision, so this is exactly
// 2e-07; dividing the same number at runtime yields 2.0000000000000002e-07.
// One ULP is nothing to a bill and everything to a comparison, and two call
// paths quoting one price at two values is precisely the class of
// disagreement this file exists to remove. Callers pricing an invocation
// should use this; [LambdaRates.RequestUSD] is its per-table equivalent for
// rates that did not come from the embedded constants.
const LambdaRequestUSD = LambdaRequestUSDPerMillion / 1e6

// RequestUSD is the charge for ONE request. For the embedded baseline prefer
// the [LambdaRequestUSD] constant: this division is done in float64 and lands
// one ULP away from it. Money from this method is compared through
// commit.Eps, never with ==.
func (r LambdaRates) RequestUSD() float64 { return r.RequestUSDPerMillion / 1e6 }

var lambdaRates = map[Region]LambdaRates{
	DefaultRegion: {
		Region:               DefaultRegion,
		RequestUSDPerMillion: LambdaRequestUSDPerMillion,
		X86GBSecondUSD:       LambdaX86GBSecondUSD,
		ARMGBSecondUSD:       LambdaARMGBSecondUSD,
	},
}

// DefaultLambdaRates returns the embedded [DefaultRegion] baseline.
func DefaultLambdaRates() LambdaRates { return lambdaRates[DefaultRegion] }

// LambdaRatesFor returns the embedded Lambda rates for a region. ok is false
// when this package has no verified rates for that region; the returned value
// is then the [DefaultRegion] baseline, with its Region field saying so, so a
// report can label it honestly instead of implying it priced the caller's
// region.
func LambdaRatesFor(region Region) (LambdaRates, bool) {
	r, ok := lambdaRates[region]
	if !ok {
		return DefaultLambdaRates(), false
	}
	return r, true
}

// --- Fargate, the ECS-only ARM rates ---------------------------------------

// ARM Fargate rates, us-east-1, Linux, on-demand
// [verified: https://aws.amazon.com/fargate/pricing/ — $0.0000089944/vCPU-s and
// $0.0000009889/GB-s, expressed here per hour]: −20 % vCPU and −19.9 % GB
// against the x86 rates ([FargateVCPUHourlyUSD], [FargateGBHourlyUSD]).
const (
	FargateARMVCPUHourlyUSD = 0.03238
	FargateARMGBHourlyUSD   = 0.00356
)

// FargateARMRates prices Graviton Fargate tiers, for one region.
//
// It is a SEPARATE type from [FargateRates] on purpose, and the separation is
// the same §7-trap-3 statement pkg/ecs makes: EKS Fargate has no ARM (and no
// Spot), so [FargateRates] must never grow an ARM field and no EKS code path
// may be able to reach one. Consolidating the numbers here does not merge the
// products — [Catalog.FargatePodHourlyCost] still cannot see these rates, and
// TestEKSFargateHasNoSpotAndNoArm still fails if [FargateRates] gains a field
// whose name says otherwise.
//
// Only pkg/ecs, which models ECS Fargate, should read this.
type FargateARMRates struct {
	Region        Region  `json:"region"`
	VCPUHourlyUSD float64 `json:"vcpuHourlyUSD"`
	GBHourlyUSD   float64 `json:"gbHourlyUSD"`
}

var fargateARMRates = map[Region]FargateARMRates{
	DefaultRegion: {
		Region:        DefaultRegion,
		VCPUHourlyUSD: FargateARMVCPUHourlyUSD,
		GBHourlyUSD:   FargateARMGBHourlyUSD,
	},
}

// DefaultFargateARMRates returns the embedded [DefaultRegion] baseline.
func DefaultFargateARMRates() FargateARMRates { return fargateARMRates[DefaultRegion] }

// FargateARMRatesFor returns the ECS-only ARM Fargate rates for a region,
// with the same ok-means-verified contract as [LambdaRatesFor].
func FargateARMRatesFor(region Region) (FargateARMRates, bool) {
	r, ok := fargateARMRates[region]
	if !ok {
		return DefaultFargateARMRates(), false
	}
	return r, true
}

// FargateRatesFor returns the x86 on-demand Fargate rates for a region, with
// the same ok-means-verified contract as [LambdaRatesFor]. These are the rates
// both EKS and ECS bill an x86 on-demand tier at — one table, two domains, so
// the two can never disagree about what the same tier costs.
func FargateRatesFor(region Region) (FargateRates, bool) {
	r, ok := fargateRates[region]
	if !ok {
		return DefaultFargateRates(), false
	}
	return r, true
}

var fargateRates = map[Region]FargateRates{
	DefaultRegion: DefaultFargateRates(),
}

// --- EBS -------------------------------------------------------------------

// Embedded EBS rates, us-east-1, USD
// [verified: https://aws.amazon.com/ebs/pricing/, fetched 2026-08-25]. gp3
// charges per GB-month plus per provisioned IOPS and MBps ABOVE the included
// gp3 baseline; that baseline (3,000 IOPS / 125 MiB/s) is a device property
// owned by pkg/ebs, not a price, and deliberately does not live here.
const (
	EBSGP2GBMonthUSD         = 0.10
	EBSGP3GBMonthUSD         = 0.08
	EBSGP3IOPSMonthUSD       = 0.005
	EBSGP3ThroughputMonthUSD = 0.06
	EBSIO1GBMonthUSD         = 0.125
	EBSIO2GBMonthUSD         = 0.125
)

// EBSRates is the EBS rate table for one region. It carries money only: the
// gp3 free allowances and the gp2 performance model are device behaviour and
// stay in pkg/ebs.
type EBSRates struct {
	Region        Region  `json:"region"`
	GP2GBMonthUSD float64 `json:"gp2GBMonthUSD"`
	GP3GBMonthUSD float64 `json:"gp3GBMonthUSD"`
	// GP3IOPSMonthUSD is charged per provisioned IOPS above the gp3 baseline.
	GP3IOPSMonthUSD float64 `json:"gp3IOPSMonthUSD"`
	// GP3ThroughputMonthUSD is charged per provisioned MBps above the gp3
	// baseline.
	GP3ThroughputMonthUSD float64 `json:"gp3ThroughputMonthUSD"`
	// IO1GBMonthUSD and IO2GBMonthUSD are the io1/io2 capacity rates, carried
	// for the io1/io2 → gp3 advisory pkg/ebs defers. Neither includes the
	// per-IOPS charge those volume types also carry.
	IO1GBMonthUSD float64 `json:"io1GBMonthUSD"`
	IO2GBMonthUSD float64 `json:"io2GBMonthUSD"`
}

var ebsRates = map[Region]EBSRates{
	DefaultRegion: {
		Region:                DefaultRegion,
		GP2GBMonthUSD:         EBSGP2GBMonthUSD,
		GP3GBMonthUSD:         EBSGP3GBMonthUSD,
		GP3IOPSMonthUSD:       EBSGP3IOPSMonthUSD,
		GP3ThroughputMonthUSD: EBSGP3ThroughputMonthUSD,
		IO1GBMonthUSD:         EBSIO1GBMonthUSD,
		IO2GBMonthUSD:         EBSIO2GBMonthUSD,
	},
}

// DefaultEBSRates returns the embedded [DefaultRegion] baseline.
func DefaultEBSRates() EBSRates { return ebsRates[DefaultRegion] }

// EBSRatesFor returns the embedded EBS rates for a region, with the same
// ok-means-verified contract as [LambdaRatesFor].
func EBSRatesFor(region Region) (EBSRates, bool) {
	r, ok := ebsRates[region]
	if !ok {
		return DefaultEBSRates(), false
	}
	return r, true
}

// --- EC2 burstable surplus credits -----------------------------------------

// EC2SurplusCreditUSDPerVCPUHour is the Linux unlimited-mode surplus rate:
// a T-instance running above its baseline utilization bills surplus CPU
// credits at this rate [verified: aws.amazon.com/ec2/instance-types/t3/ +
// EC2 unlimited-mode docs]. It is what makes the §7 trap 5 "burstable sticker
// price mirage" quantifiable: cost(u) = sticker + max(0, u − baseline) ×
// vCPUs × this rate.
const EC2SurplusCreditUSDPerVCPUHour = 0.05

// CreditsPerVCPUHour is how many CPU credits one vCPU-hour of surplus is: a
// credit is one vCPU-minute. Not a price and not region-dependent — the unit
// definition that converts the rate above into a per-credit charge.
const CreditsPerVCPUHour = 60

// EC2CreditRates is the burstable-surplus rate table for one region.
type EC2CreditRates struct {
	Region Region `json:"region"`
	// SurplusCreditUSDPerVCPUHour is the Linux unlimited-mode surplus rate.
	SurplusCreditUSDPerVCPUHour float64 `json:"surplusCreditUSDPerVCPUHour"`
}

// SurplusUSDPerCredit is the per-credit price of surplus: one credit is one
// vCPU-minute, so this is the vCPU-hour rate over [CreditsPerVCPUHour].
func (r EC2CreditRates) SurplusUSDPerCredit() float64 {
	return r.SurplusCreditUSDPerVCPUHour / CreditsPerVCPUHour
}

var ec2CreditRates = map[Region]EC2CreditRates{
	DefaultRegion: {
		Region:                      DefaultRegion,
		SurplusCreditUSDPerVCPUHour: EC2SurplusCreditUSDPerVCPUHour,
	},
}

// DefaultEC2CreditRates returns the embedded [DefaultRegion] baseline.
func DefaultEC2CreditRates() EC2CreditRates { return ec2CreditRates[DefaultRegion] }

// EC2CreditRatesFor returns the embedded surplus-credit rates for a region,
// with the same ok-means-verified contract as [LambdaRatesFor].
func EC2CreditRatesFor(region Region) (EC2CreditRates, bool) {
	r, ok := ec2CreditRates[region]
	if !ok {
		return DefaultEC2CreditRates(), false
	}
	return r, true
}

// --- The inputs that are not region-aware ----------------------------------

// LambdaFreeEphemeralStorageMB is the /tmp allowance included in every
// function's price; storage above it is billed separately
// [verified: https://aws.amazon.com/lambda/pricing/]. An allowance, not a
// rate — the same number in every region.
const LambdaFreeEphemeralStorageMB int64 = 512

// GlobalFacts are the billing inputs that do NOT vary by AWS region: product
// definitions rather than prices.
//
// Every rate type in this file carries a [Region] and is reached through a
// ...RatesFor(Region) accessor. This type has neither, and there is no
// GlobalFactsFor(region) to call — that absence is the statement. A caller
// cannot be handed one of these believing it was looked up for their region,
// because there was nothing to look up.
type GlobalFacts struct {
	// FargateOverheadBytes is the memory EKS Fargate adds to every pod's
	// request before rounding to a billable tier. A quantization input, not a
	// price: which tier a pod lands on is an AWS scheduling fact.
	FargateOverheadBytes int64 `json:"fargateOverheadBytes"`
	// LambdaFreeEphemeralStorageMB is the included /tmp allowance.
	LambdaFreeEphemeralStorageMB int64 `json:"lambdaFreeEphemeralStorageMB"`
	// HoursPerMonth is the billing-average month (8760 h/year ÷ 12) every
	// domain's "monthly" figure is computed with. A calendar convention.
	HoursPerMonth int `json:"hoursPerMonth"`
}

// Global returns the region-independent billing inputs.
func Global() GlobalFacts {
	return GlobalFacts{
		FargateOverheadBytes:         FargateOverheadBytes,
		LambdaFreeEphemeralStorageMB: LambdaFreeEphemeralStorageMB,
		HoursPerMonth:                HoursPerMonth,
	}
}
