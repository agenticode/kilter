package rds

// RDS storage-performance parity — U13, and the file trap 11 exists to stop
// anyone writing from memory.
//
// This is `pkg/ebs/parity.go`'s idiom and NONE of its numbers. The two models
// disagree in every dimension that decides a dollar:
//
//	                       pkg/ebs (a raw EBS volume)   this file (RDS)
//	gp2 burst ceiling      3,000 IOPS                   3,000 or 12,000 (striped)
//	gp2 throughput ceiling 250 MiB/s                    250 or 1,000 MiB/s
//	regime boundary        334 GiB / 1,000 GiB          400 GiB, 200 GiB (Oracle),
//	                                                    never (SQL Server)
//	gp3 provisioning       any size                     ONLY at or above the
//	                                                    striping threshold
//
// RDS stripes across FOUR volumes above an engine-dependent threshold and the
// baselines step at that threshold [verified:
// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/CHAP_Storage.html].
// Reusing pkg/ebs's constants would claim a saving in the band where RDS loses
// throughput and refuse in the band where it converts cleanly — so this file
// shares no constant, no function and no type with that one, and
// TestRDSGP2ModelIsNotTheEBSModel runs one 500 GiB MySQL volume through both
// and asserts they disagree.
//
// # Where the money actually is
//
// Not in gp2 → gp3. §2.4 is explicit: "the addressable opportunity is only the
// over-provisioned tail above the baseline", because in the striped regime the
// provisionable range STARTS at 12,000 IOPS / 500 MiB/s — a 20,000-IOPS
// instance can be reduced to 12,000 and no further, and below the threshold
// there is nothing to reduce because nothing can be provisioned. So the
// proposal this file produces most often is a REDUCTION of provisioned gp3
// performance toward a floor it may never cross, and the second most common
// output is a refusal.
//
// # Units and purity
//
// Throughput is MiB/s everywhere, matching what ModifyDBInstance provisions
// and what the RDS byte metrics divide down to. Nothing here reads a clock
// (callers pass `now` through [ParityConfig]), performs I/O, holds mutable
// package state, or iterates a map into an output.

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// --- The striping table (§2.4, verified) -----------------------------------
//
// | Engine                              | Striping threshold |
// |-------------------------------------|--------------------|
// | Db2, MariaDB, MySQL, PostgreSQL     | 400 GiB            |
// | Oracle                              | 200 GiB            |
// | SQL Server                          | never (1 volume)   |
//
// The threshold is not decoration: it decides the gp2 burst ceiling, the gp3
// baseline, AND whether gp3 can be provisioned at all.
const (
	// StripingThresholdGiB is the threshold for Db2, MariaDB, MySQL and
	// PostgreSQL.
	StripingThresholdGiB int64 = 400
	// StripingThresholdOracleGiB is Oracle's, which is half.
	StripingThresholdOracleGiB int64 = 200
	// StripingVolumes is how many EBS volumes RDS stripes across above the
	// threshold. It is 4 for every engine that stripes at all.
	StripingVolumes = 4
	// NeverStripes is the threshold reported for RDS for SQL Server, which
	// "uses a single volume" at every size. It is a distinct value rather than
	// zero so a caller cannot read "no threshold" as "threshold of zero",
	// which would put every SQL Server instance in the striped regime.
	NeverStripes int64 = -1
)

// The gp3 baselines, both regimes [verified: CHAP_Storage.html].
const (
	// GP3BaselineIOPS and GP3BaselineThroughputMBps are what gp3 delivers
	// BELOW the striping threshold, and on RDS for SQL Server at every size.
	GP3BaselineIOPS           int32 = 3000
	GP3BaselineThroughputMBps int32 = 125
	// GP3StripedBaselineIOPS and GP3StripedBaselineThroughputMBps are what it
	// delivers at or above the threshold — and they are also the floor of the
	// provisionable range, which is why a reduction stops here (§2.4:
	// "you can reduce a 20,000-IOPS instance to 12,000 and no further").
	GP3StripedBaselineIOPS           int32 = 12000
	GP3StripedBaselineThroughputMBps int32 = 500
)

