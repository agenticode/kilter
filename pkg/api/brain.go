// Package api implements the brain's HTTP surface and the client used by
// agents and controllers. stdlib only, hardened for unattended operation:
// header/read/write timeouts, bounded bodies, optional bearer auth, gzip
// ingest, Prometheus metrics, graceful shutdown.
package api

import (
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/agenticode/kilter/pkg/forecast"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
	"github.com/agenticode/kilter/pkg/store"
)

//go:embed ui/index.html
var uiFS embed.FS

// BrainConfig tunes the central decision service.
type BrainConfig struct {
	// Token, when set, is required as "Authorization: Bearer <token>" on all
	// /api/ routes. /healthz and /metrics stay open.
	Token string
	// ReadToken optionally grants read-only access to GET routes (dashboards,
	// auditors) without the mutating-ingest token.
	ReadToken string
	// MaxBodyBytes bounds snapshot uploads (after transport decompression we
	// additionally bound the decoded stream). Default 64 MiB.
	MaxBodyBytes int64
	// CheckpointEvery persists recommender state every N snapshots per
	// cluster. Default 10.
	CheckpointEvery int
	// ForecasterURL points at an external time-series model server (e.g. a
	// Chronos/TimesFM wrapper) used for capacity forecasts. Optional; the
	// built-in statistical models are the default and the fallback.
	ForecasterURL string
	Recommend     recommend.Config
	Plan          plan.Config
	Logger        *slog.Logger
}

func (c BrainConfig) withDefaults() BrainConfig {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 64 << 20
	}
	if c.CheckpointEvery <= 0 {
		c.CheckpointEvery = 10
	}
	if c.Recommend == (recommend.Config{}) {
		c.Recommend = recommend.DefaultConfig()
	}
	if c.Plan == (plan.Config{}) {
		c.Plan = plan.DefaultConfig()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Brain is the central optimizer: it ingests snapshots, learns, and serves
// recommendations, plans and cost reports per cluster.
type Brain struct {
	cfg     BrainConfig
	catalog *pricing.Catalog
	st      *store.Store // optional; nil = memory-only

	mu        sync.RWMutex
	recs      map[string]*recommend.Recommender // per cluster
	lastSnap  map[string]*model.ClusterSnapshot
	snapCount map[string]int
	demand    map[string]*demandTracker
	ledgers   map[string]*ledgerState
	approvals map[string]*approvalState

	forecaster *forecast.RemoteForecaster // nil = built-in models only

	m brainMetrics
}

type brainMetrics struct {
	snapshots  *prometheus.CounterVec
	ingestSec  prometheus.Histogram
	containers *prometheus.GaugeVec
	costHourly *prometheus.GaugeVec
	savings    *prometheus.GaugeVec
	recCount   *prometheus.GaugeVec
	registry   *prometheus.Registry
}

func newBrainMetrics() brainMetrics {
	m := brainMetrics{
		snapshots: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kilter_snapshots_received_total", Help: "Snapshots ingested per cluster."}, []string{"cluster"}),
		ingestSec: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "kilter_snapshot_ingest_seconds", Help: "Snapshot ingest latency.",
			Buckets: prometheus.DefBuckets}),
		containers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kilter_tracked_containers", Help: "Container keys tracked per cluster."}, []string{"cluster"}),
		costHourly: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kilter_cluster_cost_hourly_usd", Help: "Estimated cluster cost per hour."}, []string{"cluster"}),
		savings: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kilter_plan_savings_monthly_usd", Help: "Savings of the latest plan."}, []string{"cluster"}),
		recCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kilter_recommendations", Help: "Active recommendations per cluster."}, []string{"cluster"}),
		registry: prometheus.NewRegistry(),
	}
	m.registry.MustRegister(m.snapshots, m.ingestSec, m.containers, m.costHourly, m.savings, m.recCount)
	return m
}

// NewBrain builds a brain, restoring persisted learning when a store is given.
func NewBrain(cfg BrainConfig, catalog *pricing.Catalog, st *store.Store) (*Brain, error) {
	if catalog == nil {
		return nil, errors.New("api: nil catalog")
	}
	b := &Brain{
		cfg:       cfg.withDefaults(),
		catalog:   catalog,
		st:        st,
		recs:      map[string]*recommend.Recommender{},
		lastSnap:  map[string]*model.ClusterSnapshot{},
		snapCount: map[string]int{},
		demand:    map[string]*demandTracker{},
		ledgers:   map[string]*ledgerState{},
		approvals: map[string]*approvalState{},
		m:         newBrainMetrics(),
	}
	if b.cfg.ForecasterURL != "" {
		rf, err := forecast.NewRemoteForecaster(b.cfg.ForecasterURL)
		if err != nil {
			return nil, err
		}
		b.forecaster = rf
	}
	if st != nil {
		clusters, err := st.Clusters()
		if err != nil {
			return nil, fmt.Errorf("api: restore clusters: %w", err)
		}
		for _, c := range clusters {
			r, err := b.recommenderFor(c)
			if err != nil {
				return nil, err
			}
			if states, err := st.LoadRecommenderState(c); err == nil && states != nil {
				r.Restore(states)
			}
			if snap, err := st.LoadSnapshot(c); err == nil && snap != nil {
				b.lastSnap[c] = snap
			}
		}
		b.cfg.Logger.Info("brain restored", "clusters", len(clusters))
	}
	return b, nil
}

