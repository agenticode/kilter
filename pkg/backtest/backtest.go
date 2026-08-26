// Package backtest replays stored cluster history through Kilter's exact
// production decision path and scores what the engine would have decided
// against what actually happened next.
//
// Design reference: docs/design/reasoning-engine.md §4.4 (implementation-plan
// unit 2). This package is the project's falsifiability instrument: every
// other unit claims the engine is good, and this one turns that claim into a
// number computed from the operator's own history. It is also the gate every
// later policy change — human, closed-loop tuner, or LLM-proposed — must pass
// before it is allowed anywhere near a cluster.
//
// Four properties are load-bearing, and each has tests that fail loudly if it
// is broken:
//
//   - Same code path. The harness drives recommend.Recommender and plan.Build
//     exactly as pkg/api's brain does — observe every snapshot in order, then
//     ask on the snapshot at the decision instant. A parallel reimplementation
//     would score a policy that never runs.
//   - Independent oracle. The reference sizing is computed from future usage
//     alone (see oracle.go). Nothing in recommend.Config, plan.Config or
//     decision.Config can reach it, and the set of scored pairs is fixed
//     before any policy runs.
//   - Refusal is scored, not ignored. A non-action is an outcome: it leaves
//     the current sizing in force and is charged for it on exactly the same
//     terms as a resize. Refusing everything therefore cannot win.
//   - Determinism. Same history plus same policy yields a byte-identical
//     scorecard. Every enumeration is sorted, every float sum is over a
//     canonically ordered multiset, and no code path here calls time.Now():
//     time comes from the replayed history.
//
// Dependency direction stays downward (§8): this package imports evidence,
// recommend, plan, decision, pricing and model, and nothing imports it yet.
package backtest

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/patterns"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

// SnapshotSource yields the cluster topology-and-usage history the harness
// replays. It is a separate seam from evidence.Store on purpose: the
// substrate stores per-subject usage series and events, but not the pod and
// node topology that recommend and plan both take as input, and reconstructing
// topology from digests would be a parallel reimplementation of exactly the
// thing this package must not reimplement. See FINDINGS.md for the
// snapshot-history persistence this seam is waiting on.
//
// Implementations must return every snapshot for the cluster whose Timestamp
// lies in [from, to). Order is not required — the harness sorts — but
// timestamps must be unique per cluster, since a time series with two values
// at one instant has no defined replay order.
type SnapshotSource interface {
	Snapshots(cluster string, from, to time.Time) ([]*model.ClusterSnapshot, error)
}

// SliceSource adapts an in-memory slice of snapshots to SnapshotSource:
// the form synthetic traces, recorded fixtures and tests use.
type SliceSource []*model.ClusterSnapshot

// Snapshots implements SnapshotSource, filtering by cluster and window.
func (s SliceSource) Snapshots(cluster string, from, to time.Time) ([]*model.ClusterSnapshot, error) {
	out := make([]*model.ClusterSnapshot, 0, len(s))
	for _, snap := range s {
		if snap == nil || snap.ClusterID != cluster {
			continue
		}
		ts := snap.Timestamp.UTC()
		if ts.Before(from) || !ts.Before(to) {
			continue
		}
		out = append(out, snap)
	}
	return out, nil
}

// Config holds the scoring knobs — how the harness measures, as opposed to
// what it measures. They are deliberately not part of PolicyHash, and they
// are echoed into every Scorecard so two scorecards can never be compared
// under different yardsticks without it being visible.
type Config struct {
	// DecisionInterval is the spacing of decision instants. Default 24h,
	// which makes the scoring unit the container-day the oracle is defined
	// on. Smaller intervals sample the policy more finely at linear cost.
	DecisionInterval time.Duration
	// StarvationFactor scales the CPU request in the starvation predicate:
	// a violation is future p95 > request × factor. Default 1.0 — the
	// request must cover the future p95. Values above 1 are more permissive
	// (burst is tolerated); values below 1 demand explicit CPU headroom.
	StarvationFactor float64
	// FlipWindow is how far back a resize looks for the previous resize it
	// might be reversing. Default 7 days, per §4.4.
	FlipWindow time.Duration
	// Cost prices resources and risk. Zero fields take DefaultCostModel's.
	Cost CostModel
}

