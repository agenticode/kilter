package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing"
)

// These tests drive the REAL domain packages through the REAL CLI entry point
// over recorded snapshots. There is no network, no AWS SDK on this path, and
// no clock in any decision: --now fixes the decision time, so every dollar
// below is reproducible.

// baseArgs is the input every test shares: all four wired domains, the same
// decision time, the same scope.
func baseArgs(t *testing.T, sub string, extra ...string) []string {
	t.Helper()
	args := []string{sub,
		"--now", fixtureNow.Format("2006-01-02T15:04:05Z07:00"),
		"--scope", fixtureScope,
		"--region", fixtureRegion,
		"--snapshot", readFixture(t, "ec2-instances.json"),
		"--snapshot", readFixture(t, "ec2-volumes.json"),
		"--snapshot", readFixture(t, "ecs-services.json"),
		"--snapshot", readFixture(t, "lambda-functions.json"),
		"--kube-snapshot", readFixture(t, "cluster.json"),
	}
	return append(args, extra...)
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	var b strings.Builder
	if err := runDomainsTo(&b, args); err != nil {
		t.Fatalf("kilter domains %s: %v\n%s", strings.Join(args, " "), err, b.String())
	}
	return b.String()
}

func runJSON[T any](t *testing.T, out *T, args ...string) {
	t.Helper()
	raw := run(t, append(args, "--json")...)
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("decode --json output: %v\n%s", err, raw)
	}
}

type reportEnvelope struct {
	Report   *domain.Report `json:"report"`
	Warnings []string       `json:"warnings"`
}

// TestEveryWiredDomainIsReachableFromTheBinary is the reason this unit exists.
// Eight packages of decision logic were sitting in the repo unreachable from
// the binary; this asserts each one now answers.
func TestEveryWiredDomainIsReachableFromTheBinary(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, baseArgs(t, "report")...)
	rep := env.Report
	if rep == nil {
		t.Fatal("no report")
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("the CLI produced a report that fails its own invariants: %v", err)
	}

	// Every domain the binary wires must be present, ready, and have looked at
	// something. A domain that silently reported nothing would pass a weaker
	// assertion.
	for _, want := range []domain.Kind{
		domain.EC2, domain.ECSFargate, domain.K8sFargate, domain.Lambda,
	} {
		row, ok := rep.For(want)
		if !ok {
			t.Errorf("%s is missing from the report", want)
			continue
		}
		if !row.Health.Ready {
			t.Errorf("%s is not ready: %s", want, row.Health.Reason)
		}
		if row.Health.Targets == 0 {
			t.Errorf("%s observed no targets", want)
		}
		if row.Recommendations == 0 && row.Refused == 0 {
			t.Errorf("%s said nothing at all — neither a recommendation nor a refusal", want)
		}
		// Every Kind that made it into the registry is in the closed set.
		if !row.Kind.Valid() {
			t.Errorf("registered domain reports kind %q, outside the closed set", row.Kind)
		}
	}
	if len(env.Warnings) != 0 {
		t.Errorf("unexpected collection warnings: %v", env.Warnings)
	}
}

