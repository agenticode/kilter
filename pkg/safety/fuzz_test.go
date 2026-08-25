package safety

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// FuzzBudgetInvariants drives Budget against a straightforward reference
// implementation of "at most max grants in any trailing window". The reference
// keeps every grant forever and filters on read, so it cannot share a bug with
// the prune-in-place implementation under test.
func FuzzBudgetInvariants(f *testing.F) {
	f.Add(3, int64(3600), []byte{0, 1, 2, 60, 120})
	f.Add(0, int64(60), []byte{0, 0, 0})
	f.Add(-1, int64(0), []byte{1, 1})
	f.Add(1, int64(1), []byte{0, 1, 0, 1})
	f.Add(255, int64(86400), []byte{255, 255, 255, 255})

	f.Fuzz(func(t *testing.T, maxN int, windowSec int64, steps []byte) {
		// Keep the inputs in a range a real actuator could see, but include the
		// degenerate ends (max<=0, window<=0).
		maxN = maxN%12 - 2
		if windowSec < 0 {
			windowSec = -windowSec
		}
		window := time.Duration(windowSec%7200) * time.Second
		if len(steps) > 512 {
			steps = steps[:512]
		}

		b := NewBudget(maxN, window)
		var granted []time.Time // reference log: every grant, never pruned
		now := t0

		inWindow := func(at time.Time) int {
			cutoff := at.Add(-window)
			n := 0
			for _, g := range granted {
				if g.After(cutoff) {
					n++
				}
			}
			return n
		}

		for _, step := range steps {
			now = now.Add(time.Duration(step) * time.Second) // non-decreasing
			want := inWindow(now)

			if got := b.Used(now); got != want {
				t.Fatalf("Used(%v) = %d, want %d", now.Sub(t0), got, want)
			}
			wantAllow := want < maxN
			if got := b.Allow(now); got != wantAllow {
				t.Fatalf("Allow(%v) = %v, want %v (used=%d max=%d window=%v)",
					now.Sub(t0), got, wantAllow, want, maxN, window)
			}
			if wantAllow {
				granted = append(granted, now)
			}
			// The hard safety property: never more than max grants in a window.
			if n := inWindow(now); maxN > 0 && n > maxN {
				t.Fatalf("%d grants inside a %v window, max is %d", n, window, maxN)
			}
			// The event log is a sliding window, not a journal.
			b.mu.Lock()
			held := len(b.events)
			b.mu.Unlock()
			if maxN > 0 && held > maxN {
				t.Fatalf("retained %d events for a budget of %d", held, maxN)
			}
		}
	})
}

// FuzzCooldownsInvariants checks the contract Allow and Remaining share on
// arbitrary clocks, including ones that step backwards.
func FuzzCooldownsInvariants(f *testing.F) {
	f.Add(int64(3600), []byte{0, 30, 60, 120})
	f.Add(int64(0), []byte{0, 0})
	f.Add(int64(-5), []byte{10, 246}) // 246 == -10 minutes: the clock steps back
	f.Add(int64(1), []byte{128, 127, 128, 127})

	f.Fuzz(func(t *testing.T, intervalSec int64, steps []byte) {
		if len(steps) > 256 {
			steps = steps[:256]
		}
		interval := time.Duration(intervalSec%86400) * time.Second
		c := NewCooldowns(interval)

		// An unseen key is never in cooldown.
		if got := c.Remaining("fresh", t0); got != 0 {
			t.Fatalf("unseen key Remaining = %v, want 0", got)
		}

		// Each byte is a signed minute step, so the clock wanders forwards and
		// backwards; the saturating extremes (a zero-value time.Time) are
		// covered by TestCooldownsRemainingAgreesWithAllow.
		now := t0
		for _, step := range steps {
			now = now.Add(time.Duration(int8(step)) * time.Minute)

			rem := c.Remaining("k", now)
			allowed := c.Allow("k", now)

			if rem < 0 {
				t.Fatalf("Remaining = %v, must never be negative", rem)
			}
			// The invariant: the two answers are the same answer.
			if (rem > 0) == allowed {
				t.Fatalf("interval=%v now=%v: Remaining=%v but Allow=%v", interval, now.Sub(t0), rem, allowed)
			}
			if allowed && interval > 0 {
				// A fresh cooldown starts at exactly the full interval.
				if got := c.Remaining("k", now); got != interval {
					t.Fatalf("after a granted Allow, Remaining = %v, want %v", got, interval)
				}
			}
			// Other keys are never disturbed.
			if got := c.Remaining("other", now); got != 0 {
				t.Fatalf("untouched key has Remaining = %v", got)
			}
		}
	})
}

