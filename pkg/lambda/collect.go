package lambda

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// --- Cloud seams -----------------------------------------------------------
//
// Three interfaces, three READ operations, shaped after the AWS calls they
// stand in for, over plain Go structs. No SDK type appears here and no SDK is
// imported: this package's decision path has to link into an air-gapped binary,
// and the adapter that fills these structs from *lambda.Client, *cloudwatchlogs.Client
// and *cloudwatch.Client is a later unit's cmd/ wiring (see FINDINGS.md).
//
// There is deliberately no fourth seam. `lambda:UpdateFunctionConfiguration`
// has no representation anywhere in this package.

// FunctionRecord is one function as the inventory sees it. Tags and
// ProvisionedConcurrency come from separate AWS calls (`lambda:ListTags`,
// `lambda:ListProvisionedConcurrencyConfigs`) that the SDK adapter folds in
// here, so this package needs one seam instead of three.
type FunctionRecord struct {
	FunctionArn   string            `json:"functionArn"`
	FunctionName  string            `json:"functionName"`
	MemorySize    int64             `json:"memorySize"` // MB
	Timeout       int64             `json:"timeout"`    // seconds
	Architectures []string          `json:"architectures,omitempty"`
	Runtime       string            `json:"runtime,omitempty"`
	PackageType   string            `json:"packageType,omitempty"`
	LastModified  time.Time         `json:"lastModified,omitzero"`
	Tags          map[string]string `json:"tags,omitempty"`
	// ProvisionedConcurrency is the total allocated across versions/aliases.
	ProvisionedConcurrency int64 `json:"provisionedConcurrency,omitempty"`
	EphemeralStorageMB     int64 `json:"ephemeralStorageMB,omitempty"`
}

// ListFunctionsInput is the paginating inventory request.
type ListFunctionsInput struct {
	Marker   string `json:"marker,omitempty"`
	MaxItems int32  `json:"maxItems,omitempty"`
}

// ListFunctionsOutput is one page. An empty NextMarker ends pagination.
type ListFunctionsOutput struct {
	Functions  []FunctionRecord `json:"functions,omitempty"`
	NextMarker string           `json:"nextMarker,omitempty"`
}

// InventoryAPI is the lambda:ListFunctions-shaped seam.
type InventoryAPI interface {
	ListFunctions(ctx context.Context, in *ListFunctionsInput) (*ListFunctionsOutput, error)
}

// FilterLogEventsInput is the paginating log read. FilterPattern is passed
// through to the API so the REPORT filtering happens server-side; this package
// still validates every line it gets back, because a log filter is a string
// match and not a schema.
type FilterLogEventsInput struct {
	LogGroupName  string    `json:"logGroupName"`
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	FilterPattern string    `json:"filterPattern,omitempty"`
	NextToken     string    `json:"nextToken,omitempty"`
	Limit         int32     `json:"limit,omitempty"`
}

// FilterLogEventsOutput is one page of log events.
type FilterLogEventsOutput struct {
	Events    []LogEvent `json:"events,omitempty"`
	NextToken string     `json:"nextToken,omitempty"`
}

// LogsAPI is the logs:FilterLogEvents-shaped seam. FilterLogEvents rather than
// StartQuery/GetQueryResults on purpose: it is synchronous, it is bounded by
// the caller's ctx, and it does not leave a running Insights query behind when
// the context is cancelled.
type LogsAPI interface {
	FilterLogEvents(ctx context.Context, in *FilterLogEventsInput) (*FilterLogEventsOutput, error)
}

// MetricDataQuery is one GetMetricData query. ID must match CloudWatch's
// `^[a-z][a-zA-Z0-9_]*$`; the collector generates it and uses it — not the
// label, not the position — to route results back to functions.
type MetricDataQuery struct {
	ID            string            `json:"id"`
	Namespace     string            `json:"namespace"`
	MetricName    string            `json:"metricName"`
	Dimensions    map[string]string `json:"dimensions,omitempty"`
	PeriodSeconds int32             `json:"periodSeconds"`
	Stat          string            `json:"stat"`
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
	// StatusTruncated is this package's own marker, not a CloudWatch code:
	// GetMetricData returns one result per query — with empty values when the
	// metric has no data — so a MISSING result means the response was
	// truncated, which is a fact about the response, not about the metric.
	StatusTruncated = "Truncated"
)

// MetricDataResult is one query's answer; Timestamps and Values are parallel.
type MetricDataResult struct {
	ID         string      `json:"id"`
	Timestamps []time.Time `json:"timestamps,omitempty"`
	Values     []float64   `json:"values,omitempty"`
	StatusCode string      `json:"statusCode,omitempty"`
}

