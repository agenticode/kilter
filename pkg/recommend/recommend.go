// Package recommend turns learned usage distributions into safe, explainable
// rightsizing recommendations per container. Policy follows hard-won industry
// practice (VPA, CAST AI, ScaleOps):
//
//   - CPU request  = p95(usage) × headroom      (throttling is survivable)
//   - Memory req   = max(p99 × headroom, peak)  (OOM is not)
//   - Any OOMKill observed → memory floor bumps by OOMBumpRatio over the level
//     that OOMed, and the recommendation is never below that floor.
//   - Workloads whose HPA scales on CPU utilization keep their CPU request
//     untouched (changing it silently reshapes HPA math).
//   - Recommendations carry a confidence score; the planner only acts above a
//     threshold, and small deltas are suppressed to avoid churn.
package recommend

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/forecast"
	"github.com/agenticode/kilter/pkg/histogram"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/patterns"
)

// Config tunes the recommendation policy.
type Config struct {
	CPUPercentile    float64       // default 0.95
	CPUHeadroom      float64       // multiplier ≥1, default 1.15
	MemoryPercentile float64       // default 0.99
	MemoryHeadroom   float64       // multiplier ≥1, default 1.20
	OOMBumpRatio     float64       // memory floor bump after OOM, default 1.5
	MinMilliCPU      int64         // request floor, default 10m
	MinMemoryBytes   int64         // request floor, default 32Mi
	MinSamples       int           // samples before recommending, default 30
	MinWindow        time.Duration // observation span before recommending, default 6h
	MinChangeRatio   float64       // suppress deltas smaller than this, default 0.10
	SkipCPUForHPA    bool          // default true
}

// DefaultConfig returns production-grade defaults.
func DefaultConfig() Config {
	return Config{
		CPUPercentile:    0.95,
		CPUHeadroom:      1.15,
		MemoryPercentile: 0.99,
		MemoryHeadroom:   1.20,
		OOMBumpRatio:     1.5,
		MinMilliCPU:      10,
		MinMemoryBytes:   32 << 20,
		MinSamples:       30,
		MinWindow:        6 * time.Hour,
		MinChangeRatio:   0.10,
		SkipCPUForHPA:    true,
	}
}

func (c Config) validate() error {
	if c.CPUPercentile <= 0 || c.CPUPercentile > 1 || c.MemoryPercentile <= 0 || c.MemoryPercentile > 1 {
		return fmt.Errorf("recommend: percentiles out of range")
	}
	if c.CPUHeadroom < 1 || c.MemoryHeadroom < 1 || c.OOMBumpRatio < 1 {
		return fmt.Errorf("recommend: headroom/bump must be >= 1")
	}
	if c.MinChangeRatio < 0 || c.MinChangeRatio >= 1 {
		return fmt.Errorf("recommend: MinChangeRatio out of [0,1)")
	}
	return nil
}

// containerState is the learned memory for one container key (aggregated
// across all pod replicas of the workload, VPA-style).
type containerState struct {
	cpu    *histogram.Histogram
	mem    *histogram.Histogram
	spikes *forecast.SpikeDetector
	cpuDet *patterns.Detector
	memDet *patterns.Detector
	// restoredClass keeps the pre-restart behavior class active until the
	// fresh detector has enough samples to classify on its own.
	restoredClass patterns.Class

	firstSample time.Time
	lastSample  time.Time
	samples     int

	oomCount int
	// oomFloorBytes is OOMBumpRatio × the memory level that OOMed.
	oomFloorBytes int64
	lastOOM       time.Time

	// per-pod restart counts to detect *new* OOM events, pruned with pods.
	podRestarts map[string]int32
}

// Recommendation is one container's sizing decision.
type Recommendation struct {
	Key            model.ContainerKey `json:"key"`
	CurrentRequest model.Resources    `json:"currentRequest"`
	TargetRequest  model.Resources    `json:"targetRequest"`
	CurrentLimit   model.Resources    `json:"currentLimit"`
	TargetLimit    model.Resources    `json:"targetLimit"`
	Confidence     float64            `json:"confidence"` // 0..1
	Samples        int                `json:"samples"`
	WindowHours    float64            `json:"windowHours"`
	OOMCount       int                `json:"oomCount"`
	CPUSkipped     bool               `json:"cpuSkipped,omitempty"` // HPA-on-CPU guard
	Class          patterns.Class     `json:"class,omitempty"`      // learned behavior class
	Reason         string             `json:"reason"`
}

