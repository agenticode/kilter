package reason

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/agenticode/kilter/pkg/explain"
)

// Config builds an [Investigator].
type Config struct {
	// Provider is the model. A nil Provider is not an error at construction
	// — it is the §5.9 air-gapped posture, and [Investigator.Available]
	// reports it. Every deterministic capability, this package's registry
	// included, works with it nil.
	Provider Provider
	// Registry is the tool surface. Required.
	Registry *Registry
	// Clock is required. See clock.go for why it is not defaulted.
	Clock Clock
	// Budget bounds the loop. Zero fields take [DefaultBudget].
	Budget Budget
	// Seed is the ranked candidate list for context assembly (§5.4).
	Seed []Candidate
	// SeedStubs caps the seed; default [DefaultSeedStubs].
	SeedStubs int
	// AllowUncitedAnswer publishes an answer that cites nothing.
	//
	// Default false, and the default is the point: §5.7's citation rule is
	// what makes prose over this substrate safe, and an answer with no
	// citations is not a weaker version of a cited one, it is a different
	// kind of object. Set it only for a deployment that has decided prose
	// quality matters more than groundedness, and write down who decided.
	AllowUncitedAnswer bool
}

// Investigator runs the loop of §5.3.
type Investigator struct {
	provider Provider
	registry *Registry
	clock    Clock
	budget   Budget
	seed     []Candidate
	seedK    int
	uncited  bool
}

// New builds an investigator.
func New(cfg Config) (*Investigator, error) {
	if cfg.Registry == nil {
		return nil, fmt.Errorf("reason: an investigator needs a tool registry")
	}
	if cfg.Clock == nil {
		return nil, fmt.Errorf("reason: an investigator needs a clock; this package never reads time.Now")
	}
	b := cfg.Budget.withDefaults()
	if err := b.validate(); err != nil {
		return nil, err
	}
	k := cfg.SeedStubs
	if k == 0 {
		k = DefaultSeedStubs
	}
	return &Investigator{
		provider: cfg.Provider,
		registry: cfg.Registry,
		clock:    cfg.Clock,
		budget:   b,
		seed:     append([]Candidate(nil), cfg.Seed...),
		seedK:    k,
		uncited:  cfg.AllowUncitedAnswer,
	}, nil
}

// Available reports whether a model is configured.
//
// The §5.9 contract in one method: when this is false, `kilter ask` is
// unavailable and says so, and *nothing else changes*. Sizing, plans, safety,
// guardrails, ledger, approvals, explain payloads, backtest, what-if and cost
// attribution are all computed below this package and do not consult it. The
// LLM plane is removed, not degraded.
func (iv *Investigator) Available() bool { return iv != nil && iv.provider != nil }

// Registry exposes the tool surface. It is deliberately reachable without a
// provider: §5.9's subtle row is that MCP serves the deterministic tools with
// no model configured at all.
func (iv *Investigator) Registry() *Registry {
	if iv == nil {
		return nil
	}
	return iv.registry
}

// Question is what an operator asked.
type Question struct {
	Text string
	// Scope must match the registry's; a question about another cluster is
	// a different investigation with a different registry.
	Scope Scope
	// Initiator is the API-token identity of the caller (§5.6). MCP callers
	// are just another identity.
	Initiator string
}

// session is one investigation's mutable state. Nothing here is package
// state, so two investigations cannot see each other.
type session struct {
	iv    *Investigator
	q     Question
	audit *Audit
	spend spend

	msgs     []Message
	refusals int

	// handleFor and idFor are the citation ledger. Handles are opaque,
	// session-local tokens; see handleFor.
	handleFor map[explain.ID]string
	idFor     map[string]explain.ID
	issued    []explain.ID // issue order, for deterministic emission
}

