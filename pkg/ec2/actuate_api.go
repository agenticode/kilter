package ec2

// The U7 actuation seam.
//
// Every AWS operation this unit performs — read or write — arrives through one
// of the interfaces below, over plain Go structs. The package links no AWS
// SDK, opens no socket and reads no credential file; the SDK adapter is a
// mechanical field copy that lives in cmd/ (see ACTUATE-FINDINGS.md §Wiring).
// That is the same arrangement pkg/provider uses for EKS and pkg/ebs uses for
// ModifyVolume, and it is what makes "no live AWS call, including in tests" a
// structural property rather than a promise.
//
// Read and write are DIFFERENT interfaces on purpose:
//
//   - [PreflightAPI] is read-only. Everything that decides whether a resize is
//     safe reads through it, so the decision path can be handed a wiring that
//     physically cannot mutate anything.
//   - [InstanceActuateAPI] and [ASGActuateAPI] add the mutations. Neither is
//     satisfied by a read-only wiring, so a caller cannot pass an observer
//     where an actuator is expected and have it compile.
//
// Field names track the AWS API's, so a recorded fixture reads like the
// response it came from and the adapter is obvious.

import (
	"context"
	"time"
)

// --- pre-flight reads ------------------------------------------------------

// BlockDevice is one entry of an instance's or a launch template version's
// block device mapping.
//
// A stop-resize never touches storage, but a step that *claims* to shrink one
// is refused rather than executed-and-ignored: a plan whose recorded intent
// differs from what execution would do is a plan nobody can audit.
type BlockDevice struct {
	DeviceName string `json:"deviceName"`
	// VolumeID is set for an attached EBS volume, empty in a launch template.
	VolumeID string `json:"volumeId,omitempty"`
	// VirtualName is set for an instance-store (ephemeral) mapping —
	// "ephemeral0", "ephemeral1", … Its presence is a refusal (§3.3 Never).
	VirtualName string `json:"virtualName,omitempty"`
	VolumeType  string `json:"volumeType,omitempty"`
	SizeGiB     int64  `json:"sizeGiB,omitempty"`
	// DeleteOnTermination is recorded but never changed here.
	DeleteOnTermination bool `json:"deleteOnTermination,omitempty"`
}

// Ephemeral reports whether the mapping is instance-store backed.
func (b BlockDevice) Ephemeral() bool { return b.VirtualName != "" }

// Instance lifecycle states, as DescribeInstances reports them.
const (
	StatePending      = "pending"
	StateRunning      = "running"
	StateShuttingDown = "shutting-down"
	StateTerminated   = "terminated"
	StateStopping     = "stopping"
	StateStopped      = "stopped"
)

// Instance-initiated shutdown behaviors, from
// `ec2:DescribeInstanceAttribute(Attribute=instanceInitiatedShutdownBehavior)`.
const (
	ShutdownStop      = "stop"
	ShutdownTerminate = "terminate"
)

// Support levels reported by `ec2:DescribeInstanceTypes` for ENA and NVMe.
const (
	SupportRequired    = "required"
	SupportSupported   = "supported"
	SupportUnsupported = "unsupported"
)

// Root device types.
const (
	RootDeviceEBS           = "ebs"
	RootDeviceInstanceStore = "instance-store"
)

