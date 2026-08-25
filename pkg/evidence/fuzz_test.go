package evidence

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzCleanString: for any input and any cap, the output must be valid
// UTF-8, free of control characters, within the byte cap, idempotent, and
// accepted by its own validator. Everything stored in the substrate passes
// through here, so a hole is a hole in every store at once.
func FuzzCleanString(f *testing.F) {
	for _, s := range []string{"", "plain", "a\nb", "héllo→", "\xff\xfe", "\x00\x1b[31m", strings.Repeat("x", 300)} {
		f.Add(s, 64)
	}
	f.Fuzz(func(t *testing.T, s string, max int) {
		if max < 0 || max > 1<<16 {
			t.Skip()
		}
		got := cleanString(s, max)
		if len(got) > max {
			t.Fatalf("cleanString(%q, %d) = %q: %d bytes over cap", s, max, got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("cleanString(%q, %d) produced invalid UTF-8 %q", s, max, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("cleanString(%q, %d) kept control character %U", s, max, r)
			}
		}
		if again := cleanString(got, max); again != got {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, got, again)
		}
		if !cleanStringOK(got, max) {
			t.Fatalf("cleanStringOK rejected cleanString output %q", got)
		}
	})
}

// digestFrom builds a digest from fuzz bytes, keeping it inside the domain
// stored digests actually occupy (ordered, finite, non-negative).
func digestFrom(startSec int64, spanSec int32, a, b, c, d uint32, samples uint16, thr uint16, restarts, ooms uint16) Digest {
	vals := []float64{float64(a), float64(b), float64(c), float64(d)}
	for i := 0; i < len(vals); i++ {
		for j := i + 1; j < len(vals); j++ {
			if vals[j] < vals[i] {
				vals[i], vals[j] = vals[j], vals[i]
			}
		}
	}
	st := time.Unix(startSec%(1<<40), 0).UTC()
	span := time.Duration(spanSec%86400)*time.Second + time.Second
	return Digest{
		Start: st, End: st.Add(span), Tier: TierHourly,
		Samples:       int64(samples) + 1,
		CPU:           DigestStats{P50: vals[0], P95: vals[1], P99: vals[2], Max: vals[3]},
		Mem:           DigestStats{P50: vals[0], P95: vals[1], P99: vals[2], Max: vals[3]},
		ThrottleRatio: float64(thr) / float64(math.MaxUint16),
		Restarts:      int64(restarts),
		OOMs:          int64(ooms),
	}
}

// FuzzDigestFold is the digest-merge fuzz target the design calls for.
// foldInto is the only way stored history is compressed, so it must never
// produce a digest that violates the invariants validateDigest enforces:
// ordered finite percentiles, a throttle ratio in [0,1], non-decreasing
// counts, and a window that only ever grows.
func FuzzDigestFold(f *testing.F) {
	f.Add(int64(0), int32(3600), uint32(1), uint32(2), uint32(3), uint32(4), uint16(10), uint16(0), uint16(0), uint16(0),
		int64(3600), int32(3600), uint32(5), uint32(6), uint32(7), uint32(8), uint16(20), uint16(65535), uint16(1), uint16(2))
	f.Fuzz(func(t *testing.T,
		s1 int64, sp1 int32, a1, b1, c1, d1 uint32, n1, th1, r1, o1 uint16,
		s2 int64, sp2 int32, a2, b2, c2, d2 uint32, n2, th2, r2, o2 uint16) {

		prev := digestFrom(s1, sp1, a1, b1, c1, d1, n1, th1, r1, o1)
		next := digestFrom(s2, sp2, a2, b2, c2, d2, n2, th2, r2, o2)
		if err := validateDigest(prev, TierHourly); err != nil {
			t.Skip()
		}
		if err := validateDigest(next, TierHourly); err != nil {
			t.Skip()
		}
		before := prev
		foldInto(&prev, &next)

		if !prev.CPU.ordered() || !prev.Mem.ordered() {
			t.Fatalf("fold broke stats ordering: %+v", prev)
		}
		if math.IsNaN(prev.ThrottleRatio) || prev.ThrottleRatio < 0 || prev.ThrottleRatio > 1 {
			t.Fatalf("fold produced throttle ratio %v", prev.ThrottleRatio)
		}
		// The merged throttle mean must lie between the two inputs: it is a
		// convex combination, never an extrapolation.
		lo := math.Min(before.ThrottleRatio, next.ThrottleRatio)
		hi := math.Max(before.ThrottleRatio, next.ThrottleRatio)
		if prev.ThrottleRatio < lo-1e-9 || prev.ThrottleRatio > hi+1e-9 {
			t.Fatalf("throttle mean %v outside [%v, %v]", prev.ThrottleRatio, lo, hi)
		}
		if prev.Samples != before.Samples+next.Samples {
			t.Fatalf("Samples = %d, want %d", prev.Samples, before.Samples+next.Samples)
		}
		if prev.Restarts < before.Restarts || prev.OOMs < before.OOMs {
			t.Fatalf("fold lost counts: %+v", prev)
		}
		// Percentiles are conservative: never below either input.
		if prev.CPU.Max < before.CPU.Max || prev.CPU.Max < next.CPU.Max ||
			prev.CPU.P50 < before.CPU.P50 || prev.CPU.P50 < next.CPU.P50 {
			t.Fatalf("fold under-estimated: %+v from %+v and %+v", prev.CPU, before.CPU, next.CPU)
		}
		if !prev.Start.Equal(before.Start) || !prev.End.Equal(next.End) {
			t.Fatalf("window = [%v, %v), want [%v, %v)", prev.Start, prev.End, before.Start, next.End)
		}
		if prev.Coalesced <= before.Coalesced {
			t.Fatalf("run length did not grow: %d -> %d", before.Coalesced, prev.Coalesced)
		}
	})
}

