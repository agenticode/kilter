package commit

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reserved DB Instances — the fourth commitment product.
//
// An RDS DB instance hour is not an EC2 instance hour and its commitment is
// not a Reserved Instance. Three differences change the arithmetic, and each
// is load-bearing here [all verified:
// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_WorkingWithReservedDBInstances.html]:
//
//   - "Size-flexible reserved DB instances can only scale in their instance
//     class type. For example, a reserved DB instance for a db.r6i.large can
//     apply to a db.r6i.xlarge, but not to a db.r6id.large or a db.r7g.large,
//     because db.r6id.large and db.r7g.large are different instance class
//     types." The EC2 notion of a *family* is too coarse: db.r6i and db.r6id
//     share a family prefix in every human sense and share no discount.
//   - "Size flexibility does not apply to RDS for SQL Server and RDS for
//     Oracle License Included." Flexibility is gated on the engine, so the
//     same downsize is half-absorbed on PostgreSQL and 100 % stranded on SQL
//     Server. See [RDSSizeFlexibleEngine].
//   - "Reserved DB instance benefits apply to both Multi-AZ and Single-AZ
//     configurations… you can move from a Single-AZ deployment running on one
//     large DB instance (four normalized units per hour) to a Multi-AZ
//     deployment running on two medium DB instances (2+2 = 4 normalized units
//     per hour)." Deployment topology is a factor on the unit table, not a
//     separate product. See [RDSNormalizationUnits].
//
// And the one that is decisive but is not on that page: **Compute Savings
// Plans do not cover RDS.** Neither does an EC2 Instance Savings Plan, nor an
// EC2 Reserved Instance. A [KindRDS] line is therefore absorbed by a
// [ReservedDBInstance] and by nothing else — pinned by
// TestComputeSPNeverAbsorbsRDS.
//
// Two further verbatim clauses shape what may appear as a KindRDS line at
// all:
//
//   - "The price for a reserved DB instance doesn't provide a discount for
//     the costs associated with storage, backups, and I/O. It provides a
//     discount only on the hourly, on-demand instance usage." A KindRDS line
//     is instance-hours, full stop; [Usage.Validate] requires a DB instance
//     class on every one of them, so a storage or I/O line is structurally
//     unable to become a covered line (TestRDSStorageIsNotExpressibleAsCoveredUsage).
//   - "You can't cancel a reserved DB instance." Stranding here is permanent
//     until expiry, which is exactly what [Assessment.ValidFrom] dates.

// RDSDeployment is the deployment topology of a DB instance. It multiplies
// the size's normalized units: a Multi-AZ instance costs — and absorbs —
// twice its Single-AZ self, a Multi-AZ cluster three times.
//
// The zero value is deliberately NOT Single-AZ. Guessing Single-AZ for a
// collector that did not report the topology would halve the units a line
// needs, so a reservation would appear to cover twice the usage it really
// covers: optimistic, in the one direction this package exists to prevent.
// [Usage.Validate] rejects an unset deployment loudly; [Inventory.Bill],
// which never fails, treats it as unknown and lets the line fall through to
// exact-match coverage or on-demand. Contrast [NormalizePlatform], where an
// empty value folds to Linux/UNIX because there the guess only ever grants
// coverage AWS would also have granted.
type RDSDeployment string

const (
	RDSSingleAZ        RDSDeployment = "single-az"
	RDSMultiAZInstance RDSDeployment = "multi-az-instance"
	RDSMultiAZCluster  RDSDeployment = "multi-az-cluster"
)

