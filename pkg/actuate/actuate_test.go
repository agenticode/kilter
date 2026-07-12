package actuate

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
