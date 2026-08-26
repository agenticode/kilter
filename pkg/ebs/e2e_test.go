package ebs

import (
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// End-to-end: a recorded account, through the production collector, the
// registry, the guardrails, the plan and a mocked EC2 endpoint that executes
// the modification asynchronously — then round again, so the loop's own output
// is the next round's input.

const e2eScope = "123456789012/us-east-1"

func loadAccount(t *testing.T, clock *fakeClock) *Fixture {
	t.Helper()
	f, err := LoadFixtureFile("testdata/account-mixed.json")
	if err != nil {
		t.Fatalf("LoadFixtureFile: %v", err)
	}
	f.Now = clock.Now
	return f
}

func e2eConfig() Config {
	c := testConfig()
	c.Scope, c.Region = e2eScope, "us-east-1"
	return c
}

// TestEndToEndConvertsAndRecords walks the whole loop on a recorded account.
func TestEndToEndConvertsAndRecords(t *testing.T) {
	now := base.Add(3 * time.Hour)
	clock := newClock(now)
	f := loadAccount(t, clock)

	// --- observe ---------------------------------------------------------
	c, err := NewCollector(f, f, CollectorConfig{Scope: e2eScope, Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	snap, err := c.Collect(t.Context(), now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Targets) != 4 {
		t.Fatalf("collected %d volumes, want 4", len(snap.Targets))
	}

	// --- learn, through the registry -------------------------------------
	d := newDomain(t, e2eConfig())
	reg := domain.NewRegistry()
	if err := reg.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Learn(snap); err != nil {
		t.Fatalf("registry Learn: %v", err)
	}

	// --- decide ----------------------------------------------------------
	// An account-wide commitment inventory, so claims travel the Bill() path.
	inv := &commit.Inventory{
		SavingsPlans: []commit.SavingsPlan{{
			ID: "sp-e2e", Type: commit.SPCompute, Region: "us-east-1",
			CommitmentUSDPerHour: 5, Expires: now.Add(365 * 24 * time.Hour),
		}},
	}
	ledger := domain.NewLedger(inv, commit.Usage{Lines: []commit.UsageLine{{
		ID: "i-app", Kind: commit.KindEC2, Region: "us-east-1", InstanceType: "m5.xlarge",
		Quantity: 30, ODRate: 0.192, ComputeSPRate: 0.132,
	}}})

	recs := reg.Recommend(now, ledger)
	if len(recs) != 1 {
		t.Fatalf("got %d recommendations, want 1:\n%+v", len(recs), recs)
	}
	rec := recs[0]
	if rec.Target.ID != "vol-0a1" {
		t.Fatalf("recommended %s, want vol-0a1", rec.Target.ID)
	}
	if rec.Suppressed {
		t.Fatalf("suppressed: %s", rec.Reason)
	}
	// 4,000 GiB gp2 at $400/mo → gp3 5,200 IOPS / 130 MiB/s at $331.30/mo.
	if got := rec.Proposed.Attr(AttrIOPS); got != "5200" {
		t.Errorf("proposed IOPS = %s, want 5200", got)
	}
	if math.Abs(rec.NetSavingsMonthlyUSD-68.70) > 1e-6 {
		t.Errorf("net saving $%.2f/mo, want $68.70", rec.NetSavingsMonthlyUSD)
	}

	// The refusals are visible with their reasons, which is what the UI shows.
	rep := d.Assess(now, ledger)
	wantRefusal(t, rep, "vol-0a2", ReasonNotGP2)     // already gp3
	wantRefusal(t, rep, "vol-0a3", ReasonUnmeasured) // no metrics collected
	wantRefusal(t, rep, "vol-0a4", ReasonExceedsGP3) // 15k IOPS × headroom > 16k
	if rep.Proposed != 1 || rep.Refused != 3 {
		t.Errorf("report: %d proposed / %d refused, want 1 / 3", rep.Proposed, rep.Refused)
	}

	// --- plan, under guardrails ------------------------------------------
	windows, err := guard.ParseWindows("Mon-Sun 00:00-23:59")
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	g := domain.Guard{Now: now, Windows: windows}
	steps, err := reg.PlanSteps(Kind, recs, g)
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("planned %d steps, want 1", len(steps))
	}
	fingerprint := domain.Fingerprint(steps)
	if fingerprint == "" {
		t.Fatal("no plan fingerprint: kilter approve has nothing to bind to")
	}

	// --- act, against the mocked EC2 endpoint ----------------------------
	// Dry-run first: it must show the change and make none.
	dry := newActuator(t, f, clock, ModeDryRun, nil)
	if err := dry.Execute(t.Context(), steps[0]); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if f.ModifyCallCount() != 0 {
		t.Fatal("dry-run modified a volume")
	}
	if e, _ := dry.Entry(steps[0].Key); !strings.Contains(e.Detail, "gp3") {
		t.Errorf("dry-run entry does not show the change: %q", e.Detail)
	}

	act := newActuator(t, f, clock, ModeApply, nil)
	if err := act.Execute(t.Context(), steps[0]); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := d.RecordApplied(steps[0], clock.Now()); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}

	live, ok := f.VolumeByID("vol-0a1")
	if !ok || live.VolumeType != VolumeTypeGP3 || live.IOPS != 5200 || live.ThroughputMBps != 130 {
		t.Fatalf("the account did not change: %+v", live)
	}
	entry, ok := act.Entry(steps[0].Key)
	if !ok || entry.Status != StatusDone {
		t.Fatalf("ledger entry = %+v, want done", entry)
	}
	if entry.From.Attr(AttrVolumeType) != VolumeTypeGP2 || entry.To.Attr(AttrIOPS) != "5200" {
		t.Errorf("the ledger did not record both ends of the change: %+v", entry)
	}

	// --- observe again: the loop's output is the next round's input ------
	after := now.Add(time.Hour)
	snap2, err := c.Collect(t.Context(), after)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if err := d.Learn(snap2); err != nil {
		t.Fatalf("second Learn: %v", err)
	}
	rep2 := d.Assess(after, ledger)
	a := wantRefusal(t, rep2, "vol-0a1", ReasonNotGP2)
	if !strings.Contains(a.Refusal.Reason, "already gp3") {
		t.Errorf("second-round refusal = %q", a.Refusal.Reason)
	}
	if steps2, err := d.PlanSteps(d.Recommend(after, ledger), g); err != nil || len(steps2) != 0 {
		t.Errorf("second round planned %d step(s), err=%v; the loop does not converge", len(steps2), err)
	}

	// --- and the plan is still idempotent --------------------------------
	calls := f.ModifyCallCount()
	if err := act.Execute(t.Context(), steps[0]); err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	if f.ModifyCallCount() != calls {
		t.Error("re-executing the completed plan modified the volume again")
	}

	// --- undo ------------------------------------------------------------
	clock.Advance(DefaultCooldown + time.Minute)
	if err := act.Revert(t.Context(), steps[0]); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	live, _ = f.VolumeByID("vol-0a1")
	if live.VolumeType != VolumeTypeGP2 || live.IOPS != 12000 {
		t.Fatalf("revert did not restore the recorded gp2 volume: %+v", live)
	}
}

// TestEndToEndUnderShippedDefaults runs the same path with DefaultConfig — no
// lowered evidence gates — because the gates are the safety property and a
// suite that always relaxes them proves nothing about production.
func TestEndToEndUnderShippedDefaults(t *testing.T) {
	// 2,100 five-minute datapoints span 7.3 days: past MinWindow, past
	// MinSamples, full coverage.
	const n = 2100
	start := base
	now := start.Add(time.Duration(n) * time.Duration(PeriodSeconds) * time.Second)
	clock := newClock(now)

	f := newFixture(clock,
		[]VolumeRecord{gp2Volume("vol-full", 4000), gp2Volume("vol-thin", 4000)},
		measured("vol-full", start, n, 4000, 100),
		// 400 datapoints spanning 33 hours: past MinSamples, short of MinWindow.
		measured("vol-thin", now.Add(-400*time.Duration(PeriodSeconds)*time.Second), 400, 4000, 100))

	cfg := DefaultConfig()
	cfg.Scope, cfg.Region, cfg.ActuationAvailable = e2eScope, "us-east-1", true
	d, rep := assess(t, cfg, f, now, nil)

	// The fully observed volume may go below gp2's baseline.
	a, rec := wantProposal(t, rep, "vol-full")
	if a.Observed.Floor != FloorMeasured {
		t.Errorf("floor = %v, want %v after a full week", a.Observed.Floor, FloorMeasured)
	}
	if got := rec.Proposed.Attr(AttrIOPS); got != "5200" {
		t.Errorf("proposed IOPS = %s, want 5200", got)
	}
	if rec.Confidence < 0.99 {
		t.Errorf("confidence = %.3f after a full week of complete data", rec.Confidence)
	}

	// The thinly observed one may not: same measurement, floored proposal.
	a2, rec2 := wantProposal(t, rep, "vol-thin")
	if a2.Observed.Floor != FloorGP2Baseline {
		t.Errorf("thin volume floor = %v, want %v", a2.Observed.Floor, FloorGP2Baseline)
	}
	if got := rec2.Proposed.Attr(AttrIOPS); got != "12000" {
		t.Errorf("thin volume proposed IOPS = %s, want gp2's 12000 baseline", got)
	}
	if rec2.Confidence >= rec.Confidence {
		t.Errorf("thin confidence %.3f is not below full confidence %.3f", rec2.Confidence, rec.Confidence)
	}

	steps, err := d.PlanSteps(d.Recommend(now, nil), domain.Guard{Now: now})
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("planned %d steps, want 2", len(steps))
	}
	act := newActuator(t, f, clock, ModeApply, func(c *ActuatorConfig) {
		c.Logger = slog.New(slog.DiscardHandler)
	})
	for _, s := range steps {
		if err := act.Execute(t.Context(), s); err != nil {
			t.Fatalf("execute %s: %v", s.Target.ID, err)
		}
	}
	full, _ := f.VolumeByID("vol-full")
	thin, _ := f.VolumeByID("vol-thin")
	if full.IOPS != 5200 || thin.IOPS != 12000 {
		t.Errorf("applied configurations = full %d IOPS, thin %d IOPS", full.IOPS, thin.IOPS)
	}
}
