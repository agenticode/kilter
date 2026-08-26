package lambda

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

func newTestDomain(t *testing.T, pts ...point) *Domain {
	t.Helper()
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	tgt := target(fn(1024), events(testSpan, pts...))
	if err := d.Observe(snapOf(testSpan, tgt)); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return d
}

// The read-only half of the seam, made structural: there is no argument, no
// guard configuration and no recommendation that gets a step out of this
// domain.
func TestPlanStepsAlwaysRefuses(t *testing.T) {
	d := newTestDomain(t,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	)
	recs := d.Recommend(testNow, nil)
	if len(recs) == 0 {
		t.Fatalf("expected a recommendation to try to plan")
	}
	guards := []domain.Guard{
		{Now: testNow},
		{Now: testNow, MaxSteps: 100},
		{Now: testNow, Freeze: false, BreakerOpen: false},
	}
	for _, g := range guards {
		steps, err := d.PlanSteps(recs, g)
		if !errors.Is(err, domain.ErrReportOnly) {
			t.Fatalf("PlanSteps = %v, want ErrReportOnly", err)
		}
		if len(steps) != 0 {
			t.Fatalf("PlanSteps returned %d steps; this domain has no actuator", len(steps))
		}
	}
	// And the core's own view agrees.
	h := d.Health(testNow)
	if !h.ReportOnly {
		t.Fatalf("Health.ReportOnly must be true unconditionally")
	}
	if !strings.Contains(h.Reason, "advisory only") {
		t.Errorf("Health must say why: %s", h.Reason)
	}
}

// A registered Lambda domain must not be able to talk the core into planning.
func TestRegistryRefusesStepsForThisDomain(t *testing.T) {
	d := newTestDomain(t,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	)
	reg := domain.NewRegistry()
	if err := reg.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.Len() != 1 {
		t.Fatalf("registry has %d domains", reg.Len())
	}
	recs := reg.Recommend(testNow, nil)
	if len(recs) == 0 {
		t.Fatalf("the registry produced no recommendations")
	}
	for _, r := range recs {
		if r.Action != domain.ActionAdvisory {
			t.Fatalf("recommendation %s has action %q; this domain emits only advisory", r.Target, r.Action)
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("recommendation fails the seam contract: %v", err)
		}
	}
}

func TestRecommendationsCarryTheSuppressionAndNoMoney(t *testing.T) {
	// One measured point: the refusal must stay VISIBLE as a recommendation
	// with its code, claiming nothing.
	d := newTestDomain(t, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 1500})
	recs := d.Recommend(testNow, nil)
	if len(recs) != 1 {
		t.Fatalf("got %d recommendations, want 1", len(recs))
	}
	r := recs[0]
	if !r.Suppressed || r.SuppressCode != ReasonSingleMemoryPoint {
		t.Fatalf("suppressed=%v code=%q, want the single-memory-point refusal", r.Suppressed, r.SuppressCode)
	}
	if r.ClaimableMonthlyUSD() != 0 || r.GrossSavingsMonthlyUSD != 0 || r.NetSavingsMonthlyUSD != 0 {
		t.Fatalf("a refused change must claim exactly nothing, got gross %v net %v",
			r.GrossSavingsMonthlyUSD, r.NetSavingsMonthlyUSD)
	}
	if r.Proposed.Attr(AttrMemoryMB) != "512" {
		t.Errorf("the contemplated setting should still be visible, got %q", r.Proposed.Attr(AttrMemoryMB))
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("suppressed recommendation is malformed: %v", err)
	}
}

func TestRecommendProducesNothingWhenNothingWasContemplated(t *testing.T) {
	// Already at the cheapest measured setting: there is no alternative to
	// express, and a Recommendation whose Proposed equals its Current is not a
	// recommendation.
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	tgt := target(fn(512), events(testSpan,
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
	))
	if err := d.Observe(snapOf(testSpan, tgt)); err != nil {
		t.Fatal(err)
	}
	if recs := d.Recommend(testNow, nil); len(recs) != 0 {
		t.Fatalf("got %d recommendations, want 0", len(recs))
	}
	// The refusal still exists — in the report, which is the complete record.
	rep := d.Report(testNow, nil)
	a, ok := rep.For(tgt.Ref.ID)
	if !ok || !a.Suppressed(ReasonNoCheaperMeasurement) {
		t.Fatalf("the refusal must survive in the report: %v", codes(a))
	}
}

