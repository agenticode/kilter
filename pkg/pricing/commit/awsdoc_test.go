package commit

import (
	"testing"
	"time"
)

// near compares money. Money is float64 here (pkg/pricing's convention) and is
// never compared with ==; every assertion states its tolerance. Figures the
// AWS docs print rounded to the cent are asserted at ±$0.005, exact arithmetic
// at ±1e-9.
func near(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if d := got - want; d > tol || d < -tol {
		t.Errorf("%s = %.10f, want %.10f (tolerance %g)", what, got, want, tol)
	}
}

func covOf(t *testing.T, c Cost, id string) LineCoverage {
	t.Helper()
	for _, cv := range c.Coverage {
		if cv.LineID == id {
			return cv
		}
	}
	t.Fatalf("no coverage for line %q", id)
	return LineCoverage{}
}

func useOf(t *testing.T, c Cost, id string) CommitmentUse {
	t.Helper()
	for _, u := range c.Commitments {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("no commitment use for %q", id)
	return CommitmentUse{}
}

// assertPartition pins the Cost invariants for every bill in the suite.
func assertPartition(t *testing.T, c Cost) {
	t.Helper()
	near(t, c.HourlyUSD, c.RICommittedUSD+c.SPCommittedUSD+c.OnDemandUSD, 1e-9, "HourlyUSD partition")
	if floor := c.RICommittedUSD + c.SPCommittedUSD; c.HourlyUSD < floor-1e-9 {
		t.Errorf("bill %.10f below committed floor %.10f", c.HourlyUSD, floor)
	}
	for _, cv := range c.Coverage {
		sum := cv.RIQty + cv.EC2SPQty + cv.ComputeSPQty + cv.OnDemandQty
		near(t, sum, cv.Quantity, 1e-9, "coverage partition for "+cv.LineID)
	}
	for _, u := range c.Commitments {
		if u.UsedUSD > u.CommittedUSD+1e-9 {
			t.Errorf("commitment %q used %.10f > committed %.10f", u.ID, u.UsedUSD, u.CommittedUSD)
		}
	}
}

// ---------------------------------------------------------------------------
// apply_ri.html — "Examples of applying Reserved Instances"
// ---------------------------------------------------------------------------

func TestAWSDocApplyRIScenarios(t *testing.T) {
	type want struct{ ri, od float64 }
	tests := []struct {
		name    string
		usage   Usage
		ris     []ReservedInstance
		want    map[string]want
		wantOD  float64 // on-demand dollars left after the reservations apply
		comment string
	}{
		{
			// Scenario 1: Reserved Instances in a single account.
			name: "scenario-1/single-account",
			usage: Usage{Lines: []UsageLine{
				{ID: "m3.large/1a", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
					InstanceType: "m3.large", Platform: "Linux", Quantity: 4, ODRate: 0.133},
				{ID: "m4.xlarge/1b", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1b",
					InstanceType: "m4.xlarge", Platform: "Amazon Linux", Quantity: 2, ODRate: 0.20},
				{ID: "c4.xlarge/1c", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1c",
					InstanceType: "c4.xlarge", Platform: "Amazon Linux", Quantity: 1, ODRate: 0.199},
			}},
			ris: []ReservedInstance{
				{ID: "zonal-m3", Count: 4, InstanceType: "m3.large", Region: "us-east-1",
					AZ: "us-east-1a", Platform: "Linux", EffectiveHourlyUSD: 0.077},
				{ID: "regional-m4", Count: 4, InstanceType: "m4.large", Region: "us-east-1",
					Platform: "Amazon Linux", EffectiveHourlyUSD: 0.063},
				{ID: "regional-c4", Count: 1, InstanceType: "c4.large", Region: "us-east-1",
					Platform: "Amazon Linux", EffectiveHourlyUSD: 0.062},
			},
			// "the four m3.large zonal RIs is used by the four m3.large instances";
			// "the four m4.large regional RIs provide the full billing benefit to
			// the usage of the two m4.xlarge instances" (16 units == 16 units);
			// "the c4.large RI billing discount applies to 50% of c4.xlarge usage.
			// The remaining c4.xlarge usage is charged at the On-Demand rate."
			want: map[string]want{
				"m3.large/1a":  {ri: 4, od: 0},
				"m4.xlarge/1b": {ri: 2, od: 0},
				"c4.xlarge/1c": {ri: 0.5, od: 0.5},
			},
			wantOD: 0.5 * 0.199,
		},
		{
			// Scenario 2: single account using the normalization factor.
			name: "scenario-2/normalization-factor",
			usage: Usage{Lines: []UsageLine{
				{ID: "m3.xlarge/1a", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
					InstanceType: "m3.xlarge", Platform: "Amazon Linux", Quantity: 2, ODRate: 0.266},
				{ID: "m3.large/1b", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1b",
					InstanceType: "m3.large", Platform: "Amazon Linux", Quantity: 2, ODRate: 0.133},
			}},
			ris: []ReservedInstance{
				{ID: "regional-m3-2xl", Count: 1, InstanceType: "m3.2xlarge", Region: "us-east-1",
					Platform: "Amazon Linux", EffectiveHourlyUSD: 0.30},
			},
			// "It applies first to the m3.large instances and then to the
			// m3.xlarge instances… provides full benefit to 2 x m3.large usage
			// (8 units). This leaves 8 units… full benefit to 1 x m3.xlarge
			// usage. The remaining m3.xlarge usage is charged at On-Demand."
			want: map[string]want{
				"m3.large/1b":  {ri: 2, od: 0},
				"m3.xlarge/1a": {ri: 1, od: 1},
			},
			wantOD: 1 * 0.266,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := &Inventory{RIs: tc.ris}
			if err := inv.Validate(); err != nil {
				t.Fatalf("inventory: %v", err)
			}
			if err := tc.usage.Validate(); err != nil {
				t.Fatalf("usage: %v", err)
			}
			c := inv.Bill(tc.usage)
			assertPartition(t, c)
			for id, w := range tc.want {
				cv := covOf(t, c, id)
				near(t, cv.RIQty, w.ri, 1e-9, id+" reserved-covered quantity")
				near(t, cv.OnDemandQty, w.od, 1e-9, id+" on-demand quantity")
			}
			near(t, c.OnDemandUSD, tc.wantOD, 1e-9, "on-demand charges")
		})
	}
}

