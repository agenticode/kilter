package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/agenticode/kilter/pkg/domain"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// The live RDS path and the storage-parity seam, driven through the REAL CLI.
//
// NO TEST IN THIS FILE MAKES A LIVE AWS CALL. Not one reads a credential,
// opens ~/.aws, sets an AWS_* variable or touches a socket. Every live test
// installs a seam set built from pkg/rds's own Fixture and EnvelopeFixture,
// which is why the collector, the pagination, the window clamp, the
// access-denied retry and the envelope collection are all exercised for real
// while the SDK is never constructed. The single test that does reach
// dialRDS — TestABlankLiveRegionIsRefusedBeforeAnyCredentialIsRead — is
// deliberately the one case that provably returns before LoadDefaultConfig.
//
// A test that would try to reach the network on a developer laptop with a
// stale profile is not a slow test, it is a failed unit.

// --- fakes -----------------------------------------------------------------

// deniedError is an AccessDeniedException in the shape provider.IsAccessDenied
// actually matches: a smithy.APIError, not a string.
type deniedError struct {
	code string
	msg  string
}

func (e deniedError) Error() string                 { return e.code + ": " + e.msg }
func (e deniedError) ErrorCode() string             { return e.code }
func (e deniedError) ErrorMessage() string          { return e.msg }
func (e deniedError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func accessDenied(op string) error {
	return deniedError{code: "AccessDeniedException",
		msg: "User is not authorized to perform: " + op}
}

// liveSeamsFrom builds the live seam set out of a recorded account.
//
// It is deliberately the SAME rdsFixtureFile the --rds-fixture path decodes,
// so the two sources can be compared field for field: anything the live path
// reports that the recorded path does not is a difference in the wiring rather
// than in the data.
func liveSeamsFrom(f rdsFixtureFile, notes []string) (rdsLiveSeams, *krds.Fixture) {
	fx := &krds.Fixture{
		Instances: f.Instances, Clusters: f.Clusters, Tags: f.Tags, Metrics: f.Metrics,
		Reservations: f.Reservations, PageSize: f.PageSize, DropResults: f.DropResults,
	}
	s := rdsLiveSeams{
		Region: fixtureRegion, Inventory: fx, Metrics: fx, Commitment: fx,
		Envelope: &krds.EnvelopeFixture{
			Options: f.StorageOptions, Events: f.Events, PageSize: f.PageSize},
		Notes: func() []string { return notes },
	}
	// The three optional seams, absent in the form pkg/rds documents: a nil
	// interface, not a failing one. The difference is the subject of
	// TestAFailingMetricsSeamIsNotTheSameAsAnAbsentOne.
	if f.NoMetricsAPI {
		s.Metrics = nil
	}
	if f.NoCommitmentAPI {
		s.Commitment = nil
	}
	if f.NoEnvelopeAPI {
		s.Envelope = nil
	}
	return s, fx
}

// withLiveRDS installs a fake seam set for the duration of one test and
// restores the real dialer afterwards, so a later test can never inherit it.
func withLiveRDS(t *testing.T, s rdsLiveSeams) {
	t.Helper()
	prev := newRDSLiveSeams
	newRDSLiveSeams = func(_ context.Context, region string) (rdsLiveSeams, error) {
		out := s
		if out.Region == "" {
			out.Region = region
		}
		return out, nil
	}
	t.Cleanup(func() { newRDSLiveSeams = prev })
}

// liveArgs is the live sibling of rdsArgs: the same command over the same
// account, reached through --rds-region instead of --rds-fixture.
func liveArgs(sub string, extra ...string) []string {
	args := []string{sub,
		"--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope,
		"--region", fixtureRegion,
		"--domain", "rds",
		"--rds-region", fixtureRegion,
	}
	return append(args, extra...)
}

// runFails runs the command expecting failure and returns the error.
func runFails(t *testing.T, args ...string) error {
	t.Helper()
	var b strings.Builder
	err := runDomainsTo(&b, args)
	if err == nil {
		t.Fatalf("kilter domains %s unexpectedly succeeded:\n%s", strings.Join(args, " "), b.String())
	}
	return err
}

// rdsRefusalCodes returns every RDS refusal code in an aggregate report,
// keyed by target and counted.
func rdsRefusalCodes(env reportEnvelope) map[string]int {
	out := map[string]int{}
	for _, ref := range env.Report.Refusals {
		if ref.Target.Domain == domain.RDS {
			out[ref.Code]++
		}
	}
	return out
}

func warningsMentioning(env reportEnvelope, sub string) []string {
	var out []string
	for _, w := range env.Warnings {
		if strings.Contains(w, sub) {
			out = append(out, w)
		}
	}
	return out
}

// --- (a) the live collector ------------------------------------------------

// TestTheLiveRDSCollectorIsReachableFromTheBinary is the reason half of this
// unit exists. pkg/provider shipped both SDK adapters and nothing called them:
// --rds-fixture was the only way to drive pkg/rds, so a user with an AWS
// account could not run the RDS domain at all.
//
// The assertion is equality with the recorded path, because that is the
// strongest available statement: the live wiring passes the same seams to the
// same collector and changes nothing on the way through. A live path that
// merely "worked" could still be silently dropping a page, a tag or a series.
func TestTheLiveRDSCollectorIsReachableFromTheBinary(t *testing.T) {
	seams, fx := liveSeamsFrom(buildRDSFixture(), nil)
	withLiveRDS(t, seams)

	var live, recorded reportEnvelope
	runJSON(t, &live, liveArgs("report")...)
	runJSON(t, &recorded, rdsArgs(t, "report")...)

	dr, ok := live.Report.For(domain.RDS)
	if !ok {
		t.Fatal("the rds domain does not appear in a live report")
	}
	if !dr.Health.Ready || dr.Health.Targets != 7 {
		t.Fatalf("live collection produced %d ready=%v targets, want 7 ready", dr.Health.Targets, dr.Health.Ready)
	}
	if dr.Refused == 0 {
		t.Fatal("the live path produced no refusals; the refusals ARE this domain's output")
	}
	// Real pagination, not a single page: the recorded account uses pageSize 3
	// over 7 instances, and the live seams inherit it.
	if fx.Calls.DescribeDBInstances < 3 {
		t.Errorf("DescribeDBInstances called %d times over a 3-instance page size; "+
			"pagination did not survive the live wiring", fx.Calls.DescribeDBInstances)
	}
	if fx.Calls.ListTagsForResource == 0 {
		t.Error("ListTagsForResource was never called, so the kilter.dev/mode guardrail is unreachable live")
	}
	if fx.Calls.GetMetricData == 0 {
		t.Error("GetMetricData was never called; every instance would refuse for lack of evidence")
	}

	gotLive, err := json.Marshal(live.Report)
	if err != nil {
		t.Fatal(err)
	}
	gotRec, err := json.Marshal(recorded.Report)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotLive) != string(gotRec) {
		t.Errorf("the live path and the recorded path disagree about the same account:\n--- live ---\n%s\n--- recorded ---\n%s",
			gotLive, gotRec)
	}
}

