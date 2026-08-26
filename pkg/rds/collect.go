package rds

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// --- Cloud seams -----------------------------------------------------------
//
// Three interfaces, five READ operations, shaped after the AWS calls they
// stand in for, over plain Go structs. No SDK type appears here and no SDK is
// imported: this package's decision path has to link into an air-gapped
// binary, and the adapter that fills these structs from *rds.Client and
// *cloudwatch.Client is cmd/ wiring a later unit adds (FINDINGS.md §6).
//
// There is deliberately no fourth seam. `rds:ModifyDBInstance` has no
// representation anywhere in this package — not a method, not a struct, not a
// constant behind a flag (TestNoMutatingAPISurface).
//
// This is the FIFTH independent derivation of the CloudWatch seam in this
// tree (pkg/ec2, pkg/ebs, pkg/ecs, pkg/lambda, and now here), and
// docs/design/rds-batch-assessment.md §1.4 says so in as many words. It is
// duplicated again rather than lifted because lifting it is a change to a
// shared package this unit may not make; FINDINGS.md §5 records the four
// truths every copy has had to re-derive, so the eventual pkg/cloudwatch has
// a specification waiting for it.

// DBInstanceRecord is one DB instance as `rds:DescribeDBInstances` reports it.
// Field names follow the API so an SDK adapter is a field-for-field copy with
// no interpretation in between — interpretation is [instanceFromRecord]'s job
// and is testable here.
type DBInstanceRecord struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
	DBInstanceArn        string `json:"dbInstanceArn,omitempty"`
	DBInstanceClass      string `json:"dbInstanceClass,omitempty"`
	DBInstanceStatus     string `json:"dbInstanceStatus,omitempty"`
	Engine               string `json:"engine,omitempty"`
	EngineVersion        string `json:"engineVersion,omitempty"`
	LicenseModel         string `json:"licenseModel,omitempty"`

	MultiAZ             bool   `json:"multiAZ,omitempty"`
	DBClusterIdentifier string `json:"dbClusterIdentifier,omitempty"`
	AvailabilityZone    string `json:"availabilityZone,omitempty"`

	// ReadReplicaSourceDBInstanceIdentifier is empty on a primary.
	ReadReplicaSourceDBInstanceIdentifier string   `json:"readReplicaSourceDBInstanceIdentifier,omitempty"`
	ReadReplicaDBInstanceIdentifiers      []string `json:"readReplicaDBInstanceIdentifiers,omitempty"`

	AllocatedStorage    int64  `json:"allocatedStorage,omitempty"`    // GiB
	MaxAllocatedStorage int64  `json:"maxAllocatedStorage,omitempty"` // GiB; > AllocatedStorage ⇒ autoscaling on
	StorageType         string `json:"storageType,omitempty"`
	Iops                int32  `json:"iops,omitempty"`
	StorageThroughput   int32  `json:"storageThroughput,omitempty"`

	InstanceCreateTime time.Time `json:"instanceCreateTime,omitzero"`

	// TagList is the inline tag list some RDS responses carry. When it is
	// empty the collector falls back to ListTagsForResource.
	TagList map[string]string `json:"tagList,omitempty"`
}

// DescribeDBInstancesInput is the paginating inventory request.
type DescribeDBInstancesInput struct {
	Marker     string `json:"marker,omitempty"`
	MaxRecords int32  `json:"maxRecords,omitempty"`
}

// DescribeDBInstancesOutput is one page. An empty Marker ends pagination.
type DescribeDBInstancesOutput struct {
	DBInstances []DBInstanceRecord `json:"dbInstances,omitempty"`
	Marker      string             `json:"marker,omitempty"`
}

