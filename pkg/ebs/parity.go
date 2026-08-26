package ebs

// gp2 → gp3 at performance parity (docs/design/compute-domains.md §4.7).
//
// This file is the whole of the arithmetic, and it is pure: no clock, no I/O,
// no package-level state. Everything that decides whether a volume may be
// converted — and to what — is decided here, so the seams and the actuator
// have nothing to reinterpret.
//
// # Why the naive rule is wrong
//
// gp2 performance is a function of SIZE: 3 IOPS/GiB (floor 100, ceiling
// 16,000), plus a burst bucket to 3,000 IOPS for volumes at or below 1,000
// GiB, plus a throughput ceiling that steps from 128 to 250 MiB/s at 334 GiB.
// gp3's *free* baseline is a flat 3,000 IOPS / 125 MiB/s. So "gp3 is 20 %
// cheaper" holds only where gp2 delivers no more than that baseline:
//
//	G ≤ 1,000 GiB   gp2 baseline ≤ 3,000 IOPS — gp3's SUSTAINED 3,000 beats
//	                gp2's BURST 3,000, but gp2 still delivers up to 250 MiB/s
//	                above 334 GiB, which gp3 charges $7.50/mo to match.
//	G > 1,000 GiB   gp2 baseline is 3G IOPS > 3,000 and does not burst. The
//	                naive conversion silently divides IOPS by up to 5.3× — a
//	                4 TiB volume drops from 12,000 sustained IOPS to 3,000.
//
// [GP2Performance] models the first fact and [Rates.PlanGP3] refuses any
// configuration that does not clear measured demand, so the naive
// configuration is not a candidate rather than a cheaper one.
//
// # Units
//
// Throughput is MiB/s everywhere (1 MiB = 1<<20 bytes), which is what
// ModifyVolume provisions and what the EBS byte metrics divide down to. AWS
// prices it per "provisioned MBps"; the number provisioned is the number
// billed, so the rate applies to the same integer either way.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
)

// Volume types, as DescribeVolumes reports them.
const (
	VolumeTypeGP2      = "gp2"
	VolumeTypeGP3      = "gp3"
	VolumeTypeIO1      = "io1"
	VolumeTypeIO2      = "io2"
	VolumeTypeST1      = "st1"
	VolumeTypeSC1      = "sc1"
	VolumeTypeStandard = "standard"
)

// The gp2 performance model — [not re-verified: EBS gp2 documentation], and
// flagged as such in §4.9's unverified list. It is stated as named constants
// rather than inline literals so that a future verification pass changes one
// line and re-runs the grid test, instead of hunting magic numbers.
const (
	// GP2IOPSPerGiB is gp2's baseline provisioning ratio.
	GP2IOPSPerGiB = 3
	// GP2MinIOPS is the floor a small gp2 volume still gets.
	GP2MinIOPS = 100
	// GP2MaxIOPS is gp2's ceiling, reached at 5,334 GiB.
	GP2MaxIOPS = 16000
	// GP2BurstIOPS is the burst-bucket ceiling, available only to volumes at
	// or below GP2BurstMaxSizeGiB. It is a *credit-funded* rate: a volume that
	// exhausts its bucket falls back to baseline, which is why parity is
	// measured against baseline and burst is reported as evidence, never
	// promised.
	GP2BurstIOPS = 3000
	// GP2BurstMaxSizeGiB is the largest volume that still bursts. Above it,
	// baseline already exceeds GP2BurstIOPS.
	GP2BurstMaxSizeGiB = 1000
	// GP2LargeVolumeGiB is where gp2's throughput ceiling steps up.
	GP2LargeVolumeGiB = 334
	// GP2MaxThroughputMBps applies at or above GP2LargeVolumeGiB.
	GP2MaxThroughputMBps = 250
	// GP2SmallMaxThroughputMBps applies below it.
	GP2SmallMaxThroughputMBps = 128
	// BytesPerIO is the I/O size EBS uses to convert IOPS to throughput
	// (256 KiB for gp2's throughput ceiling arithmetic).
	BytesPerIO = 256 << 10
)

