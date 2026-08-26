package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/whatif"
)

// These tests drive pkg/whatif's REAL Scenario, gate and store through the
// REAL CLI entry point. The traces are synthetic and their oracles are known
// in closed form, so every number below is reproducible: there is no clock in
// the replay path (backtestEpoch is a constant, and --now only ever reaches
// CreatedAt), and no network anywhere.

func runWhatIfOK(t *testing.T, args ...string) string {
	t.Helper()
	var b strings.Builder
	if err := runWhatIfTo(&b, args); err != nil {
		t.Fatalf("kilter whatif %s: %v\n%s", strings.Join(args, " "), err, b.String())
	}
	return b.String()
}

func runWhatIfErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var b strings.Builder
	err := runWhatIfTo(&b, args)
	if err == nil {
		t.Fatalf("kilter whatif %s was accepted:\n%s", strings.Join(args, " "), b.String())
	}
	return b.String(), err
}

func whatifResult(t *testing.T, args ...string) *whatif.Result {
	t.Helper()
	raw := runWhatIfOK(t, append(args, "--json")...)
	var r whatif.Result
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode result: %v\n%s", err, raw)
	}
	return &r
}

// acceptedArgs is a candidate the gate ACCEPTS on the bursty trace: dropping
// CPU headroom to the bottom of the envelope is cheaper here and costs no
// safety. Kept in one place because several tests need a passing result and a
// test that silently started testing a rejection would still pass.
var acceptedArgs = []string{"--demo", "bursty", "--set", "cpu-headroom=1.05"}

// rejectedArgs is the mirror: raising CPU headroom on the regime-change trace
// buys nothing and costs resource regret.
var rejectedArgs = []string{"--demo", "regime-change", "--set", "cpu-headroom=1.30"}

// TestWhatIfRunsBothReplaysAndGatesTheDelta is the end-to-end shape: two
// scorecards, the arithmetic between them, and a verdict.
func TestWhatIfRunsBothReplaysAndGatesTheDelta(t *testing.T) {
	r := whatifResult(t, acceptedArgs...)
	if r.BaselineScore == nil || r.CandidateScore == nil {
		t.Fatal("a result must carry both scorecards; a nil one is not evidence")
	}
	if !r.Gate.Passed {
		t.Fatalf("the shipped accepted-case candidate no longer passes: %v", r.Gate.Reasons)
	}
	if r.BaselineScore.Policy == r.CandidateScore.Policy {
		t.Fatal("both scorecards are for the same policy hash")
	}
	// The delta is arithmetic over the two scorecards and nothing else.
	if got, want := r.Delta.RegretUSD,
		r.CandidateScore.RegretUSD-r.BaselineScore.RegretUSD; !near(got, want, 1e-5) {
		t.Errorf("delta regret %v, want candidate−baseline %v", got, want)
	}
	if got, want := r.Delta.MemViolations,
		r.CandidateScore.MemViolations-r.BaselineScore.MemViolations; got != want {
		t.Errorf("delta memViolations %d, want %d", got, want)
	}
	if len(r.Changes) != 1 || r.Changes[0].Axis != whatif.AxisCPUHeadroom {
		t.Errorf("changes = %+v, want exactly cpu-headroom", r.Changes)
	}
}

