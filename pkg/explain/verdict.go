package explain

import (
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/recommend"
)

// This file plumbs the production path's verdict into the payload, and
// refuses to invent one when the production path did not reach one.
//
// The tempting shortcut is to import pkg/decision here — this file already
// does, for the types — and call decision.Evaluate on evidence assembled on
// the spot. pkg/recommend/FINDINGS.md §6.4-1 refused that and
// pkg/recommend/VERDICT-FINDINGS.md §1 refused it again: a second evaluation
// is a second opinion, and it can differ from the one the operational path
// actually served. An explanation reporting a verdict the engine never
// reached is not a weaker audit trail, it is a fabricated one.
//
// So there is exactly one way a decision.Verdict enters this package: a
// caller hands one over, and it is copied into the payload unchanged. This
// file constructs no Verdict, no Refusal and no Confidence, and calls nothing
// in pkg/decision. What it adds is the third state — the one that was
// previously indistinguishable from the second.

// VerdictState says whether a decision-quality verdict (pkg/decision) stands
// behind [Explanation.Action]. Three facts, and the payload could previously
// only tell two of them apart:
//
//	refused       Action == "refuse", Refusal != nil. A verdict exists and a
//	              refusal predicate fired. Citable, with a code and a detail.
//	not-computed  Action == "unknown", VerdictOrigin.State == "not-computed".
//	              The production path considered this subject and took a named
//	              branch, but evaluated no refusal predicate and chose no
//	              action, so no verdict exists to report. Refusal is nil.
//	unknown       Action == "unknown", VerdictOrigin == nil. Nobody said
//	              anything about this subject — not even whether the engine
//	              looked at it.
//
// The first is a decision. The second and third are both absences, and they
// are different absences: "the engine considered it and reached no verdict"
// is a fact about the engine, "nothing was supplied" is a fact about the
// caller. Neither is a negative verdict, and rendering either as one would be
// the lie this package exists to prevent.
type VerdictState string

const (
	// VerdictUnknown: no verdict and no readout was supplied.
	VerdictUnknown VerdictState = "unknown"
	// VerdictNotComputed: a production readout was supplied and it reports
	// that no decision-quality verdict exists. This is what every
	// recommend.Verdict says today (pkg/recommend/VERDICT-FINDINGS.md §1).
	VerdictNotComputed VerdictState = "not-computed"
	// VerdictComputed: a decision.Verdict was reached on the production path
	// and this payload reports it verbatim.
	VerdictComputed VerdictState = "computed"
)

// VerdictOrigin records where Action came from when it did not come from a
// verdict — which of the two absences applies, and what the recommendation
// path did instead.
//
// It is absent from a payload whose verdict was computed, because for that
// case Action, Confidence and Refusal already say everything and Action
// itself is the machine-readable proof (only a real verdict can set it to
// act, recommend-only or refuse). Use [Explanation.VerdictState], which
// resolves all three states from whichever fields are present.
type VerdictOrigin struct {
	State VerdictState `json:"state"`
	// Disposition is the recommendation path's own word for the branch it
	// took: recommended, never-observed, insufficient-history or
	// no-significant-change. It is NOT a refusal code and must never be
	// rendered as one — recommend.DispositionInsufficientHistory and
	// decision.CodeInsufficientHistory are two thresholds in two
	// independently settable Configs that happen to share their defaults
	// (pkg/recommend/VERDICT-FINDINGS.md §1.2).
	Disposition string `json:"disposition,omitempty"`
	// Samples and Window are the learned history the disposition was
	// reached on, straight off the readout. Zero for never-observed.
	Samples int           `json:"samples"`
	Window  time.Duration `json:"window"`
}

// originOf projects a production readout into the payload's own vocabulary.
// It copies; it derives nothing.
func originOf(rv *recommend.Verdict) *VerdictOrigin {
	return &VerdictOrigin{
		State:       VerdictNotComputed,
		Disposition: string(rv.Disposition),
		Samples:     rv.Samples,
		Window:      rv.Window,
	}
}

// notComputedNote is the sentence that keeps "no verdict" from reading as
// "a negative verdict" in a payload a human or a narrating model will read.
func (o *VerdictOrigin) notComputedNote() string {
	return fmt.Sprintf("no decision verdict was computed on the production path; "+
		"the recommendation path's disposition for this subject was %q (%d sample%s over %s). "+
		"An absent verdict is not a refusal.",
		o.Disposition, o.Samples, pluralS(o.Samples), o.Window)
}

// VerdictState reports which of the three states this payload is in. It
// never returns "".
//
// The computed case is derived from Action rather than stored, so a payload
// built before this field existed — or decoded from one — still answers
// correctly: only a supplied decision.Verdict can make Action anything other
// than [ActionUnknown], and BuildExplain rejects a verdict whose Action is
// not one of pkg/decision's three.
func (e *Explanation) VerdictState() VerdictState {
	if e.VerdictOrigin != nil && e.VerdictOrigin.State != "" {
		return e.VerdictOrigin.State
	}
	switch decision.Action(e.Action) {
	case decision.ActionAct, decision.ActionRecommendOnly, decision.ActionRefuse:
		return VerdictComputed
	}
	return VerdictUnknown
}

// Refused reports a payload carrying an actual refusal — a verdict was
// computed and a refusal predicate fired. It is false for both absences, and
// that is the distinction the type exists to make: a caller asking "did the
// engine say no?" must not get "yes" from a subject the engine never judged.
func (e *Explanation) Refused() bool {
	return e.VerdictState() == VerdictComputed &&
		decision.Action(e.Action) == decision.ActionRefuse
}