// InstanceDetail is everything the pre-flight reads about one instance.
//
// It is deliberately NOT [InstanceRecord] (collect.go): the collector's record
// carries what a *sizing decision* needs, and this carries what a *stop* needs
// — AMI, ENA flag, shutdown behavior, block devices, lifecycle. Widening the
// collector's type would have made a read-only package carry actuation-only
// fields, and this unit may not edit collect.go in any case.
//
// Source per field:
//
//	ec2:DescribeInstances          — everything except ShutdownBehavior
//	ec2:DescribeInstanceAttribute  — ShutdownBehavior
//	                                 (Attribute=instanceInitiatedShutdownBehavior)
//
// `ec2:DescribeInstanceAttribute` is NOT in the §3.3 IAM block. See
// ACTUATE-FINDINGS.md §IAM: it must be added, and an operator who cannot grant
// it gets an honest refusal, never an assumption.
type InstanceDetail struct {
	InstanceID       string    `json:"instanceId"`
	InstanceType     string    `json:"instanceType"`
	State            string    `json:"state"`
	Architecture     string    `json:"architecture,omitempty"` // x86_64 | arm64
	ImageID          string    `json:"imageId,omitempty"`
	AvailabilityZone string    `json:"availabilityZone,omitempty"`
	Tenancy          string    `json:"tenancy,omitempty"` // default | dedicated | host
	Platform         string    `json:"platform,omitempty"`
	LaunchTime       time.Time `json:"launchTime,omitzero"`
	Tags             []Tag     `json:"tags,omitempty"`

	// EnaSupport is the instance's own enhanced-networking flag. A target
	// generation whose ENA support is "required" cannot boot without it.
	EnaSupport bool `json:"enaSupport,omitempty"`
	// SriovNetSupport is "simple" when the older Intel 82599 VF driver is
	// enabled, empty otherwise. Recorded for the report; ENA is the gate.
	SriovNetSupport string `json:"sriovNetSupport,omitempty"`

	RootDeviceType string        `json:"rootDeviceType,omitempty"`
	BlockDevices   []BlockDevice `json:"blockDevices,omitempty"`
	// InstanceStoreVolumes counts ephemeral volumes the API reported outside
	// the block device mapping. Either signal refuses.
	InstanceStoreVolumes int `json:"instanceStoreVolumes,omitempty"`

	// ShutdownBehavior is "stop", "terminate", or "" when it could not be
	// read. Empty and "terminate" both refuse — §3.3 lists "unknown shutdown
	// behavior" as a refusal, so absence of evidence is not evidence.
	ShutdownBehavior string `json:"shutdownBehavior,omitempty"`
	// HibernationConfigured marks an instance whose stop hibernates rather
	// than shuts down. Its RAM image is pinned to the current shape.
	HibernationConfigured bool `json:"hibernationConfigured,omitempty"`
	// LifecycleType is "spot", "scheduled" or empty for on-demand.
	LifecycleType string `json:"lifecycleType,omitempty"`
	// SpotInstanceRequestID is non-empty on a Spot instance.
	SpotInstanceRequestID string `json:"spotInstanceRequestId,omitempty"`
	// StateTransitionReason is echoed into the ledger on a failure so an
	// operator sees what AWS said, not what this package guessed.
	StateTransitionReason string `json:"stateTransitionReason,omitempty"`
}

// InstanceTypeInfo is the `ec2:DescribeInstanceTypes` record for one type: the
// prerequisites a target generation imposes.
type InstanceTypeInfo struct {
	InstanceType string `json:"instanceType"`
	// ENASupport and NVMeSupport are "required", "supported" or "unsupported".
	// An empty string means the API did not say, which refuses.
	ENASupport  string `json:"enaSupport,omitempty"`
	NVMeSupport string `json:"nvmeSupport,omitempty"`

	SupportedArchitectures       []string `json:"supportedArchitectures,omitempty"`
	SupportedVirtualizationTypes []string `json:"supportedVirtualizationTypes,omitempty"`
	SupportedRootDeviceTypes     []string `json:"supportedRootDeviceTypes,omitempty"`
	SupportedUsageClasses        []string `json:"supportedUsageClasses,omitempty"`

	VCPU      int32 `json:"vcpu,omitempty"`
	MemoryMiB int64 `json:"memoryMiB,omitempty"`

	BareMetal                bool  `json:"bareMetal,omitempty"`
	CurrentGeneration        bool  `json:"currentGeneration,omitempty"`
	HibernationSupported     bool  `json:"hibernationSupported,omitempty"`
	InstanceStorageSupported bool  `json:"instanceStorageSupported,omitempty"`
	InstanceStorageTotalGB   int64 `json:"instanceStorageTotalGB,omitempty"`
	DedicatedHostsSupported  bool  `json:"dedicatedHostsSupported,omitempty"`
}

