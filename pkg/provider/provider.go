// Package provider closes the loop between "delete the Node object" and
// "stop paying for the machine". Kubernetes node deletion alone does not
// terminate cloud instances (except under Karpenter); a Provider does.
//
// Providers live strictly in the actuation path: the decision engine never
// depends on them, every call is bounded by the caller's context, and a
// provider failure fails the plan step loudly — capacity is never dropped
// silently, and money is never assumed saved without confirmation.
package provider

import (
	"context"
	"fmt"
)

// NodeGroup is a scalable set of homogeneous nodes (an ASG / managed node
// group / on-prem pool).
type NodeGroup struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Min           int      `json:"min"`
	Max           int      `json:"max"`
	Desired       int      `json:"desired"`
	InstanceTypes []string `json:"instanceTypes,omitempty"`
	Spot          bool     `json:"spot,omitempty"`
}

// Provider manages node lifecycle for one cluster.
type Provider interface {
	// Name identifies the implementation ("eks", "webhook", "karpenter", "none").
	Name() string
	// Discover lists node groups and maps Kubernetes node names to group IDs.
	Discover(ctx context.Context) ([]NodeGroup, map[string]string, error)
	// ScaleTo sets a group's desired capacity (used for provision-before-drain).
	ScaleTo(ctx context.Context, groupID string, desired int) error
	// TerminateNode terminates the cloud instance backing a node, shrinking
	// its group so the capacity is not replaced. Called after the node is
	// drained and its Node object deleted. Must be idempotent: terminating an
	// already-gone instance returns nil.
	TerminateNode(ctx context.Context, nodeName, providerID string) error
}

// None is the explicit no-provider choice: node deletion is the operator's
// (or Karpenter's) signal, and Kilter makes no cloud calls.
type None struct{}

func (None) Name() string { return "none" }
func (None) Discover(context.Context) ([]NodeGroup, map[string]string, error) {
	return nil, nil, nil
}
func (None) ScaleTo(context.Context, string, int) error { return nil }
func (None) TerminateNode(context.Context, string, string) error {
	return nil
}

// Karpenter documents the karpenter contract: deleting the Node/NodeClaim
// object triggers Karpenter's finalizer, which drains remaining pods and
// terminates the instance. So the correct provider action is "nothing extra",
// but it is a distinct type so reports say who owned the termination.
type Karpenter struct{}

func (Karpenter) Name() string { return "karpenter" }
func (Karpenter) Discover(context.Context) ([]NodeGroup, map[string]string, error) {
	return nil, nil, nil
}
func (Karpenter) ScaleTo(context.Context, string, int) error {
	return fmt.Errorf("karpenter provider: scaling is declarative (NodePool limits); ScaleTo is not applicable")
}
func (Karpenter) TerminateNode(context.Context, string, string) error {
	return nil // node deletion already terminated the instance via finalizer
}

// New builds a provider by name.
//   - "none" (default), "karpenter": no external config
//   - "webhook": cfg = endpoint URL (see NewWebhook)
//   - "eks": AWS credentials from the environment; cfg = cluster name used in
//     the kubernetes.io/cluster/<name> ASG tag
func New(ctx context.Context, name, cfg string) (Provider, error) {
	switch name {
	case "", "none":
		return None{}, nil
	case "karpenter":
		return Karpenter{}, nil
	case "webhook":
		return NewWebhook(cfg)
	case "eks":
		return NewEKS(ctx, cfg)
	}
	return nil, fmt.Errorf("provider: unknown provider %q (none|karpenter|webhook|eks)", name)
}
