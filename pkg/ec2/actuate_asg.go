package ec2

// The Auto Scaling path.
//
//	ec2:CreateLaunchTemplateVersion → (ec2:ModifyLaunchTemplate) → autoscaling:StartInstanceRefresh
//
// §3.3: "ASG-level: same math against the launch template's instance type; the
// target is the template, not individual instances." This file never touches a
// group's instances. There is no StopInstances here, no
// TerminateInstanceInAutoScalingGroup, and no UpdateAutoScalingGroup — the
// only levers are a new template version and a rolling refresh, and AWS
// performs the replacement under its own min-healthy accounting.
//
// # Respecting the refresh's own rollback
//
// StartInstanceRefresh is asked for AutoRollback, so AWS restores the previous
// configuration itself when the refresh fails. That means a terminal refresh
// has three outcomes, not two, and this unit records all three honestly:
//
//	Successful                          → StatusDone
//	RollbackSuccessful / RollbackFailed → StatusRolledBack  (NOT a success)
//	Failed / Cancelled                  → StatusFailed
//
// A rolled-back refresh leaves the group on the old configuration. Reporting
// that as "done" would tell an operator the fleet is resized when it is not,
// and would make the next run skip the step forever.
//
// # What is refused, and why each one matters
//
// The dangerous failure here is not a crash — it is a SILENT NO-OP. Several
// perfectly valid ASG configurations make "edit the launch template's instance
// type" change nothing at all, while the refresh churns the whole fleet:
// a mixed-instances policy (its overrides pick the types), attribute-based
// selection (InstanceRequirements ignores instanceType), or a group pinned to
// a numeric template version (the new version is never launched). Each is
// refused rather than attempted.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// ASG stages. Like the instance stages they are DERIVED from the account on
// every entry, never remembered.
const (
	// StageTemplateReady: the group still launches the original type.
	StageTemplateReady Stage = "template-ready"
	// StageTemplateVersioned: a template version carrying the target type
	// exists and the group points at it; the refresh has not been started.
	StageTemplateVersioned Stage = "template-versioned"
	// StageRefreshing: an instance refresh is active on the group.
	StageRefreshing Stage = "refreshing"
	// StageRefreshed: the group launches the target type and no refresh is
	// active. Done.
	StageRefreshed Stage = "refreshed"
)

// ASG refusal codes.
const (
	// RefuseASGMissing: the group is not in the account.
	RefuseASGMissing = "asg-missing"
	// RefuseASGLaunchConfig: the group uses a launch configuration. There is
	// no version to create, and launch configurations are being retired.
	RefuseASGLaunchConfig = "asg-launch-configuration"
	// RefuseASGNoTemplate: the group references no launch template at all.
	RefuseASGNoTemplate = "asg-no-launch-template"
	// RefuseASGMixedInstances: the group has a mixed-instances policy, whose
	// overrides — not the template's instanceType — decide what launches.
	// Editing the template would be a silent no-op.
	RefuseASGMixedInstances = "asg-mixed-instances-policy"
	// RefuseASGInstanceRequirements: the template selects instances by
	// attribute. instanceType is ignored.
	RefuseASGInstanceRequirements = "asg-instance-requirements"
	// RefuseASGVersionPinned: the group launches a numeric template version,
	// so a new version is never used. Repointing the group is
	// autoscaling:UpdateAutoScalingGroup, which this unit does not hold.
	RefuseASGVersionPinned = "asg-version-pinned"
	// RefuseASGSuspended: Launch or Terminate is suspended, so a refresh
	// cannot replace anything and would sit InProgress indefinitely.
	RefuseASGSuspended = "asg-suspended-processes"
	// RefuseASGRefreshInProgress: a refresh toward some OTHER configuration is
	// already running. AWS would reject a second one; this says so first.
	RefuseASGRefreshInProgress = "asg-refresh-in-progress"
	// RefuseASGEmpty: the group has no capacity, so a refresh has nothing to
	// do and its success would mean nothing.
	RefuseASGEmpty = "asg-empty"
)

// Auto Scaling processes whose suspension breaks a refresh.
const (
	ProcessLaunch      = "Launch"
	ProcessTerminate   = "Terminate"
	ProcessAZRebalance = "AZRebalance"
)

// asgFacts is everything the ASG pre-flight reads.
type asgFacts struct {
	group AutoScalingGroup
	// source is the template version the group launches today.
	source LaunchTemplateVersion
	// versions is every version of the template, newest first.
	versions []LaunchTemplateVersion
	// existing is a version already carrying the target type and newer than
	// source — the evidence that a previous run got this far.
	existing *LaunchTemplateVersion
	// refresh is the newest refresh on the group, or nil.
	refresh *InstanceRefresh
	current InstanceTypeInfo
	target  InstanceTypeInfo
	image   ImageDetail
}

