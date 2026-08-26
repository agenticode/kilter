// Package ec2 is the `ec2` compute domain: plain-EC2 instances (pkg/ec2) and
// the EBS volumes attached to them (pkg/ebs), behind the one [domain.Kind]
// they share.
//
// # Why this package exists at all
//
// Two packages shipped working, tested decision cores for the same Kind and
// stopped at their own boundary. pkg/ec2 (U5) mirrors §5.2's types field for
// field but never implements [domain.Domain] — pkg/domain did not exist when
// it was written. pkg/ebs (U6) does implement it, registers under
// [domain.EC2], and its FINDINGS §6.3 states the consequence: a registry holds
// one domain per Kind, so cmd/ must register a composite or the two halves
// cannot coexist.
//
// This package is that composite plus the missing adapter. It is placed at
// pkg/domain/ec2 because §5.1 puts it there, and it lives here rather than in
// cmd/ so the wiring is testable without a CLI and reusable by pkg/api.
//
// Nothing here links an AWS SDK. Both halves take recorded snapshots; the
// SDK adapters that fill them belong in cmd/ and are not in this unit.
package ec2

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	kebs "github.com/agenticode/kilter/pkg/ebs"
	kec2 "github.com/agenticode/kilter/pkg/ec2"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Kind is the compute domain both halves serve.
const Kind = domain.EC2

// Part names, used in composite health reasons and in checkpoints. They are
// persisted, so they are constants rather than literals.
const (
	PartInstances = "instances"
	PartVolumes   = "volumes"
)

// DefaultStaleAfter is how old the newest snapshot may be before the instance
// half stops being ready. Plain EC2 is collected on a slow loop (CloudWatch
// publishes at 300 s at best), so the horizon is far wider than the
// Kubernetes domains' 15 minutes.
const DefaultStaleAfter = 2 * time.Hour

// Config wires the whole domain.
type Config struct {
	// Scope is the accountID/region this domain covers; snapshots override it.
	Scope string
	// Region labels commitment usage lines.
	Region string

	// Catalog prices instances. Required for the instance half; nil disables
	// it, which is a legal wiring (volumes only) and not an error.
	Catalog *pricing.Catalog
	// Instances tunes the instance sizer. Zero value ⇒ kec2.DefaultConfig().
	Instances kec2.Config
	// Volumes tunes the EBS domain. Zero value ⇒ kebs.DefaultConfig().
	Volumes kebs.Config

	// StaleAfter is the instance half's freshness horizon. Zero ⇒
	// DefaultStaleAfter. (The EBS half has its own, in Volumes.StaleAfter.)
	StaleAfter time.Duration

	// VolumeActuationAvailable reports that an EBS actuator is wired.
	// FALSE BY DEFAULT, and deliberately separate from anything the domain
	// says about itself: forgetting to wire credentials can never read as
	// permission to act. The instance half has NO actuation at all — U7 was
	// never built — so there is no flag for it.
	VolumeActuationAvailable bool
}

// New builds the composite `ec2` domain: the instance half, the volume half,
// and the routing between them.
//
// Routing is by target ID prefix, which is safe here for a reason worth
// stating: EC2 instance IDs begin `i-` and EBS volume IDs begin `vol-`, so the
// two ID spaces cannot collide, and pkg/ebs already ignores non-volume targets
// in a shared snapshot. The predicate is declared HERE rather than guessed by
// pkg/domain, because the composite must not invent knowledge about what its
// parts own.
func New(cfg Config) (*domain.Composite, error) {
	vols, err := NewVolumes(cfg)
	if err != nil {
		return nil, err
	}
	inst, err := NewInstances(cfg)
	if err != nil {
		return nil, err
	}
	return domain.NewComposite(Kind,
		domain.Part{Name: PartInstances, Domain: inst, Owns: ownsInstance},
		domain.Part{Name: PartVolumes, Domain: vols, Owns: ownsVolume, Accepts: volumeSnapshot},
	)
}

