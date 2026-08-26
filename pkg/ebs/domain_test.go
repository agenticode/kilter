package ebs

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// assess collects a fixture into a fresh domain and returns its report.
func assess(t *testing.T, cfg Config, f *Fixture, now time.Time, ledger domain.Netter) (*Domain, Report) {
	t.Helper()
	d := newDomain(t, cfg)
	collectInto(t, d, f, now)
	return d, d.Assess(now, ledger)
}

// TestProposesAtMeasuredParity is the happy path on the volume class the naive
// rule gets wrong: 4 TiB, sustaining 12,000 gp2 IOPS, measured at 4,000.
func TestProposesAtMeasuredParity(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
		measured("vol-a", base, 20, 4000, 100))

	_, rep := assess(t, testConfig(), f, clock.Now(), nil)
	a, rec := wantProposal(t, rep, "vol-a")

	// 4,000 IOPS × 1.3 headroom = 5,200; 100 MiB/s × 1.3 = 130.
	if rec.Proposed.Attr(AttrVolumeType) != VolumeTypeGP3 {
		t.Fatalf("proposed type = %q", rec.Proposed.Attr(AttrVolumeType))
	}
	if got := rec.Proposed.Attr(AttrIOPS); got != "5200" {
		t.Errorf("proposed IOPS = %s, want 5200 (p99 4000 × 1.3)", got)
	}
	if got := rec.Proposed.Attr(AttrThroughputMBps); got != "130" {
		t.Errorf("proposed throughput = %s, want 130", got)
	}
	if rec.Action != domain.ActionInPlace {
		t.Errorf("action = %q, want %q (ModifyVolume is online)", rec.Action, domain.ActionInPlace)
	}
	// $400 → $320 + 2200×0.005 + 5×0.06 = $331.30.
	if got := a.Parity.ProposedMonthlyUSD; math.Abs(got-331.30) > 1e-9 {
		t.Errorf("proposed cost $%.2f/mo, want $331.30", got)
	}
	if got := rec.GrossSavingsMonthlyUSD; math.Abs(got-68.70) > 1e-9 {
		t.Errorf("gross saving $%.2f/mo, want $68.70", got)
	}
	// With no ledger, net equals gross: no known commitment can strand
	// anything.
	if rec.NetSavingsMonthlyUSD != rec.GrossSavingsMonthlyUSD {
		t.Errorf("net $%.2f ≠ gross $%.2f with no commitment information",
			rec.NetSavingsMonthlyUSD, rec.GrossSavingsMonthlyUSD)
	}
	if rec.Risk != plan.RiskMedium {
		t.Errorf("risk = %q, want %q: this provisions below gp2's delivered baseline",
			rec.Risk, plan.RiskMedium)
	}
	if len(rec.Evidence) < 4 {
		t.Errorf("evidence = %d items, want the measured percentiles, the gp2 baseline and the floor",
			len(rec.Evidence))
	}
	if rep.ClaimableMonthlyUSD != rec.NetSavingsMonthlyUSD {
		t.Errorf("report claims $%.2f, recommendation claims $%.2f",
			rep.ClaimableMonthlyUSD, rec.NetSavingsMonthlyUSD)
	}
}

// TestNaiveDowngradeIsNeverProposed is §7 trap 6 at the domain level: the
// cheap-and-wrong configuration is reported as degrading and never proposed.
func TestNaiveDowngradeIsNeverProposed(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
		measured("vol-a", base, 20, 9000, 200))

	_, rep := assess(t, testConfig(), f, clock.Now(), nil)
	a, rec := wantProposal(t, rep, "vol-a")

	if !a.Parity.NaiveDegrades {
		t.Fatal("the naive gp3 baseline was not reported as degrading")
	}
	if got := rec.Proposed.Attr(AttrIOPS); got != "11700" {
		t.Errorf("proposed IOPS = %s, want 11700 (9000 × 1.3)", got)
	}
	if a.Parity.NaiveMonthlyUSD >= a.Parity.ProposedMonthlyUSD {
		t.Fatal("this fixture no longer exercises the trap: the naive config is not cheaper")
	}
	if !strings.Contains(rec.Reason, "naive") {
		t.Errorf("the reason does not mention what the naive rule would have done: %q", rec.Reason)
	}
	// Demand gp3 cannot meet is refused outright, not clamped.
	f2 := newFixture(clock, []VolumeRecord{gp2Volume("vol-b", 4000)},
		measured("vol-b", base, 20, 13000, 200))
	_, rep2 := assess(t, testConfig(), f2, clock.Now(), nil)
	wantRefusal(t, rep2, "vol-b", ReasonExceedsGP3)
}

