package ecs

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// TestAbsoluteFromPercentIsNotInverted is the unit-level pin on this package's
// central failure mode.
//
// ECS publishes CPUUtilization and MemoryUtilization as a percentage OF
// RESERVED. The conversion is `percent / 100 × reserved`. Every plausible
// mistake — dividing, dropping the /100, swapping the operands — produces a
// different number, and each is checked by name so the failure message says
// which one was made.
func TestAbsoluteFromPercentIsNotInverted(t *testing.T) {
	const (
		percent  = 25.0
		reserved = 4000.0 // milli-CPU, i.e. a 4-vCPU task
		want     = 1000.0 // 25 % of 4 vCPU is 1 vCPU
	)
	got := AbsoluteFromPercent(percent, int64(reserved))
	if !closeTo(got, want) {
		t.Fatalf("AbsoluteFromPercent(%v%%, %v) = %v, want %v", percent, reserved, got, want)
	}

	wrong := map[string]float64{
		"reserved ÷ percent (inverted)":       reserved / percent,
		"reserved ÷ (percent/100) (inverted)": reserved / (percent / 100),
		"percent × reserved (missing ÷100)":   percent * reserved,
		"percent ÷ reserved":                  percent / reserved,
	}
	for name, v := range wrong {
		if closeTo(got, v) {
			t.Errorf("AbsoluteFromPercent produced %v, which equals %s — the conversion is inverted", got, name)
		}
	}

	// Garbage never becomes demand.
	for _, p := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if v := AbsoluteFromPercent(p, 4000); v != 0 {
			t.Errorf("AbsoluteFromPercent(%v, 4000) = %v, want 0", p, v)
		}
	}
	if v := AbsoluteFromPercent(50, 0); v != 0 {
		t.Errorf("AbsoluteFromPercent with a zero denominator = %v, want 0", v)
	}
	// Over-100 % is real (a task bursting over its soft reservation) and must
	// survive: clamping it would hide the one case where growth is warranted.
	if v := AbsoluteFromPercent(150, 4000); !closeTo(v, 6000) {
		t.Errorf("AbsoluteFromPercent(150%%, 4000) = %v, want 6000", v)
	}
}

