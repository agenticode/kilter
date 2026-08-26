// Package lambda observes AWS Lambda functions and reports what their memory
// setting should be — without ever changing one.
//
// # Why this domain refuses more than it recommends
//
// Lambda bills GB-seconds: cost = memory × duration. Memory is also the only
// performance knob, because CPU is allocated *proportionally to memory*
// (≈ 1 vCPU near 1,769 MB — docs/design/compute-domains.md §4.8). So the naive
// recommendation — "max memory used is 74 MB, drop the setting from 512 MB to
// 128 MB, save 75 %" — is wrong roughly half the time: at 128 MB the function
// gets a quarter of the CPU, the duration can more than quadruple, and the
// bill goes UP.
//
// The response of duration to memory is a property of the function's code, not
// of any metric AWS publishes. It cannot be derived from a single operating
// point, and no amount of statistics over one memory setting recovers it. This
// package therefore obeys one rule, encoded as a refusal rather than as a
// caveat string:
//
//	No saving is claimed at a proposed memory setting without a MEASURED
//	duration AT that setting.
//
// With one measured setting the package reports the memory floor and the risk,
// says the cost effect is UNKNOWN, and names the trial that would answer it
// ([ReasonSingleMemoryPoint], [AdvisoryPowerTuning]). With two or more measured
// settings it compares measured bills — and when the smaller setting measured
// *more expensive*, it says so as a cost increase ([AdvisoryCostIncrease]) and
// refuses the downsize ([ReasonLowerMemoryCostsMore]). It never interpolates,
// never extrapolates, and never models a curve it has not observed.
//
// # Advisory only, structurally
//
// Nothing here modifies a function's configuration. There is no actuator, no
// mutating seam, no flag that would enable one; `lambda:UpdateFunctionConfiguration`
// is a later unit's problem (§6 U9 ships read-only). Every [Proposal] carries
// [domain.ActionAdvisory], [Report.Validate] rejects any other action class,
// and [Domain.PlanSteps] returns [domain.ErrReportOnly] unconditionally.
//
// The package links no AWS SDK and makes no network call. Its three cloud seams
// ([InventoryAPI], [LogsAPI], [MetricsAPI]) are plain Go interfaces over plain
// Go structs shaped after `lambda:ListFunctions`, `logs:FilterLogEvents` and
// `cloudwatch:GetMetricData`, so the decision path links into the air-gapped
// binary and the SDK adapter lives in cmd/ wiring a later unit adds.
//
// # Evidence
//
// The load-bearing evidence is the CloudWatch Logs REPORT line, which is the
// only place AWS publishes max-memory-used and the only place the configured
// memory of a *past* invocation survives:
//
//	REPORT RequestId: 3f9…	Duration: 12.34 ms	Billed Duration: 13 ms	Memory Size: 512 MB	Max Memory Used: 74 MB	Init Duration: 143.21 ms
//
// That is a log format, not an API. [ParseReport] is written for adversarial
// input: unknown fields are ignored, ambiguous labels are disambiguated
// longest-first, and anything that does not parse into a plausible number is
// DROPPED with a reason code rather than turned into a wrong number. A dropped
// line is counted and reported; it is never silently absorbed.
//
// # Determinism
//
// No clock: callers pass `now`. No package-level mutable state. Every iteration
// order is sorted by an intrinsic key, so shuffling functions, log events,
// metric results or tags cannot change a byte of the report — pinned by
// TestReportIsShuffleInvariant.
package lambda

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Kind names this compute domain.
const Kind = domain.Lambda

// Lambda platform limits [verified: https://aws.amazon.com/lambda/pricing/ and
// docs.aws.amazon.com/lambda/latest/dg/configuration-memory.html]. Memory is
// configurable from 128 MB to 10,240 MB in 1 MB steps; billing granularity is
// 1 ms; the maximum timeout is 15 minutes.
const (
	MinMemoryMB       int64 = 128
	MaxMemoryMB       int64 = 10240
	MaxTimeoutSeconds int64 = 900
	// MinBilledMS is Lambda's billing granularity: every invocation is billed
	// for at least one whole millisecond.
	MinBilledMS float64 = 1
	// VCPUMemoryMB is the memory setting at which a function is allocated
	// approximately one full vCPU [not re-verified: configuration-memory.html].
	// It is used for DISPLAY ONLY — see [VCPUMilliAt].
	VCPUMemoryMB int64 = 1769
)

