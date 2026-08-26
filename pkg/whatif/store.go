package whatif

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// State is where a proposal sits in its lifecycle.
//
// The set is closed and the transition table (see transitions) is the whole
// state machine. INV-4 in the design's appendix says it in one line: the only
// path to StateApplied runs through the gate and an approval. That is enforced
// here, in one place, with no second writer — Record's fields are unexported
// and no method outside this file changes them.
type State string

const (
	// StateDraft exists for one instant: Create records it as the "from" of
	// the gating transition so the audit history reads as a lifecycle rather
	// than springing into being already judged. No record rests here.
	StateDraft State = "draft"
	// StateGated means the gate ran and PASSED. Awaiting a human.
	StateGated State = "gated"
	// StateRejected is terminal: the gate failed, or a human said no.
	StateRejected State = "rejected"
	// StateApproved means a human who is not the author said yes, and the
	// approval has not expired. This is the furthest a proposal travels
	// inside this package.
	StateApproved State = "approved"
	// StateApplied is recorded by whatever actually applied the change,
	// after the fact. Nothing in this package applies anything.
	StateApplied State = "applied"
	// StateExpired is terminal: the approval's TTL elapsed before anything
	// applied it. Re-approval means a new decision on fresh evidence.
	StateExpired State = "expired"
)

// knownState is the closed set.
func knownState(s State) bool {
	switch s {
	case StateDraft, StateGated, StateRejected, StateApproved, StateApplied, StateExpired:
		return true
	}
	return false
}

// terminal reports whether a state accepts no further transitions.
func terminal(s State) bool {
	return s == StateRejected || s == StateApplied || s == StateExpired
}

// transitions is the allowed edge set, written out rather than implied by the
// code that performs each move. A table can be tested exhaustively; a scatter
// of if-statements can only be tested for the cases somebody thought of.
//
// Note what is absent: there is no edge into StateApproved from anything but
// StateGated, and no edge into StateApplied from anything but StateApproved.
var transitions = map[State][]State{
	StateDraft:    {StateGated, StateRejected},
	StateGated:    {StateApproved, StateRejected},
	StateApproved: {StateApplied, StateExpired, StateRejected},
	StateRejected: {},
	StateApplied:  {},
	StateExpired:  {},
}

// canTransition reports whether from → to is an edge.
func canTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition is one audited move. The history is append-only and travels with
// the record, so "who approved this, when, and against which verdict" survives
// a restart without a separate audit store.
type Transition struct {
	From State     `json:"from"`
	To   State     `json:"to"`
	At   time.Time `json:"at"`
	By   Actor     `json:"by"`
	Note string    `json:"note,omitempty"`
}

// Record is a proposal plus its state. Every field is unexported: outside this
// package a Record can be read through its accessors and moved through the
// Store's methods, and by no other means. `whatif.Record{state: StateApproved}`
// does not compile, and json.Unmarshal goes through UnmarshalJSON, which
// revalidates the whole thing.
type Record struct {
	proposal Proposal
	state    State
	approval Approval
	history  []Transition
}

// ID is the proposal's content-addressed fingerprint.
func (r *Record) ID() string { return r.proposal.ID() }

// Proposal returns a copy of the proposal. A copy, not a pointer: a caller
// that could mutate the proposal in place would invalidate the fingerprint
// that its approval is bound to.
func (r *Record) Proposal() Proposal { return r.proposal.clone() }

// State reports the lifecycle state.
func (r *Record) State() State { return r.state }

// Approval returns the approval, if the record has one.
func (r *Record) Approval() (Approval, bool) {
	if r.approval.IsZero() {
		return Approval{}, false
	}
	return r.approval, true
}

// History returns a copy of the audit trail.
func (r *Record) History() []Transition {
	return append(make([]Transition, 0, len(r.history)), r.history...)
}

// clone deep-copies a record, so the store hands out values callers cannot use
// to reach back into its state.
func (r *Record) clone() *Record {
	return &Record{
		proposal: r.proposal.clone(),
		state:    r.state,
		approval: r.approval,
		history:  r.History(),
	}
}