// MaxParitySizeGiB is the largest allocation the published gp2 table covers
// (65,536 GiB). A record above it is an unreadable inventory record, not a
// very large database, and it is refused rather than modelled.
const MaxParitySizeGiB int64 = 65536

// MaxStorageModificationsPer24h is the documented rate limit: "You can perform
// a maximum of four storage modifications on a DB instance within any 24-hour
// period" [verified:
// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_ModifyInstance.Settings.html].
//
// It is a READ-ONLY concern in this unit and a hard gate in U14: proposing a
// change that AWS will reject is not advice, it is noise. [CooldownVerdict]
// carries the arithmetic.
const MaxStorageModificationsPer24h = 4

// StorageModificationWindow is the period that limit applies over.
const StorageModificationWindow = 24 * time.Hour

// parityEps is the money/quantity comparison tolerance. Money is never
// compared with ==.
const parityEps = 1e-9

// MiB is one mebibyte, the unit throughput is expressed in.
const MiB float64 = 1 << 20

// --- Refusal codes ---------------------------------------------------------
//
// Every one of these is a REFUSAL, never an exclusion: the instance is
// modelled, and one specific storage-performance change was declined for one
// specific reason. TestParityReasonCodesAreDistinct pins that none collides
// with a U11 code.
const (
	// ReasonParityStorageTypeNotModelled: the volume is io1, io2 or magnetic.
	// Provisioned IOPS storage is a different product with a different price
	// function, and this unit models gp2 and gp3 only.
	ReasonParityStorageTypeNotModelled = "storage-type-not-modelled"

	// ReasonParitySizeUnusable: the allocated size is non-positive or above
	// the largest allocation the published table covers.
	ReasonParitySizeUnusable = "storage-size-unusable"

	// ReasonParityGP2BandUnpublished: AWS publishes gp2 baseline/burst bands
	// for MariaDB, MySQL and PostgreSQL (5–65,536 GiB) and for SQL Server
	// (334–999 GiB), and for nothing else. An Oracle or Db2 gp2 volume, or a
	// SQL Server volume outside that band, has no published nameplate — so
	// there is no number a conversion could preserve, and the answer is a
	// refusal rather than the MySQL row applied to an engine that stripes at a
	// different size.
	ReasonParityGP2BandUnpublished = "gp2-band-unpublished"

	// ReasonParityNotProvisionableBelowThreshold: trap 11's sharpest edge.
	// "For every DB engine except RDS for SQL Server, you can provision
	// additional IOPS and storage throughput when storage size is at or above
	// the threshold value" — below it the provisioning columns read literally
	// "N/A". A gp2 volume delivering more than 3,000 IOPS or 125 MiB/s below
	// the threshold therefore has NO gp3 form at parity, at any price.
	ReasonParityNotProvisionableBelowThreshold = "gp3-not-provisionable-below-striping-threshold"

	// ReasonParityEnvelopeUnknown: §2.4's contradiction. "AWS's own page
	// contradicts itself on the SQL Server gp3 ceiling — the gp3 table says
	// 3,000–80,000 IOPS while the comparison table on the same page says
	// 'Maximum IOPS: 64,000 (16,000 on RDS for SQL Server)'. [unverified:
	// which is current]. Do not hardcode either; read the envelope from
	// rds:DescribeValidDBInstanceModifications and refuse when it is
	// unavailable." This code is that refusal.
	ReasonParityEnvelopeUnknown = "provisioning-envelope-unknown"

	// ReasonParityExceedsEnvelope: the demand needs more than the LIVE
	// envelope allows. The instance needs io2 or a bigger allocation, not a
	// cheaper storage type.
	ReasonParityExceedsEnvelope = "storage-demand-exceeds-envelope"

	// ReasonParityNoCheaperConfig: a configuration exists that clears demand
	// and it costs at least as much as today. The RDS analogue of pkg/ebs's
	// no-cheaper-config, and it fires in a completely different size band —
	// TestThroughputParityRefusalBand.
	ReasonParityNoCheaperConfig = "storage-parity-not-cheaper"

	// ReasonParityFloorsAtBaseline: the instance is already at (or below) the
	// non-reducible baseline, so the over-provisioned tail is empty. This is
	// the common case and it is stated rather than skipped: "nothing to
	// reduce" and "not looked at" are different facts.
	ReasonParityFloorsAtBaseline = "provisioned-performance-floors-at-baseline"

	// ReasonParityNoMeasurement: ReadIOPS/WriteIOPS/ReadThroughput/
	// WriteThroughput were not all delivered in full. A reduction on an
	// unmeasured volume is a guess wearing a number, so it is refused; a
	// conversion may still proceed against the NAMEPLATE floor, which cannot
	// degrade anything.
	ReasonParityNoMeasurement = "no-io-measurement"

	// ReasonParityWindowTooShort: the I/O series covers less than the
	// configured minimum. Same treatment as no measurement for reductions.
	ReasonParityWindowTooShort = "io-window-too-short"

	// ReasonParityStorageOptimization: "You can't modify allocated storage if
	// the DB instance status is storage-optimization" [verified], and a
	// reading taken during a modification is not a reading of the steady
	// state.
	ReasonParityStorageOptimization = "storage-optimization-blocks-modification"

	// ReasonParityCooldown: four storage modifications inside 24 hours is the
	// documented ceiling, so a fifth is not a recommendation, it is an API
	// error with a dollar figure attached.
	ReasonParityCooldown = "storage-modification-cooldown"

	// ReasonParityLowConfidence: the evidence does not support acting, only
	// watching. Confidence is EARNED, never lost — see [ParityConfidence].
	ReasonParityLowConfidence = "storage-parity-low-confidence"
)

