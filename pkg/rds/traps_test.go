package rds

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// The trap tests. Each one is the falsifiable check
// docs/design/rds-batch-assessment.md §4 asks for, named after the trap it
// closes, and each fails on a plausible wrong implementation rather than on a
// typo.

// Trap 9. `FreeableMemory` is `MemAvailable`, and `MemAvailable` counts the
// page cache.
//
// The check the design doc specifies: "one fixture, two engines, identical
// FreeableMemory series, and the test asserts the two verdicts DIFFER". If
// this test ever passes with the two verdicts equal, the engine-keyed policy
// has been flattened back into a single memory rule and trap 9 is open again.
func TestFreeableMemoryIsNotHeadroom(t *testing.T) {
	// One number, used for both engines. 24 GiB free on a 32 GiB class.
	const freeMem = 24 << 30
	const freeStorage = 40 << 30

	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("pg", "db.r6i.xlarge", "postgres"),
			rec("my", "db.r6i.xlarge", "mysql"),
			rec("ora", "db.r6i.xlarge", "oracle-se2", withLicense(LicenseBYOL)),
		},
		Metrics: mergeMetrics(
			metricsFor("pg", 30, 12, freeMem, freeStorage),
			metricsFor("my", 30, 12, freeMem, freeStorage),
			metricsFor("ora", 30, 12, freeMem, freeStorage),
		),
	}
	snap := collect(t, f)
	rep := assess(t, snap, nil)

	pg, my, ora := must(t, rep, "pg"), must(t, rep, "my"), must(t, rep, "ora")

	// Same series, point for point. "Identical evidence" is asserted here
	// rather than assumed, so a fixture drift cannot make this test pass for
	// the wrong reason.
	pgSeries := memorySeries(t, snap, "pg")
	mySeries := memorySeries(t, snap, "my")
	if len(pgSeries.Points) == 0 || len(pgSeries.Points) != len(mySeries.Points) {
		t.Fatalf("the two engines were given different-length series (%d vs %d)",
			len(pgSeries.Points), len(mySeries.Points))
	}
	for i := range pgSeries.Points {
		if pgSeries.Points[i] != mySeries.Points[i] {
			t.Fatalf("datapoint %d differs between the two engines: %v vs %v",
				i, pgSeries.Points[i], mySeries.Points[i])
		}
	}

	// And different verdicts.
	if pg.Memory.Code == my.Memory.Code {
		t.Fatalf("PostgreSQL and MySQL reached the SAME verdict %q over an identical FreeableMemory "+
			"series; trap 9 is that MemAvailable means different things on the two engines",
			pg.Memory.Code)
	}
	wantCode(t, pg, ReasonFreeableMemoryIsPageCache)
	wantCode(t, my, ReasonBufferPoolScalesWithClass)

	// PostgreSQL's series is not converted into a headroom number AT ALL. A
	// populated field is an invitation to use it, and the whole point is that
	// this number does not mean what the field name would imply.
	if pg.Memory.Readable {
		t.Error("PostgreSQL's FreeableMemory was marked readable as headroom")
	}
	if pg.Memory.MinFreeBytes != 0 || pg.Memory.FreeFraction != 0 {
		t.Errorf("PostgreSQL's verdict carries headroom numbers (%v bytes, %v fraction); the refusal is "+
			"that the series is not headroom, so it must not be rendered as one",
			pg.Memory.MinFreeBytes, pg.Memory.FreeFraction)
	}

	// MySQL's IS readable — and still refuses the downsize, for the buffer-pool
	// reason rather than the page-cache one.
	if !my.Memory.Readable {
		t.Error("MySQL's FreeableMemory should be readable: InnoDB holds its buffer pool as anonymous " +
			"memory, which MemAvailable does not count as available")
	}
	if my.Memory.MinFreeBytes != freeMem {
		t.Errorf("MySQL low-water mark = %v, want %v", my.Memory.MinFreeBytes, freeMem)
	}
	if got := my.Memory.FreeFraction; math.Abs(got-0.75) > 1e-12 {
		t.Errorf("MySQL free fraction = %v, want 0.75 of a 32 GiB class", got)
	}

	// An engine whose semantics are not encoded REFUSES rather than guessing —
	// the t2-baseline precedent from pkg/ec2.
	wantCode(t, ora, ReasonMemorySemanticsUnencoded)
	if ora.Memory.Readable {
		t.Error("Oracle's FreeableMemory was read as headroom; its memory manager is not encoded here")
	}
	if ora.Memory.Code == pg.Memory.Code || ora.Memory.Code == my.Memory.Code {
		t.Errorf("the unencoded engine reused an encoded engine's verdict (%q)", ora.Memory.Code)
	}
}

