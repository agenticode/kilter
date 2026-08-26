package rds

// The parity decision path: measurement in, one proposal or a stated refusal
// out.
//
// [Parity] is the U11 [StorageParity] seam filled. The interface is used as
// U11 typed it — same method, same arguments, same return shape — so this unit
// is a file addition rather than surgery on a reviewed decision path, and
// `Config.Parity` stays nil in [DefaultConfig] exactly as
// TestStoragePerformanceIsRefusedNotBorrowed requires.
//
// Two rules survive from U11 unchanged and are re-asserted here rather than
// re-derived:
//
//   - An unverified rate may size a reported fact and may NEVER become a
//     claimed saving. [RateProvenance.Claimable] is the single gate, and
//     [ReasonUnverifiedRate] is the refusal — U11's code, reused, so a report
//     does not grow a second name for one fact.
//   - Silence is not evidence. A metric CloudWatch declined to answer for
//     produces a refusal, never a low number.

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Parity defaults. Each is a policy choice rather than an AWS fact.
const (
	// DefaultParityHeadroom is the margin applied over measured demand before
	// a configuration is sized. A storage volume provisioned exactly at its
	// observed p99 has no room for the peak the window did not contain.
	DefaultParityHeadroom = 0.25
	// DefaultParityMinConfidence is the bar a proposal must clear. Below it
	// the finding is a refusal naming the weakest factor, which is what tells
	// an operator what would fix it.
	DefaultParityMinConfidence = 0.6
	// DefaultParityPercentile is the statistic measured demand is read at.
	// The maximum would let one CloudWatch spike pin a volume at its peak
	// forever; the mean would under-provision. p99 over 1-minute datapoints is
	// what the four shipped domains use.
	DefaultParityPercentile = 0.99
)

// ParityConfig bounds the parity engine.
//
// Now is an ARGUMENT rather than a clock read, matching the whole package
// (TestNoClockReads): the cooldown arithmetic needs a present moment and this
// package never invents one.
type ParityConfig struct {
	// Now is the moment the cooldown is evaluated at.
	Now time.Time
	// Envelopes is the immutable, collected provisioning envelope set. The
	// zero value means every instance's envelope is unknown, which refuses
	// every provisioning proposal by name.
	Envelopes Envelopes
	// Performance prices the two gp3 knobs. The zero value means
	// [DefaultPerformanceRates], which is unverified and therefore cannot
	// produce a claim.
	Performance PerformanceRates
	// MinWindow is the shortest I/O observation a REDUCTION may be drawn from.
	// Zero means [DefaultMinWindow].
	MinWindow time.Duration
	// Headroom is the fraction added to measured demand. Zero means
	// [DefaultParityHeadroom].
	Headroom float64
	// MinConfidence is the floor a proposal must clear. Zero means
	// [DefaultParityMinConfidence].
	MinConfidence float64
	// Percentile is the statistic demand is read at. Zero means
	// [DefaultParityPercentile].
	Percentile float64
}

func (c ParityConfig) normalized() ParityConfig {
	if c.Performance == (PerformanceRates{}) {
		c.Performance = DefaultPerformanceRates()
	}
	if c.MinWindow <= 0 {
		c.MinWindow = DefaultMinWindow
	}
	if c.Headroom < 0 || !finite(c.Headroom) {
		c.Headroom = DefaultParityHeadroom
	}
	if c.MinConfidence <= 0 || c.MinConfidence > 1 {
		c.MinConfidence = DefaultParityMinConfidence
	}
	if c.Percentile <= 0 || c.Percentile > 1 {
		c.Percentile = DefaultParityPercentile
	}
	return c
}

// Parity implements [StorageParity]. It is immutable after construction and
// therefore safe to share across goroutines; every field it reads is either a
// value or a slice it owns.
type Parity struct{ cfg ParityConfig }

// NewParity builds the parity engine. It validates the rate table at the
// boundary so a bad override fails there rather than producing a quietly wrong
// report.
func NewParity(cfg ParityConfig) (*Parity, error) {
	c := cfg.normalized()
	if err := c.Performance.Validate(); err != nil {
		return nil, err
	}
	return &Parity{cfg: c}, nil
}

// Config returns the normalized configuration.
func (p *Parity) Config() ParityConfig { return p.cfg }

// --- Measurement -----------------------------------------------------------

// IOMeasurement is what the four RDS I/O metrics said, and whether they said
// it completely.
type IOMeasurement struct {
	// Known is true only when ALL FOUR series were delivered in full. RDS
	// bills and throttles on total I/O, so a demand figure missing its write
	// half is not a smaller demand, it is an unknown one.
	Known bool `json:"known"`
	// Demand is the headroom-applied requirement.
	Demand Demand `json:"demand"`
	// Raw is the measurement before headroom.
	Raw Demand `json:"raw"`
	// Span is the period the series actually covered.
	Span time.Duration `json:"span"`
	// Samples is the smallest sample count across the four series.
	Samples int `json:"samples,omitempty"`
	// Missing names the series that were absent or truncated, in metric order.
	Missing []string `json:"missing,omitempty"`
}

