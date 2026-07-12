// Package collect translates live Kubernetes state into Kilter's domain model.
// It is deliberately poll-based (LIST every interval) rather than informer-based:
// snapshots are self-consistent, trivially resumable, cheap for the apiserver
// at sane intervals, and immune to cache-drift bugs. kube-state-metrics proved
// this shape at scale.
package collect

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/agenticode/kilter/pkg/model"
)

// Annotations and labels Kilter understands.
const (
	AnnoHourlyCost  = "kilter.dev/hourly-cost"
	AnnoDoNotEvict  = "kilter.dev/do-not-evict"
	AnnoCASafeEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"
	AnnoMode        = "kilter.dev/mode"   // off | recommend | apply
	AnnoFreeze      = "kilter.dev/freeze" // "true" on kube-system = kill switch
)

// Collector gathers ClusterSnapshots from one cluster.
type Collector struct {
	Client  kubernetes.Interface
	Metrics metricsclient.Interface // optional; nil disables usage collection
	// ClusterID names this cluster in the brain. Default "default".
	ClusterID string
	// Namespace limits collection ("" = all namespaces).
	Namespace string
}

// Snapshot lists topology (+ usage when a metrics client is present) and
// returns a self-consistent snapshot. Topology errors abort; metrics errors
// degrade gracefully (topology is still useful without usage).
func (c *Collector) Snapshot(ctx context.Context) (*model.ClusterSnapshot, error) {
	if c.Client == nil {
		return nil, fmt.Errorf("collect: nil kubernetes client")
	}
	id := c.ClusterID
	if id == "" {
		id = "default"
	}
	snap := &model.ClusterSnapshot{ClusterID: id, Timestamp: time.Now().UTC()}

	nodeList, err := c.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("collect nodes: %w", err)
	}
	for i := range nodeList.Items {
		snap.Nodes = append(snap.Nodes, ConvertNode(&nodeList.Items[i]))
	}

	podList, err := c.Client.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("collect pods: %w", err)
	}

	// ReplicaSets index for owner resolution (RS → Deployment).
	rsOwner := map[string]model.WorkloadRef{}
	if rsList, err := c.Client.AppsV1().ReplicaSets(c.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range rsList.Items {
			rs := &rsList.Items[i]
			key := rs.Namespace + "/" + rs.Name
			if or := metav1.GetControllerOf(rs); or != nil && or.Kind == "Deployment" {
				rsOwner[key] = model.WorkloadRef{Kind: model.KindDeployment, Namespace: rs.Namespace, Name: or.Name}
			} else {
				rsOwner[key] = model.WorkloadRef{Kind: model.KindReplicaSet, Namespace: rs.Namespace, Name: rs.Name}
			}
		}
	}
	jobOwner := map[string]model.WorkloadRef{}
	if jobList, err := c.Client.BatchV1().Jobs(c.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range jobList.Items {
			j := &jobList.Items[i]
			key := j.Namespace + "/" + j.Name
			if or := metav1.GetControllerOf(j); or != nil && or.Kind == "CronJob" {
				jobOwner[key] = model.WorkloadRef{Kind: model.KindCronJob, Namespace: j.Namespace, Name: or.Name}
			} else {
				jobOwner[key] = model.WorkloadRef{Kind: model.KindJob, Namespace: j.Namespace, Name: j.Name}
			}
		}
	}

	podByKey := map[string]model.WorkloadRef{} // ns/name → workload, for usage attribution
	for i := range podList.Items {
		p := ConvertPod(&podList.Items[i], rsOwner, jobOwner)
		podByKey[p.Namespace+"/"+p.Name] = p.Workload
		snap.Pods = append(snap.Pods, p)
	}

	c.collectWorkloads(ctx, snap)
	c.collectPDBs(ctx, snap, podList.Items)

	// Namespace-level policy annotations + the cluster freeze switch.
	if nsList, err := c.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err == nil {
		for i := range nsList.Items {
			ns := &nsList.Items[i]
			if m := ns.Annotations[AnnoMode]; m != "" {
				if snap.NamespaceModes == nil {
					snap.NamespaceModes = map[string]string{}
				}
				snap.NamespaceModes[ns.Name] = m
			}
			if ns.Name == "kube-system" && ns.Annotations[AnnoFreeze] == "true" {
				snap.Frozen = true
			}
		}
	}

	if v, err := c.Client.Discovery().ServerVersion(); err == nil && v != nil {
		snap.ServerVersion = v.GitVersion
	}

	if c.Metrics != nil {
		c.collectUsage(ctx, snap, podByKey)
	}
	return snap, nil
}

