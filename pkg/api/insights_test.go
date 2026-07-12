package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// riskySnapshot: memory peak within 5% of the limit → critical oom-risk.
func riskySnapshot(cluster string, ts time.Time) *model.ClusterSnapshot {
	ref := model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "hot"}
	key := model.ContainerKey{Workload: ref, Container: "app"}
	snap := &model.ClusterSnapshot{
		ClusterID: cluster, Timestamp: ts,
		Nodes: []model.NodeSpec{{
			Name: "n1", Ready: true,
			Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
			Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		}},
		Pods: []model.PodSpec{{
			UID: "u1", Name: "hot-1", Namespace: "prod", NodeName: "n1", Phase: "Running",
			Workload: ref,
			Containers: []model.ContainerSpec{{
				Name:     "app",
				Requests: model.Resources{MilliCPU: 100, MemoryBytes: 256 << 20},
				Limits:   model.Resources{MemoryBytes: 512 << 20},
			}},
		}},
	}
	for i := 0; i < 30; i++ {
		snap.Usage = append(snap.Usage, model.Usage{
			Key: key, PodUID: "u1",
			Timestamp: ts.Add(time.Duration(i-30) * time.Minute),
			MilliCPU:  50, MemoryBytes: 500 << 20, // peak ~98% of limit
		})
	}
	return snap
}

func TestInsightsOverHTTP(t *testing.T) {
	b, _ := newBrain(t, "", false)
	if err := b.Ingest(riskySnapshot("prod", t0)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	client, _ := NewClient(srv.URL, "")
	ins, err := client.GetInsights(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range ins {
		if i.Kind == "oom-risk" && i.Severity == "critical" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected critical oom-risk, got %+v", ins)
	}
	// Unknown cluster → 404.
	if _, err := client.GetInsights(context.Background(), "ghost"); err == nil {
		t.Fatal("unknown cluster must error")
	}
}

func TestCapacityExhaustionForecast(t *testing.T) {
	b, _ := newBrain(t, "", false)
	// Demand ramps from 55% → ~85% of 4000m allocatable over 30 ingests;
	// the trend forecast must cross the 85% warning line within 24h.
	for i := 0; i < 30; i++ {
		snap := riskySnapshot("prod", t0.Add(time.Duration(i)*time.Minute))
		snap.Usage = snap.Usage[:0]
		key := model.ContainerKey{
			Workload:  model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "hot"},
			Container: "app",
		}
		snap.Usage = append(snap.Usage, model.Usage{
			Key: key, PodUID: "u1", Timestamp: snap.Timestamp,
			MilliCPU: 2200 + int64(i)*40, MemoryBytes: 1 << 30,
		})
		if err := b.Ingest(snap); err != nil {
			t.Fatal(err)
		}
	}
	ins, err := b.Insights(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range ins {
		if i.Kind == "capacity-exhaustion" {
			found = true
			if i.HorizonHours != 24 {
				t.Fatalf("horizon = %v", i.HorizonHours)
			}
		}
	}
	if !found {
		t.Fatalf("ramping demand must forecast capacity exhaustion: %+v", ins)
	}
}

func TestRemoteForecasterUsedAndFallback(t *testing.T) {
	var calls atomic.Int32
	fcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Series  []float64 `json:"series"`
			Horizon int       `json:"horizon"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		out := make([]float64, req.Horizon)
		for i := range out {
			out[i] = 3900 // predict near-total cpu exhaustion (4000m allocatable)
		}
		json.NewEncoder(w).Encode(map[string]any{"forecast": out})
	}))
	defer fcSrv.Close()

	b, err := NewBrain(BrainConfig{ForecasterURL: fcSrv.URL}, pricing.Embedded(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		snap := riskySnapshot("prod", t0.Add(time.Duration(i)*time.Minute))
		if err := b.Ingest(snap); err != nil {
			t.Fatal(err)
		}
	}
	ins, err := b.Insights(context.Background(), "prod")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("remote forecaster was never called")
	}
	foundCritical := false
	for _, i := range ins {
		if i.Kind == "capacity-exhaustion" && i.Severity == "critical" {
			foundCritical = true
		}
	}
	if !foundCritical {
		t.Fatalf("remote forecast of 3900m/4000m must yield critical exhaustion: %+v", ins)
	}

	// Kill the model server: the brain must fall back to built-in models
	// without erroring.
	fcSrv.Close()
	if _, err := b.Insights(context.Background(), "prod"); err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
}

func TestRemoteForecasterValidation(t *testing.T) {
	if _, err := NewBrain(BrainConfig{ForecasterURL: "not-a-url"}, pricing.Embedded(), nil); err == nil {
		t.Fatal("bad forecaster url must be rejected")
	}
}

func TestUIServed(t *testing.T) {
	b, _ := newBrain(t, "with-token", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ui → %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}
	// Root redirects to the UI.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, _ := client.Get(srv.URL + "/")
	r2.Body.Close()
	if r2.StatusCode != http.StatusFound || r2.Header.Get("Location") != "/ui" {
		t.Fatalf("root: %d → %q", r2.StatusCode, r2.Header.Get("Location"))
	}
}

func TestReadOnlyToken(t *testing.T) {
	b, err := NewBrain(BrainConfig{Token: "admin", ReadToken: "viewer"}, pricing.Embedded(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Ingest(riskySnapshot("prod", t0))
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	viewer, _ := NewClient(srv.URL, "viewer")
	if _, err := viewer.GetInsights(context.Background(), "prod"); err != nil {
		t.Fatalf("read token must read: %v", err)
	}
	if err := viewer.PushSnapshot(context.Background(), riskySnapshot("prod", t0)); err == nil {
		t.Fatal("read token must NOT ingest")
	}
	admin, _ := NewClient(srv.URL, "admin")
	if err := admin.PushSnapshot(context.Background(), riskySnapshot("prod", t0)); err != nil {
		t.Fatalf("admin token must ingest: %v", err)
	}
}
