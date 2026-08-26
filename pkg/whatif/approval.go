package whatif

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ActorKind is the class of thing taking an action. It is a closed set
// because the whole authorization model is "which kinds may do what", and an
// unknown kind must be an error rather than a default.
type ActorKind string

const (
	// ActorHuman is a person, authenticated by the API layer. The ONLY kind
	// that may approve.
	ActorHuman ActorKind = "human"
	// ActorAgent is the LLM reasoning plane (§5, unit 6/8). It may author
	// proposals and read everything. It may never approve, and NewApprover
	// rejects it — so unit 8's "hostile prompt attempts to self-approve" is
	// not a check that can be forgotten, it is a constructor that fails.
	ActorAgent ActorKind = "agent"
	// ActorTuner is the nightly closed-loop tuner (§4.6). Same standing as
	// the agent: it proposes, it cannot approve.
	ActorTuner ActorKind = "tuner"
	// ActorSystem is kilter itself recording something that already happened
	// — the applied transition, an expiry sweep. It cannot approve either.
	ActorSystem ActorKind = "system"
)

// maxActorID bounds an identity string. Identities land in an audit record
// that must stay small and printable; an unbounded one is a storage and a
// log-injection problem at once.
const maxActorID = 128

// Actor is who did something.
type Actor struct {
	Kind ActorKind `json:"kind"`
	ID   string    `json:"id"`
}

// Validate rejects an actor that could not be audited. IDs must be non-empty,
// bounded, and free of control characters — an identity that can embed a
// newline can forge a second line in any log that renders it.
func (a Actor) Validate() error {
	switch a.Kind {
	case ActorHuman, ActorAgent, ActorTuner, ActorSystem:
	default:
		return fmt.Errorf("whatif: unknown actor kind %q", a.Kind)
	}
	if a.ID == "" {
		return errors.New("whatif: actor id must not be empty")
	}
	if len(a.ID) > maxActorID {
		return fmt.Errorf("whatif: actor id is %d bytes, over the %d limit", len(a.ID), maxActorID)
	}
	for _, r := range a.ID {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return fmt.Errorf("whatif: actor id %q contains a control character", a.ID)
		}
	}
	return nil
}

// String is the audit form.
func (a Actor) String() string { return string(a.Kind) + ":" + a.ID }

// canApprove is the capability check. Only a human, and the constant is
// written as an equality against a single kind rather than a "not in" list so
// that adding a new ActorKind cannot accidentally grant approval.
func (a Actor) canApprove() bool { return a.Kind == ActorHuman }

// sameIdentity reports whether two actors are the same person or process for
// the purposes of the self-approval rule. Kind is deliberately NOT part of the
// comparison: if it were, "agent:alice" could author a proposal that
// "human:alice" then approves, which is self-approval wearing a costume. IDs
// are compared case-insensitively after trimming, because an identity system
// that treats "Alice" and "alice" as one person is the common case and the
// stricter reading is the safe one here.
func sameIdentity(a, b Actor) bool {
	return strings.EqualFold(strings.TrimSpace(a.ID), strings.TrimSpace(b.ID))
}

// ErrSelfApproval is returned when a proposal's author tries to approve it.
var ErrSelfApproval = errors.New("whatif: a proposal cannot be approved by its author")

// ErrNotAnApprover is returned when a non-human actor tries to become one.
var ErrNotAnApprover = errors.New("whatif: only a human actor may approve a proposal")

// Approval is the token that says a human said yes to a specific proposal.
//
// EVERY FIELD IS UNEXPORTED, AND THAT IS THE DESIGN. Outside this package:
//
//   - `whatif.Approval{...}` does not compile — there is no field to set.
//   - `json.Unmarshal(data, &ap)` produces the zero Approval — the type has no
//     exported fields and deliberately no UnmarshalJSON, so a persisted record
//     is rehydrated through Record.UnmarshalJSON, which revalidates the
//     binding rather than trusting the bytes.
//   - `reflect` cannot set an unexported field; it panics.
//
// The only way to obtain a non-zero Approval is Store.Approve, which requires
// an *Approver, which NewApprover will not build for a non-human actor, and
// which refuses to act on a proposal the same identity authored. Self-approval
// is therefore not a rule that is checked — it is a value that cannot be
// constructed.
//
// (The residual hole is package-internal code and `unsafe`. The first is this
// package's own tests, which is the point; the second is out of any Go type
// system's reach and is recorded in FINDINGS.md rather than pretended away.)
type Approval struct {
	fingerprint string
	verdict     string
	by          Actor
	at          time.Time
	expiresAt   time.Time
	note        string
}

