package pricing

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// fargateNode builds a Fargate "node" the way EKS reports one: a single-pod VM
// carrying the compute-type label, with a capacity that has nothing to do with
// the bill and (deliberately, here) a fat instance type and a cost annotation
// to prove neither is ever used.
func fargateNode(name string) model.NodeSpec {
	return model.NodeSpec{
		Name:         name,
		Labels:       map[string]string{model.LabelComputeType: "fargate"},
		Ready:        true,
		Capacity:     model.Resources{MilliCPU: 96000, MemoryBytes: 384 << 30},
		Allocatable:  model.Resources{MilliCPU: 96000, MemoryBytes: 384 << 30},
		InstanceType: "m5.24xlarge",
		Provider:     "aws",
		HourlyCost:   99,
	}
}

func ec2Node(name string) model.NodeSpec {
	return model.NodeSpec{
		Name: name, Ready: true, InstanceType: "m5.xlarge", Provider: "aws",
		Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
	}
}

func pod(name, node string, cpu, memBytes int64) model.PodSpec {
	return model.PodSpec{
		UID: name, Name: name, Namespace: "default", NodeName: node, Phase: "Running",
		Containers: []model.ContainerSpec{{Name: "app",
			Requests: model.Resources{MilliCPU: cpu, MemoryBytes: memBytes}}},
	}
}

func TestNodeIsFargate(t *testing.T) {
	byLabel := fargateNode("f1")
	if !byLabel.IsFargate() {
		t.Error("compute-type label must classify the node as Fargate")
	}
	// A snapshot from a collector that fills ManagedBy but drops labels.
	byManagedBy := model.NodeSpec{Name: "f2", ManagedBy: model.ManagedByFargate}
	if !byManagedBy.IsFargate() {
		t.Error("ManagedBy=fargate must classify the node as Fargate")
	}
	if (&model.NodeSpec{Name: "n", ManagedBy: model.ManagedByKarpenter}).IsFargate() {
		t.Error("a karpenter node is not a Fargate node")
	}
	plain := ec2Node("n")
	if plain.IsFargate() {
		t.Error("a plain EC2 node is not a Fargate node")
	}
	var nilNode *model.NodeSpec
	if nilNode.IsFargate() {
		t.Error("nil node is not a Fargate node")
	}
}

func TestSplitFargate(t *testing.T) {
	snap := &model.ClusterSnapshot{
		ClusterID: "mixed",
		Timestamp: time.Unix(1000, 0).UTC(),
		Nodes: []model.NodeSpec{
			ec2Node("ec2-a"),
			fargateNode("fargate-1"),
			ec2Node("ec2-b"),
			{Name: "fargate-2", ManagedBy: model.ManagedByFargate, Ready: true},
		},
		Pods: []model.PodSpec{
			pod("p-ec2-a", "ec2-a", 500, gb(1)),
			pod("p-far-1", "fargate-1", 200, mem(512)),
			pod("p-pending", "", 100, mem(64)),
			pod("p-far-2", "fargate-2", 1000, gb(2)),
			pod("p-ec2-b", "ec2-b", 500, gb(1)),
		},
		Usage: []model.Usage{{PodUID: "p-far-1", MilliCPU: 50}},
	}

	nodes, fargate := SplitFargate(snap)

	if len(nodes.Nodes) != 2 || nodes.Nodes[0].Name != "ec2-a" || nodes.Nodes[1].Name != "ec2-b" {
		t.Fatalf("node-backed snapshot kept the wrong nodes: %+v", nodes.Nodes)
	}
	for _, n := range nodes.Nodes {
		if n.IsFargate() {
			t.Fatalf("Fargate node %q leaked into node math", n.Name)
		}
	}
	// Pending pods belong to the node-backed side: they have no Fargate VM yet.
	wantNodePods := []string{"p-ec2-a", "p-pending", "p-ec2-b"}
	var gotNodePods []string
	for _, p := range nodes.Pods {
		gotNodePods = append(gotNodePods, p.Name)
	}
	if !reflect.DeepEqual(gotNodePods, wantNodePods) {
		t.Fatalf("node-backed pods = %v, want %v", gotNodePods, wantNodePods)
	}
	// Fargate pods come out in snapshot order, tagged with their VM.
	if len(fargate) != 2 {
		t.Fatalf("want 2 Fargate pods, got %d", len(fargate))
	}
	if fargate[0].Pod.Name != "p-far-1" || fargate[0].NodeName != "fargate-1" {
		t.Fatalf("fargate[0] = %+v", fargate[0])
	}
	if fargate[1].Pod.Name != "p-far-2" || fargate[1].NodeName != "fargate-2" {
		t.Fatalf("fargate[1] = %+v", fargate[1])
	}
	// Learning state survives the split: recommend is reused verbatim for
	// Fargate containers, so their usage history must not be dropped.
	if len(nodes.Usage) != 1 || nodes.ClusterID != "mixed" || !nodes.Timestamp.Equal(snap.Timestamp) {
		t.Fatalf("snapshot metadata lost in the split: %+v", nodes)
	}
	// The returned slices are fresh: a caller rewriting them must not corrupt
	// the input snapshot.
	nodes.Nodes[0].Name = "rewritten"
	nodes.Pods[0].Name = "rewritten"
	if snap.Nodes[0].Name != "ec2-a" || snap.Pods[0].Name != "p-ec2-a" {
		t.Fatal("SplitFargate aliased the input snapshot's slices")
	}
}

