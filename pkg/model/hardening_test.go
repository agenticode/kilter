package model

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"
)

// The recommender and planner key maps by these identity types; a slice or
// map field added to either would break half the engine, so pin
// comparability here where the type lives.
var (
	_ = map[WorkloadRef]struct{}{}
	_ = map[ContainerKey]struct{}{}
)

// TestResourcesSaturation pins the overflow contract: element-wise arithmetic
// clamps at the int64 bounds instead of wrapping. Wraparound would flip an
// absurdly large request negative, and downstream accounting clamps negatives
// to zero — turning "impossible to schedule" into "fits anywhere".
func TestResourcesSaturation(t *testing.T) {
	const mx, mn = math.MaxInt64, math.MinInt64
	addCases := []struct {
		name string
		a, b Resources
		want Resources
	}{
		{"normal", Resources{100, 200}, Resources{50, 25}, Resources{150, 225}},
		{"max plus one", Resources{mx, mx}, Resources{1, 1}, Resources{mx, mx}},
		{"max plus max", Resources{mx, mx}, Resources{mx, mx}, Resources{mx, mx}},
		{"min plus negative", Resources{mn, mn}, Resources{-1, -1}, Resources{mn, mn}},
		{"mixed signs exact", Resources{mx, mn}, Resources{-1, 1}, Resources{mx - 1, mn + 1}},
		{"negative garbage stays exact", Resources{-5, -5}, Resources{3, 3}, Resources{-2, -2}},
	}
	for _, c := range addCases {
		if got := c.a.Add(c.b); got != c.want {
			t.Errorf("Add %s: %+v + %+v = %+v, want %+v", c.name, c.a, c.b, got, c.want)
		}
		if got := c.b.Add(c.a); got != c.want {
			t.Errorf("Add %s (commuted): got %+v, want %+v", c.name, got, c.want)
		}
	}

	subCases := []struct {
		name string
		a, b Resources
		want Resources
	}{
		{"normal", Resources{100, 200}, Resources{150, 50}, Resources{-50, 150}},
		{"min minus one", Resources{mn, mn}, Resources{1, 1}, Resources{mn, mn}},
		{"max minus negative", Resources{mx, mx}, Resources{-1, -1}, Resources{mx, mx}},
		{"zero minus min", Resources{0, 0}, Resources{mn, mn}, Resources{mx, mx}},
		{"negative minus min exact", Resources{-1, -1}, Resources{mn, mn}, Resources{mx, mx}},
	}
	for _, c := range subCases {
		if got := c.a.Sub(c.b); got != c.want {
			t.Errorf("Sub %s: %+v - %+v = %+v, want %+v", c.name, c.a, c.b, got, c.want)
		}
	}
}

// TestPodRequestsOverflowSafe: a snapshot whose containers sum past MaxInt64
// must report a saturated (still-huge) request, never a wrapped negative one.
func TestPodRequestsOverflowSafe(t *testing.T) {
	p := PodSpec{Containers: []ContainerSpec{
		{Requests: Resources{math.MaxInt64, math.MaxInt64}},
		{Requests: Resources{math.MaxInt64, 1}},
	}}
	got := p.Requests()
	if got.MilliCPU != math.MaxInt64 || got.MemoryBytes != math.MaxInt64 {
		t.Fatalf("Requests = %+v, want saturated MaxInt64", got)
	}
	node := Resources{MilliCPU: 64000, MemoryBytes: 256 << 30}
	if node.Fits(got) {
		t.Fatal("saturated pod must not fit on a real node")
	}
}

func TestExtendedRequests(t *testing.T) {
	t.Run("nil when absent", func(t *testing.T) {
		p := PodSpec{Containers: []ContainerSpec{{Name: "a"}, {Name: "b"}}}
		if got := p.ExtendedRequests(); got != nil {
			t.Fatalf("want nil map, got %v", got)
		}
	})
	t.Run("merges across containers", func(t *testing.T) {
		p := PodSpec{Containers: []ContainerSpec{
			{Extended: map[string]int64{"nvidia.com/gpu": 2, "hugepages-2Mi": 512}},
			{Extended: map[string]int64{"nvidia.com/gpu": 1}},
		}}
		got := p.ExtendedRequests()
		if got["nvidia.com/gpu"] != 3 || got["hugepages-2Mi"] != 512 {
			t.Fatalf("ExtendedRequests = %v", got)
		}
	})
	t.Run("saturates instead of wrapping", func(t *testing.T) {
		p := PodSpec{Containers: []ContainerSpec{
			{Extended: map[string]int64{"nvidia.com/gpu": math.MaxInt64}},
			{Extended: map[string]int64{"nvidia.com/gpu": 5}},
		}}
		if got := p.ExtendedRequests()["nvidia.com/gpu"]; got != math.MaxInt64 {
			t.Fatalf("gpu sum = %d, want MaxInt64", got)
		}
	})
}