// memorySeries returns the FreeableMemory series the collector delivered for
// one instance.
func memorySeries(t *testing.T, snap *Snapshot, id string) Series {
	t.Helper()
	for _, tgt := range snap.Targets {
		if tgt.Instance.Identifier != id {
			continue
		}
		s, ok := tgt.SeriesFor(MetricFreeableMemory)
		if !ok {
			t.Fatalf("%s: no %s series was delivered", id, MetricFreeableMemory)
		}
		return s
	}
	t.Fatalf("no target %q in the snapshot", id)
	return Series{}
}

// Trap 10. Multi-AZ bills twice, on the instance line and nowhere else.
//
// The design doc's check, verbatim: "a Multi-AZ instance's CurrentHourlyUSD is
// exactly twice the Single-AZ price of the same class, asserted to 1e-12, and
// a class change moves both halves."
func TestMultiAZBillsTwice(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("single", "db.r6i.xlarge", "postgres"),
			rec("multi", "db.r6i.xlarge", "postgres", withMultiAZ()),
			rec("single-big", "db.r6i.2xlarge", "postgres"),
			rec("multi-big", "db.r6i.2xlarge", "postgres", withMultiAZ()),
		},
		Metrics: mergeMetrics(
			metricsFor("single", 30, 12, 24<<30, 40<<30),
			metricsFor("multi", 30, 12, 24<<30, 40<<30),
			metricsFor("single-big", 30, 12, 24<<30, 40<<30),
			metricsFor("multi-big", 30, 12, 24<<30, 40<<30),
		),
	}
	rep := assess(t, collect(t, f), nil)

	for _, tc := range []struct{ single, multi string }{
		{"single", "multi"}, {"single-big", "multi-big"},
	} {
		s, m := must(t, rep, tc.single), must(t, rep, tc.multi)
		if !s.CostKnown || !m.CostKnown {
			t.Fatalf("%s/%s: both instances must be priced", tc.single, tc.multi)
		}
		if got, want := m.CurrentHourlyUSD, s.CurrentHourlyUSD*2; math.Abs(got-want) > 1e-12 {
			t.Errorf("%s hourly = %v, want exactly 2× the Single-AZ %v (Δ %v)",
				tc.multi, got, want, got-want)
		}
		if got, want := m.CurrentMonthlyUSD, s.CurrentMonthlyUSD*2; math.Abs(got-want) > 1e-12 {
			t.Errorf("%s monthly = %v, want exactly 2× %v", tc.multi, got, want)
		}
		if m.Deployment != commit.RDSMultiAZInstance {
			t.Errorf("%s deployment = %q, want %q", tc.multi, m.Deployment, commit.RDSMultiAZInstance)
		}
	}

	// A class change moves both halves: the ratio survives it.
	small, big := must(t, rep, "multi"), must(t, rep, "multi-big")
	smallS, bigS := must(t, rep, "single"), must(t, rep, "single-big")
	ratio := func(a, b float64) float64 { return a / b }
	if got, want := ratio(big.CurrentHourlyUSD, small.CurrentHourlyUSD),
		ratio(bigS.CurrentHourlyUSD, smallS.CurrentHourlyUSD); math.Abs(got-want) > 1e-12 {
		t.Errorf("the class ratio differs between Single-AZ (%v) and Multi-AZ (%v); the multiplier is not "+
			"a factor in the price function", want, got)
	}

	// The multiplier applies to the INSTANCE line only. Storage, backups and
	// I/O are billed on their own lines — the same separation AWS states for
	// the reservation discount.
	if small.Storage.AllocatedMonthlyUSD != smallS.Storage.AllocatedMonthlyUSD {
		t.Errorf("Multi-AZ moved the storage line (%v vs %v); trap 10's multiplier is on instance hours "+
			"only", small.Storage.AllocatedMonthlyUSD, smallS.Storage.AllocatedMonthlyUSD)
	}
	ad := wantAdvisory(t, small, AdvisoryMultiAZMultiplier)
	if !strings.Contains(ad.Caveat, "instance hours only") {
		t.Errorf("the Multi-AZ advisory does not say which line the multiplier is on: %q", ad.Caveat)
	}
	wantCode(t, small, ReasonMultiAZIsAvailabilityPosture)
}