// MeasureIO reads ReadIOPS + WriteIOPS and ReadThroughput + WriteThroughput.
//
// The two halves of each dimension are summed at the PERCENTILE rather than
// point-by-point. That over-states demand — p99(read) + p99(write) is at least
// p99(read+write) — and over-stating demand is the only safe direction here:
// this package exists to stop a conversion quietly delivering less than the
// volume was doing.
//
// Throughput arrives in bytes per second ("the average number of bytes read
// from disk per second") and is divided down to MiB/s, the unit
// ModifyDBInstance provisions in.
func MeasureIO(series []Series, percentile, headroom float64) IOMeasurement {
	m := IOMeasurement{Samples: math.MaxInt}
	find := func(metric string) (Series, bool) {
		for _, s := range series {
			if s.Metric == metric {
				return s, true
			}
		}
		return Series{}, false
	}
	// Fixed order, so Missing and every string derived from it are identical
	// run to run.
	wanted := []struct {
		metric string
		scale  float64
		into   *float64
	}{
		{MetricReadIOPS, 1, &m.Raw.IOPS},
		{MetricWriteIOPS, 1, &m.Raw.IOPS},
		{MetricReadThroughput, 1 / MiB, &m.Raw.ThroughputMBps},
		{MetricWriteThroughput, 1 / MiB, &m.Raw.ThroughputMBps},
	}
	ok := true
	for _, w := range wanted {
		s, found := find(w.metric)
		if !found || !s.Usable() {
			m.Missing = append(m.Missing, w.metric)
			ok = false
			continue
		}
		v, got := s.Percentile(percentile)
		if !got || !finite(v) || v < 0 {
			m.Missing = append(m.Missing, w.metric)
			ok = false
			continue
		}
		*w.into += v * w.scale
		if s.Len() < m.Samples {
			m.Samples = s.Len()
		}
		if span := spanOf(s); span > m.Span {
			m.Span = span
		}
	}
	if !ok {
		return IOMeasurement{Missing: m.Missing}
	}
	m.Known = true
	m.Demand = Demand{
		IOPS:           m.Raw.IOPS * (1 + headroom),
		ThroughputMBps: m.Raw.ThroughputMBps * (1 + headroom),
	}
	return m
}

func spanOf(s Series) time.Duration {
	if len(s.Points) < 2 {
		return 0
	}
	lo, hi := s.Points[0].At, s.Points[0].At
	for _, p := range s.Points[1:] {
		if p.At.Before(lo) {
			lo = p.At
		}
		if p.At.After(hi) {
			hi = p.At
		}
	}
	return hi.Sub(lo)
}

// --- The plan --------------------------------------------------------------

// ParityPlan is the outcome of the parity arithmetic for one instance: the
// configuration that clears demand, what each side costs, and what the naive
// rule would have done instead.
type ParityPlan struct {
	Engine   string `json:"engine"`
	SizeGiB  int64  `json:"sizeGiB"`
	FromType string `json:"fromType"`
	// GP2 is the current gp2 nameplate, present only on a conversion.
	GP2 GP2PerformanceRDS `json:"gp2,omitzero"`
	// Current is the configuration observed today, expressed in gp3 terms.
	Current GP3Config   `json:"current"`
	Regime  GP3Regime   `json:"regime"`
	Config  GP3Config   `json:"config"`
	Floor   ParityFloor `json:"floor"`

	// Required is the demand the configuration had to clear: measured demand
	// folded with the floor. Measured is the demand as handed in.
	Required Demand `json:"required"`
	Measured Demand `json:"measured"`

	CurrentParts  []CostPart `json:"currentParts,omitempty"`
	ProposedParts []CostPart `json:"proposedParts,omitempty"`

	CurrentMonthlyUSD  float64 `json:"currentMonthlyUSD"`
	ProposedMonthlyUSD float64 `json:"proposedMonthlyUSD"`
	// DeltaMonthlyUSD is current − proposed: positive is a list-price saving.
	// It is GROSS and, for storage, also NET: "The price for a reserved DB
	// instance doesn't provide a discount for the costs associated with
	// storage, backups, and I/O" [verified], so no Reserved DB Instance can
	// absorb any part of it.
	DeltaMonthlyUSD float64        `json:"deltaMonthlyUSD"`
	RateProvenance  RateProvenance `json:"rateProvenance,omitempty"`

	// Naive is the configuration a "gp3 is always cheaper" rule would pick:
	// the free baseline for this regime.
	Naive           GP3Config `json:"naive"`
	NaiveMonthlyUSD float64   `json:"naiveMonthlyUSD"`
	// NaiveDegrades reports whether that configuration fails to clear
	// Required, and the shortfalls say by how much.
	NaiveDegrades            bool    `json:"naiveDegrades,omitempty"`
	NaiveIOPSShortfall       float64 `json:"naiveIOPSShortfall,omitempty"`
	NaiveThroughputShortfall float64 `json:"naiveThroughputShortfall,omitempty"`
}

