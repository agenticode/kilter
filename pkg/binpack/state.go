// Package binpack simulates Kubernetes scheduling well enough to answer the
// two questions every safe optimizer must answer before touching a cluster:
//
//  1. "If I drain this node, will every pod on it actually fit elsewhere?"
//  2. "What is the cheapest set of nodes that fits this workload?"
//
// Supported scheduling surface: resource requests, node readiness, max pods,
// nodeSelector, required node affinity (In/NotIn/Exists/DoesNotExist/Gt/Lt),
// taints & tolerations (NoSchedule/NoExecute), workload self-anti-affinity by
// topology key, and workload topology-spread constraints (DoNotSchedule).
// Anti-affinity and spread are evaluated per workload (the self-selecting
// case, which covers the overwhelming majority of real manifests).
package binpack

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/agenticode/kilter/pkg/model"
)

// DefaultMaxPodsPerNode mirrors the kubelet default.
const DefaultMaxPodsPerNode = 110

// NodeState tracks one node's remaining schedulable capacity.
//
// Free can go negative: ground-truth pods are force-placed without feasibility
// checks (see NewClusterState), so an overcommitted real node is represented
// faithfully rather than clamped. Fits never admits new pods onto such a node.
type NodeState struct {
	Spec     model.NodeSpec
	Free     model.Resources
	FreeExt  map[string]int64 // extended resources (GPUs, …)
	PodCount int
	MaxPods  int
	pods     map[string]*model.PodSpec // by UID
}

// requestsOf returns p's summed requests with negative dimensions clamped to
// zero. Negative requests cannot survive Kubernetes API validation, but a
// buggy collector must not be able to inflate capacity: subtracting a negative
// request would *grow* a node's Free and let the simulator promise room that
// does not exist. Used by every accounting path so place/remove stay symmetric.
func requestsOf(p *model.PodSpec) model.Resources {
	req := p.Requests()
	if req.MilliCPU < 0 {
		req.MilliCPU = 0
	}
	if req.MemoryBytes < 0 {
		req.MemoryBytes = 0
	}
	return req
}

// extendedOf returns p's summed extended requests with non-positive entries
// dropped, for the same reason requestsOf clamps: a negative GPU request must
// not mint free GPUs on a node that has none.
func extendedOf(p *model.PodSpec) map[string]int64 {
	ext := p.ExtendedRequests()
	for k, v := range ext {
		if v <= 0 {
			delete(ext, k)
		}
	}
	return ext
}

// Pods returns the pods currently assigned to this node (unordered).
func (n *NodeState) Pods() []*model.PodSpec {
	out := make([]*model.PodSpec, 0, len(n.pods))
	for _, p := range n.pods {
		out = append(out, p)
	}
	return out
}

// ClusterState is a mutable scheduling simulation. Not safe for concurrent use.
type ClusterState struct {
	nodes     []*NodeState
	nodeIndex map[string]*NodeState
	// workload → topologyKey → domain → pod count. Backs both self-anti-affinity
	// and workload topology spread.
	topoCounts map[string]map[string]map[string]int
	maxPods    int
}

// NewClusterState builds a simulation from existing nodes and pods. Existing
// pods are force-placed onto their nodes without feasibility checks (the
// cluster is ground truth, even when drifted); pending pods (no node) and pods
// referencing a node absent from nodes are ignored here and can be scheduled
// explicitly.
func NewClusterState(nodes []model.NodeSpec, pods []model.PodSpec) *ClusterState {
	cs := &ClusterState{
		nodeIndex:  map[string]*NodeState{},
		topoCounts: map[string]map[string]map[string]int{},
		maxPods:    DefaultMaxPodsPerNode,
	}
	for _, n := range nodes {
		cs.AddNode(n)
	}
	for i := range pods {
		p := &pods[i]
		if p.NodeName == "" {
			continue
		}
		if ns, ok := cs.nodeIndex[p.NodeName]; ok {
			cs.forcePlace(p, ns)
		}
	}
	return cs
}

