package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

// `kilter backtest` — the falsifiability harness, made runnable.
//
// pkg/backtest turns twelve domains' worth of self-assertion into a number,
// and on its first run it falsified the shipped engine. Until now nothing in
// the binary could call it.
//
// # Both sources of history are wired
//
// The TRACE path (--demo) replays four synthetic archetypes whose oracles are
// known in closed form, through the same observe-then-ask sequence pkg/api's
// Ingest/Plan runs. Policy files, the A/B comparison, Gate, the JSON scorecard
// and the CI exit code all work over it.
//
// The LIVE path (--cluster --db) replays a cluster's OWN retained history and
// was refused until pkg/store grew a time-keyed snapshot bucket. It is wired
// to api.Brain.Backtest and nothing about the refusal was deleted: it MOVED.
// Brain.Backtest still refuses, by type, in the two cases that matter —
// api.ErrNoHistory and api.ErrHistoryTooShort — and this command returns those
// errors unaltered, so they keep their non-zero exit and no scorecard is
// printed. The second case is the one a count check misses: backtest.Run over
// a history with no scoreable instant does not fail, it returns a Scorecard
// with the same shape, the same field names and the same confident tone as a
// real one, and `regret $0.00` over nothing reads as a perfect policy. That
// check lives in pkg/api because it is expressed in terms of backtest's own
// coverage report (Scorecard.Instants), not in terms of anything cmd can see.
//
// --from and --to are REQUIRED with --cluster, for the reason backtestEpoch is
// a constant for --demo and why-cost requires its window: a replay window that
// drifts with wall-clock time makes two runs over the same configuration
// disagree, and a scorecard whose whole value is comparability cannot be
// computed over a moving window.

const backtestUsage = `kilter backtest — replay a policy against history and score it

Usage:
  kilter backtest --demo <archetype> [flags]                 score the shipped policy over a synthetic trace
  kilter backtest --cluster <id> --db PATH --from T --to T   replay a cluster's own retained history

Archetypes (--demo), each with a closed-form oracle:
  steady          every sample at the base level
  diurnal         half the window at peak
  bursty          12 spikes/day — narrower than the 5%% CPU tail, wide enough for the memory peak
  regime-change   a level shift at the midpoint, on a decision instant

The two sources are different substrates for the same question and exactly one
may be given. --cluster reads the snapshot history a running brain persisted;
it is refused, by name and with a non-zero exit, when that history is absent or
too short to yield a single scoreable decision instant.

Flags:
  --demo KIND            synthetic archetype to score
  --cluster ID           replay this cluster's own retained history (needs --db)
  --db PATH              brain database written by "kilter brain --db PATH"
  --from RFC3339         replay window start, inclusive (required with --cluster)
  --to RFC3339           replay window end, EXCLUSIVE (required with --cluster)
  --days N               trace length in days (default 7, --demo only)
  --workloads N          containers in the trace (default 2, --demo only)
  --noise PCT            deterministic jitter, e.g. 0.05 for +/-5%% (default 0, --demo only)
  --horizon DUR          how far ahead each decision is scored (default 24h)
  --interval DUR         spacing of decision instants (default 24h)
  --starvation F         CPU violation threshold: future p95 > request x F (default 1.0)
  --incident-usd N       price of one violated container-window (default 50)
  --derive-costs         derive CPU/memory rates from the catalog and the trace's nodes (--demo only)
  --catalog PATH         pricing catalog JSON (default: embedded)
  --policy PATH          policy triple JSON; omitted means the shipped default (--demo only)
  --compare PATH         score a second policy and print Gate's verdict (--demo only)
  --enforce-refusals     run pkg/decision's refusal predicates (models the pending wiring);
                         a policy file's "enforceDecisionRefusals" overrides it per policy
                         (--demo only)
  --json                 emit the scorecard verbatim (byte-stable, CI-diffable)
  --fail-on-regression   exit non-zero when Gate rejects the candidate (--demo only)
`

