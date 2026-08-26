package ecs

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"testing"

	"github.com/agenticode/kilter/pkg/domain"
)

// mixedSnapshot builds a snapshot exercising every branch of the sizer, so the
// determinism tests below cover proposals, refusals and advisories at once.
func mixedSnapshot() *Snapshot {
	mods := []func(*fixture){
		func(f *fixture) { f.service, f.tdARN = "api", "td/api:3" },
		func(f *fixture) { f.service, f.tdARN = "worker", "td/worker:1"; f.constCPUPct, f.constMem = 90, 90 },
		func(f *fixture) { f.service, f.tdARN = "cron", "td/cron:2"; f.samples = 0 },
		func(f *fixture) {
			f.service, f.tdARN = "web", "td/web:9"
			f.tags = []Tag{{Key: TagName, Value: "web"}, {Key: TagKilterMode, Value: modeRecommend}}
			f.containers = []ContainerDefinition{
				{Name: "sidecar", Memory: 512}, {Name: "app", Memory: 1024}, {Name: "init", Memory: 256},
			}
		},
		func(f *fixture) { f.service, f.tdARN = "batch", "td/batch:4"; f.pending = 2 },
	}
	snap := &Snapshot{Domain: Kind, Scope: testScope, Cluster: testCluster, Timestamp: testNow}
	for _, m := range mods {
		snap.Services = append(snap.Services, newFixture(m).observation())
	}
	return snap
}

// TestReportIsShuffleInvariant: the report is a function of the data, not of
// the order it arrived in. Shuffling services, tags, deployments and container
// definitions must not change a byte — no Go map iteration may reach an output.
func TestReportIsShuffleInvariant(t *testing.T) {
	s := NewSizer(testConfig())
	want, err := json.Marshal(s.Report(mixedSnapshot(), testNow, nil))
	if err != nil {
		t.Fatal(err)
	}

	for seed := int64(0); seed < 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		snap := mixedSnapshot()
		rng.Shuffle(len(snap.Services), func(i, j int) {
			snap.Services[i], snap.Services[j] = snap.Services[j], snap.Services[i]
		})
		for i := range snap.Services {
			o := &snap.Services[i]
			tags := o.Service.Tags
			rng.Shuffle(len(tags), func(a, b int) { tags[a], tags[b] = tags[b], tags[a] })
			o.Tags = tagMap(tags)
			cds := o.TaskDef.ContainerDefinitions
			rng.Shuffle(len(cds), func(a, b int) { cds[a], cds[b] = cds[b], cds[a] })
			deps := o.Service.Deployments
			rng.Shuffle(len(deps), func(a, b int) { deps[a], deps[b] = deps[b], deps[a] })
		}
		got, err := json.Marshal(s.Report(snap, testNow, nil))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("seed %d produced a different report:\n got %s\nwant %s", seed, got, want)
		}
	}
}

// TestRecommendationsAndStepsAreShuffleInvariant: the projection into the
// domain-generic shape, and the plan built from it, are order-independent too —
// which is what makes a plan fingerprint mean anything.
func TestRecommendationsAndStepsAreShuffleInvariant(t *testing.T) {
	s := NewSizer(testConfig())
	base := s.Report(mixedSnapshot(), testNow, nil).Recommendations()
	wantSteps, err := PlanSteps(base, domain.Guard{Now: testNow})
	if err != nil {
		t.Fatal(err)
	}
	wantFP := domain.Fingerprint(wantSteps)
	if len(wantSteps) == 0 {
		t.Fatal("the mixed snapshot planned no steps; the test proves nothing")
	}

	for seed := int64(0); seed < 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		recs := append([]domain.Recommendation(nil), base...)
		rng.Shuffle(len(recs), func(i, j int) { recs[i], recs[j] = recs[j], recs[i] })
		steps, err := PlanSteps(recs, domain.Guard{Now: testNow})
		if err != nil {
			t.Fatal(err)
		}
		if got := domain.Fingerprint(steps); got != wantFP {
			t.Fatalf("seed %d fingerprint %s, want %s", seed, got, wantFP)
		}
		if !reflect.DeepEqual(steps, wantSteps) {
			t.Fatalf("seed %d produced different steps:\n got %+v\nwant %+v", seed, steps, wantSteps)
		}
	}
}

// TestStepKeysAreStableAcrossRuns: the idempotency key must not depend on
// anything that varies between runs, or re-executing a completed step after a
// controller restart would register a second revision.
func TestStepKeysAreStableAcrossRuns(t *testing.T) {
	s := NewSizer(testConfig())
	f := newFixture()
	a := planStepFrom(t, s, f)
	b := planStepFrom(t, s, f)
	if a.Key != b.Key {
		t.Fatalf("step key %s then %s", a.Key, b.Key)
	}
	if a.Key == "" {
		t.Fatal("step has no idempotency key")
	}
	// A different proposed size is a different step.
	c := planStepFrom(t, s, newFixture(func(f *fixture) { f.constCPUPct, f.constMem = 10, 10 }))
	if c.Key == a.Key {
		t.Fatal("two different proposals share an idempotency key")
	}
}

func planStepFrom(t *testing.T, s *Sizer, f *fixture) domain.Step {
	t.Helper()
	steps, err := PlanSteps(s.Report(f.snapshot(), testNow, nil).Recommendations(), domain.Guard{Now: testNow})
	if err != nil || len(steps) != 1 {
		t.Fatalf("PlanSteps = %d steps, %v", len(steps), err)
	}
	return steps[0]
}

// TestPlanStepsRejectsForeignRecommendations: a plan built from another
// domain's output would be applied by this domain's actuator.
func TestPlanStepsRejectsForeignRecommendations(t *testing.T) {
	s := NewSizer(testConfig())
	recs := s.Report(newFixture().snapshot(), testNow, nil).Recommendations()
	for i := range recs {
		recs[i].Target.Domain = domain.K8sFargate
	}
	if _, err := PlanSteps(recs, domain.Guard{Now: testNow}); err == nil {
		t.Fatal("planned steps for another domain's recommendations")
	}

	// And a recommendation claiming an in-place resize is refused: there is no
	// in-place resize on Fargate, and a step claiming one would understate
	// disruption to whoever executes it.
	recs = s.Report(newFixture().snapshot(), testNow, nil).Recommendations()
	for i := range recs {
		if recs[i].Action == domain.ActionRolling {
			recs[i].Action = domain.ActionInPlace
		}
	}
	if _, err := PlanSteps(recs, domain.Guard{Now: testNow}); err == nil {
		t.Fatal("planned an in-place ECS resize")
	}
}

// TestPlanStepsHonoursGuardrails.
func TestPlanStepsHonoursGuardrails(t *testing.T) {
	s := NewSizer(testConfig())
	recs := s.Report(mixedSnapshot(), testNow, nil).Recommendations()
	if _, err := PlanSteps(recs, domain.Guard{Now: testNow, Freeze: true}); err != domain.ErrFrozen {
		t.Errorf("freeze = %v, want ErrFrozen", err)
	}
	if _, err := PlanSteps(recs, domain.Guard{Now: testNow, BreakerOpen: true}); err != domain.ErrBreakerOpen {
		t.Errorf("breaker = %v, want ErrBreakerOpen", err)
	}
	steps, err := PlanSteps(recs, domain.Guard{Now: testNow, MaxSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("MaxSteps=1 produced %d steps", len(steps))
	}
}
