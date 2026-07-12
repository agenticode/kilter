package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

// Client talks to a brain. Used by agents (push) and controllers (pull).
type Client struct {
	base    string
	token   string
	hc      *http.Client
	retries int
}

// NewClient validates the base URL and returns a client with sane timeouts.
func NewClient(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("api: invalid brain url %q", baseURL)
	}
	return &Client{
		base:    strings.TrimRight(baseURL, "/"),
		token:   token,
		hc:      &http.Client{Timeout: 60 * time.Second},
		retries: 3,
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, gzipBody bool, out any) error {
	var lastErr error
	for attempt := 0; attempt < c.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			}
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
			if gzipBody {
				req.Header.Set("Content-Encoding", "gzip")
			}
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue // network errors are retryable
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("api: decode response: %w", err)
				}
			}
			return nil
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("api: %s %s → %d: %s", method, path, resp.StatusCode, truncate(respBody))
			continue // server errors are retryable
		default:
			return fmt.Errorf("api: %s %s → %d: %s", method, path, resp.StatusCode, truncate(respBody))
		}
	}
	return fmt.Errorf("api: giving up after %d attempts: %w", c.retries, lastErr)
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return strings.TrimSpace(s)
}

// PushSnapshot uploads a snapshot (gzip-compressed).
func (c *Client) PushSnapshot(ctx context.Context, snap *model.ClusterSnapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/api/v1/snapshots", buf.Bytes(), true, nil)
}

// GetPlan fetches a freshly built plan for the cluster.
func (c *Client) GetPlan(ctx context.Context, cluster string) (*plan.Plan, error) {
	var p plan.Plan
	if err := c.do(ctx, http.MethodGet, "/api/v1/clusters/"+url.PathEscape(cluster)+"/plan", nil, false, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetRecommendations fetches current recommendations for the cluster.
func (c *Client) GetRecommendations(ctx context.Context, cluster string) ([]recommend.Recommendation, error) {
	var out struct {
		Recommendations []recommend.Recommendation `json:"recommendations"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/clusters/"+url.PathEscape(cluster)+"/recommendations", nil, false, &out); err != nil {
		return nil, err
	}
	return out.Recommendations, nil
}

// GetInsights fetches the detection layer's findings for the cluster.
func (c *Client) GetInsights(ctx context.Context, cluster string) ([]model.Insight, error) {
	var out struct {
		Insights []model.Insight `json:"insights"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/clusters/"+url.PathEscape(cluster)+"/insights", nil, false, &out); err != nil {
		return nil, err
	}
	return out.Insights, nil
}

// ReportExecution records an executed plan in the cluster's audit ledger.
func (c *Client) ReportExecution(ctx context.Context, cluster string, e LedgerEntry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/api/v1/clusters/"+url.PathEscape(cluster)+"/reports", raw, false, nil)
}

// GetLedger fetches the audit ledger + cost timeline.
func (c *Client) GetLedger(ctx context.Context, cluster string) (*LedgerReport, error) {
	var out LedgerReport
	if err := c.do(ctx, http.MethodGet, "/api/v1/clusters/"+url.PathEscape(cluster)+"/ledger", nil, false, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Approve marks a plan fingerprint as approved for execution.
func (c *Client) Approve(ctx context.Context, cluster, fingerprint string) error {
	raw, _ := json.Marshal(map[string]string{"fingerprint": fingerprint})
	return c.do(ctx, http.MethodPost, "/api/v1/clusters/"+url.PathEscape(cluster)+"/approvals", raw, false, nil)
}

// GetApprovals lists currently valid approvals.
func (c *Client) GetApprovals(ctx context.Context, cluster string) ([]Approval, error) {
	var out struct {
		Approvals []Approval `json:"approvals"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/clusters/"+url.PathEscape(cluster)+"/approvals", nil, false, &out); err != nil {
		return nil, err
	}
	return out.Approvals, nil
}

// Healthy probes the brain's health endpoint.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
