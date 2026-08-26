package crossover

import (
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// TestSummaryStatesEverythingItAssumed: the assumptions are part of the
// answer, not a footnote, and the two most dangerous ones — steady state and
// no commitment netting — must always be printed.
func TestSummaryStatesEverythingItAssumed(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)}, sparseOpts())
	s := rep.Summary()
	for _, want := range []string{
		"Fargate ⇄ EC2 crossover (2026-08-26T12:00:00Z)",
		"F(P): $0.0874/h",
		"E(P): $0.0960/h",
		"0.25vCPU 1GB",
		"m5.large",
		"Break-even density",
		"per-second billing advantage",
		"commitments are not netted",
		"no Fargate Spot, no Graviton",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary is missing %q:\n%s", want, s)
		}
	}
}

// TestGravitonIsCalledOutOnlyWhenTheEC2SideUsesIt keeps the assumption list
// honest: it states what this run actually did, not a generic disclaimer.
func TestGravitonIsCalledOutOnlyWhenTheEC2SideUsesIt(t *testing.T) {
	const graviton = "buys Graviton"
	amd := Analyze(testNow, PodSet{Pods: mkPods(40, 1000, 4096)},
		Options{Candidates: []pricing.InstanceType{m5xlarge, m52xlarge}})
	if strings.Contains(strings.Join(amd.Assumptions, "\n"), graviton) {
		t.Errorf("an all-amd64 pack must not mention Graviton")
	}
	arm := Analyze(testNow, PodSet{Pods: mkPods(40, 1000, 4096)},
		Options{Candidates: []pricing.InstanceType{m5xlarge, m52xlarge, m7g2xlarge}})
	sawArm := false
	for _, tp := range arm.EC2.NodeTypes {
		sawArm = sawArm || tp.Arch == "arm64"
	}
	if !sawArm {
		t.Fatalf("expected the Graviton shape to win part of the pack, got %+v", arm.EC2.NodeTypes)
	}
	if !strings.Contains(strings.Join(arm.Assumptions, "\n"), graviton) {
		t.Errorf("a Graviton pack must say so — EKS Fargate cannot match it:\n%v", arm.Assumptions)
	}
}

// TestSpotIsCalledOutAndHasNoFargateMirror: pricing the node side on spot must
// be disclosed, and there is structurally no Fargate spot side to compare it
// against (pricing.Platform has exactly one value).
func TestSpotIsCalledOutAndHasNoFargateMirror(t *testing.T) {
	spotType := m5large
	spotType.SpotHourlyUSD = 0.035
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)},
		Options{Candidates: []pricing.InstanceType{spotType}, Spot: true})
	closeTo(t, "E(P) at spot", rep.EC2.HourlyUSD, 0.035, 1e-9)
	if !rep.EC2.NodeTypes[0].Spot {
		t.Errorf("spot nodes must be labelled spot")
	}
	if !strings.Contains(strings.Join(rep.Assumptions, "\n"), "spot rates") {
		t.Errorf("spot pricing must be disclosed: %v", rep.Assumptions)
	}
	if rep.Verdict != VerdictEC2 {
		t.Errorf("verdict = %q, want %q: spot at $0.035/h beats Fargate's $0.0874/h", rep.Verdict, VerdictEC2)
	}
}

// TestDensityAdviceNamesTheLever: the break-even is only useful if it says
// what to do about it.
func TestDensityAdviceNamesTheLever(t *testing.T) {
	sparse := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)}, sparseOpts())
	if got := densityAdvice(sparse.Density); !strings.Contains(got, "denser would flip it") {
		t.Errorf("below break-even advice = %q", got)
	}
	dense := Analyze(testNow, PodSet{Pods: mkPods(40, 1000, 4096)},
		Options{Candidates: []pricing.InstanceType{m5xlarge, m52xlarge}})
	if got := densityAdvice(dense.Density); !strings.Contains(got, "above it") {
		t.Errorf("above break-even advice = %q", got)
	}
	if got := densityAdvice(Density{Defined: true}); !strings.Contains(got, "no price") {
		t.Errorf("undefined break-even advice = %q", got)
	}
}

// TestInsightIsAdvisory: the only thing this package hands the rest of Kilter
// is a model.Insight — a finding, never an action.
func TestInsightIsAdvisory(t *testing.T) {
	rep := Analyze(testNow, PodSet{Pods: mkPods(6, 200, 512)}, sparseOpts())
	in := rep.Insight()
	if in.Kind != InsightKind || in.Severity != "info" || in.At != testNow {
		t.Fatalf("insight = %+v", in)
	}
	if in.Message != rep.Headline() {
		t.Errorf("insight message must be the headline verbatim")
	}
	if in.Workload != (model.WorkloadRef{}) || in.Node != "" || in.HorizonHours != 0 {
		t.Errorf("a crossover insight targets the pod set, not a workload or node: %+v", in)
	}
}

// TestTerminalPodsAreExcludedAndExplained: a completed Job pod holds no node
// capacity, so it must not distort E(P) — but on Fargate it does keep billing,
// which is a different finding and must not be silently swallowed.
func TestTerminalPodsAreExcludedAndExplained(t *testing.T) {
	pods := mkPods(6, 200, 512)
	done := mkPod("finished", 4000, 8192)
	done.Spec.Phase = "Succeeded"
	rep := Analyze(testNow, PodSet{Pods: append(pods, done)}, sparseOpts())

	if rep.Fargate.Pods != 6 {
		t.Errorf("terminal pod counted: %d pods priced", rep.Fargate.Pods)
	}
	closeTo(t, "F(P) ignores the terminal pod", rep.Fargate.HourlyUSD, 6*0.014565, 1e-9)
	if !hasWarning(rep, "keeps billing until it is deleted") {
		t.Errorf("the Fargate job-hygiene consequence must be stated: %v", rep.Warnings)
	}
}

// TestDaemonSetPodsCollapseToTemplates: three pods of one DaemonSet are one
// per-node overhead, not three.
func TestDaemonSetPodsCollapseToTemplates(t *testing.T) {
	ds := func(uid string) model.PodSpec {
		return model.PodSpec{
			UID: uid, Name: uid, Namespace: "kube-system",
			Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "agent"},
			Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: 200, MemoryBytes: 256 * mib}}},
		}
	}
	one := Analyze(testNow, PodSet{Pods: mkPods(6, 300, 512), DaemonSets: []model.PodSpec{ds("a")}},
		Options{Candidates: []pricing.InstanceType{m5large}})
	three := Analyze(testNow, PodSet{Pods: mkPods(6, 300, 512), DaemonSets: []model.PodSpec{ds("a"), ds("b"), ds("c")}},
		Options{Candidates: []pricing.InstanceType{m5large}})

	if three.EC2.DaemonSetTemplates != 1 {
		t.Errorf("templates = %d, want 1 per DaemonSet", three.EC2.DaemonSetTemplates)
	}
	closeTo(t, "E(P) is unchanged by observing more copies of the same DaemonSet",
		three.EC2.HourlyUSD, one.EC2.HourlyUSD, 1e-9)
	if !hasWarning(three, "collapsed into per-DaemonSet templates") {
		t.Errorf("collapsing must be reported: %v", three.Warnings)
	}
}