// DefaultConfig returns the scoring defaults.
func DefaultConfig() Config {
	return Config{
		DecisionInterval: 24 * time.Hour,
		StarvationFactor: 1.0,
		FlipWindow:       7 * 24 * time.Hour,
		Cost:             DefaultCostModel(),
	}
}

// maxDecisionInterval bounds the grid spacing. A year between decisions is
// already absurd, and the bound keeps grid arithmetic far from overflow.
const maxDecisionInterval = 365 * 24 * time.Hour

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.DecisionInterval == 0 {
		c.DecisionInterval = d.DecisionInterval
	}
	if c.StarvationFactor == 0 {
		c.StarvationFactor = d.StarvationFactor
	}
	if c.FlipWindow == 0 {
		c.FlipWindow = d.FlipWindow
	}
	c.Cost = c.Cost.withDefaults()
	return c
}

// Validate rejects scoring config that would make the numbers meaningless.
// Positive form throughout so NaN fails rather than disabling a predicate.
func (c Config) Validate() error {
	if c.DecisionInterval <= 0 || c.DecisionInterval > maxDecisionInterval {
		return fmt.Errorf("backtest: DecisionInterval %v out of (0,%v]", c.DecisionInterval, maxDecisionInterval)
	}
	if !(c.StarvationFactor > 0) || math.IsInf(c.StarvationFactor, 0) {
		return fmt.Errorf("backtest: StarvationFactor %v must be finite and > 0", c.StarvationFactor)
	}
	if c.FlipWindow < 0 {
		return fmt.Errorf("backtest: FlipWindow %v must be >= 0", c.FlipWindow)
	}
	return c.Cost.Validate()
}

// Harness replays one cluster's history against one policy.
//
// Evidence, Catalog and Scoring may be left zero; the policy triple takes
// package defaults when zero, so the zero Harness plus a History scores the
// shipped default policy.
type Harness struct {
	// Evidence is the substrate the harness reads adverse events and
	// change events from. Optional: with a nil store the harness scores
	// purely from usage, and every event-derived signal reads as absent.
	Evidence evidence.Store
	// History is the snapshot stream to replay. Required.
	History SnapshotSource

	// Rec, Plan and Decision are the policy under test — the exact configs
	// the brain would run with.
	Rec      recommend.Config
	Plan     plan.Config
	Decision decision.Config

	// EnforceDecisionRefusals runs pkg/decision's refusal predicates over
	// each container before accepting a recommendation, suppressing the
	// resize when one fires.
	//
	// It is off by default because it is not yet what production does:
	// pkg/decision shipped as unit 3, but pkg/recommend does not import it,
	// so today a refusal predicate cannot stop a recommendation from being
	// planned. Turning this on models exactly that pending wiring — using the
	// shipped predicates, not a copy of them — so the question "is wiring the
	// decision layer into the recommender an improvement?" becomes an A/B
	// scorecard through Gate instead of an opinion. See FINDINGS.md.
	EnforceDecisionRefusals bool

	// Catalog prices snapshots inside plan.Build. Nil uses the embedded
	// baseline catalog, matching a brain started without a pricing file.
	Catalog *pricing.Catalog
	// Scoring holds the measurement knobs (not policy).
	Scoring Config
}

// ErrDuplicateSnapshot rejects two snapshots for one cluster at one instant:
// the replay order of a time series with a tie is undefined, and an undefined
// replay order is a nondeterministic scorecard.
var ErrDuplicateSnapshot = errors.New("backtest: duplicate snapshot timestamp for cluster")

// Run replays [from, to) in snapshot order. At each decision instant t it
//
//  1. has already fed the recommender every snapshot up to and including the
//     one at t — the identical observe-then-ask sequence pkg/api's brain runs;
//  2. asks the engine, with the policy under test, for recommendations and a
//     plan against that snapshot;
//  3. scores the outcome for every eligible container against what the
//     evidence says happened in (t, t+horizon].
//
// Decision instants sit on the grid from, from+interval, … and are snapped
// forward to the first snapshot at or after each grid point. A grid point
// whose scoring window would run past `to` is skipped and counted in
// Skipped.NoHorizon rather than scored against a truncated future: partial
// windows would bias every metric toward whatever the surviving prefix did.
//
// Run never looks at a snapshot later than the decision instant when asking
// the engine; the future is read only by the scorer.
func (h *Harness) Run(cluster string, from, to time.Time, horizon time.Duration) (*Scorecard, error) {
	return h.run(cluster, from, to, horizon, nil)
}

