package ebs

// The ModifyVolume actuator: Kilter's first non-Kubernetes execution path.
//
// # Discipline, copied from pkg/actuate
//
//   - Dry-run unless explicitly constructed in apply mode, and dry-run and
//     apply are SYMMETRIC: they run the identical pre-flight and refuse for
//     the identical reasons. The only difference is that apply issues the call
//     and waits for it. An apply path that can do something dry-run never
//     showed is the bug class that already bit pkg/actuate once, so the two
//     modes share one code path here rather than two that agree by convention.
//   - Idempotent per Step.Key: a step whose volume already reads as the target
//     is a no-op, and a step this actuator already completed is a no-op
//     without touching the cloud at all.
//   - Every step records From, so Revert restores what was actually there
//     rather than a guess.
//   - A failure fails loudly and is recorded; nothing is assumed.
//
// # Why polling
//
// ModifyVolume is asynchronous: the API returns immediately and the volume
// walks modifying → optimizing → completed over minutes to hours. Returning
// success on the API call alone would let a plan report a change that has not
// happened. So Execute polls to a terminal state, and a poll that runs out of
// budget is reported as in-flight (not as success, not as failure) with a
// ledger entry that lets a later run RESUME the same step instead of issuing a
// second modification.

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

// ModifyVolumeInput is the write seam's request. Zero-valued fields mean
// "leave unchanged", exactly as the API defines it — which is why this unit
// never sets SizeGiB: EBS size can only grow, and growing a volume is not
// rightsizing.
type ModifyVolumeInput struct {
	VolumeID       string `json:"volumeId"`
	VolumeType     string `json:"volumeType,omitempty"`
	SizeGiB        int64  `json:"sizeGiB,omitempty"`
	IOPS           int32  `json:"iops,omitempty"`
	ThroughputMBps int32  `json:"throughputMBps,omitempty"`
}

// ModifyVolumeOutput is the write seam's response.
type ModifyVolumeOutput struct {
	Modification VolumeModification `json:"modification"`
}

// ModifyAPI is the actuation seam: the read operations plus the one mutation
// this unit is allowed to perform. It is deliberately NOT satisfied by
// [InventoryAPI], so a read-only wiring cannot be passed where a mutating one
// is expected.
type ModifyAPI interface {
	InventoryAPI
	ModifyVolume(ctx context.Context, in *ModifyVolumeInput) (*ModifyVolumeOutput, error)
}

// Mode selects whether the actuator mutates anything.
type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
)

// Ledger entry statuses.
const (
	// StatusDryRun: the step passed every pre-flight check and was not issued.
	StatusDryRun = "dry-run"
	// StatusNoop: the volume already reads as the target.
	StatusNoop = "no-op"
	// StatusDone: the modification was issued and reached a terminal state.
	StatusDone = "done"
	// StatusInFlight: issued, still modifying when the poll budget ran out.
	// Re-executing the same step resumes polling; it never re-issues.
	StatusInFlight = "in-flight"
	// StatusFailed: refused or failed. Error says which.
	StatusFailed = "failed"
)

// Actuation errors. Callers distinguish them with errors.Is.
var (
	// ErrWrongAction: the step is not an in-place volume modification.
	ErrWrongAction = errors.New("ebs: step is not an in-place volume modification")
	// ErrStepKeyMismatch: the step's idempotency key does not hash its own
	// contents. Either the plan was edited after approval or two different
	// changes share a key; both are refusals, never guesses.
	ErrStepKeyMismatch = errors.New("ebs: step key does not match its contents")
	// ErrBadStep: the step's From/To specs are not a modification this unit
	// performs.
	ErrBadStep = errors.New("ebs: step is not a gp2/gp3 modification this unit performs")
	// ErrVolumeMissing: the volume is not in the account any more.
	ErrVolumeMissing = errors.New("ebs: volume not found")
	// ErrDrift: the live volume matches neither the recorded From nor the
	// intended To. Somebody else changed it; the plan is stale.
	ErrDrift = errors.New("ebs: volume drifted from the recorded From state")
	// ErrVolumeState: the volume's state or attachment forbids modification.
	ErrVolumeState = errors.New("ebs: volume state forbids modification")
	// ErrModificationInProgress: a different modification is already running.
	ErrModificationInProgress = errors.New("ebs: another modification is already in progress")
	// ErrCooldown: the per-volume modification cooldown has not elapsed.
	ErrCooldown = errors.New("ebs: volume is in its modification cooldown")
	// ErrModificationFailed: AWS reported the modification failed.
	ErrModificationFailed = errors.New("ebs: modification failed")
	// ErrPollTimeout: the modification is still running; the step is in
	// flight, not done.
	ErrPollTimeout = errors.New("ebs: modification still in progress when the poll budget ran out")
	// ErrRevertWouldDegrade: the recorded From delivers LESS than the volume
	// delivers now. An undo that degrades performance is not an undo.
	ErrRevertWouldDegrade = errors.New("ebs: reverting would deliver less than the volume delivers now")
)

