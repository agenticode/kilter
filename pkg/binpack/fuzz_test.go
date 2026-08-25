package binpack

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// FuzzScheduleInvariants throws randomly sized nodes and adversarial pods
// (zero, negative, and oversized requests; anti-affinity; garbage spread
// constraints) at Schedule and asserts the accounting invariants that the
// rest of kilter's decision math depends on.
func FuzzScheduleInvariants(f *testing.F) {
	f.Add(int64(1), uint8(3), uint8(20))
	f.Add(int64(42), uint8(1), uint8(0))
	f.Add(int64(7), uint8(8), uint8(60))
	f.Add(int64(-99), uint8(0), uint8(5))
	f.Add(int64(1234), uint8(12), uint8(80))
	f.Fuzz(func(t *testing.T, seed int64, nNodes, nPods uint8) {
		if nNodes > 12 || nPods > 80 {
			t.Skip("keep individual executions fast")
		}
		rng := rand.New(rand.NewSource(seed))
		nodes := make([]model.NodeSpec, 0, nNodes)
		for i := 0; i < int(nNodes); i++ {
			nodes = append(nodes, node(fmt.Sprintf("n%02d", i), rng.Int63n(16001), rng.Int63n(65), nil))
		}
		var pods []*model.PodSpec
		for i := 0; i < int(nPods); i++ {
			if rng.Intn(10) == 0 {
				pods = append(pods, nil) // nil entries must be tolerated
				continue
			}
			p := pod(fmt.Sprintf("p%03d", i),
				rng.Int63n(20000)-4000, // includes negative and node-dwarfing CPU
				rng.Int63n(96<<10)-8<<10,
				fmt.Sprintf("w%d", rng.Intn(4)))
			if rng.Intn(4) == 0 {
				p.AntiAffinityKeys = []string{"kubernetes.io/hostname"}
			}
			if rng.Intn(5) == 0 {
				p.TopologySpread = []model.TopologySpreadConstraint{{
					MaxSkew:           int32(rng.Intn(3)), // 0 is invalid per K8s; must not brick
					TopologyKey:       "kubernetes.io/hostname",
					WhenUnsatisfiable: "DoNotSchedule",
				}}
			}
			pods = append(pods, p)
		}

		cs := NewClusterState(nodes, nil)
		assign, failed := cs.Schedule(pods)

		// Every non-nil pod is accounted for exactly once.
		nonNil := 0
		for _, p := range pods {
			if p != nil {
				nonNil++
			}
		}
		if len(assign)+len(failed) != nonNil {
			t.Fatalf("pod conservation broken: %d assigned + %d failed != %d pods",
				len(assign), len(failed), nonNil)
		}

		// Per-node accounting: Free = Allocatable − Σ clamped requests, never
		// negative for scheduled (not force-placed) pods, PodCount within cap.
		for _, ns := range cs.Nodes() {
			want := ns.Spec.Allocatable
			for _, p := range ns.Pods() {
				want = want.Sub(requestsOf(p))
			}
			if ns.Free != want {
				t.Fatalf("node %s free drift: got %s want %s", ns.Spec.Name, ns.Free, want)
			}
			if ns.Free.MilliCPU < 0 || ns.Free.MemoryBytes < 0 {
				t.Fatalf("node %s oversubscribed by Schedule: %s", ns.Spec.Name, ns.Free)
			}
			if ns.PodCount != len(ns.Pods()) || ns.PodCount > ns.MaxPods {
				t.Fatalf("node %s pod count drift: %d pods, cap %d", ns.Spec.Name, ns.PodCount, ns.MaxPods)
			}
		}
		for uid, n := range assign {
			if _, ok := cs.Node(n); !ok {
				t.Fatalf("pod %s assigned to unknown node %s", uid, n)
			}
		}

		// Round trip: removing everything restores pristine capacity (place and
		// remove use the same clamping, topology counts drain to zero).
		var placed []*model.PodSpec
		for _, p := range pods {
			if p != nil && assign[p.UID] != "" {
				placed = append(placed, p)
				if err := cs.Remove(p.UID, assign[p.UID]); err != nil {
					t.Fatalf("remove %s: %v", p.UID, err)
				}
			}
		}
		for _, ns := range cs.Nodes() {
			if ns.Free != ns.Spec.Allocatable || ns.PodCount != 0 {
				t.Fatalf("node %s not restored after full removal: free=%s count=%d",
					ns.Spec.Name, ns.Free, ns.PodCount)
			}
		}

		// Re-scheduling the previously placed pods on the now-empty cluster is
		// deterministic and must reproduce the original assignment; leftover
		// phantom topology counts would show up here as spurious failures.
		assign2, failed2 := cs.Schedule(placed)
		if len(failed2) != 0 {
			t.Fatalf("re-schedule after full removal failed: %+v", failed2)
		}
		for uid, n := range assign {
			if assign2[uid] != n {
				t.Fatalf("re-schedule diverged for %s: %s vs %s", uid, n, assign2[uid])
			}
		}
	})
}

// TestPlanInvariantsRandomized is a seeded property test over PlanNodes:
// pod conservation, no overpacked node, and internally consistent cost math,
// across a spread of random workload shapes.
func TestPlanInvariantsRandomized(t *testing.T) {
	cands := pricing.Embedded().Candidates("aws", "amd64")
	for seed := int64(0); seed < 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var pods []*model.PodSpec
		for i, n := 0, rng.Intn(60); i < n; i++ {
			pods = append(pods, pod(fmt.Sprintf("p%02d", i),
				rng.Int63n(4000), rng.Int63n(8<<10), fmt.Sprintf("w%d", i%5)))
		}
		plan := PlanNodes(pods, cands, PlanOptions{})

		seen := map[string]bool{}
		var sumHourly float64
		for _, n := range plan.Nodes {
			var used model.Resources
			for _, uid := range n.PodUIDs {
				if seen[uid] {
					t.Fatalf("seed %d: pod %s placed twice", seed, uid)
				}
				seen[uid] = true
				for _, p := range pods {
					if p.UID == uid {
						used = used.Add(requestsOf(p))
					}
				}
			}
			if used != n.Used {
				t.Fatalf("seed %d: node %s Used mismatch: %s vs %s", seed, n.Name, n.Used, used)
			}
			if !n.Allocatable.Fits(n.Used) {
				t.Fatalf("seed %d: node %s overpacked: used %s alloc %s", seed, n.Name, n.Used, n.Allocatable)
			}
			sumHourly += n.HourlyUSD
		}
		for _, u := range plan.Unschedulable {
			if seen[u.Pod.UID] {
				t.Fatalf("seed %d: pod %s both placed and unschedulable", seed, u.Pod.UID)
			}
			seen[u.Pod.UID] = true
		}
		if len(seen) != len(pods) {
			t.Fatalf("seed %d: conservation broken: %d accounted, %d pods", seed, len(seen), len(pods))
		}
		if math.Abs(plan.TotalHourlyUSD-sumHourly) > 1e-9 {
			t.Fatalf("seed %d: TotalHourlyUSD %v != Σ node prices %v", seed, plan.TotalHourlyUSD, sumHourly)
		}
		if plan.TotalMonthlyUSD != plan.TotalHourlyUSD*pricing.HoursPerMonth {
			t.Fatalf("seed %d: monthly/hourly inconsistent", seed)
		}
	}
}
