package ec2

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/pricing"
)

// Burstable (T-family) credit analysis — docs/design/compute-domains.md §4.6,
// and §7 trap 5.
//
// The mistake this file exists to prevent: a T instance whose credit balance
// has hit zero is being *throttled to its baseline*, not sitting idle. Its
// observed CPU is a ceiling AWS imposed, not demand the workload expressed.
// Sizing it down from that number is exactly backwards — it lowers the
// baseline and throttles harder. So a depleted instance is a refusal
// ([ReasonBurstCreditDepleted]), and the low CPU that would otherwise look
// like an easy win is reported as the symptom it is.
//
// The mirror-image mistake is the sticker-price mirage: an unlimited-mode T
// instance sustained above baseline pays surplus credits at $0.05/vCPU-hour
// on top of the sticker, so the "cheap" instance can cost more than the fixed
// one it was chosen over (t3.large loses to m5.large past ~43 % sustained
// CPU). That is reported with the realized surplus charge attached, because
// CPUSurplusCreditsCharged is ground truth and a model is not.

// SurplusCreditUSDPerVCPUHour is the Linux unlimited-mode surplus rate
// ([verified] §4.6). One CPU credit is one vCPU-minute, so a credit costs
// this divided by 60.
//
// It is a price, so the number lives in pkg/pricing with every other rate
// ([pricing.EC2SurplusCreditUSDPerVCPUHour], region-aware via
// [pricing.EC2CreditRatesFor]); this name is kept, and kept constant, so
// nothing that referenced it changes.
const SurplusCreditUSDPerVCPUHour = pricing.EC2SurplusCreditUSDPerVCPUHour

// SurplusUSDPerCredit is the per-credit price of surplus.
const SurplusUSDPerCredit = SurplusCreditUSDPerVCPUHour / CreditsPerVCPUHour

// CreditsPerVCPUHour is the credit accrual identity: an instance earns
// baseline × vCPUs credits per minute, i.e. this many per vCPU-hour at 100 %
// baseline. Every earn rate and accrual cap in [BurstSpec] is derived from it
// rather than transcribed, and TestBurstTableMatchesPublishedRates pins the
// derivation against AWS's published table.
//
// It is also what converts the surplus rate above into a per-credit charge, so
// it is defined once in pkg/pricing and read here: a divisor that drifted
// between the accrual model and the price would silently misreport both.
const CreditsPerVCPUHour = pricing.CreditsPerVCPUHour

// MaxAccrualHours is how many hours of accrual a T3-generation instance may
// bank (24 × the hourly earn rate).
const MaxAccrualHours = 24

// DepletedCreditFraction is the share of the accrual cap at or below which the
// balance counts as exhausted. It is a fraction rather than an absolute so it
// scales from t3.micro (288 credits) to t3.2xlarge (4,608).
const DepletedCreditFraction = 0.02

// DepletedWindowShare is how much of the observed window must sit at an
// exhausted balance before standard mode is called throttled. A momentary dip
// is not a throttled workload; a tenth of the window pinned at zero is.
const DepletedWindowShare = 0.10

// BurstSpec is one T-family shape's credit economics.
type BurstSpec struct {
	InstanceType string  `json:"instanceType"`
	Family       string  `json:"family"`
	Baseline     float64 `json:"baseline"` // fraction of one vCPU, 0..1
	VCPUs        int     `json:"vcpus"`
}

// CreditsPerHour is the earn rate: baseline × vCPUs × 60 credits.
func (s BurstSpec) CreditsPerHour() float64 {
	return s.Baseline * float64(s.VCPUs) * CreditsPerVCPUHour
}

// MaxCredits is the accrual cap: 24 hours of earning.
func (s BurstSpec) MaxCredits() float64 { return s.CreditsPerHour() * MaxAccrualHours }

// BaselinePercent renders the baseline the way AWS's table does.
func (s BurstSpec) BaselinePercent() float64 { return s.Baseline * 100 }

// burstFamilies are the families this package recognizes as credit-based. A
// family listed here but missing from burstBaselines is *known burstable with
// an unknown baseline*, which is a refusal, not a guess.
var burstFamilies = map[string]bool{
	"t2": true, "t3": true, "t3a": true, "t4g": true,
}

