package ecs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	kecs "github.com/agenticode/kilter/pkg/ecs"
)

var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func clusterSnapshot(t *testing.T, cluster string, services ...string) *domain.Snapshot {
	t.Helper()
	native := kecs.Snapshot{
		Domain: Kind, Scope: "000000000000/us-east-1", Cluster: cluster, Timestamp: testNow,
		Window: kecs.Window{Start: testNow.Add(-14 * 24 * time.Hour), End: testNow},
	}
	for _, s := range services {
		native.Services = append(native.Services, kecs.Observation{
			Ref: domain.TargetRef{Domain: Kind, Scope: native.Scope,
				ID: kecs.TargetID(cluster, s), Name: s},
			Service: kecs.ServiceRecord{
				ServiceName: s, Status: "ACTIVE", LaunchType: "FARGATE",
				TaskDefinition: "arn:aws:ecs:us-east-1:0:task-definition/" + s + ":1",
				DesiredCount:   1, RunningCount: 1,
			},
			TaskDef: kecs.TaskDefinitionRecord{
				Family: s, Revision: 1, Status: "ACTIVE",
				CPU: "1024", Memory: "2048", NetworkMode: kecs.NetworkModeAWSVPC,
			},
			Window: native.Window,
		})
	}
	raw, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	return &domain.Snapshot{Domain: Kind, Scope: native.Scope, Timestamp: testNow, Payload: raw}
}