// TestReservedVsUsedMathFromUtilizationPercentages walks the whole conversion
// end to end and asserts the exact tier it lands on.
//
// The service reserves 4 vCPU / 8 GB and runs at 30 % of that. The right answer
// is 2 vCPU / 4 GB. An implementation that divided instead of multiplying would
// answer 0.25 vCPU / 0.5 GB — a plausible-looking, catastrophically wrong
// recommendation — and one that dropped the /100 would blow past the Fargate
// ceiling. Both are asserted against by name.
func TestReservedVsUsedMathFromUtilizationPercentages(t *testing.T) {
	f := newFixture() // 4096 CPU units / 8192 MiB, 30 % / 30 %, 4 tasks
	a := assess(t, f)

	if a.Proposal == nil {
		t.Fatalf("no proposal; suppressions: %v", suppressionCodes(a))
	}
	if got := a.CurrentTier.String(); got != "4vCPU 8GB" {
		t.Fatalf("current tier %s, want 4vCPU 8GB", got)
	}

	// The demand, spelled out: 30 % of 4000m = 1200m, ×1.30 headroom = 1560m;
	// 30 % of 8 GiB = 2.4 GiB, ×1.25 headroom = 3 GiB.
	wantDemand := model.Resources{MilliCPU: 1560, MemoryBytes: 3 << 30}
	if a.Demand.Target != wantDemand {
		t.Fatalf("demand %s, want %s (30%% × reserved × headroom)", a.Demand.Target, wantDemand)
	}
	if a.Demand.Reserved != (model.Resources{MilliCPU: 4000, MemoryBytes: 8 << 30}) {
		t.Fatalf("reserved denominator %s, want 4000m/8GiB", a.Demand.Reserved)
	}

	if got := a.Proposal.Tier.String(); got != "2vCPU 4GB" {
		t.Fatalf("proposed tier %s, want 2vCPU 4GB", got)
	}

	// The two wrong conversions, computed here and asserted to be different
	// tiers. This is what would silently pass if the multiplication flipped.
	reservedCPU, reservedMem, pctUsed := 4000.0, float64(8<<30), 30.0
	inverted, err := RoundUpTier(model.Resources{
		MilliCPU:    int64(reservedCPU / pctUsed * 1.30),
		MemoryBytes: int64(reservedMem / pctUsed * 1.25),
	})
	if err == nil && inverted == a.Proposal.Tier {
		t.Fatal("the inverted conversion lands on the same tier: this test cannot detect the bug")
	}
	if _, err := RoundUpTier(model.Resources{
		MilliCPU:    int64(pctUsed * reservedCPU),
		MemoryBytes: int64(pctUsed * reservedMem),
	}); err == nil {
		t.Fatal("the missing-÷100 conversion is representable: this test cannot detect the bug")
	}

	// And the money, from pkg/pricing's own rates.
	r := DefaultRates()
	wantCur := r.Cost(a.CurrentTier, PlatformX86OnDemand) * 4
	wantProp := r.Cost(a.Proposal.Tier, PlatformX86OnDemand) * 4
	if !closeTo(a.CurrentHourlyUSD, wantCur) || !closeTo(a.Proposal.HourlyUSD, wantProp) {
		t.Fatalf("hourly %.6f → %.6f, want %.6f → %.6f",
			a.CurrentHourlyUSD, a.Proposal.HourlyUSD, wantCur, wantProp)
	}
	if want := (wantCur - wantProp) * pricing.HoursPerMonth; !closeTo(a.Proposal.GrossMonthlyUSD, want) {
		t.Fatalf("gross $%.4f/mo, want $%.4f/mo", a.Proposal.GrossMonthlyUSD, want)
	}
	if a.ClaimableMonthlyUSD() != a.Proposal.NetMonthlyUSD {
		t.Fatalf("claimable $%.4f but net $%.4f", a.ClaimableMonthlyUSD(), a.Proposal.NetMonthlyUSD)
	}
	if a.Proposal.Action != domain.ActionRolling {
		t.Errorf("action %q, want %q: a new revision means a new deployment",
			a.Proposal.Action, domain.ActionRolling)
	}
	if a.Proposal.Risk != "medium" {
		t.Errorf("risk %q, want medium: the proposal lowers the memory reservation", a.Proposal.Risk)
	}

	// The evidence must carry the percentages AND their denominator; a
	// percentage without its denominator is not evidence of anything.
	var sawPercent, sawReserved bool
	for _, e := range a.Evidence {
		if strings.Contains(e.Metric, "percent-of-reserved") {
			sawPercent = true
		}
		if e.Metric == "reserved" && e.Value == a.Demand.Reserved.String() {
			sawReserved = true
		}
	}
	if !sawPercent || !sawReserved {
		t.Errorf("evidence %v is missing the percentages or the reservation they are of", a.Evidence)
	}
}

// TestNoTierChangeClaimsExactlyZero is §7 trap 2 as a refusal.
//
// The service reserves 1 vCPU / 2 GB and uses half of it. Learned demand is
// 650m / 1.25 GB — a 35 % CPU cut and a 37 % memory cut — and it rounds to the
// tier the service is already on. AWS bills tiers, not requests, so the saving
// is exactly $0.00 and this package says so instead of reporting a percentage.
func TestNoTierChangeClaimsExactlyZero(t *testing.T) {
	f := newFixture(func(f *fixture) {
		f.cpu, f.memory = "1024", "2048" // 1 vCPU / 2 GB
		f.constCPUPct, f.constMem = 50, 50
	})
	a := assess(t, f)

	if a.Proposal != nil {
		t.Fatalf("proposed %s from %s: nothing crosses a tier boundary here", a.Proposal.Tier, a.CurrentTier)
	}
	if !hasSuppression(a, ReasonNoTierChange) {
		t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonNoTierChange)
	}
	// The demand really is well below the reservation in both dimensions —
	// this is not a service that is already right-sized.
	if !(a.Demand.Target.MilliCPU < a.Demand.Reserved.MilliCPU) ||
		!(a.Demand.Target.MemoryBytes < a.Demand.Reserved.MemoryBytes) {
		t.Fatalf("demand %s is not below the reservation %s; the test proves nothing",
			a.Demand.Target, a.Demand.Reserved)
	}
	// Exactly zero. Not "about zero".
	if got := a.ClaimableMonthlyUSD(); got != 0 {
		t.Fatalf("claimable $%v, want exactly $0", got)
	}
	if !strings.Contains(a.Suppressions[0].Reason, "$0.00") {
		t.Errorf("reason %q does not say the saving is $0.00", a.Suppressions[0].Reason)
	}

	// And the report total is zero too — a $0 change must not reach a headline.
	rep := NewSizer(testConfig()).Report(f.snapshot(), testNow, nil)
	if rep.ClaimableMonthlyUSD != 0 {
		t.Fatalf("report claims $%v/mo from a change that crosses no tier boundary", rep.ClaimableMonthlyUSD)
	}
}

