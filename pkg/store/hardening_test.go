package store

import (
	"encoding/json"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

// Sub-second timestamps are the norm (plan CreatedAt comes from a snapshot's
// time.Now().UTC()), so key ordering must be chronological, not lexicographic.
func TestLatestPlanChronologicalWithSubSecondTimestamps(t *testing.T) {
	s := open(t)
	// 0.2s is chronologically before 0.23s, but "…00.23Z" < "…00.2Z" as bytes.
	early := t0.Add(200 * time.Millisecond)
	late := t0.Add(230 * time.Millisecond)
	for _, tc := range []struct {
		at  time.Time
		usd float64
	}{{early, 1}, {late, 2}} {
		if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: tc.at, CurrentHourlyUSD: tc.usd}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestPlan("c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentHourlyUSD != 2 {
		t.Fatalf("LatestPlan returned a stale plan: got %v want 2", got.CurrentHourlyUSD)
	}
}

// A whole-second timestamp formats without a fraction ("…00Z"); any fractional
// timestamp in the same second sorts before it lexicographically.
func TestLatestPlanChronologicalAcrossWholeSecond(t *testing.T) {
	s := open(t)
	_ = s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: t0, CurrentHourlyUSD: 1})
	_ = s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: t0.Add(500 * time.Millisecond), CurrentHourlyUSD: 2})
	got, err := s.LatestPlan("c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentHourlyUSD != 2 {
		t.Fatalf("LatestPlan returned a stale plan: got %v want 2", got.CurrentHourlyUSD)
	}
}