// TestLiveReservationsReachTheAccountWideBaseline: the live snapshot's
// Reserved DB Instances are spliced into the commitment inventory exactly as
// the recorded path's are. An RDS line is absorbed by a Reserved DB Instance
// and by nothing else, so a live path that dropped them would understate
// coverage on every other domain's report too.
func TestLiveReservationsReachTheAccountWideBaseline(t *testing.T) {
	seams, _ := liveSeamsFrom(buildRDSFixture(), nil)
	withLiveRDS(t, seams)

	df := &domainFlags{
		now: fixtureNow.Format(time.RFC3339), scope: fixtureScope, region: fixtureRegion,
		rdsWindow: 14 * 24 * time.Hour,
	}
	df.kinds = repeatedFlag{"rds"}
	df.rdsRegions = repeatedFlag{fixtureRegion}
	rt, err := buildRuntime(df)
	if err != nil {
		t.Fatal(err)
	}
	var rdsLines int
	for _, l := range rt.Ledger.Baseline() {
		if strings.Contains(l.ID, ":db:") {
			rdsLines++
		}
	}
	if rdsLines == 0 {
		t.Fatal("no live RDS usage line reached the account-wide baseline")
	}
	for _, w := range rt.Warnings {
		if strings.Contains(w, "dropped a usage line") {
			t.Errorf("a live RDS baseline line was dropped: %s", w)
		}
	}
}

