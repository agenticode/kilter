package actuate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/provider"
)

func deployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
				}}},
			},
		},
	}
}

func nodeObj(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func podOnNode(ns, name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func wref(ns, name string) model.WorkloadRef {
	return model.WorkloadRef{Kind: model.KindDeployment, Namespace: ns, Name: name}
}

// evictionDeletesPod wires the fake so evictions actually remove pods, and
// the field selector on pod lists works (the fake ignores field selectors).
func evictionDeletesPod(client *k8sfake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		ca, ok := action.(k8stesting.CreateAction)
		if !ok || ca.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		obj, err := meta.Accessor(ca.GetObject())
		if err != nil {
			return true, nil, err
		}
		err = client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ca.GetNamespace(), obj.GetName())
		return true, nil, err
	})
}

func TestDryRunTouchesNothing(t *testing.T) {
	client := k8sfake.NewClientset(deployment("prod", "web"), nodeObj("n1"))
	a, err := New(client, Config{Mode: ModeDryRun})
	if err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{Steps: []plan.Step{
		{Seq: 1, Type: plan.StepResizeWorkload, Workload: wref("prod", "web"), Container: "app",
			ToReq: model.Resources{MilliCPU: 200, MemoryBytes: 1 << 30}},
		{Seq: 2, Type: plan.StepCordonNode, Node: "n1"},
		{Seq: 3, Type: plan.StepDeleteNode, Node: "n1"},
	}}
	rep := a.ExecutePlan(context.Background(), p)
	if rep.Done != 3 || rep.Failed != 0 {
		t.Fatalf("report: %+v", rep)
	}
	// Nothing mutated: only the seed objects, no write actions.
	for _, act := range client.Actions() {
		if act.GetVerb() != "get" && act.GetVerb() != "list" && act.GetVerb() != "watch" {
			t.Fatalf("dry-run performed %s on %s", act.GetVerb(), act.GetResource().Resource)
		}
	}
}

func TestResizeWorkloadPatchesTemplate(t *testing.T) {
	client := k8sfake.NewClientset(deployment("prod", "web"))
	a, _ := New(client, Config{Mode: ModeApply})
	err := a.ResizeWorkload(context.Background(), wref("prod", "web"), "app",
		model.Resources{MilliCPU: 250, MemoryBytes: 512 << 20},
		model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	res := d.Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().MilliValue() != 250 {
		t.Fatalf("cpu request not patched: %v", res.Requests.Cpu())
	}
	if res.Requests.Memory().Value() != 512<<20 {
		t.Fatalf("memory request not patched: %v", res.Requests.Memory())
	}
	if res.Limits.Cpu().MilliValue() != 500 {
		t.Fatalf("cpu limit not patched: %v", res.Limits.Cpu())
	}
	// Unsupported kind fails cleanly.
	if err := a.ResizeWorkload(context.Background(),
		model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "prod", Name: "ds"},
		"app", model.Resources{}, model.Resources{}); err == nil {
		t.Fatal("daemonset resize must be rejected")
	}
}

func TestCordonUncordon(t *testing.T) {
	client := k8sfake.NewClientset(nodeObj("n1"))
	a, _ := New(client, Config{Mode: ModeApply})
	if err := a.Cordon(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	n, _ := client.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("node not cordoned")
	}
	if err := a.Uncordon(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	n, _ = client.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Fatal("node not uncordoned")
	}
}

func TestEvictPod(t *testing.T) {
	client := k8sfake.NewClientset(podOnNode("prod", "web-1", "n1"))
	evictionDeletesPod(client)
	a, _ := New(client, Config{Mode: ModeApply})
	if err := a.EvictPod(context.Background(), "prod/web-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("prod").Get(context.Background(), "web-1", metav1.GetOptions{}); err == nil {
		t.Fatal("pod should be gone after eviction")
	}
	// Evicting a non-existent pod is fine (already gone).
	if err := a.EvictPod(context.Background(), "prod/ghost"); err != nil {
		t.Fatalf("missing pod should not error: %v", err)
	}
	if err := a.EvictPod(context.Background(), "malformed"); err == nil {
		t.Fatal("malformed ref must error")
	}
}

