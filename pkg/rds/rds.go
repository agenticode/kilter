// Package rds observes Amazon RDS DB instances and reports what it cannot
// safely change — which, in this domain, is nearly everything.
//
// # The refusal report is the product
//
// Every other cloud domain Kilter ships (pkg/ec2, pkg/ebs, pkg/ecs,
// pkg/lambda) exists to produce a resize. This one does not. RDS is where the
// resizable thing and the safe thing are furthest apart:
//
//   - The instance class is where the money is, and changing it is a FAILOVER.
//     "The RDS instance was modified by customer — An RDS DB instance
//     modification triggered a failover"; "Failover times are typically
//     60–120 seconds"; "The failover mechanism automatically changes the Domain
//     Name System (DNS) record … you need to re-establish any existing
//     connections to your DB instance" [all verified:
//     Concepts.MultiAZ.Failover.html]. [domain.ActionClass] has four members
//     and none of them describes that: [domain.ActionStopStart] would let an
//     executor budget a failover like an EC2 restart, and pkg/domain's own
//     comment says a domain "must never understate it". So the DB instance
//     class is not merely forbidden here, it is UNREPRESENTABLE — [Proposal]
//     has no field for it (TestProposalCannotNameAnInstanceClass).
//   - Allocated storage only ever ratchets UP. "You can't reduce the amount of
//     storage for a DB instance after storage has been allocated" [verified:
//     USER_PIOPS.Autoscaling.html]. A 4 TiB instance holding 300 GiB is a real,
//     large, reportable number and NOT a recommendation, because the only
//     remediation is a blue/green cutover or a migration.
//   - `FreeableMemory` is `MemAvailable`, which counts reclaimable page cache
//     as available. On PostgreSQL that makes a fully-cached working set look
//     like spare memory. See [MemorySemantics].
//   - Aurora shares `DescribeDBInstances` and most of the CloudWatch namespace
//     and is a third billing model wearing RDS's name. It is refused by name.
//
// What this package therefore produces is a report whose refusals carry
// dollars: "here is what this costs, here is exactly why we will not touch it,
// and here is the measurement or the API that does not exist."
//
// # Advisory only, structurally
//
// There is no actuator, no mutating seam, and no flag that would enable one.
// `rds:ModifyDBInstance` has no representation anywhere in this package
// (TestNoMutatingAPISurface). [Domain.PlanSteps] returns [domain.ErrReportOnly]
// unconditionally, [Domain.Health] reports ReportOnly true unconditionally,
// and [Report.Validate] rejects any assessment carrying an action class other
// than [domain.ActionAdvisory].
//
// The package links no AWS SDK and makes no network call. Its three read seams
// ([InventoryAPI], [MetricsAPI], [CommitmentAPI]) are plain Go interfaces over
// plain Go structs shaped after `rds:DescribeDBInstances` /
// `rds:DescribeDBClusters` / `rds:ListTagsForResource`,
// `cloudwatch:GetMetricData` and `rds:DescribeReservedDBInstances`, so the
// decision path links into the air-gapped binary and the SDK adapter lives in
// cmd/ wiring a later unit adds (FINDINGS.md §6).
//
// # Money
//
// RDS on-demand instance rates are marked **[unverified]** in
// docs/design/rds-batch-assessment.md §7: "the pricing tables are JS-rendered
// and were not retrievable". This package encodes that as a REFUSAL rather
// than as an estimate. [RateCard] stamps every rate with its [RateProvenance];
// an unverified rate may size a reported fact ("this idle instance is on the
// order of $X/mo") and may never become a claimed saving
// ([ReasonUnverifiedRate], TestUnverifiedRatesNeverBecomeASaving).
//
// # Determinism
//
// No clock: callers pass `now`. No package-level mutable state beyond fixed
// tables. Every iteration order is sorted by an intrinsic key, so shuffling
// instances, metric results or tags cannot change a byte of the report —
// pinned by TestReportIsShuffleInvariant.
package rds

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Kind names this compute domain.
//
// It is deliberately NOT one of pkg/domain's five declared kinds. RDS is a
// billable domain of its own — it is not EC2 (pkg/ebs's route, legitimate
// there because no commitment covers a volume) and it is not Fargate or
// Lambda. Adding `RDS Kind = "rds"` to pkg/domain's closed set is a one-line
// core change this unit may not make; see FINDINGS.md §6 and
// TestKindIsHonestAboutRegistration, which passes whether or not that change
// has landed.
const Kind = domain.Kind("rds")

// HoursPerMonth converts hourly to monthly cost using the billing-average
// month (8760 h/year ÷ 12), matching pkg/pricing and pkg/pricing/commit.
const HoursPerMonth = commit.HoursPerMonth

