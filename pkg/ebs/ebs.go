// Package ebs rightsizes EBS volumes: it converts gp2 volumes to gp3 at
// measured performance parity, and it is the first Kilter domain that executes
// a non-Kubernetes change.
//
// # Why EBS, and why parity first
//
// gp2 → gp3 is an online ModifyVolume: no downtime, no detach, no restart, and
// a recorded original to go back to. That makes it the smallest blast radius
// in the cloud half of Kilter, which is why it earns the first actuator.
//
// It is also the easiest place to be quietly wrong. gp2's performance is a
// function of SIZE — 3 IOPS/GiB, a burst bucket below 1 TiB, a throughput
// ceiling that steps at 334 GiB — while gp3's free baseline is a flat
// 3,000 IOPS / 125 MiB/s. A "gp3 is 20 % cheaper" rule therefore DOWNGRADES
// every volume above ~1 TiB, by up to 5.3× on IOPS, while reporting a saving
// (§7 trap 6). So the parity math (parity.go) comes first and the actuator
// comes last: a configuration that does not clear measured demand is not a
// candidate at all, cheaper or not.
//
// # Refusal is the default
//
// Every volume observed yields exactly one [Assessment], and an assessment
// without a proposal always carries the reason why. The refusals are:
// unmeasured (no IOPS series ⇒ no parity claim), already gp3/io1/io2, a
// volume state or attachment state that forbids modification, a modification
// already in flight, the post-modification cooldown, an observation window too
// short to have seen the business peak, demand no gp3 configuration can meet,
// and a parity configuration that is not actually cheaper. Silence is never an
// output.
//
// # Structure
//
//	parity.go    the arithmetic — pure, no clock, no I/O, no state
//	collect.go   the read seams (DescribeVolumes / GetMetricData shaped)
//	ebs.go       the domain.Domain implementation (this file)
//	actuate.go   the domain.Actuator implementation (ModifyVolume shaped)
//	fixture.go   recorded fixtures replaying all four seams
//
// # Determinism
//
// No clock: callers pass `now`, and the two interface methods that take none
// ([DomainCollector.Collect], [Actuator.Execute]) hold a caller-supplied clock
// function. No package-level mutable state. Every map iteration is sorted by
// an intrinsic key, so shuffling volumes, samples or tags cannot change a byte
// of the output — pinned by TestOutputIsShuffleInvariant.
//
// # Money
//
// Gross savings are the on-demand list-price delta — the fantasy. The only
// number this package presents as a saving is the bill delta from
// pkg/pricing/commit's waterfall, reached through domain.Netter, and a change
// whose net is not positive is suppressed with its reason attached (§7 trap 1).
// EBS storage is covered by no Savings Plan and no Reserved Instance, so its
// usage lines are marked ineligible: the netting exists to catch what an EBS
// change does to the REST of the bill, not to pretend a volume can be
// committed.
package ebs

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Kind is the domain this package registers as.
//
// EBS volumes are EC2 resources and share the EC2 domain's scope
// (accountID/region), and domain.Kind is a closed set that has no "ebs"
// member — registering under a kind the core does not know is refused by
// domain.Registry.Register, by design. So this domain IS the ec2 kind, in the
// same sense §5.1 puts volumes inside `pkg/domain/ec2`. The consequence for
// wiring is real and documented in FINDINGS.md: a process can register exactly
// one ec2-kind domain, so when the plain-EC2 instance domain (U5, pkg/ec2)
// grows its adapter, cmd/ must register a composite rather than both.
const Kind = domain.EC2

// Attr keys used in domain.Spec.Attrs. They are the volume's vocabulary; the
// canonical rendering of a Spec sorts them, so a Step key is stable.
const (
	AttrVolumeType        = "volumeType"
	AttrSizeGiB           = "sizeGiB"
	AttrIOPS              = "iops"
	AttrThroughputMBps    = "throughputMBps"
	AttrState             = "state"
	AttrAZ                = "az"
	AttrMultiAttach       = "multiAttach"
	AttrAttachedTo        = "attachedTo"
	AttrAttachmentState   = "attachmentState"
	AttrModificationState = "modificationState"
	AttrModificationAt    = "modificationAt"
)

