package fargate

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
	"github.com/agenticode/kilter/pkg/safety"
)

// Kind is the domain this package implements.
const Kind = domain.K8sFargate

// Config tunes the k8s-fargate domain. The zero value is not usable; start from
// [DefaultConfig].
//
// Every shave-related default is set on the assumption that a wrong shave costs
// far more than a missed one: the pod OOMs, the workload degrades, and the
// engine's credibility goes with it.
type Config struct {
	// Scope identifies the cluster (TargetRef.Scope). Snapshots override it.
	Scope string
	// Region labels commitment usage lines; Compute Savings Plans are
	// account-wide, so this is documentation rather than a matching key for
	// Fargate. "" is legal.
	Region string
	// Rates prices tiers. Zero value ⇒ pricing.DefaultFargateRates().
	Rates pricing.FargateRates
	// Recommend is the container sizing policy, reused verbatim from
	// pkg/recommend. Zero value ⇒ recommend.DefaultConfig().
	Recommend recommend.Config

	// ActuationAvailable reports whether an actuator is wired for this domain.
	// FALSE BY DEFAULT: a domain constructed without explicit actuation is
	// report-only, so forgetting to wire credentials can never be mistaken for
	// permission to act.
	ActuationAvailable bool
	// DefaultMode is the kilter.dev/mode assumed for workloads that declare
	// none: off | recommend | apply. Empty ⇒ guard.ModeApply, matching
	// pkg/plan — Kilter's mode annotation is opt-OUT, and having two defaults
	// for one annotation would be worse than either choice. Note that mode
	// only decides whether a recommendation is applicable; ActuationAvailable
	// decides whether this domain may produce steps at all.
	DefaultMode string
	// StaleAfter is how old the newest learned snapshot may be before the
	// domain degrades to report-only. Default 15m.
	StaleAfter time.Duration

	// MinShaveConfidence gates the boundary shave. Default 0.80.
	MinShaveConfidence float64
	// MinShaveWindow is the observation span a shave needs. Default 24h.
	MinShaveWindow time.Duration
	// MinShaveSamples is the sample count a shave needs. Default 120.
	MinShaveSamples int
	// NoiseBandFraction is the minimum headroom above the observed memory peak
	// a shaved request must keep, as a fraction of the peak. Default 0.10.
	NoiseBandFraction float64
	// NoiseSigmas widens that band for volatile workloads: the band is
	// max(NoiseBandFraction×peak, NoiseSigmas×stddev). Default 3.
	NoiseSigmas float64
	// MinShaveMonthlyUSD is the smallest saving worth the risk of a shave.
	// Default $1.00/month.
	MinShaveMonthlyUSD float64
	// MinMoveMonthlyUSD is the smallest saving worth a rolling restart for a
	// tier move. Default $0.10/month. (A tier move is the sizing policy's own
	// conclusion, so the bar is lower than for a shave.)
	MinMoveMonthlyUSD float64
	// MaxSampleAge is the freshness horizon in the shave confidence score.
	// Default 2h.
	MaxSampleAge time.Duration

	// RegressionWindow is how long a change is watched for OOMs/crashloops.
	// Default 1h.
	RegressionWindow time.Duration
	// QuarantineFor is how long a regressed workload is left alone.
	// Default 24h.
	QuarantineFor time.Duration
	// Floors tunes how the observed memory peak relaxes over time; reused from
	// pkg/decision so a peak and an OOM floor age the same way.
	Floors decision.FloorConfig
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		Rates:              pricing.DefaultFargateRates(),
		Recommend:          recommend.DefaultConfig(),
		DefaultMode:        guard.ModeApply,
		StaleAfter:         15 * time.Minute,
		MinShaveConfidence: 0.80,
		MinShaveWindow:     24 * time.Hour,
		MinShaveSamples:    120,
		NoiseBandFraction:  0.10,
		NoiseSigmas:        3,
		MinShaveMonthlyUSD: 1.00,
		MinMoveMonthlyUSD:  0.10,
		MaxSampleAge:       2 * time.Hour,
		RegressionWindow:   time.Hour,
		QuarantineFor:      24 * time.Hour,
		Floors:             decision.DefaultFloorConfig(),
	}
}

