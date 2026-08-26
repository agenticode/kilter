package backtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

// Backtest-local refusal codes. The enumerated codes in pkg/decision cover
// the reasons the *decision layer* refuses; these four cover the places where
// the shipped production path declines to act without going through a
// decision.Refusal, so that every scored non-action carries a reason instead
// of vanishing. They are deliberately distinct strings from any
// decision.RefusalCode value, so a Refusals map never conflates the two.
const (
	// CodeBelowChangeThreshold: the recommender produced a target within
	// recommend.Config.MinChangeRatio of the current request and suppressed
	// it as churn (recommendOne's nil return).
	CodeBelowChangeThreshold = "below-change-threshold"
	// CodeBelowConfidence: a recommendation existed but its confidence was
	// under plan.Config.MinConfidence, so the planner emitted no step.
	CodeBelowConfidence = "below-confidence"
	// CodeModeGuarded: the workload's effective kilter.dev/mode is not
	// "apply", so the planner emitted no step.
	CodeModeGuarded = "mode-guarded"
	// CodePlanDropped: a recommendation cleared confidence and mode but the
	// planner still emitted no resize step (stale target, or
	// ApplyRecommendations disabled).
	CodePlanDropped = "plan-dropped"
)

// SkipCounts records every (container, instant) pair the harness declined to
// score, and why. A backtest that quietly drops half its history reads as a
// clean scorecard; these counters make the coverage auditable.
type SkipCounts struct {
	// NoHorizon counts decision instants dropped because [t, t+horizon] did
	// not fit inside the replay window. Scoring a truncated future would
	// bias every metric toward whatever the surviving prefix happened to do.
	NoHorizon int `json:"noHorizon"`
	// NoSnapshot counts grid points with no snapshot at or after them.
	NoSnapshot int `json:"noSnapshot"`
	// NoFutureSamples counts scored containers with no usage sample in the
	// scoring window: no ground truth, therefore no oracle and no verdict.
	NoFutureSamples int `json:"noFutureSamples"`
	// EventQueryErrors counts evidence lookups that failed. They degrade a
	// record to "no adverse events known" rather than failing the run, and
	// are surfaced here so a silently broken store is visible.
	EventQueryErrors int `json:"eventQueryErrors"`
}

