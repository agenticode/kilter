package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
)

func TestLedgerRoundtripAndRealized(t *testing.T) {
	b, _ := newBrain(t, "tok", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	client, _ := NewClient(srv.URL, "tok")
	ctx := context.Background()

	// Cost history: $0.80/h before, $0.55/h after the action.
	snap := trainingSnapshot("prod")
	snap.Timestamp = t0
	b.Ingest(snap) // 2× m5.xlarge = 0.384... use recorded value from report below

	entry := LedgerEntry{
		Mode: "apply", Fingerprint: "abc123def456", Risk: "low",
		CostBeforeHourlyUSD: 0.80, ProjectedHourlyUSD: 0.55, ProjectedMonthlySavings: 182.5,
		Steps: []actuate.StepStatus{{
			Step: plan.Step{Seq: 1, Type: plan.StepResizeWorkload,
				Workload:  model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "web"},
				Container: "app",
				FromReq:   model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30},
				ToReq:     model.Resources{MilliCPU: 300, MemoryBytes: 1 << 30}},
			Status: "done",
		}},
		Done: 1,
	}
	if err := client.ReportExecution(ctx, "prod", entry); err != nil {
		t.Fatal(err)
	}
	// A later, cheaper snapshot moves the measured curve down.
	later := trainingSnapshot("prod")
	later.Timestamp = t0.Add(time.Hour)
	later.Nodes = later.Nodes[:1] // one node gone → cost halves
	b.Ingest(later)

	rep, err := client.GetLedger(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Fingerprint != "abc123def456" {
		t.Fatalf("entries: %+v", rep.Entries)
	}
	if len(rep.CostTimeline) != 2 {
		t.Fatalf("cost timeline points: %d", len(rep.CostTimeline))
	}
	// realized = (0.80 − latest) × 730; latest = one m5.xlarge = 0.192.
	want := (0.80 - 0.192) * 730
	if diff := rep.RealizedMonthlyUSD - want; diff > 0.01 || diff < -0.01 {
		t.Fatalf("realized %v, want %v", rep.RealizedMonthlyUSD, want)
	}
	if rep.Method == "" {
		t.Fatal("the realized-savings math must be stated")
	}
	// From values survived → undo has what it needs.
	if rep.Entries[0].Steps[0].Step.FromReq.MilliCPU != 2000 {
		t.Fatal("From values lost in ledger")
	}
}

func TestApprovalsLifecycle(t *testing.T) {
	b, _ := newBrain(t, "tok", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	client, _ := NewClient(srv.URL, "tok")
	ctx := context.Background()

	if b.Approved("prod", "fp-1234567890") {
		t.Fatal("nothing approved yet")
	}
	if err := client.Approve(ctx, "prod", "fp-1234567890"); err != nil {
		t.Fatal(err)
	}
	if !b.Approved("prod", "fp-1234567890") {
		t.Fatal("approval not registered")
	}
	if b.Approved("staging", "fp-1234567890") {
		t.Fatal("approvals must be per-cluster")
	}
	aps, err := client.GetApprovals(ctx, "prod")
	if err != nil || len(aps) != 1 {
		t.Fatalf("approvals list: %v %v", aps, err)
	}
	if aps[0].ExpiresAt.Sub(aps[0].ApprovedAt) != 24*time.Hour {
		t.Fatalf("TTL wrong: %+v", aps[0])
	}
	// Garbage fingerprints rejected.
	if err := client.Approve(ctx, "prod", "x"); err == nil {
		t.Fatal("short fingerprint must be rejected")
	}
	// Read token can view but not approve.
	b2, _ := NewBrain(BrainConfig{Token: "admin", ReadToken: "viewer"}, b.catalog, nil)
	srv2 := httptest.NewServer(b2.Handler())
	defer srv2.Close()
	viewer, _ := NewClient(srv2.URL, "viewer")
	if _, err := viewer.GetApprovals(ctx, "prod"); err != nil {
		t.Fatalf("viewer must list approvals: %v", err)
	}
	if err := viewer.Approve(ctx, "prod", "fp-1234567890"); err == nil {
		t.Fatal("viewer must not approve")
	}
}

func TestPlanFingerprintStability(t *testing.T) {
	b, _ := newBrain(t, "", false)
	b.Ingest(trainingSnapshot("prod"))
	p1, err := b.Plan("prod")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := b.Plan("prod")
	if p1.Fingerprint == "" || p1.Fingerprint != p2.Fingerprint {
		t.Fatalf("fingerprint must be stable across rebuilds: %q vs %q", p1.Fingerprint, p2.Fingerprint)
	}
}
