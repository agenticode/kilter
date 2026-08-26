package whatif

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/pricing"
)

// Scenario is a counterfactual: "what would have happened over this stretch of
// recorded history if the policy had been Candidate instead of Baseline?"
//
// §4.5 specifies the implementation in one sentence — "the backtest harness
// with a config override; zero new decision logic" — and that is exactly what
// this is. Everything except the policy triple is shared between the two runs
// by construction (see harness), so the comparison cannot be rigged by
// accidentally scoring the two sides under different yardsticks.
type Scenario struct {
	// Cluster names the history to replay. Required.
	Cluster string
	// From and To bound the replay window, half-open. Required, and
	// explicitly NOT defaulted from a clock: the window must come from the
	// stored history, or two runs over identical data disagree.
	From, To time.Time
	// Horizon is how far past each decision instant the scorer looks.
	// Default 24h, matching the container-day the oracle is defined on.
	Horizon time.Duration

	// Baseline is the incumbent policy; zero means the shipped default.
	Baseline Policy
	// Candidate is the policy under test. Required, and must differ from
	// Baseline — a what-if against itself is not a question.
	Candidate Policy

	// History is the snapshot stream. Required.
	History backtest.SnapshotSource
	// Evidence is the substrate adverse and change events are read from.
	// Optional: a nil store scores purely from usage.
	Evidence evidence.Store
	// Catalog prices snapshots. Nil uses the embedded baseline catalog.
	Catalog *pricing.Catalog
	// Scoring holds the measurement knobs — how the harness measures, not
	// what it measures. Shared by both runs.
	Scoring backtest.Config
	// EnforceDecisionRefusals runs pkg/decision's refusal predicates inside
	// the replay. Shared by both runs, so it is part of the yardstick and
	// not part of the policy under test.
	EnforceDecisionRefusals bool

	// Envelope and Tolerance parameterize the gate applied to the result.
	// Zero values take DefaultEnvelope and DefaultTolerance.
	Envelope  Envelope
	Tolerance Tolerance
}

// defaultHorizon matches pkg/backtest's scoring unit.
const defaultHorizon = 24 * time.Hour

// Result is a what-if answer: both scorecards, the arithmetic between them,
// and the gate's verdict on whether the candidate is worth proposing.
type Result struct {
	Cluster      string       `json:"cluster"`
	Window       [2]time.Time `json:"window"`
	HorizonHours float64      `json:"horizonHours"`

	// Baseline and Candidate are the two policies, carried whole so a result
	// is self-contained: everything needed to file it as a proposal, or to
	// re-run it, is in the object.
	Baseline  Policy `json:"baselinePolicy"`
	Candidate Policy `json:"candidatePolicy"`
	// Changes lists the axes that moved, in AllAxes order.
	Changes []Change `json:"changes"`
	// BaselineScore and CandidateScore are pkg/backtest's own output,
	// verbatim. They are carried whole rather than summarized, because the
	// summary is a convenience and the scorecard is the evidence.
	BaselineScore  *backtest.Scorecard `json:"baseline"`
	CandidateScore *backtest.Scorecard `json:"candidate"`
	Delta          Delta               `json:"delta"`
	Gate           GateResult          `json:"gate"`
}

// Encode renders the result as indented JSON with a trailing newline: the
// `kilter whatif --json` and golden-file form. Byte-identical for identical
// inputs, because every component already is.
func (r *Result) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Improved reports whether the candidate cleared the gate.
func (r *Result) Improved() bool { return r != nil && r.Gate.Passed }

// harness builds the replay for one policy.
//
// This function is the independence guarantee, and it is written as one struct
// literal on purpose. Every field except the policy triple is copied from the
// Scenario, so the two runs differ in exactly the three configs that ARE the
// policy under test, and in nothing else. There is no code path where the
// candidate is scored against a different history, a different evidence store,
// a different price list, a different horizon or a different starvation
// factor. TestBothSidesAreScoredByTheSameYardstick asserts it by reflection,
// so a field added here without being shared fails the tests.
func (s Scenario) harness(p Policy) *backtest.Harness {
	return &backtest.Harness{
		Evidence:                s.Evidence,
		History:                 s.History,
		Rec:                     p.Rec,
		Plan:                    p.Plan,
		Decision:                p.Decision,
		EnforceDecisionRefusals: s.EnforceDecisionRefusals,
		Catalog:                 s.Catalog,
		Scoring:                 s.Scoring,
	}
}