// TestSpotAndARMAdvisoriesAreLegalHere: the two levers §7 trap 3 forbids on EKS
// are the high-value ones on ECS. Both are priced, both carry the precondition
// Kilter cannot verify, and neither is ever actuatable.
func TestSpotAndARMAdvisoriesAreLegalHere(t *testing.T) {
	f := newFixture()
	a := assess(t, f)

	spot, okSpot := advisory(a, AdvisorySpot)
	arm, okARM := advisory(a, AdvisoryARM)
	if !okSpot || !okARM {
		t.Fatalf("advisories %v, want both %s and %s", a.Advisories, AdvisorySpot, AdvisoryARM)
	}
	for _, ad := range []Advisory{spot, arm} {
		if ad.EstimatedMonthlyUSD <= 0 {
			t.Errorf("%s estimates $%v/mo", ad.Code, ad.EstimatedMonthlyUSD)
		}
		if ad.Caveat == "" {
			t.Errorf("%s has no caveat: its precondition is not observable and must be stated", ad.Code)
		}
	}
	if got := arm.Proposed.Attr(AttrArch); got != string(ArchARM64) {
		t.Errorf("arm advisory proposes arch %q, want ARM64", got)
	}
	if got := spot.Proposed.Attr(AttrMarket); got != string(MarketSpot) {
		t.Errorf("spot advisory proposes market %q, want spot", got)
	}

	// An advisory never becomes a claim and never becomes a step.
	rep := NewSizer(testConfig()).Report(f.snapshot(), testNow, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invalid: %v", err)
	}
	if rep.AdvisoryMonthlyUSD <= 0 {
		t.Fatal("advisory total is not reported separately")
	}
	if rep.ClaimableMonthlyUSD >= rep.AdvisoryMonthlyUSD+rep.ClaimableMonthlyUSD {
		t.Fatal("advisory estimates leaked into the claimable total")
	}
	recs := rep.Recommendations()
	advisories := 0
	for _, r := range recs {
		if err := r.Validate(); err != nil {
			t.Fatalf("recommendation invalid: %v", err)
		}
		if r.Action != domain.ActionAdvisory {
			continue
		}
		advisories++
		if !r.Suppressed {
			t.Errorf("advisory %s is not suppressed", r.SuppressCode)
		}
		if r.ClaimableMonthlyUSD() != 0 {
			t.Errorf("advisory %s claims $%v", r.SuppressCode, r.ClaimableMonthlyUSD())
		}
	}
	if advisories != 2 {
		t.Fatalf("projected %d advisory recommendations, want 2", advisories)
	}
	steps, err := PlanSteps(recs, domain.Guard{Now: testNow})
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	for _, s := range steps {
		if s.Action == domain.ActionAdvisory {
			t.Fatalf("an advisory became step %d", s.Seq)
		}
		if s.To.Attr(AttrArch) == string(ArchARM64) || s.To.Attr(AttrMarket) == string(MarketSpot) {
			t.Fatalf("step %d carries an ECS-only advisory dimension: %v", s.Seq, s.To.Attrs)
		}
	}
	if len(steps) != 1 {
		t.Fatalf("planned %d steps, want exactly the one task-size change", len(steps))
	}
}