// ImageDetail is the `ec2:DescribeImages` record for the AMI an instance
// booted from. A target generation's ENA/NVMe prerequisite is satisfied by the
// AMI, not by the instance's current flags alone.
type ImageDetail struct {
	ImageID            string `json:"imageId"`
	State              string `json:"state,omitempty"` // available | pending | …
	Architecture       string `json:"architecture,omitempty"`
	ENASupport         bool   `json:"enaSupport,omitempty"`
	SriovNetSupport    string `json:"sriovNetSupport,omitempty"`
	BootMode           string `json:"bootMode,omitempty"` // uefi | legacy-bios | uefi-preferred
	RootDeviceType     string `json:"rootDeviceType,omitempty"`
	VirtualizationType string `json:"virtualizationType,omitempty"` // hvm | paravirtual
}

// DescribeInstanceDetailInput requests the pre-flight read for one instance.
type DescribeInstanceDetailInput struct {
	InstanceID string `json:"instanceId"`
}

// DescribeInstanceDetailOutput answers it. A nil Instance means "not in this
// account", which refuses.
type DescribeInstanceDetailOutput struct {
	Instance *InstanceDetail `json:"instance,omitempty"`
}

// DescribeInstanceTypeInput requests the prerequisites of one instance type.
type DescribeInstanceTypeInput struct {
	InstanceType string `json:"instanceType"`
}

// DescribeInstanceTypeOutput answers it. A nil Info refuses: an unknown target
// type is never assumed compatible.
type DescribeInstanceTypeOutput struct {
	Info *InstanceTypeInfo `json:"info,omitempty"`
}

// DescribeImageInput requests one AMI.
type DescribeImageInput struct {
	ImageID string `json:"imageId"`
}

// DescribeImageOutput answers it. A nil Image refuses — a deregistered AMI is
// exactly the case where a stopped instance never comes back.
type DescribeImageOutput struct {
	Image *ImageDetail `json:"image,omitempty"`
}

// PreflightAPI is the read-only half of the seam. Nothing reachable through it
// changes anything in the account.
type PreflightAPI interface {
	DescribeInstanceDetail(ctx context.Context, in *DescribeInstanceDetailInput) (*DescribeInstanceDetailOutput, error)
	DescribeInstanceType(ctx context.Context, in *DescribeInstanceTypeInput) (*DescribeInstanceTypeOutput, error)
	DescribeImage(ctx context.Context, in *DescribeImageInput) (*DescribeImageOutput, error)
}

// --- instance mutations ----------------------------------------------------

// StopInstancesInput maps to `ec2:StopInstances`.
//
// Force and Hibernate are present, always false, and never settable from a
// step: a forced stop is a power cut, and hibernation pins a RAM image to the
// shape we are about to change. They exist so the adapter cannot quietly pass
// something else through.
type StopInstancesInput struct {
	InstanceIDs []string `json:"instanceIds"`
	Force       bool     `json:"force,omitempty"`
	Hibernate   bool     `json:"hibernate,omitempty"`
}

// StopInstancesOutput reports the state transition AWS acknowledged.
type StopInstancesOutput struct {
	CurrentState  string `json:"currentState,omitempty"`
	PreviousState string `json:"previousState,omitempty"`
}

// StartInstancesInput maps to `ec2:StartInstances`.
type StartInstancesInput struct {
	InstanceIDs []string `json:"instanceIds"`
}

// StartInstancesOutput reports the acknowledged transition.
type StartInstancesOutput struct {
	CurrentState  string `json:"currentState,omitempty"`
	PreviousState string `json:"previousState,omitempty"`
}

// ModifyInstanceAttributeInput maps to `ec2:ModifyInstanceAttribute`.
//
// Exactly one attribute is modelled: instanceType. The API can also change
// user data, IAM profile association, shutdown behavior, security groups and
// more; none of that is representable here, so no bug in this package can
// reach them. §3.3 forbids changing tenancy or platform and there is no field
// for either.
type ModifyInstanceAttributeInput struct {
	InstanceID   string `json:"instanceId"`
	InstanceType string `json:"instanceType"`
}