// Scorecard is the falsifiability instrument: what a policy would have done
// over a stretch of real history, and how far that was from the best any
// policy could have done. Every field is a number computed from the user's
// own cluster history — nothing here is an estimate or a claim.
//
// The struct is JSON-stable by construction: slices are sorted, the one map
// is encoded by encoding/json in sorted key order, and every float is the sum
// of a canonically-ordered multiset (see sumSorted). Same history plus same
// policy therefore yields byte-identical output.
type Scorecard struct {
	// Policy is a content hash of the policy under test — the (recommend,
	// plan, decision) config triple — in the spirit of plan fingerprints.
	// It does not cover the scoring knobs; those are reported separately
	// below so a comparison cannot silently change the yardstick.
	Policy  string `json:"policy"`
	Cluster string `json:"cluster"`
	// Window is the half-open replay window [from, to).
	Window                [2]time.Time `json:"window"`
	HorizonHours          float64      `json:"horizonHours"`
	DecisionIntervalHours float64      `json:"decisionIntervalHours"`
	StarvationFactor      float64      `json:"starvationFactor"`

	// Coverage.
	Snapshots int `json:"snapshots"`
	Instants  int `json:"instants"`
	// Scored is the number of (container, instant) pairs with ground truth.
	Scored int `json:"scored"`
	// Decisions is the number of scored pairs where the production path
	// actually planned a resize.
	Decisions int `json:"decisions"`
	// Refusals counts every scored pair that did NOT result in a resize,
	// keyed by reason. Sum(Refusals) + Decisions == Scored.
	Refusals map[string]int `json:"refusals"`

	// Safety — would the outcome have hurt?
	MemViolations int `json:"memViolations"`
	CPUStarvation int `json:"cpuStarvation"`
	// MemOOMKills counts OOMKill events actually recorded in the substrate
	// inside scoring windows. Unlike MemViolations it is not a counterfactual
	// — it is what really happened under the sizing that was really in force.
	MemOOMKills int `json:"memOOMKills"`

	// Efficiency — did it save?
	//
	// OracleGapPct is the mean of (cost(outcome) − cost(oracle)) / cost(oracle)
	// over every scored pair, refusals included: a refusal leaves the current
	// sizing in force, and leaving money on the table is a cost like any
	// other. OracleGapPctApplied is the same mean over planned resizes only,
	// which is the number to read when asking "how good are the sizes it
	// picks", as opposed to "how good is the policy as a whole".
	OracleGapPct        float64 `json:"oracleGapPct"`
	OracleGapPctApplied float64 `json:"oracleGapPctApplied"`
	PolicyCostUSD       float64 `json:"policyCostUSD"`
	OracleCostUSD       float64 `json:"oracleCostUSD"`
	// ClaimedSavingsUSD is what the engine promised at decision time
	// (current − target, over the horizon, summed over planned resizes).
	// RealizedSavingsUSD is the part hindsight says it could have kept:
	// savings measured against max(target, oracle) per dimension, i.e. with
	// any under-sizing given back. ClaimedVsRealized is their ratio, the
	// backtest analogue of the ledger's claimed-vs-measured join.
	ClaimedSavingsUSD  float64 `json:"claimedSavingsUSD"`
	RealizedSavingsUSD float64 `json:"realizedSavingsUSD"`
	ClaimedVsRealized  float64 `json:"claimedVsRealized"`
	// ForgoneSavingsUSD is what the idle refusals cost: for each refusal over
	// a window with nothing adverse in it, the money that leaving the current
	// sizing alone kept spending above the oracle.
	ForgoneSavingsUSD float64 `json:"forgoneSavingsUSD"`

	// Refusal quality. A refusal whose window carried a violation or an
	// adverse event is a good refusal — caution that coincided with a
	// genuinely turbulent window. A refusal over a boring window is idle, and
	// its price is in ForgoneSavingsUSD.
	RefusalsGood int `json:"refusalsGood"`
	RefusalsIdle int `json:"refusalsIdle"`

	// Stability. FlipRate is the fraction of planned resizes that reverse the
	// direction of the previous resize for the same container within
	// Config.FlipWindow.
	FlipRate float64 `json:"flipRate"`
	Flips    int     `json:"flips"`

	// Overall regret in dollars.
	//   ResourceRegretUSD = Σ (cost(outcome) − cost(oracle)) × horizon
	//   RiskRegretUSD     = Σ violations × CostModel.IncidentUSD
	//   RegretUSD         = ResourceRegretUSD + RiskRegretUSD
	// Lower is better; zero means "as good as hindsight allows, with no
	// violations". Refusals are in the sum on exactly the same terms as
	// recommendations, which is what stops a refuse-everything policy from
	// scoring well: it banks no savings and still pays for whatever the
	// unchanged sizing did.
	ResourceRegretUSD float64 `json:"resourceRegretUSD"`
	RiskRegretUSD     float64 `json:"riskRegretUSD"`
	RegretUSD         float64 `json:"regretUSD"`

	Skipped SkipCounts `json:"skipped"`
	Cost    CostModel  `json:"cost"`
}

