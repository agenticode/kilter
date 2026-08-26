package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/whatif"
)

// The what-if plane, served.
//
//	GET  /api/v1/clusters/{id}/whatif?from=&to=&horizon=&set=&enforceRefusals=
//	GET  /api/v1/proposals?cluster=&state=
//	GET  /api/v1/proposals/{id}
//	POST /api/v1/proposals                     (authWrite)
//	POST /api/v1/proposals/{id}/rejections     (authWrite)
//	POST /api/v1/proposals/{id}/approvals      (authWrite) — refused, by name
//	POST /api/v1/proposals/{id}/applied        (authWrite) — refused, by name
//
// # A simulation and a measurement must not look alike on the wire
//
// This is the failure this file exists to prevent, and it is not a
// documentation problem. A what-if answer carries two backtest.Scorecards:
// the same field names, the same units and the same confident tone as the
// scorecard `kilter backtest` prints over a cluster's real history. Served
// bare, "regret $2.90" from a policy that never ran is indistinguishable from
// "regret $2.90" measured under the policy that did.
//
// So the counterfactual is not served bare. Every what-if answer is wrapped in
// Simulated, whose JSON puts the whole whatif.Result UNDER a "simulation" key
// alongside "observed": false and "applied": false. The distinction is
// structural in both directions:
//
//   - ON THE WIRE, a client that drops the envelope does not get a
//     nice-looking measurement, it gets nothing — there is no scorecard, no
//     regret and no delta at the top level to mis-read.
//   - IN GO, Simulated has no exported field and no exported constructor, so
//     no caller inside or outside this package can hand back a counterfactual
//     with "observed" set to true. It is the same discipline whatif.Approval
//     uses: not a rule that gets checked, a value that cannot be built.
//
// The proposal routes serve whatif.Record through the package's own
// MarshalJSON, verbatim (cmd/WHATIF-WIRING-FINDINGS.md §7.1: lift the
// rendering, do not re-derive it). A Record is an observation OF A DOCUMENT —
// this proposal exists, in this state, with this audit trail — and every
// number inside it already sits under `proposal.gate` / `proposal.delta`,
// which no measurement ever produces.
//
// # Three status codes, because three failures are different facts
//
// The convention explainroutes.go established, extended by one:
//
//   - THE CLUSTER WAS NEVER INGESTED → 404.
//   - THE SUBSTRATE CANNOT SUPPORT AN ANSWER → 422. No store, too few
//     retained snapshots, or a window in which no decision instant can be
//     scored. This is the case a never-populated brain hits and it must read
//     as "there is not enough evidence here", NOT as a fault — and
//     emphatically not as a 200 whose deltas are all 0.00, which is the same
//     empty replay wearing a verdict's clothes.
//   - A DEFECT IN THIS PROCESS → 500. The retained history would not read
//     back, or the harness failed on inputs this file already validated.
//     That is the one case worth paging on.
//
// Plus the two the request itself can be wrong in: 400 for a malformed or
// unbounded question, and 409 when a proposal's state machine refuses a
// transition that was legal to ask for.
//
// # Nothing here applies anything
//
// No route in this file mutates cloud state, and none accepts a proposal for
// execution. Nothing this file imports, transitively, reaches pkg/ec2 or
// pkg/rds: the actuators are deliberately unreachable and making them
// reachable is a separate decision. pkg/api as a whole reaches pkg/rds only
// through the ledger's pkg/actuate import (pkg/actuate → pkg/provider →
// pkg/rds), which predates this plane and is pinned to that one route; no file
// here imports an actuator directly, so an Actuator cannot even be named in
// this package. This package also contains no call to whatif.NewApprover,
// Store.Approve or Store.MarkApplied. All of it is asserted rather than
// asserted-in-a-comment: see TestTheAPIPackageCannotReachAnActuator and
// TestNoCodePathInThisPackageApprovesOrApplies.

// defaultWhatIfHorizon matches pkg/backtest's scoring unit and pkg/whatif's
// own default: the container-day the oracle is defined on.
const defaultWhatIfHorizon = 24 * time.Hour

// maxWhatIfRationale bounds the one free-text field a caller supplies.
// pkg/whatif enforces the same ceiling on its way in (its maxRationale is
// unexported); checking here as well is what turns an oversized rationale
// into a 400 naming the limit instead of a generic filing failure.
const maxWhatIfRationale = 4096

