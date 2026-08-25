package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
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
		At:   t0.Add(30 * time.Minute), // between the two cost measurements
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

// TestRealizedSavingsRequiresPostActionMeasurement pins the honesty rule of
// the money math: a cost measurement from before the action says nothing
// about its effect, so realized savings stay 0 until a post-action point
// exists.
func TestRealizedSavingsRequiresPostActionMeasurement(t *testing.T) {
	l := &ledgerState{}
	l.addCost(t0, 1.00) // pre-action measurement only
	l.add(LedgerEntry{At: t0.Add(time.Hour), Mode: "apply", Done: 1, CostBeforeHourlyUSD: 0.80})
	if got := l.report().RealizedMonthlyUSD; got != 0 {
		t.Fatalf("no post-action measurement yet, realized must be 0, got %v", got)
	}
	l.addCost(t0.Add(2*time.Hour), 0.50)
	want := (0.80 - 0.50) * 730
	if got := l.report().RealizedMonthlyUSD; got < want-0.01 || got > want+0.01 {
		t.Fatalf("realized = %v, want %v", got, want)
	}
}

// TestRealizedSavingsLatestByTimestamp: "latest" means latest measurement
// time, not last appended — a replayed old snapshot must not roll the
// comparison point backwards.
func TestRealizedSavingsLatestByTimestamp(t *testing.T) {
	l := &ledgerState{}
	l.add(LedgerEntry{At: t0, Mode: "apply", Done: 1, CostBeforeHourlyUSD: 0.80})
	l.addCost(t0.Add(2*time.Hour), 0.50) // the true latest measurement
	l.addCost(t0.Add(time.Hour), 0.70)   // older point replayed afterwards
	want := (0.80 - 0.50) * 730
	if got := l.report().RealizedMonthlyUSD; got < want-0.01 || got > want+0.01 {
		t.Fatalf("realized = %v, want %v (latest-by-time point)", got, want)
	}
}

func TestRealizedSavingsBaselineSelection(t *testing.T) {
	l := &ledgerState{}
	// Dry-runs and applies with nothing done never set the baseline.
	l.add(LedgerEntry{At: t0, Mode: "dry-run", Done: 3, CostBeforeHourlyUSD: 9.99})
	l.add(LedgerEntry{At: t0, Mode: "apply", Done: 0, CostBeforeHourlyUSD: 8.88})
	l.addCost(t0.Add(time.Hour), 0.50)
	if got := l.report().RealizedMonthlyUSD; got != 0 {
		t.Fatalf("no applied work → realized must be 0, got %v", got)
	}
	// The OLDEST applied entry is the baseline, not the newest.
	l.add(LedgerEntry{At: t0.Add(10 * time.Minute), Mode: "apply", Done: 1, CostBeforeHourlyUSD: 1.00})
	l.add(LedgerEntry{At: t0.Add(20 * time.Minute), Mode: "apply", Done: 1, CostBeforeHourlyUSD: 0.70})
	want := (1.00 - 0.50) * 730
	if got := l.report().RealizedMonthlyUSD; got < want-0.01 || got > want+0.01 {
		t.Fatalf("realized = %v, want %v (oldest applied baseline)", got, want)
	}
}

func TestLedgerBounded(t *testing.T) {
	l := &ledgerState{}
	for i := 0; i < ledgerLimit+50; i++ {
		l.add(LedgerEntry{At: t0.Add(time.Duration(i) * time.Minute), Mode: "dry-run"})
	}
	for i := 0; i < costHistLimit+50; i++ {
		l.addCost(t0.Add(time.Duration(i)*time.Minute), 1)
	}
	rep := l.report()
	if len(rep.Entries) != ledgerLimit {
		t.Fatalf("entries = %d, want cap %d", len(rep.Entries), ledgerLimit)
	}
	// Entries are sorted newest-first and the newest survived the cap.
	if !rep.Entries[0].At.Equal(t0.Add(time.Duration(ledgerLimit+49) * time.Minute)) {
		t.Fatalf("newest entry lost: %v", rep.Entries[0].At)
	}
	if len(rep.CostTimeline) != costHistLimit {
		t.Fatalf("cost points = %d, want cap %d", len(rep.CostTimeline), costHistLimit)
	}
	last := rep.CostTimeline[len(rep.CostTimeline)-1]
	if !last.At.Equal(t0.Add(time.Duration(costHistLimit+49) * time.Minute)) {
		t.Fatalf("newest cost point lost: %v", last.At)
	}
}

