// Package guard is Kilter's policy layer — the operator's steering wheel over
// the automation. It answers, before any step runs: is this workload opted
// in, is the cluster frozen, are we inside the change window, and is the
// cluster healthy enough to touch at all (circuit breaker)?
//
// Policy sources are plain Kubernetes annotations, so guardrails work with
// GitOps, need no CRDs, and are visible to anyone with kubectl:
//
//	kilter.dev/mode: off|recommend|apply   on a workload or namespace
//	kilter.dev/freeze: "true"              on the kube-system namespace
package guard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// Modes, most restrictive first.
const (
	ModeOff       = "off"       // Kilter never touches or moves this workload
	ModeRecommend = "recommend" // learn + recommend, never act
	ModeApply     = "apply"     // full automation
)

// ModeFor resolves a workload's effective mode: workload annotation beats
// namespace annotation beats the given default. Invalid or empty annotations
// are ignored at each level; if the default is also invalid, ModeApply is
// assumed. If the snapshot holds duplicate entries for ref, the first one
// with a valid mode wins.
//
// A nil snapshot yields ModeRecommend: with no snapshot the annotations —
// including any opt-outs — are invisible, so never act on the workload.
func ModeFor(snap *model.ClusterSnapshot, ref model.WorkloadRef, def string) string {
	if snap == nil {
		return ModeRecommend
	}
	for _, w := range snap.Workloads {
		if w.Ref == ref && validMode(w.Mode) {
			return w.Mode
		}
	}
	if m := snap.NamespaceModes[ref.Namespace]; validMode(m) {
		return m
	}
	if validMode(def) {
		return def
	}
	return ModeApply
}

func validMode(m string) bool {
	return m == ModeOff || m == ModeRecommend || m == ModeApply
}

// Window is a recurring weekly change window, e.g. "Mon-Fri 22:00-06:00".
// Node surgery (cordon/evict/delete) is only allowed inside a window;
// an empty window list means "always allowed".
type Window struct {
	Days  [7]bool // time.Weekday indexing (Sunday=0)
	Start int     // minutes since midnight, 0..1439 (inclusive bound)
	// End in minutes since midnight, 0..1440 (24:00 == 1440, exclusive
	// bound). End <= Start means the window crosses midnight into the next
	// day; Start == End therefore means a full 24 h starting at Start.
	End int
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseWindows parses a comma-separated list like
// "Mon-Fri 22:00-06:00, Sat-Sun 00:00-24:00".
func ParseWindows(spec string) ([]Window, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []Window
	for _, part := range strings.Split(spec, ",") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) != 2 {
			return nil, fmt.Errorf("guard: window %q must be 'Days HH:MM-HH:MM'", part)
		}
		var w Window
		if err := parseDays(fields[0], &w); err != nil {
			return nil, err
		}
		lo, hi, ok := strings.Cut(fields[1], "-")
		if !ok {
			return nil, fmt.Errorf("guard: bad time range %q", fields[1])
		}
		sh, sm, ok1 := parseHHMM(lo)
		eh, em, ok2 := parseHHMM(hi)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("guard: bad time range %q", fields[1])
		}
		// Start must stay strictly before end-of-day (0..23:59); the end may
		// be 24:00 exactly, but never past it — a Window carries minutes in
		// [0,1440] and InWindow relies on that.
		if sh > 23 || sm > 59 || em > 59 || eh > 24 || (eh == 24 && em > 0) {
			return nil, fmt.Errorf("guard: time out of range in %q", fields[1])
		}
		w.Start, w.End = sh*60+sm, eh*60+em
		out = append(out, w)
	}
	return out, nil
}

func parseDays(s string, w *Window) error {
	for _, rng := range strings.Split(strings.ToLower(s), "+") {
		parts := strings.SplitN(rng, "-", 2)
		from, ok := dayNames[parts[0]]
		if !ok {
			return fmt.Errorf("guard: unknown day %q", parts[0])
		}
		to := from
		if len(parts) == 2 {
			if to, ok = dayNames[parts[1]]; !ok {
				return fmt.Errorf("guard: unknown day %q", parts[1])
			}
		}
		d := from
		for {
			w.Days[d] = true
			if d == to {
				break
			}
			d = (d + 1) % 7
		}
	}
	return nil
}