func TestSplitFargateNoFargateAndNil(t *testing.T) {
	plain := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{ec2Node("a")},
		Pods:  []model.PodSpec{pod("p", "a", 100, gb(1))},
	}
	nodes, fargate := SplitFargate(plain)
	if fargate != nil {
		t.Fatalf("no Fargate nodes must yield no Fargate pods, got %+v", fargate)
	}
	if len(nodes.Nodes) != 1 || len(nodes.Pods) != 1 {
		t.Fatalf("plain cluster changed shape: %+v", nodes)
	}
	if n, f := SplitFargate(nil); n != nil || f != nil {
		t.Fatalf("nil snapshot must split to (nil, nil), got %v %v", n, f)
	}
}

// TestNodeHourlyCostRefusesFargateNodes: the whole bug in one assertion. A
// Fargate node's shape, instance type and even an explicit cost annotation are
// all not the bill.
func TestNodeHourlyCostRefusesFargateNodes(t *testing.T) {
	c := Embedded()
	n := fargateNode("f1")
	cost, src := c.NodeHourlyCost(&n)
	if cost != 0 || src != SourceFargateNode {
		t.Fatalf("Fargate node priced as a node: %v from %v", cost, src)
	}
	// Same node without the Fargate marks would have been priced — proving the
	// test is measuring the guard and not an accident of the fixture.
	n.Labels = nil
	if cost, src := c.NodeHourlyCost(&n); cost != 99 || src != SourceAnnotation {
		t.Fatalf("control case: %v from %v, want 99 from annotation", cost, src)
	}
}

