package recommend

import (
	"encoding/json"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/model"
)

// verdictScenario builds a pseudo-random cluster that reaches every
// disposition: containers with enough history and an oversized request
// (recommended), containers sized right at their usage (churn-suppressed),
// containers with too few samples or too short a window, ineligible pods
// (Job/CronJob/bare/Pending), HPA-on-CPU workloads, OOM restarts, and
// sometimes a container the recommender has never been shown.
func verdictScenario(t *testing.T, seed int64) (*Recommender, *model.ClusterSnapshot) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	r := newRec(t)

	snap := &model.ClusterSnapshot{ClusterID: "test", Timestamp: t0.Add(72 * time.Hour)}
	n := 4 + rng.Intn(6)
	for i := 0; i < n; i++ {
		kind, phase := model.KindDeployment, "Running"
		switch rng.Intn(10) {
		case 0:
			kind = model.KindJob
		case 1:
			kind = model.KindCronJob
		case 2:
			kind = model.KindBarePod
		case 3:
			phase = "Pending"
		}
		ref := model.WorkloadRef{Kind: kind, Namespace: "ns" + itoa(rng.Intn(2)), Name: "wl" + itoa(i)}
		key := model.ContainerKey{Workload: ref, Container: "app"}

		baseCPU := int64(50 + rng.Intn(900))
		baseMem := int64(64<<20) * int64(1+rng.Intn(16))

		var req model.Resources
		switch rng.Intn(3) {
		case 0: // oversized: a shrink is indicated
			req = model.Resources{
				MilliCPU:    baseCPU * int64(4+rng.Intn(8)),
				MemoryBytes: baseMem * int64(4+rng.Intn(8)),
			}
		case 1: // already sized at usage+headroom: churn suppression territory
			req = model.Resources{
				MilliCPU:    ceilInt64(float64(baseCPU) * 1.15),
				MemoryBytes: ceilInt64(float64(baseMem) * 1.20),
			}
		default: // undersized: a growth is indicated
			req = model.Resources{MilliCPU: baseCPU / 2, MemoryBytes: baseMem / 2}
		}
		var lim model.Resources
		if rng.Intn(2) == 0 {
			lim = model.Resources{MilliCPU: req.MilliCPU * 2, MemoryBytes: req.MemoryBytes * 2}
		}

		uid := "pod-" + itoa(i)
		snap.Pods = append(snap.Pods, model.PodSpec{
			UID: uid, Name: ref.Name + "-a", Namespace: ref.Namespace,
			Workload: ref, Phase: phase,
			Containers: []model.ContainerSpec{{Name: "app", Requests: req, Limits: lim}},
		})
		if rng.Intn(6) == 0 {
			snap.Workloads = append(snap.Workloads, model.WorkloadInfo{
				Ref: ref, HasHPA: true, HPATargetsCPU: true,
			})
		}

		hours := 0
		switch rng.Intn(4) {
		case 0: // no usage at all: known container, nothing learned
		case 1:
			hours = 1 + rng.Intn(5) // under the 6h MinWindow
		default:
			hours = 8 + rng.Intn(60)
		}
		for h := 0; h < hours*12; h++ {
			snap.Usage = append(snap.Usage, model.Usage{
				Key: key, PodUID: uid,
				Timestamp:   t0.Add(time.Duration(h*5) * time.Minute),
				MilliCPU:    baseCPU + int64(rng.Intn(40)),
				MemoryBytes: baseMem + int64(rng.Intn(8<<20)),
			})
		}
	}
	r.ObserveSnapshot(snap)

	if rng.Intn(3) == 0 && len(snap.Pods) > 0 {
		i := rng.Intn(len(snap.Pods))
		snap.Pods[i].Containers[0].RestartCount = 1
		snap.Pods[i].Containers[0].LastOOMKilled = true
		r.ObserveSnapshot(snap)
	}

	query := snap
	if rng.Intn(3) == 0 {
		q := *snap
		q.Pods = append(append([]model.PodSpec{}, snap.Pods...), model.PodSpec{
			UID: "pod-fresh", Name: "fresh-a", Namespace: "ns0", Phase: "Running",
			Workload:   model.WorkloadRef{Kind: model.KindDeployment, Namespace: "ns0", Name: "fresh"},
			Containers: []model.ContainerSpec{{Name: "app", Requests: model.Resources{MilliCPU: 100, MemoryBytes: 128 << 20}}},
		})
		query = &q
	}
	return r, query
}

