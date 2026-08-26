package rds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/ebs"
)

// --- fixtures ---------------------------------------------------------------

// parityCard is an OPERATOR-supplied storage card. It has to be: every shipped
// RDS rate is unverified, and an unverified rate may size a fact and may never
// become a saving (U11 §5.1), so a card that could produce a claim is the only
// one that exercises the accept path at all.
//
// gp3 is priced below gp2 per GiB here. That is a deliberate CHOICE OF FIXTURE,
// not a claim about AWS: it is the only relationship under which a conversion
// can ever be cheaper, and TestShippedParityRatesCannotClaim covers what the
// shipped card (gp2 == gp3) does instead, which is refuse everything.
func parityCard() RateCard {
	c := DefaultRates()
	c.Storage = StorageRates{
		GP2GiBMonthUSD: 0.115, GP3GiBMonthUSD: 0.092,
		IO1GiBMonthUSD: 0.125, IO2GiBMonthUSD: 0.125,
		Provenance: RateOperator,
	}
	return c
}

func parityPerf() PerformanceRates {
	return PerformanceRates{
		ProvisionedIOPSMonthUSD:       0.02,
		ProvisionedThroughputMonthUSD: 0.08,
		Provenance:                    RateOperator,
	}
}

// parityIO builds the four I/O series MeasureIO reads. Throughput is handed in
// as MiB/s and stored as bytes/s, which is what CloudWatch publishes.
func parityIO(readIOPS, writeIOPS, readMBps, writeMBps float64) []Series {
	mk := func(metric string, v float64) Series {
		return Series{Metric: metric, Stat: "Average",
			PeriodSeconds: PublicationPeriodSeconds, Points: flat(v, 48)}
	}
	return []Series{
		mk(MetricReadIOPS, readIOPS),
		mk(MetricWriteIOPS, writeIOPS),
		mk(MetricReadThroughput, readMBps*MiB),
		mk(MetricWriteThroughput, writeMBps*MiB),
	}
}

// stripedEnvelope is the striped regime's published provisionable range as a
// LIVE envelope: 12,000–64,000 IOPS and 500–4,000 MiB/s.
func stripedEnvelope(ids ...string) Envelopes {
	out := make([]Envelope, 0, len(ids))
	for _, id := range ids {
		out = append(out, Envelope{
			Identifier: id, HistoryKnown: true,
			Storage: []StorageEnvelope{{
				StorageType: StorageGP3, Known: true,
				MinIOPS: 12000, MaxIOPS: 64000,
				MinThroughputMBps: 500, MaxThroughputMBps: 4000,
			}},
		})
	}
	return NewEnvelopes(out)
}

func parityInstance(id string, sizeGiB int64, storageType string) DBInstance {
	return DBInstance{
		ARN: "arn:aws:rds:us-east-1:1234:db:" + id, Identifier: id,
		Class: "db.r6i.xlarge", Engine: "mysql", LicenseModel: LicenseGPL,
		Status: StatusAvailable, AllocatedStorageGiB: sizeGiB, StorageType: storageType,
	}
}

func mysqlEngine() Engine  { return ParseEngine("mysql", LicenseGPL) }
func oracleEngine() Engine { return ParseEngine("oracle-se2", LicenseBYOL) }
func mssqlEngine() Engine  { return ParseEngine("sqlserver-se", LicenseIncluded) }

// --- trap 11 ----------------------------------------------------------------

// TestRDSGP2ModelIsNotTheEBSModel is trap 11, stated as the design document
// states it: one 500 GiB MySQL volume through both models, both numbers named,
// and this one asserted against the published RDS table.
//
//	"a 500 GiB MySQL instance on gp2 delivers 1,500 baseline IOPS and (per the
//	band) 512–1,000 MiB/s" — docs/design/rds-batch-assessment.md §2.4
//
// pkg/ebs's model of the same size says 250 MiB/s and a 3,000 IOPS burst,
// because those are the right numbers for a raw EBS volume and the wrong ones
// for RDS, which stripes this allocation across four of them.
func TestRDSGP2ModelIsNotTheEBSModel(t *testing.T) {
	const size = 500

	rds, ok := GP2PerformanceForRDS(mysqlEngine(), size)
	if !ok {
		t.Fatal("the published RDS gp2 table has no row for a 500 GiB MySQL volume")
	}
	raw := ebs.GP2PerformanceFor(size)

	// Both numbers, named. The IOPS baselines agree — 3 IOPS/GiB is the one
	// thing the two products share — and everything that decides a dollar
	// disagrees.
	t.Logf("500 GiB MySQL: pkg/ebs says %d baseline IOPS, %d burst IOPS, %.0f MiB/s; "+
		"pkg/rds says %d baseline IOPS, %d burst IOPS, %d–%d MiB/s",
		raw.BaselineIOPS, raw.BurstIOPS, raw.BaselineThroughputMBps,
		rds.BaselineIOPS, rds.BurstIOPS, rds.MinThroughputMBps, rds.ParityThroughputMBps)

	if rds.BaselineIOPS != 1500 {
		t.Errorf("RDS baseline = %d IOPS, the published band says 1,500 at 500 GiB", rds.BaselineIOPS)
	}
	if rds.MinThroughputMBps != 512 || rds.ParityThroughputMBps != 1000 {
		t.Errorf("RDS throughput = %d–%d MiB/s, the published 400–1,335 GiB band says 512–1,000",
			rds.MinThroughputMBps, rds.ParityThroughputMBps)
	}
	if rds.BurstIOPS != 12000 || !rds.Burstable {
		t.Errorf("RDS burst = %d IOPS (burstable %v), the published band says 12,000",
			rds.BurstIOPS, rds.Burstable)
	}
	if !rds.Striped {
		t.Error("a 500 GiB MySQL allocation is above the 400 GiB threshold and must be striped")
	}

	// The disagreement, asserted rather than assumed. If pkg/ebs ever grows
	// RDS's numbers this test's premise is stale and it says so.
	if raw.BurstIOPS == rds.BurstIOPS {
		t.Fatalf("both models report a %d IOPS burst; this test no longer exercises trap 11", raw.BurstIOPS)
	}
	if int32(raw.BaselineThroughputMBps) == rds.MinThroughputMBps {
		t.Fatalf("both models report %v MiB/s; this test no longer exercises trap 11",
			raw.BaselineThroughputMBps)
	}
	if raw.BurstIOPS != 3000 || raw.BaselineThroughputMBps != 250 {
		t.Fatalf("pkg/ebs no longer says 3,000 IOPS / 250 MiB/s at 500 GiB (got %d / %v); "+
			"this test's other half is stale", raw.BurstIOPS, raw.BaselineThroughputMBps)
	}

	// And the consequence, which is the whole point: converting this volume to
	// gp3 raises IOPS ~8× and LOWERS throughput, from at least 512 MiB/s to
	// the striped baseline's 500.
	r := GP3RegimeFor(mysqlEngine(), size)
	if r.BaselineIOPS != 12000 || r.BaselineThroughputMBps != 500 {
		t.Fatalf("gp3 striped baseline = %d/%d, want 12,000/500", r.BaselineIOPS, r.BaselineThroughputMBps)
	}
	if !(r.BaselineIOPS > rds.BaselineIOPS*7) {
		t.Errorf("gp3 %d IOPS is not the ~8× rise §2.4 describes over gp2's %d",
			r.BaselineIOPS, rds.BaselineIOPS)
	}
	if !(r.BaselineThroughputMBps < rds.MinThroughputMBps) {
		t.Errorf("gp3 %d MiB/s does not fall below gp2's guaranteed %d MiB/s; §2.4's falsifiable "+
			"consequence no longer holds", r.BaselineThroughputMBps, rds.MinThroughputMBps)
	}
}