// Fingerprint is the proposal content hash this approval is bound to. An
// approval for one proposal can never be replayed onto another, because the
// fingerprint covers the author, the target, both policies and the gate
// verdict — change any of them and the binding no longer matches.
func (a Approval) Fingerprint() string { return a.fingerprint }

// Verdict is the hash of the GateResult the approval was granted against, so
// an approval cannot survive the proposal being re-gated to a different
// answer.
func (a Approval) Verdict() string { return a.verdict }

// By is the approver.
func (a Approval) By() Actor { return a.by }

// At is when approval was granted (UTC).
func (a Approval) At() time.Time { return a.at }

// ExpiresAt is when it stops counting. Approvals expire for the same reason
// pkg/api's plan approvals do: the cluster drifts, and yesterday's approved
// policy change is not today's.
func (a Approval) ExpiresAt() time.Time { return a.expiresAt }

// Note is the approver's free-text comment, sanitized on the way in.
func (a Approval) Note() string { return a.note }

// IsZero reports the absence of an approval.
func (a Approval) IsZero() bool { return a.fingerprint == "" && a.by == Actor{} }

// Live reports whether the approval is present and unexpired at t.
func (a Approval) Live(t time.Time) bool {
	return !a.IsZero() && !t.UTC().After(a.expiresAt)
}

