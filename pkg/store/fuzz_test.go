package store

import (
	"bytes"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agenticode/kilter/pkg/plan"
)

// fuzzTime maps an arbitrary int64 onto an instant the key layout can render.
// Years outside [0,9999] are rejected by SavePlan by design, so the property
// under test is about ordering, not about that guard.
func fuzzTime(n int64) time.Time {
	const spanYears = 8000
	sec := n % (spanYears * 365 * 24 * 3600)
	nsec := n % 1e9
	if nsec < 0 {
		nsec += 1e9
	}
	return time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(sec)*time.Second + time.Duration(nsec))
}

// The whole plan-history scheme rests on one invariant: within a cluster, the
// byte order of plan keys is chronological order. bbolt sorts by bytes, so if
// that ever stops holding, cursor scans silently return the wrong "latest".
func FuzzPlanKeyOrderMatchesTime(f *testing.F) {
	f.Add("c1", int64(0), int64(1))
	f.Add("c1", int64(1e9), int64(1e9+1)) // adjacent nanoseconds
	f.Add("c1", int64(0), int64(5e8))     // whole second vs fraction
	f.Add("c1", int64(2e8), int64(2.3e8)) // trimmed vs untrimmed
	f.Add("arn:aws:eks:x:1:cluster/p", int64(-1), int64(1))
	f.Fuzz(func(t *testing.T, cluster string, a, b int64) {
		ta, tb := fuzzTime(a), fuzzTime(b)
		ka, kb := planKey(cluster, ta), planKey(cluster, tb)

		// Round-trip: the key must decode back to the same instant, or the
		// entry would be unreadable and unprunable.
		for _, tc := range []struct {
			key []byte
			ts  time.Time
		}{{ka, ta}, {kb, tb}} {
			got, ok := parsePlanTime(tc.key[len(cluster)+1:])
			if !ok {
				t.Fatalf("planKey(%q, %s) does not parse back", cluster, tc.ts)
			}
			if !got.Equal(tc.ts) {
				t.Fatalf("round-trip %s -> %s", tc.ts, got)
			}
		}

		// Order agreement, in both directions.
		byBytes := bytes.Compare(ka, kb)
		switch {
		case ta.Before(tb) && byBytes >= 0:
			t.Fatalf("%s < %s but key order is %d\n%s\n%s", ta, tb, byBytes, ka, kb)
		case ta.After(tb) && byBytes <= 0:
			t.Fatalf("%s > %s but key order is %d\n%s\n%s", ta, tb, byBytes, ka, kb)
		case ta.Equal(tb) && byBytes != 0:
			t.Fatalf("%s == %s but keys differ\n%s\n%s", ta, tb, ka, kb)
		}
	})
}

// Plan keys are "<cluster>/<timestamp>" scanned by prefix, and cluster ids are
// operator-supplied strings that may contain '/' (EKS ARNs do). A key must be
// attributed to a scanning cluster only when the ids are identical — otherwise
// one cluster reads, and prunes, another's history.
func FuzzPlanKeyClusterIsolation(f *testing.F) {
	f.Add("a", "a/b", int64(1))
	f.Add("a/b", "a", int64(1))
	f.Add("prod", "prod-east", int64(1))
	f.Add("", "/", int64(1))
	f.Add("a", "a/2026-07-01T00:00:00.000000000Z", int64(1))
	f.Fuzz(func(t *testing.T, scanner, owner string, n int64) {
		ts := fuzzTime(n)
		key := planKey(owner, ts)
		prefix := []byte(scanner + "/")
		if !bytes.HasPrefix(key, prefix) {
			return // the cursor scan would never reach this key
		}
		got, ok := parsePlanTime(key[len(prefix):])
		if !ok {
			return // skipped by forEachPlan, which is the correct outcome
		}
		if scanner != owner {
			t.Fatalf("cluster %q claims a key owned by %q: %s", scanner, owner, key)
		}
		if !got.Equal(ts) {
			t.Fatalf("own key decoded to %s, want %s", got, ts)
		}
	})
}