// Run executes the loop of §5.3.
//
// It returns an error only when the investigation could not start: no
// provider (see [ErrNoProvider]), or a question that is not well formed.
// Everything else — budget exhaustion, a turn limit, a provider failure, an
// answer thrown away for bad citations — is a terminal state on the finding,
// because each of those produced a *result* an operator needs to see.
func (iv *Investigator) Run(ctx context.Context, q Question) (*Finding, error) {
	if iv == nil {
		return nil, ErrNoProvider
	}
	if iv.provider == nil {
		return nil, ErrNoProvider
	}
	if err := q.validate(iv.registry.Scope()); err != nil {
		return nil, err
	}
	s := &session{
		iv:        iv,
		q:         q,
		audit:     newAudit(iv.clock),
		spend:     spend{budget: iv.budget},
		handleFor: map[explain.ID]string{},
		idFor:     map[string]explain.ID{},
	}

	info := iv.provider.Info()
	system := iv.systemPrompt()
	seed, err := buildSeed(iv.registry.Scope(), iv.seed, iv.seedK, defaultSeedBytes)
	if err != nil {
		return nil, err
	}
	seedJSON, err := json.Marshal(seed)
	if err != nil {
		return nil, err
	}
	question, _ := scrubText(q.Text, maxQuestionBytes)

	s.audit.append(AuditKindQuestion, func(rec *AuditRecord) {
		sc := iv.registry.Scope()
		rec.Question = &AuditQuestion{
			Question:        question,
			Initiator:       q.Initiator,
			Cluster:         sc.Cluster,
			Subject:         sc.Subject.String(),
			From:            sc.From.UTC().Format(rfc3339),
			To:              sc.To.UTC().Format(rfc3339),
			Provider:        info.Name,
			Model:           info.Model,
			PromptVersion:   PromptVersion,
			RegistryVersion: RegistryVersion,
			ToolsDigest:     iv.registry.Digest(),
			SystemDigest:    digestString(system),
			SeedDigest:      digest(seedJSON),
			Budget:          iv.budget,
		}
	})

	// The question is the operator's, the seed is the engine's; they are
	// separate messages so that no formatting step can splice one into the
	// other.
	s.msgs = []Message{
		{Role: RoleUser, Content: seedJSON, Tool: "scope"},
		{Role: RoleUser, Text: question},
	}

	tools := iv.registry.Tools()
	for {
		if state := s.spend.exhausted(); state != "" {
			return s.partial(state), nil
		}
		req := ChatRequest{
			System:          system,
			Messages:        append([]Message(nil), s.msgs...),
			Tools:           tools,
			OutputSchema:    findingSchema,
			MaxOutputTokens: s.spend.turnCap(),
		}
		turn := s.spend.turns + 1
		resp, err := iv.provider.Chat(ctx, req)
		if err != nil {
			s.audit.append(AuditKindTurn, func(rec *AuditRecord) {
				rec.Turn = &AuditTurn{
					Turn:            turn,
					RequestDigest:   requestDigest(req),
					Messages:        len(req.Messages),
					MaxOutputTokens: req.MaxOutputTokens,
					// The provider's error text may quote a request body, so
					// it is recorded as a digest rather than verbatim.
					Error: digestString(err.Error()),
				}
			})
			return s.terminal(OutcomeProviderFailed, OutcomeProviderFailed, false), nil
		}
		usd := info.USDMicro(resp.Usage)
		s.spend.charge(resp.Usage, usd)

		callNames := make([]string, 0, len(resp.ToolCalls))
		for _, c := range resp.ToolCalls {
			safe, _ := scrubText(c.Tool, 64)
			callNames = append(callNames, safe)
		}
		s.audit.append(AuditKindTurn, func(rec *AuditRecord) {
			rec.Turn = &AuditTurn{
				Turn:            turn,
				RequestDigest:   requestDigest(req),
				Messages:        len(req.Messages),
				MaxOutputTokens: req.MaxOutputTokens,
				TextDigest:      digestString(resp.Text),
				OutputDigest:    digest(resp.Output),
				ToolCalls:       callNames,
				StopReason:      resp.StopReason,
				Usage:           resp.Usage,
				USDMicro:        usd,
			}
		})

		if len(resp.Output) > 0 {
			return s.publish(resp.Output), nil
		}
		if len(resp.ToolCalls) == 0 {
			// A turn that neither answered nor asked for anything. Prose
			// without the structured output is not a finding: it has no
			// citation list, so there is nothing to verify and nothing that
			// may be published.
			return s.terminal(OutcomeMalformed, "no-structured-finding", false), nil
		}

		text, _ := scrubText(resp.Text, maxAnswerBytes)
		s.msgs = append(s.msgs, Message{
			Role:  RoleAssistant,
			Text:  text,
			Calls: resp.ToolCalls,
		})
		s.runCalls(ctx, turn, resp.ToolCalls)
	}
}

