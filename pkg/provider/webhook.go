package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Webhook delegates node lifecycle to an operator-owned HTTP endpoint —
// the escape hatch that makes Kilter's full loop work on any cloud, on-prem
// (IPMI/MaaS/Terraform runners), or behind change-management systems.
//
// Contract (all POST, JSON):
//
//	{"action":"discover"}
//	  → {"groups":[{"id","name","min","max","desired","instanceTypes","spot"}],
//	     "nodes":{"<nodeName>":"<groupID>"}}
//	{"action":"scale-to","groupID":"g","desired":N}      → 2xx
//	{"action":"terminate-node","node":"n","providerID":"…"} → 2xx (idempotent)
//
// A bearer token is sent when KILTER_PROVIDER_TOKEN is set.
type Webhook struct {
	url   string
	token string
	hc    *http.Client
}

// NewWebhook validates the endpoint.
func NewWebhook(endpoint string) (*Webhook, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("provider: invalid webhook url %q", endpoint)
	}
	return &Webhook{
		url:   endpoint,
		token: os.Getenv("KILTER_PROVIDER_TOKEN"),
		hc:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) call(ctx context.Context, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return fmt.Errorf("provider webhook: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider webhook: %s returned %d: %.200s", payload["action"], resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("provider webhook: decode %s response: %w", payload["action"], err)
		}
	}
	return nil
}

func (w *Webhook) Discover(ctx context.Context) ([]NodeGroup, map[string]string, error) {
	var out struct {
		Groups []NodeGroup       `json:"groups"`
		Nodes  map[string]string `json:"nodes"`
	}
	if err := w.call(ctx, map[string]any{"action": "discover"}, &out); err != nil {
		return nil, nil, err
	}
	return out.Groups, out.Nodes, nil
}

func (w *Webhook) ScaleTo(ctx context.Context, groupID string, desired int) error {
	if desired < 0 {
		return fmt.Errorf("provider webhook: negative desired %d", desired)
	}
	return w.call(ctx, map[string]any{"action": "scale-to", "groupID": groupID, "desired": desired}, nil)
}

func (w *Webhook) TerminateNode(ctx context.Context, nodeName, providerID string) error {
	return w.call(ctx, map[string]any{"action": "terminate-node", "node": nodeName, "providerID": providerID}, nil)
}
