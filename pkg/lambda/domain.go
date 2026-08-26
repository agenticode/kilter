package lambda

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Domain adapts this package to the pkg/domain seam — its READ-ONLY half.
//
// Kind, Learn, Recommend, Health, Checkpoint and Restore are implemented.
// PlanSteps is implemented as an unconditional refusal: there is no Lambda
// actuator in this unit, this domain is advisory by design (§6 U9), and a
// domain that could be talked into emitting a step would be one bug away from
// changing a production function's memory. [domain.Registry] already refuses
// steps for a report-only domain; this refuses again, so a caller holding the
// Domain directly hits the same wall.
//
// # Two ingest paths, and why
//
// [Domain.Observe] takes the Lambda-native [Snapshot] — the collector's output,
// carrying per-invocation REPORT records. [Domain.Learn] takes the generic
// [domain.Snapshot].
//
// The generic snapshot cannot carry REPORT records. Its [domain.Sample] is one
// (metric, float, timestamp) triple, and a REPORT record is four numbers whose
// CORRELATION is the whole point: the memory setting an invocation ran at, and
// the duration it took THERE. Splitting one record into three samples throws
// that correlation away, and without it every multi-memory-point comparison in
// this package becomes impossible — which is to say, every cost claim does.
//
// So Learn ingests what the generic seam genuinely carries — the function
// inventory, tags, provisioned concurrency, and CloudWatch-style aggregate
// samples — and a function learned that way honestly refuses with
// [ReasonNoReportEvidence] until the native path delivers its log evidence. The
// fix belongs in pkg/domain (an opaque per-domain payload on Snapshot), which
// this unit may not edit; FINDINGS.md records it.
type Domain struct {
	cfg   Config
	sizer *Sizer

	mu          sync.RWMutex
	scope       string
	region      string
	targets     map[string]Target
	window      Window
	lastAt      time.Time
	stale       bool
	staleReason string
}

// NewDomain builds the read-only Lambda domain.
func NewDomain(cfg Config) (*Domain, error) {
	s, err := NewSizer(cfg)
	if err != nil {
		return nil, err
	}
	return &Domain{cfg: s.Config(), sizer: s, scope: cfg.Scope, region: cfg.Region,
		targets: map[string]Target{}}, nil
}

// Kind implements domain.Domain.
func (d *Domain) Kind() domain.Kind { return Kind }

// Observe folds a Lambda-native snapshot into learned state. The newest
// snapshot for a function REPLACES the previous one rather than accumulating:
// the collector re-queries a window of logs each tick, so accumulating would
// double-count the overlap and inflate every invocation count derived from it.
// The window is the evidence.
func (d *Domain) Observe(snap *Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q is not %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if snap.Scope != "" {
		d.scope = snap.Scope
	}
	if snap.Region != "" {
		d.region = snap.Region
	}
	d.stale, d.staleReason = snap.Stale, ""
	if snap.Stale {
		d.staleReason = "collector delivered an incomplete snapshot"
	}
	d.window = snap.Window
	for _, t := range snap.Targets {
		d.targets[t.Ref.ID] = t
	}
	at := snap.Timestamp
	if at.IsZero() {
		at = snap.Window.End
	}
	if at.After(d.lastAt) {
		d.lastAt = at
	}
	return nil
}

// Learn implements domain.Domain over the generic seam. See the type doc for
// what the generic snapshot can and cannot carry.
//
// Failure policy matches the rest of the seam: a nil, empty or payload-less
// snapshot degrades the domain and returns nil — an unreachable collector is an
// operational condition, not a programming error. Only a snapshot addressed to
// another domain is an error, because that is a wiring bug.
func (d *Domain) Learn(snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q is not %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if snap.Scope != "" {
		d.scope = snap.Scope
	}
	d.stale, d.staleReason = snap.Stale, snap.StaleReason
	if len(snap.Targets) == 0 {
		d.stale = true
		if d.staleReason == "" {
			d.staleReason = "collector delivered no Lambda targets"
		}
		return nil
	}

	byRef := map[string][]domain.Sample{}
	for _, s := range snap.Samples {
		byRef[s.Ref.ID] = append(byRef[s.Ref.ID], s)
	}
	for _, gt := range snap.Targets {
		t := Target{Ref: gt.Ref, Function: functionFromSpec(gt)}
		t.Series = seriesFromSamples(byRef[gt.Ref.ID])
		// Preserve REPORT evidence a native Observe already delivered for this
		// function: the generic path adds configuration, it does not erase
		// measurements.
		if prev, ok := d.targets[gt.Ref.ID]; ok {
			t.Reports, t.Drops = prev.Reports, prev.Drops
			if len(t.Series) == 0 {
				t.Series = prev.Series
			}
		}
		d.targets[gt.Ref.ID] = t
	}
	if snap.Timestamp.After(d.lastAt) {
		d.lastAt = snap.Timestamp
	}
	if d.window.End.IsZero() && !snap.Timestamp.IsZero() {
		d.window = Window{Start: snap.Timestamp, End: snap.Timestamp}
	}
	return nil
}