// runCalls executes one turn's tool calls in the order the model asked for
// them, appending each result to the transcript.
func (s *session) runCalls(ctx context.Context, turn int, calls []ToolCall) {
	for i, call := range calls {
		if !s.spend.toolCallAllowed(i) {
			ref := refuseAt(CodeToolCallCap, "", int64(s.spend.budget.MaxToolCallsPerTurn))
			// The name is repeated back only if it is one of ours. A capped
			// call to a tool that does not exist would otherwise be a way to
			// get an arbitrary string into the transcript without ever
			// passing a schema.
			safe, _ := scrubText(call.Tool, 64)
			known := s.iv.registry.registered(safe)
			if known {
				ref.Tool = safe
			}
			out := Outcome{Call: ToolCall{ID: call.ID, Tool: safe}, Refusal: ref, known: known}
			s.deliver(turn, call, out, nil)
			continue
		}
		out := s.iv.registry.Run(ctx, call)
		s.spend.toolCalls++
		handles := s.record(out.Cites)
		s.deliver(turn, call, out, handles)
	}
}

// deliver audits one tool call and appends its envelope to the transcript.
func (s *session) deliver(turn int, call ToolCall, out Outcome, handles []string) {
	env := out.envelope(handles)
	if out.Refusal != nil {
		s.refusals++
	}
	// The raw argument bytes are the model's, and may be the attacker's.
	// They are recorded exactly (a digest) and legibly (scrubbed), and never
	// quoted into the refusal that goes back into the transcript.
	shown, _, err := scrubJSON(call.Args, maxDisplayText)
	if err != nil || len(shown) > 1024 {
		shown = nil
	}
	s.audit.append(AuditKindTool, func(rec *AuditRecord) {
		rec.Tool = &AuditTool{
			Turn:         turn,
			Tool:         out.Call.Tool,
			CallID:       call.ID,
			ArgsDigest:   digest(call.Args),
			Args:         shown,
			Clamps:       out.Clamps,
			Refusal:      out.Refusal,
			ResultDigest: digest(out.Data),
			ResultBytes:  out.Bytes,
			Scrubbed:     out.Scrubbed,
			Citations:    handleLines(handles, out.Cites),
		}
	})
	// The message names the tool only when the name is this package's own.
	// A tool-result message is transcript, and a transcript is context: an
	// unregistered name in that field is a string the model wrote arriving
	// back in the model's input with nothing between.
	named := ""
	if out.known {
		named = out.Call.Tool
	}
	s.msgs = append(s.msgs, Message{
		Role:       RoleTool,
		ToolCallID: call.ID,
		Tool:       named,
		Content:    env,
	})
}

// record issues a handle for each newly-seen citation and returns the handles
// in the order the tool listed them.
func (s *session) record(ids []explain.ID) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		h, seen := s.handleFor[id]
		if !seen {
			h = "e" + strconv.Itoa(len(s.issued)+1)
			s.handleFor[id] = h
			s.idFor[h] = id
			s.issued = append(s.issued, id)
		}
		out = append(out, h)
	}
	return out
}

// Why the model is shown handles instead of evidence IDs.
//
// An evidence ID embeds the subject key, and a subject key is a workload name
// somebody with kubectl wrote. Showing IDs would put attacker bytes into the
// one channel that must survive a round trip through the model untouched:
// scrubbing an ID breaks the resolve, and not scrubbing it hands a rendering
// exploit to whoever reads the answer.
//
// A handle is `e` plus an ordinal, issued in first-appearance order. It
// carries no cluster bytes, it is short (so a citation costs almost no
// tokens), and it makes "the model cannot cite what it did not read" exact
// rather than probabilistic: an unissued handle is not in the ledger, and
// there is no ID to guess. [Finding.Evidence] carries the real IDs, mapped
// back after verification, so the artifact §5.3 specifies is unchanged.
//
// The cost is that a handle is meaningless outside its session. That is why
// the audit trail records the mapping, and why §6's MCP frontend — which
// talks to a client that holds its own session — uses [Outcome.JSON] and real
// IDs instead.
func handleLines(handles []string, ids []explain.ID) []string {
	if len(handles) == 0 {
		return nil
	}
	out := make([]string, 0, len(handles))
	for i, h := range handles {
		if i < len(ids) {
			out = append(out, h+" "+string(ids[i]))
		}
	}
	sort.Strings(out)
	return out
}

