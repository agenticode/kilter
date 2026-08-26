package api

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/store"
	"github.com/agenticode/kilter/pkg/whatif"
)

// ---------------------------------------------------------------- fixtures

// whatIfSnapshot builds one snapshot of a small, over-provisioned cluster:
// every container asks for 2 vCPU / 4 GiB and uses a fifteenth of it, which is
// what leaves enough regret on the table for a candidate policy to win by more
// than the gate's noise margin.
//
// Snapshots are a DAY apart (see whatIfBrain), so each one's trailing 24h of
// five-minute usage is both the history the recommender is confident from at a
// decision instant and the future the oracle is scored against at the previous
// one.
func whatIfSnapshot(cluster string, at time.Time, workloads int) *model.ClusterSnapshot {
	s := &model.ClusterSnapshot{ClusterID: cluster, Timestamp: at}
	for i := 0; i < 4; i++ {
		s.Nodes = append(s.Nodes, model.NodeSpec{
			Name: fmt.Sprintf("n%d", i), Ready: true, InstanceType: "m5.2xlarge", Provider: "aws",
			Labels:      map[string]string{"kubernetes.io/hostname": fmt.Sprintf("n%d", i), "kubernetes.io/arch": "amd64"},
			Capacity:    model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30},
			Allocatable: model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30},
		})
	}
	for w := 0; w < workloads; w++ {
		ref := model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: fmt.Sprintf("svc%d", w)}
		key := model.ContainerKey{Workload: ref, Container: "app"}
		uid := fmt.Sprintf("u%d", w)
		s.Pods = append(s.Pods, model.PodSpec{
			UID: uid, Name: fmt.Sprintf("svc%d-1", w), Namespace: "prod",
			NodeName: fmt.Sprintf("n%d", w%4), Phase: "Running",
			Labels: map[string]string{"app": fmt.Sprintf("svc%d", w)}, Workload: ref,
			CreatedAt:  at.Add(-96 * time.Hour),
			Containers: []model.ContainerSpec{{Name: "app", Requests: model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}}},
		})
		s.Workloads = append(s.Workloads, model.WorkloadInfo{Ref: ref, Replicas: 1, Ready: 1})
		for i := 0; i < 288; i++ {
			s.Usage = append(s.Usage, model.Usage{
				Key: key, PodUID: uid,
				Timestamp: at.Add(-24*time.Hour + time.Duration(i*5)*time.Minute),
				MilliCPU:  150, MemoryBytes: 400 << 20,
			})
		}
	}
	return s
}

// wiFrom is the fixture's replay window start; wiTo its end.
var wiFrom = t0.Add(48 * time.Hour)

const wiSnapshots = 4

var wiTo = wiFrom.Add(wiSnapshots * 24 * time.Hour)

// whatIfBrain ingests `count` daily snapshots of a 4-workload cluster.
func whatIfBrain(t *testing.T, count int) (*Brain, *store.Store) {
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
	for i := 0; i < count; i++ {
		if err := b.Ingest(whatIfSnapshot("c1", wiFrom.Add(time.Duration(i)*24*time.Hour), 4)); err != nil {
			t.Fatal(err)
		}
	}
	return b, st
}

// whatIfServer registers the surface on its own mux so the clock can be
// fixed: every timestamp a proposal carries then comes from the test, not
// from the wall clock, and two runs produce the same bytes.
func whatIfServer(t *testing.T, b *Brain) (*whatIfSurface, *httptest.Server) {
	t.Helper()
	s := newWhatIfSurface(b)
	s.now = whatif.FixedClock(t0.Add(240 * time.Hour))
	mux := http.NewServeMux()
	s.register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv
}

// whatIfURL builds a what-if request over the fixture's whole window.
func whatIfURL(set string) string {
	return fmt.Sprintf("/api/v1/clusters/c1/whatif?from=%s&to=%s&%s",
		wiFrom.Format(time.RFC3339), wiTo.Format(time.RFC3339), set)
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, raw)
	}
	return m
}

func errorText(t *testing.T, raw []byte) string {
	t.Helper()
	m := decodeMap(t, raw)
	s, _ := m["error"].(string)
	if s == "" {
		t.Fatalf("response carries no error field: %s", raw)
	}
	return s
}

// ---------------------------------------------------- simulation ≠ measurement