// TestRDSGP2TableMatchesPublishedBands transcribes the published RDS gp2 table
// a second time, by hand, and probes the model at both edges of every band.
//
// It deliberately does not read gp2Bands: a test that checks a constant
// against itself proves nothing, and one wrong row here silently mis-prices
// every conversion in that size range. This is the discipline
// pkg/pricing/commit's TestRDSNormalizationTableMatchesPublishedUnits set.
func TestRDSGP2TableMatchesPublishedBands(t *testing.T) {
	type row struct {
		engine                   Engine
		minGiB, maxGiB           int64
		minIOPS, maxIOPS         int32
		minTputMBps, maxTputMBps int32
		burstIOPS                int32
	}
	// docs/design/rds-batch-assessment.md §2.4, verified against
	// CHAP_Storage.html on 2026-08-26.
	published := []row{
		{mysqlEngine(), 5, 399, 100, 1197, 128, 250, 3000},
		{mysqlEngine(), 400, 1335, 1200, 4005, 512, 1000, 12000},
		{mysqlEngine(), 1336, 3999, 4008, 11997, 1000, 1000, 12000},
		{mysqlEngine(), 4000, 65536, 12000, 64000, 1000, 1000, 0},
		{mssqlEngine(), 334, 999, 1002, 2997, 250, 250, 3000},
	}
	for _, r := range published {
		for _, probe := range []struct {
			size     int64
			wantIOPS int32
		}{{r.minGiB, r.minIOPS}, {r.maxGiB, r.maxIOPS}} {
			got, ok := GP2PerformanceForRDS(r.engine, probe.size)
			if !ok {
				t.Errorf("%s %d GiB: no published band, want the %d–%d GiB row",
					r.engine.String(), probe.size, r.minGiB, r.maxGiB)
				continue
			}
			if got.BaselineIOPS != probe.wantIOPS {
				t.Errorf("%s %d GiB: baseline %d IOPS, published table says %d",
					r.engine.String(), probe.size, got.BaselineIOPS, probe.wantIOPS)
			}
			if got.MinThroughputMBps != r.minTputMBps || got.ParityThroughputMBps != r.maxTputMBps {
				t.Errorf("%s %d GiB: throughput %d–%d MiB/s, published table says %d–%d",
					r.engine.String(), probe.size, got.MinThroughputMBps, got.ParityThroughputMBps,
					r.minTputMBps, r.maxTputMBps)
			}
			wantBurst, wantBurstable := r.burstIOPS, r.burstIOPS > probe.wantIOPS
			if !wantBurstable {
				wantBurst = probe.wantIOPS
			}
			if got.BurstIOPS != wantBurst || got.Burstable != wantBurstable {
				t.Errorf("%s %d GiB: burst %d (burstable %v), published table says %d (%v)",
					r.engine.String(), probe.size, got.BurstIOPS, got.Burstable, wantBurst, wantBurstable)
			}
		}
	}

	// STRUCTURAL ABSENCE, the other half of the discipline. pkg/ebs's table has
	// a 1,000 GiB burst cutoff and a 16,000 IOPS ceiling; the RDS table has
	// neither, because four striped volumes have four burst buckets and four
	// ceilings. If the RDS model ever grows them, it has silently become the
	// EBS model.
	if p, _ := GP2PerformanceForRDS(mysqlEngine(), 1000); !p.Burstable || p.BurstIOPS != 12000 {
		t.Errorf("1,000 GiB MySQL: burst %d (burstable %v); RDS has no 1,000 GiB burst cutoff",
			p.BurstIOPS, p.Burstable)
	}
	if raw := ebs.GP2PerformanceFor(1000); raw.Burstable {
		t.Error("pkg/ebs lost its 1,000 GiB burst cutoff; this assertion's premise is stale")
	}
	if p, _ := GP2PerformanceForRDS(mysqlEngine(), 65536); p.BaselineIOPS != 64000 {
		t.Errorf("65,536 GiB MySQL: %d IOPS, the published table says 64,000 — four times pkg/ebs's "+
			"per-volume 16,000 ceiling", p.BaselineIOPS)
	}

	// Oracle and Db2 appear in the STRIPING table and in no gp2 row. Borrowing
	// MySQL's bands for an engine that stripes at a different size is trap 11
	// committed against a second engine, so they refuse.
	for _, e := range []Engine{oracleEngine(), ParseEngine("db2-se", LicenseBYOL)} {
		if _, ok := GP2PerformanceForRDS(e, 500); ok {
			t.Errorf("%s: a gp2 nameplate was produced from a table AWS does not publish for it",
				e.String())
		}
	}
	// SQL Server outside its one published band refuses for the same reason.
	for _, size := range []int64{333, 1000} {
		if _, ok := GP2PerformanceForRDS(mssqlEngine(), size); ok {
			t.Errorf("SQL Server %d GiB: outside the published 334–999 GiB row, yet a nameplate was "+
				"produced", size)
		}
	}
}

// --- the striping thresholds ------------------------------------------------

// TestStripingThresholdIsEngineDependent is the 400 / 200 / never table, and
// the reason a single "RDS gp3" constant cannot exist.
func TestStripingThresholdIsEngineDependent(t *testing.T) {
	// Transcribed from §2.4's striping table, not read from the model.
	published := []struct {
		engine    Engine
		threshold int64
	}{
		{ParseEngine("db2-ae", LicenseBYOL), 400},
		{ParseEngine("mariadb", LicenseGPL), 400},
		{ParseEngine("mysql", LicenseGPL), 400},
		{ParseEngine("postgres", LicenseGPL), 400},
		{ParseEngine("oracle-ee", LicenseBYOL), 200},
		{ParseEngine("oracle-se2", LicenseIncluded), 200},
		{ParseEngine("sqlserver-ee", LicenseIncluded), NeverStripes},
		{ParseEngine("sqlserver-web", LicenseIncluded), NeverStripes},
	}
	for _, p := range published {
		if got := StripingThresholdGiBFor(p.engine); got != p.threshold {
			t.Errorf("%s: striping threshold %d GiB, published table says %d",
				p.engine.String(), got, p.threshold)
		}
		if p.threshold == NeverStripes {
			// "never (1 volume)" — at every size, including the largest.
			for _, size := range []int64{1, 334, 399, 400, 1000, 16384, 65536} {
				if Stripes(p.engine, size) {
					t.Errorf("%s at %d GiB is striped; the published table says one volume, always",
						p.engine.String(), size)
				}
				if r := GP3RegimeFor(p.engine, size); r.Striped || r.BaselineIOPS != 3000 ||
					r.BaselineThroughputMBps != 125 {
					t.Errorf("%s at %d GiB: gp3 regime %+v, want the unstriped 3,000/125 baseline",
						p.engine.String(), size, r)
				}
			}
			continue
		}
		if Stripes(p.engine, p.threshold-1) {
			t.Errorf("%s at %d GiB is striped one GiB BELOW its threshold", p.engine.String(), p.threshold-1)
		}
		if !Stripes(p.engine, p.threshold) {
			t.Errorf("%s at %d GiB is not striped AT its threshold", p.engine.String(), p.threshold)
		}
		below := GP3RegimeFor(p.engine, p.threshold-1)
		at := GP3RegimeFor(p.engine, p.threshold)
		if below.BaselineIOPS != 3000 || below.BaselineThroughputMBps != 125 || below.Provisionable {
			t.Errorf("%s below the threshold: %+v, want 3,000/125 and NOT provisionable",
				p.engine.String(), below)
		}
		if at.BaselineIOPS != 12000 || at.BaselineThroughputMBps != 500 || !at.Provisionable {
			t.Errorf("%s at the threshold: %+v, want 12,000/500 and provisionable", p.engine.String(), at)
		}
	}

	// Oracle's threshold is HALF MySQL's, which is the falsifiable half of the
	// table: a 300 GiB volume is striped under Oracle and not under MySQL.
	if !Stripes(oracleEngine(), 300) || Stripes(mysqlEngine(), 300) {
		t.Error("a 300 GiB allocation must be striped under Oracle and unstriped under MySQL")
	}
	// And an engine with no encoded threshold is refused rather than defaulted.
	if r := GP3RegimeFor(ParseEngine("aurora-mysql", ""), 500); r.Known {
		t.Error("Aurora acquired a striping threshold; it is a different billing model (trap 16)")
	}
}

// --- provisioning below the threshold ---------------------------------------

