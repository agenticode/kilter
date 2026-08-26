package whatif

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func approvedRecord(t *testing.T) (*Store, *Record) {
	t.Helper()
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	ap, err := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Approve(ap, rec.ID(), "ship it", fixedClock())
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return s, got
}

// TestOnlyGatedAndApprovedReachesApplied is INV-4: the single path to applied
// runs through the gate and a human approval. The table is exhaustive over
// (state × transition), so a future edge added to `transitions` without a
// deliberate decision fails here.
func TestOnlyGatedAndApprovedReachesApplied(t *testing.T) {
	states := []State{StateDraft, StateGated, StateRejected, StateApproved, StateApplied, StateExpired}
	want := map[State]map[State]bool{
		StateDraft:    {StateGated: true, StateRejected: true},
		StateGated:    {StateApproved: true, StateRejected: true},
		StateApproved: {StateApplied: true, StateExpired: true, StateRejected: true},
		StateRejected: {},
		StateApplied:  {},
		StateExpired:  {},
	}
	for _, from := range states {
		for _, to := range states {
			got := canTransition(from, to)
			if got != want[from][to] {
				t.Fatalf("%s → %s: canTransition = %v, want %v", from, to, got, want[from][to])
			}
		}
	}
	// The property, stated directly rather than read off the table.
	for _, from := range states {
		if from != StateApproved && canTransition(from, StateApplied) {
			t.Fatalf("%s → applied is reachable; only approved may be applied", from)
		}
		if from != StateGated && canTransition(from, StateApproved) {
			t.Fatalf("%s → approved is reachable; only a gated proposal may be approved", from)
		}
	}
}

func TestAppliedRequiresALiveApproval(t *testing.T) {
	t.Run("gated is not enough", func(t *testing.T) {
		s := NewStore()
		rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
		_, err := s.MarkApplied(Actor{Kind: ActorSystem, ID: "brain"}, rec.ID(), "", fixedClock())
		if err == nil || !strings.Contains(err.Error(), "only an approved proposal") {
			t.Fatalf("want a refusal naming approval, got %v", err)
		}
	})
	t.Run("approved works once", func(t *testing.T) {
		s, rec := approvedRecord(t)
		got, err := s.MarkApplied(Actor{Kind: ActorSystem, ID: "brain"}, rec.ID(), "config written", fixedClock())
		if err != nil {
			t.Fatalf("MarkApplied: %v", err)
		}
		if got.State() != StateApplied {
			t.Fatalf("state = %s", got.State())
		}
		if _, err := s.MarkApplied(Actor{Kind: ActorSystem, ID: "brain"}, rec.ID(), "", fixedClock()); err == nil {
			t.Fatal("applied is terminal; a second apply must be refused")
		}
	})
	t.Run("an expired approval cannot be applied", func(t *testing.T) {
		s, rec := approvedRecord(t)
		late := FixedClock(testNow.Add(DefaultApprovalTTL + time.Minute))
		if _, err := s.MarkApplied(Actor{Kind: ActorSystem, ID: "brain"}, rec.ID(), "", late); err == nil {
			t.Fatal("an expired approval must not be applicable")
		}
		got, _ := s.Get(rec.ID())
		if got.State() != StateExpired {
			t.Fatalf("the record should have moved to expired, got %s", got.State())
		}
	})
}

func TestSweepExpiresApprovals(t *testing.T) {
	s, rec := approvedRecord(t)
	if n, err := s.Sweep(fixedClock()); err != nil || n != 0 {
		t.Fatalf("Sweep before expiry moved %d records (err %v)", n, err)
	}
	late := FixedClock(testNow.Add(DefaultApprovalTTL + time.Second))
	n, err := s.Sweep(late)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1, nil", n, err)
	}
	got, _ := s.Get(rec.ID())
	if got.State() != StateExpired {
		t.Fatalf("state = %s", got.State())
	}
	if n, _ := s.Sweep(late); n != 0 {
		t.Fatalf("a second sweep moved %d records", n)
	}
}