// Refusal codes this file adds to the parity codes in parity.go. They are
// stable strings meant to be stored and matched on; the prose beside them is
// not.
const (
	// Ownership and guardrails.
	ReasonModeOff = "guardrail-mode-off"

	// Nothing to do.
	ReasonNotGP2 = "not-gp2"

	// Evidence quality — we were not shown enough to decide.
	ReasonUnmeasured          = "unmeasured"
	ReasonInsufficientSamples = "insufficient-samples"
	ReasonInsufficientWindow  = "insufficient-window"

	// Volume state — AWS would refuse, or the answer would be unsound.
	ReasonVolumeState            = "volume-state"
	ReasonAttachmentTransition   = "attachment-transition"
	ReasonMultiAttach            = "multi-attach"
	ReasonModificationInProgress = "modification-in-progress"
	ReasonCooldown               = "modification-cooldown"

	// Economics.
	ReasonBelowMinSavings = "below-min-savings"
)

// Defaults. Every one of them is a policy choice a Config may override.
const (
	// DefaultIOPSPercentile targets p99 rather than p95: an IOPS shortfall
	// shows up as latency on the tail, which is where it hurts.
	DefaultIOPSPercentile = 0.99
	// DefaultHeadroom multiplies the measured percentile.
	DefaultHeadroom = 1.3
	// DefaultMinWindow is §4.7's "≥ 7-day window covering business peaks
	// before parity-reduction recs". Below it, proposals are floored at gp2's
	// delivered baseline instead of being provisioned to measurement.
	DefaultMinWindow = 7 * 24 * time.Hour
	// DefaultMinCoverage is the fraction of expected datapoints a window must
	// actually contain before it counts as observed.
	DefaultMinCoverage = 0.8
	// DefaultMinSamples is the floor under any parity claim: fewer points than
	// this and the volume is unmeasured, however long the window looks.
	DefaultMinSamples = 288 // one day of 5-minute datapoints
	// DefaultStaleAfter is how long a snapshot stays usable.
	DefaultStaleAfter = 2 * time.Hour
	// DefaultCooldown is the per-volume modification cooldown — [not
	// re-verified: §4.9 lists "EBS ModifyVolume 6-h cooldown" as unverified],
	// so it is a configurable refusal rather than a hard-coded fact.
	DefaultCooldown = 6 * time.Hour
	// DefaultMinMonthlySavings stops the domain proposing a modification, a
	// cooldown and a risk for pennies.
	DefaultMinMonthlySavings = 1.0
	// DefaultRetainWindow bounds learned state per volume.
	DefaultRetainWindow = 14 * 24 * time.Hour
	// DefaultMaxPoints caps samples kept per series per volume.
	DefaultMaxPoints = 4032 // 14 days of 5-minute datapoints
)

