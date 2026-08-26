package lambda

import (
	"strings"
	"testing"
	"time"
)

// --- The rule this unit exists for ----------------------------------------

// One measured memory setting cannot support a cost claim, however much data
// there is at that setting. This is asserted as a REFUSAL — a reason code on
// the assessment — not as a caveat string a UI might not render.
func TestSingleMemoryPointIsARefusalNotACaveat(t *testing.T) {
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 1500},
	})
	s := onlySuppression(t, a, ReasonSingleMemoryPoint)

	if a.Proposal != nil {
		t.Fatalf("a single operating point must never produce a proposal")
	}
	// The floor and the risk ARE reported; only the cost effect is withheld.
	if a.Observation.MemoryFloorMB != 512 {
		t.Errorf("memory floor = %d, want 512 (400 MB peak × 1.25, rounded to a 64 MB step)",
			a.Observation.MemoryFloorMB)
	}
	if !strings.Contains(s.Reason, "COST EFFECT IS UNKNOWN") {
		t.Errorf("the refusal must say the cost effect is unknown, got: %s", s.Reason)
	}
	// And the only honest next step is named.
	ad := hasAdvisory(t, a, AdvisoryPowerTuning)
	if ad.ProposedMemoryMB != 512 {
		t.Errorf("power-tuning trial proposes %d MB, want the floor (512)", ad.ProposedMemoryMB)
	}
	if ad.RateDeltaMonthlyUSD != 0 {
		t.Errorf("a trial that has not run cannot carry a dollar figure, got %v", ad.RateDeltaMonthlyUSD)
	}
	// Reported, and refused, is not the same as silent: the current bill is
	// still measured and still shown.
	if !a.CostKnown || a.CurrentMonthlyUSD <= 0 {
		t.Errorf("the CURRENT bill is measured and must be reported even when no change is proposed")
	}
}

// The headline trap: max memory used is tiny, so the naive optimizer drops the
// memory and claims 50 %. Here the smaller setting was actually MEASURED, and
// it costs MORE — CPU scales with memory, so halving memory more than doubled
// the duration. The downsize must be refused and the increase reported.
func TestLoweringMemoryThatRaisesTheBillIsRefusedAndReportedAsAnIncrease(t *testing.T) {
	// 1024 MB: 100 ms billed ⇒ 1.000 GB × 0.100 s = 0.1000 GB-s
	//  512 MB: 250 ms billed ⇒ 0.500 GB × 0.250 s = 0.1250 GB-s  (+25 %)
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 250, n: 700},
	})
	onlySuppression(t, a, ReasonLowerMemoryCostsMore)

	ad := hasAdvisory(t, a, AdvisoryCostIncrease)
	if ad.RateDeltaMonthlyUSD >= 0 {
		t.Errorf("a measured cost increase must carry a NEGATIVE delta, got %v", ad.RateDeltaMonthlyUSD)
	}
	if !strings.Contains(ad.Caveat, "NOT a saving") {
		t.Errorf("the cost-increase advisory must refuse to be read as a saving: %s", ad.Caveat)
	}
	if ad.ProposedMemoryMB != 512 {
		t.Errorf("the advisory should name the measured setting (512), got %d", ad.ProposedMemoryMB)
	}
	// The refusal totals must not smuggle the increase into a savings figure.
	if a.CandidateMemoryMB != 512 {
		t.Errorf("the contemplated candidate should stay visible, got %d", a.CandidateMemoryMB)
	}
}

// The mirror image, and the reason this domain is worth running: RAISING
// memory was measured to be cheaper, because the extra CPU shortened the
// duration by more than the memory multiplied the rate.
func TestRaisingMemoryIsProposedWhenItMeasuredCheaper(t *testing.T) {
	//  512 MB: 400 ms ⇒ 0.500 × 0.400 = 0.2000 GB-s
	// 1024 MB: 150 ms ⇒ 1.000 × 0.150 = 0.1500 GB-s  (−25 %)
	a := one(t, fn(512), testSpan, []point{
		{memoryMB: 512, maxUsedMB: 300, billedMS: 400, n: 700},
		{memoryMB: 1024, maxUsedMB: 300, billedMS: 150, n: 700},
	})
	if a.Proposal == nil {
		t.Fatalf("expected a proposal, got refusals %v", codes(a))
	}
	p := a.Proposal
	if p.MemoryMB != 1024 {
		t.Fatalf("proposed %d MB, want 1024", p.MemoryMB)
	}
	if p.Risk != RiskLow {
		t.Errorf("raising memory raises the OOM margin too; risk = %q, want %q", p.Risk, RiskLow)
	}
	if p.MeasuredSamples != 700 || p.MeasuredBilledMS != 150 {
		t.Errorf("the proposal must carry the measurement taken AT 1024 MB, got %d samples / %v ms",
			p.MeasuredSamples, p.MeasuredBilledMS)
	}
	if p.NetSavingsMonthlyUSD <= 0 || p.NetSavingsMonthlyUSD > p.GrossSavingsMonthlyUSD+1e-9 {
		t.Errorf("net %v / gross %v", p.NetSavingsMonthlyUSD, p.GrossSavingsMonthlyUSD)
	}
	if p.Action != "advisory" {
		t.Errorf("action = %q; this domain never emits anything else", p.Action)
	}
	if !strings.Contains(p.Reason, "MEASURED at both settings") {
		t.Errorf("the reason must say the claim rests on two measurements: %s", p.Reason)
	}
}

