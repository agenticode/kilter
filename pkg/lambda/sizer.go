package lambda

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/confidence"
	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// eps guards float comparisons on money and milliseconds.
const eps = 1e-12

// Config tunes the sizer. Every default is chosen for the evidence this domain
// actually has: per-invocation records rather than 5-minute averages (so the
// sample gates do the heavy lifting) over a log window that may be a slice of
// the retention period (so the window gate is not decorative).
type Config struct {
	// Scope and Region label the report; Region also keys commitment usage.
	Scope  string
	Region string
	// Rates prices GB-seconds and requests.
	Rates Rates
	// MemHeadroom multiplies observed max-memory-used to produce the floor.
	MemHeadroom float64
	// MemoryStepMB is the granularity a proposed floor is rounded UP to.
	// Lambda accepts 1 MB steps; recommending 137 MB is technically valid and
	// operationally absurd, so the default is coarser than the platform.
	MemoryStepMB int64
	// CeilingRatio decides when max-memory-used counts as "at the ceiling" and
	// therefore possibly truncated.
	CeilingRatio float64
	// MinWindow is the shortest evidence span a proposal may rest on.
	MinWindow time.Duration
	// MinInvocations is the fewest invocations that can characterize anything.
	MinInvocations float64
	// MinSamplesPerPoint is the fewest WARM invocations a memory setting needs
	// before its measured duration may carry a cost claim.
	MinSamplesPerPoint int
	// MaxColdShare is the largest fraction of cold starts a warm-duration cost
	// model may be built on.
	MaxColdShare float64
	// MinConfidence gates proposals. Confidence is earned, never assumed.
	MinConfidence float64
	// ARMAdvisory enables the Graviton finding (§4.5, advisory forever).
	ARMAdvisory bool
}

// DefaultConfig returns the shipped defaults.
func DefaultConfig() Config {
	return Config{
		Rates:              DefaultRates(),
		MemHeadroom:        1.25,
		MemoryStepMB:       64,
		CeilingRatio:       0.98,
		MinWindow:          24 * time.Hour,
		MinInvocations:     1000,
		MinSamplesPerPoint: 200,
		MaxColdShare:       0.20,
		MinConfidence:      0.65,
		ARMAdvisory:        true,
	}
}

func (c Config) validate() error {
	switch {
	case c.MemHeadroom < 1:
		return fmt.Errorf("lambda: MemHeadroom must be >= 1, got %v", c.MemHeadroom)
	case c.MemoryStepMB < 1 || c.MemoryStepMB > MaxMemoryMB:
		return fmt.Errorf("lambda: MemoryStepMB must be in [1,%d], got %v", MaxMemoryMB, c.MemoryStepMB)
	case c.CeilingRatio <= 0 || c.CeilingRatio > 1:
		return fmt.Errorf("lambda: CeilingRatio must be in (0,1], got %v", c.CeilingRatio)
	case c.MinWindow <= 0:
		return fmt.Errorf("lambda: MinWindow must be positive, got %v", c.MinWindow)
	case c.MinInvocations < 0:
		return fmt.Errorf("lambda: MinInvocations must be non-negative, got %v", c.MinInvocations)
	case c.MinSamplesPerPoint < 1:
		return fmt.Errorf("lambda: MinSamplesPerPoint must be positive, got %v", c.MinSamplesPerPoint)
	case c.MaxColdShare < 0 || c.MaxColdShare > 1:
		return fmt.Errorf("lambda: MaxColdShare must be in [0,1], got %v", c.MaxColdShare)
	case c.MinConfidence < 0 || c.MinConfidence > 1:
		return fmt.Errorf("lambda: MinConfidence must be in [0,1], got %v", c.MinConfidence)
	}
	return c.Rates.validate()
}

// ConfidenceFactor is one earned component of a confidence score.
//
// Aliased rather than redeclared: the shape and its JSON are shared with
// pkg/ec2, and an alias means a report cannot drift between them.
type ConfidenceFactor = confidence.Factor

// Confidence is a score built from nothing. It starts at zero and adds only
// what the evidence earns, so a missing signal cannot be mistaken for a present
// one; [Config.MinConfidence] is the bar it has to clear.
//
// This domain adds its factors with [confidence.Confidence.Add], which treats
// a non-finite earned value as no evidence: see pkg/confidence/FINDINGS.md §3.
type Confidence = confidence.Confidence

// Confidence weights. They sum to 1. measured-points carries the largest
// weight on purpose: it is the factor this whole package is about.
const (
	weightMeasuredPoints = 0.30
	weightReportCoverage = 0.25
	weightWarmShare      = 0.15
	weightWindow         = 0.15
	weightHeadroom       = 0.15
)

