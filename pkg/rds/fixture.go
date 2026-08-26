package rds

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Fixture replays recorded API responses through all three seams, with real
// pagination, real truncation and real empty-account behavior. It exists so
// the collector can be exercised end to end without an AWS SDK, a network, or
// a credential — and so cmd/'s eventual wiring has something to test against.
//
// It is exported for the same reason pkg/ec2's, pkg/ebs's and pkg/lambda's
// are: the seams are the contract, and a contract nobody outside the package
// can exercise is not a contract.
type Fixture struct {
	// Instances is the recorded DescribeDBInstances inventory.
	Instances []DBInstanceRecord
	// Clusters is the recorded DescribeDBClusters inventory.
	Clusters []DBClusterRecord
	// Tags maps a DB instance ARN to its ListTagsForResource answer.
	Tags map[string]map[string]string
	// Metrics maps "<dbInstanceIdentifier>/<metricName>" to recorded
	// datapoints.
	Metrics map[string][]Point
	// Reservations is the recorded DescribeReservedDBInstances inventory.
	Reservations []ReservedDBInstanceRecord
	// PageSize splits every paginated response; 0 means one page.
	PageSize int

	// InstancesErr fails DescribeDBInstances — the one hard dependency.
	InstancesErr error
	// ClustersErr fails DescribeDBClusters, which must degrade rather than
	// break: a member is still excluded, just under a more cautious name.
	ClustersErr error
	// TagsErr fails ListTagsForResource for a specific ARN.
	TagsErr map[string]error
	// MetricsErr fails GetMetricData.
	MetricsErr error
	// ReservationsErr fails DescribeReservedDBInstances.
	ReservationsErr error
	// DropResults omits the first N results from every GetMetricData page,
	// reproducing a TRUNCATED response — the case where a missing result must
	// be read as "we were not told", not as "the metric is empty". This is the
	// fixture knob that proves an idle verdict cannot be manufactured out of
	// silence.
	DropResults int

	// Calls counts seam invocations, so a test can assert the collector's
	// bounds actually bound something.
	Calls struct {
		DescribeDBInstances         int
		DescribeDBClusters          int
		ListTagsForResource         int
		GetMetricData               int
		DescribeReservedDBInstances int
	}
}

// DescribeDBInstances implements [InventoryAPI].
func (f *Fixture) DescribeDBInstances(ctx context.Context,
	in *DescribeDBInstancesInput) (*DescribeDBInstancesOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.DescribeDBInstances++
	if f.InstancesErr != nil {
		return nil, f.InstancesErr
	}
	start, err := offsetOf(in.Marker)
	if err != nil {
		return nil, err
	}
	end, next := paginate(len(f.Instances), start, f.PageSize)
	return &DescribeDBInstancesOutput{DBInstances: f.Instances[min(start, len(f.Instances)):end],
		Marker: next}, nil
}

// DescribeDBClusters implements [InventoryAPI].
func (f *Fixture) DescribeDBClusters(ctx context.Context,
	in *DescribeDBClustersInput) (*DescribeDBClustersOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.DescribeDBClusters++
	if f.ClustersErr != nil {
		return nil, f.ClustersErr
	}
	start, err := offsetOf(in.Marker)
	if err != nil {
		return nil, err
	}
	end, next := paginate(len(f.Clusters), start, f.PageSize)
	return &DescribeDBClustersOutput{DBClusters: f.Clusters[min(start, len(f.Clusters)):end],
		Marker: next}, nil
}

// ListTagsForResource implements [InventoryAPI].
func (f *Fixture) ListTagsForResource(ctx context.Context,
	in *ListTagsForResourceInput) (*ListTagsForResourceOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.ListTagsForResource++
	if err := f.TagsErr[in.ResourceName]; err != nil {
		return nil, err
	}
	return &ListTagsForResourceOutput{TagList: copyTags(f.Tags[in.ResourceName])}, nil
}

// GetMetricData implements [MetricsAPI]. It honours the requested window the
// way CloudWatch does — datapoints outside it are not returned — so a fixture
// cannot smuggle evidence past a window clamp.
func (f *Fixture) GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.GetMetricData++
	if f.MetricsErr != nil {
		return nil, f.MetricsErr
	}
	out := &GetMetricDataOutput{}
	for i, q := range in.Queries {
		if i < f.DropResults {
			continue // a query with no answer: truncation, not emptiness
		}
		key := q.Dimensions["DBInstanceIdentifier"] + "/" + q.MetricName
		r := MetricDataResult{ID: q.ID, StatusCode: StatusComplete}
		for _, p := range f.Metrics[key] {
			if !in.StartTime.IsZero() && p.At.Before(in.StartTime) {
				continue
			}
			if !in.EndTime.IsZero() && p.At.After(in.EndTime) {
				continue
			}
			r.Timestamps = append(r.Timestamps, p.At)
			r.Values = append(r.Values, p.Value)
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// DescribeReservedDBInstances implements [CommitmentAPI].
func (f *Fixture) DescribeReservedDBInstances(ctx context.Context,
	in *DescribeReservedDBInstancesInput) (*DescribeReservedDBInstancesOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.DescribeReservedDBInstances++
	if f.ReservationsErr != nil {
		return nil, f.ReservationsErr
	}
	start, err := offsetOf(in.Marker)
	if err != nil {
		return nil, err
	}
	end, next := paginate(len(f.Reservations), start, f.PageSize)
	return &DescribeReservedDBInstancesOutput{
		ReservedDBInstances: f.Reservations[min(start, len(f.Reservations)):end], Marker: next}, nil
}

func offsetOf(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscanf(token, "offset:%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("rds: fixture got an invalid pagination token %q", token)
	}
	return n, nil
}

func paginate(total, start, size int) (end int, next string) {
	if start > total {
		start = total
	}
	if size <= 0 || start+size >= total {
		return total, ""
	}
	return start + size, fmt.Sprintf("offset:%d", start+size)
}

// --- Fixture builders ------------------------------------------------------

// SyntheticMetric generates n datapoints of constant value across a window,
// starting at start and stepping by period.
func SyntheticMetric(start time.Time, period time.Duration, n int, value float64) []Point {
	if n <= 0 || period <= 0 {
		return nil
	}
	out := make([]Point, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Point{At: start.Add(time.Duration(i) * period), Value: value})
	}
	return out
}

// SyntheticSeries builds a delivered [Series] directly, for tests that
// exercise the sizer without going through the collector.
func SyntheticSeries(metric string, start time.Time, period time.Duration, values ...float64) Series {
	s := Series{Metric: metric, Source: SourceCloudWatch, Status: StatusComplete,
		PeriodSeconds: int32(period / time.Second)}
	for i, v := range values {
		s.Points = append(s.Points, Point{At: start.Add(time.Duration(i) * period), Value: v})
	}
	return s
}

// SortPoints orders datapoints by (timestamp, value), so a test that shuffles
// its fixture can still express "same input, different order".
func SortPoints(pts []Point) {
	sort.SliceStable(pts, func(i, j int) bool {
		if !pts[i].At.Equal(pts[j].At) {
			return pts[i].At.Before(pts[j].At)
		}
		return pts[i].Value < pts[j].Value
	})
}
