// This file is package pricing_test (not pricing) so it can import pkg/plan,
// which imports pkg/pricing. It is the end-to-end guard for §7 trap 7:
// Fargate "nodes" are one pod each, so consolidating or deleting them is
// nonsense, and any plan that does it double-counts savings.
package pricing_test

import (
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
)

func ec2Node(name string) model.NodeSpec {
	return model.NodeSpec{
		Name: name, Ready: true, InstanceType: "m5.xlarge", Provider: "aws",
		Labels:      map[string]string{"kubernetes.io/hostname": name, "kubernetes.io/arch": "amd64"},
		Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
	}
}

// fargateVM mirrors what EKS actually registers for a Fargate pod: a "node"
// hosting exactly one pod, whose reported allocatable is comfortably larger
// than the pod's requests — which is precisely what makes it look like a
// gloriously underutilized consolidation candidate to node-centric logic.
// tainted mirrors the NoSchedule taint EKS puts on real Fargate VMs; the
// no-taint variant exists to prove SplitFargate does not depend on it.
func fargateVM(name, managedBy string, tainted bool) model.NodeSpec {
	n := model.NodeSpec{
		Name:        name,
		Ready:       true,
		Labels:      map[string]string{model.LabelComputeType: "fargate", "kubernetes.io/hostname": name},
		Capacity:    model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
		Allocatable: model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
		ManagedBy:   managedBy,
	}
	if tainted {
		n.Taints = []model.Taint{{Key: model.LabelComputeType, Value: "fargate", Effect: "NoSchedule"}}
	}
	return n
}

func testPod(uid, node, workload string, cpu, memMiB int64) model.PodSpec {
	return model.PodSpec{
		UID: uid, Name: uid, Namespace: "default", NodeName: node, Phase: "Running",
		Labels:   map[string]string{"app": workload},
		Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "default", Name: workload},
		Containers: []model.ContainerSpec{{Name: "app",
			Requests: model.Resources{MilliCPU: cpu, MemoryBytes: memMiB << 20}}},
	}
}

// mixedCluster: three EC2 nodes (one clearly consolidatable) plus two Fargate
// pods on their own VMs.
func mixedCluster(managedBy string, tainted bool) *model.ClusterSnapshot {
	fargatePod := func(uid, node, wl string, cpu, memMiB int64) model.PodSpec {
		p := testPod(uid, node, wl, cpu, memMiB)
		p.Tolerations = []model.Toleration{{Key: model.LabelComputeType, Operator: "Equal", Value: "fargate"}}
		return p
	}
	return &model.ClusterSnapshot{
		ClusterID: "mixed",
		Timestamp: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Nodes: []model.NodeSpec{
			ec2Node("node-a"), ec2Node("node-b"), ec2Node("node-c"),
			fargateVM("fargate-1", managedBy, tainted), fargateVM("fargate-2", managedBy, tainted),
		},
		Pods: []model.PodSpec{
			testPod("a1", "node-a", "wa", 2400, 3072),
			testPod("b1", "node-b", "wb", 2000, 3072),
			testPod("c1", "node-c", "wc", 400, 1024),
			testPod("c2", "node-c", "wd", 400, 1024),
			fargatePod("f1", "fargate-1", "wf1", 200, 512),
			fargatePod("f2", "fargate-2", "wf2", 1000, 2048),
		},
	}
}

func fargateNames(snap *model.ClusterSnapshot) map[string]bool {
	out := map[string]bool{}
	for i := range snap.Nodes {
		if snap.Nodes[i].IsFargate() {
			out[snap.Nodes[i].Name] = true
		}
	}
	return out
}

// assertNoFargateSteps is the actual regression assertion: no step of any kind
// may cordon, drain, target or delete a Fargate VM, and no Fargate pod may be
// evicted for consolidation.
func assertNoFargateSteps(t *testing.T, p *plan.Plan, fargate map[string]bool, fargatePods map[string]bool) {
	t.Helper()
	for _, s := range p.Steps {
		if fargate[s.Node] || fargate[s.TargetNode] {
			t.Fatalf("step %d (%s) touches Fargate VM %q/%q: %s", s.Seq, s.Type, s.Node, s.TargetNode, s.Detail)
		}
		if fargatePods[s.PodUID] {
			t.Fatalf("step %d (%s) evicts Fargate pod %q", s.Seq, s.Type, s.PodUID)
		}
	}
	for _, r := range p.Removals {
		if fargate[r.Node] {
			t.Fatalf("plan removes Fargate VM %q — a Fargate node is one pod, there is nothing to consolidate", r.Node)
		}
	}
	for _, note := range p.Notes {
		if strings.Contains(note, "fargate-") {
			t.Fatalf("plan note names a Fargate VM: %q", note)
		}
	}
}

