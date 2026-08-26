package ec2

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// DefaultProvider is the pricing-catalog provider key this domain reads.
const DefaultProvider = "aws"

// Config tunes the sizer. Every default is deliberately more conservative
// than the Kubernetes domain's, because the evidence is worse: 5-minute
// averages instead of 1-minute samples, and usually no memory signal at all
// (§3.3 "recommendations use p95/p99 ... and headroom defaults are higher than
// in the K8s domain").
type Config struct {
	// Provider keys the pricing catalog.
	Provider string
	// CPUPercentile is the percentile of observed CPU that sizing targets.
	CPUPercentile float64
	// CPUHeadroom multiplies that percentile. Demand is
	// max(p95 × CPUHeadroom, observed peak) — the max, so a proposal can
	// never land below something we actually saw.
	CPUHeadroom float64
	// MemHeadroom multiplies observed peak memory, when memory is observable.
	MemHeadroom float64
	// CoarseResolutionHeadroom inflates demand when CloudWatch published at
	// 300 s. It is a stated safety margin, not a derived truth: a 5-minute
	// average can hide an arbitrarily higher 1-minute peak, and no multiplier
	// recovers it. See [ResolutionNote].
	CoarseResolutionHeadroom float64
	// MinWindow is the shortest observation span a proposal may rest on.
	MinWindow time.Duration
	// MinSampleCoverage is the smallest acceptable ratio of delivered
	// datapoints to the datapoints the window and period imply.
	MinSampleCoverage float64
	// MinConfidence gates proposals. Confidence is earned, never assumed.
	MinConfidence float64
	// GravitonAdvisory enables architecture-migration advisories (§4.5).
	GravitonAdvisory bool
	// AllowFixedToBurstable permits proposing a T-family target for a
	// non-burstable instance. Off by default and deliberately deferred: §4.6
	// requires the behavior-class detector to confirm steady-low (not
	// bursty-high) first, and this unit does not run it.
	AllowFixedToBurstable bool
}

// DefaultConfig returns the shipped defaults.
func DefaultConfig() Config {
	return Config{
		Provider:                 DefaultProvider,
		CPUPercentile:            0.95,
		CPUHeadroom:              1.30,
		MemHeadroom:              1.25,
		CoarseResolutionHeadroom: 1.25,
		MinWindow:                7 * 24 * time.Hour,
		MinSampleCoverage:        0.50,
		MinConfidence:            0.65,
		GravitonAdvisory:         true,
	}
}

func (c Config) validate() error {
	switch {
	case c.CPUPercentile <= 0 || c.CPUPercentile > 1:
		return fmt.Errorf("ec2: CPUPercentile must be in (0,1], got %v", c.CPUPercentile)
	case c.CPUHeadroom < 1:
		return fmt.Errorf("ec2: CPUHeadroom must be >= 1, got %v", c.CPUHeadroom)
	case c.MemHeadroom < 1:
		return fmt.Errorf("ec2: MemHeadroom must be >= 1, got %v", c.MemHeadroom)
	case c.CoarseResolutionHeadroom < 1:
		return fmt.Errorf("ec2: CoarseResolutionHeadroom must be >= 1, got %v", c.CoarseResolutionHeadroom)
	case c.MinWindow <= 0:
		return fmt.Errorf("ec2: MinWindow must be positive, got %v", c.MinWindow)
	case c.MinSampleCoverage < 0 || c.MinSampleCoverage > 1:
		return fmt.Errorf("ec2: MinSampleCoverage must be in [0,1], got %v", c.MinSampleCoverage)
	case c.MinConfidence < 0 || c.MinConfidence > 1:
		return fmt.Errorf("ec2: MinConfidence must be in [0,1], got %v", c.MinConfidence)
	}
	return nil
}

// ResolutionNote states the rule this package applies to metric resolution,
// verbatim, in every report it produces. A peak that cannot be seen is not a
// peak that may be assumed away: it is named, priced into a headroom, and
// charged against confidence.
func ResolutionNote(periodSeconds int32, headroom float64) string {
	if periodSeconds <= PeriodDetailedSeconds {
		return fmt.Sprintf("%d-second datapoints (detailed monitoring): sub-minute peaks are still averaged, "+
			"but the observed peak is a %d-second peak and is used as-is", periodSeconds, periodSeconds)
	}
	return fmt.Sprintf("%d-second datapoints (basic monitoring): CloudWatch publishes one value per %d seconds, "+
		"so a burst shorter than that is averaged away and CANNOT be recovered from this data — requesting the "+
		"Maximum statistic does not help, because there is only one datapoint per window. Demand is inflated by "+
		"%.0f%% to compensate and confidence is reduced; enable detailed monitoring for %d-second datapoints",
		periodSeconds, periodSeconds, (headroom-1)*100, PeriodDetailedSeconds)
}

// Observation is everything the sizer read, and everything it could not.
type Observation struct {
	Window          Window  `json:"window"`
	PeriodSeconds   int32   `json:"periodSeconds"`
	Samples         int     `json:"samples"`
	ExpectedSamples int     `json:"expectedSamples"`
	Coverage        float64 `json:"coverage"`
	Partial         bool    `json:"partial,omitempty"`

	// MemoryBlind is the state this package exists for: no memory series, so
	// no memory decision. See the package doc.
	MemoryBlind bool     `json:"memoryBlind"`
	Blind       []string `json:"blind,omitempty"`

	MeanCPUPercent float64 `json:"meanCPUPercent"`
	P95CPUPercent  float64 `json:"p95CPUPercent"`
	PeakCPUPercent float64 `json:"peakCPUPercent"`
	PeakCPUMilli   int64   `json:"peakCPUMilli"`
	DemandCPUMilli int64   `json:"demandCPUMilli"`

	PeakMemoryPercent float64 `json:"peakMemoryPercent,omitempty"`
	PeakMemoryBytes   int64   `json:"peakMemoryBytes,omitempty"`
	MemoryFloorBytes  int64   `json:"memoryFloorBytes"`

	ResolutionHeadroom float64 `json:"resolutionHeadroom"`
	ResolutionNote     string  `json:"resolutionNote"`

	Burst BurstState `json:"burst"`
}

