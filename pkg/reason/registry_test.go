package reason

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

func TestEveryToolAnswersOverTheFixture(t *testing.T) {
	r := registry(t, substrate(t))
	subj := `"subject_kind":"container","subject_key":"` + containerKey + `"`
	for _, tc := range []struct{ tool, args string }{
		{ToolListSubjects, `{}`},
		{ToolGetDossier, `{` + subj + `}`},
		{ToolQueryEvidence, `{` + subj + `}`},
		{ToolClusterTimeline, `{}`},
		{ToolExplain, `{` + subj + `}`},
	} {
		out := call(t, r, tc.tool, tc.args)
		if !out.OK() {
			t.Errorf("%s refused: %v", tc.tool, out.Refusal)
			continue
		}
		if len(out.Data) == 0 {
			t.Errorf("%s returned no data", tc.tool)
		}
		if out.Bytes > DefaultMaxResultBytes {
			t.Errorf("%s returned %d bytes, over the cap", tc.tool, out.Bytes)
		}
		var probe any
		if err := json.Unmarshal(out.Data, &probe); err != nil {
			t.Errorf("%s returned invalid JSON: %v", tc.tool, err)
		}
	}
}

// TestEveryCitationATooLReturnsResolves is the invariant that makes the whole
// design safe: a tool never shows the model an id the substrate cannot
// re-serve. The loop checks again at publish time; this checks the source.
func TestEveryCitationATooLReturnsResolves(t *testing.T) {
	r := registry(t, substrate(t))
	subj := `"subject_kind":"container","subject_key":"` + containerKey + `"`
	seen := 0
	for _, tc := range []struct{ tool, args string }{
		{ToolGetDossier, `{` + subj + `}`},
		{ToolQueryEvidence, `{` + subj + `}`},
		{ToolClusterTimeline, `{}`},
		{ToolExplain, `{` + subj + `}`},
	} {
		out := call(t, r, tc.tool, tc.args)
		if !out.OK() {
			t.Fatalf("%s refused: %v", tc.tool, out.Refusal)
		}
		if len(out.Cites) == 0 {
			t.Errorf("%s cited nothing; every grounded row carries an id", tc.tool)
		}
		for _, id := range out.Cites {
			if _, err := r.Resolve(id); err != nil {
				t.Errorf("%s cited %s, which does not resolve: %v", tc.tool, id, err)
			}
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no citation was checked")
	}
}

// TestCitationsComeBackInASortedOrder. A tool that discovered ids by ranging a
// map would export that map's iteration order into the transcript, and from
// there into the audit trail, where it would break byte-identity for reasons
// nobody could see.
func TestCitationsComeBackInASortedOrder(t *testing.T) {
	r := registry(t, substrate(t))
	for i := 0; i < 8; i++ {
		out := call(t, r, ToolGetDossier,
			`{"subject_kind":"container","subject_key":"`+containerKey+`"}`)
		if !out.OK() {
			t.Fatal(out.Refusal)
		}
		for j := 1; j < len(out.Cites); j++ {
			if out.Cites[j-1] >= out.Cites[j] {
				t.Fatalf("citations are not strictly sorted: %s then %s", out.Cites[j-1], out.Cites[j])
			}
		}
	}
}

// TestTheSameCallProducesTheSameBytes. Determinism at the tool level, which
// everything above it inherits.
func TestTheSameCallProducesTheSameBytes(t *testing.T) {
	m := substrate(t)
	first := ""
	for i := 0; i < 5; i++ {
		r := registry(t, m) // a fresh registry each time
		out := call(t, r, ToolGetDossier,
			`{"subject_kind":"container","subject_key":"`+containerKey+`"}`)
		if !out.OK() {
			t.Fatal(out.Refusal)
		}
		got := string(out.JSON())
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, got)
		}
	}
}

// TestAnUnknownToolIsRefusedWithoutQuotingItsName.
func TestAnUnknownToolIsRefusedWithoutQuotingItsName(t *testing.T) {
	r := registry(t, substrate(t))
	out := call(t, r, "apply_recommendation", `{}`)
	if out.OK() || out.Refusal.Code != CodeUnknownTool {
		t.Fatalf("an unregistered tool gave %+v", out.Refusal)
	}
	if !strings.Contains(string(out.JSON()), `"status":"refused"`) {
		t.Fatalf("the refusal envelope does not say it refused: %s", out.JSON())
	}
}