// Delta returns request savings (positive = shrink) per dimension.
func (r Recommendation) Delta() model.Resources {
	return r.CurrentRequest.Sub(r.TargetRequest)
}

// Recommender ingests snapshots and produces recommendations. Safe for
// concurrent use.
type Recommender struct {
	mu     sync.Mutex
	cfg    Config
	states map[model.ContainerKey]*containerState
}

// New creates a Recommender.
func New(cfg Config) (*Recommender, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Recommender{cfg: cfg, states: map[model.ContainerKey]*containerState{}}, nil
}

func newState() *containerState {
	sd, _ := forecast.NewSpikeDetector(0.05, 4)
	return &containerState{
		cpu:         histogram.MustNew(histogram.DefaultCPUOptions()),
		mem:         histogram.MustNew(histogram.DefaultMemoryOptions()),
		spikes:      sd,
		cpuDet:      &patterns.Detector{},
		memDet:      &patterns.Detector{},
		podRestarts: map[string]int32{},
	}
}

// ObserveSnapshot ingests usage samples and OOM/restart signals.
func (r *Recommender) ObserveSnapshot(snap *model.ClusterSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Index pods by UID for usage attribution and OOM detection.
	livePods := map[string]bool{}
	for i := range snap.Pods {
		pod := &snap.Pods[i]
		livePods[pod.UID] = true
		for _, c := range pod.Containers {
			key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
			st := r.states[key]
			if st == nil {
				st = newState()
				r.states[key] = st
			}
			prev, seen := st.podRestarts[pod.UID]
			if seen && c.RestartCount > prev && c.LastOOMKilled {
				st.oomCount++
				st.lastOOM = snap.Timestamp
				// The level that OOMed: the limit if set, else the request,
				// else current recommendation floor.
				oomedAt := c.Limits.MemoryBytes
				if oomedAt == 0 {
					oomedAt = c.Requests.MemoryBytes
				}
				if oomedAt > 0 {
					bumped := int64(float64(oomedAt) * r.cfg.OOMBumpRatio)
					if bumped > st.oomFloorBytes {
						st.oomFloorBytes = bumped
					}
				}
			}
			st.podRestarts[pod.UID] = c.RestartCount
		}
	}

	for _, u := range snap.Usage {
		st := r.states[u.Key]
		if st == nil {
			st = newState()
			r.states[u.Key] = st
		}
		w := 1.0
		if u.WindowSeconds > 60 {
			w = float64(u.WindowSeconds) / 60.0
		}
		st.cpu.AddSample(float64(u.MilliCPU), w, u.Timestamp)
		st.mem.AddSample(float64(u.MemoryBytes), w, u.Timestamp)
		st.spikes.Observe(float64(u.MilliCPU))
		st.cpuDet.Add(u.Timestamp, float64(u.MilliCPU))
		st.memDet.Add(u.Timestamp, float64(u.MemoryBytes))
		st.samples++
		if st.firstSample.IsZero() || u.Timestamp.Before(st.firstSample) {
			st.firstSample = u.Timestamp
		}
		if u.Timestamp.After(st.lastSample) {
			st.lastSample = u.Timestamp
		}
	}

	// Prune restart bookkeeping for pods that no longer exist.
	for key, st := range r.states {
		for uid := range st.podRestarts {
			if !livePods[uid] {
				delete(st.podRestarts, uid)
			}
		}
		_ = key
	}
}

// hpaCPUWorkloads indexes workloads whose HPA targets CPU utilization,
// mapping to the HPA's owner ("keda" or "" for plain HPA).
func hpaCPUWorkloads(snap *model.ClusterSnapshot) map[model.WorkloadRef]string {
	m := map[model.WorkloadRef]string{}
	for _, w := range snap.Workloads {
		if w.HasHPA && w.HPATargetsCPU {
			m[w.Ref] = w.HPAOwner
		}
	}
	return m
}

