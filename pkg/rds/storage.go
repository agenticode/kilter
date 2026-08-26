package rds

import (
	"fmt"
	"time"
)

// Allocated storage — trap 8, and the clearest "quantify it, refuse to act on
// it" finding in the whole RDS surface.
//
// Three verified sentences settle what is possible [all verified:
// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html]:
//
//	"Autoscaling can't decrease the allocated storage. You can't reduce the
//	amount of storage for a DB instance after storage has been allocated."
//
//	"After a DB instance has been autoscaled, its allocated storage can't be
//	reduced."
//
//	"For storage, you can't manually reduce the allocated storage of a DB
//	instance using the modify-db-instance command."
//
// The documented alternatives are a blue/green deployment or a manual
// migration to a new instance — both outside anything Kilter does or should
// do. So RDS allocated storage is a MONOTONE RATCHET, and Kilter's existing
// rule ("never recommend volume shrink"; pkg/ebs: "ModifyVolume can only grow;
// growing is not rightsizing") transfers unchanged.
//
// Two things follow, and this file encodes both.
//
// # 1. AllocatedStorage is an upper bound that is also a floor
//
// A 4 TiB instance holding 300 GiB of data is paying for 4 TiB forever. That
// is a real, large, REPORTABLE number and it is NOT a recommendation.
// `FreeStorageSpace` — "The amount of available storage space" [verified:
// rds-metrics.html] — is therefore read as EVIDENCE OF THE SIZE OF A
// PERMANENT COST and never as a saving. [StorageVerdict] has no field named
// after a saving for exactly that reason.
//
// # 2. The floor moves on its own, and nothing records it
//
// Storage autoscaling fires on a documented trigger and adds a documented
// increment — see [AutoscalingTrigger] — and "Autoscaling operations aren't
// logged by AWS CloudTrail" [verified, same page]. So a ledger entry that says
// "we changed nothing and storage grew" has no corroborating event anywhere.
// An allocated-storage increase between two snapshots is therefore
// UNATTRIBUTED by default: never Kilter's doing, and never a regression. See
// [AttributeStorageGrowth].

// The documented storage-autoscaling trigger, verbatim in structure
// [verified: USER_PIOPS.Autoscaling.html]. These constants exist so the
// advisory can QUOTE the trigger as evidence rather than paraphrase it, which
// is what lets a reader tell an autoscaling event from a Kilter change.
const (
	// AutoscaleFreeFraction: "Free available space is less than or equal to
	// 10 percent of the allocated storage."
	AutoscaleFreeFraction = 0.10
	// AutoscaleLowStorageDuration: "The low-storage condition lasts at least
	// five minutes."
	AutoscaleLowStorageDuration = 5 * time.Minute
	// AutoscaleMaxModificationsPer24h: "…and fewer than four storage
	// modifications have occurred in the past 24 hours."
	AutoscaleMaxModificationsPer24h = 4
	// AutoscaleMinIncrementGiB and AutoscaleIncrementFraction: the increment
	// is the GREATER of 10 GiB, 10 % of current allocation, or predicted
	// growth over the next 7 hours.
	AutoscaleMinIncrementGiB   int64 = 10
	AutoscaleIncrementFraction       = 0.10
	AutoscalePredictionHorizon       = 7 * time.Hour
)

// AutoscalingTrigger returns the documented trigger as one evidence string.
// It is a function rather than a constant so the numbers above are the single
// source of the prose: a reader who changes the threshold changes the quote.
func AutoscalingTrigger() string {
	return fmt.Sprintf(
		"RDS storage autoscaling raises allocated storage on its own when free space is ≤ %s of the "+
			"allocation, the low-storage condition lasts at least %s, storage optimization from the "+
			"previous modification has completed, and fewer than %d storage modifications have occurred "+
			"in the past 24 hours; it then adds the greater of %d GiB, %s of the current allocation, or "+
			"the growth predicted for the next %s. Autoscaling operations are not logged by CloudTrail",
		fmtPct(AutoscaleFreeFraction), AutoscaleLowStorageDuration, AutoscaleMaxModificationsPer24h,
		AutoscaleMinIncrementGiB, fmtPct(AutoscaleIncrementFraction), AutoscalePredictionHorizon)
}

