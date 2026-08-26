package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	kcommit "github.com/agenticode/kilter/pkg/pricing/commit"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// These tests drive pkg/rds's REAL collector and REAL domain through the REAL
// CLI entry point over a recorded account. Nothing here links an AWS SDK,
// opens a socket, reads a credential or reads a clock.

// rdsArgs is the input the RDS tests share.
func rdsArgs(t *testing.T, sub string, extra ...string) []string {
	t.Helper()
	args := []string{sub,
		"--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope,
		"--region", fixtureRegion,
		"--domain", "rds",
		"--rds-fixture", readFixture(t, rdsFixtureFileName),
	}
	return append(args, extra...)
}

// writeRDSFixture writes a modified account to a temp file and returns the path.
func writeRDSFixture(t *testing.T, f rdsFixtureFile) string {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRDSIsReachableFromTheBinary is the reason this unit exists. pkg/rds
// shipped 4,980 lines of production Go that the binary could not call.
func TestRDSIsReachableFromTheBinary(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, rdsArgs(t, "report")...)

	dr, ok := env.Report.For(domain.RDS)
	if !ok {
		t.Fatal("the rds domain does not appear in the aggregate report")
	}
	if !dr.Health.Ready {
		t.Errorf("rds is not ready after a collection: %s", dr.Health.Reason)
	}
	// Seven instances went in; every one of them came out with a reason.
	if dr.Health.Targets != 7 {
		t.Errorf("rds tracks %d targets, want 7", dr.Health.Targets)
	}
	if dr.Refused == 0 {
		t.Fatal("rds produced no refusals; the refusals ARE this domain's output")
	}

	// The four codes that carry this domain's whole argument must all be
	// reachable from the binary, not merely present in the package.
	want := map[string]bool{
		krds.ReasonInstanceClassIsAFailover:  false,
		krds.ReasonStorageCannotShrink:       false,
		krds.ReasonFreeableMemoryIsPageCache: false,
		krds.ReasonAuroraNotSupported:        false,
	}
	for _, ref := range env.Report.Refusals {
		if ref.Target.Domain != domain.RDS {
			continue
		}
		if _, ok := want[ref.Code]; ok {
			want[ref.Code] = true
		}
		if ref.Reason == "" {
			t.Errorf("%s refused %s without saying why", ref.Code, ref.Target.ID)
		}
	}
	for code, seen := range want {
		if !seen {
			t.Errorf("refusal code %q never reached the CLI", code)
		}
	}
}

// TestRDSProposesNothingAndClaimsNothing.
//
// pkg/rds/FINDINGS.md §2: Report.Totals.Proposals is 0 in every report this
// unit can produce, and that is the deliverable rather than an omission. The
// wiring must not manufacture one — not through the generic seam, not through
// the ledger, not through the aggregate roll-up.
func TestRDSProposesNothingAndClaimsNothing(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, rdsArgs(t, "report")...)
	for _, rec := range env.Report.Recommendations {
		if rec.Target.Domain == domain.RDS {
			t.Errorf("rds emitted a recommendation for %s; this domain proposes nothing", rec.Target.ID)
		}
	}
	dr, _ := env.Report.For(domain.RDS)
	if dr.ClaimableMonthlyUSD != 0 || dr.GrossMonthlyUSD != 0 {
		t.Errorf("rds claims $%v of $%v gross; every shipped rate is unverified and cannot become a saving",
			dr.ClaimableMonthlyUSD, dr.GrossMonthlyUSD)
	}
}

