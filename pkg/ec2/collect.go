package ec2

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// --- Cloud seams -----------------------------------------------------------
//
// Two interfaces, two read operations, shaped after the AWS calls they stand
// in for, over plain Go structs. No SDK type appears here: pkg/provider
// isolates aws-sdk-go-v2 behind an asgAPI-style interface and is wired in
// cmd/; this package goes one step further and does not import the SDK at all,
// because its decision path must link into an air-gapped binary. The adapter
// that fills these structs from *ec2.Client and *cloudwatch.Client is a later
// unit's cmd/ wiring (see FINDINGS.md).

// Tag is one DescribeInstances tag, in API order. Kept as a list rather than a
// map because that is what the API returns and what a recorded fixture holds;
// the collector normalizes to a map with first-occurrence-wins.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// InstanceRecord is one DescribeInstances instance, flattened. Field names
// track the API's, so a recorded fixture reads like the response it came from.
type InstanceRecord struct {
	InstanceID       string    `json:"instanceId"`
	InstanceType     string    `json:"instanceType"`
	Architecture     string    `json:"architecture,omitempty"` // x86_64 | arm64
	Platform         string    `json:"platform,omitempty"`     // "windows" or empty for Linux
	PlatformDetails  string    `json:"platformDetails,omitempty"`
	Tenancy          string    `json:"tenancy,omitempty"`
	AvailabilityZone string    `json:"availabilityZone,omitempty"`
	State            string    `json:"state,omitempty"`
	LaunchTime       time.Time `json:"launchTime,omitempty"`
	Tags             []Tag     `json:"tags,omitempty"`
	// MonitoringState is "disabled" (basic, 5-minute) or "enabled" (detailed,
	// 1-minute). Anything else is treated as basic: an unrecognized value must
	// not silently buy us resolution we do not have.
	MonitoringState string `json:"monitoringState,omitempty"`
	// CPUCredits is the T-family credit specification, "standard" or
	// "unlimited", from DescribeInstanceCreditSpecifications or the instance's
	// creditSpecification. Empty means unknown.
	CPUCredits string `json:"cpuCredits,omitempty"`
	// InstanceStoreVolumes counts attached ephemeral volumes.
	InstanceStoreVolumes int `json:"instanceStoreVolumes,omitempty"`
}

// Reservation mirrors the DescribeInstances grouping.
type Reservation struct {
	ReservationID string           `json:"reservationId,omitempty"`
	Instances     []InstanceRecord `json:"instances,omitempty"`
}

// DescribeInstancesInput is the paginating request.
type DescribeInstancesInput struct {
	NextToken  string `json:"nextToken,omitempty"`
	MaxResults int32  `json:"maxResults,omitempty"`
}

// DescribeInstancesOutput is one page. An empty NextToken ends pagination.
type DescribeInstancesOutput struct {
	Reservations []Reservation `json:"reservations,omitempty"`
	NextToken    string        `json:"nextToken,omitempty"`
}

// InventoryAPI is the DescribeInstances-shaped seam.
type InventoryAPI interface {
	DescribeInstances(ctx context.Context, in *DescribeInstancesInput) (*DescribeInstancesOutput, error)
}

// MetricDataQuery is one GetMetricData query. ID must match CloudWatch's
// `^[a-z][a-zA-Z0-9_]*$`; the collector generates and validates it, and uses
// it — not the label, not position — to route results back to targets.
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
	// metric has no data — so a *missing* result means the response was
	// truncated, and that is a fact about the response, not about the metric.
	StatusTruncated = "Truncated"
)

// MetricDataResult is one query's answer. Timestamps and Values are parallel
// and ascending; a StatusCode other than Complete means CloudWatch did not
// return the whole series, which this package treats as a refusal-grade fact,
// not a rounding error.
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

// --- Collector -------------------------------------------------------------