// retainedPlanTimes reads the plan keys for a cluster straight out of bbolt so
// a test can assert exactly which entries survived pruning.
func retainedPlanTimes(t *testing.T, s *Store, cluster string) []time.Time {
	t.Helper()
	var out []time.Time
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketPlans)
		if err != nil {
			return err
		}
		return forEachPlan(b, cluster, func(_, _ []byte, ts time.Time) error {
			out = append(out, ts)
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// Pruning must drop the chronologically oldest plans, never the newest. All
// samples share a second here, so only the fraction decides the order — which
// is exactly where byte order and chronological order diverge.
func TestPruneDropsOldestByTimeNotByBytes(t *testing.T) {
	s := open(t)
	const extra = 5
	for i := 0; i < PlanHistoryLimit+extra; i++ {
		at := t0.Add(time.Duration(i) * time.Millisecond)
		if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: at, CurrentHourlyUSD: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PlanCount("c1")
	if err != nil {
		t.Fatal(err)
	}
	if n != PlanHistoryLimit {
		t.Fatalf("PlanCount = %d, want %d", n, PlanHistoryLimit)
	}
	got := retainedPlanTimes(t, s, "c1")
	if len(got) != PlanHistoryLimit {
		t.Fatalf("retained %d keys, want %d", len(got), PlanHistoryLimit)
	}
	// The survivors must be exactly the newest PlanHistoryLimit samples.
	for i, ts := range got {
		want := t0.Add(time.Duration(extra+i) * time.Millisecond)
		if !ts.Equal(want) {
			t.Fatalf("retained[%d] = %s, want %s (wrong entries pruned)", i, ts, want)
		}
	}
	latest, err := s.LatestPlan("c1")
	if err != nil {
		t.Fatal(err)
	}
	if want := float64(PlanHistoryLimit + extra - 1); latest.CurrentHourlyUSD != want {
		t.Fatalf("newest plan was pruned or shadowed: got %v want %v", latest.CurrentHourlyUSD, want)
	}
}

// EKS cluster identifiers are often ARNs, which contain '/'. The plan key is
// "<cluster>/<timestamp>", so a '/' in the id must not make one cluster's
// history visible — or prunable — from another.
func TestPlanClusterIDContainingSlashIsIsolated(t *testing.T) {
	s := open(t)
	if err := s.SavePlan(&plan.Plan{ClusterID: "arn:aws:eks:us-east-1:1:cluster", CreatedAt: t0, CurrentHourlyUSD: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		p := &plan.Plan{
			ClusterID:        "arn:aws:eks:us-east-1:1:cluster/prod",
			CreatedAt:        t0.Add(time.Duration(i) * time.Minute),
			CurrentHourlyUSD: float64(100 + i),
		}
		if err := s.SavePlan(p); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PlanCount("arn:aws:eks:us-east-1:1:cluster")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("parent cluster sees child's plans: PlanCount = %d, want 1", n)
	}
	got, err := s.LatestPlan("arn:aws:eks:us-east-1:1:cluster")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.CurrentHourlyUSD != 1 {
		t.Fatalf("parent cluster got the child's plan: %+v", got)
	}
	if n, _ := s.PlanCount("arn:aws:eks:us-east-1:1:cluster/prod"); n != 5 {
		t.Fatalf("child cluster history = %d, want 5", n)
	}
}

// Histories written before the fixed-width key format must keep ordering
// correctly alongside new ones: readers parse the suffix instead of comparing
// key bytes, so a trimmed "…00Z" and a padded "…00.000000000Z" for the same
// second are the same instant.
func TestLegacyTrimmedKeysOrderWithNewKeys(t *testing.T) {
	s := open(t)
	// Simulate an older build: raw Put with time.RFC3339Nano keys.
	legacy := []struct {
		at  time.Time
		usd float64
	}{
		{t0, 1},                              // "…00:00Z"
		{t0.Add(500 * time.Millisecond), 2},  // "…00:00.5Z"
		{t0.Add(1500 * time.Millisecond), 3}, // "…00:01.5Z"
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPlans)
		for _, l := range legacy {
			raw, err := json.Marshal(&plan.Plan{ClusterID: "c1", CreatedAt: l.at, CurrentHourlyUSD: l.usd})
			if err != nil {
				return err
			}
			k := []byte("c1/" + l.at.UTC().Format(time.RFC3339Nano))
			if err := b.Put(k, raw); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PlanCount("c1"); err != nil || n != 3 {
		t.Fatalf("legacy keys not counted: n=%d err=%v", n, err)
	}
	got, err := s.LatestPlan("c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentHourlyUSD != 3 {
		t.Fatalf("legacy ordering wrong: got %v want 3", got.CurrentHourlyUSD)
	}
	// A new-format write for an earlier instant must not become "latest".
	if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: t0.Add(time.Second), CurrentHourlyUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.LatestPlan("c1"); err != nil || got.CurrentHourlyUSD != 3 {
		t.Fatalf("new key shadowed a newer legacy key: got %v err=%v", got.CurrentHourlyUSD, err)
	}
	// And a genuinely newer one must win over every legacy key.
	if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: t0.Add(2 * time.Second), CurrentHourlyUSD: 5}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.LatestPlan("c1"); err != nil || got.CurrentHourlyUSD != 5 {
		t.Fatalf("newest plan not returned: got %v err=%v", got.CurrentHourlyUSD, err)
	}
}

// A CreatedAt that cannot be rendered into the fixed-width key layout must be
// refused: such a key would parse back as nothing, making the plan invisible
// to LatestPlan and immune to pruning — an entry that grows the file forever.
func TestSavePlanRejectsUnrepresentableCreatedAt(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
	}{
		{"year beyond four digits", time.Date(12345, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"negative year", time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: tc.at}); err == nil {
				t.Fatal("want error for unrepresentable createdAt")
			}
			if n, _ := s.PlanCount("c1"); n != 0 {
				t.Fatalf("rejected plan still stored: n=%d", n)
			}
			// Nothing unreachable was left behind in the bucket either.
			var raw int
			_ = s.db.View(func(tx *bolt.Tx) error {
				return tx.Bucket(bucketPlans).ForEach(func(_, _ []byte) error { raw++; return nil })
			})
			if raw != 0 {
				t.Fatalf("orphan key written: %d", raw)
			}
		})
	}
}

// The zero time is representable, so it is stored rather than rejected — but
// it sorts first and is pruned first. Pinning the behaviour so a future change
// is a deliberate one.
func TestSavePlanAcceptsZeroCreatedAt(t *testing.T) {
	s := open(t)
	if err := s.SavePlan(&plan.Plan{ClusterID: "c1"}); err != nil {
		t.Fatalf("zero createdAt should round-trip: %v", err)
	}
	if n, _ := s.PlanCount("c1"); n != 1 {
		t.Fatalf("PlanCount = %d, want 1", n)
	}
	// Every zero-CreatedAt plan collides on one key: history stays at 1.
	if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CurrentHourlyUSD: 9}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PlanCount("c1"); n != 1 {
		t.Fatalf("PlanCount = %d, want 1 (same key overwrites)", n)
	}
	if p, _ := s.LatestPlan("c1"); p == nil || p.CurrentHourlyUSD != 9 {
		t.Fatalf("overwrite lost: %+v", p)
	}
}

