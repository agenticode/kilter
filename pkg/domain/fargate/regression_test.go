package fargate

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/plan"
)

// TestPostResizeOOMDrivesTheRevertPath is the failure this whole package is
// built to survive: the shave looked safe on every metric, the pod OOMed
// anyway, and the engine must notice and put the memory back — without being
// told, and without ever booking the revert as a saving.
func TestPostResizeOOMDrivesTheRevertPath(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	w := cliff()
	learn(t, d, cluster(now, 300, 72*time.Hour, w))

	rec := only(t, d.Recommend(now, nil), w.ref())
	if rec.Proposed.Attr(AttrChange) != ChangeBoundaryShave {
		t.Fatalf("setup: wanted a shave, got %q", rec.Proposed.Attr(AttrChange))
	}
	steps, err := d.PlanSteps([]domain.Recommendation{rec}, domain.Guard{Now: now})
	if err != nil || len(steps) != 1 {
		t.Fatalf("PlanSteps = (%v, %v)", steps, err)
	}
	if err := d.RecordApplied(steps[0], now); err != nil {
		t.Fatal(err)
	}

	// The controller applied it: the pods now request the shaved memory. And
	// they OOM.
	after := now.Add(10 * time.Minute)
	post := w
	post.containers[0].memReq = 8*gib - 256*mib
	post.containers[0].oom = true
	learn(t, d, cluster(after, 60, 30*time.Minute, post))

	recs := d.Recommend(after, nil)
	rev := only(t, recs, w.ref())

	if got := rev.Proposed.Attr(AttrChange); got != ChangeRevert {
		t.Fatalf("change = %q, want %q", got, ChangeRevert)
	}
	if got, want := rev.Current.Attr(AttrTier), "1vCPU 8GB"; got != want {
		t.Fatalf("current tier = %q, want the shaved %q", got, want)
	}
	if got, want := rev.Proposed.Attr(AttrTier), "2vCPU 9GB"; got != want {
		t.Fatalf("revert tier = %q, want the pre-change %q", got, want)
	}
	// A revert costs money and says so. Nothing downstream may book it as a win.
	if rev.GrossSavingsMonthlyUSD >= 0 {
		t.Errorf("revert claims $%v/mo of savings", rev.GrossSavingsMonthlyUSD)
	}
	if rev.NetSavingsMonthlyUSD > rev.GrossSavingsMonthlyUSD {
		t.Errorf("net $%v exceeds gross $%v", rev.NetSavingsMonthlyUSD, rev.GrossSavingsMonthlyUSD)
	}
	if rev.ClaimableMonthlyUSD() != 0 {
		t.Errorf("revert is claimable as $%v", rev.ClaimableMonthlyUSD())
	}
	if rev.Action != domain.ActionRolling {
		t.Errorf("action = %q, want rolling", rev.Action)
	}
	if rev.Risk != plan.RiskLow {
		t.Errorf("risk = %q; restoring a known-good size is low risk", rev.Risk)
	}
	if rev.Confidence != 1 {
		t.Errorf("confidence = %v; an observed OOM is a fact, not an inference", rev.Confidence)
	}
	if len(rev.Evidence) == 0 || rev.Evidence[0].Metric != "post-change-regression" {
		t.Fatalf("evidence does not lead with the regression: %+v", rev.Evidence)
	}
	if !contains(rev.Evidence[0].Value, "OOM") {
		t.Errorf("regression evidence %q does not name the OOM", rev.Evidence[0].Value)
	}

	// The workload is quarantined, so no fresh shave can be proposed for it...
	if !d.Quarantined(w.ref(), after) {
		t.Fatal("a regressed workload was not quarantined")
	}
	// ...and the revert survives being asked for twice. safety reports each
	// regression exactly once; losing it on the second call would silently
	// strand a broken pod at the shaved size.
	again := d.Recommend(after, nil)
	rev2 := only(t, again, w.ref())
	if rev2.Proposed.Attr(AttrChange) != ChangeRevert || rev2.Proposed.Attr(AttrTier) != "2vCPU 9GB" {
		t.Fatalf("the revert was lost on the second Recommend: %+v", rev2.Proposed.Attrs)
	}

	// Executing the revert consumes it.
	revSteps, err := d.PlanSteps([]domain.Recommendation{rev2}, domain.Guard{Now: after})
	if err != nil || len(revSteps) != 1 {
		t.Fatalf("PlanSteps(revert) = (%v, %v)", revSteps, err)
	}
	if err := d.RecordApplied(revSteps[0], after); err != nil {
		t.Fatal(err)
	}
	restored := w // requests are back where they started
	learn(t, d, cluster(after.Add(time.Minute), 60, 30*time.Minute, restored))
	none(t, d.Recommend(after.Add(time.Minute), nil), w.ref())
}