// parseHHMM parses a strict "H:MM"-style clock time into hour and minute.
// Only ASCII digits around a single colon are accepted — no signs, no
// spaces, no trailing text. (fmt.Sscanf would silently accept "+2:00" and
// ignore trailing garbage like "06:00:30", turning typos into wrong windows.)
func parseHHMM(s string) (h, m int, ok bool) {
	hs, ms, found := strings.Cut(s, ":")
	if !found || !isDigits(hs) || !isDigits(ms) {
		return 0, 0, false
	}
	var herr, merr error
	h, herr = strconv.Atoi(hs)
	m, merr = strconv.Atoi(ms)
	if herr != nil || merr != nil { // overflow on absurd digit runs
		return 0, 0, false
	}
	return h, m, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// InWindow reports whether t falls inside any window (empty list = always).
// Times compare as wall clock in t's location: the caller chooses the
// timezone the windows are written in by choosing t's location.
func InWindow(windows []Window, t time.Time) bool {
	if len(windows) == 0 {
		return true
	}
	min := t.Hour()*60 + t.Minute()
	for _, w := range windows {
		if w.End > w.Start { // same-day window
			if w.Days[t.Weekday()] && min >= w.Start && min < w.End {
				return true
			}
		} else { // crosses midnight: evening part or morning part of next day
			if w.Days[t.Weekday()] && min >= w.Start {
				return true
			}
			prev := (t.Weekday() + 6) % 7
			if w.Days[prev] && min < w.End {
				return true
			}
		}
	}
	return false
}

// BreakerConfig tunes the automation circuit breaker.
type BreakerConfig struct {
	// MaxNotReadyFraction is the highest tolerated fraction of NotReady
	// nodes; strictly exceeding it trips the breaker. Zero, negative, and
	// NaN all mean "use the default" (0.2); values >= 1 can never be
	// exceeded and so disable the node-health check.
	MaxNotReadyFraction float64
	// MaxPendingPods is the highest tolerated number of Pending pods;
	// strictly exceeding it trips the breaker. Values <= 0 mean "use the
	// default" (10).
	MaxPendingPods int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	// !(x > 0) rather than x <= 0: NaN fails every comparison, so a NaN
	// fraction would otherwise slip through and silently disable the
	// node-health check (ratio > NaN is always false).
	if !(c.MaxNotReadyFraction > 0) {
		c.MaxNotReadyFraction = 0.2
	}
	if c.MaxPendingPods <= 0 {
		c.MaxPendingPods = 10
	}
	return c
}

// Breaker decides whether the cluster is healthy enough for automation.
// Optimizing a struggling cluster is how incidents become outages: when the
// breaker is open, Kilter observes and recommends but touches nothing.
//
// Degenerate snapshots fail safe: a nil snapshot or an empty node list means
// collection failed or the cluster is gone, and either way there is nothing
// Kilter should be touching, so the breaker opens. A freeze annotation
// short-circuits with a single reason; other health signals are not evaluated
// behind it.
func Breaker(snap *model.ClusterSnapshot, cfg BreakerConfig) (open bool, reasons []string) {
	cfg = cfg.withDefaults()
	if snap == nil {
		return true, []string{"no cluster snapshot"}
	}
	if snap.Frozen {
		return true, []string{"cluster frozen via kilter.dev/freeze annotation on kube-system"}
	}
	notReady, total := 0, 0
	for i := range snap.Nodes {
		total++
		if !snap.Nodes[i].Ready {
			notReady++
		}
	}
	if total == 0 {
		reasons = append(reasons, "snapshot has no nodes")
	} else if float64(notReady)/float64(total) > cfg.MaxNotReadyFraction {
		reasons = append(reasons, fmt.Sprintf("%d/%d nodes NotReady", notReady, total))
	}
	pending := 0
	for i := range snap.Pods {
		if snap.Pods[i].Phase == "Pending" {
			pending++
		}
	}
	if pending > cfg.MaxPendingPods {
		reasons = append(reasons, fmt.Sprintf("%d pods Pending (scheduler under pressure)", pending))
	}
	return len(reasons) > 0, reasons
}
