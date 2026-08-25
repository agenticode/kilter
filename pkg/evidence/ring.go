package evidence

// ring is a bounded FIFO circular buffer. It grows lazily (doubling) up to
// the cap passed to push, so sparse subjects — the majority — pay only for
// what they hold. Index 0 is the oldest element. Not safe for concurrent
// use; Memory serializes access.
type ring[T any] struct {
	buf  []T
	head int // index of oldest element
	n    int
}

func (r *ring[T]) len() int { return r.n }

// at returns a pointer to the i-th element, 0 = oldest. Callers must not
// retain the pointer across mutations.
func (r *ring[T]) at(i int) *T {
	if i < 0 || i >= r.n {
		panic("evidence: ring index out of range")
	}
	return &r.buf[(r.head+i)%len(r.buf)]
}

// push appends v, evicting the oldest element when the ring already holds
// maxN elements. Returns the evicted element if any.
func (r *ring[T]) push(v T, maxN int) (evicted T, didEvict bool) {
	if maxN < 1 {
		maxN = 1
	}
	if r.n >= maxN {
		// Cap shrank between restores or the ring is full: evict down to
		// maxN-1 so the push lands within bounds.
		for r.n >= maxN {
			evicted, didEvict = r.dropFront()
		}
	}
	if r.n == len(r.buf) {
		r.grow(maxN)
	}
	r.buf[(r.head+r.n)%len(r.buf)] = v
	r.n++
	return evicted, didEvict
}

// dropFront removes and returns the oldest element.
func (r *ring[T]) dropFront() (T, bool) {
	var zero T
	if r.n == 0 {
		return zero, false
	}
	v := r.buf[r.head]
	r.buf[r.head] = zero // release references for GC
	r.head = (r.head + 1) % len(r.buf)
	r.n--
	if r.n == 0 {
		r.head = 0
	}
	return v, true
}

// grow doubles the backing array, capped at maxN.
func (r *ring[T]) grow(maxN int) {
	newCap := len(r.buf) * 2
	if newCap == 0 {
		newCap = 8
	}
	if newCap > maxN {
		newCap = maxN
	}
	if newCap <= len(r.buf) {
		return // already at cap; push guaranteed n < maxN
	}
	nb := make([]T, newCap)
	for i := 0; i < r.n; i++ {
		nb[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	r.buf = nb
	r.head = 0
}

// filterInPlace keeps only elements for which keep returns true, preserving
// order, and reports how many were removed. Used by age-based pruning,
// which may remove from the middle (event times are not strictly ordered
// in arrival order).
func (r *ring[T]) filterInPlace(keep func(*T) bool) int {
	if r.n == 0 {
		return 0
	}
	kept := make([]T, 0, r.n)
	for i := 0; i < r.n; i++ {
		v := r.at(i)
		if keep(v) {
			kept = append(kept, *v)
		}
	}
	removed := r.n - len(kept)
	if removed == 0 {
		return 0
	}
	for i := range r.buf {
		var zero T
		r.buf[i] = zero
	}
	copy(r.buf, kept)
	r.head = 0
	r.n = len(kept)
	return removed
}
