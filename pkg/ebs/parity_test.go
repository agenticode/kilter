package ebs

import (
	"math"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// TestHoursPerMonthMatchesPricing pins the money convention across packages: a
// monthly figure computed here must be comparable with one computed by
// pkg/pricing or pkg/pricing/commit.
func TestHoursPerMonthMatchesPricing(t *testing.T) {
	if HoursPerMonth != pricing.HoursPerMonth {
		t.Errorf("HoursPerMonth: ebs=%d pricing=%d", HoursPerMonth, pricing.HoursPerMonth)
	}
	if HoursPerMonth != commit.HoursPerMonth {
		t.Errorf("HoursPerMonth: ebs=%d commit=%d", HoursPerMonth, commit.HoursPerMonth)
	}
}

// TestGP2PerformanceGrid pins the gp2 model across every regime that matters:
// the 100-IOPS floor, the burst band, the 334 GiB throughput step, the 1 TiB
// burst cutoff and the 16,000 IOPS ceiling.
func TestGP2PerformanceGrid(t *testing.T) {
	cases := []struct {
		size      int64
		baseIOPS  int32
		burstIOPS int32
		burstable bool
		baseTput  float64
		burstTput float64
	}{
		{size: 1, baseIOPS: 100, burstIOPS: 3000, burstable: true, baseTput: 25, burstTput: 128},
		{size: 33, baseIOPS: 100, burstIOPS: 3000, burstable: true, baseTput: 25, burstTput: 128},
		{size: 34, baseIOPS: 102, burstIOPS: 3000, burstable: true, baseTput: 25.5, burstTput: 128},
		{size: 100, baseIOPS: 300, burstIOPS: 3000, burstable: true, baseTput: 75, burstTput: 128},
		{size: 166, baseIOPS: 498, burstIOPS: 3000, burstable: true, baseTput: 124.5, burstTput: 128},
		{size: 168, baseIOPS: 504, burstIOPS: 3000, burstable: true, baseTput: 126, burstTput: 128},
		{size: 333, baseIOPS: 999, burstIOPS: 3000, burstable: true, baseTput: 128, burstTput: 128},
		// The throughput ceiling steps from 128 to 250 MiB/s at 334 GiB.
		{size: 334, baseIOPS: 1002, burstIOPS: 3000, burstable: true, baseTput: 250, burstTput: 250},
		{size: 500, baseIOPS: 1500, burstIOPS: 3000, burstable: true, baseTput: 250, burstTput: 250},
		// The last burstable size, and the first one that does not burst.
		{size: 999, baseIOPS: 2997, burstIOPS: 3000, burstable: true, baseTput: 250, burstTput: 250},
		{size: 1000, baseIOPS: 3000, burstIOPS: 3000, burstable: false, baseTput: 250, burstTput: 250},
		{size: 1001, baseIOPS: 3003, burstIOPS: 3003, burstable: false, baseTput: 250, burstTput: 250},
		// The 16,000 IOPS ceiling, reached at 5,334 GiB.
		{size: 5333, baseIOPS: 15999, burstIOPS: 15999, burstable: false, baseTput: 250, burstTput: 250},
		{size: 5334, baseIOPS: 16000, burstIOPS: 16000, burstable: false, baseTput: 250, burstTput: 250},
		{size: 16384, baseIOPS: 16000, burstIOPS: 16000, burstable: false, baseTput: 250, burstTput: 250},
	}
	for _, c := range cases {
		got := GP2PerformanceFor(c.size)
		if got.BaselineIOPS != c.baseIOPS || got.BurstIOPS != c.burstIOPS || got.Burstable != c.burstable {
			t.Errorf("%d GiB: IOPS = base %d burst %d burstable %v, want base %d burst %d burstable %v",
				c.size, got.BaselineIOPS, got.BurstIOPS, got.Burstable, c.baseIOPS, c.burstIOPS, c.burstable)
		}
		if math.Abs(got.BaselineThroughputMBps-c.baseTput) > 1e-9 ||
			math.Abs(got.BurstThroughputMBps-c.burstTput) > 1e-9 {
			t.Errorf("%d GiB: throughput = base %v burst %v, want base %v burst %v",
				c.size, got.BaselineThroughputMBps, got.BurstThroughputMBps, c.baseTput, c.burstTput)
		}
	}
	if p := GP2PerformanceFor(0); p != (GP2Performance{}) {
		t.Errorf("GP2PerformanceFor(0) = %+v, want zero value", p)
	}
	if p := GP2PerformanceFor(-5); p != (GP2Performance{}) {
		t.Errorf("GP2PerformanceFor(-5) = %+v, want zero value", p)
	}
}

// TestDesignDocExamples reproduces every number §4.7 states, by hand, so a
// change to the model that breaks the design document fails here.
func TestDesignDocExamples(t *testing.T) {
	r := DefaultRates()

	// "500 GB gp2 $50/mo → gp3 $40/mo (−20 %)". True only when measurement
	// shows the volume never needed gp2's 250 MiB/s ceiling — which is why
	// this package provisions to measurement and reports the floor separately.
	p, ref := r.PlanGP3(500, Demand{IOPS: 400, ThroughputMBps: 60}, FloorMeasured)
	if ref != nil {
		t.Fatalf("500 GiB measured-parity refused: %v", ref)
	}
	if p.CurrentMonthlyUSD != 50 || p.ProposedMonthlyUSD != 40 || p.DeltaMonthlyUSD != 10 {
		t.Errorf("500 GiB measured: $%.2f → $%.2f (Δ $%.2f), want $50 → $40 (Δ $10)",
			p.CurrentMonthlyUSD, p.ProposedMonthlyUSD, p.DeltaMonthlyUSD)
	}
	if p.Config != (GP3Config{SizeGiB: 500, IOPS: 3000, ThroughputMBps: 125}) {
		t.Errorf("500 GiB measured config = %+v, want the free baseline", p.Config)
	}

	// The same volume at FULL nameplate parity costs $47.50, not $40: gp2
	// delivers 250 MiB/s at this size and gp3 charges $7.50/mo to match it.
	if got := r.NameplateParityDeltaMonthlyUSD(500); math.Abs(got-2.50) > 1e-9 {
		t.Errorf("nameplate Δ(500 GiB) = $%.4f, want $2.50", got)
	}

	// "G = 4000 (12,000 gp2 IOPS, 250 MB/s): $400 → $320 + $45 IOPS + $7.50
	// throughput = $372.50 (−6.9 %) at full parity".
	p, ref = r.PlanGP3(4000, Demand{}, FloorGP2Baseline)
	if ref != nil {
		t.Fatalf("4000 GiB nameplate parity refused: %v", ref)
	}
	if p.Config != (GP3Config{SizeGiB: 4000, IOPS: 12000, ThroughputMBps: 250}) {
		t.Errorf("4000 GiB parity config = %+v, want 12000 IOPS / 250 MiB/s", p.Config)
	}
	if math.Abs(p.ProposedMonthlyUSD-372.50) > 1e-9 || math.Abs(p.CurrentMonthlyUSD-400) > 1e-9 {
		t.Errorf("4000 GiB parity: $%.2f → $%.2f, want $400 → $372.50",
			p.CurrentMonthlyUSD, p.ProposedMonthlyUSD)
	}
	if pct := p.DeltaMonthlyUSD / p.CurrentMonthlyUSD * 100; math.Abs(pct-6.875) > 0.01 {
		t.Errorf("4000 GiB parity saving = %.3f %%, want ≈6.9 %%", pct)
	}

	// "with *measured* p99 IOPS of 4,000 → $320 + $5 + throughput-as-measured
	// ≈ −18 %".
	p, ref = r.PlanGP3(4000, Demand{IOPS: 4000, ThroughputMBps: 100}, FloorMeasured)
	if ref != nil {
		t.Fatalf("4000 GiB measured parity refused: %v", ref)
	}
	if math.Abs(p.ProposedMonthlyUSD-325) > 1e-9 {
		t.Errorf("4000 GiB measured: proposed $%.2f, want $325", p.ProposedMonthlyUSD)
	}
	if pct := p.DeltaMonthlyUSD / p.CurrentMonthlyUSD * 100; math.Abs(pct-18.75) > 0.01 {
		t.Errorf("4000 GiB measured saving = %.3f %%, want ≈18.75 %%", pct)
	}

	// "G ≤ 1000: Δ = 0.02·G" — exactly, whenever measured throughput fits in
	// gp3's free 125 MiB/s.
	for _, g := range []int64{1, 8, 100, 375, 500, 999, 1000} {
		p, ref := r.PlanGP3(g, Demand{IOPS: 50, ThroughputMBps: 10}, FloorMeasured)
		if ref != nil {
			t.Fatalf("%d GiB: refused: %v", g, ref)
		}
		if want := 0.02 * float64(g); math.Abs(p.DeltaMonthlyUSD-want) > 1e-9 {
			t.Errorf("%d GiB: Δ = $%.4f, want $%.4f", g, p.DeltaMonthlyUSD, want)
		}
	}
}

// TestNameplateDeltaMatchesDocFormula evaluates §4.7's Δ formula independently
// and compares it with the model, across the whole size range.
func TestNameplateDeltaMatchesDocFormula(t *testing.T) {
	r := DefaultRates()
	docDelta := func(g float64) float64 {
		// Δ = 0.02·G − max(0, 3G − 3000)·0.005 − max(0, T_gp2(G) − 125)·0.06,
		// with T_gp2 the *delivered baseline* throughput: min(ceiling,
		// baselineIOPS × 256 KiB), ceiling 128 below 334 GiB and 250 at or
		// above it. gp2 IOPS also stop at 16,000.
		iops := math.Min(3*g, GP2MaxIOPS)
		if iops < GP2MinIOPS {
			iops = GP2MinIOPS
		}
		ceiling := 128.0
		if g >= GP2LargeVolumeGiB {
			ceiling = 250
		}
		tput := math.Min(ceiling, iops*BytesPerIO/MiB)
		return 0.02*g - math.Max(0, iops-3000)*0.005 - math.Max(0, tput-125)*0.06
	}
	// §4.7's Δ is continuous; ModifyVolume provisions whole IOPS and whole
	// MiB/s. Kilter rounds provisioning UP, so its saving is the document's
	// minus at most one unit of each — never more than the document claims,
	// which is the direction an honest estimate rounds.
	const roundingSlack = 0.005 + 0.06
	for g := int64(1); g <= 16384; g++ {
		got := r.NameplateParityDeltaMonthlyUSD(g)
		want := docDelta(float64(g))
		if got > want+1e-9 {
			t.Fatalf("%d GiB: nameplate Δ = $%.6f exceeds the doc formula's $%.6f", g, got, want)
		}
		if want-got > roundingSlack+1e-9 {
			t.Fatalf("%d GiB: nameplate Δ = $%.6f, doc formula = $%.6f (gap exceeds integer rounding)",
				g, got, want)
		}
	}
}

// TestParityRefusalBand is the regime where gp3 is NOT cheaper: a volume large
// enough to deliver gp2's 250 MiB/s ceiling but too small for the storage
// saving to pay for it. The band is 334–375 GiB inclusive, and it is refused
// rather than proposed at a loss.
func TestParityRefusalBand(t *testing.T) {
	r := DefaultRates()
	for g := int64(300); g <= 400; g++ {
		p, ref := r.PlanGP3(g, Demand{}, FloorGP2Baseline)
		inBand := g >= 334 && g <= 375
		switch {
		case inBand && ref == nil:
			t.Errorf("%d GiB: accepted with Δ $%.4f, want refusal (gp3 is not cheaper at parity)",
				g, p.DeltaMonthlyUSD)
		case inBand && ref.Code != ReasonNoCheaperConfig:
			t.Errorf("%d GiB: refusal code %q, want %q", g, ref.Code, ReasonNoCheaperConfig)
		case !inBand && ref != nil:
			t.Errorf("%d GiB: refused (%v), want acceptance", g, ref)
		case !inBand && p.DeltaMonthlyUSD <= 0:
			t.Errorf("%d GiB: accepted with non-positive Δ $%.4f", g, p.DeltaMonthlyUSD)
		}
	}
	// The band exists only because of the throughput floor: the same volumes
	// with measurement showing modest throughput convert profitably.
	for _, g := range []int64{334, 350, 375} {
		if _, ref := r.PlanGP3(g, Demand{IOPS: 200, ThroughputMBps: 40}, FloorMeasured); ref != nil {
			t.Errorf("%d GiB measured: refused (%v), want acceptance", g, ref)
		}
	}
}

// TestNaiveMigrationLosesPerformance is §7 trap 6 as a test: on volumes above
// 1 TiB the naive "convert to gp3 at the free baseline" rule silently divides
// sustained IOPS by up to 5.3×. The proposal must never be that configuration,
// the degradation must be reported, and when the naive configuration is all
// that would be cheap enough, the answer is a refusal.
func TestNaiveMigrationLosesPerformance(t *testing.T) {
	r := DefaultRates()

	// A 4 TiB gp2 sustains 12,000 IOPS. Measurement says 9,000.
	p, ref := r.PlanGP3(4000, Demand{IOPS: 9000, ThroughputMBps: 200}, FloorMeasured)
	if ref != nil {
		t.Fatalf("refused: %v", ref)
	}
	if !p.NaiveDegrades {
		t.Fatal("naive gp3 baseline was not reported as degrading a 4 TiB volume")
	}
	if p.NaiveIOPSShortfall != 6000 {
		t.Errorf("naive IOPS shortfall = %v, want 6000", p.NaiveIOPSShortfall)
	}
	if p.NaiveThroughputShortfall != 75 {
		t.Errorf("naive throughput shortfall = %v, want 75", p.NaiveThroughputShortfall)
	}
	if p.Config == p.Naive {
		t.Fatal("proposal is the naive configuration")
	}
	if !p.Config.Clears(p.Required) {
		t.Fatalf("proposal %+v does not clear required %+v", p.Config, p.Required)
	}
	// The naive configuration is cheaper — and that is exactly why it must not
	// be a candidate.
	if !(p.NaiveMonthlyUSD < p.ProposedMonthlyUSD) {
		t.Fatalf("naive $%.2f is not cheaper than the parity proposal $%.2f; "+
			"this test no longer exercises the trap", p.NaiveMonthlyUSD, p.ProposedMonthlyUSD)
	}

	// Same volume, but the whole envelope is needed: 16,000 IOPS at 1,000
	// MiB/s is inside gp3's limits yet costs more than gp2, so it is refused
	// rather than proposed at a loss.
	_, ref = r.PlanGP3(4000, Demand{IOPS: 16000, ThroughputMBps: 1000}, FloorMeasured)
	if ref == nil || ref.Code != ReasonNoCheaperConfig {
		t.Fatalf("high-demand 4 TiB volume: refusal = %v, want %s", ref, ReasonNoCheaperConfig)
	}

	// Demand outside gp3's envelope has no gp3 form at all.
	_, ref = r.PlanGP3(4000, Demand{IOPS: 20000}, FloorMeasured)
	if ref == nil || ref.Code != ReasonExceedsGP3 {
		t.Fatalf("20,000 IOPS: refusal = %v, want %s", ref, ReasonExceedsGP3)
	}
	// The 500:1 ratio bites on small volumes.
	_, ref = r.PlanGP3(8, Demand{IOPS: 5000}, FloorMeasured)
	if ref == nil || ref.Code != ReasonExceedsGP3 {
		t.Fatalf("8 GiB / 5,000 IOPS: refusal = %v, want %s", ref, ReasonExceedsGP3)
	}
	if !strings.Contains(ref.Reason, "4000") {
		t.Errorf("refusal should name the 500:1 limit (4000 IOPS at 8 GiB): %q", ref.Reason)
	}
}

// sizes spans every gp2 regime boundary plus a spread in between.
var gridSizes = []int64{
	1, 2, 8, 33, 34, 50, 100, 166, 167, 170, 200, 333, 334, 335, 350, 375, 376,
	400, 500, 750, 999, 1000, 1001, 1024, 1500, 2000, 3000, 4000, 5000, 5333,
	5334, 6000, 8192, 10000, 12000, 16000, 16384,
}

var gridIOPS = []float64{0, 1, 99, 100, 500, 1499, 2999, 3000, 3001, 4000, 6000, 9000, 12000, 15999, 16000}

var gridTput = []float64{0, 1, 60, 124.9, 125, 125.1, 128, 200, 250, 251}

// TestParityGridNeverDegrades walks the full size × IOPS × throughput grid
// under both floors and asserts the invariants that make this package safe:
//
//  1. an accepted configuration ALWAYS clears the required demand;
//  2. an accepted configuration is always one AWS would accept;
//  3. an accepted configuration is always strictly cheaper than gp2;
//  4. under FloorGP2Baseline it never delivers less than the gp2 volume did;
//  5. the price is exactly the published rate card, recomputed independently.
func TestParityGridNeverDegrades(t *testing.T) {
	r := DefaultRates()
	accepted, refused := 0, 0
	for _, size := range gridSizes {
		gp2 := GP2PerformanceFor(size)
		for _, iops := range gridIOPS {
			for _, tp := range gridTput {
				for _, floor := range []Floor{FloorMeasured, FloorGP2Baseline} {
					d := Demand{IOPS: iops, ThroughputMBps: tp}
					p, ref := r.PlanGP3(size, d, floor)
					if ref != nil {
						refused++
						if ref.Code != ReasonExceedsGP3 && ref.Code != ReasonNoCheaperConfig {
							t.Fatalf("%d GiB %v %v: unexpected refusal %s", size, d, floor, ref)
						}
						continue
					}
					accepted++
					if !p.Config.Clears(d) {
						t.Fatalf("%d GiB %v %v: config %+v does not clear MEASURED demand",
							size, d, floor, p.Config)
					}
					if !p.Config.Clears(p.Required) {
						t.Fatalf("%d GiB %v %v: config %+v does not clear required %+v",
							size, d, floor, p.Config, p.Required)
					}
					if err := p.Config.Validate(); err != nil {
						t.Fatalf("%d GiB %v %v: invalid config %+v: %v", size, d, floor, p.Config, err)
					}
					if p.DeltaMonthlyUSD <= 0 {
						t.Fatalf("%d GiB %v %v: accepted with Δ $%.6f", size, d, floor, p.DeltaMonthlyUSD)
					}
					if floor == FloorGP2Baseline {
						if float64(p.Config.IOPS) < float64(gp2.BaselineIOPS) {
							t.Fatalf("%d GiB %v: floored config %d IOPS < gp2 baseline %d",
								size, d, p.Config.IOPS, gp2.BaselineIOPS)
						}
						if float64(p.Config.ThroughputMBps) < gp2.BaselineThroughputMBps-1e-9 {
							t.Fatalf("%d GiB %v: floored config %d MiB/s < gp2 baseline %v",
								size, d, p.Config.ThroughputMBps, gp2.BaselineThroughputMBps)
						}
					}
					want := 0.08*float64(size) +
						0.005*math.Max(0, float64(p.Config.IOPS)-3000) +
						0.06*math.Max(0, float64(p.Config.ThroughputMBps)-125)
					if math.Abs(p.ProposedMonthlyUSD-want) > 1e-9 {
						t.Fatalf("%d GiB %+v: priced $%.6f, rate card says $%.6f",
							size, p.Config, p.ProposedMonthlyUSD, want)
					}
				}
			}
		}
	}
	if accepted == 0 || refused == 0 {
		t.Fatalf("grid did not exercise both outcomes: %d accepted, %d refused", accepted, refused)
	}
	t.Logf("grid: %d accepted, %d refused", accepted, refused)
}

// TestConfigForRoundsUp pins that fractional demand rounds UP. Rounding a
// fractional IOPS demand down is the silent degradation this unit exists to
// prevent.
func TestConfigForRoundsUp(t *testing.T) {
	r := DefaultRates()
	p, ref := r.PlanGP3(4000, Demand{IOPS: 9000.2, ThroughputMBps: 250.1}, FloorMeasured)
	if ref != nil {
		t.Fatalf("refused: %v", ref)
	}
	if p.Config.IOPS != 9001 {
		t.Errorf("IOPS = %d, want 9001", p.Config.IOPS)
	}
	if p.Config.ThroughputMBps != 251 {
		t.Errorf("throughput = %d, want 251", p.Config.ThroughputMBps)
	}
}

// TestThroughputImpliesIOPS pins the coupling gp3 enforces: throughput is
// capped at 0.25 MiB/s per provisioned IOPS, so a throughput-heavy volume must
// carry the IOPS to back it or the provisioned number is a lie.
func TestThroughputImpliesIOPS(t *testing.T) {
	r := DefaultRates()
	p, ref := r.PlanGP3(8000, Demand{IOPS: 100, ThroughputMBps: 900}, FloorMeasured)
	if ref != nil {
		t.Fatalf("refused: %v", ref)
	}
	if p.Config.IOPS != 3600 {
		t.Errorf("IOPS = %d, want 3600 (4× 900 MiB/s)", p.Config.IOPS)
	}
	if err := p.Config.Validate(); err != nil {
		t.Errorf("config %+v invalid: %v", p.Config, err)
	}
}

func TestGP3Limits(t *testing.T) {
	cases := []struct {
		size     int64
		maxIOPS  int32
		atIOPS   int32
		maxTputs int32
	}{
		{size: 1, maxIOPS: 3000, atIOPS: 3000, maxTputs: 750},
		{size: 4, maxIOPS: 3000, atIOPS: 3000, maxTputs: 750},
		{size: 8, maxIOPS: 4000, atIOPS: 4000, maxTputs: 1000},
		{size: 32, maxIOPS: 16000, atIOPS: 16000, maxTputs: 1000},
		{size: 4000, maxIOPS: 16000, atIOPS: 3000, maxTputs: 750},
	}
	for _, c := range cases {
		if got := GP3MaxIOPSFor(c.size); got != c.maxIOPS {
			t.Errorf("GP3MaxIOPSFor(%d) = %d, want %d", c.size, got, c.maxIOPS)
		}
		if got := GP3MaxThroughputFor(c.atIOPS); got != c.maxTputs {
			t.Errorf("GP3MaxThroughputFor(%d) = %d, want %d", c.atIOPS, got, c.maxTputs)
		}
	}
	if got := GP3MaxIOPSFor(0); got != 0 {
		t.Errorf("GP3MaxIOPSFor(0) = %d, want 0", got)
	}
	bad := []GP3Config{
		{SizeGiB: 0, IOPS: 3000, ThroughputMBps: 125},
		{SizeGiB: 100, IOPS: 2999, ThroughputMBps: 125},
		{SizeGiB: 100, IOPS: 3000, ThroughputMBps: 124},
		{SizeGiB: 8, IOPS: 5000, ThroughputMBps: 125},
		{SizeGiB: 4000, IOPS: 3000, ThroughputMBps: 800},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", c)
		}
	}
	if err := (GP3Config{SizeGiB: 4000, IOPS: 12000, ThroughputMBps: 250}).Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestInvalidInputsRefused(t *testing.T) {
	r := DefaultRates()
	for _, c := range []struct {
		name string
		size int64
		d    Demand
		code string
	}{
		{"zero size", 0, Demand{}, ReasonInvalidSize},
		{"negative size", -1, Demand{}, ReasonInvalidSize},
		{"NaN iops", 100, Demand{IOPS: math.NaN()}, ReasonInvalidDemand},
		{"Inf throughput", 100, Demand{ThroughputMBps: math.Inf(1)}, ReasonInvalidDemand},
		{"negative iops", 100, Demand{IOPS: -1}, ReasonInvalidDemand},
	} {
		_, ref := r.PlanGP3(c.size, c.d, FloorMeasured)
		if ref == nil || ref.Code != c.code {
			t.Errorf("%s: refusal = %v, want %s", c.name, ref, c.code)
		}
	}
	if (*Refusal)(nil).String() != "" {
		t.Error("nil refusal should render empty")
	}
}

func TestRatesLoadAndValidate(t *testing.T) {
	r, err := LoadRates(strings.NewReader(`{"gp2GBMonthUSD":0.12,"gp3GBMonthUSD":0.09}`))
	if err != nil {
		t.Fatalf("LoadRates: %v", err)
	}
	if r.GP2GBMonthUSD != 0.12 || r.GP3GBMonthUSD != 0.09 {
		t.Errorf("override not applied: %+v", r)
	}
	if r.GP3IOPSMonthUSD != DefaultRates().GP3IOPSMonthUSD {
		t.Errorf("unspecified field should keep the embedded default, got %v", r.GP3IOPSMonthUSD)
	}
	for _, bad := range []string{
		`{"gp2GBMonthUS":0.12}`,            // typo: unknown field
		`{"gp2GBMonthUSD":0}`,              // zero rate
		`{"gp3GBMonthUSD":-1}`,             // negative rate
		`{"gp3FreeIOPS":-5}`,               // out of range
		`{"gp3FreeThroughputMBps":100000}`, // out of range
		`{`,                                // malformed
	} {
		if _, err := LoadRates(strings.NewReader(bad)); err == nil {
			t.Errorf("LoadRates(%s) = nil error, want a rejection", bad)
		}
	}
}

// TestNaiveRuleClaimsASavingWhereParityRefuses is the trap stated at its
// sharpest: on a 350 GiB volume the naive rule reports a 20 % saving and
// silently halves sustained throughput, while the parity math refuses the
// conversion outright because gp3 at parity costs MORE.
func TestNaiveRuleClaimsASavingWhereParityRefuses(t *testing.T) {
	r := DefaultRates()
	const size = 350

	p, ref := r.PlanGP3(size, Demand{IOPS: 900, ThroughputMBps: 240}, FloorGP2Baseline)
	if ref == nil {
		t.Fatalf("accepted with Δ $%.4f, want a refusal", p.DeltaMonthlyUSD)
	}
	if ref.Code != ReasonNoCheaperConfig {
		t.Fatalf("refusal code %q, want %q", ref.Code, ReasonNoCheaperConfig)
	}
	// What the naive rule would have reported: cheaper, and short on
	// throughput by half.
	naiveSaving := p.CurrentMonthlyUSD - p.NaiveMonthlyUSD
	if naiveSaving <= 0 {
		t.Fatalf("the naive rule does not claim a saving here ($%.2f); test no longer exercises the trap",
			naiveSaving)
	}
	if !p.NaiveDegrades || p.NaiveThroughputShortfall != 125 {
		t.Fatalf("naive degradation = %v (%.0f MiB/s short), want a 125 MiB/s shortfall",
			p.NaiveDegrades, p.NaiveThroughputShortfall)
	}
	t.Logf("naive rule: $%.2f → $%.2f (claims $%.2f/mo) at %d IOPS / %d MiB/s; "+
		"parity needs %d IOPS / %d MiB/s costing $%.2f — refused",
		p.CurrentMonthlyUSD, p.NaiveMonthlyUSD, naiveSaving, p.Naive.IOPS, p.Naive.ThroughputMBps,
		p.Config.IOPS, p.Config.ThroughputMBps, p.ProposedMonthlyUSD)
}