func TestEvictionBudgetEnforced(t *testing.T) {
	var objs []k8sruntime.Object
	for _, n := range []string{"a", "b", "c"} {
		objs = append(objs, podOnNode("prod", n, "n1"))
	}
	client := k8sfake.NewClientset(objs...)
	evictionDeletesPod(client)
	a, _ := New(client, Config{Mode: ModeApply, MaxEvictionsPerHour: 2})
	if err := a.EvictPod(context.Background(), "prod/a"); err != nil {
		t.Fatal(err)
	}
	if err := a.EvictPod(context.Background(), "prod/b"); err != nil {
		t.Fatal(err)
	}
	if err := a.EvictPod(context.Background(), "prod/c"); err == nil {
		t.Fatal("third eviction must hit the budget")
	}
}

func TestExecuteFullRemovalPlan(t *testing.T) {
	client := k8sfake.NewClientset(
		nodeObj("n1"), nodeObj("n2"),
		podOnNode("prod", "web-1", "n1"),
	)
	evictionDeletesPod(client)
	a, _ := New(client, Config{Mode: ModeApply, PollInterval: 10 * time.Millisecond, NodeDrainTimeout: time.Second})
	p := &plan.Plan{Steps: []plan.Step{
		{Seq: 1, Type: plan.StepCordonNode, Node: "n1"},
		{Seq: 2, Type: plan.StepEvictPod, Pod: "prod/web-1", Node: "n1"},
		{Seq: 3, Type: plan.StepDeleteNode, Node: "n1"},
	}}
	rep := a.ExecutePlan(context.Background(), p)
	if rep.Failed != 0 || rep.Done != 3 || rep.Aborted {
		t.Fatalf("report: %+v", rep)
	}
	if _, err := client.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{}); err == nil {
		t.Fatal("node should be deleted")
	}
}

func TestAbortOnDrainTimeout(t *testing.T) {
	// Pod stays (no eviction reactor) → WaitNodeEmpty times out → abort.
	client := k8sfake.NewClientset(nodeObj("n1"), podOnNode("prod", "stuck", "n1"))
	a, _ := New(client, Config{Mode: ModeApply, PollInterval: 10 * time.Millisecond, NodeDrainTimeout: 50 * time.Millisecond})
	p := &plan.Plan{Steps: []plan.Step{
		{Seq: 1, Type: plan.StepDeleteNode, Node: "n1"},
		{Seq: 2, Type: plan.StepCordonNode, Node: "n1"}, // must never run
	}}
	rep := a.ExecutePlan(context.Background(), p)
	if !rep.Aborted || rep.Failed != 1 {
		t.Fatalf("expected abort: %+v", rep)
	}
	if len(rep.Steps) != 1 {
		t.Fatalf("remaining steps must not run: %d", len(rep.Steps))
	}
	if _, err := client.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{}); err != nil {
		t.Fatal("node must NOT be deleted on timeout")
	}
}

func TestWaitNodeEmptyIgnoresDaemonSets(t *testing.T) {
	ctrl := true
	dsPod := podOnNode("kube-system", "fluentd-x", "n1")
	dsPod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "fluentd", Controller: &ctrl}}
	donePod := podOnNode("prod", "job-x", "n1")
	donePod.Status.Phase = corev1.PodSucceeded
	client := k8sfake.NewClientset(nodeObj("n1"), dsPod, donePod)
	a, _ := New(client, Config{Mode: ModeApply, PollInterval: 10 * time.Millisecond, NodeDrainTimeout: 100 * time.Millisecond})
	if err := a.WaitNodeEmpty(context.Background(), "n1"); err != nil {
		t.Fatalf("DS + completed pods must not block: %v", err)
	}
}

func TestEvictFailureDoesNotAbortDrain(t *testing.T) {
	// One pod's eviction fails (no reactor → eviction 'succeeds' silently)…
	// use budget exhaustion to force a failure mid-plan instead.
	client := k8sfake.NewClientset(
		nodeObj("n1"),
		podOnNode("prod", "a", "n1"), podOnNode("prod", "b", "n1"), podOnNode("prod", "c", "n1"),
	)
	evictionDeletesPod(client)
	a, _ := New(client, Config{Mode: ModeApply, MaxEvictionsPerHour: 2})
	p := &plan.Plan{Steps: []plan.Step{
		{Seq: 1, Type: plan.StepCordonNode, Node: "n1"},
		{Seq: 2, Type: plan.StepEvictPod, Pod: "prod/a", Node: "n1"},
		{Seq: 3, Type: plan.StepEvictPod, Pod: "prod/b", Node: "n1"},
		{Seq: 4, Type: plan.StepEvictPod, Pod: "prod/c", Node: "n1"}, // budget exhausted → fails
	}}
	rep := a.ExecutePlan(context.Background(), p)
	if rep.Aborted {
		t.Fatal("evict failure must not abort the remaining plan")
	}
	if rep.Failed != 1 || rep.Done != 3 {
		t.Fatalf("report: %+v", rep)
	}
}

