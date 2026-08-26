package whatif

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
)

func TestTunerIsOffByDefault(t *testing.T) {
	if DefaultTunerConfig().Enabled {
		t.Fatal("§4.6 ships the closed loop off; DefaultTunerConfig must not enable it")
	}
	// And the zero value — every TunerConfig{} anywhere in the codebase — is
	// a disabled tuner.
	if (TunerConfig{}).Enabled {
		t.Fatal("the zero TunerConfig must be disabled")
	}
	tn, err := NewTuner(DefaultTunerConfig(), NewStore())
	if err != nil {
		t.Fatal(err)
	}
	if tn.Enabled() {
		t.Fatal("Enabled() disagrees with the config")
	}
	_, err = tn.Run(DefaultPolicy(), Scenario{}, testTo, fixedClock())
	if !errors.Is(err, ErrTunerDisabled) {
		t.Fatalf("a disabled tuner ran: %v", err)
	}
}

func TestTunerCannotAuthorAsAHuman(t *testing.T) {
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	cfg.Author = Actor{Kind: ActorHuman, ID: "alice"}
	if _, err := NewTuner(cfg, NewStore()); err == nil {
		t.Fatal("the tuner was allowed to author proposals as a human, which would let " +
			"a person approve what the loop wrote")
	}
}

func TestTunerCannotApprove(t *testing.T) {
	// The whole safety argument, as a compile-and-run assertion: there is no
	// route from the tuner's identity to the approval capability.
	if _, err := NewApprover(DefaultTunerConfig().Author); !errors.Is(err, ErrNotAnApprover) {
		t.Fatalf("the tuner obtained an approver capability: %v", err)
	}
}

// ---- bounds ----

func TestCandidatesStayInsideTheEnvelope(t *testing.T) {
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	// A base policy pinned at the edge, so every "+" step wants to walk out.
	base := DefaultPolicy()
	base.Rec.CPUPercentile = 0.99
	base.Rec.MemoryPercentile = 0.999
	base.Rec.CPUHeadroom = 1.50
	base.Rec.MemoryHeadroom = 1.50
	base.Decision.BaseSoak = 24 * time.Hour

	cands, truncated, err := cfg.Candidates(base)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if truncated != 0 {
		t.Fatalf("the coordinate-wise grid should fit under the cap, %d dropped", truncated)
	}
	if len(cands) == 0 {
		t.Fatal("no candidates; the test would be vacuous")
	}
	for _, p := range cands {
		if !cfg.Envelope.Contains(p) {
			t.Fatalf("candidate outside the envelope: %v", cfg.Envelope.Violations(p))
		}
		if p.Hash() == base.Hash() {
			t.Fatal("the grid emitted the incumbent as a candidate")
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("the grid emitted an unrunnable policy: %v", err)
		}
	}
}

