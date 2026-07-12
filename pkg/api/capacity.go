package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/forecast"
	"github.com/agenticode/kilter/pkg/model"
)

// demandTracker learns one cluster's aggregate demand over time and predicts
// capacity exhaustion — the "will we run out?" half of AIOps, next to the
// "are we wasting?" half the recommender covers.
type demandTracker struct {
	mu  sync.Mutex
	cpu *forecast.HoltWinters // trend-only, milliCPU
	mem *forecast.HoltWinters // trend-only, bytes

	// recent series for external foundation-model forecasters.
	cpuHist, memHist []float64

	lastAt   time.Time
	interval time.Duration // EWMA of ingest cadence
	points   int
}

const demandHistCap = 288

func newDemandTracker() *demandTracker {
	cpu, _ := forecast.NewHoltWinters(0.3, 0.05, 0, 0)
	mem, _ := forecast.NewHoltWinters(0.3, 0.05, 0, 0)
	return &demandTracker{cpu: cpu, mem: mem}
}

// observe folds one snapshot's measured aggregate demand.
func (d *demandTracker) observe(snap *model.ClusterSnapshot) {
	var cpu, mem float64
	for _, u := range snap.Usage {
		cpu += float64(u.MilliCPU)
		mem += float64(u.MemoryBytes)
	}
	if cpu == 0 && mem == 0 {
		return // no usage data in this snapshot
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cpu.Add(cpu)
	d.mem.Add(mem)
	d.cpuHist = appendCapped(d.cpuHist, cpu, demandHistCap)
	d.memHist = appendCapped(d.memHist, mem, demandHistCap)
	if !d.lastAt.IsZero() {
		gap := snap.Timestamp.Sub(d.lastAt)
		if gap > 0 && gap < time.Hour {
			if d.interval == 0 {
				d.interval = gap
			} else {
				d.interval = (d.interval*4 + gap) / 5
			}
		}
	}
	d.lastAt = snap.Timestamp
	d.points++
}

func appendCapped(s []float64, v float64, cap_ int) []float64 {
	s = append(s, v)
	if len(s) > cap_ {
		s = s[len(s)-cap_:]
	}
	return s
}

// forecastPeak predicts the max demand over the horizon, preferring the
// external forecaster when configured and healthy.
func (d *demandTracker) forecastPeak(ctx context.Context, rf *forecast.RemoteForecaster, horizon time.Duration) (cpu, mem float64, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.points < 10 || d.interval <= 0 {
		return 0, 0, false
	}
	steps := int(horizon / d.interval)
	if steps < 1 {
		steps = 1
	}
	if steps > 5000 {
		steps = 5000
	}
	if rf != nil {
		fc, err1 := rf.Forecast(ctx, d.cpuHist, steps)
		fm, err2 := rf.Forecast(ctx, d.memHist, steps)
		if err1 == nil && err2 == nil {
			return maxOf(fc), maxOf(fm), true
		}
		// fall through to built-in models on any remote failure
	}
	for h := 1; h <= steps; h++ {
		if v := d.cpu.Forecast(h); v > cpu {
			cpu = v
		}
		if v := d.mem.Forecast(h); v > mem {
			mem = v
		}
	}
	return cpu, mem, d.cpu.Ready()
}

func maxOf(s []float64) float64 {
	m := 0.0
	for _, v := range s {
		if v > m {
			m = v
		}
	}
	return m
}

// capacityInsights compares forecast demand against current allocatable.
func capacityInsights(ctx context.Context, d *demandTracker, rf *forecast.RemoteForecaster, snap *model.ClusterSnapshot) []model.Insight {
	if d == nil || snap == nil {
		return nil
	}
	var alloc model.Resources
	for i := range snap.Nodes {
		if snap.Nodes[i].Ready && !snap.Nodes[i].Unschedulable {
			alloc = alloc.Add(snap.Nodes[i].Allocatable)
		}
	}
	if alloc.MilliCPU == 0 || alloc.MemoryBytes == 0 {
		return nil
	}
	const horizon = 24 * time.Hour
	cpuPeak, memPeak, ok := d.forecastPeak(ctx, rf, horizon)
	if !ok {
		return nil
	}
	var out []model.Insight
	check := func(kind string, peak, allocatable float64, unit func(float64) string) {
		ratio := peak / allocatable
		sev := ""
		switch {
		case ratio >= 0.95:
			sev = "critical"
		case ratio >= 0.85:
			sev = "warning"
		default:
			return
		}
		out = append(out, model.Insight{
			Kind: "capacity-exhaustion", Severity: sev,
			Message: fmt.Sprintf("forecast %s demand peaks at %s within 24h — %.0f%% of schedulable capacity (%s); add nodes or let the autoscaler know",
				kind, unit(peak), ratio*100, unit(allocatable)),
			HorizonHours: 24,
			At:           snap.Timestamp,
		})
	}
	check("cpu", cpuPeak, float64(alloc.MilliCPU), func(v float64) string { return fmt.Sprintf("%.1f vCPU", v/1000) })
	check("memory", memPeak, float64(alloc.MemoryBytes), func(v float64) string { return fmt.Sprintf("%.1f GiB", v/(1<<30)) })
	return out
}
