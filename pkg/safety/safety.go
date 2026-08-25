// Package safety is Kilter's disruption conscience. Every plan and every
// actuation passes through these checks; none of them are optional in apply
// mode. The rules encode how mature operators think about touching prod:
//
//   - Never take what a PodDisruptionBudget doesn't give.
//   - Some pods are simply not yours to move (bare pods, local state, opt-outs).
//   - Rate-limit disruption; burst evictions are how incidents start.
//   - After you change something, watch it; if it regresses, undo and back off.
package safety

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// Evictability classifies whether a pod may be moved by Kilter.
type Evictability struct {
	OK     bool
	Reason string // set when !OK
}

// CanEvict applies controller-independent eviction rules (PDBs are separate;
// see PDBGuard). DaemonSet pods return OK=false with a special reason because
// they are not *moved* — they die with the node and respawn elsewhere.
// A nil pod is not evictable: a pod we know nothing about is not ours to move.
func CanEvict(p *model.PodSpec) Evictability {
	if p == nil {
		return Evictability{false, "nil pod"}
	}
	switch {
	case p.DoNotEvict:
		return Evictability{false, "pod opted out (do-not-evict annotation)"}
	case p.Workload.Kind == model.KindBarePod:
		return Evictability{false, "bare pod: no controller would recreate it"}
	case p.Workload.Kind == model.KindDaemonSet:
		return Evictability{false, "daemonset pod: bound to its node"}
	case p.HasLocalStorage:
		return Evictability{false, "pod uses node-local storage"}
	}
	return Evictability{OK: true}
}

// BlocksDrain reports whether the pod prevents removing its node entirely.
// DaemonSet pods do NOT block a drain (they disappear with the node); every
// other non-evictable pod does. Note the DaemonSet rule wins over DoNotEvict:
// the annotation on a DaemonSet pod does not pin its node. A nil pod blocks
// the drain — when we can't classify a pod, we keep its node.
func BlocksDrain(p *model.PodSpec) (bool, string) {
	if p == nil {
		return true, "nil pod"
	}
	if p.Workload.Kind == model.KindDaemonSet {
		return false, ""
	}
	if ev := CanEvict(p); !ev.OK {
		return true, ev.Reason
	}
	return false, ""
}

// PDBGuard answers eviction questions against the cluster's disruption
// budgets, with plan-time bookkeeping: reserving an eviction decrements the
// budget so a single plan can't overspend what the API would later refuse.
//
// The ledger is seeded from the snapshot and never rises above it. Release is
// the rollback of a Reserve, not a credit line: a caller that rolls back twice,
// or releases a pod it never reserved, must not be able to invent disruptions
// the API will go on to refuse.
type PDBGuard struct {
	mu   sync.Mutex
	pdbs []model.PDB // local copy; DisruptionsAllowed is mutated by Reserve
	// initial[i] is pdbs[i].DisruptionsAllowed as collected — the ceiling
	// Release may restore to, never exceed.
	initial []int32
}

// NewPDBGuard copies the given PDBs into a guard. The copy is shallow: the
// Selector map and CoveredPodUIDs slice stay shared with the caller and are
// treated as read-only. DisruptionsAllowed is a value field, so reservations
// made here never drain the caller's snapshot — plan phases that read
// snap.PDBs after planning still see the collected numbers.
func NewPDBGuard(pdbs []model.PDB) *PDBGuard {
	cp := make([]model.PDB, len(pdbs))
	copy(cp, pdbs)
	initial := make([]int32, len(cp))
	for i := range cp {
		// The API never reports a negative allowance. A corrupt snapshot that
		// does means the same thing as zero — refuse — so normalise it here
		// rather than carry a nonsense number through the ledger, where it
		// would also pin the budget permanently below its own Release ceiling.
		if cp[i].DisruptionsAllowed < 0 {
			cp[i].DisruptionsAllowed = 0
		}
		initial[i] = cp[i].DisruptionsAllowed
	}
	return &PDBGuard{pdbs: cp, initial: initial}
}