// Proposal is a memory change this domain would make if it were allowed to act
// — which it is not, ever. Action is always [domain.ActionAdvisory] and
// [Report.Validate] rejects anything else.
//
// The invariant that makes a Proposal trustworthy: MeasuredBilledMS and
// MeasuredSamples describe a measurement taken AT MemoryMB. There is no code
// path that fills them from any other setting.
type Proposal struct {
	MemoryMB int64              `json:"memoryMB"`
	Spec     domain.Spec        `json:"spec"`
	Action   domain.ActionClass `json:"action"`
	Risk     string             `json:"risk"`

	Confidence float64 `json:"confidence"`
	// MeasuredBilledMS is the mean billed duration measured AT MemoryMB.
	MeasuredBilledMS float64 `json:"measuredBilledMS"`
	// MeasuredSamples is how many warm invocations that mean rests on.
	MeasuredSamples int `json:"measuredSamples"`

	ProposedHourlyUSD float64 `json:"proposedHourlyUSD"`
	// GrossSavingsMonthlyUSD is the on-demand list-price delta between two
	// MEASURED operating points.
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	// NetSavingsMonthlyUSD is the delta after the commitment waterfall (§4.4);
	// Compute Savings Plans cover Lambda duration account-wide, so a GB-second
	// reduction can be worth less than its list price. Never above gross.
	NetSavingsMonthlyUSD float64 `json:"netSavingsMonthlyUSD"`
	// Conservative marks a net computed under the no-SP-rate fallback: the
	// real saving is this or better, never worse.
	Conservative bool `json:"conservative,omitempty"`

	Reason string `json:"reason"`
}

// Assessment is the sizer's verdict on exactly one function. Every observed
// function produces exactly one, whether or not anything is proposed; an
// assessment with no proposal always carries the reason why.
type Assessment struct {
	Target   domain.TargetRef `json:"target"`
	Function Function         `json:"function"`
	Current  domain.Spec      `json:"current"`

	// CurrentHourlyUSD / CurrentMonthlyUSD are only populated when CostKnown:
	// the current bill is itself a MEASURED quantity, and a function whose
	// current setting was never measured has no current cost to report.
	CurrentHourlyUSD  float64 `json:"currentHourlyUSD"`
	CurrentMonthlyUSD float64 `json:"currentMonthlyUSD"`
	CostKnown         bool    `json:"costKnown"`

	Proposal *Proposal `json:"proposal,omitempty"`
	// CandidateMemoryMB is the alternative setting this assessment actually
	// contemplated, whether or not it survived. It is what makes a refusal
	// legible to a UI: "we looked at 512 MB and here is why we will not say it
	// is cheaper". Zero when nothing specific was on the table.
	CandidateMemoryMB int64 `json:"candidateMemoryMB,omitempty"`

	Suppressions []Suppression     `json:"suppressions,omitempty"`
	Advisories   []Advisory        `json:"advisories,omitempty"`
	Evidence     []domain.Evidence `json:"evidence,omitempty"`
	Observation  Observation       `json:"observation"`
	Confidence   Confidence        `json:"confidence"`
}

// Refused reports whether the sizer declined to propose a change.
func (a Assessment) Refused() bool { return a.Proposal == nil }

