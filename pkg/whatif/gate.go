package whatif

import (
	"fmt"
	"math"

	"github.com/agenticode/kilter/pkg/backtest"
)

// Tolerance parameterizes the proposal gate. It is deliberately NOT
// backtest.Tolerance:
//
//   - backtest.Tolerance answers "may this policy replace the incumbent?" — an
//     admissibility question an operator may legitimately explore, which is
//     why it carries an AllowSafetyRegression escape hatch for someone
//     plotting the risk/cost curve by hand.
//   - this answers "is this worth putting in front of an approver, unprompted?"
//     — and there is no version of that question where trading safety for
//     efficiency is an acceptable automatic answer. So the field does not
//     exist here, and Decide hardcodes AllowSafetyRegression: false when it
//     delegates. A hostile caller cannot construct a Tolerance that disarms
//     the safety rule, because no such Tolerance is representable.
//
// Zero fields take DefaultTolerance's values.
type Tolerance struct {
	// MinRegretImprovementUSD is the absolute margin by which the candidate's
	// regret must beat the incumbent's. Default $0.01: below a cent over a
	// whole replay window, "better" is float noise.
	MinRegretImprovementUSD float64 `json:"minRegretImprovementUSD"`
	// MinRegretImprovementPct is the same margin as a percentage of the
	// incumbent's regret magnitude. The required improvement is the LARGER of
	// the two, so a big-regret cluster is not tuned on rounding error and a
	// small-regret cluster is not tuned on a cent. Default 1%.
	MinRegretImprovementPct float64 `json:"minRegretImprovementPct"`
	// MaxFlipRateIncrease is the allowed rise in FlipRate. Default 0 —
	// §4.6's "flip-rate not worse", which is stricter than the 0.05
	// admissibility band pkg/backtest allows a hand-driven comparison.
	MaxFlipRateIncrease float64 `json:"maxFlipRateIncrease"`
	// MaxOracleGapIncreasePct is the allowed rise in OracleGapPct, in
	// percentage points. Default 0. It is a tie-breaker that pkg/backtest
	// consults only when regret did not strictly improve — and this gate
	// requires a strict regret improvement — so it is belt to that braces.
	MaxOracleGapIncreasePct float64 `json:"maxOracleGapIncreasePct"`
}

// DefaultTolerance returns the production proposal gate.
func DefaultTolerance() Tolerance {
	return Tolerance{
		MinRegretImprovementUSD: 0.01,
		MinRegretImprovementPct: 1,
		MaxFlipRateIncrease:     0,
		MaxOracleGapIncreasePct: 0,
	}
}

func (t Tolerance) withDefaults() Tolerance {
	d := DefaultTolerance()
	if t.MinRegretImprovementUSD == 0 {
		t.MinRegretImprovementUSD = d.MinRegretImprovementUSD
	}
	if t.MinRegretImprovementPct == 0 {
		t.MinRegretImprovementPct = d.MinRegretImprovementPct
	}
	return t
}

// backtestTolerance projects onto pkg/backtest's admissibility gate. Every
// slack is pinned at or below what this package allows, and the safety escape
// hatch is nailed shut.
func (t Tolerance) backtestTolerance() backtest.Tolerance {
	return backtest.Tolerance{
		// Regret may not increase at all; the strict-improvement margin is
		// checked separately, after delegation, so its reason names the
		// margin the operator configured rather than a slack of zero.
		MaxRegretIncreasePct:    0,
		MaxRegretIncreaseUSD:    0,
		MaxOracleGapIncreasePct: nonNegative(t.MaxOracleGapIncreasePct),
		MaxFlipRateIncrease:     nonNegative(t.MaxFlipRateIncrease),
		AllowSafetyRegression:   false,
	}
}