// TestShortWindowFloorsAtGP2Baseline is §4.7's minimum-window rule: below it a
// proposal may not go under gp2's delivered baseline, so a thin observation
// can only ever produce a same-or-better volume.
func TestShortWindowFloorsAtGP2Baseline(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	cfg := testConfig()
	cfg.MinWindow = 4 * time.Hour // the 20 recorded points span 95 minutes
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
		measured("vol-a", base, 20, 4000, 100))

	_, rep := assess(t, cfg, f, clock.Now(), nil)
	a, rec := wantProposal(t, rep, "vol-a")

	if a.Observed.Floor != FloorGP2Baseline {
		t.Fatalf("floor = %v, want %v", a.Observed.Floor, FloorGP2Baseline)
	}
	if got := rec.Proposed.Attr(AttrIOPS); got != "12000" {
		t.Errorf("proposed IOPS = %s, want gp2's delivered baseline of 12000", got)
	}
	if got := rec.Proposed.Attr(AttrThroughputMBps); got != "250" {
		t.Errorf("proposed throughput = %s, want gp2's 250 MiB/s ceiling", got)
	}
	if rec.Risk != plan.RiskLow {
		t.Errorf("risk = %q, want %q: a floored conversion degrades nothing", rec.Risk, plan.RiskLow)
	}
	if !strings.Contains(rec.Reason, "floored") {
		t.Errorf("reason does not say the proposal was floored: %q", rec.Reason)
	}

	// A volume in the 334–375 GiB band cannot pay for its own throughput
	// parity, so a short window turns into "wait", not "no".
	f2 := newFixture(clock, []VolumeRecord{gp2Volume("vol-b", 350)},
		measured("vol-b", base, 20, 200, 40))
	_, rep2 := assess(t, cfg, f2, clock.Now(), nil)
	ref := wantRefusal(t, rep2, "vol-b", ReasonInsufficientWindow)
	if !strings.Contains(ref.Refusal.Reason, "delivered baseline") {
		t.Errorf("refusal does not explain the floor: %q", ref.Refusal.Reason)
	}
}

// TestRefusalMatrix walks every reason this domain declines to act, because a
// refusal without a reason is indistinguishable from a bug.
func TestRefusalMatrix(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)

	modifying := gp2Volume("vol-modifying", 500)
	attaching := gp2Volume("vol-attaching", 500)
	attaching.Attachments = []VolumeAttachment{{InstanceID: "i-x", State: AttachmentAttaching}}
	multi := gp2Volume("vol-multi", 500)
	multi.MultiAttachEnabled = true
	errored := gp2Volume("vol-error", 500)
	errored.State = VolumeStateError
	tiny := gp2Volume("vol-tiny", 40)
	off := gp2Volume("vol-off", 500, Tag{Key: TagKilterMode, Value: guard.ModeOff})
	cooling := gp2Volume("vol-cooling", 500)
	unmeasured := gp2Volume("vol-unmeasured", 500)
	thin := gp2Volume("vol-thin", 500)

	vols := []VolumeRecord{
		modifying, attaching, multi, errored, tiny, off, cooling, unmeasured, thin,
		gp3Volume("vol-gp3", 500, 3000, 125),
		{VolumeID: "vol-io2", VolumeType: VolumeTypeIO2, SizeGiB: 500, State: VolumeStateInUse},
		{VolumeID: "vol-st1", VolumeType: VolumeTypeST1, SizeGiB: 500, State: VolumeStateInUse},
	}
	var series []RecordedSeries
	for _, id := range []string{"vol-modifying", "vol-attaching", "vol-multi", "vol-error",
		"vol-tiny", "vol-off", "vol-cooling"} {
		series = append(series, measured(id, base, 20, 200, 40)...)
	}
	series = append(series, measured("vol-thin", base, 3, 200, 40)...)

	f := newFixture(clock, vols, series)
	f.ModificationPages = []DescribeVolumesModificationsOutput{{Modifications: []VolumeModification{
		{VolumeID: "vol-modifying", ModificationState: ModificationModifying, StartTime: now.Add(-time.Minute)},
		{VolumeID: "vol-cooling", ModificationState: ModificationCompleted,
			StartTime: now.Add(-2 * time.Hour), EndTime: now.Add(-time.Hour)},
	}}}

	cfg := testConfig()
	cfg.MinSamples = 12
	_, rep := assess(t, cfg, f, now, nil)

	for _, c := range []struct{ id, code string }{
		{"vol-off", ReasonModeOff},
		{"vol-gp3", ReasonNotGP2},
		{"vol-io2", ReasonNotGP2},
		{"vol-st1", ReasonNotGP2},
		{"vol-error", ReasonVolumeState},
		{"vol-multi", ReasonMultiAttach},
		{"vol-attaching", ReasonAttachmentTransition},
		{"vol-modifying", ReasonModificationInProgress},
		{"vol-cooling", ReasonCooldown},
		{"vol-unmeasured", ReasonUnmeasured},
		{"vol-thin", ReasonInsufficientSamples},
		{"vol-tiny", ReasonBelowMinSavings},
	} {
		a := wantRefusal(t, rep, c.id, c.code)
		if a.Refusal.Reason == "" {
			t.Errorf("%s: refusal %s has no prose", c.id, c.code)
		}
	}
	if rep.Proposed != 0 {
		t.Errorf("%d proposals from a fixture of nothing but refusals", rep.Proposed)
	}
	if rep.Refused != len(vols) {
		t.Errorf("%d refusals for %d volumes: some volume produced no verdict at all", rep.Refused, len(vols))
	}
}