// HoursPerMonth converts hourly to monthly cost using the billing-average
// month (8760 h/year ÷ 12), matching pkg/pricing and pkg/pricing/commit.
const HoursPerMonth = 730

// Architectures a Lambda function can be built for. AWS spells them
// "x86_64" and "arm64" in ListFunctions output.
const (
	ArchX86 = "x86_64"
	ArchARM = "arm64"
)

// Package types. An image-packaged function carries a much stronger
// architecture-portability caveat than a zip: the whole image, not just the
// handler, must have an arm64 build.
const (
	PackageZip   = "Zip"
	PackageImage = "Image"
)

// CloudWatch metric names this package understands, all in the AWS/Lambda
// namespace.
const (
	MetricInvocations                     = "Invocations"
	MetricDuration                        = "Duration"
	MetricErrors                          = "Errors"
	MetricThrottles                       = "Throttles"
	MetricProvisionedConcurrentExecutions = "ProvisionedConcurrentExecutions"

	NamespaceLambda = "AWS/Lambda"
)

// LogGroupPrefix is where a function's REPORT lines live.
const LogGroupPrefix = "/aws/lambda/"

// Attr keys used in [domain.Spec.Attrs].
const (
	AttrMemoryMB               = "memoryMB"
	AttrArch                   = "arch"
	AttrRuntime                = "runtime"
	AttrPackageType            = "packageType"
	AttrProvisionedConcurrency = "provisionedConcurrency"
	AttrTimeoutSeconds         = "timeoutSeconds"
)

// TagKilterMode is the opt-out tag, mirroring the Kubernetes annotation
// guardrail the node domain already honors.
const TagKilterMode = "kilter.dev/mode"

// Reason codes. They are stable strings meant to be stored, matched on and
// asserted against; the prose in [Suppression.Reason] is not.
const (
	// ReasonModeOff: tagged kilter.dev/mode=off.
	ReasonModeOff = "guardrail-mode-off"

	// ReasonUnknownConfiguration: the collector delivered no usable memory
	// setting, so there is no current bill to compare anything against.
	ReasonUnknownConfiguration = "unknown-configuration"

	// ReasonProvisionedConcurrency: provisioned concurrency bills a separate
	// per-GB-hour charge for provisioned environments plus a discounted
	// duration rate, so the on-demand GB-second arithmetic in this package
	// does not describe this function's bill at all.
	ReasonProvisionedConcurrency = "provisioned-concurrency"

	// ReasonNoReportEvidence: no REPORT line survived parsing. Max memory used
	// exists nowhere else, so there is no floor and no measured duration.
	ReasonNoReportEvidence = "no-report-evidence"

	// ReasonInsufficientWindow: the observed span is too short to have seen
	// this function's memory peak.
	ReasonInsufficientWindow = "insufficient-window"

	// ReasonInsufficientInvocations: too few invocations to characterize
	// anything. A handful of samples is an anecdote.
	ReasonInsufficientInvocations = "insufficient-invocations"

	// ReasonColdStartDominated: initialization dominates the observed record,
	// so warm duration statistics do not describe the bill and a memory change
	// moves init time too — in a direction this data cannot show.
	ReasonColdStartDominated = "cold-start-dominated"

	// ReasonMemoryAtCeiling: max memory used sits at the configured memory.
	// That is evidence the measurement was TRUNCATED by the limit, not
	// evidence that the function fits — the true peak is unknown and may be
	// larger. A downsize from a possibly-truncated peak is an OOM.
	ReasonMemoryAtCeiling = "memory-at-ceiling"

	// ReasonSingleMemoryPoint: only one memory setting was ever measured. The
	// memory floor and the risk are reported; the COST EFFECT IS UNKNOWN and
	// no saving may be claimed. This is the core refusal of this unit.
	ReasonSingleMemoryPoint = "single-memory-point"

	// ReasonNoMeasurementAtCurrent: two or more settings were measured, but
	// none of them is the setting the function runs on now, so the current
	// bill itself is unmeasured and every delta from it would be a guess.
	ReasonNoMeasurementAtCurrent = "no-measurement-at-current-setting"

	// ReasonLowerMemoryCostsMore: a smaller memory setting WAS measured and it
	// measured more expensive — the GB-second trap. Reported as a cost
	// increase, never as a saving.
	ReasonLowerMemoryCostsMore = "lower-memory-costs-more"

	// ReasonNoCheaperMeasurement: nothing measured beats the current setting.
	ReasonNoCheaperMeasurement = "no-cheaper-measured-setting"

	// ReasonBelowMemoryFloor: the cheapest measured setting does not clear the
	// observed memory floor, so it is unusable however cheap it measured.
	ReasonBelowMemoryFloor = "cheapest-measurement-below-floor"

	// ReasonLowConfidence: the evidence does not support acting, only watching.
	ReasonLowConfidence = "low-confidence"

	// ReasonCommitmentNegative / ReasonCommitmentNeutral are re-exported from
	// the commitment waterfall: Compute Savings Plans cover Lambda duration
	// account-wide (§4.4), so a GB-second reduction can be worth less — or
	// nothing — than its list price suggests.
	ReasonCommitmentNegative = domain.SuppressCommitmentNegative
	ReasonCommitmentNeutral  = domain.SuppressCommitmentNeutral
)

