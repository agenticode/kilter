package api

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/explain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/store"
)

var (
	webRef = model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "web"}
	webKey = model.ContainerKey{Workload: webRef, Container: "app"}
)

// seriesSnapshot builds one snapshot of a small, real-shaped cluster. nodes
// controls the fleet size so a cost change is a change in node count, which is
// the term why-cost decomposes first.
func seriesSnapshot(cluster string, at time.Time, nodes int, cpu int64) *model.ClusterSnapshot {
	s := &model.ClusterSnapshot{ClusterID: cluster, Timestamp: at}
	for i := 0; i < nodes; i++ {
		s.Nodes = append(s.Nodes, model.NodeSpec{
			Name: fmt.Sprintf("n%d", i), Ready: true, InstanceType: "m5.xlarge", Provider: "aws",
			Labels:      map[string]string{"kubernetes.io/hostname": fmt.Sprintf("n%d", i), "kubernetes.io/arch": "amd64"},
			Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
			Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		})
	}
	s.Pods = []model.PodSpec{{
		UID: "u1", Name: "web-1", Namespace: "prod", NodeName: "n0", Phase: "Running",
		Labels: map[string]string{"app": "web"}, Workload: webRef, CreatedAt: at.Add(-time.Hour),
		Containers: []model.ContainerSpec{{Name: "app",
			Requests: model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}}},
	}}
	s.Workloads = []model.WorkloadInfo{{Ref: webRef, Replicas: 1, Ready: 1}}
	// A dense usage window ending at the snapshot instant, so the substrate
	// holds digests and the recommender has something to say.
	for i := 0; i < 288; i++ {
		s.Usage = append(s.Usage, model.Usage{
			Key: webKey, PodUID: "u1",
			Timestamp: at.Add(-24*time.Hour + time.Duration(i*5)*time.Minute),
			MilliCPU:  cpu, MemoryBytes: 400 << 20,
		})
	}
	return s
}

// brainWithHistory ingests `count` hourly snapshots and returns the brain, its
// store and the window covering them.
func brainWithHistory(t *testing.T, count int, shrinkAt int) (*Brain, *store.Store, time.Time, time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b, err := NewBrain(BrainConfig{CheckpointEvery: 1000}, pricing.Embedded(), st)
	if err != nil {
		t.Fatal(err)
	}
	base := t0.Add(48 * time.Hour)
	for i := 0; i < count; i++ {
		nodes := 4
		if shrinkAt >= 0 && i >= shrinkAt {
			nodes = 2
		}
		if err := b.Ingest(seriesSnapshot("c1", base.Add(time.Duration(i)*time.Hour), nodes, 150)); err != nil {
			t.Fatal(err)
		}
	}
	return b, st, base, base.Add(time.Duration(count) * time.Hour)
}

// ---------------------------------------------------------------- step 2/3

func TestIngestPopulatesTheSubstrate(t *testing.T) {
	b, _, from, to := brainWithHistory(t, 3, -1)

	tl, err := b.mem.Timeline("c1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != 3 {
		t.Fatalf("substrate holds %d timeline points after 3 ingests, want 3", len(tl))
	}
	for _, p := range tl {
		if p.CostUSDPerHour <= 0 {
			t.Fatalf("timeline point at %v priced at %v", p.At, p.CostUSDPerHour)
		}
		if p.Nodes != 4 {
			t.Fatalf("timeline point at %v holds %d nodes, want 4", p.At, p.Nodes)
		}
	}
	digs, err := b.mem.Digests(evidence.ContainerSubject("c1", webKey), from.Add(-48*time.Hour), to, evidence.TierHourly)
	if err != nil {
		t.Fatal(err)
	}
	if len(digs) == 0 {
		t.Fatal("no usage digests reached the substrate; an explanation would have nothing to cite")
	}
}

