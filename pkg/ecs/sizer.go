package ecs

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Config tunes the sizer. Start from [DefaultConfig]; the zero value fills in
// from it one field at a time, so a partially-set config is safe.
type Config struct {
	// Rates prices tiers. Zero value ⇒ DefaultRates().
	Rates Rates
	// Region labels commitment usage lines. Compute Savings Plans absorb
	// Fargate account-wide, so this is documentation rather than a matching
	// key. "" is legal.
	Region string
	// DefaultMode is the kilter.dev/mode assumed for services that carry no
	// tag: off | recommend | apply. Empty ⇒ apply, matching the Kubernetes
	// annotation's opt-out semantics.
	DefaultMode string

	// CPUPercentile is the percentile of observed CPU demand that sizing
	// targets. Default 0.95.
	CPUPercentile float64
	// CPUHeadroom multiplies that percentile. Default 1.30.
	CPUHeadroom float64
	// MemHeadroom multiplies the observed memory PEAK — not a percentile.
	// Exceeding a Fargate task's memory kills the task; exceeding its CPU
	// throttles it. The asymmetry in the policy is the asymmetry in the
	// failure mode. Default 1.25.
	MemHeadroom float64

	// MinWindow is the shortest observation span a proposal may rest on.
	// Default 7 days (§4.7's "≥ 7-day window covering business peaks").
	MinWindow time.Duration
	// MinSamples is the fewest datapoints a proposal may rest on. Default 720
	// — twelve hours of 1-minute data, which a 7-day window clears easily
	// unless CloudWatch has holes in it.
	MinSamples int
	// MinConfidence gates proposals. Default 0.65.
	MinConfidence float64
	// MaxSampleAge is the freshness horizon in the confidence score.
	// Default 2h.
	MaxSampleAge time.Duration

	// SpotAdvisory enables the Fargate Spot advisory (ECS only, §7 trap 3).
	SpotAdvisory bool
	// ARMAdvisory enables the Graviton advisory (ECS only, §7 trap 3).
	ARMAdvisory bool
	// MinAdvisoryMonthlyUSD is the smallest estimate worth reporting as an
	// advisory. Default $1.00/month.
	MinAdvisoryMonthlyUSD float64
	// MinMoveMonthlyUSD is the smallest saving worth a rolling deployment.
	// Default $0.10/month.
	MinMoveMonthlyUSD float64
}

// DefaultConfig returns the shipped defaults. Both ECS-only advisories are ON:
// they are the high-value levers this domain exists for, and they are advisory,
// so enabling them cannot move a task.
func DefaultConfig() Config {
	return Config{
		Rates:                 DefaultRates(),
		DefaultMode:           modeApply,
		CPUPercentile:         0.95,
		CPUHeadroom:           1.30,
		MemHeadroom:           1.25,
		MinWindow:             7 * 24 * time.Hour,
		MinSamples:            720,
		MinConfidence:         0.65,
		MaxSampleAge:          2 * time.Hour,
		SpotAdvisory:          true,
		ARMAdvisory:           true,
		MinAdvisoryMonthlyUSD: 1.00,
		MinMoveMonthlyUSD:     0.10,
	}
}

// withDefaults substitutes the documented default for any unset or
// out-of-range field. A garbage config must yield the conservative default,
// never a disabled guard.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	c.Rates = c.Rates.withDefaults()
	if !validMode(c.DefaultMode) {
		c.DefaultMode = d.DefaultMode
	}
	if !(c.CPUPercentile > 0) || c.CPUPercentile > 1 { // catches NaN
		c.CPUPercentile = d.CPUPercentile
	}
	if !(c.CPUHeadroom >= 1) || math.IsInf(c.CPUHeadroom, 0) {
		c.CPUHeadroom = d.CPUHeadroom
	}
	if !(c.MemHeadroom >= 1) || math.IsInf(c.MemHeadroom, 0) {
		c.MemHeadroom = d.MemHeadroom
	}
	if c.MinWindow <= 0 {
		c.MinWindow = d.MinWindow
	}
	if c.MinSamples <= 0 {
		c.MinSamples = d.MinSamples
	}
	if !(c.MinConfidence >= 0) || c.MinConfidence > 1 {
		c.MinConfidence = d.MinConfidence
	}
	if c.MaxSampleAge <= 0 {
		c.MaxSampleAge = d.MaxSampleAge
	}
	if !(c.MinAdvisoryMonthlyUSD >= 0) || math.IsInf(c.MinAdvisoryMonthlyUSD, 0) {
		c.MinAdvisoryMonthlyUSD = d.MinAdvisoryMonthlyUSD
	}
	if !(c.MinMoveMonthlyUSD >= 0) || math.IsInf(c.MinMoveMonthlyUSD, 0) {
		c.MinMoveMonthlyUSD = d.MinMoveMonthlyUSD
	}
	return c
}