// ---------------------------------------------------------------------------
// sp-applying.html — "Understanding how Savings Plans apply to your usage"
// ---------------------------------------------------------------------------

// spDocUsage is the page's single-hour usage example, with its illustrative
// rate table. Lambda request charges are modelled here exactly as that table
// lists them — Savings-Plan-eligible at a 0 % savings rate — so the documented
// $47.13 and $59.10 reproduce. Kilter's own design doc (§4.4) records that
// request charges are in reality SP-ineligible; that path is exercised by
// TestSPIneligibleUsageNeverConsumesCommitment, and both models produce the
// same totals here because a 0 %-savings rate equals the on-demand rate.
func spDocUsage() Usage {
	return Usage{Lines: []UsageLine{
		{ID: "r5.4xlarge", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
			InstanceType: "r5.4xlarge", Platform: "Linux/UNIX", Tenancy: "default",
			Quantity: 4, ODRate: 1.00, ComputeSPRate: 0.70, EC2SPRate: 0.60},
		{ID: "m5.24xlarge", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
			InstanceType: "m5.24xlarge", Platform: "Windows", Tenancy: "dedicated",
			Quantity: 1, ODRate: 10.00, ComputeSPRate: 8.20, EC2SPRate: 7.80},
		{ID: "fargate-gb", Kind: KindFargate, Region: "us-west-1", Unit: "GB-Hours",
			Quantity: 1600, ODRate: 0.004, ComputeSPRate: 0.003},
		{ID: "fargate-vcpu", Kind: KindFargate, Region: "us-west-1", Unit: "vCPU-Hours",
			Quantity: 400, ODRate: 0.04, ComputeSPRate: 0.03},
		{ID: "lambda-duration", Kind: KindLambda, Region: "us-east-2", Unit: "GB-Seconds",
			Quantity: 1_500_000, ODRate: 0.000015, ComputeSPRate: 0.00001275},
		{ID: "lambda-requests", Kind: KindLambda, Region: "us-east-2", Unit: "Requests-Millions",
			Quantity: 1, ODRate: 0.20, ComputeSPRate: 0.20},
	}}
}

