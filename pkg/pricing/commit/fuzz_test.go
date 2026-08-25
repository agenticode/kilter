package commit

import (
	"math"
	"reflect"
	"testing"
	"time"
)

// stream turns fuzz bytes into bounded, always-finite inputs. Bounding keeps
// the arithmetic in a range where a fixed absolute tolerance is meaningful;
// non-finite input is covered separately by
// TestBillClampsGarbageInsteadOfPoisoningTotals.
type stream struct {
	b []byte
	i int
}

func (s *stream) next() byte {
	if s.i >= len(s.b) {
		s.i++
		return 0
	}
	v := s.b[s.i]
	s.i++
	return v
}

func (s *stream) frac(scale float64) float64 { return float64(s.next()) / 255 * scale }
func (s *stream) mod(n int) int {
	if n <= 0 {
		return 0
	}
	return int(s.next()) % n
}
func (s *stream) pick(v []string) string { return v[s.mod(len(v))] }

var (
	fuzzTypes     = []string{"m5.large", "m5.xlarge", "m5.2xlarge", "c4.xlarge", "i3.metal", "g4dn.xlarge", "zz9.plural"}
	fuzzRegions   = []string{"us-east-1", "eu-west-1"}
	fuzzAZs       = []string{"", "us-east-1a", "us-east-1b", "eu-west-1a"}
	fuzzPlatforms = []string{"", "Linux/UNIX", "Windows", "RHEL"}
	fuzzTenancies = []string{"", "default", "dedicated"}
	fuzzKinds     = []Kind{KindEC2, KindFargate, KindLambda}
	fuzzUnits     = []string{"vCPU-Hours", "GB-Hours", "GB-Seconds", "Requests-Millions"}
)

func synth(data []byte) (Usage, *Inventory) {
	s := &stream{b: data}
	var u Usage
	for n := 1 + s.mod(6); n > 0; n-- {
		l := UsageLine{
			ID:       string(rune('a' + s.mod(8))),
			Kind:     fuzzKinds[s.mod(len(fuzzKinds))],
			Region:   s.pick(fuzzRegions),
			AZ:       s.pick(fuzzAZs),
			Platform: s.pick(fuzzPlatforms),
			Tenancy:  s.pick(fuzzTenancies),
			Quantity: s.frac(100),
			ODRate:   s.frac(2),
		}
		if l.Kind == KindEC2 {
			l.InstanceType = s.pick(fuzzTypes)
		} else {
			l.Unit = s.pick(fuzzUnits)
		}
		if s.next()&1 == 0 { // half the time the SP rate is unknown
			l.ComputeSPRate = l.ODRate * s.frac(1)
		}
		if s.next()&1 == 0 {
			l.EC2SPRate = l.ODRate * s.frac(1)
		}
		l.SPIneligible = s.next()&3 == 0
		u.Lines = append(u.Lines, l)
	}

	inv := &Inventory{}
	for n := s.mod(5); n > 0; n-- {
		inv.RIs = append(inv.RIs, ReservedInstance{
			ID:                 "ri" + string(rune('0'+s.mod(9))),
			Count:              1 + s.mod(5),
			InstanceType:       s.pick(fuzzTypes),
			Region:             s.pick(fuzzRegions),
			AZ:                 s.pick(fuzzAZs),
			Platform:           s.pick(fuzzPlatforms),
			Tenancy:            s.pick(fuzzTenancies),
			EffectiveHourlyUSD: s.frac(1),
			Expires:            time.Unix(int64(s.mod(200))*86400, 0).UTC(),
		})
	}
	for n := s.mod(5); n > 0; n-- {
		sp := SavingsPlan{
			ID:                   "sp" + string(rune('0'+s.mod(9))),
			Type:                 SPCompute,
			CommitmentUSDPerHour: s.frac(30),
			Expires:              time.Unix(int64(s.mod(200))*86400, 0).UTC(),
		}
		if s.next()&1 == 0 {
			sp.Type = SPEC2Instance
			sp.Region = s.pick(fuzzRegions)
			sp.Family = FamilyOf(s.pick(fuzzTypes))
		}
		inv.SavingsPlans = append(inv.SavingsPlans, sp)
	}
	return u, inv
}

