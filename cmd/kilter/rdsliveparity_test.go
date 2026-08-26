package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// The StorageParity seam (U13), driven through the REAL CLI over a recorded
// account and over the live seams.
//
// cmd/WIRING-FINDINGS.md §6.4's last bullet: "pkg/rds's StorageParity seam is
// still nil, as U11 shipped it — --rds-fixture cannot enable it and no flag
// pretends to". Both halves are fixed here, and the harder half is the second:
// a flag that pretends to enable a seam is worse than no flag, so the tests
// below assert what the report SAYS in each of the three states — seam off,
// seam on with no envelope, seam on with an envelope.
//
// No AWS call, no credential, no socket. The envelope and the event history
// come from pkg/rds's own EnvelopeFixture through the real EnvelopeCollector.

// parityFixture is the recorded account plus one instance the parity engine
// can actually reach a verdict about.
//
// db-gp3-fat is 1,000 GiB of gp3 provisioned at 20,000 IOPS / 900 MiB/s and
// measured at a fraction of it. That is the shape §2.4 says carries the money:
// a REDUCTION toward the striped regime's 12,000 / 500 floor, which is a
// different lever from the gp2→gp3 conversion and the only one on this fixture
// that clears every gate.
func parityFixture() rdsFixtureFile {
	f := buildRDSFixture()
	f.Instances = append(f.Instances, krds.DBInstanceRecord{
		DBInstanceIdentifier: "db-gp3-fat", DBInstanceArn: rdsARN("db-gp3-fat"),
		DBInstanceClass: "db.r6i.xlarge", DBInstanceStatus: krds.StatusAvailable,
		Engine: "mysql", EngineVersion: "8.0.39", LicenseModel: krds.LicenseGPL,
		AvailabilityZone: fixtureRegion + "a",
		AllocatedStorage: 1000, StorageType: krds.StorageGP3,
		Iops: 20000, StorageThroughput: 900,
		InstanceCreateTime: fixtureNow.Add(-90 * 24 * time.Hour),
	})
	// The four series MeasureIO reads. All four, in full: a demand figure
	// missing its write half is not a smaller demand, it is an unknown one,
	// and pkg/rds refuses on exactly that.
	f.Metrics["db-gp3-fat/"+krds.MetricCPUUtilization] = rdsSeries(22)
	f.Metrics["db-gp3-fat/"+krds.MetricDatabaseConns] = rdsSeries(30)
	f.Metrics["db-gp3-fat/"+krds.MetricReadIOPS] = rdsSeries(300)
	f.Metrics["db-gp3-fat/"+krds.MetricWriteIOPS] = rdsSeries(200)
	f.Metrics["db-gp3-fat/"+krds.MetricReadThroughput] = rdsSeries(20 * krds.MiB)
	f.Metrics["db-gp3-fat/"+krds.MetricWriteThroughput] = rdsSeries(10 * krds.MiB)

	// The LIVE provisioning envelope, read rather than hardcoded: AWS
	// publishes two contradictory gp3 ceilings and pkg/rds encodes neither.
	f.StorageOptions = map[string][]krds.ValidStorageOptionRecord{
		"db-gp3-fat": {{
			StorageType: krds.StorageGP3,
			MinIOPS:     12000, MaxIOPS: 64000,
			MinStorageThroughputMBps: 500, MaxStorageThroughputMBps: 4000,
			MinAllocatedStorageGiB: 20, MaxAllocatedStorageGiB: 65536,
		}},
	}
	return f
}

// parityArgs runs the recorded account with whatever parity flags are added.
func parityArgs(t *testing.T, f rdsFixtureFile, extra ...string) []string {
	t.Helper()
	args := []string{"report",
		"--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope,
		"--region", fixtureRegion,
		"--domain", "rds",
		"--rds-fixture", writeRDSFixture(t, f),
	}
	return append(args, extra...)
}

