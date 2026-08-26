// Package ecs observes ECS-on-Fargate services and says what their task
// definitions should reserve — and, behind hard gates, registers the revision
// that does it.
//
// # Why ECS Fargate is not EKS Fargate
//
// The two bill identically: one quantized vCPU/memory tier per task, per second
// (§4.1). They do NOT have the same features, and conflating them produces
// advice nobody can act on — docs/design/compute-domains.md §7 trap 3. EKS
// Fargate has no Fargate Spot and no ARM/Graviton, so pkg/pricing makes both
// unrepresentable: its [pricing.Platform] has exactly one value and its
// FargateRates struct has no ARM rate and no spot field.
//
// ECS Fargate has both. So this package carries its own [Platform] — an
// architecture crossed with a market — and its own [Rates], which do have the
// ARM rates and the spot discount. The difference lives in the type system on
// both sides: an ECS advisory cannot be built out of pkg/pricing's types, and
// an EKS recommendation cannot be built out of this package's. Neither is a
// comment that someone has to remember. TestEKSFargateStillRefusesSpotAndARM
// pins the EKS half; TestSpotAndARMArePriceableHere and
// TestSpotAndARMAdvisoriesAreLegalHere pin this half.
//
// What IS shared is the economics engine, and it is shared by reuse, not by
// copy: the tier table and the rounding are [pricing.Quantize] (U1), and the
// price function P(v,g) is [pricing.FargateRates.Cost]. The one EKS-specific
// term — the +256 MiB Kubernetes overhead, which pays for kubelet, kube-proxy
// and containerd, none of which exist in an ECS task — is cancelled out of the
// input rather than re-derived; see [RoundUpTier].
//
// # The reserved-vs-used trap
//
// ECS's default (free, automatic, 1-minute) CloudWatch metrics for a Fargate
// service are `CPUUtilization` and `MemoryUtilization`, and they are a
// PERCENTAGE OF RESERVED — not absolute usage
// [verified: AmazonECS/latest/developerguide/cloudwatch-metrics.html]. Absolute
// demand only exists after multiplying by the task definition's reservation:
//
//	used = percent / 100 × reserved
//
// Getting that backwards — dividing, or multiplying by the *proposed*
// reservation instead of the observed one — is this unit's central failure
// mode, because it fails quietly: the numbers stay plausible and the
// recommendation is wrong by the ratio of two tiers. [AbsoluteFromPercent] is
// the only conversion in the package, and TestAbsoluteFromPercentIsNotInverted
// fails if it is inverted.
//
// # Refusals over guesses
//
// Every service observed yields exactly one [Assessment], and an assessment
// with no proposal always carries the reason why. A proposal must survive, in
// order: ownership and mode tags, a metric window, sample coverage, revision
// drift, awsvpc and platform-version constraints, container-level limits, a
// confidence floor, and finally quantization — where a request change that
// crosses no tier boundary is reported as saving exactly $0 rather than as a
// saving. Silence is never an output.
//
// # Purity
//
// No clock: callers pass `now`. No package-level mutable state. No network
// call, no AWS SDK import — the two read seams ([InventoryAPI], [MetricsAPI])
// and the one write seam ([MutateAPI]) are plain Go interfaces over plain Go
// structs, so the decision path links into an air-gapped binary and the SDK
// adapter is a later unit's cmd/ wiring. Every iteration order is sorted by an
// intrinsic key; TestReportIsShuffleInvariant pins it.
package ecs

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// Kind is the compute domain this package implements.
const Kind = domain.ECSFargate

// CloudWatch coordinates for ECS's default service metrics. Both are published
// automatically for Fargate services at 1-minute resolution at no charge; only
// per-task detail needs paid Container Insights, which this unit does not use.
const (
	// Namespace is the ECS metric namespace.
	Namespace = "AWS/ECS"
	// MetricCPUUtilization is the percentage of the task definition's RESERVED
	// CPU that the service's tasks are using. Not cores. Not percent of a host.
	MetricCPUUtilization = "CPUUtilization"
	// MetricMemoryUtilization is the percentage of the task definition's
	// RESERVED memory in use.
	MetricMemoryUtilization = "MemoryUtilization"

	// DimClusterName and DimServiceName are the dimensions that scope the two
	// metrics to one service.
	DimClusterName = "ClusterName"
	DimServiceName = "ServiceName"

	// PeriodSeconds is ECS's publication period for service metrics.
	PeriodSeconds int32 = 60
)

