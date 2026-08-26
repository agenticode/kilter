package rds

// U14 — RDS storage-performance actuation, behind an approval token.
//
// # What is different about this file
//
// Everything in pkg/rds before this file was read-only, and said so
// structurally: TestNoActuationSurfaceExists asserts that *Domain cannot
// satisfy domain.Actuator, and TestNoMutatingAPISurface asserts that the
// identifier `ModifyDBInstance` appears nowhere in this package's code. Both
// still pass. What has changed is that a SECOND type in this package — one
// that no report path can reach and that no caller can construct without an
// approval token — can now send three storage arguments to a production
// database. ACTUATE-FINDINGS.md §2 states that plainly for the next reviewer,
// because a test whose name says "no actuation surface exists" must not be
// allowed to mean something it no longer means.
//
// # The shape, and where it comes from
//
// This is pkg/ec2's actuator (U7) — mode default, structural approval token,
// step ledger, recorded From, persist-before-mutate, resumability — with
// pkg/ebs's asynchronous-modification polling (U6), because an RDS storage
// modification is EBS's ModifyVolume seen from a managed service: the API
// returns immediately, the instance walks
// available → modifying → storage-optimization → available over minutes to
// hours, and the change is only real at the end of that walk.
//
// Three things are specific to RDS and none of them are optional:
//
//  1. **Four modifications per 24 hours, and unknown BLOCKS.** There is no API
//     that reports the count; it is inferred from rds:DescribeEvents. An
//     unread history is refused (FINDINGS.md §5.3), and a step refused for
//     that reason carries the ClearsAt as its ValidFrom so a scheduler can
//     retry it rather than re-derive it.
//  2. **storage-optimization locks the instance for hours after the change.**
//     Which means the poll budget usually runs out before the instance is
//     back to available, and that is NOT a failure — it is StatusInFlight, and
//     re-executing resumes the observation without issuing a second call.
//  3. **The ratchet only turns one way.** actuate_preflight.go §2.
//
// # No AWS SDK, no network, no clock
//
// Every cloud operation goes through [StorageActuateAPI]. The package imports
// no SDK and opens no socket; the decision path is pure and takes `now` from
// [ActuatorConfig.Now]. Tests run against [StorageActuateFixture], which is
// data.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Mode selects whether the actuator mutates anything. Dry-run is the default
// and the two modes share ONE code path: dry-run runs the identical pre-flight
// and refuses for the identical reasons, so an apply can never do something a
// dry-run never showed.
type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
)

// Stage is how far a storage modification has got. It is DERIVED from the live
// instance on every entry, never remembered: the ledger records the last stage
// observed, but the machine re-reads AWS and believes that instead. This is
// what makes a controller restart resume from what is true rather than from
// what was written down before the crash.
type Stage string

const (
	// StageReady: available, still the recorded From. Nothing has been sent.
	StageReady Stage = "ready"
	// StageAccepted: RDS has taken the modification and not started it —
	// PendingModifiedValues names our target and the status is still
	// available. This is the stage a lost response lands in, and observing it
	// is what stops a retry from spending a second modification.
	StageAccepted Stage = "accepted"
	// StageModifying: status is `modifying`.
	StageModifying Stage = "modifying"
	// StageOptimizing: status is `storage-optimization`. The new
	// configuration is in effect and AWS is still redistributing behind it.
	// The instance is fully usable here and no further modification is.
	StageOptimizing Stage = "storage-optimization"
	// StageDone: available, reads as the target, nothing pending.
	StageDone Stage = "done"
	// StageGone: the instance is not in the account.
	StageGone Stage = "gone"
	// StageDrift: the live shape matches neither From nor To.
	StageDrift Stage = "drift"
)

// Ledger entry statuses.
const (
	// StatusDryRun: the step passed every gate and was not issued.
	StatusDryRun = "dry-run"
	// StatusNoop: the instance already reads as the target.
	StatusNoop = "no-op"
	// StatusDone: the modification completed and the instance is available at
	// the target configuration.
	StatusDone = "done"
	// StatusInFlight: the modification was issued (or was already running)
	// and had not settled when the poll budget ran out. NOT terminal, NOT a
	// failure — re-executing resumes the observation.
	StatusInFlight = "in-flight"
	// StatusRefused: a pre-flight predicate said no. Nothing was touched.
	StatusRefused = "refused"
	// StatusFailed: the step failed. Error says how. Not terminal.
	StatusFailed = "failed"
)