func runBacktest(args []string) error { return runBacktestTo(os.Stdout, args) }

// backtestFlags is the command's input.
type backtestFlags struct {
	demo      string
	cluster   string
	db        string
	from      string
	to        string
	days      int
	workloads int
	noise     float64

	horizon     time.Duration
	interval    time.Duration
	starvation  float64
	incidentUSD float64
	deriveCosts bool
	catalog     string

	policy          string
	compare         string
	enforceRefusals bool
	jsonOut         bool
	failOnRegress   bool
}

// runBacktestTo is the testable entry point: everything printed goes to w.
func runBacktestTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ContinueOnError)
	fs.SetOutput(w)
	var bf backtestFlags
	fs.StringVar(&bf.demo, "demo", "", "synthetic archetype (steady|diurnal|bursty|regime-change)")
	fs.StringVar(&bf.cluster, "cluster", "", "replay a live cluster's own retained history")
	fs.StringVar(&bf.db, "db", "", "brain database holding the snapshot history")
	fs.StringVar(&bf.from, "from", "", "replay window start (RFC3339, required with --cluster)")
	fs.StringVar(&bf.to, "to", "", "replay window end (RFC3339, required with --cluster)")
	fs.IntVar(&bf.days, "days", 7, "trace length in days")
	fs.IntVar(&bf.workloads, "workloads", 2, "containers in the trace")
	fs.Float64Var(&bf.noise, "noise", 0, "deterministic jitter fraction")
	fs.DurationVar(&bf.horizon, "horizon", 24*time.Hour, "scoring horizon")
	fs.DurationVar(&bf.interval, "interval", 24*time.Hour, "decision interval")
	fs.Float64Var(&bf.starvation, "starvation", 0, "CPU starvation factor (0 = package default)")
	fs.Float64Var(&bf.incidentUSD, "incident-usd", 0, "price of one violated container-window")
	fs.BoolVar(&bf.deriveCosts, "derive-costs", false, "derive cost rates from the catalog")
	fs.StringVar(&bf.catalog, "catalog", "", "pricing catalog JSON")
	fs.StringVar(&bf.policy, "policy", "", "policy triple JSON")
	fs.StringVar(&bf.compare, "compare", "", "second policy triple JSON to compare against")
	fs.BoolVar(&bf.enforceRefusals, "enforce-refusals", false, "run pkg/decision's refusal predicates")
	fs.BoolVar(&bf.jsonOut, "json", false, "emit the scorecard as JSON")
	fs.BoolVar(&bf.failOnRegress, "fail-on-regression", false, "exit non-zero when Gate rejects")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case bf.cluster != "" && bf.demo != "":
		return fmt.Errorf("backtest: --demo and --cluster are different sources of history; pass one")
	case bf.cluster != "":
		return runBacktestLive(w, &bf, setFlagNames(fs))
	case bf.demo == "":
		fmt.Fprint(w, backtestUsage)
		return fmt.Errorf("backtest: --demo <archetype> or --cluster <id> is required")
	}
	return runBacktestDemo(w, &bf, setFlagNames(fs))
}

