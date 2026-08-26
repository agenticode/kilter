package ec2

import (
	"strings"
	"testing"
	"time"
)

// U15a — AWS Batch containment (docs/design/rds-batch-assessment.md §3.5, §7
// trap 14).
//
// The footgun these tests close: a Batch managed compute environment with
// minvCpus > 0 maintains instances "even if the compute environment is
// DISABLED". With an empty queue those instances present 30 days of near-zero
// CPU on a long-lived, fully-covered series — which clears every evidence gate
// this package has and reads as unambiguously oversized. Before this unit,
// kilter proposed m5.2xlarge → r5.xlarge at net $96.36/mo and confidence 0.71
// against exactly that fixture.

// batchFloorDays is the observation span §7 trap 14 specifies.
const batchFloorDays = 30

// idleFloorSeries is what an idle minvCpus floor looks like to CloudWatch:
// a full 30 days of basic-monitoring datapoints, all of them ~2 %. Nothing
// about the series is deficient — that is the entire problem.
func idleFloorSeries(ids ...string) []RecordedSeries {
	count := int((batchFloorDays * 24 * time.Hour) / basic)
	out := make([]RecordedSeries, 0, len(ids))
	for _, id := range ids {
		out = append(out, SyntheticSeries(id, MetricCPUUtilization, testNow, basic, count,
			[]float64{2, 2.4, 1.8, 2.1}))
	}
	return out
}

// batchAccount replays testdata/account-batch-managed.json — a Batch-managed
// instance, an EKS managed-node-group instance (which carries the ASG tag too)
// and a standalone instance — with the idle-floor series above.
func batchAccount(t *testing.T) *Snapshot {
	t.Helper()
	f := mustFixture(t, "testdata/account-batch-managed.json")
	f.Metrics = idleFloorSeries("i-0batchfloor", "i-1eksnode", "i-2plain")
	return mustCollect(t, f, CollectorConfig{
		Scope: "1234/us-east-1", Region: "us-east-1",
		Window: (batchFloorDays + 1) * 24 * time.Hour, CollectMemory: true,
	})
}

// The `fires alone` discipline every pkg/ec2 suppression follows, plus the two
// things a broad ownership gate has to get right: it must not swallow the
// k8s-tagged routing it sits next to, and it must not silence instances that
// are genuinely this domain's to size.
func TestBatchManagedInstanceIsSuppressedFiresAlone(t *testing.T) {
	rep := assess(t, batchAccount(t), nil)

	batchInst, ok := rep.For("i-0batchfloor")
	if !ok {
		t.Fatal("the Batch-managed instance is missing from the report")
	}
	s := only(t, batchInst, ReasonASGManaged)
	for _, want := range []string{TagASGName, "prod-batch-ce_Batch_9f2c7e1a3b", "AWS Batch", "launch template"} {
		if !strings.Contains(s.Reason, want) {
			t.Errorf("refusal must name %q so the operator can act on it: %s", want, s.Reason)
		}
	}
	if !batchInst.Excluded() {
		t.Error("a fleet-managed instance must report itself excluded from this domain")
	}
	if len(batchInst.Advisories) != 0 {
		t.Errorf("an excluded instance must not carry advisories: %+v", batchInst.Advisories)
	}
	if rep.Totals.SuppressedByCode[ReasonASGManaged] != 1 {
		t.Errorf("totals must count the suppression: %v", rep.Totals.SuppressedByCode)
	}

	// An EKS managed node group tags its instances with BOTH
	// aws:autoscaling:groupName and kubernetes.io/cluster/*. The k8s routing
	// is the more specific claim and must keep winning, or this unit would
	// quietly break the hand-off to the k8s-nodes pipeline.
	node, _ := rep.For("i-1eksnode")
	k8s := only(t, node, ReasonK8sTagged)
	if !strings.Contains(k8s.Reason, "k8s-nodes") {
		t.Errorf("an EKS node must still be routed to the k8s-nodes pipeline: %s", k8s.Reason)
	}

	// The control. Same shape, same metrics, no fleet tag: this one is ours,
	// and it gets a proposal. Without it the test above would still pass if
	// the gate suppressed the entire account.
	plain, _ := rep.For("i-2plain")
	if plain.Proposal == nil {
		t.Fatalf("a standalone instance must still be assessed: %+v", plain.Suppressions)
	}
	if plain.Excluded() {
		t.Error("a standalone instance must not be excluded")
	}
}

