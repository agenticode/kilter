package evidence

import (
	"reflect"
	"testing"
)

// contents materializes the ring oldest-first.
func contents[T any](r *ring[T]) []T {
	out := make([]T, 0, r.len())
	for i := 0; i < r.len(); i++ {
		out = append(out, *r.at(i))
	}
	return out
}

// TestRingPushEvictsOldest pins the FIFO contract across the cap boundary:
// len(r) == min(pushes, cap), contents are the newest `cap` values in order,
// and eviction is reported exactly when it happens.
func TestRingPushEvictsOldest(t *testing.T) {
	tests := []struct {
		name       string
		cap        int
		pushes     int
		wantLen    int
		wantFirst  int
		wantEvicts int
	}{
		{"under cap", 8, 3, 3, 0, 0},
		{"exactly cap", 8, 8, 8, 0, 0},
		{"one over", 8, 9, 8, 1, 1},
		{"many over", 4, 100, 4, 96, 96},
		{"cap one", 1, 5, 1, 4, 4},
		{"single push", 16, 1, 1, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r ring[int]
			evicts := 0
			for i := 0; i < tc.pushes; i++ {
				out, did := r.push(i, tc.cap)
				if did {
					evicts++
					if want := i - tc.cap; out != want {
						t.Errorf("push %d evicted %d, want %d", i, out, want)
					}
				}
			}
			if r.len() != tc.wantLen {
				t.Fatalf("len = %d, want %d", r.len(), tc.wantLen)
			}
			if evicts != tc.wantEvicts {
				t.Errorf("evictions = %d, want %d", evicts, tc.wantEvicts)
			}
			if len(r.buf) > tc.cap {
				t.Errorf("backing array %d exceeds cap %d", len(r.buf), tc.cap)
			}
			got := contents(&r)
			want := make([]int, 0, tc.wantLen)
			for i := 0; i < tc.wantLen; i++ {
				want = append(want, tc.wantFirst+i)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("contents = %v, want %v", got, want)
			}
		})
	}
}

// TestRingGrowsLazily asserts the sparse-subject promise: a ring configured
// with a huge cap that only ever holds a few elements must not allocate the
// cap. 50k mostly-idle subjects depend on this.
func TestRingGrowsLazily(t *testing.T) {
	var r ring[int]
	for i := 0; i < 5; i++ {
		r.push(i, 1<<20)
	}
	if len(r.buf) > 8 {
		t.Fatalf("backing array grew to %d for 5 elements", len(r.buf))
	}
}

// TestRingDropFront covers the empty case and head-index reset.
func TestRingDropFront(t *testing.T) {
	var r ring[int]
	if _, ok := r.dropFront(); ok {
		t.Fatal("dropFront on an empty ring reported a value")
	}
	for i := 0; i < 3; i++ {
		r.push(i, 4)
	}
	for i := 0; i < 3; i++ {
		v, ok := r.dropFront()
		if !ok || v != i {
			t.Fatalf("dropFront #%d = (%d, %v)", i, v, ok)
		}
	}
	if r.len() != 0 || r.head != 0 {
		t.Fatalf("after draining: len=%d head=%d", r.len(), r.head)
	}
	// Reuse after draining must still be FIFO.
	r.push(42, 4)
	if got := *r.at(0); got != 42 {
		t.Fatalf("reused ring at(0) = %d", got)
	}
}

// TestRingAtOutOfRange asserts at() fails loudly rather than returning a
// pointer into stale buffer memory.
func TestRingAtOutOfRange(t *testing.T) {
	var r ring[int]
	r.push(1, 4)
	for _, i := range []int{-1, 1, 99} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("at(%d) did not panic", i)
				}
			}()
			_ = r.at(i)
		}()
	}
}

// TestRingFilterInPlace covers removal from the front, middle, end, all and
// none — order must be preserved and the count exact.
func TestRingFilterInPlace(t *testing.T) {
	tests := []struct {
		name        string
		keep        func(int) bool
		want        []int
		wantRemoved int
	}{
		{"none removed", func(int) bool { return true }, []int{0, 1, 2, 3, 4}, 0},
		{"all removed", func(int) bool { return false }, []int{}, 5},
		{"front removed", func(v int) bool { return v > 1 }, []int{2, 3, 4}, 2},
		{"middle removed", func(v int) bool { return v != 2 }, []int{0, 1, 3, 4}, 1},
		{"tail removed", func(v int) bool { return v < 3 }, []int{0, 1, 2}, 2},
		{"alternating", func(v int) bool { return v%2 == 0 }, []int{0, 2, 4}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r ring[int]
			// Push past the cap first so head != 0 and the wrap path is exercised.
			for i := 0; i < 8; i++ {
				r.push(i-3, 5)
			}
			if got := contents(&r); !reflect.DeepEqual(got, []int{0, 1, 2, 3, 4}) {
				t.Fatalf("setup contents = %v", got)
			}
			removed := r.filterInPlace(func(v *int) bool { return tc.keep(*v) })
			if removed != tc.wantRemoved {
				t.Errorf("removed = %d, want %d", removed, tc.wantRemoved)
			}
			if got := contents(&r); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("contents = %v, want %v", got, tc.want)
			}
			// The ring must stay usable and FIFO afterwards.
			r.push(99, 5)
			if got := *r.at(r.len() - 1); got != 99 {
				t.Errorf("push after filter: newest = %d", got)
			}
		})
	}
}

// TestRingFilterEmptyIsNoop guards the early return.
func TestRingFilterEmptyIsNoop(t *testing.T) {
	var r ring[int]
	if n := r.filterInPlace(func(*int) bool { return false }); n != 0 {
		t.Fatalf("filter on empty ring removed %d", n)
	}
}

// TestRingReleasesReferences asserts evicted slots are zeroed so a bounded
// ring bounds retained memory, not just element count.
func TestRingReleasesReferences(t *testing.T) {
	var r ring[*int]
	for i := 0; i < 4; i++ {
		v := i
		r.push(&v, 2)
	}
	live := 0
	for _, p := range r.buf {
		if p != nil {
			live++
		}
	}
	if live != r.len() {
		t.Fatalf("%d non-nil slots for %d live elements: evicted pointers retained", live, r.len())
	}
	r.filterInPlace(func(**int) bool { return false })
	for i, p := range r.buf {
		if p != nil {
			t.Fatalf("slot %d retained a pointer after filtering everything out", i)
		}
	}
}