// MaxVolumeSizeGiB is the largest gp2 or gp3 volume AWS creates, 16 TiB. A
// record above it is an unreadable inventory record, not a very large volume,
// and it is refused rather than priced.
const MaxVolumeSizeGiB = 16384

// The gp3 configuration space — [verified: https://aws.amazon.com/ebs/pricing/]
// for the free baseline, [not re-verified] for the ratio ceilings.
const (
	GP3BaseIOPS             = 3000  // free, on every size
	GP3MaxIOPS              = 16000 // per volume
	GP3MaxIOPSPerGiB        = 500   // ratio ceiling above the free baseline
	GP3BaseThroughputMBps   = 125   // free, on every size
	GP3MaxThroughputMBps    = 1000  // per volume
	GP3ThroughputPerIOPSMBs = 0.25  // MiB/s of throughput per provisioned IOPS
)

// MiB is one mebibyte, the unit throughput is expressed in.
const MiB = 1 << 20

// eps is the money/quantity comparison tolerance, matching commit.Eps. Money is
// never compared with ==.
const eps = 1e-9

// HoursPerMonth converts monthly to hourly cost. It is the billing-average
// month and MUST equal pricing.HoursPerMonth and commit.HoursPerMonth —
// asserted by TestHoursPerMonthMatchesPricing.
const HoursPerMonth = 730

// Rates are the EBS price points, us-east-1, USD — [verified:
// https://aws.amazon.com/ebs/pricing/, fetched 2026-08-25]. They ship embedded
// and are overridable by a synced or hand-written file ([LoadRates]): relative
// math must work offline, exact billing belongs to the invoice.
type Rates struct {
	GP2GBMonthUSD float64 `json:"gp2GBMonthUSD"`
	GP3GBMonthUSD float64 `json:"gp3GBMonthUSD"`
	// GP3IOPSMonthUSD is charged per provisioned IOPS ABOVE GP3FreeIOPS.
	GP3IOPSMonthUSD float64 `json:"gp3IOPSMonthUSD"`
	// GP3ThroughputMonthUSD is charged per provisioned MBps ABOVE
	// GP3FreeThroughputMBps.
	GP3ThroughputMonthUSD float64 `json:"gp3ThroughputMonthUSD"`
	GP3FreeIOPS           int32   `json:"gp3FreeIOPS"`
	GP3FreeThroughputMBps int32   `json:"gp3FreeThroughputMBps"`
	// IO1GBMonthUSD and IO2GBMonthUSD are carried for the io1/io2 → gp3
	// advisory this unit deliberately defers (see FINDINGS.md); nothing in
	// this package prices a change with them.
	IO1GBMonthUSD float64 `json:"io1GBMonthUSD"`
	IO2GBMonthUSD float64 `json:"io2GBMonthUSD"`
}

// DefaultRates returns the embedded us-east-1 rates.
func DefaultRates() Rates {
	return Rates{
		GP2GBMonthUSD:         0.10,
		GP3GBMonthUSD:         0.08,
		GP3IOPSMonthUSD:       0.005,
		GP3ThroughputMonthUSD: 0.06,
		GP3FreeIOPS:           GP3BaseIOPS,
		GP3FreeThroughputMBps: GP3BaseThroughputMBps,
		IO1GBMonthUSD:         0.125,
		IO2GBMonthUSD:         0.125,
	}
}