// Trap 10, at the rate-card level: the ×1/×2/×3 factors come from the same
// table the reservation arithmetic uses, so cost and coverage can never
// disagree about what a topology means.
func TestDeploymentMultiplierMatchesTheReservationTable(t *testing.T) {
	card := DefaultRates()
	e := ParseEngine("postgres", LicenseGPL)
	const class = "db.r6i.xlarge"

	base, _, ok := card.HourlyUSD(class, e, commit.RDSSingleAZ)
	if !ok {
		t.Fatalf("%s is not priced", class)
	}
	for _, tc := range []struct {
		dep  commit.RDSDeployment
		want float64
	}{
		{commit.RDSSingleAZ, 1},
		{commit.RDSMultiAZInstance, 2},
		{commit.RDSMultiAZCluster, 3},
	} {
		got, _, ok := card.HourlyUSD(class, e, tc.dep)
		if !ok {
			t.Fatalf("%s under %q is not priced", class, tc.dep)
		}
		if math.Abs(got-base*tc.want) > 1e-12 {
			t.Errorf("%q priced at %v, want %v (×%v of the Single-AZ rate)", tc.dep, got, base*tc.want, tc.want)
		}
		// And the normalized units move by the same factor, which is the half
		// of trap 10 that decides reservation coverage.
		u, uok := commit.RDSClassUnits(class, tc.dep)
		su, sok := commit.RDSClassUnits(class, commit.RDSSingleAZ)
		if !uok || !sok || math.Abs(u-su*tc.want) > 1e-12 {
			t.Errorf("%q normalized units = %v, want %v", tc.dep, u, su*tc.want)
		}
	}

	// An unknown topology is refused, never defaulted to Single-AZ: that would
	// halve the bill in exactly the direction this package exists to prevent.
	if _, _, ok := card.HourlyUSD(class, e, commit.RDSDeployment("who-knows")); ok {
		t.Error("an unknown deployment topology was priced; it must refuse rather than assume Single-AZ")
	}
}

