package explain

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/model"
)

// update rewrites the golden files. Run: go test ./pkg/explain -update
var update = flag.Bool("update", false, "rewrite golden files in testdata/golden")

// t0 is the fixed epoch every fixture is built from. No test in this package
// reads a clock, because no code in this package does.
var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

const testCluster = "prod-eks-1"

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

// goldenJSON compares v's indented JSON against testdata/golden/<name>.json.
func goldenJSON(t *testing.T, name string, v any) []byte {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "golden", name+".json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return got
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run: go test ./pkg/explain -update): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
	return got
}

// goldenText is goldenJSON for prose renderings.
func goldenText(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run: go test ./pkg/explain -update): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("golden %s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// point builds a timeline observation.
func point(at time.Time, usdPerHour float64, nodes int) evidence.TimelinePoint {
	return evidence.TimelinePoint{At: at, CostUSDPerHour: usdPerHour, Nodes: nodes}
}

// ev builds an evidence event on a cluster-scoped subject.
func ev(at time.Time, kind, severity string, subj evidence.SubjectRef, attrs map[string]string) evidence.EvidenceEvent {
	return evidence.EvidenceEvent{At: at, Kind: kind, Severity: severity, Subject: subj, Attrs: attrs}
}

func workloadSubject(ns, name string) evidence.SubjectRef {
	return evidence.WorkloadSubject(testCluster, model.WorkloadRef{
		Kind: model.WorkloadKind("Deployment"), Namespace: ns, Name: name,
	})
}

func containerKey(ns, name, container string) model.ContainerKey {
	return model.ContainerKey{
		Workload:  model.WorkloadRef{Kind: model.WorkloadKind("Deployment"), Namespace: ns, Name: name},
		Container: container,
	}
}

// baseInput is the worked example the package doc's arithmetic follows:
// 10 on-demand nodes at $0.10 become 8 on-demand at $0.12 plus 4 spot at
// $0.04. Observed cost moves $1.00/h → $1.12/h.
func baseInput() Input {
	from, to := t0, t0.Add(hours(168))
	return Input{
		Cluster: testCluster,
		From:    from,
		To:      to,
		Timeline: []evidence.TimelinePoint{
			point(from, 1.00, 10),
			point(from.Add(hours(84)), 1.05, 11),
			point(to.Add(-time.Hour), 1.12, 12),
		},
		Start: &CostBasis{
			At:     from,
			Groups: []NodeGroup{{InstanceType: "m5.large", Nodes: 10, UnitUSDPerHour: 0.10}},
			Namespaces: []NamespaceDemand{
				{Namespace: "payments", MilliCPU: 12000, MemoryBytes: 32 << 30, Pods: 24},
				{Namespace: "search", MilliCPU: 8000, MemoryBytes: 16 << 30, Pods: 12},
			},
		},
		End: &CostBasis{
			At: to.Add(-time.Hour),
			Groups: []NodeGroup{
				{InstanceType: "m5.large", Nodes: 8, UnitUSDPerHour: 0.12},
				{InstanceType: "m5.large", Spot: true, Nodes: 4, UnitUSDPerHour: 0.04},
			},
			Namespaces: []NamespaceDemand{
				{Namespace: "payments", MilliCPU: 13000, MemoryBytes: 34 << 30, Pods: 26},
				{Namespace: "search", MilliCPU: 8000, MemoryBytes: 16 << 30, Pods: 12},
				{Namespace: "ml-batch", MilliCPU: 6000, MemoryBytes: 24 << 30, Pods: 8},
			},
		},
		Events: []evidence.EvidenceEvent{
			ev(t0.Add(hours(2)), evidence.EventPricingChange, evidence.SeverityInfo,
				evidence.ClusterSubject(testCluster), map[string]string{"instanceType": "m5.large", "from": "0.10", "to": "0.12"}),
			ev(t0.Add(hours(30)), evidence.EventSpotInterrupt, evidence.SeverityWarning,
				evidence.NodeSubject(testCluster, "ip-10-0-3-14"), nil),
			ev(t0.Add(hours(40)), evidence.EventDeploy, evidence.SeverityInfo,
				workloadSubject("ml-batch", "trainer"), map[string]string{"replicas": "8"}),
			ev(t0.Add(hours(50)), evidence.EventDeploy, evidence.SeverityInfo,
				workloadSubject("payments", "api"), map[string]string{"replicas": "26"}),
		},
		Actions: []LedgerAction{{
			At:                  t0.Add(hours(60)),
			Finished:            t0.Add(hours(60) + 20*time.Minute),
			Cluster:             testCluster,
			Fingerprint:         "a1b2c3d4e5f6",
			Mode:                "apply",
			Risk:                "low",
			Applied:             true,
			NodesRemoved:        2,
			Resizes:             7,
			CostBeforeHourlyUSD: 1.05,
			ProjectedHourlyUSD:  0.85,
		}},
	}
}

// memWithFixtures builds an evidence store holding everything baseInput
// cites, so citations can actually be resolved in tests.
func memWithFixtures(t *testing.T, in Input) *evidence.Memory {
	t.Helper()
	mem, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	for _, p := range in.Timeline {
		if err := mem.ObservePoint(in.Cluster, p); err != nil {
			t.Fatalf("ObservePoint: %v", err)
		}
	}
	for _, e := range in.Events {
		if err := mem.Append(e); err != nil {
			t.Fatalf("Append %s: %v", e.Kind, err)
		}
	}
	return mem
}