// TestTheAdaptersNotesAreRenderedBesideTheSnapshotWarnings.
//
// RDS-ADAPTER-FINDINGS.md §3.1 and §6.2: three degradations — an instance with
// neither ARN nor identifier, a tag with no key, an event with no date — are
// SILENT inside pkg/rds because its seam structs have no field for them. The
// adapters carry them on Notes(), and rendering that is not optional. A
// degradation nobody can see is a degradation that did not happen.
func TestTheAdaptersNotesAreRenderedBesideTheSnapshotWarnings(t *testing.T) {
	const note = "dropped a tag with an empty key on db-legacy; if it was kilter.dev/mode " +
		"the opt-out is not honoured"
	seams, _ := liveSeamsFrom(buildRDSFixture(), []string{note})
	withLiveRDS(t, seams)

	var env reportEnvelope
	runJSON(t, &env, liveArgs("report")...)
	if len(warningsMentioning(env, note)) == 0 {
		t.Errorf("the adapter's Notes() never reached the user: %v", env.Warnings)
	}
}

// TestAFailingMetricsSeamIsNotTheSameAsAnAbsentOne is the asymmetry
// RDS-ADAPTER-FINDINGS.md §3 puts in bold, and the one degradation cmd/ has to
// implement itself.
//
// cloudwatch:GetMetricData is documented OPTIONAL, and it is — but only in the
// nil form. A credential that lacks it and is wired anyway makes readMetrics
// return an error and Collect propagate it, so without the retry the operator
// gets an AccessDeniedException where the design promises a complete report in
// which every instance refuses with no-metric-evidence.
func TestAFailingMetricsSeamIsNotTheSameAsAnAbsentOne(t *testing.T) {
	f := buildRDSFixture()
	seams, fx := liveSeamsFrom(f, nil)
	fx.MetricsErr = accessDenied("cloudwatch:GetMetricData")
	withLiveRDS(t, seams)

	var env reportEnvelope
	runJSON(t, &env, liveArgs("report")...)

	dr, ok := env.Report.For(domain.RDS)
	if !ok || dr.Health.Targets != 7 {
		t.Fatalf("the inventory did not survive a denied GetMetricData: %+v", dr)
	}
	if rdsRefusalCodes(env)[krds.ReasonNoMetricEvidence] == 0 {
		t.Error("no instance refused with no-metric-evidence; silence was read as data")
	}
	if len(warningsMentioning(env, "cloudwatch:GetMetricData")) == 0 {
		t.Errorf("the degradation is invisible; a report that quietly lost its evidence "+
			"reads as a report about quiet databases: %v", env.Warnings)
	}
	// The retry must not have manufactured an idle verdict out of the silence.
	out := run(t, liveArgs("report", "--rds-detail")...)
	if strings.Contains(out, krds.AdvisoryIdleInstance) || strings.Contains(out, krds.AdvisoryIdleReadReplica) {
		t.Errorf("an idle verdict was drawn from an unanswered CloudWatch:\n%s", out)
	}
}

