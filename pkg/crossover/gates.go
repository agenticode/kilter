// Non-price gates. §4.3 of docs/design/compute-domains.md lists properties
// that make EKS Fargate impossible for a workload regardless of what it costs.
// They are encoded here as hard blocks, one gate per property, because a
// penalty — "Fargate is cheaper, minus a risk score" — is exactly the shape of
// recommendation that gets a cluster broken: no amount of savings makes a
// DaemonSet run on Fargate.
//
// Two rules hold for every gate in this file:
//
//  1. Uniform polarity. A [Fact] blocks when it is [Present]. The field names
//     are therefore stated as problems ("NoPrivateSubnet"), never as
//     capabilities, so a reader cannot invert one by accident.
//  2. Unknown blocks. A property the collector did not observe is not the same
//     as a property observed absent, and it is not a licence to guess. The
//     zero [Facts] value is all-[Unknown], so a pod nobody looked at is blocked
//     by default and says so, listing exactly which observations are missing.

package crossover

import (
	"fmt"
	"sort"

	"github.com/agenticode/kilter/pkg/model"
)

// Fact is a tri-state observation of one Fargate-blocking property. The zero
// value is Unknown, which blocks: see rule 2 above.
type Fact uint8

const (
	// Unknown means the property was not observed. It blocks.
	Unknown Fact = iota
	// Absent means the property was observed and is not present. It passes.
	Absent
	// Present means the property was observed and is present. It blocks.
	Present
)

// String renders the fact for reports.
func (f Fact) String() string {
	switch f {
	case Absent:
		return "absent"
	case Present:
		return "present"
	default:
		return "unknown"
	}
}

// blocks reports whether the fact prevents a move to Fargate.
func (f Fact) blocks() bool { return f != Absent }

// Facts are the non-price properties §4.3 gates on, per pod. Every field is a
// blocker when Present and a blocker when Unknown; only Absent passes.
//
// Fields the cluster snapshot can already answer are filled by
// [FactsFromPodSpec]; the rest are Unknown until a collector fills them (see
// FINDINGS.md §"wiring"). A pod observed running on Fargate has all of them
// Absent by construction — AWS scheduling it is the observation — which is
// what [CompatibleFacts] returns.
type Facts struct {
	// DaemonSet marks a pod belonging to a DaemonSet, or a workload served by
	// one. Fargate runs no DaemonSets at all.
	DaemonSet Fact
	// ExtendedResource marks a request for a resource outside cpu/memory —
	// nvidia.com/gpu and friends. Fargate offers none.
	ExtendedResource Fact
	// EBSVolume marks an attached EBS-backed volume.
	EBSVolume Fact
	// Privileged marks a privileged container or an added Linux capability
	// Fargate does not grant.
	Privileged Fact
	// HostPath marks a hostPath or node-local volume.
	HostPath Fact
	// HostNetwork marks hostNetwork: true.
	HostNetwork Fact
	// HostPort marks a container binding a host port.
	HostPort Fact
	// NoPrivateSubnet marks a workload with no private subnet available to a
	// Fargate profile. Stated as the problem, per rule 1.
	NoPrivateSubnet Fact
	// EvictionIntolerant marks a pod that cannot tolerate being recreated.
	// Fargate has no in-place resize and evicts pods for platform patching,
	// so "never evict me" and "run on Fargate" are contradictory.
	EvictionIntolerant Fact
}

// Gate names one non-price blocker. The set is closed and ordered by
// [AllGates]; report order never depends on map iteration.
type Gate string

const (
	GateDaemonSet          Gate = "daemonset"
	GateExtendedResource   Gate = "extended-resource"
	GateEBSVolume          Gate = "ebs-volume"
	GatePrivileged         Gate = "privileged"
	GateHostPath           Gate = "host-path"
	GateHostNetwork        Gate = "host-network"
	GateHostPort           Gate = "host-port"
	GatePrivateSubnet      Gate = "private-subnet"
	GateEvictionIntolerant Gate = "eviction-intolerant"
	// GateSizeCeiling is not a Facts field: it is computed, from the pod's own
	// requests, by the same quantizer that prices the Fargate side. A pod above
	// 16 vCPU / 120 GB has no Fargate configuration to be billed at.
	GateSizeCeiling Gate = "size-ceiling"
)