// functionFromSpec reconstructs the function configuration the generic seam can
// carry. Every field is optional; an attribute that does not parse is left at
// its zero value, which the sizer then refuses on rather than guesses at.
func functionFromSpec(t domain.Target) Function {
	f := Function{ARN: t.Ref.ID, Name: t.Ref.Name, Tags: copyTags(t.Labels)}
	f.MemoryMB = atoi64(t.Spec.Attr(AttrMemoryMB))
	if f.MemoryMB == 0 && t.Spec.Resources.MemoryBytes > 0 {
		f.MemoryMB = t.Spec.Resources.MemoryBytes >> 20
	}
	f.Architecture = t.Spec.Attr(AttrArch)
	f.Runtime = t.Spec.Attr(AttrRuntime)
	f.PackageType = t.Spec.Attr(AttrPackageType)
	f.TimeoutSec = atoi64(t.Spec.Attr(AttrTimeoutSeconds))
	f.ProvisionedConcurrency = atoi64(t.Spec.Attr(AttrProvisionedConcurrency))
	return f
}

// seriesFromSamples groups generic samples into metric series, sorted by metric
// then timestamp so the result cannot depend on delivery order.
func seriesFromSamples(samples []domain.Sample) []Series {
	if len(samples) == 0 {
		return nil
	}
	byMetric := map[string][]Point{}
	period := map[string]int32{}
	for _, s := range samples {
		byMetric[s.Metric] = append(byMetric[s.Metric], Point{At: s.Timestamp, Value: s.Value})
		if s.WindowSeconds > period[s.Metric] {
			period[s.Metric] = s.WindowSeconds
		}
	}
	out := make([]Series, 0, len(byMetric))
	for m, pts := range byMetric {
		sort.Slice(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
		out = append(out, Series{Metric: m, PeriodSeconds: period[m], Points: pts, Source: "domain-sample"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

func copyTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func atoi64(s string) int64 {
	var n int64
	if s == "" {
		return 0
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
		if n > MaxMemoryMB*1024 { // any plausible attribute is far below this
			return 0
		}
	}
	return n
}

// snapshot renders the learned state as a Lambda snapshot. Caller holds d.mu.
func (d *Domain) snapshot(now time.Time) *Snapshot {
	snap := &Snapshot{
		Domain: Kind, Scope: d.scope, Region: d.region,
		Timestamp: d.lastAt, Window: d.window, Stale: d.stale,
	}
	if snap.Timestamp.IsZero() {
		snap.Timestamp = now
	}
	if d.staleReason != "" {
		snap.Warnings = []string{d.staleReason}
	}
	for _, t := range d.targets {
		snap.Targets = append(snap.Targets, t)
	}
	SortTargets(snap.Targets)
	return snap
}

// Report renders the full read-only verdict. It is the Lambda-native output —
// what `cloud-agent --domain lambda` prints and what the UI card mirrors — and
// it carries the refusals that have no [domain.Recommendation] shape, because a
// function with nothing to propose still has something to say.
func (d *Domain) Report(now time.Time, ledger domain.Netter) *Report {
	d.mu.RLock()
	snap := d.snapshot(now)
	d.mu.RUnlock()
	return d.sizer.Assess(now, snap, ledger)
}

// Recommend implements domain.Domain. Every recommendation is
// [domain.ActionAdvisory]; suppressed ones stay visible with their reason code,
// as §5.7 requires, because a silently dropped recommendation is
// indistinguishable from a bug.
//
// A refusal with no specific alternative on the table (nothing was cheaper,
// nothing was measured) produces no Recommendation — a Recommendation whose
// Proposed equals its Current is not a recommendation, and [domain.Recommendation.Validate]
// says so. Those refusals live in [Domain.Report], which is the complete record.
func (d *Domain) Recommend(now time.Time, ledger domain.Netter) []domain.Recommendation {
	rep := d.Report(now, ledger)
	out := make([]domain.Recommendation, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		r, ok := a.Recommendation()
		if !ok {
			continue
		}
		out = append(out, r)
	}
	domain.SortRecommendations(out)
	return out
}

// Recommendation projects an assessment into the domain-generic shape, and
// reports false when there is nothing a Recommendation can express.
func (a Assessment) Recommendation() (domain.Recommendation, bool) {
	proposedMB := a.CandidateMemoryMB
	if a.Proposal != nil {
		proposedMB = a.Proposal.MemoryMB
	}
	if proposedMB == 0 || proposedMB == a.Function.MemoryMB {
		return domain.Recommendation{}, false
	}
	r := domain.Recommendation{
		Target:           a.Target,
		Current:          a.Current,
		Proposed:         SpecFor(a.Function, proposedMB, a.Function.Arch()),
		CurrentHourlyUSD: a.CurrentHourlyUSD,
		Action:           domain.ActionAdvisory,
		Risk:             RiskMedium,
		Confidence:       a.Confidence.Score,
		Evidence:         a.Evidence,
	}
	if p := a.Proposal; p != nil {
		r.ProposedHourlyUSD = p.ProposedHourlyUSD
		r.Risk = p.Risk
		r.Confidence = p.Confidence
		r.Reason = p.Reason
		r.SetSavings(p.GrossSavingsMonthlyUSD, p.NetSavingsMonthlyUSD)
		return r, true
	}
	// Suppressed. Gross and net stay at zero: this package never attaches a
	// number to a change it refuses to claim, because the number is exactly
	// what it is refusing.
	s := a.Suppressions[0]
	r.Suppressed = true
	r.SuppressCode = s.Code
	r.Reason = s.Reason
	r.ValidFrom = s.ValidFrom
	return r, true
}

// PlanSteps implements domain.Domain by refusing, always.
//
// This is not a stub and not a "not yet implemented": U9 ships advisory-only,
// and there is no code path in this package that can produce an executable
// step. The error is [domain.ErrReportOnly] so callers already written against
// the seam handle it without a special case.
func (d *Domain) PlanSteps([]domain.Recommendation, domain.Guard) ([]domain.Step, error) {
	return nil, fmt.Errorf("%w: the Lambda domain is advisory only — memory tuning changes a function's "+
		"latency as well as its bill, and no measurement this domain can take licenses an automated "+
		"lambda:UpdateFunctionConfiguration", domain.ErrReportOnly)
}

// Health reports readiness as of now. ReportOnly is true unconditionally and by
// construction; see [Domain.PlanSteps].
func (d *Domain) Health(now time.Time) domain.Health {
	d.mu.RLock()
	defer d.mu.RUnlock()
	h := domain.Health{
		Kind:         Kind,
		ReportOnly:   true,
		LastSnapshot: d.lastAt,
		Targets:      len(d.targets),
		Reason: "advisory only: this domain observes and reports, and has no actuator (docs/design/" +
			"compute-domains.md §6 U9)",
	}
	h.Ready = len(d.targets) > 0 && !d.stale
	if !h.Ready {
		switch {
		case len(d.targets) == 0:
			h.Reason = "no Lambda functions learned yet; " + h.Reason
		default:
			h.Reason = d.staleReason + "; " + h.Reason
		}
	}
	return h
}

// checkpoint is the persisted shape. It is a plain snapshot: the domain's
// learned state IS the last window of evidence, so a checkpoint round-trips
// exactly and deterministically (slices sorted, maps encoded by encoding/json
// in sorted key order).
type checkpoint struct {
	Version int       `json:"version"`
	Snap    *Snapshot `json:"snapshot"`
}

// Checkpoint implements domain.Domain. Output is deterministic.
func (d *Domain) Checkpoint() ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return json.Marshal(checkpoint{Version: 1, Snap: d.snapshot(d.lastAt)})
}

// Restore implements domain.Domain.
func (d *Domain) Restore(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var c checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("lambda: restore checkpoint: %w", err)
	}
	if c.Version != 1 {
		return fmt.Errorf("lambda: unknown checkpoint version %d", c.Version)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets = map[string]Target{}
	if c.Snap == nil {
		return nil
	}
	for _, t := range c.Snap.Targets {
		d.targets[t.Ref.ID] = t
	}
	d.scope, d.region = c.Snap.Scope, c.Snap.Region
	d.window, d.lastAt, d.stale = c.Snap.Window, c.Snap.Timestamp, c.Snap.Stale
	return nil
}

// Generic projects the Lambda snapshot onto the generic seam for the brain's
// account-wide view: the function inventory plus AGGREGATE samples.
//
// It is deliberately lossy and documented as such — per-invocation REPORT
// records do not survive it, so a Domain that only ever saw Generic() output
// refuses with [ReasonNoReportEvidence]. Use it to give the brain an inventory
// and a health signal, never as the decision path's evidence.
func (s *Snapshot) Generic() *domain.Snapshot {
	if s == nil {
		return nil
	}
	g := &domain.Snapshot{
		Domain: Kind, Scope: s.Scope, Timestamp: s.Timestamp, Stale: s.Stale,
	}
	if s.Stale {
		g.StaleReason = "lambda collector delivered an incomplete snapshot"
	}
	for _, t := range s.Targets {
		g.Targets = append(g.Targets, domain.Target{
			Ref:    t.Ref,
			Spec:   SpecFor(t.Function, t.Function.MemoryMB, t.Function.Arch()),
			Labels: copyTags(t.Function.Tags),
			Blind: []string{
				"per-invocation REPORT records do not fit domain.Sample; use lambda.Domain.Observe",
			},
		})
		for _, ser := range t.Series {
			for _, p := range ser.Points {
				g.Samples = append(g.Samples, domain.Sample{
					Ref: t.Ref, Metric: ser.Metric, Value: p.Value,
					Timestamp: p.At, WindowSeconds: ser.PeriodSeconds,
				})
			}
		}
	}
	return g
}