// templateRef renders the launch template for a message.
func templateRef(g AutoScalingGroup) string {
	if g.LaunchTemplate == nil {
		return "(none)"
	}
	if g.LaunchTemplate.LaunchTemplateID != "" {
		return g.LaunchTemplate.LaunchTemplateID
	}
	return g.LaunchTemplate.LaunchTemplateName
}

// asgFacts reads and validates the group, its template and its refreshes.
// Every structural refusal lives here, so both Preflight and Execute get the
// identical answer from the identical code.
func (a *Actuator) asgFacts(ctx context.Context, in intent) (asgFacts, error) {
	var f asgFacts
	if a.asg == nil {
		return f, ErrNoAutoScalingSeam
	}
	gctx, cancel := a.call(ctx)
	out, err := a.asg.DescribeAutoScalingGroup(gctx, &DescribeAutoScalingGroupInput{GroupName: in.ref.ID})
	cancel()
	if err != nil {
		return f, fmt.Errorf("ec2: describe auto scaling group %s: %w", in.ref.ID, err)
	}
	if out == nil || out.Group == nil {
		return f, refuse(RefuseASGMissing, in.ref, "Auto Scaling group %q is not in this account", in.ref.ID)
	}
	f.group = *out.Group

	if err := checkASGOwnership(in, f.group); err != nil {
		return f, err
	}
	if f.group.LaunchConfigurationName != "" {
		return f, refuse(RefuseASGLaunchConfig, in.ref,
			"group launches from launch configuration %q; there is no template version to create",
			f.group.LaunchConfigurationName)
	}
	if f.group.MixedInstancesPolicy {
		return f, refuse(RefuseASGMixedInstances, in.ref,
			"group has a mixed-instances policy: its overrides decide which types launch, so changing the launch template's instanceType would churn the fleet and change nothing")
	}
	if f.group.LaunchTemplate == nil {
		return f, refuse(RefuseASGNoTemplate, in.ref, "group references no launch template")
	}
	switch strings.TrimSpace(f.group.LaunchTemplate.Version) {
	case VersionDefault, VersionLatest, "":
	default:
		return f, refuse(RefuseASGVersionPinned, in.ref,
			"group launches launch template version %q: a new version would never be used, and repointing the group is autoscaling:UpdateAutoScalingGroup, which this unit does not hold",
			f.group.LaunchTemplate.Version)
	}
	for _, p := range f.group.SuspendedProcesses {
		if strings.EqualFold(p, ProcessLaunch) || strings.EqualFold(p, ProcessTerminate) {
			return f, refuse(RefuseASGSuspended, in.ref,
				"scaling process %q is suspended: an instance refresh cannot replace instances and would sit in progress indefinitely", p)
		}
	}
	if f.group.DesiredCapacity <= 0 && len(f.group.InstanceIDs) == 0 {
		return f, refuse(RefuseASGEmpty, in.ref,
			"group has desired capacity %d and no instances; a refresh would prove nothing", f.group.DesiredCapacity)
	}

	if f.versions, err = a.templateVersions(ctx, in, *f.group.LaunchTemplate); err != nil {
		return f, err
	}
	src, ok := selectSourceVersion(f.versions, f.group.LaunchTemplate.Version)
	if !ok {
		return f, refuse(RefuseASGNoTemplate, in.ref,
			"launch template %s has no %s version to copy", templateRef(f.group),
			cmpOr(f.group.LaunchTemplate.Version, VersionDefault))
	}
	f.source = src
	if src.InstanceRequirements {
		return f, refuse(RefuseASGInstanceRequirements, in.ref,
			"launch template version %d selects instances by attribute (InstanceRequirements); instanceType is ignored",
			src.VersionNumber)
	}
	f.existing = findVersionWithType(f.versions, in.toType, src.VersionNumber)

	// Drift: the template must be what the plan recorded, unless it is already
	// the target (a resume).
	srcType := strings.ToLower(strings.TrimSpace(src.InstanceType))
	switch srcType {
	case strings.ToLower(in.fromType), strings.ToLower(in.toType):
	default:
		return f, refuse(RefuseDrift, in.ref,
			"launch template version %d launches %q; the plan recorded %s → %s",
			src.VersionNumber, src.InstanceType, in.fromType, in.toType)
	}

	if f.current, err = a.describeType(ctx, in.ref, in.fromType); err != nil {
		return f, err
	}
	if f.target, err = a.describeType(ctx, in.ref, in.toType); err != nil {
		return f, err
	}
	if f.image, err = a.describeImage(ctx, in.ref, src.ImageID); err != nil {
		return f, err
	}
	if err := checkASGTemplate(in, f); err != nil {
		return f, err
	}
	if f.refresh, err = a.latestRefresh(ctx, in.ref, f.group.Name); err != nil {
		return f, err
	}
	return f, nil
}