// burstBaselines are the per-size baselines, keyed by size.
//
// t3 is [verified] in §4.6: micro 10 %, small/medium 20 %, large 30 %,
// xlarge/2xlarge 40 %. t3a and t4g mirror the t3 table — [not re-verified],
// documented AWS behavior. t2 uses a *different* table and defaults to
// standard rather than unlimited mode; it is deliberately absent, so every t2
// instance lands in [BurstUnknown] and is refused rather than sized off a
// baseline this repo has not verified. Adding it is a one-row change plus a
// row in TestBurstTableMatchesPublishedRates — see FINDINGS.md.
//
// nano is absent for the same reason: §4.6 does not list it.
var burstBaselines = buildBurstBaselines()

// t3Baselines is the single transcription of AWS's published table.
var t3Baselines = map[string]BurstSpec{
	"micro":   {Baseline: 0.10, VCPUs: 2},
	"small":   {Baseline: 0.20, VCPUs: 2},
	"medium":  {Baseline: 0.20, VCPUs: 2},
	"large":   {Baseline: 0.30, VCPUs: 2},
	"xlarge":  {Baseline: 0.40, VCPUs: 4},
	"2xlarge": {Baseline: 0.40, VCPUs: 8},
}

// buildBurstBaselines fans the t3 table out to the families that share it.
// Aliasing rather than transcribing three times means they cannot drift apart,
// and building the table in a function rather than an init() keeps this
// package free of mutable package-level state.
func buildBurstBaselines() map[string]map[string]BurstSpec {
	out := map[string]map[string]BurstSpec{}
	for _, fam := range []string{"t3", "t3a", "t4g"} {
		m := make(map[string]BurstSpec, len(t3Baselines))
		for size, spec := range t3Baselines {
			m[size] = spec
		}
		out[fam] = m
	}
	return out
}

// FamilyOf returns the family part of an instance type ("t3" for "t3.large").
func FamilyOf(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 {
		return strings.ToLower(instanceType[:i])
	}
	return strings.ToLower(instanceType)
}

// SizeOf returns the size part of an instance type ("large" for "t3.large").
func SizeOf(instanceType string) string {
	if i := strings.IndexByte(instanceType, '.'); i > 0 && i+1 < len(instanceType) {
		return strings.ToLower(instanceType[i+1:])
	}
	return ""
}

// IsBurstable reports whether an instance type is credit-based. It is
// deliberately family-based rather than catalog-based: the collector must
// decide whether to request credit metrics before any catalog lookup, and a
// burstable instance missing from the catalog must still be recognized.
func IsBurstable(instanceType string) bool { return burstFamilies[FamilyOf(instanceType)] }

// BurstBaselineFor returns the credit economics of a burstable instance type,
// and false when the type is not burstable or its baseline is not encoded.
func BurstBaselineFor(instanceType string) (BurstSpec, bool) {
	fam, size := FamilyOf(instanceType), SizeOf(instanceType)
	spec, ok := burstBaselines[fam][size]
	if !ok {
		return BurstSpec{}, false
	}
	spec.InstanceType = strings.ToLower(instanceType)
	spec.Family = fam
	return spec, true
}

// BurstSpecs returns every encoded baseline, sorted by instance type. It backs
// the table test and the report's "what do we actually know" surface.
func BurstSpecs() []BurstSpec {
	var out []BurstSpec
	for fam, sizes := range burstBaselines {
		for size := range sizes {
			spec, _ := BurstBaselineFor(fam + "." + size)
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceType < out[j].InstanceType })
	return out
}

// BurstClass is the verdict on a burstable instance's credit behavior.
type BurstClass string

const (
	// BurstNotApplicable: the instance is not credit-based.
	BurstNotApplicable BurstClass = "not-burstable"
	// BurstUnknown: credit-based, but the evidence to reason about it is
	// missing — no encoded baseline, no credit metrics, or an unknown credit
	// mode. §7 trap 5 requires credit evidence in *both* directions, so this
	// is a refusal.
	BurstUnknown BurstClass = "unknown"
	// BurstHealthy: below baseline with credits intact. Sticker covers it.
	BurstHealthy BurstClass = "healthy"
	// BurstThrottled: standard mode with an exhausted balance. Observed CPU is
	// a ceiling, not demand. Never size down from it.
	BurstThrottled BurstClass = "throttled"
	// BurstSurplus: unlimited mode paying for burst. The sticker price is not
	// the price; the fixed-family comparison must use the effective cost.
	BurstSurplus BurstClass = "surplus"
)

// Throttled reports whether the observed CPU is an AWS-imposed ceiling.
func (c BurstClass) Throttled() bool { return c == BurstThrottled }