// Reduction reports whether this plan lowers provisioned performance rather
// than converting a storage type. It is the shape §2.4 says carries the money.
func (p ParityPlan) Reduction() bool { return p.FromType == StorageGP3 }

// PlanParity is the parity decision for one instance's storage.
//
// It returns a plan only when a gp3 configuration exists that (a) clears the
// required demand, (b) AWS would accept, and (c) costs strictly less than
// today. Otherwise it returns the reason it refused. There is no third outcome
// and no "close enough".
//
// The refusal regimes are real, not defensive padding:
//
//   - Below the striping threshold gp3 delivers 3,000 IOPS / 125 MiB/s and
//     accepts no provisioning at all, so a gp2 volume the published table
//     credits with 250 MiB/s has NO gp3 form at parity.
//   - At or above it, the range starts at 12,000 / 500. A conversion from a
//     gp2 volume the table credits with 1,000 MiB/s must buy 500 MiB/s of
//     provisioned throughput, which on a mid-sized volume costs more than the
//     storage line saves — a refusal band in a completely different place from
//     pkg/ebs's 334–375 GiB.
//   - A reduction stops at 12,000 / 500 and no lower. An instance already
//     there has an empty over-provisioned tail, which is stated rather than
//     skipped.
func PlanParity(card RateCard, perf PerformanceRates, e Engine, inst DBInstance,
	env Envelope, measured Demand, floor ParityFloor) (ParityPlan, *Suppression) {

	size := inst.AllocatedStorageGiB
	storageType := strings.ToLower(strings.TrimSpace(inst.StorageType))
	if storageType != StorageGP2 && storageType != StorageGP3 {
		return ParityPlan{}, &Suppression{ReasonParityStorageTypeNotModelled, fmt.Sprintf(
			"%s is on %s storage. This unit models gp2 and gp3 only: io1 and io2 are a different product "+
				"with their own price function and their own conversion risks, and magnetic storage is a "+
				"previous generation AWS prices per I/O request. Neither is described in a vocabulary this "+
				"table has", inst.DisplayName(), orNone(inst.StorageType)), time.Time{}}
	}
	if size <= 0 || size > MaxParitySizeGiB {
		return ParityPlan{}, &Suppression{ReasonParitySizeUnusable, fmt.Sprintf(
			"%s reports %d GiB of allocated storage, outside the 1–%d GiB range the published RDS storage "+
				"table covers. That is an unreadable inventory record, not a very large database, and it is "+
				"refused rather than priced", inst.DisplayName(), size, MaxParitySizeGiB), time.Time{}}
	}
	regime := GP3RegimeFor(e, size)
	if !regime.Known {
		return ParityPlan{}, &Suppression{ReasonParityGP2BandUnpublished, fmt.Sprintf(
			"no RDS storage striping threshold is encoded for engine %q, so neither the gp2 nameplate nor "+
				"the gp3 baseline is known for it. The threshold decides the baseline, the burst ceiling "+
				"and whether gp3 can be provisioned at all — an engine missing from that table is refused "+
				"by name rather than treated as though it were MySQL", e.String()), time.Time{}}
	}
	if !measured.valid() {
		return ParityPlan{}, &Suppression{ReasonParityNoMeasurement, fmt.Sprintf(
			"the I/O measurement for %s is not usable arithmetic (%v IOPS, %v MiB/s); a broken measurement "+
				"must not become a provisioning decision", inst.DisplayName(),
			measured.IOPS, measured.ThroughputMBps), time.Time{}}
	}

	plan := ParityPlan{
		Engine: e.Family, SizeGiB: size, FromType: storageType,
		Regime: regime, Floor: floor, Measured: measured,
	}

	// The floor: what must be preserved regardless of what was measured.
	required := measured
	switch storageType {
	case StorageGP2:
		gp2, ok := GP2PerformanceForRDS(e, size)
		if !ok {
			return plan, &Suppression{ReasonParityGP2BandUnpublished, fmt.Sprintf(
				"AWS publishes gp2 baseline, throughput and burst bands for MariaDB, MySQL and PostgreSQL, "+
					"and for SQL Server between 334 and 999 GiB. It publishes none for a %d GiB %s volume, "+
					"so there is no nameplate a conversion could preserve. Borrowing the MySQL row for an "+
					"engine that stripes at a different size is exactly the error this unit exists to "+
					"prevent", size, e.String()), time.Time{}}
		}
		plan.GP2 = gp2
		plan.Current = GP3Config{SizeGiB: size, IOPS: gp2.BaselineIOPS, ThroughputMBps: gp2.ParityThroughputMBps}
		if floor == ParityFloorNameplate {
			required = required.Max(Demand{
				IOPS:           float64(gp2.BaselineIOPS),
				ThroughputMBps: float64(gp2.ParityThroughputMBps),
			})
		}
	case StorageGP3:
		cur := currentGP3(regime, inst)
		plan.Current = cur
		if floor == ParityFloorNameplate {
			required = required.Max(Demand{
				IOPS: float64(cur.IOPS), ThroughputMBps: float64(cur.ThroughputMBps)})
		}
	}
	plan.Required = required

	cfg := configFor(regime, required)
	naive := GP3Config{SizeGiB: size, IOPS: regime.BaselineIOPS, ThroughputMBps: regime.BaselineThroughputMBps}
	plan.Config, plan.Naive = cfg, naive
	plan.NaiveDegrades = !naive.Clears(required)
	if plan.NaiveDegrades {
		plan.NaiveIOPSShortfall = math.Max(0, required.IOPS-float64(naive.IOPS))
		plan.NaiveThroughputShortfall = math.Max(0, required.ThroughputMBps-float64(naive.ThroughputMBps))
	}

	// Money. Both sides, then the delta — never accumulated as produced.
	curParts, curProv, curOK := currentCostParts(card, perf, regime, storageType, plan.Current)
	newParts, newProv, newOK := GP3CostParts(card, perf, regime, cfg)
	naiveParts, _, _ := GP3CostParts(card, perf, regime, naive)
	if !curOK || !newOK {
		return plan, &Suppression{ReasonParityNoCheaperConfig, fmt.Sprintf(
			"this rate card prices no %s storage, so the delta between %s and gp3 cannot be computed. No "+
				"price means no bill delta means nothing to claim", storageType, storageType), time.Time{}}
	}
	plan.CurrentParts, plan.ProposedParts = curParts, newParts
	plan.CurrentMonthlyUSD = SumUSD(curParts)
	plan.ProposedMonthlyUSD = SumUSD(newParts)
	plan.NaiveMonthlyUSD = SumUSD(naiveParts)
	plan.DeltaMonthlyUSD = plan.CurrentMonthlyUSD - plan.ProposedMonthlyUSD
	plan.RateProvenance = curProv.weakest(newProv)

	// The gate AWS itself would apply.
	if err := cfg.Validate(regime, env.For(StorageGP3)); err != nil {
		return plan, parityValidationRefusal(inst, e, regime, env, cfg, required, err)
	}
	// Belt and braces: the property FuzzRDSParityNeverUnderProvisions asserts,
	// checked in production too. configFor cannot violate it, and if it ever
	// does the answer is a refusal, not a degraded volume.
	if !cfg.Clears(required) {
		return plan, &Suppression{ReasonParityExceedsEnvelope, fmt.Sprintf(
			"internal: gp3 %d IOPS / %d MiB/s does not clear %.0f IOPS / %.0f MiB/s",
			cfg.IOPS, cfg.ThroughputMBps, required.IOPS, required.ThroughputMBps), time.Time{}}
	}
	if cfg == plan.Current && storageType == StorageGP3 {
		return plan, &Suppression{ReasonParityFloorsAtBaseline, fmt.Sprintf(
			"%s is provisioned at %d IOPS / %d MiB/s, which is the configuration its measured demand "+
				"(%.0f IOPS / %.0f MiB/s with headroom) already requires. In the %s regime the provisionable "+
				"range starts at %d IOPS / %d MiB/s and reduction stops there, so the over-provisioned tail "+
				"on this instance is empty",
			inst.DisplayName(), plan.Current.IOPS, plan.Current.ThroughputMBps,
			required.IOPS, required.ThroughputMBps, regimeName(regime),
			regime.BaselineIOPS, regime.BaselineThroughputMBps), time.Time{}}
	}
	if plan.DeltaMonthlyUSD <= parityEps {
		return plan, &Suppression{ReasonParityNoCheaperConfig, fmt.Sprintf(
			"gp3 at parity for %s (%d IOPS / %d MiB/s) costs %s/mo against %s's %s/mo: the %s regime's "+
				"baseline is %d IOPS / %d MiB/s, so preserving %d MiB/s has to be BOUGHT, and on a %d GiB "+
				"volume that costs %s/mo more than the storage line saves",
			inst.DisplayName(), cfg.IOPS, cfg.ThroughputMBps, fmtUSD(plan.ProposedMonthlyUSD),
			storageType, fmtUSD(plan.CurrentMonthlyUSD), regimeName(regime),
			regime.BaselineIOPS, regime.BaselineThroughputMBps, cfg.ThroughputMBps, size,
			fmtUSD(-plan.DeltaMonthlyUSD)), time.Time{}}
	}
	return plan, nil
}

