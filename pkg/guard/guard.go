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
// namespace annotation beats the given default.
func ModeFor(snap *model.ClusterSnapshot, ref model.WorkloadRef, def string) string {
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
	Start int     // minutes since midnight
	End   int     // minutes; End <= Start means the window crosses midnight
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
		var sh, sm, eh, em int
		if _, err := fmt.Sscanf(fields[1], "%d:%d-%d:%d", &sh, &sm, &eh, &em); err != nil {
			return nil, fmt.Errorf("guard: bad time range %q", fields[1])
		}
		if sh < 0 || sh > 23 || eh < 0 || eh > 24 || sm < 0 || sm > 59 || em < 0 || em > 59 {
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

// InWindow reports whether t falls inside any window (empty list = always).
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
	// MaxNotReadyFraction of nodes before tripping. Default 0.2.
	MaxNotReadyFraction float64
	// MaxPendingPods before tripping (absolute). Default 10.
	MaxPendingPods int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.MaxNotReadyFraction <= 0 {
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
func Breaker(snap *model.ClusterSnapshot, cfg BreakerConfig) (open bool, reasons []string) {
	cfg = cfg.withDefaults()
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
	if total > 0 && float64(notReady)/float64(total) > cfg.MaxNotReadyFraction {
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
