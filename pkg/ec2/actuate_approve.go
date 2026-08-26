package ec2

// Approval, made structural.
//
// Every other guardrail in Kilter is a check: a function that is called before
// acting and that a future edit can forget to call. §3.3's human-in-the-loop
// gate is too important for that, because the thing on the other side of it
// stops production instances. So it is built out of the type system instead:
//
//   - [Actuator] has NO method that takes a bare [domain.Step] and acts. Its
//     only execution entry points take an [ApprovedStep].
//   - [ApprovedStep] has only unexported fields. No package outside this one
//     can write them — a composite literal `ec2.ApprovedStep{step: s}` does not
//     compile — so the only way to obtain a usable one is
//     [Approval.Authorize].
//   - The zero [ApprovedStep] is representable (Go always permits a zero
//     value) and is inert: it carries no approval, and Execute refuses it with
//     [ErrNotApproved]. TestZeroApprovedStepCannotAct pins that.
//   - [Approval] itself can only be built by [NewApproval], which recomputes
//     the plan fingerprint from the steps and refuses a token that approves
//     anything else.
//   - `*Actuator` deliberately does NOT satisfy [domain.Actuator], so it
//     cannot be handed to [domain.Registry.RegisterActuator] at all. Only a
//     [BoundActuator] — an actuator with an approval already attached — does.
//     TestActuatorIsNotRegistrableWithoutApproval pins that.
//
// What this buys: a caller who forgets the gate does not get an unapproved
// resize, they get a compile error. A caller who works around it in a hurry
// gets [ErrNotApproved] at runtime. There is no third path.

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Kind is the [domain.Kind] these steps belong to. It is [Domain], typed:
// domain.Kind is a closed set and "ec2" is a member of it.
const Kind = domain.EC2

// ApprovalToken is what `kilter approve` produces and an operator hands to a
// controller: a signed-off plan fingerprint with an expiry.
//
// It is a plain serializable struct because it crosses a process boundary. It
// is NOT authority by itself — [NewApproval] turns a token into authority only
// after re-deriving the fingerprint from the steps in hand, so a token lifted
// from one plan cannot authorize another.
type ApprovalToken struct {
	// Fingerprint is the plan fingerprint the human approved.
	Fingerprint string `json:"fingerprint"`
	// Scope is accountID/region. A token approved for one account must not
	// authorize a step in another, even if the fingerprints collide.
	Scope string `json:"scope"`
	// ApprovedBy names the human or system that approved. Recorded in the
	// ledger; never empty in a valid token.
	ApprovedBy string    `json:"approvedBy"`
	ApprovedAt time.Time `json:"approvedAt"`
	// ExpiresAt bounds the approval. Yesterday's approved plan is not today's
	// plan; the account drifts.
	ExpiresAt time.Time `json:"expiresAt"`
}

// Approval is a token that has been checked against the exact steps it
// authorizes. Its fields are unexported: outside this package the only way to
// obtain one is [NewApproval], and the only thing it can do is
// [Approval.Authorize].
type Approval struct {
	token ApprovalToken
	// keys maps an authorized step key to its sequence number. It is read-only
	// after construction; Authorize never writes to it, so an Approval is safe
	// to share across goroutines.
	keys map[string]int
}

// Approval and step errors. Callers distinguish them with errors.Is.
var (
	// ErrNotApproved: execution was attempted without a usable approval —
	// a zero [ApprovedStep], an expired token, or a token for another plan.
	ErrNotApproved = errors.New("ec2: step is not approved for execution")
	// ErrApprovalExpired: the token was valid and is no longer.
	ErrApprovalExpired = errors.New("ec2: approval has expired")
	// ErrStepNotInPlan: the step is not one of the steps the fingerprint
	// covers. Approving a plan does not approve a step somebody appended.
	ErrStepNotInPlan = errors.New("ec2: step is not covered by the approved plan")
	// ErrFingerprintMismatch: the steps in hand do not hash to the approved
	// fingerprint. The plan was edited after approval.
	ErrFingerprintMismatch = errors.New("ec2: steps do not hash to the approved fingerprint")
	// ErrStepKeyMismatch: a step's idempotency key does not hash its own
	// contents.
	ErrStepKeyMismatch = errors.New("ec2: step key does not match its contents")
	// ErrScopeMismatch: the step targets an account/region the token does not
	// cover.
	ErrScopeMismatch = errors.New("ec2: step scope is outside the approval's scope")
)