// Sizer turns observations into assessments. It is pure: no clock, no I/O, no
// mutable state — two Sizers with the same config produce identical output for
// the same input, in any order.
type Sizer struct{ cfg Config }

// NewSizer builds a sizer.
func NewSizer(cfg Config) *Sizer { return &Sizer{cfg: cfg.withDefaults()} }

// Config returns the effective configuration, defaults applied.
func (s *Sizer) Config() Config { return s.cfg }

// Demand is the absolute compute an observation implies, recovered from
// percentages of the reservation they were measured against.
type Demand struct {
	// CPU and Memory are post-headroom absolute demand.
	CPU    model.Resources `json:"-"`
	Target model.Resources `json:"target"`
	// CPUPercentP95 and MemPercentPeak are the raw CloudWatch numbers, kept so
	// a reader can redo the multiplication by hand.
	CPUPercentP95  float64 `json:"cpuPercentP95"`
	MemPercentPeak float64 `json:"memPercentPeak"`
	// Reserved is the denominator the percentages were converted against.
	Reserved model.Resources `json:"reserved"`
	Samples  int             `json:"samples"`
	Window   time.Duration   `json:"window"`
	// Trimmed is how many datapoints were dropped because they belonged to a
	// different task-definition revision.
	Trimmed int `json:"trimmed,omitempty"`
}

// Proposal is a task-size change the sizer is prepared to stand behind.
type Proposal struct {
	Tier      pricing.FargateConfig `json:"tier"`
	Spec      domain.Spec           `json:"spec"`
	HourlyUSD float64               `json:"hourlyUSD"`
	// GrossMonthlyUSD is the on-demand list-price delta. NetMonthlyUSD is the
	// bill delta after the commitment waterfall and is the only number that may
	// be presented as a saving.
	GrossMonthlyUSD float64            `json:"grossMonthlyUSD"`
	NetMonthlyUSD   float64            `json:"netMonthlyUSD"`
	Action          domain.ActionClass `json:"action"`
	Risk            string             `json:"risk"`
	Reason          string             `json:"reason"`
}

// Assessment is one service's complete verdict. Exactly one is produced per
// observed service, and an assessment with no proposal always carries at least
// one [Suppression] saying why — silence is never an output.
type Assessment struct {
	Ref     domain.TargetRef `json:"ref"`
	Cluster string           `json:"cluster"`
	Service string           `json:"service"`

	Current          domain.Spec           `json:"current"`
	CurrentTier      pricing.FargateConfig `json:"currentTier,omitzero"`
	CurrentHourlyUSD float64               `json:"currentHourlyUSD"`
	Platform         Platform              `json:"platform"`
	Tasks            int                   `json:"tasks"`

	Demand       Demand            `json:"demand,omitzero"`
	Confidence   float64           `json:"confidence"`
	Proposal     *Proposal         `json:"proposal,omitempty"`
	Suppressions []Suppression     `json:"suppressions,omitempty"`
	Advisories   []Advisory        `json:"advisories,omitempty"`
	Evidence     []domain.Evidence `json:"evidence,omitempty"`
	Notes        []string          `json:"notes,omitempty"`
}

// ClaimableMonthlyUSD is the only number this package presents as a saving: the
// net delta of an unsuppressed proposal. An advisory's estimate is never
// included — its precondition is not observable, so it is not a claim.
func (a Assessment) ClaimableMonthlyUSD() float64 {
	if a.Proposal == nil || len(a.Suppressions) > 0 || a.Proposal.NetMonthlyUSD <= 0 {
		return 0
	}
	return a.Proposal.NetMonthlyUSD
}

