package binpack

import (
	"fmt"
	"sort"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// PlanOptions tunes cheapest-node-set planning.
type PlanOptions struct {
	// SystemReservedFraction approximates kubelet/system reservation when
	// deriving allocatable from a candidate's capacity. Default 0.08.
	SystemReservedFraction float64
	// MaxPodsPerNode caps pods per planned node. Default 110.
	MaxPodsPerNode int
	// DaemonSetPods are replicated onto every planned node that they fit,
	// consuming capacity before workload pods are packed.
	DaemonSetPods []model.PodSpec
	// NodeLabels are extra labels every planned node carries (e.g. zone),
	// on top of synthesized arch/instance-type/os labels.
	NodeLabels map[string]string
	// Spot prices candidates at their spot rate where available.
	Spot bool
	// AllowBurstable admits credit-based CPU shapes (t3, B-series) into plans.
	// Off by default: their price is only real if you don't burn the CPU.
	AllowBurstable bool
	// MaxNodes hard-caps the plan size. Default 1000.
	MaxNodes int
}

func (o PlanOptions) withDefaults() PlanOptions {
	if o.SystemReservedFraction <= 0 || o.SystemReservedFraction >= 0.5 {
		o.SystemReservedFraction = 0.08
	}
	if o.MaxPodsPerNode <= 0 {
		o.MaxPodsPerNode = DefaultMaxPodsPerNode
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = 1000
	}
	return o
}

// PlannedNode is one node in a plan with its assigned pods.
type PlannedNode struct {
	Name        string               `json:"name"`
	Type        pricing.InstanceType `json:"type"`
	Spot        bool                 `json:"spot,omitempty"`
	PodUIDs     []string             `json:"podUIDs"`
	Used        model.Resources      `json:"used"`
	Allocatable model.Resources      `json:"allocatable"`
	HourlyUSD   float64              `json:"hourlyUSD"`
}

// NodePlan is the outcome of PlanNodes.
type NodePlan struct {
	Nodes           []PlannedNode   `json:"nodes"`
	TotalHourlyUSD  float64         `json:"totalHourlyUSD"`
	TotalMonthlyUSD float64         `json:"totalMonthlyUSD"`
	Unschedulable   []Unschedulable `json:"unschedulable,omitempty"`
}

// podValueUSD prices a pod's requests at fallback unit economics; used only
// as the packing-efficiency numerator, never reported as spend.
func podValueUSD(p *model.PodSpec) float64 {
	req := p.Requests()
	return float64(req.MilliCPU)/1000*pricing.FallbackCPUHourlyUSD +
		float64(req.MemoryBytes)/(1<<30)*pricing.FallbackGiBHourlyUSD
}

// synthNode fabricates the NodeSpec a fresh node of this instance type
// would register with.
func synthNode(it pricing.InstanceType, seq int, opts PlanOptions) model.NodeSpec {
	labels := map[string]string{
		"kubernetes.io/os":                 "linux",
		"kubernetes.io/arch":               it.Arch,
		"node.kubernetes.io/instance-type": it.Name,
		"kilter.dev/planned":               "true",
	}
	for k, v := range opts.NodeLabels {
		labels[k] = v
	}
	cap := it.Resources()
	alloc := model.Resources{
		MilliCPU:    int64(float64(cap.MilliCPU) * (1 - opts.SystemReservedFraction)),
		MemoryBytes: int64(float64(cap.MemoryBytes) * (1 - opts.SystemReservedFraction)),
	}
	return model.NodeSpec{
		Name:         fmt.Sprintf("planned-%s-%d", it.Name, seq),
		Labels:       labels,
		Capacity:     cap,
		Allocatable:  alloc,
		Ready:        true,
		InstanceType: it.Name,
		Provider:     it.Provider,
		Spot:         opts.Spot,
	}
}

// trialFill opens one empty node of the given type, applies DaemonSet
// overhead, then packs as many of the (pre-sorted, descending) pods as fit.
// Returns the packed pod indexes and the used resources.
func trialFill(it pricing.InstanceType, seq int, sortedPods []*model.PodSpec, opts PlanOptions) (packedIdx []int, used model.Resources, node model.NodeSpec) {
	node = synthNode(it, seq, opts)
	cs := NewClusterState([]model.NodeSpec{node}, nil)
	ns := cs.nodes[0]
	ns.MaxPods = opts.MaxPodsPerNode

	// DaemonSet overhead: each DS pod that fits consumes capacity + a pod slot.
	for i := range opts.DaemonSetPods {
		ds := opts.DaemonSetPods[i] // copy
		ds.UID = fmt.Sprintf("ds-%d-%d", seq, i)
		if cs.fits(&ds, ns) == nil {
			cs.forcePlace(&ds, ns)
		}
	}

	for i, p := range sortedPods {
		if p == nil {
			continue
		}
		if cs.fits(p, ns) == nil {
			cs.forcePlace(p, ns)
			packedIdx = append(packedIdx, i)
			used = used.Add(p.Requests())
		}
	}
	return packedIdx, used, node
}

// PlanNodes computes a low-cost node set that fits all pods. Greedy: each
// round, every candidate instance type is trial-packed against the remaining
// pods and the type with the best packed-value-per-dollar wins one node.
// Deterministic for identical inputs.
func PlanNodes(pods []*model.PodSpec, candidates []pricing.InstanceType, opts PlanOptions) NodePlan {
	opts = opts.withDefaults()
	plan := NodePlan{}
	if !opts.AllowBurstable {
		kept := make([]pricing.InstanceType, 0, len(candidates))
		for _, it := range candidates {
			if !it.Burstable {
				kept = append(kept, it)
			}
		}
		candidates = kept
	}
	if len(candidates) == 0 {
		for _, p := range pods {
			plan.Unschedulable = append(plan.Unschedulable, Unschedulable{Pod: p, Reasons: []string{"no candidate instance types"}})
		}
		return plan
	}

	// Work on a sorted copy (dominant resource descending), nil-ing out packed slots.
	remaining := make([]*model.PodSpec, len(pods))
	copy(remaining, pods)
	sort.SliceStable(remaining, func(i, j int) bool {
		a, b := dominantShare(remaining[i]), dominantShare(remaining[j])
		if a != b {
			return a > b
		}
		return remaining[i].UID < remaining[j].UID
	})
	left := len(remaining)

	seq := 0
	for left > 0 && len(plan.Nodes) < opts.MaxNodes {
		type trial struct {
			it     pricing.InstanceType
			packed []int
			used   model.Resources
			node   model.NodeSpec
			eff    float64
			price  float64
		}
		var best *trial
		for _, it := range candidates {
			packed, used, node := trialFill(it, seq, remaining, opts)
			if len(packed) == 0 {
				continue
			}
			price := it.Price(opts.Spot)
			value := 0.0
			for _, idx := range packed {
				value += podValueUSD(remaining[idx])
			}
			tr := &trial{it: it, packed: packed, used: used, node: node, eff: value / price, price: price}
			if best == nil || tr.eff > best.eff ||
				(tr.eff == best.eff && (tr.price < best.price ||
					(tr.price == best.price && tr.it.Name < best.it.Name))) {
				best = tr
			}
		}
		if best == nil {
			break // nothing packs anywhere → remaining are unschedulable
		}

		pn := PlannedNode{
			Name:        best.node.Name,
			Type:        best.it,
			Spot:        opts.Spot,
			Used:        best.used,
			Allocatable: best.node.Allocatable,
			HourlyUSD:   best.price,
		}
		for _, idx := range best.packed {
			pn.PodUIDs = append(pn.PodUIDs, remaining[idx].UID)
			remaining[idx] = nil
			left--
		}
		plan.Nodes = append(plan.Nodes, pn)
		plan.TotalHourlyUSD += best.price
		seq++

		// Compact the remaining slice periodically to keep trials fast.
		if left > 0 && left*2 < cap(remaining) {
			compact := remaining[:0]
			for _, p := range remaining {
				if p != nil {
					compact = append(compact, p)
				}
			}
			remaining = compact
		}
	}

	// Anything left could not be packed; explain against each candidate.
	for _, p := range remaining {
		if p == nil {
			continue
		}
		var reasons []string
		for _, it := range candidates {
			node := synthNode(it, 99999, opts)
			cs := NewClusterState([]model.NodeSpec{node}, nil)
			if err := cs.fits(p, cs.nodes[0]); err != nil {
				reasons = append(reasons, it.Name+": "+err.Error())
			}
		}
		if len(reasons) == 0 {
			reasons = []string{"node cap reached (MaxNodes)"}
		}
		plan.Unschedulable = append(plan.Unschedulable, Unschedulable{Pod: p, Reasons: dedupe(reasons)})
	}

	plan.TotalMonthlyUSD = plan.TotalHourlyUSD * pricing.HoursPerMonth
	return plan
}
