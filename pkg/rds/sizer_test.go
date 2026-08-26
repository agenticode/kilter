package rds

import (
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// An idle instance is REPORTED with its full monthly cost and never resized —
// and a replica with the same signature is reported distinctly.
//
// §1 invariant 5 forbids idle-instance shutdown outright, because RDS
// instances are data-bearing. So the product here is the dollar figure and the
// question it raises, not an action.
func TestIdleInstanceIsReportedNeverResized(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("quiet", "db.r6i.xlarge", "postgres"),
			rec("quiet-replica", "db.r6i.xlarge", "postgres", withReplicaOf("quiet")),
			rec("busy", "db.r6i.xlarge", "postgres"),
			rec("working", "db.r6i.xlarge", "postgres"), // no connections, but burning CPU
		},
		Metrics: mergeMetrics(
			metricsFor("quiet", 0.4, 0, 24<<30, 40*GiB),
			metricsFor("quiet-replica", 0.3, 0, 24<<30, 40*GiB),
			metricsFor("busy", 40, 25, 24<<30, 40*GiB),
			metricsFor("working", 55, 0, 24<<30, 40*GiB),
		),
	}
	rep := assess(t, collect(t, f), nil)

	quiet := must(t, rep, "quiet")
	if !quiet.Idle.Known || !quiet.Idle.Idle {
		t.Fatalf("zero DatabaseConnections across the window was not read as idle: %+v", quiet.Idle)
	}
	ad := wantAdvisory(t, quiet, AdvisoryIdleInstance)
	if ad.MonthlyUSD != quiet.CurrentMonthlyUSD || ad.MonthlyUSD <= 0 {
		t.Errorf("the idle advisory carries %v, want the full monthly cost %v",
			ad.MonthlyUSD, quiet.CurrentMonthlyUSD)
	}
	if !strings.Contains(ad.Caveat, "not a stoppable one") {
		t.Errorf("the idle advisory does not say why shutdown is not on the table: %s", ad.Caveat)
	}
	if quiet.Proposal != nil {
		t.Error("an idle instance carries a class proposal")
	}

	// A replica gets a DIFFERENT code: its size is a failover-capacity
	// decision, so "unused" is a question about whether it should exist.
	replica := must(t, rep, "quiet-replica")
	wantAdvisory(t, replica, AdvisoryIdleReadReplica)
	if replica.Advised(AdvisoryIdleInstance) {
		t.Error("an idle read replica was reported as an idle primary")
	}
	wantCode(t, replica, ReasonReplicaIsFailoverCapacity)
	wantNoCode(t, quiet, ReasonReplicaIsFailoverCapacity)

	// A busy instance is not idle, and neither is a connection-less one that
	// is burning CPU: autovacuum and replication apply are real work.
	busy := must(t, rep, "busy")
	if busy.Idle.Idle {
		t.Error("an instance with 25 connections was called idle")
	}
	working := must(t, rep, "working")
	if working.Idle.Idle {
		t.Errorf("an instance at %.0f%% CPU with no connections was called idle; non-zero CPU on a "+
			"connection-less database is work this domain cannot see", working.Idle.PeakCPUPercent)
	}

	if rep.Totals.Idle != 2 || rep.Totals.IdleReplicas != 1 {
		t.Errorf("totals: idle=%d idleReplicas=%d, want 2 and 1", rep.Totals.Idle, rep.Totals.IdleReplicas)
	}
}

