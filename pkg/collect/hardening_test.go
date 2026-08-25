package collect

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/agenticode/kilter/pkg/model"
)

// TestHourlyCostAnnotation locks the parsing contract: only finite positive
// values may become HourlyCost. ParseFloat("Inf") returns +Inf with a nil
// error, and a single +Inf node poisons every downstream cost aggregate.
func TestHourlyCostAnnotation(t *testing.T) {
	cases := []struct {
		anno string
		want float64
	}{
		{"0.0777", 0.0777},
		{"1e2", 100},
		{"0.000001", 0.000001},
		{"", 0},
		{"abc", 0},
		{"-1", 0},
		{"0", 0},
		{"-0", 0},
		{"NaN", 0},
		{"nan", 0},
		{"Inf", 0},
		{"+Inf", 0},
		{"-Inf", 0},
		{"Infinity", 0},
		{"1e400", 0},  // overflows float64 → range error
		{"-1e400", 0}, // underflows
		{" 0.5", 0},   // ParseFloat rejects surrounding spaces
		{"0.5$", 0},
	}
	for _, tc := range cases {
		n := testNode()
		n.Annotations[AnnoHourlyCost] = tc.anno
		got := ConvertNode(n).HourlyCost
		if got != tc.want {
			t.Errorf("anno %q: HourlyCost = %v, want %v", tc.anno, got, tc.want)
		}
	}
}

// FuzzHourlyCostAnnotation: whatever string an operator (or attacker with
// node annotate rights) writes, the parsed cost must stay finite and
// non-negative — the invariant every consumer of HourlyCost assumes.
func FuzzHourlyCostAnnotation(f *testing.F) {
	for _, seed := range []string{"0.5", "Inf", "-Inf", "NaN", "1e400", "", "abc", "0x1p10", "999999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, anno string) {
		n := testNode()
		n.Annotations[AnnoHourlyCost] = anno
		got := ConvertNode(n).HourlyCost
		if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
			t.Fatalf("anno %q produced non-finite/negative cost %v", anno, got)
		}
	})
}

func TestClampWindowSeconds(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int32
	}{
		{0, 0},
		{30 * time.Second, 30},
		{30*time.Second + 900*time.Millisecond, 30}, // truncates like the old float path
		{-5 * time.Second, 0},
		{time.Duration(math.MinInt64), 0},
		{time.Duration(math.MaxInt64), math.MaxInt32}, // ~292y in ns → clamp, not platform-dependent wrap
		{time.Duration(math.MaxInt32) * time.Second, math.MaxInt32},
		{(time.Duration(math.MaxInt32) + 1) * time.Second, math.MaxInt32},
	}
	for _, tc := range cases {
		if got := clampWindowSeconds(tc.d); got != tc.want {
			t.Errorf("clampWindowSeconds(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}

// TestCollectUsageGarbage feeds corrupt metrics through Snapshot: negative
// readings must be dropped (a bogus low sample argues for shrinking a
// workload), and absurd windows must clamp deterministically instead of
// becoming platform-dependent int32 wrap that recommend would use as a
// sample weight.
func TestCollectUsageGarbage(t *testing.T) {
	client := k8sfake.NewClientset(
		webPod("web-a", "web-6d4f5"), webPod("web-b", "web-6d4f5"), webPod("web-c", "web-6d4f5"),
	)
	mk := func(pod string, window time.Duration, cs ...metricsv1beta1.ContainerMetrics) metricsv1beta1.PodMetrics {
		return metricsv1beta1.PodMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: "prod"},
			Timestamp:  metav1.Time{Time: time.Now()},
			Window:     metav1.Duration{Duration: window},
			Containers: cs,
		}
	}
	cm := func(name, cpu, mem string) metricsv1beta1.ContainerMetrics {
		return metricsv1beta1.ContainerMetrics{Name: name, Usage: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		}}
	}
	items := []metricsv1beta1.PodMetrics{
		mk("web-a", 30*time.Second,
			cm("app", "137m", "300Mi"),
			cm("bad-cpu", "-100m", "1Mi"),
			cm("bad-mem", "100m", "-1")),
		mk("web-b", time.Duration(math.MaxInt64), cm("app", "1m", "1Mi")),
		mk("web-c", -30*time.Second, cm("app", "1m", "1Mi")),
		mk("ghost-pod", 30*time.Second, cm("app", "1m", "1Mi")), // not in topology → dropped
	}
	metrics := metricsfake.NewSimpleClientset()
	metrics.Fake.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: items}, nil
	})

	c := &Collector{Client: client, Metrics: metrics}
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[string]model.Usage{}
	for _, u := range snap.Usage {
		byUID[u.PodUID+"/"+u.Key.Container] = u
	}
	if len(snap.Usage) != 3 {
		t.Fatalf("want 3 surviving samples (good, huge-window, neg-window), got %d: %+v", len(snap.Usage), snap.Usage)
	}
	if _, ok := byUID["uid-web-a/bad-cpu"]; ok {
		t.Fatal("negative CPU sample must be dropped")
	}
	if _, ok := byUID["uid-web-a/bad-mem"]; ok {
		t.Fatal("negative memory sample must be dropped")
	}
	if u := byUID["uid-web-a/app"]; u.MilliCPU != 137 || u.MemoryBytes != 300<<20 || u.WindowSeconds != 30 {
		t.Fatalf("good sample mangled: %+v", u)
	}
	if u := byUID["uid-web-b/app"]; u.WindowSeconds != math.MaxInt32 {
		t.Fatalf("huge window must clamp to MaxInt32, got %d", u.WindowSeconds)
	}
	if u := byUID["uid-web-c/app"]; u.WindowSeconds != 0 {
		t.Fatalf("negative window must clamp to 0, got %d", u.WindowSeconds)
	}
}