// Validate reports the first structural problem with a rate table. A rate that
// is zero, negative or not finite would turn a savings claim into fiction, so
// an override file is rejected rather than partially trusted.
func (r Rates) Validate() error {
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"gp2GBMonthUSD", r.GP2GBMonthUSD},
		{"gp3GBMonthUSD", r.GP3GBMonthUSD},
		{"gp3IOPSMonthUSD", r.GP3IOPSMonthUSD},
		{"gp3ThroughputMonthUSD", r.GP3ThroughputMonthUSD},
	} {
		if !(f.v > 0) || math.IsInf(f.v, 0) {
			return fmt.Errorf("ebs: rate %s must be positive and finite, got %v", f.name, f.v)
		}
	}
	if r.GP3FreeIOPS < 0 || r.GP3FreeIOPS > GP3MaxIOPS {
		return fmt.Errorf("ebs: gp3FreeIOPS out of range: %d", r.GP3FreeIOPS)
	}
	if r.GP3FreeThroughputMBps < 0 || r.GP3FreeThroughputMBps > GP3MaxThroughputMBps {
		return fmt.Errorf("ebs: gp3FreeThroughputMBps out of range: %d", r.GP3FreeThroughputMBps)
	}
	return nil
}

// LoadRates parses an override rate table, rejecting unknown fields so a typo
// fails loudly instead of silently leaving an embedded default in place.
func LoadRates(r io.Reader) (Rates, error) {
	out := DefaultRates()
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Rates{}, fmt.Errorf("ebs: parse rates: %w", err)
	}
	if err := out.Validate(); err != nil {
		return Rates{}, err
	}
	return out, nil
}

// GP2Performance is what a gp2 volume of a given size actually delivers.
//
// Baseline is sustained forever; burst is credit-funded and therefore a
// property of the *bucket*, not of the volume. Parity is defined against
// baseline (§4.7 "floor at gp2's delivered baseline"); burst is carried so a
// report can say what the volume was doing when its credits ran out.
type GP2Performance struct {
	SizeGiB int64 `json:"sizeGiB"`
	// BaselineIOPS is 3 IOPS/GiB clamped to [100, 16000].
	BaselineIOPS int32 `json:"baselineIOPS"`
	// BurstIOPS is the credit-funded ceiling; equal to BaselineIOPS when the
	// volume is too large to burst.
	BurstIOPS int32 `json:"burstIOPS"`
	// Burstable reports whether a burst bucket exists at all.
	Burstable bool `json:"burstable"`
	// BaselineThroughputMBps is the sustained throughput ceiling:
	// min(size ceiling, BaselineIOPS × 256 KiB).
	BaselineThroughputMBps float64 `json:"baselineThroughputMBps"`
	// BurstThroughputMBps is the same computed from BurstIOPS.
	BurstThroughputMBps float64 `json:"burstThroughputMBps"`
}

// GP2PerformanceFor models a gp2 volume of sizeGiB. A non-positive size yields
// the zero value: an unreadable size must not silently become "100 IOPS".
func GP2PerformanceFor(sizeGiB int64) GP2Performance {
	if sizeGiB <= 0 {
		return GP2Performance{}
	}
	// Multiplied only where it cannot overflow: above 5,333 GiB the baseline
	// is capped at GP2MaxIOPS regardless.
	base := int64(GP2MaxIOPS)
	if sizeGiB <= GP2MaxIOPS/GP2IOPSPerGiB {
		base = sizeGiB * GP2IOPSPerGiB
	}
	if base < GP2MinIOPS {
		base = GP2MinIOPS
	}
	burst, burstable := int32(base), false
	if sizeGiB <= GP2BurstMaxSizeGiB && base < GP2BurstIOPS {
		burst, burstable = GP2BurstIOPS, true
	}
	ceiling := float64(GP2SmallMaxThroughputMBps)
	if sizeGiB >= GP2LargeVolumeGiB {
		ceiling = GP2MaxThroughputMBps
	}
	tput := func(iops int32) float64 {
		return math.Min(ceiling, float64(iops)*BytesPerIO/MiB)
	}
	return GP2Performance{
		SizeGiB:                sizeGiB,
		BaselineIOPS:           int32(base),
		BurstIOPS:              burst,
		Burstable:              burstable,
		BaselineThroughputMBps: tput(int32(base)),
		BurstThroughputMBps:    tput(burst),
	}
}

// GP3Config is one gp3 configuration: what ModifyVolume is asked for and what
// the invoice charges for.
type GP3Config struct {
	SizeGiB        int64 `json:"sizeGiB"`
	IOPS           int32 `json:"iops"`
	ThroughputMBps int32 `json:"throughputMBps"`
}

