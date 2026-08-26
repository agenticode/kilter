package commit

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func rdsLine(id, class string, d RDSDeployment, engine string, qty, od float64) UsageLine {
	return UsageLine{
		ID: id, Kind: KindRDS, Region: "us-east-1", InstanceType: class,
		Engine: engine, Deployment: d, Unit: "Instance-Hours",
		Quantity: qty, ODRate: od,
	}
}

func rdb(id, class string, d RDSDeployment, engine string, count int, rate float64) ReservedDBInstance {
	return ReservedDBInstance{
		ID: id, Count: count, DBInstanceClass: class, Region: "us-east-1",
		Engine: engine, Deployment: d, EffectiveHourlyUSD: rate,
	}
}

func rdsAt(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// ---------------------------------------------------------------------------
// the table
// ---------------------------------------------------------------------------

// TestRDSNormalizationTableMatchesPublishedUnits transcribes the Reserved DB
// Instance normalized-unit table a second time, by hand, from
// USER_WorkingWithReservedDBInstances.html — all fourteen sizes across all
// three deployment columns. It deliberately does not read rdsSizeUnits: a test
// that checks a constant against itself proves nothing, and one wrong row here
// silently mis-prices every RDS recommendation in that class type.
func TestRDSNormalizationTableMatchesPublishedUnits(t *testing.T) {
	published := []struct {
		size                                   string
		singleAZ, multiAZInstance, multiAZClus float64
	}{
		{"micro", 0.5, 1, 1.5},
		{"small", 1, 2, 3},
		{"medium", 2, 4, 6},
		{"large", 4, 8, 12},
		{"xlarge", 8, 16, 24},
		{"2xlarge", 16, 32, 48},
		{"4xlarge", 32, 64, 96},
		{"6xlarge", 48, 96, 144},
		{"8xlarge", 64, 128, 192},
		{"10xlarge", 80, 160, 240},
		{"12xlarge", 96, 192, 288},
		{"16xlarge", 128, 256, 384},
		{"24xlarge", 192, 384, 576},
		{"32xlarge", 256, 512, 768},
	}
	if len(published) != 14 {
		t.Fatalf("the transcription itself has %d rows, want 14", len(published))
	}
	if len(rdsSizeUnits) != len(published) {
		t.Fatalf("rdsSizeUnits has %d rows, the published table has %d",
			len(rdsSizeUnits), len(published))
	}
	for _, want := range published {
		for _, col := range []struct {
			deployment RDSDeployment
			units      float64
		}{
			{RDSSingleAZ, want.singleAZ},
			{RDSMultiAZInstance, want.multiAZInstance},
			{RDSMultiAZCluster, want.multiAZClus},
		} {
			got, ok := RDSNormalizationUnits(want.size, col.deployment)
			if !ok {
				t.Errorf("RDSNormalizationUnits(%q, %q): not found", want.size, col.deployment)
				continue
			}
			near(t, got, col.units, 1e-12,
				"RDSNormalizationUnits("+want.size+", "+string(col.deployment)+")")
			// Case and whitespace must not change a billing decision.
			if pad, ok := RDSNormalizationUnits(" "+strings.ToUpper(want.size)+" ", col.deployment); !ok || pad != got {
				t.Errorf("RDSNormalizationUnits(%q) is not case/whitespace-insensitive", want.size)
			}
			// And the same number through the instance-class door.
			cls, ok := RDSClassUnits("db.r6i."+want.size, col.deployment)
			if !ok || cls != got {
				t.Errorf("RDSClassUnits(db.r6i.%s, %s) = %v/%v, want %v",
					want.size, col.deployment, cls, ok, got)
			}
		}
	}

	// The RDS table is NOT the EC2 table. RDS publishes no `nano` row — there
	// is no db.*.nano class — while apply_ri.html does. Reusing
	// NormalizationUnits here would have quietly imported it.
	if u, ok := RDSNormalizationUnits("nano", RDSSingleAZ); ok {
		t.Errorf("RDSNormalizationUnits(nano) = %v; RDS publishes no nano row", u)
	}
	if _, ok := NormalizationUnits("nano"); !ok {
		t.Error("the EC2 table lost its nano row; this test's premise is stale")
	}
}

func TestRDSDeploymentMultiplierAndNormalization(t *testing.T) {
	for _, tc := range []struct {
		in   RDSDeployment
		want RDSDeployment
		mult float64
	}{
		{"single-az", RDSSingleAZ, 1},
		{"SINGLE-AZ", RDSSingleAZ, 1},
		{" Single_AZ ", RDSSingleAZ, 1},
		{"multi-az", RDSMultiAZInstance, 2},
		{"multi-az-instance", RDSMultiAZInstance, 2},
		{"Multi AZ", RDSMultiAZInstance, 2},
		{"multi-az-cluster", RDSMultiAZCluster, 3},
		{"Multi-AZ DB cluster", RDSMultiAZCluster, 3},
	} {
		if got := NormalizeRDSDeployment(tc.in); got != tc.want {
			t.Errorf("NormalizeRDSDeployment(%q) = %q, want %q", tc.in, got, tc.want)
		}
		m, ok := tc.in.Multiplier()
		if !ok {
			t.Errorf("%q: no multiplier", tc.in)
			continue
		}
		near(t, m, tc.mult, 1e-12, "multiplier of "+string(tc.in))
	}
	// The zero value is NOT Single-AZ: guessing it would halve the units a
	// line needs and make a reservation appear to cover twice the usage.
	for _, unknown := range []RDSDeployment{"", "standby", "multi-region", "az"} {
		if m, ok := unknown.Multiplier(); ok {
			t.Errorf("Multiplier(%q) = %v, want unknown", unknown, m)
		}
		if u, ok := RDSNormalizationUnits("large", unknown); ok {
			t.Errorf("RDSNormalizationUnits(large, %q) = %v, want unknown", unknown, u)
		}
	}
}

func TestRDSNormalizationUnitsExtrapolatesAndRefuses(t *testing.T) {
	// An undocumented <N>xlarge extrapolates as 8×N, reproducing every
	// documented row; a newly launched size must not silently lose coverage.
	for size, want := range map[string]float64{"3xlarge": 24, "9xlarge": 72, "48xlarge": 384} {
		got, ok := RDSNormalizationUnits(size, RDSSingleAZ)
		if !ok {
			t.Errorf("RDSNormalizationUnits(%q): want extrapolation, got not-found", size)
			continue
		}
		near(t, got, want, 1e-12, "RDSNormalizationUnits("+size+")")
	}
	// Anything else is unknown, which costs only flexibility, never accuracy.
	for _, size := range []string{"", "nano", "serverless", "metal", "0xlarge", "-2xlarge", "xlargexlarge"} {
		if u, ok := RDSNormalizationUnits(size, RDSSingleAZ); ok {
			t.Errorf("RDSNormalizationUnits(%q) = %v, want not-found", size, u)
		}
	}
}

// TestRDSClassTypeIsNotTheFamily: AWS's counter-example is the whole point.
// db.r6i and db.r6id are different instance class types, and a family-prefix
// rule — or reuse of FamilyOf, which returns "db" for every DB class — would
// merge discounts AWS keeps apart.
func TestRDSClassTypeIsNotTheFamily(t *testing.T) {
	for _, tc := range []struct{ class, classType, size string }{
		{"db.r6i.large", "db.r6i", "large"},
		{"db.r6i.xlarge", "db.r6i", "xlarge"},
		{"db.r6id.large", "db.r6id", "large"},
		{"db.r7g.large", "db.r7g", "large"},
		{"db.m5d.8xlarge", "db.m5d", "8xlarge"},
		{"db.t4g.micro", "db.t4g", "micro"},
		{"DB.R6I.LARGE", "db.r6i", "large"},
		{" db.x2iedn.16xlarge ", "db.x2iedn", "16xlarge"},
	} {
		if got := RDSClassType(tc.class); got != tc.classType {
			t.Errorf("RDSClassType(%q) = %q, want %q", tc.class, got, tc.classType)
		}
		if got := RDSSize(tc.class); got != tc.size {
			t.Errorf("RDSSize(%q) = %q, want %q", tc.class, got, tc.size)
		}
	}
	if RDSClassType("db.r6i.large") == RDSClassType("db.r6id.large") ||
		RDSClassType("db.r6i.large") == RDSClassType("db.r7g.large") {
		t.Fatal("db.r6i, db.r6id and db.r7g must be three different class types")
	}
	// FamilyOf is the EC2 notion and is useless here — it says "db" for all of
	// them. Recorded so nobody wires it in by reflex.
	if FamilyOf("db.r6i.large") != "db" || FamilyOf("db.r7g.large") != "db" {
		t.Fatal("FamilyOf no longer collapses DB classes; revisit the class-type rule")
	}
	// Not a DB instance class ⇒ no class type, no units, exact match only.
	for _, bad := range []string{"", "db", "db.", "db.serverless", "r6i.large", "m5.xlarge", "db..large"} {
		if ct := RDSClassType(bad); ct != "" {
			t.Errorf("RDSClassType(%q) = %q, want \"\"", bad, ct)
		}
		if u, ok := RDSClassUnits(bad, RDSSingleAZ); ok {
			t.Errorf("RDSClassUnits(%q) = %v, want not-found", bad, u)
		}
	}
}

// TestRDSEngineGateMatchesTheDocumentedExclusions pins the verbatim rule:
// "Size flexibility does not apply to RDS for SQL Server and RDS for Oracle
// License Included." (It does apply to Db2, MariaDB, MySQL, PostgreSQL and
// Oracle BYOL.)
func TestRDSEngineGateMatchesTheDocumentedExclusions(t *testing.T) {
	for _, tc := range []struct {
		engine   string
		flexible bool
	}{
		{"postgresql", true}, {"postgres", true}, {"PostgreSQL", true},
		{"mysql", true}, {"mariadb", true}, {"db2", true}, {"db2-se", true},
		{"oracle-se2(byol)", true}, {"oracle-ee(bring-your-own-license)", true},
		{"oracle-se2(license-included)", false}, {"oracle-ee(li)", false},
		{"oracle-se2", false}, // licence model unstated ⇒ ambiguous ⇒ not flexible
		{"sqlserver-se", false}, {"sqlserver-ee", false}, {"sqlserver-ex", false},
		{"sqlserver-ee(li)", false},
		{"aurora-postgresql", false}, {"aurora-mysql", false},
		{"", false}, {"cobol", false},
	} {
		if got := RDSSizeFlexibleEngine(tc.engine); got != tc.flexible {
			t.Errorf("RDSSizeFlexibleEngine(%q) = %v, want %v", tc.engine, got, tc.flexible)
		}
	}
	// Normalization folds the licence marker and the postgres spelling, and
	// preserves the edition: a sqlserver-se reservation is not a sqlserver-ee
	// reservation.
	if NormalizeRDSEngine("PostgreSQL ") != "postgresql" || NormalizeRDSEngine("postgres") != "postgresql" {
		t.Error("postgres/postgresql must fold to one identity")
	}
	if NormalizeRDSEngine("oracle-se2(license-included)") != NormalizeRDSEngine("oracle-se2(li)") {
		t.Error("licence-included spellings must fold to one identity")
	}
	if NormalizeRDSEngine("sqlserver-se") == NormalizeRDSEngine("sqlserver-ee") {
		t.Error("SQL Server editions must stay distinct")
	}
	for _, tc := range []struct{ engine, family string }{
		{"postgresql", "postgresql"}, {"oracle-se2(byol)", "oracle"},
		{"sqlserver-ee", "sqlserver"}, {"db2-ae", "db2"}, {"aurora-mysql", "aurora"},
		{"mariadb", "mariadb"}, {"", ""},
	} {
		if got := RDSEngineFamily(tc.engine); got != tc.family {
			t.Errorf("RDSEngineFamily(%q) = %q, want %q", tc.engine, got, tc.family)
		}
	}
}

// ---------------------------------------------------------------------------
// the waterfall
// ---------------------------------------------------------------------------

// TestComputeSPNeverAbsorbsRDS: a Compute Savings Plan covers EC2, Fargate and
// Lambda and stops there — it does not cover RDS. Neither does an EC2 Instance
// SP, nor an EC2 Reserved Instance whose family looks like the DB class's.
// Getting this wrong silently zeroes out every RDS number in an account that
// holds a Compute SP. Mirrors pkg/ebs's TestEBSIsNeverAbsorbedByACommitment.
func TestComputeSPNeverAbsorbsRDS(t *testing.T) {
	inv := &Inventory{
		RIs: []ReservedInstance{{
			ID: "ri-r6i", Count: 4, InstanceType: "r6i.large", Region: "us-east-1",
			EffectiveHourlyUSD: 0.05,
		}},
		SavingsPlans: []SavingsPlan{
			{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 2},
			{ID: "sp-ec2", Type: SPEC2Instance, Region: "us-east-1", Family: "m5",
				CommitmentUSDPerHour: 1},
		},
	}
	// An EC2 line big enough to exhaust both plans, so the RDS line is left
	// facing a fully-utilized Compute SP rather than an idle one.
	ec2 := UsageLine{
		ID: "i-1", Kind: KindEC2, Region: "us-east-1", InstanceType: "m5.4xlarge",
		Quantity: 10, ODRate: 0.768, ComputeSPRate: 0.48, EC2SPRate: 0.45,
	}
	// The RDS line even carries plausible SP rates: what stops it is product
	// eligibility, not a missing rate.
	db := rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "postgresql", 2, 0.30)
	db.ComputeSPRate, db.EC2SPRate = 0.19, 0.18

	u := Usage{Lines: []UsageLine{ec2, db}}
	if err := u.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	c := inv.Bill(u)
	assertPartition(t, c)

	if d := covOf(t, c, "db-1"); d.RIQty != 0 || d.EC2SPQty != 0 || d.ComputeSPQty != 0 {
		t.Errorf("an RDS line was absorbed by an EC2 commitment: %+v", d)
	} else {
		near(t, d.OnDemandQty, 2, 1e-12, "rds on-demand quantity")
		near(t, d.OnDemandUSD, 0.60, 1e-12, "rds on-demand charge")
	}
	// The plans really were live and exhausted — otherwise this test would
	// pass for the wrong reason.
	near(t, c.SPConsumedUSD, 3, 1e-9, "savings plans consumed")
	near(t, c.SPCommittedUSD, 3, 1e-12, "savings plans committed")
	// The EC2 RI could not reach the DB line either, and is fully stranded.
	near(t, useOf(t, c, "ri-r6i").StrandedUSD(), 0.20, 1e-12, "stranded r6i reservation")

	// End to end: with no RDS reservation in the account, an RDS change nets
	// exactly its list price. A commitment that cannot absorb must not net it.
	after := Usage{Lines: []UsageLine{ec2, rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.30)}}
	as := inv.NetSavings(u, after)
	if as.Suppressed {
		t.Fatalf("a commitment that cannot cover RDS suppressed an RDS change: %s", as.Reason)
	}
	near(t, as.NetHourlyUSD, as.GrossHourlyUSD, 1e-9, "net vs gross with no RDS reservation")
	near(t, as.ClaimableHourlyUSD(), 0.30, 1e-9, "claimable")
}

