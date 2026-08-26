package rds

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
	krds "github.com/agenticode/kilter/pkg/rds"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func newTestDomain(t *testing.T) *Domain {
	t.Helper()
	d, err := New(Config{Scope: "000000000000/us-east-1", Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func snapshotWith(insts ...krds.DBInstance) *krds.Snapshot {
	w := krds.Window{Start: testNow.Add(-14 * 24 * time.Hour), End: testNow}
	s := &krds.Snapshot{
		Domain: krds.Kind, Scope: "000000000000/us-east-1", Region: "us-east-1",
		Timestamp: testNow, Window: w,
	}
	for _, in := range insts {
		s.Targets = append(s.Targets, krds.Target{
			Ref:      domain.TargetRef{Domain: domain.RDS, Scope: s.Scope, ID: in.ARN, Name: in.Identifier},
			Instance: in,
		})
	}
	krds.SortTargets(s.Targets)
	return s
}

func instance(id, class, engine string, multiAZ bool) krds.DBInstance {
	return krds.DBInstance{
		ARN: "arn:aws:rds:us-east-1:000000000000:db:" + id, Identifier: id,
		Class: class, Engine: engine, LicenseModel: krds.LicenseGPL,
		Status: krds.StatusAvailable, Region: "us-east-1", MultiAZ: multiAZ,
		AllocatedStorageGiB: 100, StorageType: krds.StorageGP2,
	}
}

// TestRegistersAndStaysReportOnly: the kind is registrable now, and
// registering it grants nothing.
func TestRegistersAndStaysReportOnly(t *testing.T) {
	d := newTestDomain(t)
	if d.Kind() != domain.RDS {
		t.Fatalf("Kind() = %q, want %q", d.Kind(), domain.RDS)
	}
	reg := domain.NewRegistry()
	if err := reg.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.CanActuate(domain.RDS) {
		t.Error("registering the domain wired an actuator")
	}
	if h := d.Health(testNow); !h.ReportOnly {
		t.Error("the rds domain is not report-only")
	}
	if _, err := reg.PlanSteps(domain.RDS, nil, domain.Guard{Now: testNow}); err == nil {
		t.Error("the core planned steps for a report-only domain")
	}
	if _, ok := any(d).(domain.Refuser); !ok {
		t.Error("the adapter does not implement Refuser; the refusals ARE this domain's output")
	}
}

// TestUsageLinesCarryTheDeploymentMultiplier: the Multi-AZ line costs exactly
// twice the Single-AZ line for the same class, because both come from the same
// multiplier that the reservation arithmetic uses.
func TestUsageLinesCarryTheDeploymentMultiplier(t *testing.T) {
	d := newTestDomain(t)
	if err := d.Observe(snapshotWith(
		instance("single", "db.r6i.xlarge", "postgres", false),
		instance("multi", "db.r6i.xlarge", "postgres", true),
	)); err != nil {
		t.Fatal(err)
	}
	byID := map[string]commit.UsageLine{}
	for _, l := range d.UsageLines(testNow, nil) {
		byID[l.ID] = l
	}
	if len(byID) != 2 {
		t.Fatalf("got %d usage lines, want 2: %v", len(byID), byID)
	}
	var single, multi commit.UsageLine
	for id, l := range byID {
		if l.Kind != commit.KindRDS {
			t.Errorf("%s: kind %q, want %q", id, l.Kind, commit.KindRDS)
		}
		if l.Deployment == commit.RDSMultiAZInstance {
			multi = l
		} else {
			single = l
		}
	}
	if single.ODRate <= 0 || multi.ODRate != 2*single.ODRate {
		t.Errorf("multi $%v, single $%v — want exactly 2×", multi.ODRate, single.ODRate)
	}
}

// TestUnpricedInstancesNeverEnterTheBaseline.
//
// A baseline line for an instance nobody could price carries a rate of zero,
// and a zero-rate line makes a Reserved DB Instance look like it is absorbing
// usage that costs nothing — which overstates absorption and therefore
// overstates every OTHER domain's saving. Under-claiming is the only safe way
// to be wrong here.
func TestUnpricedInstancesNeverEnterTheBaseline(t *testing.T) {
	aurora := instance("aurora-1", "db.r6i.large", "aurora-postgresql", false)
	aurora.ClusterID = "aurora-prod"
	unknownEngine := instance("weird-1", "db.r6i.large", "cockroach", false)
	unpricedClass := instance("odd-1", "db.zz9.plural-z-alpha", "postgres", false)
	licensed := instance("mssql-1", "db.r6i.xlarge", "sqlserver-ee", false)
	licensed.LicenseModel = krds.LicenseIncluded
	priced := instance("ok-1", "db.r6i.large", "postgres", false)

	d := newTestDomain(t)
	if err := d.Observe(snapshotWith(aurora, unknownEngine, unpricedClass, licensed, priced)); err != nil {
		t.Fatal(err)
	}
	lines := d.UsageLines(testNow, nil)
	if len(lines) != 1 {
		t.Fatalf("got %d usage lines, want only the priced one: %+v", len(lines), lines)
	}
	if lines[0].ID != priced.ARN+"/instance" {
		t.Errorf("the wrong instance reached the baseline: %s", lines[0].ID)
	}
	// And every refusal is still reported: excluded is not invisible.
	if got := len(d.Refusals(testNow, nil)); got < 4 {
		t.Errorf("%d refusals for 5 instances; an excluded instance must still be reported", got)
	}
}

// TestNothingLearnedProducesNoLinesRatherThanZeroes.
func TestNothingLearnedProducesNoLinesRatherThanZeroes(t *testing.T) {
	d := newTestDomain(t)
	if lines := d.UsageLines(testNow, nil); lines != nil {
		t.Errorf("a domain with no snapshot produced %d baseline lines", len(lines))
	}
	if recs := d.Recommend(testNow, nil); len(recs) != 0 {
		t.Errorf("a domain that proposes nothing produced %d recommendations", len(recs))
	}
}

// TestOperatorRatesAreLayeredNotReplaced: an operator supplying only the
// licensed rows must not lose the open-source ones.
func TestOperatorRatesAreLayeredNotReplaced(t *testing.T) {
	card := krds.DefaultRates()
	base, _, ok := card.HourlyUSD("db.r6i.large", krds.ParseEngine("postgres", krds.LicenseGPL), commit.RDSSingleAZ)
	if !ok {
		t.Fatal("the shipped card cannot price a db.r6i.large PostgreSQL instance")
	}
	d, err := New(Config{Scope: "s", Region: "us-east-1", Rates: card})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Observe(snapshotWith(instance("ok-1", "db.r6i.large", "postgres", false))); err != nil {
		t.Fatal(err)
	}
	lines := d.UsageLines(testNow, nil)
	if len(lines) != 1 || lines[0].ODRate != base {
		t.Errorf("rate %v, want the card's %v", lines, base)
	}
}