// The ordinary good case: a measured smaller setting that is genuinely cheaper.
func TestLoweringMemoryIsProposedWhenItMeasuredCheaper(t *testing.T) {
	// 1024 MB: 100 ms ⇒ 0.1000 GB-s
	//  512 MB: 150 ms ⇒ 0.0750 GB-s  (−25 %)
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	})
	if a.Proposal == nil {
		t.Fatalf("expected a proposal, got refusals %v", codes(a))
	}
	if a.Proposal.MemoryMB != 512 {
		t.Fatalf("proposed %d MB, want 512", a.Proposal.MemoryMB)
	}
	if a.Proposal.Risk != RiskMedium {
		t.Errorf("lowering memory lowers CPU; risk = %q, want %q", a.Proposal.Risk, RiskMedium)
	}
	if a.Proposal.ProposedHourlyUSD >= a.CurrentHourlyUSD {
		t.Errorf("proposed %v/h is not below current %v/h", a.Proposal.ProposedHourlyUSD, a.CurrentHourlyUSD)
	}
}

// --- Memory floor math ----------------------------------------------------

func TestMemoryFloorMath(t *testing.T) {
	cases := []struct {
		maxUsed  int64
		headroom float64
		step     int64
		want     int64
	}{
		{74, 1.25, 64, 128},  // clamped up to the platform minimum
		{400, 1.25, 64, 512}, // 500 → next 64 MB step
		{400, 1.0, 64, 448},  // no headroom, still stepped
		{1000, 1.25, 64, 1280},
		{9000, 1.25, 64, MaxMemoryMB}, // clamped to the platform maximum
		{0, 1.25, 64, 128},            // nothing observed ⇒ the minimum
		{-5, 1.25, 64, 128},           // garbage cannot lower the floor
		{512, 1.25, 1, 640},           // 1 MB steps are legal on the platform
	}
	for _, tc := range cases {
		if got := MemoryFloorMB(tc.maxUsed, tc.headroom, tc.step); got != tc.want {
			t.Errorf("MemoryFloorMB(%d, %v, %d) = %d, want %d",
				tc.maxUsed, tc.headroom, tc.step, got, tc.want)
		}
	}
}

func TestMemoryFloorComesFromTheREPORTFixtures(t *testing.T) {
	// Peak memory is the max across every measured setting, and a measurement
	// taken at a LOWER setting still raises the floor.
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 300, billedMS: 100, n: 700},
		{memoryMB: 768, maxUsedMB: 700, billedMS: 120, n: 700},
	})
	if a.Observation.MaxMemoryUsedMB != 700 {
		t.Fatalf("max memory used = %d, want 700 (the max across settings)", a.Observation.MaxMemoryUsedMB)
	}
	if a.Observation.MemoryFloorMB != 896 {
		t.Errorf("floor = %d, want 896 (700 × 1.25 = 875 → next 64 MB step)", a.Observation.MemoryFloorMB)
	}
	// 768 measured cheaper (0.768×0.12=0.0922 vs 1.024×0.1=0.1) but is below
	// the 896 MB floor, so it is refused rather than recommended.
	onlySuppression(t, a, ReasonBelowMemoryFloor)
}

// --- Init duration is never averaged into warm duration -------------------

