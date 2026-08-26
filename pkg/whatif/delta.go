package whatif

import (
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
)

// hoursPerMonth is the projection factor used for monthly figures, matching
// pkg/explain's constant (730 = 365×24/12) so two parts of the product never
// quote different monthly numbers for the same hourly rate.
const hoursPerMonth = 730.0

// Delta is candidate-minus-baseline on every scorecard axis an approver reads.
// Negative is better for every cost and count field; positive is better only
// for Decisions and the savings fields, and each is named so the sign cannot
// be misread.
//
// Nothing here is a new measurement. Every field is arithmetic over two
// pkg/backtest scorecards, which is what keeps this package incapable of
// grading the recommender with anything of its own invention.
type Delta struct {
	// Safety. Must be <= 0 for a proposal to pass the gate.
	MemViolations int `json:"memViolations"`
	CPUStarvation int `json:"cpuStarvation"`

	// Economics.
	RegretUSD         float64 `json:"regretUSD"`
	RegretPct         float64 `json:"regretPct"`
	ResourceRegretUSD float64 `json:"resourceRegretUSD"`
	RiskRegretUSD     float64 `json:"riskRegretUSD"`
	PolicyCostUSD     float64 `json:"policyCostUSD"`
	ForgoneSavingsUSD float64 `json:"forgoneSavingsUSD"`

	// Efficiency and behaviour.
	OracleGapPct        float64 `json:"oracleGapPct"`
	OracleGapPctApplied float64 `json:"oracleGapPctApplied"`
	Decisions           int     `json:"decisions"`
	Refusals            int     `json:"refusals"`
	RefusalsIdle        int     `json:"refusalsIdle"`
	FlipRate            float64 `json:"flipRate"`
	Flips               int     `json:"flips"`

	// ProjectedMonthlyUSD extrapolates the regret improvement from the replay
	// window to a 730-hour month. It is a projection, labelled as one: it
	// assumes the next month resembles the window that was replayed, which is
	// exactly the assumption a backtest cannot verify. Negative means the
	// candidate is projected to cost less.
	ProjectedMonthlyUSD float64 `json:"projectedMonthlyUSD"`

	// WindowHours is the span the projection was scaled from, recorded so the
	// number above can be sanity-checked without the scorecards.
	WindowHours float64 `json:"windowHours"`
}

// Diff computes candidate-minus-baseline. Both arguments must be non-nil;
// a nil scorecard yields the zero Delta rather than a panic, because a Delta
// is descriptive and the gate — not this function — is what fails closed.
func Diff(base, cand *backtest.Scorecard) Delta {
	if base == nil || cand == nil {
		return Delta{}
	}
	d := Delta{
		MemViolations:       cand.MemViolations - base.MemViolations,
		CPUStarvation:       cand.CPUStarvation - base.CPUStarvation,
		RegretUSD:           round6(cand.RegretUSD - base.RegretUSD),
		RegretPct:           pctChange(base.RegretUSD, cand.RegretUSD),
		ResourceRegretUSD:   round6(cand.ResourceRegretUSD - base.ResourceRegretUSD),
		RiskRegretUSD:       round6(cand.RiskRegretUSD - base.RiskRegretUSD),
		PolicyCostUSD:       round6(cand.PolicyCostUSD - base.PolicyCostUSD),
		ForgoneSavingsUSD:   round6(cand.ForgoneSavingsUSD - base.ForgoneSavingsUSD),
		OracleGapPct:        round6(cand.OracleGapPct - base.OracleGapPct),
		OracleGapPctApplied: round6(cand.OracleGapPctApplied - base.OracleGapPctApplied),
		Decisions:           cand.Decisions - base.Decisions,
		Refusals:            countRefusals(cand) - countRefusals(base),
		RefusalsIdle:        cand.RefusalsIdle - base.RefusalsIdle,
		FlipRate:            round6(cand.FlipRate - base.FlipRate),
		Flips:               cand.Flips - base.Flips,
	}
	d.WindowHours = round6(windowHours(base.Window))
	d.ProjectedMonthlyUSD = round6(projectMonthly(
		[]float64{d.ResourceRegretUSD, d.RiskRegretUSD}, d.WindowHours))
	return d
}

// countRefusals totals the refusal map. The map is summed over sorted keys —
// integers are associative so the order cannot change the value, but the
// discipline is uniform and a future float-valued counter would inherit it
// for free rather than shipping the bug once more.
func countRefusals(sc *backtest.Scorecard) int {
	total := 0
	for _, code := range sortedRefusalCodes(sc) {
		total += sc.Refusals[code]
	}
	return total
}

// sortedRefusalCodes enumerates a scorecard's refusal codes in a fixed order,
// so no output of this package can depend on Go's map iteration order.
func sortedRefusalCodes(sc *backtest.Scorecard) []string {
	if sc == nil || len(sc.Refusals) == 0 {
		return nil
	}
	out := make([]string, 0, len(sc.Refusals))
	for code := range sc.Refusals {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// projectMonthly scales a set of dollar terms from a window to a month.
//
// The terms are summed with sumUSD rather than added inline: the caller passes
// a slice precisely so the total is a function of the multiset and not of the
// order the terms were listed in. Float addition is not associative, and
// "resource + risk" versus "risk + resource" can differ in the last bit once
// the magnitudes are far apart — which is how a proposal's bytes stop being
// reproducible.
func projectMonthly(termsUSD []float64, windowHours float64) float64 {
	total := sumUSD(termsUSD)
	if !(windowHours > 0) {
		return 0
	}
	return total * (hoursPerMonth / windowHours)
}

func windowHours(w [2]time.Time) float64 {
	if w[0].IsZero() || w[1].IsZero() || !w[1].After(w[0]) {
		return 0
	}
	return w[1].Sub(w[0]).Hours()
}