// runBacktestLive replays a cluster's own retained history through the brain
// that recorded it.
//
// Every refusal below is a flag this source cannot honour, refused by name
// rather than ignored — the same rule loadPolicy applies with
// DisallowUnknownFields, and for the same reason: a knob that is silently
// dropped produces a scorecard for a configuration nobody ran. What is NOT
// here is a check on the history itself. api.Brain.Backtest owns that, it
// refuses with typed errors, and this function returns them unaltered so the
// message the operator sees is the one pkg/api wrote and the exit code is
// non-zero.
func runBacktestLive(w io.Writer, bf *backtestFlags, set map[string]bool) error {
	// The trace-shaped flags describe a synthetic trace and mean nothing over
	// a recorded history.
	if err := refuseUnusable("backtest --cluster", "describe a synthetic trace, not a recorded history; "+
		"the history is whatever the brain retained", set,
		"days", "workloads", "noise"); err != nil {
		return err
	}
	// The policy flags would answer a different question. api.Brain.Backtest
	// scores THIS BRAIN'S policy — the recommender and planner the database's
	// brain actually runs with — so "how good is what is running here" is the
	// question a live replay answers. Scoring a policy that never ran against
	// a history it never saw is an A/B, and it needs a policy argument
	// pkg/api deliberately does not take; the seam if it is ever wanted is
	// api.BrainConfig's Recommend and Plan fields.
	if err := refuseUnusable("backtest --cluster", "belong to an A/B between two candidate policies; "+
		"a live replay scores the policy this brain runs, and --demo is where a policy comparison "+
		"belongs", set,
		"policy", "compare", "enforce-refusals", "fail-on-regression"); err != nil {
		return err
	}
	// --derive-costs reads the trace's own nodes. Over a live history the
	// equivalent is the retained snapshots, which is a cost model this
	// command does not build; refusing beats deriving a different one.
	if err := refuseUnusable("backtest --cluster", "derives a cost model from a synthetic trace's "+
		"nodes and has no defined meaning over a recorded history", set, "derive-costs"); err != nil {
		return err
	}
	if bf.db == "" {
		fmt.Fprint(w, backtestUsage)
		return fmt.Errorf("backtest --cluster %s: --db is required — the snapshot history lives in the "+
			"brain's database, and there is no history without one", bf.cluster)
	}
	if bf.from == "" || bf.to == "" {
		fmt.Fprint(w, backtestUsage)
		return fmt.Errorf("backtest --cluster %s: --from and --to are required; the replay window is an "+
			"argument, and a window that drifts with wall-clock time makes two runs over the same "+
			"configuration disagree", bf.cluster)
	}
	from, err := time.Parse(time.RFC3339, bf.from)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	to, err := time.Parse(time.RFC3339, bf.to)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}

	catalog, err := loadCatalog(bf.catalog)
	if err != nil {
		return err
	}
	src, err := openBrainSource("backtest", bf.db, catalog)
	if err != nil {
		return err
	}
	defer src.Close()
	brain, err := src.brain(api.BrainConfig{})
	if err != nil {
		return err
	}

	scoring := backtest.DefaultConfig()
	scoring.DecisionInterval = bf.interval
	if bf.starvation > 0 {
		scoring.StarvationFactor = bf.starvation
	}
	if bf.incidentUSD > 0 {
		scoring.Cost.IncidentUSD = bf.incidentUSD
	}

	// THE REFUSAL, reached from the command line. api.ErrNoHistory and
	// api.ErrHistoryTooShort come back verbatim: nothing is printed, the error
	// becomes a non-zero exit in main, and the operator is told how many
	// snapshots there were and how many instants they yielded rather than
	// being handed a scorecard whose zeros read as a verdict.
	sc, err := brain.Backtest(bf.cluster, from, to, bf.horizon, scoring)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("kilter backtest — cluster %s, replayed from %s\n"+
		"window %s .. %s  horizon %s  interval %s\n\n",
		bf.cluster, bf.db,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339),
		bf.horizon, scoring.DecisionInterval)
	return writeBacktestReport(w, bf, header, sc, nil, false, nil)
}