// TestBothEC2HalvesAnswerUnderOneKind is the Kind-collision resolution,
// end to end: pkg/ec2 (instances) and pkg/ebs (volumes) both register as
// `ec2` through one composite, and both produce output.
func TestBothEC2HalvesAnswerUnderOneKind(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, baseArgs(t, "report", "--domain", "ec2")...)
	rep := env.Report
	if len(rep.Domains) != 1 || rep.Domains[0].Kind != domain.EC2 {
		t.Fatalf("--domain ec2 registered %v", rep.Domains)
	}

	var sawInstance, sawVolume bool
	for _, rec := range rep.Recommendations {
		switch {
		case strings.HasPrefix(rec.Target.ID, "i-"):
			sawInstance = true
		case strings.HasPrefix(rec.Target.ID, "vol-"):
			sawVolume = true
		}
		if rec.Target.Domain != domain.EC2 {
			t.Errorf("%s is attributed to %q, not ec2", rec.Target.ID, rec.Target.Domain)
		}
	}
	for _, ref := range rep.Refusals {
		if strings.HasPrefix(ref.Target.ID, "i-") {
			sawInstance = true
		}
		if strings.HasPrefix(ref.Target.ID, "vol-") {
			sawVolume = true
		}
	}
	if !sawInstance {
		t.Error("the instance half (pkg/ec2) produced nothing")
	}
	if !sawVolume {
		t.Error("the volume half (pkg/ebs) produced nothing")
	}
	// Both ID spaces coexist without colliding, which is what makes one Kind
	// legitimate rather than a workaround.
	if rep.Domains[0].Health.Targets < 7 {
		t.Errorf("the composite tracks %d targets; both halves' targets should be counted",
			rep.Domains[0].Health.Targets)
	}
}

// TestCommitmentWaterfallSuppressesAGrossSaving is requirement 4 end to end.
//
// Exactly one thing changes between the two runs: an m5.2xlarge reservation
// covering the instance the sizer wants to shrink. The list-price delta is
// unchanged and still shown; the CLAIM disappears, with a stable code saying
// why. That gap is §7 trap 1.
func TestCommitmentWaterfallSuppressesAGrossSaving(t *testing.T) {
	var free, committed reportEnvelope
	runJSON(t, &free, baseArgs(t, "report")...)
	runJSON(t, &committed, baseArgs(t, "report",
		"--commitments", readFixture(t, "commitments.json"))...)

	if err := committed.Report.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	const instanceID = "i-0000000000000000a"
	var uncommittedClaim float64
	for _, rec := range free.Report.Recommendations {
		if rec.Target.ID == instanceID {
			uncommittedClaim = rec.ClaimableMonthlyUSD()
		}
	}
	if uncommittedClaim <= 0 {
		t.Fatalf("the control run does not claim a saving for %s; the fixture proves nothing", instanceID)
	}

	// With the reservation in play the instance is no longer claimable...
	for _, rec := range committed.Report.Recommendations {
		if rec.Target.ID == instanceID && rec.ClaimableMonthlyUSD() != 0 {
			t.Fatalf("%s still claims $%.2f under a reservation that would be stranded",
				instanceID, rec.ClaimableMonthlyUSD())
		}
	}
	// ...and the refusal says exactly why, with a stable code.
	var refusal *domain.Refusal
	for i := range committed.Report.Refusals {
		if committed.Report.Refusals[i].Target.ID == instanceID {
			refusal = &committed.Report.Refusals[i]
		}
	}
	if refusal == nil {
		t.Fatalf("%s vanished instead of being refused with a reason", instanceID)
	}
	switch refusal.Code {
	case domain.SuppressCommitmentNegative, domain.SuppressCommitmentNeutral:
	default:
		t.Fatalf("%s was refused as %q, want a commitment code", instanceID, refusal.Code)
	}
	if refusal.Reason == "" {
		t.Error("the commitment refusal carries no prose")
	}
	if refusal.ValidFrom.IsZero() {
		t.Error("a commitment-blocked refusal must be dated: it lapses when the commitment expires")
	}

	// The aggregate moved by exactly that instance's claim, and by nothing else.
	delta := free.Report.Totals.ClaimableMonthlyUSD - committed.Report.Totals.ClaimableMonthlyUSD
	if diff := delta - uncommittedClaim; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("the aggregate fell by $%.4f but the suppressed recommendation was $%.4f",
			delta, uncommittedClaim)
	}

	// And the text output says so, rather than quietly printing a smaller number.
	out := run(t, baseArgs(t, "report", "--commitments", readFixture(t, "commitments.json"))...)
	if !strings.Contains(out, "commitment-") {
		t.Errorf("the commitment refusal is not rendered:\n%s", out)
	}
}

