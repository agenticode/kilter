package pricing

import (
	"errors"
	"math"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

const mib = int64(1) << 20

func gb(n int64) int64  { return n * 1024 * mib }
func mem(n int64) int64 { return n * mib }

func req(cpu, memBytes int64) model.Resources {
	return model.Resources{MilliCPU: cpu, MemoryBytes: memBytes}
}

func cfg(cpu, memMiB int64) FargateConfig {
	return FargateConfig{MilliCPU: cpu, MemoryMiB: memMiB}
}

// TestFargateConfigTableMatchesSpec pins the §4.1 table literally. The table is
// the whole unit: if a row drifts, every Fargate number kilter reports drifts
// with it, so it is asserted entry by entry rather than by shape.
func TestFargateConfigTableMatchesSpec(t *testing.T) {
	want := []FargateConfig{
		// 0.25 vCPU | 0.5, 1, 2 GB
		cfg(250, 512), cfg(250, 1024), cfg(250, 2048),
		// 0.5 vCPU | 1, 2, 3, 4 GB
		cfg(500, 1024), cfg(500, 2048), cfg(500, 3072), cfg(500, 4096),
		// 1 vCPU | 2–8 GB, 1-GB steps
		cfg(1000, 2048), cfg(1000, 3072), cfg(1000, 4096), cfg(1000, 5120),
		cfg(1000, 6144), cfg(1000, 7168), cfg(1000, 8192),
	}
	// 2 vCPU | 4–16 GB, 1-GB steps
	for g := int64(4); g <= 16; g++ {
		want = append(want, cfg(2000, g*1024))
	}
	// 4 vCPU | 8–30 GB, 1-GB steps
	for g := int64(8); g <= 30; g++ {
		want = append(want, cfg(4000, g*1024))
	}
	// 8 vCPU | 16–60 GB, 4-GB steps
	for g := int64(16); g <= 60; g += 4 {
		want = append(want, cfg(8000, g*1024))
	}
	// 16 vCPU | 32–120 GB, 8-GB steps
	for g := int64(32); g <= 120; g += 8 {
		want = append(want, cfg(16000, g*1024))
	}

	got := FargateConfigs()
	if len(got) != len(want) {
		t.Fatalf("config table has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("config[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if FargateMinConfig != cfg(250, 512) {
		t.Errorf("min config = %v", FargateMinConfig)
	}
	if FargateMaxConfig != cfg(16000, 120*1024) {
		t.Errorf("max config = %v", FargateMaxConfig)
	}
	// Canonical order and uniqueness: rounding walks this slice and takes the
	// first fit, so a mis-sorted or duplicated table silently mis-tiers pods.
	seen := map[FargateConfig]bool{}
	for i, c := range got {
		if seen[c] {
			t.Errorf("duplicate config %v", c)
		}
		seen[c] = true
		if i > 0 {
			prev := got[i-1]
			ordered := prev.MilliCPU < c.MilliCPU ||
				(prev.MilliCPU == c.MilliCPU && prev.MemoryMiB < c.MemoryMiB)
			if !ordered {
				t.Errorf("config table not in canonical order at %d: %v then %v", i, prev, c)
			}
		}
	}
	// FargateConfigs must hand out a copy: a caller mutating it must not be
	// able to corrupt the quantizer for the whole process.
	got[0] = cfg(999999, 999999)
	if FargateConfigs()[0] != cfg(250, 512) {
		t.Fatal("FargateConfigs leaked the internal table")
	}
}

// TestQuantizeTierBoundaries walks every documented boundary with literal,
// hand-computed expectations (§4.1 + §4.1.1 + §4.1.3). Every case is
// "requests → what AWS bills", including the memory-ceiling jumps that force a
// vCPU bump.
func TestQuantizeTierBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		cpu, memB  int64
		want       FargateConfig
		wantTooBig bool
	}{
		// Unspecified requests get the smallest configuration: the 256 MiB
		// overhead alone still fits 0.5 GB.
		{"unspecified", 0, 0, cfg(250, 512), false},
		{"one milli one byte", 1, 1, cfg(250, 512), false},
		{"negative garbage clamps to zero", -5, -1 << 40, cfg(250, 512), false},

		// 0.25 vCPU row: 0.5, 1, 2 GB.
		{"0.25/0.5 exact", 250, mem(256), cfg(250, 512), false},
		{"0.25/0.5 +1 byte", 250, mem(256) + 1, cfg(250, 1024), false},
		{"0.25/1 exact", 250, mem(768), cfg(250, 1024), false},
		{"0.25/1 +1 byte", 250, mem(768) + 1, cfg(250, 2048), false},
		{"0.25/2 exact", 250, mem(1792), cfg(250, 2048), false},
		// 0.25 vCPU tops out at 2 GB: overflow jumps a whole vCPU tier, and
		// 0.5 vCPU has no 2 GB+ε option below 3 GB.
		{"0.25 row overflow", 250, mem(1792) + 1, cfg(500, 3072), false},
		{"cpu 251 leaves the 0.25 row", 251, mem(256), cfg(500, 1024), false},

		// 0.5 vCPU row: 1, 2, 3, 4 GB.
		{"0.5/1 exact", 500, mem(768), cfg(500, 1024), false},
		{"0.5/2 exact", 500, mem(1792), cfg(500, 2048), false},
		{"0.5/3 exact", 500, mem(2816), cfg(500, 3072), false},
		{"0.5/4 exact", 500, mem(3840), cfg(500, 4096), false},
		{"0.5 row overflow", 500, mem(3840) + 1, cfg(1000, 5120), false},
		{"cpu 501 leaves the 0.5 row", 501, 0, cfg(1000, 2048), false},

		// 1 vCPU row: 2–8 GB.
		{"1/2 exact", 1000, mem(1792), cfg(1000, 2048), false},
		{"1/2 +1 byte", 1000, mem(1792) + 1, cfg(1000, 3072), false},
		// §4.1.1, the boundary shave: 7.75 GB requested lands on 1 vCPU / 8 GB.
		{"1/8 exact (7.75GB request)", 1000, mem(7936), cfg(1000, 8192), false},
		// §4.1.1, the cliff: 1 vCPU / 8 GB requested bills as 2 vCPU / 9 GB.
		{"the overhead cliff", 1000, gb(8), cfg(2000, 9216), false},
		{"cpu 1001 leaves the 1 row", 1001, 0, cfg(2000, 4096), false},

		// 2 vCPU row: 4–16 GB.
		{"2/4 exact", 2000, mem(3840), cfg(2000, 4096), false},
		{"2/16 exact", 2000, mem(16128), cfg(2000, 16384), false},
		{"2 row overflow", 2000, mem(16128) + 1, cfg(4000, 17408), false},
		{"cpu 2001 leaves the 2 row", 2001, 0, cfg(4000, 8192), false},

		// 4 vCPU row: 8–30 GB.
		{"4/8 exact", 4000, mem(7936), cfg(4000, 8192), false},
		{"4/30 exact", 4000, mem(30464), cfg(4000, 30720), false},
		// 8 vCPU steps in 4 GB jumps, so 30 GB+ε costs 32 GB.
		{"4 row overflow", 4000, mem(30464) + 1, cfg(8000, 32768), false},
		{"cpu 4001 leaves the 4 row", 4001, 0, cfg(8000, 16384), false},

		// 8 vCPU row: 16–60 GB, 4-GB steps.
		{"8/16 exact", 8000, mem(16128), cfg(8000, 16384), false},
		{"8/16 +1 byte skips to 20", 8000, mem(16128) + 1, cfg(8000, 20480), false},
		{"8/60 exact", 8000, mem(61184), cfg(8000, 61440), false},
		// 16 vCPU steps in 8 GB jumps, so 60 GB+ε costs 64 GB.
		{"8 row overflow", 8000, mem(61184) + 1, cfg(16000, 65536), false},
		{"cpu 8001 leaves the 8 row", 8001, 0, cfg(16000, 32768), false},

		// 16 vCPU row: 32–120 GB, 8-GB steps — and the ceiling.
		{"16/32 exact", 16000, mem(32512), cfg(16000, 32768), false},
		{"16/32 +1 byte skips to 40", 16000, mem(32512) + 1, cfg(16000, 40960), false},
		{"16/120 exact (the ceiling)", 16000, mem(122624), cfg(16000, 122880), false},
		{"one byte over the ceiling", 16000, mem(122624) + 1, FargateConfig{}, true},
		{"one milli over the ceiling", 16001, 0, FargateConfig{}, true},
		{"absurd cpu", math.MaxInt64, 0, FargateConfig{}, true},
		// Saturating memory add: MaxInt64 + overhead must not wrap into "fits".
		{"absurd memory", 0, math.MaxInt64, FargateConfig{}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Quantize(req(tc.cpu, tc.memB), model.Resources{})
			if tc.wantTooBig {
				if !errors.Is(err, ErrFargateTooLarge) {
					t.Fatalf("want ErrFargateTooLarge, got config %v err %v", got, err)
				}
				if got != (FargateConfig{}) {
					t.Fatalf("rejected request must return the zero config, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Quantize(%dm, %dMiB) = %v (%s), want %v (%s)",
					tc.cpu, tc.memB/mib, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestQuantizeEveryTierIsReachableAndTight sweeps the whole table: for every
// valid configuration, the largest request that lands on it must land on it
// exactly, and one byte more must land strictly higher. That covers every tier
// boundary in both directions without hand-writing 74 cases.
func TestQuantizeEveryTierIsReachableAndTight(t *testing.T) {
	all := FargateConfigs()
	for _, c := range all {
		largest := req(c.MilliCPU, c.MemoryBytes()-FargateOverheadBytes)
		got, err := Quantize(largest, model.Resources{})
		if err != nil {
			t.Fatalf("%v: largest fitting request errored: %v", c, err)
		}
		if got != c {
			t.Fatalf("%v: largest fitting request quantized to %v", c, got)
		}

		over := req(c.MilliCPU, c.MemoryBytes()-FargateOverheadBytes+1)
		next, err := Quantize(over, model.Resources{})
		if c == FargateMaxConfig {
			if !errors.Is(err, ErrFargateTooLarge) {
				t.Fatalf("one byte past the ceiling must be rejected, got %v %v", next, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v +1 byte: %v", c, err)
		}
		if next == c {
			t.Fatalf("%v: one byte over the tier stayed on the same tier", c)
		}
		if next.MilliCPU < c.MilliCPU || next.MemoryBytes() <= c.MemoryBytes() {
			t.Fatalf("%v +1 byte → %v: must be strictly larger in memory and never smaller in cpu", c, next)
		}
	}
}

// TestQuantizeInitContainerMaxRule: init containers run to completion one at a
// time, so they never stack — the pod is sized by the per-dimension maximum of
// (sum of long-running containers, largest init container).
func TestQuantizeInitContainerMaxRule(t *testing.T) {
	cases := []struct {
		name           string
		run, initMax   model.Resources
		want           FargateConfig
		wantEffective  model.Resources
		wantSameAsPod  bool
		podInitRequest model.Resources
	}{
		{
			name: "init dominates both dimensions",
			run:  req(100, mem(128)), initMax: req(2000, gb(4)),
			want: cfg(2000, 5120), wantEffective: req(2000, gb(4)),
		},
		{
			name: "run dominates both dimensions",
			run:  req(2000, gb(4)), initMax: req(100, mem(128)),
			want: cfg(2000, 5120), wantEffective: req(2000, gb(4)),
		},
		{
			// The rule is per dimension, not "whichever total is bigger".
			name: "max is taken per dimension",
			run:  req(2000, gb(8)), initMax: req(4000, gb(1)),
			want: cfg(4000, 9216), wantEffective: req(4000, gb(8)),
		},
		{
			name: "init only",
			run:  model.Resources{}, initMax: req(500, gb(1)),
			want: cfg(500, 2048), wantEffective: req(500, gb(1)),
		},
		{
			// Init requests must never be *added* to the running requests:
			// that would over-provision every pod with an init container.
			name: "init is not additive",
			run:  req(1000, gb(4)), initMax: req(1000, gb(4)),
			want: cfg(1000, 5120), wantEffective: req(1000, gb(4)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Quantize(tc.run, tc.initMax)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Quantize(run=%s, init=%s) = %v, want %v", tc.run, tc.initMax, got, tc.want)
			}
			// The same rule reached through the pod helper.
			p := &model.PodSpec{
				Containers:   []model.ContainerSpec{{Name: "app", Requests: tc.run}},
				InitRequests: tc.initMax,
			}
			if eff := FargateEffectiveRequests(p); eff != tc.wantEffective {
				t.Fatalf("FargateEffectiveRequests = %s, want %s", eff, tc.wantEffective)
			}
			viaPod, err := QuantizePod(p)
			if err != nil || viaPod != tc.want {
				t.Fatalf("QuantizePod = %v, %v; want %v", viaPod, err, tc.want)
			}
		})
	}
}

// TestQuantizeSumsLongRunningContainers: long-running containers do stack.
func TestQuantizeSumsLongRunningContainers(t *testing.T) {
	p := &model.PodSpec{Containers: []model.ContainerSpec{
		{Name: "app", Requests: req(500, gb(1))},
		{Name: "sidecar", Requests: req(500, gb(1))},
	}}
	// 1 vCPU / 2 GB summed, +256 MiB → 1 vCPU / 3 GB.
	got, err := QuantizePod(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := cfg(1000, 3072); got != want {
		t.Fatalf("QuantizePod = %v, want %v", got, want)
	}
}

// TestQuantizeCeilingRejectionIsExplicit: a pod above the ceiling cannot run on
// Fargate, and the error says so with the numbers.
func TestQuantizeCeilingRejectionIsExplicit(t *testing.T) {
	_, err := Quantize(req(32000, gb(200)), model.Resources{})
	if !errors.Is(err, ErrFargateTooLarge) {
		t.Fatalf("err = %v, want ErrFargateTooLarge", err)
	}
	// The memory ceiling alone is enough to reject, even at a valid vCPU tier.
	if _, err := Quantize(req(16000, gb(121)), model.Resources{}); !errors.Is(err, ErrFargateTooLarge) {
		t.Fatalf("121 GB at 16 vCPU must be rejected, got %v", err)
	}
	// …and so is the vCPU ceiling alone at a trivial memory request.
	if _, err := Quantize(req(16001, mem(1)), model.Resources{}); !errors.Is(err, ErrFargateTooLarge) {
		t.Fatalf("16.001 vCPU must be rejected, got %v", err)
	}
	// The ceiling is reachable, so rejection is a real boundary, not a bug.
	if _, err := Quantize(req(16000, gb(120)-FargateOverheadBytes), model.Resources{}); err != nil {
		t.Fatalf("the ceiling configuration itself must be reachable: %v", err)
	}
}

// TestSmallPodWorkedExample reproduces §4.1.3 to the cent-fraction: 200 m CPU /
// 512 Mi requested → 0.25 vCPU / 1 GB → $0.014565/h ($10.6/mo). Memory
// quantization skips 0.75 GB because the 0.25 vCPU row has no such option.
func TestSmallPodWorkedExample(t *testing.T) {
	got, err := Quantize(req(200, mem(512)), model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	want := cfg(250, 1024)
	if got != want {
		t.Fatalf("config = %v, want %v", got, want)
	}
	if got.String() != "0.25vCPU 1GB" {
		t.Fatalf("String() = %q, want the AWS annotation form", got.String())
	}

	rates := DefaultFargateRates()
	const wantHourly = 0.014565 // 0.25×0.04048 + 1×0.004445
	if diff := rates.Cost(got) - wantHourly; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("hourly = %.9f, want %.9f", rates.Cost(got), wantHourly)
	}
	const wantMonthly = wantHourly * HoursPerMonth // $10.63
	if diff := rates.MonthlyCost(got) - wantMonthly; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("monthly = %.6f, want %.6f", rates.MonthlyCost(got), wantMonthly)
	}
	// §4.3: six such pods are the sparse-cluster case, ~$64/mo.
	if six := 6 * rates.MonthlyCost(got); six < 63 || six > 65 {
		t.Fatalf("six small pods = $%.2f/mo, want ≈$64", six)
	}
}

// TestOverheadCliffWorkedExample reproduces §4.1.1: the +256 MiB overhead pushes
// a 1 vCPU / 8 GB pod onto 2 vCPU / 9 GB and bills +59 %. This is the entire
// reason this unit exists — pricing that pod by its requests understates the
// bill by a third.
func TestOverheadCliffWorkedExample(t *testing.T) {
	rates := DefaultFargateRates()

	intended := rates.Cost(cfg(1000, 8192)) // what a naive optimizer reports
	billed, err := Quantize(req(1000, gb(8)), model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	if want := cfg(2000, 9216); billed != want {
		t.Fatalf("billed config = %v, want %v", billed, want)
	}

	const wantIntended = 0.07604   // P(1,8)
	const wantBilled = 0.120965    // P(2,9)
	const wantPenalty = 0.59       // +59 %
	const wantShaveSaving = 0.3713 // dropping to 1 vCPU / 8 GB
	if diff := intended - wantIntended; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("intended hourly = %.9f, want %.9f", intended, wantIntended)
	}
	billedUSD := rates.Cost(billed)
	if diff := billedUSD - wantBilled; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("billed hourly = %.9f, want %.9f", billedUSD, wantBilled)
	}
	if penalty := billedUSD/intended - 1; math.Abs(penalty-wantPenalty) > 0.005 {
		t.Fatalf("overhead penalty = %.4f, want ≈%.2f", penalty, wantPenalty)
	}

	// The boundary shave: requesting 7.75 GB keeps the pod on 1 vCPU / 8 GB.
	shaved, err := Quantize(req(1000, mem(7936)), model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	if want := cfg(1000, 8192); shaved != want {
		t.Fatalf("shaved config = %v, want %v", shaved, want)
	}
	saving := 1 - rates.Cost(shaved)/billedUSD
	if math.Abs(saving-wantShaveSaving) > 0.005 {
		t.Fatalf("boundary shave saves %.4f, want ≈%.2f", saving, wantShaveSaving)
	}
}

// TestRequestChangeWithinTierSavesExactlyZero pins §7 trap 2's sharpest edge:
// a request reduction that does not cross a tier boundary changes the bill by
// exactly nothing. Recommending it is the single most common wrong Fargate
// recommendation, and the assertion here is bit-exact equality, not a
// tolerance — "$0.0001/mo" is still a wrong recommendation.
func TestRequestChangeWithinTierSavesExactlyZero(t *testing.T) {
	rates := DefaultFargateRates()
	cases := []struct {
		name                 string
		beforeCPU, beforeMem int64
		afterCPU, afterMem   int64
	}{
		{"trim cpu within the 0.5 row", 500, gb(1), 300, gb(1)},
		{"trim memory within the tier", 500, mem(1536), 500, mem(1200)},
		{"trim both", 500, gb(1), 260, mem(800)},
		{"halve a request that was already at the floor", 100, mem(100), 50, mem(50)},
		{"shave 1 GB off a 4 vCPU pod that stays on 17 GB", 4000, mem(16385), 4000, mem(16200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := Quantize(req(tc.beforeCPU, tc.beforeMem), model.Resources{})
			if err != nil {
				t.Fatal(err)
			}
			after, err := Quantize(req(tc.afterCPU, tc.afterMem), model.Resources{})
			if err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatalf("case crosses a tier boundary (%v → %v); it no longer tests what it claims", before, after)
			}
			if saving := rates.Cost(before) - rates.Cost(after); saving != 0 {
				t.Fatalf("within-tier request change claimed $%v/h of savings; must be exactly 0", saving)
			}
		})
	}
}

// TestQuantizeRoundUpIsAlsoCheapest checks the claim that lets quantization stay
// rate-independent: the smallest valid configuration that fits is also the
// cheapest one under the real rates. If AWS ever adds a row where that fails,
// this fails loudly instead of quietly overcharging.
func TestQuantizeRoundUpIsAlsoCheapest(t *testing.T) {
	rates := DefaultFargateRates()
	for _, cpu := range []int64{0, 1, 100, 250, 251, 500, 700, 1000, 1500, 2000, 3000, 4000, 6000, 8000, 12000, 16000} {
		for _, memMiB := range []int64{0, 1, 300, 512, 900, 1024, 2048, 3000, 4096, 8192, 9000, 16384, 30000, 61440, 100000, 122624} {
			got, err := Quantize(req(cpu, memMiB*mib), model.Resources{})
			if err != nil {
				continue
			}
			need := req(cpu, memMiB*mib+FargateOverheadBytes)
			bestCost := math.Inf(1)
			var best FargateConfig
			for _, c := range FargateConfigs() {
				if c.MilliCPU < need.MilliCPU || c.MemoryBytes() < need.MemoryBytes {
					continue
				}
				if cost := rates.Cost(c); cost < bestCost {
					bestCost, best = cost, c
				}
			}
			if got != best {
				t.Fatalf("(%dm, %dMiB): round-up chose %v ($%.6f), cheapest fitting is %v ($%.6f)",
					cpu, memMiB, got, rates.Cost(got), best, bestCost)
			}
		}
	}
}

// TestQuantizeIsRateIndependent: overriding the rate table reprices a tier, it
// never moves a pod to a different one.
func TestQuantizeIsRateIndependent(t *testing.T) {
	before, err := Quantize(req(1000, gb(8)), model.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	// An absurd rate table where memory costs 1000× what a vCPU does.
	weird := FargateRates{Platform: EKSLinuxX86, VCPUHourlyUSD: 0.00001, GBHourlyUSD: 10}
	if !weird.valid() {
		t.Fatal("test rates should be valid")
	}
	after, err := Quantize(req(1000, gb(8)), model.Resources{})
	if err != nil || after != before {
		t.Fatalf("quantization changed with rates: %v → %v (%v)", before, after, err)
	}
	// …and the price did move, so the override seam is real.
	if weird.Cost(before) == DefaultFargateRates().Cost(before) {
		t.Fatal("overridden rates priced identically to the defaults")
	}
}
