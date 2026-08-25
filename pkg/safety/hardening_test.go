package safety

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

func ref(k model.WorkloadKind, ns, n string) model.WorkloadRef {
	return model.WorkloadRef{Kind: k, Namespace: ns, Name: n}
}

// ---------------------------------------------------------------- CanEvict

func TestCanEvictTable(t *testing.T) {
	cases := []struct {
		name   string
		pod    *model.PodSpec
		wantOK bool
	}{
		{"nil pod is never evictable", nil, false},
		{"plain deployment pod", &model.PodSpec{Workload: dep("d", "w")}, true},
		{"statefulset pod", &model.PodSpec{Workload: ref(model.KindStatefulSet, "d", "s")}, true},
		{"job pod", &model.PodSpec{Workload: ref(model.KindJob, "d", "j")}, true},
		{"cronjob pod", &model.PodSpec{Workload: ref(model.KindCronJob, "d", "c")}, true},
		{"replicaset pod", &model.PodSpec{Workload: ref(model.KindReplicaSet, "d", "rs")}, true},
		{"bare pod", &model.PodSpec{Workload: ref(model.KindBarePod, "d", "p")}, false},
		{"daemonset pod", &model.PodSpec{Workload: ref(model.KindDaemonSet, "d", "ds")}, false},
		{"do-not-evict", &model.PodSpec{Workload: dep("d", "w"), DoNotEvict: true}, false},
		{"local storage", &model.PodSpec{Workload: dep("d", "w"), HasLocalStorage: true}, false},
		// Documented current behaviour: an unclassified Kind is not one of the
		// pinned kinds, so it stays evictable. Collectors always fill Kind
		// (bare pods become KindBarePod), so this is unreachable in practice.
		{"unknown kind stays evictable", &model.PodSpec{Workload: ref("Frobnicator", "d", "x")}, true},
		{"zero-value pod stays evictable", &model.PodSpec{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := CanEvict(c.pod)
			if ev.OK != c.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q)", ev.OK, c.wantOK, ev.Reason)
			}
			// Invariant: a refusal always carries a reason, an approval never does.
			if !ev.OK && ev.Reason == "" {
				t.Error("refusal must carry a reason")
			}
			if ev.OK && ev.Reason != "" {
				t.Errorf("approval must not carry a reason, got %q", ev.Reason)
			}
		})
	}
}

// TestCanEvictPrecedence pins the order the switch encodes: the operator's
// opt-out annotation is reported before any structural reason, so an operator
// who annotated a pod is told that, not something else.
func TestCanEvictPrecedence(t *testing.T) {
	all := &model.PodSpec{
		Workload:        ref(model.KindBarePod, "d", "p"),
		DoNotEvict:      true,
		HasLocalStorage: true,
	}
	ev := CanEvict(all)
	if ev.OK {
		t.Fatal("pod with every pin must not be evictable")
	}
	if want := "pod opted out (do-not-evict annotation)"; ev.Reason != want {
		t.Fatalf("reason = %q, want the opt-out reason %q", ev.Reason, want)
	}
}

// TestCanEvictDoesNotMutate guards against the classifier quietly rewriting the
// snapshot it is handed; callers reuse these pods for scheduling afterwards.
func TestCanEvictDoesNotMutate(t *testing.T) {
	p := &model.PodSpec{
		UID: "u1", Name: "web-a", Namespace: "prod",
		Workload: dep("prod", "web"), Labels: map[string]string{"app": "web"},
		Containers: []model.ContainerSpec{{Name: "app", RestartCount: 3}},
	}
	before := fmt.Sprintf("%+v", *p)
	CanEvict(p)
	BlocksDrain(p)
	if after := fmt.Sprintf("%+v", *p); after != before {
		t.Fatalf("pod mutated:\n before %s\n after  %s", before, after)
	}
}

// --------------------------------------------------------------- BlocksDrain

func TestBlocksDrainTable(t *testing.T) {
	cases := []struct {
		name       string
		pod        *model.PodSpec
		wantBlocks bool
	}{
		{"nil pod blocks: unclassifiable means keep the node", nil, true},
		{"plain deployment pod", &model.PodSpec{Workload: dep("d", "w")}, false},
		{"daemonset pod dies with the node", &model.PodSpec{Workload: ref(model.KindDaemonSet, "d", "ds")}, false},
		// The DaemonSet rule wins over every other pin: a DS pod cannot hold a
		// node hostage, because it does not survive the node either way.
		{"daemonset + do-not-evict", &model.PodSpec{Workload: ref(model.KindDaemonSet, "d", "ds"), DoNotEvict: true}, false},
		{"daemonset + local storage", &model.PodSpec{Workload: ref(model.KindDaemonSet, "d", "ds"), HasLocalStorage: true}, false},
		{"bare pod", &model.PodSpec{Workload: ref(model.KindBarePod, "d", "p")}, true},
		{"do-not-evict", &model.PodSpec{Workload: dep("d", "w"), DoNotEvict: true}, true},
		{"local storage", &model.PodSpec{Workload: dep("d", "w"), HasLocalStorage: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks, why := BlocksDrain(c.pod)
			if blocks != c.wantBlocks {
				t.Fatalf("blocks = %v, want %v (reason %q)", blocks, c.wantBlocks, why)
			}
			// Invariant: blocking always explains itself, passing never does.
			if blocks && why == "" {
				t.Error("blocking must carry a reason")
			}
			if !blocks && why != "" {
				t.Errorf("non-blocking must not carry a reason, got %q", why)
			}
		})
	}
}