// suppress appends a blocking reason.
func (a *Assessment) suppress(code, format string, args ...any) {
	a.Suppressions = append(a.Suppressions, Suppression{Code: code, Reason: fmt.Sprintf(format, args...)})
}

// Assess produces the verdict for one observation as of now. ledger nets the
// commitment waterfall and may be nil, in which case net equals gross — with no
// known commitment nothing can be stranded.
func (s *Sizer) Assess(o Observation, now time.Time, ledger domain.Netter) Assessment {
	a := Assessment{
		Ref:      o.Ref,
		Cluster:  o.Service.ClusterName(),
		Service:  o.Service.ServiceName,
		Tasks:    o.Service.DesiredCount,
		Notes:    append([]string(nil), o.Notes...),
		Platform: platformOf(o),
	}

	// 1. Ownership and guardrails. A mode=off service is reported as refused
	//    rather than dropped: a silently missing service is indistinguishable
	//    from a collector that failed.
	mode := modeFor(o.Tags, s.cfg.DefaultMode)
	if mode == modeOff {
		a.suppress(ReasonModeOff, "service is tagged %s=off: Kilter never changes it", TagKilterMode)
	}
	if !o.Service.IsFargate() {
		a.suppress(ReasonNotFargate, "service launch type %q is not Fargate; it belongs to a node domain",
			o.Service.LaunchType)
	}

	// 2. The reservation — the denominator. Without a valid tier there is no
	//    honest conversion from percent-of-reserved to demand, and no honest
	//    current bill either.
	tier, err := TierFor(o.Reserved.Reserved)
	switch {
	case o.Reserved.IsZero():
		a.suppress(ReasonInvalidTaskSize,
			"task definition %s declares no readable task-level cpu/memory: the utilization percentages have no denominator",
			o.TaskDef.TaskDefinitionARN)
	case err != nil:
		a.suppress(ReasonInvalidTaskSize,
			"task definition %s declares %s, which is not a valid Fargate task size", o.TaskDef.TaskDefinitionARN, o.Reserved.Reserved)
	default:
		a.CurrentTier = tier
		a.CurrentHourlyUSD = s.cfg.Rates.Cost(tier, a.Platform) * float64(max(a.Tasks, 0))
	}
	a.Current = s.currentSpec(o, tier)

	// 3. Shape constraints that would make any proposal unregisterable.
	if o.TaskDef.NetworkMode != "" && o.TaskDef.NetworkMode != NetworkModeAWSVPC {
		a.suppress(ReasonNetworkMode,
			"task definition uses networkMode %q; Fargate requires %q, so no revision of this shape can run",
			o.TaskDef.NetworkMode, NetworkModeAWSVPC)
	}

	// 4. Mid-rollout. Re-pointing a service that is still converging cancels
	//    the in-flight deployment and makes the metric window unattributable.
	if inProgress, why := o.Service.DeploymentInProgress(); inProgress {
		a.suppress(ReasonDeploymentInProgress, "%s", why)
	}
	if a.Tasks <= 0 {
		a.suppress(ReasonServiceIdle, "desired count is %d: the service bills nothing to optimize", a.Tasks)
	}

	// 5. Evidence. Everything below needs a usable window.
	d, evOK := s.demand(&a, o, now)
	a.Demand = d
	a.Confidence = s.confidence(d, o, now)
	a.Evidence = s.evidence(o, a, d)

	if !evOK || len(a.Suppressions) > 0 {
		// Advisories still make sense when the block is economic or
		// evidential, but not when we cannot even price the service.
		if a.CurrentTier.IsZero() {
			return a
		}
		a.Advisories = s.advisories(o, a, a.CurrentTier)
		return a
	}
	if a.Confidence < s.cfg.MinConfidence {
		a.suppress(ReasonLowConfidence, "confidence %.2f is below the %.2f floor for a rolling deployment",
			a.Confidence, s.cfg.MinConfidence)
		a.Advisories = s.advisories(o, a, a.CurrentTier)
		return a
	}

	s.propose(&a, o, d, mode, ledger)
	base := a.CurrentTier
	if a.Proposal != nil {
		base = a.Proposal.Tier
	}
	a.Advisories = s.advisories(o, a, base)
	return a
}

