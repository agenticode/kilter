package commit

import (
	"strings"
	"testing"
	"time"
)

// The worked examples from docs/design/compute-domains.md §4.4, asserted end
// to end. Each one shows the same failure: a naive list-price subtraction
// reports a saving, the bill delta reports a loss or a wash, and the
// recommendation is suppressed with a reason and a date.

func ec2Line(id, instanceType string, qty, od float64) UsageLine {
	return UsageLine{ID: id, Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
		InstanceType: instanceType, Platform: "Linux/UNIX", Tenancy: "default",
		Quantity: qty, ODRate: od}
}

// TestStrandingExample1FamilyMigrationOffAnRI is the +135 % case: one
// m5.xlarge fully covered by a 1-yr no-upfront standard regional RI, and a
// "migrate to Graviton and save 15 %" recommendation with no other m5 usage to
// absorb the freed reservation.
func TestStrandingExample1FamilyMigrationOffAnRI(t *testing.T) {
	expiry := mustTime(t, "2027-06-01T00:00:00Z")
	inv := &Inventory{RIs: []ReservedInstance{{
		ID: "ri-m5-xlarge", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
		Platform: "Linux/UNIX", Tenancy: "default", OfferingClass: "standard",
		EffectiveHourlyUSD: 0.121, Expires: expiry,
	}}}
	before := Usage{Lines: []UsageLine{ec2Line("i-1", "m5.xlarge", 1, 0.192)}}
	after := Usage{Lines: []UsageLine{ec2Line("i-1", "m7g.xlarge", 1, 0.1632)}}

	// What a list-price optimizer sees: $0.192 → $0.1632, "save 15 %".
	gross := before.OnDemandHourlyUSD() - after.OnDemandHourlyUSD()
	near(t, gross, 0.0288, 1e-9, "naive list-price saving")
	if gross <= 0 {
		t.Fatal("precondition: the naive calculation must look like a saving")
	}
	near(t, gross/before.OnDemandHourlyUSD(), 0.15, 5e-3, "naive saving as a fraction")

	// What the invoice does.
	b, a := inv.Bill(before), inv.Bill(after)
	assertPartition(t, b)
	assertPartition(t, a)
	near(t, b.HourlyUSD, 0.121, 1e-9, "bill before (RI rate, fully absorbed)")
	near(t, a.HourlyUSD, 0.2842, 1e-9, "bill after (m7g on-demand + stranded RI)")

	as := inv.NetSavings(before, after)
	near(t, as.NetHourlyUSD, -0.1632, 1e-9, "net savings (signed)")
	if as.NetHourlyUSD >= 0 {
		t.Fatal("net must be a loss")
	}
	// "$0.2842/h vs $0.121/h before — a 135 % increase".
	increase := a.HourlyUSD/b.HourlyUSD - 1
	near(t, increase, 1.35, 1.5e-2, "bill increase (design doc: +135 %)")

	if !as.Suppressed || as.ReasonCode != ReasonCommitmentNegative {
		t.Fatalf("recommendation must be suppressed as commitment-negative, got %+v", as)
	}
	if as.ClaimableHourlyUSD() != 0 {
		t.Errorf("a suppressed recommendation must claim nothing, got %v", as.ClaimableHourlyUSD())
	}
	if !as.ValidFrom.Equal(expiry) {
		t.Errorf("ValidFrom = %v, want the RI expiry %v", as.ValidFrom, expiry)
	}
	near(t, as.StrandedHourlyUSD, 0.121, 1e-9, "newly stranded commitment")
	for _, want := range []string{"commitment stranding", "2027-06-01"} {
		if !strings.Contains(as.Reason, want) {
			t.Errorf("reason %q must mention %q", as.Reason, want)
		}
	}

	// The suppression lapses on its own: bill against the inventory as it
	// stands after expiry and the same recommendation is net-positive, with
	// net equal to gross because no commitment is left to strand.
	lapsed := inv.Active(expiry.Add(time.Hour)).NetSavings(before, after)
	if lapsed.Suppressed {
		t.Fatalf("suppression must lapse once the RI expires: %+v", lapsed)
	}
	near(t, lapsed.NetHourlyUSD, gross, 1e-9, "post-expiry net equals gross")
	near(t, lapsed.ClaimableMonthlyUSD(), gross*HoursPerMonth, 1e-9, "post-expiry monthly claim")
}

