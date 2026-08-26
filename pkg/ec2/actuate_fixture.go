package ec2

// ActuateFixture: the account this unit is tested against.
//
// It implements [InstanceActuateAPI] and [ASGActuateAPI] over in-memory data
// and enforces the rules AWS enforces — ModifyInstanceAttribute fails on a
// running instance, a stop takes time to settle, a second instance refresh is
// rejected while one is active. Tests that pass here exercise the production
// code path verbatim; there is no second implementation for tests to agree
// with by convention.
//
// It is also the reason "no live AWS call, including in tests" is structural
// rather than aspirational: this package links no SDK, so there is nothing for
// a test to accidentally reach.
//
// Fault injection is a pair of hooks rather than a table of flags:
//
//	Fail      — consulted BEFORE the effect. The call fails, nothing changes.
//	FailAfter — consulted AFTER the effect. The call fails, the change stuck.
//	            This is the lost-response case, and it is the one that leaves
//	            an instance stopped with nobody knowing.
//
// Both are called with the operation name and a 1-based per-operation counter,
// so a test can say "fail the second StartInstances" precisely.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Operation names passed to [ActuateFixture.Fail] and
// [ActuateFixture.FailAfter]. They are the AWS action names, lower-cased at
// the first letter, so a test reads like the API it stands in for.
const (
	OpDescribeInstance        = "DescribeInstances"
	OpDescribeInstanceType    = "DescribeInstanceTypes"
	OpDescribeImage           = "DescribeImages"
	OpStopInstances           = "StopInstances"
	OpStartInstances          = "StartInstances"
	OpModifyInstanceAttribute = "ModifyInstanceAttribute"

	OpDescribeASG                   = "DescribeAutoScalingGroups"
	OpDescribeLaunchTemplateVersion = "DescribeLaunchTemplateVersions"
	OpDescribeInstanceRefreshes     = "DescribeInstanceRefreshes"
	OpCreateLaunchTemplateVersion   = "CreateLaunchTemplateVersion"
	OpModifyLaunchTemplate          = "ModifyLaunchTemplate"
	OpStartInstanceRefresh          = "StartInstanceRefresh"
)

// ActuateFixture is a fake EC2 + Auto Scaling account.
type ActuateFixture struct {
	// Now is the fixture's clock. Required for refresh timestamps.
	Now func() time.Time

	// SettleAfter is how many DescribeInstances calls an instance spends in
	// "stopping" or "pending" before it settles. Zero settles on the next
	// describe, which is the fast path most tests want.
	SettleAfter int
	// RefreshSettleAfter is the same for an instance refresh.
	RefreshSettleAfter int
	// RefreshOutcome is the terminal status a started refresh reaches.
	// Empty means [RefreshSuccessful].
	RefreshOutcome string
	// RefreshStatusReason is echoed on a non-successful outcome.
	RefreshStatusReason string

	// Fail is consulted before an operation's effect; a non-nil return fails
	// the call and changes nothing.
	Fail func(op string, n int) error
	// FailAfter is consulted after the effect has been applied — the lost
	// response case.
	FailAfter func(op string, n int) error

	mu       sync.Mutex
	insts    map[string]*InstanceDetail
	types    map[string]InstanceTypeInfo
	images   map[string]ImageDetail
	groups   map[string]*AutoScalingGroup
	versions []LaunchTemplateVersion
	refresh  []InstanceRefresh
	tokens   map[string]int64 // CreateLaunchTemplateVersion idempotency
	counts   map[string]int
	settle   map[string]int
	ops      []string
}

// NewActuateFixture builds a fixture. Instances, types and images are copied,
// so a caller's slice cannot mutate the account behind the test's back.
func NewActuateFixture(now func() time.Time, insts []InstanceDetail, types []InstanceTypeInfo, images []ImageDetail) *ActuateFixture {
	f := &ActuateFixture{
		Now:    now,
		insts:  map[string]*InstanceDetail{},
		types:  map[string]InstanceTypeInfo{},
		images: map[string]ImageDetail{},
		groups: map[string]*AutoScalingGroup{},
		tokens: map[string]int64{},
		counts: map[string]int{},
		settle: map[string]int{},
	}
	for i := range insts {
		cp := insts[i]
		f.insts[cp.InstanceID] = &cp
	}
	for _, t := range types {
		f.types[strings.ToLower(t.InstanceType)] = t
	}
	for _, im := range images {
		f.images[im.ImageID] = im
	}
	return f
}