// writeJSONFile writes a rate override and returns its path.
func writeJSONFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// claimableRates is a rate file whose STORAGE rows are operator-supplied,
// which is what makes a storage saving quotable at all. Every shipped row is
// `unverified` and can size a fact and never a saving.
const claimableRates = `{
  "region": "us-east-1",
  "classes": {"open-source|db.r6i.xlarge": {"singleAZHourlyUSD": 0.48}},
  "storage": {"gp2GiBMonthUSD": 0.115, "gp3GiBMonthUSD": 0.092,
              "io1GiBMonthUSD": 0.125, "io2GiBMonthUSD": 0.125}
}`

const claimableParityRates = `{"provisionedIOPSMonthUSD": 0.02, "provisionedThroughputMonthUSD": 0.08}`

// TestParityIsOptInAndItsAbsenceIsVisible is the whole of §6.4's last bullet.
//
// Two failure modes, and the second is the dangerous one:
//
//   - a flag that does not enable the seam (the U11 state: the seam is nil and
//     nothing can fill it);
//   - a report that looks complete and is quietly missing a dimension.
//
// pkg/rds forecloses the second by construction — with Config.Parity nil the
// sizer refuses every instance's storage under no-storage-performance-model —
// and this asserts the CLI actually delivers that, and that --rds-parity
// actually replaces it with real verdicts rather than adding a flag that does
// nothing.
func TestParityIsOptInAndItsAbsenceIsVisible(t *testing.T) {
	f := parityFixture()

	var off reportEnvelope
	runJSON(t, &off, parityArgs(t, f)...)
	codes := rdsRefusalCodes(off)
	if codes[krds.ReasonNoStoragePerformanceModel] == 0 {
		t.Fatal("a report that did not assess storage parity does not say so; " +
			"a reader cannot tell a dimension that was skipped from one that found nothing")
	}
	for _, parityCode := range []string{
		krds.ReasonParityNoMeasurement, krds.ReasonParityEnvelopeUnknown,
		krds.ReasonParityFloorsAtBaseline, krds.ReasonParityCooldown,
	} {
		if codes[parityCode] != 0 {
			t.Errorf("the seam is off and %q was still emitted", parityCode)
		}
	}

	var on reportEnvelope
	runJSON(t, &on, parityArgs(t, f, "--rds-parity")...)
	onCodes := rdsRefusalCodes(on)
	if onCodes[krds.ReasonNoStoragePerformanceModel] != 0 {
		t.Errorf("--rds-parity was passed and the not-evaluated refusal is still there: " +
			"the flag pretends to enable a seam it did not")
	}
	// The seam actually ran: at least one instance now carries a verdict that
	// only the parity engine produces.
	var reached int
	for _, c := range []string{
		krds.ReasonParityNoMeasurement, krds.ReasonParityEnvelopeUnknown,
		krds.ReasonParityGP2BandUnpublished, krds.ReasonParityFloorsAtBaseline,
		krds.ReasonParityStorageTypeNotModelled, krds.ReasonParityNoCheaperConfig,
	} {
		reached += onCodes[c]
	}
	if reached == 0 {
		t.Errorf("--rds-parity produced no parity verdict of any kind: %v", onCodes)
	}
	// And every instance still carries a reason. Silence is not an output.
	dr, ok := on.Report.For(domain.RDS)
	if !ok || dr.Health.Targets != 8 {
		t.Fatalf("parity changed the inventory: %+v", dr)
	}
}

