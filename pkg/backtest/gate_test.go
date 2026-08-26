package backtest

import (
	"strings"
	"testing"
	"time"
)

func baseCard() *Scorecard {
	return &Scorecard{
		Cluster:               "prod",
		Window:                [2]time.Time{propStart, propStart.Add(7 * 24 * time.Hour)},
		HorizonHours:          24,
		DecisionIntervalHours: 24,
		StarvationFactor:      1,
		Cost:                  DefaultCostModel(),
		Decisions:             100,
		MemViolations:         2,
		CPUStarvation:         3,
		MemOOMKills:           1,
		OracleGapPct:          20,
		FlipRate:              0.10,
		RegretUSD:             100,
	}
}

func TestGateAdmitsAStrictImprovement(t *testing.T) {
	cur := baseCard()
	cand := baseCard()
	cand.MemViolations = 1
	cand.CPUStarvation = 0
	cand.OracleGapPct = 15
	cand.RegretUSD = 40
	cand.FlipRate = 0.05

	ok, reasons := Gate(cur, cand, DefaultTolerance())
	if !ok {
		t.Fatalf("a strictly better candidate was rejected: %v", reasons)
	}
	if len(reasons) != 0 {
		t.Fatalf("a passing gate must report no reasons, got %v", reasons)
	}
}

func TestGateRejections(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Scorecard)
		want string
	}{
		{"more memory violations", func(s *Scorecard) { s.MemViolations++; s.RegretUSD = 1 }, "memory violations regress"},
		{"more cpu starvation", func(s *Scorecard) { s.CPUStarvation++; s.RegretUSD = 1 }, "cpu starvation regresses"},
		{"worse regret", func(s *Scorecard) { s.RegretUSD = 101 }, "regret regresses"},
		{"worse flip rate", func(s *Scorecard) { s.FlipRate = 0.2; s.RegretUSD = 1 }, "flip rate regresses"},
		{"worse gap with no regret win", func(s *Scorecard) { s.OracleGapPct = 30 }, "oracle gap regresses"},
		{"different cluster", func(s *Scorecard) { s.Cluster = "staging" }, "cluster mismatch"},
		{"different window", func(s *Scorecard) { s.Window[1] = s.Window[1].Add(time.Hour) }, "window mismatch"},
		{"different horizon", func(s *Scorecard) { s.HorizonHours = 48 }, "horizon mismatch"},
		{"different cadence", func(s *Scorecard) { s.DecisionIntervalHours = 12 }, "decision cadence mismatch"},
		{"different starvation factor", func(s *Scorecard) { s.StarvationFactor = 2 }, "starvation factor mismatch"},
		{"different cost model", func(s *Scorecard) { s.Cost.IncidentUSD = 10 }, "cost model mismatch"},
		{"different observed OOMKills", func(s *Scorecard) { s.MemOOMKills = 9; s.RegretUSD = 1 }, "did not replay the same history"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cand := baseCard()
			tc.mut(cand)
			ok, reasons := Gate(baseCard(), cand, DefaultTolerance())
			if ok {
				t.Fatalf("the gate admitted %q", tc.name)
			}
			if !containsSubstring(reasons, tc.want) {
				t.Fatalf("reasons %v do not mention %q", reasons, tc.want)
			}
		})
	}
}

// TestGateLetsSafetyBuyEfficiency pins the one rule that is not a plain
// dominance test: a candidate that clearly improves regret is not blocked by
// an oracle gap that got worse, because regret already priced that waste.
func TestGateLetsSafetyBuyEfficiency(t *testing.T) {
	cur := baseCard()
	cand := baseCard()
	cand.CPUStarvation = 0 // a real safety win
	cand.OracleGapPct = 60 // paid for with idle headroom
	cand.RegretUSD = 30    // and still cheaper overall

	if ok, reasons := Gate(cur, cand, DefaultTolerance()); !ok {
		t.Fatalf("a safer, cheaper candidate was blocked by the gap diagnostic: %v", reasons)
	}
	// The same gap regression without the regret win must still be blocked.
	cand.RegretUSD = cur.RegretUSD
	if ok, _ := Gate(cur, cand, DefaultTolerance()); ok {
		t.Fatal("a gap regression with no regret win must not pass")
	}
}

func TestGateFailsClosedOnNil(t *testing.T) {
	for _, tc := range []struct{ cur, cand *Scorecard }{
		{nil, baseCard()}, {baseCard(), nil}, {nil, nil},
	} {
		if ok, reasons := Gate(tc.cur, tc.cand, DefaultTolerance()); ok || len(reasons) == 0 {
			t.Fatalf("a nil scorecard passed the gate (ok=%v reasons=%v)", ok, reasons)
		}
	}
}

func TestGateStopsAtIncomparability(t *testing.T) {
	cand := baseCard()
	cand.Cluster = "staging"
	cand.MemViolations = 100 // would also fail safety
	_, reasons := Gate(baseCard(), cand, DefaultTolerance())
	if containsSubstring(reasons, "memory violations regress") {
		t.Fatalf("incomparable scorecards must not be compared metric-by-metric: %v", reasons)
	}
}

func TestGateToleranceIsHonouredAndCannotBeWidenedByGarbage(t *testing.T) {
	cur := baseCard()
	cand := baseCard()
	cand.RegretUSD = 105

	if ok, _ := Gate(cur, cand, DefaultTolerance()); ok {
		t.Fatal("a 5% regret increase passed the zero-tolerance default")
	}
	tol := DefaultTolerance()
	tol.MaxRegretIncreasePct = 10
	if ok, reasons := Gate(cur, cand, tol); !ok {
		t.Fatalf("a 5%% increase failed a 10%% tolerance: %v", reasons)
	}
	tol = DefaultTolerance()
	tol.MaxRegretIncreaseUSD = 10
	if ok, reasons := Gate(cur, cand, tol); !ok {
		t.Fatalf("a $5 increase failed a $10 tolerance: %v", reasons)
	}
	// Negative and NaN tolerances clamp to zero rather than widening.
	tol = Tolerance{MaxRegretIncreasePct: -1000, MaxRegretIncreaseUSD: zeroDiv(),
		MaxOracleGapIncreasePct: zeroDiv(), MaxFlipRateIncrease: -1}
	if ok, _ := Gate(cur, cand, tol); ok {
		t.Fatal("garbage tolerances widened the gate")
	}
}

func TestGateSafetyOverrideIsExplicit(t *testing.T) {
	cur := baseCard()
	cand := baseCard()
	cand.MemViolations = 20
	cand.RegretUSD = 1

	if ok, _ := Gate(cur, cand, DefaultTolerance()); ok {
		t.Fatal("safety regression passed the default gate")
	}
	tol := DefaultTolerance()
	tol.AllowSafetyRegression = true
	if ok, reasons := Gate(cur, cand, tol); !ok {
		t.Fatalf("the explicit override did not apply: %v", reasons)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