// DBClusterRecord is one cluster as `rds:DescribeDBClusters` reports it.
//
// Clusters are read for ONE reason: to tell an Aurora cluster from a Multi-AZ
// DB cluster without inferring it from an instance's engine string. Both put a
// value in DBClusterIdentifier and both are excluded, but they are excluded by
// different names ([ReasonAuroraNotSupported] vs
// [ReasonClusterMemberNotSupported]) and a report whose whole value is that
// its statements are true must not call a PostgreSQL Multi-AZ cluster
// "Aurora".
type DBClusterRecord struct {
	DBClusterIdentifier string `json:"dbClusterIdentifier"`
	DBClusterArn        string `json:"dbClusterArn,omitempty"`
	Engine              string `json:"engine,omitempty"`
	EngineMode          string `json:"engineMode,omitempty"` // "provisioned" | "serverless"
	// DBClusterMembers are the member instance identifiers.
	DBClusterMembers []string `json:"dbClusterMembers,omitempty"`
	// ServerlessV2MinCapacity/MaxCapacity are the ACU bounds — the only levers
	// an Aurora Serverless v2 cluster actually has. Carried so the Aurora
	// refusal can name what a future unit would need to look at, never used
	// for arithmetic here.
	ServerlessV2MinCapacity float64 `json:"serverlessV2MinCapacity,omitempty"`
	ServerlessV2MaxCapacity float64 `json:"serverlessV2MaxCapacity,omitempty"`
}

// DescribeDBClustersInput is the paginating cluster request.
type DescribeDBClustersInput struct {
	Marker string `json:"marker,omitempty"`
}

// DescribeDBClustersOutput is one page.
type DescribeDBClustersOutput struct {
	DBClusters []DBClusterRecord `json:"dbClusters,omitempty"`
	Marker     string            `json:"marker,omitempty"`
}

// ListTagsForResourceInput asks for one resource's tags.
type ListTagsForResourceInput struct {
	ResourceName string `json:"resourceName"` // the DB instance ARN
}

// ListTagsForResourceOutput is that resource's tags.
type ListTagsForResourceOutput struct {
	TagList map[string]string `json:"tagList,omitempty"`
}

// InventoryAPI is the rds: read seam. Three operations, all read-only, and the
// interface names them so a reviewer can see the whole write surface this
// package has: none.
type InventoryAPI interface {
	DescribeDBInstances(ctx context.Context, in *DescribeDBInstancesInput) (*DescribeDBInstancesOutput, error)
	DescribeDBClusters(ctx context.Context, in *DescribeDBClustersInput) (*DescribeDBClustersOutput, error)
	ListTagsForResource(ctx context.Context, in *ListTagsForResourceInput) (*ListTagsForResourceOutput, error)
}

// MetricDataQuery is one GetMetricData query. ID must match CloudWatch's
// `^[a-z][a-zA-Z0-9_]*$`; the collector generates it and uses it — not the
// label, not the position — to route results back to instances.
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
	// Reading it the other way turns an unanswered query into "this database
	// had no connections", which is how an idle verdict gets manufactured out
	// of nothing.
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

// ReservedDBInstanceRecord is one reservation as
// `rds:DescribeReservedDBInstances` reports it.
//
// FixedPrice and UsagePrice are carried raw and amortized here rather than in
// cmd/, so the amortization has one implementation and one test:
// EffectiveHourly = UsagePrice + FixedPrice ÷ term-hours.
type ReservedDBInstanceRecord struct {
	ReservedDBInstanceId string    `json:"reservedDBInstanceId"`
	DBInstanceClass      string    `json:"dbInstanceClass"`
	DBInstanceCount      int       `json:"dbInstanceCount"`
	ProductDescription   string    `json:"productDescription,omitempty"`
	MultiAZ              bool      `json:"multiAZ,omitempty"`
	OfferingType         string    `json:"offeringType,omitempty"`
	State                string    `json:"state,omitempty"`
	FixedPrice           float64   `json:"fixedPrice,omitempty"`
	UsagePrice           float64   `json:"usagePrice,omitempty"`
	Duration             int64     `json:"duration,omitempty"` // seconds
	StartTime            time.Time `json:"startTime,omitzero"`
}

// DescribeReservedDBInstancesInput is the paginating reservation request.
type DescribeReservedDBInstancesInput struct {
	Marker string `json:"marker,omitempty"`
}

// DescribeReservedDBInstancesOutput is one page.
type DescribeReservedDBInstancesOutput struct {
	ReservedDBInstances []ReservedDBInstanceRecord `json:"reservedDBInstances,omitempty"`
	Marker              string                     `json:"marker,omitempty"`
}

// CommitmentAPI is the rds:DescribeReservedDBInstances-shaped seam.
//
// It is a SEPARATE seam from [InventoryAPI], and not merely for tidiness: a
// caller may hold `rds:DescribeDBInstances` and not
// `rds:DescribeReservedDBInstances`, and the correct behaviour then is a
// complete report with net == gross rather than no report at all. NewCollector
// accepts a nil CommitmentAPI for that reason.
type CommitmentAPI interface {
	DescribeReservedDBInstances(ctx context.Context,
		in *DescribeReservedDBInstancesInput) (*DescribeReservedDBInstancesOutput, error)
}