// ModifyInstanceAttributeOutput is the (empty) acknowledgement.
type ModifyInstanceAttributeOutput struct{}

// InstanceActuateAPI is the standalone-instance write seam: the read-only
// pre-flight plus the three mutations §3.3 permits. `ec2:TerminateInstances`
// is absent and stays absent — termination is a human concern (§3.3 Never).
type InstanceActuateAPI interface {
	PreflightAPI
	StopInstances(ctx context.Context, in *StopInstancesInput) (*StopInstancesOutput, error)
	StartInstances(ctx context.Context, in *StartInstancesInput) (*StartInstancesOutput, error)
	ModifyInstanceAttribute(ctx context.Context, in *ModifyInstanceAttributeInput) (*ModifyInstanceAttributeOutput, error)
}

// --- Auto Scaling ----------------------------------------------------------

// Launch template version references an ASG may hold.
const (
	VersionDefault = "$Default"
	VersionLatest  = "$Latest"
)

// LaunchTemplateRef is an ASG's pointer at a launch template.
type LaunchTemplateRef struct {
	LaunchTemplateID   string `json:"launchTemplateId,omitempty"`
	LaunchTemplateName string `json:"launchTemplateName,omitempty"`
	// Version is "$Default", "$Latest" or a decimal version number.
	Version string `json:"version,omitempty"`
}

// LaunchTemplateVersion is one `ec2:DescribeLaunchTemplateVersions` record.
type LaunchTemplateVersion struct {
	LaunchTemplateID   string `json:"launchTemplateId"`
	LaunchTemplateName string `json:"launchTemplateName,omitempty"`
	VersionNumber      int64  `json:"versionNumber"`
	DefaultVersion     bool   `json:"defaultVersion,omitempty"`
	InstanceType       string `json:"instanceType,omitempty"`
	ImageID            string `json:"imageId,omitempty"`
	// InstanceRequirements is non-empty when the template selects instances by
	// attribute rather than by type. Setting InstanceType would be ignored.
	InstanceRequirements bool          `json:"instanceRequirements,omitempty"`
	BlockDevices         []BlockDevice `json:"blockDevices,omitempty"`
}

// AutoScalingGroup is the `autoscaling:DescribeAutoScalingGroups` record this
// unit reads.
type AutoScalingGroup struct {
	Name           string             `json:"name"`
	ARN            string             `json:"arn,omitempty"`
	LaunchTemplate *LaunchTemplateRef `json:"launchTemplate,omitempty"`
	// LaunchConfigurationName is set on a legacy ASG. There is no template to
	// version, so this unit refuses.
	LaunchConfigurationName string `json:"launchConfigurationName,omitempty"`
	// MixedInstancesPolicy is true when the group carries one. Its overrides
	// (or its own InstanceRequirements) decide instance types, so editing the
	// template's type changes nothing — a silent no-op that would report
	// success. Refused.
	MixedInstancesPolicy bool `json:"mixedInstancesPolicy,omitempty"`
	// MixedInstancesTemplate is the template reference nested inside a mixed
	// instances policy, recorded so the refusal can name it.
	MixedInstancesTemplate *LaunchTemplateRef `json:"mixedInstancesTemplate,omitempty"`

	MinSize         int32 `json:"minSize"`
	MaxSize         int32 `json:"maxSize"`
	DesiredCapacity int32 `json:"desiredCapacity"`
	// SuspendedProcesses are the scaling processes an operator suspended.
	// A refresh with Launch or Terminate suspended stalls forever.
	SuspendedProcesses []string `json:"suspendedProcesses,omitempty"`
	Tags               []Tag    `json:"tags,omitempty"`
	// InstanceIDs are the group's current members, recorded for the report.
	InstanceIDs []string `json:"instanceIds,omitempty"`
}