// NaN/±Inf are not representable in JSON. The write must fail loudly and leave
// the previously stored value untouched — a cost report is worse than useless
// if a poisoned float can silently replace good data or half-write a record.
func TestSaveRejectsNonFiniteFloatsAndPreservesPriorValue(t *testing.T) {
	for _, bad := range []struct {
		name string
		v    float64
	}{{"nan", math.NaN()}, {"+inf", math.Inf(1)}, {"-inf", math.Inf(-1)}} {
		t.Run("plan/"+bad.name, func(t *testing.T) {
			s := open(t)
			good := &plan.Plan{ClusterID: "c1", CreatedAt: t0, CurrentHourlyUSD: 7}
			if err := s.SavePlan(good); err != nil {
				t.Fatal(err)
			}
			err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: t0.Add(time.Minute), CurrentHourlyUSD: bad.v})
			if err == nil {
				t.Fatal("want error for non-finite float")
			}
			if n, _ := s.PlanCount("c1"); n != 1 {
				t.Fatalf("PlanCount = %d, want 1 (failed write must not persist)", n)
			}
			p, err := s.LatestPlan("c1")
			if err != nil || p == nil || p.CurrentHourlyUSD != 7 {
				t.Fatalf("prior plan not preserved: %+v %v", p, err)
			}
		})
		t.Run("snapshot/"+bad.name, func(t *testing.T) {
			s := open(t)
			if err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: "c1", Timestamp: t0,
				Nodes: []model.NodeSpec{{Name: "n1", HourlyCost: 1.25}}}); err != nil {
				t.Fatal(err)
			}
			err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: "c1", Timestamp: t0,
				Nodes: []model.NodeSpec{{Name: "n1", HourlyCost: bad.v}}})
			if err == nil {
				t.Fatal("want error for non-finite float")
			}
			got, err := s.LoadSnapshot("c1")
			if err != nil || got == nil || len(got.Nodes) != 1 || got.Nodes[0].HourlyCost != 1.25 {
				t.Fatalf("prior snapshot not preserved: %+v %v", got, err)
			}
		})
	}
}

