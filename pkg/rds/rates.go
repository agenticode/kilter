package rds

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// The RDS rate card, and why every number in it is marked unverified.
//
// §2.8 rules out the pkg/pricing/catalog.json route: that catalog is 36 rows of
// {provider, name, family, arch, burstable, milliCPU, memoryBytes, hourlyUSD,
// spotHourlyUSD} with no `db.*` class at all and, more importantly, no place to
// put an engine, an edition or a licence model. A db.r6i.xlarge running SQL
// Server Enterprise license-included and one running PostgreSQL are the same
// hardware at different prices. So RDS gets an embedded table in the
// `fargate.json` style (pkg/pricing/fargate.go): a fixed default, a typed
// override loader that rejects unknown fields, and a price FUNCTION rather than
// a catalog lookup.
//
// The price function is where trap 10 lives:
//
//	A Multi-AZ DB instance is "a synchronous standby replica in a different
//	Availability Zone" and "You can't use a standby replica to serve read
//	traffic" [verified: Concepts.MultiAZSingleStandby.html]. The clean billing
//	evidence is the reserved-instance normalized-unit table, which prices a
//	Multi-AZ deployment at exactly TWICE the Single-AZ units of the same size,
//	and a Multi-AZ cluster at three times [verified:
//	USER_WorkingWithReservedDBInstances.html].
//
// A domain that prices a db.r6i.xlarge at the Single-AZ rate under-reports a
// Multi-AZ instance's cost — and its saving — by half. So deployment is a
// factor in [RateCard.HourlyUSD], not a label on the output, and the factor
// comes from [commit.RDSDeployment.Multiplier] so there is exactly one
// implementation of the ×1/×2/×3 table in the tree.
//
// # Provenance
//
// docs/design/rds-batch-assessment.md §7 is explicit about what was NOT
// verifiable on 2026-08-26:
//
//	[unverified] — do not build on without confirming: RDS on-demand instance
//	rates and Multi-AZ price ratio (the pricing tables are JS-rendered and were
//	not retrievable); RDS gp3 storage, provisioned-IOPS and
//	provisioned-throughput rates; RDS gp2 $/GiB-month (the $0.115 figure
//	appears only in a doc example AWS itself labels "sample prices").
//
// Every rate this package ships therefore carries [RateUnverified], and that
// is encoded as a REFUSAL rather than as a caveat string: an unverified rate
// may size a reported fact and may never become a claimed saving
// ([ReasonUnverifiedRate]). TestEveryShippedRateIsUnverified pins that no
// shipped row can quietly promote itself, and
// TestUnverifiedRatesNeverBecomeASaving pins the consequence.
//
// An operator with the real numbers supplies them through [LoadRates], and
// those rows carry [RateOperator], which unblocks the claim. That is the
// intended path to a dollar figure Kilter will stand behind.

// RateProvenance records where a rate came from and how much weight it can
// carry. It is a field on every rate rather than a property of the table,
// because a card can legitimately mix shipped and operator-supplied rows.
type RateProvenance string

const (
	// RateVerified: read from an AWS page this repository cites. No shipped
	// RDS instance rate has this provenance today; the constant exists so a
	// later unit that verifies the table has somewhere to put the result.
	RateVerified RateProvenance = "verified"
	// RateUnverified: a plausible figure from a secondary route. Sizes a
	// reported fact; never becomes a saving.
	RateUnverified RateProvenance = "unverified"
	// RateOperator: supplied by whoever runs this, who can see their own
	// invoice. Trusted for claims.
	RateOperator RateProvenance = "operator-supplied"
)

// Claimable reports whether a saving may be quoted from a rate of this
// provenance. This is the single gate trap-adjacent code asks; it is a method
// so no caller can re-derive it slightly differently.
func (p RateProvenance) Claimable() bool { return p == RateVerified || p == RateOperator }

// weakest returns the less trustworthy of two provenances, so a figure derived
// from several rates inherits the worst one.
func (p RateProvenance) weakest(o RateProvenance) RateProvenance {
	rank := func(v RateProvenance) int {
		switch v {
		case RateOperator:
			return 3
		case RateVerified:
			return 2
		case RateUnverified:
			return 1
		}
		return 0
	}
	if rank(o) < rank(p) {
		return o
	}
	return p
}