// CloudWatch facts about the AWS/RDS namespace [verified:
// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/monitoring-cloudwatch.html
// and .../rds-metrics.html].
const (
	// NamespaceRDS is where every metric this package reads lives.
	NamespaceRDS = "AWS/RDS"

	// PublicationPeriodSeconds is RDS's default: "By default, Amazon RDS
	// automatically sends metric data to CloudWatch in 1-minute periods."
	// No paid feature, no CloudWatch agent — which is why this domain carries
	// none of pkg/ec2's 25 % coarse-resolution headroom.
	PublicationPeriodSeconds int32 = 60

	// CreditPeriodSeconds is the floor for the burstable credit metrics:
	// "CPU credit metrics are available at a five-minute frequency only."
	// Asking for 60 s there returns gaps, not detail.
	CreditPeriodSeconds int32 = 300

	// RetentionAtOneMinute is the hard limit on how far back a 1-minute
	// series reaches: "Data points with a period of 60 seconds (1 minute) are
	// available for 15 days." A collector configured for a 30-day window at
	// 60 s does not get 30 days of data — it gets 15 days of data and 15 days
	// of silence, and silence read as "no usage" is how an idle verdict is
	// manufactured out of nothing. [ClampWindow] refuses to let that happen.
	RetentionAtOneMinute = 15 * 24 * time.Hour
)

// Metric names this package reads, all in [NamespaceRDS].
const (
	MetricCPUUtilization     = "CPUUtilization"
	MetricFreeableMemory     = "FreeableMemory"
	MetricFreeStorageSpace   = "FreeStorageSpace"
	MetricDatabaseConns      = "DatabaseConnections"
	MetricReadIOPS           = "ReadIOPS"
	MetricWriteIOPS          = "WriteIOPS"
	MetricReadThroughput     = "ReadThroughput"
	MetricWriteThroughput    = "WriteThroughput"
	MetricSwapUsage          = "SwapUsage"
	MetricBurstBalance       = "BurstBalance"
	MetricCPUCreditBalance   = "CPUCreditBalance"
	MetricEBSIOBalance       = "EBSIOBalance%"
	MetricEBSByteBalance     = "EBSByteBalance%"
	MetricReplicaLagSeconds  = "ReplicaLag"
	MetricNetworkReceive     = "NetworkReceiveThroughput"
	MetricNetworkTransmitted = "NetworkTransmitThroughput"
)

// Storage types RDS bills.
const (
	StorageGP2      = "gp2"
	StorageGP3      = "gp3"
	StorageIO1      = "io1"
	StorageIO2      = "io2"
	StorageMagnetic = "standard"
)

// DB instance states that make a reading meaningless or a change impossible
// [verified: USER_ModifyInstance.Settings.html — "You can't modify allocated
// storage if the DB instance status is storage-optimization"].
const (
	StatusAvailable           = "available"
	StatusStorageOptimization = "storage-optimization"
	StatusModifying           = "modifying"
	StatusStopped             = "stopped"
)

// Attr keys used in [domain.Spec.Attrs].
const (
	AttrClass                = "dbInstanceClass"
	AttrEngine               = "engine"
	AttrEngineVersion        = "engineVersion"
	AttrLicenseModel         = "licenseModel"
	AttrDeployment           = "deployment"
	AttrAllocatedStorageGiB  = "allocatedStorageGiB"
	AttrMaxAllocatedStorage  = "maxAllocatedStorageGiB"
	AttrStorageType          = "storageType"
	AttrIOPS                 = "iops"
	AttrStorageThroughput    = "storageThroughputMBps"
	AttrReplicaOf            = "readReplicaSource"
	AttrClusterID            = "dbClusterIdentifier"
	AttrMultiAZ              = "multiAZ"
	AttrStorageAutoscalingOn = "storageAutoscaling"
)

// TagKilterMode is the opt-out tag, mirroring the annotation guardrail every
// other domain honors.
const TagKilterMode = "kilter.dev/mode"