// ConfidenceFactor is one earned component of a confidence score.
type ConfidenceFactor struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Earned float64 `json:"earned"` // 0..1
	Why    string  `json:"why"`
}

// Confidence is a score built from nothing. It starts at zero and adds only
// what the evidence earns, so a missing signal cannot be mistaken for a
// present one; [Config.MinConfidence] is the bar it has to clear.
type Confidence struct {
	Score   float64            `json:"score"`
	Factors []ConfidenceFactor `json:"factors,omitempty"`
}

func (c *Confidence) add(name string, weight, earned float64, why string) {
	if earned < 0 {
		earned = 0
	}
	if earned > 1 {
		earned = 1
	}
	c.Factors = append(c.Factors, ConfidenceFactor{Name: name, Weight: weight, Earned: earned, Why: why})
	c.Score += weight * earned
}

// Confidence weights. They sum to 1.
const (
	weightCoverage   = 0.30
	weightWindow     = 0.20
	weightMemory     = 0.20
	weightResolution = 0.15
	weightBurst      = 0.15
)

// Proposal is a cost-reducing change this domain would make if it were allowed
// to act — which, in this unit, it is not. Action and Risk are labels for the
// later executing unit (§6 U7); nothing here dispatches on them.
type Proposal struct {
	Spec              Spec        `json:"spec"`
	InstanceType      string      `json:"instanceType"`
	ProposedHourlyUSD float64     `json:"proposedHourlyUSD"`
	Action            ActionClass `json:"action"`
	Risk              string      `json:"risk"`
	Confidence        float64     `json:"confidence"`

	// GrossSavingsMonthlyUSD is the on-demand list-price delta — the fantasy,
	// carried so a UI can show it beside the fact.
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	// NetSavingsMonthlyUSD is the bill delta through the commitment waterfall
	// (§4.4). It is the only number that may be presented as a saving, and it
	// is always ≤ gross.
	NetSavingsMonthlyUSD float64 `json:"netSavingsMonthlyUSD"`
	// Conservative marks a net computed under the no-SP-rate fallback: the
	// real saving is this or better, never worse.
	Conservative bool `json:"conservative,omitempty"`

	Reason string `json:"reason"`
}

// Advisory is a finding that is reported and never actuated. Architecture
// migration is the archetype: binary, AMI and container-image portability
// cannot be observed from metrics, so no amount of price evidence makes it
// safe to apply (§4.5). An advisory must always carry a Caveat —
// [Report.Validate] rejects a report where one does not.
type Advisory struct {
	Code                   string    `json:"code"`
	Message                string    `json:"message"`
	Caveat                 string    `json:"caveat"`
	ProposedType           string    `json:"proposedType,omitempty"`
	GrossSavingsMonthlyUSD float64   `json:"grossSavingsMonthlyUSD,omitempty"`
	NetSavingsMonthlyUSD   float64   `json:"netSavingsMonthlyUSD,omitempty"`
	ValidFrom              time.Time `json:"validFrom,omitempty"`
}

// Actuatable is false for every advisory, always. It is a method rather than a
// field so no serialized form and no future struct literal can claim
// otherwise.
func (Advisory) Actuatable() bool { return false }

// Action reports the advisory action class.
func (Advisory) Action() ActionClass { return ActionAdvisory }

// Assessment is the sizer's verdict on exactly one instance. Every observed
// instance produces exactly one, whether or not anything is proposed; an
// assessment with no proposal always states why.
type Assessment struct {
	Target             TargetRef `json:"target"`
	Current            Spec      `json:"current"`
	CurrentHourlyUSD   float64   `json:"currentHourlyUSD"`
	CurrentMonthlyUSD  float64   `json:"currentMonthlyUSD"`
	EffectiveHourlyUSD float64   `json:"effectiveHourlyUSD"`

	Proposal     *Proposal     `json:"proposal,omitempty"`
	Suppressions []Suppression `json:"suppressions,omitempty"`
	Advisories   []Advisory    `json:"advisories,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"`
	Observation  Observation   `json:"observation"`
	Confidence   Confidence    `json:"confidence"`
}

// Refused reports whether the sizer declined to propose a change.
func (a Assessment) Refused() bool { return a.Proposal == nil }

// Suppressed reports whether a given reason code fired.
func (a Assessment) Suppressed(code string) bool {
	for _, s := range a.Suppressions {
		if s.Code == code {
			return true
		}
	}
	return false
}

// SuppressionFor returns the suppression with the given code.
func (a Assessment) SuppressionFor(code string) (Suppression, bool) {
	for _, s := range a.Suppressions {
		if s.Code == code {
			return s, true
		}
	}
	return Suppression{}, false
}

// Excluded reports whether this instance belongs to another owner — a
// Kubernetes cluster, a fleet manager such as an Auto Scaling group or an AWS
// Batch compute environment, or an operator who tagged it off.
func (a Assessment) Excluded() bool {
	return a.Suppressed(ReasonK8sTagged) || a.Suppressed(ReasonModeOff) ||
		a.Suppressed(ReasonASGManaged)
}