// TestSnapshotCostFargatePrecedence pins §4.1.2: ProvisionedCapacity first,
// quantized requests second, node capacity never.
func TestSnapshotCostFargatePrecedence(t *testing.T) {
	c := Embedded()
	// Pod A: no annotation → quantized (200m/512Mi → 0.25 vCPU / 1 GB).
	podA := pod("a", "fargate-a", 200, mem(512))
	// Pod B: AWS stamped 1 vCPU / 2 GB, which outranks the quantizer's answer.
	podB := pod("b", "fargate-b", 200, mem(512))
	podB.ProvisionedCapacity = model.Resources{MilliCPU: 1000, MemoryBytes: gb(2)}

	snap := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{ec2Node("ec2-a"), fargateNode("fargate-a"), fargateNode("fargate-b")},
		Pods:  []model.PodSpec{pod("e", "ec2-a", 500, gb(1)), podA, podB},
	}
	cc := c.SnapshotCost(snap)

	// Fargate VMs never appear as nodes.
	if len(cc.Nodes) != 1 || cc.Nodes[0].Node != "ec2-a" {
		t.Fatalf("node costs = %+v, want only ec2-a", cc.Nodes)
	}
	if len(cc.Fargate) != 2 {
		t.Fatalf("want 2 Fargate pod costs, got %+v", cc.Fargate)
	}

	wantA := 0.014565             // P(0.25, 1)
	wantB := 0.04048 + 2*0.004445 // P(1, 2)
	if cc.Fargate[0].Pod != "default/a" || cc.Fargate[0].Source != SourceQuantized {
		t.Fatalf("pod a: %+v", cc.Fargate[0])
	}
	if cc.Fargate[0].Config != cfg(250, 1024) {
		t.Fatalf("pod a config = %v, want 0.25vCPU 1GB", cc.Fargate[0].Config)
	}
	if diff := cc.Fargate[0].HourlyUSD - wantA; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("pod a hourly = %v, want %v", cc.Fargate[0].HourlyUSD, wantA)
	}
	if cc.Fargate[1].Source != SourceProvisioned || cc.Fargate[1].Config != cfg(1000, 2048) {
		t.Fatalf("pod b must be priced from ProvisionedCapacity: %+v", cc.Fargate[1])
	}
	if diff := cc.Fargate[1].HourlyUSD - wantB; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("pod b hourly = %v, want %v", cc.Fargate[1].HourlyUSD, wantB)
	}
	if cc.Fargate[1].Node != "fargate-b" || cc.Fargate[1].UID != "b" {
		t.Fatalf("pod b identity: %+v", cc.Fargate[1])
	}

	// Totals: the cluster bill is the node bill plus the pod bill, and the
	// Fargate VMs' 96 vCPU / 384 GB shapes contribute nothing.
	wantTotal := 0.192 + wantA + wantB
	if diff := cc.HourlyUSD - wantTotal; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cluster hourly = %v, want %v", cc.HourlyUSD, wantTotal)
	}
	if diff := cc.FargateHourlyUSD - (wantA + wantB); diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("fargate hourly = %v, want %v", cc.FargateHourlyUSD, wantA+wantB)
	}
	if cc.MonthlyUSD != cc.HourlyUSD*HoursPerMonth {
		t.Fatal("monthly math broken")
	}
	for _, f := range cc.Fargate {
		if f.MonthlyUSD != f.HourlyUSD*HoursPerMonth {
			t.Fatalf("pod %s monthly math broken", f.Pod)
		}
	}

	// §4.1.2's production validation: pod b's annotation disagrees with the
	// quantizer, and that is reported, never absorbed.
	if len(cc.Warnings) != 1 || !strings.Contains(cc.Warnings[0], "default/b") {
		t.Fatalf("want one quantizer/AWS mismatch warning, got %v", cc.Warnings)
	}
	if !strings.Contains(cc.Warnings[0], "billed at the AWS value") {
		t.Fatalf("mismatch warning must say which value won: %q", cc.Warnings[0])
	}

	// Determinism: the same snapshot prices identically every time.
	if !reflect.DeepEqual(cc, c.SnapshotCost(snap)) {
		t.Fatal("SnapshotCost is not deterministic")
	}
}

// TestSnapshotCostFargateMispricingRegression states the size of the bug this
// unit fixes: the old path priced these pods by node capacity.
func TestSnapshotCostFargateMispricingRegression(t *testing.T) {
	c := Embedded()
	snap := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{fargateNode("f1")},
		Pods:  []model.PodSpec{pod("p", "f1", 200, mem(512))},
	}
	cc := c.SnapshotCost(snap)
	// Node-capacity pricing would have charged the m5.24xlarge annotation
	// ($99/h) or the 96 vCPU / 384 GB fallback. The pod bills $0.014565/h.
	if cc.HourlyUSD > 0.02 {
		t.Fatalf("cluster hourly = %v — Fargate is being priced by node shape again", cc.HourlyUSD)
	}
	if len(cc.Nodes) != 0 {
		t.Fatalf("Fargate VM leaked into node costs: %+v", cc.Nodes)
	}
}