// Actionable reports whether a size decision may be made from the observed
// CPU of an instance in this class.
func (c BurstClass) Actionable() bool {
	return c == BurstNotApplicable || c == BurstHealthy || c == BurstSurplus
}

// Credit modes, as DescribeInstanceCreditSpecifications reports them.
const (
	CreditModeStandard  = "standard"
	CreditModeUnlimited = "unlimited"
)

// BurstState is the full credit picture for one instance over one window.
type BurstState struct {
	Class        BurstClass `json:"class"`
	InstanceType string     `json:"instanceType"`
	Mode         string     `json:"mode,omitempty"`
	Spec         BurstSpec  `json:"spec,omitempty"`

	// AvgCPUFraction is the mean observed CPU over the window as a fraction of
	// total vCPU capacity — directly comparable to Spec.Baseline.
	AvgCPUFraction float64 `json:"avgCPUFraction"`
	// BalanceFirst/Last/Min describe CPUCreditBalance over the window.
	BalanceFirst float64 `json:"balanceFirst"`
	BalanceLast  float64 `json:"balanceLast"`
	BalanceMin   float64 `json:"balanceMin"`
	// AccrualPerHour is the observed balance slope; negative means the
	// instance is spending credits faster than it earns them and will deplete.
	AccrualPerHour float64 `json:"accrualPerHour"`
	// HoursToDepletion projects the observed slope to a zero balance. It is
	// zero when the balance is flat, rising, or already exhausted.
	HoursToDepletion float64 `json:"hoursToDepletion,omitempty"`
	// DepletedShare is the fraction of balance datapoints at or below the
	// exhaustion threshold.
	DepletedShare float64 `json:"depletedShare"`

	// SurplusCreditsCharged is realized, billed surplus over the window —
	// ground truth, not a model.
	SurplusCreditsCharged float64 `json:"surplusCreditsCharged"`
	SurplusBalanceMax     float64 `json:"surplusBalanceMax"`
	// SurplusHourlyUSD is the realized surplus charge amortized over the
	// window; SurplusMonthlyUSD projects it.
	SurplusHourlyUSD  float64 `json:"surplusHourlyUSD"`
	SurplusMonthlyUSD float64 `json:"surplusMonthlyUSD"`
	// StickerHourlyUSD and EffectiveHourlyUSD differ by exactly the realized
	// surplus. The second is what the instance costs.
	StickerHourlyUSD   float64 `json:"stickerHourlyUSD"`
	EffectiveHourlyUSD float64 `json:"effectiveHourlyUSD"`

	// ModeledSurplusHourlyUSD applies §4.6's cost(u) model to the observed
	// average. It is carried beside the realized charge so the two can be
	// compared, and it is never claimed as money.
	ModeledSurplusHourlyUSD float64 `json:"modeledSurplusHourlyUSD"`

	Reason string `json:"reason"`
}