// TestModeRecommendStaysVisible: a mode=recommend volume keeps its proposal in
// the report and loses only the right to be planned. A silently dropped
// recommendation is indistinguishable from a bug.
func TestModeRecommendStaysVisible(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock,
		[]VolumeRecord{gp2Volume("vol-a", 4000, Tag{Key: TagKilterMode, Value: guard.ModeRecommend})},
		measured("vol-a", base, 20, 4000, 100))

	d, rep := assess(t, testConfig(), f, clock.Now(), nil)
	_, rec := wantProposal(t, rep, "vol-a")
	if !rec.Suppressed || rec.SuppressCode != domain.SuppressModeRecommend {
		t.Fatalf("suppressed=%v code=%q, want the mode-recommend code", rec.Suppressed, rec.SuppressCode)
	}
	if rec.ClaimableMonthlyUSD() != 0 {
		t.Errorf("a suppressed recommendation claims $%.2f", rec.ClaimableMonthlyUSD())
	}
	if rep.ClaimableMonthlyUSD != 0 {
		t.Errorf("the report claims $%.2f from a suppressed proposal", rep.ClaimableMonthlyUSD)
	}
	steps, err := d.PlanSteps(d.Recommend(clock.Now(), nil), domain.Guard{Now: clock.Now()})
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("planned %d step(s) for a mode=recommend volume", len(steps))
	}
}

// suppressingNetter stands in for a commitment ledger that refuses a change.
type suppressingNetter struct{ called int }

func (n *suppressingNetter) Net(before, after []commit.UsageLine) commit.Assessment {
	n.called++
	return commit.Assessment{
		NetHourlyUSD: -1, NetMonthlyUSD: -730,
		GrossHourlyUSD: 1, GrossMonthlyUSD: 730,
		Suppressed: true, ReasonCode: commit.ReasonCommitmentNegative,
		Reason:    "stranded commitment",
		ValidFrom: base.Add(365 * 24 * time.Hour),
	}
}

// TestSavingsGoThroughTheCommitmentPath: claims are Bill() deltas, and a
// commitment-negative verdict suppresses the recommendation with its reason
// and its expiry attached (§7 trap 1).
func TestSavingsGoThroughTheCommitmentPath(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
		measured("vol-a", base, 20, 4000, 100))

	netter := &suppressingNetter{}
	_, rep := assess(t, testConfig(), f, clock.Now(), netter)
	_, rec := wantProposal(t, rep, "vol-a")
	if netter.called == 0 {
		t.Fatal("the ledger was never consulted: savings were claimed at list price")
	}
	if !rec.Suppressed || rec.SuppressCode != commit.ReasonCommitmentNegative {
		t.Fatalf("suppressed=%v code=%q", rec.Suppressed, rec.SuppressCode)
	}
	if rec.NetSavingsMonthlyUSD > 0 {
		t.Errorf("net saving $%.2f on a suppressed recommendation", rec.NetSavingsMonthlyUSD)
	}
	if rec.ValidFrom.IsZero() {
		t.Error("the suppression carries no expiry, so it can never lapse on its own")
	}
	if !strings.Contains(rec.Reason, "stranded commitment") {
		t.Errorf("the ledger's reason was dropped: %q", rec.Reason)
	}
}