// TestSnapshotAbortsOnTopologyListErrors locks the documented contract:
// every topology list failure aborts the snapshot instead of silently
// producing a partial (= lying) one. The namespace case is the kill switch:
// an unreadable freeze annotation must stop the cycle, never report
// Frozen=false.
func TestSnapshotAbortsOnTopologyListErrors(t *testing.T) {
	resources := []struct {
		resource string
		errFrag  string
	}{
		{"nodes", "collect nodes"},
		{"pods", "collect pods"},
		{"replicasets", "collect replicasets"},
		{"jobs", "collect jobs"},
		{"horizontalpodautoscalers", "collect hpas"},
		{"deployments", "collect deployments"},
		{"statefulsets", "collect statefulsets"},
		{"daemonsets", "collect daemonsets"},
		{"poddisruptionbudgets", "collect pdbs"},
		{"namespaces", "collect namespaces"},
	}
	for _, tc := range resources {
		t.Run(tc.resource, func(t *testing.T) {
			client := k8sfake.NewClientset(testNode())
			boom := errors.New("apiserver on fire")
			client.Fake.PrependReactor("list", tc.resource, func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, boom
			})
			c := &Collector{Client: client}
			snap, err := c.Snapshot(context.Background())
			if err == nil {
				t.Fatalf("list %s failed but Snapshot returned %+v", tc.resource, snap)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("error must wrap the cause, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.errFrag) {
				t.Fatalf("error %q should name the failed list (%q)", err, tc.errFrag)
			}
		})
	}
}

// TestMetricsErrorsDegrade locks the other half of the contract: a metrics
// list failure must NOT abort — topology alone still feeds guards and
// planning; only usage-based rightsizing pauses.
func TestMetricsErrorsDegrade(t *testing.T) {
	client := k8sfake.NewClientset(testNode(), webPod("web-a", "web-6d4f5"))
	metrics := metricsfake.NewSimpleClientset()
	metrics.Fake.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("metrics-server down")
	})
	c := &Collector{Client: client, Metrics: metrics}
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("metrics failure must degrade, not abort: %v", err)
	}
	if len(snap.Nodes) != 1 || len(snap.Pods) != 1 || len(snap.Usage) != 0 {
		t.Fatalf("want topology without usage: nodes=%d pods=%d usage=%d",
			len(snap.Nodes), len(snap.Pods), len(snap.Usage))
	}
}