// --- Collector -------------------------------------------------------------

// Collector limits, as documented by the APIs they bound.
const (
	// MaxQueriesPerCall is GetMetricData's per-call query limit.
	MaxQueriesPerCall = 500
	// DefaultMaxPages bounds pagination per resource so one pathological
	// account cannot spin a collector forever.
	DefaultMaxPages = 50
)

// metricSpec is one CloudWatch metric this collector reads, with the statistic
// that makes it evidence and the finest period AWS will actually answer at.
type metricSpec struct {
	name string
	stat string
	// minPeriodSeconds is the publication granularity floor. RDS publishes
	// most metrics at 60 s by default, but "CPU credit metrics are available
	// at a five-minute frequency only" [verified: rds-metrics.html]. Asking
	// for 60 s there returns gaps, and gaps in a credit series read as a
	// depleted bucket.
	minPeriodSeconds int32
}

// collectedMetrics is fixed and ordered, so the queries a collector emits —
// and therefore the fixtures that record them — are identical run to run.
//
// The statistic on each line is chosen for the direction the verdict must be
// safe in:
//
//   - FreeableMemory and FreeStorageSpace use Minimum, because the worst
//     moment is the constraint. An average would hide the 3 a.m. batch job
//     that filled the volume.
//   - DatabaseConnections uses Maximum, because the idle test must fail if
//     ANY point was non-zero. Averaging a single busy minute across a
//     fortnight rounds a live database to idle.
//   - CPUUtilization uses Average, with percentiles taken over the delivered
//     series rather than asked of CloudWatch, matching the four shipped
//     domains.
var collectedMetrics = []metricSpec{
	{MetricBurstBalance, "Minimum", CreditPeriodSeconds},
	{MetricCPUCreditBalance, "Minimum", CreditPeriodSeconds},
	{MetricCPUUtilization, "Average", PublicationPeriodSeconds},
	{MetricDatabaseConns, "Maximum", PublicationPeriodSeconds},
	{MetricFreeStorageSpace, "Minimum", PublicationPeriodSeconds},
	{MetricFreeableMemory, "Minimum", PublicationPeriodSeconds},
	{MetricReadIOPS, "Average", PublicationPeriodSeconds},
	{MetricReadThroughput, "Average", PublicationPeriodSeconds},
	{MetricSwapUsage, "Maximum", PublicationPeriodSeconds},
	{MetricWriteIOPS, "Average", PublicationPeriodSeconds},
	{MetricWriteThroughput, "Average", PublicationPeriodSeconds},
}

// CollectedMetrics returns the metric names this collector reads, in canonical
// order. Exported so a caller can size an IAM policy — or a CloudWatch bill —
// without reading the source.
func CollectedMetrics() []string {
	out := make([]string, 0, len(collectedMetrics))
	for _, m := range collectedMetrics {
		out = append(out, m.name)
	}
	return out
}

// CollectorConfig bounds one collection.
type CollectorConfig struct {
	Scope  string
	Region string
	// Window is the observation interval. Callers pass it; nothing here reads
	// the clock. It is clamped to CloudWatch retention — see [ClampWindow].
	Window Window
	// PeriodSeconds is the requested CloudWatch aggregation period. Per-metric
	// floors still apply, so a credit metric requested at 60 s is issued at
	// 300 s and the delivered series says so.
	PeriodSeconds int32
	// MaxInstances caps how many instances are collected; 0 means unlimited.
	MaxInstances int
	// MaxPages caps pagination per resource.
	MaxPages int
	// Include, when non-empty, restricts collection to these instance
	// identifiers.
	Include []string
}

// DefaultCollectorConfig returns the shipped bounds for a window.
func DefaultCollectorConfig(w Window) CollectorConfig {
	return CollectorConfig{
		Window:        w,
		PeriodSeconds: PublicationPeriodSeconds,
		MaxPages:      DefaultMaxPages,
	}
}

// Collector turns the read seams into a [Snapshot]. A collector failure yields
// a stale-marked snapshot wherever it can, never a broken brain: the inventory
// call is the only hard dependency, because without it there is nothing to
// report on at all.
type Collector struct {
	inv     InventoryAPI
	metrics MetricsAPI
	commit  CommitmentAPI
	cfg     CollectorConfig
	// clamped records that the window was shortened to CloudWatch retention,
	// so the snapshot can carry a warning saying which window was actually
	// observed.
	clamped bool
}

