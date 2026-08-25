package commit

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/pricing"
)

// TestHoursPerMonthMatchesPricing keeps this package's monthly projection tied
// to the rest of Kilter's. The constant is duplicated rather than imported so
// pkg/pricing stays free to depend on this package later without an import
// cycle; this test is what stops the duplicate from drifting.
func TestHoursPerMonthMatchesPricing(t *testing.T) {
	if HoursPerMonth != pricing.HoursPerMonth {
		t.Fatalf("commit.HoursPerMonth = %d, pricing.HoursPerMonth = %d", HoursPerMonth, pricing.HoursPerMonth)
	}
}

// TestInventoryJSONRoundTrip: the offline path. sync-commitments writes this
// file; the decision path reads it with no credentials and no network.
func TestInventoryJSONRoundTrip(t *testing.T) {
	want := &Inventory{
		RIs: []ReservedInstance{{
			ID: "ri-1", Count: 2, InstanceType: "m5.xlarge", Region: "us-east-1",
			AZ: "us-east-1a", Platform: "Linux/UNIX", Tenancy: "default",
			OfferingClass: "standard", EffectiveHourlyUSD: 0.121,
			Expires: mustTime(t, "2027-06-01T00:00:00Z"),
		}},
		SavingsPlans: []SavingsPlan{{
			ID: "sp-1", Type: SPEC2Instance, CommitmentUSDPerHour: 3,
			Region: "us-east-1", Family: "r5", Expires: mustTime(t, "2027-01-01T00:00:00Z"),
		}},
		FetchedAt: mustTime(t, "2026-08-26T00:00:00Z"),
	}
	var buf bytes.Buffer
	if err := WriteInventory(&buf, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	encoded := buf.String()

	got, err := LoadInventory(strings.NewReader(encoded))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.RIs) != 1 || got.RIs[0] != want.RIs[0] {
		t.Errorf("reserved instance round-trip:\n got %+v\nwant %+v", got.RIs, want.RIs)
	}
	if len(got.SavingsPlans) != 1 || got.SavingsPlans[0] != want.SavingsPlans[0] {
		t.Errorf("savings plan round-trip:\n got %+v\nwant %+v", got.SavingsPlans, want.SavingsPlans)
	}
	// Byte-stable: a re-sync with no change must produce no diff.
	var again bytes.Buffer
	if err := WriteInventory(&again, got); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if again.String() != encoded {
		t.Errorf("serialization is not byte-stable:\n%s\n%s", encoded, again.String())
	}
}

