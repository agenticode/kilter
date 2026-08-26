package ebs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// --- Cloud seams -----------------------------------------------------------
//
// Three read shapes and one write shape, over plain Go structs. No AWS SDK
// type appears here and this package imports no SDK: pkg/provider isolates
// aws-sdk-go-v2 behind an interface and is wired in cmd/, and this package
// goes one step further by not linking it at all, because the decision path
// must build into an air-gapped binary. The adapter that fills these structs
// from *ec2.Client and *cloudwatch.Client is cmd/ wiring a later unit adds
// (see FINDINGS.md).

// Tag is one DescribeVolumes tag, in API order. Kept as a list rather than a
// map because that is what the API returns and what a recorded fixture holds;
// the collector normalizes to a map, first occurrence wins.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Attachment states, as DescribeVolumes reports them.
const (
	AttachmentAttaching = "attaching"
	AttachmentAttached  = "attached"
	AttachmentDetaching = "detaching"
	AttachmentDetached  = "detached"
)

// Volume states, as DescribeVolumes reports them.
const (
	VolumeStateCreating  = "creating"
	VolumeStateAvailable = "available"
	VolumeStateInUse     = "in-use"
	VolumeStateDeleting  = "deleting"
	VolumeStateDeleted   = "deleted"
	VolumeStateError     = "error"
)

// VolumeAttachment is one DescribeVolumes attachment.
type VolumeAttachment struct {
	InstanceID          string `json:"instanceId,omitempty"`
	Device              string `json:"device,omitempty"`
	State               string `json:"state,omitempty"`
	DeleteOnTermination bool   `json:"deleteOnTermination,omitempty"`
}

// VolumeRecord is one DescribeVolumes volume, flattened. Field names track the
// API's, so a recorded fixture reads like the response it came from.
type VolumeRecord struct {
	VolumeID   string `json:"volumeId"`
	VolumeType string `json:"volumeType"`
	SizeGiB    int64  `json:"sizeGiB"`
	// IOPS is what the volume is provisioned for. gp2 reports its
	// size-derived baseline here; gp3/io1/io2 report the provisioned value.
	IOPS int32 `json:"iops,omitempty"`
	// ThroughputMBps is provisioned throughput; gp3 only.
	ThroughputMBps     int32              `json:"throughputMBps,omitempty"`
	State              string             `json:"state,omitempty"`
	AvailabilityZone   string             `json:"availabilityZone,omitempty"`
	Encrypted          bool               `json:"encrypted,omitempty"`
	MultiAttachEnabled bool               `json:"multiAttachEnabled,omitempty"`
	SnapshotID         string             `json:"snapshotId,omitempty"`
	CreateTime         time.Time          `json:"createTime,omitzero"`
	Attachments        []VolumeAttachment `json:"attachments,omitempty"`
	Tags               []Tag              `json:"tags,omitempty"`
}

// Name returns the Name tag, or "".
func (v VolumeRecord) Name() string {
	for _, t := range v.Tags {
		if t.Key == TagName {
			return t.Value
		}
	}
	return ""
}

// TagMap normalizes tags to a map, first occurrence wins (AWS forbids
// duplicate keys; a fixture may still carry them).
func (v VolumeRecord) TagMap() map[string]string {
	if len(v.Tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(v.Tags))
	for _, t := range v.Tags {
		if _, dup := out[t.Key]; !dup {
			out[t.Key] = t.Value
		}
	}
	return out
}

// Region derives the region from the availability zone ("us-east-1a" →
// "us-east-1"), or "" when the AZ is absent or malformed.
func (v VolumeRecord) Region() string { return regionOfAZ(v.AvailabilityZone) }

func regionOfAZ(az string) string {
	az = strings.TrimSpace(az)
	if len(az) < 2 {
		return ""
	}
	last := az[len(az)-1]
	if last < 'a' || last > 'z' {
		return ""
	}
	return az[:len(az)-1]
}

// Tag keys that decide ownership and guardrails, mirroring the Kubernetes
// annotation semantics the node domain already uses.
const (
	TagKilterMode = "kilter.dev/mode"
	TagName       = "Name"
)