// TestAThrottleIsNotMistakenForAMissingPermission is the negative of the test
// above, and it is why the retry is gated on provider.IsAccessDenied rather
// than on "the metrics call failed".
//
// Swallowing a throttle or a timeout would turn a transient fault into a
// permanently degraded report that claims the credential lacks a permission it
// actually holds — and the operator would then go and grant a permission that
// was never missing.
func TestAThrottleIsNotMistakenForAMissingPermission(t *testing.T) {
	seams, fx := liveSeamsFrom(buildRDSFixture(), nil)
	fx.MetricsErr = deniedError{code: "ThrottlingException", msg: "Rate exceeded"}
	withLiveRDS(t, seams)

	err := runFails(t, liveArgs("report")...)
	if !strings.Contains(err.Error(), "Rate exceeded") {
		t.Errorf("a throttle was reported as something else: %v", err)
	}
	if strings.Contains(err.Error(), "does not hold cloudwatch:GetMetricData") {
		t.Errorf("a throttle was recorded as a missing permission: %v", err)
	}
}

// TestAMissingRequiredPermissionFailsLoudly.
//
// DescribeDBInstances is the one hard dependency in the whole seam set: with
// no inventory there is nothing to report on. The failure mode this guards
// against is the retry being written too broadly — an AccessDenied on the
// REQUIRED call being caught by the metrics branch and downgraded into "no
// CloudWatch", which would produce a confident, complete-looking report over
// zero databases.
func TestAMissingRequiredPermissionFailsLoudly(t *testing.T) {
	seams, fx := liveSeamsFrom(buildRDSFixture(), nil)
	fx.InstancesErr = accessDenied("rds:DescribeDBInstances")
	withLiveRDS(t, seams)

	err := runFails(t, liveArgs("report")...)
	if !strings.Contains(err.Error(), "rds:DescribeDBInstances") {
		t.Errorf("the error does not name the permission that is missing: %v", err)
	}
	if strings.Contains(err.Error(), "GetMetricData") {
		t.Errorf("a denied DescribeDBInstances was routed through the metrics degradation: %v", err)
	}
}

// TestOptionalPermissionsDegradeToAWarningAndNeverToAFailure covers the two
// optional seams that already degrade INSIDE pkg/rds, wired unconditionally
// for exactly that reason.
//
// A missing optional permission must produce a report WITH A NOTE — never a
// hard failure, and never a silently smaller report.
func TestOptionalPermissionsDegradeToAWarningAndNeverToAFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*krds.Fixture)
		want   string
	}{
		{
			// rds:DescribeReservedDBInstances. Net savings equal gross, which
			// UNDER-claims; a failure here would be a run lost to a permission
			// that can only ever make a number smaller.
			name:   "DescribeReservedDBInstances",
			break_: func(f *krds.Fixture) { f.ReservationsErr = accessDenied("rds:DescribeReservedDBInstances") },
			want:   "under-claims",
		},
		{
			// rds:DescribeDBClusters. Members are still excluded, under the
			// more cautious cluster-member-not-supported rather than Aurora's
			// name.
			name:   "DescribeDBClusters",
			break_: func(f *krds.Fixture) { f.ClustersErr = accessDenied("rds:DescribeDBClusters") },
			want:   "cluster",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seams, fx := liveSeamsFrom(buildRDSFixture(), nil)
			tc.break_(fx)
			withLiveRDS(t, seams)

			var env reportEnvelope
			runJSON(t, &env, liveArgs("report")...)
			dr, ok := env.Report.For(domain.RDS)
			if !ok || dr.Health.Targets != 7 {
				t.Fatalf("a denied %s shrank the report to %+v", tc.name, dr)
			}
			if len(warningsMentioning(env, tc.want)) == 0 {
				t.Errorf("a denied %s degraded silently; the report looks complete and is not: %v",
					tc.name, env.Warnings)
			}
		})
	}
}