// volumeSnapshot reports whether a snapshot is addressed to the EBS half.
//
// It exists because of a real ordering bug, found by the CLI's
// shuffle-invariance test and recorded in cmd/FINDINGS.md §5.1. Two collectors
// feed the `ec2` kind: pkg/ebs ships the generic Targets/Samples shape, and
// pkg/ec2 ships an opaque Payload. Handed the instance snapshot, pkg/ebs sees
// zero volume targets and — correctly, for a package whose contract is one
// collector per domain — concludes its collector delivered no volumes and
// degrades itself. Whichever snapshot arrived last then decided the domain's
// health, so the same inputs in a different order produced a different report.
//
// The rule is deliberately narrow: reject only a snapshot whose entire content
// is another half's opaque payload. An EBS snapshot with no volumes is still
// accepted, because an empty account is a real answer and "we looked and found
// none" must stay distinguishable from "we never looked".
func volumeSnapshot(s *domain.Snapshot) bool {
	if s == nil {
		return false
	}
	return len(s.Payload) == 0 || len(s.Targets) > 0
}

// ownsVolume reports whether a recommendation targets an EBS volume. The
// volume-type attribute is checked first because it is what pkg/ebs actually
// keys on; the ID prefix is the fallback for a recommendation that lost its
// attrs in a round trip.
func ownsVolume(r domain.Recommendation) bool {
	if r.Current.Attr(kebs.AttrVolumeType) != "" {
		return true
	}
	return strings.HasPrefix(r.Target.ID, "vol-")
}

// ownsInstance is the complement: anything that is not a volume.
func ownsInstance(r domain.Recommendation) bool { return !ownsVolume(r) }

// NewVolumes builds the EBS half on its own, wrapped so it can explain the
// volumes it declined to change (see [Volumes.Refusals]).
func NewVolumes(cfg Config) (*Volumes, error) {
	vc := cfg.Volumes
	if vc.Scope == "" {
		vc.Scope = cfg.Scope
	}
	if vc.Region == "" {
		vc.Region = cfg.Region
	}
	vc.ActuationAvailable = cfg.VolumeActuationAvailable
	d, err := kebs.New(vc)
	if err != nil {
		return nil, fmt.Errorf("domain/ec2: volumes: %w", err)
	}
	return &Volumes{Domain: d}, nil
}

// Volumes is pkg/ebs's domain plus the one thing the seam wanted and that
// package could not add: its refusals.
//
// pkg/ebs produces exactly one verdict per volume and most of them are
// refusals — `no-cheaper-config` for the 334-375 GiB band, `not-gp2` for io2,
// `unmeasured` for a thin window. None of those can be a
// [domain.Recommendation]: a recommendation whose Proposed equals its Current
// fails Validate, correctly. They live in kebs.Report, which the generic seam
// had no way to carry, so a caller reading only recommendations would see an
// empty answer where the truth is "we looked at every volume and here is why".
type Volumes struct {
	*kebs.Domain
}

// Refusals implements [domain.Refuser] by projecting the EBS report.
func (v *Volumes) Refusals(now time.Time, ledger domain.Netter) []domain.Refusal {
	if v == nil || v.Domain == nil {
		return nil
	}
	rep := v.Assess(now, ledger)
	out := make([]domain.Refusal, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		if a.Refusal == nil {
			continue
		}
		out = append(out, domain.Refusal{
			Target: a.Ref,
			Code:   a.Refusal.Code,
			Reason: a.Refusal.Reason,
		})
	}
	domain.SortRefusals(out)
	return out
}

// Report exposes the EBS half's native report, which carries the parity math,
// the "what a naive tool would have done to this volume" column and the
// per-volume evidence the generic shape has no room for.
func (v *Volumes) Report(now time.Time, ledger domain.Netter) kebs.Report {
	return v.Assess(now, ledger)
}

// Instances is the [domain.Domain] adapter over pkg/ec2's sizer.
//
// pkg/ec2 is stateless by design: CloudWatch is the history store, so one
// collection pass carries the whole window and there is no cross-tick
// histogram to fold. The adapter is therefore a snapshot holder, not a
// learner — Learn stores, Recommend assesses, Checkpoint persists the
// snapshot.
//
// It is REPORT-ONLY, structurally and permanently for this unit: U7 (EC2
// stop/modify/start behind approval) was never built, so there is no actuator,
// no flag that could enable one, and PlanSteps refuses unconditionally.
type Instances struct {
	sizer      *kec2.Sizer
	scope      string
	region     string
	staleAfter time.Duration
	priced     bool

	mu   sync.RWMutex
	snap *kec2.Snapshot
}

