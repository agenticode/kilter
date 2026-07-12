package plan

import (
	"fmt"
	"sort"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/safety"
)

// SpotSafety classifies one workload's fitness for spot capacity.
type SpotSafety struct {
	Workload model.WorkloadRef `json:"workload"`
	Safe     bool              `json:"safe"`
	Reasons  []string          `json:"reasons,omitempty"` // why NOT safe
	Replicas int               `json:"replicas"`
	Requests model.Resources   `json:"requests"` // total across replicas
	// OnSpot counts replicas already running on spot nodes.
	OnSpot int `json:"onSpot"`
}

// SpotReport summarizes the cluster's spot opportunity.
type SpotReport struct {
	Workloads []SpotSafety `json:"workloads"`
	// SafeRequests totals requests of spot-safe pods currently on on-demand nodes.
	SafeRequests model.Resources `json:"safeRequests"`
	// EstMonthlySavingsUSD prices SafeRequests at the catalog's typical spot
	// discount for the cluster's dominant provider. An estimate, not a quote.
	EstMonthlySavingsUSD float64 `json:"estMonthlySavingsUSD"`
	DiscountApplied      float64 `json:"discountApplied"` // e.g. 0.65 = 65% off
}

// BuildSpotReport scores every multi-replica workload for spot safety.
// Safety rules (all must hold):
//   - ≥ minReplicas replicas (interruption tolerance needs redundancy)
//   - every pod evictable (no bare pods, local storage, do-not-evict)
//   - PDBs covering the pods currently allow ≥1 disruption
//   - not a StatefulSet (interruption ≠ graceful scale-in for stateful apps)
func BuildSpotReport(snap *model.ClusterSnapshot, catalog *pricing.Catalog, minReplicas int) SpotReport {
	if minReplicas < 2 {
		minReplicas = 2
	}
	nodes := snap.NodesByName()
	byWorkload := map[model.WorkloadRef][]*model.PodSpec{}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Phase != "" && p.Phase != "Running" {
			continue
		}
		byWorkload[p.Workload] = append(byWorkload[p.Workload], p)
	}
	guard := safety.NewPDBGuard(snap.PDBs)

	rep := SpotReport{}
	for ref, pods := range byWorkload {
		s := SpotSafety{Workload: ref, Replicas: len(pods)}
		var reqOnDemand model.Resources
		for _, p := range pods {
			if n, ok := nodes[p.NodeName]; ok && n.Spot {
				s.OnSpot++
			} else {
				reqOnDemand = reqOnDemand.Add(p.Requests())
			}
			s.Requests = s.Requests.Add(p.Requests())
		}

		switch {
		case ref.Kind == model.KindDaemonSet:
			continue // follows nodes; not a placement decision
		case ref.Kind == model.KindStatefulSet:
			s.Reasons = append(s.Reasons, "statefulset: interruption is not graceful scale-in")
		case ref.Kind == model.KindJob, ref.Kind == model.KindCronJob, ref.Kind == model.KindBarePod:
			s.Reasons = append(s.Reasons, string(ref.Kind)+": restart semantics unclear")
		case len(pods) < minReplicas:
			s.Reasons = append(s.Reasons, fmt.Sprintf("only %d replica(s): no interruption redundancy", len(pods)))
		}
		for _, p := range pods {
			if ev := safety.CanEvict(p); !ev.OK {
				s.Reasons = append(s.Reasons, ev.Reason)
				break
			}
		}
		if len(s.Reasons) == 0 {
			if ok, why := guard.CanEvict(pods[0]); !ok {
				s.Reasons = append(s.Reasons, why)
			}
		}
		s.Safe = len(s.Reasons) == 0
		if s.Safe {
			rep.SafeRequests = rep.SafeRequests.Add(reqOnDemand)
		}
		rep.Workloads = append(rep.Workloads, s)
	}
	sort.Slice(rep.Workloads, func(i, j int) bool {
		if rep.Workloads[i].Safe != rep.Workloads[j].Safe {
			return rep.Workloads[i].Safe
		}
		return rep.Workloads[i].Workload.String() < rep.Workloads[j].Workload.String()
	})

	rep.DiscountApplied = typicalSpotDiscount(snap, catalog)
	hourly := float64(rep.SafeRequests.MilliCPU)/1000*pricing.FallbackCPUHourlyUSD +
		float64(rep.SafeRequests.MemoryBytes)/(1<<30)*pricing.FallbackGiBHourlyUSD
	rep.EstMonthlySavingsUSD = hourly * rep.DiscountApplied * pricing.HoursPerMonth
	return rep
}

