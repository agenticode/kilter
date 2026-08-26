package reason

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const dossierArgs = `{"subject_index":0}`

// TestANilProviderRemovesTheLLMPlaneAndNothingElse is §5.9, asserted.
//
// The requirement is not "degrade gracefully". It is that with no model
// configured the LLM plane is *absent*: interrogation is unavailable and says
// so, and every deterministic capability — this package's own registry
// included — is bit-for-bit what it would have been with a model present.
func TestANilProviderRemovesTheLLMPlaneAndNothingElse(t *testing.T) {
	m := substrate(t)
	withModel := investigator(t, registry(t, m), &scriptedProvider{}, Budget{})
	airGapped, err := New(Config{Registry: registry(t, m), Clock: FixedClock(t0)})
	if err != nil {
		t.Fatalf("an investigator with no provider must still construct: %v", err)
	}

	if airGapped.Available() {
		t.Fatal("an investigator with no provider reports itself available")
	}
	if !withModel.Available() {
		t.Fatal("an investigator with a provider reports itself unavailable")
	}

	f, err := airGapped.Run(context.Background(), Question{Text: "why is cost up", Scope: testScope()})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("Run without a provider returned %v, want ErrNoProvider", err)
	}
	if f != nil {
		t.Fatal("Run without a provider returned a finding; unavailable is not a degraded answer")
	}

	// And the deterministic surface is unchanged, byte for byte.
	if a, b := airGapped.Registry().Digest(), withModel.Registry().Digest(); a != b {
		t.Fatal("the tool surface differs between a configured and an unconfigured reasoner")
	}
	subj := dossierArgs
	for _, tool := range []string{ToolListSubjects, ToolGetDossier, ToolQueryEvidence, ToolClusterTimeline, ToolExplain} {
		args := `{}`
		if tool != ToolListSubjects && tool != ToolClusterTimeline {
			args = subj
		}
		a := call(t, airGapped.Registry(), tool, args)
		b := call(t, withModel.Registry(), tool, args)
		if !a.OK() {
			t.Fatalf("%s refused with no provider configured: %v", tool, a.Refusal)
		}
		if string(a.JSON()) != string(b.JSON()) {
			t.Fatalf("%s answers differently with and without a model", tool)
		}
	}
}

// TestAScriptedTranscriptProducesAByteIdenticalAuditTrail. Same script, same
// substrate, same clock ⇒ same bytes. Map iteration, timestamps and durations
// are the three usual ways this fails, and all three are seams here.
func TestAScriptedTranscriptProducesAByteIdenticalAuditTrail(t *testing.T) {
	m := substrate(t)
	script := func() []ChatResponse {
		return []ChatResponse{
			toolTurn("t1", ToolGetDossier, dossierArgs),
			toolTurn("t2", ToolQueryEvidence, `{"subject_index":1,"limit":9999}`),
			toolTurn("t3", "no_such_tool", `{}`),
			answerTurn("payments-api was redeployed and OOMKilled [e1].", "e1"),
		}
	}
	var first []byte
	for i := 0; i < 4; i++ {
		iv := investigator(t, registry(t, m), &scriptedProvider{turns: script()}, Budget{})
		f, err := iv.Run(context.Background(), Question{Text: "what happened to payments-api", Scope: testScope()})
		if err != nil {
			t.Fatal(err)
		}
		if f.Outcome != OutcomeAnswered {
			t.Fatalf("run %d: outcome %q (%s)", i, f.Outcome, f.Reason)
		}
		b, err := f.Audit().Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Audit().Verify(); err != nil {
			t.Fatalf("run %d: the chain does not verify: %v", i, err)
		}
		if i == 0 {
			first = b
			continue
		}
		if string(b) != string(first) {
			t.Fatalf("run %d produced a different audit trail\n--- first ---\n%s\n--- now ---\n%s", i, first, b)
		}
	}
}