// NormalizeRDSDeployment folds equivalent topology spellings so that matching
// is not a string-equality coin flip. An unrecognized value is lower-cased and
// passed through, so it matches only itself and carries no unit factor.
func NormalizeRDSDeployment(d RDSDeployment) RDSDeployment {
	k := strings.ToLower(strings.TrimSpace(string(d)))
	k = strings.NewReplacer("_", "-", " ", "-").Replace(k)
	switch k {
	case "single-az", "singleaz", "single", "single-az-instance":
		return RDSSingleAZ
	case "multi-az", "multiaz", "multi-az-instance", "multiazinstance",
		"multi-az-deployment", "multi-az-db-instance":
		return RDSMultiAZInstance
	case "multi-az-cluster", "multiazcluster", "cluster", "multi-az-db-cluster":
		return RDSMultiAZCluster
	}
	return RDSDeployment(k)
}

// Multiplier returns the factor this topology applies to a size's Single-AZ
// normalized units: ×1, ×2 or ×3. ok=false means the topology is unknown, and
// the caller must not guess — see the type doc for why the direction matters.
func (d RDSDeployment) Multiplier() (float64, bool) {
	switch NormalizeRDSDeployment(d) {
	case RDSSingleAZ:
		return 1, true
	case RDSMultiAZInstance:
		return 2, true
	case RDSMultiAZCluster:
		return 3, true
	}
	return 0, false
}

// rdsSizeUnits is the Single-AZ column of the Reserved DB Instance
// normalized-unit table, transcribed from
// USER_WorkingWithReservedDBInstances.html. Fourteen rows: it is NOT the EC2
// table in [sizeUnits]. RDS publishes no `nano` row (no db.*.nano class
// exists) and none of EC2's 3xlarge/9xlarge/18xlarge/48xlarge/56xlarge/
// 96xlarge/112xlarge rows.
//
// A single wrong row silently mis-prices every recommendation for that class
// type, so TestRDSNormalizationTableMatchesPublishedUnits transcribes all
// fourteen rows a second time, independently, along with the ×2 and ×3
// deployment columns.
var rdsSizeUnits = map[string]float64{
	"micro": 0.5, "small": 1, "medium": 2, "large": 4, "xlarge": 8,
	"2xlarge": 16, "4xlarge": 32, "6xlarge": 48, "8xlarge": 64,
	"10xlarge": 80, "12xlarge": 96, "16xlarge": 128, "24xlarge": 192,
	"32xlarge": 256,
}

// RDSNormalizationUnits returns the normalized units of one DB instance-hour
// of the given size in the given deployment topology — ("large",
// RDSMultiAZInstance) → 8.
//
// Sizes AWS documents come from the table verbatim. An undocumented
// "<N>xlarge" is extrapolated as 8×N, which reproduces every documented row
// from xlarge (8) through 32xlarge (256) exactly; that keeps a newly launched
// size from silently losing its reservation coverage. Anything else returns
// ok=false, as does an unknown deployment, and an unscored line is left for an
// exact-match reservation or on-demand rather than guessed at.
func RDSNormalizationUnits(size string, d RDSDeployment) (float64, bool) {
	base, ok := rdsBaseUnits(size)
	if !ok {
		return 0, false
	}
	m, ok := d.Multiplier()
	if !ok {
		return 0, false
	}
	return base * m, true
}

func rdsBaseUnits(size string) (float64, bool) {
	s := strings.ToLower(strings.TrimSpace(size))
	if u, ok := rdsSizeUnits[s]; ok {
		return u, true
	}
	if n, ok := strings.CutSuffix(s, "xlarge"); ok && n != "" {
		if v, err := strconv.Atoi(n); err == nil && v > 0 && v <= 1<<20 {
			return 8 * float64(v), true
		}
	}
	return 0, false
}

// splitRDSClass splits a DB instance class into its class type and size:
// "db.r6i.large" → ("db.r6i", "large"). It returns ("", "") for anything that
// is not a db.<class>.<size> class — including Aurora's "db.serverless",
// which has no size and therefore no normalized units.
func splitRDSClass(class string) (classType, size string) {
	c := strings.ToLower(strings.TrimSpace(class))
	if !strings.HasPrefix(c, "db.") {
		return "", ""
	}
	i := strings.LastIndexByte(c, '.')
	if i <= len("db.") || i+1 >= len(c) {
		return "", ""
	}
	return c[:i], c[i+1:]
}