// TestFreeableMemoryVerdictDiffersByEngineThroughTheWiring is trap 9, asserted
// at the CLI rather than in the package that implements it.
//
// db-pg-primary and db-mysql-multiaz both have a large, complete, flat
// FreeableMemory series. PostgreSQL's is page cache that MemAvailable counts
// as available, so it is never converted into a headroom number at all;
// MySQL's is anonymous buffer-pool memory, so it IS readable and the downsize
// is refused for a different reason. A wiring that flattened the domain
// through domain.Sample would lose exactly this distinction.
func TestFreeableMemoryVerdictDiffersByEngineThroughTheWiring(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, rdsArgs(t, "report")...)

	codes := map[string]map[string]bool{}
	for _, ref := range env.Report.Refusals {
		if ref.Target.Domain != domain.RDS {
			continue
		}
		id := ref.Target.ID
		if codes[id] == nil {
			codes[id] = map[string]bool{}
		}
		codes[id][ref.Code] = true
	}
	pg, my := findRDSTarget(t, codes, "db-pg-primary"), findRDSTarget(t, codes, "db-mysql-multiaz")

	if !codes[pg][krds.ReasonFreeableMemoryIsPageCache] {
		t.Errorf("PostgreSQL did not refuse with %q: %v", krds.ReasonFreeableMemoryIsPageCache, keysOf(codes[pg]))
	}
	if codes[pg][krds.ReasonBufferPoolScalesWithClass] {
		t.Error("PostgreSQL was given MySQL's buffer-pool verdict")
	}
	if !codes[my][krds.ReasonBufferPoolScalesWithClass] {
		t.Errorf("MySQL did not refuse with %q: %v", krds.ReasonBufferPoolScalesWithClass, keysOf(codes[my]))
	}
	if codes[my][krds.ReasonFreeableMemoryIsPageCache] {
		t.Error("MySQL was given PostgreSQL's page-cache verdict")
	}
}

// TestMultiAZBillsTwiceThroughTheWiring: trap 10 survives the CLI. The
// Multi-AZ instance's usage line costs exactly twice the Single-AZ rate for
// the same class, and the storage line does not move.
func TestMultiAZBillsTwiceThroughTheWiring(t *testing.T) {
	lines := rdsBaselineLines(t)
	multi, ok := lines["arn:aws:rds:us-east-1:000000000000:db:db-mysql-multiaz/instance"]
	if !ok {
		t.Fatalf("the Multi-AZ instance produced no usage line: %v", keysOf(lines))
	}
	single, ok := lines["arn:aws:rds:us-east-1:000000000000:db:db-pg-replica/instance"]
	if !ok {
		t.Fatalf("the Single-AZ instance produced no usage line: %v", keysOf(lines))
	}
	if multi.InstanceType != single.InstanceType {
		t.Fatalf("fixture drift: %s vs %s", multi.InstanceType, single.InstanceType)
	}
	// The multiplier is an exact small integer, so this holds to the last bit.
	if multi.ODRate != 2*single.ODRate {
		t.Errorf("Multi-AZ rate $%v, want exactly 2 × the Single-AZ $%v", multi.ODRate, single.ODRate)
	}
	if multi.Deployment != kcommit.RDSMultiAZInstance {
		t.Errorf("deployment = %q, want %q", multi.Deployment, kcommit.RDSMultiAZInstance)
	}
}

// TestRDSUsageLinesEnterTheAccountWideBaseline.
//
// pkg/rds/FINDINGS.md §6.5 says RDS lines must be spliced into whatever the
// other domains contribute, because Compute Savings Plans absorb account-wide
// and a per-domain view over- or under-states absorption. This asserts the
// splice actually happened through the real runtime, and that only priced
// instances contributed.
func TestRDSUsageLinesEnterTheAccountWideBaseline(t *testing.T) {
	lines := rdsBaselineLines(t)
	if len(lines) == 0 {
		t.Fatal("no RDS usage line reached the account-wide baseline")
	}
	for id, l := range lines {
		if l.Kind != kcommit.KindRDS {
			t.Errorf("%s: kind %q, want %q", id, l.Kind, kcommit.KindRDS)
		}
		if l.InstanceType == "" {
			// commit.Usage.Validate requires a class on every KindRDS line,
			// which is what structurally prevents a storage line from ever
			// becoming a covered line.
			t.Errorf("%s: no DB instance class", id)
		}
		if l.ODRate <= 0 {
			t.Errorf("%s: rate $%v — an unpriced instance must not enter the baseline, "+
				"because a zero-rate line makes a reservation look like it is absorbing "+
				"usage that costs nothing", id, l.ODRate)
		}
	}
	// Aurora, the cluster member and the mode=off instance are excluded before
	// pricing, so they cannot appear.
	for _, banned := range []string{"db-aurora", "db-mysql-cluster", "db-legacy", "db-mssql"} {
		for id := range lines {
			if strings.Contains(id, banned) {
				t.Errorf("%s reached the baseline; it was never priced", banned)
			}
		}
	}
}

