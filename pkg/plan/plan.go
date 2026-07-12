// Package plan turns learned state into an executable, auditable rebalance
// plan. A plan is a strictly ordered list of steps — resize, cordon, evict,
// delete — each carrying its own risk note, plus the money math: what the
// cluster costs now, what it will cost after, and the theoretical floor a
// greenfield repack could reach.
//
// Plans are pure data: building one never touches a cluster. The controller
// executes steps; `kilter plan` prints them; the simulator replays them.
package plan

import (
	"fmt"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/binpack"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
	"github.com/agenticode/kilter/pkg/safety"
)

// Config tunes plan generation.
type Config struct {
	// MinNodeUtilization: nodes whose dominant requested-resource share is
	// below this become consolidation candidates. Default 0.5.
	MinNodeUtilization float64
	// MinConfidence filters recommendations. Default 0.6.
	MinConfidence float64
	// MaxNodeRemovals bounds disruption per plan. Default 3.
	MaxNodeRemovals int
	// ApplyRecommendations virtually rightsizes pods before consolidation,
	// which is what unlocks most node removals. Default true (set via New*).
	ApplyRecommendations bool
	// MinClusterHeadroom: after consolidation the remaining nodes must keep
	// at least this fraction of allocatable free, per dimension. Default 0.10.
	MinClusterHeadroom float64
	// RespectManagedNodes leaves nodes owned by another autoscaler
	// (Karpenter) out of consolidation; Kilter's rightsizing shrinks their
	// pods and the owning autoscaler consolidates. Default true (via New*).
	RespectManagedNodes bool
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		MinNodeUtilization:   0.5,
		MinConfidence:        0.6,
		MaxNodeRemovals:      3,
		ApplyRecommendations: true,
		MinClusterHeadroom:   0.10,
		RespectManagedNodes:  true,
	}
}

// StepType enumerates plan step kinds.
type StepType string

const (
	StepResizeWorkload StepType = "resize-workload"
	StepCordonNode     StepType = "cordon-node"
	StepEvictPod       StepType = "evict-pod"
	StepDeleteNode     StepType = "delete-node"
)

// Risk levels for steps and plans.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// Step is one ordered action in a plan.
type Step struct {
	Seq    int      `json:"seq"`
	Type   StepType `json:"type"`
	Risk   string   `json:"risk"`
	Detail string   `json:"detail"`

	// resize-workload
	Workload  model.WorkloadRef `json:"workload,omitempty"`
	Container string            `json:"container,omitempty"`
	FromReq   model.Resources   `json:"fromRequest,omitempty"`
	ToReq     model.Resources   `json:"toRequest,omitempty"`
	FromLim   model.Resources   `json:"fromLimit,omitempty"`
	ToLim     model.Resources   `json:"toLimit,omitempty"`

	// node/pod steps
	Node       string `json:"node,omitempty"`
	Pod        string `json:"pod,omitempty"` // namespace/name
	PodUID     string `json:"podUID,omitempty"`
	TargetNode string `json:"targetNode,omitempty"` // advisory: where the sim placed it
}

// NodeRemoval summarizes one consolidated node.
type NodeRemoval struct {
	Node          string  `json:"node"`
	HourlyUSD     float64 `json:"hourlyUSD"`
	EvictedPods   int     `json:"evictedPods"`
	DaemonSetPods int     `json:"daemonSetPods"`
	Utilization   float64 `json:"utilization"` // dominant requested share before removal
	Risk          string  `json:"risk"`
}

// Plan is a complete, executable optimization decision.
type Plan struct {
	ClusterID string    `json:"clusterID"`
	CreatedAt time.Time `json:"createdAt"`

	Steps       []Step                     `json:"steps"`
	Rightsizing []recommend.Recommendation `json:"rightsizing,omitempty"`
	Removals    []NodeRemoval              `json:"removals,omitempty"`

	CurrentHourlyUSD   float64 `json:"currentHourlyUSD"`
	ProjectedHourlyUSD float64 `json:"projectedHourlyUSD"`
	SavingsMonthlyUSD  float64 `json:"savingsMonthlyUSD"`
	// GreenfieldHourlyUSD is the theoretical floor if the whole cluster were
	// repacked from scratch onto catalog shapes. Informational.
	GreenfieldHourlyUSD float64 `json:"greenfieldHourlyUSD,omitempty"`
	// ReclaimedRequests is capacity freed by rightsizing (enabler metric).
	ReclaimedRequests model.Resources `json:"reclaimedRequests,omitempty"`

	Risk  string   `json:"risk"`
	Notes []string `json:"notes,omitempty"`
}

