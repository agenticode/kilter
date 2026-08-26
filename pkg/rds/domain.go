package rds

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Domain adapts this package to the pkg/domain seam — its READ-ONLY half, and
// only its read-only half.
//
// Kind, Learn, Recommend, Refusals, Health, Checkpoint and Restore are
// implemented. PlanSteps is implemented as an UNCONDITIONAL refusal, and
// Health.ReportOnly is true unconditionally. Neither is a stub and neither is
// a "not yet": there is no code path in this package that can produce an
// executable step, because [Proposal] cannot name a DB instance class and no
// mutating API appears anywhere in the package (TestNoMutatingAPISurface).
// [domain.Registry] already refuses steps for a report-only domain; this
// refuses again, so a caller holding the *Domain directly hits the same wall.
//
// # Why Recommend returns nothing and Refusals returns everything
//
// [domain.Recommendation.Validate] rejects a recommendation whose Proposed
// equals its Current — correctly, because a recommendation to change nothing
// is not a recommendation. This domain proposes nothing, so it has no
// Recommendations to return, and a caller that rendered only Recommendations
// would show a user an empty RDS section and let them conclude the tool found
// nothing.
//
// That is exactly the case pkg/domain/refusal.go was added for: "Rendering
// only Recommendations would show a user an empty report and let them conclude
// the tool found nothing — which is a different claim from 'the tool declined
// to guess, here is what it needs'." So *Domain implements [domain.Refuser],
// and the refusals are the output.
//
// # Two ingest paths, and why
//
// [Domain.Observe] takes the RDS-native [Snapshot]. [Domain.Learn] takes the
// generic [domain.Snapshot] and decodes the native one out of its Payload
// field when present.
//
// The generic Targets/Samples shape alone is insufficient here for the reason
// pkg/domain/snapshot.go already documents for pkg/ec2 and pkg/ecs: the
// evidence is per-target series with per-series STATUS, and "we were not told"
// must stay distinguishable from "there was nothing". A [domain.Sample] has no
// room for a truncation flag, so a series flattened into samples arrives
// looking complete — and a truncated DatabaseConnections series that looks
// complete is an idle verdict manufactured out of silence. Learn therefore
// prefers Payload and treats bare samples as configuration only.
type Domain struct {
	cfg   Config
	sizer *Sizer

	mu          sync.RWMutex
	scope       string
	region      string
	targets     map[string]Target
	reservation []commit.ReservedDBInstance
	window      Window
	lastAt      time.Time
	stale       bool
	staleReason string
}

// NewDomain builds the read-only RDS domain.
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