// whatIfAuthor is the identity every proposal filed through this API is
// attributed to.
//
// It names the FUNNEL, not the caller, and that is deliberate. The write
// token is held by every controller and agent that ingests a snapshot, so the
// brain cannot tell one write-token holder from another; minting a specific
// identity from an indistinguishable one would put a name in an audit trail
// that nothing backs. It is ActorSystem for the reason cmd/kilter files as
// system:kilter-cli — pkg/whatif's FINDINGS is explicit that an automated
// caller must never be given a synthesized human identity, and no actor kind
// but ActorHuman can ever approve.
var whatIfAuthor = whatif.Actor{Kind: whatif.ActorSystem, ID: "kilter-brain-api"}

// ---------------------------------------------------------------- surface

// whatIfSurface owns the proposal store the routes read and write.
//
// It is created inside registerWhatIfRoutes and captured by the handlers, so
// its lifetime is the handler's: Brain.Serve builds one Handler and the store
// lives as long as the server does. The store is IN MEMORY. §7.2's bbolt
// `proposals` bucket is pkg/store's to add and pkg/store is not this unit's;
// WHATIFROUTES-FINDINGS.md §6 records what that costs and what it does not.
type whatIfSurface struct {
	b     *Brain
	props *whatif.Store
	// now is the clock, read at the HTTP edge and passed inward as a
	// whatif.Clock. pkg/whatif never calls time.Now itself and errors rather
	// than defaulting, which is what keeps every computed number in this
	// plane a function of its inputs.
	now whatif.Clock
}