// recommenderFor returns (creating if needed) the per-cluster recommender.
// Caller must not hold b.mu.
func (b *Brain) recommenderFor(cluster string) (*recommend.Recommender, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.recs[cluster]; ok {
		return r, nil
	}
	r, err := recommend.New(b.cfg.Recommend)
	if err != nil {
		return nil, err
	}
	b.recs[cluster] = r
	return r, nil
}

// Ingest processes one snapshot (also used directly by the embedded analyze
// path, bypassing HTTP).
func (b *Brain) Ingest(snap *model.ClusterSnapshot) error {
	if snap == nil || snap.ClusterID == "" {
		return errors.New("snapshot must carry a clusterID")
	}
	start := time.Now()
	r, err := b.recommenderFor(snap.ClusterID)
	if err != nil {
		return err
	}
	r.ObserveSnapshot(snap)

	b.mu.Lock()
	b.lastSnap[snap.ClusterID] = snap
	b.snapCount[snap.ClusterID]++
	count := b.snapCount[snap.ClusterID]
	dt := b.demand[snap.ClusterID]
	if dt == nil {
		dt = newDemandTracker()
		b.demand[snap.ClusterID] = dt
	}
	b.mu.Unlock()
	dt.observe(snap)

	if b.st != nil {
		if err := b.st.SaveSnapshot(snap); err != nil {
			b.cfg.Logger.Error("persist snapshot", "err", err)
		}
		if count%b.cfg.CheckpointEvery == 0 {
			if err := b.st.SaveRecommenderState(snap.ClusterID, r.Checkpoint()); err != nil {
				b.cfg.Logger.Error("persist recommender", "err", err)
			}
		}
	}

	cost := b.catalog.SnapshotCost(snap)
	b.ledgerFor(snap.ClusterID).addCost(snap.Timestamp, cost.HourlyUSD)
	b.m.snapshots.WithLabelValues(snap.ClusterID).Inc()
	b.m.containers.WithLabelValues(snap.ClusterID).Set(float64(r.StateCount()))
	b.m.costHourly.WithLabelValues(snap.ClusterID).Set(cost.HourlyUSD)
	b.m.ingestSec.Observe(time.Since(start).Seconds())
	return nil
}

// snapshotFor returns the latest snapshot for a cluster.
func (b *Brain) snapshotFor(cluster string) *model.ClusterSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastSnap[cluster]
}

// Recommendations computes current recommendations for a cluster.
func (b *Brain) Recommendations(cluster string) ([]recommend.Recommendation, error) {
	snap := b.snapshotFor(cluster)
	if snap == nil {
		return nil, fmt.Errorf("unknown cluster %q", cluster)
	}
	r, err := b.recommenderFor(cluster)
	if err != nil {
		return nil, err
	}
	recs := r.Recommendations(snap)
	b.m.recCount.WithLabelValues(cluster).Set(float64(len(recs)))
	return recs, nil
}

// Plan builds a fresh plan for a cluster and records it.
func (b *Brain) Plan(cluster string) (*plan.Plan, error) {
	snap := b.snapshotFor(cluster)
	if snap == nil {
		return nil, fmt.Errorf("unknown cluster %q", cluster)
	}
	recs, err := b.Recommendations(cluster)
	if err != nil {
		return nil, err
	}
	p, err := plan.Build(snap, recs, b.catalog, b.cfg.Plan)
	if err != nil {
		return nil, err
	}
	b.m.savings.WithLabelValues(cluster).Set(p.SavingsMonthlyUSD)
	if b.st != nil {
		if err := b.st.SavePlan(p); err != nil {
			b.cfg.Logger.Error("persist plan", "err", err)
		}
	}
	return p, nil
}

