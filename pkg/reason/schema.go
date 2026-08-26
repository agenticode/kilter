package reason

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxArgsBytes bounds one tool call's argument object. It is small on
// purpose: every argument this package declares is a name, a bound, or an
// instant, and none of those is kilobytes long. The cap is checked before any
// parsing, so a 10 KiB workload name echoed back as an argument costs one
// length comparison rather than a decode.
const maxArgsBytes = 8 << 10

// paramKind is the shape of one argument. The kind fixes the disposition —
// see [Clamp] for why that is not a per-call decision.
type paramKind uint8

const (
	kindIdent paramKind = iota + 1
	kindEnum
	kindQuantity
	kindInstant
	kindFlag
	kindIdentList
)

// Param is one validated argument. Its fields are unexported and its
// constructors are the only way to make one, so a schema cannot declare a
// parameter whose disposition was chosen by whoever wrote the tool.
type Param struct {
	name, desc string
	kind       paramKind
	required   bool
	maxLen     int
	enum       []string
	min        int64
	max        int64
	def        int64
	maxItems   int
}

// Name is the wire name of the parameter.
func (p Param) Name() string { return p.name }

// Clamps reports whether an out-of-range value is lowered (a quantity) or
// refused (an identity).
func (p Param) Clamps() bool { return p.kind == kindQuantity }

// Ident declares a required identity-bearing string: a subject key, a cluster
// id, a fingerprint. Over-long or unclean values are refused, never trimmed.
func Ident(name, desc string) Param {
	return Param{name: name, desc: desc, kind: kindIdent, required: true, maxLen: maxDisplayIdent}
}

// OptIdent is [Ident] without the requirement.
func OptIdent(name, desc string) Param {
	p := Ident(name, desc)
	p.required = false
	return p
}

// Enum declares a string drawn from a closed set. The set is compared
// exactly: there is no normalization step in which a near-miss becomes a hit.
func Enum(name, desc string, required bool, allowed ...string) Param {
	vals := append([]string(nil), allowed...)
	sort.Strings(vals)
	return Param{name: name, desc: desc, kind: kindEnum, required: required, enum: vals}
}

// Quantity declares a bounded integer — the one shape that clamps. def is
// used when the argument is absent; min and max bound everything else.
func Quantity(name, desc string, min, max, def int64) Param {
	return Param{name: name, desc: desc, kind: kindQuantity, min: min, max: max, def: def}
}

// Instant declares an optional RFC3339 timestamp. Windows are arguments in
// this package exactly as they are in pkg/explain: an answer whose window
// drifts with wall-clock time is not replayable.
func Instant(name, desc string) Param {
	return Param{name: name, desc: desc, kind: kindInstant}
}

// Flag declares an optional boolean, absent meaning false.
func Flag(name, desc string) Param {
	return Param{name: name, desc: desc, kind: kindFlag}
}

// IdentList declares a bounded list of identity-bearing strings — event
// kinds, subject keys. Over-long lists are refused: shortening one silently
// answers a narrower question than the operator's.
func IdentList(name, desc string, maxItems int) Param {
	return Param{name: name, desc: desc, kind: kindIdentList, maxItems: maxItems, maxLen: maxDisplayIdent}
}

// Schema is a tool's whole argument surface. Parameters are held in name
// order so the emitted JSON Schema — which is part of the cacheable prompt
// prefix (§5.4) — is byte-identical for the same set of parameters however
// they were declared.
type Schema struct {
	params []Param
}

// NewSchema orders and checks a parameter set.
func NewSchema(params ...Param) (Schema, error) {
	out := append([]Param(nil), params...)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	for i, p := range out {
		if p.kind == 0 {
			return Schema{}, fmt.Errorf("reason: parameter %q was not built by a Param constructor", p.name)
		}
		if p.name == "" || strings.ContainsAny(p.name, " \t\"\\") {
			return Schema{}, fmt.Errorf("reason: parameter name %q is not usable", p.name)
		}
		if i > 0 && out[i-1].name == p.name {
			return Schema{}, fmt.Errorf("reason: parameter %q declared twice", p.name)
		}
		if p.kind == kindQuantity && (p.min > p.max || p.def < p.min || p.def > p.max) {
			return Schema{}, fmt.Errorf("reason: parameter %q has bounds [%d,%d] and default %d",
				p.name, p.min, p.max, p.def)
		}
		if p.kind == kindEnum && len(p.enum) == 0 {
			return Schema{}, fmt.Errorf("reason: enum parameter %q enumerates nothing", p.name)
		}
		if p.kind == kindIdentList && p.maxItems <= 0 {
			return Schema{}, fmt.Errorf("reason: list parameter %q admits no items", p.name)
		}
	}
	return Schema{params: out}, nil
}

// Params returns the parameters in name order.
func (s Schema) Params() []Param { return append([]Param(nil), s.params...) }