func newWhatIfSurface(b *Brain) *whatIfSurface {
	return &whatIfSurface{
		b:     b,
		props: whatif.NewStore(),
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// registerWhatIfRoutes adds the what-if and proposal endpoints.
func (b *Brain) registerWhatIfRoutes(mux *http.ServeMux) {
	newWhatIfSurface(b).register(mux)
}

func (s *whatIfSurface) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/clusters/{id}/whatif", s.b.auth(s.handleWhatIf))
	mux.HandleFunc("GET /api/v1/proposals", s.b.auth(s.handleListProposals))
	mux.HandleFunc("GET /api/v1/proposals/{id}", s.b.auth(s.handleGetProposal))
	mux.HandleFunc("POST /api/v1/proposals", s.b.authWrite(s.handleCreateProposal))
	mux.HandleFunc("POST /api/v1/proposals/{id}/rejections", s.b.authWrite(s.handleRejectProposal))
	// Registered rather than absent, for the reason `kilter whatif
	// --auto-tune=apply` is a flag that refuses rather than an unknown flag:
	// a 404 from the mux teaches a caller nothing, and the reason is the
	// answer here.
	mux.HandleFunc("POST /api/v1/proposals/{id}/approvals", s.b.authWrite(s.handleApprovalRefused))
	mux.HandleFunc("POST /api/v1/proposals/{id}/applied", s.b.authWrite(s.handleAppliedRefused))
}

// ---------------------------------------------------------------- the answer

// SimulationBasis says what was replayed to produce a counterfactual, so a
// reader can tell how much history is behind it without decoding two
// scorecards. Policies appear as their backtest.PolicyHash, which is the same
// string Scorecard.Policy carries.
type SimulationBasis struct {
	Cluster               string       `json:"cluster"`
	Window                [2]time.Time `json:"window"`
	HorizonHours          float64      `json:"horizonHours"`
	DecisionIntervalHours float64      `json:"decisionIntervalHours"`
	SnapshotsReplayed     int          `json:"snapshotsReplayed"`
	InstantsScored        int          `json:"instantsScored"`
	IncumbentPolicy       string       `json:"incumbentPolicy"`
	CandidatePolicy       string       `json:"candidatePolicy"`
	// EnforceDecisionRefusals is part of the yardstick, not of the policy
	// under test: it applies to BOTH replays or to neither.
	EnforceDecisionRefusals bool `json:"enforceDecisionRefusals"`
}

// simulationStatement is emitted with every counterfactual. It is on the wire
// rather than in this comment because the reader who most needs it is a
// client that never opens this file.
const simulationStatement = "counterfactual: recorded history replayed under a policy that was never in force. " +
	"Nothing here was measured on the fleet and nothing here has been applied."

// Simulated is the envelope every counterfactual answer is served in.
//
// Every field is unexported and there is no exported constructor, so the
// marker cannot be built with the wrong value: within Go, "observed" is not a
// field a caller can set, and on the wire the result is only reachable under
// the "simulation" key. Dropping the envelope loses the answer, which is the
// only failure mode safer than mis-reading it.
type Simulated struct {
	result *whatif.Result
	basis  SimulationBasis
}

// Result returns the counterfactual this envelope carries.
func (s Simulated) Result() *whatif.Result { return s.result }

// Basis returns what was replayed to produce it.
func (s Simulated) Basis() SimulationBasis { return s.basis }

// MarshalJSON writes the envelope. observed and applied are constants, not
// fields: there is no code path, here or in a caller, that can emit a
// counterfactual claiming to be either.
func (s Simulated) MarshalJSON() ([]byte, error) {
	if s.result == nil {
		return nil, errors.New("api: simulation envelope carries no result")
	}
	return json.Marshal(struct {
		Kind       string          `json:"kind"`
		Observed   bool            `json:"observed"`
		Applied    bool            `json:"applied"`
		Statement  string          `json:"statement"`
		Basis      SimulationBasis `json:"basis"`
		Simulation *whatif.Result  `json:"simulation"`
	}{
		Kind:       "simulation",
		Observed:   false,
		Applied:    false,
		Statement:  simulationStatement,
		Basis:      s.basis,
		Simulation: s.result,
	})
}

func newSimulated(res *whatif.Result, req WhatIfRequest) Simulated {
	sim := Simulated{result: res}
	if res == nil {
		return sim
	}
	sim.basis = SimulationBasis{
		Cluster:                 res.Cluster,
		Window:                  res.Window,
		HorizonHours:            res.HorizonHours,
		IncumbentPolicy:         res.Baseline.Hash(),
		CandidatePolicy:         res.Candidate.Hash(),
		EnforceDecisionRefusals: req.EnforceDecisionRefusals,
	}
	if sc := res.BaselineScore; sc != nil {
		sim.basis.DecisionIntervalHours = sc.DecisionIntervalHours
		sim.basis.SnapshotsReplayed = sc.Snapshots
		sim.basis.InstantsScored = sc.Instants
	}
	return sim
}

// ---------------------------------------------------------------- WhatIf

// WhatIfRequest is the question a counterfactual answers. It is a struct
// rather than an argument list because the routes, cmd/ and pkg/mcp all ask
// the same question and a positional sixth argument is where they would start
// to disagree.
type WhatIfRequest struct {
	Cluster  string
	From, To time.Time
	Horizon  time.Duration
	// Candidate is the policy under test. It must differ from the incumbent —
	// a what-if against itself is not a question.
	Candidate whatif.Policy
	// EnforceDecisionRefusals runs pkg/decision's predicates inside BOTH
	// replays. It is the yardstick, not the policy: pkg/whatif shares it
	// across the two sides by construction so they cannot be scored under
	// different rules.
	EnforceDecisionRefusals bool
}

// IncumbentPolicy is the policy this brain actually runs: the recommender and
// planner configs it was started with, plus pkg/decision's shipped defaults.
//
// It is the baseline every counterfactual is measured against, for the reason
// Brain.Backtest scores this brain's own config: the question an operator is
// asking is "would this be better than what we are running", not "would this
// be better than the shipped default".
//
// The decision triple is the default rather than a configured one because
// nothing in production consults pkg/decision today (pkg/recommend does not
// import it — cmd/WIRING-FINDINGS.md §6.4). It reaches a replay only when
// EnforceDecisionRefusals is set, which is why moving the soak axis without
// it is refused rather than answered.
func (b *Brain) IncumbentPolicy() whatif.Policy {
	return whatif.Policy{
		Rec:      b.cfg.Recommend,
		Plan:     b.cfg.Plan,
		Decision: decision.DefaultConfig(),
	}
}

// WhatIf replays a cluster's own retained history twice — once under this
// brain's policy, once under the candidate — and reports the delta and the
// gate's verdict.
//
// It is exported for the reason WhyCost and Explain are: the counterfactual,
// not the HTTP framing, is the product, and a second caller must not have to
// remember which refusals are load-bearing.
//
// It refuses rather than returning a comparison of two empty replays. That
// refusal matters MORE here than it does for Backtest, not less: a backtest
// over an empty history prints one suspicious scorecard, while a what-if
// prints a comparison in which every delta is 0.00, the regret change is
// $0.0000, and the gate reports "short of the required margin" — a considered
// -sounding negative verdict about a measurement that never happened.
func (b *Brain) WhatIf(req WhatIfRequest) (*whatif.Result, error) {
	cluster := strings.TrimSpace(req.Cluster)
	if cluster == "" {
		return nil, badRequest{errors.New("a what-if needs a cluster")}
	}
	// 404 before 422: "that cluster was never ingested" and "that cluster has
	// too little history" are different facts and an operator acts on them
	// differently.
	if b.snapshotFor(cluster) == nil {
		return nil, notIngested{fmt.Errorf("unknown cluster %q", cluster)}
	}
	if b.st == nil {
		return nil, notEnoughEvidence{ErrNoHistory{Cluster: cluster}}
	}
	if err := checkExplainWindow(req.From, req.To); err != nil {
		return nil, err
	}
	horizon := req.Horizon
	if horizon == 0 {
		horizon = defaultWhatIfHorizon
	}
	if horizon <= 0 || horizon > maxExplainWindow {
		return nil, badRequest{fmt.Errorf("horizon %v out of (0,%v]", horizon, maxExplainWindow)}
	}
	// backtest refuses a horizon wider than the window it is asked to replay.
	// Checking it here keeps that a 400 — the caller asked for a scoring
	// window that does not fit inside the replay window — rather than a
	// replay failure this file would have to call a defect.
	if span := req.To.Sub(req.From); horizon > span {
		return nil, badRequest{fmt.Errorf(
			"horizon %v exceeds the %v window: every decision instant needs a full horizon of history after it",
			horizon, span)}
	}
	incumbent := b.IncumbentPolicy()
	if err := req.Candidate.Validate(); err != nil {
		return nil, badRequest{err}
	}
	if req.Candidate.Hash() == incumbent.Hash() {
		return nil, badRequest{errors.New(
			"the candidate is identical to the policy this brain runs; a what-if against itself is not a question")}
	}

	// The count check comes from the retained rows, before anything is
	// replayed, so "there is no history" is answered as such rather than as a
	// scorecard full of zeros.
	snaps, err := b.st.Snapshots(cluster, req.From, req.To)
	if err != nil {
		// The window bound was checked above and the cluster id came from the
		// same brain that ingested it, so a failure here is the retained
		// history not reading back: a defect, not a question anyone asked
		// wrongly.
		return nil, defect{fmt.Errorf("reading the retained history: %w", err)}
	}
	retained := len(snaps)
	// Dropped before the replays rather than left in scope for the compiler
	// to decide about: each replay re-reads and re-decodes the window itself,
	// and this slice is the same rows a third time. On the largest retained
	// history that is hundreds of megabytes of decoded snapshot held for no
	// reason.
	snaps = nil
	if retained < MinReplaySnapshots {
		return nil, notEnoughEvidence{ErrHistoryTooShort{
			Cluster: cluster, From: req.From, To: req.To, Snapshots: retained,
			Interval: backtest.DefaultConfig().DecisionInterval, Horizon: horizon,
		}}
	}

	res, err := whatif.Scenario{
		Cluster: cluster,
		From:    req.From, To: req.To,
		Horizon:   horizon,
		Baseline:  incumbent,
		Candidate: req.Candidate,
		// The store is the SnapshotSource, satisfied structurally, so both
		// replays read the same retained rows the count check just saw.
		History:                 b.st,
		Evidence:                b.mem,
		Catalog:                 b.catalog,
		Scoring:                 backtest.DefaultConfig(),
		EnforceDecisionRefusals: req.EnforceDecisionRefusals,
	}.Run()
	if err != nil {
		// Everything Scenario.validate rejects was checked above, so what is
		// left is a replay that failed on inputs this function accepted.
		return nil, defect{err}
	}
	if res == nil || res.BaselineScore == nil || res.CandidateScore == nil {
		return nil, defect{errors.New("the replay returned no scorecards")}
	}
	// backtest's own coverage report, not a predicate reimplemented here.
	// Both sides are scored over the same instants by construction, so the
	// baseline's count is the comparison's.
	if res.BaselineScore.Instants == 0 {
		return nil, notEnoughEvidence{ErrHistoryTooShort{
			Cluster: cluster, From: req.From, To: req.To,
			Snapshots: res.BaselineScore.Snapshots, Instants: res.BaselineScore.Instants,
			Interval: time.Duration(res.BaselineScore.DecisionIntervalHours * float64(time.Hour)),
			Horizon:  horizon,
		}}
	}
	return res, nil
}

func (s *whatIfSurface) handleWhatIf(w http.ResponseWriter, r *http.Request) {
	req, err := s.parseWhatIfQuery(r)
	if err != nil {
		writeErr(w, whatIfStatus(err), err)
		return
	}
	res, err := s.b.WhatIf(req)
	if err != nil {
		writeErr(w, whatIfStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, newSimulated(res, req))
}

// parseWhatIfQuery builds the request from the URL.
//
// from and to are REQUIRED, for the reason why-cost requires them: the window
// is an argument, and an answer computed over a wall-clock default cannot be
// replayed or compared to a stored one.
func (s *whatIfSurface) parseWhatIfQuery(r *http.Request) (WhatIfRequest, error) {
	q := r.URL.Query()
	from, to, err := parseWindow(r, time.Time{}, time.Time{})
	if err != nil {
		return WhatIfRequest{}, badRequest{err}
	}
	horizon := defaultWhatIfHorizon
	if raw := strings.TrimSpace(q.Get("horizon")); raw != "" {
		horizon, err = time.ParseDuration(raw)
		if err != nil {
			return WhatIfRequest{}, badRequest{fmt.Errorf("horizon: %w", err)}
		}
	}
	enforce, err := parseBoolParam(q.Get("enforceRefusals"))
	if err != nil {
		return WhatIfRequest{}, err
	}
	moves, err := parseAxisQuery(q["set"])
	if err != nil {
		return WhatIfRequest{}, err
	}
	candidate, err := s.candidate(moves, enforce)
	if err != nil {
		return WhatIfRequest{}, err
	}
	return WhatIfRequest{
		Cluster: r.PathValue("id"), From: from, To: to, Horizon: horizon,
		Candidate: candidate, EnforceDecisionRefusals: enforce,
	}, nil
}

// ---------------------------------------------------------------- the axes

// parseAxisQuery reads repeated `set=<axis>=<value>` parameters.
func parseAxisQuery(raw []string) (map[whatif.Axis]float64, error) {
	out := make(map[whatif.Axis]float64, len(raw))
	for _, kv := range raw {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, badRequest{fmt.Errorf("set %q: want <axis>=<value>, e.g. set=cpu-headroom=1.05", kv)}
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil, badRequest{fmt.Errorf("set %s: %w", name, err)}
		}
		axis := whatif.Axis(strings.TrimSpace(name))
		if _, dup := out[axis]; dup {
			// Two values for one knob is a question with two answers.
			return nil, badRequest{fmt.Errorf("set %s given twice", axis)}
		}
		out[axis] = v
	}
	return out, nil
}

// candidate projects the requested moves onto this brain's own policy.
//
// The projection restates the five-field axis→config mapping because
// whatif.Axis.get/set are unexported — the same restatement cmd/kilter's
// applyAxisSets had to make, and the same risk: setting MemoryHeadroom when
// the caller said cpu-headroom is internally consistent and wrong. The
// cross-check is pkg/whatif's OWN projection, which computes Result.Changes
// from Axis.get; TestSetMovesTheAxisPkgWhatifThinksItMoves drives every axis
// through the route and requires the change pkg/whatif reports to name the
// axis the caller named and carry the value it carried.
func (s *whatIfSurface) candidate(moves map[whatif.Axis]float64, enforceRefusals bool) (whatif.Policy, error) {
	if len(moves) == 0 {
		return whatif.Policy{}, badRequest{fmt.Errorf(
			"name at least one axis to move: set=<axis>=<value>, one of %s", axisList())}
	}
	// Validated in a sorted order so the first complaint about a request with
	// several bad axes does not depend on map iteration.
	names := make([]string, 0, len(moves))
	for a := range moves {
		names = append(names, string(a))
	}
	sort.Strings(names)
	bounds := whatif.HardBounds()
	for _, n := range names {
		axis := whatif.Axis(n)
		if !axis.Known() {
			return whatif.Policy{}, badRequest{fmt.Errorf("unknown axis %q: one of %s", n, axisList())}
		}
		v := moves[axis]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return whatif.Policy{}, badRequest{fmt.Errorf("axis %s: %v is not a finite value", n, v)}
		}
		if r, ok := bounds[axis]; ok && !r.Contains(v) {
			return whatif.Policy{}, badRequest{fmt.Errorf("axis %s: %v is outside the hard bounds %s", n, v, r)}
		}
	}
	// §3.6's mismatch, in the other direction. The soak axis lives in
	// decision.Config, and decision.Config can only VETO a change when the
	// refusal predicates are enforced. Without them the replay never consults
	// it for a decision — at most it borrows a predicate to NAME a refusal
	// that happened for another reason — so the two sides decide identically,
	// the delta is zeroes, and the answer reads as "we tested a longer soak
	// and it changed nothing". That is a what-if of a policy nobody ran,
	// which is the artefact this plane exists to prevent.
	if _, ok := moves[whatif.AxisBaseSoak]; ok && !enforceRefusals {
		return whatif.Policy{}, badRequest{fmt.Errorf(
			"axis %s moves decision.Config, which reaches a replay only when the refusal predicates run: "+
				"add enforceRefusals=true (it applies to BOTH replays, so it is part of the yardstick and "+
				"not part of the policy under test) or the two sides would differ only in their policy hash",
			whatif.AxisBaseSoak)}
	}

	// Applied in AllAxes order — the order pkg/whatif enumerates changes in —
	// so nothing about the answer depends on map iteration.
	p := s.b.IncumbentPolicy()
	for _, axis := range whatif.AllAxes {
		v, ok := moves[axis]
		if !ok {
			continue
		}
		switch axis {
		case whatif.AxisCPUPercentile:
			p.Rec.CPUPercentile = v
		case whatif.AxisMemoryPercentile:
			p.Rec.MemoryPercentile = v
		case whatif.AxisCPUHeadroom:
			p.Rec.CPUHeadroom = v
		case whatif.AxisMemoryHeadroom:
			p.Rec.MemoryHeadroom = v
		case whatif.AxisBaseSoak:
			// Quantized to whole minutes exactly as whatif.hoursToDuration
			// does, so a float round-trip cannot produce a duration whose
			// string form differs run to run.
			p.Decision.BaseSoak = time.Duration(math.Round(v*60)) * time.Minute
		}
	}
	return p, nil
}

