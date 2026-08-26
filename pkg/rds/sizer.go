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

// The sizer, which never sizes anything.
//
// Every other domain's sizer answers "what shape should this be?". This one
// answers "what is this costing, and what would it take before anyone could
// honestly answer the first question?". The output is [Assessment]: one per DB
// instance, always carrying at least one [Suppression], because an instance
// this domain says nothing about is indistinguishable from an instance it
// failed to look at.
//
// The order of the gates is load-bearing and is the same discipline pkg/ec2
// established: exclusions fire ALONE. An Aurora cluster member does not also
// get told that its PostgreSQL page cache is misleading, because "PostgreSQL
// page cache" is a sentence in a vocabulary that does not describe it.

// Defaults for [Config]. Each is a policy choice rather than an AWS fact, and
// each is stated here so a report can echo the thresholds it was produced
// under.
const (
	// DefaultMinWindow is the shortest observation this domain will draw an
	// idle conclusion from. Three days is enough to contain a daily batch job
	// and is deliberately NOT enough to contain a weekly one — which is why a
	// window shorter than DefaultFullWindow still caveats the verdict.
	DefaultMinWindow = 72 * time.Hour
	// DefaultFullWindow is the window at which no window caveat is attached.
	// It is capped by CloudWatch: 1-minute datapoints live 15 days
	// ([RetentionAtOneMinute]), so 14 days is the longest honest ask.
	DefaultFullWindow = 14 * 24 * time.Hour
	// DefaultStorageOverprovisionThreshold is the unused fraction at which the
	// allocated-storage floor is worth a line in the report. It is a REPORTING
	// threshold: nothing is proposed on either side of it.
	DefaultStorageOverprovisionThreshold = 0.25
	// DefaultIdleCPUPercent is the CPU below which an instance with zero
	// connections is described as idle rather than as "busy with something we
	// cannot see". Non-zero CPU on a connection-less database is real work —
	// autovacuum, replication apply, backups — and saying "idle" over it would
	// be false.
	DefaultIdleCPUPercent = 5.0
)

// StorageParity is the seam U13 fills, and the single call site U11 reserves
// for it.
//
// U13 owns `pkg/rds/parity.go`: the engine-keyed gp2/gp3 regime tables, the
// 400 GiB / 200 GiB / never striping thresholds, measured-parity conversion,
// provisioned IOPS and throughput reduction toward the non-reducible baseline,
// and the refusal band (§2.4, trap 11). None of that exists yet, and this
// package must not guess at it: pkg/ebs's constants are wrong for RDS in all
// three of the ways trap 11 names, so reusing them would claim a saving in the
// band where RDS loses throughput and refuse in the band where it converts
// cleanly.
//
// Until U13 lands, [Config.Parity] is nil and every instance carries the
// storage-performance refusal this file emits instead. The interface is
// declared here rather than in parity.go so the call site in
// [Sizer.assessTarget] is written, reviewed and typed now, and U13 is a file
// addition rather than a surgery on the decision path.
//
// A parity implementation may return a [Proposal] — U13 is read-only, so that
// proposal is still [domain.ActionAdvisory] and [Report.Validate] still
// rejects anything else. Actuation is U14's problem and lives behind approval.
type StorageParity interface {
	// AssessParity evaluates one instance's storage configuration against its
	// measured I/O. It returns a proposal when the conversion is safe AND
	// cheaper, the suppressions explaining why not otherwise, and ok=false
	// when it declined to look at all.
	AssessParity(inst DBInstance, e Engine, series []Series, card RateCard) (*Proposal, []Suppression, bool)
}

// Config bounds one assessment run. It is echoed into [Report] so a stored
// report explains its own thresholds.
type Config struct {
	Scope  string `json:"scope,omitempty"`
	Region string `json:"region,omitempty"`

	// Rates prices instance-hours and storage. The zero value means
	// [DefaultRates].
	Rates RateCard `json:"-"`

	MinWindow  time.Duration `json:"minWindow"`
	FullWindow time.Duration `json:"fullWindow"`

	StorageOverprovisionThreshold float64 `json:"storageOverprovisionThreshold"`
	IdleCPUPercent                float64 `json:"idleCPUPercent"`

	// Parity is the U13 seam. Nil in U11 — see [StorageParity].
	Parity StorageParity `json:"-"`
}

// DefaultConfig returns the shipped policy.
func DefaultConfig() Config {
	return Config{
		Rates:                         DefaultRates(),
		MinWindow:                     DefaultMinWindow,
		FullWindow:                    DefaultFullWindow,
		StorageOverprovisionThreshold: DefaultStorageOverprovisionThreshold,
		IdleCPUPercent:                DefaultIdleCPUPercent,
	}
}

func (c Config) normalized() Config {
	if len(c.Rates.Classes) == 0 {
		c.Rates = DefaultRates()
	}
	if c.MinWindow <= 0 {
		c.MinWindow = DefaultMinWindow
	}
	if c.FullWindow <= 0 {
		c.FullWindow = DefaultFullWindow
	}
	if c.FullWindow < c.MinWindow {
		c.FullWindow = c.MinWindow
	}
	if c.StorageOverprovisionThreshold <= 0 || c.StorageOverprovisionThreshold > 1 {
		c.StorageOverprovisionThreshold = DefaultStorageOverprovisionThreshold
	}
	if c.IdleCPUPercent <= 0 {
		c.IdleCPUPercent = DefaultIdleCPUPercent
	}
	return c
}