// TestAWindowWiderThanTheScopeIsClampedAndAnEmptyOneIsRefused.
//
// The span is a quantity — an operator asking for 90 days of a 3-day window
// wants the 3 days, said out loud. A window that does not overlap the scope at
// all is refused, because "zero events" reads exactly like "nothing happened".
func TestAWindowWiderThanTheScopeIsClampedAndAnEmptyOneIsRefused(t *testing.T) {
	r := registry(t, substrate(t))
	subj := `"subject_kind":"container","subject_key":"` + containerKey + `"`

	wide := call(t, r, ToolQueryEvidence,
		`{`+subj+`,"from":"2020-01-01T00:00:00Z","to":"2030-01-01T00:00:00Z"}`)
	if !wide.OK() {
		t.Fatalf("a wide window was refused: %v", wide.Refusal)
	}
	if len(wide.Clamps) == 0 {
		t.Fatal("a window wider than the scope was served without saying so")
	}
	var got queryEvidenceOut
	if err := json.Unmarshal(wide.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.From.Before(t0) || got.To.After(tEnd) {
		t.Fatalf("served window [%v, %v) escapes the scope", got.From, got.To)
	}

	empty := call(t, r, ToolQueryEvidence,
		`{`+subj+`,"from":"2030-01-01T00:00:00Z","to":"2031-01-01T00:00:00Z"}`)
	if empty.OK() || empty.Refusal.Code != CodeWindowInverted {
		t.Fatalf("a non-overlapping window gave %+v", empty.Refusal)
	}
}

// TestASubjectOutsideTheUniverseIsRefusedRatherThanAnsweredEmpty.
func TestASubjectOutsideTheUniverseIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	r := registry(t, substrate(t))
	out := call(t, r, ToolQueryEvidence,
		`{"subject_kind":"container","subject_key":"default/Deployment/does-not-exist/app"}`)
	if out.OK() {
		t.Fatalf("an unknown subject was answered: %s", out.Data)
	}
	if out.Refusal.Code != CodeOutOfScope {
		t.Fatalf("an unknown subject gave %q", out.Refusal.Code)
	}
}