// Advisory codes. An advisory is reported and never actuated — see [Advisory].
const (
	// AdvisoryARM is the Graviton migration finding: arm64 is ~20 % cheaper
	// per GB-second, but the duration on arm64 is UNMEASURED and portability
	// is unobservable, so it is never a claimed saving.
	AdvisoryARM = "arm-migration"
	// AdvisoryPowerTuning names the measurement that would answer the question
	// this package refuses to guess at.
	AdvisoryPowerTuning = "power-tuning-trial"
	// AdvisoryMemoryTruncated: max memory used is at the configured ceiling.
	AdvisoryMemoryTruncated = "memory-possibly-truncated"
	// AdvisoryCostIncrease: a measured setting that costs MORE, reported so a
	// human can see the trap instead of walking into it.
	AdvisoryCostIncrease = "measured-cost-increase"
	// AdvisoryUnderMemory: the observed floor exceeds the configured memory —
	// the function is under-provisioned, and raising memory costs money this
	// unit will not spend on its own.
	AdvisoryUnderMemory = "under-provisioned-memory"
)

// Risk levels, matching the strings pkg/plan uses.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// Evidence sources.
const (
	SourceReportLine    = "cloudwatch-logs-report"
	SourceCloudWatch    = "cloudwatch"
	SourceListFunctions = "list-functions"
	SourceLedger        = "commitment-ledger"
)

// Suppression is a stated reason a change was not proposed, or a stronger one
// withheld. ValidFrom is set when the block is dated — a commitment term that
// expires — so the suppression lapses on its own.
type Suppression struct {
	Code      string    `json:"code"`
	Reason    string    `json:"reason"`
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

// Advisory is a finding that is reported and never actuated. Architecture
// migration is the archetype: portability cannot be observed from a metric, and
// neither can the duration a function would run at on a different CPU, so no
// amount of price evidence makes it applicable. An advisory must always carry a
// Caveat — [Report.Validate] rejects a report where one does not.
type Advisory struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Caveat  string `json:"caveat"`
	// ProposedMemoryMB / ProposedArch name the shape the advisory discusses.
	ProposedMemoryMB int64  `json:"proposedMemoryMB,omitempty"`
	ProposedArch     string `json:"proposedArch,omitempty"`
	// RateDeltaMonthlyUSD is a LIST-RATE delta at UNCHANGED duration. It is
	// deliberately not called a saving and is never added to a claim: for the
	// ARM advisory the duration on arm64 is unmeasured, and for a cost-increase
	// advisory the number is negative by construction.
	RateDeltaMonthlyUSD float64 `json:"rateDeltaMonthlyUSD,omitempty"`
}