// Recommendations computes sizing decisions for every container currently
// present in the snapshot. Containers without enough history return no entry.
func (r *Recommender) Recommendations(snap *model.ClusterSnapshot) []Recommendation {
	r.mu.Lock()
	defer r.mu.Unlock()

	hpaCPU := hpaCPUWorkloads(snap)

	// Deduplicate container keys across replicas; remember current sizing.
	type current struct {
		req, lim model.Resources
	}
	currents := map[model.ContainerKey]current{}
	for i := range snap.Pods {
		pod := &snap.Pods[i]
		if pod.Phase != "" && pod.Phase != "Running" {
			continue
		}
		// Bare pods and Jobs are not rightsized (restart cost/semantics).
		switch pod.Workload.Kind {
		case model.KindBarePod, model.KindJob, model.KindCronJob:
			continue
		}
		for _, c := range pod.Containers {
			key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
			currents[key] = current{req: c.Requests, lim: c.Limits}
		}
	}

	var out []Recommendation
	for key, cur := range currents {
		st := r.states[key]
		if st == nil || st.samples < r.cfg.MinSamples {
			continue
		}
		window := st.lastSample.Sub(st.firstSample)
		if window < r.cfg.MinWindow {
			continue
		}

		hpaOwner, hpaOnCPU := hpaCPU[key.Workload]
		rec := r.recommendOne(key, st, cur.req, cur.lim, hpaOnCPU, hpaOwner, window)
		if rec != nil {
			out = append(out, *rec)
		}
	}
	return out
}

func (r *Recommender) recommendOne(key model.ContainerKey, st *containerState,
	curReq, curLim model.Resources, hpaOnCPU bool, hpaOwner string, window time.Duration) *Recommendation {

	// Adaptive policy: the learned behavior class tunes percentile/headroom
	// on top of the operator's base config.
	class, feats := st.cpuDet.Analyze()
	if class == patterns.ClassUnknown && st.restoredClass != "" {
		class = st.restoredClass // sticky across restarts until relearned
	}
	pol := patterns.PolicyFor(class, r.cfg.CPUPercentile, r.cfg.CPUHeadroom, r.cfg.MemoryHeadroom)

	targetCPU := int64(math.Ceil(st.cpu.Percentile(pol.CPUPercentile) * pol.CPUHeadroom))
	if targetCPU < r.cfg.MinMilliCPU {
		targetCPU = r.cfg.MinMilliCPU
	}

	memP := st.mem.Percentile(r.cfg.MemoryPercentile) * pol.MemoryHeadroom
	memPeak := st.mem.Max()
	targetMem := int64(math.Ceil(math.Max(memP, memPeak)))
	// Predictive sizing for up-trending memory: cover the next day of growth
	// so the workload does not walk into its own ceiling.
	if _, mf := st.memDet.Analyze(); mf.TrendPerDay > 0.05 {
		projected := int64(memPeak * (1 + math.Min(mf.TrendPerDay, 1.0)))
		if projected > targetMem {
			targetMem = projected
		}
	}
	if targetMem < r.cfg.MinMemoryBytes {
		targetMem = r.cfg.MinMemoryBytes
	}
	if st.oomFloorBytes > targetMem {
		targetMem = st.oomFloorBytes
	}

	cpuSkipped := false
	if hpaOnCPU && r.cfg.SkipCPUForHPA {
		targetCPU = curReq.MilliCPU // leave HPA math alone
		cpuSkipped = true
	}

	target := model.Resources{MilliCPU: targetCPU, MemoryBytes: targetMem}

	// Suppress churn: both dimensions within MinChangeRatio of current → skip.
	if !r.significant(curReq.MilliCPU, target.MilliCPU) &&
		!r.significant(curReq.MemoryBytes, target.MemoryBytes) {
		return nil
	}

	// Limits policy: preserve the container's limit:request ratio per dimension.
	// No limit stays no limit. Guaranteed (limit==request) stays Guaranteed.
	targetLim := model.Resources{}
	if curLim.MilliCPU > 0 && curReq.MilliCPU > 0 {
		ratio := float64(curLim.MilliCPU) / float64(curReq.MilliCPU)
		targetLim.MilliCPU = int64(math.Ceil(float64(target.MilliCPU) * ratio))
	}
	if curLim.MemoryBytes > 0 && curReq.MemoryBytes > 0 {
		ratio := float64(curLim.MemoryBytes) / float64(curReq.MemoryBytes)
		targetLim.MemoryBytes = int64(math.Ceil(float64(target.MemoryBytes) * ratio))
		// Memory limit must never sit below the OOM floor.
		if st.oomFloorBytes > 0 && targetLim.MemoryBytes < st.oomFloorBytes {
			targetLim.MemoryBytes = st.oomFloorBytes
		}
	}

	conf := r.confidence(st, window)
	reason := fmt.Sprintf("class=%s (%s); cpu p%.0f=%dm mem p%.0f=%dMi peak=%dMi",
		class, feats,
		pol.CPUPercentile*100, int64(st.cpu.Percentile(pol.CPUPercentile)),
		r.cfg.MemoryPercentile*100, int64(st.mem.Percentile(r.cfg.MemoryPercentile))>>20, int64(st.mem.Max())>>20)
	if st.oomCount > 0 {
		reason += fmt.Sprintf("; %d OOM(s), floor=%dMi", st.oomCount, st.oomFloorBytes>>20)
	}
	if cpuSkipped {
		if hpaOwner != "" {
			reason += fmt.Sprintf("; cpu untouched (%s-managed HPA scales on CPU)", hpaOwner)
		} else {
			reason += "; cpu untouched (HPA scales on CPU)"
		}
	}

	return &Recommendation{
		Key:            key,
		Class:          class,
		CurrentRequest: curReq,
		TargetRequest:  target,
		CurrentLimit:   curLim,
		TargetLimit:    targetLim,
		Confidence:     conf,
		Samples:        st.samples,
		WindowHours:    window.Hours(),
		OOMCount:       st.oomCount,
		CPUSkipped:     cpuSkipped,
		Reason:         reason,
	}
}