// TestTheEvaluationPathCannotBePointedAtThePolicyUnderTest.
//
// pkg/whatif's sharpest constraint: if the evaluation can be traced back to
// the policy under test, the number is worthless. Two halves are asserted
// here, because the CLI is the layer that could reintroduce either.
//
//  1. There is no flag that scores a policy against itself. A candidate equal
//     to the baseline is refused BEFORE anything is replayed, and no scorecard
//     is printed.
//  2. The yardstick is shared. The oracle, the scored set and the ground-truth
//     OOM counter are computed from future usage alone, so they must be
//     IDENTICAL across the two runs no matter how different the policies are.
//     If the evaluation ever started tracing back to the thing under test, the
//     oracle would move with it.
func TestTheEvaluationPathCannotBePointedAtThePolicyUnderTest(t *testing.T) {
	// 1. A what-if against itself is not a question.
	for _, args := range [][]string{
		{"--demo", "steady", "--set", "cpu-headroom=1.15"}, // the shipped value
		{"--demo", "steady", "--candidate", "default"},
	} {
		out, err := runWhatIfErr(t, args...)
		if !strings.Contains(err.Error(), "identical to the baseline") {
			t.Errorf("%v: err = %q, want the identical-candidate refusal", args, err)
		}
		if strings.Contains(out, "regret") || strings.Contains(out, "Gate") {
			t.Errorf("%v: a scorecard was printed for a self-comparison:\n%s", args, out)
		}
	}

	// 2. The two most different policies the envelope allows still share the
	// yardstick, through the CLI.
	aggressive := whatifResult(t, "--demo", "bursty", "--workloads", "3",
		"--set", "cpu-percentile=0.80", "--set", "memory-percentile=0.95",
		"--set", "cpu-headroom=1.05", "--set", "memory-headroom=1.05")
	conservative := whatifResult(t, "--demo", "bursty", "--workloads", "3",
		"--set", "cpu-percentile=0.99", "--set", "memory-percentile=0.999",
		"--set", "cpu-headroom=1.50", "--set", "memory-headroom=1.50")
	if aggressive.CandidateScore.Policy == conservative.CandidateScore.Policy {
		t.Fatal("the two candidates hashed the same; the test proves nothing")
	}
	for _, r := range []*whatif.Result{aggressive, conservative} {
		b, c := r.BaselineScore, r.CandidateScore
		if b.OracleCostUSD != c.OracleCostUSD {
			t.Errorf("oracle cost moved with the policy: %v vs %v", b.OracleCostUSD, c.OracleCostUSD)
		}
		if b.Scored != c.Scored || b.Instants != c.Instants || b.Snapshots != c.Snapshots {
			t.Errorf("coverage moved with the policy: scored %d/%d instants %d/%d snapshots %d/%d",
				b.Scored, c.Scored, b.Instants, c.Instants, b.Snapshots, c.Snapshots)
		}
		if b.MemOOMKills != c.MemOOMKills {
			t.Errorf("ground-truth OOM kills moved with the policy: %d vs %d",
				b.MemOOMKills, c.MemOOMKills)
		}
		if b.StarvationFactor != c.StarvationFactor || b.Cost != c.Cost {
			t.Error("the cost model or starvation factor differed between the two runs")
		}
	}
	// And across the two invocations: the oracle is a property of the history.
	if aggressive.BaselineScore.OracleCostUSD != conservative.BaselineScore.OracleCostUSD {
		t.Error("the oracle differs between two runs over the same trace")
	}
}