// TestReservedDBInstanceCoversMultiAZAsTwoUnits reproduces the AWS page's own
// example: "you can move from a Single-AZ deployment running on one large DB
// instance (four normalized units per hour) to a Multi-AZ deployment running
// on two medium DB instances (2+2 = 4 normalized units per hour)."
func TestReservedDBInstanceCoversMultiAZAsTwoUnits(t *testing.T) {
	inv := &Inventory{ReservedDBs: []ReservedDBInstance{
		rdb("rdb-1", "db.m6i.large", RDSSingleAZ, "postgresql", 1, 0.10),
	}}
	// The reservation supplies 4 units. A Multi-AZ medium needs 2 × 2 = 4.
	multiAZMedium := Usage{Lines: []UsageLine{
		rdsLine("db-1", "db.m6i.medium", RDSMultiAZInstance, "postgresql", 1, 0.30),
	}}
	if err := multiAZMedium.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	c := inv.Bill(multiAZMedium)
	assertPartition(t, c)
	near(t, covOf(t, c, "db-1").RIQty, 1, 1e-12, "covered instance-hours")
	near(t, c.OnDemandUSD, 0, 1e-12, "on-demand remainder")
	near(t, c.HourlyUSD, 0.10, 1e-12, "bill")
	near(t, c.StrandedUSD, 0, 1e-12, "stranded")

	// The same move in the other direction: a Multi-AZ medium reservation
	// (2 × 2 = 4 units) covers a Single-AZ large (4 units).
	back := &Inventory{ReservedDBs: []ReservedDBInstance{
		rdb("rdb-2", "db.m6i.medium", RDSMultiAZInstance, "postgresql", 1, 0.10),
	}}
	singleAZLarge := Usage{Lines: []UsageLine{
		rdsLine("db-2", "db.m6i.large", RDSSingleAZ, "postgresql", 1, 0.30),
	}}
	c2 := back.Bill(singleAZLarge)
	assertPartition(t, c2)
	near(t, covOf(t, c2, "db-2").RIQty, 1, 1e-12, "covered instance-hours, reversed")
	near(t, c2.StrandedUSD, 0, 1e-12, "stranded, reversed")

	// And the multiplier is real, not cosmetic: the same Single-AZ large
	// reservation covers only half a Multi-AZ large (4 of the 8 units it needs).
	half := inv.Bill(Usage{Lines: []UsageLine{
		rdsLine("db-3", "db.m6i.large", RDSMultiAZInstance, "postgresql", 1, 0.60),
	}})
	assertPartition(t, half)
	near(t, covOf(t, half, "db-3").RIQty, 0.5, 1e-12, "half a Multi-AZ large")
	near(t, half.OnDemandUSD, 0.30, 1e-12, "the other half at on-demand")

	// A Multi-AZ cluster is three writers-plus-readers' worth, not two.
	clusterUnits, ok := RDSClassUnits("db.m6i.large", RDSMultiAZCluster)
	if !ok || clusterUnits != 12 {
		t.Fatalf("Multi-AZ cluster large = %v/%v, want 12", clusterUnits, ok)
	}
	third := (&Inventory{ReservedDBs: []ReservedDBInstance{
		rdb("rdb-3", "db.m6i.large", RDSSingleAZ, "postgresql", 3, 0.10),
	}}).Bill(Usage{Lines: []UsageLine{
		rdsLine("db-4", "db.m6i.large", RDSMultiAZCluster, "postgresql", 1, 0.90),
	}})
	assertPartition(t, third)
	near(t, covOf(t, third, "db-4").RIQty, 1, 1e-12, "three Single-AZ larges cover one Multi-AZ cluster large")
	near(t, third.StrandedUSD, 0, 1e-12, "nothing stranded")
}