// Evidence sources.
const (
	SourceCloudWatch = "cloudwatch"
	SourceDescribe   = "ecs:DescribeServices"
	SourceTaskDef    = "ecs:DescribeTaskDefinition"
	SourceQuantizer  = "pricing.Quantize"
)

// Tag keys that decide ownership and guardrails, mirroring the Kubernetes
// annotation semantics the node domain already uses. ECS surfaces service tags
// through DescribeServices when `include=TAGS` is requested.
const (
	TagKilterMode = "kilter.dev/mode"
	TagName       = "Name"
)

// Reason codes. They are stable strings meant to be stored, matched on and
// asserted against; the prose in [Suppression.Reason] is not.
const (
	// Ownership and guardrails.
	ReasonModeOff       = "guardrail-mode-off"
	ReasonModeRecommend = "guardrail-mode-recommend"
	ReasonNotFargate    = "not-fargate"
	ReasonServiceIdle   = "service-idle"

	// Evidence quality.
	ReasonNoMetricWindow      = "no-metric-window"
	ReasonPartialMetrics      = "partial-metrics"
	ReasonInsufficientWindow  = "insufficient-window"
	ReasonInsufficientSamples = "insufficient-samples"
	ReasonLowConfidence       = "low-confidence"
	// ReasonRevisionDrift: the task definition changed inside the metric
	// window, so the percentages on either side of the change are percentages
	// of DIFFERENT reservations and cannot be converted with one denominator.
	ReasonRevisionDrift = "revision-drift"

	// Shape constraints that block the proposal itself.
	ReasonDeploymentInProgress = "deployment-in-progress"
	ReasonNetworkMode          = "network-mode-unsupported"
	ReasonPlatformVersion      = "platform-version-too-old"
	ReasonInvalidTaskSize      = "invalid-task-size"
	ReasonContainerLimits      = "container-limits-block"
	ReasonTooLargeForFargate   = "exceeds-fargate-maximum"

	// Economics.
	// ReasonNoTierChange: the proposed reservation rounds to the tier the
	// service is already billed at, so the change saves exactly $0 (§7 trap 2).
	ReasonNoTierChange = "no-tier-change"
	// ReasonZeroSaving: the tiers differ but price identically — a rolling
	// deployment for nothing.
	ReasonZeroSaving = "zero-saving"
	// ReasonBelowMoveFloor: the saving is real but too small to be worth
	// replacing every task in the service.
	ReasonBelowMoveFloor = "below-move-floor"
	ReasonUndersized     = "undersized"
)

// Advisory codes. An advisory is never actuatable — see [Advisory].
const (
	// AdvisorySpot is the Fargate Spot capacity-provider advisory. Legal here,
	// illegal on EKS.
	AdvisorySpot = "fargate-spot"
	// AdvisoryARM is the Graviton/ARM64 runtime-platform advisory. Legal here,
	// illegal on EKS.
	AdvisoryARM = "graviton-migration"
	// AdvisoryUndersized reports a service whose observed demand already
	// exceeds its reservation. Growing it costs money, so it is reported, never
	// proposed.
	AdvisoryUndersized = "undersized"
)