func TestLearnAcceptsTheGenericSeamAndSaysWhatItCannotCarry(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	gen := &domain.Snapshot{
		Domain: Kind, Scope: testScope, Timestamp: testNow,
		Targets: []domain.Target{{
			Ref:    domain.TargetRef{Domain: Kind, Scope: testScope, ID: testARN, Name: testName},
			Spec:   SpecFor(fn(1024), 1024, ArchX86),
			Labels: map[string]string{"team": "payments"},
		}},
		Samples: []domain.Sample{
			{Ref: domain.TargetRef{Domain: Kind, Scope: testScope, ID: testARN}, Metric: MetricInvocations,
				Value: 5000, Timestamp: testNow, WindowSeconds: 300},
		},
	}
	if err := d.Learn(gen); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	rep := d.Report(testNow, nil)
	a, ok := rep.For(testARN)
	if !ok {
		t.Fatalf("the generic seam must at least deliver the inventory")
	}
	if a.Function.MemoryMB != 1024 || a.Function.Arch() != ArchX86 {
		t.Errorf("configuration did not survive the generic seam: %+v", a.Function)
	}
	// And it refuses honestly, because REPORT records cannot ride that seam.
	onlySuppression(t, a, ReasonNoReportEvidence)

	// A snapshot addressed elsewhere is a wiring bug and is reported as one.
	if err := d.Learn(&domain.Snapshot{Domain: domain.EC2}); !errors.Is(err, domain.ErrWrongDomain) {
		t.Fatalf("Learn(other domain) = %v, want ErrWrongDomain", err)
	}
	if err := d.Observe(&Snapshot{Domain: domain.EC2}); !errors.Is(err, domain.ErrWrongDomain) {
		t.Fatalf("Observe(other domain) = %v, want ErrWrongDomain", err)
	}
	// Nil is a no-op, not a crash: an unreachable collector is operational.
	if err := d.Learn(nil); err != nil {
		t.Fatalf("Learn(nil) = %v", err)
	}
	if err := d.Observe(nil); err != nil {
		t.Fatalf("Observe(nil) = %v", err)
	}
}

