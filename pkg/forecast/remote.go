package forecast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// NewRemoteForecaster validates the endpoint URL.
func NewRemoteForecaster(endpoint string) (*RemoteForecaster, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("forecast: invalid forecaster url %q", endpoint)
	}
	return &RemoteForecaster{url: endpoint, hc: &http.Client{Timeout: 10 * time.Second}}, nil
}

// Forecast requests `horizon` future points for the series.
func (rf *RemoteForecaster) Forecast(ctx context.Context, series []float64, horizon int) ([]float64, error) {
	if len(series) == 0 || horizon < 1 {
		return nil, fmt.Errorf("forecast: empty series or horizon %d", horizon)
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
		return nil, fmt.Errorf("forecast: remote returned %d", resp.StatusCode)
	}
	var out struct {
		Forecast []float64 `json:"forecast"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("forecast: decode remote response: %w", err)
	}
	if len(out.Forecast) == 0 {
		return nil, fmt.Errorf("forecast: remote returned no points")
	}
	for _, v := range out.Forecast {
		if v != v || v < 0 { // NaN or negative demand
			return nil, fmt.Errorf("forecast: remote returned invalid value %v", v)
		}
	}
	return out.Forecast, nil
}