// matching returns indexes of PDBs selecting the pod.
func (g *PDBGuard) matching(p *model.PodSpec) []int {
	var out []int
	for i := range g.pdbs {
		if g.pdbs[i].Covers(p) {
			out = append(out, i)
		}
	}
	return out
}

// CanEvict reports whether all budgets covering the pod currently allow one
// more disruption. A nil pod is refused: PDB coverage is decided by the pod's
// namespace, labels and UID, so a pod we cannot read matches nothing and would
// otherwise sail past every budget.
func (g *PDBGuard) CanEvict(p *model.PodSpec) (bool, string) {
	if p == nil {
		return false, "nil pod"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, i := range g.matching(p) {
		if g.pdbs[i].DisruptionsAllowed <= 0 {
			return false, fmt.Sprintf("PDB %s/%s allows no disruptions", g.pdbs[i].Namespace, g.pdbs[i].Name)
		}
	}
	return true, ""
}

// Reserve consumes one disruption from every budget covering the pod.
// Returns false (reserving nothing) if any budget is exhausted; the check runs
// over every covering budget before a single one is decremented, so a refused
// reservation leaves the ledger untouched. A nil pod is refused, as in CanEvict.
func (g *PDBGuard) Reserve(p *model.PodSpec) (bool, string) {
	if p == nil {
		return false, "nil pod"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	idxs := g.matching(p)
	for _, i := range idxs {
		if g.pdbs[i].DisruptionsAllowed <= 0 {
			return false, fmt.Sprintf("PDB %s/%s allows no disruptions", g.pdbs[i].Namespace, g.pdbs[i].Name)
		}
	}
	for _, i := range idxs {
		g.pdbs[i].DisruptionsAllowed--
	}
	return true, ""
}

// Release returns one disruption to every budget covering the pod (e.g. after
// its replacement went Ready, or when a plan rolls back a reservation it can no
// longer use). It never lifts a budget above the value collected from the
// cluster, so an unbalanced Release is a no-op rather than free disruption
// budget. Releasing a nil pod is harmless: no budget covers a pod that cannot
// be read, so there is nothing to give back. Unlike CanEvict and Reserve it
// needs no explicit nil check — matching nothing is the safe direction here,
// and the fail-open direction there.
func (g *PDBGuard) Release(p *model.PodSpec) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, i := range g.matching(p) {
		if g.pdbs[i].DisruptionsAllowed < g.initial[i] {
			g.pdbs[i].DisruptionsAllowed++
		}
	}
}

// Cooldowns rate-limits repeated actions on the same key (workload, node).
type Cooldowns struct {
	mu       sync.Mutex
	last     map[string]time.Time
	interval time.Duration
}

// NewCooldowns creates a tracker with the given minimum interval per key.
func NewCooldowns(interval time.Duration) *Cooldowns {
	return &Cooldowns{last: map[string]time.Time{}, interval: interval}
}

// Allow reports whether the key is out of cooldown, and if so starts a new one.
func (c *Cooldowns) Allow(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.last[key]; ok && now.Sub(t) < c.interval {
		return false
	}
	c.last[key] = now
	return true
}

// Remaining returns how much cooldown is left for a key (0 if none).
//
// It mirrors Allow's predicate exactly — Remaining > 0 if and only if Allow
// would deny — so a caller that asks "how long?" and a caller that asks "may
// I?" can never disagree. That needs care on garbage clocks: time.Time.Sub
// saturates at ±the Duration range, and interval-elapsed then overflows to a
// negative for a zero-value or far-past `now`, which would report "no
// cooldown" while Allow denies. Clamp instead of wrapping.
func (c *Cooldowns) Remaining(key string, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.last[key]
	if !ok {
		return 0
	}
	elapsed := now.Sub(t)
	if elapsed >= c.interval {
		return 0
	}
	d := c.interval - elapsed
	if d < 0 {
		return time.Duration(math.MaxInt64)
	}
	return d
}

// Prune drops keys whose cooldown has lapsed and reports how many went.
//
// It changes no answer this tracker gives — to Allow and Remaining a lapsed key
// and an unknown key are already the same thing. It exists because the
// controller keys cooldowns by workload and node and keeps one tracker for the
// lifetime of the process: without a sweep, every workload ever deleted and
// every node ever scaled in stays resident forever.
func (c *Cooldowns) Prune(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k, t := range c.last {
		// Same predicate as Remaining's "nothing left"; see its comment for why
		// this must not be written as a subtraction against the interval.
		if now.Sub(t) >= c.interval {
			delete(c.last, k)
			n++
		}
	}
	return n
}

// Budget is a sliding-window disruption budget: at most N evictions per window.
type Budget struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	events []time.Time
}

