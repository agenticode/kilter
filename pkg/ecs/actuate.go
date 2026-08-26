package ecs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// --- Write seam ------------------------------------------------------------

// RegisterTaskDefinitionInput registers a new revision. It takes a whole task
// definition because that is what the API takes: ECS revisions are immutable
// wholes, not patches, so a caller that wants to change cpu/memory must send
// everything else back unchanged. The actuator therefore READS the current
// revision and edits a copy, rather than reconstructing one from a spec — a
// reconstructed task definition silently drops whatever fields the spec has no
// room for, which on ECS includes log configuration, secrets and volumes.
type RegisterTaskDefinitionInput struct {
	TaskDefinition TaskDefinitionRecord `json:"taskDefinition"`
	Tags           []Tag                `json:"tags,omitempty"`
}

// RegisterTaskDefinitionOutput carries the new revision, ARN included.
type RegisterTaskDefinitionOutput struct {
	TaskDefinition TaskDefinitionRecord `json:"taskDefinition"`
}

// UpdateServiceInput re-points a service at a task-definition revision.
//
// It has no desired-count field, and that absence is deliberate: scaling a
// service is its autoscaler's job (§3.4 "Never: change desired count"). A
// struct that cannot express a count cannot accidentally send one — pinned by
// TestUpdateServiceCannotChangeDesiredCount.
type UpdateServiceInput struct {
	Cluster        string `json:"cluster"`
	Service        string `json:"service"`
	TaskDefinition string `json:"taskDefinition"`
}

// UpdateServiceOutput carries the updated service.
type UpdateServiceOutput struct {
	Service ServiceRecord `json:"service"`
}

// MutateAPI is the write seam: exactly two calls, both idempotency-checked by
// the actuator before use. It is separate from [InventoryAPI] so a collector
// can be handed credentials that physically cannot register a revision.
type MutateAPI interface {
	RegisterTaskDefinition(ctx context.Context, in *RegisterTaskDefinitionInput) (*RegisterTaskDefinitionOutput, error)
	UpdateService(ctx context.Context, in *UpdateServiceInput) (*UpdateServiceOutput, error)
}

// --- Refusals --------------------------------------------------------------

// ErrRefused marks a hard gate. Callers match it with errors.Is; the specific
// gate is in [Refusal.Code], which is a stable string.
var ErrRefused = errors.New("ecs: refused")

// Refusal is a stated reason the actuator did not act. It is an error rather
// than a boolean because refusing must fail the step loudly: a gate that
// returned "nothing to do" would be indistinguishable from success.
type Refusal struct {
	Code   string
	Target string
	Reason string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("ecs: refused %s on %s: %s", r.Code, r.Target, r.Reason)
}

// Unwrap makes errors.Is(err, ErrRefused) true for every gate.
func (r *Refusal) Unwrap() error { return ErrRefused }

func refuse(code, target, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Target: target, Reason: fmt.Sprintf(format, args...)}
}

// --- Actuator --------------------------------------------------------------

// ActuatorConfig configures one plan's execution.
type ActuatorConfig struct {
	// Guard is the guardrail context this plan runs under: the decision time,
	// the change windows, freeze and the circuit breaker. It is per-plan
	// configuration rather than a per-call argument because [domain.Actuator]
	// has no room for one and this package has no clock of its own — a zero
	// Guard.Now is refused rather than defaulted.
	Guard domain.Guard
	// DefaultMode is the kilter.dev/mode assumed for a service with no tag.
	// Empty ⇒ apply.
	DefaultMode string
}

// Actuator executes ECS task-size steps: register a new revision, then
// re-point the service at it.
//
// Every Execute re-reads the live service before acting. That is what makes the
// gates real rather than advisory — a plan approved an hour ago says nothing
// about whether a deployment started since — and what makes the step idempotent
// per [domain.Step.Key]: a service already running the target size is a no-op,
// so re-executing a completed step after a controller restart does not register
// a second identical revision.
type Actuator struct {
	inv InventoryAPI
	mut MutateAPI
	cfg ActuatorConfig
}

