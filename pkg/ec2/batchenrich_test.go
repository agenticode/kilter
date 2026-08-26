package ec2

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// U15b — the three AWS Batch insights. Each is a report-scope Advisory, and
// the whole point of the unit is that none of them is ever anything more.

// batchSeam is a two-compute-environment account:
//
//   - prod-batch-ce: MANAGED/EC2, minvCpus 16 against maxvCpus 256, no
//     allocationStrategy (so BEST_FIT by AWS's documented default), fed by one
//     enabled queue;
//   - spot-batch-ce: MANAGED/SPOT with bidPercentage 40 and minvCpus 0 — so it
//     produces the bid finding and NOT a floor finding;
//   - legacy-unmanaged: UNMANAGED, which kilter must ignore entirely because
//     AWS Batch provisions nothing for it.
func batchSeam() *BatchFixture {
	return &BatchFixture{
		Pages: []DescribeComputeEnvironmentsOutput{{ComputeEnvironments: []ComputeEnvironmentDetail{
			{
				ComputeEnvironmentName: "prod-batch-ce",
				ComputeEnvironmentARN:  "arn:aws:batch:us-east-1:1234:compute-environment/prod-batch-ce",
				Type:                   BatchManaged, State: BatchEnabled, Status: "VALID",
				ComputeResources: &ComputeResource{
					Type: BatchCETypeEC2, MinvCpus: 16, MaxvCpus: 256, DesiredvCpus: 16,
					InstanceTypes: []string{"m5", "c5"},
				},
			},
			{
				ComputeEnvironmentName: "spot-batch-ce",
				Type:                   BatchManaged, State: BatchEnabled, Status: "VALID",
				ComputeResources: &ComputeResource{
					Type: BatchCETypeSpot, AllocationStrategy: "SPOT_PRICE_CAPACITY_OPTIMIZED",
					MinvCpus: 0, MaxvCpus: 512, BidPercentage: 40,
					InstanceTypes: []string{"c5.xlarge"},
				},
			},
			{
				ComputeEnvironmentName: "legacy-unmanaged",
				Type:                   BatchUnmanaged, State: BatchEnabled, Status: "VALID",
			},
		}}},
		QueuePages: []DescribeJobQueuesOutput{{JobQueues: []JobQueueDetail{{
			JobQueueName: "nightly-etl", State: BatchEnabled, Priority: 1,
			ComputeEnvironmentOrder: []ComputeEnvironmentOrder{{Order: 1, ComputeEnvironment: "prod-batch-ce"}},
		}}}},
	}
}

// batchReport collects the U15a account with the Batch seam attached and sizes
// it, so every assertion below runs through the production collector and the
// production sizer — not through a hand-built Report.
func batchReport(t *testing.T, seam BatchAPI) *Report {
	t.Helper()
	f := mustFixture(t, "testdata/account-batch-managed.json")
	f.Metrics = idleFloorSeries("i-0batchfloor", "i-1eksnode", "i-2plain")
	cfg := CollectorConfig{
		Scope: "1234/us-east-1", Region: "us-east-1",
		Window: (batchFloorDays + 1) * 24 * time.Hour, CollectMemory: true,
	}
	if seam != nil {
		cfg.Batch = seam
	}
	return assess(t, mustCollect(t, f, cfg), nil)
}

func advisory(t *testing.T, rep *Report, code string) Advisory {
	t.Helper()
	var got []string
	for _, ad := range rep.Advisories {
		if ad.Code == code {
			return ad
		}
		got = append(got, ad.Code)
	}
	t.Fatalf("no %q advisory in the report; got %v", code, got)
	return Advisory{}
}