// Trap 8. Storage cannot shrink, so no proposal may ever say it does.
func TestStorageFloorNeverProposesShrink(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			// 4 TiB allocated, ~300 GiB used: the design doc's own example.
			rec("fat", "db.r6i.xlarge", "postgres", withStorage(4096, 0, StorageGP2)),
		},
		Metrics: metricsFor("fat", 20, 30, 24<<30, 3796*GiB),
	}
	rep := assess(t, collect(t, f), nil)
	a := must(t, rep, "fat")

	wantCode(t, a, ReasonStorageCannotShrink)
	if a.Proposal != nil {
		t.Fatal("an instance with 92 % unused storage carries a proposal; the API that would realize it " +
			"does not exist")
	}
	if !a.Storage.FillKnown {
		t.Fatal("FreeStorageSpace was delivered but the fill level was not read")
	}
	if got := a.Storage.UnusedFraction; got < 0.9 || got > 0.95 {
		t.Errorf("unused fraction = %v, want ≈0.926 (3796 of 4096 GiB)", got)
	}
	ad := wantAdvisory(t, a, AdvisoryStorageFloor)
	if ad.MonthlyUSD <= 0 {
		t.Error("the storage floor advisory carries no dollar figure; trap 8 asks for a reported fact " +
			"with a dollar attached")
	}
	for _, want := range []string{"blue/green", "migration", "never read as headroom"} {
		if !strings.Contains(ad.Caveat, want) {
			t.Errorf("the storage floor caveat omits %q: %s", want, ad.Caveat)
		}
	}

	// The gate itself: a proposal below the observed allocation is rejected by
	// Validate, whatever produced it.
	bad := *rep
	bad.Assessments = append([]Assessment(nil), rep.Assessments...)
	bad.Assessments[0].Proposal = &Proposal{
		AllocatedStorageGiB: 512, Action: "advisory", Risk: RiskLow, Confidence: 0.9,
		Reason: "shrink it", NetSavingsMonthlyUSD: 10, GrossSavingsMonthlyUSD: 10,
		RateProvenance: RateOperator,
	}
	err := bad.Validate()
	if err == nil {
		t.Fatal("Validate accepted a proposal that shrinks allocated storage")
	}
	if !strings.Contains(err.Error(), "can only ever grow") {
		t.Errorf("Validate rejected the shrink for the wrong reason: %v", err)
	}
}

// Trap 8's second half: an autoscaling ceiling is a ratchet, not headroom.
func TestAutoscaledStorageIsAFloorNotAChoice(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("auto", "db.m6i.large", "postgres", withStorage(200, 1000, StorageGP2)),
			rec("fixed", "db.m6i.large", "postgres", withStorage(200, 0, StorageGP2)),
		},
		Metrics: mergeMetrics(
			metricsFor("auto", 20, 30, 6<<30, 150*GiB),
			metricsFor("fixed", 20, 30, 6<<30, 150*GiB),
		),
	}
	rep := assess(t, collect(t, f), nil)
	auto, fixed := must(t, rep, "auto"), must(t, rep, "fixed")

	if !auto.Storage.AutoscalingEnabled {
		t.Fatal("MaxAllocatedStorage > AllocatedStorage was not read as autoscaling")
	}
	if fixed.Storage.AutoscalingEnabled {
		t.Fatal("an instance with no MaxAllocatedStorage was reported as autoscaling")
	}
	wantCode(t, auto, ReasonStorageAutoscalingRatchet)
	wantNoCode(t, fixed, ReasonStorageAutoscalingRatchet)

	// The documented trigger is QUOTED as evidence, not paraphrased: that is
	// what lets a reader tell an autoscaling event from a Kilter change.
	ad := wantAdvisory(t, auto, AdvisoryStorageAutoscaling)
	for _, want := range []string{"10.0%", "5m0s", "fewer than 4", "10 GiB", "7h0m0s", "not logged by CloudTrail"} {
		if !strings.Contains(ad.Caveat, want) {
			t.Errorf("the autoscaling caveat omits the documented trigger clause %q:\n%s", want, ad.Caveat)
		}
	}
	var sawEvidence bool
	for _, ev := range auto.Evidence {
		if ev.Metric == "storage-autoscaling-trigger" {
			sawEvidence = true
		}
	}
	if !sawEvidence {
		t.Error("the autoscaling trigger is not carried as evidence")
	}

	// And FreeStorageSpace headroom is never a saving on either instance.
	for _, a := range []Assessment{auto, fixed} {
		if a.Proposal != nil {
			t.Errorf("%s: free storage was turned into a proposal", a.Instance.Identifier)
		}
		if rep.Totals.GrossSavingsMonthlyUSD != 0 || rep.Totals.NetSavingsMonthlyUSD != 0 {
			t.Fatalf("free storage reached the savings totals (gross %v, net %v)",
				rep.Totals.GrossSavingsMonthlyUSD, rep.Totals.NetSavingsMonthlyUSD)
		}
	}
}

