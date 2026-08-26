package ec2

// U7 — EC2 instance and Auto Scaling actuation, behind an approval token.
//
// # What is different about this file
//
// Everything Kilter shipped before U6 was advisory. U6 (pkg/ebs) added the
// first non-Kubernetes mutation, and it was an online, reversible-upward one:
// a ModifyVolume that degrades to a slower volume at worst. This unit stops
// running instances. A bug here is downtime or data loss, not a wrong number.
//
// So the shape below is pkg/ebs's actuator — same dry-run/apply symmetry, same
// per-key idempotency, same ledger, same "record From so Revert restores what
// was actually there" — with four things added that a stop-resize needs and a
// volume modification does not:
//
//  1. **A structural approval gate** (actuate_approve.go). The only method
//     that acts takes an [ApprovedStep], which no foreign package can build.
//  2. **A resumable multi-stage machine** (actuate_instance.go). Stop, modify
//     and start are three cloud transitions with two crash windows between
//     them. The stage is re-derived from the LIVE instance on every entry, so
//     a controller that restarts mid-resize resumes from what AWS actually
//     shows rather than from what a ledger remembers.
//  3. **A one-way economics gate.** Every gate that can say "this resize is
//     not worth doing" runs while the instance is still running. Once it is
//     stopped, the machine's only job is to get it running again — forward at
//     the new type, or rolled back to the old one. Nothing past the stop can
//     decide to give up and walk away, because that decision is exactly what
//     leaves an instance stopped and forgotten.
//  4. **Persist-before-mutate.** When a [ActuatorConfig.Persist] hook is
//     wired, the ledger is flushed BEFORE each mutating call, never after, so
//     the crash window contains "we may have stopped it" rather than "we
//     definitely stopped it and nobody knows".
//
// # No AWS SDK, no network, no clock
//
// Every cloud operation goes through the interfaces in actuate_api.go. The
// package imports no SDK and opens no socket; the decision path is pure and
// takes `now` from [ActuatorConfig.Now]. Tests run against
// [ActuateFixture], which is data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Mode selects whether the actuator mutates anything. Dry-run is the default
// and the two modes share one code path: dry-run runs the identical pre-flight
// and refuses for the identical reasons, so an apply can never do something a
// dry-run never showed.
type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
)

// Stage is where a stop-start resize has got to. It is DERIVED from the live
// instance, never remembered: the ledger records the last stage observed, but
// the machine re-reads AWS on every entry and believes that instead.
type Stage string

const (
	// StageReady: running, still the original type. Nothing has happened.
	StageReady Stage = "ready"
	// StageStopping: AWS is stopping it.
	StageStopping Stage = "stopping"
	// StageStopped: stopped, still the original type. The modify has not run.
	StageStopped Stage = "stopped"
	// StageModified: stopped, already the target type. Only the start remains.
	StageModified Stage = "modified"
	// StageStarting: AWS is starting it.
	StageStarting Stage = "starting"
	// StageRunning: running as the target type. Done.
	StageRunning Stage = "running"
	// StageGone: terminated or terminating. No step acts on it.
	StageGone Stage = "gone"
	// StageUnknown: the live shape matches neither From nor To. Drift.
	StageUnknown Stage = "unknown"
)

// Ledger entry statuses.
const (
	// StatusDryRun: the step passed every gate and was not issued.
	StatusDryRun = "dry-run"
	// StatusNoop: the instance already reads as the target.
	StatusNoop = "no-op"
	// StatusDone: the resize completed and the instance is running as To.
	StatusDone = "done"
	// StatusInFlight: a transition was issued and had not settled when the
	// poll budget ran out. NOT terminal — re-executing resumes.
	StatusInFlight = "in-flight"
	// StatusRolledBack: something failed after the instance was stopped and
	// the machine restored it to its ORIGINAL type, running. The step did not
	// succeed and must not be retried automatically; a human looks at it.
	StatusRolledBack = "rolled-back"
	// StatusRefused: a pre-flight predicate said no. Nothing was touched.
	StatusRefused = "refused"
	// StatusFailed: the step failed. Error says how. Not terminal.
	StatusFailed = "failed"
)