// clone deep-copies the slice fields of a proposal.
func (p Proposal) clone() Proposal {
	p.Changes = append([]Change(nil), p.Changes...)
	p.EvidenceIDs = append([]string(nil), p.EvidenceIDs...)
	p.Gate.Failed = append([]Rule(nil), p.Gate.Failed...)
	p.Gate.Reasons = append([]string(nil), p.Gate.Reasons...)
	p.Gate.Wins = append([]string(nil), p.Gate.Wins...)
	return p
}

// move applies a transition after checking the edge exists.
func (r *Record) move(to State, by Actor, note string, at time.Time) error {
	if !canTransition(r.state, to) {
		return fmt.Errorf("whatif: proposal %s cannot go %s → %s", r.ID(), r.state, to)
	}
	clean, err := sanitizeNote(note)
	if err != nil {
		return err
	}
	r.history = append(r.history, Transition{
		From: r.state, To: to, At: at.UTC(), By: by, Note: clean,
	})
	r.state = to
	return nil
}

// ---- store ----

// ErrNotFound is returned for an unknown proposal ID.
var ErrNotFound = errors.New("whatif: no such proposal")

// maxRecords bounds the store. A tuner that runs nightly and an agent that can
// file proposals need a ceiling, or a store backed by a file grows without
// limit. The oldest terminal records are evicted first; a non-terminal record
// is never evicted, because losing a pending approval silently is worse than
// refusing the write.
const maxRecords = 1000

// Store holds proposals and owns their state machine. It is concrete rather
// than an interface on purpose: an interface would let a caller supply an
// implementation whose Approve does not check anything, and the state machine
// is the security property. Persistence is the seam instead — Snapshot and
// Load move bytes, and cmd/ decides where they live (see FINDINGS.md).
type Store struct {
	mu   sync.Mutex
	byID map[string]*Record
	ttl  time.Duration
}

// NewStore builds an empty store with the default approval TTL.
func NewStore() *Store {
	return &Store{byID: map[string]*Record{}, ttl: DefaultApprovalTTL}
}

// NewStoreWithTTL builds a store whose approvals expire after ttl.
func NewStoreWithTTL(ttl time.Duration) (*Store, error) {
	if ttl <= 0 || ttl > maxApprovalTTL {
		return nil, fmt.Errorf("whatif: approval ttl %v out of (0,%v]", ttl, maxApprovalTTL)
	}
	s := NewStore()
	s.ttl = ttl
	return s, nil
}

// Create gates a spec and files the result.
//
// The gate runs HERE, from the scorecards, using this package's rules — the
// caller supplies evidence and gets a verdict, never the other way round. A
// spec whose candidate fails the gate is still filed, in StateRejected, with
// the reasons: a rejected proposal is a record of a question that was asked
// and answered, and discarding it would make the tuner look like it never ran.
//
// The returned record is a copy. Filing the same spec twice returns the
// existing record unchanged, because proposals are content-addressed — which
// is what makes a nightly tuner idempotent instead of a duplicate factory.
func (s *Store) Create(author Actor, spec Spec, clock Clock) (*Record, error) {
	if err := author.Validate(); err != nil {
		return nil, err
	}
	now := clock.now()
	if now.IsZero() {
		return nil, errors.New("whatif: Create needs a clock")
	}
	norm, err := spec.normalize()
	if err != nil {
		return nil, err
	}
	p, err := build(author, norm, now)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byID[p.ID()]; ok {
		return existing.clone(), nil
	}
	if err := s.evictLocked(); err != nil {
		return nil, err
	}
	rec := &Record{proposal: p, state: StateDraft}
	rec.history = append(rec.history, Transition{
		From: "", To: StateDraft, At: now, By: author, Note: "filed",
	})
	to, note := StateRejected, firstReason(p.Gate)
	if p.Gate.Passed {
		to, note = StateGated, "gate passed"
	}
	if err := rec.move(to, Actor{Kind: ActorSystem, ID: "gate"}, note, now); err != nil {
		return nil, err
	}
	s.byID[p.ID()] = rec
	return rec.clone(), nil
}

// firstReason renders the leading gate reason for the audit note.
func firstReason(g GateResult) string {
	if len(g.Reasons) == 0 {
		return "gate failed"
	}
	return g.Reasons[0]
}