// Insights returns the detection layer's current findings for a cluster:
// workload-level predictions from the recommender plus cluster-level
// capacity-exhaustion forecasts.
func (b *Brain) Insights(ctx context.Context, cluster string) ([]model.Insight, error) {
	snap := b.snapshotFor(cluster)
	if snap == nil {
		return nil, fmt.Errorf("unknown cluster %q", cluster)
	}
	r, err := b.recommenderFor(cluster)
	if err != nil {
		return nil, err
	}
	out := r.Insights(snap)
	b.mu.RLock()
	dt := b.demand[cluster]
	b.mu.RUnlock()
	out = append(out, capacityInsights(ctx, dt, b.forecaster, snap)...)
	if rep := plan.BuildSpotReport(snap, b.catalog, 2); rep.EstMonthlySavingsUSD >= 10 {
		safe := 0
		for _, w := range rep.Workloads {
			if w.Safe {
				safe++
			}
		}
		out = append(out, model.Insight{
			Kind: "spot-opportunity", Severity: "info",
			Message: fmt.Sprintf("%d spot-safe workload(s) running on on-demand capacity — moving them could save ~$%.0f/month (%.0f%% typical spot discount)",
				safe, rep.EstMonthlySavingsUSD, rep.DiscountApplied*100),
			At: snap.Timestamp,
		})
	}
	return out, nil
}

// Clusters lists known cluster ids.
func (b *Brain) Clusters() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.lastSnap))
	for c := range b.lastSnap {
		out = append(out, c)
	}
	return out
}

// ---- HTTP ----

// Handler returns the brain's full HTTP handler.
func (b *Brain) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(b.m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, _ *http.Request) {
		// The page itself is public; every data call it makes goes through
		// the token-guarded API.
		raw, err := uiFS.ReadFile("ui/index.html")
		if err != nil {
			http.Error(w, "ui unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(raw)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})

	mux.HandleFunc("POST /api/v1/snapshots", b.authWrite(b.handleIngest))
	mux.HandleFunc("GET /api/v1/clusters", b.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"clusters": b.Clusters()})
	}))
	mux.HandleFunc("GET /api/v1/clusters/{id}/recommendations", b.auth(func(w http.ResponseWriter, r *http.Request) {
		recs, err := b.Recommendations(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recommendations": recs})
	}))
	mux.HandleFunc("GET /api/v1/clusters/{id}/plan", b.auth(func(w http.ResponseWriter, r *http.Request) {
		p, err := b.Plan(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}))
	mux.HandleFunc("GET /api/v1/clusters/{id}/insights", b.auth(func(w http.ResponseWriter, r *http.Request) {
		ins, err := b.Insights(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"insights": ins})
	}))
	b.registerTrustRoutes(mux)
	mux.HandleFunc("GET /api/v1/clusters/{id}/cost", b.auth(func(w http.ResponseWriter, r *http.Request) {
		snap := b.snapshotFor(r.PathValue("id"))
		if snap == nil {
			writeErr(w, http.StatusNotFound, fmt.Errorf("unknown cluster"))
			return
		}
		writeJSON(w, http.StatusOK, b.catalog.SnapshotCost(snap))
	}))
	return mux
}

// auth guards read routes: the write token or the read token both pass.
func (b *Brain) auth(next http.HandlerFunc) http.HandlerFunc {
	return b.authz(next, false)
}

// authWrite guards mutating routes: only the write token passes.
func (b *Brain) authWrite(next http.HandlerFunc) http.HandlerFunc {
	return b.authz(next, true)
}

func (b *Brain) authz(next http.HandlerFunc, write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b.cfg.Token != "" {
			got := r.Header.Get("Authorization")
			ok := got == "Bearer "+b.cfg.Token ||
				(!write && b.cfg.ReadToken != "" && got == "Bearer "+b.cfg.ReadToken)
			if !ok {
				writeErr(w, http.StatusUnauthorized, errors.New("invalid or missing token"))
				return
			}
		}
		next(w, r)
	}
}

func (b *Brain) handleIngest(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, b.cfg.MaxBodyBytes)
	var reader = body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad gzip: %w", err))
			return
		}
		defer gz.Close()
		reader = http.MaxBytesReader(w, readCloser{gz}, b.cfg.MaxBodyBytes)
	}
	var snap model.ClusterSnapshot
	dec := json.NewDecoder(reader)
	if err := dec.Decode(&snap); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode snapshot: %w", err))
		return
	}
	if err := b.Ingest(&snap); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "cluster": snap.ClusterID})
}

type readCloser struct{ *gzip.Reader }

func (readCloser) Close() error { return nil }

// decodeBody parses a bounded JSON request body, writing the error response
// itself on failure (callers just return).
func decodeBody(w http.ResponseWriter, r *http.Request, maxBytes int64, out any) error {
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(body).Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// Serve runs the brain HTTP server until ctx is cancelled, then shuts down
// gracefully.
func (b *Brain) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           b.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	b.cfg.Logger.Info("brain listening", "addr", addr)
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		// Final checkpoint on shutdown.
		if b.st != nil {
			b.mu.RLock()
			for cluster, r := range b.recs {
				_ = b.st.SaveRecommenderState(cluster, r.Checkpoint())
			}
			b.mu.RUnlock()
		}
		return nil
	case err := <-errCh:
		return err
	}
}