// Proposal is a change this package would stand behind.
//
// Read the ABSENT fields as the specification. There is no DBInstanceClass,
// no MultiAZ, no EngineVersion and nothing that deletes — the §2.10 list of
// things that must be "not representable at all", following the pkg/ecs
// precedent where "never change desired count" is unrepresentable rather than
// merely forbidden. TestProposalCannotNameAnInstanceClass asserts it by
// reflection, so the structural guarantee survives a future edit that forgets
// why it was structural.
//
// AllocatedStorageGiB is present and is a RATCHET GUARD, not a lever:
// [Report.Validate] rejects any proposal whose allocated storage is below the
// observed value (trap 8), and FuzzRDSNeverProposesLessStorage re-asserts it
// over arbitrary inputs. U11 produces no proposals at all; the type exists so
// that when U13 does, the gate is already in front of it.
type Proposal struct {
	// AllocatedStorageGiB is what the instance would be left with. It may
	// equal or exceed the observed allocation and may never be below it.
	AllocatedStorageGiB int64 `json:"allocatedStorageGiB,omitempty"`
	// StorageType, IOPS and StorageThroughputMBps are the online, reversible,
	// documented-downtime-free knobs (§2.7). They are the ONLY knobs this
	// package will ever name.
	StorageType           string `json:"storageType,omitempty"`
	IOPS                  int32  `json:"iops,omitempty"`
	StorageThroughputMBps int32  `json:"storageThroughputMBps,omitempty"`

	Action     domain.ActionClass `json:"action"`
	Risk       string             `json:"risk"`
	Confidence float64            `json:"confidence"`
	Reason     string             `json:"reason"`

	ProposedHourlyUSD      float64 `json:"proposedHourlyUSD"`
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	NetSavingsMonthlyUSD   float64 `json:"netSavingsMonthlyUSD"`
	// RateProvenance is the weakest provenance behind the numbers above. A
	// proposal whose provenance is not [RateProvenance.Claimable] is a
	// [Report.Validate] violation: an unverified rate may size a fact and may
	// never become a claim.
	RateProvenance RateProvenance `json:"rateProvenance,omitempty"`
}

// IdleVerdict is the DatabaseConnections read.
//
// `DatabaseConnections` under-counts sessions — "engine-internal,
// parallel-execution and scheduler sessions are excluded" [verified:
// rds-metrics.html] — which is the SAFE direction for an idle test: an
// instance that looks busy might really be idle, but one that looks idle IS
// idle by this measure. The Maximum statistic is used so a single connected
// minute in a fortnight is enough to fail the test.
type IdleVerdict struct {
	// Known reports whether the question could be answered at all. A partial
	// or absent series answers false, and a false Known never produces an idle
	// advisory: silence is not evidence of quiet.
	Known bool `json:"known,omitempty"`
	Idle  bool `json:"idle,omitempty"`
	// MaxConnections is the peak concurrent connection count observed.
	MaxConnections float64 `json:"maxConnections,omitempty"`
	// PeakCPUPercent is the busiest CPU minute. Non-zero CPU on a
	// connection-less database is real work (autovacuum, replication apply,
	// backups), so it withholds the idle verdict.
	PeakCPUPercent float64 `json:"peakCPUPercent,omitempty"`
	CPUKnown       bool    `json:"cpuKnown,omitempty"`
	Samples        int     `json:"samples,omitempty"`
	// IsReplica routes the finding to [AdvisoryIdleReadReplica] instead of
	// [AdvisoryIdleInstance]: a replica with no connections is either unused
	// or a pure standby, which is a different conversation from an idle
	// primary.
	IsReplica bool `json:"isReplica,omitempty"`
}