// currentGP3 expresses an observed gp3 instance as a configuration. An
// instance reporting nothing is at its regime baseline: RDS returns the
// provisioned values only when they were set.
func currentGP3(r GP3Regime, inst DBInstance) GP3Config {
	c := GP3Config{SizeGiB: r.SizeGiB, IOPS: inst.IOPS, ThroughputMBps: inst.StorageThroughputMBps}
	if c.IOPS < r.BaselineIOPS {
		c.IOPS = r.BaselineIOPS
	}
	if c.ThroughputMBps < r.BaselineThroughputMBps {
		c.ThroughputMBps = r.BaselineThroughputMBps
	}
	c.ProvisionedIOPS = c.IOPS > r.BaselineIOPS
	c.ProvisionedThroughput = c.ThroughputMBps > r.BaselineThroughputMBps
	return c
}

func currentCostParts(card RateCard, perf PerformanceRates, r GP3Regime, storageType string,
	cur GP3Config) ([]CostPart, RateProvenance, bool) {

	if storageType == StorageGP2 {
		return GP2CostParts(card, cur.SizeGiB)
	}
	return GP3CostParts(card, perf, r, cur)
}

func regimeName(r GP3Regime) string {
	if r.Striped {
		return "striped"
	}
	if r.ThresholdGiB == NeverStripes {
		return "single-volume"
	}
	return "sub-threshold"
}