// Trap 8's ledger rule: an allocated-storage increase between two snapshots is
// unattributed, because autoscaling leaves no CloudTrail event.
func TestStorageGrowthBetweenSnapshotsIsUnattributed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prior, cur int64
		want       StorageAttribution
	}{
		{"first sight", 0, 200, StorageUnchanged},
		{"unchanged", 200, 200, StorageUnchanged},
		{"grew", 200, 220, StorageGrewUnattributed},
		{"shrank", 220, 200, StorageShrankImpossible},
	} {
		got, why := AttributeStorageGrowth(tc.prior, tc.cur)
		if got != tc.want {
			t.Errorf("%s: attribution = %q, want %q", tc.name, got, tc.want)
		}
		if got != StorageUnchanged && why == "" {
			t.Errorf("%s: attribution %q carries no explanation", tc.name, got)
		}
	}
	// The direction of the defaults is the point.
	_, why := AttributeStorageGrowth(200, 220)
	for _, want := range []string{"not Kilter", "not logged by CloudTrail", "unattributed"} {
		if !strings.Contains(why, want) {
			t.Errorf("the growth explanation omits %q: %s", want, why)
		}
	}
	_, why = AttributeStorageGrowth(220, 200)
	if !strings.Contains(why, "no saving is inferred") {
		t.Errorf("an impossible shrink must not be absorbed as a saving: %s", why)
	}
}

// Trap 16. Aurora is refused by name, and a non-Aurora cluster member is
// refused under a DIFFERENT name — because calling a PostgreSQL Multi-AZ
// cluster "Aurora" would be a false statement in a report whose whole value is
// that its statements are true.
func TestAuroraIsRefusedByName(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("aur", "db.r6g.large", "aurora-postgresql", withCluster("aur-cluster")),
			rec("aurless", "db.serverless", "aurora-mysql", withCluster("sless-cluster")),
			rec("mazc", "db.r6i.xlarge", "mysql", withCluster("mazc-cluster")),
			rec("plain", "db.r6i.xlarge", "postgres"),
		},
		Clusters: []DBClusterRecord{
			{DBClusterIdentifier: "aur-cluster", Engine: "aurora-postgresql", EngineMode: "provisioned"},
			{DBClusterIdentifier: "sless-cluster", Engine: "aurora-mysql", EngineMode: "provisioned",
				ServerlessV2MinCapacity: 2, ServerlessV2MaxCapacity: 64},
			{DBClusterIdentifier: "mazc-cluster", Engine: "mysql", EngineMode: "provisioned"},
		},
		Metrics: mergeMetrics(
			metricsFor("aur", 30, 12, 8<<30, 40*GiB),
			metricsFor("aurless", 30, 12, 8<<30, 40*GiB),
			metricsFor("mazc", 30, 12, 8<<30, 40*GiB),
			metricsFor("plain", 30, 12, 24<<30, 40*GiB),
		),
	}
	rep := assess(t, collect(t, f), nil)

	for _, id := range []string{"aur", "aurless"} {
		a := must(t, rep, id)
		wantCode(t, a, ReasonAuroraNotSupported)
		if len(a.Suppressions) != 1 {
			t.Errorf("%s: an exclusion must fire alone, got %v", id, a.Codes())
		}
		if len(a.Advisories) != 0 || a.Proposal != nil {
			t.Errorf("%s: an Aurora instance produced findings in RDS's vocabulary", id)
		}
		if !strings.Contains(a.Suppressions[0].Reason, "ACU") {
			t.Errorf("%s: the Aurora refusal does not name why the RDS model does not apply: %s",
				id, a.Suppressions[0].Reason)
		}
	}
	// The serverless cluster's min-ACU floor is named, because that is the
	// lever an Aurora unit would actually start from.
	if !strings.Contains(must(t, rep, "aurless").Suppressions[0].Reason, "min-ACU floor is 2.0") {
		t.Error("the Aurora refusal does not name the cluster's min-ACU floor")
	}

	// A Multi-AZ DB cluster member: excluded, and NOT called Aurora.
	mazc := must(t, rep, "mazc")
	wantCode(t, mazc, ReasonClusterMemberNotSupported)
	wantNoCode(t, mazc, ReasonAuroraNotSupported)

	// A plain instance in the same account is unaffected.
	plain := must(t, rep, "plain")
	if plain.Excluded() {
		t.Error("a standalone DB instance was excluded alongside the Aurora ones")
	}
}