// §7 trap 14, stated as a test. The floor is not idle demand; it is bought
// capacity. Exactly one suppression, no proposal — and the counterfactual
// proves it is the ownership gate doing the work, not a lucky evidence gate.
func TestMinVCpusFloorIsNotReadAsIdleDemand(t *testing.T) {
	a, ok := assess(t, batchAccount(t), nil).For("i-0batchfloor")
	if !ok {
		t.Fatal("the idle-floor instance is missing from the report")
	}
	only(t, a, ReasonASGManaged)

	// The same 30 days of 2 % CPU on the same instance type with the fleet tag
	// removed. This is what kilter used to say about the floor, and every
	// assertion here is a gate the floor clears — window, coverage,
	// confidence, net savings. None of them is what stops the proposal.
	untagged := rec("i-0batchfloor", "m5.2xlarge")
	untagged.LaunchTime = testNow.AddDate(0, 0, -90)
	twin := mustCollect(t, &Fixture{
		InventoryPages: []DescribeInstancesOutput{{Reservations: []Reservation{{
			Instances: []InstanceRecord{untagged}}}}},
		Metrics: idleFloorSeries("i-0batchfloor"),
	}, CollectorConfig{
		Scope: "1234/us-east-1", Region: "us-east-1",
		Window: (batchFloorDays + 1) * 24 * time.Hour, CollectMemory: true,
	})
	b := single(t, assess(t, twin, nil))
	if b.Proposal == nil {
		t.Fatalf("the counterfactual no longer proposes anything (%+v); this test can no longer prove that the "+
			"ownership gate — rather than an evidence gate — is what protects the floor", b.Suppressions)
	}
	cfg := DefaultConfig()
	if got := b.Observation.Window.Duration(); got < cfg.MinWindow {
		t.Errorf("observed window %s is below MinWindow %s: the fixture no longer clears the window gate",
			got, cfg.MinWindow)
	}
	if b.Observation.Coverage < cfg.MinSampleCoverage {
		t.Errorf("coverage %.2f is below MinSampleCoverage %.2f: the fixture no longer clears the sample gate",
			b.Observation.Coverage, cfg.MinSampleCoverage)
	}
	if b.Proposal.Confidence < cfg.MinConfidence {
		t.Errorf("confidence %.2f is below MinConfidence %.2f", b.Proposal.Confidence, cfg.MinConfidence)
	}
	if b.Proposal.NetSavingsMonthlyUSD <= 0 {
		t.Errorf("the counterfactual proposal claims %s/mo", fmtUSD(b.Proposal.NetSavingsMonthlyUSD))
	}
	t.Logf("without the fleet tag kilter would propose %s → %s at net %s/mo, confidence %.2f — the recommendation "+
		"this suppression exists to stop", untagged.InstanceType, b.Proposal.InstanceType,
		fmtUSD(b.Proposal.NetSavingsMonthlyUSD), b.Proposal.Confidence)
}

// The selector has to be the AWS-applied tag, not a convention. An instance
// tagged by hand with a lookalike key is still this domain's to size, and an
// ASG member with an empty group value is still an ASG member.
func TestFleetSelectorIsTheReservedAWSTagOnly(t *testing.T) {
	if _, ok := (Instance{Tags: map[string]string{"autoscaling:groupName": "asg-1"}}).AutoScalingGroup(); ok {
		t.Error("a lookalike tag key must not suppress an instance")
	}
	if _, ok := (Instance{Tags: map[string]string{TagName: "asg-1"}}).AutoScalingGroup(); ok {
		t.Error("an unrelated tag must not suppress an instance")
	}
	g, ok := (Instance{Tags: map[string]string{TagASGName: "  batch-ce_Batch_1  "}}).AutoScalingGroup()
	if !ok || g != "batch-ce_Batch_1" {
		t.Errorf("AutoScalingGroup() = %q, %v", g, ok)
	}
	if g, ok := (Instance{Tags: map[string]string{TagASGName: ""}}).AutoScalingGroup(); !ok || g != "(unnamed)" {
		t.Errorf("an empty group value is still ASG membership, got %q, %v", g, ok)
	}
}