// parityValidationRefusal turns a configuration AWS would reject into the
// refusal that names WHY, so a reader can tell "we are below the striping
// threshold" from "we never read the envelope" from "this needs io2".
func parityValidationRefusal(inst DBInstance, e Engine, r GP3Regime, env Envelope,
	cfg GP3Config, required Demand, err error) *Suppression {

	switch {
	case cfg.Provisions() && !r.Provisionable:
		return &Suppression{ReasonParityNotProvisionableBelowThreshold, fmt.Sprintf(
			"%s holds %d GiB, below the %d GiB striping threshold for %s. Down there gp3 IS %d IOPS / "+
				"%d MiB/s and the published provisioning columns read \"N/A\" — \"you can provision "+
				"additional IOPS and storage throughput when storage size is at or above the threshold "+
				"value\". Clearing %.0f IOPS / %.0f MiB/s would need %d IOPS / %d MiB/s, which RDS will not "+
				"sell at this size at any price, so the conversion is refused rather than proposed at a "+
				"quiet %.0f MiB/s loss",
			inst.DisplayName(), r.SizeGiB, r.ThresholdGiB, e.String(),
			r.BaselineIOPS, r.BaselineThroughputMBps, required.IOPS, required.ThroughputMBps,
			cfg.IOPS, cfg.ThroughputMBps,
			math.Max(0, required.ThroughputMBps-float64(r.BaselineThroughputMBps))), time.Time{}}
	case cfg.Provisions() && !env.For(StorageGP3).Known:
		return &Suppression{ReasonParityEnvelopeUnknown, fmt.Sprintf(
			"provisioning %d IOPS / %d MiB/s on %s needs the range rds:DescribeValidDBInstanceModifications "+
				"reports, and it was not read. AWS's own storage page states two contradictory gp3 "+
				"ceilings — the gp3 table says 3,000–80,000 IOPS while the comparison table on the same "+
				"page says \"Maximum IOPS: 64,000 (16,000 on RDS for SQL Server)\" — so this package "+
				"hardcodes neither and refuses instead. Supply the seam to rds.NewEnvelopeCollector to "+
				"unblock it", cfg.IOPS, cfg.ThroughputMBps, inst.DisplayName()), time.Time{}}
	}
	return &Suppression{ReasonParityExceedsEnvelope, fmt.Sprintf(
		"no gp3 configuration this instance accepts clears %.0f IOPS / %.0f MiB/s on %s (envelope: %s): %v. "+
			"A volume that needs more than gp3 sells needs io2 or a larger allocation, not a cheaper "+
			"storage type", required.IOPS, required.ThroughputMBps, inst.DisplayName(),
		env.For(StorageGP3).Describe(), err), time.Time{}}
}

// --- Confidence ------------------------------------------------------------

// Parity confidence weights. They sum to 1. Measurement carries the largest
// weight because a parity decision is a measurement decision.
const (
	weightParityMeasurement = 0.35
	weightParityWindow      = 0.25
	weightParityEnvelope    = 0.20
	weightParityHeadroom    = 0.20
)