// NewInstances builds the instance half.
//
// A nil catalog is legal: the half then reports report-only with a reason
// naming the missing catalog, rather than pricing every instance at zero. An
// unpriced instance is a refusal in pkg/ec2 already; an unpriced FLEET is a
// wiring mistake and says so.
func NewInstances(cfg Config) (*Instances, error) {
	d := &Instances{
		scope:      cfg.Scope,
		region:     cfg.Region,
		staleAfter: cfg.StaleAfter,
		priced:     cfg.Catalog != nil,
	}
	if d.staleAfter <= 0 {
		d.staleAfter = DefaultStaleAfter
	}
	if !d.priced {
		return d, nil
	}
	sc := cfg.Instances
	if sc.Provider == "" {
		sc = mergeInstanceConfig(sc)
	}
	s, err := kec2.NewSizer(cfg.Catalog, sc)
	if err != nil {
		return nil, fmt.Errorf("domain/ec2: instances: %w", err)
	}
	d.sizer = s
	return d, nil
}

// mergeInstanceConfig fills an unset config from the package defaults one
// field at a time, so a caller that set two fields does not lose the rest.
func mergeInstanceConfig(in kec2.Config) kec2.Config {
	def := kec2.DefaultConfig()
	if in.Provider == "" {
		in.Provider = def.Provider
	}
	if in.CPUPercentile <= 0 {
		in.CPUPercentile = def.CPUPercentile
	}
	if in.CPUHeadroom <= 0 {
		in.CPUHeadroom = def.CPUHeadroom
	}
	if in.MemHeadroom <= 0 {
		in.MemHeadroom = def.MemHeadroom
	}
	if in.CoarseResolutionHeadroom <= 0 {
		in.CoarseResolutionHeadroom = def.CoarseResolutionHeadroom
	}
	if in.MinWindow <= 0 {
		in.MinWindow = def.MinWindow
	}
	if in.MinSampleCoverage <= 0 {
		in.MinSampleCoverage = def.MinSampleCoverage
	}
	if in.MinConfidence <= 0 {
		in.MinConfidence = def.MinConfidence
	}
	return in
}

// Kind implements [domain.Domain].
func (d *Instances) Kind() domain.Kind { return Kind }

// Learn stores the snapshot carried in the generic envelope's Payload.
//
// A snapshot with no payload degrades this half to report-only rather than
// erroring — an unreachable collector is an operational condition — and a
// payload it cannot decode does the same, loudly, because a malformed snapshot
// must not be mistaken for an empty account. A snapshot with targets but no
// payload is a shared EC2-domain snapshot (volumes, most likely) and is
// ignored here without disturbing what this half already learned, exactly as
// pkg/ebs ignores instance targets.
func (d *Instances) Learn(snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q delivered to %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	if len(snap.Payload) == 0 {
		return nil
	}
	var native kec2.Snapshot
	if err := json.Unmarshal(snap.Payload, &native); err != nil {
		return fmt.Errorf("domain/ec2: instances: decode payload: %w", err)
	}
	if native.Domain != "" && native.Domain != kec2.Domain {
		// Not ours: an ec2-scope snapshot may legitimately carry another
		// half's payload.
		return nil
	}
	if native.Scope == "" {
		native.Scope = snap.Scope
	}
	if native.Scope == "" {
		native.Scope = d.scope
	}
	if native.Region == "" {
		native.Region = d.region
	}
	if native.Timestamp.IsZero() {
		native.Timestamp = snap.Timestamp
	}
	if snap.Stale {
		native.Stale = true
		if snap.StaleReason != "" {
			native.Warnings = appendUnique(native.Warnings, snap.StaleReason)
		}
	}
	d.mu.Lock()
	d.snap = &native
	d.mu.Unlock()
	return nil
}

func appendUnique(in []string, s string) []string {
	for _, v := range in {
		if v == s {
			return in
		}
	}
	out := append(in, s)
	sort.Strings(out)
	return out
}

// Observe is the native ingest path, for a caller that already holds a
// pkg/ec2 snapshot and does not want to round-trip it through JSON.
func (d *Instances) Observe(snap *kec2.Snapshot) error {
	if snap == nil {
		return nil
	}
	cp := *snap
	d.mu.Lock()
	d.snap = &cp
	d.mu.Unlock()
	return nil
}

// Report runs the sizer, returning nil when nothing has been learned or no
// catalog was wired.
func (d *Instances) Report(now time.Time, ledger domain.Netter) *kec2.Report {
	d.mu.RLock()
	snap := d.snap
	d.mu.RUnlock()
	if d.sizer == nil || snap == nil {
		return nil
	}
	return d.sizer.Assess(now, snap, domain.InventoryOf(ledger))
}