// approvalWire is the on-disk shape. It exists only inside this file and in
// Record's codec: the marshal side is public information, the unmarshal side
// goes through newApprovalFromWire, which revalidates every invariant.
type approvalWire struct {
	Fingerprint string    `json:"fingerprint"`
	Verdict     string    `json:"verdict"`
	By          Actor     `json:"by"`
	At          time.Time `json:"at"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Note        string    `json:"note,omitempty"`
}

// MarshalJSON renders the approval for humans and for storage. There is no
// matching UnmarshalJSON on purpose — see the type comment.
func (a Approval) MarshalJSON() ([]byte, error) {
	if a.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(a.wire())
}

func (a Approval) wire() approvalWire {
	return approvalWire{
		Fingerprint: a.fingerprint,
		Verdict:     a.verdict,
		By:          a.by,
		At:          a.at,
		ExpiresAt:   a.expiresAt,
		Note:        a.note,
	}
}

// newApprovalFromWire rebuilds an approval from storage, enforcing every
// intrinsic invariant. The cross-checks that need the proposal (binding,
// author ≠ approver) are Record.UnmarshalJSON's job, immediately after.
func newApprovalFromWire(w approvalWire) (Approval, error) {
	if err := w.By.Validate(); err != nil {
		return Approval{}, err
	}
	if !w.By.canApprove() {
		return Approval{}, fmt.Errorf("%w: %s", ErrNotAnApprover, w.By)
	}
	if len(w.Fingerprint) != fingerprintLen {
		return Approval{}, fmt.Errorf("whatif: approval fingerprint %q is not %d hex chars",
			w.Fingerprint, fingerprintLen)
	}
	if w.Verdict == "" {
		return Approval{}, errors.New("whatif: approval carries no gate verdict")
	}
	if w.At.IsZero() {
		return Approval{}, errors.New("whatif: approval has no timestamp")
	}
	if !w.ExpiresAt.After(w.At) {
		return Approval{}, fmt.Errorf("whatif: approval expires at %s, not after its grant at %s",
			w.ExpiresAt.Format(time.RFC3339), w.At.Format(time.RFC3339))
	}
	note, err := sanitizeNote(w.Note)
	if err != nil {
		return Approval{}, err
	}
	return Approval{
		fingerprint: w.Fingerprint,
		verdict:     w.Verdict,
		by:          w.By,
		at:          w.At.UTC(),
		expiresAt:   w.ExpiresAt.UTC(),
		note:        note,
	}, nil
}

// Approver is the capability to approve. It is a distinct type from Actor so
// that "can approve" is something a caller must have been given, not something
// it can assert about itself at a call site.
type Approver struct {
	actor Actor
}

// NewApprover builds the capability, or refuses. A non-human actor — the LLM
// agent of §5, the nightly tuner of §4.6, kilter's own system identity — gets
// an error here, which is why unit 8's hostile-prompt scenario terminates at
// the type system rather than at a policy check somewhere downstream.
func NewApprover(a Actor) (*Approver, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	if !a.canApprove() {
		return nil, fmt.Errorf("%w: %s", ErrNotAnApprover, a)
	}
	return &Approver{actor: a}, nil
}

// Actor reports who this approver is.
func (ap *Approver) Actor() Actor { return ap.actor }

// DefaultApprovalTTL bounds how long an approval stays valid, matching
// pkg/api's plan-approval TTL. The two are the same idiom deliberately: an
// operator should not have to learn a second approval model for policy
// changes, and a divergent TTL would be the first place the two drift.
const DefaultApprovalTTL = 24 * time.Hour

// maxApprovalTTL is the ceiling. An approval that outlives the history it was
// justified by is not an approval, it is a standing instruction.
const maxApprovalTTL = 7 * 24 * time.Hour

// approve mints the token. It is unexported: the only exported route to an
// Approval is Store.Approve, so an approval that is not recorded in a store's
// audit history cannot exist.
//
// Every precondition is re-checked here even though Store.Approve has already
// checked most of them, because this function is the one that constructs the
// value and it must be correct standing alone.
func (ap *Approver) approve(rec *Record, note string, now time.Time, ttl time.Duration) (Approval, error) {
	if ap == nil {
		return Approval{}, ErrNotAnApprover
	}
	if err := ap.actor.Validate(); err != nil {
		return Approval{}, err
	}
	if !ap.actor.canApprove() {
		return Approval{}, fmt.Errorf("%w: %s", ErrNotAnApprover, ap.actor)
	}
	if rec == nil {
		return Approval{}, errors.New("whatif: no proposal to approve")
	}
	if sameIdentity(rec.proposal.Author, ap.actor) {
		return Approval{}, fmt.Errorf("%w (author %s, approver %s)",
			ErrSelfApproval, rec.proposal.Author, ap.actor)
	}
	if !rec.proposal.Gate.Passed {
		return Approval{}, fmt.Errorf("whatif: proposal %s did not pass the gate and cannot be approved: %v",
			rec.ID(), rec.proposal.Gate.Reasons)
	}
	if rec.state != StateGated {
		return Approval{}, fmt.Errorf("whatif: proposal %s is %s, not %s", rec.ID(), rec.state, StateGated)
	}
	now = now.UTC()
	if now.IsZero() {
		return Approval{}, errors.New("whatif: approval needs a clock")
	}
	if ttl <= 0 {
		ttl = DefaultApprovalTTL
	}
	if ttl > maxApprovalTTL {
		return Approval{}, fmt.Errorf("whatif: approval ttl %v exceeds the %v ceiling", ttl, maxApprovalTTL)
	}
	clean, err := sanitizeNote(note)
	if err != nil {
		return Approval{}, err
	}
	return Approval{
		fingerprint: rec.proposal.Fingerprint(),
		verdict:     verdictHash(rec.proposal.Gate),
		by:          ap.actor,
		at:          now,
		expiresAt:   now.Add(ttl),
		note:        clean,
	}, nil
}

// maxNote bounds free text stored in a proposal or approval.
const maxNote = 2048

// sanitizeNote bounds and de-fangs operator- or model-supplied prose. Notes
// reach a CLI, an API response and a future LLM context window; a note that
// can carry control characters can forge a log line, and a note that can carry
// a megabyte can fill a store.
func sanitizeNote(s string) (string, error) {
	if len(s) > maxNote {
		return "", fmt.Errorf("whatif: note is %d bytes, over the %d limit", len(s), maxNote)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(' ')
		case r == unicode.ReplacementChar || unicode.IsControl(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

// verdictHash fingerprints a gate verdict so an approval can be bound to the
// exact answer it was granted against.
func verdictHash(g GateResult) string {
	h := sha256.New()
	fmt.Fprintf(h, "passed|%v\n", g.Passed)
	fmt.Fprintf(h, "policies|%s|%s\n", g.BaselinePolicy, g.CandidatePolicy)
	fmt.Fprintf(h, "margin|%v\n", g.RequiredRegretImprovementUSD)
	fmt.Fprintf(h, "tol|%v|%v|%v|%v\n",
		g.Tolerance.MinRegretImprovementUSD, g.Tolerance.MinRegretImprovementPct,
		g.Tolerance.MaxFlipRateIncrease, g.Tolerance.MaxOracleGapIncreasePct)
	for i, r := range g.Failed {
		fmt.Fprintf(h, "failed|%d|%s\n", i, r)
	}
	for i, r := range g.Reasons {
		fmt.Fprintf(h, "reason|%d|%s\n", i, r)
	}
	for i, w := range g.Wins {
		fmt.Fprintf(h, "win|%d|%s\n", i, w)
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintLen]
}
