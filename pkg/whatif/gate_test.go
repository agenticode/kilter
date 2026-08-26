package whatif

import (
	"math"
	"reflect"
	"testing"

	"github.com/agenticode/kilter/pkg/backtest"
)

func TestGateAcceptsAStrictlyBetterCandidate(t *testing.T) {
	res := Decide(gateInput(nil, better))
	mustPass(t, res)
	if res.BaselinePolicy == res.CandidatePolicy {
		t.Fatal("the two policies must hash differently for this fixture to mean anything")
	}
	if len(res.Wins) == 0 {
		t.Fatal("a passing proposal must be able to say what got better")
	}
}

func TestGateFailsClosedOnNilScorecards(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, cand *backtest.Scorecard
	}{
		{"both nil", nil, nil},
		{"no baseline", nil, scoreFor(candidatePolicy(), better)},
		{"no candidate", scoreFor(baselinePolicy(), nil), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gateInput(nil, better)
			in.BaselineScore, in.CandidateScore = tc.base, tc.cand
			mustFail(t, Decide(in), RuleWellFormed)
		})
	}
}

// TestTiesAreNotProposals is the difference between pkg/backtest's
// admissibility gate (may this replace the incumbent?) and this one (is this
// worth waking an approver for?).
func TestTiesAreNotProposals(t *testing.T) {
	res := Decide(gateInput(nil, nil)) // identical scorecards
	mustFail(t, res, RuleStrictImprovement)

	// And the admissibility gate would have allowed it, which is exactly why
	// this package needs a rule of its own.
	in := gateInput(nil, nil)
	if ok, why := backtest.Gate(in.BaselineScore, in.CandidateScore,
		DefaultTolerance().backtestTolerance()); !ok {
		t.Fatalf("fixture is wrong: backtest.Gate should admit a tie, got %v", why)
	}
}

func TestGateRejectsAnImprovementBelowTheNoiseMargin(t *testing.T) {
	// $0.001 better on a $30 baseline: below both the 1% relative margin and
	// the one-cent absolute floor.
	res := Decide(gateInput(nil, func(sc *backtest.Scorecard) {
		sc.RegretUSD = 29.999
		sc.ResourceRegretUSD = 19.999
	}))
	mustFail(t, res, RuleStrictImprovement)
	if res.RequiredRegretImprovementUSD != 0.3 {
		t.Fatalf("required margin should be 1%% of $30 = $0.30, got %v",
			res.RequiredRegretImprovementUSD)
	}
}

// ---- the two rules the design cares most about ----

// TestRefuseEverythingCannotWin covers the case pkg/backtest's economics do
// not: a window where the incumbent's resizes really were harmful, so doing
// nothing scores better on every metric the admissibility gate reads.
func TestRefuseEverythingCannotWin(t *testing.T) {
	in := gateInput(
		func(sc *backtest.Scorecard) {
			// An incumbent that resized a lot and hurt itself doing it.
			sc.Decisions = 60
			sc.Refusals = map[string]int{}
			sc.MemViolations = 20
			sc.RegretUSD = 500
			sc.ResourceRegretUSD = 100
			sc.RiskRegretUSD = 400
			sc.FlipRate = 0.4
			sc.Flips = 24
		},
		func(sc *backtest.Scorecard) {
			// A policy that decides nothing at all: safer, cheaper on this
			// window, stabler. Every admissibility rule is satisfied.
			sc.Decisions = 0
			sc.Refusals = map[string]int{backtest.CodeBelowConfidence: 60}
			sc.MemViolations = 0
			sc.CPUStarvation = 0
			sc.RegretUSD = 60
			sc.ResourceRegretUSD = 60
			sc.RiskRegretUSD = 0
			sc.OracleGapPct = 75
			sc.FlipRate = 0
			sc.Flips = 0
		})

	// Establish that the weaker gate really would have let it through, so the
	// test is proving this package adds something rather than restating
	// pkg/backtest.
	if ok, why := backtest.Gate(in.BaselineScore, in.CandidateScore,
		DefaultTolerance().backtestTolerance()); !ok {
		t.Fatalf("fixture is wrong: admissibility should pass here, got %v", why)
	}

	res := Decide(in)
	mustFail(t, res, RuleNotRefusalCollapse)
	if !reasonMentions(res, "stops deciding entirely") {
		t.Fatalf("the reason must name the collapse: %v", res.Reasons)
	}
}

