package safety

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

var t0 = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

func dep(ns, name string) model.WorkloadRef {
	return model.WorkloadRef{Kind: model.KindDeployment, Namespace: ns, Name: name}
}

func TestCanEvictRules(t *testing.T) {
	ok := &model.PodSpec{Workload: dep("d", "w")}
	if ev := CanEvict(ok); !ev.OK {
		t.Fatalf("plain deployment pod must be evictable: %s", ev.Reason)
	}
	cases := []struct {
		name string
		pod  model.PodSpec
	}{
		{"do-not-evict", model.PodSpec{Workload: dep("d", "w"), DoNotEvict: true}},
		{"bare pod", model.PodSpec{Workload: model.WorkloadRef{Kind: model.KindBarePod, Namespace: "d", Name: "p"}}},
		{"daemonset", model.PodSpec{Workload: model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "d", Name: "ds"}}},
		{"local storage", model.PodSpec{Workload: dep("d", "w"), HasLocalStorage: true}},
	}
	for _, c := range cases {
		if ev := CanEvict(&c.pod); ev.OK || ev.Reason == "" {
			t.Errorf("%s: must be non-evictable with reason", c.name)
		}
	}
}

func TestBlocksDrain(t *testing.T) {
	ds := &model.PodSpec{Workload: model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "d", Name: "ds"}}
	if blocks, _ := BlocksDrain(ds); blocks {
		t.Fatal("daemonset pods must not block drain")
	}
	bare := &model.PodSpec{Workload: model.WorkloadRef{Kind: model.KindBarePod, Namespace: "d", Name: "p"}}
	if blocks, reason := BlocksDrain(bare); !blocks || reason == "" {
		t.Fatal("bare pod must block drain with reason")
	}
	normal := &model.PodSpec{Workload: dep("d", "w")}
	if blocks, _ := BlocksDrain(normal); blocks {
		t.Fatal("normal pod must not block drain")
	}
}

func TestPDBGuardReserveRelease(t *testing.T) {
	g := NewPDBGuard([]model.PDB{{
		Namespace: "prod", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 2,
	}})
	pod := &model.PodSpec{Namespace: "prod", Labels: map[string]string{"app": "web"}, Workload: dep("prod", "web")}
	other := &model.PodSpec{Namespace: "prod", Labels: map[string]string{"app": "api"}, Workload: dep("prod", "api")}

	if ok, _ := g.Reserve(pod); !ok {
		t.Fatal("first reserve should pass")
	}
	if ok, _ := g.Reserve(pod); !ok {
		t.Fatal("second reserve should pass")
	}
	if ok, reason := g.Reserve(pod); ok || reason == "" {
		t.Fatal("third reserve must fail with reason")
	}
	if ok, _ := g.CanEvict(pod); ok {
		t.Fatal("CanEvict must be false when budget exhausted")
	}
	// Unrelated pod is unaffected.
	if ok, _ := g.Reserve(other); !ok {
		t.Fatal("non-matching pod must not be limited")
	}
	// Namespace must match even when labels do.
	otherNS := &model.PodSpec{Namespace: "dev", Labels: map[string]string{"app": "web"}, Workload: dep("dev", "web")}
	if ok, _ := g.Reserve(otherNS); !ok {
		t.Fatal("same labels in another namespace must not match")
	}
	g.Release(pod)
	if ok, _ := g.Reserve(pod); !ok {
		t.Fatal("release must restore budget")
	}
}

func TestCooldowns(t *testing.T) {
	c := NewCooldowns(10 * time.Minute)
	if !c.Allow("node/a", t0) {
		t.Fatal("first action allowed")
	}
	if c.Allow("node/a", t0.Add(5*time.Minute)) {
		t.Fatal("within cooldown must deny")
	}
	if got := c.Remaining("node/a", t0.Add(5*time.Minute)); got != 5*time.Minute {
		t.Fatalf("remaining = %v", got)
	}
	if !c.Allow("node/b", t0) {
		t.Fatal("different key unaffected")
	}
	if !c.Allow("node/a", t0.Add(11*time.Minute)) {
		t.Fatal("after cooldown must allow")
	}
}