// TestParityWithoutTheEnvelopeSeamRefusesByNameRatherThanAssumingACeiling.
//
// RDS-ADAPTER-FINDINGS.md §4.4: StorageEnvelope.Known with MaxIOPS == 0 reads
// as "no ceiling to enforce" in pkg/rds/parity.go, so an unknown ceiling that
// leaked through as a zero one would let an 80,000-IOPS configuration pass
// validation on an instance capped at 16,000. The seam's absence therefore has
// to arrive as an UNKNOWN envelope and a named refusal — never as a permissive
// default and never as a silent hole.
func TestParityWithoutTheEnvelopeSeamRefusesByNameRatherThanAssumingACeiling(t *testing.T) {
	f := parityFixture()
	f.NoEnvelopeAPI = true

	var env reportEnvelope
	runJSON(t, &env, parityArgs(t, f, "--rds-parity")...)

	dr, ok := env.Report.For(domain.RDS)
	if !ok || dr.Health.Targets != 8 {
		t.Fatalf("a missing rds:DescribeValidDBInstanceModifications shrank the report: %+v", dr)
	}
	if rdsRefusalCodes(env)[krds.ReasonParityEnvelopeUnknown] == 0 {
		t.Errorf("no instance refused with %q; an unread envelope became an unlimited one: %v",
			krds.ReasonParityEnvelopeUnknown, rdsRefusalCodes(env))
	}
	if len(warningsMentioning(env, "DescribeValidDBInstanceModifications")) == 0 {
		t.Errorf("the missing seam is not named anywhere in the warnings: %v", env.Warnings)
	}
	// No proposal can be produced without a ceiling to check it against.
	for _, rec := range env.Report.Recommendations {
		if rec.Target.Domain == domain.RDS {
			t.Errorf("a proposal survived an unknown envelope: %s", rec.Target.ID)
		}
	}
}

// TestParityReachesAProposalOnlyWithClaimableRates is the seam's happy path
// and its money gate in one test, because they are one decision.
//
// pkg/rds does the whole arithmetic either way — the magnitude is reported
// whatever the rate says. What the provenance decides is whether that
// magnitude is allowed to be called a saving. §7 could not retrieve the RDS
// gp3 storage and provisioned-performance rates from AWS, so every shipped
// figure is `unverified` and refuses under unverified-rate; supplying
// --rds-rates and --rds-parity-rates is what unblocks the claim.
func TestParityReachesAProposalOnlyWithClaimableRates(t *testing.T) {
	f := parityFixture()

	// Unverified: the arithmetic runs, the opportunity is sized, the claim is
	// refused by name.
	var unverified reportEnvelope
	runJSON(t, &unverified, parityArgs(t, f, "--rds-parity")...)
	if rdsRefusalCodes(unverified)[krds.ReasonUnverifiedRate] == 0 {
		t.Error("an unverified storage rate produced no unverified-rate refusal")
	}

	rates := writeJSONFile(t, "rates.json", claimableRates)
	perf := writeJSONFile(t, "parity-rates.json", claimableParityRates)
	args := parityArgs(t, f, "--rds-parity", "--rds-rates", rates, "--rds-parity-rates", perf, "--rds-detail")

	var claimed struct {
		Report   *domain.Report `json:"report"`
		RDS      *krds.Report   `json:"rds"`
		Warnings []string       `json:"warnings"`
	}
	runJSON(t, &claimed, args...)
	if claimed.RDS == nil {
		t.Fatal("--rds-detail produced no pkg/rds report")
	}
	if claimed.RDS.Totals.Proposals == 0 {
		t.Fatalf("no proposal survived claimable rates; the seam can never produce one:\n%s",
			mustJSON(t, claimed.RDS.Totals))
	}
	// Report.Validate already ran inside the CLI (it refuses to print an
	// invalid report), so reaching here means every clause in validateProposal
	// held — including "allocated storage can only ever grow" and "an
	// unverified rate is a magnitude, not a saving".
	var found bool
	for _, a := range claimed.RDS.Assessments {
		if a.Proposal == nil {
			continue
		}
		found = true
		if a.Proposal.Action != domain.ActionAdvisory {
			t.Errorf("%s proposes action %q; this domain is advisory only", a.Target.ID, a.Proposal.Action)
		}
		if a.Proposal.AllocatedStorageGiB < a.Instance.AllocatedStorageGiB {
			t.Errorf("%s proposes shrinking allocated storage, which no RDS API can do", a.Target.ID)
		}
		if !a.Proposal.RateProvenance.Claimable() {
			t.Errorf("%s claims %v/mo from a %q rate", a.Target.ID,
				a.Proposal.NetSavingsMonthlyUSD, a.Proposal.RateProvenance)
		}
		if a.Proposal.IOPS < 12000 || a.Proposal.StorageThroughputMBps < 500 {
			t.Errorf("%s proposes %d IOPS / %d MiB/s, below the striped regime's non-reducible floor",
				a.Target.ID, a.Proposal.IOPS, a.Proposal.StorageThroughputMBps)
		}
	}
	if !found {
		t.Fatal("Totals.Proposals is non-zero and no assessment carries a proposal")
	}
	// The domain still recommends nothing through the generic seam: a
	// domain.Recommendation must carry a Proposed resource shape and pkg/rds
	// has none to give. The proposal is a REPORT line, not an executable one.
	for _, rec := range claimed.Report.Recommendations {
		if rec.Target.Domain == domain.RDS {
			t.Errorf("a parity proposal leaked into the actuatable recommendation stream: %s", rec.Target.ID)
		}
	}
}