// demand recovers absolute demand from ECS's percent-of-reserved metrics.
//
// This is the unit's central computation and its central failure mode. The
// conversion is [AbsoluteFromPercent] — used ×, not ÷ — applied against the
// reservation of the revision that PRODUCED the datapoints, which is why the
// window is trimmed to the current deployment first.
func (s *Sizer) demand(a *Assessment, o Observation, now time.Time) (Demand, bool) {
	d := Demand{Reserved: o.Reserved.Reserved}
	if o.Reserved.IsZero() {
		return d, false
	}

	cpu, mem := o.CPUPercent, o.MemPercent
	before := cpu.Len() + mem.Len()

	// Revision drift: datapoints published before the current deployment
	// started are percentages of a DIFFERENT reservation. Converting them with
	// this denominator would rescale history to a size that never ran.
	if p, ok := o.Service.Primary(); ok && !p.CreatedAt.IsZero() && p.CreatedAt.After(o.Window.Start) {
		cpu = cpu.TrimBefore(p.CreatedAt)
		mem = mem.TrimBefore(p.CreatedAt)
		d.Trimmed = before - (cpu.Len() + mem.Len())
		if d.Trimmed > 0 {
			a.Notes = append(a.Notes, fmt.Sprintf(
				"dropped %d datapoint(s) published before deployment %s (%s): they are percentages of an older revision's reservation",
				d.Trimmed, p.ID, p.CreatedAt.UTC().Format(time.RFC3339)))
		}
	}

	if !cpu.Complete() || !mem.Complete() {
		a.suppress(ReasonPartialMetrics,
			"CloudWatch returned %s/%s for this service: the window is not whole, so no percentile from it is trustworthy",
			statusOr(cpu.StatusCode), statusOr(mem.StatusCode))
		return d, false
	}
	if cpu.Len() == 0 || mem.Len() == 0 {
		a.suppress(ReasonNoMetricWindow,
			"no %s/%s datapoints in %s: ECS publishes these free for Fargate services, so an empty window means the service was not running or the metrics were not readable",
			MetricCPUUtilization, MetricMemoryUtilization, o.Window.Span().Round(time.Minute))
		return d, false
	}

	samples := min(cpu.Len(), mem.Len())
	window := min(cpu.Span(), mem.Span())
	d.Samples, d.Window = samples, window

	if d.Trimmed > 0 && (samples < s.cfg.MinSamples || window < s.cfg.MinWindow) {
		a.suppress(ReasonRevisionDrift,
			"the task definition changed inside the window; only %d datapoint(s) over %s belong to the running revision (need %d over %s)",
			samples, window.Round(time.Minute), s.cfg.MinSamples, s.cfg.MinWindow)
		return d, false
	}
	if window < s.cfg.MinWindow {
		a.suppress(ReasonInsufficientWindow, "observed %s of history, below the %s minimum",
			window.Round(time.Minute), s.cfg.MinWindow)
		return d, false
	}
	if samples < s.cfg.MinSamples {
		a.suppress(ReasonInsufficientSamples, "observed %d datapoint(s), below the %d minimum",
			samples, s.cfg.MinSamples)
		return d, false
	}

	d.CPUPercentP95 = cpu.Percentile(s.cfg.CPUPercentile)
	d.MemPercentPeak = mem.Max()

	// The multiplication. reserved is the observed revision's reservation, and
	// nothing else may be substituted for it.
	cpuAbs := AbsoluteFromPercent(d.CPUPercentP95, d.Reserved.MilliCPU)
	memAbs := AbsoluteFromPercent(d.MemPercentPeak, d.Reserved.MemoryBytes)

	d.Target = model.Resources{
		MilliCPU:    ceilInt64(cpuAbs * s.cfg.CPUHeadroom),
		MemoryBytes: ceilInt64(memAbs * s.cfg.MemHeadroom),
	}
	d.CPU = model.Resources{MilliCPU: ceilInt64(cpuAbs), MemoryBytes: ceilInt64(memAbs)}
	return d, true
}

