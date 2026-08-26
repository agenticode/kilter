package whatif

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"time"
)

func passingSpec() Spec {
	return Spec{
		Kind:           KindPolicyChange,
		Target:         Target{Cluster: "c1"},
		Baseline:       baselinePolicy(),
		Candidate:      candidatePolicy(),
		BaselineScore:  scoreFor(baselinePolicy(), nil),
		CandidateScore: scoreFor(candidatePolicy(), better),
		Envelope:       DefaultEnvelope(),
		Tolerance:      DefaultTolerance(),
		Rationale:      "cpu headroom 1.15 → 1.20 removes two throttle events",
	}
}

func mustCreate(t *testing.T, s *Store, author Actor) *Record {
	t.Helper()
	rec, err := s.Create(author, passingSpec(), fixedClock())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.State() != StateGated {
		t.Fatalf("expected a gated proposal, got %s (%v)", rec.State(), rec.Proposal().Gate.Reasons)
	}
	return rec
}

// ---- the headline property ----

// TestSelfApprovalIsUnrepresentable is the test the brief asks for: try every
// route a hostile caller has to approving its own proposal, and fail on each.
func TestSelfApprovalIsUnrepresentable(t *testing.T) {
	author := Actor{Kind: ActorHuman, ID: "alice"}

	t.Run("the author approves directly", func(t *testing.T) {
		s := NewStore()
		rec := mustCreate(t, s, author)
		ap, err := NewApprover(author)
		if err != nil {
			t.Fatalf("alice is a human and must be able to hold the capability: %v", err)
		}
		if _, err := s.Approve(ap, rec.ID(), "lgtm", fixedClock()); !errors.Is(err, ErrSelfApproval) {
			t.Fatalf("want ErrSelfApproval, got %v", err)
		}
		if got, _ := s.Get(rec.ID()); got.State() != StateGated {
			t.Fatalf("a refused approval must not move the record, got %s", got.State())
		}
	})

	t.Run("the author changes kind and approves", func(t *testing.T) {
		// "agent:alice" files it, "human:alice" tries to approve. Same
		// person, different costume — sameIdentity ignores Kind for exactly
		// this reason.
		s := NewStore()
		rec := mustCreate(t, s, Actor{Kind: ActorAgent, ID: "alice"})
		ap, err := NewApprover(Actor{Kind: ActorHuman, ID: "Alice"}) // and different case
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Approve(ap, rec.ID(), "", fixedClock()); !errors.Is(err, ErrSelfApproval) {
			t.Fatalf("want ErrSelfApproval, got %v", err)
		}
	})

	t.Run("a non-human cannot become an approver at all", func(t *testing.T) {
		for _, k := range []ActorKind{ActorAgent, ActorTuner, ActorSystem} {
			if _, err := NewApprover(Actor{Kind: k, ID: "x"}); !errors.Is(err, ErrNotAnApprover) {
				t.Fatalf("kind %s: want ErrNotAnApprover, got %v", k, err)
			}
		}
	})

	t.Run("a nil approver is not a capability", func(t *testing.T) {
		s := NewStore()
		rec := mustCreate(t, s, author)
		if _, err := s.Approve(nil, rec.ID(), "", fixedClock()); !errors.Is(err, ErrNotAnApprover) {
			t.Fatalf("want ErrNotAnApprover, got %v", err)
		}
	})

	t.Run("an approval cannot be forged by json", func(t *testing.T) {
		// Approval has no exported fields and deliberately no UnmarshalJSON,
		// so the most natural forgery attempt produces the zero value.
		var forged Approval
		raw := `{"fingerprint":"deadbeefdeadbeef","verdict":"v","by":{"kind":"human","id":"mallory"},
		         "at":"2026-01-08T03:00:00Z","expiresAt":"2026-01-09T03:00:00Z"}`
		if err := json.Unmarshal([]byte(raw), &forged); err != nil {
			t.Fatalf("unmarshal should be a no-op, not an error: %v", err)
		}
		if !forged.IsZero() {
			t.Fatalf("json forged a non-zero Approval: %+v", forged)
		}
		if forged.Live(testNow) {
			t.Fatal("a zero approval must never be live")
		}
	})

	t.Run("reflect cannot set the fields", func(t *testing.T) {
		var ap Approval
		v := reflect.ValueOf(&ap).Elem().FieldByName("by")
		if !v.IsValid() {
			t.Fatal("fixture is wrong: the field should exist, just be unsettable")
		}
		if v.CanSet() {
			t.Fatal("an unexported Approval field became settable through reflect")
		}
	})
}

