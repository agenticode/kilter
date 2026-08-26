package whatif

import (
	"reflect"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
)

// trace builds a synthetic history through pkg/backtest's own generator, so
// what-if is exercised against the same fixtures the harness is tested with
// rather than a private trace generator that might drift from it.
func trace(t *testing.T, spec backtest.TraceSpec) *backtest.Trace {
	t.Helper()
	if spec.Start.IsZero() {
		spec.Start = testFrom
	}
	tr, err := spec.Build()
	if err != nil {
		t.Fatalf("building trace: %v", err)
	}
	return tr
}

// scenarioOver wires a trace into a Scenario. Every field except the policies
// comes from one place, which is the property Run relies on.
func scenarioOver(t *testing.T, tr *backtest.Trace, base, cand Policy) Scenario {
	t.Helper()
	store, err := tr.Store()
	if err != nil {
		t.Fatalf("building evidence: %v", err)
	}
	return Scenario{
		Cluster:   tr.Cluster,
		From:      tr.Start,
		To:        tr.End,
		Horizon:   24 * time.Hour,
		Baseline:  base,
		Candidate: cand,
		History:   tr.Source(),
		Evidence:  store,
		Envelope:  DefaultEnvelope(),
		Tolerance: DefaultTolerance(),
	}
}

func TestWhatIfRunsBothPoliciesThroughTheRealHarness(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceDiurnal, Days: 5, Workloads: 3})
	res, err := scenarioOver(t, tr, baselinePolicy(), candidatePolicy()).Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BaselineScore == nil || res.CandidateScore == nil {
		t.Fatal("a result must carry both scorecards")
	}
	if res.BaselineScore.Policy != baselinePolicy().Hash() {
		t.Fatalf("baseline scorecard is for %s, want %s",
			res.BaselineScore.Policy, baselinePolicy().Hash())
	}
	if res.CandidateScore.Policy != candidatePolicy().Hash() {
		t.Fatalf("candidate scorecard is for %s, want %s",
			res.CandidateScore.Policy, candidatePolicy().Hash())
	}
	if res.BaselineScore.Scored == 0 {
		t.Fatal("the fixture scored nothing; the test would be vacuous")
	}
	if len(res.Changes) != 1 || res.Changes[0].Axis != AxisCPUHeadroom {
		t.Fatalf("changes = %+v, want one cpu-headroom move", res.Changes)
	}
}

// TestBothSidesAreScoredByTheSameYardstick is the independence guarantee,
// asserted structurally: the two harnesses differ in the policy triple and in
// nothing else. If somebody adds a field to backtest.Harness and Scenario
// starts varying it between the runs, this fails.
func TestBothSidesAreScoredByTheSameYardstick(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 3})
	s := scenarioOver(t, tr, baselinePolicy(), candidatePolicy())

	a := s.harness(s.Baseline)
	b := s.harness(s.Candidate)
	if reflect.DeepEqual(a.Rec, b.Rec) {
		t.Fatal("fixture is wrong: the two policies must differ")
	}
	// Blank the policy triple — everything that remains is the yardstick.
	zero := func(h *backtest.Harness) *backtest.Harness {
		c := *h
		c.Rec, c.Plan, c.Decision = baselinePolicy().Rec, baselinePolicy().Plan, baselinePolicy().Decision
		return &c
	}
	if !reflect.DeepEqual(zero(a), zero(b)) {
		t.Fatalf("the two runs do not share a yardstick:\n%+v\n%+v", zero(a), zero(b))
	}

	// The harness struct must not have grown a field Scenario forgets to
	// share. Rec, Plan and Decision are the policy; everything else must be
	// copied from the Scenario, and this count is the tripwire.
	const knownHarnessFields = 8
	if got := reflect.TypeOf(backtest.Harness{}).NumField(); got != knownHarnessFields {
		t.Fatalf("backtest.Harness has %d fields, expected %d: a new field must be "+
			"deliberately shared between the two runs or deliberately made policy",
			got, knownHarnessFields)
	}
}

