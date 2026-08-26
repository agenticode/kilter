package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/whatif"
)

// `kilter whatif` — the counterfactual, made runnable.
//
// pkg/whatif shipped as reasoning-engine unit 5 with a complete CLI and API
// specification and zero reachability: nothing in the binary called it. This
// file is that specification implemented, not redesigned — the flags, the
// output shape and the exit codes come from pkg/whatif/FINDINGS.md, "CLI and
// API surface: what a later unit must do".
//
// # The three lines this command does not cross
//
// 1. THE DEMO PATH IS WIRED; THE LIVE PATH REFUSES. A what-if replays recorded
//    history TWICE, once per policy. pkg/store keeps only the LATEST snapshot
//    per cluster, so --cluster has nothing to replay and refuses by name
//    (whatifClusterRefusal). The failure mode is worse here than it is for
//    `kilter backtest`, because the output is a COMPARISON: two scorecards
//    over an empty replay agree on every field, so the delta is all zeros and
//    "no strict improvement" reads as a considered verdict rather than as
//    "nothing was replayed".
//
// 2. --auto-tune=apply IS NOT IMPLEMENTED, and says so by name. Auto-apply
//    needs a writer, and a writer outside INV-4's single funnel is the thing
//    the whole unit is built to prevent. See autoTuneRefusal.
//
// 3. THE EVALUATION PATH CANNOT BE POINTED AT THE POLICY UNDER TEST. There is
//    no flag that scores the candidate against itself: whatif.Scenario shares
//    every yardstick field between the two runs by construction, and a
//    candidate equal to the baseline is refused by Scenario.validate before
//    anything is replayed. --enforce-refusals is deliberately a SCENARIO knob
//    here (shared by both runs) even though `kilter backtest` treats the same
//    field as part of the policy — see loadWhatIfPolicy.

const whatifUsageHead = `kilter whatif — what would this policy have done instead?

Usage:
  kilter whatif --demo <archetype> --candidate <file.json> [flags]
  kilter whatif --demo <archetype> --set <axis>=<value> [flags]
  kilter whatif --cluster <id> ...                       (refused: see below)

Two replays of the same recorded history — one under the incumbent policy, one
under the candidate — scored by the same yardstick, differenced, and put
through §4.6's dominance gate. Nothing here computes a quality number of its
own: every figure is pkg/backtest's output or arithmetic over two of its
scorecards.

Archetypes (--demo), each with a closed-form oracle:
  steady          every sample at the base level
  diurnal         half the window at peak
  bursty          12 spikes/day
  regime-change   a level shift at the midpoint, on a decision instant

History flags:
  --demo KIND            synthetic archetype to replay
  --cluster ID           replay a live cluster's own history (refused: see below)
  --days N               trace length in days (default 7)
  --workloads N          containers in the trace (default 2)
  --noise PCT            deterministic jitter, e.g. 0.05 for +/-5% (default 0)
  --from 30d|RFC3339     window start; a relative value is measured back from the
                         NEWEST SNAPSHOT in the history, never from the wall clock
  --to RFC3339           window end, exclusive (default: the end of the history)

Yardstick flags (shared by BOTH runs — changing one changes neither policy):
  --horizon DUR          how far ahead each decision is scored (default 24h)
  --interval DUR         spacing of decision instants (default 24h)
  --starvation F         CPU violation threshold: future p95 > request x F
  --incident-usd N       price of one violated container-window (default 50)
  --derive-costs         derive CPU/memory rates from the catalog and the nodes
  --catalog PATH         pricing catalog JSON (default: embedded)
  --enforce-refusals     run pkg/decision's refusal predicates in both replays

Policy flags:
  --policy default|PATH  the incumbent (default: the shipped policy)
  --candidate PATH       the policy under test
  --set AXIS=VALUE       move one axis of the candidate (repeatable)

Output flags:
  --json                 emit whatif.Result verbatim (byte-stable, CI-diffable)
  --fail-on-no-improvement   exit non-zero when the gate rejects the candidate
  --propose              file the result as a proposal (needs --store PATH)
  --store PATH           the proposal store file (see kilter proposals)
  --rationale TEXT       why this change is being proposed
  --author-id ID         the identity to file under (never a human: see below)
  --now RFC3339          audit timestamp for --propose (default: the wall clock).
                         It never touches the replay window, which comes from
                         the history.
  --auto-tune off        off|propose|apply — propose and apply are refused by
                         name; see the output of --auto-tune=apply

--set axes and the HARD BOUNDS no config, tuner, agent or API caller may widen
(whatif.HardBounds()):
`