// Spec attribute keys. A Spec's Resources hold the billed task size — the thing
// that costs money — while the identifiers an actuator needs live in Attrs.
const (
	// AttrTaskSize is the billed tier as AWS labels it, e.g. "1vCPU 2GB".
	AttrTaskSize = "taskSize"
	// AttrTaskCPU and AttrTaskMemory are the task-definition fields verbatim,
	// in the units RegisterTaskDefinition takes them ("1024", "2048").
	AttrTaskCPU    = "taskCPU"
	AttrTaskMemory = "taskMemory"
	// AttrTaskDefinition is the full task-definition ARN including the
	// revision. On the From spec it is the rollback target: reverting is an
	// UpdateService back to this exact revision, not a new revision.
	AttrTaskDefinition = "taskDefinition"
	// AttrFamily is the task-definition family.
	AttrFamily = "family"
	// AttrCluster and AttrService identify the UpdateService target.
	AttrCluster = "cluster"
	AttrService = "service"
	// AttrDesiredCount records the service's desired count at decision time.
	// It is documentation: this domain never changes a desired count, because
	// that belongs to the service's own autoscaler (§3.4).
	AttrDesiredCount = "desiredCount"
	// AttrArch and AttrMarket carry the ECS-only dimensions (§7 trap 3). They
	// appear on advisory specs only.
	AttrArch   = "arch"
	AttrMarket = "market"
	// AttrChange names the lever.
	AttrChange = "change"
	// AttrPlatformVersion records the service's Fargate platform version.
	AttrPlatformVersion = "platformVersion"
)

// Change types recorded in AttrChange.
const (
	ChangeTaskSize = "task-size"
	ChangeSpot     = "capacity-provider-spot"
	ChangeARM      = "runtime-platform-arm64"
)

// Suppression is a stated reason a change was not proposed (or a stronger one
// was withheld). ValidFrom is set when the block is dated — a commitment term
// that expires — so the suppression lapses on its own.
type Suppression struct {
	Code      string    `json:"code"`
	Reason    string    `json:"reason"`
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

// Advisory is a report-only finding: a change Kilter can price but must never
// apply, because its precondition is not observable from metrics. Fargate Spot
// needs interruption tolerance; ARM64 needs image and binary portability.
// Neither is in CloudWatch, so neither is ever an actuatable step.
type Advisory struct {
	Code string `json:"code"`
	// EstimatedMonthlyUSD is what the change would save at list price if its
	// unverifiable precondition holds. It is never claimed as a saving: it does
	// not reach a plan, a ledger entry, or [Assessment.ClaimableMonthlyUSD].
	EstimatedMonthlyUSD float64 `json:"estimatedMonthlyUSD"`
	// Caveat states the precondition Kilter cannot verify. Never empty.
	Caveat   string            `json:"caveat"`
	Detail   string            `json:"detail,omitempty"`
	Proposed domain.Spec       `json:"proposed,omitzero"`
	Evidence []domain.Evidence `json:"evidence,omitempty"`
}

// --- Task-definition size parsing -----------------------------------------

// ErrTaskSize reports a task-definition cpu/memory field this package cannot
// read. RegisterTaskDefinition accepts either raw units ("1024", "2048") or
// labelled ones ("1 vCPU", "2GB"), so both are parsed; anything else is an
// error rather than a guess, because guessing the denominator of a utilization
// percentage is exactly the bug this package exists to avoid.
var ErrTaskSize = fmt.Errorf("ecs: unreadable task size")

// ParseTaskCPU reads a task-definition `cpu` value into milli-CPU.
//
//	"1024" | "1024 " → 1000m      (CPU units: 1024 units = 1 vCPU)
//	"1 vCPU" | "1vcpu" → 1000m
//	"0.25 vCPU" → 250m
func ParseTaskCPU(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("%w: empty cpu", ErrTaskSize)
	}
	if n, ok := strings.CutSuffix(t, "vcpu"); ok {
		v, err := parsePositive(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("%w: cpu %q: %v", ErrTaskSize, s, err)
		}
		return int64(math.Round(v * 1000)), nil
	}
	v, err := parsePositive(t)
	if err != nil {
		return 0, fmt.Errorf("%w: cpu %q: %v", ErrTaskSize, s, err)
	}
	// CPU units: 1024 = one vCPU.
	return int64(math.Round(v / 1024 * 1000)), nil
}