// NewActuator builds an actuator. Both seams are required; the read seam is not
// optional, because acting without re-reading is exactly the failure the gates
// exist to prevent.
func NewActuator(inv InventoryAPI, mut MutateAPI, cfg ActuatorConfig) (*Actuator, error) {
	if inv == nil {
		return nil, fmt.Errorf("ecs: actuator needs an inventory seam for pre-flight reads")
	}
	if mut == nil {
		return nil, fmt.Errorf("ecs: actuator needs a mutate seam")
	}
	if cfg.Guard.Now.IsZero() {
		return nil, fmt.Errorf("ecs: actuator needs Guard.Now (this package has no clock)")
	}
	return &Actuator{inv: inv, mut: mut, cfg: cfg}, nil
}

// Domain implements domain.Actuator.
func (a *Actuator) Domain() domain.Kind { return Kind }

// Execute applies a task-size step: RegisterTaskDefinition with the new
// cpu/memory, then UpdateService onto the new revision.
//
// Gates, in order, all hard:
//
//  1. freeze, circuit breaker, change window ([domain.Guard.Allow]);
//  2. the step is a rolling task-size change of this domain's shape;
//  3. the service still exists, still runs on Fargate, and is not tagged
//     kilter.dev/mode=off;
//  4. no deployment is in progress — re-pointing a converging service cancels
//     the in-flight rollout and leaves nobody able to say which revision the
//     running tasks came from;
//  5. the service still runs the revision the step recorded as From. Drift
//     means somebody else changed it, and the recorded rollback target would
//     no longer restore what was there;
//  6. the proposed size is a valid Fargate tier that clears the task
//     definition's container-level floors and its platform version.
func (a *Actuator) Execute(ctx context.Context, step domain.Step) error {
	if err := a.cfg.Guard.Allow(); err != nil {
		return err
	}
	if step.Action != domain.ActionRolling {
		return refuse("action-class", step.Target.String(),
			"step action %q; every ECS task-size change is %q", step.Action, domain.ActionRolling)
	}
	cluster, service, err := stepTarget(step)
	if err != nil {
		return err
	}
	svc, err := a.describeService(ctx, cluster, service)
	if err != nil {
		return err
	}
	if err := a.gateService(svc, step); err != nil {
		return err
	}

	toCPU, toMem, err := stepSize(step.To)
	if err != nil {
		return refuse(ReasonInvalidTaskSize, step.Target.String(), "%v", err)
	}
	tier, err := TierFor(model.Resources{MilliCPU: toCPU, MemoryBytes: toMem})
	if err != nil {
		return refuse(ReasonInvalidTaskSize, step.Target.String(),
			"proposed size %s is not a valid Fargate task size", step.To.Resources)
	}

	running := runningTaskDefinition(svc)
	fromARN := step.From.Attr(AttrTaskDefinition)
	if fromARN == "" {
		return refuse(ReasonInvalidTaskSize, step.Target.String(),
			"step records no %s on its From spec, so it has no rollback target and must not be applied", AttrTaskDefinition)
	}

	// Idempotency, derived from live state rather than from a cache: if the
	// service already runs something other than From at the target size, this
	// step has already been applied and re-running it is a no-op.
	if running != fromARN {
		cur, err := a.describeTaskDefinition(ctx, running)
		if err != nil {
			return err
		}
		if res, rerr := cur.Reserved(); rerr == nil && res == tier.Resources() {
			return nil // already applied
		}
		return refuse("revision-drift", step.Target.String(),
			"service runs %s but the step was planned against %s; the recorded rollback target no longer matches reality",
			running, fromARN)
	}

	base, err := a.describeTaskDefinition(ctx, fromARN)
	if err != nil {
		return err
	}
	if err := a.gateShape(base, svc, tier, step); err != nil {
		return err
	}

	next := base
	next.TaskDefinitionARN = ""
	next.Revision = 0
	next.Status = ""
	next.CPU = FormatTaskCPU(tier)
	next.Memory = FormatTaskMemory(tier)

	out, err := a.mut.RegisterTaskDefinition(ctx, &RegisterTaskDefinitionInput{TaskDefinition: next})
	if err != nil {
		return fmt.Errorf("ecs: register task definition for %s: %w", step.Target, err)
	}
	if out == nil || out.TaskDefinition.TaskDefinitionARN == "" {
		return fmt.Errorf("ecs: register task definition for %s: seam returned no revision ARN", step.Target)
	}
	if _, err := a.mut.UpdateService(ctx, &UpdateServiceInput{
		Cluster:        cluster,
		Service:        service,
		TaskDefinition: out.TaskDefinition.TaskDefinitionARN,
	}); err != nil {
		// The revision exists but the service was not re-pointed. That is
		// harmless — an unused revision costs nothing — and it is reported
		// loudly rather than retried silently.
		return fmt.Errorf("ecs: update service %s to %s (revision registered but not deployed): %w",
			step.Target, out.TaskDefinition.TaskDefinitionARN, err)
	}
	return nil
}