// NewCollector builds a collector.
//
// metrics and commit may be nil. A caller with no `cloudwatch:GetMetricData`
// permission still gets a complete inventory, and every instance in it
// honestly refuses with [ReasonNoMetricEvidence]; a caller with no
// `rds:DescribeReservedDBInstances` permission gets a report whose net equals
// its gross, which under-claims and can never invent a saving.
//
// The window is clamped here rather than at query time so the snapshot's
// Window field is the window that was actually observed. A snapshot that
// claims a 30-day window and contains 15 days of data is a lie told by
// omission, and every downstream "insufficient window" gate would read the
// claim rather than the data.
func NewCollector(inv InventoryAPI, metrics MetricsAPI, cm CommitmentAPI, cfg CollectorConfig) (*Collector, error) {
	if inv == nil {
		return nil, fmt.Errorf("rds: collector needs an inventory API")
	}
	if cfg.Window.Duration() <= 0 {
		return nil, fmt.Errorf("rds: collector needs a positive window, got %s", cfg.Window.String())
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	if cfg.PeriodSeconds <= 0 {
		cfg.PeriodSeconds = PublicationPeriodSeconds
	}
	w, clamped := ClampWindow(cfg.Window, cfg.PeriodSeconds)
	cfg.Window = w
	return &Collector{inv: inv, metrics: metrics, commit: cm, cfg: cfg, clamped: clamped}, nil
}

// Domain identifies this collector's domain, matching the domain.Collector
// contract. The Collect signature differs from that interface deliberately: it
// returns the RDS-native [Snapshot], because per-series truncation status does
// not fit [domain.Sample] and "we were not told" must stay distinguishable
// from "there was nothing" (pkg/domain/snapshot.go's Payload note).
func (c *Collector) Domain() domain.Kind { return Kind }

// Window returns the observation window this collector will actually use —
// the caller's, clamped to CloudWatch retention.
func (c *Collector) Window() Window { return c.cfg.Window }

// Collect reads the inventory, the clusters, the tags, the metrics and the
// reservations, and returns a snapshot. Every AWS-shaped call is bounded by
// ctx and by the collector's page budget; exceeding a budget marks the
// snapshot stale with a warning that says which budget.
func (c *Collector) Collect(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{
		Domain: Kind, Scope: c.cfg.Scope, Region: c.cfg.Region,
		Timestamp: c.cfg.Window.End, Window: c.cfg.Window,
	}
	if c.clamped {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf(
			"the observation window was shortened to %s: CloudWatch keeps 1-minute datapoints for %s, and a "+
				"longer window would pad the series with silence that reads as idleness",
			c.cfg.Window.String(), RetentionAtOneMinute))
	}

	records, warns, err := c.describeInstances(ctx)
	if err != nil {
		return nil, err
	}
	snap.Warnings = append(snap.Warnings, warns...)
	if len(warns) > 0 {
		snap.Stale = true
	}

	clusters, cwarns := c.describeClusters(ctx)
	snap.Warnings = append(snap.Warnings, cwarns...)

	for _, r := range records {
		inst := instanceFromRecord(r, c.cfg.Region)
		if len(inst.Tags) == 0 {
			tags, warn := c.readTags(ctx, inst.ARN)
			if warn != "" {
				snap.Warnings = append(snap.Warnings, warn)
			}
			inst.Tags = tags
		}
		// A cluster's engine is authoritative over a member's. A Multi-AZ DB
		// cluster's members report "mysql"; only the cluster says it is a
		// cluster deployment, and only the cluster's engine says whether it is
		// Aurora.
		if cl, ok := clusters[inst.ClusterID]; ok && cl.Engine != "" && inst.Engine == "" {
			inst.Engine = cl.Engine
		}
		snap.Targets = append(snap.Targets, Target{
			Ref:      targetRef(c.cfg.Scope, inst),
			Instance: inst,
			Cluster:  clusterInfoFor(clusters, inst.ClusterID),
		})
	}

	if err := c.readMetrics(ctx, snap); err != nil {
		return nil, err
	}
	res, rwarn := c.describeReservations(ctx)
	snap.Reservations = res
	if rwarn != "" {
		snap.Warnings = append(snap.Warnings, rwarn)
	}

	SortTargets(snap.Targets)
	SortReservations(snap.Reservations)
	snap.Warnings = sortWarnings(snap.Warnings)
	return snap, nil
}

