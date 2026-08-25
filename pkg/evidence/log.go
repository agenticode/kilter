package evidence

import "sort"

// Accounted byte costs. These are deterministic approximations of the
// in-memory footprint (struct overhead + string bytes), used for the hard
// byte budgets. They are intentionally on the generous side: the budget
// must bound real memory, not flatter it.
const (
	eventFixedBytes      = 96
	attrPairBytes        = 32
	decisionFixedBytes   = 112
	subjectOverheadBytes = 256 // map entry + ring header + heap slot per subject
	sampleBytes          = 48
	digestBytes          = 136
	accSampleBytes       = 24 // one pending hour-accumulator sample (cpu+mem+throttle)
	pointBytes           = 64
)

func eventBytes(ev *EvidenceEvent) int64 {
	b := int64(eventFixedBytes)
	b += int64(len(ev.Kind) + len(ev.Severity) + len(ev.Dedup))
	b += int64(len(ev.Subject.Cluster) + len(ev.Subject.Kind) + len(ev.Subject.Key))
	for k, v := range ev.Attrs {
		b += int64(attrPairBytes + len(k) + len(v))
	}
	return b
}

func decisionBytes(d *DecisionRecord) int64 {
	b := int64(decisionFixedBytes)
	b += int64(len(d.Kind) + len(d.Summary) + len(d.Fingerprint) + len(d.Payload))
	b += int64(len(d.Subject.Cluster) + len(d.Subject.Kind) + len(d.Subject.Key))
	return b
}

// entry wraps a stored value with its arrival sequence (global, monotonic —
// the deterministic eviction order) and its accounted byte cost.
type entry[T any] struct {
	v   T
	seq uint64
	b   int64
}

// subjLog is one subject's bounded ring within a budgetedLog.
type subjLog[T any] struct {
	ref   SubjectRef
	ring  ring[entry[T]]
	bytes int64
	hIdx  int // index in the budgetedLog heap; -1 when detached
}

func (s *subjLog[T]) frontSeq() uint64 { return s.ring.at(0).seq }

// budgetedLog is a per-subject ring store under a global byte budget.
// Budget pressure evicts the globally oldest entry (smallest sequence
// number) first, via a min-heap over subjects keyed by their oldest entry.
type budgetedLog[T any] struct {
	subs  map[SubjectRef]*subjLog[T]
	heap  minHeap[*subjLog[T]]
	bytes int64 // accounted bytes incl. per-subject overhead
	count int   // total entries
}

func newBudgetedLog[T any]() *budgetedLog[T] {
	l := &budgetedLog[T]{subs: map[SubjectRef]*subjLog[T]{}}
	l.heap = minHeap[*subjLog[T]]{
		less:   func(a, b *subjLog[T]) bool { return a.frontSeq() < b.frontSeq() },
		setIdx: func(s *subjLog[T], i int) { s.hIdx = i },
	}
	return l
}

// getOrCreate returns the subject's log, creating (and charging) it.
func (l *budgetedLog[T]) getOrCreate(ref SubjectRef) *subjLog[T] {
	s := l.subs[ref]
	if s == nil {
		s = &subjLog[T]{ref: ref, hIdx: -1}
		l.subs[ref] = s
		l.bytes += subjectOverheadBytes
	}
	return s
}

// append stores v under ref, evicting the subject's oldest entries until the
// push fits the per-subject cap. Returns the number of entries evicted.
//
// Eviction is drained here rather than delegated to ring.push because push
// reports only its last eviction: a cap that shrank between appends (a
// checkpoint restored under a tightened config) drops many entries at once,
// and every one of them must be un-accounted or the byte budget leaks.
func (l *budgetedLog[T]) append(ref SubjectRef, v T, b int64, seq uint64, capPerSubject int) int {
	if capPerSubject < 1 {
		capPerSubject = 1
	}
	s := l.getOrCreate(ref)
	wasEmpty := s.ring.len() == 0
	evicted := 0
	for s.ring.len() >= capPerSubject {
		ev, ok := s.ring.dropFront()
		if !ok {
			break
		}
		s.bytes -= ev.b
		l.bytes -= ev.b
		l.count--
		evicted++
	}
	s.ring.push(entry[T]{v: v, seq: seq, b: b}, capPerSubject)
	s.bytes += b
	l.bytes += b
	l.count++
	if wasEmpty {
		l.heap.push(s)
	} else if evicted > 0 {
		l.heap.fix(s.hIdx) // front seq changed
	}
	return evicted
}

// enforceBudget evicts globally-oldest entries until bytes fit maxBytes.
// Returns the number of entries evicted.
func (l *budgetedLog[T]) enforceBudget(maxBytes int64) int {
	evicted := 0
	for l.bytes > maxBytes && l.heap.len() > 0 {
		s := l.heap.peek()
		e, _ := s.ring.dropFront()
		s.bytes -= e.b
		l.bytes -= e.b
		l.count--
		evicted++
		if s.ring.len() == 0 {
			l.dropSubject(s)
		} else {
			l.heap.fix(s.hIdx)
		}
	}
	return evicted
}

// dropSubject removes an empty subject and refunds its overhead.
func (l *budgetedLog[T]) dropSubject(s *subjLog[T]) {
	if s.hIdx >= 0 {
		l.heap.removeAt(s.hIdx)
	}
	delete(l.subs, s.ref)
	l.bytes -= subjectOverheadBytes
}

// filterAll applies keep to every entry of every subject (age pruning),
// removing empties, and returns how many entries were removed. Map
// iteration order does not matter: removal is order-independent and no
// output is produced here.
func (l *budgetedLog[T]) filterAll(keep func(ref SubjectRef, e *entry[T]) bool) int {
	removed := 0
	for _, s := range l.subs {
		frontBefore := uint64(0)
		if s.ring.len() > 0 {
			frontBefore = s.frontSeq()
		}
		n := s.ring.filterInPlace(func(e *entry[T]) bool {
			if keep(s.ref, e) {
				return true
			}
			s.bytes -= e.b
			l.bytes -= e.b
			return false
		})
		if n == 0 {
			continue
		}
		removed += n
		l.count -= n
		if s.ring.len() == 0 {
			l.dropSubject(s)
		} else if s.frontSeq() != frontBefore {
			l.heap.fix(s.hIdx)
		}
	}
	return removed
}

// sortedRefs returns every subject in the documented total order.
func (l *budgetedLog[T]) sortedRefs() []SubjectRef {
	refs := make([]SubjectRef, 0, len(l.subs))
	for ref := range l.subs {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].less(refs[j]) })
	return refs
}