// TestAnUnreadableTagIsNotAnAbsentTag is the most dangerous class of bug in
// this codebase, asserted at the wiring.
//
// db-legacy carries kilter.dev/mode=off. If a denied ListTagsForResource
// collapsed into an empty tag map, "I could not look" would become "I looked
// and there was nothing", and an operator who tagged a database to be left
// alone would be silently disobeyed by a report that says nothing about it.
//
// The two halves are asserted together on purpose. The guardrail refusal
// DISAPPEARING is what makes the warning load-bearing: assert only the warning
// and the test still passes on a wiring that honours the tag anyway; assert
// only the refusal and the test cannot tell "unreadable" from "absent".
func TestAnUnreadableTagIsNotAnAbsentTag(t *testing.T) {
	legacy := rdsARN("db-legacy")

	// Control: with the tag readable, the guardrail fires and nothing warns.
	seams, _ := liveSeamsFrom(buildRDSFixture(), nil)
	withLiveRDS(t, seams)
	var honoured reportEnvelope
	runJSON(t, &honoured, liveArgs("report")...)
	if rdsRefusalCodes(honoured)[krds.ReasonModeOff] == 0 {
		t.Fatal("the control is broken: kilter.dev/mode=off did not reach the report at all")
	}
	if len(warningsMentioning(honoured, krds.TagKilterMode)) != 0 {
		t.Errorf("a readable tag produced a guardrail warning: %v", honoured.Warnings)
	}

	// The subject: ListTagsForResource denied for that one instance.
	denied, fx := liveSeamsFrom(buildRDSFixture(), nil)
	fx.TagsErr = map[string]error{legacy: accessDenied("rds:ListTagsForResource")}
	withLiveRDS(t, denied)
	var unread reportEnvelope
	runJSON(t, &unread, liveArgs("report")...)

	dr, ok := unread.Report.For(domain.RDS)
	if !ok || dr.Health.Targets != 7 {
		t.Fatalf("a denied ListTagsForResource shrank the report: %+v", dr)
	}
	// It says so, by name, naming the instance AND the guardrail — the two
	// facts an operator needs to know their opt-out was not evaluated.
	warns := warningsMentioning(unread, krds.TagKilterMode)
	if len(warns) == 0 {
		t.Fatalf("an unreadable tag was reported as no tag: %v", unread.Warnings)
	}
	var named bool
	for _, w := range warns {
		if strings.Contains(w, legacy) {
			named = true
		}
	}
	if !named {
		t.Errorf("the warning does not name the instance whose guardrail went unevaluated: %v", warns)
	}
	// And the consequence is real rather than theoretical: the opt-out is NOT
	// honoured. That is precisely why the warning has to exist.
	if rdsRefusalCodes(unread)[krds.ReasonModeOff] != 0 {
		t.Error("the mode=off guardrail fired without the tags being readable; " +
			"this test can no longer distinguish an unreadable tag from an absent one")
	}
}

// TestTheLiveWindowIsClampedByPkgRDSAndTheClampIsSaidOutLoud.
//
// The clamp is pkg/rds's job and this wiring does not re-derive it — it
// reports it. A 30-day request returns 15 days of data inside a 30-day window,
// and silence read across the other 15 is how "this database had no
// connections for a month" gets manufactured.
func TestTheLiveWindowIsClampedByPkgRDSAndTheClampIsSaidOutLoud(t *testing.T) {
	seams, _ := liveSeamsFrom(buildRDSFixture(), nil)
	withLiveRDS(t, seams)

	var env reportEnvelope
	runJSON(t, &env, liveArgs("report", "--rds-window", "720h")...)
	if len(warningsMentioning(env, "clamped")) == 0 {
		t.Errorf("a 30-day live window was accepted silently: %v", env.Warnings)
	}
	out := run(t, liveArgs("report", "--rds-window", "720h", "--rds-detail")...)
	if strings.Contains(out, "720h0m0s window") {
		t.Errorf("the live report renders the REQUESTED window rather than the observed one:\n%s", out)
	}
}

