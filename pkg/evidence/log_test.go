package evidence

import (
	"fmt"
	"reflect"
	"testing"
)

// fixedBytes is a trivial byte cost so budget arithmetic in these tests is
// exact and readable.
const fixedBytes = int64(100)

func newTestLog(t *testing.T) *budgetedLog[int] {
	t.Helper()
	return newBudgetedLog[int]()
}

func checkIntLog(t *testing.T, l *budgetedLog[int]) {
	t.Helper()
	checkLog(t, "log", l, func(*int) int64 { return fixedBytes })
}

func appendN(l *budgetedLog[int], ref SubjectRef, seq *uint64, capPerSubject, n int) int {
	evicted := 0
	for i := 0; i < n; i++ {
		*seq++
		evicted += l.append(ref, i, fixedBytes, *seq, capPerSubject)
	}
	return evicted
}

// TestLogPerSubjectCap: the per-subject ring cap is a hard bound and every
// cap eviction is reported exactly once, including when the cap shrinks
// between appends (which a checkpoint restore under a tightened config can
// do). Under-reporting here silently corrupts the global byte accounting.
func TestLogPerSubjectCap(t *testing.T) {
	tests := []struct {
		name        string
		caps        []int // cap used for each successive append
		wantLen     int
		wantEvicted int
	}{
		{"under cap", []int{8, 8, 8}, 3, 0},
		{"at cap", []int{3, 3, 3}, 3, 0},
		{"over cap", []int{3, 3, 3, 3, 3}, 3, 2},
		{"cap of one", []int{1, 1, 1, 1}, 1, 3},
		{"cap shrinks", []int{8, 8, 8, 8, 8, 8, 8, 8, 2}, 2, 7},
		{"cap collapses to one", []int{6, 6, 6, 6, 6, 6, 1}, 1, 6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLog(t)
			ref := subj("a")
			var seq uint64
			evicted := 0
			for i, c := range tc.caps {
				seq++
				evicted += l.append(ref, i, fixedBytes, seq, c)
			}
			s := l.subs[ref]
			if s.ring.len() != tc.wantLen {
				t.Errorf("ring len = %d, want %d", s.ring.len(), tc.wantLen)
			}
			if evicted != tc.wantEvicted {
				t.Errorf("reported evictions = %d, want %d", evicted, tc.wantEvicted)
			}
			checkIntLog(t, l)
		})
	}
}

// TestLogBudgetEvictsGloballyOldest: budget pressure must evict by global
// arrival order across subjects, not per-subject and not in map order.
func TestLogBudgetEvictsGloballyOldest(t *testing.T) {
	l := newTestLog(t)
	var seq uint64
	// Interleave three subjects so global order != per-subject order.
	refs := []SubjectRef{subj("a"), subj("b"), subj("c")}
	for round := 0; round < 4; round++ {
		for _, r := range refs {
			seq++
			l.append(r, round, fixedBytes, seq, 16)
		}
	}
	if l.count != 12 {
		t.Fatalf("count = %d, want 12", l.count)
	}
	// Room for 5 entries plus the three subject overheads.
	budget := 5*fixedBytes + 3*subjectOverheadBytes
	evicted := l.enforceBudget(budget)
	if evicted != 7 {
		t.Errorf("evicted %d, want 7", evicted)
	}
	if l.bytes > budget {
		t.Errorf("bytes = %d over budget %d", l.bytes, budget)
	}
	// The 5 survivors must be the 5 highest sequence numbers (8..12).
	var gotSeqs []uint64
	for _, s := range l.subs {
		for i := 0; i < s.ring.len(); i++ {
			gotSeqs = append(gotSeqs, s.ring.at(i).seq)
		}
	}
	want := map[uint64]bool{8: true, 9: true, 10: true, 11: true, 12: true}
	if len(gotSeqs) != 5 {
		t.Fatalf("%d survivors, want 5", len(gotSeqs))
	}
	for _, s := range gotSeqs {
		if !want[s] {
			t.Errorf("seq %d survived but is not among the 5 newest", s)
		}
	}
	checkIntLog(t, l)
}

// TestLogBudgetDropsEmptySubjects: a subject evicted down to nothing must
// release its overhead, otherwise 50k churning subjects leak 256B each
// forever.
func TestLogBudgetDropsEmptySubjects(t *testing.T) {
	l := newTestLog(t)
	var seq uint64
	for i := 0; i < 20; i++ {
		appendN(l, subj(fmt.Sprintf("s%02d", i)), &seq, 4, 3)
	}
	if len(l.subs) != 20 {
		t.Fatalf("subjects = %d, want 20", len(l.subs))
	}
	// A budget that fits only a couple of subjects' worth of anything.
	l.enforceBudget(minBudgetBytes / 64)
	if len(l.subs) >= 20 {
		t.Errorf("no subject was dropped: %d remain", len(l.subs))
	}
	for ref, s := range l.subs {
		if s.ring.len() == 0 {
			t.Errorf("empty subject %v retained", ref)
		}
	}
	checkIntLog(t, l)

	// Draining everything must leave a genuinely empty log.
	l.enforceBudget(0)
	if l.count != 0 || len(l.subs) != 0 || l.bytes != 0 || l.heap.len() != 0 {
		t.Fatalf("after full drain: count=%d subs=%d bytes=%d heap=%d",
			l.count, len(l.subs), l.bytes, l.heap.len())
	}
}