// whatifUsage renders the usage text with the hard bounds appended. The bounds
// are read from whatif.HardBounds() rather than restated, so help text cannot
// drift from the values actually enforced; they are enumerated in
// whatif.AllAxes order, so the text is byte-stable.
func whatifUsage() string {
	var b strings.Builder
	b.WriteString(whatifUsageHead)
	hard := whatif.HardBounds()
	for _, a := range whatif.AllAxes {
		r := hard[a]
		unit := ""
		if a == whatif.AxisBaseSoak {
			unit = " hours, written with a unit: 8h, 90m"
		}
		fmt.Fprintf(&b, "  %-20s [%g, %g]%s\n", a, r.Min, r.Max, unit)
	}
	b.WriteString(`
The declared search space (--set is checked against the HARD bounds; the gate
additionally checks the candidate against whatif.DefaultEnvelope, which is
narrower on every axis).
`)
	return b.String()
}

func runWhatIf(args []string) error { return runWhatIfTo(os.Stdout, args) }

// whatifFlags is the command's input.
type whatifFlags struct {
	demo      string
	cluster   string
	days      int
	workloads int
	noise     float64
	from      string
	to        string

	horizon     time.Duration
	interval    time.Duration
	starvation  float64
	incidentUSD float64
	deriveCosts bool
	catalog     string

	policy    string
	candidate string
	sets      repeatedFlag

	enforceRefusals bool
	jsonOut         bool
	failOnNoImprove bool

	propose   bool
	store     string
	rationale string
	authorID  string
	now       string

	autoTune string
}

// runWhatIfTo is the testable entry point: everything printed goes to w.
func runWhatIfTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("whatif", flag.ContinueOnError)
	fs.SetOutput(w)
	var wf whatifFlags
	fs.StringVar(&wf.demo, "demo", "", "synthetic archetype (steady|diurnal|bursty|regime-change)")
	fs.StringVar(&wf.cluster, "cluster", "", "replay a live cluster's own history")
	fs.IntVar(&wf.days, "days", 7, "trace length in days")
	fs.IntVar(&wf.workloads, "workloads", 2, "containers in the trace")
	fs.Float64Var(&wf.noise, "noise", 0, "deterministic jitter fraction")
	fs.StringVar(&wf.from, "from", "", "window start (30d or RFC3339)")
	fs.StringVar(&wf.to, "to", "", "window end, exclusive (RFC3339)")
	fs.DurationVar(&wf.horizon, "horizon", 24*time.Hour, "scoring horizon")
	fs.DurationVar(&wf.interval, "interval", 24*time.Hour, "decision interval")
	fs.Float64Var(&wf.starvation, "starvation", 0, "CPU starvation factor (0 = package default)")
	fs.Float64Var(&wf.incidentUSD, "incident-usd", 0, "price of one violated container-window")
	fs.BoolVar(&wf.deriveCosts, "derive-costs", false, "derive cost rates from the catalog")
	fs.StringVar(&wf.catalog, "catalog", "", "pricing catalog JSON")
	fs.StringVar(&wf.policy, "policy", "", "incumbent policy triple JSON")
	fs.StringVar(&wf.candidate, "candidate", "", "candidate policy triple JSON")
	fs.Var(&wf.sets, "set", "move one candidate axis, AXIS=VALUE (repeatable)")
	fs.BoolVar(&wf.enforceRefusals, "enforce-refusals", false, "run pkg/decision's refusal predicates in both replays")
	fs.BoolVar(&wf.jsonOut, "json", false, "emit whatif.Result as JSON")
	fs.BoolVar(&wf.failOnNoImprove, "fail-on-no-improvement", false, "exit non-zero when the gate rejects")
	fs.BoolVar(&wf.propose, "propose", false, "file the result as a proposal")
	fs.StringVar(&wf.store, "store", "", "proposal store file")
	fs.StringVar(&wf.rationale, "rationale", "", "why this change is being proposed")
	fs.StringVar(&wf.authorID, "author-id", "", "the identity to file the proposal under")
	fs.StringVar(&wf.now, "now", "", "audit timestamp as RFC3339 (default: now)")
	fs.StringVar(&wf.autoTune, "auto-tune", "off", "off|propose|apply (propose and apply are refused)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The auto-tune refusal is checked FIRST, before any replay: a user who
	// asked for auto-apply must not receive a scorecard that reads as though
	// the request was honoured and merely printed instead of applied.
	if err := autoTuneRefusal(wf.autoTune); err != nil {
		return err
	}

	switch {
	case wf.cluster != "" && wf.demo != "":
		return fmt.Errorf("whatif: --demo and --cluster are different sources of history; pass one")
	case wf.cluster != "":
		return whatifClusterRefusal(wf.cluster)
	case wf.demo == "":
		fmt.Fprint(w, whatifUsage())
		return fmt.Errorf("whatif: --demo <archetype> or --cluster <id> is required")
	}
	if wf.candidate == "" && len(wf.sets) == 0 {
		fmt.Fprint(w, whatifUsage())
		return fmt.Errorf("whatif: a what-if needs a candidate: --candidate <file.json> or --set <axis>=<value>")
	}
	if wf.propose && wf.store == "" {
		return whatifProposeRefusal()
	}
	return runWhatIfDemo(w, &wf)
}

