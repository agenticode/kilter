package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/model"
)

// The brain's evidence substrate.
//
// Before this file `grep 'evidence\.' pkg/api/brain.go` returned nothing, and
// that single absence blocked both explanation routes: WhyCost reads
// Store.Timeline, BuildExplain reads the substrate through the dossier
// builder, and — the part that makes it non-optional — Verify re-resolves
// every citation against *the same store that produced the answer*. A route
// served from a store the brain never populated does not degrade gracefully;
// it serves an answer whose citations point at nothing.
//
// Everything here is fed from Ingest, which is the only place the brain sees
// a cluster. Three kinds of record go in:
//
//   - SAMPLES, one per model.Usage row, against the container subject. This is
//     what the digests, and therefore the usage half of an explanation, are
//     built from.
//   - A TIMELINE POINT per snapshot, against the cluster. This is ΔCost: the
//     decomposition explains a measurement, it does not replace one.
//   - DEPLOY AND OOMKILL EVENTS, derived from the fields the collectors
//     already carry (ContainerSpec.RestartCount / LastOOMKilled, and the
//     declared requests/limits that a rollout changes). These are what ground
//     a term or a driver in something an operator can go and look at.
//
// Nothing here invents a signal. model.Usage has no throttle ratio and no
// restart delta, so Sample.ThrottleRatio and Sample.Restarts stay zero rather
// than carrying a number nobody measured — "signal absent" is a state
// pkg/decision already knows how to read.

// substrateState is the per-cluster memory that turns levels into edges: the
// substrate stores "a deploy happened", but a snapshot only says "the spec is
// currently X". Detecting the change needs the previous snapshot's X.
//
// It is bounded by construction. Both maps are REBUILT from the current
// snapshot on every ingest, carrying forward only keys the snapshot still
// mentions, so pod churn over months cannot grow them: their size is the
// size of the cluster, not the size of its history.
type substrateState struct {
	mu sync.Mutex
	// restarts maps "<podUID>\x00<container>" to the restart count last seen.
	restarts map[string]int32
	// specs maps a container template to the sizing last seen declared.
	specs map[model.ContainerKey]sizing
	// replicas maps a workload to the replica count last seen.
	replicas map[model.WorkloadRef]int32
}

type sizing struct {
	requests model.Resources
	limits   model.Resources
}

func newSubstrateState() *substrateState {
	return &substrateState{
		restarts: map[string]int32{},
		specs:    map[model.ContainerKey]sizing{},
		replicas: map[model.WorkloadRef]int32{},
	}
}

// substrateFor returns (creating) a cluster's change-detection state.
// Caller must not hold b.mu.
func (b *Brain) substrateFor(cluster string) *substrateState {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.subs[cluster]
	if s == nil {
		s = newSubstrateState()
		b.subs[cluster] = s
	}
	return s
}

// Evidence exposes the brain's substrate as the read interface pkg/explain
// resolves citations against. It is never nil: a brain without a substrate
// could not verify an answer, and an unverifiable answer is the thing this
// package must not serve.
func (b *Brain) Evidence() evidence.Store { return b.mem }

// restoreEvidence rebuilds the substrate from the store, or starts empty.
//
// A checkpoint that cannot be understood is a HARD failure of NewBrain, not a
// silent fresh start. Starting empty after a failed restore would look
// identical to a cold boot, and the brain would then serve explanations over
// a substrate missing exactly the history the operator is asking about.
func restoreEvidence(cfg evidence.Config, cs evidence.CheckpointStore) (*evidence.Memory, error) {
	mem, err := evidence.Load(context.Background(), cs)
	switch {
	case err == nil:
		return mem, nil
	case errors.Is(err, evidence.ErrNoCheckpoint):
		return evidence.NewMemory(cfg)
	default:
		return nil, fmt.Errorf("api: restore evidence substrate: %w", err)
	}
}

// saveEvidence checkpoints the substrate. Errors are logged, not returned:
// losing a checkpoint costs history, while failing an ingest costs the
// observation itself, and the observation is the more expensive of the two.
func (b *Brain) saveEvidence() {
	if b.st == nil {
		return
	}
	if err := evidence.Save(context.Background(), b.st.EvidenceCheckpoints(), b.mem); err != nil {
		b.cfg.Logger.Error("persist evidence", "err", err)
	}
}