// Empty reports whether the plan contains no actionable steps.
func (p *Plan) Empty() bool { return len(p.Steps) == 0 }

// Build computes a plan from a snapshot and current recommendations.
func Build(snap *model.ClusterSnapshot, recs []recommend.Recommendation, catalog *pricing.Catalog, cfg Config) (*Plan, error) {
	if snap == nil || catalog == nil {
		return nil, fmt.Errorf("plan: nil snapshot or catalog")
	}
	if cfg.MinNodeUtilization <= 0 {
		cfg.MinNodeUtilization = 0.5
	}
	if cfg.MinConfidence <= 0 {
		cfg.MinConfidence = 0.6
	}
	if cfg.MaxNodeRemovals <= 0 {
		cfg.MaxNodeRemovals = 3
	}
	if cfg.MinClusterHeadroom <= 0 {
		cfg.MinClusterHeadroom = 0.10
	}

	p := &Plan{ClusterID: snap.ClusterID, CreatedAt: snap.Timestamp}
	cost := catalog.SnapshotCost(snap)
	p.CurrentHourlyUSD = cost.HourlyUSD
	nodeCost := map[string]float64{}
	for _, nc := range cost.Nodes {
		nodeCost[nc.Node] = nc.HourlyUSD
	}

	// Working copies: pods get virtually resized, then consolidation runs on
	// the adjusted shapes.
	pods := make([]model.PodSpec, len(snap.Pods))
	copy(pods, snap.Pods)
	seq := 1

	if cfg.ApplyRecommendations {
		accepted := map[model.ContainerKey]recommend.Recommendation{}
		for _, r := range recs {
			if r.Confidence >= cfg.MinConfidence {
				accepted[r.Key] = r
			}
		}
		for i := range pods {
			pod := &pods[i]
			for j := range pod.Containers {
				c := &pod.Containers[j]
				key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
				r, ok := accepted[key]
				if !ok {
					continue
				}
				p.ReclaimedRequests = p.ReclaimedRequests.Add(c.Requests.Sub(r.TargetRequest))
				c.Requests = r.TargetRequest
				if r.TargetLimit.MilliCPU > 0 || r.TargetLimit.MemoryBytes > 0 {
					c.Limits = r.TargetLimit
				}
			}
		}
		// One resize step per accepted recommendation, ordered by workload.
		keys := make([]model.ContainerKey, 0, len(accepted))
		for k := range accepted {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			r := accepted[k]
			p.Rightsizing = append(p.Rightsizing, r)
			p.Steps = append(p.Steps, Step{
				Seq: seq, Type: StepResizeWorkload, Risk: RiskLow,
				Workload: k.Workload, Container: k.Container,
				FromReq: r.CurrentRequest, ToReq: r.TargetRequest,
				FromLim: r.CurrentLimit, ToLim: r.TargetLimit,
				Detail: fmt.Sprintf("%s → %s (confidence %.2f; %s)",
					r.CurrentRequest, r.TargetRequest, r.Confidence, r.Reason),
			})
			seq++
		}
	}

	// Consolidation: authoritative sim arrays evolve as removals are accepted.
	nodes := make([]model.NodeSpec, len(snap.Nodes))
	copy(nodes, snap.Nodes)
	pdbGuard := safety.NewPDBGuard(snap.PDBs)

	type candidate struct {
		name string
		util float64
		cost float64
	}
	utilization := func(nodes []model.NodeSpec, pods []model.PodSpec, name string) float64 {
		var used model.Resources
		var alloc model.Resources
		for i := range nodes {
			if nodes[i].Name == name {
				alloc = nodes[i].Allocatable
			}
		}
		for i := range pods {
			if pods[i].NodeName == name {
				used = used.Add(pods[i].Requests())
			}
		}
		if alloc.MilliCPU == 0 || alloc.MemoryBytes == 0 {
			return 1
		}
		cpu := float64(used.MilliCPU) / float64(alloc.MilliCPU)
		mem := float64(used.MemoryBytes) / float64(alloc.MemoryBytes)
		if cpu > mem {
			return cpu
		}
		return mem
	}

	removed := 0
	for removed < cfg.MaxNodeRemovals {
		// Rank current candidates each round on the evolving state.
		var cands []candidate
		for i := range nodes {
			n := &nodes[i]
			if !n.Ready || n.Unschedulable || isControlPlane(n) {
				continue
			}
			if cfg.RespectManagedNodes && n.ManagedBy != "" {
				continue // the owning autoscaler consolidates; our resizes feed it
			}
			util := utilization(nodes, pods, n.Name)
			if util >= cfg.MinNodeUtilization {
				continue
			}
			blocked := false
			for j := range pods {
				if pods[j].NodeName != n.Name {
					continue
				}
				if blocks, _ := safety.BlocksDrain(&pods[j]); blocks {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			cands = append(cands, candidate{name: n.Name, util: util, cost: nodeCost[n.Name]})
		}
		if len(cands) == 0 {
			break
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].util != cands[j].util {
				return cands[i].util < cands[j].util
			}
			if cands[i].cost != cands[j].cost {
				return cands[i].cost > cands[j].cost
			}
			return cands[i].name < cands[j].name
		})

		accepted := false
		for _, cand := range cands {
			newNodes, newPods, steps, removal, ok := tryRemove(nodes, pods, cand.name, cand.util, nodeCost[cand.name], pdbGuard, cfg, &seq)
			if !ok {
				continue
			}
			nodes, pods = newNodes, newPods
			p.Steps = append(p.Steps, steps...)
			p.Removals = append(p.Removals, removal)
			removed++
			accepted = true
			break
		}
		if !accepted {
			break
		}
	}

	// Money math.
	p.ProjectedHourlyUSD = p.CurrentHourlyUSD
	for _, r := range p.Removals {
		p.ProjectedHourlyUSD -= r.HourlyUSD
	}
	p.SavingsMonthlyUSD = (p.CurrentHourlyUSD - p.ProjectedHourlyUSD) * pricing.HoursPerMonth

	// Greenfield floor: repack every workload pod (post-resize) from scratch.
	p.GreenfieldHourlyUSD = greenfieldFloor(snap, pods, catalog)

	if cfg.RespectManagedNodes {
		managed := 0
		for i := range snap.Nodes {
			if snap.Nodes[i].ManagedBy != "" {
				managed++
			}
		}
		if managed > 0 {
			p.Notes = append(p.Notes, fmt.Sprintf(
				"%d node(s) are managed by karpenter: left to its consolidation (kilter's rightsizing feeds it); set RespectManagedNodes=false to override", managed))
		}
	}
	p.Risk = planRisk(p)
	if p.Empty() {
		p.Notes = append(p.Notes, "cluster is already in kilter: no beneficial changes found")
	}
	return p, nil
}

