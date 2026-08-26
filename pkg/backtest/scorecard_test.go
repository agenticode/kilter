package backtest

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func res(milli int64, gib float64) model.Resources {
	return model.Resources{MilliCPU: milli, MemoryBytes: int64(gib * bytesPerGiB)}
}

func testCost() CostModel {
	return CostModel{CPUUSDPerCoreHour: 0.02, MemUSDPerGiBHour: 0.01, IncidentUSD: 10}
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestScoreArithmetic works the aggregation on three hand-built decisions
// whose dollars can be checked with a pencil: one resize, one idle refusal,
// one refusal over a window that went wrong.
func TestScoreArithmetic(t *testing.T) {
	key := func(n string) model.ContainerKey {
		return model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
			Namespace: "default", Name: n}, Container: "app"}
	}
	recs := []record{
		{ // a resize: 2 cores/4GiB → 1 core/2GiB, oracle 0.8 core/1GiB
			Key: key("a"), At: propStart, Applied: true,
			Current: res(2000, 4), Target: res(1000, 2), Chosen: res(1000, 2),
			Oracle: res(800, 1), Samples: 10,
		},
		{ // an idle refusal: nothing changed, nothing went wrong
			Key: key("b"), At: propStart, Code: CodeBelowChangeThreshold,
			Current: res(2000, 4), Target: res(2000, 4), Chosen: res(2000, 4),
			Oracle: res(800, 1), Samples: 10,
		},
		{ // a refusal over a turbulent window, and the sizing did not hold
			Key: key("c"), At: propStart, Code: CodeModeGuarded,
			Current: res(500, 1), Target: res(500, 1), Chosen: res(500, 1),
			Oracle: res(800, 2), Samples: 10, OOMKills: 1, Adverse: true,
			MemViolation: true, CPUStarved: true,
		},
	}
	sc := score(recs, testCost(), 10*time.Hour, 7*24*time.Hour)

	if sc.Scored != 3 || sc.Decisions != 1 {
		t.Fatalf("scored=%d decisions=%d, want 3 and 1", sc.Scored, sc.Decisions)
	}
	if sc.Refusals[CodeBelowChangeThreshold] != 1 || sc.Refusals[CodeModeGuarded] != 1 {
		t.Fatalf("refusals = %v", sc.Refusals)
	}
	if sc.MemViolations != 1 || sc.CPUStarvation != 1 || sc.MemOOMKills != 1 {
		t.Fatalf("safety counters: mem=%d cpu=%d ooms=%d", sc.MemViolations, sc.CPUStarvation, sc.MemOOMKills)
	}
	if sc.RefusalsGood != 1 || sc.RefusalsIdle != 1 {
		t.Fatalf("refusal quality: good=%d idle=%d", sc.RefusalsGood, sc.RefusalsIdle)
	}

	// Hourly prices: a = 0.02+0.02 = 0.04, b = 0.04+0.04 = 0.08,
	// c = 0.01+0.01 = 0.02; oracles 0.026, 0.026, 0.036. Times ten hours.
	near(t, "PolicyCostUSD", sc.PolicyCostUSD, 0.40+0.80+0.20)
	near(t, "OracleCostUSD", sc.OracleCostUSD, 0.26+0.26+0.36)
	near(t, "ResourceRegretUSD", sc.ResourceRegretUSD, 1.40-0.88)
	near(t, "RiskRegretUSD", sc.RiskRegretUSD, 2*10) // two violations on one decision
	near(t, "RegretUSD", sc.RegretUSD, 0.52+20)
	near(t, "ForgoneSavingsUSD", sc.ForgoneSavingsUSD, 0.80-0.26)
	near(t, "ClaimedSavingsUSD", sc.ClaimedSavingsUSD, 0.80-0.40)
	near(t, "RealizedSavingsUSD", sc.RealizedSavingsUSD, 0.80-0.40)
	near(t, "ClaimedVsRealized", sc.ClaimedVsRealized, 1)

	gapA := (0.40 - 0.26) / 0.26
	gapB := (0.80 - 0.26) / 0.26
	gapC := (0.20 - 0.36) / 0.36
	near(t, "OracleGapPct", sc.OracleGapPct, (gapA+gapB+gapC)/3*100)
	near(t, "OracleGapPctApplied", sc.OracleGapPctApplied, gapA*100)

	// An undersized decision must show a negative gap; that is the signature
	// the risk term is there to pair with, not a bug in the arithmetic.
	if gapC >= 0 {
		t.Fatal("the undersized decision should price below the oracle")
	}
}

