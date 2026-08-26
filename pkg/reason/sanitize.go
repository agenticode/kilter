package reason

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The display half of §5.7. Everything here answers one question: a string
// arrived from a cluster anyone with kubectl can write to, and it is about to
// be shown to a model, an operator's terminal, or a browser. What has to come
// off it first?
//
// pkg/evidence already strips C0 controls and DEL at ingest, and that is the
// load-bearing pass. It is deliberately not trusted here, for two reasons.
// The first is that this package must also declaw strings that never went
// through the substrate — an operator's own question, a model's answer, a
// future Store implementation. The second is measured rather than assumed:
// evidence.cleanString tests `r < 0x20 || r == 0x7f`, so the C1 block
// survives it, and U+009B is the single-byte CSI introducer that
// xterm-family terminals honour exactly like ESC-[. A workload named
// "<U+009B>2K" clears the operator's line without ever containing an ESC.
// Zero-width and bidi format runes (U+200B..U+200F, U+202A..U+202E,
// U+2066..U+2069, U+FEFF) survive ingest too, and those are how two different
// subjects are made to render identically.

// Display caps. Free-text fields are length-capped at 128 characters per
// §5.7; identifiers get more room because a container key is legitimately
// long (evidence allows 512 bytes) and truncating one produces a different
// identifier rather than a shorter one.
const (
	maxDisplayText  = 128
	maxDisplayIdent = 512
	// maxQuestionBytes bounds the operator's own question. The question is
	// the one input the harness cannot re-derive, so it is capped rather
	// than scrubbed into silence.
	maxQuestionBytes = 4096
	// maxAnswerBytes bounds a model's answer before it is published.
	maxAnswerBytes = 32 << 10
)

// unsafeRune reports whether r must never reach a terminal, a model, or a
// browser through this package.
func unsafeRune(r rune) bool {
	switch {
	case r == utf8.RuneError:
		return true // invalid UTF-8, decoded byte-wise by the caller
	case r < 0x20, r == 0x7f: // C0 and DEL
		return true
	case r >= 0x80 && r <= 0x9f: // C1: U+009B is CSI, and needs no ESC
		return true
	case r == 0xfeff: // BOM / zero-width no-break space
		return true
	case r >= 0x200b && r <= 0x200f: // zero-width space .. RLM
		return true
	case r >= 0x202a && r <= 0x202e: // bidi embedding / override
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case unicode.Is(unicode.Cf, r): // every other format rune
		return true
	}
	return false
}

// scrubText removes every unsafe rune and caps the result at max bytes on a
// rune boundary. It reports whether it changed anything, because a silent
// scrub is a silent lie about what the cluster actually contains: callers
// surface the flag rather than swallowing it.
//
// Removal, not replacement: a replacement character is itself a rendering
// decision, and the only thing a caller can usefully do with "this name had a
// bidi override in it" is say so, which the flag lets it do.
func scrubText(s string, max int) (string, bool) {
	clean := true
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if unsafeRune(r) {
			clean = false
			continue
		}
		if b.Len()+size > max {
			clean = false
			break
		}
		b.WriteRune(r)
	}
	if clean {
		return s, false
	}
	return b.String(), true
}

// scrubJSON walks a JSON document and scrubs every string in it — object keys
// included, because a key is rendered too. It returns compact, canonical
// bytes: numbers keep their exact source text (json.Number), and object keys
// are emitted in sorted order, so the same document always encodes to the
// same bytes no matter which Store produced it.
//
// The count is how many strings were altered. Zero is the common case and the
// one the hostile-corpus tests are written to disturb.
func scrubJSON(raw []byte, maxString int) (json.RawMessage, int, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, 0, err
	}
	// Trailing content would mean two documents in one tool result; the
	// second would never be seen by a decoder and is therefore a channel
	// for smuggling text past every check that reads the first.
	if dec.More() {
		return nil, 0, errTrailingJSON
	}
	n := 0
	out := scrubValue(v, maxString, &n)
	enc, err := json.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	return json.RawMessage(enc), n, nil
}

// scrubValue rebuilds a decoded document with every string scrubbed. Maps are
// rebuilt as maps: encoding/json sorts map keys on the way out, which is the
// canonical order this package relies on everywhere.
func scrubValue(v any, maxString int, n *int) any {
	switch t := v.(type) {
	case string:
		s, changed := scrubText(t, maxString)
		if changed {
			*n++
		}
		return s
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = scrubValue(e, maxString, n)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			key, changed := scrubText(k, maxDisplayText)
			if changed {
				*n++
			}
			val := scrubValue(e, maxString, n)
			// A collision after scrubbing means two keys differed only by
			// runes that must not be shown. Keeping the lexicographically
			// smaller encoding is a total rule; picking by map order is not.
			if prev, dup := out[key]; dup && lessJSON(prev, val) {
				continue
			}
			out[key] = val
		}
		return out
	}
	return v // numbers, bools, null
}

// lessJSON is a total order over two already-scrubbed values, used only to
// break a post-scrub key collision deterministically.
func lessJSON(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) < string(bb)
}

// stripExternalLinks removes the target of every markdown link that does not
// point at kilter's own resource scheme, keeping the link text.
//
// §5.7 assigns this to "the renderer". This package does it anyway, because
// the renderer is not one program: an answer reaches a terminal, a browser, a
// webhook and an MCP client, and those disagree about what a link is. A URL
// is a model's only egress other than the answer itself — read the cluster,
// encode it in a query string, get the operator to click — so it is closed
// here, once, above every renderer.
func stripExternalLinks(s string) (string, int) {
	stripped := 0
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '[' {
			b.WriteByte(s[i])
			i++
			continue
		}
		shut := strings.IndexByte(s[i:], ']')
		if shut < 0 || i+shut+1 >= len(s) || s[i+shut+1] != '(' {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i+shut+1:], ')')
		if end < 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		text := s[i+1 : i+shut]
		target := s[i+shut+2 : i+shut+1+end]
		if allowedLinkTarget(target) {
			b.WriteString(s[i : i+shut+2+end])
		} else {
			stripped++
			b.WriteString(text)
		}
		i += shut + 2 + end
	}
	return b.String(), stripped
}

// allowedLinkTarget permits only kilter's own resource scheme — the same URIs
// §6 exposes over MCP. Everything else, http(s) included, is a way out.
func allowedLinkTarget(t string) bool {
	return strings.HasPrefix(t, "kilter://") && !strings.ContainsAny(t, " \t\"'<>")
}

// kv is one sorted key/value pair. Nothing in this package emits from a map
// range; sortedKV is the single conversion point.
type kv struct {
	K string `json:"k"`
	V string `json:"v"`
}

func sortedKV(m map[string]string, maxVal int) []kv {
	if len(m) == 0 {
		return nil
	}
	out := make([]kv, 0, len(m))
	for k, v := range m {
		ck, _ := scrubText(k, maxDisplayText)
		cv, _ := scrubText(v, maxVal)
		out = append(out, kv{K: ck, V: cv})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].K != out[j].K {
			return out[i].K < out[j].K
		}
		return out[i].V < out[j].V
	})
	return out
}