// collectUsage pulls metrics.k8s.io pod metrics; failures degrade silently
// (the snapshot still carries topology).
func (c *Collector) collectUsage(ctx context.Context, snap *model.ClusterSnapshot, podByKey map[string]model.WorkloadRef) {
	pmList, err := c.Metrics.MetricsV1beta1().PodMetricses(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	uidByKey := map[string]string{}
	for i := range snap.Pods {
		uidByKey[snap.Pods[i].Namespace+"/"+snap.Pods[i].Name] = snap.Pods[i].UID
	}
	for i := range pmList.Items {
		pm := &pmList.Items[i]
		key := pm.Namespace + "/" + pm.Name
		wl, ok := podByKey[key]
		if !ok {
			continue
		}
		ts := pm.Timestamp.Time
		if ts.IsZero() {
			ts = snap.Timestamp
		}
		for _, cm := range pm.Containers {
			cpu := cm.Usage.Cpu().MilliValue()
			mem := cm.Usage.Memory().Value()
			snap.Usage = append(snap.Usage, model.Usage{
				Key:           model.ContainerKey{Workload: wl, Container: cm.Name},
				PodUID:        uidByKey[key],
				Timestamp:     ts,
				MilliCPU:      cpu,
				MemoryBytes:   mem,
				WindowSeconds: int32(pm.Window.Duration.Seconds()),
			})
		}
	}
}

func (c *Collector) collectWorkloads(ctx context.Context, snap *model.ClusterSnapshot) {
	type hpaInfo struct {
		min, max   int32
		targetsCPU bool
		owner      string
	}
	hpas := map[string]hpaInfo{} // Kind/ns/name
	if hpaList, err := c.Client.AutoscalingV2().HorizontalPodAutoscalers(c.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range hpaList.Items {
			h := &hpaList.Items[i]
			info := hpaInfo{max: h.Spec.MaxReplicas}
			if h.Spec.MinReplicas != nil {
				info.min = *h.Spec.MinReplicas
			}
			for _, or := range h.OwnerReferences {
				if or.Kind == "ScaledObject" {
					info.owner = "keda" // KEDA drives this HPA
				}
			}
			for _, m := range h.Spec.Metrics {
				if m.Type == "Resource" && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU {
					info.targetsCPU = true
				}
			}
			key := h.Spec.ScaleTargetRef.Kind + "/" + h.Namespace + "/" + h.Spec.ScaleTargetRef.Name
			hpas[key] = info
		}
	}
	attach := func(ref model.WorkloadRef, replicas, ready int32, lbls, annos map[string]string) {
		w := model.WorkloadInfo{Ref: ref, Replicas: replicas, Ready: ready, Labels: lbls, Mode: annos[AnnoMode]}
		if h, ok := hpas[string(ref.Kind)+"/"+ref.Namespace+"/"+ref.Name]; ok {
			w.HasHPA = true
			w.HPAMinReplicas, w.HPAMaxReplicas, w.HPATargetsCPU = h.min, h.max, h.targetsCPU
			w.HPAOwner = h.owner
		}
		snap.Workloads = append(snap.Workloads, w)
	}
	if l, err := c.Client.AppsV1().Deployments(c.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range l.Items {
			d := &l.Items[i]
			var reps int32 = 1
			if d.Spec.Replicas != nil {
				reps = *d.Spec.Replicas
			}
			attach(model.WorkloadRef{Kind: model.KindDeployment, Namespace: d.Namespace, Name: d.Name},
				reps, d.Status.ReadyReplicas, d.Labels, d.Annotations)
		}
	}
	if l, err := c.Client.AppsV1().StatefulSets(c.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range l.Items {
			s := &l.Items[i]
			var reps int32 = 1
			if s.Spec.Replicas != nil {
				reps = *s.Spec.Replicas
			}
			attach(model.WorkloadRef{Kind: model.KindStatefulSet, Namespace: s.Namespace, Name: s.Name},
				reps, s.Status.ReadyReplicas, s.Labels, s.Annotations)
		}
	}
	if l, err := c.Client.AppsV1().DaemonSets(c.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range l.Items {
			d := &l.Items[i]
			attach(model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: d.Namespace, Name: d.Name},
				d.Status.DesiredNumberScheduled, d.Status.NumberReady, d.Labels, d.Annotations)
		}
	}
}