// PriceBand is the pricing identity of an engine: the axis AWS charges
// differently along.
//
// The three open-source engines share one band. That is a MODELLED
// equivalence, not a quoted fact — AWS publishes MariaDB, MySQL and PostgreSQL
// rates separately even where they agree — and it is stated here rather than
// hidden in a table so a reader can disagree with it in one place. It is safe
// in the direction that matters because the whole band is [RateUnverified] and
// therefore cannot produce a claim.
//
// The licensed engines get a band per (family, edition, licence), because that
// is exactly the axis §2.8 says breaks the pure price model. This package
// ships no licensed rows, so every SQL Server, Oracle and Db2 instance is
// refused by name with [ReasonEngineNotPriced] until an operator supplies the
// rows.
func PriceBand(e Engine) string {
	switch e.Family {
	case "":
		return ""
	case FamilyPostgreSQL, FamilyMySQL, FamilyMariaDB:
		return "open-source"
	}
	parts := []string{e.Family}
	if e.Edition != "" {
		parts = append(parts, e.Edition)
	}
	switch e.License {
	case LicenseIncluded:
		parts = append(parts, "li")
	case LicenseBYOL:
		parts = append(parts, "byol")
	}
	return strings.Join(parts, "-")
}

// ClassRate is one Single-AZ, on-demand DB instance-hour.
//
// The field is named SingleAZHourlyUSD rather than HourlyUSD on purpose: a
// caller that reaches for it directly is holding half of a Multi-AZ instance's
// price, and the name says so. [RateCard.HourlyUSD] is the supported way to
// price an instance.
type ClassRate struct {
	SingleAZHourlyUSD float64        `json:"singleAZHourlyUSD"`
	Provenance        RateProvenance `json:"provenance,omitempty"`
}

// StorageRates prices the storage line, which the deployment multiplier does
// NOT touch. The separation is AWS's own: a Reserved DB Instance "doesn't
// provide a discount for the costs associated with storage, backups, and I/O.
// It provides a discount only on the hourly, on-demand instance usage"
// [verified], and this package keeps the same line separation on the cost side
// so the two halves of a bill never contaminate each other.
//
// FINDINGS.md §5 records the one thing this leaves open: whether AWS bills
// Multi-AZ STORAGE at 1× or 2×. The unit follows its specification (§4 trap 10:
// "the multiplier applies to the instance hours and not to storage, backups or
// I/O") and flags the question rather than silently picking the other answer.
type StorageRates struct {
	GP2GiBMonthUSD float64        `json:"gp2GiBMonthUSD,omitempty"`
	GP3GiBMonthUSD float64        `json:"gp3GiBMonthUSD,omitempty"`
	IO1GiBMonthUSD float64        `json:"io1GiBMonthUSD,omitempty"`
	IO2GiBMonthUSD float64        `json:"io2GiBMonthUSD,omitempty"`
	Provenance     RateProvenance `json:"provenance,omitempty"`
}

// GiBMonthUSD returns the monthly rate for a storage type, and false when this
// card cannot price it.
func (s StorageRates) GiBMonthUSD(storageType string) (float64, bool) {
	var v float64
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case StorageGP2:
		v = s.GP2GiBMonthUSD
	case StorageGP3:
		v = s.GP3GiBMonthUSD
	case StorageIO1:
		v = s.IO1GiBMonthUSD
	case StorageIO2:
		v = s.IO2GiBMonthUSD
	default:
		return 0, false
	}
	if !finite(v) || v <= 0 {
		return 0, false
	}
	return v, true
}

// RateCard prices DB instance-hours and storage for one region.
//
// Classes is keyed by rateKey(band, class) — never by class alone, because a
// class alone does not have a price in RDS.
type RateCard struct {
	Region  string               `json:"-"`
	Classes map[string]ClassRate `json:"classes,omitempty"`
	Storage StorageRates         `json:"storage,omitzero"`
}

func rateKey(band, class string) string {
	return band + "|" + strings.ToLower(strings.TrimSpace(class))
}

