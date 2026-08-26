package lambda

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Fixture replays recorded API responses through all three seams, with real
// pagination, real truncation and real empty-account behavior. It exists so
// the collector can be exercised end to end without an AWS SDK, a network, or a
// credential — and so a later unit's cmd/ wiring has something to test against.
//
// It is exported for the same reason pkg/ec2's is: the seams are the contract,
// and a contract nobody outside the package can exercise is not a contract.
type Fixture struct {
	// Functions is the recorded ListFunctions inventory.
	Functions []FunctionRecord
	// Events maps a log group name to its recorded events.
	Events map[string][]LogEvent
	// Metrics maps "<functionName>/<metricName>" to recorded datapoints.
	Metrics map[string][]Point
	// PageSize splits every paginated response; 0 means one page.
	PageSize int

	// ListErr fails ListFunctions.
	ListErr error
	// LogsErr fails FilterLogEvents for a specific log group.
	LogsErr map[string]error
	// MetricsErr fails GetMetricData.
	MetricsErr error
	// DropResults omits the first N results from every GetMetricData page,
	// reproducing a truncated response — the case where a missing result must
	// be read as "we were not told", not as "the metric is empty".
	DropResults int

	// Calls counts seam invocations, so a test can assert the collector's
	// bounds actually bound something.
	Calls struct {
		ListFunctions   int
		FilterLogEvents int
		GetMetricData   int
	}
}

// ListFunctions implements [InventoryAPI].
func (f *Fixture) ListFunctions(ctx context.Context, in *ListFunctionsInput) (*ListFunctionsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.ListFunctions++
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	start, err := offsetOf(in.Marker)
	if err != nil {
		return nil, err
	}
	page, next := paginate(len(f.Functions), start, f.PageSize)
	out := &ListFunctionsOutput{Functions: f.Functions[start:page], NextMarker: next}
	return out, nil
}

// FilterLogEvents implements [LogsAPI].
func (f *Fixture) FilterLogEvents(ctx context.Context, in *FilterLogEventsInput) (*FilterLogEventsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.FilterLogEvents++
	if err := f.LogsErr[in.LogGroupName]; err != nil {
		return nil, err
	}
	all := f.Events[in.LogGroupName]
	// Honor the requested window the way the API does: events outside it are
	// not returned, so a fixture cannot smuggle evidence past a window gate.
	var win []LogEvent
	for _, e := range all {
		if !in.Start.IsZero() && e.Timestamp.Before(in.Start) {
			continue
		}
		if !in.End.IsZero() && e.Timestamp.After(in.End) {
			continue
		}
		win = append(win, e)
	}
	start, err := offsetOf(in.NextToken)
	if err != nil {
		return nil, err
	}
	if start > len(win) {
		start = len(win)
	}
	page, next := paginate(len(win), start, f.PageSize)
	return &FilterLogEventsOutput{Events: win[start:page], NextToken: next}, nil
}

// GetMetricData implements [MetricsAPI].
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
			continue
		}
		key := q.Dimensions["FunctionName"] + "/" + q.MetricName
		pts := f.Metrics[key]
		r := MetricDataResult{ID: q.ID, StatusCode: StatusComplete}
		for _, p := range pts {
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

func offsetOf(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	var n int
	if _, err := fmt.Sscanf(token, "offset:%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("lambda: fixture got an invalid pagination token %q", token)
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

// ReportLine renders a REPORT log line in the tab-separated form the AWS Lambda
// platform emits. Cold invocations carry an Init Duration field; warm ones do
// not, which is exactly how the platform distinguishes them.
func ReportLine(requestID string, durationMS, billedMS float64, memoryMB, maxUsedMB int64, initMS float64) string {
	line := fmt.Sprintf("REPORT RequestId: %s\tDuration: %.2f ms\tBilled Duration: %.0f ms\t"+
		"Memory Size: %d MB\tMax Memory Used: %d MB\t", requestID, durationMS, billedMS, memoryMB, maxUsedMB)
	if initMS > 0 {
		line += fmt.Sprintf("Init Duration: %.2f ms\t", initMS)
	}
	return line
}

// SyntheticReports generates n REPORT events at a memory setting, spread evenly
// across [start, start+span]. Every coldEvery-th invocation is a cold start;
// coldEvery <= 0 means all warm.
func SyntheticReports(prefix string, start time.Time, span time.Duration, n int,
	memoryMB, maxUsedMB int64, billedMS, initMS float64, coldEvery int) []LogEvent {

	if n <= 0 {
		return nil
	}
	step := span / time.Duration(n)
	out := make([]LogEvent, 0, n)
	for i := 0; i < n; i++ {
		init := 0.0
		if coldEvery > 0 && i%coldEvery == 0 {
			init = initMS
		}
		out = append(out, LogEvent{
			Timestamp: start.Add(time.Duration(i) * step),
			Message: ReportLine(fmt.Sprintf("%s-%04d", prefix, i), billedMS-0.4, billedMS,
				memoryMB, maxUsedMB, init),
		})
	}
	return out
}

// SyntheticMetric generates n datapoints of constant value across a window.
func SyntheticMetric(start time.Time, period time.Duration, n int, value float64) []Point {
	out := make([]Point, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Point{At: start.Add(time.Duration(i) * period), Value: value})
	}
	return out
}

// SortEvents orders log events by (timestamp, message), so a test that shuffles
// its fixture can still express "same input, different order".
func SortEvents(evs []LogEvent) {
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].Timestamp.Equal(evs[j].Timestamp) {
			return evs[i].Timestamp.Before(evs[j].Timestamp)
		}
		return evs[i].Message < evs[j].Message
	})
}
