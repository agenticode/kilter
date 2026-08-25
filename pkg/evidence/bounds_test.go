package evidence

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// The scale target from the design: 50k containers. These tests assert the
// hard bound — the substrate must refuse to grow past its configured budget
// no matter how much is thrown at it — rather than merely trending small.

// TestEventBudgetBoundsUnderLoad drives far more events than the budget can
// hold, across far more subjects than the budget can hold, and asserts the
// byte budget is never exceeded at any point during ingest.
func TestEventBudgetBoundsUnderLoad(t *testing.T) {
	const (
		subjects   = 5000
		perSubj    = 40
		maxBytes   = 1 << 20 // 1 MiB: two orders of magnitude below the load
		perSubjCap = 8
	)
	m := newMem(t, Config{
		MaxEventsPerSubject: perSubjCap,
		MaxEventBytes:       maxBytes,
		DedupWindow:         time.Nanosecond,
	})
	for i := 0; i < subjects*perSubj; i++ {
		s := subj(fmt.Sprintf("ns/deploy-%05d/app", i%subjects))
		e := ev(s, EventDeploy, time.Duration(i)*time.Second)
		e.Attrs = map[string]string{"image": fmt.Sprintf("registry.example.com/team/app:v%d", i)}
		mustAppend(t, m, e)
		if got := m.events.bytes; got > maxBytes {
			t.Fatalf("append %d: event bytes %d exceeded budget %d", i, got, maxBytes)
		}
	}
	st := m.Stats()
	if st.EventBytes > maxBytes {
		t.Fatalf("EventBytes = %d, budget %d", st.EventBytes, maxBytes)
	}
	if st.EventBytes == 0 || st.Events == 0 {
		t.Fatal("budget enforcement emptied the store entirely")
	}
	// Every subject is capped, so the entry count is bounded twice over.
	if st.Events > st.EventSubjects*perSubjCap {
		t.Errorf("%d events across %d subjects exceeds the per-subject cap %d",
			st.Events, st.EventSubjects, perSubjCap)
	}
	if st.EvictedEventsBudget == 0 {
		t.Error("budget evictions were not counted")
	}
	checkMemory(t, m)
	t.Logf("held %d events / %d subjects in %d bytes (evicted %d by cap, %d by budget)",
		st.Events, st.EventSubjects, st.EventBytes, st.EvictedEventsCap, st.EvictedEventsBudget)
}

// TestSeriesBudgetBoundsUnderLoad asserts the same for tiered series, and
// that shedding is coldest-first: the subject written most recently must
// still hold history after the budget bites.
func TestSeriesBudgetBoundsUnderLoad(t *testing.T) {
	const (
		subjects = 2000
		hours    = 6
		maxBytes = 1 << 20
	)
	m := newMem(t, Config{MaxSeriesBytes: maxBytes, RawSampleCap: 64})
	for h := 0; h < hours; h++ {
		for i := 0; i < subjects; i++ {
			s := subj(fmt.Sprintf("ns/sts-%05d/app", i))
			d := time.Duration(h)*time.Hour + time.Duration(i%12)*5*time.Minute
			mustObserve(t, m, s, sample(d, int64(100+i%50), int64(1<<28)))
			if m.seriesBytes > maxBytes {
				t.Fatalf("h=%d i=%d: series bytes %d exceeded budget %d", h, i, m.seriesBytes, maxBytes)
			}
		}
	}
	st := m.Stats()
	if st.SeriesBytes > maxBytes {
		t.Fatalf("SeriesBytes = %d, budget %d", st.SeriesBytes, maxBytes)
	}
	if st.EvictedSeriesItems == 0 {
		t.Error("series evictions were not counted")
	}
	// The most recently written subject is the warmest and must have survived.
	last := subj(fmt.Sprintf("ns/sts-%05d/app", subjects-1))
	raw, _ := m.Digests(last, time.Time{}, time.Time{}, TierRaw)
	if len(raw) == 0 {
		t.Error("the warmest subject lost all history: eviction is not coldest-first")
	}
	checkMemory(t, m)
	t.Logf("held %d series (%d raw, %d hourly, %d daily) in %d bytes",
		st.SeriesSubjects, st.RawSamples, st.HourlyDigests, st.DailyDigests, st.SeriesBytes)
}