// TestGP3IsNotProvisionableBelowTheThreshold is the sharpest edge of trap 11:
// below the striping threshold the published provisioning columns read "N/A",
// so a configuration that provisions there is a Validate() violation and can
// never become a proposal.
func TestGP3IsNotProvisionableBelowTheThreshold(t *testing.T) {
	e := mysqlEngine()
	const size = 399 // one GiB below MySQL's 400 GiB threshold
	r := GP3RegimeFor(e, size)
	if r.Provisionable {
		t.Fatal("a 399 GiB MySQL volume reports as provisionable")
	}
	env := stripedEnvelope("db").Get("db") // a KNOWN envelope, so this is not an envelope refusal

	// Every way of asking for more than the baseline is rejected.
	for _, c := range []GP3Config{
		{SizeGiB: size, IOPS: 3001, ThroughputMBps: 125, ProvisionedIOPS: true},
		{SizeGiB: size, IOPS: 3000, ThroughputMBps: 126, ProvisionedThroughput: true},
		{SizeGiB: size, IOPS: 12000, ThroughputMBps: 500, ProvisionedIOPS: true, ProvisionedThroughput: true},
	} {
		err := c.Validate(r, env.For(StorageGP3))
		if err == nil {
			t.Fatalf("Validate accepted %+v below the striping threshold", c)
		}
		if !strings.Contains(err.Error(), "N/A") {
			t.Errorf("the refusal does not quote the published \"N/A\" column: %v", err)
		}
	}
	// The baseline itself is fine — it is what the volume delivers for free.
	if err := (GP3Config{SizeGiB: size, IOPS: 3000, ThroughputMBps: 125}).Validate(r, env.For(StorageGP3)); err != nil {
		t.Errorf("the free baseline was rejected below the threshold: %v", err)
	}
	// And below the baseline is rejected in the other direction.
	if err := (GP3Config{SizeGiB: size, IOPS: 2999, ThroughputMBps: 125}).Validate(r, env.For(StorageGP3)); err == nil {
		t.Error("Validate accepted a configuration below the non-reducible baseline")
	}

	// End to end: a 399 GiB gp2 volume the published table credits with 250
	// MiB/s cannot be converted, and the refusal says so by name.
	inst := parityInstance("db", size, StorageGP2)
	_, ref := PlanParity(parityCard(), parityPerf(), e, inst, env, Demand{}, ParityFloorNameplate)
	if ref == nil || ref.Code != ReasonParityNotProvisionableBelowThreshold {
		t.Fatalf("399 GiB gp2 conversion: refusal = %v, want %s", ref, ReasonParityNotProvisionableBelowThreshold)
	}

	// The engine never emits such a proposal, so Report.Validate never sees
	// one — which is why the gate lives in GP3Config.Validate rather than in
	// an assertion someone can argue past.
	p, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db"),
		Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	prop, sup, ok := p.AssessParity(inst, e, parityIO(400, 200, 120, 60), parityCard())
	if !ok || prop != nil {
		t.Fatalf("a proposal was produced below the striping threshold: %+v", prop)
	}
	if !hasParityCode(sup, ReasonParityNotProvisionableBelowThreshold) {
		t.Fatalf("suppressions = %v, want %s", parityCodes(sup),
			ReasonParityNotProvisionableBelowThreshold)
	}

	// RDS for SQL Server is the ONE exception in the published sentence — "for
	// every DB engine except RDS for SQL Server" — and it can provision at any
	// size despite never striping.
	sq := GP3RegimeFor(mssqlEngine(), 200)
	if !sq.Provisionable || sq.Striped {
		t.Errorf("SQL Server at 200 GiB: %+v, want provisionable and unstriped", sq)
	}
}

// --- the reduction floor ----------------------------------------------------

// TestGP3ReductionFloorsAtTheBaseline is §2.4's "you can reduce a 20,000-IOPS
// instance to 12,000 and no further", asserted as a hard floor rather than as
// a clamp that a lower measurement could argue past.
func TestGP3ReductionFloorsAtTheBaseline(t *testing.T) {
	e := mysqlEngine()
	inst := parityInstance("db", 1000, StorageGP3)
	inst.IOPS, inst.StorageThroughputMBps = 20000, 900
	env := stripedEnvelope("db").Get("db")

	// Measured demand is far below the baseline — and the proposal still stops
	// at 12,000 / 500.
	plan, ref := PlanParity(parityCard(), parityPerf(), e, inst,
		env, Demand{IOPS: 500, ThroughputMBps: 20}, ParityFloorMeasured)
	if ref != nil {
		t.Fatalf("refused: %v", ref)
	}
	if plan.Config.IOPS != 12000 || plan.Config.ThroughputMBps != 500 {
		t.Fatalf("proposed %d IOPS / %d MiB/s, want the 12,000 / 500 floor",
			plan.Config.IOPS, plan.Config.ThroughputMBps)
	}
	if plan.Config.Provisions() {
		t.Error("a configuration sitting exactly on the baseline is not provisioning anything")
	}
	if !plan.Reduction() {
		t.Error("a gp3 → gp3 plan is a reduction")
	}
	// Independently priced: 1,000 GiB at $0.092, minus 8,000 IOPS at $0.02 and
	// 400 MiB/s at $0.08 that no longer have to be paid for.
	wantCurrent := 0.092*1000 + 0.02*(20000-12000) + 0.08*(900-500)
	wantProposed := 0.092 * 1000
	if math.Abs(plan.CurrentMonthlyUSD-wantCurrent) > 1e-9 ||
		math.Abs(plan.ProposedMonthlyUSD-wantProposed) > 1e-9 {
		t.Errorf("priced $%.4f → $%.4f, rate card says $%.4f → $%.4f",
			plan.CurrentMonthlyUSD, plan.ProposedMonthlyUSD, wantCurrent, wantProposed)
	}

	// Zero demand cannot push it lower, and neither can a negative one being
	// folded in: the floor is the regime's, not the measurement's.
	for _, d := range []Demand{{}, {IOPS: 1, ThroughputMBps: 1}} {
		p2, ref2 := PlanParity(parityCard(), parityPerf(), e, inst, env, d, ParityFloorMeasured)
		if ref2 != nil {
			t.Fatalf("demand %+v refused: %v", d, ref2)
		}
		if p2.Config.IOPS < 12000 || p2.Config.ThroughputMBps < 500 {
			t.Fatalf("demand %+v produced %d/%d, below the non-reducible baseline",
				d, p2.Config.IOPS, p2.Config.ThroughputMBps)
		}
	}

	// An instance ALREADY at the floor has an empty tail, and that is stated
	// rather than skipped.
	at := parityInstance("db", 1000, StorageGP3)
	at.IOPS, at.StorageThroughputMBps = 12000, 500
	_, ref = PlanParity(parityCard(), parityPerf(), e, at, env, Demand{IOPS: 100}, ParityFloorMeasured)
	if ref == nil || ref.Code != ReasonParityFloorsAtBaseline {
		t.Fatalf("an instance at the floor: refusal = %v, want %s", ref, ReasonParityFloorsAtBaseline)
	}

	// Below the striping threshold there is nothing to reduce, because there
	// was never anything to provision.
	small := parityInstance("db", 200, StorageGP3)
	_, ref = PlanParity(parityCard(), parityPerf(), e, small, env, Demand{IOPS: 100}, ParityFloorMeasured)
	if ref == nil || ref.Code != ReasonParityFloorsAtBaseline {
		t.Fatalf("a sub-threshold gp3 volume: refusal = %v, want %s", ref, ReasonParityFloorsAtBaseline)
	}
}

// --- the refusal band -------------------------------------------------------

