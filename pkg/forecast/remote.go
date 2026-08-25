package forecast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
)

// RemoteForecaster delegates forecasting to an external model server — the
// integration point for pre-trained time-series foundation models (Amazon
// Chronos/Chronos-Bolt, Google TimesFM, Moirai, …) served behind a thin HTTP
// wrapper. Kilter's built-in statistical models remain the default and the
// fallback: the brain must keep deciding when the model server is down.
//
// Contract:
//
//	POST <url>            {"series": [..float64], "horizon": N}
//	→ 200 application/json {"forecast": [..float64]}   // length N
type RemoteForecaster struct {
	url string
	hc  *http.Client
}

// maxResponseBytes caps how much of a remote response is read: a misbehaving
// model server must not be able to balloon the brain's memory.
const maxResponseBytes = 8 << 20

// NewRemoteForecaster validates the endpoint URL. The client enforces a
// 10-second request timeout in addition to any caller context deadline.
func NewRemoteForecaster(endpoint string) (*RemoteForecaster, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("forecast: invalid forecaster url %q", endpoint)
	}
	return &RemoteForecaster{url: endpoint, hc: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Forecast requests `horizon` future points for the series. It returns
// exactly horizon points or an error — a partial answer would silently
// under-cover the horizon for callers that take the max over the window.
func (rf *RemoteForecaster) Forecast(ctx context.Context, series []float64, horizon int) ([]float64, error) {
	if len(series) == 0 || horizon < 1 {
		return nil, fmt.Errorf("forecast: empty series or horizon %d", horizon)
	}
	// Reject non-finite inputs up front: json.Marshal would fail on them
	// anyway, but with an error that doesn't say where the garbage is.
	for i, v := range series {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("forecast: series[%d] is non-finite (%v)", i, v)
		}
	}
	body, err := json.Marshal(map[string]any{"series": series, "horizon": horizon})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rf.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rf.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forecast: remote call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the keep-alive connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("forecast: remote returned %d", resp.StatusCode)
	}
	var out struct {
		Forecast []float64 `json:"forecast"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("forecast: decode remote response: %w", err)
	}
	if len(out.Forecast) != horizon {
		return nil, fmt.Errorf("forecast: remote returned %d points, want %d", len(out.Forecast), horizon)
	}
	for i, v := range out.Forecast {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 { // demand must be finite and non-negative
			return nil, fmt.Errorf("forecast: remote returned invalid value %v at point %d", v, i)
		}
	}
	return out.Forecast, nil
}
