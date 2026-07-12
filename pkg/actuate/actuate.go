// Package actuate executes plans against a live cluster. It is the only
// Kilter package that mutates Kubernetes state, and it refuses to do so
// outside its safety envelope:
//
//   - dry-run unless explicitly constructed in apply mode
//   - every eviction passes the sliding disruption budget and PDB API
//     (evictions go through policy/v1 Eviction, so the apiserver enforces
//     budgets even if our snapshot was stale)
//   - nodes are cordoned before eviction, deleted only once empty
//   - workload resizes go through the controller template (normal rollout);
//     if the apiserver supports in-place pod resize, running pods are also
//     patched directly so the change lands without a restart
package actuate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/provider"
	"github.com/agenticode/kilter/pkg/safety"
)

// Mode selects whether the actuator mutates anything.
type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModeApply  Mode = "apply"
)

// Config tunes the actuator.
type Config struct {
	Mode Mode
	// MaxEvictionsPerHour feeds the sliding disruption budget. Default 20.
	MaxEvictionsPerHour int
	// NodeDrainTimeout bounds waiting for a node to empty. Default 5m.
	NodeDrainTimeout time.Duration
	// PollInterval for drain waiting. Default 5s.
	PollInterval time.Duration
	// InPlaceResize additionally patches running pods via the resize
	// subresource (K8s ≥1.33) so resizes land without restarts.
	InPlaceResize bool
	// Provider terminates cloud instances after node deletion so freed
	// capacity stops billing. Default: provider.None (no cloud calls).
	Provider provider.Provider
	Logger   *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Mode == "" {
		c.Mode = ModeDryRun
	}
	if c.MaxEvictionsPerHour <= 0 {
		c.MaxEvictionsPerHour = 20
	}
	if c.NodeDrainTimeout <= 0 {
		c.NodeDrainTimeout = 5 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.Provider == nil {
		c.Provider = provider.None{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Actuator executes plan steps.
type Actuator struct {
	client kubernetes.Interface
	cfg    Config
	budget *safety.Budget
}

// New builds an actuator.
func New(client kubernetes.Interface, cfg Config) (*Actuator, error) {
	if client == nil {
		return nil, fmt.Errorf("actuate: nil client")
	}
	cfg = cfg.withDefaults()
	return &Actuator{
		client: client,
		cfg:    cfg,
		budget: safety.NewBudget(cfg.MaxEvictionsPerHour, time.Hour),
	}, nil
}

// StepStatus is the outcome of one executed step.
type StepStatus struct {
	Step   plan.Step `json:"step"`
	Status string    `json:"status"` // done | dry-run | skipped | failed
	Error  string    `json:"error,omitempty"`
}

// Report summarizes a plan execution.
type Report struct {
	Mode     Mode         `json:"mode"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Steps    []StepStatus `json:"steps"`
	Done     int          `json:"done"`
	Failed   int          `json:"failed"`
	Skipped  int          `json:"skipped"`
	// Aborted is set when a failure stopped the remaining steps.
	Aborted bool `json:"aborted,omitempty"`
}

// ExecutePlan runs the plan's steps in order. A failed node-removal step
// aborts the remaining steps of that plan (never leave a half-drained node
// and keep going); failed resizes are recorded and skipped past.
func (a *Actuator) ExecutePlan(ctx context.Context, p *plan.Plan) *Report {
	rep := &Report{Mode: a.cfg.Mode, Started: time.Now().UTC()}
	defer func() { rep.Finished = time.Now().UTC() }()

	for _, s := range p.Steps {
		if ctx.Err() != nil {
			rep.Aborted = true
			break
		}
		st := a.execute(ctx, s)
		rep.Steps = append(rep.Steps, st)
		switch st.Status {
		case "done", "dry-run":
			rep.Done++
		case "skipped":
			rep.Skipped++
		case "failed":
			rep.Failed++
			if s.Type != plan.StepResizeWorkload {
				// Node-surgery failure: stop the whole plan.
				rep.Aborted = true
			}
		}
		if rep.Aborted {
			break
		}
	}
	return rep
}

func (a *Actuator) execute(ctx context.Context, s plan.Step) StepStatus {
	log := a.cfg.Logger.With("step", s.Seq, "type", string(s.Type))
	if a.cfg.Mode == ModeDryRun {
		log.Info("dry-run", "detail", s.Detail)
		return StepStatus{Step: s, Status: "dry-run"}
	}
	var err error
	switch s.Type {
	case plan.StepResizeWorkload:
		err = a.ResizeWorkload(ctx, s.Workload, s.Container, s.ToReq, s.ToLim)
	case plan.StepCordonNode:
		err = a.Cordon(ctx, s.Node)
	case plan.StepEvictPod:
		err = a.EvictPod(ctx, s.Pod)
	case plan.StepDeleteNode:
		if err = a.WaitNodeEmpty(ctx, s.Node); err == nil {
			err = a.DeleteNode(ctx, s.Node)
		}
	default:
		return StepStatus{Step: s, Status: "skipped", Error: "unknown step type"}
	}
	if err != nil {
		log.Error("step failed", "err", err)
		return StepStatus{Step: s, Status: "failed", Error: err.Error()}
	}
	log.Info("step done", "detail", s.Detail)
	return StepStatus{Step: s, Status: "done"}
}

// resourcesToK8s renders a model.Resources as a k8s resource map fragment.
func resourcesToK8s(r model.Resources) map[string]string {
	out := map[string]string{}
	if r.MilliCPU > 0 {
		out["cpu"] = fmt.Sprintf("%dm", r.MilliCPU)
	}
	if r.MemoryBytes > 0 {
		out["memory"] = fmt.Sprintf("%d", r.MemoryBytes)
	}
	return out
}

// ResizeWorkload patches the controller's pod template with new requests and
// limits for one container, then (optionally) resizes running pods in place.
func (a *Actuator) ResizeWorkload(ctx context.Context, ref model.WorkloadRef, container string, req, lim model.Resources) error {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{{
						"name": container,
						"resources": map[string]any{
							"requests": resourcesToK8s(req),
							"limits":   resourcesToK8s(lim),
						},
					}},
				},
			},
		},
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	switch ref.Kind {
	case model.KindDeployment:
		_, err = a.client.AppsV1().Deployments(ref.Namespace).
			Patch(ctx, ref.Name, types.StrategicMergePatchType, raw, metav1.PatchOptions{})
	case model.KindStatefulSet:
		_, err = a.client.AppsV1().StatefulSets(ref.Namespace).
			Patch(ctx, ref.Name, types.StrategicMergePatchType, raw, metav1.PatchOptions{})
	default:
		return fmt.Errorf("resize: unsupported workload kind %s", ref.Kind)
	}
	if err != nil {
		return fmt.Errorf("resize %s: %w", ref, err)
	}
	if a.cfg.InPlaceResize {
		a.resizePodsInPlace(ctx, ref, container, req, lim)
	}
	return nil
}

// resizePodsInPlace best-effort patches running pods via the resize
// subresource. Failures are logged, never fatal: the rollout from the
// template patch will converge the state regardless.
func (a *Actuator) resizePodsInPlace(ctx context.Context, ref model.WorkloadRef, container string, req, lim model.Resources) {
	pods, err := a.client.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	patch := map[string]any{
		"spec": map[string]any{
			"containers": []map[string]any{{
				"name": container,
				"resources": map[string]any{
					"requests": resourcesToK8s(req),
					"limits":   resourcesToK8s(lim),
				},
			}},
		},
	}
	raw, _ := json.Marshal(patch)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if or := metav1.GetControllerOf(pod); or == nil || !ownedBy(pod, ref) {
			continue
		}
		_, err := a.client.CoreV1().Pods(ref.Namespace).
			Patch(ctx, pod.Name, types.StrategicMergePatchType, raw, metav1.PatchOptions{}, "resize")
		if err != nil {
			a.cfg.Logger.Warn("in-place resize failed (rollout will converge)",
				"pod", pod.Name, "err", err)
		}
	}
}

// ownedBy loosely matches a pod to its workload through owner names: direct
// owner for statefulsets, hash-suffixed ReplicaSet for deployments.
func ownedBy(pod *corev1.Pod, ref model.WorkloadRef) bool {
	or := metav1.GetControllerOf(pod)
	if or == nil || pod.Namespace != ref.Namespace {
		return false
	}
	switch ref.Kind {
	case model.KindStatefulSet:
		return or.Kind == "StatefulSet" && or.Name == ref.Name
	case model.KindDeployment:
		return or.Kind == "ReplicaSet" && strings.HasPrefix(or.Name, ref.Name+"-")
	}
	return false
}

// Cordon marks a node unschedulable.
func (a *Actuator) Cordon(ctx context.Context, node string) error {
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err := a.client.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("cordon %s: %w", node, err)
	}
	return nil
}

// Uncordon reverts a cordon (used on aborted plans).
func (a *Actuator) Uncordon(ctx context.Context, node string) error {
	patch := []byte(`{"spec":{"unschedulable":false}}`)
	_, err := a.client.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("uncordon %s: %w", node, err)
	}
	return nil
}

// EvictPod evicts "namespace/name" through the eviction API, honoring the
// local sliding budget; the apiserver additionally enforces PDBs. A PDB
// rejection (429) is retried a few times before giving up.
func (a *Actuator) EvictPod(ctx context.Context, nsName string) error {
	parts := strings.SplitN(nsName, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("evict: bad pod ref %q", nsName)
	}
	ns, name := parts[0], parts[1]
	if !a.budget.Allow(time.Now()) {
		return fmt.Errorf("evict %s: disruption budget exhausted (%d/h)", nsName, a.cfg.MaxEvictionsPerHour)
	}
	ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 10 * time.Second):
			}
		}
		err := a.client.CoreV1().Pods(ns).EvictV1(ctx, ev)
		switch {
		case err == nil:
			return nil
		case apierrors.IsNotFound(err):
			return nil // already gone
		case apierrors.IsTooManyRequests(err):
			lastErr = err // PDB says not now
			continue
		default:
			return fmt.Errorf("evict %s: %w", nsName, err)
		}
	}
	return fmt.Errorf("evict %s: PDB kept refusing: %w", nsName, lastErr)
}

// WaitNodeEmpty polls until only DaemonSet/mirror pods remain on the node.
func (a *Actuator) WaitNodeEmpty(ctx context.Context, node string) error {
	deadline := time.Now().Add(a.cfg.NodeDrainTimeout)
	for {
		pods, err := a.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + node,
		})
		if err != nil {
			return fmt.Errorf("wait empty %s: %w", node, err)
		}
		blocking := 0
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			if or := metav1.GetControllerOf(p); or != nil && or.Kind == "DaemonSet" {
				continue
			}
			if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
				continue
			}
			blocking++
		}
		if blocking == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wait empty %s: %d pods still present after %s", node, blocking, a.cfg.NodeDrainTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.cfg.PollInterval):
		}
	}
}

// DeleteNode removes the Node object, then asks the provider to terminate
// the backing instance so the freed capacity stops billing. Provider failure
// is a step failure: capacity accounting must never be assumed.
func (a *Actuator) DeleteNode(ctx context.Context, node string) error {
	providerID := ""
	if n, err := a.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); err == nil {
		providerID = n.Spec.ProviderID
	}
	err := a.client.CoreV1().Nodes().Delete(ctx, node, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete node %s: %w", node, err)
	}
	if a.cfg.Provider.Name() != "none" {
		if err := a.cfg.Provider.TerminateNode(ctx, node, providerID); err != nil {
			return fmt.Errorf("node %s deleted but instance termination failed (%s provider): %w",
				node, a.cfg.Provider.Name(), err)
		}
		a.cfg.Logger.Info("instance terminated", "node", node, "provider", a.cfg.Provider.Name())
	}
	return nil
}