// TestQuarantineBlocksNewShavesButNotTheRevert: after a regression the
// workload is left alone even when the numbers look attractive again.
func TestQuarantineBlocksNewShaves(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	w := cliff()
	learn(t, d, cluster(now, 300, 72*time.Hour, w))
	rec := only(t, d.Recommend(now, nil), w.ref())
	steps, err := d.PlanSteps([]domain.Recommendation{rec}, domain.Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordApplied(steps[0], now); err != nil {
		t.Fatal(err)
	}

	after := now.Add(10 * time.Minute)
	post := w
	post.containers[0].oom = true // regressed, but the requests were rolled back already
	learn(t, d, cluster(after, 60, 30*time.Minute, post))
	if got := d.Recommend(after, nil); len(got) != 0 {
		// The pod is back at its original spec, so the revert is a no-op and is
		// dropped; the quarantine must then keep the shave from re-firing.
		t.Fatalf("quarantined workload still got a recommendation:%s", describe(got))
	}
	if !d.Quarantined(w.ref(), after) {
		t.Fatal("workload not quarantined")
	}
	// The quarantine lapses on schedule.
	if d.Quarantined(w.ref(), after.Add(25*time.Hour)) {
		t.Fatal("quarantine never lapses")
	}
}

// TestRegressionWithoutARecordedChangeIsNotOurs: Kilter only reverts changes it
// made. A workload that starts OOMing on its own is left to its owners.
func TestRegressionWithoutARecordedChangeIsNotOurs(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	w := cliff()
	w.containers[0].oom = true
	learn(t, d, cluster(now, 300, 72*time.Hour, w))
	for _, r := range d.Recommend(now, nil) {
		if r.Proposed.Attr(AttrChange) == ChangeRevert {
			t.Fatalf("reverted a change Kilter never made: %s", r.Reason)
		}
	}
}

func TestRecordAppliedRejectsGarbage(t *testing.T) {
	d := newDomain(t)
	step := domain.Step{Target: domain.TargetRef{Domain: Kind, Scope: "c1", ID: "nonsense"}}
	if err := d.RecordApplied(step, now); err == nil {
		t.Error("accepted a malformed target ID")
	}
	step.Target.ID = "Deployment/default/api"
	if err := d.RecordApplied(step, now); err == nil {
		t.Error("accepted a change before any snapshot was learned")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestPendingRevertSurvivesARestart: a controller crash between detecting a
// regression and executing the revert must not strand the pod at the size that
// broke it. The pending revert is part of the checkpoint.
func TestPendingRevertSurvivesARestart(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	w := cliff()
	learn(t, d, cluster(now, 300, 72*time.Hour, w))
	rec := only(t, d.Recommend(now, nil), w.ref())
	steps, err := d.PlanSteps([]domain.Recommendation{rec}, domain.Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordApplied(steps[0], now); err != nil {
		t.Fatal(err)
	}

	// The change is recorded but no regression has fired yet: the checkpoint
	// must carry the armed watch's From spec.
	armed, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(armed), `"applied"`) {
		t.Fatalf("checkpoint does not persist the applied change: %s", armed)
	}

	after := now.Add(10 * time.Minute)
	post := w
	post.containers[0].memReq = 8*gib - 256*mib
	post.containers[0].oom = true
	postSnap := cluster(after, 60, 30*time.Minute, post)
	learn(t, d, postSnap)
	if got := only(t, d.Recommend(after, nil), w.ref()); got.Proposed.Attr(AttrChange) != ChangeRevert {
		t.Fatalf("setup: wanted a revert, got %q", got.Proposed.Attr(AttrChange))
	}

	// Now the controller restarts with the revert still pending.
	pending, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(pending), `"reverts"`) {
		t.Fatalf("checkpoint does not persist the pending revert: %s", pending)
	}
	restarted := newDomain(t, noPolicyMoves)
	if err := restarted.Restore(pending); err != nil {
		t.Fatal(err)
	}
	learn(t, restarted, postSnap)
	rev := only(t, restarted.Recommend(after, nil), w.ref())
	if rev.Proposed.Attr(AttrChange) != ChangeRevert || rev.Proposed.Attr(AttrTier) != "2vCPU 9GB" {
		t.Fatalf("the revert did not survive the restart: %+v", rev.Proposed.Attrs)
	}
}
