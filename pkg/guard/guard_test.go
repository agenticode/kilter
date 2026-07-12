package guard

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func ref(ns, name string) model.WorkloadRef {
	return model.WorkloadRef{Kind: model.KindDeployment, Namespace: ns, Name: name}
}

func TestModeResolution(t *testing.T) {
	snap := &model.ClusterSnapshot{
		Workloads: []model.WorkloadInfo{
			{Ref: ref("prod", "critical"), Mode: "off"},
			{Ref: ref("prod", "web"), Mode: ""},
			{Ref: ref("dev", "junk"), Mode: "banana"}, // invalid → inherit
		},
		NamespaceModes: map[string]string{"prod": "recommend"},
	}
	if got := ModeFor(snap, ref("prod", "critical"), ModeApply); got != ModeOff {
		t.Fatalf("workload annotation must win: %s", got)
	}
	if got := ModeFor(snap, ref("prod", "web"), ModeApply); got != ModeRecommend {
		t.Fatalf("namespace annotation must apply: %s", got)
	}
	if got := ModeFor(snap, ref("dev", "junk"), ModeApply); got != ModeApply {
		t.Fatalf("invalid annotation falls back to default: %s", got)
	}
	if got := ModeFor(snap, ref("other", "x"), ""); got != ModeApply {
		t.Fatalf("empty default means apply: %s", got)
	}
}

func TestParseWindows(t *testing.T) {
	ws, err := ParseWindows("Mon-Fri 22:00-06:00, Sat+Sun 00:00-24:00")
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("windows: %d", len(ws))
	}
	if !ws[0].Days[time.Monday] || ws[0].Days[time.Saturday] {
		t.Fatalf("day parsing wrong: %+v", ws[0].Days)
	}
	if !ws[1].Days[time.Saturday] || !ws[1].Days[time.Sunday] || ws[1].Days[time.Monday] {
		t.Fatalf("split days wrong: %+v", ws[1].Days)
	}
	for _, bad := range []string{"Mon", "Mon 25:00-06:00", "Xyz 10:00-11:00", "Mon-Fri 10:00"} {
		if _, err := ParseWindows(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
	if ws, _ := ParseWindows(""); ws != nil {
		t.Fatal("empty spec = no windows")
	}
}

func TestInWindow(t *testing.T) {
	ws, _ := ParseWindows("Mon-Fri 22:00-06:00")
	at := func(day time.Weekday, h, m int) time.Time {
		// 2026-07-13 is a Monday.
		base := time.Date(2026, 7, 13, h, m, 0, 0, time.UTC)
		return base.AddDate(0, 0, int(day-time.Monday))
	}
	cases := []struct {
		t    time.Time
		want bool
		why  string
	}{
		{at(time.Monday, 23, 0), true, "Mon 23:00 inside evening part"},
		{at(time.Tuesday, 3, 0), true, "Tue 03:00 inside Mon window crossing midnight"},
		{at(time.Monday, 12, 0), false, "Mon noon outside"},
		{at(time.Saturday, 23, 0), false, "Sat evening not in Mon-Fri"},
		{at(time.Saturday, 3, 0), true, "Sat 03:00 tail of Fri window"},
		{at(time.Monday, 6, 0), false, "boundary: 06:00 is exclusive"},
		{at(time.Monday, 22, 0), true, "boundary: 22:00 is inclusive"},
	}
	for _, c := range cases {
		if got := InWindow(ws, c.t); got != c.want {
			t.Errorf("%s: got %v", c.why, got)
		}
	}
	if !InWindow(nil, time.Now()) {
		t.Fatal("no windows = always allowed")
	}
}

func TestBreaker(t *testing.T) {
	healthy := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{{Name: "a", Ready: true}, {Name: "b", Ready: true}},
		Pods:  []model.PodSpec{{UID: "p1", Phase: "Running"}},
	}
	if open, _ := Breaker(healthy, BreakerConfig{}); open {
		t.Fatal("healthy cluster must not trip")
	}

	sick := &model.ClusterSnapshot{
		Nodes: []model.NodeSpec{{Name: "a", Ready: true}, {Name: "b", Ready: false}},
	}
	open, reasons := Breaker(sick, BreakerConfig{})
	if !open || len(reasons) == 0 {
		t.Fatalf("50%% NotReady must trip: %v", reasons)
	}

	pending := &model.ClusterSnapshot{Nodes: []model.NodeSpec{{Name: "a", Ready: true}}}
	for i := 0; i < 15; i++ {
		pending.Pods = append(pending.Pods, model.PodSpec{UID: string(rune('a' + i)), Phase: "Pending"})
	}
	if open, _ := Breaker(pending, BreakerConfig{}); !open {
		t.Fatal("pending surge must trip")
	}

	frozen := &model.ClusterSnapshot{Frozen: true,
		Nodes: []model.NodeSpec{{Name: "a", Ready: true}}}
	open, reasons = Breaker(frozen, BreakerConfig{})
	if !open || len(reasons) != 1 {
		t.Fatalf("freeze must trip with its own reason: %v", reasons)
	}
}
