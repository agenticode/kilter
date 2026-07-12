package collect

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/agenticode/kilter/pkg/model"
)

func i32Ptr(i int32) *int32 { return &i }

func testNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ip-10-0-1-7",
			Labels: map[string]string{
				"node.kubernetes.io/instance-type": "m5.xlarge",
				"topology.kubernetes.io/zone":      "us-east-1a",
				"topology.kubernetes.io/region":    "us-east-1",
				"karpenter.sh/capacity-type":       "spot",
				"kubernetes.io/hostname":           "ip-10-0-1-7",
			},
			Annotations: map[string]string{AnnoHourlyCost: "0.0777"},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-east-1a/i-0abc",
			Taints: []corev1.Taint{{
				Key: "dedicated", Value: "batch", Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3920m"),
				corev1.ResourceMemory: resource.MustParse("15Gi"),
			},
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: corev1.ConditionTrue,
			}},
		},
	}
}

func TestConvertNode(t *testing.T) {
	n := ConvertNode(testNode())
	if !n.Ready || n.Unschedulable {
		t.Fatal("node should be ready/schedulable")
	}
	if n.InstanceType != "m5.xlarge" || n.Zone != "us-east-1a" || n.Region != "us-east-1" {
		t.Fatalf("identity wrong: %+v", n)
	}
	if n.Provider != "aws" || !n.Spot {
		t.Fatalf("provider/spot wrong: %+v", n)
	}
	if n.HourlyCost != 0.0777 {
		t.Fatalf("cost annotation not read: %v", n.HourlyCost)
	}
	if n.Capacity.MilliCPU != 4000 || n.Allocatable.MilliCPU != 3920 {
		t.Fatalf("cpu wrong: %+v", n)
	}
	if n.Allocatable.MemoryBytes != 15<<30 {
		t.Fatalf("memory wrong: %d", n.Allocatable.MemoryBytes)
	}
	if len(n.Taints) != 1 || n.Taints[0].Effect != "NoSchedule" {
		t.Fatalf("taints wrong: %+v", n.Taints)
	}
}

func TestProviderAndSpotDetection(t *testing.T) {
	if p := providerFromID("gce://proj/zone/inst"); p != "gcp" {
		t.Fatal(p)
	}
	if p := providerFromID("azure:///subscriptions/x"); p != "azure" {
		t.Fatal(p)
	}
	if p := providerFromID("kind://docker/kind/kind-control-plane"); p != "" {
		t.Fatal(p)
	}
	if !isSpot(map[string]string{"cloud.google.com/gke-spot": "true"}) {
		t.Fatal("gke spot missed")
	}
	if !isSpot(map[string]string{"kubernetes.azure.com/scalesetpriority": "spot"}) {
		t.Fatal("azure spot missed")
	}
	if isSpot(map[string]string{"karpenter.sh/capacity-type": "on-demand"}) {
		t.Fatal("on-demand flagged as spot")
	}
}

func webPod(name, rsName string) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "prod", UID: types.UID("uid-" + name),
			Labels: map[string]string{"app": "web"},
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet", Name: rsName, Controller: &ctrl,
			}},
			Annotations: map[string]string{},
		},
		Spec: corev1.PodSpec{
			NodeName: "ip-10-0-1-7",
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
				},
			}},
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
						TopologyKey:   "kubernetes.io/hostname",
					}},
				},
			},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone",
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			}},
			Tolerations: []corev1.Toleration{{
				Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "batch",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 3,
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
				},
			}},
		},
	}
}

func TestConvertPodFull(t *testing.T) {
	rsOwner := map[string]model.WorkloadRef{
		"prod/web-6d4f5": {Kind: model.KindDeployment, Namespace: "prod", Name: "web"},
	}
	p := ConvertPod(webPod("web-6d4f5-abc", "web-6d4f5"), rsOwner, nil)

	if p.Workload.Kind != model.KindDeployment || p.Workload.Name != "web" {
		t.Fatalf("owner resolution failed: %+v", p.Workload)
	}
	c := p.Containers[0]
	if c.Requests.MilliCPU != 500 || c.Requests.MemoryBytes != 1<<30 {
		t.Fatalf("requests wrong: %+v", c.Requests)
	}
	if c.Limits.MilliCPU != 1000 || c.Limits.MemoryBytes != 2<<30 {
		t.Fatalf("limits wrong: %+v", c.Limits)
	}
	if c.RestartCount != 3 || !c.LastOOMKilled {
		t.Fatalf("OOM state lost: %+v", c)
	}
	if len(p.AntiAffinityKeys) != 1 || p.AntiAffinityKeys[0] != "kubernetes.io/hostname" {
		t.Fatalf("self anti-affinity not detected: %v", p.AntiAffinityKeys)
	}
	if len(p.TopologySpread) != 1 || p.TopologySpread[0].TopologyKey != "topology.kubernetes.io/zone" {
		t.Fatalf("spread lost: %+v", p.TopologySpread)
	}
	if len(p.Tolerations) != 1 || !p.Tolerations[0].Tolerates(model.Taint{Key: "dedicated", Value: "batch", Effect: "NoSchedule"}) {
		t.Fatalf("tolerations lost: %+v", p.Tolerations)
	}
	if p.QOSClass != "Burstable" || p.Phase != "Running" {
		t.Fatalf("status lost: %+v", p)
	}
	if p.HasLocalStorage || p.DoNotEvict {
		t.Fatal("flags should be unset")
	}
}