// withDefaults substitutes documented defaults for unset or out-of-range
// fields. A garbage config must yield the conservative default, never a
// disabled guard — the same rule pkg/guard and pkg/decision follow.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if !c.Rates.Platform.Valid() {
		c.Rates = d.Rates
	}
	if c.Recommend == (recommend.Config{}) {
		c.Recommend = d.Recommend
	}
	if c.DefaultMode != guard.ModeOff && c.DefaultMode != guard.ModeRecommend &&
		c.DefaultMode != guard.ModeApply {
		c.DefaultMode = d.DefaultMode
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = d.StaleAfter
	}
	if !(c.MinShaveConfidence > 0) || c.MinShaveConfidence > 1 { // catches NaN
		c.MinShaveConfidence = d.MinShaveConfidence
	}
	if c.MinShaveWindow <= 0 {
		c.MinShaveWindow = d.MinShaveWindow
	}
	if c.MinShaveSamples <= 0 {
		c.MinShaveSamples = d.MinShaveSamples
	}
	if !(c.NoiseBandFraction >= 0) || c.NoiseBandFraction > 10 {
		c.NoiseBandFraction = d.NoiseBandFraction
	}
	if !(c.NoiseSigmas >= 0) || c.NoiseSigmas > 100 {
		c.NoiseSigmas = d.NoiseSigmas
	}
	if !(c.MinShaveMonthlyUSD >= 0) {
		c.MinShaveMonthlyUSD = d.MinShaveMonthlyUSD
	}
	if !(c.MinMoveMonthlyUSD >= 0) {
		c.MinMoveMonthlyUSD = d.MinMoveMonthlyUSD
	}
	if c.MaxSampleAge <= 0 {
		c.MaxSampleAge = d.MaxSampleAge
	}
	if c.RegressionWindow <= 0 {
		c.RegressionWindow = d.RegressionWindow
	}
	if c.QuarantineFor <= 0 {
		c.QuarantineFor = d.QuarantineFor
	}
	return c
}