// checkNoDisagreement is the whole unit: Recommendations and Verdicts run
// over the same snapshot and the same locked state, and there is no input on
// which they can report different things — not about what was recommended,
// and not about what was refused.
func checkNoDisagreement(t *testing.T, r *Recommender, snap *model.ClusterSnapshot, label string) map[Disposition]int {
	t.Helper()

	recs := r.Recommendations(snap)
	vs := r.Verdicts(snap)
	// Reverse the call order too: neither may leave state behind that
	// changes the other's answer.
	vsRev := r.Verdicts(snap)
	recsRev := r.Recommendations(snap)

	if len(recs) != len(recsRev) || len(vs) != len(vsRev) {
		t.Fatalf("%s: call order changed the answer: recs %d→%d, verdicts %d→%d",
			label, len(recs), len(recsRev), len(vs), len(vsRev))
	}

	byKey := make(map[model.ContainerKey]Recommendation, len(recs))
	for _, rec := range recs {
		if _, dup := byKey[rec.Key]; dup {
			t.Fatalf("%s: Recommendations returned %s twice", label, rec.Key)
		}
		byKey[rec.Key] = rec
	}

	seen := map[model.ContainerKey]bool{}
	counts := map[Disposition]int{}
	recommended := 0
	for _, v := range vs {
		if seen[v.Key] {
			t.Fatalf("%s: Verdicts returned %s twice", label, v.Key)
		}
		seen[v.Key] = true
		counts[v.Disposition]++

		rec, wasRecommended := byKey[v.Key]
		switch v.Disposition {
		case DispositionRecommended:
			recommended++
			if !wasRecommended {
				t.Fatalf("%s: %s: Verdicts says recommended, Recommendations never reported it",
					label, v.Key)
			}
			if v.Rec == nil {
				t.Fatalf("%s: %s: recommended verdict carries no Recommendation", label, v.Key)
			}
			if *v.Rec != rec {
				t.Fatalf("%s: %s: verdict recommendation diverges from the served one:\n verdict %+v\n served  %+v",
					label, v.Key, *v.Rec, rec)
			}
		case DispositionNeverObserved, DispositionInsufficientHistory, DispositionNoSignificantChange:
			// The cases that matter: a container the engine stayed silent
			// about must not also appear in what it served.
			if wasRecommended {
				t.Fatalf("%s: %s: Verdicts says %q, Recommendations served %+v",
					label, v.Key, v.Disposition, rec)
			}
			if v.Rec != nil {
				t.Fatalf("%s: %s: disposition %q carries a Recommendation", label, v.Key, v.Disposition)
			}
		default:
			t.Fatalf("%s: %s: unknown disposition %q", label, v.Key, v.Disposition)
		}

		// No verdict may ever claim a decision production did not compute.
		if got := v.State(); got != VerdictNotComputed {
			t.Fatalf("%s: %s: state %q — pkg/recommend evaluates no refusal predicate, so no verdict exists",
				label, v.Key, got)
		}
		if dec, ok := v.Decision(); ok {
			t.Fatalf("%s: %s: Decision() claims a verdict %+v that the production path never reached",
				label, v.Key, dec)
		}
	}

	if recommended != len(recs) {
		t.Fatalf("%s: %d recommended verdicts for %d recommendations", label, recommended, len(recs))
	}
	for key := range byKey {
		if !seen[key] {
			t.Fatalf("%s: %s was recommended but Verdicts never considered it", label, key)
		}
	}
	return counts
}