// PlanFingerprint is the content hash of a step list, canonicalized so it does
// not depend on the order the steps arrived in.
//
// [domain.Fingerprint] hashes the slice as given. That is correct for a plan
// the core just built (its steps are already in Seq order) and wrong for a
// plan that round-tripped through JSON, a map, or a merge. Sorting by
// (Seq, Key) first makes the fingerprint a property of the plan's CONTENT,
// which is what a human is approving — pinned by
// TestPlanFingerprintIsShuffleInvariant.
func PlanFingerprint(steps []domain.Step) string {
	if len(steps) == 0 {
		return ""
	}
	sorted := make([]domain.Step, len(steps))
	copy(sorted, steps)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Seq != sorted[j].Seq {
			return sorted[i].Seq < sorted[j].Seq
		}
		return sorted[i].Key < sorted[j].Key
	})
	return domain.Fingerprint(sorted)
}

// NewApproval validates a token against the steps it claims to approve.
//
// Every check is a refusal, never a repair:
//
//  1. the token names a fingerprint, an approver and an expiry;
//  2. the steps are non-empty, in this domain, and each key hashes its own
//     contents (so a step edited after approval is caught even if the plan
//     fingerprint were somehow preserved);
//  3. the steps hash to the token's fingerprint;
//  4. the token has not expired as of now.
//
// now is passed, never read: this package has no clock.
func NewApproval(steps []domain.Step, tok ApprovalToken, now time.Time) (Approval, error) {
	switch {
	case tok.Fingerprint == "":
		return Approval{}, fmt.Errorf("%w: token carries no fingerprint", ErrNotApproved)
	case tok.ApprovedBy == "":
		return Approval{}, fmt.Errorf("%w: token names no approver", ErrNotApproved)
	case tok.ExpiresAt.IsZero():
		return Approval{}, fmt.Errorf("%w: token has no expiry; an approval that never lapses is not an approval", ErrNotApproved)
	case len(steps) == 0:
		return Approval{}, fmt.Errorf("%w: an empty plan cannot be approved", ErrNotApproved)
	case !now.IsZero() && !now.Before(tok.ExpiresAt):
		return Approval{}, fmt.Errorf("%w: approved %s, expired %s, now %s", ErrApprovalExpired,
			tok.ApprovedAt.UTC().Format(time.RFC3339), tok.ExpiresAt.UTC().Format(time.RFC3339),
			now.UTC().Format(time.RFC3339))
	}
	keys := make(map[string]int, len(steps))
	for _, s := range steps {
		if s.Target.Domain != Kind {
			return Approval{}, fmt.Errorf("%w: step %d targets domain %q, not %q",
				ErrNotApproved, s.Seq, s.Target.Domain, Kind)
		}
		if s.Key == "" {
			return Approval{}, fmt.Errorf("%w: step %d has no key", ErrStepKeyMismatch, s.Seq)
		}
		if want := domain.StepKey(s.Target, s.From, s.To); s.Key != want {
			return Approval{}, fmt.Errorf("%w: step %d claims %q, contents hash to %q",
				ErrStepKeyMismatch, s.Seq, s.Key, want)
		}
		if prev, dup := keys[s.Key]; dup {
			return Approval{}, fmt.Errorf("%w: steps %d and %d share key %q",
				ErrNotApproved, prev, s.Seq, s.Key)
		}
		if tok.Scope != "" && s.Target.Scope != tok.Scope {
			return Approval{}, fmt.Errorf("%w: step %d targets %q, token covers %q",
				ErrScopeMismatch, s.Seq, s.Target.Scope, tok.Scope)
		}
		keys[s.Key] = s.Seq
	}
	if fp := PlanFingerprint(steps); fp != tok.Fingerprint {
		return Approval{}, fmt.Errorf("%w: steps hash to %q, token approves %q",
			ErrFingerprintMismatch, fp, tok.Fingerprint)
	}
	return Approval{token: tok, keys: keys}, nil
}

// Token returns a copy of the underlying token, for the ledger and the report.
func (a Approval) Token() ApprovalToken { return a.token }

// Fingerprint is the approved plan fingerprint, or "" for the zero Approval.
func (a Approval) Fingerprint() string { return a.token.Fingerprint }

// Valid reports whether this approval was constructed by [NewApproval].
func (a Approval) Valid() bool { return len(a.keys) > 0 && a.token.Fingerprint != "" }

// Covers reports whether a step key is one this approval authorizes.
func (a Approval) Covers(key string) bool { _, ok := a.keys[key]; return ok }

