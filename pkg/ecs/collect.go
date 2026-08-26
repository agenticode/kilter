package ecs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
)

// --- Cloud seams -----------------------------------------------------------
//
// Three interfaces over plain Go structs, shaped after the AWS calls they stand
// in for. No SDK type appears here: pkg/provider isolates aws-sdk-go-v2 behind
// an asgAPI-style interface and is wired in cmd/; this package goes one step
// further and does not import the SDK at all, because its decision path must
// link into an air-gapped binary. The adapter that fills these structs from
// *ecs.Client and *cloudwatch.Client is a later unit's cmd/ wiring.
//
// Read and write are deliberately separate interfaces ([InventoryAPI] and
// [MetricsAPI] versus [MutateAPI]) so the collector can be handed credentials
// that physically cannot register a task definition — the observe/actuate IAM
// split of §3.3, expressed in Go.

// Tag is one ECS resource tag, in API order.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// tagMap normalizes a tag list to a map, first occurrence wins.
func tagMap(tags []Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t.Key == "" {
			continue
		}
		if _, dup := out[t.Key]; !dup {
			out[t.Key] = t.Value
		}
	}
	return out
}

// Deployment is one ECS service deployment. A Fargate service with more than
// one live deployment, or with a PRIMARY deployment that has not finished
// rolling out, is mid-change: its metrics mix two task sizes and its service
// definition is being rewritten by someone else.
type Deployment struct {
	ID     string `json:"id"`
	Status string `json:"status"` // PRIMARY | ACTIVE | INACTIVE
	// TaskDefinition is the revision ARN this deployment runs.
	TaskDefinition string `json:"taskDefinition"`
	DesiredCount   int    `json:"desiredCount"`
	RunningCount   int    `json:"runningCount"`
	PendingCount   int    `json:"pendingCount"`
	FailedTasks    int    `json:"failedTasks"`
	// RolloutState is COMPLETED | IN_PROGRESS | FAILED. Empty when the service
	// does not use the rollout-state fields (older deployment controllers).
	RolloutState       string    `json:"rolloutState,omitempty"`
	RolloutStateReason string    `json:"rolloutStateReason,omitempty"`
	CreatedAt          time.Time `json:"createdAt,omitzero"`
	UpdatedAt          time.Time `json:"updatedAt,omitzero"`
}

// Deployment statuses and rollout states.
const (
	DeploymentPrimary    = "PRIMARY"
	DeploymentActive     = "ACTIVE"
	RolloutInProgress    = "IN_PROGRESS"
	RolloutFailed        = "FAILED"
	RolloutCompleted     = "COMPLETED"
	CapacityProviderSpot = "FARGATE_SPOT"
	CapacityProviderOD   = "FARGATE"
	LaunchTypeFargate    = "FARGATE"
)

// CapacityProviderItem is one entry of a service's capacity-provider strategy.
type CapacityProviderItem struct {
	CapacityProvider string `json:"capacityProvider"`
	Weight           int32  `json:"weight,omitempty"`
	Base             int32  `json:"base,omitempty"`
}

// ServiceRecord is one DescribeServices service, flattened. Field names track
// the API's, so a recorded fixture reads like the response it came from.
type ServiceRecord struct {
	ServiceARN  string `json:"serviceArn"`
	ServiceName string `json:"serviceName"`
	ClusterARN  string `json:"clusterArn"`
	Status      string `json:"status,omitempty"` // ACTIVE | DRAINING | INACTIVE
	// LaunchType is FARGATE, EC2 or EXTERNAL, and is empty when the service
	// uses a capacity-provider strategy instead.
	LaunchType               string                 `json:"launchType,omitempty"`
	CapacityProviderStrategy []CapacityProviderItem `json:"capacityProviderStrategy,omitempty"`
	// PlatformVersion is "LATEST" or an explicit version such as "1.4.0".
	PlatformVersion string `json:"platformVersion,omitempty"`
	// PlatformFamily is "Linux" or a "Windows_Server_*" string.
	PlatformFamily string `json:"platformFamily,omitempty"`
	// TaskDefinition is the revision ARN the service is configured with.
	TaskDefinition string       `json:"taskDefinition"`
	DesiredCount   int          `json:"desiredCount"`
	RunningCount   int          `json:"runningCount"`
	PendingCount   int          `json:"pendingCount"`
	Deployments    []Deployment `json:"deployments,omitempty"`
	Tags           []Tag        `json:"tags,omitempty"`
	CreatedAt      time.Time    `json:"createdAt,omitzero"`
}