// Silence is not evidence of quiet. A truncated DatabaseConnections series
// must never produce an idle verdict — this is the fifth independent
// derivation of the CloudWatch truth that a missing result is truncation.
func TestTruncatedMetricsNeverProduceAnIdleVerdict(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("dark", "db.r6i.xlarge", "postgres")},
		// No recorded metrics at all AND a dropped result: CloudWatch was not
		// asked, or did not answer.
		DropResults: len(collectedMetrics),
	}
	rep := assess(t, collect(t, f), nil)
	a := must(t, rep, "dark")

	if a.Idle.Idle || a.Idle.Known {
		t.Fatalf("a truncated response produced an idle verdict: %+v", a.Idle)
	}
	if a.Advised(AdvisoryIdleInstance) {
		t.Fatal("an instance CloudWatch did not answer for was reported as idle")
	}
	wantCode(t, a, ReasonTruncatedMetrics)
	wantNoCode(t, a, ReasonNoMetricEvidence)

	// And with a complete-but-empty answer, the distinction holds the other
	// way: we were told, and there was nothing.
	empty := &Fixture{Instances: []DBInstanceRecord{rec("empty", "db.r6i.xlarge", "postgres")}}
	rep2 := assess(t, collect(t, empty), nil)
	b := must(t, rep2, "empty")
	wantCode(t, b, ReasonNoMetricEvidence)
	wantNoCode(t, b, ReasonTruncatedMetrics)
	if b.Idle.Idle {
		t.Error("an instance with no datapoints was called idle")
	}
}

// An engine or a class this package has not encoded is refused BY NAME, so a
// reader can act on the refusal instead of guessing what happened.
func TestUnknownEngineOrClassRefusesByName(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("weird-engine", "db.r6i.xlarge", "cockroachdb"),
			rec("weird-class", "db.zz9.plural-z-alpha", "postgres"),
			rec("unpriced-class", "db.r8g.48xlarge", "postgres"),
			rec("unpriced-engine", "db.r6i.xlarge", "sqlserver-ee", withLicense(LicenseIncluded)),
		},
		Metrics: mergeMetrics(
			metricsFor("weird-engine", 30, 12, 24<<30, 40*GiB),
			metricsFor("weird-class", 30, 12, 24<<30, 40*GiB),
			metricsFor("unpriced-class", 30, 12, 24<<30, 40*GiB),
			metricsFor("unpriced-engine", 30, 12, 24<<30, 40*GiB),
		),
	}
	rep := assess(t, collect(t, f), nil)

	we := must(t, rep, "weird-engine")
	wantCode(t, we, ReasonUnknownEngine)
	if !strings.Contains(we.Suppressions[0].Reason, "cockroachdb") {
		t.Errorf("the unknown-engine refusal does not name the engine: %s", we.Suppressions[0].Reason)
	}
	if len(we.Suppressions) != 1 {
		t.Errorf("unknown-engine is an exclusion and must fire alone, got %v", we.Codes())
	}

	// A class that does not parse as db.<class>.<size>: no units, no
	// reservation matching, no rate.
	wc := must(t, rep, "weird-class")
	wantCode(t, wc, ReasonUnknownInstanceClass)
	if wc.CostKnown {
		t.Error("an unparseable class was priced")
	}
	if !namesValue(wc, ReasonUnknownInstanceClass, "db.zz9.plural-z-alpha") {
		t.Error("the unknown-class refusal does not name the class")
	}

	// A well-formed class with no shipped rate: refused, not interpolated from
	// its neighbours.
	uc := must(t, rep, "unpriced-class")
	wantCode(t, uc, ReasonUnknownInstanceClass)
	if !namesValue(uc, ReasonUnknownInstanceClass, "interpolated") {
		t.Error("the missing-rate refusal does not say it declined to interpolate")
	}

	// A licensed engine with no shipped rows is a DIFFERENT refusal from a
	// missing class, because a reader acts on the two differently.
	ue := must(t, rep, "unpriced-engine")
	wantCode(t, ue, ReasonEngineNotPriced)
	wantNoCode(t, ue, ReasonUnknownInstanceClass)
	if !namesValue(ue, ReasonEngineNotPriced, "sqlserver-ee") {
		t.Error("the unpriced-engine refusal does not name the engine")
	}
	// It is a refusal, not an exclusion: the storage and memory findings still
	// apply to a SQL Server instance even though its rate does not.
	wantCode(t, ue, ReasonStorageCannotShrink)
	wantCode(t, ue, ReasonMemorySemanticsUnencoded)
}

