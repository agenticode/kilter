package rds

import (
	"fmt"
	"strings"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Engine semantics — trap 9, and the reason this file exists.
//
// The RDS metrics reference is precise, and the precision is the problem:
//
//	FreeableMemory — The amount of available random access memory. For
//	MariaDB, MySQL, Oracle, and PostgreSQL DB instances, this metric reports
//	the value of the MemAvailable field of /proc/meminfo.
//	[verified: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-metrics.html]
//
// `MemAvailable` is the kernel's estimate of memory obtainable for a new
// allocation without swapping, and it COUNTS RECLAIMABLE PAGE CACHE AS
// AVAILABLE. That single sentence splits the fleet in half:
//
//   - PostgreSQL deliberately leans on the OS page cache. `shared_buffers` is
//     typically a fraction of RAM and the rest of the working set lives in
//     cache that MemAvailable reports as free. A well-tuned PostgreSQL
//     instance with a hot 200 GiB working set fully cached reports LARGE
//     FreeableMemory. The metric says "spare"; the truth is "in use,
//     productively". Downsizing on it evicts the cache, converts memory hits
//     into storage reads, and moves the cost from the instance line to the
//     I/O and latency lines.
//   - MySQL and MariaDB hold the InnoDB buffer pool as ANONYMOUS memory, which
//     MemAvailable does not count as available. Here FreeableMemory is close
//     to real headroom — but the pool is sized from the parameter group as a
//     fraction of instance memory [unverified: the RDS default parameter
//     group's innodb_buffer_pool_size formula], so shrinking the class shrinks
//     the pool, which changes the I/O profile, which changes CPU. The signal
//     is honest; the effect of acting on it is not linear.
//   - SQL Server, Oracle and Db2 run their own memory managers and carry their
//     own licence-linked class constraints.
//
// §7 trap 4 (memory-blind EC2 downsizing) does not catch this: there, the
// signal is ABSENT and a refusal fires. Here the signal is PRESENT and
// engine-dependently misleading, so pkg/ec2's memory-blind rule is satisfied
// on paper while being violated in spirit. Nothing would fire.
//
// The only honest handling is an engine-keyed policy that DEFAULTS TO REFUSAL,
// exactly as pkg/ec2 defaults t2 to `unknown` rather than guessing its
// baselines: "Rather than guess, burstFamilies recognizes t2 as credit-based
// while burstBaselines has no row for it, so every t2 instance lands in
// unknown and is refused" (pkg/ec2/FINDINGS.md §5).

// Engine families this package recognizes. An engine outside this set is
// refused by name ([ReasonUnknownEngine]) rather than treated as
// "probably like MySQL".
const (
	FamilyPostgreSQL = "postgresql"
	FamilyMySQL      = "mysql"
	FamilyMariaDB    = "mariadb"
	FamilyOracle     = "oracle"
	FamilySQLServer  = "sqlserver"
	FamilyDb2        = "db2"
	FamilyAurora     = "aurora"
)

// Licence models RDS reports.
const (
	LicenseIncluded = "license-included"
	LicenseBYOL     = "bring-your-own-license"
	LicenseGPL      = "general-public-license"
)

// Engine is a parsed RDS engine identity: the family, its edition where the
// family has one, and the licence model.
//
// All three are load-bearing rather than decorative. The family decides
// FreeableMemory semantics (this file) and the storage striping threshold
// (§2.4, U13's table). The edition decides the price band — a db.r6i.xlarge
// running SQL Server Enterprise license-included and one running PostgreSQL
// are the same hardware at different prices (§2.8). The licence decides
// reservation size flexibility: it applies to Oracle BYOL and not to Oracle
// License Included (§2.6).
type Engine struct {
	// Raw is the DescribeDBInstances spelling, preserved so a refusal can name
	// the engine the operator will recognize.
	Raw string `json:"raw"`
	// Family is one of the Family* constants, or "" when unrecognized.
	Family string `json:"family,omitempty"`
	// Edition is "ee", "se", "se1", "se2", "web", "ex" for SQL Server and
	// Oracle; "" for the open-source engines, which have no editions.
	Edition string `json:"edition,omitempty"`
	// License is one of the License* constants, or "" when not reported.
	License string `json:"license,omitempty"`
}

// ParseEngine normalizes a DescribeDBInstances engine string and licence model
// into an [Engine].
//
// The engine strings AWS emits are: "postgres", "mysql", "mariadb", "db2-ae",
// "db2-se", "oracle-ee", "oracle-ee-cdb", "oracle-se2", "oracle-se2-cdb",
// "sqlserver-ee", "sqlserver-se", "sqlserver-ex", "sqlserver-web",
// "aurora-mysql", "aurora-postgresql". Anything else parses to an Engine with
// an empty Family, which every caller treats as a refusal.
func ParseEngine(engine, licenseModel string) Engine {
	raw := strings.TrimSpace(engine)
	e := Engine{Raw: raw, License: normalizeLicense(licenseModel)}
	k := strings.ToLower(strings.ReplaceAll(raw, "_", "-"))
	switch {
	case k == "":
		return e
	case strings.HasPrefix(k, "aurora"):
		e.Family = FamilyAurora
		// The Aurora sub-engine ("aurora-mysql") is kept in Edition so a
		// refusal can name it, but it changes nothing: Aurora is excluded
		// whole (trap 16).
		e.Edition = strings.TrimPrefix(strings.TrimPrefix(k, "aurora"), "-")
	case k == "postgres" || k == "postgresql":
		e.Family = FamilyPostgreSQL
	case k == "mysql":
		e.Family = FamilyMySQL
	case k == "mariadb":
		e.Family = FamilyMariaDB
	case strings.HasPrefix(k, "sqlserver"):
		e.Family, e.Edition = FamilySQLServer, editionSuffix(k, "sqlserver")
	case strings.HasPrefix(k, "oracle"):
		e.Family, e.Edition = FamilyOracle, oracleEdition(k)
	case strings.HasPrefix(k, "db2"):
		e.Family, e.Edition = FamilyDb2, editionSuffix(k, "db2")
	}
	return e
}

// editionSuffix pulls "ee" out of "sqlserver-ee".
func editionSuffix(k, prefix string) string {
	return strings.TrimPrefix(strings.TrimPrefix(k, prefix), "-")
}

// oracleEdition pulls "se2" out of "oracle-se2-cdb". The "-cdb" suffix marks
// the multitenant container-database architecture, which is not a separate
// price band or a separate reservation identity, so it is dropped.
func oracleEdition(k string) string {
	ed := editionSuffix(k, "oracle")
	return strings.TrimSuffix(ed, "-cdb")
}

func normalizeLicense(l string) string {
	k := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(l, "_", "-")))
	switch k {
	case "license-included", "li", "license included":
		return LicenseIncluded
	case "bring-your-own-license", "byol", "bring your own license":
		return LicenseBYOL
	case "general-public-license", "gpl", "general public license", "postgresql-license":
		return LicenseGPL
	}
	return k
}

