package pricing

import (
	"errors"
	"math"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

// nonNegative maps a fuzz-supplied delta to a non-negative magnitude without
// tripping over -MinInt64 having no int64 representation.
func nonNegative(d int64) int64 {
	if d >= 0 {
		return d
	}
	if d == math.MinInt64 {
		return math.MaxInt64
	}
	return -d
}

// FuzzQuantize asserts the quantizer's contract on arbitrary input, including
// the garbage a snapshot can carry: the answer is always a real Fargate tier,
// always covers the request plus the 256 MiB overhead, is always the smallest
// and cheapest such tier, is monotonic in both dimensions, and is rejected only
// when the ceiling genuinely cannot hold the pod. Nothing here depends on the
// rate table beyond the cost check.
func FuzzQuantize(f *testing.F) {
	f.Add(int64(200), int64(512<<20), int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(1000), int64(8<<30), int64(0), int64(0), int64(1), int64(1<<20))
	f.Add(int64(0), int64(0), int64(0), int64(0), int64(250), int64(1<<30))
	f.Add(int64(16000), int64(120<<30), int64(0), int64(0), int64(1), int64(1))
	f.Add(int64(-1), int64(-1), int64(-1), int64(-1), int64(-1), int64(-1))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(0), int64(math.MaxInt64-1), int64(0), int64(0), int64(0), int64(1))

	rates := DefaultFargateRates()
	table := FargateConfigs()
	valid := make(map[FargateConfig]bool, len(table))
	for _, c := range table {
		valid[c] = true
	}

	f.Fuzz(func(t *testing.T, cpu, memB, initCPU, initMem, dCPU, dMem int64) {
		run := model.Resources{MilliCPU: cpu, MemoryBytes: memB}
		init := model.Resources{MilliCPU: initCPU, MemoryBytes: initMem}

		got, err := Quantize(run, init)

		// Determinism: a pure function of its inputs, no map iteration, no clock.
		if got2, err2 := Quantize(run, init); got2 != got || (err2 == nil) != (err == nil) {
			t.Fatalf("non-deterministic: (%v,%v) then (%v,%v)", got, err, got2, err2)
		}

		eff := clampNonNegative(run).Max(clampNonNegative(init))
		need := eff.Add(model.Resources{MemoryBytes: FargateOverheadBytes})
		ceilingFits := FargateMaxConfig.MilliCPU >= need.MilliCPU &&
			FargateMaxConfig.MemoryBytes() >= need.MemoryBytes

		if err != nil {
			if !errors.Is(err, ErrFargateTooLarge) {
				t.Fatalf("unexpected error kind: %v", err)
			}
			if got != (FargateConfig{}) {
				t.Fatalf("rejection returned a configuration: %v", got)
			}
			if ceilingFits {
				t.Fatalf("rejected %s though the ceiling %v covers it", need, FargateMaxConfig)
			}
			return
		}
		if !ceilingFits {
			t.Fatalf("accepted %s though it exceeds the ceiling %v", need, FargateMaxConfig)
		}

		// 1. The answer is a real tier — never an interpolated shape.
		if !valid[got] {
			t.Fatalf("%v is not a valid Fargate configuration", got)
		}
		// 2. It covers the effective request *plus* the overhead, in both
		//    dimensions. This is the invariant whose absence is the live bug.
		if got.MilliCPU < need.MilliCPU || got.MemoryBytes() < need.MemoryBytes {
			t.Fatalf("%v does not cover %s (request %s + 256 MiB)", got, need, eff)
		}
		if got.MemoryBytes() < eff.MemoryBytes+FargateOverheadBytes && eff.MemoryBytes >= 0 &&
			eff.MemoryBytes <= math.MaxInt64-FargateOverheadBytes {
			t.Fatalf("%v swallowed the overhead for request %s", got, eff)
		}
		// 3. It is the smallest such tier: nothing earlier in canonical order fits.
		for _, c := range table {
			if c == got {
				break
			}
			if c.MilliCPU >= need.MilliCPU && c.MemoryBytes() >= need.MemoryBytes {
				t.Fatalf("%v fits %s and is smaller than the chosen %v", c, need, got)
			}
		}
		// 4. …and therefore also the cheapest, which is what makes savings math
		//    honest rather than merely conservative.
		for _, c := range table {
			if c.MilliCPU < need.MilliCPU || c.MemoryBytes() < need.MemoryBytes {
				continue
			}
			if rates.Cost(c) < rates.Cost(got) {
				t.Fatalf("%v ($%.6f) fits %s and is cheaper than %v ($%.6f)",
					c, rates.Cost(c), need, got, rates.Cost(got))
			}
		}
		// 5. The answer round-trips through the AWS annotation form, so the
		//    production cross-check against CapacityProvisioned can work.
		if parsed, perr := ParseCapacityProvisioned(got.String()); perr != nil || parsed != got {
			t.Fatalf("%v renders as %q which parses back to %v (%v)", got, got.String(), parsed, perr)
		}

		// 6. Monotonic in each dimension: asking for more never bills less.
		bigger := run.Add(model.Resources{
			MilliCPU:    nonNegative(dCPU),
			MemoryBytes: nonNegative(dMem),
		})
		up, upErr := Quantize(bigger, init)
		if upErr != nil {
			// Growing a request may cross the ceiling; it may never make a
			// previously-rejected request acceptable.
			return
		}
		if up.MilliCPU < got.MilliCPU || up.MemoryBytes() < got.MemoryBytes() {
			t.Fatalf("not monotonic: %s → %v but %s → %v", eff, got, bigger, up)
		}
		if rates.Cost(up) < rates.Cost(got) {
			t.Fatalf("not monotonic in price: %s costs $%.6f, %s costs $%.6f",
				eff, rates.Cost(got), bigger, rates.Cost(up))
		}
	})
}

// FuzzQuantizeMonotonicDownward is the mirror property, stated on the failure
// path: if a request is rejected, every larger request is rejected too.
func FuzzQuantizeMonotonicDownward(f *testing.F) {
	f.Add(int64(16001), int64(0), int64(1), int64(1))
	f.Add(int64(16000), int64(120<<30), int64(0), int64(1<<20))
	f.Fuzz(func(t *testing.T, cpu, memB, dCPU, dMem int64) {
		run := model.Resources{MilliCPU: cpu, MemoryBytes: memB}
		if _, err := Quantize(run, model.Resources{}); err == nil {
			return
		}
		bigger := run.Add(model.Resources{
			MilliCPU:    nonNegative(dCPU),
			MemoryBytes: nonNegative(dMem),
		})
		if got, err := Quantize(bigger, model.Resources{}); err == nil {
			t.Fatalf("%s was rejected but the larger %s quantized to %v", run, bigger, got)
		}
	})
}