// Actuation errors that are not pre-flight refusals.
const (
	// ErrPollTimeout: the modification is still running. The step is in
	// flight, not failed. An RDS storage modification routinely outlives any
	// sane poll budget, so this is the EXPECTED outcome of a successful
	// apply, not an exceptional one.
	ErrPollTimeout actuateError = "rds: storage modification still in progress when the poll budget ran out"
	// ErrInstanceVanished: the instance stopped existing mid-modification.
	ErrInstanceVanished actuateError = "rds: DB instance disappeared during the modification"
	// ErrDriftDuringModification: the live instance stopped matching either
	// the recorded From or the intended To while the machine was watching.
	ErrDriftDuringModification actuateError = "rds: DB instance drifted away from the plan during the modification"
	// ErrNoLedgerEntry: a mutation was attempted for a step with no ledger
	// entry. A modification nobody wrote down is the state this unit must
	// never reach.
	ErrNoLedgerEntry actuateError = "rds: refusing to modify an unrecorded step"
)

// ErrIrreversible is [domain.ErrIrreversible], returned when a step's action
// class has no undo in this unit. It is a function rather than a var because
// TestNoUnexpectedPackageState forbids package-level state here; callers use
// errors.Is(err, domain.ErrIrreversible) either way.
func ErrIrreversible() error { return domain.ErrIrreversible }

// Actuator defaults.
const (
	DefaultActuateCallTimeout  = 30 * time.Second
	DefaultActuatePollInterval = 30 * time.Second
	// DefaultActuatePollTimeout is deliberately short relative to how long a
	// storage modification takes. The honest posture is "issue it, watch it
	// for a while, record it as in-flight and come back", not "block a
	// controller for six hours pretending to be synchronous".
	DefaultActuatePollTimeout = 15 * time.Minute
	// maxStageVisits bounds the machine so a flapping instance ends as a
	// reported result rather than an infinite loop against a billed API.
	maxStageVisits = 64
)