// TestSetMovesTheAxisPkgWhatifThinksItMoves.
//
// whatif.Axis.get/set are unexported, so cmd/ restates the five-field
// projection in applyAxisSets. This cross-checks every axis against
// pkg/whatif's OWN projection — Result.Changes is computed by changesBetween,
// which uses Axis.get — so a mis-mapped field (setting memory headroom when
// the caller asked for CPU headroom) fails here rather than quietly tuning the
// wrong knob under the right name.
func TestSetMovesTheAxisPkgWhatifThinksItMoves(t *testing.T) {
	for _, tc := range []struct {
		axis  whatif.Axis
		value string
		want  float64
	}{
		{whatif.AxisCPUPercentile, "0.90", 0.90},
		{whatif.AxisMemoryPercentile, "0.995", 0.995},
		{whatif.AxisCPUHeadroom, "1.25", 1.25},
		{whatif.AxisMemoryHeadroom, "1.35", 1.35},
		{whatif.AxisBaseSoak, "8h", 8},
	} {
		t.Run(string(tc.axis), func(t *testing.T) {
			r := whatifResult(t, "--demo", "steady", "--set", string(tc.axis)+"="+tc.value)
			if len(r.Changes) != 1 {
				t.Fatalf("changes = %+v, want exactly one axis to have moved", r.Changes)
			}
			c := r.Changes[0]
			if c.Axis != tc.axis {
				t.Fatalf("--set %s moved %s instead", tc.axis, c.Axis)
			}
			if !near(c.To, tc.want, 1e-9) {
				t.Errorf("%s = %v, want %v", tc.axis, c.To, tc.want)
			}
		})
	}
	// Every axis at once still moves exactly five, and no more.
	r := whatifResult(t, "--demo", "steady",
		"--set", "cpu-percentile=0.90", "--set", "memory-percentile=0.995",
		"--set", "cpu-headroom=1.25", "--set", "memory-headroom=1.35",
		"--set", "base-soak=8h")
	if len(r.Changes) != len(whatif.AllAxes) {
		t.Fatalf("moved %d axes, want all %d", len(r.Changes), len(whatif.AllAxes))
	}
	for i, c := range r.Changes {
		if c.Axis != whatif.AllAxes[i] {
			t.Errorf("changes[%d] = %s, want AllAxes order (%s)", i, c.Axis, whatif.AllAxes[i])
		}
	}
}

// TestSetIsRejectedRatherThanIgnored. A knob the caller asked to tune that is
// silently dropped produces a what-if whose rationale does not describe what
// was measured — the same failure a misspelled policy-file key would be.
func TestSetIsRejectedRatherThanIgnored(t *testing.T) {
	for _, tc := range []struct{ name, set, want string }{
		{"unknown axis", "gpu-headroom=1.2", "unknown axis"},
		{"typo", "cpu-headrooom=1.2", "unknown axis"},
		{"no equals", "cpu-headroom", "expected AXIS=VALUE"},
		{"not a number", "cpu-headroom=wide", "invalid syntax"},
		{"bare duration", "base-soak=3600", "durations need a unit"},
		{"above hard bound", "cpu-headroom=3.0", "outside the hard bounds"},
		{"below hard bound", "cpu-headroom=0.5", "outside the hard bounds"},
		{"soak past the ceiling", "base-soak=200h", "outside the hard bounds"},
		{"percentile past one", "memory-percentile=1.5", "outside the hard bounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runWhatIfErr(t, "--demo", "steady", "--set", tc.set)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
			if strings.Contains(out, "regret") {
				t.Errorf("a scorecard was printed for a rejected --set:\n%s", out)
			}
		})
	}
}

// TestHardBoundsCannotBeWidenedFromTheCommandLine states the property the
// table above exercises: whatif.HardBounds() is the CLI's limit too, and the
// help text quotes the same map rather than a restatement of it.
func TestHardBoundsCannotBeWidenedFromTheCommandLine(t *testing.T) {
	usage := whatifUsage()
	for axis, r := range whatif.HardBounds() {
		if !strings.Contains(usage, string(axis)) {
			t.Errorf("--help does not name axis %s", axis)
		}
		// Just outside each bound must be refused.
		for _, v := range []float64{r.Min - 0.01, r.Max + 0.01} {
			if axis == whatif.AxisBaseSoak && v < 0 {
				continue // a negative duration is a parse error, covered above
			}
			val := formatSetValue(axis, v)
			if _, err := runWhatIfErr(t, "--demo", "steady", "--set", string(axis)+"="+val); err == nil {
				t.Errorf("--set %s=%s outside %v was accepted", axis, val, r)
			}
		}
	}
	// A caller mutating the returned map does not move the CLI's limits.
	stolen := whatif.HardBounds()
	stolen[whatif.AxisCPUHeadroom] = whatif.Range{Min: 0, Max: 100}
	if _, err := runWhatIfErr(t, "--demo", "steady", "--set", "cpu-headroom=3.0"); err == nil {
		t.Error("mutating the HardBounds copy widened the CLI's limits")
	}
}