// Observe folds an RDS-native snapshot into learned state. The newest snapshot
// for an instance REPLACES the previous one rather than accumulating: the
// collector re-queries a window of metrics each tick, and accumulating would
// double-count the overlap. The window is the evidence.
//
// One thing survives the replacement: the previous allocated storage, carried
// forward into [Target.PriorAllocatedStorageGiB]. That is trap 8's ledger
// rule — storage autoscaling moves the floor on its own and leaves no
// CloudTrail event, so a growth between two snapshots must be recorded as
// unattributed rather than lost.
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
	if len(snap.Reservations) > 0 {
		d.reservation = append([]commit.ReservedDBInstance(nil), snap.Reservations...)
		SortReservations(d.reservation)
	}
	for _, t := range snap.Targets {
		if prev, ok := d.targets[t.Ref.ID]; ok && t.PriorAllocatedStorageGiB == 0 {
			t.PriorAllocatedStorageGiB = prev.Instance.AllocatedStorageGiB
		}
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

// Learn implements domain.Domain over the generic seam.
//
// Failure policy matches the rest of the seam: a nil, empty or payload-less
// snapshot degrades the domain and returns nil — an unreachable collector is
// an operational condition, not a programming error. Only a snapshot addressed
// to another domain is an error, because that is a wiring bug.
func (d *Domain) Learn(snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q is not %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	// The lossless path. A domain-native snapshot in Payload carries the
	// per-series truncation status the generic shape cannot.
	if len(snap.Payload) > 0 {
		var native Snapshot
		if err := json.Unmarshal(snap.Payload, &native); err == nil && native.Domain == Kind {
			if snap.Commitments != nil && len(native.Reservations) == 0 {
				native.Reservations = snap.Commitments.ReservedDBs
			}
			return d.Observe(&native)
		}
		// A payload this domain cannot decode degrades it to report-only
		// rather than failing the brain.
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if snap.Scope != "" {
		d.scope = snap.Scope
	}
	d.stale, d.staleReason = snap.Stale, snap.StaleReason
	if snap.Commitments != nil && len(snap.Commitments.ReservedDBs) > 0 {
		d.reservation = append([]commit.ReservedDBInstance(nil), snap.Commitments.ReservedDBs...)
		SortReservations(d.reservation)
	}
	if len(snap.Targets) == 0 {
		d.stale = true
		if d.staleReason == "" {
			d.staleReason = "collector delivered no RDS targets"
		}
		return nil
	}

	byRef := map[string][]domain.Sample{}
	for _, s := range snap.Samples {
		byRef[s.Ref.ID] = append(byRef[s.Ref.ID], s)
	}
	for _, gt := range snap.Targets {
		t := Target{Ref: gt.Ref, Instance: instanceFromSpec(gt)}
		t.Series = seriesFromSamples(byRef[gt.Ref.ID])
		if prev, ok := d.targets[gt.Ref.ID]; ok {
			t.Cluster = prev.Cluster
			t.PriorAllocatedStorageGiB = prev.Instance.AllocatedStorageGiB
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

// instanceFromSpec reconstructs what the generic seam can carry. Every field
// is optional; an attribute that does not parse is left at its zero value,
// which the sizer then refuses on rather than guesses at.
func instanceFromSpec(t domain.Target) DBInstance {
	d := DBInstance{ARN: t.Ref.ID, Identifier: t.Ref.Name, Tags: copyTags(t.Labels)}
	d.Class = t.Spec.Attr(AttrClass)
	d.Engine = t.Spec.Attr(AttrEngine)
	d.EngineVersion = t.Spec.Attr(AttrEngineVersion)
	d.LicenseModel = t.Spec.Attr(AttrLicenseModel)
	d.StorageType = t.Spec.Attr(AttrStorageType)
	d.ReplicaSource = t.Spec.Attr(AttrReplicaOf)
	d.ClusterID = t.Spec.Attr(AttrClusterID)
	d.MultiAZ = t.Spec.Attr(AttrMultiAZ) == "true"
	d.AllocatedStorageGiB = atoi64(t.Spec.Attr(AttrAllocatedStorageGiB))
	d.MaxAllocatedStorageGiB = atoi64(t.Spec.Attr(AttrMaxAllocatedStorage))
	d.IOPS = int32(atoi64(t.Spec.Attr(AttrIOPS)))
	d.StorageThroughputMBps = int32(atoi64(t.Spec.Attr(AttrStorageThroughput)))
	return d
}

// seriesFromSamples groups generic samples into metric series, sorted by
// metric then timestamp so the result cannot depend on delivery order.
//
// Every series produced here is marked Partial. That is not pessimism: a
// [domain.Sample] has no truncation flag, so a series rebuilt from samples
// genuinely cannot say whether CloudWatch answered in full — and this domain's
// idle verdict must never fire on a series that cannot say so. Deliver the
// native snapshot through Payload to get a complete one.
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
		sort.SliceStable(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
		out = append(out, Series{
			Metric: m, PeriodSeconds: period[m], Points: pts, Source: "domain-sample",
			Partial: true, Status: StatusPartialData,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

func atoi64(s string) int64 {
	if s == "" {
		return 0
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
		if n > 1<<40 { // no plausible RDS attribute is this large
			return 0
		}
	}
	return n
}

// snapshot renders the learned state as an RDS snapshot. Caller holds d.mu.
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
	snap.Reservations = append([]commit.ReservedDBInstance(nil), d.reservation...)
	SortTargets(snap.Targets)
	SortReservations(snap.Reservations)
	return snap
}

// Report renders the full read-only verdict — what `kilter --domain rds`
// prints and what the UI card mirrors.
func (d *Domain) Report(now time.Time, ledger domain.Netter) *Report {
	d.mu.RLock()
	snap := d.snapshot(now)
	d.mu.RUnlock()
	return d.sizer.Assess(now, snap, ledger)
}

// Recommend implements domain.Domain and returns nothing, always.
//
// This is the honest answer, not an omission: a [domain.Recommendation]
// requires a Proposed spec that differs from Current, and the only fields this
// domain could change are ones it will not touch. The complete output is
// [Domain.Refusals] and [Domain.Report]. An empty slice rather than nil so a
// caller ranging over it sees "asked and answered" rather than "never ran".
func (d *Domain) Recommend(now time.Time, ledger domain.Netter) []domain.Recommendation {
	return []domain.Recommendation{}
}

// Refusals implements domain.Refuser: every target this domain declined to
// change, and why. This IS the domain's output.
func (d *Domain) Refusals(now time.Time, ledger domain.Netter) []domain.Refusal {
	return d.Report(now, ledger).Refusals()
}

// PlanSteps implements domain.Domain by refusing, always.
//
// The error is [domain.ErrReportOnly] so callers already written against the
// seam handle it without a special case.
func (d *Domain) PlanSteps([]domain.Recommendation, domain.Guard) ([]domain.Step, error) {
	return nil, fmt.Errorf("%w: the RDS domain is advisory only — an instance-class change is a failover "+
		"that rewrites a DNS record and drops every pooled connection, allocated storage cannot be reduced "+
		"by any API, and no action class in this engine can represent either honestly (docs/design/"+
		"rds-batch-assessment.md §2.7, §2.3)", domain.ErrReportOnly)
}

// Health reports readiness as of now. ReportOnly is true unconditionally and
// by construction; see [Domain.PlanSteps].
func (d *Domain) Health(now time.Time) domain.Health {
	d.mu.RLock()
	defer d.mu.RUnlock()
	h := domain.Health{
		Kind:         Kind,
		ReportOnly:   true,
		LastSnapshot: d.lastAt,
		Targets:      len(d.targets),
		Reason: "advisory only: this domain observes and reports, and has no actuator (docs/design/" +
			"rds-batch-assessment.md §5 U11)",
	}
	h.Ready = len(d.targets) > 0 && !d.stale
	if !h.Ready {
		switch {
		case len(d.targets) == 0:
			h.Reason = "no RDS DB instances learned yet; " + h.Reason
		default:
			h.Reason = d.staleReason + "; " + h.Reason
		}
	}
	return h
}

// checkpoint is the persisted shape. The domain's learned state IS the last
// window of evidence, so a checkpoint round-trips exactly and
// deterministically (slices sorted, maps encoded by encoding/json in sorted
// key order).
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
		return fmt.Errorf("rds: restore checkpoint: %w", err)
	}
	if c.Version != 1 {
		return fmt.Errorf("rds: unknown checkpoint version %d", c.Version)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets = map[string]Target{}
	d.reservation = nil
	if c.Snap == nil {
		return nil
	}
	for _, t := range c.Snap.Targets {
		d.targets[t.Ref.ID] = t
	}
	d.reservation = append([]commit.ReservedDBInstance(nil), c.Snap.Reservations...)
	d.scope, d.region = c.Snap.Scope, c.Snap.Region
	d.window, d.lastAt, d.stale = c.Snap.Window, c.Snap.Timestamp, c.Snap.Stale
	return nil
}

// Generic projects the RDS snapshot onto the generic seam for the brain's
// account-wide view: the instance inventory, its tags, aggregate samples, and
// the NATIVE snapshot in Payload.
//
// The Targets/Samples half is deliberately lossy and declares its blind spots
// so nobody mistakes it for the decision path. Payload is the lossless half,
// and the reason the seam grew that field.
func (s *Snapshot) Generic() *domain.Snapshot {
	if s == nil {
		return nil
	}
	g := &domain.Snapshot{
		Domain: Kind, Scope: s.Scope, Timestamp: s.Timestamp, Stale: s.Stale,
	}
	if s.Stale {
		g.StaleReason = "rds collector delivered an incomplete snapshot"
	}
	if b, err := json.Marshal(s); err == nil {
		g.Payload = b
	}
	if len(s.Reservations) > 0 {
		g.Commitments = &commit.Inventory{ReservedDBs: append(
			[]commit.ReservedDBInstance(nil), s.Reservations...)}
	}
	for _, t := range s.Targets {
		g.Targets = append(g.Targets, domain.Target{
			Ref:    t.Ref,
			Spec:   SpecFor(t.Instance),
			Labels: copyTags(t.Instance.Tags),
			Blind: []string{
				"per-series CloudWatch truncation status does not fit domain.Sample; use rds.Domain.Observe " +
					"or the Payload field, or every series is treated as partial",
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
