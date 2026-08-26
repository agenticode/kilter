package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/backtest"
)

// These tests drive pkg/backtest's REAL harness through the REAL CLI entry
// point. The traces are synthetic and the oracles are known in closed form,
// so every number below is reproducible: there is no clock in the command
// (backtestEpoch is a constant) and no network anywhere.

func runBacktestOK(t *testing.T, args ...string) string {
	t.Helper()
	var b strings.Builder
	if err := runBacktestTo(&b, args); err != nil {
		t.Fatalf("kilter backtest %s: %v\n%s", strings.Join(args, " "), err, b.String())
	}
	return b.String()
}

func backtestScorecard(t *testing.T, args ...string) *backtest.Scorecard {
	t.Helper()
	raw := runBacktestOK(t, append(args, "--json")...)
	var sc backtest.Scorecard
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		t.Fatalf("decode scorecard: %v\n%s", err, raw)
	}
	return &sc
}

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBacktestReproducesTheShippedGoldens.
//
// pkg/backtest/FINDINGS.md publishes what its goldens say about the shipped
// engine over 7 days and 2 workloads. Those numbers were produced inside the
// package; this asserts the CLI reaches the same ones, which is the difference
// between a harness that exists and a harness a user can run.
func TestBacktestReproducesTheShippedGoldens(t *testing.T) {
	for _, tc := range []struct {
		archetype  string
		decisions  int
		mem, cpu   int
		gapApplied float64
		flipRate   float64
		regret     float64
	}{
		{"steady", 12, 0, 0, 17.3, 0, 2.74},
		{"diurnal", 12, 0, 0, 40.5, 0, 4.02},
		{"bursty", 12, 0, 0, 23.4, 0, 2.90},
		{"regime-change", 12, 2, 2, 5.7, 0.167, 202.57},
	} {
		t.Run(tc.archetype, func(t *testing.T) {
			sc := backtestScorecard(t, "--demo", tc.archetype)
			if sc.Decisions != tc.decisions {
				t.Errorf("decisions = %d, want %d", sc.Decisions, tc.decisions)
			}
			if sc.MemViolations != tc.mem || sc.CPUStarvation != tc.cpu {
				t.Errorf("safety = (mem %d, cpu %d), want (%d, %d)",
					sc.MemViolations, sc.CPUStarvation, tc.mem, tc.cpu)
			}
			if !near(sc.OracleGapPctApplied, tc.gapApplied, 0.05) {
				t.Errorf("applied oracle gap = %.2f, want %.1f", sc.OracleGapPctApplied, tc.gapApplied)
			}
			if !near(sc.FlipRate, tc.flipRate, 0.001) {
				t.Errorf("flip rate = %.3f, want %.3f", sc.FlipRate, tc.flipRate)
			}
			if !near(sc.RegretUSD, tc.regret, 0.005) {
				t.Errorf("regret = $%.2f, want $%.2f", sc.RegretUSD, tc.regret)
			}
		})
	}
}

// TestOracleGapIsRenderedInTheUnitsTheScorecardUses.
//
// Regression for a real bug in this wiring: Scorecard.OracleGapPct is ALREADY
// multiplied by 100 inside pkg/backtest, while its doc comment states the
// unscaled ratio. The first draft of the renderer multiplied again and printed
// a 9,445 % oracle gap for a trace whose real gap is 94.5 %.
func TestOracleGapIsRenderedInTheUnitsTheScorecardUses(t *testing.T) {
	sc := backtestScorecard(t, "--demo", "regime-change")
	out := runBacktestOK(t, "--demo", "regime-change")
	want := strings.TrimSpace(strings.Split(strings.TrimSpace(
		strings.Replace(strings.Split(out, "oracleGap ")[1], "%", " ", 1)), " ")[0])
	if want != "94.5" {
		t.Fatalf("rendered oracle gap %q, want 94.5", want)
	}
	if !near(sc.OracleGapPct, 94.5, 0.05) {
		t.Fatalf("scorecard oracle gap = %v, want ~94.5", sc.OracleGapPct)
	}
}