// gateSpec binds a gate to the fact it reads and to why it blocks. It is a
// function, not a package-level var, so no caller can mutate the gate table.
type gateSpec struct {
	gate Gate
	fact func(Facts) Fact
	why  string
}

func gateSpecs() []gateSpec {
	return []gateSpec{
		{GateDaemonSet, func(f Facts) Fact { return f.DaemonSet },
			"Fargate runs one pod per VM and supports no DaemonSets, so a DaemonSet serving this workload would stop serving it"},
		{GateExtendedResource, func(f Facts) Fact { return f.ExtendedResource },
			"Fargate offers no GPUs or other extended resources"},
		{GateEBSVolume, func(f Facts) Fact { return f.EBSVolume },
			"Fargate pods cannot attach EBS volumes"},
		{GatePrivileged, func(f Facts) Fact { return f.Privileged },
			"Fargate does not run privileged containers"},
		{GateHostPath, func(f Facts) Fact { return f.HostPath },
			"Fargate exposes no host filesystem to mount"},
		{GateHostNetwork, func(f Facts) Fact { return f.HostNetwork },
			"Fargate pods cannot use the host network"},
		{GateHostPort, func(f Facts) Fact { return f.HostPort },
			"Fargate pods cannot bind host ports"},
		{GatePrivateSubnet, func(f Facts) Fact { return f.NoPrivateSubnet },
			"a Fargate profile can only place pods in private subnets"},
		{GateEvictionIntolerant, func(f Facts) Fact { return f.EvictionIntolerant },
			"Fargate never resizes in place and evicts pods to patch the platform"},
	}
}

// AllGates returns every gate in report order, including GateSizeCeiling. It
// returns a fresh slice so no caller can reorder the report for everyone else.
func AllGates() []Gate {
	specs := gateSpecs()
	out := make([]Gate, 0, len(specs)+1)
	for _, s := range specs {
		out = append(out, s.gate)
	}
	return append(out, GateSizeCeiling)
}

// BlockKind separates "this cannot work" from "nobody checked". Both block a
// move to Fargate; only the first is a property of the workload.
type BlockKind string

const (
	// BlockViolation: the property was observed and Fargate cannot provide it.
	BlockViolation BlockKind = "violation"
	// BlockUnverified: the property was not observed, so the move cannot be
	// shown to be safe. Conservative by construction — Kilter refuses to
	// recommend a move it cannot verify is legal.
	BlockUnverified BlockKind = "unverified"
)

// Block is one gate's refusal, with the pods that tripped it.
type Block struct {
	Gate Gate      `json:"gate"`
	Kind BlockKind `json:"kind"`
	// Reason is a complete human sentence; reports never synthesize their own.
	Reason string `json:"reason"`
	// Pods lists the offending pods as namespace/name, sorted, capped by
	// maxListedPods with a trailing "… and N more". Empty means the block is a
	// property of the whole pod set rather than of individual pods.
	Pods []string `json:"pods,omitempty"`
}

// maxListedPods caps the pod list on a block so a 5000-pod cluster does not
// produce a 5000-line report. The count is never lost: it is stated in the
// overflow entry.
const maxListedPods = 10