// TestBlocksDrainAgreesWithCanEvict pins the relationship between the two
// entry points for every pod that is not a DaemonSet pod: blocking a drain and
// being non-evictable are the same question.
func TestBlocksDrainAgreesWithCanEvict(t *testing.T) {
	kinds := []model.WorkloadKind{
		model.KindDeployment, model.KindStatefulSet, model.KindJob,
		model.KindCronJob, model.KindReplicaSet, model.KindBarePod, "",
	}
	for _, k := range kinds {
		for _, dne := range []bool{false, true} {
			for _, local := range []bool{false, true} {
				p := &model.PodSpec{Workload: ref(k, "d", "w"), DoNotEvict: dne, HasLocalStorage: local}
				blocks, why := BlocksDrain(p)
				ev := CanEvict(p)
				if blocks == ev.OK {
					t.Fatalf("kind=%q dne=%v local=%v: blocks=%v but CanEvict.OK=%v", k, dne, local, blocks, ev.OK)
				}
				if blocks && why != ev.Reason {
					t.Fatalf("kind=%q: drain reason %q != evict reason %q", k, why, ev.Reason)
				}
			}
		}
	}
}

// ------------------------------------------------------------------ PDBGuard

func webPod(uid, ns string, labels map[string]string) *model.PodSpec {
	return &model.PodSpec{UID: uid, Name: uid, Namespace: ns, Labels: labels, Workload: dep(ns, "web")}
}

// TestPDBGuardNilPodFailsClosed: a pod we cannot identify cannot be checked
// against any budget, so the guard must refuse rather than wave it through.
func TestPDBGuardNilPodFailsClosed(t *testing.T) {
	g := NewPDBGuard([]model.PDB{{
		Namespace: "prod", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 5,
	}})
	if ok, why := g.CanEvict(nil); ok || why == "" {
		t.Errorf("CanEvict(nil) = %v %q, want refusal with reason", ok, why)
	}
	if ok, why := g.Reserve(nil); ok || why == "" {
		t.Errorf("Reserve(nil) = %v %q, want refusal with reason", ok, why)
	}
	// Release(nil) must be a harmless no-op, not a panic and not free budget.
	g.Release(nil)
}

// TestPDBGuardReserveIsAtomic: a pod covered by two budgets, one exhausted,
// must not consume from the healthy one. A partial reservation would silently
// block an unrelated pod later in the same plan.
func TestPDBGuardReserveIsAtomic(t *testing.T) {
	g := NewPDBGuard([]model.PDB{
		{Namespace: "prod", Name: "by-app", Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 1},
		{Namespace: "prod", Name: "by-tier", Selector: map[string]string{"tier": "front"}, DisruptionsAllowed: 0},
	})
	both := webPod("both", "prod", map[string]string{"app": "web", "tier": "front"})
	appOnly := webPod("app-only", "prod", map[string]string{"app": "web"})

	if ok, why := g.Reserve(both); ok {
		t.Fatal("reserve must fail when any covering budget is exhausted")
	} else if why == "" {
		t.Fatal("failed reserve must explain which budget refused")
	}
	if ok, why := g.Reserve(appOnly); !ok {
		t.Fatalf("the healthy budget must be untouched by the failed reserve: %s", why)
	}
}

// TestPDBGuardReleaseCannotMintBudget: Release is a rollback of a Reserve. An
// unbalanced Release (a caller bug, a double rollback) must not raise a budget
// above what the cluster reported, or the plan overspends what the API will
// grant and the extra evictions fail mid-flight.
func TestPDBGuardReleaseCannotMintBudget(t *testing.T) {
	g := NewPDBGuard([]model.PDB{{
		Namespace: "prod", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 1,
	}})
	pod := webPod("p", "prod", map[string]string{"app": "web"})

	// Rollback without a matching reservation.
	g.Release(pod)
	g.Release(pod)
	g.Release(pod)

	if ok, why := g.Reserve(pod); !ok {
		t.Fatalf("the one real disruption must still be available: %s", why)
	}
	if ok, _ := g.Reserve(pod); ok {
		t.Fatal("budget was inflated past its collected value by unbalanced Release")
	}
}

// TestPDBGuardReserveReleaseRoundTrip: a balanced reserve/release pair leaves
// the ledger exactly where it started, however many times it is repeated.
func TestPDBGuardReserveReleaseRoundTrip(t *testing.T) {
	g := NewPDBGuard([]model.PDB{{
		Namespace: "prod", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 2,
	}})
	pod := webPod("p", "prod", map[string]string{"app": "web"})
	for i := 0; i < 50; i++ {
		if ok, why := g.Reserve(pod); !ok {
			t.Fatalf("round %d: reserve failed: %s", i, why)
		}
		g.Release(pod)
	}
	// Still exactly 2 disruptions, no more and no fewer.
	if ok, _ := g.Reserve(pod); !ok {
		t.Fatal("first of two disruptions must be available")
	}
	if ok, _ := g.Reserve(pod); !ok {
		t.Fatal("second of two disruptions must be available")
	}
	if ok, _ := g.Reserve(pod); ok {
		t.Fatal("third reserve must fail")
	}
}