// Known reports whether the engine family is one this package has encoded.
func (e Engine) Known() bool { return e.Family != "" }

// IsAurora reports trap 16's subject.
func (e Engine) IsAurora() bool { return e.Family == FamilyAurora }

// Licensed reports whether the engine carries a per-edition licence cost that
// breaks the pure PRICE model (§2.8). The open-source engines do not.
func (e Engine) Licensed() bool {
	return e.Family == FamilySQLServer || e.Family == FamilyOracle || e.Family == FamilyDb2
}

// licenceMarkedInReservations reports whether the licence model is part of the
// identity a RESERVATION matches on.
//
// It is deliberately NARROWER than [Engine.Licensed]. AWS's reserved-DB-instance
// page names licence models for exactly two engines — "Size flexibility does
// not apply to RDS for SQL Server and RDS for Oracle License Included" — and
// pkg/pricing/commit's [commit.NormalizeRDSEngine] folds exactly those
// spellings ("oracle-se2(license-included)", "sqlserver-ee(li)"). Db2 has a
// per-edition price and its reservation product description is
// **[unverified]**, so appending a licence marker to a Db2 usage line would
// stop it matching a Db2 reservation whose description carries none — turning
// every Db2 reservation into apparent stranding.
//
// Getting this wrong is safe in one direction only. Too FEW markers can let a
// reservation cover usage AWS would not have covered (optimistic, and the
// thing this package exists to prevent); too MANY strand more than reality
// (pessimistic). Db2 is left unmarked because the two sides of the comparison
// are then built the same way from the same unverified spelling, which is the
// case that neither over- nor under-matches — and the price axis, where Db2's
// licence really does matter, is [PriceBand] and is unaffected.
func (e Engine) licenceMarkedInReservations() bool {
	return e.Family == FamilySQLServer || e.Family == FamilyOracle
}

