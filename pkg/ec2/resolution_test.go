package ec2

import (
	"strings"
	"testing"
	"time"
)

// The same workload, twice: once as CloudWatch publishes it with detailed
// monitoring (1-minute datapoints) and once as it publishes it for free
// (5-minute averages of the very same minutes). Nothing about the machine
// changes — only what the account is allowed to see.
//
// The spike is 1 minute of 100 % CPU in every 20. At 1-minute resolution it is
// the peak. Averaged into a 5-minute bucket it becomes 28 %, and the sizer,
// reasoning honestly from what it was given, proposes an instance HALF the
// size. That is the cost of basic monitoring, and this package's answer is to
// name it in every report rather than quietly assume it away.
func TestMetricResolutionChangesTheObservedPeak(t *testing.T) {
	const id = "i-spike"
	fine := SyntheticSeries(id, MetricCPUUtilization, testNow, time.Minute,
		int((time.Duration(windowDays)*24*time.Hour)/time.Minute),
		[]float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 100})
	coarse := Downsample(fine, 5)
	coarse.InstanceID = id

	mem := series(id, memAgent, time.Minute, 28, 30, 29)
	coarseMem := Downsample(mem, 5)

	// Ground truth, before anyone's resolution: the peak really is 100 %.
	if peak := maxOf(fine.Points); peak != 100 {
		t.Fatalf("fixture peak = %v, want 100", peak)
	}
	if peak := maxOf(coarse.Points); peak != 28 {
		t.Fatalf("5-minute average peak = %v, want 28 (the 1-minute peak is averaged away)", peak)
	}

	detailedRep := assess(t, collectFor(t,
		[]InstanceRecord{rec(id, "m5.xlarge", detailed)},
		[]RecordedSeries{fine, mem}), nil)
	basicRep := assess(t, collectFor(t,
		[]InstanceRecord{rec(id, "m5.xlarge")},
		[]RecordedSeries{coarse, coarseMem}), nil)

	d := single(t, detailedRep)
	b := single(t, basicRep)

	if d.Observation.PeriodSeconds != PeriodDetailedSeconds || b.Observation.PeriodSeconds != PeriodBasicSeconds {
		t.Fatalf("periods = %d / %d, want %d / %d", d.Observation.PeriodSeconds, b.Observation.PeriodSeconds,
			PeriodDetailedSeconds, PeriodBasicSeconds)
	}
	if !(b.Observation.PeakCPUPercent < d.Observation.PeakCPUPercent) {
		t.Fatalf("coarse peak %.1f%% is not lower than fine peak %.1f%%",
			b.Observation.PeakCPUPercent, d.Observation.PeakCPUPercent)
	}

	// Both proposals are honest about what THEY saw...
	for _, a := range []Assessment{d, b} {
		if a.Proposal == nil {
			t.Fatalf("%ds: expected a proposal, got %+v", a.Observation.PeriodSeconds, a.Suppressions)
		}
		if a.Proposal.Spec.Resources.MilliCPU < a.Observation.PeakCPUMilli {
			t.Fatalf("%ds: proposal %d mCPU is below its own observed peak %d mCPU",
				a.Observation.PeriodSeconds, a.Proposal.Spec.Resources.MilliCPU, a.Observation.PeakCPUMilli)
		}
	}
	// ...and the coarse one is nevertheless below the peak that really happened.
	if b.Proposal.Spec.Resources.MilliCPU >= d.Observation.PeakCPUMilli {
		t.Fatalf("this test is no longer demonstrating anything: the 5-minute proposal (%d mCPU) already "+
			"covers the true 1-minute peak (%d mCPU)",
			b.Proposal.Spec.Resources.MilliCPU, d.Observation.PeakCPUMilli)
	}

	// So the rule has to be visible in the output, not implied.
	if !strings.Contains(b.Observation.ResolutionNote, "CANNOT be recovered") {
		t.Errorf("the 5-minute report does not state what it cannot see:\n%s", b.Observation.ResolutionNote)
	}
	if b.Observation.ResolutionHeadroom <= 1 {
		t.Errorf("coarse resolution headroom = %v, want > 1", b.Observation.ResolutionHeadroom)
	}
	if d.Observation.ResolutionHeadroom != 1 {
		t.Errorf("1-minute data needs no inflation, got %v", d.Observation.ResolutionHeadroom)
	}
	if !(b.Confidence.Score < d.Confidence.Score) {
		t.Errorf("coarse confidence %.3f must be below fine confidence %.3f",
			b.Confidence.Score, d.Confidence.Score)
	}
	if basicRep.Totals.CoarseResolution != 1 || detailedRep.Totals.CoarseResolution != 0 {
		t.Errorf("totals must count coarse-resolution instances: %d / %d",
			basicRep.Totals.CoarseResolution, detailedRep.Totals.CoarseResolution)
	}
}

// The resolution rule is stated in prose, in both directions, and the prose is
// part of the contract.
func TestResolutionNoteStatesTheRule(t *testing.T) {
	coarse := ResolutionNote(PeriodBasicSeconds, 1.25)
	for _, want := range []string{"300-second", "basic monitoring", "Maximum statistic does not help",
		"inflated by 25%", "detailed monitoring"} {
		if !strings.Contains(coarse, want) {
			t.Errorf("coarse note missing %q:\n%s", want, coarse)
		}
	}
	fine := ResolutionNote(PeriodDetailedSeconds, 1)
	if strings.Contains(fine, "CANNOT") {
		t.Errorf("1-minute note should not carry the basic-monitoring warning:\n%s", fine)
	}
}

func maxOf(pts []Point) float64 {
	m := 0.0
	for _, p := range pts {
		if p.Value > m {
			m = p.Value
		}
	}
	return m
}