// TestThroughputParityRefusalBand is the RDS analogue of pkg/ebs's
// TestParityRefusalBand, and it fires in a completely different size range —
// which is what proves the two tables are not silently shared.
//
// pkg/ebs refuses 334–375 GiB: volumes large enough to deliver gp2's 250 MiB/s
// ceiling but too small for the storage saving to pay for it.
//
// RDS refuses 400–1,739 GiB, for a different reason with different numbers: at
// or above the striping threshold the published gp2 band credits the volume
// with up to 1,000 MiB/s while gp3's striped baseline is 500, so 500 MiB/s of
// throughput has to be BOUGHT, and until the allocation is large enough the
// storage line does not cover it. Below 400 GiB RDS refuses too — but with a
// different code, because down there the throughput cannot be bought at all.
func TestThroughputParityRefusalBand(t *testing.T) {
	card, perf, e := parityCard(), parityPerf(), mysqlEngine()
	env := stripedEnvelope("db").Get("db")

	// The band boundary, recomputed by hand from the rate card rather than
	// read from the model: converting costs 500 MiB/s × $0.08 and saves
	// (0.115 − 0.092) $/GiB.
	cheaper := func(g int64) bool {
		return 0.115*float64(g)-(0.092*float64(g)+500*0.08) > 0
	}

	var notCheaper, notProvisionable []int64
	for g := int64(300); g <= 2000; g++ {
		inst := parityInstance("db", g, StorageGP2)
		_, ref := PlanParity(card, perf, e, inst, env, Demand{}, ParityFloorNameplate)
		switch {
		case ref == nil:
			if !cheaper(g) {
				t.Fatalf("%d GiB: accepted, but the rate card says gp3 at parity is not cheaper", g)
			}
		case ref.Code == ReasonParityNoCheaperConfig:
			notCheaper = append(notCheaper, g)
			if cheaper(g) {
				t.Fatalf("%d GiB: refused as not cheaper, but the rate card says it is", g)
			}
		case ref.Code == ReasonParityNotProvisionableBelowThreshold:
			notProvisionable = append(notProvisionable, g)
			if g >= 400 {
				t.Fatalf("%d GiB: refused as not provisionable at or above the 400 GiB threshold", g)
			}
		default:
			t.Fatalf("%d GiB: unexpected refusal %s: %s", g, ref.Code, ref.Reason)
		}
	}
	if len(notCheaper) == 0 || len(notProvisionable) == 0 {
		t.Fatalf("the sweep did not exercise both refusals: %d not-cheaper, %d not-provisionable",
			len(notCheaper), len(notProvisionable))
	}
	lo, hi := notCheaper[0], notCheaper[len(notCheaper)-1]
	if lo != 400 || hi != 1739 {
		t.Errorf("the RDS throughput-parity refusal band is %d–%d GiB, want 400–1,739", lo, hi)
	}
	if int64(len(notCheaper)) != hi-lo+1 {
		t.Errorf("the refusal band is not contiguous: %d sizes across %d–%d", len(notCheaper), lo, hi)
	}
	if notProvisionable[0] != 300 || notProvisionable[len(notProvisionable)-1] != 399 {
		t.Errorf("the not-provisionable range is %d–%d GiB, want 300–399 across this sweep",
			notProvisionable[0], notProvisionable[len(notProvisionable)-1])
	}

	// THE POINT. pkg/ebs's band is 334–375 GiB; this one is 400–1,739. They do
	// not overlap, and inside pkg/ebs's band RDS refuses for a reason pkg/ebs
	// has no name for.
	const ebsLo, ebsHi = 334, 375
	if lo <= ebsHi && ebsLo <= hi {
		t.Fatalf("the RDS band %d–%d overlaps pkg/ebs's %d–%d; the tables are not distinguishable here",
			lo, hi, ebsLo, ebsHi)
	}
	er := ebs.DefaultRates()
	for _, g := range []int64{ebsLo, 350, ebsHi} {
		if _, ref := er.PlanGP3(g, ebs.Demand{}, ebs.FloorGP2Baseline); ref == nil {
			t.Fatalf("pkg/ebs no longer refuses at %d GiB; this test's premise is stale", g)
		}
		_, ref := PlanParity(card, perf, e, parityInstance("db", g, StorageGP2), env,
			Demand{}, ParityFloorNameplate)
		if ref == nil || ref.Code != ReasonParityNotProvisionableBelowThreshold {
			t.Fatalf("%d GiB: RDS refusal = %v, want %s — the same size, a different reason",
				g, ref, ReasonParityNotProvisionableBelowThreshold)
		}
	}
	// And the inverse: at 1,000 GiB pkg/ebs claims a saving where RDS loses
	// money. That is trap 11's exact wording — "would claim a saving in the
	// band where RDS loses throughput".
	if _, ref := er.PlanGP3(1000, ebs.Demand{}, ebs.FloorGP2Baseline); ref != nil {
		t.Fatalf("pkg/ebs no longer converts a 1,000 GiB volume at parity: %v", ref)
	}
	plan, ref := PlanParity(card, perf, e, parityInstance("db", 1000, StorageGP2), env,
		Demand{}, ParityFloorNameplate)
	if ref == nil || ref.Code != ReasonParityNoCheaperConfig {
		t.Fatalf("1,000 GiB: RDS refusal = %v, want %s", ref, ReasonParityNoCheaperConfig)
	}
	t.Logf("1,000 GiB MySQL: pkg/ebs converts at parity; RDS needs %d IOPS / %d MiB/s costing "+
		"$%.2f/mo against gp2's $%.2f/mo and refuses",
		plan.Config.IOPS, plan.Config.ThroughputMBps, plan.ProposedMonthlyUSD, plan.CurrentMonthlyUSD)

	// The band exists only because of the nameplate throughput floor: the same
	// volumes with measurement showing modest I/O convert profitably.
	for _, g := range []int64{400, 1000, 1739} {
		if _, ref := PlanParity(card, perf, e, parityInstance("db", g, StorageGP2), env,
			Demand{IOPS: 800, ThroughputMBps: 40}, ParityFloorMeasured); ref != nil {
			t.Errorf("%d GiB measured: refused (%v), want acceptance", g, ref)
		}
	}
}

// --- the two modification gates ---------------------------------------------

// TestFourModificationsPer24HoursIsACooldown is the documented rate limit:
// "You can perform a maximum of four storage modifications on a DB instance
// within any 24-hour period" [verified].
func TestFourModificationsPer24HoursIsACooldown(t *testing.T) {
	inst := parityInstance("db", 1000, StorageGP3)
	inst.IOPS, inst.StorageThroughputMBps = 20000, 900
	e := mysqlEngine()

	withMods := func(offsets ...time.Duration) Envelopes {
		env := Envelope{Identifier: "db", HistoryKnown: true,
			Storage: []StorageEnvelope{{StorageType: StorageGP3, Known: true,
				MinIOPS: 12000, MaxIOPS: 64000, MinThroughputMBps: 500, MaxThroughputMBps: 4000}}}
		for _, off := range offsets {
			env.StorageModifications = append(env.StorageModifications, testNow.Add(off))
		}
		return NewEnvelopes([]Envelope{env})
	}
	assessWith := func(t *testing.T, envs Envelopes) (*Proposal, []Suppression) {
		t.Helper()
		p, err := NewParity(ParityConfig{Now: testNow, Envelopes: envs, Performance: parityPerf()})
		if err != nil {
			t.Fatal(err)
		}
		prop, sup, ok := p.AssessParity(inst, e, parityIO(400, 200, 30, 10), parityCard())
		if !ok {
			t.Fatal("AssessParity declined to look; every instance must carry a reason")
		}
		return prop, sup
	}

	// Three inside the window: still one modification left, so the reduction
	// stands.
	prop, sup := assessWith(t, withMods(-1*time.Hour, -5*time.Hour, -20*time.Hour))
	if prop == nil {
		t.Fatalf("three modifications in 24 h blocked a proposal: %v", parityCodes(sup))
	}
	if hasParityCode(sup, ReasonParityCooldown) {
		t.Error("the cooldown fired at three modifications")
	}

	// Four inside the window: blocked, and the refusal says when it clears.
	prop, sup = assessWith(t, withMods(-1*time.Hour, -5*time.Hour, -20*time.Hour, -23*time.Hour))
	if prop != nil {
		t.Fatal("a fifth storage modification was proposed inside the 24-hour limit")
	}
	if !hasParityCode(sup, ReasonParityCooldown) {
		t.Fatalf("suppressions = %v, want %s", parityCodes(sup), ReasonParityCooldown)
	}
	// The cooldown fires ALONE among the parity refusals: a change AWS will
	// reject is not a change worth pricing.
	if len(sup) != 1 {
		t.Errorf("the cooldown carried %d suppressions (%v); a blocked modification is the whole "+
			"finding", len(sup), parityCodes(sup))
	}

	// The fourth just outside the window: three remain inside, so it clears.
	prop, _ = assessWith(t, withMods(-1*time.Hour, -5*time.Hour, -20*time.Hour,
		-StorageModificationWindow-time.Minute))
	if prop == nil {
		t.Fatal("a modification older than 24 hours was still counted against the limit")
	}

	// The arithmetic itself, independent of the engine.
	v := withMods(-1*time.Hour, -2*time.Hour, -3*time.Hour, -4*time.Hour).Get("db").Cooldown(testNow)
	if !v.Known || v.Recent != 4 || !v.Blocked {
		t.Fatalf("cooldown = %+v, want 4 recent and blocked", v)
	}
	if want := testNow.Add(-4 * time.Hour).Add(StorageModificationWindow); !v.ClearsAt.Equal(want) {
		t.Errorf("clears at %s, want %s (24 h after the OLDEST of the four)", v.ClearsAt, want)
	}
	// An unknown history never clears the cooldown: silence is not evidence.
	unknown := Envelope{Identifier: "db"}.Cooldown(testNow)
	if unknown.Known || unknown.Blocked {
		t.Errorf("an unread event history reported %+v; it must report Known=false", unknown)
	}
}

// TestStorageOptimizationStateBlocks is the other documented gate: "You can't
// modify allocated storage if the DB instance status is storage-optimization"
// [verified], and an instance mid-modification is not describing its steady
// state anyway.
func TestStorageOptimizationStateBlocks(t *testing.T) {
	e := mysqlEngine()
	p, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db"),
		Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{StatusStorageOptimization, StatusModifying, "Storage-Optimization"} {
		inst := parityInstance("db", 1000, StorageGP3)
		inst.IOPS, inst.StorageThroughputMBps = 20000, 900
		inst.Status = status
		prop, sup, ok := p.AssessParity(inst, e, parityIO(400, 200, 30, 10), parityCard())
		if !ok {
			t.Fatalf("%s: AssessParity declined to look", status)
		}
		if prop != nil {
			t.Fatalf("%s: a storage modification was proposed on an instance that cannot accept one",
				status)
		}
		if !hasParityCode(sup, ReasonParityStorageOptimization) {
			t.Fatalf("%s: suppressions = %v, want %s", status, parityCodes(sup),
				ReasonParityStorageOptimization)
		}
		if len(sup) != 1 {
			t.Errorf("%s: the block carried %d suppressions (%v); it is the whole finding",
				status, len(sup), parityCodes(sup))
		}
	}
	// An available instance is not blocked, so the gate is not "reject
	// everything" — which would be a gate that never has to be right.
	ok := parityInstance("db", 1000, StorageGP3)
	ok.IOPS, ok.StorageThroughputMBps = 20000, 900
	if prop, sup, _ := p.AssessParity(ok, e, parityIO(400, 200, 30, 10), parityCard()); prop == nil {
		t.Fatalf("an available instance was blocked: %v", parityCodes(sup))
	}
}