// TestPDBGuardDoesNotMutateCallerSlice: the guard is seeded from the live
// snapshot (plan.go passes snap.PDBs directly); reserving must not drain the
// snapshot that later plan phases still read.
func TestPDBGuardDoesNotMutateCallerSlice(t *testing.T) {
	pdbs := []model.PDB{{
		Namespace: "prod", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: 3,
	}}
	g := NewPDBGuard(pdbs)
	pod := webPod("p", "prod", map[string]string{"app": "web"})
	for i := 0; i < 3; i++ {
		if ok, _ := g.Reserve(pod); !ok {
			t.Fatalf("reserve %d failed", i)
		}
	}
	if pdbs[0].DisruptionsAllowed != 3 {
		t.Fatalf("caller's PDB was drained to %d; the guard must keep its own ledger", pdbs[0].DisruptionsAllowed)
	}
	// Conversely, editing the caller's slice afterwards must not move the guard.
	pdbs[0].DisruptionsAllowed = 99
	if ok, _ := g.Reserve(pod); ok {
		t.Fatal("guard budget must not follow the caller's slice")
	}
}

func TestPDBGuardCoverageTable(t *testing.T) {
	cases := []struct {
		name    string
		pdb     model.PDB
		pod     *model.PodSpec
		covered bool // covered && exhausted => refusal
	}{
		{
			"label match in same namespace",
			model.PDB{Namespace: "prod", Name: "z", Selector: map[string]string{"app": "web"}},
			webPod("a", "prod", map[string]string{"app": "web"}), true,
		},
		{
			"same labels, other namespace",
			model.PDB{Namespace: "prod", Name: "z", Selector: map[string]string{"app": "web"}},
			webPod("a", "dev", map[string]string{"app": "web"}), false,
		},
		{
			"empty selector selects nothing",
			model.PDB{Namespace: "prod", Name: "z"},
			webPod("a", "prod", map[string]string{"app": "web"}), false,
		},
		{
			"pod with no labels",
			model.PDB{Namespace: "prod", Name: "z", Selector: map[string]string{"app": "web"}},
			webPod("a", "prod", nil), false,
		},
		{
			"selector is a subset requirement, extra pod labels still match",
			model.PDB{Namespace: "prod", Name: "z", Selector: map[string]string{"app": "web"}},
			webPod("a", "prod", map[string]string{"app": "web", "tier": "front"}), true,
		},
		{
			"CoveredPodUIDs wins over labels: listed",
			model.PDB{Namespace: "prod", Name: "z", Selector: map[string]string{"app": "nope"}, CoveredPodUIDs: []string{"a"}},
			webPod("a", "prod", map[string]string{"app": "web"}), true,
		},
		{
			"CoveredPodUIDs wins over labels: not listed",
			model.PDB{Namespace: "prod", Name: "z", Selector: map[string]string{"app": "web"}, CoveredPodUIDs: []string{"other"}},
			webPod("a", "prod", map[string]string{"app": "web"}), false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.pdb
			p.DisruptionsAllowed = 0 // exhausted: only a covered pod is refused
			g := NewPDBGuard([]model.PDB{p})
			ok, why := g.CanEvict(c.pod)
			if c.covered && ok {
				t.Fatalf("pod must be covered and therefore refused")
			}
			if !c.covered && !ok {
				t.Fatalf("pod must not be covered, got refusal %q", why)
			}
		})
	}
}

// TestPDBGuardNegativeBudgetRefuses: DisruptionsAllowed should never be
// negative, but a corrupt snapshot must fail closed rather than wrap.
func TestPDBGuardNegativeBudgetRefuses(t *testing.T) {
	for _, n := range []int32{-1, math.MinInt32} {
		g := NewPDBGuard([]model.PDB{{
			Namespace: "prod", Name: "bad",
			Selector: map[string]string{"app": "web"}, DisruptionsAllowed: n,
		}})
		pod := webPod("p", "prod", map[string]string{"app": "web"})
		if ok, _ := g.CanEvict(pod); ok {
			t.Errorf("DisruptionsAllowed=%d must refuse", n)
		}
		if ok, _ := g.Reserve(pod); ok {
			t.Errorf("DisruptionsAllowed=%d must refuse to reserve", n)
		}
		// Normalised to zero in the guard's own ledger, so a later Release
		// cannot restore it to a negative ceiling and pin it there.
		g.Release(pod)
		if ok, _ := g.CanEvict(pod); ok {
			t.Errorf("DisruptionsAllowed=%d must still refuse after a Release", n)
		}
		g.mu.Lock()
		got := g.pdbs[0].DisruptionsAllowed
		g.mu.Unlock()
		if got != 0 {
			t.Errorf("ledger holds %d, want the normalised 0", got)
		}
	}
}

// TestPDBGuardNormalisationDoesNotTouchCaller: normalising a corrupt budget is
// the guard's private business; the caller's snapshot is not ours to rewrite.
func TestPDBGuardNormalisationDoesNotTouchCaller(t *testing.T) {
	pdbs := []model.PDB{{
		Namespace: "prod", Name: "bad",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: -7,
	}}
	NewPDBGuard(pdbs)
	if pdbs[0].DisruptionsAllowed != -7 {
		t.Fatalf("caller's PDB was rewritten to %d", pdbs[0].DisruptionsAllowed)
	}
}