func TestClaimedVsRealizedGivesBackUnsafeSavings(t *testing.T) {
	key := model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
		Namespace: "default", Name: "a"}, Container: "app"}
	// Claimed: 2 cores/4GiB → 0.5 core/1GiB. Hindsight says 1 core/2GiB was
	// the floor, so a third of the promised saving could never have been kept.
	recs := []record{{
		Key: key, At: propStart, Applied: true,
		Current: res(2000, 4), Target: res(500, 1), Chosen: res(500, 1),
		Oracle: res(1000, 2), Samples: 10, MemViolation: true, CPUStarved: true,
	}}
	sc := score(recs, testCost(), 10*time.Hour, 7*24*time.Hour)

	near(t, "ClaimedSavingsUSD", sc.ClaimedSavingsUSD, (0.08-0.02)*10)
	near(t, "RealizedSavingsUSD", sc.RealizedSavingsUSD, (0.08-0.04)*10)
	near(t, "ClaimedVsRealized", sc.ClaimedVsRealized, 0.4/0.6)
	if !(sc.ClaimedVsRealized < 1) {
		t.Fatalf("an undersizing decision realized %v of its claim", sc.ClaimedVsRealized)
	}
}

func TestClaimedVsRealizedIsZeroWithoutClaims(t *testing.T) {
	key := model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
		Namespace: "default", Name: "a"}, Container: "app"}
	// A resize that grows the container claims no savings; it buys safety.
	recs := []record{{
		Key: key, At: propStart, Applied: true,
		Current: res(500, 1), Target: res(2000, 4), Chosen: res(2000, 4),
		Oracle: res(1800, 3), Samples: 10,
	}}
	sc := score(recs, testCost(), 10*time.Hour, 7*24*time.Hour)
	if sc.ClaimedSavingsUSD != 0 || sc.RealizedSavingsUSD != 0 || sc.ClaimedVsRealized != 0 {
		t.Fatalf("a growth-only run reported claimed=%v realized=%v ratio=%v",
			sc.ClaimedSavingsUSD, sc.RealizedSavingsUSD, sc.ClaimedVsRealized)
	}
}

func TestFlipRateCountsTargetReversals(t *testing.T) {
	key := model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
		Namespace: "default", Name: "a"}, Container: "app"}
	targets := []int64{1000, 500, 900, 400} // shrink, grow, shrink
	var recs []record
	for i, tgt := range targets {
		recs = append(recs, record{
			Key: key, At: propStart.Add(time.Duration(i) * 24 * time.Hour), Applied: true,
			Current: res(2000, 4), Target: res(tgt, 2), Chosen: res(tgt, 2),
			Oracle: res(400, 1), Samples: 10,
		})
	}
	sc := score(recs, testCost(), 24*time.Hour, 7*24*time.Hour)
	if sc.Flips != 2 {
		t.Fatalf("flips = %d, want 2 (grow after shrink, then shrink after grow)", sc.Flips)
	}
	near(t, "FlipRate", sc.FlipRate, 0.5)

	// Spread the same sequence beyond the flip window and nothing churns.
	for i := range recs {
		recs[i].At = propStart.Add(time.Duration(i) * 10 * 24 * time.Hour)
	}
	if sc := score(recs, testCost(), 24*time.Hour, 7*24*time.Hour); sc.Flips != 0 {
		t.Fatalf("flips = %d across 10-day gaps with a 7-day window, want 0", sc.Flips)
	}
}

func TestFlipRateIgnoresRepeatedIdenticalTargets(t *testing.T) {
	key := model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
		Namespace: "default", Name: "a"}, Container: "app"}
	var recs []record
	for i := 0; i < 5; i++ {
		recs = append(recs, record{
			Key: key, At: propStart.Add(time.Duration(i) * 24 * time.Hour), Applied: true,
			Current: res(2000, 4), Target: res(700, 2), Chosen: res(700, 2),
			Oracle: res(400, 1), Samples: 10,
		})
	}
	if sc := score(recs, testCost(), 24*time.Hour, 7*24*time.Hour); sc.Flips != 0 || sc.FlipRate != 0 {
		t.Fatalf("a policy that says the same thing five times churned: %d flips", sc.Flips)
	}
}