// TestWhatIfRouteServesASimulationNotAMeasurement is the trap this file exists
// for. Two scorecards from a policy that never ran have the same field names,
// the same units and the same confident tone as a measurement of the policy
// that did. The distinction has to be structural, or a client drops it.
func TestWhatIfRouteServesASimulationNotAMeasurement(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)

	code, raw := do(t, srv, "GET", whatIfURL("set=memory-headroom=1.05"), "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	body := decodeMap(t, raw)

	// 1. The envelope says what it is, on the wire, where a client that never
	//    opens this file will see it.
	if body["kind"] != "simulation" {
		t.Fatalf("kind is %v, want \"simulation\"", body["kind"])
	}
	for _, f := range []string{"observed", "applied"} {
		v, ok := body[f].(bool)
		if !ok || v {
			t.Fatalf("%q is %v; a counterfactual must never claim to be %s", f, body[f], f)
		}
	}
	if s, _ := body["statement"].(string); !strings.Contains(s, "never in force") {
		t.Fatalf("statement does not say the policy never ran: %q", s)
	}

	// 2. The answer is ONLY reachable under "simulation". Nothing at the top
	//    level can be mistaken for a measurement, because nothing at the top
	//    level is a number about the fleet.
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"applied", "basis", "kind", "observed", "simulation", "statement"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("top-level keys %v, want %v", keys, want)
	}

	// 3. A client that drops the envelope gets NOTHING — not a plausible
	//    measurement. This is the property that makes the distinction
	//    impossible to lose by accident.
	var bare whatif.Result
	if err := json.Unmarshal(raw, &bare); err != nil {
		t.Fatal(err)
	}
	if bare.Cluster != "" || bare.BaselineScore != nil || bare.CandidateScore != nil || bare.Gate.Passed {
		t.Fatalf("the payload decodes as a bare whatif.Result (%+v); the envelope is droppable", bare)
	}

	// 4. And in Go: Simulated has no exported field, so no caller anywhere can
	//    hand back a counterfactual with observed set to true.
	st := reflect.TypeOf(Simulated{})
	for i := 0; i < st.NumField(); i++ {
		if st.Field(i).IsExported() {
			t.Fatalf("Simulated.%s is exported; the marker can be built with the wrong value",
				st.Field(i).Name)
		}
	}

	// The basis describes what was replayed, so a reader can weigh the answer.
	basis := body["basis"].(map[string]any)
	if got := basis["snapshotsReplayed"]; got != float64(wiSnapshots) {
		t.Fatalf("snapshotsReplayed %v, want %d", got, wiSnapshots)
	}
	if got := basis["instantsScored"]; got.(float64) < 1 {
		t.Fatalf("instantsScored %v: nothing was scored", got)
	}
	if got, want := basis["incumbentPolicy"], b.IncumbentPolicy().Hash(); got != want {
		t.Fatalf("incumbentPolicy %v, want the brain's own policy %v", got, want)
	}
	if basis["candidatePolicy"] == basis["incumbentPolicy"] {
		t.Fatal("the candidate hashes to the incumbent; nothing was compared")
	}
	sim := body["simulation"].(map[string]any)
	for _, k := range []string{"baseline", "candidate", "delta", "gate", "changes"} {
		if _, ok := sim[k]; !ok {
			t.Fatalf("the simulation is missing %q: %v", k, sim)
		}
	}
}

// TestWhatIfAnswerIsByteIdenticalAcrossRepeatedRequests repeats one request in
// ONE process, which is the real test: Go randomizes map iteration on every
// range, and a scorecard carries a refusal map.
func TestWhatIfAnswerIsByteIdenticalAcrossRepeatedRequests(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)
	var first []byte
	for i := 0; i < 4; i++ {
		code, raw := do(t, srv, "GET", whatIfURL("set=memory-headroom=1.05&set=cpu-headroom=1.05"), "")
		if code != http.StatusOK {
			t.Fatalf("run %d: status %d: %s", i, code, raw)
		}
		if i == 0 {
			first = raw
			continue
		}
		if string(raw) != string(first) {
			t.Fatalf("run %d differs from run 0", i)
		}
	}
}

// ---------------------------------------------------------------- statuses

