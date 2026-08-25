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

	appsv1 "k8s.io/api/apps/v1"
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
	// evictBackoff is the base delay between retries of a PDB-refused
	// eviction: attempt n waits n×evictBackoff. Shortened only in tests.
	evictBackoff time.Duration
}

// New builds an actuator. An unset Mode defaults to dry-run; any other
// unknown Mode is rejected rather than defaulted, because everything past
// this constructor trusts Mode blindly — a typo'd mode must fail here, not
// fall through the dry-run check and mutate a live cluster.
func New(client kubernetes.Interface, cfg Config) (*Actuator, error) {
	if client == nil {
		return nil, fmt.Errorf("actuate: nil client")
	}
	cfg = cfg.withDefaults()
	if cfg.Mode != ModeDryRun && cfg.Mode != ModeApply {
		return nil, fmt.Errorf("actuate: unknown mode %q", cfg.Mode)
	}
	return &Actuator{
		client:       client,
		cfg:          cfg,
		budget:       safety.NewBudget(cfg.MaxEvictionsPerHour, time.Hour),
		evictBackoff: 10 * time.Second,
	}, nil
}

// Step outcome values recorded in StepStatus.Status.
const (
	StatusDone    = "done"    // step applied and confirmed
	StatusDryRun  = "dry-run" // step previewed only (counted as done in Report)
	StatusSkipped = "skipped" // step not applicable (unknown type)
	StatusFailed  = "failed"  // step attempted and failed; Error is set
)

// StepStatus is the outcome of one executed step.
type StepStatus struct {
	Step   plan.Step `json:"step"`
	Status string    `json:"status"` // one of the Status* constants
	Error  string    `json:"error,omitempty"`
}

// Report summarizes a plan execution.
type Report struct {
	Mode     Mode         `json:"mode"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Steps    []StepStatus `json:"steps"`
	// Done counts steps that succeeded — including dry-run previews, so a
	// clean dry-run and a clean apply of the same plan report the same Done.
	Done    int `json:"done"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Aborted is set when a failure stopped the remaining steps; steps that
	// never ran are absent from Steps.
	Aborted bool `json:"aborted,omitempty"`
}