// formatSetValue renders a --set value in the unit the axis takes.
func formatSetValue(a whatif.Axis, v float64) string {
	if a == whatif.AxisBaseSoak {
		return strings.TrimSuffix(strconvFormat(v), " ") + "h"
	}
	return strconvFormat(v)
}

func strconvFormat(v float64) string {
	return strings.TrimRight(strings.TrimRight(jsonNumber(v), "0"), ".")
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestWhatIfLiveHistoryRefusesRatherThanComparingTwoEmptyReplays.
//
// The comparison makes this refusal MORE important than `kilter backtest`'s,
// not less: two runs over an empty replay agree on every field, so the delta
// is all zeros and the gate's "no strict improvement" reads as a measurement
// that was taken and came back negative.
func TestWhatIfLiveHistoryRefusesRatherThanComparingTwoEmptyReplays(t *testing.T) {
	out, err := runWhatIfErr(t, "--cluster", "prod", "--set", "cpu-headroom=1.05")
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
	// No scorecard, no delta, no verdict.
	for _, forbidden := range []string{"regret", "Gate", "delta", "oracleGap"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%q was printed for a cluster with no history:\n%s", forbidden, out)
		}
	}
	// And it refuses rather than quietly preferring one source over the other.
	if _, err := runWhatIfErr(t, "--cluster", "prod", "--demo", "steady",
		"--set", "cpu-headroom=1.05"); !strings.Contains(err.Error(), "pass one") {
		t.Errorf("--cluster with --demo: err = %v", err)
	}
}

// TestAutoTuneApplyIsRefusedByName.
//
// pkg/whatif deferred auto-apply on principle, not for budget: apply needs a
// writer, and the writer is what breaks INV-4's single funnel. A CLI flag that
// quietly implemented it would be the same mistake one layer up, so the flag
// exists and refuses — and it refuses BEFORE any replay, so the output cannot
// read as though the request was honoured and merely printed.
func TestAutoTuneApplyIsRefusedByName(t *testing.T) {
	out, err := runWhatIfErr(t, "--demo", "bursty", "--set", "cpu-headroom=1.05",
		"--auto-tune=apply")
	msg := err.Error()
	for _, want := range []string{
		"--auto-tune=apply", "refused", "writer", "INV-4",
		"pkg/api", "CONFIGURED OPERATOR IDENTITY", "ledger",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(out, "regret") || strings.Contains(out, "Gate") {
		t.Errorf("a scorecard was printed for --auto-tune=apply:\n%s", out)
	}

	// propose is refused too — the nightly loop is brain wiring, not a verb.
	_, err = runWhatIfErr(t, "--demo", "bursty", "--set", "cpu-headroom=1.05",
		"--auto-tune=propose")
	for _, want := range []string{"NewTuner", "nightly", "historyEnd", "--propose"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the propose refusal does not mention %q:\n%s", want, err)
		}
	}

	// off is the default and is a no-op.
	if out := runWhatIfOK(t, append(append([]string{}, acceptedArgs...), "--auto-tune=off")...); !strings.Contains(out, "ACCEPTED") {
		t.Errorf("--auto-tune=off changed the answer:\n%s", out)
	}
	if _, err := runWhatIfErr(t, "--demo", "bursty", "--set", "cpu-headroom=1.05",
		"--auto-tune=sideways"); !strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("an unknown --auto-tune mode was not rejected: %v", err)
	}
}

