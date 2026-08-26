package fargate

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// Test fixtures build a Fargate-shaped cluster snapshot: one single-pod VM per
// pod, labelled and ManagedBy-marked exactly as EKS presents them, plus a usage
// series per container template (pkg/recommend aggregates replicas VPA-style,
// and so does this domain).

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// ctr describes one container of a workload.
type ctr struct {
	name   string
	cpuReq int64 // millicores
	memReq int64 // bytes
	cpuUse int64 // millicores, constant
	memUse int64 // bytes, constant
	memMax int64 // optional single spike, applied to the middle sample
	oom    bool  // report the container as having been OOM-killed
}

// wl describes one Fargate workload.
type wl struct {
	name        string
	ns          string
	kind        model.WorkloadKind
	replicas    int
	containers  []ctr
	init        model.Resources
	provisioned model.Resources
	mode        string
	phase       string
}

func (w wl) ref() model.WorkloadRef {
	k := w.kind
	if k == "" {
		k = model.KindDeployment
	}
	ns := w.ns
	if ns == "" {
		ns = "default"
	}
	return model.WorkloadRef{Kind: k, Namespace: ns, Name: w.name}
}

// cluster renders the workloads into a snapshot with `samples` usage points per
// container spread evenly over `window`, the newest at `now`.
func cluster(now time.Time, samples int, window time.Duration, ws ...wl) *model.ClusterSnapshot {
	snap := &model.ClusterSnapshot{ClusterID: "c1", Timestamp: now}
	for _, w := range ws {
		ref := w.ref()
		for r := 0; r < max(w.replicas, 1); r++ {
			nodeName := w.name + "-fargate-" + string(rune('a'+r))
			snap.Nodes = append(snap.Nodes, model.NodeSpec{
				Name:      nodeName,
				ManagedBy: model.ManagedByFargate,
				Labels:    map[string]string{model.LabelComputeType: "fargate"},
				// A Fargate VM reports a capacity unrelated to the bill; it is
				// deliberately implausible here so any code that priced it
				// would be obvious.
				Capacity:    model.Resources{MilliCPU: 96000, MemoryBytes: 384 * gib},
				Allocatable: model.Resources{MilliCPU: 96000, MemoryBytes: 384 * gib},
				Ready:       true,
				Taints: []model.Taint{{
					Key: model.LabelComputeType, Value: "fargate", Effect: "NoSchedule"}},
			})
			pod := model.PodSpec{
				UID:                 w.name + "-" + string(rune('a'+r)),
				Name:                w.name + "-" + string(rune('a'+r)),
				Namespace:           ref.Namespace,
				Workload:            ref,
				NodeName:            nodeName,
				InitRequests:        w.init,
				ProvisionedCapacity: w.provisioned,
				Phase:               w.phase,
			}
			if pod.Phase == "" {
				pod.Phase = "Running"
			}
			for _, c := range w.containers {
				pod.Containers = append(pod.Containers, model.ContainerSpec{
					Name:          c.name,
					Requests:      model.Resources{MilliCPU: c.cpuReq, MemoryBytes: c.memReq},
					RestartCount:  0,
					LastOOMKilled: c.oom,
				})
			}
			snap.Pods = append(snap.Pods, pod)
		}
		snap.Workloads = append(snap.Workloads, model.WorkloadInfo{
			Ref: ref, Replicas: int32(max(w.replicas, 1)), Ready: int32(max(w.replicas, 1)), Mode: w.mode,
		})
		for _, c := range w.containers {
			key := model.ContainerKey{Workload: ref, Container: c.name}
			snap.Usage = append(snap.Usage, series(key, w.name+"-a", now, samples, window, c)...)
		}
	}
	return snap
}

// series generates a constant usage series, optionally with one spike in the
// middle so a test can separate "peak" from "typical".
func series(key model.ContainerKey, uid string, now time.Time, samples int, window time.Duration, c ctr) []model.Usage {
	if samples < 1 {
		return nil
	}
	step := time.Duration(0)
	if samples > 1 {
		step = window / time.Duration(samples-1)
	}
	start := now.Add(-window)
	out := make([]model.Usage, 0, samples)
	for i := 0; i < samples; i++ {
		mem := c.memUse
		if c.memMax > 0 && i == samples/2 {
			mem = c.memMax
		}
		out = append(out, model.Usage{
			Key:           key,
			PodUID:        uid,
			Timestamp:     start.Add(time.Duration(i) * step),
			MilliCPU:      c.cpuUse,
			MemoryBytes:   mem,
			WindowSeconds: 60,
		})
	}
	return out
}

// newDomain builds a ready-to-act domain (actuation wired) with optional
// config tweaks.
func newDomain(t *testing.T, mut ...func(*Config)) *Domain {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Scope = "c1"
	cfg.Region = "us-east-1"
	cfg.ActuationAvailable = true
	for _, m := range mut {
		m(&cfg)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// noPolicyMoves suppresses pkg/recommend's own resize proposals so a test
// measures the boundary shave in isolation. MinChangeRatio just under 1 means
// no realistic delta is "significant", so the recommender emits nothing and the
// tier-move candidate degenerates to "keep the current requests".
func noPolicyMoves(c *Config) { c.Recommend.MinChangeRatio = 0.99 }

// learn feeds a snapshot through the seam exactly as the brain would.
func learn(t *testing.T, d *Domain, snap *model.ClusterSnapshot) {
	t.Helper()
	if err := d.Learn(&domain.Snapshot{
		Domain: Kind, Scope: "c1", Timestamp: snap.Timestamp, Cluster: snap,
	}); err != nil {
		t.Fatalf("Learn: %v", err)
	}
}

// only returns the single recommendation for a workload, failing otherwise.
func only(t *testing.T, recs []domain.Recommendation, ref model.WorkloadRef) domain.Recommendation {
	t.Helper()
	var found []domain.Recommendation
	for _, r := range recs {
		if r.Target.ID == targetID(ref) {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 recommendation for %s, got %d (all: %s)", ref, len(found), describe(recs))
	}
	if err := found[0].Validate(); err != nil {
		t.Fatalf("invalid recommendation: %v", err)
	}
	return found[0]
}

func none(t *testing.T, recs []domain.Recommendation, ref model.WorkloadRef) {
	t.Helper()
	for _, r := range recs {
		if r.Target.ID == targetID(ref) {
			t.Fatalf("expected no recommendation for %s, got %s: %s",
				ref, r.Proposed.Attr(AttrChange), r.Reason)
		}
	}
}

func describe(recs []domain.Recommendation) string {
	out := ""
	for _, r := range recs {
		out += "\n  " + r.Target.ID + " " + r.Proposed.Attr(AttrChange) + " " +
			r.Current.Attr(AttrTier) + "->" + r.Proposed.Attr(AttrTier) + " " + r.Reason
	}
	if out == "" {
		return " (none)"
	}
	return out
}

// tierOf parses the billed tier back out of a spec, proving the attribute and
// the priced Resources agree.
func tierOf(t *testing.T, s domain.Spec) pricing.FargateConfig {
	t.Helper()
	cfg, err := pricing.ParseCapacityProvisioned(s.Attr(AttrTier))
	if err != nil {
		t.Fatalf("spec tier %q: %v", s.Attr(AttrTier), err)
	}
	if cfg.Resources() != s.Resources {
		t.Fatalf("spec Resources %s disagree with tier %s", s.Resources, cfg)
	}
	return cfg
}