func TestLoadInventoryRejectsBadData(t *testing.T) {
	for _, tc := range []struct{ name, json, wantErr string }{
		{"unknown field", `{"reservedInstances":[],"typo":1}`, "parse inventory"},
		{"bad json", `{`, "parse inventory"},
		{"zero count", `{"reservedInstances":[{"id":"a","count":0,"instanceType":"m5.large","region":"us-east-1","effectiveHourlyUSD":0.1}]}`, "count must be"},
		{"no instance type", `{"reservedInstances":[{"id":"a","count":1,"region":"us-east-1","effectiveHourlyUSD":0.1}]}`, "instanceType required"},
		{"no region", `{"reservedInstances":[{"id":"a","count":1,"instanceType":"m5.large","effectiveHourlyUSD":0.1}]}`, "region required"},
		{"negative rate", `{"reservedInstances":[{"id":"a","count":1,"instanceType":"m5.large","region":"us-east-1","effectiveHourlyUSD":-1}]}`, "bad effectiveHourlyUSD"},
		{"duplicate ri id", `{"reservedInstances":[{"id":"a","count":1,"instanceType":"m5.large","region":"us-east-1","effectiveHourlyUSD":0.1},{"id":"a","count":1,"instanceType":"m5.large","region":"us-east-1","effectiveHourlyUSD":0.1}]}`, "duplicate reserved instance"},
		{"unknown plan type", `{"savingsPlans":[{"id":"s","type":"magic","commitmentUSDPerHour":1}]}`, "unknown type"},
		{"negative commitment", `{"savingsPlans":[{"id":"s","type":"compute","commitmentUSDPerHour":-1}]}`, "bad commitment"},
		{"unscoped ec2 plan", `{"savingsPlans":[{"id":"s","type":"ec2-instance","commitmentUSDPerHour":1}]}`, "needs region and family"},
		{"duplicate sp id", `{"savingsPlans":[{"id":"s","type":"compute","commitmentUSDPerHour":1},{"id":"s","type":"compute","commitmentUSDPerHour":1}]}`, "duplicate savings plan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadInventory(strings.NewReader(tc.json))
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q must mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestActiveFiltersExpiredCommitments: this is how a suppression lapses.
func TestActiveFiltersExpiredCommitments(t *testing.T) {
	past := mustTime(t, "2026-01-01T00:00:00Z")
	future := mustTime(t, "2028-01-01T00:00:00Z")
	inv := &Inventory{
		RIs: []ReservedInstance{
			{ID: "expired", Count: 1, InstanceType: "m5.large", Region: "us-east-1", EffectiveHourlyUSD: 0.05, Expires: past},
			{ID: "live", Count: 1, InstanceType: "m5.large", Region: "us-east-1", EffectiveHourlyUSD: 0.05, Expires: future},
			{ID: "open-ended", Count: 1, InstanceType: "m5.large", Region: "us-east-1", EffectiveHourlyUSD: 0.05},
		},
		SavingsPlans: []SavingsPlan{
			{ID: "sp-expired", Type: SPCompute, CommitmentUSDPerHour: 1, Expires: past},
			{ID: "sp-live", Type: SPCompute, CommitmentUSDPerHour: 1, Expires: future},
		},
	}
	now := mustTime(t, "2027-01-01T00:00:00Z")
	got := inv.Active(now)
	if len(got.RIs) != 2 || got.RIs[0].ID != "live" || got.RIs[1].ID != "open-ended" {
		t.Errorf("Active kept the wrong reservations: %+v", got.RIs)
	}
	if len(got.SavingsPlans) != 1 || got.SavingsPlans[0].ID != "sp-live" {
		t.Errorf("Active kept the wrong plans: %+v", got.SavingsPlans)
	}
	if len(inv.RIs) != 3 || len(inv.SavingsPlans) != 2 {
		t.Error("Active must not mutate its receiver")
	}
	// A nil inventory is a valid, commitment-free account.
	var nilInv *Inventory
	if a := nilInv.Active(now); a == nil || len(a.RIs) != 0 {
		t.Errorf("nil inventory Active = %+v", a)
	}
}

// TestRateTableAppliesAndDegrades covers the seam a future
// `kilter pricing sync-commitments` fills: rates arrive as a plain table, and
// anything the table lacks falls through to the conservative path.
func TestRateTableAppliesAndDegrades(t *testing.T) {
	known := UsageLine{ID: "a", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.xlarge",
		Platform: "Linux/UNIX", Tenancy: "default", Quantity: 1, ODRate: 0.192}
	unknown := UsageLine{ID: "b", Kind: KindEC2, Region: "us-east-1", InstanceType: "m7g.xlarge",
		Quantity: 1, ODRate: 0.1632}
	pinned := UsageLine{ID: "c", Kind: KindFargate, Region: "us-east-1", Unit: "vCPU-Hours",
		Quantity: 1, ODRate: 0.04, ComputeSPRate: 0.031}
	ineligible := UsageLine{ID: "d", Kind: KindLambda, Region: "us-east-1", Unit: "Requests-Millions",
		Quantity: 1, ODRate: 0.20, SPIneligible: true}

	var rt RateTable
	rt.Set(RateKey(known), SPRates{ComputeUSD: 0.122, EC2InstanceUSD: 0.110})
	rt.Set(RateKey(pinned), SPRates{ComputeUSD: 0.028})
	rt.Set(RateKey(ineligible), SPRates{ComputeUSD: 0.19})

	in := Usage{Lines: []UsageLine{known, unknown, pinned, ineligible}}
	out := rt.Apply(in)

	near(t, out.Lines[0].ComputeSPRate, 0.122, 1e-12, "known compute rate applied")
	near(t, out.Lines[0].EC2SPRate, 0.110, 1e-12, "known ec2 rate applied")
	near(t, out.Lines[1].ComputeSPRate, 0, 1e-12, "unknown usage type keeps a zero rate")
	near(t, out.Lines[2].ComputeSPRate, 0.031, 1e-12, "a rate pinned on the line wins")
	near(t, out.Lines[3].ComputeSPRate, 0, 1e-12, "ineligible usage is never given a rate")
	if in.Lines[0].ComputeSPRate != 0 {
		t.Error("Apply must not mutate its input")
	}

	// The whole table survives a write/read cycle, and a nil table is simply
	// an empty one — every lookup misses, every line degrades conservatively.
	var buf bytes.Buffer
	if err := WriteRateTable(&buf, &rt); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := LoadRateTable(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r, ok := back.Lookup(known); !ok || r.ComputeUSD != 0.122 {
		t.Errorf("round-trip lost the rate: %+v %v", r, ok)
	}
	var nilTable *RateTable
	if _, ok := nilTable.Lookup(known); ok {
		t.Error("nil rate table must miss")
	}
	if _, err := LoadRateTable(strings.NewReader(`{"rates":{"x":{"computeUSD":-1}}}`)); err == nil {
		t.Error("negative rate must be rejected")
	}
	if _, err := LoadRateTable(strings.NewReader(`{"nope":1}`)); err == nil {
		t.Error("unknown field must be rejected")
	}
}

// TestRateKeyDistinguishesWhatChangesTheRate.
func TestRateKeyDistinguishesWhatChangesTheRate(t *testing.T) {
	base := UsageLine{Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.xlarge",
		Platform: "Linux/UNIX", Tenancy: "default"}
	same := base
	same.ID, same.Quantity, same.ODRate, same.AZ = "other", 99, 1.5, "us-east-1z"
	if RateKey(base) != RateKey(same) {
		t.Error("id, quantity, rate and AZ must not change the rate key")
	}
	for _, differ := range []func(UsageLine) UsageLine{
		func(l UsageLine) UsageLine { l.Region = "eu-west-1"; return l },
		func(l UsageLine) UsageLine { l.InstanceType = "m5.large"; return l },
		func(l UsageLine) UsageLine { l.Platform = "Windows"; return l },
		func(l UsageLine) UsageLine { l.Tenancy = "dedicated"; return l },
		func(l UsageLine) UsageLine { l.Kind = KindFargate; return l },
	} {
		if RateKey(differ(base)) == RateKey(base) {
			t.Errorf("rate key must distinguish %+v", differ(base))
		}
	}
}

// TestCommitmentSourceSeamIsPureAndOffline documents, in code, that the sync
// interfaces are satisfiable without any cloud access — the offline decision
// path is the default, not the degraded mode.
func TestCommitmentSourceSeamIsPureAndOffline(t *testing.T) {
	var _ CommitmentSource = fileSource{}
	var _ RateSource = fileSource{}

	src := fileSource{inv: &Inventory{SavingsPlans: []SavingsPlan{
		{ID: "sp", Type: SPCompute, CommitmentUSDPerHour: 1},
	}}}
	inv, err := src.FetchCommitments(t.Context())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(inv.SavingsPlans) != 1 {
		t.Fatalf("inventory not returned: %+v", inv)
	}
	if _, err := src.FetchRates(t.Context(), "us-east-1"); err != nil {
		t.Fatalf("rates: %v", err)
	}
}

type fileSource struct{ inv *Inventory }

func (f fileSource) FetchCommitments(ctx context.Context) (*Inventory, error) {
	return f.inv, ctx.Err()
}
func (f fileSource) FetchRates(ctx context.Context, region string) (*RateTable, error) {
	return &RateTable{}, ctx.Err()
}
