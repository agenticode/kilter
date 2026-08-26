package explain

import (
	"errors"
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// This file answers one question about a finished payload: how much of what
// it asserts stands on evidence the store actually holds *for the subject*.
//
// The question exists because BuildExplain answers for any well-formed
// subject, whether or not the substrate has ever heard of it. A mistyped
// container name therefore produced a payload — an honest one, it says it
// grounds nothing — which the HTTP route served as 200 and the CLI printed
// with exit 0 (cmd/BRAINWIRE-FINDINGS.md §6.1). A confident-looking answer to
// a question about a subject that does not exist is the failure this file
// makes typed.

// GroundingState says how much of the payload stands on stored evidence.
//
// The load-bearing distinction is absent vs thin, and it is not a matter of
// degree:
//
//   - **absent** is a statement about the *subject*. The store returned no
//     record of any kind for it in this window — no digest in any tier, no
//     event, no decision, and nothing a cap withheld. Either the subject was
//     mistyped or it never ran here. There is nothing to explain, and saying
//     so is the only answer that is not a guess.
//   - **thin** is a statement about the *history*. Records exist; they are
//     just too few, or of the wrong kind, for any driver to stand on. That is
//     a legitimate answer callers ask for on purpose — a young workload has
//     thin history and an operator still wants to see its usage and its
//     window — so it must stay a payload.
//
// Collapsing the two either deletes the thin answer or hands a typo a 200.
type GroundingState string

const (
	// GroundingGrounded: the store holds records for this subject in this
	// window, and at least one driver cites them.
	GroundingGrounded GroundingState = "grounded"
	// GroundingThin: records exist for this subject, but no driver could be
	// grounded in them, so the payload cites nothing. An answer, not an
	// error: the usage summary, the window and the sizing are all still
	// true, they simply have no citable reason attached.
	GroundingThin GroundingState = "thin"
	// GroundingAbsent: the store holds no record of this subject in this
	// window at all. This is the state [Explanation.GroundingError] turns
	// into an error — a 422 and a non-zero exit — and the only one it does.
	GroundingAbsent GroundingState = "absent"
	// GroundingUnknown: the payload carries no [Grounding] report and cites
	// nothing, so whether evidence exists was never computed. BuildExplain
	// never produces this; a hand-built payload, or one decoded from a
	// document whose grounding object was stripped, does. It deliberately
	// does NOT read as absent: absence is a computed fact, and a payload
	// that did not compute it may not claim it.
	GroundingUnknown GroundingState = "unknown"
)

// Grounding is the arithmetic behind [GroundingState] — the counts the state
// was computed from, so a reader can check the verdict rather than trust it.
//
// Every count except ParentEvents is a record the store returned *for the
// subject*. ParentEvents is kept out of that sum on purpose: they are the
// parent workload's deploys and HPA actions, borrowed by BuildExplain so a
// container's post-change refusal can be explained at all. They describe the
// workload. A container that never ran under a workload that deploys weekly
// has a parent with plenty of events and still has no history of its own.
type Grounding struct {
	State GroundingState `json:"state"`

	// Digests, Events and Decisions are what the dossier returned for the
	// subject after its caps were applied.
	Digests   int `json:"digests"`
	Events    int `json:"events"`
	Decisions int `json:"decisions"`
	// UsageWindows and Samples come from the dossier's usage summary, which
	// is computed by a query of its own across every stored tier and is not
	// gated by MaxDigests. It is therefore the one witness a caller cannot
	// suppress, and the reason "no digests in the requested tier" can never
	// be mistaken for "no usage data".
	UsageWindows int   `json:"usageWindows"`
	Samples      int64 `json:"samples"`
	// Withheld counts records the store did return for the subject that a
	// count cap or the byte cap dropped before the payload saw them. It is
	// what makes absence honest under a caller who asked for nothing:
	// MaxEvents < 0 empties Events, but the dossier still reports what it
	// dropped, so the section was queried and the answer is "records exist",
	// not "no records".
	Withheld int `json:"withheld,omitempty"`
	// ParentEvents counts change events borrowed from the parent workload.
	// Not evidence about the subject; see the type comment.
	ParentEvents int `json:"parentEvents,omitempty"`
	// Citations is len(Explanation.Citations) — how many ids the finished
	// payload actually leans on.
	Citations int `json:"citations"`
}

// Any reports whether the store holds any record of the subject itself in
// this window. It is the absent/not-absent test, and the whole of it.
func (g Grounding) Any() bool {
	return g.Digests > 0 || g.Events > 0 || g.Decisions > 0 ||
		g.UsageWindows > 0 || g.Samples > 0 || g.Withheld > 0
}

// stateFor resolves the three computable states. Citations is passed rather
// than read off the struct so the caller cannot forget to set it first.
func (g Grounding) stateFor(citations int) GroundingState {
	switch {
	case !g.Any():
		return GroundingAbsent
	case citations == 0:
		return GroundingThin
	default:
		return GroundingGrounded
	}
}

// ErrNoEvidence reports a subject the store holds no record of in the
// requested window. Callers match it with errors.Is; pkg/api maps it to 422
// and cmd/kilter to a non-zero exit.
var ErrNoEvidence = errors.New("explain: no evidence is stored for this subject in this window")

// NoEvidenceError is the typed form, carrying the subject, the window and
// the counts the answer was computed from — everything an operator needs to
// tell "I mistyped the name" from "I asked about the wrong window".
//
// It wraps [ErrNoEvidence]. The payload is deliberately not attached: a
// caller that wants it already has it, because this error is produced from a
// payload rather than instead of one.
type NoEvidenceError struct {
	Subject   evidence.SubjectRef
	From, To  time.Time
	Grounding Grounding
}

func (e *NoEvidenceError) Error() string {
	s := fmt.Sprintf("explain: no evidence is stored for subject %s in [%s, %s)",
		e.Subject, e.From.UTC().Format(time.RFC3339), e.To.UTC().Format(time.RFC3339))
	if e.Grounding.ParentEvents > 0 {
		s += fmt.Sprintf("; the %d citation%s in the payload %s borrowed from the parent workload",
			e.Grounding.ParentEvents, pluralS(e.Grounding.ParentEvents),
			pluralVerb(e.Grounding.ParentEvents, "is", "are"))
	}
	return s
}

func (e *NoEvidenceError) Unwrap() error { return ErrNoEvidence }

// GroundingState reports how much of this payload stands on stored evidence.
// It never returns "".
//
// A payload BuildExplain produced always carries a [Grounding] report unless
// it is grounded, so the fallbacks below only fire for a hand-built or
// re-decoded payload: citations mean grounded, and nothing at all means
// [GroundingUnknown], never absent.
func (e *Explanation) GroundingState() GroundingState {
	if e.Grounding != nil && e.Grounding.State != "" {
		return e.Grounding.State
	}
	if len(e.Citations) > 0 {
		return GroundingGrounded
	}
	return GroundingUnknown
}

// GroundingError returns a [*NoEvidenceError] when, and only when, the store
// held no record of this subject in this window — the state a caller turns
// into a 422 and a non-zero exit. It returns nil for thin history, because
// thin history is an answer, and nil for [GroundingUnknown], because a
// payload that never computed absence must not assert it.
//
// It is a method rather than a second return value from BuildExplain on
// purpose: BuildExplain's signature is load-bearing in pkg/api and cmd/, and
// an additive check breaks no caller that has not adopted it. The cost is
// that a caller can forget to call it; VERDICT-FINDINGS.md §5 gives both call
// sites verbatim.
func (e *Explanation) GroundingError() error {
	if e.GroundingState() != GroundingAbsent {
		return nil
	}
	var g Grounding
	if e.Grounding != nil {
		g = *e.Grounding
	}
	return &NoEvidenceError{Subject: e.Subject, From: e.From, To: e.To, Grounding: g}
}