// FuzzWaterfall asserts the structural invariants of Bill on arbitrary input:
// it never allocates more commitment than exists, never bills below the
// committed floor, never loses or invents usage, and never returns a
// different answer for the same input.
func FuzzWaterfall(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{3, 0, 0, 1, 1, 200, 128, 0, 0, 2, 2, 1, 5, 90, 4, 4, 40, 200, 60})
	f.Add([]byte{5, 2, 1, 0, 3, 250, 250, 1, 1, 0, 0, 0, 3, 3, 3, 3, 250, 7, 7, 7, 200, 9, 9})
	f.Add([]byte{6, 1, 1, 1, 1, 100, 100, 0, 50, 0, 60, 1, 4, 2, 2, 2, 2, 100, 4, 255, 255, 255, 255})

	f.Fuzz(func(t *testing.T, data []byte) {
		const tol = 1e-6
		u, inv := synth(data)
		c := inv.Bill(u)

		// Deterministic: same input, same answer, bit for bit.
		if again := inv.Bill(u); !reflect.DeepEqual(again, c) {
			t.Fatalf("Bill is not deterministic:\n%+v\n%+v", c, again)
		}

		// Finite: no NaN or Inf may escape into a total.
		for name, v := range map[string]float64{
			"HourlyUSD": c.HourlyUSD, "RICommittedUSD": c.RICommittedUSD,
			"RIUsedUSD": c.RIUsedUSD, "SPCommittedUSD": c.SPCommittedUSD,
			"SPConsumedUSD": c.SPConsumedUSD, "OnDemandUSD": c.OnDemandUSD,
			"StrandedUSD": c.StrandedUSD,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s is not finite: %v", name, v)
			}
			if v < -tol {
				t.Fatalf("%s is negative: %v", name, v)
			}
		}

		// The bill partitions exactly, and never dips below the committed
		// floor: you cannot bill your way out of what you already promised.
		floor := c.RICommittedUSD + c.SPCommittedUSD
		if d := c.HourlyUSD - (floor + c.OnDemandUSD); d > tol || d < -tol {
			t.Fatalf("bill %v != committed %v + on-demand %v", c.HourlyUSD, floor, c.OnDemandUSD)
		}
		if c.HourlyUSD < floor-tol {
			t.Fatalf("bill %v below committed floor %v", c.HourlyUSD, floor)
		}

		// Never allocate more commitment than exists — per commitment…
		for _, cu := range c.Commitments {
			if cu.UsedUSD > cu.CommittedUSD+tol {
				t.Fatalf("commitment %q used %v > committed %v", cu.ID, cu.UsedUSD, cu.CommittedUSD)
			}
			if cu.UsedUSD < -tol {
				t.Fatalf("commitment %q used %v < 0", cu.ID, cu.UsedUSD)
			}
		}
		// …and in aggregate.
		if c.RIUsedUSD > c.RICommittedUSD+tol {
			t.Fatalf("RI used %v > committed %v", c.RIUsedUSD, c.RICommittedUSD)
		}
		if c.SPConsumedUSD > c.SPCommittedUSD+tol {
			t.Fatalf("SP consumed %v > committed %v", c.SPConsumedUSD, c.SPCommittedUSD)
		}

		// Reservations are allocated in normalization units, so check that
		// budget directly: the units handed out cannot exceed the units owned.
		var riUnitsAvailable float64
		for _, ri := range inv.RIs {
			per, ok := InstanceUnits(ri.InstanceType)
			if !ok {
				per = 1
			}
			riUnitsAvailable += float64(ri.Count) * per
		}
		var riUnitsAllocated float64
		for _, cv := range c.Coverage {
			// Every component of the partition is non-negative and sums back
			// to the line's quantity: usage is never lost or invented.
			for _, q := range []float64{cv.RIQty, cv.EC2SPQty, cv.ComputeSPQty, cv.OnDemandQty} {
				if q < -tol || math.IsNaN(q) {
					t.Fatalf("line %q: bad coverage component %v", cv.LineID, q)
				}
			}
			sum := cv.RIQty + cv.EC2SPQty + cv.ComputeSPQty + cv.OnDemandQty
			if d := sum - cv.Quantity; d > tol || d < -tol {
				t.Fatalf("line %q: coverage %v != quantity %v", cv.LineID, sum, cv.Quantity)
			}
			if cv.OnDemandUSD < -tol {
				t.Fatalf("line %q: negative on-demand charge %v", cv.LineID, cv.OnDemandUSD)
			}
		}
		// Coverage is emitted in canonical order, so it is index-aligned with
		// the canonically ordered usage. Matching by ID alone is not enough:
		// distinct lines may legitimately share an ID.
		states := newStates(u.Lines)
		if len(states) != len(c.Coverage) {
			t.Fatalf("coverage has %d entries for %d lines", len(c.Coverage), len(states))
		}
		for i, cv := range c.Coverage {
			if states[i].line.ID != cv.LineID {
				t.Fatalf("coverage %d is not aligned with the usage: %q vs %q",
					i, cv.LineID, states[i].line.ID)
			}
			if cv.RIQty == 0 {
				continue
			}
			per, ok := InstanceUnits(states[i].line.InstanceType)
			if !ok {
				per = 1 // the waterfall counts whole instances for unknown types
			}
			riUnitsAllocated += cv.RIQty * per
		}
		if riUnitsAllocated > riUnitsAvailable+tol {
			t.Fatalf("allocated %v normalization units from a pool of %v",
				riUnitsAllocated, riUnitsAvailable)
		}
	})
}

