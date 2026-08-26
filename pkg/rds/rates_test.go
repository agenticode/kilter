package rds

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Every rate this package ships is [RateUnverified], and that fact is
// enforced rather than documented. §7 of the design doc could not retrieve
// AWS's RDS pricing tables, and a rate nobody verified must not be able to
// promote itself into a saving by being edited without a second thought.
func TestEveryShippedRateIsUnverified(t *testing.T) {
	card := DefaultRates()
	if len(card.Classes) == 0 {
		t.Fatal("the shipped card prices nothing")
	}
	for key, r := range card.Classes {
		if r.Provenance != RateUnverified {
			t.Errorf("shipped rate %q claims provenance %q. docs/design/rds-batch-assessment.md §7 marks "+
				"RDS on-demand rates unverified; promoting one requires citing the AWS page it came from "+
				"and saying so here", key, r.Provenance)
		}
		if r.Provenance.Claimable() {
			t.Errorf("shipped rate %q is claimable", key)
		}
	}
	if card.Storage.Provenance != RateUnverified {
		t.Errorf("shipped storage rates claim provenance %q; the $0.115 gp2 figure appears only in an "+
			"example AWS itself labels \"sample prices\"", card.Storage.Provenance)
	}
	if err := card.Validate(); err != nil {
		t.Fatalf("the shipped card does not satisfy its own validator: %v", err)
	}

	// The provenance ladder itself.
	if !RateOperator.Claimable() || !RateVerified.Claimable() {
		t.Error("an operator-supplied or verified rate must be claimable")
	}
	if RateUnverified.Claimable() || RateProvenance("").Claimable() {
		t.Error("an unverified or unattributed rate must never be claimable")
	}
	if got := RateOperator.weakest(RateUnverified); got != RateUnverified {
		t.Errorf("weakest(operator, unverified) = %q, want unverified: a figure derived from several "+
			"rates inherits the worst one", got)
	}
	if got := RateUnverified.weakest(RateOperator); got != RateUnverified {
		t.Errorf("weakest is not symmetric: got %q", got)
	}
}

// Mutating the card a caller receives must not change anyone else's prices.
// This package holds no mutable state, and DefaultRates handing out the
// shipped map would quietly break that.
func TestDefaultRatesHandsOutACopy(t *testing.T) {
	a := DefaultRates()
	key := rateKey("open-source", "db.r6i.large")
	before := a.Classes[key]
	a.Classes[key] = ClassRate{SingleAZHourlyUSD: 999, Provenance: RateOperator}
	a.Classes["open-source|db.new.large"] = ClassRate{SingleAZHourlyUSD: 1, Provenance: RateOperator}

	b := DefaultRates()
	if b.Classes[key] != before {
		t.Fatal("mutating one DefaultRates card changed the next one; the shipped table is shared")
	}
	if _, added := b.Classes["open-source|db.new.large"]; added {
		t.Fatal("a row added to one card appeared in the next")
	}
}

// The price band is the axis AWS charges differently along, and the licensed
// engines are separated from the open-source ones — §2.8's point.
func TestPriceBandSeparatesLicensedEngines(t *testing.T) {
	for _, tc := range []struct {
		engine, license, want string
	}{
		{"postgres", LicenseGPL, "open-source"},
		{"mysql", LicenseGPL, "open-source"},
		{"mariadb", "", "open-source"},
		{"sqlserver-ee", LicenseIncluded, "sqlserver-ee-li"},
		{"sqlserver-se", LicenseIncluded, "sqlserver-se-li"},
		{"oracle-ee", LicenseBYOL, "oracle-ee-byol"},
		{"oracle-ee", LicenseIncluded, "oracle-ee-li"},
		{"oracle-se2-cdb", LicenseBYOL, "oracle-se2-byol"},
		{"db2-ae", LicenseBYOL, "db2-ae-byol"},
		{"nonesuch", "", ""},
	} {
		got := PriceBand(ParseEngine(tc.engine, tc.license))
		if got != tc.want {
			t.Errorf("PriceBand(%q, %q) = %q, want %q", tc.engine, tc.license, got, tc.want)
		}
	}

	// A SQL Server instance is not priced from the open-source band, however
	// convenient that would be.
	card := DefaultRates()
	mssql := ParseEngine("sqlserver-ee", LicenseIncluded)
	if _, _, ok := card.HourlyUSD("db.r6i.xlarge", mssql, commit.RDSSingleAZ); ok {
		t.Fatal("a SQL Server instance was priced from the open-source rate table; the same hardware " +
			"costs different amounts under different engines and licences")
	}
	if card.PricesBand(mssql) {
		t.Error("PricesBand reported rows for a band this package ships none for")
	}
	if !card.PricesBand(ParseEngine("postgres", LicenseGPL)) {
		t.Error("PricesBand reported no rows for the band the shipped table is entirely made of")
	}
}

