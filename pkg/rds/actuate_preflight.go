package rds

// The pre-flight refusal layer.
//
// This file is written first and runs first, and nothing in it can act. It is
// PURE: no I/O, no clock, no mutable state, no reachable mutation. It takes a
// step and the facts a read seam already fetched, and answers one question —
// may this database's storage be modified right now? — with either silence or
// a [RefusalError] carrying a stable machine-readable code.
//
// Four rules govern it, and the first is the one that matters.
//
//  1. **When in doubt, refuse.** Not "default to the safe value", not "assume
//     the common case": refuse, with a code that names the missing fact. An
//     unreadable modification history refuses. An unreadable tag set refuses.
//     An envelope nobody answered refuses. FINDINGS.md §5.3 makes this
//     explicit for the cooldown, where `Known=false` MUST block, and the same
//     reasoning covers every other unknown here.
//  2. **The ratchet only turns one way.** This actuator moves a volume UP or
//     SIDEWAYS and never down. A reduction of provisioned IOPS or throughput
//     is a real saving U13 will happily identify, and it is also the change
//     that starves a production primary of I/O if the measurement was wrong.
//     U14 refuses to execute it; a human does it by hand. See
//     ACTUATE-FINDINGS.md §4 for the honest cost of that decision.
//  3. **Everything is re-read live.** U13's assessment is minutes to hours
//     old. The envelope, the modification history, the instance status and
//     the allocated storage are all re-read microseconds before the call and
//     re-validated with the SAME functions the read-only path used.
//  4. **The refusal is the product.** Every predicate has a code, the code is
//     asserted by exactly one test, and the prose names the fact that blocked
//     it. A refusal a human cannot act on is a bug.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// ErrRefused is what every pre-flight refusal matches with errors.Is. The
// specific reason is the [RefusalError.Code], read with [RefusalCode].
const ErrRefused actuateError = "rds: refused"