// RDSClassType returns the DB *instance class type* — "db.r6i" for
// "db.r6i.large" — which is the unit of size flexibility. AWS's own
// counter-example is the whole point: db.r6i.large and db.r6id.large are
// different class types and share no discount, even though a family-prefix
// rule would call them the same thing. Returns "" if the input is not a DB
// instance class.
func RDSClassType(class string) string { t, _ := splitRDSClass(class); return t }

// RDSSize returns the size of a DB instance class — "large" for
// "db.r6i.large" — or "".
func RDSSize(class string) string { _, s := splitRDSClass(class); return s }

// RDSClassUnits returns the normalized units of one hour of the given DB
// instance class in the given topology: ("db.r6i.large", RDSMultiAZInstance)
// → 8. ok=false for an unrecognized class, size or topology.
func RDSClassUnits(class string, d RDSDeployment) (float64, bool) {
	classType, size := splitRDSClass(class)
	if classType == "" {
		return 0, false
	}
	return RDSNormalizationUnits(size, d)
}

// NormalizeRDSEngine folds an engine or ProductDescription spelling into the
// identity a reservation matches on. DescribeReservedDBInstances reports
// "postgresql", "oracle-se2(license-included)", "sqlserver-ee(li)" and
// friends; DescribeDBInstances reports "postgres", "oracle-se2",
// "sqlserver-ee". Only the license marker and the postgres/postgresql spelling
// are folded — the engine *edition* is preserved, because a sqlserver-se
// reservation does not cover sqlserver-ee usage.
//
// An unrecognized engine is lower-cased and passed through, so it matches only
// itself and is never size-flexible.
func NormalizeRDSEngine(engine string) string {
	e := strings.ToLower(strings.Join(strings.Fields(engine), ""))
	e = strings.NewReplacer(
		"_", "-",
		"(license-included)", "(li)",
		"license-included", "li",
		"(bring-your-own-license)", "(byol)",
		"bring-your-own-license", "byol",
	).Replace(e)
	if e == "postgres" {
		e = "postgresql"
	}
	return e
}

// RDSEngineFamily returns the engine family of a normalized engine string:
// "postgresql", "mysql", "mariadb", "db2", "oracle", "sqlserver", "aurora", or
// "" for an empty engine.
func RDSEngineFamily(engine string) string {
	e := NormalizeRDSEngine(engine)
	if i := strings.IndexAny(e, "-("); i >= 0 {
		e = e[:i]
	}
	return e
}

// rdsSizeFlexibleFamilies is the closed set from the verbatim clause "Size
// flexibility does not apply to RDS for SQL Server and RDS for Oracle License
// Included. (It does apply to Db2, MariaDB, MySQL, PostgreSQL and Oracle
// BYOL.)" Oracle is absent here because it is licence-dependent and handled in
// [RDSSizeFlexibleEngine]; SQL Server is absent because it is excluded.
//
// Aurora is absent deliberately. Its billing model is a third thing wearing
// RDS's name and this repo has not verified it, so an Aurora line gets
// exact-match coverage only — it strands rather than guesses.
var rdsSizeFlexibleFamilies = map[string]bool{
	"db2": true, "mariadb": true, "mysql": true, "postgresql": true,
}

// RDSSizeFlexibleEngine reports whether a reservation on this engine may float
// across sizes within its class type.
//
// Oracle qualifies only under BYOL, and only when the licence model is
// explicit: an Oracle engine that names no licence model is ambiguous, and
// ambiguity resolves to *not* flexible. That direction over-states stranding
// and can only suppress a recommendation, never manufacture one.
func RDSSizeFlexibleEngine(engine string) bool {
	e := NormalizeRDSEngine(engine)
	switch RDSEngineFamily(e) {
	case "":
		return false
	case "oracle":
		return strings.Contains(e, "byol")
	}
	return rdsSizeFlexibleFamilies[RDSEngineFamily(e)]
}