// whatifClusterRefusal is the honest half of this command.
//
// It names the seam, says what it would take, and exits non-zero. It does NOT
// fall back to the one snapshot pkg/store holds — see the comment on
// backtestLiveRefusal for why a scorecard over an empty replay is the worst
// available outcome, and note that a COMPARISON over an empty replay is worse
// still: a delta of all zeros and a verdict of "no strict improvement" read as
// a measurement that was taken and came back negative.
func whatifClusterRefusal(cluster string) error {
	return fmt.Errorf(`whatif --cluster %s: refused — snapshot history is not persisted.

A what-if replays recorded history TWICE, once per policy. pkg/store keeps only
the LATEST snapshot per cluster (SaveSnapshot/LoadSnapshot are keyed by cluster,
not by time), so there is no history to replay and both runs would score a
single instant.

Printing that comparison would be worse than printing a single bad scorecard:
two runs over an empty replay agree on every field, so every delta is 0.00,
"regret $0.00" reads as "perfect" and the gate's "regret improves by $0.0000,
short of the $0.0100 margin" reads as "we measured, and the candidate is not
better" — when nothing was measured at all.

What it needs (pkg/whatif/FINDINGS.md, "The snapshot-history seam";
pkg/backtest/FINDINGS.md, "Seams this unit needed and did not find", item 1): a
time-keyed snapshot bucket in pkg/store — SaveSnapshotAt(snap) and
Snapshots(cluster, from, to) — plus an adapter implementing
backtest.SnapshotSource, which is the type whatif.Scenario.History already
takes. At a 5-minute cadence a 30-day window is 8,640 snapshots per cluster, so
the realistic shape is keyframe-plus-delta or a reduced replay snapshot.

Meanwhile: kilter whatif --demo regime-change --set cpu-headroom=1.30`, cluster)
}

// autoTuneRefusal implements §4.6's mode flag as far as this unit may, which
// is: not at all, by name, with the reason.
//
// The flag exists rather than being silently absent because "flag provided but
// not defined" teaches a reader nothing, and the two refusals below are the
// whole point of the unit. `off` is the default and is a no-op here — the
// nightly loop is a brain, not a CLI.
func autoTuneRefusal(mode string) error {
	switch strings.TrimSpace(mode) {
	case "", "off":
		return nil
	case "propose":
		return fmt.Errorf(`whatif --auto-tune=propose: refused — the nightly tuner is brain wiring, not a CLI verb.

§4.6's loop is constructed at brain start (whatif.NewTuner, unconditionally, so
a disabled-but-invalid config is a startup error rather than a 3am surprise) and
Run(basePolicy, scenario, historyEnd, clock) fires on the nightly timer, where
historyEnd is the newest snapshot's timestamp — once per cluster. That is
pkg/api's wiring and it is not built; it also needs the same snapshot-history
seam --cluster refuses on. See cmd/WHATIF-WIRING-FINDINGS.md.

To ask the same question once, by hand:
  kilter whatif --demo <archetype> --set <axis>=<value> --propose --store PATH`)
	case "apply":
		return fmt.Errorf(`whatif --auto-tune=apply: refused, by name, and not for want of budget.

§4.6 item 4 offers auto-apply for a proposal that dominates on every metric
inside hard bounds. Auto-apply needs a WRITER, and a writer is exactly what
breaks INV-4's single funnel: gated → approved by a human who is not the author
→ applied. pkg/whatif has no writer on principle ("if a future change adds a
writer here, it has broken the unit"); putting one behind a CLI flag would be
the same mistake one layer up, where it would also be outside the audit trail
the funnel exists to produce.

When it is built it belongs in pkg/api, as a caller that
  (a) reads a gated proposal,
  (b) mints an approval as a CONFIGURED OPERATOR IDENTITY distinct from the
      tuner — configured, so that a human decided in advance and in writing,
  (c) writes the config,
  (d) posts applied, with the §4.6 ledger entry in the same breath.
Every step is already representable in pkg/whatif. None of it may live here.`)
	default:
		return fmt.Errorf("whatif --auto-tune %q: unknown mode (off|propose|apply)", mode)
	}
}