// TestVerdictsAndRecommendationsCannotDisagree is the divergence proof. It
// is the reason this seam reads the production path instead of re-evaluating
// it: every container, every disposition, every seed.
func TestVerdictsAndRecommendationsCannotDisagree(t *testing.T) {
	total := map[Disposition]int{}
	for seed := int64(1); seed <= 200; seed++ {
		r, snap := verdictScenario(t, seed)
		for d, n := range checkNoDisagreement(t, r, snap, "seed "+itoa(int(seed))) {
			total[d] += n
		}
	}
	// A property test that never reached a silent disposition would prove
	// only that the happy path agrees. Require every branch.
	for _, d := range []Disposition{
		DispositionRecommended,
		DispositionNeverObserved,
		DispositionInsufficientHistory,
		DispositionNoSignificantChange,
	} {
		if total[d] == 0 {
			t.Fatalf("corpus never reached disposition %q; the agreement proof does not cover it", d)
		}
	}
	t.Logf("dispositions exercised: %v", total)
}

// TestVerdictDispositions pins each branch to a hand-built cause, so a
// mislabelled disposition fails here with a name rather than as a count.
func TestVerdictDispositions(t *testing.T) {
	find := func(t *testing.T, vs []Verdict, key model.ContainerKey) Verdict {
		t.Helper()
		for _, v := range vs {
			if v.Key == key {
				return v
			}
		}
		t.Fatalf("no verdict for %s (have %d)", key, len(vs))
		return Verdict{}
	}

	t.Run("recommended", func(t *testing.T) {
		r := newRec(t)
		ref := deployRef("web")
		snap := mkSnap(ref, model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}, model.Resources{}, 24,
			func(i int) int64 { return 150 }, func(i int) int64 { return 300 << 20 })
		r.ObserveSnapshot(snap)
		v := find(t, r.Verdicts(snap), model.ContainerKey{Workload: ref, Container: "app"})
		if v.Disposition != DispositionRecommended || v.Rec == nil {
			t.Fatalf("got %q rec=%v, want recommended with a recommendation", v.Disposition, v.Rec)
		}
		if v.Samples != v.Rec.Samples || v.Window.Hours() != v.Rec.WindowHours {
			t.Fatalf("verdict history %d/%v disagrees with the recommendation's %d/%vh",
				v.Samples, v.Window, v.Rec.Samples, v.Rec.WindowHours)
		}
	})

	t.Run("no-significant-change", func(t *testing.T) {
		r := newRec(t)
		ref := deployRef("tight")
		// Usage flat at 100m/200Mi; request already at p95×headroom.
		req := model.Resources{MilliCPU: 115, MemoryBytes: ceilInt64(float64(200<<20) * 1.20)}
		snap := mkSnap(ref, req, model.Resources{}, 24,
			func(i int) int64 { return 100 }, func(i int) int64 { return 200 << 20 })
		r.ObserveSnapshot(snap)
		if got := len(r.Recommendations(snap)); got != 0 {
			t.Fatalf("fixture is wrong: want a suppressed container, got %d recommendations", got)
		}
		v := find(t, r.Verdicts(snap), model.ContainerKey{Workload: ref, Container: "app"})
		if v.Disposition != DispositionNoSignificantChange {
			t.Fatalf("got %q, want no-significant-change", v.Disposition)
		}
		if v.Samples < DefaultConfig().MinSamples {
			t.Fatalf("suppressed container reported %d samples; it had enough history", v.Samples)
		}
	})

	t.Run("insufficient-history-samples", func(t *testing.T) {
		r := newRec(t)
		ref := deployRef("young")
		// 8h span (over MinWindow) but only 8 samples (under MinSamples).
		key := model.ContainerKey{Workload: ref, Container: "app"}
		snap := &model.ClusterSnapshot{
			ClusterID: "test", Timestamp: t0.Add(8 * time.Hour),
			Pods: []model.PodSpec{{
				UID: "pod-1", Name: "young-a", Namespace: ref.Namespace, Workload: ref, Phase: "Running",
				Containers: []model.ContainerSpec{{Name: "app",
					Requests: model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}}},
			}},
		}
		for i := 0; i < 8; i++ {
			snap.Usage = append(snap.Usage, model.Usage{
				Key: key, PodUID: "pod-1", Timestamp: t0.Add(time.Duration(i) * time.Hour),
				MilliCPU: 100, MemoryBytes: 200 << 20,
			})
		}
		r.ObserveSnapshot(snap)
		v := find(t, r.Verdicts(snap), key)
		if v.Disposition != DispositionInsufficientHistory {
			t.Fatalf("got %q, want insufficient-history", v.Disposition)
		}
		if v.Samples != 8 {
			t.Fatalf("samples %d, want 8", v.Samples)
		}
	})

	t.Run("insufficient-history-window", func(t *testing.T) {
		r := newRec(t)
		ref := deployRef("narrow")
		// 4h of dense sampling: plenty of samples, window under MinWindow.
		snap := mkSnap(ref, model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}, model.Resources{}, 4,
			func(i int) int64 { return 100 }, func(i int) int64 { return 200 << 20 })
		r.ObserveSnapshot(snap)
		v := find(t, r.Verdicts(snap), model.ContainerKey{Workload: ref, Container: "app"})
		if v.Disposition != DispositionInsufficientHistory {
			t.Fatalf("got %q, want insufficient-history", v.Disposition)
		}
		if v.Samples < DefaultConfig().MinSamples {
			t.Fatalf("fixture is wrong: %d samples, wanted the window to be the binding gate", v.Samples)
		}
		if v.Window >= DefaultConfig().MinWindow {
			t.Fatalf("window %v is not under MinWindow", v.Window)
		}
	})

	t.Run("insufficient-history-no-usage", func(t *testing.T) {
		r := newRec(t)
		ref := deployRef("silent")
		snap := mkSnap(ref, model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30}, model.Resources{}, 0,
			func(i int) int64 { return 0 }, func(i int) int64 { return 0 })
		r.ObserveSnapshot(snap)
		v := find(t, r.Verdicts(snap), model.ContainerKey{Workload: ref, Container: "app"})
		// Observed, so it is known; nothing learned, so the gate is history.
		if v.Disposition != DispositionInsufficientHistory || v.Samples != 0 {
			t.Fatalf("got %q with %d samples, want insufficient-history with 0", v.Disposition, v.Samples)
		}
	})

	t.Run("never-observed", func(t *testing.T) {
		r := newRec(t)
		ref := deployRef("fresh")
		snap := mkSnap(ref, model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30}, model.Resources{}, 24,
			func(i int) int64 { return 100 }, func(i int) int64 { return 200 << 20 })
		// No ObserveSnapshot at all.
		v := find(t, r.Verdicts(snap), model.ContainerKey{Workload: ref, Container: "app"})
		if v.Disposition != DispositionNeverObserved {
			t.Fatalf("got %q, want never-observed", v.Disposition)
		}
		if v.Samples != 0 || !v.FirstSample.IsZero() || !v.LastSample.IsZero() {
			t.Fatalf("never-observed carried history: %+v", v)
		}
	})
}

