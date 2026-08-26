package whatif

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrTunerDisabled is returned by Tuner.Run when the tuner is off, which is
// the shipped default. It is a distinct error so a caller can tell "we looked
// and found nothing" from "we never looked".
var ErrTunerDisabled = errors.New("whatif: the policy tuner is disabled (--auto-tune is off by default)")

// Step is one axis's grid spacing, in envelope units.
type Step struct {
	Axis Axis    `json:"axis"`
	Size float64 `json:"size"`
}

// DefaultSteps is §4.6's grid, verbatim: "percentile ±2pts, headroom ±5%,
// soak ±2h". Nothing wider ships, because a wider step is a bigger jump onto
// a production cluster per nightly cycle, and the loop's safety comes from
// being able to walk back one step as easily as it walked forward.
func DefaultSteps() []Step {
	return []Step{
		{Axis: AxisCPUPercentile, Size: 0.02},
		{Axis: AxisMemoryPercentile, Size: 0.02},
		{Axis: AxisCPUHeadroom, Size: 0.05},
		{Axis: AxisMemoryHeadroom, Size: 0.05},
		{Axis: AxisBaseSoak, Size: 2},
	}
}

// maxCandidates is the hard ceiling on one tuning run, whatever the config
// says. Each candidate is a full replay of the trailing window; an unbounded
// grid is a nightly job that never finishes.
const maxCandidates = 64

// TunerConfig parameterizes the nightly loop.
type TunerConfig struct {
	// Enabled is FALSE BY DEFAULT and must be set explicitly. §4.6 ships the
	// loop off; this is the field that keeps it off, and it is the zero value
	// so that every zero TunerConfig in the codebase is a disabled tuner.
	Enabled bool `json:"enabled"`

	// Envelope is the declared search space. Required when enabled: a tuner
	// with no envelope is an unbounded search, which this package will not do.
	Envelope Envelope `json:"envelope"`
	// Steps is the grid spacing per axis. Empty means DefaultSteps.
	Steps []Step `json:"steps"`
	// FullFactorial searches every combination of every axis instead of one
	// axis at a time. Off by default: the coordinate-wise grid is 2n+1
	// candidates where the factorial is 3^n, and §4.6 asks for "a small
	// candidate grid". When on, the enumeration is truncated at
	// MaxCandidates and the number dropped is reported, never silently.
	FullFactorial bool `json:"fullFactorial"`
	// MaxCandidates bounds one run. Zero means maxCandidates; values above
	// it are clamped down, never up.
	MaxCandidates int `json:"maxCandidates"`

	// Trailing is how much history to replay. Default 30 days, per §4.6.
	Trailing time.Duration `json:"trailing"`
	// Horizon is passed to the harness. Default 24h.
	Horizon time.Duration `json:"horizon"`

	// Tolerance is the gate. Zero takes DefaultTolerance.
	Tolerance Tolerance `json:"tolerance"`

	// Author identifies the tuner in the audit trail. Zero takes
	// {tuner, "nightly"}. It may not be a human: the loop must never be able
	// to file a proposal that looks like a person filed it, because then a
	// person could approve it and the author≠approver rule would be
	// satisfied by a lie.
	Author Actor `json:"author"`
}

// DefaultTunerConfig returns the shipped configuration — which is OFF.
func DefaultTunerConfig() TunerConfig {
	return TunerConfig{
		Enabled:       false,
		Envelope:      DefaultEnvelope(),
		Steps:         DefaultSteps(),
		MaxCandidates: maxCandidates,
		Trailing:      30 * 24 * time.Hour,
		Horizon:       defaultHorizon,
		Tolerance:     DefaultTolerance(),
		Author:        Actor{Kind: ActorTuner, ID: "nightly"},
	}
}

// normalize applies defaults and rejects a configuration that could not run
// safely. Every bound is clamped downward only: a config file cannot widen the
// search, only narrow it.
func (c TunerConfig) normalize() (TunerConfig, error) {
	d := DefaultTunerConfig()
	if c.Envelope.Bounds == nil {
		c.Envelope = d.Envelope
	}
	if err := c.Envelope.Validate(); err != nil {
		return TunerConfig{}, err
	}
	if len(c.Steps) == 0 {
		c.Steps = d.Steps
	}
	steps, err := normalizeSteps(c.Steps, c.Envelope)
	if err != nil {
		return TunerConfig{}, err
	}
	c.Steps = steps
	if c.MaxCandidates <= 0 || c.MaxCandidates > maxCandidates {
		c.MaxCandidates = maxCandidates
	}
	if c.Trailing <= 0 {
		c.Trailing = d.Trailing
	}
	if c.Horizon <= 0 {
		c.Horizon = d.Horizon
	}
	if c.Horizon > c.Trailing {
		return TunerConfig{}, fmt.Errorf("whatif: tuner horizon %v exceeds the %v trailing window",
			c.Horizon, c.Trailing)
	}
	c.Tolerance = c.Tolerance.withDefaults()
	if c.Author == (Actor{}) {
		c.Author = d.Author
	}
	if err := c.Author.Validate(); err != nil {
		return TunerConfig{}, err
	}
	if c.Author.canApprove() {
		return TunerConfig{}, fmt.Errorf(
			"whatif: the tuner may not author proposals as a human (%s): "+
				"it would let a person approve what the loop wrote", c.Author)
	}
	return c, nil
}

