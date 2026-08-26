package reason

import (
	"encoding/json"
	"strings"
	"testing"
)

func testSchema(t *testing.T) Schema {
	t.Helper()
	s, err := NewSchema(
		Ident("key", "an identity"),
		Enum("kind", "a closed set", false, "container", "workload"),
		Quantity("limit", "a bounded quantity", 1, 50, 20),
		Instant("from", "a coordinate"),
		Flag("verbose", "a flag"),
		IdentList("kinds", "a bounded list of identities", 3),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAQuantityClampsAndSaysSo is half the disposition rule: asking for more
// rows than the schema allows is served, at the schema's number, out loud.
func TestAQuantityClampsAndSaysSo(t *testing.T) {
	s := testSchema(t)
	args, clamps, ref := s.Validate(json.RawMessage(`{"key":"k","limit":9999}`))
	if ref != nil {
		t.Fatalf("an over-large quantity was refused (%v); quantities clamp", ref)
	}
	if got := args.Int("limit"); got != 50 {
		t.Fatalf("limit clamped to %d, want 50", got)
	}
	if len(clamps) != 1 || clamps[0].Field != "limit" || clamps[0].Asked != 9999 || clamps[0].Used != 50 {
		t.Fatalf("clamp reported as %+v; a silent clamp is a lie about what was served", clamps)
	}
	// And below the floor, the same way.
	_, clamps, ref = s.Validate(json.RawMessage(`{"key":"k","limit":-4}`))
	if ref != nil || len(clamps) != 1 || clamps[0].Used != 1 {
		t.Fatalf("a below-floor quantity gave ref=%v clamps=%+v", ref, clamps)
	}
}

// TestAnIdentityRefusesRatherThanTruncating is the other half, and the one
// that matters under attack: a truncated identifier is a different identifier
// that frequently still resolves, so a 600-byte name must not silently become
// a lookup of the 512-byte prefix.
func TestAnIdentityRefusesRatherThanTruncating(t *testing.T) {
	s := testSchema(t)
	long := strings.Repeat("a", maxDisplayIdent+1)
	_, _, ref := s.Validate(json.RawMessage(`{"key":"` + long + `"}`))
	if ref == nil {
		t.Fatal("an over-long identity was accepted")
	}
	if ref.Code != CodeTooLong || ref.Field != "key" || ref.Limit != maxDisplayIdent {
		t.Fatalf("refusal is %+v, want too-long on key at the schema's own bound", ref)
	}
	// A list refuses on count for the same reason: a shortened list answers a
	// narrower question than the one that was asked.
	_, _, ref = s.Validate(json.RawMessage(`{"key":"k","kinds":["a","b","c","d"]}`))
	if ref == nil || ref.Code != CodeTooManyItems || ref.Limit != 3 {
		t.Fatalf("an over-long list gave %+v, want a refusal at the item bound", ref)
	}
}

// TestUnknownArgumentsAreRefusedNotDropped. A dropped filter turns a narrow
// question into a broad answer, which is the failure nobody notices.
func TestUnknownArgumentsAreRefusedNotDropped(t *testing.T) {
	s := testSchema(t)
	_, _, ref := s.Validate(json.RawMessage(`{"key":"k","namespace":"payments"}`))
	if ref == nil || ref.Code != CodeUnknownArgument {
		t.Fatalf("an undeclared argument gave %+v", ref)
	}
	// And the refusal does not name it: an undeclared name was chosen by the
	// caller, and repeating it is the echo the whole defense exists to stop.
	if ref.Field != "" {
		t.Fatalf("the refusal names the undeclared argument %q", ref.Field)
	}
}

// TestADuplicateKeyIsRefused. `{"limit":10,"limit":9999}` says two different
// things to two readers; encoding/json keeps the last, a streaming validator
// might see the first, and nothing downstream should have to know which it is.
func TestADuplicateKeyIsRefused(t *testing.T) {
	s := testSchema(t)
	_, _, ref := s.Validate(json.RawMessage(`{"key":"k","limit":10,"limit":9999}`))
	if ref == nil {
		t.Fatal("a duplicate argument key was accepted")
	}
}

// TestAQuantityThatIsNotAnIntegerIsRefusedRatherThanClamped. There is no safe
// clamp for a value that could not be read as an integer: 1e400 lowered to the
// maximum would look like a deliberate request for the maximum.
func TestAQuantityThatIsNotAnIntegerIsRefusedRatherThanClamped(t *testing.T) {
	s := testSchema(t)
	for _, arg := range []string{`{"key":"k","limit":1.5}`, `{"key":"k","limit":1e400}`, `{"key":"k","limit":"20"}`} {
		_, clamps, ref := s.Validate(json.RawMessage(arg))
		if ref == nil {
			t.Errorf("%s was accepted", arg)
			continue
		}
		if len(clamps) != 0 {
			t.Errorf("%s produced clamps %+v as well as a refusal", arg, clamps)
		}
		if ref.Code != CodeNotAnInteger && ref.Code != CodeWrongType {
			t.Errorf("%s gave %q", arg, ref.Code)
		}
	}
}

// TestMissingRequiredAndBadEnumAreRefused.
func TestMissingRequiredAndBadEnumAreRefused(t *testing.T) {
	s := testSchema(t)
	if _, _, ref := s.Validate(json.RawMessage(`{}`)); ref == nil || ref.Code != CodeMissingArgument {
		t.Fatalf("a missing required argument gave %+v", ref)
	}
	if _, _, ref := s.Validate(json.RawMessage(`{"key":"k","kind":"Container"}`)); ref == nil || ref.Code != CodeNotAllowed {
		t.Fatalf("a near-miss enum gave %+v; the set is compared exactly", ref)
	}
	if _, _, ref := s.Validate(json.RawMessage(`{"key":"k","from":"yesterday"}`)); ref == nil || ref.Code != CodeNotAnInstant {
		t.Fatalf("a non-RFC3339 instant gave %+v", ref)
	}
}

// TestAnExplicitNullIsAnAbsentArgument. Models emit null for optional fields
// constantly; treating it as a type error would refuse well-formed calls, and
// treating it as a value would put a nil where a string belongs.
func TestAnExplicitNullIsAnAbsentArgument(t *testing.T) {
	s := testSchema(t)
	args, _, ref := s.Validate(json.RawMessage(`{"key":"k","kind":null,"limit":null}`))
	if ref != nil {
		t.Fatalf("an explicit null was refused: %v", ref)
	}
	if args.Has("kind") {
		t.Fatal("a null argument was recorded as supplied")
	}
	if got := args.Int("limit"); got != 20 {
		t.Fatalf("a null quantity took %d rather than its default 20", got)
	}
}

// TestAnEnormousArgumentObjectIsRefusedBeforeItIsParsed. The cap exists so a
// 10 KiB name costs one length comparison, not a decode.
func TestAnEnormousArgumentObjectIsRefusedBeforeItIsParsed(t *testing.T) {
	s := testSchema(t)
	huge := `{"key":"` + strings.Repeat("x", 10<<10) + `"}`
	_, _, ref := s.Validate(json.RawMessage(huge))
	if ref == nil || ref.Code != CodeArgsTooLarge {
		t.Fatalf("a %d-byte argument object gave %+v", len(huge), ref)
	}
}

// TestTheEmittedSchemaIsStrictAndStable. additionalProperties:false is what
// makes "unknown arguments are refused" a contract with the model rather than
// only a check on our side; stability is what lets the tool block be a cache
// prefix (§5.4).
func TestTheEmittedSchemaIsStrictAndStable(t *testing.T) {
	a := testSchema(t)
	b, err := NewSchema( // same parameters, declared in a different order
		IdentList("kinds", "a bounded list of identities", 3),
		Flag("verbose", "a flag"),
		Instant("from", "a coordinate"),
		Quantity("limit", "a bounded quantity", 1, 50, 20),
		Enum("kind", "a closed set", false, "workload", "container"),
		Ident("key", "an identity"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.JSON()) != string(b.JSON()) {
		t.Fatalf("declaration order leaked into the schema:\n%s\n%s", a.JSON(), b.JSON())
	}
	if !strings.Contains(string(a.JSON()), `"additionalProperties":false`) {
		t.Fatal("the emitted schema is not strict")
	}
	var probe map[string]any
	if err := json.Unmarshal(a.JSON(), &probe); err != nil {
		t.Fatalf("the emitted schema is not valid JSON: %v", err)
	}
}

// TestNewSchemaRefusesAnIncoherentParameterSet.
func TestNewSchemaRefusesAnIncoherentParameterSet(t *testing.T) {
	for name, params := range map[string][]Param{
		"duplicate":         {Ident("k", "d"), Ident("k", "d")},
		"unconstructed":     {{}},
		"default-out":       {Quantity("n", "d", 1, 10, 99)},
		"inverted":          {Quantity("n", "d", 10, 1, 5)},
		"empty-enum":        {Enum("e", "d", false)},
		"list-with-no-room": {IdentList("l", "d", 0)},
	} {
		if _, err := NewSchema(params...); err == nil {
			t.Errorf("NewSchema accepted the %s case", name)
		}
	}
}
