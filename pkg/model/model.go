// Package model defines Kilter's core domain types. It is intentionally free of
// Kubernetes API dependencies so the whole decision engine stays pure, portable,
// and testable in milliseconds. Collectors translate live cluster state into these
// types; actuators translate decisions back.
package model

import (
	"fmt"
	"math"
	"time"
)

// Resources is an amount of compute expressed in scheduler units:
// integer milli-CPU and bytes. Integer math only — no floating point drift.
type Resources struct {
	MilliCPU    int64 `json:"milliCPU"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// satAdd64 returns a+b clamped to the int64 range instead of wrapping.
func satAdd64(a, b int64) int64 {
	s := a + b
	if a > 0 && b > 0 && s < 0 {
		return math.MaxInt64
	}
	if a < 0 && b < 0 && s >= 0 {
		return math.MinInt64
	}
	return s
}

// satSub64 returns a-b clamped to the int64 range instead of wrapping.
func satSub64(a, b int64) int64 {
	d := a - b
	if a >= 0 && b < 0 && d < 0 {
		return math.MaxInt64
	}
	if a < 0 && b > 0 && d >= 0 {
		return math.MinInt64
	}
	return d
}

// Add returns the element-wise sum, saturating at the int64 bounds instead of
// wrapping. Wraparound is the dangerous failure here: a garbage snapshot whose
// requests sum past MaxInt64 would flip negative, and downstream accounting
// clamps negative requests to zero — minting free capacity out of an absurdly
// large pod. A saturated sum stays absurdly large and fails Fits everywhere,
// which is the safe direction.
func (r Resources) Add(o Resources) Resources {
	return Resources{satAdd64(r.MilliCPU, o.MilliCPU), satAdd64(r.MemoryBytes, o.MemoryBytes)}
}

// Sub returns the element-wise difference, saturating at the int64 bounds
// instead of wrapping. Negative results are legitimate (overcommit shows up as
// negative Free); only wraparound is forbidden, for the same reason as Add.
func (r Resources) Sub(o Resources) Resources {
	return Resources{satSub64(r.MilliCPU, o.MilliCPU), satSub64(r.MemoryBytes, o.MemoryBytes)}
}

// Fits reports whether o fits inside r (both dimensions).
func (r Resources) Fits(o Resources) bool {
	return o.MilliCPU <= r.MilliCPU && o.MemoryBytes <= r.MemoryBytes
}

// IsZero reports whether both dimensions are zero.
func (r Resources) IsZero() bool { return r.MilliCPU == 0 && r.MemoryBytes == 0 }

// Max returns the element-wise maximum.
func (r Resources) Max(o Resources) Resources {
	out := r
	if o.MilliCPU > out.MilliCPU {
		out.MilliCPU = o.MilliCPU
	}
	if o.MemoryBytes > out.MemoryBytes {
		out.MemoryBytes = o.MemoryBytes
	}
	return out
}

// String renders as "<milliCPU>m/<MiB>Mi" for logs; memory truncates toward
// zero, so sub-MiB amounts print as 0Mi.
func (r Resources) String() string {
	return fmt.Sprintf("%dm/%dMi", r.MilliCPU, r.MemoryBytes/(1<<20))
}

// WorkloadKind mirrors the owning controller kind of a pod.
type WorkloadKind string

const (
	KindDeployment  WorkloadKind = "Deployment"
	KindStatefulSet WorkloadKind = "StatefulSet"
	KindDaemonSet   WorkloadKind = "DaemonSet"
	KindJob         WorkloadKind = "Job"
	KindCronJob     WorkloadKind = "CronJob"
	KindReplicaSet  WorkloadKind = "ReplicaSet"
	KindBarePod     WorkloadKind = "Pod"
)

// WorkloadRef identifies a workload (controller) in a cluster. It is used as
// a map key across the decision engine, so it must stay comparable — never
// add slice or map fields.
type WorkloadRef struct {
	Kind      WorkloadKind `json:"kind"`
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
}

// String renders as "Kind/namespace/name".
func (w WorkloadRef) String() string {
	return fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
}

// ContainerKey identifies a container template within a workload. Like
// WorkloadRef it serves as a map key, so it must stay comparable.
type ContainerKey struct {
	Workload  WorkloadRef `json:"workload"`
	Container string      `json:"container"`
}

// String renders as "Kind/namespace/name/container".
func (c ContainerKey) String() string {
	return c.Workload.String() + "/" + c.Container
}

// Taint mirrors a node taint.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"` // NoSchedule | PreferNoSchedule | NoExecute
}

// Toleration mirrors a pod toleration (subset: Exists/Equal operators).
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"` // Exists | Equal (default Equal)
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"` // empty matches all effects
}