// TestEnforceRefusalsIsTheYardstickNotThePolicy.
//
// The same field is POLICY for `kilter backtest` and YARDSTICK for
// `kilter whatif`: whatif.Scenario.EnforceDecisionRefusals is shared by both
// replays so the two sides cannot be scored under different rules. A policy
// file that set it would be silently ignored, which is a what-if of a policy
// nobody ran — so it is refused by name.
func TestEnforceRefusalsIsTheYardstickNotThePolicy(t *testing.T) {
	for _, flagName := range []string{"--policy", "--candidate"} {
		path := writePolicy(t, `{"enforceDecisionRefusals": true, "recommend": {"cpuHeadroom": 1.05}}`)
		_, err := runWhatIfErr(t, "--demo", "steady", flagName, path,
			"--set", "memory-headroom=1.30")
		if !strings.Contains(err.Error(), "not part of the policy in a what-if") {
			t.Errorf("%s: err = %q, want the yardstick refusal", flagName, err)
		}
		if !strings.Contains(err.Error(), "--enforce-refusals") {
			t.Errorf("%s: the refusal does not name the flag that works: %q", flagName, err)
		}
	}
	// The flag itself applies to BOTH replays: turning it on must not change
	// which policy each scorecard belongs to, and must move both sides.
	off := whatifResult(t, "--demo", "regime-change", "--workloads", "3", "--set", "cpu-headroom=1.30")
	on := whatifResult(t, "--demo", "regime-change", "--workloads", "3", "--set", "cpu-headroom=1.30",
		"--enforce-refusals")
	if off.BaselineScore.Policy != on.BaselineScore.Policy ||
		off.CandidateScore.Policy != on.CandidateScore.Policy {
		t.Error("--enforce-refusals changed a policy hash; it is a yardstick knob, not a policy knob")
	}
	if on.BaselineScore.CPUStarvation == off.BaselineScore.CPUStarvation &&
		on.CandidateScore.CPUStarvation == off.CandidateScore.CPUStarvation {
		t.Skip("the trace no longer starves; the A/B proves nothing")
	}
	if on.BaselineScore.CPUStarvation != 0 || on.CandidateScore.CPUStarvation != 0 {
		t.Errorf("--enforce-refusals reached only one side: starvation baseline %d candidate %d",
			on.BaselineScore.CPUStarvation, on.CandidateScore.CPUStarvation)
	}
}

// TestWhatIfWindowComesFromTheHistoryNotTheWallClock.
//
// whatif.Scenario takes no clock at all, so resolving the window is the CLI's
// whole job. A relative --from is measured back from the newest snapshot; a
// window past either end of the history is clamped and the clamp is REPORTED,
// because a run claiming 30 days over 7 days of history is a lie by omission.
func TestWhatIfWindowComesFromTheHistoryNotTheWallClock(t *testing.T) {
	full := whatifResult(t, acceptedArgs...)
	if got := full.Window[0].UTC().Format("2006-01-02T15:04:05Z"); got != "2026-01-05T00:00:00Z" {
		t.Errorf("default window starts at %s, want the trace epoch", got)
	}

	short := whatifResult(t, append(append([]string{}, acceptedArgs...), "--from", "3d")...)
	if !short.Window[0].After(full.Window[0]) {
		t.Errorf("--from 3d did not narrow the window: %v vs %v", short.Window[0], full.Window[0])
	}
	if short.BaselineScore.Instants >= full.BaselineScore.Instants {
		t.Errorf("--from 3d scored %d instants, not fewer than %d",
			short.BaselineScore.Instants, full.BaselineScore.Instants)
	}
	out := runWhatIfOK(t, append(append([]string{}, acceptedArgs...), "--from", "3d")...)
	if !strings.Contains(out, "newest snapshot") || !strings.Contains(out, "not the wall clock") {
		t.Errorf("the anchor was not reported:\n%s", out)
	}

	// A window wider than the history clamps, loudly, and scores the same as
	// the whole history rather than pretending to cover 30 days.
	wide := runWhatIfOK(t, append(append([]string{}, acceptedArgs...), "--from", "30d")...)
	if !strings.Contains(wide, "clamped to the start of the history") {
		t.Errorf("a 30-day window over a 7-day trace did not report a clamp:\n%s", wide)
	}
	wideR := whatifResult(t, append(append([]string{}, acceptedArgs...), "--from", "30d")...)
	if !wideR.Window[0].Equal(full.Window[0]) {
		t.Errorf("clamped window start %v, want %v", wideR.Window[0], full.Window[0])
	}

	// An inverted window is refused rather than replayed.
	if _, err := runWhatIfErr(t, "--demo", "bursty", "--set", "cpu-headroom=1.05",
		"--from", "2026-01-11T00:00:00Z", "--to", "2026-01-06T00:00:00Z"); !strings.Contains(
		err.Error(), "empty or inverted") {
		t.Errorf("an inverted window was accepted: %v", err)
	}
	if _, err := runWhatIfErr(t, "--demo", "bursty", "--set", "cpu-headroom=1.05",
		"--from", "yesterday"); !strings.Contains(err.Error(), "--from") {
		t.Errorf("a bad --from was not reported: %v", err)
	}
}