// TestEBSIsNeverAbsorbedByACommitment: no Savings Plan and no Reserved
// Instance covers EBS storage, so a real ledger must leave the numbers alone.
// Getting this wrong would silently zero out every EBS saving in an account
// with a Compute SP.
func TestEBSIsNeverAbsorbedByACommitment(t *testing.T) {
	clock := newClock(base.Add(48 * time.Hour))
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
		measured("vol-a", base, 20, 4000, 100))

	inv := &commit.Inventory{
		SavingsPlans: []commit.SavingsPlan{{
			ID: "sp-1", Type: commit.SPCompute, Region: "us-east-1",
			CommitmentUSDPerHour: 10, Expires: base.Add(365 * 24 * time.Hour),
		}},
	}
	ledger := domain.NewLedger(inv, commit.Usage{})
	_, rep := assess(t, testConfig(), f, clock.Now(), ledger)
	_, rec := wantProposal(t, rep, "vol-a")

	if rec.Suppressed {
		t.Fatalf("a Compute SP suppressed an EBS change: %s", rec.Reason)
	}
	if math.Abs(rec.NetSavingsMonthlyUSD-rec.GrossSavingsMonthlyUSD) > 1e-6 {
		t.Errorf("net $%.4f ≠ gross $%.4f: an EBS line was absorbed by a commitment",
			rec.NetSavingsMonthlyUSD, rec.GrossSavingsMonthlyUSD)
	}
}

// TestPlanStepsGuardrails: the trust machinery is domain-neutral, and this
// domain must not find a way around it.
func TestPlanStepsGuardrails(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000), gp2Volume("vol-b", 8000)},
		append(measured("vol-a", base, 20, 4000, 100), measured("vol-b", base, 20, 4000, 100)...))
	d, _ := assess(t, testConfig(), f, now, nil)
	recs := d.Recommend(now, nil)
	if len(recs) != 2 {
		t.Fatalf("got %d recommendations, want 2", len(recs))
	}

	windows, err := guard.ParseWindows("Mon-Fri 22:00-23:00")
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	for _, c := range []struct {
		name string
		g    domain.Guard
		want error
	}{
		{"freeze", domain.Guard{Now: now, Freeze: true}, domain.ErrFrozen},
		{"breaker", domain.Guard{Now: now, BreakerOpen: true}, domain.ErrBreakerOpen},
		{"outside window", domain.Guard{Now: now, Windows: windows}, domain.ErrOutsideWindow},
	} {
		if _, err := d.PlanSteps(recs, c.g); err == nil || !strings.Contains(err.Error(), c.want.Error()) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}

	steps, err := d.PlanSteps(recs, domain.Guard{Now: now})
	if err != nil {
		t.Fatalf("PlanSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	for i, s := range steps {
		if s.Seq != i+1 {
			t.Errorf("step %d has Seq %d", i, s.Seq)
		}
		if want := domain.StepKey(s.Target, s.From, s.To); s.Key != want {
			t.Errorf("step key %q does not hash its own contents (%q)", s.Key, want)
		}
		if s.From.Attr(AttrVolumeType) != VolumeTypeGP2 || s.To.Attr(AttrVolumeType) != VolumeTypeGP3 {
			t.Errorf("step %d is not a gp2 → gp3 modification: %+v", i, s)
		}
		if !strings.Contains(s.Detail, "gp2") || !strings.Contains(s.Detail, "gp3") {
			t.Errorf("step detail does not describe the change: %q", s.Detail)
		}
	}

	capped, err := d.PlanSteps(recs, domain.Guard{Now: now, MaxSteps: 1})
	if err != nil || len(capped) != 1 {
		t.Errorf("MaxSteps=1 produced %d step(s), err=%v", len(capped), err)
	}

	// A recommendation from another domain, or with an action this domain
	// cannot perform, is a wiring bug and is refused rather than executed.
	alien := recs[0]
	alien.Target.Domain = domain.Lambda
	if _, err := d.PlanSteps([]domain.Recommendation{alien}, domain.Guard{Now: now}); err == nil {
		t.Error("a recommendation for another domain was planned")
	}
	rolling := recs[0]
	rolling.Action = domain.ActionRolling
	if _, err := d.PlanSteps([]domain.Recommendation{rolling}, domain.Guard{Now: now}); err == nil {
		t.Error("a rolling action was planned by a domain that only does in-place")
	}
}