// propose builds the task-size proposal, or the reason there is none.
func (s *Sizer) propose(a *Assessment, o Observation, d Demand, mode string, ledger domain.Netter) {
	floors := o.TaskDef.ContainerFloors()
	want := d.Target.Max(floors)

	bare, bareErr := RoundUpTier(d.Target)
	tier, err := RoundUpTier(want)
	if err != nil {
		a.suppress(ReasonTooLargeForFargate, "demand %s rounds past the 16 vCPU / 120 GB Fargate ceiling: %v", want, err)
		return
	}
	if bareErr == nil && bare != tier {
		a.Notes = append(a.Notes, fmt.Sprintf(
			"container-level limits hold the task at %s; demand alone would fit %s", tier, bare))
	}

	// 8 and 16 vCPU task sizes need platform version 1.4.0 or later.
	if tier.MilliCPU >= 8000 && !platformVersionAtLeast(o.Service.PlatformVersion, 1, 4) {
		a.suppress(ReasonPlatformVersion,
			"task size %s needs Fargate platform version 1.4.0 or later; the service is pinned to %q",
			tier, o.Service.PlatformVersion)
		return
	}

	proposedHourly := s.cfg.Rates.Cost(tier, a.Platform) * float64(a.Tasks)
	grossMonthly := (a.CurrentHourlyUSD - proposedHourly) * pricing.HoursPerMonth

	// §7 trap 2, stated as a refusal rather than as a number: a request change
	// that does not cross a tier boundary saves exactly $0. Quantization is why
	// "we shaved 15 % off the CPU request" and "we saved money" are different
	// claims.
	if tier == a.CurrentTier {
		if bareErr == nil && bare != a.CurrentTier {
			a.suppress(ReasonContainerLimits,
				"container hard limits (%s) hold the task at %s; without them the learned demand would fit %s",
				floors, a.CurrentTier, bare)
			return
		}
		a.suppress(ReasonNoTierChange,
			"learned demand %s rounds to %s — the tier the service is already billed at — so the change saves exactly $0.00",
			d.Target, tier)
		return
	}
	if grossMonthly < 0 {
		a.suppress(ReasonUndersized,
			"learned demand %s needs %s, larger than the current %s: growing the task costs $%.2f/mo and is reported, not proposed",
			d.Target, tier, a.CurrentTier, -grossMonthly)
		a.Advisories = append(a.Advisories, Advisory{
			Code:                AdvisoryUndersized,
			EstimatedMonthlyUSD: grossMonthly,
			Caveat:              "spending money is a human decision; this domain proposes reductions only",
			Detail:              fmt.Sprintf("%s → %s", a.CurrentTier, tier),
			Proposed:            s.proposedSpec(o, tier, ChangeTaskSize, a.Tasks),
		})
		return
	}
	if grossMonthly == 0 {
		a.suppress(ReasonZeroSaving,
			"%s and %s are different task sizes at the same price: a rolling deployment for $0.00",
			a.CurrentTier, tier)
		return
	}
	if grossMonthly < s.cfg.MinMoveMonthlyUSD {
		a.suppress(ReasonBelowMoveFloor,
			"saving $%.2f/mo is below the $%.2f floor for a rolling deployment",
			grossMonthly, s.cfg.MinMoveMonthlyUSD)
		return
	}

	p := &Proposal{
		Tier:      tier,
		Spec:      s.proposedSpec(o, tier, ChangeTaskSize, a.Tasks),
		HourlyUSD: proposedHourly,
		Action:    domain.ActionRolling,
		Risk:      riskOf(a.CurrentTier, tier),
	}
	net := grossMonthly
	if ledger != nil {
		before := s.usageLine(a.Ref, s.cfg.Rates.Cost(a.CurrentTier, a.Platform), a.Tasks)
		after := s.usageLine(a.Ref, s.cfg.Rates.Cost(tier, a.Platform), a.Tasks)
		as := ledger.Net([]commit.UsageLine{before}, []commit.UsageLine{after})
		net = as.NetMonthlyUSD
		if as.Suppressed {
			a.Suppressions = append(a.Suppressions, Suppression{
				Code: as.ReasonCode, Reason: as.Reason, ValidFrom: as.ValidFrom,
			})
			if net > 0 {
				net = 0
			}
		}
	}
	if net > grossMonthly {
		net = grossMonthly // commitments can only ever make a change worth less
	}
	p.GrossMonthlyUSD, p.NetMonthlyUSD = grossMonthly, net
	p.Reason = fmt.Sprintf(
		"task size %s → %s for %d task(s): p%d CPU %.1f%% and peak memory %.1f%% of reserved put demand at %s; $%.2f/mo gross",
		a.CurrentTier, tier, a.Tasks, int(s.cfg.CPUPercentile*100),
		d.CPUPercentP95, d.MemPercentPeak, d.Target, grossMonthly)

	if mode == modeRecommend {
		a.Suppressions = append(a.Suppressions, Suppression{
			Code:   domain.SuppressModeRecommend,
			Reason: fmt.Sprintf("service is tagged %s=recommend: reporting only", TagKilterMode),
		})
	}
	a.Proposal = p
}