// ReservedDBInstance is one Reserved DB Instance purchase, as returned by
// rds:DescribeReservedDBInstances.
//
// EffectiveHourlyUSD is the amortized all-in rate for ONE reservation: hourly
// recurring charge plus upfront ÷ term hours. It is charged for every hour of
// the term whether or not usage absorbs it — and, unlike an EC2 Reserved
// Instance, it cannot even be sold on: "You can't cancel a reserved DB
// instance." Stranding is therefore permanent until Expires, which is the date
// [Inventory.NetSavings] puts on a suppression.
//
// The discount covers instance hours only: "doesn't provide a discount for the
// costs associated with storage, backups, and I/O".
//
// OfferingType (All Upfront / Partial Upfront / No Upfront) is recorded for
// reporting and deliberately does not affect billing — the amortization is
// already in EffectiveHourlyUSD.
type ReservedDBInstance struct {
	ID              string `json:"id"`
	Count           int    `json:"count"`
	DBInstanceClass string `json:"dbInstanceClass"` // "db.r6i.large"
	Region          string `json:"region"`
	// Engine is the reservation's product description, e.g. "postgresql",
	// "sqlserver-ee(li)", "oracle-se2(byol)". A reservation covers its own
	// engine and no other.
	Engine     string        `json:"engine"`
	Deployment RDSDeployment `json:"deployment"`

	OfferingType       string    `json:"offeringType,omitempty"`
	EffectiveHourlyUSD float64   `json:"effectiveHourlyUSD"`
	Expires            time.Time `json:"expires,omitempty"`
}

// ClassType returns the reservation's DB instance class type ("db.r6i").
func (r ReservedDBInstance) ClassType() string { return RDSClassType(r.DBInstanceClass) }

// Units returns the normalized units ONE of these reservations supplies.
// ok=false means the class or the topology is unrecognized, in which case the
// caller must fall back to exact-class matching rather than guess.
func (r ReservedDBInstance) Units() (float64, bool) {
	return RDSClassUnits(r.DBInstanceClass, r.Deployment)
}

// SizeFlexible reports whether this reservation's discount floats across sizes
// within its instance class type. That requires a size-flexible engine, a
// recognizable class type, and a known unit count — the RDS analogue of
// [ReservedInstance.SizeFlexible], with the engine gate replacing the
// platform/tenancy gate and the class type replacing the family.
func (r ReservedDBInstance) SizeFlexible() bool {
	if !RDSSizeFlexibleEngine(r.Engine) || r.ClassType() == "" {
		return false
	}
	_, ok := r.Units()
	return ok
}

// validateRDS reports the first structural defect in a KindRDS usage line.
// Called from [Usage.Validate]; see [RDSDeployment] for why an unset topology
// is an error rather than a default.
func (l UsageLine) validateRDS() error {
	switch {
	case l.InstanceType == "":
		return fmt.Errorf("rds line needs a DB instance class in instanceType")
	case RDSClassType(l.InstanceType) == "":
		return fmt.Errorf("instanceType %q is not a DB instance class (want db.<class>.<size>)", l.InstanceType)
	case l.Engine == "":
		return fmt.Errorf("rds line needs an engine: size flexibility is engine-gated")
	case l.Deployment == "":
		return fmt.Errorf("rds line needs a deployment (%q, %q or %q): topology is a unit multiplier, not a label",
			RDSSingleAZ, RDSMultiAZInstance, RDSMultiAZCluster)
	case !rdsDeploymentKnown(l.Deployment):
		return fmt.Errorf("rds line has unknown deployment %q", l.Deployment)
	case l.SPIneligible:
		// No Savings Plan covers RDS in the first place, so the flag can only
		// mean the caller believes some plan might have. Say so rather than
		// let a wrong mental model pass silently.
		return fmt.Errorf("rds line must not set spIneligible: no savings plan covers RDS at all")
	}
	return nil
}

