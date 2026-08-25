package evidence

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func dossierFixture(t *testing.T) (*Memory, SubjectRef) {
	t.Helper()
	m := newMem(t, Config{})
	s := subj("Deployment/prod/checkout/main")
	for i := 0; i < 40*12; i++ {
		smp := sample(time.Duration(i)*5*time.Minute, int64(200+i%60), int64(1<<30)+int64(i%13)<<20)
		smp.ThrottleRatio = float64(i%5) / 10
		if i%211 == 0 {
			smp.Restarts, smp.OOMs = 1, 1
		}
		mustObserve(t, m, s, smp)
	}
	for i := 0; i < 60; i++ {
		e := ev(s, []string{EventDeploy, EventOOMKill, EventHPAScale, EventThrottleHigh}[i%4],
			time.Duration(i)*time.Hour)
		e.Attrs = map[string]string{"image": fmt.Sprintf("checkout:v%d", i)}
		mustAppend(t, m, e)
	}
	for i := 0; i < 30; i++ {
		if err := m.RecordDecision(DecisionRecord{
			At: at(time.Duration(i) * time.Hour), Subject: s, Kind: DecisionRecommendation,
			Summary: fmt.Sprintf("cpu 250m -> %dm", 240-i),
			Payload: json.RawMessage(fmt.Sprintf(`{"cpu":%d}`, 240-i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return m, s
}

// TestDossierSizeCap is the §3.4 contract: whatever the store holds, the
// dossier encodes within the requested cap. This is what makes it safe to
// hand to an LLM context window or an API response without a second guess.
func TestDossierSizeCap(t *testing.T) {
	m, s := dossierFixture(t)
	for _, max := range []int{minDossierBytes, 300, 512, 1024, DefaultDossierBytes, 8192, 65536} {
		t.Run(fmt.Sprint(max), func(t *testing.T) {
			d, err := m.Dossier(DossierRequest{Subject: s, MaxBytes: max})
			if err != nil {
				// The contract is fit-or-fail: a cap below the irreducible
				// skeleton is refused, never silently under-served.
				if max >= DefaultDossierBytes {
					t.Fatalf("MaxBytes=%d: %v", max, err)
				}
				t.Logf("MaxBytes=%d refused: %v", max, err)
				return
			}
			data, err := d.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > max {
				t.Fatalf("dossier encoded to %d bytes, cap %d", len(data), max)
			}
			// The skeleton always survives.
			if d.Subject != s {
				t.Errorf("subject lost: %v", d.Subject)
			}
			if d.Usage.Tier == TierNone {
				t.Error("usage summary lost to the size cap")
			}
			if len(data) < max/4 && d.Truncated != nil && d.Truncated.Reason == "byte-cap" {
				t.Errorf("dossier shrank to %d for a %d cap: over-eager", len(data), max)
			}
		})
	}
}

// TestDossierTruncationIsReported: nothing is dropped silently.
func TestDossierTruncationIsReported(t *testing.T) {
	m, s := dossierFixture(t)
	d, err := m.Dossier(DossierRequest{Subject: s, MaxBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if d.Truncated == nil {
		t.Fatal("a 512-byte dossier over a full fixture reported no truncation")
	}
	if d.Truncated.Reason == "" {
		t.Error("truncation carries no reason")
	}
	total := d.Truncated.Events + d.Truncated.Decisions + d.Truncated.Digests
	if total == 0 {
		t.Error("truncation reported nothing dropped")
	}

	// A generous cap over a small store must report nothing.
	m2 := newMem(t, Config{})
	small := subj("tiny")
	mustAppend(t, m2, ev(small, EventDeploy, 0))
	d2, err := m2.Dossier(DossierRequest{Subject: small, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	if d2.Truncated != nil {
		t.Errorf("a one-event dossier reported truncation: %+v", d2.Truncated)
	}
}

// TestDossierDeterministic: same store, same request, byte-identical result.
func TestDossierDeterministic(t *testing.T) {
	m, s := dossierFixture(t)
	req := DossierRequest{
		Subject: s, MaxBytes: 2048,
		Attachments: []Attachment{
			{Name: "sizing", Data: json.RawMessage(`{"cpu":180,"mem":1073741824}`)},
			{Name: "class", Data: json.RawMessage(`"diurnal"`)},
		},
	}
	first, err := m.Dossier(req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := first.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		got, err := m.Dossier(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := got.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(want) {
			t.Fatalf("build %d differs from build 0", i)
		}
	}
}

// TestDossierOrderingAndWindow pins the documented orders and the window.
func TestDossierOrderingAndWindow(t *testing.T) {
	m, s := dossierFixture(t)
	d, err := m.Dossier(DossierRequest{
		Subject: s, From: at(10 * time.Hour), To: at(20 * time.Hour), MaxBytes: 65536,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) == 0 {
		t.Fatal("no events in the window")
	}
	for i := 1; i < len(d.Events); i++ {
		if d.Events[i].At.After(d.Events[i-1].At) {
			t.Errorf("events are not newest-first at %d", i)
		}
	}
	for _, e := range d.Events {
		if e.At.Before(at(10*time.Hour)) || !e.At.Before(at(20*time.Hour)) {
			t.Errorf("event at %v is outside the requested window", e.At)
		}
	}
	for i := 1; i < len(d.Decisions); i++ {
		if d.Decisions[i].At.After(d.Decisions[i-1].At) {
			t.Errorf("decisions are not newest-first at %d", i)
		}
	}
	for i := 1; i < len(d.Digests); i++ {
		if d.Digests[i].Start.Before(d.Digests[i-1].Start) {
			t.Errorf("digests are not oldest-first at %d", i)
		}
	}
	if !d.From.Equal(at(10*time.Hour)) || !d.To.Equal(at(20*time.Hour)) {
		t.Errorf("window echoed as [%v, %v)", d.From, d.To)
	}
	if !d.Usage.From.Equal(d.From) || !d.Usage.To.Equal(d.To) {
		t.Error("usage summary window disagrees with the dossier window")
	}
}

// TestDossierUsageSummary: the summary is exact on raw samples, and the
// digest tiers are conservative — never below the exact answer.
func TestDossierUsageSummary(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	var cpus []int64
	for i := 0; i < 24; i++ {
		v := int64(100 + i*7)
		cpus = append(cpus, v)
		smp := sample(time.Duration(i)*5*time.Minute, v, 1<<30)
		smp.ThrottleRatio = 0.25
		mustObserve(t, m, s, smp)
	}
	d, err := m.Dossier(DossierRequest{Subject: s, DigestTier: TierRaw, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.Tier != TierRaw {
		t.Fatalf("usage tier = %d, want raw", d.Usage.Tier)
	}
	if d.Usage.Samples != 24 {
		t.Errorf("Samples = %d, want 24", d.Usage.Samples)
	}
	want := statsFromValues(cpus)
	if d.Usage.CPU.Max != want.Max {
		t.Errorf("CPU.Max = %v, want %v", d.Usage.CPU.Max, want.Max)
	}
	if !d.Usage.CPU.ordered() {
		t.Errorf("usage CPU stats unordered: %+v", d.Usage.CPU)
	}
	if diff := d.Usage.ThrottleRatio - 0.25; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("ThrottleRatio = %v, want 0.25", d.Usage.ThrottleRatio)
	}

	// Fold enough hours that the hourly tier exists, then check the
	// conservative direction.
	for i := 24; i < 24*12*3; i++ {
		mustObserve(t, m, s, sample(time.Duration(i)*5*time.Minute, int64(100+i%50), 1<<30))
	}
	rawD, err := m.Dossier(DossierRequest{Subject: s, DigestTier: TierRaw, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	hourD, err := m.Dossier(DossierRequest{Subject: s, DigestTier: TierHourly, MaxBytes: 65536})
	if err != nil {
		t.Fatal(err)
	}
	if hourD.Usage.Tier != TierHourly {
		t.Fatalf("usage tier = %d, want hourly", hourD.Usage.Tier)
	}
	if hourD.Usage.CPU.Max < rawD.Usage.CPU.Max {
		t.Errorf("hourly summary Max %v under-estimates the raw window's %v",
			hourD.Usage.CPU.Max, rawD.Usage.CPU.Max)
	}
}

// TestDossierUsageFallsBackTiers: an empty requested tier must not leave the
// dossier usage-blind if a coarser tier covers the window.
func TestDossierUsageFallsBackTiers(t *testing.T) {
	m := newMem(t, Config{RawSampleCap: 12})
	s := subj("a")
	for i := 0; i < 24*12; i++ {
		mustObserve(t, m, s, sample(time.Duration(i)*5*time.Minute, 150, 1<<30))
	}
	// A window entirely before the surviving raw ring: raw is empty there,
	// hourly is not.
	d, err := m.Dossier(DossierRequest{
		Subject: s, From: t0, To: at(2 * time.Hour), DigestTier: TierRaw, MaxBytes: 65536,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Digests) != 0 {
		t.Errorf("raw tier returned %d digests for a window it does not cover", len(d.Digests))
	}
	if d.Usage.Tier != TierHourly {
		t.Errorf("usage tier = %d, want a fallback to hourly", d.Usage.Tier)
	}
	if d.Usage.Samples == 0 {
		t.Error("fallback summary has no samples")
	}
}

// TestDossierNoData: an unknown subject yields a valid, honest, empty case
// file rather than an error or a misleading zero-usage claim.
func TestDossierNoData(t *testing.T) {
	m := newMem(t, Config{})
	d, err := m.Dossier(DossierRequest{Subject: subj("ghost")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.Tier != TierNone {
		t.Errorf("usage tier = %d, want TierNone", d.Usage.Tier)
	}
	if d.Usage.Samples != 0 || d.Usage.Windows != 0 {
		t.Errorf("empty dossier claims data: %+v", d.Usage)
	}
	if len(d.Events) != 0 || len(d.Decisions) != 0 || len(d.Digests) != 0 {
		t.Error("empty dossier carries content")
	}
	if d.Truncated != nil {
		t.Error("empty dossier reports truncation")
	}
	data, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > DefaultDossierBytes {
		t.Errorf("empty dossier is %d bytes", len(data))
	}
}

// TestDossierAttachments: higher-layer sections pass through verbatim and
// are the last thing dropped under pressure.
func TestDossierAttachments(t *testing.T) {
	m, s := dossierFixture(t)
	atts := []Attachment{
		{Name: "sizing", Data: json.RawMessage(`{"cpu":180}`)},
		{Name: "refusal", Data: json.RawMessage(`{"reason":"hpa-thrash"}`)},
	}
	d, err := m.Dossier(DossierRequest{Subject: s, MaxBytes: 1024, Attachments: atts})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Attachments, atts) {
		t.Fatalf("attachments = %+v, want %+v", d.Attachments, atts)
	}
	if d.Truncated == nil || d.Truncated.Attachments != 0 {
		t.Errorf("attachments were dropped before history: %+v", d.Truncated)
	}
	// The result must not alias the caller's slice.
	d.Attachments[0].Name = "mutated"
	if atts[0].Name != "sizing" {
		t.Error("dossier aliases the caller's attachment slice")
	}
}

// TestDossierRequestValidation: bad requests fail loudly.
func TestDossierRequestValidation(t *testing.T) {
	m, s := dossierFixture(t)
	tests := []struct {
		name string
		req  DossierRequest
	}{
		{"no subject", DossierRequest{}},
		{"subject without key", DossierRequest{Subject: SubjectRef{Kind: "container"}}},
		{"byte cap too small", DossierRequest{Subject: s, MaxBytes: 8}},
		{"byte cap too large", DossierRequest{Subject: s, MaxBytes: maxDossierBytes + 1}},
		{"too many events", DossierRequest{Subject: s, MaxEvents: maxDossierItems + 1}},
		{"too many decisions", DossierRequest{Subject: s, MaxDecisions: maxDossierItems + 1}},
		{"too many digests", DossierRequest{Subject: s, MaxDigests: maxDossierItems + 1}},
		{"unknown tier", DossierRequest{Subject: s, DigestTier: 9}},
		{"inverted window", DossierRequest{Subject: s, From: at(time.Hour), To: t0}},
		{"attachment without a name", DossierRequest{Subject: s,
			Attachments: []Attachment{{Data: json.RawMessage(`1`)}}}},
		{"attachment without data", DossierRequest{Subject: s,
			Attachments: []Attachment{{Name: "x"}}}},
		{"attachment with invalid json", DossierRequest{Subject: s,
			Attachments: []Attachment{{Name: "x", Data: json.RawMessage(`{oops`)}}}},
		{"attachment not compact", DossierRequest{Subject: s,
			Attachments: []Attachment{{Name: "x", Data: json.RawMessage(`{"a": 1}`)}}}},
		{"attachment name with control chars", DossierRequest{Subject: s,
			Attachments: []Attachment{{Name: "a\x00b", Data: json.RawMessage(`1`)}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Dossier(tc.req); err == nil {
				t.Fatal("Dossier accepted an invalid request")
			}
		})
	}
	if _, err := BuildDossier(nil, DossierRequest{Subject: s}); err == nil {
		t.Error("BuildDossier accepted a nil store")
	}
}

// TestDossierImpossibleCapFails: an attachment too big for the cap must be
// an error, never a silently gutted dossier.
func TestDossierImpossibleCapFails(t *testing.T) {
	m, s := dossierFixture(t)
	big := json.RawMessage(`{"blob":"` + strings.Repeat("x", 4000) + `"}`)
	// Under a cap it cannot fit, the attachment is dropped down the ladder
	// and the drop is reported — the dossier never exceeds its cap.
	small, err := m.Dossier(DossierRequest{
		Subject: s, MaxBytes: 1024, Attachments: []Attachment{{Name: "huge", Data: big}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(small.Attachments) != 0 || small.Truncated == nil || small.Truncated.Attachments != 1 {
		t.Fatalf("oversized attachment not dropped and reported: %+v", small.Truncated)
	}
	if data, _ := small.Encode(); len(data) > 1024 {
		t.Fatalf("dossier is %d bytes for a 1024 cap", len(data))
	}
	// A cap below the irreducible skeleton is an error, not a fragment.
	if _, err := m.Dossier(DossierRequest{Subject: s, MaxBytes: minDossierBytes}); err == nil {
		t.Fatal("Dossier returned a result under a cap smaller than its skeleton")
	}
	// The same attachment under a cap that fits must succeed.
	d, err := m.Dossier(DossierRequest{
		Subject: s, MaxBytes: 8192, Attachments: []Attachment{{Name: "huge", Data: big}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Attachments) != 1 {
		t.Error("the attachment was dropped despite fitting")
	}
}

// TestDossierCountCaps: the count caps bind independently of the byte cap.
func TestDossierCountCaps(t *testing.T) {
	m, s := dossierFixture(t)
	d, err := m.Dossier(DossierRequest{
		Subject: s, MaxBytes: 65536, MaxEvents: 3, MaxDecisions: 2, MaxDigests: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 3 || len(d.Decisions) != 2 || len(d.Digests) != 4 {
		t.Fatalf("counts = (%d, %d, %d), want (3, 2, 4)",
			len(d.Events), len(d.Decisions), len(d.Digests))
	}
	if d.Truncated == nil || d.Truncated.Reason != "count-cap" {
		t.Errorf("count truncation not reported: %+v", d.Truncated)
	}
	// The kept items must be the newest events/decisions and newest digests.
	all, _ := m.Events(s, time.Time{}, time.Time{})
	if !d.Events[0].At.Equal(all[len(all)-1].At) {
		t.Error("the event cap kept the wrong end of the history")
	}
	// MaxDigests < 0 suppresses digests entirely.
	none, err := m.Dossier(DossierRequest{Subject: s, MaxBytes: 65536, MaxDigests: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Digests) != 0 {
		t.Errorf("MaxDigests<0 returned %d digests", len(none.Digests))
	}
	if none.Usage.Tier == TierNone {
		t.Error("suppressing digests also suppressed the usage summary")
	}
}

// TestDossierBuildsOverStoreInterface: the builder must work against any
// Store, since pkg/store provides a different implementation later.
func TestDossierBuildsOverStoreInterface(t *testing.T) {
	m, s := dossierFixture(t)
	var st Store = m
	d, err := BuildDossier(st, DossierRequest{Subject: s, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := m.Dossier(DossierRequest{Subject: s, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.Encode()
	b, _ := direct.Encode()
	if string(a) != string(b) {
		t.Error("BuildDossier over the interface differs from the method")
	}
}

// FuzzDossierSizeCap: for any cap and any caps-on-counts, a successfully
// built dossier must honour its byte cap and report every drop.
func FuzzDossierSizeCap(f *testing.F) {
	f.Add(4096, 24, 8, 24, 1)
	f.Add(256, 0, 0, 0, 0)
	f.Add(1<<20, 256, 256, 256, 2)
	m, err := NewMemory(Config{})
	if err != nil {
		f.Fatal(err)
	}
	s := subj("Deployment/prod/checkout/main")
	for i := 0; i < 600; i++ {
		_ = m.ObserveSample(s, Sample{
			At: t0.Add(time.Duration(i) * 5 * time.Minute), MilliCPU: int64(200 + i%60),
			MemoryBytes: int64(1<<30) + int64(i%13)<<20,
		})
	}
	for i := 0; i < 60; i++ {
		e := EvidenceEvent{At: t0.Add(time.Duration(i) * time.Hour), Kind: EventDeploy, Subject: s}
		e.Attrs = map[string]string{"image": fmt.Sprintf("checkout:v%d", i)}
		_ = m.Append(e)
		_ = m.RecordDecision(DecisionRecord{
			At: t0.Add(time.Duration(i) * time.Hour), Subject: s, Kind: DecisionRecommendation,
			Summary: "resize", Payload: json.RawMessage(`{"cpu":180}`),
		})
	}
	f.Fuzz(func(t *testing.T, maxBytes, maxEvents, maxDecisions, maxDigests, tier int) {
		req := DossierRequest{
			Subject: s, MaxBytes: maxBytes, MaxEvents: maxEvents,
			MaxDecisions: maxDecisions, MaxDigests: maxDigests, DigestTier: tier,
		}
		d, err := BuildDossier(m, req)
		if err != nil {
			return // invalid requests are rejected, which is the contract
		}
		data, err := d.Encode()
		if err != nil {
			t.Fatalf("encoding a built dossier failed: %v", err)
		}
		eff := req.withDefaults()
		if len(data) > eff.MaxBytes {
			t.Fatalf("dossier is %d bytes for a %d cap", len(data), eff.MaxBytes)
		}
		if len(d.Events) > eff.MaxEvents || len(d.Decisions) > eff.MaxDecisions {
			t.Fatalf("count caps violated: %d events, %d decisions", len(d.Events), len(d.Decisions))
		}
		if eff.MaxDigests > 0 && len(d.Digests) > eff.MaxDigests {
			t.Fatalf("digest cap violated: %d", len(d.Digests))
		}
		if d.Subject != s {
			t.Fatalf("subject mangled: %v", d.Subject)
		}
		for i := 1; i < len(d.Events); i++ {
			if d.Events[i].At.After(d.Events[i-1].At) {
				t.Fatal("events not newest-first")
			}
		}
		if d.Usage.Samples < 0 || d.Usage.ThrottleRatio < 0 || d.Usage.ThrottleRatio > 1 {
			t.Fatalf("usage summary out of range: %+v", d.Usage)
		}
		if d.Usage.Tier != TierNone && !d.Usage.CPU.ordered() {
			t.Fatalf("usage stats unordered: %+v", d.Usage.CPU)
		}
	})
}