// Tolerates reports whether the toleration tolerates the taint,
// following Kubernetes semantics.
func (t Toleration) Tolerates(taint Taint) bool {
	if t.Effect != "" && t.Effect != taint.Effect {
		return false
	}
	// Empty key with Exists tolerates everything.
	if t.Key == "" {
		return t.Operator == "Exists"
	}
	if t.Key != taint.Key {
		return false
	}
	switch t.Operator {
	case "Exists":
		return true
	case "Equal", "":
		return t.Value == taint.Value
	}
	return false
}

// NodeSelectorRequirement is one matchExpression on node labels.
type NodeSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"` // In | NotIn | Exists | DoesNotExist | Gt | Lt
	Values   []string `json:"values,omitempty"`
}

// NodeSelectorTerm is ANDed requirements; terms themselves are ORed.
type NodeSelectorTerm struct {
	MatchExpressions []NodeSelectorRequirement `json:"matchExpressions,omitempty"`
}

// TopologySpreadConstraint is the scheduling-relevant subset.
type TopologySpreadConstraint struct {
	MaxSkew           int32             `json:"maxSkew"`
	TopologyKey       string            `json:"topologyKey"`
	WhenUnsatisfiable string            `json:"whenUnsatisfiable"` // DoNotSchedule | ScheduleAnyway
	LabelSelector     map[string]string `json:"labelSelector,omitempty"`
}

