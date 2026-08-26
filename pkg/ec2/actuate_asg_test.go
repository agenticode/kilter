package ec2

// The Auto Scaling path.
//
// Two properties dominate here and neither is about the happy path:
//
//  1. This code NEVER touches a group's instances. Every test asserts it.
//  2. A silent no-op is worse than a failure. Several legal ASG shapes make
//     "edit the launch template's instance type" change nothing while the
//     refresh churns the whole fleet; each is refused with its own code.

import (
	"errors"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

const asgName = "asg-app"

func actGroup(tune func(*AutoScalingGroup)) AutoScalingGroup {
	g := AutoScalingGroup{
		Name: asgName, ARN: "arn:aws:autoscaling:::autoScalingGroup:1:" + asgName,
		LaunchTemplate:  &LaunchTemplateRef{LaunchTemplateID: "lt-1", Version: VersionDefault},
		MinSize:         2,
		MaxSize:         6,
		DesiredCapacity: 3,
		InstanceIDs:     []string{"i-1", "i-2", "i-3"},
		Tags:            []Tag{{Key: TagName, Value: asgName}},
	}
	if tune != nil {
		tune(&g)
	}
	return g
}

func actVersions(tune func(*LaunchTemplateVersion)) []LaunchTemplateVersion {
	v := LaunchTemplateVersion{
		LaunchTemplateID: "lt-1", VersionNumber: 1, DefaultVersion: true,
		InstanceType: "m5.2xlarge", ImageID: "ami-nitro",
		BlockDevices: []BlockDevice{{DeviceName: "/dev/xvda", VolumeType: "gp3", SizeGiB: 100}},
	}
	if tune != nil {
		tune(&v)
	}
	return []LaunchTemplateVersion{v}
}

func newASGFixture(clock *actClock, g AutoScalingGroup, versions []LaunchTemplateVersion, refreshes []InstanceRefresh) *ActuateFixture {
	f := NewActuateFixture(clock.Now, nil, actTypes(), actImages())
	return f.WithASG(g, versions, refreshes)
}

// assertNoInstanceMutations is §3.3's "never touch ASG instances directly",
// asserted rather than asserted-about-in-a-comment.
func assertNoInstanceMutations(t *testing.T, f *ActuateFixture) {
	t.Helper()
	for _, op := range []string{OpStopInstances, OpStartInstances, OpModifyInstanceAttribute} {
		if n := f.Count(op); n != 0 {
			t.Errorf("the ASG path issued %s %d time(s); it must only version the template and refresh", op, n)
		}
	}
}

func TestASGMigrationVersionsTheTemplateAndRefreshes(t *testing.T) {
	clock := newActClock(actBase)
	f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actASGStep(actStepOpts{})

	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertNoInstanceMutations(t, f)

	versions := f.Versions()
	if len(versions) != 2 {
		t.Fatalf("created %d version(s), want exactly 2", len(versions))
	}
	newest := versions[0]
	if newest.InstanceType != "m6i.2xlarge" || newest.VersionNumber != 2 || !newest.DefaultVersion {
		t.Fatalf("new version = %+v, want v2 m6i.2xlarge as the default", newest)
	}
	if len(newest.BlockDevices) != 1 || newest.BlockDevices[0].SizeGiB != 100 {
		t.Fatalf("the new version did not inherit storage: %+v", newest.BlockDevices)
	}
	if n := f.Count(OpStartInstanceRefresh); n != 1 {
		t.Fatalf("started %d refresh(es), want 1", n)
	}

	e, _ := a.Entry(step.Key)
	if e.Status != StatusDone || e.Stage != StageRefreshed {
		t.Fatalf("ledger = %s/%s, want %s/%s", e.Status, e.Stage, StatusDone, StageRefreshed)
	}
	if e.PriorVersion != 1 || e.PriorDefaultVersion != 1 || e.NewVersion != 2 {
		t.Fatalf("the ledger did not record the undo coordinates: %+v", e)
	}
	if e.RefreshID == "" || e.RefreshStatus != RefreshSuccessful || e.TemplateID != "lt-1" {
		t.Fatalf("the ledger did not record the refresh: %+v", e)
	}
}

// AWS's own rollback is respected: a rolled-back refresh is not a success.
func TestASGRefreshRollbackIsNotSuccess(t *testing.T) {
	for _, outcome := range []string{RefreshRollbackSuccessful, RefreshRollbackFailed} {
		t.Run(outcome, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
			f.RefreshOutcome = outcome
			f.RefreshStatusReason = "instances failed to become healthy"
			a := newActActuator(t, f, clock, ModeApply, nil)
			step := actASGStep(actStepOpts{})
			as := actAuthorized(t, actBase, step)

			err := a.Execute(t.Context(), as)
			if err == nil {
				t.Fatal("a rolled-back refresh reported success")
			}
			e, _ := a.Entry(step.Key)
			if e.Status != StatusRolledBack {
				t.Fatalf("status = %q, want %q", e.Status, StatusRolledBack)
			}
			if e.RefreshStatus != outcome {
				t.Errorf("refresh status = %q, want %q", e.RefreshStatus, outcome)
			}
			if !e.Terminal() {
				t.Error("a rolled-back refresh must be terminal: re-refreshing a whole fleet is a human's decision")
			}
			assertNoInstanceMutations(t, f)

			// And a controller that lost its ledger re-reports it rather than
			// churning the fleet again.
			fresh := newActActuator(t, f, clock, ModeApply, nil)
			if err := fresh.Execute(t.Context(), as); err == nil {
				t.Fatal("a restart reported the rolled-back migration as success")
			}
			if n := f.Count(OpStartInstanceRefresh); n != 1 {
				t.Errorf("a restart started %d refreshes; a rolled-back fleet must not be churned again", n)
			}
		})
	}
}