func cmpOr(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

// checkASGOwnership mirrors checkOwnership for a group. A Kubernetes node
// group belongs to the k8s-nodes domain, which manages the same ASG through
// pod-level guardrails this path cannot see.
func checkASGOwnership(in intent, g AutoScalingGroup) error {
	for _, t := range g.Tags {
		switch {
		case t.Key == TagEKSCluster || t.Key == TagAWSEKSCluster || strings.HasPrefix(t.Key, TagK8sClusterPrefix):
			return refuse(RefuseK8sTagged, in.ref,
				"group carries %q: it is a Kubernetes node group and belongs to the k8s-nodes domain", t.Key)
		case t.Key == TagKilterMode && strings.EqualFold(strings.TrimSpace(t.Value), "off"):
			return refuse(RefuseModeOff, in.ref, "group carries %s=off", TagKilterMode)
		}
	}
	return nil
}

// checkASGTemplate is the boot-prerequisite matrix for a launch template.
//
// It reuses [checkENA] and [checkNVMe] verbatim rather than restating them.
// There is no running instance to read an enaSupport attribute from, so the
// AMI's own flag stands in for both — which is the stricter reading, since an
// AMI without enaSupport cannot launch on an ENA-required type no matter what
// any instance attribute says.
func checkASGTemplate(in intent, f asgFacts) error {
	synth := facts{
		live: InstanceDetail{
			InstanceID:     f.group.Name,
			InstanceType:   f.source.InstanceType,
			ImageID:        f.image.ImageID,
			Architecture:   f.image.Architecture,
			EnaSupport:     f.image.ENASupport,
			RootDeviceType: f.image.RootDeviceType,
		},
		current: f.current,
		target:  f.target,
		image:   f.image,
	}
	if f.image.ImageID == "" || (f.image.State != "" && !strings.EqualFold(f.image.State, "available")) {
		return refuse(RefuseImageMissing, in.ref,
			"launch template AMI %q is not available (state %q)", f.source.ImageID, f.image.State)
	}
	arch := normalizeArch(f.image.Architecture)
	if !supportsArch(f.target, arch) {
		return refuse(RefuseArchMismatch, in.ref,
			"target %s supports %s, the template's AMI %s is %s",
			in.toType, strings.Join(f.target.SupportedArchitectures, ","), f.image.ImageID, arch)
	}
	if v := strings.TrimSpace(f.image.VirtualizationType); v != "" && !containsFold(f.target.SupportedVirtualizationTypes, v) {
		return refuse(RefuseVirtualization, in.ref,
			"template AMI %s is %s-virtualized and target %s supports %s",
			f.image.ImageID, v, in.toType, strings.Join(f.target.SupportedVirtualizationTypes, ","))
	}
	if err := checkENA(in, synth); err != nil {
		return err
	}
	if err := checkNVMe(in, synth); err != nil {
		return err
	}
	if f.target.BareMetal {
		return refuse(RefuseBareMetal, in.ref, "target %s is a bare-metal type", in.toType)
	}
	// Storage never shrinks. A new version copies the source's block device
	// mappings verbatim, so this can only fire when the step itself claims a
	// reduction — but a plan whose recorded intent contradicts what execution
	// would do is not auditable, and §3.3 is absolute about the direction.
	if err := checkBlockDevicesNotShrunk(in, f.source.BlockDevices, f.existing); err != nil {
		return err
	}
	return checkShapeMatchesCatalog(in, synth)
}

// checkBlockDevicesNotShrunk compares the source template's mappings with a
// pre-existing target version's, when one exists.
func checkBlockDevicesNotShrunk(in intent, source []BlockDevice, existing *LaunchTemplateVersion) error {
	if existing == nil {
		return nil
	}
	sizes := make(map[string]int64, len(source))
	names := make([]string, 0, len(source))
	for _, bd := range source {
		sizes[bd.DeviceName] = bd.SizeGiB
		names = append(names, bd.DeviceName)
	}
	sort.Strings(names)
	have := make(map[string]int64, len(existing.BlockDevices))
	for _, bd := range existing.BlockDevices {
		have[bd.DeviceName] = bd.SizeGiB
	}
	for _, name := range names {
		was := sizes[name]
		now, ok := have[name]
		if !ok {
			return refuse(RefuseStorageShrink, in.ref,
				"launch template version %d drops block device %s that the source version carries",
				existing.VersionNumber, name)
		}
		if now < was {
			return refuse(RefuseStorageShrink, in.ref,
				"launch template version %d shrinks %s from %d GiB to %d GiB; Kilter never shrinks storage (§3.3)",
				existing.VersionNumber, name, was, now)
		}
	}
	return nil
}

// templateVersions reads every version of a template, newest first.
func (a *Actuator) templateVersions(ctx context.Context, in intent, ref LaunchTemplateRef) ([]LaunchTemplateVersion, error) {
	cctx, cancel := a.call(ctx)
	defer cancel()
	out, err := a.asg.DescribeLaunchTemplateVersions(cctx, &DescribeLaunchTemplateVersionsInput{
		LaunchTemplateID:   ref.LaunchTemplateID,
		LaunchTemplateName: ref.LaunchTemplateName,
	})
	if err != nil {
		return nil, fmt.Errorf("ec2: describe launch template versions for %s: %w",
			cmpOr(ref.LaunchTemplateID, ref.LaunchTemplateName), err)
	}
	if out == nil || len(out.Versions) == 0 {
		return nil, refuse(RefuseASGNoTemplate, in.ref,
			"launch template %s has no versions", cmpOr(ref.LaunchTemplateID, ref.LaunchTemplateName))
	}
	vs := make([]LaunchTemplateVersion, len(out.Versions))
	copy(vs, out.Versions)
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].VersionNumber > vs[j].VersionNumber })
	return vs, nil
}