// String renders the engine identity for a report line.
func (e Engine) String() string {
	if e.Raw != "" {
		return e.Raw
	}
	if e.Family == "" {
		return "(unknown engine)"
	}
	return e.Family
}

// CommitEngine renders the identity a Reserved DB Instance matches on, in the
// spelling pkg/pricing/commit normalizes. The licence marker is appended for
// the licensed engines because a sqlserver-ee(li) reservation and a
// sqlserver-ee(byol) reservation are different products; it is omitted for the
// open-source engines, where AWS reports no licence distinction.
func (e Engine) CommitEngine() string {
	if e.Family == "" {
		return commit.NormalizeRDSEngine(e.Raw)
	}
	base := e.Family
	if e.Edition != "" {
		base += "-" + e.Edition
	}
	if e.licenceMarkedInReservations() {
		switch e.License {
		case LicenseIncluded:
			base += "(li)"
		case LicenseBYOL:
			base += "(byol)"
		}
	}
	return commit.NormalizeRDSEngine(base)
}

// SizeFlexible reports whether a reservation on this engine floats across
// sizes within its instance class type — trap 13, delegated to U12 so there is
// exactly one implementation of the rule in the tree.
//
// "Size flexibility does not apply to RDS for SQL Server and RDS for Oracle
// License Included. (It does apply to Db2, MariaDB, MySQL, PostgreSQL and
// Oracle BYOL.)" [verified: USER_WorkingWithReservedDBInstances.html]
func (e Engine) SizeFlexible() bool { return commit.RDSSizeFlexibleEngine(e.CommitEngine()) }

// MemorySemantics classifies what a `FreeableMemory` series MEANS on an
// engine. It is the encoded form of trap 9 and the reason two instances with
// byte-identical series get different verdicts.
type MemorySemantics string

const (
	// MemPageCacheDominant: the engine's working set lives in the OS page
	// cache, which MemAvailable counts as available. FreeableMemory is NOT
	// headroom and must not be converted into one. PostgreSQL.
	MemPageCacheDominant MemorySemantics = "page-cache-dominant"

	// MemAnonymousPool: the engine's working set is anonymous memory, which
	// MemAvailable does not count as available. FreeableMemory approximates
	// headroom — and the pool that produces it is itself a function of the
	// instance class, so the headroom is readable and the CONSEQUENCE of
	// acting on it is not linear. MySQL, MariaDB.
	MemAnonymousPool MemorySemantics = "anonymous-buffer-pool"

	// MemUnencoded: this package has not encoded the engine's memory manager.
	// The default, and a refusal. SQL Server, Oracle, Db2, and anything new.
	MemUnencoded MemorySemantics = "unencoded"
)

// memorySemantics is the whole policy, as a table, so it is reviewable at a
// glance and so adding an engine is a deliberate act rather than a fallthrough.
//
// Only the two engines whose buffer semantics are documented above appear.
// Oracle is ABSENT even though rds-metrics.html names it in the MemAvailable
// sentence: knowing which /proc field the metric reports is not the same as
// knowing what the SGA/PGA do with it, and this package refuses the difference
// rather than splitting it.
var memorySemantics = map[string]MemorySemantics{
	FamilyPostgreSQL: MemPageCacheDominant,
	FamilyMySQL:      MemAnonymousPool,
	FamilyMariaDB:    MemAnonymousPool,
}

// MemorySemanticsFor returns the encoded semantics for an engine, defaulting
// to [MemUnencoded].
func MemorySemanticsFor(e Engine) MemorySemantics {
	if s, ok := memorySemantics[e.Family]; ok {
		return s
	}
	return MemUnencoded
}

