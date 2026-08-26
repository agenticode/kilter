// Package ec2 observes plain (non-Kubernetes) EC2 instances and reports what
// they should be — without ever changing one.
//
// # Read-only, structurally
//
// This package contains no actuation and no mutating call of any kind, not
// behind a flag, not behind an interface. Its two cloud seams ([InventoryAPI],
// [MetricsAPI]) expose exactly two read operations, shaped after
// `ec2:DescribeInstances` and `cloudwatch:GetMetricData`. Instance resize,
// stop/start and ASG refresh are a later unit (docs/design/compute-domains.md
// §6 U7); nothing here can reach them.
//
// The package also links no AWS SDK and makes no network call. The seams are
// plain Go interfaces over plain Go structs, so the decision path stays
// air-gapped and the SDK adapter lives in cmd/ wiring a later unit adds.
//
// # The rule this package exists for
//
// CloudWatch reports no memory metric for EC2 without the CloudWatch agent
// ([verified] §3.3). A sizer that ignores this shrinks a memory-bound instance
// to death on CPU evidence alone — §7 trap 4. So:
//
//   - Memory-blind is a named, first-class state ([Observation.MemoryBlind]).
//   - In that state the memory floor is the instance's *current* memory. Only
//     same-or-more-memory moves are eligible.
//   - When the floor is what stopped a cheaper choice, that is reported as a
//     refusal ([ReasonMemoryBlind]) naming the instance type we declined to
//     pick — not silently as a smaller recommendation.
//
// Refusal is the default. A proposal has to earn its way past a window
// requirement, a sample-coverage requirement, per-instance suppressions and a
// confidence floor, in that order. Every instance observed yields exactly one
// [Assessment], and an [Assessment] without a proposal always carries the
// reason why — silence is never an output.
//
// # Determinism
//
// No clock: callers pass `now`. No package-level mutable state. Every
// iteration order is sorted by an intrinsic key, so shuffling instances,
// metric results or tags cannot change a byte of the report — pinned by
// TestReportIsShuffleInvariant.
//
// # Money
//
// Gross savings are the on-demand list-price delta — the fantasy. The only
// number this package presents as a saving is the bill delta computed by
// pkg/pricing/commit's waterfall (§4.4), and a change whose net is not
// positive is suppressed with its reason attached (§7 trap 1).
package ec2

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// Domain names this compute domain, matching docs/design/compute-domains.md
// §5.2 `domain.Kind`. It is a plain string rather than an import of pkg/domain
// so this package can ship before that seam exists; the adapter is one struct
// literal (see FINDINGS.md).
const Domain = "ec2"

// CloudWatch metric names this package understands. The EC2 namespace is
// "AWS/EC2"; memory comes from the CloudWatch agent's "CWAgent" namespace and
// is absent on an instance that does not run it.
const (
	MetricCPUUtilization           = "CPUUtilization"           // percent, AWS/EC2
	MetricCPUCreditBalance         = "CPUCreditBalance"         // credits, AWS/EC2, T-family only
	MetricCPUCreditUsage           = "CPUCreditUsage"           // credits, AWS/EC2, T-family only
	MetricCPUSurplusCreditBalance  = "CPUSurplusCreditBalance"  // credits, AWS/EC2, T-family unlimited
	MetricCPUSurplusCreditsCharged = "CPUSurplusCreditsCharged" // credits, AWS/EC2, T-family unlimited
	MetricMemUsedPercent           = "mem_used_percent"         // percent, CWAgent — absent without the agent
)

// Namespaces the collector queries.
const (
	NamespaceEC2     = "AWS/EC2"
	NamespaceCWAgent = "CWAgent"
)

// CloudWatch publication periods. Basic (free) EC2 monitoring publishes one
// datapoint per 300 s; 1-minute datapoints require paid detailed monitoring
// ([verified] §3.3). Requesting Maximum at a 300 s period buys nothing when
// only one datapoint exists per window — the limit is publication granularity,
// not the statistic.
const (
	PeriodBasicSeconds    int32 = 300
	PeriodDetailedSeconds int32 = 60
)

