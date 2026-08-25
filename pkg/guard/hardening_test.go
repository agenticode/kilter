package guard

import (
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func TestParseWindowsStrict(t *testing.T) {
	cases := []struct {
		spec string
		ok   bool
		why  string
	}{
		{"Mon 09:00-17:00", true, "plain single day"},
		{"MON-fri 22:00-06:00", true, "case-insensitive days"},
		{"mon 9:5-17:59", true, "single-digit hour/minute tolerated"},
		{" Mon-Fri  22:00-06:00 ,Sat 00:00-24:00 ", true, "whitespace tolerated"},
		{"Fri-Mon 09:00-17:00", true, "day range wrapping the week end"},
		{"Sun-Sat 00:00-24:00", true, "all days, full day"},
		{"Mon+Wed+Fri 10:00-11:00", true, "day union"},
		{"Mon 10:00-10:00", true, "start==end is a 24h wrap window"},
		{"Mon 22:00-00:00", true, "end at midnight"},
		{"Mon 00:00-24:00", true, "24:00 is a valid end"},

		{"", true, "empty spec = no windows"},
		{"Mon", false, "missing time range"},
		{"Mon 10:00", false, "missing dash"},
		{"Mon 1000-1100", false, "missing colons"},
		{"Mon-Fri 22:00-06:00 extra", false, "trailing field"},
		{"Mon 22:00-06:00:30", false, "trailing garbage after range"},
		{"Mon 22:00-06:00x", false, "trailing letter after range"},
		{"Mon +2:00-06:00", false, "sign in hour"},
		{"Mon 2:-5-06:00", false, "sign in minute"},
		{"Mon 24:00-06:00", false, "start hour 24"},
		{"Mon 00:00-24:30", false, "end past 24:00"},
		{"Mon 22:60-06:00", false, "start minute 60"},
		{"Mon 22:00-06:60", false, "end minute 60"},
		{"Mon 25:00-06:00", false, "start hour out of range"},
		{"Mon 99999999999999999999:00-06:00", false, "hour overflow"},
		{"Xyz 10:00-11:00", false, "unknown day"},
		{"Monday 10:00-11:00", false, "full day name"},
		{"Mon++Fri 10:00-11:00", false, "empty day between pluses"},
		{"Mon- 10:00-11:00", false, "dangling day range"},
		{"Mon--Fri 10:00-11:00", false, "double dash in days"},
		{"Mon 10:00-11:00,", false, "trailing comma"},
		{",", false, "bare comma"},
		{"Mon ٣:00-06:00", false, "non-ASCII digit"},
	}
	for _, c := range cases {
		ws, err := ParseWindows(c.spec)
		if c.ok && err != nil {
			t.Errorf("%s: ParseWindows(%q) unexpectedly failed: %v", c.why, c.spec, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: ParseWindows(%q) should have failed, got %+v", c.why, c.spec, ws)
		}
		if err != nil && ws != nil {
			t.Errorf("%s: error must come with nil windows", c.why)
		}
	}
}

func TestParseWindowsInvariants(t *testing.T) {
	ws, err := ParseWindows("Fri-Mon 09:00-17:00, Mon 22:00-00:00, Sun 23:30-00:15")
	if err != nil {
		t.Fatal(err)
	}
	w := ws[0]
	for d := time.Sunday; d <= time.Saturday; d++ {
		want := d == time.Friday || d == time.Saturday || d == time.Sunday || d == time.Monday
		if w.Days[d] != want {
			t.Errorf("Fri-Mon: day %v = %v, want %v", d, w.Days[d], want)
		}
	}
	if ws[1].Start != 22*60 || ws[1].End != 0 {
		t.Errorf("22:00-00:00 parsed to %d-%d", ws[1].Start, ws[1].End)
	}
	for _, w := range ws {
		if w.Start < 0 || w.Start > 1439 || w.End < 0 || w.End > 1440 {
			t.Errorf("minutes out of documented range: %+v", w)
		}
	}
}

// expandWeek is an independent reference for InWindow: it materializes every
// covered minute of a week as a bitmap by expanding each window into explicit
// [Start,End) intervals, spilling cross-midnight tails into the next day.
func expandWeek(ws []Window) []bool {
	set := make([]bool, 7*1440)
	for _, w := range ws {
		for d := 0; d < 7; d++ {
			if !w.Days[d] {
				continue
			}
			if w.End > w.Start {
				for m := w.Start; m < w.End; m++ {
					set[d*1440+m] = true
				}
			} else {
				for m := w.Start; m < 1440; m++ {
					set[d*1440+m] = true
				}
				for m := 0; m < w.End; m++ {
					set[((d+1)%7)*1440+m] = true
				}
			}
		}
	}
	return set
}

// TestInWindowMatchesReference sweeps every minute of a full week and checks
// InWindow against the interval-expansion reference, for a spread of window
// shapes (cross-midnight, full-day, 24h wrap, week-end day wrap, overlaps).
func TestInWindowMatchesReference(t *testing.T) {
	specs := []string{
		"Mon-Fri 22:00-06:00",
		"Sat+Sun 00:00-24:00",
		"Sun 23:30-00:15",
		"Mon 10:00-10:00",
		"Mon 22:00-00:00",
		"Fri-Mon 09:00-17:00",
		"Mon-Fri 22:00-06:00, Sat+Sun 08:00-20:00",
		"Sun-Sat 00:00-00:00",
	}
	// 2026-07-12 is a Sunday; start the sweep at its midnight.
	base := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	if base.Weekday() != time.Sunday {
		t.Fatal("test anchor must be a Sunday")
	}
	for _, spec := range specs {
		ws, err := ParseWindows(spec)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		ref := expandWeek(ws)
		for i := 0; i < 7*1440; i++ {
			at := base.Add(time.Duration(i) * time.Minute)
			if got := InWindow(ws, at); got != ref[i] {
				t.Fatalf("%q: %s (%v) = %v, reference says %v",
					spec, at.Format("Mon 15:04"), at.Weekday(), got, ref[i])
			}
		}
	}
}

func TestInWindowEdges(t *testing.T) {
	// A window with no days set can't come out of the parser but must still
	// never match if built by hand.
	dead := []Window{{Start: 0, End: 1440}}
	if InWindow(dead, time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)) {
		t.Error("window with zero days must never match")
	}
	// Seconds within a minute must not matter: 05:59:59 is inside a window
	// ending 06:00, 06:00:30 is outside.
	ws, _ := ParseWindows("Mon-Fri 22:00-06:00")
	in := time.Date(2026, 7, 14, 5, 59, 59, 0, time.UTC) // Tue
	out := time.Date(2026, 7, 14, 6, 0, 30, 0, time.UTC)
	if !InWindow(ws, in) {
		t.Error("05:59:59 must be inside a -06:00 window")
	}
	if InWindow(ws, out) {
		t.Error("06:00:30 must be outside a -06:00 window")
	}
}

func TestModeForHardening(t *testing.T) {
	if got := ModeFor(nil, ref("prod", "x"), ModeApply); got != ModeRecommend {
		t.Errorf("nil snapshot must fail safe to recommend, got %s", got)
	}

	dup := &model.ClusterSnapshot{Workloads: []model.WorkloadInfo{
		{Ref: ref("ns", "a"), Mode: ModeApply},
		{Ref: ref("ns", "a"), Mode: ModeOff},
	}}
	if got := ModeFor(dup, ref("ns", "a"), ModeRecommend); got != ModeApply {
		t.Errorf("first valid duplicate must win, got %s", got)
	}

	skip := &model.ClusterSnapshot{Workloads: []model.WorkloadInfo{
		{Ref: ref("ns", "a"), Mode: "garbage"},
		{Ref: ref("ns", "a"), Mode: ModeOff},
	}}
	if got := ModeFor(skip, ref("ns", "a"), ModeApply); got != ModeOff {
		t.Errorf("invalid duplicate must be skipped, got %s", got)
	}

	// Namespace modes apply only to their own namespace; nil map is fine.
	nsOnly := &model.ClusterSnapshot{NamespaceModes: map[string]string{"prod": ModeOff}}
	if got := ModeFor(nsOnly, ref("dev", "x"), ModeRecommend); got != ModeRecommend {
		t.Errorf("other namespace's mode must not leak, got %s", got)
	}
	empty := &model.ClusterSnapshot{}
	if got := ModeFor(empty, ref("ns", "x"), "bogus"); got != ModeApply {
		t.Errorf("invalid default must fall back to apply, got %s", got)
	}
	if got := ModeFor(empty, ref("ns", "x"), ModeOff); got != ModeOff {
		t.Errorf("valid default must be honored, got %s", got)
	}
}

func nodes(ready, notReady int) []model.NodeSpec {
	var out []model.NodeSpec
	for i := 0; i < ready; i++ {
		out = append(out, model.NodeSpec{Name: "r", Ready: true})
	}
	for i := 0; i < notReady; i++ {
		out = append(out, model.NodeSpec{Name: "n", Ready: false})
	}
	return out
}

func pendingPods(n int) []model.PodSpec {
	var out []model.PodSpec
	for i := 0; i < n; i++ {
		out = append(out, model.PodSpec{Phase: "Pending"})
	}
	return out
}

func TestBreakerHardening(t *testing.T) {
	open, reasons := Breaker(nil, BreakerConfig{})
	if !open || len(reasons) != 1 {
		t.Fatalf("nil snapshot must open the breaker: %v", reasons)
	}

	open, reasons = Breaker(&model.ClusterSnapshot{}, BreakerConfig{})
	if !open || len(reasons) == 0 {
		t.Fatalf("empty node list must open the breaker: %v", reasons)
	}

	// NaN in the config must fall back to the default, not disable the check.
	sick := &model.ClusterSnapshot{Nodes: nodes(1, 1)}
	if open, _ = Breaker(sick, BreakerConfig{MaxNotReadyFraction: math.NaN()}); !open {
		t.Fatal("NaN fraction must not disable the node-health check")
	}

	// Exactly at the threshold does not trip; strictly above does.
	if open, _ = Breaker(&model.ClusterSnapshot{Nodes: nodes(4, 1)}, BreakerConfig{}); open {
		t.Fatal("1/5 NotReady == 0.2 must not trip the default breaker")
	}
	if open, _ = Breaker(&model.ClusterSnapshot{Nodes: nodes(3, 2)}, BreakerConfig{}); !open {
		t.Fatal("2/5 NotReady > 0.2 must trip the default breaker")
	}

	// Fraction >= 1 documents itself as "node check disabled".
	allDown := &model.ClusterSnapshot{Nodes: nodes(0, 3)}
	if open, _ = Breaker(allDown, BreakerConfig{MaxNotReadyFraction: 1}); open {
		t.Fatal("fraction 1 tolerates even 100% NotReady")
	}

	// Pending-pod threshold: exactly at the default (10) holds, 11 trips.
	atLimit := &model.ClusterSnapshot{Nodes: nodes(1, 0), Pods: pendingPods(10)}
	if open, _ = Breaker(atLimit, BreakerConfig{}); open {
		t.Fatal("10 pending pods must not trip the default breaker")
	}
	over := &model.ClusterSnapshot{Nodes: nodes(1, 0), Pods: pendingPods(11)}
	if open, _ = Breaker(over, BreakerConfig{}); !open {
		t.Fatal("11 pending pods must trip the default breaker")
	}

	// Freeze short-circuits with a single reason even if everything is sick.
	frozenSick := &model.ClusterSnapshot{Frozen: true, Nodes: nodes(0, 5), Pods: pendingPods(50)}
	open, reasons = Breaker(frozenSick, BreakerConfig{})
	if !open || len(reasons) != 1 {
		t.Fatalf("freeze must short-circuit with one reason: %v", reasons)
	}
}

func FuzzParseWindows(f *testing.F) {
	for _, seed := range []string{
		"Mon-Fri 22:00-06:00, Sat+Sun 00:00-24:00",
		"Sun 23:30-00:15",
		"Mon 10:00-10:00",
		"mon 9:5-17:59",
		"Mon 22:00-06:00:30",
		"Mon +2:00-06:00",
		"Mon 00:00-24:30",
		"", ",", "Mon", "Xyz 10:00-11:00",
	} {
		f.Add(seed)
	}
	anchor := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, spec string) {
		ws, err := ParseWindows(spec)
		if err != nil {
			if ws != nil {
				t.Fatalf("error must come with nil windows: %v / %+v", err, ws)
			}
			return
		}
		for _, w := range ws {
			anyDay := false
			for _, d := range w.Days {
				anyDay = anyDay || d
			}
			if !anyDay {
				t.Fatalf("parsed window covers no days: %+v (spec %q)", w, spec)
			}
			if w.Start < 0 || w.Start > 1439 || w.End < 0 || w.End > 1440 {
				t.Fatalf("window minutes out of range: %+v (spec %q)", w, spec)
			}
		}
		// Whatever parsed must be evaluable without panicking.
		_ = InWindow(ws, anchor)
	})
}