// TestSizeFlexibilityIsEngineGated: the identical downsize, once on PostgreSQL
// and once on SQL Server. "Size flexibility does not apply to RDS for SQL
// Server", so the same recommendation is half-absorbed on one engine and
// strands the reservation completely on the other.
func TestSizeFlexibilityIsEngineGated(t *testing.T) {
	const (
		odXLarge = 0.50
		odLarge  = 0.25
		resRate  = 0.20
	)
	expiry := rdsAt(t, "2027-09-01T00:00:00Z")

	for _, tc := range []struct {
		name             string
		engine           string
		wantCoveredAfter float64
		wantNet          float64
		wantStranded     float64
		wantReason       string
	}{
		{
			name: "postgresql absorbs the downsize", engine: "postgresql",
			// 8 units reserved, 4 used: the freed half is stranded, the bill
			// does not move, and the list-price saving is a fiction.
			wantCoveredAfter: 1, wantNet: 0, wantStranded: 0.10,
			wantReason: ReasonCommitmentNeutral,
		},
		{
			name: "sql server strands the whole reservation", engine: "sqlserver-se",
			// No size flexibility: db.r6i.large is simply not db.r6i.xlarge.
			// The reservation strands 100 % AND the new instance bills on
			// demand, so the change costs money.
			wantCoveredAfter: 0, wantNet: -odLarge, wantStranded: resRate,
			wantReason: ReasonCommitmentNegative,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &Inventory{ReservedDBs: []ReservedDBInstance{func() ReservedDBInstance {
				r := rdb("rdb-1", "db.r6i.xlarge", RDSSingleAZ, tc.engine, 1, resRate)
				r.Expires = expiry
				return r
			}()}}
			before := Usage{Lines: []UsageLine{
				rdsLine("db-1", "db.r6i.xlarge", RDSSingleAZ, tc.engine, 1, odXLarge),
			}}
			after := Usage{Lines: []UsageLine{
				rdsLine("db-1", "db.r6i.large", RDSSingleAZ, tc.engine, 1, odLarge),
			}}
			for _, u := range []Usage{before, after} {
				if err := u.Validate(); err != nil {
					t.Fatalf("validate: %v", err)
				}
			}

			b, a := inv.Bill(before), inv.Bill(after)
			assertPartition(t, b)
			assertPartition(t, a)
			near(t, covOf(t, b, "db-1").RIQty, 1, 1e-12, "before: covered")
			near(t, b.StrandedUSD, 0, 1e-12, "before: stranded")
			near(t, covOf(t, a, "db-1").RIQty, tc.wantCoveredAfter, 1e-12, "after: covered")
			near(t, a.StrandedUSD, tc.wantStranded, 1e-12, "after: stranded")

			as := inv.NetSavings(before, after)
			near(t, as.GrossHourlyUSD, odXLarge-odLarge, 1e-12, "gross")
			near(t, as.NetHourlyUSD, tc.wantNet, 1e-12, "net")
			if !as.Suppressed || as.ReasonCode != tc.wantReason {
				t.Fatalf("suppressed=%v reason=%q, want suppressed with %q",
					as.Suppressed, as.ReasonCode, tc.wantReason)
			}
			if as.ClaimableHourlyUSD() != 0 {
				t.Errorf("suppressed recommendation claimed %v", as.ClaimableHourlyUSD())
			}
			if !as.ValidFrom.Equal(expiry) {
				t.Errorf("ValidFrom = %v, want the reservation's expiry %v", as.ValidFrom, expiry)
			}
		})
	}

	// The two engines really did receive the same recommendation: the only
	// difference between the runs is the engine string.
	pg := (&Inventory{ReservedDBs: []ReservedDBInstance{rdb("r", "db.r6i.xlarge", RDSSingleAZ, "postgresql", 1, resRate)}}).
		Bill(Usage{Lines: []UsageLine{rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, odLarge)}})
	ss := (&Inventory{ReservedDBs: []ReservedDBInstance{rdb("r", "db.r6i.xlarge", RDSSingleAZ, "sqlserver-se", 1, resRate)}}).
		Bill(Usage{Lines: []UsageLine{rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "sqlserver-se", 1, odLarge)}})
	if pg.HourlyUSD == ss.HourlyUSD {
		t.Fatalf("both engines billed %v: the engine gate is not doing anything", pg.HourlyUSD)
	}
	// Oracle License Included is excluded for the same reason; Oracle BYOL is not.
	li := (&Inventory{ReservedDBs: []ReservedDBInstance{rdb("r", "db.r6i.xlarge", RDSSingleAZ, "oracle-se2(li)", 1, resRate)}}).
		Bill(Usage{Lines: []UsageLine{rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "oracle-se2(li)", 1, odLarge)}})
	byol := (&Inventory{ReservedDBs: []ReservedDBInstance{rdb("r", "db.r6i.xlarge", RDSSingleAZ, "oracle-se2(byol)", 1, resRate)}}).
		Bill(Usage{Lines: []UsageLine{rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "oracle-se2(byol)", 1, odLarge)}})
	near(t, li.HourlyUSD, ss.HourlyUSD, 1e-12, "oracle license-included bills like SQL Server")
	near(t, byol.HourlyUSD, pg.HourlyUSD, 1e-12, "oracle BYOL bills like PostgreSQL")
}

