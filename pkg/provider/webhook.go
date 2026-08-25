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
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("provider webhook: %s returned %d: %.200s", payload["action"], resp.StatusCode, raw)
	}
	if out == nil {
		// The 2xx status already confirmed the action; a body read hiccup
		// after that does not negate it.
		return nil
	}
	// Here the body IS the result (discover): a truncated read must fail
	// loudly, not feed a partial payload into sizing decisions.
	if readErr != nil {
		return fmt.Errorf("provider webhook: read %s response: %w", payload["action"], readErr)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("provider webhook: decode %s response: %w", payload["action"], err)
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
	if err := validateDiscovery(out.Groups, out.Nodes); err != nil {
		return nil, nil, fmt.Errorf("provider webhook: discover: %w", err)
	}
	return out.Groups, out.Nodes, nil
}

// validateDiscovery rejects endpoint payloads that would poison downstream
// sizing decisions: empty or duplicate group IDs, negative sizes, Max < Min,
// and node mappings with an empty side. Two lenient choices are deliberate —
// Desired outside [Min,Max] passes (systems mid-rebalance report that
// transiently), and a node may map to a group absent from Groups (endpoints
// can expose pools for termination without listing them as scalable).
func validateDiscovery(groups []NodeGroup, nodes map[string]string) error {
	ids := make(map[string]bool, len(groups))
	for i, g := range groups {
		if g.ID == "" {
			return fmt.Errorf("group[%d] (name %q) has empty id", i, g.Name)
		}
		if ids[g.ID] {
			return fmt.Errorf("duplicate group id %q", g.ID)
		}
		ids[g.ID] = true
		if g.Min < 0 || g.Desired < 0 {
			return fmt.Errorf("group %q has negative size (min=%d desired=%d)", g.ID, g.Min, g.Desired)
		}
		if g.Max < g.Min {
			return fmt.Errorf("group %q has max %d < min %d", g.ID, g.Max, g.Min)
		}
	}
	for node, gid := range nodes {
		if node == "" {
			return fmt.Errorf("nodes map contains an empty node name")
		}
		if gid == "" {
			return fmt.Errorf("node %q maps to an empty group id", node)
		}
	}
	return nil
}

func (w *Webhook) ScaleTo(ctx context.Context, groupID string, desired int) error {
	if groupID == "" {
		return fmt.Errorf("provider webhook: empty group id")
	}
	if desired < 0 {
		return fmt.Errorf("provider webhook: negative desired %d", desired)
	}
	return w.call(ctx, map[string]any{"action": "scale-to", "groupID": groupID, "desired": desired}, nil)
}

// TerminateNode asks the endpoint to terminate the machine backing the node.
// providerID may be empty — callers read it before deleting the Node object,
// but the object can already be gone — and the endpoint then resolves the
// machine by node name. With both identifiers empty there is nothing to
// resolve, so that is rejected client-side.
func (w *Webhook) TerminateNode(ctx context.Context, nodeName, providerID string) error {
	if nodeName == "" && providerID == "" {
		return fmt.Errorf("provider webhook: terminate-node needs a node name or providerID")
	}
	return w.call(ctx, map[string]any{"action": "terminate-node", "node": nodeName, "providerID": providerID}, nil)
}