// ClusterName extracts the cluster name from the cluster ARN, which is the
// CloudWatch `ClusterName` dimension value.
func (s ServiceRecord) ClusterName() string { return arnTail(s.ClusterARN) }

// IsFargate reports whether the service runs on Fargate, by launch type or by
// capacity-provider strategy.
func (s ServiceRecord) IsFargate() bool {
	if strings.EqualFold(s.LaunchType, LaunchTypeFargate) {
		return true
	}
	for _, c := range s.CapacityProviderStrategy {
		if c.CapacityProvider == CapacityProviderOD || c.CapacityProvider == CapacityProviderSpot {
			return true
		}
	}
	return false
}

// UsesSpot reports whether any part of the service's capacity already comes
// from FARGATE_SPOT.
func (s ServiceRecord) UsesSpot() bool {
	for _, c := range s.CapacityProviderStrategy {
		if c.CapacityProvider == CapacityProviderSpot {
			return true
		}
	}
	return false
}

// Primary returns the PRIMARY deployment, if the service reported one.
func (s ServiceRecord) Primary() (Deployment, bool) {
	for _, d := range s.Deployments {
		if d.Status == DeploymentPrimary {
			return d, true
		}
	}
	return Deployment{}, false
}

// DeploymentInProgress reports whether the service is mid-rollout, with the
// reason. It is deliberately generous about what counts: a service that is
// still converging must not be re-pointed at a new revision, because the second
// UpdateService cancels the first mid-flight and nobody can say afterwards
// which revision the tasks came from.
func (s ServiceRecord) DeploymentInProgress() (bool, string) {
	live := 0
	for _, d := range s.Deployments {
		switch d.Status {
		case DeploymentPrimary, DeploymentActive:
			live++
		}
		if d.Status == DeploymentPrimary && d.RolloutState == RolloutInProgress {
			return true, fmt.Sprintf("deployment %s is %s: %s", d.ID, d.RolloutState, d.RolloutStateReason)
		}
		if d.Status == DeploymentPrimary && d.RolloutState == RolloutFailed {
			return true, fmt.Sprintf("deployment %s FAILED: %s", d.ID, d.RolloutStateReason)
		}
	}
	if live > 1 {
		return true, fmt.Sprintf("%d live deployments: the service is mid-rollout", live)
	}
	if p, ok := s.Primary(); ok {
		if p.PendingCount > 0 || p.RunningCount != p.DesiredCount {
			return true, fmt.Sprintf("deployment %s has %d/%d running and %d pending",
				p.ID, p.RunningCount, p.DesiredCount, p.PendingCount)
		}
	}
	if s.PendingCount > 0 || (s.DesiredCount > 0 && s.RunningCount != s.DesiredCount) {
		return true, fmt.Sprintf("service has %d/%d tasks running and %d pending",
			s.RunningCount, s.DesiredCount, s.PendingCount)
	}
	return false, ""
}

// RuntimePlatform is the task definition's runtimePlatform block.
type RuntimePlatform struct {
	CPUArchitecture       string `json:"cpuArchitecture,omitempty"`       // X86_64 | ARM64
	OperatingSystemFamily string `json:"operatingSystemFamily,omitempty"` // LINUX | WINDOWS_SERVER_*
}

// Arch resolves the declared architecture, defaulting to X86_64 as ECS does.
func (r RuntimePlatform) Arch() Arch {
	if strings.EqualFold(r.CPUArchitecture, string(ArchARM64)) {
		return ArchARM64
	}
	return ArchX86
}

// OS resolves the declared operating-system family, defaulting to Linux as ECS
// does.
func (r RuntimePlatform) OS() OSFamily {
	if strings.HasPrefix(strings.ToUpper(r.OperatingSystemFamily), "WINDOWS") {
		return OSWindows
	}
	return OSLinux
}

// ContainerDefinition is the subset of a container definition that constrains
// the task size. ECS rejects a task whose task-level memory is below the sum of
// its containers' hard limits, or whose task-level cpu is below the sum of
// container cpu — so these are hard floors on any proposal, not preferences.
type ContainerDefinition struct {
	Name string `json:"name"`
	// CPU is container-level CPU units (1024 = 1 vCPU); 0 means unset.
	CPU int32 `json:"cpu,omitempty"`
	// Memory is the hard limit in MiB; 0 means unset.
	Memory int32 `json:"memory,omitempty"`
	// MemoryReservation is the soft limit in MiB; 0 means unset.
	MemoryReservation int32 `json:"memoryReservation,omitempty"`
}