// TestPDBGuardNoPDBsAllowsEverything: absence of a budget is not a refusal.
func TestPDBGuardNoPDBsAllowsEverything(t *testing.T) {
	for _, g := range []*PDBGuard{NewPDBGuard(nil), NewPDBGuard([]model.PDB{})} {
		pod := webPod("p", "prod", map[string]string{"app": "web"})
		if ok, why := g.CanEvict(pod); !ok {
			t.Fatalf("no PDBs must allow eviction, got %q", why)
		}
		if ok, why := g.Reserve(pod); !ok {
			t.Fatalf("no PDBs must allow reserve, got %q", why)
		}
	}
}

// TestPDBGuardConcurrentReserve: exactly DisruptionsAllowed reservations may
// succeed no matter how many goroutines race for them.
func TestPDBGuardConcurrentReserve(t *testing.T) {
	const budget = 10
	const racers = 200
	g := NewPDBGuard([]model.PDB{{
		Namespace: "prod", Name: "web-pdb",
		Selector: map[string]string{"app": "web"}, DisruptionsAllowed: budget,
	}})
	pod := webPod("p", "prod", map[string]string{"app": "web"})

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := g.Reserve(pod); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != budget {
		t.Fatalf("granted %d reservations, want exactly %d", granted, budget)
	}
}

// ----------------------------------------------------------------- Cooldowns

func TestCooldownsTable(t *testing.T) {
	cases := []struct {
		name      string
		interval  time.Duration
		at        time.Duration // offset from the priming call at t0
		wantAllow bool
		wantRem   time.Duration
	}{
		{"immediately after priming", time.Hour, 0, false, time.Hour},
		{"mid-cooldown", time.Hour, 30 * time.Minute, false, 30 * time.Minute},
		{"one nanosecond short", time.Hour, time.Hour - 1, false, 1},
		{"exactly at the boundary is out of cooldown", time.Hour, time.Hour, true, 0},
		{"past the boundary", time.Hour, time.Hour + 1, true, 0},
		{"zero interval never blocks", 0, 0, true, 0},
		{"negative interval never blocks", -time.Hour, 0, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cd := NewCooldowns(c.interval)
			if !cd.Allow("k", t0) {
				t.Fatal("priming call must be allowed")
			}
			now := t0.Add(c.at)
			if got := cd.Remaining("k", now); got != c.wantRem {
				t.Errorf("Remaining = %v, want %v", got, c.wantRem)
			}
			if got := cd.Allow("k", now); got != c.wantAllow {
				t.Errorf("Allow = %v, want %v", got, c.wantAllow)
			}
		})
	}
}

// TestCooldownsRemainingAgreesWithAllow is the invariant the two methods share:
// a caller that asks "how long?" and a caller that asks "may I?" must never
// disagree, including on garbage clocks.
func TestCooldownsRemainingAgreesWithAllow(t *testing.T) {
	var zeroTime time.Time
	nows := []time.Time{
		t0, t0.Add(time.Nanosecond), t0.Add(30 * time.Minute),
		t0.Add(time.Hour - 1), t0.Add(time.Hour), t0.Add(2 * time.Hour),
		t0.Add(-time.Minute), // clock stepped backwards
		zeroTime,             // uninitialised time.Time
		time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), // absurd future
	}
	for _, iv := range []time.Duration{0, time.Nanosecond, time.Hour, 24 * time.Hour} {
		for _, now := range nows {
			cd := NewCooldowns(iv)
			cd.Allow("k", t0)
			rem := cd.Remaining("k", now)
			allowed := cd.Allow("k", now)
			if (rem > 0) == allowed {
				t.Errorf("interval=%v now=%v: Remaining=%v but Allow=%v", iv, now, rem, allowed)
			}
			if rem < 0 {
				t.Errorf("interval=%v now=%v: Remaining must never be negative, got %v", iv, now, rem)
			}
		}
	}
}

// TestCooldownsDeniedAttemptsDoNotExtend: a hot loop of denied attempts must
// not push the deadline out forever (that would be a livelock).
func TestCooldownsDeniedAttemptsDoNotExtend(t *testing.T) {
	cd := NewCooldowns(10 * time.Minute)
	cd.Allow("k", t0)
	for i := 1; i <= 100; i++ {
		cd.Allow("k", t0.Add(time.Duration(i)*time.Second))
	}
	if !cd.Allow("k", t0.Add(10*time.Minute)) {
		t.Fatal("cooldown must still expire at t0+interval despite denied attempts")
	}
}

func TestCooldownsUnknownKey(t *testing.T) {
	cd := NewCooldowns(time.Hour)
	if got := cd.Remaining("never-seen", t0); got != 0 {
		t.Fatalf("unknown key Remaining = %v, want 0", got)
	}
	if !cd.Allow("never-seen", t0) {
		t.Fatal("unknown key must be allowed")
	}
}

func TestCooldownsKeysAreIndependent(t *testing.T) {
	cd := NewCooldowns(time.Hour)
	if !cd.Allow("", t0) {
		t.Fatal("empty key is a key like any other")
	}
	if cd.Allow("", t0) {
		t.Fatal("empty key must respect its own cooldown")
	}
	if !cd.Allow("other", t0) {
		t.Fatal("distinct keys must not share a cooldown")
	}
}