// Recommend projects the sizer's report into the generic shape.
//
// Only assessments with a PROPOSAL become recommendations: an assessment that
// refused has no alternative spec, and [domain.Recommendation.Validate]
// rightly rejects a recommendation that proposes no change. The refusals are
// not lost — they come back through [Instances.Refusals].
//
// Advisories (Graviton) are deliberately NOT projected as recommendations
// here. pkg/ec2 keeps their money in a separate total precisely so an
// architecture port whose portability nobody verified cannot be added to a
// savings figure, and projecting them into a list whose totals are summed
// would undo that. They stay in [Instances.Report].
func (d *Instances) Recommend(now time.Time, ledger domain.Netter) []domain.Recommendation {
	rep := d.Report(now, ledger)
	if rep == nil {
		return nil
	}
	out := make([]domain.Recommendation, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		rec, ok := recommendationOf(a)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	domain.SortRecommendations(out)
	return out
}

// recommendationOf projects one assessment, or reports that it is a refusal.
func recommendationOf(a kec2.Assessment) (domain.Recommendation, bool) {
	if a.Proposal == nil {
		return domain.Recommendation{}, false
	}
	p := a.Proposal
	rec := domain.Recommendation{
		Target:            targetRefOf(a.Target),
		Current:           specOf(a.Current),
		Proposed:          specOf(p.Spec),
		CurrentHourlyUSD:  a.CurrentHourlyUSD,
		ProposedHourlyUSD: p.ProposedHourlyUSD,
		// U5 has no actuator, so every instance change is advice until U7
		// exists. Labelling a stop-start as anything an executor might plan
		// would be a promise this wiring cannot keep.
		Action:     domain.ActionAdvisory,
		Risk:       p.Risk,
		Confidence: a.Confidence.Score,
		Evidence:   evidenceOf(a.Evidence),
		Reason:     p.Reason,
	}
	// SetSavings is the only supported way to populate the two fields, and it
	// clamps net to gross. pkg/ec2 already nets through the commitment
	// waterfall; this re-asserts the invariant at the seam rather than
	// trusting a number that crossed a package boundary.
	rec.SetSavings(p.GrossSavingsMonthlyUSD, p.NetSavingsMonthlyUSD)
	if s, ok := firstSuppression(a.Suppressions); ok {
		rec.Suppressed, rec.SuppressCode, rec.Reason, rec.ValidFrom = true, s.Code, s.Reason, s.ValidFrom
	}
	if len(rec.Evidence) == 0 {
		// Validate rejects an evidence-free recommendation, and it is right
		// to: a claim with no stated basis is not reviewable. Rather than
		// drop the recommendation silently, state the one fact that is always
		// true of it.
		rec.Evidence = []domain.Evidence{{
			Metric: "instance-type",
			Value:  a.Current.Attrs[kec2.AttrInstanceType],
			Source: "describe-instances",
		}}
	}
	return rec, true
}

func firstSuppression(ss []kec2.Suppression) (kec2.Suppression, bool) {
	if len(ss) == 0 {
		return kec2.Suppression{}, false
	}
	return ss[0], true
}

// Refusals implements [domain.Refuser]: every instance the sizer declined to
// resize, with the reason code that stopped it.
func (d *Instances) Refusals(now time.Time, ledger domain.Netter) []domain.Refusal {
	rep := d.Report(now, ledger)
	if rep == nil {
		return nil
	}
	out := make([]domain.Refusal, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		if a.Proposal != nil {
			continue
		}
		s, ok := firstSuppression(a.Suppressions)
		if !ok {
			// pkg/ec2 guarantees this cannot happen (Report.Validate rejects
			// it), so say so rather than dropping the instance.
			s = kec2.Suppression{Code: "unstated", Reason: "the sizer produced neither a proposal nor a reason"}
		}
		out = append(out, domain.Refusal{
			Target:    targetRefOf(a.Target),
			Code:      s.Code,
			Reason:    s.Reason,
			ValidFrom: s.ValidFrom,
		})
	}
	domain.SortRefusals(out)
	return out
}

// PlanSteps refuses unconditionally.
//
// This is not a stub. pkg/ec2 has no actuation surface — no Actuator, no
// mutating method, no flag that would enable one — because U7 was never built,
// and a domain that could be talked into emitting a step would be one bug away
// from stopping a production instance. [domain.Registry] refuses steps for a
// report-only domain already; this refuses again, so a caller holding the
// half directly hits the same wall.
func (d *Instances) PlanSteps([]domain.Recommendation, domain.Guard) ([]domain.Step, error) {
	return nil, fmt.Errorf("%w: %s: instance actuation is not implemented (design §6 U7)",
		domain.ErrReportOnly, Kind)
}

// Health reports what this half can currently do.
func (d *Instances) Health(now time.Time) domain.Health {
	h := domain.Health{Kind: Kind, ReportOnly: true}
	switch {
	case !d.priced:
		h.Reason = "no pricing catalog is wired: instances cannot be priced, so no bill delta can be computed"
		return h
	default:
	}
	d.mu.RLock()
	snap := d.snap
	d.mu.RUnlock()
	if snap == nil {
		h.Reason = "no snapshot has been learned: report-only until a collector delivers one"
		return h
	}
	h.Targets = len(snap.Targets)
	h.LastSnapshot = snap.Timestamp
	age := now.Sub(snap.Timestamp)
	switch {
	case !now.IsZero() && age > d.staleAfter:
		h.Reason = fmt.Sprintf("newest snapshot is %s old (limit %s)", age.Round(time.Second), d.staleAfter)
	case snap.Stale:
		h.Ready = true
		h.Reason = "partial collection: " + strings.Join(snap.Warnings, "; ")
	default:
		h.Ready = true
		h.Reason = "instance actuation is not implemented (design §6 U7)"
	}
	return h
}

// instanceCheckpoint is the persisted form.
type instanceCheckpoint struct {
	Version  int            `json:"version"`
	Snapshot *kec2.Snapshot `json:"snapshot,omitempty"`
}

// Checkpoint persists the last snapshot. Deterministic: every field of
// kec2.Snapshot is already sorted by its collector, and encoding/json sorts
// map keys.
func (d *Instances) Checkpoint() ([]byte, error) {
	d.mu.RLock()
	snap := d.snap
	d.mu.RUnlock()
	return json.Marshal(instanceCheckpoint{Version: 1, Snapshot: snap})
}

// Restore reloads a checkpoint.
func (d *Instances) Restore(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var cp instanceCheckpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return fmt.Errorf("domain/ec2: instances: restore: %w", err)
	}
	d.mu.Lock()
	d.snap = cp.Snapshot
	d.mu.Unlock()
	return nil
}

