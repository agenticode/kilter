package commit

import (
	"math"
	"strings"
	"testing"
)

// The conservative fallback for missing Savings Plans rates (§4.4: "v1 falls
// back to a conservative bound … conservative means Kilter under-promises
// savings, never over-promises"). The rule, restated: a Savings-Plan-eligible
// line with no known rate is billed at zero marginal cost and consumes none of
// the commitment, on BOTH sides of a comparison, so a change confined to such
// lines nets exactly $0.

func spInv(commitment float64) *Inventory {
	return &Inventory{SavingsPlans: []SavingsPlan{
		{ID: "sp", Type: SPCompute, CommitmentUSDPerHour: commitment},
	}}
}

func fleet(qty, spRate float64, instanceType string, od float64) Usage {
	l := ec2Line("fleet", instanceType, qty, od)
	l.ComputeSPRate = spRate
	return Usage{Lines: []UsageLine{l}}
}

// TestConservativeFallbackUnderClaims: with rates known the shrink is worth
// $1.92/h; with rates missing it is worth $0. Never the other way round.
func TestConservativeFallbackUnderClaims(t *testing.T) {
	inv := spInv(2.00)

	informed := inv.NetSavings(fleet(30, 0.12, "m5.xlarge", 0.192), fleet(20, 0.12, "m5.xlarge", 0.192))
	near(t, informed.NetHourlyUSD, 1.92, 1e-9, "net with rates known")
	if informed.Conservative {
		t.Error("full rate data must not be flagged conservative")
	}

	blind := inv.NetSavings(fleet(30, 0, "m5.xlarge", 0.192), fleet(20, 0, "m5.xlarge", 0.192))
	near(t, blind.NetHourlyUSD, 0, 1e-9, "net with rates missing")
	if !blind.Conservative {
		t.Error("a rate-less bill must be flagged conservative")
	}
	if !blind.Before.Fallback || !blind.After.Fallback {
		t.Error("both bills must record the fallback")
	}
	if blind.NetHourlyUSD > informed.NetHourlyUSD+Eps {
		t.Fatalf("fallback claimed %v > informed %v: the fallback must under-claim, never over-claim",
			blind.NetHourlyUSD, informed.NetHourlyUSD)
	}
	if !strings.Contains(blind.Reason, "conservative floor") {
		t.Errorf("a conservative result must say so; reason = %q", blind.Reason)
	}
}

// TestConservativeFallbackUnderClaimsAcrossShapes pins the direction of the
// rule over a spread of commitment sizes and usage changes: the rate-less
// answer is never better than the fully-informed one.
func TestConservativeFallbackUnderClaimsAcrossShapes(t *testing.T) {
	cases := []struct{ commitment, beforeQty, afterQty, spRate, od float64 }{
		{2.00, 30, 20, 0.12, 0.192},   // saturated both sides
		{2.00, 20, 10, 0.12, 0.192},   // saturated, then under
		{2.00, 10, 5, 0.12, 0.192},    // under both sides
		{0.01, 4, 1, 0.12, 0.192},     // commitment barely binds
		{50.00, 100, 40, 0.12, 0.192}, // commitment never binds
		{2.00, 8, 8, 0.12, 0.192},     // no change at all
	}
	for _, c := range cases {
		inv := spInv(c.commitment)
		informed := inv.NetSavings(
			fleet(c.beforeQty, c.spRate, "m5.xlarge", c.od),
			fleet(c.afterQty, c.spRate, "m5.xlarge", c.od))
		blind := inv.NetSavings(
			fleet(c.beforeQty, 0, "m5.xlarge", c.od),
			fleet(c.afterQty, 0, "m5.xlarge", c.od))
		if blind.ClaimableHourlyUSD() > informed.ClaimableHourlyUSD()+Eps {
			t.Errorf("commitment %v, %v→%v: fallback claims %v > informed %v",
				c.commitment, c.beforeQty, c.afterQty,
				blind.ClaimableHourlyUSD(), informed.ClaimableHourlyUSD())
		}
	}
}

