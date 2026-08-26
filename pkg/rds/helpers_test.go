package rds

import (
	"context"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Fixed clock. Nothing in this package reads time.Now (TestNoClockReads), and
// nothing in its tests does either: a test whose fixture depends on the wall
// clock cannot fail the same way twice.
var (
	testEnd   = time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	testStart = testEnd.Add(-14 * 24 * time.Hour)
	testNow   = testEnd
)

func testWindow() Window { return Window{Start: testStart, End: testEnd} }

// instOpt mutates a recorded DB instance. Options rather than a wide
// constructor so each test states only the axis it is about.
type instOpt func(*DBInstanceRecord)

func withMultiAZ() instOpt { return func(r *DBInstanceRecord) { r.MultiAZ = true } }
func withLicense(l string) instOpt {
	return func(r *DBInstanceRecord) { r.LicenseModel = l }
}
func withCluster(id string) instOpt {
	return func(r *DBInstanceRecord) { r.DBClusterIdentifier = id }
}
func withReplicaOf(src string) instOpt {
	return func(r *DBInstanceRecord) { r.ReadReplicaSourceDBInstanceIdentifier = src }
}
func withStorage(allocated, max int64, kind string) instOpt {
	return func(r *DBInstanceRecord) {
		r.AllocatedStorage, r.MaxAllocatedStorage, r.StorageType = allocated, max, kind
	}
}
func withStatus(s string) instOpt { return func(r *DBInstanceRecord) { r.DBInstanceStatus = s } }
func withTags(kv map[string]string) instOpt {
	return func(r *DBInstanceRecord) { r.TagList = kv }
}

func rec(id, class, engine string, opts ...instOpt) DBInstanceRecord {
	r := DBInstanceRecord{
		DBInstanceIdentifier: id,
		DBInstanceArn:        "arn:aws:rds:us-east-1:1234:db:" + id,
		DBInstanceClass:      class,
		DBInstanceStatus:     StatusAvailable,
		Engine:               engine,
		EngineVersion:        "16.3",
		LicenseModel:         LicenseGPL,
		AllocatedStorage:     100,
		StorageType:          StorageGP2,
		InstanceCreateTime:   testStart.Add(-90 * 24 * time.Hour),
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

// flat records n datapoints of one value across the test window, at the
// publication granularity a real RDS metric arrives on.
func flat(v float64, n int) []Point {
	return SyntheticMetric(testStart, testWindow().Duration()/time.Duration(n), n, v)
}

// metricsFor builds the recorded CloudWatch answers for one instance. Every
// series this package reads is present, so a test that wants a metric ABSENT
// has to remove it deliberately.
func metricsFor(id string, cpu, conns, freeMemBytes, freeStorageBytes float64) map[string][]Point {
	return map[string][]Point{
		id + "/" + MetricCPUUtilization:   flat(cpu, 48),
		id + "/" + MetricDatabaseConns:    flat(conns, 48),
		id + "/" + MetricFreeableMemory:   flat(freeMemBytes, 48),
		id + "/" + MetricFreeStorageSpace: flat(freeStorageBytes, 48),
		id + "/" + MetricReadIOPS:         flat(120, 48),
		id + "/" + MetricWriteIOPS:        flat(45, 48),
	}
}

func mergeMetrics(ms ...map[string][]Point) map[string][]Point {
	out := map[string][]Point{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// collect runs the fixture through the real collector, which is the only way a
// test gets a snapshot with truthful per-series truncation status.
func collect(t *testing.T, f *Fixture, mutate ...func(*CollectorConfig)) *Snapshot {
	t.Helper()
	cfg := DefaultCollectorConfig(testWindow())
	cfg.Scope, cfg.Region = "1234/us-east-1", "us-east-1"
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := NewCollector(f, f, f, cfg)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return snap
}

// assess runs a snapshot through the sizer and validates the report. Every
// test goes through here so no test can accidentally accept a report that
// violates the package's own invariants.
func assess(t *testing.T, snap *Snapshot, ledger domain.Netter, mutate ...func(*Config)) *Report {
	t.Helper()
	cfg := DefaultConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	s, err := NewSizer(cfg)
	if err != nil {
		t.Fatalf("NewSizer: %v", err)
	}
	rep := s.Assess(testNow, snap, ledger)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report violates its own invariants: %v", err)
	}
	return rep
}

// must fetches an assessment by instance identifier, failing the test when it
// is absent.
func must(t *testing.T, rep *Report, id string) Assessment {
	t.Helper()
	for _, a := range rep.Assessments {
		if a.Instance.Identifier == id {
			return a
		}
	}
	t.Fatalf("no assessment for %q; report has %d", id, len(rep.Assessments))
	return Assessment{}
}

// wantCode fails unless the assessment carries the reason code.
func wantCode(t *testing.T, a Assessment, code string) {
	t.Helper()
	if !a.Suppressed(code) {
		t.Fatalf("%s: want refusal %q, got %v", a.Instance.Identifier, code, a.Codes())
	}
}

// wantNoCode fails when the assessment carries the reason code.
func wantNoCode(t *testing.T, a Assessment, code string) {
	t.Helper()
	if a.Suppressed(code) {
		t.Fatalf("%s: did not want refusal %q, got %v", a.Instance.Identifier, code, a.Codes())
	}
}

// wantAdvisory fails unless the assessment carries the advisory code, and
// returns it.
func wantAdvisory(t *testing.T, a Assessment, code string) Advisory {
	t.Helper()
	for _, ad := range a.Advisories {
		if ad.Code == code {
			return ad
		}
	}
	var got []string
	for _, ad := range a.Advisories {
		got = append(got, ad.Code)
	}
	t.Fatalf("%s: want advisory %q, got %v", a.Instance.Identifier, code, got)
	return Advisory{}
}

// operatorRates returns the default card with operator-supplied rows layered
// on, so a test can exercise the paths an unverified rate refuses.
func operatorRates(t *testing.T, rows map[string]float64) RateCard {
	t.Helper()
	card := DefaultRates()
	for k, v := range rows {
		card.Classes[k] = ClassRate{SingleAZHourlyUSD: v, Provenance: RateOperator}
	}
	card.Storage.Provenance = RateOperator
	if err := card.Validate(); err != nil {
		t.Fatalf("operator rate card: %v", err)
	}
	return card
}

// ledgerWith builds the U12 ledger over a Reserved DB Instance inventory and
// an account-wide baseline. Going through domain.NewLedger rather than calling
// commit directly is the point: it is the same splice cmd/ performs, so a test
// that passes here is a test of the wiring as well as the arithmetic.
func ledgerWith(res []commit.ReservedDBInstance, baseline ...commit.UsageLine) *domain.Ledger {
	inv := &commit.Inventory{ReservedDBs: res}
	return domain.NewLedger(inv, commit.Usage{Lines: baseline})
}