// JSON renders the strict JSON Schema a provider sends to a model:
// `additionalProperties: false`, every bound expressed, nothing optional left
// to inference. It is assembled by hand rather than by marshalling a map so
// that key order is this function's decision and not encoding/json's.
func (s Schema) JSON() json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"type":"object","additionalProperties":false`)
	var required []string
	for _, p := range s.params {
		if p.required {
			required = append(required, p.name)
		}
	}
	if len(required) > 0 {
		b.WriteString(`,"required":[`)
		for i, r := range required {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(&b, r)
		}
		b.WriteByte(']')
	}
	b.WriteString(`,"properties":{`)
	for i, p := range s.params {
		if i > 0 {
			b.WriteByte(',')
		}
		writeJSONString(&b, p.name)
		b.WriteByte(':')
		p.writeJSON(&b)
	}
	b.WriteString("}}")
	return json.RawMessage(b.Bytes())
}

func (p Param) writeJSON(b *bytes.Buffer) {
	switch p.kind {
	case kindIdent:
		b.WriteString(`{"type":"string","maxLength":`)
		b.WriteString(strconv.Itoa(p.maxLen))
	case kindEnum:
		b.WriteString(`{"type":"string","enum":[`)
		for i, e := range p.enum {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, e)
		}
		b.WriteByte(']')
	case kindQuantity:
		b.WriteString(`{"type":"integer","minimum":`)
		b.WriteString(strconv.FormatInt(p.min, 10))
		b.WriteString(`,"maximum":`)
		b.WriteString(strconv.FormatInt(p.max, 10))
		b.WriteString(`,"default":`)
		b.WriteString(strconv.FormatInt(p.def, 10))
	case kindInstant:
		b.WriteString(`{"type":"string","format":"date-time"`)
	case kindFlag:
		b.WriteString(`{"type":"boolean"`)
	case kindIdentList:
		b.WriteString(`{"type":"array","maxItems":`)
		b.WriteString(strconv.Itoa(p.maxItems))
		b.WriteString(`,"items":{"type":"string","maxLength":`)
		b.WriteString(strconv.Itoa(p.maxLen))
		b.WriteString(`}`)
	}
	b.WriteString(`,"description":`)
	writeJSONString(b, p.desc)
	b.WriteByte('}')
}

// writeJSONString emits a JSON string. The values are ours — parameter names,
// descriptions, enumerations — so this is about determinism, not safety:
// encoding/json's HTML escaping is stable, and going through it keeps the
// emitted schema identical to what a decoder would round-trip.
func writeJSONString(b *bytes.Buffer, s string) {
	enc, err := json.Marshal(s)
	if err != nil { // impossible for a Go string; a panic here would be a lie
		b.WriteString(`""`)
		return
	}
	b.Write(enc)
}

// Args is a validated argument set. It has no exported constructor and its
// storage is unexported, so the only way to obtain one is [Schema.Validate].
//
// That is the structural half of "tool arguments are never echoed back into a
// subsequent tool call unvalidated": there is no conversion, however careless,
// from a tool *result* to an Args. A value reaches a tool body only by having
// been decoded from a model's arguments and checked against a schema this
// package declared.
type Args struct {
	vals map[string]any
}

// Str returns an ident or enum argument, or "" if absent.
func (a Args) Str(name string) string {
	s, _ := a.vals[name].(string)
	return s
}

// Int returns a quantity argument, already clamped into range.
func (a Args) Int(name string) int64 {
	n, _ := a.vals[name].(int64)
	return n
}

// Time returns an instant argument in UTC, or the zero time if absent.
func (a Args) Time(name string) time.Time {
	t, _ := a.vals[name].(time.Time)
	return t
}

// Bool returns a flag argument.
func (a Args) Bool(name string) bool {
	b, _ := a.vals[name].(bool)
	return b
}

// List returns a list argument in the order given.
func (a Args) List(name string) []string {
	l, _ := a.vals[name].([]string)
	return append([]string(nil), l...)
}

// Has reports whether the argument was supplied (as opposed to defaulted).
func (a Args) Has(name string) bool {
	_, ok := a.vals[name]
	return ok
}

// Validate checks a model's argument object against the schema. It returns
// the typed arguments, every clamp it applied, and — if the call cannot be
// served as asked — a refusal that names the offending field and never quotes
// its value.
//
// Order is deliberate: the byte cap first (so an enormous object is rejected
// without being parsed), then the object shape, then unknown properties, then
// per-parameter checks in name order. A call that violates several rules
// always reports the same one.
func (s Schema) Validate(raw json.RawMessage) (Args, []Clamp, *Refusal) {
	if len(raw) > maxArgsBytes {
		return Args{}, nil, refuseAt(CodeArgsTooLarge, "", maxArgsBytes)
	}
	fields, ref := decodeArgObject(raw)
	if ref != nil {
		return Args{}, nil, ref
	}
	declared := make(map[string]bool, len(s.params))
	for _, p := range s.params {
		declared[p.name] = true
	}
	// Unknown properties are refused, not dropped. A dropped argument turns
	// "search namespace=payments" into "search everything" — a broader
	// answer to a narrower question, which is the failure nobody notices.
	unknown := make([]string, 0, 4)
	for name := range fields {
		if !declared[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		// The name is NOT repeated back. An undeclared argument's name was
		// chosen by whoever wrote the call, and a refusal that quotes it is a
		// channel from the model's output straight back into the model's
		// input — which is the echo §5.7 forbids, arriving through the one
		// component whose job was to stop it.
		//
		// The cost is a less specific message. It is small: the model holds
		// the schema, and additionalProperties:false already enumerates every
		// name that is allowed. The audit trail keeps the scrubbed name for
		// the operator, who is not the attacker's target here.
		return Args{}, nil, refuse(CodeUnknownArgument, "")
	}

	vals := make(map[string]any, len(s.params))
	var clamps []Clamp
	for _, p := range s.params {
		rawVal, present := fields[p.name]
		if present && isJSONNull(rawVal) {
			present = false // an explicit null is an absent argument
		}
		if !present {
			if p.required {
				return Args{}, nil, refuse(CodeMissingArgument, p.name)
			}
			if p.kind == kindQuantity {
				vals[p.name] = p.def
			}
			continue
		}
		v, c, ref := p.parse(rawVal)
		if ref != nil {
			return Args{}, nil, ref
		}
		if c != nil {
			clamps = append(clamps, *c)
		}
		vals[p.name] = v
	}
	return Args{vals: vals}, clamps, nil
}

// parse validates one argument value.
func (p Param) parse(raw json.RawMessage) (any, *Clamp, *Refusal) {
	switch p.kind {
	case kindIdent:
		s, ref := p.parseIdent(raw)
		if ref != nil {
			return nil, nil, ref
		}
		if s == "" && p.required {
			return nil, nil, refuse(CodeMissingArgument, p.name)
		}
		return s, nil, nil

	case kindEnum:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, refuse(CodeWrongType, p.name)
		}
		for _, e := range p.enum {
			if e == s {
				return s, nil, nil
			}
		}
		return nil, nil, refuse(CodeNotAllowed, p.name)

	case kindQuantity:
		// A quoted number decodes into json.Number without complaint, so the
		// literal is checked first: "20" is a string, and a schema that
		// silently accepts a string where it declared an integer has stopped
		// being a description of what the model may send.
		if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] == '"' {
			return nil, nil, refuse(CodeWrongType, p.name)
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, nil, refuse(CodeWrongType, p.name)
		}
		asked, err := strconv.ParseInt(strings.TrimSpace(n.String()), 10, 64)
		if err != nil {
			// A float, an exponent, or something beyond int64. There is no
			// safe clamp for a value we could not read as an integer: 1e400
			// clamped to the maximum would look like a deliberate request.
			return nil, nil, refuse(CodeNotAnInteger, p.name)
		}
		used := asked
		if used < p.min {
			used = p.min
		}
		if used > p.max {
			used = p.max
		}
		if used != asked {
			return used, &Clamp{Field: p.name, Asked: asked, Used: used}, nil
		}
		return used, nil, nil

	case kindInstant:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, refuse(CodeWrongType, p.name)
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, nil, refuse(CodeNotAnInstant, p.name)
		}
		return t.UTC(), nil, nil

	case kindFlag:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, refuse(CodeWrongType, p.name)
		}
		return b, nil, nil

	case kindIdentList:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, nil, refuse(CodeWrongType, p.name)
		}
		if len(items) > p.maxItems {
			return nil, nil, refuseAt(CodeTooManyItems, p.name, int64(p.maxItems))
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ref := p.parseIdent(item)
			if ref != nil {
				return nil, nil, ref
			}
			out = append(out, s)
		}
		return out, nil, nil
	}
	return nil, nil, refuse(CodeWrongType, p.name)
}

// parseIdent is the identity path: length first, then cleanliness, and no
// truncation anywhere. Length is checked before content so that a 10 KiB name
// is reported as too long rather than as unclean — the two have different
// fixes, and the operator reading the audit trail needs the right one.
func (p Param) parseIdent(raw json.RawMessage) (string, *Refusal) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", refuse(CodeWrongType, p.name)
	}
	if len(s) > p.maxLen {
		return "", refuseAt(CodeTooLong, p.name, int64(p.maxLen))
	}
	if _, changed := scrubText(s, p.maxLen); changed {
		return "", refuse(CodeNotClean, p.name)
	}
	return s, nil
}

// decodeArgObject reads a flat JSON object, rejecting duplicate keys.
//
// encoding/json resolves a duplicate key by keeping the last, which makes
// `{"limit":10,"limit":9999}` a document that says two different things to
// two readers. Nothing downstream should have to know which reader it is.
func decodeArgObject(raw json.RawMessage) (map[string]json.RawMessage, *Refusal) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, refuse(CodeArgsNotObject, "")
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, refuse(CodeArgsNotObject, "")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, refuse(CodeArgsNotObject, "")
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, refuse(CodeArgsNotObject, "")
		}
		if _, dup := out[key]; dup {
			return nil, refuse(CodeUnknownArgument, "")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, refuse(CodeArgsNotObject, "")
		}
		out[key] = v
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, refuse(CodeArgsNotObject, "")
	}
	if dec.More() {
		return nil, refuse(CodeArgsNotObject, "")
	}
	return out, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
