package explain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

func TestIDRoundTrip(t *testing.T) {
	at := t0.Add(37 * time.Minute).Add(123456789 * time.Nanosecond)
	subj := evidence.ContainerSubject(testCluster, containerKey("payments", "api", "server"))

	cases := []struct {
		name string
		id   ID
		want Ref
	}{
		{"event", EventID(evidence.EvidenceEvent{At: at, Kind: evidence.EventOOMKill, Subject: subj}),
			Ref{Kind: KindEvent, Cluster: testCluster, Subject: subj, EventKind: evidence.EventOOMKill, At: at}},
		{"decision", DecisionID(evidence.DecisionRecord{At: at, Subject: subj, Kind: evidence.DecisionRefusal}),
			Ref{Kind: KindDecision, Cluster: testCluster, Subject: subj, At: at}},
		{"digest", DigestID(subj, evidence.Digest{Start: at, Tier: evidence.TierHourly}),
			Ref{Kind: KindDigest, Cluster: testCluster, Subject: subj, Tier: evidence.TierHourly, At: at}},
		{"timeline", TimelineID(testCluster, evidence.TimelinePoint{At: at}),
			Ref{Kind: KindTimeline, Cluster: testCluster, At: at}},
		{"action", ActionID(LedgerAction{At: at, Cluster: testCluster, Fingerprint: "deadbeef"}),
			Ref{Kind: KindAction, Cluster: testCluster, Token: "deadbeef", At: at}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.id)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.id, err)
			}
			if got.Kind != tc.want.Kind || got.Cluster != tc.want.Cluster || got.Subject != tc.want.Subject ||
				got.EventKind != tc.want.EventKind || got.Tier != tc.want.Tier || got.Token != tc.want.Token {
				t.Errorf("Parse(%q) = %+v, want %+v", tc.id, got, tc.want)
			}
			if !got.At.Equal(tc.want.At) {
				t.Errorf("Parse(%q).At = %v, want %v", tc.id, got.At, tc.want.At)
			}
			if got.At.Location() != time.UTC {
				t.Errorf("Parse(%q).At is in %v, want UTC", tc.id, got.At.Location())
			}
		})
	}
}

// TestIDSurvivesHostileNames is the parsing-exactness test. Subject keys carry
// slashes by design ("Kind/namespace/name/container"), and a workload name is
// attacker-controlled (§5.7 threat 1), so an id must round-trip '/', '@' and
// '%' rather than merely usually work.
func TestIDSurvivesHostileNames(t *testing.T) {
	hostile := []string{
		"weird/name",
		"at@sign",
		"percent%signs",
		"all%2Fthe@things/at%once",
		"evt/other/cluster/kind/key/deploy@1",
		"",
	}
	for _, name := range hostile {
		subj := evidence.SubjectRef{Cluster: "c/l@u%s", Kind: evidence.SubjectWorkload, Key: "Deployment/ns/" + name}
		id := EventID(evidence.EvidenceEvent{At: t0, Kind: evidence.EventDeploy, Subject: subj})
		ref, err := Parse(id)
		if err != nil {
			t.Fatalf("Parse(%q): %v", id, err)
		}
		if ref.Subject.Key != subj.Key || ref.Cluster != subj.Cluster {
			t.Errorf("round trip of %q lost data: got cluster %q key %q", name, ref.Cluster, ref.Subject.Key)
		}
		if ref.EventKind != evidence.EventDeploy {
			t.Errorf("round trip of %q produced event kind %q", name, ref.EventKind)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		id   ID
	}{
		{"empty", ""},
		{"no timestamp", "evt/c/kind/key/deploy"},
		{"non-numeric timestamp", "tl/c@yesterday"},
		{"unknown prefix", "wat/c@1"},
		{"too few segments", "evt/c@1"},
		{"too many segments", "tl/c/extra@1"},
		{"bad tier", "dig/x/c/kind/key@1"},
		{"truncated escape", "tl/c%2@1"},
		{"unknown escape", "tl/c%zz@1"},
		{"trailing percent", "tl/c%@1"},
		{"timestamp overflow", "tl/c@99999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Parse(tc.id); err == nil {
				t.Fatalf("Parse(%q) = %+v, want an error", tc.id, got)
			} else if !errors.Is(err, ErrBadID) {
				t.Errorf("Parse(%q) error %v, want ErrBadID", tc.id, err)
			}
		})
	}
}

