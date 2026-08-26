package domain

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

func fargateLine(id string, rate, qty float64) commit.UsageLine {
	return commit.UsageLine{ID: id, Kind: commit.KindFargate, Region: "us-east-1",
		Unit: "pod-hours", Quantity: qty, ODRate: rate}
}

// TestNilLedgerNetEqualsGross: with no commitment information nothing can be
// stranded, so the bill delta is the on-demand delta.
func TestNilLedgerNetEqualsGross(t *testing.T) {
	before := []commit.UsageLine{fargateLine("f/a", 0.12097, 1)}
	after := []commit.UsageLine{fargateLine("f/a", 0.07604, 1)}
	for _, tc := range []struct {
		name string
		l    *Ledger
	}{
		{"nil *Ledger", nil},
		{"nil inventory", NewLedger(nil, commit.Usage{})},
		{"empty inventory", NewLedger(&commit.Inventory{}, commit.Usage{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			as := tc.l.Net(before, after)
			if as.Suppressed {
				t.Fatalf("uncommitted change suppressed: %s", as.Reason)
			}
			want := 0.12097 - 0.07604
			if math.Abs(as.NetHourlyUSD-want) > 1e-9 || math.Abs(as.GrossHourlyUSD-want) > 1e-9 {
				t.Fatalf("net=%v gross=%v, want %v for both", as.NetHourlyUSD, as.GrossHourlyUSD, want)
			}
		})
	}
}

// TestLedgerSplicesIntoAccountWideUsage is the reason this seam exists:
// Compute Savings Plans absorb usage account-wide, so the assessment must be
// computed against the whole account, not against the changed lines alone.
func TestLedgerSplicesIntoAccountWideUsage(t *testing.T) {
	ec2Line := commit.UsageLine{ID: "ec2/i-1", Kind: commit.KindEC2, InstanceType: "m5.xlarge",
		Region: "us-east-1", Quantity: 1, ODRate: 0.192, ComputeSPRate: 0.1}
	// A known Fargate Savings-Plan rate, so the bill genuinely moves. (Kilter
	// does not normally have one — see TestFargateWithoutSPRateNetsZero.)
	before := fargateLine("f/a", 0.12097, 1)
	before.ComputeSPRate = 0.06
	after := fargateLine("f/a", 0.07604, 1)
	after.ComputeSPRate = 0.038

	inv := &commit.Inventory{SavingsPlans: []commit.SavingsPlan{
		{ID: "sp-1", Type: commit.SPCompute, CommitmentUSDPerHour: 0.10,
			Expires: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}}

	full := NewLedger(inv, commit.Usage{Lines: []commit.UsageLine{ec2Line, before}})
	as := full.Net([]commit.UsageLine{before}, []commit.UsageLine{after})
	if as.Before.HourlyUSD <= as.After.HourlyUSD {
		t.Fatalf("shrinking the pod did not lower the bill: %v -> %v",
			as.Before.HourlyUSD, as.After.HourlyUSD)
	}
	if as.NetHourlyUSD > as.GrossHourlyUSD+1e-12 {
		t.Fatalf("net %v exceeds gross %v", as.NetHourlyUSD, as.GrossHourlyUSD)
	}

	// The same change assessed WITHOUT the account-wide baseline sees a
	// Savings Plan with nothing else to absorb it, so it reports a different —
	// and never larger — saving. That is the bug the seam prevents.
	partial := NewLedger(inv, commit.Usage{})
	asPartial := partial.Net([]commit.UsageLine{before}, []commit.UsageLine{after})
	if asPartial.NetHourlyUSD > as.NetHourlyUSD+1e-12 {
		t.Fatalf("a partial usage view over-claimed: %v > %v", asPartial.NetHourlyUSD, as.NetHourlyUSD)
	}

	// A line the baseline does not know about is added, never dropped.
	newLine := fargateLine("f/new", 0.05, 1)
	newLine.ComputeSPRate = 0.03
	as2 := full.Net(nil, []commit.UsageLine{newLine})
	if as2.After.HourlyUSD <= as2.Before.HourlyUSD {
		t.Fatal("an added line did not raise the bill")
	}
}

// TestFargateWithoutSPRateNetsZero pins the conservative fallback in the exact
// shape the k8s-fargate domain meets it. Kilter has no Fargate Savings-Plan
// rates, so when a Compute Savings Plan exists, pkg/pricing/commit treats the
// covered pod as free at the margin and the commitment as stranded. The bill
// therefore does not move, the change is suppressed as commitment-neutral, and
// the domain claims nothing.
//
// This is deliberately pessimistic: it under-claims savings and can never
// invent them. Wiring real Fargate SP rates is what unlocks the claim.
func TestFargateWithoutSPRateNetsZero(t *testing.T) {
	inv := &commit.Inventory{SavingsPlans: []commit.SavingsPlan{
		{ID: "sp-1", Type: commit.SPCompute, CommitmentUSDPerHour: 0.10},
	}}
	l := NewLedger(inv, commit.Usage{Lines: []commit.UsageLine{fargateLine("f/a", 0.12097, 1)}})
	as := l.Net([]commit.UsageLine{fargateLine("f/a", 0.12097, 1)},
		[]commit.UsageLine{fargateLine("f/a", 0.07604, 1)})

	if !as.Conservative {
		t.Fatal("assessment should be flagged conservative")
	}
	if as.NetHourlyUSD != 0 {
		t.Fatalf("net = %v, want exactly 0 under the fallback", as.NetHourlyUSD)
	}
	if !as.Suppressed || as.ReasonCode != commit.ReasonCommitmentNeutral {
		t.Fatalf("want commitment-neutral suppression, got suppressed=%v code=%q",
			as.Suppressed, as.ReasonCode)
	}
	if as.NetHourlyUSD > as.GrossHourlyUSD {
		t.Fatal("net exceeds gross")
	}
}

// TestLedgerSuppressesCommitmentStranding reproduces §4.4's trap in the shape a
// domain will meet it: a Compute Savings Plan absorbs the whole saving, so the
// change buys risk for nothing.
func TestLedgerSuppressesCommitmentStranding(t *testing.T) {
	inv := &commit.Inventory{SavingsPlans: []commit.SavingsPlan{
		{ID: "sp-big", Type: commit.SPCompute, CommitmentUSDPerHour: 50,
			Expires: time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)},
	}}
	l := NewLedger(inv, commit.Usage{Lines: []commit.UsageLine{fargateLine("f/a", 0.12097, 10)}})
	as := l.Net([]commit.UsageLine{fargateLine("f/a", 0.12097, 10)},
		[]commit.UsageLine{fargateLine("f/a", 0.07604, 10)})

	if !as.Suppressed {
		t.Fatalf("fully-committed change not suppressed: net=%v gross=%v", as.NetHourlyUSD, as.GrossHourlyUSD)
	}
	if as.ReasonCode != commit.ReasonCommitmentNeutral && as.ReasonCode != commit.ReasonCommitmentNegative {
		t.Fatalf("unexpected reason code %q", as.ReasonCode)
	}
	// The suppression codes commit and domain use are the same strings, so a
	// domain can forward them without translation. Pin that.
	if commit.ReasonCommitmentNegative != SuppressCommitmentNegative ||
		commit.ReasonCommitmentNeutral != SuppressCommitmentNeutral {
		t.Fatal("commit and domain suppression codes have diverged")
	}
	// Nothing is NEWLY stranded here (the plan was already fully stranded), so
	// there is no date on which the arithmetic changes. The assessment says so
	// in prose rather than inventing one.
	if !as.ValidFrom.IsZero() {
		t.Errorf("ValidFrom = %v, want zero: nothing is newly stranded", as.ValidFrom)
	}
	if !contains(as.Reason, "will not lapse on its own") {
		t.Errorf("reason %q should say the suppression does not lapse", as.Reason)
	}
	if as.ClaimableHourlyUSD() != 0 {
		t.Errorf("suppressed assessment claims $%v", as.ClaimableHourlyUSD())
	}
}

// TestSpliceIsOrderIndependent: the baseline is canonicalized, so the bill a
// domain sees cannot depend on the order lines were collected in.
func TestSpliceIsOrderIndependent(t *testing.T) {
	lines := []commit.UsageLine{
		fargateLine("f/c", 0.05, 1), fargateLine("f/a", 0.06, 1), fargateLine("f/b", 0.07, 1),
	}
	rng := rand.New(rand.NewSource(3))
	var want float64
	for i := 0; i < 100; i++ {
		shuffled := append([]commit.UsageLine(nil), lines...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		l := NewLedger(&commit.Inventory{}, commit.Usage{Lines: shuffled})
		as := l.Net([]commit.UsageLine{fargateLine("f/b", 0.07, 1)},
			[]commit.UsageLine{fargateLine("f/b", 0.03, 1)})
		if i == 0 {
			want = as.NetHourlyUSD
			continue
		}
		if as.NetHourlyUSD != want {
			t.Fatalf("splice order changed the net: %v vs %v", as.NetHourlyUSD, want)
		}
	}
	if math.Abs(want-0.04) > 1e-9 {
		t.Fatalf("net = %v, want 0.04", want)
	}
}

func TestSpliceReplacesByIDAndAppendsAnonymous(t *testing.T) {
	base := []commit.UsageLine{fargateLine("a", 1, 1), fargateLine("b", 2, 1)}
	got := splice(base, []commit.UsageLine{fargateLine("b", 9, 1), fargateLine("c", 3, 1), fargateLine("", 4, 1)})
	if len(got) != 4 {
		t.Fatalf("splice produced %d lines: %+v", len(got), got)
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[1].ODRate != 9 {
		t.Fatalf("replace-by-ID failed: %+v", got)
	}
	if got[2].ID != "c" || got[3].ID != "" {
		t.Fatalf("append order wrong: %+v", got)
	}
	// The baseline is not mutated: it is reused for every assessment.
	if base[1].ODRate != 2 {
		t.Fatal("splice mutated the baseline")
	}
}
