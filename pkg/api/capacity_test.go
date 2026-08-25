package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/forecast"
	"github.com/agenticode/kilter/pkg/model"
)

func demandSnap(ts time.Time, cpuMilli, memBytes int64) *model.ClusterSnapshot {
	return &model.ClusterSnapshot{
		ClusterID: "c", Timestamp: ts,
		Usage: []model.Usage{{MilliCPU: cpuMilli, MemoryBytes: memBytes, Timestamp: ts}},
	}
}

func TestAppendCapped(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		v    float64
		cap  int
		want []float64
	}{
		{"empty", nil, 1, 3, []float64{1}},
		{"under cap", []float64{1, 2}, 3, 3, []float64{1, 2, 3}},
		{"at cap drops oldest", []float64{1, 2, 3}, 4, 3, []float64{2, 3, 4}},
		{"cap of one keeps newest", []float64{9}, 10, 1, []float64{10}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendCapped(append([]float64(nil), tt.in...), tt.v, tt.cap)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
	// Property: after any number of appends, len ≤ cap and the newest values win.
	var s []float64
	for i := 0; i < demandHistCap*3; i++ {
		s = appendCapped(s, float64(i), demandHistCap)
		if len(s) > demandHistCap {
			t.Fatalf("cap exceeded at i=%d: len=%d", i, len(s))
		}
	}
	if s[len(s)-1] != float64(demandHistCap*3-1) || s[0] != float64(demandHistCap*2) {
		t.Fatalf("window wrong: first=%v last=%v", s[0], s[len(s)-1])
	}
}

func TestMaxOf(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"mixed", []float64{3, 9, 1}, 9},
		{"all negative floors at zero", []float64{-5, -1}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maxOf(tt.in); got != tt.want {
				t.Fatalf("maxOf(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDemandTrackerObserve(t *testing.T) {
	type obs struct {
		offMin   int
		cpu, mem int64
	}
	tests := []struct {
		name         string
		obs          []obs
		wantPoints   int
		wantInterval time.Duration
	}{
		{"zero-usage snapshot ignored",
			[]obs{{0, 0, 0}}, 0, 0},
		{"cadence learned from in-order gaps",
			[]obs{{0, 100, 1}, {5, 100, 1}}, 2, 5 * time.Minute},
		{"gap of an hour or more excluded from cadence",
			[]obs{{0, 100, 1}, {120, 100, 1}}, 2, 0},
		{"duplicate timestamps never yield an interval",
			[]obs{{0, 100, 1}, {0, 100, 1}, {0, 100, 1}}, 3, 0},
		// The replayed 1-minute snapshot must not drag the cadence clock
		// backwards: the 10-minute sample's gap is 5m (from the 5m sample),
		// not 9m (from the replay).
		{"out-of-order replay does not corrupt cadence",
			[]obs{{0, 100, 1}, {5, 100, 1}, {1, 100, 1}, {10, 100, 1}}, 4, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDemandTracker()
			for _, o := range tt.obs {
				d.observe(demandSnap(t0.Add(time.Duration(o.offMin)*time.Minute), o.cpu, o.mem))
			}
			d.mu.Lock()
			points, interval := d.points, d.interval
			d.mu.Unlock()
			if points != tt.wantPoints {
				t.Errorf("points = %d, want %d", points, tt.wantPoints)
			}
			if interval != tt.wantInterval {
				t.Errorf("interval = %v, want %v", interval, tt.wantInterval)
			}
		})
	}
}

func TestForecastPeakGating(t *testing.T) {
	ctx := context.Background()

	// Fewer than 10 points → no forecast.
	d := newDemandTracker()
	for i := 0; i < 9; i++ {
		d.observe(demandSnap(t0.Add(time.Duration(i*5)*time.Minute), 100, 1<<30))
	}
	if _, _, ok := d.forecastPeak(ctx, nil, 24*time.Hour); ok {
		t.Fatal("forecast must not fire below 10 points")
	}

	// Enough points but no cadence (identical timestamps) → no forecast.
	d2 := newDemandTracker()
	for i := 0; i < 12; i++ {
		d2.observe(demandSnap(t0, 100, 1<<30))
	}
	if _, _, ok := d2.forecastPeak(ctx, nil, 24*time.Hour); ok {
		t.Fatal("forecast must not fire without a cadence estimate")
	}

	// A ramp with steady cadence forecasts a peak above the last observation.
	d3 := newDemandTracker()
	last := int64(0)
	for i := 0; i < 20; i++ {
		last = 100 + int64(i)*50
		d3.observe(demandSnap(t0.Add(time.Duration(i*5)*time.Minute), last, 1<<30))
	}
	cpu, mem, ok := d3.forecastPeak(ctx, nil, 24*time.Hour)
	if !ok {
		t.Fatal("ramp with cadence must forecast")
	}
	if cpu <= float64(last) {
		t.Fatalf("rising trend must peak above last observation: peak %v, last %d", cpu, last)
	}
	if mem <= 0 {
		t.Fatalf("mem peak = %v", mem)
	}
}

// TestForecastPeakDoesNotBlockObserve pins the invariant that a slow remote
// forecaster can never stall snapshot ingestion: observe() and forecastPeak()
// share a mutex, so the remote HTTP call must happen outside it.
func TestForecastPeakDoesNotBlockObserve(t *testing.T) {
	var enterOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	fcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enterOnce.Do(func() { close(entered) })
		<-release
		json.NewEncoder(w).Encode(map[string]any{"forecast": []float64{1}})
	}))
	defer fcSrv.Close()
	rf, err := forecast.NewRemoteForecaster(fcSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	d := newDemandTracker()
	for i := 0; i < 12; i++ {
		d.observe(demandSnap(t0.Add(time.Duration(i*5)*time.Minute), 100, 1<<30))
	}
	peakDone := make(chan struct{})
	go func() {
		defer close(peakDone)
		d.forecastPeak(context.Background(), rf, 24*time.Hour)
	}()
	<-entered // the remote call is now in flight

	obsDone := make(chan struct{})
	go func() {
		defer close(obsDone)
		d.observe(demandSnap(t0.Add(time.Hour), 100, 1<<30))
	}()
	select {
	case <-obsDone:
		// ingest stayed live while the forecaster hung — invariant holds
	case <-time.After(2 * time.Second):
		t.Error("observe blocked behind a hung remote forecaster")
	}
	close(release)
	<-peakDone
}

func TestCapacityInsightsNilSafety(t *testing.T) {
	if got := capacityInsights(context.Background(), nil, nil, demandSnap(t0, 1, 1)); got != nil {
		t.Fatalf("nil tracker must yield no insights, got %+v", got)
	}
	if got := capacityInsights(context.Background(), newDemandTracker(), nil, nil); got != nil {
		t.Fatalf("nil snapshot must yield no insights, got %+v", got)
	}
	// A cluster with no schedulable capacity cannot produce a ratio.
	d := newDemandTracker()
	for i := 0; i < 12; i++ {
		d.observe(demandSnap(t0.Add(time.Duration(i*5)*time.Minute), 100, 1<<30))
	}
	snap := demandSnap(t0.Add(time.Hour), 100, 1<<30) // no nodes at all
	if got := capacityInsights(context.Background(), d, nil, snap); got != nil {
		t.Fatalf("zero allocatable must yield no insights, got %+v", got)
	}
}