// typicalSpotDiscount averages the catalog's spot discount for the cluster's
// dominant provider; falls back to the industry-typical 65%.
func typicalSpotDiscount(snap *model.ClusterSnapshot, catalog *pricing.Catalog) float64 {
	provider, _ := dominantProviderArch(snap)
	sum, n := 0.0, 0
	for _, it := range catalog.Candidates(provider, "") {
		if it.SpotHourlyUSD > 0 {
			sum += 1 - it.SpotHourlyUSD/it.HourlyUSD
			n++
		}
	}
	if n == 0 {
		return 0.65
	}
	return sum / float64(n)
}

// SpotInterruptionTaints are the taints interruption handlers place on nodes
// about to be reclaimed (AWS Node Termination Handler, Karpenter, GKE).
var SpotInterruptionTaints = []string{
	"aws-node-termination-handler/spot-itn",
	"aws-node-termination-handler/rebalance-recommendation",
	"karpenter.sh/disrupted",
	"karpenter.sh/disruption",
	"cloud.google.com/impending-node-termination",
}

// InterruptedSpotNodes returns nodes flagged for imminent reclamation.
// Controllers fast-track their drains: cooldowns don't apply (the machine is
// dying either way), but PDBs still do.
func InterruptedSpotNodes(snap *model.ClusterSnapshot) []string {
	var out []string
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		for _, t := range n.Taints {
			for _, known := range SpotInterruptionTaints {
				if t.Key == known {
					out = append(out, n.Name)
					goto next
				}
			}
		}
	next:
	}
	sort.Strings(out)
	return out
}

// EmergencyDrainPlan builds a minimal cordon+evict plan for an interrupted
// node. It does NOT delete the node (the cloud reclaims it) and does not
// simulate placement (there is no time; the scheduler will place evictees).
func EmergencyDrainPlan(snap *model.ClusterSnapshot, node string) *Plan {
	p := &Plan{ClusterID: snap.ClusterID, CreatedAt: snap.Timestamp, Risk: RiskMedium}
	seq := 1
	p.Steps = append(p.Steps, Step{
		Seq: seq, Type: StepCordonNode, Node: node, Risk: RiskLow,
		Detail: fmt.Sprintf("spot interruption: cordon %s ahead of reclamation", node),
	})
	seq++
	var pods []*model.PodSpec
	for i := range snap.Pods {
		if snap.Pods[i].NodeName == node {
			pods = append(pods, &snap.Pods[i])
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].UID < pods[j].UID })
	for _, pod := range pods {
		if pod.Workload.Kind == model.KindDaemonSet {
			continue
		}
		if ev := safety.CanEvict(pod); !ev.OK && !pod.DoNotEvict {
			// Even normally-pinned pods are better restarted proactively than
			// killed by the hypervisor — but explicit opt-outs stay respected.
			continue
		} else if pod.DoNotEvict {
			continue
		}
		p.Steps = append(p.Steps, Step{
			Seq: seq, Type: StepEvictPod, Node: node, Risk: RiskMedium,
			Pod: pod.Namespace + "/" + pod.Name, PodUID: pod.UID,
			Detail: fmt.Sprintf("evict %s/%s before spot reclamation", pod.Namespace, pod.Name),
		})
		seq++
	}
	p.Notes = append(p.Notes, "emergency spot drain: cooldowns bypassed, PDBs still enforced by the eviction API")
	return p
}