// run is Run with an optional out-parameter for the per-decision ledger.
func (h *Harness) run(cluster string, from, to time.Time, horizon time.Duration, ledger *[]record) (*Scorecard, error) {
	if cluster == "" {
		return nil, errors.New("backtest: cluster must be named")
	}
	if h.History == nil {
		return nil, errors.New("backtest: no snapshot history to replay")
	}
	from, to = from.UTC(), to.UTC()
	if from.IsZero() || to.IsZero() {
		return nil, errors.New("backtest: replay window needs explicit bounds")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("backtest: replay window [%s, %s) is empty or inverted",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if horizon <= 0 {
		return nil, fmt.Errorf("backtest: horizon %v must be > 0", horizon)
	}
	if horizon > to.Sub(from) {
		return nil, fmt.Errorf("backtest: horizon %v exceeds the replay window %v", horizon, to.Sub(from))
	}

	scoring := h.Scoring.withDefaults()
	if err := scoring.Validate(); err != nil {
		return nil, err
	}
	recCfg := h.Rec
	if recCfg == (recommend.Config{}) {
		recCfg = recommend.DefaultConfig()
	}
	planCfg := h.Plan
	if planCfg == (plan.Config{}) {
		planCfg = plan.DefaultConfig()
	}
	decCfg := h.Decision
	if decCfg == (decision.Config{}) {
		decCfg = decision.DefaultConfig()
	}
	// recommend.New is the policy's own validator; using it (rather than a
	// copy of its rules) means an invalid policy fails here for exactly the
	// reason it would fail at brain start.
	rec, err := recommend.New(recCfg)
	if err != nil {
		return nil, fmt.Errorf("backtest: policy under test is invalid: %w", err)
	}
	if err := decCfg.Validate(); err != nil {
		return nil, fmt.Errorf("backtest: policy under test is invalid: %w", err)
	}
	catalog := h.Catalog
	if catalog == nil {
		catalog = pricing.Embedded()
	}

	snaps, err := h.orderedSnapshots(cluster, from, to)
	if err != nil {
		return nil, err
	}

	sc := &Scorecard{
		Policy:                PolicyHash(recCfg, planCfg, decCfg),
		Cluster:               cluster,
		Window:                [2]time.Time{from, to},
		HorizonHours:          round6(horizon.Hours()),
		DecisionIntervalHours: round6(scoring.DecisionInterval.Hours()),
		StarvationFactor:      round6(scoring.StarvationFactor),
		Snapshots:             len(snaps),
	}
	if len(snaps) == 0 {
		sc.Refusals = map[string]int{}
		sc.Cost = scoring.Cost
		return sc, nil
	}

	future := newFutureIndex(snaps)
	instants, skipped := decisionInstants(snaps, from, to, horizon, scoring.DecisionInterval)
	sc.Skipped = skipped
	sc.Instants = len(instants)

	// atInstant[i] is true when snapshot i is a decision instant. Built as a
	// set so the replay stays a single ordered pass.
	atInstant := make(map[int]bool, len(instants))
	for _, i := range instants {
		atInstant[i] = true
	}

	st := &replayState{
		harness:  h,
		cluster:  cluster,
		rec:      rec,
		planCfg:  planCfg,
		decCfg:   decCfg,
		catalog:  catalog,
		scoring:  scoring,
		horizon:  horizon,
		future:   future,
		learned:  map[model.ContainerKey]*learnState{},
		skipped:  &sc.Skipped,
		lastSeen: map[model.ContainerKey]patterns.Class{},
	}

	var records []record
	for i, snap := range snaps {
		// Observe first, ask second — the brain's Ingest/Plan order. The
		// recommender therefore knows the snapshot at t and nothing after it.
		rec.ObserveSnapshot(snap)
		st.observe(snap)
		if atInstant[i] {
			records = append(records, st.decide(snap)...)
		}
	}

	if ledger != nil {
		*ledger = records
	}
	out := score(records, scoring.Cost, horizon, scoring.FlipWindow)
	// Carry the replay metadata across; score() owns only the aggregates.
	out.Policy, out.Cluster, out.Window = sc.Policy, sc.Cluster, sc.Window
	out.HorizonHours, out.DecisionIntervalHours = sc.HorizonHours, sc.DecisionIntervalHours
	out.StarvationFactor, out.Snapshots, out.Instants = sc.StarvationFactor, sc.Snapshots, sc.Instants
	out.Skipped = sc.Skipped
	return out, nil
}

// orderedSnapshots fetches, filters and totally orders the replay input.
func (h *Harness) orderedSnapshots(cluster string, from, to time.Time) ([]*model.ClusterSnapshot, error) {
	raw, err := h.History.Snapshots(cluster, from, to)
	if err != nil {
		return nil, fmt.Errorf("backtest: snapshot history: %w", err)
	}
	out := make([]*model.ClusterSnapshot, 0, len(raw))
	for _, snap := range raw {
		if snap == nil || snap.ClusterID != cluster || snap.Timestamp.IsZero() {
			continue
		}
		ts := snap.Timestamp.UTC()
		if ts.Before(from) || !ts.Before(to) {
			continue
		}
		out = append(out, snap)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.UTC().Before(out[j].Timestamp.UTC())
	})
	for i := 1; i < len(out); i++ {
		if out[i].Timestamp.UTC().Equal(out[i-1].Timestamp.UTC()) {
			return nil, fmt.Errorf("%w: %s at %s", ErrDuplicateSnapshot, cluster,
				out[i].Timestamp.UTC().Format(time.RFC3339Nano))
		}
	}
	return out, nil
}

// decisionInstants maps the decision grid onto snapshot indices. A grid point
// resolves to the first snapshot at or after it whose full scoring window
// still fits inside [from, to). Duplicates (a grid finer than the snapshot
// cadence) collapse to one instant.
func decisionInstants(snaps []*model.ClusterSnapshot, from, to time.Time,
	horizon, interval time.Duration) ([]int, SkipCounts) {

	var (
		out     []int
		skipped SkipCounts
		last    = -1
		search  int
	)
	for t := from; !t.After(to); t = t.Add(interval) {
		if t.Add(horizon).After(to) {
			skipped.NoHorizon++
			continue
		}
		// Snapshots are sorted, and t only moves forward, so the scan
		// resumes where the previous grid point left off: one pass total.
		for search < len(snaps) && snaps[search].Timestamp.UTC().Before(t) {
			search++
		}
		if search >= len(snaps) {
			skipped.NoSnapshot++
			continue
		}
		if snaps[search].Timestamp.UTC().Add(horizon).After(to) {
			// The snapped instant drifted far enough forward that its window
			// no longer fits, even though the grid point's did.
			skipped.NoHorizon++
			continue
		}
		if search != last {
			out = append(out, search)
			last = search
		}
	}
	return out, skipped
}

// learnState mirrors the history bookkeeping the recommender keeps privately,
// so the harness can fill decision.Evidence without reaching into it. It
// counts and timestamps only — no percentiles, no policy — which is why it is
// bookkeeping rather than a parallel implementation. FINDINGS.md records the
// Recommender.Verdicts seam that would remove it.
type learnState struct {
	samples     int
	first, last time.Time
}

// replayState carries everything the per-instant scoring needs.
type replayState struct {
	harness *Harness
	cluster string
	rec     *recommend.Recommender
	planCfg plan.Config
	decCfg  decision.Config
	catalog *pricing.Catalog
	scoring Config
	horizon time.Duration
	future  *futureIndex
	learned map[model.ContainerKey]*learnState
	skipped *SkipCounts
	// lastSeen remembers the last class the recommender reported for a
	// container, so a refusal evaluated while the recommender is silent can
	// still scale its soak by class instead of defaulting to steady.
	lastSeen map[model.ContainerKey]patterns.Class
}

// observe mirrors the recommender's sample-acceptance guards so the history
// counters used for the insufficient-history predicate match what the
// recommender actually learned from.
func (s *replayState) observe(snap *model.ClusterSnapshot) {
	for _, u := range snap.Usage {
		if u.MilliCPU < 0 || u.MemoryBytes < 0 || u.Timestamp.IsZero() {
			continue
		}
		ls := s.learned[u.Key]
		if ls == nil {
			ls = &learnState{}
			s.learned[u.Key] = ls
		}
		ls.samples++
		ts := u.Timestamp.UTC()
		if ls.first.IsZero() || ts.Before(ls.first) {
			ls.first = ts
		}
		if ts.After(ls.last) {
			ls.last = ts
		}
	}
}

// containerCurrent is one eligible container and its sizing at an instant.
type containerCurrent struct {
	key      model.ContainerKey
	req, lim model.Resources
}

// eligibleContainers reproduces the *eligibility* filter of
// recommend.Recommendations — running pods, excluding bare pods and
// Job/CronJob, deduplicated by container key — so that the scored set is
// exactly the set the engine considered. It duplicates a filter, not a
// decision: no percentile, threshold or policy value is involved. The
// recommender does not expose "which containers did you consider", and
// FINDINGS.md asks for that seam.
func eligibleContainers(snap *model.ClusterSnapshot) []containerCurrent {
	byKey := map[model.ContainerKey]containerCurrent{}
	for i := range snap.Pods {
		pod := &snap.Pods[i]
		if pod.Phase != "" && pod.Phase != "Running" {
			continue
		}
		switch pod.Workload.Kind {
		case model.KindBarePod, model.KindJob, model.KindCronJob:
			continue
		}
		for _, c := range pod.Containers {
			key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
			byKey[key] = containerCurrent{key: key, req: c.Requests, lim: c.Limits}
		}
	}
	out := make([]containerCurrent, 0, len(byKey))
	for _, cc := range byKey {
		out = append(out, cc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key.String() < out[j].key.String() })
	return out
}

// decide runs the production path against one snapshot and scores every
// eligible container against the horizon that followed.
func (s *replayState) decide(snap *model.ClusterSnapshot) []record {
	t := snap.Timestamp.UTC()

	recs := s.rec.Recommendations(snap)
	byKey := make(map[model.ContainerKey]recommend.Recommendation, len(recs))
	for _, r := range recs {
		byKey[r.Key] = r
		if r.Class != "" {
			s.lastSeen[r.Key] = r.Class
		}
	}
	steps := map[model.ContainerKey]plan.Step{}
	p, err := plan.Build(snap, recs, s.catalog, s.planCfg)
	if err == nil {
		for _, st := range p.Steps {
			if st.Type != plan.StepResizeWorkload {
				continue
			}
			steps[model.ContainerKey{Workload: st.Workload, Container: st.Container}] = st
		}
	}
	// A planner error is a policy-level failure, not a per-container one:
	// with no plan, nothing would have been applied, so every container
	// scores as a refusal with the planner's own reason.
	planFailed := err != nil

	eligible := eligibleContainers(snap)
	out := make([]record, 0, len(eligible))
	for _, cc := range eligible {
		ws, ok := s.windowFor(cc.key, t)
		if !ok {
			s.skipped.NoFutureSamples++
			continue
		}
		rc, recommended := byKey[cc.key]
		r := record{
			Key: cc.key, At: t,
			Current: cc.req, Target: cc.req, Chosen: cc.req,
			Oracle:  oracleRequest(ws, s.scoring.StarvationFactor),
			Samples: ws.Samples, OOMKills: ws.OOMs, Adverse: ws.Adverse,
		}
		if recommended {
			r.Target = rc.TargetRequest
		}

		// Modelled unit-3 wiring: when enabled, the decision layer gets to
		// veto the change before the planner is consulted, so the current
		// sizing stays in force and the refusal carries the predicate's code.
		var veto *decision.Refusal
		if s.harness.EnforceDecisionRefusals {
			veto = s.refuses(cc, rc, recommended, t)
		}
		st, planned := steps[cc.key]
		switch {
		case veto != nil:
			r.Code = string(veto.Code)
		case planned && !planFailed:
			r.Applied = true
			r.Chosen = st.ToReq
		default:
			r.Code = s.refusalCode(snap, cc, byKey, planFailed, t)
		}
		r.MemViolation, r.CPUStarved = violates(ws, r.Chosen, s.scoring.StarvationFactor)
		out = append(out, r)
	}
	return out
}

// refuses evaluates pkg/decision's shipped predicates for one container at
// one instant, returning the refusal that fired (nil when none did).
func (s *replayState) refuses(cc containerCurrent, rc recommend.Recommendation,
	recommended bool, t time.Time) *decision.Refusal {

	ev := s.decisionEvidence(cc.key, t)
	if recommended {
		// Whether the sizing math wants to shrink is a fact about the
		// recommendation, not a policy choice, so it can be reported
		// honestly to the signal-conflict predicate.
		ev.ShrinkIndicated = rc.TargetRequest.MilliCPU < rc.CurrentRequest.MilliCPU ||
			rc.TargetRequest.MemoryBytes < rc.CurrentRequest.MemoryBytes
		if rc.Class != "" {
			ev.Class = rc.Class
		}
	}
	return decision.Evaluate(ev, s.decCfg, t)
}

// refusalCode names why nothing was applied to this container at this
// instant. Order matters: the decision layer's predicates come first (they
// describe the engine's own reasoning), then the production path's own
// filters, from earliest to latest in the pipeline.
func (s *replayState) refusalCode(snap *model.ClusterSnapshot, cc containerCurrent,
	byKey map[model.ContainerKey]recommend.Recommendation, planFailed bool, t time.Time) string {

	if rc, has := byKey[cc.key]; has {
		// A recommendation existed; the planner is what declined it.
		if planFailed {
			return CodePlanDropped
		}
		if guard.ModeFor(snap, cc.key.Workload, s.planCfg.DefaultMode) != guard.ModeApply {
			return CodeModeGuarded
		}
		minConf := s.planCfg.MinConfidence
		if !(minConf > 0) {
			minConf = plan.DefaultConfig().MinConfidence
		}
		if rc.Confidence < minConf {
			return CodeBelowConfidence
		}
		return CodePlanDropped
	}
	// No recommendation at all. Ask the shipped refusal predicates why.
	if ref := decision.Evaluate(s.decisionEvidence(cc.key, t), s.decCfg, t); ref != nil {
		return string(ref.Code)
	}
	// The predicates found no grounds, so the only remaining reason the
	// recommender stayed silent is its churn suppression.
	return CodeBelowChangeThreshold
}

// decisionEvidence assembles the per-subject fact sheet pkg/decision's
// predicates evaluate. Only the facts the harness can honestly source are
// filled; every unset field means "signal absent", which every predicate
// treats as no grounds to refuse. FINDINGS.md lists what stays unfillable
// until unit 3 is wired into recommend and the substrate's collectors land.
func (s *replayState) decisionEvidence(key model.ContainerKey, t time.Time) decision.Evidence {
	ev := decision.Evidence{Class: s.lastSeen[key]}
	if ls := s.learned[key]; ls != nil {
		ev.Samples = ls.samples
		ev.LastSample = ls.last
		if !ls.first.IsZero() && ls.last.After(ls.first) {
			ev.Window = ls.last.Sub(ls.first)
		}
	}
	store := s.harness.Evidence
	if store == nil {
		return ev
	}
	subj := evidence.ContainerSubject(s.cluster, key)
	// Change events up to and including the decision instant: a change
	// landing exactly at t is news the engine already has, and the soak
	// starts counting from it.
	changes, err := store.Events(subj, time.Time{}, t.Add(time.Nanosecond),
		evidence.EventDeploy, evidence.EventHPAScale, evidence.EventKilterAction, evidence.EventActuationStep)
	if err != nil {
		s.skipped.EventQueryErrors++
	} else if n := len(changes); n > 0 {
		ev.LastChange = changes[n-1].At
	}
	// Conflicting signals inside the learning window that led up to t.
	learnFrom := t.Add(-s.learningSpan())
	adverse, err := store.Events(subj, learnFrom, t.Add(time.Nanosecond),
		evidence.EventOOMKill, evidence.EventThrottleHigh, evidence.EventRegimeChange)
	if err != nil {
		s.skipped.EventQueryErrors++
		return ev
	}
	for _, e := range adverse {
		switch e.Kind {
		case evidence.EventOOMKill:
			ev.OOMsInWindow += 1 + e.Count
		case evidence.EventThrottleHigh:
			ev.ThrottledInWindow = true
		case evidence.EventRegimeChange:
			if e.At.After(ev.LastChangepoint) {
				ev.LastChangepoint = e.At
			}
		}
	}
	return ev
}

// learningSpan is the lookback used for "inside the learning window"
// signals: the policy's own minimum window, but never shorter than the
// decision cadence, so consecutive instants cover the history continuously.
func (s *replayState) learningSpan() time.Duration {
	span := s.decCfg.MinWindow
	if s.scoring.DecisionInterval > span {
		span = s.scoring.DecisionInterval
	}
	if span <= 0 {
		span = 24 * time.Hour
	}
	return span
}

// windowFor gathers the ground truth for (t, t+horizon]: the usage the
// container actually drew, plus the adverse events the substrate recorded.
// Reports false when the window holds no usage sample — with no ground truth
// there is nothing to score and nothing to build an oracle from.
func (s *replayState) windowFor(key model.ContainerKey, t time.Time) (windowStats, bool) {
	pts := s.future.window(key, t, t.Add(s.horizon))
	if len(pts) == 0 {
		return windowStats{}, false
	}
	ws := statsOver(pts)
	store := s.harness.Evidence
	if store == nil {
		return ws, true
	}
	// The scoring window is (t, t+horizon]; evidence windows are [from, to),
	// so both bounds shift by one nanosecond to express it exactly.
	evs, err := store.Events(evidence.ContainerSubject(s.cluster, key),
		t.Add(time.Nanosecond), t.Add(s.horizon).Add(time.Nanosecond),
		evidence.EventOOMKill, evidence.EventThrottleHigh, evidence.EventRegimeChange)
	if err != nil {
		s.skipped.EventQueryErrors++
		return ws, true
	}
	for _, e := range evs {
		ws.Adverse = true
		if e.Kind == evidence.EventOOMKill {
			ws.OOMs += 1 + e.Count
		}
	}
	return ws, true
}

// futureIndex is the replay's usage history, indexed for window queries. It
// is built once from the same snapshots the engine replays, so scoring and
// learning can never disagree about what was measured.
type futureIndex struct {
	byKey map[model.ContainerKey][]usagePoint
}

func newFutureIndex(snaps []*model.ClusterSnapshot) *futureIndex {
	f := &futureIndex{byKey: map[model.ContainerKey][]usagePoint{}}
	// Snapshots arrive sorted, so appending in order keeps every per-key
	// slice sorted by time without a second sort.
	for _, snap := range snaps {
		for _, u := range snap.Usage {
			if u.MilliCPU < 0 || u.MemoryBytes < 0 || u.Timestamp.IsZero() {
				continue
			}
			f.byKey[u.Key] = append(f.byKey[u.Key], usagePoint{
				At: u.Timestamp.UTC(), MilliCPU: u.MilliCPU, MemoryBytes: u.MemoryBytes,
			})
		}
	}
	// Usage entries within one snapshot may be listed in any order (several
	// replicas at the same instant, or a collector that batches out of
	// order), so the per-key slices are sorted explicitly. Ties keep their
	// arrival order, which does not affect any window statistic.
	for _, pts := range f.byKey {
		sort.SliceStable(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	}
	return f
}

// window returns the points in (from, to] — strictly after the decision
// instant (the engine already saw that sample) and including the far edge.
func (f *futureIndex) window(key model.ContainerKey, from, to time.Time) []usagePoint {
	pts := f.byKey[key]
	if len(pts) == 0 {
		return nil
	}
	lo := sort.Search(len(pts), func(i int) bool { return pts[i].At.After(from) })
	hi := sort.Search(len(pts), func(i int) bool { return pts[i].At.After(to) })
	if lo >= hi {
		return nil
	}
	return pts[lo:hi]
}

// records exposes the per-decision ledger behind a scorecard. It is the same
// replay Run performs, stopping before aggregation, and exists so tests can
// inspect and shuffle individual decisions.
func (h *Harness) records(cluster string, from, to time.Time, horizon time.Duration) ([]record, error) {
	var out []record
	_, err := h.run(cluster, from, to, horizon, &out)
	return out, err
}