// FuzzNetSavings asserts that the published number is always consistent with
// the two bills behind it, and that a suppressed recommendation claims nothing.
func FuzzNetSavings(f *testing.F) {
	f.Add([]byte{2, 1, 1, 0, 0, 200, 128, 0, 0, 3, 3}, []byte{2, 1, 1, 0, 0, 100, 128, 0, 0, 3, 3})
	f.Add([]byte{4, 0, 0, 0, 0, 255, 255, 1, 1, 1, 1}, []byte{4, 0, 0, 0, 0, 10, 255, 1, 1, 1, 1})

	f.Fuzz(func(t *testing.T, beforeData, afterData []byte) {
		const tol = 1e-6
		before, inv := synth(beforeData)
		after, _ := synth(afterData)

		as := inv.NetSavings(before, after)
		if math.IsNaN(as.NetHourlyUSD) || math.IsInf(as.NetHourlyUSD, 0) {
			t.Fatalf("net is not finite: %v", as.NetHourlyUSD)
		}
		if d := as.NetHourlyUSD - (as.Before.HourlyUSD - as.After.HourlyUSD); d > tol || d < -tol {
			t.Fatalf("net %v is not the bill delta %v - %v",
				as.NetHourlyUSD, as.Before.HourlyUSD, as.After.HourlyUSD)
		}
		if d := as.NetMonthlyUSD - as.NetHourlyUSD*HoursPerMonth; d > tol || d < -tol {
			t.Fatalf("monthly net %v inconsistent with hourly %v", as.NetMonthlyUSD, as.NetHourlyUSD)
		}
		if as.Suppressed {
			if as.ClaimableHourlyUSD() != 0 || as.ClaimableMonthlyUSD() != 0 {
				t.Fatalf("suppressed recommendation claimed %v", as.ClaimableHourlyUSD())
			}
			if as.ReasonCode == "" || as.Reason == "" {
				t.Fatal("suppression must carry a reason code and a reason")
			}
		}
		if as.ClaimableHourlyUSD() < 0 {
			t.Fatalf("claimed a negative saving: %v", as.ClaimableHourlyUSD())
		}
		if !as.Suppressed && as.NetHourlyUSD > Eps &&
			as.ClaimableHourlyUSD() != as.NetHourlyUSD {
			t.Fatalf("unsuppressed claim %v != net %v", as.ClaimableHourlyUSD(), as.NetHourlyUSD)
		}
		// A suppression that carries a date must point at a commitment that
		// actually exists in the inventory.
		if !as.ValidFrom.IsZero() {
			found := false
			for _, ri := range inv.RIs {
				found = found || ri.Expires.Equal(as.ValidFrom)
			}
			for _, sp := range inv.SavingsPlans {
				found = found || sp.Expires.Equal(as.ValidFrom)
			}
			if !found {
				t.Fatalf("ValidFrom %v matches no commitment expiry", as.ValidFrom)
			}
		}
	})
}