func namesValue(a Assessment, code, want string) bool {
	for _, s := range a.Suppressions {
		if s.Code == code && strings.Contains(s.Reason, want) {
			return true
		}
	}
	return false
}

// Every assessment states a reason. An instance this domain says nothing about
// is indistinguishable from one it failed to look at.
func TestEveryAssessmentStatesAReason(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("a", "db.r6i.xlarge", "postgres"),
			rec("b", "db.m6i.large", "mysql", withMultiAZ()),
			rec("c", "db.t4g.medium", "mariadb", withStorage(20, 100, StorageGP2)),
			rec("d", "db.r6i.large", "aurora-postgresql", withCluster("cl")),
			rec("e", "db.r6i.large", "oracle-se2", withLicense(LicenseBYOL)),
			rec("f", "db.r6i.large", "postgres", withTags(map[string]string{TagKilterMode: "off"})),
			rec("g", "db.r6i.large", "postgres", withStatus(StatusStorageOptimization)),
			rec("h", "db.r6i.large", "postgres", withReplicaOf("a")),
		},
		Clusters: []DBClusterRecord{{DBClusterIdentifier: "cl", Engine: "aurora-postgresql"}},
		Metrics: mergeMetrics(
			metricsFor("a", 30, 12, 24<<30, 40*GiB),
			metricsFor("b", 30, 12, 4<<30, 40*GiB),
			metricsFor("c", 5, 1, 2<<30, 8*GiB),
			metricsFor("d", 30, 12, 8<<30, 40*GiB),
			metricsFor("e", 30, 12, 8<<30, 40*GiB),
			metricsFor("f", 30, 12, 8<<30, 40*GiB),
			metricsFor("g", 30, 12, 8<<30, 40*GiB),
			metricsFor("h", 30, 0, 8<<30, 40*GiB),
		),
	}
	rep := assess(t, collect(t, f), nil)
	if len(rep.Assessments) != 8 {
		t.Fatalf("got %d assessments, want 8", len(rep.Assessments))
	}
	for _, a := range rep.Assessments {
		if len(a.Suppressions) == 0 {
			t.Errorf("%s says nothing at all", a.Target.ID)
		}
		if len(a.Evidence) == 0 {
			t.Errorf("%s states no evidence", a.Target.ID)
		}
		for _, s := range a.Suppressions {
			if s.Code == "" || s.Reason == "" {
				t.Errorf("%s has an empty suppression %+v", a.Target.ID, s)
			}
			if len(s.Reason) < 40 {
				t.Errorf("%s: refusal %q reads as a label, not an explanation: %q", a.Target.ID, s.Code, s.Reason)
			}
		}
	}

	// The permanent refusal is on every instance this domain actually models.
	for _, id := range []string{"a", "b", "c", "e", "g", "h"} {
		wantCode(t, must(t, rep, id), ReasonInstanceClassIsAFailover)
	}
	// And on none of the excluded ones.
	for _, id := range []string{"d", "f"} {
		a := must(t, rep, id)
		if !a.Excluded() {
			t.Errorf("%s should be excluded", id)
		}
		wantNoCode(t, a, ReasonInstanceClassIsAFailover)
	}

	// The mid-modification instance says its reading is transient.
	wantCode(t, must(t, rep, "g"), ReasonInstanceStateUnstable)

	// Nothing anywhere is proposed, and no saving is claimed.
	if rep.Totals.Proposals != 0 || rep.Totals.NetSavingsMonthlyUSD != 0 ||
		rep.Totals.GrossSavingsMonthlyUSD != 0 {
		t.Fatalf("this domain proposed something: %+v", rep.Totals)
	}
	if rep.Totals.Refusals != len(rep.Assessments) {
		t.Errorf("refusals=%d, want one per assessment (%d)", rep.Totals.Refusals, len(rep.Assessments))
	}

	// The generic seam carries the refusals, because domain.Recommendation
	// cannot express one.
	refs := rep.Refusals()
	if len(refs) == 0 {
		t.Fatal("the report projects no domain.Refusals; those ARE this domain's output")
	}
	for _, r := range refs {
		if r.Code == "" || r.Reason == "" {
			t.Errorf("a projected refusal is missing its code or reason: %+v", r)
		}
	}
}