// FuzzRefuseEverythingCannotWin is the property behind the case above: no
// scorecard shape, and no tolerance, lets a decide-nothing candidate through
// when the incumbent decided something.
func FuzzRefuseEverythingCannotWin(f *testing.F) {
	f.Add(10, 30.0, 5.0, 2, 1, 0.5, 100.0)
	f.Add(1, 0.0, -1.0, 0, 0, 0.0, 0.0)
	f.Add(1000, 1e9, 0.0, 99, 99, 1.0, 1e9)

	f.Fuzz(func(t *testing.T, baseDecisions int, baseRegret, candRegret float64,
		baseMemV, baseCPUV int, flip, tolUSD float64) {
		if baseDecisions <= 0 || baseDecisions > 1_000_000 {
			t.Skip()
		}
		for _, v := range []float64{baseRegret, candRegret, flip, tolUSD} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Skip()
			}
		}
		if baseMemV < 0 || baseCPUV < 0 {
			t.Skip()
		}
		in := gateInput(
			func(sc *backtest.Scorecard) {
				sc.Decisions = baseDecisions
				sc.Scored = baseDecisions
				sc.RegretUSD = baseRegret
				sc.MemViolations = baseMemV
				sc.CPUStarvation = baseCPUV
				sc.FlipRate = flip
			},
			func(sc *backtest.Scorecard) {
				sc.Decisions = 0
				sc.Scored = baseDecisions
				sc.RegretUSD = candRegret
				sc.MemViolations = 0
				sc.CPUStarvation = 0
				sc.FlipRate = 0
			})
		in.Tolerance = Tolerance{
			MinRegretImprovementUSD: tolUSD,
			MinRegretImprovementPct: tolUSD,
			MaxFlipRateIncrease:     flip,
			MaxOracleGapIncreasePct: flip,
		}
		if res := Decide(in); res.Passed {
			t.Fatalf("a refuse-everything candidate passed the gate: %+v", res)
		}
	})
}

// TestSafetyIsNotForSaleAtAnyPrice is the second rule the brief names: a
// candidate that is dramatically cheaper but hurts one more container must be
// rejected, and rejected loudly enough that an approver could not mistake it
// for an improvement.
func TestSafetyIsNotForSaleAtAnyPrice(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*backtest.Scorecard)
		want string
	}{
		{"one more memory violation", func(sc *backtest.Scorecard) {
			sc.MemViolations = 3
			sc.RegretUSD = 0.0001
			sc.ResourceRegretUSD = 0.0001
			sc.RiskRegretUSD = 0
		}, "memory violations regress"},
		{"one more starved container", func(sc *backtest.Scorecard) {
			sc.CPUStarvation = 2
			sc.RegretUSD = 0.0001
			sc.ResourceRegretUSD = 0.0001
			sc.RiskRegretUSD = 0
		}, "cpu starvation regresses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Decide(gateInput(nil, tc.mut))
			mustFail(t, res, RuleAdmissible)
			if !reasonMentions(res, tc.want) {
				t.Fatalf("the reason must name the safety regression, got %v", res.Reasons)
			}
			// The win is still reported — an approver reading a rejection
			// should see what the candidate was buying, or every rejection
			// looks identical.
			if len(res.Wins) == 0 {
				t.Fatal("a rejected proposal must still report what improved")
			}
		})
	}
}

