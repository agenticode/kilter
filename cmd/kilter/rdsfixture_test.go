package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	krds "github.com/agenticode/kilter/pkg/rds"
)

// The recorded RDS account, and why each row is in it.
//
// Same discipline as testdata/ec2-instances.json: generated, committed, and
// re-checked by TestWriteRDSFixture, so a regeneration that changes the bytes
// means pkg/rds's collector changed and a reviewer should look. Regenerate:
//
//	go test ./cmd/kilter -run TestWriteRDSFixture -update-fixtures
//
// Every instance is the INTERESTING case rather than the easy one, chosen so
// the wiring exercises a different refusal path per row:
//
//	db-pg-primary        PostgreSQL, 500 GiB gp2 with autoscaling on and 80 %
//	                     of it never used — trap 9 (FreeableMemory is page
//	                     cache, so the series is not converted to headroom at
//	                     all) and trap 8 (the floor has a dollar and no API).
//	db-mysql-multiaz     MySQL Multi-AZ — trap 10: the instance line is
//	                     doubled and the storage line is not. Its FreeableMemory
//	                     IS readable (anonymous buffer pool) and the downsize is
//	                     STILL refused, for a different reason. Same metric,
//	                     different engine, different verdict: that is the trap
//	                     nothing else in the tree catches.
//	db-pg-replica        a read replica with zero connections across the whole
//	                     window — the one replica finding that is safe to state.
//	db-mssql             SQL Server EE license-included — refused BY NAME with
//	                     engine-not-priced rather than quoted an open-source
//	                     rate for a licensed engine.
//	db-aurora            Aurora, refused by name (trap 16).
//	db-mysql-cluster     a Multi-AZ DB CLUSTER member on MySQL, refused under
//	                     its OWN name and not Aurora's — §5.3, and the reason
//	                     DescribeDBClusters is read at all.
//	db-legacy            tagged kilter.dev/mode=off — the guardrail, which
//	                     reaches the report only because ListTagsForResource is
//	                     wired.
//
// One Reserved DB Instance covers db.r6i.xlarge on PostgreSQL, so the
// commitment seam has something to answer with.

const rdsFixtureFileName = "rds-account.json"

func rdsARN(id string) string {
	return "arn:aws:rds:" + fixtureRegion + ":000000000000:db:" + id
}

// rdsSeries records one metric at a 6-hour cadence across the window. The
// cadence is deliberately coarse: every evidence gate in pkg/rds is about
// window SPAN and delivery completeness, not sample count, so a dense series
// would add a megabyte of JSON and prove nothing extra.
func rdsSeries(value float64) []krds.Point {
	const step = 6 * time.Hour
	const days = 14
	n := int((days * 24 * time.Hour) / step)
	start := fixtureNow.Add(-days * 24 * time.Hour).Add(step)
	return krds.SyntheticMetric(start, step, n, value)
}