// §3.2: minvCpus is "the entire Batch cost finding" — a floor AWS maintains
// "even if the compute environment is DISABLED". It is priced, and it is
// priced as an insight: a dollar figure the operator can act on, attached to
// the one caveat kilter cannot resolve.
func TestMinVCpusIdleFloorIsPricedAsAnInsight(t *testing.T) {
	rep := batchReport(t, batchSeam())
	ad := advisory(t, rep, AdvisoryBatchMinVCpusFloor)

	// 16 vCPUs at the cheapest declared m5/c5 rate. c5.large is $0.085 for 2
	// vCPUs = $0.0425/vCPU-h, cheaper than m5's $0.048, so the floor is
	// 16 × 0.0425 × 730 = $496.40/mo.
	const wantMonthly = 16 * (0.085 / 2) * HoursPerMonth
	if math.Abs(ad.GrossSavingsMonthlyUSD-wantMonthly) > 1e-6 {
		t.Errorf("floor priced at %s/mo, want %s/mo", fmtUSD(ad.GrossSavingsMonthlyUSD), fmtUSD(wantMonthly))
	}
	if !strings.Contains(ad.Message, fmtUSD(wantMonthly)) {
		t.Errorf("the advisory must quote the monthly cost of the floor: %s", ad.Message)
	}
	for _, want := range []string{"prod-batch-ce", "16 vCPU", "minvCpus", "DISABLED", "c5.large", "nightly-etl"} {
		if !strings.Contains(ad.Message, want) {
			t.Errorf("message must name %q: %s", want, ad.Message)
		}
	}

	// The caveat is the finding. Removing the floor trades cost for job start
	// latency, and kilter measures no latency at all — so it must say so, and
	// it must not book the money.
	for _, want := range []string{"START LATENCY", "does not measure", "UpdateComputeEnvironment"} {
		if !strings.Contains(ad.Caveat, want) {
			t.Errorf("caveat must state %q: %s", want, ad.Caveat)
		}
	}
	if ad.NetSavingsMonthlyUSD != 0 {
		t.Errorf("an unmeasurable latency trade must not be claimed as net savings, got %s/mo",
			fmtUSD(ad.NetSavingsMonthlyUSD))
	}
	if rep.Totals.AdvisoryNetSavingsMonthlyUSD != 0 {
		t.Errorf("the floor must not reach the report's advisory savings total, got %s/mo",
			fmtUSD(rep.Totals.AdvisoryNetSavingsMonthlyUSD))
	}
	if ad.Actuatable() {
		t.Fatal("Advisory.Actuatable() must be false")
	}
	if ad.Action() != ActionAdvisory {
		t.Errorf("Action() = %q, want %q", ad.Action(), ActionAdvisory)
	}

	// The Spot compute environment has minvCpus 0: no floor, no finding. A
	// package that reported one would be inventing a cost.
	for _, other := range rep.Advisories {
		if other.Code == AdvisoryBatchMinVCpusFloor && strings.Contains(other.Message, "spot-batch-ce") {
			t.Error("a minvCpus of 0 must not produce a floor finding")
		}
	}
}

// Both are one-enum findings on a resource AWS "assumes full control of".
// Reported, never changed — and structurally unable to be changed, because the
// package has no actuator and an advisory can never claim to be actuatable.
func TestBestFitAndBidPercentageAreReportedNeverChanged(t *testing.T) {
	rep := batchReport(t, batchSeam())

	bestFit := advisory(t, rep, AdvisoryBatchBestFit)
	for _, want := range []string{"prod-batch-ce", "keeps costs lower but can limit scaling",
		"don't support infrastructure updates", "documented default"} {
		if !strings.Contains(bestFit.Message, want) {
			t.Errorf("BEST_FIT message must state %q: %s", want, bestFit.Message)
		}
	}
	// spot-batch-ce names SPOT_PRICE_CAPACITY_OPTIMIZED, which is what AWS
	// recommends: it must not be reported at all.
	if strings.Contains(bestFit.Message, "spot-batch-ce") {
		t.Error("a compute environment that already uses the recommended strategy must produce no finding")
	}

	bid := advisory(t, rep, AdvisoryBatchBidPercentage)
	for _, want := range []string{"spot-batch-ce", "40%", "we recommend leaving this field empty"} {
		if !strings.Contains(bid.Message, want) {
			t.Errorf("bidPercentage message must state %q: %s", want, bid.Message)
		}
	}
	if strings.Contains(bid.Message, "prod-batch-ce") {
		t.Error("bidPercentage does not apply to an on-demand EC2 compute environment")
	}

	for _, ad := range []Advisory{bestFit, bid} {
		if ad.Actuatable() {
			t.Fatalf("%s claims to be actuatable", ad.Code)
		}
		if ad.Action() != ActionAdvisory {
			t.Errorf("%s action = %q", ad.Code, ad.Action())
		}
		if ad.Caveat == "" {
			t.Errorf("%s has no caveat", ad.Code)
		}
		if ad.GrossSavingsMonthlyUSD != 0 || ad.NetSavingsMonthlyUSD != 0 {
			t.Errorf("%s must claim no money: gross %s net %s", ad.Code,
				fmtUSD(ad.GrossSavingsMonthlyUSD), fmtUSD(ad.NetSavingsMonthlyUSD))
		}
		if ad.ProposedType != "" {
			t.Errorf("%s proposes an instance type (%q); these findings are not about instances",
				ad.Code, ad.ProposedType)
		}
	}

	// An unmanaged compute environment is not AWS Batch's to provision, so
	// kilter has nothing to say about it.
	for _, ad := range rep.Advisories {
		if strings.Contains(ad.Message, "legacy-unmanaged") {
			t.Errorf("an UNMANAGED compute environment must produce no findings: %s", ad.Message)
		}
	}
	if len(rep.Advisories) != 3 {
		var codes []string
		for _, ad := range rep.Advisories {
			codes = append(codes, ad.Code)
		}
		t.Errorf("expected exactly the three U15b insights, got %v", codes)
	}
	if rep.Totals.Advisories < 3 {
		t.Errorf("report-scope advisories must be counted in the totals: %d", rep.Totals.Advisories)
	}
}