// TestApprovalHasNoExportedFields is the structural assertion behind the test
// above: the guarantee is the type's shape, so the shape is what is tested.
// If somebody exports a field for convenience, this fails before the security
// property quietly disappears.
func TestApprovalHasNoExportedFields(t *testing.T) {
	ty := reflect.TypeOf(Approval{})
	for i := 0; i < ty.NumField(); i++ {
		if f := ty.Field(i); f.IsExported() {
			t.Fatalf("Approval.%s is exported: the type must not be constructible "+
				"with a struct literal outside this package", f.Name)
		}
	}
	// And Record, which holds the state, for the same reason.
	rty := reflect.TypeOf(Record{})
	for i := 0; i < rty.NumField(); i++ {
		if f := rty.Field(i); f.IsExported() {
			t.Fatalf("Record.%s is exported: state must only move through the Store", f.Name)
		}
	}
}

// TestOnlyStoreApproveMintsAnApproval scans the package source: no exported
// function or method other than Store.Approve may return an Approval. That is
// what makes "every approval is in an audit history" true by construction
// rather than by convention.
func TestOnlyStoreApproveMintsAnApproval(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing package: %v", err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
					continue
				}
				for _, res := range fn.Type.Results.List {
					if !returnsApproval(res.Type) {
						continue
					}
					if isMethodOn(fn, "Store") && fn.Name.Name == "Approve" {
						continue
					}
					if isMethodOn(fn, "Record") && fn.Name.Name == "Approval" {
						continue // an accessor, not a constructor
					}
					t.Fatalf("%s returns an Approval; only Store.Approve may mint one",
						fn.Name.Name)
				}
			}
		}
	}
}

func returnsApproval(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "Approval"
	case *ast.StarExpr:
		return returnsApproval(t.X)
	}
	return false
}

func isMethodOn(fn *ast.FuncDecl, typeName string) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == typeName
}

// ---- the happy path, so the tests above are not vacuous ----

func TestApprovalByADifferentHumanWorks(t *testing.T) {
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	ap, err := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Approve(ap, rec.ID(), "verified against last month's incident", fixedClock())
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.State() != StateApproved {
		t.Fatalf("state = %s", got.State())
	}
	approval, ok := got.Approval()
	if !ok {
		t.Fatal("an approved record must carry its approval")
	}
	if approval.Fingerprint() != rec.ID() {
		t.Fatalf("approval bound to %s, proposal is %s", approval.Fingerprint(), rec.ID())
	}
	if approval.Verdict() != verdictHash(rec.Proposal().Gate) {
		t.Fatal("approval must be bound to the verdict it was granted against")
	}
	if !approval.At().Equal(testNow) {
		t.Fatalf("approval time %s should come from the clock", approval.At())
	}
	if want := testNow.Add(DefaultApprovalTTL); !approval.ExpiresAt().Equal(want) {
		t.Fatalf("expiry %s, want %s", approval.ExpiresAt(), want)
	}
}

func TestApprovalRejectsAFailedGate(t *testing.T) {
	s := NewStore()
	spec := passingSpec()
	spec.CandidateScore = scoreFor(candidatePolicy(), nil) // a tie: gate rejects
	rec, err := s.Create(Actor{Kind: ActorTuner, ID: "nightly"}, spec, fixedClock())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.State() != StateRejected {
		t.Fatalf("a failing candidate must be filed rejected, got %s", rec.State())
	}
	ap, _ := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	if _, err := s.Approve(ap, rec.ID(), "", fixedClock()); err == nil {
		t.Fatal("a rejected proposal must not be approvable")
	}
}

func TestApprovalNoteIsSanitizedAndBounded(t *testing.T) {
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorAgent, ID: "reasoner"})
	ap, _ := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	got, err := s.Approve(ap, rec.ID(), "ok\nINFO: fake log line\x00", fixedClock())
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	note, _ := got.Approval()
	if strings.ContainsAny(note.Note(), "\n\x00") {
		t.Fatalf("note kept control characters: %q", note.Note())
	}

	s2 := NewStore()
	rec2 := mustCreate(t, s2, Actor{Kind: ActorAgent, ID: "reasoner"})
	ap2, _ := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	if _, err := s2.Approve(ap2, rec2.ID(), strings.Repeat("x", maxNote+1), fixedClock()); err == nil {
		t.Fatal("an oversized note must be refused")
	}
}

func TestActorValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    Actor
		ok   bool
	}{
		{"human", Actor{Kind: ActorHuman, ID: "alice"}, true},
		{"unknown kind", Actor{Kind: "root", ID: "alice"}, false},
		{"no id", Actor{Kind: ActorHuman}, false},
		{"long id", Actor{Kind: ActorHuman, ID: strings.Repeat("a", maxActorID+1)}, false},
		{"newline in id", Actor{Kind: ActorHuman, ID: "alice\nbob"}, false},
		{"nul in id", Actor{Kind: ActorHuman, ID: "alice\x00"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.a.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestApprovalTTLIsBounded(t *testing.T) {
	if _, err := NewStoreWithTTL(0); err == nil {
		t.Fatal("a zero ttl must be refused")
	}
	if _, err := NewStoreWithTTL(maxApprovalTTL + time.Hour); err == nil {
		t.Fatal("an unbounded ttl must be refused")
	}
	s, err := NewStoreWithTTL(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	ap, _ := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	got, err := s.Approve(ap, rec.ID(), "", fixedClock())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := got.Approval()
	if want := testNow.Add(time.Hour); !a.ExpiresAt().Equal(want) {
		t.Fatalf("expiry %s, want %s", a.ExpiresAt(), want)
	}
}

// TestStoredApprovalsAreRevalidatedFieldByField exercises the rehydration
// path directly. Record.UnmarshalJSON is the only caller, but the intrinsic
// invariants belong to the approval itself and are tested where they live.
func TestStoredApprovalsAreRevalidatedFieldByField(t *testing.T) {
	good := approvalWire{
		Fingerprint: strings.Repeat("a", fingerprintLen),
		Verdict:     "v",
		By:          Actor{Kind: ActorHuman, ID: "bob"},
		At:          testNow,
		ExpiresAt:   testNow.Add(time.Hour),
	}
	if _, err := newApprovalFromWire(good); err != nil {
		t.Fatalf("a well-formed approval failed to load: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*approvalWire)
	}{
		{"invalid actor", func(w *approvalWire) { w.By = Actor{Kind: "root", ID: "x"} }},
		{"non-human approver", func(w *approvalWire) { w.By.Kind = ActorTuner }},
		{"short fingerprint", func(w *approvalWire) { w.Fingerprint = "abc" }},
		{"no verdict", func(w *approvalWire) { w.Verdict = "" }},
		{"no timestamp", func(w *approvalWire) { w.At = time.Time{} }},
		{"expiry before grant", func(w *approvalWire) { w.ExpiresAt = w.At.Add(-time.Hour) }},
		{"expiry equals grant", func(w *approvalWire) { w.ExpiresAt = w.At }},
		{"oversized note", func(w *approvalWire) { w.Note = strings.Repeat("x", maxNote+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := good
			tc.mut(&w)
			if _, err := newApprovalFromWire(w); err == nil {
				t.Fatal("a malformed stored approval loaded")
			}
		})
	}
}

// TestApproveRefusesOutsideItsPreconditions covers the guards inside the
// minting function itself, so it is correct standing alone rather than only in
// the sequence Store.Approve happens to call it in.
func TestApproveRefusesOutsideItsPreconditions(t *testing.T) {
	s := NewStore()
	rec := mustCreate(t, s, Actor{Kind: ActorTuner, ID: "nightly"})
	inner, _ := s.Get(rec.ID())
	good, err := NewApprover(Actor{Kind: ActorHuman, ID: "bob"})
	if err != nil {
		t.Fatal(err)
	}

	var nilAp *Approver
	if _, err := nilAp.approve(inner, "", testNow, time.Hour); !errors.Is(err, ErrNotAnApprover) {
		t.Fatalf("nil approver: %v", err)
	}
	if _, err := good.approve(nil, "", testNow, time.Hour); err == nil {
		t.Fatal("approved a nil record")
	}
	if _, err := good.approve(inner, "", time.Time{}, time.Hour); err == nil {
		t.Fatal("approved without a clock")
	}
	if _, err := good.approve(inner, "", testNow, maxApprovalTTL+time.Hour); err == nil {
		t.Fatal("approved with an unbounded ttl")
	}
	// A zero ttl falls back to the default rather than minting something
	// that expires immediately.
	ap, err := good.approve(inner, "", testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := testNow.Add(DefaultApprovalTTL); !ap.ExpiresAt().Equal(want) {
		t.Fatalf("expiry %s, want %s", ap.ExpiresAt(), want)
	}
	// An approver whose actor was corrupted after construction.
	broken := &Approver{actor: Actor{Kind: ActorHuman, ID: ""}}
	if _, err := broken.approve(inner, "", testNow, time.Hour); err == nil {
		t.Fatal("an invalid approver minted an approval")
	}
	demoted := &Approver{actor: Actor{Kind: ActorAgent, ID: "reasoner"}}
	if _, err := demoted.approve(inner, "", testNow, time.Hour); !errors.Is(err, ErrNotAnApprover) {
		t.Fatalf("a demoted approver minted an approval: %v", err)
	}
}
