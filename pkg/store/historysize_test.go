package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// The retention arithmetic, measured rather than assumed.
//
// pkg/api/SUBSTRATE-FINDINGS.md quotes worst-case bytes per cluster, and a
// quoted number nobody recomputes is a number that goes stale the first time
// model.ClusterSnapshot grows a field. This test logs the table the FINDINGS
// cite and asserts the one thing the code depends on: even a very large
// cluster's snapshot frames well under maxSnapshotRecordBytes, so the
// per-record refusal is a guard against pathology and not a routine outcome.

// syntheticCluster builds a snapshot with realistic field population at a
// given scale. No clock: the timestamp is fixed so the framed size is a
// function of the shape alone.
func syntheticCluster(nodes, pods, usage int) *model.ClusterSnapshot {
	at := t0
	snap := &model.ClusterSnapshot{ClusterID: "prod-eks-usw2", Timestamp: at}
	nodeName := func(i int) string {
		return fmt.Sprintf("ip-10-1-%d-%d.us-west-2.compute.internal", i/250, i%250)
	}
	for i := 0; i < nodes; i++ {
		snap.Nodes = append(snap.Nodes, model.NodeSpec{
			Name: nodeName(i),
			Labels: map[string]string{
				"kubernetes.io/hostname": fmt.Sprintf("node-%d", i), "kubernetes.io/arch": "amd64",
				"topology.kubernetes.io/zone": "us-west-2a", "node.kubernetes.io/instance-type": "m5.2xlarge"},
			Ready: true, InstanceType: "m5.2xlarge", Zone: "us-west-2a", Region: "us-west-2",
			Provider: "aws", HourlyCost: 0.384, CreatedAt: at.Add(-72 * time.Hour),
			Capacity:    model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30},
			Allocatable: model.Resources{MilliCPU: 7800, MemoryBytes: 30 << 30},
		})
	}
	ref := func(i int) model.WorkloadRef {
		return model.WorkloadRef{Kind: model.KindDeployment,
			Namespace: fmt.Sprintf("team-%d", i%40), Name: fmt.Sprintf("svc-%d", i%300)}
	}
	for i := 0; i < pods; i++ {
		r := ref(i)
		snap.Pods = append(snap.Pods, model.PodSpec{
			UID: fmt.Sprintf("a1b2c3d4-e5f6-7890-abcd-%012d", i), Name: fmt.Sprintf("%s-7d9f8b6c5-%05d", r.Name, i),
			Namespace: r.Namespace, NodeName: nodeName(i % max(nodes, 1)), Workload: r,
			Phase: "Running", QOSClass: "Burstable", CreatedAt: at.Add(-24 * time.Hour),
			Labels: map[string]string{"app": r.Name, "pod-template-hash": "7d9f8b6c5"},
			Containers: []model.ContainerSpec{{Name: "app",
				Requests: model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30},
				Limits:   model.Resources{MilliCPU: 2000, MemoryBytes: 2 << 30}}},
		})
	}
	for i := 0; i < usage; i++ {
		snap.Usage = append(snap.Usage, model.Usage{
			Key:       model.ContainerKey{Workload: ref(i), Container: "app"},
			PodUID:    fmt.Sprintf("a1b2c3d4-e5f6-7890-abcd-%012d", i),
			Timestamp: at, MilliCPU: 137, MemoryBytes: 812345678, WindowSeconds: 60})
	}
	return snap
}

func TestFramedRecordSizeIsWellUnderTheRecordCap(t *testing.T) {
	ret := DefaultSnapshotRetention()
	for _, c := range []struct{ nodes, pods int }{{10, 60}, {50, 500}, {200, 3000}, {1000, 15000}} {
		snap := syntheticCluster(c.nodes, c.pods, c.pods)
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := encodeSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}
		full := int64(ret.MaxPerCluster) * int64(len(rec))
		bound := full
		if bound > ret.MaxBytesPerCluster {
			bound = ret.MaxBytesPerCluster
		}
		t.Logf("nodes=%5d pods=%6d  raw=%9d B  framed=%8d B (%.0fx)  %d rows=%6.1f MiB  retained=%.1f MiB (%d rows)",
			c.nodes, c.pods, len(raw), len(rec), float64(len(raw))/float64(len(rec)),
			ret.MaxPerCluster, float64(full)/(1<<20), float64(bound)/(1<<20), bound/int64(len(rec)))
		if len(rec) >= maxSnapshotRecordBytes {
			t.Fatalf("a %d-node cluster frames to %d bytes, at or over the %d-byte record cap: "+
				"the per-record refusal has become a routine outcome, not a pathology guard",
				c.nodes, len(rec), maxSnapshotRecordBytes)
		}
	}
}