// evictLocked drops the oldest terminal record when the store is full.
func (s *Store) evictLocked() error {
	if len(s.byID) < maxRecords {
		return nil
	}
	var oldest *Record
	for _, id := range s.sortedIDsLocked() {
		r := s.byID[id]
		if !terminal(r.state) {
			continue
		}
		if oldest == nil || r.proposal.CreatedAt.Before(oldest.proposal.CreatedAt) {
			oldest = r
		}
	}
	if oldest == nil {
		return fmt.Errorf("whatif: proposal store is full (%d records, none terminal)", len(s.byID))
	}
	delete(s.byID, oldest.ID())
	return nil
}

// Get returns a copy of one record.
func (s *Store) Get(id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	return r.clone(), true
}

// List returns every record, oldest first, ties broken by ID. Deterministic
// by construction: this is what `kilter proposals` prints, and a listing whose
// order depended on map iteration would differ between two runs over identical
// state.
func (s *Store) List() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, 0, len(s.byID))
	for _, id := range s.sortedIDsLocked() {
		out = append(out, s.byID[id].clone())
	}
	return out
}

// ListState filters List by state.
func (s *Store) ListState(want State) []*Record {
	all := s.List()
	out := make([]*Record, 0, len(all))
	for _, r := range all {
		if r.state == want {
			out = append(out, r)
		}
	}
	return out
}

// sortedIDsLocked orders records by (CreatedAt, ID).
func (s *Store) sortedIDsLocked() []string {
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.byID[ids[i]], s.byID[ids[j]]
		if !a.proposal.CreatedAt.Equal(b.proposal.CreatedAt) {
			return a.proposal.CreatedAt.Before(b.proposal.CreatedAt)
		}
		return ids[i] < ids[j]
	})
	return ids
}