// WithASG adds an Auto Scaling group, its launch template versions and any
// pre-existing refreshes.
func (f *ActuateFixture) WithASG(g AutoScalingGroup, versions []LaunchTemplateVersion, refreshes []InstanceRefresh) *ActuateFixture {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := g
	f.groups[g.Name] = &cp
	f.versions = append(f.versions, versions...)
	f.refresh = append(f.refresh, refreshes...)
	return f
}

// Instance returns a copy of one instance's live state.
func (f *ActuateFixture) Instance(id string) (InstanceDetail, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.insts[id]
	if !ok {
		return InstanceDetail{}, false
	}
	return *d, true
}

// SetInstance replaces one instance's live state — the seam a test uses to
// simulate somebody else moving an instance mid-flight.
func (f *ActuateFixture) SetInstance(d InstanceDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := d
	f.insts[d.InstanceID] = &cp
}

// Versions returns the launch template versions, newest first.
func (f *ActuateFixture) Versions() []LaunchTemplateVersion {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]LaunchTemplateVersion, len(f.versions))
	copy(out, f.versions)
	sort.SliceStable(out, func(i, j int) bool { return out[i].VersionNumber > out[j].VersionNumber })
	return out
}

// Refreshes returns the instance refreshes that exist in the account.
func (f *ActuateFixture) Refreshes() []InstanceRefresh {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InstanceRefresh, len(f.refresh))
	copy(out, f.refresh)
	return out
}

// Ops returns the operation names issued so far, in order.
func (f *ActuateFixture) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.ops))
	copy(out, f.ops)
	return out
}

// Count returns how many times an operation was called.
func (f *ActuateFixture) Count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

// Mutations counts every call that can change the account. A test asserting
// "this refusal touched nothing" asserts on this.
func (f *ActuateFixture) Mutations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, op := range []string{
		OpStopInstances, OpStartInstances, OpModifyInstanceAttribute,
		OpCreateLaunchTemplateVersion, OpModifyLaunchTemplate, OpStartInstanceRefresh,
	} {
		n += f.counts[op]
	}
	return n
}

// enter records the call and runs the pre-effect fault hook. Caller holds no
// lock; enter takes and releases it.
func (f *ActuateFixture) enter(ctx context.Context, op string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.counts[op]++
	n := f.counts[op]
	f.ops = append(f.ops, op)
	fail := f.Fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(op, n); err != nil {
			return n, err
		}
	}
	return n, nil
}

// leave runs the post-effect fault hook.
func (f *ActuateFixture) leave(op string, n int) error {
	f.mu.Lock()
	after := f.FailAfter
	f.mu.Unlock()
	if after == nil {
		return nil
	}
	return after(op, n)
}

func (f *ActuateFixture) now() time.Time {
	if f.Now == nil {
		return time.Time{}
	}
	return f.Now()
}

// --- PreflightAPI ----------------------------------------------------------

// DescribeInstanceDetail also advances transient states: an instance that has
// been "stopping" for SettleAfter describes becomes "stopped". That models
// AWS's asynchrony and is what makes the polling path real.
func (f *ActuateFixture) DescribeInstanceDetail(ctx context.Context, in *DescribeInstanceDetailInput) (*DescribeInstanceDetailOutput, error) {
	n, err := f.enter(ctx, OpDescribeInstance)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	d, ok := f.insts[in.InstanceID]
	if ok {
		switch d.State {
		case StateStopping, StatePending:
			f.settle[in.InstanceID]++
			if f.settle[in.InstanceID] > f.SettleAfter {
				if d.State == StateStopping {
					d.State = StateStopped
				} else {
					d.State = StateRunning
				}
				f.settle[in.InstanceID] = 0
			}
		}
	}
	var cp *InstanceDetail
	if ok {
		v := *d
		cp = &v
	}
	f.mu.Unlock()
	if err := f.leave(OpDescribeInstance, n); err != nil {
		return nil, err
	}
	return &DescribeInstanceDetailOutput{Instance: cp}, nil
}