// selectSourceVersion resolves what "$Default", "$Latest" or "" means for this
// template. Versions must already be sorted newest first.
func selectSourceVersion(versions []LaunchTemplateVersion, ref string) (LaunchTemplateVersion, bool) {
	switch strings.TrimSpace(ref) {
	case VersionLatest:
		if len(versions) == 0 {
			return LaunchTemplateVersion{}, false
		}
		return versions[0], true
	default: // "$Default" and "" both mean the default version
		for _, v := range versions {
			if v.DefaultVersion {
				return v, true
			}
		}
		return LaunchTemplateVersion{}, false
	}
}

// findVersionWithType returns the OLDEST version newer than after that already
// carries the wanted instance type — the one a previous run created. Oldest,
// not newest, so a retry converges on one version rather than creating a chain.
func findVersionWithType(versions []LaunchTemplateVersion, want string, after int64) *LaunchTemplateVersion {
	var out *LaunchTemplateVersion
	for i := range versions {
		v := versions[i]
		if v.VersionNumber <= after {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(v.InstanceType), strings.TrimSpace(want)) {
			continue
		}
		if out == nil || v.VersionNumber < out.VersionNumber {
			cp := v
			out = &cp
		}
	}
	return out
}

// latestRefresh reads the newest refresh on a group, or nil.
func (a *Actuator) latestRefresh(ctx context.Context, ref domain.TargetRef, group string) (*InstanceRefresh, error) {
	cctx, cancel := a.call(ctx)
	defer cancel()
	out, err := a.asg.DescribeInstanceRefreshes(cctx, &DescribeInstanceRefreshesInput{GroupName: group})
	if err != nil {
		return nil, fmt.Errorf("ec2: describe instance refreshes for %s: %w", group, err)
	}
	if out == nil || len(out.Refreshes) == 0 {
		return nil, nil
	}
	rs := make([]InstanceRefresh, len(out.Refreshes))
	copy(rs, out.Refreshes)
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].StartTime.Equal(rs[j].StartTime) {
			return rs[i].StartTime.After(rs[j].StartTime)
		}
		return rs[i].InstanceRefreshID > rs[j].InstanceRefreshID
	})
	// An ACTIVE refresh always outranks a newer terminal one: it is the thing
	// occupying the group, and it is what a resume must join.
	for i := range rs {
		if rs[i].Active() {
			cp := rs[i]
			return &cp, nil
		}
	}
	cp := rs[0]
	return &cp, nil
}