// Actuation errors that are not pre-flight refusals.
var (
	// ErrNoAutoScalingSeam: an ASG step was presented to an actuator wired
	// without an Auto Scaling seam. Not a refusal code — a wiring bug.
	ErrNoAutoScalingSeam = errors.New("ec2: no Auto Scaling seam is wired; this actuator cannot act on an ASG")
	// ErrPollTimeout: a transition was issued and had not settled in the poll
	// budget. The step is in flight, not failed.
	ErrPollTimeout = errors.New("ec2: transition still in progress when the poll budget ran out")
	// ErrStuck: the machine saw the same stage too many times to believe it is
	// making progress. Reported rather than looped on.
	ErrStuck = errors.New("ec2: the resize made no progress")
	// ErrIrreversible: the step's action class has no undo in this unit.
	ErrIrreversible = domain.ErrIrreversible
)

// Actuator defaults.
const (
	DefaultCallTimeout          = 30 * time.Second
	DefaultActuatePollInterval  = 15 * time.Second
	DefaultActuatePollTimeout   = 10 * time.Minute
	DefaultMinHealthyPercentage = 90
	DefaultInstanceWarmup       = 5 * time.Minute
	// maxStageVisits bounds the machine. Six stages, each visited at most
	// this often, so a flapping instance ends as a reported failure rather
	// than an infinite loop against a billed API.
	maxStageVisits = 3
)

