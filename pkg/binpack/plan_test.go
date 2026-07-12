package binpack

import (
	"fmt"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

func awsCandidates(t testing.TB) []pricing.InstanceType {
	return pricing.Embedded().Candidates("aws", "amd64")
}

func TestPlanNodesPacksEverything(t *testing.T) {
	var pods []*model.PodSpec
	for i := 0; i < 20; i++ {
		pods = append(pods, pod(fmt.Sprintf("p%02d", i), 500, 1024, fmt.Sprintf("w%d", i%4)))
	}
	plan := PlanNodes(pods, awsCandidates(t), PlanOptions{})
	if len(plan.Unschedulable) != 0 {
		t.Fatalf("unschedulable: %+v", plan.Unschedulable)
	}
	placed := 0
	for _, n := range plan.Nodes {
		placed += len(n.PodUIDs)
		if !n.Allocatable.Fits(n.Used) {
			t.Fatalf("node %s overpacked: used %s alloc %s", n.Name, n.Used, n.Allocatable)
		}
	}
	if placed != 20 {
		t.Fatalf("placed %d/20", placed)
	}
	if plan.TotalHourlyUSD <= 0 || plan.TotalMonthlyUSD != plan.TotalHourlyUSD*pricing.HoursPerMonth {
		t.Fatalf("cost math wrong: %+v", plan)
	}
	// 20 × (500m,1Gi) = 10 vCPU / 20 GiB total. A sane plan costs less than a
	// naive m5.4xlarge (16 vCPU/64Gi @ .768) + should beat 2×m5.2xlarge too.
	if plan.TotalHourlyUSD > 0.768 {
		t.Fatalf("plan too expensive: $%.3f/h for 10vCPU/20GiB", plan.TotalHourlyUSD)
	}
}

func TestPlanUsesCheapEfficientShapes(t *testing.T) {
	// CPU-hungry pods: compute-optimized (c5/c6i) should dominate the plan.
	var pods []*model.PodSpec
	for i := 0; i < 16; i++ {
		pods = append(pods, pod(fmt.Sprintf("c%02d", i), 900, 512, "cpu-app"))
	}
	plan := PlanNodes(pods, awsCandidates(t), PlanOptions{})
	if len(plan.Unschedulable) != 0 {
		t.Fatalf("unschedulable: %+v", plan.Unschedulable)
	}
	cpuOptimized := 0
	for _, n := range plan.Nodes {
		if n.Type.Family == "c5" || n.Type.Family == "c6i" {
			cpuOptimized++
		}
	}
	if cpuOptimized == 0 {
		t.Fatalf("expected compute-optimized nodes for cpu-heavy pods, got %+v", planTypes(plan))
	}
}

func planTypes(p NodePlan) []string {
	var out []string
	for _, n := range p.Nodes {
		out = append(out, n.Type.Name)
	}
	return out
}

func TestPlanDaemonSetOverhead(t *testing.T) {
	var pods []*model.PodSpec
	for i := 0; i < 8; i++ {
		pods = append(pods, pod(fmt.Sprintf("p%d", i), 800, 1024, "app"))
	}
	// Heavy enough that a node absorbing it can no longer hold all 8 pods.
	ds := pod("ds-template", 1500, 2048, "logger")
	ds.Workload.Kind = model.KindDaemonSet

	without := PlanNodes(pods, awsCandidates(t), PlanOptions{})
	with := PlanNodes(pods, awsCandidates(t), PlanOptions{DaemonSetPods: []model.PodSpec{*ds}})
	if len(with.Unschedulable) != 0 || len(without.Unschedulable) != 0 {
		t.Fatal("nothing should be unschedulable")
	}
	if with.TotalHourlyUSD <= without.TotalHourlyUSD {
		t.Fatalf("DS overhead must increase cost: with=$%.4f without=$%.4f",
			with.TotalHourlyUSD, without.TotalHourlyUSD)
	}
}

func TestPlanUnschedulableGiant(t *testing.T) {
	giant := pod("giant", 200000, 4<<20, "huge") // 200 vCPU
	plan := PlanNodes([]*model.PodSpec{giant}, awsCandidates(t), PlanOptions{})
	if len(plan.Unschedulable) != 1 {
		t.Fatalf("giant must be unschedulable, got %+v", plan)
	}
	if len(plan.Unschedulable[0].Reasons) == 0 {
		t.Fatal("reasons must be populated")
	}
}

func TestPlanSpotCheaper(t *testing.T) {
	var pods []*model.PodSpec
	for i := 0; i < 10; i++ {
		pods = append(pods, pod(fmt.Sprintf("p%d", i), 1000, 2048, "app"))
	}
	onDemand := PlanNodes(pods, awsCandidates(t), PlanOptions{})
	spot := PlanNodes(pods, awsCandidates(t), PlanOptions{Spot: true})
	if spot.TotalHourlyUSD >= onDemand.TotalHourlyUSD {
		t.Fatalf("spot plan must be cheaper: spot=$%.4f od=$%.4f",
			spot.TotalHourlyUSD, onDemand.TotalHourlyUSD)
	}
}

func TestPlanRespectsNodeSelector(t *testing.T) {
	p := pod("zoned", 500, 512, "app")
	p.NodeSelector = map[string]string{"topology.kubernetes.io/zone": "us-east-1a"}

	blocked := PlanNodes([]*model.PodSpec{p}, awsCandidates(t), PlanOptions{})
	if len(blocked.Unschedulable) != 1 {
		t.Fatal("zone selector must block planning without matching NodeLabels")
	}
	allowed := PlanNodes([]*model.PodSpec{p}, awsCandidates(t), PlanOptions{
		NodeLabels: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
	})
	if len(allowed.Unschedulable) != 0 {
		t.Fatalf("zone selector should be satisfied: %+v", allowed.Unschedulable)
	}
}

func TestPlanArchFiltering(t *testing.T) {
	p := pod("armv", 500, 512, "app")
	p.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}
	plan := PlanNodes([]*model.PodSpec{p}, pricing.Embedded().Candidates("aws", ""), PlanOptions{})
	if len(plan.Unschedulable) != 0 {
		t.Fatalf("arm pod should plan onto arm node: %+v", plan.Unschedulable)
	}
	for _, n := range plan.Nodes {
		if n.Type.Arch != "arm64" {
			t.Fatalf("arm pod landed on %s (%s)", n.Type.Name, n.Type.Arch)
		}
	}
}