// TestReportOnlyIsEnforcedByTheCore: a domain with no actuator wired may
// recommend and may not plan, and the refusal comes from the registry as well
// as from the domain.
func TestReportOnlyIsEnforcedByTheCore(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	cfg := testConfig()
	cfg.ActuationAvailable = false
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)}, measured("vol-a", base, 20, 4000, 100))
	d, _ := assess(t, cfg, f, now, nil)

	reg := domain.NewRegistry()
	if err := reg.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	recs := reg.Recommend(now, nil)
	if len(recs) != 1 {
		t.Fatalf("registry produced %d recommendations, want 1", len(recs))
	}
	if recs[0].Target.Domain != Kind {
		t.Errorf("registry stamped domain %q", recs[0].Target.Domain)
	}
	if _, err := reg.PlanSteps(Kind, recs, domain.Guard{Now: now}); err == nil ||
		!strings.Contains(err.Error(), domain.ErrReportOnly.Error()) {
		t.Errorf("registry planned steps for a report-only domain: %v", err)
	}
	if _, err := d.PlanSteps(recs, domain.Guard{Now: now}); err == nil {
		t.Error("the domain planned steps for itself while report-only")
	}
	h := reg.Health(now)
	if len(h) != 1 || !h[0].ReportOnly || h[0].Reason == "" {
		t.Errorf("health = %+v, want report-only with a reason", h)
	}
}

func TestHealthStates(t *testing.T) {
	now := base.Add(48 * time.Hour)
	d := newDomain(t, testConfig())

	if h := d.Health(now); h.Ready || h.Reason == "" {
		t.Errorf("a domain that learned nothing reports %+v", h)
	}
	if err := d.Learn(nil); err != nil {
		t.Errorf("Learn(nil): %v", err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: domain.Lambda}); err == nil {
		t.Error("a snapshot for another domain was accepted")
	}
	if err := d.Learn(&domain.Snapshot{Domain: Kind, Timestamp: now}); err != nil {
		t.Errorf("an empty snapshot errored instead of degrading: %v", err)
	}
	if h := d.Health(now); h.Ready {
		t.Error("an empty snapshot left the domain ready")
	}

	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)}, measured("vol-a", base, 20, 4000, 100))
	collectInto(t, d, f, now)
	if h := d.Health(now); !h.Ready || h.ReportOnly || h.Targets != 1 {
		t.Errorf("after a clean collection: %+v", h)
	}
	// A stale collector stops the domain being ready, and the reason says so.
	late := now.Add(DefaultStaleAfter + time.Minute)
	if h := d.Health(late); h.Ready || !strings.Contains(h.Reason, "collector as down") {
		t.Errorf("stale health = %+v", h)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000), gp2Volume("vol-b", 2000)},
		append(measured("vol-a", base, 20, 4000, 100), measured("vol-b", base, 20, 900, 30)...))
	d, before := assess(t, testConfig(), f, now, nil)

	blob, err := d.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	again, err := d.Checkpoint()
	if err != nil || string(blob) != string(again) {
		t.Fatal("Checkpoint is not deterministic")
	}

	restored := newDomain(t, testConfig())
	if err := restored.Restore(blob); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	after := restored.Assess(now, nil)
	if jsonOf(t, before) != jsonOf(t, after) {
		t.Error("a restored domain does not report what the original reported")
	}
	if err := restored.Restore([]byte(`{"version":99}`)); err == nil {
		t.Error("an unknown checkpoint version was accepted")
	}
	if err := restored.Restore([]byte("{")); err == nil {
		t.Error("malformed checkpoint bytes were accepted")
	}
	if err := restored.Restore(nil); err != nil {
		t.Errorf("Restore(nil): %v", err)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestOutputIsShuffleInvariant pins determinism: the report must not depend on
// the order volumes, samples or tags arrived in, and Go randomizes map order
// on purpose to catch exactly this.
func TestOutputIsShuffleInvariant(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)

	vols := []VolumeRecord{
		gp2Volume("vol-a", 4000, Tag{Key: "Name", Value: "a"}, Tag{Key: "env", Value: "prod"}),
		gp2Volume("vol-b", 2000, Tag{Key: "env", Value: "dev"}, Tag{Key: "Name", Value: "b"}),
		gp2Volume("vol-c", 350),
		gp3Volume("vol-d", 500, 3000, 125),
	}
	var series []RecordedSeries
	for i, id := range []string{"vol-a", "vol-b", "vol-c"} {
		series = append(series, measured(id, base, 20, float64(1000*(i+1)), 40)...)
	}

	want := ""
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 8; round++ {
		v := append([]VolumeRecord(nil), vols...)
		s := append([]RecordedSeries(nil), series...)
		rng.Shuffle(len(v), func(i, j int) { v[i], v[j] = v[j], v[i] })
		rng.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })

		f := newFixture(clock, v, s)
		_, rep := assess(t, testConfig(), f, now, nil)
		got := jsonOf(t, rep)
		if round == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("report changed with input order\nround %d: %s\nround 0: %s", round, got, want)
		}
	}
}