// DescribeVolumesInput is the paginating inventory request.
type DescribeVolumesInput struct {
	NextToken  string `json:"nextToken,omitempty"`
	MaxResults int32  `json:"maxResults,omitempty"`
}

// DescribeVolumesOutput is one page. An empty NextToken ends pagination.
type DescribeVolumesOutput struct {
	Volumes   []VolumeRecord `json:"volumes,omitempty"`
	NextToken string         `json:"nextToken,omitempty"`
}

// Modification states, as DescribeVolumesModifications reports them. A volume
// is usable throughout: "modifying" and "optimizing" are online states, which
// is exactly why gp2 → gp3 is the right first non-Kubernetes actuation.
const (
	ModificationModifying  = "modifying"
	ModificationOptimizing = "optimizing"
	ModificationCompleted  = "completed"
	ModificationFailed     = "failed"
)

// VolumeModification is one DescribeVolumesModifications record.
type VolumeModification struct {
	VolumeID          string `json:"volumeId"`
	ModificationState string `json:"modificationState,omitempty"`
	StatusMessage     string `json:"statusMessage,omitempty"`

	TargetVolumeType     string `json:"targetVolumeType,omitempty"`
	TargetSizeGiB        int64  `json:"targetSizeGiB,omitempty"`
	TargetIOPS           int32  `json:"targetIOPS,omitempty"`
	TargetThroughputMBps int32  `json:"targetThroughputMBps,omitempty"`

	OriginalVolumeType     string `json:"originalVolumeType,omitempty"`
	OriginalSizeGiB        int64  `json:"originalSizeGiB,omitempty"`
	OriginalIOPS           int32  `json:"originalIOPS,omitempty"`
	OriginalThroughputMBps int32  `json:"originalThroughputMBps,omitempty"`

	Progress  int32     `json:"progress,omitempty"`
	StartTime time.Time `json:"startTime,omitzero"`
	EndTime   time.Time `json:"endTime,omitzero"`
}

// InFlight reports whether the modification is still running. AWS keeps
// completed records for 30 days, so "a record exists" is not "a change is
// happening".
func (m VolumeModification) InFlight() bool {
	return m.ModificationState == ModificationModifying || m.ModificationState == ModificationOptimizing
}

// At is the time the modification last moved: its end when it has one, else
// its start. It is what the cooldown counts from.
func (m VolumeModification) At() time.Time {
	if !m.EndTime.IsZero() {
		return m.EndTime
	}
	return m.StartTime
}

// DescribeVolumesModificationsInput is the paginating modification request. An
// empty VolumeIDs asks for every volume with a modification record.
type DescribeVolumesModificationsInput struct {
	VolumeIDs  []string `json:"volumeIds,omitempty"`
	NextToken  string   `json:"nextToken,omitempty"`
	MaxResults int32    `json:"maxResults,omitempty"`
}

// DescribeVolumesModificationsOutput is one page.
type DescribeVolumesModificationsOutput struct {
	Modifications []VolumeModification `json:"modifications,omitempty"`
	NextToken     string               `json:"nextToken,omitempty"`
}

// InventoryAPI is the read seam: DescribeVolumes plus
// DescribeVolumesModifications. Both are read-only; nothing in this interface
// can change a volume.
type InventoryAPI interface {
	DescribeVolumes(ctx context.Context, in *DescribeVolumesInput) (*DescribeVolumesOutput, error)
	DescribeVolumesModifications(ctx context.Context, in *DescribeVolumesModificationsInput) (*DescribeVolumesModificationsOutput, error)
}

// MetricDataQuery is one GetMetricData query. ID must match CloudWatch's
// `^[a-z][a-zA-Z0-9_]*$`; the collector generates it and uses it — not the
// label, not position — to route results back to volumes.
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
	// GetMetricData returns one result per query — empty when the metric has
	// no data — so a *missing* result is a fact about the response, not about
	// the metric.
	StatusTruncated = "Truncated"
)

// MetricDataResult is one query's answer. Timestamps and Values are parallel
// and ascending.
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
	Messages  []string           `json:"messages,omitempty"`
	NextToken string             `json:"nextToken,omitempty"`
}

// MetricsAPI is the GetMetricData-shaped seam.
type MetricsAPI interface {
	GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error)
}