// An override file rejects unknown fields, so a file that tries to introduce a
// Multi-AZ column fails loudly rather than being silently ignored — the
// multiplier belongs to the price function (trap 10) and an override must not
// be able to contradict it.
func TestLoadRatesRejectsUnknownFieldsAndStampsProvenance(t *testing.T) {
	good := `{"region":"eu-west-1","classes":{"open-source|db.r6i.large":{"singleAZHourlyUSD":0.31}},
		"storage":{"gp2GiBMonthUSD":0.13,"gp3GiBMonthUSD":0.125}}`
	card, err := LoadRates(strings.NewReader(good))
	if err != nil {
		t.Fatalf("LoadRates on a valid file: %v", err)
	}
	if card.Region != "eu-west-1" {
		t.Errorf("region = %q", card.Region)
	}
	r, ok := card.Lookup("db.r6i.large", ParseEngine("postgres", LicenseGPL))
	if !ok || r.SingleAZHourlyUSD != 0.31 {
		t.Fatalf("loaded rate = %+v", r)
	}
	if r.Provenance != RateOperator || !r.Provenance.Claimable() {
		t.Errorf("a loaded rate carries provenance %q, want %q: an operator can see their own invoice",
			r.Provenance, RateOperator)
	}
	if card.Storage.Provenance != RateOperator {
		t.Errorf("loaded storage rates carry %q", card.Storage.Provenance)
	}

	for _, bad := range []struct{ name, body string }{
		{"multi-az column", `{"classes":{"open-source|db.r6i.large":
			{"singleAZHourlyUSD":0.31,"multiAZHourlyUSD":0.62}}}`},
		{"unknown top-level field", `{"rates":{},"classes":{"open-source|db.r6i.large":
			{"singleAZHourlyUSD":0.31}}}`},
		{"no classes", `{"region":"us-east-1"}`},
		{"malformed key", `{"classes":{"db.r6i.large":{"singleAZHourlyUSD":0.31}}}`},
		{"not a db class", `{"classes":{"open-source|m5.large":{"singleAZHourlyUSD":0.31}}}`},
		{"zero rate", `{"classes":{"open-source|db.r6i.large":{"singleAZHourlyUSD":0}}}`},
		{"negative rate", `{"classes":{"open-source|db.r6i.large":{"singleAZHourlyUSD":-1}}}`},
		{"not json", `nope`},
	} {
		if _, err := LoadRates(strings.NewReader(bad.body)); err == nil {
			t.Errorf("LoadRates accepted %s", bad.name)
		}
	}
}

// Merge layers operator rows over the shipped table without restating it, and
// the merged rows keep their own provenance.
func TestMergeLayersOperatorRowsOverShippedOnes(t *testing.T) {
	base := DefaultRates()
	over, err := LoadRates(strings.NewReader(
		`{"classes":{"sqlserver-ee-li|db.r6i.large":{"singleAZHourlyUSD":1.6},
		  "open-source|db.r6i.large":{"singleAZHourlyUSD":0.25}}}`))
	if err != nil {
		t.Fatal(err)
	}
	merged := base.Merge(over)
	if err := merged.Validate(); err != nil {
		t.Fatal(err)
	}
	pg := ParseEngine("postgres", LicenseGPL)
	mssql := ParseEngine("sqlserver-ee", LicenseIncluded)

	if r, _ := merged.Lookup("db.r6i.large", pg); r.SingleAZHourlyUSD != 0.25 || !r.Provenance.Claimable() {
		t.Errorf("the operator row did not override the shipped one: %+v", r)
	}
	if r, ok := merged.Lookup("db.r6i.xlarge", pg); !ok || r.Provenance != RateUnverified {
		t.Errorf("a shipped row not in the override lost its provenance: %+v", r)
	}
	if r, ok := merged.Lookup("db.r6i.large", mssql); !ok || r.SingleAZHourlyUSD != 1.6 {
		t.Errorf("the override did not add the licensed band: %+v", r)
	}
	// The base is untouched.
	if base.PricesBand(mssql) {
		t.Error("Merge mutated the card it was called on")
	}
}