func TestResizeFailureDoesNotAbortPlan(t *testing.T) {
	client := k8sfake.NewClientset(nodeObj("n1")) // deployment missing → resize fails
	a, _ := New(client, Config{Mode: ModeApply})
	p := &plan.Plan{Steps: []plan.Step{
		{Seq: 1, Type: plan.StepResizeWorkload, Workload: wref("prod", "ghost"), Container: "app",
			ToReq: model.Resources{MilliCPU: 100}},
		{Seq: 2, Type: plan.StepCordonNode, Node: "n1"},
	}}
	rep := a.ExecutePlan(context.Background(), p)
	if rep.Aborted {
		t.Fatal("resize failure must not abort node-independent steps")
	}
	if rep.Failed != 1 || rep.Done != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

// recordingProvider verifies the actuator hands terminations to the provider.
type recordingProvider struct {
	terminated []string
	fail       bool
}

func (r *recordingProvider) Name() string { return "recording" }
func (r *recordingProvider) Discover(context.Context) ([]provider.NodeGroup, map[string]string, error) {
	return nil, nil, nil
}
func (r *recordingProvider) ScaleTo(context.Context, string, int) error { return nil }
func (r *recordingProvider) TerminateNode(_ context.Context, node, providerID string) error {
	if r.fail {
		return fmt.Errorf("cloud says no")
	}
	r.terminated = append(r.terminated, node+"|"+providerID)
	return nil
}

func TestDeleteNodeCallsProvider(t *testing.T) {
	n := nodeObj("n1")
	n.Spec.ProviderID = "aws:///us-east-1a/i-0abc"
	client := k8sfake.NewClientset(n)
	rec := &recordingProvider{}
	a, _ := New(client, Config{Mode: ModeApply, Provider: rec})
	if err := a.DeleteNode(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if len(rec.terminated) != 1 || rec.terminated[0] != "n1|aws:///us-east-1a/i-0abc" {
		t.Fatalf("provider not invoked correctly: %v", rec.terminated)
	}
}

func TestDeleteNodeProviderFailureFailsStep(t *testing.T) {
	client := k8sfake.NewClientset(nodeObj("n1"))
	a, _ := New(client, Config{Mode: ModeApply, Provider: &recordingProvider{fail: true}})
	if err := a.DeleteNode(context.Background(), "n1"); err == nil {
		t.Fatal("provider failure must fail the step loudly")
	}
}

// ownedPod builds a running pod with a controller owner reference.
func ownedPod(ns, name string, owner metav1.OwnerReference, labels map[string]string) *corev1.Pod {
	ctrl := true
	owner.Controller = &ctrl
	p := podOnNode(ns, name, "n1")
	p.OwnerReferences = []metav1.OwnerReference{owner}
	p.Labels = labels
	return p
}

func TestOwnedBy(t *testing.T) {
	rs := func(name string) metav1.OwnerReference { return metav1.OwnerReference{Kind: "ReplicaSet", Name: name} }
	hash := func(h string) map[string]string { return map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: h} }
	cases := []struct {
		name string
		pod  *corev1.Pod
		ref  model.WorkloadRef
		want bool
	}{
		{"deployment pod matches", ownedPod("prod", "api-abc12-x", rs("api-abc12"), hash("abc12")),
			wref("prod", "api"), true},
		{"sibling deployment with shared prefix must NOT match",
			ownedPod("prod", "api-gateway-zz9-x", rs("api-gateway-zz9"), hash("zz9")),
			wref("prod", "api"), false},
		{"hash label missing → no match (safe: rollout converges it)",
			ownedPod("prod", "api-abc12-x", rs("api-abc12"), nil),
			wref("prod", "api"), false},
		{"replicaset name not built from our hash", ownedPod("prod", "api-other-x", rs("api-other"), hash("abc12")),
			wref("prod", "api"), false},
		{"wrong namespace", ownedPod("stage", "api-abc12-x", rs("api-abc12"), hash("abc12")),
			wref("prod", "api"), false},
		{"no controller", podOnNode("prod", "bare", "n1"), wref("prod", "api"), false},
		{"statefulset direct owner", ownedPod("prod", "db-0", metav1.OwnerReference{Kind: "StatefulSet", Name: "db"}, nil),
			model.WorkloadRef{Kind: model.KindStatefulSet, Namespace: "prod", Name: "db"}, true},
		{"statefulset name mismatch", ownedPod("prod", "db2-0", metav1.OwnerReference{Kind: "StatefulSet", Name: "db2"}, nil),
			model.WorkloadRef{Kind: model.KindStatefulSet, Namespace: "prod", Name: "db"}, false},
		{"unsupported kind", ownedPod("prod", "ds-x", metav1.OwnerReference{Kind: "DaemonSet", Name: "ds"}, nil),
			model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "prod", Name: "ds"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownedBy(tc.pod, tc.ref); got != tc.want {
				t.Fatalf("ownedBy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInPlaceResizeTargetsOnlyOwnedPods(t *testing.T) {
	rs := func(name string) metav1.OwnerReference { return metav1.OwnerReference{Kind: "ReplicaSet", Name: name} }
	client := k8sfake.NewClientset(
		deployment("prod", "api"),
		ownedPod("prod", "api-abc12-1", rs("api-abc12"),
			map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: "abc12"}),
		ownedPod("prod", "api-gateway-zz9-1", rs("api-gateway-zz9"),
			map[string]string{appsv1.DefaultDeploymentUniqueLabelKey: "zz9"}),
	)
	a, _ := New(client, Config{Mode: ModeApply, InPlaceResize: true})
	err := a.ResizeWorkload(context.Background(), wref("prod", "api"), "app",
		model.Resources{MilliCPU: 100}, model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	var resized []string
	for _, act := range client.Actions() {
		if act.GetVerb() == "patch" && act.GetResource().Resource == "pods" && act.GetSubresource() == "resize" {
			resized = append(resized, act.(k8stesting.PatchAction).GetName())
		}
	}
	if len(resized) != 1 || resized[0] != "api-abc12-1" {
		t.Fatalf("in-place resize must touch exactly the owned pod, got %v", resized)
	}
}

func TestExecutePlanNilPlan(t *testing.T) {
	client := k8sfake.NewClientset()
	a, _ := New(client, Config{Mode: ModeApply})
	rep := a.ExecutePlan(context.Background(), nil)
	if rep == nil || rep.Done != 0 || rep.Failed != 0 || rep.Aborted {
		t.Fatalf("nil plan must yield an empty report, got %+v", rep)
	}
	if rep.Finished.IsZero() {
		t.Fatal("Finished must be stamped even for a nil plan")
	}
}

func TestExecutePlanCtxAlreadyCanceled(t *testing.T) {
	client := k8sfake.NewClientset(nodeObj("n1"))
	a, _ := New(client, Config{Mode: ModeApply})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := a.ExecutePlan(ctx, &plan.Plan{Steps: []plan.Step{{Seq: 1, Type: plan.StepCordonNode, Node: "n1"}}})
	if !rep.Aborted || len(rep.Steps) != 0 {
		t.Fatalf("canceled ctx must abort before any step: %+v", rep)
	}
}

func TestUnknownStepSkippedInBothModes(t *testing.T) {
	for _, mode := range []Mode{ModeDryRun, ModeApply} {
		client := k8sfake.NewClientset()
		a, _ := New(client, Config{Mode: mode})
		rep := a.ExecutePlan(context.Background(), &plan.Plan{Steps: []plan.Step{
			{Seq: 1, Type: plan.StepType("teleport-node")},
		}})
		if rep.Skipped != 1 || rep.Done != 0 {
			t.Fatalf("%s: unknown step must be skipped, not counted done: %+v", mode, rep)
		}
	}
}

func TestDryRunFlagsGarbageResize(t *testing.T) {
	client := k8sfake.NewClientset(deployment("prod", "web"))
	a, _ := New(client, Config{Mode: ModeDryRun})
	rep := a.ExecutePlan(context.Background(), &plan.Plan{Steps: []plan.Step{
		{Seq: 1, Type: plan.StepResizeWorkload, Workload: wref("prod", "web"), Container: "app",
			ToReq: model.Resources{MilliCPU: -100}},
		{Seq: 2, Type: plan.StepCordonNode, Node: "n1"},
	}}) // garbage resize must surface on the preview, not on apply day
	if rep.Failed != 1 || rep.Done != 1 || rep.Aborted {
		t.Fatalf("report: %+v", rep)
	}
	for _, act := range client.Actions() {
		t.Fatalf("dry-run performed %s on %s", act.GetVerb(), act.GetResource().Resource)
	}
}

func TestResizeWorkloadRejectsGarbage(t *testing.T) {
	cases := []struct {
		name      string
		container string
		req, lim  model.Resources
	}{
		{"empty container name", "", model.Resources{MilliCPU: 100}, model.Resources{}},
		{"negative cpu request", "app", model.Resources{MilliCPU: -1}, model.Resources{}},
		{"negative memory request", "app", model.Resources{MemoryBytes: -1}, model.Resources{}},
		{"negative cpu limit", "app", model.Resources{}, model.Resources{MilliCPU: math.MinInt64}},
		{"negative memory limit", "app", model.Resources{}, model.Resources{MemoryBytes: -5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := k8sfake.NewClientset(deployment("prod", "web"))
			a, _ := New(client, Config{Mode: ModeApply})
			if err := a.ResizeWorkload(context.Background(), wref("prod", "web"), tc.container, tc.req, tc.lim); err == nil {
				t.Fatal("garbage resize must be rejected")
			}
			if n := len(client.Actions()); n != 0 {
				t.Fatalf("rejected resize must not touch the API (%d actions)", n)
			}
		})
	}
}

func TestResizeWorkloadZeroFieldsLeaveCurrentValues(t *testing.T) {
	// The seed deployment requests cpu=2, memory=4Gi. A memory-only resize
	// must leave the cpu request untouched (zero field ⇒ no change).
	client := k8sfake.NewClientset(deployment("prod", "web"))
	a, _ := New(client, Config{Mode: ModeApply})
	err := a.ResizeWorkload(context.Background(), wref("prod", "web"), "app",
		model.Resources{MemoryBytes: 1 << 30}, model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := client.AppsV1().Deployments("prod").Get(context.Background(), "web", metav1.GetOptions{})
	res := d.Spec.Template.Spec.Containers[0].Resources
	if res.Requests.Cpu().MilliValue() != 2000 {
		t.Fatalf("cpu request must be untouched, got %v", res.Requests.Cpu())
	}
	if res.Requests.Memory().Value() != 1<<30 {
		t.Fatalf("memory request not patched: %v", res.Requests.Memory())
	}
}

func TestDeleteNodeProviderIDReadFailure(t *testing.T) {
	// With a real provider, a transient providerID read failure must fail the
	// step BEFORE deleting the Node: the ID is the only handle on the cloud
	// instance, and losing it means paying for the machine forever.
	client := k8sfake.NewClientset(nodeObj("n1"))
	client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("apiserver hiccup"))
	})
	rec := &recordingProvider{}
	a, _ := New(client, Config{Mode: ModeApply, Provider: rec})
	if err := a.DeleteNode(context.Background(), "n1"); err == nil {
		t.Fatal("providerID read failure must fail the step before the delete")
	}
	if _, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("nodes"), "", "n1"); err != nil {
		t.Fatal("node must NOT be deleted when the providerID could not be read")
	}
	if len(rec.terminated) != 0 {
		t.Fatalf("nothing should be terminated: %v", rec.terminated)
	}
}

func TestDeleteNodeGetFailureToleratedWithoutProvider(t *testing.T) {
	// No cloud provider ⇒ the providerID is not needed; a transient read
	// failure must not block the (otherwise idempotent) node deletion.
	client := k8sfake.NewClientset(nodeObj("n1"))
	client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("apiserver hiccup"))
	})
	a, _ := New(client, Config{Mode: ModeApply})
	if err := a.DeleteNode(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("nodes"), "", "n1"); err == nil {
		t.Fatal("node should be deleted")
	}
}