// StorageVerdict is what this package will say about one instance's allocated
// storage.
//
// Read the field names as the specification. There is `UnusedGiB` and
// `UnusedMonthlyUSD`, and there is no `SavingGiB` and no
// `PotentialSavingMonthlyUSD`, because the API that would realize one does not
// exist. TestStorageVerdictNamesNoSaving asserts that by reflection, so the
// next person to add a field has to argue with a test rather than with a
// comment.
type StorageVerdict struct {
	AllocatedGiB    int64 `json:"allocatedGiB"`
	MaxAllocatedGiB int64 `json:"maxAllocatedGiB,omitempty"`
	// AutoscalingEnabled is MaxAllocatedGiB > AllocatedGiB: the ratchet can
	// move without anyone asking it to.
	AutoscalingEnabled bool `json:"autoscalingEnabled,omitempty"`

	// FillKnown reports whether FreeStorageSpace was delivered in full. When
	// false, every field below is zero: an unobserved fill level produces no
	// number at all, because a report that says "0 GiB used" about an
	// instance CloudWatch declined to answer for is worse than one that says
	// nothing.
	FillKnown bool `json:"fillKnown,omitempty"`
	// MinFreeGiB is the LOW-WATER mark of FreeStorageSpace across the window —
	// the fullest the volume ever got. Using the mean would understate the
	// floor a database actually needs.
	MinFreeGiB float64 `json:"minFreeGiB,omitempty"`
	// UsedGiB is AllocatedGiB − MinFreeGiB: the high-water mark of consumption.
	UsedGiB float64 `json:"usedGiB,omitempty"`
	// UnusedGiB is MinFreeGiB, named for what it is: allocated storage that
	// was never used and can never be returned.
	UnusedGiB float64 `json:"unusedGiB,omitempty"`
	// UnusedFraction is UnusedGiB ÷ AllocatedGiB.
	UnusedFraction float64 `json:"unusedFraction,omitempty"`

	// UnusedMonthlyUSD is the monthly cost of the unused allocation. It is a
	// COST, not a saving: the remediation is a blue/green deployment or a
	// migration to a new instance, neither of which Kilter does.
	UnusedMonthlyUSD float64 `json:"unusedMonthlyUSD,omitempty"`
	// AllocatedMonthlyUSD is the monthly cost of the whole allocation.
	AllocatedMonthlyUSD float64        `json:"allocatedMonthlyUSD,omitempty"`
	RateProvenance      RateProvenance `json:"rateProvenance,omitempty"`

	// Samples is how many FreeStorageSpace datapoints backed the verdict.
	Samples int `json:"samples,omitempty"`
}

// AssessStorage measures the allocated-storage floor. It never proposes
// anything: the return value is a description, and the only code that reads it
// turns it into an advisory and a refusal.
//
// card may be a zero RateCard, in which case the dollar fields stay zero and
// the finding is reported without a magnitude — which is still the right
// finding, just a weaker one.
func AssessStorage(d DBInstance, free Series, card RateCard) StorageVerdict {
	v := StorageVerdict{
		AllocatedGiB:       d.AllocatedStorageGiB,
		MaxAllocatedGiB:    d.MaxAllocatedStorageGiB,
		AutoscalingEnabled: d.StorageAutoscaling(),
		Samples:            free.Len(),
	}
	if v.AllocatedGiB <= 0 {
		return v
	}
	if total, prov, ok := card.StorageMonthlyUSD(d.StorageType, v.AllocatedGiB); ok {
		v.AllocatedMonthlyUSD, v.RateProvenance = total, prov
	}

	// A partial series is not a fill level. CloudWatch declining to answer and
	// a volume being empty are different facts and must not produce the same
	// number.
	if !free.Usable() {
		return v
	}
	lo, ok := free.Min()
	if !ok || !finite(lo) || lo < 0 {
		return v
	}
	v.FillKnown = true
	v.MinFreeGiB = lo / GiB
	if v.MinFreeGiB > float64(v.AllocatedGiB) {
		// More free space than allocated storage: the two came from different
		// places (the metric is bytes on the filesystem, the allocation is
		// what AWS bills) and reconciling them is not this package's job.
		// Clamp rather than report a negative usage.
		v.MinFreeGiB = float64(v.AllocatedGiB)
	}
	v.UnusedGiB = v.MinFreeGiB
	v.UsedGiB = float64(v.AllocatedGiB) - v.UnusedGiB
	v.UnusedFraction = v.UnusedGiB / float64(v.AllocatedGiB)
	if v.AllocatedMonthlyUSD > 0 {
		v.UnusedMonthlyUSD = v.AllocatedMonthlyUSD * v.UnusedFraction
	}
	return v
}

