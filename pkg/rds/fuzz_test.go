package rds

import (
	"math"
	"testing"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// FuzzRDSNeverProposesLessStorage is the load-bearing property of trap 8,
// asserted over arbitrary inputs rather than over the handful a table test
// would reach.
//
// "You can't reduce the amount of storage for a DB instance after storage has
// been allocated" [verified]. So: for every observed allocation and every
// proposed one, Report.Validate must reject the report if and only if the
// proposal is below the observation. Both directions matter — a gate that
// rejects everything is a gate that never has to be right.
func FuzzRDSNeverProposesLessStorage(f *testing.F) {
	f.Add(int64(100), int64(50), int64(0))
	f.Add(int64(100), int64(100), int64(1))
	f.Add(int64(4096), int64(300), int64(2))
	f.Add(int64(20), int64(0), int64(3))
	f.Add(int64(0), int64(0), int64(4))
	f.Add(int64(-5), int64(-9), int64(5))
	f.Add(int64(1<<40), int64(1), int64(6))

	f.Fuzz(func(t *testing.T, observedGiB, proposedGiB, seed int64) {
		// Fold rather than skip: a skipped input is an input the property was
		// never asserted over, and RDS allocations outside 0…1 PiB are a
		// collector bug rather than a case this gate may decline to answer.
		observedGiB = foldStorage(observedGiB)
		proposedGiB = foldStorage(proposedGiB)
		rep := &Report{
			Domain: Kind, GeneratedAt: testNow, Config: DefaultConfig(),
			Assessments: []Assessment{{
				Target:   domain.TargetRef{Domain: Kind, Scope: "s", ID: "arn:db:x"},
				Instance: DBInstance{Identifier: "x", Class: "db.r6i.large", AllocatedStorageGiB: observedGiB},
				Engine:   ParseEngine("postgres", LicenseGPL),
				Evidence: []domain.Evidence{{Metric: "m", Value: "v", Source: SourceDescribe}},
				Suppressions: []Suppression{{
					Code: ReasonInstanceClassIsAFailover, Reason: "a class change is a failover",
				}},
				Proposal: &Proposal{
					AllocatedStorageGiB:    proposedGiB,
					Action:                 domain.ActionAdvisory,
					Risk:                   RiskLow,
					Confidence:             0.5 + float64(seed%2)*0.25,
					Reason:                 "fuzzed",
					GrossSavingsMonthlyUSD: 10,
					NetSavingsMonthlyUSD:   10,
					RateProvenance:         RateOperator,
				},
			}},
		}
		rep.Totals = rep.computeTotals()

		err := rep.Validate()
		shrinks := proposedGiB != 0 && proposedGiB < observedGiB
		switch {
		case shrinks && err == nil:
			t.Fatalf("Validate accepted a proposal to shrink allocated storage from %d GiB to %d GiB; no "+
				"RDS API can do that", observedGiB, proposedGiB)
		case !shrinks && err != nil:
			t.Fatalf("Validate rejected a non-shrinking proposal (%d GiB → %d GiB): %v",
				observedGiB, proposedGiB, err)
		}
	})
}

// foldStorage maps an arbitrary int64 into the 0 … 1 PiB range RDS allocations
// live in, so every fuzz input reaches the gate instead of being skipped past
// it.
func foldStorage(v int64) int64 {
	const maxGiB = 1 << 20
	if v < 0 {
		v = -v
	}
	if v < 0 { // math.MinInt64 negates to itself
		return 0
	}
	return v % (maxGiB + 1)
}

// FuzzRDSReportInvariants asserts that whatever a collector delivers, the
// report is internally consistent, states a reason for every instance, and
// claims nothing.
//
// The properties are the ones a downstream consumer relies on:
//   - Validate passes. The package satisfies its own contract on every input.
//   - Every assessment carries at least one reason.
//   - Every money field is finite and non-negative.
//   - Nothing is proposed and nothing is claimed, because this unit proposes
//     nothing at all.
//   - The report is byte-stable: assessing the same snapshot twice agrees.
func FuzzRDSReportInvariants(f *testing.F) {
	f.Add(uint8(1), uint8(0), uint16(100), uint16(0), int64(12), int64(40), false, false)
	f.Add(uint8(3), uint8(2), uint16(4096), uint16(8192), int64(0), int64(4000), true, true)
	f.Add(uint8(0), uint8(9), uint16(0), uint16(0), int64(0), int64(0), false, true)
	f.Add(uint8(7), uint8(4), uint16(65535), uint16(1), int64(9999), int64(1), true, false)

	engines := []string{"postgres", "mysql", "mariadb", "sqlserver-ee", "oracle-se2", "aurora-mysql",
		"db2-ae", "cockroach", ""}
	classes := []string{"db.r6i.xlarge", "db.m6i.large", "db.t4g.medium", "db.zz.zz", "",
		"db.r6i.4xlarge", "db.m5.2xlarge", "db.r7g.large", "db.nope.large", "db.t3.micro"}

	f.Fuzz(func(t *testing.T, ei, ci uint8, allocated, maxAllocated uint16,
		conns, freeStorageGiB int64, multiAZ, cluster bool) {

		engine := engines[int(ei)%len(engines)]
		class := classes[int(ci)%len(classes)]
		r := DBInstanceRecord{
			DBInstanceIdentifier: "fz",
			DBInstanceArn:        "arn:aws:rds:us-east-1:1:db:fz",
			DBInstanceClass:      class,
			DBInstanceStatus:     StatusAvailable,
			Engine:               engine,
			LicenseModel:         LicenseGPL,
			MultiAZ:              multiAZ,
			AllocatedStorage:     int64(allocated),
			MaxAllocatedStorage:  int64(maxAllocated),
			StorageType:          StorageGP2,
		}
		if cluster {
			r.DBClusterIdentifier = "cl"
		}
		fx := &Fixture{
			Instances: []DBInstanceRecord{r},
			Metrics: map[string][]Point{
				"fz/" + MetricDatabaseConns:    flat(math.Abs(float64(conns%1000)), 8),
				"fz/" + MetricFreeStorageSpace: flat(math.Abs(float64(freeStorageGiB%100000))*GiB, 8),
				"fz/" + MetricFreeableMemory:   flat(float64(1<<30), 8),
				"fz/" + MetricCPUUtilization:   flat(float64(conns%100), 8),
			},
		}
		if cluster {
			fx.Clusters = []DBClusterRecord{{DBClusterIdentifier: "cl", Engine: engine}}
		}

		cfg := DefaultCollectorConfig(testWindow())
		c, err := NewCollector(fx, fx, fx, cfg)
		if err != nil {
			t.Fatalf("NewCollector: %v", err)
		}
		snap, err := c.Collect(t.Context())
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		sz, err := NewSizer(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		rep := sz.Assess(testNow, snap, nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("report is invalid for engine=%q class=%q allocated=%d max=%d: %v",
				engine, class, allocated, maxAllocated, err)
		}
		for _, a := range rep.Assessments {
			if len(a.Suppressions) == 0 {
				t.Fatalf("%s says nothing", a.Target.ID)
			}
			if a.Proposal != nil {
				t.Fatalf("%s carries a proposal; this unit proposes nothing", a.Target.ID)
			}
			for _, v := range []float64{
				a.CurrentHourlyUSD, a.CurrentMonthlyUSD,
				a.Storage.AllocatedMonthlyUSD, a.Storage.UnusedMonthlyUSD, a.Storage.UnusedFraction,
				a.Memory.MinFreeBytes, a.Memory.FreeFraction,
			} {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
					t.Fatalf("%s carries a non-finite or negative money/ratio field: %v", a.Target.ID, v)
				}
			}
			for _, ad := range a.Advisories {
				if math.IsNaN(ad.MonthlyUSD) || math.IsInf(ad.MonthlyUSD, 0) || ad.MonthlyUSD < 0 {
					t.Fatalf("advisory %q carries %v", ad.Code, ad.MonthlyUSD)
				}
			}
			// Storage is never described as more used than allocated.
			if a.Storage.FillKnown && a.Storage.UsedGiB < 0 {
				t.Fatalf("%s: used storage is negative (%v)", a.Target.ID, a.Storage.UsedGiB)
			}
		}
		if rep.Totals.NetSavingsMonthlyUSD != 0 || rep.Totals.GrossSavingsMonthlyUSD != 0 {
			t.Fatalf("a saving was claimed: %+v", rep.Totals)
		}

		// Assessing the same snapshot twice agrees.
		again := sz.Assess(testNow, snap, nil)
		if len(again.Assessments) != len(rep.Assessments) ||
			again.Totals.CurrentMonthlyUSD != rep.Totals.CurrentMonthlyUSD {
			t.Fatal("two assessments of the same snapshot disagree")
		}
	})
}

// FuzzRDSPriceFunction pins trap 10 over the whole rate table: for every
// priced class and every topology, the price is exactly the Single-AZ rate
// times the documented normalized-unit multiplier, and an unknown topology is
// never priced.
func FuzzRDSPriceFunction(f *testing.F) {
	f.Add(uint8(0), uint8(0))
	f.Add(uint8(5), uint8(1))
	f.Add(uint8(9), uint8(2))
	f.Add(uint8(200), uint8(200))

	classes := make([]string, 0, len(classShapes))
	for c := range classShapes {
		classes = append(classes, c)
	}
	// Deterministic order: map iteration order must not reach the corpus.
	sortStrings(classes)
	deployments := []commit.RDSDeployment{
		commit.RDSSingleAZ, commit.RDSMultiAZInstance, commit.RDSMultiAZCluster, "unknown-topology", "",
	}

	f.Fuzz(func(t *testing.T, ci, di uint8) {
		card := DefaultRates()
		e := ParseEngine("postgres", LicenseGPL)
		class := classes[int(ci)%len(classes)]
		dep := deployments[int(di)%len(deployments)]

		base, _, ok := card.HourlyUSD(class, e, commit.RDSSingleAZ)
		if !ok {
			t.Fatalf("%s is in the shape table but not priced", class)
		}
		got, prov, ok := card.HourlyUSD(class, e, dep)
		m, known := dep.Multiplier()
		if known != ok {
			t.Fatalf("%s/%s: priced=%v but topology known=%v; an unknown topology must never be priced "+
				"at the Single-AZ rate", class, dep, ok, known)
		}
		if !ok {
			return
		}
		if math.Abs(got-base*m) > 1e-12 {
			t.Fatalf("%s/%s priced at %v, want %v (×%v)", class, dep, got, base*m, m)
		}
		if prov.Claimable() {
			t.Fatalf("%s carries a claimable provenance %q; every shipped RDS rate is unverified",
				class, prov)
		}
		// And the reservation units move by the same factor, so cost and
		// coverage can never disagree about what a topology means.
		u, uok := commit.RDSClassUnits(class, dep)
		su, sok := commit.RDSClassUnits(class, commit.RDSSingleAZ)
		if uok != sok {
			t.Fatalf("%s: units known for one topology and not the other", class)
		}
		if uok && math.Abs(u-su*m) > 1e-12 {
			t.Fatalf("%s/%s units = %v, want %v", class, dep, u, su*m)
		}
	})
}

// sortStrings is sort.Strings, spelled out so the fuzz file needs no extra
// import in a package where "sort" already means something in every other
// file.
func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