// TestClassTypeChangeStrandsEntirely is AWS's own counter-example: "a reserved
// DB instance for a db.r6i.large can apply to a db.r6i.xlarge, but not to a
// db.r6id.large or db.r7g.large, because db.r6id.large and db.r7g.large are
// different instance class types." This is §4.4 example 1 (the +135 % family
// migration) wearing an RDS name.
func TestClassTypeChangeStrandsEntirely(t *testing.T) {
	expiry := rdsAt(t, "2027-06-01T00:00:00Z")
	inv := &Inventory{ReservedDBs: []ReservedDBInstance{func() ReservedDBInstance {
		r := rdb("rdb-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.10)
		r.Expires = expiry
		return r
	}()}}
	before := Usage{Lines: []UsageLine{
		rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.25),
	}}

	// The reservation does float within its own class type — the positive half
	// of the same sentence, without which "strands entirely" proves nothing.
	up := inv.Bill(Usage{Lines: []UsageLine{
		rdsLine("db-1", "db.r6i.xlarge", RDSSingleAZ, "postgresql", 1, 0.50),
	}})
	near(t, covOf(t, up, "db-1").RIQty, 0.5, 1e-12, "db.r6i.large reservation on db.r6i.xlarge usage")

	for _, class := range []string{"db.r6id.large", "db.r7g.large"} {
		t.Run(class, func(t *testing.T) {
			// A Graviton/NVMe migration that looks like a saving at list price.
			after := Usage{Lines: []UsageLine{
				rdsLine("db-1", class, RDSSingleAZ, "postgresql", 1, 0.22),
			}}
			if err := after.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			a := inv.Bill(after)
			assertPartition(t, a)
			if q := covOf(t, a, "db-1").RIQty; q != 0 {
				t.Fatalf("%s absorbed %v instance-hours of a db.r6i reservation", class, q)
			}
			near(t, a.StrandedUSD, 0.10, 1e-12, "the whole reservation is stranded")
			near(t, useOf(t, a, "rdb-1").UsedUSD, 0, 1e-12, "reservation utilization")

			as := inv.NetSavings(before, after)
			near(t, as.GrossHourlyUSD, 0.03, 1e-12, "gross (the list-price fantasy)")
			near(t, as.NetHourlyUSD, -0.22, 1e-12, "net (the invoice)")
			if !as.Suppressed || as.ReasonCode != ReasonCommitmentNegative {
				t.Fatalf("suppressed=%v reason=%q, want a negative-net suppression",
					as.Suppressed, as.ReasonCode)
			}
			if as.ClaimableHourlyUSD() != 0 || as.ClaimableMonthlyUSD() != 0 {
				t.Errorf("suppressed recommendation claimed %v", as.ClaimableHourlyUSD())
			}
			if !as.ValidFrom.Equal(expiry) {
				t.Errorf("ValidFrom = %v, want the reservation end %v", as.ValidFrom, expiry)
			}
			if !strings.Contains(as.Reason, "2027-06-01") {
				t.Errorf("the suppression prose does not name the lapse date: %q", as.Reason)
			}
			// And it does lapse on its own, with no stored state to expire.
			day := expiry.Add(24 * time.Hour)
			if lapsed := inv.Active(day).NetSavings(before, after); lapsed.Suppressed {
				t.Errorf("the suppression outlived the reservation: %s", lapsed.Reason)
			}
		})
	}
}

// TestRDSDeploymentIsNotGuessedWhenUnset: an unset topology is unknown, not
// Single-AZ. Guessing would halve the units the line needs and hand it
// coverage AWS would not have given.
func TestRDSDeploymentIsNotGuessedWhenUnset(t *testing.T) {
	inv := &Inventory{ReservedDBs: []ReservedDBInstance{
		rdb("rdb-1", "db.r6i.large", RDSSingleAZ, "postgresql", 4, 0.10),
	}}
	line := rdsLine("db-1", "db.r6i.large", "", "postgresql", 1, 0.25)
	u := Usage{Lines: []UsageLine{line}}
	if err := u.Validate(); err == nil {
		t.Fatal("Validate accepted an RDS line with no deployment")
	} else if !strings.Contains(err.Error(), "deployment") {
		t.Errorf("error %q must name the deployment", err)
	}
	// Bill never fails, so it must clamp rather than guess: no coverage.
	c := inv.Bill(u)
	assertPartition(t, c)
	if q := covOf(t, c, "db-1").RIQty; q != 0 {
		t.Errorf("an unset deployment was treated as Single-AZ and absorbed %v", q)
	}
	near(t, c.OnDemandUSD, 0.25, 1e-12, "the line bills on-demand")
}