func axisList() string {
	names := make([]string, 0, len(whatif.AllAxes))
	for _, a := range whatif.AllAxes {
		names = append(names, string(a))
	}
	return strings.Join(names, ", ")
}

func parseBoolParam(raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, badRequest{fmt.Errorf("enforceRefusals: %w", err)}
	}
	return v, nil
}

// ---------------------------------------------------------------- proposals

// handleListProposals is a pure projection of Store.List()/ListState().
//
// List() already arrives sorted by (CreatedAt, ID) and is NOT re-sorted here:
// a second sort would be a second definition of "the order proposals are read
// in", and the CLI's listing and this one would drift.
func (s *whatIfSurface) handleListProposals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	recs := s.props.List()
	if raw := strings.TrimSpace(q.Get("state")); raw != "" {
		want := whatif.State(raw)
		if !knownProposalState(want) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf(
				"state %q: one of gated, approved, rejected, applied, expired", raw))
			return
		}
		recs = s.props.ListState(want)
	}
	if c := strings.TrimSpace(q.Get("cluster")); c != "" {
		filtered := make([]*whatif.Record, 0, len(recs))
		for _, rec := range recs {
			if rec.Proposal().Cluster == c {
				filtered = append(filtered, rec)
			}
		}
		recs = filtered
	}
	if recs == nil {
		// Always an array: an empty store is [], never null, so a consumer
		// cannot confuse "no proposals" with "no answer".
		recs = []*whatif.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": recs})
}