// TestTimelineAndDecisionBounds closes the remaining two stores.
func TestTimelineAndDecisionBounds(t *testing.T) {
	m := newMem(t, Config{
		TimelineCap:            50,
		MaxDecisionsPerSubject: 4,
		MaxDecisionBytes:       minBudgetBytes,
	})
	for i := 0; i < 500; i++ {
		if err := m.ObservePoint("c1", TimelinePoint{At: at(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.Stats().TimelinePoints; got != 50 {
		t.Errorf("timeline points = %d, cap is 50", got)
	}
	pts, _ := m.Timeline("c1", time.Time{}, time.Time{})
	if !pts[0].At.Equal(at(450 * time.Minute)) {
		t.Errorf("oldest surviving point = %v, want the newest 50", pts[0].At)
	}

	for i := 0; i < 2000; i++ {
		if err := m.RecordDecision(DecisionRecord{
			At: at(time.Duration(i) * time.Minute), Subject: subj(fmt.Sprintf("s%03d", i%200)),
			Kind: DecisionRecommendation, Summary: "sized cpu down",
			Payload: []byte(`{"cpu":100,"mem":1073741824}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	st := m.Stats()
	if st.DecisionBytes > minBudgetBytes {
		t.Errorf("DecisionBytes = %d over budget %d", st.DecisionBytes, minBudgetBytes)
	}
	for _, ref := range m.Subjects() {
		ds, _ := m.Decisions(ref, time.Time{}, time.Time{})
		if len(ds) > 4 {
			t.Errorf("subject %v holds %d decisions, cap is 4", ref, len(ds))
		}
	}
	checkMemory(t, m)
}

// TestScaleSoakHeapBounded is the memory assertion the design asks for: at
// 50k subjects the live heap must stay within a small multiple of the
// configured byte budgets, not within a small multiple of what was ingested.
func TestScaleSoakHeapBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("scale soak: skipped under -short")
	}
	const (
		subjects   = 50000
		rounds     = 4
		eventBytes = 8 << 20
		seriesCap  = 8 << 20
	)
	m := newMem(t, Config{
		MaxEventsPerSubject: 16,
		MaxEventBytes:       eventBytes,
		MaxSeriesBytes:      seriesCap,
		RawSampleCap:        32,
		DedupWindow:         time.Nanosecond,
	})
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for r := 0; r < rounds; r++ {
		for i := 0; i < subjects; i++ {
			s := subj(fmt.Sprintf("Deployment/ns-%03d/app-%05d/main", i%500, i))
			d := time.Duration(r)*time.Hour + time.Duration(i%12)*5*time.Minute
			mustAppend(t, m, ev(s, EventDeploy, d))
			mustObserve(t, m, s, sample(d, int64(100+i%40), 1<<28))
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	st := m.Stats()
	if st.EventBytes > eventBytes {
		t.Errorf("EventBytes = %d over budget %d", st.EventBytes, eventBytes)
	}
	if st.SeriesBytes > seriesCap {
		t.Errorf("SeriesBytes = %d over budget %d", st.SeriesBytes, seriesCap)
	}
	// Ingested payload here is ~200k events + ~200k samples; if the store
	// retained anything close to that, the heap would be hundreds of MiB.
	// Allow a generous 8x over the combined accounted budget for Go's
	// allocator overhead, map load factor and the test's own scratch.
	const ceiling = 8 * (eventBytes + seriesCap)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if grew > ceiling {
		t.Errorf("heap grew %d bytes for a %d-byte budget (ceiling %d)",
			grew, eventBytes+seriesCap, ceiling)
	}
	checkMemory(t, m)
	t.Logf("50k subjects x %d rounds: heap +%.1f MiB, accounted %.1f MiB (events %d, series %d)",
		rounds, float64(grew)/(1<<20), float64(st.EventBytes+st.SeriesBytes)/(1<<20),
		st.Events, st.SeriesSubjects)
}