// targetRefOf converts pkg/ec2's §5.2-shaped ref into the real one.
func targetRefOf(r kec2.TargetRef) domain.TargetRef {
	return domain.TargetRef{Domain: Kind, Scope: r.Scope, ID: r.ID, Name: r.Name}
}

// specOf converts pkg/ec2's spec, copying the attribute map so the
// recommendation cannot mutate when the report does.
func specOf(s kec2.Spec) domain.Spec {
	out := domain.Spec{Resources: s.Resources}
	if len(s.Attrs) == 0 {
		return out
	}
	out.Attrs = make(map[string]string, len(s.Attrs))
	for k, v := range s.Attrs {
		out.Attrs[k] = v
	}
	return out
}

func evidenceOf(in []kec2.Evidence) []domain.Evidence {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.Evidence, len(in))
	for i, e := range in {
		out[i] = domain.Evidence{
			Metric: e.Metric, Value: e.Value, Window: e.Window,
			Samples: e.Samples, Source: e.Source, At: e.At,
		}
	}
	return out
}

// UsageLines projects the instance half's priced inventory into commitment
// usage lines, so the brain can build the ACCOUNT-WIDE baseline every domain's
// ledger splices into. Without it, a domain's net saving is computed against a
// baseline containing only its own targets, which understates absorption and
// overstates the saving (§4.4 ex.3).
//
// It is a projection of what pkg/ec2 already priced, so an instance the
// catalog cannot price is omitted rather than billed at zero.
func (d *Instances) UsageLines(now time.Time, ledger domain.Netter) []commit.UsageLine {
	rep := d.Report(now, ledger)
	if rep == nil {
		return nil
	}
	out := make([]commit.UsageLine, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		if a.CurrentHourlyUSD <= 0 {
			continue
		}
		it := a.Current.Attrs[kec2.AttrInstanceType]
		if it == "" {
			continue
		}
		out = append(out, commit.UsageLine{
			ID:           a.Target.ID,
			Kind:         commit.KindEC2,
			Region:       rep.Region,
			InstanceType: it,
			Quantity:     1,
			ODRate:       a.CurrentHourlyUSD,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