func TestInitDurationIsSegregatedFromWarmDuration(t *testing.T) {
	// Every third invocation is cold and pays 400 ms of init. If init leaked
	// into the warm mean it would land near 100 + 133 = 233 ms.
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 900, coldEvery: 5, initMS: 400},
	})
	p, ok := a.Observation.Current()
	if !ok {
		t.Fatalf("no measurement at the current setting")
	}
	if p.MeanBilledMS != 100 {
		t.Errorf("warm mean billed = %v, want exactly 100: init time must not be averaged in", p.MeanBilledMS)
	}
	if p.Cold != 180 || p.Warm != 720 {
		t.Errorf("warm/cold split = %d/%d, want 720/180", p.Warm, p.Cold)
	}
	if p.MeanInitMS != 400 {
		t.Errorf("mean init = %v, want 400 (measured over cold invocations only)", p.MeanInitMS)
	}
	if a.Observation.ColdShare < 0.19 || a.Observation.ColdShare > 0.21 {
		t.Errorf("cold share = %v, want ~0.2", a.Observation.ColdShare)
	}
}

// --- Every suppression, firing alone --------------------------------------

func TestSuppressionModeOffFiresAlone(t *testing.T) {
	a := one(t, fn(1024, withTag(TagKilterMode, "off")), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	})
	onlySuppression(t, a, ReasonModeOff)
	if !a.Excluded() {
		t.Errorf("a mode=off function is excluded from this domain")
	}
	if len(a.Advisories) != 0 {
		t.Errorf("an opted-out function gets no advisories either, got %v", advisoryCodes(a))
	}
}

func TestSuppressionUnknownConfigurationFiresAlone(t *testing.T) {
	a := one(t, fn(0), testSpan, []point{{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700}})
	onlySuppression(t, a, ReasonUnknownConfiguration)
}

func TestSuppressionProvisionedConcurrencyFiresAlone(t *testing.T) {
	a := one(t, fn(1024, withProvisioned(5)), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	})
	s := onlySuppression(t, a, ReasonProvisionedConcurrency)
	if !strings.Contains(s.Reason, "per-GB-hour") {
		t.Errorf("the refusal must name the different billing model: %s", s.Reason)
	}
}

// Provisioned concurrency is caught from the METRIC too, so a collector that
// could not read the configuration still cannot price the wrong bill.
func TestProvisionedConcurrencyIsAlsoDetectedFromTheMetric(t *testing.T) {
	series := Series{
		Metric: MetricProvisionedConcurrentExecutions, Stat: "Maximum", Source: SourceCloudWatch,
		Points: SyntheticMetric(testStart(), time.Hour, 4, 3),
	}
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	}, series)
	onlySuppression(t, a, ReasonProvisionedConcurrency)
}

func TestSuppressionNoReportEvidenceFiresAlone(t *testing.T) {
	a := assessTarget(t, DefaultConfig(), nil, testSpan, target(fn(1024), nil))
	s := onlySuppression(t, a, ReasonNoReportEvidence)
	if !strings.Contains(s.Reason, "nowhere else") {
		t.Errorf("the refusal must explain that max-memory-used exists only in REPORT: %s", s.Reason)
	}
}

// Every REPORT line was malformed: that is a different fact from "the function
// never ran", and the refusal says which one happened.
func TestNoReportEvidenceDistinguishesDroppedFromAbsent(t *testing.T) {
	evs := []LogEvent{
		{Timestamp: testNow, Message: "REPORT RequestId: a\tDuration: ??? ms\tBilled Duration: 1 ms\t" +
			"Memory Size: 128 MB\tMax Memory Used: 1 MB\t"},
		{Timestamp: testNow, Message: "REPORT RequestId: b\tDuration: 1.00 ms\tMemory Size: 128 MB\t"},
	}
	a := assessTarget(t, DefaultConfig(), nil, testSpan, target(fn(1024), evs))
	s := onlySuppression(t, a, ReasonNoReportEvidence)
	if !strings.Contains(s.Reason, "dropped") {
		t.Errorf("dropped lines must be reported, not absorbed: %s", s.Reason)
	}
	if a.Observation.Dropped != 2 {
		t.Errorf("dropped = %d, want 2", a.Observation.Dropped)
	}
}

func TestSuppressionInsufficientWindowFiresAlone(t *testing.T) {
	a := one(t, fn(1024), time.Hour, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	})
	onlySuppression(t, a, ReasonInsufficientWindow)
}

func TestSuppressionInsufficientInvocationsFiresAlone(t *testing.T) {
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 150},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 150},
	})
	s := onlySuppression(t, a, ReasonInsufficientInvocations)
	if !strings.Contains(s.Reason, "300 invocation") {
		t.Errorf("the refusal must state what it saw: %s", s.Reason)
	}
}