// Tag keys that decide ownership and guardrails, mirroring the Kubernetes
// annotation semantics the node domain already uses.
const (
	TagK8sClusterPrefix = "kubernetes.io/cluster/" // any value ⇒ cluster node
	TagEKSCluster       = "eks:cluster-name"
	TagAWSEKSCluster    = "aws:eks:cluster-name"
	TagKilterMode       = "kilter.dev/mode"
	TagName             = "Name"
)

// Reason codes. They are stable strings meant to be stored, matched on and
// asserted against; the prose in Suppression.Reason is not.
const (
	// Ownership and guardrails — the instance is not this domain's to size.
	ReasonK8sTagged = "k8s-tagged"
	ReasonModeOff   = "guardrail-mode-off"

	// Evidence quality — we were not shown enough to decide.
	ReasonNoMetrics           = "no-metrics"
	ReasonPartialMetrics      = "partial-metrics"
	ReasonInsufficientWindow  = "insufficient-window"
	ReasonInsufficientSamples = "insufficient-samples"
	ReasonUnknownInstanceType = "unknown-instance-type"
	ReasonLowConfidence       = "low-confidence"

	// The memory-blind rule (§7 trap 4).
	ReasonMemoryBlind = "memory-blind"

	// Burstable analytics (§4.6, §7 trap 5).
	ReasonBurstEvidenceMissing = "burst-evidence-missing"
	ReasonBurstCreditDepleted  = "burst-credit-depleted"
	ReasonBurstSurplusCharged  = "burst-surplus-charged"

	// ReasonUndersized suppresses a downsize on an instance whose observed
	// demand already exceeds its shape. Growing it is reported as an advisory,
	// never proposed: spending money is not this unit's call.
	ReasonUndersized = "undersized"

	// Nothing was wrong; nothing was cheaper.
	ReasonNoCheaperCandidate = "no-cheaper-candidate"
)

// Advisory codes. An advisory is never actuatable — see [Advisory].
const (
	AdvisoryGraviton      = "graviton-migration"
	AdvisoryBurstThrottle = "burst-throttled"
	AdvisoryBurstSurplus  = "burst-surplus"
	AdvisoryUndersized    = "undersized"
)

// TargetRef identifies one billable, independently resizable unit. It is the
// §5.2 shape, field for field, so the later pkg/domain adapter is a copy.
type TargetRef struct {
	Domain string `json:"domain"`
	Scope  string `json:"scope"` // accountID/region
	ID     string `json:"id"`    // instance ID
	Name   string `json:"name,omitempty"`
}

func (r TargetRef) String() string { return r.Domain + "/" + r.Scope + "/" + r.ID }