// --- the envelope -----------------------------------------------------------

// TestProvisioningEnvelopeIsReadNeverHardcoded is §2.4's instruction taken
// literally: AWS states two contradictory gp3 ceilings, so neither is in this
// package and an unread envelope is a refusal.
func TestProvisioningEnvelopeIsReadNeverHardcoded(t *testing.T) {
	e := mysqlEngine()
	inst := parityInstance("db", 1000, StorageGP3)
	inst.IOPS, inst.StorageThroughputMBps = 30000, 1200

	// No envelope at all: the reduction is refused by name.
	p, err := NewParity(ParityConfig{Now: testNow, Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	prop, sup, _ := p.AssessParity(inst, e, parityIO(9000, 5000, 400, 200), parityCard())
	if prop != nil {
		t.Fatal("a provisioning proposal was made without reading the live envelope")
	}
	if !hasParityCode(sup, ReasonParityEnvelopeUnknown) {
		t.Fatalf("suppressions = %v, want %s", parityCodes(sup), ReasonParityEnvelopeUnknown)
	}
	// The refusal quotes both contradictory ceilings, because "not implemented"
	// and "AWS contradicts itself" are different things to a reader.
	reason := parityReasonFor(sup, ReasonParityEnvelopeUnknown)
	for _, want := range []string{"80,000", "64,000", "16,000"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the envelope refusal does not name the %s ceiling: %q", want, reason)
		}
	}

	// The neither-hardcoded property, stated structurally: no source file in
	// this package may contain either published SQL Server gp3 ceiling.
	_, src := packageFiles(t)
	for name, body := range src {
		code := identifiers(t, name, body)
		for _, banned := range []string{"80000", "16000"} {
			if strings.Contains(code, banned) {
				t.Errorf("%s contains the literal %s outside a comment; §2.4's contradictory gp3 "+
					"ceilings must come from DescribeValidDBInstanceModifications, never from here",
					name, banned)
			}
		}
	}

	// With the envelope read, the same instance produces a proposal inside it.
	p2, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db"),
		Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	prop, sup, _ = p2.AssessParity(inst, e, parityIO(9000, 5000, 400, 200), parityCard())
	if prop == nil {
		t.Fatalf("a reduction inside the envelope was refused: %v", parityCodes(sup))
	}
	if prop.IOPS < 12000 || prop.IOPS > 64000 {
		t.Errorf("proposed %d IOPS, outside the envelope's 12,000–64,000", prop.IOPS)
	}

	// A demand beyond the envelope has no gp3 form at all — and the ceiling
	// that refuses it is the LIVE one, not a published guess.
	tight := NewEnvelopes([]Envelope{{
		Identifier: "db", HistoryKnown: true,
		Storage: []StorageEnvelope{{StorageType: StorageGP3, Known: true,
			MinIOPS: 12000, MaxIOPS: 16000, MinThroughputMBps: 500, MaxThroughputMBps: 1000}},
	}})
	p3, err := NewParity(ParityConfig{Now: testNow, Envelopes: tight, Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	prop, sup, _ = p3.AssessParity(inst, e, parityIO(20000, 10000, 400, 200), parityCard())
	if prop != nil {
		t.Fatal("a configuration outside the live envelope was proposed")
	}
	if !hasParityCode(sup, ReasonParityExceedsEnvelope) {
		t.Fatalf("suppressions = %v, want %s", parityCodes(sup), ReasonParityExceedsEnvelope)
	}
}

// TestEnvelopeCollectorDegradesRatherThanBreaks pins the optional-seam rule
// every shipped domain follows: a seam that fails produces a weaker report,
// never no report — and never a hardcoded ceiling in place of the answer.
func TestEnvelopeCollectorDegradesRatherThanBreaks(t *testing.T) {
	boom := errors.New("AccessDenied")
	f := &EnvelopeFixture{
		Options: map[string][]ValidStorageOptionRecord{
			"good": {{StorageType: StorageGP3, MinIOPS: 12000, MaxIOPS: 64000,
				MinStorageThroughputMBps: 500, MaxStorageThroughputMBps: 4000}},
			"nomods": {{StorageType: StorageGP3, MinIOPS: 12000, MaxIOPS: 64000,
				MinStorageThroughputMBps: 500, MaxStorageThroughputMBps: 4000}},
			"noevents": {{StorageType: StorageGP3, MinIOPS: 12000, MaxIOPS: 64000,
				MinStorageThroughputMBps: 500, MaxStorageThroughputMBps: 4000}},
		},
		Events: map[string][]EventRecord{
			"good": {
				{SourceIdentifier: "good", SourceType: EventSourceDBInstance, Date: testNow.Add(-2 * time.Hour),
					Message: "Applying modification to allocated storage", Categories: []string{"configuration change"}},
				{SourceIdentifier: "good", SourceType: EventSourceDBInstance, Date: testNow.Add(-3 * time.Hour),
					Message: "Backing up DB instance", Categories: []string{"backup"}},
				{SourceIdentifier: "good", SourceType: EventSourceDBInstance, Date: testNow.Add(-4 * time.Hour),
					Message: "Finished applying modification to Provisioned IOPS"},
			},
		},
		OptionsErr: map[string]error{"denied": boom},
		EventsErr:  map[string]error{"noevents": boom},
		PageSize:   2,
	}
	c := NewEnvelopeCollector(f, EnvelopeCollectorConfig{
		Window: Window{Start: testNow.Add(-7 * 24 * time.Hour), End: testNow}})
	envs, err := c.Collect(context.Background(), []string{"good", "denied", "noevents", "nomods", "good", " "})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if envs.Len() != 4 {
		t.Fatalf("collected %d envelopes, want 4 (duplicates and blanks dropped)", envs.Len())
	}
	// Sorted, always: nothing derived from an Envelopes may depend on map order.
	ids := make([]string, 0, envs.Len())
	for _, e := range envs.All() {
		ids = append(ids, e.Identifier)
	}
	if !sort.StringsAreSorted(ids) {
		t.Errorf("envelopes are not in identifier order: %v", ids)
	}

	good := envs.Get("good")
	if e := good.For(StorageGP3); !e.Known || e.MaxIOPS != 64000 || e.MaxThroughputMBps != 4000 {
		t.Errorf("good: envelope %+v", e)
	}
	if len(good.StorageModifications) != 2 || !good.HistoryKnown {
		t.Errorf("good: %d storage modifications (historyKnown %v), want 2 — the backup is not one",
			len(good.StorageModifications), good.HistoryKnown)
	}
	if !sort.SliceIsSorted(good.StorageModifications, func(i, j int) bool {
		return good.StorageModifications[i].Before(good.StorageModifications[j])
	}) {
		t.Error("storage modifications are not in ascending order")
	}

	denied := envs.Get("denied")
	if denied.For(StorageGP3).Known {
		t.Error("a denied DescribeValidDBInstanceModifications produced a known envelope")
	}
	if !envs.Get("noevents").For(StorageGP3).Known {
		t.Error("a denied DescribeEvents lost the envelope too; the two seams degrade independently")
	}
	if envs.Get("noevents").HistoryKnown {
		t.Error("a denied DescribeEvents reported a known history")
	}
	if envs.Get("nomods").HistoryKnown != true || len(envs.Get("nomods").StorageModifications) != 0 {
		t.Error("an instance with no events must report a KNOWN, empty history")
	}
	if len(envs.Warnings) != 2 {
		t.Errorf("warnings = %v, want one per failed seam", envs.Warnings)
	}
	if !sort.StringsAreSorted(envs.Warnings) {
		t.Errorf("warnings are not sorted: %v", envs.Warnings)
	}

	// A nil seam is legal and refuses loudly rather than guessing.
	nilc := NewEnvelopeCollector(nil, EnvelopeCollectorConfig{})
	envs, err = nilc.Collect(context.Background(), []string{"good"})
	if err != nil || envs.Len() != 0 || len(envs.Warnings) != 1 {
		t.Fatalf("nil seam: %d envelopes, warnings %v, err %v", envs.Len(), envs.Warnings, err)
	}
	if envs.Get("good").For(StorageGP3).Known {
		t.Error("a nil seam produced a known envelope")
	}
	// Cancellation is honoured.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Collect(ctx, []string{"good"}); err == nil {
		t.Error("Collect ignored a cancelled context")
	}
}

// --- money ------------------------------------------------------------------

// TestParityCostSumIsShuffleInvariant is PR#27's bug as a standing test: float
// addition is not associative, so a total accumulated in arrival order changes
// with arrival order. SumUSD sorts first, and this walks every permutation of a
// four-part bill to prove the last bit never moves.
func TestParityCostSumIsShuffleInvariant(t *testing.T) {
	parts := []CostPart{
		{Name: "storage", USD: 0.1},
		{Name: "iops", USD: 0.2},
		{Name: "throughput", USD: 0.3},
		{Name: "backup", USD: 0.7},
	}
	want := SumUSD(parts)
	naive := func(in []CostPart) float64 {
		var s float64
		for _, p := range in {
			s += p.USD
		}
		return s
	}
	perms, naiveResults := 0, map[float64]bool{}
	var walk func(cur, rest []CostPart)
	walk = func(cur, rest []CostPart) {
		if len(rest) == 0 {
			perms++
			naiveResults[naive(cur)] = true
			if got := SumUSD(cur); got != want {
				t.Fatalf("permutation %v summed to %.17g, want %.17g", parityPartNames(cur), got, want)
			}
			return
		}
		for i := range rest {
			next := append(append([]CostPart(nil), rest[:i]...), rest[i+1:]...)
			walk(append(append([]CostPart(nil), cur...), rest[i]), next)
		}
	}
	walk(nil, parts)
	if perms != 24 {
		t.Fatalf("walked %d permutations, want 24", perms)
	}
	// The premise: naive accumulation really does differ across those same
	// permutations, so the sort is load bearing rather than decorative.
	if len(naiveResults) < 2 {
		t.Fatal("naive accumulation is order-independent for this fixture; the test no longer " +
			"exercises the bug PR#27 shipped")
	}
	t.Logf("naive accumulation produced %d distinct totals across 24 permutations; SumUSD produced one",
		len(naiveResults))

	// Non-finite parts contribute zero rather than poisoning the total.
	if got := SumUSD([]CostPart{{Name: "a", USD: math.NaN()}, {Name: "b", USD: 5}}); got != 5 {
		t.Errorf("SumUSD with a NaN part = %v, want 5", got)
	}
}

// TestShippedParityRatesCannotClaim is U11 §5.1's rule extended to this unit:
// every shipped rate is unverified, an unverified rate may size a fact and may
// never become a saving, and the shipped card therefore produces refusals.
func TestShippedParityRatesCannotClaim(t *testing.T) {
	d := DefaultPerformanceRates()
	if d.Provenance != RateUnverified {
		t.Errorf("shipped parity rates carry provenance %q; §7 records these figures as unverified",
			d.Provenance)
	}
	if d.Provenance.Claimable() {
		t.Error("the shipped parity rates are claimable; an unverified rate may never become a saving")
	}
	if err := d.Validate(); err != nil {
		t.Errorf("the shipped parity rates do not validate: %v", err)
	}
	for _, bad := range []PerformanceRates{
		{ProvisionedIOPSMonthUSD: 0, ProvisionedThroughputMonthUSD: 1, Provenance: RateOperator},
		{ProvisionedIOPSMonthUSD: 1, ProvisionedThroughputMonthUSD: -1, Provenance: RateOperator},
		{ProvisionedIOPSMonthUSD: math.Inf(1), ProvisionedThroughputMonthUSD: 1, Provenance: RateOperator},
		{ProvisionedIOPSMonthUSD: 1, ProvisionedThroughputMonthUSD: 1},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want a rejection", bad)
		}
	}

	// End to end on the SHIPPED card, where gp2 and gp3 cost the same per GiB:
	// a conversion is never cheaper, and a reduction that IS cheaper still
	// cannot be claimed.
	inst := parityInstance("db", 1000, StorageGP3)
	inst.IOPS, inst.StorageThroughputMBps = 20000, 900
	p, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db")})
	if err != nil {
		t.Fatal(err)
	}
	prop, sup, _ := p.AssessParity(inst, mysqlEngine(), parityIO(400, 200, 30, 10), DefaultRates())
	if prop != nil {
		t.Fatalf("an unverified rate produced a claimed saving of %s/mo",
			fmtUSD(prop.NetSavingsMonthlyUSD))
	}
	if !hasParityCode(sup, ReasonUnverifiedRate) {
		t.Fatalf("suppressions = %v, want U11's %s reused rather than a second name for one fact",
			parityCodes(sup), ReasonUnverifiedRate)
	}
	// It still SIZES the opportunity — that is the whole point of the rule.
	if r := parityReasonFor(sup, ReasonUnverifiedRate); !strings.Contains(r, "$") {
		t.Errorf("the unverified-rate refusal does not size the opportunity: %q", r)
	}
	// A gp2 conversion under the shipped card refuses on price, not provenance.
	conv := parityInstance("db", 1000, StorageGP2)
	if _, sup, _ := p.AssessParity(conv, mysqlEngine(), parityIO(400, 200, 30, 10), DefaultRates()); //nolint
	!hasParityCode(sup, ReasonParityNoCheaperConfig) {
		t.Errorf("gp2 → gp3 at equal $/GiB: suppressions = %v, want %s",
			parityCodes(sup), ReasonParityNoCheaperConfig)
	}
}

// --- evidence and confidence ------------------------------------------------

// TestParityConfidenceIsEarnedNotLost pins the pkg/ec2 / pkg/lambda structure
// FINDINGS §7.3 named: a score that starts at zero and adds only what the
// evidence earns, so a missing signal can never be mistaken for a present one.
func TestParityConfidenceIsEarnedNotLost(t *testing.T) {
	e := mysqlEngine()
	inst := parityInstance("db", 1000, StorageGP3)
	inst.IOPS, inst.StorageThroughputMBps = 20000, 900

	full, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db"),
		Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	prop, sup, _ := full.AssessParity(inst, e, parityIO(400, 200, 30, 10), parityCard())
	if prop == nil {
		t.Fatalf("full evidence refused: %v", parityCodes(sup))
	}
	if prop.Confidence <= 0 || prop.Confidence > 1 {
		t.Fatalf("confidence %v is outside (0,1]", prop.Confidence)
	}

	// Strip the window down and the score must FALL, not stay put: nothing is
	// earned by a series that did not cover the minimum.
	short := []Series{}
	for _, s := range parityIO(400, 200, 30, 10) {
		s.Points = s.Points[:2] // two datapoints ⇒ a few hours of span
		short = append(short, s)
	}
	weak, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db"),
		Performance: parityPerf(), MinWindow: 72 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	prop2, sup2, _ := weak.AssessParity(inst, e, short, parityCard())
	if prop2 != nil {
		t.Fatal("a reduction was proposed from a window too short to contain a peak")
	}
	if !hasParityCode(sup2, ReasonParityWindowTooShort) {
		t.Fatalf("suppressions = %v, want %s", parityCodes(sup2), ReasonParityWindowTooShort)
	}

	// The factor structure itself, and the "names what would fix it" property.
	var c ParityConfidence
	c.add("io-measurement", 0.35, 1, "complete")
	c.add("window", 0.25, 0.2, "observed 6h against a 72h minimum")
	c.add("envelope", 0.20, 1, "read")
	c.add("headroom", 0.20, 1, "ample")
	if want := 0.35*1 + 0.25*0.2 + 0.20*1 + 0.20*1; math.Abs(c.Score-want) > 1e-9 {
		t.Errorf("score = %v, want %v (weights × earned, summed)", c.Score, want)
	}
	if !strings.HasPrefix(c.WeakestFactor(), "window: ") {
		t.Errorf("WeakestFactor = %q, want the window factor", c.WeakestFactor())
	}
	if (ParityConfidence{}).WeakestFactor() != "no single dominant factor" {
		t.Error("an empty confidence must not claim a dominant factor")
	}
	// Earned is clamped: a factor cannot earn more than its weight.
	var over ParityConfidence
	over.add("x", 0.5, 99, "why")
	over.add("y", 0.5, math.NaN(), "why")
	if over.Score != 0.5 {
		t.Errorf("score = %v, want 0.5: earned clamps to [0,1] and NaN earns nothing", over.Score)
	}
}

// TestNoMeasurementFallsBackToNameplateAndNeverToZero is the silence rule: a
// metric CloudWatch declined to answer for produces a refusal and a nameplate
// floor, never a low demand figure.
func TestNoMeasurementFallsBackToNameplateAndNeverToZero(t *testing.T) {
	e := mysqlEngine()
	p, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("db"),
		Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	// Every subset that is missing even one series is unknown.
	full := parityIO(400, 200, 30, 10)
	for i := range full {
		partial := append([]Series(nil), full...)
		partial[i].Partial, partial[i].Status = true, StatusTruncated
		m := MeasureIO(partial, DefaultParityPercentile, DefaultParityHeadroom)
		if m.Known {
			t.Fatalf("a truncated %s produced a known measurement", partial[i].Metric)
		}
		if m.Demand != (Demand{}) {
			t.Fatalf("a truncated %s produced demand %+v; silence is not a small number",
				partial[i].Metric, m.Demand)
		}
		if len(m.Missing) == 0 {
			t.Fatalf("a truncated %s was not named as missing", partial[i].Metric)
		}
	}
	// A reduction is refused outright without measurement.
	inst := parityInstance("db", 1000, StorageGP3)
	inst.IOPS, inst.StorageThroughputMBps = 20000, 900
	prop, sup, _ := p.AssessParity(inst, e, nil, parityCard())
	if prop != nil {
		t.Fatal("a provisioned-performance cut was proposed with no I/O measurement at all")
	}
	if !hasParityCode(sup, ReasonParityNoMeasurement) {
		t.Fatalf("suppressions = %v, want %s", parityCodes(sup), ReasonParityNoMeasurement)
	}

	// Demand sums the two halves at the percentile and converts bytes to MiB/s.
	m := MeasureIO(parityIO(400, 200, 30, 10), 0.99, 0)
	if math.Abs(m.Raw.IOPS-600) > 1e-9 || math.Abs(m.Raw.ThroughputMBps-40) > 1e-9 {
		t.Errorf("measured %+v, want 600 IOPS and 40 MiB/s (read + write, bytes ÷ MiB)", m.Raw)
	}
	// Headroom is applied on top and rounds demand UP, never down.
	m = MeasureIO(parityIO(400, 200, 30, 10), 0.99, 0.25)
	if math.Abs(m.Demand.IOPS-750) > 1e-9 || math.Abs(m.Demand.ThroughputMBps-50) > 1e-9 {
		t.Errorf("demand with 25%% headroom = %+v, want 750 IOPS and 50 MiB/s", m.Demand)
	}
}

// --- the seam, end to end ---------------------------------------------------

// TestParityWiresIntoTheReservedSeam drives the whole thing through U11's
// Sizer and Report.Validate, which is the only way to know the proposal this
// unit produces is one the shipped gate accepts.
func TestParityWiresIntoTheReservedSeam(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("shrink", "db.r6i.xlarge", "mysql", withStorage(1000, 0, StorageGP3)),
		},
		Metrics: mergeMetrics(
			metricsFor("shrink", 30, 12, 24<<30, 400*GiB),
			map[string][]Point{
				"shrink/" + MetricReadIOPS:        flat(400, 48),
				"shrink/" + MetricWriteIOPS:       flat(200, 48),
				"shrink/" + MetricReadThroughput:  flat(30*MiB, 48),
				"shrink/" + MetricWriteThroughput: flat(10*MiB, 48),
			}),
	}
	f.Instances[0].Iops, f.Instances[0].StorageThroughput = 20000, 900

	par, err := NewParity(ParityConfig{Now: testNow, Envelopes: stripedEnvelope("shrink"),
		Performance: parityPerf()})
	if err != nil {
		t.Fatal(err)
	}
	rep := assess(t, collect(t, f), nil, func(c *Config) {
		c.Rates = parityCard()
		c.Parity = par
	})
	a := must(t, rep, "shrink")
	if a.Proposal == nil {
		t.Fatalf("no proposal: %v", a.Codes())
	}
	// U11's permanent refusals survive: this unit adds a verdict, it does not
	// replace the report.
	wantCode(t, a, ReasonInstanceClassIsAFailover)
	wantCode(t, a, ReasonStorageCannotShrink)
	wantCode(t, a, ReasonParityFloorsAtBaseline)
	// And the seam replaces the U11 placeholder rather than sitting beside it.
	wantNoCode(t, a, ReasonNoStoragePerformanceModel)

	p := a.Proposal
	if p.StorageType != StorageGP3 || p.IOPS != 12000 || p.StorageThroughputMBps != 500 {
		t.Errorf("proposal = %s %d IOPS / %d MiB/s, want gp3 12,000 / 500",
			p.StorageType, p.IOPS, p.StorageThroughputMBps)
	}
	if p.AllocatedStorageGiB != 1000 {
		t.Errorf("proposal names %d GiB of allocated storage; the trap-8 ratchet guard requires the "+
			"observed 1,000", p.AllocatedStorageGiB)
	}
	if p.Action != domain.ActionAdvisory {
		t.Errorf("action = %q; U13 is read-only", p.Action)
	}
	if p.NetSavingsMonthlyUSD != p.GrossSavingsMonthlyUSD {
		t.Errorf("net %v != gross %v; no Reserved DB Instance discounts storage, so the two are the "+
			"same number by construction", p.NetSavingsMonthlyUSD, p.GrossSavingsMonthlyUSD)
	}
	want := 0.02*(20000-12000) + 0.08*(900-500)
	if math.Abs(p.NetSavingsMonthlyUSD-want) > 1e-9 {
		t.Errorf("saving $%.4f/mo, rate card says $%.4f", p.NetSavingsMonthlyUSD, want)
	}
	if rep.Totals.Proposals != 1 || math.Abs(rep.Totals.NetSavingsMonthlyUSD-want) > 1e-9 {
		t.Errorf("totals = %d proposals, $%.4f/mo", rep.Totals.Proposals, rep.Totals.NetSavingsMonthlyUSD)
	}
	// The shipped default still leaves the seam nil — U11's
	// TestStoragePerformanceIsRefusedNotBorrowed depends on it and this unit
	// must not have changed it.
	if DefaultConfig().Parity != nil {
		t.Fatal("U13 wired itself into DefaultConfig; the seam is opt-in")
	}
}