// Refusal codes.
//
// Codes the read-only path already defines are REUSED, not redefined: an
// operator filtering on `storage-modification-cooldown` must see U13's
// suppression and U14's refusal under one code, or the roll-up silently splits
// one fact in two.
const (
	// --- FINDINGS.md §5.3: the four-per-24-hours limit ---

	// RefuseCooldown: four storage modifications already fell inside the
	// trailing 24 hours. A fifth is an API error, not a change.
	RefuseCooldown = ReasonParityCooldown
	// RefuseCooldownUnknown: the event seam did not answer, so the count is
	// UNKNOWN. §5.3: unknown never clears the cooldown. It is a separate code
	// from [RefuseCooldown] because the operator's next action differs —
	// "wait until ClearsAt" against "grant rds:DescribeEvents" — and one code
	// for both would hide which of those is needed.
	RefuseCooldownUnknown = "storage-modification-history-unknown"

	// --- FINDINGS.md §5.4: the in-flight gate, re-checked live ---

	// RefuseStateUnstable: the instance is `modifying` or
	// `storage-optimization` right now. Reuses U13's code.
	RefuseStateUnstable = ReasonParityStorageOptimization
	// RefuseNotAvailable: the instance is in some other non-available state —
	// stopped, backing-up, failing over, deleting. A storage modification
	// against one of those either fails or lands somewhere nobody predicted.
	RefuseNotAvailable = "instance-not-available"
	// RefusePendingModification: RDS has already accepted a storage change it
	// has not applied. Issuing a second one spends another of the four
	// modifications this instance gets in 24 hours.
	RefusePendingModification = "storage-modification-pending"

	// --- FINDINGS.md §5.2: the envelope, re-read live ---

	// RefuseEnvelopeUnknown: DescribeValidDBInstanceModifications was not
	// answered for this instance at execute time. Reuses U13's code.
	RefuseEnvelopeUnknown = ReasonParityEnvelopeUnknown
	// RefuseExceedsEnvelope: the LIVE envelope rejects the configuration the
	// plan carries. The commonest cause is the one §5.2 names: the instance
	// class changed between plan and apply, and a different class has a
	// different envelope.
	RefuseExceedsEnvelope = ReasonParityExceedsEnvelope
	// RefuseNotProvisionable: the size is below the striping threshold, where
	// the published provisioning columns read "N/A". Reuses U13's code.
	RefuseNotProvisionable = ReasonParityNotProvisionableBelowThreshold
	// RefuseBaselineArgument: the step asks to SEND an --iops or
	// --storage-throughput argument for a value that equals the regime
	// baseline. The baseline is what the volume delivers for free; naming it
	// is at best a wasted modification out of four and at worst an error.
	RefuseBaselineArgument = "baseline-value-must-not-be-sent"

	// --- the trap-8 ratchet ---

	// RefuseRatchet: the step would reduce IOPS, throughput or allocated
	// storage below what is there now. This actuator only moves up or
	// sideways.
	RefuseRatchet = "storage-performance-ratchet"
	// RefuseAllocationDrift: the observed allocated storage no longer matches
	// the allocation the proposal was computed against. Storage autoscaling
	// moves that number without anyone asking, and every figure in the plan —
	// the regime, the striping verdict, the price — was derived from the old
	// one.
	RefuseAllocationDrift = "allocated-storage-drift"

	// --- shape of the request ---

	// RefuseWrongAction: the step's action class is not the in-place storage
	// modification this actuator performs.
	RefuseWrongAction = "wrong-action"
	// RefuseBadStep: the step is structurally unusable — no target, no
	// storage type, a key that does not hash its own contents.
	RefuseBadStep = "bad-step"
	// RefuseNoChange: From and To describe the same configuration.
	RefuseNoChange = "no-change"
	// RefuseStorageTypeNotModelled: a storage type outside gp2/gp3. Reuses
	// U13's code.
	RefuseStorageTypeNotModelled = ReasonParityStorageTypeNotModelled
	// RefuseSizeUnusable: an allocation outside 1–65,536 GiB. Reuses U13's.
	RefuseSizeUnusable = ReasonParitySizeUnusable
	// RefuseUnknownEngine: no gp3 regime is encoded for the engine. Reuses
	// U11's code.
	RefuseUnknownEngine = ReasonUnknownEngine
	// RefuseEngineChanged: the step's From and To disagree about the engine,
	// or the live instance runs a different one. The regime is engine-keyed,
	// so this makes every number in the plan describe another database.
	RefuseEngineChanged = "engine-mismatch"

	// --- guardrails ---

	// RefuseModeOff: kilter.dev/mode=off. Reuses U11's code.
	RefuseModeOff = ReasonModeOff
	// RefuseGuardrailUnknown: the tag set could not be read, so the mode
	// guardrail is unknown. An unreadable "never touch this" is not an
	// absent one.
	RefuseGuardrailUnknown = "guardrail-tags-unknown"

	// --- live state ---

	// RefuseInstanceMissing: the instance is not in the account.
	RefuseInstanceMissing = "instance-missing"
	// RefuseDrift: the live instance matches neither the recorded From nor
	// the intended To. Somebody else changed it; the plan is stale.
	RefuseDrift = "drift"
)