// End-to-end: whatever sequence of saves arrives, the store must retain at
// most PlanHistoryLimit plans per cluster and report the chronologically
// newest retained plan as the latest.
func FuzzPlanHistoryInvariants(f *testing.F) {
	f.Add(int64(0), int64(1), int64(2), uint8(3))
	f.Add(int64(0), int64(0), int64(0), uint8(60))
	f.Add(int64(1e9), int64(-1e9), int64(5e8), uint8(55))
	f.Fuzz(func(t *testing.T, a, b, c int64, count uint8) {
		// Bound the work: fuzzing must stay fast enough to explore.
		n := int(count)%(PlanHistoryLimit+8) + 1
		steps := []int64{a, b, c}

		s, err := Open(filepath.Join(t.TempDir(), "kilter.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		const cluster = "arn:aws:eks:eu-west-1:1:cluster/prod"
		var newest time.Time
		var saved int
		for i := 0; i < n; i++ {
			at := fuzzTime(steps[i%len(steps)] + int64(i))
			err := s.SavePlan(&plan.Plan{ClusterID: cluster, CreatedAt: at,
				CurrentHourlyUSD: float64(i)})
			if err != nil {
				t.Fatalf("SavePlan(%s): %v", at, err)
			}
			saved++
			if at.After(newest) {
				newest = at
			}
		}

		got, err := s.PlanCount(cluster)
		if err != nil {
			t.Fatal(err)
		}
		if got > PlanHistoryLimit {
			t.Fatalf("PlanCount = %d exceeds limit %d", got, PlanHistoryLimit)
		}
		if got == 0 {
			t.Fatalf("saved %d plans, retained none", saved)
		}
		latest, err := s.LatestPlan(cluster)
		if err != nil {
			t.Fatal(err)
		}
		if latest == nil {
			t.Fatal("LatestPlan returned nil after saves")
		}
		if latest.ClusterID != cluster {
			t.Fatalf("LatestPlan cluster = %q, want %q", latest.ClusterID, cluster)
		}
		// The newest instant ever written is never the one pruned: pruning
		// only ever drops entries older than the retained window.
		if !latest.CreatedAt.Equal(newest) {
			t.Fatalf("LatestPlan CreatedAt = %s, want %s", latest.CreatedAt, newest)
		}
		// A neighbouring id must see nothing of it.
		if n, err := s.PlanCount(cluster + "/child"); err != nil || n != 0 {
			t.Fatalf("child id sees %d plans (err %v)", n, err)
		}
		if n, err := s.PlanCount(strings.TrimSuffix(cluster, "/prod")); err != nil || n != 0 {
			t.Fatalf("parent id sees %d plans (err %v)", n, err)
		}
	})
}

// Cluster ids are arbitrary bytes; no id may panic, silently swallow a write,
// or make a neighbouring id's history readable.
func FuzzClusterIDRoundtrip(f *testing.F) {
	f.Add("c1")
	f.Add("")
	f.Add("/")
	f.Add("\x00\xff")
	f.Add(strings.Repeat("z", 300))
	f.Fuzz(func(t *testing.T, cluster string) {
		s, err := Open(filepath.Join(t.TempDir(), "kilter.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		// Ids that cannot round-trip are refused, not half-stored: empty, not
		// valid UTF-8 (json.Marshal would rewrite it to U+FFFD), or past
		// bbolt's key limit.
		if cluster == "" || !utf8.ValidString(cluster) || len(cluster) > 30000 {
			if err := s.SavePlan(&plan.Plan{ClusterID: cluster, CreatedAt: t0}); err == nil {
				t.Fatalf("SavePlan(%q) should have been rejected", cluster)
			}
			if n, err := s.PlanCount(cluster); err != nil || n != 0 {
				t.Fatalf("rejected id %q left %d plans (err %v)", cluster, n, err)
			}
			return
		}

		want := math.Float64frombits(0x4048000000000000) // 48.0, exactly representable
		if err := s.SavePlan(&plan.Plan{ClusterID: cluster, CreatedAt: t0, CurrentHourlyUSD: want}); err != nil {
			t.Fatalf("SavePlan(%q): %v", cluster, err)
		}
		got, err := s.LatestPlan(cluster)
		if err != nil {
			t.Fatalf("LatestPlan(%q): %v", cluster, err)
		}
		if got == nil || got.CurrentHourlyUSD != want || got.ClusterID != cluster {
			t.Fatalf("round-trip lost for %q: %+v", cluster, got)
		}
		if n, err := s.PlanCount(cluster); err != nil || n != 1 {
			t.Fatalf("PlanCount(%q) = %d, %v; want 1, nil", cluster, n, err)
		}
		// Appending a path segment must not reach the parent's plan.
		if p, err := s.LatestPlan(cluster + "/x"); err != nil || p != nil {
			t.Fatalf("child id %q/x sees the parent's plan: %+v %v", cluster, p, err)
		}
	})
}