// TestWhatIfHumanOutputReportsTheNumbersItClaims.
//
// The rendered text is an independent restatement of the JSON, so it can drift
// from it. Every figure the human form prints is checked against the decoded
// result — this is the shape of bug PR#41 found in `kilter backtest`, where a
// percentage was scaled twice and printed 9,445 % for a real 94.5 %.
func TestWhatIfHumanOutputReportsTheNumbersItClaims(t *testing.T) {
	for _, args := range [][]string{acceptedArgs, rejectedArgs} {
		r := whatifResult(t, args...)
		out := runWhatIfOK(t, args...)

		for _, want := range []string{
			r.Cluster,
			r.BaselineScore.Policy,
			r.CandidateScore.Policy,
			signedUSD(r.Delta.RegretUSD),
			signedUSD(r.Delta.ResourceRegretUSD),
			signedUSD(r.Delta.RiskRegretUSD),
			signedUSD(r.Delta.ProjectedMonthlyUSD),
			signedInt(r.Delta.MemViolations),
			signedInt(r.Delta.CPUStarvation),
			signedFloat(r.Delta.OracleGapPct, 1),
			usd(r.Gate.RequiredRegretImprovementUSD),
			r.Window[0].UTC().Format("2006-01-02T15:04:05Z"),
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: output does not contain %q:\n%s", args, want, out)
			}
		}
		// The oracle gap is printed in the units the scorecard uses: it is
		// ALREADY scaled by 100 inside pkg/backtest.
		if !strings.Contains(out, "oracleGap") {
			t.Errorf("%v: no oracle gap in the output", args)
		}
		verdict := "REJECTED"
		if r.Gate.Passed {
			verdict = "ACCEPTED"
		}
		if !strings.Contains(out, verdict) {
			t.Errorf("%v: output does not say %s:\n%s", args, verdict, out)
		}
		for _, reason := range r.Gate.Reasons {
			if !strings.Contains(out, reason) {
				t.Errorf("%v: gate reason %q was not printed", args, reason)
			}
		}
		// Wins are printed even on a rejection: hiding them makes every
		// rejection look alike.
		for _, win := range r.Gate.Wins {
			if !strings.Contains(out, win) {
				t.Errorf("%v: win %q was not printed", args, win)
			}
		}
		// The projection is labelled as one, everywhere it is printed.
		if !strings.Contains(out, "projected") {
			t.Errorf("%v: the monthly figure is not labelled a projection", args)
		}
	}
}