// Instance refresh statuses, from `autoscaling:DescribeInstanceRefreshes`.
const (
	RefreshPending            = "Pending"
	RefreshInProgress         = "InProgress"
	RefreshSuccessful         = "Successful"
	RefreshFailed             = "Failed"
	RefreshCancelling         = "Cancelling"
	RefreshCancelled          = "Cancelled"
	RefreshRollbackInProgress = "RollbackInProgress"
	RefreshRollbackFailed     = "RollbackFailed"
	RefreshRollbackSuccessful = "RollbackSuccessful"
	RefreshBaking             = "Baking"
)

// InstanceRefresh is one refresh record.
type InstanceRefresh struct {
	InstanceRefreshID  string    `json:"instanceRefreshId"`
	GroupName          string    `json:"groupName"`
	Status             string    `json:"status"`
	StatusReason       string    `json:"statusReason,omitempty"`
	PercentageComplete int32     `json:"percentageComplete,omitempty"`
	StartTime          time.Time `json:"startTime,omitzero"`
	EndTime            time.Time `json:"endTime,omitzero"`
}

// Active reports whether the refresh still occupies the group. AWS rejects a
// second StartInstanceRefresh while one is active, so this decides "resume"
// versus "issue".
func (r InstanceRefresh) Active() bool {
	switch r.Status {
	case RefreshPending, RefreshInProgress, RefreshBaking, RefreshCancelling, RefreshRollbackInProgress:
		return true
	}
	return false
}

// Terminal reports whether the refresh has finished, in any outcome.
func (r InstanceRefresh) Terminal() bool { return !r.Active() && r.Status != "" }

// RolledBack reports whether the refresh's own rollback ran. A rolled-back
// refresh is NOT a success: the group is back on the old template version and
// the step must say so rather than claim the resize landed.
func (r InstanceRefresh) RolledBack() bool {
	return r.Status == RefreshRollbackSuccessful || r.Status == RefreshRollbackFailed
}

// DescribeAutoScalingGroupInput requests one group.
type DescribeAutoScalingGroupInput struct {
	GroupName string `json:"groupName"`
}

// DescribeAutoScalingGroupOutput answers it; a nil Group refuses.
type DescribeAutoScalingGroupOutput struct {
	Group *AutoScalingGroup `json:"group,omitempty"`
}

// DescribeLaunchTemplateVersionsInput requests template versions. An empty
// Versions list means "$Default and $Latest".
type DescribeLaunchTemplateVersionsInput struct {
	LaunchTemplateID   string   `json:"launchTemplateId,omitempty"`
	LaunchTemplateName string   `json:"launchTemplateName,omitempty"`
	Versions           []string `json:"versions,omitempty"`
}

// DescribeLaunchTemplateVersionsOutput answers it.
type DescribeLaunchTemplateVersionsOutput struct {
	Versions []LaunchTemplateVersion `json:"versions,omitempty"`
}

// CreateLaunchTemplateVersionInput maps to `ec2:CreateLaunchTemplateVersion`.
//
// SourceVersion is always set: the new version is a copy of a known one with
// exactly one field changed. ClientToken is derived from the step key, so a
// retry after a lost response creates no second version.
type CreateLaunchTemplateVersionInput struct {
	LaunchTemplateID   string `json:"launchTemplateId,omitempty"`
	LaunchTemplateName string `json:"launchTemplateName,omitempty"`
	SourceVersion      string `json:"sourceVersion"`
	InstanceType       string `json:"instanceType"`
	VersionDescription string `json:"versionDescription,omitempty"`
	ClientToken        string `json:"clientToken"`
}

// CreateLaunchTemplateVersionOutput reports the version created.
type CreateLaunchTemplateVersionOutput struct {
	Version LaunchTemplateVersion `json:"version"`
}

// ModifyLaunchTemplateInput maps to `ec2:ModifyLaunchTemplate`. The only
// modelled change is which version is the default.
type ModifyLaunchTemplateInput struct {
	LaunchTemplateID   string `json:"launchTemplateId,omitempty"`
	LaunchTemplateName string `json:"launchTemplateName,omitempty"`
	DefaultVersion     string `json:"defaultVersion"`
	ClientToken        string `json:"clientToken"`
}