// Reason codes. They are stable strings meant to be stored, matched on and
// asserted against; the prose in [Suppression.Reason] is not.
//
// Codes divide into two groups. An EXCLUSION means this domain does not model
// the target at all and fires alone — nothing else is said about that
// instance, because everything else would be said in a vocabulary that does
// not apply to it. A REFUSAL means the target is modelled and a specific
// change was declined for a specific reason.
const (
	// --- Exclusions (fire alone) ---

	// ReasonAuroraNotSupported: trap 16. Aurora shares DescribeDBInstances and
	// most of the CloudWatch namespace and is a different billing model. An
	// Aurora Serverless v2 "instance" has no class to resize — capacity is
	// ACUs in 0.5 steps, billed per second, able to scale to zero [verified:
	// aurora-serverless-v2.html] — and Aurora storage is cluster-managed, so
	// neither the class arithmetic nor the storage ratchet in this package
	// describes it. Refused by name; Aurora gets its own unit.
	ReasonAuroraNotSupported = "aurora-not-supported"

	// ReasonClusterMemberNotSupported: a member of a Multi-AZ DB CLUSTER (one
	// writer, two readers) on a non-Aurora engine. The reserved-instance table
	// prices that deployment at 3× the Single-AZ units of the same size, which
	// is a property of the cluster and not of any one member, and this unit
	// models per-instance economics only. Distinct from
	// [ReasonAuroraNotSupported] because calling a PostgreSQL Multi-AZ cluster
	// "Aurora" would be a false statement in a report whose whole value is
	// that its statements are true.
	ReasonClusterMemberNotSupported = "cluster-member-not-supported"

	// ReasonModeOff: tagged kilter.dev/mode=off.
	ReasonModeOff = "guardrail-mode-off"

	// ReasonUnknownEngine: the engine string is not one this package has
	// encoded. It is refused rather than treated as "probably like MySQL" —
	// the t2-baseline precedent from pkg/ec2/FINDINGS.md §5.
	ReasonUnknownEngine = "unknown-engine"

	// ReasonUnknownInstanceClass: the DB instance class does not parse as
	// db.<class>.<size>, so it has no normalized units, no reservation
	// matching and no rate. No price ⇒ no bill delta ⇒ nothing to claim.
	ReasonUnknownInstanceClass = "unknown-instance-class"

	// ReasonEngineNotPriced: the engine is understood but this package ships
	// no rate for it. §2.8: a db.r6i.xlarge running SQL Server Enterprise
	// license-included and one running PostgreSQL are the same hardware at
	// different prices, and the honest v1 is "ship the engines whose rates we
	// have, refuse the rest by name" rather than quote an open-source rate for
	// a licensed engine.
	ReasonEngineNotPriced = "engine-not-priced"

	// ReasonUnknownDeployment: the deployment topology could not be
	// determined. Guessing Single-AZ would halve both the cost and the saving
	// of a Multi-AZ instance (trap 10), so it is refused.
	ReasonUnknownDeployment = "unknown-deployment"

	// --- Refusals ---

	// ReasonUnverifiedRate: the rate that sized this instance is
	// [RateUnverified]. docs/design/rds-batch-assessment.md §7 marks RDS
	// on-demand instance rates, the Multi-AZ price ratio and every gp3 storage
	// rate as unverified. A magnitude may be reported from one; a saving may
	// not be claimed from one.
	ReasonUnverifiedRate = "unverified-rate"

	// ReasonInstanceClassIsAFailover: trap 12. The class is where the money
	// is and changing it drops every pooled connection via a DNS change whose
	// client-side TTL is unobservable from any API. Permanently advisory.
	ReasonInstanceClassIsAFailover = "instance-class-change-is-a-failover"

	// ReasonFreeableMemoryIsPageCache: trap 9, PostgreSQL side. FreeableMemory
	// is MemAvailable, which counts reclaimable page cache as available; a
	// well-tuned PostgreSQL instance with a fully-cached hot working set
	// reports LARGE freeable memory. Downsizing on that evidence evicts the
	// cache and moves cost from the instance line to the I/O and latency
	// lines. The series is not headroom and is not read as headroom.
	ReasonFreeableMemoryIsPageCache = "freeable-memory-is-page-cache"

	// ReasonBufferPoolScalesWithClass: trap 9, MySQL/MariaDB side. InnoDB
	// holds its buffer pool as anonymous memory, which MemAvailable does not
	// count as available, so here FreeableMemory IS close to real headroom —
	// but the pool is sized from the parameter group as a fraction of instance
	// memory [unverified: the default parameter group's
	// innodb_buffer_pool_size formula], so shrinking the class shrinks the
	// pool, which changes the I/O profile, which changes CPU. The signal is
	// honest and the effect of acting on it is not linear.
	ReasonBufferPoolScalesWithClass = "buffer-pool-scales-with-class"

	// ReasonMemorySemanticsUnencoded: trap 9's default. An engine whose
	// FreeableMemory semantics this package has not encoded refuses rather
	// than guesses — SQL Server and Oracle run their own memory managers.
	ReasonMemorySemanticsUnencoded = "engine-memory-semantics-unencoded"

	// ReasonStorageCannotShrink: trap 8. "You can't reduce the amount of
	// storage for a DB instance after storage has been allocated" and "you
	// can't manually reduce the allocated storage of a DB instance using the
	// modify-db-instance command" [both verified]. FreeStorageSpace is
	// therefore never read as a saving.
	ReasonStorageCannotShrink = "allocated-storage-cannot-shrink"

	// ReasonStorageAutoscalingRatchet: trap 8's second half. Storage
	// autoscaling moves the floor on its own and "autoscaling operations
	// aren't logged by AWS CloudTrail" [verified], so an allocated-storage
	// increase between two snapshots is UNATTRIBUTED by default — never
	// Kilter's doing and never a regression.
	ReasonStorageAutoscalingRatchet = "storage-autoscaling-ratchet"

	// ReasonReplicaIsFailoverCapacity: §2.5. A read replica's size is usually
	// chosen so it can be PROMOTED, not to fit its own workload.
	// Recommending a smaller replica on utilization evidence proposes an
	// instance that cannot absorb a failover — a correctness claim about an
	// availability property no API exposes.
	ReasonReplicaIsFailoverCapacity = "replica-is-failover-capacity"

	// ReasonMultiAZIsAvailabilityPosture: §2.5. Turning Multi-AZ off halves a
	// bill and halves an SLA. It is mechanically cheap ("Downtime doesn't
	// occur during this change") and it is a deliberate availability decision,
	// so this package represents it as a fact and never as a lever.
	ReasonMultiAZIsAvailabilityPosture = "multi-az-is-availability-posture"

	// ReasonInsufficientWindow: the observed span is too short to characterize
	// a database. A day of metrics does not contain a weekly batch job.
	ReasonInsufficientWindow = "insufficient-window"

	// ReasonNoMetricEvidence: no usable CloudWatch series arrived. Reported as
	// a refusal, never as "this instance is idle".
	ReasonNoMetricEvidence = "no-metric-evidence"

	// ReasonTruncatedMetrics: CloudWatch did not answer for a metric this
	// verdict depends on. A missing result is truncation, not an empty
	// metric — the distinction four domains have each had to re-derive.
	ReasonTruncatedMetrics = "truncated-metric-response"

	// ReasonSizeFlexibilityExcluded: trap 13. "Size flexibility does not apply
	// to RDS for SQL Server and RDS for Oracle License Included" [verified],
	// so a downsize inside the class type that a PostgreSQL reservation would
	// partly absorb strands 100 % here.
	ReasonSizeFlexibilityExcluded = "size-flexibility-excluded"

	// ReasonInstanceStateUnstable: the instance is mid-modification or
	// storage-optimizing, so its metrics describe a transient shape.
	ReasonInstanceStateUnstable = "instance-state-unstable"

	// ReasonNoStoragePerformanceModel: trap 11, stated as a refusal rather
	// than filled with a borrowed table. RDS stripes across four volumes above
	// an engine-dependent threshold (400 GiB, 200 GiB for Oracle, never for
	// SQL Server), its gp2 burst reaches 12,000 IOPS rather than EBS's 3,000
	// and its throughput reaches 1,000 MiB/s rather than 250, and gp3 cannot
	// be provisioned AT ALL below the threshold [all verified: CHAP_Storage.html].
	// Reusing pkg/ebs/parity.go's constants would claim a saving in the band
	// where RDS loses throughput and refuse in the band where RDS converts
	// cleanly. U13 owns the real table; until then this domain says so.
	ReasonNoStoragePerformanceModel = "no-storage-performance-model"

	// ReasonCommitmentNegative / ReasonCommitmentNeutral are re-exported from
	// the commitment waterfall. RDS's product is the Reserved DB Instance and
	// nothing else: no Savings Plan of any type covers RDS.
	ReasonCommitmentNegative = domain.SuppressCommitmentNegative
	ReasonCommitmentNeutral  = domain.SuppressCommitmentNeutral
)