// FuzzPDBGuardLedger throws arbitrary reserve/release sequences at the guard
// and checks the ledger never leaves [0, collected] — the range that makes it
// a faithful model of what the API will actually grant.
func FuzzPDBGuardLedger(f *testing.F) {
	f.Add(int32(2), []byte{0, 0, 0, 1, 1, 1})
	f.Add(int32(0), []byte{1, 1, 0})
	f.Add(int32(-4), []byte{0, 1})
	f.Add(int32(127), []byte{2, 0, 2, 1, 2})

	f.Fuzz(func(t *testing.T, allowed int32, ops []byte) {
		if len(ops) > 512 {
			ops = ops[:512]
		}
		pdbs := []model.PDB{
			{Namespace: "prod", Name: "by-app", Selector: map[string]string{"app": "web"}, DisruptionsAllowed: allowed},
			{Namespace: "prod", Name: "by-tier", Selector: map[string]string{"tier": "front"}, DisruptionsAllowed: allowed / 2},
		}
		// The guard normalises a corrupt negative allowance to zero; the
		// reference ceiling has to do the same.
		initial := make([]int32, len(pdbs))
		for i := range pdbs {
			if v := pdbs[i].DisruptionsAllowed; v > 0 {
				initial[i] = v
			}
		}
		g := NewPDBGuard(pdbs)

		pods := []*model.PodSpec{
			webPod("both", "prod", map[string]string{"app": "web", "tier": "front"}),
			webPod("app", "prod", map[string]string{"app": "web"}),
			webPod("tier", "prod", map[string]string{"tier": "front"}),
			webPod("none", "prod", map[string]string{"app": "other"}),
			webPod("dev", "dev", map[string]string{"app": "web"}),
			nil,
		}

		ledger := func() []int32 {
			g.mu.Lock()
			defer g.mu.Unlock()
			return []int32{g.pdbs[0].DisruptionsAllowed, g.pdbs[1].DisruptionsAllowed}
		}
		check := func(where string) {
			for i, v := range ledger() {
				if v < 0 {
					t.Fatalf("%s: PDB %d went negative (%d)", where, i, v)
				}
				if v > initial[i] {
					t.Fatalf("%s: PDB %d rose to %d above its collected %d", where, i, v, initial[i])
				}
			}
		}
		// A budget collected as negative is garbage: it must stay put, never be
		// "restored" upward by a Release.
		check("initial")

		for _, op := range ops {
			p := pods[int(op/4)%len(pods)]
			switch op % 4 {
			case 0:
				before := ledger()
				ok, why := g.Reserve(p)
				if !ok && why == "" {
					t.Fatal("refused reserve must carry a reason")
				}
				if !ok {
					// A refused reservation is all-or-nothing.
					for i, v := range ledger() {
						if v != before[i] {
							t.Fatalf("failed Reserve moved PDB %d from %d to %d", i, before[i], v)
						}
					}
				}
			case 1:
				g.Release(p)
			case 2:
				ok, why := g.CanEvict(p)
				if !ok && why == "" {
					t.Fatal("refused CanEvict must carry a reason")
				}
				// CanEvict is a dry run of Reserve: with no concurrent caller,
				// its answer must be the one Reserve gives.
				got, _ := g.Reserve(p)
				if got != ok {
					t.Fatalf("CanEvict said %v but Reserve said %v", ok, got)
				}
				if got {
					g.Release(p)
				}
			default:
				// Balanced round trip must be a no-op on the ledger.
				before := ledger()
				if ok, _ := g.Reserve(p); ok {
					g.Release(p)
				}
				for i, v := range ledger() {
					if v != before[i] {
						t.Fatalf("reserve/release round trip moved PDB %d from %d to %d", i, before[i], v)
					}
				}
			}
			check("after op")
		}
	})
}

// FuzzRegressionDetector checks the detector's two non-negotiable properties on
// arbitrary pod histories: a workload whose pods report nothing new after the
// change is never reverted, and a verdict is delivered at most once.
func FuzzRegressionDetector(f *testing.F) {
	f.Add([]byte{0, 0, 1, 1}, []byte{0, 0, 1, 1}, int64(60))
	f.Add([]byte{40, 0}, []byte{0, 0}, int64(60))
	f.Add([]byte{}, []byte{3, 1}, int64(600))
	f.Add([]byte{255, 255}, []byte{255, 255}, int64(1))

	f.Fuzz(func(t *testing.T, base, after []byte, elapsedSec int64) {
		if len(base) > 128 {
			base = base[:128]
		}
		if len(after) > 128 {
			after = after[:128]
		}
		if elapsedSec < 0 {
			elapsedSec = -elapsedSec
		}
		r := dep("prod", "api")

		// Each byte pair is one pod: (restart count, OOM flag).
		build := func(b []byte, uidPrefix string) *model.ClusterSnapshot {
			snap := &model.ClusterSnapshot{}
			for i := 0; i+1 < len(b); i += 2 {
				snap.Pods = append(snap.Pods, pod(
					uidPrefix+string(rune('a'+i%26))+string(rune('a'+i/26%26)),
					r, int32(b[i]), b[i+1]%2 == 0))
			}
			return snap
		}

		d := NewRegressionDetector(30*time.Minute, time.Hour)
		baseSnap := build(base, "p")
		d.RecordChange(r, baseSnap, t0)

		// Re-checking the exact baseline can never be a regression: nothing
		// changed, so nothing may be reverted.
		if regs := d.Check(baseSnap, t0.Add(time.Second)); len(regs) != 0 {
			t.Fatalf("unchanged snapshot reported %d regressions: %+v", len(regs), regs)
		}

		now := t0.Add(time.Duration(elapsedSec%7200) * time.Second)
		afterSnap := build(after, "q")
		first := d.Check(afterSnap, now)
		if len(first) > 1 {
			t.Fatalf("one workload cannot regress %d times at once", len(first))
		}
		for _, g := range first {
			if g.Ref != r {
				t.Fatalf("verdict for the wrong workload: %v", g.Ref)
			}
			if g.Reason == "" {
				t.Fatal("verdict must carry a reason")
			}
			if !g.DetectedAt.Equal(now) {
				t.Fatalf("DetectedAt = %v, want %v", g.DetectedAt, now)
			}
			if !d.Quarantined(r, now) {
				t.Fatal("a regressed workload must be quarantined")
			}
		}
		// A verdict is delivered once; the watch is consumed with it.
		if again := d.Check(afterSnap, now); len(again) != 0 {
			t.Fatalf("regression repeated: %+v", again)
		}
	})
}