// --- The published gp2 table, transcribed --------------------------------

// GP2Band is one row of the published RDS gp2 table (§2.4, verified against
// CHAP_Storage.html). Both endpoints of every range are carried, because the
// table publishes RANGES per band and not a per-size formula, and inventing
// the formula is exactly the thing trap 11 warns about.
type GP2Band struct {
	MinGiB, MaxGiB int64
	// MinBaselineIOPS and MaxBaselineIOPS are the band's endpoints. Within a
	// band, baseline IOPS is 3 per GiB — the endpoints are 3× the band edges
	// in every published row, which is the corroboration that lets
	// [GP2PerformanceForRDS] interpolate IOPS and refuse to interpolate
	// throughput.
	MinBaselineIOPS, MaxBaselineIOPS int32
	// MinThroughputMBps and MaxThroughputMBps are the band's published
	// throughput range. Parity is measured against the MAXIMUM: the table does
	// not say which size in the band gets which number, so the only value that
	// cannot under-provision is the largest one the band can deliver.
	MinThroughputMBps, MaxThroughputMBps int32
	// BurstIOPS is the credit-funded ceiling, 0 where the published table
	// reads "N/A (baseline exceeds burst)".
	BurstIOPS int32
}

// gp2Bands is the published gp2 table for one engine, or nil when AWS
// publishes no gp2 band for it.
//
// It is a FUNCTION returning a fresh slice rather than a package-level table,
// so this package keeps its "no package-level mutable state" property
// (TestNoUnexpectedPackageState) and no caller can mutate another's numbers.
//
// Transcribed verbatim from §2.4:
//
//	| Engine / size                        | Baseline IOPS | Throughput   | Burst  |
//	|--------------------------------------|---------------|--------------|--------|
//	| MariaDB/MySQL/PostgreSQL 5–399 GiB   | 100–1,197     | 128–250      | 3,000  |
//	| … 400–1,335 GiB                      | 1,200–4,005   | 512–1,000    | 12,000 |
//	| … 1,336–3,999 GiB                    | 4,008–11,997  | 1,000        | 12,000 |
//	| … 4,000–65,536 GiB                   | 12,000–64,000 | 1,000        | N/A    |
//	| SQL Server 334–999 GiB               | 1,002–2,997   | 250          | 3,000  |
//
// Oracle and Db2 are ABSENT on purpose. They appear in the striping table (200
// GiB and 400 GiB) and not in the gp2 table, and an engine that stripes at a
// different size cannot borrow MySQL's bands — that is trap 11 committed
// against a second engine. They refuse with [ReasonParityGP2BandUnpublished].
func gp2Bands(e Engine) []GP2Band {
	switch e.Family {
	case FamilyMySQL, FamilyMariaDB, FamilyPostgreSQL:
		return []GP2Band{
			{MinGiB: 5, MaxGiB: 399, MinBaselineIOPS: 100, MaxBaselineIOPS: 1197,
				MinThroughputMBps: 128, MaxThroughputMBps: 250, BurstIOPS: 3000},
			{MinGiB: 400, MaxGiB: 1335, MinBaselineIOPS: 1200, MaxBaselineIOPS: 4005,
				MinThroughputMBps: 512, MaxThroughputMBps: 1000, BurstIOPS: 12000},
			{MinGiB: 1336, MaxGiB: 3999, MinBaselineIOPS: 4008, MaxBaselineIOPS: 11997,
				MinThroughputMBps: 1000, MaxThroughputMBps: 1000, BurstIOPS: 12000},
			{MinGiB: 4000, MaxGiB: 65536, MinBaselineIOPS: 12000, MaxBaselineIOPS: 64000,
				MinThroughputMBps: 1000, MaxThroughputMBps: 1000, BurstIOPS: 0},
		}
	case FamilySQLServer:
		return []GP2Band{
			{MinGiB: 334, MaxGiB: 999, MinBaselineIOPS: 1002, MaxBaselineIOPS: 2997,
				MinThroughputMBps: 250, MaxThroughputMBps: 250, BurstIOPS: 3000},
		}
	}
	return nil
}