func (c *Collector) collectPDBs(ctx context.Context, snap *model.ClusterSnapshot, pods []corev1.Pod) {
	pdbList, err := c.Client.PolicyV1().PodDisruptionBudgets(c.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range pdbList.Items {
		k := &pdbList.Items[i]
		p := model.PDB{
			Namespace:          k.Namespace,
			Name:               k.Name,
			DisruptionsAllowed: k.Status.DisruptionsAllowed,
			CurrentHealthy:     k.Status.CurrentHealthy,
			DesiredHealthy:     k.Status.DesiredHealthy,
		}
		if k.Spec.Selector != nil {
			p.Selector = k.Spec.Selector.MatchLabels
			// Exact coverage with full selector semantics (matchExpressions too).
			if sel, err := metav1.LabelSelectorAsSelector(k.Spec.Selector); err == nil {
				for j := range pods {
					if pods[j].Namespace == k.Namespace && sel.Matches(labels.Set(pods[j].Labels)) {
						p.CoveredPodUIDs = append(p.CoveredPodUIDs, string(pods[j].UID))
					}
				}
			}
		}
		snap.PDBs = append(snap.PDBs, p)
	}
}

// ConvertNode maps a corev1.Node to the domain model.
func ConvertNode(n *corev1.Node) model.NodeSpec {
	out := model.NodeSpec{
		Name:          n.Name,
		Labels:        n.Labels,
		Unschedulable: n.Spec.Unschedulable,
		CreatedAt:     n.CreationTimestamp.Time,
		Capacity: model.Resources{
			MilliCPU:    n.Status.Capacity.Cpu().MilliValue(),
			MemoryBytes: n.Status.Capacity.Memory().Value(),
		},
		Allocatable: model.Resources{
			MilliCPU:    n.Status.Allocatable.Cpu().MilliValue(),
			MemoryBytes: n.Status.Allocatable.Memory().Value(),
		},
	}
	for name, q := range n.Status.Allocatable {
		if isExtendedResource(string(name)) && q.Value() > 0 {
			if out.ExtendedAllocatable == nil {
				out.ExtendedAllocatable = map[string]int64{}
			}
			out.ExtendedAllocatable[string(name)] = q.Value()
		}
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			out.Ready = cond.Status == corev1.ConditionTrue
		}
	}
	for _, t := range n.Spec.Taints {
		out.Taints = append(out.Taints, model.Taint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}

	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := n.Labels[k]; ok && v != "" {
				return v
			}
		}
		return ""
	}
	out.InstanceType = get("node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type")
	out.Zone = get("topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone")
	out.Region = get("topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region")
	out.Provider = providerFromID(n.Spec.ProviderID)
	out.Spot = isSpot(n.Labels)
	if _, ok := n.Labels["karpenter.sh/nodepool"]; ok {
		out.ManagedBy = "karpenter"
	} else if _, ok := n.Labels["karpenter.sh/provisioner-name"]; ok {
		out.ManagedBy = "karpenter" // pre-v1 karpenter label
	}
	if v, ok := n.Annotations[AnnoHourlyCost]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			out.HourlyCost = f
		}
	}
	return out
}

// isExtendedResource reports whether a resource name gates scheduling
// beyond cpu/memory (GPUs, FPGAs, vendor devices).
func isExtendedResource(name string) bool {
	switch name {
	case "cpu", "memory", "ephemeral-storage", "pods":
		return false
	}
	return strings.Contains(name, "/") // vendor-namespaced (nvidia.com/gpu, …)
}

func providerFromID(id string) string {
	switch {
	case len(id) >= 6 && id[:6] == "aws://":
		return "aws"
	case len(id) >= 6 && id[:6] == "gce://":
		return "gcp"
	case len(id) >= 8 && id[:8] == "azure://":
		return "azure"
	}
	return ""
}

func isSpot(l map[string]string) bool {
	return l["karpenter.sh/capacity-type"] == "spot" ||
		l["eks.amazonaws.com/capacityType"] == "SPOT" ||
		l["cloud.google.com/gke-spot"] == "true" ||
		l["cloud.google.com/gke-preemptible"] == "true" ||
		l["kubernetes.azure.com/scalesetpriority"] == "spot"
}