// Revert rolls the service back to the revision the step recorded as From.
//
// It registers nothing. The From spec carries the full task-definition ARN
// including its revision number, and ECS revisions are immutable and never
// garbage-collected while a service references them, so the rollback is a
// single UpdateService onto a revision that is already known-good. That is why
// the revision — not just the cpu/memory numbers — is what gets recorded.
//
// The same gates apply as to Execute. A rollback is still a deployment: it
// replaces every task in the service, so it is not exempt from the change
// window or from the deployment-in-progress check.
func (a *Actuator) Revert(ctx context.Context, step domain.Step) error {
	if err := a.cfg.Guard.Allow(); err != nil {
		return err
	}
	if step.Action != domain.ActionRolling {
		return fmt.Errorf("%w: action %q", domain.ErrIrreversible, step.Action)
	}
	fromARN := step.From.Attr(AttrTaskDefinition)
	if fromARN == "" {
		return fmt.Errorf("%w: step %s records no %s to roll back to",
			domain.ErrIrreversible, step.Target, AttrTaskDefinition)
	}
	cluster, service, err := stepTarget(step)
	if err != nil {
		return err
	}
	svc, err := a.describeService(ctx, cluster, service)
	if err != nil {
		return err
	}
	if err := a.gateService(svc, step); err != nil {
		return err
	}
	if runningTaskDefinition(svc) == fromARN {
		return nil // already reverted
	}
	if _, err := a.mut.UpdateService(ctx, &UpdateServiceInput{
		Cluster:        cluster,
		Service:        service,
		TaskDefinition: fromARN,
	}); err != nil {
		return fmt.Errorf("ecs: revert service %s to %s: %w", step.Target, fromARN, err)
	}
	return nil
}

// gateService applies the gates that depend on the live service.
func (a *Actuator) gateService(svc ServiceRecord, step domain.Step) error {
	tgt := step.Target.String()
	if mode := modeFor(tagMap(svc.Tags), a.cfg.DefaultMode); mode == modeOff {
		return refuse(ReasonModeOff, tgt, "service is tagged %s=off", TagKilterMode)
	}
	if !svc.IsFargate() {
		return refuse(ReasonNotFargate, tgt, "service launch type %q is not Fargate", svc.LaunchType)
	}
	if inProgress, why := svc.DeploymentInProgress(); inProgress {
		return refuse(ReasonDeploymentInProgress, tgt, "%s", why)
	}
	return nil
}

// gateShape applies the gates that depend on the task definition being edited.
func (a *Actuator) gateShape(base TaskDefinitionRecord, svc ServiceRecord,
	tier pricing.FargateConfig, step domain.Step) error {

	tgt := step.Target.String()
	if base.NetworkMode != "" && base.NetworkMode != NetworkModeAWSVPC {
		return refuse(ReasonNetworkMode, tgt,
			"task definition uses networkMode %q; Fargate requires %q", base.NetworkMode, NetworkModeAWSVPC)
	}
	if tier.MilliCPU >= 8000 && !platformVersionAtLeast(svc.PlatformVersion, 1, 4) {
		return refuse(ReasonPlatformVersion, tgt,
			"task size %s needs Fargate platform version 1.4.0 or later; the service is pinned to %q",
			tier, svc.PlatformVersion)
	}
	floors := base.ContainerFloors()
	if !tier.Resources().Fits(floors) {
		return refuse(ReasonContainerLimits, tgt,
			"proposed task size %s is below the container-level floor %s; RegisterTaskDefinition would reject it",
			tier, floors)
	}
	return nil
}