// GP2PerformanceRDS is what an RDS gp2 volume of a given size and engine
// actually delivers, per the published table.
//
// Note what is NOT here: any 334 GiB step, any 1,000 GiB burst cutoff, any
// 16,000 IOPS ceiling. Those are pkg/ebs's numbers for a raw EBS volume and
// they are wrong here in all three ways trap 11 names.
type GP2PerformanceRDS struct {
	Engine  string `json:"engine"`
	SizeGiB int64  `json:"sizeGiB"`
	// Band is the published row this size fell in.
	Band GP2Band `json:"band"`
	// Striped reports whether RDS spreads this allocation across
	// [StripingVolumes] volumes.
	Striped bool `json:"striped"`
	// BaselineIOPS is 3 IOPS per GiB, clamped into the band's published
	// endpoints. Every published band endpoint is exactly 3× its size edge,
	// so this interpolation reproduces the table rather than extending it.
	BaselineIOPS int32 `json:"baselineIOPS"`
	// BurstIOPS is the credit-funded ceiling — 12,000 in the striped regime,
	// where pkg/ebs would say 3,000. Equal to BaselineIOPS when the published
	// row says burst is N/A.
	BurstIOPS int32 `json:"burstIOPS"`
	// Burstable reports whether a burst bucket exists at all.
	Burstable bool `json:"burstable"`
	// ParityThroughputMBps is the throughput a conversion must preserve: the
	// band's published MAXIMUM. See [GP2Band].
	ParityThroughputMBps int32 `json:"parityThroughputMBps"`
	// MinThroughputMBps is the band's published minimum, carried so a report
	// can quote the range rather than a single number.
	MinThroughputMBps int32 `json:"minThroughputMBps"`
}

// GP2PerformanceForRDS models an RDS gp2 volume. ok is false when the engine
// and size fall outside every published band, which is a refusal
// ([ReasonParityGP2BandUnpublished]) and never a default.
func GP2PerformanceForRDS(e Engine, sizeGiB int64) (GP2PerformanceRDS, bool) {
	if sizeGiB <= 0 || sizeGiB > MaxParitySizeGiB {
		return GP2PerformanceRDS{}, false
	}
	for _, b := range gp2Bands(e) {
		if sizeGiB < b.MinGiB || sizeGiB > b.MaxGiB {
			continue
		}
		base := sizeGiB * 3
		if base < int64(b.MinBaselineIOPS) {
			base = int64(b.MinBaselineIOPS)
		}
		if base > int64(b.MaxBaselineIOPS) {
			base = int64(b.MaxBaselineIOPS)
		}
		p := GP2PerformanceRDS{
			Engine:               e.Family,
			SizeGiB:              sizeGiB,
			Band:                 b,
			Striped:              Stripes(e, sizeGiB),
			BaselineIOPS:         int32(base),
			BurstIOPS:            int32(base),
			ParityThroughputMBps: b.MaxThroughputMBps,
			MinThroughputMBps:    b.MinThroughputMBps,
		}
		if b.BurstIOPS > p.BaselineIOPS {
			p.BurstIOPS, p.Burstable = b.BurstIOPS, true
		}
		return p, true
	}
	return GP2PerformanceRDS{}, false
}