// CloudWatch EBS metrics. All are published in the AWS/EBS namespace with a
// single VolumeId dimension.
const (
	NamespaceEBS = "AWS/EBS"

	MetricVolumeReadOps    = "VolumeReadOps"    // Sum: completed read operations in the period
	MetricVolumeWriteOps   = "VolumeWriteOps"   // Sum: completed write operations
	MetricVolumeReadBytes  = "VolumeReadBytes"  // Sum: bytes read
	MetricVolumeWriteBytes = "VolumeWriteBytes" // Sum: bytes written
	MetricBurstBalance     = "BurstBalance"     // Average: percent of the gp2 burst bucket
)

// Statistics requested per metric.
const (
	StatSum     = "Sum"
	StatAverage = "Average"
)

// Derived sample metrics, the vocabulary §5.2 names for this domain. The
// collector converts CloudWatch's per-period counters into rates once, so the
// brain never has to know a period to read a sample.
const (
	SampleIOPS            = "iops"
	SampleThroughputMBps  = "throughput-mbps"
	SampleBurstBalancePct = "burst-balance-pct"
)

// Evidence sources.
const (
	SourceCloudWatch     = "cloudwatch"
	SourceDescribeVolume = "describe-volumes"
	SourceRateCard       = "ebs-rates"
)

// PeriodSeconds is EBS's CloudWatch publication period. EBS publishes every
// 5 minutes at no charge; 1-minute data requires detailed monitoring, which
// this unit does not assume. Requesting 60 s where CloudWatch publishes every
// 300 s returns 300 s data with four fifths of the datapoints empty, which
// reads as a coverage failure rather than as the resolution limit it is.
const PeriodSeconds int32 = 300

// Collector budgets.
const (
	// MaxSeriesPerCall is CloudWatch's GetMetricData query limit.
	MaxSeriesPerCall = 500
	// DefaultSeriesPerCall is the batch size used unless configured.
	DefaultSeriesPerCall = 100
	// DefaultMaxPages bounds pagination on every seam.
	DefaultMaxPages = 50
	// DefaultWindow is the metric lookback. It is twice the 7-day minimum
	// window a parity-reduction needs, so a domain restart does not spend a
	// week refusing.
	DefaultWindow = 14 * 24 * time.Hour
)

// CollectorConfig tunes the collector.
type CollectorConfig struct {
	// Scope is the snapshot scope, conventionally "accountID/region".
	Scope string
	// Region labels the snapshot; when empty it is derived from the first
	// volume's availability zone.
	Region string
	// Window is the metric lookback. Zero means DefaultWindow.
	Window time.Duration
	// MaxSeriesPerCall caps the GetMetricData batch. Zero means
	// DefaultSeriesPerCall; above MaxSeriesPerCall is rejected.
	MaxSeriesPerCall int
	// MaxPages bounds pagination on every seam. Zero means DefaultMaxPages.
	MaxPages int
	// CollectBurstBalance requests gp2's BurstBalance series. It costs one
	// query per gp2 volume and is the only direct evidence that a volume is
	// living on credits it will one day run out of.
	CollectBurstBalance bool
	// MeteredTypes lists the volume types metrics are requested for, lowercased.
	// Empty means {"gp2"}: gp2 is the only type this unit can act on, and a
	// query for a volume nothing can be done about is a bill for nothing.
	MeteredTypes []string
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
	return DefaultSeriesPerCall
}

func (c CollectorConfig) metered() map[string]bool {
	out := map[string]bool{}
	if len(c.MeteredTypes) == 0 {
		out[VolumeTypeGP2] = true
		return out
	}
	for _, t := range c.MeteredTypes {
		out[strings.ToLower(strings.TrimSpace(t))] = true
	}
	return out
}

// Collector turns the read seams into one domain.Snapshot. It holds no clock,
// no global state and no mutable configuration; two Collectors over the same
// fixtures produce identical snapshots.
type Collector struct {
	inv InventoryAPI
	met MetricsAPI
	cfg CollectorConfig
}