func TestSuppressionColdStartDominatedFiresAlone(t *testing.T) {
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700, coldEvery: 2, initMS: 500},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700, coldEvery: 2, initMS: 500},
	})
	s := onlySuppression(t, a, ReasonColdStartDominated)
	if !strings.Contains(s.Reason, "initialization") {
		t.Errorf("the refusal must name init time: %s", s.Reason)
	}
}

func TestSuppressionMemoryAtCeilingFiresAlone(t *testing.T) {
	a := one(t, fn(512), testSpan, []point{
		{memoryMB: 512, maxUsedMB: 512, billedMS: 100, n: 700},
		{memoryMB: 1024, maxUsedMB: 300, billedMS: 100, n: 700},
	})
	s := onlySuppression(t, a, ReasonMemoryAtCeiling)
	if !strings.Contains(s.Reason, "TRUNCATED") {
		t.Errorf("the refusal must name truncation, not fit: %s", s.Reason)
	}
	ad := hasAdvisory(t, a, AdvisoryMemoryTruncated)
	if !strings.Contains(ad.Message, "LOWER BOUND") {
		t.Errorf("the advisory must call the measurement a lower bound: %s", ad.Message)
	}
	if !a.Observation.AtCeiling {
		t.Errorf("observation must record the ceiling")
	}
}

func TestSuppressionNoMeasurementAtCurrentFiresAlone(t *testing.T) {
	// The function runs at 2048 MB now; every measurement is from before the
	// change. The current bill is therefore unmeasured.
	a := one(t, fn(2048), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	})
	s := onlySuppression(t, a, ReasonNoMeasurementAtCurrent)
	if !strings.Contains(s.Reason, "CURRENT bill is unmeasured") {
		t.Errorf("the refusal must say the baseline is missing: %s", s.Reason)
	}
	if a.CostKnown {
		t.Errorf("a function whose current setting was never measured has no known cost")
	}
}

func TestSuppressionNoCheaperMeasurementFiresAlone(t *testing.T) {
	// The function already runs at the cheapest measured setting.
	a := one(t, fn(512), testSpan, []point{
		{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
	})
	onlySuppression(t, a, ReasonNoCheaperMeasurement)
}

func TestSuppressionLowConfidenceFiresAlone(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinConfidence = 0.75
	// A CloudWatch invocation count far above the parsed REPORT count: the log
	// query saw a sliver of the traffic, so the sample is not the population.
	tgt := target(fn(1024), events(testSpan,
		point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700},
		point{memoryMB: 512, maxUsedMB: 400, billedMS: 150, n: 700},
	), invocationSeries(testSpan, 2_000_000))

	a := assessTarget(t, cfg, nil, testSpan, tgt)
	s := onlySuppression(t, a, ReasonLowConfidence)
	if !strings.Contains(s.Reason, "report-coverage") {
		t.Errorf("the refusal must name the weakest factor: %s", s.Reason)
	}
	if a.Confidence.Score >= cfg.MinConfidence {
		t.Errorf("confidence %v should be below the %v gate", a.Confidence.Score, cfg.MinConfidence)
	}
}

// --- Confidence is earned -------------------------------------------------

func TestConfidenceStartsAtZeroAndIsEarned(t *testing.T) {
	s, err := NewSizer(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	empty := s.confidence(Observation{CurrentIndex: -1}, nil)
	if empty.Score != 0 {
		t.Errorf("confidence over no evidence = %v, want 0", empty.Score)
	}
	var total float64
	for _, f := range empty.Factors {
		total += f.Weight
	}
	if total < 0.999 || total > 1.001 {
		t.Errorf("confidence weights sum to %v, want 1", total)
	}
}

// --- ARM advisory ---------------------------------------------------------

func TestARMAdvisoryIsARateDeltaNotASaving(t *testing.T) {
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 1500},
	})
	ad := hasAdvisory(t, a, AdvisoryARM)
	if ad.ProposedArch != ArchARM {
		t.Errorf("advisory arch = %q", ad.ProposedArch)
	}
	if ad.RateDeltaMonthlyUSD <= 0 {
		t.Errorf("the arm64 rate is lower, so the rate delta is positive; got %v", ad.RateDeltaMonthlyUSD)
	}
	for _, want := range []string{"RATE delta at UNCHANGED duration", "not a saving", "Portability"} {
		if !strings.Contains(ad.Caveat, want) {
			t.Errorf("ARM caveat must contain %q, got: %s", want, ad.Caveat)
		}
	}
	if ad.Actuatable() {
		t.Errorf("an advisory is never actuatable")
	}
	// It is counted apart from claimed savings, everywhere.
	if a.Proposal != nil {
		t.Fatalf("this fixture must not produce a proposal")
	}
}