// TestRDSStorageIsNotExpressibleAsCoveredUsage pins the verbatim clause "The
// price for a reserved DB instance doesn't provide a discount for the costs
// associated with storage, backups, and I/O." The defence is structural: a
// KindRDS line must carry a DB instance class, so a storage line cannot become
// a covered line even by accident.
func TestRDSStorageIsNotExpressibleAsCoveredUsage(t *testing.T) {
	storage := UsageLine{
		ID: "db-1-storage", Kind: KindRDS, Region: "us-east-1",
		Engine: "postgresql", Deployment: RDSSingleAZ,
		Unit: "GB-Month", Quantity: 500, ODRate: 0.000159,
	}
	if err := (Usage{Lines: []UsageLine{storage}}).Validate(); err == nil {
		t.Fatal("Validate accepted an RDS storage line as instance usage")
	}
	// Even unvalidated, a huge reservation cannot reach it.
	inv := &Inventory{ReservedDBs: []ReservedDBInstance{
		rdb("rdb-1", "db.r6i.32xlarge", RDSSingleAZ, "postgresql", 10, 5),
	}}
	c := inv.Bill(Usage{Lines: []UsageLine{storage}})
	assertPartition(t, c)
	if q := covOf(t, c, "db-1-storage").RIQty; q != 0 {
		t.Errorf("a reserved DB instance discounted %v of storage", q)
	}
	near(t, c.OnDemandUSD, 500*0.000159, 1e-12, "storage bills at list price")
	near(t, c.StrandedUSD, 50, 1e-12, "the reservation is stranded, not applied")
}

// TestRDSNetCanExceedGrossWhenFreedUnitsAreAbsorbed documents the boundary of
// the "net ≤ gross" claim for RDS, the same way FINDINGS.md documents it for
// EC2. Removing a cheap-per-unit instance frees reserved units that a
// previously on-demand, expensive-per-unit instance absorbs — so the invoice
// falls by more than the list price says. Clamping would hide a real saving,
// and it is why FuzzRDSWaterfall asserts net ≤ gross only where it is true.
func TestRDSNetCanExceedGrossWhenFreedUnitsAreAbsorbed(t *testing.T) {
	inv := &Inventory{ReservedDBs: []ReservedDBInstance{
		rdb("rdb-1", "db.r6i.xlarge", RDSSingleAZ, "postgresql", 1, 0.30), // 8 units
	}}
	// Smallest-first: four mediums (2 units each) take all 8 units, leaving the
	// large — which is dear per unit — entirely on demand.
	small := rdsLine("db-small", "db.r6i.medium", RDSSingleAZ, "postgresql", 4, 0.05)
	big := rdsLine("db-big", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 1.00)

	before := Usage{Lines: []UsageLine{small, big}}
	after := Usage{Lines: []UsageLine{big}} // the mediums are switched off
	for _, u := range []Usage{before, after} {
		if err := u.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
	}
	b, a := inv.Bill(before), inv.Bill(after)
	assertPartition(t, b)
	assertPartition(t, a)
	near(t, covOf(t, b, "db-big").RIQty, 0, 1e-12, "before: the large gets nothing")
	near(t, b.HourlyUSD, 1.30, 1e-12, "before: bill")
	near(t, covOf(t, a, "db-big").RIQty, 1, 1e-12, "after: the large absorbs the freed units")
	near(t, a.HourlyUSD, 0.30, 1e-12, "after: bill")

	as := inv.NetSavings(before, after)
	near(t, as.GrossHourlyUSD, 0.20, 1e-12, "gross")
	near(t, as.NetHourlyUSD, 1.00, 1e-12, "net")
	if as.NetHourlyUSD <= as.GrossHourlyUSD {
		t.Fatal("this fixture is meant to exhibit net > gross; it no longer does")
	}
	if as.Suppressed {
		t.Errorf("a genuine saving was suppressed: %s", as.Reason)
	}
	near(t, as.ClaimableHourlyUSD(), 1.00, 1e-12, "claimable")
}

// ---------------------------------------------------------------------------
// widening the closed set changes nothing that was already there
// ---------------------------------------------------------------------------

func mixedNonRDSUsage() Usage {
	return Usage{Lines: []UsageLine{
		{ID: "i-1", Kind: KindEC2, Region: "us-east-1", AZ: "us-east-1a",
			InstanceType: "m5.large", Quantity: 2, ODRate: 0.096,
			ComputeSPRate: 0.06, EC2SPRate: 0.05},
		{ID: "i-2", Kind: KindEC2, Region: "us-east-1",
			InstanceType: "m5.xlarge", Quantity: 3, ODRate: 0.192,
			ComputeSPRate: 0.12, EC2SPRate: 0.10},
		{ID: "task-1", Kind: KindFargate, Region: "us-east-1", Unit: "vCPU-Hours",
			Quantity: 5, ODRate: 0.04048, ComputeSPRate: 0.0303},
		{ID: "fn-1", Kind: KindLambda, Region: "us-east-1", Unit: "GB-Seconds",
			Quantity: 100000, ODRate: 0.0000166667, ComputeSPRate: 0.0000116667},
		{ID: "fn-1-req", Kind: KindLambda, Region: "us-east-1", Unit: "Requests",
			Quantity: 2, ODRate: 0.20, SPIneligible: true},
	}}
}

func mixedNonRDSInventory() *Inventory {
	return &Inventory{
		RIs: []ReservedInstance{
			{ID: "ri-zonal", Count: 1, InstanceType: "m5.large", Region: "us-east-1",
				AZ: "us-east-1a", EffectiveHourlyUSD: 0.05},
			{ID: "ri-regional", Count: 1, InstanceType: "m5.xlarge", Region: "us-east-1",
				EffectiveHourlyUSD: 0.08},
		},
		SavingsPlans: []SavingsPlan{
			{ID: "sp-ec2", Type: SPEC2Instance, Region: "us-east-1", Family: "m5",
				CommitmentUSDPerHour: 0.05},
			{ID: "sp-compute", Type: SPCompute, CommitmentUSDPerHour: 0.20},
		},
	}
}

