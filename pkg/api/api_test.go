package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/store"
)

var t0 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

// trainingSnapshot carries 24h of usage so recommendations fire immediately.
func trainingSnapshot(cluster string) *model.ClusterSnapshot {
	ref := model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "web"}
	key := model.ContainerKey{Workload: ref, Container: "app"}
	snap := &model.ClusterSnapshot{
		ClusterID: cluster, Timestamp: t0.Add(24 * time.Hour),
		Nodes: []model.NodeSpec{
			{Name: "n1", Ready: true, InstanceType: "m5.xlarge", Provider: "aws",
				Labels:      map[string]string{"kubernetes.io/hostname": "n1", "kubernetes.io/arch": "amd64"},
				Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
				Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30}},
			{Name: "n2", Ready: true, InstanceType: "m5.xlarge", Provider: "aws",
				Labels:      map[string]string{"kubernetes.io/hostname": "n2", "kubernetes.io/arch": "amd64"},
				Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
				Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30}},
		},
		Pods: []model.PodSpec{{
			UID: "u1", Name: "web-1", Namespace: "prod", NodeName: "n1", Phase: "Running",
			Labels: map[string]string{"app": "web"}, Workload: ref,
			Containers: []model.ContainerSpec{{Name: "app",
				Requests: model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}}},
		}},
	}
	for i := 0; i < 288; i++ {
		snap.Usage = append(snap.Usage, model.Usage{
			Key: key, PodUID: "u1",
			Timestamp: t0.Add(time.Duration(i*5) * time.Minute),
			MilliCPU:  150, MemoryBytes: 400 << 20,
		})
	}
	return snap
}