// TestARMAdvisoryRespectsItsOwnEligibility: the lever exists on ECS, but not on
// Windows tasks, not on an old platform version, and not for a task that is
// already arm64. Offering it anyway would be trap 3 in the other direction.
func TestARMAdvisoryRespectsItsOwnEligibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*fixture)
	}{
		{"windows", func(f *fixture) { f.osFamily = "WINDOWS_SERVER_2019_CORE" }},
		{"platform-1.3.0", func(f *fixture) { f.platformVersion = "1.3.0" }},
		{"already-arm", func(f *fixture) { f.arch = string(ArchARM64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := assess(t, newFixture(tc.mod))
			if _, ok := advisory(a, AdvisoryARM); ok {
				t.Errorf("offered an ARM advisory for %s", tc.name)
			}
		})
	}
	// Spot has its own eligibility: not on Windows, not twice.
	t.Run("spot-windows", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) { f.osFamily = "WINDOWS_SERVER_2019_CORE" }))
		if _, ok := advisory(a, AdvisorySpot); ok {
			t.Error("offered Fargate Spot for a Windows task")
		}
	})
	t.Run("spot-already", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) {
			f.launchType = ""
			f.strategy = []CapacityProviderItem{{CapacityProvider: CapacityProviderSpot, Weight: 1}}
		}))
		if _, ok := advisory(a, AdvisorySpot); ok {
			t.Error("offered Fargate Spot to a service already on Fargate Spot")
		}
	})
}

// TestDeploymentInProgressRefuses: a converging service is not assessable.
func TestDeploymentInProgressRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*fixture)
	}{
		{"pending-tasks", func(f *fixture) { f.pending = 1 }},
		{"under-desired", func(f *fixture) { f.running = 2 }},
		{"rollout-in-progress", func(f *fixture) {
			f.deployments = []Deployment{{
				ID: "d1", Status: DeploymentPrimary, TaskDefinition: f.tdARN,
				DesiredCount: f.tasks, RunningCount: f.tasks,
				RolloutState: RolloutInProgress, RolloutStateReason: "rolling",
				CreatedAt: testNow.Add(-30 * 24 * time.Hour),
			}}
		}},
		{"two-live-deployments", func(f *fixture) {
			f.deployments = []Deployment{
				{ID: "d1", Status: DeploymentPrimary, TaskDefinition: f.tdARN,
					DesiredCount: f.tasks, RunningCount: f.tasks, RolloutState: RolloutCompleted,
					CreatedAt: testNow.Add(-30 * 24 * time.Hour)},
				{ID: "d0", Status: DeploymentActive, TaskDefinition: f.tdARN,
					DesiredCount: 0, RunningCount: 0,
					CreatedAt: testNow.Add(-31 * 24 * time.Hour)},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := assess(t, newFixture(tc.mod))
			if a.Proposal != nil {
				t.Fatalf("proposed a change mid-deployment: %s", a.Proposal.Tier)
			}
			if !hasSuppression(a, ReasonDeploymentInProgress) {
				t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonDeploymentInProgress)
			}
		})
	}
}

// TestEvidenceGatesRefuse covers the "no metric window" family.
func TestEvidenceGatesRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		mod  func(*fixture)
	}{
		{"empty-window", ReasonNoMetricWindow, func(f *fixture) { f.samples = 0 }},
		{"partial-cpu", ReasonPartialMetrics, func(f *fixture) { f.cpuStatus = StatusPartialData }},
		{"partial-mem", ReasonPartialMetrics, func(f *fixture) { f.memStatus = StatusInternal }},
		{"truncated", ReasonPartialMetrics, func(f *fixture) { f.cpuStatus = StatusTruncated }},
		{"short-window", ReasonInsufficientWindow, func(f *fixture) {
			f.samples, f.period = 200, time.Minute // 3.3 h < 24 h
		}},
		{"too-few-samples", ReasonInsufficientSamples, func(f *fixture) {
			f.samples, f.period = 30, 4*time.Hour // 116 h span, 30 datapoints
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := assess(t, newFixture(tc.mod))
			if a.Proposal != nil {
				t.Fatalf("proposed %s without usable evidence", a.Proposal.Tier)
			}
			if !hasSuppression(a, tc.want) {
				t.Fatalf("suppressions %v, want %s", suppressionCodes(a), tc.want)
			}
			if len(a.Evidence) == 0 {
				t.Error("a refusal still has to state what was observed")
			}
		})
	}
}