func (r *Recommender) significant(cur, target int64) bool {
	if cur == 0 {
		return target != 0
	}
	diff := math.Abs(float64(target-cur)) / float64(cur)
	return diff >= r.cfg.MinChangeRatio
}

// confidence blends history depth, window span, and CPU volatility into 0..1.
func (r *Recommender) confidence(st *containerState, window time.Duration) float64 {
	bySamples := math.Min(1, float64(st.samples)/float64(r.cfg.MinSamples*4))
	byWindow := math.Min(1, window.Hours()/(2*r.cfg.MinWindow.Hours()))
	volatilityPenalty := math.Min(0.5, st.spikes.SpikeRate()*5)
	c := bySamples * byWindow * (1 - volatilityPenalty)
	return math.Round(c*100) / 100
}

// StateCount returns the number of tracked container keys (for metrics).
func (r *Recommender) StateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.states)
}

// GC drops state for containers not seen since the cutoff, returning how many
// were removed. Call periodically to bound memory on churny clusters.
func (r *Recommender) GC(cutoff time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for key, st := range r.states {
		if st.lastSample.Before(cutoff) {
			delete(r.states, key)
			n++
		}
	}
	return n
}

// CheckpointState is the serializable form of one container's learning.
type CheckpointState struct {
	Key         model.ContainerKey   `json:"key"`
	CPU         histogram.Checkpoint `json:"cpu"`
	Memory      histogram.Checkpoint `json:"memory"`
	FirstSample time.Time            `json:"firstSample"`
	LastSample  time.Time            `json:"lastSample"`
	Samples     int                  `json:"samples"`
	OOMCount    int                  `json:"oomCount"`
	OOMFloor    int64                `json:"oomFloor"`
	Class       patterns.Class       `json:"class,omitempty"`
}

// Checkpoint exports all learned state.
func (r *Recommender) Checkpoint() []CheckpointState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CheckpointState, 0, len(r.states))
	for key, st := range r.states {
		class, _ := st.cpuDet.Analyze()
		if class == patterns.ClassUnknown && st.restoredClass != "" {
			class = st.restoredClass
		}
		out = append(out, CheckpointState{
			Key: key, CPU: st.cpu.Checkpoint(), Memory: st.mem.Checkpoint(),
			FirstSample: st.firstSample, LastSample: st.lastSample,
			Samples: st.samples, OOMCount: st.oomCount, OOMFloor: st.oomFloorBytes,
			Class: class,
		})
	}
	return out
}

// Restore loads previously checkpointed state, replacing current state for
// those keys. Corrupt entries are skipped, not fatal.
func (r *Recommender) Restore(states []CheckpointState) (restored int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cs := range states {
		cpu, err := histogram.FromCheckpoint(cs.CPU)
		if err != nil {
			continue
		}
		mem, err := histogram.FromCheckpoint(cs.Memory)
		if err != nil {
			continue
		}
		st := newState()
		st.cpu, st.mem = cpu, mem
		st.firstSample, st.lastSample = cs.FirstSample, cs.LastSample
		st.samples, st.oomCount, st.oomFloorBytes = cs.Samples, cs.OOMCount, cs.OOMFloor
		if cs.Class != "" && cs.Class != patterns.ClassUnknown {
			st.restoredClass = cs.Class
		}
		r.states[cs.Key] = st
		restored++
	}
	return restored
}
