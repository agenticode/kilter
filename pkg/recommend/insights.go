package recommend

import (
	"fmt"
	"sort"

	"github.com/agenticode/kilter/pkg/model"
)

// Insights runs the detection layer over current state: predictive findings
// derived from learned distributions and trends, each with its evidence.
// This is read-only — insights inform operators and future plans.
func (r *Recommender) Insights(snap *model.ClusterSnapshot) []model.Insight {
	r.mu.Lock()
	defer r.mu.Unlock()

	type current struct{ req, lim model.Resources }
	currents := map[model.ContainerKey]current{}
	for i := range snap.Pods {
		pod := &snap.Pods[i]
		if pod.Phase != "" && pod.Phase != "Running" {
			continue
		}
		for _, c := range pod.Containers {
			key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
			currents[key] = current{req: c.Requests, lim: c.Limits}
		}
	}

	var out []model.Insight
	for key, cur := range currents {
		st := r.states[key]
		if st == nil || st.samples < 10 {
			continue
		}

		// Predictive OOM risk: learned memory peak + observed growth versus
		// the container's memory limit.
		if lim := float64(cur.lim.MemoryBytes); lim > 0 {
			peak := st.mem.Max()
			_, mf := st.memDet.Analyze()
			slopePerDay := mf.TrendPerDay * mf.Mean // bytes/day
			switch {
			case peak >= 0.95*lim:
				out = append(out, model.Insight{
					Kind: "oom-risk", Severity: "critical",
					Workload: key.Workload, Container: key.Container,
					Message: fmt.Sprintf("memory peak %dMi is within 5%% of the %dMi limit — OOMKill imminent",
						int64(peak)>>20, cur.lim.MemoryBytes>>20),
					At: snap.Timestamp,
				})
			case slopePerDay > 0 && peak+slopePerDay >= 0.90*lim:
				hours := (0.95*lim - peak) / slopePerDay * 24
				if hours < 0 {
					hours = 0
				}
				out = append(out, model.Insight{
					Kind: "oom-risk", Severity: "warning",
					Workload: key.Workload, Container: key.Container,
					Message: fmt.Sprintf("memory growing %+.0f%%/day; peak %dMi will reach the %dMi limit in ~%.0fh",
						mf.TrendPerDay*100, int64(peak)>>20, cur.lim.MemoryBytes>>20, hours),
					HorizonHours: hours,
					At:           snap.Timestamp,
				})
			}
		}

		// CPU saturation: sustained p95 near the CPU limit means throttling.
		if lim := float64(cur.lim.MilliCPU); lim > 0 {
			p95 := st.cpu.Percentile(0.95)
			if p95 >= 0.90*lim {
				out = append(out, model.Insight{
					Kind: "cpu-saturation", Severity: "warning",
					Workload: key.Workload, Container: key.Container,
					Message: fmt.Sprintf("cpu p95 %dm is ≥90%% of the %dm limit — sustained throttling likely",
						int64(p95), cur.lim.MilliCPU),
					At: snap.Timestamp,
				})
			}
		}

		// Behavior signal: sustained growth is worth knowing about even
		// before it threatens a limit.
		if class, cf := st.cpuDet.Analyze(); class == "growing" {
			out = append(out, model.Insight{
				Kind: "growth-trend", Severity: "info",
				Workload: key.Workload, Container: key.Container,
				Message: fmt.Sprintf("cpu demand growing %+.0f%%/day — predictive headroom applied", cf.TrendPerDay*100),
				At:      snap.Timestamp,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		si, sj := sevRank(out[i].Severity), sevRank(out[j].Severity)
		if si != sj {
			return si > sj
		}
		return out[i].Workload.String() < out[j].Workload.String()
	})
	return out
}

func sevRank(s string) int {
	switch s {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	}
	return 0
}
