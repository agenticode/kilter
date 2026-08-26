package backtest

import (
	"errors"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

func TestRunRejectsGarbageInput(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 3})
	base := func() *Harness { return &Harness{History: tr.Source()} }

	tests := []struct {
		name    string
		harness *Harness
		cluster string
		from    time.Time
		to      time.Time
		horizon time.Duration
	}{
		{"unnamed cluster", base(), "", tr.Start, tr.End, time.Hour},
		{"no history source", &Harness{}, tr.Cluster, tr.Start, tr.End, time.Hour},
		{"zero from", base(), tr.Cluster, time.Time{}, tr.End, time.Hour},
		{"zero to", base(), tr.Cluster, tr.Start, time.Time{}, time.Hour},
		{"inverted window", base(), tr.Cluster, tr.End, tr.Start, time.Hour},
		{"empty window", base(), tr.Cluster, tr.Start, tr.Start, time.Hour},
		{"zero horizon", base(), tr.Cluster, tr.Start, tr.End, 0},
		{"negative horizon", base(), tr.Cluster, tr.Start, tr.End, -time.Hour},
		{"horizon past the window", base(), tr.Cluster, tr.Start, tr.End, 400 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.harness.Run(tc.cluster, tc.from, tc.to, tc.horizon); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestRunRejectsInvalidPolicyAndScoring(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 3})
	tests := []struct {
		name string
		mut  func(*Harness)
	}{
		{"invalid recommender policy", func(h *Harness) {
			h.Rec = recommend.DefaultConfig()
			h.Rec.CPUPercentile = 2
		}},
		{"invalid decision policy", func(h *Harness) {
			h.Decision = decision.DefaultConfig()
			h.Decision.MinClassStability = 5
		}},
		{"invalid decision interval", func(h *Harness) {
			h.Scoring = DefaultConfig()
			h.Scoring.DecisionInterval = -time.Hour
		}},
		{"invalid starvation factor", func(h *Harness) {
			h.Scoring = DefaultConfig()
			h.Scoring.StarvationFactor = -1
		}},
		{"invalid cost model", func(h *Harness) {
			h.Scoring = DefaultConfig()
			h.Scoring.Cost.IncidentUSD = -5
		}},
		{"negative flip window", func(h *Harness) {
			h.Scoring = DefaultConfig()
			h.Scoring.FlipWindow = -time.Hour
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Harness{History: tr.Source()}
			tc.mut(h)
			if _, err := h.Run(tr.Cluster, tr.Start, tr.End, time.Hour); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestRunRejectsDuplicateSnapshotTimestamps(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 3})
	dupes := append([]*model.ClusterSnapshot(nil), tr.Snapshots...)
	dupes = append(dupes, tr.Snapshots[100])
	h := &Harness{History: SliceSource(dupes)}
	_, err := h.Run(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if !errors.Is(err, ErrDuplicateSnapshot) {
		t.Fatalf("got %v, want ErrDuplicateSnapshot", err)
	}
}

// TestTheEngineNeverSeesTheFuture is the leakage test. Two histories that are
// identical up to day 5 and wildly different after it must produce identical
// recommendations at every instant before day 5. If the harness ever fed a
// later snapshot to the recommender before asking, this is what would catch
// it — and every number in the package would otherwise be optimistic.
func TestTheEngineNeverSeesTheFuture(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 7, Workloads: 2})
	spikeFrom := tr.Start.Add(5 * 24 * time.Hour)

	spiked := *tr
	snaps := make([]*model.ClusterSnapshot, 0, len(tr.Snapshots))
	for _, src := range tr.Snapshots {
		cp := *src
		if !cp.Timestamp.Before(spikeFrom) {
			cp.Usage = append([]model.Usage(nil), src.Usage...)
			for i := range cp.Usage {
				cp.Usage[i].MilliCPU *= 10
				cp.Usage[i].MemoryBytes *= 3
			}
		}
		snaps = append(snaps, &cp)
	}
	spiked.Snapshots = snaps

	clean, err := defaultPolicy().harness(tr).records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := defaultPolicy().harness(&spiked).records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean) != len(dirty) {
		t.Fatalf("record counts differ: %d vs %d", len(clean), len(dirty))
	}
	compared := 0
	for i := range clean {
		if !clean[i].At.Before(spikeFrom) {
			continue
		}
		compared++
		if clean[i].Target != dirty[i].Target || clean[i].Applied != dirty[i].Applied {
			t.Fatalf("decision at %s changed because of usage %s later: %s/%v vs %s/%v",
				clean[i].At, spikeFrom.Sub(clean[i].At), clean[i].Target, clean[i].Applied,
				dirty[i].Target, dirty[i].Applied)
		}
	}
	if compared == 0 {
		t.Fatal("no decisions fell before the divergence; the test proves nothing")
	}
	// Sanity: the spike must actually have changed something afterwards,
	// or the two histories were not different enough to test anything.
	changed := false
	for i := range clean {
		if clean[i].At.After(spikeFrom) && clean[i].Target != dirty[i].Target {
			changed = true
		}
	}
	if !changed {
		t.Fatal("the injected spike changed no later decision; the histories are not distinguishable")
	}
}

