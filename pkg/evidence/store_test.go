package evidence

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConfigValidation pins which configurations are refused. A store that
// accepts a nonsense bound is a store with no bound.
func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"zero config takes defaults", func(*Config) {}, true},
		{"negative ring cap", func(c *Config) { c.MaxEventsPerSubject = -1 }, false},
		{"ring cap over hard max", func(c *Config) { c.MaxEventsPerSubject = maxRingCap + 1 }, false},
		{"ring cap at hard max", func(c *Config) { c.MaxEventsPerSubject = maxRingCap }, true},
		{"negative age", func(c *Config) { c.EventMaxAge = -time.Hour }, false},
		{"tiny byte budget", func(c *Config) { c.MaxEventBytes = 1 }, false},
		{"byte budget at floor", func(c *Config) { c.MaxEventBytes = minBudgetBytes }, true},
		{"negative series budget", func(c *Config) { c.MaxSeriesBytes = -1 }, false},
		{"tolerance zero uses default", func(c *Config) { c.CoalesceTolerance = 0 }, true},
		{"tolerance one", func(c *Config) { c.CoalesceTolerance = 1 }, false},
		{"tolerance negative", func(c *Config) { c.CoalesceTolerance = -0.5 }, false},
		{"tolerance NaN", func(c *Config) { c.CoalesceTolerance = nan() }, false},
		{"tolerance Inf", func(c *Config) { c.CoalesceTolerance = inf() }, false},
		{"payload cap too small", func(c *Config) { c.MaxDecisionPayloadBytes = 1 }, false},
		{"payload cap too big", func(c *Config) { c.MaxDecisionPayloadBytes = 1<<20 + 1 }, false},
		{"negative timeline clusters", func(c *Config) { c.MaxTimelineClusters = -3 }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			tc.mut(&c)
			m, err := NewMemory(c)
			if (err == nil) != tc.ok {
				t.Fatalf("NewMemory = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && m.Config() != DefaultConfig().withDefaults() && c == (Config{}) {
				t.Error("zero config did not resolve to the defaults")
			}
		})
	}
}

// TestCleanString covers control-character stripping, invalid UTF-8 removal
// and rune-boundary truncation — the §5.7 ingest hygiene rules.
func TestCleanString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"plain", "hello", 16, "hello"},
		{"empty", "", 16, ""},
		{"strips newline", "a\nb", 16, "ab"},
		{"strips tab and CR", "a\tb\rc", 16, "abc"},
		{"strips NUL", "a\x00b", 16, "ab"},
		{"strips DEL", "a\x7fb", 16, "ab"},
		{"strips ANSI escape", "a\x1b[31mred", 16, "a[31mred"},
		{"keeps unicode", "héllo→", 32, "héllo→"},
		{"drops invalid utf8", "a\xffb", 16, "ab"},
		{"truncates ascii", "abcdef", 3, "abc"},
		{"truncates at rune boundary", "ab→cd", 4, "ab"},
		{"keeps whole rune when it fits", "ab→cd", 5, "ab→"},
		{"zero max", "abc", 0, ""},
		{"keeps replacement char", "a�b", 16, "a�b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanString(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("cleanString(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if len(got) > tc.max {
				t.Errorf("result %q exceeds max %d", got, tc.max)
			}
			// Idempotence: cleaning a clean string is a no-op, which is what
			// makes cleanStringOK a valid restore-side dual.
			if again := cleanString(got, tc.max); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
			if !cleanStringOK(got, tc.max) {
				t.Errorf("cleanStringOK rejected its own output %q", got)
			}
		})
	}
}

