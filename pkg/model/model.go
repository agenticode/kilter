// Package model defines Kilter's core domain types. It is intentionally free of
// Kubernetes API dependencies so the whole decision engine stays pure, portable,
// and testable in milliseconds. Collectors translate live cluster state into these
// types; actuators translate decisions back.
package model

import (
	"fmt"
	"time"
)

// Resources is an amount of compute expressed in scheduler units:
// integer milli-CPU and bytes. Integer math only — no floating point drift.
type Resources struct {
	MilliCPU    int64 `json:"milliCPU"`
	MemoryBytes int64 `json:"memoryBytes"`
}

func (r Resources) Add(o Resources) Resources {
	return Resources{r.MilliCPU + o.MilliCPU, r.MemoryBytes + o.MemoryBytes}
}

func (r Resources) Sub(o Resources) Resources {
	return Resources{r.MilliCPU - o.MilliCPU, r.MemoryBytes - o.MemoryBytes}
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

// WorkloadRef identifies a workload (controller) in a cluster.
type WorkloadRef struct {
	Kind      WorkloadKind `json:"kind"`
	Namespace string       `json:"namespace"`
	Name      string       `json:"name"`
}

func (w WorkloadRef) String() string {
	return fmt.Sprintf("%s/%s/%s", w.Kind, w.Namespace, w.Name)
}

// ContainerKey identifies a container template within a workload.
type ContainerKey struct {
	Workload  WorkloadRef `json:"workload"`
	Container string      `json:"container"`
}

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

// Limits sums container limits (0 means unlimited for that dimension).
func (p *PodSpec) Limits() Resources {
	var sum Resources
	for _, c := range p.Containers {
		sum = sum.Add(c.Limits)
	}
	return sum
}

// ContainerSpec is one container's declared sizing.
type ContainerSpec struct {
	Name           string    `json:"name"`
	Requests       Resources `json:"requests"`
	Limits         Resources `json:"limits"`
	RestartCount   int32     `json:"restartCount,omitempty"`
	LastOOMKilled  bool      `json:"lastOOMKilled,omitempty"`
	LastTerminated string    `json:"lastTerminated,omitempty"` // reason of last termination
}

// NodeSpec captures a node's capacity and scheduling surface.
type NodeSpec struct {
	Name          string            `json:"name"`
	Labels        map[string]string `json:"labels,omitempty"`
	Taints        []Taint           `json:"taints,omitempty"`
	Capacity      Resources         `json:"capacity"`
	Allocatable   Resources         `json:"allocatable"`
	Ready         bool              `json:"ready"`
	Unschedulable bool              `json:"unschedulable,omitempty"`
	CreatedAt     time.Time         `json:"createdAt,omitempty"`

	// Pricing identity — resolved by pkg/pricing.
	InstanceType string  `json:"instanceType,omitempty"` // from node.kubernetes.io/instance-type
	Zone         string  `json:"zone,omitempty"`         // topology.kubernetes.io/zone
	Region       string  `json:"region,omitempty"`
	Provider     string  `json:"provider,omitempty"` // aws | gcp | azure | custom
	Spot         bool    `json:"spot,omitempty"`
	HourlyCost   float64 `json:"hourlyCost,omitempty"` // resolved cost, USD/h
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
}

// Matches reports whether the PDB selector matches the given labels.
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
}

// NodesByName indexes nodes.
func (s *ClusterSnapshot) NodesByName() map[string]*NodeSpec {
	m := make(map[string]*NodeSpec, len(s.Nodes))
	for i := range s.Nodes {
		m[s.Nodes[i].Name] = &s.Nodes[i]
	}
	return m
}

// PodsOnNode returns pods assigned to the given node.
func (s *ClusterSnapshot) PodsOnNode(node string) []*PodSpec {
	var out []*PodSpec
	for i := range s.Pods {
		if s.Pods[i].NodeName == node {
			out = append(out, &s.Pods[i])
		}
	}
	return out
}