// TestWhatIfStatusCodesAreThreeDifferentFacts pins the contract
// SUBSTRATE-FINDINGS.md §3.2 states, because collapsing any pair of these
// makes the surface lie about a different thing:
//
//   - 404 → 422 would report a cluster nobody ever sent as "not enough data".
//   - 422 → 500 pages an operator at 3am for an empty database.
//   - 422 → 200 is the `regret $0.00` failure in a different costume.
func TestWhatIfStatusCodesAreThreeDifferentFacts(t *testing.T) {
	t.Run("404 the cluster was never ingested", func(t *testing.T) {
		b, _ := whatIfBrain(t, wiSnapshots)
		_, srv := whatIfServer(t, b)
		path := strings.Replace(whatIfURL("set=memory-headroom=1.05"), "/c1/", "/nope/", 1)
		code, raw := do(t, srv, "GET", path, "")
		if code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", code, raw)
		}
		if !strings.Contains(errorText(t, raw), "unknown cluster") {
			t.Fatalf("404 does not name the missing cluster: %s", raw)
		}
	})

	t.Run("422 the substrate holds too little history", func(t *testing.T) {
		b, _ := whatIfBrain(t, 1) // ingested, so the cluster IS known
		_, srv := whatIfServer(t, b)
		code, raw := do(t, srv, "GET", whatIfURL("set=memory-headroom=1.05"), "")
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422: %s", code, raw)
		}
		msg := errorText(t, raw)
		if !strings.Contains(msg, "snapshot(s)") || !strings.Contains(msg, "refused") {
			t.Fatalf("422 does not read as an evidence shortfall: %s", msg)
		}
		// It must not read as a fault, and it must not carry an answer.
		assertNoVerdict(t, raw)
	})

	t.Run("422 a brain with no store at all", func(t *testing.T) {
		b, _ := newBrain(t, "", false)
		if err := b.Ingest(whatIfSnapshot("c1", wiFrom, 1)); err != nil {
			t.Fatal(err)
		}
		_, srv := whatIfServer(t, b)
		code, raw := do(t, srv, "GET", whatIfURL("set=memory-headroom=1.05"), "")
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422: %s", code, raw)
		}
		if !strings.Contains(errorText(t, raw), "no snapshot history") {
			t.Fatalf("422 does not name the missing history: %s", raw)
		}
	})

	t.Run("500 only for a defect in this process", func(t *testing.T) {
		// The retained history will not read back. That is not a question
		// anyone asked wrongly and it is not an evidence shortfall: the brain
		// believes it has history and cannot produce it.
		st, err := store.Open(filepath.Join(t.TempDir(), "brain.db"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewBrain(BrainConfig{CheckpointEvery: 1000}, pricing.Embedded(), st)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < wiSnapshots; i++ {
			if err := b.Ingest(whatIfSnapshot("c1", wiFrom.Add(time.Duration(i)*24*time.Hour), 1)); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		_, srv := whatIfServer(t, b)
		code, raw := do(t, srv, "GET", whatIfURL("set=memory-headroom=1.05"), "")
		if code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500: %s", code, raw)
		}
		msg := errorText(t, raw)
		if !strings.Contains(msg, "retained history") {
			t.Fatalf("500 does not name what failed: %s", msg)
		}
		// A defect must not be dressed as an evidence shortfall — an operator
		// who reads "not enough history" goes back to bed.
		if strings.Contains(msg, "refused") || strings.Contains(msg, "snapshot(s)") {
			t.Fatalf("a defect is reported as an evidence shortfall: %s", msg)
		}
	})
}

// TestWhatIfRefusesTwoEmptyReplaysRatherThanComparingThem is the refusal
// cmd/WHATIF-WIRING-FINDINGS.md §3.1 argues matters more here than for a plain
// backtest: two replays over nothing agree on every field, so every delta is
// 0.00, the gate reports "short of the margin", and the whole thing reads as a
// considered negative verdict about a measurement that never happened.
func TestWhatIfRefusesTwoEmptyReplaysRatherThanComparingThem(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)
	// Two retained snapshots sit inside this window, so it looks like a
	// history; neither has a full 30-hour scoring horizon after it inside the
	// window, so nothing can be scored.
	url := fmt.Sprintf("/api/v1/clusters/c1/whatif?from=%s&to=%s&horizon=%s&set=memory-headroom=1.05",
		wiFrom.Add(time.Hour).Format(time.RFC3339),
		wiFrom.Add(48*time.Hour+time.Second).Format(time.RFC3339), "30h")
	code, raw := do(t, srv, "GET", url, "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", code, raw)
	}
	if !strings.Contains(errorText(t, raw), "no decision instant") {
		t.Fatalf("the refusal does not name the reason: %s", raw)
	}
	assertNoVerdict(t, raw)
}

// assertNoVerdict fails if a refusal smuggled a scorecard, a delta or a gate
// verdict out with it. A refusal that also prints numbers is a refusal a
// reader will read past.
func assertNoVerdict(t *testing.T, raw []byte) {
	t.Helper()
	for _, forbidden := range []string{"regret", "oracleGap", "\"gate\"", "\"delta\"", "\"simulation\""} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("a refusal carries %q: %s", forbidden, raw)
		}
	}
}