// TestSanitizeEvent covers rejection and normalization of appended events.
func TestSanitizeEvent(t *testing.T) {
	s := subj("a")
	tests := []struct {
		name    string
		in      EvidenceEvent
		wantErr bool
		check   func(*testing.T, EvidenceEvent)
	}{
		{"zero time", EvidenceEvent{Kind: "deploy", Subject: s}, true, nil},
		{"no kind", EvidenceEvent{At: t0, Subject: s}, true, nil},
		{"kind is only control chars", EvidenceEvent{At: t0, Kind: "\n\t", Subject: s}, true, nil},
		{"no subject kind", EvidenceEvent{At: t0, Kind: "k", Subject: SubjectRef{Key: "x"}}, true, nil},
		{"no subject key", EvidenceEvent{At: t0, Kind: "k", Subject: SubjectRef{Kind: "container"}}, true, nil},
		{"bad severity", EvidenceEvent{At: t0, Kind: "k", Subject: s, Severity: "meh"}, true, nil},
		{"empty severity defaults to info", EvidenceEvent{At: t0, Kind: "k", Subject: s}, false,
			func(t *testing.T, e EvidenceEvent) {
				if e.Severity != SeverityInfo {
					t.Errorf("Severity = %q, want info", e.Severity)
				}
			}},
		{"caller count is discarded", EvidenceEvent{At: t0, Kind: "k", Subject: s, Count: 99}, false,
			func(t *testing.T, e EvidenceEvent) {
				if e.Count != 0 {
					t.Errorf("Count = %d, want 0", e.Count)
				}
			}},
		{"time normalized to utc",
			EvidenceEvent{At: t0.In(time.FixedZone("KST", 9*3600)), Kind: "k", Subject: s}, false,
			func(t *testing.T, e EvidenceEvent) {
				if e.At.Location() != time.UTC {
					t.Errorf("At location = %v", e.At.Location())
				}
			}},
		{"long kind truncated",
			EvidenceEvent{At: t0, Kind: strings.Repeat("k", 500), Subject: s}, false,
			func(t *testing.T, e EvidenceEvent) {
				if len(e.Kind) != maxKindLen {
					t.Errorf("Kind len = %d, want %d", len(e.Kind), maxKindLen)
				}
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeEvent(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if err := validateEvent(got); err != nil {
				t.Errorf("sanitized event fails validateEvent: %v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestSanitizeAttrs: the attr map is count- and length-capped, freshly
// allocated (never aliasing caller memory), and truncation is deterministic.
func TestSanitizeAttrs(t *testing.T) {
	if got := sanitizeAttrs(nil); got != nil {
		t.Errorf("nil attrs -> %v", got)
	}
	if got := sanitizeAttrs(map[string]string{"\n": "v"}); got != nil {
		t.Errorf("attrs with only unusable keys -> %v, want nil", got)
	}

	in := map[string]string{}
	for i := 0; i < maxAttrs*3; i++ {
		in[fmt.Sprintf("k%03d", i)] = "v"
	}
	first := sanitizeAttrs(in)
	if len(first) != maxAttrs {
		t.Fatalf("attrs = %d, want cap %d", len(first), maxAttrs)
	}
	// Deterministic: the lexicographically-first keys survive, every time.
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(sanitizeAttrs(in), first) {
			t.Fatal("attr truncation is not deterministic across map iterations")
		}
	}
	for k := range first {
		if k >= fmt.Sprintf("k%03d", maxAttrs) {
			t.Errorf("kept key %q is not among the lexicographically-first %d", k, maxAttrs)
		}
	}

	// Values are truncated and the caller's map is untouched.
	src := map[string]string{"key": strings.Repeat("x", 900)}
	out := sanitizeAttrs(src)
	if len(out["key"]) != maxAttrValLen {
		t.Errorf("value len = %d, want %d", len(out["key"]), maxAttrValLen)
	}
	if len(src["key"]) != 900 {
		t.Error("sanitizeAttrs mutated the caller's map")
	}
	out["key"] = "changed"
	if src["key"] == "changed" {
		t.Error("sanitized attrs alias the caller's map")
	}
}

// TestAppendDedup: repeated warnings fold into one stored event with a
// running count, the newest timestamp and the highest severity seen.
func TestAppendDedup(t *testing.T) {
	m := newMem(t, Config{DedupWindow: time.Hour})
	s := subj("a")

	base := ev(s, EventProbeFailure, 0)
	base.Dedup = "probe/readiness"
	mustAppend(t, m, base)

	// Same (Kind, Dedup) inside the window: folds, with a severity upgrade.
	up := base
	up.At = at(30 * time.Minute)
	up.Severity = SeverityCritical
	mustAppend(t, m, up)

	// Outside the window: stored separately.
	far := base
	far.At = at(3 * time.Hour)
	mustAppend(t, m, far)

	// Different dedup key: stored separately.
	other := base
	other.At = at(3*time.Hour + time.Minute)
	other.Dedup = "probe/liveness"
	mustAppend(t, m, other)

	// Different kind, same dedup key: stored separately.
	kind := base
	kind.At = at(3*time.Hour + 2*time.Minute)
	kind.Kind = EventOOMKill
	mustAppend(t, m, kind)

	got, err := m.Events(s, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("stored %d events, want 4: %+v", len(got), got)
	}
	first := got[0]
	if first.Count != 1 {
		t.Errorf("folded Count = %d, want 1", first.Count)
	}
	if !first.At.Equal(at(30 * time.Minute)) {
		t.Errorf("folded At = %v, want the newer timestamp", first.At)
	}
	if first.Severity != SeverityCritical {
		t.Errorf("folded Severity = %q, want the upgraded critical", first.Severity)
	}
	if m.Stats().CoalescedEvents != 1 {
		t.Errorf("CoalescedEvents = %d, want 1", m.Stats().CoalescedEvents)
	}
	checkMemory(t, m)
}

// TestAppendDedupSeverityUpgradeIsAccounted: a severity upgrade changes the
// stored event's byte cost. If the store forgets to re-account it, the
// global byte budget drifts from reality — silently, forever.
func TestAppendDedupSeverityUpgradeIsAccounted(t *testing.T) {
	m := newMem(t, Config{DedupWindow: time.Hour})
	s := subj("a")
	e := ev(s, EventProbeFailure, 0)
	e.Dedup = "d"
	e.Severity = SeverityInfo
	mustAppend(t, m, e)
	e.At = at(time.Minute)
	e.Severity = SeverityCritical
	mustAppend(t, m, e)
	checkMemory(t, m)
}

// TestAppendDedupNoKeyNeverFolds: without a Dedup key every append is its
// own event, even if byte-identical.
func TestAppendDedupNoKeyNeverFolds(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	for i := 0; i < 5; i++ {
		mustAppend(t, m, ev(s, EventDeploy, time.Duration(i)*time.Minute))
	}
	got, _ := m.Events(s, time.Time{}, time.Time{})
	if len(got) != 5 {
		t.Fatalf("stored %d events, want 5", len(got))
	}
	if m.Stats().CoalescedEvents != 0 {
		t.Error("events without a dedup key were folded")
	}
}

// TestAppendCountSaturates: the fold counter must saturate rather than
// overflow into a negative count.
func TestAppendCountSaturates(t *testing.T) {
	m := newMem(t, Config{DedupWindow: time.Hour})
	s := subj("a")
	e := ev(s, EventProbeFailure, 0)
	e.Dedup = "d"
	mustAppend(t, m, e)
	m.mu.Lock()
	m.events.subs[s].ring.at(0).v.Count = maxEventCount
	m.mu.Unlock()
	e.At = at(time.Minute)
	mustAppend(t, m, e)
	got, _ := m.Events(s, time.Time{}, time.Time{})
	if got[0].Count != maxEventCount {
		t.Errorf("Count = %d, want it pinned at %d", got[0].Count, maxEventCount)
	}
}

// TestEventsWindowAndKinds is the §3.4 query contract.
func TestEventsWindowAndKinds(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	kinds := []string{EventDeploy, EventOOMKill, EventHPAScale}
	for i := 0; i < 9; i++ {
		mustAppend(t, m, ev(s, kinds[i%3], time.Duration(i)*time.Hour))
	}
	tests := []struct {
		name  string
		from  time.Time
		to    time.Time
		kinds []string
		want  int
	}{
		{"everything", time.Time{}, time.Time{}, nil, 9},
		{"half open excludes to", at(0), at(3 * time.Hour), nil, 3},
		{"includes from", at(2 * time.Hour), at(3 * time.Hour), nil, 1},
		{"empty window", at(2 * time.Hour), at(2 * time.Hour), nil, 0},
		{"open start", time.Time{}, at(2 * time.Hour), nil, 2},
		{"open end", at(7 * time.Hour), time.Time{}, nil, 2},
		{"single kind", time.Time{}, time.Time{}, []string{EventOOMKill}, 3},
		{"two kinds", time.Time{}, time.Time{}, []string{EventOOMKill, EventDeploy}, 6},
		{"unknown kind", time.Time{}, time.Time{}, []string{"nope"}, 0},
		{"kind and window", at(0), at(4 * time.Hour), []string{EventDeploy}, 2},
		{"window past the end", at(99 * time.Hour), time.Time{}, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Events(s, tc.from, tc.to, tc.kinds...)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d events, want %d", len(got), tc.want)
			}
			for i := 1; i < len(got); i++ {
				if got[i].At.Before(got[i-1].At) {
					t.Errorf("results not oldest-first at %d", i)
				}
			}
		})
	}
}

// TestEventsInvertedWindow: an inverted window is a caller bug, not an
// empty result.
func TestEventsInvertedWindow(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	mustAppend(t, m, ev(s, EventDeploy, 0))
	if _, err := m.Events(s, at(time.Hour), at(0)); err == nil {
		t.Error("Events accepted an inverted window")
	}
	if _, err := m.Decisions(s, at(time.Hour), at(0)); err == nil {
		t.Error("Decisions accepted an inverted window")
	}
	if _, err := m.Digests(s, at(time.Hour), at(0), TierRaw); err == nil {
		t.Error("Digests accepted an inverted window")
	}
	if _, err := m.Timeline("c1", at(time.Hour), at(0)); err == nil {
		t.Error("Timeline accepted an inverted window")
	}
}

// TestEventsTieBreakIsArrivalOrder: equal timestamps must order by arrival,
// never by map or ring layout, so query output is reproducible.
func TestEventsTieBreakIsArrivalOrder(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	for i := 0; i < 20; i++ {
		e := ev(s, EventDeploy, 0)
		e.Attrs = map[string]string{"i": fmt.Sprintf("%02d", i)}
		mustAppend(t, m, e)
	}
	var want []string
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("%02d", i))
	}
	for run := 0; run < 10; run++ {
		got, _ := m.Events(s, time.Time{}, time.Time{})
		var order []string
		for _, e := range got {
			order = append(order, e.Attrs["i"])
		}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("run %d: order = %v, want %v", run, order, want)
		}
	}
}

// TestEventsReturnsDefensiveCopies: a caller mutating a result must not be
// able to corrupt stored state.
func TestEventsReturnsDefensiveCopies(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	e := ev(s, EventDeploy, 0)
	e.Attrs = map[string]string{"image": "v1"}
	mustAppend(t, m, e)

	got, _ := m.Events(s, time.Time{}, time.Time{})
	got[0].Attrs["image"] = "pwned"
	got[0].Kind = "pwned"

	again, _ := m.Events(s, time.Time{}, time.Time{})
	if again[0].Attrs["image"] != "v1" || again[0].Kind != EventDeploy {
		t.Fatalf("stored event was mutated through a query result: %+v", again[0])
	}
	// The append path must not alias the caller's map either.
	e.Attrs["image"] = "v2"
	third, _ := m.Events(s, time.Time{}, time.Time{})
	if third[0].Attrs["image"] != "v1" {
		t.Fatal("stored event aliases the caller's attrs map")
	}
}

// TestAppendPerSubjectCapAndBudget: both bounds hold, and both are counted.
func TestAppendPerSubjectCapAndBudget(t *testing.T) {
	m := newMem(t, Config{MaxEventsPerSubject: 4})
	s := subj("a")
	for i := 0; i < 20; i++ {
		mustAppend(t, m, ev(s, EventDeploy, time.Duration(i)*time.Minute))
	}
	got, _ := m.Events(s, time.Time{}, time.Time{})
	if len(got) != 4 {
		t.Fatalf("kept %d events, cap is 4", len(got))
	}
	if !got[0].At.Equal(at(16 * time.Minute)) {
		t.Errorf("oldest survivor is %v, want the 17th append", got[0].At)
	}
	if m.Stats().EvictedEventsCap != 16 {
		t.Errorf("EvictedEventsCap = %d, want 16", m.Stats().EvictedEventsCap)
	}
	checkMemory(t, m)
}

// TestRecordDecisionPayload covers the payload gate: valid JSON only, under
// the cap, never aliasing caller memory.
func TestRecordDecisionPayload(t *testing.T) {
	m := newMem(t, Config{MaxDecisionPayloadBytes: 64})
	s := subj("a")
	base := DecisionRecord{At: t0, Subject: s, Kind: DecisionRecommendation, Summary: "ok"}

	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"no payload", "", false},
		{"object", `{"cpu":100}`, false},
		{"array", `[1,2,3]`, false},
		{"bare string", `"hi"`, false},
		{"invalid json", `{cpu:100}`, true},
		{"truncated json", `{"cpu":`, true},
		{"over cap", `{"x":"` + strings.Repeat("y", 100) + `"}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			if tc.payload != "" {
				d.Payload = json.RawMessage(tc.payload)
			}
			err := m.RecordDecision(d)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}

	// No aliasing in either direction.
	d := base
	d.At = at(time.Hour)
	buf := []byte(`{"a":1}`)
	d.Payload = buf
	if err := m.RecordDecision(d); err != nil {
		t.Fatal(err)
	}
	buf[2] = 'z'
	got, _ := m.Decisions(s, at(time.Hour), time.Time{})
	if string(got[0].Payload) != `{"a":1}` {
		t.Errorf("stored payload aliased the caller's buffer: %s", got[0].Payload)
	}
	got[0].Payload[2] = 'q'
	again, _ := m.Decisions(s, at(time.Hour), time.Time{})
	if string(again[0].Payload) != `{"a":1}` {
		t.Errorf("query result aliased stored payload: %s", again[0].Payload)
	}
	checkMemory(t, m)
}

// TestRecordDecisionRejects covers the non-payload validation.
func TestRecordDecisionRejects(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	bad := []DecisionRecord{
		{Subject: s, Kind: DecisionAction},                       // zero time
		{At: t0, Kind: DecisionAction},                           // no subject
		{At: t0, Subject: s},                                     // no kind
		{At: t0, Subject: s, Kind: "\x00"},                       // kind cleans to empty
		{At: t0, Subject: SubjectRef{Kind: "k"}, Kind: "action"}, // subject without key
	}
	for i, d := range bad {
		if err := m.RecordDecision(d); err == nil {
			t.Errorf("case %d: RecordDecision accepted %+v", i, d)
		}
	}
}

// TestObserveSampleOutOfOrder: series are time-ordered by construction, so
// a late sample is refused loudly and counted.
func TestObserveSampleOutOfOrder(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	mustObserve(t, m, s, sample(time.Hour, 100, 1<<20))
	// Equal timestamps are allowed.
	mustObserve(t, m, s, sample(time.Hour, 110, 1<<20))
	err := m.ObserveSample(s, sample(time.Minute, 100, 1<<20))
	if err == nil {
		t.Fatal("ObserveSample accepted an older sample")
	}
	if !isOutOfOrder(err) {
		t.Errorf("err = %v, want ErrOutOfOrder", err)
	}
	if m.Stats().DroppedSamples != 1 {
		t.Errorf("DroppedSamples = %d, want 1", m.Stats().DroppedSamples)
	}
	checkMemory(t, m)
}

// TestObserveSampleRejectsGarbage: nothing invalid is ever stored, and a
// rejected sample must not leave a half-created subject behind.
func TestObserveSampleRejectsGarbage(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	if err := m.ObserveSample(s, Sample{MilliCPU: 1}); err == nil {
		t.Error("accepted a sample with no timestamp")
	}
	if err := m.ObserveSample(s, Sample{At: t0, MilliCPU: -5}); err == nil {
		t.Error("accepted a negative cpu sample")
	}
	if err := m.ObserveSample(SubjectRef{Kind: "container"}, sample(0, 1, 1)); err == nil {
		t.Error("accepted a subject with no key")
	}
	if len(m.series) != 0 {
		t.Errorf("rejected samples created %d series", len(m.series))
	}
	checkMemory(t, m)
}

// TestDigestsTiers walks the three query tiers on one populated series.
func TestDigestsTiers(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	for i := 0; i < 30*12; i++ {
		mustObserve(t, m, s, sample(time.Duration(i)*5*time.Minute, int64(100+i%9), 1<<30))
	}
	raw, err := m.Digests(s, time.Time{}, time.Time{}, TierRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("no raw digests")
	}
	for i, d := range raw {
		if d.Tier != TierRaw || d.Samples != 1 || !d.Start.Equal(d.End) {
			t.Fatalf("raw digest %d = %+v, want a point-in-time tier-0 digest", i, d)
		}
		if d.CPU.P50 != d.CPU.Max {
			t.Fatalf("raw digest %d has spread: %+v", i, d.CPU)
		}
	}
	hourly, _ := m.Digests(s, time.Time{}, time.Time{}, TierHourly)
	if len(hourly) == 0 {
		t.Fatal("no hourly digests")
	}
	for _, d := range hourly {
		if err := validateDigest(d, TierHourly); err != nil {
			t.Fatalf("hourly digest invalid: %v", err)
		}
	}
	if _, err := m.Digests(s, time.Time{}, time.Time{}, 7); err == nil {
		t.Error("Digests accepted an unknown tier")
	}
	if got, _ := m.Digests(subj("unknown"), time.Time{}, time.Time{}, TierHourly); got != nil {
		t.Error("unknown subject returned digests")
	}
	// Windowing on digests is overlap-based, not containment-based.
	d0 := hourly[0]
	mid := d0.Start.Add(d0.End.Sub(d0.Start) / 2)
	overlap, _ := m.Digests(s, mid, mid.Add(time.Minute), TierHourly)
	if len(overlap) != 1 {
		t.Errorf("a window inside a digest matched %d digests, want 1", len(overlap))
	}
	checkMemory(t, m)
}

// TestObservePointValidation covers the tier-3 ingest gate.
func TestObservePointValidation(t *testing.T) {
	m := newMem(t, Config{MaxTimelineClusters: 2})
	bad := []struct {
		name    string
		cluster string
		p       TimelinePoint
	}{
		{"no cluster", "", TimelinePoint{At: t0}},
		{"cluster is control chars", "\n\t", TimelinePoint{At: t0}},
		{"no timestamp", "c1", TimelinePoint{}},
		{"negative cost", "c1", TimelinePoint{At: t0, CostUSDPerHour: -1}},
		{"NaN cost", "c1", TimelinePoint{At: t0, CostUSDPerHour: nan()}},
		{"Inf cost", "c1", TimelinePoint{At: t0, CostUSDPerHour: inf()}},
		{"absurd cost", "c1", TimelinePoint{At: t0, CostUSDPerHour: 1e13}},
		{"negative nodes", "c1", TimelinePoint{At: t0, Nodes: -1}},
		{"events set by caller", "c1", TimelinePoint{At: t0, Events: []EvidenceEvent{}}},
	}
	for _, tc := range bad {
		if err := m.ObservePoint(tc.cluster, tc.p); err == nil {
			t.Errorf("%s: ObservePoint accepted %+v", tc.name, tc.p)
		}
	}
	if err := m.ObservePoint("c1", TimelinePoint{At: t0, CostUSDPerHour: 1, Nodes: 3}); err != nil {
		t.Fatal(err)
	}
	if err := m.ObservePoint("c1", TimelinePoint{At: at(-time.Hour)}); err == nil {
		t.Error("ObservePoint accepted an older point")
	}
	if err := m.ObservePoint("c2", TimelinePoint{At: t0}); err != nil {
		t.Fatal(err)
	}
	if err := m.ObservePoint("c3", TimelinePoint{At: t0}); err == nil {
		t.Error("ObservePoint exceeded MaxTimelineClusters")
	}
	checkMemory(t, m)
}

// TestTimelineOverlay: events attach to the latest point at or before them,
// events before the first point attach to the first, and per-point overlay
// is capped keeping the most severe.
func TestTimelineOverlay(t *testing.T) {
	m := newMem(t, Config{MaxOverlayEventsPerPoint: 3})
	for i := 0; i < 4; i++ {
		if err := m.ObservePoint("c1", TimelinePoint{
			At: at(time.Duration(i) * time.Hour), CostUSDPerHour: float64(i), Nodes: i,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// An event before every point, and events across the buckets.
	mustAppend(t, m, ev(subj("a"), EventDeploy, -time.Minute))
	mustAppend(t, m, ev(subj("a"), EventDeploy, 30*time.Minute))
	mustAppend(t, m, ev(subj("b"), EventOOMKill, 90*time.Minute))
	// Another cluster's events must never leak in.
	mustAppend(t, m, EvidenceEvent{
		At: at(90 * time.Minute), Kind: EventDeploy,
		Subject: SubjectRef{Cluster: "other", Kind: SubjectNode, Key: "n"},
	})
	// Overflow the third bucket with mixed severities.
	for i := 0; i < 6; i++ {
		e := ev(subj("c"), EventEvicted, 2*time.Hour+time.Duration(i)*time.Minute)
		if i == 5 {
			e.Severity = SeverityCritical
		} else if i == 4 {
			e.Severity = SeverityWarning
		}
		mustAppend(t, m, e)
	}

	pts, err := m.Timeline("c1", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 4 {
		t.Fatalf("got %d points, want 4", len(pts))
	}
	if len(pts[0].Events) != 2 {
		t.Errorf("point 0 has %d events, want 2 (the pre-first event folds in)", len(pts[0].Events))
	}
	if len(pts[1].Events) != 1 {
		t.Errorf("point 1 has %d events, want 1", len(pts[1].Events))
	}
	if len(pts[2].Events) != 3 {
		t.Errorf("point 2 has %d events, want the cap of 3", len(pts[2].Events))
	}
	sev := map[string]bool{}
	for _, e := range pts[2].Events {
		sev[e.Severity] = true
		if e.Subject.Cluster != "c1" {
			t.Errorf("event from cluster %q leaked into c1's timeline", e.Subject.Cluster)
		}
	}
	if !sev[SeverityCritical] || !sev[SeverityWarning] {
		t.Errorf("overlay cap dropped the severe events: %v", sev)
	}
	for i, p := range pts {
		for j := 1; j < len(p.Events); j++ {
			if p.Events[j].At.Before(p.Events[j-1].At) {
				t.Errorf("point %d events not oldest-first at %d", i, j)
			}
		}
	}
	// Determinism across repeated calls (map iteration must not leak).
	for run := 0; run < 10; run++ {
		again, _ := m.Timeline("c1", time.Time{}, time.Time{})
		if !reflect.DeepEqual(pts, again) {
			t.Fatalf("run %d produced a different timeline", run)
		}
	}
	if got, _ := m.Timeline("nope", time.Time{}, time.Time{}); got != nil {
		t.Error("unknown cluster returned points")
	}
	checkMemory(t, m)
}

// TestSubjectsSorted: the union of all three stores in one documented order.
func TestSubjectsSorted(t *testing.T) {
	m := newMem(t, Config{})
	mustAppend(t, m, ev(SubjectRef{Cluster: "b", Kind: SubjectNode, Key: "n1"}, EventDeploy, 0))
	mustObserve(t, m, SubjectRef{Cluster: "a", Kind: SubjectContainer, Key: "c1"}, sample(0, 1, 1))
	if err := m.RecordDecision(DecisionRecord{
		At: t0, Subject: SubjectRef{Cluster: "a", Kind: SubjectWorkload, Key: "w1"}, Kind: DecisionRefusal,
	}); err != nil {
		t.Fatal(err)
	}
	want := []SubjectRef{
		{Cluster: "a", Kind: SubjectContainer, Key: "c1"},
		{Cluster: "a", Kind: SubjectWorkload, Key: "w1"},
		{Cluster: "b", Kind: SubjectNode, Key: "n1"},
	}
	for run := 0; run < 10; run++ {
		if got := m.Subjects(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: Subjects() = %v, want %v", run, got, want)
		}
	}
}

// TestPruneRetention drives every store past its retention horizon with an
// injected clock and asserts what survives.
func TestPruneRetention(t *testing.T) {
	const day = 24 * time.Hour
	m := newMem(t, Config{
		EventMaxAge:    2 * time.Hour,
		DecisionMaxAge: 3 * time.Hour,
		RawMaxAge:      90 * time.Minute,
		HourlyMaxAge:   4 * time.Hour,
		DailyMaxAge:    10 * day,
		TimelineMaxAge: 2 * time.Hour,
	})
	s := subj("a")
	for i := 0; i < 6; i++ {
		d := time.Duration(i) * time.Hour
		mustAppend(t, m, ev(s, EventDeploy, d))
		if err := m.RecordDecision(DecisionRecord{
			At: at(d), Subject: s, Kind: DecisionRecommendation, Summary: "s",
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.ObservePoint("c1", TimelinePoint{At: at(d)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6*12; i++ {
		mustObserve(t, m, s, sample(time.Duration(i)*5*time.Minute, 100, 1<<30))
	}

	if _, err := m.Prune(time.Time{}); err == nil {
		t.Error("Prune accepted a zero clock")
	}
	now := at(6 * time.Hour)
	removed, err := m.Prune(now)
	if err != nil {
		t.Fatal(err)
	}
	if removed == 0 {
		t.Fatal("Prune removed nothing")
	}
	checkMemory(t, m)

	evs, _ := m.Events(s, time.Time{}, time.Time{})
	for _, e := range evs {
		if e.At.Before(now.Add(-2 * time.Hour)) {
			t.Errorf("event at %v survived a 2h retention", e.At)
		}
	}
	if len(evs) != 2 {
		t.Errorf("kept %d events, want 2 (4h and 5h marks)", len(evs))
	}
	decs, _ := m.Decisions(s, time.Time{}, time.Time{})
	if len(decs) != 3 {
		t.Errorf("kept %d decisions, want 3", len(decs))
	}
	raw, _ := m.Digests(s, time.Time{}, time.Time{}, TierRaw)
	for _, d := range raw {
		if d.Start.Before(now.Add(-90 * time.Minute)) {
			t.Errorf("raw sample at %v survived a 90m retention", d.Start)
		}
	}
	pts, _ := m.Timeline("c1", time.Time{}, time.Time{})
	for _, p := range pts {
		if p.At.Before(now.Add(-2 * time.Hour)) {
			t.Errorf("timeline point at %v survived a 2h retention", p.At)
		}
	}
	st := m.Stats()
	if st.PrunedEvents == 0 || st.PrunedDecisions == 0 || st.PrunedSeriesItems == 0 || st.PrunedTimeline == 0 {
		t.Errorf("prune counters incomplete: %+v", st)
	}
}

// TestPruneEmptiesEverything: pruning far past every horizon must leave a
// genuinely empty store — no orphan subjects, no leaked bytes.
func TestPruneEmptiesEverything(t *testing.T) {
	m := newMem(t, Config{})
	for i := 0; i < 50; i++ {
		s := subj(fmt.Sprintf("c%02d", i))
		mustAppend(t, m, ev(s, EventDeploy, time.Duration(i)*time.Minute))
		mustObserve(t, m, s, sample(time.Duration(i)*time.Minute, 100, 1<<20))
		if err := m.RecordDecision(DecisionRecord{At: at(0), Subject: s, Kind: DecisionAction}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.ObservePoint("c1", TimelinePoint{At: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Prune(t0.Add(1000 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	checkMemory(t, m)
	st := m.Stats()
	if st.Events != 0 || st.Decisions != 0 || st.RawSamples != 0 ||
		st.SeriesSubjects != 0 || st.TimelineClusters != 0 {
		t.Fatalf("store not empty after total prune: %+v", st)
	}
	if st.EventBytes != 0 || st.DecisionBytes != 0 || st.SeriesBytes != 0 {
		t.Fatalf("bytes leaked after total prune: events=%d decisions=%d series=%d",
			st.EventBytes, st.DecisionBytes, st.SeriesBytes)
	}
	if len(m.Subjects()) != 0 {
		t.Fatalf("Subjects() = %v after total prune", m.Subjects())
	}
	// The store must still be usable afterwards.
	mustAppend(t, m, ev(subj("fresh"), EventDeploy, 0))
	checkMemory(t, m)
}

// TestConcurrentUse is the race-detector target for the documented
// "safe for concurrent use" claim on Memory.
func TestConcurrentUse(t *testing.T) {
	m := newMem(t, Config{MaxEventsPerSubject: 32, RawSampleCap: 64})
	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s := subj(fmt.Sprintf("c%d", w%3))
			for i := 0; i < 200; i++ {
				d := time.Duration(i) * time.Minute
				_ = m.Append(ev(s, EventDeploy, d))
				_ = m.ObserveSample(s, sample(d, int64(i), 1<<20))
				_ = m.RecordDecision(DecisionRecord{At: at(d), Subject: s, Kind: DecisionAction})
				_ = m.ObservePoint("c1", TimelinePoint{At: at(d)})
				_, _ = m.Events(s, time.Time{}, time.Time{})
				_, _ = m.Digests(s, time.Time{}, time.Time{}, TierHourly)
				_, _ = m.Timeline("c1", time.Time{}, time.Time{})
				_ = m.Subjects()
				_ = m.Stats()
				if i%50 == 0 {
					_, _ = m.Prune(at(d))
				}
			}
		}(w)
	}
	wg.Wait()
	checkMemory(t, m)
}

func isOutOfOrder(err error) bool {
	for err != nil {
		if err == ErrOutOfOrder {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestStatsGauges: every gauge must reflect what the store actually holds,
// recomputed on call. Budgets that are reported wrong are budgets nobody
// can act on.
func TestStatsGauges(t *testing.T) {
	m := newMem(t, Config{})
	if got := m.Stats(); got != (Stats{}) {
		t.Fatalf("empty store stats = %+v, want the zero value", got)
	}
	a, b := subj("a"), subj("b")
	for i := 0; i < 3; i++ {
		mustAppend(t, m, ev(a, EventDeploy, time.Duration(i)*time.Hour))
	}
	mustAppend(t, m, ev(b, EventOOMKill, 0))
	for i := 0; i < 24*12; i++ {
		mustObserve(t, m, a, sample(time.Duration(i)*5*time.Minute, 100, 1<<30))
	}
	if err := m.RecordDecision(DecisionRecord{At: t0, Subject: a, Kind: DecisionAction}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := m.ObservePoint("c1", TimelinePoint{At: at(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	st := m.Stats()
	if st.EventSubjects != 2 || st.Events != 4 {
		t.Errorf("events: %d subjects / %d events, want 2 / 4", st.EventSubjects, st.Events)
	}
	if st.SeriesSubjects != 1 || st.RawSamples == 0 || st.HourlyDigests == 0 {
		t.Errorf("series gauges = %+v", st)
	}
	if st.Decisions != 1 {
		t.Errorf("Decisions = %d, want 1", st.Decisions)
	}
	if st.TimelineClusters != 1 || st.TimelinePoints != 5 {
		t.Errorf("timeline gauges = %d clusters / %d points", st.TimelineClusters, st.TimelinePoints)
	}
	if st.TimelineBytes != 5*pointBytes+subjectOverheadBytes {
		t.Errorf("TimelineBytes = %d", st.TimelineBytes)
	}
	if st.EventBytes != m.events.bytes || st.DecisionBytes != m.decisions.bytes ||
		st.SeriesBytes != m.seriesBytes {
		t.Error("reported byte gauges disagree with the stores")
	}
	// Gauges are recomputed, not accumulated: reading twice gives the same
	// answer, and a prune moves them down.
	if again := m.Stats(); again != st {
		t.Error("Stats is not idempotent")
	}
	if _, err := m.Prune(at(10000 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	after := m.Stats()
	if after.Events != 0 || after.RawSamples != 0 || after.TimelinePoints != 0 {
		t.Errorf("gauges did not fall after a total prune: %+v", after)
	}
	if after.PrunedEvents == 0 {
		t.Error("lifetime counters were reset by Prune")
	}
}