// StripingThresholdGiBFor is the engine-dependent threshold, and
// [NeverStripes] for RDS for SQL Server.
func StripingThresholdGiBFor(e Engine) int64 {
	switch e.Family {
	case FamilySQLServer:
		return NeverStripes
	case FamilyOracle:
		return StripingThresholdOracleGiB
	case FamilyMySQL, FamilyMariaDB, FamilyPostgreSQL, FamilyDb2:
		return StripingThresholdGiB
	}
	return 0 // unknown engine: not modelled at all
}

// Stripes reports whether this allocation is spread across
// [StripingVolumes] volumes.
func Stripes(e Engine, sizeGiB int64) bool {
	t := StripingThresholdGiBFor(e)
	if t <= 0 {
		return false
	}
	return sizeGiB >= t
}

// --- The gp3 regime --------------------------------------------------------

// GP3Regime is what gp3 delivers, and what it will accept, for one engine at
// one size.
type GP3Regime struct {
	Engine       string `json:"engine"`
	SizeGiB      int64  `json:"sizeGiB"`
	ThresholdGiB int64  `json:"thresholdGiB"` // NeverStripes for SQL Server
	Striped      bool   `json:"striped"`
	// BaselineIOPS and BaselineThroughputMBps are free and non-reducible: they
	// are what the volume delivers with nothing provisioned, and they are the
	// floor of the provisionable range where one exists.
	BaselineIOPS           int32 `json:"baselineIOPS"`
	BaselineThroughputMBps int32 `json:"baselineThroughputMBps"`
	// Provisionable reports whether ModifyDBInstance will accept an --iops or
	// --storage-throughput value at all. Below the striping threshold the
	// published columns read "N/A", and RDS for SQL Server is the one engine
	// that can provision at every size.
	Provisionable bool `json:"provisionable"`
	// Known is false for an engine this package does not model.
	Known bool `json:"known"`
}

// GP3RegimeFor returns the gp3 regime for an engine and size.
//
// Read the Provisionable field as the whole of trap 11's second half: below
// the threshold, gp3 IS 3,000 IOPS / 125 MiB/s and there is no knob. A parity
// checker that thinks it can buy its way to 250 MiB/s down there is describing
// a product AWS does not sell.
func GP3RegimeFor(e Engine, sizeGiB int64) GP3Regime {
	t := StripingThresholdGiBFor(e)
	if t == 0 {
		return GP3Regime{Engine: e.Family, SizeGiB: sizeGiB}
	}
	r := GP3Regime{
		Engine: e.Family, SizeGiB: sizeGiB, ThresholdGiB: t, Known: true,
		Striped:                Stripes(e, sizeGiB),
		BaselineIOPS:           GP3BaselineIOPS,
		BaselineThroughputMBps: GP3BaselineThroughputMBps,
	}
	switch {
	case t == NeverStripes:
		// "For every DB engine except RDS for SQL Server, you can provision
		// additional IOPS and storage throughput when storage size is at or
		// above the threshold value." SQL Server is the exception, in both
		// directions: it never stripes, and it can always provision.
		r.Provisionable = true
	case r.Striped:
		r.BaselineIOPS = GP3StripedBaselineIOPS
		r.BaselineThroughputMBps = GP3StripedBaselineThroughputMBps
		r.Provisionable = true
	}
	return r
}

// --- Configurations, and the gate in front of them -------------------------

// GP3Config is one gp3 storage configuration: what the volume delivers, and
// which parts of that had to be paid for.
//
// IOPS and ThroughputMBps are EFFECTIVE totals, not deltas. The Provisioned*
// flags say whether a value must be sent to ModifyDBInstance at all — a
// configuration sitting exactly on the baseline provisions nothing, costs
// nothing extra, and needs no envelope.
type GP3Config struct {
	SizeGiB               int64 `json:"sizeGiB"`
	IOPS                  int32 `json:"iops"`
	ThroughputMBps        int32 `json:"throughputMBps"`
	ProvisionedIOPS       bool  `json:"provisionedIOPS,omitempty"`
	ProvisionedThroughput bool  `json:"provisionedThroughput,omitempty"`
}