// shippedClassRates is the embedded table: us-east-1, open-source engines,
// Single-AZ, on-demand, [RateUnverified] every one.
//
// It is deliberately SMALL. §2.8's honest v1 is "ship the engines whose rates
// we have, refuse the rest by name", and the same discipline applies within an
// engine: a class absent from this table is refused with
// [ReasonUnknownInstanceClass] rather than interpolated from its neighbours.
// pkg/ec2 already established that default — "no price ⇒ no bill delta ⇒
// nothing to claim".
//
// The rows are written out one per class rather than generated as
// base × size-factor. RDS on-demand pricing does happen to be close to linear
// in normalized units within a family, but that linearity is itself
// [unverified], and a generated table would hide one assumption inside
// another.
var shippedClassRates = map[string]ClassRate{
	// Burstable, current generation (db.t3, Intel).
	rateKey("open-source", "db.t3.micro"):   {0.017, RateUnverified},
	rateKey("open-source", "db.t3.small"):   {0.034, RateUnverified},
	rateKey("open-source", "db.t3.medium"):  {0.068, RateUnverified},
	rateKey("open-source", "db.t3.large"):   {0.136, RateUnverified},
	rateKey("open-source", "db.t3.xlarge"):  {0.272, RateUnverified},
	rateKey("open-source", "db.t3.2xlarge"): {0.544, RateUnverified},

	// Burstable, Graviton (db.t4g).
	rateKey("open-source", "db.t4g.micro"):   {0.016, RateUnverified},
	rateKey("open-source", "db.t4g.small"):   {0.032, RateUnverified},
	rateKey("open-source", "db.t4g.medium"):  {0.065, RateUnverified},
	rateKey("open-source", "db.t4g.large"):   {0.129, RateUnverified},
	rateKey("open-source", "db.t4g.xlarge"):  {0.258, RateUnverified},
	rateKey("open-source", "db.t4g.2xlarge"): {0.516, RateUnverified},

	// General purpose (db.m5, db.m6i).
	rateKey("open-source", "db.m5.large"):    {0.171, RateUnverified},
	rateKey("open-source", "db.m5.xlarge"):   {0.342, RateUnverified},
	rateKey("open-source", "db.m5.2xlarge"):  {0.684, RateUnverified},
	rateKey("open-source", "db.m5.4xlarge"):  {1.368, RateUnverified},
	rateKey("open-source", "db.m6i.large"):   {0.171, RateUnverified},
	rateKey("open-source", "db.m6i.xlarge"):  {0.342, RateUnverified},
	rateKey("open-source", "db.m6i.2xlarge"): {0.684, RateUnverified},
	rateKey("open-source", "db.m6i.4xlarge"): {1.368, RateUnverified},

	// Memory optimized (db.r5, db.r6i) — where most production RDS lives.
	rateKey("open-source", "db.r5.large"):    {0.240, RateUnverified},
	rateKey("open-source", "db.r5.xlarge"):   {0.480, RateUnverified},
	rateKey("open-source", "db.r5.2xlarge"):  {0.960, RateUnverified},
	rateKey("open-source", "db.r5.4xlarge"):  {1.920, RateUnverified},
	rateKey("open-source", "db.r6i.large"):   {0.240, RateUnverified},
	rateKey("open-source", "db.r6i.xlarge"):  {0.480, RateUnverified},
	rateKey("open-source", "db.r6i.2xlarge"): {0.960, RateUnverified},
	rateKey("open-source", "db.r6i.4xlarge"): {1.920, RateUnverified},

	// Memory optimized, Graviton (db.r7g). Present so the Graviton advisory
	// has a rate to name; the advisory still claims nothing, because whether
	// a database runs the same on Graviton is not observable from any metric
	// (§2.10) and the reservation stranding is total (§2.6).
	rateKey("open-source", "db.r7g.large"):   {0.2158, RateUnverified},
	rateKey("open-source", "db.r7g.xlarge"):  {0.4316, RateUnverified},
	rateKey("open-source", "db.r7g.2xlarge"): {0.8632, RateUnverified},
	rateKey("open-source", "db.r7g.4xlarge"): {1.7264, RateUnverified},
}