// TestRevisionDriftTrimsThenRefuses: percentages published before the running
// revision rolled out are percentages of a DIFFERENT reservation. They are
// dropped; if too few remain, the service is refused rather than sized against
// a denominator that never applied.
func TestRevisionDriftTrimsThenRefuses(t *testing.T) {
	t.Run("refuses-when-little-is-left", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) {
			f.primaryDeployCreatedAt = testNow.Add(-2 * time.Hour)
		}))
		if a.Proposal != nil {
			t.Fatalf("sized against %d datapoints from an older revision", a.Demand.Samples)
		}
		if !hasSuppression(a, ReasonRevisionDrift) {
			t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonRevisionDrift)
		}
		if a.Demand.Trimmed <= 0 {
			t.Error("nothing was trimmed; the drift was not detected")
		}
	})

	t.Run("proceeds-when-enough-remains", func(t *testing.T) {
		f := newFixture(func(f *fixture) {
			f.samples = 600 // ~100 h of history, of which 72 h post-rollout
			f.primaryDeployCreatedAt = testNow.Add(-72 * time.Hour)
		})
		a := assess(t, f)
		if a.Proposal == nil {
			t.Fatalf("refused with %v despite 72 h of post-rollout history", suppressionCodes(a))
		}
		if a.Demand.Trimmed <= 0 {
			t.Fatal("older datapoints were not trimmed")
		}
		var noted bool
		for _, n := range a.Notes {
			if strings.Contains(n, "older revision") {
				noted = true
			}
		}
		if !noted {
			t.Errorf("notes %v do not record the trim", a.Notes)
		}
	})
}

// TestShapeConstraintsRefuse covers awsvpc, platform version and container
// limits — the constraints that block the proposed shape itself.
func TestShapeConstraintsRefuse(t *testing.T) {
	t.Run("network-mode", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) { f.networkMode = "bridge" }))
		if a.Proposal != nil {
			t.Fatal("proposed a revision for a task that cannot run on Fargate")
		}
		if !hasSuppression(a, ReasonNetworkMode) {
			t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonNetworkMode)
		}
	})

	t.Run("platform-version-blocks-large-size", func(t *testing.T) {
		// 90 % utilization on 4 vCPU / 8 GB needs 8 vCPU, which requires
		// platform version 1.4.0 or later.
		a := assess(t, newFixture(func(f *fixture) {
			f.constCPUPct, f.constMem = 90, 90
			f.platformVersion = "1.3.0"
		}))
		if a.Proposal != nil {
			t.Fatalf("proposed %s on platform version 1.3.0", a.Proposal.Tier)
		}
		if !hasSuppression(a, ReasonPlatformVersion) {
			t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonPlatformVersion)
		}
	})

	t.Run("container-limits-block-entirely", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) {
			f.containers = []ContainerDefinition{
				{Name: "app", CPU: 4096, Memory: 8192}, // the whole task
			}
		}))
		if a.Proposal != nil {
			t.Fatalf("proposed %s below the container hard limits", a.Proposal.Tier)
		}
		if !hasSuppression(a, ReasonContainerLimits) {
			t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonContainerLimits)
		}
	})

	t.Run("container-limits-weaken-but-allow", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) {
			f.containers = []ContainerDefinition{{Name: "app", Memory: 6144}} // 6 GB hard limit
		}))
		if a.Proposal == nil {
			t.Fatalf("refused with %v; a 2vCPU/6GB revision is still cheaper", suppressionCodes(a))
		}
		if got := a.Proposal.Tier.String(); got != "2vCPU 6GB" {
			t.Fatalf("proposed %s, want 2vCPU 6GB (held up by the container hard limit)", got)
		}
		var noted bool
		for _, n := range a.Notes {
			if strings.Contains(n, "container-level limits") {
				noted = true
			}
		}
		if !noted {
			t.Errorf("notes %v do not say the container limits held the size up", a.Notes)
		}
	})
}