// validate rejects a scenario that could not produce a meaningful answer, and
// returns it with defaults applied.
func (s Scenario) validate() (Scenario, error) {
	if s.Cluster == "" {
		return Scenario{}, errors.New("whatif: scenario must name a cluster")
	}
	if s.History == nil {
		return Scenario{}, errors.New("whatif: scenario has no snapshot history to replay")
	}
	s.From, s.To = s.From.UTC(), s.To.UTC()
	if s.From.IsZero() || s.To.IsZero() {
		return Scenario{}, errors.New("whatif: replay window needs explicit bounds from the stored history")
	}
	if !s.To.After(s.From) {
		return Scenario{}, fmt.Errorf("whatif: replay window [%s,%s) is empty or inverted",
			s.From.Format(time.RFC3339), s.To.Format(time.RFC3339))
	}
	if s.Horizon == 0 {
		s.Horizon = defaultHorizon
	}
	s.Baseline = s.Baseline.withDefaults()
	s.Candidate = s.Candidate.withDefaults()
	if err := s.Baseline.Validate(); err != nil {
		return Scenario{}, fmt.Errorf("whatif: baseline: %w", err)
	}
	if err := s.Candidate.Validate(); err != nil {
		return Scenario{}, fmt.Errorf("whatif: candidate: %w", err)
	}
	if s.Baseline.Hash() == s.Candidate.Hash() {
		return Scenario{}, errors.New("whatif: candidate is identical to the baseline; there is nothing to compare")
	}
	if s.Envelope.Bounds == nil {
		s.Envelope = DefaultEnvelope()
	}
	s.Tolerance = s.Tolerance.withDefaults()
	return s, nil
}

// Run replays the history twice — once per policy — and reports the delta.
//
// Both replays go through backtest.Harness.Run, which is the same code path
// `kilter backtest` and the brain use. No scoring, no oracle and no cost model
// is reimplemented here: a policy that looks good in a what-if is good by the
// same yardstick the product already publishes, and a bug in that yardstick
// shows up in both places at once rather than in whichever one the operator
// happened not to be looking at.
func (s Scenario) Run() (*Result, error) {
	s, err := s.validate()
	if err != nil {
		return nil, err
	}

	baseScore, err := s.harness(s.Baseline).Run(s.Cluster, s.From, s.To, s.Horizon)
	if err != nil {
		return nil, fmt.Errorf("whatif: replaying the baseline: %w", err)
	}
	candScore, err := s.harness(s.Candidate).Run(s.Cluster, s.From, s.To, s.Horizon)
	if err != nil {
		return nil, fmt.Errorf("whatif: replaying the candidate: %w", err)
	}

	return &Result{
		Cluster:        s.Cluster,
		Window:         baseScore.Window,
		HorizonHours:   baseScore.HorizonHours,
		Baseline:       s.Baseline,
		Candidate:      s.Candidate,
		Changes:        changesBetween(s.Baseline, s.Candidate),
		BaselineScore:  baseScore,
		CandidateScore: candScore,
		Delta:          Diff(baseScore, candScore),
		Gate: Decide(GateInput{
			Baseline:       s.Baseline,
			Candidate:      s.Candidate,
			BaselineScore:  baseScore,
			CandidateScore: candScore,
			Envelope:       s.Envelope,
			Tolerance:      s.Tolerance,
		}),
	}, nil
}

// Spec turns a result into something Store.Create can file.
//
// The gate verdict is deliberately NOT carried across. Store.Create runs
// Decide itself, from the scorecards, so a caller cannot file a proposal
// carrying a verdict it computed somewhere else under a tolerance of its own
// choosing. Handing over the evidence rather than the conclusion is what makes
// that enforceable.
func (r *Result) Spec(target Target, envelope Envelope, tol Tolerance, rationale string, evidenceIDs []string) (Spec, error) {
	if r == nil {
		return Spec{}, errors.New("whatif: no result to propose")
	}
	if r.BaselineScore == nil || r.CandidateScore == nil {
		return Spec{}, errors.New("whatif: result carries no scorecards")
	}
	if target.Cluster == "" {
		target.Cluster = r.Cluster
	}
	return Spec{
		Kind:           KindPolicyChange,
		Target:         target,
		Baseline:       r.Baseline,
		Candidate:      r.Candidate,
		BaselineScore:  r.BaselineScore,
		CandidateScore: r.CandidateScore,
		Envelope:       envelope,
		Tolerance:      tol,
		Rationale:      rationale,
		EvidenceIDs:    evidenceIDs,
	}, nil
}