// TestWhatIfSignsAreRenderedSoTheDirectionCannotBeMisread.
func TestWhatIfSignsAreRenderedSoTheDirectionCannotBeMisread(t *testing.T) {
	r := whatifResult(t, acceptedArgs...)
	if !(r.Delta.RegretUSD < 0) {
		t.Fatalf("the accepted case no longer improves regret (%v)", r.Delta.RegretUSD)
	}
	out := runWhatIfOK(t, acceptedArgs...)
	if !strings.Contains(out, "-$") {
		t.Errorf("an improvement was not rendered with a minus sign:\n%s", out)
	}
	rej := whatifResult(t, rejectedArgs...)
	if !(rej.Delta.RegretUSD > 0) {
		t.Fatalf("the rejected case no longer regresses regret (%v)", rej.Delta.RegretUSD)
	}
	if !strings.Contains(runWhatIfOK(t, rejectedArgs...), "+$") {
		t.Error("a regression was not rendered with a plus sign")
	}
}

// TestFailOnNoImprovementIsTheCIGate.
func TestFailOnNoImprovementIsTheCIGate(t *testing.T) {
	// The rejected candidate is reported, and without the flag the command
	// still succeeds: a what-if that answers "no" answered the question.
	out := runWhatIfOK(t, rejectedArgs...)
	if !strings.Contains(out, "REJECTED") {
		t.Fatalf("the gate accepted a regression:\n%s", out)
	}
	rejected := append(append([]string{}, rejectedArgs...), "--fail-on-no-improvement")
	out, err := runWhatIfErr(t, rejected...)
	if !strings.Contains(err.Error(), "did not pass the gate") {
		t.Errorf("err = %v, want the gate failure", err)
	}
	if !strings.Contains(out, "REJECTED") {
		t.Errorf("the reasons were not printed alongside the failure:\n%s", out)
	}
	// And it does not fire on an accepted candidate.
	runWhatIfOK(t, append(append([]string{}, acceptedArgs...), "--fail-on-no-improvement")...)
}

// TestWhatIfOutputIsByteIdenticalAcrossRuns. Go randomizes map iteration on
// every range, so repeating in ONE process is the real determinism test.
func TestWhatIfOutputIsByteIdenticalAcrossRuns(t *testing.T) {
	args := []string{"--demo", "bursty", "--noise", "0.05", "--workloads", "3",
		"--set", "cpu-headroom=1.05", "--set", "base-soak=8h"}
	base := runWhatIfOK(t, args...)
	baseJSON := runWhatIfOK(t, append(append([]string{}, args...), "--json")...)
	for i := 0; i < 6; i++ {
		if got := runWhatIfOK(t, args...); got != base {
			t.Fatalf("text run %d differs", i)
		}
		if got := runWhatIfOK(t, append(append([]string{}, args...), "--json")...); got != baseJSON {
			t.Fatalf("json run %d differs", i)
		}
	}
	// --set order must not change the answer: the same multiset of overrides
	// is the same candidate policy.
	swapped := runWhatIfOK(t, "--demo", "bursty", "--noise", "0.05", "--workloads", "3",
		"--set", "base-soak=8h", "--set", "cpu-headroom=1.05")
	if swapped == base {
		return // identical rendering including the "set:" echo line
	}
	// The echo line records the order the operator typed; everything computed
	// from it must not.
	stripSet := func(s string) string {
		var keep []string
		for _, line := range strings.Split(s, "\n") {
			if !strings.HasPrefix(line, "set: ") {
				keep = append(keep, line)
			}
		}
		return strings.Join(keep, "\n")
	}
	if stripSet(swapped) != stripSet(base) {
		t.Error("reordering --set changed the result")
	}
}

// TestWhatIfJSONGolden pins the byte-stable --json form.
//
// Result.Encode() is what `--json` writes verbatim and what a CI job diffs, so
// a change to it is a change to a published interface. Regenerate with:
//
//	go test ./cmd/kilter -run TestWhatIfJSONGolden -update-fixtures
func TestWhatIfJSONGolden(t *testing.T) {
	got := runWhatIfOK(t, "--demo", "bursty", "--set", "cpu-headroom=1.05", "--json")
	path := fixturePath("whatif-bursty.json")
	if *updateFixtures {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-fixtures to create it)", err)
	}
	if got != string(want) {
		t.Errorf("kilter whatif --json drifted from %s.\n"+
			"If this is intentional: go test ./cmd/kilter -run TestWhatIfJSONGolden -update-fixtures\n"+
			"got %d bytes, want %d", path, len(got), len(want))
	}
	// The golden is also a round-trip check: it must decode into a Result
	// whose gate verdict is the one the golden claims.
	var r whatif.Result
	if err := json.Unmarshal(want, &r); err != nil {
		t.Fatalf("the golden does not decode: %v", err)
	}
	if !r.Gate.Passed {
		t.Error("the golden pins a REJECTED comparison; pin an accepted one, " +
			"so a regression in the gate is visible here")
	}
}

