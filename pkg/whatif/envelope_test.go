package whatif

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRangeIsTotalUnderGarbage(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Range
		ok   bool
	}{
		{"ordinary", Range{Min: 1, Max: 2}, true},
		{"degenerate point", Range{Min: 1, Max: 1}, true},
		{"inverted", Range{Min: 2, Max: 1}, false},
		{"NaN min", Range{Min: math.NaN(), Max: 1}, false},
		{"NaN max", Range{Min: 1, Max: math.NaN()}, false},
		{"+Inf max", Range{Min: 1, Max: math.Inf(1)}, false},
		{"-Inf min", Range{Min: math.Inf(-1), Max: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.r.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	r := Range{Min: 1, Max: 2}
	if r.Contains(math.NaN()) {
		t.Fatal("NaN must never be contained")
	}
	// A NaN clamps to Min: the caller asked for a value on this axis and the
	// envelope's job is to produce one that is inside it.
	if got := r.Clamp(math.NaN()); got != 1 {
		t.Fatalf("Clamp(NaN) = %v, want 1", got)
	}
	if got := r.Clamp(0); got != 1 {
		t.Fatalf("Clamp(0) = %v", got)
	}
	if got := r.Clamp(9); got != 2 {
		t.Fatalf("Clamp(9) = %v", got)
	}
	if got := r.String(); got != "[1,2]" {
		t.Fatalf("String() = %q", got)
	}
}

func TestHardBoundsCannotBeWidenedByTheCaller(t *testing.T) {
	got := HardBounds()
	for a := range got {
		got[a] = Range{Min: -1e9, Max: 1e9}
	}
	// The package's own copy must be untouched.
	env := Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 0.1, Max: 9}}}
	if err := env.Validate(); err == nil {
		t.Fatal("mutating HardBounds()'s return value widened the real limits")
	}
	for a, r := range HardBounds() {
		if r != hardEnvelope[a] {
			t.Fatalf("HardBounds() no longer matches the real limits for %s", a)
		}
	}
}

func TestEnvelopeAxesAndViolationsAreOrdered(t *testing.T) {
	env := DefaultEnvelope()
	axes := env.Axes()
	if len(axes) != len(AllAxes) {
		t.Fatalf("Axes() = %v", axes)
	}
	for i := range axes {
		if axes[i] != AllAxes[i] {
			t.Fatalf("Axes() is not in AllAxes order: %v", axes)
		}
	}
	// An envelope that pins an axis simply does not list it.
	partial := Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 1.1, Max: 1.2}}}
	if got := partial.Axes(); len(got) != 1 || got[0] != AxisCPUHeadroom {
		t.Fatalf("Axes() = %v", got)
	}

	out := DefaultPolicy()
	out.Rec.CPUPercentile = 0.5
	out.Rec.MemoryHeadroom = 1.9
	out.Decision.BaseSoak = 48 * time.Hour
	v := env.Violations(out)
	if len(v) != 3 {
		t.Fatalf("Violations = %v, want 3", v)
	}
	// Reported in AllAxes order, and the soak is rendered as a duration.
	if !strings.HasPrefix(v[0], string(AxisCPUPercentile)) {
		t.Fatalf("violations out of order: %v", v)
	}
	if !strings.Contains(v[2], "48h") {
		t.Fatalf("the soak violation should read as a duration: %q", v[2])
	}
}

func TestEnvelopeClampLeavesUndeclaredAxesAlone(t *testing.T) {
	env := Envelope{Bounds: map[Axis]Range{AxisCPUHeadroom: {Min: 1.1, Max: 1.2}}}
	in := DefaultPolicy()
	in.Rec.CPUHeadroom = 1.9
	in.Rec.MemoryHeadroom = 1.9 // undeclared: must survive untouched
	out := env.Clamp(in)
	if out.Rec.CPUHeadroom != 1.2 {
		t.Fatalf("CPUHeadroom = %v, want the clamped 1.2", out.Rec.CPUHeadroom)
	}
	if out.Rec.MemoryHeadroom != 1.9 {
		t.Fatalf("an undeclared axis was moved to %v", out.Rec.MemoryHeadroom)
	}
}

func TestAxisSetAndGetRoundTrip(t *testing.T) {
	p := DefaultPolicy()
	for _, a := range AllAxes {
		before := a.get(p)
		if math.IsNaN(before) {
			t.Fatalf("axis %s has no projection", a)
		}
		q := a.set(p, before)
		if got := a.get(q); got != before {
			t.Fatalf("axis %s did not round-trip: %v → %v", a, before, got)
		}
	}
	if Axis("nonsense").Known() {
		t.Fatal("an unknown axis reported itself known")
	}
	if !math.IsNaN(Axis("nonsense").get(p)) {
		t.Fatal("an unknown axis projected a value")
	}
	// Setting an unknown axis must be a no-op, not a silent write.
	if q := Axis("nonsense").set(p, 99); q.Hash() != p.Hash() {
		t.Fatal("an unknown axis mutated the policy")
	}
}