// TestVerdictsCoverExactlyTheEligibleSet pins the filter pkg/backtest
// reimplements in eligibleContainers: Running pods only, no bare pods, no
// Job/CronJob, deduplicated by container key.
func TestVerdictsCoverExactlyTheEligibleSet(t *testing.T) {
	r := newRec(t)
	mk := func(kind model.WorkloadKind, name, phase, uid string) model.PodSpec {
		ref := model.WorkloadRef{Kind: kind, Namespace: "default", Name: name}
		return model.PodSpec{
			UID: uid, Name: name + "-" + uid, Namespace: "default", Workload: ref, Phase: phase,
			Containers: []model.ContainerSpec{{Name: "app",
				Requests: model.Resources{MilliCPU: 100, MemoryBytes: 128 << 20}}},
		}
	}
	snap := &model.ClusterSnapshot{ClusterID: "test", Timestamp: t0, Pods: []model.PodSpec{
		mk(model.KindDeployment, "web", "Running", "a"),
		mk(model.KindDeployment, "web", "Running", "b"), // same key: deduplicated
		mk(model.KindStatefulSet, "db", "", "c"),        // empty phase counts as running
		mk(model.KindDeployment, "pending", "Pending", "d"),
		mk(model.KindJob, "job", "Running", "e"),
		mk(model.KindCronJob, "cron", "Running", "f"),
		mk(model.KindBarePod, "bare", "Running", "g"),
	}}
	r.ObserveSnapshot(snap)

	var got []string
	for _, v := range r.Verdicts(snap) {
		got = append(got, v.Key.String())
	}
	// Verdicts sorts by Key.String(); "Deployment/..." < "StatefulSet/...".
	want := []string{"Deployment/default/web/app", "StatefulSet/default/db/app"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("considered %v, want %v", got, want)
	}
}