// tryRemove simulates draining one node. On success it returns the evolved
// nodes/pods arrays, the plan steps, and a removal summary.
func tryRemove(nodes []model.NodeSpec, pods []model.PodSpec, name string, util, hourly float64,
	guard *safety.PDBGuard, cfg Config, seq *int) ([]model.NodeSpec, []model.PodSpec, []Step, NodeRemoval, bool) {

	cs := binpack.NewClusterState(nodes, pods)
	moved, err := cs.RemoveNode(name)
	if err != nil {
		return nil, nil, nil, NodeRemoval{}, false
	}

	var evictable []*model.PodSpec
	dsCount := 0
	for _, p := range moved {
		if p.Workload.Kind == model.KindDaemonSet {
			dsCount++
			continue
		}
		evictable = append(evictable, p)
	}

	// PDB dry check before reserving anything.
	for _, p := range evictable {
		if ok, _ := guard.CanEvict(p); !ok {
			return nil, nil, nil, NodeRemoval{}, false
		}
	}

	assign, failed := cs.Schedule(evictable)
	if len(failed) > 0 {
		return nil, nil, nil, NodeRemoval{}, false
	}

	// Headroom guard: remaining cluster keeps MinClusterHeadroom free.
	var free, alloc model.Resources
	for _, ns := range cs.Nodes() {
		free = free.Add(ns.Free)
		alloc = alloc.Add(ns.Spec.Allocatable)
	}
	if alloc.MilliCPU > 0 && float64(free.MilliCPU)/float64(alloc.MilliCPU) < cfg.MinClusterHeadroom {
		return nil, nil, nil, NodeRemoval{}, false
	}
	if alloc.MemoryBytes > 0 && float64(free.MemoryBytes)/float64(alloc.MemoryBytes) < cfg.MinClusterHeadroom {
		return nil, nil, nil, NodeRemoval{}, false
	}

	// Commit: reserve PDB disruptions; evolve arrays; emit steps.
	for _, p := range evictable {
		if ok, _ := guard.Reserve(p); !ok {
			return nil, nil, nil, NodeRemoval{}, false // raced budget; abort removal
		}
	}

	newNodes := make([]model.NodeSpec, 0, len(nodes)-1)
	for _, n := range nodes {
		if n.Name != name {
			newNodes = append(newNodes, n)
		}
	}
	newPods := make([]model.PodSpec, 0, len(pods))
	for _, p := range pods {
		if p.NodeName != name {
			newPods = append(newPods, p)
			continue
		}
		if p.Workload.Kind == model.KindDaemonSet {
			continue // dies with the node
		}
		p.NodeName = assign[p.UID]
		newPods = append(newPods, p)
	}

	risk := RiskLow
	if len(evictable) > 8 {
		risk = RiskMedium
	}
	if len(evictable) > 20 {
		risk = RiskHigh
	}

	steps := []Step{{
		Seq: *seq, Type: StepCordonNode, Node: name, Risk: RiskLow,
		Detail: fmt.Sprintf("cordon %s (%.0f%% utilized, $%.3f/h)", name, util*100, hourly),
	}}
	*seq++
	sort.Slice(evictable, func(i, j int) bool { return evictable[i].UID < evictable[j].UID })
	for _, p := range evictable {
		steps = append(steps, Step{
			Seq: *seq, Type: StepEvictPod, Node: name, Risk: risk,
			Pod: p.Namespace + "/" + p.Name, PodUID: p.UID, TargetNode: assign[p.UID],
			Detail: fmt.Sprintf("evict %s/%s (sim target: %s)", p.Namespace, p.Name, assign[p.UID]),
		})
		*seq++
	}
	steps = append(steps, Step{
		Seq: *seq, Type: StepDeleteNode, Node: name, Risk: risk,
		Detail: fmt.Sprintf("remove empty node %s: saves $%.2f/month", name, hourly*pricing.HoursPerMonth),
	})
	*seq++

	return newNodes, newPods, steps, NodeRemoval{
		Node: name, HourlyUSD: hourly, EvictedPods: len(evictable),
		DaemonSetPods: dsCount, Utilization: util, Risk: risk,
	}, true
}

