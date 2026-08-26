package ec2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

// Recorded fixtures.
//
// pkg/provider proves its EKS provider against fakes that implement its
// asgAPI seam; this is the same idea one step further along: the fixture is
// *data*, recorded from an account into JSON, replayed through the two seams
// with the same pagination, truncation and empty-account behavior the real
// APIs have. Tests that use it exercise the production collector path
// verbatim, and a fixture can be captured from a real account without this
// package ever learning how to call AWS.
//
// The fixture is not safe for concurrent use; a Collector is single-goroutine
// by construction.

// RecordedSeries is one metric's recorded datapoints for one instance.
type RecordedSeries struct {
	InstanceID string `json:"instanceId"`
	Metric     string `json:"metric"`
	// Status is the recorded GetMetricData status code. Empty means Complete.
	Status string  `json:"status,omitempty"`
	Points []Point `json:"points,omitempty"`
}

// Fixture replays recorded responses through [InventoryAPI] and [MetricsAPI].
type Fixture struct {
	// InventoryPages are literal recorded DescribeInstances pages, in order.
	// An empty slice is an empty account: one page, no reservations.
	InventoryPages []DescribeInstancesOutput `json:"inventoryPages,omitempty"`
	// Metrics is the recorded metric table. A query with no matching row gets
	// an empty-but-Complete result, which is what CloudWatch returns for a
	// metric that has no data — the ordinary memory-blind case.
	Metrics []RecordedSeries `json:"metrics,omitempty"`
	// MetricPageSize splits results across GetMetricData pages. Zero means one
	// page per call.
	MetricPageSize int `json:"metricPageSize,omitempty"`
	// TruncateResultsAt drops every result at or beyond this index within a
	// call, and issues no continuation token — the shape of a response that
	// silently answers fewer queries than it was asked. Zero disables it.
	TruncateResultsAt int `json:"truncateResultsAt,omitempty"`
	// Messages are returned on the first metric page of every call.
	Messages []string `json:"messages,omitempty"`
	// InventoryFailAt and MetricsFailAt fail the Nth call (1-based) with a
	// transport error. Zero disables.
	InventoryFailAt int `json:"inventoryFailAt,omitempty"`
	MetricsFailAt   int `json:"metricsFailAt,omitempty"`
	// RepeatInventoryToken makes the pager hand back a token that never
	// advances — the broken-pager case a page budget alone would not catch.
	RepeatInventoryToken bool `json:"repeatInventoryToken,omitempty"`

	// InventoryRequests and MetricRequests record what the collector asked
	// for, so tests can assert the request contract (batch size, requested
	// period) and not merely the response handling.
	InventoryRequests []DescribeInstancesInput `json:"-"`
	MetricRequests    []GetMetricDataInput     `json:"-"`
}

