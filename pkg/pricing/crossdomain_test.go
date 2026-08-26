package pricing_test

// Cross-domain consistency: the same resource, priced through two different
// domains, must agree to the cent.
//
// These tests live here rather than in any one domain because none of the
// domains can see the others — pkg/ebs cannot import pkg/lambda, and neither
// should. This package is the one they all depend on, and its external test
// package is the only place the whole graph is visible at once.
//
// What they are for: the failure mode this unit exists to prevent is not "a
// rate is wrong", which a single-domain test catches. It is "two domains
// quietly disagree" — one table gets a region added, an override honoured, a
// digit fixed, and the other does not, and the engine reports two different
// costs for one resource without anything failing. Every test below is written
// against a disagreement that was possible, not against a happy path.

import (
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/ebs"
	"github.com/agenticode/kilter/pkg/ec2"
	"github.com/agenticode/kilter/pkg/ecs"
	"github.com/agenticode/kilter/pkg/lambda"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// cents rounds to the unit money is reconciled in. "Agree to the cent" is the
// contract; float equality is not, because the two sides reach the number by
// different arithmetic.
func cents(usd float64) int64 { return int64(math.Round(usd * 100)) }

// --- Fargate: one tier table, two domains ----------------------------------

// An x86 on-demand Fargate tier costs the same whether ECS or EKS bills it —
// it is one AWS rate card. Before consolidation the two packages held separate
// ARM constants and shared only the x86 pair by convention; nothing checked
// every tier, so a divergence would have shown up as two different dollar
// figures for one task in one report.
func TestFargateX86TierPriceAgreesBetweenEKSAndECS(t *testing.T) {
	eks := pricing.DefaultFargateRates()
	ecsRates := ecs.DefaultRates()
	tiers := pricing.FargateConfigs()
	if len(tiers) < 70 {
		t.Fatalf("only %d tiers: the table is not the §4.1 table", len(tiers))
	}
	for _, c := range tiers {
		gotEKS := eks.Cost(c)
		gotECS := ecsRates.Cost(c, ecs.PlatformX86OnDemand)
		if math.Abs(gotEKS-gotECS) > commit.Eps {
			t.Fatalf("%s: EKS bills %v/h, ECS bills %v/h — one tier, two prices", c, gotEKS, gotECS)
		}
		if a, b := cents(eks.MonthlyCost(c)), cents(ecsRates.MonthlyCost(c, ecs.PlatformX86OnDemand)); a != b {
			t.Fatalf("%s: monthly EKS %d¢ vs ECS %d¢", c, a, b)
		}
	}
}

// Agreement is asserted at [commit.Eps], never bitwise, and that is not
// pedantry: on arm64 the Go compiler may fuse `v·rate_v + g·rate_g` into a
// single FMA at one call site and not at another, so pkg/pricing's ONE price
// function returns two float64 values one ULP apart depending on who inlined
// it. 1 vCPU / 5 GB is such a tier ($0.06270500000000001 vs $0.062705).
//
// The engine-wide rule that follows: money is compared through Eps or in
// cents. This test states it as a property so a future "tighten this to ==" is
// a deliberate act with a documented reason to overrule.
func TestFargatePricesAgreeToTheCentNotToTheBit(t *testing.T) {
	eks := pricing.DefaultFargateRates()
	ecsRates := ecs.DefaultRates()
	for _, c := range pricing.FargateConfigs() {
		a, b := eks.Cost(c), ecsRates.Cost(c, ecs.PlatformX86OnDemand)
		if math.Abs(a-b) > commit.Eps {
			t.Fatalf("%s: %v vs %v exceeds the money tolerance", c, a, b)
		}
		if cents(a*float64(pricing.HoursPerMonth)) != cents(b*float64(pricing.HoursPerMonth)) {
			t.Fatalf("%s: the two prices round to different cents", c)
		}
	}
}

// The same workload, sized by both domains: the tiers legitimately DIFFER (EKS
// adds 256 MiB for kubelet/kube-proxy/containerd, ECS does not), and that is
// the whole of the difference. Whatever tier each lands on, both price that
// tier identically.
//
// This is the §7 trap 2 / §4.1.1 honesty rule stated across domains: the +59 %
// overhead cliff must be attributable entirely to quantization, never to the
// rate tables having drifted apart.
func TestSameWorkloadPricedByBothDomainsDiffersOnlyByQuantization(t *testing.T) {
	req := model.Resources{MilliCPU: 1000, MemoryBytes: 8 << 30} // AWS's own example

	eksTier, err := pricing.Quantize(req, model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	ecsTier, err := ecs.RoundUpTier(req)
	if err != nil {
		t.Fatal(err)
	}
	if eksTier == ecsTier {
		t.Fatalf("EKS and ECS both sized %s to %s: the +256 MiB Kubernetes overhead has gone missing",
			req, eksTier)
	}
	if want := (pricing.FargateConfig{MilliCPU: 2000, MemoryMiB: 9 * 1024}); eksTier != want {
		t.Errorf("EKS tier = %s, want %s (§4.1.1)", eksTier, want)
	}
	if want := (pricing.FargateConfig{MilliCPU: 1000, MemoryMiB: 8 * 1024}); ecsTier != want {
		t.Errorf("ECS tier = %s, want %s (§4.1)", ecsTier, want)
	}

	eks, ecsRates := pricing.DefaultFargateRates(), ecs.DefaultRates()
	for _, tier := range []pricing.FargateConfig{eksTier, ecsTier} {
		a, b := eks.Cost(tier), ecsRates.Cost(tier, ecs.PlatformX86OnDemand)
		if math.Abs(a-b) > commit.Eps {
			t.Fatalf("%s: EKS %v/h vs ECS %v/h — the difference is supposed to be the TIER, not the rate", tier, a, b)
		}
	}

	// The §4.1.1 arithmetic itself, so a rate edit that preserved
	// cross-domain agreement while moving both sides still fails.
	intended, billed := ecsRates.Cost(ecsTier, ecs.PlatformX86OnDemand), eks.Cost(eksTier)
	if math.Abs(intended-0.07604) > 1e-9 {
		t.Errorf("P(1,8) = %v, want 0.07604/h", intended)
	}
	// §4.1.1 quotes this rounded to $0.12097; the exact product is
	// 2·0.04048 + 9·0.004445 = $0.120965.
	if math.Abs(billed-0.120965) > 1e-9 {
		t.Errorf("P(2,9) = %v, want 0.120965/h ($0.12097 rounded, §4.1.1)", billed)
	}
	if penalty := billed/intended - 1; math.Abs(penalty-0.59) > 0.005 {
		t.Errorf("overhead cliff = %.1f%%, want +59%% (§4.1.1)", penalty*100)
	}
}

// The ARM rates moved into pkg/pricing. That must not have moved ARM onto EKS:
// pkg/pricing exposes them under a type EKS pricing cannot reach, and the
// EKS-facing rate table still has no ARM or Spot dimension to set.
func TestARMFargateRatesAreSingleSourcedWithoutReachingEKS(t *testing.T) {
	arm := pricing.DefaultFargateARMRates()
	if ecs.ARMVCPUHourlyUSD != arm.VCPUHourlyUSD || ecs.ARMGBHourlyUSD != arm.GBHourlyUSD {
		t.Fatalf("ecs ARM rates (%v, %v) != pricing (%v, %v)",
			ecs.ARMVCPUHourlyUSD, ecs.ARMGBHourlyUSD, arm.VCPUHourlyUSD, arm.GBHourlyUSD)
	}
	r := ecs.DefaultRates()
	if r.ArmVCPUHourlyUSD != arm.VCPUHourlyUSD || r.ArmGBHourlyUSD != arm.GBHourlyUSD {
		t.Fatalf("ecs.DefaultRates ARM pair (%v, %v) != pricing (%v, %v)",
			r.ArmVCPUHourlyUSD, r.ArmGBHourlyUSD, arm.VCPUHourlyUSD, arm.GBHourlyUSD)
	}

	// §4.5: −20 % vCPU, −19.9 % GB against x86.
	x86 := pricing.DefaultFargateRates()
	vcpuDelta := (x86.VCPUHourlyUSD - arm.VCPUHourlyUSD) / x86.VCPUHourlyUSD
	gbDelta := (x86.GBHourlyUSD - arm.GBHourlyUSD) / x86.GBHourlyUSD
	if math.Abs(vcpuDelta-0.20) > 0.001 {
		t.Errorf("ARM vCPU delta = %.4f, want ≈0.20 (§4.5)", vcpuDelta)
	}
	if math.Abs(gbDelta-0.199) > 0.001 {
		t.Errorf("ARM GB delta = %.4f, want ≈0.199 (§4.5)", gbDelta)
	}
	if arm.VCPUHourlyUSD >= x86.VCPUHourlyUSD || arm.GBHourlyUSD >= x86.GBHourlyUSD {
		t.Fatal("ARM is not cheaper than x86: the two rate pairs have been swapped")
	}
}

// A Fargate tier is priced by exactly one function, P(v,g) = v·rate_v + g·rate_g,
// and the ECS pricer evaluates pkg/pricing's. Overriding the EKS rate table must
// therefore change what EKS bills and leave ECS's own table alone — the two
// share arithmetic, not state.
func TestFargateRateOverrideDoesNotLeakBetweenDomains(t *testing.T) {
	tier := pricing.FargateConfig{MilliCPU: 1000, MemoryMiB: 2048}
	before := ecs.DefaultRates().Cost(tier, ecs.PlatformX86OnDemand)

	doubled := pricing.FargateRates{
		Platform:      pricing.EKSLinuxX86,
		VCPUHourlyUSD: pricing.FargateVCPUHourlyUSD * 2,
		GBHourlyUSD:   pricing.FargateGBHourlyUSD * 2,
	}
	if got, want := doubled.Cost(tier), pricing.DefaultFargateRates().Cost(tier)*2; math.Abs(got-want) > 1e-12 {
		t.Fatalf("doubled EKS rates priced %v, want %v", got, want)
	}
	if after := ecs.DefaultRates().Cost(tier, ecs.PlatformX86OnDemand); math.Abs(after-before) > commit.Eps {
		t.Fatalf("ECS default price moved from %v to %v because an EKS rate value was constructed", before, after)
	}
}

// --- Lambda ----------------------------------------------------------------

func TestLambdaRatesAreSingleSourced(t *testing.T) {
	p := pricing.DefaultLambdaRates()
	if lambda.RequestUSDPerMillion != p.RequestUSDPerMillion {
		t.Errorf("lambda.RequestUSDPerMillion = %v, pricing = %v", lambda.RequestUSDPerMillion, p.RequestUSDPerMillion)
	}
	if lambda.X86GBSecondUSD != p.X86GBSecondUSD {
		t.Errorf("lambda.X86GBSecondUSD = %v, pricing = %v", lambda.X86GBSecondUSD, p.X86GBSecondUSD)
	}
	if lambda.ARMGBSecondUSD != p.ARMGBSecondUSD {
		t.Errorf("lambda.ARMGBSecondUSD = %v, pricing = %v", lambda.ARMGBSecondUSD, p.ARMGBSecondUSD)
	}
	if int64(lambda.FreeEphemeralStorageMB) != pricing.Global().LambdaFreeEphemeralStorageMB {
		t.Errorf("lambda.FreeEphemeralStorageMB = %v, pricing = %v",
			lambda.FreeEphemeralStorageMB, pricing.Global().LambdaFreeEphemeralStorageMB)
	}

	r := lambda.DefaultRates()
	if r.GBSecondUSD != p.X86GBSecondUSD || r.ArmGBSecondUSD != p.ARMGBSecondUSD {
		t.Fatalf("lambda.DefaultRates() = %+v, pricing baseline = %+v", r, p)
	}
	if r.RequestUSD != pricing.LambdaRequestUSD {
		t.Errorf("lambda.DefaultRates().RequestUSD = %v, want the exact constant %v",
			r.RequestUSD, pricing.LambdaRequestUSD)
	}
	if math.Abs(r.RequestUSD-p.RequestUSD()) > commit.Eps {
		t.Errorf("the exact request charge %v and the table's %v are not one price",
			r.RequestUSD, p.RequestUSD())
	}
	// §4.5/§4.8: the arm64 rate is 20 % below x86 — a RATE ratio, nothing more.
	if d := r.ArmRateDelta(); math.Abs(d-0.20) > 0.001 {
		t.Errorf("ArmRateDelta = %.5f, want ≈0.20", d)
	}
}

// The request charge quoted per million and the request charge quoted per
// request are the same price. They are used in different places — the sizer
// prices an invocation, the commitment waterfall bills a Requests-Millions
// line — and a report that mixed the two conventions would misprice requests
// by a factor of a million.
func TestLambdaRequestChargeIsOnePriceInTwoUnits(t *testing.T) {
	p := pricing.DefaultLambdaRates()
	if got, want := p.RequestUSD()*1e6, p.RequestUSDPerMillion; math.Abs(got-want) > commit.Eps {
		t.Fatalf("RequestUSD()·1e6 = %v, RequestUSDPerMillion = %v", got, want)
	}
	const millionRequests = 1e6
	perInvocation := lambda.DefaultRates().RequestUSD * millionRequests
	perLine := p.RequestUSDPerMillion * (millionRequests / 1e6)
	if math.Abs(perInvocation-perLine) > commit.Eps {
		t.Fatalf("one million requests costs %v via the sizer and %v via the waterfall", perInvocation, perLine)
	}
}

// capturingNetter records the usage lines a domain hands the ledger. It nets
// nothing: it exists so a test can see what the domain claims the bill is made
// of, which is the only exported path to those lines.
type capturingNetter struct{ before, after []commit.UsageLine }

func (c *capturingNetter) Net(before, after []commit.UsageLine) commit.Assessment {
	c.before = append(c.before, before...)
	c.after = append(c.after, after...)
	b := commit.Usage{Lines: before}.OnDemandHourlyUSD()
	a := commit.Usage{Lines: after}.OnDemandHourlyUSD()
	return commit.Assessment{GrossHourlyUSD: b - a, NetHourlyUSD: b - a}
}

// The disagreement this test was written for, and it was a live one.
//
// pkg/lambda's usage lines priced DURATION from the caller's Rates and
// REQUESTS from the package constant. An operator who overrode
// Config.Rates.RequestUSD — the documented override point — got that override
// honoured by Rates.InvocationUSD and silently ignored by the commitment
// waterfall: the same requests, in the same report, at two different prices.
// Nothing failed, because nothing compared them.
func TestLambdaRequestLineHonoursOverriddenRates(t *testing.T) {
	const factor = 3.0
	custom := lambda.DefaultRates()
	custom.RequestUSD *= factor

	lines := lambdaUsageLines(t, custom)
	var requests, duration *commit.UsageLine
	for i := range lines {
		switch lines[i].Unit {
		case "Requests-Millions":
			requests = &lines[i]
		case "GB-Seconds":
			duration = &lines[i]
		}
	}
	if requests == nil || duration == nil {
		t.Fatalf("lambda reported %d usage lines, want a GB-Seconds and a Requests-Millions line", len(lines))
	}

	wantRequestRate := custom.RequestUSD * 1e6
	if math.Abs(requests.ODRate-wantRequestRate) > commit.Eps {
		t.Errorf("requests line ODRate = %v, want %v (the overridden rate). "+
			"A constant here ignores Config.Rates and prices requests twice in one report.",
			requests.ODRate, wantRequestRate)
	}
	if math.Abs(requests.ODRate-pricing.LambdaRequestUSDPerMillion) <= commit.Eps {
		t.Errorf("requests line ODRate is the embedded %v despite a %.0f× override: "+
			"the waterfall is quoting the rate card, not the operator's rates",
			pricing.LambdaRequestUSDPerMillion, factor)
	}
	// The duration line already honoured the override; both must, or neither.
	if math.Abs(duration.ODRate-custom.GBSecond("x86_64")) > commit.Eps {
		t.Errorf("duration line ODRate = %v, want %v", duration.ODRate, custom.GBSecond("x86_64"))
	}
}

// Lowering Lambda memory can RAISE the bill. That is a property of the cost
// model — cost = requests + (MB/1024)·(billedMS/1000)·rate, where billedMS is
// a function of memory that no metric reveals (§4.8) — not of the caller, and
// it must survive the rates moving packages. If it ever stopped holding, the
// sizer's central refusal would become a "saving" it happily recommends.
func TestLambdaCostCurveStaysNonMonotoneInMemory(t *testing.T) {
	r := lambda.DefaultRates()
	const arch = "x86_64"

	// Sub-linear speedup: halving memory more than doubles duration.
	high := r.InvocationUSD(arch, 1024, 100)
	low := r.InvocationUSD(arch, 512, 250)
	if !(low > high) {
		t.Fatalf("512 MB/250 ms costs %v, 1024 MB/100 ms costs %v: "+
			"lowering memory no longer raises the bill and the sizer's core refusal is unfounded", low, high)
	}
	// Perfectly linear speedup: GB-seconds are flat, so only the request
	// charge separates the two — memory is free speed, not a saving.
	flatHigh := r.InvocationUSD(arch, 1024, 100)
	flatLow := r.InvocationUSD(arch, 512, 200)
	if math.Abs(flatHigh-flatLow) > commit.Eps {
		t.Fatalf("linear speedup priced %v at 1024 MB and %v at 512 MB: GB-seconds should be flat", flatHigh, flatLow)
	}
	// And the arm64 RATE delta is not a bill delta at an unknown duration:
	// a 25 % slower arm64 run at a 20 % lower rate costs MORE.
	x86 := r.InvocationUSD("x86_64", 1024, 100)
	arm := r.InvocationUSD("arm64", 1024, 125)
	if !(arm > x86) {
		t.Fatalf("arm64 at 125 ms costs %v vs x86_64 at 100 ms %v: "+
			"the −20 %% rate is being treated as a −20 %% bill", arm, x86)
	}
}

// lambdaUsageLines drives the real Lambda domain far enough to emit commitment
// usage lines, and returns them. It goes through the exported surface only —
// no test helper is borrowed from pkg/lambda — so it also proves the lines are
// reachable by the wiring cmd/ will do.
func lambdaUsageLines(t *testing.T, rates lambda.Rates) []commit.UsageLine {
	t.Helper()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	const span = 48 * time.Hour
	start := now.Add(-span)
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:checkout"
	const scope = "123456789012/us-east-1"

	fn := lambda.Function{
		ARN: arn, Name: "checkout", MemoryMB: 1024, TimeoutSec: 30,
		Architecture: "x86_64", Runtime: "python3.13", PackageType: "Zip",
		LastModified: start,
	}
	// Two measured operating points, both amply sampled: the sizer needs a
	// second point before it will price anything at all.
	var events []lambda.LogEvent
	events = append(events, lambda.SyntheticReports("a", start, span, 1200, 1024, 400, 100, 400, 0)...)
	events = append(events, lambda.SyntheticReports("b", start, span, 1200, 512, 400, 150, 400, 0)...)
	lambda.SortEvents(events)

	tgt := lambda.Target{
		Ref:      domain.TargetRef{Domain: lambda.Kind, Scope: scope, ID: arn, Name: fn.Name},
		Function: fn,
		Series: []lambda.Series{{
			Metric: lambda.MetricInvocations, Stat: "Sum", Source: lambda.SourceCloudWatch,
			Points: lambda.SyntheticMetric(start, time.Hour, int(span/time.Hour), 5000),
		}},
	}
	tgt.Reports, tgt.Drops = lambda.ParseEvents(events)
	targets := []lambda.Target{tgt}
	lambda.SortTargets(targets)

	cfg := lambda.DefaultConfig()
	cfg.Scope, cfg.Region, cfg.Rates = scope, "us-east-1", rates
	dom, err := lambda.NewDomain(cfg)
	if err != nil {
		t.Fatal(err)
	}
	snap := &lambda.Snapshot{
		Domain: lambda.Kind, Scope: scope, Region: "us-east-1", Timestamp: now,
		Window:  lambda.Window{Start: start, End: now},
		Targets: targets,
	}
	if err := dom.Observe(snap); err != nil {
		t.Fatal(err)
	}
	cap := &capturingNetter{}
	if rep := dom.Report(now, cap); rep == nil {
		t.Fatal("nil report")
	}
	if len(cap.before) == 0 {
		t.Fatal("the Lambda domain produced no commitment usage lines: " +
			"this test can no longer see the rates it is checking")
	}
	return cap.before
}

// --- EBS -------------------------------------------------------------------

func TestEBSRatesAreSingleSourced(t *testing.T) {
	p := pricing.DefaultEBSRates()
	r := ebs.DefaultRates()
	for _, c := range []struct {
		name      string
		got, want float64
	}{
		{"gp2GBMonthUSD", r.GP2GBMonthUSD, p.GP2GBMonthUSD},
		{"gp3GBMonthUSD", r.GP3GBMonthUSD, p.GP3GBMonthUSD},
		{"gp3IOPSMonthUSD", r.GP3IOPSMonthUSD, p.GP3IOPSMonthUSD},
		{"gp3ThroughputMonthUSD", r.GP3ThroughputMonthUSD, p.GP3ThroughputMonthUSD},
		{"io1GBMonthUSD", r.IO1GBMonthUSD, p.IO1GBMonthUSD},
		{"io2GBMonthUSD", r.IO2GBMonthUSD, p.IO2GBMonthUSD},
	} {
		if c.got != c.want {
			t.Errorf("ebs %s = %v, pricing = %v", c.name, c.got, c.want)
		}
	}
	// The free allowances deliberately did NOT move: they are what the device
	// delivers, not what it costs. They must still equal the gp3 baseline.
	if int32(r.GP3FreeIOPS) != ebs.GP3BaseIOPS || int32(r.GP3FreeThroughputMBps) != ebs.GP3BaseThroughputMBps {
		t.Errorf("gp3 free allowances (%d, %d) != gp3 device baseline (%d, %d)",
			r.GP3FreeIOPS, r.GP3FreeThroughputMBps, ebs.GP3BaseIOPS, ebs.GP3BaseThroughputMBps)
	}
}

// §4.7's worked examples, priced through pkg/ebs against pkg/pricing's rates.
// A rate that moved packages while changing value passes every same-package
// test and fails here.
func TestEBSWorkedExamplesFromTheDesignDoc(t *testing.T) {
	r := ebs.DefaultRates()

	// 500 GiB gp2 = $50/mo; the parity gp3 (3,000 IOPS / 125 MiB/s, both free)
	// = $40/mo, −20 %.
	if got := cents(r.GP2MonthlyUSD(500)); got != 5000 {
		t.Errorf("500 GiB gp2 = %d¢, want 5000¢", got)
	}
	parity500 := ebs.GP3Config{SizeGiB: 500, IOPS: 3000, ThroughputMBps: 125}
	if got := cents(r.GP3MonthlyUSD(parity500)); got != 4000 {
		t.Errorf("500 GiB parity gp3 = %d¢, want 4000¢", got)
	}

	// 4,000 GiB gp2 delivers 12,000 IOPS and 250 MiB/s. At full parity gp3
	// costs $320 capacity + $45 IOPS + $7.50 throughput = $372.50 (−6.9 %).
	if got := cents(r.GP2MonthlyUSD(4000)); got != 40000 {
		t.Errorf("4000 GiB gp2 = %d¢, want 40000¢", got)
	}
	parity4000 := ebs.GP3Config{SizeGiB: 4000, IOPS: 12000, ThroughputMBps: 250}
	if got := cents(r.GP3MonthlyUSD(parity4000)); got != 37250 {
		t.Errorf("4000 GiB parity gp3 = %d¢, want 37250¢ ($320 + $45 + $7.50)", got)
	}
}

// A gp2 volume and its parity gp3 replacement are the same stored bytes. The
// hourly and monthly views of that bill must be the same money — pkg/ebs
// quotes monthly, the commitment waterfall quotes hourly, and the conversion
// is the one shared HoursPerMonth.
func TestEBSMonthlyAndHourlyViewsAgree(t *testing.T) {
	r := ebs.DefaultRates()
	monthly := r.GP3MonthlyUSD(ebs.GP3Config{SizeGiB: 1000, IOPS: 3000, ThroughputMBps: 125})
	hourly := monthly / float64(ebs.HoursPerMonth)
	if got := cents(hourly * float64(pricing.HoursPerMonth)); got != cents(monthly) {
		t.Fatalf("round-tripping %v/mo through hourly gave %d¢", monthly, got)
	}
}

// --- EC2 burstable surplus -------------------------------------------------

func TestEC2SurplusRateIsSingleSourced(t *testing.T) {
	p := pricing.DefaultEC2CreditRates()
	if ec2.SurplusCreditUSDPerVCPUHour != p.SurplusCreditUSDPerVCPUHour {
		t.Errorf("ec2.SurplusCreditUSDPerVCPUHour = %v, pricing = %v",
			ec2.SurplusCreditUSDPerVCPUHour, p.SurplusCreditUSDPerVCPUHour)
	}
	if math.Abs(ec2.SurplusUSDPerCredit-p.SurplusUSDPerCredit()) > 1e-15 {
		t.Errorf("ec2.SurplusUSDPerCredit = %v, pricing = %v", ec2.SurplusUSDPerCredit, p.SurplusUSDPerCredit())
	}
	if ec2.CreditsPerVCPUHour != pricing.CreditsPerVCPUHour {
		t.Errorf("CreditsPerVCPUHour: ec2=%d pricing=%d", ec2.CreditsPerVCPUHour, pricing.CreditsPerVCPUHour)
	}

	// §4.6/§7 trap 5: the breakeven that makes the burstable mirage
	// quantifiable is a joint fact about the catalog and the surplus rate.
	// t3.large ($0.0832, 2 vCPU, 30 % baseline) vs m5.large ($0.096):
	// 0.0832 + (u−0.3)·2·0.05 = 0.096 ⇒ u ≈ 43 %.
	cat := pricing.Embedded()
	t3, ok := cat.Lookup("aws", "t3.large")
	if !ok {
		t.Fatal("t3.large missing from the catalog")
	}
	m5, ok := cat.Lookup("aws", "m5.large")
	if !ok {
		t.Fatal("m5.large missing from the catalog")
	}
	const baseline, vcpus = 0.30, 2.0
	u := baseline + (m5.HourlyUSD-t3.HourlyUSD)/(vcpus*p.SurplusCreditUSDPerVCPUHour)
	if math.Abs(u-0.43) > 0.005 {
		t.Errorf("t3.large/m5.large breakeven = %.3f sustained CPU, want ≈0.43 (§4.6). "+
			"Either a catalog price or the surplus rate moved without the other.", u)
	}
}

// --- The money convention every domain shares ------------------------------

// One month, five packages. A domain whose HoursPerMonth drifted would report
// monthly figures that cannot be added to any other domain's — and the brain
// adds them.
func TestHoursPerMonthAgreesAcrossEveryDomain(t *testing.T) {
	for _, c := range []struct {
		pkg string
		v   int
	}{
		{"commit", commit.HoursPerMonth},
		{"ebs", ebs.HoursPerMonth},
		{"ec2", ec2.HoursPerMonth},
		{"lambda", lambda.HoursPerMonth},
	} {
		if c.v != pricing.HoursPerMonth {
			t.Errorf("HoursPerMonth: %s=%d pricing=%d", c.pkg, c.v, pricing.HoursPerMonth)
		}
	}
	if pricing.Global().HoursPerMonth != pricing.HoursPerMonth {
		t.Errorf("Global().HoursPerMonth = %d, want %d", pricing.Global().HoursPerMonth, pricing.HoursPerMonth)
	}
}

// The Fargate overhead is a quantization input shared by both Fargate domains
// and by neither's rate table. ECS cancels it out of the EKS quantizer to get
// its own tiers; if the two ever read different constants, an ECS task would
// be sized against a Kubernetes overhead it does not pay.
func TestFargateOverheadIsOneConstantForBothDomains(t *testing.T) {
	if pricing.Global().FargateOverheadBytes != pricing.FargateOverheadBytes {
		t.Fatalf("Global() reports %d overhead bytes, the constant is %d",
			pricing.Global().FargateOverheadBytes, pricing.FargateOverheadBytes)
	}
	// ECS tiers = EKS tiers with the overhead cancelled, at every boundary.
	for _, tier := range pricing.FargateConfigs() {
		need := model.Resources{MilliCPU: tier.MilliCPU, MemoryBytes: tier.MemoryBytes()}
		got, err := ecs.RoundUpTier(need)
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		if got != tier {
			t.Fatalf("ECS sized an exact tier %s to %s", tier, got)
		}
		withOverhead := model.Resources{
			MilliCPU:    tier.MilliCPU,
			MemoryBytes: tier.MemoryBytes() - pricing.FargateOverheadBytes,
		}
		eks, err := pricing.Quantize(withOverhead, model.Resources{})
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		if eks != tier {
			t.Fatalf("EKS sized %s (tier %s minus the overhead) to %s", withOverhead, tier, eks)
		}
	}
}