// TestUndersizedIsReportedNeverProposed: spending money is a human decision.
func TestUndersizedIsReportedNeverProposed(t *testing.T) {
	a := assess(t, newFixture(func(f *fixture) { f.constCPUPct, f.constMem = 90, 90 }))
	if a.Proposal != nil {
		t.Fatalf("proposed a growth to %s", a.Proposal.Tier)
	}
	if !hasSuppression(a, ReasonUndersized) {
		t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonUndersized)
	}
	ad, ok := advisory(a, AdvisoryUndersized)
	if !ok {
		t.Fatal("undersizing was suppressed without being reported")
	}
	if ad.EstimatedMonthlyUSD >= 0 {
		t.Errorf("undersize advisory estimates $%v: growing a task costs money", ad.EstimatedMonthlyUSD)
	}
	if a.ClaimableMonthlyUSD() != 0 {
		t.Errorf("claimed $%v from a growth", a.ClaimableMonthlyUSD())
	}
}

// TestModeTagsAreHonoured: mode=off refuses outright and stays visible;
// mode=recommend keeps the proposal but makes it unclaimable and unplannable.
func TestModeTagsAreHonoured(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		a := assess(t, newFixture(func(f *fixture) {
			f.tags = []Tag{{Key: TagKilterMode, Value: modeOff}}
		}))
		if a.Proposal != nil {
			t.Fatal("proposed a change to a mode=off service")
		}
		if !hasSuppression(a, ReasonModeOff) {
			t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonModeOff)
		}
		if len(a.Evidence) == 0 {
			t.Error("a mode=off service still has to appear in the report with its evidence")
		}
	})

	t.Run("recommend", func(t *testing.T) {
		f := newFixture(func(f *fixture) {
			f.tags = []Tag{{Key: TagKilterMode, Value: modeRecommend}}
		})
		a := assess(t, f)
		if a.Proposal == nil {
			t.Fatalf("mode=recommend must still report the proposal; got %v", suppressionCodes(a))
		}
		if !hasSuppression(a, domain.SuppressModeRecommend) {
			t.Fatalf("suppressions %v, want %s", suppressionCodes(a), domain.SuppressModeRecommend)
		}
		if a.ClaimableMonthlyUSD() != 0 {
			t.Errorf("claimed $%v from a mode=recommend service", a.ClaimableMonthlyUSD())
		}
		rep := NewSizer(testConfig()).Report(f.snapshot(), testNow, nil)
		steps, err := PlanSteps(rep.Recommendations(), domain.Guard{Now: testNow})
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 0 {
			t.Fatalf("planned %d steps for a mode=recommend service", len(steps))
		}
	})
}

// TestNonFargateAndIdleRefuse.
func TestNonFargateAndIdleRefuse(t *testing.T) {
	a := assess(t, newFixture(func(f *fixture) { f.launchType = "EC2"; f.strategy = nil }))
	if !hasSuppression(a, ReasonNotFargate) {
		t.Errorf("suppressions %v, want %s", suppressionCodes(a), ReasonNotFargate)
	}
	a = assess(t, newFixture(func(f *fixture) { f.tasks, f.running = 0, 0 }))
	if !hasSuppression(a, ReasonServiceIdle) {
		t.Errorf("suppressions %v, want %s", suppressionCodes(a), ReasonServiceIdle)
	}
	if a.Proposal != nil {
		t.Error("proposed a change to a service with no tasks")
	}
}

// TestUnreadableTaskSizeRefuses: without a denominator there is no conversion
// and no bill, and guessing one would poison every percentage in the window.
func TestUnreadableTaskSizeRefuses(t *testing.T) {
	a := assess(t, newFixture(func(f *fixture) { f.badSizeStrings = true }))
	if a.Proposal != nil {
		t.Fatal("proposed a change without a readable reservation")
	}
	if !hasSuppression(a, ReasonInvalidTaskSize) {
		t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonInvalidTaskSize)
	}
	if a.CurrentHourlyUSD != 0 {
		t.Errorf("quoted $%v/h for a service whose size could not be read", a.CurrentHourlyUSD)
	}
}

