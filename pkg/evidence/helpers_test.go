package evidence

import (
	"fmt"
	"math"
	"testing"
	"time"
)

// t0 is the fixed clock origin every test builds times from. No test calls
// time.Now(): the substrate is clock-injected by construction and the tests
// hold it to that.
var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func subj(key string) SubjectRef {
	return SubjectRef{Cluster: "c1", Kind: SubjectContainer, Key: key}
}

func newMem(t *testing.T, cfg Config) *Memory {
	t.Helper()
	m, err := NewMemory(cfg)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Invariant checkers. Every test that mutates a store calls checkMemory at
// the end: these are the documented invariants of the package, so a bug in
// any code path shows up as a failing invariant rather than a wrong number
// three units downstream.
// ---------------------------------------------------------------------------

// checkLog verifies the byte/count bookkeeping of a budgetedLog against a
// recomputed ground truth, plus the heap's structural invariants.
func checkLog[T any](t *testing.T, name string, l *budgetedLog[T], byteOf func(*T) int64) {
	t.Helper()
	wantBytes := int64(0)
	wantCount := 0
	for ref, s := range l.subs {
		if s.ref != ref {
			t.Errorf("%s: subs[%v].ref = %v", name, ref, s.ref)
		}
		if s.ring.len() == 0 {
			t.Errorf("%s: subject %v retained with an empty ring", name, ref)
		}
		subBytes := int64(0)
		for i := 0; i < s.ring.len(); i++ {
			e := s.ring.at(i)
			if got := byteOf(&e.v); got != e.b {
				t.Errorf("%s: subject %v entry %d accounted %d bytes, recomputes to %d",
					name, ref, i, e.b, got)
			}
			subBytes += e.b
			if i > 0 && s.ring.at(i-1).seq >= e.seq {
				t.Errorf("%s: subject %v seq not strictly increasing at %d", name, ref, i)
			}
		}
		if s.bytes != subBytes {
			t.Errorf("%s: subject %v bytes = %d, want %d", name, ref, s.bytes, subBytes)
		}
		wantBytes += subBytes + subjectOverheadBytes
		wantCount += s.ring.len()
	}
	if l.bytes != wantBytes {
		t.Errorf("%s: log bytes = %d, want %d", name, l.bytes, wantBytes)
	}
	if l.count != wantCount {
		t.Errorf("%s: log count = %d, want %d", name, l.count, wantCount)
	}
	if l.heap.len() != len(l.subs) {
		t.Errorf("%s: heap holds %d subjects, map holds %d", name, l.heap.len(), len(l.subs))
	}
	checkHeap(t, name, &l.heap, func(s *subjLog[T]) int { return s.hIdx })
}

// checkHeap verifies the min-heap property and index write-back.
func checkHeap[T any](t *testing.T, name string, h *minHeap[T], idxOf func(T) int) {
	t.Helper()
	for i, v := range h.items {
		if got := idxOf(v); got != i {
			t.Errorf("%s: heap item %d carries index %d", name, i, got)
		}
		if i > 0 {
			parent := (i - 1) / 2
			if h.less(h.items[i], h.items[parent]) {
				t.Errorf("%s: heap property violated at %d", name, i)
			}
		}
	}
}

// seriesBytesOf recomputes a series' accounted bytes from its contents.
func seriesBytesOf(sr *series) int64 {
	b := int64(sr.raw.len()) * sampleBytes
	b += int64(sr.hourly.len()) * digestBytes
	b += int64(sr.daily.len()) * digestBytes
	if sr.acc != nil {
		b += sr.acc.bytes()
	}
	if sr.day != nil {
		b += digestBytes
	}
	return b
}

// checkMemory asserts every documented structural invariant of a *Memory.
func checkMemory(t *testing.T, m *Memory) {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()

	checkLog(t, "events", m.events, func(ev *EvidenceEvent) int64 { return eventBytes(ev) })
	checkLog(t, "decisions", m.decisions, func(d *DecisionRecord) int64 { return decisionBytes(d) })

	wantSeries := int64(0)
	for ref, sr := range m.series {
		if sr.ref != ref {
			t.Errorf("series[%v].ref = %v", ref, sr.ref)
		}
		if got := seriesBytesOf(sr); got != sr.bytes {
			t.Errorf("series %v accounted %d bytes, recomputes to %d", ref, sr.bytes, got)
		}
		wantSeries += sr.bytes + subjectOverheadBytes
		checkSeriesShape(t, sr, m.cfg)
	}
	if m.seriesBytes != wantSeries {
		t.Errorf("seriesBytes = %d, want %d", m.seriesBytes, wantSeries)
	}
	if m.seriesHeap.len() != len(m.series) {
		t.Errorf("series heap holds %d, map holds %d", m.seriesHeap.len(), len(m.series))
	}
	checkHeap(t, "series", &m.seriesHeap, func(s *series) int { return s.hIdx })

	for cluster, tl := range m.timelines {
		if tl.points.len() == 0 {
			t.Errorf("timeline %q retained with no points", cluster)
		}
		for i := 1; i < tl.points.len(); i++ {
			if tl.points.at(i).At.Before(tl.points.at(i - 1).At) {
				t.Errorf("timeline %q points out of order at %d", cluster, i)
			}
		}
	}
}

// checkSeriesShape asserts the per-series ordering and cap invariants.
func checkSeriesShape(t *testing.T, sr *series, cfg Config) {
	t.Helper()
	if sr.raw.len() > cfg.RawSampleCap {
		t.Errorf("series %v: %d raw samples over cap %d", sr.ref, sr.raw.len(), cfg.RawSampleCap)
	}
	if sr.hourly.len() > cfg.HourlyCap {
		t.Errorf("series %v: %d hourly digests over cap %d", sr.ref, sr.hourly.len(), cfg.HourlyCap)
	}
	if sr.daily.len() > cfg.DailyCap {
		t.Errorf("series %v: %d daily digests over cap %d", sr.ref, sr.daily.len(), cfg.DailyCap)
	}
	for i := 1; i < sr.raw.len(); i++ {
		if sr.raw.at(i).At.Before(sr.raw.at(i - 1).At) {
			t.Errorf("series %v: raw samples out of time order at %d", sr.ref, i)
		}
	}
	for _, tier := range []struct {
		name string
		r    *ring[Digest]
		want int
	}{{"hourly", &sr.hourly, TierHourly}, {"daily", &sr.daily, TierDaily}} {
		for i := 0; i < tier.r.len(); i++ {
			d := tier.r.at(i)
			if err := validateDigest(*d, tier.want); err != nil {
				t.Errorf("series %v: stored %s digest %d invalid: %v", sr.ref, tier.name, i, err)
			}
			if i > 0 && d.Start.Before(tier.r.at(i-1).End) {
				t.Errorf("series %v: %s digests %d and %d overlap", sr.ref, tier.name, i-1, i)
			}
		}
	}
	if sr.acc != nil && len(sr.acc.cpu) > cfg.MaxSamplesPerHour {
		t.Errorf("series %v: hour accumulator holds %d samples over cap %d",
			sr.ref, len(sr.acc.cpu), cfg.MaxSamplesPerHour)
	}
}

// ev builds a minimal valid event.
func ev(s SubjectRef, kind string, d time.Duration) EvidenceEvent {
	return EvidenceEvent{At: at(d), Kind: kind, Subject: s, Severity: SeverityInfo}
}

func mustAppend(t *testing.T, m *Memory, e EvidenceEvent) {
	t.Helper()
	if err := m.Append(e); err != nil {
		t.Fatalf("Append(%v): %v", e.Kind, err)
	}
}

func mustObserve(t *testing.T, m *Memory, s SubjectRef, smp Sample) {
	t.Helper()
	if err := m.ObserveSample(s, smp); err != nil {
		t.Fatalf("ObserveSample(%v, %v): %v", s, smp.At, err)
	}
}

func sample(d time.Duration, cpu, mem int64) Sample {
	return Sample{At: at(d), MilliCPU: cpu, MemoryBytes: mem}
}

var _ = fmt.Sprintf

func nan() float64 { return math.NaN() }
func inf() float64 { return math.Inf(1) }