// ActuatorConfig tunes the actuator.
type ActuatorConfig struct {
	// Mode defaults to [ModeDryRun]. An unknown value is REJECTED by
	// [NewActuator] rather than defaulted: everything past the constructor
	// trusts Mode, so a typo must fail there and not fall through into a
	// modification.
	Mode Mode
	// Now is the clock. REQUIRED — this package reads no clock of its own
	// (TestNoClockReads), so cmd/ passes time.Now and tests pass a fake.
	Now func() time.Time
	// CallTimeout bounds every individual cloud call.
	CallTimeout time.Duration
	// PollInterval and PollTimeout bound waiting for a modification to settle.
	PollInterval time.Duration
	PollTimeout  time.Duration
	// EventWindow is how much rds:DescribeEvents history to read when
	// re-checking the cooldown. Zero means [StorageModificationWindow]. A
	// shorter window cannot answer the four-per-24-hours question, so
	// [NewActuator] refuses one.
	EventWindow time.Duration
	// Sleep waits between polls. Zero means a context-aware timer; tests
	// inject one that advances their fake clock instead of spending time.
	Sleep func(ctx context.Context, d time.Duration) error
	// Persist, when set, is called with the serialized ledger BEFORE every
	// mutating cloud call and after every status change. It is the difference
	// between "a controller restart resumes" and "a controller restart
	// rediscovers". A Persist that returns an error ABORTS the mutation.
	Persist func(ctx context.Context, ledger []byte) error
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c ActuatorConfig) withDefaults() ActuatorConfig {
	if c.Mode == "" {
		c.Mode = ModeDryRun
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultActuateCallTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultActuatePollInterval
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = DefaultActuatePollTimeout
	}
	if c.EventWindow <= 0 {
		c.EventWindow = StorageModificationWindow
	}
	if c.Sleep == nil {
		c.Sleep = actuateSleep
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// actuateSleep waits d, or returns early when the context ends. It uses a
// timer rather than a clock read, so the package still has no time.Now.
func actuateSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// LedgerEntry is one recorded execution attempt.
//
// It carries the claimed monthly saving only when the step attested one (see
// [AttrNetSavingsMonthlyUSD]), and it is never recomputed here: the claim
// belongs to the assessment that produced the step, and a second arithmetic
// would become a second source of truth for the bill.
type LedgerEntry struct {
	Key    string             `json:"key"`
	Target domain.TargetRef   `json:"target"`
	Action domain.ActionClass `json:"action"`
	From   domain.Spec        `json:"from"`
	To     domain.Spec        `json:"to"`
	Mode   Mode               `json:"mode"`
	Status string             `json:"status"`
	Stage  Stage              `json:"stage,omitempty"`

	// Fingerprint and ApprovedBy record WHICH approval authorized this, so an
	// audit can answer "who said yes to modifying this database" from the
	// ledger alone.
	Fingerprint string `json:"fingerprint,omitempty"`
	ApprovedBy  string `json:"approvedBy,omitempty"`

	// Revert marks an entry produced by [Actuator.Revert], and Origin names
	// the forward step it undoes.
	Revert bool   `json:"revert,omitempty"`
	Origin string `json:"origin,omitempty"`

	// Sent records the exact call this step made, so an audit can read what
	// was sent rather than infer it from the specs. It is set in dry-run too,
	// which is what makes a dry-run a preview instead of a promise.
	Sent ModifyStorageInput `json:"sent,omitzero"`

	// Attempts counts MUTATING calls issued for this key. It must never
	// exceed one for a step that completes: the four-per-24-hours limit makes
	// a spurious retry expensive in a way a retry against most APIs is not.
	Attempts int `json:"attempts"`
	// Polls counts live state reads made for this key.
	Polls int `json:"polls,omitempty"`

	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// IssuedAt is when the modification was accepted by RDS. It is the start
	// of this instance's 24-hour window and the number an operator needs to
	// know when the next modification becomes possible.
	IssuedAt time.Time `json:"issuedAt,omitzero"`

	// ClaimedMonthlyUSD is the step's attested net monthly saving, carried
	// verbatim and never recomputed. Absent attestation reads as no claim.
	ClaimedMonthlyUSD float64 `json:"claimedMonthlyUSD,omitempty"`
	Claimed           bool    `json:"claimed,omitempty"`

	// RefusalCode is the machine-readable pre-flight code, empty when the
	// step was not refused.
	RefusalCode string `json:"refusalCode,omitempty"`
	// ValidFrom is when a dated refusal lapses — for a cooldown, the moment
	// the oldest of the four modifications leaves the window
	// (FINDINGS.md §5.3). A scheduler retries at this time instead of
	// re-deriving it.
	ValidFrom time.Time `json:"validFrom,omitzero"`
	Detail    string    `json:"detail,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Terminal reports whether the entry represents work that must not be redone.
//
// A dry-run is NOT terminal: previewing and then applying is the normal
// sequence. A refusal is not terminal either — the fact that refused it
// (a cooldown, an unstable state) is usually one that clears on its own.
func (e LedgerEntry) Terminal() bool {
	return e.Status == StatusDone || e.Status == StatusNoop
}

// Settled reports whether the entry describes a step that needs no further
// action. Its negation is what a controller must re-execute on startup: an
// in-flight entry is a modification that may still be running against a
// production database with nobody watching it.
func (e LedgerEntry) Settled() bool {
	switch e.Status {
	case StatusDone, StatusNoop, StatusDryRun:
		return true
	case StatusRefused:
		// A refusal BEFORE anything was issued touched nothing, so there is
		// nothing to come back to. A refusal AFTER a modification was issued
		// is different in the way that matters: that modification is still
		// running against a production database, and an entry that drops off
		// [Actuator.Unsettled] is one nobody will look at again.
		return e.IssuedAt.IsZero() && e.Attempts == 0
	}
	return false
}

// Actuator executes approved RDS storage-performance steps. Safe for
// concurrent use.
type Actuator struct {
	api StorageActuateAPI
	cfg ActuatorConfig

	mu     sync.Mutex
	ledger map[string]*LedgerEntry
	order  []string
}

// NewActuator builds an actuator.
//
// api is required and is the mutating seam; there is no constructor that
// produces an actuator without one, because an actuator that cannot act is a
// [Preflight] call and this type would be a confusing way to spell it.
func NewActuator(api StorageActuateAPI, cfg ActuatorConfig) (*Actuator, error) {
	if api == nil {
		return nil, fmt.Errorf("rds: actuator needs a storage seam")
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("rds: actuator needs a clock (this package has none): pass ActuatorConfig.Now")
	}
	if cfg.EventWindow < 0 || (cfg.EventWindow > 0 && cfg.EventWindow < StorageModificationWindow) {
		return nil, fmt.Errorf(
			"rds: EventWindow %s is shorter than the %s modification window; a history that short cannot "+
				"answer the four-per-24-hours question and this unit will not guess at it",
			cfg.EventWindow, StorageModificationWindow)
	}
	cfg = cfg.withDefaults()
	if cfg.Mode != ModeDryRun && cfg.Mode != ModeApply {
		return nil, fmt.Errorf("rds: unknown mode %q", cfg.Mode)
	}
	return &Actuator{api: api, cfg: cfg, ledger: map[string]*LedgerEntry{}}, nil
}

// Mode reports whether this actuator mutates anything.
func (a *Actuator) Mode() Mode { return a.cfg.Mode }

// Domain reports the domain kind this actuator serves.
func (a *Actuator) Domain() domain.Kind { return Kind }

// --- execution --------------------------------------------------------------

// Execute performs one approved step.
//
// Order of operations, and why each one is where it is:
//
//  1. A step this actuator already finished returns immediately, with no cloud
//     call. Re-running a completed plan after a restart costs nothing.
//  2. The approval is re-checked against the clock. A storage modification and
//     its optimization phase take hours; an approval that expired halfway
//     through does not authorize the rest.
//  3. The step is decoded. Nothing past this point wonders whether a field was
//     set.
//  4. The live facts are read: instance state, envelope, event history. All
//     three, every time, however recent the plan is.
//  5. The pure pre-flight runs, identically in dry-run and in apply.
//  6. Dry-run stops here having recorded the EXACT call apply would make.
//  7. Apply issues one modification and then OBSERVES rather than assumes.
func (a *Actuator) Execute(ctx context.Context, as ApprovedStep) error {
	return a.execute(ctx, as)
}

// Revert undoes a step by restoring its recorded From.
//
// It takes the ORIGINAL [ApprovedStep], not a separately approved inverse: the
// human who approved making this change is the authority for unmaking it, and
// requiring a fresh signature to undo would strand a database at the
// configuration that broke it.
//
// Two things are true of a revert here and both are stated in
// ACTUATE-FINDINGS.md §4 rather than hidden:
//
//   - The revert CONSUMES one of the four storage modifications this instance
//     is allowed in 24 hours. A change and its undo are two of four. A change,
//     an undo and a retry are three, and there is no fourth chance to get it
//     right that day.
//   - A revert can never be talked below the regime baseline. It restores
//     [ParityPlan.Current], whose IOPS and throughput are already floored at
//     the baseline, and the identical pre-flight — including the ratchet and
//     the live envelope — runs against the inverse step.
func (a *Actuator) Revert(ctx context.Context, as ApprovedStep) error {
	if !as.Approved() {
		return fmt.Errorf("%w: revert also requires the approval that authorized the step", ErrNotApproved)
	}
	step := as.Step()
	if step.Action != domain.ActionInPlace {
		return fmt.Errorf("%w: %q is not revertible by this actuator", domain.ErrIrreversible, step.Action)
	}
	inv := domain.Step{
		Seq:    step.Seq,
		Target: step.Target,
		Action: step.Action,
		From:   step.To,
		To:     step.From,
		Risk:   step.Risk,
		Detail: "revert of " + step.Key,
	}
	inv.Key = domain.StepKey(inv.Target, inv.From, inv.To)
	// The inverse is authorized by the same approval, so it is constructed
	// here rather than by Authorize — which would (correctly) refuse a key the
	// approved plan never contained.
	rev := ApprovedStep{step: inv, approval: as.approval, authorized: true, origin: step.Key, undo: true}
	return a.execute(ctx, rev)
}

func (a *Actuator) execute(ctx context.Context, as ApprovedStep) error {
	now := a.cfg.Now()
	step := as.Step()

	// (1) Already finished. No cloud call, no second modification, no error.
	if e, ok := a.entry(step.Key); ok && e.Terminal() {
		return nil
	}
	// (2) The approval, re-checked against the clock.
	if err := as.check(now); err != nil {
		a.record(step, as, now, StatusRefused, "", err)
		return err
	}
	// (3) The step's own shape.
	in, err := decodeStep(step, as.undo, as.origin)
	if err != nil {
		a.record(step, as, now, StatusRefused, "", err)
		return err
	}
	// (4) The live facts — all of them, every time.
	f, err := a.facts(ctx, in, now)
	if err != nil {
		status := StatusFailed
		if IsRefusal(err) {
			status = StatusRefused
		}
		a.record(step, as, now, status, "", err)
		return err
	}
	// (5) The pure pre-flight, identical in both modes.
	if err := checkStorage(in, f, now); err != nil {
		a.record(step, as, now, StatusRefused, "", err)
		return err
	}

	stage := stageOf(in, f)
	if stage == StageDone {
		a.record(step, as, now, StatusNoop,
			fmt.Sprintf("%s already reads as %s at %d IOPS / %d MiB/s",
				in.ref.ID, in.toType, f.want.IOPS, f.want.ThroughputMBps), nil)
		return nil
	}

	call := callFor(in, f)
	detail := describeCall(call, f)
	// (6) Dry-run stops here, having recorded the exact call apply would make.
	if a.cfg.Mode == ModeDryRun {
		a.recordCall(step, as, now, StatusDryRun, stage, detail, call, nil)
		return nil
	}

	// (7) Apply. A modification RDS has already accepted is resumed, never
	// re-issued: a duplicate would spend a second of the four this instance
	// gets in 24 hours. StageReady is the ONLY stage that issues, and it is
	// by construction the only one in which nothing is pending.
	if stage == StageReady && !inFlightTowardTarget(in, f) {
		if err := a.mutate(ctx, step, as, now, stage, detail, call); err != nil {
			a.record(step, as, now, StatusFailed, detail, err)
			return err
		}
		cctx, cancel := a.call(ctx)
		_, err := a.api.ModifyStorage(cctx, &call)
		cancel()
		if err != nil {
			err = fmt.Errorf("rds: modify storage for %s: %w", in.ref.ID, err)
			// The response was lost or the call failed; which one is not
			// knowable from here. The entry stays non-terminal, so the next
			// execution re-reads PendingModifiedValues and finds out.
			a.finish(step.Key, StatusFailed, stage, a.cfg.Now(), detail, err)
			return err
		}
		a.markIssued(step.Key, a.cfg.Now())
	} else {
		a.setDetail(step.Key, "resume: "+detail)
	}

	// Observe rather than assume.
	final, polls, err := a.poll(ctx, in, f)
	fin := a.cfg.Now()
	a.addPolls(step.Key, polls)
	switch {
	case err == nil:
		a.finish(step.Key, StatusDone, final, fin, detail, nil)
		a.cfg.Logger.Info("rds storage modified",
			"instance", in.ref.ID, "storageType", call.StorageType,
			"iops", call.IOPS, "storageThroughput", call.StorageThroughputMBps)
		return nil
	case errors.Is(err, ErrPollTimeout):
		// NOT a failure. storage-optimization outlives any sane poll budget.
		a.finish(step.Key, StatusInFlight, final, fin, detail, err)
		return err
	default:
		a.finish(step.Key, StatusFailed, final, fin, detail, err)
		return err
	}
}

// Preflight runs every read-only gate for a step and reports the refusal, if
// any, WITHOUT an approval and without touching anything.
//
// This is the path a report, a `--dry-run` and a UI use. It needs no approval
// precisely because it cannot act: separating "may I look?" from "may I act?"
// is what lets the approval gate stay absolute without making the tool opaque.
func (a *Actuator) Preflight(ctx context.Context, step domain.Step) error {
	now := a.cfg.Now()
	in, err := decodeStep(step, false, "")
	if err != nil {
		return err
	}
	f, err := a.facts(ctx, in, now)
	if err != nil {
		return err
	}
	return checkStorage(in, f, now)
}

// PlannedCall returns the exact call [Actuator.Execute] would issue for a
// step, or the refusal that stops it. It is how cmd/ renders a plan a human is
// about to approve: the three arguments, and nothing else.
func (a *Actuator) PlannedCall(ctx context.Context, step domain.Step) (ModifyStorageInput, error) {
	now := a.cfg.Now()
	in, err := decodeStep(step, false, "")
	if err != nil {
		return ModifyStorageInput{}, err
	}
	f, err := a.facts(ctx, in, now)
	if err != nil {
		return ModifyStorageInput{}, err
	}
	if err := checkStorage(in, f, now); err != nil {
		return ModifyStorageInput{}, err
	}
	return callFor(in, f), nil
}

// --- the live reads ---------------------------------------------------------

// facts reads everything the pre-flight needs, live. All three reads happen on
// every execution: FINDINGS.md §5.2 and §5.4 make re-reading mandatory, and a
// cache here would be a way to make them optional by accident.
func (a *Actuator) facts(ctx context.Context, in storageIntent, now time.Time) (storageFacts, error) {
	var f storageFacts

	cctx, cancel := a.call(ctx)
	out, err := a.api.DescribeInstanceState(cctx, &DescribeInstanceStateInput{DBInstanceIdentifier: in.ref.ID})
	cancel()
	if err != nil {
		return f, fmt.Errorf("rds: describe %s: %w", in.ref.ID, err)
	}
	if out == nil || !out.Found {
		return f, refuse(RefuseInstanceMissing, in.ref,
			"%s is not in this account. A plan that names an instance nobody can describe is stale, and "+
				"an actuator that treats \"not found\" as \"try again\" is one that eventually finds "+
				"something else with the same name", in.ref.ID)
	}
	f.live = out.Instance

	// The envelope and the modification history, re-read through the SAME
	// collector U13 used, so the two units cannot disagree about what the
	// seam said.
	ec := NewEnvelopeCollector(a.api, EnvelopeCollectorConfig{
		Window: Window{Start: now.Add(-a.cfg.EventWindow), End: now},
	})
	ectx, ecancel := a.call(ctx)
	envs, err := ec.Collect(ectx, []string{in.ref.ID})
	ecancel()
	if err != nil {
		return f, fmt.Errorf("rds: read modification envelope for %s: %w", in.ref.ID, err)
	}
	f.env = envs.Get(in.ref.ID)
	f.cool = f.env.Cooldown(now)

	f.regime = GP3RegimeFor(in.engine, in.allocGiB)
	f.liveCfg = configOf(f.regime, in.allocGiB, f.live.IOPS, f.live.StorageThroughputMBps)
	f.want = configOf(f.regime, in.allocGiB, in.toIOPS, in.toTput)
	f.from = configOf(f.regime, in.allocGiB, in.fromIOPS, in.fromTput)
	return f, nil
}

// stageOf derives the stage from the LIVE instance. Nothing here consults the
// ledger: that is the whole point.
func stageOf(in storageIntent, f storageFacts) Stage {
	switch strings.ToLower(strings.TrimSpace(f.live.Status)) {
	case StatusModifying:
		return StageModifying
	case StatusStorageOptimization:
		return StageOptimizing
	}
	atTarget := f.live.NormalizedStorageType() == in.toType &&
		f.liveCfg.IOPS == f.want.IOPS && f.liveCfg.ThroughputMBps == f.want.ThroughputMBps
	if atTarget {
		if f.live.PendingStorageChange() {
			return StageAccepted
		}
		return StageDone
	}
	if f.live.PendingStorageChange() {
		return StageAccepted
	}
	if f.live.NormalizedStorageType() == in.fromType &&
		f.liveCfg.IOPS == f.from.IOPS && f.liveCfg.ThroughputMBps == f.from.ThroughputMBps {
		return StageReady
	}
	return StageDrift
}

// callFor builds the exact modification. It is the ONLY place a
// [ModifyStorageInput] is constructed, and it takes its provisioning
// arguments from [argumentsFor] — the same function the pre-flight's
// baseline check used, so the thing checked and the thing sent are the same
// thing by construction rather than by agreement.
func callFor(in storageIntent, f storageFacts) ModifyStorageInput {
	iops, tput := argumentsFor(f.regime, f.want)
	return ModifyStorageInput{
		DBInstanceIdentifier:  in.ref.ID,
		ClientToken:           clientToken(in.key),
		StorageType:           in.toType,
		IOPS:                  iops,
		StorageThroughputMBps: tput,
	}
}

// describeCall renders the call for the ledger and the dry-run preview, naming
// the arguments that will be OMITTED as well as the ones that will be sent —
// an operator reading a preview needs to see that the baseline is not being
// bought, not infer it from an absence.
func describeCall(c ModifyStorageInput, f storageFacts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "modify %s: --storage-type %s", c.DBInstanceIdentifier, c.StorageType)
	if c.IOPS > 0 {
		fmt.Fprintf(&b, " --iops %d", c.IOPS)
	} else {
		fmt.Fprintf(&b, " (--iops omitted: %d IOPS is the free %s baseline)",
			f.regime.BaselineIOPS, regimeName(f.regime))
	}
	if c.StorageThroughputMBps > 0 {
		fmt.Fprintf(&b, " --storage-throughput %d", c.StorageThroughputMBps)
	} else {
		fmt.Fprintf(&b, " (--storage-throughput omitted: %d MiB/s is the free %s baseline)",
			f.regime.BaselineThroughputMBps, regimeName(f.regime))
	}
	return b.String()
}

// clientToken derives a deterministic idempotency identity from the step key.
// See [ModifyStorageInput.ClientToken] for what AWS does and does not do with
// it.
func clientToken(key string) string { return "kilter-rds-storage-" + key }

// call bounds one cloud operation with the configured timeout. Every mutating
// and reading call in this unit goes through it, so no single hung API call
// can hold a half-modified database hostage.
func (a *Actuator) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.cfg.CallTimeout)
}

// poll observes the modification to a terminal stage.
//
// It re-derives the stage from a fresh read every iteration and never trusts
// its own previous answer, so an interruption at ANY stage boundary is
// resumable: a restarted controller enters here with whatever AWS shows and
// carries on from there.
func (a *Actuator) poll(ctx context.Context, in storageIntent, f0 storageFacts) (Stage, int, error) {
	deadline := a.cfg.Now().Add(a.cfg.PollTimeout)
	stage := stageOf(in, f0)
	polls := 0
	for visits := 0; visits < maxStageVisits; visits++ {
		cctx, cancel := a.call(ctx)
		out, err := a.api.DescribeInstanceState(cctx, &DescribeInstanceStateInput{DBInstanceIdentifier: in.ref.ID})
		cancel()
		polls++
		if err != nil {
			return stage, polls, fmt.Errorf("rds: describe %s while observing: %w", in.ref.ID, err)
		}
		if out == nil || !out.Found {
			return StageGone, polls, fmt.Errorf("%w: %s", ErrInstanceVanished, in.ref.ID)
		}
		f := f0
		f.live = out.Instance
		f.liveCfg = configOf(f.regime, in.allocGiB, f.live.IOPS, f.live.StorageThroughputMBps)
		stage = stageOf(in, f)
		switch stage {
		case StageDone:
			return stage, polls, nil
		case StageDrift:
			return stage, polls, fmt.Errorf("%w: %s is %s at %d IOPS / %d MiB/s",
				ErrDriftDuringModification, in.ref.ID, orNone(f.live.NormalizedStorageType()),
				f.liveCfg.IOPS, f.liveCfg.ThroughputMBps)
		}
		if !a.cfg.Now().Before(deadline) {
			break
		}
		if err := a.cfg.Sleep(ctx, a.cfg.PollInterval); err != nil {
			return stage, polls, err
		}
		if !a.cfg.Now().Before(deadline) {
			break
		}
	}
	return stage, polls, fmt.Errorf("%w: %s is at stage %q", ErrPollTimeout, in.ref.ID, stage)
}