// TestScoredSetMatchesRecommenderEligibility: the harness must score exactly
// the containers the recommender considers. Scoring a Job would invent
// refusals that the engine never had the chance to make.
func TestScoredSetMatchesRecommenderEligibility(t *testing.T) {
	start := propStart
	const cluster = "eligibility"
	sized := model.Resources{MilliCPU: 1000, MemoryBytes: 1 << 30}
	mk := func(kind model.WorkloadKind, name, phase string) model.PodSpec {
		ref := model.WorkloadRef{Kind: kind, Namespace: "default", Name: name}
		return model.PodSpec{
			UID: name, Name: name + "-0", Namespace: "default", Workload: ref,
			NodeName: "node-00", Phase: phase,
			Containers: []model.ContainerSpec{{Name: "app", Requests: sized, Limits: sized}},
		}
	}
	pods := []model.PodSpec{
		mk(model.KindDeployment, "web", "Running"),
		mk(model.KindStatefulSet, "db", "Running"),
		mk(model.KindJob, "import", "Running"),
		mk(model.KindCronJob, "nightly", "Running"),
		mk(model.KindBarePod, "debug", "Running"),
		mk(model.KindDeployment, "starting", "Pending"),
	}
	nodes := []model.NodeSpec{{
		Name: "node-00", Ready: true, InstanceType: "m5.2xlarge", Provider: "aws",
		Capacity:    model.Resources{MilliCPU: 8000, MemoryBytes: 32 << 30},
		Allocatable: model.Resources{MilliCPU: 7800, MemoryBytes: 30 << 30},
	}}

	var snaps []*model.ClusterSnapshot
	for i := 0; i < 48*12; i++ { // two days at five minutes
		ts := start.Add(time.Duration(i) * 5 * time.Minute)
		snap := &model.ClusterSnapshot{ClusterID: cluster, Timestamp: ts, Nodes: nodes, Pods: pods}
		for _, p := range pods {
			snap.Workloads = append(snap.Workloads, model.WorkloadInfo{Ref: p.Workload, Replicas: 1, Ready: 1})
			snap.Usage = append(snap.Usage, model.Usage{
				Key:    model.ContainerKey{Workload: p.Workload, Container: "app"},
				PodUID: p.UID, Timestamp: ts, MilliCPU: 100, MemoryBytes: 256 << 20,
				WindowSeconds: 300,
			})
		}
		snaps = append(snaps, snap)
	}

	h := &Harness{History: SliceSource(snaps)}
	recs, err := h.records(cluster, start, start.Add(48*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Key.Workload.Name] = true
	}
	for _, want := range []string{"web", "db"} {
		if !seen[want] {
			t.Errorf("workload %q was never scored", want)
		}
	}
	for _, unwanted := range []string{"import", "nightly", "debug", "starting"} {
		if seen[unwanted] {
			t.Errorf("workload %q was scored, but the recommender never sizes it", unwanted)
		}
	}
}