// NewCollector builds a collector. Both seams are required: a metrics-less
// collector would produce volumes with no evidence at all, which the domain
// refuses on anyway — failing here says why, once.
func NewCollector(inv InventoryAPI, met MetricsAPI, cfg CollectorConfig) (*Collector, error) {
	if inv == nil {
		return nil, fmt.Errorf("ebs: collector needs an inventory seam")
	}
	if met == nil {
		return nil, fmt.Errorf("ebs: collector needs a metrics seam")
	}
	if cfg.Window < 0 {
		return nil, fmt.Errorf("ebs: collector window must not be negative (got %s)", cfg.Window)
	}
	if cfg.MaxSeriesPerCall > MaxSeriesPerCall {
		return nil, fmt.Errorf("ebs: MaxSeriesPerCall %d exceeds the GetMetricData limit of %d",
			cfg.MaxSeriesPerCall, MaxSeriesPerCall)
	}
	return &Collector{inv: inv, met: met, cfg: cfg}, nil
}

// Collect reads inventory, modification records and metrics, and returns a
// snapshot for the window ending at now.
//
// It never returns a nil snapshot with a nil error, and it never blocks the
// brain on a partial cloud: an exhausted page budget or a short metric
// response marks the snapshot stale instead of failing. A transport error on
// the first inventory call IS returned, because a failed call is not evidence
// of an empty account.
func (c *Collector) Collect(ctx context.Context, now time.Time) (*domain.Snapshot, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("ebs: collect needs a caller-supplied now (this package has no clock)")
	}
	start := now.Add(-c.cfg.window())

	vols, stale, staleReason, err := c.describeVolumes(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].VolumeID < vols[j].VolumeID })

	mods, modStale, modReason := c.describeModifications(ctx)
	if modStale {
		stale, staleReason = true, joinReason(staleReason, modReason)
	}

	region := c.cfg.Region
	if region == "" {
		for _, v := range vols {
			if r := v.Region(); r != "" {
				region = r
				break
			}
		}
	}
	scope := c.cfg.Scope
	if scope == "" {
		scope = region
	}

	metered := c.cfg.metered()
	targets := make([]domain.Target, 0, len(vols))
	wanted := make([]VolumeRecord, 0, len(vols))
	for _, v := range vols {
		t := domain.Target{
			Ref:    domain.TargetRef{Domain: Kind, Scope: scope, ID: v.VolumeID, Name: v.Name()},
			Spec:   SpecOf(v),
			Labels: v.TagMap(),
		}
		if m, ok := mods[v.VolumeID]; ok {
			t.Spec = withModification(t.Spec, m)
		}
		if !metered[strings.ToLower(v.VolumeType)] {
			// Declared, not hidden: the domain reports "no metrics" for this
			// volume, and a reader can tell it from a volume whose metrics
			// were requested and came back empty.
			t.Blind = []string{SampleIOPS, SampleThroughputMBps}
		} else {
			wanted = append(wanted, v)
		}
		targets = append(targets, t)
	}

	samples, blind, metStale, metReason := c.collectMetrics(ctx, scope, wanted, start, now)
	if metStale {
		stale, staleReason = true, joinReason(staleReason, metReason)
	}
	for i := range targets {
		if b := blind[targets[i].Ref.ID]; len(b) > 0 {
			targets[i].Blind = append(targets[i].Blind, b...)
			sort.Strings(targets[i].Blind)
		}
	}

	return &domain.Snapshot{
		Domain:      Kind,
		Scope:       scope,
		Timestamp:   now,
		Targets:     targets,
		Samples:     samples,
		Stale:       stale,
		StaleReason: staleReason,
	}, nil
}

func joinReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// SpecOf renders a volume record as a domain Spec. It is exported because the
// actuator compares a live volume against a step's recorded From with it, and
// two different renderings of "the same volume" would break idempotency.
func SpecOf(v VolumeRecord) domain.Spec {
	attrs := map[string]string{
		AttrVolumeType: strings.ToLower(v.VolumeType),
		AttrSizeGiB:    strconv.FormatInt(v.SizeGiB, 10),
	}
	if v.IOPS > 0 {
		attrs[AttrIOPS] = strconv.FormatInt(int64(v.IOPS), 10)
	}
	if v.ThroughputMBps > 0 {
		attrs[AttrThroughputMBps] = strconv.FormatInt(int64(v.ThroughputMBps), 10)
	}
	if v.State != "" {
		attrs[AttrState] = strings.ToLower(v.State)
	}
	if v.AvailabilityZone != "" {
		attrs[AttrAZ] = v.AvailabilityZone
	}
	if v.MultiAttachEnabled {
		attrs[AttrMultiAttach] = "true"
	}
	if len(v.Attachments) > 0 {
		ids := make([]string, 0, len(v.Attachments))
		states := make([]string, 0, len(v.Attachments))
		for _, a := range v.Attachments {
			if a.InstanceID != "" {
				ids = append(ids, a.InstanceID)
			}
			if a.State != "" {
				states = append(states, strings.ToLower(a.State))
			}
		}
		sort.Strings(ids)
		sort.Strings(states)
		if len(ids) > 0 {
			attrs[AttrAttachedTo] = strings.Join(ids, ",")
		}
		if len(states) > 0 {
			attrs[AttrAttachmentState] = strings.Join(states, ",")
		}
	}
	return domain.Spec{Attrs: attrs}
}

// withModification stamps the volume's newest modification record onto its
// spec. The state and the time it last moved are what the domain's in-progress
// and cooldown refusals read.
func withModification(s domain.Spec, m VolumeModification) domain.Spec {
	if m.ModificationState != "" {
		s = s.WithAttr(AttrModificationState, strings.ToLower(m.ModificationState))
	}
	if at := m.At(); !at.IsZero() {
		s = s.WithAttr(AttrModificationAt, at.UTC().Format(time.RFC3339))
	}
	return s
}

// describeVolumes pages the inventory seam.
func (c *Collector) describeVolumes(ctx context.Context) ([]VolumeRecord, bool, string, error) {
	var out []VolumeRecord
	token := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= c.cfg.maxPages() {
			return out, true, fmt.Sprintf("inventory page budget of %d exhausted: volume list is partial",
				c.cfg.maxPages()), nil
		}
		res, err := c.inv.DescribeVolumes(ctx, &DescribeVolumesInput{NextToken: token})
		if err != nil {
			if page == 0 {
				return nil, false, "", fmt.Errorf("ebs: describe volumes: %w", err)
			}
			return out, true, fmt.Sprintf("inventory paging stopped after %d page(s): %v", page, err), nil
		}
		if res == nil {
			return out, true, "inventory seam returned no response", nil
		}
		out = append(out, res.Volumes...)
		if res.NextToken == "" {
			return out, false, "", nil
		}
		if seen[res.NextToken] {
			return out, true, "inventory pager returned a repeating page token: volume list is partial", nil
		}
		seen[res.NextToken] = true
		token = res.NextToken
	}
}

// describeModifications pages the modification seam and keeps the newest
// record per volume. A failure here is never fatal — it costs the in-progress
// and cooldown refusals their evidence, which the domain reports as a blind
// spot and refuses on, rather than acting blind.
func (c *Collector) describeModifications(ctx context.Context) (map[string]VolumeModification, bool, string) {
	out := map[string]VolumeModification{}
	token := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= c.cfg.maxPages() {
			return out, true, fmt.Sprintf("modification page budget of %d exhausted", c.cfg.maxPages())
		}
		res, err := c.inv.DescribeVolumesModifications(ctx, &DescribeVolumesModificationsInput{NextToken: token})
		if err != nil {
			return out, true, fmt.Sprintf("describe volume modifications failed: %v", err)
		}
		if res == nil {
			return out, true, "modification seam returned no response"
		}
		for _, m := range res.Modifications {
			if m.VolumeID == "" {
				continue
			}
			prev, ok := out[m.VolumeID]
			// Newest wins; an in-flight record always wins, because "a change
			// is happening now" outranks "a change happened".
			if !ok || m.InFlight() || (!prev.InFlight() && m.At().After(prev.At())) {
				out[m.VolumeID] = m
			}
		}
		if res.NextToken == "" {
			return out, false, ""
		}
		if seen[res.NextToken] {
			return out, true, "modification pager returned a repeating page token"
		}
		seen[res.NextToken] = true
		token = res.NextToken
	}
}

// metricSpec is one requested series.
type metricSpec struct {
	metric string
	stat   string
}

