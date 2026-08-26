package reason

import (
	"context"
	"encoding/json"
	"math"
)

// Provider is the model seam, and it is an interface for the same reason
// pkg/forecast's RemoteForecaster is a struct behind one: the engine must
// keep working when the thing behind the seam is absent (§7.1, and the
// fallback at pkg/api/capacity.go).
//
// This unit ships NO implementation. Not an Anthropic client, not an
// openai-compat client, not an HTTP call to a model endpoint, not behind a
// build tag. `go.mod` and `go.sum` are byte-identical to what they were
// before this package existed, and TestPackageDepsAreStdlibAndIntraRepo keeps
// them that way. A provider is an organ: it links against this interface from
// its own package, and the air gap is preserved by the fact that a kilter
// binary built without one still does everything except narrate.
//
// Implementations map ChatRequest to their wire format. They are expected to
// stream internally and return the completed turn; nothing above this seam
// consumes partial output, because §7.3 forbids streaming partial findings
// into automation.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Info() ProviderInfo
}

// Role is a message's author.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in the transcript.
//
// A tool result is a Message with RoleTool whose Content is the registry's
// envelope — scrubbed, canonical, and labelled untrusted. Text and Content are
// never both set: prose and data do not share a field, so no formatting step
// can concatenate a cluster string into an instruction.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text,omitempty"`

	// ToolCallID and Tool identify which call a RoleTool message answers.
	ToolCallID string `json:"toolCallId,omitempty"`
	Tool       string `json:"tool,omitempty"`
	// Content is the tool result envelope for a RoleTool message, or the
	// deterministic seed context for the opening RoleUser message. Always
	// valid, canonical JSON.
	Content json.RawMessage `json:"content,omitempty"`

	// Calls are the tool calls an assistant turn made. A provider needs them
	// to reconstruct its wire format's tool-use blocks, and they carry the
	// model's own raw arguments.
	//
	// This is not the echo §5.7 forbids. The rule is that a tool RESULT's
	// bytes never become a subsequent call's arguments: a result reaches the
	// model only inside Content, and the only path from a model's output to
	// a tool is [Schema.Validate]. Replaying the assistant's own emitted
	// call block is what the wire format requires and introduces no new
	// source of bytes.
	Calls []ToolCall `json:"calls,omitempty"`
}

// ToolCall is a model's request to run a tool. Args is whatever the model
// emitted: unvalidated, unbounded, and the single most attacker-influenceable
// value in this package. Nothing reads it except [Schema.Validate].
type ToolCall struct {
	ID   string          `json:"id"`
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// ChatRequest is one model turn.
type ChatRequest struct {
	// System is the cacheable prefix: [SystemPrompt] plus the scope header.
	System string
	// Messages is the transcript so far.
	Messages []Message
	// Tools is the registry's surface, in name order.
	Tools []ToolDescriptor
	// OutputSchema is the strict schema a final answer must satisfy. A
	// provider maps it to structured output / a forced tool.
	OutputSchema json.RawMessage
	// MaxOutputTokens is what remains of the loop's budget for this turn. It
	// is a number the loop computes, never one the provider chooses: a
	// per-call cap that the caller cannot lower is not a budget.
	MaxOutputTokens int64
}

// ChatResponse is one model turn's result.
type ChatResponse struct {
	// Text is the assistant's prose for this turn, if any.
	Text string
	// ToolCalls are the tools the model wants run before the next turn.
	ToolCalls []ToolCall
	// Output is the structured finding, set only on the final turn. A
	// response with Output set ends the loop.
	Output json.RawMessage
	// Usage is what the turn cost. A provider that cannot report usage must
	// report an estimate rather than zero: a zero-usage provider would make
	// the loop's budget unenforceable, which is the one failure mode a cost
	// optimizer cannot ship.
	Usage Usage
	// StopReason is the provider's own word for why the turn ended,
	// recorded verbatim in the audit trail.
	StopReason string
}

// Usage is one turn's token accounting.
type Usage struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens,omitempty"`
}

// Total is what the budget counts.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens + u.CachedInputTokens }

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CachedInputTokens += o.CachedInputTokens
}

// ProviderInfo pins what answered and what it charges (§5.5, §5.8). Prices
// are per million tokens, which is how every provider quotes them.
type ProviderInfo struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	// PromptVersion lets a provider that owns its own prompt say so; the
	// loop records this package's [PromptVersion] as well.
	PromptVersion string `json:"promptVersion,omitempty"`

	USDPerMInput       float64 `json:"usdPerMInput"`
	USDPerMOutput      float64 `json:"usdPerMOutput"`
	USDPerMCachedInput float64 `json:"usdPerMCachedInput"`
}

// USDMicro prices a usage record in millionths of a dollar.
//
// Micro-dollars, not float dollars, for pkg/explain's reason: a cost that is
// summed across turns and then compared byte-for-byte between two replays
// cannot be a float. The conversion is exact by construction — dollars =
// tokens/1e6 × USDPerM, so micro-dollars = tokens × USDPerM — and rounds once,
// at the end of each term.
func (i ProviderInfo) USDMicro(u Usage) int64 {
	return term(u.InputTokens, i.USDPerMInput) +
		term(u.OutputTokens, i.USDPerMOutput) +
		term(u.CachedInputTokens, i.USDPerMCachedInput)
}

func term(tokens int64, usdPerM float64) int64 {
	if tokens <= 0 || usdPerM <= 0 || math.IsNaN(usdPerM) || math.IsInf(usdPerM, 0) {
		return 0
	}
	v := math.Round(float64(tokens) * usdPerM)
	if v > math.MaxInt64/2 {
		return math.MaxInt64 / 2
	}
	return int64(v)
}