// Config tunes the domain.
type Config struct {
	// Scope is the account/region this domain covers; snapshots override it.
	Scope string
	// Region labels usage lines handed to the commitment ledger.
	Region string
	// Rates prices both sides. The zero value means DefaultRates.
	Rates Rates

	// IOPSPercentile and ThroughputPercentile are the percentiles sizing
	// targets. Zero means DefaultIOPSPercentile.
	IOPSPercentile       float64
	ThroughputPercentile float64
	// IOPSHeadroom and ThroughputHeadroom multiply those percentiles. Zero
	// means DefaultHeadroom. Below 1 is rejected: provisioning under what was
	// measured is the whole failure mode.
	IOPSHeadroom       float64
	ThroughputHeadroom float64

	// MinWindow, MinCoverage and MinSamples decide whether a volume counts as
	// observed, and whether a proposal may go below gp2's delivered baseline.
	MinWindow   time.Duration
	MinCoverage float64
	MinSamples  int

	// Cooldown is the per-volume wait after a modification.
	Cooldown time.Duration
	// StaleAfter is how old the newest snapshot may be before the domain stops
	// being ready.
	StaleAfter time.Duration
	// MinMonthlySavingsUSD is the smallest saving worth a modification.
	MinMonthlySavingsUSD float64
	// DefaultMode is the mode for a volume with no kilter.dev/mode tag.
	// Empty means guard.ModeApply, matching the node domain's default.
	DefaultMode string
	// ActuationAvailable reports that an actuator is wired. False keeps the
	// domain report-only, enforced by the core.
	ActuationAvailable bool

	// RetainWindow and MaxPoints bound learned state per volume.
	RetainWindow time.Duration
	MaxPoints    int
}

// DefaultConfig returns the shipped policy.
func DefaultConfig() Config {
	return Config{
		Rates:                DefaultRates(),
		IOPSPercentile:       DefaultIOPSPercentile,
		ThroughputPercentile: DefaultIOPSPercentile,
		IOPSHeadroom:         DefaultHeadroom,
		ThroughputHeadroom:   DefaultHeadroom,
		MinWindow:            DefaultMinWindow,
		MinCoverage:          DefaultMinCoverage,
		MinSamples:           DefaultMinSamples,
		Cooldown:             DefaultCooldown,
		StaleAfter:           DefaultStaleAfter,
		MinMonthlySavingsUSD: DefaultMinMonthlySavings,
		DefaultMode:          guard.ModeApply,
		RetainWindow:         DefaultRetainWindow,
		MaxPoints:            DefaultMaxPoints,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Rates == (Rates{}) {
		c.Rates = d.Rates
	}
	if c.IOPSPercentile <= 0 {
		c.IOPSPercentile = d.IOPSPercentile
	}
	if c.ThroughputPercentile <= 0 {
		c.ThroughputPercentile = d.ThroughputPercentile
	}
	if c.IOPSHeadroom <= 0 {
		c.IOPSHeadroom = d.IOPSHeadroom
	}
	if c.ThroughputHeadroom <= 0 {
		c.ThroughputHeadroom = d.ThroughputHeadroom
	}
	if c.MinWindow <= 0 {
		c.MinWindow = d.MinWindow
	}
	if c.MinCoverage <= 0 {
		c.MinCoverage = d.MinCoverage
	}
	if c.MinSamples <= 0 {
		c.MinSamples = d.MinSamples
	}
	if c.Cooldown <= 0 {
		c.Cooldown = d.Cooldown
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = d.StaleAfter
	}
	if c.MinMonthlySavingsUSD <= 0 {
		c.MinMonthlySavingsUSD = d.MinMonthlySavingsUSD
	}
	if c.DefaultMode == "" {
		c.DefaultMode = d.DefaultMode
	}
	if c.RetainWindow <= 0 {
		c.RetainWindow = d.RetainWindow
	}
	if c.MaxPoints <= 0 {
		c.MaxPoints = d.MaxPoints
	}
	return c
}

// Validate reports the first policy that would make the domain unsafe.
func (c Config) Validate() error {
	switch {
	case c.IOPSPercentile > 1 || c.ThroughputPercentile > 1:
		return fmt.Errorf("ebs: percentiles must be in (0,1], got %v/%v",
			c.IOPSPercentile, c.ThroughputPercentile)
	case c.IOPSHeadroom < 1 || c.ThroughputHeadroom < 1:
		return fmt.Errorf("ebs: headroom must be at least 1 (provisioning below measurement is the failure mode), got %v/%v",
			c.IOPSHeadroom, c.ThroughputHeadroom)
	case c.MinCoverage > 1:
		return fmt.Errorf("ebs: MinCoverage must be in (0,1], got %v", c.MinCoverage)
	case c.DefaultMode != guard.ModeOff && c.DefaultMode != guard.ModeRecommend && c.DefaultMode != guard.ModeApply:
		return fmt.Errorf("ebs: unknown default mode %q", c.DefaultMode)
	}
	return c.Rates.Validate()
}

// Point is one observation.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// series is a bounded, time-ordered observation set for one metric on one
// volume. Points are deduplicated by timestamp, so replaying an overlapping
// snapshot cannot inflate the sample count that confidence is derived from.
type series struct {
	Points []Point `json:"points,omitempty"`
}

func (s *series) observe(at time.Time, v float64, retain time.Duration, maxPoints int) {
	if at.IsZero() || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return
	}
	at = at.UTC()
	i := sort.Search(len(s.Points), func(i int) bool { return !s.Points[i].At.Before(at) })
	switch {
	case i < len(s.Points) && s.Points[i].At.Equal(at):
		s.Points[i].Value = v // same instant re-delivered: replace, never double-count
	case i == len(s.Points):
		s.Points = append(s.Points, Point{At: at, Value: v})
	default:
		s.Points = append(s.Points, Point{})
		copy(s.Points[i+1:], s.Points[i:])
		s.Points[i] = Point{At: at, Value: v}
	}
	s.prune(retain, maxPoints)
}