// FuzzSafetyForEfficiencyIsNeverAccepted is the property: for ANY tolerance a
// caller can construct, a safety regression is fatal. The gate has no field
// that disables it, and this asserts no field combination can be made to.
func FuzzSafetyForEfficiencyIsNeverAccepted(f *testing.F) {
	f.Add(1, 0, 0.0, 0.0, 0.0, 0.0)
	f.Add(0, 1, -1.0, -1.0, 1e9, 1e9)
	f.Add(50, 50, 1e300, -1e300, 0.5, 100.0)

	f.Fuzz(func(t *testing.T, extraMem, extraCPU int, tolUSD, tolPct, tolFlip, tolGap float64) {
		if extraMem < 0 || extraCPU < 0 || (extraMem == 0 && extraCPU == 0) {
			t.Skip()
		}
		if extraMem > 1_000_000 || extraCPU > 1_000_000 {
			t.Skip()
		}
		in := gateInput(nil, func(sc *backtest.Scorecard) {
			sc.MemViolations += extraMem
			sc.CPUStarvation += extraCPU
			// Make it look irresistible on every other axis.
			sc.RegretUSD = -1e6
			sc.ResourceRegretUSD = -1e6
			sc.RiskRegretUSD = 0
			sc.OracleGapPct = 0
			sc.FlipRate = 0
			sc.ForgoneSavingsUSD = 0
		})
		in.Tolerance = Tolerance{
			MinRegretImprovementUSD: tolUSD,
			MinRegretImprovementPct: tolPct,
			MaxFlipRateIncrease:     tolFlip,
			MaxOracleGapIncreasePct: tolGap,
		}
		res := Decide(in)
		if res.Passed {
			t.Fatalf("a safety regression was accepted under tolerance %+v", in.Tolerance)
		}
		if !failedRule(res, RuleAdmissible) {
			t.Fatalf("safety must fail the admissible rule, got %v: %v", res.Failed, res.Reasons)
		}
	})
}

// TestToleranceHasNoSafetyEscapeHatch is the structural half of the rule
// above: pkg/backtest exposes AllowSafetyRegression for an operator plotting
// the risk curve by hand, and this package deliberately does not re-export it.
func TestToleranceHasNoSafetyEscapeHatch(t *testing.T) {
	ty := reflect.TypeOf(Tolerance{})
	for i := 0; i < ty.NumField(); i++ {
		if ty.Field(i).Type.Kind() == reflect.Bool {
			t.Fatalf("Tolerance grew a bool field %q — a proposal gate must have no "+
				"switch that turns a rule off", ty.Field(i).Name)
		}
	}
	if got := DefaultTolerance().backtestTolerance().AllowSafetyRegression; got {
		t.Fatal("the delegated tolerance must pin AllowSafetyRegression false")
	}
	// And no value of the exported fields can flip it.
	for _, v := range []float64{0, 1, -1, 1e300, math.Inf(1), math.NaN()} {
		tol := Tolerance{
			MinRegretImprovementUSD: v,
			MinRegretImprovementPct: v,
			MaxFlipRateIncrease:     v,
			MaxOracleGapIncreasePct: v,
		}
		bt := tol.backtestTolerance()
		if bt.AllowSafetyRegression {
			t.Fatalf("tolerance %v disarmed the safety rule", v)
		}
		if bt.MaxRegretIncreasePct != 0 || bt.MaxRegretIncreaseUSD != 0 {
			t.Fatalf("tolerance %v widened the regret rule: %+v", v, bt)
		}
		if bt.MaxFlipRateIncrease < 0 || bt.MaxOracleGapIncreasePct < 0 {
			t.Fatalf("tolerance %v produced a negative slack: %+v", v, bt)
		}
	}
}

// ---- hostile-caller rules ----

// TestScorecardMustBelongToTheProposedPolicy is the attack where a caller
// submits an excellent scorecard for policy A while proposing policy B.
func TestScorecardMustBelongToTheProposedPolicy(t *testing.T) {
	t.Run("candidate scorecard swapped", func(t *testing.T) {
		in := gateInput(nil, better)
		other := DefaultPolicy()
		other.Rec.MemoryHeadroom = 1.45
		in.CandidateScore = scoreFor(other, better) // great numbers, wrong policy
		mustFail(t, Decide(in), RuleIdentity)
	})
	t.Run("baseline scorecard swapped", func(t *testing.T) {
		in := gateInput(nil, better)
		other := DefaultPolicy()
		other.Rec.MemoryHeadroom = 1.45
		in.BaselineScore = scoreFor(other, nil)
		mustFail(t, Decide(in), RuleIdentity)
	})
	t.Run("candidate is the incumbent restated", func(t *testing.T) {
		in := GateInput{
			Baseline:       baselinePolicy(),
			Candidate:      baselinePolicy(),
			BaselineScore:  scoreFor(baselinePolicy(), nil),
			CandidateScore: scoreFor(baselinePolicy(), better),
			Envelope:       DefaultEnvelope(),
			Tolerance:      DefaultTolerance(),
		}
		mustFail(t, Decide(in), RuleIdentity)
	})
}