// ParityConfidenceFactor is one earned component of a parity confidence score.
type ParityConfidenceFactor struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Earned float64 `json:"earned"` // 0..1
	Why    string  `json:"why"`
}

// ParityConfidence is a score built from nothing. It starts at zero and adds
// only what the evidence earns, so a missing signal cannot be mistaken for a
// present one. This is the "earned, not lost" structure pkg/ec2 and pkg/lambda
// established, and FINDINGS §7.3 named as the shape to copy.
type ParityConfidence struct {
	Score   float64                  `json:"score"`
	Factors []ParityConfidenceFactor `json:"factors,omitempty"`
}

func (c *ParityConfidence) add(name string, weight, earned float64, why string) {
	if !finite(earned) || earned < 0 {
		earned = 0
	}
	if earned > 1 {
		earned = 1
	}
	c.Factors = append(c.Factors, ParityConfidenceFactor{
		Name: name, Weight: weight, Earned: earned, Why: why})
	c.Score += weight * earned
}

// WeakestFactor names the factor that cost the most confidence, so a
// low-confidence refusal says what would fix it. It is the fourth copy of
// pkg/ec2's weakestFactor and, as FINDINGS §7.3 says, that is its own argument
// for lifting the shape into a shared package.
func (c ParityConfidence) WeakestFactor() string {
	worst, lost := "", -1.0
	for _, f := range c.Factors {
		if l := f.Weight * (1 - f.Earned); l > lost {
			worst, lost = f.Name+": "+f.Why, l
		}
	}
	if worst == "" {
		return "no single dominant factor"
	}
	return worst
}

func (p *Parity) confidence(m IOMeasurement, env Envelope, plan ParityPlan) ParityConfidence {
	var c ParityConfidence

	measured, why := 0.0, "no complete I/O series was delivered"
	if m.Known {
		measured, why = 1, fmt.Sprintf("%d datapoints across ReadIOPS, WriteIOPS, ReadThroughput and "+
			"WriteThroughput", m.Samples)
	}
	c.add("io-measurement", weightParityMeasurement, measured, why)

	window := 0.0
	if p.cfg.MinWindow > 0 {
		window = m.Span.Seconds() / p.cfg.MinWindow.Seconds()
	}
	c.add("window", weightParityWindow, window, fmt.Sprintf("observed %s of I/O against a %s minimum",
		m.Span.Round(time.Minute), p.cfg.MinWindow.Round(time.Minute)))

	envEarned, envWhy := 0.0, "the provisioning envelope was not read"
	if e := env.For(StorageGP3); e.Known {
		envEarned, envWhy = 1, "rds:DescribeValidDBInstanceModifications reported "+e.Describe()
	} else if !plan.Config.Provisions() {
		// A configuration that provisions nothing needs no envelope, and a
		// factor that punishes it for lacking one would be measuring the wrong
		// thing.
		envEarned, envWhy = 1, "the proposal provisions nothing above the baseline, so no envelope is needed"
	}
	c.add("envelope", weightParityEnvelope, envEarned, envWhy)

	head, headWhy := 0.0, "measured demand already meets the proposed configuration"
	if plan.Config.IOPS > 0 && plan.Config.ThroughputMBps > 0 && m.Known {
		iopsMargin := 1 - m.Raw.IOPS/float64(plan.Config.IOPS)
		tputMargin := 1 - m.Raw.ThroughputMBps/float64(plan.Config.ThroughputMBps)
		head = math.Min(1, math.Max(0, math.Min(iopsMargin, tputMargin))/p.cfg.Headroom)
		headWhy = fmt.Sprintf("%.0f IOPS / %.0f MiB/s measured against a proposed %d IOPS / %d MiB/s",
			m.Raw.IOPS, m.Raw.ThroughputMBps, plan.Config.IOPS, plan.Config.ThroughputMBps)
	}
	c.add("headroom", weightParityHeadroom, head, headWhy)
	return c
}

// --- The seam ---------------------------------------------------------------