func (c *Collector) describeInstances(ctx context.Context) ([]DBInstanceRecord, []string, error) {
	var (
		out    []DBInstanceRecord
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
				"stopped listing DB instances after %d pages: the inventory is incomplete, so instances "+
					"beyond it are ABSENT from this report rather than reported as having no findings",
				c.cfg.MaxPages))
			break
		}
		res, err := c.inv.DescribeDBInstances(ctx, &DescribeDBInstancesInput{Marker: marker})
		if err != nil {
			return nil, nil, fmt.Errorf("rds: describe db instances: %w", err)
		}
		if res == nil {
			break
		}
		for _, r := range res.DBInstances {
			id := recordID(r)
			if id == "" || seen[id] {
				continue
			}
			if want != nil && !want[r.DBInstanceIdentifier] {
				continue
			}
			seen[id] = true
			out = append(out, r)
			if c.cfg.MaxInstances > 0 && len(out) >= c.cfg.MaxInstances {
				warns = append(warns, fmt.Sprintf(
					"stopped at the %d-instance cap: the remaining instances are absent from this report",
					c.cfg.MaxInstances))
				sortRecords(out)
				return out, warns, nil
			}
		}
		if res.Marker == "" {
			break
		}
		marker = res.Marker
	}
	sortRecords(out)
	return out, warns, nil
}

// describeClusters reads cluster membership. A failure is a WARNING, not an
// error: without it a cluster member is still excluded, just under the more
// cautious name — see [clusterInfoFor].
func (c *Collector) describeClusters(ctx context.Context) (map[string]DBClusterRecord, []string) {
	out := map[string]DBClusterRecord{}
	var (
		warns  []string
		marker string
	)
	for page := 0; page < c.cfg.MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return out, append(warns, fmt.Sprintf("cluster listing was cancelled (%v)", err))
		}
		res, err := c.inv.DescribeDBClusters(ctx, &DescribeDBClustersInput{Marker: marker})
		if err != nil {
			return out, append(warns, fmt.Sprintf(
				"could not list DB clusters (%v): cluster members are still excluded, but the report cannot "+
					"say whether a member belongs to Aurora or to a Multi-AZ DB cluster", err))
		}
		if res == nil {
			break
		}
		for _, cl := range res.DBClusters {
			if cl.DBClusterIdentifier != "" {
				out[cl.DBClusterIdentifier] = cl
			}
		}
		if res.Marker == "" {
			break
		}
		marker = res.Marker
	}
	return out, warns
}

// readTags reads one instance's tags. A denied read is an IAM gap, not a
// reason to fail the whole collection — but it does mean the kilter.dev/mode
// guardrail cannot be evaluated, which the warning says out loud.
func (c *Collector) readTags(ctx context.Context, arn string) (map[string]string, string) {
	if arn == "" {
		return nil, ""
	}
	res, err := c.inv.ListTagsForResource(ctx, &ListTagsForResourceInput{ResourceName: arn})
	if err != nil {
		return nil, fmt.Sprintf(
			"could not read tags for %s (%v): the %s guardrail cannot be evaluated for it, so an opt-out "+
				"tag on that instance would not be honoured", arn, err, TagKilterMode)
	}
	if res == nil {
		return nil, ""
	}
	return copyTags(res.TagList), ""
}