// runBacktestDemo scores one or two policies over a synthetic trace.
func runBacktestDemo(w io.Writer, bf *backtestFlags, set map[string]bool) error {
	// The live source's flags are refused here for the same reason the trace's
	// flags are refused there: the trace's window is backtestEpoch plus --days
	// by construction, so a --from that was quietly ignored would produce a
	// scorecard over a window the operator did not ask for.
	if err := refuseUnusable("backtest --demo", "belong to --cluster; a trace's window is "+
		"backtestEpoch plus --days and its history is generated, not stored", set,
		"db", "from", "to"); err != nil {
		return err
	}
	kind, err := parseArchetype(bf.demo)
	if err != nil {
		return err
	}
	// The trace's start is a FIXED instant, never time.Now(): the replay
	// window must come from the history, or two runs over the same
	// configuration disagree.
	spec := backtest.TraceSpec{
		Cluster:   "demo-" + string(kind),
		Kind:      kind,
		Start:     backtestEpoch,
		Days:      bf.days,
		Workloads: bf.workloads,
		NoisePct:  bf.noise,
	}
	trace, err := spec.Build()
	if err != nil {
		return fmt.Errorf("backtest --demo %s: %w", bf.demo, err)
	}
	store, err := trace.Store()
	if err != nil {
		return fmt.Errorf("backtest: build evidence store: %w", err)
	}

	scoring := backtest.DefaultConfig()
	scoring.DecisionInterval = bf.interval
	if bf.starvation > 0 {
		scoring.StarvationFactor = bf.starvation
	}
	if bf.incidentUSD > 0 {
		scoring.Cost.IncidentUSD = bf.incidentUSD
	}
	catalog, err := loadCatalog(bf.catalog)
	if err != nil {
		return err
	}
	if bf.deriveCosts {
		if len(trace.Snapshots) == 0 {
			return fmt.Errorf("backtest: --derive-costs needs at least one snapshot")
		}
		cm, err := backtest.CostModelFromCatalog(catalog, trace.Snapshots[len(trace.Snapshots)-1], scoring.Cost)
		if err != nil {
			return fmt.Errorf("backtest: --derive-costs: %w", err)
		}
		scoring.Cost = cm
	}

	base, err := loadPolicy(bf.policy, bf.enforceRefusals)
	if err != nil {
		return err
	}
	run := func(p policy) (*backtest.Scorecard, error) {
		h := &backtest.Harness{
			Evidence:                store,
			History:                 trace.Source(),
			Rec:                     p.Rec,
			Plan:                    p.Plan,
			Decision:                p.Decision,
			EnforceDecisionRefusals: p.EnforceRefusals,
			Catalog:                 catalog,
			Scoring:                 scoring,
		}
		return h.Run(trace.Cluster, trace.Start, trace.End, bf.horizon)
	}

	current, err := run(base)
	if err != nil {
		return fmt.Errorf("backtest: %w", err)
	}

	var candidate *backtest.Scorecard
	var gateOK bool
	var gateReasons []string
	if bf.compare != "" {
		alt, err := loadPolicy(bf.compare, bf.enforceRefusals)
		if err != nil {
			return err
		}
		candidate, err = run(alt)
		if err != nil {
			return fmt.Errorf("backtest --compare: %w", err)
		}
		gateOK, gateReasons = backtest.Gate(current, candidate, backtest.DefaultTolerance())
	}

	header := fmt.Sprintf("kilter backtest — %s trace, %d days, %d workloads\n"+
		"window %s .. %s  horizon %s  interval %s\n\n",
		kind, bf.days, bf.workloads,
		trace.Start.UTC().Format(time.RFC3339), trace.End.UTC().Format(time.RFC3339),
		bf.horizon, scoring.DecisionInterval)
	return writeBacktestReport(w, bf, header, current, candidate, gateOK, gateReasons)
}