// TestNoCloudWatchPermissionIsACompleteReportNotAFailure.
//
// §6.2: a caller holding rds:Describe* and not cloudwatch:GetMetricData still
// gets a complete inventory, and every instance in it honestly refuses with
// no-metric-evidence. The wiring must deliver that report rather than an
// error, and it must NOT deliver an idle verdict manufactured out of silence.
func TestNoCloudWatchPermissionIsACompleteReportNotAFailure(t *testing.T) {
	f := buildRDSFixture()
	f.NoMetricsAPI = true
	path := writeRDSFixture(t, f)

	var env reportEnvelope
	runJSON(t, &env, "report", "--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope, "--region", fixtureRegion,
		"--domain", "rds", "--rds-fixture", path, "--json")

	dr, ok := env.Report.For(domain.RDS)
	if !ok || dr.Health.Targets != 7 {
		t.Fatalf("the inventory did not survive the missing metrics permission: %+v", dr)
	}
	var noEvidence int
	for _, ref := range env.Report.Refusals {
		if ref.Code == krds.ReasonNoMetricEvidence {
			noEvidence++
		}
	}
	if noEvidence == 0 {
		t.Error("no instance refused with no-metric-evidence; silence was read as data")
	}
	// The idle advisory is the one that must never fire on silence.
	out := run(t, "report", "--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope, "--region", fixtureRegion,
		"--domain", "rds", "--rds-fixture", path, "--rds-detail")
	if strings.Contains(out, krds.AdvisoryIdleInstance) || strings.Contains(out, krds.AdvisoryIdleReadReplica) {
		t.Errorf("an idle verdict was manufactured from an unanswered CloudWatch:\n%s", out)
	}
}

// TestTheWindowIsClampedAndTheClampIsSaidOutLoud.
//
// 1-minute CloudWatch datapoints live 15 days. A 30-day request does not fail;
// it returns 15 days of data inside a 30-day window, and silence read across
// the other 15 is how "this database had no connections for a month" gets
// manufactured. The collector clamps, and the CLI says so rather than
// rendering the window the operator asked for.
func TestTheWindowIsClampedAndTheClampIsSaidOutLoud(t *testing.T) {
	var env reportEnvelope
	runJSON(t, &env, rdsArgs(t, "report", "--rds-window", "720h")...)
	var found bool
	for _, w := range env.Warnings {
		if strings.Contains(w, "clamped") {
			found = true
		}
	}
	if !found {
		t.Errorf("a 30-day window was accepted silently: %v", env.Warnings)
	}
	out := run(t, rdsArgs(t, "report", "--rds-window", "720h", "--rds-detail")...)
	if strings.Contains(out, "720h0m0s window") {
		t.Errorf("the report renders the REQUESTED window rather than the observed one:\n%s", out)
	}
	if !strings.Contains(out, "360h0m0s window") {
		t.Errorf("the report does not render the clamped 15-day window:\n%s", out)
	}
}

// TestRDSPlanIsRefusedByTheCore.
//
// Not by pkg/rds being polite. Registry.PlanSteps checks Health BEFORE the
// domain is consulted, and no actuator exists for the kind, so there are two
// independent walls in front of an RDS step and the domain's own
// unconditional refusal is the third.
func TestRDSPlanIsRefusedByTheCore(t *testing.T) {
	var env struct {
		Plans []domain.Plan `json:"plans"`
	}
	runJSON(t, &env, rdsArgs(t, "plan")...)
	if len(env.Plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(env.Plans))
	}
	p := env.Plans[0]
	if p.Kind != domain.RDS {
		t.Fatalf("kind = %q", p.Kind)
	}
	if p.Actuatable {
		t.Error("rds claims an actuator; there is no mutating RDS API anywhere in the tree")
	}
	if p.RefusalCode != domain.RefuseReportOnly {
		t.Errorf("refusal = %q (%s), want %q", p.RefusalCode, p.Refusal, domain.RefuseReportOnly)
	}
	if len(p.Steps) != 0 {
		t.Errorf("rds produced %d steps", len(p.Steps))
	}
}