// TestTamperingWithTheAuditTrailIsEvident. Hash-chained, per §5.6.
func TestTamperingWithTheAuditTrailIsEvident(t *testing.T) {
	iv := investigator(t, registry(t, substrate(t)),
		&scriptedProvider{turns: []ChatResponse{
			toolTurn("t1", ToolGetDossier, dossierArgs),
			answerTurn("a deploy happened [e1].", "e1"),
		}}, Budget{})
	f, err := iv.Run(context.Background(), Question{Text: "what happened", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	a := f.Audit()
	if err := a.Verify(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the question after the fact — the single most useful edit for
	// somebody covering their tracks.
	a.records[0].Question.Question = "a much more reasonable question"
	if err := a.Verify(); err == nil {
		t.Fatal("an altered audit record still verifies")
	}
}

// TestTheAuditTrailRecordsWhatWasAskedReturnedAndRefused. A refusal that
// leaves no trace is indistinguishable from a question never asked, which is
// exactly the ambiguity an operator opens this to resolve.
func TestTheAuditTrailRecordsWhatWasAskedReturnedAndRefused(t *testing.T) {
	iv := investigator(t, registry(t, substrate(t)),
		&scriptedProvider{turns: []ChatResponse{
			toolTurn("t1", ToolGetDossier, dossierArgs),
			toolTurn("t2", ToolQueryEvidence, `{"subject_index":9999}`), // refused
			answerTurn("a deploy happened [e1].", "e1"),
		}}, Budget{})
	f, err := iv.Run(context.Background(), Question{Text: "what happened", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	var refusals, served int
	for _, rec := range f.Audit().Records() {
		kinds[rec.Kind]++
		if rec.Tool == nil {
			continue
		}
		if rec.Tool.ArgsDigest == "" {
			t.Error("a tool record does not record what was asked")
		}
		if rec.Tool.Refusal != nil {
			refusals++
			if rec.Tool.Refusal.Code == "" || rec.Tool.Refusal.Detail == "" {
				t.Error("a recorded refusal carries no code or no detail")
			}
			continue
		}
		served++
		if rec.Tool.ResultDigest == "" || rec.Tool.ResultBytes == 0 {
			t.Error("a served tool call does not record what came back")
		}
	}
	if kinds[AuditKindQuestion] != 1 || kinds[AuditKindOutcome] != 1 {
		t.Fatalf("the chain is not bracketed by a question and an outcome: %v", kinds)
	}
	if kinds[AuditKindTurn] != 3 {
		t.Fatalf("recorded %d turns, want 3", kinds[AuditKindTurn])
	}
	if refusals != 1 || served != 1 {
		t.Fatalf("recorded %d refusals and %d served calls, want 1 and 1", refusals, served)
	}
	if f.Refusals != 1 {
		t.Fatalf("the finding reports %d refusals", f.Refusals)
	}
}

// TestEveryRecordedRefusalDetailComesFromTheTable. The anti-echo property,
// checked over a whole run rather than at one call site.
func TestEveryRecordedRefusalDetailComesFromTheTable(t *testing.T) {
	r := registry(t, substrate(t))
	var turns []ChatResponse
	for i, h := range corpus {
		args, err := json.Marshal(map[string]string{"subject_kind": "container", "subject_key": h.value})
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, toolTurn("t"+itoa(i), ToolQueryEvidence, string(args)))
	}
	turns = append(turns, answerTurn("nothing could be read.", ""))
	iv, err := New(Config{
		Provider: &scriptedProvider{turns: turns},
		Registry: r,
		Clock:    StepClock(t0, time.Second),
		Budget:   Budget{MaxToolCallsPerTurn: 8, MaxTurns: 20},
		// This run has no citations by construction: every call is refused.
		AllowUncitedAnswer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := iv.Run(context.Background(), Question{Text: "read everything", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, rec := range f.Audit().Records() {
		if rec.Tool == nil || rec.Tool.Refusal == nil {
			continue
		}
		seen++
		if want := refusalDetail[rec.Tool.Refusal.Code]; rec.Tool.Refusal.Detail != want {
			t.Errorf("refusal %q carries detail %q, which is not the table's",
				rec.Tool.Refusal.Code, rec.Tool.Refusal.Detail)
		}
	}
	if seen != len(corpus) {
		t.Fatalf("saw %d refusals, want %d", seen, len(corpus))
	}
}

// TestBudgetExhaustionIsATerminalStateWithItsOwnOutput.
//
// Neither a success nor an error: a third thing, carrying the work that was
// actually done. The evidence the session read is real and an operator can
// use it; what is missing is the conclusion, and the finding says so.
func TestBudgetExhaustionIsATerminalStateWithItsOwnOutput(t *testing.T) {
	iv := investigator(t, registry(t, substrate(t)),
		&scriptedProvider{turns: []ChatResponse{toolTurn("t1", ToolGetDossier, dossierArgs)}},
		Budget{MaxTokens: 5_000, MinTurnTokens: 2_000, MaxOutputTokensPerTurn: 1_000, MaxTurns: 50})
	f, err := iv.Run(context.Background(), Question{Text: "why", Scope: testScope()})
	if err != nil {
		t.Fatalf("budget exhaustion must not be an error: %v", err)
	}
	if f.Outcome != OutcomeBudgetTokens {
		t.Fatalf("outcome %q, want %q", f.Outcome, OutcomeBudgetTokens)
	}
	if !f.Partial {
		t.Fatal("budget-exhausted work is not marked partial")
	}
	if f.Answer != "" || f.Published() {
		t.Fatal("a partial finding carries an answer")
	}
	if len(f.Evidence) == 0 || len(f.Citations) == 0 {
		t.Fatal("the partial work was not reported: the evidence the session read is missing")
	}
	if f.Usage.Total() > 5_000 {
		t.Fatalf("the loop spent %d tokens against a budget of 5000", f.Usage.Total())
	}
	if f.USDMicro == 0 {
		t.Fatal("the investigation reports no cost; a cost optimizer accounts for its own spend")
	}
	if len(f.Notes) == 0 {
		t.Fatal("a partial finding says nothing about why it is partial")
	}
}

// TestTheTurnCapIsLoweredToWhatRemains. A per-call cap the caller cannot
// lower is a speed limit, not a budget.
func TestTheTurnCapIsLoweredToWhatRemains(t *testing.T) {
	p := &scriptedProvider{turns: []ChatResponse{toolTurn("t1", ToolGetDossier, dossierArgs)}}
	iv := investigator(t, registry(t, substrate(t)), p,
		Budget{MaxTokens: 5_000, MinTurnTokens: 500, MaxOutputTokensPerTurn: 4_000, MaxTurns: 50})
	if _, err := iv.Run(context.Background(), Question{Text: "why", Scope: testScope()}); err != nil {
		t.Fatal(err)
	}
	if len(p.seen) < 3 {
		t.Fatalf("only %d turns ran", len(p.seen))
	}
	if p.seen[0].MaxOutputTokens != 4_000 {
		t.Fatalf("the first turn was capped at %d, want the per-turn cap", p.seen[0].MaxOutputTokens)
	}
	last := p.seen[len(p.seen)-1].MaxOutputTokens
	if last >= 4_000 {
		t.Fatalf("the last turn was still offered %d tokens; the cap never fell to what remained", last)
	}
	for i := 1; i < len(p.seen); i++ {
		if p.seen[i].MaxOutputTokens > p.seen[i-1].MaxOutputTokens {
			t.Fatalf("the turn cap rose between turn %d and %d", i, i+1)
		}
	}
}

// TestThePricedBudgetStopsTheLoop, with the same accounting the savings card
// will show (§5.8).
func TestThePricedBudgetStopsTheLoop(t *testing.T) {
	p := &scriptedProvider{
		turns: []ChatResponse{toolTurn("t1", ToolGetDossier, dossierArgs)},
		info: ProviderInfo{Name: "scripted", Model: "expensive-1",
			USDPerMInput: 15, USDPerMOutput: 75},
	}
	iv := investigator(t, registry(t, substrate(t)), p,
		Budget{MaxTurns: 50, MaxUSDMicro: 100_000}) // $0.10
	f, err := iv.Run(context.Background(), Question{Text: "why", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	if f.Outcome != OutcomeBudgetUSD {
		t.Fatalf("outcome %q, want %q", f.Outcome, OutcomeBudgetUSD)
	}
	if f.USDMicro < 100_000 {
		t.Fatalf("stopped at %d micro-USD, below the budget it was meant to exhaust", f.USDMicro)
	}
	// Priced in integers so two replays compare byte for byte.
	want := p.Info().USDMicro(f.Usage)
	if f.USDMicro != want {
		t.Fatalf("finding priced at %d, recomputed as %d", f.USDMicro, want)
	}
}

// TestAFanOutTurnIsCappedAndTheExtrasAreRefusedNotDropped.
func TestAFanOutTurnIsCappedAndTheExtrasAreRefusedNotDropped(t *testing.T) {
	var calls []ToolCall
	for i := 0; i < 12; i++ {
		calls = append(calls, ToolCall{ID: "t" + itoa(i), Tool: ToolGetDossier, Args: json.RawMessage(dossierArgs)})
	}
	p := &scriptedProvider{turns: []ChatResponse{
		{ToolCalls: calls, Usage: Usage{InputTokens: 500, OutputTokens: 100}},
		answerTurn("a deploy happened [e1].", "e1"),
	}}
	iv := investigator(t, registry(t, substrate(t)), p, Budget{MaxToolCallsPerTurn: 4})
	f, err := iv.Run(context.Background(), Question{Text: "everything", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	if f.ToolCalls != 4 {
		t.Fatalf("%d tool calls ran against a per-turn cap of 4", f.ToolCalls)
	}
	if f.Refusals != 8 {
		t.Fatalf("%d of the 8 over-cap calls were refused; the rest vanished", f.Refusals)
	}
	// Every one of the twelve is in the trail: eight refusals are eight
	// recorded refusals, not eight absences.
	var toolRecords int
	for _, rec := range f.Audit().Records() {
		if rec.Tool != nil {
			toolRecords++
		}
	}
	if toolRecords != 12 {
		t.Fatalf("the trail records %d of 12 attempted calls", toolRecords)
	}
}

// TestAnAnsweredFindingIsPinnedAndPriced (§5.5, INV-5).
func TestAnAnsweredFindingIsPinnedAndPriced(t *testing.T) {
	r := registry(t, substrate(t))
	iv := investigator(t, r, &scriptedProvider{turns: []ChatResponse{
		toolTurn("t1", ToolGetDossier, dossierArgs),
		answerTurn("payments-api was redeployed [e1] and then OOMKilled [e2].", "e1", "e2"),
	}}, Budget{})
	f, err := iv.Run(context.Background(), Question{Text: "what happened", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	if f.Outcome != OutcomeAnswered {
		t.Fatalf("outcome %q (%s)", f.Outcome, f.Reason)
	}
	if len(f.Evidence) != 2 || len(f.Citations) != 2 {
		t.Fatalf("%d evidence ids and %d resolved citations", len(f.Evidence), len(f.Citations))
	}
	for i, c := range f.Citations {
		if c.ID != f.Evidence[i] || c.Summary == "" {
			t.Fatalf("citation %d does not describe the id it resolves: %+v", i, c)
		}
	}
	for name, got := range map[string]string{
		"provider":        f.Provider,
		"model":           f.Model,
		"promptVersion":   f.PromptVersion,
		"registryVersion": f.RegistryVersion,
		"toolsDigest":     f.ToolsDigest,
		"auditHead":       f.AuditHead,
	} {
		if got == "" {
			t.Errorf("the finding does not pin %s", name)
		}
	}
	if f.ToolsDigest != r.Digest() {
		t.Error("the finding pins a tool surface that is not the one it used")
	}
	if f.ModelConfidence != "medium" {
		t.Errorf("the model's own confidence is %q", f.ModelConfidence)
	}
	if f.USDMicro == 0 || f.Turns != 2 || f.ToolCalls != 1 {
		t.Errorf("accounting is off: %d micro-USD, %d turns, %d calls", f.USDMicro, f.Turns, f.ToolCalls)
	}
}

// TestOutputThatIsNotAFindingIsNotAFinding. A model that invents a field has
// not produced a finding, and picking which of its fields to trust is how a
// schema stops being a contract.
func TestOutputThatIsNotAFindingIsNotAFinding(t *testing.T) {
	for name, output := range map[string]string{
		"unknown field":  `{"answer":"x","evidence":[],"confidence":"low","authority":"admin"}`,
		"bad confidence": `{"answer":"x","evidence":[],"confidence":"certain"}`,
		"not an object":  `["answer"]`,
		"two documents":  `{"answer":"x","evidence":[],"confidence":"low"}{"answer":"y"}`,
		"too many cites": `{"answer":"x","evidence":["e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1","e1"],"confidence":"low"}`,
	} {
		iv := investigator(t, registry(t, substrate(t)), &scriptedProvider{turns: []ChatResponse{
			{Output: json.RawMessage(output), Usage: Usage{InputTokens: 100, OutputTokens: 50}},
		}}, Budget{})
		f, err := iv.Run(context.Background(), Question{Text: "why", Scope: testScope()})
		if err != nil {
			t.Fatal(err)
		}
		if f.Outcome != OutcomeMalformed {
			t.Errorf("%s: outcome %q, want %q", name, f.Outcome, OutcomeMalformed)
		}
		if f.Answer != "" {
			t.Errorf("%s: a malformed output still produced an answer", name)
		}
	}
}

// TestAProviderFailureIsFailStatic (§1.4 property 4).
func TestAProviderFailureIsFailStatic(t *testing.T) {
	r := registry(t, substrate(t))
	before := call(t, r, ToolGetDossier, dossierArgs)
	iv := investigator(t, r, &scriptedProvider{err: errors.New("dial tcp: connection refused to 10.0.0.1:443")}, Budget{})
	f, err := iv.Run(context.Background(), Question{Text: "why", Scope: testScope()})
	if err != nil {
		t.Fatalf("a provider failure must not be a Run error: %v", err)
	}
	if f.Outcome != OutcomeProviderFailed {
		t.Fatalf("outcome %q, want %q", f.Outcome, OutcomeProviderFailed)
	}
	// The deterministic plane is untouched, and the provider's error text —
	// which quoted an address — is not in the trail.
	after := call(t, r, ToolGetDossier, dossierArgs)
	if string(before.JSON()) != string(after.JSON()) {
		t.Fatal("a provider failure changed a deterministic answer")
	}
	b, err := f.Audit().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "10.0.0.1") {
		t.Fatal("the provider's error text was copied into the audit trail verbatim")
	}
}

// TestAnAnswerThatCitesNothingIsDiscardedByDefault. §5.7's citation rule is
// what makes prose over this substrate safe; an uncited answer is not a
// weaker cited one, it is a different kind of object.
func TestAnAnswerThatCitesNothingIsDiscardedByDefault(t *testing.T) {
	iv := investigator(t, registry(t, substrate(t)),
		&scriptedProvider{turns: []ChatResponse{answerTurn("everything looks fine.")}}, Budget{})
	f, err := iv.Run(context.Background(), Question{Text: "how are things", Scope: testScope()})
	if err != nil {
		t.Fatal(err)
	}
	if f.Outcome != OutcomeDiscarded || f.Reason != "answer-cited-nothing" {
		t.Fatalf("outcome %q reason %q", f.Outcome, f.Reason)
	}
	if f.Answer != "" {
		t.Fatal("a discarded answer still carries text")
	}
}

// TestTheQuestionAndTheSeedAreSeparateMessages. A formatting step that
// concatenated the engine's seed with the operator's question would be one
// string-builder call away from concatenating a cluster name with an
// instruction.
func TestTheQuestionAndTheSeedAreSeparateMessages(t *testing.T) {
	p := &scriptedProvider{turns: []ChatResponse{answerTurn("fine.", "")}}
	iv := investigator(t, registry(t, substrate(t)), p, Budget{})
	if _, err := iv.Run(context.Background(), Question{Text: "why is cost up", Scope: testScope()}); err != nil {
		t.Fatal(err)
	}
	req := p.seen[0]
	if len(req.Messages) != 2 {
		t.Fatalf("the opening transcript has %d messages", len(req.Messages))
	}
	if req.Messages[0].Text != "" || len(req.Messages[0].Content) == 0 {
		t.Fatal("the seed message carries prose")
	}
	if req.Messages[1].Text != "why is cost up" || len(req.Messages[1].Content) != 0 {
		t.Fatal("the question message carries data")
	}
	if !strings.Contains(req.System, SystemPrompt) || !strings.Contains(req.System, cluster) {
		t.Fatal("the system prompt is missing its constant half or its scope header")
	}
	if string(req.OutputSchema) != string(findingSchema) {
		t.Fatal("the turn did not demand the finding schema")
	}
}

// TestAQuestionOutsideTheRegistrysClusterIsRefusedBeforeATurnIsSpent.
func TestAQuestionOutsideTheRegistrysClusterIsRefusedBeforeATurnIsSpent(t *testing.T) {
	p := &scriptedProvider{turns: []ChatResponse{answerTurn("fine.", "")}}
	iv := investigator(t, registry(t, substrate(t)), p, Budget{})
	if _, err := iv.Run(context.Background(), Question{Text: "why", Scope: Scope{Cluster: "staging"}}); err == nil {
		t.Fatal("a question about another cluster was accepted")
	}
	if _, err := iv.Run(context.Background(), Question{Text: "", Scope: testScope()}); err == nil {
		t.Fatal("an empty question was accepted")
	}
	if p.calls != 0 {
		t.Fatalf("%d model turns were spent on a question that was never valid", p.calls)
	}
}

// TestNewRefusesAnInvestigatorWithoutASeam.
func TestNewRefusesAnInvestigatorWithoutASeam(t *testing.T) {
	r := registry(t, substrate(t))
	if _, err := New(Config{Registry: r}); err == nil {
		t.Error("New accepted an investigator with no clock")
	}
	if _, err := New(Config{Clock: FixedClock(t0)}); err == nil {
		t.Error("New accepted an investigator with no registry")
	}
	if _, err := New(Config{Registry: r, Clock: FixedClock(t0),
		Budget: Budget{MaxTokens: 100, MaxOutputTokensPerTurn: 4096}}); err == nil {
		t.Error("New accepted a budget that lets one turn exceed the whole investigation")
	}
}