// TestTheParityRatesFileCannotDeclareItsOwnProvenance.
//
// Provenance is the single gate between "this sizes an opportunity" and "this
// is a saving somebody can put in a business case". rds.LoadRates gives a rate
// file no way to name its own, and a second loader that did would be a way to
// promote a guess to a claim by typing a word.
func TestTheParityRatesFileCannotDeclareItsOwnProvenance(t *testing.T) {
	bad := writeJSONFile(t, "perf.json",
		`{"provisionedIOPSMonthUSD":0.02,"provisionedThroughputMonthUSD":0.08,"provenance":"verified"}`)
	if _, err := loadRDSPerformanceRates(bad); err == nil {
		t.Error("a parity rate file was allowed to declare itself verified")
	}
	// A non-positive rate is refused at the boundary rather than producing a
	// quietly wrong report downstream.
	for _, body := range []string{
		`{"provisionedIOPSMonthUSD":0,"provisionedThroughputMonthUSD":0.08}`,
		`{"provisionedIOPSMonthUSD":0.02,"provisionedThroughputMonthUSD":-1}`,
		`{"provisionedIOPSMonthUSD":0.02,"iopsRate":0.08}`,
	} {
		if _, err := loadRDSPerformanceRates(writeJSONFile(t, "perf.json", body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	// And the shipped default is what you get with no file: unverified, so it
	// sizes and never claims.
	got, err := loadRDSPerformanceRates("")
	if err != nil {
		t.Fatal(err)
	}
	if got != (krds.PerformanceRates{}) {
		t.Errorf("an absent --rds-parity-rates invented rates %+v rather than deferring to pkg/rds", got)
	}
}

// TestTheStorageModificationCooldownIsReadFromRecordedEvents.
//
// "You can perform a maximum of four storage modifications on a DB instance
// within any 24-hour period." A fifth is not a recommendation, it is an API
// error with a dollar figure attached — so the events seam has to be wired,
// not merely declared, and the wiring has to hand it a window longer than 24
// hours or the limit cannot be ruled out from it.
func TestTheStorageModificationCooldownIsReadFromRecordedEvents(t *testing.T) {
	f := parityFixture()
	f.Events = map[string][]krds.EventRecord{"db-gp3-fat": {
		{SourceIdentifier: "db-gp3-fat", SourceType: krds.EventSourceDBInstance,
			Categories: []string{krds.EventCategoryConfigurationChange},
			Message:    "Finished applying modification to allocated storage",
			Date:       fixtureNow.Add(-2 * time.Hour)},
		{SourceIdentifier: "db-gp3-fat", SourceType: krds.EventSourceDBInstance,
			Categories: []string{krds.EventCategoryConfigurationChange},
			Message:    "Finished applying modification to allocated storage",
			Date:       fixtureNow.Add(-4 * time.Hour)},
		{SourceIdentifier: "db-gp3-fat", SourceType: krds.EventSourceDBInstance,
			Categories: []string{krds.EventCategoryConfigurationChange},
			Message:    "Finished applying modification to allocated storage",
			Date:       fixtureNow.Add(-6 * time.Hour)},
		{SourceIdentifier: "db-gp3-fat", SourceType: krds.EventSourceDBInstance,
			Categories: []string{krds.EventCategoryConfigurationChange},
			Message:    "Finished applying modification to allocated storage",
			Date:       fixtureNow.Add(-8 * time.Hour)},
	}}
	rates := writeJSONFile(t, "rates.json", claimableRates)
	perf := writeJSONFile(t, "parity-rates.json", claimableParityRates)

	var env reportEnvelope
	runJSON(t, &env, parityArgs(t, f, "--rds-parity",
		"--rds-rates", rates, "--rds-parity-rates", perf)...)

	if rdsRefusalCodes(env)[krds.ReasonParityCooldown] == 0 {
		t.Fatalf("four storage modifications in eight hours did not block a fifth: %v",
			rdsRefusalCodes(env))
	}
	for _, ref := range env.Report.Refusals {
		if ref.Code == krds.ReasonParityCooldown && !strings.Contains(ref.Reason, "24-hour") {
			t.Errorf("the cooldown refusal does not state the limit it is enforcing: %s", ref.Reason)
		}
	}
	// The window handed to the events seam must exceed the 24-hour period, or
	// the collector warns that the limit cannot be ruled out. It must not.
	if w := warningsMentioning(env, "shorter than the 24h0m0s"); len(w) != 0 {
		t.Errorf("the event window is too short to answer the question it was asked: %v", w)
	}
}

// TestParityIsReachableOnTheLivePathToo. The envelope seam is the same
// *provider.RDSAPI as the inventory seam, so the flag has to work through
// --rds-region as well as --rds-fixture — and produce the same verdicts.
func TestParityIsReachableOnTheLivePathToo(t *testing.T) {
	f := parityFixture()
	seams, _ := liveSeamsFrom(f, nil)
	withLiveRDS(t, seams)

	rates := writeJSONFile(t, "rates.json", claimableRates)
	perf := writeJSONFile(t, "parity-rates.json", claimableParityRates)

	var live, recorded reportEnvelope
	runJSON(t, &live, liveArgs("report", "--rds-parity", "--rds-rates", rates, "--rds-parity-rates", perf)...)
	runJSON(t, &recorded, parityArgs(t, f, "--rds-parity", "--rds-rates", rates, "--rds-parity-rates", perf)...)

	if a, b := mustJSON(t, live.Report), mustJSON(t, recorded.Report); a != b {
		t.Errorf("the live parity path and the recorded one disagree:\n--- live ---\n%s\n--- recorded ---\n%s", a, b)
	}
	if rdsRefusalCodes(live)[krds.ReasonNoStoragePerformanceModel] != 0 {
		t.Error("--rds-parity did not reach the live path")
	}
}

// TestADeniedDescribeEventsLeavesTheLimitUnverifiedRatherThanCleared.
//
// rds:DescribeEvents is optional and degrades per instance: HistoryKnown goes
// false, so Cooldown answers "unknown" rather than "clear". That is the
// difference between "we checked and there is room for a fifth modification"
// and "we could not check" — and since the parity engine proceeds either way,
// the ONLY thing standing between an operator and the second being read as the
// first is the warning. So the warning is what this asserts.
func TestADeniedDescribeEventsLeavesTheLimitUnverifiedRatherThanCleared(t *testing.T) {
	f := parityFixture()
	seams, _ := liveSeamsFrom(f, nil)
	seams.Envelope = &krds.EnvelopeFixture{
		Options:   f.StorageOptions,
		EventsErr: map[string]error{"db-gp3-fat": accessDenied("rds:DescribeEvents")},
	}
	withLiveRDS(t, seams)

	var env reportEnvelope
	runJSON(t, &env, liveArgs("report", "--rds-parity")...)

	dr, ok := env.Report.For(domain.RDS)
	if !ok || dr.Health.Targets != 8 {
		t.Fatalf("a denied DescribeEvents shrank the report: %+v", dr)
	}
	warns := warningsMentioning(env, "DescribeEvents")
	if len(warns) == 0 {
		t.Fatalf("an unreadable modification history was reported as an empty one: %v", env.Warnings)
	}
	var stated bool
	for _, w := range warns {
		if strings.Contains(w, "unverified rather than cleared") {
			stated = true
		}
	}
	if !stated {
		t.Errorf("the warning does not say the limit is unverified rather than cleared: %v", warns)
	}
}