// asgStage derives where the ASG resize has got to, from the account alone.
//
// The distinction that matters: a launch template pointing at the target type
// is NOT a migrated fleet. The template says what the next instance launches
// with; only a completed instance refresh says what the running instances are.
// Conflating the two would report a resize as done the moment the template was
// edited, with every instance still on the old type.
func asgStage(f asgFacts, in intent) Stage {
	if f.refresh != nil && f.refresh.Active() {
		return StageRefreshing
	}
	if !strings.EqualFold(strings.TrimSpace(f.source.InstanceType), strings.TrimSpace(in.toType)) {
		// The template is not at the target yet — whether or not a version
		// carrying it exists. driveASG creates the version if needed and
		// points the template at it.
		return StageTemplateReady
	}
	if f.refresh == nil {
		// Template pointed, nothing refreshed. This is the resume point after
		// a crash between ModifyLaunchTemplate and StartInstanceRefresh.
		return StageTemplateVersioned
	}
	// A terminal refresh is the last word, in every outcome. A refresh that
	// FAILED or rolled back is deliberately NOT retried automatically: churning
	// a whole fleet a second time is a decision for a human, and the ledger
	// entry stays non-terminal so the next run re-reports it without acting.
	return StageRefreshed
}

// reportRefresh classifies a group whose template is already at the target and
// whose refresh has finished. All three AWS outcomes are reported honestly; a
// rolled-back refresh is never a success, because the group is back on its
// previous configuration and calling it done would make the next run skip the
// step forever.
func (a *Actuator) reportRefresh(in intent, f asgFacts, now time.Time) error {
	rf := f.refresh
	switch {
	case rf == nil || rf.Status == RefreshSuccessful:
		a.finish(in.key, StatusNoop, StageRefreshed, now,
			fmt.Sprintf("group %s already launches %s", f.group.Name, in.toType), nil)
		return nil
	case rf.RolledBack():
		err := fmt.Errorf("ec2: instance refresh %s on %s ended %s: %s",
			rf.InstanceRefreshID, f.group.Name, rf.Status, rf.StatusReason)
		a.finish(in.key, StatusRolledBack, StageRefreshed, now, "", err)
		return err
	default:
		err := fmt.Errorf("ec2: instance refresh %s on %s ended %s: %s",
			rf.InstanceRefreshID, f.group.Name, rf.Status, rf.StatusReason)
		a.finish(in.key, StatusFailed, StageRefreshed, now, "", err)
		return err
	}
}

// executeASG runs (or resumes) one ASG migration.
func (a *Actuator) executeASG(ctx context.Context, as ApprovedStep, revert bool, now time.Time) error {
	step := as.Step()
	in, err := decodeStep(step, domain.ActionRolling)
	if err != nil {
		a.recordRefusal(step, as, revert, now, err)
		return err
	}
	in.origin, in.revert = as.origin, revert
	a.begin(step, as, revert, now)

	f, err := a.asgFacts(ctx, in)
	if err != nil {
		a.finish(in.key, statusFor(err), "", now, "", err)
		return err
	}
	stage := asgStage(f, in)
	a.observe(in.key, stage, now)
	a.recordTemplate(in.key, f)

	switch stage {
	case StageRefreshed:
		return a.reportRefresh(in, f, now)

	case StageTemplateReady:
		// Nothing has happened. Every gate runs here and only here.
		if err := checkEconomics(in, now); err != nil {
			a.finish(in.key, StatusRefused, stage, now, "", err)
			return err
		}
		if a.cfg.Mode == ModeDryRun {
			a.finish(in.key, StatusDryRun, stage, now, a.plannedASGCalls(in, f), nil)
			return nil
		}

	case StageRefreshing:
		if f.refresh != nil && !a.refreshTargetsUs(f, in) {
			err := refuse(RefuseASGRefreshInProgress, in.ref,
				"instance refresh %s is already %s on %s toward another configuration",
				f.refresh.InstanceRefreshID, f.refresh.Status, f.group.Name)
			a.finish(in.key, StatusRefused, stage, now, "", err)
			return err
		}
		if a.cfg.Mode == ModeDryRun {
			a.finish(in.key, StatusDryRun, stage, now,
				fmt.Sprintf("would resume instance refresh %s on %s", f.refresh.InstanceRefreshID, f.group.Name), nil)
			return nil
		}

	default: // StageTemplateVersioned: the template is pointed, nothing refreshed
		if a.cfg.Mode == ModeDryRun {
			a.finish(in.key, StatusDryRun, stage, now,
				fmt.Sprintf("would StartInstanceRefresh(%s) onto launch template version %d",
					f.group.Name, desiredVersion(f)), nil)
			return nil
		}
	}
	return a.driveASG(ctx, in, f, stage, revert)
}