// TestMemoryPercentileCannotWalkPastOne is the concrete corner the clamp
// exists for: the default 0.99 plus a 0.02 step is 1.01, which recommend.New
// rejects outright.
func TestMemoryPercentileCannotWalkPastOne(t *testing.T) {
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	cands, _, err := cfg.Candidates(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cands {
		if p.Rec.MemoryPercentile > 1 {
			t.Fatalf("candidate has MemoryPercentile %v", p.Rec.MemoryPercentile)
		}
		if p.Rec.CPUPercentile > 1 {
			t.Fatalf("candidate has CPUPercentile %v", p.Rec.CPUPercentile)
		}
	}
}

func TestCandidateEnumerationIsDeterministic(t *testing.T) {
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	cfg.FullFactorial = true
	first, trunc, err := cfg.Candidates(DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if trunc == 0 {
		t.Fatal("the factorial grid should exceed the cap; the truncation path is untested otherwise")
	}
	for i := 0; i < 20; i++ {
		got, gotTrunc, err := cfg.Candidates(DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if gotTrunc != trunc || len(got) != len(first) {
			t.Fatalf("run %d: %d candidates (%d dropped), want %d (%d)",
				i, len(got), gotTrunc, len(first), trunc)
		}
		for j := range got {
			if got[j].Hash() != first[j].Hash() {
				t.Fatalf("run %d differed at %d", i, j)
			}
		}
	}
	if len(first) > maxCandidates {
		t.Fatalf("%d candidates, over the %d cap", len(first), maxCandidates)
	}
}

func TestTunerConfigRejectsAnUnboundedSearch(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*TunerConfig)
	}{
		{"envelope wider than hard bounds", func(c *TunerConfig) {
			c.Envelope = Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 0.2, Max: 8}}}
			c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: 0.05}}
		}},
		{"empty envelope", func(c *TunerConfig) { c.Envelope = Envelope{Bounds: map[Axis]Range{}} }},
		{"unknown axis in steps", func(c *TunerConfig) {
			c.Steps = []Step{{Axis: "everything", Size: 1}}
		}},
		{"zero step", func(c *TunerConfig) { c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: 0}} }},
		{"negative step", func(c *TunerConfig) { c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: -1}} }},
		{"NaN step", func(c *TunerConfig) {
			c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: math.NaN()}}
		}},
		{"step wider than its envelope", func(c *TunerConfig) {
			c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: 1.0}}
		}},
		{"step on an undeclared axis", func(c *TunerConfig) {
			c.Envelope = Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 1.1, Max: 1.3}}}
			c.Steps = []Step{{Axis: AxisMemoryHeadroom, Size: 0.05}}
		}},
		{"conflicting steps on one axis", func(c *TunerConfig) {
			c.Steps = []Step{
				{Axis: AxisCPUHeadroom, Size: 0.05},
				{Axis: AxisCPUHeadroom, Size: 0.10},
			}
		}},
		{"horizon longer than the window", func(c *TunerConfig) {
			c.Trailing = time.Hour
			c.Horizon = 24 * time.Hour
		}},
		{"invalid author", func(c *TunerConfig) { c.Author = Actor{Kind: "root", ID: "x"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultTunerConfig()
			cfg.Enabled = true
			tc.mut(&cfg)
			if _, err := NewTuner(cfg, NewStore()); err == nil {
				t.Fatal("an unbounded or invalid tuner config was accepted")
			}
			// And a disabled tuner with the same broken config must also
			// fail at construction, not on the night somebody enables it.
			cfg.Enabled = false
			if _, err := NewTuner(cfg, NewStore()); err == nil {
				t.Fatal("a disabled tuner accepted an invalid config")
			}
		})
	}
}

func TestTunerConfigClampsDownwardOnly(t *testing.T) {
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	cfg.MaxCandidates = 1_000_000
	norm, err := cfg.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if norm.MaxCandidates != maxCandidates {
		t.Fatalf("MaxCandidates = %d, want the %d ceiling", norm.MaxCandidates, maxCandidates)
	}
	cfg.MaxCandidates = 3
	norm, err = cfg.normalize()
	if err != nil {
		t.Fatal(err)
	}
	if norm.MaxCandidates != 3 {
		t.Fatalf("a narrower cap was widened to %d", norm.MaxCandidates)
	}
}