func newBrain(t *testing.T, token string, withStore bool) (*Brain, *store.Store) {
	t.Helper()
	var st *store.Store
	if withStore {
		var err error
		st, err = store.Open(filepath.Join(t.TempDir(), "brain.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
	}
	b, err := NewBrain(BrainConfig{Token: token, CheckpointEvery: 1}, pricing.Embedded(), st)
	if err != nil {
		t.Fatal(err)
	}
	return b, st
}

func TestIngestAndRecommendOverHTTP(t *testing.T) {
	b, _ := newBrain(t, "", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	client, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if !client.Healthy(context.Background()) {
		t.Fatal("healthz failed")
	}
	if err := client.PushSnapshot(context.Background(), trainingSnapshot("prod")); err != nil {
		t.Fatal(err)
	}
	recs, err := client.GetRecommendations(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 recommendation, got %d", len(recs))
	}
	if recs[0].TargetRequest.MilliCPU >= 2000 {
		t.Fatalf("should shrink cpu: %+v", recs[0].TargetRequest)
	}

	p, err := client.GetPlan(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if p.ClusterID != "prod" || p.CurrentHourlyUSD <= 0 {
		t.Fatalf("plan wrong: %+v", p)
	}
	// Rightsizing shrinks web-1 to ~<400m → node n1 (only pod) removable.
	if len(p.Removals) != 1 {
		t.Fatalf("expected 1 node removal, got %+v", p.Removals)
	}
	if p.SavingsMonthlyUSD <= 0 {
		t.Fatal("savings must be positive")
	}
}

func TestAuthEnforced(t *testing.T) {
	b, _ := newBrain(t, "sekrit", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	// No token → 401.
	resp, err := http.Get(srv.URL + "/api/v1/clusters")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	// healthz stays open.
	resp2, _ := http.Get(srv.URL + "/healthz")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatal("healthz must not require auth")
	}
	// Correct token via client works.
	client, _ := NewClient(srv.URL, "sekrit")
	if err := client.PushSnapshot(context.Background(), trainingSnapshot("prod")); err != nil {
		t.Fatal(err)
	}
	// Wrong token fails without retries.
	bad, _ := NewClient(srv.URL, "wrong")
	if err := bad.PushSnapshot(context.Background(), trainingSnapshot("prod")); err == nil {
		t.Fatal("wrong token must fail")
	}
}

func TestIngestRejectsGarbage(t *testing.T) {
	b, _ := newBrain(t, "", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	post := func(body []byte, enc string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/snapshots", bytes.NewReader(body))
		if enc != "" {
			req.Header.Set("Content-Encoding", enc)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post([]byte("not json"), ""); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
	if code := post([]byte("not gzip"), "gzip"); code != http.StatusBadRequest {
		t.Fatalf("bad gzip: %d", code)
	}
	if code := post([]byte(`{"clusterID":""}`), ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing cluster id: %d", code)
	}
}

func TestBodySizeLimit(t *testing.T) {
	b, err := NewBrain(BrainConfig{MaxBodyBytes: 1024}, pricing.Embedded(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	big, _ := json.Marshal(trainingSnapshot("prod")) // ≫ 1 KiB
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/snapshots", bytes.NewReader(big))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted {
		t.Fatal("oversized body must be rejected")
	}

	// Zip bomb: tiny compressed, huge decompressed → also rejected.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(bytes.Repeat([]byte("A"), 1<<20))
	gz.Close()
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/snapshots", &buf)
	req2.Header.Set("Content-Encoding", "gzip")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusAccepted {
		t.Fatal("zip bomb must be rejected")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "brain.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := NewBrain(BrainConfig{CheckpointEvery: 1}, pricing.Embedded(), st)
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.Ingest(trainingSnapshot("prod")); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	b2, err := NewBrain(BrainConfig{}, pricing.Embedded(), st2)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := b2.Recommendations("prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("learning lost across restart: %d recs", len(recs))
	}
}

func TestMetricsExposed(t *testing.T) {
	b, _ := newBrain(t, "with-token-metrics-still-open", false)
	b.Ingest(trainingSnapshot("prod"))
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	body := buf.String()
	for _, want := range []string{"kilter_snapshots_received_total", "kilter_cluster_cost_hourly_usd"} {
		if !bytes.Contains([]byte(body), []byte(want)) {
			t.Fatalf("metric %s missing", want)
		}
	}
}

func TestConcurrentIngest(t *testing.T) {
	b, _ := newBrain(t, "", true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				snap := trainingSnapshot(fmt.Sprintf("c%d", i%3))
				if err := b.Ingest(snap); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	if got := len(b.Clusters()); got != 3 {
		t.Fatalf("clusters: %d", got)
	}
}

// TestAuthAdversarialTokens: near-miss credentials — prefixes, extensions,
// case-twiddled schemes — must all be rejected, and the read token must never
// unlock a write route.
func TestAuthAdversarialTokens(t *testing.T) {
	b, err := NewBrain(BrainConfig{Token: "sekrit-token", ReadToken: "read-token"}, pricing.Embedded(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	get := func(header string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/clusters", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	tests := []struct {
		name, header string
		want         int
	}{
		{"missing header", "", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"token prefix", "Bearer sekrit", http.StatusUnauthorized},
		{"token extended", "Bearer sekrit-token-x", http.StatusUnauthorized},
		{"lowercase scheme", "bearer sekrit-token", http.StatusUnauthorized},
		{"no scheme", "sekrit-token", http.StatusUnauthorized},
		{"write token", "Bearer sekrit-token", http.StatusOK},
		{"read token", "Bearer read-token", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := get(tt.header); got != tt.want {
				t.Fatalf("GET with %q → %d, want %d", tt.header, got, tt.want)
			}
		})
	}
	// Read token on a write route stays locked out.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/snapshots", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer read-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("read token on write route → %d, want 401", resp.StatusCode)
	}
}

func TestOversizedBodyReturns413(t *testing.T) {
	b, err := NewBrain(BrainConfig{MaxBodyBytes: 1024}, pricing.Embedded(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	big, _ := json.Marshal(trainingSnapshot("prod"))
	resp, err := http.Post(srv.URL+"/api/v1/snapshots", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body → %d, want 413", resp.StatusCode)
	}
	// A gzip bomb that stays syntactically valid JSON until cut off must hit
	// the decompressed-size bound and report 413, not a misleading 400.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(`{"clusterID":"`))
	gz.Write(bytes.Repeat([]byte("A"), 1<<20))
	gz.Write([]byte(`"}`))
	gz.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/snapshots", &buf)
	req.Header.Set("Content-Encoding", "gzip")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("zip bomb → %d, want 413", resp2.StatusCode)
	}
}

func TestCostEndpoint(t *testing.T) {
	b, _ := newBrain(t, "", false)
	b.Ingest(trainingSnapshot("prod"))
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/clusters/prod/cost")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cost → %d", resp.StatusCode)
	}
	var cost struct {
		HourlyUSD float64 `json:"hourlyUSD"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cost); err != nil {
		t.Fatal(err)
	}
	if cost.HourlyUSD <= 0 {
		t.Fatalf("2× m5.xlarge must cost > 0, got %v", cost.HourlyUSD)
	}
	r2, _ := http.Get(srv.URL + "/api/v1/clusters/ghost/cost")
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown cluster cost → %d, want 404", r2.StatusCode)
	}
}

func TestIngestNormalizesZeroTimestamp(t *testing.T) {
	b, _ := newBrain(t, "", false)
	snap := trainingSnapshot("prod")
	snap.Timestamp = time.Time{}
	if err := b.Ingest(snap); err != nil {
		t.Fatal(err)
	}
	rep := b.Ledger("prod")
	if len(rep.CostTimeline) != 1 {
		t.Fatalf("cost timeline points: %d", len(rep.CostTimeline))
	}
	if rep.CostTimeline[0].At.Before(t0) {
		t.Fatalf("zero timestamp must be normalized to ingest time, got %v", rep.CostTimeline[0].At)
	}
}

func TestClustersSorted(t *testing.T) {
	b, _ := newBrain(t, "", false)
	for _, id := range []string{"zeta", "alpha", "mike", "bravo"} {
		if err := b.Ingest(&model.ClusterSnapshot{ClusterID: id, Timestamp: t0}); err != nil {
			t.Fatal(err)
		}
	}
	got := b.Clusters()
	want := []string{"alpha", "bravo", "mike", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("clusters: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("clusters not sorted: %v", got)
		}
	}
}

// FuzzIngestEndpoint throws arbitrary bodies (optionally declared as gzip) at
// the ingest route: the server must always answer with a sane status and
// never panic, corrupt state, or accept a snapshot without a cluster id.
func FuzzIngestEndpoint(f *testing.F) {
	f.Add([]byte("not json"), false)
	f.Add([]byte(`{"clusterID":""}`), false)
	f.Add([]byte(`{"clusterID":"c","timestamp":"2026-01-01T00:00:00Z"}`), false)
	f.Add([]byte(`{"clusterID":"c","usage":[{"milliCPU":-99,"memoryBytes":-1}]}`), false)
	f.Add([]byte{0x1f, 0x8b, 0x00}, true) // truncated gzip magic
	var gzValid bytes.Buffer
	gw := gzip.NewWriter(&gzValid)
	gw.Write([]byte(`{"clusterID":"gz"}`))
	gw.Close()
	f.Add(gzValid.Bytes(), true)

	b, err := NewBrain(BrainConfig{MaxBodyBytes: 1 << 20}, pricing.Embedded(), nil)
	if err != nil {
		f.Fatal(err)
	}
	// In-process, not over TCP: fuzzing thousands of over-limit bodies through
	// real sockets exhausts ephemeral ports (each 413 closes the connection).
	handler := b.Handler()

	f.Fuzz(func(t *testing.T, body []byte, gzipped bool) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots", bytes.NewReader(body))
		if gzipped {
			req.Header.Set("Content-Encoding", "gzip")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusAccepted, http.StatusBadRequest,
			http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		default:
			t.Fatalf("unexpected status %d", rec.Code)
		}
	})
}

func TestClientValidation(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "ftp://x", "http://"} {
		if _, err := NewClient(bad, ""); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

var _ = plan.Plan{} // keep import if assertions above change