// observeIntoSubstrate records one snapshot's evidence.
//
// Two ordering rules from pkg/evidence apply and both are handled rather than
// propagated: samples and timeline points must arrive in non-decreasing time
// order per subject, and an out-of-order one is refused with ErrOutOfOrder.
// A replayed or late snapshot is a normal event on an ingest endpoint, so it
// is counted and skipped — never allowed to fail the ingest, and never
// silently absorbed either: the counts are logged.
func (b *Brain) observeIntoSubstrate(snap *model.ClusterSnapshot, hourlyUSD float64) {
	cluster := snap.ClusterID

	// The timeline's node count is the count the COMPOSITION prices, so the
	// two agree on what a node is (a Fargate "node" is a single-pod VM billed
	// per quantized pod, and basisFrom excludes it).
	if err := b.mem.ObservePoint(cluster, evidence.TimelinePoint{
		At: snap.Timestamp, CostUSDPerHour: hourlyUSD, Nodes: int(countPricedNodes(snap)),
	}); err != nil && !errors.Is(err, evidence.ErrOutOfOrder) {
		b.cfg.Logger.Error("record timeline point", "cluster", cluster, "err", err)
	}

	// Samples are fed oldest-first. A snapshot carries a whole window of usage
	// rows in collector order, and feeding them unsorted would make every row
	// older than the newest one a dropped sample — the substrate would hold a
	// sparse, arrival-order-dependent subset of a series it was handed whole.
	idx := make([]int, len(snap.Usage))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, c int) bool {
		ua, uc := &snap.Usage[idx[a]], &snap.Usage[idx[c]]
		ta, tc := sampleTime(ua, snap), sampleTime(uc, snap)
		if !ta.Equal(tc) {
			return ta.Before(tc)
		}
		// A stable tie-break so two rows at one instant are always fed in the
		// same order, whatever order the collector emitted them in.
		return ua.Key.String() < uc.Key.String()
	})
	var stale, failed int
	for _, i := range idx {
		u := &snap.Usage[i]
		err := b.mem.ObserveSample(evidence.ContainerSubject(cluster, u.Key), evidence.Sample{
			At: sampleTime(u, snap), MilliCPU: u.MilliCPU, MemoryBytes: u.MemoryBytes,
		})
		switch {
		case err == nil:
		case errors.Is(err, evidence.ErrOutOfOrder):
			stale++
		default:
			failed++
		}
	}
	if stale > 0 || failed > 0 {
		b.cfg.Logger.Debug("substrate samples skipped", "cluster", cluster,
			"stale", stale, "rejected", failed, "total", len(snap.Usage))
	}

	for _, ev := range b.substrateFor(cluster).events(cluster, snap) {
		if err := b.mem.Append(ev); err != nil {
			b.cfg.Logger.Error("record evidence event", "cluster", cluster, "kind", ev.Kind, "err", err)
		}
	}
}

// sampleTime is a usage row's own timestamp, falling back to the snapshot's.
// A zero sample time would put a year-1 point on the series.
func sampleTime(u *model.Usage, snap *model.ClusterSnapshot) time.Time {
	if u.Timestamp.IsZero() {
		return snap.Timestamp
	}
	return u.Timestamp
}

// events diffs the snapshot against the previous one and returns the evidence
// events the difference implies, in a deterministic order.
//
// The returned slice is sorted by (Kind, Subject) before it is appended, so
// the arrival sequence the substrate assigns — which is its eviction and
// tie-break key, and which is checkpointed — does not depend on Go's map
// iteration order. Two runs over the same history produce the same substrate.
func (s *substrateState) events(cluster string, snap *model.ClusterSnapshot) []evidence.EvidenceEvent {
	s.mu.Lock()
	defer s.mu.Unlock()

	nextRestarts := make(map[string]int32, len(s.restarts))
	nextSpecs := make(map[model.ContainerKey]sizing, len(s.specs))
	nextReplicas := make(map[model.WorkloadRef]int32, len(s.replicas))
	var out []evidence.EvidenceEvent

	// ---- OOMKills, per pod-container.
	//
	// The predicate is pkg/recommend's (recommend.go: `seen && c.RestartCount
	// > prev && c.LastOOMKilled`) with one addition: a container first seen
	// already carrying an OOMKill is reported once. It is news to this brain,
	// and suppressing it would mean a brain that starts after an incident can
	// never see it. Because the state is keyed on the pod UID and the restart
	// count, it is reported exactly once and not again.
	for i := range snap.Pods {
		p := &snap.Pods[i]
		for j := range p.Containers {
			c := &p.Containers[j]
			k := p.UID + "\x00" + c.Name
			prev, seen := s.restarts[k]
			nextRestarts[k] = c.RestartCount
			if !c.LastOOMKilled {
				continue
			}
			if seen && c.RestartCount <= prev {
				continue
			}
			if !seen && c.RestartCount == 0 {
				// LastOOMKilled with no restart yet is a container that has
				// not come back; there is no completed kill to date.
				continue
			}
			key := model.ContainerKey{Workload: p.Workload, Container: c.Name}
			out = append(out, evidence.EvidenceEvent{
				// The snapshot instant, not the kill instant: a
				// model.ClusterSnapshot does not carry the termination time,
				// and inventing one would make a citation point at a moment
				// nobody observed.
				At:       snap.Timestamp,
				Kind:     evidence.EventOOMKill,
				Subject:  evidence.ContainerSubject(cluster, key),
				Severity: evidence.SeverityCritical,
				Attrs: map[string]string{
					"pod":          p.Name,
					"namespace":    p.Namespace,
					"restartCount": strconv.Itoa(int(c.RestartCount)),
					"observedAt":   "snapshot",
				},
				// Folds informer replays of the same kill inside the
				// substrate's dedup window.
				Dedup: k + "\x00" + strconv.Itoa(int(c.RestartCount)),
			})
		}
	}

	// ---- Deploys, per workload.
	//
	// A deploy is a spec change. What a snapshot carries of one is the
	// declared sizing of each container template and the replica count, so
	// those are what is diffed. Image and generation are not in
	// model.ClusterSnapshot; a deploy that changed only the image is
	// therefore invisible here, which FINDINGS records rather than papers over.
	changed := map[model.WorkloadRef][]string{}
	for _, key := range currentSizings(snap, nextSpecs) {
		prev, seen := s.specs[key.k]
		if seen && prev != key.v {
			changed[key.k.Workload] = append(changed[key.k.Workload],
				key.k.Container+": "+sizingDelta(prev, key.v))
		}
	}
	for i := range snap.Workloads {
		w := &snap.Workloads[i]
		if _, dup := nextReplicas[w.Ref]; dup {
			continue // first entry wins, deterministically
		}
		nextReplicas[w.Ref] = w.Replicas
		if prev, seen := s.replicas[w.Ref]; seen && prev != w.Replicas {
			changed[w.Ref] = append(changed[w.Ref],
				"replicas: "+strconv.Itoa(int(prev))+"→"+strconv.Itoa(int(w.Replicas)))
		}
	}
	refs := make([]model.WorkloadRef, 0, len(changed))
	for ref := range changed {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	for _, ref := range refs {
		detail := changed[ref]
		sort.Strings(detail)
		out = append(out, evidence.EvidenceEvent{
			At:   snap.Timestamp,
			Kind: evidence.EventDeploy,
			// Against the WORKLOAD, not the container template: that is where
			// pkg/explain looks for the change events that explain a
			// container's post-rollout behaviour (payload.go's parent-workload
			// pull).
			Subject:  evidence.WorkloadSubject(cluster, ref),
			Severity: evidence.SeverityInfo,
			Attrs: map[string]string{
				"namespace": ref.Namespace,
				"workload":  ref.Name,
				"changed":   joinCapped(detail, 3),
			},
			Dedup: ref.String() + "\x00" + joinCapped(detail, 3),
		})
	}

	s.restarts, s.specs, s.replicas = nextRestarts, nextSpecs, nextReplicas
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Subject.String() < out[j].Subject.String()
	})
	return out
}