// riskOf classifies a task-size change. Memory is the dimension that kills a
// task — Fargate throttles CPU but OOM-kills memory — so any proposal that
// lowers the memory reservation is at least medium risk.
func riskOf(from, to pricing.FargateConfig) string {
	if to.MemoryMiB < from.MemoryMiB {
		return plan.RiskMedium
	}
	return plan.RiskLow
}

// --- Advisories ------------------------------------------------------------

// advisories computes the two levers that exist on ECS and cannot exist on EKS
// (§7 trap 3), against the tier the service would end up on.
//
// Both are [domain.ActionAdvisory] and both carry a caveat naming the
// precondition Kilter cannot verify, because neither precondition is in
// CloudWatch: Spot needs interruption tolerance, ARM64 needs image and binary
// portability. An advisory's estimate never reaches a plan or a ledger.
func (s *Sizer) advisories(o Observation, a Assessment, base pricing.FargateConfig) []Advisory {
	if base.IsZero() || a.Tasks <= 0 {
		return a.Advisories
	}
	out := a.Advisories
	os := o.TaskDef.RuntimePlatform.OS()
	arch := o.TaskDef.RuntimePlatform.Arch()
	tasks := float64(a.Tasks)

	// ARM/Graviton: ECS runs arm64 Fargate tasks; EKS Fargate cannot
	// ("Can run workloads that require Arm processors: No" [verified]).
	if s.cfg.ARMAdvisory && arch == ArchX86 {
		switch {
		case os != OSLinux:
			// Windows tasks have no ARM64 option at all; saying nothing is
			// right, saying "migrate to Graviton" would be trap 3 in reverse.
		case !platformVersionAtLeast(o.Service.PlatformVersion, 1, 4):
			// Reported as a note rather than an advisory: the lever exists,
			// the service's pinned platform version is what blocks it.
		default:
			x86 := s.cfg.Rates.Cost(base, Platform{Arch: ArchX86, Market: a.Platform.Market})
			arm := s.cfg.Rates.Cost(base, Platform{Arch: ArchARM64, Market: a.Platform.Market})
			est := (x86 - arm) * tasks * pricing.HoursPerMonth
			if est >= s.cfg.MinAdvisoryMonthlyUSD {
				out = append(out, Advisory{
					Code:                AdvisoryARM,
					EstimatedMonthlyUSD: est,
					Caveat: "image and binary portability to arm64 is not observable from metrics: " +
						"every container image in the task definition must have an arm64 manifest, " +
						"and any compiled dependency must have an arm64 build",
					Detail: fmt.Sprintf("runtimePlatform.cpuArchitecture X86_64 → ARM64 at %s for %d task(s): −%.1f%%",
						base, a.Tasks, pct(x86-arm, x86)),
					Proposed: s.proposedSpec(o, base, ChangeARM, a.Tasks).
						WithAttr(AttrArch, string(ArchARM64)),
					Evidence: []domain.Evidence{{
						Metric: "arm-rate-delta",
						Value:  fmt.Sprintf("$%.5f/h → $%.5f/h per task", x86, arm),
						Source: SourceQuantizer,
					}},
				})
			}
		}
	}

	// Fargate Spot: ECS only ("Amazon EKS doesn't support Fargate Spot"
	// [verified]). Not available for Windows tasks.
	if s.cfg.SpotAdvisory && !o.Service.UsesSpot() && os == OSLinux && a.Platform.Market == MarketOnDemand {
		od := s.cfg.Rates.Cost(base, Platform{Arch: arch, Market: MarketOnDemand})
		spot := s.cfg.Rates.Cost(base, Platform{Arch: arch, Market: MarketSpot})
		est := (od - spot) * tasks * pricing.HoursPerMonth
		if est >= s.cfg.MinAdvisoryMonthlyUSD {
			out = append(out, Advisory{
				Code:                AdvisorySpot,
				EstimatedMonthlyUSD: est,
				Caveat: "Fargate Spot tasks are reclaimed with a two-minute warning; interruption tolerance " +
					"is a property of the workload, not of its metrics, and a service using the FARGATE " +
					"launch type must first be moved to a capacity-provider strategy",
				Detail: fmt.Sprintf("FARGATE → FARGATE_SPOT at %s for %d task(s), assuming a %.0f%% discount",
					base, a.Tasks, s.cfg.Rates.withDefaults().SpotDiscount*100),
				Proposed: s.proposedSpec(o, base, ChangeSpot, a.Tasks).
					WithAttr(AttrMarket, string(MarketSpot)),
				Evidence: []domain.Evidence{{
					Metric: "spot-rate-delta",
					Value:  fmt.Sprintf("$%.5f/h → $%.5f/h per task", od, spot),
					Source: SourceQuantizer,
				}},
			})
		}
	}
	return out
}

