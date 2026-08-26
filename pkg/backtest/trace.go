package backtest

import (
	"fmt"
	"math"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/model"
)

// Synthetic traces with analytically-known oracles
// ================================================
//
// These generators are the harness's acceptance evidence (§4.4, §9 unit 2):
// histories whose correct answer is known in closed form, so a scorecard can
// be checked against arithmetic rather than against itself. They are exported
// rather than test-only because unit 5 (what-if) and the `kilter backtest
// --demo` path want the same fixtures, and because a user asking "what does
// this number mean" deserves a runnable example.
//
// The parameters are chosen so the order statistics the oracle is defined on
// land exactly on generated levels at the default 5-minute cadence and 24h
// scoring horizon (288 samples per window):
//
//   steady        every sample at Base          → p95 = Base,  max = BaseMem
//   diurnal       12h at Peak, 12h at Base      → p95 = Peak,  max = PeakMem
//   bursty        1 sample in 24 at Peak (4.2%) → p95 = Base,  max = PeakMem
//   regime-change Base, then Peak from midpoint → per-window, one level each
//
// The bursty case is the one that makes the memory/CPU asymmetry visible: a
// spike narrower than the 5% tail leaves the CPU oracle at the baseline (a
// throttle is survivable) while the memory oracle must still cover the peak
// (an OOM is not) — exactly the policy split pkg/recommend implements.
//
// Noise breaks the closed form on purpose: NoisePct > 0 produces a history
// whose oracle must be recomputed rather than asserted, which is what the
// determinism and dominance tests want, and what §10 means by "clean
// synthetic evals overstate quality".

// TraceKind enumerates the archetypes.
type TraceKind string

const (
	TraceSteady       TraceKind = "steady"
	TraceDiurnal      TraceKind = "diurnal"
	TraceBursty       TraceKind = "bursty"
	TraceRegimeChange TraceKind = "regime-change"
)

// burstEvery is the bursty archetype's spike spacing in samples. 24 samples
// at the 5-minute default is one spike every two hours: 12 per day, 4.17% of
// a 288-sample window, comfortably inside the 5% tail the CPU oracle ignores
// and comfortably above the histogram's 1e-4 negligible-weight floor so the
// memory peak estimator still sees it.
const burstEvery = 24

// diurnalPeakFrom and diurnalPeakTo bound the daily peak in UTC hours.
const (
	diurnalPeakFrom = 8
	diurnalPeakTo   = 20
)

// TraceSpec describes a synthetic history. Zero fields take documented
// defaults; Build validates the rest.
type TraceSpec struct {
	Cluster   string    // default "backtest"
	Kind      TraceKind // default TraceSteady
	Start     time.Time // required; truncated to the sample interval
	Days      int       // default 7
	Interval  time.Duration
	Workloads int    // containers in the trace, default 1
	Namespace string // default "default"

	BaseMilliCPU int64 // default 200
	PeakMilliCPU int64 // default 3 × Base
	BaseMemBytes int64 // default 512 MiB
	PeakMemBytes int64 // default 3/2 × Base

	// OversizeFactor sets the requests the workloads actually ran with, as a
	// multiple of the trace's highest level. Default 3: a realistically
	// over-provisioned cluster, which is what leaves savings on the table for
	// a policy to find and for a refusal to forgo.
	OversizeFactor float64

	// NoisePct jitters every sample by ±NoisePct (0.05 = ±5%) using a
	// deterministic hash of (NoiseSeed, workload, sample) — no global RNG, no
	// dependence on iteration order. Default 0 (clean).
	NoisePct  float64
	NoiseSeed uint64

	// DeployAt injects a deploy event per workload at these offsets from
	// Start, so the post-change-soak refusal has something real to fire on.
	// The regime-change archetype adds one at the level shift automatically.
	DeployAt []time.Duration
	// OOMAt injects an OOMKill event per workload at these offsets, so
	// refusal quality (a refusal over a genuinely turbulent window) can be
	// exercised without hand-writing a store.
	OOMAt []time.Duration

	// NodeInstanceType prices the trace's nodes. Default "m5.2xlarge".
	NodeInstanceType string
	// Nodes is the node count, default 2.
	Nodes int
}