// GetMetricDataOutput is one page of results.
type GetMetricDataOutput struct {
	Results   []MetricDataResult `json:"results,omitempty"`
	NextToken string             `json:"nextToken,omitempty"`
}

// MetricsAPI is the cloudwatch:GetMetricData-shaped seam.
type MetricsAPI interface {
	GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error)
}

// --- Collector -------------------------------------------------------------

// Collector limits, as documented by the APIs they bound.
const (
	// MaxQueriesPerCall is GetMetricData's per-call query limit.
	MaxQueriesPerCall = 500
	// DefaultMaxPages bounds pagination per resource so one pathological
	// account cannot spin a collector forever.
	DefaultMaxPages = 50
	// DefaultMaxEventsPerFunction bounds the REPORT lines read per function.
	// Hitting it marks the target's snapshot stale rather than silently
	// analyzing a prefix of the window — a prefix is a different window.
	DefaultMaxEventsPerFunction = 20000
	// ReportFilterPattern is the server-side filter. It is an optimization
	// only: every returned line is still parsed and validated here.
	ReportFilterPattern = "REPORT"
)

// metricSpec is one CloudWatch metric this collector reads, with the statistic
// that makes it evidence.
type metricSpec struct {
	name string
	stat string
}

// collectedMetrics is fixed and ordered, so the queries a collector emits — and
// therefore the fixtures that record them — are identical run to run.
var collectedMetrics = []metricSpec{
	{MetricDuration, "Average"},
	{MetricErrors, "Sum"},
	{MetricInvocations, "Sum"},
	{MetricProvisionedConcurrentExecutions, "Maximum"},
	{MetricThrottles, "Sum"},
}

// CollectorConfig bounds one collection.
type CollectorConfig struct {
	Scope  string
	Region string
	// Window is the observation interval. Callers pass it; nothing here reads
	// the clock.
	Window Window
	// PeriodSeconds is the CloudWatch aggregation period.
	PeriodSeconds int32
	// MaxFunctions caps how many functions are collected; 0 means unlimited.
	MaxFunctions int
	// MaxPages caps pagination per resource.
	MaxPages int
	// MaxEventsPerFunction caps log events read per function.
	MaxEventsPerFunction int
	// Include, when non-empty, restricts collection to these function names.
	Include []string
}

// DefaultCollectorConfig returns the shipped bounds for a window.
func DefaultCollectorConfig(w Window) CollectorConfig {
	return CollectorConfig{
		Window:               w,
		PeriodSeconds:        300,
		MaxPages:             DefaultMaxPages,
		MaxEventsPerFunction: DefaultMaxEventsPerFunction,
	}
}

// Collector turns the three read seams into a [Snapshot]. A collector failure
// yields a stale-marked snapshot wherever it can, never a broken brain: the
// inventory call is the only hard dependency, because without it there is
// nothing to report on at all.
type Collector struct {
	inv     InventoryAPI
	logs    LogsAPI
	metrics MetricsAPI
	cfg     CollectorConfig
}