func TestDecisionInstantsSnapForwardAndRequireAFullHorizon(t *testing.T) {
	start := propStart
	at := func(h int) *model.ClusterSnapshot {
		return &model.ClusterSnapshot{ClusterID: "c", Timestamp: start.Add(time.Duration(h) * time.Hour)}
	}
	// Snapshots at 0h, 5h, 9h, 20h against a 24h window with a 6h horizon
	// and a 6h grid: grid points 0,6,12,18 fit; 24 does not.
	snaps := []*model.ClusterSnapshot{at(0), at(5), at(9), at(20)}
	got, skipped := decisionInstants(snaps, start, start.Add(24*time.Hour), 6*time.Hour, 6*time.Hour)

	// 0h→snapshot 0; 6h→snapshot at 9h; 12h→snapshot at 20h, whose window
	// would end at 26h and so is dropped; 18h→also the 20h snapshot, dropped
	// for the same reason; 24h→no horizon left.
	want := []int{0, 2}
	if len(got) != len(want) {
		t.Fatalf("instants = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("instants = %v, want %v", got, want)
		}
	}
	if skipped.NoHorizon != 3 {
		t.Fatalf("NoHorizon = %d, want 3", skipped.NoHorizon)
	}
}

func TestDecisionInstantsCountMissingSnapshots(t *testing.T) {
	start := propStart
	// A single snapshot at the very beginning: later grid points have
	// nothing at or after them.
	snaps := []*model.ClusterSnapshot{{ClusterID: "c", Timestamp: start}}
	got, skipped := decisionInstants(snaps, start, start.Add(72*time.Hour), 12*time.Hour, 24*time.Hour)
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("instants = %v, want [0]", got)
	}
	if skipped.NoSnapshot != 2 {
		t.Fatalf("NoSnapshot = %d, want 2 (the 24h and 48h grid points)", skipped.NoSnapshot)
	}
}

// TestScoringWindowExcludesTheDecisionInstant: the sample at t is what the
// engine just learned from, so counting it as "the future" would let the
// harness grade the engine on its own input.
func TestScoringWindowExcludesTheDecisionInstant(t *testing.T) {
	key := model.ContainerKey{Workload: model.WorkloadRef{Kind: model.KindDeployment,
		Namespace: "default", Name: "web"}, Container: "app"}
	f := &futureIndex{byKey: map[model.ContainerKey][]usagePoint{key: {
		{At: propStart, MilliCPU: 1},
		{At: propStart.Add(time.Minute), MilliCPU: 2},
		{At: propStart.Add(2 * time.Minute), MilliCPU: 3},
		{At: propStart.Add(3 * time.Minute), MilliCPU: 4},
	}}}
	got := f.window(key, propStart, propStart.Add(2*time.Minute))
	if len(got) != 2 || got[0].MilliCPU != 2 || got[1].MilliCPU != 3 {
		t.Fatalf("window (t, t+2m] = %+v, want the samples at +1m and +2m", got)
	}
	if n := len(f.window(key, propStart.Add(9*time.Minute), propStart.Add(20*time.Minute))); n != 0 {
		t.Fatalf("a window past the history returned %d points", n)
	}
	missing := model.ContainerKey{Container: "nope"}
	if n := len(f.window(missing, propStart, propStart.Add(time.Hour))); n != 0 {
		t.Fatalf("an unknown container returned %d points", n)
	}
}

func TestZeroHarnessRunsTheShippedDefaults(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 3})
	sc := mustRun(t, &Harness{History: tr.Source()}, tr)
	want := PolicyHash(recommend.DefaultConfig(), plan.DefaultConfig(), decision.DefaultConfig())
	if sc.Policy != want {
		t.Fatalf("policy hash = %q, want the shipped defaults %q", sc.Policy, want)
	}
	if sc.Cost != DefaultCostModel() {
		t.Fatalf("cost model = %+v, want the default %+v", sc.Cost, DefaultCostModel())
	}
	if sc.StarvationFactor != 1 || sc.DecisionIntervalHours != 24 {
		t.Fatalf("scoring defaults did not apply: %+v", sc)
	}
}