func TestCreateIsIdempotentOnIdenticalSpecs(t *testing.T) {
	s := NewStore()
	author := Actor{Kind: ActorTuner, ID: "nightly"}
	a := mustCreate(t, s, author)
	b := mustCreate(t, s, author)
	if a.ID() != b.ID() {
		t.Fatalf("identical specs produced two ids: %s vs %s", a.ID(), b.ID())
	}
	if len(s.List()) != 1 {
		t.Fatalf("store holds %d records, want 1", len(s.List()))
	}
	// A different author is a different proposal: the author is inside the
	// fingerprint, so an approval can never be replayed across authors.
	c := mustCreate(t, s, Actor{Kind: ActorAgent, ID: "reasoner"})
	if c.ID() == a.ID() {
		t.Fatal("two authors must not share a proposal identity")
	}
}

func TestRecordsHandedOutAreCopies(t *testing.T) {
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	p := rec.Proposal()
	p.Rationale = "tampered"
	if len(p.Changes) > 0 {
		p.Changes[0].Axis = "tampered"
	}
	h := rec.History()
	if len(h) > 0 {
		h[0].By = Actor{Kind: ActorHuman, ID: "mallory"}
	}
	again, _ := s.Get(rec.ID())
	if again.Proposal().Rationale == "tampered" {
		t.Fatal("mutating a returned proposal reached into the store")
	}
	if got := again.Proposal().Changes; len(got) > 0 && got[0].Axis == "tampered" {
		t.Fatal("mutating a returned change slice reached into the store")
	}
	if got := again.History(); len(got) > 0 && got[0].By.ID == "mallory" {
		t.Fatal("mutating a returned history reached into the store")
	}
}

// ---- persistence and tampering ----

func TestSnapshotRoundTrips(t *testing.T) {
	s, _ := approvedRecord(t)
	if _, err := s.Create(Actor{Kind: ActorAgent, ID: "reasoner"}, func() Spec {
		sp := passingSpec()
		sp.CandidateScore = scoreFor(candidatePolicy(), nil) // rejected
		return sp
	}(), fixedClock()); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	back, err := Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	again, err := back.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(again) {
		t.Fatalf("snapshot is not stable across a round trip:\n%s\n---\n%s", raw, again)
	}
	if len(back.List()) != len(s.List()) {
		t.Fatalf("round trip lost records: %d vs %d", len(back.List()), len(s.List()))
	}
}

func TestSnapshotIsByteIdenticalForIdenticalState(t *testing.T) {
	build := func() *Store {
		s, _ := approvedRecord(t)
		return s
	}
	first, err := build().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := build().Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("snapshot %d differed:\n%s\n---\n%s", i, first, got)
		}
	}
}