// Overprovisioned reports whether the unused fraction is large enough to be
// worth stating. The threshold is a REPORTING threshold, not a safety one:
// nothing is proposed either way, and the only consequence of crossing it is
// that a human sees a line about it.
func (v StorageVerdict) Overprovisioned(threshold float64) bool {
	return v.FillKnown && v.AllocatedGiB > 0 && v.UnusedFraction >= threshold
}

// Reason renders the trap-8 refusal prose for this verdict, quoting AWS.
func (v StorageVerdict) Reason() string {
	base := "allocated storage is a one-way ratchet: \"You can't reduce the amount of storage for a DB " +
		"instance after storage has been allocated\" and \"you can't manually reduce the allocated storage " +
		"of a DB instance using the modify-db-instance command\". The documented alternatives are a " +
		"blue/green deployment or a migration to a new instance, neither of which Kilter performs"
	if !v.FillKnown {
		return base
	}
	return fmt.Sprintf("%s of %d GiB allocated was never used across the window (%s) — %s",
		fmtGiB(v.UnusedGiB), v.AllocatedGiB, fmtPct(v.UnusedFraction), base)
}

// StorageAttribution is how an allocated-storage change between two snapshots
// is accounted for.
type StorageAttribution string

const (
	// StorageUnchanged: the allocation is what it was.
	StorageUnchanged StorageAttribution = "unchanged"
	// StorageGrewUnattributed: the allocation increased. It was NOT Kilter —
	// this domain has no actuator and cannot change storage — and it may have
	// been autoscaling, which leaves no CloudTrail event. Unattributed is the
	// only honest answer and it is the default.
	StorageGrewUnattributed StorageAttribution = "grew-unattributed"
	// StorageShrankImpossible: the allocation DECREASED, which the API cannot
	// do. Something is wrong with the observation — two different instances
	// under one identifier, a replaced instance reusing a name, a bad fixture.
	// Reported as an anomaly rather than absorbed as a saving.
	StorageShrankImpossible StorageAttribution = "shrank-impossible"
)

// AttributeStorageGrowth classifies a change in allocated storage between two
// observations of the same instance.
//
// This is the ledger rule from trap 8, as a pure function so the rule has one
// implementation and one test. The direction of the defaults is the point: a
// growth is never Kilter's doing, and a shrink is never believed.
func AttributeStorageGrowth(priorGiB, currentGiB int64) (StorageAttribution, string) {
	switch {
	case priorGiB <= 0 || priorGiB == currentGiB:
		return StorageUnchanged, ""
	case currentGiB > priorGiB:
		return StorageGrewUnattributed, fmt.Sprintf(
			"allocated storage rose from %d GiB to %d GiB between snapshots. This domain has no actuator, "+
				"so it was not Kilter; storage autoscaling is the usual cause and its operations are not "+
				"logged by CloudTrail, so there is no event to correlate against. Recorded as unattributed "+
				"rather than as a change or a regression", priorGiB, currentGiB)
	default:
		return StorageShrankImpossible, fmt.Sprintf(
			"allocated storage fell from %d GiB to %d GiB, which no RDS API can do. The two observations "+
				"are probably not the same instance — a replaced instance reusing an identifier is the "+
				"common case. Reported as an anomaly; no saving is inferred from it", priorGiB, currentGiB)
	}
}
