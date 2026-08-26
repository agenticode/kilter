package fargate

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// TestDegradesToReportOnly walks every way this domain can lose sight of its
// targets. In all of them it must keep reporting and stop planning — never the
// other way round, and never a panic.
func TestDegradesToReportOnly(t *testing.T) {
	t.Run("never learned", func(t *testing.T) {
		d := newDomain(t)
		h := d.Health(now)
		if h.Ready || !h.ReportOnly {
			t.Fatalf("health = %+v, want not-ready and report-only", h)
		}
		if !contains(h.Reason, "collector") {
			t.Errorf("reason %q does not name the missing collector", h.Reason)
		}
		if got := d.Recommend(now, nil); got != nil {
			t.Errorf("recommended %d things with no data", len(got))
		}
		if _, err := d.PlanSteps(nil, domain.Guard{Now: now}); !errors.Is(err, domain.ErrReportOnly) {
			t.Errorf("PlanSteps = %v, want ErrReportOnly", err)
		}
	})

	t.Run("collector shipped no cluster payload", func(t *testing.T) {
		d := newDomain(t)
		if err := d.Learn(&domain.Snapshot{Domain: Kind, Timestamp: now}); err != nil {
			t.Fatalf("a payload-less snapshot must degrade, not fail: %v", err)
		}
		h := d.Health(now)
		if h.Ready || !h.ReportOnly || !contains(h.Reason, "no cluster snapshot") {
			t.Fatalf("health = %+v", h)
		}
	})

	t.Run("partial collection", func(t *testing.T) {
		d := newDomain(t)
		snap := cluster(now, 300, 72*time.Hour, cliff())
		if err := d.Learn(&domain.Snapshot{Domain: Kind, Timestamp: now, Cluster: snap,
			Stale: true, StaleReason: "metrics.k8s.io timed out for 2 of 5 namespaces"}); err != nil {
			t.Fatal(err)
		}
		h := d.Health(now)
		if h.Ready || !h.ReportOnly || !contains(h.Reason, "timed out") {
			t.Fatalf("health = %+v, want the collector's own reason carried through", h)
		}
		// Learning still happened: what did arrive is still worth reporting.
		if got := d.Recommend(now, nil); len(got) == 0 {
			t.Error("a partial collection produced no recommendations at all")
		}
	})

	t.Run("stale by age", func(t *testing.T) {
		d := newDomain(t)
		learn(t, d, cluster(now, 300, 72*time.Hour, cliff()))
		if h := d.Health(now); h.ReportOnly {
			t.Fatalf("fresh domain is report-only: %+v", h)
		}
		h := d.Health(now.Add(time.Hour))
		if h.Ready || !h.ReportOnly || !contains(h.Reason, "old") {
			t.Fatalf("health after an hour = %+v", h)
		}
	})

	t.Run("actuation not wired", func(t *testing.T) {
		d := newDomain(t, func(c *Config) { c.ActuationAvailable = false })
		learn(t, d, cluster(now, 300, 72*time.Hour, cliff()))
		h := d.Health(now)
		if !h.Ready {
			t.Fatalf("a domain with data is ready even without an actuator: %+v", h)
		}
		if !h.ReportOnly || !contains(h.Reason, "actuation") {
			t.Fatalf("health = %+v, want report-only with an actuation reason", h)
		}
		// It still recommends — report-only means REPORT.
		recs := d.Recommend(now, nil)
		if len(recs) == 0 {
			t.Fatal("a report-only domain stopped reporting")
		}
		if _, err := d.PlanSteps(recs, domain.Guard{Now: now}); !errors.Is(err, domain.ErrReportOnly) {
			t.Fatalf("PlanSteps = %v, want ErrReportOnly", err)
		}
	})

	t.Run("default construction is report-only", func(t *testing.T) {
		// The default Config does NOT set ActuationAvailable: forgetting to
		// wire credentials must never read as permission to act.
		d, err := New(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		learn(t, d, cluster(now, 300, 72*time.Hour, cliff()))
		if h := d.Health(now); !h.ReportOnly {
			t.Fatalf("a default-constructed domain may act: %+v", h)
		}
	})
}

func TestLearnInputHandling(t *testing.T) {
	d := newDomain(t)
	if err := d.Learn(nil); err != nil {
		t.Errorf("Learn(nil) = %v", err)
	}
	err := d.Learn(&domain.Snapshot{Domain: domain.EC2, Timestamp: now})
	if !errors.Is(err, domain.ErrWrongDomain) {
		t.Errorf("Learn(ec2 snapshot) = %v, want ErrWrongDomain", err)
	}
	// An unlabelled snapshot is accepted: the registry routes by kind, so the
	// field is redundant there and optional for a direct caller.
	if err := d.Learn(&domain.Snapshot{Timestamp: now,
		Cluster: cluster(now, 300, 72*time.Hour, cliff())}); err != nil {
		t.Errorf("Learn(unlabelled) = %v", err)
	}
}

// TestFargateNodesNeverPricedAsNodes: the fixture's Fargate VMs report 96 vCPU
// and 384 GB. If any of that leaked into the domain the numbers would be
// absurd, so pin the actual bill against the tier table.
func TestFargateNodesNeverPricedAsNodes(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	learn(t, d, cluster(now, 300, 72*time.Hour, cliff()))
	rec := only(t, d.Recommend(now, nil), cliff().ref())
	if rec.CurrentHourlyUSD > 1 {
		t.Fatalf("current cost $%v/h looks like node capacity, not a Fargate tier", rec.CurrentHourlyUSD)
	}
	rates := pricing.DefaultFargateRates()
	if want := rates.Cost(pricing.FargateConfig{MilliCPU: 2000, MemoryMiB: 9216}); rec.CurrentHourlyUSD != want {
		t.Fatalf("current = $%v/h, want the 2vCPU/9GB tier price $%v/h", rec.CurrentHourlyUSD, want)
	}
}

// TestModeGuardrails: kilter.dev/mode decides applicability, and it does so
// visibly — a mode=recommend workload stays in the report with a reason rather
// than vanishing.
func TestModeGuardrails(t *testing.T) {
	off, rec := cliff(), cliff()
	off.name, off.mode = "off-wl", guard.ModeOff
	rec.name, rec.mode = "rec-wl", guard.ModeRecommend
	apply := cliff()
	apply.name, apply.mode = "apply-wl", guard.ModeApply

	d := newDomain(t, noPolicyMoves)
	learn(t, d, cluster(now, 300, 72*time.Hour, off, rec, apply))
	recs := d.Recommend(now, nil)

	none(t, recs, off.ref()) // "Kilter never touches or moves this workload"

	r := only(t, recs, rec.ref())
	if !r.Suppressed || r.SuppressCode != domain.SuppressModeRecommend {
		t.Fatalf("mode=recommend not suppressed: suppressed=%v code=%q", r.Suppressed, r.SuppressCode)
	}
	if r.ClaimableMonthlyUSD() != 0 {
		t.Errorf("a suppressed recommendation claims $%v", r.ClaimableMonthlyUSD())
	}
	a := only(t, recs, apply.ref())
	if a.Suppressed {
		t.Fatalf("mode=apply suppressed: %s", a.Reason)
	}

	// PlanSteps skips the suppressed one.
	steps, err := d.PlanSteps(recs, domain.Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Target.ID != targetID(apply.ref()) {
		t.Fatalf("steps = %+v, want only the mode=apply workload", steps)
	}
}

func TestPlanStepsShape(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	a, b := cliff(), cliff()
	a.name, b.name = "aaa", "bbb"
	learn(t, d, cluster(now, 300, 72*time.Hour, a, b))
	recs := d.Recommend(now, nil)

	steps, err := d.PlanSteps(recs, domain.Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps", len(steps))
	}
	if steps[0].Seq != 1 || steps[1].Seq != 2 {
		t.Errorf("sequence numbers = %d,%d", steps[0].Seq, steps[1].Seq)
	}
	if steps[0].Target.ID >= steps[1].Target.ID {
		t.Errorf("steps not in deterministic target order: %v", steps)
	}
	if steps[0].Key == steps[1].Key || steps[0].Key == "" {
		t.Errorf("step keys are not distinct: %q %q", steps[0].Key, steps[1].Key)
	}
	if !steps[0].From.Equal(recs[0].Current) || !steps[0].To.Equal(recs[0].Proposed) {
		t.Error("step does not record the exact from/to specs needed for revert")
	}
	if !contains(steps[0].Detail, ChangeBoundaryShave) {
		t.Errorf("step detail %q does not name the lever", steps[0].Detail)
	}

	// MaxSteps caps the plan.
	capped, err := d.PlanSteps(recs, domain.Guard{Now: now, MaxSteps: 1})
	if err != nil || len(capped) != 1 {
		t.Fatalf("MaxSteps=1 gave %d steps (%v)", len(capped), err)
	}
	// Guardrails refuse before any step is built.
	if _, err := d.PlanSteps(recs, domain.Guard{Now: now, Freeze: true}); !errors.Is(err, domain.ErrFrozen) {
		t.Errorf("frozen PlanSteps = %v", err)
	}
	if _, err := d.PlanSteps(recs, domain.Guard{Now: now, BreakerOpen: true}); !errors.Is(err, domain.ErrBreakerOpen) {
		t.Errorf("breaker-open PlanSteps = %v", err)
	}
}

// TestOutputIsDeterministicUnderShuffle: nothing on an output path may depend
// on map iteration order or on the order the collector listed pods and samples.
func TestOutputIsDeterministicUnderShuffle(t *testing.T) {
	base := cluster(now, 300, 72*time.Hour,
		cliff(),
		wl{name: "over", replicas: 3, containers: []ctr{
			{name: "app", cpuReq: 2000, memReq: 8 * gib, cpuUse: 150, memUse: 400 * mib},
			{name: "log", cpuReq: 500, memReq: 1 * gib, cpuUse: 20, memUse: 90 * mib},
		}},
		wl{name: "zebra", containers: []ctr{
			{name: "app", cpuReq: 4000, memReq: 20 * gib, cpuUse: 500, memUse: 2 * gib}}},
	)

	rng := rand.New(rand.NewSource(42))
	var want []byte
	for i := 0; i < 50; i++ {
		s := *base
		s.Pods = append([]model.PodSpec(nil), base.Pods...)
		s.Usage = append([]model.Usage(nil), base.Usage...)
		s.Nodes = append([]model.NodeSpec(nil), base.Nodes...)
		s.Workloads = append([]model.WorkloadInfo(nil), base.Workloads...)
		rng.Shuffle(len(s.Pods), func(a, b int) { s.Pods[a], s.Pods[b] = s.Pods[b], s.Pods[a] })
		rng.Shuffle(len(s.Usage), func(a, b int) { s.Usage[a], s.Usage[b] = s.Usage[b], s.Usage[a] })
		rng.Shuffle(len(s.Nodes), func(a, b int) { s.Nodes[a], s.Nodes[b] = s.Nodes[b], s.Nodes[a] })

		d := newDomain(t, noPolicyMoves)
		learn(t, d, &s)
		recs := d.Recommend(now, nil)
		if len(recs) == 0 {
			t.Fatal("fixture produced nothing")
		}
		got, err := json.Marshal(recs)
		if err != nil {
			t.Fatal(err)
		}
		if want == nil {
			want = got
			continue
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("iteration %d: output depends on input order\nwant %s\ngot  %s", i, want, got)
		}
	}
	// Same for the step plan and its fingerprint.
	d := newDomain(t, noPolicyMoves)
	learn(t, d, base)
	steps, err := d.PlanSteps(d.Recommend(now, nil), domain.Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	fp := domain.Fingerprint(steps)
	for i := 0; i < 20; i++ {
		d2 := newDomain(t, noPolicyMoves)
		learn(t, d2, base)
		s2, err := d2.PlanSteps(d2.Recommend(now, nil), domain.Guard{Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if domain.Fingerprint(s2) != fp {
			t.Fatalf("plan fingerprint is not stable: %q vs %q", domain.Fingerprint(s2), fp)
		}
	}
}

func TestCheckpointRoundTripAndDeterminism(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	snap := cluster(now, 300, 72*time.Hour, cliff(),
		wl{name: "other", containers: []ctr{{name: "app", cpuReq: 500, memReq: 2 * gib, cpuUse: 100, memUse: 500 * mib}}})
	learn(t, d, snap)

	b1, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		b2, err := d.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatal("checkpoint is not byte-stable")
		}
	}

	restored := newDomain(t, noPolicyMoves)
	if err := restored.Restore(b1); err != nil {
		t.Fatal(err)
	}
	// Restored learning is not a live view: the domain stays report-only until
	// a collector feeds it again.
	if h := restored.Health(now); !h.ReportOnly || h.Ready {
		t.Fatalf("restored domain claims to be live: %+v", h)
	}
	if got := restored.Recommend(now, nil); got != nil {
		t.Fatalf("restored domain recommended without a snapshot: %d", len(got))
	}
	// Restore-then-learn must be indistinguishable from learn-then-learn: the
	// checkpoint carries the whole learned state, not a summary of it.
	learn(t, d, snap)
	learn(t, restored, snap)
	w, _ := json.Marshal(d.Recommend(now, nil))
	g, _ := json.Marshal(restored.Recommend(now, nil))
	if !bytes.Equal(w, g) {
		t.Fatalf("restore lost state:\nwant %s\ngot  %s", w, g)
	}

	if err := restored.Restore(nil); err != nil {
		t.Errorf("Restore(nil) = %v", err)
	}
	if err := restored.Restore([]byte(`{"version":99}`)); err == nil {
		t.Error("accepted a future checkpoint version")
	}
	if err := restored.Restore([]byte("not json")); err == nil {
		t.Error("accepted malformed checkpoint bytes")
	}
}

// TestRegistryIntegration wires the domain the way cmd/ will and exercises the
// whole seam end to end.
func TestRegistryIntegration(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	r := domain.NewRegistry()
	if err := r.Register(d); err != nil {
		t.Fatal(err)
	}
	snap := cluster(now, 300, 72*time.Hour, cliff())
	if err := r.Learn(&domain.Snapshot{Domain: Kind, Scope: "c1", Timestamp: now, Cluster: snap}); err != nil {
		t.Fatal(err)
	}
	recs := r.Recommend(now, nil)
	if len(recs) != 1 {
		t.Fatalf("registry produced %d recommendations", len(recs))
	}
	if recs[0].Target.Domain != Kind {
		t.Errorf("target domain = %q", recs[0].Target.Domain)
	}
	steps, err := r.PlanSteps(Kind, recs, domain.Guard{Now: now})
	if err != nil || len(steps) != 1 {
		t.Fatalf("PlanSteps = (%v, %v)", steps, err)
	}
	hs := r.Health(now)
	if len(hs) != 1 || hs[0].Kind != Kind || hs[0].Targets != 1 {
		t.Fatalf("health = %+v", hs)
	}
}

// TestLedgerRoutingNeverExceedsGross: with a commitment in play the claim drops
// to the bill delta, and the recommendation says why.
func TestLedgerRoutingNeverExceedsGross(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	learn(t, d, cluster(now, 300, 72*time.Hour, cliff()))

	plain := only(t, d.Recommend(now, nil), cliff().ref())
	if plain.NetSavingsMonthlyUSD != plain.GrossSavingsMonthlyUSD {
		t.Fatalf("no ledger: net $%v != gross $%v", plain.NetSavingsMonthlyUSD, plain.GrossSavingsMonthlyUSD)
	}

	inv := &commit.Inventory{SavingsPlans: []commit.SavingsPlan{
		{ID: "sp-1", Type: commit.SPCompute, CommitmentUSDPerHour: 10},
	}}
	led := domain.NewLedger(inv, commit.Usage{})
	netted := only(t, d.Recommend(now, led), cliff().ref())

	if netted.GrossSavingsMonthlyUSD != plain.GrossSavingsMonthlyUSD {
		t.Errorf("gross moved with the ledger: $%v vs $%v",
			netted.GrossSavingsMonthlyUSD, plain.GrossSavingsMonthlyUSD)
	}
	if netted.NetSavingsMonthlyUSD > netted.GrossSavingsMonthlyUSD {
		t.Fatalf("net $%v exceeds gross $%v", netted.NetSavingsMonthlyUSD, netted.GrossSavingsMonthlyUSD)
	}
	if !netted.Suppressed || netted.SuppressCode != domain.SuppressCommitmentNeutral {
		t.Fatalf("commitment-absorbed saving not suppressed: %+v", netted.SuppressCode)
	}
	if netted.ClaimableMonthlyUSD() != 0 {
		t.Errorf("claims $%v against a stranded commitment", netted.ClaimableMonthlyUSD())
	}
	if !contains(netted.Reason, "commitment stranding") {
		t.Errorf("reason %q does not explain the suppression", netted.Reason)
	}
}

func TestSpecEncodingRoundTrip(t *testing.T) {
	cfg := pricing.FargateConfig{MilliCPU: 1000, MemoryMiB: 8192}
	cs := []ContainerChange{
		{Name: "zeta", Requests: model.Resources{MilliCPU: 200, MemoryBytes: 3 * gib}},
		{Name: "alpha", Requests: model.Resources{MilliCPU: 800, MemoryBytes: 5 * gib}},
	}
	s := buildSpec(cfg, model.Resources{MilliCPU: 1000, MemoryBytes: 8 * gib}, cs,
		ChangeTierMove, string(pricing.SourceQuantized), 2)

	back, err := Containers(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Name != "alpha" || back[1].Name != "zeta" {
		t.Fatalf("containers not sorted by name: %+v", back)
	}
	if back[0].Requests != cs[1].Requests || back[1].Requests != cs[0].Requests {
		t.Fatalf("requests lost in the round trip: %+v", back)
	}
	if s.Attr(AttrTier) != "1vCPU 8GB" || s.Attr(AttrReplicas) != "2" {
		t.Fatalf("attrs = %v", s.Attrs)
	}
	if s.Resources != cfg.Resources() {
		t.Fatalf("spec resources %s are not the billed tier %s", s.Resources, cfg)
	}
	// Malformed breakdowns are errors, never partial patches.
	bad := s.WithAttr("container/app/wattage", "9")
	if _, err := Containers(bad); err == nil {
		t.Error("accepted an unknown container attribute")
	}
	bad = s.WithAttr("container/app/milliCPU", "banana")
	if _, err := Containers(bad); err == nil {
		t.Error("accepted a non-numeric container attribute")
	}
}

func TestConfigDefaultsAreConservative(t *testing.T) {
	garbage := Config{
		MinShaveConfidence: -1, MinShaveWindow: -time.Hour, MinShaveSamples: -5,
		NoiseBandFraction: -1, NoiseSigmas: -1, MinShaveMonthlyUSD: -1,
		StaleAfter: -1, RegressionWindow: -1, QuarantineFor: -1, DefaultMode: "nonsense",
	}
	got := garbage.withDefaults()
	want := DefaultConfig()
	if got.MinShaveConfidence != want.MinShaveConfidence || got.MinShaveWindow != want.MinShaveWindow ||
		got.MinShaveSamples != want.MinShaveSamples || got.NoiseBandFraction != want.NoiseBandFraction ||
		got.NoiseSigmas != want.NoiseSigmas || got.MinShaveMonthlyUSD != want.MinShaveMonthlyUSD ||
		got.StaleAfter != want.StaleAfter || got.DefaultMode != want.DefaultMode {
		t.Fatalf("garbage config did not fall back to defaults:\n%+v", got)
	}
	if !got.Rates.Platform.Valid() {
		t.Error("rates were not defaulted")
	}
	// EKS Fargate has one platform; the domain cannot be pointed at another.
	if got.Rates.Platform != pricing.EKSLinuxX86 {
		t.Errorf("platform = %v", got.Rates.Platform)
	}
}