// Spec is a resource specification in this domain's vocabulary. Attrs carry
// the axes model.Resources has no room for; keys are sorted by encoding/json,
// so a Spec marshals identically regardless of insertion order.
type Spec struct {
	Resources model.Resources   `json:"resources,omitempty"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

// Attr keys used in [Spec.Attrs].
const (
	AttrInstanceType = "instanceType"
	AttrArch         = "arch"
	AttrPlatform     = "platform"
	AttrTenancy      = "tenancy"
	AttrAZ           = "az"
	AttrCreditMode   = "creditMode"
	AttrBurstable    = "burstable"
)

// Evidence is one observable fact backing a decision — a metric window, a
// percentile, a source system. Every assessment states its evidence; an
// assessment with none is a bug, and [Report.Validate] says so.
type Evidence struct {
	Metric  string    `json:"metric"`
	Value   string    `json:"value"`
	Window  string    `json:"window,omitempty"`
	Samples int       `json:"samples,omitempty"`
	Source  string    `json:"source"` // cloudwatch | cwagent | describe-instances | catalog
	At      time.Time `json:"at,omitempty"`
}

// Suppression is a stated reason a change was not proposed (or a stronger one
// was withheld). ValidFrom is set when the block is dated — a commitment term
// that expires — so the suppression lapses on its own.
type Suppression struct {
	Code      string    `json:"code"`
	Reason    string    `json:"reason"`
	ValidFrom time.Time `json:"validFrom,omitempty"`
}

// ActionClass is descriptive metadata for a later executing unit (§5.2). This
// package has no executor: an ActionClass here is a label on a report, never a
// dispatch key.
type ActionClass string

const (
	ActionStopStart ActionClass = "stop-start" // downtime: stop, ModifyInstanceAttribute, start
	ActionAdvisory  ActionClass = "advisory"   // never auto-applied
)

// Risk levels, matching the strings pkg/plan uses.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// Point is one metric datapoint.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Series is one metric's observations for one target, as delivered. The
// period is data, not configuration: it records the granularity CloudWatch
// actually published, which is what bounds what can be seen.
type Series struct {
	Metric        string  `json:"metric"`
	Namespace     string  `json:"namespace,omitempty"`
	Stat          string  `json:"stat,omitempty"`
	PeriodSeconds int32   `json:"periodSeconds"`
	Points        []Point `json:"points,omitempty"`
	// Partial marks a series CloudWatch did not deliver in full — a
	// GetMetricData status other than "Complete", or a response the collector
	// had to truncate. A partial window is not a window: the sizer refuses.
	Partial bool `json:"partial,omitempty"`
	// Status is the raw GetMetricData status code, kept for the report.
	Status string `json:"status,omitempty"`
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

// Min returns the smallest observed value, and false when there are none.
func (s Series) Min() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	m := s.Points[0].Value
	for _, p := range s.Points[1:] {
		if p.Value < m {
			m = p.Value
		}
	}
	return m, true
}

// Mean returns the arithmetic mean of the observations, and false when there
// are none. Every point weighs the same because CloudWatch publishes one
// datapoint per period.
func (s Series) Mean() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	var sum float64
	for _, p := range s.Points {
		sum += p.Value
	}
	return sum / float64(len(s.Points)), true
}

// Sum returns the total of the observations.
func (s Series) Sum() float64 {
	var sum float64
	for _, p := range s.Points {
		sum += p.Value
	}
	return sum
}

// Last returns the most recent observation. Points are kept sorted by time.
func (s Series) Last() (Point, bool) {
	if len(s.Points) == 0 {
		return Point{}, false
	}
	return s.Points[len(s.Points)-1], true
}

// Percentile returns the p-quantile (0..1) by nearest rank over a sorted copy
// of the values. Nearest rank, not interpolation: with 5-minute datapoints the
// sample count is small and interpolation invents values between real ones.
func (s Series) Percentile(p float64) (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	vals := make([]float64, len(s.Points))
	for i, pt := range s.Points {
		vals[i] = pt.Value
	}
	sort.Float64s(vals)
	switch {
	case p <= 0:
		return vals[0], true
	case p >= 1:
		return vals[len(vals)-1], true
	}
	rank := int(float64(len(vals))*p + 0.9999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(vals) {
		rank = len(vals)
	}
	return vals[rank-1], true
}

// Window is the closed observation interval a series or snapshot covers.
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

func (w Window) String() string {
	if w.Duration() == 0 {
		return "empty"
	}
	return w.Duration().Round(time.Minute).String()
}

// Instance is one observed EC2 instance, normalized from DescribeInstances.
// It carries only what a sizing decision or a guardrail reads.
type Instance struct {
	ID               string            `json:"id"`
	InstanceType     string            `json:"instanceType"`
	Architecture     string            `json:"architecture,omitempty"` // amd64 | arm64
	Platform         string            `json:"platform,omitempty"`     // "" ⇒ Linux/UNIX
	Tenancy          string            `json:"tenancy,omitempty"`      // "" ⇒ default
	AvailabilityZone string            `json:"availabilityZone,omitempty"`
	State            string            `json:"state,omitempty"` // running | stopped | …
	LaunchTime       time.Time         `json:"launchTime,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
	// DetailedMonitoring is `monitoring.state == "enabled"`. False means
	// CloudWatch publishes at 300 s and no requested period can improve it.
	DetailedMonitoring bool `json:"detailedMonitoring,omitempty"`
	// CreditMode is the T-family credit specification: "standard",
	// "unlimited", or "" when unknown or not applicable.
	CreditMode string `json:"creditMode,omitempty"`
	// InstanceStore marks ephemeral local storage. Stopping such an instance
	// destroys its data, so a later actuating unit must refuse a stop-start
	// resize here; this package records it as risk, and nothing more.
	InstanceStore bool `json:"instanceStore,omitempty"`
}