func TestIngestPopulatesTheSnapshotHistory(t *testing.T) {
	b, st, from, to := brainWithHistory(t, 3, -1)
	snaps, err := st.Snapshots("c1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("history holds %d snapshots after 3 hourly ingests, want 3", len(snaps))
	}
	// The bucket must be a real replay source, not a shape: SnapshotSource is
	// what backtest takes.
	var src backtest.SnapshotSource = st
	got, err := src.Snapshots("c1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("SnapshotSource returned %d, want 3", len(got))
	}
	_ = b
}

// TestDeployAndOOMEventsReachTheSubstrate covers the third thing Ingest owes
// the substrate: the change and adverse events that ground a driver.
func TestDeployAndOOMEventsReachTheSubstrate(t *testing.T) {
	b, _ := newBrain(t, "", false)
	base := t0.Add(48 * time.Hour)

	first := seriesSnapshot("c1", base, 4, 150)
	if err := b.Ingest(first); err != nil {
		t.Fatal(err)
	}
	// A rollout: the container's request changes and the replica count moves.
	second := seriesSnapshot("c1", base.Add(time.Hour), 4, 150)
	second.Pods[0].Containers[0].Requests = model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30}
	second.Workloads[0].Replicas = 3
	// And it came back from an OOMKill.
	second.Pods[0].Containers[0].RestartCount = 1
	second.Pods[0].Containers[0].LastOOMKilled = true
	if err := b.Ingest(second); err != nil {
		t.Fatal(err)
	}

	deploys, err := b.mem.Events(evidence.WorkloadSubject("c1", webRef), base, base.Add(2*time.Hour), evidence.EventDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if len(deploys) != 1 {
		t.Fatalf("got %d deploy events, want 1", len(deploys))
	}
	if !strings.Contains(deploys[0].Attrs["changed"], "replicas") {
		t.Fatalf("deploy event does not describe the change: %v", deploys[0].Attrs)
	}
	ooms, err := b.mem.Events(evidence.ContainerSubject("c1", webKey), base, base.Add(2*time.Hour), evidence.EventOOMKill)
	if err != nil {
		t.Fatal(err)
	}
	if len(ooms) != 1 {
		t.Fatalf("got %d oomkill events, want 1", len(ooms))
	}
	if ooms[0].Severity != evidence.SeverityCritical {
		t.Fatalf("oomkill recorded at severity %q", ooms[0].Severity)
	}

	// The same OOM, still reported by a later snapshot with no new restart,
	// must NOT become a second kill: three events out of one incident is a
	// false claim about how often it happened.
	third := seriesSnapshot("c1", base.Add(2*time.Hour), 4, 150)
	third.Pods[0].Containers[0].RestartCount = 1
	third.Pods[0].Containers[0].LastOOMKilled = true
	if err := b.Ingest(third); err != nil {
		t.Fatal(err)
	}
	ooms, err = b.mem.Events(evidence.ContainerSubject("c1", webKey), base, base.Add(4*time.Hour), evidence.EventOOMKill)
	if err != nil {
		t.Fatal(err)
	}
	if len(ooms) != 1 {
		t.Fatalf("a re-reported OOM produced %d events; the level was mistaken for an edge", len(ooms))
	}
}

// TestSubstrateStateDoesNotGrowWithPodChurn pins the bound on the only
// unbounded-looking state this unit adds. Pods churn forever; the detector's
// memory must be the size of the cluster, not of its history.
func TestSubstrateStateDoesNotGrowWithPodChurn(t *testing.T) {
	b, _ := newBrain(t, "", false)
	base := t0.Add(48 * time.Hour)
	for i := 0; i < 40; i++ {
		snap := seriesSnapshot("c1", base.Add(time.Duration(i)*time.Hour), 4, 150)
		snap.Pods[0].UID = fmt.Sprintf("pod-%d", i) // a brand new pod each time
		if err := b.Ingest(snap); err != nil {
			t.Fatal(err)
		}
	}
	s := b.substrateFor("c1")
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.restarts) != 1 {
		t.Fatalf("restart state holds %d entries after 40 distinct pods, want 1", len(s.restarts))
	}
	if len(s.specs) != 1 || len(s.replicas) != 1 {
		t.Fatalf("spec/replica state holds %d/%d entries, want 1/1", len(s.specs), len(s.replicas))
	}
}

