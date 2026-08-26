// Package fargate implements the k8s-fargate compute domain: EKS pods billed
// per pod by quantized vCPU/memory tier.
//
// Why this domain exists at all. Kilter's node pipeline optimizes by packing
// containers onto cheaper machines. On Fargate there is no machine to pack —
// every pod is its own single-pod VM and AWS bills the *tier* it rounds the
// pod's requests up to. So the only two levers are:
//
//	tier move     — the learned sizing lands the pod on a cheaper tier;
//	boundary shave — the pod sits just over a tier boundary, and dropping the
//	                 memory request to just under it drops a whole tier.
//
// Neither is expressible in a node-centric pipeline, and a request change that
// does not cross a tier boundary saves exactly $0 (pkg/pricing, U1).
//
// The shave is the dangerous one and is treated as such throughout: it is
// emitted only above a confidence threshold, above a minimum observation
// window and sample count, never for a container ever seen OOM-killed, and
// never when the shaved request would land inside the noise band of the
// observed memory peak. A shave that saves $2/month and OOMs a pod is a net
// loss, so the defaults are deliberately reluctant.
package fargate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// Spec attribute keys. A Fargate Spec's Resources hold the *billed tier* — the
// thing that costs money — while the per-container requests that produce that
// tier live in Attrs, because the executor patches containers, not pods.
const (
	// AttrTier is the billed configuration as AWS stamps it, e.g. "1vCPU 8GB".
	AttrTier = "tier"
	// AttrCostSource records how the tier was resolved: the AWS-stamped
	// CapacityProvisioned annotation, or the quantizer over requests.
	AttrCostSource = "costSource"
	// AttrChange names the lever: tier-move, boundary-shave or revert.
	AttrChange = "change"
	// AttrReplicas is how many pods of this workload are billed at this tier.
	AttrReplicas = "replicas"
	// AttrRequestMilliCPU / AttrRequestMemoryBytes are the pod-level effective
	// requests (max of summed long-running containers and largest init
	// container) the tier was quantized from.
	AttrRequestMilliCPU    = "requests.milliCPU"
	AttrRequestMemoryBytes = "requests.memoryBytes"
	// attrContainerPrefix namespaces the per-container breakdown:
	// "container/<name>/milliCPU" and "container/<name>/memoryBytes".
	attrContainerPrefix = "container/"
)

// Change types recorded in AttrChange.
const (
	ChangeTierMove      = "tier-move"
	ChangeBoundaryShave = "boundary-shave"
	ChangeRevert        = "revert"
)

// ContainerChange is one container's request specification inside a Fargate
// Spec. Both the current and the proposed Spec list every container, so an
// actuator can diff them without consulting the cluster.
type ContainerChange struct {
	Name     string
	Requests model.Resources
}

// buildSpec renders a billed tier plus its per-container request breakdown as a
// domain.Spec. Containers are sorted by name, so the Spec — and therefore the
// step key hashed from it — is identical however the pod listed them.
func buildSpec(cfg pricing.FargateConfig, eff model.Resources, containers []ContainerChange,
	change, costSource string, replicas int) domain.Spec {

	sorted := make([]ContainerChange, len(containers))
	copy(sorted, containers)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	attrs := map[string]string{
		AttrTier:               cfg.String(),
		AttrChange:             change,
		AttrReplicas:           strconv.Itoa(replicas),
		AttrRequestMilliCPU:    strconv.FormatInt(eff.MilliCPU, 10),
		AttrRequestMemoryBytes: strconv.FormatInt(eff.MemoryBytes, 10),
	}
	if costSource != "" {
		attrs[AttrCostSource] = costSource
	}
	for _, c := range sorted {
		attrs[attrContainerPrefix+c.Name+"/milliCPU"] = strconv.FormatInt(c.Requests.MilliCPU, 10)
		attrs[attrContainerPrefix+c.Name+"/memoryBytes"] = strconv.FormatInt(c.Requests.MemoryBytes, 10)
	}
	return domain.Spec{Resources: cfg.Resources(), Attrs: attrs}
}

// Containers decodes the per-container request breakdown of a Fargate Spec,
// sorted by container name. It is the actuator's entry point: patch each
// container's requests to these values and the pod lands on Spec.Resources.
//
// The decode is deliberately strict — a malformed or half-written breakdown is
// an error, never a partial patch.
func Containers(s domain.Spec) ([]ContainerChange, error) {
	byName := map[string]*ContainerChange{}
	var order []string
	for _, k := range s.AttrKeys() { // sorted: no map-order dependence
		if !strings.HasPrefix(k, attrContainerPrefix) {
			continue
		}
		rest := strings.TrimPrefix(k, attrContainerPrefix)
		slash := strings.LastIndex(rest, "/")
		if slash <= 0 {
			return nil, fmt.Errorf("fargate: malformed container attribute %q", k)
		}
		name, field := rest[:slash], rest[slash+1:]
		v, err := strconv.ParseInt(s.Attrs[k], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("fargate: container attribute %q: %w", k, err)
		}
		cc := byName[name]
		if cc == nil {
			cc = &ContainerChange{Name: name}
			byName[name] = cc
			order = append(order, name)
		}
		switch field {
		case "milliCPU":
			cc.Requests.MilliCPU = v
		case "memoryBytes":
			cc.Requests.MemoryBytes = v
		default:
			return nil, fmt.Errorf("fargate: unknown container attribute %q", k)
		}
	}
	out := make([]ContainerChange, 0, len(order))
	for _, n := range order { // order comes from the sorted key scan
		out = append(out, *byName[n])
	}
	return out, nil
}

// targetID renders a workload as a TargetRef.ID: "Kind/namespace/name".
func targetID(w model.WorkloadRef) string { return w.String() }

// parseTargetID is the inverse of targetID. Workload kinds, namespaces and
// names never contain "/", so the split is unambiguous.
func parseTargetID(id string) (model.WorkloadRef, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return model.WorkloadRef{}, fmt.Errorf("fargate: malformed target ID %q (want Kind/namespace/name)", id)
	}
	return model.WorkloadRef{
		Kind:      model.WorkloadKind(parts[0]),
		Namespace: parts[1],
		Name:      parts[2],
	}, nil
}