// Actuatable is false for every advisory, always. It is a method rather than a
// field so no serialized form and no future struct literal can claim otherwise.
func (Advisory) Actuatable() bool { return false }

// Action reports the advisory action class.
func (Advisory) Action() domain.ActionClass { return domain.ActionAdvisory }

// Window is the closed observation interval a snapshot covers.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Duration is the covered span, never negative.
func (w Window) Duration() time.Duration {
	if w.End.Before(w.Start) {
		return 0
	}
	return w.End.Sub(w.Start)
}

// Hours is the covered span in hours, never negative.
func (w Window) Hours() float64 { return w.Duration().Hours() }

func (w Window) String() string {
	if w.Duration() == 0 {
		return "empty"
	}
	return w.Duration().Round(time.Minute).String()
}

// Function is one observed Lambda function, normalized from ListFunctions /
// GetFunctionConfiguration. It carries only what a sizing decision or a
// guardrail reads.
type Function struct {
	ARN          string            `json:"arn"`
	Name         string            `json:"name"`
	MemoryMB     int64             `json:"memoryMB"`
	TimeoutSec   int64             `json:"timeoutSeconds,omitempty"`
	Architecture string            `json:"architecture,omitempty"` // x86_64 | arm64
	Runtime      string            `json:"runtime,omitempty"`
	PackageType  string            `json:"packageType,omitempty"` // Zip | Image
	LastModified time.Time         `json:"lastModified,omitzero"`
	Tags         map[string]string `json:"tags,omitempty"`
	// ProvisionedConcurrency is the total configured across aliases/versions.
	// Non-zero means a different billing model entirely (§ ReasonProvisionedConcurrency).
	ProvisionedConcurrency int64 `json:"provisionedConcurrency,omitempty"`
	// EphemeralStorageMB is /tmp size. Storage above 512 MB is billed
	// separately and is NOT modeled here; it is carried so the report can say
	// so rather than quietly omit a line of the bill.
	EphemeralStorageMB int64 `json:"ephemeralStorageMB,omitempty"`
}

// Arch returns the function's architecture, defaulting to x86_64 the way
// ListFunctions does when the field is absent.
func (f Function) Arch() string {
	if f.Architecture == ArchARM {
		return ArchARM
	}
	return ArchX86
}

// ModeOff reports the kilter.dev/mode=off tag guardrail.
func (f Function) ModeOff() bool {
	return strings.EqualFold(strings.TrimSpace(f.Tags[TagKilterMode]), "off")
}

// DisplayName returns the function name, falling back to its ARN.
func (f Function) DisplayName() string {
	if f.Name != "" {
		return f.Name
	}
	return f.ARN
}

// Point is one metric datapoint.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Series is one CloudWatch metric's observations for one function, as
// delivered. The period is data, not configuration: it records the granularity
// CloudWatch actually published.
type Series struct {
	Metric        string  `json:"metric"`
	Stat          string  `json:"stat,omitempty"`
	Source        string  `json:"source,omitempty"`
	PeriodSeconds int32   `json:"periodSeconds,omitempty"`
	Points        []Point `json:"points,omitempty"`
	// Partial marks a series CloudWatch did not deliver in full.
	Partial bool   `json:"partial,omitempty"`
	Status  string `json:"status,omitempty"`
}

// Sum totals the observations.
func (s Series) Sum() float64 {
	var sum float64
	for _, p := range s.Points {
		sum += p.Value
	}
	return sum
}

// Max returns the largest observed value, and false when there are none.
func (s Series) Max() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	m := s.Points[0].Value
	for _, p := range s.Points[1:] {
		if p.Value > m {
			m = p.Value
		}
	}
	return m, true
}

// Target is one function plus everything observed about it.
type Target struct {
	Ref      domain.TargetRef `json:"ref"`
	Function Function         `json:"function"`
	// Reports are the parsed REPORT records, sorted by (timestamp, request ID).
	Reports []ReportRecord `json:"reports,omitempty"`
	// Drops are the REPORT lines that did not survive parsing, aggregated by
	// reason code and sorted. A dropped line is reported, never absorbed.
	Drops []Drop `json:"drops,omitempty"`
	// Series is sorted by metric name.
	Series []Series `json:"series,omitempty"`
}