// refreshTargetsUs reports whether the active refresh is the one this step
// started. The ledger records the ID; matching on it is the only honest test,
// because a refresh somebody else started toward another configuration must
// not be adopted as ours.
func (a *Actuator) refreshTargetsUs(f asgFacts, in intent) bool {
	if f.refresh == nil {
		return false
	}
	if e, ok := a.entry(in.key); ok && e.RefreshID == f.refresh.InstanceRefreshID {
		return true
	}
	// No recorded ID (a restart with no persisted ledger). A refresh is ours
	// only when the template already carries the target type, which is the
	// state this step's own CreateLaunchTemplateVersion would have produced.
	return f.existing != nil || strings.EqualFold(f.source.InstanceType, in.toType)
}

func (a *Actuator) plannedASGCalls(in intent, f asgFacts) string {
	mod := ""
	if f.group.LaunchTemplate != nil && strings.TrimSpace(f.group.LaunchTemplate.Version) != VersionLatest {
		mod = fmt.Sprintf(" → ModifyLaunchTemplate(%s, defaultVersion=<new>)", templateRef(f.group))
	}
	return fmt.Sprintf("CreateLaunchTemplateVersion(%s, source=%d, instanceType=%s)%s → StartInstanceRefresh(%s, minHealthy=%d%%, autoRollback); was %s",
		templateRef(f.group), f.source.VersionNumber, in.toType, mod, f.group.Name,
		a.cfg.MinHealthyPercentage, in.fromType)
}

// recordTemplate stores the undo coordinates the moment they are known: the
// version the group launched and the template's default at that instant. §3.3:
// "the ledger stores the prior version".
func (a *Actuator) recordTemplate(key string, f asgFacts) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return
	}
	if f.group.LaunchTemplate != nil {
		e.TemplateID = cmpOr(f.group.LaunchTemplate.LaunchTemplateID, f.group.LaunchTemplate.LaunchTemplateName)
	}
	if e.PriorVersion == 0 {
		e.PriorVersion = f.source.VersionNumber
		for _, v := range f.versions {
			if v.DefaultVersion {
				e.PriorDefaultVersion = v.VersionNumber
				break
			}
		}
	}
	if f.refresh != nil {
		e.RefreshStatus = f.refresh.Status
	}
}