// TaskDefinitionRecord is one DescribeTaskDefinition result, flattened.
type TaskDefinitionRecord struct {
	TaskDefinitionARN string `json:"taskDefinitionArn,omitempty"`
	Family            string `json:"family"`
	Revision          int    `json:"revision,omitempty"`
	Status            string `json:"status,omitempty"` // ACTIVE | INACTIVE
	// CPU and Memory are task-level, as the API returns them: "1024"/"1 vCPU"
	// and "2048"/"2GB".
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// NetworkMode must be awsvpc for Fargate [verified: ECS developer guide].
	NetworkMode             string                `json:"networkMode,omitempty"`
	RequiresCompatibilities []string              `json:"requiresCompatibilities,omitempty"`
	RuntimePlatform         RuntimePlatform       `json:"runtimePlatform,omitzero"`
	ContainerDefinitions    []ContainerDefinition `json:"containerDefinitions,omitempty"`
	EphemeralStorageGiB     int                   `json:"ephemeralStorageGiB,omitempty"`
}

// NetworkModeAWSVPC is the only network mode Fargate supports.
const NetworkModeAWSVPC = "awsvpc"

// Reserved parses the task-level cpu/memory into scheduler units.
func (t TaskDefinitionRecord) Reserved() (model.Resources, error) {
	cpu, err := ParseTaskCPU(t.CPU)
	if err != nil {
		return model.Resources{}, err
	}
	mem, err := ParseTaskMemory(t.Memory)
	if err != nil {
		return model.Resources{}, err
	}
	return model.Resources{MilliCPU: cpu, MemoryBytes: mem}, nil
}

// ContainerFloors returns the task-level cpu/memory floor the container
// definitions impose: the sum of container CPU units and the sum of container
// hard memory limits. A proposal below either floor is rejected by
// RegisterTaskDefinition, so it is refused here with a reason instead of being
// sent and failing in the cloud.
func (t TaskDefinitionRecord) ContainerFloors() model.Resources {
	var cpuUnits, memMiB int64
	for _, c := range t.ContainerDefinitions {
		if c.CPU > 0 {
			cpuUnits += int64(c.CPU)
		}
		// The hard limit is the binding one; a soft reservation may exceed the
		// task size without failing registration.
		if c.Memory > 0 {
			memMiB += int64(c.Memory)
		}
	}
	return model.Resources{
		MilliCPU:    cpuUnits * 1000 / 1024,
		MemoryBytes: memMiB << 20,
	}
}

// --- Seam inputs and outputs ----------------------------------------------

// ListServicesInput is the paginating service listing.
type ListServicesInput struct {
	Cluster    string `json:"cluster"`
	NextToken  string `json:"nextToken,omitempty"`
	MaxResults int32  `json:"maxResults,omitempty"`
	LaunchType string `json:"launchType,omitempty"`
}

// ListServicesOutput is one page. An empty NextToken ends pagination.
type ListServicesOutput struct {
	ServiceARNs []string `json:"serviceArns,omitempty"`
	NextToken   string   `json:"nextToken,omitempty"`
}

// DescribeServicesInput describes up to [MaxServicesPerDescribe] services.
type DescribeServicesInput struct {
	Cluster  string   `json:"cluster"`
	Services []string `json:"services"`
	// IncludeTags requests the service tags that carry kilter.dev/mode.
	IncludeTags bool `json:"includeTags,omitempty"`
}

