package ec2

// The pre-flight refusal layer.
//
// This file is the reason U7 exists in the shape it does. Everything below is
// PURE: no I/O, no clock, no mutable state, no way to reach a mutation. It
// takes a step and the facts a read-only seam already fetched, and it answers
// one question — may this instance be stopped and resized? — with either
// silence or a [RefusalError] carrying a stable machine-readable code.
//
// Three rules govern it.
//
//  1. **Absence of evidence refuses.** An unreadable shutdown behavior, an
//     instance type the catalog has never heard of, a missing AMI: each is a
//     refusal, not a default. §3.3 says "on any doubt the step is advisory".
//  2. **The refusal is the product.** Every predicate has a code, the code is
//     asserted by exactly one test, and the prose names the fact that blocked
//     it. A refusal a human cannot act on is a bug.
//  3. **Economics are judged once, before anything is touched.** See
//     actuate_instance.go: the moment an instance is stopped, the machine's
//     only remaining job is to get it running again. Re-litigating whether
//     the resize saves money while the instance is down is how a fleet ends up
//     stopped and forgotten.

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// ErrRefused is what every pre-flight refusal matches with errors.Is. The
// specific reason is the [RefusalError.Code], read with [RefusalCode].
var ErrRefused = errors.New("ec2: refused")

// Refusal codes for the actuator. Codes the read-only sizer already defines
// are REUSED, not redefined: an operator filtering a report on `memory-blind`
// must see the sizer's refusal and the actuator's refusal under one code.
const (
	// --- ownership and guardrails (§3.3 "Never") ---

	// RefuseK8sTagged: the instance belongs to a Kubernetes cluster.
	RefuseK8sTagged = ReasonK8sTagged
	// RefuseModeOff: kilter.dev/mode=off.
	RefuseModeOff = ReasonModeOff
	// RefuseASGMember: the instance was launched by an Auto Scaling group.
	// Resizing it directly is reverted by the next scale-out, and §3.3
	// forbids touching ASG instances except through the group.
	RefuseASGMember = ReasonASGManaged

	// --- data loss ---

	// RefuseInstanceStore: the instance has ephemeral local storage. Stopping
	// it destroys that data, permanently and silently. §3.3: never.
	RefuseInstanceStore = "instance-store"
	// RefuseShutdownBehavior: instance-initiated shutdown behavior is
	// "terminate", or could not be read at all.
	RefuseShutdownBehavior = "shutdown-behavior"
	// RefuseStorageShrink: the step reduces storage. Never allowed (§3.3).
	RefuseStorageShrink = "storage-shrink"
	// RefuseHibernation: the instance is hibernation-configured; its stop
	// writes a RAM image tied to the shape we are about to change.
	RefuseHibernation = "hibernation-configured"

	// --- boot prerequisites (§6 U7: "ENA/NVMe") ---

	// RefuseENAUnsupported: the target generation requires enhanced
	// networking and the running pair (instance flag, AMI flag) does not
	// demonstrate it.
	RefuseENAUnsupported = "ena-unsupported"
	// RefuseNVMeUnsupported: the target generation requires NVMe block
	// devices and the AMI has not been observed booting on NVMe.
	RefuseNVMeUnsupported = "nvme-unsupported"
	// RefuseArchMismatch: the target type does not run the AMI's architecture.
	RefuseArchMismatch = "arch-mismatch"
	// RefuseVirtualization: the AMI's virtualization type is not supported by
	// the target.
	RefuseVirtualization = "virtualization-unsupported"
	// RefuseRootDevice: the target does not support the AMI's root device
	// type.
	RefuseRootDevice = "root-device-unsupported"
	// RefuseUnknownInstanceType: DescribeInstanceTypes had no record. An
	// unknown target is never assumed compatible.
	RefuseUnknownInstanceType = ReasonUnknownInstanceType
	// RefuseImageMissing: the AMI is gone or not available. A stopped
	// instance whose AMI was deregistered still starts, but nothing about its
	// prerequisites can be verified, so this refuses before the stop.
	RefuseImageMissing = "image-missing"

	// --- economics (§7 traps 1 and 4) ---

	// RefuseCommitmentNegative: the change raises the bill through the
	// commitment waterfall.
	RefuseCommitmentNegative = domain.SuppressCommitmentNegative
	// RefuseCommitmentUnchecked: the step carries no commitment-checked
	// savings attestation, so nobody knows whether it raises the bill.
	RefuseCommitmentUnchecked = "commitment-unchecked"
	// RefuseMemoryBlind: the step cuts memory with no memory signal (§7 trap
	// 4). Same code the sizer uses for the same rule.
	RefuseMemoryBlind = ReasonMemoryBlind

	// --- shape of the request ---

	// RefuseWrongAction: the step's action class is not one this actuator
	// performs.
	RefuseWrongAction = "wrong-action"
	// RefuseBadStep: the step is structurally unusable — no target, no
	// instance type, a key that does not hash its contents.
	RefuseBadStep = "bad-step"
	// RefuseNoChange: From and To describe the same shape.
	RefuseNoChange = "no-change"
	// RefuseTenancy: the step would move a dedicated or host-tenancy
	// instance, or the target type cannot host it. §3.3: never change tenancy.
	RefuseTenancy = "tenancy"
	// RefuseSpot: Spot instances are not resized by this unit. A stopped Spot
	// instance's capacity is not held for it.
	RefuseSpot = "spot-instance"
	// RefuseBareMetal: metal instances have no stop-resize path worth the
	// risk here.
	RefuseBareMetal = "bare-metal"

	// --- live state ---

	// RefuseInstanceMissing: the instance is not in the account.
	RefuseInstanceMissing = "instance-missing"
	// RefuseInstanceState: the instance is terminating, terminated, or
	// otherwise in a state no step may act on.
	RefuseInstanceState = "instance-state"
	// RefuseDrift: the live instance matches neither the recorded From nor
	// the intended To. Somebody else changed it; the plan is stale.
	RefuseDrift = "drift"
)