func TestSnapshotCostFargateWarnings(t *testing.T) {
	c := Embedded()

	// An annotation that is not a valid configuration is not trusted.
	bogus := pod("bogus", "f1", 200, mem(512))
	bogus.ProvisionedCapacity = model.Resources{MilliCPU: 333, MemoryBytes: mem(700)}
	// A pod above the Fargate ceiling cannot be scheduled, so it has no bill.
	huge := pod("huge", "f1", 32000, gb(200))

	snap := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{fargateNode("f1")},
		Pods:  []model.PodSpec{bogus, huge},
	}
	cc := c.SnapshotCost(snap)

	if len(cc.Fargate) != 1 || cc.Fargate[0].Pod != "default/bogus" {
		t.Fatalf("want only the bogus pod priced, got %+v", cc.Fargate)
	}
	if cc.Fargate[0].Source != SourceQuantized || cc.Fargate[0].Config != cfg(250, 1024) {
		t.Fatalf("invalid ProvisionedCapacity must fall back to the quantizer: %+v", cc.Fargate[0])
	}
	if len(cc.Warnings) != 2 {
		t.Fatalf("want 2 warnings, got %v", cc.Warnings)
	}
	if !strings.Contains(cc.Warnings[0], "not a valid Fargate configuration") {
		t.Fatalf("warning[0] = %q", cc.Warnings[0])
	}
	if !strings.Contains(cc.Warnings[1], "left unpriced") || !strings.Contains(cc.Warnings[1], "default/huge") {
		t.Fatalf("warning[1] = %q", cc.Warnings[1])
	}
	// No fabricated bill for the unschedulable pod, in either direction.
	if diff := cc.HourlyUSD - 0.014565; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cluster hourly = %v, want only the priceable pod", cc.HourlyUSD)
	}
}

// TestSnapshotCostUnnamedFargateNode: an unnamed Fargate node must not capture
// every unscheduled pod (both carry the empty node name).
func TestSnapshotCostUnnamedFargateNode(t *testing.T) {
	c := Embedded()
	snap := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{{Labels: map[string]string{model.LabelComputeType: "fargate"}}},
		Pods:  []model.PodSpec{pod("pending", "", 200, mem(512))},
	}
	cc := c.SnapshotCost(snap)
	if len(cc.Fargate) != 0 || cc.HourlyUSD != 0 {
		t.Fatalf("unscheduled pod priced as Fargate: %+v", cc)
	}
}

func TestFargatePodConfigPrecedenceUnit(t *testing.T) {
	// Requests only.
	p := pod("p", "f1", 200, mem(512))
	if got, src, err := FargatePodConfig(&p); err != nil || got != cfg(250, 1024) || src != SourceQuantized {
		t.Fatalf("quantized path: %v %v %v", got, src, err)
	}
	// Annotation wins, even when it is smaller than the quantizer's answer —
	// AWS's stamp is the bill, not our model of it.
	p.ProvisionedCapacity = model.Resources{MilliCPU: 250, MemoryBytes: mem(512)}
	if got, src, err := FargatePodConfig(&p); err != nil || got != cfg(250, 512) || src != SourceProvisioned {
		t.Fatalf("provisioned path: %v %v %v", got, src, err)
	}
	if _, _, err := FargatePodConfig(nil); err == nil {
		t.Fatal("nil pod must error")
	}
	if _, err := QuantizePod(nil); err == nil {
		t.Fatal("QuantizePod(nil) must error")
	}
	// A pod above the ceiling is an error, not a silently clamped price.
	big := pod("big", "f1", 20000, gb(1))
	if _, _, err := FargatePodConfig(&big); err == nil {
		t.Fatal("over-ceiling pod must not resolve to a configuration")
	}
	if cost, _, err := Embedded().FargatePodHourlyCost(&big); err == nil || cost != 0 {
		t.Fatalf("over-ceiling pod priced at %v (err %v); want 0 and an error", cost, err)
	}
	small := pod("small", "f1", 200, mem(512))
	cost, src, err := Embedded().FargatePodHourlyCost(&small)
	if err != nil || src != SourceQuantized {
		t.Fatalf("FargatePodHourlyCost = %v %v %v", cost, src, err)
	}
	if diff := cost - 0.014565; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("FargatePodHourlyCost = %v, want 0.014565", cost)
	}
	if FargatePodWarnings(nil) != nil {
		t.Fatal("nil pod has no warnings")
	}
}