// TestFreezeSwitch: the kube-system freeze annotation is the operator's
// hands-off switch; namespace mode annotations select per-namespace policy.
func TestFreezeSwitch(t *testing.T) {
	frozen := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "kube-system",
		Annotations: map[string]string{AnnoFreeze: "true"},
	}}
	prod := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "prod",
		Annotations: map[string]string{AnnoMode: "recommend"},
	}}
	c := &Collector{Client: k8sfake.NewClientset(frozen, prod)}
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Frozen {
		t.Fatal("kube-system freeze annotation must set Frozen")
	}
	if snap.NamespaceModes["prod"] != "recommend" {
		t.Fatalf("namespace mode lost: %+v", snap.NamespaceModes)
	}

	// freeze on any other namespace is NOT the kill switch.
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "prod2",
		Annotations: map[string]string{AnnoFreeze: "true"},
	}}
	c2 := &Collector{Client: k8sfake.NewClientset(other)}
	snap2, err := c2.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap2.Frozen {
		t.Fatal("freeze annotation outside kube-system must not freeze the cluster")
	}
}

// TestHPATargetsCPUShapes: both Resource and ContainerResource CPU metrics
// make an HPA react to CPU request changes; memory-only HPAs do not.
func TestHPATargetsCPUShapes(t *testing.T) {
	cases := []struct {
		name    string
		metrics []autoscalingv2.MetricSpec
		want    bool
	}{
		{"resource-cpu", []autoscalingv2.MetricSpec{{
			Type:     autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU},
		}}, true},
		{"container-resource-cpu", []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ContainerResourceMetricSourceType,
			ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
				Name: corev1.ResourceCPU, Container: "app",
			},
		}}, true},
		{"resource-memory-only", []autoscalingv2.MetricSpec{{
			Type:     autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory},
		}}, false},
		{"external-only", []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ExternalMetricSourceType,
		}}, false},
		{"malformed-nil-resource", []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ResourceMetricSourceType, // Resource left nil
		}, {
			Type: autoscalingv2.ContainerResourceMetricSourceType, // ContainerResource left nil
		}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
				Spec:       appsv1.DeploymentSpec{Replicas: i32Ptr(2)},
			}
			hpa := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
					MaxReplicas:    10,
					Metrics:        tc.metrics,
				},
			}
			c := &Collector{Client: k8sfake.NewClientset(deploy, hpa)}
			snap, err := c.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var w *model.WorkloadInfo
			for i := range snap.Workloads {
				if w == nil && snap.Workloads[i].Ref.Name == "web" {
					w = &snap.Workloads[i]
				}
			}
			if w == nil || !w.HasHPA {
				t.Fatalf("workload/HPA missing: %+v", snap.Workloads)
			}
			if w.HPATargetsCPU != tc.want {
				t.Fatalf("HPATargetsCPU = %v, want %v", w.HPATargetsCPU, tc.want)
			}
		})
	}
}

// TestSidecarContainers: native sidecars (restartable init containers) run
// for the pod's whole life and count toward the scheduler's request sum, so
// they must appear in the pod model; run-to-completion init containers must
// not (they would inflate the steady-state sum).
func TestSidecarContainers(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := webPod("web-sc", "web-6d4f5")
	pod.Spec.InitContainers = []corev1.Container{
		{
			Name: "init-schema", // plain init: runs to completion, excluded
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("2"),
			}},
		},
		{
			Name:          "istio-proxy", // sidecar: lives with the pod, included
			RestartPolicy: &always,
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}},
		},
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "istio-proxy", RestartCount: 2,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
		},
	}}
	p := ConvertPod(pod, nil, nil)

	if len(p.Containers) != 2 {
		t.Fatalf("want sidecar+app, got %d containers: %+v", len(p.Containers), p.Containers)
	}
	if p.Containers[0].Name != "istio-proxy" || p.Containers[1].Name != "app" {
		t.Fatalf("container order: %q, %q", p.Containers[0].Name, p.Containers[1].Name)
	}
	sc := p.Containers[0]
	if sc.Requests.MilliCPU != 100 || sc.Requests.MemoryBytes != 128<<20 {
		t.Fatalf("sidecar requests lost: %+v", sc.Requests)
	}
	if sc.RestartCount != 2 || !sc.LastOOMKilled {
		t.Fatalf("sidecar status (from InitContainerStatuses) lost: %+v", sc)
	}
	// The pod's schedulable footprint = app 500m + sidecar 100m; the 2-CPU
	// plain init container must not inflate it.
	if got := p.Requests().MilliCPU; got != 600 {
		t.Fatalf("Requests().MilliCPU = %d, want 600", got)
	}
}