// TestLowConfidenceRefuses.
func TestLowConfidenceRefuses(t *testing.T) {
	// Full history and a full window, but the newest datapoint is an hour old
	// against a two-hour freshness horizon: the freshness term halves the
	// score to 0.50, below the 0.65 floor.
	a := assess(t, newFixture(func(f *fixture) { f.endOffset = time.Hour }))
	if a.Confidence >= testConfig().MinConfidence {
		t.Fatalf("confidence %.2f did not drop below the floor; the fixture proves nothing", a.Confidence)
	}
	if a.Proposal != nil {
		t.Fatal("proposed a rolling deployment below the confidence floor")
	}
	if !hasSuppression(a, ReasonLowConfidence) {
		t.Fatalf("suppressions %v, want %s", suppressionCodes(a), ReasonLowConfidence)
	}
}

// TestCommitmentNettingNeverOverClaims wires a Compute Savings Plan through the
// same waterfall every other domain uses and asserts the one invariant that
// matters: net ≤ gross, and a suppressed assessment claims nothing.
func TestCommitmentNettingNeverOverClaims(t *testing.T) {
	f := newFixture()
	led := domain.NewLedger(&commit.Inventory{SavingsPlans: []commit.SavingsPlan{
		{ID: "sp", Type: commit.SPCompute, CommitmentUSDPerHour: 5},
	}}, commit.Usage{})

	a := NewSizer(testConfig()).Assess(f.observation(), testNow, led)
	if a.Proposal == nil {
		t.Fatalf("no proposal: %v", suppressionCodes(a))
	}
	if a.Proposal.NetMonthlyUSD > a.Proposal.GrossMonthlyUSD {
		t.Fatalf("net $%.2f > gross $%.2f", a.Proposal.NetMonthlyUSD, a.Proposal.GrossMonthlyUSD)
	}
	if len(a.Suppressions) > 0 && a.ClaimableMonthlyUSD() != 0 {
		t.Fatalf("claimed $%v while suppressed as %v", a.ClaimableMonthlyUSD(), suppressionCodes(a))
	}

	// Without a ledger, net equals gross: no known commitment can strand
	// anything.
	b := NewSizer(testConfig()).Assess(f.observation(), testNow, nil)
	if b.Proposal.NetMonthlyUSD != b.Proposal.GrossMonthlyUSD {
		t.Fatalf("with no ledger, net $%.4f != gross $%.4f",
			b.Proposal.NetMonthlyUSD, b.Proposal.GrossMonthlyUSD)
	}
}

// TestDefaultConfigFloors pins the shipped defaults and proves a realistic
// service — fourteen days of ECS's free 1-minute datapoints — clears them.
func TestDefaultConfigFloors(t *testing.T) {
	d := DefaultConfig()
	switch {
	case d.CPUPercentile != 0.95:
		t.Errorf("CPUPercentile = %v", d.CPUPercentile)
	case d.MinWindow != 7*24*time.Hour:
		t.Errorf("MinWindow = %v", d.MinWindow)
	case d.MinSamples != 720:
		t.Errorf("MinSamples = %v", d.MinSamples)
	case !d.SpotAdvisory || !d.ARMAdvisory:
		t.Error("the two ECS-only advisories must ship enabled: they are why this domain exists")
	case d.MemHeadroom < 1 || d.CPUHeadroom < 1:
		t.Error("headroom below 1 would propose less than was observed")
	}

	f := newFixture(func(f *fixture) {
		f.samples, f.period = 20160, time.Minute // 14 days of 1-minute data
	})
	a := NewSizer(DefaultConfig()).Assess(f.observation(), testNow, nil)
	if a.Proposal == nil {
		t.Fatalf("the shipped defaults refuse a fourteen-day window: %v", suppressionCodes(a))
	}
	if a.Confidence < d.MinConfidence {
		t.Fatalf("confidence %.2f below the %.2f floor on full evidence", a.Confidence, d.MinConfidence)
	}
}