func TestARMAdvisoryStrengthensTheCaveatForContainerImages(t *testing.T) {
	a := one(t, fn(1024, withPackage(PackageImage)), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 1500},
	})
	ad := hasAdvisory(t, a, AdvisoryARM)
	if !strings.Contains(ad.Caveat, "ENTIRE image") {
		t.Errorf("an image-packaged function needs the stronger caveat: %s", ad.Caveat)
	}
}

func TestARMAdvisoryIsAbsentForFunctionsAlreadyOnGraviton(t *testing.T) {
	a := one(t, fn(1024, withArch(ArchARM)), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 1500},
	})
	noAdvisory(t, a, AdvisoryARM)
}

// --- Under-provisioning is reported, never proposed ------------------------

func TestUnderProvisionedMemoryIsAdvisoryOnly(t *testing.T) {
	a := one(t, fn(1024), testSpan, []point{
		{memoryMB: 1024, maxUsedMB: 950, billedMS: 100, n: 1500},
	})
	ad := hasAdvisory(t, a, AdvisoryUnderMemory)
	if ad.ProposedMemoryMB <= 1024 {
		t.Errorf("the floor should exceed the current setting, got %d", ad.ProposedMemoryMB)
	}
	if !strings.Contains(ad.Caveat, "No saving and no cost increase is claimed") {
		t.Errorf("growing memory costs money and this unit does not spend it: %s", ad.Caveat)
	}
	if a.Proposal != nil {
		t.Fatalf("under-provisioning must never become a proposal")
	}
}

// --- Cost model -----------------------------------------------------------

func TestGBSecondCostModel(t *testing.T) {
	r := DefaultRates()
	// AWS's own worked shape: 512 MB for 1 s is 0.5 GB-s.
	if got := GBSeconds(512, 1000); got != 0.5 {
		t.Errorf("GBSeconds(512MB, 1s) = %v, want 0.5", got)
	}
	if got := GBSeconds(1024, 100); got != 0.1 {
		t.Errorf("GBSeconds(1024MB, 100ms) = %v, want 0.1", got)
	}
	want := r.RequestUSD + 0.5*X86GBSecondUSD
	if got := r.InvocationUSD(ArchX86, 512, 1000); got != want {
		t.Errorf("InvocationUSD = %v, want %v", got, want)
	}
	// An unknown architecture is priced as x86: the more expensive of the two,
	// so a typo can never manufacture a discount.
	if r.GBSecond("sparc") != r.GBSecond(ArchX86) {
		t.Errorf("an unknown architecture must not be cheaper than x86")
	}
	if delta := r.ArmRateDelta(); delta < 0.19 || delta > 0.21 {
		t.Errorf("arm rate delta = %v, want ~0.20", delta)
	}
	// Billing granularity: 1 ms minimum, rounded up.
	for _, tc := range []struct{ in, want float64 }{{0, 1}, {0.4, 1}, {1, 1}, {1.2, 2}, {12.34, 13}} {
		if got := BillableMS(tc.in); got != tc.want {
			t.Errorf("BillableMS(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestConfigValidationRejectsNonsense(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"headroom below 1", func(c *Config) { c.MemHeadroom = 0.5 }},
		{"zero step", func(c *Config) { c.MemoryStepMB = 0 }},
		{"ceiling ratio above 1", func(c *Config) { c.CeilingRatio = 1.5 }},
		{"negative window", func(c *Config) { c.MinWindow = -time.Hour }},
		{"zero samples per point", func(c *Config) { c.MinSamplesPerPoint = 0 }},
		{"cold share above 1", func(c *Config) { c.MaxColdShare = 2 }},
		{"confidence above 1", func(c *Config) { c.MinConfidence = 1.5 }},
		{"negative GB-second rate", func(c *Config) { c.Rates.GBSecondUSD = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mut(&cfg)
			if _, err := NewSizer(cfg); err == nil {
				t.Fatalf("expected NewSizer to reject %s", tc.name)
			}
		})
	}
}

func TestNilSnapshotIsNotACrash(t *testing.T) {
	s, err := NewSizer(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	rep := s.Assess(testNow, nil, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("an empty report must still be valid: %v", err)
	}
	if len(rep.Warnings) == 0 {
		t.Errorf("an empty report must say why it is empty")
	}
}