// writeBacktestReport renders one or two scorecards, in whichever form --json
// asked for, then applies --fail-on-regression.
//
// Shared by both sources so a scorecard over a live history and a scorecard
// over a trace are the same bytes in the same layout — they are meant to be
// compared, and a second renderer is a second layout waiting to drift. Only
// the header line differs, and it names which source produced the numbers.
func writeBacktestReport(w io.Writer, bf *backtestFlags, header string,
	current, candidate *backtest.Scorecard, gateOK bool, gateReasons []string) error {
	if bf.jsonOut {
		if candidate == nil {
			// Verbatim: Scorecard.Encode is the byte-stable, CI-diffable form.
			raw, err := current.Encode()
			if err != nil {
				return err
			}
			_, err = w.Write(append(raw, '\n'))
			return failOnGate(err, bf, candidate, gateOK)
		}
		out := map[string]any{
			"current": current, "candidate": candidate,
			"gate": map[string]any{"accepted": gateOK, "reasons": gateReasons},
		}
		if err := writeJSON(w, out); err != nil {
			return err
		}
		return failOnGate(nil, bf, candidate, gateOK)
	}

	var b strings.Builder
	b.WriteString(header)
	writeScorecard(&b, "policy", current)
	if candidate != nil {
		b.WriteString("\n")
		writeScorecard(&b, "candidate", candidate)
		b.WriteString("\n  Gate\n")
		if gateOK {
			b.WriteString("    ACCEPTED: the candidate dominates on the terms §4.6 defines\n")
		} else {
			b.WriteString("    REJECTED\n")
		}
		for _, r := range gateReasons {
			fmt.Fprintf(&b, "      %s\n", r)
		}
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return failOnGate(nil, bf, candidate, gateOK)
}

// failOnGate turns a rejected comparison into the CI exit code §4.4 asks for.
func failOnGate(prior error, bf *backtestFlags, candidate *backtest.Scorecard, ok bool) error {
	if prior != nil {
		return prior
	}
	if bf.failOnRegress && candidate != nil && !ok {
		return fmt.Errorf("backtest: the candidate policy did not pass the gate")
	}
	return nil
}

// writeScorecard renders one scorecard. Every enumeration is sorted, so two
// runs over the same trace print the same bytes.
func writeScorecard(b *strings.Builder, label string, s *backtest.Scorecard) {
	fmt.Fprintf(b, "  %s %s\n", label, s.Policy)
	fmt.Fprintf(b, "    scored %d  decisions %d  refusals %d (good %d, idle %d)\n",
		s.Scored, s.Decisions, s.Scored-s.Decisions, s.RefusalsGood, s.RefusalsIdle)
	fmt.Fprintf(b, "    safety      memViolations %d  cpuStarvation %d  oomKills %d\n",
		s.MemViolations, s.CPUStarvation, s.MemOOMKills)
	// OracleGapPct is ALREADY scaled by 100 inside pkg/backtest (scorecard.go:
	// `meanSorted(gaps) * 100`), even though the doc comment states the
	// unscaled ratio. Multiplying again here printed a 9,445 % oracle gap for
	// the regime-change golden whose real value is 94.5.
	fmt.Fprintf(b, "    efficiency  oracleGap %.1f%%  (applied %.1f%%)  claimed/realized %.2f\n",
		s.OracleGapPct, s.OracleGapPctApplied, s.ClaimedVsRealized)
	fmt.Fprintf(b, "    stability   flipRate %.3f  flips %d\n", s.FlipRate, s.Flips)
	fmt.Fprintf(b, "    regret      $%.2f  (resource $%.2f + risk $%.2f)\n",
		s.RegretUSD, s.ResourceRegretUSD, s.RiskRegretUSD)
	fmt.Fprintf(b, "    forgone     $%.2f left on the table by idle refusals\n", s.ForgoneSavingsUSD)
	if len(s.Refusals) > 0 {
		codes := make([]string, 0, len(s.Refusals))
		for c := range s.Refusals {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		b.WriteString("    why it refused\n")
		for _, c := range codes {
			fmt.Fprintf(b, "      %-28s %d\n", c, s.Refusals[c])
		}
	}
}

// backtestEpoch is the trace's fixed start. It is a constant rather than a
// clock read because a replay window that drifts with wall-clock time makes
// two runs over the same configuration disagree — and the whole value of a
// scorecard is that it is comparable.
var backtestEpoch = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

func parseArchetype(s string) (backtest.TraceKind, error) {
	k := backtest.TraceKind(strings.TrimSpace(s))
	for _, known := range []backtest.TraceKind{
		backtest.TraceSteady, backtest.TraceDiurnal,
		backtest.TraceBursty, backtest.TraceRegimeChange,
	} {
		if k == known {
			return k, nil
		}
	}
	return "", fmt.Errorf("backtest --demo %q: unknown archetype (known: steady, diurnal, bursty, regime-change)", s)
}

// ---------------------------------------------------------------- policy file

// policy is the (recommend, plan, decision) triple under test.
type policy struct {
	Rec      recommend.Config
	Plan     plan.Config
	Decision decision.Config
	// EnforceRefusals runs pkg/decision's refusal predicates before a
	// recommendation is accepted. It is part of the POLICY rather than of the
	// scoring knobs because it is pending production wiring, not a yardstick:
	// pkg/decision shipped as unit 3 and pkg/recommend does not import it, so
	// today a refusal predicate cannot stop a recommendation from being
	// planned. Making it a policy field is what turns "should we wire the
	// decision layer in?" into an A/B through Gate instead of an opinion.
	EnforceRefusals bool
}

// policyFile is the on-disk shape of a policy triple.
//
// It is a cmd/-side projection rather than the three Config structs directly,
// and the mismatch that forced it is worth naming: none of recommend.Config,
// plan.Config or decision.Config carries json tags, so decoding into them
// gives a file whose keys are Go field names and whose durations are integer
// nanoseconds — and, worse, whose OMITTED fields decode as zero. A policy file
// where leaving out `cpuHeadroom` silently means "headroom 1.0" is a footgun
// with a business consequence. Every field here is therefore a POINTER,
// overlaid onto the package defaults, so absent means default and present
// means present.
type policyFile struct {
	Recommend *struct {
		CPUPercentile    *float64 `json:"cpuPercentile,omitempty"`
		CPUHeadroom      *float64 `json:"cpuHeadroom,omitempty"`
		MemoryPercentile *float64 `json:"memoryPercentile,omitempty"`
		MemoryHeadroom   *float64 `json:"memoryHeadroom,omitempty"`
		MinSamples       *int     `json:"minSamples,omitempty"`
		MinWindow        *string  `json:"minWindow,omitempty"`
		MinChangeRatio   *float64 `json:"minChangeRatio,omitempty"`
		SkipCPUForHPA    *bool    `json:"skipCPUForHPA,omitempty"`
	} `json:"recommend,omitempty"`
	Plan *struct {
		MinNodeUtilization   *float64 `json:"minNodeUtilization,omitempty"`
		MinConfidence        *float64 `json:"minConfidence,omitempty"`
		MaxNodeRemovals      *int     `json:"maxNodeRemovals,omitempty"`
		ApplyRecommendations *bool    `json:"applyRecommendations,omitempty"`
		MinClusterHeadroom   *float64 `json:"minClusterHeadroom,omitempty"`
		RespectManagedNodes  *bool    `json:"respectManagedNodes,omitempty"`
		DefaultMode          *string  `json:"defaultMode,omitempty"`
	} `json:"plan,omitempty"`
	Decision *struct {
		MinSamples            *int     `json:"minSamples,omitempty"`
		MinWindow             *string  `json:"minWindow,omitempty"`
		BaseSoak              *string  `json:"baseSoak,omitempty"`
		ClassFlipWindow       *string  `json:"classFlipWindow,omitempty"`
		MinClassStability     *float64 `json:"minClassStability,omitempty"`
		MaxHPAThrashPerHour   *float64 `json:"maxHPAThrashPerHour,omitempty"`
		MaxForecastDivergence *float64 `json:"maxForecastDivergence,omitempty"`
		ActConfidence         *float64 `json:"actConfidence,omitempty"`
	} `json:"decision,omitempty"`
	// EnforceDecisionRefusals models the pending pkg/decision wiring. See
	// policy.EnforceRefusals.
	EnforceDecisionRefusals *bool `json:"enforceDecisionRefusals,omitempty"`
}

// loadPolicy reads a policy triple, defaulting to the shipped policy.
func loadPolicy(path string, enforceDefault bool) (policy, error) {
	p := policy{
		Rec:      recommend.DefaultConfig(),
		Plan:     plan.DefaultConfig(),
		Decision: decision.DefaultConfig(),
	}
	p.EnforceRefusals = enforceDefault
	if path == "" || path == "default" {
		return p, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return policy{}, fmt.Errorf("--policy: %w", err)
	}
	var pf policyFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// Unknown fields are rejected: a knob misspelled in a policy file that is
	// silently ignored produces a scorecard for a policy nobody ran.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pf); err != nil {
		return policy{}, fmt.Errorf("%s: %w", path, err)
	}
	if r := pf.Recommend; r != nil {
		setF(&p.Rec.CPUPercentile, r.CPUPercentile)
		setF(&p.Rec.CPUHeadroom, r.CPUHeadroom)
		setF(&p.Rec.MemoryPercentile, r.MemoryPercentile)
		setF(&p.Rec.MemoryHeadroom, r.MemoryHeadroom)
		setI(&p.Rec.MinSamples, r.MinSamples)
		if err := setD(&p.Rec.MinWindow, r.MinWindow, path, "recommend.minWindow"); err != nil {
			return policy{}, err
		}
		setF(&p.Rec.MinChangeRatio, r.MinChangeRatio)
		setB(&p.Rec.SkipCPUForHPA, r.SkipCPUForHPA)
	}
	if pl := pf.Plan; pl != nil {
		setF(&p.Plan.MinNodeUtilization, pl.MinNodeUtilization)
		setF(&p.Plan.MinConfidence, pl.MinConfidence)
		setI(&p.Plan.MaxNodeRemovals, pl.MaxNodeRemovals)
		setB(&p.Plan.ApplyRecommendations, pl.ApplyRecommendations)
		setF(&p.Plan.MinClusterHeadroom, pl.MinClusterHeadroom)
		setB(&p.Plan.RespectManagedNodes, pl.RespectManagedNodes)
		setS(&p.Plan.DefaultMode, pl.DefaultMode)
	}
	if d := pf.Decision; d != nil {
		setI(&p.Decision.MinSamples, d.MinSamples)
		if err := setD(&p.Decision.MinWindow, d.MinWindow, path, "decision.minWindow"); err != nil {
			return policy{}, err
		}
		if err := setD(&p.Decision.BaseSoak, d.BaseSoak, path, "decision.baseSoak"); err != nil {
			return policy{}, err
		}
		if err := setD(&p.Decision.ClassFlipWindow, d.ClassFlipWindow, path, "decision.classFlipWindow"); err != nil {
			return policy{}, err
		}
		setF(&p.Decision.MinClassStability, d.MinClassStability)
		setF(&p.Decision.MaxHPAThrashPerHour, d.MaxHPAThrashPerHour)
		setF(&p.Decision.MaxForecastDivergence, d.MaxForecastDivergence)
		setF(&p.Decision.ActConfidence, d.ActConfidence)
	}
	setB(&p.EnforceRefusals, pf.EnforceDecisionRefusals)
	return p, nil
}

func setF(dst *float64, src *float64) {
	if src != nil {
		*dst = *src
	}
}
func setI(dst *int, src *int) {
	if src != nil {
		*dst = *src
	}
}
func setB(dst *bool, src *bool) {
	if src != nil {
		*dst = *src
	}
}
func setS(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

// setD parses a duration written the way a human writes one ("6h", "45m").
func setD(dst *time.Duration, src *string, path, field string) error {
	if src == nil {
		return nil
	}
	d, err := time.ParseDuration(*src)
	if err != nil {
		// A bare number is a common mistake and means nanoseconds in Go's
		// encoding, which is never what anybody meant.
		if _, numErr := strconv.Atoi(*src); numErr == nil {
			return fmt.Errorf("%s: %s = %q: durations need a unit (\"6h\", \"45m\")", path, field, *src)
		}
		return fmt.Errorf("%s: %s: %w", path, field, err)
	}
	*dst = d
	return nil
}
