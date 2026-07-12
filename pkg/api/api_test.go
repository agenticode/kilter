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

func TestClientValidation(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "ftp://x", "http://"} {
		if _, err := NewClient(bad, ""); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

var _ = plan.Plan{} // keep import if assertions above change