// Failure mirrors the API's per-resource failure entry. A failure is reported,
// never treated as "the service does not exist".
type Failure struct {
	ARN    string `json:"arn,omitempty"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// DescribeServicesOutput is one describe result.
type DescribeServicesOutput struct {
	Services []ServiceRecord `json:"services,omitempty"`
	Failures []Failure       `json:"failures,omitempty"`
}

// DescribeTaskDefinitionInput names one task definition by ARN or family:revision.
type DescribeTaskDefinitionInput struct {
	TaskDefinition string `json:"taskDefinition"`
}

// DescribeTaskDefinitionOutput is one task definition.
type DescribeTaskDefinitionOutput struct {
	TaskDefinition TaskDefinitionRecord `json:"taskDefinition"`
	Tags           []Tag                `json:"tags,omitempty"`
}

// InventoryAPI is the read-only ECS seam: three calls, no mutation, shaped
// after ecs:ListServices, ecs:DescribeServices and ecs:DescribeTaskDefinition.
type InventoryAPI interface {
	ListServices(ctx context.Context, in *ListServicesInput) (*ListServicesOutput, error)
	DescribeServices(ctx context.Context, in *DescribeServicesInput) (*DescribeServicesOutput, error)
	DescribeTaskDefinition(ctx context.Context, in *DescribeTaskDefinitionInput) (*DescribeTaskDefinitionOutput, error)
}

// MetricDataQuery is one GetMetricData query. ID must match CloudWatch's
// `^[a-z][a-zA-Z0-9_]*$`; the collector generates it and uses it — not the
// label, not position — to route results back to services.
type MetricDataQuery struct {
	ID            string            `json:"id"`
	Namespace     string            `json:"namespace"`
	MetricName    string            `json:"metricName"`
	Dimensions    map[string]string `json:"dimensions,omitempty"`
	PeriodSeconds int32             `json:"periodSeconds"`
	Stat          string            `json:"stat"`
	Label         string            `json:"label,omitempty"`
}

// GetMetricDataInput is the batched, paginating metric request.
type GetMetricDataInput struct {
	Queries   []MetricDataQuery `json:"queries"`
	StartTime time.Time         `json:"startTime"`
	EndTime   time.Time         `json:"endTime"`
	NextToken string            `json:"nextToken,omitempty"`
}

// GetMetricData status codes.
const (
	StatusComplete    = "Complete"
	StatusPartialData = "PartialData"
	StatusForbidden   = "Forbidden"
	StatusInternal    = "InternalError"
	// StatusTruncated is this package's own marker, not a CloudWatch code:
	// GetMetricData returns one result per query — with empty values when the
	// metric has no data — so a *missing* result is a fact about the response,
	// not about the metric.
	StatusTruncated = "Truncated"
)

// MetricDataResult is one query's answer. Timestamps and Values are parallel;
// the collector sorts them ascending and does not assume the API did.
type MetricDataResult struct {
	ID         string      `json:"id"`
	Label      string      `json:"label,omitempty"`
	Timestamps []time.Time `json:"timestamps,omitempty"`
	Values     []float64   `json:"values,omitempty"`
	StatusCode string      `json:"statusCode,omitempty"`
}

// GetMetricDataOutput is one page of results.
type GetMetricDataOutput struct {
	Results   []MetricDataResult `json:"results,omitempty"`
	NextToken string             `json:"nextToken,omitempty"`
	Messages  []string           `json:"messages,omitempty"`
}

// MetricsAPI is the GetMetricData-shaped seam.
type MetricsAPI interface {
	GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error)
}

// --- Series ---------------------------------------------------------------

// Window is a closed observation interval.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Span is the window's duration, never negative.
func (w Window) Span() time.Duration {
	if w.End.Before(w.Start) {
		return 0
	}
	return w.End.Sub(w.Start)
}

// Series is one metric's datapoints over a window, ascending by timestamp.
type Series struct {
	Metric        string      `json:"metric"`
	Timestamps    []time.Time `json:"timestamps,omitempty"`
	Values        []float64   `json:"values,omitempty"`
	PeriodSeconds int32       `json:"periodSeconds,omitempty"`
	// StatusCode is CloudWatch's, or [StatusTruncated] when no result came
	// back for the query at all.
	StatusCode string `json:"statusCode,omitempty"`
}

// Len is the datapoint count.
func (s Series) Len() int { return len(s.Values) }

// Complete reports whether CloudWatch returned the whole series.
func (s Series) Complete() bool { return s.StatusCode == "" || s.StatusCode == StatusComplete }

// Span is the time between the first and last datapoint.
func (s Series) Span() time.Duration {
	if len(s.Timestamps) < 2 {
		return 0
	}
	return s.Timestamps[len(s.Timestamps)-1].Sub(s.Timestamps[0])
}

// Last is the newest datapoint's timestamp.
func (s Series) Last() time.Time {
	if len(s.Timestamps) == 0 {
		return time.Time{}
	}
	return s.Timestamps[len(s.Timestamps)-1]
}

// Max is the largest value, 0 for an empty series.
func (s Series) Max() float64 {
	m := 0.0
	for _, v := range s.Values {
		if v > m {
			m = v
		}
	}
	return m
}

// Percentile returns the nearest-rank percentile of the values, p in (0,1].
// The copy is deliberate: a report must not reorder the series it reports.
func (s Series) Percentile(p float64) float64 {
	if len(s.Values) == 0 || !(p > 0) || p > 1 {
		return 0
	}
	v := make([]float64, len(s.Values))
	copy(v, s.Values)
	sort.Float64s(v)
	rank := int(float64(len(v))*p+0.999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(v) {
		rank = len(v) - 1
	}
	return v[rank]
}

// TrimBefore drops datapoints published before t, returning a new Series.
//
// It is what makes revision drift survivable rather than fatal: percentages
// published before the current task definition rolled out are percentages of a
// DIFFERENT reservation, and converting them with the current denominator would
// silently rescale history. They are dropped, and if too few remain, the sizer
// refuses.
func (s Series) TrimBefore(t time.Time) Series {
	if t.IsZero() || len(s.Timestamps) == 0 {
		return s
	}
	i := 0
	for i < len(s.Timestamps) && s.Timestamps[i].Before(t) {
		i++
	}
	if i == 0 {
		return s
	}
	out := s
	out.Timestamps = s.Timestamps[i:]
	out.Values = s.Values[i:]
	return out
}

// sortAscending orders a series by timestamp and drops non-finite values.
func (s *Series) sortAscending() {
	n := min(len(s.Timestamps), len(s.Values))
	idx := make([]int, 0, n)
	for i := range n {
		v := s.Values[i]
		if v != v || v > 1e18 || v < -1e18 || s.Timestamps[i].IsZero() {
			continue // NaN, Inf-ish, or an unstamped datapoint: not evidence
		}
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return s.Timestamps[idx[a]].Before(s.Timestamps[idx[b]])
	})
	ts := make([]time.Time, len(idx))
	vs := make([]float64, len(idx))
	for i, j := range idx {
		ts[i], vs[i] = s.Timestamps[j], s.Values[j]
	}
	s.Timestamps, s.Values = ts, vs
}

// --- Observation and snapshot ---------------------------------------------

// Observation is everything known about one ECS Fargate service at collection
// time: what it reserves, what it is using as a percentage of that, and the
// facts that decide whether either number may be acted on.
type Observation struct {
	Ref     domain.TargetRef     `json:"ref"`
	Service ServiceRecord        `json:"service"`
	TaskDef TaskDefinitionRecord `json:"taskDefinition"`
	// Reserved is the DENOMINATOR of CPUPercent and MemPercent, carried with
	// the revision it came from so a conversion can never be applied to
	// percentages a different revision produced.
	Reserved Reservation `json:"reserved"`
	// CPUPercent and MemPercent are percent-of-reserved, never absolute.
	CPUPercent Series `json:"cpuPercent"`
	MemPercent Series `json:"memPercent"`
	Window     Window `json:"window"`
	// Tags is the service's tag map (guardrail source).
	Tags map[string]string `json:"tags,omitempty"`
	// Notes are collection-time facts a reader needs: a truncated response, a
	// trimmed window, a task definition that could not be read.
	Notes []string `json:"notes,omitempty"`
	// Blind lists dimensions with no usable signal at all.
	Blind []string `json:"blind,omitempty"`
}

// Snapshot is one collection pass over one cluster.
type Snapshot struct {
	Domain    domain.Kind   `json:"domain"`
	Scope     string        `json:"scope"`
	Cluster   string        `json:"cluster"`
	Timestamp time.Time     `json:"timestamp"`
	Window    Window        `json:"window"`
	Services  []Observation `json:"services,omitempty"`
	// Stale marks a partial collection: a page budget exhausted, a describe
	// failure, a truncated metric response. The brain still learns from what
	// arrived; the sizer refuses on the affected services individually.
	Stale       bool     `json:"stale,omitempty"`
	StaleReason string   `json:"staleReason,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// --- Collector -------------------------------------------------------------

// API limits, as documented by the calls they bound.
const (
	// MaxServicesPerDescribe is DescribeServices' per-call limit.
	MaxServicesPerDescribe = 10
	// MaxSeriesPerCall is GetMetricData's per-call query limit.
	MaxSeriesPerCall = 500
	// DefaultMaxPages bounds pagination on every seam. A server that keeps
	// handing back a token is a bug, not a big account; the budget turns an
	// unbounded loop into a stale-marked snapshot.
	DefaultMaxPages = 200
	// DefaultWindow is the metric lookback.
	DefaultWindow = 14 * 24 * time.Hour
	// MaxWindow is the longest lookback that still returns 1-minute data:
	// CloudWatch retains 60-second datapoints for 15 days and aggregates older
	// ones to coarser periods, so a longer window silently changes resolution.
	MaxWindow = 15 * 24 * time.Hour
)

// CollectorConfig configures one cluster's collection.
type CollectorConfig struct {
	// Cluster is the ECS cluster name or ARN to list services in. Required.
	Cluster string
	// Scope is the snapshot scope, conventionally "accountID/region".
	Scope string
	// Window is the metric lookback. Zero means DefaultWindow; anything above
	// MaxWindow is clamped, with a warning, rather than silently downgraded to
	// 5-minute data.
	Window time.Duration
	// MaxSeriesPerCall caps the metric batch. Zero means MaxSeriesPerCall.
	MaxSeriesPerCall int
	// MaxPages bounds pagination. Zero means DefaultMaxPages.
	MaxPages int
	// IncludeNonFargate collects services that are not on Fargate. Off by
	// default: this domain prices Fargate tiers, and an EC2-launch-type service
	// belongs to the node domain.
	IncludeNonFargate bool
}

func (c CollectorConfig) window() (time.Duration, string) {
	w := c.Window
	if w <= 0 {
		w = DefaultWindow
	}
	if w > MaxWindow {
		return MaxWindow, fmt.Sprintf(
			"window %s exceeds CloudWatch's %s retention of 1-minute datapoints; clamped to %s to keep the resolution",
			w, MaxWindow, MaxWindow)
	}
	return w, ""
}

func (c CollectorConfig) maxPages() int {
	if c.MaxPages > 0 {
		return c.MaxPages
	}
	return DefaultMaxPages
}

func (c CollectorConfig) batchSize() int {
	if c.MaxSeriesPerCall > 0 && c.MaxSeriesPerCall <= MaxSeriesPerCall {
		return c.MaxSeriesPerCall
	}
	return MaxSeriesPerCall
}

// Collector turns the read seams into one [Snapshot]. It holds no clock, no
// global state and no mutable configuration; two Collectors over the same
// fixtures produce identical snapshots.
type Collector struct {
	inv InventoryAPI
	met MetricsAPI
	cfg CollectorConfig
}

// NewCollector builds a collector. Both seams are required: a nil metrics seam
// would produce a snapshot of services with no evidence at all, which the sizer
// would refuse on anyway — failing here says why, once.
func NewCollector(inv InventoryAPI, met MetricsAPI, cfg CollectorConfig) (*Collector, error) {
	if inv == nil {
		return nil, fmt.Errorf("ecs: collector needs an inventory seam")
	}
	if met == nil {
		return nil, fmt.Errorf("ecs: collector needs a metrics seam")
	}
	if strings.TrimSpace(cfg.Cluster) == "" {
		return nil, fmt.Errorf("ecs: collector needs a cluster name")
	}
	if cfg.Window < 0 {
		return nil, fmt.Errorf("ecs: collector window must not be negative (got %s)", cfg.Window)
	}
	if cfg.MaxSeriesPerCall > MaxSeriesPerCall {
		return nil, fmt.Errorf("ecs: MaxSeriesPerCall %d exceeds the GetMetricData limit of %d",
			cfg.MaxSeriesPerCall, MaxSeriesPerCall)
	}
	return &Collector{inv: inv, met: met, cfg: cfg}, nil
}

// Domain names the domain this collector feeds.
func (c *Collector) Domain() domain.Kind { return Kind }

// Collect reads services, their task definitions, and their default CloudWatch
// metrics for the window ending at now.
//
// It never returns a nil snapshot alongside a nil error, and it never blocks
// the brain on a partial cloud: an exhausted page budget, a per-service
// describe failure, or a truncated metric response marks the snapshot stale and
// the affected series incomplete, rather than failing the pass. A transport
// error from a seam IS returned, because a failed call is not evidence of an
// empty cluster.
func (c *Collector) Collect(ctx context.Context, now time.Time) (*Snapshot, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("ecs: collect needs a caller-supplied now (this package has no clock)")
	}
	win, warn := c.cfg.window()
	snap := &Snapshot{
		Domain:    Kind,
		Scope:     c.cfg.Scope,
		Cluster:   c.cfg.Cluster,
		Timestamp: now,
		Window:    Window{Start: now.Add(-win), End: now},
	}
	if warn != "" {
		snap.Warnings = append(snap.Warnings, warn)
	}

	arns, stale, err := c.listServices(ctx)
	if err != nil {
		return nil, err
	}
	if stale {
		snap.Stale, snap.StaleReason = true, "service listing hit the page budget; the inventory is partial"
	}

	services, failures, err := c.describeServices(ctx, arns)
	if err != nil {
		return nil, err
	}
	for _, f := range failures {
		snap.Stale = true
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("describe-services failure for %s: %s %s", f.ARN, f.Reason, f.Detail))
	}
	if snap.Stale && snap.StaleReason == "" {
		snap.StaleReason = "one or more services could not be described"
	}

	// Task definitions, deduplicated by ARN. The cache is a local, so two
	// collections cannot share (or race on) it.
	tds := map[string]TaskDefinitionRecord{}
	obs := make([]Observation, 0, len(services))
	for _, s := range services {
		if !c.cfg.IncludeNonFargate && !s.IsFargate() {
			continue
		}
		o := Observation{
			Ref: domain.TargetRef{
				Domain: Kind,
				Scope:  c.cfg.Scope,
				ID:     TargetID(s.ClusterName(), s.ServiceName),
				Name:   s.ServiceName,
			},
			Service: s,
			Window:  snap.Window,
			Tags:    tagMap(s.Tags),
		}
		tdARN := s.TaskDefinition
		if p, ok := s.Primary(); ok && p.TaskDefinition != "" {
			// The PRIMARY deployment is what is actually running; the service's
			// own field can already name a revision that has not rolled out.
			tdARN = p.TaskDefinition
		}
		td, ok := tds[tdARN]
		if !ok {
			out, err := c.inv.DescribeTaskDefinition(ctx, &DescribeTaskDefinitionInput{TaskDefinition: tdARN})
			if err != nil {
				return nil, fmt.Errorf("ecs: describe task definition %s: %w", tdARN, err)
			}
			if out == nil {
				return nil, fmt.Errorf("ecs: describe task definition %s: seam returned no result", tdARN)
			}
			td = out.TaskDefinition
			tds[tdARN] = td
		}
		o.TaskDef = td
		if res, err := td.Reserved(); err == nil {
			o.Reserved = Reservation{Revision: td.Revision, ARN: td.TaskDefinitionARN, Reserved: res}
		} else {
			o.Notes = append(o.Notes, fmt.Sprintf(
				"task definition %s declares an unreadable size (cpu=%q memory=%q): %v",
				tdARN, td.CPU, td.Memory, err))
			o.Blind = append(o.Blind, "reservation")
		}
		obs = append(obs, o)
	}

	// Metrics last: one batched pass over every observed service, two series
	// each. Routing is by query ID, never by position.
	if err := c.fetchMetrics(ctx, snap, obs); err != nil {
		return nil, err
	}

	sort.SliceStable(obs, func(i, j int) bool { return obs[i].Ref.ID < obs[j].Ref.ID })
	snap.Services = obs
	return snap, nil
}