// FuzzTunerStaysInsideItsEnvelope is the bounds fuzz the brief asks for: no
// base policy, no envelope and no step size lets the grid emit a policy
// outside the declared search space — and the envelope itself can never be
// wider than the hard bounds.
func FuzzTunerStaysInsideItsEnvelope(f *testing.F) {
	f.Add(0.95, 0.99, 1.15, 1.20, 6.0, 0.02, 0.05, 2.0, 0.1, 0.9, false)
	f.Add(0.0, 0.0, 0.0, 0.0, 0.0, 1e-9, 1e-9, 1e-9, 0.0, 1.0, true)
	f.Add(1e9, -1e9, math.MaxFloat64, -1.0, 1e6, 1e6, 1e6, 1e6, -1.0, 2.0, true)

	f.Fuzz(func(t *testing.T,
		cpuP, memP, cpuH, memH, soak float64, // base policy, possibly absurd
		stepP, stepH, stepS float64, // step sizes
		lo, hi float64, // envelope shape, as fractions of the hard range
		factorial bool) {

		for _, v := range []float64{cpuP, memP, cpuH, memH, soak, stepP, stepH, stepS, lo, hi} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Skip()
			}
		}
		// Build an envelope inside the hard bounds by construction: this is
		// the contract a caller must satisfy, and Validate enforces it.
		env := Envelope{Bounds: map[Axis]Range{}}
		for _, a := range AllAxes {
			hard := hardEnvelope[a]
			w := hard.Max - hard.Min
			l := hard.Min + clamp01(lo)*w
			h := hard.Min + clamp01(hi)*w
			if h < l {
				l, h = h, l
			}
			env.Bounds[a] = Range{Min: l, Max: h}
		}
		if err := env.Validate(); err != nil {
			t.Fatalf("a fuzz-built envelope inside the hard bounds failed to validate: %v", err)
		}

		base := DefaultPolicy()
		base.Rec.CPUPercentile = cpuP
		base.Rec.MemoryPercentile = memP
		base.Rec.CPUHeadroom = cpuH
		base.Rec.MemoryHeadroom = memH
		base.Decision.BaseSoak = hoursToDuration(soak)

		cfg := DefaultTunerConfig()
		cfg.Enabled = true
		cfg.Envelope = env
		cfg.FullFactorial = factorial
		cfg.Steps = []Step{
			{Axis: AxisCPUPercentile, Size: stepP},
			{Axis: AxisMemoryPercentile, Size: stepP},
			{Axis: AxisCPUHeadroom, Size: stepH},
			{Axis: AxisMemoryHeadroom, Size: stepH},
			{Axis: AxisBaseSoak, Size: stepS},
		}

		cands, truncated, err := cfg.Candidates(base)
		if err != nil {
			// A step wider than its axis, or a degenerate envelope, is a
			// configuration error — a refusal, never a silent widening.
			return
		}
		if truncated < 0 {
			t.Fatalf("negative truncation count %d", truncated)
		}
		if len(cands) > maxCandidates {
			t.Fatalf("%d candidates, over the %d hard cap", len(cands), maxCandidates)
		}
		for _, p := range cands {
			if v := env.Violations(p); len(v) > 0 {
				t.Fatalf("candidate escaped the envelope: %v", v)
			}
			// And the envelope can never have been wider than the hard
			// bounds, so an in-envelope candidate is in-hard-bounds too.
			for _, a := range AllAxes {
				got := a.get(p)
				hard := hardEnvelope[a]
				if !hard.Contains(got) {
					t.Fatalf("candidate %s=%v escaped the hard bounds %s", a, got, hard)
				}
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("the grid emitted an unrunnable policy: %v", err)
			}
		}
	})
}

func clamp01(f float64) float64 {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// ---- an end-to-end nightly run ----

func enabledTuner(t *testing.T, store *Store, mut func(*TunerConfig)) *Tuner {
	t.Helper()
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	cfg.Trailing = 5 * 24 * time.Hour
	if mut != nil {
		mut(&cfg)
	}
	tn, err := NewTuner(cfg, store)
	if err != nil {
		t.Fatalf("NewTuner: %v", err)
	}
	return tn
}

func TestTunerRunFilesOnlyGatedProposals(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{
		Kind: backtest.TraceDiurnal, Days: 6, Workloads: 3, NoisePct: 0.05, NoiseSeed: 7,
	})
	store := NewStore()
	tn := enabledTuner(t, store, func(c *TunerConfig) {
		// One axis keeps the run fast and the assertions legible.
		c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: 0.05}}
	})
	scen := scenarioOver(t, tr, DefaultPolicy(), DefaultPolicy())
	rep, err := tn.Run(DefaultPolicy(), scen, tr.End, fixedClock())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Considered) == 0 {
		t.Fatal("the tuner considered nothing")
	}
	if rep.RanAt != testNow {
		t.Fatalf("RanAt = %s, want the clock's %s", rep.RanAt, testNow)
	}
	if rep.Window[1] != tr.End {
		t.Fatalf("the window must end at the history, not the wall clock: %v", rep.Window)
	}
	if len(rep.Accepted) != len(rep.Filed) {
		t.Fatalf("%d accepted but %d filed", len(rep.Accepted), len(rep.Filed))
	}
	for _, id := range rep.Filed {
		rec, ok := store.Get(id)
		if !ok {
			t.Fatalf("filed proposal %s is not in the store", id)
		}
		if rec.State() != StateGated {
			t.Fatalf("filed proposal %s is %s, want gated", id, rec.State())
		}
		if rec.Proposal().Author.Kind != ActorTuner {
			t.Fatalf("proposal author = %s, want a tuner", rec.Proposal().Author)
		}
		if _, has := rec.Approval(); has {
			t.Fatal("the tuner filed a pre-approved proposal")
		}
		if !strings.HasPrefix(rec.Proposal().Rationale, "nightly tuner:") {
			t.Fatalf("rationale = %q", rec.Proposal().Rationale)
		}
	}
	// Nothing the tuner rejected may be in the store as gated.
	for _, rec := range store.ListState(StateGated) {
		if !rec.Proposal().Gate.Passed {
			t.Fatalf("proposal %s is gated but its verdict says otherwise", rec.ID())
		}
	}
}