// publish is the citation gate. An answer arrives here; whether it leaves is
// decided entirely by whether its citations hold up.
func (s *session) publish(raw json.RawMessage) *Finding {
	mf, ok := decodeFinding(raw)
	if !ok {
		return s.terminal(OutcomeMalformed, "output-does-not-match-schema", false)
	}

	var (
		ids           []explain.ID
		cites         []explain.Citation
		unfetched     int
		unresolvable  int
		rejectDigests []string
	)
	for _, h := range mf.Evidence {
		if len(h) > maxHandleLen {
			unfetched++
			rejectDigests = append(rejectDigests, digestString(h))
			continue
		}
		id, issued := s.idFor[h]
		if !issued {
			// The model cited something it was never shown. This is the
			// fabrication case, and it is the one that must not be
			// survivable by adding a caveat.
			unfetched++
			rejectDigests = append(rejectDigests, digestString(h))
			continue
		}
		c, err := s.iv.registry.Resolve(id)
		if err != nil {
			// Shown once, gone now: pruned, evicted, or a store that
			// disagrees with itself. Either way the claim is no longer
			// grounded, and pkg/explain's rule applies — do not print the
			// claim, rather than print it without the citation.
			unresolvable++
			rejectDigests = append(rejectDigests, digestString(string(id)))
			continue
		}
		ids = append(ids, id)
		cites = append(cites, c)
	}

	f := s.finding()
	f.unresolvable, f.unfetched = unresolvable, unfetched
	sort.Strings(rejectDigests)
	f.rejectDigests = rejectDigests

	if unfetched+unresolvable > 0 {
		f.Outcome = OutcomeDiscarded
		f.Reason = "citations-did-not-verify"
		f.Notes = append(f.Notes, discardRationale)
		s.closeAudit(f, nil)
		return f
	}
	if len(ids) == 0 && !s.iv.uncited {
		f.Outcome = OutcomeDiscarded
		f.Reason = "answer-cited-nothing"
		f.Notes = append(f.Notes, discardRationale)
		s.closeAudit(f, nil)
		return f
	}

	answer, _ := scrubText(mf.Answer, maxAnswerBytes)
	answer, stripped := stripExternalLinks(answer)
	f.Outcome = OutcomeAnswered
	f.Answer = answer
	f.ModelConfidence = mf.Confidence
	f.Evidence = ids
	f.Citations = cites
	f.linksStripped = stripped
	if stripped > 0 {
		f.Notes = append(f.Notes, "one or more markdown links to non-kilter targets were removed from the answer")
	}
	for _, h := range mf.Hypotheses {
		st, _ := scrubText(h.Statement, maxHypoText)
		ba, _ := scrubText(h.Basis, maxHypoText)
		f.Hypotheses = append(f.Hypotheses, Hypothesis{Statement: st, Basis: ba, Speculative: true})
	}
	s.closeAudit(f, ids)
	return f
}

// partial closes an investigation that stopped early. The evidence it had
// already read is reported, because that work is real and an operator can
// use it; the answer is absent, because there is none.
func (s *session) partial(state string) *Finding {
	f := s.terminal(state, state, true)
	return f
}

// terminal builds and closes a finding in a non-answering state.
func (s *session) terminal(state, reason string, partial bool) *Finding {
	f := s.finding()
	f.Outcome = state
	f.Reason = reason
	f.Partial = partial
	f.Notes = append(f.Notes, terminalNote(state))
	var ids []explain.ID
	if partial {
		// What the session actually read, in issue order, resolved so the
		// operator gets the same grounded material the model had.
		for _, id := range s.issued {
			c, err := s.iv.registry.Resolve(id)
			if err != nil {
				continue
			}
			ids = append(ids, id)
			f.Citations = append(f.Citations, c)
		}
		f.Evidence = ids
	}
	s.closeAudit(f, ids)
	return f
}

