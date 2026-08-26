package ebs

import (
	"math"
	"strings"
	"testing"
)

// FuzzParityNeverUnderProvisions is the invariant this whole unit exists to
// hold: a proposed gp3 configuration NEVER clears less than the measured
// demand it was derived from — not for any size, any measurement, or any
// floor. A violation is a silently degraded volume, which is §7 trap 6.
//
// Every accepted plan must also be a configuration AWS would accept and
// strictly cheaper than the gp2 volume; anything else must come back as a
// refusal with a code.
func FuzzParityNeverUnderProvisions(f *testing.F) {
	seeds := []struct {
		size  int64
		iops  float64
		tput  float64
		floor bool
	}{
		{4000, 4000, 100, false},
		{4000, 12000, 250, true},
		{500, 0, 0, false},
		{350, 200, 40, true},
		{1, 1, 1, false},
		{16384, 16000, 250, true},
		{1000, 3000, 250, false},
		{8, 5000, 0, false},
		{100, math.NaN(), 0, false},
		{-1, 10, 10, true},
		{0, 0, 0, false},
		{5334, 15999, 249.5, true},
	}
	for _, s := range seeds {
		f.Add(s.size, s.iops, s.tput, s.floor)
	}

	rates := DefaultRates()
	f.Fuzz(func(t *testing.T, sizeGiB int64, iops, tput float64, floored bool) {
		floor := FloorMeasured
		if floored {
			floor = FloorGP2Baseline
		}
		measured := Demand{IOPS: iops, ThroughputMBps: tput}
		plan, ref := rates.PlanGP3(sizeGiB, measured, floor)

		if ref != nil {
			if ref.Code == "" || ref.Reason == "" {
				t.Fatalf("refusal without a code or reason: %+v", ref)
			}
			return
		}

		// (1) The invariant.
		if !plan.Config.Clears(measured) {
			t.Fatalf("%d GiB: proposed %+v does not clear measured %+v", sizeGiB, plan.Config, measured)
		}
		if !plan.Config.Clears(plan.Required) {
			t.Fatalf("%d GiB: proposed %+v does not clear required %+v", sizeGiB, plan.Config, plan.Required)
		}
		// (2) A configuration AWS would accept.
		if err := plan.Config.Validate(); err != nil {
			t.Fatalf("%d GiB: proposed an invalid configuration %+v: %v", sizeGiB, plan.Config, err)
		}
		// (3) Strictly cheaper, and priced by the rate card.
		if plan.DeltaMonthlyUSD <= 0 {
			t.Fatalf("%d GiB: accepted a change that saves $%.6f", sizeGiB, plan.DeltaMonthlyUSD)
		}
		want := rates.GP3MonthlyUSD(plan.Config)
		if math.Abs(plan.ProposedMonthlyUSD-want) > 1e-9 {
			t.Fatalf("%d GiB: priced $%.6f, rate card says $%.6f", sizeGiB, plan.ProposedMonthlyUSD, want)
		}
		if math.IsNaN(plan.DeltaMonthlyUSD) || math.IsInf(plan.DeltaMonthlyUSD, 0) {
			t.Fatalf("%d GiB: non-finite saving %v", sizeGiB, plan.DeltaMonthlyUSD)
		}
		// (4) Under the baseline floor, never below what gp2 delivered.
		if floor == FloorGP2Baseline {
			gp2 := GP2PerformanceFor(sizeGiB)
			if float64(plan.Config.IOPS) < float64(gp2.BaselineIOPS)-eps ||
				float64(plan.Config.ThroughputMBps) < gp2.BaselineThroughputMBps-eps {
				t.Fatalf("%d GiB: floored proposal %+v is below the gp2 baseline (%d IOPS, %v MiB/s)",
					sizeGiB, plan.Config, gp2.BaselineIOPS, gp2.BaselineThroughputMBps)
			}
		}
		// (5) The naive configuration is reported honestly: if it would have
		// been short, the shortfall is stated and the proposal is not it.
		if plan.NaiveDegrades && plan.Config == plan.Naive {
			t.Fatalf("%d GiB: proposed the naive configuration while reporting it as degrading", sizeGiB)
		}
		if !plan.NaiveDegrades && !plan.Naive.Clears(plan.Required) {
			t.Fatalf("%d GiB: naive %+v does not clear %+v but was not reported as degrading",
				sizeGiB, plan.Naive, plan.Required)
		}
	})
}

// FuzzLoadRates pins that an override rate file can never produce rates the
// parity math would then use: either it parses into something Validate
// accepts, or it is rejected.
func FuzzLoadRates(f *testing.F) {
	f.Add(`{"gp2GBMonthUSD":0.1}`)
	f.Add(`{"gp3FreeIOPS":3000,"gp3IOPSMonthUSD":0.005}`)
	f.Add(`{"gp2GBMonthUSD":-1}`)
	f.Add(`{`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, in string) {
		r, err := LoadRates(strings.NewReader(in))
		if err != nil {
			return
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("LoadRates accepted rates that do not validate: %+v: %v", r, err)
		}
		// Accepted rates must price a plain volume finitely.
		if v := r.GP2MonthlyUSD(500); math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("accepted rates price a 500 GiB gp2 volume at %v", v)
		}
	})
}