// stat is the learned memory behaviour of one container, aggregated across the
// workload's replicas exactly as pkg/recommend aggregates its histograms.
//
// pkg/recommend keeps richer state, but deliberately does not export the two
// numbers a boundary shave turns on — the observed peak and its dispersion —
// because its own policy applies headroom before anyone sees them. A shave has
// no policy headroom to hide behind, so it measures them here.
type stat struct {
	Samples int64     `json:"samples"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	// PeakBytes is the largest memory sample ever observed, and PeakAt when.
	PeakBytes int64     `json:"peakBytes"`
	PeakAt    time.Time `json:"peakAt"`
	// LastBytes is the newest sample: the level the peak relaxes toward.
	LastBytes int64 `json:"lastBytes"`
	// Mean and M2 are Welford accumulators over memory samples. Welford rather
	// than Σx/Σx² because byte-scale squares (1e18 and up) lose precision fast.
	Mean float64 `json:"mean"`
	M2   float64 `json:"m2"`
	// OOMSeen records that this container was observed OOM-killed at least
	// once, ever. It never decays: an OOM is permanent evidence that this
	// container's memory demand is not fully captured by its samples, which is
	// exactly the condition under which a shave must not fire.
	OOMSeen bool      `json:"oomSeen,omitempty"`
	LastOOM time.Time `json:"lastOOM,omitzero"`
}

// observe folds one usage sample in. Garbage is dropped rather than clamped,
// matching pkg/recommend: a collector emitting negative usage or a zero
// timestamp is broken, not reporting a low value, and clamping would drag the
// peak down — the one direction a memory guard must never fail.
func (s *stat) observe(mem int64, ts time.Time) {
	if mem < 0 || ts.IsZero() {
		return
	}
	s.Samples++
	d := float64(mem) - s.Mean
	s.Mean += d / float64(s.Samples)
	s.M2 += d * (float64(mem) - s.Mean)
	// PeakAt is the LATEST time the peak level was reached, not the first time
	// it was ingested. Ties must not be broken by arrival order — the same
	// samples delivered in a different order would otherwise age the peak
	// differently — and taking the latest is also the conservative direction:
	// the peak holds its full strength for longer.
	if mem > s.PeakBytes {
		s.PeakBytes, s.PeakAt = mem, ts
	} else if mem == s.PeakBytes && ts.After(s.PeakAt) {
		s.PeakAt = ts
	}
	if s.First.IsZero() || ts.Before(s.First) {
		s.First = ts
	}
	if ts.After(s.Last) || s.Last.IsZero() {
		s.Last = ts
		s.LastBytes = mem
	}
}

// window is the observation span.
func (s *stat) window() time.Duration {
	if s.First.IsZero() || s.Last.Before(s.First) {
		return 0
	}
	return s.Last.Sub(s.First)
}

// stddev is the sample standard deviation of observed memory, 0 below two
// samples.
func (s *stat) stddev() float64 {
	if s.Samples < 2 || !(s.M2 > 0) {
		return 0
	}
	v := s.M2 / float64(s.Samples-1)
	if !(v > 0) || math.IsInf(v, 0) {
		return 0
	}
	return math.Sqrt(v)
}

// effectivePeak is the observed peak as of now, relaxed with age.
//
// It reuses decision.EffectiveOOMFloor because the semantics are identical: a
// high-water mark that must hold at full strength for a while, then decay its
// excess over the recent level geometrically, and never fall below what is
// currently observed. Sharing the implementation means a peak and an OOM floor
// cannot drift apart in behaviour.
func (s *stat) effectivePeak(now time.Time, cfg decision.FloorConfig) int64 {
	if s == nil || s.PeakBytes <= 0 {
		return 0
	}
	return decision.EffectiveOOMFloor(s.PeakBytes, s.PeakAt, now, s.LastBytes, cfg)
}

// appliedChange is a change this domain planned and the controller executed,
// kept so a post-change regression can be reverted to the exact prior spec.
type appliedChange struct {
	Target domain.TargetRef `json:"target"`
	From   domain.Spec      `json:"from"`
	To     domain.Spec      `json:"to"`
	At     time.Time        `json:"at"`
	// Set only once a regression has been attributed to this change.
	RegressionReason string    `json:"regressionReason,omitempty"`
	DetectedAt       time.Time `json:"detectedAt,omitzero"`
}

// Domain is the k8s-fargate compute domain. Safe for concurrent use; pure —
// no I/O, no clock.
type Domain struct {
	mu  sync.Mutex
	cfg Config

	rec     *recommend.Recommender
	regress *safety.RegressionDetector

	stats   map[model.ContainerKey]*stat
	applied map[model.WorkloadRef]appliedChange
	// reverts holds revert recommendations produced by a detected regression.
	// safety.RegressionDetector reports each regression exactly once, so the
	// verdict is parked here and re-emitted every Recommend until the
	// controller records that it acted — otherwise a second Recommend call at
	// the same instant would silently lose the revert.
	reverts map[model.WorkloadRef]appliedChange

	scope       string
	last        *model.ClusterSnapshot // Fargate-only projection of the cluster
	lastAt      time.Time
	targets     int
	stale       bool
	staleReason string
}

// New builds a k8s-fargate domain.
func New(cfg Config) (*Domain, error) {
	cfg = cfg.withDefaults()
	r, err := recommend.New(cfg.Recommend)
	if err != nil {
		return nil, fmt.Errorf("fargate: %w", err)
	}
	return &Domain{
		cfg:     cfg,
		rec:     r,
		regress: safety.NewRegressionDetector(cfg.RegressionWindow, cfg.QuarantineFor),
		stats:   map[model.ContainerKey]*stat{},
		applied: map[model.WorkloadRef]appliedChange{},
		reverts: map[model.WorkloadRef]appliedChange{},
		scope:   cfg.Scope,
	}, nil
}

// Kind implements domain.Domain.
func (d *Domain) Kind() domain.Kind { return Kind }

// Learn folds a snapshot into the domain's learned state.
//
// Failure policy: a snapshot that is absent, empty, or missing its cluster
// payload degrades the domain (Health goes report-only) and returns nil. Only a
// snapshot addressed to the wrong domain is an error, because that is a wiring
// bug rather than an operational condition. "A collector failure yields a
// stale-marked domain, never a broken brain."
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
	if snap.Cluster == nil {
		d.stale = true
		if d.staleReason == "" {
			d.staleReason = "collector delivered no cluster snapshot"
		}
		return nil
	}
	if d.scope == "" {
		d.scope = snap.Cluster.ClusterID
	}

	fs := fargateOnly(snap.Cluster)
	d.rec.ObserveSnapshot(fs)

	for i := range fs.Pods {
		pod := &fs.Pods[i]
		for _, c := range pod.Containers {
			if !c.LastOOMKilled {
				continue
			}
			// An OOM anywhere in this workload's replicas disqualifies the
			// container template from being shaved. The kill is attributed to
			// the snapshot timestamp; exact attribution is pkg/recommend's job
			// (it owns the restart-delta bookkeeping), and this flag only ever
			// makes the domain more reluctant.
			st := d.stat(model.ContainerKey{Workload: pod.Workload, Container: c.Name})
			st.OOMSeen = true
			if fs.Timestamp.After(st.LastOOM) {
				st.LastOOM = fs.Timestamp
			}
		}
	}
	for _, u := range fs.Usage {
		d.stat(u.Key).observe(u.MemoryBytes, u.Timestamp)
	}

	workloads := map[model.WorkloadRef]bool{}
	for i := range fs.Pods {
		workloads[fs.Pods[i].Workload] = true
	}
	d.targets = len(workloads)
	d.last = fs
	at := fs.Timestamp
	if at.IsZero() {
		at = snap.Timestamp
	}
	if at.After(d.lastAt) {
		d.lastAt = at
	}
	return nil
}

// stat returns (creating if needed) the learned state for a container key.
// Caller holds d.mu.
func (d *Domain) stat(k model.ContainerKey) *stat {
	s := d.stats[k]
	if s == nil {
		s = &stat{}
		d.stats[k] = s
	}
	return s
}

// fargateOnly projects the Fargate half of a cluster snapshot: only pods on
// Fargate VMs, only their workloads, only their usage. Nodes are dropped
// outright — a Fargate "node" is a single-pod VM whose shape is not the bill,
// and nothing downstream of here may see one.
//
// Maps are used for membership only; every output slice is built by walking an
// input slice, so the projection is deterministic.
func fargateOnly(s *model.ClusterSnapshot) *model.ClusterSnapshot {
	_, fargatePods := pricing.SplitFargate(s)

	out := *s
	out.Nodes = nil
	out.Pods = make([]model.PodSpec, 0, len(fargatePods))
	keys := make(map[model.ContainerKey]bool, len(fargatePods))
	wl := make(map[model.WorkloadRef]bool, len(fargatePods))
	for _, fp := range fargatePods {
		out.Pods = append(out.Pods, fp.Pod)
		wl[fp.Pod.Workload] = true
		for _, c := range fp.Pod.Containers {
			keys[model.ContainerKey{Workload: fp.Pod.Workload, Container: c.Name}] = true
		}
	}
	out.Usage = make([]model.Usage, 0, len(s.Usage))
	for _, u := range s.Usage {
		if keys[u.Key] {
			out.Usage = append(out.Usage, u)
		}
	}
	out.Workloads = make([]model.WorkloadInfo, 0, len(s.Workloads))
	for _, w := range s.Workloads {
		if wl[w.Ref] {
			out.Workloads = append(out.Workloads, w)
		}
	}
	return &out
}

// Health implements domain.Domain.
func (d *Domain) Health(now time.Time) domain.Health {
	d.mu.Lock()
	defer d.mu.Unlock()

	h := domain.Health{Kind: Kind, LastSnapshot: d.lastAt, Targets: d.targets}
	switch {
	case d.last == nil:
		h.Reason = "no cluster snapshot learned yet: the Kubernetes collector is absent or has not reported"
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
		h.Reason = "actuation is not wired for this domain (no Kubernetes client): recommendations only"
	default:
		h.Ready = true
	}
	h.ReportOnly = !h.Ready || !d.cfg.ActuationAvailable
	return h
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
			return nil, fmt.Errorf("fargate: recommendation for %q handed to the k8s-fargate domain", r.Target.Domain)
		}
		// Rule with teeth: an in-place resize is impossible on Fargate, and a
		// step that claimed one would understate disruption to the executor's
		// eviction budget and PDB accounting. Refuse the plan rather than
		// emit it.
		if r.Action != domain.ActionRolling {
			return nil, fmt.Errorf("fargate: recommendation for %s has action %q; every Fargate resize is %q",
				r.Target, r.Action, domain.ActionRolling)
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
			Detail: fmt.Sprintf("%s: %s → %s (%s)", r.Proposed.Attr(AttrChange),
				r.Current.Attr(AttrTier), r.Proposed.Attr(AttrTier), r.Reason),
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// RecordApplied tells the domain that a step was executed. It arms the
// post-change regression watch against the health of the workload as it stands
// right now, and remembers the exact prior spec so a regression can be reverted
// to it rather than to a guess.
//
// Calling it is not optional: without it a bad resize is never detected, so the
// controller must call it for every step it completes.
func (d *Domain) RecordApplied(step domain.Step, now time.Time) error {
	ref, err := parseTargetID(step.Target.ID)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		return fmt.Errorf("fargate: RecordApplied(%s) before any snapshot was learned", step.Target)
	}
	d.regress.RecordChange(ref, d.last, now)
	d.applied[ref] = appliedChange{Target: step.Target, From: step.From, To: step.To, At: now}
	// Acting on the workload consumes any pending revert for it: either this
	// step *is* the revert, or it supersedes it. Either way the watch above is
	// re-armed on the new state.
	delete(d.reverts, ref)
	return nil
}

// Quarantined reports whether a workload is currently held back after a
// post-change regression.
func (d *Domain) Quarantined(ref model.WorkloadRef, now time.Time) bool {
	return d.regress.Quarantined(ref, now)
}

// checkpoint is the persisted form of the domain's learned state.
type checkpoint struct {
	Version     int                         `json:"version"`
	Scope       string                      `json:"scope,omitempty"`
	Stats       []statEntry                 `json:"stats,omitempty"`
	Recommender []recommend.CheckpointState `json:"recommender,omitempty"`
	Applied     []appliedEntry              `json:"applied,omitempty"`
	Reverts     []appliedEntry              `json:"reverts,omitempty"`
}

type statEntry struct {
	Key  model.ContainerKey `json:"key"`
	Stat stat               `json:"stat"`
}

type appliedEntry struct {
	Ref    model.WorkloadRef `json:"ref"`
	Change appliedChange     `json:"change"`
}

const checkpointVersion = 1

// Checkpoint serializes learned state. Output is deterministic: every map is
// emitted as a slice sorted by key, so two checkpoints of identical state are
// byte-identical and a store can compare them.
//
// The observed cluster snapshot is deliberately NOT persisted. After a restart
// the domain is report-only until a collector feeds it again — restored
// learning must never be mistaken for a live view.
func (d *Domain) Checkpoint() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cp := checkpoint{Version: checkpointVersion, Scope: d.scope, Recommender: d.rec.Checkpoint()}
	// pkg/recommend hands its checkpoint back in map order; a store that
	// compares bytes to decide whether to write needs a stable one.
	sort.Slice(cp.Recommender, func(i, j int) bool {
		return cp.Recommender[i].Key.String() < cp.Recommender[j].Key.String()
	})
	for k, s := range d.stats {
		cp.Stats = append(cp.Stats, statEntry{Key: k, Stat: *s})
	}
	sort.Slice(cp.Stats, func(i, j int) bool { return cp.Stats[i].Key.String() < cp.Stats[j].Key.String() })
	cp.Applied = sortedApplied(d.applied)
	cp.Reverts = sortedApplied(d.reverts)
	return json.Marshal(cp)
}

func sortedApplied(m map[model.WorkloadRef]appliedChange) []appliedEntry {
	if len(m) == 0 {
		return nil
	}
	out := make([]appliedEntry, 0, len(m))
	for ref, c := range m {
		out = append(out, appliedEntry{Ref: ref, Change: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.String() < out[j].Ref.String() })
	return out
}

// Restore reloads learned state. Unknown or future versions are rejected rather
// than half-applied.
func (d *Domain) Restore(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var cp checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return fmt.Errorf("fargate: restore: %w", err)
	}
	if cp.Version != checkpointVersion {
		return fmt.Errorf("fargate: restore: unsupported checkpoint version %d", cp.Version)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cp.Scope != "" {
		d.scope = cp.Scope
	}
	d.stats = make(map[model.ContainerKey]*stat, len(cp.Stats))
	for i := range cp.Stats {
		s := cp.Stats[i].Stat
		d.stats[cp.Stats[i].Key] = &s
	}
	d.applied = make(map[model.WorkloadRef]appliedChange, len(cp.Applied))
	for _, e := range cp.Applied {
		d.applied[e.Ref] = e.Change
	}
	d.reverts = make(map[model.WorkloadRef]appliedChange, len(cp.Reverts))
	for _, e := range cp.Reverts {
		d.reverts[e.Ref] = e.Change
	}
	d.rec.Restore(cp.Recommender)
	return nil
}

// compile-time proof that the domain satisfies the seam.
var _ domain.Domain = (*Domain)(nil)