// TestStrandingExample2DownsizeInsideAnRIFamily: m5.xlarge (8 units) under a
// regional m5.xlarge RI, rightsized to m5.large (4 units). "Claimed 50 %,
// realized 0 % — unless other m5 usage absorbs the 4 freed units."
func TestStrandingExample2DownsizeInsideAnRIFamily(t *testing.T) {
	expiry := mustTime(t, "2026-11-15T00:00:00Z")
	inv := &Inventory{RIs: []ReservedInstance{{
		ID: "ri-m5-xlarge", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
		Platform: "Linux/UNIX", Tenancy: "default", EffectiveHourlyUSD: 0.121, Expires: expiry,
	}}}

	t.Run("nothing-absorbs-the-freed-units", func(t *testing.T) {
		before := Usage{Lines: []UsageLine{ec2Line("i-1", "m5.xlarge", 1, 0.192)}}
		after := Usage{Lines: []UsageLine{ec2Line("i-1", "m5.large", 1, 0.096)}}

		gross := before.OnDemandHourlyUSD() - after.OnDemandHourlyUSD()
		near(t, gross/before.OnDemandHourlyUSD(), 0.50, 1e-9, "naive claim: 50 %")

		as := inv.NetSavings(before, after)
		near(t, as.Before.HourlyUSD, 0.121, 1e-9, "bill before")
		near(t, as.After.HourlyUSD, 0.121, 1e-9, "bill after: identical, 4 units stranded")
		near(t, as.NetHourlyUSD, 0, 1e-9, "realized: 0 %")
		near(t, as.StrandedHourlyUSD, 0.121/2, 1e-9, "half the reservation stranded")

		if !as.Suppressed || as.ReasonCode != ReasonCommitmentNeutral {
			t.Fatalf("must be suppressed as commitment-neutral, got %+v", as)
		}
		if as.ClaimableHourlyUSD() != 0 || as.ClaimableMonthlyUSD() != 0 {
			t.Error("a wash must claim nothing")
		}
		if !as.ValidFrom.Equal(expiry) {
			t.Errorf("ValidFrom = %v, want %v", as.ValidFrom, expiry)
		}
		// Coverage shows why: the reservation still bills, half unused.
		near(t, covOf(t, as.After, "i-1").RIQty, 1, 1e-9, "the m5.large is fully covered")
		near(t, as.After.RIUsedUSD, 0.121/2, 1e-9, "only half the reservation is absorbed")
	})

	t.Run("other-m5-usage-absorbs-the-freed-units", func(t *testing.T) {
		// The ledger checks the claim against observed uncovered m5 usage:
		// with a second m5.large already paying on-demand, the freed 4 units
		// land on it and the full list-price saving is realized.
		before := Usage{Lines: []UsageLine{
			ec2Line("i-1", "m5.xlarge", 1, 0.192),
			ec2Line("i-2", "m5.large", 1, 0.096),
		}}
		after := Usage{Lines: []UsageLine{
			ec2Line("i-1", "m5.large", 1, 0.096),
			ec2Line("i-2", "m5.large", 1, 0.096),
		}}
		as := inv.NetSavings(before, after)
		// Before: RI covers i-2 (smallest first, 4 units) then half of i-1.
		near(t, as.Before.HourlyUSD, 0.121+0.096, 1e-9, "bill before")
		near(t, as.After.HourlyUSD, 0.121, 1e-9, "bill after: both m5.large fit the reservation")
		near(t, as.NetHourlyUSD, 0.096, 1e-9, "net equals gross once absorbed")
		near(t, as.GrossHourlyUSD, 0.096, 1e-9, "gross")
		if as.Suppressed {
			t.Fatalf("absorbed savings are real and must not be suppressed: %+v", as)
		}
		near(t, as.After.StrandedUSD, 0, 1e-9, "nothing stranded")
	})
}