// TestParityReasonCodesAreDistinct extends U11's TestReasonCodesAreDistinct to
// this unit's codes, including against U11's, because two codes with one value
// silently merge two findings in every roll-up.
func TestParityReasonCodesAreDistinct(t *testing.T) {
	all := map[string]string{
		"ReasonParityStorageTypeNotModelled":         ReasonParityStorageTypeNotModelled,
		"ReasonParitySizeUnusable":                   ReasonParitySizeUnusable,
		"ReasonParityGP2BandUnpublished":             ReasonParityGP2BandUnpublished,
		"ReasonParityNotProvisionableBelowThreshold": ReasonParityNotProvisionableBelowThreshold,
		"ReasonParityEnvelopeUnknown":                ReasonParityEnvelopeUnknown,
		"ReasonParityExceedsEnvelope":                ReasonParityExceedsEnvelope,
		"ReasonParityNoCheaperConfig":                ReasonParityNoCheaperConfig,
		"ReasonParityFloorsAtBaseline":               ReasonParityFloorsAtBaseline,
		"ReasonParityNoMeasurement":                  ReasonParityNoMeasurement,
		"ReasonParityWindowTooShort":                 ReasonParityWindowTooShort,
		"ReasonParityStorageOptimization":            ReasonParityStorageOptimization,
		"ReasonParityCooldown":                       ReasonParityCooldown,
		"ReasonParityLowConfidence":                  ReasonParityLowConfidence,
	}
	// Every U11 code, so a collision across units is caught here.
	u11 := []string{
		ReasonAuroraNotSupported, ReasonClusterMemberNotSupported, ReasonModeOff, ReasonUnknownEngine,
		ReasonUnknownInstanceClass, ReasonEngineNotPriced, ReasonUnknownDeployment, ReasonUnverifiedRate,
		ReasonInstanceClassIsAFailover, ReasonFreeableMemoryIsPageCache, ReasonBufferPoolScalesWithClass,
		ReasonMemorySemanticsUnencoded, ReasonStorageCannotShrink, ReasonStorageAutoscalingRatchet,
		ReasonReplicaIsFailoverCapacity, ReasonMultiAZIsAvailabilityPosture, ReasonInsufficientWindow,
		ReasonNoMetricEvidence, ReasonTruncatedMetrics, ReasonSizeFlexibilityExcluded,
		ReasonInstanceStateUnstable, ReasonNoStoragePerformanceModel,
	}
	seen := map[string]string{}
	for _, c := range u11 {
		seen[c] = "a U11 code"
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic failure output
	for _, name := range names {
		code := all[name]
		if code == "" {
			t.Errorf("%s is empty", name)
			continue
		}
		if code != strings.ToLower(code) || strings.ContainsAny(code, " _") {
			t.Errorf("%s = %q: codes are lower-case and hyphenated so they are safe to store and group on",
				name, code)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("%s collides with %s on %q", name, prev, code)
		}
		seen[code] = name
	}
}

// --- helpers ----------------------------------------------------------------

func hasParityCode(sup []Suppression, code string) bool {
	for _, s := range sup {
		if s.Code == code {
			return true
		}
	}
	return false
}

func parityCodes(sup []Suppression) []string {
	out := make([]string, 0, len(sup))
	for _, s := range sup {
		out = append(out, s.Code)
	}
	return out
}

func parityReasonFor(sup []Suppression, code string) string {
	for _, s := range sup {
		if s.Code == code {
			return s.Reason
		}
	}
	return ""
}

func parityPartNames(in []CostPart) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.Name)
	}
	return out
}

