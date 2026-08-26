package ec2

// The standalone-instance stop-resize machine.
//
//	ec2:StopInstances → ec2:ModifyInstanceAttribute(instanceType) → ec2:StartInstances
//
// Three cloud transitions, two crash windows, one instance that is not serving
// traffic for the duration. The design that makes that survivable:
//
//   - **The stage is observed, never remembered.** Every entry re-reads the
//     instance and derives the stage from AWS's own answer ([stageOf]). A
//     ledger that says "stopping" and an account that says "running as the new
//     type" disagree; the account wins, every time.
//   - **Gates run once, at the top, while the instance is still up.** Past the
//     stop the machine may only move forward (to the target, running) or back
//     (to the original type, running). It is never allowed to conclude "this
//     resize is no longer worth doing" and stop, because that conclusion with
//     a stopped instance underneath it is an outage nobody is looking for.
//   - **A failed modify rolls forward into a start.** If
//     ModifyInstanceAttribute fails on a stopped instance, the machine starts
//     it again at its ORIGINAL type and records [StatusRolledBack]. Service
//     comes back; the step is honestly marked as not-done.
//   - **Every transition is recorded before it is issued.** See
//     [Actuator.mutate].

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// stageOf derives where a resize has got to from the live instance alone.
//
// It compares the live instance type against BOTH the step's From and its To,
// which is what makes the machine resumable: "stopped, already the new type"
// is a different world from "stopped, still the old type", and only the
// account can tell them apart.
func stageOf(live InstanceDetail, fromType, toType string) Stage {
	t := strings.ToLower(strings.TrimSpace(live.InstanceType))
	from := strings.ToLower(strings.TrimSpace(fromType))
	to := strings.ToLower(strings.TrimSpace(toType))
	switch strings.ToLower(strings.TrimSpace(live.State)) {
	case StateRunning:
		switch t {
		case to:
			return StageRunning
		case from:
			return StageReady
		}
	case StateStopped:
		switch t {
		case to:
			return StageModified
		case from:
			return StageStopped
		}
	case StateStopping:
		return StageStopping
	case StatePending:
		return StageStarting
	case StateShuttingDown, StateTerminated:
		return StageGone
	}
	return StageUnknown
}

// transient reports whether a stage is one AWS is actively moving through, so
// the machine waits rather than acts.
func (s Stage) transient() bool { return s == StageStopping || s == StageStarting }

// describeInstance reads one instance through the read-only seam.
func (a *Actuator) describeInstance(ctx context.Context, ref domain.TargetRef) (InstanceDetail, error) {
	cctx, cancel := a.call(ctx)
	defer cancel()
	out, err := a.api.DescribeInstanceDetail(cctx, &DescribeInstanceDetailInput{InstanceID: ref.ID})
	if err != nil {
		return InstanceDetail{}, fmt.Errorf("ec2: describe %s: %w", ref.ID, err)
	}
	if out == nil || out.Instance == nil {
		return InstanceDetail{}, refuse(RefuseInstanceMissing, ref, "instance %s is not in this account", ref.ID)
	}
	live := *out.Instance
	if live.InstanceID != "" && live.InstanceID != ref.ID {
		return InstanceDetail{}, refuse(RefuseInstanceMissing, ref,
			"the seam answered for %s when asked about %s", live.InstanceID, ref.ID)
	}
	return live, nil
}

// describeType reads one instance type's prerequisites.
func (a *Actuator) describeType(ctx context.Context, ref domain.TargetRef, instanceType string) (InstanceTypeInfo, error) {
	cctx, cancel := a.call(ctx)
	defer cancel()
	out, err := a.api.DescribeInstanceType(cctx, &DescribeInstanceTypeInput{InstanceType: instanceType})
	if err != nil {
		return InstanceTypeInfo{}, fmt.Errorf("ec2: describe instance type %s: %w", instanceType, err)
	}
	if out == nil || out.Info == nil {
		return InstanceTypeInfo{}, refuse(RefuseUnknownInstanceType, ref,
			"DescribeInstanceTypes has no record for %q; an unknown target is never assumed compatible", instanceType)
	}
	return *out.Info, nil
}