// SeriesFor returns the named series, and false when it was not delivered.
func (t Target) SeriesFor(metric string) (Series, bool) {
	i := sort.Search(len(t.Series), func(i int) bool { return t.Series[i].Metric >= metric })
	if i < len(t.Series) && t.Series[i].Metric == metric {
		return t.Series[i], true
	}
	return Series{}, false
}

// Snapshot is what a collector ships to the brain: the §5.2 domain snapshot,
// specialized to Lambda targets.
type Snapshot struct {
	Domain    domain.Kind `json:"domain"`
	Scope     string      `json:"scope"` // accountID/region
	Region    string      `json:"region,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
	Window    Window      `json:"window"`
	// Targets is sorted by function ARN.
	Targets []Target `json:"targets,omitempty"`
	// Stale marks a snapshot the collector could not complete.
	Stale bool `json:"stale,omitempty"`
	// Warnings are human-readable collection problems, sorted and deduped.
	Warnings []string `json:"warnings,omitempty"`
}

// SortTargets puts targets and everything inside them in canonical order, so
// two collectors that walked the same account in a different order ship the
// same snapshot.
func SortTargets(ts []Target) {
	for i := range ts {
		SortReports(ts[i].Reports)
		sort.Slice(ts[i].Series, func(a, b int) bool { return ts[i].Series[a].Metric < ts[i].Series[b].Metric })
		sort.Slice(ts[i].Drops, func(a, b int) bool { return ts[i].Drops[a].Code < ts[i].Drops[b].Code })
	}
	sort.Slice(ts, func(a, b int) bool { return ts[a].Ref.ID < ts[b].Ref.ID })
}

// sortWarnings deduplicates and sorts.
func sortWarnings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, w := range in {
		if seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// VCPUMilliAt is the CPU a function is allocated at a memory setting, in
// millicores, on the documented linear proportion (≈ 1 vCPU at 1,769 MB).
//
// DISPLAY ONLY. It is never an input to a cost claim or to a duration
// prediction: the whole thesis of this package is that the duration response to
// CPU is a property of the code, not something derivable from the allocation.
func VCPUMilliAt(memoryMB int64) int64 {
	if memoryMB <= 0 {
		return 0
	}
	return memoryMB * 1000 / VCPUMemoryMB
}

// ClampMemoryMB clamps a memory setting to the platform range.
func ClampMemoryMB(mb int64) int64 {
	switch {
	case mb < MinMemoryMB:
		return MinMemoryMB
	case mb > MaxMemoryMB:
		return MaxMemoryMB
	}
	return mb
}

// SpecFor renders a memory setting as a domain Spec.
func SpecFor(f Function, memoryMB int64, arch string) domain.Spec {
	s := domain.Spec{
		Attrs: map[string]string{
			AttrMemoryMB: fmt.Sprintf("%d", memoryMB),
			AttrArch:     arch,
		},
	}
	s.Resources.MemoryBytes = memoryMB << 20
	s.Resources.MilliCPU = VCPUMilliAt(memoryMB)
	if f.Runtime != "" {
		s.Attrs[AttrRuntime] = f.Runtime
	}
	if f.PackageType != "" {
		s.Attrs[AttrPackageType] = f.PackageType
	}
	if f.TimeoutSec > 0 {
		s.Attrs[AttrTimeoutSeconds] = fmt.Sprintf("%d", f.TimeoutSec)
	}
	if f.ProvisionedConcurrency > 0 {
		s.Attrs[AttrProvisionedConcurrency] = fmt.Sprintf("%d", f.ProvisionedConcurrency)
	}
	return s
}

// fmtUSD renders money at a fixed width so golden output does not drift with
// float formatting.
func fmtUSD(v float64) string { return fmt.Sprintf("$%.4f", v) }

// fmtMS renders a duration in milliseconds.
func fmtMS(v float64) string { return fmt.Sprintf("%.2fms", v) }

// fmtMB renders a memory setting.
func fmtMB(mb int64) string { return fmt.Sprintf("%dMB", mb) }