func TestEnvelopeIsEnforcedAtAcceptanceNotOnlyAtGeneration(t *testing.T) {
	in := gateInput(nil, better)
	out := DefaultPolicy()
	out.Rec.CPUHeadroom = 1.95 // legal per hard bounds, outside the envelope
	in.Candidate = out
	in.CandidateScore = scoreFor(out, better)
	res := Decide(in)
	mustFail(t, res, RuleEnvelope)
	if !reasonMentions(res, "cpu-headroom") {
		t.Fatalf("the reason must name the axis, got %v", res.Reasons)
	}
}

func TestGateRejectsAnInvalidEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  Envelope
	}{
		{"empty", Envelope{}},
		{"unknown axis", Envelope{Bounds: map[Axis]Range{"nonsense": {Min: 0, Max: 1}}}},
		{"inverted", Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 1.5, Max: 1.1}}}},
		{"NaN bound", Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 1, Max: math.NaN()}}}},
		{"wider than hard bounds", Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 0.1, Max: 9}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := gateInput(nil, better)
			in.Envelope = tc.env
			mustFail(t, Decide(in), RuleWellFormed)
		})
	}
}

func TestGateRejectsAnUnrunnableCandidatePolicy(t *testing.T) {
	in := gateInput(nil, better)
	bad := DefaultPolicy()
	bad.Rec.MinSamples = 0 // recommend.New rejects this
	in.Candidate = bad
	in.CandidateScore = scoreFor(bad, better)
	// Widen the envelope so the failure is unambiguously about validity.
	in.Envelope = Envelope{Bounds: HardBounds()}
	mustFail(t, Decide(in), RuleWellFormed)
}

func TestCoverageMismatchVoidsTheComparison(t *testing.T) {
	t.Run("scored pairs", func(t *testing.T) {
		in := gateInput(nil, func(sc *backtest.Scorecard) {
			better(sc)
			sc.Scored = 59
		})
		mustFail(t, Decide(in), RuleCoverage)
	})
	t.Run("instants", func(t *testing.T) {
		in := gateInput(nil, func(sc *backtest.Scorecard) {
			better(sc)
			sc.Instants = 5
		})
		mustFail(t, Decide(in), RuleCoverage)
	})
}

// TestGateStopsAtIncomparability mirrors pkg/backtest's rule: once the
// yardsticks differ, no metric below is on the same scale, so no metric-level
// reason may be reported.
func TestGateStopsAtIncomparability(t *testing.T) {
	in := gateInput(nil, func(sc *backtest.Scorecard) {
		better(sc)
		sc.HorizonHours = 48
		sc.Scored = 1 // would also trip coverage, if coverage were reached
	})
	res := Decide(in)
	mustFail(t, res, RuleAdmissible)
	if failedRule(res, RuleCoverage) || failedRule(res, RuleStrictImprovement) {
		t.Fatalf("metric rules ran on incomparable scorecards: %v", res.Failed)
	}
}

func TestGateIsDeterministic(t *testing.T) {
	// A candidate that breaks several rules at once: the reason list must be
	// identical run to run, or a proposal's bytes are not stable.
	mk := func() GateInput {
		in := gateInput(nil, func(sc *backtest.Scorecard) {
			sc.MemViolations = 9
			sc.CPUStarvation = 9
			sc.FlipRate = 0.9
			sc.RegretUSD = 99
		})
		out := DefaultPolicy()
		out.Rec.CPUHeadroom = 1.95
		out.Rec.MemoryHeadroom = 1.95
		in.Candidate = out
		in.CandidateScore.Policy = out.Hash()
		return in
	}
	first := Decide(mk())
	for i := 0; i < 50; i++ {
		got := Decide(mk())
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("run %d differed:\n%+v\n%+v", i, first, got)
		}
	}
	if len(first.Failed) != len(first.Reasons) {
		t.Fatalf("Failed and Reasons must stay index-aligned: %d vs %d",
			len(first.Failed), len(first.Reasons))
	}
}

func TestGateNeverReportsANonFiniteMargin(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -5} {
		in := gateInput(func(sc *backtest.Scorecard) { sc.RegretUSD = v }, better)
		res := Decide(in)
		m := res.RequiredRegretImprovementUSD
		if math.IsNaN(m) || math.IsInf(m, 0) || m < 0 {
			t.Fatalf("baseline regret %v produced margin %v", v, m)
		}
	}
}