// Actuator defaults.
const (
	DefaultPollInterval = 30 * time.Second
	DefaultPollTimeout  = 15 * time.Minute
)

// ActuatorConfig tunes the actuator.
type ActuatorConfig struct {
	// Mode defaults to ModeDryRun. Any other unknown value is rejected by
	// NewActuator rather than defaulted: everything past the constructor
	// trusts Mode blindly, so a typo must fail there, not fall through the
	// dry-run check and modify a live volume.
	Mode Mode
	// Now is the clock. It is REQUIRED: this package reads no clock of its
	// own, so cmd/ passes time.Now and tests pass a fake.
	Now func() time.Time
	// Cooldown is the minimum gap between modifications of one volume. Zero
	// means DefaultCooldown.
	Cooldown time.Duration
	// PollInterval and PollTimeout bound progress polling.
	PollInterval time.Duration
	PollTimeout  time.Duration
	// Sleep waits between polls. Zero means a context-aware timer. Tests
	// inject one that advances their fake clock instead.
	Sleep func(ctx context.Context, d time.Duration) error
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c ActuatorConfig) withDefaults() ActuatorConfig {
	if c.Mode == "" {
		c.Mode = ModeDryRun
	}
	if c.Cooldown <= 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = DefaultPollTimeout
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// sleepCtx waits d, or returns early if the context ends. It uses a timer
// rather than a clock read, so the package still has no time.Now.
func sleepCtx(ctx context.Context, d time.Duration) error {
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
// It carries no money on purpose. The claimed saving belongs to the
// recommendation that produced the step (net, post-commitment); duplicating a
// number here would create a second source of truth for the bill, and the
// second one would be list price. Join by Key when a later unit corroborates
// claimed against measured.
type LedgerEntry struct {
	Key    string             `json:"key"`
	Target domain.TargetRef   `json:"target"`
	Action domain.ActionClass `json:"action"`
	From   domain.Spec        `json:"from"`
	To     domain.Spec        `json:"to"`
	Mode   Mode               `json:"mode"`
	Status string             `json:"status"`
	// Revert marks an entry produced by [Actuator.Revert].
	Revert bool `json:"revert,omitempty"`
	// Attempts counts Execute calls that reached the cloud for this key.
	Attempts   int       `json:"attempts"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// Polls counts modification-state reads made for this key.
	Polls  int    `json:"polls,omitempty"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Terminal reports whether the entry represents work that must not be redone.
// A dry-run is NOT terminal: previewing a step and then applying it is the
// normal sequence.
func (e LedgerEntry) Terminal() bool {
	return e.Status == StatusDone || e.Status == StatusNoop
}

// Actuator executes volume modifications. Safe for concurrent use.
type Actuator struct {
	api ModifyAPI
	cfg ActuatorConfig

	mu     sync.Mutex
	ledger map[string]*LedgerEntry
	order  []string
}

// NewActuator builds an actuator.
func NewActuator(api ModifyAPI, cfg ActuatorConfig) (*Actuator, error) {
	if api == nil {
		return nil, fmt.Errorf("ebs: actuator needs a ModifyVolume seam")
	}
	if cfg.Now == nil {
		return nil, fmt.Errorf("ebs: actuator needs a clock (this package has none): pass ActuatorConfig.Now")
	}
	cfg = cfg.withDefaults()
	if cfg.Mode != ModeDryRun && cfg.Mode != ModeApply {
		return nil, fmt.Errorf("ebs: unknown mode %q", cfg.Mode)
	}
	return &Actuator{api: api, cfg: cfg, ledger: map[string]*LedgerEntry{}}, nil
}

// Domain implements domain.Actuator.
func (a *Actuator) Domain() domain.Kind { return Kind }

// Mode reports whether this actuator mutates anything.
func (a *Actuator) Mode() Mode { return a.cfg.Mode }

// volumeConfig is the part of a volume that a modification changes. Comparing
// whole Specs would be wrong: a Spec also carries state, attachment and
// modification attributes that move on their own, and a step must not read as
// drift because a volume was detached.
type volumeConfig struct {
	Type    string
	SizeGiB int64
	IOPS    int32
	Tput    int32
}

func configOfSpec(s domain.Spec) volumeConfig {
	return volumeConfig{
		Type:    strings.ToLower(s.Attr(AttrVolumeType)),
		SizeGiB: sizeOf(s),
		IOPS:    intAttr(s, AttrIOPS),
		Tput:    intAttr(s, AttrThroughputMBps),
	}
}

func configOfVolume(v VolumeRecord) volumeConfig {
	return volumeConfig{
		Type:    strings.ToLower(v.VolumeType),
		SizeGiB: v.SizeGiB,
		IOPS:    v.IOPS,
		Tput:    v.ThroughputMBps,
	}
}

func (c volumeConfig) String() string {
	out := fmt.Sprintf("%s %d GiB", c.Type, c.SizeGiB)
	if c.IOPS > 0 {
		out += fmt.Sprintf(" %d IOPS", c.IOPS)
	}
	if c.Tput > 0 {
		out += fmt.Sprintf(" %d MiB/s", c.Tput)
	}
	return out
}

// matches compares two configurations for equality, ignoring fields the type
// does not provision. gp2 reports a size-derived IOPS number that AWS may
// round differently than this package models, so gp2 identity is (type, size).
func (c volumeConfig) matches(o volumeConfig) bool {
	if c.Type != o.Type || c.SizeGiB != o.SizeGiB {
		return false
	}
	if c.Type == VolumeTypeGP3 {
		return c.IOPS == o.IOPS && c.Tput == o.Tput
	}
	return true
}

// delivered is the performance a configuration actually provides: provisioned
// for gp3, size-derived baseline for gp2. Revert compares with it.
func (c volumeConfig) delivered() Demand {
	switch c.Type {
	case VolumeTypeGP3:
		return Demand{IOPS: float64(c.IOPS), ThroughputMBps: float64(c.Tput)}
	case VolumeTypeGP2:
		p := GP2PerformanceFor(c.SizeGiB)
		return Demand{IOPS: float64(p.BaselineIOPS), ThroughputMBps: p.BaselineThroughputMBps}
	}
	return Demand{}
}

// preflight is everything both modes do. It reads the cloud, decides what
// would happen, and refuses identically in dry-run and in apply.
type preflight struct {
	live     VolumeRecord
	from, to volumeConfig
	// atTarget: the volume already reads as To.
	atTarget bool
	// resume: a modification toward To is already running, so Execute polls it
	// instead of issuing a second one.
	resume bool
	input  ModifyVolumeInput
}

// Execute implements domain.Actuator.
//
// Order of operations, and why:
//
//  1. A step this actuator already finished returns immediately, without a
//     cloud call. That is the resumable-plan contract: re-running a completed
//     plan after a controller restart must cost nothing and change nothing.
//  2. Everything else runs the same pre-flight in both modes.
//  3. A volume that already reads as the target is a no-op — even inside its
//     cooldown, because doing nothing is always allowed.
//  4. Dry-run stops here, having recorded exactly the call apply would make.
func (a *Actuator) Execute(ctx context.Context, step domain.Step) error {
	return a.execute(ctx, step, false)
}

// Revert undoes a step by restoring its recorded From.
//
// "Revert upward": for every step this unit plans, From is the gp2 volume and
// To is a gp3 configuration provisioned to measured demand, so restoring From
// RAISES delivered performance (a 4 TiB gp2 sustains 12,000 IOPS; the gp3 it
// became may be provisioned for 4,000). A revert that would LOWER delivered
// performance is refused with [ErrRevertWouldDegrade]: that can only happen
// when the live volume is not what the step left behind, and quietly degrading
// somebody else's volume is not an undo.
//
// The cooldown applies to reverts too. AWS enforces it, so a revert inside the
// window is reported honestly as [ErrCooldown] rather than attempted and lost.
func (a *Actuator) Revert(ctx context.Context, step domain.Step) error {
	if step.Action != domain.ActionInPlace {
		return fmt.Errorf("%w: %q is not revertible by this actuator", domain.ErrIrreversible, step.Action)
	}
	rev := domain.Step{
		Seq:    step.Seq,
		Target: step.Target,
		Action: step.Action,
		From:   step.To,
		To:     step.From,
		Risk:   step.Risk,
		Detail: "revert of " + step.Key,
	}
	rev.Key = domain.StepKey(rev.Target, rev.From, rev.To)
	return a.execute(ctx, rev, true)
}

func (a *Actuator) execute(ctx context.Context, step domain.Step, revert bool) error {
	now := a.cfg.Now()

	if e, ok := a.entry(step.Key); ok && e.Terminal() {
		// Already done. No cloud call, no second modification, no error.
		return nil
	}

	pf, err := a.check(ctx, step, revert, now)
	if err != nil {
		a.record(step, revert, now, StatusFailed, "", err)
		return err
	}
	if pf.atTarget {
		a.record(step, revert, now, StatusNoop,
			fmt.Sprintf("volume %s already reads as %s", step.Target.ID, pf.to), nil)
		return nil
	}

	detail := fmt.Sprintf("ModifyVolume(%s: %s → %s)", step.Target.ID, pf.from, pf.to)
	if pf.resume {
		detail = fmt.Sprintf("resume in-flight modification of %s → %s", step.Target.ID, pf.to)
	}
	if a.cfg.Mode == ModeDryRun {
		a.record(step, revert, now, StatusDryRun, detail, nil)
		return nil
	}

	if !pf.resume {
		if _, err := a.api.ModifyVolume(ctx, &pf.input); err != nil {
			err = fmt.Errorf("ebs: modify %s: %w", step.Target.ID, err)
			a.record(step, revert, now, StatusFailed, detail, err)
			return err
		}
	}
	a.bumpAttempt(step, revert, now, detail)

	polls, err := a.poll(ctx, step.Target.ID, pf.to)
	fin := a.cfg.Now()
	switch {
	case err == nil:
		a.finish(step.Key, StatusDone, fin, polls, nil)
		a.cfg.Logger.Info("ebs volume modified",
			"volume", step.Target.ID, "from", pf.from.String(), "to", pf.to.String())
		return nil
	case errors.Is(err, ErrPollTimeout):
		// Not a failure: the modification is running. The entry stays
		// non-terminal so a later Execute resumes polling instead of issuing
		// a second modification.
		a.finish(step.Key, StatusInFlight, fin, polls, err)
		return err
	default:
		a.finish(step.Key, StatusFailed, fin, polls, err)
		return err
	}
}

// check runs the pre-flight. It is shared verbatim by dry-run and apply.
func (a *Actuator) check(ctx context.Context, step domain.Step, revert bool, now time.Time) (preflight, error) {
	var pf preflight
	if step.Action != domain.ActionInPlace {
		return pf, fmt.Errorf("%w: %q", ErrWrongAction, step.Action)
	}
	if step.Target.ID == "" {
		return pf, fmt.Errorf("%w: step has no target volume", ErrBadStep)
	}
	if want := domain.StepKey(step.Target, step.From, step.To); step.Key != "" && step.Key != want {
		return pf, fmt.Errorf("%w: step claims %q, contents hash to %q", ErrStepKeyMismatch, step.Key, want)
	}
	pf.from, pf.to = configOfSpec(step.From), configOfSpec(step.To)
	if err := validTransition(pf.from, pf.to); err != nil {
		return pf, err
	}

	live, err := a.describeOne(ctx, step.Target.ID)
	if err != nil {
		return pf, err
	}
	pf.live = live
	liveCfg := configOfVolume(live)

	if revert {
		// The upward rule, checked against what is LIVE rather than against
		// the step, so a volume somebody else already changed cannot be
		// silently downgraded by an undo.
		d, l := pf.to.delivered(), liveCfg.delivered()
		if d.IOPS < l.IOPS-eps || d.ThroughputMBps < l.ThroughputMBps-eps {
			return pf, fmt.Errorf("%w: restoring %s delivers %.0f IOPS / %.0f MiB/s against the live %s at %.0f IOPS / %.0f MiB/s",
				ErrRevertWouldDegrade, pf.to, d.IOPS, d.ThroughputMBps, liveCfg, l.IOPS, l.ThroughputMBps)
		}
	}

	switch {
	case liveCfg.matches(pf.to):
		pf.atTarget = true
		return pf, nil
	case !liveCfg.matches(pf.from):
		return pf, fmt.Errorf("%w: live volume is %s, the plan recorded %s", ErrDrift, liveCfg, pf.from)
	}

	if err := modifiable(live); err != nil {
		return pf, err
	}

	mod, err := a.modificationOf(ctx, step.Target.ID)
	if err != nil {
		return pf, err
	}
	if mod != nil {
		if mod.InFlight() {
			if targetConfigOf(*mod).matches(pf.to) {
				pf.resume = true
				return pf, nil
			}
			return pf, fmt.Errorf("%w: volume %s is %s toward %s",
				ErrModificationInProgress, step.Target.ID, mod.ModificationState, targetConfigOf(*mod))
		}
		if at := mod.At(); !at.IsZero() {
			if wait := a.cfg.Cooldown - now.Sub(at); wait > 0 {
				return pf, fmt.Errorf("%w: last modified %s ago, %s of the %s cooldown remain",
					ErrCooldown, now.Sub(at).Round(time.Minute), wait.Round(time.Minute), a.cfg.Cooldown)
			}
		}
	}

	pf.input = ModifyVolumeInput{VolumeID: step.Target.ID, VolumeType: pf.to.Type}
	if pf.to.Type == VolumeTypeGP3 {
		pf.input.IOPS, pf.input.ThroughputMBps = pf.to.IOPS, pf.to.Tput
	}
	return pf, nil
}

// validTransition rejects anything that is not a gp2 ⇄ gp3 modification of a
// volume whose size does not change.
func validTransition(from, to volumeConfig) error {
	if from.Type != VolumeTypeGP2 && from.Type != VolumeTypeGP3 {
		return fmt.Errorf("%w: from-type %q", ErrBadStep, from.Type)
	}
	if to.Type != VolumeTypeGP2 && to.Type != VolumeTypeGP3 {
		return fmt.Errorf("%w: to-type %q", ErrBadStep, to.Type)
	}
	if from.SizeGiB <= 0 || from.SizeGiB != to.SizeGiB {
		return fmt.Errorf("%w: size %d GiB → %d GiB (this unit never resizes a volume)",
			ErrBadStep, from.SizeGiB, to.SizeGiB)
	}
	if from.matches(to) {
		return fmt.Errorf("%w: from and to are the same configuration (%s)", ErrBadStep, from)
	}
	if to.Type == VolumeTypeGP3 {
		cfg := GP3Config{SizeGiB: to.SizeGiB, IOPS: to.IOPS, ThroughputMBps: to.Tput}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrBadStep, err)
		}
	} else {
		// gp2 provisions nothing. A gp2 spec legitimately carries the
		// size-derived baseline DescribeVolumes reports — a revert's target is
		// exactly that — but any other number was invented, and provisioned
		// throughput on gp2 does not exist. The request never carries either.
		if want := GP2PerformanceFor(to.SizeGiB).BaselineIOPS; to.Tput != 0 || (to.IOPS != 0 && to.IOPS != want) {
			return fmt.Errorf("%w: gp2 target carries provisioned IOPS/throughput (%d/%d); a %d GiB gp2 volume delivers %d IOPS and provisions no throughput",
				ErrBadStep, to.IOPS, to.Tput, to.SizeGiB, want)
		}
	}
	return nil
}

// modifiable applies the state rules ModifyVolume enforces.
func modifiable(v VolumeRecord) error {
	switch strings.ToLower(v.State) {
	case VolumeStateAvailable, VolumeStateInUse:
	case "":
		return fmt.Errorf("%w: volume state is unknown", ErrVolumeState)
	default:
		return fmt.Errorf("%w: volume is %s, needs %s or %s", ErrVolumeState, v.State,
			VolumeStateAvailable, VolumeStateInUse)
	}
	if v.MultiAttachEnabled {
		return fmt.Errorf("%w: multi-attach is enabled and gp3 does not support it", ErrVolumeState)
	}
	for _, at := range v.Attachments {
		switch strings.ToLower(at.State) {
		case AttachmentAttaching, AttachmentDetaching:
			return fmt.Errorf("%w: attachment to %s is %s", ErrVolumeState, at.InstanceID, at.State)
		}
	}
	return nil
}

// targetConfigOf reads a modification record's target configuration.
func targetConfigOf(m VolumeModification) volumeConfig {
	return volumeConfig{
		Type:    strings.ToLower(m.TargetVolumeType),
		SizeGiB: m.TargetSizeGiB,
		IOPS:    m.TargetIOPS,
		Tput:    m.TargetThroughputMBps,
	}
}

// describeOne reads one volume. The seam pages; a single-volume read still
// walks pages because a fixture (and a throttled API) may split anything.
func (a *Actuator) describeOne(ctx context.Context, volumeID string) (VolumeRecord, error) {
	token := ""
	seen := map[string]bool{}
	for page := 0; page < DefaultMaxPages; page++ {
		res, err := a.api.DescribeVolumes(ctx, &DescribeVolumesInput{NextToken: token})
		if err != nil {
			return VolumeRecord{}, fmt.Errorf("ebs: describe %s: %w", volumeID, err)
		}
		if res == nil {
			return VolumeRecord{}, fmt.Errorf("%w: %s (empty response)", ErrVolumeMissing, volumeID)
		}
		for _, v := range res.Volumes {
			if v.VolumeID == volumeID {
				return v, nil
			}
		}
		if res.NextToken == "" || seen[res.NextToken] {
			break
		}
		seen[res.NextToken] = true
		token = res.NextToken
	}
	return VolumeRecord{}, fmt.Errorf("%w: %s", ErrVolumeMissing, volumeID)
}

// modificationOf reads the newest modification record for a volume, or nil.
func (a *Actuator) modificationOf(ctx context.Context, volumeID string) (*VolumeModification, error) {
	token := ""
	seen := map[string]bool{}
	var out *VolumeModification
	for page := 0; page < DefaultMaxPages; page++ {
		res, err := a.api.DescribeVolumesModifications(ctx,
			&DescribeVolumesModificationsInput{VolumeIDs: []string{volumeID}, NextToken: token})
		if err != nil {
			return nil, fmt.Errorf("ebs: describe modifications for %s: %w", volumeID, err)
		}
		if res == nil {
			return nil, nil
		}
		for i := range res.Modifications {
			m := res.Modifications[i]
			if m.VolumeID != volumeID {
				continue
			}
			if out == nil || m.InFlight() || (!out.InFlight() && m.At().After(out.At())) {
				cp := m
				out = &cp
			}
		}
		if res.NextToken == "" || seen[res.NextToken] {
			break
		}
		seen[res.NextToken] = true
		token = res.NextToken
	}
	return out, nil
}

// poll waits for the modification to reach a terminal state.
//
// "optimizing" counts as terminal-good: AWS has applied the new type and the
// volume delivers the new performance while it finishes moving blocks in the
// background. Waiting for "completed" can take hours and would hold a step
// open for a change that has already landed.
func (a *Actuator) poll(ctx context.Context, volumeID string, want volumeConfig) (int, error) {
	attempts := int(a.cfg.PollTimeout / a.cfg.PollInterval)
	if attempts < 1 {
		attempts = 1
	}
	polls := 0
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return polls, err
		}
		mod, err := a.modificationOf(ctx, volumeID)
		polls++
		if err != nil {
			return polls, err
		}
		if mod != nil {
			switch mod.ModificationState {
			case ModificationCompleted, ModificationOptimizing:
				return polls, nil
			case ModificationFailed:
				return polls, fmt.Errorf("%w: %s: %s", ErrModificationFailed, volumeID, mod.StatusMessage)
			}
		} else {
			// No record: some paths complete before one appears. Trust the
			// volume itself, never the absence of evidence.
			if live, err := a.describeOne(ctx, volumeID); err == nil && configOfVolume(live).matches(want) {
				return polls, nil
			}
		}
		if i+1 >= attempts {
			return polls, fmt.Errorf("%w: %s after %d poll(s)", ErrPollTimeout, volumeID, polls)
		}
		if err := a.cfg.Sleep(ctx, a.cfg.PollInterval); err != nil {
			return polls, err
		}
	}
}