// NewBudget allows max events per window.
func NewBudget(max int, window time.Duration) *Budget {
	return &Budget{max: max, window: window}
}

// Allow consumes one slot if available.
func (b *Budget) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-b.window)
	kept := b.events[:0]
	for _, e := range b.events {
		if e.After(cutoff) {
			kept = append(kept, e)
		}
	}
	b.events = kept
	if len(b.events) >= b.max {
		return false
	}
	b.events = append(b.events, now)
	return true
}

// Used reports current consumption within the window.
func (b *Budget) Used(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-b.window)
	n := 0
	for _, e := range b.events {
		if e.After(cutoff) {
			n++
		}
	}
	return n
}

// Regression is a post-change health degradation on a workload Kilter touched.
type Regression struct {
	Ref        model.WorkloadRef
	Reason     string
	DetectedAt time.Time
}

// RegressionDetector watches workloads after Kilter changes them. If restarts
// or OOM kills rise within the observation window, the change is declared a
// regression: the controller reverts it and quarantines the recommendation.
type RegressionDetector struct {
	mu     sync.Mutex
	window time.Duration
	// baseline restart totals per changed workload, captured at change time.
	changes map[model.WorkloadRef]*changeRecord
	// quarantined workloads → until when.
	quarantine map[model.WorkloadRef]time.Time
	quarFor    time.Duration
}

// restartTolerance is how many post-change restarts are written off as the
// rollout itself: replacing a pod legitimately costs a restart or two while the
// new one warms up. The third restart inside the observation window is a
// crashloop, not a rollout.
const restartTolerance = 2

// podHealth is one pod's failure counters at a point in time.
type podHealth struct {
	restarts int64
	ooms     int64
}

type changeRecord struct {
	at time.Time
	// pods is the per-pod baseline, keyed by podHealthKey.
	//
	// Restart counters live on the *pod*, not on the workload, and a resize
	// replaces every pod — so the workload's post-change total is unrelated to
	// its pre-change total. Comparing the two sums (the obvious implementation)
	// hides exactly the failure this watch exists to catch: a workload with a
	// long restart history is replaced by fresh pods starting at 0, and the new
	// pods can crashloop for a long time before the total climbs back above the
	// old baseline. Per-pod deltas are the only comparison that means anything.
	pods map[string]podHealth
}

// podHealthKey identifies a pod across snapshots. UID is the only stable
// identity a collector gives us; the namespace/name fallback keeps hand-built
// PodSpecs (tests, non-Kubernetes callers) from collapsing into one bucket.
func podHealthKey(p *model.PodSpec) string {
	if p.UID != "" {
		return "uid:" + p.UID
	}
	return "nn:" + p.Namespace + "/" + p.Name
}

// sinceChange sums the failure counters that appeared after the baseline was
// taken, across the workload's current pods.
//
// Pods absent from the baseline were created by (or after) the change, so
// everything they report is new. Surviving pods contribute only their increase.
// A *decrease* — a replaced container, a kubelet that reset the counter —
// contributes zero and can never offset another pod's genuine regression.
func (r *changeRecord) sinceChange(cur map[string]podHealth) (restarts, ooms int64) {
	for key, h := range cur {
		base := r.pods[key] // zero value when the pod is new: count it all
		if d := h.restarts - base.restarts; d > 0 {
			restarts += d
		}
		if d := h.ooms - base.ooms; d > 0 {
			ooms += d
		}
	}
	return restarts, ooms
}

