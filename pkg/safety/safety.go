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
func CanEvict(p *model.PodSpec) Evictability {
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
// other non-evictable pod does.
func BlocksDrain(p *model.PodSpec) (bool, string) {
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
type PDBGuard struct {
	mu   sync.Mutex
	pdbs []model.PDB // local copy; DisruptionsAllowed is mutated by Reserve
}

// NewPDBGuard copies the given PDBs into a guard.
func NewPDBGuard(pdbs []model.PDB) *PDBGuard {
	cp := make([]model.PDB, len(pdbs))
	copy(cp, pdbs)
	return &PDBGuard{pdbs: cp}
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
// more disruption.
func (g *PDBGuard) CanEvict(p *model.PodSpec) (bool, string) {
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
// Returns false (reserving nothing) if any budget is exhausted.
func (g *PDBGuard) Reserve(p *model.PodSpec) (bool, string) {
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
// its replacement went Ready).
func (g *PDBGuard) Release(p *model.PodSpec) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, i := range g.matching(p) {
		g.pdbs[i].DisruptionsAllowed++
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
func (c *Cooldowns) Remaining(key string, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.last[key]
	if !ok {
		return 0
	}
	if d := c.interval - now.Sub(t); d > 0 {
		return d
	}
	return 0
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

type changeRecord struct {
	at       time.Time
	restarts int64
	ooms     int64
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

// workloadHealth sums restart/OOM counters for a workload in a snapshot.
func workloadHealth(snap *model.ClusterSnapshot, ref model.WorkloadRef) (restarts, ooms int64) {
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Workload != ref {
			continue
		}
		for _, c := range p.Containers {
			restarts += int64(c.RestartCount)
			if c.LastOOMKilled {
				ooms++
			}
		}
	}
	return restarts, ooms
}

// RecordChange captures the health baseline for a workload Kilter just changed.
func (d *RegressionDetector) RecordChange(ref model.WorkloadRef, snap *model.ClusterSnapshot, now time.Time) {
	restarts, ooms := workloadHealth(snap, ref)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.changes[ref] = &changeRecord{at: now, restarts: restarts, ooms: ooms}
}

// Check compares current health against baselines. Regressed workloads are
// returned once and quarantined; expired watches are dropped.
func (d *RegressionDetector) Check(snap *model.ClusterSnapshot, now time.Time) []Regression {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Regression
	for ref, rec := range d.changes {
		if now.Sub(rec.at) > d.window {
			delete(d.changes, ref)
			continue
		}
		restarts, ooms := workloadHealth(snap, ref)
		var reason string
		switch {
		case ooms > rec.ooms:
			reason = fmt.Sprintf("OOM kills rose %d→%d after change", rec.ooms, ooms)
		case restarts > rec.restarts+2:
			reason = fmt.Sprintf("restarts rose %d→%d after change (crashloop)", rec.restarts, restarts)
		default:
			continue
		}
		out = append(out, Regression{Ref: ref, Reason: reason, DetectedAt: now})
		d.quarantine[ref] = now.Add(d.quarFor)
		delete(d.changes, ref)
	}
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