// TestEveryWhatIfFailureIsClassified pins the mapping itself, including the
// default: an error nobody classified is a hole in whatifroutes.go, not a fact
// about the caller's evidence, so it must not read as one.
func TestEveryWhatIfFailureIsClassified(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{badRequest{fmt.Errorf("x")}, http.StatusBadRequest},
		{notIngested{fmt.Errorf("x")}, http.StatusNotFound},
		{notEnoughEvidence{fmt.Errorf("x")}, http.StatusUnprocessableEntity},
		{notEnoughEvidence{ErrNoHistory{Cluster: "c1"}}, http.StatusUnprocessableEntity},
		{notEnoughEvidence{ErrHistoryTooShort{Cluster: "c1"}}, http.StatusUnprocessableEntity},
		{conflict{fmt.Errorf("x")}, http.StatusConflict},
		{defect{fmt.Errorf("x")}, http.StatusInternalServerError},
		{fmt.Errorf("wrapped: %w", whatif.ErrNotFound), http.StatusNotFound},
		{fmt.Errorf("nobody classified me"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := whatIfStatus(c.err); got != c.want {
			t.Fatalf("whatIfStatus(%T) = %d, want %d", c.err, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- the bound

// TestWhatIfRefusesAnUnboundedWindow. The what-if route materialises the
// window TWICE — one replay per policy — so an unbounded `from` is a denial of
// service aimed at the brain's own store. The bound is refused rather than
// truncated: a silently narrowed window answers a question the caller did not
// ask, and its answer looks exactly like an answer to the one they did.
func TestWhatIfRefusesAnUnboundedWindow(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)

	// The bound this route enforces is the one the rest of the substrate
	// honours (SUBSTRATE-FINDINGS.md §1.3), not a second number that can drift
	// from it.
	if maxExplainWindow != store.DefaultSnapshotRetention().MaxWindow {
		t.Fatalf("route cap %v does not match the store's %v",
			maxExplainWindow, store.DefaultSnapshotRetention().MaxWindow)
	}

	for _, c := range []struct{ name, from, to string }{
		{"wider than the cap", wiTo.Add(-401 * 24 * time.Hour).Format(time.RFC3339), wiTo.Format(time.RFC3339)},
		{"inverted", wiTo.Format(time.RFC3339), wiFrom.Format(time.RFC3339)},
		{"empty", wiFrom.Format(time.RFC3339), wiFrom.Format(time.RFC3339)},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := fmt.Sprintf("/api/v1/clusters/c1/whatif?from=%s&to=%s&set=memory-headroom=1.05", c.from, c.to)
			code, raw := do(t, srv, "GET", url, "")
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", code, raw)
			}
			assertNoVerdict(t, raw)
		})
	}

	// A window one hour inside the cap is answered (or refused for evidence),
	// never refused for its width: the bound is a cap, not a rounding.
	url := fmt.Sprintf("/api/v1/clusters/c1/whatif?from=%s&to=%s&set=memory-headroom=1.05",
		wiTo.Add(-399*24*time.Hour).Format(time.RFC3339), wiTo.Format(time.RFC3339))
	code, raw := do(t, srv, "GET", url, "")
	if code != http.StatusOK {
		t.Fatalf("a window inside the cap was refused with %d: %s", code, raw)
	}

	// Both bounds are required — an answer computed over a wall-clock default
	// cannot be replayed.
	for _, q := range []string{"from=" + wiFrom.Format(time.RFC3339), "to=" + wiTo.Format(time.RFC3339)} {
		code, raw := do(t, srv, "GET", "/api/v1/clusters/c1/whatif?set=memory-headroom=1.05&"+q, "")
		if code != http.StatusBadRequest {
			t.Fatalf("a half-specified window returned %d: %s", code, raw)
		}
	}
}

// ---------------------------------------------------------------- the axes

// TestSetMovesTheAxisPkgWhatifThinksItMoves cross-checks the axis→config
// projection this package had to restate (whatif.Axis.get/set are unexported)
// against pkg/whatif's OWN projection: Result.Changes is computed by
// changesBetween, which uses Axis.get. A mis-mapped field — MemoryHeadroom
// moved when the caller said cpu-headroom — is internally consistent and
// wrong, and fails here rather than quietly tuning the wrong knob under the
// right name.
func TestSetMovesTheAxisPkgWhatifThinksItMoves(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)
	cases := []struct {
		axis  whatif.Axis
		value string
		want  float64
		extra string
	}{
		{whatif.AxisCPUPercentile, "0.90", 0.90, ""},
		{whatif.AxisMemoryPercentile, "0.95", 0.95, ""},
		{whatif.AxisCPUHeadroom, "1.05", 1.05, ""},
		{whatif.AxisMemoryHeadroom, "1.05", 1.05, ""},
		// The soak lives in decision.Config, which reaches a replay only when
		// the refusal predicates run — see the refusal test below.
		{whatif.AxisBaseSoak, "3", 3, "&enforceRefusals=true"},
	}
	for _, c := range cases {
		t.Run(string(c.axis), func(t *testing.T) {
			code, raw := do(t, srv, "GET", whatIfURL(fmt.Sprintf("set=%s=%s%s", c.axis, c.value, c.extra)), "")
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, raw)
			}
			var body struct {
				Simulation struct {
					Changes []whatif.Change `json:"changes"`
				} `json:"simulation"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Simulation.Changes) != 1 {
				t.Fatalf("pkg/whatif reports %d changes for one --set: %+v",
					len(body.Simulation.Changes), body.Simulation.Changes)
			}
			got := body.Simulation.Changes[0]
			if got.Axis != c.axis {
				t.Fatalf("set %s moved %s", c.axis, got.Axis)
			}
			if got.To != c.want {
				t.Fatalf("set %s=%s landed at %v", c.axis, c.value, got.To)
			}
		})
	}
}

// TestBaseSoakWithoutEnforcedRefusalsIsRefused. decision.Config reaches a
// replay only when the refusal predicates are enforced, so a soak what-if
// without them scores two policies that behave identically and prints a delta
// of zeroes — a what-if of a policy nobody ran, which is the artefact this
// plane exists to prevent. cmd/WHATIF-WIRING-FINDINGS.md §3.6 is the same
// mismatch seen from the other side.
func TestBaseSoakWithoutEnforcedRefusalsIsRefused(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)
	code, raw := do(t, srv, "GET", whatIfURL("set=base-soak=3"), "")
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", code, raw)
	}
	msg := errorText(t, raw)
	if !strings.Contains(msg, "enforceRefusals") || !strings.Contains(msg, "BOTH replays") {
		t.Fatalf("the refusal does not name the yardstick: %s", msg)
	}
	// And with the yardstick set on both sides, the same request is answered.
	code, raw = do(t, srv, "GET", whatIfURL("set=base-soak=3&enforceRefusals=true"), "")
	if code != http.StatusOK {
		t.Fatalf("status %d with enforceRefusals: %s", code, raw)
	}
	if body := decodeMap(t, raw); body["basis"].(map[string]any)["enforceDecisionRefusals"] != true {
		t.Fatalf("the basis does not record the yardstick: %s", raw)
	}
}

// TestWhatIfRejectsAQuestionItCannotAnswer covers the malformed-candidate
// cases, each of which would otherwise produce a confident answer to something
// the caller did not mean.
func TestWhatIfRejectsAQuestionItCannotAnswer(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)
	inc := b.IncumbentPolicy()
	cases := []struct{ name, set, wantMsg string }{
		{"no axis at all", "", "at least one axis"},
		{"unknown axis", "set=gpu-headroom=2", "unknown axis"},
		{"outside the hard bounds", "set=cpu-headroom=9", "hard bounds"},
		{"not a number", "set=cpu-headroom=lots", "invalid syntax"},
		{"no value", "set=cpu-headroom", "<axis>=<value>"},
		{"twice", "set=cpu-headroom=1.05&set=cpu-headroom=1.06", "given twice"},
		{"the incumbent restated", fmt.Sprintf("set=cpu-headroom=%v", inc.Rec.CPUHeadroom), "not a question"},
		{"a horizon that is not a duration", "set=cpu-headroom=1.05&horizon=soon", "horizon"},
		{"a horizon wider than the window", "set=cpu-headroom=1.05&horizon=200h", "exceeds the"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, raw := do(t, srv, "GET", whatIfURL(c.set), "")
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", code, raw)
			}
			if !strings.Contains(errorText(t, raw), c.wantMsg) {
				t.Fatalf("400 does not say why: %s", raw)
			}
		})
	}
}

// ---------------------------------------------------------------- proposals

// filedProposal drives POST /api/v1/proposals and returns the decoded record.
func filedProposal(t *testing.T, srv *httptest.Server, set string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"cluster":"c1","from":%q,"to":%q,"set":{%s},"rationale":"nightly sweep"}`,
		wiFrom.Format(time.RFC3339), wiTo.Format(time.RFC3339), set)
	code, raw := do(t, srv, "POST", "/api/v1/proposals", body)
	if code != http.StatusOK {
		t.Fatalf("filing: status %d: %s", code, raw)
	}
	return decodeMap(t, raw)
}

// TestProposalRoutesAreProjectionsOfTheStore. §7.1: Record has MarshalJSON, so
// the wire form is the package's. These handlers must therefore be byte-equal
// to the store's own rendering — anything else is a second definition of what
// a proposal is, which is a second answer waiting to disagree with the CLI's.
func TestProposalRoutesAreProjectionsOfTheStore(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	s, srv := whatIfServer(t, b)

	rec := filedProposal(t, srv, `"memory-headroom":1.05`)
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatalf("filed record carries no id: %v", rec)
	}
	if rec["state"] != string(whatif.StateGated) {
		t.Fatalf("state %v, want gated: the gate passed on this candidate", rec["state"])
	}

	stored, ok := s.props.Get(id)
	if !ok {
		t.Fatalf("the record the route returned is not in the store: %s", id)
	}
	wantBytes, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}

	code, raw := do(t, srv, "GET", "/api/v1/proposals/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	if strings.TrimSpace(string(raw)) != string(wantBytes) {
		t.Fatalf("GET /proposals/{id} is not pkg/whatif's own wire form:\n got %s\nwant %s", raw, wantBytes)
	}

	code, raw = do(t, srv, "GET", "/api/v1/proposals", "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	var list struct {
		Proposals []json.RawMessage `json:"proposals"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Proposals) != 1 {
		t.Fatalf("list holds %d records, want 1", len(list.Proposals))
	}
	if string(list.Proposals[0]) != string(wantBytes) {
		t.Fatalf("the listed record is not the stored one:\n got %s\nwant %s", list.Proposals[0], wantBytes)
	}

	t.Run("filters", func(t *testing.T) {
		for _, c := range []struct {
			query string
			want  int
		}{
			{"?cluster=c1", 1}, {"?cluster=other", 0},
			{"?state=gated", 1}, {"?state=rejected", 0},
			{"?cluster=c1&state=gated", 1}, {"?cluster=other&state=gated", 0},
		} {
			code, raw := do(t, srv, "GET", "/api/v1/proposals"+c.query, "")
			if code != http.StatusOK {
				t.Fatalf("%s: status %d: %s", c.query, code, raw)
			}
			var got struct {
				Proposals []json.RawMessage `json:"proposals"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Proposals) != c.want {
				t.Fatalf("%s: %d records, want %d", c.query, len(got.Proposals), c.want)
			}
			if got.Proposals == nil {
				t.Fatalf("%s: proposals is null; an empty result must be []", c.query)
			}
		}
	})

	t.Run("unknown state and unknown id", func(t *testing.T) {
		code, raw := do(t, srv, "GET", "/api/v1/proposals?state=approvedish", "")
		if code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400: %s", code, raw)
		}
		code, raw = do(t, srv, "GET", "/api/v1/proposals/deadbeefdeadbeef", "")
		if code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", code, raw)
		}
	})
}