// The seam is enrichment. Every way it can be absent has to leave a working
// EC2 report behind, because an operator whose role lacks batch:Describe* must
// not lose their instance report over three insights they never asked for.
func TestBatchEnrichmentIsOptional(t *testing.T) {
	// The baseline every degraded case is compared against: no seam at all.
	// This is also the pre-U15b behaviour, so it pins that attaching nothing
	// changes nothing.
	plain := batchReport(t, nil)
	if len(plain.Advisories) != 0 {
		t.Errorf("a nil seam must produce no advisories: %+v", plain.Advisories)
	}
	if err := plain.Validate(); err != nil {
		t.Fatalf("a report with no Batch enrichment must still be valid: %v", err)
	}

	// NewCollector must accept a nil seam without complaint — the nil is the
	// documented default, not an omission to be caught.
	if _, err := NewCollector(&Fixture{}, &Fixture{}, CollectorConfig{Batch: nil}); err != nil {
		t.Fatalf("NewCollector must accept a nil Batch seam: %v", err)
	}

	for _, tc := range []struct {
		name string
		seam BatchAPI
		warn string
	}{
		{"describe compute environments fails", &BatchFixture{CEFailAt: 1}, "DescribeComputeEnvironments failed"},
		{"describe job queues fails", func() BatchAPI {
			f := batchSeam()
			f.QueueFailAt = 1
			return f
		}(), "DescribeJobQueues failed"},
		{"account runs no batch", &BatchFixture{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := batchReport(t, tc.seam)
			if err := rep.Validate(); err != nil {
				t.Fatalf("a degraded Batch seam must still leave a valid report: %v", err)
			}
			if len(rep.Assessments) != len(plain.Assessments) {
				t.Errorf("the Batch seam changed the instance report: %d assessments, want %d",
					len(rep.Assessments), len(plain.Assessments))
			}
			if rep.Stale {
				t.Error("a Batch failure must not mark the EC2 snapshot stale; no target is affected by it")
			}
			for _, id := range []string{"i-0batchfloor", "i-1eksnode", "i-2plain"} {
				a, ok := rep.For(id)
				b, _ := plain.For(id)
				if !ok || (a.Proposal == nil) != (b.Proposal == nil) {
					t.Errorf("%s: the Batch seam changed a sizing decision", id)
				}
			}
			if tc.warn != "" {
				if !hasWarning(rep.Warnings, tc.warn) {
					t.Errorf("a degraded seam must say so: %v", rep.Warnings)
				}
			}
			// The report still renders. A UI reading a degraded report is the
			// case that would otherwise ship a panic.
			var buf bytes.Buffer
			if err := rep.WriteText(&buf); err != nil {
				t.Fatalf("WriteText: %v", err)
			}
		})
	}

	// A job-queue failure loses the queue names and keeps the floor: the
	// finding degrades in detail, not in existence.
	partial := batchSeam()
	partial.QueueFailAt = 1
	ad := advisory(t, batchReport(t, partial), AdvisoryBatchMinVCpusFloor)
	if !strings.Contains(ad.Message, "prod-batch-ce") {
		t.Errorf("the floor finding must survive a job-queue failure: %s", ad.Message)
	}
	if strings.Contains(ad.Message, "nightly-etl") {
		t.Error("queue names cannot be reported when the queue call failed")
	}

	// A sizer handed a snapshot with no Batch inventory at all — the shape a
	// hand-built or replayed-from-JSON snapshot has.
	s, err := NewSizer(testCatalog(t), DefaultConfig())
	if err != nil {
		t.Fatalf("sizer: %v", err)
	}
	if got := s.batchInsights(nil); got != nil {
		t.Errorf("a nil snapshot must yield no insights, got %+v", got)
	}
	if got := s.batchInsights(&Snapshot{Domain: Domain}); got != nil {
		t.Errorf("a snapshot with no Batch inventory must yield no insights, got %+v", got)
	}
}