// NewCollector builds a collector. logs and metrics may be nil — a caller with
// no logs:FilterLogEvents permission still gets an inventory, and every
// function in it honestly refuses with [ReasonNoReportEvidence].
func NewCollector(inv InventoryAPI, logs LogsAPI, metrics MetricsAPI, cfg CollectorConfig) (*Collector, error) {
	if inv == nil {
		return nil, fmt.Errorf("lambda: collector needs an inventory API")
	}
	if cfg.Window.Duration() <= 0 {
		return nil, fmt.Errorf("lambda: collector needs a positive window, got %s", cfg.Window.String())
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	if cfg.MaxEventsPerFunction <= 0 {
		cfg.MaxEventsPerFunction = DefaultMaxEventsPerFunction
	}
	if cfg.PeriodSeconds <= 0 {
		cfg.PeriodSeconds = 300
	}
	return &Collector{inv: inv, logs: logs, metrics: metrics, cfg: cfg}, nil
}

// Domain identifies this collector's domain, matching the domain.Collector
// contract. The Collect signature differs from that interface deliberately: it
// returns the Lambda-native [Snapshot], because per-invocation REPORT records
// do not fit domain.Sample — see [Domain] and [Snapshot.Generic].
func (c *Collector) Domain() domain.Kind { return Kind }

// Collect reads the inventory, the REPORT lines and the metrics, and returns a
// snapshot. Every AWS-shaped call is bounded by ctx and by the collector's page
// budgets; exceeding a budget marks the snapshot stale with a warning that says
// which budget and for which function.
func (c *Collector) Collect(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{
		Domain: Kind, Scope: c.cfg.Scope, Region: c.cfg.Region,
		Timestamp: c.cfg.Window.End, Window: c.cfg.Window,
	}
	funcs, warns, err := c.listFunctions(ctx)
	if err != nil {
		return nil, err
	}
	snap.Warnings = append(snap.Warnings, warns...)
	if len(warns) > 0 {
		snap.Stale = true
	}

	for _, f := range funcs {
		t := Target{
			Ref:      targetRef(c.cfg.Scope, f),
			Function: functionFromRecord(f),
		}
		events, evWarn, err := c.readReports(ctx, f)
		if err != nil {
			return nil, err
		}
		if evWarn != "" {
			snap.Warnings = append(snap.Warnings, evWarn)
			snap.Stale = true
		}
		t.Reports, t.Drops = ParseEvents(events)
		snap.Targets = append(snap.Targets, t)
	}

	if err := c.readMetrics(ctx, snap); err != nil {
		return nil, err
	}
	SortTargets(snap.Targets)
	snap.Warnings = sortWarnings(snap.Warnings)
	return snap, nil
}

func (c *Collector) listFunctions(ctx context.Context) ([]FunctionRecord, []string, error) {
	var (
		out    []FunctionRecord
		warns  []string
		marker string
		seen   = map[string]bool{}
		want   = includeSet(c.cfg.Include)
	)
	for page := 0; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if page >= c.cfg.MaxPages {
			warns = append(warns, fmt.Sprintf(
				"stopped listing functions after %d pages: the inventory is incomplete, so functions beyond it "+
					"are absent from this report rather than reported as having no findings", c.cfg.MaxPages))
			break
		}
		res, err := c.inv.ListFunctions(ctx, &ListFunctionsInput{Marker: marker})
		if err != nil {
			return nil, nil, fmt.Errorf("lambda: list functions: %w", err)
		}
		if res == nil {
			break
		}
		for _, f := range res.Functions {
			id := f.FunctionArn
			if id == "" {
				id = f.FunctionName
			}
			if id == "" || seen[id] {
				continue
			}
			if want != nil && !want[f.FunctionName] {
				continue
			}
			seen[id] = true
			out = append(out, f)
			if c.cfg.MaxFunctions > 0 && len(out) >= c.cfg.MaxFunctions {
				warns = append(warns, fmt.Sprintf(
					"stopped at the %d-function cap: the remaining functions are absent from this report",
					c.cfg.MaxFunctions))
				sortRecords(out)
				return out, warns, nil
			}
		}
		if res.NextMarker == "" {
			break
		}
		marker = res.NextMarker
	}
	sortRecords(out)
	return out, warns, nil
}

func (c *Collector) readReports(ctx context.Context, f FunctionRecord) ([]LogEvent, string, error) {
	if c.logs == nil {
		return nil, "", nil
	}
	var (
		out   []LogEvent
		token string
	)
	group := LogGroupPrefix + f.FunctionName
	for page := 0; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if page >= c.cfg.MaxPages {
			return out, fmt.Sprintf(
				"stopped reading %s after %d pages: the evidence for %s covers only part of the window",
				group, c.cfg.MaxPages, f.FunctionName), nil
		}
		res, err := c.logs.FilterLogEvents(ctx, &FilterLogEventsInput{
			LogGroupName:  group,
			Start:         c.cfg.Window.Start,
			End:           c.cfg.Window.End,
			FilterPattern: ReportFilterPattern,
			NextToken:     token,
		})
		if err != nil {
			// A missing log group is the normal state of a function that has
			// never run, and a denied read is an IAM gap. Neither is worth
			// failing the whole collection over: the function is reported with
			// no evidence, which is exactly what happened.
			return out, fmt.Sprintf("could not read %s (%v): %s is reported without REPORT evidence",
				group, err, f.FunctionName), nil
		}
		if res == nil {
			break
		}
		out = append(out, res.Events...)
		if len(out) >= c.cfg.MaxEventsPerFunction {
			out = out[:c.cfg.MaxEventsPerFunction]
			return out, fmt.Sprintf(
				"hit the %d-event cap on %s: the analyzed evidence is a PREFIX of the window, which is a "+
					"different window", c.cfg.MaxEventsPerFunction, group), nil
		}
		if res.NextToken == "" {
			break
		}
		token = res.NextToken
	}
	return out, "", nil
}