// Advisory codes. An advisory is reported and never actuated — see [Advisory].
const (
	// AdvisoryIdleInstance: DatabaseConnections ≡ 0 across the whole window.
	// The metric "under-counts sessions (engine-internal, parallel-execution
	// and scheduler sessions are excluded)" [verified: rds-metrics.html],
	// which is the SAFE direction for an idle test: a database that looks
	// busy might be idle, but one that looks idle is idle. Carries the full
	// monthly cost and no class proposal — RDS instances are data-bearing and
	// §1 invariant 5 forbids idle-instance shutdown outright.
	AdvisoryIdleInstance = "idle-instance"

	// AdvisoryIdleReadReplica: the same signature on a read replica, reported
	// DISTINCTLY. A replica with zero connections is either unused or a pure
	// standby, and either way that is a different conversation from an idle
	// primary — it is the one replica finding §2.5 says is safe to state.
	AdvisoryIdleReadReplica = "idle-read-replica"

	// AdvisoryStorageFloor: allocated storage far exceeds used storage. A
	// quantified fact with an explicit "the only remediation is blue/green or
	// a migration" caveat, never a recommendation.
	AdvisoryStorageFloor = "allocated-storage-floor"

	// AdvisoryStorageAutoscaling: MaxAllocatedStorage > AllocatedStorage. The
	// ratchet can move without any Kilter action and without any CloudTrail
	// event; the advisory quotes the documented trigger so a reader can tell
	// a Kilter change from an autoscaling event.
	AdvisoryStorageAutoscaling = "storage-autoscaling-enabled"

	// AdvisoryMultiAZMultiplier: trap 10 made visible. The instance line is
	// doubled; storage, backups and I/O are not.
	AdvisoryMultiAZMultiplier = "multi-az-doubles-the-instance-line"

	// AdvisoryUnverifiedRate: every dollar figure on this target came from an
	// unverified rate. Reported once per target so no reader mistakes an
	// order-of-magnitude for an invoice.
	AdvisoryUnverifiedRate = "unverified-rate-magnitude"

	// AdvisoryReservationStranding: a reservation covering this instance's
	// class type would strand under a downsize this domain will not propose.
	// Reported so the arithmetic is visible even though the change is not.
	AdvisoryReservationStranding = "reservation-would-strand"
)