func (f *ActuateFixture) DescribeInstanceType(ctx context.Context, in *DescribeInstanceTypeInput) (*DescribeInstanceTypeOutput, error) {
	n, err := f.enter(ctx, OpDescribeInstanceType)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	t, ok := f.types[strings.ToLower(in.InstanceType)]
	f.mu.Unlock()
	if err := f.leave(OpDescribeInstanceType, n); err != nil {
		return nil, err
	}
	if !ok {
		return &DescribeInstanceTypeOutput{}, nil
	}
	return &DescribeInstanceTypeOutput{Info: &t}, nil
}

func (f *ActuateFixture) DescribeImage(ctx context.Context, in *DescribeImageInput) (*DescribeImageOutput, error) {
	n, err := f.enter(ctx, OpDescribeImage)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	im, ok := f.images[in.ImageID]
	f.mu.Unlock()
	if err := f.leave(OpDescribeImage, n); err != nil {
		return nil, err
	}
	if !ok {
		return &DescribeImageOutput{}, nil
	}
	return &DescribeImageOutput{Image: &im}, nil
}

// --- InstanceActuateAPI ----------------------------------------------------

func (f *ActuateFixture) StopInstances(ctx context.Context, in *StopInstancesInput) (*StopInstancesOutput, error) {
	n, err := f.enter(ctx, OpStopInstances)
	if err != nil {
		return nil, err
	}
	if in.Force || in.Hibernate {
		return nil, fmt.Errorf("fixture: this unit must never force or hibernate a stop")
	}
	f.mu.Lock()
	var prev, cur string
	for _, id := range in.InstanceIDs {
		d, ok := f.insts[id]
		if !ok {
			f.mu.Unlock()
			return nil, fmt.Errorf("fixture: InvalidInstanceID.NotFound: %s", id)
		}
		prev = d.State
		switch d.State {
		case StateRunning:
			d.State = StateStopping
			f.settle[id] = 0
		case StateStopping, StateStopped:
			// Idempotent, exactly as StopInstances is.
		default:
			f.mu.Unlock()
			return nil, fmt.Errorf("fixture: IncorrectInstanceState: %s is %s", id, d.State)
		}
		cur = d.State
	}
	f.mu.Unlock()
	if err := f.leave(OpStopInstances, n); err != nil {
		return nil, err
	}
	return &StopInstancesOutput{PreviousState: prev, CurrentState: cur}, nil
}

func (f *ActuateFixture) StartInstances(ctx context.Context, in *StartInstancesInput) (*StartInstancesOutput, error) {
	n, err := f.enter(ctx, OpStartInstances)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	var prev, cur string
	for _, id := range in.InstanceIDs {
		d, ok := f.insts[id]
		if !ok {
			f.mu.Unlock()
			return nil, fmt.Errorf("fixture: InvalidInstanceID.NotFound: %s", id)
		}
		prev = d.State
		switch d.State {
		case StateStopped:
			d.State = StatePending
			f.settle[id] = 0
		case StatePending, StateRunning:
			// Idempotent.
		default:
			f.mu.Unlock()
			return nil, fmt.Errorf("fixture: IncorrectInstanceState: %s is %s", id, d.State)
		}
		cur = d.State
	}
	f.mu.Unlock()
	if err := f.leave(OpStartInstances, n); err != nil {
		return nil, err
	}
	return &StartInstancesOutput{PreviousState: prev, CurrentState: cur}, nil
}

// ModifyInstanceAttribute enforces the rule that makes the whole stop-start
// dance necessary: the instance must be stopped.
func (f *ActuateFixture) ModifyInstanceAttribute(ctx context.Context, in *ModifyInstanceAttributeInput) (*ModifyInstanceAttributeOutput, error) {
	n, err := f.enter(ctx, OpModifyInstanceAttribute)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	d, ok := f.insts[in.InstanceID]
	if !ok {
		f.mu.Unlock()
		return nil, fmt.Errorf("fixture: InvalidInstanceID.NotFound: %s", in.InstanceID)
	}
	if d.State != StateStopped {
		state := d.State
		f.mu.Unlock()
		return nil, fmt.Errorf("fixture: IncorrectInstanceState: %s is %s, ModifyInstanceAttribute(instanceType) requires stopped",
			in.InstanceID, state)
	}
	if _, known := f.types[strings.ToLower(in.InstanceType)]; !known {
		f.mu.Unlock()
		return nil, fmt.Errorf("fixture: InvalidParameterValue: unknown instance type %q", in.InstanceType)
	}
	d.InstanceType = in.InstanceType
	f.mu.Unlock()
	if err := f.leave(OpModifyInstanceAttribute, n); err != nil {
		return nil, err
	}
	return &ModifyInstanceAttributeOutput{}, nil
}