// whatifProposeRefusal explains why --propose needs a named file.
func whatifProposeRefusal() error {
	return fmt.Errorf(`whatif --propose: refused — --store PATH is required.

whatif.Store.Create is what runs the gate and mints the proposal ID, so a
proposal that is not stored is a receipt for a document that does not exist:
` + "`kilter proposals show <id>`" + ` would not find it. pkg/whatif owns no file by
design — Snapshot() and Load() move bytes and cmd/ decides where they live — so
the file has to be named:

  kilter whatif --demo bursty --set cpu-headroom=1.30 --propose --store ./proposals.json

That file is a LOCAL artifact. The fleet's proposals belong in pkg/store's
bbolt file under a new ` + "`proposals`" + ` bucket (pkg/whatif/FINDINGS.md, "Brain
wiring"), which is pkg/api's to add and is not wired yet.`)
}

// runWhatIfDemo replays a synthetic trace under two policies.
func runWhatIfDemo(w io.Writer, wf *whatifFlags) error {
	kind, err := parseArchetype(wf.demo)
	if err != nil {
		// The archetype set is shared with `kilter backtest`; the flag name in
		// its message is not, so only that is restated.
		return fmt.Errorf("whatif %s", strings.TrimPrefix(err.Error(), "backtest "))
	}
	// The trace's start is the same FIXED instant `kilter backtest` uses. A
	// replay window that drifts with the wall clock makes two runs over one
	// configuration disagree, and here it would additionally make a proposal's
	// fingerprint — which covers the window — unreproducible.
	spec := backtest.TraceSpec{
		Cluster:   "demo-" + string(kind),
		Kind:      kind,
		Start:     backtestEpoch,
		Days:      wf.days,
		Workloads: wf.workloads,
		NoisePct:  wf.noise,
	}
	trace, err := spec.Build()
	if err != nil {
		return fmt.Errorf("whatif --demo %s: %w", wf.demo, err)
	}
	store, err := trace.Store()
	if err != nil {
		return fmt.Errorf("whatif: build evidence store: %w", err)
	}

	from, to, notes, err := resolveWhatIfWindow(wf, trace)
	if err != nil {
		return err
	}

	scoring := backtest.DefaultConfig()
	scoring.DecisionInterval = wf.interval
	if wf.starvation > 0 {
		scoring.StarvationFactor = wf.starvation
	}
	if wf.incidentUSD > 0 {
		scoring.Cost.IncidentUSD = wf.incidentUSD
	}
	catalog, err := loadCatalog(wf.catalog)
	if err != nil {
		return err
	}
	if wf.deriveCosts {
		if len(trace.Snapshots) == 0 {
			return fmt.Errorf("whatif: --derive-costs needs at least one snapshot")
		}
		cm, err := backtest.CostModelFromCatalog(catalog, trace.Snapshots[len(trace.Snapshots)-1], scoring.Cost)
		if err != nil {
			return fmt.Errorf("whatif: --derive-costs: %w", err)
		}
		scoring.Cost = cm
	}

	baseline, err := loadWhatIfPolicy(wf.policy, "--policy")
	if err != nil {
		return err
	}
	candidate := baseline
	if wf.candidate != "" {
		candidate, err = loadWhatIfPolicy(wf.candidate, "--candidate")
		if err != nil {
			return err
		}
	}
	candidate, applied, err := applyAxisSets(candidate, wf.sets)
	if err != nil {
		return err
	}

	scen := whatif.Scenario{
		Cluster:   trace.Cluster,
		From:      from,
		To:        to,
		Horizon:   wf.horizon,
		Baseline:  baseline,
		Candidate: candidate,
		History:   trace.Source(),
		Evidence:  store,
		Catalog:   catalog,
		Scoring:   scoring,
		// Shared by both runs: this is a property of the yardstick, not of
		// the policy under test. See loadWhatIfPolicy.
		EnforceDecisionRefusals: wf.enforceRefusals,
	}
	// Returned unwrapped: pkg/whatif already prefixes its errors with
	// "whatif:", and a second prefix would say it twice.
	result, err := scen.Run()
	if err != nil {
		return err
	}

	if wf.jsonOut {
		// Verbatim: Result.Encode is the byte-stable, CI-diffable form, and
		// is what a golden file pins.
		raw, encErr := result.Encode()
		if encErr != nil {
			return encErr
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
	} else {
		if err := writeWhatIf(w, wf, kind, trace, notes, applied, result); err != nil {
			return err
		}
	}

	if wf.propose {
		if err := proposeWhatIf(w, wf, result); err != nil {
			return err
		}
	}
	if wf.failOnNoImprove && !result.Improved() {
		return fmt.Errorf("whatif: the candidate policy did not pass the gate")
	}
	return nil
}

// resolveWhatIfWindow turns --from/--to into concrete instants.
//
// The anchor is the HISTORY, never time.Now(): `--from 30d` is measured back
// from the newest snapshot in the replayed history, which is the rule
// pkg/backtest's FINDINGS states and pkg/whatif's repeats. whatif.Scenario
// takes no clock at all, so resolving this window is the CLI's whole job here
// — and getting it from a wall clock would make two runs over identical data
// disagree, and a proposal's fingerprint (which covers the window) change
// every night.
//
// A window that reaches past either end of the recorded history is CLAMPED and
// the clamp is reported, for the same reason `kilter domains --rds-fixture`
// reports its window clamp: a run that claims a 30-day window over 7 days of
// history is a lie told by omission.
func resolveWhatIfWindow(wf *whatifFlags, trace *backtest.Trace) (time.Time, time.Time, []string, error) {
	newest := trace.Start
	for _, s := range trace.Snapshots {
		if s.Timestamp.After(newest) {
			newest = s.Timestamp
		}
	}
	var notes []string

	to := trace.End
	if wf.to != "" {
		t, err := time.Parse(time.RFC3339, wf.to)
		if err != nil {
			return time.Time{}, time.Time{}, nil, fmt.Errorf("whatif --to: %w (RFC3339)", err)
		}
		to = t.UTC()
	}

	from := trace.Start
	if wf.from != "" {
		if d, ok, err := parseRelativeWindow(wf.from); err != nil {
			return time.Time{}, time.Time{}, nil, err
		} else if ok {
			// Relative to the newest SNAPSHOT, per the spec — not to `to`,
			// which the caller may have set past the end of the history.
			from = newest.Add(-d)
			notes = append(notes, fmt.Sprintf(
				"--from %s resolved against the newest snapshot (%s), not the wall clock",
				wf.from, newest.UTC().Format(time.RFC3339)))
		} else {
			t, err := time.Parse(time.RFC3339, wf.from)
			if err != nil {
				return time.Time{}, time.Time{}, nil, fmt.Errorf(
					"whatif --from: %w (RFC3339, or a relative span like 30d)", err)
			}
			from = t.UTC()
		}
	}

	if from.Before(trace.Start) {
		notes = append(notes, fmt.Sprintf("window start clamped to the start of the history (%s)",
			trace.Start.UTC().Format(time.RFC3339)))
		from = trace.Start
	}
	if to.After(trace.End) {
		notes = append(notes, fmt.Sprintf("window end clamped to the end of the history (%s)",
			trace.End.UTC().Format(time.RFC3339)))
		to = trace.End
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, nil, fmt.Errorf(
			"whatif: replay window [%s,%s) is empty or inverted",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	return from.UTC(), to.UTC(), notes, nil
}

// parseRelativeWindow reads the `30d` / `72h` form the spec writes for --from.
// Days are spelled out because time.ParseDuration has no day unit, and an
// operator who writes 30d means thirty times twenty-four hours.
func parseRelativeWindow(s string) (time.Duration, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, false, fmt.Errorf("whatif --from %q: %w", s, err)
		}
		if !(n > 0) {
			return 0, false, fmt.Errorf("whatif --from %q: a relative window must be positive", s)
		}
		return time.Duration(n * float64(24*time.Hour)), true, nil
	}
	// An RFC3339 timestamp starts with a four-digit year, so it can never be
	// mistaken for a duration; anything ParseDuration accepts is relative.
	if d, err := time.ParseDuration(s); err == nil {
		if !(d > 0) {
			return 0, false, fmt.Errorf("whatif --from %q: a relative window must be positive", s)
		}
		return d, true, nil
	}
	return 0, false, nil
}

// ---------------------------------------------------------------- the policies

// loadWhatIfPolicy reads a policy triple into whatif.Policy.
//
// It reuses `kilter backtest`'s loader — same file format, same
// pointer-overlay semantics, same rejection of unknown fields — and then
// refuses one field of it, which is the one mismatch between the two commands
// and is worth stating in full:
//
// `enforceDecisionRefusals` is part of the POLICY for `kilter backtest`
// (cmd/kilter/backtest.go's `policy` struct), because there the question is
// "should we wire pkg/decision in?" and an A/B through Gate is the honest way
// to ask it. In a what-if it is part of the YARDSTICK:
// whatif.Scenario.EnforceDecisionRefusals is a scenario field shared by both
// replays, precisely so the two sides cannot be scored under different rules.
// A policy file that set it would therefore be silently ignored, and a what-if
// of a policy nobody ran is exactly the artefact this whole unit exists to
// prevent. So it is refused by name, with the flag that does work.
func loadWhatIfPolicy(path, flagName string) (whatif.Policy, error) {
	if path == "" || path == "default" {
		return whatif.DefaultPolicy(), nil
	}
	// Two loads with opposite defaults: if the file PINNED the field, both
	// agree; if it left the field out, they differ. Cheaper and less brittle
	// than a second parser for one boolean.
	withFalse, err := loadPolicy(path, false)
	if err != nil {
		return whatif.Policy{}, fmt.Errorf("%s: %w", flagName, err)
	}
	withTrue, err := loadPolicy(path, true)
	if err != nil {
		return whatif.Policy{}, fmt.Errorf("%s: %w", flagName, err)
	}
	if withFalse.EnforceRefusals == withTrue.EnforceRefusals {
		return whatif.Policy{}, fmt.Errorf(
			`%s %s: "enforceDecisionRefusals" is not part of the policy in a what-if.

whatif.Scenario shares it between BOTH replays (it is a scenario field, not a
policy field), so the two sides are scored under the same rules; a policy file
that set it here would be silently ignored and the answer would describe a
policy nobody ran. Use --enforce-refusals, which applies to both runs, or
`+"`kilter backtest --compare`"+`, where it IS the policy under test.`, flagName, path)
	}
	return whatif.Policy{
		Rec:      withFalse.Rec,
		Plan:     withFalse.Plan,
		Decision: withFalse.Decision,
	}, nil
}

// applyAxisSets applies the repeatable --set overrides to the candidate.
//
// The axis names are exactly whatif.AllAxes and an unknown one is REJECTED
// rather than ignored: silently dropping a knob the caller asked to tune
// produces a what-if whose rationale does not describe what was measured.
// Values are checked against whatif.HardBounds() here — the absolute limits no
// caller may widen — and the gate independently re-checks the candidate
// against the declared envelope, which is narrower. Producer and checker are
// different code on purpose.
//
// whatif.Axis.get/set are unexported, so the five-field projection is restated
// here. TestSetMovesTheAxisPkgWhatifThinksItMoves cross-checks every axis
// against pkg/whatif's own projection (Result.Changes), so a mis-mapped field
// fails the build's tests rather than quietly tuning the wrong knob.
func applyAxisSets(p whatif.Policy, sets []string) (whatif.Policy, []string, error) {
	hard := whatif.HardBounds()
	var applied []string
	for _, raw := range sets {
		name, value, ok := strings.Cut(raw, "=")
		if !ok {
			return whatif.Policy{}, nil, fmt.Errorf("whatif --set %q: expected AXIS=VALUE", raw)
		}
		axis := whatif.Axis(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if !axis.Known() {
			return whatif.Policy{}, nil, fmt.Errorf("whatif --set %q: unknown axis (known: %s)",
				name, strings.Join(axisNames(), ", "))
		}
		var num float64
		if axis == whatif.AxisBaseSoak {
			d, err := time.ParseDuration(value)
			if err != nil {
				// A bare number means nanoseconds in Go's encoding, which is
				// never what anybody meant — same refusal backtest's setD makes.
				if _, numErr := strconv.Atoi(value); numErr == nil {
					return whatif.Policy{}, nil, fmt.Errorf(
						"whatif --set %s=%q: durations need a unit (\"8h\", \"90m\")", axis, value)
				}
				return whatif.Policy{}, nil, fmt.Errorf("whatif --set %s: %w", axis, err)
			}
			num = d.Hours()
			p.Decision.BaseSoak = d
		} else {
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return whatif.Policy{}, nil, fmt.Errorf("whatif --set %s=%q: %w", axis, value, err)
			}
			num = f
			switch axis {
			case whatif.AxisCPUPercentile:
				p.Rec.CPUPercentile = f
			case whatif.AxisMemoryPercentile:
				p.Rec.MemoryPercentile = f
			case whatif.AxisCPUHeadroom:
				p.Rec.CPUHeadroom = f
			case whatif.AxisMemoryHeadroom:
				p.Rec.MemoryHeadroom = f
			}
		}
		if r, ok := hard[axis]; ok && !r.Contains(num) {
			return whatif.Policy{}, nil, fmt.Errorf(
				"whatif --set %s=%s: outside the hard bounds [%g,%g], which no config file, "+
					"tuner, agent or API caller may widen (whatif.HardBounds())",
				axis, value, r.Min, r.Max)
		}
		applied = append(applied, string(axis)+"="+value)
	}
	return p, applied, nil
}

// axisNames renders whatif.AllAxes for an error message, in the package's own
// fixed order.
func axisNames() []string {
	out := make([]string, 0, len(whatif.AllAxes))
	for _, a := range whatif.AllAxes {
		out = append(out, string(a))
	}
	return out
}

// ---------------------------------------------------------------- rendering

// writeWhatIf renders the human form. Every enumeration is sorted or fixed, so
// two runs over the same trace print the same bytes.
func writeWhatIf(w io.Writer, wf *whatifFlags, kind backtest.TraceKind, trace *backtest.Trace,
	notes, applied []string, r *whatif.Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "kilter whatif — %s, %s trace, %d days, %d workloads\n",
		r.Cluster, kind, wf.days, wf.workloads)
	fmt.Fprintf(&b, "window %s .. %s  horizon %s  interval %s\n",
		r.Window[0].UTC().Format(time.RFC3339), r.Window[1].UTC().Format(time.RFC3339),
		wf.horizon, wf.interval)
	for _, n := range notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	if len(applied) > 0 {
		fmt.Fprintf(&b, "set: %s\n", strings.Join(applied, " "))
	}
	b.WriteString("\n  what changed\n")
	if len(r.Changes) == 0 {
		b.WriteString("    (no declared axis moved; the policies differ elsewhere)\n")
	}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "    %s\n", c.Text)
	}
	b.WriteString("\n")
	writeScorecard(&b, "baseline", r.BaselineScore)
	b.WriteString("\n")
	writeScorecard(&b, "candidate", r.CandidateScore)
	b.WriteString("\n")
	writeDelta(&b, r.Delta)
	b.WriteString("\n  Gate\n")
	if r.Gate.Passed {
		b.WriteString("    ACCEPTED: the candidate dominates on the terms §4.6 defines\n")
	} else {
		b.WriteString("    REJECTED\n")
	}
	for _, reason := range r.Gate.Reasons {
		fmt.Fprintf(&b, "      %s\n", reason)
	}
	for _, win := range r.Gate.Wins {
		fmt.Fprintf(&b, "      win: %s\n", win)
	}
	fmt.Fprintf(&b, "    required regret improvement %s\n",
		usd(r.Gate.RequiredRegretImprovementUSD))
	_, err := io.WriteString(w, b.String())
	return err
}