// NewRegressionDetector watches for `window` after each change and
// quarantines regressed workloads for `quarantineFor`.
func NewRegressionDetector(window, quarantineFor time.Duration) *RegressionDetector {
	return &RegressionDetector{
		window:     window,
		changes:    map[model.WorkloadRef]*changeRecord{},
		quarantine: map[model.WorkloadRef]time.Time{},
		quarFor:    quarantineFor,
	}
}

// workloadHealth indexes the failure counters of a workload's pods in a
// snapshot, keyed by podHealthKey. A nil snapshot yields an empty index: a
// failed collection is missing evidence, not a clean bill of health. Pods that
// share a key (unset UID *and* unset name) accumulate rather than overwrite,
// degrading to a workload-wide sum instead of silently dropping counters.
func workloadHealth(snap *model.ClusterSnapshot, ref model.WorkloadRef) map[string]podHealth {
	out := map[string]podHealth{}
	if snap == nil {
		return out
	}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Workload != ref {
			continue
		}
		key := podHealthKey(p)
		h := out[key]
		for _, c := range p.Containers {
			h.restarts += int64(c.RestartCount)
			if c.LastOOMKilled {
				h.ooms++
			}
		}
		out[key] = h
	}
	return out
}

// RecordChange captures the health baseline for a workload Kilter just changed.
// Calling it again for the same workload re-baselines the watch on the current
// numbers.
//
// A nil snapshot arms nothing. Without a baseline every pre-existing restart
// would later read as new and revert a change that was in fact healthy, and a
// spurious revert is itself production disruption.
func (d *RegressionDetector) RecordChange(ref model.WorkloadRef, snap *model.ClusterSnapshot, now time.Time) {
	if snap == nil {
		return
	}
	pods := workloadHealth(snap, ref)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.changes[ref] = &changeRecord{at: now, pods: pods}
}

// Check compares current health against baselines. Regressed workloads are
// returned once and quarantined; expired watches are dropped. The result is
// sorted by workload so operators, logs and tests see a stable order rather
// than Go's per-run map order.
//
// A nil snapshot is a failed collection: watches stay armed and nothing is
// reported, because reverting on no evidence is itself a disruption. Watches
// still expire on schedule — a window we could not observe has still passed.
func (d *RegressionDetector) Check(snap *model.ClusterSnapshot, now time.Time) []Regression {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Drop lapsed quarantines here as well as in Quarantined: a long-lived
	// controller only asks about workloads it still plans to touch, so
	// Quarantined alone cannot be relied on to bound this map.
	for ref, until := range d.quarantine {
		if now.After(until) {
			delete(d.quarantine, ref)
		}
	}

	var out []Regression
	for ref, rec := range d.changes {
		if now.Sub(rec.at) > d.window {
			delete(d.changes, ref)
			continue
		}
		// A nil snapshot yields an empty index (see workloadHealth), hence
		// zero deltas and no verdict — the watch simply stays armed.
		restarts, ooms := rec.sinceChange(workloadHealth(snap, ref))
		var reason string
		switch {
		case ooms > 0:
			reason = fmt.Sprintf("%d OOM kill(s) after change", ooms)
		case restarts > restartTolerance:
			reason = fmt.Sprintf("%d restarts after change (crashloop)", restarts)
		default:
			continue
		}
		out = append(out, Regression{Ref: ref, Reason: reason, DetectedAt: now})
		d.quarantine[ref] = now.Add(d.quarFor)
		delete(d.changes, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Ref, out[j].Ref
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})
	return out
}

// Quarantined reports whether a workload is currently quarantined (recently
// regressed after a Kilter change; leave it alone).
func (d *RegressionDetector) Quarantined(ref model.WorkloadRef, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.quarantine[ref]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(d.quarantine, ref)
		return false
	}
	return true
}