// TestTheOracleIsIndependentOfThePolicyUnderTest is the "do not grade the
// recommender with itself" assertion at this package's boundary. pkg/backtest
// guarantees it internally; this proves the what-if path did not undo it by
// scoring the two policies over different ground truth.
func TestTheOracleIsIndependentOfThePolicyUnderTest(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceBursty, Days: 5, Workloads: 2})

	// Two candidates that are as different as the envelope allows.
	aggressive := DefaultPolicy()
	aggressive.Rec.CPUPercentile = 0.80
	aggressive.Rec.CPUHeadroom = 1.05
	aggressive.Rec.MemoryHeadroom = 1.05
	conservative := DefaultPolicy()
	conservative.Rec.CPUPercentile = 0.99
	conservative.Rec.CPUHeadroom = 1.50
	conservative.Rec.MemoryHeadroom = 1.50

	res, err := scenarioOver(t, tr, aggressive, conservative).Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.BaselineScore.OracleCostUSD != res.CandidateScore.OracleCostUSD {
		t.Fatalf("the oracle moved with the policy: $%v vs $%v — the evaluation is "+
			"tracing back to the thing under test",
			res.BaselineScore.OracleCostUSD, res.CandidateScore.OracleCostUSD)
	}
	if res.BaselineScore.Scored != res.CandidateScore.Scored {
		t.Fatalf("the scored set moved with the policy: %d vs %d",
			res.BaselineScore.Scored, res.CandidateScore.Scored)
	}
	if res.BaselineScore.MemOOMKills != res.CandidateScore.MemOOMKills {
		t.Fatalf("recorded ground truth moved with the policy: %d vs %d",
			res.BaselineScore.MemOOMKills, res.CandidateScore.MemOOMKills)
	}
	if res.BaselineScore.Snapshots != res.CandidateScore.Snapshots ||
		res.BaselineScore.Instants != res.CandidateScore.Instants {
		t.Fatal("the two runs did not replay the same history")
	}
	if res.BaselineScore.Policy == res.CandidateScore.Policy {
		t.Fatal("fixture is wrong: the policies must differ")
	}
}

func TestWhatIfIsDeterministic(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{
		Kind: backtest.TraceRegimeChange, Days: 6, Workloads: 3,
		NoisePct: 0.07, NoiseSeed: 42,
		OOMAt:    []time.Duration{30 * time.Hour},
		DeployAt: []time.Duration{12 * time.Hour},
	})
	run := func() []byte {
		res, err := scenarioOver(t, tr, baselinePolicy(), candidatePolicy()).Run()
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		b, err := res.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return b
	}
	first := run()
	for i := 0; i < 5; i++ {
		if got := run(); string(got) != string(first) {
			t.Fatalf("run %d produced different bytes", i)
		}
	}
}

// TestWhatIfDoesNotReadTheClock: the same history replayed at two different
// wall-clock moments must produce identical bytes. Scenario takes no Clock at
// all, which is the strongest form of the guarantee, and this asserts the
// window is not being derived from one behind the scenes.
func TestWhatIfIgnoresTheWallClock(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 4})
	s := scenarioOver(t, tr, baselinePolicy(), candidatePolicy())
	a, err := s.Run()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	b, err := s.Run()
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := a.Encode()
	bb, _ := b.Encode()
	if string(ab) != string(bb) {
		t.Fatal("two runs at different wall-clock instants differed")
	}
}

func TestScenarioValidation(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 3})
	good := scenarioOver(t, tr, baselinePolicy(), candidatePolicy())
	for _, tc := range []struct {
		name string
		mut  func(*Scenario)
	}{
		{"no cluster", func(s *Scenario) { s.Cluster = "" }},
		{"no history", func(s *Scenario) { s.History = nil }},
		{"no window", func(s *Scenario) { s.From, s.To = time.Time{}, time.Time{} }},
		{"inverted window", func(s *Scenario) { s.From, s.To = s.To, s.From }},
		{"candidate == baseline", func(s *Scenario) { s.Candidate = s.Baseline }},
		{"invalid candidate", func(s *Scenario) { s.Candidate.Rec.CPUPercentile = 5 }},
		{"invalid baseline", func(s *Scenario) { s.Baseline.Rec.MinWindow = -time.Hour }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := good
			tc.mut(&s)
			if _, err := s.Run(); err == nil {
				t.Fatal("an invalid scenario ran")
			}
		})
	}
}

// TestWhatIfResultFilesAsAProposal closes the loop: run a what-if, hand the
// evidence to the store, and get a gated proposal whose verdict the store
// computed for itself.
func TestWhatIfResultFilesAsAProposal(t *testing.T) {
	tr := trace(t, backtest.TraceSpec{Kind: backtest.TraceSteady, Days: 5, Workloads: 2})
	// A candidate with more memory headroom on an over-provisioned steady
	// trace: whether it wins is up to the numbers, which is the point.
	cand := DefaultPolicy()
	cand.Rec.MemoryHeadroom = 1.25
	res, err := scenarioOver(t, tr, baselinePolicy(), cand).Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	spec, err := res.Spec(Target{Cluster: tr.Cluster}, DefaultEnvelope(), DefaultTolerance(),
		"exploring memory headroom", []string{"ev:1"})
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	s := NewStore()
	rec, err := s.Create(Actor{Kind: ActorAgent, ID: "reasoner"}, spec, fixedClock())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The store's verdict must match the what-if's: they are the same
	// function over the same evidence.
	if rec.Proposal().Gate.Passed != res.Gate.Passed {
		t.Fatalf("the store gated %v, the what-if gated %v",
			rec.Proposal().Gate.Passed, res.Gate.Passed)
	}
	wantState := StateRejected
	if res.Gate.Passed {
		wantState = StateGated
	}
	if rec.State() != wantState {
		t.Fatalf("state = %s, want %s", rec.State(), wantState)
	}
}