// AssessParity implements [StorageParity].
//
// It always returns ok=true and at least one suppression. "Declined to look"
// and "looked and found nothing" are different facts, and a seam that can
// return silently would make them indistinguishable in the report — the same
// argument U11 makes for every assessment carrying a reason.
//
// A proposal is produced only when ALL of the following hold, and each failure
// is a named refusal:
//
//	the storage type is gp2 or gp3           ReasonParityStorageTypeNotModelled
//	the size is inside the published table   ReasonParitySizeUnusable
//	the engine has an encoded threshold      ReasonParityGP2BandUnpublished
//	the instance is not mid-modification     ReasonParityStorageOptimization
//	fewer than four modifications in 24 h    ReasonParityCooldown
//	the I/O series were delivered in full    ReasonParityNoMeasurement
//	the window is long enough for a cut      ReasonParityWindowTooShort
//	gp3 can be provisioned at this size      ReasonParityNotProvisionableBelowThreshold
//	the live envelope was read               ReasonParityEnvelopeUnknown
//	the configuration is inside it           ReasonParityExceedsEnvelope
//	it is strictly cheaper                   ReasonParityNoCheaperConfig
//	the tail above the baseline is non-empty ReasonParityFloorsAtBaseline
//	confidence clears the floor              ReasonParityLowConfidence
//	no unverified rate sizes the claim       ReasonUnverifiedRate
func (p *Parity) AssessParity(inst DBInstance, e Engine, series []Series, card RateCard) (
	*Proposal, []Suppression, bool) {

	var sup []Suppression
	add := func(s Suppression) { sup = append(sup, s) }

	// Instances are described by identifier; the ARN is tried as a fallback so
	// a caller that keyed its envelopes the other way still gets an answer
	// rather than a silent refusal.
	env := p.cfg.Envelopes.Get(inst.Identifier)
	if len(env.Storage) == 0 && !env.HistoryKnown {
		if alt := p.cfg.Envelopes.Get(inst.ARN); len(alt.Storage) > 0 || alt.HistoryKnown {
			env = alt
		}
	}

	// The two facts that block a modification outright, checked before any
	// arithmetic: a change that cannot be applied is not advice.
	if strings.EqualFold(strings.TrimSpace(inst.Status), StatusStorageOptimization) ||
		strings.EqualFold(strings.TrimSpace(inst.Status), StatusModifying) {
		add(Suppression{ReasonParityStorageOptimization, fmt.Sprintf(
			"%s is in state %q. \"You can't modify allocated storage if the DB instance status is "+
				"storage-optimization\", a storage modification leaves an instance in that state for hours "+
				"afterwards, and a reading taken during one describes a transient shape rather than the "+
				"steady state this arithmetic assumes", inst.DisplayName(), inst.Status), time.Time{}})
		return nil, sup, true
	}
	cool := env.Cooldown(p.cfg.Now)
	if cool.Blocked {
		add(Suppression{ReasonParityCooldown, fmt.Sprintf(
			"%s has had %d storage modifications in the last %s. \"You can perform a maximum of four "+
				"storage modifications on a DB instance within any 24-hour period\", so a fifth is not a "+
				"recommendation, it is an API error with a dollar figure attached. The limit clears at %s",
			inst.DisplayName(), cool.Recent, StorageModificationWindow,
			cool.ClearsAt.UTC().Format(time.RFC3339)), time.Time{}})
		return nil, sup, true
	}

	// Measurement decides the floor. No measurement is not a small number.
	m := MeasureIO(series, p.cfg.Percentile, p.cfg.Headroom)
	floor := ParityFloorMeasured
	switch {
	case !m.Known:
		floor = ParityFloorNameplate
		add(Suppression{ReasonParityNoMeasurement, fmt.Sprintf(
			"CloudWatch did not deliver %s in full for %s, so measured demand is unknown rather than low. "+
				"The conversion arithmetic falls back to the published nameplate, which cannot degrade "+
				"anything, and no reduction of provisioned performance is considered at all: cutting a "+
				"number nobody measured is a guess wearing a decimal point",
			strings.Join(m.Missing, ", "), inst.DisplayName()), time.Time{}})
	case m.Span < p.cfg.MinWindow:
		floor = ParityFloorNameplate
		add(Suppression{ReasonParityWindowTooShort, fmt.Sprintf(
			"the I/O series for %s spans %s, below the %s minimum. A database's peak is a weekly and "+
				"monthly shape; a window that cannot contain one cannot rule one out, so the nameplate "+
				"floor applies and no reduction is considered",
			inst.DisplayName(), m.Span.Round(time.Minute), p.cfg.MinWindow), time.Time{}})
	}

	plan, refusal := PlanParity(card, p.cfg.Performance, e, inst, env, m.Demand, floor)
	if refusal != nil {
		add(*refusal)
		return nil, sup, true
	}
	// A reduction on thin evidence is refused above; reaching here with the
	// nameplate floor and a gp3 source would mean proposing a cut nobody
	// measured, so the plan is only ever a conversion in that state.
	if plan.Reduction() && floor == ParityFloorNameplate {
		return nil, sup, true
	}

	conf := p.confidence(m, env, plan)
	if conf.Score < p.cfg.MinConfidence {
		add(Suppression{ReasonParityLowConfidence, fmt.Sprintf(
			"confidence %.2f is below the %.2f floor for %s (%s): the evidence supports watching this "+
				"volume, not changing it", conf.Score, p.cfg.MinConfidence, inst.DisplayName(),
			conf.WeakestFactor()), time.Time{}})
		return nil, sup, true
	}

	// U11's rule, reused rather than re-derived: an unverified rate may size a
	// reported fact and may never become a claimed saving.
	if !plan.RateProvenance.Claimable() {
		add(Suppression{ReasonUnverifiedRate, fmt.Sprintf(
			"a %s gp3 configuration for %s would save on the order of %s/mo, and the rate behind that "+
				"number is %q. §7 records that the RDS gp2/gp3 $/GiB-month figures and the provisioned "+
				"IOPS and throughput rates could not be retrieved from AWS, so this sizes the opportunity "+
				"and is not a saving. Supply verified rates with rds.LoadRates and rds.PerformanceRates "+
				"before putting it in front of a finance team",
			regimeName(plan.Regime), inst.DisplayName(), fmtUSD(plan.DeltaMonthlyUSD),
			plan.RateProvenance), time.Time{}})
		return nil, sup, true
	}

	prop := &Proposal{
		AllocatedStorageGiB: inst.AllocatedStorageGiB, // the trap-8 ratchet guard: never below observed
		StorageType:         StorageGP3,
		Action:              domain.ActionAdvisory,
		Risk:                parityRisk(plan),
		Confidence:          conf.Score,
		Reason:              parityReason(inst, plan),
		// Storage is billed on its own line and no Reserved DB Instance
		// discounts it, so gross and net are the same number by construction
		// rather than by an unapplied ledger.
		// IOPS and StorageThroughputMBps are EFFECTIVE TOTALS, which is what
		// ModifyDBInstance takes — it accepts absolute values, never deltas.
		// U14 decides whether to SEND them by asking GP3RegimeFor whether this
		// size can be provisioned at all; a value equal to the regime baseline
		// is what the volume delivers for free and needs no API argument.
		IOPS:                   plan.Config.IOPS,
		StorageThroughputMBps:  plan.Config.ThroughputMBps,
		ProposedHourlyUSD:      plan.ProposedMonthlyUSD / HoursPerMonth,
		GrossSavingsMonthlyUSD: plan.DeltaMonthlyUSD,
		NetSavingsMonthlyUSD:   plan.DeltaMonthlyUSD,
		RateProvenance:         plan.RateProvenance,
	}
	// Even on the happy path the instance keeps its permanent storage
	// refusals: the class is still a failover and the allocation still cannot
	// shrink. Those come from U11; what this seam adds is the one thing it
	// WILL stand behind.
	add(Suppression{ReasonParityFloorsAtBaseline, fmt.Sprintf(
		"%s can go to %d IOPS / %d MiB/s and no lower: in the %s regime the provisionable range starts at "+
			"%d IOPS / %d MiB/s, so the proposal above stops at that floor rather than at measured demand "+
			"(%.0f IOPS / %.0f MiB/s with headroom)",
		inst.DisplayName(), plan.Config.IOPS, plan.Config.ThroughputMBps, regimeName(plan.Regime),
		plan.Regime.BaselineIOPS, plan.Regime.BaselineThroughputMBps,
		plan.Required.IOPS, plan.Required.ThroughputMBps), time.Time{}})
	return prop, sup, true
}