// ConvertPod maps a corev1.Pod to the domain model, resolving its workload
// owner through the supplied ReplicaSet/Job indexes.
func ConvertPod(p *corev1.Pod, rsOwner, jobOwner map[string]model.WorkloadRef) model.PodSpec {
	out := model.PodSpec{
		UID:       string(p.UID),
		Name:      p.Name,
		Namespace: p.Namespace,
		Labels:    p.Labels,
		NodeName:  p.Spec.NodeName,
		Phase:     string(p.Status.Phase),
		QOSClass:  string(p.Status.QOSClass),
		CreatedAt: p.CreationTimestamp.Time,
		Workload:  resolveOwner(p, rsOwner, jobOwner),
	}
	if p.Spec.Priority != nil {
		out.Priority = *p.Spec.Priority
	}
	out.PriorityClass = p.Spec.PriorityClassName
	out.NodeSelector = p.Spec.NodeSelector

	statuses := map[string]corev1.ContainerStatus{}
	for _, cs := range p.Status.ContainerStatuses {
		statuses[cs.Name] = cs
	}
	for i := range p.Spec.Containers {
		c := &p.Spec.Containers[i]
		spec := model.ContainerSpec{
			Name: c.Name,
			Requests: model.Resources{
				MilliCPU:    c.Resources.Requests.Cpu().MilliValue(),
				MemoryBytes: c.Resources.Requests.Memory().Value(),
			},
			Limits: model.Resources{
				MilliCPU:    c.Resources.Limits.Cpu().MilliValue(),
				MemoryBytes: c.Resources.Limits.Memory().Value(),
			},
		}
		for name, q := range c.Resources.Requests {
			if isExtendedResource(string(name)) && q.Value() > 0 {
				if spec.Extended == nil {
					spec.Extended = map[string]int64{}
				}
				spec.Extended[string(name)] = q.Value()
			}
		}
		if cs, ok := statuses[c.Name]; ok {
			spec.RestartCount = cs.RestartCount
			if term := cs.LastTerminationState.Terminated; term != nil {
				spec.LastTerminated = term.Reason
				spec.LastOOMKilled = term.Reason == "OOMKilled"
			}
		}
		out.Containers = append(out.Containers, spec)
	}

	for _, v := range p.Spec.Volumes {
		if v.EmptyDir != nil || v.HostPath != nil {
			out.HasLocalStorage = true
			break
		}
	}
	if p.Annotations[AnnoDoNotEvict] == "true" || p.Annotations[AnnoCASafeEvict] == "false" {
		out.DoNotEvict = true
	}

	if aff := p.Spec.Affinity; aff != nil {
		if na := aff.NodeAffinity; na != nil && na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
			for _, term := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
				var mt model.NodeSelectorTerm
				for _, req := range term.MatchExpressions {
					mt.MatchExpressions = append(mt.MatchExpressions, model.NodeSelectorRequirement{
						Key: req.Key, Operator: string(req.Operator), Values: req.Values,
					})
				}
				out.RequiredAffinity = append(out.RequiredAffinity, mt)
			}
		}
		// Self-anti-affinity: required podAntiAffinity whose selector matches
		// this pod's own labels → the workload spreads itself by topology key.
		if pa := aff.PodAntiAffinity; pa != nil {
			for _, term := range pa.RequiredDuringSchedulingIgnoredDuringExecution {
				if term.LabelSelector == nil {
					continue
				}
				sel, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
				if err != nil || !sel.Matches(labels.Set(p.Labels)) {
					continue
				}
				out.AntiAffinityKeys = append(out.AntiAffinityKeys, term.TopologyKey)
			}
		}
	}

	for _, tol := range p.Spec.Tolerations {
		out.Tolerations = append(out.Tolerations, model.Toleration{
			Key: tol.Key, Operator: string(tol.Operator), Value: tol.Value, Effect: string(tol.Effect),
		})
	}
	for _, tsc := range p.Spec.TopologySpreadConstraints {
		c := model.TopologySpreadConstraint{
			MaxSkew: tsc.MaxSkew, TopologyKey: tsc.TopologyKey,
			WhenUnsatisfiable: string(tsc.WhenUnsatisfiable),
		}
		if tsc.LabelSelector != nil {
			c.LabelSelector = tsc.LabelSelector.MatchLabels
		}
		out.TopologySpread = append(out.TopologySpread, c)
	}
	return out
}

func resolveOwner(p *corev1.Pod, rsOwner, jobOwner map[string]model.WorkloadRef) model.WorkloadRef {
	or := metav1.GetControllerOf(p)
	if or == nil {
		return model.WorkloadRef{Kind: model.KindBarePod, Namespace: p.Namespace, Name: p.Name}
	}
	key := p.Namespace + "/" + or.Name
	switch or.Kind {
	case "ReplicaSet":
		if ref, ok := rsOwner[key]; ok {
			return ref
		}
		return model.WorkloadRef{Kind: model.KindReplicaSet, Namespace: p.Namespace, Name: or.Name}
	case "Job":
		if ref, ok := jobOwner[key]; ok {
			return ref
		}
		return model.WorkloadRef{Kind: model.KindJob, Namespace: p.Namespace, Name: or.Name}
	case "StatefulSet":
		return model.WorkloadRef{Kind: model.KindStatefulSet, Namespace: p.Namespace, Name: or.Name}
	case "DaemonSet":
		return model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: p.Namespace, Name: or.Name}
	}
	return model.WorkloadRef{Kind: model.WorkloadKind(or.Kind), Namespace: p.Namespace, Name: or.Name}
}

// DaemonSetTemplates extracts one representative pod per DaemonSet from a
// snapshot, for per-node overhead in planning.
func DaemonSetTemplates(snap *model.ClusterSnapshot) []model.PodSpec {
	seen := map[model.WorkloadRef]bool{}
	var out []model.PodSpec
	for i := range snap.Pods {
		p := snap.Pods[i]
		if p.Workload.Kind != model.KindDaemonSet || seen[p.Workload] {
			continue
		}
		seen[p.Workload] = true
		out = append(out, p)
	}
	return out
}