// Trace is a generated history plus the metadata needed to score it.
type Trace struct {
	Spec      TraceSpec
	Cluster   string
	Start     time.Time
	End       time.Time // exclusive: Start + Days
	Snapshots []*model.ClusterSnapshot
	Events    []evidence.EvidenceEvent
	Keys      []model.ContainerKey
	// ShiftAt is the regime-change instant (zero for other archetypes).
	ShiftAt time.Time
}

// Source adapts the trace to the harness input.
func (t *Trace) Source() SnapshotSource { return SliceSource(t.Snapshots) }

// Store builds an in-memory substrate holding the trace's events. The usage
// series is deliberately not mirrored into it: the harness reads usage from
// the snapshot stream (the same bytes the engine learns from), so writing a
// second copy would create two sources of truth for one fact.
func (t *Trace) Store() (*evidence.Memory, error) {
	m, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		return nil, err
	}
	for _, ev := range t.Events {
		if err := m.Append(ev); err != nil {
			return nil, fmt.Errorf("backtest: seeding trace evidence: %w", err)
		}
	}
	return m, nil
}

func (s TraceSpec) withDefaults() TraceSpec {
	if s.Cluster == "" {
		s.Cluster = "backtest"
	}
	if s.Kind == "" {
		s.Kind = TraceSteady
	}
	if s.Days == 0 {
		s.Days = 7
	}
	if s.Interval == 0 {
		s.Interval = 5 * time.Minute
	}
	if s.Workloads == 0 {
		s.Workloads = 1
	}
	if s.Namespace == "" {
		s.Namespace = "default"
	}
	if s.BaseMilliCPU == 0 {
		s.BaseMilliCPU = 200
	}
	if s.PeakMilliCPU == 0 {
		s.PeakMilliCPU = 3 * s.BaseMilliCPU
	}
	if s.BaseMemBytes == 0 {
		s.BaseMemBytes = 512 << 20
	}
	if s.PeakMemBytes == 0 {
		s.PeakMemBytes = s.BaseMemBytes * 3 / 2
	}
	if s.OversizeFactor == 0 {
		s.OversizeFactor = 3
	}
	if s.NodeInstanceType == "" {
		s.NodeInstanceType = "m5.2xlarge"
	}
	if s.Nodes == 0 {
		s.Nodes = 2
	}
	return s
}

func (s TraceSpec) validate() error {
	if s.Start.IsZero() {
		return fmt.Errorf("backtest: TraceSpec needs a Start")
	}
	if s.Days < 1 || s.Days > 400 {
		return fmt.Errorf("backtest: TraceSpec Days %d out of [1,400]", s.Days)
	}
	if s.Interval <= 0 || s.Interval > 24*time.Hour {
		return fmt.Errorf("backtest: TraceSpec Interval %v out of (0,24h]", s.Interval)
	}
	if s.Workloads < 1 || s.Workloads > 1000 {
		return fmt.Errorf("backtest: TraceSpec Workloads %d out of [1,1000]", s.Workloads)
	}
	if s.BaseMilliCPU <= 0 || s.BaseMemBytes <= 0 {
		return fmt.Errorf("backtest: TraceSpec base levels must be > 0")
	}
	if s.PeakMilliCPU < s.BaseMilliCPU || s.PeakMemBytes < s.BaseMemBytes {
		return fmt.Errorf("backtest: TraceSpec peak levels must be >= base levels")
	}
	if !(s.OversizeFactor >= 1) || math.IsInf(s.OversizeFactor, 0) {
		return fmt.Errorf("backtest: TraceSpec OversizeFactor %v must be finite and >= 1", s.OversizeFactor)
	}
	if !(s.NoisePct >= 0) || !(s.NoisePct < 0.5) {
		return fmt.Errorf("backtest: TraceSpec NoisePct %v out of [0,0.5)", s.NoisePct)
	}
	if s.Nodes < 1 || s.Nodes > 100 {
		return fmt.Errorf("backtest: TraceSpec Nodes %d out of [1,100]", s.Nodes)
	}
	switch s.Kind {
	case TraceSteady, TraceDiurnal, TraceBursty, TraceRegimeChange:
	default:
		return fmt.Errorf("backtest: unknown TraceKind %q", s.Kind)
	}
	if s.Kind == TraceRegimeChange && s.Days < 2 {
		return fmt.Errorf("backtest: the regime-change archetype needs at least 2 days")
	}
	return nil
}

