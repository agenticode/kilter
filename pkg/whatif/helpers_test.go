package whatif

import (
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
)

// Fixed instants. No test in this package may read the wall clock: the whole
// point of taking a Clock argument is that a proposal is reproducible, and a
// test that used time.Now would be unable to assert byte-identity.
var (
	testFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	testTo   = time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)
	testNow  = time.Date(2026, 1, 8, 3, 0, 0, 0, time.UTC)
)

// fixedClock is the Clock every test uses.
func fixedClock() Clock { return func() time.Time { return testNow } }

// baselinePolicy is the shipped policy; candidatePolicy differs on exactly one
// in-envelope axis, which is the shape every real proposal has.
func baselinePolicy() Policy { return DefaultPolicy() }

func candidatePolicy() Policy {
	p := DefaultPolicy()
	p.Rec.CPUHeadroom = 1.20
	return p
}

// scoreFor builds a scorecard that is internally consistent with a policy:
// same hash, same yardstick, plausible coverage. mut applies the one thing the
// test is actually about, so each test reads as "everything equal, except…".
func scoreFor(p Policy, mut func(*backtest.Scorecard)) *backtest.Scorecard {
	sc := &backtest.Scorecard{
		Policy:                p.Hash(),
		Cluster:               "c1",
		Window:                [2]time.Time{testFrom, testTo},
		HorizonHours:          24,
		DecisionIntervalHours: 24,
		StarvationFactor:      1,
		Snapshots:             2016,
		Instants:              6,
		Scored:                60,
		Decisions:             12,
		Refusals:              map[string]int{backtest.CodeBelowChangeThreshold: 48},
		MemViolations:         2,
		CPUStarvation:         1,
		MemOOMKills:           3,
		OracleGapPct:          30,
		OracleGapPctApplied:   12,
		PolicyCostUSD:         100,
		OracleCostUSD:         80,
		ForgoneSavingsUSD:     5,
		FlipRate:              0.05,
		Flips:                 1,
		ResourceRegretUSD:     20,
		RiskRegretUSD:         10,
		RegretUSD:             30,
		Cost:                  backtest.DefaultCostModel(),
	}
	if mut != nil {
		mut(sc)
	}
	return sc
}

// gateInput assembles the standard comparison: shipped policy vs a candidate
// that is strictly better on regret and no worse anywhere else.
func gateInput(mutBase, mutCand func(*backtest.Scorecard)) GateInput {
	return GateInput{
		Baseline:       baselinePolicy(),
		Candidate:      candidatePolicy(),
		BaselineScore:  scoreFor(baselinePolicy(), mutBase),
		CandidateScore: scoreFor(candidatePolicy(), mutCand),
		Envelope:       DefaultEnvelope(),
		Tolerance:      DefaultTolerance(),
	}
}

// better is the standard "this candidate deserves to pass" mutation.
func better(sc *backtest.Scorecard) {
	sc.RegretUSD = 20
	sc.ResourceRegretUSD = 12
	sc.RiskRegretUSD = 8
	sc.OracleGapPct = 22
}

// failedRule reports whether the named rule is among the failures.
func failedRule(res GateResult, r Rule) bool {
	for _, f := range res.Failed {
		if f == r {
			return true
		}
	}
	return false
}

// reasonMentions reports whether any reason contains the substring.
func reasonMentions(res GateResult, sub string) bool {
	for _, r := range res.Reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func mustPass(t *testing.T, res GateResult) {
	t.Helper()
	if !res.Passed {
		t.Fatalf("expected the gate to pass, it failed: %v", res.Reasons)
	}
}

func mustFail(t *testing.T, res GateResult, r Rule) {
	t.Helper()
	if res.Passed {
		t.Fatalf("expected the gate to reject on %s, it passed", r)
	}
	if !failedRule(res, r) {
		t.Fatalf("expected rule %s to fire, got %v (%v)", r, res.Failed, res.Reasons)
	}
}