// ActuatorConfig tunes the actuator.
type ActuatorConfig struct {
	// Mode defaults to ModeDryRun. An unknown value is rejected by
	// [NewActuator] rather than defaulted: everything past the constructor
	// trusts Mode, so a typo must fail there and not fall through into a stop.
	Mode Mode
	// Now is the clock. REQUIRED — this package reads no clock of its own, so
	// cmd/ passes time.Now and tests pass a fake.
	Now func() time.Time
	// CallTimeout bounds every individual cloud call.
	CallTimeout time.Duration
	// PollInterval and PollTimeout bound waiting for a transition to settle.
	PollInterval time.Duration
	PollTimeout  time.Duration
	// Sleep waits between polls. Zero means a context-aware timer; tests
	// inject one that advances their fake clock instead of spending time.
	Sleep func(ctx context.Context, d time.Duration) error
	// Persist, when set, is called with the serialized ledger BEFORE every
	// mutating cloud call and after every status change. It is the difference
	// between "a controller restart resumes" and "a controller restart
	// rediscovers". A Persist that returns an error ABORTS the mutation: an
	// unrecorded stop is the failure mode this unit exists to prevent.
	Persist func(ctx context.Context, ledger []byte) error
	// MinHealthyPercentage is the ASG instance-refresh floor. Zero means
	// [DefaultMinHealthyPercentage].
	MinHealthyPercentage int32
	// InstanceWarmup is how long a refreshed instance is given before it
	// counts as healthy. Zero means [DefaultInstanceWarmup].
	InstanceWarmup time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c ActuatorConfig) withDefaults() ActuatorConfig {
	if c.Mode == "" {
		c.Mode = ModeDryRun
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultActuatePollInterval
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = DefaultActuatePollTimeout
	}
	if c.Sleep == nil {
		c.Sleep = actuateSleep
	}
	if c.MinHealthyPercentage <= 0 {
		c.MinHealthyPercentage = DefaultMinHealthyPercentage
	}
	if c.InstanceWarmup <= 0 {
		c.InstanceWarmup = DefaultInstanceWarmup
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
// It carries no money, exactly as pkg/ebs's does and for the same reason: the
// claimed saving belongs to the recommendation that produced the step, and a
// second copy here would become a second source of truth for the bill. It does
// carry downtime, because downtime is a cost this unit alone can measure.
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
	// audit can answer "who said yes to this stop" from the ledger alone.
	Fingerprint string `json:"fingerprint,omitempty"`
	ApprovedBy  string `json:"approvedBy,omitempty"`

	// Revert marks an entry produced by [Actuator.Revert].
	Revert bool `json:"revert,omitempty"`
	// Attempts counts mutating calls issued for this key.
	Attempts int `json:"attempts"`
	// Polls counts state reads made for this key.
	Polls int `json:"polls,omitempty"`

	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// StoppedAt is when the instance was first observed stopped, and
	// RunningAt when it was first observed running again. Downtime is their
	// difference, recorded once and never recomputed.
	StoppedAt time.Time     `json:"stoppedAt,omitzero"`
	RunningAt time.Time     `json:"runningAt,omitzero"`
	Downtime  time.Duration `json:"downtimeNanos,omitempty"`

	// RefusalCode is the machine-readable pre-flight code, empty when the step
	// was not refused.
	RefusalCode string `json:"refusalCode,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`

	// --- Auto Scaling only ---

	// RefreshID is the instance refresh this step started or resumed.
	RefreshID string `json:"refreshId,omitempty"`
	// TemplateID names the launch template the step versioned.
	TemplateID string `json:"templateId,omitempty"`
	// PriorVersion is the launch template version the group ran before, and
	// PriorDefaultVersion the template's default at that moment. Both are the
	// undo: Revert points the template back at PriorDefaultVersion.
	PriorVersion        int64 `json:"priorVersion,omitempty"`
	PriorDefaultVersion int64 `json:"priorDefaultVersion,omitempty"`
	// NewVersion is the version this step created.
	NewVersion int64 `json:"newVersion,omitempty"`
	// RefreshStatus is the last status observed, so a rolled-back refresh is
	// visible as such rather than as a bare failure.
	RefreshStatus string `json:"refreshStatus,omitempty"`
}

// Terminal reports whether the entry represents work that must not be redone.
//
// Done and no-op are obvious. Rolled-back is terminal too, and deliberately:
// the machine already restored the instance to its original shape and running
// state, so there is nothing to resume — and retrying automatically would
// stop the instance again for a change that just failed. A human decides.
//
// A dry-run is NOT terminal: previewing and then applying is the normal
// sequence.
func (e LedgerEntry) Terminal() bool {
	return e.Status == StatusDone || e.Status == StatusNoop || e.Status == StatusRolledBack
}

// Settled reports whether the entry describes an instance in a safe resting
// state — running, or never touched. Its negation is the condition a resume
// must act on, and it is what the fuzz target asserts can always be reached.
func (e LedgerEntry) Settled() bool {
	switch e.Status {
	case StatusDone, StatusNoop, StatusRolledBack, StatusDryRun, StatusRefused:
		return true
	}
	return false
}

// Actuator executes approved EC2 resize steps. Safe for concurrent use.
type Actuator struct {
	api InstanceActuateAPI
	asg ASGActuateAPI
	cfg ActuatorConfig

	mu     sync.Mutex
	ledger map[string]*LedgerEntry
	order  []string
}

// NewActuator builds an actuator.
//
// api is the standalone-instance seam and is required. asg is the Auto Scaling
// seam and may be nil, in which case ASG steps are refused with
// [ErrNoAutoScalingSeam] rather than silently skipped — a controller wired
// without Auto Scaling permissions must say so, not appear to succeed.
func NewActuator(api InstanceActuateAPI, asg ASGActuateAPI, cfg ActuatorConfig) (*Actuator, error) {
	if api == nil {
		return nil, fmt.Errorf("ec2: actuator needs an instance seam")
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("ec2: actuator needs a clock (this package has none): pass ActuatorConfig.Now")
	}
	cfg = cfg.withDefaults()
	if cfg.Mode != ModeDryRun && cfg.Mode != ModeApply {
		return nil, fmt.Errorf("ec2: unknown mode %q", cfg.Mode)
	}
	if cfg.MinHealthyPercentage > 100 {
		return nil, fmt.Errorf("ec2: MinHealthyPercentage %d is above 100", cfg.MinHealthyPercentage)
	}
	return &Actuator{api: api, asg: asg, cfg: cfg, ledger: map[string]*LedgerEntry{}}, nil
}

// Mode reports whether this actuator mutates anything.
func (a *Actuator) Mode() Mode { return a.cfg.Mode }

// Domain reports the domain kind this actuator serves.
func (a *Actuator) Domain() domain.Kind { return Kind }

// CanActuateASG reports whether an Auto Scaling seam is wired.
func (a *Actuator) CanActuateASG() bool { return a.asg != nil }

// Execute performs one approved step.
//
// The step's action class selects the path: [domain.ActionStopStart] runs the
// standalone stop/modify/start machine, [domain.ActionRolling] runs the launch
// template + instance refresh path. Anything else is refused.
//
// Order of operations:
//
//  1. A step this actuator already finished returns immediately, with no
//     cloud call. Re-running a completed plan after a restart costs nothing.
//  2. The approval is re-checked against the clock. A plan takes minutes; an
//     approval that expired halfway through does not authorize the rest.
//  3. Everything else runs the same gates in dry-run and in apply.
func (a *Actuator) Execute(ctx context.Context, as ApprovedStep) error {
	return a.execute(ctx, as, false)
}

// Revert undoes a step by restoring its recorded From.
//
// It takes the ORIGINAL [ApprovedStep], not a separately approved inverse: the
// human who approved making this change is the authority for unmaking it, and
// requiring a fresh signature to undo would strand a workload at the size that
// broke it. [domain.Registry.Revert] takes the same position about guardrails
// for the same reason.
//
// The inverse step's key is recomputed from the swapped specs, so the undo has
// its own ledger entry and its own idempotency.
func (a *Actuator) Revert(ctx context.Context, as ApprovedStep) error {
	if !as.Approved() {
		return fmt.Errorf("%w: revert also requires the approval that authorized the step", ErrNotApproved)
	}
	step := as.Step()
	switch step.Action {
	case domain.ActionStopStart, domain.ActionRolling:
	default:
		return fmt.Errorf("%w: %q is not revertible by this actuator", ErrIrreversible, step.Action)
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
	return a.execute(ctx, rev, true)
}

func (a *Actuator) execute(ctx context.Context, as ApprovedStep, revert bool) error {
	now := a.cfg.Now()

	step := as.Step()
	if e, ok := a.entry(step.Key); ok && e.Terminal() {
		return nil
	}
	if err := as.check(now); err != nil {
		a.recordRefusal(step, as, revert, now, err)
		return err
	}
	switch step.Action {
	case domain.ActionStopStart:
		return a.executeInstance(ctx, as, revert, now)
	case domain.ActionRolling:
		if a.asg == nil {
			a.record(step, as, revert, now, StatusFailed, "", ErrNoAutoScalingSeam)
			return ErrNoAutoScalingSeam
		}
		return a.executeASG(ctx, as, revert, now)
	default:
		err := refuse(RefuseWrongAction, step.Target,
			"action %q: this actuator performs %q (standalone instance) and %q (Auto Scaling group) only",
			step.Action, domain.ActionStopStart, domain.ActionRolling)
		a.recordRefusal(step, as, revert, now, err)
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
	switch step.Action {
	case domain.ActionStopStart:
		in, err := decodeStep(step, domain.ActionStopStart)
		if err != nil {
			return err
		}
		if err := checkEconomics(in, now); err != nil {
			return err
		}
		f, err := a.instanceFacts(ctx, in)
		if err != nil {
			return err
		}
		return checkInstance(in, f)
	case domain.ActionRolling:
		if a.asg == nil {
			return ErrNoAutoScalingSeam
		}
		in, err := decodeStep(step, domain.ActionRolling)
		if err != nil {
			return err
		}
		if err := checkEconomics(in, now); err != nil {
			return err
		}
		_, err = a.asgFacts(ctx, in)
		return err
	default:
		return refuse(RefuseWrongAction, step.Target, "action %q is not actuated by this unit", step.Action)
	}
}

// call bounds one cloud operation with the configured timeout. Every mutating
// and reading call in this unit goes through it, so no single hung API call
// can hold a stopped instance hostage.
func (a *Actuator) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.cfg.CallTimeout)
}

// clientToken derives a deterministic idempotency token from the step key and
// the operation. AWS deduplicates on it, so a retry after a lost response
// cannot create a second launch template version.
func clientToken(key, op string) string {
	return "kilter-" + op + "-" + key
}

// --- ledger ----------------------------------------------------------------

func (a *Actuator) entry(key string) (LedgerEntry, bool) {
	if key == "" {
		return LedgerEntry{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return LedgerEntry{}, false
	}
	return *e, true
}

// Entry returns one step's ledger entry.
func (a *Actuator) Entry(key string) (LedgerEntry, bool) { return a.entry(key) }

// upsert returns the entry for a step, creating it on first sight. Caller
// holds a.mu.
func (a *Actuator) upsert(step domain.Step, as ApprovedStep, revert bool, now time.Time) *LedgerEntry {
	key := step.Key
	if key == "" {
		key = domain.StepKey(step.Target, step.From, step.To)
	}
	e, ok := a.ledger[key]
	if !ok {
		e = &LedgerEntry{
			Key: key, Target: step.Target, Action: step.Action,
			From: step.From, To: step.To, Revert: revert, StartedAt: now,
			Fingerprint: as.approval.token.Fingerprint,
			ApprovedBy:  as.approval.token.ApprovedBy,
		}
		a.ledger[key] = e
		a.order = append(a.order, key)
	}
	return e
}

// record writes (or updates) the entry for a step.
func (a *Actuator) record(step domain.Step, as ApprovedStep, revert bool, now time.Time, status, detail string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.upsert(step, as, revert, now)
	e.Mode = a.cfg.Mode
	e.Status = status
	if detail != "" {
		e.Detail = detail
	}
	if err != nil {
		e.Error = err.Error()
		if code := RefusalCode(err); code != "" {
			e.RefusalCode = code
		}
	} else {
		e.Error, e.RefusalCode = "", ""
	}
	if status != StatusInFlight {
		e.FinishedAt = now
	}
}

// recordRefusal records a refusal, which by definition touched nothing.
func (a *Actuator) recordRefusal(step domain.Step, as ApprovedStep, revert bool, now time.Time, err error) {
	a.record(step, as, revert, now, StatusRefused, "", err)
}

// mutate is the persist-before-act barrier.
//
// It records the intent, flushes the ledger through [ActuatorConfig.Persist],
// and only then lets the caller issue the cloud call. A Persist failure aborts
// the mutation: a stop nobody wrote down is precisely the state this unit must
// never reach, so failing to record is failing to act.
func (a *Actuator) mutate(ctx context.Context, key string, now time.Time, stage Stage, detail string) error {
	a.mu.Lock()
	e, ok := a.ledger[key]
	if ok {
		e.Mode = a.cfg.Mode
		e.Status = StatusInFlight
		e.Stage = stage
		e.Detail = detail
		e.Attempts++
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("ec2: no ledger entry for %s: refusing to mutate an unrecorded step", key)
	}
	return a.persist(ctx)
}

// persist flushes the ledger when a hook is wired.
func (a *Actuator) persist(ctx context.Context) error {
	if a.cfg.Persist == nil {
		return nil
	}
	b, err := a.LedgerJSON()
	if err != nil {
		return fmt.Errorf("ec2: serialize ledger: %w", err)
	}
	if err := a.cfg.Persist(ctx, b); err != nil {
		return fmt.Errorf("ec2: persist ledger before mutating: %w", err)
	}
	return nil
}

// observe updates the stage and the downtime bookkeeping from a live read.
func (a *Actuator) observe(key string, stage Stage, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return
	}
	e.Stage = stage
	e.Polls++
	switch stage {
	case StageStopped, StageModified:
		if e.StoppedAt.IsZero() {
			e.StoppedAt = now
		}
	case StageRunning, StageReady:
		if !e.StoppedAt.IsZero() && e.RunningAt.IsZero() {
			e.RunningAt = now
			e.Downtime = now.Sub(e.StoppedAt)
		}
	}
}

// finish closes an entry out.
func (a *Actuator) finish(key, status string, stage Stage, now time.Time, detail string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return
	}
	e.Status = status
	if stage != "" {
		e.Stage = stage
	}
	if detail != "" {
		e.Detail = detail
	}
	e.FinishedAt = now
	if err != nil {
		e.Error = err.Error()
		if code := RefusalCode(err); code != "" {
			e.RefusalCode = code
		}
	} else {
		e.Error, e.RefusalCode = "", ""
	}
}