// --- ASGPreflightAPI -------------------------------------------------------

func (f *ActuateFixture) DescribeAutoScalingGroup(ctx context.Context, in *DescribeAutoScalingGroupInput) (*DescribeAutoScalingGroupOutput, error) {
	n, err := f.enter(ctx, OpDescribeASG)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	g, ok := f.groups[in.GroupName]
	var cp *AutoScalingGroup
	if ok {
		v := *g
		cp = &v
	}
	f.mu.Unlock()
	if err := f.leave(OpDescribeASG, n); err != nil {
		return nil, err
	}
	return &DescribeAutoScalingGroupOutput{Group: cp}, nil
}

func (f *ActuateFixture) DescribeLaunchTemplateVersions(ctx context.Context, in *DescribeLaunchTemplateVersionsInput) (*DescribeLaunchTemplateVersionsOutput, error) {
	n, err := f.enter(ctx, OpDescribeLaunchTemplateVersion)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	out := make([]LaunchTemplateVersion, 0, len(f.versions))
	for _, v := range f.versions {
		if in.LaunchTemplateID != "" && v.LaunchTemplateID != in.LaunchTemplateID {
			continue
		}
		if in.LaunchTemplateName != "" && v.LaunchTemplateName != in.LaunchTemplateName {
			continue
		}
		out = append(out, v)
	}
	f.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].VersionNumber > out[j].VersionNumber })
	if err := f.leave(OpDescribeLaunchTemplateVersion, n); err != nil {
		return nil, err
	}
	return &DescribeLaunchTemplateVersionsOutput{Versions: out}, nil
}

// DescribeInstanceRefreshes advances an active refresh toward its outcome, the
// same way DescribeInstanceDetail advances a transient instance state.
func (f *ActuateFixture) DescribeInstanceRefreshes(ctx context.Context, in *DescribeInstanceRefreshesInput) (*DescribeInstanceRefreshesOutput, error) {
	n, err := f.enter(ctx, OpDescribeInstanceRefreshes)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	out := make([]InstanceRefresh, 0, len(f.refresh))
	for i := range f.refresh {
		r := &f.refresh[i]
		if in.GroupName != "" && r.GroupName != in.GroupName {
			continue
		}
		if r.Active() {
			key := "refresh:" + r.InstanceRefreshID
			f.settle[key]++
			if f.settle[key] > f.RefreshSettleAfter {
				r.Status = cmpOr(f.RefreshOutcome, RefreshSuccessful)
				r.StatusReason = f.RefreshStatusReason
				r.PercentageComplete = 100
				r.EndTime = f.now()
				if r.RolledBack() || r.Status == RefreshFailed || r.Status == RefreshCancelled {
					r.PercentageComplete = 0
				}
			}
		}
		out = append(out, *r)
	}
	f.mu.Unlock()
	if err := f.leave(OpDescribeInstanceRefreshes, n); err != nil {
		return nil, err
	}
	return &DescribeInstanceRefreshesOutput{Refreshes: out}, nil
}

// --- ASGActuateAPI ---------------------------------------------------------

