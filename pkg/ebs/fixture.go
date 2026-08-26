package ebs

// Recorded fixtures.
//
// pkg/provider proves its EKS provider against fakes that implement its
// asgAPI seam; this is the same idea one step further along: the fixture is
// *data*, recorded from an account into JSON, replayed through the seams with
// the same pagination, truncation and asynchrony the real APIs have. Tests
// that use it exercise the production collector and actuator paths verbatim,
// and a fixture can be captured from a real account without this package ever
// learning how to call AWS.
//
// It is also a mocked EC2 endpoint for the write seam: ModifyVolume mutates
// the recorded inventory through a modifying → optimizing → completed state
// machine driven by polls, which is what makes an end-to-end test of an
// asynchronous change possible without a cloud.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// RecordedSeries is one metric's recorded datapoints for one volume, in the
// units CloudWatch returns them: Sum of operations or bytes over the period,
// not a rate.
type RecordedSeries struct {
	VolumeID string `json:"volumeId"`
	Metric   string `json:"metric"`
	// Status is the recorded GetMetricData status code. Empty means Complete.
	Status string  `json:"status,omitempty"`
	Points []Point `json:"points,omitempty"`
}

// Fixture replays recorded responses through [InventoryAPI], [MetricsAPI] and
// [ModifyAPI]. It is safe for concurrent use; the actuator polls from one
// goroutine, but tests do not have to.
type Fixture struct {
	// InventoryPages are literal recorded DescribeVolumes pages, in order. An
	// empty slice is an empty account: one page, no volumes.
	InventoryPages []DescribeVolumesOutput `json:"inventoryPages,omitempty"`
	// ModificationPages are recorded DescribeVolumesModifications pages.
	ModificationPages []DescribeVolumesModificationsOutput `json:"modificationPages,omitempty"`
	// Metrics is the recorded metric table. A query with no matching row gets
	// an empty-but-Complete result, which is what CloudWatch returns for a
	// metric that has no data.
	Metrics []RecordedSeries `json:"metrics,omitempty"`

	// MetricPageSize splits results across GetMetricData pages. Zero means one
	// page per call.
	MetricPageSize int `json:"metricPageSize,omitempty"`
	// TruncateResultsAt drops every result at or beyond this index within a
	// call and issues no continuation token — a response that silently answers
	// fewer queries than it was asked. Zero disables it.
	TruncateResultsAt int `json:"truncateResultsAt,omitempty"`
	// RepeatInventoryToken makes the pager hand back a token that never
	// advances — the broken-pager case a page budget alone would not catch.
	RepeatInventoryToken bool `json:"repeatInventoryToken,omitempty"`

	// FailAt fields fail the Nth call (1-based) on that seam with a transport
	// error. Zero disables.
	InventoryFailAt    int `json:"inventoryFailAt,omitempty"`
	ModificationFailAt int `json:"modificationFailAt,omitempty"`
	MetricsFailAt      int `json:"metricsFailAt,omitempty"`
	ModifyFailAt       int `json:"modifyFailAt,omitempty"`

	// PollsToOptimizing is how many DescribeVolumesModifications calls a
	// modification this fixture started spends in "modifying" before it turns
	// "optimizing" — the point at which AWS has applied the new type and the
	// volume delivers the new performance. Zero means 1.
	PollsToOptimizing int `json:"pollsToOptimizing,omitempty"`
	// PollsToCompleted is how many further calls it spends optimizing. Zero
	// means 1.
	PollsToCompleted int `json:"pollsToCompleted,omitempty"`
	// FailModification makes the started modification report "failed" instead
	// of progressing.
	FailModification bool `json:"failModification,omitempty"`

	// Now stamps modification records. ModifyVolume refuses without it: a
	// modification with no start time would make every cooldown untestable.
	Now func() time.Time `json:"-"`

	// Requests record what was asked for, so tests can assert the request
	// contract and not merely the response handling.
	InventoryRequests    []DescribeVolumesInput              `json:"-"`
	ModificationRequests []DescribeVolumesModificationsInput `json:"-"`
	MetricRequests       []GetMetricDataInput                `json:"-"`
	ModifyRequests       []ModifyVolumeInput                 `json:"-"`

	mu          sync.Mutex
	invCalls    int
	modsCalls   int
	metCalls    int
	modifyCalls int
	// started tracks modifications this fixture created, and how many
	// modification reads have happened since.
	polls map[string]int
	live  map[string]*VolumeModification
}