// --- Specs, evidence, plumbing --------------------------------------------

// currentSpec renders the service's current billed shape.
func (s *Sizer) currentSpec(o Observation, tier pricing.FargateConfig) domain.Spec {
	attrs := map[string]string{
		AttrChange:          ChangeTaskSize,
		AttrCluster:         o.Service.ClusterName(),
		AttrService:         o.Service.ServiceName,
		AttrFamily:          o.TaskDef.Family,
		AttrTaskDefinition:  o.Reserved.ARN,
		AttrDesiredCount:    strconv.Itoa(o.Service.DesiredCount),
		AttrArch:            string(o.TaskDef.RuntimePlatform.Arch()),
		AttrMarket:          string(marketOf(o.Service)),
		AttrPlatformVersion: o.Service.PlatformVersion,
	}
	if o.TaskDef.TaskDefinitionARN != "" {
		attrs[AttrTaskDefinition] = o.TaskDef.TaskDefinitionARN
	}
	if !tier.IsZero() {
		attrs[AttrTaskSize] = tier.String()
		attrs[AttrTaskCPU] = FormatTaskCPU(tier)
		attrs[AttrTaskMemory] = FormatTaskMemory(tier)
	} else {
		attrs[AttrTaskCPU] = o.TaskDef.CPU
		attrs[AttrTaskMemory] = o.TaskDef.Memory
	}
	return domain.Spec{Resources: o.Reserved.Reserved, Attrs: attrs}
}

// proposedSpec renders a target tier as a spec an actuator can register.
func (s *Sizer) proposedSpec(o Observation, tier pricing.FargateConfig, change string, tasks int) domain.Spec {
	return domain.Spec{
		Resources: tier.Resources(),
		Attrs: map[string]string{
			AttrChange:          change,
			AttrCluster:         o.Service.ClusterName(),
			AttrService:         o.Service.ServiceName,
			AttrFamily:          o.TaskDef.Family,
			AttrTaskSize:        tier.String(),
			AttrTaskCPU:         FormatTaskCPU(tier),
			AttrTaskMemory:      FormatTaskMemory(tier),
			AttrDesiredCount:    strconv.Itoa(tasks),
			AttrArch:            string(o.TaskDef.RuntimePlatform.Arch()),
			AttrMarket:          string(marketOf(o.Service)),
			AttrPlatformVersion: o.Service.PlatformVersion,
		},
	}
}

// usageLine renders a service's Fargate spend as a commitment usage line.
//
// ComputeSPRate is left unknown (0) on purpose: Kilter does not have Fargate
// Savings-Plan rates, and pkg/pricing/commit's documented behaviour for an
// unknown rate is to assume the commitment is fully stranded and the usage free
// at the margin. That under-claims savings and can never invent them.
func (s *Sizer) usageLine(ref domain.TargetRef, hourly float64, tasks int) commit.UsageLine {
	return commit.UsageLine{
		ID:       "ecs-fargate/" + ref.Scope + "/" + ref.ID,
		Kind:     commit.KindFargate,
		Region:   s.cfg.Region,
		Unit:     "task-hours",
		Quantity: float64(max(tasks, 0)),
		ODRate:   hourly,
	}
}