// Trap 13. Reserved DB Instances are not Reserved Instances: size flexibility
// is scoped to the instance CLASS TYPE and is absent for SQL Server and Oracle
// License Included. This runs end to end through the U12 ledger.
func TestSQLServerAndOracleLIAreExactMatchOnly(t *testing.T) {
	// Operator rates, so the arithmetic is claimable and the refusal under
	// test is the size-flexibility one rather than the unverified-rate one.
	rates := operatorRates(t, map[string]float64{
		"open-source|db.r6i.xlarge":     0.48,
		"open-source|db.r6i.large":      0.24,
		"sqlserver-ee-li|db.r6i.xlarge": 3.20,
		"sqlserver-ee-li|db.r6i.large":  1.60,
		"oracle-ee-li|db.r6i.xlarge":    2.10,
		"oracle-ee-li|db.r6i.large":     1.05,
		"oracle-ee-byol|db.r6i.xlarge":  0.90,
		"oracle-ee-byol|db.r6i.large":   0.45,
	})

	cases := []struct {
		id, engine, license string
		wantFlexible        bool
	}{
		{"pg", "postgres", LicenseGPL, true},
		{"mssql", "sqlserver-ee", LicenseIncluded, false},
		{"orali", "oracle-ee", LicenseIncluded, false},
		{"orabyol", "oracle-ee", LicenseBYOL, true},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			inst := rec(tc.id, "db.r6i.xlarge", tc.engine, withLicense(tc.license))
			f := &Fixture{
				Instances: []DBInstanceRecord{inst},
				Metrics:   metricsFor(tc.id, 30, 12, 24<<30, 40*GiB),
				Reservations: []ReservedDBInstanceRecord{{
					ReservedDBInstanceId: "res-" + tc.id,
					DBInstanceClass:      "db.r6i.xlarge",
					DBInstanceCount:      1,
					ProductDescription:   engineProductDescription(tc.engine, tc.license),
					State:                "active",
					UsagePrice:           0.30,
					Duration:             365 * 24 * 3600,
					StartTime:            testStart.Add(-30 * 24 * time.Hour),
				}},
			}
			snap := collect(t, f)
			if len(snap.Reservations) != 1 {
				t.Fatalf("the reservation did not survive collection: %+v", snap.Reservations)
			}
			e := ParseEngine(tc.engine, tc.license)
			before := UsageLines(snap.Targets[0].Ref, snap.Targets[0].Instance, e, commit.RDSSingleAZ,
				mustRate(t, rates, "db.r6i.xlarge", e))
			if err := (commit.Usage{Lines: before}).Validate(); err != nil {
				t.Fatalf("the usage line U11 builds is not one U12 accepts: %v", err)
			}
			led := ledgerWith(snap.Reservations, before...)

			rep := assess(t, snap, led, func(c *Config) { c.Rates = rates })
			a := must(t, rep, tc.id)

			if a.Commitment.SizeFlexible != tc.wantFlexible {
				t.Fatalf("engine %q licence %q: size-flexible = %v, want %v",
					tc.engine, tc.license, a.Commitment.SizeFlexible, tc.wantFlexible)
			}
			if !a.Commitment.Assessed {
				t.Fatal("the ledger was supplied but the commitment verdict says it was not assessed")
			}
			if a.Commitment.CandidateClass != "db.r6i.large" {
				t.Fatalf("candidate class = %q, want the next smaller size in the SAME class type",
					a.Commitment.CandidateClass)
			}
			if a.Commitment.GrossMonthlyUSD <= 0 {
				t.Fatalf("gross = %v; the list price of a one-size-down move is positive",
					a.Commitment.GrossMonthlyUSD)
			}

			if tc.wantFlexible {
				// The reservation floats onto the smaller size, so the bill
				// barely moves and the domain says so rather than claiming the
				// list-price delta.
				if a.Commitment.NetMonthlyUSD > a.Commitment.GrossMonthlyUSD+1e-9 {
					t.Errorf("net %v exceeds gross %v", a.Commitment.NetMonthlyUSD, a.Commitment.GrossMonthlyUSD)
				}
				wantNoCode(t, a, ReasonSizeFlexibilityExcluded)
				return
			}

			// Not size-flexible: the reservation cannot follow the downsize, so
			// it strands entirely.
			wantCode(t, a, ReasonSizeFlexibilityExcluded)
			if a.Commitment.StrandedMonthlyUSD <= 0 {
				t.Errorf("a non-size-flexible engine stranded nothing (%v); \"Size flexibility does not "+
					"apply to RDS for SQL Server and RDS for Oracle License Included\"",
					a.Commitment.StrandedMonthlyUSD)
			}
			ad := wantAdvisory(t, a, AdvisoryReservationStranding)
			if !strings.Contains(ad.Caveat, "can't cancel") {
				t.Errorf("the stranding advisory does not say the stranding is permanent: %s", ad.Caveat)
			}
			// And still no proposal: the move is a failover either way.
			if a.Proposal != nil {
				t.Error("the commitment arithmetic produced a proposal; the class change is a failover")
			}
		})
	}
}