func (c *Collector) metricSpecs() []metricSpec {
	specs := []metricSpec{
		{MetricVolumeReadOps, StatSum},
		{MetricVolumeWriteOps, StatSum},
		{MetricVolumeReadBytes, StatSum},
		{MetricVolumeWriteBytes, StatSum},
	}
	if c.cfg.CollectBurstBalance {
		specs = append(specs, metricSpec{MetricBurstBalance, StatAverage})
	}
	return specs
}

// collectMetrics requests every series for every metered volume, in batches,
// and folds CloudWatch's per-period counters into rate samples.
func (c *Collector) collectMetrics(ctx context.Context, scope string, vols []VolumeRecord,
	start, end time.Time) ([]domain.Sample, map[string][]string, bool, string) {

	blind := map[string][]string{}
	if len(vols) == 0 {
		return nil, blind, false, ""
	}
	specs := c.metricSpecs()

	type qref struct {
		volumeID string
		metric   string
	}
	queries := make([]MetricDataQuery, 0, len(vols)*len(specs))
	refs := make(map[string]qref, len(vols)*len(specs))
	for vi, v := range vols {
		for si, s := range specs {
			id := fmt.Sprintf("q%d_%d", vi, si)
			queries = append(queries, MetricDataQuery{
				ID:            id,
				Namespace:     NamespaceEBS,
				MetricName:    s.metric,
				Dimensions:    map[string]string{"VolumeId": v.VolumeID},
				PeriodSeconds: PeriodSeconds,
				Stat:          s.stat,
				Label:         v.VolumeID + "/" + s.metric,
			})
			refs[id] = qref{v.VolumeID, s.metric}
		}
	}

	// series[volumeID][metric][unix] = value. Keyed by timestamp so read and
	// write counters can be paired exactly, never by position.
	series := map[string]map[string]map[int64]float64{}
	statuses := map[string]map[string]string{}
	record := func(r MetricDataResult) {
		ref, ok := refs[r.ID]
		if !ok {
			return
		}
		if statuses[ref.volumeID] == nil {
			statuses[ref.volumeID] = map[string]string{}
		}
		st := r.StatusCode
		if st == "" {
			st = StatusComplete
		}
		statuses[ref.volumeID][ref.metric] = st
		if series[ref.volumeID] == nil {
			series[ref.volumeID] = map[string]map[int64]float64{}
		}
		if series[ref.volumeID][ref.metric] == nil {
			series[ref.volumeID][ref.metric] = map[int64]float64{}
		}
		n := len(r.Timestamps)
		if len(r.Values) < n {
			n = len(r.Values)
		}
		for i := 0; i < n; i++ {
			series[ref.volumeID][ref.metric][r.Timestamps[i].UTC().Unix()] = r.Values[i]
		}
	}

	stale, reason := false, ""
	batch := c.cfg.batchSize()
	for from := 0; from < len(queries); from += batch {
		to := from + batch
		if to > len(queries) {
			to = len(queries)
		}
		chunk := queries[from:to]
		answered := map[string]bool{}
		token := ""
		pageSeen := map[string]bool{}
		for page := 0; ; page++ {
			if page >= c.cfg.maxPages() {
				stale = true
				reason = joinReason(reason, fmt.Sprintf("metric page budget of %d exhausted", c.cfg.maxPages()))
				break
			}
			res, err := c.met.GetMetricData(ctx, &GetMetricDataInput{
				Queries: chunk, StartTime: start, EndTime: end, NextToken: token,
			})
			if err != nil {
				stale = true
				reason = joinReason(reason, fmt.Sprintf("get metric data failed: %v", err))
				break
			}
			if res == nil {
				stale = true
				reason = joinReason(reason, "metrics seam returned no response")
				break
			}
			for _, r := range res.Results {
				answered[r.ID] = true
				record(r)
			}
			if res.NextToken == "" || pageSeen[res.NextToken] {
				break
			}
			pageSeen[res.NextToken] = true
			token = res.NextToken
		}
		// A query CloudWatch did not answer is a truncated response, not an
		// empty metric. Marking it Truncated is what stops the domain from
		// reading silence as "this volume does no I/O".
		for _, q := range chunk {
			if answered[q.ID] {
				continue
			}
			stale = true
			record(MetricDataResult{ID: q.ID, StatusCode: StatusTruncated})
		}
	}
	if stale && reason == "" {
		reason = "metric response omitted some requested series"
	}

	var samples []domain.Sample
	for _, v := range vols {
		ref := domain.TargetRef{Domain: Kind, Scope: scope, ID: v.VolumeID, Name: v.Name()}
		vs := series[v.VolumeID]
		st := statuses[v.VolumeID]

		ops := mergeCounters(vs[MetricVolumeReadOps], vs[MetricVolumeWriteOps])
		bytes := mergeCounters(vs[MetricVolumeReadBytes], vs[MetricVolumeWriteBytes])
		period := float64(PeriodSeconds)

		iopsOK := complete(st, MetricVolumeReadOps) && complete(st, MetricVolumeWriteOps) && len(ops) > 0
		tputOK := complete(st, MetricVolumeReadBytes) && complete(st, MetricVolumeWriteBytes) && len(bytes) > 0
		if !iopsOK {
			blind[v.VolumeID] = append(blind[v.VolumeID], SampleIOPS)
		}
		if !tputOK {
			blind[v.VolumeID] = append(blind[v.VolumeID], SampleThroughputMBps)
		}
		if iopsOK {
			for _, ts := range sortedKeys(ops) {
				samples = append(samples, domain.Sample{
					Ref: ref, Metric: SampleIOPS, Value: ops[ts] / period,
					Timestamp: time.Unix(ts, 0).UTC(), WindowSeconds: PeriodSeconds,
				})
			}
		}
		if tputOK {
			for _, ts := range sortedKeys(bytes) {
				samples = append(samples, domain.Sample{
					Ref: ref, Metric: SampleThroughputMBps, Value: bytes[ts] / period / MiB,
					Timestamp: time.Unix(ts, 0).UTC(), WindowSeconds: PeriodSeconds,
				})
			}
		}
		if bb := vs[MetricBurstBalance]; len(bb) > 0 && complete(st, MetricBurstBalance) {
			for _, ts := range sortedKeys(bb) {
				samples = append(samples, domain.Sample{
					Ref: ref, Metric: SampleBurstBalancePct, Value: bb[ts],
					Timestamp: time.Unix(ts, 0).UTC(), WindowSeconds: PeriodSeconds,
				})
			}
		}
	}
	return samples, blind, stale, reason
}