// The generic seam preserves REPORT evidence an earlier native Observe
// delivered: adding configuration must never erase measurements.
func TestLearnDoesNotEraseNativeEvidence(t *testing.T) {
	d := newTestDomain(t,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	)
	before := d.Report(testNow, nil)
	a0, _ := before.For(testARN)
	if a0.Proposal == nil {
		t.Fatalf("setup: expected a proposal")
	}
	err := d.Learn(&domain.Snapshot{
		Domain: Kind, Scope: testScope, Timestamp: testNow,
		Targets: []domain.Target{{
			Ref:  domain.TargetRef{Domain: Kind, Scope: testScope, ID: testARN, Name: testName},
			Spec: SpecFor(fn(1024), 1024, ArchX86),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := d.Report(testNow, nil)
	a1, _ := after.For(testARN)
	if a1.Proposal == nil {
		t.Fatalf("the generic seam erased the REPORT evidence: %v", codes(a1))
	}
}

func TestCheckpointRoundTrips(t *testing.T) {
	d := newTestDomain(t,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	)
	blob, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(blob); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	again, err := restored.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != string(again) {
		t.Fatalf("checkpoint did not round-trip")
	}
	a, _ := d.Report(testNow, nil).For(testARN)
	b, _ := restored.Report(testNow, nil).For(testARN)
	if (a.Proposal == nil) != (b.Proposal == nil) {
		t.Fatalf("restored state produced a different verdict")
	}
	if a.Proposal != nil && a.Proposal.MemoryMB != b.Proposal.MemoryMB {
		t.Fatalf("restored proposal differs: %d vs %d", a.Proposal.MemoryMB, b.Proposal.MemoryMB)
	}
	// Garbage in is an error, not a silent reset.
	if err := restored.Restore([]byte("{not json")); err == nil {
		t.Errorf("Restore must reject malformed input")
	}
	if err := restored.Restore(nil); err != nil {
		t.Errorf("Restore(nil) must be a no-op, got %v", err)
	}
}

func TestHealthReportsWhatItKnows(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	h := d.Health(testNow)
	if h.Ready || !h.ReportOnly || h.Targets != 0 {
		t.Fatalf("an empty domain is not ready and is report-only: %+v", h)
	}
	if !strings.Contains(h.Reason, "no Lambda functions") {
		t.Errorf("Health must say what is missing: %s", h.Reason)
	}
	d = newTestDomain(t, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700})
	h = d.Health(testNow)
	if !h.Ready || !h.ReportOnly || h.Targets != 1 {
		t.Fatalf("a learned domain is ready and still report-only: %+v", h)
	}
	if h.Kind != Kind {
		t.Errorf("Health.Kind = %q", h.Kind)
	}
}

// Compute Savings Plans cover Lambda duration account-wide. A GB-second
// reduction that the commitment would have absorbed anyway is worth less — or
// nothing — than its list price, and the domain must say so instead of
// claiming it (§4.4, §7 trap 1).
func TestCommitmentWaterfallCanSuppressAMeasuredSaving(t *testing.T) {
	tgt := target(fn(1024), events(testSpan,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	))
	// Without commitments the change is proposed.
	base := assessTarget(t, DefaultConfig(), nil, testSpan, tgt)
	if base.Proposal == nil {
		t.Fatalf("setup: expected a proposal, got %v", codes(base))
	}

	// A Compute Savings Plan committing far more per hour than this function
	// spends: the reduction strands committed money.
	inv := &commit.Inventory{SavingsPlans: []commit.SavingsPlan{{
		ID: "sp-1", Type: commit.SPCompute, CommitmentUSDPerHour: 1.0,
		Expires: testNow.AddDate(1, 0, 0),
	}}}
	ledger := domain.NewLedger(inv, commit.Usage{})
	withSP := assessTarget(t, DefaultConfig(), ledger, testSpan, tgt)
	if withSP.Proposal != nil {
		t.Fatalf("a fully-stranded change must not be proposed as a saving")
	}
	if len(withSP.Suppressions) != 1 {
		t.Fatalf("expected one suppression, got %v", codes(withSP))
	}
	if code := withSP.Suppressions[0].Code; !strings.HasPrefix(code, "commitment-") {
		t.Fatalf("suppression = %q, want a commitment reason", code)
	}
}

func TestSnapshotGenericIsLossyAndSaysSo(t *testing.T) {
	tgt := target(fn(1024), events(testSpan, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700}),
		invocationSeries(testSpan, 5000))
	gen := snapOf(testSpan, tgt).Generic()
	if gen.Domain != Kind || len(gen.Targets) != 1 {
		t.Fatalf("Generic() lost the inventory: %+v", gen)
	}
	if len(gen.Targets[0].Blind) == 0 ||
		!strings.Contains(gen.Targets[0].Blind[0], "REPORT records do not fit") {
		t.Errorf("Generic() must declare the blind spot it creates: %v", gen.Targets[0].Blind)
	}
	if len(gen.Samples) == 0 {
		t.Errorf("aggregate metric samples should survive the projection")
	}
	// Round-tripping through the generic seam loses the REPORT evidence, and a
	// domain fed only that refuses rather than guessing.
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(gen); err != nil {
		t.Fatal(err)
	}
	a, _ := d.Report(testNow, nil).For(testARN)
	onlySuppression(t, a, ReasonNoReportEvidence)
}

func TestNoActuationSurfaceExists(t *testing.T) {
	// A compile-time assertion is the real test: this package must satisfy the
	// read-only Domain contract and must NOT satisfy domain.Actuator. If a
	// later change adds Execute/Revert to *Domain, this stops compiling.
	var _ domain.Domain = (*Domain)(nil)
	if _, isActuator := any((*Domain)(nil)).(domain.Actuator); isActuator {
		t.Fatalf("pkg/lambda grew an actuator; U9 is advisory only")
	}
	if _, isActuator := any((*Sizer)(nil)).(domain.Actuator); isActuator {
		t.Fatalf("the sizer grew an actuator")
	}
}

func TestObserveReplacesRatherThanAccumulates(t *testing.T) {
	d := newTestDomain(t, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700})
	first, _ := d.Report(testNow, nil).For(testARN)
	// The same window collected again must not double the invocation count:
	// the collector re-reads a window every tick.
	tgt := target(fn(1024), events(testSpan, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700}))
	if err := d.Observe(snapOf(testSpan, tgt)); err != nil {
		t.Fatal(err)
	}
	second, _ := d.Report(testNow, nil).For(testARN)
	if first.Observation.Records != second.Observation.Records {
		t.Fatalf("re-observing the same window changed the record count: %d → %d",
			first.Observation.Records, second.Observation.Records)
	}
}

func TestDomainIsSafeUnderConcurrentUse(t *testing.T) {
	d := newTestDomain(t,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 400},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 400},
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			d.Recommend(testNow.Add(time.Duration(i)*time.Minute), nil)
			d.Health(testNow)
		}
	}()
	tgt := target(fn(1024), events(testSpan, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 400}))
	for i := 0; i < 20; i++ {
		if err := d.Observe(snapOf(testSpan, tgt)); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Checkpoint(); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}
