package commit

import (
	"math/rand/v2"
	"reflect"
	"testing"
)

// richScenario exercises every stage of the waterfall at once: zonal and
// regional reservations, a non-size-flexible reservation, two EC2 Instance
// Savings Plan scopes, two pooled Compute plans, and Fargate/Lambda lines that
// only a Compute plan can reach.
func richScenario() (Usage, *Inventory) {
	u := Usage{Lines: []UsageLine{
		{ID: "a", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a", InstanceType: "m5.large",
			Quantity: 3, ODRate: 0.096, ComputeSPRate: 0.061, EC2SPRate: 0.055},
		{ID: "b", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1b", InstanceType: "m5.xlarge",
			Quantity: 2, ODRate: 0.192, ComputeSPRate: 0.122, EC2SPRate: 0.110},
		{ID: "c", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a", InstanceType: "c5.2xlarge",
			Quantity: 1, ODRate: 0.34, ComputeSPRate: 0.21, EC2SPRate: 0.19},
		{ID: "d", Kind: KindEC2, Region: "eu-west-1", AZ: "eu-west-1a", InstanceType: "m5.large",
			Platform: "Windows", Quantity: 2, ODRate: 0.188},
		{ID: "e", Kind: KindFargate, Region: "us-east-1", Unit: "vCPU-Hours",
			Quantity: 40, ODRate: 0.04048, ComputeSPRate: 0.028},
		{ID: "f", Kind: KindFargate, Region: "us-east-1", Unit: "GB-Hours",
			Quantity: 80, ODRate: 0.004445, ComputeSPRate: 0.0031},
		{ID: "g", Kind: KindLambda, Region: "us-east-1", Unit: "GB-Seconds",
			Quantity: 500000, ODRate: 0.0000166667, ComputeSPRate: 0.0000141},
		{ID: "h", Kind: KindLambda, Region: "us-east-1", Unit: "Requests-Millions",
			Quantity: 4, ODRate: 0.20, SPIneligible: true},
		{ID: "i", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1c", InstanceType: "zz9.plural",
			Quantity: 1, ODRate: 1.50},
	}}
	inv := &Inventory{
		RIs: []ReservedInstance{
			{ID: "ri-zonal", Count: 1, InstanceType: "m5.large", Region: "us-east-1",
				AZ: "us-east-1a", EffectiveHourlyUSD: 0.061},
			{ID: "ri-regional", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
				EffectiveHourlyUSD: 0.121},
			{ID: "ri-windows", Count: 1, InstanceType: "m5.large", Region: "eu-west-1",
				Platform: "Windows", EffectiveHourlyUSD: 0.12},
			{ID: "ri-unknown-type", Count: 1, InstanceType: "zz9.plural", Region: "us-east-1",
				AZ: "us-east-1c", EffectiveHourlyUSD: 0.9},
		},
		SavingsPlans: []SavingsPlan{
			{ID: "sp-ec2-m5", Type: SPEC2Instance, CommitmentUSDPerHour: 0.20, Region: "us-east-1", Family: "m5"},
			{ID: "sp-ec2-c5", Type: SPEC2Instance, CommitmentUSDPerHour: 0.10, Region: "us-east-1", Family: "c5"},
			{ID: "sp-compute-1", Type: SPCompute, CommitmentUSDPerHour: 0.50},
			{ID: "sp-compute-2", Type: SPCompute, CommitmentUSDPerHour: 0.75},
		},
	}
	return u, inv
}