// Cluster ids are operator-supplied strings, not identifiers. None of these
// may panic, corrupt a neighbour's history, or silently succeed-and-vanish.
func TestAdversarialClusterIDs(t *testing.T) {
	cases := []struct {
		name    string
		cluster string
		wantErr bool
	}{
		{"empty", "", true},
		{"eks arn with slash", "arn:aws:eks:eu-west-1:1:cluster/prod", false},
		{"leading slash", "/prod", false},
		{"trailing slash", "prod/", false},
		{"double slash", "prod//east", false},
		{"only slashes", "///", false},
		{"unicode", "프로덕션-클러스터-🌏", false},
		{"nul byte", "prod\x00east", false},
		{"newline", "prod\neast", false},
		{"looks like a timestamp", "2026-07-01T00:00:00.000000000Z", false},
		{"key exceeds bbolt limit", strings.Repeat("x", 40000), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			err := s.SavePlan(&plan.Plan{ClusterID: tc.cluster, CreatedAt: t0, CurrentHourlyUSD: 3})
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("SavePlan: %v", err)
			}
			n, err := s.PlanCount(tc.cluster)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("PlanCount = %d, want 1", n)
			}
			got, err := s.LatestPlan(tc.cluster)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.CurrentHourlyUSD != 3 {
				t.Fatalf("round-trip lost: %+v", got)
			}
			// Snapshots key on the whole id, so they must round-trip too.
			if err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: tc.cluster, Timestamp: t0}); err != nil {
				t.Fatalf("SaveSnapshot: %v", err)
			}
			if snap, err := s.LoadSnapshot(tc.cluster); err != nil || snap == nil || snap.ClusterID != tc.cluster {
				t.Fatalf("snapshot round-trip lost: %+v %v", snap, err)
			}
		})
	}
}

// Every pair of ids in this set is a prefix-of, suffix-of or slash-neighbour
// relation of another. Each must see exactly its own plans.
func TestPlanHistoriesAreMutuallyIsolated(t *testing.T) {
	s := open(t)
	ids := []string{"a", "a/b", "a/b/c", "a-b", "a/", "ab", "/a", ""}
	for i, id := range ids {
		if id == "" {
			continue // rejected by SavePlan; included to exercise the reader paths
		}
		// Give each cluster a distinct number of plans.
		for j := 0; j <= i; j++ {
			p := &plan.Plan{ClusterID: id, CreatedAt: t0.Add(time.Duration(j) * time.Minute),
				CurrentHourlyUSD: float64(i*100 + j)}
			if err := s.SavePlan(p); err != nil {
				t.Fatalf("SavePlan(%q): %v", id, err)
			}
		}
	}
	for i, id := range ids {
		if id == "" {
			if n, err := s.PlanCount(""); err != nil || n != 0 {
				t.Fatalf("empty id sees %d plans (err %v)", n, err)
			}
			continue
		}
		n, err := s.PlanCount(id)
		if err != nil {
			t.Fatal(err)
		}
		if n != i+1 {
			t.Fatalf("PlanCount(%q) = %d, want %d", id, n, i+1)
		}
		got, err := s.LatestPlan(id)
		if err != nil {
			t.Fatal(err)
		}
		if want := float64(i*100 + i); got == nil || got.CurrentHourlyUSD != want {
			t.Fatalf("LatestPlan(%q) = %+v, want %v", id, got, want)
		}
	}
}

// Corrupt bytes in the file must surface as an error, never as a zero-valued
// "successful" load that the brain would act on.
func TestCorruptRecordsReturnErrorsNotZeroValues(t *testing.T) {
	s := open(t)
	garbage := []byte("{not json")
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketSnapshots).Put([]byte("c1"), garbage); err != nil {
			return err
		}
		if err := tx.Bucket(bucketRecommender).Put([]byte("c1"), garbage); err != nil {
			return err
		}
		return tx.Bucket(bucketPlans).Put(planKey("c1", t0), garbage)
	}); err != nil {
		t.Fatal(err)
	}
	if snap, err := s.LoadSnapshot("c1"); err == nil || snap != nil {
		t.Fatalf("corrupt snapshot: got %+v, %v; want nil, error", snap, err)
	}
	if st, err := s.LoadRecommenderState("c1"); err == nil || st != nil {
		t.Fatalf("corrupt recommender state: got %+v, %v; want nil, error", st, err)
	}
	if p, err := s.LatestPlan("c1"); err == nil || p != nil {
		t.Fatalf("corrupt plan: got %+v, %v; want nil, error", p, err)
	}
	// The key is still well-formed, so it stays countable and prunable.
	if n, err := s.PlanCount("c1"); err != nil || n != 1 {
		t.Fatalf("PlanCount over corrupt value: n=%d err=%v", n, err)
	}
}