func TestDeleteNodeAlreadyGoneStillCallsProvider(t *testing.T) {
	// Node object already deleted by someone else: termination must still be
	// confirmed with the provider, never assumed.
	client := k8sfake.NewClientset()
	rec := &recordingProvider{}
	a, _ := New(client, Config{Mode: ModeApply, Provider: rec})
	if err := a.DeleteNode(context.Background(), "ghost"); err != nil {
		t.Fatal(err)
	}
	if len(rec.terminated) != 1 || rec.terminated[0] != "ghost|" {
		t.Fatalf("provider must still be asked to terminate: %v", rec.terminated)
	}
}

// evictionRefusals wires the fake so the first `refuse` eviction attempts get
// a PDB-style 429, then succeed. Returns the attempt counter.
func evictionRefusals(client *k8sfake.Clientset, refuse int) *int32 {
	var attempts int32
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		ca, ok := action.(k8stesting.CreateAction)
		if !ok || ca.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		if int(atomic.AddInt32(&attempts, 1)) <= refuse {
			return true, nil, apierrors.NewTooManyRequests("PDB disruption budget exhausted", 1)
		}
		return true, nil, nil
	})
	return &attempts
}

func TestEvictPodRetriesPDBRefusal(t *testing.T) {
	client := k8sfake.NewClientset(podOnNode("prod", "web-1", "n1"))
	attempts := evictionRefusals(client, 2)
	a, _ := New(client, Config{Mode: ModeApply})
	a.evictBackoff = time.Millisecond
	if err := a.EvictPod(context.Background(), "prod/web-1"); err != nil {
		t.Fatalf("eviction must succeed once the PDB relents: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != 3 {
		t.Fatalf("expected 3 attempts (2 refused + 1 ok), got %d", got)
	}
}

func TestEvictPodGivesUpAfterPDBRefusals(t *testing.T) {
	client := k8sfake.NewClientset(podOnNode("prod", "web-1", "n1"))
	attempts := evictionRefusals(client, 1000)
	a, _ := New(client, Config{Mode: ModeApply})
	a.evictBackoff = time.Millisecond
	err := a.EvictPod(context.Background(), "prod/web-1")
	if err == nil || !apierrors.IsTooManyRequests(errors.Unwrap(err)) {
		t.Fatalf("persistent PDB refusal must surface the 429: %v", err)
	}
	if got := atomic.LoadInt32(attempts); got != 4 {
		t.Fatalf("expected exactly 4 attempts, got %d", got)
	}
}

func TestEvictPodCtxCanceledDuringBackoff(t *testing.T) {
	client := k8sfake.NewClientset(podOnNode("prod", "web-1", "n1"))
	evictionRefusals(client, 1000)
	a, _ := New(client, Config{Mode: ModeApply})
	a.evictBackoff = time.Hour // ctx must win, not the backoff
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := a.EvictPod(ctx, "prod/web-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancellation must not wait out the backoff")
	}
}