// TestTheEnvelopeLabelsEveryResultUntrusted. §5.7's data/instruction
// separation is carried by the payload, every turn, and not only by a system
// prompt that a long transcript pushes out of sight.
func TestTheEnvelopeLabelsEveryResultUntrusted(t *testing.T) {
	r := registry(t, substrate(t))
	out := call(t, r, ToolListSubjects, `{}`)
	var env resultEnvelope
	if err := json.Unmarshal(out.JSON(), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Untrusted || env.Note != untrustedNote {
		t.Fatalf("envelope is not labelled untrusted: %+v", env)
	}
}

// TestATimingOutToolIsRefusedNotAwaited, and a panicking one costs an answer
// rather than the brain.
func TestATimingOutToolIsRefusedNotAwaited(t *testing.T) {
	r := registry(t, substrate(t))
	schema, err := NewSchema()
	if err != nil {
		t.Fatal(err)
	}
	slow, err := readOnlyTool("slow", "sleeps past its box", schema, 20*time.Millisecond,
		func(ctx context.Context, _ Input) (Result, error) {
			<-ctx.Done()
			time.Sleep(5 * time.Millisecond)
			return result(map[string]string{"never": "arrives"}, nil, nil)
		})
	if err != nil {
		t.Fatal(err)
	}
	boom, err := readOnlyTool("boom", "panics", schema, time.Second,
		func(context.Context, Input) (Result, error) {
			var m map[string]string
			m["x"] = "y" // nil map write
			return Result{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(slow); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(boom); err != nil {
		t.Fatal(err)
	}

	if out := call(t, r, "slow", `{}`); out.OK() || out.Refusal.Code != CodeToolTimeout {
		t.Fatalf("a tool past its time box gave %+v", out.Refusal)
	}
	if out := call(t, r, "boom", `{}`); out.OK() || out.Refusal.Code != CodeToolFailed {
		t.Fatalf("a panicking tool gave %+v", out.Refusal)
	}
}

// TestAnOversizeResultIsRefusedRatherThanTruncated. Truncated JSON is not
// JSON, and a model handed half a document will confabulate the rest.
func TestAnOversizeResultIsRefusedRatherThanTruncated(t *testing.T) {
	r := registry(t, substrate(t))
	schema, err := NewSchema()
	if err != nil {
		t.Fatal(err)
	}
	// Many rows rather than one enormous string: scrubJSON caps individual
	// strings for display (and counts the change), so the byte cap has to be
	// provoked with structure, which is also how a real tool would blow it.
	fat, err := readOnlyTool("fat", "returns more than the cap", schema, time.Second,
		func(context.Context, Input) (Result, error) {
			rows := make([]string, 500)
			for i := range rows {
				rows[i] = strings.Repeat("x", 40)
			}
			return result(struct {
				Rows []string `json:"rows"`
			}{rows}, nil, nil)
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fat); err != nil {
		t.Fatal(err)
	}
	out := call(t, r, "fat", `{}`)
	if out.OK() {
		t.Fatalf("an oversize result was served (%d bytes)", out.Bytes)
	}
	if out.Refusal.Code != CodeResultTooLarge || out.Refusal.Limit != DefaultMaxResultBytes {
		t.Fatalf("an oversize result gave %+v", out.Refusal)
	}
}

// TestPaginationIsAStableTotalOrder. §5.2 requires deterministic pagination:
// two pages must partition the matched set, with no row seen twice or missed.
func TestPaginationIsAStableTotalOrder(t *testing.T) {
	keys := []string{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		keys = append(keys, "default/Deployment/"+n+"/app")
	}
	r := registry(t, substrate(t, keys...))
	seen := map[string]bool{}
	for offset := 0; offset < 8; offset += 3 {
		out := call(t, r, ToolListSubjects, `{"limit":3,"offset":`+itoa(offset)+`}`)
		if !out.OK() {
			t.Fatal(out.Refusal)
		}
		var page listSubjectsOut
		if err := json.Unmarshal(out.Data, &page); err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Subjects {
			if seen[row.Key] {
				t.Fatalf("key %q appeared on two pages", row.Key)
			}
			seen[row.Key] = true
		}
	}
	if len(seen) != len(keys) {
		t.Fatalf("paging saw %d of %d subjects", len(seen), len(keys))
	}
}

// TestTheTimelineIsSampledAcrossTheWindowNotTruncatedToOneEnd. A cost timeline
// read from one end says nothing about the shape of the window that was asked
// about, which is how "cost rose on the 14th" becomes invisible.
func TestTheTimelineIsSampledAcrossTheWindowNotTruncatedToOneEnd(t *testing.T) {
	r := registry(t, substrate(t))
	out := call(t, r, ToolClusterTimeline, `{"points":4}`)
	if !out.OK() {
		t.Fatal(out.Refusal)
	}
	var got timelineOut
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Stored < 20 || len(got.Points) != 4 {
		t.Fatalf("sampled %d of %d points", len(got.Points), got.Stored)
	}
	if !got.Points[0].At.Equal(t0) {
		t.Fatalf("the first sampled point is %v, want the window's first stored point", got.Points[0].At)
	}
	if !got.Points[3].At.After(got.Points[0].At.Add(20 * time.Hour)) {
		t.Fatalf("the last sampled point is %v; the sample does not span the window", got.Points[3].At)
	}
}

func TestEvenIndicesSpansAndStaysStrictlyIncreasing(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 7, 24, 1000} {
		for _, want := range []int{1, 2, 5, 24, 48} {
			got := evenIndices(n, want)
			if n == 0 {
				if got != nil {
					t.Errorf("evenIndices(0,%d) = %v", want, got)
				}
				continue
			}
			if len(got) > want || len(got) > n {
				t.Errorf("evenIndices(%d,%d) returned %d indices", n, want, len(got))
			}
			if got[0] != 0 {
				t.Errorf("evenIndices(%d,%d) does not start at 0", n, want)
			}
			for i := 1; i < len(got); i++ {
				if got[i] <= got[i-1] || got[i] >= n {
					t.Errorf("evenIndices(%d,%d) = %v is not a strictly increasing index set", n, want, got)
					break
				}
			}
			if want > 1 && n > 1 && got[len(got)-1] != n-1 {
				t.Errorf("evenIndices(%d,%d) misses the last point", n, want)
			}
		}
	}
}

// TestRegistryConstructionRefusesAnUnboundedScope. The window is an argument,
// and a registry without one would serve a different answer every minute.
func TestRegistryConstructionRefusesAnUnboundedScope(t *testing.T) {
	m := substrate(t)
	for name, sc := range map[string]Scope{
		"no cluster":    {From: t0, To: tEnd},
		"no window":     {Cluster: cluster},
		"inverted":      {Cluster: cluster, From: tEnd, To: t0},
		"empty":         {Cluster: cluster, From: t0, To: t0},
		"half a window": {Cluster: cluster, From: t0},
	} {
		if _, err := NewRegistry(RegistryConfig{Scope: sc, Store: m}); err == nil {
			t.Errorf("NewRegistry accepted the %q scope", name)
		}
	}
	if _, err := NewRegistry(RegistryConfig{Scope: testScope()}); err == nil {
		t.Error("NewRegistry accepted a nil store")
	}
}

// TestTheToolsDigestMovesWithTheSurface. The audit trail records it so a
// replay can tell "the model behaved differently" from "the model was offered
// a different surface".
func TestTheToolsDigestMovesWithTheSurface(t *testing.T) {
	m := substrate(t)
	a := registry(t, m)
	b := registry(t, m)
	if a.Digest() != b.Digest() {
		t.Fatal("two registries over the same surface digest differently")
	}
	schema, err := NewSchema()
	if err != nil {
		t.Fatal(err)
	}
	extra, err := readOnlyTool("extra", "one more", schema, time.Second,
		func(context.Context, Input) (Result, error) { return result(struct{}{}, nil, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Register(extra); err != nil {
		t.Fatal(err)
	}
	if a.Digest() == b.Digest() {
		t.Fatal("adding a tool did not move the surface digest")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

var _ = evidence.SubjectContainer