func rdsDeploymentKnown(d RDSDeployment) bool { _, ok := d.Multiplier(); return ok }

// validateReservedDBs rejects reservations that would silently skew every bill
// built on them. Mirrors the [ReservedInstance] half of [Inventory.Validate].
func (inv *Inventory) validateReservedDBs() error {
	seen := map[string]bool{}
	for i, r := range inv.ReservedDBs {
		switch {
		case r.Count <= 0:
			return fmt.Errorf("commit: reserved db instance %d (%q): count must be > 0, got %d", i, r.ID, r.Count)
		case r.DBInstanceClass == "":
			return fmt.Errorf("commit: reserved db instance %d (%q): dbInstanceClass required", i, r.ID)
		case RDSClassType(r.DBInstanceClass) == "":
			return fmt.Errorf("commit: reserved db instance %d (%q): dbInstanceClass %q is not a DB instance class", i, r.ID, r.DBInstanceClass)
		case r.Region == "":
			return fmt.Errorf("commit: reserved db instance %d (%q): region required", i, r.ID)
		case r.Engine == "":
			return fmt.Errorf("commit: reserved db instance %d (%q): engine required", i, r.ID)
		case r.Deployment == "":
			return fmt.Errorf("commit: reserved db instance %d (%q): deployment required", i, r.ID)
		case !rdsDeploymentKnown(r.Deployment):
			return fmt.Errorf("commit: reserved db instance %d (%q): unknown deployment %q", i, r.ID, r.Deployment)
		case !finite(r.EffectiveHourlyUSD) || r.EffectiveHourlyUSD < 0:
			return fmt.Errorf("commit: reserved db instance %d (%q): bad effectiveHourlyUSD %v", i, r.ID, r.EffectiveHourlyUSD)
		}
		if r.ID != "" {
			if seen[r.ID] {
				return fmt.Errorf("commit: duplicate reserved db instance id %q", r.ID)
			}
			seen[r.ID] = true
		}
	}
	return nil
}

// activeReservedDBs is the [Inventory.Active] filter for this product.
func (inv *Inventory) activeReservedDBs(t time.Time) []ReservedDBInstance {
	if inv == nil {
		return nil
	}
	var out []ReservedDBInstance
	for _, r := range inv.ReservedDBs {
		if r.Expires.IsZero() || r.Expires.After(t) {
			out = append(out, r)
		}
	}
	return out
}

// canonicalReservedDBs orders reservations soonest-expiring first, for the
// same reason [Inventory.canonicalRIs] does: the order cannot change the bill,
// but it decides which reservation is reported as stranded and therefore what
// date a suppression carries. Consuming the soonest-expiring first leaves the
// stranded units on the longest-lived reservation.
func (inv *Inventory) canonicalReservedDBs() []ReservedDBInstance {
	if inv == nil {
		return nil
	}
	out := append([]ReservedDBInstance(nil), inv.ReservedDBs...)
	sort.SliceStable(out, func(i, j int) bool { return rdbKey(out[i]) < rdbKey(out[j]) })
	return out
}

func rdbKey(r ReservedDBInstance) string {
	exp := "9999-99-99"
	if !r.Expires.IsZero() {
		exp = r.Expires.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{exp, r.ID, r.DBInstanceClass, r.Region,
		NormalizeRDSEngine(r.Engine), string(NormalizeRDSDeployment(r.Deployment)),
		strconv.Itoa(r.Count), strconv.FormatFloat(r.EffectiveHourlyUSD, 'g', 17, 64)}, "\x00")
}

// rdsLineUnits is a usage line's normalized units per instance-hour.
func rdsLineUnits(l UsageLine) (float64, bool) {
	return RDSClassUnits(l.InstanceType, l.Deployment)
}