// TestATamperedStoreDoesNotLoad is the second lock: Approval cannot be forged
// in Go, but a file is bytes. Each case edits a persisted store the way an
// attacker with write access to the state file would.
func TestATamperedStoreDoesNotLoad(t *testing.T) {
	s, rec := approvedRecord(t)
	raw, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	author := rec.Proposal().Author

	for _, tc := range []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			name: "author becomes the approver",
			edit: func(s string) string {
				return strings.Replace(s, `"id": "`+author.ID+`"`, `"id": "bob"`, 1)
			},
			want: "does not match its contents",
		},
		{
			name: "approver becomes the author",
			edit: func(s string) string {
				return strings.Replace(s, `"id": "bob"`, `"id": "`+author.ID+`"`, 1)
			},
			want: "approved by its author",
		},
		{
			name: "a non-human approver",
			edit: func(s string) string {
				return strings.Replace(s, `"kind": "human"`, `"kind": "agent"`, 1)
			},
			want: "only a human actor may approve",
		},
		{
			name: "the gate verdict is flipped to passed",
			edit: func(s string) string {
				return strings.Replace(s, `"passed": true`, `"passed": false`, 1)
			},
			want: "does not match its contents",
		},
		{
			name: "the regret numbers are improved",
			edit: func(s string) string {
				return strings.Replace(s, `"candidateRegretUSD": 20`, `"candidateRegretUSD": 1`, 1)
			},
			want: "does not match its contents",
		},
		{
			name: "an unknown field is added",
			edit: func(s string) string {
				return strings.Replace(s, `"state": "approved"`, `"state": "approved", "override": true`, 1)
			},
			want: "unknown field",
		},
		{
			name: "an unknown state",
			edit: func(s string) string {
				return strings.Replace(s, `"state": "approved"`, `"state": "superapproved"`, 1)
			},
			want: "unknown proposal state",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edited := tc.edit(string(raw))
			if edited == string(raw) {
				t.Fatalf("the edit did not apply; fixture is stale:\n%s", raw)
			}
			_, err := Load([]byte(edited))
			if err == nil {
				t.Fatal("a tampered store loaded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestAnApprovedStateCannotBeAssertedInStorage is the direct attack: take a
// gated record and simply write "approved" into the file.
func TestAnApprovedStateCannotBeAssertedInStorage(t *testing.T) {
	s := NewStore()
	mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	raw, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []State{StateApproved, StateApplied} {
		edited := strings.Replace(string(raw), `"state": "gated"`, `"state": "`+string(state)+`"`, 1)
		if _, err := Load([]byte(edited)); err == nil {
			t.Fatalf("a gated record was promoted to %s by editing the file", state)
		} else if !strings.Contains(err.Error(), "carries no approval") {
			t.Fatalf("error %q should name the missing approval", err)
		}
	}
	// And the momentary draft state is not storable at all.
	edited := strings.Replace(string(raw), `"state": "gated"`, `"state": "draft"`, 1)
	if _, err := Load([]byte(edited)); err == nil {
		t.Fatal("draft must not be a storable resting state")
	}
}

func TestLoadRejectsGarbage(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"not json", "{"},
		{"wrong version", `{"version":99,"ttlSeconds":86400,"records":[]}`},
		{"zero ttl", `{"version":1,"ttlSeconds":0,"records":[]}`},
		{"huge ttl", `{"version":1,"ttlSeconds":99999999,"records":[]}`},
		{"nil record", `{"version":1,"ttlSeconds":86400,"records":[null]}`},
		{"unknown top-level field", `{"version":1,"ttlSeconds":86400,"records":[],"admin":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load([]byte(tc.raw)); err == nil {
				t.Fatal("garbage loaded")
			}
		})
	}
}

func TestRecordMarshalsWithItsID(t *testing.T) {
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		ID    string `json:"id"`
		State State  `json:"state"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.ID != rec.ID() || w.State != StateGated {
		t.Fatalf("marshalled record = %+v", w)
	}
}

// ---- misc ----

func TestCreateRejectsAnUnauditableSpec(t *testing.T) {
	s := NewStore()
	author := Actor{Kind: ActorTuner, ID: "nightly"}
	for _, tc := range []struct {
		name string
		mut  func(*Spec)
	}{
		{"no cluster", func(sp *Spec) { sp.Target.Cluster = "" }},
		{"cluster mismatch", func(sp *Spec) { sp.Target.Cluster = "elsewhere" }},
		{"no baseline scorecard", func(sp *Spec) { sp.BaselineScore = nil }},
		{"annotation kind", func(sp *Spec) { sp.Kind = KindAnnotationChange }},
		{"unknown kind", func(sp *Spec) { sp.Kind = "delete-everything" }},
		{"oversized rationale", func(sp *Spec) { sp.Rationale = strings.Repeat("x", maxRationale+1) }},
		{"too many citations", func(sp *Spec) {
			for i := 0; i < maxEvidenceIDs+1; i++ {
				sp.EvidenceIDs = append(sp.EvidenceIDs, string(rune('a'+i%26))+strings.Repeat("z", i))
			}
		}},
		{"control chars in target", func(sp *Spec) { sp.Target.Namespace = "ns\nfake" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := passingSpec()
			tc.mut(&sp)
			if _, err := s.Create(author, sp, fixedClock()); err == nil {
				t.Fatal("an unauditable spec was accepted")
			}
		})
	}
	if _, err := s.Create(Actor{Kind: "root", ID: "x"}, passingSpec(), fixedClock()); err == nil {
		t.Fatal("an invalid author was accepted")
	}
	if _, err := s.Create(author, passingSpec(), nil); err == nil {
		t.Fatal("a nil clock was accepted")
	}
}

func TestEvidenceIDsAreSortedAndDeduped(t *testing.T) {
	s := NewStore()
	sp := passingSpec()
	sp.EvidenceIDs = []string{"ev:b", "ev:a", "ev:b", "", "ev:c"}
	rec, err := s.Create(Actor{Kind: ActorAgent, ID: "reasoner"}, sp, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	got := rec.Proposal().EvidenceIDs
	want := []string{"ev:a", "ev:b", "ev:c"}
	if len(got) != len(want) {
		t.Fatalf("evidence ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evidence ids = %v, want %v", got, want)
		}
	}
	// The same set in a different order must be the same proposal.
	sp2 := passingSpec()
	sp2.EvidenceIDs = []string{"ev:c", "ev:a", "ev:b"}
	rec2, err := s.Create(Actor{Kind: ActorAgent, ID: "reasoner"}, sp2, fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	if rec2.ID() != rec.ID() {
		t.Fatalf("citation order changed the proposal identity: %s vs %s", rec.ID(), rec2.ID())
	}
}

func TestGetAndListOnAnUnknownID(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get invented a record")
	}
	if _, err := s.Approve(&Approver{actor: Actor{Kind: ActorHuman, ID: "bob"}}, "nope", "", fixedClock()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.Reject(Actor{Kind: ActorHuman, ID: "bob"}, "nope", "", fixedClock()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.MarkApplied(Actor{Kind: ActorSystem, ID: "brain"}, "nope", "", fixedClock()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListIsOrderedAndFiltered(t *testing.T) {
	s := NewStore()
	base := passingSpec()
	for i, h := range []float64{1.18, 1.20, 1.22, 1.24} {
		sp := base
		sp.Candidate = DefaultPolicy()
		sp.Candidate.Rec.CPUHeadroom = h
		sp.CandidateScore = scoreFor(sp.Candidate, better)
		clock := FixedClock(testNow.Add(time.Duration(i) * time.Minute))
		if _, err := s.Create(Actor{Kind: ActorTuner, ID: "nightly"}, sp, clock); err != nil {
			t.Fatal(err)
		}
	}
	list := s.List()
	if len(list) != 4 {
		t.Fatalf("%d records", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].Proposal().CreatedAt.Before(list[i-1].Proposal().CreatedAt) {
			t.Fatal("List is not oldest-first")
		}
	}
	if got := len(s.ListState(StateGated)); got != 4 {
		t.Fatalf("ListState(gated) = %d", got)
	}
	if got := len(s.ListState(StateApplied)); got != 0 {
		t.Fatalf("ListState(applied) = %d", got)
	}
}

func TestStoreIsSafeUnderConcurrency(t *testing.T) {
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	ap, _ := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})

	var wg sync.WaitGroup
	var mu sync.Mutex
	approved := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				if _, err := s.Approve(ap, rec.ID(), "", fixedClock()); err == nil {
					mu.Lock()
					approved++
					mu.Unlock()
				}
			case 1:
				s.List()
			case 2:
				s.Get(rec.ID())
			case 3:
				s.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	if approved != 1 {
		t.Fatalf("%d concurrent approvals succeeded, want exactly 1", approved)
	}
}