// A class this package prices must have a shape, and a class with a shape must
// be priced. A rate with no shape renders a report line with a blank
// denominator; a shape with no rate tempts someone to price it.
func TestEveryPricedClassHasAShape(t *testing.T) {
	card := DefaultRates()
	priced := map[string]bool{}
	for key := range card.Classes {
		_, class, _ := strings.Cut(key, "|")
		priced[class] = true
		if _, ok := ShapeOf(class); !ok {
			t.Errorf("class %q has a rate and no shape", class)
		}
	}
	for class := range classShapes {
		if !priced[class] {
			t.Errorf("class %q has a shape and no rate", class)
		}
	}
	// And the shapes are plausible: RDS classes carry at least 1 GiB and at
	// least one vCPU, and the memory-optimized families carry more memory per
	// vCPU than the general-purpose ones.
	for class, sh := range classShapes {
		if sh.VCPU < 1 || sh.MemoryBytes < gibibyte {
			t.Errorf("class %q has an implausible shape %+v", class, sh)
		}
	}
	r, _ := ShapeOf("db.r6i.xlarge")
	m, _ := ShapeOf("db.m6i.xlarge")
	if r.MemoryBytes <= m.MemoryBytes {
		t.Error("the memory-optimized family does not carry more memory than the general-purpose one")
	}
	if _, ok := ShapeOf("db.nonesuch.large"); ok {
		t.Error("ShapeOf invented a shape for an unknown class")
	}
}

// The storage line is priced independently of the deployment multiplier, and
// an unknown storage type is refused rather than priced at zero.
func TestStorageRatesAreSeparateFromTheInstanceLine(t *testing.T) {
	card := DefaultRates()
	for _, kind := range []string{StorageGP2, StorageGP3, StorageIO1, StorageIO2} {
		v, prov, ok := card.StorageMonthlyUSD(kind, 100)
		if !ok || v <= 0 {
			t.Errorf("%s of 100 GiB priced at %v (ok=%v)", kind, v, ok)
		}
		if prov.Claimable() {
			t.Errorf("%s storage rate claims to be verified", kind)
		}
	}
	if _, _, ok := card.StorageMonthlyUSD(StorageMagnetic, 100); ok {
		t.Error("magnetic storage was priced; this package ships no rate for it")
	}
	if _, _, ok := card.StorageMonthlyUSD("", 100); ok {
		t.Error("an unset storage type was priced")
	}
	if _, _, ok := card.StorageMonthlyUSD(StorageGP2, 0); ok {
		t.Error("zero GiB was priced")
	}
	// Linear in size, so a report line and a fleet total agree.
	one, _, _ := card.StorageMonthlyUSD(StorageGP2, 100)
	two, _, _ := card.StorageMonthlyUSD(StorageGP2, 200)
	if math.Abs(two-2*one) > 1e-12 {
		t.Errorf("storage pricing is not linear: %v vs %v", two, 2*one)
	}
}