// driveASG is the ASG machine.
func (a *Actuator) driveASG(ctx context.Context, in intent, f asgFacts, stage Stage, revert bool) error {
	now := a.cfg.Now()
	ref := *f.group.LaunchTemplate

	if stage == StageTemplateReady && revert {
		// A revert points the template back at the version it had, exactly as
		// §3.3 describes, when this actuator recorded one. Otherwise it falls
		// through and creates a version carrying the original type — the same
		// outcome by a longer road, and the only road available for a $Latest
		// group.
		prior, err := a.revertToPriorVersion(ctx, in, f, ref)
		if err != nil {
			return err
		}
		if prior != nil {
			f.existing = prior
			stage = StageTemplateVersioned
		}
	}

	if stage == StageTemplateReady {
		version := f.existing
		if version == nil {
			detail := fmt.Sprintf("CreateLaunchTemplateVersion(%s, source=%d, instanceType=%s)",
				templateRef(f.group), f.source.VersionNumber, in.toType)
			if err := a.mutate(ctx, in.key, now, StageTemplateReady, detail); err != nil {
				a.finish(in.key, StatusFailed, StageTemplateReady, a.cfg.Now(), detail, err)
				return err
			}
			cctx, cancel := a.call(ctx)
			out, err := a.asg.CreateLaunchTemplateVersion(cctx, &CreateLaunchTemplateVersionInput{
				LaunchTemplateID:   ref.LaunchTemplateID,
				LaunchTemplateName: ref.LaunchTemplateName,
				SourceVersion:      strconv.FormatInt(f.source.VersionNumber, 10),
				InstanceType:       in.toType,
				VersionDescription: "kilter " + in.key + ": " + in.fromType + " → " + in.toType,
				ClientToken:        clientToken(in.key, "ltv"),
			})
			cancel()
			if err != nil {
				err = fmt.Errorf("ec2: create launch template version for %s: %w", templateRef(f.group), err)
				a.finish(in.key, StatusFailed, StageTemplateReady, a.cfg.Now(), detail, err)
				return err
			}
			v := out.Version
			version = &v
		}
		a.recordNewVersion(in.key, version.VersionNumber)

		// A $Latest group already launches the new version. A $Default group
		// needs the template's default repointed — the only ModifyLaunchTemplate
		// this unit ever issues, and it changes nothing but which version is
		// the default.
		if strings.TrimSpace(ref.Version) != VersionLatest {
			detail := fmt.Sprintf("ModifyLaunchTemplate(%s, defaultVersion=%d)", templateRef(f.group), version.VersionNumber)
			if err := a.mutate(ctx, in.key, a.cfg.Now(), StageTemplateVersioned, detail); err != nil {
				a.finish(in.key, StatusFailed, StageTemplateVersioned, a.cfg.Now(), detail, err)
				return err
			}
			cctx, cancel := a.call(ctx)
			_, err := a.asg.ModifyLaunchTemplate(cctx, &ModifyLaunchTemplateInput{
				LaunchTemplateID:   ref.LaunchTemplateID,
				LaunchTemplateName: ref.LaunchTemplateName,
				DefaultVersion:     strconv.FormatInt(version.VersionNumber, 10),
				ClientToken:        clientToken(in.key, "mlt"),
			})
			cancel()
			if err != nil {
				err = fmt.Errorf("ec2: point %s at version %d: %w", templateRef(f.group), version.VersionNumber, err)
				a.finish(in.key, StatusFailed, StageTemplateVersioned, a.cfg.Now(), detail, err)
				return err
			}
		}
		f.existing = version
		stage = StageTemplateVersioned
	}

	if stage == StageTemplateVersioned {
		v := desiredVersion(f)
		detail := fmt.Sprintf("StartInstanceRefresh(%s, version=%d, minHealthy=%d%%, autoRollback)",
			f.group.Name, v, a.cfg.MinHealthyPercentage)
		if err := a.mutate(ctx, in.key, a.cfg.Now(), StageTemplateVersioned, detail); err != nil {
			a.finish(in.key, StatusFailed, StageTemplateVersioned, a.cfg.Now(), detail, err)
			return err
		}
		cctx, cancel := a.call(ctx)
		out, err := a.asg.StartInstanceRefresh(cctx, &StartInstanceRefreshInput{
			GroupName:                   f.group.Name,
			MinHealthyPercentage:        a.cfg.MinHealthyPercentage,
			InstanceWarmupSeconds:       int32(a.cfg.InstanceWarmup / time.Second),
			AutoRollback:                true,
			DesiredConfigurationVersion: strconv.FormatInt(v, 10),
			ClientToken:                 clientToken(in.key, "refresh"),
		})
		cancel()
		if err != nil {
			err = fmt.Errorf("ec2: start instance refresh on %s: %w", f.group.Name, err)
			a.finish(in.key, StatusFailed, StageTemplateVersioned, a.cfg.Now(), detail, err)
			return err
		}
		a.recordRefresh(in.key, out.InstanceRefreshID)
		stage = StageRefreshing
	}

	// StageRefreshing: wait it out, and respect whatever AWS's own rollback
	// decided.
	rf, err := a.awaitRefresh(ctx, in, f.group.Name)
	fin := a.cfg.Now()
	if err != nil {
		a.finish(in.key, inFlightOr(err), StageRefreshing, fin, "", err)
		return err
	}
	a.recordRefreshStatus(in.key, rf)
	switch {
	case rf.Status == RefreshSuccessful:
		a.finish(in.key, StatusDone, StageRefreshed, fin,
			fmt.Sprintf("%s refreshed onto %s (refresh %s)", f.group.Name, in.toType, rf.InstanceRefreshID), nil)
		a.cfg.Logger.Info("asg refreshed onto a new instance type",
			"group", f.group.Name, "from", in.fromType, "to", in.toType, "refresh", rf.InstanceRefreshID)
		return nil
	case rf.RolledBack():
		err := fmt.Errorf("ec2: instance refresh %s on %s ended %s: %s",
			rf.InstanceRefreshID, f.group.Name, rf.Status, rf.StatusReason)
		a.finish(in.key, StatusRolledBack, StageRefreshing, fin, "", err)
		a.cfg.Logger.Warn("asg instance refresh rolled back; the group is on its previous configuration",
			"group", f.group.Name, "refresh", rf.InstanceRefreshID, "status", rf.Status)
		return err
	default:
		err := fmt.Errorf("ec2: instance refresh %s on %s ended %s: %s",
			rf.InstanceRefreshID, f.group.Name, rf.Status, rf.StatusReason)
		a.finish(in.key, StatusFailed, StageRefreshing, fin, "", err)
		return err
	}
}