// Steps returns the authorized step keys in canonical (sequence, then key)
// order — never map order.
func (a Approval) Steps() []string {
	if len(a.keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.keys))
	for k := range a.keys {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if a.keys[out[i]] != a.keys[out[j]] {
			return a.keys[out[i]] < a.keys[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// Authorize turns a step into the only value this package's actuators accept.
//
// It re-checks everything [NewApproval] checked about this one step, because
// the step handed to Authorize need not be the same value that was hashed —
// callers hold plans in slices, maps and JSON, and "it was in the plan when we
// approved it" is a belief, not a fact.
func (a Approval) Authorize(step domain.Step) (ApprovedStep, error) {
	if !a.Valid() {
		return ApprovedStep{}, fmt.Errorf("%w: no approval was presented", ErrNotApproved)
	}
	if step.Key == "" {
		return ApprovedStep{}, fmt.Errorf("%w: step has no key", ErrStepKeyMismatch)
	}
	if want := domain.StepKey(step.Target, step.From, step.To); step.Key != want {
		return ApprovedStep{}, fmt.Errorf("%w: step claims %q, contents hash to %q",
			ErrStepKeyMismatch, step.Key, want)
	}
	if !a.Covers(step.Key) {
		return ApprovedStep{}, fmt.Errorf("%w: %s is not in plan %s",
			ErrStepNotInPlan, step.Key, a.token.Fingerprint)
	}
	if a.token.Scope != "" && step.Target.Scope != a.token.Scope {
		return ApprovedStep{}, fmt.Errorf("%w: step targets %q, token covers %q",
			ErrScopeMismatch, step.Target.Scope, a.token.Scope)
	}
	return ApprovedStep{step: step, approval: a, authorized: true}, nil
}

// AuthorizeAll authorizes a whole plan, in the order given, failing on the
// first step the approval does not cover.
func (a Approval) AuthorizeAll(steps []domain.Step) ([]ApprovedStep, error) {
	out := make([]ApprovedStep, 0, len(steps))
	for _, s := range steps {
		as, err := a.Authorize(s)
		if err != nil {
			return nil, err
		}
		out = append(out, as)
	}
	return out, nil
}

// ApprovedStep is a step plus the approval that authorizes it. Its fields are
// unexported and there is no exported constructor other than
// [Approval.Authorize], so the zero value is the only one a foreign package
// can build — and the zero value cannot act.
type ApprovedStep struct {
	step     domain.Step
	approval Approval
	// origin is the key of the step this one undoes, set only by
	// [Actuator.Revert]. It is how the undo finds the forward step's ledger
	// entry — and therefore the launch template version to point back at —
	// without recomputing a key from partial specs.
	origin string
	// undo marks the inverse step [Actuator.Revert] builds. An undo is
	// authorized by the approval that authorized the change it reverses — see
	// [ApprovedStep.check].
	undo bool
	// authorized is the bit Execute reads. It is separate from the approval's
	// own validity so that the zero ApprovedStep is unambiguously inert even
	// if a future field arrangement makes Approval's zero value look plausible.
	authorized bool
}

// Step returns the underlying step. Reading a step is harmless; acting on one
// is what needs the approval, and there is no path from here back to Execute.
func (s ApprovedStep) Step() domain.Step { return s.step }

// Approved reports whether this value came from [Approval.Authorize].
func (s ApprovedStep) Approved() bool { return s.authorized && s.approval.Valid() }

// Token returns the approving token, zero for an unauthorized step.
func (s ApprovedStep) Token() ApprovalToken { return s.approval.token }

// check re-validates the approval at execution time. An approval that was
// valid when the plan was authorized can expire while a plan runs — a stop, a
// modify and a start take minutes — so the expiry is re-read at every step
// rather than once at the top.
//
// An UNDO is exempt from the expiry and the coverage test, and still requires
// a real approval. The inverse step's key was never in the approved plan (it
// cannot be: the plan approved making the change, not unmaking it), and an
// approval that lapsed while the plan ran is precisely when an undo is most
// needed. [domain.Registry.Revert] skips freeze, breaker and change window for
// the same reason. What an undo can do is bounded elsewhere: it restores the
// From this actuator itself recorded, and every safety predicate still runs.
//
// It takes now as an argument; this package reads no clock.
func (s ApprovedStep) check(now time.Time) error {
	if !s.Approved() {
		return fmt.Errorf("%w: execution requires an approval token (design §3.3 HITL gate)", ErrNotApproved)
	}
	if s.undo {
		return nil
	}
	if !now.IsZero() && !now.Before(s.approval.token.ExpiresAt) {
		return fmt.Errorf("%w: token expired %s, now %s", ErrApprovalExpired,
			s.approval.token.ExpiresAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if !s.approval.Covers(s.step.Key) {
		return fmt.Errorf("%w: %s", ErrStepNotInPlan, s.step.Key)
	}
	return nil
}