// confidence scores how well the service is observed.
func (s *Sizer) confidence(d Demand, o Observation, now time.Time) float64 {
	if d.Samples == 0 {
		return 0
	}
	age := now.Sub(maxTime(o.CPUPercent.Last(), o.MemPercent.Last()))
	if age < 0 {
		age = 0
	}
	c := decision.Compose(
		decision.TermHistoryDepth(d.Samples, 2*s.cfg.MinSamples),
		decision.TermWindowSpan(d.Window, 2*s.cfg.MinWindow),
		decision.TermFreshness(age, s.cfg.MaxSampleAge),
	)
	return c.Score
}

// evidence lists the observable facts behind an assessment, in a fixed order.
// The two percentage lines and the reservation line are always present
// together: a percentage without its denominator is not evidence of anything,
// and printing them apart is how the inversion bug survives review.
func (s *Sizer) evidence(o Observation, a Assessment, d Demand) []domain.Evidence {
	ev := []domain.Evidence{{
		Metric: "reserved",
		Value:  d.Reserved.String(),
		Source: SourceTaskDef,
		At:     o.Window.End,
	}}
	if !a.CurrentTier.IsZero() {
		ev = append(ev, domain.Evidence{
			Metric: "task-size",
			Value:  a.CurrentTier.String(),
			Source: SourceQuantizer,
		})
	}
	if d.Samples > 0 {
		win := d.Window.Round(time.Minute).String()
		ev = append(ev,
			domain.Evidence{
				Metric:  "cpu-p" + strconv.Itoa(int(s.cfg.CPUPercentile*100)) + "-percent-of-reserved",
				Value:   fmt.Sprintf("%.2f%%", d.CPUPercentP95),
				Window:  win,
				Samples: d.Samples,
				Source:  SourceCloudWatch,
				At:      o.CPUPercent.Last(),
			},
			domain.Evidence{
				Metric:  "mem-peak-percent-of-reserved",
				Value:   fmt.Sprintf("%.2f%%", d.MemPercentPeak),
				Window:  win,
				Samples: d.Samples,
				Source:  SourceCloudWatch,
				At:      o.MemPercent.Last(),
			},
			domain.Evidence{
				Metric: "demand-absolute",
				Value:  d.CPU.String() + " (percent × reserved)",
				Window: win,
				Source: SourceCloudWatch,
			},
		)
	}
	ev = append(ev, domain.Evidence{
		Metric: "desired-count",
		Value:  strconv.Itoa(a.Tasks),
		Source: SourceDescribe,
	})
	return ev
}

// platformOf reports the platform a service is currently billed under.
func platformOf(o Observation) Platform {
	return Platform{Arch: o.TaskDef.RuntimePlatform.Arch(), Market: marketOf(o.Service)}
}

// marketOf reports the market a service currently buys capacity in. A mixed
// strategy reads as spot only when spot is the whole of it: pricing a
// half-spot service as if it were all spot would understate its bill and
// therefore its savings.
func marketOf(s ServiceRecord) Market {
	if len(s.CapacityProviderStrategy) == 0 {
		return MarketOnDemand
	}
	for _, c := range s.CapacityProviderStrategy {
		if c.CapacityProvider != CapacityProviderSpot {
			return MarketOnDemand
		}
	}
	return MarketSpot
}

func statusOr(s string) string {
	if s == "" {
		return StatusComplete
	}
	return s
}

// ceilInt64 rounds a non-negative float up to an int64, saturating instead of
// wrapping. A garbage percentage must stay absurdly large and fail to fit any
// tier, never wrap negative and read as "needs nothing".
func ceilInt64(f float64) int64 {
	if !(f > 0) || math.IsNaN(f) {
		return 0
	}
	if f >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(math.Ceil(f))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func pct(delta, of float64) float64 {
	if !(of > 0) {
		return 0
	}
	return delta / of * 100
}