// TestClaimableNeverExceedsGrossThroughTheCLI: Net ≤ Gross at every level of
// the CLI's aggregate, with and without commitments.
func TestClaimableNeverExceedsGrossThroughTheCLI(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no commitments", baseArgs(t, "report")},
		{"with commitments", baseArgs(t, "report",
			"--commitments", readFixture(t, "commitments.json"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env reportEnvelope
			runJSON(t, &env, tc.args...)
			rep := env.Report
			if err := rep.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if rep.Totals.ClaimableMonthlyUSD > rep.Totals.GrossMonthlyUSD {
				t.Fatalf("totals claim $%.2f > gross $%.2f",
					rep.Totals.ClaimableMonthlyUSD, rep.Totals.GrossMonthlyUSD)
			}
			for _, d := range rep.Domains {
				if d.ClaimableMonthlyUSD > d.GrossMonthlyUSD {
					t.Errorf("%s claims $%.2f > gross $%.2f",
						d.Kind, d.ClaimableMonthlyUSD, d.GrossMonthlyUSD)
				}
			}
			for _, rec := range rep.Recommendations {
				if rec.NetSavingsMonthlyUSD > rec.GrossSavingsMonthlyUSD {
					t.Errorf("%s: net $%.4f > gross $%.4f", rec.Target,
						rec.NetSavingsMonthlyUSD, rec.GrossSavingsMonthlyUSD)
				}
				if rec.Suppressed && rec.ClaimableMonthlyUSD() != 0 {
					t.Errorf("%s is suppressed and still claims $%.4f",
						rec.Target, rec.ClaimableMonthlyUSD())
				}
			}
		})
	}
}

// TestFargateOverheadCliffSurvivesTheWiring pins one exact number all the way
// through the CLI: the §4.1.1 pod that requests 1 vCPU / 8 GiB, is billed
// 2 vCPU / 9 GB because Fargate adds 256 MiB before rounding, and drops a
// whole vCPU when the request is shaved under the tier boundary.
//
// $32.795/month is pkg/domain/fargate's own asserted figure. If the wiring
// ever priced a Fargate pod by its node's reported capacity — 96 vCPU /
// 384 GB in this fixture, §7 trap 7 — this number would be nonsense.
func TestFargateOverheadCliffSurvivesTheWiring(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, baseArgs(t, "report", "--domain", "k8s-fargate")...)

	if len(env.Report.Recommendations) != 1 {
		t.Fatalf("k8s-fargate produced %d recommendations, want 1",
			len(env.Report.Recommendations))
	}
	rec := env.Report.Recommendations[0]
	if got := rec.Target.ID; got != "Deployment/default/api" {
		t.Errorf("target = %q", got)
	}
	if rec.Action != domain.ActionRolling {
		t.Errorf("action = %q; a Fargate resize is never in-place", rec.Action)
	}
	if got := rec.ClaimableMonthlyUSD(); got < 32.79 || got > 32.80 {
		t.Errorf("claimable = $%.4f/mo, want the $32.795 overhead-cliff figure", got)
	}
	if !strings.Contains(rec.Current.Canonical(), "2") {
		t.Errorf("current spec %q does not look like the billed 2vCPU tier", rec.Current.Canonical())
	}
}

