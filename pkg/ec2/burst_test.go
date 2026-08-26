package ec2

import (
	"strings"
	"testing"
	"time"
)

// The published T3 table, transcribed independently of the code under test.
// Every earn rate and accrual cap in this package is DERIVED from baseline ×
// vCPUs; this asserts the derivation reproduces AWS's numbers exactly, so a
// wrong baseline cannot hide behind a hand-copied credit rate.
func TestBurstTableMatchesPublishedRates(t *testing.T) {
	published := []struct {
		instanceType   string
		baselinePct    float64
		vcpus          int
		creditsPerHour float64
		maxCredits     float64
	}{
		{"t3.micro", 10, 2, 12, 288},
		{"t3.small", 20, 2, 24, 576},
		{"t3.medium", 20, 2, 24, 576},
		{"t3.large", 30, 2, 36, 864},
		{"t3.xlarge", 40, 4, 96, 2304},
		{"t3.2xlarge", 40, 8, 192, 4608},
	}
	for _, want := range published {
		for _, fam := range []string{"t3", "t3a", "t4g"} {
			name := fam + "." + SizeOf(want.instanceType)
			spec, ok := BurstBaselineFor(name)
			if !ok {
				t.Fatalf("%s: no encoded baseline", name)
			}
			if spec.BaselinePercent() != want.baselinePct || spec.VCPUs != want.vcpus {
				t.Errorf("%s: baseline %.0f%%/%d vCPU, want %.0f%%/%d",
					name, spec.BaselinePercent(), spec.VCPUs, want.baselinePct, want.vcpus)
			}
			if spec.CreditsPerHour() != want.creditsPerHour {
				t.Errorf("%s: derived %.0f credits/h, AWS publishes %.0f",
					name, spec.CreditsPerHour(), want.creditsPerHour)
			}
			if spec.MaxCredits() != want.maxCredits {
				t.Errorf("%s: derived cap %.0f credits, AWS publishes %.0f",
					name, spec.MaxCredits(), want.maxCredits)
			}
		}
	}
	if n := len(BurstSpecs()); n != 18 {
		t.Errorf("BurstSpecs returned %d rows, want 18 (3 families × 6 sizes)", n)
	}
}

// A family we know is credit-based but whose baseline this repo has not
// verified must refuse, not guess. t2 uses a different table and defaults to
// standard mode; sizing it off the t3 baselines would invert decisions.
func TestUnverifiedBurstFamilyIsUnknownNotAssumed(t *testing.T) {
	if !IsBurstable("t2.large") {
		t.Fatal("t2 must be recognized as credit-based")
	}
	if _, ok := BurstBaselineFor("t2.large"); ok {
		t.Fatal("t2 baselines are not verified in this repo and must not be encoded")
	}
	if _, ok := BurstBaselineFor("t3.nano"); ok {
		t.Fatal("t3.nano is not in the verified table and must not be encoded")
	}
	if IsBurstable("m5.large") || IsBurstable("") {
		t.Error("non-burstable types must not be classified as burstable")
	}
}