func TestBudgetSlidingWindow(t *testing.T) {
	b := NewBudget(3, time.Hour)
	for i := 0; i < 3; i++ {
		if !b.Allow(t0.Add(time.Duration(i) * time.Minute)) {
			t.Fatalf("event %d should be allowed", i)
		}
	}
	if b.Allow(t0.Add(10 * time.Minute)) {
		t.Fatal("4th event within window must be denied")
	}
	if got := b.Used(t0.Add(10 * time.Minute)); got != 3 {
		t.Fatalf("used = %d", got)
	}
	// After the window slides past the first event, one slot frees up.
	if !b.Allow(t0.Add(61 * time.Minute)) {
		t.Fatal("slot should free after window slides")
	}
}

func TestRegressionDetector(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, 24*time.Hour)
	ref := dep("prod", "api")
	base := &model.ClusterSnapshot{Pods: []model.PodSpec{{
		Workload: ref, Containers: []model.ContainerSpec{{Name: "app", RestartCount: 2}},
	}}}
	d.RecordChange(ref, base, t0)

	// No change → no regression.
	if regs := d.Check(base, t0.Add(5*time.Minute)); len(regs) != 0 {
		t.Fatalf("no regression expected: %+v", regs)
	}

	// OOM after change → regression + quarantine.
	oomed := &model.ClusterSnapshot{Pods: []model.PodSpec{{
		Workload: ref, Containers: []model.ContainerSpec{{Name: "app", RestartCount: 3, LastOOMKilled: true}},
	}}}
	regs := d.Check(oomed, t0.Add(10*time.Minute))
	if len(regs) != 1 {
		t.Fatalf("want 1 regression, got %d", len(regs))
	}
	if !d.Quarantined(ref, t0.Add(1*time.Hour)) {
		t.Fatal("workload must be quarantined")
	}
	if d.Quarantined(ref, t0.Add(30*time.Hour)) {
		t.Fatal("quarantine must expire")
	}
	// Regression reported once, then watch is dropped.
	if regs := d.Check(oomed, t0.Add(11*time.Minute)); len(regs) != 0 {
		t.Fatal("regression must not repeat")
	}
}

func TestRegressionCrashloopThreshold(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	ref := dep("prod", "flaky")
	mk := func(restarts int32) *model.ClusterSnapshot {
		return &model.ClusterSnapshot{Pods: []model.PodSpec{{
			Workload: ref, Containers: []model.ContainerSpec{{Name: "app", RestartCount: restarts}},
		}}}
	}
	d.RecordChange(ref, mk(1), t0)
	// +2 restarts: tolerated (could be a rollout).
	if regs := d.Check(mk(3), t0.Add(5*time.Minute)); len(regs) != 0 {
		t.Fatal("+2 restarts should be tolerated")
	}
	// +3 restarts: crashloop.
	if regs := d.Check(mk(4), t0.Add(6*time.Minute)); len(regs) != 1 {
		t.Fatal("+3 restarts must be a regression")
	}
}

func TestRegressionWindowExpiry(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	ref := dep("prod", "slow")
	base := &model.ClusterSnapshot{Pods: []model.PodSpec{{
		Workload: ref, Containers: []model.ContainerSpec{{Name: "app"}},
	}}}
	d.RecordChange(ref, base, t0)
	bad := &model.ClusterSnapshot{Pods: []model.PodSpec{{
		Workload: ref, Containers: []model.ContainerSpec{{Name: "app", RestartCount: 9, LastOOMKilled: true}},
	}}}
	// Past the window: watch expired, no regression attributed to us.
	if regs := d.Check(bad, t0.Add(2*time.Hour)); len(regs) != 0 {
		t.Fatal("expired watch must not fire")
	}
}