func TestASGRefreshFailureIsReportedNotRetried(t *testing.T) {
	clock := newActClock(actBase)
	f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
	f.RefreshOutcome = RefreshFailed
	f.RefreshStatusReason = "capacity unavailable"
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actASGStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)

	if err := a.Execute(t.Context(), as); err == nil {
		t.Fatal("a failed refresh reported success")
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusFailed || e.RefreshStatus != RefreshFailed {
		t.Fatalf("ledger = %+v", e)
	}
	fresh := newActActuator(t, f, clock, ModeApply, nil)
	if err := fresh.Execute(t.Context(), as); err == nil {
		t.Fatal("a restart reported a failed refresh as success")
	}
	if n := f.Count(OpStartInstanceRefresh); n != 1 {
		t.Errorf("a restart started %d refreshes, want 1", n)
	}
}

// Silent no-ops, refused. Each of these ASG shapes ignores the launch
// template's instanceType, so editing it would churn the fleet for nothing.
func TestASGRefusalMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		group    func(*AutoScalingGroup)
		versions func(*LaunchTemplateVersion)
		refresh  []InstanceRefresh
		account  func(*ActuateFixture)
		step     actStepOpts
		code     string
	}{
		{name: "mixed instances policy", code: RefuseASGMixedInstances,
			group: func(g *AutoScalingGroup) { g.MixedInstancesPolicy = true }},
		{name: "launch configuration", code: RefuseASGLaunchConfig,
			group: func(g *AutoScalingGroup) { g.LaunchConfigurationName = "lc-legacy"; g.LaunchTemplate = nil }},
		{name: "no launch template", code: RefuseASGNoTemplate,
			group: func(g *AutoScalingGroup) { g.LaunchTemplate = nil }},
		{name: "pinned template version", code: RefuseASGVersionPinned,
			group: func(g *AutoScalingGroup) { g.LaunchTemplate.Version = "1" }},
		{name: "attribute-based instance selection", code: RefuseASGInstanceRequirements,
			versions: func(v *LaunchTemplateVersion) { v.InstanceRequirements = true }},
		{name: "launch suspended", code: RefuseASGSuspended,
			group: func(g *AutoScalingGroup) { g.SuspendedProcesses = []string{ProcessLaunch} }},
		{name: "terminate suspended", code: RefuseASGSuspended,
			group: func(g *AutoScalingGroup) { g.SuspendedProcesses = []string{ProcessAZRebalance, ProcessTerminate} }},
		{name: "empty group", code: RefuseASGEmpty,
			group: func(g *AutoScalingGroup) { g.DesiredCapacity = 0; g.InstanceIDs = nil }},
		{name: "kubernetes node group", code: RefuseK8sTagged,
			group: func(g *AutoScalingGroup) {
				g.Tags = append(g.Tags, Tag{Key: TagK8sClusterPrefix + "prod", Value: "owned"})
			}},
		{name: "operator opted out", code: RefuseModeOff,
			group: func(g *AutoScalingGroup) { g.Tags = append(g.Tags, Tag{Key: TagKilterMode, Value: "off"}) }},
		{name: "template drifted", code: RefuseDrift,
			versions: func(v *LaunchTemplateVersion) { v.InstanceType = "t2.2xlarge" }},
		{name: "template AMI is arm64", code: RefuseArchMismatch,
			versions: func(v *LaunchTemplateVersion) { v.ImageID = "ami-arm" }},
		{name: "template AMI has no ENA", code: RefuseENAUnsupported,
			versions: func(v *LaunchTemplateVersion) { v.ImageID = "ami-noena" }},
		{name: "commitment negative", code: RefuseCommitmentNegative,
			step: actStepOpts{net: "-4"}},
		{name: "memory blind", code: RefuseMemoryBlind,
			step: actStepOpts{toType: "m6i.xlarge", toCPU: 4000, toMem: actMem(16), memSig: MemorySignalNone}},
		{name: "a refresh toward another configuration is running", code: RefuseASGRefreshInProgress,
			refresh: []InstanceRefresh{{InstanceRefreshID: "refresh-someone-else", GroupName: asgName,
				Status: RefreshInProgress, StartTime: actBase.Add(-time.Minute)}},
			account: func(f *ActuateFixture) { f.RefreshSettleAfter = 50 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newASGFixture(clock, actGroup(tc.group), actVersions(tc.versions), tc.refresh)
			if tc.account != nil {
				tc.account(f)
			}
			a := newActActuator(t, f, clock, ModeApply, nil)
			step := actASGStep(tc.step)
			wantRefusal(t, f, a.Execute(t.Context(), actAuthorized(t, actBase, step)), tc.code)
			assertNoInstanceMutations(t, f)
		})
	}
}

