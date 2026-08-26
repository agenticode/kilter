package reason

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/explain"
)

// Reader is the substrate as a tool is allowed to see it: the four query
// methods of [evidence.Store], and not its fifth.
//
// evidence.Store carries Append, and evidence.Sink carries three more
// writers. A tool handed a Store could append an event — not a cluster
// mutation, but a write into the record that the same investigation then
// cites, which is the shape of "the model manufactured its own evidence".
type Reader interface {
	Events(s evidence.SubjectRef, from, to time.Time, kinds ...string) ([]evidence.EvidenceEvent, error)
	Digests(s evidence.SubjectRef, from, to time.Time, tier int) ([]evidence.Digest, error)
	Timeline(cluster string, from, to time.Time) ([]evidence.TimelinePoint, error)
	Decisions(s evidence.SubjectRef, from, to time.Time) ([]evidence.DecisionRecord, error)
}

// roStore is the capability boundary made concrete. It forwards the four
// reads and refuses the one write.
//
// It satisfies [evidence.Store] on purpose, because two things this package
// needs — [evidence.BuildDossier] and [explain.BuildExplain] — take a Store
// and there is no narrower interface to give them. Rather than hand either of
// them a live writer, the write method is present and unconditionally
// hostile: TestTheNarrowedStoreRefusesToWrite pins that at runtime, because
// "it is obviously never called" is exactly the assumption that ages badly.
// The wrapped store is an unexported field, so no assertion recovers it.
type roStore struct {
	st evidence.Store
}

func (r roStore) Events(s evidence.SubjectRef, from, to time.Time, kinds ...string) ([]evidence.EvidenceEvent, error) {
	return r.st.Events(s, from, to, kinds...)
}

func (r roStore) Digests(s evidence.SubjectRef, from, to time.Time, tier int) ([]evidence.Digest, error) {
	return r.st.Digests(s, from, to, tier)
}

func (r roStore) Timeline(cluster string, from, to time.Time) ([]evidence.TimelinePoint, error) {
	return r.st.Timeline(cluster, from, to)
}

func (r roStore) Decisions(s evidence.SubjectRef, from, to time.Time) ([]evidence.DecisionRecord, error) {
	return r.st.Decisions(s, from, to)
}

// Append refuses. See the type comment.
func (r roStore) Append(evidence.EvidenceEvent) error { return ErrReadOnly }

// ErrReadOnly is what the narrowed substrate answers to any attempt to write
// through it.
const ErrReadOnly internalError = "reason: the reasoning plane's view of the substrate is read-only; nothing reachable from a tool may write"

// Scope binds an investigation to a cluster and a window. Both are arguments,
// never clock reads: an investigation whose window drifts is not replayable,
// and replayability is what makes "show me exactly what the AI saw" a query
// rather than a hope (§5.5).
type Scope struct {
	Cluster string
	// Subject optionally narrows the investigation to one subject. It is
	// carried into the audit record and into the seed context; the registry
	// enforces the cluster rather than the subject, because a question about
	// one workload is routinely answered by looking at its siblings.
	Subject  evidence.SubjectRef
	From, To time.Time
}

func (s Scope) validate() error {
	if s.Cluster == "" {
		return fmt.Errorf("reason: scope needs a cluster")
	}
	if s.From.IsZero() || s.To.IsZero() {
		return fmt.Errorf("reason: scope needs a bounded window (the window is an argument, never a clock)")
	}
	if !s.To.After(s.From) {
		return fmt.Errorf("reason: scope window [%v, %v) is empty or inverted", s.From, s.To)
	}
	return nil
}

// explainFn is the capability a tool is given instead of a store when it
// needs a deterministic explain payload. The registry supplies it, already
// bound to the narrowed substrate and already verifying its own citations.
type explainFn func(s evidence.SubjectRef, from, to time.Time) (*explain.Explanation, error)

// Input is everything a tool body receives. There is no field on it through
// which anything can be written.
type Input struct {
	// Args are validated and clamped. See [Args] for why a tool cannot be
	// handed anything else.
	Args Args
	// Read is the narrowed substrate.
	Read Reader
	// Scope is the investigation's cluster and window.
	Scope Scope
	// Subjects is the enumerable universe in [evidence.SubjectRef] order,
	// snapshotted once at registry construction. Tools read it; nothing can
	// grow it.
	Subjects []evidence.SubjectRef

	// ro is the same value as Read, typed so the two substrate helpers that
	// insist on an evidence.Store can be called without a type assertion
	// that would look, to a later reader, like the boundary being crossed.
	ro roStore
	// explain builds a verified explain payload.
	explain explainFn
}