// TestVerdictNotComputedIsNotARefusal is the anti-collapse test: the two
// facts "no verdict was computed" and "a verdict was computed and it is a
// refusal" must stay distinguishable in Go and over the wire.
func TestVerdictNotComputedIsNotARefusal(t *testing.T) {
	r := newRec(t)
	ref := deployRef("web")
	snap := mkSnap(ref, model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}, model.Resources{}, 24,
		func(i int) int64 { return 150 }, func(i int) int64 { return 300 << 20 })
	r.ObserveSnapshot(snap)
	vs := r.Verdicts(snap)
	if len(vs) != 1 {
		t.Fatalf("want 1 verdict, got %d", len(vs))
	}
	v := vs[0]

	if v.State() != VerdictNotComputed {
		t.Fatalf("state %q, want %q", v.State(), VerdictNotComputed)
	}
	dec, ok := v.Decision()
	if ok {
		t.Fatalf("Decision() reported a verdict production never computed: %+v", dec)
	}
	// A caller that drops the ok still cannot land on a disposition: the
	// zero Action matches none of the three real ones.
	for _, a := range []decision.Action{decision.ActionAct, decision.ActionRecommendOnly, decision.ActionRefuse} {
		if dec.Action == a {
			t.Fatalf("the absent verdict's Action equals %q — absence collapsed into a disposition", a)
		}
	}
	if dec.Refusal != nil {
		t.Fatalf("the absent verdict carries a refusal: %+v", dec.Refusal)
	}

	// The wire form: verdictState says so, and there is no verdict object
	// to misread. A JSON consumer reading .verdict.action finds nothing.
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["verdictState"]) != `"not-computed"` {
		t.Fatalf("verdictState = %s, want \"not-computed\"", raw["verdictState"])
	}
	if _, present := raw["verdict"]; present {
		t.Fatalf("a not-computed verdict serialized a verdict object: %s", b)
	}
	for _, forbidden := range []string{"action", "refusal", "confidence"} {
		if _, present := raw[forbidden]; present {
			t.Fatalf("not-computed verdict exposes %q at the top level: %s", forbidden, b)
		}
	}
}

// TestVerdictJSONRoundTripKeepsAbsenceAbsent: a document with no verdict
// object decodes to not-computed even if its verdictState field says
// otherwise, so a truncated or hand-edited payload cannot manufacture one.
func TestVerdictJSONRoundTripKeepsAbsenceAbsent(t *testing.T) {
	r := newRec(t)
	ref := deployRef("web")
	snap := mkSnap(ref, model.Resources{MilliCPU: 2000, MemoryBytes: 4 << 30}, model.Resources{}, 24,
		func(i int) int64 { return 150 }, func(i int) int64 { return 300 << 20 })
	r.ObserveSnapshot(snap)
	v := r.Verdicts(snap)[0]

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var back Verdict
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Key != v.Key || back.Disposition != v.Disposition || back.Samples != v.Samples {
		t.Fatalf("round trip lost data: %+v vs %+v", back, v)
	}
	if back.Rec == nil || *back.Rec != *v.Rec {
		t.Fatalf("round trip lost the recommendation")
	}
	if _, ok := back.Decision(); ok || back.State() != VerdictNotComputed {
		t.Fatalf("round trip invented a verdict: state %q", back.State())
	}

	// A document that claims "computed" without carrying one stays absent.
	lying := strings.Replace(string(b), `"verdictState":"not-computed"`, `"verdictState":"computed"`, 1)
	if lying == string(b) {
		t.Fatalf("fixture did not contain the verdictState field: %s", b)
	}
	var forged Verdict
	if err := json.Unmarshal([]byte(lying), &forged); err != nil {
		t.Fatal(err)
	}
	if _, ok := forged.Decision(); ok || forged.State() != VerdictNotComputed {
		t.Fatalf("a document claiming \"computed\" with no verdict decoded to %q", forged.State())
	}

	// And the zero Verdict — the value a caller gets from a lookup miss —
	// must read as not-computed, not as an empty disposition.
	var zero Verdict
	if _, ok := zero.Decision(); ok || zero.State() != VerdictNotComputed {
		t.Fatalf("the zero Verdict reports state %q", zero.State())
	}
}