func TestTunerRunIsDeterministicAndIdempotent(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 5, Workloads: 2})
	mk := func() (*Store, *TunerReport) {
		store := NewStore()
		tn := enabledTuner(t, store, func(c *TunerConfig) {
			c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: 0.05}, {Axis: AxisMemoryHeadroom, Size: 0.05}}
		})
		scen := scenarioOver(t, tr, DefaultPolicy(), DefaultPolicy())
		rep, err := tn.Run(DefaultPolicy(), scen, tr.End, fixedClock())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return store, rep
	}
	s1, r1 := mk()
	s2, r2 := mk()
	b1, err := s1.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatal("two identical nightly runs produced different stores")
	}
	if len(r1.Filed) != len(r2.Filed) {
		t.Fatalf("filed %d vs %d", len(r1.Filed), len(r2.Filed))
	}
	for i := range r1.Filed {
		if r1.Filed[i] != r2.Filed[i] {
			t.Fatalf("filed ids differ at %d: %s vs %s", i, r1.Filed[i], r2.Filed[i])
		}
	}

	// Running the same night twice against one store must not duplicate.
	store, _ := mk()
	before := len(store.List())
	tn := enabledTuner(t, store, func(c *TunerConfig) {
		c.Steps = []Step{{Axis: AxisCPUHeadroom, Size: 0.05}, {Axis: AxisMemoryHeadroom, Size: 0.05}}
	})
	scen := scenarioOver(t, tr, DefaultPolicy(), DefaultPolicy())
	if _, err := tn.Run(DefaultPolicy(), scen, tr.End, fixedClock()); err != nil {
		t.Fatal(err)
	}
	if got := len(store.List()); got != before {
		t.Fatalf("a repeated run grew the store from %d to %d", before, got)
	}
}

func TestTunerNeedsHistoryNotAWallClock(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 5})
	tn := enabledTuner(t, NewStore(), nil)
	scen := scenarioOver(t, tr, DefaultPolicy(), DefaultPolicy())
	if _, err := tn.Run(DefaultPolicy(), scen, time.Time{}, fixedClock()); err == nil {
		t.Fatal("the tuner ran without an anchor in the recorded history")
	}
	if _, err := tn.Run(DefaultPolicy(), scen, tr.End, nil); err == nil {
		t.Fatal("the tuner ran without a clock")
	}
}

func TestTunerReportTotalsAreOrderIndependent(t *testing.T) {
	// The reported dollars must be a function of the multiset of accepted
	// candidates, not of the order they finished in. Magnitudes are chosen so
	// naive left-to-right addition loses the small terms in one order and
	// keeps them in another.
	terms := []float64{1e16, 1, -1e16, 1, 0.5, -0.25}
	want := sumUSD(terms)
	perms := [][]float64{
		{1, -1e16, 0.5, 1e16, -0.25, 1},
		{-0.25, 1e16, 1, 1, -1e16, 0.5},
		{0.5, 1, 1, -0.25, 1e16, -1e16},
	}
	for i, p := range perms {
		if got := sumUSD(p); got != want {
			t.Fatalf("permutation %d summed to %v, want %v", i, got, want)
		}
	}
	// And the naive sum really does disagree, so the test is not vacuous.
	naive := 0.0
	for _, v := range perms[0] {
		naive += v
	}
	if naive == want {
		t.Skip("this platform's float addition happens to be order-independent here")
	}
}
