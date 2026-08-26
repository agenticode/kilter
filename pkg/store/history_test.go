package store

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/agenticode/kilter/pkg/model"
)

// snapAt builds a small but structurally real snapshot: a node and a pod, so
// the encoded record exercises the same shape production stores.
func snapAt(cluster string, ts time.Time, nodes int) *model.ClusterSnapshot {
	s := &model.ClusterSnapshot{ClusterID: cluster, Timestamp: ts}
	for i := 0; i < nodes; i++ {
		s.Nodes = append(s.Nodes, model.NodeSpec{
			Name: fmt.Sprintf("n%d", i), Ready: true, InstanceType: "m5.xlarge", Provider: "aws",
			Capacity:    model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
			Allocatable: model.Resources{MilliCPU: 4000, MemoryBytes: 16 << 30},
		})
	}
	s.Pods = []model.PodSpec{{
		UID: "u1", Name: "web-1", Namespace: "prod", NodeName: "n0", Phase: "Running",
		Workload:   model.WorkloadRef{Kind: model.KindDeployment, Namespace: "prod", Name: "web"},
		Containers: []model.ContainerSpec{{Name: "app", Requests: model.Resources{MilliCPU: 500, MemoryBytes: 1 << 30}}},
	}}
	return s
}

// testRetention is the tiny bound the eviction tests write past. Cadence 0
// retains every distinct timestamp, so the caps are what is under test.
func testRetention(max int, maxBytes int64) SnapshotRetention {
	return SnapshotRetention{Cadence: 0, MaxPerCluster: max, MaxBytesPerCluster: maxBytes, MaxWindow: 400 * 24 * time.Hour}
}

