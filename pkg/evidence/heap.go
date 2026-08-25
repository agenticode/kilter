package evidence

// minHeap is a deterministic min-heap with index write-back, so holders can
// be fixed or removed in O(log n) when their key changes. Keys are derived
// from monotonic sequence numbers, which are unique — so heap order is a
// total order and eviction is deterministic.
type minHeap[T any] struct {
	items  []T
	less   func(a, b T) bool
	setIdx func(v T, i int)
}

func (h *minHeap[T]) len() int { return len(h.items) }

func (h *minHeap[T]) peek() T { return h.items[0] }

func (h *minHeap[T]) push(v T) {
	h.items = append(h.items, v)
	i := len(h.items) - 1
	h.setIdx(v, i)
	h.up(i)
}

func (h *minHeap[T]) pop() T {
	v := h.items[0]
	h.removeAt(0)
	return v
}

// removeAt deletes the element at index i.
func (h *minHeap[T]) removeAt(i int) {
	last := len(h.items) - 1
	h.setIdx(h.items[i], -1)
	if i != last {
		h.items[i] = h.items[last]
		h.setIdx(h.items[i], i)
	}
	var zero T
	h.items[last] = zero
	h.items = h.items[:last]
	if i != last {
		h.fix(i)
	}
}

// fix restores heap order after the key of the element at i changed.
func (h *minHeap[T]) fix(i int) {
	if !h.down(i) {
		h.up(i)
	}
}

func (h *minHeap[T]) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.setIdx(h.items[i], i)
	h.setIdx(h.items[j], j)
}

func (h *minHeap[T]) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !h.less(h.items[i], h.items[parent]) {
			break
		}
		h.swap(i, parent)
		i = parent
	}
}

// down sifts i toward the leaves; reports whether it moved.
func (h *minHeap[T]) down(i int) bool {
	start := i
	n := len(h.items)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		smallest := left
		if right := left + 1; right < n && h.less(h.items[right], h.items[left]) {
			smallest = right
		}
		if !h.less(h.items[smallest], h.items[i]) {
			break
		}
		h.swap(i, smallest)
		i = smallest
	}
	return i > start
}