// FactsFromPodSpec derives the facts a cluster snapshot can already answer and
// leaves the rest Unknown — which blocks, loudly and by name.
//
// Derivable today: DaemonSet (owning controller kind), ExtendedResource
// (container extended requests), HostPath (PodSpec.HasLocalStorage covers
// hostPath and node-local PVs) and EvictionIntolerant (PodSpec.DoNotEvict,
// i.e. the kilter.dev/do-not-evict annotation).
//
// Not derivable, and therefore Unknown: EBSVolume, Privileged, HostNetwork,
// HostPort, NoPrivateSubnet. pkg/model carries no volume, security-context or
// subnet information, and inventing it from a heuristic (say, "StatefulSets
// use EBS") would produce exactly the confident-and-wrong recommendation the
// gates exist to prevent.
func FactsFromPodSpec(p *model.PodSpec) Facts {
	if p == nil {
		return Facts{}
	}
	return Facts{
		DaemonSet:          factOf(p.Workload.Kind == model.KindDaemonSet),
		ExtendedResource:   factOf(len(p.ExtendedRequests()) > 0),
		HostPath:           factOf(p.HasLocalStorage),
		EvictionIntolerant: factOf(p.DoNotEvict),
	}
}

// CompatibleFacts returns facts in which every gate property was observed and
// found absent — the only state in which a pod may be recommended for Fargate.
//
// It is what a pod already running on Fargate deserves: AWS scheduling it there
// is a stronger proof of compatibility than any check this package could run.
// A caller whose collector has genuinely checked all nine properties may also
// use it; a caller that has not, must not.
func CompatibleFacts() Facts {
	return Facts{
		DaemonSet: Absent, ExtendedResource: Absent, EBSVolume: Absent,
		Privileged: Absent, HostPath: Absent, HostNetwork: Absent,
		HostPort: Absent, NoPrivateSubnet: Absent, EvictionIntolerant: Absent,
	}
}

func factOf(present bool) Fact {
	if present {
		return Present
	}
	return Absent
}

// evaluateGates returns the blocks raised by the pod set, in AllGates order:
// for each gate, the pods that trip it, then the pods nobody checked. Gates
// are evaluated per pod because every property here is a property of a pod;
// cluster-wide properties (a private subnet exists, a DaemonSet serves this
// workload) are expressed by setting the same fact on every pod, which is what
// [FromSnapshot] does. Iteration is over slices only, so the order is fixed.
func evaluateGates(pods []Pod) []Block {
	var out []Block
	for _, spec := range gateSpecs() {
		var violating, unverified []string
		for i := range pods {
			f := spec.fact(pods[i].Facts)
			if !f.blocks() {
				continue
			}
			if f == Present {
				violating = append(violating, podKey(&pods[i].Spec))
			} else {
				unverified = append(unverified, podKey(&pods[i].Spec))
			}
		}
		if len(violating) > 0 {
			out = append(out, Block{
				Gate: spec.gate, Kind: BlockViolation,
				Reason: string(spec.gate) + ": " + spec.why, Pods: capPods(violating),
			})
		}
		if len(unverified) > 0 {
			out = append(out, Block{
				Gate: spec.gate, Kind: BlockUnverified,
				Reason: string(spec.gate) + ": not observed, so a move to Fargate cannot be shown to be safe (" + spec.why + ")",
				Pods:   capPods(unverified),
			})
		}
	}
	return out
}

// sortBlocks puts blocks into AllGates order. It is stable, so within one gate
// the order evaluateGates produced — violations before unverified — survives,
// and blocks appended later (the set-level DaemonSet block, the computed
// size-ceiling block) land in their gate's group instead of at the end.
func sortBlocks(blocks []Block) []Block {
	order := make(map[Gate]int, len(blocks))
	for i, g := range AllGates() {
		order[g] = i
	}
	sort.SliceStable(blocks, func(i, j int) bool { return order[blocks[i].Gate] < order[blocks[j].Gate] })
	return blocks
}

// capPods sorts and truncates a pod list, stating the count it dropped.
func capPods(in []string) []string {
	sort.Strings(in)
	if len(in) <= maxListedPods {
		return in
	}
	out := append([]string(nil), in[:maxListedPods]...)
	return append(out, fmt.Sprintf("… and %d more", len(in)-maxListedPods))
}

// podKey names a pod for reports: namespace/name, falling back to the UID when
// the snapshot carries no name.
func podKey(p *model.PodSpec) string {
	if p.Name == "" {
		if p.UID == "" {
			return "<unnamed pod>"
		}
		return p.UID
	}
	return p.Namespace + "/" + p.Name
}