// ParseTaskMemory reads a task-definition `memory` value into bytes.
//
//	"2048" → 2 GiB   (raw values are MiB)
//	"2GB" | "2 gb" → 2 GiB   (AWS's "GB" label means GiB in the tier table)
func ParseTaskMemory(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, fmt.Errorf("%w: empty memory", ErrTaskSize)
	}
	if n, ok := strings.CutSuffix(t, "gb"); ok {
		v, err := parsePositive(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("%w: memory %q: %v", ErrTaskSize, s, err)
		}
		return int64(math.Round(v*1024)) << 20, nil
	}
	if n, ok := strings.CutSuffix(t, "mb"); ok {
		v, err := parsePositive(strings.TrimSpace(n))
		if err != nil {
			return 0, fmt.Errorf("%w: memory %q: %v", ErrTaskSize, s, err)
		}
		return int64(math.Round(v)) << 20, nil
	}
	v, err := parsePositive(t)
	if err != nil {
		return 0, fmt.Errorf("%w: memory %q: %v", ErrTaskSize, s, err)
	}
	return int64(math.Round(v)) << 20, nil
}

// parsePositive parses a finite, strictly positive decimal.
func parsePositive(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if !(v > 0) || math.IsInf(v, 0) || v > 1<<40 {
		return 0, fmt.Errorf("value %q is not a finite positive number in range", s)
	}
	return v, nil
}

// FormatTaskCPU renders a tier's CPU as RegisterTaskDefinition takes it: CPU
// units, where 1024 units is one vCPU.
func FormatTaskCPU(c pricing.FargateConfig) string {
	return strconv.FormatInt(int64(math.Round(float64(c.MilliCPU)/1000*1024)), 10)
}

// FormatTaskMemory renders a tier's memory as RegisterTaskDefinition takes it:
// whole MiB.
func FormatTaskMemory(c pricing.FargateConfig) string {
	return strconv.FormatInt(c.MemoryMiB, 10)
}

// Reservation is one task's reserved compute — the DENOMINATOR of ECS's
// utilization percentages, and the only thing that turns them into demand.
type Reservation struct {
	// Revision is the task-definition revision the reservation came from. It
	// is carried alongside the numbers so a conversion can be checked against
	// the revision that was actually running when the metric was published.
	Revision int             `json:"revision"`
	ARN      string          `json:"arn,omitempty"`
	Reserved model.Resources `json:"reserved"`
}

// IsZero reports whether the reservation is unusable as a denominator.
func (r Reservation) IsZero() bool {
	return r.Reserved.MilliCPU <= 0 || r.Reserved.MemoryBytes <= 0
}

// AbsoluteFromPercent converts one ECS utilization datapoint into absolute
// demand.
//
//	used = percent / 100 × reserved
//
// The direction matters and is the reason this is a named function rather than
// three inline characters. ECS publishes CPUUtilization and MemoryUtilization
// as a percentage OF THE TASK DEFINITION'S RESERVATION, so:
//
//   - dividing by the percentage instead of multiplying inverts the whole
//     recommendation — a 25 %-utilized 4-vCPU service reads as needing 16 vCPU
//     instead of 1;
//   - multiplying by the PROPOSED reservation instead of the observed one
//     re-scales history to a size that never ran, which is the same bug with a
//     smaller constant.
//
// Percentages above 100 are real (a task can burst over its soft reservation)
// and are preserved. Negative, NaN and infinite percentages are dropped to 0:
// garbage must not travel into a size.
func AbsoluteFromPercent(percent float64, reserved int64) float64 {
	if !(percent > 0) || math.IsInf(percent, 0) || reserved <= 0 {
		return 0
	}
	return percent / 100 * float64(reserved)
}

// modeFor resolves a service's effective kilter.dev/mode from its tags. An
// unrecognized value falls back to def, which itself falls back to
// guard.ModeApply — the same opt-out semantics as the Kubernetes annotation.
func modeFor(tags map[string]string, def string) string {
	if m, ok := tags[TagKilterMode]; ok && validMode(m) {
		return m
	}
	if validMode(def) {
		return def
	}
	return modeApply
}

// Mode values, mirroring pkg/guard's annotation vocabulary. They are redeclared
// rather than imported so this package's tag semantics are readable in one
// place; TestModeVocabularyMatchesGuard pins them equal to pkg/guard's.
const (
	modeOff       = "off"
	modeRecommend = "recommend"
	modeApply     = "apply"
)

func validMode(m string) bool { return m == modeOff || m == modeRecommend || m == modeApply }