func TestPolicyHashSeparatesPolicies(t *testing.T) {
	base := PolicyHash(recommend.DefaultConfig(), plan.DefaultConfig(), decision.DefaultConfig())
	if len(base) != 16 {
		t.Fatalf("policy hash %q is %d chars, want 16", base, len(base))
	}
	for _, p := range namedPolicies()[1:] {
		if got := PolicyHash(p.rec, p.pl, p.dec); got == base {
			t.Fatalf("policy %q hashes the same as the default (%s)", p.name, got)
		}
	}
	// And it is stable: the same configs always hash the same.
	if again := PolicyHash(recommend.DefaultConfig(), plan.DefaultConfig(), decision.DefaultConfig()); again != base {
		t.Fatalf("policy hash is not stable: %q then %q", base, again)
	}
}

func TestEmptyHistoryScoresNothing(t *testing.T) {
	h := &Harness{History: SliceSource(nil)}
	sc, err := h.Run("nowhere", propStart, propStart.Add(48*time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Snapshots != 0 || sc.Scored != 0 || sc.Decisions != 0 {
		t.Fatalf("an empty history produced %+v", sc)
	}
	if sc.Refusals == nil {
		t.Fatal("Refusals must be an empty map, not nil, so JSON output is stable")
	}
	if _, err := sc.Encode(); err != nil {
		t.Fatalf("encoding an empty scorecard: %v", err)
	}
}

func TestEvidenceStoreIsOptional(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 5, Workloads: 2,
		OOMAt: []time.Duration{30 * time.Hour}})
	withStore := defaultPolicy().harness(tr)
	withStore.Evidence = mustStore(t, tr)
	withCard := mustRun(t, withStore, tr)
	withoutCard := mustRun(t, defaultPolicy().harness(tr), tr)

	if withoutCard.MemOOMKills != 0 {
		t.Fatalf("with no substrate there is no OOM ground truth, got %d", withoutCard.MemOOMKills)
	}
	if withCard.MemOOMKills != 2 {
		t.Fatalf("with the substrate, want the 2 seeded OOMKills, got %d", withCard.MemOOMKills)
	}
	// Usage-derived numbers come from the snapshots either way, so they must
	// be identical: the substrate adds facts, it does not change the replay.
	if withCard.PolicyCostUSD != withoutCard.PolicyCostUSD || withCard.OracleCostUSD != withoutCard.OracleCostUSD {
		t.Fatalf("the substrate changed the replay: %v/%v vs %v/%v",
			withCard.PolicyCostUSD, withCard.OracleCostUSD,
			withoutCard.PolicyCostUSD, withoutCard.OracleCostUSD)
	}
}

func TestStarvationFactorMovesTheOracleAndThePredicate(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceDiurnal, Start: propStart, Days: 5, Workloads: 1})
	strict := defaultPolicy().harness(tr)
	strict.Scoring = DefaultConfig()
	strict.Scoring.StarvationFactor = 0.5 // demand 2× the p95 in the request

	lenient := defaultPolicy().harness(tr)
	lenient.Scoring = DefaultConfig()
	lenient.Scoring.StarvationFactor = 2 // tolerate bursting to 2× the request

	strictRecs, err := strict.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	lenientRecs, err := lenient.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := range strictRecs {
		if strictRecs[i].Oracle.MilliCPU <= lenientRecs[i].Oracle.MilliCPU {
			t.Fatalf("decision %d: a stricter starvation factor must demand more CPU (%d vs %d)",
				i, strictRecs[i].Oracle.MilliCPU, lenientRecs[i].Oracle.MilliCPU)
		}
		if strictRecs[i].Oracle.MemoryBytes != lenientRecs[i].Oracle.MemoryBytes {
			t.Fatalf("decision %d: the starvation factor must not touch memory", i)
		}
	}
	if mustRun(t, strict, tr).CPUStarvation <= mustRun(t, lenient, tr).CPUStarvation {
		t.Fatal("a stricter starvation factor must find at least as many starvation events")
	}
}