// revertToPriorVersion implements §3.3's "point template back": when the
// forward step recorded the template's prior default version, the undo is a
// single ModifyLaunchTemplate rather than a new version.
//
// It returns the version it repointed to, or nil when that road is not open —
// a $Latest group, a lost ledger, or a prior version that is not the shape
// this undo is restoring. In every one of those cases the caller falls back to
// creating a version, which reaches the same place.
func (a *Actuator) revertToPriorVersion(ctx context.Context, in intent, f asgFacts, ref LaunchTemplateRef) (*LaunchTemplateVersion, error) {
	if strings.TrimSpace(ref.Version) == VersionLatest {
		return nil, nil // $Latest cannot be repointed; a new version is the only undo
	}
	e, ok := a.entry(in.origin)
	if !ok || e.PriorDefaultVersion == 0 {
		return nil, nil
	}
	prior := versionByNumber(f.versions, e.PriorDefaultVersion)
	if prior == nil {
		return nil, nil
	}
	// in.toType is the ORIGINAL type here: this intent is the inverse step.
	if !strings.EqualFold(strings.TrimSpace(prior.InstanceType), strings.TrimSpace(in.toType)) {
		return nil, nil
	}
	detail := fmt.Sprintf("revert: ModifyLaunchTemplate(%s, defaultVersion=%d)", templateRef(f.group), prior.VersionNumber)
	if err := a.mutate(ctx, in.key, a.cfg.Now(), StageTemplateReady, detail); err != nil {
		a.finish(in.key, StatusFailed, StageTemplateReady, a.cfg.Now(), detail, err)
		return nil, err
	}
	cctx, cancel := a.call(ctx)
	_, err := a.asg.ModifyLaunchTemplate(cctx, &ModifyLaunchTemplateInput{
		LaunchTemplateID:   ref.LaunchTemplateID,
		LaunchTemplateName: ref.LaunchTemplateName,
		DefaultVersion:     strconv.FormatInt(prior.VersionNumber, 10),
		ClientToken:        clientToken(in.key, "mlt-revert"),
	})
	cancel()
	if err != nil {
		err = fmt.Errorf("ec2: point %s back at version %d: %w", templateRef(f.group), prior.VersionNumber, err)
		a.finish(in.key, StatusFailed, StageTemplateReady, a.cfg.Now(), detail, err)
		return nil, err
	}
	a.recordNewVersion(in.key, prior.VersionNumber)
	return prior, nil
}

// desiredVersion is the launch template version a refresh must pin itself to:
// the one this step created when it created one, and otherwise the version the
// group already launches. Pinning matters — an unpinned refresh follows
// whatever $Latest becomes while it runs.
func desiredVersion(f asgFacts) int64 {
	if f.existing != nil {
		return f.existing.VersionNumber
	}
	return f.source.VersionNumber
}

func versionByNumber(versions []LaunchTemplateVersion, n int64) *LaunchTemplateVersion {
	for i := range versions {
		if versions[i].VersionNumber == n {
			cp := versions[i]
			return &cp
		}
	}
	return nil
}

// awaitRefresh polls until the refresh reaches a terminal status.
func (a *Actuator) awaitRefresh(ctx context.Context, in intent, group string) (InstanceRefresh, error) {
	attempts := int(a.cfg.PollTimeout / a.cfg.PollInterval)
	if attempts < 1 {
		attempts = 1
	}
	var last InstanceRefresh
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		rf, err := a.latestRefresh(ctx, in.ref, group)
		if err != nil {
			return last, err
		}
		if rf == nil {
			return last, fmt.Errorf("ec2: no instance refresh on %s after starting one", group)
		}
		last = *rf
		a.observe(in.key, StageRefreshing, a.cfg.Now())
		if last.Terminal() {
			return last, nil
		}
		if i+1 >= attempts {
			return last, fmt.Errorf("%w: refresh %s on %s is %q after %d poll(s)",
				ErrPollTimeout, last.InstanceRefreshID, group, last.Status, i+1)
		}
		if err := a.cfg.Sleep(ctx, a.cfg.PollInterval); err != nil {
			return last, err
		}
	}
}

func (a *Actuator) recordNewVersion(key string, version int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[key]; ok {
		e.NewVersion = version
	}
}

func (a *Actuator) recordRefresh(key, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[key]; ok {
		e.RefreshID = id
	}
}

func (a *Actuator) recordRefreshStatus(key string, rf InstanceRefresh) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[key]; ok {
		e.RefreshID = rf.InstanceRefreshID
		e.RefreshStatus = rf.Status
	}
}