// describeImage reads the AMI.
func (a *Actuator) describeImage(ctx context.Context, ref domain.TargetRef, imageID string) (ImageDetail, error) {
	if strings.TrimSpace(imageID) == "" {
		return ImageDetail{}, refuse(RefuseImageMissing, ref,
			"the instance reports no AMI, so its ENA/NVMe prerequisites cannot be verified")
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	out, err := a.api.DescribeImage(cctx, &DescribeImageInput{ImageID: imageID})
	if err != nil {
		return ImageDetail{}, fmt.Errorf("ec2: describe image %s: %w", imageID, err)
	}
	if out == nil || out.Image == nil {
		return ImageDetail{}, refuse(RefuseImageMissing, ref,
			"AMI %s is not in this account (deregistered or shared away)", imageID)
	}
	return *out.Image, nil
}

// factsFor gathers everything the pre-flight predicates read, given a live
// instance already in hand.
func (a *Actuator) factsFor(ctx context.Context, in intent, live InstanceDetail) (facts, error) {
	f := facts{live: live}
	var err error
	if f.current, err = a.describeType(ctx, in.ref, in.fromType); err != nil {
		return f, err
	}
	if f.target, err = a.describeType(ctx, in.ref, in.toType); err != nil {
		return f, err
	}
	if f.image, err = a.describeImage(ctx, in.ref, live.ImageID); err != nil {
		return f, err
	}
	return f, nil
}

// instanceFacts reads the instance and then everything about it.
func (a *Actuator) instanceFacts(ctx context.Context, in intent) (facts, error) {
	live, err := a.describeInstance(ctx, in.ref)
	if err != nil {
		return facts{}, err
	}
	return a.factsFor(ctx, in, live)
}

// checkInstance is the whole standalone pre-flight matrix, in the order a
// human would want it: who owns this, would stopping it destroy anything,
// would it come back, does the plan agree with the catalog.
func checkInstance(in intent, f facts) error {
	if err := checkOwnership(in, f.live); err != nil {
		return err
	}
	if err := checkSafety(in, f); err != nil {
		return err
	}
	return checkShapeMatchesCatalog(in, f)
}

// executeInstance runs (or resumes) one stop-start resize.
func (a *Actuator) executeInstance(ctx context.Context, as ApprovedStep, revert bool, now time.Time) error {
	step := as.Step()
	in, err := decodeStep(step, domain.ActionStopStart)
	if err != nil {
		a.recordRefusal(step, as, revert, now, err)
		return err
	}
	in.origin, in.revert = as.origin, revert
	a.begin(step, as, revert, now)

	live, err := a.describeInstance(ctx, in.ref)
	if err != nil {
		a.finish(in.key, statusFor(err), "", now, "", err)
		return err
	}
	stage := stageOf(live, in.fromType, in.toType)
	a.observe(in.key, stage, now)

	switch stage {
	case StageGone:
		err := refuse(RefuseInstanceState, in.ref,
			"instance is %q; no step acts on a terminating or terminated instance", live.State)
		a.finish(in.key, StatusRefused, stage, now, "", err)
		return err
	case StageUnknown:
		err := refuse(RefuseDrift, in.ref,
			"live instance is %s/%s; the plan recorded %s → %s. Somebody else changed it, so the plan is stale",
			live.InstanceType, live.State, in.fromType, in.toType)
		a.finish(in.key, StatusRefused, stage, now, "", err)
		return err
	case StageRunning:
		// Already the target, already running. Doing nothing is always
		// allowed, and costs no cloud call beyond the one read above.
		a.finish(in.key, StatusNoop, StageRunning, now,
			fmt.Sprintf("instance %s already runs as %s", in.ref.ID, in.toType), nil)
		return nil
	case StageReady:
		// Nothing has happened yet, so this is the ONE place every gate runs.
		if err := checkEconomics(in, now); err != nil {
			a.finish(in.key, StatusRefused, stage, now, "", err)
			return err
		}
		f, err := a.factsFor(ctx, in, live)
		if err != nil {
			a.finish(in.key, statusFor(err), stage, now, "", err)
			return err
		}
		if err := checkInstance(in, f); err != nil {
			a.finish(in.key, StatusRefused, stage, now, "", err)
			return err
		}
		if a.cfg.Mode == ModeDryRun {
			a.finish(in.key, StatusDryRun, stage, now, a.plannedCalls(in), nil)
			return nil
		}
	default:
		// Stopped, stopping, modified or starting: this is a RESUME. The gates
		// are not re-run — see the file comment. The instance is already down
		// and the only acceptable outcomes are "running as To" and "running as
		// From".
		if a.cfg.Mode == ModeDryRun {
			a.finish(in.key, StatusDryRun, stage, now,
				fmt.Sprintf("would resume %s from stage %q (%s/%s)", in.ref.ID, stage, live.InstanceType, live.State), nil)
			return nil
		}
		a.cfg.Logger.Warn("resuming an interrupted EC2 resize",
			"instance", in.ref.ID, "stage", string(stage), "from", in.fromType, "to", in.toType)
	}
	return a.driveStopStart(ctx, in, stage)
}

// plannedCalls renders exactly what apply would issue, so a dry-run entry is
// reviewable rather than a bare "ok".
func (a *Actuator) plannedCalls(in intent) string {
	return fmt.Sprintf("StopInstances(%s) → ModifyInstanceAttribute(%s, instanceType=%s) → StartInstances(%s); was %s",
		in.ref.ID, in.ref.ID, in.toType, in.ref.ID, in.fromType)
}

// statusFor classifies an error for the ledger: a refusal is a refusal, and
// everything else is a failure.
func statusFor(err error) string {
	if IsRefusal(err) {
		return StatusRefused
	}
	return StatusFailed
}

// begin creates the ledger entry without asserting anything about its outcome.
func (a *Actuator) begin(step domain.Step, as ApprovedStep, revert bool, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.upsert(step, as, revert, now)
	e.Mode = a.cfg.Mode
	if e.Fingerprint == "" {
		e.Fingerprint = as.approval.token.Fingerprint
		e.ApprovedBy = as.approval.token.ApprovedBy
	}
}

// driveStopStart is the machine. It only ever returns with the instance
// running, or with a non-terminal ledger entry naming the stage a later run
// must resume from.
func (a *Actuator) driveStopStart(ctx context.Context, in intent, stage Stage) error {
	visits := map[Stage]int{}
	for {
		now := a.cfg.Now()
		visits[stage]++
		if visits[stage] > maxStageVisits {
			err := fmt.Errorf("%w: %s saw stage %q %d times; something else is moving this instance",
				ErrStuck, in.ref.ID, stage, visits[stage])
			a.finish(in.key, StatusFailed, stage, now, "", err)
			return err
		}

		switch stage {
		case StageRunning:
			a.finish(in.key, StatusDone, StageRunning, now,
				fmt.Sprintf("%s resized %s → %s", in.ref.ID, in.fromType, in.toType), nil)
			a.cfg.Logger.Info("ec2 instance resized",
				"instance", in.ref.ID, "from", in.fromType, "to", in.toType)
			return nil

		case StageReady:
			detail := fmt.Sprintf("StopInstances(%s)", in.ref.ID)
			if err := a.mutate(ctx, in.key, now, StageStopping, detail); err != nil {
				a.finish(in.key, StatusFailed, StageReady, a.cfg.Now(), detail, err)
				return err
			}
			if err := a.stopInstance(ctx, in); err != nil {
				// Nothing was stopped, or we do not know. Either way the next
				// run re-observes; the entry stays non-terminal.
				a.finish(in.key, StatusFailed, StageStopping, a.cfg.Now(), detail, err)
				return err
			}
			stage = StageStopping

		case StageStopped:
			detail := fmt.Sprintf("ModifyInstanceAttribute(%s, instanceType=%s)", in.ref.ID, in.toType)
			if err := a.mutate(ctx, in.key, now, StageStopped, detail); err != nil {
				a.finish(in.key, StatusFailed, StageStopped, a.cfg.Now(), detail, err)
				return err
			}
			if err := a.modifyInstanceType(ctx, in); err != nil {
				// The instance is DOWN and cannot become what the plan wanted.
				// Restore service at the original type rather than leaving it
				// stopped — this is the branch the fuzz target exists for.
				return a.rollback(ctx, in, err)
			}
			stage = StageModified

		case StageModified:
			detail := fmt.Sprintf("StartInstances(%s) as %s", in.ref.ID, in.toType)
			if err := a.mutate(ctx, in.key, now, StageStarting, detail); err != nil {
				a.finish(in.key, StatusFailed, StageModified, a.cfg.Now(), detail, err)
				return err
			}
			if err := a.startInstance(ctx, in); err != nil {
				a.finish(in.key, StatusFailed, StageModified, a.cfg.Now(), detail, err)
				return err
			}
			stage = StageStarting

		case StageStopping, StageStarting:
			next, err := a.awaitSettled(ctx, in)
			if err != nil {
				a.finish(in.key, inFlightOr(err), stage, a.cfg.Now(), "", err)
				return err
			}
			stage = next

		case StageGone:
			err := refuse(RefuseInstanceState, in.ref,
				"instance %s terminated while its resize was in flight; there is nothing left to resume", in.ref.ID)
			a.finish(in.key, StatusFailed, StageGone, now, "", err)
			return err

		default: // StageUnknown
			err := refuse(RefuseDrift, in.ref,
				"instance %s no longer matches %s or %s while its resize was in flight",
				in.ref.ID, in.fromType, in.toType)
			a.finish(in.key, StatusFailed, StageUnknown, now, "", err)
			return err
		}
	}
}

// inFlightOr maps a poll timeout to the in-flight status — not a failure: the
// transition is running and a later execution resumes it.
func inFlightOr(err error) string {
	if errors.Is(err, ErrPollTimeout) {
		return StatusInFlight
	}
	return statusFor(err)
}

// rollback restores service after a modify that cannot succeed.
//
// It starts the instance at its ORIGINAL type. If even that fails the entry is
// left non-terminal and failed, so the next run resumes from StageStopped,
// tries the modify again, and rolls back again — the loop always ends with the
// instance running or with a human looking at a persistent failure. What it
// never does is return success, and never leaves the entry terminal with the
// instance down.
func (a *Actuator) rollback(ctx context.Context, in intent, cause error) error {
	now := a.cfg.Now()
	detail := fmt.Sprintf("rollback: StartInstances(%s) at the original %s after %v", in.ref.ID, in.fromType, cause)
	a.cfg.Logger.Error("ec2 resize failed with the instance stopped — restoring the original type",
		"instance", in.ref.ID, "type", in.fromType, "err", cause)

	if err := a.mutate(ctx, in.key, now, StageStopped, detail); err != nil {
		a.finish(in.key, StatusFailed, StageStopped, a.cfg.Now(), detail, errors.Join(cause, err))
		return errors.Join(cause, err)
	}
	if err := a.startInstance(ctx, in); err != nil {
		a.finish(in.key, StatusFailed, StageStopped, a.cfg.Now(), detail, errors.Join(cause, err))
		return errors.Join(cause, err)
	}
	stage, err := a.awaitSettled(ctx, in)
	fin := a.cfg.Now()
	if err != nil {
		a.finish(in.key, inFlightOr(err), stage, fin, detail, errors.Join(cause, err))
		return errors.Join(cause, err)
	}
	switch stage {
	case StageReady:
		a.finish(in.key, StatusRolledBack, StageReady, fin, detail, cause)
		return cause
	case StageRunning:
		// The instance came back as the TARGET type: the modify had in fact
		// landed and the error was a lost response. Reporting a rollback here
		// would be a lie, and reporting a failure would make the next run stop
		// the instance again for work that is already done.
		a.cfg.Logger.Info("the failed modify had landed after all; the instance is running as the target",
			"instance", in.ref.ID, "type", in.toType)
		a.finish(in.key, StatusDone, StageRunning, fin, detail, nil)
		return nil
	default:
		a.finish(in.key, StatusFailed, stage, fin, detail, cause)
		return cause
	}
}

// awaitSettled polls until the instance leaves a transient state.
func (a *Actuator) awaitSettled(ctx context.Context, in intent) (Stage, error) {
	attempts := int(a.cfg.PollTimeout / a.cfg.PollInterval)
	if attempts < 1 {
		attempts = 1
	}
	stage := StageUnknown
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return stage, err
		}
		live, err := a.describeInstance(ctx, in.ref)
		if err != nil {
			return stage, err
		}
		stage = stageOf(live, in.fromType, in.toType)
		a.observe(in.key, stage, a.cfg.Now())
		if !stage.transient() {
			return stage, nil
		}
		if i+1 >= attempts {
			return stage, fmt.Errorf("%w: %s is %q after %d poll(s)", ErrPollTimeout, in.ref.ID, stage, i+1)
		}
		if err := a.cfg.Sleep(ctx, a.cfg.PollInterval); err != nil {
			return stage, err
		}
	}
}