// The distinction this unit exists to encode: two t3.large instances with the
// SAME low observed CPU. One has credits; one has run out. The first is idle
// and may be downsized. The second is throttled — its low CPU is a ceiling AWS
// imposed — and must not be.
func TestCreditDepletedInstanceIsNotDownsized(t *testing.T) {
	const id = "i-burst"
	cpu := series(id, MetricCPUUtilization, basic, 28, 30, 29, 31) // ≈ the 30 % baseline
	mem := series(id, memAgent, basic, 22, 24, 23)

	healthy := []RecordedSeries{cpu, mem,
		series(id, MetricCPUCreditBalance, basic, 800, 810, 795, 820),
		series(id, MetricCPUSurplusCreditsCharged, basic, 0),
		series(id, MetricCPUSurplusCreditBalance, basic, 0),
		series(id, MetricCPUCreditUsage, basic, 0.4, 0.5),
	}
	depleted := []RecordedSeries{cpu, mem,
		series(id, MetricCPUCreditBalance, basic, 0, 0.2, 0, 0.1), // exhausted
		series(id, MetricCPUSurplusCreditsCharged, basic, 0),
		series(id, MetricCPUSurplusCreditBalance, basic, 0),
		series(id, MetricCPUCreditUsage, basic, 3, 3),
	}

	// Healthy credits: the low CPU is real, and the downsize is made.
	h := single(t, assess(t, collectFor(t,
		[]InstanceRecord{rec(id, "t3.large", standard)}, healthy), nil))
	if h.Observation.Burst.Class != BurstHealthy {
		t.Fatalf("class = %q, want healthy (%s)", h.Observation.Burst.Class, h.Observation.Burst.Reason)
	}
	if h.Proposal == nil || h.Proposal.InstanceType != "t3.medium" {
		t.Fatalf("healthy t3.large should downsize to t3.medium, got %+v / %+v", h.Proposal, h.Suppressions)
	}

	// Depleted credits: identical CPU, opposite verdict.
	d := single(t, assess(t, collectFor(t,
		[]InstanceRecord{rec(id, "t3.large", standard)}, depleted), nil))
	if d.Observation.Burst.Class != BurstThrottled {
		t.Fatalf("class = %q, want throttled (%s)", d.Observation.Burst.Class, d.Observation.Burst.Reason)
	}
	s := only(t, d, ReasonBurstCreditDepleted)
	for _, want := range []string{"ceiling", "baseline", "throttle"} {
		if !strings.Contains(strings.ToLower(s.Reason), want) {
			t.Errorf("refusal does not explain throttling (%q missing):\n%s", want, s.Reason)
		}
	}
	// And the advisory points the other way: more capacity, not less.
	ad, ok := d.AdvisoryFor(AdvisoryBurstThrottle)
	if !ok {
		t.Fatal("a throttled instance must produce an advisory")
	}
	if ad.GrossSavingsMonthlyUSD != 0 || ad.NetSavingsMonthlyUSD != 0 {
		t.Error("a throttled instance's fix is not a saving and must not claim one")
	}
	if !strings.Contains(ad.Message, "not less capacity") {
		t.Errorf("advisory should say the fix is upward:\n%s", ad.Message)
	}
}

// Without credit metrics, a low CPU average on a T instance is not evidence of
// anything — §7 trap 5 requires credit evidence in both directions.
func TestBurstEvidenceMissingFiresAlone(t *testing.T) {
	snap := collectFor(t,
		[]InstanceRecord{rec("i-nocred", "t3.large", unlimited)},
		[]RecordedSeries{
			series("i-nocred", MetricCPUUtilization, basic, 5, 6, 4),
			series("i-nocred", memAgent, basic, 20, 22),
		},
	)
	a := single(t, assess(t, snap, nil))
	s := only(t, a, ReasonBurstEvidenceMissing)
	if !strings.Contains(s.Reason, MetricCPUCreditBalance) {
		t.Errorf("refusal must name the missing metric: %s", s.Reason)
	}
}

// An unknown credit mode inverts the meaning of the same balance, so it is
// also a refusal.
func TestBurstUnknownCreditModeFiresAlone(t *testing.T) {
	const id = "i-nomode"
	snap := collectFor(t,
		[]InstanceRecord{rec(id, "t3.large")}, // no cpuCredits recorded
		[]RecordedSeries{
			series(id, MetricCPUUtilization, basic, 5, 6, 4),
			series(id, MetricCPUCreditBalance, basic, 700, 720),
			series(id, memAgent, basic, 20, 22),
		},
	)
	a := single(t, assess(t, snap, nil))
	s := only(t, a, ReasonBurstEvidenceMissing)
	if !strings.Contains(s.Reason, "credit mode is unknown") {
		t.Errorf("refusal must name the unknown mode: %s", s.Reason)
	}
}

// The sticker-price mirage: unlimited mode paying surplus credits. Realized
// charges are reported as ground truth, and a smaller burstable shape — which
// would lower the baseline and buy MORE surplus — is refused.
func TestBurstSurplusIsPricedAndSmallerBurstableRefused(t *testing.T) {
	const id = "i-surplus"
	snap := collectFor(t,
		[]InstanceRecord{rec(id, "t3.xlarge", unlimited, detailed)},
		[]RecordedSeries{
			series(id, MetricCPUUtilization, detailP, 24, 26, 25, 30),
			series(id, memAgent, detailP, 18, 20, 19),
			series(id, MetricCPUCreditBalance, detailP, 0, 1, 0),
			series(id, MetricCPUSurplusCreditsCharged, detailP, 2, 3, 2),
			series(id, MetricCPUSurplusCreditBalance, detailP, 40, 45),
			series(id, MetricCPUCreditUsage, detailP, 5, 5),
		},
	)
	a := single(t, assess(t, snap, nil))
	b := a.Observation.Burst
	if b.Class != BurstSurplus {
		t.Fatalf("class = %q, want surplus (%s)", b.Class, b.Reason)
	}
	if b.SurplusCreditsCharged <= 0 || b.SurplusHourlyUSD <= 0 {
		t.Fatalf("realized surplus not measured: %+v", b)
	}
	if b.EffectiveHourlyUSD <= b.StickerHourlyUSD {
		t.Errorf("effective %v must exceed sticker %v", b.EffectiveHourlyUSD, b.StickerHourlyUSD)
	}
	if a.EffectiveHourlyUSD != b.EffectiveHourlyUSD {
		t.Error("the assessment must surface the effective cost, not the sticker")
	}
	s := only(t, a, ReasonBurstSurplusCharged)
	if !strings.Contains(s.Reason, "surplus") {
		t.Errorf("refusal must name the surplus: %s", s.Reason)
	}
	ad, ok := a.AdvisoryFor(AdvisoryBurstSurplus)
	if !ok {
		t.Fatal("a surplus-charging instance must produce an advisory")
	}
	if !strings.Contains(ad.Caveat, "NOT netted through the commitment waterfall") {
		t.Errorf("the advisory must refuse to claim un-netted money:\n%s", ad.Caveat)
	}
}