// LoadFixture parses a recorded fixture, rejecting unknown fields so a typo in
// a hand-edited recording fails loudly instead of silently disabling a case.
func LoadFixture(r io.Reader) (*Fixture, error) {
	var f Fixture
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("ebs: parse fixture: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// LoadFixtureFile reads a recorded fixture from disk.
func LoadFixtureFile(path string) (*Fixture, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	f, err := LoadFixture(fh)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func (f *Fixture) validate() error {
	seen := map[string]bool{}
	for _, m := range f.Metrics {
		if m.VolumeID == "" || m.Metric == "" {
			return fmt.Errorf("ebs: fixture metric row needs volumeId and metric")
		}
		key := m.VolumeID + "\x00" + m.Metric
		if seen[key] {
			return fmt.Errorf("ebs: fixture has duplicate metric rows for %s/%s", m.VolumeID, m.Metric)
		}
		seen[key] = true
		for i := 1; i < len(m.Points); i++ {
			if !m.Points[i-1].At.Before(m.Points[i].At) {
				return fmt.Errorf("ebs: fixture %s/%s points are not strictly ascending in time",
					m.VolumeID, m.Metric)
			}
		}
	}
	if f.MetricPageSize < 0 || f.TruncateResultsAt < 0 {
		return fmt.Errorf("ebs: fixture page sizes must not be negative")
	}
	vols := map[string]bool{}
	for _, p := range f.InventoryPages {
		for _, v := range p.Volumes {
			if v.VolumeID == "" {
				return fmt.Errorf("ebs: fixture volume record has no volumeId")
			}
			if vols[v.VolumeID] {
				return fmt.Errorf("ebs: fixture lists volume %s twice", v.VolumeID)
			}
			vols[v.VolumeID] = true
		}
	}
	return nil
}

func (f *Fixture) pollsToOptimizing() int {
	if f.PollsToOptimizing > 0 {
		return f.PollsToOptimizing
	}
	return 1
}

func (f *Fixture) pollsToCompleted() int {
	if f.PollsToCompleted > 0 {
		return f.PollsToCompleted
	}
	return 1
}

// DescribeVolumes implements [InventoryAPI].
func (f *Fixture) DescribeVolumes(_ context.Context, in *DescribeVolumesInput) (*DescribeVolumesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invCalls++
	if in != nil {
		f.InventoryRequests = append(f.InventoryRequests, *in)
	}
	if f.InventoryFailAt == f.invCalls {
		return nil, fmt.Errorf("simulated DescribeVolumes failure on call %d", f.invCalls)
	}
	idx := 0
	if in != nil && in.NextToken != "" {
		n, err := parseToken(in.NextToken)
		if err != nil {
			return nil, err
		}
		idx = n
	}
	if len(f.InventoryPages) == 0 {
		return &DescribeVolumesOutput{}, nil
	}
	if idx >= len(f.InventoryPages) {
		return &DescribeVolumesOutput{}, nil
	}
	page := f.InventoryPages[idx]
	out := DescribeVolumesOutput{Volumes: append([]VolumeRecord(nil), page.Volumes...)}
	switch {
	case f.RepeatInventoryToken:
		out.NextToken = "page-0"
	case idx+1 < len(f.InventoryPages):
		out.NextToken = fmt.Sprintf("page-%d", idx+1)
	}
	return &out, nil
}

func parseToken(t string) (int, error) {
	if !strings.HasPrefix(t, "page-") {
		return 0, fmt.Errorf("ebs: fixture got an unrecognized page token %q", t)
	}
	var n int
	if _, err := fmt.Sscanf(t, "page-%d", &n); err != nil {
		return 0, fmt.Errorf("ebs: fixture got a malformed page token %q", t)
	}
	return n, nil
}

// DescribeVolumesModifications implements [InventoryAPI]. It merges the
// recorded pages with modifications this fixture started, advancing the state
// machine one step per call.
func (f *Fixture) DescribeVolumesModifications(_ context.Context,
	in *DescribeVolumesModificationsInput) (*DescribeVolumesModificationsOutput, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.modsCalls++
	if in != nil {
		f.ModificationRequests = append(f.ModificationRequests, *in)
	}
	if f.ModificationFailAt == f.modsCalls {
		return nil, fmt.Errorf("simulated DescribeVolumesModifications failure on call %d", f.modsCalls)
	}

	wanted := map[string]bool{}
	if in != nil {
		for _, id := range in.VolumeIDs {
			wanted[id] = true
		}
	}
	keep := func(id string) bool { return len(wanted) == 0 || wanted[id] }

	idx := 0
	if in != nil && in.NextToken != "" {
		n, err := parseToken(in.NextToken)
		if err != nil {
			return nil, err
		}
		idx = n
	}

	var out DescribeVolumesModificationsOutput
	if idx < len(f.ModificationPages) {
		for _, m := range f.ModificationPages[idx].Modifications {
			if keep(m.VolumeID) && f.live[m.VolumeID] == nil {
				out.Modifications = append(out.Modifications, m)
			}
		}
		if idx+1 < len(f.ModificationPages) {
			out.NextToken = fmt.Sprintf("page-%d", idx+1)
		}
	}
	// Live modifications ride on the first page only; they are this fixture's
	// own state, not recorded data.
	if idx == 0 {
		ids := make([]string, 0, len(f.live))
		for id := range f.live {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if !keep(id) {
				continue
			}
			out.Modifications = append(out.Modifications, *f.advance(id))
		}
	}
	return &out, nil
}

// advance moves one live modification along by a poll. Caller holds f.mu.
func (f *Fixture) advance(id string) *VolumeModification {
	m := f.live[id]
	if m.ModificationState == ModificationCompleted || m.ModificationState == ModificationFailed {
		return m
	}
	f.polls[id]++
	n := f.polls[id]
	if f.FailModification {
		m.ModificationState = ModificationFailed
		m.StatusMessage = "simulated modification failure"
		m.EndTime = f.stamp()
		return m
	}
	switch {
	case n >= f.pollsToOptimizing()+f.pollsToCompleted():
		m.ModificationState = ModificationCompleted
		m.Progress = 100
		if m.EndTime.IsZero() {
			m.EndTime = f.stamp()
		}
		f.applyTarget(id, m)
	case n >= f.pollsToOptimizing():
		m.ModificationState = ModificationOptimizing
		m.Progress = 50
		// AWS applies the new type and performance when optimizing starts; the
		// background block move is what "optimizing" is.
		f.applyTarget(id, m)
	default:
		m.ModificationState = ModificationModifying
	}
	return m
}

// applyTarget writes the modification's target into the inventory record.
// Caller holds f.mu.
func (f *Fixture) applyTarget(id string, m *VolumeModification) {
	for pi := range f.InventoryPages {
		for vi := range f.InventoryPages[pi].Volumes {
			v := &f.InventoryPages[pi].Volumes[vi]
			if v.VolumeID != id {
				continue
			}
			if m.TargetVolumeType != "" {
				v.VolumeType = m.TargetVolumeType
			}
			if m.TargetSizeGiB > 0 {
				v.SizeGiB = m.TargetSizeGiB
			}
			if strings.EqualFold(m.TargetVolumeType, VolumeTypeGP3) {
				v.IOPS, v.ThroughputMBps = m.TargetIOPS, m.TargetThroughputMBps
			} else {
				// gp2 derives its IOPS from size and provisions no throughput.
				v.IOPS = GP2PerformanceFor(v.SizeGiB).BaselineIOPS
				v.ThroughputMBps = 0
			}
			return
		}
	}
}

func (f *Fixture) stamp() time.Time {
	if f.Now == nil {
		return time.Time{}
	}
	return f.Now().UTC()
}

// ModifyVolume implements [ModifyAPI]. It behaves like the API it stands in
// for: it refuses an unknown volume and a volume that is already being
// modified, and it starts an asynchronous modification rather than completing
// one.
func (f *Fixture) ModifyVolume(_ context.Context, in *ModifyVolumeInput) (*ModifyVolumeOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("ebs: fixture ModifyVolume(nil)")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modifyCalls++
	f.ModifyRequests = append(f.ModifyRequests, *in)
	if f.ModifyFailAt == f.modifyCalls {
		return nil, fmt.Errorf("simulated ModifyVolume failure on call %d", f.modifyCalls)
	}
	if f.Now == nil {
		return nil, fmt.Errorf("ebs: fixture needs a clock (Fixture.Now) before it can modify a volume")
	}
	var cur *VolumeRecord
	for pi := range f.InventoryPages {
		for vi := range f.InventoryPages[pi].Volumes {
			if f.InventoryPages[pi].Volumes[vi].VolumeID == in.VolumeID {
				cur = &f.InventoryPages[pi].Volumes[vi]
			}
		}
	}
	if cur == nil {
		return nil, fmt.Errorf("InvalidVolume.NotFound: %s", in.VolumeID)
	}
	if m := f.live[in.VolumeID]; m != nil && m.InFlight() {
		return nil, fmt.Errorf("IncorrectModificationState: %s is already %s", in.VolumeID, m.ModificationState)
	}
	if in.SizeGiB > 0 && in.SizeGiB < cur.SizeGiB {
		return nil, fmt.Errorf("InvalidParameterValue: %s cannot shrink from %d to %d GiB",
			in.VolumeID, cur.SizeGiB, in.SizeGiB)
	}
	mod := &VolumeModification{
		VolumeID:               in.VolumeID,
		ModificationState:      ModificationModifying,
		TargetVolumeType:       orDefault(in.VolumeType, cur.VolumeType),
		TargetSizeGiB:          orDefaultInt(in.SizeGiB, cur.SizeGiB),
		TargetIOPS:             in.IOPS,
		TargetThroughputMBps:   in.ThroughputMBps,
		OriginalVolumeType:     cur.VolumeType,
		OriginalSizeGiB:        cur.SizeGiB,
		OriginalIOPS:           cur.IOPS,
		OriginalThroughputMBps: cur.ThroughputMBps,
		StartTime:              f.stamp(),
	}
	if f.live == nil {
		f.live = map[string]*VolumeModification{}
	}
	if f.polls == nil {
		f.polls = map[string]int{}
	}
	f.live[in.VolumeID] = mod
	f.polls[in.VolumeID] = 0
	return &ModifyVolumeOutput{Modification: *mod}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

// GetMetricData implements [MetricsAPI].
func (f *Fixture) GetMetricData(_ context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metCalls++
	if in != nil {
		f.MetricRequests = append(f.MetricRequests, *in)
	}
	if f.MetricsFailAt == f.metCalls {
		return nil, fmt.Errorf("simulated GetMetricData failure on call %d", f.metCalls)
	}
	if in == nil {
		return &GetMetricDataOutput{}, nil
	}

	results := make([]MetricDataResult, 0, len(in.Queries))
	for i, q := range in.Queries {
		if f.TruncateResultsAt > 0 && i >= f.TruncateResultsAt {
			break
		}
		res := MetricDataResult{ID: q.ID, Label: q.Label, StatusCode: StatusComplete}
		if row := f.seriesFor(q.Dimensions["VolumeId"], q.MetricName); row != nil {
			if row.Status != "" {
				res.StatusCode = row.Status
			}
			for _, p := range row.Points {
				if p.At.Before(in.StartTime) || p.At.After(in.EndTime) {
					continue
				}
				res.Timestamps = append(res.Timestamps, p.At)
				res.Values = append(res.Values, p.Value)
			}
		}
		results = append(results, res)
	}

	start := 0
	if in.NextToken != "" {
		n, err := parseToken(in.NextToken)
		if err != nil {
			return nil, err
		}
		start = n
	}
	if f.MetricPageSize <= 0 || start >= len(results) {
		if start > 0 {
			return &GetMetricDataOutput{}, nil
		}
		return &GetMetricDataOutput{Results: results}, nil
	}
	end := start + f.MetricPageSize
	if end > len(results) {
		end = len(results)
	}
	out := GetMetricDataOutput{Results: results[start:end]}
	if end < len(results) {
		out.NextToken = fmt.Sprintf("page-%d", end)
	}
	return &out, nil
}

// seriesFor finds a recorded row. Caller holds f.mu.
func (f *Fixture) seriesFor(volumeID, metric string) *RecordedSeries {
	for i := range f.Metrics {
		if f.Metrics[i].VolumeID == volumeID && f.Metrics[i].Metric == metric {
			return &f.Metrics[i]
		}
	}
	return nil
}

// VolumeByID returns the fixture's current record for a volume, which is how a
// test asserts that an actuation actually landed.
func (f *Fixture) VolumeByID(id string) (VolumeRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for pi := range f.InventoryPages {
		for _, v := range f.InventoryPages[pi].Volumes {
			if v.VolumeID == id {
				return v, true
			}
		}
	}
	return VolumeRecord{}, false
}

// ModifyCallCount reports how many mutations were issued — the number that
// must stay at zero in dry-run and must not grow when a completed step is
// re-executed.
func (f *Fixture) ModifyCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modifyCalls
}

var (
	_ InventoryAPI = (*Fixture)(nil)
	_ MetricsAPI   = (*Fixture)(nil)
	_ ModifyAPI    = (*Fixture)(nil)
)
