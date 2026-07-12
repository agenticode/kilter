package model

import "testing"

func TestResourcesArithmetic(t *testing.T) {
	a := Resources{MilliCPU: 500, MemoryBytes: 1 << 30}
	b := Resources{MilliCPU: 250, MemoryBytes: 1 << 29}
	if got := a.Add(b); got.MilliCPU != 750 || got.MemoryBytes != (1<<30)+(1<<29) {
		t.Fatalf("Add = %+v", got)
	}
	if got := a.Sub(b); got.MilliCPU != 250 || got.MemoryBytes != 1<<29 {
		t.Fatalf("Sub = %+v", got)
	}
	if !a.Fits(b) {
		t.Fatal("b should fit in a")
	}
	if b.Fits(a) {
		t.Fatal("a should not fit in b")
	}
	if got := b.Max(a); got != a {
		t.Fatalf("Max = %+v", got)
	}
	if !(Resources{}).IsZero() {
		t.Fatal("zero value should be zero")
	}
}

func TestTolerationSemantics(t *testing.T) {
	taint := Taint{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}
	cases := []struct {
		name string
		tol  Toleration
		want bool
	}{
		{"exact equal", Toleration{Key: "dedicated", Operator: "Equal", Value: "gpu", Effect: "NoSchedule"}, true},
		{"default operator equal", Toleration{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}, true},
		{"wrong value", Toleration{Key: "dedicated", Operator: "Equal", Value: "cpu", Effect: "NoSchedule"}, false},
		{"exists matches any value", Toleration{Key: "dedicated", Operator: "Exists", Effect: "NoSchedule"}, true},
		{"empty effect matches all", Toleration{Key: "dedicated", Operator: "Exists"}, true},
		{"wrong effect", Toleration{Key: "dedicated", Operator: "Exists", Effect: "NoExecute"}, false},
		{"wrong key", Toleration{Key: "other", Operator: "Exists"}, false},
		{"global exists tolerates all", Toleration{Operator: "Exists"}, true},
		{"empty key equal never matches", Toleration{Operator: "Equal"}, false},
	}
	for _, c := range cases {
		if got := c.tol.Tolerates(taint); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestPDBMatches(t *testing.T) {
	pdb := PDB{Selector: map[string]string{"app": "web"}}
	if !pdb.Matches(map[string]string{"app": "web", "tier": "front"}) {
		t.Fatal("should match superset labels")
	}
	if pdb.Matches(map[string]string{"app": "api"}) {
		t.Fatal("should not match different value")
	}
	empty := PDB{}
	if empty.Matches(map[string]string{"app": "web"}) {
		t.Fatal("empty selector must not match (unlike k8s empty-selects-all, PDB collectors always set selectors)")
	}
}

func TestSnapshotIndexes(t *testing.T) {
	s := ClusterSnapshot{
		Nodes: []NodeSpec{{Name: "a"}, {Name: "b"}},
		Pods: []PodSpec{
			{Name: "p1", NodeName: "a"},
			{Name: "p2", NodeName: "b"},
			{Name: "p3", NodeName: "a"},
		},
	}
	if len(s.NodesByName()) != 2 {
		t.Fatal("node index wrong")
	}
	if got := s.PodsOnNode("a"); len(got) != 2 {
		t.Fatalf("PodsOnNode(a) = %d pods", len(got))
	}
	if got := s.PodsOnNode("c"); got != nil {
		t.Fatalf("PodsOnNode(c) should be nil, got %v", got)
	}
}

func TestPodAggregation(t *testing.T) {
	p := PodSpec{Containers: []ContainerSpec{
		{Requests: Resources{100, 1 << 20}, Limits: Resources{200, 1 << 21}},
		{Requests: Resources{50, 1 << 20}},
	}}
	if got := p.Requests(); got.MilliCPU != 150 || got.MemoryBytes != 2<<20 {
		t.Fatalf("Requests = %+v", got)
	}
	if got := p.Limits(); got.MilliCPU != 200 {
		t.Fatalf("Limits = %+v", got)
	}
}