// Collector limits, as documented by the APIs they bound.
const (
	// MaxSeriesPerCall is GetMetricData's per-call query limit (§3.3).
	MaxSeriesPerCall = 500
	// DefaultMaxPages bounds pagination on both seams. A server that keeps
	// handing back a token is a bug, not a big account; the budget turns an
	// unbounded loop into a stale-marked snapshot.
	DefaultMaxPages = 200
	// DefaultWindow is the observation lookback.
	DefaultWindow = 14 * 24 * time.Hour
)

// CollectorConfig configures one region's collection.
type CollectorConfig struct {
	// Scope is the snapshot scope, conventionally "accountID/region".
	Scope string
	// Region labels the snapshot; when empty it is derived from the first
	// instance's availability zone.
	Region string
	// Window is the metric lookback. Zero means DefaultWindow.
	Window time.Duration
	// PreferredPeriodSeconds is the period requested for instances with
	// detailed monitoring. Zero means PeriodDetailedSeconds. Instances without
	// detailed monitoring are always requested at PeriodBasicSeconds: asking
	// for 60 s where CloudWatch publishes every 300 s returns 300 s data with
	// four fifths of the datapoints empty, which reads as a coverage failure
	// rather than as the resolution limit it is.
	PreferredPeriodSeconds int32
	// MaxSeriesPerCall caps the batch size. Zero means MaxSeriesPerCall.
	MaxSeriesPerCall int
	// MaxPages bounds pagination on both seams. Zero means DefaultMaxPages.
	MaxPages int
	// CollectMemory requests the CloudWatch agent's mem_used_percent series.
	// It costs one query per instance and is usually absent; when it is
	// absent the instance is memory-blind and the sizer says so.
	CollectMemory bool
	// IncludeStates lists instance states to collect, lowercased. Empty means
	// {"running"}: a stopped instance bills nothing per hour and has no
	// current CPU to size against.
	IncludeStates []string
	// Batch is the OPTIONAL AWS Batch seam (batchenrich.go). Nil — the zero
	// value — is the supported default and produces exactly the snapshot this
	// collector produced before the seam existed. When set, two read-only
	// describes enrich the report with compute-environment advisories; a seam
	// that errors degrades to a warning, never to a failed collection.
	Batch BatchAPI
}

