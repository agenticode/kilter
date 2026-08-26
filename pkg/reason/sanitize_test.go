package reason

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScrubRemovesRatherThanReplaces. A replacement character is itself a
// rendering decision, and it would let two distinct names collapse into the
// same visible string — which is the confusion the scrub exists to prevent.
func TestScrubRemovesRatherThanReplaces(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"c0 escape":     {"app\x1b[2Kx", "app[2Kx"},
		"c1 csi":        {"app\u009b2Kx", "app2Kx"},
		"del":           {"app\x7fx", "appx"},
		"zero width":    {"pay\u200dments\u200b-api", "payments-api"},
		"bidi override": {"app\u202egnp-\u202c", "appgnp-"},
		"bom":           {"\ufeffapp", "app"},
		"clean":         {"payments-api", "payments-api"},
	} {
		got, changed := scrubText(tc.in, maxDisplayIdent)
		if got != tc.want {
			t.Errorf("%s: scrubbed to %q, want %q", name, got, tc.want)
		}
		if changed != (tc.in != tc.want) {
			t.Errorf("%s: reported changed=%v", name, changed)
		}
	}
}

// TestScrubTruncatesOnARuneBoundary. Cutting a multi-byte rune in half
// produces invalid UTF-8, which encoding/json silently replaces — and a
// silent replacement in the middle of an audit trail is a byte-identity
// failure nobody can trace.
func TestScrubTruncatesOnARuneBoundary(t *testing.T) {
	in := strings.Repeat("é", 100) // two bytes each
	got, changed := scrubText(in, 11)
	if !changed {
		t.Fatal("a truncation was not reported")
	}
	if len(got) != 10 || got != strings.Repeat("é", 5) {
		t.Fatalf("truncated to %d bytes: %q", len(got), got)
	}
}

// TestScrubJSONIsCanonicalAndDeep. Object keys are rendered too, so they are
// scrubbed too; and the output is canonical so the same document always
// encodes to the same bytes whichever Store produced it.
func TestScrubJSONIsCanonicalAndDeep(t *testing.T) {
	raw := []byte("{\"z\":1,\"a\":{\"na\u200bme\":\"pay\u200dments\"},\"n\":[{\"k\":\"a\u200db\"}],\"big\":123456789012345678}")
	got, n, err := scrubJSON(raw, maxDisplayIdent)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("reported %d scrubbed strings, want 3 (one key, two values)", n)
	}
	want := `{"a":{"name":"payments"},"big":123456789012345678,"n":[{"k":"ab"}],"z":1}`
	if string(got) != want {
		t.Fatalf("scrubbed to\n%s\nwant\n%s", got, want)
	}
	// Numbers keep their exact source text: a float64 round trip would turn
	// 123456789012345678 into 123456789012345680.
	if !strings.Contains(string(got), "123456789012345678") {
		t.Fatal("a large integer lost precision on the way through")
	}
}

// TestScrubJSONRefusesASecondDocument. Trailing content is a channel for text
// that every decoder downstream would ignore and a human reading raw bytes
// would not.
func TestScrubJSONRefusesASecondDocument(t *testing.T) {
	if _, _, err := scrubJSON([]byte(`{"a":1}{"b":2}`), maxDisplayIdent); err == nil {
		t.Fatal("two documents in one result were accepted")
	}
}

// TestKeysThatCollideAfterScrubbingResolveDeterministically. Two keys
// differing only by a zero-width rune become one key; which value survives
// must be a rule, not a map iteration.
func TestKeysThatCollideAfterScrubbingResolveDeterministically(t *testing.T) {
	raw := []byte("{\"na\u200bme\":\"first\",\"name\":\"second\"}")
	first := ""
	for i := 0; i < 50; i++ {
		got, _, err := scrubJSON(raw, maxDisplayIdent)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(got)
			continue
		}
		if string(got) != first {
			t.Fatalf("a post-scrub key collision resolved two ways: %s vs %s", first, got)
		}
	}
	var probe map[string]string
	if err := json.Unmarshal([]byte(first), &probe); err != nil {
		t.Fatal(err)
	}
	// The rule is "keep the lexicographically smaller encoding", which is
	// total; "keep whichever the map yielded first" is not.
	if len(probe) != 1 || probe["name"] != "first" {
		t.Fatalf("collision resolved to %v, want the lexicographically smaller value", probe)
	}
}

// TestOnlyKilterLinksSurvive. A URL is a model's only egress other than the
// answer itself.
func TestOnlyKilterLinksSurvive(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want string
		stripped int
	}{
		"external": {"see [here](https://evil.example/?d=secret) now", "see here now", 1},
		"kilter":   {"see [plan](kilter://clusters/prod/plan)", "see [plan](kilter://clusters/prod/plan)", 0},
		"relative": {"see [x](/api/v1)", "see x", 1},
		// A nested paren ends the target early; the tail is left as literal
		// text, which is the safe direction — a stray ")" is visible, a
		// half-parsed scheme is not.
		"js":         {"see [x](javascript:alert(1))", "see x)", 1},
		"spaced":     {"see [x](kilter://a b)", "see x", 1},
		"not a link": {"a [bracket] and a (paren)", "a [bracket] and a (paren)", 0},
		"unclosed":   {"a [bracket](unclosed", "a [bracket](unclosed", 0},
		"two":        {"[a](http://x) and [b](http://y)", "a and b", 2},
	} {
		got, n := stripExternalLinks(tc.in)
		if got != tc.want || n != tc.stripped {
			t.Errorf("%s: %q -> %q (%d stripped), want %q (%d)", name, tc.in, got, n, tc.want, tc.stripped)
		}
	}
}

// TestSortedKVNeverEmitsFromAMapRange.
func TestSortedKVNeverEmitsFromAMapRange(t *testing.T) {
	m := map[string]string{"image": "app:v1", "generation": "7", "a\u200db": "x", "zone": "eu-west-1a"}
	first := sortedKV(m, maxDisplayText)
	for i := 0; i < 50; i++ {
		got := sortedKV(m, maxDisplayText)
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("sortedKV is not stable: %+v vs %+v", first, got)
			}
		}
	}
	if first[0].K != "ab" || first[1].K != "generation" {
		t.Fatalf("sortedKV is not in key order after scrubbing: %+v", first)
	}
}