// ExecutePlan runs the plan's steps in order. Failed evictions and resizes
// are recorded and skipped past — partial drain progress beats an aborted
// drain, and the next reconcile retries what remains. Only cordon/delete
// failures abort the plan: WaitNodeEmpty independently guarantees a node is
// never deleted while pods that failed to evict still run on it.
func (a *Actuator) ExecutePlan(ctx context.Context, p *plan.Plan) *Report {
	rep := &Report{Mode: a.cfg.Mode, Started: time.Now().UTC()}
	defer func() { rep.Finished = time.Now().UTC() }()
	if p == nil {
		return rep
	}

	for _, s := range p.Steps {
		if ctx.Err() != nil {
			rep.Aborted = true
			break
		}
		st := a.execute(ctx, s)
		rep.Steps = append(rep.Steps, st)
		switch st.Status {
		case StatusDone, StatusDryRun:
			rep.Done++
		case StatusSkipped:
			rep.Skipped++
		case StatusFailed:
			rep.Failed++
			if s.Type != plan.StepResizeWorkload && s.Type != plan.StepEvictPod {
				// Cordon/delete failure: stop the whole plan.
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
	switch s.Type {
	case plan.StepResizeWorkload, plan.StepCordonNode, plan.StepEvictPod, plan.StepDeleteNode:
	default:
		// Checked before the dry-run branch so both modes report the same
		// thing: a step apply would skip must not preview as success.
		return StepStatus{Step: s, Status: StatusSkipped, Error: "unknown step type"}
	}
	if a.cfg.Mode == ModeDryRun {
		// Previews validate what they can without an API call, so garbage
		// steps surface on the dry run rather than on apply day.
		if s.Type == plan.StepResizeWorkload {
			if err := validateResize(s.Container, s.ToReq, s.ToLim); err != nil {
				log.Error("dry-run: invalid step", "err", err)
				return StepStatus{Step: s, Status: StatusFailed, Error: err.Error()}
			}
		}
		log.Info("dry-run", "detail", s.Detail)
		return StepStatus{Step: s, Status: StatusDryRun}
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
	}
	if err != nil {
		log.Error("step failed", "err", err)
		return StepStatus{Step: s, Status: StatusFailed, Error: err.Error()}
	}
	log.Info("step done", "detail", s.Detail)
	return StepStatus{Step: s, Status: StatusDone}
}

// resourcesToK8s renders a model.Resources as a k8s resource map fragment.
// Zero fields are omitted: under a strategic merge patch an absent key leaves
// the workload's current value unchanged. Negative values are rejected by
// validateResize before this is ever called.
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

// validateResize rejects resize inputs that would otherwise fail in confusing
// ways or, worse, succeed as a lie: a negative quantity would be silently
// dropped from the patch and the no-op reported "done", and an empty container
// name would strategic-merge-append a nameless broken container to the
// template instead of updating an existing one.
func validateResize(container string, req, lim model.Resources) error {
	if container == "" {
		return fmt.Errorf("resize: empty container name")
	}
	if req.MilliCPU < 0 || req.MemoryBytes < 0 || lim.MilliCPU < 0 || lim.MemoryBytes < 0 {
		return fmt.Errorf("resize container %s: negative resources (req %s, lim %s)", container, req, lim)
	}
	return nil
}

// ResizeWorkload patches the controller's pod template with new requests and
// limits for one container, then (optionally) resizes running pods in place.
// Zero-valued fields of req/lim leave the corresponding current value
// unchanged, so a fully-zero resize is a deliberate no-op (the undo path uses
// this when the original workload had nothing set).
func (a *Actuator) ResizeWorkload(ctx context.Context, ref model.WorkloadRef, container string, req, lim model.Resources) error {
	if err := validateResize(container, req, lim); err != nil {
		return err
	}
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
		a.cfg.Logger.Warn("in-place resize skipped: listing pods failed (rollout will converge)",
			"workload", ref.String(), "err", err)
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
	raw, err := json.Marshal(patch)
	if err != nil {
		a.cfg.Logger.Warn("in-place resize skipped: patch marshal failed (rollout will converge)",
			"workload", ref.String(), "err", err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !ownedBy(pod, ref) {
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

// ownedBy matches a pod to its workload through owner names: direct owner for
// statefulsets; for deployments the controller is a ReplicaSet named
// <deployment>-<pod-template-hash>, so the pod's hash label reconstructs the
// exact expected name. A bare name-prefix test is not enough: deployment "api"
// must not claim pods of "api-gateway" (ReplicaSet "api-gateway-<hash>" has
// the prefix "api-"), or an in-place resize would squeeze a sibling workload's
// running pods. False negatives are safe here — the template rollout converges
// any pod this skips.
func ownedBy(pod *corev1.Pod, ref model.WorkloadRef) bool {
	or := metav1.GetControllerOf(pod)
	if or == nil || pod.Namespace != ref.Namespace {
		return false
	}
	switch ref.Kind {
	case model.KindStatefulSet:
		return or.Kind == "StatefulSet" && or.Name == ref.Name
	case model.KindDeployment:
		hash := pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
		return or.Kind == "ReplicaSet" && hash != "" && or.Name == ref.Name+"-"+hash
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
// rejection (429) is retried a few times before giving up. The budget slot is
// consumed per attempt sequence, even when the pod turns out to be already
// gone — counting conservatively can only slow us down, never over-disrupt.
func (a *Actuator) EvictPod(ctx context.Context, nsName string) error {
	parts := strings.SplitN(nsName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
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
			case <-time.After(time.Duration(attempt) * a.evictBackoff):
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
	// The Node object is the only record of the providerID. With a real
	// provider, a transient read failure must fail the step BEFORE the
	// delete: deleting first would orphan the instance — no ID left to
	// terminate it with, billing forever — while failing here just retries
	// the whole (idempotent) step on the next reconcile.
	providerID := ""
	switch n, err := a.client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); {
	case err == nil:
		providerID = n.Spec.ProviderID
	case apierrors.IsNotFound(err):
		// Already deleted by someone else; proceed so the provider call
		// still runs — termination is confirmed, never assumed.
	case a.cfg.Provider.Name() != "none":
		return fmt.Errorf("delete node %s: reading providerID before delete: %w", node, err)
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