// GP3MaxIOPSFor is the largest IOPS a gp3 volume of this size accepts: the free
// baseline is available at every size, above it the 500:1 ratio applies, and
// 16,000 caps everything.
func GP3MaxIOPSFor(sizeGiB int64) int32 {
	if sizeGiB <= 0 {
		return 0
	}
	byRatio := int64(GP3MaxIOPS)
	if sizeGiB <= GP3MaxIOPS/GP3MaxIOPSPerGiB {
		byRatio = sizeGiB * GP3MaxIOPSPerGiB
	}
	if byRatio > GP3MaxIOPS {
		byRatio = GP3MaxIOPS
	}
	if byRatio < GP3BaseIOPS {
		byRatio = GP3BaseIOPS
	}
	return int32(byRatio)
}

// GP3MaxThroughputFor is the largest throughput a gp3 volume with this many
// provisioned IOPS accepts: 0.25 MiB/s per IOPS, capped at 1,000 MiB/s.
func GP3MaxThroughputFor(iops int32) int32 {
	if iops <= 0 {
		return 0
	}
	byRatio := float64(iops) * GP3ThroughputPerIOPSMBs
	if byRatio > GP3MaxThroughputMBps {
		byRatio = GP3MaxThroughputMBps
	}
	return int32(byRatio)
}

// Validate reports whether a configuration is one AWS would accept.
func (c GP3Config) Validate() error {
	switch {
	case c.SizeGiB <= 0:
		return fmt.Errorf("ebs: gp3 size must be positive, got %d GiB", c.SizeGiB)
	case c.IOPS < GP3BaseIOPS:
		return fmt.Errorf("ebs: gp3 IOPS must be at least %d, got %d", GP3BaseIOPS, c.IOPS)
	case c.IOPS > GP3MaxIOPSFor(c.SizeGiB):
		return fmt.Errorf("ebs: gp3 %d IOPS exceeds the %d limit for a %d GiB volume",
			c.IOPS, GP3MaxIOPSFor(c.SizeGiB), c.SizeGiB)
	case c.ThroughputMBps < GP3BaseThroughputMBps:
		return fmt.Errorf("ebs: gp3 throughput must be at least %d MiB/s, got %d",
			GP3BaseThroughputMBps, c.ThroughputMBps)
	case c.ThroughputMBps > GP3MaxThroughputFor(c.IOPS):
		return fmt.Errorf("ebs: gp3 %d MiB/s exceeds the %d MiB/s limit at %d IOPS",
			c.ThroughputMBps, GP3MaxThroughputFor(c.IOPS), c.IOPS)
	}
	return nil
}

// Demand is measured, headroom-applied demand that a configuration must clear.
// Both fields are what the volume was observed to need, not what it was
// provisioned for.
type Demand struct {
	IOPS           float64 `json:"iops"`
	ThroughputMBps float64 `json:"throughputMBps"`
}

// valid reports whether demand is usable arithmetic. NaN, ±Inf and negatives
// are refused rather than clamped: they mean the caller's measurement is
// broken, and a broken measurement must not become a provisioning decision.
func (d Demand) valid() bool {
	for _, v := range []float64{d.IOPS, d.ThroughputMBps} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return true
}

// Max returns the element-wise maximum, used to fold a floor into demand.
func (d Demand) Max(o Demand) Demand {
	return Demand{IOPS: math.Max(d.IOPS, o.IOPS), ThroughputMBps: math.Max(d.ThroughputMBps, o.ThroughputMBps)}
}

// Clears reports whether the configuration meets demand in BOTH dimensions.
// This is the predicate the whole unit turns on: a configuration that does not
// clear measured demand is not a candidate, cheaper or not.
func (c GP3Config) Clears(d Demand) bool {
	if !d.valid() {
		return false
	}
	return float64(c.IOPS) >= d.IOPS-eps && float64(c.ThroughputMBps) >= d.ThroughputMBps-eps
}