// MemoryVerdict is what this package will say about a `FreeableMemory` series.
//
// Readable is the load-bearing field. When it is false, MinFreeBytes and
// FreeFraction are ZERO — not "computed but unused". A number that must not be
// read as headroom is not carried in a field named after headroom, because the
// next reader will use it.
type MemoryVerdict struct {
	Semantics MemorySemantics `json:"semantics"`
	// Readable reports whether the series may be read as spare memory at all.
	Readable bool `json:"readable"`
	// Code is the refusal code this verdict produces. Every verdict produces
	// one: even the readable case refuses the downsize, for a different reason
	// ([ReasonBufferPoolScalesWithClass]).
	Code string `json:"code"`
	// Reason is the prose for a human.
	Reason string `json:"reason"`
	// MinFreeBytes is the LOW-WATER mark of the series — the worst moment, not
	// the average — and is set only when Readable.
	MinFreeBytes float64 `json:"minFreeBytes,omitempty"`
	// FreeFraction is MinFreeBytes as a fraction of the class's memory, set
	// only when Readable and only when the class's memory size is known. It is
	// never used to size a downsize; it exists so the report can state the
	// magnitude of what it is declining to act on.
	FreeFraction float64 `json:"freeFraction,omitempty"`
	// Samples is how many datapoints backed the verdict.
	Samples int `json:"samples,omitempty"`
}

// AssessMemory converts a `FreeableMemory` series into a verdict — or refuses
// to. This is trap 9's fix, and it is the function TestFreeableMemoryIsNotHeadroom
// drives with two engines and one series.
//
// classMemoryBytes may be 0 when the class's memory size is unknown; the
// verdict then omits FreeFraction rather than dividing by a guess.
func AssessMemory(e Engine, s Series, classMemoryBytes float64) MemoryVerdict {
	sem := MemorySemanticsFor(e)
	v := MemoryVerdict{Semantics: sem, Samples: s.Len()}

	if !s.Usable() {
		v.Code = ReasonNoMetricEvidence
		if s.Partial {
			v.Code = ReasonTruncatedMetrics
			v.Reason = fmt.Sprintf("CloudWatch did not deliver %s in full (status %q); a missing answer is "+
				"not an empty metric, so no memory verdict is available", MetricFreeableMemory, s.Status)
			return v
		}
		v.Reason = fmt.Sprintf("no %s datapoints were delivered, so this instance's memory is unobserved",
			MetricFreeableMemory)
		return v
	}

	switch sem {
	case MemPageCacheDominant:
		// The refusal that matters. Do NOT compute a headroom number here:
		// the whole point is that the series does not mean what the field name
		// would imply, and a populated field is an invitation to use it.
		v.Code = ReasonFreeableMemoryIsPageCache
		v.Reason = fmt.Sprintf(
			"%s on %s reports MemAvailable, which counts reclaimable page cache as available. %s leans on "+
				"the OS page cache by design, so a fully-cached hot working set reports large freeable "+
				"memory while every byte of it is in productive use. Downsizing on this evidence evicts "+
				"the cache and moves cost from the instance line to the I/O and latency lines, so this "+
				"series is not read as headroom at all",
			MetricFreeableMemory, e.String(), FamilyPostgreSQL)
		return v

	case MemAnonymousPool:
		v.Readable = true
		if lo, ok := s.Min(); ok {
			v.MinFreeBytes = lo
			if classMemoryBytes > 0 {
				v.FreeFraction = lo / classMemoryBytes
			}
		}
		v.Code = ReasonBufferPoolScalesWithClass
		v.Reason = fmt.Sprintf(
			"%s on %s is close to real headroom — InnoDB holds its buffer pool as anonymous memory, which "+
				"MemAvailable does not count as available — but the pool is sized from the parameter group "+
				"as a fraction of instance memory, so a smaller class means a smaller pool, more disk I/O "+
				"and more CPU. The low-water mark is %s; the response of the bill to a class change is not "+
				"linear in it, so the number is reported and not acted on",
			MetricFreeableMemory, e.String(), fmtGiB(v.MinFreeBytes/GiB))
		return v

	default:
		v.Code = ReasonMemorySemanticsUnencoded
		v.Reason = fmt.Sprintf(
			"%s has not encoded what %s means on engine %q. The metric reports MemAvailable, and whether "+
				"that is spare memory or a productively-used cache is a property of the engine's memory "+
				"manager. An engine whose semantics are not encoded refuses rather than guesses",
			"pkg/rds", MetricFreeableMemory, e.String())
		return v
	}
}