func TestFlipsAreCountedPerContainer(t *testing.T) {
	mk := func(n string) model.ContainerKey {
		return model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
			Namespace: "default", Name: n}, Container: "app"}
	}
	// Interleaved containers whose individual sequences never reverse. If
	// the walk lost track of which container it was on, this would flip.
	var recs []record
	for i, tgt := range []int64{1000, 900, 800} {
		for _, n := range []string{"a", "b"} {
			recs = append(recs, record{
				Key: mk(n), At: propStart.Add(time.Duration(i) * 24 * time.Hour), Applied: true,
				Current: res(2000, 4), Target: res(tgt, 2), Chosen: res(tgt, 2),
				Oracle: res(400, 1), Samples: 10,
			})
		}
	}
	if sc := score(recs, testCost(), 24*time.Hour, 7*24*time.Hour); sc.Flips != 0 {
		t.Fatalf("interleaved containers produced %d spurious flips", sc.Flips)
	}
}

func TestZeroCostOracleIsExcludedFromTheGapMean(t *testing.T) {
	key := func(n string) model.ContainerKey {
		return model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
			Namespace: "default", Name: n}, Container: "app"}
	}
	recs := []record{
		{Key: key("idle"), At: propStart, Applied: true,
			Current: res(100, 0.5), Target: res(10, 0), Chosen: res(10, 0),
			Oracle: model.Resources{}, Samples: 10},
		{Key: key("busy"), At: propStart, Applied: true,
			Current: res(2000, 4), Target: res(1000, 2), Chosen: res(1000, 2),
			Oracle: res(800, 1), Samples: 10},
	}
	sc := score(recs, testCost(), 10*time.Hour, 7*24*time.Hour)
	// Only the second decision has a denominator, so the mean is its gap.
	near(t, "OracleGapPct", sc.OracleGapPct, (0.40-0.26)/0.26*100)
	if math.IsNaN(sc.OracleGapPct) || math.IsInf(sc.OracleGapPct, 0) {
		t.Fatalf("a zero-cost oracle leaked into the mean: %v", sc.OracleGapPct)
	}
	// It still counts in the totals — the container was scored, after all.
	if sc.Scored != 2 {
		t.Fatalf("scored = %d, want 2", sc.Scored)
	}
}

func TestScoreOfNothingIsAllZeroes(t *testing.T) {
	sc := score(nil, testCost(), 24*time.Hour, 7*24*time.Hour)
	if sc.Scored != 0 || sc.Decisions != 0 || sc.RegretUSD != 0 || sc.FlipRate != 0 {
		t.Fatalf("empty score = %+v", sc)
	}
	if sc.Refusals == nil {
		t.Fatal("Refusals must be non-nil so the JSON shape never changes")
	}
	if _, err := sc.Encode(); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeSortsRefusalCodes(t *testing.T) {
	sc := score(nil, testCost(), time.Hour, time.Hour)
	for _, code := range []string{CodePlanDropped, CodeBelowConfidence, CodeModeGuarded, CodeBelowChangeThreshold} {
		sc.Refusals[code] = 1
	}
	b, err := sc.Encode()
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	last := -1
	for _, code := range []string{CodeBelowChangeThreshold, CodeBelowConfidence, CodeModeGuarded, CodePlanDropped} {
		at := strings.Index(body, `"`+code+`"`)
		if at < 0 {
			t.Fatalf("code %q missing from the encoding", code)
		}
		if at < last {
			t.Fatalf("refusal codes are not encoded in sorted order:\n%s", body)
		}
		last = at
	}
	if !strings.HasSuffix(body, "}\n") {
		t.Fatal("encoded scorecards must end with a newline")
	}
}

func TestReversedOnlyCountsOppositeMoves(t *testing.T) {
	tests := []struct {
		prev, next direction
		want       bool
	}{
		{dirShrink, dirGrow, true},
		{dirGrow, dirShrink, true},
		{dirShrink, dirShrink, false},
		{dirGrow, dirGrow, false},
		{dirNone, dirGrow, false},
		{dirGrow, dirNone, false},
		{dirNone, dirNone, false},
	}
	for _, tc := range tests {
		if got := reversed(tc.prev, tc.next); got != tc.want {
			t.Errorf("reversed(%d, %d) = %v, want %v", tc.prev, tc.next, got, tc.want)
		}
	}
	if dirOf(5, 5) != dirNone || dirOf(5, 4) != dirShrink || dirOf(4, 5) != dirGrow {
		t.Fatal("dirOf disagrees with its own names")
	}
}