// TestCooldownsConcurrentAllow: exactly one racer may claim a fresh key.
func TestCooldownsConcurrentAllow(t *testing.T) {
	cd := NewCooldowns(time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cd.Allow("node/a", t0) {
				mu.Lock()
				won++
				mu.Unlock()
			}
			cd.Remaining("node/a", t0)
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d goroutines claimed the same cooldown, want exactly 1", won)
	}
}

// -------------------------------------------------------------------- Budget

func TestBudgetTable(t *testing.T) {
	cases := []struct {
		name   string
		max    int
		window time.Duration
		// offsets of Allow calls from t0, and whether each must be granted.
		calls []time.Duration
		want  []bool
	}{
		{
			"max zero denies everything",
			0, time.Hour,
			[]time.Duration{0, time.Minute}, []bool{false, false},
		},
		{
			"negative max denies everything",
			-3, time.Hour,
			[]time.Duration{0, time.Minute}, []bool{false, false},
		},
		{
			"budget of one",
			1, time.Hour,
			[]time.Duration{0, time.Minute, time.Hour, time.Hour + time.Nanosecond},
			// t0 granted; +1m denied; +1h: the t0 event sits exactly on the
			// cutoff and is dropped, so a slot frees.
			[]bool{true, false, true, false},
		},
		{
			// t0 and +30m fill the budget; +45m is refused. By +1h1s the t0
			// event has aged out and by +90m the +30m one has, so each frees
			// exactly one slot — and +90m1s finds the window full again.
			"window slides one event at a time",
			2, time.Hour,
			[]time.Duration{0, 30 * time.Minute, 45 * time.Minute, time.Hour + time.Second, 90 * time.Minute, 90*time.Minute + time.Second},
			[]bool{true, true, false, true, true, false},
		},
		{
			"zero window means no memory, so no limit",
			1, 0,
			[]time.Duration{0, 0, time.Minute}, []bool{true, true, true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := NewBudget(c.max, c.window)
			for i, off := range c.calls {
				now := t0.Add(off)
				// Used must predict Allow: they share one pruning rule.
				predicted := b.Used(now) < c.max
				got := b.Allow(now)
				if got != c.want[i] {
					t.Errorf("call %d (t0+%v): Allow = %v, want %v", i, off, got, c.want[i])
				}
				if predicted != got {
					t.Errorf("call %d (t0+%v): Used-based prediction %v disagreed with Allow %v", i, off, predicted, got)
				}
			}
		})
	}
}

// TestBudgetUsedDoesNotConsume: Used is a read; polling it must not burn slots.
func TestBudgetUsedDoesNotConsume(t *testing.T) {
	b := NewBudget(2, time.Hour)
	for i := 0; i < 100; i++ {
		b.Used(t0)
	}
	if !b.Allow(t0) || !b.Allow(t0) {
		t.Fatal("polling Used must not consume budget")
	}
	if b.Allow(t0) {
		t.Fatal("third Allow must be denied")
	}
}

// TestBudgetDoesNotGrowUnbounded: the event log is pruned to the window, so a
// long-lived actuator cannot leak memory one eviction at a time.
func TestBudgetDoesNotGrowUnbounded(t *testing.T) {
	b := NewBudget(3, time.Minute)
	for i := 0; i < 100000; i++ {
		b.Allow(t0.Add(time.Duration(i) * time.Second))
	}
	b.mu.Lock()
	n := len(b.events)
	c := cap(b.events)
	b.mu.Unlock()
	if n > 3 {
		t.Fatalf("retained %d events for a budget of 3", n)
	}
	if c > 64 {
		t.Fatalf("event log capacity grew to %d; the backing array is leaking", c)
	}
}

// TestBudgetConcurrentAllow: the window limit holds under contention.
func TestBudgetConcurrentAllow(t *testing.T) {
	const max = 25
	b := NewBudget(max, time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if b.Allow(t0.Add(time.Duration(i) * time.Millisecond)) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
			b.Used(t0)
		}(i)
	}
	wg.Wait()
	if granted != max {
		t.Fatalf("granted %d, want exactly %d", granted, max)
	}
}

// ------------------------------------------------------- RegressionDetector

func snapWith(pods ...model.PodSpec) *model.ClusterSnapshot {
	return &model.ClusterSnapshot{Pods: pods}
}

func pod(uid string, r model.WorkloadRef, restarts int32, oom bool) model.PodSpec {
	return model.PodSpec{
		UID: uid, Name: uid, Namespace: r.Namespace, Workload: r,
		Containers: []model.ContainerSpec{{Name: "app", RestartCount: restarts, LastOOMKilled: oom}},
	}
}

// TestRegressionSurvivesPodReplacement is the core of the detector's job: a
// resize replaces every pod, so the new pods start at RestartCount 0. Comparing
// the workload's new total against the old pods' lifetime totals hides the very
// crashloop the watch exists to catch.
func TestRegressionSurvivesPodReplacement(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	// Long-lived pod with a lot of restart history, healthy right now.
	d.RecordChange(r, snapWith(pod("old-1", r, 40, false)), t0)
	// Our change rolled the pod; the replacement is crashlooping.
	regs := d.Check(snapWith(pod("new-1", r, 5, false)), t0.Add(5*time.Minute))
	if len(regs) != 1 {
		t.Fatalf("want 1 regression for a crashlooping replacement pod, got %d: %+v", len(regs), regs)
	}
}