// TestGarbageConfigFallsBackToDefaults: a nonsense config must yield the
// conservative default, never a disabled guard.
func TestGarbageConfigFallsBackToDefaults(t *testing.T) {
	got := Config{
		CPUPercentile: -1, CPUHeadroom: 0.1, MemHeadroom: math.NaN(),
		MinWindow: -time.Hour, MinSamples: -5, MinConfidence: 7,
		MaxSampleAge: -time.Hour, MinMoveMonthlyUSD: math.Inf(1),
	}.withDefaults()
	d := DefaultConfig()
	if got.CPUPercentile != d.CPUPercentile || got.CPUHeadroom != d.CPUHeadroom ||
		got.MemHeadroom != d.MemHeadroom || got.MinWindow != d.MinWindow ||
		got.MinSamples != d.MinSamples || got.MinConfidence != d.MinConfidence ||
		got.MaxSampleAge != d.MaxSampleAge || got.MinMoveMonthlyUSD != d.MinMoveMonthlyUSD {
		t.Fatalf("withDefaults left garbage in place: %+v", got)
	}
}

// TestEveryAssessmentStatesAReason is the package's contract, checked over a
// mixed snapshot: proposal or reason, always evidence, never a claim above the
// list price.
func TestEveryAssessmentStatesAReason(t *testing.T) {
	mods := []func(*fixture){
		func(f *fixture) { f.service = "a" },
		func(f *fixture) { f.service = "b"; f.constCPUPct, f.constMem = 90, 90 },
		func(f *fixture) { f.service = "c"; f.pending = 1 },
		func(f *fixture) { f.service = "d"; f.samples = 0 },
		func(f *fixture) { f.service = "e"; f.tags = []Tag{{Key: TagKilterMode, Value: modeOff}} },
		func(f *fixture) { f.service = "f"; f.badSizeStrings = true },
		func(f *fixture) {
			f.service = "g"
			f.cpu, f.memory = "1024", "2048"
			f.constCPUPct, f.constMem = 50, 50
		},
	}
	snap := &Snapshot{Domain: Kind, Scope: testScope, Cluster: testCluster, Timestamp: testNow}
	for _, m := range mods {
		snap.Services = append(snap.Services, newFixture(m).observation())
	}
	rep := NewSizer(testConfig()).Report(snap, testNow, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invalid: %v", err)
	}
	if len(rep.Assessments) != len(mods) {
		t.Fatalf("assessed %d of %d services", len(rep.Assessments), len(mods))
	}
	for _, r := range rep.Recommendations() {
		if err := r.Validate(); err != nil {
			t.Fatalf("recommendation invalid: %v", err)
		}
	}
}

// TestReportWithoutSnapshotIsHonest.
func TestReportWithoutSnapshotIsHonest(t *testing.T) {
	rep := NewSizer(testConfig()).Report(nil, testNow, nil)
	if rep == nil || !rep.Stale || rep.StaleReason == "" {
		t.Fatalf("a nil snapshot must produce a stale report with a reason, got %+v", rep)
	}
	if rep.ClaimableMonthlyUSD != 0 || len(rep.Assessments) != 0 {
		t.Fatal("a report with no snapshot claims something")
	}
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestSeriesPercentileAndTrim covers the two Series operations decisions rest
// on, including that TrimBefore does not alias its input.
func TestSeriesPercentileAndTrim(t *testing.T) {
	base := testNow.Add(-10 * time.Minute)
	s := Series{Metric: "m"}
	for i := range 10 {
		s.Timestamps = append(s.Timestamps, base.Add(time.Duration(i)*time.Minute))
		s.Values = append(s.Values, float64(i+1)) // 1..10
	}
	if got := s.Percentile(1.0); got != 10 {
		t.Errorf("p100 = %v, want 10", got)
	}
	if got := s.Percentile(0.5); got != 5 {
		t.Errorf("p50 = %v, want 5", got)
	}
	if got := s.Max(); got != 10 {
		t.Errorf("max = %v, want 10", got)
	}
	if got := s.Percentile(0); got != 0 {
		t.Errorf("p0 = %v, want the 0 guard", got)
	}
	trimmed := s.TrimBefore(base.Add(5 * time.Minute))
	if trimmed.Len() != 5 || trimmed.Values[0] != 6 {
		t.Fatalf("TrimBefore kept %d datapoints starting at %v", trimmed.Len(), trimmed.Values[0])
	}
	if s.Len() != 10 {
		t.Fatal("TrimBefore mutated its receiver")
	}
	if got := s.Percentile(0.5); got != 5 {
		t.Error("Percentile reordered the series it reports")
	}
}