// Provisions reports whether this configuration asks AWS for anything above
// the free baseline. It is the predicate that decides whether the live
// envelope is required.
func (c GP3Config) Provisions() bool { return c.ProvisionedIOPS || c.ProvisionedThroughput }

// Validate reports whether AWS would accept this configuration for the given
// regime and envelope, and is the [ReasonParityNotProvisionableBelowThreshold]
// gate: a configuration that provisions below the striping threshold is an
// error here, so it can never become a proposal.
//
// env may be an unknown envelope; a configuration that provisions anything is
// then rejected, because §2.4's two published SQL Server ceilings contradict
// each other and neither may be hardcoded.
func (c GP3Config) Validate(r GP3Regime, env StorageEnvelope) error {
	switch {
	case !r.Known:
		return fmt.Errorf("rds: no gp3 regime is encoded for engine %q", r.Engine)
	case c.SizeGiB <= 0 || c.SizeGiB > MaxParitySizeGiB:
		return fmt.Errorf("rds: gp3 size %d GiB is outside the 1–%d GiB range", c.SizeGiB, MaxParitySizeGiB)
	case c.SizeGiB != r.SizeGiB:
		return fmt.Errorf("rds: gp3 config is for %d GiB but the regime was computed for %d GiB",
			c.SizeGiB, r.SizeGiB)
	case c.IOPS < r.BaselineIOPS:
		return fmt.Errorf("rds: gp3 %d IOPS is below the non-reducible %d IOPS baseline for a %d GiB %s "+
			"volume", c.IOPS, r.BaselineIOPS, c.SizeGiB, r.Engine)
	case c.ThroughputMBps < r.BaselineThroughputMBps:
		return fmt.Errorf("rds: gp3 %d MiB/s is below the non-reducible %d MiB/s baseline for a %d GiB %s "+
			"volume", c.ThroughputMBps, r.BaselineThroughputMBps, c.SizeGiB, r.Engine)
	case c.ProvisionedIOPS != (c.IOPS > r.BaselineIOPS):
		return fmt.Errorf("rds: gp3 config claims provisionedIOPS=%v at %d IOPS against a %d IOPS baseline",
			c.ProvisionedIOPS, c.IOPS, r.BaselineIOPS)
	case c.ProvisionedThroughput != (c.ThroughputMBps > r.BaselineThroughputMBps):
		return fmt.Errorf("rds: gp3 config claims provisionedThroughput=%v at %d MiB/s against a %d MiB/s "+
			"baseline", c.ProvisionedThroughput, c.ThroughputMBps, r.BaselineThroughputMBps)
	}
	if !c.Provisions() {
		return nil
	}
	if !r.Provisionable {
		return fmt.Errorf("rds: a %d GiB %s volume is below the %d GiB striping threshold, where the "+
			"published provisioning columns read \"N/A\": %d IOPS / %d MiB/s cannot be provisioned there at "+
			"any price", c.SizeGiB, r.Engine, r.ThresholdGiB, c.IOPS, c.ThroughputMBps)
	}
	if !env.Known {
		return fmt.Errorf("rds: %d IOPS / %d MiB/s must be checked against "+
			"DescribeValidDBInstanceModifications, which was not read: AWS publishes two contradictory gp3 "+
			"ceilings and this package hardcodes neither", c.IOPS, c.ThroughputMBps)
	}
	// The live envelope bounds the ceiling; the PUBLISHED baseline bounds the
	// floor. Taking the stricter of the two in each direction means a stale or
	// generous envelope can never talk this package below a documented floor.
	if lo := maxInt32(env.MinIOPS, r.BaselineIOPS); c.IOPS < lo {
		return fmt.Errorf("rds: %d IOPS is below the %d IOPS floor this instance reports", c.IOPS, lo)
	}
	if env.MaxIOPS > 0 && c.IOPS > env.MaxIOPS {
		return fmt.Errorf("rds: %d IOPS exceeds the %d IOPS ceiling this instance reports", c.IOPS, env.MaxIOPS)
	}
	if lo := maxInt32(env.MinThroughputMBps, r.BaselineThroughputMBps); c.ThroughputMBps < lo {
		return fmt.Errorf("rds: %d MiB/s is below the %d MiB/s floor this instance reports",
			c.ThroughputMBps, lo)
	}
	if env.MaxThroughputMBps > 0 && c.ThroughputMBps > env.MaxThroughputMBps {
		return fmt.Errorf("rds: %d MiB/s exceeds the %d MiB/s ceiling this instance reports",
			c.ThroughputMBps, env.MaxThroughputMBps)
	}
	return nil
}