// engineProductDescription renders the spelling DescribeReservedDBInstances
// uses for a reservation's engine.
func engineProductDescription(engine, license string) string {
	e := ParseEngine(engine, license)
	return e.CommitEngine()
}

func mustRate(t *testing.T, card RateCard, class string, e Engine) float64 {
	t.Helper()
	r, _, ok := card.HourlyUSD(class, e, commit.RDSSingleAZ)
	if !ok {
		t.Fatalf("%s under %s is not priced by the test card", class, e.String())
	}
	return r
}

// Trap 12's U11-shaped structural fix. U14 asserts by reflection that the
// MUTATE input cannot name a class; U11 has no mutate input at all, so the
// equivalent assertion is that the PROPOSAL cannot name one either.
func TestProposalCannotNameAnInstanceClass(t *testing.T) {
	banned := []string{"class", "multiaz", "engineversion", "delete", "az", "availabilityzone"}
	for _, name := range structFieldNames(t, Proposal{}) {
		lower := strings.ToLower(name)
		for _, b := range banned {
			if strings.Contains(lower, b) {
				t.Errorf("rds.Proposal has a field %q. A DB instance class change is a failover that "+
					"rewrites a DNS record and drops every pooled connection, and no domain.ActionClass "+
					"describes that honestly — so it must be UNREPRESENTABLE here, not merely forbidden "+
					"(the pkg/ecs desired-count precedent)", name)
			}
		}
	}
}

// The same discipline on the storage verdict: it names a COST, never a saving,
// because the API that would realize one does not exist.
func TestStorageVerdictNamesNoSaving(t *testing.T) {
	for _, name := range structFieldNames(t, StorageVerdict{}) {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "saving") || strings.Contains(lower, "reclaim") ||
			strings.Contains(lower, "shrink") {
			t.Errorf("rds.StorageVerdict has a field %q; allocated storage cannot be reduced by any RDS "+
				"API, so a field named after a saving would be a promise this package cannot keep", name)
		}
	}
}