// TestLogFilterAll covers age-style pruning that removes from the middle of
// a ring and empties whole subjects.
func TestLogFilterAll(t *testing.T) {
	l := newTestLog(t)
	var seq uint64
	appendN(l, subj("keep"), &seq, 16, 5)
	appendN(l, subj("drop"), &seq, 16, 5)
	// Drop everything belonging to "drop" plus the middle of "keep".
	removed := l.filterAll(func(ref SubjectRef, e *entry[int]) bool {
		if ref.Key == "drop" {
			return false
		}
		return e.v != 2
	})
	if removed != 6 {
		t.Errorf("removed = %d, want 6", removed)
	}
	if _, ok := l.subs[subj("drop")]; ok {
		t.Error("fully-filtered subject was retained")
	}
	got := contents(&l.subs[subj("keep")].ring)
	var vals []int
	for _, e := range got {
		vals = append(vals, e.v)
	}
	if !reflect.DeepEqual(vals, []int{0, 1, 3, 4}) {
		t.Errorf("survivors = %v, want [0 1 3 4]", vals)
	}
	checkIntLog(t, l)
}

// TestLogFilterAllFixesHeapAfterFrontChange: pruning the oldest entry of a
// subject changes its heap key. If the heap is not fixed, the next budget
// eviction picks the wrong victim.
func TestLogFilterAllFixesHeapAfterFrontChange(t *testing.T) {
	l := newTestLog(t)
	var seq uint64
	a, b := subj("a"), subj("b")
	appendN(l, a, &seq, 16, 3) // seq 1,2,3
	appendN(l, b, &seq, 16, 3) // seq 4,5,6
	// Prune a's two oldest: a's front becomes seq 3, still ahead of b's 4.
	l.filterAll(func(ref SubjectRef, e *entry[int]) bool {
		return !(ref.Key == "a" && e.seq <= 2)
	})
	checkIntLog(t, l)
	if got := l.heap.peek().ref; got != a {
		t.Fatalf("heap min = %v, want %v", got, a)
	}
	// Now prune a's last entry: b (seq 4) must become the minimum.
	l.filterAll(func(ref SubjectRef, e *entry[int]) bool { return ref.Key != "a" })
	checkIntLog(t, l)
	if l.heap.len() != 1 || l.heap.peek().ref != b {
		t.Fatalf("after dropping a, heap = %d items, min %v", l.heap.len(), l.heap.peek().ref)
	}
}

// TestLogSortedRefsDeterministic: subject enumeration must not leak map
// order. Building the same set in different insertion orders must give the
// same slice, sorted by (Cluster, Kind, Key).
func TestLogSortedRefsDeterministic(t *testing.T) {
	mk := func(order []int) []SubjectRef {
		l := newTestLog(t)
		var seq uint64
		all := []SubjectRef{
			{Cluster: "b", Kind: "node", Key: "n1"},
			{Cluster: "a", Kind: "workload", Key: "w2"},
			{Cluster: "a", Kind: "container", Key: "c9"},
			{Cluster: "a", Kind: "workload", Key: "w1"},
			{Cluster: "", Kind: "cluster", Key: "z"},
		}
		for _, i := range order {
			appendN(l, all[i], &seq, 4, 1)
		}
		return l.sortedRefs()
	}
	want := []SubjectRef{
		{Cluster: "", Kind: "cluster", Key: "z"},
		{Cluster: "a", Kind: "container", Key: "c9"},
		{Cluster: "a", Kind: "workload", Key: "w1"},
		{Cluster: "a", Kind: "workload", Key: "w2"},
		{Cluster: "b", Kind: "node", Key: "n1"},
	}
	for _, order := range [][]int{{0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {2, 0, 4, 1, 3}} {
		if got := mk(order); !reflect.DeepEqual(got, want) {
			t.Errorf("insertion order %v gave %v", order, got)
		}
	}
}

// TestLogAppendReusesSubject: re-appending after a subject was dropped must
// re-charge overhead exactly once, not twice and not zero times.
func TestLogAppendReusesSubject(t *testing.T) {
	l := newTestLog(t)
	var seq uint64
	ref := subj("a")
	appendN(l, ref, &seq, 4, 3)
	l.enforceBudget(0)
	checkIntLog(t, l)
	appendN(l, ref, &seq, 4, 2)
	if l.bytes != 2*fixedBytes+subjectOverheadBytes {
		t.Errorf("bytes = %d, want %d", l.bytes, 2*fixedBytes+subjectOverheadBytes)
	}
	checkIntLog(t, l)
}