// Encode renders the scorecard as indented JSON with a trailing newline —
// the golden-file and CI-artifact form. Byte-identical for identical inputs.
func (s *Scorecard) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// PolicyHash fingerprints a policy triple. Fields are written explicitly in a
// fixed order rather than reflected over, so adding a field to any upstream
// config is a compile-time-visible decision about whether it changes policy
// identity — the same discipline plan.fingerprint uses.
func PolicyHash(rec recommend.Config, pl plan.Config, dec decision.Config) string {
	h := sha256.New()
	fmt.Fprintf(h, "rec|%v|%v|%v|%v|%v|%d|%d|%d|%v|%v|%v\n",
		rec.CPUPercentile, rec.CPUHeadroom, rec.MemoryPercentile, rec.MemoryHeadroom,
		rec.OOMBumpRatio, rec.MinMilliCPU, rec.MinMemoryBytes, rec.MinSamples,
		rec.MinWindow, rec.MinChangeRatio, rec.SkipCPUForHPA)
	fmt.Fprintf(h, "plan|%v|%v|%d|%v|%v|%v|%s\n",
		pl.MinNodeUtilization, pl.MinConfidence, pl.MaxNodeRemovals,
		pl.ApplyRecommendations, pl.MinClusterHeadroom, pl.RespectManagedNodes, pl.DefaultMode)
	fmt.Fprintf(h, "dec|%d|%v|%v|%v|%v|%v|%v|%v\n",
		dec.MinSamples, dec.MinWindow, dec.BaseSoak, dec.ClassFlipWindow,
		dec.MinClassStability, dec.MaxHPAThrashPerHour, dec.MaxForecastDivergence, dec.ActConfidence)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// direction is the sign of a recommended change on one dimension.
type direction int8

const (
	dirNone   direction = 0
	dirShrink direction = -1
	dirGrow   direction = 1
)

func dirOf(from, to int64) direction {
	switch {
	case to < from:
		return dirShrink
	case to > from:
		return dirGrow
	}
	return dirNone
}

// record is one scored (container, decision instant) pair. It carries
// everything the aggregation needs, so scoring is a pure function of a
// []record and never re-reads history — which is what lets the determinism
// test shuffle records and demand identical output.
type record struct {
	Key model.ContainerKey
	At  time.Time

	// Applied is true when the production path planned a resize. Code is the
	// refusal reason otherwise (always non-empty when !Applied).
	Applied bool
	Code    string

	Current model.Resources // request in force at the decision instant
	Target  model.Resources // what the engine recommended (== Current when none)
	Chosen  model.Resources // request in force over the scoring window
	Oracle  model.Resources // cheapest safe request, in hindsight

	MemViolation bool
	CPUStarved   bool
	OOMKills     int
	Adverse      bool
	Samples      int
}

// lessRecord is the total order records are aggregated in: by instant, then
// by container key. Both components are already policy-independent, so two
// runs over the same history enumerate the same sequence.
func lessRecord(a, b record) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.Key.String() < b.Key.String()
}