// Result is what a tool returns: a JSON document for the model, the evidence
// IDs that document showed it, and any bound the tool itself applied.
//
// The citation list is the tool's declaration of what the session has now
// read. The loop will not let a finding cite an ID that no tool declared, so
// a tool that forgets to declare one has made that evidence uncitable rather
// than making a fabrication possible — the safe direction for that mistake.
//
// Result has no exported constructor: a Result can only come from a tool body
// in this package, so no caller can synthesize "a tool said this".
type Result struct {
	body   json.RawMessage
	cites  []explain.ID
	clamps []Clamp
}

// result builds a Result from any JSON-marshalable value.
func result(v any, cites []explain.ID, clamps []Clamp) (Result, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return Result{}, err
	}
	return Result{
		body:   body,
		cites:  append([]explain.ID(nil), cites...),
		clamps: append([]Clamp(nil), clamps...),
	}, nil
}

// ToolFunc is a tool body.
type ToolFunc func(ctx context.Context, in Input) (Result, error)

// Tool is one entry in the registry.
//
// # Read-only by construction
//
// The shape is pkg/rds's ApprovedStep, for the same reason and with the same
// mechanics. An actuating tool must be UNREPRESENTABLE, not merely
// unregistered:
//
//   - Every field is unexported, so `reason.Tool{...}` does not compile in
//     any other package.
//   - The constructor, [readOnlyTool], is unexported too. There is therefore
//     no way at all for pkg/api, cmd/, or a future pkg/mcp to put a tool into
//     this registry: the tool surface is enumerated in this package, which is
//     what §5.2 means by "explicitly enumerated". A tool needing data from
//     above this package takes a narrow read interface the caller implements
//     (the [Reader] pattern), not a registration hook.
//   - `readOnly` is set in exactly one place. Unit 8's `propose_*` tools are
//     not read-only, so admitting them will be a visible diff to this file
//     and to [Registry.Register] — not a call somewhere else that this
//     package cannot see.
//   - The zero Tool is representable — Go always permits a zero value — and
//     is inert: [Registry.Register] refuses it, because its stamp is false.
//     TestTheZeroToolCannotBeRegistered pins that.
//   - A tool body's only handle on the world is [Input], which carries a
//     [Reader]. It cannot reach evidence.Sink, pkg/ec2, pkg/rds, a Kubernetes
//     client or a plan.
//
// What would still defeat it is a closure: a body compiled *inside this
// package* could capture a writer from its enclosing scope, and no type
// prevents that. That is why the second half of the proof is
// TestNoActuatorSymbolIsReachableFromThisPackage, which derives the forbidden
// identifier set from pkg/ec2's and pkg/rds's own actuate*.go sources and
// fails if any file here names one — the technique of
// cmd/BRAINWIRE-FINDINGS.md §6, pointed at this package.
type Tool struct {
	name        string
	description string
	schema      Schema
	timeout     time.Duration
	run         ToolFunc
	readOnly    bool
}

// maxToolTimeout bounds a tool's time box. §5.3 puts the default at 2s and
// queues what cannot meet it; nothing in this unit sits longer than a human
// waits for one turn.
const maxToolTimeout = 30 * time.Second

// DefaultToolTimeout is §5.3's time box.
const DefaultToolTimeout = 2 * time.Second

// readOnlyTool builds a tool. It is the only constructor, and the only place
// a Tool is stamped read-only.
func readOnlyTool(name, description string, schema Schema, timeout time.Duration, run ToolFunc) (Tool, error) {
	if name == "" {
		return Tool{}, fmt.Errorf("reason: a tool needs a name")
	}
	if description == "" {
		return Tool{}, fmt.Errorf("reason: tool %q needs a description; it is the model's only documentation", name)
	}
	if run == nil {
		return Tool{}, fmt.Errorf("reason: tool %q has no body", name)
	}
	if timeout <= 0 || timeout > maxToolTimeout {
		return Tool{}, fmt.Errorf("reason: tool %q timeout %v outside (0, %v]", name, timeout, maxToolTimeout)
	}
	return Tool{
		name:        name,
		description: description,
		schema:      schema,
		timeout:     timeout,
		run:         run,
		readOnly:    true,
	}, nil
}

// Name, Description, Schema and Timeout expose the tool's contract without
// exposing its body.
func (t Tool) Name() string           { return t.name }
func (t Tool) Description() string    { return t.description }
func (t Tool) Schema() Schema         { return t.schema }
func (t Tool) Timeout() time.Duration { return t.timeout }

// ReadOnly reports the stamp [readOnlyTool] applies. It is false only for a
// zero Tool, which is the one Tool value another package can produce and the
// one [Registry.Register] refuses.
func (t Tool) ReadOnly() bool { return t.readOnly }

// ToolDescriptor is the wire form of a tool: what a provider sends to a model
// and what §6's MCP `tools/list` will serve, unchanged.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"readOnly"`
}
