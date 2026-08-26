package ecs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
)

// planStep runs the whole pipeline — assess, project, plan — and returns the
// single step it produces, so the actuator tests act on exactly what the sizer
// would hand a controller.
func planStep(t *testing.T, f *fixture) domain.Step {
	t.Helper()
	rep := NewSizer(testConfig()).Report(f.snapshot(), testNow, nil)
	steps, err := PlanSteps(rep.Recommendations(), domain.Guard{Now: testNow})
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("planned %d steps, want 1", len(steps))
	}
	return steps[0]
}

func newActuator(t *testing.T, api *fakeAPI, mods ...func(*ActuatorConfig)) *Actuator {
	t.Helper()
	cfg := ActuatorConfig{Guard: domain.Guard{Now: testNow}}
	for _, m := range mods {
		m(&cfg)
	}
	a, err := NewActuator(api, api, cfg)
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	return a
}

// TestExecuteRegistersRevisionThenUpdatesService is the happy path, asserted
// field by field: the new revision changes cpu/memory and NOTHING else, and the
// service is re-pointed at it.
func TestExecuteRegistersRevisionThenUpdatesService(t *testing.T) {
	f := newFixture(func(f *fixture) {
		f.containers = []ContainerDefinition{{Name: "app", MemoryReservation: 512}}
	})
	step := planStep(t, f)
	api := newFakeAPI(f)
	act := newActuator(t, api)

	if err := act.Execute(context.Background(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(api.registered) != 1 {
		t.Fatalf("registered %d revisions, want 1", len(api.registered))
	}
	got := api.registered[0].TaskDefinition
	if got.CPU != "2048" || got.Memory != "4096" {
		t.Fatalf("registered cpu=%q memory=%q, want 2048/4096 (2 vCPU / 4 GB)", got.CPU, got.Memory)
	}
	// Everything else must survive: a revision is an immutable whole, and a
	// reconstructed one silently drops fields the spec has no room for.
	base := f.taskDef()
	if got.Family != base.Family || got.NetworkMode != base.NetworkMode ||
		!reflect.DeepEqual(got.ContainerDefinitions, base.ContainerDefinitions) ||
		got.RuntimePlatform != base.RuntimePlatform ||
		!reflect.DeepEqual(got.RequiresCompatibilities, base.RequiresCompatibilities) {
		t.Fatalf("registered revision lost fields:\n got %+v\nwant %+v", got, base)
	}
	if got.TaskDefinitionARN != "" || got.Revision != 0 || got.Status != "" {
		t.Errorf("registered revision carries server-assigned fields: arn=%q rev=%d status=%q",
			got.TaskDefinitionARN, got.Revision, got.Status)
	}

	if len(api.updated) != 1 {
		t.Fatalf("called UpdateService %d times, want 1", len(api.updated))
	}
	u := api.updated[0]
	if u.Cluster != testCluster || u.Service != testService {
		t.Errorf("UpdateService targeted %s/%s", u.Cluster, u.Service)
	}
	if u.TaskDefinition == testTDARN || !strings.HasSuffix(u.TaskDefinition, ":8") {
		t.Errorf("UpdateService pointed at %q, want the newly registered revision 8", u.TaskDefinition)
	}
}

// TestUpdateServiceCannotChangeDesiredCount pins §3.4's "never change desired
// count" in the type system: the input struct has no field that could carry
// one, so no bug can send one.
func TestUpdateServiceCannotChangeDesiredCount(t *testing.T) {
	rt := reflect.TypeOf(UpdateServiceInput{})
	for i := range rt.NumField() {
		n := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(n, "count") || strings.Contains(n, "desired") ||
			strings.Contains(n, "capacityprovider") || strings.Contains(n, "scal") {
			t.Errorf("UpdateServiceInput gained field %q: scaling belongs to the service's autoscaler",
				rt.Field(i).Name)
		}
	}
	if rt.NumField() != 3 {
		t.Errorf("UpdateServiceInput has %d fields; it should stay at cluster/service/taskDefinition", rt.NumField())
	}
}

// TestRevertRestoresTheRecordedRevision is the rollback contract: Revert points
// the service back at the exact revision the step recorded as From, and
// registers nothing new to do it.
func TestRevertRestoresTheRecordedRevision(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	if got := step.From.Attr(AttrTaskDefinition); got != testTDARN {
		t.Fatalf("step recorded From revision %q, want %q", got, testTDARN)
	}
	api := newFakeAPI(f)
	act := newActuator(t, api)

	if err := act.Execute(context.Background(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	newARN := api.updated[0].TaskDefinition
	if api.services[testService].TaskDefinition != newARN {
		t.Fatal("the fake did not move the service onto the new revision")
	}

	if err := act.Revert(context.Background(), step); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if len(api.registered) != 1 {
		t.Fatalf("Revert registered %d extra revisions; a rollback re-points, it does not re-register",
			len(api.registered)-1)
	}
	if len(api.updated) != 2 {
		t.Fatalf("called UpdateService %d times, want 2", len(api.updated))
	}
	if got := api.updated[1].TaskDefinition; got != testTDARN {
		t.Fatalf("reverted to %q, want the recorded From revision %q", got, testTDARN)
	}
	if got := api.services[testService].TaskDefinition; got != testTDARN {
		t.Fatalf("service left on %q after revert", got)
	}

	// And reverting twice is a no-op, not a second deployment.
	if err := act.Revert(context.Background(), step); err != nil {
		t.Fatalf("second Revert: %v", err)
	}
	if len(api.updated) != 2 {
		t.Fatalf("a redundant revert issued another UpdateService (%d total)", len(api.updated))
	}
}

// TestRevertWithoutARecordedRevisionIsIrreversible: honest undo. A step that
// never recorded where it came from cannot claim to restore it.
func TestRevertWithoutARecordedRevisionIsIrreversible(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	step.From = domain.Spec{Resources: step.From.Resources} // drop the attributes
	api := newFakeAPI(f)
	act := newActuator(t, api)

	err := act.Revert(context.Background(), step)
	if !errors.Is(err, domain.ErrIrreversible) {
		t.Fatalf("Revert = %v, want ErrIrreversible", err)
	}
	if len(api.updated) != 0 {
		t.Fatal("a step with no recorded From still issued an UpdateService")
	}
}

// TestExecuteRefusesDeploymentInProgress: the gate this unit was asked for.
func TestExecuteRefusesDeploymentInProgress(t *testing.T) {
	f := newFixture()
	step := planStep(t, f) // planned while the service was healthy…
	api := newFakeAPI(f)

	// …and a deployment started before the controller got to it.
	svc := api.services[testService]
	svc.Deployments = []Deployment{{
		ID: "d2", Status: DeploymentPrimary, TaskDefinition: testTDARN,
		DesiredCount: svc.DesiredCount, RunningCount: svc.DesiredCount,
		RolloutState: RolloutInProgress, RolloutStateReason: "ECS deployment in progress.",
		CreatedAt: testNow.Add(-time.Minute),
	}}
	api.services[testService] = svc

	act := newActuator(t, api)
	err := act.Execute(context.Background(), step)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Execute = %v, want a refusal", err)
	}
	var r *Refusal
	if !errors.As(err, &r) || r.Code != ReasonDeploymentInProgress {
		t.Fatalf("refusal code %v, want %s", err, ReasonDeploymentInProgress)
	}
	if len(api.registered) != 0 || len(api.updated) != 0 {
		t.Fatal("a refused step still touched the cloud")
	}

	// Revert is gated identically: a rollback is still a deployment.
	if err := act.Revert(context.Background(), step); !errors.Is(err, ErrRefused) {
		t.Fatalf("Revert = %v, want the same refusal", err)
	}
}

// TestExecuteRefusesGuardrails: freeze, breaker and the change window all stop
// the actuator before it reads anything.
func TestExecuteRefusesGuardrails(t *testing.T) {
	outside, err := guard.ParseWindows("Mon-Fri 22:00-23:00") // testNow is Wednesday 12:00
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		g    domain.Guard
		want error
	}{
		{"freeze", domain.Guard{Now: testNow, Freeze: true}, domain.ErrFrozen},
		{"breaker", domain.Guard{Now: testNow, BreakerOpen: true}, domain.ErrBreakerOpen},
		{"outside-window", domain.Guard{Now: testNow, Windows: outside}, domain.ErrOutsideWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			step := planStep(t, f)
			api := newFakeAPI(f)
			act := newActuator(t, api, func(c *ActuatorConfig) { c.Guard = tc.g })

			if err := act.Execute(context.Background(), step); !errors.Is(err, tc.want) {
				t.Fatalf("Execute = %v, want %v", err, tc.want)
			}
			if err := act.Revert(context.Background(), step); !errors.Is(err, tc.want) {
				t.Fatalf("Revert = %v, want %v", err, tc.want)
			}
			if len(api.registered) != 0 || len(api.updated) != 0 {
				t.Fatal("a gated step still touched the cloud")
			}
		})
	}

	// The window gate must be checked against the live guard, so a plan that
	// was legal when built is still refused when executed late.
	inside, err := guard.ParseWindows("Mon-Fri 11:00-13:00")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture()
	api := newFakeAPI(f)
	act := newActuator(t, api, func(c *ActuatorConfig) {
		c.Guard = domain.Guard{Now: testNow, Windows: inside}
	})
	if err := act.Execute(context.Background(), planStep(t, f)); err != nil {
		t.Fatalf("Execute inside the change window: %v", err)
	}
}

// TestExecuteRefusesModeOff: the tag guardrail is re-read live, so a service
// tagged off after the plan was approved is still protected.
func TestExecuteRefusesModeOff(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	api := newFakeAPI(f)
	svc := api.services[testService]
	svc.Tags = []Tag{{Key: TagKilterMode, Value: modeOff}}
	api.services[testService] = svc

	act := newActuator(t, api)
	err := act.Execute(context.Background(), step)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != ReasonModeOff {
		t.Fatalf("Execute = %v, want a %s refusal", err, ReasonModeOff)
	}
	if len(api.registered) != 0 {
		t.Fatal("a mode=off service got a new revision")
	}
}

// TestExecuteIsIdempotent: re-running a completed step after a controller
// restart must not register a second identical revision.
func TestExecuteIsIdempotent(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	api := newFakeAPI(f)
	act := newActuator(t, api)

	if err := act.Execute(context.Background(), step); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := act.Execute(context.Background(), step); err != nil {
		t.Fatalf("re-Execute: %v", err)
	}
	if len(api.registered) != 1 || len(api.updated) != 1 {
		t.Fatalf("re-executing a completed step registered %d revisions and issued %d updates",
			len(api.registered), len(api.updated))
	}
}

// TestExecuteRefusesRevisionDrift: somebody else changed the service, so the
// recorded rollback target no longer describes reality.
func TestExecuteRefusesRevisionDrift(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	api := newFakeAPI(f)

	other := "arn:aws:ecs:us-east-1:111122223333:task-definition/web:9"
	td := f.taskDef()
	td.TaskDefinitionARN, td.Revision = other, 9
	td.CPU, td.Memory = "8192", "16384" // a different size, so this is not "already applied"
	api.taskDefs[other] = td
	svc := api.services[testService]
	svc.TaskDefinition = other
	svc.Deployments[0].TaskDefinition = other
	api.services[testService] = svc

	act := newActuator(t, api)
	err := act.Execute(context.Background(), step)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != "revision-drift" {
		t.Fatalf("Execute = %v, want a revision-drift refusal", err)
	}
	if len(api.registered) != 0 {
		t.Fatal("a drifted service got a new revision")
	}
}

// TestExecuteRefusesShapeConstraints: awsvpc, container floors and the platform
// version are re-checked against the live task definition, not against whatever
// the plan believed.
func TestExecuteRefusesShapeConstraints(t *testing.T) {
	t.Run("network-mode", func(t *testing.T) {
		f := newFixture()
		step := planStep(t, f)
		api := newFakeAPI(f)
		td := api.taskDefs[testTDARN]
		td.NetworkMode = "bridge"
		api.taskDefs[testTDARN] = td

		err := newActuator(t, api).Execute(context.Background(), step)
		var r *Refusal
		if !errors.As(err, &r) || r.Code != ReasonNetworkMode {
			t.Fatalf("Execute = %v, want %s", err, ReasonNetworkMode)
		}
	})

	t.Run("container-floor", func(t *testing.T) {
		f := newFixture()
		step := planStep(t, f)
		api := newFakeAPI(f)
		td := api.taskDefs[testTDARN]
		td.ContainerDefinitions = []ContainerDefinition{{Name: "app", Memory: 8192}}
		api.taskDefs[testTDARN] = td

		err := newActuator(t, api).Execute(context.Background(), step)
		var r *Refusal
		if !errors.As(err, &r) || r.Code != ReasonContainerLimits {
			t.Fatalf("Execute = %v, want %s", err, ReasonContainerLimits)
		}
		if len(api.registered) != 0 {
			t.Fatal("sent a revision RegisterTaskDefinition would have rejected")
		}
	})

	t.Run("platform-version", func(t *testing.T) {
		f := newFixture(func(f *fixture) { f.platformVersion = "1.3.0" })
		api := newFakeAPI(f)
		// Hand-built: the sizer would never propose a growth, but a step from
		// an older plan or another producer must still be gated.
		step := domain.Step{
			Target: domain.TargetRef{Domain: Kind, Scope: testScope, ID: TargetID(testCluster, testService)},
			Action: domain.ActionRolling,
			From: domain.Spec{Attrs: map[string]string{
				AttrTaskDefinition: testTDARN, AttrCluster: testCluster, AttrService: testService,
			}},
			To: domain.Spec{Resources: model.Resources{MilliCPU: 8000, MemoryBytes: 16 << 30},
				Attrs: map[string]string{AttrCluster: testCluster, AttrService: testService}},
		}
		err := newActuator(t, api).Execute(context.Background(), step)
		var r *Refusal
		if !errors.As(err, &r) || r.Code != ReasonPlatformVersion {
			t.Fatalf("Execute = %v, want %s", err, ReasonPlatformVersion)
		}
	})
}

// TestExecuteRefusesNonRollingSteps: there is no in-place resize on Fargate, so
// a step claiming one is a bug in whoever produced it.
func TestExecuteRefusesNonRollingSteps(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	api := newFakeAPI(f)
	act := newActuator(t, api)

	for _, cls := range []domain.ActionClass{domain.ActionInPlace, domain.ActionAdvisory, domain.ActionStopStart} {
		s := step
		s.Action = cls
		if err := act.Execute(context.Background(), s); !errors.Is(err, ErrRefused) {
			t.Errorf("Execute with action %q = %v, want a refusal", cls, err)
		}
	}
	if len(api.registered) != 0 || len(api.updated) != 0 {
		t.Fatal("a non-rolling step touched the cloud")
	}
}

// TestExecuteRefusesMismatchedStepTarget: the target ID and the spec attributes
// must agree about which service is being changed.
func TestExecuteRefusesMismatchedStepTarget(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	step.To = step.To.WithAttr(AttrService, "somebody-else")
	api := newFakeAPI(f)

	err := newActuator(t, api).Execute(context.Background(), step)
	var r *Refusal
	if !errors.As(err, &r) || r.Code != "malformed-step" {
		t.Fatalf("Execute = %v, want a malformed-step refusal", err)
	}
}

// TestExecuteFailsLoudlyWhenTheServiceIsGone: a missing service is an error,
// never "nothing to do".
func TestExecuteFailsLoudlyWhenTheServiceIsGone(t *testing.T) {
	f := newFixture()
	step := planStep(t, f)
	api := newFakeAPI(f)
	delete(api.services, testService)

	if err := newActuator(t, api).Execute(context.Background(), step); err == nil {
		t.Fatal("Execute succeeded against a service that does not exist")
	}
}

// TestNewActuatorRequiresItsSeamsAndAClock.
func TestNewActuatorRequiresItsSeamsAndAClock(t *testing.T) {
	api := newFakeAPI(newFixture())
	if _, err := NewActuator(nil, api, ActuatorConfig{Guard: domain.Guard{Now: testNow}}); err == nil {
		t.Error("built an actuator with no read seam: it could not run its own pre-flight")
	}
	if _, err := NewActuator(api, nil, ActuatorConfig{Guard: domain.Guard{Now: testNow}}); err == nil {
		t.Error("built an actuator with no write seam")
	}
	if _, err := NewActuator(api, api, ActuatorConfig{}); err == nil {
		t.Error("built an actuator with no decision time: this package has no clock")
	}
}

// TestActuatorSatisfiesTheDomainSeam.
func TestActuatorSatisfiesTheDomainSeam(t *testing.T) {
	var a domain.Actuator = &Actuator{}
	if a.Domain() != domain.ECSFargate {
		t.Fatalf("Domain() = %q, want %q", a.Domain(), domain.ECSFargate)
	}
}