// Risk levels, matching the strings pkg/plan uses.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// Evidence sources.
const (
	SourceCloudWatch     = "cloudwatch"
	SourceDescribe       = "describe-db-instances"
	SourceTags           = "list-tags-for-resource"
	SourceLedger         = "commitment-ledger"
	SourceRateCard       = "rds-rate-card"
	SourceAWSDocumented  = "aws-documented-behaviour"
	SourceReservedDBDesc = "describe-reserved-db-instances"
)

// Suppression is a stated reason a change was not proposed, or a stronger one
// withheld. ValidFrom is set when the block is dated — a commitment term that
// expires — so the suppression lapses on its own.
type Suppression struct {
	Code      string    `json:"code"`
	Reason    string    `json:"reason"`
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

// Advisory is a finding that is reported and never actuated. An advisory must
// always carry a Caveat — [Report.Validate] rejects a report where one does
// not, because an advisory stripped of its caveat reads as an actionable
// saving.
type Advisory struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Caveat  string `json:"caveat"`
	// MonthlyUSD is a magnitude, not a saving: the monthly cost of the thing
	// being described. It is never summed into a claim and it is never
	// negative-signed to look like a delta.
	MonthlyUSD float64 `json:"monthlyUSD,omitempty"`
	// RateProvenance records whether MonthlyUSD came from a rate anyone has
	// actually verified. See [RateCard].
	RateProvenance RateProvenance `json:"rateProvenance,omitempty"`
}

// Actuatable is false for every advisory, always. It is a method rather than a
// field so no serialized form and no future struct literal can claim otherwise.
func (Advisory) Actuatable() bool { return false }

// Action reports the advisory action class.
func (Advisory) Action() domain.ActionClass { return domain.ActionAdvisory }

// Window is the closed observation interval a snapshot covers.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Duration is the covered span, never negative.
func (w Window) Duration() time.Duration {
	if w.End.Before(w.Start) {
		return 0
	}
	return w.End.Sub(w.Start)
}

// Hours is the covered span in hours, never negative.
func (w Window) Hours() float64 { return w.Duration().Hours() }

func (w Window) String() string {
	if w.Duration() == 0 {
		return "empty"
	}
	return w.Duration().Round(time.Minute).String()
}

// ClampWindow shortens a window to what CloudWatch will actually answer at the
// given period, and reports whether it had to.
//
// At 60 s the reach is [RetentionAtOneMinute]; asking for more does not fail,
// it returns a shorter series inside a longer window — and a shorter series
// read across a longer window is how "this database had no connections for 30
// days" gets manufactured from 15 days of data and 15 days of silence.
//
// Periods above 60 s reach further back (3-hour datapoints for 455 days), so
// only the 1-minute case is clamped here; a collector that widens the period
// gets its window back. Callers pass the window, so nothing here reads a clock.
func ClampWindow(w Window, periodSeconds int32) (Window, bool) {
	if periodSeconds > PublicationPeriodSeconds || w.Duration() <= RetentionAtOneMinute {
		return w, false
	}
	return Window{Start: w.End.Add(-RetentionAtOneMinute), End: w.End}, true
}

