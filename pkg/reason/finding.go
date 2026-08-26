package reason

import (
	"bytes"
	"encoding/json"

	"github.com/agenticode/kilter/pkg/explain"
)

// Terminal states. An investigation ends in exactly one of these, and three
// of them are neither a success nor an error.
//
// This is the same shape pkg/decision gives a refusal: a bounded process that
// stops early has produced a *result*, and modelling that as an error throws
// away the part of it that was worth having. A caller switches on
// [Finding.Outcome]; it does not infer the state from whether err is nil.
const (
	// OutcomeAnswered: a structured finding arrived and every citation in it
	// was both fetched this session and re-resolved against the substrate.
	// The only state in which [Finding.Answer] is non-empty.
	OutcomeAnswered = "answered"

	// OutcomeTurnLimit: the loop hit MaxTurns. Partial.
	OutcomeTurnLimit = "turn-limit"

	// OutcomeBudgetTokens: the token budget could not fund another turn.
	// Partial.
	OutcomeBudgetTokens = "budget-exhausted-tokens"

	// OutcomeBudgetUSD: the priced budget is spent. Partial.
	OutcomeBudgetUSD = "budget-exhausted-usd"

	// OutcomeDiscarded: an answer arrived and was thrown away because its
	// citations did not hold up. Not partial — there is nothing partial
	// about it, the answer is gone.
	OutcomeDiscarded = "discarded"

	// OutcomeMalformed: the model returned something that is not a finding,
	// or stopped without producing one.
	OutcomeMalformed = "malformed-finding"

	// OutcomeProviderFailed: the model server failed. The deterministic
	// engine is unaffected; that is the §1.4 fail-static property, and this
	// state is what it looks like from here.
	OutcomeProviderFailed = "provider-failed"
)

// Why a discard rather than an annotation.
//
// The tempting alternative is to publish the answer with "3 of 7 citations
// could not be verified" attached. It fails for a reason that has nothing to
// do with models: an annotation is a second thing to read, and the answer is
// the first. A platform engineer forwards the paragraph to an app team; the
// annotation does not travel with it. A dashboard renders the answer in a
// card and the warning in a tooltip. A summary quotes the conclusion.
//
// Worse, the annotation is *most* likely to be ignored in exactly the case it
// matters: a confident, fluent, well-structured answer whose citations happen
// not to resolve is what a successful injection produces. Publishing it with
// a caveat converts a structural defense into a UX one, and §5.7 puts
// prompt-level and presentation-level defenses last for that reason.
//
// So the answer is discarded and the failure is recorded. What survives is
// the audit trail — which cites nothing, claims nothing, and shows exactly
// which citations failed and how.
const discardRationale = "answer discarded: at least one citation was not fetched in this session or did not re-resolve"

// Hypothesis is speculation, labelled. §1.3(c): hypotheses feed humans, never
// the planner, and they are never inputs to sizing.
type Hypothesis struct {
	Statement string `json:"statement"`
	Basis     string `json:"basis,omitempty"`
	// Speculative is always true. It is a field rather than a convention so
	// that a consumer which loses this type still sees the label.
	Speculative bool `json:"speculative"`
}