// TestLearnForgetsDeletedVolumes: learned state for a volume that left the
// account would otherwise be recommended forever.
func TestLearnForgetsDeletedVolumes(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000), gp2Volume("vol-b", 4000)},
		append(measured("vol-a", base, 20, 4000, 100), measured("vol-b", base, 20, 4000, 100)...))
	d, rep := assess(t, testConfig(), f, now, nil)
	if len(rep.Assessments) != 2 {
		t.Fatalf("got %d assessments, want 2", len(rep.Assessments))
	}

	f2 := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)}, measured("vol-a", base, 20, 4000, 100))
	collectInto(t, d, f2, now.Add(time.Hour))
	rep2 := d.Assess(now.Add(time.Hour), nil)
	if len(rep2.Assessments) != 1 || rep2.Assessments[0].Ref.ID != "vol-a" {
		t.Fatalf("a deleted volume survived: %d assessments", len(rep2.Assessments))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"headroom below 1", Config{IOPSHeadroom: 0.5}},
		{"percentile above 1", Config{IOPSPercentile: 1.5}},
		{"coverage above 1", Config{MinCoverage: 2}},
		{"unknown mode", Config{DefaultMode: "sometimes"}},
		{"broken rates", Config{Rates: Rates{GP2GBMonthUSD: -1, GP3GBMonthUSD: 1,
			GP3IOPSMonthUSD: 1, GP3ThroughputMonthUSD: 1}}},
	} {
		if _, err := New(c.cfg); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if _, err := New(Config{}); err != nil {
		t.Errorf("the zero config should default cleanly: %v", err)
	}
}

// TestSamplesAreDeduplicatedByTimestamp: overlapping snapshots are the normal
// case (a 14-day lookback re-collected hourly), and double-counting them would
// inflate the sample count that confidence and the window gate read.
func TestSamplesAreDeduplicatedByTimestamp(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)}, measured("vol-a", base, 20, 4000, 100))
	d, rep := assess(t, testConfig(), f, now, nil)
	first := only(t, rep, "vol-a").Observed.Samples

	collectInto(t, d, f, now.Add(time.Hour))
	second := only(t, d.Assess(now.Add(time.Hour), nil), "vol-a").Observed.Samples
	if first != second {
		t.Errorf("re-learning the same window changed the sample count: %d → %d", first, second)
	}
}

// TestLearnIgnoresNonVolumeTargets: the ec2 domain kind covers instances as
// well as volumes (see [Kind]), so a composite collector's snapshot carries
// both. This domain must read its half and leave the rest alone — and above
// all must not conclude that its volumes were deleted.
func TestLearnIgnoresNonVolumeTargets(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)}, measured("vol-a", base, 20, 4000, 100))
	d, rep := assess(t, testConfig(), f, now, nil)
	if len(rep.Assessments) != 1 {
		t.Fatalf("got %d assessments, want 1", len(rep.Assessments))
	}

	instances := &domain.Snapshot{
		Domain: Kind, Scope: "123456789012/us-east-1", Timestamp: now.Add(time.Minute),
		Targets: []domain.Target{{
			Ref:  domain.TargetRef{Domain: Kind, Scope: "123456789012/us-east-1", ID: "i-0abc"},
			Spec: domain.Spec{Attrs: map[string]string{"instanceType": "m5.xlarge"}},
		}},
	}
	if err := d.Learn(instances); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	rep2 := d.Assess(now.Add(time.Minute), nil)
	if len(rep2.Assessments) != 1 || rep2.Assessments[0].Ref.ID != "vol-a" {
		t.Fatalf("an instance-only snapshot changed the volume set: %+v", rep2.Assessments)
	}
	if _, rec := wantProposal(t, rep2, "vol-a"); rec.Target.ID != "vol-a" {
		t.Error("the volume stopped being recommended after an instance snapshot")
	}
}