func TestEvictPodMalformedRefDoesNotConsumeBudget(t *testing.T) {
	client := k8sfake.NewClientset(podOnNode("prod", "web-1", "n1"))
	evictionDeletesPod(client)
	a, _ := New(client, Config{Mode: ModeApply, MaxEvictionsPerHour: 1})
	for _, bad := range []string{"", "noslash", "/name", "ns/"} {
		if err := a.EvictPod(context.Background(), bad); err == nil {
			t.Fatalf("ref %q must be rejected", bad)
		}
	}
	// The single budget slot must still be available for a real eviction.
	if err := a.EvictPod(context.Background(), "prod/web-1"); err != nil {
		t.Fatalf("malformed refs must not consume budget: %v", err)
	}
}

func TestWaitNodeEmptyIgnoresMirrorPods(t *testing.T) {
	mirror := podOnNode("kube-system", "etcd-n1", "n1")
	mirror.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "hash"}
	client := k8sfake.NewClientset(nodeObj("n1"), mirror)
	a, _ := New(client, Config{Mode: ModeApply, PollInterval: 10 * time.Millisecond, NodeDrainTimeout: 100 * time.Millisecond})
	if err := a.WaitNodeEmpty(context.Background(), "n1"); err != nil {
		t.Fatalf("mirror pods must not block a drain: %v", err)
	}
}