// Demand is measured, headroom-applied I/O demand a configuration must clear.
// Both fields are what the volume was observed to need, never what it was
// provisioned for.
type Demand struct {
	IOPS           float64 `json:"iops"`
	ThroughputMBps float64 `json:"throughputMBps"`
}

// valid reports whether demand is usable arithmetic. NaN, ±Inf and negatives
// are refused rather than clamped: they mean the measurement is broken, and a
// broken measurement must not become a provisioning decision.
func (d Demand) valid() bool {
	for _, v := range []float64{d.IOPS, d.ThroughputMBps} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return true
}

// Max returns the element-wise maximum, used to fold a floor into demand.
func (d Demand) Max(o Demand) Demand {
	return Demand{IOPS: math.Max(d.IOPS, o.IOPS), ThroughputMBps: math.Max(d.ThroughputMBps, o.ThroughputMBps)}
}

// Clears reports whether the configuration meets demand in BOTH dimensions.
// This is the predicate the whole unit turns on: a configuration that does not
// clear demand is not a candidate, cheaper or not.
func (c GP3Config) Clears(d Demand) bool {
	if !d.valid() {
		return false
	}
	return float64(c.IOPS) >= d.IOPS-parityEps &&
		float64(c.ThroughputMBps) >= d.ThroughputMBps-parityEps
}

// ParityFloor selects how much of the current configuration's delivered
// performance a proposal must preserve regardless of what was measured.
type ParityFloor int

const (
	// ParityFloorMeasured provisions to measured demand alone. Legitimate only
	// once the window is long enough to have contained the peak, which
	// [ParityConfig.MinWindow] enforces.
	ParityFloorMeasured ParityFloor = iota
	// ParityFloorNameplate never provisions below what the CURRENT
	// configuration delivers. Thin evidence uses it, so a conversion made on
	// weak evidence cannot degrade anything.
	ParityFloorNameplate
)

func (f ParityFloor) String() string {
	if f == ParityFloorNameplate {
		return "nameplate"
	}
	return "measured"
}

// configFor is the smallest valid gp3 configuration that clears d. It is the
// ONLY place a configuration is synthesized, so "never below demand" is one
// rule in one function: demand rounds UP, and the regime baseline is a floor
// that is never crossed downward.
func configFor(r GP3Regime, d Demand) GP3Config {
	iops := ceilInt32(d.IOPS)
	if iops < r.BaselineIOPS {
		iops = r.BaselineIOPS
	}
	tput := ceilInt32(d.ThroughputMBps)
	if tput < r.BaselineThroughputMBps {
		tput = r.BaselineThroughputMBps
	}
	return GP3Config{
		SizeGiB: r.SizeGiB, IOPS: iops, ThroughputMBps: tput,
		ProvisionedIOPS:       iops > r.BaselineIOPS,
		ProvisionedThroughput: tput > r.BaselineThroughputMBps,
	}
}

// ceilInt32 rounds up and saturates rather than wrapping. Demand always rounds
// UP: rounding a fractional IOPS demand down is exactly the silent degradation
// this unit exists to prevent.
func ceilInt32(v float64) int32 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	c := math.Ceil(v - parityEps)
	if c > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(c)
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// --- Money -----------------------------------------------------------------