// LoadFixture parses a recorded fixture, rejecting unknown fields so a typo in
// a hand-edited recording fails loudly instead of silently disabling a case.
func LoadFixture(r io.Reader) (*Fixture, error) {
	var f Fixture
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("ec2: parse fixture: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// LoadFixtureFile reads a recorded fixture from disk.
func LoadFixtureFile(path string) (*Fixture, error) {
	b, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	f, err := LoadFixture(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func (f *Fixture) validate() error {
	seen := map[string]bool{}
	for _, m := range f.Metrics {
		if m.InstanceID == "" || m.Metric == "" {
			return fmt.Errorf("ec2: fixture metric row needs instanceId and metric")
		}
		key := m.InstanceID + "\x00" + m.Metric
		if seen[key] {
			return fmt.Errorf("ec2: fixture has duplicate metric rows for %s/%s", m.InstanceID, m.Metric)
		}
		seen[key] = true
		for i := 1; i < len(m.Points); i++ {
			if !m.Points[i-1].At.Before(m.Points[i].At) {
				return fmt.Errorf("ec2: fixture %s/%s points are not strictly ascending in time",
					m.InstanceID, m.Metric)
			}
		}
	}
	if f.MetricPageSize < 0 {
		return fmt.Errorf("ec2: fixture MetricPageSize must not be negative")
	}
	if f.TruncateResultsAt < 0 {
		return fmt.Errorf("ec2: fixture TruncateResultsAt must not be negative")
	}
	return nil
}

// WriteFixture serializes a fixture, so a recording captured elsewhere can be
// committed as testdata.
func WriteFixture(w io.Writer, f *Fixture) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

// DescribeInstances replays the recorded inventory pages.
func (f *Fixture) DescribeInstances(ctx context.Context, in *DescribeInstancesInput) (*DescribeInstancesOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("ec2 fixture: nil DescribeInstances input")
	}
	f.InventoryRequests = append(f.InventoryRequests, *in)
	if f.InventoryFailAt > 0 && len(f.InventoryRequests) == f.InventoryFailAt {
		return nil, fmt.Errorf("ec2 fixture: injected DescribeInstances failure on call %d", f.InventoryFailAt)
	}
	if len(f.InventoryPages) == 0 {
		return &DescribeInstancesOutput{}, nil // an empty account
	}
	idx, err := pageIndex(in.NextToken)
	if err != nil {
		return nil, err
	}
	if idx >= len(f.InventoryPages) {
		return nil, fmt.Errorf("ec2 fixture: DescribeInstances token %q is past the recorded pages", in.NextToken)
	}
	page := f.InventoryPages[idx]
	out := &DescribeInstancesOutput{Reservations: page.Reservations}
	switch {
	case f.RepeatInventoryToken:
		out.NextToken = in.NextToken // a pager that never advances
		if out.NextToken == "" {
			out.NextToken = "0"
		}
	case page.NextToken != "":
		out.NextToken = page.NextToken // an explicitly recorded token
	case idx+1 < len(f.InventoryPages):
		out.NextToken = strconv.Itoa(idx + 1)
	}
	return out, nil
}

// GetMetricData answers the requested queries from the recorded table, in
// query order, with the recorded pagination and truncation behavior.
func (f *Fixture) GetMetricData(ctx context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("ec2 fixture: nil GetMetricData input")
	}
	f.MetricRequests = append(f.MetricRequests, *in)
	if f.MetricsFailAt > 0 && len(f.MetricRequests) == f.MetricsFailAt {
		return nil, fmt.Errorf("ec2 fixture: injected GetMetricData failure on call %d", f.MetricsFailAt)
	}
	if len(in.Queries) > MaxSeriesPerCall {
		// The real API rejects this; a fixture that silently accepted it would
		// hide a batching bug.
		return nil, fmt.Errorf("ec2 fixture: %d queries exceeds the GetMetricData limit of %d",
			len(in.Queries), MaxSeriesPerCall)
	}
	results := make([]MetricDataResult, 0, len(in.Queries))
	for _, q := range in.Queries {
		if err := validQueryID(q.ID); err != nil {
			return nil, err
		}
		r := MetricDataResult{ID: q.ID, Label: q.Label, StatusCode: StatusComplete}
		if rec, ok := f.lookup(q.Dimensions["InstanceId"], q.MetricName); ok {
			if rec.Status != "" {
				r.StatusCode = rec.Status
			}
			for _, p := range rec.Points {
				if p.At.Before(in.StartTime) || p.At.After(in.EndTime) {
					continue // CloudWatch honors the requested window
				}
				r.Timestamps = append(r.Timestamps, p.At)
				r.Values = append(r.Values, p.Value)
			}
		}
		results = append(results, r)
	}
	if f.TruncateResultsAt > 0 && f.TruncateResultsAt < len(results) {
		return &GetMetricDataOutput{Results: results[:f.TruncateResultsAt], Messages: f.Messages}, nil
	}

	size := f.MetricPageSize
	if size <= 0 || size >= len(results) {
		return &GetMetricDataOutput{Results: results, Messages: f.Messages}, nil
	}
	idx, err := pageIndex(in.NextToken)
	if err != nil {
		return nil, err
	}
	start := idx * size
	if start > len(results) {
		return nil, fmt.Errorf("ec2 fixture: GetMetricData token %q is past the recorded results", in.NextToken)
	}
	end := start + size
	if end > len(results) {
		end = len(results)
	}
	out := &GetMetricDataOutput{Results: results[start:end]}
	if idx == 0 {
		out.Messages = f.Messages
	}
	if end < len(results) {
		out.NextToken = strconv.Itoa(idx + 1)
	}
	return out, nil
}

func (f *Fixture) lookup(instanceID, metric string) (RecordedSeries, bool) {
	for _, m := range f.Metrics {
		if m.InstanceID == instanceID && m.Metric == metric {
			return m, true
		}
	}
	return RecordedSeries{}, false
}

func pageIndex(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	i, err := strconv.Atoi(token)
	if err != nil || i < 0 {
		return 0, fmt.Errorf("ec2 fixture: malformed pagination token %q", token)
	}
	return i, nil
}

// validQueryID enforces CloudWatch's ID rule, so a collector that generated an
// ID the API would reject fails in tests rather than in production.
func validQueryID(id string) error {
	if id == "" {
		return fmt.Errorf("ec2 fixture: query has no ID")
	}
	if id[0] < 'a' || id[0] > 'z' {
		return fmt.Errorf("ec2 fixture: query ID %q must start with a lowercase letter", id)
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		if !ok {
			return fmt.Errorf("ec2 fixture: query ID %q contains %q, which CloudWatch rejects", id, string(c))
		}
	}
	return nil
}

// SyntheticSeries builds an evenly spaced series ending at end, which is how
// tests express "fourteen days of 5-minute datapoints" without committing
// four thousand JSON objects. values is cycled to fill count points.
//
// It lives beside the fixture rather than in a _test.go file because a later
// unit's simulator needs the same generator to exercise this domain, exactly
// as the existing e2e drives decisions through the production path.
func SyntheticSeries(instanceID, metric string, end time.Time, period time.Duration, count int, values []float64) RecordedSeries {
	if count < 0 {
		count = 0
	}
	rs := RecordedSeries{InstanceID: instanceID, Metric: metric, Points: make([]Point, 0, count)}
	for i := 0; i < count; i++ {
		v := 0.0
		if len(values) > 0 {
			v = values[i%len(values)]
		}
		at := end.Add(-time.Duration(count-1-i) * period)
		rs.Points = append(rs.Points, Point{At: at, Value: v})
	}
	return rs
}

// Downsample averages a series into longer periods, the way CloudWatch's basic
// monitoring publishes one 5-minute value where detailed monitoring publishes
// five 1-minute values. It exists so a test can hold the underlying truth
// fixed and vary only what the account is allowed to see.
func Downsample(rs RecordedSeries, factor int) RecordedSeries {
	if factor <= 1 || len(rs.Points) == 0 {
		return rs
	}
	out := RecordedSeries{InstanceID: rs.InstanceID, Metric: rs.Metric, Status: rs.Status}
	for start := 0; start < len(rs.Points); start += factor {
		end := start + factor
		if end > len(rs.Points) {
			end = len(rs.Points)
		}
		var sum float64
		for _, p := range rs.Points[start:end] {
			sum += p.Value
		}
		out.Points = append(out.Points, Point{
			At:    rs.Points[end-1].At,
			Value: sum / float64(end-start),
		})
	}
	return out
}

// SortMetrics orders the recorded table canonically, so a fixture written back
// out is stable regardless of how it was assembled.
func SortMetrics(rows []RecordedSeries) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].InstanceID != rows[j].InstanceID {
			return rows[i].InstanceID < rows[j].InstanceID
		}
		return rows[i].Metric < rows[j].Metric
	})
}