func TestIsExtendedResource(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"cpu", false},
		{"memory", false},
		{"ephemeral-storage", false},
		{"pods", false},
		{"hugepages-2Mi", true},
		{"hugepages-1Gi", true},
		{"nvidia.com/gpu", true},
		{"example.com/fpga", true},
		{"attachable-volumes-aws-ebs", false}, // node-only bookkeeping, no "/"
		{"", false},
	}
	for _, tc := range cases {
		if got := isExtendedResource(tc.name); got != tc.want {
			t.Errorf("isExtendedResource(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHugepagesCollected(t *testing.T) {
	n := testNode()
	n.Status.Allocatable["hugepages-2Mi"] = resource.MustParse("1Gi")
	node := ConvertNode(n)
	if node.ExtendedAllocatable["hugepages-2Mi"] != 1<<30 {
		t.Fatalf("node hugepages allocatable lost: %+v", node.ExtendedAllocatable)
	}
	pod := webPod("hp-pod", "web-6d4f5")
	pod.Spec.Containers[0].Resources.Requests["hugepages-2Mi"] = resource.MustParse("512Mi")
	p := ConvertPod(pod, nil, nil)
	if p.Containers[0].Extended["hugepages-2Mi"] != 512<<20 {
		t.Fatalf("pod hugepages request lost: %+v", p.Containers[0].Extended)
	}
}

func TestProviderFromIDBoundaries(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"aws:///us-east-1a/i-0abc", "aws"},
		{"aws://", "aws"},
		{"aws:/", ""},
		{"gce://proj/zone/inst", "gcp"},
		{"azure:///subscriptions/x", "azure"},
		{"AWS://x", ""}, // providerIDs are lowercase; no case folding
		{"", ""},
		{"kind://docker/kind/kind-control-plane", ""},
		{"awsx//", ""},
	}
	for _, tc := range cases {
		if got := providerFromID(tc.id); got != tc.want {
			t.Errorf("providerFromID(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func FuzzProviderFromID(f *testing.F) {
	for _, seed := range []string{"aws:///x", "gce://", "azure://", "", "aws", strings.Repeat("a", 1000)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		got := providerFromID(id)
		switch got {
		case "", "aws", "gcp", "azure":
		default:
			t.Fatalf("unexpected provider %q for id %q", got, id)
		}
		if got == "aws" && !strings.HasPrefix(id, "aws://") {
			t.Fatalf("aws claimed for %q", id)
		}
	})
}

func TestDaemonSetTemplates(t *testing.T) {
	if got := DaemonSetTemplates(nil); got != nil {
		t.Fatalf("nil snapshot must yield nil, got %+v", got)
	}
	ds := model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "node-exporter"}
	snap := &model.ClusterSnapshot{Pods: []model.PodSpec{
		{UID: "a", Name: "ne-1", Workload: ds},
		{UID: "b", Name: "ne-2", Workload: ds}, // same DS → deduped
		{UID: "c", Name: "web", Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "web"}},
	}}
	got := DaemonSetTemplates(snap)
	if len(got) != 1 || got[0].UID != "a" {
		t.Fatalf("want one template per DS (first pod), got %+v", got)
	}
}

// TestHourlyCostParseFloatContract documents WHY the finite check exists:
// ParseFloat really does return +Inf with a nil error for "Inf".
func TestHourlyCostParseFloatContract(t *testing.T) {
	f, err := strconv.ParseFloat("Inf", 64)
	if err != nil || !math.IsInf(f, 1) {
		t.Skip("ParseFloat contract changed; revisit the finite guard")
	}
}