// applyReservedDB applies one Reserved DB Instance and reports its capacity
// and consumption in normalized units. It is the RDS twin of [applyRI], and
// the differences are exactly the three the AWS page documents: matching is on
// the *class type* rather than the family, flexibility is gated on the engine
// rather than on platform/tenancy, and the deployment topology multiplies the
// units on both sides.
//
// It touches KindRDS lines only. Nothing else in the waterfall touches them,
// so this is the sole path by which an RDS line can be discounted.
func applyReservedDB(r ReservedDBInstance, states []*lineState) (capUnits, usedUnits float64) {
	perRes, unitsOK := r.Units()
	if !unitsOK {
		// Unknown class or topology: count whole instances. Matching is
		// exact-class-only in that case, so the scale cancels out.
		perRes = 1
	}
	capUnits = float64(max(r.Count, 0)) * perRes
	if capUnits <= Eps {
		return capUnits, 0
	}

	flexible := r.SizeFlexible()
	classType := r.ClassType()
	engine := NormalizeRDSEngine(r.Engine)
	deployment := NormalizeRDSDeployment(r.Deployment)
	class := strings.ToLower(strings.TrimSpace(r.DBInstanceClass))

	type candidate struct {
		s     *lineState
		units float64 // normalized units per instance-hour of this line
	}
	var elig []candidate
	for _, s := range states {
		l := s.line
		// "Discounts for reserved DB instances are tied to instance type and
		// AWS Region", and size flexibility additionally requires "the same
		// AWS Region and database engine".
		if s.remaining <= Eps || l.Kind != KindRDS ||
			!strings.EqualFold(l.Region, r.Region) ||
			NormalizeRDSEngine(l.Engine) != engine {
			continue
		}
		units, unitsKnown := rdsLineUnits(l)
		if flexible {
			// Same class type, any size, any topology — the units carry both
			// the size and the Single-AZ ⇄ Multi-AZ conversion. A line whose
			// class or topology has no known unit count cannot be scored
			// against the pool, so it is left for an exact-class reservation
			// or for on-demand.
			if !unitsKnown || RDSClassType(l.InstanceType) != classType {
				continue
			}
		} else {
			// Not size-flexible: the class and the topology must both match
			// exactly. Multi-AZ ⇄ Single-AZ conversion is itself expressed in
			// normalized units, so an engine barred from size flexibility
			// cannot cross that boundary either. [unverified] — AWS documents
			// the exclusion, not its interaction with topology; requiring the
			// exact match strands more, never less.
			if strings.ToLower(strings.TrimSpace(l.InstanceType)) != class ||
				NormalizeRDSDeployment(l.Deployment) != deployment {
				continue
			}
			if !unitsKnown {
				units = perRes // whole-instance counting on the exact-match path
			}
		}
		if units <= 0 {
			continue
		}
		elig = append(elig, candidate{s, units})
	}

	// Smallest instance first, mirroring apply_ri.html's documented order for
	// the EC2 product. The order cannot change the total units absorbed — that
	// is min(capacity, demand) either way — but it decides per-line
	// attribution, and attribution must be deterministic.
	sort.SliceStable(elig, func(i, j int) bool {
		if flexible {
			if d := elig[i].units - elig[j].units; d < -Eps || d > Eps {
				return elig[i].units < elig[j].units
			}
		}
		return elig[i].s.key < elig[j].s.key
	})

	budget := capUnits
	for _, c := range elig {
		if budget <= Eps {
			break
		}
		takeUnits := math.Min(budget, c.s.remaining*c.units)
		if takeUnits <= 0 {
			continue
		}
		qty := takeUnits / c.units
		if qty > c.s.remaining {
			qty = c.s.remaining
		}
		c.s.remaining -= qty
		c.s.cov.RIQty += qty
		budget -= qty * c.units
	}
	return capUnits, capUnits - budget
}