// TestExistingKindsAreUnaffectedByRDS: widening the closed set of Kinds and
// adding a fourth commitment product must not move one cent of an
// EC2/Fargate/Lambda bill. The whole existing suite is the primary evidence;
// this asserts the stronger statement directly, line by line.
func TestExistingKindsAreUnaffectedByRDS(t *testing.T) {
	baseUsage, baseInv := mixedNonRDSUsage(), mixedNonRDSInventory()
	baseline := baseInv.Bill(baseUsage)
	assertPartition(t, baseline)

	rdsLines := []UsageLine{
		rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.30),
		rdsLine("db-2", "db.r6i.xlarge", RDSMultiAZInstance, "sqlserver-se", 2, 1.20),
	}
	rdsRes := []ReservedDBInstance{
		rdb("rdb-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.18),
		rdb("rdb-2", "db.m6i.large", RDSMultiAZInstance, "mysql", 2, 0.25),
	}
	var rdsCommitted float64
	for _, r := range rdsRes {
		rdsCommitted += float64(r.Count) * r.EffectiveHourlyUSD
	}

	for _, tc := range []struct {
		name  string
		usage Usage
		inv   *Inventory
		// extra RI dollars the RDS reservations add, if any
		extraCommitted float64
	}{
		{"rds reservations, no rds usage", baseUsage,
			&Inventory{RIs: baseInv.RIs, SavingsPlans: baseInv.SavingsPlans, ReservedDBs: rdsRes},
			rdsCommitted},
		{"rds usage, no rds reservations",
			Usage{Lines: append(append([]UsageLine{}, baseUsage.Lines...), rdsLines...)},
			baseInv, 0},
		{"both", Usage{Lines: append(append([]UsageLine{}, baseUsage.Lines...), rdsLines...)},
			&Inventory{RIs: baseInv.RIs, SavingsPlans: baseInv.SavingsPlans, ReservedDBs: rdsRes},
			rdsCommitted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.usage.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if err := tc.inv.Validate(); err != nil {
				t.Fatalf("validate inventory: %v", err)
			}
			got := tc.inv.Bill(tc.usage)
			assertPartition(t, got)

			// Every pre-existing line is paid for in exactly the same way.
			var onDemand float64
			for _, want := range baseline.Coverage {
				g := covOf(t, got, want.LineID)
				if !reflect.DeepEqual(g, want) {
					t.Errorf("coverage for %q changed:\n got %+v\nwant %+v", want.LineID, g, want)
				}
				onDemand += g.OnDemandUSD
			}
			near(t, onDemand, baseline.OnDemandUSD, 1e-12, "on-demand over pre-existing lines")

			// Savings Plans are untouched: no RDS line consumed a cent of one.
			near(t, got.SPCommittedUSD, baseline.SPCommittedUSD, 1e-12, "SP committed")
			near(t, got.SPConsumedUSD, baseline.SPConsumedUSD, 1e-12, "SP consumed")

			// EC2 reservations behave identically; the RI aggregate grows by
			// exactly the RDS reservations' committed spend and no more.
			for _, want := range baseline.Commitments {
				if !reflect.DeepEqual(useOf(t, got, want.ID), want) {
					t.Errorf("commitment %q changed:\n got %+v\nwant %+v",
						want.ID, useOf(t, got, want.ID), want)
				}
			}
			near(t, got.RICommittedUSD, baseline.RICommittedUSD+tc.extraCommitted, 1e-12, "RI committed")
			if got.Fallback != baseline.Fallback {
				t.Errorf("Fallback flipped to %v", got.Fallback)
			}
		})
	}

	// And the published number for a pure EC2 change is bit-identical whether
	// or not the account also runs RDS.
	after := mixedNonRDSUsage()
	after.Lines[1].Quantity = 1
	plain := baseInv.NetSavings(baseUsage, after)
	withRDS := (&Inventory{RIs: baseInv.RIs, SavingsPlans: baseInv.SavingsPlans, ReservedDBs: rdsRes}).
		NetSavings(
			Usage{Lines: append(append([]UsageLine{}, baseUsage.Lines...), rdsLines...)},
			Usage{Lines: append(append([]UsageLine{}, after.Lines...), rdsLines...)})
	near(t, withRDS.NetHourlyUSD, plain.NetHourlyUSD, 1e-12, "net saving of an EC2 change")
	near(t, withRDS.GrossHourlyUSD, plain.GrossHourlyUSD, 1e-12, "gross saving of an EC2 change")
	if withRDS.Suppressed != plain.Suppressed || withRDS.ReasonCode != plain.ReasonCode {
		t.Errorf("RDS in the account changed an EC2 verdict: %v/%q vs %v/%q",
			withRDS.Suppressed, withRDS.ReasonCode, plain.Suppressed, plain.ReasonCode)
	}
}

// ---------------------------------------------------------------------------
// inventory plumbing
// ---------------------------------------------------------------------------