// TestEKSFargateHasNoSpotAndNoArm enforces §7 trap 3 structurally. It is a
// reflection test on purpose: a future edit that adds a spot discount or an ARM
// rate to the EKS rate table fails here, at the type, rather than shipping an
// unactionable recommendation.
func TestEKSFargateHasNoSpotAndNoArm(t *testing.T) {
	rt := reflect.TypeOf(FargateRates{})
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		for _, banned := range []string{"spot", "arm", "graviton"} {
			if strings.Contains(name, banned) {
				t.Fatalf("FargateRates.%s: EKS Fargate has no %s dimension", rt.Field(i).Name, banned)
			}
		}
	}
	// Platform has no exported field, so no caller can mint another platform.
	pt := reflect.TypeOf(Platform{})
	for i := 0; i < pt.NumField(); i++ {
		if pt.Field(i).IsExported() {
			t.Fatalf("Platform.%s is exported: a caller could construct a spot/arm platform", pt.Field(i).Name)
		}
	}
	if !EKSLinuxX86.Valid() || EKSLinuxX86.String() != "eks/linux/x86_64/on-demand" {
		t.Fatalf("EKSLinuxX86 = %q", EKSLinuxX86)
	}
	// The zero Platform is not a pricing platform.
	var zero Platform
	if zero.Valid() {
		t.Fatal("the zero Platform must be invalid")
	}
	if (FargateRates{VCPUHourlyUSD: 1, GBHourlyUSD: 1}).valid() {
		t.Fatal("rates without a valid platform must not price")
	}
}

func TestFargateRatesOverrideSeam(t *testing.T) {
	good := `{"vcpuHourlyUSD": 0.05, "gbHourlyUSD": 0.006}`
	r, err := LoadFargateRates(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if r.VCPUHourlyUSD != 0.05 || r.GBHourlyUSD != 0.006 || r.Platform != EKSLinuxX86 {
		t.Fatalf("override = %+v", r)
	}

	// An override file cannot smuggle in an ECS-only dimension.
	bad := []string{
		`{"vcpuHourlyUSD": 0.05, "gbHourlyUSD": 0.006, "spotDiscount": 0.7}`,
		`{"vcpuHourlyUSD": 0.05, "gbHourlyUSD": 0.006, "armVCPUHourlyUSD": 0.03}`,
		`{"vcpuHourlyUSD": 0.05}`, // missing GB rate
		`{"vcpuHourlyUSD": -1, "gbHourlyUSD": 0.006}`,
		`{"vcpuHourlyUSD": 0, "gbHourlyUSD": 0}`,
		`{"platform": "ecs/linux/arm64/spot", "vcpuHourlyUSD": 0.05, "gbHourlyUSD": 0.006}`,
		`not json`,
	}
	for _, s := range bad {
		if got, err := LoadFargateRates(strings.NewReader(s)); err == nil {
			t.Errorf("must reject %s (got %+v)", s, got)
		}
	}
	if _, err := LoadFargateRatesFile("/nonexistent/fargate.json"); err == nil {
		t.Error("missing rate file must error")
	}
}

func TestCatalogFargateRates(t *testing.T) {
	c := Embedded()
	if c.FargateRates() != DefaultFargateRates() {
		t.Fatalf("embedded catalog rates = %+v", c.FargateRates())
	}
	// A zero-value catalog (constructed by some other path) must still price
	// Fargate at the baseline rather than at zero.
	var zero Catalog
	if zero.FargateRates() != DefaultFargateRates() {
		t.Fatal("zero catalog must fall back to the embedded rates")
	}
	var nilCat *Catalog
	if nilCat.FargateRates() != DefaultFargateRates() {
		t.Fatal("nil catalog must fall back to the embedded rates")
	}

	custom := FargateRates{Platform: EKSLinuxX86, VCPUHourlyUSD: 0.05, GBHourlyUSD: 0.006}
	c2, err := c.WithFargateRates(custom)
	if err != nil {
		t.Fatal(err)
	}
	if c2.FargateRates() != custom {
		t.Fatalf("override not applied: %+v", c2.FargateRates())
	}
	if c.FargateRates() != DefaultFargateRates() {
		t.Fatal("WithFargateRates mutated the original catalog")
	}
	if _, err := c.WithFargateRates(FargateRates{}); err == nil {
		t.Fatal("invalid rates must be rejected, not silently ignored")
	}

	// The override reaches SnapshotCost.
	snap := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{fargateNode("f1")},
		Pods:  []model.PodSpec{pod("p", "f1", 200, mem(512))},
	}
	want := 0.25*0.05 + 1*0.006
	if diff := c2.SnapshotCost(snap).HourlyUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("overridden cluster hourly = %v, want %v", c2.SnapshotCost(snap).HourlyUSD, want)
	}
}