// describeService reads one service, failing loudly when it is absent: a step
// against a service that no longer exists must not read as "nothing to do".
func (a *Actuator) describeService(ctx context.Context, cluster, service string) (ServiceRecord, error) {
	out, err := a.inv.DescribeServices(ctx, &DescribeServicesInput{
		Cluster: cluster, Services: []string{service}, IncludeTags: true,
	})
	if err != nil {
		return ServiceRecord{}, fmt.Errorf("ecs: describe service %s/%s: %w", cluster, service, err)
	}
	if out == nil {
		return ServiceRecord{}, fmt.Errorf("ecs: describe service %s/%s: seam returned no result", cluster, service)
	}
	for _, f := range out.Failures {
		return ServiceRecord{}, fmt.Errorf("ecs: describe service %s/%s: %s %s", cluster, service, f.Reason, f.Detail)
	}
	for _, s := range out.Services {
		if s.ServiceName == service || s.ServiceARN == service {
			return s, nil
		}
	}
	return ServiceRecord{}, fmt.Errorf("ecs: service %s/%s not found", cluster, service)
}

func (a *Actuator) describeTaskDefinition(ctx context.Context, arn string) (TaskDefinitionRecord, error) {
	out, err := a.inv.DescribeTaskDefinition(ctx, &DescribeTaskDefinitionInput{TaskDefinition: arn})
	if err != nil {
		return TaskDefinitionRecord{}, fmt.Errorf("ecs: describe task definition %s: %w", arn, err)
	}
	if out == nil {
		return TaskDefinitionRecord{}, fmt.Errorf("ecs: describe task definition %s: seam returned no result", arn)
	}
	return out.TaskDefinition, nil
}

// runningTaskDefinition is the revision the service's tasks actually run: the
// PRIMARY deployment's, falling back to the service's configured one.
func runningTaskDefinition(s ServiceRecord) string {
	if p, ok := s.Primary(); ok && p.TaskDefinition != "" {
		return p.TaskDefinition
	}
	return s.TaskDefinition
}

// stepTarget resolves the cluster and service a step acts on. The spec
// attributes are authoritative when present; the target ID is the fallback, and
// a disagreement between them is a refusal rather than a coin flip.
func stepTarget(step domain.Step) (cluster, service string, err error) {
	cluster, service, idErr := ParseTargetID(step.Target.ID)
	ac, as := step.To.Attr(AttrCluster), step.To.Attr(AttrService)
	if ac == "" && as == "" {
		ac, as = step.From.Attr(AttrCluster), step.From.Attr(AttrService)
	}
	if idErr != nil {
		if ac == "" || as == "" {
			return "", "", refuse("malformed-step", step.Target.String(), "%v", idErr)
		}
		return ac, as, nil
	}
	if (ac != "" && ac != cluster) || (as != "" && as != service) {
		return "", "", refuse("malformed-step", step.Target.String(),
			"step attributes name %s/%s but the target ID names %s/%s", ac, as, cluster, service)
	}
	return cluster, service, nil
}

// stepSize reads the proposed task size out of a spec, preferring the canonical
// Resources and falling back to the task-definition-shaped attributes.
func stepSize(s domain.Spec) (milliCPU, memoryBytes int64, err error) {
	if s.Resources.MilliCPU > 0 && s.Resources.MemoryBytes > 0 {
		return s.Resources.MilliCPU, s.Resources.MemoryBytes, nil
	}
	cpu, memStr := strings.TrimSpace(s.Attr(AttrTaskCPU)), strings.TrimSpace(s.Attr(AttrTaskMemory))
	if cpu == "" || memStr == "" {
		return 0, 0, fmt.Errorf("spec carries neither resources nor %s/%s attributes", AttrTaskCPU, AttrTaskMemory)
	}
	c, err := ParseTaskCPU(cpu)
	if err != nil {
		return 0, 0, err
	}
	m, err := ParseTaskMemory(memStr)
	if err != nil {
		return 0, 0, err
	}
	return c, m, nil
}

// compile-time proof that the actuator satisfies the domain seam.
var _ domain.Actuator = (*Actuator)(nil)