func TestReportValidation(t *testing.T) {
	tests := []struct {
		name string
		e    LedgerEntry
		ok   bool
	}{
		{"apply ok", LedgerEntry{Mode: "apply"}, true},
		{"dry-run ok", LedgerEntry{Mode: "dry-run"}, true},
		{"empty mode", LedgerEntry{}, false},
		{"misspelled mode", LedgerEntry{Mode: "Apply"}, false},
		{"negative done", LedgerEntry{Mode: "apply", Done: -1}, false},
		{"negative failed", LedgerEntry{Mode: "apply", Failed: -3}, false},
		{"negative cost before", LedgerEntry{Mode: "apply", CostBeforeHourlyUSD: -0.5}, false},
		{"negative projected", LedgerEntry{Mode: "apply", ProjectedHourlyUSD: -1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.e.validate()
			if tt.ok && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("want rejection, got nil")
			}
		})
	}
}

func TestReportEndpointRejectsGarbage(t *testing.T) {
	b, _ := newBrain(t, "", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	client, _ := NewClient(srv.URL, "")
	ctx := context.Background()

	if err := client.ReportExecution(ctx, "prod", LedgerEntry{Mode: "Apply", Done: 1}); err == nil {
		t.Fatal("misspelled mode must be rejected, not silently dropped from realized savings")
	}
	if err := client.ReportExecution(ctx, "prod", LedgerEntry{Mode: "apply", Done: -2}); err == nil {
		t.Fatal("negative counters must be rejected")
	}
	if err := client.ReportExecution(ctx, "prod", LedgerEntry{Mode: "apply", CostBeforeHourlyUSD: -9}); err == nil {
		t.Fatal("negative cost must be rejected")
	}
	if got := len(b.Ledger("prod").Entries); got != 0 {
		t.Fatalf("rejected entries must not be recorded, ledger has %d", got)
	}
	if err := client.ReportExecution(ctx, "prod", LedgerEntry{Mode: "apply", Done: 1}); err != nil {
		t.Fatalf("valid entry must be accepted: %v", err)
	}
	if got := len(b.Ledger("prod").Entries); got != 1 {
		t.Fatalf("ledger entries = %d, want 1", got)
	}
}

func TestApprovalExpiryAndPurge(t *testing.T) {
	a := &approvalState{byF: map[string]Approval{}}
	a.approve("fp-aaaaaaaa", t0)
	if !a.approved("fp-aaaaaaaa", t0.Add(approvalTTL)) {
		t.Fatal("approval must hold until the TTL boundary")
	}
	if a.approved("fp-aaaaaaaa", t0.Add(approvalTTL+time.Second)) {
		t.Fatal("approval must expire after the TTL")
	}
	if got := a.list(t0.Add(approvalTTL + time.Second)); len(got) != 0 {
		t.Fatalf("expired approvals must not be listed: %+v", got)
	}

	// approve() itself purges expired fingerprints so the map stays bounded.
	b := &approvalState{byF: map[string]Approval{}}
	b.approve("fp-old11111", t0)
	b.approve("fp-new22222", t0.Add(2*approvalTTL))
	b.mu.Lock()
	n := len(b.byF)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("expired approval not purged on approve: map holds %d", n)
	}
}

func TestApprovalFingerprintBounds(t *testing.T) {
	b, _ := newBrain(t, "", false)
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	client, _ := NewClient(srv.URL, "")
	ctx := context.Background()

	if err := client.Approve(ctx, "prod", strings.Repeat("a", 7)); err == nil {
		t.Fatal("7-char fingerprint must be rejected")
	}
	if err := client.Approve(ctx, "prod", strings.Repeat("a", 129)); err == nil {
		t.Fatal("129-char fingerprint must be rejected")
	}
	for _, n := range []int{8, 128} {
		if err := client.Approve(ctx, "prod", strings.Repeat("a", n)); err != nil {
			t.Fatalf("%d-char fingerprint must be accepted: %v", n, err)
		}
	}
}

func TestConcurrentTrustState(t *testing.T) {
	b, _ := newBrain(t, "", false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				at := t0.Add(time.Duration(i*20+j) * time.Minute)
				b.ledgerFor("prod").addCost(at, float64(j))
				b.ledgerFor("prod").add(LedgerEntry{At: at, Mode: "apply", Done: 1})
				_ = b.Ledger("prod")
				fp := fmt.Sprintf("fp-%08d", i)
				b.approvalsFor("prod").approve(fp, at)
				_ = b.Approved("prod", fp)
				_ = b.approvalsFor("prod").list(at)
			}
		}(i)
	}
	wg.Wait()
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