type sizingEntry struct {
	k model.ContainerKey
	v sizing
}

// currentSizings returns each container template's currently declared sizing,
// filling next as it goes.
//
// During a rollout the old and new pods coexist with different specs, so
// "the" sizing of a template is ambiguous. The NEWEST pod's spec wins
// (CreatedAt, then UID as a total order) — that is the closest thing a
// snapshot holds to the desired spec, and it is a function of the snapshot
// rather than of pod ordering.
func currentSizings(snap *model.ClusterSnapshot, next map[model.ContainerKey]sizing) []sizingEntry {
	type cand struct {
		v         sizing
		createdAt time.Time
		uid       string
	}
	best := map[model.ContainerKey]cand{}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Phase == "Succeeded" || p.Phase == "Failed" {
			continue
		}
		for j := range p.Containers {
			c := &p.Containers[j]
			k := model.ContainerKey{Workload: p.Workload, Container: c.Name}
			cur := cand{sizing{c.Requests, c.Limits}, p.CreatedAt, p.UID}
			prev, seen := best[k]
			if !seen || cur.createdAt.After(prev.createdAt) ||
				(cur.createdAt.Equal(prev.createdAt) && cur.uid > prev.uid) {
				best[k] = cur
			}
		}
	}
	out := make([]sizingEntry, 0, len(best))
	for k, c := range best {
		next[k] = c.v
		out = append(out, sizingEntry{k, c.v})
	}
	// Sorted: this slice drives event emission order.
	sort.Slice(out, func(i, j int) bool { return out[i].k.String() < out[j].k.String() })
	return out
}

func sizingDelta(a, b sizing) string {
	return "req " + resStr(a.requests) + "→" + resStr(b.requests) +
		", lim " + resStr(a.limits) + "→" + resStr(b.limits)
}

func resStr(r model.Resources) string {
	return strconv.FormatInt(r.MilliCPU, 10) + "m/" + strconv.FormatInt(r.MemoryBytes, 10) + "B"
}

// joinCapped renders at most n items, naming how many it left out. Attrs are
// length-capped by the substrate anyway; saying "+4 more" beats being
// truncated mid-word by the sanitizer.
func joinCapped(items []string, n int) string {
	if len(items) <= n {
		return joinComma(items)
	}
	return joinComma(items[:n]) + " (+" + strconv.Itoa(len(items)-n) + " more)"
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// countPricedNodes counts the nodes the cost composition prices, so the
// timeline's node count and the composition's agree about what a node is.
func countPricedNodes(snap *model.ClusterSnapshot) int64 {
	var n int64
	for i := range snap.Nodes {
		if !snap.Nodes[i].IsFargate() {
			n++
		}
	}
	return n
}