func TestReservedDBInstanceJSONRoundTripAndValidation(t *testing.T) {
	want := &Inventory{
		ReservedDBs: []ReservedDBInstance{{
			ID: "ri-db-1", Count: 2, DBInstanceClass: "db.r6i.large",
			Region: "us-east-1", Engine: "postgresql", Deployment: RDSMultiAZInstance,
			OfferingType: "Partial Upfront", EffectiveHourlyUSD: 0.187,
			Expires: rdsAt(t, "2027-06-01T00:00:00Z"),
		}},
		FetchedAt: rdsAt(t, "2026-08-26T00:00:00Z"),
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
	if len(got.ReservedDBs) != 1 || got.ReservedDBs[0] != want.ReservedDBs[0] {
		t.Errorf("round-trip:\n got %+v\nwant %+v", got.ReservedDBs, want.ReservedDBs)
	}
	var again bytes.Buffer
	if err := WriteInventory(&again, got); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if again.String() != encoded {
		t.Errorf("serialization is not byte-stable:\n%s\n%s", encoded, again.String())
	}

	for _, tc := range []struct{ name, json, wantErr string }{
		{"zero count", `{"reservedDBInstances":[{"id":"a","count":0,"dbInstanceClass":"db.r6i.large","region":"us-east-1","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":0.1}]}`, "count must be"},
		{"no class", `{"reservedDBInstances":[{"id":"a","count":1,"region":"us-east-1","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":0.1}]}`, "dbInstanceClass required"},
		{"not a db class", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"r6i.large","region":"us-east-1","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":0.1}]}`, "is not a DB instance class"},
		{"no region", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":0.1}]}`, "region required"},
		{"no engine", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","region":"us-east-1","deployment":"single-az","effectiveHourlyUSD":0.1}]}`, "engine required"},
		{"no deployment", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","region":"us-east-1","engine":"postgresql","effectiveHourlyUSD":0.1}]}`, "deployment required"},
		{"unknown deployment", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","region":"us-east-1","engine":"postgresql","deployment":"three-az","effectiveHourlyUSD":0.1}]}`, "unknown deployment"},
		{"negative rate", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","region":"us-east-1","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":-1}]}`, "bad effectiveHourlyUSD"},
		{"duplicate id", `{"reservedDBInstances":[{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","region":"us-east-1","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":0.1},{"id":"a","count":1,"dbInstanceClass":"db.r6i.large","region":"us-east-1","engine":"postgresql","deployment":"single-az","effectiveHourlyUSD":0.1}]}`, "duplicate reserved db instance"},
		{"unknown field", `{"reservedDBInstances":[{"id":"a","multiAZ":true}]}`, "parse inventory"},
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

func TestRDSUsageLineValidation(t *testing.T) {
	ok := rdsLine("db-1", "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.25)
	if err := (Usage{Lines: []UsageLine{ok}}).Validate(); err != nil {
		t.Fatalf("a well-formed RDS line was rejected: %v", err)
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*UsageLine)
		wantErr string
	}{
		{"no class", func(l *UsageLine) { l.InstanceType = "" }, "DB instance class"},
		{"ec2 class", func(l *UsageLine) { l.InstanceType = "r6i.large" }, "not a DB instance class"},
		{"aurora serverless", func(l *UsageLine) { l.InstanceType = "db.serverless" }, "not a DB instance class"},
		{"no engine", func(l *UsageLine) { l.Engine = "" }, "engine"},
		{"no deployment", func(l *UsageLine) { l.Deployment = "" }, "deployment"},
		{"unknown deployment", func(l *UsageLine) { l.Deployment = "two-az" }, "unknown deployment"},
		{"sp-ineligible flag", func(l *UsageLine) { l.SPIneligible = true }, "no savings plan covers RDS"},
		// The checks the other kinds get apply to RDS too.
		{"bad quantity", func(l *UsageLine) { l.Quantity = math.NaN() }, "bad quantity"},
		{"negative rate", func(l *UsageLine) { l.ODRate = -1 }, "bad on-demand rate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := ok
			tc.mutate(&l)
			err := (Usage{Lines: []UsageLine{l}}).Validate()
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q must mention %q", err, tc.wantErr)
			}
		})
	}
	// The kind itself is still a closed set.
	bad := ok
	bad.Kind = "dynamodb"
	if err := (Usage{Lines: []UsageLine{bad}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("widening the closed set let %q through: %v", bad.Kind, err)
	}
}

// TestActiveDropsExpiredReservedDBInstances: this is how an RDS suppression
// lapses — the same mechanism the EC2 products use, with no stored state.
func TestActiveDropsExpiredReservedDBInstances(t *testing.T) {
	past, future := rdsAt(t, "2026-01-01T00:00:00Z"), rdsAt(t, "2028-01-01T00:00:00Z")
	mk := func(id string, exp time.Time) ReservedDBInstance {
		r := rdb(id, "db.r6i.large", RDSSingleAZ, "postgresql", 1, 0.1)
		r.Expires = exp
		return r
	}
	inv := &Inventory{ReservedDBs: []ReservedDBInstance{
		mk("expired", past), mk("live", future), mk("open-ended", time.Time{}),
	}}
	got := inv.Active(rdsAt(t, "2027-01-01T00:00:00Z"))
	if len(got.ReservedDBs) != 2 || got.ReservedDBs[0].ID != "live" || got.ReservedDBs[1].ID != "open-ended" {
		t.Errorf("Active kept the wrong reservations: %+v", got.ReservedDBs)
	}
	if len(inv.ReservedDBs) != 3 {
		t.Error("Active must not mutate its receiver")
	}
	var nilInv *Inventory
	if a := nilInv.Active(future); a == nil || len(a.ReservedDBs) != 0 {
		t.Errorf("nil inventory Active = %+v", a)
	}
}

// ---------------------------------------------------------------------------
// fuzz
// ---------------------------------------------------------------------------

var (
	fuzzDBClasses = []string{
		"db.r6i.large", "db.r6i.xlarge", "db.r6id.large", "db.r7g.large",
		"db.m6i.2xlarge", "db.t4g.micro", "db.serverless", "zz9.plural",
	}
	fuzzEngines = []string{
		"postgresql", "postgres", "mysql", "sqlserver-se",
		"oracle-se2(byol)", "oracle-se2(li)", "aurora-postgresql", "",
	}
	fuzzDeployments = []RDSDeployment{
		RDSSingleAZ, RDSMultiAZInstance, RDSMultiAZCluster, "", "two-az",
	}
	fuzzRDSRegions = []string{"us-east-1", "eu-west-1"}
)

func shuffledBy[T any](s *stream, in []T) []T {
	if len(in) == 0 {
		return in
	}
	out := append([]T(nil), in...)
	for i := len(out) - 1; i > 0; i-- {
		j := s.mod(i + 1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// synthRDS builds bounded, always-finite RDS-heavy input, plus enough
// EC2/Fargate/Lambda noise and enough EC2 commitments to catch cross-product
// leakage in either direction.
func synthRDS(data []byte) (Usage, *Inventory, *stream) {
	s := &stream{b: data}
	var u Usage
	for n := 1 + s.mod(5); n > 0; n-- {
		u.Lines = append(u.Lines, UsageLine{
			ID:           "db" + string(rune('a'+s.mod(6))),
			Kind:         KindRDS,
			Region:       s.pick(fuzzRDSRegions),
			InstanceType: s.pick(fuzzDBClasses),
			Engine:       s.pick(fuzzEngines),
			Deployment:   fuzzDeployments[s.mod(len(fuzzDeployments))],
			Unit:         "Instance-Hours",
			Quantity:     s.frac(20),
			ODRate:       s.frac(4),
		})
	}
	for n := s.mod(3); n > 0; n-- {
		l := UsageLine{
			ID:       "x" + string(rune('a'+s.mod(4))),
			Kind:     fuzzKinds[s.mod(len(fuzzKinds))],
			Region:   s.pick(fuzzRDSRegions),
			AZ:       s.pick(fuzzAZs),
			Quantity: s.frac(50),
			ODRate:   s.frac(2),
		}
		if l.Kind == KindEC2 {
			l.InstanceType = s.pick(fuzzTypes)
		} else {
			l.Unit = s.pick(fuzzUnits)
		}
		if s.next()&1 == 0 {
			l.ComputeSPRate = l.ODRate * s.frac(1)
		}
		u.Lines = append(u.Lines, l)
	}

	inv := &Inventory{}
	for n := s.mod(5); n > 0; n-- {
		inv.ReservedDBs = append(inv.ReservedDBs, ReservedDBInstance{
			ID:                 "rdb" + string(rune('0'+s.mod(9))),
			Count:              1 + s.mod(4),
			DBInstanceClass:    s.pick(fuzzDBClasses),
			Region:             s.pick(fuzzRDSRegions),
			Engine:             s.pick(fuzzEngines),
			Deployment:         fuzzDeployments[s.mod(len(fuzzDeployments))],
			EffectiveHourlyUSD: s.frac(2),
			Expires:            time.Unix(int64(s.mod(200))*86400, 0).UTC(),
		})
	}
	for n := s.mod(3); n > 0; n-- {
		inv.RIs = append(inv.RIs, ReservedInstance{
			ID: "ri" + string(rune('0'+s.mod(9))), Count: 1 + s.mod(3),
			InstanceType: s.pick(fuzzTypes), Region: s.pick(fuzzRDSRegions),
			EffectiveHourlyUSD: s.frac(1),
		})
	}
	for n := s.mod(3); n > 0; n-- {
		inv.SavingsPlans = append(inv.SavingsPlans, SavingsPlan{
			ID: "sp" + string(rune('0'+s.mod(9))), Type: SPCompute,
			CommitmentUSDPerHour: s.frac(20),
		})
	}
	return u, inv, s
}

// FuzzRDSWaterfall asserts, over arbitrary reservation inventories, that the
// RDS stage is finite, deterministic, order-independent, never over-allocates
// a reservation, never touches a non-RDS line and is never touched by a non-RDS
// commitment — and that shrinking one DB instance nets no more than its
// list-price delta.
//
// The last property is scoped to a single line on purpose. "Net ≤ Gross" is
// FALSE in general and deliberately not clamped (see FINDINGS.md, and
// TestRDSNetCanExceedGrossWhenFreedUnitsAreAbsorbed for the RDS witness):
// freeing reserved units that a dearer instance then absorbs is genuinely
// worth more than the list price says. With one line there is nothing else to
// absorb the freed units, so the bound holds and asserting it is meaningful
// rather than merely lucky.
func FuzzRDSWaterfall(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1})
	f.Add([]byte{2, 0, 0, 0, 0, 200, 128, 0, 1, 1, 0, 100, 60, 0, 1, 2, 0, 0, 0, 128, 3})
	f.Add([]byte{4, 1, 1, 3, 2, 250, 250, 2, 0, 2, 1, 200, 200, 0, 0, 3, 3, 1, 1, 1, 255, 9, 9, 9})
	f.Add([]byte{5, 0, 3, 4, 1, 100, 90, 1, 1, 0, 3, 40, 30, 2, 2, 2, 2, 200, 4, 255, 255, 255})

	f.Fuzz(func(t *testing.T, data []byte) {
		const tol = 1e-6
		usage, inv, s := synthRDS(data)
		c := inv.Bill(usage)

		// Deterministic, bit for bit.
		if again := inv.Bill(usage); !reflect.DeepEqual(again, c) {
			t.Fatalf("Bill is not deterministic:\n%+v\n%+v", c, again)
		}
		// Order-independent: the answer is a function of the multiset.
		shuffled := &Inventory{
			RIs:          shuffledBy(s, inv.RIs),
			SavingsPlans: shuffledBy(s, inv.SavingsPlans),
			ReservedDBs:  shuffledBy(s, inv.ReservedDBs),
			FetchedAt:    inv.FetchedAt,
		}
		if got := shuffled.Bill(Usage{Lines: shuffledBy(s, usage.Lines)}); !reflect.DeepEqual(got, c) {
			t.Fatalf("Bill depends on input order:\n%+v\n%+v", c, got)
		}

		// Finite, non-negative, and partitioned; never below the committed floor.
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
		floor := c.RICommittedUSD + c.SPCommittedUSD
		if d := c.HourlyUSD - (floor + c.OnDemandUSD); d > tol || d < -tol {
			t.Fatalf("bill %v != committed %v + on-demand %v", c.HourlyUSD, floor, c.OnDemandUSD)
		}
		if c.HourlyUSD < floor-tol {
			t.Fatalf("bill %v below committed floor %v", c.HourlyUSD, floor)
		}
		for _, cu := range c.Commitments {
			if cu.UsedUSD > cu.CommittedUSD+tol || cu.UsedUSD < -tol {
				t.Fatalf("commitment %q used %v of %v", cu.ID, cu.UsedUSD, cu.CommittedUSD)
			}
		}
		// Committed spend is a function of the inventory alone — the point of
		// the whole package. An RDS reservation is charged for an hour in
		// which no DB ran.
		empty := inv.Bill(Usage{})
		if d := empty.RICommittedUSD - c.RICommittedUSD; d > tol || d < -tol {
			t.Fatalf("RI committed depends on usage: %v vs %v", empty.RICommittedUSD, c.RICommittedUSD)
		}

		states := newStates(usage.Lines)
		if len(states) != len(c.Coverage) {
			t.Fatalf("coverage has %d entries for %d lines", len(c.Coverage), len(states))
		}
		// No Savings Plan and no EC2 reservation may reach an RDS line, and no
		// RDS reservation may reach anything else. The units handed out cannot
		// exceed the units owned.
		unitsComparable := true
		var owned, allocated float64
		for _, r := range inv.ReservedDBs {
			u, ok := r.Units()
			if !ok {
				unitsComparable = false // whole-instance counting; not the same scale
				continue
			}
			owned += float64(r.Count) * u
		}
		for i, cv := range c.Coverage {
			line := states[i].line
			if line.ID != cv.LineID {
				t.Fatalf("coverage %d is not aligned with the usage: %q vs %q", i, cv.LineID, line.ID)
			}
			sum := cv.RIQty + cv.EC2SPQty + cv.ComputeSPQty + cv.OnDemandQty
			if d := sum - cv.Quantity; d > tol || d < -tol {
				t.Fatalf("line %q: coverage %v != quantity %v", cv.LineID, sum, cv.Quantity)
			}
			if line.Kind != KindRDS {
				continue
			}
			if cv.EC2SPQty != 0 || cv.ComputeSPQty != 0 {
				t.Fatalf("a savings plan absorbed RDS line %q: %+v", cv.LineID, cv)
			}
			if cv.Fallback {
				t.Fatalf("RDS line %q took the no-SP-rate fallback path", cv.LineID)
			}
			ue, ok := rdsLineUnits(line)
			if !ok {
				if cv.RIQty > tol {
					unitsComparable = false
				}
				continue
			}
			allocated += cv.RIQty * ue
		}
		if unitsComparable && allocated > owned+tol {
			t.Fatalf("allocated %v normalized units from a pool of %v", allocated, owned)
		}

		// Cross-product isolation, stated as an equality: strip the RDS lines
		// and the RDS reservations and every other line is paid for in exactly
		// the same way.
		plain := &Inventory{RIs: inv.RIs, SavingsPlans: inv.SavingsPlans, FetchedAt: inv.FetchedAt}
		var kept []UsageLine
		for _, l := range usage.Lines {
			if l.Kind != KindRDS {
				kept = append(kept, l)
			}
		}
		ref := plain.Bill(Usage{Lines: kept})
		var j int
		for i, cv := range c.Coverage {
			if states[i].line.Kind == KindRDS {
				continue
			}
			if j >= len(ref.Coverage) || !reflect.DeepEqual(cv, ref.Coverage[j]) {
				t.Fatalf("RDS changed how non-RDS line %q is paid for:\n%+v\n%+v", cv.LineID, cv, ref.Coverage[j])
			}
			j++
		}
		if j != len(ref.Coverage) {
			t.Fatalf("non-RDS coverage count %d != %d", j, len(ref.Coverage))
		}
		if ref.SPCommittedUSD != c.SPCommittedUSD || ref.SPConsumedUSD != c.SPConsumedUSD {
			t.Fatalf("RDS moved savings-plan consumption: %v/%v vs %v/%v",
				c.SPCommittedUSD, c.SPConsumedUSD, ref.SPCommittedUSD, ref.SPConsumedUSD)
		}
		if ref.Fallback != c.Fallback {
			t.Fatalf("RDS flipped the conservative-fallback flag to %v", c.Fallback)
		}

		// Shrinking ONE DB instance: 0 ≤ net ≤ gross, over whatever inventory
		// the fuzzer built. See the doc comment for why this is single-line.
		for _, l := range usage.Lines {
			if l.Kind != KindRDS {
				continue
			}
			smaller := l
			smaller.Quantity = l.Quantity * s.frac(1)
			before := Usage{Lines: []UsageLine{l}}
			after := Usage{Lines: []UsageLine{smaller}}
			as := inv.NetSavings(before, after)
			if math.IsNaN(as.NetHourlyUSD) || math.IsInf(as.NetHourlyUSD, 0) {
				t.Fatalf("net is not finite: %v", as.NetHourlyUSD)
			}
			if as.NetHourlyUSD < -tol {
				t.Fatalf("shrinking one DB instance raised the bill by %v", -as.NetHourlyUSD)
			}
			if as.NetHourlyUSD > as.GrossHourlyUSD+tol {
				t.Fatalf("net %v exceeds gross %v for a single-line shrink",
					as.NetHourlyUSD, as.GrossHourlyUSD)
			}
			if as.Conservative {
				t.Fatal("an RDS-only assessment claimed the no-SP-rate fallback")
			}
			if as.Suppressed && (as.ClaimableHourlyUSD() != 0 || as.ReasonCode == "") {
				t.Fatalf("suppressed assessment claimed %v with reason %q",
					as.ClaimableHourlyUSD(), as.ReasonCode)
			}
			break
		}
	})
}
