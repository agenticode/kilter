package forecast

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRemoteForecasterValidation(t *testing.T) {
	bad := []string{"", "://x", "ftp://host/x", "http://", "not a url", "/relative/path"}
	for _, u := range bad {
		if _, err := NewRemoteForecaster(u); err == nil {
			t.Errorf("endpoint %q should be rejected", u)
		}
	}
	good := []string{"http://model-server:8080/forecast", "https://example.com"}
	for _, u := range good {
		if _, err := NewRemoteForecaster(u); err != nil {
			t.Errorf("endpoint %q should be accepted: %v", u, err)
		}
	}
}

func TestRemoteForecastHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Series  []float64 `json:"series"`
			Horizon int       `json:"horizon"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if len(req.Series) != 3 || req.Horizon != 2 {
			t.Errorf("unexpected payload: %+v", req)
		}
		json.NewEncoder(w).Encode(map[string]any{"forecast": []float64{4, 5}})
	}))
	defer srv.Close()

	rf, err := NewRemoteForecaster(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rf.Forecast(context.Background(), []float64{1, 2, 3}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("forecast = %v, want [4 5]", got)
	}
}

func TestRemoteForecastLengthContract(t *testing.T) {
	// The contract promises exactly `horizon` points. A short response would
	// silently under-cover the horizon (callers take max over the window), so
	// any length mismatch must be an error, not a partial answer.
	for _, n := range []int{1, 3, 5} {
		pts := make([]float64, n)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"forecast": pts})
		}))
		rf, _ := NewRemoteForecaster(srv.URL)
		_, err := rf.Forecast(context.Background(), []float64{1, 2}, 4)
		srv.Close()
		if err == nil {
			t.Errorf("%d points for horizon 4 must be rejected", n)
		}
	}
}

func TestRemoteForecastRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"negative", `{"forecast":[1,-2]}`},
		{"empty", `{"forecast":[]}`},
		{"missing field", `{}`},
		{"malformed", `{"forecast":[1,`},
		{"wrong type", `{"forecast":["a","b"]}`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(c.body))
		}))
		rf, _ := NewRemoteForecaster(srv.URL)
		_, err := rf.Forecast(context.Background(), []float64{1}, 2)
		srv.Close()
		if err == nil {
			t.Errorf("%s: body %q must be rejected", c.name, c.body)
		}
	}
}

func TestRemoteForecastServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	rf, _ := NewRemoteForecaster(srv.URL)
	if _, err := rf.Forecast(context.Background(), []float64{1}, 1); err == nil {
		t.Fatal("HTTP 500 must be an error")
	}
}

func TestRemoteForecastInvalidInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called for invalid inputs")
	}))
	defer srv.Close()
	rf, _ := NewRemoteForecaster(srv.URL)
	ctx := context.Background()
	if _, err := rf.Forecast(ctx, nil, 5); err == nil {
		t.Error("empty series must be rejected")
	}
	if _, err := rf.Forecast(ctx, []float64{1, 2}, 0); err == nil {
		t.Error("horizon 0 must be rejected")
	}
	if _, err := rf.Forecast(ctx, []float64{1, math.NaN()}, 5); err == nil {
		t.Error("NaN in series must be rejected")
	}
	if _, err := rf.Forecast(ctx, []float64{math.Inf(1)}, 5); err == nil {
		t.Error("Inf in series must be rejected")
	}
}

func TestRemoteForecastContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	rf, _ := NewRemoteForecaster(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rf.Forecast(ctx, []float64{1}, 1); err == nil {
		t.Fatal("cancelled context must be an error")
	}
}

func TestRemoteForecastOversizedResponse(t *testing.T) {
	// A response beyond the 8 MiB read limit is truncated mid-JSON and must
	// surface as a decode error rather than hanging or OOMing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"forecast":[`))
		chunk := strings.Repeat("0,", 1<<16)
		for written := 0; written < 9<<20; written += len(chunk) {
			w.Write([]byte(chunk))
		}
		w.Write([]byte(`0]}`))
	}))
	defer srv.Close()
	rf, _ := NewRemoteForecaster(srv.URL)
	if _, err := rf.Forecast(context.Background(), []float64{1}, 1); err == nil {
		t.Fatal("oversized response must be an error")
	}
}