// DBInstance is one observed RDS DB instance, normalized from
// DescribeDBInstances plus ListTagsForResource. It carries only what a verdict
// or a guardrail reads.
//
// There is deliberately no field for anything this package would need in order
// to WRITE: no pending-modification target class, no apply-immediately flag.
// The struct describes what was observed, not what could be sent.
type DBInstance struct {
	ARN        string `json:"arn"`
	Identifier string `json:"identifier"`
	Class      string `json:"dbInstanceClass"` // "db.r6i.large"
	// Engine is the raw DescribeDBInstances spelling: "postgres", "mysql",
	// "sqlserver-ee", "aurora-postgresql", "oracle-se2".
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion,omitempty"`
	// LicenseModel is "license-included", "bring-your-own-license" or
	// "general-public-license". It decides both the price band (§2.8) and
	// whether an Oracle reservation is size-flexible (§2.6).
	LicenseModel string `json:"licenseModel,omitempty"`
	Status       string `json:"status,omitempty"`
	Region       string `json:"region,omitempty"`

	// MultiAZ is the synchronous-standby topology. It is a 2× multiplier on
	// the instance line (trap 10), not a label.
	MultiAZ bool `json:"multiAZ,omitempty"`
	// ClusterID is non-empty for a member of a DB cluster — Aurora, or a
	// Multi-AZ DB cluster. Both are excluded, for different reasons.
	ClusterID string `json:"dbClusterIdentifier,omitempty"`
	// ReplicaSource is the identifier this instance replicates from, empty on
	// a primary.
	ReplicaSource string `json:"readReplicaSource,omitempty"`
	// Replicas are the identifiers replicating FROM this instance.
	Replicas []string `json:"readReplicas,omitempty"`

	AvailabilityZone string `json:"availabilityZone,omitempty"`

	// AllocatedStorageGiB is a monotone ratchet: it can grow and can never
	// shrink. See [ReasonStorageCannotShrink].
	AllocatedStorageGiB int64 `json:"allocatedStorageGiB,omitempty"`
	// MaxAllocatedStorageGiB is the storage-autoscaling ceiling. Greater than
	// AllocatedStorageGiB means autoscaling is ENABLED, which makes the floor
	// something that moves on its own.
	MaxAllocatedStorageGiB int64  `json:"maxAllocatedStorageGiB,omitempty"`
	StorageType            string `json:"storageType,omitempty"`
	IOPS                   int32  `json:"iops,omitempty"`
	StorageThroughputMBps  int32  `json:"storageThroughputMBps,omitempty"`

	InstanceCreateTime time.Time         `json:"instanceCreateTime,omitzero"`
	Tags               map[string]string `json:"tags,omitempty"`
}

// IsReplica reports whether this instance replicates from another.
func (d DBInstance) IsReplica() bool { return strings.TrimSpace(d.ReplicaSource) != "" }

// StorageAutoscaling reports whether the allocated-storage ratchet can move
// without anyone asking it to.
func (d DBInstance) StorageAutoscaling() bool {
	return d.MaxAllocatedStorageGiB > d.AllocatedStorageGiB && d.AllocatedStorageGiB > 0
}

// ModeOff reports the kilter.dev/mode=off tag guardrail.
func (d DBInstance) ModeOff() bool {
	return strings.EqualFold(strings.TrimSpace(d.Tags[TagKilterMode]), "off")
}

// StateUnstable reports whether the instance is mid-change, which makes its
// metrics describe a transient shape rather than a steady state.
func (d DBInstance) StateUnstable() bool {
	switch strings.ToLower(strings.TrimSpace(d.Status)) {
	case StatusModifying, StatusStorageOptimization:
		return true
	}
	return false
}

// Deployment returns the topology as pkg/pricing/commit models it, and false
// when it cannot be determined.
//
// The false case matters: [commit.RDSDeployment]'s zero value is deliberately
// not Single-AZ, because guessing Single-AZ for an instance whose topology was
// not reported would halve the units a line needs and make a reservation
// appear to cover twice the usage it really covers. This function inherits
// that discipline — an instance with no identifier at all reports false and is
// excluded rather than priced at half.
func (d DBInstance) Deployment() (commit.RDSDeployment, bool) {
	if strings.TrimSpace(d.Identifier) == "" && strings.TrimSpace(d.ARN) == "" {
		return "", false
	}
	if strings.TrimSpace(d.ClusterID) != "" {
		return commit.RDSMultiAZCluster, true
	}
	if d.MultiAZ {
		return commit.RDSMultiAZInstance, true
	}
	return commit.RDSSingleAZ, true
}

// DisplayName returns the instance identifier, falling back to its ARN.
func (d DBInstance) DisplayName() string {
	if d.Identifier != "" {
		return d.Identifier
	}
	return d.ARN
}