// PerformanceRates prices the two gp3 knobs the storage $/GiB-month rate on
// [RateCard] does not cover.
//
// They live here rather than on [StorageRates] because they are not a property
// of stored bytes: they are the price of a provisioned NUMBER, charged only
// above the free baseline, and the baseline itself is engine- and
// size-dependent (this file's whole subject). §7 lists "RDS gp3 storage,
// provisioned-IOPS and provisioned-throughput rates" among the figures that
// could not be retrieved from AWS, so the shipped values carry
// [RateUnverified] and can size a fact while never becoming a saving — the
// same rule U11 encodes for every other RDS dollar.
type PerformanceRates struct {
	// ProvisionedIOPSMonthUSD is charged per IOPS ABOVE the regime baseline.
	ProvisionedIOPSMonthUSD float64 `json:"provisionedIOPSMonthUSD"`
	// ProvisionedThroughputMonthUSD is charged per MiB/s ABOVE the regime
	// baseline.
	ProvisionedThroughputMonthUSD float64        `json:"provisionedThroughputMonthUSD"`
	Provenance                    RateProvenance `json:"provenance,omitempty"`
}

// DefaultPerformanceRates returns the embedded, unverified figures. Like
// [DefaultRates] they are a magnitude and not an invoice, and
// TestEveryShippedParityRateIsUnverified pins that no future edit promotes a
// row without also verifying it.
func DefaultPerformanceRates() PerformanceRates {
	return PerformanceRates{
		ProvisionedIOPSMonthUSD:       0.02,
		ProvisionedThroughputMonthUSD: 0.08,
		Provenance:                    RateUnverified,
	}
}

// Validate rejects a rate table that would turn a savings claim into fiction.
func (p PerformanceRates) Validate() error {
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"provisionedIOPSMonthUSD", p.ProvisionedIOPSMonthUSD},
		{"provisionedThroughputMonthUSD", p.ProvisionedThroughputMonthUSD},
	} {
		if !finite(f.v) || f.v <= 0 {
			return fmt.Errorf("rds: parity rate %s must be positive and finite, got %v", f.name, f.v)
		}
	}
	if p.Provenance == "" {
		return fmt.Errorf("rds: parity rates carry no provenance; a rate with no provenance cannot be " +
			"gated and would become a claim by omission")
	}
	return nil
}

// CostPart is one named component of a monthly storage bill. Costs are summed
// through [SumUSD] in a defined order rather than accumulated as they are
// produced, because float64 addition is not associative and a total that
// changes with arrival order is a total nobody can reconcile twice.
type CostPart struct {
	Name string  `json:"name"`
	USD  float64 `json:"usd"`
}

// SumUSD adds cost parts in NAME order, so the same set of parts always
// produces the same last bit. TestParityCostSumIsShuffleInvariant proves it
// over every permutation of a four-part bill.
func SumUSD(parts []CostPart) float64 {
	ordered := append([]CostPart(nil), parts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].USD < ordered[j].USD
	})
	var total float64
	for _, p := range ordered {
		total += zeroIfNotFinite(p.USD)
	}
	return total
}

// GP2CostParts prices an RDS gp2 volume. gp2 charges for allocated size alone:
// its baseline and burst performance are included, which is precisely why a
// conversion that has to BUY the same performance on gp3 can cost more.
func GP2CostParts(card RateCard, sizeGiB int64) ([]CostPart, RateProvenance, bool) {
	total, prov, ok := card.StorageMonthlyUSD(StorageGP2, sizeGiB)
	if !ok {
		return nil, "", false
	}
	return []CostPart{{Name: "storage", USD: total}}, prov, true
}

// GP3CostParts prices a gp3 configuration: allocated size, plus IOPS and
// throughput above the regime's free baseline.
func GP3CostParts(card RateCard, perf PerformanceRates, r GP3Regime, c GP3Config) (
	[]CostPart, RateProvenance, bool) {

	total, prov, ok := card.StorageMonthlyUSD(StorageGP3, c.SizeGiB)
	if !ok {
		return nil, "", false
	}
	parts := []CostPart{{Name: "storage", USD: total}}
	if extra := c.IOPS - r.BaselineIOPS; extra > 0 {
		parts = append(parts, CostPart{Name: "iops", USD: float64(extra) * perf.ProvisionedIOPSMonthUSD})
		prov = prov.weakest(perf.Provenance)
	}
	if extra := c.ThroughputMBps - r.BaselineThroughputMBps; extra > 0 {
		parts = append(parts, CostPart{
			Name: "throughput", USD: float64(extra) * perf.ProvisionedThroughputMonthUSD})
		prov = prov.weakest(perf.Provenance)
	}
	return parts, prov, true
}