// TestRefusalsAreTheProductNotAnErrorPath: the report names, for each domain,
// what it declined to do. A domain that assessed targets and proposed nothing
// must not read as a domain that found nothing.
func TestRefusalsAreTheProductNotAnErrorPath(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, baseArgs(t, "report")...)
	rep := env.Report

	if rep.Totals.Refused == 0 {
		t.Fatal("no refusals at all; the fixture is not exercising them")
	}
	// Each of these is a distinct arm of a domain's decision core, and each is
	// only visible because the wiring carries refusals as first-class output.
	for _, want := range []string{
		"memory-blind",           // pkg/ec2, §7 trap 4
		"burst-credit-depleted",  // pkg/ec2, §7 trap 5
		"k8s-tagged",             // pkg/ec2: a cluster node belongs to another pipeline
		"not-gp2",                // pkg/ebs
		"deployment-in-progress", // pkg/ecs
		"single-memory-point",    // pkg/lambda, its whole reason for existing
	} {
		if !hasCode(rep, want) {
			t.Errorf("refusal code %q never surfaced", want)
		}
	}
	for _, ref := range rep.Refusals {
		if ref.Code == "" || ref.Reason == "" {
			t.Errorf("refusal for %s has no code or no reason", ref.Target)
		}
	}

	out := run(t, baseArgs(t, "report")...)
	if !strings.Contains(out, "What kilter declined to do, and why") {
		t.Errorf("the text report has no refusals panel:\n%s", out)
	}
	if !strings.Contains(out, "Degraded domains") {
		t.Errorf("report-only domains are not labelled as such:\n%s", out)
	}
}

func hasCode(rep *domain.Report, code string) bool {
	for _, c := range rep.Totals.RefusedByCode {
		if c.Code == code {
			return true
		}
	}
	for _, c := range rep.Totals.SuppressedByCode {
		if c.Code == code {
			return true
		}
	}
	return false
}

// TestNoDomainCanPlanAStepInThisBuild is the honest state of the binary: every
// domain refuses, and each says why. This is the assertion that will fail —
// correctly — the day an SDK actuator is wired, at which point it should be
// updated rather than deleted.
func TestNoDomainCanPlanAStepInThisBuild(t *testing.T) {
	var env struct {
		Plans []domain.Plan `json:"plans"`
	}
	runJSON(t, &env, baseArgs(t, "plan")...)
	if len(env.Plans) != 4 {
		t.Fatalf("got %d plans, want one per wired domain", len(env.Plans))
	}
	for _, p := range env.Plans {
		if p.Actuatable {
			t.Errorf("%s claims an actuator; none is wired in this build", p.Kind)
		}
		if p.RefusalCode != domain.RefuseReportOnly {
			t.Errorf("%s refused as %q (%s), want %q",
				p.Kind, p.RefusalCode, p.Refusal, domain.RefuseReportOnly)
		}
		if len(p.Steps) != 0 {
			t.Errorf("%s produced %d steps with no actuator", p.Kind, len(p.Steps))
		}
		if p.Refusal == "" {
			t.Errorf("%s refused without saying why", p.Kind)
		}
	}
	out := run(t, baseArgs(t, "plan")...)
	if !strings.Contains(out, "refused (report-only)") {
		t.Errorf("the plan output does not surface the refusal:\n%s", out)
	}
}

// TestGuardrailsRefuseThePlan: freeze and the circuit breaker are honoured at
// the CLI, and their refusal is distinguishable from report-only.
func TestGuardrailsRefuseThePlan(t *testing.T) {
	for _, flag := range []string{"--freeze", "--breaker-open"} {
		t.Run(flag, func(t *testing.T) {
			var env struct {
				Plans []domain.Plan `json:"plans"`
			}
			runJSON(t, &env, baseArgs(t, "plan", flag)...)
			for _, p := range env.Plans {
				if len(p.Steps) != 0 {
					t.Errorf("%s planned steps under %s", p.Kind, flag)
				}
				if p.RefusalCode == "" {
					t.Errorf("%s refused without a code", p.Kind)
				}
			}
		})
	}
}

