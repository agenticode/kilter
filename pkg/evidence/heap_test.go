package evidence

import (
	"math/rand"
	"sort"
	"testing"
)

type hitem struct {
	key int
	idx int
}

func newIntHeap() *minHeap[*hitem] {
	return &minHeap[*hitem]{
		less:   func(a, b *hitem) bool { return a.key < b.key },
		setIdx: func(v *hitem, i int) { v.idx = i },
	}
}

func assertHeap(t *testing.T, h *minHeap[*hitem]) {
	t.Helper()
	checkHeap(t, "int", h, func(v *hitem) int { return v.idx })
}

// TestHeapPopOrder asserts pop yields keys in ascending order for a range of
// insertion patterns, including duplicates.
func TestHeapPopOrder(t *testing.T) {
	tests := []struct {
		name string
		keys []int
	}{
		{"empty-ish", []int{1}},
		{"ascending", []int{1, 2, 3, 4, 5, 6, 7}},
		{"descending", []int{9, 8, 7, 6, 5, 4, 3, 2, 1}},
		{"duplicates", []int{3, 1, 3, 1, 2, 2, 2}},
		{"negatives", []int{-5, 3, -1, 0, -5, 7}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newIntHeap()
			for _, k := range tc.keys {
				h.push(&hitem{key: k})
				assertHeap(t, h)
			}
			want := append([]int(nil), tc.keys...)
			sort.Ints(want)
			var got []int
			for h.len() > 0 {
				if h.peek() != h.items[0] {
					t.Fatal("peek is not items[0]")
				}
				v := h.pop()
				if v.idx != -1 {
					t.Errorf("popped item still carries index %d", v.idx)
				}
				got = append(got, v.key)
				assertHeap(t, h)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("pop order = %v, want %v", got, want)
				}
			}
		})
	}
}

// TestHeapRemoveAt removes every position of a heap of each size and asserts
// the remaining multiset and heap property. removeAt is what subject
// eviction relies on, so an off-by-one here silently loses data.
func TestHeapRemoveAt(t *testing.T) {
	for n := 1; n <= 12; n++ {
		for victim := 0; victim < n; victim++ {
			h := newIntHeap()
			for i := 0; i < n; i++ {
				h.push(&hitem{key: (i * 7) % n})
			}
			gone := h.items[victim]
			h.removeAt(victim)
			if h.len() != n-1 {
				t.Fatalf("n=%d victim=%d: len = %d", n, victim, h.len())
			}
			if gone.idx != -1 {
				t.Errorf("n=%d victim=%d: removed item carries index %d", n, victim, gone.idx)
			}
			assertHeap(t, h)
			for _, it := range h.items {
				if it == gone {
					t.Fatalf("n=%d victim=%d: removed item still present", n, victim)
				}
			}
		}
	}
}

// TestHeapFixAfterKeyChange is the operation budget eviction depends on:
// a subject's front sequence changes and the heap must re-order in place.
func TestHeapFixAfterKeyChange(t *testing.T) {
	h := newIntHeap()
	items := make([]*hitem, 0, 16)
	for i := 0; i < 16; i++ {
		it := &hitem{key: i * 10}
		items = append(items, it)
		h.push(it)
	}
	// Raise the minimum: it must sink.
	min := h.peek()
	min.key = 1000
	h.fix(min.idx)
	assertHeap(t, h)
	if h.peek() == min {
		t.Fatal("raised key is still the minimum")
	}
	// Lower an arbitrary item below everything: it must surface.
	last := items[len(items)-1]
	last.key = -1
	h.fix(last.idx)
	assertHeap(t, h)
	if h.peek() != last {
		t.Fatal("lowered key did not become the minimum")
	}
}

// TestHeapRandomizedOps hammers the three mutators together and asserts the
// invariants after each step.
func TestHeapRandomizedOps(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	h := newIntHeap()
	live := map[*hitem]bool{}
	for step := 0; step < 4000; step++ {
		switch {
		case h.len() == 0 || rng.Intn(3) == 0:
			it := &hitem{key: rng.Intn(500)}
			h.push(it)
			live[it] = true
		case rng.Intn(2) == 0:
			i := rng.Intn(h.len())
			it := h.items[i]
			h.removeAt(i)
			delete(live, it)
		default:
			i := rng.Intn(h.len())
			h.items[i].key = rng.Intn(500)
			h.fix(i)
		}
		assertHeap(t, h)
		if h.len() != len(live) {
			t.Fatalf("step %d: heap len %d, live %d", step, h.len(), len(live))
		}
	}
	prev := -1
	for h.len() > 0 {
		v := h.pop()
		if v.key < prev {
			t.Fatalf("pop order broke: %d after %d", v.key, prev)
		}
		prev = v.key
	}
}