// prune drops points older than the retention window relative to the NEWEST
// point (not to a clock this package does not have), then caps the count.
func (s *series) prune(retain time.Duration, maxPoints int) {
	if len(s.Points) == 0 {
		return
	}
	if retain > 0 {
		cutoff := s.Points[len(s.Points)-1].At.Add(-retain)
		i := sort.Search(len(s.Points), func(i int) bool { return !s.Points[i].At.Before(cutoff) })
		if i > 0 {
			s.Points = append([]Point(nil), s.Points[i:]...)
		}
	}
	if maxPoints > 0 && len(s.Points) > maxPoints {
		s.Points = append([]Point(nil), s.Points[len(s.Points)-maxPoints:]...)
	}
}

// percentile returns the p-quantile by nearest rank. Nearest rank, not
// interpolation: with 5-minute datapoints the sample count is small and
// interpolation invents values between real ones.
func (s series) percentile(p float64) (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	vals := make([]float64, len(s.Points))
	for i, pt := range s.Points {
		vals[i] = pt.Value
	}
	sort.Float64s(vals)
	switch {
	case p <= 0:
		return vals[0], true
	case p >= 1:
		return vals[len(vals)-1], true
	}
	rank := int(float64(len(vals))*p + 0.9999999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(vals) {
		rank = len(vals)
	}
	return vals[rank-1], true
}

func (s series) min() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	out := s.Points[0].Value
	for _, p := range s.Points[1:] {
		if p.Value < out {
			out = p.Value
		}
	}
	return out, true
}

// span is the interval the series covers.
func (s series) span() time.Duration {
	if len(s.Points) < 2 {
		return 0
	}
	return s.Points[len(s.Points)-1].At.Sub(s.Points[0].At)
}

// volume is one volume's learned state.
type volume struct {
	Ref    domain.TargetRef  `json:"ref"`
	Spec   domain.Spec       `json:"spec"`
	Labels map[string]string `json:"labels,omitempty"`
	Blind  []string          `json:"blind,omitempty"`
	SeenAt time.Time         `json:"seenAt,omitzero"`

	IOPS  series `json:"iops,omitzero"`
	Tput  series `json:"throughput,omitzero"`
	Burst series `json:"burst,omitzero"`
}

// Domain is the EBS half of the ec2 compute domain. Safe for concurrent use;
// pure — no I/O, no clock.
type Domain struct {
	mu  sync.Mutex
	cfg Config

	volumes map[string]*volume
	// applied records modifications this process executed, so the cooldown
	// survives a collector that has not yet observed the modification record.
	applied map[string]time.Time

	scope       string
	lastAt      time.Time
	stale       bool
	staleReason string
	learned     bool
}