// Floor selects how much of gp2's nameplate performance a proposal must
// preserve regardless of what was measured.
type Floor int

const (
	// FloorMeasured provisions to measured demand alone. It is only legitimate
	// once the observation window is long enough to have seen the business
	// peak (§4.7: "requires a ≥ 7-day window covering business peaks before
	// parity-reduction recs").
	FloorMeasured Floor = iota
	// FloorGP2Baseline never provisions below what the gp2 volume delivers
	// sustained. Short windows and thin sample coverage use it, so a
	// conversion made on weak evidence cannot degrade anything.
	FloorGP2Baseline
)

func (f Floor) String() string {
	if f == FloorGP2Baseline {
		return "gp2-baseline"
	}
	return "measured"
}

// Refusal codes produced by the parity math itself. Domain-level refusals
// (unmeasured, wrong type, guardrails, cooldown) live in domain.go.
const (
	// ReasonInvalidSize: the volume's size is not usable arithmetic.
	ReasonInvalidSize = "invalid-size"
	// ReasonInvalidDemand: the measurement is NaN/Inf/negative.
	ReasonInvalidDemand = "invalid-demand"
	// ReasonExceedsGP3: no gp3 configuration clears the required demand — the
	// volume needs io2 or a bigger volume, not a cheaper one.
	ReasonExceedsGP3 = "exceeds-gp3"
	// ReasonNoCheaperConfig: gp3 at parity costs at least as much as gp2. This
	// is a real regime, not a rounding artifact — see [Rates.PlanGP3].
	ReasonNoCheaperConfig = "no-cheaper-config"
)

// Refusal is a stated reason no proposal was produced.
type Refusal struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func (r *Refusal) String() string {
	if r == nil {
		return ""
	}
	return r.Code + ": " + r.Reason
}

// ParityPlan is the outcome of the parity math for one volume: what gp3
// configuration clears demand, what each side costs, and what the naive
// conversion would have done.
type ParityPlan struct {
	SizeGiB int64          `json:"sizeGiB"`
	GP2     GP2Performance `json:"gp2"`
	Config  GP3Config      `json:"config"`
	Floor   Floor          `json:"floor"`

	// Required is the demand the configuration had to clear: measured demand
	// folded with the floor.
	Required Demand `json:"required"`
	// Measured is the demand as handed in, before the floor.
	Measured Demand `json:"measured"`

	CurrentMonthlyUSD  float64 `json:"currentMonthlyUSD"`
	ProposedMonthlyUSD float64 `json:"proposedMonthlyUSD"`
	// DeltaMonthlyUSD is current − proposed: positive is a list-price saving.
	// It is GROSS. Only the commitment-netted number may be claimed.
	DeltaMonthlyUSD float64 `json:"deltaMonthlyUSD"`

	// Naive is the configuration a "gp3 is always cheaper" rule would pick:
	// the free baseline, 3,000 IOPS / 125 MiB/s.
	Naive GP3Config `json:"naive"`
	// NaiveMonthlyUSD is what that would have cost.
	NaiveMonthlyUSD float64 `json:"naiveMonthlyUSD"`
	// NaiveDegrades is true when the naive configuration does not clear
	// Required — i.e. when the naive rule would have silently downgraded this
	// volume. The report says so; the planner never proposes it.
	NaiveDegrades bool `json:"naiveDegrades"`
	// NaiveIOPSShortfall and NaiveThroughputShortfall quantify by how much,
	// for the report and for the operator's sanity.
	NaiveIOPSShortfall       float64 `json:"naiveIOPSShortfall,omitempty"`
	NaiveThroughputShortfall float64 `json:"naiveThroughputShortfall,omitempty"`
}

// GP2MonthlyUSD prices a gp2 volume. gp2 charges for size alone.
func (r Rates) GP2MonthlyUSD(sizeGiB int64) float64 {
	if sizeGiB <= 0 {
		return 0
	}
	return float64(sizeGiB) * r.GP2GBMonthUSD
}