// CommitmentVerdict is what the Reserved DB Instance ledger says about a
// hypothetical one-size-down move — a move this domain will never propose.
//
// It is computed anyway, and reported as an advisory, because trap 13's whole
// point is that the arithmetic is counter-intuitive: "the same downsize is
// partially absorbed on PostgreSQL and 100 % stranded on SQL Server". A
// human deciding whether to do the failover by hand needs that number, and
// producing it is the first thing U12 made possible.
type CommitmentVerdict struct {
	// Assessed is false when no ledger was supplied. Net then equals gross,
	// which under-claims and can never invent a saving.
	Assessed bool `json:"assessed,omitempty"`
	// SizeFlexible is the engine gate: "Size flexibility does not apply to
	// RDS for SQL Server and RDS for Oracle License Included" [verified].
	SizeFlexible bool `json:"sizeFlexible"`
	// CandidateClass is the next smaller priced class in the SAME instance
	// class type — AWS's own unit of size flexibility, where db.r6i.large and
	// db.r6id.large are different class types and share no discount.
	CandidateClass string `json:"candidateClass,omitempty"`

	GrossMonthlyUSD    float64 `json:"grossMonthlyUSD,omitempty"`
	NetMonthlyUSD      float64 `json:"netMonthlyUSD,omitempty"`
	StrandedMonthlyUSD float64 `json:"strandedMonthlyUSD,omitempty"`

	Code      string    `json:"code,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

// Assessment is one DB instance's complete verdict: what it costs, what was
// observed, and every reason this domain declined to change it.
type Assessment struct {
	Target   domain.TargetRef `json:"target"`
	Instance DBInstance       `json:"instance"`
	Engine   Engine           `json:"engineIdentity"`
	// Deployment is the topology as the price function and the reservation
	// arithmetic both see it.
	Deployment commit.RDSDeployment `json:"deployment,omitempty"`
	Current    domain.Spec          `json:"current"`
	Shape      ClassShape           `json:"shape,omitzero"`
	// PriorAllocatedStorageGiB is what the previous snapshot of this instance
	// reported, forwarded from [Target] so trap 8's ledger rule has something
	// to compare against. See [AttributeStorageGrowth].
	PriorAllocatedStorageGiB int64 `json:"priorAllocatedStorageGiB,omitempty"`

	// CostKnown reports whether the instance line could be priced at all.
	CostKnown         bool           `json:"costKnown,omitempty"`
	CurrentHourlyUSD  float64        `json:"currentHourlyUSD,omitempty"`
	CurrentMonthlyUSD float64        `json:"currentMonthlyUSD,omitempty"`
	RateProvenance    RateProvenance `json:"rateProvenance,omitempty"`

	Memory     MemoryVerdict     `json:"memory,omitzero"`
	Storage    StorageVerdict    `json:"storage,omitzero"`
	Idle       IdleVerdict       `json:"idle,omitzero"`
	Commitment CommitmentVerdict `json:"commitment,omitzero"`

	Evidence     []domain.Evidence `json:"evidence,omitempty"`
	Suppressions []Suppression     `json:"suppressions,omitempty"`
	Advisories   []Advisory        `json:"advisories,omitempty"`
	// Proposal is nil for every assessment this unit produces. See [Proposal].
	Proposal *Proposal `json:"proposal,omitempty"`
}

// exclusionCodes are the reasons that fire ALONE: this domain does not model
// the target at all, so nothing else it could say would be said in a
// vocabulary that applies.
var exclusionCodes = map[string]bool{
	ReasonModeOff:                   true,
	ReasonAuroraNotSupported:        true,
	ReasonClusterMemberNotSupported: true,
	ReasonUnknownEngine:             true,
	ReasonUnknownDeployment:         true,
}

// Excluded reports whether this instance is outside the domain entirely.
func (a Assessment) Excluded() bool {
	for _, s := range a.Suppressions {
		if exclusionCodes[s.Code] {
			return true
		}
	}
	return false
}

// Suppressed reports whether a specific reason code fired.
func (a Assessment) Suppressed(code string) bool {
	for _, s := range a.Suppressions {
		if s.Code == code {
			return true
		}
	}
	return false
}

// Codes returns this assessment's suppression codes in report order.
func (a Assessment) Codes() []string {
	out := make([]string, 0, len(a.Suppressions))
	for _, s := range a.Suppressions {
		out = append(out, s.Code)
	}
	return out
}

// WorstRateProvenance is the least trustworthy rate behind any dollar on this
// assessment — the instance line's, the storage line's, or both.
//
// It returns "" when no dollar was produced at all, which is a third state a
// caller must distinguish from "unverified": an instance nobody could price is
// not an instance priced badly.
func (a Assessment) WorstRateProvenance() RateProvenance {
	var out RateProvenance
	for _, p := range []RateProvenance{a.RateProvenance, a.Storage.RateProvenance} {
		if p == "" {
			continue
		}
		if out == "" {
			out = p
			continue
		}
		out = out.weakest(p)
	}
	return out
}

// Advised reports whether a specific advisory code fired.
func (a Assessment) Advised(code string) bool {
	for _, ad := range a.Advisories {
		if ad.Code == code {
			return true
		}
	}
	return false
}

// Sizer turns a snapshot into a report. It is pure: no clock, no I/O, no
// package-level state. Callers pass `now`.
type Sizer struct{ cfg Config }

// NewSizer validates a config and builds a sizer.
func NewSizer(cfg Config) (*Sizer, error) {
	c := cfg.normalized()
	if err := c.Rates.Validate(); err != nil {
		return nil, err
	}
	return &Sizer{cfg: c}, nil
}

// Config returns the normalized configuration this sizer runs under.
func (s *Sizer) Config() Config { return s.cfg }

// Assess produces the report. ledger may be nil, in which case net equals
// gross throughout — correct, because with no known commitment nothing can be
// stranded.
func (s *Sizer) Assess(now time.Time, snap *Snapshot, ledger domain.Netter) *Report {
	rep := &Report{
		Domain: Kind, GeneratedAt: now, Config: s.cfg,
	}
	if snap == nil {
		rep.Totals = rep.computeTotals()
		return rep
	}
	rep.Scope, rep.Region, rep.Window, rep.Stale = snap.Scope, snap.Region, snap.Window, snap.Stale
	rep.Warnings = sortWarnings(snap.Warnings)
	rep.Reservations = len(snap.Reservations)

	for _, t := range snap.Targets {
		rep.Assessments = append(rep.Assessments, s.assessTarget(now, snap, t, ledger))
	}
	sort.SliceStable(rep.Assessments, func(i, j int) bool {
		return rep.Assessments[i].Target.ID < rep.Assessments[j].Target.ID
	})
	rep.Totals = rep.computeTotals()
	return rep
}

func (s *Sizer) assessTarget(now time.Time, snap *Snapshot, t Target, ledger domain.Netter) Assessment {
	inst := t.Instance
	e := ParseEngine(inst.Engine, inst.LicenseModel)
	a := Assessment{
		Target:                   t.Ref,
		Instance:                 inst,
		Engine:                   e,
		Current:                  SpecFor(inst),
		PriorAllocatedStorageGiB: t.PriorAllocatedStorageGiB,
	}
	if shape, ok := ShapeOf(inst.Class); ok {
		a.Shape = shape
		a.Current.Resources.MilliCPU = int64(shape.VCPU) * 1000
		a.Current.Resources.MemoryBytes = shape.MemoryBytes
	}
	a.Evidence = append(a.Evidence, domain.Evidence{
		Metric: "observation-window", Value: snap.Window.String(),
		Window: snap.Window.String(), Source: SourceCloudWatch, At: snap.Window.End,
	}, domain.Evidence{
		Metric: "db-instance-class", Value: orNone(inst.Class),
		Source: SourceDescribe, At: snap.Window.End,
	})

	// --- Exclusions. Each fires alone. ------------------------------------

	if inst.ModeOff() {
		return a.exclude(ReasonModeOff, fmt.Sprintf(
			"%s carries the tag %s=off; this instance is excluded from every finding in this report",
			inst.DisplayName(), TagKilterMode))
	}
	if e.IsAurora() || auroraCluster(t) {
		return a.exclude(ReasonAuroraNotSupported, auroraReason(t, e))
	}
	if inst.ClusterID != "" {
		return a.exclude(ReasonClusterMemberNotSupported, fmt.Sprintf(
			"%s is a member of DB cluster %q. A Multi-AZ DB cluster is one writer and two readers, which "+
				"the reserved-instance table prices at three times the Single-AZ units of the same size — a "+
				"property of the cluster, not of any one member. This unit models per-instance economics "+
				"only, so cluster members are reported and excluded rather than described in a vocabulary "+
				"that does not fit them", inst.DisplayName(), inst.ClusterID))
	}
	if !e.Known() {
		return a.exclude(ReasonUnknownEngine, fmt.Sprintf(
			"engine %q is not one this package has encoded. Engine decides what FreeableMemory means, which "+
				"storage striping threshold applies, and whether a reservation is size-flexible; an engine "+
				"with none of that encoded is refused by name rather than treated as though it were MySQL",
			e.String()))
	}
	dep, depOK := inst.Deployment()
	if !depOK {
		return a.exclude(ReasonUnknownDeployment, fmt.Sprintf(
			"could not determine the deployment topology of %s. Topology is a ×1/×2/×3 multiplier on the "+
				"instance line and on reservation coverage, so guessing Single-AZ would halve both this "+
				"instance's cost and any saving derived from it", inst.DisplayName()))
	}
	a.Deployment = dep

	// --- Price the instance line. Trap 10 lives in the multiplier. --------

	s.price(&a)

	// --- Evidence gates ---------------------------------------------------

	mem, _ := t.SeriesFor(MetricFreeableMemory)
	free, _ := t.SeriesFor(MetricFreeStorageSpace)
	conns, _ := t.SeriesFor(MetricDatabaseConns)
	cpu, _ := t.SeriesFor(MetricCPUUtilization)

	anyUsable := mem.Usable() || free.Usable() || conns.Usable() || cpu.Usable()
	anyPartial := mem.Partial || free.Partial || conns.Partial || cpu.Partial
	switch {
	case !anyUsable && anyPartial:
		a.suppress(ReasonTruncatedMetrics, "CloudWatch returned no complete series for this instance. A "+
			"missing result is truncation, not an empty metric, so nothing here is read as quiet")
	case !anyUsable:
		a.suppress(ReasonNoMetricEvidence, fmt.Sprintf(
			"no CloudWatch datapoints were delivered for %s, so this instance is reported without evidence "+
				"rather than reported as having no findings", inst.DisplayName()))
	}
	if inst.StateUnstable() {
		a.suppress(ReasonInstanceStateUnstable, fmt.Sprintf(
			"%s is in state %q, so its metrics describe a transient shape. \"You can't modify allocated "+
				"storage if the DB instance status is storage-optimization\", and a reading taken during a "+
				"modification is not a reading of the steady state", inst.DisplayName(), inst.Status))
	}
	if d := snap.Window.Duration(); d > 0 && d < s.cfg.MinWindow {
		a.suppress(ReasonInsufficientWindow, fmt.Sprintf(
			"the observation window is %s, below the %s minimum. A database's peak is a weekly and monthly "+
				"shape; a window that cannot contain one cannot rule one out",
			snap.Window.String(), s.cfg.MinWindow))
	}

	// --- Trap 9. The verdict differs by engine over an identical series. ---

	a.Memory = AssessMemory(e, mem, float64(a.Shape.MemoryBytes))
	a.suppress(a.Memory.Code, a.Memory.Reason)
	if a.Memory.Samples > 0 {
		v := "not readable as headroom"
		if a.Memory.Readable {
			v = fmtGiB(a.Memory.MinFreeBytes / GiB)
		}
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: MetricFreeableMemory + "-min", Value: v, Window: snap.Window.String(),
			Samples: a.Memory.Samples, Source: SourceCloudWatch, At: snap.Window.End,
		})
	}

	// --- Trap 8. Allocated storage is a floor with a dollar on it. --------

	a.Storage = AssessStorage(inst, free, s.cfg.Rates)
	s.storageFindings(&a, snap)
	s.rateProvenanceFindings(&a)

	// --- Idle, and the replica distinction (§2.5) -------------------------

	a.Idle = assessIdle(conns, cpu, inst, s.cfg.IdleCPUPercent)
	s.idleFindings(&a, snap)

	// --- The permanent refusals (§2.10) -----------------------------------

	a.suppress(ReasonInstanceClassIsAFailover, fmt.Sprintf(
		"changing the class of %s is a failover: \"The RDS instance was modified by customer — An RDS DB "+
			"instance modification triggered a failover\", \"Failover times are typically 60–120 seconds\", "+
			"and \"The failover mechanism automatically changes the Domain Name System (DNS) record … you "+
			"need to re-establish any existing connections\". No action class in this engine describes that "+
			"honestly — stop-start would budget it like an EC2 restart, which returns the same endpoint — so "+
			"the class is not a field this domain can even express", inst.DisplayName()))

	if inst.IsReplica() {
		a.suppress(ReasonReplicaIsFailoverCapacity, fmt.Sprintf(
			"%s is a read replica of %q. A replica's size is usually chosen so it can be PROMOTED, not to "+
				"fit its own workload, so sizing it on utilization proposes an instance that cannot absorb "+
				"a failover — a correctness claim about an availability property no API exposes",
			inst.DisplayName(), inst.ReplicaSource))
	}
	if dep == commit.RDSMultiAZInstance {
		a.suppress(ReasonMultiAZIsAvailabilityPosture, fmt.Sprintf(
			"%s is Multi-AZ. Turning that off is mechanically cheap — \"Downtime doesn't occur during this "+
				"change\" — and it halves a bill by halving an SLA, so it is an availability posture and not "+
				"a cost setting. This domain reports the multiplier and never represents the toggle",
			inst.DisplayName()))
		a.advise(Advisory{
			Code: AdvisoryMultiAZMultiplier,
			Message: fmt.Sprintf("Multi-AZ doubles the instance line: %s runs a synchronous standby that "+
				"cannot serve read traffic, and the reserved-instance table prices the deployment at "+
				"exactly twice the Single-AZ normalized units of the same size",
				inst.DisplayName()),
			Caveat: "the multiplier applies to instance hours only — storage, backups and I/O are billed on " +
				"their own lines, the same separation AWS states for the reservation discount. Removing " +
				"Multi-AZ is not represented by this domain at any price",
			MonthlyUSD:     a.CurrentMonthlyUSD / 2,
			RateProvenance: a.RateProvenance,
		})
	}

	// --- The commitment arithmetic U12 made possible (trap 13) ------------

	a.Commitment = s.assessCommitment(now, snap, &a, ledger)
	if a.Commitment.Code != "" {
		a.suppress(a.Commitment.Code, a.Commitment.Reason)
		if a.Commitment.ValidFrom.After(time.Time{}) {
			a.Suppressions[len(a.Suppressions)-1].ValidFrom = a.Commitment.ValidFrom
		}
	}

	// --- The U13 seam. Nil in this unit; see [StorageParity]. -------------

	if s.cfg.Parity != nil {
		if p, sup, ok := s.cfg.Parity.AssessParity(inst, e, t.Series, s.cfg.Rates); ok {
			for _, x := range sup {
				a.suppress(x.Code, x.Reason)
			}
			a.Proposal = p
		}
	} else {
		a.suppress(ReasonNoStoragePerformanceModel, fmt.Sprintf(
			"storage-performance parity for %s is not evaluated in this unit. RDS stripes across four "+
				"volumes above an engine-dependent threshold and its gp2 burst reaches 12,000 IOPS and "+
				"1,000 MiB/s rather than EBS's 3,000 and 250 — reusing pkg/ebs's constants would claim a "+
				"saving in the band where RDS loses throughput and refuse in the band where it converts "+
				"cleanly, so no storage-performance verdict is offered rather than a borrowed one",
			orNone(inst.StorageType)))
	}

	// Every assessment reaching here carries at least the failover refusal, so
	// "silence is not an output" holds by construction rather than by luck.
	return a
}

// price fills the instance-line cost, and refuses to let an unverified rate
// become anything more than a magnitude.
func (s *Sizer) price(a *Assessment) {
	if commit.RDSClassType(a.Instance.Class) == "" {
		a.suppress(ReasonUnknownInstanceClass, fmt.Sprintf(
			"%q is not a DB instance class of the form db.<class>.<size>, so it has no normalized units, no "+
				"reservation matching and no rate. No price means no bill delta means nothing to claim",
			a.Instance.Class))
		return
	}
	hourly, prov, ok := s.cfg.Rates.HourlyUSD(a.Instance.Class, a.Engine, a.Deployment)
	if !ok {
		if !s.cfg.Rates.PricesBand(a.Engine) {
			a.suppress(ReasonEngineNotPriced, fmt.Sprintf(
				"this package ships no rate for engine %q (price band %q). The same hardware costs different "+
					"amounts under different engines, editions and licence models, so quoting an "+
					"open-source rate for it would be a wrong number rather than a missing one. Supply the "+
					"rows with rds.LoadRates to price it", a.Engine.String(), PriceBand(a.Engine)))
			return
		}
		a.suppress(ReasonUnknownInstanceClass, fmt.Sprintf(
			"no rate is shipped for class %q under engine %q. A class absent from the table is refused "+
				"rather than interpolated from its neighbours", a.Instance.Class, a.Engine.String()))
		return
	}
	a.CostKnown = true
	a.CurrentHourlyUSD = hourly
	a.CurrentMonthlyUSD = MonthlyUSD(hourly)
	a.RateProvenance = prov
	a.Evidence = append(a.Evidence, domain.Evidence{
		Metric: "instance-hour-rate", Value: fmtUSD(hourly) + "/h (" + string(prov) + ")",
		Source: SourceRateCard,
	}, domain.Evidence{
		Metric: "deployment", Value: string(a.Deployment) + " ×" + deploymentFactorString(a.Deployment),
		Source: SourceDescribe,
	})
}

// rateProvenanceFindings emits the unverified-rate refusal.
//
// It runs AFTER the storage verdict, and keys off [Assessment.WorstRateProvenance]
// rather than off the instance rate alone. An operator who supplies verified
// db.* class rates but leaves the shipped gp2/gp3 $/GiB-month figures in place
// still has an unverified dollar in their report — §7 marks the storage rates
// unverified separately from the instance rates, and a target is only as
// trustworthy as the weakest rate that priced any part of it.
func (s *Sizer) rateProvenanceFindings(a *Assessment) {
	prov := a.WorstRateProvenance()
	if prov == "" || prov.Claimable() {
		return
	}
	a.suppress(ReasonUnverifiedRate, fmt.Sprintf(
		"at least one rate behind the dollars on %s is %s. docs/design/rds-batch-assessment.md §7 records "+
			"that RDS on-demand instance rates, the Multi-AZ price ratio and the gp2/gp3 $/GiB-month "+
			"figures could not be retrieved from AWS, and the $0.115 gp2 figure appears only in an example "+
			"AWS itself labels \"sample prices\". These numbers size a fact and may not become a saving",
		a.Instance.DisplayName(), prov))
	a.advise(Advisory{
		Code: AdvisoryUnverifiedRate,
		Message: fmt.Sprintf("%s is on the order of %s/mo of instance spend plus %s/mo of storage, at %s "+
			"rates", a.Instance.DisplayName(), fmtUSD(a.CurrentMonthlyUSD),
			fmtUSD(a.Storage.AllocatedMonthlyUSD), prov),
		Caveat: "an order of magnitude, not an invoice: supply verified rates with rds.LoadRates before " +
			"putting this number in front of a finance team",
		MonthlyUSD:     a.CurrentMonthlyUSD + a.Storage.AllocatedMonthlyUSD,
		RateProvenance: prov,
	})
}

func deploymentFactorString(d commit.RDSDeployment) string {
	m, ok := d.Multiplier()
	if !ok {
		return "?"
	}
	return strconv.FormatFloat(m, 'g', -1, 64)
}

// storageFindings turns the storage verdict into the trap-8 output: an
// advisory carrying the cost, and a refusal carrying the reason.
func (s *Sizer) storageFindings(a *Assessment, snap *Snapshot) {
	v := a.Storage
	if v.AllocatedGiB <= 0 {
		return
	}
	a.Evidence = append(a.Evidence, domain.Evidence{
		Metric: "allocated-storage", Value: fmt.Sprintf("%d GiB", v.AllocatedGiB),
		Source: SourceDescribe, At: snap.Window.End,
	})
	if v.FillKnown {
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: MetricFreeStorageSpace + "-min", Value: fmtGiB(v.MinFreeGiB),
			Window: snap.Window.String(), Samples: v.Samples, Source: SourceCloudWatch, At: snap.Window.End,
		})
	}

	// The refusal always fires when there is an allocation, whether or not the
	// instance is over-provisioned: the ratchet is a property of RDS, not of
	// this instance's fill level.
	a.suppress(ReasonStorageCannotShrink, v.Reason())

	if v.Overprovisioned(s.cfg.StorageOverprovisionThreshold) {
		a.advise(Advisory{
			Code: AdvisoryStorageFloor,
			Message: fmt.Sprintf("%s of %s allocated on %s was never used across the window (%s)",
				fmtGiB(v.UnusedGiB), fmt.Sprintf("%d GiB", v.AllocatedGiB), a.Instance.DisplayName(),
				fmtPct(v.UnusedFraction)),
			Caveat: "this is a permanent cost, not a saving: no RDS API reduces allocated storage, and the " +
				"documented alternatives are a blue/green deployment or a migration to a new instance. " +
				"FreeStorageSpace is reported here as the size of that cost and is never read as headroom",
			MonthlyUSD:     v.UnusedMonthlyUSD,
			RateProvenance: v.RateProvenance,
		})
	}

	if v.AutoscalingEnabled {
		a.suppress(ReasonStorageAutoscalingRatchet, fmt.Sprintf(
			"%s has storage autoscaling enabled (allocated %d GiB, ceiling %d GiB). %s. The floor can "+
				"therefore rise between two runs of this report with no event anywhere to attribute it to, "+
				"so an increase is recorded as unattributed rather than as a change or a regression",
			a.Instance.DisplayName(), v.AllocatedGiB, v.MaxAllocatedGiB, AutoscalingTrigger()))
		a.advise(Advisory{
			Code: AdvisoryStorageAutoscaling,
			Message: fmt.Sprintf("%s can autoscale storage from %d GiB up to %d GiB, and the increase is "+
				"permanent once it happens", a.Instance.DisplayName(), v.AllocatedGiB, v.MaxAllocatedGiB),
			Caveat:         AutoscalingTrigger(),
			MonthlyUSD:     v.AllocatedMonthlyUSD,
			RateProvenance: v.RateProvenance,
		})
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: "storage-autoscaling-trigger", Value: AutoscalingTrigger(),
			Source: SourceAWSDocumented,
		})
	}

	// The ledger rule: a growth between snapshots is unattributed.
	if attr, why := AttributeStorageGrowth(a.PriorAllocatedStorageGiB, v.AllocatedGiB); attr != StorageUnchanged {
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: "allocated-storage-change", Value: string(attr) + ": " + why,
			Source: SourceAWSDocumented, At: snap.Window.End,
		})
	}
}

func (s *Sizer) idleFindings(a *Assessment, snap *Snapshot) {
	v := a.Idle
	if v.Samples > 0 {
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: MetricDatabaseConns + "-max", Value: fmt.Sprintf("%.0f", v.MaxConnections),
			Window: snap.Window.String(), Samples: v.Samples, Source: SourceCloudWatch, At: snap.Window.End,
		})
	}
	if !v.Known || !v.Idle {
		return
	}
	code, what := AdvisoryIdleInstance, "instance"
	caveat := "an idle RDS instance is not a stoppable one: it is data-bearing, and this engine never " +
		"proposes shutting one down. The finding is the dollar figure and the question it raises for a " +
		"human — DatabaseConnections under-counts sessions, so zero here means zero"
	if v.IsReplica {
		code, what = AdvisoryIdleReadReplica, "read replica"
		caveat = "a replica with no connections is either unused or a pure standby. Either way its size is " +
			"a failover-capacity decision and not a utilization one, so this is reported as a question " +
			"about whether the replica should exist — never as a resize"
	}
	a.advise(Advisory{
		Code: code,
		Message: fmt.Sprintf("%s %s carried zero database connections across the whole %s window, at a peak "+
			"of %.1f%% CPU", what, a.Instance.DisplayName(), snap.Window.String(), v.PeakCPUPercent),
		Caveat:         caveat,
		MonthlyUSD:     a.CurrentMonthlyUSD,
		RateProvenance: a.RateProvenance,
	})
}

// assessIdle reads DatabaseConnections and CPUUtilization together. Both must
// be known and quiet: a connection-less database burning CPU is doing work
// this domain cannot see, and calling it idle would be false.
func assessIdle(conns, cpu Series, inst DBInstance, idleCPU float64) IdleVerdict {
	v := IdleVerdict{Samples: conns.Len(), IsReplica: inst.IsReplica()}
	allZero, known := conns.AllZero()
	if !known {
		return v
	}
	v.Known = true
	if hi, ok := conns.Max(); ok {
		v.MaxConnections = hi
	}
	if hi, ok := cpu.Max(); ok && cpu.Usable() {
		v.CPUKnown, v.PeakCPUPercent = true, hi
	}
	v.Idle = allZero && (!v.CPUKnown || v.PeakCPUPercent <= idleCPU)
	return v
}

// assessCommitment runs the hypothetical one-size-down move through the U12
// ledger and reports what would happen to the reservation. Nothing is
// proposed; the number is the product.
func (s *Sizer) assessCommitment(now time.Time, snap *Snapshot, a *Assessment, ledger domain.Netter) CommitmentVerdict {
	v := CommitmentVerdict{SizeFlexible: a.Engine.SizeFlexible()}
	if !a.CostKnown {
		return v
	}
	smaller, smallerHourly, ok := s.smallerClass(a)
	if !ok {
		return v
	}
	v.CandidateClass = smaller
	v.GrossMonthlyUSD = MonthlyUSD(a.CurrentHourlyUSD - smallerHourly)

	if ledger == nil {
		v.NetMonthlyUSD = v.GrossMonthlyUSD
		return v
	}
	v.Assessed = true
	before := UsageLines(a.Target, a.Instance, a.Engine, a.Deployment, a.CurrentHourlyUSD)
	after := UsageLines(a.Target, withClass(a.Instance, smaller), a.Engine, a.Deployment, smallerHourly)
	as := ledger.Net(before, after)
	v.NetMonthlyUSD = as.NetMonthlyUSD
	v.StrandedMonthlyUSD = as.StrandedHourlyUSD * HoursPerMonth
	v.ValidFrom = as.ValidFrom

	switch {
	case as.Suppressed && as.ReasonCode != "":
		v.Code, v.Reason = as.ReasonCode, as.Reason
	case !v.SizeFlexible && v.StrandedMonthlyUSD > 0:
		v.Code = ReasonSizeFlexibilityExcluded
	}
	if !v.SizeFlexible && hasReservationFor(snap, a) {
		v.Code = ReasonSizeFlexibilityExcluded
		v.Reason = fmt.Sprintf(
			"a Reserved DB Instance on engine %q is not size-flexible — \"Size flexibility does not apply "+
				"to RDS for SQL Server and RDS for Oracle License Included\" — so a move from %s to %s "+
				"strands the reservation entirely rather than floating onto the smaller size. On PostgreSQL "+
				"or MySQL the same move would be partly absorbed; here it is 100 %% stranded",
			a.Engine.String(), a.Instance.Class, smaller)
	}
	if v.Code != "" && v.Reason == "" {
		v.Reason = fmt.Sprintf("moving %s to %s nets %s/mo against a %s/mo list-price delta once the "+
			"Reserved DB Instance inventory is applied", a.Instance.DisplayName(), smaller,
			fmtUSD(v.NetMonthlyUSD), fmtUSD(v.GrossMonthlyUSD))
	}
	if v.StrandedMonthlyUSD > 0 {
		a.advise(Advisory{
			Code: AdvisoryReservationStranding,
			Message: fmt.Sprintf("a hypothetical move from %s to %s would strand %s/mo of Reserved DB "+
				"Instance commitment (list price says %s/mo, the bill says %s/mo)",
				a.Instance.Class, smaller, fmtUSD(v.StrandedMonthlyUSD), fmtUSD(v.GrossMonthlyUSD),
				fmtUSD(v.NetMonthlyUSD)),
			Caveat: "this domain does not propose the move — the class change is a failover — and \"You " +
				"can't cancel a reserved DB instance\", so the stranding is permanent until the term ends. " +
				"The number is here so a human doing it by hand knows what it costs",
			MonthlyUSD:     v.StrandedMonthlyUSD,
			RateProvenance: a.RateProvenance,
		})
	}
	_ = now
	return v
}

// smallerClass finds the next smaller PRICED class in the same instance class
// type — AWS's own unit of size flexibility, where db.r6i.large and
// db.r6id.large are different class types and share no discount at all.
func (s *Sizer) smallerClass(a *Assessment) (string, float64, bool) {
	classType := commit.RDSClassType(a.Instance.Class)
	if classType == "" {
		return "", 0, false
	}
	cur, ok := commit.RDSClassUnits(a.Instance.Class, commit.RDSSingleAZ)
	if !ok {
		return "", 0, false
	}
	var (
		best      string
		bestUnits float64
		bestRate  float64
	)
	keys := make([]string, 0, len(s.cfg.Rates.Classes))
	for k := range s.cfg.Rates.Classes {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic tie-breaking
	for _, k := range keys {
		_, class, found := strings.Cut(k, "|")
		if !found || commit.RDSClassType(class) != classType {
			continue
		}
		u, ok := commit.RDSClassUnits(class, commit.RDSSingleAZ)
		if !ok || u >= cur || u <= bestUnits {
			continue
		}
		hourly, _, ok := s.cfg.Rates.HourlyUSD(class, a.Engine, a.Deployment)
		if !ok {
			continue
		}
		best, bestUnits, bestRate = class, u, hourly
	}
	if best == "" {
		return "", 0, false
	}
	return best, bestRate, true
}

func withClass(d DBInstance, class string) DBInstance { d.Class = class; return d }

func hasReservationFor(snap *Snapshot, a *Assessment) bool {
	want := commit.RDSClassType(a.Instance.Class)
	engine := a.Engine.CommitEngine()
	for _, r := range snap.Reservations {
		if r.ClassType() == want && commit.NormalizeRDSEngine(r.Engine) == engine {
			return true
		}
	}
	return false
}

// UsageLines renders one DB instance-hour as commitment usage lines.
//
// Exactly ONE line, and it is instance-hours. That is not a simplification:
// "The price for a reserved DB instance doesn't provide a discount for the
// costs associated with storage, backups, and I/O. It provides a discount only
// on the hourly, on-demand instance usage" [verified], and
// [commit.Usage.Validate] enforces it by requiring a DB instance class on
// every KindRDS line — so a storage line is structurally unable to become a
// covered line. Storage cost is reported on [StorageVerdict] and never enters
// the waterfall.
//
// Quantity is 1 instance-hour and ODRate is the DEPLOYMENT-ADJUSTED hourly
// rate, so a Multi-AZ instance's line costs twice a Single-AZ one's exactly as
// it consumes twice the normalized units. The two halves of trap 10 stay in
// step because they come from the same multiplier.
//
// Exported so a test — and cmd/'s account-wide baseline — can go end to end
// through the U12 waterfall without reconstructing the line shape.
func UsageLines(ref domain.TargetRef, d DBInstance, e Engine,
	dep commit.RDSDeployment, hourlyUSD float64) []commit.UsageLine {

	id := ref.ID
	if id == "" {
		id = d.ARN
	}
	if id == "" {
		id = d.Identifier
	}
	return []commit.UsageLine{{
		ID:           id + "/instance",
		Kind:         commit.KindRDS,
		Region:       d.Region,
		InstanceType: d.Class,
		Engine:       e.CommitEngine(),
		Deployment:   dep,
		Unit:         "Instance-Hours",
		Quantity:     1,
		ODRate:       zeroIfNotFinite(hourlyUSD),
	}}
}

// --- Assessment helpers ----------------------------------------------------

// exclude records an exclusion and returns the assessment. Exclusions fire
// alone: whatever was already collected stays as evidence, and NO other
// suppression or advisory is added.
func (a Assessment) exclude(code, reason string) Assessment {
	a.Suppressions = []Suppression{{Code: code, Reason: reason}}
	a.Advisories = nil
	a.Proposal = nil
	return a
}

func (a *Assessment) suppress(code, reason string) {
	if code == "" || reason == "" {
		return
	}
	for _, s := range a.Suppressions {
		if s.Code == code {
			return
		}
	}
	a.Suppressions = append(a.Suppressions, Suppression{Code: code, Reason: reason})
}

func (a *Assessment) advise(ad Advisory) {
	if ad.Code == "" || ad.Caveat == "" {
		return
	}
	for _, x := range a.Advisories {
		if x.Code == ad.Code {
			return
		}
	}
	ad.MonthlyUSD = zeroIfNotFinite(ad.MonthlyUSD)
	a.Advisories = append(a.Advisories, ad)
}

// auroraCluster reports whether the cluster this instance belongs to is an
// Aurora cluster, using the CLUSTER's engine rather than the member's.
func auroraCluster(t Target) bool {
	return t.Cluster.Known && ParseEngine(t.Cluster.Engine, "").IsAurora()
}

func auroraReason(t Target, e Engine) string {
	base := fmt.Sprintf(
		"%s is Aurora (%s). Aurora shares DescribeDBInstances and most of the CloudWatch namespace with "+
			"RDS and is a different billing model wearing its name: an Aurora Serverless v2 instance has no "+
			"class to resize — capacity is ACUs in 0.5 steps, measured per second, able to scale to zero "+
			"with automatic pause and resume — its storage is cluster-managed rather than an "+
			"AllocatedStorage the operator sets, and its highest-value lever is the I/O-Optimized cluster "+
			"mode, which is nothing like instance rightsizing. Applying provisioned-instance reasoning "+
			"here would produce recommendations about a field that does not control the bill",
		t.Instance.DisplayName(), e.String())
	if t.Cluster.ServerlessV2MinACU > 0 {
		base += fmt.Sprintf(". Its cluster's min-ACU floor is %.1f ACU, which is a configuration choice "+
			"rather than a demand signal and is where an Aurora unit would start", t.Cluster.ServerlessV2MinACU)
	}
	return base
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unset)"
	}
	return s
}