func (f *ActuateFixture) CreateLaunchTemplateVersion(ctx context.Context, in *CreateLaunchTemplateVersionInput) (*CreateLaunchTemplateVersionOutput, error) {
	n, err := f.enter(ctx, OpCreateLaunchTemplateVersion)
	if err != nil {
		return nil, err
	}
	if in.ClientToken == "" {
		return nil, fmt.Errorf("fixture: CreateLaunchTemplateVersion without a client token is not idempotent")
	}
	f.mu.Lock()
	// Idempotency: the same client token returns the same version, never a
	// second one. This is what makes a retry after a lost response safe.
	if v, ok := f.tokens[in.ClientToken]; ok {
		for _, ver := range f.versions {
			if ver.VersionNumber == v {
				cp := ver
				f.mu.Unlock()
				if err := f.leave(OpCreateLaunchTemplateVersion, n); err != nil {
					return nil, err
				}
				return &CreateLaunchTemplateVersionOutput{Version: cp}, nil
			}
		}
	}
	var src *LaunchTemplateVersion
	max := int64(0)
	for i := range f.versions {
		v := f.versions[i]
		if in.LaunchTemplateID != "" && v.LaunchTemplateID != in.LaunchTemplateID {
			continue
		}
		if in.LaunchTemplateName != "" && v.LaunchTemplateName != in.LaunchTemplateName {
			continue
		}
		if v.VersionNumber > max {
			max = v.VersionNumber
		}
		if strconv.FormatInt(v.VersionNumber, 10) == in.SourceVersion {
			cp := v
			src = &cp
		}
	}
	if src == nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("fixture: InvalidLaunchTemplateId.VersionNotFound: %s", in.SourceVersion)
	}
	nv := *src
	nv.VersionNumber = max + 1
	nv.DefaultVersion = false
	nv.InstanceType = in.InstanceType
	nv.BlockDevices = append([]BlockDevice(nil), src.BlockDevices...)
	f.versions = append(f.versions, nv)
	f.tokens[in.ClientToken] = nv.VersionNumber
	f.mu.Unlock()
	if err := f.leave(OpCreateLaunchTemplateVersion, n); err != nil {
		return nil, err
	}
	return &CreateLaunchTemplateVersionOutput{Version: nv}, nil
}

func (f *ActuateFixture) ModifyLaunchTemplate(ctx context.Context, in *ModifyLaunchTemplateInput) (*ModifyLaunchTemplateOutput, error) {
	n, err := f.enter(ctx, OpModifyLaunchTemplate)
	if err != nil {
		return nil, err
	}
	want, err := strconv.ParseInt(in.DefaultVersion, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("fixture: InvalidParameterValue: default version %q", in.DefaultVersion)
	}
	f.mu.Lock()
	found := false
	for i := range f.versions {
		v := &f.versions[i]
		if in.LaunchTemplateID != "" && v.LaunchTemplateID != in.LaunchTemplateID {
			continue
		}
		if in.LaunchTemplateName != "" && v.LaunchTemplateName != in.LaunchTemplateName {
			continue
		}
		v.DefaultVersion = v.VersionNumber == want
		if v.DefaultVersion {
			found = true
		}
	}
	f.mu.Unlock()
	if !found {
		return nil, fmt.Errorf("fixture: InvalidLaunchTemplateId.VersionNotFound: %d", want)
	}
	if err := f.leave(OpModifyLaunchTemplate, n); err != nil {
		return nil, err
	}
	return &ModifyLaunchTemplateOutput{DefaultVersion: want}, nil
}

// StartInstanceRefresh rejects a second refresh while one is active, exactly
// as AWS does.
func (f *ActuateFixture) StartInstanceRefresh(ctx context.Context, in *StartInstanceRefreshInput) (*StartInstanceRefreshOutput, error) {
	n, err := f.enter(ctx, OpStartInstanceRefresh)
	if err != nil {
		return nil, err
	}
	if !in.AutoRollback {
		return nil, fmt.Errorf("fixture: this unit must always request auto rollback")
	}
	if in.MinHealthyPercentage <= 0 || in.MinHealthyPercentage > 100 {
		return nil, fmt.Errorf("fixture: ValidationError: minHealthyPercentage %d", in.MinHealthyPercentage)
	}
	f.mu.Lock()
	for _, r := range f.refresh {
		if r.GroupName == in.GroupName && r.Active() {
			f.mu.Unlock()
			return nil, fmt.Errorf("fixture: InstanceRefreshInProgress: %s", r.InstanceRefreshID)
		}
	}
	id := fmt.Sprintf("refresh-%s-%d", in.GroupName, len(f.refresh)+1)
	f.refresh = append(f.refresh, InstanceRefresh{
		InstanceRefreshID: id, GroupName: in.GroupName,
		Status: RefreshInProgress, StartTime: f.now(),
	})
	f.mu.Unlock()
	if err := f.leave(OpStartInstanceRefresh, n); err != nil {
		return nil, err
	}
	return &StartInstanceRefreshOutput{InstanceRefreshID: id}, nil
}

var (
	_ InstanceActuateAPI = (*ActuateFixture)(nil)
	_ ASGActuateAPI      = (*ActuateFixture)(nil)
)
