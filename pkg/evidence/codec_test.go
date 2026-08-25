package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// memStore is an in-memory CheckpointStore standing in for the bbolt-backed
// one pkg/store will provide.
type memStore struct {
	data []byte
	err  error
}

func (s *memStore) SaveCheckpoint(_ context.Context, d []byte) error {
	if s.err != nil {
		return s.err
	}
	s.data = append([]byte(nil), d...)
	return nil
}

func (s *memStore) LoadCheckpoint(_ context.Context) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.data, nil
}

var _ CheckpointStore = (*memStore)(nil)

// populate builds a substrate exercising every store and every tier.
func populate(t *testing.T, cfg Config) *Memory {
	t.Helper()
	m := newMem(t, cfg)
	for i := 0; i < 3; i++ {
		s := subj(fmt.Sprintf("Deployment/ns/app-%d/main", i))
		for h := 0; h < 30*12; h++ {
			mustObserve(t, m, s, Sample{
				At:            at(time.Duration(h) * 5 * time.Minute),
				MilliCPU:      int64(100 + (h*i)%37),
				MemoryBytes:   int64(1<<30) + int64(h%11)<<20,
				ThrottleRatio: float64(h%7) / 7,
				Restarts:      int64(boolToInt(h%97 == 0)),
			})
		}
		for k := 0; k < 5; k++ {
			e := ev(s, EventDeploy, time.Duration(k)*time.Hour)
			e.Attrs = map[string]string{"image": fmt.Sprintf("app:v%d", k), "gen": fmt.Sprint(k)}
			e.Severity = []string{SeverityInfo, SeverityWarning, SeverityCritical}[k%3]
			mustAppend(t, m, e)
		}
		warn := ev(s, EventProbeFailure, time.Hour)
		warn.Dedup = "probe/readiness"
		mustAppend(t, m, warn)
		warn.At = at(time.Hour + 10*time.Minute)
		warn.Severity = SeverityCritical
		mustAppend(t, m, warn)

		for k := 0; k < 3; k++ {
			if err := m.RecordDecision(DecisionRecord{
				At: at(time.Duration(k) * time.Hour), Subject: s, Kind: DecisionRecommendation,
				Summary: "cpu 250m -> 180m", Fingerprint: fmt.Sprintf("fp-%d-%d", i, k),
				Payload: json.RawMessage(fmt.Sprintf(`{"cpu":%d,"mem":1073741824}`, 180+k)),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Subjects of other kinds, and a fleet-level subject with no cluster.
	mustAppend(t, m, ev(NodeSubject("c1", "ip-10-0-0-1"), EventNodePressure, time.Hour))
	mustAppend(t, m, ev(SubjectRef{Kind: SubjectCluster, Key: "fleet"}, EventPricingChange, time.Hour))
	for i := 0; i < 20; i++ {
		if err := m.ObservePoint("c1", TimelinePoint{
			At: at(time.Duration(i) * time.Hour), CostUSDPerHour: 1.5 + float64(i)/8, Nodes: 10 + i,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.ObservePoint("c2", TimelinePoint{At: t0, CostUSDPerHour: 0.25, Nodes: 2}); err != nil {
		t.Fatal(err)
	}
	return m
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestCheckpointRoundTripExact is the codec contract: encode, decode,
// re-encode must be byte-identical, and the restored store must answer
// every query exactly as the original did.
func TestCheckpointRoundTripExact(t *testing.T) {
	m := populate(t, Config{})
	first, err := m.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	cp, err := UnmarshalCheckpoint(first)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := FromCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restored.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("checkpoint is not byte-exact across a round trip\nlen %d vs %d", len(first), len(second))
	}
	assertSameObservable(t, m, restored)
	checkMemory(t, restored)
	t.Logf("checkpoint of %d subjects: %d bytes", len(cp.Subjects), len(first))
}

// TestCheckpointDeterministic: encoding the same substrate repeatedly must
// produce identical bytes — no map iteration order may reach the wire.
func TestCheckpointDeterministic(t *testing.T) {
	m := populate(t, Config{})
	want, err := m.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := m.MarshalCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("encoding %d differs from encoding 0", i)
		}
	}
	// Two independently built but identically fed stores must agree too.
	other := populate(t, Config{})
	got, err := other.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("two identically-fed substrates encode differently")
	}
}

// assertSameObservable compares two stores through the public query surface.
func assertSameObservable(t *testing.T, a, b *Memory) {
	t.Helper()
	refsA, refsB := a.Subjects(), b.Subjects()
	if !reflect.DeepEqual(refsA, refsB) {
		t.Fatalf("Subjects differ:\n a=%v\n b=%v", refsA, refsB)
	}
	for _, ref := range refsA {
		evA, _ := a.Events(ref, time.Time{}, time.Time{})
		evB, _ := b.Events(ref, time.Time{}, time.Time{})
		if !reflect.DeepEqual(evA, evB) {
			t.Errorf("Events(%v) differ:\n a=%+v\n b=%+v", ref, evA, evB)
		}
		dA, _ := a.Decisions(ref, time.Time{}, time.Time{})
		dB, _ := b.Decisions(ref, time.Time{}, time.Time{})
		if !reflect.DeepEqual(dA, dB) {
			t.Errorf("Decisions(%v) differ", ref)
		}
		for _, tier := range []int{TierRaw, TierHourly, TierDaily} {
			gA, _ := a.Digests(ref, time.Time{}, time.Time{}, tier)
			gB, _ := b.Digests(ref, time.Time{}, time.Time{}, tier)
			if !reflect.DeepEqual(gA, gB) {
				t.Errorf("Digests(%v, tier %d) differ", ref, tier)
			}
		}
	}
	for _, c := range []string{"c1", "c2", "nope"} {
		pA, _ := a.Timeline(c, time.Time{}, time.Time{})
		pB, _ := b.Timeline(c, time.Time{}, time.Time{})
		if !reflect.DeepEqual(pA, pB) {
			t.Errorf("Timeline(%q) differ", c)
		}
	}
	if a.Config() != b.Config() {
		t.Error("configs differ")
	}
}

// TestRestoredStoreKeepsIngesting: a restored substrate must accept new
// writes without out-of-order errors or sequence collisions.
func TestRestoredStoreKeepsIngesting(t *testing.T) {
	m := populate(t, Config{})
	restored, err := FromCheckpoint(m.Checkpoint())
	if err != nil {
		t.Fatal(err)
	}
	s := subj("Deployment/ns/app-0/main")
	// Continuing the series from where it left off must be accepted.
	for i := 0; i < 24; i++ {
		mustObserve(t, restored, s, sample(30*12*5*time.Minute+time.Duration(i)*5*time.Minute, 120, 1<<30))
	}
	mustAppend(t, restored, ev(s, EventOOMKill, 500*time.Hour))
	if err := restored.ObservePoint("c1", TimelinePoint{At: at(500 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	checkMemory(t, restored)

	// Sequences must continue past every restored one, so query tie-breaks
	// stay stable across the restart boundary.
	evs, _ := restored.Events(s, time.Time{}, time.Time{})
	if !evs[len(evs)-1].At.Equal(at(500 * time.Hour)) {
		t.Error("the post-restore event is not newest")
	}
}

// TestFromCheckpointRejects covers the validation gate: a corrupt or
// hostile checkpoint must be refused, never partially applied.
func TestFromCheckpointRejects(t *testing.T) {
	good := populate(t, Config{}).Checkpoint()
	clone := func() *Checkpoint {
		data, err := json.Marshal(good)
		if err != nil {
			t.Fatal(err)
		}
		cp, err := UnmarshalCheckpoint(data)
		if err != nil {
			t.Fatal(err)
		}
		return cp
	}
	// rich is the index of a subject carrying events, decisions and a full
	// series, so every mutation below has something to corrupt.
	rich := -1
	for i, st := range good.Subjects {
		if st.Series != nil && st.Series.Pending != nil && len(st.Events) > 1 &&
			len(st.Decisions) > 0 && len(st.Series.Hourly) > 1 && len(st.Series.Raw) > 1 {
			rich = i
			break
		}
	}
	if rich < 0 {
		t.Fatal("no subject in the fixture carries events, decisions and a full series")
	}
	tests := []struct {
		name string
		mut  func(*Checkpoint)
	}{
		{"wrong version", func(c *Checkpoint) { c.Version = 99 }},
		{"invalid config", func(c *Checkpoint) { c.Config.CoalesceTolerance = 5 }},
		{"subjects out of order", func(c *Checkpoint) {
			c.Subjects[0], c.Subjects[1] = c.Subjects[1], c.Subjects[0]
		}},
		{"duplicate subject", func(c *Checkpoint) { c.Subjects[1].Subject = c.Subjects[0].Subject }},
		{"subject without key", func(c *Checkpoint) { c.Subjects[rich].Subject.Key = "" }},
		{"event with control characters", func(c *Checkpoint) {
			c.Subjects[rich].Events[0].Event.Kind = "de\x00ploy"
		}},
		{"event with unknown severity", func(c *Checkpoint) {
			c.Subjects[rich].Events[0].Event.Severity = "catastrophic"
		}},
		{"event filed under the wrong subject", func(c *Checkpoint) {
			c.Subjects[rich].Events[0].Event.Subject.Key = "elsewhere"
		}},
		{"event with a non-UTC time", func(c *Checkpoint) {
			c.Subjects[rich].Events[0].Event.At = t0.In(time.FixedZone("KST", 9*3600))
		}},
		{"event sequences not increasing", func(c *Checkpoint) {
			c.Subjects[rich].Events[1].Seq = c.Subjects[rich].Events[0].Seq
		}},
		{"zero event sequence", func(c *Checkpoint) { c.Subjects[rich].Events[0].Seq = 0 }},
		{"too many events for the cap", func(c *Checkpoint) {
			c.Config.MaxEventsPerSubject = 1
		}},
		{"seq below stored sequences", func(c *Checkpoint) { c.Seq = 1 }},
		{"negative attr count over cap", func(c *Checkpoint) {
			attrs := map[string]string{}
			for i := 0; i < maxAttrs+1; i++ {
				attrs[fmt.Sprintf("k%d", i)] = "v"
			}
			c.Subjects[rich].Events[0].Event.Attrs = attrs
		}},
		{"oversized attr value", func(c *Checkpoint) {
			c.Subjects[rich].Events[0].Event.Attrs = map[string]string{"k": strings.Repeat("v", 999)}
		}},
		{"digest with the wrong tier", func(c *Checkpoint) {
			c.Subjects[rich].Series.Hourly[0].Tier = TierDaily
		}},
		{"digest with unordered stats", func(c *Checkpoint) {
			c.Subjects[rich].Series.Hourly[0].CPU.P50 = 1e9
		}},
		{"digest with an inverted window", func(c *Checkpoint) {
			d := &c.Subjects[rich].Series.Hourly[0]
			d.End = d.Start.Add(-time.Hour)
		}},
		{"overlapping digests", func(c *Checkpoint) {
			h := c.Subjects[rich].Series.Hourly
			h[1].Start = h[0].Start
		}},
		{"raw samples out of order", func(c *Checkpoint) {
			r := c.Subjects[rich].Series.Raw
			r[0], r[1] = r[1], r[0]
		}},
		{"negative sample", func(c *Checkpoint) { c.Subjects[rich].Series.Raw[0].MilliCPU = -1 }},
		{"pending hour not on an hour boundary", func(c *Checkpoint) {
			c.Subjects[rich].Series.Pending.Hour = c.Subjects[rich].Series.Pending.Hour.Add(time.Minute)
		}},
		{"pending hour arrays disagree", func(c *Checkpoint) {
			p := c.Subjects[rich].Series.Pending
			p.CPU = p.CPU[:len(p.CPU)-1]
		}},
		{"pending hour over the per-hour cap", func(c *Checkpoint) { c.Config.MaxSamplesPerHour = 1 }},
		{"raw ring over cap", func(c *Checkpoint) { c.Config.RawSampleCap = 2 }},
		{"timeline point with a stored overlay", func(c *Checkpoint) {
			c.Timelines[0].Points[0].Events = []EvidenceEvent{}
		}},
		{"timeline points out of order", func(c *Checkpoint) {
			p := c.Timelines[0].Points
			p[0], p[1] = p[1], p[0]
		}},
		{"timeline with NaN cost", func(c *Checkpoint) {
			c.Timelines[0].Points[0].CostUSDPerHour = 1e13
		}},
		{"timelines out of order", func(c *Checkpoint) {
			c.Timelines[0], c.Timelines[1] = c.Timelines[1], c.Timelines[0]
		}},
		{"too many timeline clusters", func(c *Checkpoint) { c.Config.MaxTimelineClusters = 1 }},
		{"empty timeline cluster name", func(c *Checkpoint) { c.Timelines[0].Cluster = "" }},
		{"decision with an invalid payload", func(c *Checkpoint) {
			c.Subjects[rich].Decisions[0].Decision.Payload = json.RawMessage(`{oops`)
		}},
		{"decision with a non-compact payload", func(c *Checkpoint) {
			c.Subjects[rich].Decisions[0].Decision.Payload = json.RawMessage(`{"a": 1}`)
		}},
		{"decision filed under the wrong subject", func(c *Checkpoint) {
			c.Subjects[rich].Decisions[0].Decision.Subject.Key = "elsewhere"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := clone()
			tc.mut(cp)
			if _, err := FromCheckpoint(cp); err == nil {
				t.Fatal("FromCheckpoint accepted a corrupt checkpoint")
			}
		})
	}
	// The unmutated clone must still restore, or the table proves nothing.
	if _, err := FromCheckpoint(clone()); err != nil {
		t.Fatalf("the control case failed to restore: %v", err)
	}
}

// TestFromCheckpointTightenedBudget: restoring under a smaller byte budget
// must shed the excess rather than exceed the bound.
func TestFromCheckpointTightenedBudget(t *testing.T) {
	m := populate(t, Config{})
	cp := m.Checkpoint()
	before := cp.Config.MaxEventBytes
	cp.Config.MaxEventBytes = minBudgetBytes
	cp.Config.MaxSeriesBytes = minBudgetBytes
	cp.Config.MaxDecisionBytes = minBudgetBytes
	restored, err := FromCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	st := restored.Stats()
	if st.EventBytes > minBudgetBytes || st.SeriesBytes > minBudgetBytes || st.DecisionBytes > minBudgetBytes {
		t.Fatalf("restore exceeded the tightened budgets: %+v", st)
	}
	if st.SeriesBytes == 0 {
		t.Error("restore under a tightened budget discarded everything")
	}
	checkMemory(t, restored)
	t.Logf("restored under %d-byte budget (was %d): %d event bytes, %d series bytes",
		minBudgetBytes, before, st.EventBytes, st.SeriesBytes)
}

// TestCheckpointEmptyStore: an empty substrate round-trips to an empty one.
func TestCheckpointEmptyStore(t *testing.T) {
	m := newMem(t, Config{})
	data, err := m.MarshalCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := FromCheckpoint(mustUnmarshal(t, data))
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Subjects()) != 0 || restored.Stats().Events != 0 {
		t.Fatal("empty checkpoint restored non-empty state")
	}
	checkMemory(t, restored)
}

// TestCheckpointDoesNotAliasStore: mutating the store after taking a
// checkpoint must not change the checkpoint.
func TestCheckpointDoesNotAliasStore(t *testing.T) {
	m := newMem(t, Config{})
	s := subj("a")
	e := ev(s, EventDeploy, 0)
	e.Attrs = map[string]string{"image": "v1"}
	mustAppend(t, m, e)
	cp := m.Checkpoint()
	before, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 10; i++ {
		mustAppend(t, m, ev(s, EventOOMKill, time.Duration(i)*time.Hour))
	}
	after, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the checkpoint changed when the store was written to")
	}
}

// TestSaveLoad exercises the CheckpointStore seam end to end.
func TestSaveLoad(t *testing.T) {
	ctx := context.Background()
	cs := &memStore{}
	if _, err := Load(ctx, cs); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("cold Load = %v, want ErrNoCheckpoint", err)
	}
	m := populate(t, Config{})
	if err := Save(ctx, cs, m); err != nil {
		t.Fatal(err)
	}
	restored, err := Load(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	assertSameObservable(t, m, restored)

	// Backend failures propagate; they are never swallowed.
	boom := errors.New("disk on fire")
	if err := Save(ctx, &memStore{err: boom}, m); !errors.Is(err, boom) {
		t.Errorf("Save error = %v, want the backend error", err)
	}
	if _, err := Load(ctx, &memStore{err: boom}); !errors.Is(err, boom) {
		t.Errorf("Load error = %v, want the backend error", err)
	}
	if _, err := UnmarshalCheckpoint([]byte("{not json")); err == nil {
		t.Error("UnmarshalCheckpoint accepted garbage")
	}
	if _, err := FromCheckpoint(nil); !errors.Is(err, ErrNoCheckpoint) {
		t.Errorf("FromCheckpoint(nil) = %v, want ErrNoCheckpoint", err)
	}
}

func mustUnmarshal(t *testing.T, data []byte) *Checkpoint {
	t.Helper()
	cp, err := UnmarshalCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// FuzzCheckpointDecode: no arbitrary byte string may panic the decoder or
// restore a store that violates the substrate's invariants. Anything that
// survives validation must be a legal store.
func FuzzCheckpointDecode(f *testing.F) {
	m, err := NewMemory(Config{})
	if err != nil {
		f.Fatal(err)
	}
	_ = m.Append(EvidenceEvent{At: t0, Kind: EventDeploy, Subject: subj("a")})
	_ = m.ObserveSample(subj("a"), Sample{At: t0, MilliCPU: 5})
	seed, err := m.MarshalCheckpoint()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte(`{"version":1,"config":{},"seq":1,"subjects":[{"subject":{"kind":"k","key":"v"}}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		cp, err := UnmarshalCheckpoint(data)
		if err != nil {
			return
		}
		restored, err := FromCheckpoint(cp)
		if err != nil {
			return
		}
		checkMemory(t, restored)
		// A restored store must re-encode to something that restores again
		// to the identical bytes: validation admits only fixed points.
		once, err := restored.MarshalCheckpoint()
		if err != nil {
			t.Fatalf("restored store failed to encode: %v", err)
		}
		again, err := FromCheckpoint(mustUnmarshalF(t, once))
		if err != nil {
			t.Fatalf("re-restoring a restored store failed: %v", err)
		}
		twice, err := again.MarshalCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		if string(once) != string(twice) {
			t.Fatalf("restore is not a fixed point:\n %s\n %s", once, twice)
		}
	})
}

func mustUnmarshalF(t *testing.T, data []byte) *Checkpoint {
	t.Helper()
	cp, err := UnmarshalCheckpoint(data)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}