// Point is one metric datapoint.
type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// Series is one CloudWatch metric's observations for one instance, as
// delivered. The period is DATA, not configuration: it records the granularity
// CloudWatch actually published, which is how a reader can tell a 5-minute
// credit metric from a 1-minute utilization metric.
type Series struct {
	Metric        string  `json:"metric"`
	Stat          string  `json:"stat,omitempty"`
	Source        string  `json:"source,omitempty"`
	PeriodSeconds int32   `json:"periodSeconds,omitempty"`
	Points        []Point `json:"points,omitempty"`
	// Partial marks a series CloudWatch did not deliver in full. A partial
	// series never produces an "everything was quiet" verdict.
	Partial bool   `json:"partial,omitempty"`
	Status  string `json:"status,omitempty"`
}

// Len is the number of delivered datapoints.
func (s Series) Len() int { return len(s.Points) }

// Usable reports whether the series can back a verdict: delivered in full and
// non-empty.
func (s Series) Usable() bool { return !s.Partial && len(s.Points) > 0 }

// Max returns the largest observed value, and false when there are none.
func (s Series) Max() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	m := s.Points[0].Value
	for _, p := range s.Points[1:] {
		if p.Value > m {
			m = p.Value
		}
	}
	return m, true
}

// Min returns the smallest observed value, and false when there are none.
func (s Series) Min() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	m := s.Points[0].Value
	for _, p := range s.Points[1:] {
		if p.Value < m {
			m = p.Value
		}
	}
	return m, true
}

// Mean returns the arithmetic mean, and false when there are no points.
func (s Series) Mean() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	var sum float64
	for _, p := range s.Points {
		sum += p.Value
	}
	return sum / float64(len(s.Points)), true
}

// Percentile returns the p-th percentile (0..1) by nearest-rank over a sorted
// copy of the values. It allocates rather than sorting in place: a Series is
// shared across assessments and must never be reordered by being read.
func (s Series) Percentile(p float64) (float64, bool) {
	if len(s.Points) == 0 || p < 0 || p > 1 {
		return 0, false
	}
	vals := make([]float64, 0, len(s.Points))
	for _, pt := range s.Points {
		vals = append(vals, pt.Value)
	}
	sort.Float64s(vals)
	idx := int(float64(len(vals)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx], true
}

// AllZero reports whether every delivered point is zero, and whether the
// question could be answered at all. A partial or empty series answers false
// for `known`: silence is not evidence of quiet.
func (s Series) AllZero() (allZero, known bool) {
	if !s.Usable() {
		return false, false
	}
	for _, p := range s.Points {
		if p.Value != 0 {
			return false, true
		}
	}
	return true, true
}

// Target is one DB instance plus everything observed about it.
type Target struct {
	Ref      domain.TargetRef `json:"ref"`
	Instance DBInstance       `json:"instance"`
	// Cluster is what the collector learned about the cluster this instance
	// belongs to, if any. Empty for a standalone DB instance.
	Cluster ClusterInfo `json:"cluster,omitzero"`
	// Series is sorted by metric name.
	Series []Series `json:"series,omitempty"`
	// PriorAllocatedStorageGiB is the allocated storage the LAST snapshot of
	// this instance reported, or 0 on first sight. It exists so trap 8's
	// ledger rule has something to compare against: an allocated-storage
	// increase between two snapshots is unattributed by default, because
	// storage autoscaling moves the floor on its own and leaves no CloudTrail
	// event. See [AttributeStorageGrowth]. It is filled by [Domain.Observe],
	// never by the collector, because only the domain has a previous
	// observation to remember.
	PriorAllocatedStorageGiB int64 `json:"priorAllocatedStorageGiB,omitempty"`
	// StorageHistory is every allocated-storage observation of this instance
	// that survived the caller's retention, oldest first. It is the seam
	// FINDINGS.md §7.4 named: PriorAllocatedStorageGiB answers "did it move
	// since last time", and this answers "how far has it moved, over how long,
	// and in how many steps" — which is the only version of the question a
	// lumpy, thinned, one-way ratchet can be asked honestly.
	//
	// It is filled by the CALLER, from persisted checkpoints, and never by the
	// collector: a collector sees one instant and a growth is a claim about
	// several. Empty is the normal state of an unwired deployment and produces
	// [GrowthNoHistory] rather than a fleet of identical refusals. See
	// [StorageHistory.Append] and GROWTH-FINDINGS.md §6.
	StorageHistory StorageHistory `json:"storageHistory,omitempty"`
}

// SeriesFor returns the named series, and false when it was not delivered.
func (t Target) SeriesFor(metric string) (Series, bool) {
	i := sort.Search(len(t.Series), func(i int) bool { return t.Series[i].Metric >= metric })
	if i < len(t.Series) && t.Series[i].Metric == metric {
		return t.Series[i], true
	}
	return Series{}, false
}

// Snapshot is what a collector ships to the brain: the §5.2 domain snapshot,
// specialized to RDS targets.
type Snapshot struct {
	Domain    domain.Kind `json:"domain"`
	Scope     string      `json:"scope"` // accountID/region
	Region    string      `json:"region,omitempty"`
	Timestamp time.Time   `json:"timestamp"`
	Window    Window      `json:"window"`
	// Targets is sorted by instance ARN.
	Targets []Target `json:"targets,omitempty"`
	// Reservations are the account's Reserved DB Instances, sorted. They are
	// carried on the snapshot rather than fetched by the sizer because the
	// sizer is pure.
	Reservations []commit.ReservedDBInstance `json:"reservations,omitempty"`
	// Stale marks a snapshot the collector could not complete.
	Stale bool `json:"stale,omitempty"`
	// Warnings are human-readable collection problems, sorted and deduped.
	Warnings []string `json:"warnings,omitempty"`
}

// SortTargets puts targets and everything inside them in canonical order, so
// two collectors that walked the same account in a different order ship the
// same snapshot.
func SortTargets(ts []Target) {
	for i := range ts {
		sort.SliceStable(ts[i].Series, func(a, b int) bool {
			return ts[i].Series[a].Metric < ts[i].Series[b].Metric
		})
		sort.Strings(ts[i].Instance.Replicas)
	}
	sort.SliceStable(ts, func(a, b int) bool { return ts[a].Ref.ID < ts[b].Ref.ID })
}

// SortReservations orders reservations by an intrinsic key so a shuffled
// DescribeReservedDBInstances response cannot change a byte of the report.
func SortReservations(rs []commit.ReservedDBInstance) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].ID != rs[j].ID {
			return rs[i].ID < rs[j].ID
		}
		if rs[i].DBInstanceClass != rs[j].DBInstanceClass {
			return rs[i].DBInstanceClass < rs[j].DBInstanceClass
		}
		return rs[i].Engine < rs[j].Engine
	})
}