// AddNode inserts a node into the simulation. Re-adding an existing name
// replaces the previous node entirely — its pods leave the simulation, exactly
// as RemoveNode followed by AddNode — so the index and node list can never
// disagree about which state backs a name.
func (cs *ClusterState) AddNode(spec model.NodeSpec) *NodeState {
	if _, exists := cs.nodeIndex[spec.Name]; exists {
		cs.RemoveNode(spec.Name) // cannot fail: the name is indexed
	}
	ns := &NodeState{
		Spec:    spec,
		Free:    spec.Allocatable,
		MaxPods: cs.maxPods,
		pods:    map[string]*model.PodSpec{},
	}
	if len(spec.ExtendedAllocatable) > 0 {
		ns.FreeExt = map[string]int64{}
		for k, v := range spec.ExtendedAllocatable {
			ns.FreeExt[k] = v
		}
	}
	// cs.nodes stays sorted by name; splice into position rather than re-sorting
	// the whole slice on every add.
	idx := sort.Search(len(cs.nodes), func(i int) bool { return cs.nodes[i].Spec.Name >= spec.Name })
	cs.nodes = append(cs.nodes, nil)
	copy(cs.nodes[idx+1:], cs.nodes[idx:])
	cs.nodes[idx] = ns
	cs.nodeIndex[spec.Name] = ns
	return ns
}

// Node returns the state for a node name.
func (cs *ClusterState) Node(name string) (*NodeState, bool) {
	ns, ok := cs.nodeIndex[name]
	return ns, ok
}

// Nodes returns node states sorted by name.
func (cs *ClusterState) Nodes() []*NodeState { return cs.nodes }

// forcePlace assigns p to ns without feasibility checks and reports whether it
// was placed. A UID already on the node is left untouched: overwriting the map
// entry while decrementing Free twice would corrupt accounting forever.
func (cs *ClusterState) forcePlace(p *model.PodSpec, ns *NodeState) bool {
	if _, dup := ns.pods[p.UID]; dup {
		return false
	}
	ns.pods[p.UID] = p
	ns.PodCount++
	ns.Free = ns.Free.Sub(requestsOf(p))
	for k, v := range extendedOf(p) {
		if ns.FreeExt == nil {
			ns.FreeExt = map[string]int64{}
		}
		ns.FreeExt[k] -= v
	}
	cs.adjustTopo(p, &ns.Spec, +1)
	return true
}

// Place assigns a pod to a node, updating capacity and topology counts.
// Callers should have verified Fits first; Place does not re-check, but it
// does reject a UID that is already on the node.
func (cs *ClusterState) Place(p *model.PodSpec, nodeName string) error {
	ns, ok := cs.nodeIndex[nodeName]
	if !ok {
		return fmt.Errorf("binpack: unknown node %q", nodeName)
	}
	if !cs.forcePlace(p, ns) {
		return fmt.Errorf("binpack: pod %q already on node %q", p.UID, nodeName)
	}
	return nil
}

// Remove unassigns a pod from its node.
func (cs *ClusterState) Remove(podUID, nodeName string) error {
	ns, ok := cs.nodeIndex[nodeName]
	if !ok {
		return fmt.Errorf("binpack: unknown node %q", nodeName)
	}
	p, ok := ns.pods[podUID]
	if !ok {
		return fmt.Errorf("binpack: pod %q not on node %q", podUID, nodeName)
	}
	delete(ns.pods, podUID)
	ns.PodCount--
	// Mirror forcePlace exactly (same clamping) so remove restores precisely
	// what place consumed.
	ns.Free = ns.Free.Add(requestsOf(p))
	for k, v := range extendedOf(p) {
		ns.FreeExt[k] += v
	}
	cs.adjustTopo(p, &ns.Spec, -1)
	return nil
}

// RemoveNode deletes a node from the simulation, returning its pods.
func (cs *ClusterState) RemoveNode(name string) ([]*model.PodSpec, error) {
	ns, ok := cs.nodeIndex[name]
	if !ok {
		return nil, fmt.Errorf("binpack: unknown node %q", name)
	}
	pods := ns.Pods()
	for _, p := range pods {
		if err := cs.Remove(p.UID, name); err != nil {
			return nil, err
		}
	}
	delete(cs.nodeIndex, name)
	for i, n := range cs.nodes {
		if n == ns {
			cs.nodes = append(cs.nodes[:i], cs.nodes[i+1:]...)
			break
		}
	}
	return pods, nil
}

func (cs *ClusterState) adjustTopo(p *model.PodSpec, node *model.NodeSpec, delta int) {
	if len(p.AntiAffinityKeys) == 0 && len(p.TopologySpread) == 0 {
		return // hot path: unconstrained pods need no topology bookkeeping
	}
	wl := p.Workload.String()
	keys := map[string]bool{}
	for _, k := range p.AntiAffinityKeys {
		keys[k] = true
	}
	for _, c := range p.TopologySpread {
		keys[c.TopologyKey] = true
	}
	for k := range keys {
		domain, ok := node.Labels[k]
		if !ok {
			continue
		}
		byKey := cs.topoCounts[wl]
		if byKey == nil {
			byKey = map[string]map[string]int{}
			cs.topoCounts[wl] = byKey
		}
		byDomain := byKey[k]
		if byDomain == nil {
			byDomain = map[string]int{}
			byKey[k] = byDomain
		}
		byDomain[domain] += delta
		if byDomain[domain] <= 0 {
			delete(byDomain, domain)
		}
	}
}