// TestABlankLiveRegionIsRefusedBeforeAnyCredentialIsRead exercises the REAL
// dialer — the only test here that does — and is safe precisely because
// provider.NewRDSAPI rejects a blank region before it calls LoadDefaultConfig.
//
// The region is not a tidiness check: CollectorConfig.Region stamps
// DBInstance.Region and therefore selects the rate-card row that prices every
// instance, so a client talking to one region under a config naming another
// produces a report whose every dollar is confidently wrong.
func TestABlankLiveRegionIsRefusedBeforeAnyCredentialIsRead(t *testing.T) {
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE",
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"} {
		t.Setenv(k, "")
	}
	err := runFails(t, "report", "--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope, "--domain", "rds", "--rds-region", "")
	if !strings.Contains(err.Error(), "region required") {
		t.Errorf("a blank --rds-region was not refused by name: %v", err)
	}
}

// TestNoActuatorBecomesReachableThroughTheLivePath.
//
// This unit wires READ-ONLY paths. pkg/rds's actuator exists and stays
// unreachable: Registry.PlanSteps checks Health before the domain is
// consulted, there is no rds row in the actuator table, and nothing in the
// live wiring imports ApprovedStep or ModifyStorage. Making an actuator
// reachable is a separate, separately-approved decision, and this asserts the
// live source did not smuggle one in.
func TestNoActuatorBecomesReachableThroughTheLivePath(t *testing.T) {
	seams, _ := liveSeamsFrom(buildRDSFixture(), nil)
	withLiveRDS(t, seams)

	var env struct {
		Plans []domain.Plan `json:"plans"`
	}
	runJSON(t, &env, liveArgs("plan", "--rds-parity")...)
	if len(env.Plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(env.Plans))
	}
	p := env.Plans[0]
	if p.Actuatable || len(p.Steps) != 0 || p.RefusalCode != domain.RefuseReportOnly {
		t.Errorf("a live, parity-enabled rds domain produced actuatable=%v steps=%d refusal=%q",
			p.Actuatable, len(p.Steps), p.RefusalCode)
	}
}

// flakyInventory fails DescribeDBInstances from the Nth call onward, so a
// collection can succeed once and fail on the retry — which is the only shape
// in which a warning exists before a fatal error.
type flakyInventory struct {
	*krds.Fixture
	failFrom int
	calls    int
	err      error
}

func (f *flakyInventory) DescribeDBInstances(ctx context.Context,
	in *krds.DescribeDBInstancesInput) (*krds.DescribeDBInstancesOutput, error) {

	f.calls++
	if f.calls >= f.failFrom {
		return nil, f.err
	}
	return f.Fixture.DescribeDBInstances(ctx, in)
}

// TestAFailedCollectionSaysWhatItHadAlreadyLearned.
//
// buildRuntime returns nil on error, so every warning appended to
// runtime.Warnings on the way to a failure is discarded — and an operator is
// left with one line where the run had already recorded that it fell back from
// a denied GetMetricData and then died of something else entirely. rdsFailure
// puts them in the only channel that survives an aborted build.
func TestAFailedCollectionSaysWhatItHadAlreadyLearned(t *testing.T) {
	seams, fx := liveSeamsFrom(buildRDSFixture(), nil)
	// Denied metrics: the first pass records the degradation and retries with
	// the seam dropped. The retry then dies of a transient inventory fault,
	// which is a different and fatal problem. The recorded account pages 7
	// instances 3 at a time, so the second pass starts at call 4.
	fx.MetricsErr = accessDenied("cloudwatch:GetMetricData")
	seams.Inventory = &flakyInventory{Fixture: fx, failFrom: 4,
		err: errors.New("RequestTimeout: connection reset")}
	withLiveRDS(t, seams)

	err := runFails(t, liveArgs("report")...)
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the fatal cause is not named: %v", err)
	}
	if !strings.Contains(err.Error(), "cloudwatch:GetMetricData") {
		t.Errorf("the degradation the run had already recorded was thrown away with the runtime: %v", err)
	}
}
