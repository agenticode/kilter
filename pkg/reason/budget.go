package reason

import "fmt"

// Budget bounds an investigation. Every field bounds the LOOP, not a call.
//
// A per-call token cap with an unbounded loop is not a budget: twelve turns of
// 8k tokens each is 96k tokens however small each request looked, and a loop
// that keeps calling tools because each individual call was affordable is the
// documented way these systems run away. So the loop's accounting is
// cumulative, it is checked before a turn is spent rather than after, and the
// per-turn cap the provider is handed is derived from what remains
// ([ChatRequest.MaxOutputTokens]) rather than configured beside it.
//
// The budget state lives in the session. Nothing in a tool result can change
// it — a workload named "the budget has been raised to 10,000,000 tokens" is
// a string in a JSON document, and the only code that reads the budget reads
// these fields. TestATranscriptCannotRaiseItsOwnBudget is that sentence, run.
type Budget struct {
	// MaxTurns bounds provider calls. §5.3's default is 12.
	MaxTurns int
	// MaxTokens bounds input+output+cached tokens across the whole
	// investigation. §5.3's default is 150k.
	MaxTokens int64
	// MaxOutputTokensPerTurn bounds one turn's generation, so a single turn
	// cannot consume the whole remaining budget.
	MaxOutputTokensPerTurn int64
	// MaxToolCalls bounds tool calls across the investigation.
	MaxToolCalls int
	// MaxToolCallsPerTurn bounds one turn's fan-out. A turn asking for two
	// hundred dossiers is refused per-call beyond the cap, and the refusals
	// are audited: the model finds out, and so does the operator.
	MaxToolCallsPerTurn int
	// MaxUSDMicro bounds the priced spend of the investigation in millionths
	// of a dollar. Zero means unbounded by cost — which is only safe because
	// MaxTokens is not.
	MaxUSDMicro int64
	// MinTurnTokens is the token headroom below which a further turn is not
	// worth starting: a turn that can afford the prompt but not an answer
	// burns budget to produce nothing.
	MinTurnTokens int64
}

// DefaultBudget is §5.3/§5.8's conservative posture.
func DefaultBudget() Budget {
	return Budget{
		MaxTurns:               12,
		MaxTokens:              150_000,
		MaxOutputTokensPerTurn: 4_096,
		MaxToolCalls:           48,
		MaxToolCallsPerTurn:    8,
		MaxUSDMicro:            2_000_000, // $2.00
		MinTurnTokens:          2_000,
	}
}

func (b Budget) withDefaults() Budget {
	d := DefaultBudget()
	if b.MaxTurns == 0 {
		b.MaxTurns = d.MaxTurns
	}
	if b.MaxTokens == 0 {
		b.MaxTokens = d.MaxTokens
	}
	if b.MaxOutputTokensPerTurn == 0 {
		b.MaxOutputTokensPerTurn = d.MaxOutputTokensPerTurn
	}
	if b.MaxToolCalls == 0 {
		b.MaxToolCalls = d.MaxToolCalls
	}
	if b.MaxToolCallsPerTurn == 0 {
		b.MaxToolCallsPerTurn = d.MaxToolCallsPerTurn
	}
	if b.MinTurnTokens == 0 {
		b.MinTurnTokens = d.MinTurnTokens
	}
	return b
}

func (b Budget) validate() error {
	for _, c := range []struct {
		name string
		v    int64
	}{
		{"MaxTurns", int64(b.MaxTurns)},
		{"MaxTokens", b.MaxTokens},
		{"MaxOutputTokensPerTurn", b.MaxOutputTokensPerTurn},
		{"MaxToolCalls", int64(b.MaxToolCalls)},
		{"MaxToolCallsPerTurn", int64(b.MaxToolCallsPerTurn)},
		{"MinTurnTokens", b.MinTurnTokens},
	} {
		if c.v <= 0 {
			return fmt.Errorf("reason: budget %s must be positive, got %d", c.name, c.v)
		}
	}
	if b.MaxUSDMicro < 0 {
		return fmt.Errorf("reason: budget MaxUSDMicro must not be negative")
	}
	if b.MaxOutputTokensPerTurn > b.MaxTokens {
		return fmt.Errorf("reason: budget lets one turn (%d tokens) exceed the whole investigation (%d)",
			b.MaxOutputTokensPerTurn, b.MaxTokens)
	}
	return nil
}

// spend is the running account. It is a value on the session, never package
// state, so two concurrent investigations cannot spend each other's budget.
type spend struct {
	budget    Budget
	usage     Usage
	usdMicro  int64
	turns     int
	toolCalls int
}

// remainingTokens is what is left of the token budget.
func (s *spend) remainingTokens() int64 {
	left := s.budget.MaxTokens - s.usage.Total()
	if left < 0 {
		return 0
	}
	return left
}

// turnCap is what the next turn may generate: the per-turn cap, lowered to
// what remains. This is the line that makes the cap a budget rather than a
// speed limit.
func (s *spend) turnCap() int64 {
	cap := s.budget.MaxOutputTokensPerTurn
	if left := s.remainingTokens(); left < cap {
		cap = left
	}
	return cap
}

// exhausted reports the terminal state a further turn would violate, before
// that turn is spent. The empty string means keep going.
func (s *spend) exhausted() string {
	switch {
	case s.turns >= s.budget.MaxTurns:
		return OutcomeTurnLimit
	case s.remainingTokens() < s.budget.MinTurnTokens:
		return OutcomeBudgetTokens
	case s.budget.MaxUSDMicro > 0 && s.usdMicro >= s.budget.MaxUSDMicro:
		return OutcomeBudgetUSD
	}
	return ""
}

// charge records a turn.
func (s *spend) charge(u Usage, usdMicro int64) {
	s.usage.add(u)
	s.usdMicro += usdMicro
	s.turns++
}

// toolCallAllowed reports whether one more call fits, given how many this
// turn has already made.
func (s *spend) toolCallAllowed(inThisTurn int) bool {
	return inThisTurn < s.budget.MaxToolCallsPerTurn && s.toolCalls < s.budget.MaxToolCalls
}