// readMetrics batches one GetMetricData call per MaxQueriesPerCall queries and
// routes results back by query ID. A result CloudWatch did not return marks the
// series partial: a missing answer is not an empty metric.
func (c *Collector) readMetrics(ctx context.Context, snap *Snapshot) error {
	if c.metrics == nil {
		return nil
	}
	type slot struct {
		target int
		metric metricSpec
	}
	var (
		queries []MetricDataQuery
		slots   = map[string]slot{}
	)
	for i, t := range snap.Targets {
		for j, m := range collectedMetrics {
			id := fmt.Sprintf("q%d_%d", i, j)
			slots[id] = slot{target: i, metric: m}
			queries = append(queries, MetricDataQuery{
				ID:            id,
				Namespace:     NamespaceLambda,
				MetricName:    m.name,
				Dimensions:    map[string]string{"FunctionName": t.Function.Name},
				PeriodSeconds: c.cfg.PeriodSeconds,
				Stat:          m.stat,
			})
		}
	}
	got := map[string]MetricDataResult{}
	for start := 0; start < len(queries); start += MaxQueriesPerCall {
		end := start + MaxQueriesPerCall
		if end > len(queries) {
			end = len(queries)
		}
		token := ""
		for page := 0; ; page++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if page >= c.cfg.MaxPages {
				snap.Stale = true
				snap.Warnings = append(snap.Warnings,
					"stopped paging metric results: some series are incomplete and their functions are refused")
				break
			}
			res, err := c.metrics.GetMetricData(ctx, &GetMetricDataInput{
				Queries:   queries[start:end],
				StartTime: c.cfg.Window.Start,
				EndTime:   c.cfg.Window.End,
				NextToken: token,
			})
			if err != nil {
				return fmt.Errorf("lambda: get metric data: %w", err)
			}
			if res == nil {
				break
			}
			for _, r := range res.Results {
				if _, dup := got[r.ID]; dup {
					continue
				}
				got[r.ID] = r
			}
			if res.NextToken == "" {
				break
			}
			token = res.NextToken
		}
	}

	for id, sl := range slots {
		r, ok := got[id]
		ser := Series{
			Metric: sl.metric.name, Stat: sl.metric.stat, Source: SourceCloudWatch,
			PeriodSeconds: c.cfg.PeriodSeconds,
		}
		if !ok {
			ser.Partial, ser.Status = true, StatusTruncated
		} else {
			ser.Status = r.StatusCode
			if ser.Status == "" {
				ser.Status = StatusComplete
			}
			ser.Partial = ser.Status != StatusComplete
			n := len(r.Timestamps)
			if len(r.Values) < n {
				n = len(r.Values)
				ser.Partial, ser.Status = true, StatusPartialData
			}
			for i := 0; i < n; i++ {
				ser.Points = append(ser.Points, Point{At: r.Timestamps[i], Value: r.Values[i]})
			}
			sort.Slice(ser.Points, func(i, j int) bool { return ser.Points[i].At.Before(ser.Points[j].At) })
		}
		snap.Targets[sl.target].Series = append(snap.Targets[sl.target].Series, ser)
	}
	return nil
}

func targetRef(scope string, f FunctionRecord) domain.TargetRef {
	id := f.FunctionArn
	if id == "" {
		id = f.FunctionName
	}
	return domain.TargetRef{Domain: Kind, Scope: scope, ID: id, Name: f.FunctionName}
}

func functionFromRecord(f FunctionRecord) Function {
	arch := ArchX86
	for _, a := range f.Architectures {
		if a == ArchARM {
			arch = ArchARM
			break
		}
	}
	id := f.FunctionArn
	if id == "" {
		id = f.FunctionName
	}
	return Function{
		ARN:                    id,
		Name:                   f.FunctionName,
		MemoryMB:               f.MemorySize,
		TimeoutSec:             f.Timeout,
		Architecture:           arch,
		Runtime:                f.Runtime,
		PackageType:            f.PackageType,
		LastModified:           f.LastModified,
		Tags:                   copyTags(f.Tags),
		ProvisionedConcurrency: f.ProvisionedConcurrency,
		EphemeralStorageMB:     f.EphemeralStorageMB,
	}
}

func includeSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out[n] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sortRecords orders the inventory by ARN so collection order — which is an
// AWS pagination detail — cannot reach the snapshot.
func sortRecords(fs []FunctionRecord) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].FunctionArn != fs[j].FunctionArn {
			return fs[i].FunctionArn < fs[j].FunctionArn
		}
		return fs[i].FunctionName < fs[j].FunctionName
	})
}