func TestConvertPodFlags(t *testing.T) {
	pod := webPod("web-x", "web-6d4f5")
	pod.Spec.Volumes = []corev1.Volume{{Name: "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	pod.Annotations[AnnoCASafeEvict] = "false"
	p := ConvertPod(pod, nil, nil)
	if !p.HasLocalStorage {
		t.Fatal("emptyDir must set HasLocalStorage")
	}
	if !p.DoNotEvict {
		t.Fatal("CA safe-to-evict=false must set DoNotEvict")
	}
	// Non-self anti-affinity (selector for a different app) must NOT register.
	pod2 := webPod("web-y", "web-6d4f5")
	pod2.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].
		LabelSelector.MatchLabels = map[string]string{"app": "other"}
	p2 := ConvertPod(pod2, nil, nil)
	if len(p2.AntiAffinityKeys) != 0 {
		t.Fatal("anti-affinity against another app must not become self key")
	}
}

func TestBarePodOwner(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "d", UID: "u1"}}
	p := ConvertPod(pod, nil, nil)
	if p.Workload.Kind != model.KindBarePod {
		t.Fatalf("bare pod kind: %v", p.Workload.Kind)
	}
}

func TestSnapshotEndToEnd(t *testing.T) {
	ctrl := true
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", Labels: map[string]string{"app": "web"}},
		Spec:       appsv1.DeploymentSpec{Replicas: i32Ptr(2)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-6d4f5", Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: &ctrl}},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "backup-123", Namespace: "prod",
			OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "backup", Controller: &ctrl}},
		},
	}
	jobPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "backup-123-xyz", Namespace: "prod", UID: "uid-job",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Job", Name: "backup-123", Controller: &ctrl}},
		},
		Spec: corev1.PodSpec{NodeName: "ip-10-0-1-7", Containers: []corev1.Container{{Name: "app"}}},
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"},
			MinReplicas:    i32Ptr(2), MaxReplicas: 10,
			Metrics: []autoscalingv2.MetricSpec{{
				Type:     autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU},
			}},
		},
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "web-pdb", Namespace: "prod"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			// matchExpressions: the case map-selectors cannot express.
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"},
				}},
			},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 1},
	}

	client := k8sfake.NewClientset(
		testNode(), deploy, rs, job, hpa, pdb,
		webPod("web-6d4f5-abc", "web-6d4f5"), jobPod,
	)
	pm := metricsv1beta1.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "web-6d4f5-abc", Namespace: "prod"},
		Timestamp:  metav1.Time{Time: time.Now()},
		Window:     metav1.Duration{Duration: 30 * time.Second},
		Containers: []metricsv1beta1.ContainerMetrics{{
			Name: "app",
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("137m"),
				corev1.ResourceMemory: resource.MustParse("300Mi"),
			},
		}},
	}
	metrics := metricsfake.NewSimpleClientset()
	// The metrics fake tracker mis-maps PodMetrics' GVR, so List through the
	// typed client comes back empty; serve the list via a reactor instead.
	metrics.Fake.PrependReactor("list", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, &metricsv1beta1.PodMetricsList{Items: []metricsv1beta1.PodMetrics{pm}}, nil
	})

	c := &Collector{Client: client, Metrics: metrics, ClusterID: "test-cluster"}
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.ClusterID != "test-cluster" || len(snap.Nodes) != 1 || len(snap.Pods) != 2 {
		t.Fatalf("shape wrong: nodes=%d pods=%d", len(snap.Nodes), len(snap.Pods))
	}

	var web, jp *model.PodSpec
	for i := range snap.Pods {
		switch snap.Pods[i].Name {
		case "web-6d4f5-abc":
			web = &snap.Pods[i]
		case "backup-123-xyz":
			jp = &snap.Pods[i]
		}
	}
	if web.Workload.Kind != model.KindDeployment || web.Workload.Name != "web" {
		t.Fatalf("RS→Deployment resolution failed: %+v", web.Workload)
	}
	if jp.Workload.Kind != model.KindCronJob || jp.Workload.Name != "backup" {
		t.Fatalf("Job→CronJob resolution failed: %+v", jp.Workload)
	}

	// Workloads + HPA flags.
	var found bool
	for _, w := range snap.Workloads {
		if w.Ref.Kind == model.KindDeployment && w.Ref.Name == "web" {
			found = true
			if !w.HasHPA || !w.HPATargetsCPU || w.HPAMaxReplicas != 10 {
				t.Fatalf("HPA info lost: %+v", w)
			}
		}
	}
	if !found {
		t.Fatal("deployment workload missing")
	}

	// PDB with matchExpressions covers the web pod by UID.
	if len(snap.PDBs) != 1 {
		t.Fatalf("pdbs: %d", len(snap.PDBs))
	}
	if !snap.PDBs[0].Covers(web) {
		t.Fatal("PDB matchExpressions coverage failed")
	}
	if snap.PDBs[0].Covers(jp) {
		t.Fatal("PDB must not cover job pod")
	}

	// Usage attributed to the deployment's container key.
	if len(snap.Usage) != 1 {
		t.Fatalf("usage samples: %d", len(snap.Usage))
	}
	u := snap.Usage[0]
	if u.Key.Workload.Name != "web" || u.MilliCPU != 137 || u.MemoryBytes != 300<<20 {
		t.Fatalf("usage wrong: %+v", u)
	}
	if u.WindowSeconds != 30 {
		t.Fatalf("window lost: %d", u.WindowSeconds)
	}

	// DaemonSet template helper works on empty case.
	if got := DaemonSetTemplates(snap); len(got) != 0 {
		t.Fatalf("unexpected DS templates: %d", len(got))
	}
}

func TestSnapshotSurvivesMissingMetrics(t *testing.T) {
	client := k8sfake.NewClientset(testNode())
	c := &Collector{Client: client} // no metrics client
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Nodes) != 1 || len(snap.Usage) != 0 {
		t.Fatal("topology-only snapshot expected")
	}
	if snap.ClusterID != "default" {
		t.Fatalf("default cluster id: %q", snap.ClusterID)
	}
}