// writeDelta renders candidate-minus-baseline. Every field is signed
// explicitly, because the whole value of a delta is that the reader can tell
// which direction it moved without consulting the two scorecards.
func writeDelta(b *strings.Builder, d whatif.Delta) {
	b.WriteString("  delta (candidate − baseline; negative is better everywhere except decisions)\n")
	fmt.Fprintf(b, "    safety      memViolations %s  cpuStarvation %s\n",
		signedInt(d.MemViolations), signedInt(d.CPUStarvation))
	fmt.Fprintf(b, "    regret      %s  (%s)  resource %s  risk %s\n",
		signedUSD(d.RegretUSD), signedPct(d.RegretPct),
		signedUSD(d.ResourceRegretUSD), signedUSD(d.RiskRegretUSD))
	fmt.Fprintf(b, "    efficiency  oracleGap %s pts  (applied %s pts)  forgone %s\n",
		signedFloat(d.OracleGapPct, 1), signedFloat(d.OracleGapPctApplied, 1),
		signedUSD(d.ForgoneSavingsUSD))
	fmt.Fprintf(b, "    behaviour   decisions %s  refusals %s (idle %s)  flipRate %s\n",
		signedInt(d.Decisions), signedInt(d.Refusals), signedInt(d.RefusalsIdle),
		signedFloat(d.FlipRate, 3))
	// Labelled a projection everywhere it is printed: it assumes next month
	// resembles the window that was replayed, which is exactly the assumption
	// a backtest cannot verify.
	fmt.Fprintf(b, "    projected   %s/month, extrapolated from %.1fh of history\n",
		signedUSD(d.ProjectedMonthlyUSD), d.WindowHours)
}