func TestWaitNodeEmptyCtxCanceled(t *testing.T) {
	client := k8sfake.NewClientset(nodeObj("n1"), podOnNode("prod", "stuck", "n1"))
	a, _ := New(client, Config{Mode: ModeApply, PollInterval: 10 * time.Millisecond, NodeDrainTimeout: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := a.WaitNodeEmpty(ctx, "n1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestNewRejectsUnknownMode(t *testing.T) {
	// Anything that isn't exactly dry-run or apply must fail construction:
	// a typo'd mode falling through the dry-run check would apply for real.
	client := k8sfake.NewClientset()
	if _, err := New(client, Config{Mode: Mode("aply")}); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
	if a, err := New(client, Config{}); err != nil || a.cfg.Mode != ModeDryRun {
		t.Fatalf("empty mode must default to dry-run: %v", err)
	}
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("nil client must be rejected")
	}
}

func TestConfigDefaults(t *testing.T) {
	cases := []struct {
		name string
		in   Config
	}{
		{"zero config", Config{}},
		{"negative values", Config{MaxEvictionsPerHour: -5, NodeDrainTimeout: -time.Minute, PollInterval: -time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.Mode != ModeDryRun {
				t.Fatalf("default mode must be dry-run, got %s", got.Mode)
			}
			if got.MaxEvictionsPerHour != 20 || got.NodeDrainTimeout != 5*time.Minute || got.PollInterval != 5*time.Second {
				t.Fatalf("defaults not applied: %+v", got)
			}
			if got.Provider == nil || got.Provider.Name() != "none" || got.Logger == nil {
				t.Fatalf("provider/logger defaults not applied: %+v", got)
			}
		})
	}
	explicit := Config{Mode: ModeApply, MaxEvictionsPerHour: 7, NodeDrainTimeout: time.Minute, PollInterval: time.Second}.withDefaults()
	if explicit.Mode != ModeApply || explicit.MaxEvictionsPerHour != 7 ||
		explicit.NodeDrainTimeout != time.Minute || explicit.PollInterval != time.Second {
		t.Fatalf("explicit values must be kept: %+v", explicit)
	}
}

func TestResourcesToK8s(t *testing.T) {
	cases := []struct {
		name string
		in   model.Resources
		want map[string]string
	}{
		{"zero omits both", model.Resources{}, map[string]string{}},
		{"negatives omitted", model.Resources{MilliCPU: -1, MemoryBytes: math.MinInt64}, map[string]string{}},
		{"cpu only", model.Resources{MilliCPU: 250}, map[string]string{"cpu": "250m"}},
		{"memory only", model.Resources{MemoryBytes: 1 << 30}, map[string]string{"memory": "1073741824"}},
		{"both, at int64 max", model.Resources{MilliCPU: math.MaxInt64, MemoryBytes: math.MaxInt64},
			map[string]string{"cpu": "9223372036854775807m", "memory": "9223372036854775807"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resourcesToK8s(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// FuzzResourcesToK8s checks the quantity-rendering invariants for arbitrary
// inputs: only positive fields are emitted, and everything emitted parses as
// a valid Kubernetes quantity that round-trips to the exact input — a wrong
// suffix here (m vs none) would silently resize workloads by 1000×.
func FuzzResourcesToK8s(f *testing.F) {
	f.Add(int64(0), int64(0))
	f.Add(int64(1), int64(1))
	f.Add(int64(-1), int64(-1))
	f.Add(int64(250), int64(1)<<30)
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64))
	f.Add(int64(math.MinInt64), int64(math.MinInt64))
	f.Fuzz(func(t *testing.T, cpu, mem int64) {
		out := resourcesToK8s(model.Resources{MilliCPU: cpu, MemoryBytes: mem})
		if s, ok := out["cpu"]; ok != (cpu > 0) {
			t.Fatalf("cpu key presence %v for value %d", ok, cpu)
		} else if ok {
			q, err := resource.ParseQuantity(s)
			if err != nil {
				t.Fatalf("cpu %q does not parse: %v", s, err)
			}
			if q.MilliValue() != cpu {
				t.Fatalf("cpu round-trip %d → %q → %d", cpu, s, q.MilliValue())
			}
		}
		if s, ok := out["memory"]; ok != (mem > 0) {
			t.Fatalf("memory key presence %v for value %d", ok, mem)
		} else if ok {
			q, err := resource.ParseQuantity(s)
			if err != nil {
				t.Fatalf("memory %q does not parse: %v", s, err)
			}
			if q.Value() != mem {
				t.Fatalf("memory round-trip %d → %q → %d", mem, s, q.Value())
			}
		}
		if len(out) > 2 {
			t.Fatalf("unexpected keys: %v", out)
		}
	})
}
