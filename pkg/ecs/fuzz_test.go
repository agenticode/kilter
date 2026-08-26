package ecs

import (
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// validTiers is the tier table as a set, so the fuzz target can assert that
// every proposal is a configuration AWS will actually accept.
var validTiers = func() map[pricing.FargateConfig]bool {
	m := map[pricing.FargateConfig]bool{}
	for _, c := range pricing.FargateConfigs() {
		m[c] = true
	}
	return m
}()

// FuzzSizerInvariants drives the whole decision path — task-size parsing, the
// percent-of-reserved conversion, the quantizer reuse, the gates, the
// advisories and the projection — with arbitrary task definitions and arbitrary
// utilization, and asserts the invariants that must hold for EVERY output this
// package can produce:
//
//	proposal or reason      — silence is never an output;
//	proposals are real tiers — and never the tier the service is already on;
//	gross > 0 on a proposal — a rolling deployment for $0 is not proposed;
//	net ≤ gross, both finite — commitments only ever make a change worth less;
//	claim = 0 when suppressed — including every advisory, always;
//	no advisory becomes a step;
//	Report.Validate and domain.Recommendation.Validate both pass.
func FuzzSizerInvariants(f *testing.F) {
	f.Add(4096, 8192, 30.0, 30.0, 4, 300, 0, 0)
	f.Add(1024, 2048, 50.0, 50.0, 1, 300, 1, 0)  // no tier change
	f.Add(256, 512, 99.0, 99.0, 10, 300, 2, 0)   // smallest tier, hot
	f.Add(16384, 122880, 1.0, 1.0, 2, 300, 3, 0) // ceiling, cold
	f.Add(0, 0, 0.0, 0.0, 0, 0, 0, 0)            // nothing at all
	f.Add(-1, -1, -1.0, -1.0, -1, -1, -1, -1)    // negatives everywhere
	f.Add(4096, 8192, math.MaxFloat64, math.MaxFloat64, 3, 200, 4, 12)
	f.Add(4096, 8192, math.NaN(), math.Inf(1), 3, 200, 5, 200)
	f.Add(2048, 4096, 12.5, 87.5, 6, 250, 6, 48)

	f.Fuzz(func(t *testing.T, cpuUnits, memMiB int, cpuPct, memPct float64,
		tasks, samples, variant, driftHours int) {

		tasks = abs(tasks) % 12
		samples = abs(samples) % 320
		variant = abs(variant)
		driftHours = abs(driftHours) % 400

		fx := newFixture(func(fx *fixture) {
			fx.cpu = strconv.Itoa(cpuUnits)
			fx.memory = strconv.Itoa(memMiB)
			fx.tasks, fx.running = tasks, tasks
			fx.samples = samples
			fx.cpuPct = alternating(cpuPct, samples)
			fx.memPct = alternating(memPct, samples)
			if variant&1 != 0 {
				fx.arch = string(ArchARM64)
			}
			if variant&2 != 0 {
				fx.osFamily = "WINDOWS_SERVER_2019_CORE"
			}
			if variant&4 != 0 {
				fx.platformVersion = "1.3.0"
			}
			if variant&8 != 0 {
				fx.tags = []Tag{{Key: TagKilterMode, Value: modeRecommend}}
			}
			if variant&16 != 0 {
				fx.launchType = ""
				fx.strategy = []CapacityProviderItem{{CapacityProvider: CapacityProviderSpot, Weight: 1}}
			}
			if variant&32 != 0 {
				fx.containers = []ContainerDefinition{{Name: "app", CPU: int32(abs(cpuUnits) % 20000),
					Memory: int32(abs(memMiB) % 200000)}}
			}
			if driftHours > 0 {
				fx.primaryDeployCreatedAt = testNow.Add(-time.Duration(driftHours) * time.Hour)
			}
		})

		rep := NewSizer(testConfig()).Report(fx.snapshot(), testNow, nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("report invalid: %v", err)
		}
		if len(rep.Assessments) != 1 {
			t.Fatalf("assessed %d services, want 1", len(rep.Assessments))
		}
		a := rep.Assessments[0]

		if a.Proposal == nil && len(a.Suppressions) == 0 {
			t.Fatal("no proposal and no reason: silence is never an output")
		}
		if !isFinite(a.CurrentHourlyUSD) || a.CurrentHourlyUSD < 0 {
			t.Fatalf("current hourly $%v", a.CurrentHourlyUSD)
		}
		if a.Confidence < 0 || a.Confidence > 1 || math.IsNaN(a.Confidence) {
			t.Fatalf("confidence %v out of range", a.Confidence)
		}
		if !isFinite(rep.ClaimableMonthlyUSD) || rep.ClaimableMonthlyUSD < 0 {
			t.Fatalf("report claims $%v/mo", rep.ClaimableMonthlyUSD)
		}

		if p := a.Proposal; p != nil {
			if !validTiers[p.Tier] {
				t.Fatalf("proposed %+v, which is not a valid Fargate task size", p.Tier)
			}
			if p.Tier == a.CurrentTier {
				t.Fatal("proposed the tier the service is already billed at: that saves exactly $0")
			}
			if !(p.GrossMonthlyUSD > 0) {
				t.Fatalf("proposed a rolling deployment for $%v/mo", p.GrossMonthlyUSD)
			}
			if !isFinite(p.GrossMonthlyUSD) || !isFinite(p.NetMonthlyUSD) {
				t.Fatalf("non-finite savings: gross %v net %v", p.GrossMonthlyUSD, p.NetMonthlyUSD)
			}
			if p.NetMonthlyUSD > p.GrossMonthlyUSD {
				t.Fatalf("net $%v > gross $%v", p.NetMonthlyUSD, p.GrossMonthlyUSD)
			}
			if p.Action != domain.ActionRolling {
				t.Fatalf("action %q: every ECS task-size change is rolling", p.Action)
			}
			// The proposed size must round-trip through the strings
			// RegisterTaskDefinition actually takes.
			cpu, err := ParseTaskCPU(p.Spec.Attr(AttrTaskCPU))
			mem, merr := ParseTaskMemory(p.Spec.Attr(AttrTaskMemory))
			if err != nil || merr != nil ||
				(model.Resources{MilliCPU: cpu, MemoryBytes: mem}) != p.Tier.Resources() {
				t.Fatalf("proposal %s does not round-trip through its task-definition strings (%q/%q)",
					p.Tier, p.Spec.Attr(AttrTaskCPU), p.Spec.Attr(AttrTaskMemory))
			}
		}
		if len(a.Suppressions) > 0 && a.ClaimableMonthlyUSD() != 0 {
			t.Fatalf("claimed $%v while suppressed as %v", a.ClaimableMonthlyUSD(), suppressionCodes(a))
		}
		for _, s := range a.Suppressions {
			if s.Code == "" || s.Reason == "" {
				t.Fatalf("suppression %+v has no code or no reason", s)
			}
		}
		for _, ad := range a.Advisories {
			if ad.Caveat == "" {
				t.Fatalf("advisory %s states no unverifiable precondition", ad.Code)
			}
			if !isFinite(ad.EstimatedMonthlyUSD) {
				t.Fatalf("advisory %s estimates %v", ad.Code, ad.EstimatedMonthlyUSD)
			}
			// §7 trap 3 in the other direction: an ARM advisory must never be
			// offered for a task that is already arm64 or is not Linux.
			if ad.Code == AdvisoryARM &&
				(fx.arch == string(ArchARM64) || fx.osFamily != "LINUX") {
				t.Fatalf("offered an ARM advisory for arch=%s os=%s", fx.arch, fx.osFamily)
			}
			if ad.Code == AdvisorySpot && fx.osFamily != "LINUX" {
				t.Fatal("offered Fargate Spot for a Windows task")
			}
		}

		recs := rep.Recommendations()
		for _, r := range recs {
			if err := r.Validate(); err != nil {
				t.Fatalf("recommendation invalid: %v", err)
			}
			if r.Action == domain.ActionAdvisory && r.ClaimableMonthlyUSD() != 0 {
				t.Fatalf("advisory %s claims $%v", r.SuppressCode, r.ClaimableMonthlyUSD())
			}
		}
		steps, err := PlanSteps(recs, domain.Guard{Now: testNow})
		if err != nil {
			t.Fatalf("PlanSteps: %v", err)
		}
		for _, s := range steps {
			if s.Action != domain.ActionRolling {
				t.Fatalf("step %d has action %q", s.Seq, s.Action)
			}
			if _, err := TierFor(s.To.Resources); err != nil {
				t.Fatalf("step %d targets %s, which is not a task size", s.Seq, s.To.Resources)
			}
			if s.From.Attr(AttrTaskDefinition) == "" {
				t.Fatalf("step %d records no rollback revision", s.Seq)
			}
		}
	})
}

// FuzzRoundUpTier pins the quantizer reuse against arbitrary demand: whatever
// comes back must be a real tier that actually holds the demand, and anything
// past the ceiling must be an error rather than a fabricated bill.
func FuzzRoundUpTier(f *testing.F) {
	f.Add(int64(1000), int64(8<<30))
	f.Add(int64(0), int64(0))
	f.Add(int64(-1), int64(-1))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64))
	f.Add(int64(16000), int64(120<<30))
	f.Add(int64(16001), int64(120<<30))

	f.Fuzz(func(t *testing.T, milliCPU, memoryBytes int64) {
		need := model.Resources{MilliCPU: milliCPU, MemoryBytes: memoryBytes}
		got, err := RoundUpTier(need)
		if err != nil {
			if !errors.Is(err, pricing.ErrFargateTooLarge) {
				t.Fatalf("RoundUpTier(%s) = %v, want ErrFargateTooLarge or nil", need, err)
			}
			// The ceiling is the only legal reason to refuse.
			if milliCPU <= pricing.FargateMaxConfig.MilliCPU &&
				memoryBytes <= pricing.FargateMaxConfig.MemoryBytes() {
				t.Fatalf("RoundUpTier(%s) refused a need that fits the ceiling", need)
			}
			return
		}
		if !validTiers[got] {
			t.Fatalf("RoundUpTier(%s) = %+v, which is not in the tier table", need, got)
		}
		if got.MilliCPU < milliCPU || got.MemoryBytes() < memoryBytes {
			t.Fatalf("RoundUpTier(%s) = %s, which does not hold the demand", need, got)
		}
		// Unlike EKS, ECS adds no overhead: a need that is already a tier must
		// round to itself.
		if validTiers[pricing.FargateConfig{MilliCPU: milliCPU, MemoryMiB: memoryBytes >> 20}] &&
			memoryBytes%(1<<20) == 0 {
			want := pricing.FargateConfig{MilliCPU: milliCPU, MemoryMiB: memoryBytes >> 20}
			if got != want {
				t.Fatalf("RoundUpTier(%s) = %s, want the tier itself (%s): an overhead crept in",
					need, got, want)
			}
		}
	})
}

// alternating builds a series that varies, so percentile and peak are not the
// same number and a bug that confuses them shows up.
func alternating(v float64, n int) []float64 {
	if n <= 0 {
		return []float64{}
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = v
		if i%4 == 0 {
			out[i] = v / 2
		}
	}
	return out
}

func abs(i int) int {
	if i < 0 {
		if i == math.MinInt {
			return 0
		}
		return -i
	}
	return i
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
