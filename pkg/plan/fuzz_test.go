package plan

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/safety"
)

// clusterFromFuzz deterministically derives a small adversarial cluster and
// config from raw fuzz bytes. Layout: byte 0 = node count / PDB & removal
// knobs, then one flag byte per node, then 3 bytes per pod
// (cpu, memory, placement+kind flags).
func clusterFromFuzz(data []byte) (*model.ClusterSnapshot, Config) {
	if len(data) < 3 {
		return nil, Config{}
	}
	nodeCount := int(data[0])%4 + 1
	cfg := DefaultConfig()
	cfg.MaxNodeRemovals = int(data[0]/4)%4 + 1
	pdbAllowed := int32(data[0]/16) % 3

	var nodes []model.NodeSpec
	i := 1
	for n := 0; n < nodeCount && i < len(data); n++ {
		flags := data[i]
		i++
		node := m5xlarge(fmt.Sprintf("node-%d", n))
		if flags&1 != 0 {
			node.Ready = false
		}
		if flags&2 != 0 {
			node.Unschedulable = true
		}
		if flags&4 != 0 {
			node.Labels["node-role.kubernetes.io/control-plane"] = ""
		}
		if flags&8 != 0 {
			node.ManagedBy = "karpenter"
		}
		if flags&16 != 0 {
			node.Spot = true
		}
		if flags&32 != 0 {
			node.HourlyCost = 0.5
		}
		if flags&64 != 0 {
			node.Allocatable = model.Resources{MilliCPU: 2000, MemoryBytes: 8 << 30}
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		return nil, Config{}
	}

	var pods []model.PodSpec
	for pi := 0; i+2 < len(data) && pi < 40; pi++ {
		cpu, mem, flags := int64(data[i]), int64(data[i+1]), data[i+2]
		i += 3
		wl := fmt.Sprintf("w%d", pi%5)
		p := runningPod(fmt.Sprintf("p%d", pi), "", wl, cpu*40, mem*64)
		if flags&128 == 0 { // else pending: no node
			p.NodeName = nodes[int(flags&3)%len(nodes)].Name
		}
		if flags&4 != 0 {
			p.DoNotEvict = true
		}
		if flags&8 != 0 {
			p.Workload.Kind = model.KindDaemonSet
		}
		if flags&16 != 0 {
			p.Workload.Kind = model.KindBarePod
		}
		if flags&32 != 0 {
			p.Phase = "Succeeded"
		}
		if flags&64 != 0 {
			p.HasLocalStorage = true
		}
		pods = append(pods, p)
	}

	snap := snapshot(nodes, pods)
	snap.PDBs = []model.PDB{{
		Namespace: "default", Name: "w0-pdb",
		Selector: map[string]string{"app": "w0"}, DisruptionsAllowed: pdbAllowed,
	}}
	return snap, cfg
}

// FuzzBuildInvariants hammers Build with adversarial clusters and asserts the
// invariants a decision-critical planner must never break, whatever the input:
// bounded removals, non-negative money math, budget-respecting evictions,
// determinism, and a read-only snapshot.
func FuzzBuildInvariants(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{7, 0, 0, 10, 4, 1, 200, 200, 2, 30, 3, 3})
	f.Add([]byte{255, 1, 2, 4, 8, 255, 255, 128, 0, 0, 32, 90, 8, 16, 10, 10, 4})
	f.Add([]byte{16, 64, 16, 8, 50, 5, 0, 60, 6, 1, 70, 7, 2, 80, 8, 3})

	f.Fuzz(func(t *testing.T, data []byte) {
		snap, cfg := clusterFromFuzz(data)
		if snap == nil {
			t.Skip()
		}
		before, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Build(snap, nil, pricing.Embedded(), cfg)
		if err != nil {
			t.Fatalf("Build errored on valid snapshot: %v", err)
		}

		// The snapshot is read-only input.
		after, _ := json.Marshal(snap)
		if string(before) != string(after) {
			t.Fatal("Build mutated the snapshot")
		}

		// Rebuilding must be byte-identical in intent.
		p2, err := Build(snap, nil, pricing.Embedded(), cfg)
		if err != nil || p.Fingerprint != p2.Fingerprint {
			t.Fatalf("nondeterministic plan: %s vs %s (err %v)", p.Fingerprint, p2.Fingerprint, err)
		}

		if len(p.Removals) > cfg.MaxNodeRemovals {
			t.Fatalf("removals %d exceed bound %d", len(p.Removals), cfg.MaxNodeRemovals)
		}

		// Money math: 0 ≤ projected ≤ current; savings consistent.
		if p.ProjectedHourlyUSD < -1e-9 || p.ProjectedHourlyUSD > p.CurrentHourlyUSD+1e-9 {
			t.Fatalf("projected $%v outside [0, current $%v]", p.ProjectedHourlyUSD, p.CurrentHourlyUSD)
		}
		wantSavings := (p.CurrentHourlyUSD - p.ProjectedHourlyUSD) * pricing.HoursPerMonth
		if diff := p.SavingsMonthlyUSD - wantSavings; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("savings %v inconsistent with removal costs (want %v)", p.SavingsMonthlyUSD, wantSavings)
		}

		assertSeqContiguous(t, p)

		removed := map[string]bool{}
		for _, r := range p.Removals {
			if removed[r.Node] {
				t.Fatalf("node %s removed twice", r.Node)
			}
			removed[r.Node] = true
		}

		podByUID := map[string]*model.PodSpec{}
		for i := range snap.Pods {
			podByUID[snap.Pods[i].UID] = &snap.Pods[i]
		}
		// Nodes that must never be touched: broken, foreign-owned, control
		// plane, or pinned by a live unevictable pod.
		for i := range snap.Nodes {
			n := &snap.Nodes[i]
			if !removed[n.Name] {
				continue
			}
			if !n.Ready || n.Unschedulable || isControlPlane(n) || n.ManagedBy != "" {
				t.Fatalf("removed untouchable node %s (%+v)", n.Name, n)
			}
			for j := range snap.Pods {
				pd := &snap.Pods[j]
				if pd.NodeName != n.Name || isTerminal(pd) {
					continue
				}
				if blocks, why := safety.BlocksDrain(pd); blocks {
					t.Fatalf("removed node %s pinned by pod %s: %s", n.Name, pd.UID, why)
				}
			}
		}

		// Evict steps reference real, live, movable pods, each moved at most
		// once, and always onto a node the plan itself does not remove.
		evictedOnce := map[string]bool{}
		pdbSpent := 0
		for _, s := range p.Steps {
			if s.Type != StepEvictPod {
				continue
			}
			pd := podByUID[s.PodUID]
			if pd == nil {
				t.Fatalf("evict step for unknown pod %q", s.PodUID)
			}
			if isTerminal(pd) || pd.Workload.Kind == model.KindDaemonSet || pd.DoNotEvict {
				t.Fatalf("evict step for unmovable/terminal pod %q", s.PodUID)
			}
			if s.TargetNode == "" || !removed[s.Node] {
				t.Fatalf("evict step must move a pod off a removed node: %+v", s)
			}
			if removed[s.TargetNode] {
				t.Fatalf("pod %s targeted at node %s that the plan removes", s.PodUID, s.TargetNode)
			}
			if evictedOnce[s.PodUID] {
				t.Fatalf("pod %s evicted twice in one plan", s.PodUID)
			}
			evictedOnce[s.PodUID] = true
			if snap.PDBs[0].Covers(pd) {
				pdbSpent++
			}
		}
		if pdbSpent > int(snap.PDBs[0].DisruptionsAllowed) {
			t.Fatalf("plan spends %d disruptions, PDB allows %d", pdbSpent, snap.PDBs[0].DisruptionsAllowed)
		}
	})
}