// shippedStorageRates are the RDS storage rates, us-east-1.
//
// $0.115/GiB-month for gp2 is the figure §7 flags as appearing "only in a doc
// example AWS itself labels 'sample prices'". It is shipped because a storage
// floor with no dollar on it is not the finding trap 8 asks for — "allocated
// storage is a reported fact with a dollar attached" — and it is shipped as
// [RateUnverified] because a sample price is not a price.
var shippedStorageRates = StorageRates{
	GP2GiBMonthUSD: 0.115,
	GP3GiBMonthUSD: 0.115,
	IO1GiBMonthUSD: 0.125,
	IO2GiBMonthUSD: 0.125,
	Provenance:     RateUnverified,
}

// DefaultRates returns the embedded baseline card. The returned maps are
// copies: a caller that mutated the shipped table would change every other
// caller's prices, and this package holds no mutable state.
func DefaultRates() RateCard {
	c := RateCard{
		Region:  DefaultRegion,
		Classes: make(map[string]ClassRate, len(shippedClassRates)),
		Storage: shippedStorageRates,
	}
	for k, v := range shippedClassRates {
		c.Classes[k] = v
	}
	return c
}

// DefaultRegion is the region every embedded rate here is quoted in, matching
// pkg/pricing's own baseline.
const DefaultRegion = "us-east-1"

// Lookup returns the Single-AZ rate for a class under an engine, and false
// when this card cannot price it.
func (c RateCard) Lookup(class string, e Engine) (ClassRate, bool) {
	band := PriceBand(e)
	if band == "" || commit.RDSClassType(class) == "" {
		return ClassRate{}, false
	}
	r, ok := c.Classes[rateKey(band, class)]
	if !ok || !finite(r.SingleAZHourlyUSD) || r.SingleAZHourlyUSD <= 0 {
		return ClassRate{}, false
	}
	return r, true
}