// levelAt returns the true (pre-noise) levels for workload w at sample i.
func (s TraceSpec) levelAt(i int, ts time.Time, shift time.Time) (int64, int64) {
	switch s.Kind {
	case TraceDiurnal:
		if h := ts.UTC().Hour(); h >= diurnalPeakFrom && h < diurnalPeakTo {
			return s.PeakMilliCPU, s.PeakMemBytes
		}
	case TraceBursty:
		if i%burstEvery == 0 {
			return s.PeakMilliCPU, s.PeakMemBytes
		}
	case TraceRegimeChange:
		if !ts.Before(shift) {
			return s.PeakMilliCPU, s.PeakMemBytes
		}
	}
	return s.BaseMilliCPU, s.BaseMemBytes
}

// jitter applies the deterministic ±NoisePct band. The multiplier is derived
// by hashing the coordinates rather than by stepping a stateful generator, so
// a sample's value depends only on where it is — never on how many samples
// were produced before it, and never on iteration order.
func (s TraceSpec) jitter(v int64, w, i int) int64 {
	if !(s.NoisePct > 0) || v <= 0 {
		return v
	}
	u := float64(splitmix64(s.NoiseSeed^mix(uint64(w), uint64(i)))>>11) / float64(uint64(1)<<53)
	out := int64(math.Round(float64(v) * (1 + s.NoisePct*(2*u-1))))
	if out < 1 {
		out = 1
	}
	return out
}

// mix folds two coordinates into one seed. Multiplying by distinct odd
// constants keeps neighbouring (w, i) pairs far apart in the hash space.
func mix(a, b uint64) uint64 { return a*0x9E3779B97F4A7C15 ^ b*0xBF58476D1CE4E5B9 }

// splitmix64 is the standard finalizer: a bijective avalanche over 64 bits.
// Vendored (four lines) rather than taken from math/rand so the trace is a
// pure function of its spec, with no dependence on any global source.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// Build generates the trace.
func (s TraceSpec) Build() (*Trace, error) {
	s = s.withDefaults()
	if err := s.validate(); err != nil {
		return nil, err
	}
	start := s.Start.UTC().Truncate(s.Interval)
	span := time.Duration(s.Days) * 24 * time.Hour
	end := start.Add(span)
	shift := time.Time{}
	if s.Kind == TraceRegimeChange {
		// Land the shift on a whole day so it coincides with a decision
		// instant at the default 24h cadence: that is what lets the
		// post-change-soak refusal and the regime's first violation be
		// attributed to a specific, nameable instant.
		shift = start.Add(time.Duration(s.Days/2) * 24 * time.Hour)
	}

	// Requests the workloads actually ran with: a multiple of the highest
	// level the trace ever reaches, so the unchanged sizing is safe by
	// construction and every violation the scorecard reports is one the
	// policy under test introduced.
	peakCPU, peakMem := s.PeakMilliCPU, s.PeakMemBytes
	if s.NoisePct > 0 {
		peakCPU = int64(math.Ceil(float64(peakCPU) * (1 + s.NoisePct)))
		peakMem = int64(math.Ceil(float64(peakMem) * (1 + s.NoisePct)))
	}
	current := model.Resources{
		MilliCPU:    int64(math.Ceil(float64(peakCPU) * s.OversizeFactor)),
		MemoryBytes: int64(math.Ceil(float64(peakMem) * s.OversizeFactor)),
	}

	keys := make([]model.ContainerKey, 0, s.Workloads)
	refs := make([]model.WorkloadRef, 0, s.Workloads)
	for w := 0; w < s.Workloads; w++ {
		ref := model.WorkloadRef{Kind: model.KindDeployment, Namespace: s.Namespace,
			Name: fmt.Sprintf("app-%02d", w)}
		refs = append(refs, ref)
		keys = append(keys, model.ContainerKey{Workload: ref, Container: "app"})
	}

	nodes := s.buildNodes()
	nSamples := int(span / s.Interval)
	snaps := make([]*model.ClusterSnapshot, 0, nSamples)
	for i := 0; i < nSamples; i++ {
		ts := start.Add(time.Duration(i) * s.Interval)
		snap := &model.ClusterSnapshot{
			ClusterID: s.Cluster, Timestamp: ts,
			Nodes:     nodes,
			Pods:      make([]model.PodSpec, 0, s.Workloads),
			Workloads: make([]model.WorkloadInfo, 0, s.Workloads),
			Usage:     make([]model.Usage, 0, s.Workloads),
		}
		for w := 0; w < s.Workloads; w++ {
			uid := fmt.Sprintf("pod-%02d", w)
			snap.Pods = append(snap.Pods, model.PodSpec{
				UID: uid, Name: fmt.Sprintf("app-%02d-0", w), Namespace: s.Namespace,
				Workload: refs[w], NodeName: nodes[w%len(nodes)].Name,
				Phase: "Running", QOSClass: "Guaranteed", CreatedAt: start,
				Containers: []model.ContainerSpec{{
					Name: "app", Requests: current, Limits: current,
				}},
			})
			snap.Workloads = append(snap.Workloads, model.WorkloadInfo{
				Ref: refs[w], Replicas: 1, Ready: 1,
			})
			cpu, mem := s.levelAt(i, ts, shift)
			snap.Usage = append(snap.Usage, model.Usage{
				Key: keys[w], PodUID: uid, Timestamp: ts,
				MilliCPU:      s.jitter(cpu, w, i),
				MemoryBytes:   s.jitter(mem, w, i),
				WindowSeconds: int32(s.Interval / time.Second),
			})
		}
		snaps = append(snaps, snap)
	}

	return &Trace{
		Spec: s, Cluster: s.Cluster, Start: start, End: end,
		Snapshots: snaps, Keys: keys, ShiftAt: shift,
		Events: s.buildEvents(keys, start, shift),
	}, nil
}