// Approve records a human's yes.
//
// This is the ONLY exported function in the package that can produce an
// Approval, and it requires an *Approver, which NewApprover will not build for
// a non-human actor. Combined with the author ≠ approver check inside
// (*Approver).approve, self-approval has no representation: there is no
// sequence of calls, from any actor, that reaches StateApproved on a proposal
// that actor filed.
func (s *Store) Approve(ap *Approver, id, note string, clock Clock) (*Record, error) {
	if ap == nil {
		return nil, ErrNotAnApprover
	}
	now := clock.now()
	if now.IsZero() {
		return nil, errors.New("whatif: Approve needs a clock")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	approval, err := ap.approve(rec, note, now, s.ttl)
	if err != nil {
		return nil, err
	}
	if err := rec.move(StateApproved, ap.actor, note, now); err != nil {
		return nil, err
	}
	rec.approval = approval
	return rec.clone(), nil
}

// Reject records a no. Any actor may reject: refusing to make a change is
// always safe, so it needs no capability. The gate itself rejects through this
// path too, via Create.
func (s *Store) Reject(by Actor, id, reason string, clock Clock) (*Record, error) {
	if err := by.Validate(); err != nil {
		return nil, err
	}
	now := clock.now()
	if now.IsZero() {
		return nil, errors.New("whatif: Reject needs a clock")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := rec.move(StateRejected, by, reason, now); err != nil {
		return nil, err
	}
	rec.approval = Approval{}
	return rec.clone(), nil
}

// MarkApplied records — after the fact — that something else applied the
// change. This package applies nothing: there is no client, no writer and no
// executor here, and the argument order (an actor reporting, not commanding)
// is the shape of that boundary.
//
// It is the enforcement point for INV-4. A record reaches StateApplied only
// from StateApproved, which is reachable only from StateGated, which Create
// sets only when Decide passed. The approval must additionally still be live
// and still bound to this exact proposal and verdict — so a change applied a
// week after approval, or applied against a proposal whose contents drifted,
// is refused rather than back-dated into the audit trail.
func (s *Store) MarkApplied(by Actor, id, note string, clock Clock) (*Record, error) {
	if err := by.Validate(); err != nil {
		return nil, err
	}
	now := clock.now()
	if now.IsZero() {
		return nil, errors.New("whatif: MarkApplied needs a clock")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if rec.state != StateApproved {
		return nil, fmt.Errorf("whatif: proposal %s is %s; only an approved proposal can be applied",
			id, rec.state)
	}
	if err := rec.checkApprovalBinding(); err != nil {
		return nil, err
	}
	if !rec.approval.Live(now) {
		if err := rec.move(StateExpired, Actor{Kind: ActorSystem, ID: "ttl"}, "approval expired", now); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("whatif: proposal %s approval expired at %s",
			id, rec.approval.ExpiresAt().Format(time.RFC3339))
	}
	if err := rec.move(StateApplied, by, note, now); err != nil {
		return nil, err
	}
	return rec.clone(), nil
}

// Sweep expires approvals whose TTL has elapsed and returns how many moved.
// Callers run it on a timer; MarkApplied checks independently, so a store that
// is never swept is safe, just untidy.
func (s *Store) Sweep(clock Clock) (int, error) {
	now := clock.now()
	if now.IsZero() {
		return 0, errors.New("whatif: Sweep needs a clock")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.sortedIDsLocked() {
		rec := s.byID[id]
		if rec.state != StateApproved || rec.approval.Live(now) {
			continue
		}
		if err := rec.move(StateExpired, Actor{Kind: ActorSystem, ID: "ttl"}, "approval expired", now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// checkApprovalBinding re-verifies that the record's approval belongs to this
// exact proposal and verdict, and was not granted by its author. Called on
// load and again before apply: the producer and the checker of this binding
// are deliberately different code paths.
func (r *Record) checkApprovalBinding() error {
	if r.approval.IsZero() {
		return fmt.Errorf("whatif: proposal %s is %s but carries no approval", r.ID(), r.state)
	}
	if want := r.proposal.Fingerprint(); r.approval.Fingerprint() != want {
		return fmt.Errorf("whatif: approval is bound to proposal %s, not %s",
			r.approval.Fingerprint(), want)
	}
	if want := verdictHash(r.proposal.Gate); r.approval.Verdict() != want {
		return fmt.Errorf("whatif: approval was granted against a different gate verdict")
	}
	if !r.proposal.Gate.Passed {
		return fmt.Errorf("whatif: proposal %s carries an approval but did not pass the gate", r.ID())
	}
	if !r.approval.By().canApprove() {
		return fmt.Errorf("%w: %s", ErrNotAnApprover, r.approval.By())
	}
	if sameIdentity(r.proposal.Author, r.approval.By()) {
		return fmt.Errorf("%w (author %s, approver %s)",
			ErrSelfApproval, r.proposal.Author, r.approval.By())
	}
	return nil
}

// ---- persistence seam ----

// recordWire is the on-disk shape of a record.
type recordWire struct {
	ID       string        `json:"id"`
	Proposal Proposal      `json:"proposal"`
	State    State         `json:"state"`
	Approval *approvalWire `json:"approval,omitempty"`
	History  []Transition  `json:"history"`
}

// MarshalJSON renders a record.
func (r *Record) MarshalJSON() ([]byte, error) {
	w := recordWire{
		ID:       r.ID(),
		Proposal: r.proposal,
		State:    r.state,
		History:  r.history,
	}
	if !r.approval.IsZero() {
		aw := r.approval.wire()
		w.Approval = &aw
	}
	return json.Marshal(w)
}

// UnmarshalJSON rebuilds a record from storage and revalidates every
// invariant the type system enforces at construction time.
//
// This is the second lock on the door. The first is that Approval has no
// exported fields, so it cannot be forged in Go; but a store persisted to a
// file is bytes an attacker (or a bug) can edit, and "state": "approved" is
// easy to type. So: the ID must be the fingerprint the contents actually hash
// to, an approved or applied record must carry a live-shaped approval bound to
// that fingerprint and that verdict, granted by a human who is not the author,
// and a gated record must actually have passed the gate. A tampered file
// fails to load rather than loading a lie.
func (r *Record) UnmarshalJSON(b []byte) error {
	var w recordWire
	// A Decoder rather than json.Unmarshal, everywhere this package parses
	// stored state: only the Decoder can reject unknown fields, and silently
	// ignoring one in a security-relevant record is how a format change
	// becomes a dropped invariant.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("whatif: decoding proposal record: %w", err)
	}
	if !knownState(w.State) {
		return fmt.Errorf("whatif: unknown proposal state %q", w.State)
	}
	if w.State == StateDraft {
		return errors.New("whatif: a stored record may not be in the momentary draft state")
	}
	rec := &Record{proposal: w.Proposal, state: w.State, history: w.History}
	if got := rec.proposal.Fingerprint(); got != w.ID {
		return fmt.Errorf("whatif: record id %q does not match its contents (%s): tampered or truncated",
			w.ID, got)
	}
	if w.Approval != nil {
		ap, err := newApprovalFromWire(*w.Approval)
		if err != nil {
			return err
		}
		rec.approval = ap
	}
	switch w.State {
	case StateApproved, StateApplied:
		if err := rec.checkApprovalBinding(); err != nil {
			return err
		}
	case StateGated:
		if !rec.proposal.Gate.Passed {
			return fmt.Errorf("whatif: proposal %s is %s but its gate did not pass", w.ID, w.State)
		}
		if !rec.approval.IsZero() {
			return fmt.Errorf("whatif: proposal %s is %s but carries an approval", w.ID, w.State)
		}
	case StateRejected:
		if !rec.approval.IsZero() {
			return fmt.Errorf("whatif: proposal %s is rejected but carries an approval", w.ID)
		}
	case StateExpired:
		// An expired record keeps its approval as history: it is the record
		// of a decision that was made and then ran out of time, and the
		// binding must still be sound.
		if err := rec.checkApprovalBinding(); err != nil {
			return err
		}
	}
	for _, tr := range rec.history {
		if tr.From != "" && !knownState(tr.From) {
			return fmt.Errorf("whatif: history references unknown state %q", tr.From)
		}
		if !knownState(tr.To) {
			return fmt.Errorf("whatif: history references unknown state %q", tr.To)
		}
	}
	*r = *rec
	return nil
}

// storeWire is the on-disk shape of a whole store: a sorted slice, never a
// map, so the bytes are identical for identical state.
type storeWire struct {
	Version int       `json:"version"`
	TTLSecs int64     `json:"ttlSeconds"`
	Records []*Record `json:"records"`
}

// storeVersion is bumped when the wire format changes incompatibly.
const storeVersion = 1

// Snapshot serializes the store. Byte-identical for identical state: records
// are emitted in List order, and every nested collection is already sorted.
// cmd/ writes these bytes wherever proposals should live (see FINDINGS.md);
// this package deliberately owns no file, no bucket and no schema migration.
func (s *Store) Snapshot() ([]byte, error) {
	s.mu.Lock()
	ttl := s.ttl
	recs := make([]*Record, 0, len(s.byID))
	for _, id := range s.sortedIDsLocked() {
		recs = append(recs, s.byID[id].clone())
	}
	s.mu.Unlock()
	b, err := json.MarshalIndent(storeWire{
		Version: storeVersion,
		TTLSecs: int64(ttl / time.Second),
		Records: recs,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Load rebuilds a store from Snapshot bytes, revalidating every record.
func Load(b []byte) (*Store, error) {
	var w storeWire
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("whatif: decoding proposal store: %w", err)
	}
	if w.Version != storeVersion {
		return nil, fmt.Errorf("whatif: proposal store version %d, expected %d", w.Version, storeVersion)
	}
	ttl := time.Duration(w.TTLSecs) * time.Second
	if ttl <= 0 || ttl > maxApprovalTTL {
		return nil, fmt.Errorf("whatif: stored approval ttl %v out of (0,%v]", ttl, maxApprovalTTL)
	}
	if len(w.Records) > maxRecords {
		return nil, fmt.Errorf("whatif: stored %d records, over the %d cap", len(w.Records), maxRecords)
	}
	s := &Store{byID: make(map[string]*Record, len(w.Records)), ttl: ttl}
	for _, rec := range w.Records {
		if rec == nil {
			return nil, errors.New("whatif: nil record in proposal store")
		}
		if _, dup := s.byID[rec.ID()]; dup {
			return nil, fmt.Errorf("whatif: duplicate proposal %s in store", rec.ID())
		}
		s.byID[rec.ID()] = rec
	}
	return s, nil
}