// The credit price identity: $0.05 per vCPU-hour is $0.05/60 per credit.
func TestSurplusCreditPricing(t *testing.T) {
	if got := SurplusUSDPerCredit * 60; !nearly(got, SurplusCreditUSDPerVCPUHour) {
		t.Fatalf("60 credits = %v, want %v per vCPU-hour", got, SurplusCreditUSDPerVCPUHour)
	}
	in := Instance{ID: "i-1", InstanceType: "t3.large", CreditMode: CreditModeUnlimited}
	tgt := Target{Series: []Series{
		{Metric: MetricCPUCreditBalance, Points: []Point{
			{At: testNow.Add(-time.Hour), Value: 0}, {At: testNow, Value: 0}}},
		{Metric: MetricCPUSurplusCreditsCharged, Points: []Point{
			{At: testNow.Add(-time.Hour), Value: 60}, {At: testNow, Value: 60}}},
		{Metric: MetricCPUUtilization, Points: []Point{{At: testNow, Value: 50}}},
	}}
	// 120 credits over a 2-hour window = 60 credits/h = $0.05/h.
	st := AnalyzeBurst(in, tgt, 2, 0.0832, 2*time.Hour)
	if st.Class != BurstSurplus {
		t.Fatalf("class = %q (%s)", st.Class, st.Reason)
	}
	if !nearly(st.SurplusHourlyUSD, 0.05) {
		t.Errorf("surplus = %v/h, want $0.05/h", st.SurplusHourlyUSD)
	}
	if !nearly(st.EffectiveHourlyUSD, 0.0832+0.05) {
		t.Errorf("effective = %v/h, want sticker + surplus", st.EffectiveHourlyUSD)
	}
}

// §4.6's cost(u) model is carried beside the realized charge, never instead of
// it: it is a model, and CPUSurplusCreditsCharged is the invoice.
func TestModeledSurplusIsReportedBesideRealized(t *testing.T) {
	in := Instance{ID: "i-1", InstanceType: "t3.large", CreditMode: CreditModeUnlimited}
	tgt := Target{Series: []Series{
		{Metric: MetricCPUCreditBalance, Points: []Point{{At: testNow, Value: 0}}},
		{Metric: MetricCPUSurplusCreditBalance, Points: []Point{{At: testNow, Value: 5}}},
		{Metric: MetricCPUUtilization, Points: []Point{{At: testNow, Value: 55}}},
	}}
	st := AnalyzeBurst(in, tgt, 2, 0.0832, time.Hour)
	// (0.55 − 0.30) × 2 vCPU × $0.05 = $0.025/h.
	if !nearly(st.ModeledSurplusHourlyUSD, 0.025) {
		t.Errorf("modeled surplus = %v, want $0.025/h", st.ModeledSurplusHourlyUSD)
	}
	if st.SurplusHourlyUSD != 0 {
		t.Errorf("no credits were charged, so realized surplus must be $0, got %v", st.SurplusHourlyUSD)
	}
}

func TestNonBurstableIsNotApplicable(t *testing.T) {
	st := AnalyzeBurst(Instance{InstanceType: "m5.large"}, Target{}, 2, 0.096, time.Hour)
	if st.Class != BurstNotApplicable || !st.Class.Actionable() {
		t.Fatalf("m5.large: class = %q", st.Class)
	}
	if st.EffectiveHourlyUSD != 0.096 {
		t.Errorf("effective cost = %v, want the sticker", st.EffectiveHourlyUSD)
	}
	if BurstThrottled.Actionable() || !BurstThrottled.Throttled() {
		t.Error("a throttled class must be neither actionable nor mistaken for anything else")
	}
}