// --- the three mutations ----------------------------------------------------

// stopInstance issues ec2:StopInstances. Force and Hibernate are never set:
// a forced stop is a power cut, and hibernation pins a RAM image to a shape
// this step is about to change.
func (a *Actuator) stopInstance(ctx context.Context, in intent) error {
	cctx, cancel := a.call(ctx)
	defer cancel()
	if _, err := a.api.StopInstances(cctx, &StopInstancesInput{InstanceIDs: []string{in.ref.ID}}); err != nil {
		return fmt.Errorf("ec2: stop %s: %w", in.ref.ID, err)
	}
	return nil
}

// modifyInstanceType issues ec2:ModifyInstanceAttribute for instanceType, and
// nothing else — there is no other field on the input.
func (a *Actuator) modifyInstanceType(ctx context.Context, in intent) error {
	cctx, cancel := a.call(ctx)
	defer cancel()
	_, err := a.api.ModifyInstanceAttribute(cctx, &ModifyInstanceAttributeInput{
		InstanceID: in.ref.ID, InstanceType: in.toType,
	})
	if err != nil {
		return fmt.Errorf("ec2: modify %s to %s: %w", in.ref.ID, in.toType, err)
	}
	return nil
}

// startInstance issues ec2:StartInstances.
func (a *Actuator) startInstance(ctx context.Context, in intent) error {
	cctx, cancel := a.call(ctx)
	defer cancel()
	if _, err := a.api.StartInstances(cctx, &StartInstancesInput{InstanceIDs: []string{in.ref.ID}}); err != nil {
		return fmt.Errorf("ec2: start %s: %w", in.ref.ID, err)
	}
	return nil
}