func hasWarning(warns []string, want string) bool {
	for _, w := range warns {
		if strings.Contains(w, want) {
			return true
		}
	}
	return false
}

// A floor kilter cannot price is still a floor. Reporting it unpriced beats
// inventing a rate for AWS's `optimal` bundle, whose families vary by region
// and change over time.
func TestUnpriceableFloorIsReportedWithoutANumber(t *testing.T) {
	seam := &BatchFixture{Pages: []DescribeComputeEnvironmentsOutput{{
		ComputeEnvironments: []ComputeEnvironmentDetail{{
			ComputeEnvironmentName: "optimal-ce", Type: BatchManaged, State: BatchDisabled, Status: "VALID",
			ComputeResources: &ComputeResource{
				Type: BatchCETypeEC2, MinvCpus: 8, MaxvCpus: 64, InstanceTypes: []string{"optimal"},
			},
		}},
	}}}
	rep := batchReport(t, seam)
	ad := advisory(t, rep, AdvisoryBatchMinVCpusFloor)
	if ad.GrossSavingsMonthlyUSD != 0 {
		t.Errorf("an unpriceable floor must not carry a price: %s", fmtUSD(ad.GrossSavingsMonthlyUSD))
	}
	for _, want := range []string{"could not be priced", "optimal", "No job queue is attached",
		"DISABLED and the floor is being held anyway"} {
		if !strings.Contains(ad.Message, want) {
			t.Errorf("message must state %q: %s", want, ad.Message)
		}
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// The report is a stored artifact: two collectors that saw the same account in
// a different order must produce the same bytes, Batch included.
func TestBatchInsightsAreOrderIndependent(t *testing.T) {
	forward := batchSeam()
	reversed := batchSeam()
	ces := reversed.Pages[0].ComputeEnvironments
	for i, j := 0, len(ces)-1; i < j; i, j = i+1, j-1 {
		ces[i], ces[j] = ces[j], ces[i]
	}

	var a, b bytes.Buffer
	if err := batchReport(t, forward).WriteText(&a); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := batchReport(t, reversed).WriteText(&b); err != nil {
		t.Fatalf("write: %v", err)
	}
	if a.String() != b.String() {
		t.Error("shuffling the compute environments changed the report")
	}
	if !strings.Contains(a.String(), "ADVISE ["+AdvisoryBatchMinVCpusFloor+"]") {
		t.Errorf("report-scope advisories must be rendered:\n%s", a.String())
	}
}

// Validate is the package's contract check and the fuzz target leans on it
// entirely. Report-scope advisories have to be inside it, not beside it.
func TestValidateCatchesReportAdvisoryViolations(t *testing.T) {
	for _, tc := range []struct {
		name string
		ad   Advisory
		want string
	}{
		{"no caveat", Advisory{Code: AdvisoryBatchBestFit, Message: "m"}, "has no caveat"},
		{"no code", Advisory{Message: "m", Caveat: "c"}, "no code"},
	} {
		rep := batchReport(t, batchSeam())
		rep.Advisories = append(rep.Advisories, tc.ad)
		rep.Totals = rep.computeTotals()
		err := rep.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}
