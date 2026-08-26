package rds

import (
	"math"
	"testing"
)

// FuzzRDSParityNeverUnderProvisions is this unit's load-bearing property,
// asserted over arbitrary inputs rather than over the handful a table test
// reaches — the pkg/ebs discipline, applied to a different table.
//
// The property: whenever PlanParity returns a plan with no refusal, that plan
//
//  1. clears the required demand in BOTH dimensions;
//  2. clears the MEASURED demand too, whichever floor was in play;
//  3. never sits below the regime's non-reducible baseline;
//  4. is a configuration AWS would accept — including the "not provisionable
//     below the striping threshold" rule and the LIVE envelope;
//  5. is strictly cheaper than what the instance costs today;
//  6. prices to the rate card, recomputed independently.
//
// A conversion that silently delivers less than the volume was doing is the
// single failure this whole unit exists to prevent, and (1) through (4) are
// four different ways of saying it cannot happen.
func FuzzRDSParityNeverUnderProvisions(f *testing.F) {
	f.Add(int64(0), int64(500), 400.0, 60.0, int64(0), int64(0), int64(0), int64(0))
	f.Add(int64(1), int64(1000), 9000.0, 400.0, int64(1), int64(20000), int64(900), int64(1))
	f.Add(int64(2), int64(200), 100.0, 10.0, int64(0), int64(0), int64(0), int64(2))
	f.Add(int64(3), int64(399), 0.0, 0.0, int64(1), int64(0), int64(0), int64(0))
	f.Add(int64(4), int64(65536), 64000.0, 4000.0, int64(0), int64(0), int64(0), int64(1))
	f.Add(int64(5), int64(0), math.NaN(), math.Inf(1), int64(1), int64(-5), int64(-5), int64(3))
	f.Add(int64(0), int64(1<<40), -1.0, -1.0, int64(0), int64(1<<31), int64(1<<31), int64(2))

	card, perf := parityCard(), parityPerf()

	f.Fuzz(func(t *testing.T, engineSeed, sizeGiB int64, iops, tput float64,
		floorSeed, curIOPS, curTput, envSeed int64) {

		// Fold rather than skip: a skipped input is an input the property was
		// never asserted over.
		engines := []Engine{
			mysqlEngine(), ParseEngine("postgres", LicenseGPL), ParseEngine("mariadb", LicenseGPL),
			oracleEngine(), mssqlEngine(), ParseEngine("db2-ae", LicenseBYOL),
			ParseEngine("aurora-mysql", ""), ParseEngine("greatdb", ""),
		}
		e := engines[foldIndex(engineSeed, len(engines))]
		size := foldParitySize(sizeGiB)
		floor := ParityFloorMeasured
		if floorSeed%2 == 1 {
			floor = ParityFloorNameplate
		}

		storageType := StorageGP2
		inst := DBInstance{
			ARN: "arn:aws:rds:us-east-1:1234:db:z", Identifier: "z", Class: "db.r6i.xlarge",
			Engine: e.Raw, Status: StatusAvailable, AllocatedStorageGiB: size,
		}
		if curIOPS != 0 || curTput != 0 {
			storageType = StorageGP3
			inst.IOPS = foldInt32(curIOPS)
			inst.StorageThroughputMBps = foldInt32(curTput)
		}
		inst.StorageType = storageType

		envelopes := []Envelopes{
			{}, // never read
			stripedEnvelope("z"),
			NewEnvelopes([]Envelope{{Identifier: "z", HistoryKnown: true,
				Storage: []StorageEnvelope{{StorageType: StorageGP3, Known: true,
					MinIOPS: 12000, MaxIOPS: 16000, MinThroughputMBps: 500, MaxThroughputMBps: 1000}}}}),
			NewEnvelopes([]Envelope{{Identifier: "z", HistoryKnown: true,
				Storage: []StorageEnvelope{{StorageType: StorageGP3, Known: true,
					MinIOPS: 3000, MaxIOPS: 80000, MinThroughputMBps: 125, MaxThroughputMBps: 4000}}}}),
		}
		env := envelopes[foldIndex(envSeed, len(envelopes))].Get("z")

		measured := Demand{IOPS: iops, ThroughputMBps: tput}
		plan, ref := PlanParity(card, perf, e, inst, env, measured, floor)
		if ref != nil {
			if ref.Code == "" || ref.Reason == "" {
				t.Fatalf("refusal with no %s", map[bool]string{true: "code", false: "reason"}[ref.Code == ""])
			}
			return
		}

		// (1) and (2).
		if !plan.Config.Clears(plan.Required) {
			t.Fatalf("%s %d GiB %v: config %+v does not clear required %+v",
				e.String(), size, floor, plan.Config, plan.Required)
		}
		if !plan.Config.Clears(measured) {
			t.Fatalf("%s %d GiB %v: config %+v does not clear MEASURED %+v",
				e.String(), size, floor, plan.Config, measured)
		}
		// (3).
		if plan.Config.IOPS < plan.Regime.BaselineIOPS ||
			plan.Config.ThroughputMBps < plan.Regime.BaselineThroughputMBps {
			t.Fatalf("%s %d GiB: config %+v is below the non-reducible baseline %d/%d",
				e.String(), size, plan.Config, plan.Regime.BaselineIOPS, plan.Regime.BaselineThroughputMBps)
		}
		// Under the nameplate floor, never below what the volume delivers now.
		if floor == ParityFloorNameplate {
			if plan.Config.IOPS < plan.Current.IOPS ||
				plan.Config.ThroughputMBps < plan.Current.ThroughputMBps {
				t.Fatalf("%s %d GiB: floored config %+v delivers less than the current %+v",
					e.String(), size, plan.Config, plan.Current)
			}
		}
		// (4).
		if err := plan.Config.Validate(plan.Regime, env.For(StorageGP3)); err != nil {
			t.Fatalf("%s %d GiB: accepted a configuration AWS would reject: %v", e.String(), size, err)
		}
		if plan.Config.Provisions() && !plan.Regime.Provisionable {
			t.Fatalf("%s %d GiB: provisioned %+v below the striping threshold", e.String(), size, plan.Config)
		}
		if plan.Config.Provisions() && !env.For(StorageGP3).Known {
			t.Fatalf("%s %d GiB: provisioned %+v with no live envelope", e.String(), size, plan.Config)
		}
		// (5).
		if plan.DeltaMonthlyUSD <= 0 {
			t.Fatalf("%s %d GiB: accepted with Δ $%.6f", e.String(), size, plan.DeltaMonthlyUSD)
		}
		// (6): the rate card, recomputed by hand.
		want := 0.092*float64(size) +
			0.02*math.Max(0, float64(plan.Config.IOPS-plan.Regime.BaselineIOPS)) +
			0.08*math.Max(0, float64(plan.Config.ThroughputMBps-plan.Regime.BaselineThroughputMBps))
		if math.Abs(plan.ProposedMonthlyUSD-want) > 1e-9 {
			t.Fatalf("%s %d GiB %+v: priced $%.6f, rate card says $%.6f",
				e.String(), size, plan.Config, plan.ProposedMonthlyUSD, want)
		}
		// A proposal is only ever produced from a claimable rate, and this card
		// is operator-supplied, so the provenance must survive the arithmetic.
		if !plan.RateProvenance.Claimable() {
			t.Fatalf("an operator-supplied card produced %q provenance", plan.RateProvenance)
		}
	})
}

// foldParitySize maps an arbitrary int64 into a plausible allocation. RDS
// allocations outside 0…1 PiB are a collector bug rather than a case this
// arithmetic may decline to answer, so out-of-range values are folded to the
// edges instead of skipped.
func foldParitySize(v int64) int64 {
	if v < 0 {
		v = -v
	}
	if v < 0 { // math.MinInt64
		return MaxParitySizeGiB
	}
	return v % (MaxParitySizeGiB + 2)
}

func foldInt32(v int64) int32 {
	if v < 0 {
		v = -v
	}
	if v < 0 {
		return math.MaxInt32
	}
	return int32(v % (math.MaxInt32 / 2))
}

func foldIndex(v int64, n int) int {
	if n <= 0 {
		return 0
	}
	i := int(v % int64(n))
	if i < 0 {
		i += n
	}
	return i
}