// TestBillIsIndependentOfInputOrder pins the determinism invariant: Bill is a
// pure function of the multiset of usage lines and commitments. Shuffling the
// input — or Go's per-run randomized map iteration order — must not move a
// single number, because a plan that changes between runs cannot be approved,
// fingerprinted or audited.
func TestBillIsIndependentOfInputOrder(t *testing.T) {
	u, inv := richScenario()
	if err := u.Validate(); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if err := inv.Validate(); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	want := inv.Bill(u)
	assertPartition(t, want)
	if want.OnDemandUSD <= 0 || want.SPConsumedUSD <= 0 || want.RIUsedUSD <= 0 {
		t.Fatalf("scenario must exercise every stage, got %+v", want)
	}

	r := rand.New(rand.NewPCG(0x5eed, 0xf00d))
	for i := 0; i < 200; i++ {
		su := Usage{Lines: append([]UsageLine(nil), u.Lines...)}
		r.Shuffle(len(su.Lines), func(a, b int) { su.Lines[a], su.Lines[b] = su.Lines[b], su.Lines[a] })
		si := &Inventory{
			RIs:          append([]ReservedInstance(nil), inv.RIs...),
			SavingsPlans: append([]SavingsPlan(nil), inv.SavingsPlans...),
		}
		r.Shuffle(len(si.RIs), func(a, b int) { si.RIs[a], si.RIs[b] = si.RIs[b], si.RIs[a] })
		r.Shuffle(len(si.SavingsPlans), func(a, b int) {
			si.SavingsPlans[a], si.SavingsPlans[b] = si.SavingsPlans[b], si.SavingsPlans[a]
		})

		got := si.Bill(su)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d changed the bill:\n got %+v\nwant %+v", i, got, want)
		}
		// Bit-identical, not merely close: the sums are ordered too.
		if got.HourlyUSD != want.HourlyUSD {
			t.Fatalf("permutation %d: HourlyUSD %v != %v", i, got.HourlyUSD, want.HourlyUSD)
		}
	}
}

// TestNetSavingsIsIndependentOfInputOrder extends the same guarantee to the
// number plans and the API actually publish.
func TestNetSavingsIsIndependentOfInputOrder(t *testing.T) {
	u, inv := richScenario()
	after := Usage{Lines: append([]UsageLine(nil), u.Lines...)}
	after.Lines[1].InstanceType, after.Lines[1].ODRate = "m5.large", 0.096
	after.Lines[4].Quantity = 20

	want := inv.NetSavings(u, after)
	r := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 100; i++ {
		sb := Usage{Lines: append([]UsageLine(nil), u.Lines...)}
		sa := Usage{Lines: append([]UsageLine(nil), after.Lines...)}
		r.Shuffle(len(sb.Lines), func(a, b int) { sb.Lines[a], sb.Lines[b] = sb.Lines[b], sb.Lines[a] })
		r.Shuffle(len(sa.Lines), func(a, b int) { sa.Lines[a], sa.Lines[b] = sa.Lines[b], sa.Lines[a] })
		si := &Inventory{
			RIs:          append([]ReservedInstance(nil), inv.RIs...),
			SavingsPlans: append([]SavingsPlan(nil), inv.SavingsPlans...),
		}
		r.Shuffle(len(si.RIs), func(a, b int) { si.RIs[a], si.RIs[b] = si.RIs[b], si.RIs[a] })
		if got := si.NetSavings(sb, sa); !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d changed the assessment:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

// TestBillIsRaceFreeAcrossGoroutines: Bill takes the inventory by pointer and
// must not mutate it, so concurrent callers (the API server prices while the
// planner plans) cannot interfere. Run under -race.
func TestBillIsRaceFreeAcrossGoroutines(t *testing.T) {
	u, inv := richScenario()
	want := inv.Bill(u)
	done := make(chan Cost, 8)
	for i := 0; i < 8; i++ {
		go func() { done <- inv.Bill(u) }()
	}
	for i := 0; i < 8; i++ {
		if got := <-done; !reflect.DeepEqual(got, want) {
			t.Fatalf("concurrent bill differs:\n got %+v\nwant %+v", got, want)
		}
	}
	if len(inv.RIs) != 4 || len(inv.SavingsPlans) != 4 {
		t.Error("Bill must not mutate the inventory")
	}
}