// Finding is one investigation's artifact.
type Finding struct {
	Question string `json:"question"`
	Scope    Scope  `json:"scope"`

	// Outcome is one of the terminal states above.
	Outcome string `json:"outcome"`
	// Partial marks work that stopped early. The evidence is real; the
	// conclusion is missing.
	Partial bool `json:"partial"`
	// Reason is a machine code — a refusal code or a terminal state — never
	// free text, and never a quoted argument.
	Reason string `json:"reason,omitempty"`

	// Answer is empty unless Outcome is OutcomeAnswered.
	Answer string `json:"answer,omitempty"`
	// Confidence is the MODEL's own, kept lexically apart from the engine's
	// scored confidence, which lives in the explain payload. Two numbers
	// called confidence that mean different things must never be one field.
	ModelConfidence string       `json:"modelConfidence,omitempty"`
	Hypotheses      []Hypothesis `json:"hypotheses,omitempty"`

	// Evidence is the citation set, as real IDs, in sorted order.
	Evidence []explain.ID `json:"evidence,omitempty"`
	// Citations is the same set resolved: what each ID actually is.
	Citations []explain.Citation `json:"citations,omitempty"`
	// Notes are this package's own remarks. They never quote substrate text.
	Notes []string `json:"notes,omitempty"`

	Turns     int   `json:"turns"`
	ToolCalls int   `json:"toolCalls"`
	Refusals  int   `json:"refusals"`
	Usage     Usage `json:"usage"`
	// USDMicro is this investigation's own inference cost in millionths of a
	// dollar (§5.8: a cost optimizer accounts for its own spend).
	USDMicro int64 `json:"usdMicro"`

	Provider        string `json:"provider"`
	Model           string `json:"model"`
	PromptVersion   string `json:"promptVersion"`
	RegistryVersion string `json:"registryVersion"`
	ToolsDigest     string `json:"toolsDigest"`
	// AuditHead is the last audit record's hash — the handle by which this
	// finding is tied to the transcript that produced it.
	AuditHead string `json:"auditHead"`

	// Publish-gate bookkeeping. Unexported because the numbers belong in the
	// audit record, where they sit next to the digests that identify which
	// citations failed; a consumer that wants them reads the trail.
	unresolvable  int
	unfetched     int
	rejectDigests []string
	linksStripped int

	audit *Audit
}

// Published reports whether this finding may be shown as an answer.
func (f *Finding) Published() bool { return f != nil && f.Outcome == OutcomeAnswered }

// Audit returns the investigation's chain. It is never nil for a finding this
// package produced.
func (f *Finding) Audit() *Audit { return f.audit }

// modelFinding is the structured output a final turn must produce. It is
// decoded with DisallowUnknownFields: a model that invents a field has not
// produced a finding, and guessing which of its fields to trust is how a
// schema stops being a contract.
type modelFinding struct {
	Answer     string   `json:"answer"`
	Evidence   []string `json:"evidence"`
	Confidence string   `json:"confidence"`
	Hypotheses []struct {
		Statement string `json:"statement"`
		Basis     string `json:"basis"`
	} `json:"hypotheses"`
}

// Bounds on a model's structured output, mirrored in findingSchema.
const (
	maxFindingCitations = 32
	maxFindingHypos     = 8
	maxHypoText         = 512
	maxHandleLen        = 16
)

// findingSchema is the strict output schema. Citations are *handles* — see
// [session.handleFor] — not evidence IDs, which is why maxLength is 16.
var findingSchema = json.RawMessage(`{
"type":"object",
"additionalProperties":false,
"required":["answer","evidence","confidence"],
"properties":{
"answer":{"type":"string","maxLength":32768,"description":"markdown. Every claim must cite a handle from the evidence list."},
"evidence":{"type":"array","maxItems":32,"items":{"type":"string","maxLength":16},"description":"citation handles exactly as they appeared in a tool result this session, e.g. e3. Handles you did not see are rejected and the whole answer is discarded."},
"confidence":{"type":"string","enum":["low","medium","high"],"description":"your own confidence, which is not the engine's"},
"hypotheses":{"type":"array","maxItems":8,"items":{"type":"object","additionalProperties":false,"required":["statement"],"properties":{"statement":{"type":"string","maxLength":512},"basis":{"type":"string","maxLength":512}}},"description":"speculation, labelled as such; never an input to sizing"}
}}`)

// decodeFinding parses a model's structured output.
func decodeFinding(raw json.RawMessage) (*modelFinding, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var mf modelFinding
	if err := dec.Decode(&mf); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	if len(mf.Evidence) > maxFindingCitations || len(mf.Hypotheses) > maxFindingHypos {
		return nil, false
	}
	switch mf.Confidence {
	case "low", "medium", "high":
	default:
		return nil, false
	}
	return &mf, true
}