// TestVerdictsAreSortedAndRepeatable: Go randomizes map iteration on every
// range, so repeating in one process is the real determinism test.
func TestVerdictsAreSortedAndRepeatable(t *testing.T) {
	r, snap := verdictScenario(t, 7)
	first, err := json.Marshal(r.Verdicts(snap))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		vs := r.Verdicts(snap)
		for j := 1; j < len(vs); j++ {
			if !(vs[j-1].Key.String() < vs[j].Key.String()) {
				t.Fatalf("run %d: unsorted at %d: %s then %s", i, j, vs[j-1].Key, vs[j].Key)
			}
		}
		again, err := json.Marshal(vs)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d differs from run 0:\n%s\n%s", i, first, again)
		}
	}
}

func TestVerdictsNilSnapshot(t *testing.T) {
	r := newRec(t)
	if vs := r.Verdicts(nil); vs != nil {
		t.Fatalf("nil snapshot returned %v", vs)
	}
}

// TestVerdictsConcurrentWithObserve runs under -race: Verdicts takes the
// same lock ObserveSnapshot and Recommendations do.
func TestVerdictsConcurrentWithObserve(t *testing.T) {
	r, snap := verdictScenario(t, 11)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch i % 4 {
				case 0:
					r.ObserveSnapshot(snap)
				case 1:
					_ = r.Recommendations(snap)
				case 2:
					_ = r.Verdicts(snap)
				default:
					for _, v := range r.Verdicts(snap) {
						if _, ok := v.Decision(); ok {
							t.Errorf("%s: verdict appeared under concurrency", v.Key)
							return
						}
					}
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestVerdictsAgreeAfterMutation: the agreement must survive state changing
// underneath — new samples, an OOM, and a GC that drops learned state.
func TestVerdictsAgreeAfterMutation(t *testing.T) {
	r, snap := verdictScenario(t, 3)
	checkNoDisagreement(t, r, snap, "initial")

	for i := range snap.Pods {
		snap.Pods[i].Containers[0].RestartCount++
		snap.Pods[i].Containers[0].LastOOMKilled = true
	}
	r.ObserveSnapshot(snap)
	checkNoDisagreement(t, r, snap, "after OOM")

	if n := r.GC(t0.Add(365 * 24 * time.Hour)); n == 0 {
		t.Fatalf("GC dropped nothing; the mutation case is not exercised")
	}
	counts := checkNoDisagreement(t, r, snap, "after GC")
	if counts[DispositionNeverObserved] == 0 {
		t.Fatalf("after a full GC every container should be unknown again: %v", counts)
	}
}

// mkExactHistory builds a container with exactly `samples` usage points
// spanning exactly `span`, so a gate can be tested on its boundary rather
// than near it.
func mkExactHistory(t *testing.T, name string, req model.Resources, samples int, span time.Duration) (*Recommender, *model.ClusterSnapshot) {
	t.Helper()
	r := newRec(t)
	ref := deployRef(name)
	key := model.ContainerKey{Workload: ref, Container: "app"}
	snap := &model.ClusterSnapshot{
		ClusterID: "test", Timestamp: t0.Add(span),
		Pods: []model.PodSpec{{
			UID: "pod-1", Name: name + "-a", Namespace: ref.Namespace,
			Workload: ref, Phase: "Running",
			Containers: []model.ContainerSpec{{Name: "app", Requests: req}},
		}},
	}
	for i := 0; i < samples; i++ {
		off := time.Duration(0)
		if samples > 1 {
			// Integer math so the last point lands on exactly t0+span.
			off = time.Duration(int64(span) * int64(i) / int64(samples-1))
		}
		snap.Usage = append(snap.Usage, model.Usage{
			Key: key, PodUID: "pod-1", Timestamp: t0.Add(off),
			MilliCPU: 100, MemoryBytes: 200 << 20,
		})
	}
	r.ObserveSnapshot(snap)
	return r, snap
}

// TestVerdictsAgreeAtEveryGateBoundary walks each gate across its exact
// threshold. A property corpus samples the space; it does not land on
// boundaries, and a gate that drifts by one is exactly how the two paths
// would come apart in practice.
func TestVerdictsAgreeAtEveryGateBoundary(t *testing.T) {
	cfg := DefaultConfig()
	oversized := model.Resources{MilliCPU: 4000, MemoryBytes: 8 << 30}

	t.Run("sample-count", func(t *testing.T) {
		for _, samples := range []int{
			0, 1,
			cfg.MinSamples - 2, cfg.MinSamples - 1, cfg.MinSamples, cfg.MinSamples + 1,
		} {
			// A window well clear of MinWindow, so samples is the only gate.
			r, snap := mkExactHistory(t, "samples", oversized, samples, 12*time.Hour)
			label := "samples=" + itoa(samples)
			checkNoDisagreement(t, r, snap, label)

			vs := r.Verdicts(snap)
			want := DispositionRecommended
			if samples < cfg.MinSamples {
				want = DispositionInsufficientHistory
			}
			if vs[0].Disposition != want {
				t.Fatalf("%s: disposition %q, want %q", label, vs[0].Disposition, want)
			}
		}
	})

	t.Run("window-span", func(t *testing.T) {
		samples := 4 * cfg.MinSamples // never the binding gate
		for _, span := range []time.Duration{
			cfg.MinWindow - time.Nanosecond, cfg.MinWindow, cfg.MinWindow + time.Nanosecond,
			2 * cfg.MinWindow,
		} {
			r, snap := mkExactHistory(t, "window", oversized, samples, span)
			label := "span=" + span.String()
			checkNoDisagreement(t, r, snap, label)

			vs := r.Verdicts(snap)
			if vs[0].Window != span {
				t.Fatalf("%s: window %v, want exactly %v", label, vs[0].Window, span)
			}
			want := DispositionRecommended
			if span < cfg.MinWindow {
				want = DispositionInsufficientHistory
			}
			if vs[0].Disposition != want {
				t.Fatalf("%s: disposition %q, want %q", label, vs[0].Disposition, want)
			}
		}
	})

	// The churn gate has no fixed number to stand on — the target comes out
	// of percentile math — so sweep the current request densely through it
	// and require that both sides of the boundary were actually reached.
	t.Run("change-ratio", func(t *testing.T) {
		probe, probeSnap := mkExactHistory(t, "churn", oversized, 4*cfg.MinSamples, 24*time.Hour)
		recs := probe.Recommendations(probeSnap)
		if len(recs) != 1 {
			t.Fatalf("probe produced %d recommendations, want 1", len(recs))
		}
		target := recs[0].TargetRequest

		seen := map[Disposition]int{}
		for step := 0; step <= 100; step++ {
			scale := 0.80 + float64(step)*0.005 // 0.80 … 1.30
			req := model.Resources{
				MilliCPU:    ceilInt64(float64(target.MilliCPU) * scale),
				MemoryBytes: ceilInt64(float64(target.MemoryBytes) * scale),
			}
			r, snap := mkExactHistory(t, "churn", req, 4*cfg.MinSamples, 24*time.Hour)
			label := "scale=" + itoa(step)
			checkNoDisagreement(t, r, snap, label)
			seen[r.Verdicts(snap)[0].Disposition]++
		}
		if seen[DispositionRecommended] == 0 || seen[DispositionNoSignificantChange] == 0 {
			t.Fatalf("sweep never crossed the suppression boundary: %v", seen)
		}
	})
}