// The mode=off guardrail excludes an instance from everything, not just from
// proposals — because a proposal is not the only thing an operator might be
// opting out of.
func TestModeOffExcludesEverything(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("off", "db.r6i.xlarge", "postgres", withStorage(4096, 8192, StorageGP2)),
		},
		Tags:    map[string]map[string]string{"arn:aws:rds:us-east-1:1234:db:off": {TagKilterMode: "off"}},
		Metrics: metricsFor("off", 0, 0, 24<<30, 4000*GiB),
	}
	rep := assess(t, collect(t, f), nil)
	a := must(t, rep, "off")
	wantCode(t, a, ReasonModeOff)
	if len(a.Suppressions) != 1 || len(a.Advisories) != 0 {
		t.Errorf("mode=off did not fire alone: %v / %d advisories", a.Codes(), len(a.Advisories))
	}
	if rep.Totals.Excluded != 1 {
		t.Errorf("excluded=%d, want 1", rep.Totals.Excluded)
	}
	// Not even the idle finding, which would otherwise have fired loudly.
	if a.Advised(AdvisoryIdleInstance) || a.Advised(AdvisoryStorageFloor) {
		t.Error("an opted-out instance still produced findings")
	}
}

// An unverified rate sizes a fact and never becomes a saving. This is the
// consequence half of the provenance model.
func TestUnverifiedRatesNeverBecomeASaving(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("u", "db.r6i.xlarge", "postgres")},
		Metrics:   metricsFor("u", 30, 12, 24<<30, 40*GiB),
	}
	rep := assess(t, collect(t, f), nil)
	a := must(t, rep, "u")

	if !a.CostKnown || a.CurrentMonthlyUSD <= 0 {
		t.Fatal("the shipped rate did not size the instance at all; a magnitude is still useful")
	}
	if a.RateProvenance.Claimable() {
		t.Fatalf("a shipped RDS rate reported itself as claimable (%q); §7 marks them unverified",
			a.RateProvenance)
	}
	wantCode(t, a, ReasonUnverifiedRate)
	ad := wantAdvisory(t, a, AdvisoryUnverifiedRate)
	if !strings.Contains(ad.Caveat, "not an invoice") {
		t.Errorf("the unverified-rate advisory does not say what the number is not: %s", ad.Caveat)
	}
	if rep.Totals.Unverified != 1 {
		t.Errorf("totals.Unverified = %d, want 1", rep.Totals.Unverified)
	}

	// Validate refuses a claim built on it, whatever produced the claim.
	bad := *rep
	bad.Assessments = append([]Assessment(nil), rep.Assessments...)
	bad.Assessments[0].Proposal = &Proposal{
		Action: domain.ActionAdvisory, Risk: RiskLow, Confidence: 0.9, Reason: "cheaper",
		GrossSavingsMonthlyUSD: 50, NetSavingsMonthlyUSD: 50, RateProvenance: RateUnverified,
	}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "unverified") {
		t.Fatalf("Validate accepted a saving from an unverified rate: %v", err)
	}

	// With an operator-supplied card, the refusal lapses — that is the
	// intended path to a number Kilter will stand behind.
	rep2 := assess(t, collect(t, f), nil, func(c *Config) {
		c.Rates = operatorRates(t, map[string]float64{"open-source|db.r6i.xlarge": 0.48})
	})
	b := must(t, rep2, "u")
	if !b.RateProvenance.Claimable() {
		t.Fatalf("an operator-supplied rate is still not claimable (%q)", b.RateProvenance)
	}
	wantNoCode(t, b, ReasonUnverifiedRate)

	// And a MIXED card — verified class rates over the shipped, unverified
	// $/GiB-month figures — still refuses. A target is only as trustworthy as
	// the weakest rate that priced any part of it.
	mixed := DefaultRates().Merge(mustLoadRates(t,
		`{"classes":{"open-source|db.r6i.xlarge":{"singleAZHourlyUSD":0.48}}}`))
	rep3 := assess(t, collect(t, f), nil, func(c *Config) { c.Rates = mixed })
	c := must(t, rep3, "u")
	if !c.RateProvenance.Claimable() {
		t.Fatalf("the instance line should be claimable under the operator override, got %q",
			c.RateProvenance)
	}
	if c.Storage.RateProvenance.Claimable() {
		t.Fatalf("the storage line should still be unverified, got %q", c.Storage.RateProvenance)
	}
	if got := c.WorstRateProvenance(); got.Claimable() {
		t.Fatalf("WorstRateProvenance = %q; the weakest rate behind any dollar decides", got)
	}
	wantCode(t, c, ReasonUnverifiedRate)
	if rep3.Totals.Unverified != 1 {
		t.Errorf("totals.Unverified = %d, want 1: a target with one unverified dollar counts",
			rep3.Totals.Unverified)
	}

	// An instance nobody could price at all is a THIRD state, not "unverified".
	if got := (Assessment{}).WorstRateProvenance(); got != "" {
		t.Errorf("an unpriced assessment reports provenance %q, want the empty third state", got)
	}
}