// TestRegressionOOMOnReplacementPod: same trap for the OOM signal. A workload
// that had OOMed before the change must still be able to report a fresh OOM.
func TestRegressionOOMOnReplacementPod(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("old-1", r, 0, true)), t0)
	regs := d.Check(snapWith(pod("new-1", r, 0, true)), t0.Add(5*time.Minute))
	if len(regs) != 1 {
		t.Fatalf("want 1 regression: the replacement pod OOMed after our change, got %d", len(regs))
	}
	if len(regs) == 1 && regs[0].Reason == "" {
		t.Error("regression must carry a reason")
	}
}

// TestRegressionNotMaskedByRetiredPod: one pod retiring with a big restart
// history must not offset another pod's genuine new crashloop.
func TestRegressionNotMaskedByRetiredPod(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("a", r, 50, false), pod("b", r, 0, false)), t0)
	// "a" was replaced by "c"; "b" survived and is now crashlooping.
	regs := d.Check(snapWith(pod("c", r, 0, false), pod("b", r, 4, false)), t0.Add(time.Minute))
	if len(regs) != 1 {
		t.Fatalf("surviving pod's +4 restarts must be a regression, got %d", len(regs))
	}
}

// TestRegressionSurvivingPodCounterReset: a kubelet restart can reset a
// counter. A decrease must be read as "no new restarts", never as credit that
// offsets other pods.
func TestRegressionSurvivingPodCounterReset(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("a", r, 100, false), pod("b", r, 0, false)), t0)
	regs := d.Check(snapWith(pod("a", r, 0, false), pod("b", r, 3, false)), t0.Add(time.Minute))
	if len(regs) != 1 {
		t.Fatalf("a counter reset on 'a' must not cancel 'b' crashlooping, got %d", len(regs))
	}
}

// TestRegressionThresholdTable pins the +2-restart tolerance and the
// zero-tolerance OOM rule against a pod that survives the change.
func TestRegressionThresholdTable(t *testing.T) {
	r := dep("prod", "api")
	cases := []struct {
		name           string
		baseR, afterR  int32
		baseO, afterO  bool
		wantRegression bool
	}{
		{"no change", 5, 5, false, false, false},
		{"+1 restart tolerated", 5, 6, false, false, false},
		{"+2 restarts tolerated (a rollout)", 5, 7, false, false, false},
		{"+3 restarts is a crashloop", 5, 8, false, false, true},
		{"restarts fell", 5, 1, false, false, false},
		{"first OOM after the change", 0, 0, false, true, true},
		{"pre-existing OOM is not ours", 0, 0, true, true, false},
		{"OOM cleared", 0, 0, true, false, false},
		{"OOM beats the restart tolerance", 5, 6, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewRegressionDetector(30*time.Minute, time.Hour)
			d.RecordChange(r, snapWith(pod("a", r, c.baseR, c.baseO)), t0)
			regs := d.Check(snapWith(pod("a", r, c.afterR, c.afterO)), t0.Add(time.Minute))
			if got := len(regs) == 1; got != c.wantRegression {
				t.Fatalf("regression = %v, want %v (%+v)", got, c.wantRegression, regs)
			}
			if c.wantRegression && !d.Quarantined(r, t0.Add(time.Minute)) {
				t.Error("a regressed workload must be quarantined")
			}
		})
	}
}

// TestRegressionNilSnapshot: collectors fail. A nil snapshot must not panic,
// and must not be read as "everything regressed" — reverting on no evidence
// churns production.
func TestRegressionNilSnapshot(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("a", r, 3, false)), t0)
	if regs := d.Check(nil, t0.Add(time.Minute)); len(regs) != 0 {
		t.Fatalf("nil snapshot must yield no regressions, got %+v", regs)
	}
	// The watch survives a failed collection and still works afterwards.
	if regs := d.Check(snapWith(pod("a", r, 9, false)), t0.Add(2*time.Minute)); len(regs) != 1 {
		t.Fatalf("watch must survive a failed collection, got %d regressions", len(regs))
	}
}

// TestRegressionRecordChangeNilSnapshot: without a baseline there is nothing to
// compare against, so no watch may be armed — an empty baseline would read
// every pre-existing restart as new and revert a healthy change.
func TestRegressionRecordChangeNilSnapshot(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, nil, t0)
	if regs := d.Check(snapWith(pod("a", r, 40, true)), t0.Add(time.Minute)); len(regs) != 0 {
		t.Fatalf("no baseline must mean no verdict, got %+v", regs)
	}
}

// TestRegressionEmptySnapshotIsNotEvidence: a workload with no pods reports no
// counters; that is missing data, not health, and must not clear the watch.
func TestRegressionWorkloadDisappeared(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("a", r, 3, false)), t0)
	if regs := d.Check(snapWith(), t0.Add(time.Minute)); len(regs) != 0 {
		t.Fatalf("a vanished workload is not a regression, got %+v", regs)
	}
}

// TestRegressionOtherWorkloadsIgnored: counters from a different workload must
// never be attributed to the one we changed.
func TestRegressionOtherWorkloadsIgnored(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	mine := dep("prod", "api")
	theirs := dep("prod", "web")
	d.RecordChange(mine, snapWith(pod("a", mine, 0, false)), t0)
	regs := d.Check(snapWith(pod("a", mine, 0, false), pod("z", theirs, 99, true)), t0.Add(time.Minute))
	if len(regs) != 0 {
		t.Fatalf("another workload's pain is not our regression, got %+v", regs)
	}
}