// PricesBand reports whether this card has ANY row for an engine's price band.
// It is the difference between "we do not price SQL Server at all"
// ([ReasonEngineNotPriced]) and "we price this engine but not this class"
// ([ReasonUnknownInstanceClass]) — two refusals a reader acts on differently.
func (c RateCard) PricesBand(e Engine) bool {
	band := PriceBand(e)
	if band == "" {
		return false
	}
	prefix := band + "|"
	for k := range c.Classes {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// HourlyUSD prices one DB instance-hour: the Single-AZ class rate multiplied
// by the deployment topology's factor.
//
// This is trap 10's fix. The multiplier is ×1 Single-AZ, ×2 Multi-AZ instance,
// ×3 Multi-AZ cluster, read from [commit.RDSDeployment.Multiplier] — the same
// table the reservation arithmetic uses, so a Multi-AZ instance's cost and its
// reservation coverage can never disagree about what topology means.
//
// The multiplier is an exact small integer, so `HourlyUSD(multi) ==
// HourlyUSD(single) * 2` holds to the last bit, which is what
// TestMultiAZBillsTwice asserts to 1e-12.
//
// ok=false when the class, the engine or the topology is unknown. It never
// falls back to the Single-AZ rate for an unknown topology: that would halve
// the bill in exactly the direction this package exists to prevent.
func (c RateCard) HourlyUSD(class string, e Engine, d commit.RDSDeployment) (float64, RateProvenance, bool) {
	r, ok := c.Lookup(class, e)
	if !ok {
		return 0, "", false
	}
	m, ok := d.Multiplier()
	if !ok {
		return 0, "", false
	}
	return r.SingleAZHourlyUSD * m, r.Provenance, true
}

// StorageMonthlyUSD prices an allocated-storage line. The deployment
// multiplier is deliberately NOT applied — see [StorageRates].
func (c RateCard) StorageMonthlyUSD(storageType string, gib int64) (float64, RateProvenance, bool) {
	if gib <= 0 {
		return 0, "", false
	}
	rate, ok := c.Storage.GiBMonthUSD(storageType)
	if !ok {
		return 0, "", false
	}
	prov := c.Storage.Provenance
	if prov == "" {
		prov = RateUnverified
	}
	return float64(gib) * rate, prov, true
}

// Validate reports the first structural defect in a card. Called by
// [LoadRates] and by [NewSizer], so a bad override fails at the boundary
// rather than producing a quietly wrong report.
func (c RateCard) Validate() error {
	keys := make([]string, 0, len(c.Classes))
	for k := range c.Classes {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error message
	for _, k := range keys {
		r := c.Classes[k]
		band, class, ok := strings.Cut(k, "|")
		switch {
		case !ok || band == "" || class == "":
			return fmt.Errorf("rds: rate key %q is not \"<band>|<db.class.size>\"", k)
		case commit.RDSClassType(class) == "":
			return fmt.Errorf("rds: rate key %q names %q, which is not a DB instance class", k, class)
		case !finite(r.SingleAZHourlyUSD) || r.SingleAZHourlyUSD <= 0:
			return fmt.Errorf("rds: rate %q has a non-positive singleAZHourlyUSD %v", k, r.SingleAZHourlyUSD)
		case r.Provenance == "":
			return fmt.Errorf("rds: rate %q has no provenance; an unattributed rate is one nobody can "+
				"decide whether to trust", k)
		case !knownProvenance(r.Provenance):
			return fmt.Errorf("rds: rate %q has unknown provenance %q", k, r.Provenance)
		}
	}
	s := c.Storage
	for name, v := range map[string]float64{
		"gp2GiBMonthUSD": s.GP2GiBMonthUSD, "gp3GiBMonthUSD": s.GP3GiBMonthUSD,
		"io1GiBMonthUSD": s.IO1GiBMonthUSD, "io2GiBMonthUSD": s.IO2GiBMonthUSD,
	} {
		if !finite(v) || v < 0 {
			return fmt.Errorf("rds: storage rate %s is %v", name, v)
		}
	}
	if s.Provenance != "" && !knownProvenance(s.Provenance) {
		return fmt.Errorf("rds: storage rates have unknown provenance %q", s.Provenance)
	}
	return nil
}

func knownProvenance(p RateProvenance) bool {
	return p == RateVerified || p == RateUnverified || p == RateOperator
}

// rateFile is the on-disk override shape. It is separate from [RateCard] so
// the JSON contract is explicit and so `region` can be a first-class field
// rather than something a caller has to remember to set.
type rateFile struct {
	Region  string                 `json:"region,omitempty"`
	Classes map[string]rateFileRow `json:"classes"`
	Storage *StorageRates          `json:"storage,omitempty"`
}

// rateFileRow is one row of the override file, keyed by "<band>|<class>".
type rateFileRow struct {
	SingleAZHourlyUSD float64 `json:"singleAZHourlyUSD"`
}

// LoadRates parses a rate override, mirroring how pkg/pricing parses a Fargate
// rate file: unknown fields are REJECTED, so an override that tries to
// introduce a `multiAZHourlyUSD` column fails loudly instead of being silently
// ignored — the multiplier is a property of the price function and an override
// must not be able to contradict it (trap 10).
//
// Every loaded row is stamped [RateOperator]. That is the point of the file:
// an operator can see their own invoice, and a rate they supply is one this
// package will stand behind in a claim.
func LoadRates(r io.Reader) (RateCard, error) {
	var f rateFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return RateCard{}, fmt.Errorf("rds: parse rate card: %w", err)
	}
	if len(f.Classes) == 0 {
		return RateCard{}, fmt.Errorf("rds: rate card declares no classes")
	}
	out := RateCard{Region: f.Region, Classes: make(map[string]ClassRate, len(f.Classes))}
	for k, row := range f.Classes {
		out.Classes[k] = ClassRate{SingleAZHourlyUSD: row.SingleAZHourlyUSD, Provenance: RateOperator}
	}
	if f.Storage != nil {
		out.Storage = *f.Storage
		out.Storage.Provenance = RateOperator
	}
	if err := out.Validate(); err != nil {
		return RateCard{}, err
	}
	return out, nil
}

// LoadRatesFile loads a rate override from disk.
func LoadRatesFile(path string) (RateCard, error) {
	f, err := os.Open(path)
	if err != nil {
		return RateCard{}, err
	}
	defer f.Close()
	return LoadRates(f)
}

// Merge returns a copy of c with over's rows layered on top. Used to let an
// operator supply the SQL Server rows this package does not ship without
// having to restate every open-source row.
func (c RateCard) Merge(over RateCard) RateCard {
	out := RateCard{Region: c.Region, Storage: c.Storage,
		Classes: make(map[string]ClassRate, len(c.Classes)+len(over.Classes))}
	for k, v := range c.Classes {
		out.Classes[k] = v
	}
	for k, v := range over.Classes {
		out.Classes[k] = v
	}
	if over.Region != "" {
		out.Region = over.Region
	}
	if len(over.Classes) > 0 && over.Storage.Provenance != "" {
		out.Storage = over.Storage
	}
	return out
}

// --- Class shapes ----------------------------------------------------------

// ClassShape is the hardware behind a DB instance class. It is NOT a price and
// is not used to derive one: it exists so a report can say "the low-water
// FreeableMemory was 12 GiB out of 32 GiB" instead of quoting a bare byte
// count, and so pkg/domain's canonical Resources dimensions can be filled.
type ClassShape struct {
	VCPU        int   `json:"vcpu"`
	MemoryBytes int64 `json:"memoryBytes"`
	// Burstable marks the credit-based families, whose CPU metrics mean
	// something different and whose credit series publish at 5 minutes only.
	Burstable bool `json:"burstable,omitempty"`
}

const gibibyte int64 = 1 << 30

// classShapes covers exactly the classes [shippedClassRates] prices. A class
// with a rate and no shape would render a report line with a blank denominator;
// a class with a shape and no rate would tempt someone to price it.
// TestEveryPricedClassHasAShape keeps the two tables in step.
var classShapes = map[string]ClassShape{
	"db.t3.micro":   {2, 1 * gibibyte, true},
	"db.t3.small":   {2, 2 * gibibyte, true},
	"db.t3.medium":  {2, 4 * gibibyte, true},
	"db.t3.large":   {2, 8 * gibibyte, true},
	"db.t3.xlarge":  {4, 16 * gibibyte, true},
	"db.t3.2xlarge": {8, 32 * gibibyte, true},

	"db.t4g.micro":   {2, 1 * gibibyte, true},
	"db.t4g.small":   {2, 2 * gibibyte, true},
	"db.t4g.medium":  {2, 4 * gibibyte, true},
	"db.t4g.large":   {2, 8 * gibibyte, true},
	"db.t4g.xlarge":  {4, 16 * gibibyte, true},
	"db.t4g.2xlarge": {8, 32 * gibibyte, true},

	"db.m5.large":   {2, 8 * gibibyte, false},
	"db.m5.xlarge":  {4, 16 * gibibyte, false},
	"db.m5.2xlarge": {8, 32 * gibibyte, false},
	"db.m5.4xlarge": {16, 64 * gibibyte, false},

	"db.m6i.large":   {2, 8 * gibibyte, false},
	"db.m6i.xlarge":  {4, 16 * gibibyte, false},
	"db.m6i.2xlarge": {8, 32 * gibibyte, false},
	"db.m6i.4xlarge": {16, 64 * gibibyte, false},

	"db.r5.large":   {2, 16 * gibibyte, false},
	"db.r5.xlarge":  {4, 32 * gibibyte, false},
	"db.r5.2xlarge": {8, 64 * gibibyte, false},
	"db.r5.4xlarge": {16, 128 * gibibyte, false},

	"db.r6i.large":   {2, 16 * gibibyte, false},
	"db.r6i.xlarge":  {4, 32 * gibibyte, false},
	"db.r6i.2xlarge": {8, 64 * gibibyte, false},
	"db.r6i.4xlarge": {16, 128 * gibibyte, false},

	"db.r7g.large":   {2, 16 * gibibyte, false},
	"db.r7g.xlarge":  {4, 32 * gibibyte, false},
	"db.r7g.2xlarge": {8, 64 * gibibyte, false},
	"db.r7g.4xlarge": {16, 128 * gibibyte, false},
}

// ShapeOf returns the hardware behind a DB instance class, and false when this
// package has no row for it.
func ShapeOf(class string) (ClassShape, bool) {
	s, ok := classShapes[strings.ToLower(strings.TrimSpace(class))]
	return s, ok
}

// finite maps NaN and ±Inf to false. Garbage arithmetic must not be able to
// travel into a savings claim.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// zeroIfNotFinite is the value-returning form.
func zeroIfNotFinite(f float64) float64 {
	if !finite(f) {
		return 0
	}
	return f
}