func TestUnescapeIsExactInverse(t *testing.T) {
	for _, s := range []string{"", "plain", "%", "/", "@", "%%", "a/b@c%d", "%25%2F%40"} {
		got, err := unescSeg(escSeg(s))
		if err != nil {
			t.Fatalf("unescSeg(escSeg(%q)): %v", s, err)
		}
		if got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
	// Lowercase escapes are not what escSeg emits, so they must not parse:
	// two spellings of one id would break de-duplication and citation
	// equality.
	if _, err := unescSeg("%2f"); err == nil {
		t.Error("lowercase escapes must be rejected, not silently accepted")
	}
}

func TestResolverResolvesEverythingItStores(t *testing.T) {
	in := baseInput()
	mem := memWithFixtures(t, in)
	subj := evidence.ContainerSubject(testCluster, containerKey("payments", "api", "server"))
	if err := mem.ObserveSample(subj, evidence.Sample{At: t0.Add(time.Minute), MilliCPU: 500, MemoryBytes: 1 << 28}); err != nil {
		t.Fatalf("ObserveSample: %v", err)
	}
	if err := mem.ObserveSample(subj, evidence.Sample{At: t0.Add(2 * time.Hour), MilliCPU: 600, MemoryBytes: 1 << 28}); err != nil {
		t.Fatalf("ObserveSample: %v", err)
	}
	dec := evidence.DecisionRecord{At: t0.Add(3 * time.Hour), Subject: subj, Kind: evidence.DecisionRecommendation, Summary: "shrink cpu"}
	if err := mem.RecordDecision(dec); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	digests, err := mem.Digests(subj, t0, t0.Add(hours(48)), evidence.TierHourly)
	if err != nil {
		t.Fatalf("Digests: %v", err)
	}
	if len(digests) == 0 {
		t.Fatal("fixture produced no hourly digest to cite")
	}

	r := Resolver{Store: mem, Actions: in.Actions}
	ids := []ID{
		EventID(in.Events[0]),
		TimelineID(testCluster, in.Timeline[0]),
		ActionID(in.Actions[0]),
		DecisionID(dec),
		DigestID(subj, digests[0]),
	}
	cits, err := r.ResolveAll(ids)
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	for i, c := range cits {
		if c.ID != ids[i] {
			t.Errorf("citation %d has id %q, want %q", i, c.ID, ids[i])
		}
		if c.Summary == "" {
			t.Errorf("citation %q resolved with an empty summary", c.ID)
		}
	}
}

func TestResolverReportsMisses(t *testing.T) {
	mem := memWithFixtures(t, baseInput())
	r := Resolver{Store: mem}
	subj := evidence.ContainerSubject(testCluster, containerKey("payments", "api", "server"))
	misses := []ID{
		EventID(evidence.EvidenceEvent{At: t0.Add(999 * time.Hour), Kind: evidence.EventOOMKill, Subject: subj}),
		DecisionID(evidence.DecisionRecord{At: t0, Subject: subj}),
		DigestID(subj, evidence.Digest{Start: t0, Tier: evidence.TierHourly}),
		TimelineID("no-such-cluster", evidence.TimelinePoint{At: t0}),
		ActionID(LedgerAction{At: t0, Cluster: testCluster, Fingerprint: "nope"}),
	}
	for _, id := range misses {
		if _, err := r.Resolve(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) error = %v, want ErrNotFound", id, err)
		}
	}
}

// TestResolveWrongEventKindAtSameInstant: two events can share a nanosecond;
// an id must resolve to the one it names and not to its neighbour.
func TestResolveWrongEventKindAtSameInstant(t *testing.T) {
	mem, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	subj := evidence.WorkloadSubject(testCluster, containerKey("ns", "app", "c").Workload)
	deploy := ev(t0.Add(time.Hour), evidence.EventDeploy, evidence.SeverityInfo, subj, nil)
	hpa := ev(t0.Add(time.Hour), evidence.EventHPAScale, evidence.SeverityInfo, subj, nil)
	for _, e := range []evidence.EvidenceEvent{deploy, hpa} {
		if err := mem.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	r := Resolver{Store: mem}
	c, err := r.Resolve(EventID(hpa))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.HasPrefix(c.Summary, evidence.EventHPAScale) {
		t.Errorf("resolved to %q, want the hpa-scale event", c.Summary)
	}
	missing := ev(t0.Add(time.Hour), evidence.EventOOMKill, evidence.SeverityCritical, subj, nil)
	if _, err := r.Resolve(EventID(missing)); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unstored kind at a stored instant resolved: %v", err)
	}
}

func TestResolveWithoutStore(t *testing.T) {
	var r Resolver
	if _, err := r.Resolve(TimelineID("c", evidence.TimelinePoint{At: t0})); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve with no store = %v, want ErrNotFound", err)
	}
}

func TestDedupeIDs(t *testing.T) {
	in := []ID{"c", "a", "b", "a", "c", "a"}
	got, dropped := dedupeIDs(in, -1)
	want := []ID{"a", "b", "c"}
	if dropped != 0 || len(got) != len(want) {
		t.Fatalf("dedupeIDs = %v (dropped %d), want %v", got, dropped, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedupeIDs = %v, want %v", got, want)
		}
	}
	got, dropped = dedupeIDs(in, 2)
	if dropped != 1 || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("capped dedupeIDs = %v (dropped %d), want [a b] dropping 1", got, dropped)
	}
	if got, dropped := dedupeIDs(nil, 4); got != nil || dropped != 0 {
		t.Errorf("dedupeIDs(nil) = %v, %d", got, dropped)
	}
}

// TestDedupeIDsDoesNotMutateCaller guards a real hazard in the in-place
// dedup idiom: the shared pools in payload.go are handed to dedupeIDs many
// times, and one reordered pool would change another driver's citations.
func TestDedupeIDsDoesNotMutateCaller(t *testing.T) {
	in := []ID{"c", "a", "b", "a"}
	orig := append([]ID(nil), in...)
	dedupeIDs(in, 2)
	for i := range orig {
		if in[i] != orig[i] {
			t.Fatalf("dedupeIDs mutated its argument: %v, was %v", in, orig)
		}
	}
}