// A file missing its buckets (truncated, or written by another tool) must be
// reported, not dereferenced through a nil bucket.
func TestMissingBucketsAreErrorsNotPanics(t *testing.T) {
	s := open(t)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketRecommender, bucketSnapshots, bucketPlans} {
			if err := tx.DeleteBucket(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSnapshot("c1"); err == nil {
		t.Error("LoadSnapshot: want error")
	}
	if _, err := s.LoadRecommenderState("c1"); err == nil {
		t.Error("LoadRecommenderState: want error")
	}
	if _, err := s.Clusters(); err == nil {
		t.Error("Clusters: want error")
	}
	if _, err := s.LatestPlan("c1"); err == nil {
		t.Error("LatestPlan: want error")
	}
	if _, err := s.PlanCount("c1"); err == nil {
		t.Error("PlanCount: want error")
	}
	if err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: "c1"}); err == nil {
		t.Error("SaveSnapshot: want error")
	}
	if err := s.SaveRecommenderState("c1", nil); err == nil {
		t.Error("SaveRecommenderState: want error")
	}
	if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: t0}); err == nil {
		t.Error("SavePlan: want error")
	}
}

// The point of the package: state must survive a process restart.
func TestStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kilter.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: "c1", Timestamp: t0,
		Nodes: []model.NodeSpec{{Name: "n1", Ready: true}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.SavePlan(&plan.Plan{ClusterID: "c1",
			CreatedAt: t0.Add(time.Duration(i) * time.Millisecond), CurrentHourlyUSD: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveRecommenderState("c1", []recommend.CheckpointState{{Samples: 11}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if cs, err := s2.Clusters(); err != nil || len(cs) != 1 || cs[0] != "c1" {
		t.Fatalf("clusters after reopen: %v %v", cs, err)
	}
	if snap, err := s2.LoadSnapshot("c1"); err != nil || snap == nil || len(snap.Nodes) != 1 {
		t.Fatalf("snapshot lost across reopen: %+v %v", snap, err)
	}
	if n, err := s2.PlanCount("c1"); err != nil || n != 3 {
		t.Fatalf("plan history lost across reopen: n=%d err=%v", n, err)
	}
	if p, err := s2.LatestPlan("c1"); err != nil || p == nil || p.CurrentHourlyUSD != 2 {
		t.Fatalf("latest plan wrong after reopen: %+v %v", p, err)
	}
	if st, err := s2.LoadRecommenderState("c1"); err != nil || len(st) != 1 || st[0].Samples != 11 {
		t.Fatalf("recommender state lost across reopen: %+v %v", st, err)
	}
}

func TestOpenRejectsUnusablePaths(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, path string }{
		{"empty path", ""},
		{"directory", dir},
		{"missing parent", filepath.Join(dir, "no", "such", "dir", "k.db")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(tc.path)
			if err == nil {
				s.Close()
				t.Fatal("want error")
			}
		})
	}
}

// Open must time out rather than block forever when the file is already
// locked; a brain that hangs on startup is indistinguishable from a hung node.
func TestOpenTimesOutOnLockedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 5s bbolt lock timeout")
	}
	path := filepath.Join(t.TempDir(), "kilter.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	start := time.Now()
	second, err := Open(path)
	if err == nil {
		second.Close()
		t.Fatal("second Open should not have acquired the lock")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Open blocked for %s; the lock timeout is not effective", elapsed)
	}
}

// Concurrent writers to one cluster must never leave more than the limit
// retained: pruning and the append share a transaction.
func TestConcurrentSavePlanHoldsHistoryLimit(t *testing.T) {
	s := open(t)
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				// Distinct keys across goroutines, sub-second apart.
				at := t0.Add(time.Duration(w*20+j) * time.Millisecond)
				if err := s.SavePlan(&plan.Plan{ClusterID: "c1", CreatedAt: at}); err != nil {
					t.Errorf("SavePlan: %v", err)
					return
				}
				if n, err := s.PlanCount("c1"); err != nil || n > PlanHistoryLimit {
					t.Errorf("PlanCount = %d (err %v), must stay <= %d", n, err, PlanHistoryLimit)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	n, err := s.PlanCount("c1")
	if err != nil {
		t.Fatal(err)
	}
	if n != PlanHistoryLimit {
		t.Fatalf("PlanCount = %d, want %d", n, PlanHistoryLimit)
	}
	// The newest instant written by any goroutine must be the retained latest.
	p, err := s.LatestPlan("c1")
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(119 * time.Millisecond); !p.CreatedAt.Equal(want) {
		t.Fatalf("latest CreatedAt = %s, want %s", p.CreatedAt, want)
	}
}

// Saving a plan older than everything retained, with the history already full,
// prunes the plan just written — it does not evict a newer one.
func TestBackdatedPlanDoesNotEvictNewerHistory(t *testing.T) {
	s := open(t)
	for i := 0; i < PlanHistoryLimit; i++ {
		if err := s.SavePlan(&plan.Plan{ClusterID: "c1",
			CreatedAt: t0.Add(time.Duration(i) * time.Minute), CurrentHourlyUSD: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SavePlan(&plan.Plan{ClusterID: "c1",
		CreatedAt: t0.Add(-time.Hour), CurrentHourlyUSD: -1}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PlanCount("c1"); n != PlanHistoryLimit {
		t.Fatalf("PlanCount = %d, want %d", n, PlanHistoryLimit)
	}
	got := retainedPlanTimes(t, s, "c1")
	if got[0].Before(t0) {
		t.Fatalf("backdated plan retained at the cost of a newer one: %s", got[0])
	}
	if !got[0].Equal(t0) {
		t.Fatalf("oldest retained = %s, want %s", got[0], t0)
	}
}

func TestSaveNilArguments(t *testing.T) {
	s := open(t)
	if err := s.SaveSnapshot(nil); err == nil {
		t.Error("SaveSnapshot(nil): want error")
	}
	if err := s.SavePlan(nil); err == nil {
		t.Error("SavePlan(nil): want error")
	}
	// A nil checkpoint slice is legitimate (a cluster with nothing learned).
	if err := s.SaveRecommenderState("c1", nil); err != nil {
		t.Errorf("SaveRecommenderState(nil): %v", err)
	}
	if st, err := s.LoadRecommenderState("c1"); err != nil || st != nil {
		t.Errorf("nil state round-trip: %+v %v", st, err)
	}
}

// Unknown clusters read as empty, never as an error or a zero-valued plan.
func TestUnknownClusterReadsEmpty(t *testing.T) {
	s := open(t)
	if err := s.SavePlan(&plan.Plan{ClusterID: "other", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "c1", "othe", "other/x", strings.Repeat("y", 5000)} {
		if p, err := s.LatestPlan(id); err != nil || p != nil {
			t.Errorf("LatestPlan(%q) = %+v, %v; want nil, nil", id, p, err)
		}
		if n, err := s.PlanCount(id); err != nil || n != 0 {
			t.Errorf("PlanCount(%q) = %d, %v; want 0, nil", id, n, err)
		}
		if snap, err := s.LoadSnapshot(id); err != nil || snap != nil {
			t.Errorf("LoadSnapshot(%q) = %+v, %v; want nil, nil", id, snap, err)
		}
	}
}

func TestClustersListsSnapshotOwnersInOrder(t *testing.T) {
	s := open(t)
	// Insertion order deliberately unsorted.
	for _, id := range []string{"zeta", "alpha", "Mid", "alpha/child"} {
		if err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: id, Timestamp: t0}); err != nil {
			t.Fatal(err)
		}
	}
	// A cluster known only through plans is not a snapshot owner.
	if err := s.SavePlan(&plan.Plan{ClusterID: "plans-only", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Clusters()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Mid", "alpha", "alpha/child", "zeta"}
	if !slices.Equal(got, want) {
		t.Fatalf("Clusters() = %v, want %v", got, want)
	}
	if cs, err := open(t).Clusters(); err != nil || cs != nil {
		t.Fatalf("empty store Clusters() = %v, %v; want nil, nil", cs, err)
	}
}

// A record whose payload names a different cluster than its key is corrupt.
// Handing it back would mean serving one cluster's node deletions under
// another cluster's name.
func TestRecordClusterIdentityIsVerified(t *testing.T) {
	s := open(t)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		snap, err := json.Marshal(&model.ClusterSnapshot{ClusterID: "staging", Timestamp: t0})
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketSnapshots).Put([]byte("prod"), snap); err != nil {
			return err
		}
		p, err := json.Marshal(&plan.Plan{ClusterID: "staging", CreatedAt: t0})
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketPlans).Put(planKey("prod", t0), p); err != nil {
			return err
		}
		// JSON "null" decodes without error into a zero value; it must not
		// pass as a real record either.
		if err := tx.Bucket(bucketSnapshots).Put([]byte("nullish"), []byte("null")); err != nil {
			return err
		}
		return tx.Bucket(bucketPlans).Put(planKey("nullish", t0), []byte("null"))
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadSnapshot("prod"); err == nil || got != nil {
		t.Fatalf("mismatched snapshot: got %+v, %v; want nil, error", got, err)
	}
	if got, err := s.LatestPlan("prod"); err == nil || got != nil {
		t.Fatalf("mismatched plan: got %+v, %v; want nil, error", got, err)
	}
	if got, err := s.LoadSnapshot("nullish"); err == nil || got != nil {
		t.Fatalf("null snapshot: got %+v, %v; want nil, error", got, err)
	}
	if got, err := s.LatestPlan("nullish"); err == nil || got != nil {
		t.Fatalf("null plan: got %+v, %v; want nil, error", got, err)
	}
}

// encoding/json rewrites invalid UTF-8 as U+FFFD, so a non-UTF-8 cluster id
// would produce a record whose own ClusterID no longer matched the key it was
// filed under — stored successfully, unreadable forever after.
func TestNonUTF8ClusterIDIsRejectedNotMangled(t *testing.T) {
	bad := []string{"\xff", "prod\xff\xfe", "\x80east"}
	for _, id := range bad {
		t.Run(strconv.Quote(id), func(t *testing.T) {
			s := open(t)
			if err := s.SavePlan(&plan.Plan{ClusterID: id, CreatedAt: t0}); err == nil {
				t.Error("SavePlan: want error")
			}
			if err := s.SaveSnapshot(&model.ClusterSnapshot{ClusterID: id, Timestamp: t0}); err == nil {
				t.Error("SaveSnapshot: want error")
			}
			if err := s.SaveRecommenderState(id, []recommend.CheckpointState{{Samples: 1}}); err == nil {
				t.Error("SaveRecommenderState: want error")
			}
			// Nothing was written under any key.
			var keys int
			_ = s.db.View(func(tx *bolt.Tx) error {
				for _, b := range [][]byte{bucketRecommender, bucketSnapshots, bucketPlans} {
					if err := tx.Bucket(b).ForEach(func(_, _ []byte) error { keys++; return nil }); err != nil {
						return err
					}
				}
				return nil
			})
			if keys != 0 {
				t.Errorf("rejected id left %d keys behind", keys)
			}
		})
	}
	// The valid-UTF-8 oddities stay accepted: rejecting them would lock out
	// legitimate operator-chosen ids.
	for _, id := range []string{"prod\x00east", "프로덕션", "a b", "ÿ"} {
		s := open(t)
		if err := s.SavePlan(&plan.Plan{ClusterID: id, CreatedAt: t0}); err != nil {
			t.Errorf("SavePlan(%q): %v", id, err)
			continue
		}
		if p, err := s.LatestPlan(id); err != nil || p == nil || p.ClusterID != id {
			t.Errorf("round-trip lost for %q: %+v %v", id, p, err)
		}
	}
}