// TestConservativeFallbackHarmonizesRateAvailability is the one way the
// fallback could over-claim: a line priced with a known rate before the change
// and no known rate after would appear to drop to zero cost, manufacturing a
// saving out of a pure family swap. NetSavings forces both sides onto the
// fallback path for any line either side could not price.
func TestConservativeFallbackHarmonizesRateAvailability(t *testing.T) {
	inv := spInv(2.00)
	before := fleet(30, 0.12, "m5.xlarge", 0.192) // rate known
	after := fleet(30, 0, "m7g.xlarge", 0.1632)   // same count, rate unknown

	// The raw bills do show the phantom: this is why NetSavings, not a pair of
	// Bill calls, is the supported way to evaluate a change.
	b, a := inv.Bill(before), inv.Bill(after)
	if !a.Fallback {
		t.Fatal("precondition: the after-bill must take the fallback")
	}
	phantom := b.HourlyUSD - a.HourlyUSD
	near(t, phantom, 2.56, 1e-9, "unharmonized bill delta (the phantom saving)")

	as := inv.NetSavings(before, after)
	near(t, as.NetHourlyUSD, 0, 1e-9, "harmonized net: no phantom")
	if !as.Conservative {
		t.Error("result must be flagged conservative")
	}
	if as.ClaimableHourlyUSD() != 0 {
		t.Errorf("claimed %v from an unpriceable change", as.ClaimableHourlyUSD())
	}
}

// TestFallbackOnlyWhenACommitmentCouldCover: with no Savings Plan in the
// inventory there is nothing to strand, so a missing rate is irrelevant and
// the usage bills at on-demand.
func TestFallbackOnlyWhenACommitmentCouldCover(t *testing.T) {
	inv := &Inventory{}
	c := inv.Bill(fleet(10, 0, "m5.xlarge", 0.192))
	assertPartition(t, c)
	if c.Fallback {
		t.Error("no commitment exists, so no fallback should be reported")
	}
	near(t, c.OnDemandUSD, 1.92, 1e-9, "everything bills on-demand")
}

// TestBillClampsGarbageInsteadOfPoisoningTotals mirrors pkg/pricing's posture:
// decision code must not propagate NaN or ±Inf into a total. Validate is where
// bad data is meant to fail loudly.
func TestBillClampsGarbageInsteadOfPoisoningTotals(t *testing.T) {
	inf, nan := math.Inf(1), math.NaN()
	u := Usage{Lines: []UsageLine{
		{ID: "nan-qty", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.large", Quantity: nan, ODRate: 0.096},
		{ID: "inf-rate", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.large", Quantity: 1, ODRate: inf},
		{ID: "negative", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.large", Quantity: -5, ODRate: 0.096},
		{ID: "good", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.large", Quantity: 2, ODRate: 0.096},
	}}
	inv := &Inventory{RIs: []ReservedInstance{
		{ID: "bad-ri", Count: -3, InstanceType: "m5.large", Region: "us-east-1", EffectiveHourlyUSD: nan},
	}}
	c := inv.Bill(u)
	assertPartition(t, c)
	if isNaNOrInf(c.HourlyUSD) || isNaNOrInf(c.OnDemandUSD) || isNaNOrInf(c.StrandedUSD) {
		t.Fatalf("garbage propagated into totals: %+v", c)
	}
	near(t, c.HourlyUSD, 2*0.096, 1e-9, "only the well-formed line is priced")

	if err := u.Validate(); err == nil {
		t.Error("Validate must reject the garbage usage loudly")
	}
	if err := inv.Validate(); err == nil {
		t.Error("Validate must reject the garbage inventory loudly")
	}
}

func math_Inf() float64         { return 1 / zero() }
func math_NaN() float64         { return zero() / zero() }
func zero() float64             { return 0 }
func isNaNOrInf(f float64) bool { return !finite(f) }
