package reason

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// The audit trail (§5.6). One investigation is one chain of records: what was
// asked, what came back, and what was refused. The third of those is the one
// that is usually missing, and its absence is not neutral — a refusal that
// leaves no trace is indistinguishable from a question that was never asked,
// which is exactly the ambiguity an operator is trying to resolve when they
// open this.
//
// # Byte-identical for a replayed transcript
//
// Two runs of the same scripted transcript against the same substrate produce
// the same bytes. That holds because:
//
//   - Time enters only through [Clock]. There is no time.Now here.
//   - Every list is emitted in a documented order and no record contains a Go
//     map. Args are recorded as sorted key/value pairs; citations are sorted.
//   - Durations are not recorded. A tool's elapsed time is real and varies;
//     recording it would put a stopwatch reading in a hash chain.
//   - The chain is over canonical JSON of each record with its own Hash field
//     empty, so the hash is a function of the content and the previous hash
//     and nothing else.
//
// # What is stored, given that everything is hostile
//
// A tool call's arguments are the model's bytes, which may be the attacker's
// bytes. They are recorded twice, deliberately: [AuditTool.ArgsDigest] is the
// exact SHA-256 of what arrived, and [AuditTool.Args] is the same document
// scrubbed and capped for display. The digest keeps the record forensically
// exact; the scrubbed copy is what an operator's terminal renders. Nothing in
// this file is fed back to a model — the anti-echo rule governs the
// transcript, and the audit trail is downstream of it.

// AuditRecord is one entry. Exactly one of the payload pointers is set; the
// Kind says which.
type AuditRecord struct {
	Seq  int       `json:"seq"`
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`

	Question *AuditQuestion `json:"question,omitempty"`
	Turn     *AuditTurn     `json:"turn,omitempty"`
	Tool     *AuditTool     `json:"tool,omitempty"`
	Outcome  *AuditOutcome  `json:"outcome,omitempty"`

	// Prev is the previous record's Hash; "" for the first.
	Prev string `json:"prev"`
	// Hash is this record's own hash, computed over the record with Hash
	// empty. Tampering with any earlier record invalidates every later one.
	Hash string `json:"hash"`
}

// Record kinds.
const (
	AuditKindQuestion = "question"
	AuditKindTurn     = "turn"
	AuditKindTool     = "tool"
	AuditKindOutcome  = "outcome"
)

// AuditQuestion opens the chain: what was asked, by whom, of what, with which
// versions pinned (§5.5, INV-5).
type AuditQuestion struct {
	Question  string `json:"question"` // scrubbed and capped
	Initiator string `json:"initiator,omitempty"`
	Cluster   string `json:"cluster"`
	Subject   string `json:"subject,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`

	Provider        string `json:"provider"`
	Model           string `json:"model"`
	PromptVersion   string `json:"promptVersion"`
	RegistryVersion string `json:"registryVersion"`
	ToolsDigest     string `json:"toolsDigest"`
	SystemDigest    string `json:"systemDigest"`
	SeedDigest      string `json:"seedDigest"`
	Budget          Budget `json:"budget"`
}

// AuditTurn records one provider round trip.
type AuditTurn struct {
	Turn            int    `json:"turn"`
	RequestDigest   string `json:"requestDigest"`
	Messages        int    `json:"messages"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`

	TextDigest   string   `json:"textDigest,omitempty"`
	OutputDigest string   `json:"outputDigest,omitempty"`
	ToolCalls    []string `json:"toolCalls,omitempty"` // tool names, call order
	StopReason   string   `json:"stopReason,omitempty"`
	Error        string   `json:"error,omitempty"` // a provider failure, by class

	Usage    Usage `json:"usage"`
	USDMicro int64 `json:"usdMicro"`
}