// TestParityReportIsShuffleInvariant is TestReportIsShuffleInvariant with the
// U13 seam wired in: the same account, the instances, the metric datapoints
// and the collected ENVELOPES all handed over in a different order every time,
// and the rendered report asserted byte-identical.
//
// This is PR#27's bug as a standing test. Float addition is not associative, so
// the moment a total is accumulated in arrival order — across cost parts,
// across advisories, across the fleet — the number changes with the order the
// account happened to be walked in. Sorting before summing is what makes two
// runs of the same report comparable, and nothing else does.
func TestParityReportIsShuffleInvariant(t *testing.T) {
	instances := []DBInstanceRecord{
		rec("alpha", "db.r6i.xlarge", "mysql", withStorage(1000, 0, StorageGP3)),
		rec("bravo", "db.r6i.xlarge", "postgres", withStorage(2000, 0, StorageGP3)),
		rec("charlie", "db.m6i.large", "mysql", withStorage(350, 0, StorageGP2)),
		rec("delta", "db.m6i.large", "postgres", withStorage(1000, 0, StorageGP2)),
		rec("echo", "db.r6i.large", "mariadb", withStorage(600, 0, StorageGP3),
			withStatus(StatusStorageOptimization)),
		rec("foxtrot", "db.r6i.large", "postgres", withStorage(800, 0, StorageIO1)),
		// golf has NO I/O series, so it falls back to the nameplate floor —
		// which is where the 400–1,739 GiB throughput-parity refusal band is.
		rec("golf", "db.m6i.large", "mysql", withStorage(1000, 0, StorageGP2)),
	}
	instances[0].Iops, instances[0].StorageThroughput = 20000, 900
	instances[1].Iops, instances[1].StorageThroughput = 30000, 1200
	instances[4].Iops, instances[4].StorageThroughput = 18000, 700

	ids := make([]string, 0, len(instances))
	metrics := map[string][]Point{}
	for i, in := range instances {
		ids = append(ids, in.DBInstanceIdentifier)
		for k, v := range metricsFor(in.DBInstanceIdentifier, 30, 12, 24<<30, 300*GiB) {
			metrics[k] = v
		}
		id := in.DBInstanceIdentifier
		if id == "golf" {
			continue
		}
		metrics[id+"/"+MetricReadIOPS] = flat(float64(300+50*i), 48)
		metrics[id+"/"+MetricWriteIOPS] = flat(float64(150+25*i), 48)
		metrics[id+"/"+MetricReadThroughput] = flat(float64(20+i)*MiB, 48)
		metrics[id+"/"+MetricWriteThroughput] = flat(float64(8+i)*MiB, 48)
	}

	var want []byte
	for pi, order := range permutations(len(instances)) {
		f := &Fixture{
			Instances: permute(instances, order),
			Metrics:   metrics,
			PageSize:  1 + pi,
		}
		// The envelopes arrive in a different order too, and NewEnvelopes is
		// the only thing standing between that and the output.
		par, err := NewParity(ParityConfig{
			Now:         testNow,
			Envelopes:   stripedEnvelopePermuted(permute(ids, order)),
			Performance: parityPerf(),
		})
		if err != nil {
			t.Fatal(err)
		}
		rep := assess(t, collect(t, f), nil, func(c *Config) {
			c.Rates = parityCard()
			c.Parity = par
		})

		var buf bytes.Buffer
		if err := rep.WriteText(&buf); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		j, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got := append(buf.Bytes(), j...)
		if want == nil {
			want = got
			if rep.Totals.Proposals == 0 {
				t.Fatal("the fixture produced no proposals; a shuffle test over zero money proves nothing")
			}
			if rep.Totals.NetSavingsMonthlyUSD <= 0 {
				t.Fatal("the fixture produced no summed savings; there is no total to shuffle")
			}
			t.Logf("%d proposals totalling %s/mo across %d instances",
				rep.Totals.Proposals, fmtUSD(rep.Totals.NetSavingsMonthlyUSD), rep.Totals.Instances)
			// The fixture is only a fair shuffle test if it covers the paths:
			// a reduction, a conversion that refuses, a blocked state and a
			// storage type this unit does not model.
			wantCode(t, must(t, rep, "alpha"), ReasonParityFloorsAtBaseline)
			wantCode(t, must(t, rep, "delta"), ReasonParityFloorsAtBaseline)
			wantCode(t, must(t, rep, "golf"), ReasonParityNoMeasurement)
			wantCode(t, must(t, rep, "golf"), ReasonParityNoCheaperConfig)
			wantCode(t, must(t, rep, "echo"), ReasonParityStorageOptimization)
			wantCode(t, must(t, rep, "foxtrot"), ReasonParityStorageTypeNotModelled)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("permutation %d rendered a different report; input ORDER reached the output", pi)
		}
	}
}

