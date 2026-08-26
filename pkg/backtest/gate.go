package backtest

import (
	"fmt"
	"math"
	"time"
)

// Tolerance parameterizes Gate. The defaults encode §4.6's dominance rule:
// a candidate policy must not regress safety at all, must not cost more
// overall, and must not churn measurably more. Efficiency gets a small band
// because the oracle gap is a mean over a finite sample and a fraction of a
// percentage point is noise, not a regression.
type Tolerance struct {
	// MaxRegretIncreasePct allows the candidate's total regret to exceed the
	// current policy's by this percentage of the current magnitude.
	// Default 0: regret may not increase.
	MaxRegretIncreasePct float64
	// MaxRegretIncreaseUSD is an absolute slack added to the same test, so a
	// near-zero current regret does not make the percentage band vanish.
	// Default 0.
	MaxRegretIncreaseUSD float64
	// MaxOracleGapIncreasePct is the allowed increase in OracleGapPct, in
	// percentage points. Default 2. It is checked only when the candidate
	// did not strictly improve regret — see Gate for why.
	MaxOracleGapIncreasePct float64
	// MaxFlipRateIncrease is the allowed increase in FlipRate. Default 0.05.
	MaxFlipRateIncrease float64
	// AllowSafetyRegression disables the safety gate. It exists so an
	// operator deliberately exploring the risk/cost curve can, and it is
	// named to be uncomfortable to type. Default false.
	AllowSafetyRegression bool
}

// DefaultTolerance returns the production gate.
func DefaultTolerance() Tolerance {
	return Tolerance{
		MaxRegretIncreasePct:    0,
		MaxRegretIncreaseUSD:    0,
		MaxOracleGapIncreasePct: 2,
		MaxFlipRateIncrease:     0.05,
	}
}

// Gate decides whether a candidate policy may replace the current one, and
// says why not when it may not. It is the only sanctioned path from "a
// policy scored well" to "a policy runs": the closed-loop tuner, a human
// proposal and an LLM proposal all pass through here (§4.6, INV-4).
//
// Rules, evaluated in a fixed order so `reasons` is deterministic:
//
//  1. The two scorecards must be comparable — same cluster, same window,
//     same horizon, cadence, starvation factor and cost model. Comparing
//     policies scored under different yardsticks is the easiest way to
//     manufacture a win, so it is rejected outright rather than tolerated.
//  2. The ground-truth counters (MemOOMKills) must agree. They record what
//     really happened and cannot depend on the policy; a disagreement means
//     the two runs did not see the same history.
//  3. No safety regression: neither MemViolations nor CPUStarvation may rise.
//  4. Regret within tolerance. Regret is the scalar that already prices the
//     efficiency/risk trade-off, so it is the primary economic test.
//  5. Oracle gap within tolerance — but only when regret did not strictly
//     improve. The gap is a diagnostic measuring the same dollars regret
//     already counted; gating on it unconditionally would double-count
//     efficiency and block exactly the trade an operator most wants to make:
//     buying a large safety win for a small, priced amount of waste. When
//     regret is flat or worse the gap is the tie-breaker, and it applies.
//  6. Stability within tolerance: FlipRate.
//
// A nil scorecard on either side fails closed.
func Gate(current, candidate *Scorecard, tol Tolerance) (bool, []string) {
	if current == nil || candidate == nil {
		return false, []string{"gate: a nil scorecard cannot be compared"}
	}
	var reasons []string

	if current.Cluster != candidate.Cluster {
		reasons = append(reasons, fmt.Sprintf("cluster mismatch: %q vs %q", current.Cluster, candidate.Cluster))
	}
	if !sameWindow(current.Window, candidate.Window) {
		reasons = append(reasons, fmt.Sprintf("window mismatch: %s vs %s",
			windowString(current.Window), windowString(candidate.Window)))
	}
	if current.HorizonHours != candidate.HorizonHours {
		reasons = append(reasons, fmt.Sprintf("horizon mismatch: %gh vs %gh", current.HorizonHours, candidate.HorizonHours))
	}
	if current.DecisionIntervalHours != candidate.DecisionIntervalHours {
		reasons = append(reasons, fmt.Sprintf("decision cadence mismatch: %gh vs %gh",
			current.DecisionIntervalHours, candidate.DecisionIntervalHours))
	}
	if current.StarvationFactor != candidate.StarvationFactor {
		reasons = append(reasons, fmt.Sprintf("starvation factor mismatch: %g vs %g",
			current.StarvationFactor, candidate.StarvationFactor))
	}
	if current.Cost != candidate.Cost {
		reasons = append(reasons, "cost model mismatch: regret is not comparable across price lists")
	}
	if len(reasons) > 0 {
		// Nothing below is meaningful once the yardsticks differ.
		return false, reasons
	}

	if current.MemOOMKills != candidate.MemOOMKills {
		reasons = append(reasons, fmt.Sprintf(
			"observed OOMKills differ (%d vs %d): the two runs did not replay the same history",
			current.MemOOMKills, candidate.MemOOMKills))
	}
	if !tol.AllowSafetyRegression {
		if candidate.MemViolations > current.MemViolations {
			reasons = append(reasons, fmt.Sprintf("memory violations regress: %d → %d",
				current.MemViolations, candidate.MemViolations))
		}
		if candidate.CPUStarvation > current.CPUStarvation {
			reasons = append(reasons, fmt.Sprintf("cpu starvation regresses: %d → %d",
				current.CPUStarvation, candidate.CPUStarvation))
		}
	}
	slack := nonNegative(tol.MaxRegretIncreaseUSD) +
		math.Abs(current.RegretUSD)*nonNegative(tol.MaxRegretIncreasePct)/100
	if allowed := current.RegretUSD + slack; candidate.RegretUSD > allowed {
		reasons = append(reasons, fmt.Sprintf("regret regresses: $%.4f → $%.4f (allowed $%.4f)",
			current.RegretUSD, candidate.RegretUSD, allowed))
	}
	if candidate.RegretUSD >= current.RegretUSD {
		if gapAllowed := current.OracleGapPct + nonNegative(tol.MaxOracleGapIncreasePct); candidate.OracleGapPct > gapAllowed {
			reasons = append(reasons, fmt.Sprintf("oracle gap regresses: %.3f%% → %.3f%% (allowed %.3f%%)",
				current.OracleGapPct, candidate.OracleGapPct, gapAllowed))
		}
	}
	if flipAllowed := current.FlipRate + nonNegative(tol.MaxFlipRateIncrease); candidate.FlipRate > flipAllowed {
		reasons = append(reasons, fmt.Sprintf("flip rate regresses: %.4f → %.4f (allowed %.4f)",
			current.FlipRate, candidate.FlipRate, flipAllowed))
	}
	return len(reasons) == 0, reasons
}

// nonNegative clamps a tolerance to zero, so a NaN or negative slack cannot
// widen the gate (NaN fails every positive-form comparison, and a negative
// tolerance would be a tightening the caller almost certainly did not mean).
func nonNegative(f float64) float64 {
	if !(f > 0) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func sameWindow(a, b [2]time.Time) bool {
	return a[0].Equal(b[0]) && a[1].Equal(b[1])
}

func windowString(w [2]time.Time) string {
	return "[" + w[0].UTC().Format(time.RFC3339) + "," + w[1].UTC().Format(time.RFC3339) + ")"
}