// AuditTool records one tool call: asked, returned, or refused.
type AuditTool struct {
	Turn       int    `json:"turn"`
	Tool       string `json:"tool"`
	CallID     string `json:"callId,omitempty"`
	ArgsDigest string `json:"argsDigest"`
	// Args is the argument document, scrubbed and capped, for a human.
	Args json.RawMessage `json:"args,omitempty"`

	Clamps  []Clamp  `json:"clamps,omitempty"`
	Refusal *Refusal `json:"refusal,omitempty"`

	ResultDigest string   `json:"resultDigest,omitempty"`
	ResultBytes  int      `json:"resultBytes,omitempty"`
	Scrubbed     int      `json:"scrubbedStrings,omitempty"`
	Citations    []string `json:"citations,omitempty"` // handle -> id, sorted
}

// AuditOutcome closes the chain.
type AuditOutcome struct {
	State   string `json:"state"`
	Partial bool   `json:"partial"`
	// Reason is a refusal code or a terminal-state code; never free text.
	Reason       string   `json:"reason,omitempty"`
	AnswerDigest string   `json:"answerDigest,omitempty"`
	AnswerBytes  int      `json:"answerBytes,omitempty"`
	Citations    []string `json:"citations,omitempty"`
	// Discarded counts citations the publish gate rejected, by cause. The
	// ids themselves are the model's bytes and are recorded as digests.
	Unresolvable  int      `json:"unresolvableCitations,omitempty"`
	Unfetched     int      `json:"unfetchedCitations,omitempty"`
	RejectDigests []string `json:"rejectedCitationDigests,omitempty"`
	LinksStripped int      `json:"linksStripped,omitempty"`

	Turns     int   `json:"turns"`
	ToolCalls int   `json:"toolCalls"`
	Refusals  int   `json:"refusals"`
	Usage     Usage `json:"usage"`
	USDMicro  int64 `json:"usdMicro"`
}

// Audit is one investigation's chain.
type Audit struct {
	clock   Clock
	records []AuditRecord
}

func newAudit(c Clock) *Audit { return &Audit{clock: c} }

// append seals a record into the chain.
func (a *Audit) append(kind string, fill func(*AuditRecord)) {
	rec := AuditRecord{Seq: len(a.records), Kind: kind, At: a.clock.now()}
	fill(&rec)
	if n := len(a.records); n > 0 {
		rec.Prev = a.records[n-1].Hash
	}
	rec.Hash = hashRecord(rec)
	a.records = append(a.records, rec)
}

// hashRecord hashes a record with its own Hash field empty.
func hashRecord(rec AuditRecord) string {
	rec.Hash = ""
	b, err := json.Marshal(rec)
	if err != nil {
		// Every field is this package's own and marshalable; a failure here
		// would be a bug, and a hash of the error text would hide it.
		return digest([]byte("reason: unmarshalable audit record"))
	}
	return digest(b)
}

// Records returns the chain in order.
func (a *Audit) Records() []AuditRecord { return append([]AuditRecord(nil), a.records...) }

// Encode renders the chain as canonical JSON — the bytes a determinism test
// compares and a store persists.
func (a *Audit) Encode() ([]byte, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(a.records)
}

// Verify re-derives every hash and checks the chain links up. It is what
// makes "tampering is evident" a check rather than a claim.
func (a *Audit) Verify() error {
	prev := ""
	for i, rec := range a.records {
		if rec.Seq != i {
			return fmt.Errorf("reason: audit record %d carries seq %d", i, rec.Seq)
		}
		if rec.Prev != prev {
			return fmt.Errorf("reason: audit record %d does not link to its predecessor", i)
		}
		if want := hashRecord(rec); want != rec.Hash {
			return fmt.Errorf("reason: audit record %d has been altered", i)
		}
		prev = rec.Hash
	}
	return nil
}

// Head is the last record's hash — the value a caller stores to chain one
// investigation's audit to the ledger that holds it.
func (a *Audit) Head() string {
	if a == nil || len(a.records) == 0 {
		return ""
	}
	return a.records[len(a.records)-1].Hash
}

// digest is the one hash in this package. SHA-256, hex, full width: an audit
// chain that truncates its hashes to save bytes is an audit chain with a
// cheaper collision.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// digestString is digest over a string.
func digestString(s string) string { return digest([]byte(s)) }