// AnalyzeBurst classifies one instance's credit behavior.
//
// vcpus and stickerHourlyUSD come from the pricing catalog; window is the
// observed span. Every branch that cannot be substantiated returns
// [BurstUnknown] with a reason, because a guess here inverts a decision.
func AnalyzeBurst(in Instance, t Target, vcpus int, stickerHourlyUSD float64, window time.Duration) BurstState {
	st := BurstState{
		InstanceType:     in.InstanceType,
		Mode:             in.CreditMode,
		StickerHourlyUSD: stickerHourlyUSD,
	}
	if !IsBurstable(in.InstanceType) {
		st.Class = BurstNotApplicable
		st.EffectiveHourlyUSD = stickerHourlyUSD
		st.Reason = "not a credit-based (T-family) instance type"
		return st
	}

	spec, ok := BurstBaselineFor(in.InstanceType)
	if !ok {
		st.Class = BurstUnknown
		st.EffectiveHourlyUSD = stickerHourlyUSD
		st.Reason = fmt.Sprintf("%s is credit-based but this build encodes no verified CPU baseline for it, "+
			"so its observed CPU cannot be told apart from throttling", in.InstanceType)
		return st
	}
	if vcpus > 0 {
		spec.VCPUs = vcpus
	}
	st.Spec = spec

	cpu, hasCPU := t.SeriesFor(MetricCPUUtilization)
	if hasCPU {
		if mean, ok := cpu.Mean(); ok {
			st.AvgCPUFraction = mean / 100
		}
	}

	bal, hasBal := t.SeriesFor(MetricCPUCreditBalance)
	if !hasBal || len(bal.Points) == 0 {
		st.Class = BurstUnknown
		st.EffectiveHourlyUSD = stickerHourlyUSD
		st.Reason = fmt.Sprintf("no %s datapoints: without credit evidence a low CPU average on %s is "+
			"indistinguishable from baseline throttling", MetricCPUCreditBalance, in.InstanceType)
		return st
	}
	st.BalanceFirst = bal.Points[0].Value
	last, _ := bal.Last()
	st.BalanceLast = last.Value
	st.BalanceMin, _ = bal.Min()

	threshold := spec.MaxCredits() * DepletedCreditFraction
	var depleted int
	for _, p := range bal.Points {
		if p.Value <= threshold {
			depleted++
		}
	}
	st.DepletedShare = float64(depleted) / float64(len(bal.Points))

	if span := bal.Points[len(bal.Points)-1].At.Sub(bal.Points[0].At); span > 0 {
		st.AccrualPerHour = (st.BalanceLast - st.BalanceFirst) / span.Hours()
		if st.AccrualPerHour < 0 && st.BalanceLast > threshold {
			st.HoursToDepletion = st.BalanceLast / -st.AccrualPerHour
		}
	}

	if s, ok := t.SeriesFor(MetricCPUSurplusCreditsCharged); ok {
		st.SurplusCreditsCharged = s.Sum()
	}
	if s, ok := t.SeriesFor(MetricCPUSurplusCreditBalance); ok {
		st.SurplusBalanceMax, _ = s.Max()
	}
	if h := window.Hours(); h > 0 {
		st.SurplusHourlyUSD = st.SurplusCreditsCharged * SurplusUSDPerCredit / h
	}
	st.SurplusMonthlyUSD = st.SurplusHourlyUSD * HoursPerMonth
	st.EffectiveHourlyUSD = stickerHourlyUSD + st.SurplusHourlyUSD

	// §4.6: cost(u) = sticker + max(0, u − baseline) × vCPUs × $0.05.
	if over := st.AvgCPUFraction - spec.Baseline; over > 0 {
		st.ModeledSurplusHourlyUSD = over * float64(spec.VCPUs) * SurplusCreditUSDPerVCPUHour
	}

	mode := strings.ToLower(strings.TrimSpace(in.CreditMode))
	switch mode {
	case CreditModeStandard:
		if st.DepletedShare >= DepletedWindowShare {
			st.Class = BurstThrottled
			st.Reason = fmt.Sprintf(
				"standard mode with an exhausted credit balance for %.0f%% of the window (min %.1f credits, "+
					"cap %.0f): CPU is pinned at the %.0f%% baseline by AWS, so the observed %.1f%% average is a "+
					"ceiling, not demand",
				st.DepletedShare*100, st.BalanceMin, spec.MaxCredits(), spec.BaselinePercent(), st.AvgCPUFraction*100)
			return st
		}
	case CreditModeUnlimited:
		if st.SurplusCreditsCharged > 0 || st.SurplusBalanceMax > 0 {
			st.Class = BurstSurplus
			st.Reason = fmt.Sprintf(
				"unlimited mode is paying for burst: %.1f surplus credits charged over the window = %s/h on top "+
					"of the %s/h sticker, so this instance actually costs %s/h",
				st.SurplusCreditsCharged, fmtUSD(st.SurplusHourlyUSD), fmtUSD(stickerHourlyUSD),
				fmtUSD(st.EffectiveHourlyUSD))
			return st
		}
	default:
		st.Class = BurstUnknown
		st.Reason = fmt.Sprintf("credit mode is unknown for %s: standard mode throttles at the baseline and "+
			"unlimited mode bills surplus, and the two invert the sizing decision", in.InstanceType)
		return st
	}

	st.Class = BurstHealthy
	st.Reason = fmt.Sprintf(
		"%s mode, %.1f%% average CPU against a %.0f%% baseline, credit balance %.0f of %.0f (%+.1f/h): "+
			"the sticker price covers this workload",
		mode, st.AvgCPUFraction*100, spec.BaselinePercent(), st.BalanceLast, spec.MaxCredits(), st.AccrualPerHour)
	return st
}

// HoursPerMonth is the billing-average month, matching pricing.HoursPerMonth
// and commit.HoursPerMonth. This package already depends on pkg/pricing for
// the instance catalog, so there is no import edge to save by duplicating it:
// it is defined from pricing's and structurally cannot drift.
const HoursPerMonth = pricing.HoursPerMonth