// TestStrandingExample3ComputeSPUnderCommitment: a fully utilized $2.00/h
// Compute Savings Plan. Rightsizing below the commitment pays the difference
// anyway — the commitment is per-hour and does not carry over.
func TestStrandingExample3ComputeSPUnderCommitment(t *testing.T) {
	expiry := mustTime(t, "2027-01-31T00:00:00Z")
	inv := &Inventory{SavingsPlans: []SavingsPlan{{
		ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 2.00, Expires: expiry,
	}}}
	line := func(qty float64) Usage {
		l := ec2Line("fleet", "m5.xlarge", qty, 0.192)
		l.ComputeSPRate = 0.12
		return Usage{Lines: []UsageLine{l}}
	}

	// 20 instance-hours at the SP rate is $2.40, so the plan is fully utilized
	// and $0.64/h spills to on-demand.
	before := line(20)
	b := inv.Bill(before)
	assertPartition(t, b)
	near(t, b.SPConsumedUSD, 2.00, 1e-9, "commitment fully utilized before")
	near(t, b.OnDemandUSD, 0.64, 1e-9, "spill above the commitment")
	near(t, b.HourlyUSD, 2.64, 1e-9, "bill before")

	t.Run("shrink-into-the-commitment", func(t *testing.T) {
		// Down to 15: the on-demand spill disappears, but everything below the
		// commitment is already paid for. Net is the spill, not the list delta.
		as := inv.NetSavings(before, line(15))
		near(t, as.GrossHourlyUSD, 5*0.192, 1e-9, "naive claim")
		near(t, as.NetHourlyUSD, 0.64, 1e-9, "net is the on-demand spill only")
		near(t, as.After.HourlyUSD, 2.00, 1e-9, "bill floors at the commitment")
		if as.Suppressed {
			t.Fatalf("a real (if smaller) saving must survive: %+v", as)
		}
		if as.NetHourlyUSD >= as.GrossHourlyUSD {
			t.Error("net must be strictly below gross while the commitment binds")
		}
	})

	t.Run("shrink-below-the-commitment", func(t *testing.T) {
		// From 10 to 5 instance-hours: both sides are under the commitment, so
		// the whole list-price saving is stranded.
		as := inv.NetSavings(line(10), line(5))
		near(t, as.GrossHourlyUSD, 5*0.192, 1e-9, "naive claim")
		near(t, as.NetHourlyUSD, 0, 1e-9, "realized saving is zero")
		near(t, as.Before.HourlyUSD, 2.00, 1e-9, "bill before is the commitment")
		near(t, as.After.HourlyUSD, 2.00, 1e-9, "bill after is the commitment")
		near(t, as.StrandedHourlyUSD, 5*0.12, 1e-9, "newly stranded commitment")
		if !as.Suppressed || as.ReasonCode != ReasonCommitmentNeutral {
			t.Fatalf("must be suppressed as commitment-neutral, got %+v", as)
		}
		if !as.ValidFrom.Equal(expiry) {
			t.Errorf("ValidFrom = %v, want the plan expiry %v", as.ValidFrom, expiry)
		}
	})

	t.Run("absorbed-account-wide-by-fargate", func(t *testing.T) {
		// Compute Savings Plans cover Fargate and Lambda too, so absorption is
		// account-wide: the same shrink is fully realized when other domains
		// are paying on-demand for eligible usage the freed commitment can
		// take over. This is why commitments are modelled once, centrally.
		fargate := UsageLine{ID: "fargate", Kind: KindFargate, Region: "us-east-1",
			Unit: "vCPU-Hours", Quantity: 100, ODRate: 0.04048, ComputeSPRate: 0.028}
		withFargate := func(qty float64) Usage {
			u := line(qty)
			u.Lines = append(u.Lines, fargate)
			return u
		}
		as := inv.NetSavings(withFargate(10), withFargate(5))
		near(t, as.StrandedHourlyUSD, 0, 1e-9, "freed commitment lands on Fargate")
		if as.Suppressed {
			t.Fatalf("absorbed savings must not be suppressed: %+v", as)
		}
		if as.NetHourlyUSD <= 0 {
			t.Errorf("net = %v, want a positive realized saving", as.NetHourlyUSD)
		}
	})
}

// TestNoCommitmentsMeansNetEqualsGross: the ledger must be invisible to
// accounts that hold no commitments. A nil inventory prices at on-demand.
func TestNoCommitmentsMeansNetEqualsGross(t *testing.T) {
	before := Usage{Lines: []UsageLine{ec2Line("i-1", "m5.xlarge", 1, 0.192)}}
	after := Usage{Lines: []UsageLine{ec2Line("i-1", "m5.large", 1, 0.096)}}
	for name, inv := range map[string]*Inventory{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			as := inv.NetSavings(before, after)
			near(t, as.NetHourlyUSD, as.GrossHourlyUSD, 1e-9, "net equals gross")
			near(t, as.NetHourlyUSD, 0.096, 1e-9, "the full list-price delta is real")
			if as.Suppressed {
				t.Fatalf("nothing to strand, nothing to suppress: %+v", as)
			}
			near(t, as.ClaimableMonthlyUSD(), 0.096*HoursPerMonth, 1e-9, "monthly claim")
		})
	}
}