// GP3MonthlyUSD prices a gp3 configuration: size, plus IOPS and throughput
// above the free baselines.
func (r Rates) GP3MonthlyUSD(c GP3Config) float64 {
	if c.SizeGiB <= 0 {
		return 0
	}
	cost := float64(c.SizeGiB) * r.GP3GBMonthUSD
	if extra := c.IOPS - r.GP3FreeIOPS; extra > 0 {
		cost += float64(extra) * r.GP3IOPSMonthUSD
	}
	if extra := c.ThroughputMBps - r.GP3FreeThroughputMBps; extra > 0 {
		cost += float64(extra) * r.GP3ThroughputMonthUSD
	}
	return cost
}

// NameplateParityConfig is the gp3 configuration that matches a gp2 volume's
// full delivered baseline — the configuration §4.7's Δ formula prices, and the
// one used when the evidence is too thin to provision to measurement.
func NameplateParityConfig(sizeGiB int64) GP3Config {
	p := GP2PerformanceFor(sizeGiB)
	return configFor(sizeGiB, Demand{IOPS: float64(p.BaselineIOPS), ThroughputMBps: p.BaselineThroughputMBps})
}

// NameplateParityDeltaMonthlyUSD is §4.7's Δ, evaluated for a size:
//
//	Δ = 0.02·G − max(0, 3G − 3000)·0.005 − max(0, T_gp2(G) − 125)·0.06
//
// It is the monthly saving of converting at FULL nameplate parity, and it is
// exported because it is the number the design document states and the one a
// reviewer will check by hand. Note that it is NOT always the number this
// package reports: with a long enough window Kilter provisions to measured
// demand, which saves more, and in the 334–375 GiB band Δ is negative.
func (r Rates) NameplateParityDeltaMonthlyUSD(sizeGiB int64) float64 {
	if sizeGiB <= 0 {
		return 0
	}
	return r.GP2MonthlyUSD(sizeGiB) - r.GP3MonthlyUSD(NameplateParityConfig(sizeGiB))
}

// configFor is the smallest valid gp3 configuration that clears d. It is the
// only place a configuration is synthesized, so "never below demand" is one
// rule in one function:
//
//   - IOPS: demand rounded UP, at least the free baseline, and at least 4× the
//     throughput demand (gp3 delivers 0.25 MiB/s per provisioned IOPS, so
//     throughput silently caps without the IOPS to back it).
//   - Throughput: demand rounded UP, at least the free baseline.
//
// The result may be invalid (above gp3's ceilings); the caller checks.
func configFor(sizeGiB int64, d Demand) GP3Config {
	tput := ceilInt32(d.ThroughputMBps)
	if tput < GP3BaseThroughputMBps {
		tput = GP3BaseThroughputMBps
	}
	iops := ceilInt32(d.IOPS)
	if iops < GP3BaseIOPS {
		iops = GP3BaseIOPS
	}
	if need := ceilInt32(float64(tput) / GP3ThroughputPerIOPSMBs); need > iops {
		iops = need
	}
	return GP3Config{SizeGiB: sizeGiB, IOPS: iops, ThroughputMBps: tput}
}

// ceilInt32 rounds up and saturates rather than wrapping. Demand always rounds
// UP: rounding a fractional IOPS demand down is exactly the silent degradation
// this unit exists to prevent.
func ceilInt32(v float64) int32 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	c := math.Ceil(v - eps)
	if c > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(c)
}