func buildRDSFixture() rdsFixtureFile {
	const gib = float64(1 << 30)
	inst := []krds.DBInstanceRecord{
		{
			DBInstanceIdentifier: "db-pg-primary", DBInstanceArn: rdsARN("db-pg-primary"),
			DBInstanceClass: "db.r6i.xlarge", DBInstanceStatus: krds.StatusAvailable,
			Engine: "postgres", EngineVersion: "16.4", LicenseModel: krds.LicenseGPL,
			AvailabilityZone: fixtureRegion + "a",
			// 500 allocated, autoscaling to 1000: the floor can move on its
			// own and leaves no CloudTrail event.
			AllocatedStorage: 500, MaxAllocatedStorage: 1000,
			StorageType:                      krds.StorageGP2,
			ReadReplicaDBInstanceIdentifiers: []string{"db-pg-replica"},
			InstanceCreateTime:               fixtureNow.Add(-400 * 24 * time.Hour),
		},
		{
			DBInstanceIdentifier: "db-mysql-multiaz", DBInstanceArn: rdsARN("db-mysql-multiaz"),
			DBInstanceClass: "db.r6i.large", DBInstanceStatus: krds.StatusAvailable,
			Engine: "mysql", EngineVersion: "8.0.39", LicenseModel: krds.LicenseGPL,
			MultiAZ: true, AvailabilityZone: fixtureRegion + "b",
			AllocatedStorage: 200, StorageType: krds.StorageGP3, Iops: 3000,
			InstanceCreateTime: fixtureNow.Add(-300 * 24 * time.Hour),
		},
		{
			DBInstanceIdentifier: "db-pg-replica", DBInstanceArn: rdsARN("db-pg-replica"),
			DBInstanceClass: "db.r6i.large", DBInstanceStatus: krds.StatusAvailable,
			Engine: "postgres", EngineVersion: "16.4", LicenseModel: krds.LicenseGPL,
			AvailabilityZone:                      fixtureRegion + "c",
			ReadReplicaSourceDBInstanceIdentifier: "db-pg-primary",
			AllocatedStorage:                      500, StorageType: krds.StorageGP2,
			InstanceCreateTime: fixtureNow.Add(-200 * 24 * time.Hour),
		},
		{
			DBInstanceIdentifier: "db-mssql", DBInstanceArn: rdsARN("db-mssql"),
			DBInstanceClass: "db.r6i.xlarge", DBInstanceStatus: krds.StatusAvailable,
			Engine: "sqlserver-ee", EngineVersion: "15.00", LicenseModel: krds.LicenseIncluded,
			AvailabilityZone: fixtureRegion + "a",
			AllocatedStorage: 300, StorageType: krds.StorageGP2,
			InstanceCreateTime: fixtureNow.Add(-500 * 24 * time.Hour),
		},
		{
			DBInstanceIdentifier: "db-aurora", DBInstanceArn: rdsARN("db-aurora"),
			DBInstanceClass: "db.r6i.large", DBInstanceStatus: krds.StatusAvailable,
			Engine: "aurora-postgresql", EngineVersion: "15.4", LicenseModel: krds.LicenseGPL,
			DBClusterIdentifier: "aurora-prod", AvailabilityZone: fixtureRegion + "a",
			InstanceCreateTime: fixtureNow.Add(-120 * 24 * time.Hour),
		},
		{
			DBInstanceIdentifier: "db-mysql-cluster", DBInstanceArn: rdsARN("db-mysql-cluster"),
			DBInstanceClass: "db.r6i.large", DBInstanceStatus: krds.StatusAvailable,
			Engine: "mysql", EngineVersion: "8.0.39", LicenseModel: krds.LicenseGPL,
			DBClusterIdentifier: "mazdb-prod", AvailabilityZone: fixtureRegion + "b",
			AllocatedStorage: 100, StorageType: krds.StorageGP3,
			InstanceCreateTime: fixtureNow.Add(-60 * 24 * time.Hour),
		},
		{
			DBInstanceIdentifier: "db-legacy", DBInstanceArn: rdsARN("db-legacy"),
			DBInstanceClass: "db.t3.medium", DBInstanceStatus: krds.StatusAvailable,
			Engine: "postgres", EngineVersion: "13.16", LicenseModel: krds.LicenseGPL,
			AvailabilityZone: fixtureRegion + "c",
			AllocatedStorage: 50, StorageType: krds.StorageGP2,
			InstanceCreateTime: fixtureNow.Add(-900 * 24 * time.Hour),
		},
	}

	clusters := []krds.DBClusterRecord{
		{
			DBClusterIdentifier: "aurora-prod", Engine: "aurora-postgresql",
			EngineMode: "provisioned", DBClusterMembers: []string{"db-aurora"},
			ServerlessV2MinCapacity: 0.5, ServerlessV2MaxCapacity: 16,
		},
		{
			// A PostgreSQL/MySQL Multi-AZ DB cluster. Calling it "Aurora"
			// would be a false statement in a report whose whole value is
			// that its statements are true.
			DBClusterIdentifier: "mazdb-prod", Engine: "mysql",
			EngineMode: "provisioned", DBClusterMembers: []string{"db-mysql-cluster"},
		},
	}

	tags := map[string]map[string]string{
		rdsARN("db-pg-primary"): {"env": "prod", "team": "payments"},
		rdsARN("db-legacy"):     {krds.TagKilterMode: "off"},
	}

	metrics := map[string][]krds.Point{
		// A busy primary whose FreeableMemory looks like 9 GiB of headroom and
		// is nothing of the kind.
		"db-pg-primary/" + krds.MetricCPUUtilization:   rdsSeries(28),
		"db-pg-primary/" + krds.MetricFreeableMemory:   rdsSeries(9 * gib),
		"db-pg-primary/" + krds.MetricFreeStorageSpace: rdsSeries(400 * gib),
		"db-pg-primary/" + krds.MetricDatabaseConns:    rdsSeries(42),

		// The same-shaped memory series on MySQL, where it IS readable.
		"db-mysql-multiaz/" + krds.MetricCPUUtilization:   rdsSeries(31),
		"db-mysql-multiaz/" + krds.MetricFreeableMemory:   rdsSeries(6 * gib),
		"db-mysql-multiaz/" + krds.MetricFreeStorageSpace: rdsSeries(150 * gib),
		"db-mysql-multiaz/" + krds.MetricDatabaseConns:    rdsSeries(12),

		// Zero connections and near-zero CPU across the whole window.
		"db-pg-replica/" + krds.MetricCPUUtilization:   rdsSeries(1),
		"db-pg-replica/" + krds.MetricFreeableMemory:   rdsSeries(11 * gib),
		"db-pg-replica/" + krds.MetricFreeStorageSpace: rdsSeries(420 * gib),
		"db-pg-replica/" + krds.MetricDatabaseConns:    rdsSeries(0),

		"db-mssql/" + krds.MetricCPUUtilization: rdsSeries(15),
		"db-mssql/" + krds.MetricDatabaseConns:  rdsSeries(8),
	}

	return rdsFixtureFile{
		Instances: inst,
		Clusters:  clusters,
		Tags:      tags,
		Metrics:   metrics,
		Reservations: []krds.ReservedDBInstanceRecord{{
			ReservedDBInstanceId: "ri-rds-1", DBInstanceClass: "db.r6i.xlarge",
			DBInstanceCount: 1, ProductDescription: "postgresql",
			OfferingType: "All Upfront", State: "active",
			FixedPrice: 2800, UsagePrice: 0,
			Duration:  int64((365 * 24 * time.Hour).Seconds()),
			StartTime: fixtureNow.Add(-180 * 24 * time.Hour),
		}},
		// Two pages, so the collector's pagination is actually exercised
		// rather than merely present.
		PageSize: 3,
	}
}

// TestWriteRDSFixture mirrors TestWriteDomainFixtures: it regenerates the
// recorded account under -update-fixtures and otherwise asserts the committed
// bytes still match. The fixture is fed to pkg/rds's REAL collector, so a diff
// here is a change in that collector.
func TestWriteRDSFixture(t *testing.T) {
	want, err := json.Marshal(buildRDSFixture())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want = append(want, '\n')
	path := fixturePath(rdsFixtureFileName)
	if *updateFixtures {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(want))
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-fixtures to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s is stale (%d bytes on disk, %d generated).\n"+
			"Regenerate with -update-fixtures and review the diff — a change here "+
			"means pkg/rds's collector changed.", path, len(got), len(want))
	}
}