// New builds the EBS domain.
func New(cfg Config) (*Domain, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Domain{
		cfg:     cfg,
		volumes: map[string]*volume{},
		applied: map[string]time.Time{},
		scope:   cfg.Scope,
	}, nil
}

// Kind implements domain.Domain.
func (d *Domain) Kind() domain.Kind { return Kind }

// Learn folds a snapshot into the domain's learned state.
//
// Failure policy matches the seam's contract: an absent or payload-less
// snapshot degrades the domain (Health goes report-only) and returns nil. Only
// a snapshot addressed to another domain is an error, because that is a wiring
// bug rather than an operational condition.
//
// A snapshot that was never addressed to this domain at all is neither: see
// [addressedHere]. It is a no-op, because it is not evidence about this
// domain's collector in any direction.
func (d *Domain) Learn(snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q is not %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	if !addressedHere(snap) {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if snap.Scope != "" {
		d.scope = snap.Scope
	}
	d.stale, d.staleReason = snap.Stale, snap.StaleReason
	if len(snap.Targets) == 0 && len(snap.Samples) == 0 {
		d.stale = true
		if d.staleReason == "" {
			d.staleReason = "collector delivered no volumes"
		}
		return nil
	}
	d.learned = true

	seen := map[string]bool{}
	for _, t := range snap.Targets {
		if t.Ref.ID == "" || !isVolumeTarget(t) {
			// Not a volume. The ec2 kind covers instances too (see [Kind]), so
			// a composite collector's snapshot carries both; this domain reads
			// its half and leaves the rest alone rather than reporting an
			// instance as an unreadable volume.
			continue
		}
		seen[t.Ref.ID] = true
		v := d.volumes[t.Ref.ID]
		if v == nil {
			v = &volume{}
			d.volumes[t.Ref.ID] = v
		}
		ref := t.Ref
		ref.Domain = Kind
		v.Ref, v.Spec, v.Labels, v.Blind = ref, t.Spec, t.Labels, append([]string(nil), t.Blind...)
		sort.Strings(v.Blind)
		if snap.Timestamp.After(v.SeenAt) {
			v.SeenAt = snap.Timestamp
		}
	}
	// A volume that vanished from the inventory was deleted or moved out of
	// scope; keeping its learned state would let it be recommended forever.
	// The sweep is keyed on VOLUME targets, not on targets: a snapshot that
	// carried only instances says nothing about which volumes still exist.
	if len(seen) > 0 {
		for id := range d.volumes {
			if !seen[id] {
				delete(d.volumes, id)
				delete(d.applied, id)
			}
		}
	}

	for _, s := range snap.Samples {
		v := d.volumes[s.Ref.ID]
		if v == nil {
			continue // a sample for a volume no target described: ignore, never invent
		}
		switch s.Metric {
		case SampleIOPS:
			v.IOPS.observe(s.Timestamp, s.Value, d.cfg.RetainWindow, d.cfg.MaxPoints)
		case SampleThroughputMBps:
			v.Tput.observe(s.Timestamp, s.Value, d.cfg.RetainWindow, d.cfg.MaxPoints)
		case SampleBurstBalancePct:
			v.Burst.observe(s.Timestamp, s.Value, d.cfg.RetainWindow, d.cfg.MaxPoints)
		}
	}
	if snap.Timestamp.After(d.lastAt) {
		d.lastAt = snap.Timestamp
	}
	return nil
}

// RecordApplied tells the domain a modification was executed, so the cooldown
// holds even before a collector observes the modification record. The
// controller must call it for every step it completes.
func (d *Domain) RecordApplied(step domain.Step, now time.Time) error {
	if step.Target.ID == "" {
		return fmt.Errorf("ebs: RecordApplied with no target volume")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if prev, ok := d.applied[step.Target.ID]; !ok || now.After(prev) {
		d.applied[step.Target.ID] = now.UTC()
	}
	return nil
}

// Health implements domain.Domain.
func (d *Domain) Health(now time.Time) domain.Health {
	d.mu.Lock()
	defer d.mu.Unlock()

	h := domain.Health{Kind: Kind, LastSnapshot: d.lastAt, Targets: len(d.volumes)}
	switch {
	case !d.learned:
		h.Reason = "no volume inventory learned yet: the EBS collector is absent or has not reported"
		if d.staleReason != "" {
			h.Reason = d.staleReason
		}
	case d.stale:
		h.Reason = "partial collection: " + d.staleReason
	case now.Sub(d.lastAt) > d.cfg.StaleAfter:
		h.Reason = fmt.Sprintf("newest snapshot is %s old (limit %s): treating the collector as down",
			now.Sub(d.lastAt).Round(time.Second), d.cfg.StaleAfter)
	case !d.cfg.ActuationAvailable:
		h.Ready = true
		h.Reason = "actuation is not wired for this domain (no EC2 client): recommendations only"
	default:
		h.Ready = true
	}
	h.ReportOnly = !h.Ready || !d.cfg.ActuationAvailable
	return h
}

// Recommend implements domain.Domain. It is [Domain.Assess] projected onto the
// recommendations that have a proposal; the refusals stay visible in the
// report, which is what cmd/ and the UI render.
func (d *Domain) Recommend(now time.Time, ledger domain.Netter) []domain.Recommendation {
	rep := d.Assess(now, ledger)
	out := make([]domain.Recommendation, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		if a.Recommendation != nil {
			out = append(out, *a.Recommendation)
		}
	}
	domain.SortRecommendations(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// PlanSteps orders applicable recommendations into executable steps.
//
// Report-only is re-checked here even though domain.Registry.PlanSteps already
// enforces it: a caller holding the Domain directly must hit the same wall.
func (d *Domain) PlanSteps(recs []domain.Recommendation, g domain.Guard) ([]domain.Step, error) {
	if h := d.Health(g.Now); h.ReportOnly {
		return nil, fmt.Errorf("%w: %s", domain.ErrReportOnly, h.Reason)
	}
	if err := g.Allow(); err != nil {
		return nil, err
	}
	applicable := make([]domain.Recommendation, 0, len(recs))
	for _, r := range recs {
		if r.Suppressed {
			continue
		}
		if r.Target.Domain != "" && r.Target.Domain != Kind {
			return nil, fmt.Errorf("ebs: recommendation for %q handed to the ebs domain", r.Target.Domain)
		}
		// A volume modification is online. Claiming anything else would make
		// the executor reserve disruption budget it does not need; claiming
		// in-place for something that is not would be worse. gp2 → gp3 is the
		// only change this unit plans, and it is in-place.
		if r.Action != domain.ActionInPlace {
			return nil, fmt.Errorf("ebs: recommendation for %s has action %q; every volume modification is %q",
				r.Target, r.Action, domain.ActionInPlace)
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("ebs: %w", err)
		}
		applicable = append(applicable, r)
	}
	domain.SortRecommendations(applicable)

	out := make([]domain.Step, 0, len(applicable))
	for _, r := range applicable {
		if g.MaxSteps > 0 && len(out) >= g.MaxSteps {
			break
		}
		out = append(out, domain.Step{
			Seq:    len(out) + 1,
			Key:    domain.StepKey(r.Target, r.Current, r.Proposed),
			Target: r.Target,
			Action: r.Action,
			From:   r.Current,
			To:     r.Proposed,
			Risk:   r.Risk,
			Detail: fmt.Sprintf("modify %s: %s → %s (%s)", r.Target.ID,
				describeSpec(r.Current), describeSpec(r.Proposed), r.Reason),
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// describeSpec renders a volume spec for a human: "gp2 4000 GiB (12000 IOPS,
// 250 MiB/s)".
func describeSpec(s domain.Spec) string {
	var b strings.Builder
	b.WriteString(s.Attr(AttrVolumeType))
	if sz := s.Attr(AttrSizeGiB); sz != "" {
		b.WriteString(" " + sz + " GiB")
	}
	iops, tput := s.Attr(AttrIOPS), s.Attr(AttrThroughputMBps)
	switch {
	case iops != "" && tput != "":
		b.WriteString(" (" + iops + " IOPS, " + tput + " MiB/s)")
	case iops != "":
		b.WriteString(" (" + iops + " IOPS)")
	}
	return b.String()
}

// checkpoint is the persisted form of the learned state.
type checkpoint struct {
	Version int           `json:"version"`
	Scope   string        `json:"scope,omitempty"`
	LastAt  time.Time     `json:"lastAt,omitzero"`
	Learned bool          `json:"learned,omitempty"`
	Volumes []*volume     `json:"volumes,omitempty"`
	Applied []appliedMark `json:"applied,omitempty"`
}

type appliedMark struct {
	VolumeID string    `json:"volumeId"`
	At       time.Time `json:"at"`
}

const checkpointVersion = 1

// Checkpoint implements domain.Domain. Output is deterministic: volumes and
// applied marks are emitted in sorted order, never in map order.
func (d *Domain) Checkpoint() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	c := checkpoint{Version: checkpointVersion, Scope: d.scope, LastAt: d.lastAt, Learned: d.learned}
	ids := make([]string, 0, len(d.volumes))
	for id := range d.volumes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c.Volumes = append(c.Volumes, d.volumes[id])
	}
	aids := make([]string, 0, len(d.applied))
	for id := range d.applied {
		aids = append(aids, id)
	}
	sort.Strings(aids)
	for _, id := range aids {
		c.Applied = append(c.Applied, appliedMark{VolumeID: id, At: d.applied[id]})
	}
	return json.Marshal(c)
}

// Restore implements domain.Domain.
func (d *Domain) Restore(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var c checkpoint
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("ebs: restore: %w", err)
	}
	if c.Version != checkpointVersion {
		return fmt.Errorf("ebs: restore: unknown checkpoint version %d", c.Version)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scope, d.lastAt, d.learned = c.Scope, c.LastAt, c.Learned
	d.volumes = make(map[string]*volume, len(c.Volumes))
	for _, v := range c.Volumes {
		if v == nil || v.Ref.ID == "" {
			continue
		}
		v.Ref.Domain = Kind
		d.volumes[v.Ref.ID] = v
	}
	d.applied = make(map[string]time.Time, len(c.Applied))
	for _, a := range c.Applied {
		if a.VolumeID != "" {
			d.applied[a.VolumeID] = a.At
		}
	}
	return nil
}

// isVolumeTarget reports whether a snapshot target is an EBS volume. A target
// is one if it declares a volume type, or if its ID has the volume prefix —
// the second case keeps a volume whose inventory record was incomplete, so it
// is refused with a stated reason instead of disappearing.
func isVolumeTarget(t domain.Target) bool {
	return t.Spec.Attr(AttrVolumeType) != "" || strings.HasPrefix(t.Ref.ID, volumeIDPrefix)
}

// volumeIDPrefix is the EBS volume ID prefix.
const volumeIDPrefix = "vol-"

// isVolumeSample is [isVolumeTarget] for the metric half of a snapshot: a
// sample is this domain's if it names one of the metrics this domain reads, or
// if its target ID has the volume prefix.
func isVolumeSample(s domain.Sample) bool {
	switch s.Metric {
	case SampleIOPS, SampleThroughputMBps, SampleBurstBalancePct:
		return true
	}
	return strings.HasPrefix(s.Ref.ID, volumeIDPrefix)
}

// hasVolumeEvidence reports whether a snapshot says anything at all about
// volumes.
func hasVolumeEvidence(snap *domain.Snapshot) bool {
	for _, t := range snap.Targets {
		if isVolumeTarget(t) {
			return true
		}
	}
	for _, s := range snap.Samples {
		if isVolumeSample(s) {
			return true
		}
	}
	return false
}

// addressedHere reports whether a snapshot is evidence about THIS domain's
// collector.
//
// [Kind] is shared. The ec2 domain covers instances as well as volumes, and
// more than one collector feeds it: pkg/domain/ec2 composes an instance half
// beside this one, which ships an opaque Payload only that half can decode.
// Read through this package's own one-collector contract, that snapshot is
// indistinguishable from "my collector delivered no volumes", so the domain
// degraded itself on someone else's collection — and whichever snapshot
// arrived last decided its health. The same inputs in a different order
// produced a different health line and a different plan refusal.
// cmd/FINDINGS.md §5.1 is the report; TestSiblingSnapshotDoesNotDecideHealth
// is the reproduction.
//
// The rule: a snapshot that carries content, none of it ours, was not
// addressed to us and says nothing — about our volumes, our freshness or our
// collector's health. A snapshot that carries NOTHING is our own collector
// reporting an empty account. That is a real answer and it still degrades the
// domain, because "we looked and found none" must stay distinguishable from
// "we never looked" (Health separates the two by prose). Collapsing those two
// would be worse than the bug this guards.
//
// It deliberately does not decode Payload: the field is opaque to everything
// but the domain that wrote it, and this domain never writes one.
func addressedHere(snap *domain.Snapshot) bool {
	if hasVolumeEvidence(snap) {
		return true
	}
	return len(snap.Payload) == 0 && len(snap.Targets) == 0 && len(snap.Samples) == 0
}

// modeOf resolves a volume's effective mode from its tags.
func (d *Domain) modeOf(labels map[string]string) string {
	switch m := strings.ToLower(strings.TrimSpace(labels[TagKilterMode])); m {
	case guard.ModeOff, guard.ModeRecommend, guard.ModeApply:
		return m
	}
	return d.cfg.DefaultMode
}

// usageLine renders a volume's storage spend as a commitment usage line.
//
// EBS is covered by no Savings Plan and no Reserved Instance, so the line is
// marked SPIneligible and carries no instance type: it must pass through the
// waterfall to on-demand untouched. It is still handed to the ledger, because
// the ledger's job is to price the WHOLE account before and after — and
// because a future unit that changes an instance and its volume in one plan
// must see both lines.
func (d *Domain) usageLine(ref domain.TargetRef, monthlyUSD float64) commit.UsageLine {
	return commit.UsageLine{
		ID:           "ebs/" + ref.Scope + "/" + ref.ID,
		Kind:         commit.KindEC2,
		Region:       d.cfg.Region,
		Unit:         "volume-hours",
		Quantity:     1,
		ODRate:       monthlyUSD / HoursPerMonth,
		SPIneligible: true,
	}
}

// riskOf grades the change. A gp2 → gp3 conversion at or above parity is
// online and reversible, which is low risk; a conversion that provisions below
// what gp2 delivered is a real performance decision and is graded medium even
// though the mechanics are identical.
func riskOf(p ParityPlan) string {
	if p.Floor == FloorGP2Baseline {
		return plan.RiskLow
	}
	if float64(p.Config.IOPS) < float64(p.GP2.BaselineIOPS) ||
		float64(p.Config.ThroughputMBps) < p.GP2.BaselineThroughputMBps {
		return plan.RiskMedium
	}
	return plan.RiskLow
}

// sizeOf reads the volume size from a spec.
func sizeOf(s domain.Spec) int64 {
	n, err := strconv.ParseInt(s.Attr(AttrSizeGiB), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func intAttr(s domain.Spec, key string) int32 {
	n, err := strconv.ParseInt(s.Attr(key), 10, 32)
	if err != nil {
		return 0
	}
	return int32(n)
}

var _ domain.Domain = (*Domain)(nil)