// readMetrics batches one GetMetricData call per MaxQueriesPerCall queries and
// routes results back BY QUERY ID. A result CloudWatch did not return marks
// the series partial: a missing answer is not an empty metric.
func (c *Collector) readMetrics(ctx context.Context, snap *Snapshot) error {
	if c.metrics == nil {
		for i := range snap.Targets {
			snap.Targets[i].Series = nil
		}
		snap.Warnings = append(snap.Warnings,
			"no metrics API was wired: every instance is reported without CloudWatch evidence and refuses")
		return nil
	}
	type slot struct {
		target int
		metric metricSpec
		period int32
	}
	var (
		queries []MetricDataQuery
		slots   = map[string]slot{}
	)
	for i, t := range snap.Targets {
		for j, m := range collectedMetrics {
			period := c.cfg.PeriodSeconds
			if period < m.minPeriodSeconds {
				period = m.minPeriodSeconds
			}
			id := fmt.Sprintf("q%d_%d", i, j)
			slots[id] = slot{target: i, metric: m, period: period}
			queries = append(queries, MetricDataQuery{
				ID:            id,
				Namespace:     NamespaceRDS,
				MetricName:    m.name,
				Dimensions:    map[string]string{"DBInstanceIdentifier": t.Instance.Identifier},
				PeriodSeconds: period,
				Stat:          m.stat,
			})
		}
	}

	got := map[string]MetricDataResult{}
	for start := 0; start < len(queries); start += MaxQueriesPerCall {
		end := min(start+MaxQueriesPerCall, len(queries))
		token := ""
		for page := 0; ; page++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if page >= c.cfg.MaxPages {
				snap.Stale = true
				snap.Warnings = append(snap.Warnings,
					"stopped paging metric results: some series are incomplete and their instances are refused")
				break
			}
			res, err := c.metrics.GetMetricData(ctx, &GetMetricDataInput{
				Queries:   queries[start:end],
				StartTime: c.cfg.Window.Start,
				EndTime:   c.cfg.Window.End,
				NextToken: token,
			})
			if err != nil {
				return fmt.Errorf("rds: get metric data: %w", err)
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

	// Iterate the SLOTS, not the results: a query with no answer must produce
	// a partial series, and iterating results would silently drop it.
	ids := make([]string, 0, len(slots))
	for id := range slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		sl := slots[id]
		ser := Series{
			Metric: sl.metric.name, Stat: sl.metric.stat, Source: SourceCloudWatch,
			PeriodSeconds: sl.period,
		}
		r, ok := got[id]
		if !ok {
			ser.Partial, ser.Status = true, StatusTruncated
			snap.Targets[sl.target].Series = append(snap.Targets[sl.target].Series, ser)
			continue
		}
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
		sort.SliceStable(ser.Points, func(i, j int) bool { return ser.Points[i].At.Before(ser.Points[j].At) })
		snap.Targets[sl.target].Series = append(snap.Targets[sl.target].Series, ser)
	}
	return nil
}

// describeReservations reads the account's Reserved DB Instances and amortizes
// each into the shape pkg/pricing/commit bills against.
func (c *Collector) describeReservations(ctx context.Context) ([]commit.ReservedDBInstance, string) {
	if c.commit == nil {
		return nil, ""
	}
	var (
		out    []commit.ReservedDBInstance
		marker string
	)
	for page := 0; page < c.cfg.MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return out, fmt.Sprintf("reservation listing was cancelled (%v)", err)
		}
		res, err := c.commit.DescribeReservedDBInstances(ctx, &DescribeReservedDBInstancesInput{Marker: marker})
		if err != nil {
			return nil, fmt.Sprintf(
				"could not list Reserved DB Instances (%v): net savings equal gross in this report, which "+
					"under-claims — a reservation that would strand cannot be seen", err)
		}
		if res == nil {
			break
		}
		for _, r := range res.ReservedDBInstances {
			if rdb, ok := reservationFromRecord(r, c.cfg.Region); ok {
				out = append(out, rdb)
			}
		}
		if res.Marker == "" {
			break
		}
		marker = res.Marker
	}
	return out, ""
}

// reservationFromRecord amortizes one DescribeReservedDBInstances record.
//
// EffectiveHourly = UsagePrice + FixedPrice ÷ term-hours, matching what
// [commit.ReservedDBInstance] documents. A record with a non-positive duration
// keeps only its usage price — an unamortizable upfront is dropped rather than
// divided by zero, which under-states the reservation's cost and therefore
// under-states stranding, the safe direction.
//
// Only `active` and `payment-pending` reservations are returned. A `retired`
// reservation bills nothing and would manufacture coverage that does not exist.
func reservationFromRecord(r ReservedDBInstanceRecord, region string) (commit.ReservedDBInstance, bool) {
	switch strings.ToLower(strings.TrimSpace(r.State)) {
	case "", "active", "payment-pending", "pending-payment":
	default:
		return commit.ReservedDBInstance{}, false
	}
	if r.DBInstanceCount <= 0 || r.DBInstanceClass == "" {
		return commit.ReservedDBInstance{}, false
	}
	eff := r.UsagePrice
	if r.Duration > 0 {
		eff += r.FixedPrice / (float64(r.Duration) / 3600)
	}
	if !finite(eff) || eff < 0 {
		eff = 0
	}
	dep := commit.RDSSingleAZ
	if r.MultiAZ {
		dep = commit.RDSMultiAZInstance
	}
	out := commit.ReservedDBInstance{
		ID:                 r.ReservedDBInstanceId,
		Count:              r.DBInstanceCount,
		DBInstanceClass:    r.DBInstanceClass,
		Region:             region,
		Engine:             commit.NormalizeRDSEngine(r.ProductDescription),
		Deployment:         dep,
		OfferingType:       r.OfferingType,
		EffectiveHourlyUSD: eff,
	}
	if r.Duration > 0 && !r.StartTime.IsZero() {
		out.Expires = r.StartTime.Add(time.Duration(r.Duration) * time.Second)
	}
	return out, true
}