// knownProposalState is the closed set a filter may name. StateDraft is
// absent because no record rests there — Create moves through it in the same
// call — so accepting it would be a filter that can only ever return nothing.
func knownProposalState(s whatif.State) bool {
	switch s {
	case whatif.StateGated, whatif.StateApproved, whatif.StateRejected,
		whatif.StateApplied, whatif.StateExpired:
		return true
	}
	return false
}

func (s *whatIfSurface) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	rec, ok := s.props.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %s", whatif.ErrNotFound, id))
		return
	}
	// The record whole — proposal, state, approval and audit trail — through
	// pkg/whatif's own MarshalJSON. Same bytes `kilter proposals show --json`
	// prints.
	writeJSON(w, http.StatusOK, rec)
}

// proposalRequest is the body of POST /api/v1/proposals.
//
// It is the QUESTION, not the answer. §7.1 specifies a Spec-ish body and is
// explicit about what must not be in it: no verdict, because Store.Create
// runs Decide itself. This goes one step further and does not accept the
// SCORECARDS either — the brain replays its own retained history to produce
// them. A caller-supplied scorecard is a measurement of unknown provenance,
// and a proposal is a document whose whole value is that its numbers came
// from somewhere checkable.
//
// Unknown fields are rejected, so a body that grows a "gate", an "author", an
// "envelope" or a "tolerance" is a 400 rather than a silently ignored field.
// That is the regression §7.1 names, made loud.
type proposalRequest struct {
	Cluster         string             `json:"cluster"`
	From            string             `json:"from"`
	To              string             `json:"to"`
	Horizon         string             `json:"horizon,omitempty"`
	Set             map[string]float64 `json:"set"`
	EnforceRefusals bool               `json:"enforceRefusals,omitempty"`
	Rationale       string             `json:"rationale,omitempty"`
}