func signedInt(v int) string {
	if v > 0 {
		return "+" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

func signedUSD(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("+$%.2f", v)
}

func signedPct(v float64) string {
	return fmt.Sprintf("%+.1f%%", v)
}

func signedFloat(v float64, digits int) string {
	return fmt.Sprintf("%+.*f", digits, v)
}

// ---------------------------------------------------------------- --propose

// proposeWhatIf files the result as a proposal.
//
// Three properties are load-bearing and each is a deliberate choice rather
// than a default:
//
//  1. THE VERDICT IS NOT CARRIED ACROSS. Result.Spec deliberately omits the
//     GateResult, and Store.Create runs Decide itself from the scorecards. A
//     caller — this one included — hands over evidence and receives a
//     judgment; it cannot hand over a judgment.
//
//  2. THE AUTHOR IS NEVER A HUMAN. pkg/whatif's FINDINGS says the CLI actor is
//     {human, <authenticated identity>} — and a local CLI process has no
//     authenticated identity to offer. $USER, os/user and the uid are
//     properties of the SESSION, which anything running in it inherits,
//     including unit 8's reasoner. So the author is filed as
//     Actor{Kind: ActorSystem, ID: <--author-id, default "kilter-cli">}. The
//     ID matters: whatif.sameIdentity compares IDs and IGNORES Kind, so an
//     operator who names themselves here is BLOCKED from later approving this
//     proposal through the authenticated funnel. That direction is the safe
//     one — naming yourself can only ever remove a capability — which is why
//     it is a flag while the approver identity is not.
//
//  3. THE CLOCK IS AN ARGUMENT. --now feeds CreatedAt and the audit trail and
//     nothing else; the replay window comes from the history. CreatedAt is
//     excluded from the fingerprint, so the proposal ID is the same whether it
//     was filed today or in a month.
func proposeWhatIf(w io.Writer, wf *whatifFlags, r *whatif.Result) error {
	now, err := parseNowFlag(wf.now)
	if err != nil {
		return err
	}
	author := whatif.Actor{Kind: whatif.ActorSystem, ID: "kilter-cli"}
	if id := strings.TrimSpace(wf.authorID); id != "" {
		author.ID = id
	}
	spec, err := r.Spec(whatif.Target{Cluster: r.Cluster},
		whatif.DefaultEnvelope(), whatif.DefaultTolerance(), wf.rationale, nil)
	if err != nil {
		return fmt.Errorf("whatif --propose: %w", err)
	}
	store, err := openProposalStore(wf.store)
	if err != nil {
		return err
	}
	rec, err := store.Create(author, spec, whatif.FixedClock(now))
	if err != nil {
		return fmt.Errorf("whatif --propose: %w", err)
	}
	if err := saveProposalStore(wf.store, store); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nfiled proposal %s (%s) by %s in %s\n",
		rec.ID(), rec.State(), author, wf.store)
	if rec.State() == whatif.StateRejected {
		fmt.Fprintf(w, "  the gate rejected it; it is filed anyway, because a rejected proposal is\n"+
			"  the record of a question that was asked and answered\n")
	}
	fmt.Fprintf(w, "  kilter proposals show %s --store %s\n", rec.ID(), wf.store)
	return nil
}

// parseNowFlag resolves --now. The wall clock is read here, at the edge of the
// program, and passed inward as a whatif.Clock — pkg/whatif never calls
// time.Now itself, and a caller that forgets a clock gets an error rather than
// a silently unreproducible proposal.
func parseNowFlag(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--now: %w (RFC3339)", err)
	}
	return t.UTC(), nil
}