// listServices pages through ListServices, returning the ARNs in API order.
func (c *Collector) listServices(ctx context.Context) ([]string, bool, error) {
	var out []string
	token := ""
	for page := 0; ; page++ {
		if page >= c.cfg.maxPages() {
			return out, true, nil
		}
		res, err := c.inv.ListServices(ctx, &ListServicesInput{Cluster: c.cfg.Cluster, NextToken: token})
		if err != nil {
			return nil, false, fmt.Errorf("ecs: list services: %w", err)
		}
		if res == nil {
			return nil, false, fmt.Errorf("ecs: list services: seam returned no result")
		}
		out = append(out, res.ServiceARNs...)
		if res.NextToken == "" || res.NextToken == token {
			return out, false, nil
		}
		token = res.NextToken
	}
}

// describeServices batches DescribeServices at the API's limit.
func (c *Collector) describeServices(ctx context.Context, arns []string) ([]ServiceRecord, []Failure, error) {
	var out []ServiceRecord
	var failures []Failure
	for i := 0; i < len(arns); i += MaxServicesPerDescribe {
		batch := arns[i:min(i+MaxServicesPerDescribe, len(arns))]
		res, err := c.inv.DescribeServices(ctx, &DescribeServicesInput{
			Cluster: c.cfg.Cluster, Services: batch, IncludeTags: true,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ecs: describe services: %w", err)
		}
		if res == nil {
			return nil, nil, fmt.Errorf("ecs: describe services: seam returned no result")
		}
		out = append(out, res.Services...)
		failures = append(failures, res.Failures...)
	}
	return out, failures, nil
}

// queryID builds a CloudWatch-legal, collision-free query ID.
func queryID(kind string, i int) string { return kind + strconv.Itoa(i) }

// fetchMetrics fills each observation's two series. Results are routed by query
// ID; a query with no result at all is marked [StatusTruncated], which the
// sizer treats as a refusal-grade fact rather than as "no usage".
func (c *Collector) fetchMetrics(ctx context.Context, snap *Snapshot, obs []Observation) error {
	type slot struct {
		idx    int
		metric string
	}
	queries := make([]MetricDataQuery, 0, 2*len(obs))
	slots := make(map[string]slot, 2*len(obs))
	for i := range obs {
		s := obs[i].Service
		dims := map[string]string{DimClusterName: s.ClusterName(), DimServiceName: s.ServiceName}
		for _, m := range []struct {
			kind, name string
		}{
			{"cpu", MetricCPUUtilization},
			{"mem", MetricMemoryUtilization},
		} {
			id := queryID(m.kind, i)
			slots[id] = slot{idx: i, metric: m.name}
			queries = append(queries, MetricDataQuery{
				ID:         id,
				Namespace:  Namespace,
				MetricName: m.name,
				Dimensions: dims,
				// ECS publishes service metrics every 60 s at no charge; asking
				// for less resolution than exists would throw away evidence,
				// and asking for more would return empty datapoints.
				PeriodSeconds: PeriodSeconds,
				// Average across the service's tasks. The percentage is
				// already relative to the reservation, so the average is
				// per-task demand, not a total.
				Stat:  "Average",
				Label: s.ServiceName + "/" + m.name,
			})
		}
		obs[i].CPUPercent = Series{Metric: MetricCPUUtilization, PeriodSeconds: PeriodSeconds, StatusCode: StatusTruncated}
		obs[i].MemPercent = Series{Metric: MetricMemoryUtilization, PeriodSeconds: PeriodSeconds, StatusCode: StatusTruncated}
	}

	for i := 0; i < len(queries); i += c.cfg.batchSize() {
		batch := queries[i:min(i+c.cfg.batchSize(), len(queries))]
		token := ""
		for page := 0; ; page++ {
			if page >= c.cfg.maxPages() {
				snap.Stale = true
				snap.Warnings = append(snap.Warnings, "metric pagination hit the page budget; some series are partial")
				break
			}
			res, err := c.met.GetMetricData(ctx, &GetMetricDataInput{
				Queries:   batch,
				StartTime: snap.Window.Start,
				EndTime:   snap.Window.End,
				NextToken: token,
			})
			if err != nil {
				return fmt.Errorf("ecs: get metric data: %w", err)
			}
			if res == nil {
				return fmt.Errorf("ecs: get metric data: seam returned no result")
			}
			snap.Warnings = append(snap.Warnings, res.Messages...)
			for _, r := range res.Results {
				sl, ok := slots[r.ID]
				if !ok {
					snap.Warnings = append(snap.Warnings, "unroutable metric result id "+r.ID)
					continue
				}
				tgt := &obs[sl.idx].CPUPercent
				if sl.metric == MetricMemoryUtilization {
					tgt = &obs[sl.idx].MemPercent
				}
				// A paginated series arrives in pieces; append, then sort.
				if tgt.StatusCode == StatusTruncated {
					tgt.StatusCode = r.StatusCode
				} else if r.StatusCode != "" && r.StatusCode != StatusComplete {
					tgt.StatusCode = r.StatusCode
				}
				tgt.Timestamps = append(tgt.Timestamps, r.Timestamps...)
				tgt.Values = append(tgt.Values, r.Values...)
			}
			if res.NextToken == "" || res.NextToken == token {
				break
			}
			token = res.NextToken
		}
	}

	for i := range obs {
		obs[i].CPUPercent.sortAscending()
		obs[i].MemPercent.sortAscending()
		for _, s := range []Series{obs[i].CPUPercent, obs[i].MemPercent} {
			if !s.Complete() {
				obs[i].Notes = append(obs[i].Notes,
					fmt.Sprintf("%s returned %s: the series is not the whole window", s.Metric, s.StatusCode))
			}
		}
	}
	return nil
}

// TargetID renders a service as a TargetRef.ID: "cluster/service". Neither an
// ECS cluster name nor a service name may contain "/", so the split is
// unambiguous.
func TargetID(cluster, service string) string { return cluster + "/" + service }

// ParseTargetID is the inverse of [TargetID].
func ParseTargetID(id string) (cluster, service string, err error) {
	cluster, service, ok := strings.Cut(id, "/")
	if !ok || cluster == "" || service == "" {
		return "", "", fmt.Errorf("ecs: malformed target ID %q (want cluster/service)", id)
	}
	return cluster, service, nil
}

// arnTail returns the last "/"-separated element of an ARN, or the input when
// it is already a bare name.
func arnTail(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// platformVersionAtLeast reports whether an ECS Fargate platform version is at
// least major.minor.
//
// "LATEST" and "" (the API default) are treated as current, which is what AWS
// resolves them to. An unparseable version reads as too old: refusing a
// proposal because a version string was unreadable is recoverable, proposing an
// 8-vCPU task onto platform version 1.3.0 is not.
func platformVersionAtLeast(pv string, major, minor int) bool {
	t := strings.TrimSpace(strings.ToUpper(pv))
	if t == "" || t == "LATEST" {
		return true
	}
	parts := strings.Split(t, ".")
	if len(parts) < 2 {
		return false
	}
	gotMajor, err1 := strconv.Atoi(parts[0])
	gotMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if gotMajor != major {
		return gotMajor > major
	}
	return gotMinor >= minor
}