// ---------------------------------------------------------------- persistence

func TestEvidenceSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "brain.db")
	base := t0.Add(48 * time.Hour)

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBrain(BrainConfig{CheckpointEvery: 1}, pricing.Embedded(), st)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := b.Ingest(seriesSnapshot("c1", base.Add(time.Duration(i)*time.Hour), 4, 150)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := b.mem.Timeline("c1", base, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	b2, err := NewBrain(BrainConfig{CheckpointEvery: 1}, pricing.Embedded(), st2)
	if err != nil {
		t.Fatal(err)
	}
	after, err := b2.mem.Timeline("c1", base, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || len(after) != 3 {
		t.Fatalf("restart restored %d timeline points, had %d", len(after), len(before))
	}
	for i := range after {
		if !after[i].At.Equal(before[i].At) || after[i].CostUSDPerHour != before[i].CostUSDPerHour {
			t.Fatalf("point %d changed across the restart: %+v vs %+v", i, after[i], before[i])
		}
	}
	// The digests must come back too, or an explanation after a restart would
	// silently lose exactly the history the operator restarted to look at.
	digs, err := b2.mem.Digests(evidence.ContainerSubject("c1", webKey), base.Add(-48*time.Hour), base.Add(3*time.Hour), evidence.TierHourly)
	if err != nil {
		t.Fatal(err)
	}
	if len(digs) == 0 {
		t.Fatal("usage digests did not survive the restart")
	}
}

// TestUnreadableCheckpointFailsTheBrainRatherThanStartingEmpty is the other
// half of the compatibility contract. Starting empty after a failed restore
// is indistinguishable from a cold boot, and the brain would then answer
// questions about history it silently discarded.
func TestUnreadableCheckpointFailsTheBrainRatherThanStartingEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "brain.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A well-framed envelope carrying a substrate checkpoint from a FUTURE
	// version of pkg/evidence: readable at the store layer, refused by
	// evidence.FromCheckpoint.
	future, err := json.Marshal(map[string]any{"version": evidence.CheckpointVersion + 1, "config": evidence.DefaultConfig()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEvidenceCheckpoint(future); err != nil {
		t.Fatal(err)
	}
	_, err = NewBrain(BrainConfig{}, pricing.Embedded(), st)
	if err == nil {
		t.Fatal("a brain started on an unreadable checkpoint, silently discarding it")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("error does not name the version problem: %v", err)
	}
}

// ---------------------------------------------------------------- refusal

func TestBacktestRefusesAHistoryTooShortToReplay(t *testing.T) {
	b, _, from, to := brainWithHistory(t, 1, -1)
	cfg := backtest.DefaultConfig()
	sc, err := b.Backtest("c1", from, to.Add(48*time.Hour), 24*time.Hour, cfg)
	if err == nil {
		t.Fatalf("a one-snapshot history produced a scorecard: %+v", sc)
	}
	if sc != nil {
		t.Fatal("a scorecard was returned alongside the refusal")
	}
	var short ErrHistoryTooShort
	if !asErr(err, &short) {
		t.Fatalf("refusal is not typed as too-short history: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal does not say it refused: %v", err)
	}
}

// TestBacktestRefusesWhenNothingWouldBeScored is the subtler half, and the
// one the count check alone would miss: enough snapshots to look like a
// history, but every one of them sits too close to the end of the requested
// window for a full scoring horizon to fit after it — the shape of "you asked
// about the last month and the brain started yesterday". backtest.Run answers
// that with a clean, empty, confident-looking scorecard; it must not reach an
// operator.
func TestBacktestRefusesWhenNothingWouldBeScored(t *testing.T) {
	b, _, base, _ := brainWithHistory(t, 2, -1)
	from := base.Add(-time.Hour)
	to := base.Add(time.Hour + time.Second) // both snapshots are inside
	sc, err := b.Backtest("c1", from, to, time.Hour+30*time.Second, backtest.DefaultConfig())
	if err == nil {
		t.Fatalf("a history with no scoreable instant produced a scorecard: snapshots=%d instants=%d",
			sc.Snapshots, sc.Instants)
	}
	if !strings.Contains(err.Error(), "no decision instant") {
		t.Fatalf("refusal does not name the reason: %v", err)
	}
}

func TestBacktestWithoutAStoreRefusesByName(t *testing.T) {
	b, _ := newBrain(t, "", false)
	_, err := b.Backtest("c1", t0, t0.Add(72*time.Hour), 24*time.Hour, backtest.DefaultConfig())
	var noHist ErrNoHistory
	if !asErr(err, &noHist) {
		t.Fatalf("a store-less brain did not refuse by name: %T %v", err, err)
	}
}

func asErr[T error](err error, target *T) bool {
	if v, ok := err.(T); ok {
		*target = v
		return true
	}
	return false
}

// ---------------------------------------------------------------- routes

func TestWhyCostRouteServesAVerifiedAttribution(t *testing.T) {
	// The fleet shrinks halfway through, so ΔCost is real and the node-count
	// term has something to explain.
	b, _, from, to := brainWithHistory(t, 6, 3)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	att := new(explain.Attribution)
	code := getJSON(t, srv.URL+"/api/v1/clusters/c1/why-cost?from="+
		from.UTC().Format(time.RFC3339)+"&to="+to.UTC().Format(time.RFC3339), att)
	if code != http.StatusOK {
		t.Fatalf("why-cost returned %d", code)
	}
	if att.Cluster != "c1" {
		t.Fatalf("attribution names cluster %q", att.Cluster)
	}
	if att.DeltaMicro == 0 {
		t.Fatal("a fleet that halved produced a zero ΔCost")
	}
	// The invariant, re-checked on the wire: sum(terms) + residual == delta.
	sum, err := att.Sum()
	if err != nil {
		t.Fatal(err)
	}
	if sum != att.DeltaMicro {
		t.Fatalf("terms sum to %d but ΔCost is %d", sum, att.DeltaMicro)
	}
	if len(att.Citations()) == 0 {
		t.Fatal("the served attribution carries no citations")
	}
	// The composition edges must have come from the time-keyed snapshot
	// history. Without them WhyCost still answers, but it says so in Notes and
	// only the node-count term is computable — so the absence of that note is
	// the assertion that the history is actually feeding the decomposition.
	for _, n := range att.Notes {
		if strings.Contains(n, "no fleet composition supplied") {
			t.Fatalf("the route served a degraded answer: the snapshot history did not reach basisFrom (%q)", n)
		}
	}
	var kinds []string
	for _, term := range att.Terms {
		kinds = append(kinds, term.Kind)
	}
	if len(kinds) < 2 {
		t.Fatalf("a composition-backed decomposition produced only %v", kinds)
	}
	// And every citation must resolve against the brain's own substrate —
	// the same store that produced the answer.
	if err := att.Verify(explain.Resolver{Store: b.Evidence()}); err != nil {
		t.Fatalf("a served attribution has a dangling citation: %v", err)
	}
}

// TestWhyCostOverAnUnpopulatedSubstrateRefuses is the failure this whole
// substrate exists to prevent, asserted from the outside: a brain that never
// saw the history must say so, not 500 and not answer confidently.
func TestWhyCostOverAnUnpopulatedSubstrateRefuses(t *testing.T) {
	b, _ := newBrain(t, "", false)
	if err := b.Ingest(seriesSnapshot("c1", t0.Add(48*time.Hour), 4, 150)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	var body map[string]string
	// A window well away from the one point that exists.
	code := getJSON(t, srv.URL+"/api/v1/clusters/c1/why-cost?from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z", &body)
	if code == http.StatusInternalServerError {
		t.Fatalf("an empty substrate produced a 500: %v", body)
	}
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for an unanswerable window, got %d (%v)", code, body)
	}
	if !strings.Contains(body["error"], "timeline point") {
		t.Fatalf("error does not explain what is missing: %v", body)
	}
}

func TestWhyCostRequiresBothWindowBounds(t *testing.T) {
	b, _, from, _ := brainWithHistory(t, 3, -1)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	var body map[string]string
	code := getJSON(t, srv.URL+"/api/v1/clusters/c1/why-cost?from="+from.UTC().Format(time.RFC3339), &body)
	if code != http.StatusBadRequest {
		t.Fatalf("a missing --to returned %d, want 400", code)
	}
	if !strings.Contains(body["error"], "replay") {
		t.Fatalf("error does not say why the window is required: %v", body)
	}
}

func TestExplainRouteServesAVerifiedPayload(t *testing.T) {
	b, _, from, to := brainWithHistory(t, 4, -1)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	ex := new(explain.Explanation)
	code := getJSON(t, srv.URL+"/api/v1/clusters/c1/explain?subject=Deployment/prod/web/app&from="+
		from.Add(-48*time.Hour).UTC().Format(time.RFC3339)+"&to="+to.UTC().Format(time.RFC3339), ex)
	if code != http.StatusOK {
		t.Fatalf("explain returned %d", code)
	}
	if ex.Subject.Key != webKey.String() {
		t.Fatalf("payload names subject %q", ex.Subject.Key)
	}
	if len(ex.Citations) == 0 {
		t.Fatal("the served explanation carries no citations")
	}
	if err := ex.Verify(explain.Resolver{Store: b.Evidence()}); err != nil {
		t.Fatalf("a served explanation has a dangling citation: %v", err)
	}
	// The window must be echoed as concrete instants, whatever was asked.
	if ex.From.IsZero() || ex.To.IsZero() {
		t.Fatalf("payload does not state its own window: [%v, %v]", ex.From, ex.To)
	}
}

func TestExplainDefaultWindowIsResolvedFromHistoryNotAClock(t *testing.T) {
	b, _, _, to := brainWithHistory(t, 4, -1)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	url := srv.URL + "/api/v1/clusters/c1/explain?subject=Deployment/prod/web/app"

	first := new(explain.Explanation)
	if code := getJSON(t, url, first); code != http.StatusOK {
		t.Fatalf("explain returned %d", code)
	}
	// The default window ends one second after the newest ingested snapshot,
	// so two calls a moment apart must agree exactly.
	wantTo := to.Add(-time.Hour).Add(time.Second)
	if !first.To.Equal(wantTo) {
		t.Fatalf("default window ends at %v, want %v (the latest snapshot, not a clock)", first.To, wantTo)
	}
	second := new(explain.Explanation)
	if code := getJSON(t, url, second); code != http.StatusOK {
		t.Fatalf("second explain returned %d", code)
	}
	if !second.From.Equal(first.From) || !second.To.Equal(first.To) {
		t.Fatalf("two identical requests resolved different windows: [%v,%v] vs [%v,%v]",
			first.From, first.To, second.From, second.To)
	}
}

func TestExplainRejectsAMalformedSubject(t *testing.T) {
	b, _, _, _ := brainWithHistory(t, 3, -1)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	for _, subject := range []string{"", "web", "Deployment/prod", "Deployment//web/app", "a/b/c/d/e"} {
		var body map[string]string
		code := getJSON(t, srv.URL+"/api/v1/clusters/c1/explain?subject="+subject, &body)
		if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
			t.Fatalf("subject %q returned %d, want a refusal", subject, code)
		}
	}
}

func TestExplainRoutesRejectAnUnknownCluster(t *testing.T) {
	b, _, from, to := brainWithHistory(t, 3, -1)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	for _, path := range []string{
		"/api/v1/clusters/nope/why-cost?from=" + from.UTC().Format(time.RFC3339) + "&to=" + to.UTC().Format(time.RFC3339),
		"/api/v1/clusters/nope/explain?subject=Deployment/prod/web/app",
	} {
		var body map[string]string
		if code := getJSON(t, srv.URL+path, &body); code != http.StatusNotFound {
			t.Fatalf("%s returned %d, want 404", path, code)
		}
	}
}

// TestExplainRoutesRefuseAnUnboundedWindow: the substrate is bounded, so a
// ten-year window cannot hold ten years. Returning the bounded slice would
// read as a complete answer over the requested span.
func TestExplainRoutesRefuseAnUnboundedWindow(t *testing.T) {
	b, _, _, to := brainWithHistory(t, 3, -1)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	far := to.Add(-10 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, path := range []string{
		"/api/v1/clusters/c1/why-cost?from=" + far + "&to=" + to.UTC().Format(time.RFC3339),
		"/api/v1/clusters/c1/explain?subject=Deployment/prod/web/app&from=" + far + "&to=" + to.UTC().Format(time.RFC3339),
	} {
		var body map[string]string
		code := getJSON(t, srv.URL+path, &body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s returned %d, want 400", path, code)
		}
		if !strings.Contains(body["error"], "over the") {
			t.Fatalf("refusal does not name the cap: %v", body)
		}
	}
}

// TestWhyCostRefusesAnAnswerItCannotVerify drives the publish gate directly:
// an attribution whose citations were computed against one substrate and
// verified against another must not be served.
func TestWhyCostRefusesAnAnswerItCannotVerify(t *testing.T) {
	b, _, from, to := brainWithHistory(t, 6, 3)
	att, err := b.WhyCost("c1", from, to)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := att.Verify(explain.Resolver{Store: empty}); err == nil {
		t.Fatal("Verify passed against a store that never saw the observations; " +
			"the publish gate this route depends on does not gate")
	}
}

// ---------------------------------------------------------------- determinism

// TestWhyCostIsIndependentOfIngestOrderAndRepeats is the shuffle test: money
// is summed in a defined order, and no map iteration reaches the answer.
func TestWhyCostIsIndependentOfIngestOrderAndRepeats(t *testing.T) {
	build := func(seed int64) *explain.Attribution {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "brain.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		b, err := NewBrain(BrainConfig{CheckpointEvery: 1000}, pricing.Embedded(), st)
		if err != nil {
			t.Fatal(err)
		}
		base := t0.Add(48 * time.Hour)
		snaps := make([]*model.ClusterSnapshot, 6)
		for i := range snaps {
			nodes := 4
			if i >= 3 {
				nodes = 2
			}
			snaps[i] = seriesSnapshot("c1", base.Add(time.Duration(i)*time.Hour), nodes, 150)
		}
		if seed != 0 {
			// Shuffle the ORDER OF THE PODS AND NODES inside each snapshot.
			// Snapshot order itself is not shuffled: the substrate rejects
			// out-of-order points by design, and the CLI already sorts.
			r := rand.New(rand.NewSource(seed))
			for _, s := range snaps {
				r.Shuffle(len(s.Nodes), func(i, j int) { s.Nodes[i], s.Nodes[j] = s.Nodes[j], s.Nodes[i] })
				r.Shuffle(len(s.Usage), func(i, j int) { s.Usage[i], s.Usage[j] = s.Usage[j], s.Usage[i] })
			}
		}
		for _, s := range snaps {
			if err := b.Ingest(s); err != nil {
				t.Fatal(err)
			}
		}
		att, err := b.WhyCost("c1", base, base.Add(6*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return att
	}

	want, err := json.Marshal(build(0))
	if err != nil {
		t.Fatal(err)
	}
	// Three permutations, and each repeated in-process: Go randomizes map
	// iteration on every range, so repetition inside one process is the real
	// determinism test.
	for _, seed := range []int64{1, 2, 3} {
		for rep := 0; rep < 3; rep++ {
			got, err := json.Marshal(build(seed))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("seed %d rep %d produced a different attribution:\n got %s\nwant %s", seed, rep, got, want)
			}
		}
	}
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
	return resp.StatusCode
}