func stripedEnvelopePermuted(ids []string) Envelopes {
	out := make([]Envelope, 0, len(ids))
	for _, id := range ids {
		out = append(out, Envelope{
			Identifier: id, HistoryKnown: true,
			Storage: []StorageEnvelope{
				// Deliberately out of type order, so normalizeStorageEnvelopes
				// is the thing that fixes it rather than the caller.
				{StorageType: StorageIO2, Known: true, MinIOPS: 1000, MaxIOPS: 256000},
				{StorageType: StorageGP3, Known: true, MinIOPS: 12000, MaxIOPS: 64000,
					MinThroughputMBps: 500, MaxThroughputMBps: 4000},
			},
		})
	}
	return NewEnvelopes(out)
}

// TestParityRefusesWhatItDoesNotModel covers the two refusals that are about
// the INPUT rather than about the arithmetic: a storage type this unit has no
// price function for, and a size outside the published table. Both are stated
// by name, because "we do not model io2" and "we looked and found nothing" are
// different facts to a reader.
func TestParityRefusesWhatItDoesNotModel(t *testing.T) {
	card, perf, e := parityCard(), parityPerf(), mysqlEngine()
	env := stripedEnvelope("db").Get("db")

	for _, st := range []string{StorageIO1, StorageIO2, StorageMagnetic, "", "gp4"} {
		inst := parityInstance("db", 1000, st)
		_, ref := PlanParity(card, perf, e, inst, env, Demand{IOPS: 100}, ParityFloorMeasured)
		if ref == nil || ref.Code != ReasonParityStorageTypeNotModelled {
			t.Errorf("%q storage: refusal = %v, want %s", st, ref, ReasonParityStorageTypeNotModelled)
		}
	}
	for _, size := range []int64{0, -1, MaxParitySizeGiB + 1, 1 << 40} {
		inst := parityInstance("db", size, StorageGP2)
		_, ref := PlanParity(card, perf, e, inst, env, Demand{IOPS: 100}, ParityFloorMeasured)
		if ref == nil || ref.Code != ReasonParitySizeUnusable {
			t.Errorf("%d GiB: refusal = %v, want %s", size, ref, ReasonParitySizeUnusable)
		}
	}
	// A broken measurement is refused rather than clamped: it means the
	// caller's arithmetic failed, and a failed measurement must not become a
	// provisioning decision.
	for _, d := range []Demand{{IOPS: math.NaN()}, {ThroughputMBps: math.Inf(1)}, {IOPS: -1}} {
		inst := parityInstance("db", 1000, StorageGP2)
		if _, ref := PlanParity(card, perf, e, inst, env, d, ParityFloorMeasured); ref == nil ||
			ref.Code != ReasonParityNoMeasurement {
			t.Errorf("demand %+v: refusal = %v, want %s", d, ref, ReasonParityNoMeasurement)
		}
	}
	// And an engine with no encoded striping threshold, which is neither of
	// the above and must not fall through to MySQL's table.
	inst := parityInstance("db", 1000, StorageGP2)
	if _, ref := PlanParity(card, perf, ParseEngine("greatdb", ""), inst, env,
		Demand{IOPS: 100}, ParityFloorMeasured); ref == nil ||
		ref.Code != ReasonParityGP2BandUnpublished {
		t.Errorf("unknown engine: refusal = %v, want %s", ref, ReasonParityGP2BandUnpublished)
	}
}