// TestAWSDocSavingsPlansListPrice pins the page's stated on-demand total.
func TestAWSDocSavingsPlansListPrice(t *testing.T) {
	near(t, spDocUsage().OnDemandHourlyUSD(), 59.10, 5e-3,
		"documented On-Demand charge for the example hour")
}

func TestAWSDocSavingsPlansScenarios(t *testing.T) {
	tests := []struct {
		name string
		inv  *Inventory
		// Documented figures. wantSPConsumed is the commitment the hour
		// actually absorbs; wantOD is the on-demand remainder.
		wantSPConsumed float64
		wantOD         float64
		wantBill       float64
		tol            float64
		check          func(t *testing.T, c Cost)
	}{
		{
			// Scenario 1: "$50.00/hour commitment… multiplying each of your
			// usages by the equivalent Compute Savings Plans is $47.13."
			name: "scenario-1/all-usage-covered",
			inv: &Inventory{SavingsPlans: []SavingsPlan{
				{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 50.00},
			}},
			// $47.125 exactly; the page prints its cent-rounded form, $47.13.
			wantSPConsumed: 47.125,
			wantOD:         0,
			wantBill:       50.00,
			tol:            5e-3,
			check: func(t *testing.T, c Cost) {
				// Use-it-or-lose-it: the unabsorbed commitment is stranded,
				// not carried into the next hour.
				near(t, c.StrandedUSD, 50.00-47.125, 5e-3, "stranded commitment")
			},
		},
		{
			// Scenario 2: "$2.00/hour commitment… covers approximately 2.9
			// units of this usage. The remaining 1.1 units are charged at
			// On-Demand rates, resulting in $1.14 of On-Demand charges for r5.
			// The Fargate, m5.24xlarge and Lambda usage… $55.10. The total
			// On-Demand charges for this usage are $56.24."
			name: "scenario-2/some-usage-covered",
			inv: &Inventory{SavingsPlans: []SavingsPlan{
				{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 2.00},
			}},
			wantSPConsumed: 2.00,
			wantOD:         56.24,
			wantBill:       58.24,
			tol:            5e-3,
			check: func(t *testing.T, c Cost) {
				r5 := covOf(t, c, "r5.4xlarge")
				near(t, r5.ComputeSPQty, 2.9, 5e-2, "r5 units covered (doc: ~2.9)")
				near(t, r5.OnDemandQty, 1.1, 5e-2, "r5 units on-demand (doc: ~1.1)")
				near(t, r5.OnDemandUSD, 1.14, 5e-3, "r5 on-demand charge")
				rest := c.OnDemandUSD - r5.OnDemandUSD
				near(t, rest, 55.10, 5e-3, "Fargate + m5.24xlarge + Lambda on-demand")
			},
		},
		{
			// Scenario 3: "$19.60/hour commitment… applied to r5.4xlarge first
			// (30%)… then Fargate (25%), memory before compute because it has
			// the lower Savings Plans rate. The hourly commitment of $19.60 is
			// met… m5.24xlarge and Lambda On-Demand charges are $32.70."
			name: "scenario-3/across-products",
			inv: &Inventory{SavingsPlans: []SavingsPlan{
				{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 19.60},
			}},
			wantSPConsumed: 19.60,
			wantOD:         32.70,
			wantBill:       52.30,
			tol:            5e-3,
			check: func(t *testing.T, c Cost) {
				near(t, covOf(t, c, "r5.4xlarge").ComputeSPQty, 4, 1e-9, "all r5 covered")
				near(t, covOf(t, c, "fargate-gb").ComputeSPQty, 1600, 1e-9, "all Fargate GB covered")
				near(t, covOf(t, c, "fargate-vcpu").ComputeSPQty, 400, 1e-9, "all Fargate vCPU covered")
				near(t, covOf(t, c, "m5.24xlarge").OnDemandQty, 1, 1e-9, "m5.24xlarge on-demand")
				near(t, c.StrandedUSD, 0, 1e-9, "commitment exactly met")
			},
		},
		{
			// Scenario 4: "$18.20/hour commitment… two EC2 RIs for r5.4xlarge
			// Linux shared tenancy in us-east-1. First, the RI covers two of
			// the r5.4xlarge instances. Then the Savings Plans rate is applied
			// to the remaining r5.4xlarge and the Fargate usage, which exhausts
			// the hourly commitment of $18.20… $32.70."
			// The RI's own effective rate is not stated on the page; $0.60/h is
			// this fixture's choice and only moves the RI term of the bill.
			name: "scenario-4/savings-plan-after-reserved-instances",
			inv: &Inventory{
				RIs: []ReservedInstance{{ID: "ri-r5", Count: 2, InstanceType: "r5.4xlarge",
					Region: "us-east-1", Platform: "Linux/UNIX", Tenancy: "default",
					EffectiveHourlyUSD: 0.60}},
				SavingsPlans: []SavingsPlan{
					{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 18.20},
				},
			},
			wantSPConsumed: 18.20,
			wantOD:         32.70,
			wantBill:       2*0.60 + 18.20 + 32.70,
			tol:            5e-3,
			check: func(t *testing.T, c Cost) {
				r5 := covOf(t, c, "r5.4xlarge")
				near(t, r5.RIQty, 2, 1e-9, "RI covers two r5.4xlarge first")
				near(t, r5.ComputeSPQty, 2, 1e-9, "SP covers the remaining two")
				near(t, r5.OnDemandQty, 0, 1e-9, "no r5 left on-demand")
			},
		},
		{
			// Scenario 5: "EC2 Instance Family Savings Plan for the r5 family
			// in us-east-1 with a $3.00/hour commitment… covers all of the
			// r5.4xlarge usage because multiplying the usage by the EC2
			// Instance Family Savings Plan rate is $2.40… Next, the Compute
			// Savings Plan is applied to the Fargate usage… The hourly
			// commitment of $16.80 is met… $32.70."
			name: "scenario-5/multiple-savings-plans",
			inv: &Inventory{SavingsPlans: []SavingsPlan{
				{ID: "sp-ec2-r5", Type: SPEC2Instance, CommitmentUSDPerHour: 3.00,
					Region: "us-east-1", Family: "r5"},
				{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 16.80},
			}},
			wantSPConsumed: 2.40 + 16.80,
			wantOD:         32.70,
			wantBill:       3.00 + 16.80 + 32.70,
			tol:            5e-3,
			check: func(t *testing.T, c Cost) {
				r5 := covOf(t, c, "r5.4xlarge")
				near(t, r5.EC2SPQty, 4, 1e-9, "EC2 Instance SP covers all r5 usage")
				near(t, r5.ComputeSPQty, 0, 1e-9, "Compute SP does not touch r5")
				near(t, useOf(t, c, "sp-ec2-r5").UsedUSD, 2.40, 5e-3, "EC2 Instance SP consumption")
				near(t, useOf(t, c, "sp-compute").UsedUSD, 16.80, 5e-3, "Compute SP consumption")
				// The EC2 Instance SP is applied first even though it is the
				// narrower plan and leaves $0.60 stranded.
				near(t, useOf(t, c, "sp-ec2-r5").StrandedUSD(), 0.60, 5e-3, "EC2 Instance SP stranded")
				near(t, covOf(t, c, "fargate-gb").ComputeSPQty, 1600, 1e-9, "Fargate GB before vCPU")
				near(t, covOf(t, c, "fargate-vcpu").ComputeSPQty, 400, 1e-9, "Fargate vCPU next")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.inv.Validate(); err != nil {
				t.Fatalf("inventory: %v", err)
			}
			u := spDocUsage()
			if err := u.Validate(); err != nil {
				t.Fatalf("usage: %v", err)
			}
			c := tc.inv.Bill(u)
			assertPartition(t, c)
			if c.Fallback {
				t.Error("documented scenarios have full rate data; fallback must not trigger")
			}
			near(t, c.SPConsumedUSD, tc.wantSPConsumed, tc.tol, "savings-plan commitment consumed")
			near(t, c.OnDemandUSD, tc.wantOD, tc.tol, "on-demand charges")
			near(t, c.HourlyUSD, tc.wantBill, tc.tol, "total hourly bill")
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

// TestSPIneligibleUsageNeverConsumesCommitment covers the §4.4 rule that
// Lambda request charges and Fargate ephemeral storage cannot be absorbed by a
// Savings Plan, whatever rate happens to be attached to the line.
func TestSPIneligibleUsageNeverConsumesCommitment(t *testing.T) {
	inv := &Inventory{SavingsPlans: []SavingsPlan{
		{ID: "sp", Type: SPCompute, CommitmentUSDPerHour: 100},
	}}
	u := Usage{Lines: []UsageLine{
		{ID: "requests", Kind: KindLambda, Region: "us-east-2", Unit: "Requests-Millions",
			Quantity: 3, ODRate: 0.20, ComputeSPRate: 0.10, SPIneligible: true},
	}}
	c := inv.Bill(u)
	assertPartition(t, c)
	near(t, covOf(t, c, "requests").OnDemandQty, 3, 1e-9, "ineligible usage stays on-demand")
	near(t, c.SPConsumedUSD, 0, 1e-9, "ineligible usage consumes no commitment")
	near(t, c.HourlyUSD, 100+0.60, 1e-9, "bill is commitment plus on-demand")
	if c.Fallback {
		t.Error("ineligible usage must not be reported as a rate fallback")
	}
}

// TestEC2InstanceSPScopeIsRespected pins the two scope rules that make an EC2
// Instance Savings Plan narrower than a Compute plan: one family, one region,
// and never Fargate or Lambda.
func TestEC2InstanceSPScopeIsRespected(t *testing.T) {
	inv := &Inventory{SavingsPlans: []SavingsPlan{
		{ID: "sp-ec2", Type: SPEC2Instance, CommitmentUSDPerHour: 100, Region: "us-east-1", Family: "m5"},
	}}
	u := Usage{Lines: []UsageLine{
		{ID: "in-scope", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.large",
			Quantity: 1, ODRate: 0.096, EC2SPRate: 0.05, ComputeSPRate: 0.06},
		{ID: "wrong-family", Kind: KindEC2, Region: "us-east-1", InstanceType: "c5.large",
			Quantity: 1, ODRate: 0.085, EC2SPRate: 0.05, ComputeSPRate: 0.06},
		{ID: "wrong-region", Kind: KindEC2, Region: "eu-west-1", InstanceType: "m5.large",
			Quantity: 1, ODRate: 0.107, EC2SPRate: 0.05, ComputeSPRate: 0.06},
		{ID: "fargate", Kind: KindFargate, Region: "us-east-1", Unit: "vCPU-Hours",
			Quantity: 10, ODRate: 0.04, ComputeSPRate: 0.03},
	}}
	c := inv.Bill(u)
	assertPartition(t, c)
	near(t, covOf(t, c, "in-scope").EC2SPQty, 1, 1e-9, "in-scope usage covered")
	for _, id := range []string{"wrong-family", "wrong-region", "fargate"} {
		near(t, covOf(t, c, id).EC2SPQty, 0, 1e-9, id+" must not be covered by an EC2 Instance SP")
		if covOf(t, c, id).Fallback {
			t.Errorf("%s: out-of-scope line must not take the rate fallback", id)
		}
	}
	near(t, c.SPConsumedUSD, 0.05, 1e-9, "only the in-scope line consumes commitment")
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad time %q: %v", s, err)
	}
	return ts
}