func TestASGMissingGroupIsRefused(t *testing.T) {
	clock := newActClock(actBase)
	f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actASGStep(actStepOpts{id: "asg-ghost"})
	wantRefusal(t, f, a.Execute(t.Context(), actAuthorized(t, actBase, step)), RefuseASGMissing)
}

func TestASGDryRunTouchesNothing(t *testing.T) {
	clock := newActClock(actBase)
	f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
	a := newActActuator(t, f, clock, ModeDryRun, nil)
	step := actASGStep(actStepOpts{})
	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("a dry-run issued %d mutating call(s): %v", n, f.Ops())
	}
	if len(f.Versions()) != 1 {
		t.Fatal("a dry-run created a launch template version")
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusDryRun {
		t.Fatalf("status = %q", e.Status)
	}
}

// Resumability: every ASG stage boundary, resumed by a controller that lost
// its ledger. The property is that no boundary produces a second launch
// template version or a second refresh.
func TestASGResumesFromEveryStageBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(*ActuateFixture) func()
		tune func(*ActuatorConfig)
	}{
		{name: "before the version is created", arm: failFirst(OpCreateLaunchTemplateVersion)},
		{name: "the create's response is lost", arm: loseFirstResponse(OpCreateLaunchTemplateVersion)},
		{name: "before the default is pointed", arm: failFirst(OpModifyLaunchTemplate)},
		{name: "the modify's response is lost", arm: loseFirstResponse(OpModifyLaunchTemplate)},
		{name: "before the refresh is started", arm: failFirst(OpStartInstanceRefresh)},
		{name: "the refresh start's response is lost", arm: loseFirstResponse(OpStartInstanceRefresh)},
		{
			name: "while the refresh is running",
			arm: func(f *ActuateFixture) func() {
				f.RefreshSettleAfter = 50
				return func() { f.RefreshSettleAfter = 0 }
			},
			tune: func(c *ActuatorConfig) { c.PollTimeout = c.PollInterval },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
			repair := tc.arm(f)
			step := actASGStep(actStepOpts{})
			as := actAuthorized(t, actBase, step)

			a := newActActuator(t, f, clock, ModeApply, tc.tune)
			if err := a.Execute(t.Context(), as); err == nil {
				t.Fatal("the interrupted attempt reported success")
			}
			if e, ok := a.Entry(step.Key); !ok || e.Terminal() {
				t.Fatalf("an interrupted ASG step left entry %+v; it must stay resumable", e)
			}
			repair()

			for i := 0; i < 4; i++ {
				fresh := newActActuator(t, f, clock, ModeApply, nil)
				if err := fresh.Execute(t.Context(), as); err == nil {
					break
				} else if i == 3 {
					t.Fatalf("resume did not converge: %v", err)
				}
			}
			versions := f.Versions()
			if len(versions) != 2 {
				t.Fatalf("resume produced %d versions, want 2: a retry must not chain versions", len(versions))
			}
			if versions[0].InstanceType != "m6i.2xlarge" || !versions[0].DefaultVersion {
				t.Fatalf("final version = %+v", versions[0])
			}
			if n := len(f.Refreshes()); n != 1 {
				t.Errorf("resume left %d refreshes on the group, want 1: a fleet must not be churned twice", n)
			}
			assertNoInstanceMutations(t, f)
		})
	}
}