func TestSnapshotHistoryRoundtripsInTimestampOrder(t *testing.T) {
	s := open(t)
	if err := s.SetSnapshotRetention(testRetention(50, 1<<20)); err != nil {
		t.Fatal(err)
	}
	// Written out of order on purpose: the query sorts, the caller does not.
	for _, i := range []int{3, 0, 4, 1, 2} {
		if err := s.SaveSnapshotAt(snapAt("c1", t0.Add(time.Duration(i)*time.Hour), i+1)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Snapshots("c1", t0, t0.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d snapshots, want 5", len(got))
	}
	for i, snap := range got {
		want := t0.Add(time.Duration(i) * time.Hour)
		if !snap.Timestamp.Equal(want) {
			t.Fatalf("snapshot %d at %v, want %v", i, snap.Timestamp, want)
		}
		if len(snap.Nodes) != i+1 {
			t.Fatalf("snapshot %d holds %d nodes, want %d — records are crossed", i, len(snap.Nodes), i+1)
		}
		if snap.ClusterID != "c1" {
			t.Fatalf("snapshot %d holds cluster %q", i, snap.ClusterID)
		}
	}
}

func TestSnapshotHistoryWindowIsHalfOpen(t *testing.T) {
	s := open(t)
	if err := s.SetSnapshotRetention(testRetention(50, 1<<20)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := s.SaveSnapshotAt(snapAt("c1", t0.Add(time.Duration(i)*time.Hour), 1)); err != nil {
			t.Fatal(err)
		}
	}
	// [t0+1h, t0+3h) must hold exactly the points at +1h and +2h: backtest's
	// SnapshotSource contract is [from, to), and a snapshot at exactly `to`
	// belongs to the next window.
	got, err := s.Snapshots("c1", t0.Add(time.Hour), t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Timestamp.Equal(t0.Add(time.Hour)) || !got[1].Timestamp.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("half-open window returned %d snapshots: %v", len(got), timestamps(got))
	}
}

func timestamps(snaps []*model.ClusterSnapshot) []time.Time {
	out := make([]time.Time, len(snaps))
	for i, s := range snaps {
		out[i] = s.Timestamp
	}
	return out
}

// TestSnapshotHistoryEvictsPastTheCountCap writes past the bound and asserts
// the oldest are gone — the bound is measured, not hoped.
func TestSnapshotHistoryEvictsPastTheCountCap(t *testing.T) {
	s := open(t)
	const cap = 8
	if err := s.SetSnapshotRetention(testRetention(cap, 1<<20)); err != nil {
		t.Fatal(err)
	}
	const wrote = 25
	for i := 0; i < wrote; i++ {
		if err := s.SaveSnapshotAt(snapAt("c1", t0.Add(time.Duration(i)*time.Hour), 1)); err != nil {
			t.Fatal(err)
		}
		n, err := s.SnapshotHistoryCount("c1")
		if err != nil {
			t.Fatal(err)
		}
		if n > cap {
			t.Fatalf("after %d writes the history holds %d, over the %d cap", i+1, n, cap)
		}
	}
	got, err := s.Snapshots("c1", t0, t0.Add(wrote*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != cap {
		t.Fatalf("history holds %d, want %d", len(got), cap)
	}
	// The survivors must be the NEWEST, not the first written.
	for i, snap := range got {
		want := t0.Add(time.Duration(wrote-cap+i) * time.Hour)
		if !snap.Timestamp.Equal(want) {
			t.Fatalf("survivor %d is at %v, want %v — eviction is not oldest-first", i, snap.Timestamp, want)
		}
	}
}

func TestSnapshotHistoryEvictsPastTheByteBudget(t *testing.T) {
	s := open(t)
	// A budget far below what 30 records need, with a count cap that cannot
	// be the thing that binds.
	const budget = 6 << 10
	if err := s.SetSnapshotRetention(testRetention(10000, budget)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := s.SaveSnapshotAt(snapAt("c1", t0.Add(time.Duration(i)*time.Hour), 20)); err != nil {
			t.Fatal(err)
		}
		n, err := s.SnapshotHistoryBytes("c1")
		if err != nil {
			t.Fatal(err)
		}
		// One record may exceed the budget alone (the newest is always kept);
		// two must not.
		if i > 0 && n > budget {
			t.Fatalf("after %d writes the history holds %d bytes, over the %d budget", i+1, n, budget)
		}
	}
	got, err := s.Snapshots("c1", t0, t0.Add(30*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) >= 30 {
		t.Fatalf("byte budget retained %d of 30 snapshots; want some but not all", len(got))
	}
	if !got[len(got)-1].Timestamp.Equal(t0.Add(29 * time.Hour)) {
		t.Fatalf("newest survivor is %v, want the last write", got[len(got)-1].Timestamp)
	}
}

// TestSingleOversizeRecordIsRetainedAlone pins the documented exception: a
// record bigger than the whole budget still leaves one snapshot behind. An
// empty history reads as "nothing happened"; one over-budget row reads as
// what it is.
func TestSingleOversizeRecordIsRetainedAlone(t *testing.T) {
	s := open(t)
	if err := s.SetSnapshotRetention(testRetention(100, 1024)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.SaveSnapshotAt(snapAt("c1", t0.Add(time.Duration(i)*time.Hour), 60)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SnapshotHistoryCount("c1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("history holds %d records, want exactly the newest", n)
	}
}

func TestDefaultRetentionThinsToItsCadence(t *testing.T) {
	s := open(t)
	// Default cadence is an hour; a 5-minute ingest cadence over six hours is
	// 72 writes and must leave 6 rows.
	base := t0
	for i := 0; i < 72; i++ {
		if err := s.SaveSnapshotAt(snapAt("c1", base.Add(time.Duration(i*5)*time.Minute), 1)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SnapshotHistoryCount("c1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("hourly cadence retained %d of 72 five-minute snapshots, want 6", n)
	}
	got, err := s.Snapshots("c1", base, base.Add(6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i, snap := range got {
		want := base.Add(time.Duration(i) * time.Hour)
		if !snap.Timestamp.Equal(want) {
			t.Fatalf("retained %v for bucket %d, want the first of the bucket (%v)", snap.Timestamp, i, want)
		}
	}
}

// TestResavingTheSameTimestampReplaces is the idempotence the ingest path
// depends on: replaying a recorded history must not double the rows, and a
// duplicate timestamp would break backtest's uniqueness requirement outright.
func TestResavingTheSameTimestampReplaces(t *testing.T) {
	s := open(t)
	for i := 0; i < 4; i++ {
		if err := s.SaveSnapshotAt(snapAt("c1", t0, 7)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SnapshotHistoryCount("c1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("four saves of one timestamp produced %d rows", n)
	}
	got, err := s.Snapshots("c1", t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Nodes) != 7 {
		t.Fatalf("replacement lost the payload: %d rows", len(got))
	}
}

func TestSnapshotWindowWiderThanTheCapIsRefused(t *testing.T) {
	s := open(t)
	if err := s.SaveSnapshotAt(snapAt("c1", t0, 1)); err != nil {
		t.Fatal(err)
	}
	// Ten years. A caller who typed the wrong unit is told so rather than
	// handed a scan whose cost it did not intend.
	_, err := s.Snapshots("c1", t0.Add(-10*365*24*time.Hour), t0.Add(time.Hour))
	if err == nil {
		t.Fatal("a ten-year window was accepted")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("refusal does not name the cap: %v", err)
	}
	// And the same query inside the cap still works.
	if _, err := s.Snapshots("c1", t0, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotWindowMustNotBeEmptyOrInverted(t *testing.T) {
	s := open(t)
	for _, tc := range []struct{ from, to time.Time }{
		{t0, t0},
		{t0.Add(time.Hour), t0},
	} {
		if _, err := s.Snapshots("c1", tc.from, tc.to); err == nil {
			t.Fatalf("window [%v, %v) was accepted", tc.from, tc.to)
		}
	}
}

func TestSnapshotHistoryRejectsAZeroTimestamp(t *testing.T) {
	s := open(t)
	err := s.SaveSnapshotAt(&model.ClusterSnapshot{ClusterID: "c1"})
	if err == nil {
		t.Fatal("a snapshot with no timestamp was stored")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("error does not name the problem: %v", err)
	}
}

func TestSnapshotHistoryIsolatesClusterIDsContainingSlashes(t *testing.T) {
	s := open(t)
	if err := s.SetSnapshotRetention(testRetention(50, 1<<20)); err != nil {
		t.Fatal(err)
	}
	// An EKS ARN contains '/', and "a" is a prefix of "a/b".
	parent, child := "a", "a/b"
	if err := s.SaveSnapshotAt(snapAt(parent, t0, 1)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.SaveSnapshotAt(snapAt(child, t0.Add(time.Duration(i)*time.Hour), 2)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.SnapshotHistoryCount(parent)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cluster %q sees %d rows; the child's history leaked into it", parent, n)
	}
	got, err := s.Snapshots(parent, t0, t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ClusterID != parent {
		t.Fatalf("parent query returned %d rows for %v", len(got), timestamps(got))
	}
	childRows, err := s.SnapshotHistoryCount(child)
	if err != nil {
		t.Fatal(err)
	}
	if childRows != 3 {
		t.Fatalf("child holds %d rows, want 3 — the parent's prune ate them", childRows)
	}
}

func TestUnknownSnapshotFrameIsRefusedNotGuessed(t *testing.T) {
	s := open(t)
	if err := s.SaveSnapshotAt(snapAt("c1", t0, 1)); err != nil {
		t.Fatal(err)
	}
	// Overwrite the record with a future framing.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapHistory)
		if err != nil {
			return err
		}
		return b.Put(snapKey("c1", t0), []byte("KSN9\x00\x00"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshots("c1", t0, t0.Add(time.Hour)); err == nil {
		t.Fatal("a record in an unknown frame was decoded anyway")
	}
}

func TestSnapshotHistoryIsUnaffectedByQueryOrder(t *testing.T) {
	s := open(t)
	if err := s.SetSnapshotRetention(testRetention(64, 1<<20)); err != nil {
		t.Fatal(err)
	}
	order := rand.New(rand.NewSource(7)).Perm(20)
	for _, i := range order {
		if err := s.SaveSnapshotAt(snapAt("c1", t0.Add(time.Duration(i)*time.Hour), 1+i%3)); err != nil {
			t.Fatal(err)
		}
	}
	// Eight reads in one process: Go randomizes map iteration on every range,
	// so in-process repetition is the real determinism test.
	var first []time.Time
	for r := 0; r < 8; r++ {
		got, err := s.Snapshots("c1", t0, t0.Add(20*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ts := timestamps(got)
		if r == 0 {
			first = ts
			continue
		}
		if len(ts) != len(first) {
			t.Fatalf("read %d returned %d snapshots, first read returned %d", r, len(ts), len(first))
		}
		for i := range ts {
			if !ts[i].Equal(first[i]) {
				t.Fatalf("read %d differs at %d: %v vs %v", r, i, ts[i], first[i])
			}
		}
	}
}

func TestSnapshotRetentionRejectsUnboundedConfigurations(t *testing.T) {
	s := open(t)
	base := DefaultSnapshotRetention()
	bad := []SnapshotRetention{
		func() SnapshotRetention { r := base; r.MaxPerCluster = 0; return r }(),
		func() SnapshotRetention { r := base; r.MaxBytesPerCluster = 0; return r }(),
		func() SnapshotRetention { r := base; r.MaxWindow = 0; return r }(),
		func() SnapshotRetention { r := base; r.Cadence = -time.Hour; return r }(),
	}
	for i, r := range bad {
		if err := s.SetSnapshotRetention(r); err == nil {
			t.Fatalf("case %d: an unbounded retention was accepted", i)
		}
	}
	if got := s.SnapshotRetention(); got != base {
		t.Fatalf("a rejected retention was applied anyway: %+v", got)
	}
}