// TestTheProposerCannotSupplyTheVerdict. The request body carries the QUESTION
// — window and axes — and no scorecard, no gate and no author. Store.Create
// runs Decide itself over scorecards this brain produced from its own retained
// history. A body that grows any of those fields is a 400, not a silently
// ignored field.
func TestTheProposerCannotSupplyTheVerdict(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	s, srv := whatIfServer(t, b)

	window := fmt.Sprintf(`"cluster":"c1","from":%q,"to":%q,"set":{"memory-headroom":1.05}`,
		wiFrom.Format(time.RFC3339), wiTo.Format(time.RFC3339))
	for _, smuggled := range []string{
		`"gate":{"passed":true}`,
		`"author":{"kind":"human","id":"alice"}`,
		`"envelope":{"bounds":{}}`,
		`"tolerance":{"minRegretImprovementUSD":0}`,
		`"baselineScore":{"regretUSD":99}`,
		`"candidateScore":{"regretUSD":0}`,
		`"state":"approved"`,
		`"approval":{"by":{"kind":"human","id":"alice"}}`,
	} {
		code, raw := do(t, srv, "POST", "/api/v1/proposals", "{"+window+","+smuggled+"}")
		if code != http.StatusBadRequest {
			t.Fatalf("body carrying %s returned %d, want 400: %s", smuggled, code, raw)
		}
		if len(s.props.List()) != 0 {
			t.Fatalf("body carrying %s filed something", smuggled)
		}
	}

	// A candidate the gate rejects is STILL filed, in rejected: a rejected
	// proposal is the record of a question that was asked and answered, and
	// discarding it would make a loop that ran look like one that never did.
	rec := filedProposal(t, srv, `"cpu-headroom":1.05`)
	if rec["state"] != string(whatif.StateRejected) {
		t.Fatalf("state %v, want rejected", rec["state"])
	}
	proposal := rec["proposal"].(map[string]any)
	gate := proposal["gate"].(map[string]any)
	if gate["passed"] != false {
		t.Fatalf("the gate passed a candidate the tolerance rejects: %v", gate)
	}
	if reasons, _ := gate["reasons"].([]any); len(reasons) == 0 {
		t.Fatal("a rejection with no reason is not a record of anything")
	}
	// The author is the funnel, never a human and never request-supplied.
	author := proposal["author"].(map[string]any)
	if author["kind"] != string(whatif.ActorSystem) || author["id"] != whatIfAuthor.ID {
		t.Fatalf("author %v, want %v", author, whatIfAuthor)
	}
}