// complete reports whether a metric came back whole. An absent status means
// the query was never issued (an unmetered volume), which is not completeness.
func complete(st map[string]string, metric string) bool {
	s, ok := st[metric]
	return ok && s == StatusComplete
}

// mergeCounters adds two per-timestamp counter series. A timestamp present in
// one and absent in the other contributes what it has: EBS publishes read and
// write counters independently, and an absent write counter means no writes,
// not unknown writes.
func mergeCounters(a, b map[int64]float64) map[int64]float64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[int64]float64, len(a)+len(b))
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

// sortedKeys returns map keys in ascending order. Every iteration over a map
// in this package goes through a sort, so no output depends on Go's randomized
// map order.
func sortedKeys(m map[int64]float64) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DomainCollector adapts [Collector] to domain.Collector, which takes no clock
// argument. The clock is supplied by the caller here, at wiring time, so this
// package still reads no clock of its own.
type DomainCollector struct {
	c   *Collector
	now func() time.Time
}

// NewDomainCollector wraps a collector with a caller-supplied clock.
func NewDomainCollector(c *Collector, now func() time.Time) (*DomainCollector, error) {
	if c == nil {
		return nil, fmt.Errorf("ebs: nil collector")
	}
	if now == nil {
		return nil, fmt.Errorf("ebs: collector needs a clock (this package has none)")
	}
	return &DomainCollector{c: c, now: now}, nil
}

// Domain implements domain.Collector.
func (d *DomainCollector) Domain() domain.Kind { return Kind }

// Collect implements domain.Collector.
func (d *DomainCollector) Collect(ctx context.Context) (*domain.Snapshot, error) {
	return d.c.Collect(ctx, d.now())
}

var _ domain.Collector = (*DomainCollector)(nil)