// PlanGP3 is the parity decision for one gp2 volume.
//
// It returns a plan only when a gp3 configuration exists that (a) clears the
// required demand and (b) costs strictly less than the gp2 volume. Otherwise
// it returns the reason it refused. There is no third outcome and no "close
// enough": every proposal this package can make comes from here.
//
// The refusal regimes are real, not defensive padding:
//
//   - 334–375 GiB volumes that must preserve gp2's 250 MiB/s ceiling cost MORE
//     on gp3 ($7.50/mo of throughput against $6.68–7.50/mo of storage saving).
//   - Volumes below ~9 GiB that must preserve 128 MiB/s likewise.
//   - Anything whose measured demand exceeds gp3's 16,000 IOPS / 1,000 MiB/s
//     envelope, or the 500:1 IOPS-per-GiB ratio, has no gp3 form at all.
func (r Rates) PlanGP3(sizeGiB int64, measured Demand, floor Floor) (ParityPlan, *Refusal) {
	if sizeGiB <= 0 || sizeGiB > MaxVolumeSizeGiB {
		return ParityPlan{}, &Refusal{ReasonInvalidSize, fmt.Sprintf(
			"volume size %d GiB is outside the 1–%d GiB gp2/gp3 range: the inventory record is unusable",
			sizeGiB, MaxVolumeSizeGiB)}
	}
	if !measured.valid() {
		return ParityPlan{}, &Refusal{ReasonInvalidDemand,
			fmt.Sprintf("measured demand is not usable arithmetic (%v IOPS, %v MiB/s)",
				measured.IOPS, measured.ThroughputMBps)}
	}

	gp2 := GP2PerformanceFor(sizeGiB)
	required := measured
	if floor == FloorGP2Baseline {
		required = required.Max(Demand{
			IOPS:           float64(gp2.BaselineIOPS),
			ThroughputMBps: gp2.BaselineThroughputMBps,
		})
	}

	cfg := configFor(sizeGiB, required)
	naive := GP3Config{SizeGiB: sizeGiB, IOPS: GP3BaseIOPS, ThroughputMBps: GP3BaseThroughputMBps}

	plan := ParityPlan{
		SizeGiB:            sizeGiB,
		GP2:                gp2,
		Config:             cfg,
		Floor:              floor,
		Required:           required,
		Measured:           measured,
		CurrentMonthlyUSD:  r.GP2MonthlyUSD(sizeGiB),
		ProposedMonthlyUSD: r.GP3MonthlyUSD(cfg),
		Naive:              naive,
		NaiveMonthlyUSD:    r.GP3MonthlyUSD(naive),
		NaiveDegrades:      !naive.Clears(required),
	}
	plan.DeltaMonthlyUSD = plan.CurrentMonthlyUSD - plan.ProposedMonthlyUSD
	if plan.NaiveDegrades {
		plan.NaiveIOPSShortfall = math.Max(0, required.IOPS-float64(naive.IOPS))
		plan.NaiveThroughputShortfall = math.Max(0, required.ThroughputMBps-float64(naive.ThroughputMBps))
	}

	if err := cfg.Validate(); err != nil {
		return plan, &Refusal{ReasonExceedsGP3, fmt.Sprintf(
			"no gp3 configuration clears the measured demand of %.0f IOPS / %.0f MiB/s on a %d GiB volume: %v",
			required.IOPS, required.ThroughputMBps, sizeGiB, err)}
	}
	// Belt and braces: the one invariant the fuzz target asserts, checked in
	// production too. configFor cannot violate it, and if it ever does the
	// answer is a refusal, not a degraded volume.
	if !cfg.Clears(required) {
		return plan, &Refusal{ReasonExceedsGP3, fmt.Sprintf(
			"internal: gp3 %d IOPS / %d MiB/s does not clear %.0f IOPS / %.0f MiB/s",
			cfg.IOPS, cfg.ThroughputMBps, required.IOPS, required.ThroughputMBps)}
	}
	if plan.DeltaMonthlyUSD <= eps {
		return plan, &Refusal{ReasonNoCheaperConfig, fmt.Sprintf(
			"gp3 at parity (%d IOPS / %d MiB/s) costs $%.2f/mo against gp2's $%.2f/mo: converting this %d GiB volume would cost $%.2f/mo more",
			cfg.IOPS, cfg.ThroughputMBps, plan.ProposedMonthlyUSD, plan.CurrentMonthlyUSD,
			sizeGiB, -plan.DeltaMonthlyUSD)}
	}
	return plan, nil
}
