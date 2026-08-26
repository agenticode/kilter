package backtest

import (
	"math"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// The oracle
// ==========
//
// The oracle is the cheapest request that would have produced zero violations
// over a scoring window, computed *in hindsight* from what actually happened.
// It is the reference every policy is measured against, and it is the one
// thing in this package that must never depend on the policy under test: an
// oracle derived from the recommender would score the recommender against
// itself and prove exactly nothing — the same failure mode as a test that
// reads the constant it is checking.
//
// Two structural properties keep that honest, and both are asserted by tests:
//
//  1. The oracle is a pure function of (future usage samples, the violation
//     predicates, Config.StarvationFactor). No recommend.Config, plan.Config
//     or decision.Config value reaches it.
//  2. The set of (container, instant) pairs an oracle is computed for is
//     fixed before any policy runs: every eligible container at every
//     decision instant is scored, whether the engine recommended, refused or
//     was filtered by the planner. A policy therefore cannot change *which*
//     oracles exist, only how far its own sizing lands from them.
//
// Violation predicates (also policy-independent):
//
//   - memory: max(usage) over the window > the sizing's memory request.
//     The request — not the limit — is the ceiling, for two reasons. It is
//     the stricter of the two (pkg/recommend guarantees an emitted limit is
//     never below the emitted request, so request ≤ limit always), and it is
//     the one that cannot be gamed: a policy that hid a peak behind a
//     generous limit while under-reserving would score free memory it never
//     paid for, and the cluster would meet that peak as unreserved node
//     memory — a node-pressure eviction rather than a container OOMKill, but
//     an incident either way. Literal OOMKills observed in the substrate are
//     counted separately as ground truth (Scorecard.MemOOMKills).
//   - CPU: p95(usage) over the window > request × StarvationFactor.
//     Throttling is survivable where an OOM is not, which is why CPU is
//     scored on a percentile and memory on the peak — the same asymmetry
//     pkg/recommend's policy is built on.
//
// Inverting those predicates gives the cheapest safe request directly:
// memory must cover the peak, and CPU must satisfy p95 ≤ R × f, i.e.
// R ≥ p95/f. Because the oracle is the exact infimum of the safe set, any
// zero-violation sizing costs at least as much as the oracle — so a negative
// per-decision gap is not a scoring artifact, it is the signature of an
// under-provisioned decision, and the violation counters will say so.

// windowStats are the order statistics of one scoring window for one
// container, aggregated across all replicas exactly as the recommender
// aggregates them.
type windowStats struct {
	Samples int
	CPUP95  int64 // milliCPU
	MemMax  int64 // bytes
	OOMs    int   // OOMKill events observed in the window (evidence ground truth)
	Adverse bool  // the window carried an adverse event (OOM, throttle, regime change)
}

// usagePoint is one replica's measured usage at one instant.
type usagePoint struct {
	At          time.Time
	MilliCPU    int64
	MemoryBytes int64
}

// percentileInt64 is the nearest-rank percentile of an already-sorted slice:
// the value at position ceil(p·n), 1-indexed. Nearest-rank (rather than an
// interpolating variant) keeps the result an observed value, which is what
// makes the analytic assertions on the synthetic traces exact. An empty
// slice has no percentile and reports zero.
func percentileInt64(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if !(p > 0) { // covers NaN and negatives: the safe end is the smallest value
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	rank := int(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// statsOver computes the window statistics for a slice of usage points.
// The caller has already restricted pts to the scoring window.
func statsOver(pts []usagePoint) windowStats {
	ws := windowStats{Samples: len(pts)}
	if len(pts) == 0 {
		return ws
	}
	cpu := make([]int64, 0, len(pts))
	for _, p := range pts {
		cpu = append(cpu, p.MilliCPU)
		if p.MemoryBytes > ws.MemMax {
			ws.MemMax = p.MemoryBytes
		}
	}
	sort.Slice(cpu, func(i, j int) bool { return cpu[i] < cpu[j] })
	ws.CPUP95 = percentileInt64(cpu, cpuOraclePercentile)
	return ws
}

// cpuOraclePercentile is the percentile the CPU starvation predicate — and
// therefore the CPU oracle — is defined on. It is fixed at p95 by the
// Scorecard's own definition ("future p95 > recommended request × starvation
// factor"); it is deliberately NOT recommend.Config.CPUPercentile, because
// letting the policy choose the yardstick it is measured with is precisely
// the trap this package exists to avoid.
const cpuOraclePercentile = 0.95

// oracleRequest returns the cheapest request that produces zero violations
// over the window: memory covers the observed peak, CPU satisfies
// p95 ≤ request × starvation. A window with no samples has no oracle and
// returns the zero value; callers skip such records.
func oracleRequest(ws windowStats, starvation float64) model.Resources {
	if ws.Samples == 0 {
		return model.Resources{}
	}
	// R ≥ p95/f, and R is an integer count of milliCPU, so the infimum of
	// the safe set is ceil(p95/f). Guard the divisor in positive form so a
	// NaN or non-positive factor falls back to 1 (predicate: p95 > R) rather
	// than producing an Inf or a negative request.
	f := starvation
	if !(f > 0) || math.IsInf(f, 0) {
		f = 1
	}
	cpu := int64(math.Ceil(float64(ws.CPUP95) / f))
	if cpu < 0 {
		cpu = 0
	}
	return model.Resources{MilliCPU: cpu, MemoryBytes: ws.MemMax}
}

// violates reports whether a sizing would have hurt over the window.
// req is the request actually in force (the engine's target where a resize
// was planned, the unchanged current request otherwise).
func violates(ws windowStats, req model.Resources, starvation float64) (mem, cpu bool) {
	if ws.Samples == 0 {
		return false, false
	}
	f := starvation
	if !(f > 0) || math.IsInf(f, 0) {
		f = 1
	}
	mem = ws.MemMax > req.MemoryBytes
	cpu = float64(ws.CPUP95) > float64(req.MilliCPU)*f
	return mem, cpu
}