// Engine parsing: the identity three different subsystems key off.
func TestParseEngineIdentity(t *testing.T) {
	for _, tc := range []struct {
		engine, license           string
		family, edition, commitID string
		flexible                  bool
	}{
		{"postgres", LicenseGPL, FamilyPostgreSQL, "", "postgresql", true},
		{"POSTGRES", "", FamilyPostgreSQL, "", "postgresql", true},
		{"mysql", "", FamilyMySQL, "", "mysql", true},
		{"mariadb", "", FamilyMariaDB, "", "mariadb", true},
		{"db2-se", LicenseBYOL, FamilyDb2, "se", "db2-se", true},
		{"sqlserver-ee", LicenseIncluded, FamilySQLServer, "ee", "sqlserver-ee(li)", false},
		{"sqlserver-web", LicenseIncluded, FamilySQLServer, "web", "sqlserver-web(li)", false},
		{"oracle-ee", LicenseIncluded, FamilyOracle, "ee", "oracle-ee(li)", false},
		{"oracle-ee", LicenseBYOL, FamilyOracle, "ee", "oracle-ee(byol)", true},
		{"oracle-se2-cdb", LicenseBYOL, FamilyOracle, "se2", "oracle-se2(byol)", true},
		{"oracle-se2", "", FamilyOracle, "se2", "oracle-se2", false}, // ambiguous ⇒ not flexible
		{"aurora-postgresql", "", FamilyAurora, "postgresql", "aurora-postgresql", false},
		{"cockroachdb", "", "", "", "cockroachdb", false},
		{"", "", "", "", "", false},
	} {
		e := ParseEngine(tc.engine, tc.license)
		if e.Family != tc.family {
			t.Errorf("ParseEngine(%q).Family = %q, want %q", tc.engine, e.Family, tc.family)
		}
		if e.Edition != tc.edition {
			t.Errorf("ParseEngine(%q).Edition = %q, want %q", tc.engine, e.Edition, tc.edition)
		}
		if got := e.CommitEngine(); got != tc.commitID {
			t.Errorf("ParseEngine(%q, %q).CommitEngine() = %q, want %q",
				tc.engine, tc.license, got, tc.commitID)
		}
		if got := e.SizeFlexible(); got != tc.flexible {
			t.Errorf("ParseEngine(%q, %q).SizeFlexible() = %v, want %v",
				tc.engine, tc.license, got, tc.flexible)
		}
	}

	// The size-flexibility answer is U12's, not a second implementation here.
	for _, e := range []Engine{
		ParseEngine("sqlserver-ee", LicenseIncluded),
		ParseEngine("oracle-ee", LicenseIncluded),
		ParseEngine("postgres", LicenseGPL),
	} {
		if e.SizeFlexible() != commit.RDSSizeFlexibleEngine(e.CommitEngine()) {
			t.Errorf("%s: this package disagrees with pkg/pricing/commit about size flexibility", e)
		}
	}
}

// The memory policy is a table with a refusing default, so adding an engine is
// a deliberate act rather than a fallthrough.
func TestMemorySemanticsDefaultToRefusal(t *testing.T) {
	for _, tc := range []struct {
		engine string
		want   MemorySemantics
	}{
		{"postgres", MemPageCacheDominant},
		{"mysql", MemAnonymousPool},
		{"mariadb", MemAnonymousPool},
		{"oracle-se2", MemUnencoded},
		{"sqlserver-ee", MemUnencoded},
		{"db2-ae", MemUnencoded},
		{"aurora-mysql", MemUnencoded},
		{"nonesuch", MemUnencoded},
		{"", MemUnencoded},
	} {
		if got := MemorySemanticsFor(ParseEngine(tc.engine, "")); got != tc.want {
			t.Errorf("MemorySemanticsFor(%q) = %q, want %q", tc.engine, got, tc.want)
		}
	}
	// Only the two engines whose buffer semantics the doc states are encoded.
	if len(memorySemantics) != 3 {
		t.Errorf("the memory policy has %d rows; adding one is a deliberate act that needs its own "+
			"justification and its own test", len(memorySemantics))
	}

	// An unusable series never produces a headroom number on any engine.
	for _, e := range []string{"postgres", "mysql", "oracle-se2"} {
		v := AssessMemory(ParseEngine(e, ""), Series{Partial: true, Status: StatusTruncated}, 32*float64(gibibyte))
		if v.Readable || v.MinFreeBytes != 0 {
			t.Errorf("%s: a truncated series produced a headroom reading %+v", e, v)
		}
		if v.Code != ReasonTruncatedMetrics {
			t.Errorf("%s: a truncated series was refused as %q", e, v.Code)
		}
		empty := AssessMemory(ParseEngine(e, ""), Series{Status: StatusComplete}, 0)
		if empty.Code != ReasonNoMetricEvidence {
			t.Errorf("%s: an empty series was refused as %q", e, empty.Code)
		}
	}

	// With no class shape, the fraction is omitted rather than divided by a
	// guess.
	v := AssessMemory(ParseEngine("mysql", ""),
		SyntheticSeries(MetricFreeableMemory, testStart, time.Hour, 8e9, 9e9), 0)
	if !v.Readable || v.MinFreeBytes != 8e9 {
		t.Fatalf("MySQL verdict = %+v", v)
	}
	if v.FreeFraction != 0 {
		t.Errorf("a free fraction was computed with no class memory size: %v", v.FreeFraction)
	}
}