func (c CollectorConfig) window() time.Duration {
	if c.Window > 0 {
		return c.Window
	}
	return DefaultWindow
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

func (c CollectorConfig) states() []string {
	if len(c.IncludeStates) == 0 {
		return []string{"running"}
	}
	out := make([]string, 0, len(c.IncludeStates))
	for _, s := range c.IncludeStates {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	sort.Strings(out)
	return out
}

// periodFor applies the resolution rule: detailed monitoring buys 1-minute
// datapoints, its absence pins publication at 5 minutes, and no request
// parameter changes that.
func (c CollectorConfig) periodFor(detailed bool) int32 {
	if !detailed {
		return PeriodBasicSeconds
	}
	if c.PreferredPeriodSeconds > 0 {
		return c.PreferredPeriodSeconds
	}
	return PeriodDetailedSeconds
}

// Collector turns the two read seams into one [Snapshot]. It holds no clock,
// no global state and no mutable configuration; two Collectors over the same
// fixtures produce byte-identical snapshots.
type Collector struct {
	inv InventoryAPI
	met MetricsAPI
	cfg CollectorConfig
}

// NewCollector builds a collector. Both seams are required: a nil metrics seam
// would produce a snapshot of instances with no evidence at all, which the
// sizer would refuse on anyway — failing here says why, once.
func NewCollector(inv InventoryAPI, met MetricsAPI, cfg CollectorConfig) (*Collector, error) {
	if inv == nil {
		return nil, fmt.Errorf("ec2: collector needs an inventory seam")
	}
	if met == nil {
		return nil, fmt.Errorf("ec2: collector needs a metrics seam")
	}
	if cfg.Window < 0 {
		return nil, fmt.Errorf("ec2: collector window must not be negative (got %s)", cfg.Window)
	}
	if cfg.MaxSeriesPerCall > MaxSeriesPerCall {
		return nil, fmt.Errorf("ec2: MaxSeriesPerCall %d exceeds the GetMetricData limit of %d",
			cfg.MaxSeriesPerCall, MaxSeriesPerCall)
	}
	return &Collector{inv: inv, met: met, cfg: cfg}, nil
}

// Domain names the domain this collector feeds.
func (c *Collector) Domain() string { return Domain }

// Collect reads inventory, then metrics, and returns a snapshot for the window
// ending at now. It never returns a nil snapshot alongside a nil error, and it
// never blocks the brain on a partial cloud: a page budget exhausted or a
// short metric response marks the snapshot stale and the affected series
// partial instead of failing. A transport error from a seam is returned as an
// error, because a failed call is not evidence of an empty account.
func (c *Collector) Collect(ctx context.Context, now time.Time) (*Snapshot, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("ec2: collect needs a caller-supplied now (this package has no clock)")
	}
	win := Window{Start: now.Add(-c.cfg.window()), End: now}
	snap := &Snapshot{
		Domain:    Domain,
		Scope:     c.cfg.Scope,
		Region:    c.cfg.Region,
		Timestamp: now,
		Window:    win,
	}
	var warns []string

	instances, invWarns, stale, err := c.describeInstances(ctx)
	if err != nil {
		return nil, err
	}
	warns = append(warns, invWarns...)
	snap.Stale = snap.Stale || stale

	if snap.Region == "" {
		for _, in := range instances {
			if r := in.Region(); r != "" {
				snap.Region = r
				break
			}
		}
	}

	targets := make([]Target, 0, len(instances))
	for _, in := range instances {
		targets = append(targets, Target{
			Ref: TargetRef{
				Domain: Domain,
				Scope:  c.cfg.Scope,
				ID:     in.ID,
				Name:   in.Name(),
			},
			Instance: in,
		})
	}
	// Sorted by instance ID before metrics are requested: query IDs are
	// positional, so a stable target order is what makes the request — and the
	// whole snapshot — reproducible.
	sort.Slice(targets, func(i, j int) bool { return targets[i].Ref.ID < targets[j].Ref.ID })

	metWarns, metStale, err := c.collectMetrics(ctx, targets, win)
	if err != nil {
		return nil, err
	}
	warns = append(warns, metWarns...)
	snap.Stale = snap.Stale || metStale

	for i := range targets {
		sort.Slice(targets[i].Series, func(a, b int) bool {
			return targets[i].Series[a].Metric < targets[i].Series[b].Metric
		})
		targets[i].Blind = blindSpots(targets[i])
	}
	snap.Targets = targets

	// The optional Batch enrichment, last and deliberately consequence-free:
	// it adds report-scope advisories and cannot mark a target partial, mark
	// the snapshot stale, or fail the collection. A permissions gap on
	// batch:Describe* costs the operator three insights, not their EC2 report.
	if c.cfg.Batch != nil {
		bi, batchWarns := collectBatch(ctx, c.cfg.Batch, c.cfg.maxPages())
		snap.Batch = bi
		warns = append(warns, batchWarns...)
	}

	snap.Warnings = sortWarnings(warns)
	return snap, nil
}

// blindSpots declares what could not be observed for this target. "memory" is
// the one that changes decisions; "disk-space" is declared because EBS metrics
// are I/O, never fill, and no later unit should mistake their presence for a
// capacity signal.
func blindSpots(t Target) []string {
	var out []string
	if s, ok := t.SeriesFor(MetricMemUsedPercent); !ok || len(s.Points) == 0 {
		out = append(out, "memory")
	}
	out = append(out, "disk-space")
	sort.Strings(out)
	return out
}

func (c *Collector) describeInstances(ctx context.Context) ([]Instance, []string, bool, error) {
	want := map[string]bool{}
	for _, s := range c.cfg.states() {
		want[s] = true
	}
	var (
		out   []Instance
		warns []string
		token string
		seen  = map[string]bool{}
	)
	for page := 0; ; page++ {
		if page >= c.cfg.maxPages() {
			warns = append(warns, fmt.Sprintf(
				"inventory pagination stopped after %d pages; snapshot is incomplete", c.cfg.maxPages()))
			return out, warns, true, nil
		}
		resp, err := c.inv.DescribeInstances(ctx, &DescribeInstancesInput{NextToken: token})
		if err != nil {
			return nil, nil, false, fmt.Errorf("ec2: describe instances (page %d): %w", page, err)
		}
		if resp == nil {
			return nil, nil, false, fmt.Errorf("ec2: describe instances (page %d): nil response", page)
		}
		for _, r := range resp.Reservations {
			for _, rec := range r.Instances {
				if rec.InstanceID == "" {
					warns = append(warns, "skipped an instance record with no instance ID")
					continue
				}
				if seen[rec.InstanceID] {
					// Duplicate across pages: keep the first, say so. Silently
					// keeping both would double-count the account's usage in
					// the commitment waterfall.
					warns = append(warns, fmt.Sprintf("instance %s appeared on more than one page; kept the first", rec.InstanceID))
					continue
				}
				seen[rec.InstanceID] = true
				in := normalizeInstance(rec)
				if st := strings.ToLower(in.State); st != "" && !want[st] {
					continue
				}
				out = append(out, in)
			}
		}
		if resp.NextToken == "" {
			break
		}
		if resp.NextToken == token {
			// A token that does not advance is a broken pager, not a big
			// account. Loudly, once.
			return nil, nil, false, fmt.Errorf("ec2: describe instances: pagination token did not advance (%q)", token)
		}
		token = resp.NextToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, warns, false, nil
}

// normalizeInstance maps one API record onto the decision vocabulary,
// preferring "unknown" over an optimistic default everywhere it matters.
func normalizeInstance(r InstanceRecord) Instance {
	in := Instance{
		ID:                 r.InstanceID,
		InstanceType:       strings.TrimSpace(r.InstanceType),
		Architecture:       normalizeArch(r.Architecture),
		Platform:           normalizePlatform(r.Platform, r.PlatformDetails),
		Tenancy:            strings.ToLower(strings.TrimSpace(r.Tenancy)),
		AvailabilityZone:   strings.TrimSpace(r.AvailabilityZone),
		State:              strings.ToLower(strings.TrimSpace(r.State)),
		LaunchTime:         r.LaunchTime,
		DetailedMonitoring: strings.EqualFold(strings.TrimSpace(r.MonitoringState), "enabled"),
		CreditMode:         strings.ToLower(strings.TrimSpace(r.CPUCredits)),
		InstanceStore:      r.InstanceStoreVolumes > 0,
	}
	if len(r.Tags) > 0 {
		in.Tags = make(map[string]string, len(r.Tags))
		for _, t := range r.Tags {
			if t.Key == "" {
				continue
			}
			if _, dup := in.Tags[t.Key]; dup {
				continue // first occurrence wins; deterministic regardless of order
			}
			in.Tags[t.Key] = t.Value
		}
	}
	if in.Tenancy == "" {
		in.Tenancy = "default"
	}
	return in
}

// normalizeArch maps EC2 architecture strings onto the catalog's vocabulary.
// An unrecognized value stays empty rather than defaulting to amd64: guessing
// the architecture is how an ARM instance gets priced as x86.
func normalizeArch(a string) string {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "x86_64", "amd64", "x86_64_mac":
		return "amd64"
	case "arm64", "aarch64", "arm64_mac":
		return "arm64"
	}
	return ""
}

// normalizePlatform maps to the commitment package's platform vocabulary.
// Reserved Instance size flexibility applies only to Linux/UNIX, so getting
// this wrong changes the bill math, not just a label.
func normalizePlatform(platform, details string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	d := strings.ToLower(strings.TrimSpace(details))
	switch {
	case p == "windows" || strings.Contains(d, "windows"):
		return "Windows"
	case strings.Contains(d, "red hat"), strings.Contains(d, "rhel"):
		return "Red Hat Enterprise Linux"
	case strings.Contains(d, "suse"):
		return "SUSE Linux"
	}
	return "Linux/UNIX"
}

// metricPlan is one requested series, with the routing information the
// response needs to be reassembled.
type metricPlan struct {
	queryID   string
	targetIdx int
	metric    string
	namespace string
	stat      string
	period    int32
}

// plannedQueries enumerates the series to request, in a deterministic order:
// targets by instance ID (already sorted), metrics by name within a target.
func (c *Collector) plannedQueries(targets []Target) []metricPlan {
	var plans []metricPlan
	for i, t := range targets {
		metrics := []struct {
			name, ns, stat string
		}{
			// Maximum, not Average: at 60 s it is the peak; at 300 s it is
			// identical to Average because basic monitoring publishes one
			// datapoint per window — which is the honest shape of the limit.
			{MetricCPUUtilization, NamespaceEC2, "Maximum"},
		}
		if IsBurstable(t.Instance.InstanceType) {
			metrics = append(metrics,
				struct{ name, ns, stat string }{MetricCPUCreditBalance, NamespaceEC2, "Minimum"},
				struct{ name, ns, stat string }{MetricCPUCreditUsage, NamespaceEC2, "Sum"},
				struct{ name, ns, stat string }{MetricCPUSurplusCreditBalance, NamespaceEC2, "Maximum"},
				struct{ name, ns, stat string }{MetricCPUSurplusCreditsCharged, NamespaceEC2, "Sum"},
			)
		}
		if c.cfg.CollectMemory {
			metrics = append(metrics,
				struct{ name, ns, stat string }{MetricMemUsedPercent, NamespaceCWAgent, "Maximum"})
		}
		sort.Slice(metrics, func(a, b int) bool { return metrics[a].name < metrics[b].name })
		period := c.cfg.periodFor(t.Instance.DetailedMonitoring)
		for _, m := range metrics {
			plans = append(plans, metricPlan{
				queryID:   fmt.Sprintf("m%d_%d", i, len(plans)),
				targetIdx: i,
				metric:    m.name,
				namespace: m.ns,
				stat:      m.stat,
				period:    period,
			})
		}
	}
	return plans
}

func (c *Collector) collectMetrics(ctx context.Context, targets []Target, win Window) ([]string, bool, error) {
	plans := c.plannedQueries(targets)
	if len(plans) == 0 {
		return nil, false, nil
	}
	byID := make(map[string]metricPlan, len(plans))
	for _, p := range plans {
		byID[p.queryID] = p
	}
	// series[targetIdx][metric] accumulates results across pages.
	series := make([]map[string]*Series, len(targets))
	for i := range series {
		series[i] = map[string]*Series{}
	}

	var (
		warns    []string
		stale    bool
		batch    = c.cfg.batchSize()
		answered = map[string]bool{}
	)
	for start := 0; start < len(plans); start += batch {
		end := start + batch
		if end > len(plans) {
			end = len(plans)
		}
		chunk := plans[start:end]
		queries := make([]MetricDataQuery, 0, len(chunk))
		for _, p := range chunk {
			queries = append(queries, MetricDataQuery{
				ID:            p.queryID,
				Namespace:     p.namespace,
				MetricName:    p.metric,
				Dimensions:    map[string]string{"InstanceId": targets[p.targetIdx].Ref.ID},
				PeriodSeconds: p.period,
				Stat:          p.stat,
				Label:         targets[p.targetIdx].Ref.ID + "/" + p.metric,
			})
		}
		token := ""
		for page := 0; ; page++ {
			if page >= c.cfg.maxPages() {
				warns = append(warns, fmt.Sprintf(
					"metric pagination stopped after %d pages; some series are incomplete", c.cfg.maxPages()))
				stale = true
				break
			}
			resp, err := c.met.GetMetricData(ctx, &GetMetricDataInput{
				Queries:   queries,
				StartTime: win.Start,
				EndTime:   win.End,
				NextToken: token,
			})
			if err != nil {
				return nil, false, fmt.Errorf("ec2: get metric data (batch %d, page %d): %w", start/batch, page, err)
			}
			if resp == nil {
				return nil, false, fmt.Errorf("ec2: get metric data (batch %d, page %d): nil response", start/batch, page)
			}
			warns = append(warns, resp.Messages...)
			for _, r := range resp.Results {
				p, ok := byID[r.ID]
				if !ok {
					warns = append(warns, fmt.Sprintf("discarded metric result with unknown query id %q", r.ID))
					continue
				}
				answered[r.ID] = true
				s := series[p.targetIdx][p.metric]
				if s == nil {
					s = &Series{
						Metric:        p.metric,
						Namespace:     p.namespace,
						Stat:          p.stat,
						PeriodSeconds: p.period,
						Status:        StatusComplete,
					}
					series[p.targetIdx][p.metric] = s
				}
				if err := appendResult(s, r); err != nil {
					return nil, false, fmt.Errorf("ec2: get metric data (%s/%s): %w",
						targets[p.targetIdx].Ref.ID, p.metric, err)
				}
			}
			if resp.NextToken == "" {
				break
			}
			if resp.NextToken == token {
				return nil, false, fmt.Errorf("ec2: get metric data: pagination token did not advance (%q)", token)
			}
			token = resp.NextToken
		}
	}

	// Truncation detection. GetMetricData answers every query it accepts, with
	// an empty value list when the metric has no data — the ordinary case for
	// mem_used_percent without the agent, and the reason memory-blind exists.
	// So a query with NO result at all is a truncated response, and the
	// difference matters enormously: absence of data is evidence, absence of
	// an answer is not. Unanswered queries become explicitly truncated series
	// so the sizer refuses instead of reading them as "nothing was using it".
	var unanswered []string
	for _, p := range plans {
		if answered[p.queryID] {
			continue
		}
		stale = true
		id := targets[p.targetIdx].Ref.ID
		unanswered = append(unanswered, id+"/"+p.metric)
		if series[p.targetIdx][p.metric] == nil {
			series[p.targetIdx][p.metric] = &Series{
				Metric:        p.metric,
				Namespace:     p.namespace,
				Stat:          p.stat,
				PeriodSeconds: p.period,
				Status:        StatusTruncated,
				Partial:       true,
			}
		}
	}
	if len(unanswered) > 0 {
		sort.Strings(unanswered)
		warns = append(warns, fmt.Sprintf(
			"GetMetricData returned no result for %d of %d queries (first: %s); the response was truncated and "+
				"the affected series are marked incomplete", len(unanswered), len(plans), unanswered[0]))
	}

	for i := range targets {
		metrics := make([]string, 0, len(series[i]))
		for m := range series[i] {
			metrics = append(metrics, m)
		}
		sort.Strings(metrics)
		for _, m := range metrics {
			s := series[i][m]
			sort.Slice(s.Points, func(a, b int) bool { return s.Points[a].At.Before(s.Points[b].At) })
			if s.Status != StatusComplete {
				s.Partial = true
			}
			targets[i].Series = append(targets[i].Series, *s)
		}
	}
	if stale {
		// Every series that was still being paged when the budget ran out is
		// suspect, and we cannot tell which — so all of them are.
		for i := range targets {
			for j := range targets[i].Series {
				targets[i].Series[j].Partial = true
				if targets[i].Series[j].Status == StatusComplete {
					// It said Complete, but we stopped reading before the end;
					// the series is incomplete for a reason that is ours, not
					// CloudWatch's, and the status should say which.
					targets[i].Series[j].Status = StatusTruncated
				}
			}
		}
	}
	return warns, stale, nil
}

// appendResult folds one GetMetricData result into a series, rejecting the
// malformed shapes that would otherwise become silent data corruption.
func appendResult(s *Series, r MetricDataResult) error {
	if len(r.Timestamps) != len(r.Values) {
		return fmt.Errorf("result %q has %d timestamps and %d values", r.ID, len(r.Timestamps), len(r.Values))
	}
	if r.StatusCode != "" && r.StatusCode != StatusComplete {
		s.Status = r.StatusCode
		s.Partial = true
	}
	for i := range r.Timestamps {
		s.Points = append(s.Points, Point{At: r.Timestamps[i], Value: r.Values[i]})
	}
	return nil
}