func TestSoakQuantization(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want time.Duration
	}{
		{0, 0},
		{-5, 0},
		{math.NaN(), 0},
		{6, 6 * time.Hour},
		{2.5, 150 * time.Minute},
		{1e9, maxSoakHours * time.Hour},
	} {
		if got := hoursToDuration(tc.in); got != tc.want {
			t.Fatalf("hoursToDuration(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPolicyHelpers(t *testing.T) {
	if !(Policy{}).IsZero() {
		t.Fatal("the zero Policy must report itself zero")
	}
	if DefaultPolicy().IsZero() {
		t.Fatal("the default policy is not zero")
	}
	// withDefaults must fill each group independently.
	partial := Policy{Rec: DefaultPolicy().Rec}
	full := partial.withDefaults()
	if full.Plan != DefaultPolicy().Plan || full.Decision != DefaultPolicy().Decision {
		t.Fatal("withDefaults left a group unset")
	}
	if full.Rec != partial.Rec {
		t.Fatal("withDefaults overwrote a set group")
	}
	// Hash must agree with the scorecard's own notion of policy identity.
	if DefaultPolicy().Hash() != scoreFor(DefaultPolicy(), nil).Policy {
		t.Fatal("Policy.Hash and backtest.PolicyHash disagree")
	}
}

func TestPolicyValidationCoversThePlanFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Policy)
	}{
		{"plan confidence", func(p *Policy) { p.Plan.MinConfidence = 1.5 }},
		{"plan confidence NaN", func(p *Policy) { p.Plan.MinConfidence = math.NaN() }},
		{"plan utilization", func(p *Policy) { p.Plan.MinNodeUtilization = -1 }},
		{"plan headroom", func(p *Policy) { p.Plan.MinClusterHeadroom = 2 }},
		{"plan removals", func(p *Policy) { p.Plan.MaxNodeRemovals = -1 }},
		{"recommend percentile", func(p *Policy) { p.Rec.CPUPercentile = 0 }},
		{"decision soak", func(p *Policy) { p.Decision.MinClassStability = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPolicy()
			tc.mut(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("an invalid policy validated")
			}
		})
	}
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("the shipped policy must validate: %v", err)
	}
}

func TestTargetStringAndValidation(t *testing.T) {
	tg := Target{Cluster: "c", Namespace: "ns", Class: "steady"}
	if got := tg.String(); got != "c/ns#steady" {
		t.Fatalf("String() = %q", got)
	}
	if err := tg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Target{Cluster: "   "}).Validate(); err == nil {
		t.Fatal("a blank cluster validated")
	}
	if err := (Target{Cluster: strings.Repeat("c", maxActorID+1)}).Validate(); err == nil {
		t.Fatal("an oversized cluster validated")
	}
}

func TestApprovalMarshalsAndZeroIsNull(t *testing.T) {
	var zero Approval
	b, err := json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("a zero approval marshalled as %s", b)
	}
	_, rec := approvedRecord(t)
	ap, _ := rec.Approval()
	b, err = json.Marshal(ap)
	if err != nil {
		t.Fatal(err)
	}
	var w approvalWire
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.By.ID != "bob" || w.Fingerprint != rec.ID() {
		t.Fatalf("marshalled approval = %+v", w)
	}
	if got := (&Approver{actor: Actor{Kind: ActorHuman, ID: "bob"}}).Actor(); got.ID != "bob" {
		t.Fatalf("Approver.Actor() = %+v", got)
	}
}

func TestRejectClosesAProposalAndDropsItsApproval(t *testing.T) {
	s, rec := approvedRecord(t)
	got, err := s.Reject(Actor{Kind: ActorHuman, ID: "carol"}, rec.ID(), "changed our minds", fixedClock())
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if got.State() != StateRejected {
		t.Fatalf("state = %s", got.State())
	}
	if _, has := got.Approval(); has {
		t.Fatal("a rejected proposal must not keep its approval")
	}
	if _, err := s.Reject(Actor{Kind: ActorHuman, ID: "carol"}, rec.ID(), "", fixedClock()); err == nil {
		t.Fatal("rejected is terminal")
	}
	if _, err := s.Reject(Actor{Kind: "root", ID: "x"}, rec.ID(), "", fixedClock()); err == nil {
		t.Fatal("an invalid actor rejected a proposal")
	}
	if _, err := s.Reject(Actor{Kind: ActorHuman, ID: "carol"}, rec.ID(), "", nil); err == nil {
		t.Fatal("Reject ran without a clock")
	}
	// The history records every move, in order.
	h := got.History()
	if len(h) < 3 || h[len(h)-1].To != StateRejected || h[len(h)-1].By.ID != "carol" {
		t.Fatalf("history = %+v", h)
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []State{StateRejected, StateApplied, StateExpired} {
		if !terminal(s) {
			t.Fatalf("%s should be terminal", s)
		}
		if len(transitions[s]) != 0 {
			t.Fatalf("%s is terminal but has outgoing edges %v", s, transitions[s])
		}
	}
	for _, s := range []State{StateDraft, StateGated, StateApproved} {
		if terminal(s) {
			t.Fatalf("%s should not be terminal", s)
		}
	}
	if knownState("nonsense") {
		t.Fatal("an unknown state reported itself known")
	}
}

func TestStoreEvictsOnlyTerminalRecords(t *testing.T) {
	s := NewStore()
	// Fill with gated (non-terminal) records: eviction must refuse rather
	// than drop somebody's pending approval.
	base := passingSpec()
	for i := 0; i < maxRecords; i++ {
		sp := base
		sp.Candidate = DefaultPolicy()
		sp.Candidate.Rec.MinMilliCPU = int64(11 + i)
		sp.CandidateScore = scoreFor(sp.Candidate, better)
		if _, err := s.Create(Actor{Kind: ActorTuner, ID: "nightly"}, sp,
			FixedClock(testNow.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if got := len(s.List()); got != maxRecords {
		t.Fatalf("store holds %d records", got)
	}
	sp := base
	sp.Candidate = DefaultPolicy()
	sp.Candidate.Rec.MinMilliCPU = 99999
	sp.CandidateScore = scoreFor(sp.Candidate, better)
	if _, err := s.Create(Actor{Kind: ActorTuner, ID: "nightly"}, sp, fixedClock()); err == nil {
		t.Fatal("a full store of pending proposals accepted another write")
	}

	// Once one is terminal, the oldest terminal record makes room.
	victim := s.List()[0]
	if _, err := s.Reject(Actor{Kind: ActorHuman, ID: "carol"}, victim.ID(), "no", fixedClock()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Actor{Kind: ActorTuner, ID: "nightly"}, sp, fixedClock()); err != nil {
		t.Fatalf("a store with a terminal record refused a write: %v", err)
	}
	if _, still := s.Get(victim.ID()); still {
		t.Fatal("the evicted record is still present")
	}
	if got := len(s.List()); got != maxRecords {
		t.Fatalf("store holds %d records after eviction", got)
	}
}

func TestTunerConfigAccessor(t *testing.T) {
	cfg := DefaultTunerConfig()
	cfg.Enabled = true
	cfg.MaxCandidates = 5
	tn, err := NewTuner(cfg, NewStore())
	if err != nil {
		t.Fatal(err)
	}
	if got := tn.Config(); got.MaxCandidates != 5 || !got.Enabled {
		t.Fatalf("Config() = %+v", got)
	}
	if _, err := NewTuner(cfg, nil); err == nil {
		t.Fatal("a tuner was built without a store")
	}
	var nilTuner *Tuner
	if nilTuner.Enabled() {
		t.Fatal("a nil tuner reported itself enabled")
	}
	if _, err := nilTuner.Run(DefaultPolicy(), Scenario{}, testTo, fixedClock()); err == nil {
		t.Fatal("a nil tuner ran")
	}
}

func TestResultSpecValidation(t *testing.T) {
	var nilResult *Result
	if _, err := nilResult.Spec(Target{}, DefaultEnvelope(), DefaultTolerance(), "", nil); err == nil {
		t.Fatal("a nil result produced a spec")
	}
	if _, err := (&Result{}).Spec(Target{}, DefaultEnvelope(), DefaultTolerance(), "", nil); err == nil {
		t.Fatal("a result with no scorecards produced a spec")
	}
	r := &Result{
		Cluster:        "c1",
		Baseline:       baselinePolicy(),
		Candidate:      candidatePolicy(),
		BaselineScore:  scoreFor(baselinePolicy(), nil),
		CandidateScore: scoreFor(candidatePolicy(), better),
		Gate:           GateResult{Passed: true},
	}
	if !r.Improved() {
		t.Fatal("Improved() disagrees with the gate")
	}
	spec, err := r.Spec(Target{}, DefaultEnvelope(), DefaultTolerance(), "why", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Target.Cluster != "c1" {
		t.Fatalf("Spec did not default the cluster: %+v", spec.Target)
	}
	// The verdict does not travel: the store must recompute it.
	if spec.BaselineScore != r.BaselineScore || spec.CandidateScore != r.CandidateScore {
		t.Fatal("Spec must hand over the scorecards themselves")
	}
	if nilResult.Improved() {
		t.Fatal("a nil result reported an improvement")
	}
}