// TestFilingTheSameProposalTwiceIsOneProposal. Proposals are content-addressed
// and CreatedAt is outside the fingerprint, so a nightly loop that re-derives
// the same candidate is idempotent rather than a duplicate factory — which is
// also why this route answers 200 and never 201.
func TestFilingTheSameProposalTwiceIsOneProposal(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	s, srv := whatIfServer(t, b)
	first := filedProposal(t, srv, `"memory-headroom":1.05`)
	s.now = whatif.FixedClock(t0.Add(600 * time.Hour)) // a different night
	second := filedProposal(t, srv, `"memory-headroom":1.05`)
	if first["id"] != second["id"] {
		t.Fatalf("the same proposal filed twice has two ids: %v vs %v", first["id"], second["id"])
	}
	if n := len(s.props.List()); n != 1 {
		t.Fatalf("the store holds %d records after two identical filings", n)
	}
}

// TestRejectionMovesTheRecordAndConflictIsNotAnError404. Any actor may reject:
// refusing to make a change is always safe, so it needs no capability. That
// asymmetry is why this route exists and approvals do not.
func TestRejectionMovesTheRecordAndConflictIsNotAnError404(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	_, srv := whatIfServer(t, b)
	rec := filedProposal(t, srv, `"memory-headroom":1.05`)
	id := rec["id"].(string)

	code, raw := do(t, srv, "POST", "/api/v1/proposals/"+id+"/rejections", `{"reason":"not this quarter"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	got := decodeMap(t, raw)
	if got["state"] != string(whatif.StateRejected) {
		t.Fatalf("state %v, want rejected", got["state"])
	}
	history, _ := got["history"].([]any)
	last := history[len(history)-1].(map[string]any)
	if last["note"] != "not this quarter" || last["to"] != string(whatif.StateRejected) {
		t.Fatalf("the audit trail does not record the rejection: %v", last)
	}

	// Rejecting a rejected proposal is a fact about the record's state, not
	// about the request, and not a missing resource.
	code, raw = do(t, srv, "POST", "/api/v1/proposals/"+id+"/rejections", `{"reason":"again"}`)
	if code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", code, raw)
	}
	code, raw = do(t, srv, "POST", "/api/v1/proposals/deadbeefdeadbeef/rejections", `{"reason":"x"}`)
	if code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", code, raw)
	}
	code, raw = do(t, srv, "POST", "/api/v1/proposals/"+id+"/rejections", `{"by":{"kind":"human","id":"alice"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a rejection naming its own actor returned %d, want 400: %s", code, raw)
	}
}

// ---------------------------------------------------------------- refusals

// TestApprovalsAndAppliedAreRefusedByName. Both routes exist and refuse,
// rather than being absent: a 404 from the mux teaches a caller nothing, and
// the reason IS the answer here.
func TestApprovalsAndAppliedAreRefusedByName(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	s, srv := whatIfServer(t, b)
	rec := filedProposal(t, srv, `"memory-headroom":1.05`)
	id := rec["id"].(string)
	before, _ := s.props.Get(id)
	beforeBytes, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		path  string
		names []string
	}{
		{"/approvals", []string{"human token tier", "NewApprover", "rejections"}},
		{"/applied", []string{"LedgerEntry", "MarkApplied", "approvals"}},
	} {
		code, raw := do(t, srv, "POST", "/api/v1/proposals/"+id+c.path, `{}`)
		if code != http.StatusNotImplemented {
			t.Fatalf("%s: status %d, want 501: %s", c.path, code, raw)
		}
		msg := errorText(t, raw)
		for _, name := range c.names {
			if !strings.Contains(msg, name) {
				t.Fatalf("%s refusal does not name %q: %s", c.path, name, msg)
			}
		}
		if !strings.Contains(msg, id) {
			t.Fatalf("%s refusal does not name the proposal it refused: %s", c.path, msg)
		}
	}

	// The refused calls moved nothing: same state, same approval (none), same
	// audit trail. A refusal that half-wrote is worse than one that shouted.
	after, _ := s.props.Get(id)
	afterBytes, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(beforeBytes) {
		t.Fatalf("a refused call changed the record:\nbefore %s\nafter  %s", beforeBytes, afterBytes)
	}
	if after.State() != whatif.StateGated {
		t.Fatalf("state %s after two refusals, want gated", after.State())
	}
	if _, ok := after.Approval(); ok {
		t.Fatal("a refused approval minted an approval")
	}
}

// TestNoAPIPathReachesApprovedOrApplied is the aggregate property, stated over
// the bytes rather than over the code: after any sequence of requests this
// surface accepts, the store contains no approved record, no applied record,
// no approval and no human actor.
func TestNoAPIPathReachesApprovedOrApplied(t *testing.T) {
	b, _ := whatIfBrain(t, wiSnapshots)
	s, srv := whatIfServer(t, b)
	gated := filedProposal(t, srv, `"memory-headroom":1.05`)["id"].(string)
	rejected := filedProposal(t, srv, `"cpu-headroom":1.05`)["id"].(string)

	for _, req := range []struct{ method, path, body string }{
		{"POST", "/api/v1/proposals/" + gated + "/approvals", `{}`},
		{"POST", "/api/v1/proposals/" + gated + "/approvals", `{"by":{"kind":"human","id":"alice"}}`},
		{"POST", "/api/v1/proposals/" + gated + "/applied", `{}`},
		{"POST", "/api/v1/proposals/" + rejected + "/approvals", `{}`},
		{"POST", "/api/v1/proposals/" + rejected + "/applied", `{}`},
		{"POST", "/api/v1/proposals/" + rejected + "/rejections", `{"reason":"no"}`},
		{"GET", "/api/v1/proposals", ""},
		{"GET", "/api/v1/proposals/" + gated, ""},
	} {
		do(t, srv, req.method, req.path, req.body)
	}

	raw, err := s.props.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"approved"`, `"applied"`, `"approval"`, `"human"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the proposal store contains %s after an API-only session:\n%s", forbidden, raw)
		}
	}
	// It reloads, which is the tamper check pkg/whatif runs on the way in.
	if _, err := whatif.Load(raw); err != nil {
		t.Fatalf("the store this surface wrote does not load back: %v", err)
	}
}