// Ledger returns every recorded entry, ordered by first sight — deterministic
// for a given step sequence and independent of map iteration.
func (a *Actuator) Ledger() []LedgerEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]LedgerEntry, 0, len(a.order))
	for _, k := range a.order {
		if e, ok := a.ledger[k]; ok {
			out = append(out, *e)
		}
	}
	return out
}

// LedgerJSON serializes the ledger for pkg/store. Entries are emitted in key
// order so the bytes are stable across processes.
func (a *Actuator) LedgerJSON() ([]byte, error) {
	entries := a.Ledger()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return json.Marshal(entries)
}

// RestoreLedger reloads a serialized ledger, so a restarted controller knows
// which steps it already finished — and, more importantly, which ones it
// started and has not.
func (a *Actuator) RestoreLedger(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var entries []LedgerEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("ec2: restore ledger: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ledger = make(map[string]*LedgerEntry, len(entries))
	a.order = a.order[:0]
	for i := range entries {
		e := entries[i]
		if e.Key == "" {
			continue
		}
		if _, dup := a.ledger[e.Key]; dup {
			continue
		}
		a.ledger[e.Key] = &e
		a.order = append(a.order, e.Key)
	}
	return nil
}

// Unsettled returns the entries describing work that is neither finished nor
// safely at rest, in key order. A controller calls this on startup: every key
// it returns is an instance that may be stopped right now, and re-executing
// its step is what brings it back.
func (a *Actuator) Unsettled() []LedgerEntry {
	entries := a.Ledger()
	out := make([]LedgerEntry, 0, len(entries))
	for _, e := range entries {
		if !e.Settled() {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// --- aggregates -------------------------------------------------------------

// LedgerSummary is the roll-up a report renders.
//
// PR#27 shipped a real non-determinism bug because totals were summed in
// arrival order. Everything here is summed after a sort on an intrinsic key,
// and TestSummaryIsShuffleInvariant shuffles the input and compares bytes.
// Downtime is an int64 count of nanoseconds, so its addition is exact and
// order could not change the total even unsorted — the sort is here so that
// the entry ORDER in the rendered summary is stable too, and so that a future
// float field cannot reintroduce the bug by being added to a function that
// never sorted.
type LedgerSummary struct {
	Entries int `json:"entries"`
	// ByStatus and ByRefusal are ordered by descending count, then by code.
	ByStatus  []domain.CodeCount `json:"byStatus,omitempty"`
	ByRefusal []domain.CodeCount `json:"byRefusal,omitempty"`
	// TotalDowntime is the sum of every recorded downtime, summed in key
	// order.
	TotalDowntime time.Duration `json:"totalDowntimeNanos,omitempty"`
	// LongestDowntime and LongestKey name the worst single outage. Ties break
	// on the key, so the answer is total.
	LongestDowntime time.Duration `json:"longestDowntimeNanos,omitempty"`
	LongestKey      string        `json:"longestKey,omitempty"`
	// Stopped counts entries observed with the instance stopped and not yet
	// running again. It is the number an operator must never see grow.
	Stopped int `json:"stopped"`
}

// Summarize rolls a ledger up deterministically.
func Summarize(entries []LedgerEntry) LedgerSummary {
	sorted := make([]LedgerEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	out := LedgerSummary{Entries: len(sorted)}
	statuses := make([]string, 0, len(sorted))
	refusals := make([]string, 0, len(sorted))
	for _, e := range sorted {
		if e.Status != "" {
			statuses = append(statuses, e.Status)
		}
		if e.RefusalCode != "" {
			refusals = append(refusals, e.RefusalCode)
		}
		out.TotalDowntime += e.Downtime
		if e.Downtime > out.LongestDowntime {
			out.LongestDowntime, out.LongestKey = e.Downtime, e.Key
		}
		if !e.StoppedAt.IsZero() && e.RunningAt.IsZero() {
			out.Stopped++
		}
	}
	out.ByStatus = tally(statuses)
	out.ByRefusal = tally(refusals)
	return out
}

// tally counts codes into a canonically ordered slice: descending count, then
// code. It never ranges over a map on an output path without sorting after.
func tally(codes []string) []domain.CodeCount {
	if len(codes) == 0 {
		return nil
	}
	counts := make(map[string]int, len(codes))
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			counts[c]++
		}
	}
	out := make([]domain.CodeCount, 0, len(counts))
	for c, n := range counts {
		out = append(out, domain.CodeCount{Code: c, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// --- the registrable form ---------------------------------------------------

// BoundActuator is an [Actuator] with an approval already attached. It is the
// ONLY form that satisfies [domain.Actuator], and therefore the only form
// [domain.Registry.RegisterActuator] will take.
//
// That is the structural half of the approval gate seen from the wiring side:
// cmd/ cannot register a bare actuator and let the registry drive it, because
// a bare actuator has no Execute(ctx, Step) method to satisfy the interface.
// It must first obtain an approval — which requires a token, which requires a
// human — and bind it to a specific plan fingerprint.
type BoundActuator struct {
	a  *Actuator
	ap Approval
}

// Bind attaches an approval, producing the registrable form.
func (a *Actuator) Bind(ap Approval) (*BoundActuator, error) {
	if !ap.Valid() {
		return nil, fmt.Errorf("%w: Bind needs an approval from NewApproval", ErrNotApproved)
	}
	return &BoundActuator{a: a, ap: ap}, nil
}

// Domain implements domain.Actuator.
func (b *BoundActuator) Domain() domain.Kind { return Kind }

// Fingerprint is the plan this actuator is bound to.
func (b *BoundActuator) Fingerprint() string { return b.ap.Fingerprint() }

// Execute implements domain.Actuator. A step the bound approval does not cover
// is refused here, so binding one plan does not authorize another.
func (b *BoundActuator) Execute(ctx context.Context, step domain.Step) error {
	as, err := b.ap.Authorize(step)
	if err != nil {
		b.a.recordRefusal(step, ApprovedStep{}, false, b.a.cfg.Now(), err)
		return err
	}
	return b.a.Execute(ctx, as)
}

// Revert implements domain.Actuator.
func (b *BoundActuator) Revert(ctx context.Context, step domain.Step) error {
	as, err := b.ap.Authorize(step)
	if err != nil {
		return err
	}
	return b.a.Revert(ctx, as)
}

// Ledger exposes the underlying actuator's ledger.
func (b *BoundActuator) Ledger() []LedgerEntry { return b.a.Ledger() }

var _ domain.Actuator = (*BoundActuator)(nil)