func mustLoadRates(t *testing.T, body string) RateCard {
	t.Helper()
	card, err := LoadRates(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadRates: %v", err)
	}
	return card
}

// A window shorter than the minimum is a stated refusal, not a silent
// weakening of every verdict above it.
func TestShortWindowIsRefusedNotAbsorbed(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("s", "db.r6i.xlarge", "postgres")},
		Metrics:   metricsFor("s", 30, 12, 24<<30, 40*GiB),
	}
	short := Window{Start: testEnd.Add(-6 * time.Hour), End: testEnd}
	snap := collect(t, f, func(c *CollectorConfig) { c.Window = short })
	rep := assess(t, snap, nil)
	wantCode(t, must(t, rep, "s"), ReasonInsufficientWindow)

	full := assess(t, collect(t, f), nil)
	wantNoCode(t, must(t, full, "s"), ReasonInsufficientWindow)
}

// The U13 seam is declared, reserved and — in this unit — deliberately unused,
// and the sizer says so rather than borrowing pkg/ebs's numbers.
func TestStoragePerformanceIsRefusedNotBorrowed(t *testing.T) {
	f := &Fixture{
		Instances: []DBInstanceRecord{
			rec("gp2-500", "db.r6i.xlarge", "mysql", withStorage(500, 0, StorageGP2)),
		},
		Metrics: metricsFor("gp2-500", 30, 12, 24<<30, 200*GiB),
	}
	rep := assess(t, collect(t, f), nil)
	a := must(t, rep, "gp2-500")
	wantCode(t, a, ReasonNoStoragePerformanceModel)
	if a.Proposal != nil {
		t.Fatal("a storage-performance proposal was produced without a storage-performance model")
	}
	// The refusal names why pkg/ebs's table is the wrong one — trap 11 —
	// because "not implemented" and "implemented wrong elsewhere" are
	// different things to a reader.
	if !namesValue(a, ReasonNoStoragePerformanceModel, "12,000") {
		t.Error("the parity refusal does not name RDS's own burst ceiling")
	}
	if shippedSizer.Config().Parity != nil {
		t.Fatal("U11 must ship with the parity seam nil")
	}
}

// shippedSizer is built from DefaultConfig, so the nil-seam assertion above is
// about the SHIPPED default rather than about a config a test constructed.
var shippedSizer = func() *Sizer {
	sz, err := NewSizer(DefaultConfig())
	if err != nil {
		panic(err)
	}
	return sz
}()