// TestNoCodePathInThisPackageApprovesOrApplies reads the package rather than
// the prose. The refusals above are text; this is the fact behind them —
// pkg/api contains no call to the three functions that could move a proposal
// past the gate, so the refusal cannot rot into a comment about code that
// changed.
func TestNoCodePathInThisPackageApprovesOrApplies(t *testing.T) {
	forbidden := map[string]string{
		"NewApprover": "mints the capability to approve",
		"Approve":     "moves a proposal to approved",
		"MarkApplied": "records a change as applied",
	}
	fset := token.NewFileSet()
	for _, path := range goFilesIn(t, ".") {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if why, bad := forbidden[sel.Sel.Name]; bad {
				t.Errorf("%s calls %s, which %s", fset.Position(call.Pos()), sel.Sel.Name, why)
			}
			return true
		})
	}
}

// TestTheAPIPackageCannotReachAnActuator.
//
// pkg/ec2/actuate*.go and pkg/rds/actuate*.go exist and are deliberately
// unreachable; making them reachable is a separate decision that is not this
// unit's to take. A what-if route is a HYPOTHETICAL, and the cheapest
// guarantee that a hypothetical cannot become an action is that the code which
// could act is not in the import graph at all.
//
// Writing this test found that the blanket form of that claim is FALSE for
// pkg/api as a whole, and the honest statement is narrower. pkg/api reaches
// pkg/rds — pkg/actuate -> pkg/provider -> pkg/rds — because ledger.go and
// explainroutes.go import pkg/actuate for actuate.StatusDone. That predates
// this unit and is not this unit's to remove, so it is pinned here instead of
// papered over: the path is asserted to run only through pkg/actuate, and no
// file in pkg/api may import an actuator directly, which is what makes an
// Actuator un-nameable in this package.
//
// What is asserted without qualification is the part this unit is responsible
// for: the what-if plane's own closure reaches neither actuator, and pkg/api
// never reaches pkg/ec2 at all.
func TestTheAPIPackageCannotReachAnActuator(t *testing.T) {
	const root = "../.."
	const ec2, rds = "github.com/agenticode/kilter/pkg/ec2", "github.com/agenticode/kilter/pkg/rds"

	// Not a vacuous test: the packages exist and really do carry actuators.
	for _, dir := range []string{"pkg/ec2", "pkg/rds"} {
		raw, err := os.ReadFile(filepath.Join(root, dir, "actuate.go"))
		if err != nil {
			t.Fatalf("%s/actuate.go: %v", dir, err)
		}
		for _, decl := range []string{"func NewActuator(", "func (a *Actuator) Execute("} {
			if !strings.Contains(string(raw), decl) {
				t.Fatalf("%s/actuate.go no longer declares %q; this test is naming something that moved",
					dir, decl)
			}
		}
	}

	// 1. The what-if plane itself. Everything whatifroutes.go pulls in,
	//    transitively, reaches neither actuator: a route in this file cannot
	//    call one even indirectly.
	plane := closureOfImports(t, root, fileImports(t, "whatifroutes.go"), nil)
	for _, banned := range []string{ec2, rds} {
		if path, reached := plane[banned]; reached {
			t.Fatalf("the what-if plane reaches %s via %s", banned, path)
		}
	}

	// 2. pkg/api as a whole never reaches the EC2 actuator.
	api := closureOfImports(t, root, packageImports(t, "."), nil)
	if path, reached := api[ec2]; reached {
		t.Fatalf("pkg/api reaches %s via %s", ec2, path)
	}

	// 3. It does reach the RDS package, and only through pkg/actuate. Treat
	//    pkg/actuate as a leaf and the reachability disappears — which is the
	//    proof that the ledger's StatusDone import is the whole of it, and
	//    that nothing else in this package (including this unit) opened a
	//    second door.
	withoutActuate := closureOfImports(t, root, packageImports(t, "."),
		map[string]bool{"github.com/agenticode/kilter/pkg/actuate": true})
	if path, reached := withoutActuate[rds]; reached {
		t.Fatalf("pkg/api reaches %s by a route other than pkg/actuate: %s", rds, path)
	}

	// 4. No file here imports an actuator directly, so no Actuator value can
	//    be named, constructed or called in this package at all.
	for _, file := range goFilesIn(t, ".") {
		for _, imp := range fileImports(t, file) {
			if imp == ec2 || imp == rds {
				t.Fatalf("%s imports %s directly", file, imp)
			}
		}
	}

	// 5. And the what-if plane's own file imports nothing that can talk to a
	//    cloud API.
	for _, banned := range []string{
		"github.com/agenticode/kilter/pkg/actuate",
		ec2, rds,
		"github.com/agenticode/kilter/pkg/provider",
		"github.com/agenticode/kilter/pkg/collect",
	} {
		for _, got := range fileImports(t, "whatifroutes.go") {
			if got == banned {
				t.Fatalf("whatifroutes.go imports %s", banned)
			}
		}
	}
}