// TestOutputIsByteIdenticalAcrossRuns. Go randomizes map iteration within a
// single process, so running the same command twice in one process is the
// real test — and float addition is not associative, so this covers the money
// as well as the rendering.
func TestOutputIsByteIdenticalAcrossRuns(t *testing.T) {
	for _, sub := range []string{"list", "report", "plan"} {
		t.Run(sub, func(t *testing.T) {
			want := run(t, baseArgs(t, sub)...)
			for i := 0; i < 8; i++ {
				if got := run(t, baseArgs(t, sub)...); got != want {
					t.Fatalf("run %d differs\n--- want ---\n%s\n--- got ---\n%s", i, want, got)
				}
			}
			// Snapshot order must not matter either: the registry routes by
			// the snapshot's own domain field.
			shuffled := []string{sub,
				"--now", fixtureNow.Format("2006-01-02T15:04:05Z07:00"),
				"--scope", fixtureScope, "--region", fixtureRegion,
				"--snapshot", readFixture(t, "lambda-functions.json"),
				"--snapshot", readFixture(t, "ecs-services.json"),
				"--snapshot", readFixture(t, "ec2-volumes.json"),
				"--snapshot", readFixture(t, "ec2-instances.json"),
				"--kube-snapshot", readFixture(t, "cluster.json"),
			}
			if got := run(t, shuffled...); got != want {
				t.Fatalf("output depends on the order snapshots were supplied in\n"+
					"--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

// TestListSurfacesActuationCapabilityPerDomain.
func TestListSurfacesActuationCapabilityPerDomain(t *testing.T) {
	out := run(t, baseArgs(t, "list")...)
	for _, k := range []string{"ec2", "ecs-fargate", "k8s-fargate", "lambda"} {
		if !strings.Contains(out, k) {
			t.Errorf("%s is missing from the list:\n%s", k, out)
		}
	}
	if !strings.Contains(out, "report-only") {
		t.Errorf("no domain is labelled report-only, though none has an actuator:\n%s", out)
	}
}

// TestZeroDomainsBehavesLikeTodaysBinary: design invariant 1 at the CLI.
// A `kilter domains report` with nothing wired must succeed and say nothing,
// not fail.
func TestZeroDomainsBehavesLikeTodaysBinary(t *testing.T) {
	out := run(t, "report", "--now", fixtureNow.Format("2006-01-02T15:04:05Z07:00"),
		"--domain", "lambda")
	if !strings.Contains(out, "lambda") {
		t.Errorf("the registered-but-unfed domain is missing:\n%s", out)
	}
	if !strings.Contains(out, "no Lambda functions learned yet") {
		t.Errorf("an unfed domain does not explain itself:\n%s", out)
	}
	if strings.Contains(out, "$-") {
		t.Errorf("negative money in an empty report:\n%s", out)
	}
}

// TestBadInputIsRefusedRatherThanGuessed.
func TestBadInputIsRefusedRatherThanGuessed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown subcommand", []string{"frobnicate"}, "unknown subcommand"},
		{"no subcommand", nil, "subcommand is required"},
		{"unknown domain", []string{"report", "--domain", "rds"}, "unknown domain"},
		{"known but unwired domain", []string{"report", "--domain", "k8s-nodes"}, "not wired into this binary"},
		{"missing snapshot file", []string{"report", "--snapshot", "testdata/nope.json"}, "no such file"},
		{"bad --now", []string{"report", "--now", "yesterday"}, "--now"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			err := runDomainsTo(&b, tc.args)
			if err == nil {
				t.Fatalf("bad input was accepted:\n%s", b.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSnapshotForAnUnregisteredDomainIsWarnedAboutNotDropped: silently
// discarding collected data is indistinguishable from a broken collector.
func TestSnapshotForAnUnregisteredDomainIsWarnedAboutNotDropped(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, "report",
		"--now", fixtureNow.Format("2006-01-02T15:04:05Z07:00"),
		"--domain", "lambda",
		"--snapshot", readFixture(t, "ec2-instances.json"))
	if len(env.Warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the unregistered ec2 snapshot", env.Warnings)
	}
	if !strings.Contains(env.Warnings[0], "not registered") {
		t.Errorf("warning = %q", env.Warnings[0])
	}
	out := run(t, "report",
		"--now", fixtureNow.Format("2006-01-02T15:04:05Z07:00"),
		"--domain", "lambda",
		"--snapshot", readFixture(t, "ec2-instances.json"))
	if !strings.Contains(out, "Collection warnings") {
		t.Errorf("the warning is not rendered:\n%s", out)
	}
}

// TestOpaquePayloadPreservesLambdaEvidence is the fix for
// pkg/lambda/FINDINGS.md §8, proven rather than asserted.
//
// The generic snapshot shape cannot carry a REPORT record — four numbers whose
// correlation IS the cost claim. Before Snapshot.Payload existed, a function
// crossing the seam could only refuse with `no-report-evidence`. Here the same
// function comes through the CLI with a MEASURED comparison at two memory
// settings, which is the only kind of Lambda claim this engine will make.
func TestOpaquePayloadPreservesLambdaEvidence(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, baseArgs(t, "report", "--domain", "lambda")...)
	rep := env.Report

	if hasCode(rep, "no-report-evidence") {
		t.Fatal("the lossy ingest path is still in use: REPORT evidence did not survive the seam")
	}
	var measured bool
	for _, rec := range rep.Recommendations {
		if strings.Contains(rec.Target.ID, "thumbnailer") {
			measured = true
			if !strings.Contains(rec.Reason, "MEASURED at both settings") {
				t.Errorf("the multi-point claim lost its evidence: %q", rec.Reason)
			}
			if rec.Action != domain.ActionAdvisory {
				t.Errorf("a Lambda recommendation is %q, not advisory", rec.Action)
			}
		}
	}
	if !measured {
		t.Error("the two-memory-point function produced no recommendation")
	}
	// And the fleet's honest default is still a refusal, not an estimate.
	if !hasCode(rep, "single-memory-point") {
		t.Error("the single-memory-point refusal disappeared")
	}
}

// TestFargateCrossoverIsReachableAndAdvisory wires the last of the eight
// packages that had no path to the binary: pkg/crossover, behind
// `kilter analyze --fargate`.
//
// It is advisory by construction — a comparison of two MODELLED bills for the
// same pods, not a delta against anyone's invoice — so this asserts what it
// must never become: a claim, a step, or a savings figure the report totals
// would pick up.
func TestFargateCrossoverIsReachableAndAdvisory(t *testing.T) {
	snap := buildClusterSnapshot()
	rep := fargateCrossover(fixtureNow, snap, loadEmbeddedCatalog(t))
	if rep == nil {
		t.Fatal("the crossover produced nothing for a cluster with a schedulable pod")
	}
	if rep.At != fixtureNow {
		t.Errorf("At = %v; the caller supplies the clock, the package never reads one", rep.At)
	}
	if rep.Verdict == "" {
		t.Error("no verdict")
	}
	// §7 trap 7: the fixture's Fargate VM reports 96 vCPU / 384 GB. If that
	// shell ever reached the node math, the EC2 side would be nonsense.
	if rep.EC2.Purchased.MilliCPU >= 96000 {
		t.Errorf("a Fargate VM's shell capacity reached the node math: %+v", rep.EC2.Purchased)
	}
	// Assumptions are part of the answer, not a footnote — the duty-cycle one
	// in particular, which is the largest known error in the number.
	if len(rep.Assumptions) == 0 {
		t.Error("the crossover states no assumptions")
	}
	if rep.Summary() == "" || rep.Headline() == "" {
		t.Error("the crossover renders nothing")
	}
	// It is an Insight — a finding. Nothing in pkg/plan or pkg/actuate
	// consumes one, which is what keeps it advisory.
	if in := rep.Insight(); in.Kind != "fargate-crossover" {
		t.Errorf("Insight kind = %q", in.Kind)
	}

	// And an empty cluster is a normal answer, not a crash.
	if got := fargateCrossover(fixtureNow, nil, loadEmbeddedCatalog(t)); got != nil {
		t.Errorf("a nil snapshot produced a report: %+v", got)
	}
}

func loadEmbeddedCatalog(t *testing.T) *pricing.Catalog {
	t.Helper()
	cat, err := loadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	return cat
}