func (s *whatIfSurface) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	// DisallowUnknownFields is the enforcement point: silently ignoring a
	// field in a security-relevant request is how "the proposer cannot supply
	// the verdict" becomes a comment about a field that is being accepted.
	dec.DisallowUnknownFields()
	var req proposalRequest
	if err := dec.Decode(&req); err != nil {
		writeErr(w, decodeStatus(err), fmt.Errorf("decode body: %w", err))
		return
	}
	rec, err := s.create(req)
	if err != nil {
		writeErr(w, whatIfStatus(err), err)
		return
	}
	// 200, never 201. Proposals are content-addressed, so filing the same one
	// twice returns the record that already exists — which is what makes a
	// nightly tuner idempotent rather than a duplicate factory — and a 201
	// would claim a document was created that was not.
	writeJSON(w, http.StatusOK, rec)
}

func (s *whatIfSurface) create(req proposalRequest) (*whatif.Record, error) {
	from, err := time.Parse(time.RFC3339, strings.TrimSpace(req.From))
	if err != nil {
		return nil, badRequest{fmt.Errorf("from: %w", err)}
	}
	to, err := time.Parse(time.RFC3339, strings.TrimSpace(req.To))
	if err != nil {
		return nil, badRequest{fmt.Errorf("to: %w", err)}
	}
	horizon := defaultWhatIfHorizon
	if raw := strings.TrimSpace(req.Horizon); raw != "" {
		if horizon, err = time.ParseDuration(raw); err != nil {
			return nil, badRequest{fmt.Errorf("horizon: %w", err)}
		}
	}
	if len(req.Rationale) > maxWhatIfRationale {
		return nil, badRequest{fmt.Errorf("rationale is %d bytes, over the %d limit",
			len(req.Rationale), maxWhatIfRationale)}
	}
	moves := make(map[whatif.Axis]float64, len(req.Set))
	for k, v := range req.Set {
		moves[whatif.Axis(strings.TrimSpace(k))] = v
	}
	candidate, err := s.candidate(moves, req.EnforceRefusals)
	if err != nil {
		return nil, err
	}
	wi := WhatIfRequest{
		Cluster: req.Cluster, From: from, To: to, Horizon: horizon,
		Candidate: candidate, EnforceDecisionRefusals: req.EnforceRefusals,
	}
	// The same call, the same refusals and the same status codes the GET
	// serves: a proposal cannot be filed from evidence the what-if route
	// would have refused to show.
	res, err := s.b.WhatIf(wi)
	if err != nil {
		return nil, err
	}
	// The envelope and the tolerance are the SHIPPED defaults and are not
	// request fields. They are the yardstick the gate judges by; a proposer
	// that could widen them would be supplying its own verdict with extra
	// steps.
	// The target is left empty so Spec fills it from the RESULT's own
	// cluster: a proposal can then never name a cluster other than the one
	// that was actually replayed. Namespace and Class stay empty for the
	// reason cmd/ leaves them empty — nothing narrows the replay to them, so
	// a narrowed target would be documentation printed on top of a
	// cluster-wide measurement. EvidenceIDs are empty rather than filled with
	// plausible-looking strings: a citation that resolves to nothing is worse
	// than no citation.
	spec, err := res.Spec(whatif.Target{},
		whatif.DefaultEnvelope(), whatif.DefaultTolerance(), req.Rationale, nil)
	if err != nil {
		return nil, defect{err}
	}
	// Store.Create runs the gate. Its verdict is not in the request and not
	// in this file.
	rec, err := s.props.Create(whatIfAuthor, spec, s.now)
	if err != nil {
		return nil, defect{fmt.Errorf("filing the proposal: %w", err)}
	}
	return rec, nil
}