// workloadCount returns pods of workload wl in the topology domain.
func (cs *ClusterState) workloadCount(wl, topoKey, domain string) int {
	return cs.topoCounts[wl][topoKey][domain]
}

// Fits reports whether the pod can schedule onto the node right now.
// A nil error means it fits; otherwise the error explains the first failure.
func (cs *ClusterState) Fits(p *model.PodSpec, nodeName string) error {
	ns, ok := cs.nodeIndex[nodeName]
	if !ok {
		return fmt.Errorf("binpack: unknown node %q", nodeName)
	}
	return cs.fits(p, ns)
}

func (cs *ClusterState) fits(p *model.PodSpec, ns *NodeState) error {
	node := &ns.Spec
	if !node.Ready {
		return fmt.Errorf("node not ready")
	}
	if node.Unschedulable {
		return fmt.Errorf("node unschedulable (cordoned)")
	}
	if ns.PodCount >= ns.MaxPods {
		return fmt.Errorf("node pod limit %d reached", ns.MaxPods)
	}
	if req := requestsOf(p); !ns.Free.Fits(req) {
		return fmt.Errorf("insufficient free resources: need %s, free %s", req, ns.Free)
	}
	for k, v := range extendedOf(p) {
		if ns.FreeExt[k] < v {
			return fmt.Errorf("insufficient %s: need %d, free %d", k, v, ns.FreeExt[k])
		}
	}
	for k, v := range p.NodeSelector {
		if node.Labels[k] != v {
			return fmt.Errorf("nodeSelector %s=%s not satisfied", k, v)
		}
	}
	if len(p.RequiredAffinity) > 0 && !matchAffinity(p.RequiredAffinity, node.Labels) {
		return fmt.Errorf("required node affinity not satisfied")
	}
	for _, taint := range node.Taints {
		if taint.Effect != "NoSchedule" && taint.Effect != "NoExecute" {
			continue
		}
		tolerated := false
		for _, tol := range p.Tolerations {
			if tol.Tolerates(taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return fmt.Errorf("untolerated taint %s=%s:%s", taint.Key, taint.Value, taint.Effect)
		}
	}
	wl := p.Workload.String()
	for _, key := range p.AntiAffinityKeys {
		domain, ok := node.Labels[key]
		if !ok {
			continue
		}
		if cs.workloadCount(wl, key, domain) > 0 {
			return fmt.Errorf("anti-affinity: %s already has a %s pod in %s=%s", wl, p.Workload.Name, key, domain)
		}
	}
	for _, c := range p.TopologySpread {
		if c.WhenUnsatisfiable != "DoNotSchedule" {
			continue
		}
		domain, ok := node.Labels[c.TopologyKey]
		if !ok {
			continue // nodes without the key are unconstrained
		}
		if err := cs.checkSkew(wl, c, domain); err != nil {
			return err
		}
	}
	return nil
}

// checkSkew enforces maxSkew for a workload spread constraint if the pod were
// placed into domain. Skew = count(domain)+1 − min(count over all domains
// present on schedulable nodes).
func (cs *ClusterState) checkSkew(wl string, c model.TopologySpreadConstraint, domain string) error {
	counts := cs.topoCounts[wl][c.TopologyKey]
	minCount := -1
	seen := map[string]bool{}
	for _, ns := range cs.nodes {
		d, ok := ns.Spec.Labels[c.TopologyKey]
		if !ok || seen[d] || !ns.Spec.Ready || ns.Spec.Unschedulable {
			continue
		}
		seen[d] = true
		cnt := counts[d]
		if minCount < 0 || cnt < minCount {
			minCount = cnt
		}
	}
	if minCount < 0 {
		minCount = 0
	}
	// Kubernetes API validation requires maxSkew >= 1. Placing into the least-
	// loaded domain always yields skew 1 here, so a garbage maxSkew <= 0 would
	// reject every placement; treat it as the strictest valid constraint instead.
	maxSkew := int(c.MaxSkew)
	if maxSkew < 1 {
		maxSkew = 1
	}
	after := counts[domain] + 1
	if after-minCount > maxSkew {
		return fmt.Errorf("topology spread: placing in %s=%s gives skew %d > maxSkew %d",
			c.TopologyKey, domain, after-minCount, maxSkew)
	}
	return nil
}

func matchAffinity(terms []model.NodeSelectorTerm, labels map[string]string) bool {
	for _, term := range terms { // terms are ORed
		if matchTerm(term, labels) {
			return true
		}
	}
	return false
}

func matchTerm(term model.NodeSelectorTerm, labels map[string]string) bool {
	for _, req := range term.MatchExpressions { // requirements are ANDed
		if !matchRequirement(req, labels) {
			return false
		}
	}
	return true
}

func matchRequirement(req model.NodeSelectorRequirement, labels map[string]string) bool {
	val, exists := labels[req.Key]
	switch req.Operator {
	case "In":
		if !exists {
			return false
		}
		for _, v := range req.Values {
			if v == val {
				return true
			}
		}
		return false
	case "NotIn":
		if !exists {
			return true
		}
		for _, v := range req.Values {
			if v == val {
				return false
			}
		}
		return true
	case "Exists":
		return exists
	case "DoesNotExist":
		return !exists
	case "Gt", "Lt":
		if !exists || len(req.Values) != 1 {
			return false
		}
		nodeV, err1 := strconv.ParseInt(val, 10, 64)
		reqV, err2 := strconv.ParseInt(req.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if req.Operator == "Gt" {
			return nodeV > reqV
		}
		return nodeV < reqV
	}
	return false
}

// Unschedulable describes a pod that could not be placed anywhere.
type Unschedulable struct {
	Pod     *model.PodSpec
	Reasons []string // one representative reason per distinct failure
}

// Schedule places pods into the cluster state using best-fit-decreasing:
// pods sorted by dominant resource descending; each goes to the fitting node
// that leaves the least normalized capacity behind (tightest fit). Mutates cs.
// Returns assignments (pod UID → node name) and pods that fit nowhere.
// Nil entries are ignored; a repeated UID is reported unschedulable rather
// than double-booked.
func (cs *ClusterState) Schedule(pods []*model.PodSpec) (map[string]string, []Unschedulable) {
	sorted := make([]*model.PodSpec, 0, len(pods))
	seen := make(map[string]bool, len(pods))
	var failed []Unschedulable
	for _, p := range pods {
		if p == nil {
			continue
		}
		if seen[p.UID] {
			failed = append(failed, Unschedulable{Pod: p, Reasons: []string{"duplicate pod UID"}})
			continue
		}
		seen[p.UID] = true
		sorted = append(sorted, p)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := dominantShare(sorted[i]), dominantShare(sorted[j])
		if a != b {
			return a > b
		}
		return sorted[i].UID < sorted[j].UID
	})

	assignments := map[string]string{}
	for _, p := range sorted {
		best, reasons := cs.bestNode(p)
		if best == nil {
			failed = append(failed, Unschedulable{Pod: p, Reasons: dedupe(reasons)})
			continue
		}
		cs.forcePlace(p, best)
		assignments[p.UID] = best.Spec.Name
	}
	return assignments, failed
}

// bestNode returns the tightest-fit node for p, or nil with failure reasons.
func (cs *ClusterState) bestNode(p *model.PodSpec) (*NodeState, []string) {
	var best *NodeState
	bestScore := 0.0
	var reasons []string
	req := requestsOf(p)
	for _, ns := range cs.nodes {
		if err := cs.fits(p, ns); err != nil {
			reasons = append(reasons, ns.Spec.Name+": "+err.Error())
			continue
		}
		score := leftoverScore(ns, req)
		if best == nil || score < bestScore ||
			(score == bestScore && ns.Spec.Name < best.Spec.Name) {
			best = ns
			bestScore = score
		}
	}
	return best, reasons
}

// leftoverScore is the normalized free capacity remaining after placement;
// lower = tighter fit.
func leftoverScore(ns *NodeState, req model.Resources) float64 {
	score := 0.0
	if a := ns.Spec.Allocatable.MilliCPU; a > 0 {
		score += float64(ns.Free.MilliCPU-req.MilliCPU) / float64(a)
	}
	if a := ns.Spec.Allocatable.MemoryBytes; a > 0 {
		score += float64(ns.Free.MemoryBytes-req.MemoryBytes) / float64(a)
	}
	return score
}

// dominantShare normalizes a pod's requests to a single sortable magnitude
// using the conventional 1 vCPU ≈ 4 GiB exchange rate.
func dominantShare(p *model.PodSpec) float64 {
	req := requestsOf(p)
	cpu := float64(req.MilliCPU)
	mem := float64(req.MemoryBytes) / float64(4<<30) * 1000
	if cpu > mem {
		return cpu
	}
	return mem
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) > 5 {
		out = out[:5] // keep reports readable
	}
	return out
}