// ModifyLaunchTemplateOutput is the acknowledgement.
type ModifyLaunchTemplateOutput struct {
	DefaultVersion int64 `json:"defaultVersion"`
}

// StartInstanceRefreshInput maps to `autoscaling:StartInstanceRefresh`.
//
// The preferences this unit sets are the conservative ones and the only ones
// representable: a minimum healthy percentage, an instance warmup, and AWS's
// own automatic rollback. There is no field for MaxHealthyPercentage, no
// SkipMatching override and no CheckpointPercentages — a refresh this package
// starts is always the slow, safe kind.
type StartInstanceRefreshInput struct {
	GroupName string `json:"groupName"`
	// MinHealthyPercentage is the fraction of the group that must stay in
	// service. AWS's default is 90.
	MinHealthyPercentage int32 `json:"minHealthyPercentage"`
	// InstanceWarmupSeconds is how long a replacement is given before it
	// counts as healthy.
	InstanceWarmupSeconds int32 `json:"instanceWarmupSeconds,omitempty"`
	// AutoRollback asks AWS to restore the previous configuration itself when
	// the refresh fails. It is always true.
	AutoRollback bool `json:"autoRollback"`
	// DesiredConfigurationVersion pins the refresh to the launch template
	// version this step created, rather than to whatever $Latest becomes.
	DesiredConfigurationVersion string `json:"desiredConfigurationVersion,omitempty"`
	ClientToken                 string `json:"clientToken"`
}

// StartInstanceRefreshOutput reports the refresh AWS started.
type StartInstanceRefreshOutput struct {
	InstanceRefreshID string `json:"instanceRefreshId"`
}

// DescribeInstanceRefreshesInput requests a group's refreshes, newest first.
type DescribeInstanceRefreshesInput struct {
	GroupName string `json:"groupName"`
	// InstanceRefreshIDs narrows the read to one refresh.
	InstanceRefreshIDs []string `json:"instanceRefreshIds,omitempty"`
}

// DescribeInstanceRefreshesOutput answers it, newest first.
type DescribeInstanceRefreshesOutput struct {
	Refreshes []InstanceRefresh `json:"refreshes,omitempty"`
}

// ASGPreflightAPI is the read-only Auto Scaling seam.
type ASGPreflightAPI interface {
	PreflightAPI
	DescribeAutoScalingGroup(ctx context.Context, in *DescribeAutoScalingGroupInput) (*DescribeAutoScalingGroupOutput, error)
	DescribeLaunchTemplateVersions(ctx context.Context, in *DescribeLaunchTemplateVersionsInput) (*DescribeLaunchTemplateVersionsOutput, error)
	DescribeInstanceRefreshes(ctx context.Context, in *DescribeInstanceRefreshesInput) (*DescribeInstanceRefreshesOutput, error)
}

// ASGActuateAPI is the Auto Scaling write seam.
//
// `autoscaling:UpdateAutoScalingGroup` is absent: changing desired capacity,
// min or max is not rightsizing, and `autoscaling:TerminateInstanceInAutoScalingGroup`
// is absent for the same reason `ec2:TerminateInstances` is. The group's own
// instances are never mutated directly (§3.3 "Never touch ... unless acting on
// the ASG itself") — the only lever is the template plus a refresh.
type ASGActuateAPI interface {
	ASGPreflightAPI
	CreateLaunchTemplateVersion(ctx context.Context, in *CreateLaunchTemplateVersionInput) (*CreateLaunchTemplateVersionOutput, error)
	ModifyLaunchTemplate(ctx context.Context, in *ModifyLaunchTemplateInput) (*ModifyLaunchTemplateOutput, error)
	StartInstanceRefresh(ctx context.Context, in *StartInstanceRefreshInput) (*StartInstanceRefreshOutput, error)
}