// PodSpec captures the scheduling- and sizing-relevant parts of a pod.
type PodSpec struct {
	UID       string            `json:"uid"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Workload  WorkloadRef       `json:"workload"`
	Labels    map[string]string `json:"labels,omitempty"`

	NodeName string `json:"nodeName,omitempty"`

	Containers []ContainerSpec `json:"containers"`
	// InitRequests is the element-wise maximum over the pod's init container
	// requests. Init containers run to completion one at a time, so they never
	// stack: the pod's effective request is max(InitRequests, sum of
	// Containers' requests) per dimension. Zero value = no init containers.
	// Fargate sizes (and bills) a pod from exactly that maximum.
	InitRequests Resources `json:"initRequests,omitempty"`
	// ProvisionedCapacity is the billed capacity AWS stamped on a Fargate pod
	// via the CapacityProvisioned annotation (e.g. "0.25vCPU 0.5GB"). It is
	// ground truth for what the pod costs and outranks any computed estimate.
	// Zero value = not a Fargate pod, or the annotation was absent.
	ProvisionedCapacity Resources `json:"provisionedCapacity,omitempty"`

	NodeSelector     map[string]string          `json:"nodeSelector,omitempty"`
	RequiredAffinity []NodeSelectorTerm         `json:"requiredAffinity,omitempty"` // ORed terms
	Tolerations      []Toleration               `json:"tolerations,omitempty"`
	AntiAffinityKeys []string                   `json:"antiAffinityKeys,omitempty"` // topology keys with self-anti-affinity
	TopologySpread   []TopologySpreadConstraint `json:"topologySpread,omitempty"`
	PriorityClass    string                     `json:"priorityClass,omitempty"`
	Priority         int32                      `json:"priority,omitempty"`
	QOSClass         string                     `json:"qosClass,omitempty"` // Guaranteed | Burstable | BestEffort
	Phase            string                     `json:"phase,omitempty"`    // Running | Pending | ...
	CreatedAt        time.Time                  `json:"createdAt,omitempty"`

	// HasLocalStorage marks pods using node-local data (local PVs); draining
	// such a pod loses state, so consolidation treats it as pinned.
	HasLocalStorage bool `json:"hasLocalStorage,omitempty"`
	// DoNotEvict mirrors the kilter.dev/do-not-evict pod annotation (also
	// honored: cluster-autoscaler.kubernetes.io/safe-to-evict=false).
	DoNotEvict bool `json:"doNotEvict,omitempty"`
}

// Requests sums container requests.
func (p *PodSpec) Requests() Resources {
	var sum Resources
	for _, c := range p.Containers {
		sum = sum.Add(c.Requests)
	}
	return sum
}

// ExtendedRequests sums non-core resource requests across containers,
// saturating at the int64 bounds (see Add for why wraparound is dangerous).
// Returns nil when no container declares extended resources.
func (p *PodSpec) ExtendedRequests() map[string]int64 {
	var out map[string]int64
	for _, c := range p.Containers {
		for k, v := range c.Extended {
			if out == nil {
				out = map[string]int64{}
			}
			out[k] = satAdd64(out[k], v)
		}
	}
	return out
}

// Limits sums container limits. A zero limit means unlimited for that
// dimension, so the sum understates the pod: a single zero-limit container
// makes the true pod-level cap unbounded even though the sum stays finite.
// Callers enforcing a ceiling must check for zero-limit containers themselves.
func (p *PodSpec) Limits() Resources {
	var sum Resources
	for _, c := range p.Containers {
		sum = sum.Add(c.Limits)
	}
	return sum
}

// ContainerSpec is one container's declared sizing.
type ContainerSpec struct {
	Name     string    `json:"name"`
	Requests Resources `json:"requests"`
	Limits   Resources `json:"limits"`
	// Extended holds non-core resource requests (nvidia.com/gpu, …) that
	// gate scheduling but are not optimized by Kilter.
	Extended       map[string]int64 `json:"extended,omitempty"`
	RestartCount   int32            `json:"restartCount,omitempty"`
	LastOOMKilled  bool             `json:"lastOOMKilled,omitempty"`
	LastTerminated string           `json:"lastTerminated,omitempty"` // reason of last termination
}

// NodeSpec captures a node's capacity and scheduling surface.
type NodeSpec struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Taints      []Taint           `json:"taints,omitempty"`
	Capacity    Resources         `json:"capacity"`
	Allocatable Resources         `json:"allocatable"`
	// ExtendedAllocatable holds allocatable non-core resources (GPUs, …).
	ExtendedAllocatable map[string]int64 `json:"extendedAllocatable,omitempty"`
	Ready               bool             `json:"ready"`
	Unschedulable       bool             `json:"unschedulable,omitempty"`
	CreatedAt           time.Time        `json:"createdAt,omitempty"`

	// Pricing identity — resolved by pkg/pricing.
	InstanceType string  `json:"instanceType,omitempty"` // from node.kubernetes.io/instance-type
	Zone         string  `json:"zone,omitempty"`         // topology.kubernetes.io/zone
	Region       string  `json:"region,omitempty"`
	Provider     string  `json:"provider,omitempty"` // aws | gcp | azure | custom
	Spot         bool    `json:"spot,omitempty"`
	HourlyCost   float64 `json:"hourlyCost,omitempty"` // resolved cost, USD/h

	// ManagedBy marks nodes whose lifecycle Kilter does not own:
	// ManagedByKarpenter (another autoscaler consolidates them; Kilter's
	// rightsizing feeds it) or ManagedByFargate (the "node" is a single-pod
	// Fargate VM — there is nothing to pack, drain, or delete).
	ManagedBy string `json:"managedBy,omitempty"`
}

// Values for NodeSpec.ManagedBy.
const (
	ManagedByKarpenter = "karpenter"
	ManagedByFargate   = "fargate"
)

// LabelComputeType is the EKS node label naming the compute engine behind a
// node; the value "fargate" marks a Fargate single-pod VM.
const LabelComputeType = "eks.amazonaws.com/compute-type"

// IsFargate reports whether the node is a Fargate single-pod VM rather than a
// real, shareable machine. Such a "node" hosts exactly one pod, is billed by
// quantized pod configuration rather than by node shape, and must never enter
// bin-packing, consolidation, or node pricing.
//
// The label is checked as well as ManagedBy so a snapshot taken by an older
// collector — which never set ManagedBy — is still classified correctly.
func (n *NodeSpec) IsFargate() bool {
	if n == nil {
		return false
	}
	return n.ManagedBy == ManagedByFargate || n.Labels[LabelComputeType] == ManagedByFargate
}

// Usage is a point-in-time measured usage sample for a container.
type Usage struct {
	Key         ContainerKey `json:"key"`
	PodUID      string       `json:"podUID"`
	Timestamp   time.Time    `json:"timestamp"`
	MilliCPU    int64        `json:"milliCPU"`
	MemoryBytes int64        `json:"memoryBytes"`
	// WindowSeconds is the averaging window the sample represents (metrics-server ~60s).
	WindowSeconds int32 `json:"windowSeconds,omitempty"`
}

// PDB captures a PodDisruptionBudget's current arithmetic.
type PDB struct {
	Namespace          string            `json:"namespace"`
	Name               string            `json:"name"`
	Selector           map[string]string `json:"selector,omitempty"`
	DisruptionsAllowed int32             `json:"disruptionsAllowed"`
	CurrentHealthy     int32             `json:"currentHealthy"`
	DesiredHealthy     int32             `json:"desiredHealthy"`
	// CoveredPodUIDs is the exact pod set the PDB selected at collection time.
	// Collectors fill it using full Kubernetes selector semantics (including
	// matchExpressions, which Selector cannot express). When present it takes
	// precedence over Selector.
	CoveredPodUIDs []string `json:"coveredPodUIDs,omitempty"`
}

// Matches reports whether the PDB selector matches the given labels. An empty
// selector matches nothing — unlike the Kubernetes empty-selects-all rule —
// so a zero-value PDB cannot freeze eviction for a whole namespace. Genuine
// select-all PDBs are still honored: collectors express them through
// CoveredPodUIDs, which Covers prefers over label matching.
func (p *PDB) Matches(labels map[string]string) bool {
	if len(p.Selector) == 0 {
		return false
	}
	for k, v := range p.Selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Covers reports whether the PDB applies to the pod, preferring exact
// collection-time coverage over label matching. An empty CoveredPodUIDs is
// indistinguishable from "collector didn't fill it", so it falls back to
// Selector, which errs toward covering. A nil receiver or pod never covers.
func (p *PDB) Covers(pod *PodSpec) bool {
	if p == nil || pod == nil {
		return false
	}
	if pod.Namespace != p.Namespace {
		return false
	}
	if len(p.CoveredPodUIDs) > 0 {
		for _, uid := range p.CoveredPodUIDs {
			if uid == pod.UID {
				return true
			}
		}
		return false
	}
	return p.Matches(pod.Labels)
}

// WorkloadInfo aggregates a controller and its replica intent.
type WorkloadInfo struct {
	Ref            WorkloadRef       `json:"ref"`
	Replicas       int32             `json:"replicas"`
	Ready          int32             `json:"ready"`
	Labels         map[string]string `json:"labels,omitempty"`
	HasHPA         bool              `json:"hasHPA,omitempty"`
	HPAMinReplicas int32             `json:"hpaMinReplicas,omitempty"`
	HPAMaxReplicas int32             `json:"hpaMaxReplicas,omitempty"`
	HPATargetsCPU  bool              `json:"hpaTargetsCPU,omitempty"` // request changes shift HPA math
	// HPAOwner marks HPAs driven by another controller ("keda").
	HPAOwner string `json:"hpaOwner,omitempty"`
	// Mode is the workload's kilter.dev/mode annotation: "" (inherit),
	// "off" (hands off entirely), "recommend" (no automation), "apply".
	Mode string `json:"mode,omitempty"`
}

// Insight is a detection-layer finding: a predicted or observed condition
// worth human or automated attention. Insights are how Kilter closes the
// AIOps loop between telemetry and action — every one carries its evidence.
type Insight struct {
	Kind     string `json:"kind"`     // oom-risk | cpu-saturation | capacity-exhaustion | ...
	Severity string `json:"severity"` // info | warning | critical

	Workload  WorkloadRef `json:"workload,omitempty"`
	Container string      `json:"container,omitempty"`
	Node      string      `json:"node,omitempty"`

	Message string `json:"message"`
	// HorizonHours estimates time-to-impact for predictive insights (0 = now).
	HorizonHours float64   `json:"horizonHours,omitempty"`
	At           time.Time `json:"at"`
}

// ClusterSnapshot is the unit shipped from agent to brain: complete topology
// plus the usage samples gathered since the previous snapshot.
type ClusterSnapshot struct {
	ClusterID string         `json:"clusterID"`
	Timestamp time.Time      `json:"timestamp"`
	Nodes     []NodeSpec     `json:"nodes"`
	Pods      []PodSpec      `json:"pods"`
	Workloads []WorkloadInfo `json:"workloads,omitempty"`
	PDBs      []PDB          `json:"pdbs,omitempty"`
	Usage     []Usage        `json:"usage,omitempty"`
	// K8s server info — feature gates like InPlacePodVerticalScaling depend on it.
	ServerVersion string `json:"serverVersion,omitempty"`
	// NamespaceModes carries kilter.dev/mode namespace annotations.
	NamespaceModes map[string]string `json:"namespaceModes,omitempty"`
	// Frozen mirrors the kilter.dev/freeze=true annotation on the kube-system
	// namespace: the cluster-wide kill switch for all Kilter automation.
	Frozen bool `json:"frozen,omitempty"`
}

// NodesByName indexes nodes by name. Values point into s.Nodes, so mutations
// through the map are visible in the snapshot. Duplicate names keep the last
// entry.
func (s *ClusterSnapshot) NodesByName() map[string]*NodeSpec {
	m := make(map[string]*NodeSpec, len(s.Nodes))
	for i := range s.Nodes {
		m[s.Nodes[i].Name] = &s.Nodes[i]
	}
	return m
}

// PodsOnNode returns pods assigned to the given node. An empty node name
// returns nil rather than the unscheduled pods that carry an empty NodeName:
// no real node is named "", and counting pending pods as running on one would
// silently inflate a caller's utilization math.
func (s *ClusterSnapshot) PodsOnNode(node string) []*PodSpec {
	if node == "" {
		return nil
	}
	var out []*PodSpec
	for i := range s.Pods {
		if s.Pods[i].NodeName == node {
			out = append(out, &s.Pods[i])
		}
	}
	return out
}