// TestRegressionRecordChangeRebaselines: applying a second change to the same
// workload restarts the watch from the current numbers.
func TestRegressionRecordChangeRebaselines(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("a", r, 0, false)), t0)
	// Second change observes the elevated count as the new normal.
	d.RecordChange(r, snapWith(pod("a", r, 9, false)), t0.Add(time.Minute))
	if regs := d.Check(snapWith(pod("a", r, 9, false)), t0.Add(2*time.Minute)); len(regs) != 0 {
		t.Fatalf("re-baselined watch must not fire on the old delta, got %+v", regs)
	}
	if regs := d.Check(snapWith(pod("a", r, 13, false)), t0.Add(3*time.Minute)); len(regs) != 1 {
		t.Fatalf("re-baselined watch must fire on a new delta, got %d", len(regs))
	}
}

func TestRegressionWindowBoundary(t *testing.T) {
	r := dep("prod", "api")
	bad := snapWith(pod("a", r, 99, true))
	cases := []struct {
		name string
		at   time.Duration
		want int
	}{
		{"inside the window", 29 * time.Minute, 1},
		{"exactly at the window edge", 30 * time.Minute, 1},
		{"one nanosecond past", 30*time.Minute + 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewRegressionDetector(30*time.Minute, time.Hour)
			d.RecordChange(r, snapWith(pod("a", r, 0, false)), t0)
			if got := len(d.Check(bad, t0.Add(c.at))); got != c.want {
				t.Fatalf("got %d regressions, want %d", got, c.want)
			}
		})
	}
}

func TestRegressionQuarantineBoundary(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	d.RecordChange(r, snapWith(pod("a", r, 0, false)), t0)
	if len(d.Check(snapWith(pod("a", r, 0, true)), t0)) != 1 {
		t.Fatal("setup: expected a regression at t0")
	}
	cases := []struct {
		name string
		at   time.Duration
		want bool
	}{
		{"immediately", 0, true},
		{"just before expiry", time.Hour - 1, true},
		{"exactly at expiry", time.Hour, true},
		{"just after expiry", time.Hour + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := d.Quarantined(r, t0.Add(c.at)); got != c.want {
				t.Fatalf("Quarantined = %v, want %v", got, c.want)
			}
		})
	}
	// Never-regressed workloads are never quarantined.
	if d.Quarantined(dep("prod", "other"), t0) {
		t.Fatal("untouched workload must not be quarantined")
	}
}

// TestRegressionOrderIsDeterministic: Check ranges over a map. Callers log and
// act on this slice, so its order must not depend on Go's map seed.
func TestRegressionOrderIsDeterministic(t *testing.T) {
	names := []string{"delta", "alpha", "charlie", "bravo", "echo"}
	var want []string
	for run := 0; run < 20; run++ {
		d := NewRegressionDetector(30*time.Minute, time.Hour)
		for _, n := range names {
			r := dep("prod", n)
			d.RecordChange(r, snapWith(pod("a-"+n, r, 0, false)), t0)
		}
		var pods []model.PodSpec
		for _, n := range names {
			pods = append(pods, pod("a-"+n, dep("prod", n), 0, true))
		}
		regs := d.Check(snapWith(pods...), t0.Add(time.Minute))
		if len(regs) != len(names) {
			t.Fatalf("run %d: got %d regressions, want %d", run, len(regs), len(names))
		}
		var got []string
		for _, g := range regs {
			got = append(got, g.Ref.String())
		}
		if want == nil {
			want = got
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("run %d: order %v differs from %v", run, got, want)
			}
		}
	}
}

// TestRegressionConcurrentUse exercises the detector the way the controller
// does — a reconcile loop plus readers — under -race.
func TestRegressionConcurrentUse(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	refs := make([]model.WorkloadRef, 8)
	for i := range refs {
		refs[i] = dep("prod", fmt.Sprintf("w%d", i))
	}
	snap := &model.ClusterSnapshot{}
	for i, r := range refs {
		snap.Pods = append(snap.Pods, pod(fmt.Sprintf("p%d", i), r, int32(i), i%2 == 0))
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := refs[i%len(refs)]
			now := t0.Add(time.Duration(i) * time.Second)
			switch i % 3 {
			case 0:
				d.RecordChange(r, snap, now)
			case 1:
				d.Check(snap, now)
			default:
				d.Quarantined(r, now)
			}
		}(i)
	}
	wg.Wait()
}