// TestWhatIfPolicyFileFailsLoudly — the same contract `kilter backtest` has,
// through this command's loader.
func TestWhatIfPolicyFileFailsLoudly(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown field", `{"recommend": {"cpuHeadrooom": 1.5}}`, "unknown field"},
		{"bare duration", `{"decision": {"baseSoak": "3600"}}`, "durations need a unit"},
		{"not json", `{`, "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writePolicy(t, tc.body)
			out, err := runWhatIfErr(t, "--demo", "steady", "--candidate", path)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if strings.Contains(out, "regret") {
				t.Errorf("a comparison was printed for a broken policy file:\n%s", out)
			}
		})
	}
	// A candidate file and --set compose: the file is the base, --set moves it.
	path := writePolicy(t, `{"recommend": {"cpuHeadroom": 1.05}}`)
	r := whatifResult(t, "--demo", "steady", "--candidate", path, "--set", "memory-headroom=1.30")
	axes := map[whatif.Axis]float64{}
	for _, c := range r.Changes {
		axes[c.Axis] = c.To
	}
	if !near(axes[whatif.AxisCPUHeadroom], 1.05, 1e-9) {
		t.Errorf("the candidate file's cpu-headroom was lost: %+v", r.Changes)
	}
	if !near(axes[whatif.AxisMemoryHeadroom], 1.30, 1e-9) {
		t.Errorf("--set did not apply on top of the candidate file: %+v", r.Changes)
	}
}

// TestWhatIfNeedsACandidate: a what-if with nothing under test is not a
// question, and the usage text is printed rather than an empty comparison.
func TestWhatIfNeedsACandidate(t *testing.T) {
	out, err := runWhatIfErr(t, "--demo", "steady")
	if !strings.Contains(err.Error(), "needs a candidate") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out, "--set AXIS=VALUE") {
		t.Errorf("the usage text was not printed:\n%s", out)
	}
	if _, err := runWhatIfErr(t); !strings.Contains(err.Error(), "is required") {
		t.Errorf("whatif with no source of history: err = %v", err)
	}
}

// TestWhatIfHelpQuotesTheEnforcedBounds: the help text is generated from
// whatif.HardBounds(), in whatif.AllAxes order, so it is byte-stable and
// cannot drift from the values actually enforced.
func TestWhatIfHelpQuotesTheEnforcedBounds(t *testing.T) {
	first := whatifUsage()
	for i := 0; i < 5; i++ {
		if whatifUsage() != first {
			t.Fatalf("the usage text is not byte-stable (run %d)", i)
		}
	}
	last := -1
	for _, a := range whatif.AllAxes {
		i := strings.Index(first, "\n  "+string(a)+" ")
		if i < 0 {
			t.Fatalf("axis %s is missing from the help text", a)
		}
		if i < last {
			t.Errorf("axis %s is out of AllAxes order in the help text", a)
		}
		last = i
	}
	for axis, r := range whatif.HardBounds() {
		want := formatBound(r.Min) + ", " + formatBound(r.Max)
		if !strings.Contains(first, want) {
			t.Errorf("help text does not quote %s's bounds [%s]", axis, want)
		}
	}
}

func formatBound(v float64) string {
	return strings.TrimSuffix(jsonNumber(v), ".0")
}