// AdvisoryFor returns the advisory with the given code.
func (a Assessment) AdvisoryFor(code string) (Advisory, bool) {
	for _, ad := range a.Advisories {
		if ad.Code == code {
			return ad, true
		}
	}
	return Advisory{}, false
}

func (a *Assessment) suppress(code, reason string) {
	a.Suppressions = append(a.Suppressions, Suppression{Code: code, Reason: reason})
}

func (a *Assessment) evidence(e Evidence) { a.Evidence = append(a.Evidence, e) }

// Sizer turns a snapshot into a report. It is pure: no I/O, no clock, no
// mutable state. The same snapshot and inventory always produce the same
// report, byte for byte.
type Sizer struct {
	cat *pricing.Catalog
	cfg Config
}

// NewSizer builds a sizer over a pricing catalog.
func NewSizer(cat *pricing.Catalog, cfg Config) (*Sizer, error) {
	if cat == nil {
		return nil, fmt.Errorf("ec2: sizer needs a pricing catalog")
	}
	if cfg.Provider == "" {
		cfg.Provider = DefaultProvider
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Sizer{cat: cat, cfg: cfg}, nil
}

// Config returns the sizer's configuration.
func (s *Sizer) Config() Config { return s.cfg }

// Assess evaluates every target in a snapshot against the account's commitment
// position and returns one report. inv may be nil — an account with no
// commitments, where gross and net savings coincide.
//
// Commitment netting is account-wide by construction: the before/after usage
// handed to the waterfall contains every priced instance in the snapshot, not
// only the one being changed, because Compute Savings Plans absorb usage
// account-wide and a partial view overstates savings (§4.4 ex.3).
func (s *Sizer) Assess(now time.Time, snap *Snapshot, inv *commit.Inventory) *Report {
	rep := &Report{
		Domain:      Domain,
		GeneratedAt: now,
		Config:      s.cfg,
	}
	if snap == nil {
		rep.Warnings = []string{"no snapshot: nothing was observed"}
		return rep
	}
	rep.Scope = snap.Scope
	rep.Region = snap.Region
	rep.Window = snap.Window
	rep.Stale = snap.Stale
	rep.Warnings = append(rep.Warnings, snap.Warnings...)

	active := inv.Active(now)
	base, priced := s.accountUsage(snap)
	if len(base.Lines) < len(snap.Targets) {
		rep.Warnings = append(rep.Warnings,
			"some instances are absent from the pricing catalog and are excluded from the commitment waterfall; "+
				"their assessments are refusals")
	}

	for _, t := range snap.Targets {
		rep.Assessments = append(rep.Assessments, s.assess(now, snap, t, active, base, priced))
	}
	sort.Slice(rep.Assessments, func(i, j int) bool {
		return rep.Assessments[i].Target.ID < rep.Assessments[j].Target.ID
	})
	// AWS Batch insights (U15b). Report-scope, because they describe a compute
	// environment's configuration rather than any one instance — and because
	// U15a excludes Batch-managed instances, which may carry no advisories.
	rep.Advisories = s.batchInsights(snap)
	rep.Warnings = sortWarnings(rep.Warnings)
	rep.Totals = rep.computeTotals()
	return rep
}

// accountUsage builds the account-wide before-usage and an index from instance
// ID to its line position. Instances the catalog cannot price are omitted:
// pricing them at zero would understate the account's commitment absorption
// and overstate every other instance's savings.
func (s *Sizer) accountUsage(snap *Snapshot) (commit.Usage, map[string]int) {
	var u commit.Usage
	idx := map[string]int{}
	for _, t := range snap.Targets {
		it, ok := s.cat.Lookup(s.cfg.Provider, t.Instance.InstanceType)
		if !ok {
			continue
		}
		region := snap.Region
		if region == "" {
			region = t.Instance.Region()
		}
		idx[t.Ref.ID] = len(u.Lines)
		u.Lines = append(u.Lines, commit.UsageLine{
			ID:           t.Ref.ID,
			Kind:         commit.KindEC2,
			Region:       region,
			AZ:           t.Instance.AvailabilityZone,
			InstanceType: t.Instance.InstanceType,
			Platform:     t.Instance.Platform,
			Tenancy:      t.Instance.Tenancy,
			Unit:         "Hrs",
			Quantity:     1,
			ODRate:       it.HourlyUSD,
		})
	}
	return u, idx
}

// withSwap returns the account usage with one instance's line replaced by the
// proposed shape — the "after" side of the bill delta.
func withSwap(base commit.Usage, idx map[string]int, id string, to pricing.InstanceType) commit.Usage {
	after := commit.Usage{Lines: append([]commit.UsageLine(nil), base.Lines...)}
	i, ok := idx[id]
	if !ok {
		return after
	}
	after.Lines[i].InstanceType = to.Name
	after.Lines[i].ODRate = to.HourlyUSD
	return after
}

func (s *Sizer) assess(now time.Time, snap *Snapshot, t Target, inv *commit.Inventory,
	base commit.Usage, idx map[string]int) Assessment {

	in := t.Instance
	a := Assessment{
		Target: t.Ref,
		Current: Spec{
			Attrs: map[string]string{
				AttrInstanceType: in.InstanceType,
				AttrArch:         in.Architecture,
				AttrPlatform:     in.Platform,
				AttrTenancy:      in.Tenancy,
				AttrAZ:           in.AvailabilityZone,
			},
		},
	}
	if in.CreditMode != "" {
		a.Current.Attrs[AttrCreditMode] = in.CreditMode
	}
	a.evidence(Evidence{
		Metric: "instance-type", Value: in.InstanceType, Source: "describe-instances", At: in.LaunchTime,
	})

	// (1) Ownership. A cluster node is the k8s-nodes pipeline's target: sizing
	// it here would ignore pod requests, PDBs and the binpacker entirely, and
	// double-count its savings against a plan that already owns it (§3.3).
	if cluster, ok := in.K8sCluster(); ok {
		a.suppress(ReasonK8sTagged, fmt.Sprintf(
			"carries a Kubernetes cluster tag (%s): this instance is a node of cluster %q and belongs to the "+
				"k8s-nodes pipeline, which sizes it against pod requests and eviction guardrails this domain "+
				"cannot see", TagK8sClusterPrefix+"*", cluster))
		return a
	}
	if in.ModeOff() {
		a.suppress(ReasonModeOff, fmt.Sprintf(
			"tagged %s=off: the operator has opted this instance out, mirroring the Kubernetes annotation "+
				"guardrail", TagKilterMode))
		return a
	}
	// Fleet ownership (§3.5, §7 trap 14). An ASG member is not the resizable
	// unit — its launch template is — and the fleet manager may be AWS Batch,
	// which "creates and manages ... Amazon EC2 Auto Scaling Groups" and
	// "assumes full control of the compute resources in a managed compute
	// environment and can terminate instances ... at any time" [verified].
	// A Batch compute environment with minvCpus > 0 keeps instances alive with
	// an empty queue "even if the compute environment is DISABLED", so it
	// presents as a long-lived, fully-covered, near-zero-CPU instance that
	// clears every evidence gate below and reads as unambiguously oversized.
	// Shrinking that floor is wrong in kind, not in degree: the floor is a
	// deliberately bought job start latency, and this domain measures no
	// latency at all. The insight AWS Batch operators actually want is priced
	// against the compute environment, not the instance — see batchenrich.go.
	if group, ok := in.AutoScalingGroup(); ok {
		a.suppress(ReasonASGManaged, fmt.Sprintf(
			"carries %s=%q: this instance was launched by an Auto Scaling group, so its shape comes from a launch "+
				"template and a per-instance resize is reverted by the next scale-out. If that group is an AWS "+
				"Batch managed compute environment, resizing is worse than useless — AWS assumes full control of "+
				"those instances and warns that modifying them by hand causes INVALID compute environments and "+
				"unexpected costs, and a minvCpus>0 idle floor looks exactly like an oversized instance from here. "+
				"Sizing the launch template is a template-level job this domain does not do (FINDINGS.md §5); for "+
				"Batch, the compute environment's minvCpus floor is reported as an advisory instead",
			TagASGName, group))
		return a
	}

	// (2) Can we price it? An instance the catalog does not know has no
	// current cost, so no delta it could produce would be a number.
	cur, ok := s.cat.Lookup(s.cfg.Provider, in.InstanceType)
	if !ok {
		a.suppress(ReasonUnknownInstanceType, fmt.Sprintf(
			"instance type %q is not in the pricing catalog for provider %q: without a price there is no bill "+
				"delta to claim; run `kilter pricing sync-aws` or supply a catalog that contains it",
			in.InstanceType, s.cfg.Provider))
		return a
	}
	a.Current.Resources = model.Resources{MilliCPU: cur.MilliCPU, MemoryBytes: cur.MemoryBytes}
	a.Current.Attrs[AttrBurstable] = fmt.Sprintf("%t", cur.Burstable || IsBurstable(in.InstanceType))
	a.CurrentHourlyUSD = cur.HourlyUSD
	a.CurrentMonthlyUSD = cur.HourlyUSD * HoursPerMonth
	a.EffectiveHourlyUSD = cur.HourlyUSD

	obs := Observation{Window: snap.Window, Blind: t.Blind}
	cpu, hasCPU := t.SeriesFor(MetricCPUUtilization)

	// (3) Evidence quality, in the order that makes each failure legible.
	// Incompleteness is checked first and separately from absence: a series
	// CloudWatch failed to deliver looks exactly like a series with no data,
	// and only one of those two is evidence.
	for _, ser := range t.Series {
		if ser.Partial {
			obs.Partial = true
		}
	}
	if obs.Partial {
		if hasCPU {
			obs.PeriodSeconds = cpu.PeriodSeconds
			obs.Samples = len(cpu.Points)
		}
		a.Observation = obs
		a.suppress(ReasonPartialMetrics, fmt.Sprintf(
			"CloudWatch did not deliver a complete series (status %q): a partial window is not a window, because "+
				"the missing datapoints are exactly where a peak would hide", seriesStatus(t)))
		return a
	}
	if !hasCPU || len(cpu.Points) == 0 {
		a.Observation = obs
		a.suppress(ReasonNoMetrics, fmt.Sprintf(
			"no %s datapoints were returned for this instance: there is nothing to size against", MetricCPUUtilization))
		return a
	}
	obs.PeriodSeconds = cpu.PeriodSeconds
	obs.Samples = len(cpu.Points)
	obs.Window = Window{Start: cpu.Points[0].At, End: cpu.Points[len(cpu.Points)-1].At}
	obs.ResolutionHeadroom = s.resolutionHeadroom(cpu.PeriodSeconds)
	obs.ResolutionNote = ResolutionNote(cpu.PeriodSeconds, obs.ResolutionHeadroom)

	if obs.Window.Duration() < s.cfg.MinWindow {
		a.Observation = obs
		a.suppress(ReasonInsufficientWindow, fmt.Sprintf(
			"observed window is %s, shorter than the %s minimum: a window that does not span a business cycle "+
				"has not seen the peak it would be sized under",
			obs.Window.String(), s.cfg.MinWindow.Round(time.Hour)))
		return a
	}

	if obs.PeriodSeconds > 0 {
		obs.ExpectedSamples = int(obs.Window.Duration().Seconds() / float64(obs.PeriodSeconds))
	}
	obs.Coverage = 1
	if obs.ExpectedSamples > 0 {
		obs.Coverage = math.Min(1, float64(obs.Samples)/float64(obs.ExpectedSamples))
	}
	if obs.Coverage < s.cfg.MinSampleCoverage {
		a.Observation = obs
		a.suppress(ReasonInsufficientSamples, fmt.Sprintf(
			"only %d of the ~%d datapoints the window implies were delivered (%.0f%% coverage, minimum %.0f%%): "+
				"the gaps are unobserved time, not idle time",
			obs.Samples, obs.ExpectedSamples, obs.Coverage*100, s.cfg.MinSampleCoverage*100))
		return a
	}

	// (4) CPU demand. The max of (percentile × headroom) and the observed peak
	// means a proposal can never sit below something we actually measured —
	// the invariant FuzzSizerNeverUndersizes pins.
	obs.MeanCPUPercent, _ = cpu.Mean()
	obs.P95CPUPercent, _ = cpu.Percentile(s.cfg.CPUPercentile)
	obs.PeakCPUPercent, _ = cpu.Max()
	obs.PeakCPUMilli = pctToMilli(obs.PeakCPUPercent, cur.MilliCPU)
	demand := math.Max(obs.P95CPUPercent*s.cfg.CPUHeadroom, obs.PeakCPUPercent) * obs.ResolutionHeadroom
	obs.DemandCPUMilli = pctToMilli(demand, cur.MilliCPU)
	if obs.DemandCPUMilli < obs.PeakCPUMilli {
		obs.DemandCPUMilli = obs.PeakCPUMilli
	}

	a.evidence(Evidence{
		Metric: "cpu-p95", Value: fmtPct(obs.P95CPUPercent), Window: obs.Window.String(),
		Samples: obs.Samples, Source: "cloudwatch", At: obs.Window.End,
	})
	a.evidence(Evidence{
		Metric: "cpu-peak", Value: fmtPct(obs.PeakCPUPercent), Window: obs.Window.String(),
		Samples: obs.Samples, Source: "cloudwatch", At: obs.Window.End,
	})
	a.evidence(Evidence{
		Metric: "metric-resolution", Value: fmt.Sprintf("%ds", obs.PeriodSeconds),
		Window: obs.Window.String(), Samples: obs.Samples, Source: "cloudwatch",
	})

	// (5) Memory, or the documented absence of it.
	mem, hasMem := t.SeriesFor(MetricMemUsedPercent)
	obs.MemoryBlind = !hasMem || len(mem.Points) == 0
	if obs.MemoryBlind {
		obs.MemoryFloorBytes = cur.MemoryBytes
		a.evidence(Evidence{
			Metric: "mem-blind", Value: "no memory series",
			Window: obs.Window.String(), Source: "cloudwatch",
		})
	} else {
		obs.PeakMemoryPercent, _ = mem.Max()
		obs.PeakMemoryBytes = pctToBytes(obs.PeakMemoryPercent, cur.MemoryBytes)
		obs.MemoryFloorBytes = pctToBytes(obs.PeakMemoryPercent*s.cfg.MemHeadroom, cur.MemoryBytes)
		if obs.MemoryFloorBytes < obs.PeakMemoryBytes {
			obs.MemoryFloorBytes = obs.PeakMemoryBytes
		}
		a.evidence(Evidence{
			Metric: "mem-peak", Value: fmtPct(obs.PeakMemoryPercent), Window: obs.Window.String(),
			Samples: len(mem.Points), Source: "cwagent", At: obs.Window.End,
		})
	}

	// (6) Burstable analytics.
	obs.Burst = AnalyzeBurst(in, t, int(cur.MilliCPU/1000), cur.HourlyUSD, obs.Window.Duration())
	a.Observation = obs
	a.EffectiveHourlyUSD = obs.Burst.EffectiveHourlyUSD
	if obs.Burst.Class != BurstNotApplicable {
		a.evidence(Evidence{
			Metric: "burst-class", Value: string(obs.Burst.Class), Window: obs.Window.String(),
			Source: "cloudwatch", At: obs.Window.End,
		})
	}
	switch obs.Burst.Class {
	case BurstUnknown:
		a.suppress(ReasonBurstEvidenceMissing, obs.Burst.Reason+
			"; §4.6 requires credit evidence in both directions before a T-family instance is resized")
		return a
	case BurstThrottled:
		a.evidence(Evidence{
			Metric: "credit-balance-min", Value: fmt.Sprintf("%.1f credits", obs.Burst.BalanceMin),
			Window: obs.Window.String(), Source: "cloudwatch", At: obs.Window.End,
		})
		a.suppress(ReasonBurstCreditDepleted, obs.Burst.Reason+
			"; sizing down from a throttled instance's CPU lowers its baseline and throttles it harder")
		a.Advisories = append(a.Advisories, Advisory{
			Code: AdvisoryBurstThrottle,
			Message: fmt.Sprintf(
				"%s is throttled at its %.0f%% baseline in standard mode; the fix is more capacity or unlimited "+
					"mode, not less capacity. Cheapest same-architecture fixed shape that clears the observed "+
					"ceiling: %s", in.InstanceType, obs.Burst.Spec.BaselinePercent(),
				s.describeFixedAlternative(in, cur, obs)),
			Caveat: "advisory only: the observed CPU is a throttling ceiling, so the true demand of this " +
				"workload is unknown and no saving is claimed. Enable unlimited mode or move to a fixed-performance " +
				"family to measure it.",
		})
		return a
	case BurstSurplus:
		a.evidence(Evidence{
			Metric: "surplus-credits-usd", Value: fmtUSD(obs.Burst.SurplusHourlyUSD) + "/h",
			Window: obs.Window.String(), Source: "cloudwatch", At: obs.Window.End,
		})
		a.Advisories = append(a.Advisories, Advisory{
			Code: AdvisoryBurstSurplus,
			Message: fmt.Sprintf("%s: %s. Sticker %s/h + surplus %s/h = %s/h effective (%s/mo); §4.6's breakeven "+
				"against a fixed-performance family is where this stops being the cheap option",
				in.InstanceType, obs.Burst.Reason, fmtUSD(cur.HourlyUSD), fmtUSD(obs.Burst.SurplusHourlyUSD),
				fmtUSD(obs.Burst.EffectiveHourlyUSD), fmtUSD(obs.Burst.EffectiveHourlyUSD*HoursPerMonth)),
			Caveat: "advisory only: surplus credit charges are billed on demand, and whether a Savings Plan " +
				"absorbs them is not modeled here, so the effective cost above is NOT netted through the " +
				"commitment waterfall and no saving is claimed from it.",
		})
	}

	// (7) Candidate search. Burstable targets are eligible only when the
	// current instance is already burstable and its credits are healthy —
	// otherwise the sticker-price mirage (§7 trap 5) picks the trap.
	// BurstSurplus keeps burstable candidates in the search on purpose: an
	// instance already paying for surplus must be *seen* to be refused a
	// smaller burstable shape, rather than have the trap quietly filtered out
	// of the candidate set with no reason attached.
	allowBurstable := obs.Burst.Class == BurstHealthy || obs.Burst.Class == BurstSurplus ||
		(s.cfg.AllowFixedToBurstable && obs.Burst.Class == BurstNotApplicable)

	best, foundBest := s.cheapest(in.Architecture, obs.DemandCPUMilli, obs.MemoryFloorBytes, allowBurstable)

	// The memory-blind counterfactual: what would we have chosen with no
	// memory floor at all? When that is cheaper than what the floor allows,
	// the floor is the thing that stopped us, and that is a refusal to state —
	// not a quieter recommendation to make.
	if obs.MemoryBlind {
		if blind, ok := s.cheapest(in.Architecture, obs.DemandCPUMilli, 0, allowBurstable); ok {
			if blind.Name != in.InstanceType && (!foundBest || blind.HourlyUSD < best.HourlyUSD-1e-12) {
				a.suppress(ReasonMemoryBlind, fmt.Sprintf(
					"CPU evidence alone would have proposed %s (%s/h, %s of memory), but this instance runs no "+
						"CloudWatch agent so there is NO memory metric for it (§7 trap 4). Memory is floored at the "+
						"current %s and only same-or-more-memory moves are eligible; install the CloudWatch agent "+
						"(mem_used_percent) to make this decidable",
					blind.Name, fmtUSD(blind.HourlyUSD), gib(blind.MemoryBytes), gib(cur.MemoryBytes)))
			}
		}
	}

	// (8) Is the current shape even big enough? A sizer that can only shrink
	// is a downsizer. Undersizing is reported, never proposed: it costs money,
	// and spending it is not this unit's call.
	if cur.MilliCPU < obs.DemandCPUMilli || cur.MemoryBytes < obs.MemoryFloorBytes {
		a.Advisories = append(a.Advisories, Advisory{
			Code: AdvisoryUndersized,
			Message: fmt.Sprintf(
				"observed demand (%d mCPU, %s memory floor) exceeds the current %s (%d mCPU, %s): this instance "+
					"looks undersized, not oversized",
				obs.DemandCPUMilli, gib(obs.MemoryFloorBytes), in.InstanceType, cur.MilliCPU, gib(cur.MemoryBytes)),
			Caveat: "advisory only: growing an instance raises the bill, and this unit proposes only " +
				"cost-reducing changes. No saving is claimed.",
		})
		a.suppress(ReasonUndersized,
			"no downsize proposed: observed demand already exceeds this instance's shape")
		a.Advisories = append(a.Advisories, s.gravitonAdvisory(in, cur, obs, inv, base, idx)...)
		a.Confidence = s.confidence(obs)
		return a
	}

	if !foundBest || best.Name == in.InstanceType || best.HourlyUSD >= cur.HourlyUSD-1e-12 {
		// When the memory-blind floor is what stopped a cheaper choice, that
		// refusal has already been stated and is the whole reason; adding
		// "nothing was cheaper" beside it would bury the finding.
		if !a.Suppressed(ReasonMemoryBlind) {
			a.suppress(ReasonNoCheaperCandidate, fmt.Sprintf(
				"%s is already the cheapest catalog shape that clears %d mCPU of demand and a %s memory floor",
				in.InstanceType, obs.DemandCPUMilli, gib(obs.MemoryFloorBytes)))
		}
		a.Advisories = append(a.Advisories, s.gravitonAdvisory(in, cur, obs, inv, base, idx)...)
		a.Confidence = s.confidence(obs)
		return a
	}

	// (9) Within-family downsizes of a surplus-charging T instance are the
	// sticker mirage in miniature: the credits, not the size, are the cost.
	if obs.Burst.Class == BurstSurplus && IsBurstable(best.Name) {
		a.suppress(ReasonBurstSurplusCharged, fmt.Sprintf(
			"%s is already paying %s/h in surplus credits; moving to a smaller burstable shape (%s) lowers the "+
				"baseline and buys more surplus, not less", in.InstanceType, fmtUSD(obs.Burst.SurplusHourlyUSD),
			best.Name))
		a.Confidence = s.confidence(obs)
		return a
	}

	// (10) Confidence. Earned, then gated.
	a.Confidence = s.confidence(obs)
	if a.Confidence.Score < s.cfg.MinConfidence {
		a.suppress(ReasonLowConfidence, fmt.Sprintf(
			"confidence %.2f is below the %.2f floor (%s): the evidence does not support acting, only watching",
			a.Confidence.Score, s.cfg.MinConfidence, weakestFactor(a.Confidence)))
		a.Advisories = append(a.Advisories, s.gravitonAdvisory(in, cur, obs, inv, base, idx)...)
		return a
	}

	// (11) The bill, not the list price.
	as := inv.NetSavings(base, withSwap(base, idx, t.Ref.ID, best))
	a.evidence(Evidence{
		Metric: "net-savings", Value: fmtUSD(as.NetMonthlyUSD) + "/mo",
		Window: obs.Window.String(), Source: "commitment-ledger", At: now,
	})
	if as.Suppressed {
		a.Suppressions = append(a.Suppressions, Suppression{
			Code:      as.ReasonCode,
			Reason:    fmt.Sprintf("moving %s to %s: %s", in.InstanceType, best.Name, as.Reason),
			ValidFrom: as.ValidFrom,
		})
		a.Advisories = append(a.Advisories, s.gravitonAdvisory(in, cur, obs, inv, base, idx)...)
		return a
	}

	risk, riskWhy := RiskMedium, "stop-start resize is downtime and must run in a change window behind approval"
	if in.InstanceStore {
		risk = RiskHigh
		riskWhy = "this instance has instance-store volumes: stopping it destroys their data, so a stop-start " +
			"resize must be refused by any executor"
	}
	a.Proposal = &Proposal{
		Spec: Spec{
			Resources: model.Resources{MilliCPU: best.MilliCPU, MemoryBytes: best.MemoryBytes},
			Attrs: map[string]string{
				AttrInstanceType: best.Name,
				AttrArch:         best.Arch,
				AttrBurstable:    fmt.Sprintf("%t", best.Burstable),
			},
		},
		InstanceType:           best.Name,
		ProposedHourlyUSD:      best.HourlyUSD,
		Action:                 ActionStopStart,
		Risk:                   risk,
		Confidence:             a.Confidence.Score,
		GrossSavingsMonthlyUSD: as.GrossMonthlyUSD,
		NetSavingsMonthlyUSD:   as.ClaimableMonthlyUSD(),
		Conservative:           as.Conservative,
		Reason: fmt.Sprintf(
			"%s → %s: %s p%.0f CPU with %.0f%% headroom needs %d mCPU (peak %s), memory floor %s; %s/h → %s/h, "+
				"net %s/mo after commitments. %s",
			in.InstanceType, best.Name, fmtPct(obs.P95CPUPercent), s.cfg.CPUPercentile*100,
			(s.cfg.CPUHeadroom-1)*100, obs.DemandCPUMilli, fmtPct(obs.PeakCPUPercent),
			gib(obs.MemoryFloorBytes), fmtUSD(cur.HourlyUSD), fmtUSD(best.HourlyUSD),
			fmtUSD(as.ClaimableMonthlyUSD()), riskWhy),
	}
	a.Advisories = append(a.Advisories, s.gravitonAdvisory(in, cur, obs, inv, base, idx)...)
	return a
}

// resolutionHeadroom is the stated inflation applied to coarse data.
func (s *Sizer) resolutionHeadroom(period int32) float64 {
	if period > PeriodDetailedSeconds {
		return s.cfg.CoarseResolutionHeadroom
	}
	return 1
}

// cheapest finds the cheapest catalog shape that clears the demand floors,
// restricted to the current architecture. Architecture changes are advisory
// (§4.5) and never appear here. Candidates arrive price-sorted with
// (provider, name) tie-breaking, so the winner does not depend on catalog
// order.
func (s *Sizer) cheapest(arch string, needCPUMilli, minMemBytes int64, allowBurstable bool) (pricing.InstanceType, bool) {
	for _, it := range s.cat.Candidates(s.cfg.Provider, arch) {
		if it.MilliCPU < needCPUMilli || it.MemoryBytes < minMemBytes {
			continue
		}
		if !allowBurstable && (it.Burstable || IsBurstable(it.Name)) {
			continue
		}
		return it, true
	}
	return pricing.InstanceType{}, false
}

// describeFixedAlternative names the cheapest fixed-performance shape that
// clears a throttled instance's observed ceiling, for the advisory prose.
func (s *Sizer) describeFixedAlternative(in Instance, cur pricing.InstanceType, obs Observation) string {
	alt, ok := s.cheapest(in.Architecture, obs.DemandCPUMilli, cur.MemoryBytes, false)
	if !ok {
		return "none in the catalog"
	}
	return fmt.Sprintf("%s at %s/h (vs %s/h)", alt.Name, fmtUSD(alt.HourlyUSD), fmtUSD(cur.HourlyUSD))
}

// gravitonAdvisory flags an ARM shape that would be cheaper (§4.5). It is
// always advisory: binary, AMI and container-image portability is not
// observable from any metric, so no price evidence can make it applicable. The
// $ delta is netted through the commitment waterfall, because family-scoped
// commitments do not follow an instance to a different family.
func (s *Sizer) gravitonAdvisory(in Instance, cur pricing.InstanceType, obs Observation,
	inv *commit.Inventory, base commit.Usage, idx map[string]int) []Advisory {

	if !s.cfg.GravitonAdvisory || in.Architecture != "amd64" {
		return nil
	}
	memFloor := obs.MemoryFloorBytes
	if memFloor < cur.MemoryBytes && obs.MemoryBlind {
		memFloor = cur.MemoryBytes
	}
	arm, ok := s.cheapest("arm64", obs.DemandCPUMilli, memFloor, false)
	if !ok || arm.HourlyUSD >= cur.HourlyUSD-1e-12 {
		return nil
	}
	as := inv.NetSavings(base, withSwap(base, idx, in.ID, arm))
	caveat := "advisory only, never auto-applied: architecture portability cannot be observed from metrics. " +
		"Every binary, AMI, container image and vendored dependency on this instance must have an arm64 build, " +
		"and nothing in CloudWatch can tell you whether it does."
	if as.ClaimableMonthlyUSD() <= 0 {
		caveat += fmt.Sprintf(" The list-price delta of %s/mo is NOT a saving here: %s",
			fmtUSD(as.GrossMonthlyUSD), as.Reason)
	} else {
		caveat += " Family-scoped commitments (m5 RIs, EC2 Instance Savings Plans) do not follow an instance " +
			"across families; the net below already prices that."
	}
	return []Advisory{{
		Code: AdvisoryGraviton,
		Message: fmt.Sprintf("%s → %s (arm64): %s/h → %s/h at the same or better shape (%d mCPU, %s)",
			in.InstanceType, arm.Name, fmtUSD(cur.HourlyUSD), fmtUSD(arm.HourlyUSD), arm.MilliCPU,
			gib(arm.MemoryBytes)),
		Caveat:                 caveat,
		ProposedType:           arm.Name,
		GrossSavingsMonthlyUSD: as.GrossMonthlyUSD,
		NetSavingsMonthlyUSD:   as.ClaimableMonthlyUSD(),
		ValidFrom:              as.ValidFrom,
	}}
}

// confidence scores the evidence. Nothing is assumed: each factor adds only
// what it can demonstrate.
func (s *Sizer) confidence(obs Observation) Confidence {
	var c Confidence
	c.add("sample-coverage", weightCoverage, obs.Coverage,
		fmt.Sprintf("%d of ~%d expected datapoints", obs.Samples, obs.ExpectedSamples))

	windowEarned := 0.0
	if s.cfg.MinWindow > 0 {
		windowEarned = obs.Window.Duration().Seconds() / s.cfg.MinWindow.Seconds()
	}
	c.add("window", weightWindow, windowEarned,
		fmt.Sprintf("observed %s against a %s minimum", obs.Window.String(), s.cfg.MinWindow.Round(time.Hour)))

	memEarned, memWhy := 1.0, "memory observed via the CloudWatch agent"
	if obs.MemoryBlind {
		memEarned, memWhy = 0, "memory-blind: no CloudWatch agent, so no memory metric exists"
	}
	c.add("memory-signal", weightMemory, memEarned, memWhy)

	resEarned, resWhy := 1.0, fmt.Sprintf("%d-second datapoints", obs.PeriodSeconds)
	if obs.PeriodSeconds > PeriodDetailedSeconds {
		resEarned = 0.4
		resWhy = fmt.Sprintf("%d-second datapoints hide shorter peaks", obs.PeriodSeconds)
	}
	c.add("metric-resolution", weightResolution, resEarned, resWhy)

	burstEarned, burstWhy := 1.0, "not a credit-based instance type"
	switch obs.Burst.Class {
	case BurstUnknown:
		burstEarned, burstWhy = 0, "burstable with no usable credit evidence"
	case BurstThrottled:
		burstEarned, burstWhy = 0, "credit-depleted: observed CPU is a throttling ceiling"
	case BurstHealthy, BurstSurplus:
		burstWhy = "credit metrics present and classified"
	}
	c.add("burst-evidence", weightBurst, burstEarned, burstWhy)
	return c
}

// weakestFactor names the factor that cost the most confidence, so a
// low-confidence refusal says what would fix it.
func weakestFactor(c Confidence) string {
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

func seriesStatus(t Target) string {
	for _, s := range t.Series {
		if s.Partial && s.Status != "" && s.Status != StatusComplete {
			return s.Status
		}
	}
	return StatusPartialData
}

// pctToMilli converts a CloudWatch CPU percentage into millicores of a shape.
func pctToMilli(pct float64, totalMilli int64) int64 {
	if pct <= 0 || totalMilli <= 0 {
		return 0
	}
	return int64(math.Ceil(pct / 100 * float64(totalMilli)))
}

// pctToBytes converts a memory percentage into bytes of a shape.
func pctToBytes(pct float64, totalBytes int64) int64 {
	if pct <= 0 || totalBytes <= 0 {
		return 0
	}
	return int64(math.Ceil(pct / 100 * float64(totalBytes)))
}

func gib(b int64) string { return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30)) }