// TestRDSOutputIsShuffleInvariantAndByteIdentical.
//
// Two properties in one, because they fail differently. Repeating in ONE
// process is the real determinism test — Go randomizes map iteration on every
// range — and shuffling the recorded account is the money test: pkg/ecs
// shipped a bug this quarter where float sums varied with addend order, so a
// total that is not sorted before it is summed is not a function of its
// inputs.
func TestRDSOutputIsShuffleInvariantAndByteIdentical(t *testing.T) {
	base := run(t, rdsArgs(t, "report", "--rds-detail")...)
	for i := 0; i < 8; i++ {
		if got := run(t, rdsArgs(t, "report", "--rds-detail")...); got != base {
			t.Fatalf("run %d differs from run 0 in the same process", i)
		}
	}

	// Now permute everything the collector could plausibly walk in a different
	// order: the inventory pages, the cluster list and the reservations.
	for i, perm := range [][]int{
		{6, 5, 4, 3, 2, 1, 0},
		{3, 0, 6, 1, 5, 2, 4},
		{1, 2, 0, 4, 3, 6, 5},
	} {
		f := buildRDSFixture()
		shuffled := make([]krds.DBInstanceRecord, len(f.Instances))
		for j, at := range perm {
			shuffled[j] = f.Instances[at]
		}
		f.Instances = shuffled
		f.Clusters = []krds.DBClusterRecord{f.Clusters[1], f.Clusters[0]}
		path := writeRDSFixture(t, f)

		got := run(t, "report", "--now", fixtureNow.Format(time.RFC3339),
			"--scope", fixtureScope, "--region", fixtureRegion,
			"--domain", "rds", "--rds-fixture", path, "--rds-detail")
		if got != base {
			t.Errorf("permutation %d changed the report; a total that depends on "+
				"input order is not a function of its inputs", i)
		}
	}
}

// TestRDSSnapshotAlsoArrivesThroughTheGenericSeam.
//
// The fixture path is one of two ways in. A collector running elsewhere ships
// a domain.Snapshot with the native snapshot in Payload, and --snapshot routes
// it by its "domain" field like every other domain's. Both must produce the
// same findings, because Payload is the lossless half of the projection.
func TestRDSSnapshotAlsoArrivesThroughTheGenericSeam(t *testing.T) {
	// Collect once, exactly as `kilter domains --rds-fixture` does.
	snap, _, err := collectRDS(t.Context(), readFixture(t, rdsFixtureFileName),
		fixtureScope, fixtureRegion, fixtureNow, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	generic := snap.Generic()
	raw, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rds-snapshot.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	viaSnapshot := run(t, "report", "--now", fixtureNow.Format(time.RFC3339),
		"--scope", fixtureScope, "--region", fixtureRegion,
		"--domain", "rds", "--snapshot", path, "--rds-detail")
	viaFixture := run(t, rdsArgs(t, "report", "--rds-detail")...)
	if viaSnapshot != viaFixture {
		t.Errorf("the generic seam lost evidence the collector path kept:\n--- snapshot ---\n%s\n--- fixture ---\n%s",
			viaSnapshot, viaFixture)
	}
}

// ---------------------------------------------------------------- helpers

// rdsBaselineLines runs the real runtime and returns the RDS half of the
// account-wide commitment baseline, keyed by line ID.
func rdsBaselineLines(t *testing.T) map[string]kcommit.UsageLine {
	t.Helper()
	df := &domainFlags{
		now:       fixtureNow.Format(time.RFC3339),
		scope:     fixtureScope,
		region:    fixtureRegion,
		rdsWindow: 14 * 24 * time.Hour,
	}
	df.kinds = repeatedFlag{"rds"}
	df.rdsFixtures = repeatedFlag{readFixture(t, rdsFixtureFileName)}
	rt, err := buildRuntime(df)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]kcommit.UsageLine{}
	for _, l := range rt.Ledger.Baseline() {
		if l.Kind == kcommit.KindRDS {
			out[l.ID] = l
		}
	}
	return out
}