// sortWarnings deduplicates and sorts.
func sortWarnings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, w := range in {
		if seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// copyTags returns a private copy of a tag map, or nil.
func copyTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SpecFor renders a DB instance as a domain Spec. It is the CURRENT shape and
// there is no counterpart that renders a proposed one, because the only fields
// this package would change are storage-performance fields a later unit owns.
func SpecFor(d DBInstance) domain.Spec {
	s := domain.Spec{Attrs: map[string]string{AttrClass: d.Class}}
	if d.Engine != "" {
		s.Attrs[AttrEngine] = d.Engine
	}
	if d.EngineVersion != "" {
		s.Attrs[AttrEngineVersion] = d.EngineVersion
	}
	if d.LicenseModel != "" {
		s.Attrs[AttrLicenseModel] = d.LicenseModel
	}
	if dep, ok := d.Deployment(); ok {
		s.Attrs[AttrDeployment] = string(dep)
	}
	if d.MultiAZ {
		s.Attrs[AttrMultiAZ] = "true"
	}
	if d.AllocatedStorageGiB > 0 {
		s.Attrs[AttrAllocatedStorageGiB] = strconv.FormatInt(d.AllocatedStorageGiB, 10)
	}
	if d.MaxAllocatedStorageGiB > 0 {
		s.Attrs[AttrMaxAllocatedStorage] = strconv.FormatInt(d.MaxAllocatedStorageGiB, 10)
		s.Attrs[AttrStorageAutoscalingOn] = strconv.FormatBool(d.StorageAutoscaling())
	}
	if d.StorageType != "" {
		s.Attrs[AttrStorageType] = d.StorageType
	}
	if d.IOPS > 0 {
		s.Attrs[AttrIOPS] = strconv.FormatInt(int64(d.IOPS), 10)
	}
	if d.StorageThroughputMBps > 0 {
		s.Attrs[AttrStorageThroughput] = strconv.FormatInt(int64(d.StorageThroughputMBps), 10)
	}
	if d.ReplicaSource != "" {
		s.Attrs[AttrReplicaOf] = d.ReplicaSource
	}
	if d.ClusterID != "" {
		s.Attrs[AttrClusterID] = d.ClusterID
	}
	return s
}

// MonthlyUSD projects an hourly cost onto a billing-average month.
func MonthlyUSD(hourly float64) float64 { return hourly * HoursPerMonth }

// GiB is one gibibyte in bytes, the unit RDS reports FreeStorageSpace and
// FreeableMemory in (bytes) and AllocatedStorage in (GiB).
const GiB float64 = 1 << 30

// fmtUSD renders money at a fixed width so golden output does not drift with
// float formatting.
func fmtUSD(v float64) string { return fmt.Sprintf("$%.4f", v) }

// fmtGiB renders a storage quantity.
func fmtGiB(v float64) string { return fmt.Sprintf("%.1fGiB", v) }

// fmtPct renders a fraction as a percentage.
func fmtPct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }
