// The other half of the proposals bucket's contract, asserted against the
// package that actually produces the bytes.
//
// checkpoint_test.go tests the framing with stand-in documents, which is the
// right level for a byte pipe. What it cannot test is that the pipe is
// SUFFICIENT: that a real whatif.Store, carrying every state a proposal can
// rest in, survives a round trip well enough to be reloaded — and that a blob
// which did NOT come out of this pipe unchanged fails to reload rather than
// reloading as something more permissive than it was.
//
// The import lives in the external test package for seams_ext_test.go's
// reason: production pkg/store imports nothing above pkg/model, and asserting
// the seam here means a signature drift in pkg/whatif is a compile error in
// this package's own test run.
package store_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/store"
	"github.com/agenticode/kilter/pkg/whatif"
)

// whatif.Store is the ProposalSnapshotter pkg/store declares. Structural, so
// neither package imports the other.
var _ store.ProposalSnapshotter = (*whatif.Store)(nil)

// No test here may read the wall clock: a proposal's identity is content-
// addressed and its expiry is clock-driven, so a test that used time.Now
// could not assert byte-identity or a deterministic set of states.
var (
	propFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	propTo   = time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)
	propNow  = time.Date(2026, 1, 8, 3, 0, 0, 0, time.UTC)
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/kilter.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// scoreFor builds a scorecard internally consistent with a policy — the same
// recipe pkg/whatif's own tests use, restated because helpers_test.go is
// internal to that package.
func scoreFor(p whatif.Policy, mut func(*backtest.Scorecard)) *backtest.Scorecard {
	sc := &backtest.Scorecard{
		Policy:                p.Hash(),
		Cluster:               "prod-east",
		Window:                [2]time.Time{propFrom, propTo},
		HorizonHours:          24,
		DecisionIntervalHours: 24,
		StarvationFactor:      1,
		Snapshots:             2016,
		Instants:              6,
		Scored:                60,
		Decisions:             12,
		Refusals:              map[string]int{backtest.CodeBelowChangeThreshold: 48},
		MemViolations:         2,
		CPUStarvation:         1,
		MemOOMKills:           3,
		OracleGapPct:          30,
		OracleGapPctApplied:   12,
		PolicyCostUSD:         100,
		OracleCostUSD:         80,
		ForgoneSavingsUSD:     5,
		FlipRate:              0.05,
		Flips:                 1,
		ResourceRegretUSD:     20,
		RiskRegretUSD:         10,
		RegretUSD:             30,
		Cost:                  backtest.DefaultCostModel(),
	}
	if mut != nil {
		mut(sc)
	}
	return sc
}

// betterCandidate is the "this deserves to pass" mutation; worseCandidate is
// its opposite, which the gate refuses and Create files as rejected.
func betterCandidate(sc *backtest.Scorecard) {
	sc.RegretUSD = 20
	sc.ResourceRegretUSD = 12
	sc.RiskRegretUSD = 8
	sc.OracleGapPct = 22
}

func worseCandidate(sc *backtest.Scorecard) {
	sc.RegretUSD = 90
	sc.ResourceRegretUSD = 60
	sc.RiskRegretUSD = 30
	sc.OracleGapPct = 55
	sc.MemOOMKills = 40
}

// specFor builds a spec whose candidate differs from the shipped policy on one
// in-envelope axis — the shape every real proposal has.
func specFor(headroom float64, pass bool, rationale string, evidence []string) whatif.Spec {
	cand := whatif.DefaultPolicy()
	cand.Rec.CPUHeadroom = headroom
	mut := betterCandidate
	if !pass {
		mut = worseCandidate
	}
	return whatif.Spec{
		Kind:           whatif.KindPolicyChange,
		Target:         whatif.Target{Cluster: "prod-east", Namespace: "payments", Class: "batch"},
		Baseline:       whatif.DefaultPolicy(),
		Candidate:      cand,
		BaselineScore:  scoreFor(whatif.DefaultPolicy(), nil),
		CandidateScore: scoreFor(cand, mut),
		Envelope:       whatif.DefaultEnvelope(),
		Tolerance:      whatif.DefaultTolerance(),
		Rationale:      rationale,
		EvidenceIDs:    evidence,
	}
}

var (
	tunerActor = whatif.Actor{Kind: whatif.ActorTuner, ID: "nightly"}
	agentActor = whatif.Actor{Kind: whatif.ActorAgent, ID: "reasoner"}
)

// populated builds a proposal store holding one record in every state a stored
// record may rest in, and returns it with the states it produced.
//
// StateDraft is absent on purpose and cannot be produced: whatif's
// UnmarshalJSON refuses a stored draft, because no record rests there.
func populated(t *testing.T) (*whatif.Store, map[string]whatif.State) {
	t.Helper()
	ws := whatif.NewStore()
	at := func(d time.Duration) whatif.Clock { return whatif.FixedClock(propNow.Add(d)) }

	// Rejected by the gate, at filing time.
	gateRejected, err := ws.Create(tunerActor, specFor(1.21, false, "regret is worse", nil), at(0))
	if err != nil {
		t.Fatal(err)
	}
	// Filed, gate passed, awaiting a human — and then rejected BY that human,
	// which is a different road to the same terminal state and a different
	// audit history.
	humanRejected, err := ws.Create(tunerActor, specFor(1.22, true, "candidate B", []string{"ev-1", "ev-2"}), at(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Reject(whatif.Actor{Kind: whatif.ActorHuman, ID: "bob"}, humanRejected.ID(), "not this quarter", at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Still gated: nobody has answered.
	gated, err := ws.Create(agentActor, specFor(1.23, true, "candidate C", []string{"ev-3"}), at(0))
	if err != nil {
		t.Fatal(err)
	}
	// Approved and then left to lapse.
	expired, err := ws.Create(tunerActor, specFor(1.24, true, "candidate D", nil), at(0))
	if err != nil {
		t.Fatal(err)
	}
	// Approved, and recorded — after the fact — as having been applied by
	// something outside this process. pkg/store neither performs nor enables
	// that; the state is here because losing "this was applied" across a
	// restart is precisely the audit-trail hole §5.3 names.
	applied, err := ws.Create(tunerActor, specFor(1.25, true, "candidate E", nil), at(0))
	if err != nil {
		t.Fatal(err)
	}
	// Approved and still live at snapshot time.
	approved, err := ws.Create(tunerActor, specFor(1.26, true, "candidate F", []string{"ev-9"}), at(0))
	if err != nil {
		t.Fatal(err)
	}

	alice, err := whatif.NewApprover(whatif.Actor{Kind: whatif.ActorHuman, ID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{expired.ID(), applied.ID()} {
		if _, err := ws.Approve(alice, id, "ship it", at(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ws.MarkApplied(whatif.Actor{Kind: whatif.ActorSystem, ID: "deploy"}, applied.ID(), "rolled out", at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Sweep past the first two approvals' TTL: the un-applied one expires, the
	// applied one is already terminal and is not touched.
	swept, err := ws.Sweep(at(whatif.DefaultApprovalTTL + 2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if swept != 1 {
		t.Fatalf("sweep moved %d records, want exactly the un-applied approval", swept)
	}
	// Approve the last one after the sweep, so it is still live in the bytes.
	if _, err := ws.Approve(alice, approved.ID(), "ship it", at(whatif.DefaultApprovalTTL+3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	want := map[string]whatif.State{
		gateRejected.ID():  whatif.StateRejected,
		humanRejected.ID(): whatif.StateRejected,
		gated.ID():         whatif.StateGated,
		expired.ID():       whatif.StateExpired,
		applied.ID():       whatif.StateApplied,
		approved.ID():      whatif.StateApproved,
	}
	assertStates(t, ws, want)
	return ws, want
}

func assertStates(t *testing.T, ws *whatif.Store, want map[string]whatif.State) {
	t.Helper()
	got := map[string]whatif.State{}
	for _, r := range ws.List() {
		got[r.ID()] = r.State()
	}
	if len(got) != len(want) {
		t.Fatalf("store holds %d records, want %d", len(got), len(want))
	}
	for id, st := range want {
		if got[id] != st {
			t.Fatalf("proposal %s is %q, want %q", id, got[id], st)
		}
	}
}

// TestProposalStoreRoundTripsBitExactWithEveryGateState is the fidelity
// assertion §5.3 needs: a brain restart must return the proposal plane it had,
// not an approximation of it. Bit-exact because anything less means the store
// is re-encoding, and a persistence layer that re-encodes a security-relevant
// record is a persistence layer that can change one.
func TestProposalStoreRoundTripsBitExactWithEveryGateState(t *testing.T) {
	s := openStore(t)
	ws, want := populated(t)

	blob, err := ws.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProposals(blob); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadProposals()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("the proposal store did not round-trip byte for byte\n got %d bytes\nwant %d bytes", len(got), len(blob))
	}

	// And the bytes are still a store: every record comes back in exactly the
	// state it was written in, including the two terminal ones and the live
	// approval. This is the property the actuator prohibition rests on — a
	// restart may not promote anything.
	reloaded, err := whatif.Load(got)
	if err != nil {
		t.Fatalf("a store this package wrote did not reload: %v", err)
	}
	assertStates(t, reloaded, want)

	// Re-snapshotting the reloaded store reproduces the same bytes, so a
	// second housekeeping tick after a restart is a no-op write rather than a
	// diff against itself.
	again, err := reloaded.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, blob) {
		t.Fatal("a reload-then-snapshot cycle changed the bytes")
	}
}

// TestAForgedApprovalDoesNotComeBackApproved is the actuator prohibition,
// stated over the bytes.
//
// pkg/store is a byte pipe and deliberately does not parse a proposal, so it
// cannot itself refuse a forgery — and it must not silently repair one either.
// The property being asserted is therefore the pair: the pipe hands back
// exactly what it was given (it neither grants nor launders capability), and
// what it was given fails to load as approved. A gated proposal edited to say
// "approved" does not become an approved proposal by being written to disk and
// read back.
func TestAForgedApprovalDoesNotComeBackApproved(t *testing.T) {
	s := openStore(t)
	ws, _ := populated(t)
	honest, err := ws.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		forge  func(string) string
		reason string
	}{
		{
			name:   "gated record relabelled approved",
			forge:  func(s string) string { return strings.Replace(s, `"state": "gated"`, `"state": "approved"`, 1) },
			reason: "approval binding",
		},
		{
			name: "approved record's approval deleted, state kept",
			forge: func(s string) string {
				// Drop the approval object from the live-approved record but
				// leave "state": "approved" in place.
				i := strings.Index(s, `"approval"`)
				if i < 0 {
					t.Fatal("fixture has no approval to strip")
				}
				j := strings.Index(s[i:], "\n    },\n")
				if j < 0 {
					t.Fatal("could not find the end of the approval object")
				}
				return s[:i] + s[i+j+len("\n    },\n")+4:]
			},
			reason: "approval binding",
		},
		{
			name: "approver rewritten to the author",
			forge: func(s string) string {
				return strings.Replace(s, `"id": "alice"`, `"id": "nightly"`, 1)
			},
			reason: "self-approval",
		},
		{
			name: "gate verdict flipped to passed",
			forge: func(s string) string {
				return strings.Replace(s, `"passed": false`, `"passed": true`, 1)
			},
			reason: "content fingerprint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged := []byte(tc.forge(string(honest)))
			if bytes.Equal(forged, honest) {
				t.Fatal("the forgery changed nothing; the test proves nothing")
			}
			if err := s.SaveProposals(forged); err != nil {
				t.Fatal(err)
			}
			got, err := s.LoadProposals()
			if err != nil {
				t.Fatal(err)
			}
			// The pipe is a pipe: no repair, no rejection, no rewriting.
			if !bytes.Equal(got, forged) {
				t.Fatal("the store rewrote the bytes it was given")
			}
			// And the forgery does not load. Not "loads without the approval",
			// not "loads as gated" — does not load.
			reloaded, err := whatif.Load(got)
			if err == nil {
				for _, r := range reloaded.List() {
					if r.State() == whatif.StateApproved || r.State() == whatif.StateApplied {
						t.Fatalf("a forged blob produced a %s record (%s)", r.State(), r.ID())
					}
				}
				t.Fatalf("a forged blob (%s) loaded", tc.reason)
			}
		})
	}
}

// TestColdBrainStartIsDistinguishableFromALostStore walks the exact branch a
// caller writes at NewBrain, and pins that the two outcomes it must tell apart
// really do arrive differently. If these ever collapsed, a brain whose file
// was truncated would come up serving an empty proposal plane and reporting
// nothing wrong — the audit trail would be gone and the only evidence of it
// would be its absence.
func TestColdBrainStartIsDistinguishableFromALostStore(t *testing.T) {
	s := openStore(t)

	blob, err := s.LoadProposals()
	if !errors.Is(err, store.ErrNoCheckpoint) {
		t.Fatalf("a first boot reported %v, want ErrNoCheckpoint", err)
	}
	if blob != nil {
		t.Fatal("a first boot returned bytes as well as an error")
	}

	ws, want := populated(t)
	if err := s.SaveProposalsFrom(ws); err != nil {
		t.Fatal(err)
	}
	blob, err = s.LoadProposals()
	if err != nil {
		t.Fatalf("a warm boot reported %v", err)
	}
	reloaded, err := whatif.Load(blob)
	if err != nil {
		t.Fatal(err)
	}
	assertStates(t, reloaded, want)

	// Now lose it. Truncating the stored value is the cheapest stand-in for
	// the half-written record a crash leaves behind.
	if err := s.SaveProposals(blob[:len(blob)/2]); err != nil {
		t.Fatal(err)
	}
	blob, err = s.LoadProposals()
	if err != nil {
		t.Fatalf("a truncated snapshot is still a well-framed one: %v", err)
	}
	if _, err := whatif.Load(blob); err == nil {
		t.Fatal("half a proposal store loaded")
	}
	// The framing layer's own corruption is the louder case, and it must not
	// look like a first boot either.
	if err := s.SaveProposals(blob); err != nil {
		t.Fatal(err)
	}
}

// TestOneRecordIsWellUnderItsShareOfTheCap is the arithmetic behind
// MaxProposalsCheckpointBytes, asserted rather than asserted-in-a-comment.
//
// The record built here holds every free-text field at pkg/whatif's own limit:
// a full-length rationale, the maximum citation set at the maximum citation
// length, and a full-length approval note. If a later change to pkg/whatif
// grows a record past its share of the cap, a full store stops being storable
// — and this fails first, in this package, instead of at 3am in a brain whose
// proposals silently stopped persisting.
func TestOneRecordIsWellUnderItsShareOfTheCap(t *testing.T) {
	const whatifMaxRecords = 1000 // pkg/whatif's unexported maxRecords

	// maxEvidenceIDs citations at maxEvidenceIDLen, and a rationale at the
	// bound that actually binds: Spec.normalize checks maxRationale (4096) and
	// then runs sanitizeNote, whose maxNote (2048) refuses first, so 2048 is
	// the largest rationale a proposal can carry.
	ids := make([]string, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%03d", strings.Repeat("e", 509), i) // maxEvidenceIDLen
	}
	ws := whatif.NewStore()
	at := func(d time.Duration) whatif.Clock { return whatif.FixedClock(propNow.Add(d)) }
	rec, err := ws.Create(tunerActor, specFor(1.28, true, strings.Repeat("r", 2048), ids), at(0))
	if err != nil {
		t.Fatal(err)
	}
	alice, err := whatif.NewApprover(whatif.Actor{Kind: whatif.ActorHuman, ID: strings.Repeat("a", 128)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Approve(alice, rec.ID(), strings.Repeat("n", 2048), at(time.Hour)); err != nil {
		t.Fatal(err)
	}
	full, err := ws.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	empty, err := whatif.NewStore().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	perRecord := len(full) - len(empty)
	t.Logf("worst-case record: %d bytes; × %d records = %d bytes; cap is %d",
		perRecord, whatifMaxRecords, perRecord*whatifMaxRecords, store.MaxProposalsCheckpointBytes)

	if perRecord*whatifMaxRecords > store.MaxProposalsCheckpointBytes {
		t.Fatalf("a full proposal store (%d records × %d bytes = %d) no longer fits under the %d-byte cap",
			whatifMaxRecords, perRecord, perRecord*whatifMaxRecords, store.MaxProposalsCheckpointBytes)
	}
	// And it does fit, framed, through the real bucket.
	s := openStore(t)
	if err := s.SaveProposals(full); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadProposals()
	if err != nil || !bytes.Equal(got, full) {
		t.Fatalf("a maximal record did not round-trip: %v", err)
	}
}

// TestHousekeepingTickShape is the call sequence PERSIST-FINDINGS.md §5 tells
// pkg/api to hang off its timer, executed here so the documented sequence is
// one that compiles and runs rather than one that reads well.
func TestHousekeepingTickShape(t *testing.T) {
	s := openStore(t)
	ws, _ := populated(t)
	tick := func(now time.Time) {
		t.Helper()
		if _, err := ws.Sweep(whatif.FixedClock(now)); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveProposalsFrom(ws); err != nil {
			t.Fatal(err)
		}
	}
	// Two ticks before the live approval lapses, one after.
	tick(propNow.Add(whatif.DefaultApprovalTTL + 4*time.Hour))
	before, err := s.LoadProposals()
	if err != nil {
		t.Fatal(err)
	}
	tick(propNow.Add(whatif.DefaultApprovalTTL + 5*time.Hour))
	if same, err := s.LoadProposals(); err != nil || !bytes.Equal(same, before) {
		t.Fatalf("a tick with nothing to sweep rewrote the snapshot: %v", err)
	}
	tick(propNow.Add(3 * whatif.DefaultApprovalTTL))
	after, err := s.LoadProposals()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("sweeping the live approval to expired did not reach the store")
	}
	reloaded, err := whatif.Load(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reloaded.List() {
		if r.State() == whatif.StateApproved {
			t.Fatalf("proposal %s is still approved after its TTL elapsed", r.ID())
		}
	}
}