// RefusalError is a refusal with a stable code. It is the only error type this
// package's pre-flight produces, so a caller can render a refusal report
// without string matching.
type RefusalError struct {
	Code   string           `json:"code"`
	Target domain.TargetRef `json:"target"`
	Reason string           `json:"reason"`
}

func (e *RefusalError) Error() string {
	if e.Target.ID != "" {
		return fmt.Sprintf("ec2: refused %s (%s): %s", e.Target.ID, e.Code, e.Reason)
	}
	return fmt.Sprintf("ec2: refused (%s): %s", e.Code, e.Reason)
}

// Is makes every refusal match [ErrRefused], so a caller that only cares
// "was this refused?" needs no type assertion.
func (e *RefusalError) Is(target error) bool { return target == ErrRefused }

// refuse builds a refusal.
func refuse(code string, ref domain.TargetRef, format string, args ...any) error {
	return &RefusalError{Code: code, Target: ref, Reason: fmt.Sprintf(format, args...)}
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

// --- attestation attributes -------------------------------------------------

// Attestation attributes a step's To spec must carry.
//
// They live in [domain.Spec.Attrs] under a `kilter.dev/` prefix — an
// annotation namespace, deliberately distinct from the resource axes
// (`instanceType`, `arch`, …) beside them — for one reason: [domain.StepKey]
// hashes every attribute, so the attestation is part of the step's idempotency
// key and therefore part of the plan fingerprint a human approved. Editing a
// savings claim after approval changes the key, and the approval stops
// covering the step. An attestation carried outside the spec would be a number
// anyone could rewrite between approval and execution.
//
// This is the single deliberate deviation from pkg/ebs's actuator shape; see
// ACTUATE-FINDINGS.md §Deviations.
const (
	// AttrNetSavingsMonthlyUSD is the bill delta through the commitment
	// waterfall (§4.4) — the only number that may be called a saving.
	AttrNetSavingsMonthlyUSD = "kilter.dev/net-savings-monthly-usd"
	// AttrGrossSavingsMonthlyUSD is the on-demand list-price delta.
	AttrGrossSavingsMonthlyUSD = "kilter.dev/gross-savings-monthly-usd"
	// AttrCommitmentCheckedAt is when the commitment inventory the net was
	// computed against was fetched, RFC3339. Absent ⇒ nothing was checked.
	AttrCommitmentCheckedAt = "kilter.dev/commitment-checked-at"
	// AttrMemorySignal is "cwagent" when a real memory series backed the
	// decision, "none" when the target is memory-blind (§7 trap 4).
	AttrMemorySignal = "kilter.dev/memory-signal"
	// AttrStorageGiB is the total attached EBS storage the step promises to
	// leave in place. Compared From→To; never reduced.
	AttrStorageGiB = "kilter.dev/storage-gib"
)

// Memory signal values.
const (
	MemorySignalCWAgent = "cwagent"
	MemorySignalNone    = "none"
)

// CommitmentMaxAge bounds how stale a commitment check may be. Reserved
// Instances and Savings Plans expire on dates; a net savings number computed
// against an inventory from last quarter is a guess about a different account.
const CommitmentMaxAge = 30 * 24 * time.Hour

// --- the decoded step -------------------------------------------------------

// intent is a step decoded into the fields the pre-flight reasons about.
type intent struct {
	ref      domain.TargetRef
	action   domain.ActionClass
	key      string
	fromType string
	toType   string
	fromMem  int64
	toMem    int64
	fromCPU  int64
	toCPU    int64
	fromStor int64
	toStor   int64

	// origin is the forward step's key when this intent is an undo, and
	// revert marks it as one.
	origin string
	revert bool
	// econErr defers an unreadable attestation to [checkEconomics], so an undo
	// — which is never judged on economics — is not blocked by a savings
	// claim it structurally cannot carry.
	econErr error

	memorySignal string
	net          float64
	gross        float64
	checkedAt    time.Time
}

// decodeStep validates a step's shape and reads its attributes. It is the
// first gate: nothing past it has to wonder whether a field was set.
func decodeStep(step domain.Step, want domain.ActionClass) (intent, error) {
	var in intent
	ref := step.Target
	if step.Action != want {
		return in, refuse(RefuseWrongAction, ref, "step action is %q, this path performs %q", step.Action, want)
	}
	if ref.Domain != Kind {
		return in, refuse(RefuseBadStep, ref, "step targets domain %q, not %q", ref.Domain, Kind)
	}
	if ref.ID == "" {
		return in, refuse(RefuseBadStep, ref, "step has no target ID")
	}
	if step.Key == "" {
		return in, refuse(RefuseBadStep, ref, "step has no idempotency key")
	}
	if got := domain.StepKey(ref, step.From, step.To); got != step.Key {
		return in, refuse(RefuseBadStep, ref,
			"step key %q does not hash its own contents (%q): the plan was edited after it was built",
			step.Key, got)
	}
	in.ref, in.action, in.key = ref, step.Action, step.Key
	in.fromType = strings.TrimSpace(step.From.Attr(AttrInstanceType))
	in.toType = strings.TrimSpace(step.To.Attr(AttrInstanceType))
	if in.fromType == "" || in.toType == "" {
		return in, refuse(RefuseBadStep, ref, "step does not name both instance types (from %q, to %q)",
			in.fromType, in.toType)
	}
	if strings.EqualFold(in.fromType, in.toType) {
		return in, refuse(RefuseNoChange, ref, "from and to are both %s", in.fromType)
	}
	in.fromMem, in.toMem = step.From.Resources.MemoryBytes, step.To.Resources.MemoryBytes
	in.fromCPU, in.toCPU = step.From.Resources.MilliCPU, step.To.Resources.MilliCPU
	if in.fromMem <= 0 || in.toMem <= 0 || in.fromCPU <= 0 || in.toCPU <= 0 {
		return in, refuse(RefuseBadStep, ref,
			"step does not state both shapes (from %d mCPU/%d B, to %d mCPU/%d B)",
			in.fromCPU, in.fromMem, in.toCPU, in.toMem)
	}
	// §3.3: never change tenancy or platform. Both are recorded on the spec by
	// the sizer, so a step that changes either is structurally rejected here
	// rather than being ignored by an actuator that only writes instanceType.
	if a, b := step.From.Attr(AttrTenancy), step.To.Attr(AttrTenancy); a != b {
		return in, refuse(RefuseTenancy, ref, "step changes tenancy %q → %q; this unit never does", a, b)
	}
	if a, b := step.From.Attr(AttrPlatform), step.To.Attr(AttrPlatform); a != b {
		return in, refuse(RefuseBadStep, ref, "step changes platform %q → %q; this unit never does", a, b)
	}
	in.fromStor = intAttr(step.From, AttrStorageGiB)
	in.toStor = intAttr(step.To, AttrStorageGiB)
	in.memorySignal = strings.TrimSpace(step.To.Attr(AttrMemorySignal))

	var err error
	if in.net, err = floatAttr(step.To, AttrNetSavingsMonthlyUSD); err != nil {
		in.econErr = refuse(RefuseCommitmentUnchecked, ref, "%v", err)
	}
	if in.gross, err = floatAttr(step.To, AttrGrossSavingsMonthlyUSD); err != nil && in.econErr == nil {
		in.econErr = refuse(RefuseCommitmentUnchecked, ref, "%v", err)
	}
	if at := strings.TrimSpace(step.To.Attr(AttrCommitmentCheckedAt)); at != "" {
		if in.checkedAt, err = time.Parse(time.RFC3339, at); err != nil && in.econErr == nil {
			in.econErr = refuse(RefuseCommitmentUnchecked, ref, "%s is not an RFC3339 time: %q",
				AttrCommitmentCheckedAt, at)
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

// floatAttr reads a money attribute. A missing or non-finite value is an
// error, never a zero: "no claim" and "claims exactly $0" are different
// statements and only one of them is a bug.
func floatAttr(s domain.Spec, key string) (float64, error) {
	raw := strings.TrimSpace(s.Attr(key))
	if raw == "" {
		return 0, fmt.Errorf("step carries no %s attestation", key)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not a number: %q", key, raw)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%s is not finite: %q", key, raw)
	}
	return v, nil
}

// --- the pure predicates ----------------------------------------------------

// checkEconomics is §7 traps 1 and 4, plus the storage rule. It is pure and it
// runs before a single cloud call, so a commitment-negative plan never even
// reads the account.
//
// An UNDO is exempt from the money and memory-signal halves, and deliberately.
// A revert restores a shape the workload demonstrably ran on; it is not an
// investment and it will never show a saving, so requiring one would make
// every bad change permanent. [domain.Registry.Revert] takes the same position
// about guardrails, for the same reason. The storage rule still applies, and
// so does every safety predicate in [checkSafety] — an undo may not destroy
// data either.
//
// now is passed in; this package has no clock.
func checkEconomics(in intent, now time.Time) error {
	if in.revert {
		return checkStorage(in, false)
	}
	if in.econErr != nil {
		return in.econErr
	}
	// Trap 1: commitment stranding. A rec that raises the bill must never be
	// executed, and a rec nobody checked is indistinguishable from one that
	// raises it.
	if in.checkedAt.IsZero() {
		return refuse(RefuseCommitmentUnchecked, in.ref,
			"step carries no %s: without a commitment inventory the net bill delta is unknown, and §7 trap 1 is exactly the case where the list-price saving is a fiction",
			AttrCommitmentCheckedAt)
	}
	if !now.IsZero() {
		if age := now.Sub(in.checkedAt); age > CommitmentMaxAge {
			return refuse(RefuseCommitmentUnchecked, in.ref,
				"the commitment inventory this net was computed against was fetched %s ago (limit %s); commitments expire on dates",
				age.Round(time.Hour), CommitmentMaxAge)
		} else if age < -time.Hour {
			return refuse(RefuseCommitmentUnchecked, in.ref,
				"the commitment check is dated %s, which is in the future",
				in.checkedAt.UTC().Format(time.RFC3339))
		}
	}
	if in.net > in.gross+1e-9 {
		return refuse(RefuseCommitmentUnchecked, in.ref,
			"step claims net %s > gross %s, which the commitment waterfall makes arithmetically impossible",
			fmtUSD(in.net), fmtUSD(in.gross))
	}
	if in.net <= 0 {
		return refuse(RefuseCommitmentNegative, in.ref,
			"net bill delta through the commitment waterfall is %s/mo (gross %s/mo): stopping this instance to resize it would not save money",
			fmtUSD(in.net), fmtUSD(in.gross))
	}

	// Trap 4: memory-blind downsizing. Same rule the sizer enforces in
	// report.go's validateProposal — never propose less memory than current
	// without a memory signal — re-checked here because a step reaches this
	// actuator through a plan store, an API and a controller restart, none of
	// which re-run the sizer.
	if in.toMem < in.fromMem && in.memorySignal != MemorySignalCWAgent {
		signal := in.memorySignal
		if signal == "" {
			signal = "(absent)"
		}
		return refuse(RefuseMemoryBlind, in.ref,
			"step cuts memory %s → %s with memory signal %s: CloudWatch publishes no EC2 memory metric without the CloudWatch agent, so there is no evidence this instance fits (§7 trap 4)",
			gib(in.fromMem), gib(in.toMem), signal)
	}

	return checkStorage(in, true)
}

// checkStorage is §3.3's "never resize storage down".
//
// strict additionally requires the declaration to be present and readable: a
// forward resize that cannot state its own storage is not auditable, so it is
// refused. An undo is held to the weaker rule — it may not SHRINK anything,
// but a missing declaration on the shape the workload already ran is not a
// reason to leave it stranded at the shape that broke it.
func checkStorage(in intent, strict bool) error {
	if strict && (in.fromStor < 0 || in.toStor < 0) {
		return refuse(RefuseStorageShrink, in.ref,
			"step does not declare %s on both sides (from %d, to %d); a resize that cannot state its storage is not auditable",
			AttrStorageGiB, in.fromStor, in.toStor)
	}
	if in.fromStor >= 0 && in.toStor >= 0 && in.toStor < in.fromStor {
		return refuse(RefuseStorageShrink, in.ref,
			"step reduces storage %d GiB → %d GiB; Kilter never shrinks storage (§3.3)",
			in.fromStor, in.toStor)
	}
	return nil
}

// facts is everything the read-only seam fetched about one instance.
type facts struct {
	live    InstanceDetail
	current InstanceTypeInfo
	target  InstanceTypeInfo
	image   ImageDetail
}

// checkOwnership is §3.3's "Never" list, evaluated against live tags rather
// than against whatever the plan recorded. Tags change; a plan built before an
// operator tagged an instance `kilter.dev/mode=off` must not run after.
func checkOwnership(in intent, live InstanceDetail) error {
	for _, t := range live.Tags {
		switch {
		case t.Key == TagEKSCluster || t.Key == TagAWSEKSCluster || strings.HasPrefix(t.Key, TagK8sClusterPrefix):
			return refuse(RefuseK8sTagged, in.ref,
				"instance carries %q: it is a Kubernetes node and belongs to the k8s-nodes domain, which sizes it against pod requests and eviction budgets this path cannot see",
				t.Key)
		case t.Key == TagKilterMode && strings.EqualFold(strings.TrimSpace(t.Value), "off"):
			return refuse(RefuseModeOff, in.ref, "instance carries %s=off", TagKilterMode)
		case t.Key == TagASGName:
			return refuse(RefuseASGMember, in.ref,
				"instance was launched by Auto Scaling group %q: its shape comes from a launch template, so a direct resize is undone by the next scale-out (§3.3 never touch ASG instances directly)",
				t.Value)
		}
	}
	return nil
}

// checkSafety is the data-loss and boot-prerequisite matrix. Every branch is a
// documented §3.3 refusal.
func checkSafety(in intent, f facts) error {
	live := f.live

	// --- data loss ---
	if live.InstanceStoreVolumes > 0 {
		return refuse(RefuseInstanceStore, in.ref,
			"instance has %d instance-store volume(s): stopping it destroys that data permanently (§3.3 never)",
			live.InstanceStoreVolumes)
	}
	for _, bd := range live.BlockDevices {
		if bd.Ephemeral() {
			return refuse(RefuseInstanceStore, in.ref,
				"block device %s is instance-store backed (%s): stopping this instance destroys that data permanently (§3.3 never)",
				bd.DeviceName, bd.VirtualName)
		}
	}
	if strings.EqualFold(live.RootDeviceType, RootDeviceInstanceStore) {
		return refuse(RefuseInstanceStore, in.ref,
			"root device is instance-store backed: the instance cannot be stopped without destroying it")
	}
	switch strings.ToLower(strings.TrimSpace(live.ShutdownBehavior)) {
	case ShutdownStop:
	case ShutdownTerminate:
		return refuse(RefuseShutdownBehavior, in.ref,
			"instance-initiated shutdown behavior is %q: a stop would terminate the instance (§3.3 never)",
			ShutdownTerminate)
	default:
		return refuse(RefuseShutdownBehavior, in.ref,
			"instance-initiated shutdown behavior could not be read (%q); on any doubt the step is advisory (§3.3)",
			live.ShutdownBehavior)
	}
	if live.HibernationConfigured {
		return refuse(RefuseHibernation, in.ref,
			"instance is hibernation-configured: its stop writes a RAM image bound to the current shape")
	}

	// --- what this unit never resizes ---
	if live.SpotInstanceRequestID != "" || strings.EqualFold(live.LifecycleType, "spot") {
		return refuse(RefuseSpot, in.ref,
			"instance is a Spot instance (%s): stopped Spot capacity is not held, so a resize can strand it",
			live.LifecycleType)
	}
	if tn := strings.ToLower(strings.TrimSpace(live.Tenancy)); tn != "" && tn != "default" {
		return refuse(RefuseTenancy, in.ref,
			"instance tenancy is %q; §3.3 forbids changing tenancy and a %s-tenancy resize is not this unit's call", tn, tn)
	}
	if f.target.BareMetal {
		return refuse(RefuseBareMetal, in.ref, "target %s is a bare-metal type", in.toType)
	}

	// --- boot prerequisites ---
	if f.image.ImageID == "" || (f.image.State != "" && !strings.EqualFold(f.image.State, "available")) {
		return refuse(RefuseImageMissing, in.ref,
			"AMI %q is not available (state %q): its ENA/NVMe prerequisites cannot be verified, so the instance is not stopped",
			live.ImageID, f.image.State)
	}
	liveArch := normalizeArch(live.Architecture)
	imgArch := normalizeArch(f.image.Architecture)
	if imgArch != "" && liveArch != "" && imgArch != liveArch {
		return refuse(RefuseArchMismatch, in.ref,
			"instance reports architecture %s but its AMI %s is %s; the account's own data disagrees with itself",
			liveArch, f.image.ImageID, imgArch)
	}
	if !supportsArch(f.target, liveArch) {
		return refuse(RefuseArchMismatch, in.ref,
			"target %s supports %s, the AMI is %s: the instance would not boot",
			in.toType, strings.Join(f.target.SupportedArchitectures, ","), liveArch)
	}
	if v := strings.TrimSpace(f.image.VirtualizationType); v != "" && !containsFold(f.target.SupportedVirtualizationTypes, v) {
		return refuse(RefuseVirtualization, in.ref,
			"AMI %s is %s-virtualized and target %s supports %s",
			f.image.ImageID, v, in.toType, strings.Join(f.target.SupportedVirtualizationTypes, ","))
	}
	if rd := strings.TrimSpace(live.RootDeviceType); rd != "" && len(f.target.SupportedRootDeviceTypes) > 0 &&
		!containsFold(f.target.SupportedRootDeviceTypes, rd) {
		return refuse(RefuseRootDevice, in.ref,
			"target %s supports root devices %s, this instance is %s",
			in.toType, strings.Join(f.target.SupportedRootDeviceTypes, ","), rd)
	}
	if err := checkENA(in, f); err != nil {
		return err
	}
	return checkNVMe(in, f)
}

// checkENA enforces the enhanced-networking prerequisite.
//
// A target whose ENA support is "required" needs BOTH the instance's own
// enaSupport flag and the AMI's, because AWS reads the AMI's flag at launch
// and the instance's flag while running. "supported" is not "enabled", and an
// empty string from DescribeInstanceTypes means the API did not say — which
// refuses, because guessing here produces an instance that stops and never
// comes back.
func checkENA(in intent, f facts) error {
	switch strings.ToLower(strings.TrimSpace(f.target.ENASupport)) {
	case SupportRequired:
		if !f.live.EnaSupport {
			return refuse(RefuseENAUnsupported, in.ref,
				"target %s requires enhanced networking (ENA) and the instance's enaSupport attribute is false",
				in.toType)
		}
		if !f.image.ENASupport {
			return refuse(RefuseENAUnsupported, in.ref,
				"target %s requires enhanced networking (ENA) and AMI %s does not declare enaSupport",
				in.toType, f.image.ImageID)
		}
		return nil
	case SupportSupported, SupportUnsupported:
		return nil
	default:
		return refuse(RefuseENAUnsupported, in.ref,
			"DescribeInstanceTypes reports no ENA support level for %s (%q); an unverified prerequisite is a refusal, not a default",
			in.toType, f.target.ENASupport)
	}
}

// checkNVMe enforces the NVMe prerequisite, and does it without inventing a
// signal that does not exist.
//
// No AWS API reports whether an AMI carries NVMe drivers. What IS observable
// is the pair (current type, running instance): if the instance is running
// right now on a type whose NVMe support is "required", then this AMI
// demonstrably boots with NVMe block devices — that is evidence, not
// inference. If the current type's NVMe support is anything else, nothing in
// the account says the AMI has the driver, and a move to an NVMe-required
// generation is refused.
//
// The cost is refusing some safe migrations (an Xen-era AMI that does have the
// driver). The alternative is a stopped instance that never boots again, and
// §3.3 is explicit about which way to err.
func checkNVMe(in intent, f facts) error {
	switch strings.ToLower(strings.TrimSpace(f.target.NVMeSupport)) {
	case SupportSupported, SupportUnsupported:
		return nil
	case SupportRequired:
		if strings.EqualFold(strings.TrimSpace(f.current.NVMeSupport), SupportRequired) {
			return nil
		}
		return refuse(RefuseNVMeUnsupported, in.ref,
			"target %s requires NVMe block devices; the instance runs on %s (NVMe %q), so nothing observable shows AMI %s carries the NVMe driver",
			in.toType, in.fromType, f.current.NVMeSupport, f.image.ImageID)
	default:
		return refuse(RefuseNVMeUnsupported, in.ref,
			"DescribeInstanceTypes reports no NVMe support level for %s (%q); an unverified prerequisite is a refusal, not a default",
			in.toType, f.target.NVMeSupport)
	}
}

// checkShapeMatchesCatalog makes the step's claimed shape agree with what AWS
// says the target type is. A plan that promises 8 GiB and a type that delivers
// 4 is how a memory-blind refusal gets bypassed by arithmetic.
func checkShapeMatchesCatalog(in intent, f facts) error {
	if f.target.MemoryMiB > 0 {
		want := f.target.MemoryMiB * 1024 * 1024
		if want != in.toMem {
			return refuse(RefuseBadStep, in.ref,
				"step says target %s has %s of memory; DescribeInstanceTypes says %s",
				in.toType, gib(in.toMem), gib(want))
		}
	}
	if f.target.VCPU > 0 {
		want := int64(f.target.VCPU) * 1000
		if want != in.toCPU {
			return refuse(RefuseBadStep, in.ref,
				"step says target %s has %d mCPU; DescribeInstanceTypes says %d",
				in.toType, in.toCPU, want)
		}
	}
	// Instance storage on the TARGET is the mirror of the instance-store rule:
	// moving onto a type with ephemeral disks is allowed (the disks start
	// empty), moving off one is not — but the source check already refused any
	// instance that has ephemeral storage, so this only guards the case where
	// the step's own storage arithmetic disagrees with the catalog.
	if f.current.InstanceStorageSupported && f.current.InstanceStorageTotalGB > 0 {
		return refuse(RefuseInstanceStore, in.ref,
			"current type %s carries %d GB of instance storage; stopping it destroys that data (§3.3 never)",
			in.fromType, f.current.InstanceStorageTotalGB)
	}
	return nil
}

func supportsArch(t InstanceTypeInfo, arch string) bool {
	if arch == "" {
		return false
	}
	for _, a := range t.SupportedArchitectures {
		if normalizeArch(a) == arch {
			return true
		}
	}
	return false
}

func containsFold(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}