func findRDSTarget(t *testing.T, codes map[string]map[string]bool, want string) string {
	t.Helper()
	for id := range codes {
		if strings.Contains(id, want) {
			return id
		}
	}
	t.Fatalf("no refusal for %q; saw %v", want, keysOf(codes))
	return ""
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------- adversarial

// hostileLiner is a domain that contributes poisoned usage lines to the
// account-wide commitment baseline.
//
// It is the same attack j1-wire closed one level down. A domain's OUTPUT
// reaches a shared structure the core owns, and until this unit nothing
// checked it: the baseline is what every OTHER domain's net savings are
// computed against, so a domain that inflates it makes some other domain's
// stranded commitment look absorbed and its saving look real.
type hostileLiner struct {
	domain.Domain
	lines []kcommit.UsageLine
}

func (h *hostileLiner) UsageLines(time.Time, domain.Netter) []kcommit.UsageLine {
	out := make([]kcommit.UsageLine, len(h.lines))
	copy(out, h.lines)
	return out
}

// honestPart is the minimum a registry needs to hold a hostile liner.
type honestPart struct{ kind domain.Kind }

func (h *honestPart) Kind() domain.Kind            { return h.kind }
func (h *honestPart) Learn(*domain.Snapshot) error { return nil }
func (h *honestPart) Recommend(time.Time, domain.Netter) []domain.Recommendation {
	return nil
}
func (h *honestPart) PlanSteps([]domain.Recommendation, domain.Guard) ([]domain.Step, error) {
	return nil, domain.ErrReportOnly
}
func (h *honestPart) Health(time.Time) domain.Health {
	return domain.Health{Kind: h.kind, Ready: true, ReportOnly: true}
}
func (h *honestPart) Checkpoint() ([]byte, error) { return nil, nil }
func (h *honestPart) Restore([]byte) error        { return nil }

// TestPoisonedUsageLinesNeverReachTheAccountWideBaseline.
//
// Every rejection direction is conservative: a dropped line means less usage
// to absorb a commitment, which means MORE apparent stranding and a SMALLER
// claimed saving. Under-claiming is the only safe way to be wrong about a
// number somebody puts in a business case.
func TestPoisonedUsageLinesNeverReachTheAccountWideBaseline(t *testing.T) {
	good := kcommit.UsageLine{
		ID: "arn:honest/instance", Kind: kcommit.KindRDS, Region: fixtureRegion,
		InstanceType: "db.r6i.large", Engine: "postgresql",
		Deployment: kcommit.RDSSingleAZ, Unit: "Instance-Hours", Quantity: 1, ODRate: 0.24,
	}
	poisoned := []kcommit.UsageLine{
		good,
		// Anonymous: never matches in the splice, so it is always appended.
		func() kcommit.UsageLine { l := good; l.ID = ""; return l }(),
		// Free money: prices real usage at nothing.
		func() kcommit.UsageLine { l := good; l.ID = "arn:zero/instance"; l.ODRate = 0; return l }(),
		func() kcommit.UsageLine {
			l := good
			l.ID = "arn:nan/instance"
			l.ODRate = math.NaN()
			return l
		}(),
		func() kcommit.UsageLine { l := good; l.ID = "arn:noqty/instance"; l.Quantity = 0; return l }(),
		// A second line under an ID already contributed: the splice REPLACES,
		// so this silently rewrites the honest one.
		func() kcommit.UsageLine { l := good; l.ODRate = 999; return l }(),
	}

	reg := domain.NewRegistry()
	if err := reg.Register(&hostileLiner{Domain: &honestPart{kind: domain.RDS}, lines: poisoned}); err != nil {
		t.Fatal(err)
	}
	ledger, warnings := buildLedger(reg, nil, fixtureNow)

	base := ledger.Baseline()
	if len(base) != 1 {
		t.Fatalf("got %d baseline lines, want only the honest one: %+v", len(base), base)
	}
	if base[0].ODRate != 0.24 {
		t.Errorf("the honest line was rewritten to $%v", base[0].ODRate)
	}
	if len(warnings) != len(poisoned)-1 {
		t.Errorf("got %d warnings for %d poisoned lines: %v", len(warnings), len(poisoned)-1, warnings)
	}
	// A drop is never silent.
	for _, w := range warnings {
		if !strings.Contains(w, "dropped a usage line") {
			t.Errorf("warning does not say what happened: %q", w)
		}
	}
}

// TestTheShippedDomainsContributeCleanBaselineLines is the control: the gate
// above must be a no-op for every domain this binary actually wires, or it is
// silently shrinking a real baseline.
func TestTheShippedDomainsContributeCleanBaselineLines(t *testing.T) {
	df := &domainFlags{
		now: fixtureNow.Format(time.RFC3339), scope: fixtureScope, region: fixtureRegion,
		rdsWindow: 14 * 24 * time.Hour,
	}
	df.snapshots = repeatedFlag{
		readFixture(t, "ec2-instances.json"), readFixture(t, "ec2-volumes.json"),
		readFixture(t, "ecs-services.json"), readFixture(t, "lambda-functions.json"),
	}
	df.rdsFixtures = repeatedFlag{readFixture(t, rdsFixtureFileName)}
	rt, err := buildRuntime(df)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range rt.Warnings {
		if strings.Contains(w, "dropped a usage line") {
			t.Errorf("a shipped domain's baseline line was dropped: %s", w)
		}
	}
	if len(rt.Ledger.Baseline()) == 0 {
		t.Fatal("the account-wide baseline is empty; the control proves nothing")
	}
}