// TestMixedClusterPlansNoFargateNodeSteps is the §7 trap 7 regression: a mixed
// cluster must never produce a Fargate node step. It covers the three states a
// snapshot can be in, weakest first.
func TestMixedClusterPlansNoFargateNodeSteps(t *testing.T) {
	catalog := pricing.Embedded()
	fargatePods := map[string]bool{"f1": true, "f2": true}

	t.Run("ManagedBy=fargate alone keeps them out of node removals", func(t *testing.T) {
		// Weakest state: ManagedBy set, no taint. RespectManagedNodes drops the
		// VMs from removal candidacy, so no Fargate node is cordoned, drained
		// or deleted — the headline trap. It does NOT stop binpack from using
		// a Fargate VM as a *destination* for evicted pods; only the split
		// does. See FARGATE-FINDINGS.md.
		snap := mixedCluster(model.ManagedByFargate, false)
		p, err := plan.Build(snap, nil, catalog, plan.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		fargate := fargateNames(snap)
		for _, r := range p.Removals {
			if fargate[r.Node] {
				t.Fatalf("plan removes Fargate VM %q", r.Node)
			}
		}
		for _, s := range p.Steps {
			if fargate[s.Node] {
				t.Fatalf("step %d (%s) acts on Fargate VM %q", s.Seq, s.Type, s.Node)
			}
		}
		// Not vacuous: the real underutilized EC2 node is still consolidated.
		if len(p.Removals) != 1 || p.Removals[0].Node != "node-c" {
			t.Fatalf("expected node-c to still be removed, got %+v", p.Removals)
		}
	})

	t.Run("a realistic EKS snapshot is clean end to end", func(t *testing.T) {
		// What a live cluster looks like: ManagedBy plus the NoSchedule taint
		// EKS puts on every Fargate VM, which also keeps binpack from placing
		// anything there.
		snap := mixedCluster(model.ManagedByFargate, true)
		p, err := plan.Build(snap, nil, catalog, plan.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		assertNoFargateSteps(t, p, fargateNames(snap), fargatePods)
		if len(p.Removals) != 1 || p.Removals[0].Node != "node-c" {
			t.Fatalf("expected node-c to still be removed, got %+v", p.Removals)
		}
	})

	t.Run("SplitFargate keeps them out structurally", func(t *testing.T) {
		// Label only, and no taint either — a collector that does not yet fill
		// ManagedBy, on a snapshot that carries no scheduling hint at all.
		// Unsplit, these VMs are 10 %-utilized removal candidates *and* free
		// real estate for evicted pods; split, they cannot be either.
		snap := mixedCluster("", false)
		nodes, fargate := pricing.SplitFargate(snap)
		if len(fargate) != 2 {
			t.Fatalf("split found %d Fargate pods, want 2", len(fargate))
		}
		p, err := plan.Build(nodes, nil, catalog, plan.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		assertNoFargateSteps(t, p, fargateNames(snap), fargatePods)
		if len(p.Removals) != 1 || p.Removals[0].Node != "node-c" {
			t.Fatalf("expected node-c to still be removed, got %+v", p.Removals)
		}
	})

	t.Run("both wirings produce the same steps", func(t *testing.T) {
		managed, err := plan.Build(mixedCluster(model.ManagedByFargate, true), nil, catalog, plan.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		split, _ := pricing.SplitFargate(mixedCluster("", false))
		splitPlan, err := plan.Build(split, nil, catalog, plan.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		if managed.Fingerprint != splitPlan.Fingerprint {
			t.Fatalf("plan fingerprints differ: %s vs %s", managed.Fingerprint, splitPlan.Fingerprint)
		}
	})
}

// TestMixedClusterCostAccounting: the mixed cluster's bill is the EC2 nodes
// plus the two quantized Fargate pods — the Fargate VMs' own 2 vCPU / 4 GB
// shapes contribute nothing, and the savings claimed for removing an EC2 node
// are unaffected by Fargate being in the cluster at all.
func TestMixedClusterCostAccounting(t *testing.T) {
	catalog := pricing.Embedded()
	snap := mixedCluster(model.ManagedByFargate, true)
	cc := catalog.SnapshotCost(snap)

	if len(cc.Nodes) != 3 {
		t.Fatalf("want 3 priced nodes, got %d: %+v", len(cc.Nodes), cc.Nodes)
	}
	// f1: 200m/512Mi → 0.25 vCPU / 1 GB. f2: 1000m/2Gi → 1 vCPU / 3 GB.
	wantF1 := 0.25*pricing.FargateVCPUHourlyUSD + 1*pricing.FargateGBHourlyUSD
	wantF2 := 1*pricing.FargateVCPUHourlyUSD + 3*pricing.FargateGBHourlyUSD
	if diff := cc.FargateHourlyUSD - (wantF1 + wantF2); diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("fargate hourly = %v, want %v", cc.FargateHourlyUSD, wantF1+wantF2)
	}
	wantTotal := 3*0.192 + wantF1 + wantF2
	if diff := cc.HourlyUSD - wantTotal; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cluster hourly = %v, want %v", cc.HourlyUSD, wantTotal)
	}
	if len(cc.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", cc.Warnings)
	}

	// The plan claims exactly one m5.xlarge of savings, not a cent of Fargate.
	p, err := plan.Build(snap, nil, catalog, plan.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if diff := p.CurrentHourlyUSD - wantTotal; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("plan current hourly = %v, want %v", p.CurrentHourlyUSD, wantTotal)
	}
	wantSavings := 0.192 * pricing.HoursPerMonth
	if diff := p.SavingsMonthlyUSD - wantSavings; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("claimed savings %v/mo, want one m5.xlarge (%v/mo)", p.SavingsMonthlyUSD, wantSavings)
	}
}
