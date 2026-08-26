package provider

// The live CloudWatch adapter for pkg/rds's metrics seam.
//
// This is the fifth independent derivation of a CloudWatch seam in this tree
// (pkg/ec2, pkg/ebs, pkg/ecs, pkg/lambda, pkg/rds) and the first that talks to
// the SDK. pkg/rds owns the query construction, the 500-per-call batching, the
// ID routing and the page loop; this file owns the type translation and one
// decision the translation cannot avoid — what an EMPTY status code means.
//
// pkg/rds/collect.go reads a delivered series like this:
//
//	ser.Status = r.StatusCode
//	if ser.Status == "" { ser.Status = StatusComplete }
//	ser.Partial = ser.Status != StatusComplete
//
// so an unset status becomes "Complete" and the series is treated as whole
// evidence. That is the string form of the nil-pointer trap: "CloudWatch did
// not say" would become "CloudWatch said the series is complete", and a
// complete-looking DatabaseConnections series is exactly what an idle verdict
// is made of. This adapter therefore reports an unset status as PartialData —
// the same reading pkg/rds itself applies to a result that never arrived.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	krds "github.com/agenticode/kilter/pkg/rds"
)

// DefaultMetricsCallTimeout bounds one cloudwatch:GetMetricData call. It is
// longer than [DefaultRDSCallTimeout] because the request is much larger:
// pkg/rds batches up to 500 queries per call, each spanning the whole
// observation window.
const DefaultMetricsCallTimeout = 60 * time.Second

// cloudwatchSDK is the minimal CloudWatch surface pkg/rds needs, satisfied by
// *cloudwatch.Client and by test fakes. One operation, and it is a GET.
type cloudwatchSDK interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput,
		opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// CloudWatchAPI adapts *cloudwatch.Client to [krds.MetricsAPI].
//
// The seam is OPTIONAL: pkg/rds accepts a nil MetricsAPI and every instance
// then refuses with no-metric-evidence, which is a complete report rather than
// a failed one. Note that a mid-collection GetMetricData FAILURE is not the
// same thing — pkg/rds returns it as a hard error from Collect. cmd/ must
// therefore decide before collecting whether the credential holds
// cloudwatch:GetMetricData; see RDS-ADAPTER-FINDINGS.md §5.
type CloudWatchAPI struct {
	api     cloudwatchSDK
	region  string
	timeout time.Duration
	notes   noteSet
}

var _ krds.MetricsAPI = (*CloudWatchAPI)(nil)

// NewCloudWatchAPI loads AWS credentials from the environment and targets one
// region — the SAME region as the [RDSAPI] it is paired with, because a metric
// is published in the region its database lives in and a cross-region pairing
// silently returns empty series for every instance.
func NewCloudWatchAPI(ctx context.Context, region string) (*CloudWatchAPI, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		return nil, fmt.Errorf("provider cloudwatch: region required: RDS metrics are published in " +
			"the region the database lives in, and a mismatched region returns empty series that read " +
			"as an idle database")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("provider cloudwatch: load aws config: %w", err)
	}
	return newCloudWatchAPI(cloudwatch.NewFromConfig(cfg), region), nil
}

// newCloudWatchAPI is the test seam.
func newCloudWatchAPI(client cloudwatchSDK, region string) *CloudWatchAPI {
	return &CloudWatchAPI{api: client, region: region, timeout: DefaultMetricsCallTimeout}
}

// Region is the region this adapter reads.
func (a *CloudWatchAPI) Region() string { return a.region }

// SetCallTimeout overrides the per-call deadline. A non-positive value
// restores [DefaultMetricsCallTimeout].
func (a *CloudWatchAPI) SetCallTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultMetricsCallTimeout
	}
	a.timeout = d
}

// Notes returns what this adapter observed that [krds.GetMetricDataOutput] has
// no field for — CloudWatch's own Messages list, unidentified results, and
// results delivered with no status. cmd/ must render them beside
// Snapshot.Warnings.
func (a *CloudWatchAPI) Notes() []string { return a.notes.list() }