// TestCooldownsPrune: pruning bounds the tracker's memory in a long-lived
// controller without changing a single answer it gives.
func TestCooldownsPrune(t *testing.T) {
	cd := NewCooldowns(time.Hour)
	cd.Allow("lapsed-a", t0)
	cd.Allow("lapsed-b", t0)
	cd.Allow("active", t0.Add(90*time.Minute))

	now := t0.Add(2 * time.Hour)
	// "active" was primed at +90m, so at +2h only 30m of its hour has run.
	if got := cd.Prune(now); got != 2 {
		t.Fatalf("pruned %d keys, want 2", got)
	}
	cd.mu.Lock()
	resident := len(cd.last)
	cd.mu.Unlock()
	if resident != 1 {
		t.Fatalf("%d keys still resident, want 1", resident)
	}
	// The surviving key kept its deadline; the pruned ones behave as before.
	if got := cd.Remaining("active", now); got != 30*time.Minute {
		t.Fatalf("active Remaining = %v, want 30m", got)
	}
	if cd.Allow("active", now) {
		t.Fatal("pruning must not release a live cooldown")
	}
	if got := cd.Remaining("lapsed-a", now); got != 0 {
		t.Fatalf("pruned key Remaining = %v, want 0", got)
	}
	if !cd.Allow("lapsed-a", now) {
		t.Fatal("a pruned key must behave exactly like a lapsed one")
	}
	// Pruning an empty or already-pruned tracker is a no-op.
	if got := NewCooldowns(time.Hour).Prune(now); got != 0 {
		t.Fatalf("pruning an empty tracker removed %d keys", got)
	}
}

// TestCooldownsPruneAgreesWithRemaining: Prune's predicate and Remaining's must
// stay the same predicate — a key is prunable exactly when nothing is left.
func TestCooldownsPruneAgreesWithRemaining(t *testing.T) {
	for _, iv := range []time.Duration{0, -time.Hour, time.Nanosecond, time.Hour} {
		for _, off := range []time.Duration{-time.Hour, 0, time.Hour - 1, time.Hour, 2 * time.Hour} {
			cd := NewCooldowns(iv)
			cd.Allow("k", t0)
			now := t0.Add(off)
			left := cd.Remaining("k", now)
			pruned := cd.Prune(now) == 1
			if pruned != (left == 0) {
				t.Errorf("interval=%v off=%v: pruned=%v but Remaining=%v", iv, off, pruned, left)
			}
		}
	}
}

// TestRegressionKeylessPodsAccumulate covers the podHealthKey fallback: pods
// with neither a UID nor a name collapse into one bucket, and their counters
// must add up there rather than the last pod overwriting the rest. Losing
// counters this way would understate a crashloop.
func TestRegressionKeylessPodsAccumulate(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := dep("prod", "api")
	keyless := func(restarts int32) model.PodSpec {
		return model.PodSpec{Workload: r, Containers: []model.ContainerSpec{
			{Name: "app", RestartCount: restarts},
		}}
	}
	// Three anonymous pods, 1 restart each: three restarts in total.
	d.RecordChange(r, snapWith(keyless(0), keyless(0), keyless(0)), t0)
	if regs := d.Check(snapWith(keyless(1), keyless(1), keyless(1)), t0.Add(time.Minute)); len(regs) != 1 {
		t.Fatalf("3 accumulated restarts must exceed the tolerance of %d, got %d regressions",
			restartTolerance, len(regs))
	}
}

// TestRegressionCheckKeepsLiveQuarantines: Check sweeps lapsed quarantines to
// bound the map. It must sweep only the lapsed ones — releasing a workload that
// is still serving out its quarantine would let Kilter touch it again.
func TestRegressionCheckKeepsLiveQuarantines(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, 24*time.Hour)
	victim := dep("prod", "victim")
	other := dep("prod", "other")

	d.RecordChange(victim, snapWith(pod("v", victim, 0, false)), t0)
	if len(d.Check(snapWith(pod("v", victim, 0, true)), t0)) != 1 {
		t.Fatal("setup: victim must regress")
	}

	// An unrelated reconcile, well inside the victim's 24h quarantine.
	d.RecordChange(other, snapWith(pod("o", other, 0, false)), t0.Add(time.Hour))
	d.Check(snapWith(pod("o", other, 0, false)), t0.Add(time.Hour))

	if !d.Quarantined(victim, t0.Add(time.Hour)) {
		t.Fatal("an unrelated Check released a live quarantine")
	}
	// And the sweep still does its job once the quarantine really has lapsed.
	d.Check(snapWith(), t0.Add(25*time.Hour))
	d.mu.Lock()
	held := len(d.quarantine)
	d.mu.Unlock()
	if held != 0 {
		t.Fatalf("%d lapsed quarantines still resident", held)
	}
}

// TestRegressionStatefulSetPodNameReuse: a StatefulSet recreates a pod under
// the *same name* with a new UID. Keying baselines by name would treat the
// replacement as the original, compare its fresh counter against the
// predecessor's lifetime total, and hide the crashloop — the same trap as
// comparing workload-wide sums, one level down. UID is the identity that moves.
func TestRegressionStatefulSetPodNameReuse(t *testing.T) {
	d := NewRegressionDetector(30*time.Minute, time.Hour)
	r := ref(model.KindStatefulSet, "prod", "web")
	at := func(uid string, restarts int32) model.PodSpec {
		return model.PodSpec{
			UID: uid, Name: "web-0", Namespace: "prod", Workload: r,
			Containers: []model.ContainerSpec{{Name: "app", RestartCount: restarts}},
		}
	}
	// web-0 has restarted plenty over its life and is healthy right now.
	d.RecordChange(r, snapWith(at("uid-old", 40)), t0)
	// Our resize recreated web-0: same name, new UID, and it is crashlooping.
	if regs := d.Check(snapWith(at("uid-new", 5)), t0.Add(5*time.Minute)); len(regs) != 1 {
		t.Fatalf("recreated pod's 5 restarts must be a regression, got %d", len(regs))
	}
}