func TestParseCapacityProvisioned(t *testing.T) {
	// Every valid configuration round-trips through the annotation form AWS
	// stamps, which is what makes the production validation in §4.1.2 possible.
	for _, c := range FargateConfigs() {
		got, err := ParseCapacityProvisioned(c.String())
		if err != nil {
			t.Fatalf("%v (%q): %v", c, c.String(), err)
		}
		if got != c {
			t.Fatalf("%q round-tripped to %v", c.String(), got)
		}
	}
	if got, err := ParseCapacityProvisioned("0.25vCPU 0.5GB"); err != nil || got != cfg(250, 512) {
		t.Fatalf("AWS example: %v %v", got, err)
	}
	if got, err := ParseCapacityProvisioned("  2VCPU   9gb  "); err != nil || got != cfg(2000, 9216) {
		t.Fatalf("case/space tolerance: %v %v", got, err)
	}
	bad := []string{
		"", "0.25vCPU", "0.25vCPU 1GB extra", "0.3vCPU 1GB", "1vCPU 9GB",
		"abcvCPU 1GB", "0.25vCPU 1GiB", "0.25 1GB", "-1vCPU 1GB", "0vCPU 1GB",
	}
	for _, s := range bad {
		if got, err := ParseCapacityProvisioned(s); err == nil {
			t.Errorf("must reject %q (got %v)", s, got)
		}
	}
}

func TestFargateConfigAccessors(t *testing.T) {
	c := cfg(250, 512)
	if c.VCPU() != 0.25 || c.MemoryGB() != 0.5 || c.MemoryBytes() != mem(512) {
		t.Fatalf("accessors: %+v", c)
	}
	if c.Resources() != (model.Resources{MilliCPU: 250, MemoryBytes: mem(512)}) {
		t.Fatalf("Resources() = %v", c.Resources())
	}
	if c.IsZero() || !(FargateConfig{}).IsZero() {
		t.Fatal("IsZero broken")
	}
	if got, ok := FargateConfigFor(model.Resources{MilliCPU: 250, MemoryBytes: mem(512)}); !ok || got != c {
		t.Fatalf("FargateConfigFor = %v %v", got, ok)
	}
	if _, ok := FargateConfigFor(model.Resources{MilliCPU: 251, MemoryBytes: mem(512)}); ok {
		t.Fatal("FargateConfigFor must match exactly")
	}
	if FargateEffectiveRequests(nil) != (model.Resources{}) {
		t.Fatal("nil pod has no requests")
	}
}