// GetMetricData implements [krds.MetricsAPI]: one page per call, with the
// caller's NextToken in and CloudWatch's NextToken out.
func (a *CloudWatchAPI) GetMetricData(ctx context.Context,
	in *krds.GetMetricDataInput) (*krds.GetMetricDataOutput, error) {

	if in == nil {
		in = &krds.GetMetricDataInput{}
	}
	if len(in.Queries) == 0 {
		// CloudWatch rejects an empty query list. Nothing was asked, so
		// nothing was answered — and no call is made.
		return &krds.GetMetricDataOutput{}, nil
	}
	// StartTime and EndTime are required by the API. Refused here rather than
	// sent, because AWS answers a zero time with a validation error that reads
	// nothing like "the caller passed an empty window".
	if in.StartTime.IsZero() || in.EndTime.IsZero() || !in.EndTime.After(in.StartTime) {
		return nil, fmt.Errorf("provider cloudwatch: GetMetricData needs a positive window, got %s..%s",
			in.StartTime.Format(time.RFC3339), in.EndTime.Format(time.RFC3339))
	}

	queries := make([]cwtypes.MetricDataQuery, 0, len(in.Queries))
	for _, q := range in.Queries {
		if q.PeriodSeconds <= 0 {
			return nil, fmt.Errorf("provider cloudwatch: query %q asks for a %d-second period",
				q.ID, q.PeriodSeconds)
		}
		period, id, ns, name, stat := q.PeriodSeconds, q.ID, q.Namespace, q.MetricName, q.Stat
		queries = append(queries, cwtypes.MetricDataQuery{
			Id: &id,
			MetricStat: &cwtypes.MetricStat{
				Metric: &cwtypes.Metric{
					Namespace:  &ns,
					MetricName: &name,
					Dimensions: dimensions(q.Dimensions),
				},
				Period: &period,
				Stat:   &stat,
				// Unit is deliberately unset. Naming a unit FILTERS datapoints
				// to those published with it, so a wrong guess returns an
				// empty series — which reads as a quiet database.
			},
			ReturnData: boolPtr(true),
		})
	}

	start, end, token := in.StartTime, in.EndTime, in.NextToken
	cctx, cancel := context.WithTimeout(ctx, a.callTimeout())
	defer cancel()
	res, err := a.api.GetMetricData(cctx, &cloudwatch.GetMetricDataInput{
		MetricDataQueries: queries,
		StartTime:         &start,
		EndTime:           &end,
		NextToken:         strPtr(token),
		// ScanBy and MaxDatapoints are deliberately left at CloudWatch's
		// defaults. See RDS-ADAPTER-FINDINGS.md §7.1: a response that exceeds
		// the datapoint budget is paged, and pkg/rds keeps the FIRST result
		// it sees per query ID, so which end of a split series survives is a
		// pkg/rds question this adapter must not answer by picking a scan
		// order.
	})
	if err != nil {
		return nil, fmt.Errorf("provider cloudwatch: GetMetricData (%d queries): %w", len(queries), err)
	}
	if res == nil {
		return nil, nil
	}

	out := &krds.GetMetricDataOutput{NextToken: str(res.NextToken)}
	// CloudWatch reports over-large or malformed requests in Messages rather
	// than as an error. GetMetricDataOutput has nowhere to put them.
	a.noteMessages("GetMetricData", res.Messages)
	for _, r := range res.MetricDataResults {
		id := str(r.Id)
		if id == "" {
			// pkg/rds routes results BY ID — not by label, not by position.
			// An unidentified result can only land in a slot no query owns,
			// leaving the real query unanswered and its series Truncated,
			// which is the safe outcome and an invisible one.
			a.notes.add("cloudwatch:GetMetricData returned a result with no Id; it cannot be routed " +
				"to the query that asked for it, so that query's series is reported as truncated " +
				"rather than empty")
		}
		a.noteMessages("result "+orUnnamed(id), r.Messages)
		out.Results = append(out.Results, krds.MetricDataResult{
			ID:         id,
			Timestamps: append([]time.Time(nil), r.Timestamps...),
			Values:     append([]float64(nil), r.Values...),
			StatusCode: a.statusCode(r.StatusCode, id),
		})
	}
	return out, nil
}

// statusCode translates CloudWatch's StatusCode enum, resolving the unset case
// AWAY from "Complete".
//
// CloudWatch defines Complete, PartialData, InternalError and Forbidden.
// Anything that is not Complete makes the series partial in pkg/rds, which is
// the correct handling for all three of the others, so they pass through
// verbatim. The empty string does NOT pass through: pkg/rds turns it into
// Complete, and a series nobody vouched for must not become evidence.
func (a *CloudWatchAPI) statusCode(sc cwtypes.StatusCode, id string) string {
	if strings.TrimSpace(string(sc)) == "" {
		a.notes.add("cloudwatch:GetMetricData returned a result for query %s with no status code; it "+
			"is reported as %s rather than complete, because an unvouched-for series must not become "+
			"evidence", orUnnamed(id), krds.StatusPartialData)
		return krds.StatusPartialData
	}
	return string(sc)
}

func (a *CloudWatchAPI) noteMessages(where string, msgs []cwtypes.MessageData) {
	for _, m := range msgs {
		code, val := str(m.Code), str(m.Value)
		if code == "" && val == "" {
			continue
		}
		a.notes.add("cloudwatch:%s reported %q: %s", where, code, val)
	}
}

func (a *CloudWatchAPI) callTimeout() time.Duration {
	if a.timeout <= 0 {
		return DefaultMetricsCallTimeout
	}
	return a.timeout
}

// dimensions converts the seam's map to CloudWatch's list, sorted by name.
// The sort is not cosmetic: Go randomizes map iteration, and an unsorted
// dimension list makes two identical collections issue two different requests,
// which defeats every replay and every diff this tree relies on.
func dimensions(in map[string]string) []cwtypes.Dimension {
	if len(in) == 0 {
		return nil
	}
	names := make([]string, 0, len(in))
	for k := range in {
		if k != "" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	out := make([]cwtypes.Dimension, 0, len(names))
	for _, n := range names {
		name, value := n, in[n]
		out = append(out, cwtypes.Dimension{Name: &name, Value: &value})
	}
	return out
}

func boolPtr(b bool) *bool { return &b }