// nonNegative clamps a tolerance to zero, so a NaN, infinite or negative
// slack cannot widen the gate. NaN fails every positive-form comparison, and
// a negative tolerance would be a tightening the caller almost certainly did
// not mean.
func nonNegative(f float64) float64 {
	if !(f > 0) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// GateInput is everything the dominance rules read. Passing a struct (rather
// than six positional arguments) keeps the call sites legible and makes it a
// compile-time-visible decision when a rule starts reading something new.
type GateInput struct {
	// Baseline and Candidate are the two policies. Their hashes are checked
	// against the scorecards, so a caller cannot submit a flattering
	// scorecard for one policy while proposing another.
	Baseline  Policy `json:"baseline"`
	Candidate Policy `json:"candidate"`
	// BaselineScore and CandidateScore come from pkg/backtest. Nothing in
	// this package computes a quality number, so these are the only evidence
	// the gate has and the only evidence it needs.
	BaselineScore  *backtest.Scorecard `json:"-"`
	CandidateScore *backtest.Scorecard `json:"-"`
	// Envelope bounds what the candidate is allowed to be at all.
	Envelope  Envelope  `json:"envelope"`
	Tolerance Tolerance `json:"tolerance"`
}

// Rule names the dominance rules, so a reason string is greppable and a test
// can assert which rule fired rather than matching prose.
type Rule string

const (
	// RuleWellFormed: both scorecards present, envelope valid.
	RuleWellFormed Rule = "well-formed"
	// RuleEnvelope: the candidate policy lies inside the declared envelope.
	RuleEnvelope Rule = "envelope"
	// RuleIdentity: each scorecard is the scorecard OF the policy it is
	// offered for, and the candidate is not the incumbent restated.
	RuleIdentity Rule = "identity"
	// RuleAdmissible: pkg/backtest's gate — comparable yardsticks, agreeing
	// ground truth, no safety regression, no regret regression, no flip-rate
	// regression. Safety is inside this rule and cannot be disabled here.
	RuleAdmissible Rule = "admissible"
	// RuleCoverage: the two runs measured the same set of things.
	RuleCoverage Rule = "coverage"
	// RuleNotRefusalCollapse: the candidate did not simply stop deciding.
	RuleNotRefusalCollapse Rule = "not-refusal-collapse"
	// RuleStrictImprovement: regret improved by more than the noise margin.
	RuleStrictImprovement Rule = "strict-improvement"
)

// GateResult is the gate's verdict, in a form that encodes deterministically
// and reads as an explanation. It is attached to the proposal the approver
// sees (§4.4), so it must survive a JSON round trip unchanged.
type GateResult struct {
	// Passed is true only when every rule held.
	Passed bool `json:"passed"`
	// Failed lists the rules that did not hold, in evaluation order.
	Failed []Rule `json:"failed,omitempty"`
	// Reasons explains each failure, in the same order.
	Reasons []string `json:"reasons,omitempty"`
	// Wins summarizes what actually got better, in a fixed order. Present
	// even on a failing result: "safer but not cheaper" is information an
	// approver wants, and hiding it would make every rejection look alike.
	Wins []string `json:"wins,omitempty"`
	// RequiredRegretImprovementUSD is the margin that was applied, recorded
	// so a verdict can be re-read years later without re-deriving it from a
	// config file that has since changed.
	RequiredRegretImprovementUSD float64 `json:"requiredRegretImprovementUSD"`
	// BaselinePolicy and CandidatePolicy are the two policy hashes.
	BaselinePolicy  string `json:"baselinePolicy"`
	CandidatePolicy string `json:"candidatePolicy"`
	// Tolerance is the gate the verdict was reached under.
	Tolerance Tolerance `json:"tolerance"`
}

// fail records a rule failure. Rules are evaluated in a fixed order and each
// appends here, so Failed and Reasons are index-aligned and deterministic.
func (g *GateResult) fail(r Rule, format string, args ...any) {
	g.Failed = append(g.Failed, r)
	g.Reasons = append(g.Reasons, fmt.Sprintf(string(r)+": "+format, args...))
}

// Decide applies §4.6's dominance rules and returns the verdict.
//
// The rules, in evaluation order. Each is a hard gate: there is no scoring, no
// weighting, and no way for a large win on one axis to buy a regression on
// another. That asymmetry is the point — a policy change is a one-way door
// onto a production cluster, and the loss function is not symmetric.
//
//  1. RuleWellFormed — both scorecards present; the envelope validates. Fails
//     closed: a nil scorecard is not "no evidence against", it is no evidence.
//  2. RuleEnvelope — the candidate lies inside the declared search space. This
//     is checked at acceptance and not only at generation, because the
//     producer of a candidate (tuner, human, LLM agent) is untrusted; a
//     proposal that arrived by any route at all must still be in-envelope.
//  3. RuleIdentity — CandidateScore.Policy == Candidate.Hash() and likewise
//     for the baseline, and the two policies differ. Without this a hostile
//     caller submits policy A's excellent scorecard while proposing policy B,
//     and every rule below is evaluating the wrong thing.
//  4. RuleAdmissible — delegated to backtest.Gate with AllowSafetyRegression
//     pinned false. That covers comparability of the yardsticks (same cluster,
//     window, horizon, cadence, starvation factor, cost model), agreement of
//     the ground-truth OOMKill counter, no rise in MemViolations or
//     CPUStarvation, no rise in regret, and no rise in flip rate. Delegating
//     rather than reimplementing is what keeps this gate measuring the same
//     thing `kilter backtest` reports.
//  5. RuleCoverage — the two runs scored the same number of (container,
//     instant) pairs over the same number of instants. pkg/backtest fixes the
//     scored set before any policy runs, so an inequality here means the runs
//     did not see the same history and the comparison is void.
//  6. RuleNotRefusalCollapse — a candidate that decides nothing at all, where
//     the incumbent decided something, is rejected outright. Refusing
//     everything already scores badly (pkg/backtest charges a refusal for the
//     sizing it leaves in force), so this rule is not what stops the obvious
//     case; it stops the subtle one, where the incumbent's resizes were
//     genuinely harmful over this particular window and "do nothing" therefore
//     wins on regret. That is a real finding and a human should read it — it
//     is not a policy tweak to auto-propose at 3am.
//  7. RuleStrictImprovement — regret must improve by max(MinRegretImprovement
//     USD, |baseline regret| × MinRegretImprovementPct). backtest.Gate only
//     requires that regret not get worse; a proposal must be strictly, and
//     measurably, better. Ties are not proposals.
//
// Rules 5–7 are skipped when 4 failed: backtest.Gate stops at the first
// incomparability, so once the yardsticks disagree every metric below it is
// comparing numbers that were never on the same scale.
func Decide(in GateInput) GateResult {
	tol := in.Tolerance.withDefaults()
	res := GateResult{Tolerance: tol}

	base, cand := in.BaselineScore, in.CandidateScore
	if base == nil || cand == nil {
		res.fail(RuleWellFormed, "a nil scorecard is not evidence (baseline=%v candidate=%v)",
			base != nil, cand != nil)
		return res
	}
	basePolicy := in.Baseline.withDefaults()
	candPolicy := in.Candidate.withDefaults()
	res.BaselinePolicy = basePolicy.Hash()
	res.CandidatePolicy = candPolicy.Hash()

	if err := in.Envelope.Validate(); err != nil {
		res.fail(RuleWellFormed, "%v", err)
		return res
	}
	if err := candPolicy.Validate(); err != nil {
		res.fail(RuleWellFormed, "%v", err)
		return res
	}

	for _, v := range in.Envelope.Violations(candPolicy) {
		res.fail(RuleEnvelope, "candidate %s", v)
	}

	if base.Policy != res.BaselinePolicy {
		res.fail(RuleIdentity, "baseline scorecard is for policy %s, not %s",
			base.Policy, res.BaselinePolicy)
	}
	if cand.Policy != res.CandidatePolicy {
		res.fail(RuleIdentity, "candidate scorecard is for policy %s, not %s",
			cand.Policy, res.CandidatePolicy)
	}
	if res.BaselinePolicy == res.CandidatePolicy {
		res.fail(RuleIdentity, "candidate is the incumbent restated (%s)", res.CandidatePolicy)
	}

	ok, reasons := backtest.Gate(base, cand, tol.backtestTolerance())
	if !ok {
		for _, r := range reasons {
			res.fail(RuleAdmissible, "%s", r)
		}
		// Nothing below is meaningful once backtest.Gate has rejected: it
		// stops at the first incomparability, so the remaining metrics may
		// not be on the same scale at all.
		res.Wins = winsOf(base, cand)
		res.RequiredRegretImprovementUSD = round6(requiredImprovement(base.RegretUSD, tol))
		return res
	}

	if base.Scored != cand.Scored {
		res.fail(RuleCoverage, "scored pairs differ (%d vs %d): the runs did not see the same history",
			base.Scored, cand.Scored)
	}
	if base.Instants != cand.Instants {
		res.fail(RuleCoverage, "decision instants differ (%d vs %d)", base.Instants, cand.Instants)
	}

	if cand.Decisions == 0 && base.Decisions > 0 {
		res.fail(RuleNotRefusalCollapse,
			"candidate stops deciding entirely (%d → 0 resizes over %d scored pairs); "+
				"a policy that improves by doing nothing is a finding for a human, not an auto-proposal",
			base.Decisions, cand.Scored)
	}

	need := requiredImprovement(base.RegretUSD, tol)
	res.RequiredRegretImprovementUSD = round6(need)
	if got := base.RegretUSD - cand.RegretUSD; !(got >= need) {
		res.fail(RuleStrictImprovement,
			"regret improves by $%.4f, short of the $%.4f margin ($%.4f → $%.4f)",
			got, need, base.RegretUSD, cand.RegretUSD)
	}

	res.Wins = winsOf(base, cand)
	res.Passed = len(res.Failed) == 0
	return res
}

// requiredImprovement is the larger of the absolute and relative margins, so
// neither a large-regret cluster (where a cent is noise) nor a small-regret
// one (where 1% is a fraction of a cent) can be tuned on nothing.
func requiredImprovement(baselineRegret float64, tol Tolerance) float64 {
	abs := nonNegative(tol.MinRegretImprovementUSD)
	rel := math.Abs(baselineRegret) * nonNegative(tol.MinRegretImprovementPct) / 100
	if math.IsNaN(rel) || math.IsInf(rel, 0) {
		rel = 0
	}
	return math.Max(abs, rel)
}

// winsOf summarizes what got better, in a fixed order. It never claims a win
// for a metric that got worse — a rejected proposal's Wins list is what the
// approver reads to understand what the candidate was actually trying to buy.
func winsOf(base, cand *backtest.Scorecard) []string {
	var out []string
	if cand.MemViolations < base.MemViolations {
		out = append(out, fmt.Sprintf("memory violations %d → %d", base.MemViolations, cand.MemViolations))
	}
	if cand.CPUStarvation < base.CPUStarvation {
		out = append(out, fmt.Sprintf("cpu starvation %d → %d", base.CPUStarvation, cand.CPUStarvation))
	}
	if cand.RegretUSD < base.RegretUSD {
		out = append(out, fmt.Sprintf("regret $%.4f → $%.4f", base.RegretUSD, cand.RegretUSD))
	}
	if cand.OracleGapPct < base.OracleGapPct {
		out = append(out, fmt.Sprintf("oracle gap %.3f%% → %.3f%%", base.OracleGapPct, cand.OracleGapPct))
	}
	if cand.FlipRate < base.FlipRate {
		out = append(out, fmt.Sprintf("flip rate %.4f → %.4f", base.FlipRate, cand.FlipRate))
	}
	if cand.ForgoneSavingsUSD < base.ForgoneSavingsUSD {
		out = append(out, fmt.Sprintf("forgone savings $%.4f → $%.4f",
			base.ForgoneSavingsUSD, cand.ForgoneSavingsUSD))
	}
	return out
}