// rejectionRequest is the body of POST /api/v1/proposals/{id}/rejections.
type rejectionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// handleRejectProposal records a no.
//
// Any actor may reject: refusing to make a change is always safe, so it needs
// no capability. That asymmetry is the whole reason this route is implemented
// and the approvals route is not — what an unauthenticated funnel cannot
// supply is a human, and a rejection does not need one.
func (s *whatIfSurface) handleRejectProposal(w http.ResponseWriter, r *http.Request) {
	var req rejectionRequest
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	// An absent body is a rejection with no reason, which is allowed; a
	// malformed one is not. A body naming its own actor is rejected with
	// every other unknown field — the actor comes from the funnel.
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, decodeStatus(err), fmt.Errorf("decode body: %w", err))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	rec, err := s.props.Reject(whatIfAuthor, id, req.Reason, s.now)
	if err != nil {
		writeErr(w, whatIfStatus(rejectionError(err)), err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// rejectionError classifies what Store.Reject refused. An unknown id is a
// 404; a record that cannot leave its current state is a 409, because the
// request was well-formed and the answer is about the record's state rather
// than about the request.
func rejectionError(err error) error {
	if errors.Is(err, whatif.ErrNotFound) {
		return notIngested{err}
	}
	return conflict{err}
}

// ---------------------------------------------------------------- refusals

// handleApprovalRefused is the security boundary, and this build cannot hold
// it, so it refuses.
//
// §7.1: "the token tier must be one the MCP server and the reasoner do not
// hold, and the actor must come from the token, never from the body". This
// brain has two tiers — the write token and the read token — and every
// controller and agent that ingests a snapshot holds the write one. Minting
// Actor{Kind: ActorHuman} from it would hand the approval capability to the
// exact actor the human-only rule exists to exclude, and would do it silently,
// in the direction that grants capability.
//
// So the route exists, names what is missing, and moves nothing.
func (s *whatIfSurface) handleApprovalRefused(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	writeErr(w, http.StatusNotImplemented, fmt.Errorf(
		`approving proposal %s is refused: this brain has no human token tier to authenticate an approver.

whatif.NewApprover requires an Actor{Kind: human}, and pkg/whatif's guarantee is that
self-approval is not a rule that gets checked but a value that cannot be constructed. It
rests on one input this process cannot supply: proof that a human is on the other end.

BrainConfig carries a write token and a read token. The write token is held by every
controller and agent that ingests a snapshot — including the reasoner — so deriving
Actor{Kind: ActorHuman} from it would grant approval to the actor the human-only rule
exists to exclude, and pkg/whatif's structural guarantee would become a convention that
one bearer token walks around.

A human tier is a new BrainConfig field and a startup-time decision about which callers
hold it. That is a configuration change this unit did not make; see
pkg/api/WHATIFROUTES-FINDINGS.md and cmd/WHATIF-WIRING-FINDINGS.md §3.3.

What this funnel can do, because it needs no capability:
  POST /api/v1/proposals/%s/rejections`, id, id))
}

// handleAppliedRefused explains why the after-the-fact record is not here.
//
// Two independent reasons, and the second is the one that settles it: nothing
// in pkg/api writes a policy config, and §4.6 requires a LedgerEntry in the
// same breath as the applied transition — proposal ID, both policy hashes and
// the claimed Delta.ProjectedMonthlyUSD — because that entry is what later
// lets the claimed-vs-measured join score the tuner itself. Recording
// "applied" from here would put an unbacked fact into an audit trail whose
// whole value is that its facts are backed. It is also unreachable: only an
// approved proposal can be applied, and no approval can be minted here.
func (s *whatIfSurface) handleAppliedRefused(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	writeErr(w, http.StatusNotImplemented, fmt.Errorf(
		`recording proposal %s as applied is refused: nothing in this process applies a policy change.

whatif.Store.MarkApplied records, AFTER THE FACT, that a config change actually landed,
and §4.6 requires a LedgerEntry written with it: the proposal id, both policy hashes and
the claimed Delta.ProjectedMonthlyUSD. This package writes neither the config nor that
entry, so an "applied" recorded here would be an unbacked fact in an audit trail.

It is also unreachable: only an approved proposal can be applied, and no approval can be
minted here (POST /api/v1/proposals/%s/approvals explains why).

A what-if plane is a hypothetical. It does not apply anything, and it does not accept a
proposal for execution — pkg/ec2 and pkg/rds are not reachable from pkg/api and making
them reachable is a separate decision. See pkg/api/WHATIFROUTES-FINDINGS.md.`, id, id))
}

// ---------------------------------------------------------------- errors

// The failure classes this plane adds to explainroutes.go's three, as types
// rather than string matching, so the HTTP status is a property of the
// failure and not of its wording.
type (
	// notIngested is "there is no such thing here" — 404.
	notIngested struct{ error }
	// conflict is "the request was fine, the record's state says no" — 409.
	conflict struct{ error }
	// defect is "this process could not do something it should have been able
	// to do" — 500, and the only class worth paging on.
	defect struct{ error }
)

// whatIfStatus maps a failure to its status code.
//
// The default is 500 on purpose, and it is the opposite of explainStatus's.
// Every error this file returns is classified where it is created, so an
// unclassified one means a path was added without deciding what kind of
// failure it is — which is itself a defect in this file rather than a fact
// about the caller's evidence. TestEveryWhatIfFailureIsClassified pins the
// classes that are reachable.
func whatIfStatus(err error) int {
	switch err.(type) {
	case badRequest:
		return http.StatusBadRequest
	case notIngested:
		return http.StatusNotFound
	case notEnoughEvidence:
		return http.StatusUnprocessableEntity
	case conflict:
		return http.StatusConflict
	case defect:
		return http.StatusInternalServerError
	}
	if errors.Is(err, whatif.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// compile-time assertion that the record shape these routes serve is the one
// pkg/whatif marshals: Record has MarshalJSON, so writeJSON emits the
// package's own wire form rather than a pkg/api-side projection of it — the
// same assertion cmd/kilter/proposals.go makes about its own output.
var _ json.Marshaler = (*whatif.Record)(nil)

// and that a counterfactual can only leave this package inside its envelope.
var _ json.Marshaler = Simulated{}