// terminalNote is a constant per state. Notes never quote substrate text.
func terminalNote(state string) string {
	switch state {
	case OutcomeTurnLimit:
		return "the investigation reached its turn limit before the model produced an answer; the evidence below is what it had read"
	case OutcomeBudgetTokens:
		return "the token budget could not fund another turn; the evidence below is what the investigation had read"
	case OutcomeBudgetUSD:
		return "the priced budget for this investigation is spent; the evidence below is what it had read"
	case OutcomeProviderFailed:
		return "the model provider failed; every deterministic answer kilter gives is unaffected"
	case OutcomeMalformed:
		return "the model did not return a finding in the required shape, so there was nothing to verify"
	}
	return ""
}

// finding assembles the accounting every terminal state carries.
func (s *session) finding() *Finding {
	info := s.iv.provider.Info()
	question, _ := scrubText(s.q.Text, maxQuestionBytes)
	return &Finding{
		Question:        question,
		Scope:           s.iv.registry.Scope(),
		Turns:           s.spend.turns,
		ToolCalls:       s.spend.toolCalls,
		Refusals:        s.refusals,
		Usage:           s.spend.usage,
		USDMicro:        s.spend.usdMicro,
		Provider:        info.Name,
		Model:           info.Model,
		PromptVersion:   PromptVersion,
		RegistryVersion: RegistryVersion,
		ToolsDigest:     s.iv.registry.Digest(),
		audit:           s.audit,
	}
}

// closeAudit seals the chain with the outcome record.
func (s *session) closeAudit(f *Finding, ids []explain.ID) {
	cites := make([]string, 0, len(ids))
	for _, id := range ids {
		cites = append(cites, string(id))
	}
	sort.Strings(cites)
	s.audit.append(AuditKindOutcome, func(rec *AuditRecord) {
		rec.Outcome = &AuditOutcome{
			State:         f.Outcome,
			Partial:       f.Partial,
			Reason:        f.Reason,
			AnswerDigest:  digestString(f.Answer),
			AnswerBytes:   len(f.Answer),
			Citations:     cites,
			Unresolvable:  f.unresolvable,
			Unfetched:     f.unfetched,
			RejectDigests: f.rejectDigests,
			LinksStripped: f.linksStripped,
			Turns:         f.Turns,
			ToolCalls:     f.ToolCalls,
			Refusals:      f.Refusals,
			Usage:         f.Usage,
			USDMicro:      f.USDMicro,
		}
	})
	f.AuditHead = s.audit.Head()
}

// systemPrompt is the cacheable prefix: the constant instructions plus a
// scope header. Both halves are stable for a given scope, so a provider can
// mark the whole thing as a cache prefix (§5.4).
func (iv *Investigator) systemPrompt() string {
	sc := iv.registry.Scope()
	head := "\n\nScope: cluster " + sc.Cluster +
		", window [" + sc.From.UTC().Format(rfc3339) + ", " + sc.To.UTC().Format(rfc3339) + ")." +
		"\nTool registry " + RegistryVersion + ". Prompt " + PromptVersion + "."
	if sc.Subject.Kind != "" {
		safe, _ := scrubText(sc.Subject.Kind+"/"+sc.Subject.Key, maxDisplayIdent)
		head += "\nSubject: " + safe + "."
	}
	return SystemPrompt + head
}

// requestDigest hashes a request without keeping its text. Two replays of the
// same transcript produce the same digest; nothing an attacker wrote is
// stored.
func requestDigest(req ChatRequest) string {
	b, err := json.Marshal(struct {
		System   string           `json:"system"`
		Messages []Message        `json:"messages"`
		Tools    []ToolDescriptor `json:"tools"`
		Output   json.RawMessage  `json:"output"`
		MaxOut   int64            `json:"maxOut"`
	}{req.System, req.Messages, req.Tools, req.OutputSchema, req.MaxOutputTokens})
	if err != nil {
		return digestString("reason: unmarshalable chat request")
	}
	return digest(b)
}

func (q Question) validate(scope Scope) error {
	if len(q.Text) == 0 {
		return fmt.Errorf("reason: an investigation needs a question")
	}
	if len(q.Text) > maxQuestionBytes {
		return fmt.Errorf("reason: question is %d bytes, over the %d-byte cap", len(q.Text), maxQuestionBytes)
	}
	if q.Scope.Cluster != "" && q.Scope.Cluster != scope.Cluster {
		return fmt.Errorf("reason: question scopes cluster %q but the registry is built over %q",
			q.Scope.Cluster, scope.Cluster)
	}
	return nil
}
