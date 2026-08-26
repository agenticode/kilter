package ec2

import (
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// Report.Validate is the package's own contract check, and the fuzz target
// leans on it entirely. A check that passes everything proves nothing, so this
// hand-corrupts a valid report one invariant at a time and requires each
// corruption to be caught.
func TestValidateCatchesEachViolation(t *testing.T) {
	base := func() *Report {
		a := Assessment{
			Target:            TargetRef{Domain: Domain, Scope: "acct/us-east-1", ID: "i-1"},
			Current:           Spec{Resources: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30}},
			CurrentHourlyUSD:  0.192,
			CurrentMonthlyUSD: 0.192 * HoursPerMonth,
			Evidence:          []Evidence{{Metric: "cpu-p95", Value: "10.0%", Source: "cloudwatch"}},
			Observation: Observation{
				PeakCPUMilli: 400, DemandCPUMilli: 600,
				PeakMemoryBytes: 4 << 30, MemoryFloorBytes: 5 << 30,
			},
			Confidence: Confidence{Score: 0.9},
			Proposal: &Proposal{
				Spec:                   Spec{Resources: model.Resources{MilliCPU: 2000, MemoryBytes: 8 << 30}},
				InstanceType:           "m5.large",
				ProposedHourlyUSD:      0.096,
				Action:                 ActionStopStart,
				Risk:                   RiskMedium,
				Confidence:             0.9,
				GrossSavingsMonthlyUSD: 70,
				NetSavingsMonthlyUSD:   70,
				Reason:                 "test",
			},
		}
		r := &Report{Domain: Domain, Assessments: []Assessment{a}}
		r.Totals = r.computeTotals()
		return r
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the baseline report must be valid: %v", err)
	}

	cases := []struct {
		name    string
		corrupt func(*Report)
		want    string
	}{
		{"cpu below observed peak", func(r *Report) {
			r.Assessments[0].Observation.PeakCPUMilli = 3000
		}, "below the observed peak"},
		{"cpu below computed demand", func(r *Report) {
			r.Assessments[0].Observation.DemandCPUMilli = 2500
		}, "below computed demand"},
		{"memory below observed peak", func(r *Report) {
			r.Assessments[0].Observation.PeakMemoryBytes = 12 << 30
		}, "below the observed peak"},
		{"memory cut while memory-blind", func(r *Report) {
			r.Assessments[0].Observation.MemoryBlind = true
			r.Assessments[0].Observation.PeakMemoryBytes = 0
		}, "memory-blind but the proposal cuts memory"},
		{"non-positive claim", func(r *Report) {
			r.Assessments[0].Proposal.NetSavingsMonthlyUSD = 0
			r.Totals = r.computeTotals()
		}, "must be net-positive"},
		{"net above gross", func(r *Report) {
			r.Assessments[0].Proposal.GrossSavingsMonthlyUSD = 10
		}, "above gross"},
		{"proposal on a throttled instance", func(r *Report) {
			r.Assessments[0].Observation.Burst.Class = BurstThrottled
		}, "credit-throttled but carries a proposal"},
		{"assessment with no evidence", func(r *Report) {
			r.Assessments[0].Evidence = nil
		}, "no evidence"},
		{"refusal with no reason", func(r *Report) {
			r.Assessments[0].Proposal = nil
			r.Totals = r.computeTotals()
		}, "silence is not an output"},
		{"advisory with no caveat", func(r *Report) {
			r.Assessments[0].Advisories = []Advisory{{Code: AdvisoryGraviton, Message: "m7g", Caveat: ""}}
			r.Totals = r.computeTotals()
		}, "no caveat"},
		{"excluded instance with a proposal", func(r *Report) {
			r.Assessments[0].Suppressions = []Suppression{{Code: ReasonK8sTagged, Reason: "node"}}
		}, "excluded from this domain but carries a proposal"},
		{"excluded instance with advisories", func(r *Report) {
			r.Assessments[0].Proposal = nil
			r.Assessments[0].Suppressions = []Suppression{{Code: ReasonModeOff, Reason: "off"}}
			r.Assessments[0].Advisories = []Advisory{{Code: AdvisoryGraviton, Caveat: "advisory only"}}
			r.Totals = r.computeTotals()
		}, "excluded from this domain but carries advisories"},
		{"unsorted assessments", func(r *Report) {
			extra := r.Assessments[0]
			extra.Target.ID = "i-0"
			r.Assessments = append(r.Assessments, extra)
			r.Totals = r.computeTotals()
		}, "not sorted"},
		{"totals that lie", func(r *Report) {
			r.Totals.NetSavingsMonthlyUSD = 9999
		}, "totals do not match"},
		{"wrong domain", func(r *Report) { r.Domain = "lambda" }, "report domain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.corrupt(r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("corruption %q was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// An advisory can never be marshalled or constructed into an actuatable
// state — the property is a method, not a field.
func TestAdvisoryIsStructurallyNonActuatable(t *testing.T) {
	if (Advisory{}).Actuatable() {
		t.Fatal("advisories must never be actuatable")
	}
	if (Advisory{}).Action() != ActionAdvisory {
		t.Fatal("an advisory's action class must be advisory")
	}
}

// Series statistics are the arithmetic every decision rests on.
func TestSeriesStatistics(t *testing.T) {
	s := Series{Points: []Point{
		{At: testNow.Add(-4 * time.Minute), Value: 10},
		{At: testNow.Add(-3 * time.Minute), Value: 30},
		{At: testNow.Add(-2 * time.Minute), Value: 20},
		{At: testNow.Add(-time.Minute), Value: 40},
		{At: testNow, Value: 50},
	}}
	if m, _ := s.Max(); m != 50 {
		t.Errorf("max = %v", m)
	}
	if m, _ := s.Min(); m != 10 {
		t.Errorf("min = %v", m)
	}
	if m, _ := s.Mean(); m != 30 {
		t.Errorf("mean = %v", m)
	}
	if s.Sum() != 150 {
		t.Errorf("sum = %v", s.Sum())
	}
	if last, _ := s.Last(); last.Value != 50 {
		t.Errorf("last = %v", last)
	}
	// Nearest rank, not interpolation: every percentile is a value that was
	// actually observed.
	for _, tc := range []struct{ p, want float64 }{{0, 10}, {0.5, 30}, {0.95, 50}, {1, 50}} {
		if got, _ := s.Percentile(tc.p); got != tc.want {
			t.Errorf("p%.2f = %v, want %v", tc.p, got, tc.want)
		}
	}
	empty := Series{}
	if _, ok := empty.Max(); ok {
		t.Error("an empty series must report absence, not zero")
	}
	if _, ok := empty.Percentile(0.95); ok {
		t.Error("an empty series has no percentile")
	}
}

func TestInstanceOwnershipHelpers(t *testing.T) {
	if _, ok := (Instance{Tags: map[string]string{"kubernetes.io/cluster/x": "owned"}}).K8sCluster(); !ok {
		t.Error("cluster tag not detected")
	}
	if c, ok := (Instance{Tags: map[string]string{TagEKSCluster: "prod"}}).K8sCluster(); !ok || c != "prod" {
		t.Errorf("eks tag: %q %v", c, ok)
	}
	if _, ok := (Instance{Tags: map[string]string{"Name": "web"}}).K8sCluster(); ok {
		t.Error("an ordinary tag must not read as a cluster tag")
	}
	if !(Instance{Tags: map[string]string{TagKilterMode: " OFF "}}).ModeOff() {
		t.Error("the mode guardrail must tolerate case and whitespace")
	}
	if (Instance{Tags: map[string]string{TagKilterMode: "apply"}}).ModeOff() {
		t.Error("mode=apply is not mode=off")
	}
	if got := (Instance{AvailabilityZone: "us-east-1a"}).Region(); got != "us-east-1" {
		t.Errorf("region = %q", got)
	}
	if got := (Instance{AvailabilityZone: "us-east-1"}).Region(); got != "us-east-1" {
		t.Errorf("region = %q", got)
	}
	if got := (Instance{ID: "i-1"}).Name(); got != "i-1" {
		t.Errorf("name = %q", got)
	}
}