// --- ledger ---------------------------------------------------------------

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

// record writes (or updates) the entry for a step.
func (a *Actuator) record(step domain.Step, revert bool, now time.Time, status, detail string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.upsert(step, revert, now)
	e.Mode = a.cfg.Mode
	e.Status = status
	if detail != "" {
		e.Detail = detail
	}
	if err != nil {
		e.Error = err.Error()
	} else {
		e.Error = ""
	}
	if status != StatusInFlight {
		e.FinishedAt = now
	}
}

// bumpAttempt records that a cloud mutation was issued for this step.
func (a *Actuator) bumpAttempt(step domain.Step, revert bool, now time.Time, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.upsert(step, revert, now)
	e.Mode = a.cfg.Mode
	e.Status = StatusInFlight
	e.Detail = detail
	e.Attempts++
}

func (a *Actuator) finish(key, status string, now time.Time, polls int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return
	}
	e.Status = status
	e.Polls += polls
	e.FinishedAt = now
	if err != nil {
		e.Error = err.Error()
	} else {
		e.Error = ""
	}
}

// upsert returns the entry for a step, creating it on first sight. Caller
// holds a.mu.
func (a *Actuator) upsert(step domain.Step, revert bool, now time.Time) *LedgerEntry {
	key := step.Key
	if key == "" {
		key = domain.StepKey(step.Target, step.From, step.To)
	}
	e, ok := a.ledger[key]
	if !ok {
		e = &LedgerEntry{
			Key: key, Target: step.Target, Action: step.Action,
			From: step.From, To: step.To, Revert: revert, StartedAt: now,
		}
		a.ledger[key] = e
		a.order = append(a.order, key)
	}
	return e
}

// Ledger returns every recorded entry, ordered by first sight. Order is
// insertion order, which is deterministic for a given step sequence and does
// not depend on map iteration.
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

// Entry returns one step's ledger entry.
func (a *Actuator) Entry(key string) (LedgerEntry, bool) { return a.entry(key) }

// LedgerJSON serializes the ledger for pkg/store. Entries are emitted in key
// order so the bytes are stable across processes.
func (a *Actuator) LedgerJSON() ([]byte, error) {
	entries := a.Ledger()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return json.Marshal(entries)
}

// RestoreLedger reloads a serialized ledger, so a restarted controller knows
// which steps it already finished.
func (a *Actuator) RestoreLedger(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var entries []LedgerEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("ebs: restore ledger: %w", err)
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

var _ domain.Actuator = (*Actuator)(nil)