// Suppressed reports whether a given reason code fired.
func (a Assessment) Suppressed(code string) bool {
	_, ok := a.SuppressionFor(code)
	return ok
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

// AdvisoryFor returns the advisory with the given code.
func (a Assessment) AdvisoryFor(code string) (Advisory, bool) {
	for _, ad := range a.Advisories {
		if ad.Code == code {
			return ad, true
		}
	}
	return Advisory{}, false
}

// Excluded reports whether the operator opted this function out.
func (a Assessment) Excluded() bool { return a.Suppressed(ReasonModeOff) }

func (a *Assessment) suppress(code, reason string) {
	a.Suppressions = append(a.Suppressions, Suppression{Code: code, Reason: reason})
}

func (a *Assessment) evidence(e domain.Evidence) { a.Evidence = append(a.Evidence, e) }

// Sizer turns a snapshot into a report. It is pure: no I/O, no clock, no
// mutable state. The same snapshot and ledger always produce the same report,
// byte for byte.
type Sizer struct{ cfg Config }

// NewSizer builds a sizer.
func NewSizer(cfg Config) (*Sizer, error) {
	if cfg.Rates == (Rates{}) {
		cfg.Rates = DefaultRates()
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Sizer{cfg: cfg}, nil
}

// Config returns the sizer's configuration.
func (s *Sizer) Config() Config { return s.cfg }

// Assess evaluates every target in a snapshot and returns one report. ledger
// may be nil — an account with no known commitments, where gross and net
// savings coincide because nothing can be stranded.
func (s *Sizer) Assess(now time.Time, snap *Snapshot, ledger domain.Netter) *Report {
	rep := &Report{Domain: Kind, GeneratedAt: now, Config: s.cfg}
	if snap == nil {
		rep.Warnings = []string{"no snapshot: nothing was observed"}
		return rep
	}
	rep.Scope, rep.Region, rep.Window, rep.Stale = snap.Scope, snap.Region, snap.Window, snap.Stale
	if rep.Region == "" {
		rep.Region = s.cfg.Region
	}
	rep.Warnings = append(rep.Warnings, snap.Warnings...)

	for _, t := range snap.Targets {
		rep.Assessments = append(rep.Assessments, s.assess(now, snap, t, ledger))
	}
	sort.Slice(rep.Assessments, func(i, j int) bool {
		return rep.Assessments[i].Target.ID < rep.Assessments[j].Target.ID
	})
	rep.Warnings = sortWarnings(rep.Warnings)
	rep.Totals = rep.computeTotals()
	return rep
}

func (s *Sizer) assess(now time.Time, snap *Snapshot, t Target, ledger domain.Netter) Assessment {
	f := t.Function
	arch := f.Arch()
	a := Assessment{Target: t.Ref, Function: f, Current: SpecFor(f, f.MemoryMB, arch)}
	a.evidence(domain.Evidence{
		Metric: "memory-size", Value: fmtMB(f.MemoryMB), Source: SourceListFunctions, At: f.LastModified,
	})

	// (1) Ownership. The operator's opt-out wins over everything.
	if f.ModeOff() {
		a.suppress(ReasonModeOff, fmt.Sprintf(
			"tagged %s=off: the operator has opted this function out, mirroring the Kubernetes annotation "+
				"guardrail", TagKilterMode))
		return a
	}

	// (2) Can we even name the current shape? Memory is the only knob, so a
	// function whose memory the collector could not read has no current bill
	// and no proposable change.
	if f.MemoryMB < MinMemoryMB || f.MemoryMB > MaxMemoryMB {
		a.suppress(ReasonUnknownConfiguration, fmt.Sprintf(
			"configured memory reads as %d MB, outside Lambda's %d–%d MB range: without the current setting "+
				"there is no bill to compare against and no valid setting to propose",
			f.MemoryMB, MinMemoryMB, MaxMemoryMB))
		return a
	}

	obs := observe(t, snap.Window, s.cfg)
	a.Observation = obs
	if obs.Dropped > 0 {
		a.evidence(domain.Evidence{
			Metric: "report-lines-dropped", Value: fmt.Sprintf("%d", obs.Dropped),
			Samples: obs.Records, Source: SourceReportLine,
		})
	}

	// The current bill is a measured quantity too. Populate it as soon as
	// there is a measurement at the current setting, so a refusal can still
	// tell the operator what this function costs today.
	cur, hasCur := obs.Current()
	if hasCur && cur.Warm > 0 && obs.InvocationsPerHour > 0 {
		a.CurrentHourlyUSD = s.cfg.Rates.HourlyUSD(obs.InvocationsPerHour, arch, cur.MemoryMB, cur.MeanBilledMS)
		a.CurrentMonthlyUSD = MonthlyUSD(a.CurrentHourlyUSD)
		a.CostKnown = true
	}

	// (3) Provisioned concurrency is a different billing model: a per-GB-hour
	// charge for kept-warm environments plus a discounted duration rate. Every
	// number this package computes is on-demand GB-seconds, so applying it here
	// would price a bill this function does not have.
	if obs.ProvisionedConcurrency > 0 {
		a.suppress(ReasonProvisionedConcurrency, fmt.Sprintf(
			"%d provisioned concurrent execution(s) are configured: provisioned concurrency bills a separate "+
				"per-GB-hour charge for the kept-warm environments plus a discounted duration rate, so this "+
				"package's on-demand GB-second arithmetic does not describe this function's bill",
			obs.ProvisionedConcurrency))
		return a
	}

	// (4) Evidence. Max-memory-used exists in exactly one place — the REPORT
	// line — so no REPORT lines means no floor and no measured duration.
	if obs.Records == 0 {
		a.suppress(ReasonNoReportEvidence, s.noEvidenceReason(t, obs))
		return a
	}
	a.evidence(domain.Evidence{
		Metric: "report-records", Value: fmt.Sprintf("%d warm / %d cold", obs.Warm, obs.Cold),
		Window: obs.Window.String(), Samples: obs.Records, Source: SourceReportLine, At: obs.Window.End,
	})
	a.evidence(domain.Evidence{
		Metric: "max-memory-used", Value: fmtMB(obs.MaxMemoryUsedMB),
		Window: obs.Window.String(), Samples: obs.Records, Source: SourceReportLine, At: obs.Window.End,
	})
	a.evidence(domain.Evidence{
		Metric: "memory-floor", Value: fmtMB(obs.MemoryFloorMB),
		Window: obs.Window.String(), Samples: obs.Records, Source: SourceReportLine, At: obs.Window.End,
	})
	a.evidence(domain.Evidence{
		Metric: "invocations", Value: fmt.Sprintf("%.0f over %s", obs.Invocations, obs.Window.String()),
		Window: obs.Window.String(), Source: obs.InvocationSource, At: obs.Window.End,
	})
	a.evidence(domain.Evidence{
		Metric: "memory-points", Value: memoryPointsValue(obs.Points),
		Window: obs.Window.String(), Samples: len(obs.Points), Source: SourceReportLine, At: obs.Window.End,
	})
	if obs.Cold > 0 {
		a.evidence(domain.Evidence{
			Metric: "cold-start-share", Value: fmt.Sprintf("%.1f%%", obs.ColdShare*100),
			Window: obs.Window.String(), Samples: obs.Cold, Source: SourceReportLine, At: obs.Window.End,
		})
	}
	if obs.MemoryFloorMB > f.MemoryMB {
		a.Advisories = append(a.Advisories, Advisory{
			Code: AdvisoryUnderMemory,
			Message: fmt.Sprintf(
				"observed max memory used %s needs a %s floor at %.0f%% headroom, above the configured %s: "+
					"this function looks under-provisioned, not over-provisioned",
				fmtMB(obs.MaxMemoryUsedMB), fmtMB(obs.MemoryFloorMB), (s.cfg.MemHeadroom-1)*100, fmtMB(f.MemoryMB)),
			ProposedMemoryMB: obs.MemoryFloorMB,
			Caveat: "advisory only: raising memory raises the GB-second rate, and whether it raises the BILL " +
				"depends on how duration responds — which is unmeasured at that setting. No saving and no cost " +
				"increase is claimed.",
		})
	}

	// (5) Window. A slice of a log group has not seen this function's peak.
	if obs.Window.Duration() < s.cfg.MinWindow {
		a.suppress(ReasonInsufficientWindow, fmt.Sprintf(
			"the surviving REPORT lines span %s, shorter than the %s minimum: a window this short has not seen "+
				"the memory peak this function would be sized under, and the invocation mix in it is not the "+
				"steady state", obs.Window.String(), s.cfg.MinWindow.Round(time.Minute)))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (6) Volume. A handful of samples is an anecdote.
	if obs.Invocations < s.cfg.MinInvocations {
		a.suppress(ReasonInsufficientInvocations, fmt.Sprintf(
			"%.0f invocation(s) observed over %s (source: %s), below the %.0f minimum: too few to characterize "+
				"either the memory peak or the duration distribution",
			obs.Invocations, obs.Window.String(), obs.InvocationSource, s.cfg.MinInvocations))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (7) Cold starts. Init time is a separate phase under separate billing
	// rules, and it moves with memory too — in a direction this data cannot
	// show. When it dominates, the warm mean is not the bill.
	if obs.ColdShare > s.cfg.MaxColdShare {
		a.suppress(ReasonColdStartDominated, fmt.Sprintf(
			"%.1f%% of invocations paid initialization (mean init %s), above the %.0f%% ceiling: warm duration "+
				"statistics do not describe this function's bill, and a memory change moves init time as well "+
				"as handler time — neither of which is measured at any other setting",
			obs.ColdShare*100, fmtMS(meanInit(obs.Points)), s.cfg.MaxColdShare*100))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (8) Truncation. A max-memory-used sitting at the configured ceiling is
	// evidence of RISK, not of a good fit: the platform cannot report a number
	// above the limit, so the true peak is unknown and at least this large.
	if obs.AtCeiling {
		a.Advisories = append(a.Advisories, Advisory{
			Code: AdvisoryMemoryTruncated,
			Message: fmt.Sprintf(
				"max memory used reached %s of the configured %s: the platform cannot report a value above the "+
					"limit, so this measurement is a LOWER BOUND on demand, not the demand",
				fmtMB(obs.MaxMemoryUsedMB), fmtMB(f.MemoryMB)),
			ProposedMemoryMB: obs.MemoryFloorMB,
			Caveat: "advisory only: raise memory to at least " + fmtMB(obs.MemoryFloorMB) + " to find out what " +
				"this function actually needs. Until then every memory number for it is a floor, and no saving " +
				"or cost increase can be computed from a truncated measurement.",
		})
		a.CandidateMemoryMB = obs.MemoryFloorMB
		a.suppress(ReasonMemoryAtCeiling, fmt.Sprintf(
			"max memory used (%s) sits at the configured %s: that is a function which may have been TRUNCATED "+
				"by its limit, not one that fits. Sizing down from a truncated peak is an out-of-memory error "+
				"with a savings estimate attached", fmtMB(obs.MaxMemoryUsedMB), fmtMB(f.MemoryMB)))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (9) THE RULE. A cost claim needs a measured duration at the setting it
	// claims about. Fewer than two adequately-measured settings means there is
	// nothing to compare, and the cost effect of any change is UNKNOWN.
	usable := obs.UsablePoints(s.cfg.MinSamplesPerPoint)
	if len(usable) < 2 {
		tuning := s.powerTuningAdvisory(f, obs, a)
		a.CandidateMemoryMB = tuning.ProposedMemoryMB
		a.Advisories = append(a.Advisories, tuning)
		a.suppress(ReasonSingleMemoryPoint, fmt.Sprintf(
			"only %d memory setting(s) carry at least %d warm invocations (%s). Lambda bills memory × duration "+
				"and allocates CPU in proportion to memory, so lowering memory can raise the bill; the duration "+
				"at any other setting is a property of this function's code and appears in no metric. The memory "+
				"floor is %s and the risk is stated — THE COST EFFECT IS UNKNOWN and no saving is claimed",
			len(usable), s.cfg.MinSamplesPerPoint, memoryPointsValue(obs.Points), fmtMB(obs.MemoryFloorMB)))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (10) The current bill has to be measured too, or every delta from it is
	// a guess dressed as a baseline.
	if !hasCur || cur.Warm < s.cfg.MinSamplesPerPoint {
		a.suppress(ReasonNoMeasurementAtCurrent, fmt.Sprintf(
			"the function runs at %s, but that setting carries only %d warm invocation(s) of the %d required "+
				"(measured settings: %s): the CURRENT bill is unmeasured, so every delta from it would be a guess",
			fmtMB(f.MemoryMB), warmAt(obs, f.MemoryMB), s.cfg.MinSamplesPerPoint, memoryPointsValue(obs.Points)))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}
	curCost := s.cfg.Rates.HourlyUSD(obs.InvocationsPerHour, arch, cur.MemoryMB, cur.MeanBilledMS)
	a.CurrentHourlyUSD, a.CurrentMonthlyUSD, a.CostKnown = curCost, MonthlyUSD(curCost), true

	// (11) Compare MEASURED points only. Nothing is interpolated, and no point
	// is scored on a duration measured somewhere else.
	best, trap, blockedByFloor := s.compare(usable, cur, obs, arch, curCost)

	if trap != nil {
		trapCost := s.cfg.Rates.HourlyUSD(obs.InvocationsPerHour, arch, trap.MemoryMB, trap.MeanBilledMS)
		a.Advisories = append(a.Advisories, Advisory{
			Code: AdvisoryCostIncrease,
			Message: fmt.Sprintf(
				"%s → %s was MEASURED and costs MORE: billed duration rose %s → %s (%d and %d warm samples), so "+
					"the bill rises %s/mo. Lambda allocates CPU in proportion to memory; this function is one of "+
					"the ones that pays for it",
				fmtMB(cur.MemoryMB), fmtMB(trap.MemoryMB), fmtMS(cur.MeanBilledMS), fmtMS(trap.MeanBilledMS),
				cur.Warm, trap.Warm, fmtUSD(MonthlyUSD(trapCost-curCost))),
			ProposedMemoryMB:    trap.MemoryMB,
			RateDeltaMonthlyUSD: MonthlyUSD(curCost - trapCost), // negative: a cost increase
			Caveat: "advisory only, and deliberately NOT a saving: this is the naive recommendation " +
				"(\"max memory used is low, lower the memory\") measured and found to cost more.",
		})
	}

	if best == nil {
		switch {
		case trap != nil:
			a.CandidateMemoryMB = trap.MemoryMB
			a.suppress(ReasonLowerMemoryCostsMore, fmt.Sprintf(
				"the only measured setting below %s is %s, and it measured MORE expensive (billed duration "+
					"%s vs %s): lowering memory here raises the bill, so the downsize is refused and the "+
					"increase is reported instead",
				fmtMB(cur.MemoryMB), fmtMB(trap.MemoryMB), fmtMS(trap.MeanBilledMS), fmtMS(cur.MeanBilledMS)))
		case blockedByFloor != nil:
			a.CandidateMemoryMB = blockedByFloor.MemoryMB
			a.suppress(ReasonBelowMemoryFloor, fmt.Sprintf(
				"%s measured cheaper than the current %s, but it is below the %s memory floor that %s of "+
					"observed max-memory-used requires: a cheaper setting the function cannot fit in is not a "+
					"saving, it is an out-of-memory error",
				fmtMB(blockedByFloor.MemoryMB), fmtMB(cur.MemoryMB), fmtMB(obs.MemoryFloorMB),
				fmtMB(obs.MaxMemoryUsedMB)))
		default:
			a.suppress(ReasonNoCheaperMeasurement, fmt.Sprintf(
				"%s is the cheapest of the %d measured setting(s) (%s) that clears the %s memory floor",
				fmtMB(cur.MemoryMB), len(usable), memoryPointsValue(usable), fmtMB(obs.MemoryFloorMB)))
		}
		a.Confidence = s.confidence(obs, usable)
		a.Advisories = append(a.Advisories, s.powerTuningAdvisory(f, obs, a))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (12) Confidence. Earned, then gated.
	a.CandidateMemoryMB = best.MemoryMB
	a.Confidence = s.confidence(obs, usable)
	if a.Confidence.Score < s.cfg.MinConfidence {
		a.suppress(ReasonLowConfidence, fmt.Sprintf(
			"confidence %.2f is below the %.2f floor (%s): the evidence does not support acting, only watching",
			a.Confidence.Score, s.cfg.MinConfidence, weakestFactor(a.Confidence)))
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	// (13) The bill, not the list price. Compute Savings Plans absorb Lambda
	// duration account-wide, so a GB-second reduction can be worth less — or
	// nothing — than its list-price delta suggests (§4.4, §7 trap 1).
	bestCost := s.cfg.Rates.HourlyUSD(obs.InvocationsPerHour, arch, best.MemoryMB, best.MeanBilledMS)
	as := netSavings(ledger,
		s.usage(t.Ref, arch, obs.InvocationsPerHour, obs.InvocationsPerHour*cur.GBSecondsPerInvocation()),
		s.usage(t.Ref, arch, obs.InvocationsPerHour, obs.InvocationsPerHour*best.GBSecondsPerInvocation()))
	a.evidence(domain.Evidence{
		Metric: "net-savings", Value: fmtUSD(as.NetMonthlyUSD) + "/mo",
		Window: obs.Window.String(), Source: SourceLedger, At: now,
	})
	if as.Suppressed {
		a.Suppressions = append(a.Suppressions, Suppression{
			Code:      as.ReasonCode,
			Reason:    fmt.Sprintf("moving %s to %s: %s", fmtMB(cur.MemoryMB), fmtMB(best.MemoryMB), as.Reason),
			ValidFrom: as.ValidFrom,
		})
		a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
		return a
	}

	risk, riskWhy := RiskMedium, "less memory is less CPU: latency rises, and this measurement is the average "+
		"of a past window, not a guarantee about future traffic"
	if best.MemoryMB > cur.MemoryMB {
		risk, riskWhy = RiskLow, "more memory is more CPU: this change was measured to be both cheaper and "+
			"faster, and it raises the out-of-memory margin rather than lowering it"
	}
	a.Proposal = &Proposal{
		MemoryMB:               best.MemoryMB,
		Spec:                   SpecFor(f, best.MemoryMB, arch),
		Action:                 domain.ActionAdvisory,
		Risk:                   risk,
		Confidence:             a.Confidence.Score,
		MeasuredBilledMS:       best.MeanBilledMS,
		MeasuredSamples:        best.Warm,
		ProposedHourlyUSD:      bestCost,
		GrossSavingsMonthlyUSD: as.GrossMonthlyUSD,
		NetSavingsMonthlyUSD:   as.ClaimableMonthlyUSD(),
		Conservative:           as.Conservative,
		Reason: fmt.Sprintf(
			"%s → %s: MEASURED at both settings — %s over %d warm invocations vs %s over %d — so the %s/mo is a "+
				"comparison of two observed bills, not a projection. Memory floor %s (max used %s). %s",
			fmtMB(cur.MemoryMB), fmtMB(best.MemoryMB), fmtMS(cur.MeanBilledMS), cur.Warm,
			fmtMS(best.MeanBilledMS), best.Warm, fmtUSD(as.ClaimableMonthlyUSD()),
			fmtMB(obs.MemoryFloorMB), fmtMB(obs.MaxMemoryUsedMB), riskWhy),
	}
	a.Advisories = append(a.Advisories, s.powerTuningAdvisory(f, obs, a))
	a.Advisories = append(a.Advisories, s.armAdvisory(f, obs, a)...)
	return a
}

// compare picks the cheapest MEASURED setting that clears the memory floor and
// is not itself a truncated measurement. It also reports the two ways a
// candidate can lose, because both are findings:
//
//   - trap: a LOWER memory setting that measured MORE expensive — the
//     GB-second trap this unit exists to refuse.
//   - blockedByFloor: a cheaper setting the function cannot fit in.
func (s *Sizer) compare(usable []MemoryPoint, cur MemoryPoint, obs Observation, arch string,
	curCost float64) (best, trap, blockedByFloor *MemoryPoint) {

	var bestCost, trapCost float64
	for i := range usable {
		p := usable[i]
		cost := s.cfg.Rates.HourlyUSD(obs.InvocationsPerHour, arch, p.MemoryMB, p.MeanBilledMS)
		if p.MemoryMB < cur.MemoryMB && cost > curCost+eps {
			// The naive recommendation, measured and refuted. Keep the most
			// expensive one: it is the loudest counter-example.
			if trap == nil || cost > trapCost {
				q := usable[i]
				trap, trapCost = &q, cost
			}
		}
		if p.MemoryMB == cur.MemoryMB || p.AtCeiling || cost >= curCost-eps {
			continue
		}
		if p.MemoryMB < obs.MemoryFloorMB {
			if blockedByFloor == nil || p.MemoryMB > blockedByFloor.MemoryMB {
				q := usable[i]
				blockedByFloor = &q
			}
			continue
		}
		if best == nil || cost < bestCost {
			q := usable[i]
			best, bestCost = &q, cost
		}
	}
	if best != nil {
		// A cheaper measured setting that clears the floor wins outright; the
		// blocked candidate is then noise, not a finding.
		blockedByFloor = nil
	}
	return best, trap, blockedByFloor
}

// usage renders this function's hourly usage for the commitment waterfall.
func (s *Sizer) usage(ref domain.TargetRef, arch string, invPerHour, gbSecPerHour float64) []commit.UsageLine {
	region := s.cfg.Region
	if region == "" {
		region = ref.Scope
	}
	return usageLines(ref, region, arch, s.cfg.Rates, invPerHour, gbSecPerHour)
}

// netSavings runs the commitment waterfall, or the no-commitment identity when
// no ledger was supplied: with nothing committed, nothing can be stranded and
// the bill delta IS the list-price delta.
func netSavings(ledger domain.Netter, before, after []commit.UsageLine) commit.Assessment {
	if ledger != nil {
		return ledger.Net(before, after)
	}
	b := commit.Usage{Lines: before}.OnDemandHourlyUSD()
	af := commit.Usage{Lines: after}.OnDemandHourlyUSD()
	d := zeroIfNotFinite(b - af)
	return commit.Assessment{
		NetHourlyUSD: d, NetMonthlyUSD: d * HoursPerMonth,
		GrossHourlyUSD: d, GrossMonthlyUSD: d * HoursPerMonth,
	}
}

// noEvidenceReason explains an empty evidence set in terms of what the
// collector actually saw, so "no data" is never indistinguishable from "we
// dropped it all".
func (s *Sizer) noEvidenceReason(t Target, obs Observation) string {
	if obs.Dropped > 0 {
		return fmt.Sprintf(
			"no REPORT line survived parsing: %d line(s) were dropped (%s). Max memory used is published in the "+
				"REPORT record and nowhere else, so without one there is no memory floor and no measured duration",
			obs.Dropped, dropSummary(t.Drops))
	}
	return fmt.Sprintf(
		"no REPORT lines were delivered for %s%s. Max memory used is published in the REPORT record and nowhere "+
			"else — CloudWatch's Duration metric cannot substitute, because it says nothing about memory — so "+
			"there is no floor to compute and no duration measured at any known memory setting",
		LogGroupPrefix, t.Function.DisplayName())
}

// powerTuningAdvisory names the measurement that would answer the question this
// package refuses to guess at. It is the honest output of a single-point
// observation: not a recommendation, an experiment.
func (s *Sizer) powerTuningAdvisory(f Function, obs Observation, a Assessment) Advisory {
	candidate := obs.MemoryFloorMB
	if candidate >= f.MemoryMB {
		// Nothing below the current setting is available to try; the useful
		// experiment is upward, where more CPU may shorten duration enough to
		// pay for itself.
		candidate = ClampMemoryMB(f.MemoryMB * 2)
	}
	return Advisory{
		Code: AdvisoryPowerTuning,
		Message: fmt.Sprintf(
			"to make this decidable, MEASURE %s: publish a version at %s, send it a representative share of "+
				"traffic until at least %d warm invocations have run, and re-run kilter. Two measured settings "+
				"are the minimum a GB-second bill comparison needs; one is an opinion",
			f.DisplayName(), fmtMB(candidate), s.cfg.MinSamplesPerPoint),
		ProposedMemoryMB: candidate,
		Caveat: "advisory only, and NOT a saving: the point of the trial is that nobody — including this " +
			"tool — knows what " + fmtMB(candidate) + " costs for this function until it runs there. Duration " +
			"may fall (cheaper), stay flat (cheaper), or more than double (more expensive).",
	}
}

// armAdvisory reports the Graviton rate gap (§4.5). It is advisory forever, and
// the number it carries is deliberately called a RATE delta:
//
//   - The arm64 GB-second rate is ~20 % lower [verified: lambda/pricing].
//   - The DURATION on arm64 is not measured, and this package's whole rule is
//     that a bill needs a measured duration at the setting it claims about.
//     A different CPU architecture is at least as big a change as a different
//     memory setting.
//   - Portability is unobservable from any metric.
func (s *Sizer) armAdvisory(f Function, obs Observation, a Assessment) []Advisory {
	if !s.cfg.ARMAdvisory || f.Arch() != ArchX86 || !a.CostKnown || a.CurrentHourlyUSD <= 0 {
		return nil
	}
	cur, ok := obs.Current()
	if !ok {
		return nil
	}
	armCost := s.cfg.Rates.HourlyUSD(obs.InvocationsPerHour, ArchARM, cur.MemoryMB, cur.MeanBilledMS)
	delta := MonthlyUSD(a.CurrentHourlyUSD - armCost)
	if delta <= 0 {
		return nil
	}
	portability := "every native module, compiled dependency and vendored binary in the deployment package must " +
		"have an arm64 build, and nothing in CloudWatch can tell you whether they do"
	if f.PackageType == PackageImage {
		portability = "this is a container-image function: the ENTIRE image — base layer, system packages, " +
			"native modules — must be rebuilt for linux/arm64, and nothing in CloudWatch can tell you whether " +
			"that is possible"
	}
	return []Advisory{{
		Code: AdvisoryARM,
		Message: fmt.Sprintf(
			"%s runs on %s; arm64/Graviton bills %.0f%% less per GB-second (%s vs %s), a %s/mo rate difference "+
				"at this function's current %s billed duration",
			f.DisplayName(), ArchX86, s.cfg.Rates.ArmRateDelta()*100,
			fmt.Sprintf("$%.10f", s.cfg.Rates.ArmGBSecondUSD), fmt.Sprintf("$%.10f", s.cfg.Rates.GBSecondUSD),
			fmtUSD(delta), fmtMS(cur.MeanBilledMS)),
		ProposedArch:        ArchARM,
		RateDeltaMonthlyUSD: delta,
		Caveat: "advisory only, never auto-applied, and the figure above is a RATE delta at UNCHANGED duration " +
			"— not a saving. Graviton is a different CPU: the duration this function runs at on arm64 is " +
			"unmeasured, and duration is half of a GB-second bill, so the real delta may be larger, smaller or " +
			"negative. Portability: " + portability + ". Measure it on a published version before believing " +
			"any number here.",
	}}
}

// confidence scores the evidence. Nothing is assumed: each factor adds only
// what it can demonstrate.
func (s *Sizer) confidence(obs Observation, usable []MemoryPoint) Confidence {
	var c Confidence

	pointsEarned, pointsWhy := 0.0, fmt.Sprintf("%d memory setting(s) measured with >= %d warm invocations",
		len(usable), s.cfg.MinSamplesPerPoint)
	switch {
	case len(usable) >= 3:
		pointsEarned = 1
	case len(usable) == 2:
		pointsEarned = 0.8
	}
	c.Add("measured-points", weightMeasuredPoints, pointsEarned, pointsWhy)

	c.Add("report-coverage", weightReportCoverage, obs.ReportCoverage,
		fmt.Sprintf("%d REPORT lines parsed for %.0f invocations (source: %s)",
			obs.Records, obs.Invocations, obs.InvocationSource))

	// Nothing observed earns nothing: with no invocations at all the cold
	// share is 0, and a factor that scores a blank record as perfect is a
	// factor that mistakes absence for evidence.
	warmEarned, warmWhy := 0.0, "no invocations observed"
	if obs.Warm+obs.Cold > 0 {
		warmEarned = 1 - obs.ColdShare
		warmWhy = fmt.Sprintf("%.1f%% of invocations were cold starts", obs.ColdShare*100)
	}
	c.Add("warm-share", weightWarmShare, warmEarned, warmWhy)

	// A Lambda log window is a slice of hours, not days, so the prose rounds
	// to the minute. That argument is the domain fact pkg/confidence refuses
	// to guess: rounding this minimum to the hour would print a wrong number.
	windowEarned, windowWhy := confidence.WindowFactor(
		obs.Window.Duration(), s.cfg.MinWindow, obs.Window.String(), time.Minute)
	c.Add(confidence.FactorWindow, weightWindow, windowEarned, windowWhy)

	headEarned, headWhy := 0.0, "max memory used is at the configured ceiling: possibly truncated"
	if cur, ok := obs.Current(); ok && cur.MemoryMB > 0 && !cur.AtCeiling {
		margin := 1 - float64(cur.MaxMemoryUsedMB)/float64(cur.MemoryMB)
		headEarned = math.Min(1, margin/0.25)
		headWhy = fmt.Sprintf("%s used of %s configured (%.0f%% margin)",
			fmtMB(cur.MaxMemoryUsedMB), fmtMB(cur.MemoryMB), margin*100)
	}
	c.Add("memory-headroom", weightHeadroom, headEarned, headWhy)
	return c
}

// weakestFactor names the factor that cost the most confidence, so a
// low-confidence refusal says what would fix it.
func weakestFactor(c Confidence) string { return confidence.WeakestFactor(c) }

// memoryPointsValue renders the measured settings as evidence prose.
func memoryPointsValue(points []MemoryPoint) string {
	if len(points) == 0 {
		return "none"
	}
	out := ""
	for i, p := range points {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s×%d", fmtMB(p.MemoryMB), p.Samples())
	}
	return out
}

func dropSummary(drops []Drop) string {
	if len(drops) == 0 {
		return "none"
	}
	out := ""
	for i, d := range drops {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s×%d", d.Code, d.Count)
	}
	return out
}

func warmAt(obs Observation, memoryMB int64) int {
	if p, ok := obs.PointAt(memoryMB); ok {
		return p.Warm
	}
	return 0
}

func meanInit(points []MemoryPoint) float64 {
	var sum float64
	var n int
	for _, p := range points {
		sum += p.MeanInitMS * float64(p.Cold)
		n += p.Cold
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