// FuzzSanitizeEvent: whatever a collector hands the substrate, sanitizeEvent
// must either reject it or produce an event its own validator accepts and
// that re-sanitizes to itself. That fixed-point property is what lets the
// codec restore checkpoints byte-exactly.
func FuzzSanitizeEvent(f *testing.F) {
	f.Add("deploy", "c1", "container", "k", "warning", "dedup", "attrK", "attrV", int64(0))
	f.Add("\x00\x1b", "\xff", "", "", "nope", strings.Repeat("d", 500), "", "", int64(-1))
	f.Fuzz(func(t *testing.T, kind, cluster, sKind, sKey, sev, dedup, ak, av string, nanos int64) {
		in := EvidenceEvent{
			At:       time.Unix(0, nanos%(1<<62)),
			Kind:     kind,
			Subject:  SubjectRef{Cluster: cluster, Kind: sKind, Key: sKey},
			Severity: sev,
			Dedup:    dedup,
			Attrs:    map[string]string{ak: av},
		}
		got, err := sanitizeEvent(in)
		if err != nil {
			return
		}
		if err := validateEvent(got); err != nil {
			t.Fatalf("sanitizeEvent output rejected by validateEvent: %v (%+v)", err, got)
		}
		again, err := sanitizeEvent(got)
		if err != nil {
			t.Fatalf("re-sanitizing a sanitized event failed: %v", err)
		}
		if !eventsEqual(got, again) {
			t.Fatalf("sanitize is not a fixed point:\n %+v\n %+v", got, again)
		}
		if eventBytes(&got) <= 0 {
			t.Fatalf("non-positive accounted bytes for %+v", got)
		}
	})
}

// FuzzMemoryIngest drives the whole store with an arbitrary opcode stream
// and asserts the structural invariants survive any interleaving. Budgets
// are set tiny so eviction, coalescing and pruning all fire constantly.
func FuzzMemoryIngest(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	f.Add([]byte{3, 200, 3, 100, 1, 50, 4, 255})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 4096 {
			t.Skip()
		}
		m, err := NewMemory(Config{
			MaxEventsPerSubject:    3,
			MaxEventBytes:          minBudgetBytes,
			MaxSeriesBytes:         minBudgetBytes,
			MaxDecisionBytes:       minBudgetBytes,
			MaxDecisionsPerSubject: 2,
			RawSampleCap:           4,
			MaxSamplesPerHour:      3,
			HourlyCap:              3,
			DailyCap:               2,
			TimelineCap:            4,
			MaxTimelineClusters:    2,
		})
		if err != nil {
			t.Fatal(err)
		}
		clock := t0
		for i := 0; i+1 < len(ops); i += 2 {
			op, arg := ops[i], ops[i+1]
			s := subj(string(rune('a' + arg%4)))
			clock = clock.Add(time.Duration(arg%37) * 7 * time.Minute)
			switch op % 6 {
			case 0:
				e := EvidenceEvent{At: clock, Kind: EventDeploy, Subject: s}
				if arg%3 == 0 {
					e.Dedup = "d"
					e.Severity = SeverityCritical
				}
				_ = m.Append(e)
			case 1:
				_ = m.ObserveSample(s, Sample{At: clock, MilliCPU: int64(arg), MemoryBytes: int64(arg) << 20})
			case 2:
				_ = m.RecordDecision(DecisionRecord{At: clock, Subject: s, Kind: DecisionAction, Summary: "x"})
			case 3:
				_ = m.ObservePoint(string(rune('x'+arg%3)), TimelinePoint{At: clock, Nodes: int(arg)})
			case 4:
				_, _ = m.Prune(clock)
			case 5:
				_, _ = m.Events(s, time.Time{}, clock)
				_, _ = m.Digests(s, time.Time{}, time.Time{}, int(arg%3))
				_, _ = m.Timeline("x", time.Time{}, time.Time{})
				_ = m.Subjects()
			}
		}
		checkMemory(t, m)
		st := m.Stats()
		if st.EventBytes > minBudgetBytes || st.SeriesBytes > minBudgetBytes || st.DecisionBytes > minBudgetBytes {
			t.Fatalf("a budget was exceeded: %+v", st)
		}
	})
}

// eventsEqual compares two events including their attrs map.
func eventsEqual(a, b EvidenceEvent) bool {
	if !a.At.Equal(b.At) || a.Kind != b.Kind || a.Subject != b.Subject ||
		a.Severity != b.Severity || a.Dedup != b.Dedup || a.Count != b.Count ||
		len(a.Attrs) != len(b.Attrs) {
		return false
	}
	for k, v := range a.Attrs {
		if b.Attrs[k] != v {
			return false
		}
	}
	return true
}