// closureOfImports maps every intra-repo package reachable from the given
// import list to the path that reached it. Packages in stop are recorded but
// not descended into.
func closureOfImports(t *testing.T, root string, imports []string, stop map[string]bool) map[string]string {
	t.Helper()
	const prefix = "github.com/agenticode/kilter/"
	seen := map[string]string{}
	var walk func(imports []string, trail string)
	walk = func(imports []string, trail string) {
		for _, imp := range imports {
			if !strings.HasPrefix(imp, prefix) {
				continue
			}
			if _, ok := seen[imp]; ok {
				continue
			}
			seen[imp] = trail + " -> " + imp
			if stop[imp] {
				continue
			}
			walk(packageImports(t, filepath.Join(root, strings.TrimPrefix(imp, prefix))), seen[imp])
		}
	}
	walk(imports, "pkg/api")
	return seen
}

// packageImports is every import of a package directory's non-test files.
func packageImports(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, file := range goFilesIn(t, dir) {
		out = append(out, fileImports(t, file)...)
	}
	sort.Strings(out)
	return out
}

// goFilesIn lists a package directory's non-test Go files, sorted.
func goFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

func fileImports(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		out = append(out, strings.Trim(imp.Path.Value, `"`))
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- wiring

// TestWhatIfRoutesAreServedByTheBrainHandler checks the one thing the tests
// above cannot: that Handler() registers this plane at all, and that it sits
// behind the same auth as everything else — reads on the read token, writes on
// the write token only.
func TestWhatIfRoutesAreServedByTheBrainHandler(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b, err := NewBrain(BrainConfig{Token: "write-tok", ReadToken: "read-tok", CheckpointEvery: 1000},
		pricing.Embedded(), st)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < wiSnapshots; i++ {
		if err := b.Ingest(whatIfSnapshot("c1", wiFrom.Add(time.Duration(i)*24*time.Hour), 1)); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)

	call := func(method, path, token string) int {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	whatIfPath := whatIfURL("set=memory-headroom=1.05")
	if got := call("GET", whatIfPath, ""); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated what-if returned %d, want 401", got)
	}
	if got := call("GET", whatIfPath, "read-tok"); got != http.StatusOK {
		t.Fatalf("read-token what-if returned %d, want 200", got)
	}
	if got := call("GET", "/api/v1/proposals", "read-tok"); got != http.StatusOK {
		t.Fatalf("read-token proposals list returned %d, want 200", got)
	}
	// The read token must not be able to file, reject, approve or apply.
	for _, path := range []string{
		"/api/v1/proposals",
		"/api/v1/proposals/deadbeefdeadbeef/rejections",
		"/api/v1/proposals/deadbeefdeadbeef/approvals",
		"/api/v1/proposals/deadbeefdeadbeef/applied",
	} {
		if got := call("POST", path, "read-tok"); got != http.StatusUnauthorized {
			t.Fatalf("POST %s with the read token returned %d, want 401", path, got)
		}
	}
	if got := call("POST", "/api/v1/proposals/deadbeefdeadbeef/approvals", "write-tok"); got != http.StatusNotImplemented {
		t.Fatalf("approvals with the write token returned %d, want 501", got)
	}
}