func (s TraceSpec) buildNodes() []model.NodeSpec {
	// Allocatable is deliberately a shade under capacity, as a real kubelet
	// reports it: system reserved plus eviction threshold.
	capacity := model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30}
	alloc := model.Resources{MilliCPU: 7800, MemoryBytes: 30 << 30}
	out := make([]model.NodeSpec, 0, s.Nodes)
	for n := 0; n < s.Nodes; n++ {
		out = append(out, model.NodeSpec{
			Name:         fmt.Sprintf("node-%02d", n),
			Capacity:     capacity,
			Allocatable:  alloc,
			Ready:        true,
			InstanceType: s.NodeInstanceType,
			Provider:     "aws",
			Region:       "us-east-1",
			Zone:         "us-east-1a",
			Labels:       map[string]string{"kubernetes.io/arch": "amd64"},
		})
	}
	return out
}

func (s TraceSpec) buildEvents(keys []model.ContainerKey, start, shift time.Time) []evidence.EvidenceEvent {
	var out []evidence.EvidenceEvent
	add := func(kind, sev string, at time.Time, key model.ContainerKey, detail string) {
		out = append(out, evidence.EvidenceEvent{
			At: at, Kind: kind, Severity: sev,
			Subject: evidence.ContainerSubject(s.Cluster, key),
			Attrs:   map[string]string{"trace": string(s.Kind), "detail": detail},
		})
	}
	for _, key := range keys {
		for _, off := range s.DeployAt {
			add(evidence.EventDeploy, evidence.SeverityInfo, start.Add(off), key, "synthetic deploy")
		}
		for _, off := range s.OOMAt {
			add(evidence.EventOOMKill, evidence.SeverityCritical, start.Add(off), key, "synthetic oomkill")
		}
		if !shift.IsZero() {
			// The level shift is modelled as what usually causes one: a
			// deploy. It gives the post-change-soak predicate a real event
			// and the scorer a real adverse marker at a known instant.
			add(evidence.EventDeploy, evidence.SeverityInfo, shift, key, "regime shift")
			add(evidence.EventRegimeChange, evidence.SeverityWarning, shift, key, "level shift up")
		}
	}
	return out
}