// §3.3: "Rollback = point template back + refresh again; the ledger stores the
// prior version."
func TestASGRevertPointsTheTemplateBack(t *testing.T) {
	clock := newActClock(actBase)
	f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actASGStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)

	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := a.Revert(t.Context(), as); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	versions := f.Versions()
	if len(versions) != 2 {
		t.Fatalf("the undo created %d extra version(s); it must repoint the recorded prior version", len(versions)-2)
	}
	var def LaunchTemplateVersion
	for _, v := range versions {
		if v.DefaultVersion {
			def = v
		}
	}
	if def.VersionNumber != 1 || def.InstanceType != "m5.2xlarge" {
		t.Fatalf("default version after the undo = %+v, want v1 m5.2xlarge", def)
	}
	if n := f.Count(OpStartInstanceRefresh); n != 2 {
		t.Errorf("the undo started %d refreshes in total, want 2 (one each way)", n)
	}
	inverse := domain.StepKey(step.Target, step.To, step.From)
	rev, ok := a.Entry(inverse)
	if !ok || !rev.Revert || rev.Status != StatusDone {
		t.Fatalf("undo entry = %+v", rev)
	}
	assertNoInstanceMutations(t, f)
}

// A $Latest group has no default to repoint, so the undo creates a version
// carrying the original type. Same destination, longer road, still no churn of
// the instances by hand.
func TestASGRevertOnALatestGroupCreatesAVersion(t *testing.T) {
	clock := newActClock(actBase)
	g := actGroup(func(g *AutoScalingGroup) { g.LaunchTemplate.Version = VersionLatest })
	f := newASGFixture(clock, g, actVersions(nil), nil)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actASGStep(actStepOpts{})
	as := actAuthorized(t, actBase, step)

	if err := a.Execute(t.Context(), as); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := f.Count(OpModifyLaunchTemplate); n != 0 {
		t.Errorf("a $Latest group had its default repointed %d time(s); it does not need it", n)
	}
	if err := a.Revert(t.Context(), as); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	versions := f.Versions()
	if len(versions) != 3 || versions[0].InstanceType != "m5.2xlarge" {
		t.Fatalf("versions after the undo = %+v", versions)
	}
	assertNoInstanceMutations(t, f)
}

// An ASG step presented to an actuator with no Auto Scaling seam is a wiring
// bug, and says so rather than appearing to succeed.
func TestASGWithoutASeamFailsLoudly(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a, err := NewActuator(f, nil, ActuatorConfig{Now: clock.Now, Mode: ModeApply})
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	step := actASGStep(actStepOpts{})
	if err := a.Execute(t.Context(), actAuthorized(t, actBase, step)); !errors.Is(err, ErrNoAutoScalingSeam) {
		t.Fatalf("Execute: err = %v, want ErrNoAutoScalingSeam", err)
	}
	if err := a.Preflight(t.Context(), step); !errors.Is(err, ErrNoAutoScalingSeam) {
		t.Fatalf("Preflight: err = %v, want ErrNoAutoScalingSeam", err)
	}
	e, _ := a.Entry(step.Key)
	if e.Status != StatusFailed {
		t.Errorf("status = %q, want %q", e.Status, StatusFailed)
	}
}

// A template version that shrinks a volume is refused even though this unit
// would never create one: a plan whose recorded intent contradicts execution
// is not auditable.
func TestASGRefusesAShrunkenTemplateVersion(t *testing.T) {
	clock := newActClock(actBase)
	versions := actVersions(nil)
	versions = append(versions, LaunchTemplateVersion{
		LaunchTemplateID: "lt-1", VersionNumber: 2, InstanceType: "m6i.2xlarge", ImageID: "ami-nitro",
		BlockDevices: []BlockDevice{{DeviceName: "/dev/xvda", VolumeType: "gp3", SizeGiB: 40}},
	})
	f := newASGFixture(clock, actGroup(nil), versions, nil)
	a := newActActuator(t, f, clock, ModeApply, nil)
	step := actASGStep(actStepOpts{})
	wantRefusal(t, f, a.Execute(t.Context(), actAuthorized(t, actBase, step)), RefuseStorageShrink)
}

// Preflight is the tokenless read-only gate a report and a --dry-run use. It
// must answer identically to Execute and touch nothing.
func TestASGPreflightNeedsNoApprovalAndTouchesNothing(t *testing.T) {
	clock := newActClock(actBase)
	f := newASGFixture(clock, actGroup(nil), actVersions(nil), nil)
	a := newActActuator(t, f, clock, ModeApply, nil)

	if err := a.Preflight(t.Context(), actASGStep(actStepOpts{})); err != nil {
		t.Fatalf("Preflight on a healthy group: %v", err)
	}
	if n := f.Mutations(); n != 0 {
		t.Fatalf("Preflight issued %d mutating call(s): %v", n, f.Ops())
	}
	wantRefusal(t, f, a.Preflight(t.Context(), actASGStep(actStepOpts{net: "-9"})), RefuseCommitmentNegative)

	bad := actASGStep(actStepOpts{})
	bad.Action = domain.ActionInPlace
	bad.Key = domain.StepKey(bad.Target, bad.From, bad.To)
	wantRefusal(t, f, a.Preflight(t.Context(), bad), RefuseWrongAction)
	if len(f.Versions()) != 1 {
		t.Fatal("Preflight created a launch template version")
	}
}