// RefusalError is a refusal with a stable code. It is the only error type this
// unit's pre-flight produces, so a caller can render a refusal report without
// string matching.
type RefusalError struct {
	Code   string           `json:"code"`
	Target domain.TargetRef `json:"target"`
	Reason string           `json:"reason"`
	// ValidFrom is when a DATED refusal lapses on its own — for a cooldown,
	// the moment the oldest of the four modifications leaves the 24-hour
	// window (FINDINGS.md §5.3 calls this the right ValidFrom for a deferred
	// step). It is zero for a refusal that does not clear by waiting, and the
	// difference matters: a scheduler may retry the first kind and must never
	// spin on the second.
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

func (e *RefusalError) Error() string {
	if e.Target.ID != "" {
		return fmt.Sprintf("rds: refused %s (%s): %s", e.Target.ID, e.Code, e.Reason)
	}
	return fmt.Sprintf("rds: refused (%s): %s", e.Code, e.Reason)
}

// Is makes every refusal match [ErrRefused], so a caller that only cares
// "was this refused?" needs no type assertion.
func (e *RefusalError) Is(target error) bool { return target == error(ErrRefused) }

// refuse builds a refusal that does not clear by waiting.
func refuse(code string, ref domain.TargetRef, format string, args ...any) error {
	return &RefusalError{Code: code, Target: ref, Reason: fmt.Sprintf(format, args...)}
}

// refuseUntil builds a DATED refusal: one that lapses on its own at validFrom.
func refuseUntil(code string, ref domain.TargetRef, validFrom time.Time, format string, args ...any) error {
	return &RefusalError{Code: code, Target: ref, Reason: fmt.Sprintf(format, args...), ValidFrom: validFrom}
}

// RefusalValidFrom returns when a dated refusal lapses, or the zero time when
// err is not one or does not clear by waiting.
func RefusalValidFrom(err error) time.Time {
	var r *RefusalError
	if errors.As(err, &r) {
		return r.ValidFrom
	}
	return time.Time{}
}

// RefusalCode returns the machine-readable code of a refusal, or "" when err
// is not one.
func RefusalCode(err error) string {
	var r *RefusalError
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// IsRefusal reports whether err is a pre-flight refusal.
func IsRefusal(err error) bool { return errors.Is(err, ErrRefused) }

// --- the decoded step -------------------------------------------------------

// storageIntent is a step decoded into the fields the pre-flight reasons
// about. Every field comes from the step; nothing here is observed.
type storageIntent struct {
	ref    domain.TargetRef
	key    string
	engine Engine
	// allocGiB is the allocation both specs must agree on. This unit never
	// changes allocated storage (trap 8: the floor only ratchets up), so a
	// step whose From and To disagree about it is malformed, not ambitious.
	allocGiB   int64
	fromType   string
	toType     string
	fromIOPS   int32
	toIOPS     int32
	fromTput   int32
	toTput     int32
	claimedUSD float64
	claimed    bool
	revert     bool
	origin     string
}

// AttrNetSavingsMonthlyUSD is the optional attestation a step's To spec may
// carry: the net monthly bill delta through the commitment waterfall.
//
// For storage it is also the gross — "the price for a reserved DB instance
// doesn't provide a discount for the costs associated with storage, backups,
// and I/O" [verified], FINDINGS.md §6.1 — so there is exactly one number and
// no waterfall to get wrong. It lives under a `kilter.dev/` annotation prefix
// beside the resource axes, which means [domain.StepKey] hashes it and editing
// a savings claim after approval changes the key and voids the approval.
//
// It is OPTIONAL and never a gate. This unit does not decide whether a change
// is worth making — U13 did that, against rates whose provenance it already
// refused to overstate. Carrying the number lets the ledger roll up what was
// actually executed without a second source of truth for the bill.
const AttrNetSavingsMonthlyUSD = "kilter.dev/net-savings-monthly-usd"

// decodeStep validates a step's shape and reads its attributes. It is the
// first gate: nothing past it has to wonder whether a field was set.
func decodeStep(step domain.Step, revert bool, origin string) (storageIntent, error) {
	var in storageIntent
	ref := step.Target
	if step.Action != domain.ActionInPlace {
		return in, refuse(RefuseWrongAction, ref,
			"step action is %q; this actuator performs %q storage modifications and nothing else",
			step.Action, domain.ActionInPlace)
	}
	if ref.Domain != Kind {
		return in, refuse(RefuseBadStep, ref, "step targets domain %q, not %q", ref.Domain, Kind)
	}
	if strings.TrimSpace(ref.ID) == "" {
		return in, refuse(RefuseBadStep, ref, "step has no target DB instance identifier")
	}
	if step.Key == "" {
		return in, refuse(RefuseBadStep, ref, "step has no idempotency key")
	}
	if got := domain.StepKey(ref, step.From, step.To); got != step.Key {
		return in, refuse(RefuseBadStep, ref,
			"step key %q does not hash its own contents (%q): the plan was edited after it was built",
			step.Key, got)
	}
	in.ref, in.key, in.revert, in.origin = ref, step.Key, revert, origin

	// The engine is engine-keyed policy, not decoration: it selects the
	// striping threshold, which selects the whole gp3 regime.
	fromEng := strings.TrimSpace(step.From.Attr(AttrEngine))
	toEng := strings.TrimSpace(step.To.Attr(AttrEngine))
	if fromEng == "" || toEng == "" {
		return in, refuse(RefuseBadStep, ref,
			"step does not name the engine on both specs (from %q, to %q); the gp3 regime is engine-keyed",
			fromEng, toEng)
	}
	if !strings.EqualFold(fromEng, toEng) {
		return in, refuse(RefuseEngineChanged, ref,
			"step changes engine %q → %q; this unit never does, and the striping threshold differs between them",
			fromEng, toEng)
	}
	in.engine = ParseEngine(fromEng, step.From.Attr(AttrLicenseModel))
	if !in.engine.Known() {
		return in, refuse(RefuseUnknownEngine, ref,
			"engine %q is not one this package models, so no striping threshold and no gp3 regime exist for it",
			fromEng)
	}

	fromAlloc := intAttr(step.From, AttrAllocatedStorageGiB)
	toAlloc := intAttr(step.To, AttrAllocatedStorageGiB)
	if fromAlloc <= 0 || toAlloc <= 0 {
		return in, refuse(RefuseBadStep, ref,
			"step does not state the allocated storage on both specs (from %d, to %d GiB)", fromAlloc, toAlloc)
	}
	if fromAlloc != toAlloc {
		return in, refuse(RefuseRatchet, ref,
			"step changes allocated storage %d → %d GiB. This unit modifies storage PERFORMANCE and never "+
				"the allocation: allocated storage is a one-way ratchet (trap 8) whose floor can never be "+
				"lowered again, and buying that permanently to gain a reversible performance regime is not "+
				"a trade this actuator makes on an operator's behalf",
			fromAlloc, toAlloc)
	}
	if fromAlloc > MaxParitySizeGiB {
		return in, refuse(RefuseSizeUnusable, ref,
			"allocated storage %d GiB is outside the 1–%d GiB range this package models",
			fromAlloc, MaxParitySizeGiB)
	}
	in.allocGiB = fromAlloc

	in.fromType = strings.ToLower(strings.TrimSpace(step.From.Attr(AttrStorageType)))
	in.toType = strings.ToLower(strings.TrimSpace(step.To.Attr(AttrStorageType)))
	for _, t := range []string{in.fromType, in.toType} {
		if t != StorageGP2 && t != StorageGP3 {
			return in, refuse(RefuseStorageTypeNotModelled, ref,
				"storage type %s is not one this unit modifies. It models gp2 and gp3 only: io1 and io2 "+
					"are a different product with their own price function and their own conversion risks",
				orNone(t))
		}
	}
	if in.toType != StorageGP3 {
		return in, refuse(RefuseStorageTypeNotModelled, ref,
			"step targets %s. Every modification this unit performs lands on gp3", in.toType)
	}

	in.fromIOPS = int32Attr(step.From, AttrIOPS)
	in.toIOPS = int32Attr(step.To, AttrIOPS)
	in.fromTput = int32Attr(step.From, AttrStorageThroughput)
	in.toTput = int32Attr(step.To, AttrStorageThroughput)
	if in.toIOPS <= 0 || in.toTput <= 0 {
		return in, refuse(RefuseBadStep, ref,
			"step does not state the target effective IOPS and throughput (%d IOPS, %d MiB/s). "+
				"ModifyDBInstance takes ABSOLUTE values, so a proposal that omits one is not a smaller "+
				"change, it is an unspecified one",
			in.toIOPS, in.toTput)
	}
	if in.fromType == in.toType && in.fromIOPS == in.toIOPS && in.fromTput == in.toTput {
		return in, refuse(RefuseNoChange, ref,
			"from and to are the same configuration (%s, %d IOPS, %d MiB/s)",
			in.fromType, in.fromIOPS, in.fromTput)
	}

	if raw := strings.TrimSpace(step.To.Attr(AttrNetSavingsMonthlyUSD)); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		switch {
		case err != nil:
			return in, refuse(RefuseBadStep, ref, "%s is not a number: %q", AttrNetSavingsMonthlyUSD, raw)
		case v != v || v > 1e15 || v < -1e15:
			return in, refuse(RefuseBadStep, ref, "%s is not a finite number: %q", AttrNetSavingsMonthlyUSD, raw)
		default:
			in.claimedUSD, in.claimed = v, true
		}
	}
	return in, nil
}

// intAttr reads a non-negative integer attribute; anything unparseable reads
// as -1 so a garbage value can never pass for zero.
func intAttr(s domain.Spec, key string) int64 {
	raw := strings.TrimSpace(s.Attr(key))
	if raw == "" {
		return -1
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return -1
	}
	return v
}

func int32Attr(s domain.Spec, key string) int32 {
	v := intAttr(s, key)
	if v < 0 || v > 1<<31-1 {
		return -1
	}
	return int32(v)
}

// --- the observed facts -----------------------------------------------------

// storageFacts is everything the read seams reported at execute time. It is
// data: the predicates below take it and touch nothing.
type storageFacts struct {
	live   InstanceStateRecord
	env    Envelope
	cool   CooldownVerdict
	regime GP3Regime
	// liveCfg is the live configuration expressed in the same GP3Config shape
	// as the plan's, floored at the regime baseline exactly as
	// [ParityPlan.Current] is (FINDINGS.md §5.6).
	liveCfg GP3Config
	// want is the configuration the step asks for.
	want GP3Config
	// from is the configuration the step recorded as current, which is the
	// exact value a revert restores.
	from GP3Config
}

// configOf expresses a from/to pair of the step in GP3Config terms under a
// regime. Values below the baseline are raised TO the baseline, never sent as
// themselves: the baseline is non-reducible, so a plan naming a lower number
// is describing a volume that does not exist.
func configOf(r GP3Regime, sizeGiB int64, iops, tput int32) GP3Config {
	c := GP3Config{SizeGiB: sizeGiB, IOPS: iops, ThroughputMBps: tput}
	if c.IOPS < r.BaselineIOPS {
		c.IOPS = r.BaselineIOPS
	}
	if c.ThroughputMBps < r.BaselineThroughputMBps {
		c.ThroughputMBps = r.BaselineThroughputMBps
	}
	c.ProvisionedIOPS = c.IOPS > r.BaselineIOPS
	c.ProvisionedThroughput = c.ThroughputMBps > r.BaselineThroughputMBps
	return c
}

// inFlightTowardTarget reports whether the change RDS is ALREADY applying is
// the one this step asked for.
//
// It is the single most important predicate in this file after the ratchet,
// and it exists because the three gates that stop a modification being ISSUED
// — `modifying`, `storage-optimization`, a pending change — are exactly the
// states a RESUMED step is legitimately in. A pre-flight that could not tell
// the two apart would refuse to observe the very modification it started,
// leaving a production database mid-change with nobody watching it. That is a
// worse failure than the one those gates prevent.
//
// It is safe to be wrong in the permissive direction here and only here,
// because a step that reaches the execute path in a resuming state issues
// NOTHING: [Actuator.execute] sends a modification only from [StageReady], and
// no state this function accepts derives to StageReady. The worst case of a
// false positive is a poll budget spent watching an instance that is not
// changing, which ends as an honest in-flight entry.
func inFlightTowardTarget(in storageIntent, f storageFacts) bool {
	// An allocation change is never ours: this unit does not make them.
	if f.live.PendingAllocatedStorageGiB > 0 {
		return false
	}
	// The values have already landed and AWS is still optimizing behind them.
	liveAtTarget := f.live.NormalizedStorageType() == in.toType &&
		f.liveCfg.IOPS == f.want.IOPS && f.liveCfg.ThroughputMBps == f.want.ThroughputMBps
	if liveAtTarget {
		return !f.live.PendingStorageChange()
	}
	if !f.live.PendingStorageChange() {
		return false
	}
	// A pending change: compare what the instance will BE once it lands.
	effType := strings.ToLower(strings.TrimSpace(f.live.PendingStorageType))
	if effType == "" {
		effType = f.live.NormalizedStorageType()
	}
	effIOPS, effTput := f.live.IOPS, f.live.StorageThroughputMBps
	if f.live.PendingIOPS > 0 {
		effIOPS = f.live.PendingIOPS
	}
	if f.live.PendingStorageThroughputMBps > 0 {
		effTput = f.live.PendingStorageThroughputMBps
	}
	eff := configOf(f.regime, in.allocGiB, effIOPS, effTput)
	return effType == in.toType && eff.IOPS == f.want.IOPS && eff.ThroughputMBps == f.want.ThroughputMBps
}

// checkStorage is the whole pre-flight, and it is pure.
//
// The order is not cosmetic. Guardrails and liveness come before arithmetic so
// that a mode=off instance is refused for being mode=off rather than for some
// incidental envelope detail, and the cooldown comes before the envelope so an
// operator sees "you have used your four modifications" rather than a
// provisioning complaint they cannot act on until tomorrow anyway.
//
// now is an argument; this package reads no clock.
func checkStorage(in storageIntent, f storageFacts, now time.Time) error {
	live := f.live.Instance()

	// --- guardrails ---
	if !f.live.TagsKnown {
		return refuse(RefuseGuardrailUnknown, in.ref,
			"the tag set for %s could not be read, so the kilter.dev/mode guardrail is UNKNOWN. An "+
				"unreadable \"never touch this\" is not an absent one, and the whole point of the tag is "+
				"that it works when nobody is watching",
			in.ref.ID)
	}
	if live.ModeOff() {
		return refuse(RefuseModeOff, in.ref,
			"%s carries %s=off. That tag is an operator saying never, and it outranks every number in "+
				"this plan", in.ref.ID, TagKilterMode)
	}

	// Is AWS already applying the change this step asked for? If so, the only
	// thing left to do is watch it, and the gates below — which decide
	// whether a modification may be ISSUED — do not apply. Identity and the
	// guardrails still do.
	resuming := inFlightTowardTarget(in, f)
	if resuming {
		return checkResumeIdentity(in, f)
	}

	// --- FINDINGS.md §5.4: the in-flight gate, re-read live ---
	if live.StateUnstable() {
		return refuse(RefuseStateUnstable, in.ref,
			"%s is in state %q right now. \"You can't modify allocated storage if the DB instance status "+
				"is storage-optimization\", and that state persists for hours after a modification. U13 "+
				"observed this instance minutes to hours ago; this is the state it is in as the call would "+
				"be made", in.ref.ID, f.live.Status)
	}
	if st := strings.ToLower(strings.TrimSpace(f.live.Status)); st != StatusAvailable {
		return refuse(RefuseNotAvailable, in.ref,
			"%s is in state %q, not %q. A storage modification is only defined against an available "+
				"instance; against anything else it either fails or lands at a moment nobody chose",
			in.ref.ID, orNone(f.live.Status), StatusAvailable)
	}
	if f.live.PendingStorageChange() {
		return refuse(RefusePendingModification, in.ref,
			"%s already has a storage modification pending (type %s, %d IOPS, %d MiB/s, %d GiB). RDS has "+
				"accepted it and not finished applying it; issuing another spends a second of the four "+
				"modifications this instance is allowed in 24 hours",
			in.ref.ID, orNone(f.live.PendingStorageType), f.live.PendingIOPS,
			f.live.PendingStorageThroughputMBps, f.live.PendingAllocatedStorageGiB)
	}

	// --- FINDINGS.md §5.3: the cooldown. Unknown BLOCKS. ---
	if !f.cool.Known {
		return refuse(RefuseCooldownUnknown, in.ref,
			"the storage-modification history for %s could not be read, so the four-per-24-hours limit is "+
				"UNKNOWN. Unknown is not zero: an instance that has already had four modifications looks "+
				"exactly like this one from here, and the difference between them is an API error against "+
				"a production database. Grant rds:DescribeEvents or wait",
			in.ref.ID)
	}
	if f.cool.Blocked {
		return refuseUntil(RefuseCooldown, in.ref, f.cool.ClearsAt,
			"%s has had %d storage modifications in the last %s. \"You can perform a maximum of four "+
				"storage modifications on a DB instance within any 24-hour period\", so a fifth is not a "+
				"change, it is an API error. The limit clears at %s",
			in.ref.ID, f.cool.Recent, StorageModificationWindow,
			f.cool.ClearsAt.UTC().Format(time.RFC3339))
	}

	// --- the plan still describes this instance ---
	if err := checkIdentity(in, f); err != nil {
		return err
	}

	// --- drift: the live volume is neither what we recorded nor what we want ---
	liveType := f.live.NormalizedStorageType()
	atTarget := liveType == in.toType && f.liveCfg.IOPS == f.want.IOPS &&
		f.liveCfg.ThroughputMBps == f.want.ThroughputMBps
	atFrom := liveType == in.fromType && f.liveCfg.IOPS == f.from.IOPS &&
		f.liveCfg.ThroughputMBps == f.from.ThroughputMBps
	if !atTarget && !atFrom {
		return refuse(RefuseDrift, in.ref,
			"%s is %s at %d IOPS / %d MiB/s; the plan recorded %s at %d IOPS / %d MiB/s and targets %s at "+
				"%d IOPS / %d MiB/s. Somebody else changed this volume, so the plan describes a database "+
				"that no longer exists",
			in.ref.ID, orNone(liveType), f.liveCfg.IOPS, f.liveCfg.ThroughputMBps,
			in.fromType, f.from.IOPS, f.from.ThroughputMBps,
			in.toType, f.want.IOPS, f.want.ThroughputMBps)
	}
	if atTarget {
		// Already there. Nothing below can refuse doing nothing.
		return nil
	}

	// --- the trap-8 ratchet: up or sideways, never down ---
	//
	// Checked against the LIVE configuration, not against the step's From, so
	// a volume somebody else already lowered cannot be lowered further by a
	// plan that predates them.
	if f.want.IOPS < f.liveCfg.IOPS || f.want.ThroughputMBps < f.liveCfg.ThroughputMBps {
		return refuse(RefuseRatchet, in.ref,
			"%s delivers %d IOPS / %d MiB/s and this step would set %d IOPS / %d MiB/s. This actuator "+
				"moves a volume up or sideways and never down. Reducing provisioned performance is a real "+
				"saving and it is also the change that starves a production primary of I/O if the "+
				"measurement behind it was wrong, so it is executed by a human with a hand on the "+
				"CloudWatch graph, not by a controller at 3 a.m.",
			in.ref.ID, f.liveCfg.IOPS, f.liveCfg.ThroughputMBps, f.want.IOPS, f.want.ThroughputMBps)
	}

	// --- FINDINGS.md §5.2: re-validate against the LIVE envelope ---
	gp3 := f.env.For(StorageGP3)
	if f.want.Provisions() && !f.regime.Provisionable {
		return refuse(RefuseNotProvisionable, in.ref,
			"a %d GiB %s volume is below the %d GiB striping threshold, where the published provisioning "+
				"columns read \"N/A\". %d IOPS / %d MiB/s cannot be bought there at any price, so the "+
				"arguments must not be sent",
			in.allocGiB, f.regime.Engine, f.regime.ThresholdGiB, f.want.IOPS, f.want.ThroughputMBps)
	}
	if f.want.Provisions() && !gp3.Known {
		return refuse(RefuseEnvelopeUnknown, in.ref,
			"%d IOPS / %d MiB/s must be checked against DescribeValidDBInstanceModifications for %s, and "+
				"it was not answered at execute time. AWS publishes two contradictory gp3 ceilings and "+
				"this package hardcodes neither, so an unread envelope is a refusal and never a guess",
			f.want.IOPS, f.want.ThroughputMBps, in.ref.ID)
	}
	if err := f.want.Validate(f.regime, gp3); err != nil {
		return refuse(RefuseExceedsEnvelope, in.ref,
			"the LIVE envelope for %s (%s) rejects the configuration this plan carries: %v. The commonest "+
				"cause is the one that makes re-reading mandatory — the instance class changed between "+
				"plan and apply, and a different class has a different envelope",
			in.ref.ID, gp3.Describe(), err)
	}

	// --- the baseline must not be sent ---
	//
	// Last, because it is the narrowest: a step that gets everything else
	// right can still name a number that is free.
	return checkNoBaselineArgument(in, f)
}

// checkIdentity is the set of facts that must hold whatever this step is about
// to do: the plan describes THIS database, at THIS size, running THIS engine.
// Nothing here is about whether a modification may be issued.
func checkIdentity(in storageIntent, f storageFacts) error {
	if eng := strings.TrimSpace(f.live.Engine); eng != "" && !sameEngineFamily(in.engine, eng, f.live.LicenseModel) {
		return refuse(RefuseEngineChanged, in.ref,
			"the plan was built for engine %q and %s is running %q. The striping threshold — and therefore "+
				"the entire gp3 regime, its baseline and whether anything can be provisioned at all — is "+
				"engine-keyed", in.engine.Raw, in.ref.ID, eng)
	}
	if f.live.AllocatedStorageGiB != in.allocGiB {
		return refuse(RefuseAllocationDrift, in.ref,
			"%s is allocated %d GiB and the plan was computed against %d GiB. Storage autoscaling moves "+
				"that number without anyone asking, and the striping verdict, the regime baseline, the "+
				"envelope and every price in this plan were derived from the old one",
			in.ref.ID, f.live.AllocatedStorageGiB, in.allocGiB)
	}
	if !f.regime.Known {
		return refuse(RefuseUnknownEngine, in.ref,
			"no gp3 regime is encoded for engine %q at %d GiB", in.engine.Raw, in.allocGiB)
	}
	return nil
}

// checkResumeIdentity is the whole pre-flight for a step that is only going to
// WATCH a modification AWS has already accepted. It runs the identity checks
// and nothing else, because everything else in this file answers a question
// nobody is asking any more: the modification is running, and the choice is
// between observing it and not.
func checkResumeIdentity(in storageIntent, f storageFacts) error {
	return checkIdentity(in, f)
}

// checkNoBaselineArgument is the last predicate and the narrowest: it proves
// that the call this pre-flight is about to authorize sends an --iops or
// --storage-throughput argument ONLY where the regime says one may be sent.
//
// FINDINGS.md §5.1 states the rule in two halves and both are enforced here:
// a value equal to the regime baseline is free and needs no argument, and
// below the striping threshold sending one at all is an error.
func checkNoBaselineArgument(in storageIntent, f storageFacts) error {
	// The sub-threshold guard is stated against the CONFIGURATION rather than
	// against the arguments [argumentsFor] derived from it, so it stays a real
	// check rather than a restatement of that function. It shares
	// [RefuseNotProvisionable] with the earlier gate because it is the same
	// fact — a size below the striping threshold accepts nothing — and one
	// fact must not reach an operator under two codes.
	if !f.regime.Provisionable && f.want.Provisions() {
		return refuse(RefuseNotProvisionable, in.ref,
			"a %d GiB %s volume accepts no provisioning arguments, and this call would ask for %d IOPS / "+
				"%d MiB/s", in.allocGiB, f.regime.Engine, f.want.IOPS, f.want.ThroughputMBps)
	}
	sendIOPS, sendTput := argumentsFor(f.regime, f.want)
	if sendIOPS > 0 && sendIOPS == f.regime.BaselineIOPS {
		return refuse(RefuseBaselineArgument, in.ref,
			"the call would send --iops %d, which is exactly the free baseline for the %s regime at %d "+
				"GiB. Sending it buys nothing and spends one of four modifications per 24 hours",
			sendIOPS, regimeName(f.regime), in.allocGiB)
	}
	if sendTput > 0 && sendTput == f.regime.BaselineThroughputMBps {
		return refuse(RefuseBaselineArgument, in.ref,
			"the call would send --storage-throughput %d, which is exactly the free baseline for the %s "+
				"regime at %d GiB", sendTput, regimeName(f.regime), in.allocGiB)
	}
	return nil
}

// argumentsFor decides which values are SENT, which is the whole of §5.1's
// second half. A configuration sitting on the baseline provisions nothing and
// names nothing; zero means "omit the argument".
//
// It is a pure function of the regime and the configuration and is used by
// both the pre-flight and the call builder, so the thing checked and the thing
// sent cannot drift apart.
func argumentsFor(r GP3Regime, c GP3Config) (iops, tput int32) {
	if !r.Provisionable {
		return 0, 0
	}
	if c.ProvisionedIOPS {
		iops = c.IOPS
	}
	if c.ProvisionedThroughput {
		tput = c.ThroughputMBps
	}
	return iops, tput
}

// sameEngineFamily reports whether a live engine string still describes the
// engine the plan was built for. It compares the parsed FAMILY rather than the
// raw string, because "postgres" and "postgres" differing only in an edition
// suffix must not be treated as a different database — and because the family
// is what selects the striping threshold.
func sameEngineFamily(planned Engine, liveEngine, liveLicense string) bool {
	live := ParseEngine(liveEngine, liveLicense)
	return live.Known() && live.Family == planned.Family
}