// ClusterInfo is what a target knows about the cluster it belongs to. Empty
// for a standalone DB instance.
type ClusterInfo struct {
	ID string `json:"id,omitempty"`
	// Engine is the CLUSTER's engine, which is authoritative: an Aurora
	// cluster says "aurora-postgresql" while its members may report the same
	// string, and a Multi-AZ DB cluster says "mysql" exactly as its members do.
	Engine string `json:"engine,omitempty"`
	// Mode is "provisioned" or "serverless".
	Mode string `json:"mode,omitempty"`
	// Known reports whether DescribeDBClusters actually answered for this
	// cluster. When false the member is still excluded — see
	// [ReasonClusterMemberNotSupported] — but the report says it could not
	// determine which kind of cluster it was.
	Known bool `json:"known,omitempty"`
	// ServerlessV2MinACU is the min-ACU floor: the Aurora analogue of Batch's
	// minvCpus, a cost that is a configuration choice rather than a demand
	// signal. Carried so the Aurora refusal can name the lever a future unit
	// would look at.
	ServerlessV2MinACU float64 `json:"serverlessV2MinACU,omitempty"`
}

func clusterInfoFor(clusters map[string]DBClusterRecord, id string) ClusterInfo {
	if id == "" {
		return ClusterInfo{}
	}
	cl, ok := clusters[id]
	if !ok {
		return ClusterInfo{ID: id}
	}
	return ClusterInfo{
		ID: id, Engine: cl.Engine, Mode: cl.EngineMode, Known: true,
		ServerlessV2MinACU: cl.ServerlessV2MinCapacity,
	}
}

func recordID(r DBInstanceRecord) string {
	if r.DBInstanceArn != "" {
		return r.DBInstanceArn
	}
	return r.DBInstanceIdentifier
}

func targetRef(scope string, d DBInstance) domain.TargetRef {
	id := d.ARN
	if id == "" {
		id = d.Identifier
	}
	return domain.TargetRef{Domain: Kind, Scope: scope, ID: id, Name: d.Identifier}
}

func instanceFromRecord(r DBInstanceRecord, region string) DBInstance {
	return DBInstance{
		ARN:                    recordID(r),
		Identifier:             r.DBInstanceIdentifier,
		Class:                  r.DBInstanceClass,
		Engine:                 r.Engine,
		EngineVersion:          r.EngineVersion,
		LicenseModel:           r.LicenseModel,
		Status:                 r.DBInstanceStatus,
		Region:                 region,
		MultiAZ:                r.MultiAZ,
		ClusterID:              r.DBClusterIdentifier,
		ReplicaSource:          r.ReadReplicaSourceDBInstanceIdentifier,
		Replicas:               append([]string(nil), r.ReadReplicaDBInstanceIdentifiers...),
		AvailabilityZone:       r.AvailabilityZone,
		AllocatedStorageGiB:    r.AllocatedStorage,
		MaxAllocatedStorageGiB: r.MaxAllocatedStorage,
		StorageType:            r.StorageType,
		IOPS:                   r.Iops,
		StorageThroughputMBps:  r.StorageThroughput,
		InstanceCreateTime:     r.InstanceCreateTime,
		Tags:                   copyTags(r.TagList),
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
func sortRecords(rs []DBInstanceRecord) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].DBInstanceArn != rs[j].DBInstanceArn {
			return rs[i].DBInstanceArn < rs[j].DBInstanceArn
		}
		return rs[i].DBInstanceIdentifier < rs[j].DBInstanceIdentifier
	})
}