// greenfieldFloor prices a from-scratch repack of all workload pods.
func greenfieldFloor(snap *model.ClusterSnapshot, pods []model.PodSpec, catalog *pricing.Catalog) float64 {
	provider, arch := dominantProviderArch(snap)
	cands := catalog.Candidates(provider, arch)
	if len(cands) == 0 {
		return 0
	}
	var workload []*model.PodSpec
	var dsPods []model.PodSpec
	for i := range pods {
		if pods[i].NodeName == "" {
			continue
		}
		if pods[i].Workload.Kind == model.KindDaemonSet {
			continue
		}
		workload = append(workload, &pods[i])
	}
	// One DS overhead template per daemonset (they replicate per node).
	seen := map[model.WorkloadRef]bool{}
	for i := range pods {
		if pods[i].Workload.Kind == model.KindDaemonSet && !seen[pods[i].Workload] {
			seen[pods[i].Workload] = true
			dsPods = append(dsPods, pods[i])
		}
	}
	gp := binpack.PlanNodes(workload, cands, binpack.PlanOptions{DaemonSetPods: dsPods})
	if len(gp.Unschedulable) > 0 {
		return 0 // floor not meaningful if the repack can't hold everything
	}
	return gp.TotalHourlyUSD
}

func dominantProviderArch(snap *model.ClusterSnapshot) (string, string) {
	pc, ac := map[string]int{}, map[string]int{}
	for i := range snap.Nodes {
		pc[snap.Nodes[i].Provider]++
		ac[snap.Nodes[i].Labels["kubernetes.io/arch"]]++
	}
	return maxKey(pc), maxKey(ac)
}

func maxKey(m map[string]int) string {
	best, bestN := "", -1
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

func isControlPlane(n *model.NodeSpec) bool {
	_, cp := n.Labels["node-role.kubernetes.io/control-plane"]
	_, master := n.Labels["node-role.kubernetes.io/master"]
	return cp || master
}

func planRisk(p *Plan) string {
	risk := RiskLow
	for _, s := range p.Steps {
		if s.Risk == RiskHigh {
			return RiskHigh
		}
		if s.Risk == RiskMedium {
			risk = RiskMedium
		}
	}
	return risk
}