func TestSliceSourceFiltersByClusterAndWindow(t *testing.T) {
	a := &model.ClusterSnapshot{ClusterID: "a", Timestamp: propStart}
	b := &model.ClusterSnapshot{ClusterID: "b", Timestamp: propStart.Add(time.Hour)}
	late := &model.ClusterSnapshot{ClusterID: "a", Timestamp: propStart.Add(10 * time.Hour)}
	src := SliceSource{a, nil, b, late}

	got, err := src.Snapshots("a", propStart, propStart.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != a {
		t.Fatalf("got %d snapshots, want only the in-window one for cluster a", len(got))
	}
	// The window is half-open, so a snapshot exactly on `to` is excluded.
	if got, _ := src.Snapshots("a", propStart, propStart); len(got) != 0 {
		t.Fatalf("an empty window returned %d snapshots", len(got))
	}
}

func TestRunAndRecordsAgree(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceBursty, Start: propStart, Days: 7, Workloads: 3})
	h := defaultPolicy().harness(tr)
	h.Evidence = mustStore(t, tr)
	sc := mustRun(t, h, tr)
	recs, err := h.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != sc.Scored {
		t.Fatalf("records() produced %d rows, the scorecard counted %d", len(recs), sc.Scored)
	}
	applied := 0
	for _, r := range recs {
		if r.Applied {
			applied++
		}
	}
	if applied != sc.Decisions {
		t.Fatalf("records() has %d applied rows, the scorecard counted %d decisions", applied, sc.Decisions)
	}
}

// TestRefusalCodeTaxonomy walks every place the production path can decline
// to act and asserts the scorecard names it. A refusal without a reason is
// the silence §4.2 exists to abolish, so each of these must be reachable and
// distinct.
func TestRefusalCodeTaxonomy(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 5, Workloads: 2})
	store := mustStore(t, tr)

	tests := []struct {
		name string
		mut  func(*Harness)
		want string
	}{
		{"churn suppression", func(h *Harness) {
			h.Rec = recommend.DefaultConfig()
			h.Rec.MinChangeRatio = 0.99
		}, CodeBelowChangeThreshold},
		{"planner confidence bar", func(h *Harness) {
			h.Plan = plan.DefaultConfig()
			h.Plan.MinConfidence = 1.01
		}, CodeBelowConfidence},
		{"advisory mode", func(h *Harness) {
			h.Plan = plan.DefaultConfig()
			h.Plan.DefaultMode = "recommend"
		}, CodeModeGuarded},
		{"rightsizing disabled in the planner", func(h *Harness) {
			h.Plan = plan.DefaultConfig()
			h.Plan.ApplyRecommendations = false
		}, CodePlanDropped},
		{"history too short", func(h *Harness) {
			h.Rec = recommend.DefaultConfig()
			h.Rec.MinSamples = 1 << 30
			h.Decision = decision.DefaultConfig()
			h.Decision.MinSamples = 1 << 30
		}, string(decision.CodeInsufficientHistory)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Harness{History: tr.Source(), Evidence: store}
			tc.mut(h)
			sc := mustRun(t, h, tr)
			if sc.Refusals[tc.want] == 0 {
				t.Fatalf("no %q refusal; got %v (decisions=%d)", tc.want, sc.Refusals, sc.Decisions)
			}
		})
	}
}

// TestEnforcedRefusalsSurfaceDecisionCodes: with the decision layer wired in,
// a container inside its post-change soak must be refused by name, and the
// current sizing must stay in force.
func TestEnforcedRefusalsSurfaceDecisionCodes(t *testing.T) {
	// A deploy 90 minutes before the day-2 instant sits well inside the 6h
	// default soak; a deploy at the start of the trace does not.
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 5, Workloads: 1,
		DeployAt: []time.Duration{46*time.Hour + 30*time.Minute}})
	h := defaultPolicy().harness(tr)
	h.Evidence = mustStore(t, tr)
	h.EnforceDecisionRefusals = true

	recs, err := h.records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	soaked := 0
	for _, r := range recs {
		if r.Code != string(decision.CodePostChangeSoak) {
			continue
		}
		soaked++
		if r.Applied {
			t.Fatal("a refused decision was also applied")
		}
		if r.Chosen != r.Current {
			t.Fatalf("a refusal must leave the current sizing in force: chosen %s, current %s",
				r.Chosen, r.Current)
		}
	}
	if soaked != 1 {
		t.Fatalf("got %d post-change-soak refusals, want exactly the day-2 instant", soaked)
	}
}