// normalizeSteps validates, de-duplicates and orders the grid spacing. Steps
// are sorted into AllAxes order so candidate enumeration — and therefore the
// proposal a given history produces — does not depend on config file order.
func normalizeSteps(in []Step, env Envelope) ([]Step, error) {
	byAxis := map[Axis]float64{}
	for _, st := range in {
		if !st.Axis.Known() {
			return nil, fmt.Errorf("whatif: tuner step names unknown axis %q", st.Axis)
		}
		if !(st.Size > 0) {
			return nil, fmt.Errorf("whatif: tuner step for %s is %v; must be > 0", st.Axis, st.Size)
		}
		r, ok := env.Bounds[st.Axis]
		if !ok {
			// A step on an axis the envelope pins is a contradiction: the
			// caller asked to search a knob it also declared off-limits.
			return nil, fmt.Errorf("whatif: tuner step on %s, which the envelope does not declare", st.Axis)
		}
		if width := r.Max - r.Min; st.Size > width {
			return nil, fmt.Errorf("whatif: tuner step %v on %s is wider than its envelope %s",
				st.Size, st.Axis, r)
		}
		if prev, dup := byAxis[st.Axis]; dup && prev != st.Size {
			return nil, fmt.Errorf("whatif: two different steps declared for %s (%v, %v)",
				st.Axis, prev, st.Size)
		}
		byAxis[st.Axis] = st.Size
	}
	out := make([]Step, 0, len(byAxis))
	for _, a := range AllAxes {
		if size, ok := byAxis[a]; ok {
			out = append(out, Step{Axis: a, Size: size})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("whatif: tuner has no axes to search")
	}
	return out, nil
}

// Candidates enumerates the grid around a base policy, deterministically.
//
// Every candidate is clamped into the envelope before it is emitted, and any
// candidate that lands back on the base policy (because the clamp pulled it
// there) is dropped: proposing the incumbent is not a proposal. The result is
// sorted by policy hash and de-duplicated, so the same (base, config) always
// produces the same list in the same order, whatever the history says.
//
// Truncated reports how many candidates the MaxCandidates cap dropped. It is
// returned rather than logged internally so a caller can surface it — a search
// that silently covered half the grid reads exactly like one that covered all
// of it.
func (c TunerConfig) Candidates(base Policy) (out []Policy, truncated int, err error) {
	cfg, err := c.normalize()
	if err != nil {
		return nil, 0, err
	}
	base = cfg.Envelope.Clamp(base.withDefaults())

	raw := []Policy{}
	if cfg.FullFactorial {
		raw = factorial(base, cfg.Steps, cfg.Envelope)
	} else {
		for _, st := range cfg.Steps {
			for _, sign := range []float64{-1, 1} {
				p := st.Axis.set(base, st.Axis.get(base)+sign*st.Size)
				raw = append(raw, cfg.Envelope.Clamp(p))
			}
		}
	}

	baseHash := base.Hash()
	seen := map[string]bool{baseHash: true}
	keyed := make([]Policy, 0, len(raw))
	for _, p := range raw {
		h := p.Hash()
		if seen[h] {
			continue
		}
		// A candidate the engine could not run is dropped here rather than
		// failing the whole nightly run: the grid walks to the edge of the
		// envelope by design, and one arithmetic corner should not stop the
		// other candidates from being scored.
		if err := p.Validate(); err != nil {
			continue
		}
		// Belt to the clamp's braces. The clamp above should make this
		// unreachable; if it ever is reachable, an out-of-envelope candidate
		// must be dropped, not proposed. FuzzTunerStaysInsideItsEnvelope
		// asserts this never fires.
		if !cfg.Envelope.Contains(p) {
			continue
		}
		seen[h] = true
		keyed = append(keyed, p)
	}
	sort.Slice(keyed, func(i, j int) bool { return keyed[i].Hash() < keyed[j].Hash() })
	if len(keyed) > cfg.MaxCandidates {
		truncated = len(keyed) - cfg.MaxCandidates
		keyed = keyed[:cfg.MaxCandidates]
	}
	return keyed, truncated, nil
}

// factorial enumerates every (−, 0, +) combination across the stepped axes.
// It is bounded by the caller's MaxCandidates; the recursion itself is bounded
// by len(AllAxes), which is a compile-time constant.
func factorial(base Policy, steps []Step, env Envelope) []Policy {
	out := []Policy{base}
	for _, st := range steps {
		next := make([]Policy, 0, len(out)*3)
		for _, p := range out {
			for _, sign := range []float64{-1, 0, 1} {
				if sign == 0 {
					next = append(next, p)
					continue
				}
				next = append(next, env.Clamp(st.Axis.set(p, st.Axis.get(p)+sign*st.Size)))
			}
		}
		out = next
	}
	return out
}

// TunerCandidate is one scored point on the grid.
type TunerCandidate struct {
	Policy     Policy     `json:"policy"`
	Hash       string     `json:"hash"`
	Changes    []Change   `json:"changes"`
	Delta      Delta      `json:"delta"`
	Gate       GateResult `json:"gate"`
	ProposalID string     `json:"proposalID,omitempty"`
	// Err is set when this candidate could not be replayed. One bad
	// candidate does not fail the run; it is recorded and skipped.
	Err string `json:"error,omitempty"`
}

// TunerReport is one nightly run's output.
type TunerReport struct {
	Cluster      string       `json:"cluster"`
	Window       [2]time.Time `json:"window"`
	HorizonHours float64      `json:"horizonHours"`
	RanAt        time.Time    `json:"ranAt"`
	BasePolicy   string       `json:"basePolicy"`

	// Considered is every candidate the grid produced, sorted by policy hash
	// — the same order Candidates emitted them in.
	Considered []TunerCandidate `json:"considered"`
	// Accepted names the candidates that passed the gate, best first.
	Accepted []string `json:"accepted"`
	// Truncated is how many grid points the cap dropped. Reported, never
	// silent.
	Truncated int `json:"truncated"`
	// Filed is the proposal IDs written to the store, in Accepted order.
	Filed []string `json:"filed"`

	// BestProjectedMonthlyUSD is the projected monthly saving of the best
	// accepted candidate, and TotalProjectedMonthlyUSD is the sum over all
	// accepted ones — the latter as an upper bound on what a night's search
	// found, NOT a claim that the candidates compose (they do not; they are
	// alternatives). Both go through sumUSD.
	BestProjectedMonthlyUSD  float64 `json:"bestProjectedMonthlyUSD"`
	TotalProjectedMonthlyUSD float64 `json:"totalProjectedMonthlyUSD"`
}

// Tuner is the nightly closed loop of §4.6: enumerate a bounded grid, score
// each point with the real backtest harness, gate each result, and file the
// survivors as proposals.
//
// It cannot apply anything. It holds no cluster client, and the furthest state
// it can move a proposal to is StateGated — StateApproved needs an *Approver,
// and NewApprover refuses an ActorTuner. That is the whole safety argument,
// and it is a constructor signature rather than a policy.
type Tuner struct {
	cfg   TunerConfig
	store *Store
}

// NewTuner builds a tuner. A disabled config is accepted and constructed —
// Run is where the refusal happens — so that a brain can hold a tuner
// unconditionally and the "is it on?" question has exactly one answer site.
func NewTuner(cfg TunerConfig, store *Store) (*Tuner, error) {
	if store == nil {
		return nil, errors.New("whatif: tuner needs a proposal store")
	}
	if !cfg.Enabled {
		// Still normalize, so a disabled-but-invalid config is a startup
		// error rather than a surprise on the night somebody enables it.
		if _, err := cfg.normalize(); err != nil {
			return nil, err
		}
		return &Tuner{cfg: cfg, store: store}, nil
	}
	norm, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	norm.Enabled = true
	return &Tuner{cfg: norm, store: store}, nil
}

// Enabled reports whether the loop will run.
func (t *Tuner) Enabled() bool { return t != nil && t.cfg.Enabled }

// Config returns the normalized configuration.
func (t *Tuner) Config() TunerConfig { return t.cfg }

// Run executes one nightly cycle over [historyEnd−Trailing, historyEnd).
//
// historyEnd comes from the caller — it is the newest snapshot in the store,
// NOT the wall clock. §4.4's CLI note says the same thing about `--from 30d`,
// and for the same reason: a window derived from time.Now makes two runs over
// identical history disagree, and a proposal that is not reproducible cannot
// be audited. The clock is used only to stamp records.
func (t *Tuner) Run(base Policy, scen Scenario, historyEnd time.Time, clock Clock) (*TunerReport, error) {
	if t == nil || !t.cfg.Enabled {
		return nil, ErrTunerDisabled
	}
	now := clock.now()
	if now.IsZero() {
		return nil, errors.New("whatif: tuner needs a clock")
	}
	historyEnd = historyEnd.UTC()
	if historyEnd.IsZero() {
		return nil, errors.New("whatif: tuner needs the end of the recorded history, not a wall clock")
	}
	base = t.cfg.Envelope.Clamp(base.withDefaults())
	if err := base.Validate(); err != nil {
		return nil, fmt.Errorf("whatif: tuner base policy: %w", err)
	}

	cands, truncated, err := t.cfg.Candidates(base)
	if err != nil {
		return nil, err
	}

	scen.From = historyEnd.Add(-t.cfg.Trailing)
	scen.To = historyEnd
	scen.Horizon = t.cfg.Horizon
	scen.Baseline = base
	scen.Envelope = t.cfg.Envelope
	scen.Tolerance = t.cfg.Tolerance

	rep := &TunerReport{
		Cluster:      scen.Cluster,
		Window:       [2]time.Time{scen.From, scen.To},
		HorizonHours: round6(t.cfg.Horizon.Hours()),
		RanAt:        now,
		BasePolicy:   base.Hash(),
		Truncated:    truncated,
	}

	type accepted struct {
		hash string
		res  *Result
	}
	var wins []accepted
	for _, cand := range cands {
		s := scen
		s.Candidate = cand
		entry := TunerCandidate{Policy: cand, Hash: cand.Hash(), Changes: changesBetween(base, cand)}
		res, err := s.Run()
		if err != nil {
			entry.Err = err.Error()
			rep.Considered = append(rep.Considered, entry)
			continue
		}
		entry.Delta, entry.Gate = res.Delta, res.Gate
		if res.Gate.Passed {
			wins = append(wins, accepted{hash: cand.Hash(), res: res})
		}
		rep.Considered = append(rep.Considered, entry)
	}

	// Best first: largest regret improvement, ties broken by policy hash so
	// the ordering is total and reproducible.
	sort.Slice(wins, func(i, j int) bool {
		di := wins[i].res.Delta.RegretUSD
		dj := wins[j].res.Delta.RegretUSD
		if di != dj {
			return di < dj // more negative is a bigger improvement
		}
		return wins[i].hash < wins[j].hash
	})

	monthly := make([]float64, 0, len(wins))
	for _, w := range wins {
		rep.Accepted = append(rep.Accepted, w.hash)
		monthly = append(monthly, w.res.Delta.ProjectedMonthlyUSD)
		id, err := t.file(w.res, clock)
		if err != nil {
			return nil, err
		}
		rep.Filed = append(rep.Filed, id)
		for i := range rep.Considered {
			if rep.Considered[i].Hash == w.hash {
				rep.Considered[i].ProposalID = id
			}
		}
	}
	if len(monthly) > 0 {
		rep.BestProjectedMonthlyUSD = round6(monthly[0])
	}
	// sumUSD, not a running total: the order candidates finished in must not
	// change the reported dollars.
	rep.TotalProjectedMonthlyUSD = round6(sumUSD(monthly))
	return rep, nil
}

// file writes an accepted result to the store as a proposal. The store re-runs
// the gate from the scorecards, so this is evidence being handed over, not a
// verdict being asserted.
func (t *Tuner) file(res *Result, clock Clock) (string, error) {
	spec, err := res.Spec(Target{Cluster: res.Cluster}, t.cfg.Envelope, t.cfg.Tolerance,
		tunerRationale(res), nil)
	if err != nil {
		return "", err
	}
	rec, err := t.store.Create(t.cfg.Author, spec, clock)
	if err != nil {
		return "", err
	}
	if rec.State() != StateGated {
		// The store's own gate disagreed with the one Scenario.Run ran. They
		// are the same function over the same inputs, so this is impossible;
		// if it ever happens it is a bug, and filing anyway would mean the
		// tuner had talked its way past the gate.
		return "", fmt.Errorf("whatif: tuner filed %s and the store gated it %s: %v",
			rec.ID(), rec.State(), rec.Proposal().Gate.Reasons)
	}
	return rec.ID(), nil
}

// tunerRationale writes the deterministic one-liner an approver reads first.
// It is generated, not free text: the tuner has no model and no opinions, and
// a rationale that claimed more than the scorecard says would be the first
// place this loop started lying.
func tunerRationale(res *Result) string {
	var parts string
	for i, c := range res.Changes {
		if i > 0 {
			parts += ", "
		}
		parts += c.Text
	}
	return fmt.Sprintf("nightly tuner: %s — regret $%.4f → $%.4f over %.0fh of replay, "+
		"no safety or stability regression",
		parts, res.BaselineScore.RegretUSD, res.CandidateScore.RegretUSD, res.Delta.WindowHours)
}