// Name returns the Name tag, or the instance ID.
func (i Instance) Name() string {
	if n := i.Tags[TagName]; n != "" {
		return n
	}
	return i.ID
}

// K8sCluster returns the cluster this instance belongs to, and true when it
// belongs to one. Cluster nodes are the k8s-nodes pipeline's targets, never
// this one (§3.3 "Never"): sizing them here would double-count against the
// binpacker and ignore pod-level guardrails entirely.
func (i Instance) K8sCluster() (string, bool) {
	// Sorted iteration: the reported cluster must not depend on map order.
	keys := make([]string, 0, len(i.Tags))
	for k := range i.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == TagEKSCluster || k == TagAWSEKSCluster {
			if v := i.Tags[k]; v != "" {
				return v, true
			}
			return "(unnamed)", true
		}
		if strings.HasPrefix(k, TagK8sClusterPrefix) {
			if name := strings.TrimPrefix(k, TagK8sClusterPrefix); name != "" {
				return name, true
			}
			return "(unnamed)", true
		}
	}
	return "", false
}

// ModeOff reports the kilter.dev/mode=off tag guardrail.
func (i Instance) ModeOff() bool {
	return strings.EqualFold(strings.TrimSpace(i.Tags[TagKilterMode]), "off")
}

// Region derives the region from the availability zone ("us-east-1a" ⇒
// "us-east-1"), falling back to the empty string. Local and wavelength zones
// carry a suffix this does not attempt to parse; callers set Snapshot.Region
// explicitly, and this is only the fallback.
func (i Instance) Region() string {
	az := strings.TrimSpace(i.AvailabilityZone)
	if len(az) < 2 {
		return ""
	}
	last := az[len(az)-1]
	if last >= 'a' && last <= 'z' {
		return az[:len(az)-1]
	}
	return az
}

// Target is one instance plus everything observed about it.
type Target struct {
	Ref      TargetRef `json:"ref"`
	Instance Instance  `json:"instance"`
	// Series is sorted by metric name so iteration is order-independent.
	Series []Series `json:"series,omitempty"`
	// Blind lists declared blind spots ("memory", "disk-space"), sorted.
	Blind []string `json:"blind,omitempty"`
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
// specialized to EC2 targets.
type Snapshot struct {
	Domain    string    `json:"domain"`
	Scope     string    `json:"scope"`
	Region    string    `json:"region,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Window    Window    `json:"window"`
	// Targets is sorted by instance ID.
	Targets []Target `json:"targets,omitempty"`
	// Stale marks a snapshot the collector could not complete — a page budget
	// exhausted, a metric call that returned less than it was asked for. The
	// brain still gets a snapshot (a domain degrades, it does not break the
	// core), but every affected target carries a partial series and the sizer
	// refuses on it.
	Stale bool `json:"stale,omitempty"`
	// Warnings are human-readable collection problems, sorted and deduped.
	Warnings []string `json:"warnings,omitempty"`
}

// sortWarnings deduplicates and sorts, so two collectors that hit the same
// problems in a different order ship the same snapshot.
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

// fmtUSD renders money at a fixed width so golden output does not drift with
// float formatting.
func fmtUSD(v float64) string { return fmt.Sprintf("$%.4f", v) }

// fmtPct renders a 0..100 percentage.
func fmtPct(v float64) string { return fmt.Sprintf("%.1f%%", v) }