func TestPlanDeterminism(t *testing.T) {
	mk := func() NodePlan {
		var pods []*model.PodSpec
		for i := 0; i < 50; i++ {
			pods = append(pods, pod(fmt.Sprintf("p%02d", i), int64(100+(i*137)%1500), int64(256+(i*911)%4096), fmt.Sprintf("w%d", i%7)))
		}
		return PlanNodes(pods, awsCandidates(t), PlanOptions{})
	}
	a, b := mk(), mk()
	if fmt.Sprintf("%v", planTypes(a)) != fmt.Sprintf("%v", planTypes(b)) {
		t.Fatalf("plans differ: %v vs %v", planTypes(a), planTypes(b))
	}
	if a.TotalHourlyUSD != b.TotalHourlyUSD {
		t.Fatal("plan cost differs across runs")
	}
}

func TestPlanBurstablePolicy(t *testing.T) {
	// Sustained 900m pods: default plan must avoid t3 even though t3.medium
	// is nominally the cheapest per vCPU; AllowBurstable opts back in.
	var pods []*model.PodSpec
	for i := 0; i < 4; i++ {
		pods = append(pods, pod(fmt.Sprintf("s%d", i), 900, 512, "sustained"))
	}
	def := PlanNodes(pods, awsCandidates(t), PlanOptions{})
	for _, n := range def.Nodes {
		if n.Type.Burstable {
			t.Fatalf("default plan picked burstable %s", n.Type.Name)
		}
	}
	allowed := PlanNodes(pods, awsCandidates(t), PlanOptions{AllowBurstable: true})
	if allowed.TotalHourlyUSD >= def.TotalHourlyUSD {
		t.Fatalf("burstable-allowed plan should be nominally cheaper: %v vs %v",
			allowed.TotalHourlyUSD, def.TotalHourlyUSD)
	}
}

func TestPlanNoCandidates(t *testing.T) {
	plan := PlanNodes([]*model.PodSpec{pod("p", 100, 128, "w")}, nil, PlanOptions{})
	if len(plan.Unschedulable) != 1 || len(plan.Nodes) != 0 {
		t.Fatalf("no candidates: %+v", plan)
	}
}

func BenchmarkPlan10kPods(b *testing.B) {
	var pods []*model.PodSpec
	for i := 0; i < 10000; i++ {
		pods = append(pods, pod(fmt.Sprintf("p%05d", i), int64(50+(i*37)%950), int64(64+(i*253)%3900), fmt.Sprintf("w%d", i%100)))
	}
	cands := pricing.Embedded().Candidates("aws", "amd64")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		plan := PlanNodes(pods, cands, PlanOptions{})
		if len(plan.Unschedulable) != 0 {
			b.Fatal("unexpected unschedulable")
		}
	}
}