func TestPDBCovers(t *testing.T) {
	uidPDB := &PDB{
		Namespace:      "prod",
		Selector:       map[string]string{"app": "web"},
		CoveredPodUIDs: []string{"u1", "u2"},
	}
	selPDB := &PDB{Namespace: "prod", Selector: map[string]string{"app": "web"}}
	cases := []struct {
		name string
		pdb  *PDB
		pod  *PodSpec
		want bool
	}{
		{"uid list hit beats label miss", uidPDB,
			&PodSpec{UID: "u1", Namespace: "prod", Labels: map[string]string{"app": "api"}}, true},
		{"uid list miss beats label hit", uidPDB,
			&PodSpec{UID: "u9", Namespace: "prod", Labels: map[string]string{"app": "web"}}, false},
		{"namespace mismatch always loses", uidPDB,
			&PodSpec{UID: "u1", Namespace: "staging", Labels: map[string]string{"app": "web"}}, false},
		{"empty uid list falls back to selector", selPDB,
			&PodSpec{UID: "u1", Namespace: "prod", Labels: map[string]string{"app": "web"}}, true},
		{"selector fallback respects labels", selPDB,
			&PodSpec{UID: "u1", Namespace: "prod", Labels: map[string]string{"app": "api"}}, false},
		{"nil pod never covered", uidPDB, nil, false},
		{"nil receiver never covers", nil,
			&PodSpec{UID: "u1", Namespace: "prod", Labels: map[string]string{"app": "web"}}, false},
	}
	for _, c := range cases {
		if got := c.pdb.Covers(c.pod); got != c.want {
			t.Errorf("%s: Covers = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestPodsOnNodeEmptyName: unscheduled pods carry NodeName "" — they must not
// be reported as running on a node named "".
func TestPodsOnNodeEmptyName(t *testing.T) {
	s := ClusterSnapshot{Pods: []PodSpec{
		{Name: "pending-1", NodeName: "", Phase: "Pending"},
		{Name: "running-1", NodeName: "a"},
	}}
	if got := s.PodsOnNode(""); got != nil {
		t.Fatalf("PodsOnNode(\"\") = %d pods, want nil", len(got))
	}
	if got := s.PodsOnNode("a"); len(got) != 1 || got[0].Name != "running-1" {
		t.Fatalf("PodsOnNode(a) = %v", got)
	}
}

func TestNodesByNameSemantics(t *testing.T) {
	s := ClusterSnapshot{Nodes: []NodeSpec{
		{Name: "a", Ready: false},
		{Name: "a", Ready: true}, // duplicate: last entry wins
		{Name: "b"},
	}}
	m := s.NodesByName()
	if len(m) != 2 {
		t.Fatalf("index size = %d, want 2", len(m))
	}
	if !m["a"].Ready {
		t.Fatal("duplicate name should keep the last entry")
	}
	// Values alias the snapshot slice so accounting code can mutate in place.
	m["b"].Ready = true
	if !s.Nodes[2].Ready {
		t.Fatal("map values must point into s.Nodes")
	}
}

// TestTolerationAdversarial covers inputs Kubernetes validation would reject
// but a garbage snapshot can still carry. Unknown operators must never
// tolerate: erring toward "does not tolerate" only makes placement more
// conservative, never unsafe.
func TestTolerationAdversarial(t *testing.T) {
	taint := Taint{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}
	cases := []struct {
		name string
		tol  Toleration
		want bool
	}{
		{"unknown operator", Toleration{Key: "dedicated", Operator: "Matches", Value: "gpu"}, false},
		{"unknown operator empty key", Toleration{Operator: "Matches"}, false},
		{"exists ignores stray value", Toleration{Key: "dedicated", Operator: "Exists", Value: "ignored"}, true},
		{"effect-only zero toleration", Toleration{Effect: "NoSchedule"}, false},
		{"empty toleration", Toleration{}, false},
		{"global exists wrong effect", Toleration{Operator: "Exists", Effect: "NoExecute"}, false},
		{"equal empty value vs valued taint", Toleration{Key: "dedicated", Operator: "Equal"}, false},
	}
	for _, c := range cases {
		if got := c.tol.Tolerates(taint); got != c.want {
			t.Errorf("%s: Tolerates = %v, want %v", c.name, got, c.want)
		}
	}
	// Empty-valued taint with empty-valued Equal toleration does match.
	if !(Toleration{Key: "k", Operator: "Equal"}).Tolerates(Taint{Key: "k", Effect: "NoSchedule"}) {
		t.Error("empty values on both sides should match under Equal")
	}
}

// TestSnapshotJSONRoundTrip pins the agent→brain wire contract: a fully
// populated snapshot must survive marshal/unmarshal byte-exact in meaning.
// This catches future json-tag collisions or field omissions that would
// silently drop data between agent and brain versions.
func TestSnapshotJSONRoundTrip(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 30, 0, 0, time.UTC)
	snap := ClusterSnapshot{
		ClusterID: "c1",
		Timestamp: at,
		Nodes: []NodeSpec{{
			Name:                "n1",
			Labels:              map[string]string{"zone": "a"},
			Taints:              []Taint{{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}},
			Capacity:            Resources{MilliCPU: 8000, MemoryBytes: 32 << 30},
			Allocatable:         Resources{MilliCPU: 7500, MemoryBytes: 30 << 30},
			ExtendedAllocatable: map[string]int64{"nvidia.com/gpu": 4},
			Ready:               true,
			Unschedulable:       true,
			CreatedAt:           at,
			InstanceType:        "m5.2xlarge",
			Zone:                "us-east-1a",
			Region:              "us-east-1",
			Provider:            "aws",
			Spot:                true,
			HourlyCost:          0.384,
			ManagedBy:           "karpenter",
		}},
		Pods: []PodSpec{{
			UID:       "u1",
			Name:      "web-abc",
			Namespace: "prod",
			Workload:  WorkloadRef{Kind: KindDeployment, Namespace: "prod", Name: "web"},
			Labels:    map[string]string{"app": "web"},
			NodeName:  "n1",
			Containers: []ContainerSpec{{
				Name:           "app",
				Requests:       Resources{MilliCPU: 500, MemoryBytes: 1 << 30},
				Limits:         Resources{MilliCPU: 1000, MemoryBytes: 2 << 30},
				Extended:       map[string]int64{"nvidia.com/gpu": 1},
				RestartCount:   3,
				LastOOMKilled:  true,
				LastTerminated: "OOMKilled",
			}},
			NodeSelector: map[string]string{"zone": "a"},
			RequiredAffinity: []NodeSelectorTerm{{MatchExpressions: []NodeSelectorRequirement{
				{Key: "arch", Operator: "In", Values: []string{"amd64"}},
			}}},
			Tolerations:      []Toleration{{Key: "dedicated", Operator: "Equal", Value: "gpu", Effect: "NoSchedule"}},
			AntiAffinityKeys: []string{"kubernetes.io/hostname"},
			TopologySpread: []TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "zone", WhenUnsatisfiable: "DoNotSchedule",
				LabelSelector: map[string]string{"app": "web"},
			}},
			PriorityClass:   "high",
			Priority:        1000,
			QOSClass:        "Burstable",
			Phase:           "Running",
			CreatedAt:       at,
			HasLocalStorage: true,
			DoNotEvict:      true,
		}},
		Workloads: []WorkloadInfo{{
			Ref:      WorkloadRef{Kind: KindDeployment, Namespace: "prod", Name: "web"},
			Replicas: 3, Ready: 3,
			Labels: map[string]string{"app": "web"},
			HasHPA: true, HPAMinReplicas: 2, HPAMaxReplicas: 10, HPATargetsCPU: true,
			HPAOwner: "keda", Mode: "recommend",
		}},
		PDBs: []PDB{{
			Namespace: "prod", Name: "web-pdb",
			Selector:           map[string]string{"app": "web"},
			DisruptionsAllowed: 1, CurrentHealthy: 3, DesiredHealthy: 2,
			CoveredPodUIDs: []string{"u1"},
		}},
		Usage: []Usage{{
			Key:       ContainerKey{Workload: WorkloadRef{Kind: KindDeployment, Namespace: "prod", Name: "web"}, Container: "app"},
			PodUID:    "u1",
			Timestamp: at,
			MilliCPU:  420, MemoryBytes: 900 << 20,
			WindowSeconds: 60,
		}},
		ServerVersion:  "v1.31.0",
		NamespaceModes: map[string]string{"prod": "apply"},
		Frozen:         true,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ClusterSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(snap, got) {
		t.Fatalf("round trip lost data:\n want %+v\n got  %+v", snap, got)
	}
}

func TestStringFormats(t *testing.T) {
	if got := (Resources{MilliCPU: 1500, MemoryBytes: 512 << 20}).String(); got != "1500m/512Mi" {
		t.Errorf("Resources.String = %q", got)
	}
	// Sub-MiB truncates toward zero rather than rounding — documented behavior.
	if got := (Resources{MilliCPU: 10, MemoryBytes: 1<<20 - 1}).String(); got != "10m/0Mi" {
		t.Errorf("Resources.String sub-MiB = %q", got)
	}
	if got := (Resources{MilliCPU: -100, MemoryBytes: -(3 << 20)}).String(); got != "-100m/-3Mi" {
		t.Errorf("Resources.String negative = %q", got)
	}
	ref := WorkloadRef{Kind: KindDeployment, Namespace: "prod", Name: "web"}
	if got := ref.String(); got != "Deployment/prod/web" {
		t.Errorf("WorkloadRef.String = %q", got)
	}
	key := ContainerKey{Workload: ref, Container: "app"}
	if got := key.String(); got != "Deployment/prod/web/app" {
		t.Errorf("ContainerKey.String = %q", got)
	}
}