// TestBacktestLiveHistoryRefusesRatherThanScoringOneSnapshot.
//
// This is the honest half of the command. pkg/store keeps only the LATEST
// snapshot per cluster, so a live replay has no history to replay. Running the
// harness against that one snapshot would yield a scorecard with the same
// shape, the same field names and the same confident tone as a real one — the
// worst possible failure, because the number looks fine.
func TestBacktestLiveHistoryRefusesRatherThanScoringOneSnapshot(t *testing.T) {
	var b strings.Builder
	err := runBacktestTo(&b, []string{"--cluster", "prod"})
	if err == nil {
		t.Fatalf("a live backtest was accepted:\n%s", b.String())
	}
	msg := err.Error()
	for _, want := range []string{
		"snapshot history is not persisted",
		"pkg/store",
		"SaveSnapshotAt",
		"Snapshots(cluster, from, to)",
		"backtest.SnapshotSource",
		"--demo",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(b.String(), "regret") {
		t.Errorf("a scorecard was printed for a cluster with no history:\n%s", b.String())
	}
	// And it refuses rather than quietly preferring one source over the other.
	if err := runBacktestTo(&b, []string{"--cluster", "prod", "--demo", "steady"}); err == nil {
		t.Error("--cluster and --demo together were accepted")
	}
}

// TestWiringTheDecisionLayerIsAnImprovementThroughTheCLI.
//
// pkg/backtest's headline result, reproduced end to end: on the regime-change
// trace, enforcing pkg/decision's refusal predicates removes ALL CPU
// starvation and halves total regret, bought with a few dollars of extra idle
// headroom. The test asserts the win AND that it was paid for — a scorecard
// that reported only the win would be an advertisement.
func TestWiringTheDecisionLayerIsAnImprovementThroughTheCLI(t *testing.T) {
	enforced := writePolicy(t, `{"enforceDecisionRefusals": true}`)
	raw := runBacktestOK(t, "--demo", "regime-change", "--workloads", "3",
		"--compare", enforced, "--json")
	var env struct {
		Current   *backtest.Scorecard `json:"current"`
		Candidate *backtest.Scorecard `json:"candidate"`
		Gate      struct {
			Accepted bool     `json:"accepted"`
			Reasons  []string `json:"reasons"`
		} `json:"gate"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	cur, cand := env.Current, env.Candidate

	if cur.CPUStarvation == 0 {
		t.Fatal("the shipped policy starved nothing; the A/B proves nothing")
	}
	if cand.CPUStarvation != 0 {
		t.Errorf("enforced refusals left %d CPU starvations, want 0", cand.CPUStarvation)
	}
	if cand.RegretUSD >= cur.RegretUSD/1.5 {
		t.Errorf("regret $%.2f -> $%.2f: expected roughly a halving", cur.RegretUSD, cand.RegretUSD)
	}
	if cand.ResourceRegretUSD <= cur.ResourceRegretUSD {
		t.Errorf("resource regret $%.2f -> $%.2f: the safety win must be PAID for, "+
			"and the scorecard must report both halves of the trade",
			cur.ResourceRegretUSD, cand.ResourceRegretUSD)
	}
	if cur.MemViolations != cand.MemViolations {
		t.Errorf("memory violations moved (%d -> %d); the level-shift window is "+
			"unavoidable and no policy should change it", cur.MemViolations, cand.MemViolations)
	}
	if !env.Gate.Accepted {
		t.Errorf("Gate rejected a strict improvement: %v", env.Gate.Reasons)
	}
	if _, ok := cand.Refusals["post-change-soak"]; !ok {
		t.Errorf("no post-change-soak refusal fired: %v", cand.Refusals)
	}
}

// TestFailOnRegressionIsTheCIGate: a policy that refuses everything banks no
// savings and still pays for whatever the unchanged sizing did, so it cannot
// dominate — and --fail-on-regression turns that into an exit code.
func TestFailOnRegressionIsTheCIGate(t *testing.T) {
	refuser := writePolicy(t, `{"recommend": {"minSamples": 1000000}}`)

	// Without the flag the comparison is reported and the command succeeds.
	out := runBacktestOK(t, "--demo", "steady", "--compare", refuser)
	if !strings.Contains(out, "REJECTED") {
		t.Fatalf("Gate accepted a refuse-everything policy:\n%s", out)
	}

	var b strings.Builder
	err := runBacktestTo(&b, []string{"--demo", "steady", "--compare", refuser, "--fail-on-regression"})
	if err == nil {
		t.Fatal("--fail-on-regression did not fail on a rejected candidate")
	}
	if !strings.Contains(b.String(), "REJECTED") {
		t.Errorf("the reasons were not printed alongside the failure:\n%s", b.String())
	}
}

// TestBacktestOutputIsByteIdenticalAcrossRuns. Go randomizes map iteration on
// every range, so repeating in ONE process is the real determinism test.
func TestBacktestOutputIsByteIdenticalAcrossRuns(t *testing.T) {
	base := runBacktestOK(t, "--demo", "bursty", "--noise", "0.05")
	baseJSON := runBacktestOK(t, "--demo", "bursty", "--noise", "0.05", "--json")
	for i := 0; i < 6; i++ {
		if got := runBacktestOK(t, "--demo", "bursty", "--noise", "0.05"); got != base {
			t.Fatalf("text run %d differs", i)
		}
		if got := runBacktestOK(t, "--demo", "bursty", "--noise", "0.05", "--json"); got != baseJSON {
			t.Fatalf("json run %d differs", i)
		}
	}
}

// TestPolicyFileFailsLoudly.
//
// A knob misspelled in a policy file that is silently ignored produces a
// scorecard for a policy nobody ran — and the scorecard looks fine. Unknown
// fields are rejected; so is a bare number where a duration belongs, because
// Go would read it as nanoseconds and nobody ever meant that.
func TestPolicyFileFailsLoudly(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown field", `{"recommend": {"cpuHeadrooom": 1.5}}`, "unknown field"},
		{"bare duration", `{"decision": {"baseSoak": "3600"}}`, "durations need a unit"},
		{"bad duration", `{"recommend": {"minWindow": "six hours"}}`, "minWindow"},
		{"not json", `{`, "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePolicy(t, tc.body)
			var b strings.Builder
			err := runBacktestTo(&b, []string{"--demo", "steady", "--policy", path})
			if err == nil {
				t.Fatalf("a broken policy file was accepted:\n%s", b.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
	// A missing knob means DEFAULT, not zero: an empty policy file must score
	// exactly like no policy file at all.
	empty := writePolicy(t, `{}`)
	if runBacktestOK(t, "--demo", "steady", "--policy", empty) != runBacktestOK(t, "--demo", "steady") {
		t.Error("an empty policy file scored differently from the shipped default")
	}
}

// TestBacktestRejectsUnknownArchetypes.
func TestBacktestRejectsUnknownArchetypes(t *testing.T) {
	var b strings.Builder
	if err := runBacktestTo(&b, []string{"--demo", "chaotic"}); err == nil ||
		!strings.Contains(err.Error(), "unknown archetype") {
		t.Fatalf("err = %v, want an unknown-archetype error", err)
	}
	if err := runBacktestTo(&b, nil); err == nil {
		t.Fatal("backtest with no source of history was accepted")
	}
}

// TestDerivedCostsComeFromTheCatalogNotTheDefaults.
func TestDerivedCostsComeFromTheCatalogNotTheDefaults(t *testing.T) {
	def := backtestScorecard(t, "--demo", "steady")
	derived := backtestScorecard(t, "--demo", "steady", "--derive-costs")
	if derived.Cost == def.Cost {
		t.Skip("the embedded catalog happens to agree with the default cost model")
	}
	if derived.Cost.IncidentUSD != def.Cost.IncidentUSD {
		t.Errorf("--derive-costs moved IncidentUSD (%v -> %v); it prices RISK, "+
			"which no node catalog knows about",
			def.Cost.IncidentUSD, derived.Cost.IncidentUSD)
	}
}

func near(got, want, tol float64) bool {
	d := got - want
	return d <= tol && -d <= tol
}