// TestOneDomainCoversManyClusters is the whole reason this adapter is not
// empty: a pkg/ecs snapshot is one CLUSTER, a domain.Kind is an ACCOUNT.
func TestOneDomainCoversManyClusters(t *testing.T) {
	d, err := New(Config{Scope: "000000000000/us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(clusterSnapshot(t, "prod", "web", "api")); err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(clusterSnapshot(t, "staging", "web")); err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if h.Targets != 3 {
		t.Fatalf("Targets = %d, want 3 across both clusters", h.Targets)
	}
	if got := len(d.Reports(testNow, nil)); got != 2 {
		t.Fatalf("Reports = %d, want one per cluster", got)
	}
	// Re-learning one cluster replaces only that cluster.
	if err := d.Learn(clusterSnapshot(t, "prod", "web")); err != nil {
		t.Fatal(err)
	}
	if got := d.Health(testNow).Targets; got != 2 {
		t.Fatalf("Targets = %d after re-learning prod, want 2", got)
	}
	// Reports come back in sorted cluster order, always.
	for i := 0; i < 30; i++ {
		reps := d.Reports(testNow, nil)
		if reps[0].Cluster != "prod" || reps[1].Cluster != "staging" {
			t.Fatalf("Reports are not in sorted cluster order: %s, %s",
				reps[0].Cluster, reps[1].Cluster)
		}
	}
}

// TestActuationIsAWiringFactNotADomainOpinion. pkg/ecs ships a complete,
// tested actuator; whether one is WIRED is a fact about cmd/.
func TestActuationIsAWiringFactNotADomainOpinion(t *testing.T) {
	unwired, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unwired.Learn(clusterSnapshot(t, "prod", "web")); err != nil {
		t.Fatal(err)
	}
	h := unwired.Health(testNow)
	if !h.ReportOnly {
		t.Fatal("a domain with no actuator claims it can act")
	}
	if !strings.Contains(h.Reason, "no actuator") {
		t.Errorf("health reason = %q", h.Reason)
	}
	if _, err := unwired.PlanSteps(nil, domain.Guard{Now: testNow}); !errors.Is(err, domain.ErrReportOnly) {
		t.Fatalf("PlanSteps = %v, want ErrReportOnly", err)
	}

	wired, err := New(Config{ActuationAvailable: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := wired.Learn(clusterSnapshot(t, "prod", "web")); err != nil {
		t.Fatal(err)
	}
	if wired.Health(testNow).ReportOnly {
		t.Fatal("a wired domain still reports report-only")
	}
	// Nothing applicable: no steps, no error.
	steps, err := wired.PlanSteps(nil, domain.Guard{Now: testNow})
	if err != nil || len(steps) != 0 {
		t.Fatalf("PlanSteps = (%v, %v)", steps, err)
	}
}

// TestUnfedAndStaleDomainsAreReportOnly.
func TestUnfedAndStaleDomainsAreReportOnly(t *testing.T) {
	d, err := New(Config{ActuationAvailable: true})
	if err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if h.Ready || !h.ReportOnly || !strings.Contains(h.Reason, "no snapshot") {
		t.Fatalf("an unfed domain reports %+v", h)
	}

	old := clusterSnapshot(t, "prod", "web")
	old.Timestamp = testNow.Add(-6 * time.Hour)
	var native kecs.Snapshot
	if err := json.Unmarshal(old.Payload, &native); err != nil {
		t.Fatal(err)
	}
	native.Timestamp = old.Timestamp
	raw, _ := json.Marshal(native)
	old.Payload = raw
	if err := d.Learn(old); err != nil {
		t.Fatal(err)
	}
	h = d.Health(testNow)
	if h.Ready || !h.ReportOnly {
		t.Fatalf("a six-hour-old snapshot reports %+v", h)
	}
	if !strings.Contains(h.Reason, "old") {
		t.Errorf("health reason = %q", h.Reason)
	}
}

// TestLearnInputHandling.
func TestLearnInputHandling(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(nil); err != nil {
		t.Errorf("Learn(nil) = %v", err)
	}
	// A payload-less snapshot for this kind is a no-op, not a wipe.
	if err := d.Learn(clusterSnapshot(t, "prod", "web")); err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Timestamp: testNow}); err != nil {
		t.Fatal(err)
	}
	if got := d.Health(testNow).Targets; got != 1 {
		t.Fatalf("Targets = %d after an empty snapshot; the domain forgot its services", got)
	}
	if err := d.Learn(&domain.Snapshot{Domain: domain.Lambda}); !errors.Is(err, domain.ErrWrongDomain) {
		t.Errorf("Learn(foreign) = %v, want ErrWrongDomain", err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Payload: []byte("{{")}); err == nil {
		t.Error("a malformed payload was accepted as an empty cluster")
	}
	// A snapshot with no cluster name has no key to store under.
	if err := d.Observe(&kecs.Snapshot{Domain: Kind}); err == nil {
		t.Error("a snapshot with no cluster was accepted")
	}
}

// TestRefusalsExplainEveryServiceWithNoProposal.
func TestRefusalsExplainEveryServiceWithNoProposal(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(clusterSnapshot(t, "prod", "web", "api")); err != nil {
		t.Fatal(err)
	}
	if _, ok := any(d).(domain.Refuser); !ok {
		t.Fatal("the ECS domain cannot explain its refusals")
	}
	// No metrics at all in this fixture, so both services refuse.
	refs := d.Refusals(testNow, nil)
	if len(refs) != 2 {
		t.Fatalf("Refusals = %v, want one per service", refs)
	}
	for _, r := range refs {
		if r.Code == "" || r.Reason == "" {
			t.Errorf("refusal has no code or reason: %+v", r)
		}
		if r.Target.Domain != Kind {
			t.Errorf("refusal is attributed to %q", r.Target.Domain)
		}
	}
	if refs[0].Target.ID > refs[1].Target.ID {
		t.Error("refusals are not canonically ordered")
	}
	if got := d.Recommend(testNow, nil); len(got) != 0 {
		t.Errorf("services with no evidence produced recommendations: %v", got)
	}
}

// TestCheckpointRoundTripIsDeterministic.
func TestCheckpointRoundTripIsDeterministic(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Learn in one order...
	if err := d.Learn(clusterSnapshot(t, "staging", "web")); err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(clusterSnapshot(t, "prod", "api")); err != nil {
		t.Fatal(err)
	}
	first, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := d.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatal("Checkpoint is not byte-stable")
		}
	}
	// ...and the other. Clusters are persisted sorted, so the bytes match.
	other, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Learn(clusterSnapshot(t, "prod", "api")); err != nil {
		t.Fatal(err)
	}
	if err := other.Learn(clusterSnapshot(t, "staging", "web")); err != nil {
		t.Fatal(err)
	}
	swapped, err := other.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(swapped) != string(first) {
		t.Fatal("Checkpoint depends on the order clusters were learned in")
	}

	restored, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(first); err != nil {
		t.Fatal(err)
	}
	if got := restored.Health(testNow).Targets; got != 2 {
		t.Fatalf("restored domain tracks %d targets, want 2", got)
	}
	if err := restored.Restore(nil); err != nil {
		t.Errorf("Restore(nil) = %v", err)
	}
	if err := restored.Restore([]byte("nope")); err == nil {
		t.Error("a malformed checkpoint was accepted")
	}
}