// score aggregates records into a Scorecard. It is deliberately separated
// from the replay so it can be tested on shuffled inputs.
func score(recs []record, cost CostModel, horizon, flipWindow time.Duration) *Scorecard {
	sorted := append(make([]record, 0, len(recs)), recs...)
	sort.SliceStable(sorted, func(i, j int) bool { return lessRecord(sorted[i], sorted[j]) })

	hours := horizon.Hours()
	sc := &Scorecard{Refusals: map[string]int{}, Cost: cost}

	var (
		policyCosts []float64
		oracleCosts []float64
		gapsAll     []float64
		gapsApplied []float64
		claimed     []float64
		realized    []float64
		forgone     []float64
		riskTerms   []float64
	)

	// churn tracks, per container, the previous planned target and the
	// direction of the last *material* move. A flip is a move that reverses
	// the previous move within FlipWindow — "shrink to 229m, then grow back
	// to 825m two days later" is the churn an operator feels, and it is
	// measured between successive targets rather than against the current
	// request, because a backtest never applies its own advice: the current
	// request never moves, so every recommendation would otherwise look like
	// the same one-way change forever.
	type churnState struct {
		prevTarget     model.Resources
		havePrev       bool
		cpuDir, memDir direction
		cpuAt, memAt   time.Time
	}
	churn := map[model.ContainerKey]*churnState{}

	for i := range sorted {
		r := &sorted[i]
		sc.Scored++

		pc := cost.HourlyUSD(r.Chosen) * hours
		oc := cost.HourlyUSD(r.Oracle) * hours
		policyCosts = append(policyCosts, pc)
		oracleCosts = append(oracleCosts, oc)

		// The gap is a ratio, so it needs a non-zero denominator. A container
		// whose whole window measured zero CPU and zero memory has a
		// zero-cost oracle; it contributes to the cost sums but not to the
		// mean gap, because "infinitely worse than free" is not a number an
		// operator can act on.
		if oc > 0 {
			gap := (pc - oc) / oc
			gapsAll = append(gapsAll, gap)
			if r.Applied {
				gapsApplied = append(gapsApplied, gap)
			}
		}

		nViol := 0
		if r.MemViolation {
			sc.MemViolations++
			nViol++
		}
		if r.CPUStarved {
			sc.CPUStarvation++
			nViol++
		}
		sc.MemOOMKills += r.OOMKills
		if nViol > 0 {
			riskTerms = append(riskTerms, float64(nViol)*cost.IncidentUSD)
		}

		if r.Applied {
			sc.Decisions++
			// Claimed: what the engine said it would save. Realized: what
			// hindsight says it could have kept, i.e. the same saving
			// measured against a target raised to the oracle wherever it
			// dipped below. Both are clamped at zero: a resize that grows a
			// container claims no savings, it buys safety.
			cl := (cost.HourlyUSD(r.Current) - cost.HourlyUSD(r.Target)) * hours
			safe := r.Target.Max(r.Oracle)
			rl := (cost.HourlyUSD(r.Current) - cost.HourlyUSD(safe)) * hours
			if cl > 0 {
				claimed = append(claimed, cl)
				if rl < 0 {
					rl = 0
				}
				realized = append(realized, rl)
			}
			cs := churn[r.Key]
			if cs == nil {
				cs = &churnState{}
				churn[r.Key] = cs
			}
			if cs.havePrev {
				flipped := false
				if d := dirOf(cs.prevTarget.MilliCPU, r.Target.MilliCPU); d != dirNone {
					if reversed(cs.cpuDir, d) && r.At.Sub(cs.cpuAt) <= flipWindow {
						flipped = true
					}
					cs.cpuDir, cs.cpuAt = d, r.At
				}
				if d := dirOf(cs.prevTarget.MemoryBytes, r.Target.MemoryBytes); d != dirNone {
					if reversed(cs.memDir, d) && r.At.Sub(cs.memAt) <= flipWindow {
						flipped = true
					}
					cs.memDir, cs.memAt = d, r.At
				}
				if flipped {
					sc.Flips++
				}
			}
			cs.prevTarget, cs.havePrev = r.Target, true
		} else {
			sc.Refusals[r.Code]++
			if r.Adverse || r.MemViolation || r.CPUStarved || r.OOMKills > 0 {
				sc.RefusalsGood++
			} else {
				sc.RefusalsIdle++
				if d := pc - oc; d > 0 {
					forgone = append(forgone, d)
				}
			}
		}
	}

	sc.PolicyCostUSD = round6(sumSorted(policyCosts))
	sc.OracleCostUSD = round6(sumSorted(oracleCosts))
	sc.ResourceRegretUSD = round6(sumSorted(policyCosts) - sumSorted(oracleCosts))
	sc.RiskRegretUSD = round6(sumSorted(riskTerms))
	sc.RegretUSD = round6(sc.ResourceRegretUSD + sc.RiskRegretUSD)
	sc.OracleGapPct = round6(meanSorted(gapsAll) * 100)
	sc.OracleGapPctApplied = round6(meanSorted(gapsApplied) * 100)
	sc.ClaimedSavingsUSD = round6(sumSorted(claimed))
	sc.RealizedSavingsUSD = round6(sumSorted(realized))
	if c := sumSorted(claimed); c > 0 {
		sc.ClaimedVsRealized = round6(sumSorted(realized) / c)
	}
	sc.ForgoneSavingsUSD = round6(sumSorted(forgone))
	if sc.Decisions > 0 {
		sc.FlipRate = round6(float64(sc.Flips) / float64(sc.Decisions))
	}
	return sc
}

// reversed reports whether two successive recommended directions on the same
// dimension oppose each other. A "no change" leg is not a reversal — only a
// grow following a shrink, or the converse, is the churn this measures.
func reversed(prev, next direction) bool {
	return (prev == dirShrink && next == dirGrow) || (prev == dirGrow && next == dirShrink)
}