func parityRisk(plan ParityPlan) string {
	if plan.Reduction() {
		return RiskMedium
	}
	return RiskLow
}

func parityReason(inst DBInstance, plan ParityPlan) string {
	if plan.Reduction() {
		return fmt.Sprintf(
			"%s is provisioned at %d IOPS / %d MiB/s and measured %.0f IOPS / %.0f MiB/s at p99 with "+
				"headroom. Reducing to %d IOPS / %d MiB/s — the floor of the %s regime's provisionable "+
				"range — clears that demand and saves %s/mo. Downtime does not occur during this change",
			inst.DisplayName(), plan.Current.IOPS, plan.Current.ThroughputMBps,
			plan.Required.IOPS, plan.Required.ThroughputMBps,
			plan.Config.IOPS, plan.Config.ThroughputMBps, regimeName(plan.Regime),
			fmtUSD(plan.DeltaMonthlyUSD))
	}
	return fmt.Sprintf(
		"%s is on gp2 delivering a published %d IOPS / %d MiB/s. A gp3 volume at %d IOPS / %d MiB/s clears "+
			"the required %.0f IOPS / %.0f MiB/s and saves %s/mo. Downtime does not occur during this change",
		inst.DisplayName(), plan.GP2.BaselineIOPS, plan.GP2.ParityThroughputMBps,
		plan.Config.IOPS, plan.Config.ThroughputMBps,
		plan.Required.IOPS, plan.Required.ThroughputMBps, fmtUSD(plan.DeltaMonthlyUSD))
}
