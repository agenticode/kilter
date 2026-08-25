package evidence

import (
	"math"
	"testing"
	"time"
)

func stats(p50, p95, p99, mx float64) DigestStats {
	return DigestStats{P50: p50, P95: p95, P99: p99, Max: mx}
}

// TestNearestRank pins the percentile convention: the smallest value with at
// least ceil(p*n) values at or below it, clamped to the slice.
func TestNearestRank(t *testing.T) {
	tests := []struct {
		name   string
		sorted []int64
		p      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single p50", []int64{7}, 0.50, 7},
		{"single p99", []int64{7}, 0.99, 7},
		{"two p50 takes lower", []int64{1, 9}, 0.50, 1},
		{"two p95 takes upper", []int64{1, 9}, 0.95, 9},
		{"ten p50", []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 0.50, 4},
		{"ten p95", []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 0.95, 9},
		{"p zero clamps low", []int64{3, 4, 5}, 0, 3},
		{"p one clamps high", []int64{3, 4, 5}, 1, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearestRank(tc.sorted, tc.p); got != tc.want {
				t.Errorf("nearestRank(%v, %v) = %v, want %v", tc.sorted, tc.p, got, tc.want)
			}
		})
	}
}

// TestStatsFromValues asserts the ordering invariant every stored digest
// claims, that the input is not mutated, and that Max is exact.
func TestStatsFromValues(t *testing.T) {
	tests := [][]int64{
		{},
		{5},
		{9, 1},
		{1, 1, 1, 1},
		{0, 0, 0, 100},
		{100, 50, 75, 25, 0},
		{maxSampleValue, 0, 1},
	}
	for _, vals := range tests {
		in := append([]int64(nil), vals...)
		got := statsFromValues(in)
		for i := range in {
			if in[i] != vals[i] {
				t.Fatalf("statsFromValues mutated its input: %v -> %v", vals, in)
			}
		}
		if len(vals) > 0 && !got.ordered() {
			t.Errorf("statsFromValues(%v) = %+v: ordering invariant violated", vals, got)
		}
		if len(vals) > 0 {
			max := vals[0]
			for _, v := range vals {
				if v > max {
					max = v
				}
			}
			if got.Max != float64(max) {
				t.Errorf("statsFromValues(%v).Max = %v, want %v", vals, got.Max, max)
			}
		}
	}
}

// TestDigestStatsOrdered is the guard validateDigest and the codec rely on.
func TestDigestStatsOrdered(t *testing.T) {
	tests := []struct {
		name string
		d    DigestStats
		want bool
	}{
		{"zero", DigestStats{}, true},
		{"ascending", stats(1, 2, 3, 4), true},
		{"flat", stats(5, 5, 5, 5), true},
		{"p50 above p95", stats(3, 2, 4, 5), false},
		{"max below p99", stats(1, 2, 3, 2), false},
		{"negative", stats(-1, 2, 3, 4), false},
		{"NaN", stats(1, math.NaN(), 3, 4), false},
		{"+Inf", stats(1, 2, 3, math.Inf(1)), false},
		{"-Inf", stats(math.Inf(-1), 2, 3, 4), false},
	}
	for _, tc := range tests {
		if got := tc.d.ordered(); got != tc.want {
			t.Errorf("%s: ordered() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDigestStatsMergePreservesOrder: element-wise max merging must preserve
// the ordering invariant for any pair of ordered inputs — that is the whole
// argument for using max rather than averaging percentiles.
func TestDigestStatsMergePreservesOrder(t *testing.T) {
	cases := []DigestStats{
		{}, stats(1, 2, 3, 4), stats(0, 0, 0, 9), stats(5, 5, 5, 5), stats(2, 100, 200, 1e9),
	}
	for _, a := range cases {
		for _, b := range cases {
			m := a
			m.merge(b)
			if !m.ordered() {
				t.Errorf("merge(%+v, %+v) = %+v: not ordered", a, b, m)
			}
			if m.Max < a.Max || m.Max < b.Max {
				t.Errorf("merge(%+v, %+v).Max = %v under-estimates", a, b, m.Max)
			}
		}
	}
}

func hourly(startHour int, cpu, mem DigestStats, samples int64) Digest {
	s := t0.Add(time.Duration(startHour) * time.Hour)
	return Digest{Start: s, End: s.Add(time.Hour), Tier: TierHourly, Samples: samples, CPU: cpu, Mem: mem}
}

// TestCanCoalesce pins the run-length coalescing predicate: adjacency,
// boringness and tolerance. Coalescing a window that contains restarts or
// OOMs would erase evidence, so those must never fold.
func TestCanCoalesce(t *testing.T) {
	base := hourly(0, stats(100, 110, 120, 130), stats(1e9, 1.1e9, 1.2e9, 1.3e9), 12)
	tests := []struct {
		name string
		mut  func(prev, next *Digest)
		want bool
	}{
		{"identical adjacent", func(p, n *Digest) {}, true},
		{"gap between windows", func(p, n *Digest) { n.Start = n.Start.Add(time.Hour); n.End = n.End.Add(time.Hour) }, false},
		{"overlapping windows", func(p, n *Digest) { n.Start = p.Start }, false},
		{"within tolerance", func(p, n *Digest) { n.CPU = stats(105, 115, 126, 137) }, true},
		{"cpu out of tolerance", func(p, n *Digest) { n.CPU = stats(100, 110, 120, 400) }, false},
		{"mem out of tolerance", func(p, n *Digest) { n.Mem = stats(1e9, 1.1e9, 1.2e9, 5e9) }, false},
		{"tiny cpu diff under floor", func(p, n *Digest) { p.CPU = stats(1, 2, 3, 4); n.CPU = stats(2, 3, 4, 5) }, true},
		{"tiny mem diff under floor", func(p, n *Digest) { p.Mem = stats(0, 0, 0, 1<<20); n.Mem = stats(0, 0, 0, 8<<20) }, true},
		{"prev has restarts", func(p, n *Digest) { p.Restarts = 1 }, false},
		{"next has restarts", func(p, n *Digest) { n.Restarts = 1 }, false},
		{"prev has ooms", func(p, n *Digest) { p.OOMs = 1 }, false},
		{"next has ooms", func(p, n *Digest) { n.OOMs = 1 }, false},
		{"throttle jump", func(p, n *Digest) { n.ThrottleRatio = 0.4 }, false},
		{"throttle wiggle", func(p, n *Digest) { n.ThrottleRatio = 0.02 }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev, next := base, hourly(1, base.CPU, base.Mem, 12)
			tc.mut(&prev, &next)
			if got := canCoalesce(&prev, &next, 0.10); got != tc.want {
				t.Errorf("canCoalesce = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFoldInto covers the merge arithmetic: window extension, conservative
// percentiles, additive counts, sample-weighted throttle mean, run length.
func TestFoldInto(t *testing.T) {
	prev := hourly(0, stats(100, 110, 120, 130), stats(10, 11, 12, 13), 10)
	prev.ThrottleRatio = 0.10
	prev.Restarts, prev.OOMs = 1, 2
	next := hourly(1, stats(90, 200, 210, 220), stats(20, 21, 22, 23), 30)
	next.ThrottleRatio = 0.50
	next.Restarts, next.OOMs = 3, 4
	next.Coalesced = 2

	got := prev
	foldInto(&got, &next)

	if !got.Start.Equal(prev.Start) || !got.End.Equal(next.End) {
		t.Errorf("window = [%v, %v), want [%v, %v)", got.Start, got.End, prev.Start, next.End)
	}
	if got.Samples != 40 {
		t.Errorf("Samples = %d, want 40", got.Samples)
	}
	// Sample-weighted mean: (0.10*10 + 0.50*30)/40 = 0.40.
	if math.Abs(got.ThrottleRatio-0.40) > 1e-12 {
		t.Errorf("ThrottleRatio = %v, want 0.40", got.ThrottleRatio)
	}
	if got.CPU != (DigestStats{P50: 100, P95: 200, P99: 210, Max: 220}) {
		t.Errorf("CPU = %+v, want element-wise max", got.CPU)
	}
	if got.Restarts != 4 || got.OOMs != 6 {
		t.Errorf("counts = (%d, %d), want (4, 6)", got.Restarts, got.OOMs)
	}
	// prev held 1 window, next held 3 (Coalesced=2): folded run length is 4,
	// i.e. Coalesced == 3.
	if got.Coalesced != 3 {
		t.Errorf("Coalesced = %d, want 3", got.Coalesced)
	}
	if !got.CPU.ordered() || !got.Mem.ordered() {
		t.Error("fold broke the stats ordering invariant")
	}
	if got.ThrottleRatio < 0 || got.ThrottleRatio > 1 {
		t.Errorf("ThrottleRatio %v escaped [0,1]", got.ThrottleRatio)
	}
}

// TestSanitizeSample is the garbage gate: what is rejected outright, what is
// clamped, and what passes through untouched.
func TestSanitizeSample(t *testing.T) {
	tests := []struct {
		name    string
		in      Sample
		wantErr bool
		want    Sample
	}{
		{"zero time rejected", Sample{MilliCPU: 1}, true, Sample{}},
		{"negative cpu rejected", Sample{At: t0, MilliCPU: -1}, true, Sample{}},
		{"negative mem rejected", Sample{At: t0, MemoryBytes: -1}, true, Sample{}},
		{"negative restarts rejected", Sample{At: t0, Restarts: -1}, true, Sample{}},
		{"negative ooms rejected", Sample{At: t0, OOMs: -1}, true, Sample{}},
		{"absurd cpu rejected", Sample{At: t0, MilliCPU: maxSampleValue + 1}, true, Sample{}},
		{"absurd mem rejected", Sample{At: t0, MemoryBytes: maxSampleValue + 1}, true, Sample{}},
		{"NaN throttle rejected", Sample{At: t0, ThrottleRatio: math.NaN()}, true, Sample{}},
		{"+Inf throttle rejected", Sample{At: t0, ThrottleRatio: math.Inf(1)}, true, Sample{}},
		{"-Inf throttle rejected", Sample{At: t0, ThrottleRatio: math.Inf(-1)}, true, Sample{}},
		{"cap value accepted", Sample{At: t0, MilliCPU: maxSampleValue}, false,
			Sample{At: t0, MilliCPU: maxSampleValue}},
		{"throttle clamped low", Sample{At: t0, ThrottleRatio: -0.5}, false, Sample{At: t0}},
		{"throttle clamped high", Sample{At: t0, ThrottleRatio: 3}, false,
			Sample{At: t0, ThrottleRatio: 1}},
		{"clean sample", Sample{At: t0, MilliCPU: 5, MemoryBytes: 9, ThrottleRatio: 0.5}, false,
			Sample{At: t0, MilliCPU: 5, MemoryBytes: 9, ThrottleRatio: 0.5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeSample(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if err := validateSample(got); err != nil {
				t.Errorf("sanitized sample fails validateSample: %v", err)
			}
		})
	}
}

// TestSanitizeSampleNormalizesZone: a non-UTC timestamp must be normalized,
// and validateSample must reject the un-normalized form.
func TestSanitizeSampleNormalizesZone(t *testing.T) {
	zone := time.FixedZone("KST", 9*3600)
	in := Sample{At: t0.In(zone), MilliCPU: 1}
	if err := validateSample(in); err == nil {
		t.Error("validateSample accepted a non-UTC timestamp")
	}
	got, err := sanitizeSample(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.At.Location() != time.UTC || !got.At.Equal(t0) {
		t.Errorf("At = %v (%v), want %v UTC", got.At, got.At.Location(), t0)
	}
	if err := validateSample(got); err != nil {
		t.Errorf("normalized sample rejected: %v", err)
	}
}

// TestIngestRollsHours drives a series across hour and day boundaries and
// asserts the tier structure plus exact byte accounting. Accounting drift
// here is invisible until the byte budget silently stops bounding memory.
func TestIngestRollsHours(t *testing.T) {
	cfg := DefaultConfig().withDefaults()
	cfg.RawSampleCap = 1000
	st := &Stats{}
	sr := &series{ref: subj("a"), hIdx: -1}
	var seq uint64
	total := int64(0)

	// 50 hours of one sample per 5 minutes, alternating slightly so hours
	// stay boring and coalesce.
	for i := 0; i < 50*12; i++ {
		seq++
		smp := Sample{At: at(time.Duration(i) * 5 * time.Minute), MilliCPU: 100, MemoryBytes: 1 << 30}
		total += sr.ingest(smp, seq, &cfg, st)
		if got := seriesBytesOf(sr); got != sr.bytes {
			t.Fatalf("sample %d: series accounted %d bytes, contents recompute to %d",
				i, sr.bytes, got)
		}
	}
	if total != sr.bytes {
		t.Errorf("sum of ingest deltas = %d, series bytes = %d", total, sr.bytes)
	}
	if sr.raw.len() != 600 {
		t.Errorf("raw samples = %d, want 600", sr.raw.len())
	}
	// 49 hours finalized (hour 50 still pending) and every one boring, so
	// they run-length coalesce into a single hourly digest.
	if sr.hourly.len() != 1 {
		t.Errorf("hourly digests = %d, want 1 (all boring)", sr.hourly.len())
	}
	if st.CoalescedHourly != 48 {
		t.Errorf("CoalescedHourly = %d, want 48", st.CoalescedHourly)
	}
	// Days 0 and 1 are both flushed, adjacent and boring, so they too
	// run-length coalesce; day 2 is still accumulating.
	if sr.daily.len() != 1 {
		t.Errorf("daily digests = %d, want 1 (days 0 and 1 coalesced)", sr.daily.len())
	}
	if st.CoalescedDaily != 1 {
		t.Errorf("CoalescedDaily = %d, want 1", st.CoalescedDaily)
	}
	if d := sr.daily.at(0); !d.Start.Equal(t0) || !d.End.Equal(t0.Add(48*time.Hour)) || d.Coalesced != 1 {
		t.Errorf("daily digest = [%v, %v) coalesced=%d, want the two-day run", d.Start, d.End, d.Coalesced)
	}
	if sr.day == nil {
		t.Error("day 2 rollup is not accumulating")
	}
	h := sr.hourly.at(0)
	if !h.Start.Equal(t0) || !h.End.Equal(t0.Add(49*time.Hour)) {
		t.Errorf("coalesced hourly window = [%v, %v)", h.Start, h.End)
	}
	if h.Samples != 49*12 {
		t.Errorf("coalesced hourly Samples = %d, want %d", h.Samples, 49*12)
	}
	checkSeriesShape(t, sr, cfg)
}

// TestIngestPerHourSampleCap: a runaway collector must not grow the pending
// hour without bound; overflow is counted, never stored.
func TestIngestPerHourSampleCap(t *testing.T) {
	cfg := DefaultConfig().withDefaults()
	cfg.MaxSamplesPerHour = 10
	st := &Stats{}
	sr := &series{ref: subj("a"), hIdx: -1}
	var seq uint64
	for i := 0; i < 100; i++ {
		seq++
		sr.ingest(Sample{At: at(time.Duration(i) * time.Second), MilliCPU: int64(i)}, seq, &cfg, st)
	}
	if len(sr.acc.cpu) != 10 {
		t.Errorf("accumulator holds %d samples, cap is 10", len(sr.acc.cpu))
	}
	if st.DroppedSamples != 90 {
		t.Errorf("DroppedSamples = %d, want 90", st.DroppedSamples)
	}
	if sr.acc.dropped != 90 {
		t.Errorf("acc.dropped = %d, want 90", sr.acc.dropped)
	}
	if got := seriesBytesOf(sr); got != sr.bytes {
		t.Errorf("accounted %d bytes, recomputes to %d", sr.bytes, got)
	}
	// The finalized digest must report every sample the hour saw, while its
	// percentiles cover only the retained ones.
	d := sr.acc.finalize()
	if d.Samples != 100 {
		t.Errorf("digest Samples = %d, want 100 (kept + dropped)", d.Samples)
	}
	if d.CPU.Max != 9 {
		t.Errorf("digest CPU.Max = %v, want 9 (first 10 samples retained)", d.CPU.Max)
	}
}

// TestIngestInterestingHoursDoNotCoalesce: a restart or a load change is
// evidence and must survive as its own digest.
func TestIngestInterestingHoursDoNotCoalesce(t *testing.T) {
	cfg := DefaultConfig().withDefaults()
	st := &Stats{}
	sr := &series{ref: subj("a"), hIdx: -1}
	var seq uint64
	for hour := 0; hour < 6; hour++ {
		for i := 0; i < 12; i++ {
			seq++
			smp := Sample{
				At:          at(time.Duration(hour)*time.Hour + time.Duration(i)*5*time.Minute),
				MilliCPU:    100,
				MemoryBytes: 1 << 30,
			}
			if hour == 3 && i == 0 {
				smp.Restarts = 1
				smp.OOMs = 1
			}
			sr.ingest(smp, seq, &cfg, st)
		}
	}
	// Hours 0-2 coalesce; hour 3 carries the restart and stands alone;
	// hours 4 (and the pending 5) start a fresh run after it.
	if sr.hourly.len() != 3 {
		t.Fatalf("hourly digests = %d, want 3", sr.hourly.len())
	}
	if got := sr.hourly.at(0).Coalesced; got != 2 {
		t.Errorf("first digest Coalesced = %d, want 2", got)
	}
	d := sr.hourly.at(1)
	if d.Restarts != 1 || d.OOMs != 1 || d.Coalesced != 0 {
		t.Errorf("restart hour digest = %+v, want an uncoalesced restart/oom window", d)
	}
	checkSeriesShape(t, sr, cfg)
}

// TestEvictOneOrder: shedding prefers the cheapest-to-lose tier first (raw
// samples), and reports emptiness only when nothing is left.
func TestEvictOneOrder(t *testing.T) {
	cfg := DefaultConfig().withDefaults()
	st := &Stats{}
	sr := &series{ref: subj("a"), hIdx: -1}
	var seq uint64
	for i := 0; i < 30*12; i++ {
		seq++
		sr.ingest(Sample{At: at(time.Duration(i) * 5 * time.Minute), MilliCPU: int64(i % 7)}, seq, &cfg, st)
	}
	wantRaw, wantHourly, wantDaily := sr.raw.len(), sr.hourly.len(), sr.daily.len()
	if wantRaw == 0 || wantHourly == 0 || wantDaily == 0 {
		t.Fatalf("setup: raw=%d hourly=%d daily=%d", wantRaw, wantHourly, wantDaily)
	}
	// Raw first.
	if _, empty := sr.evictOne(st); empty {
		t.Fatal("series reported empty with data left")
	}
	if sr.raw.len() != wantRaw-1 || sr.hourly.len() != wantHourly {
		t.Errorf("first eviction did not take a raw sample")
	}
	// Drain raw, then hourly, then daily, then the accumulators.
	steps := 0
	for {
		_, empty := sr.evictOne(st)
		steps++
		if got := seriesBytesOf(sr); got != sr.bytes {
			t.Fatalf("step %d: accounted %d bytes, recomputes to %d", steps, sr.bytes, got)
		}
		if empty {
			break
		}
		if steps > 10000 {
			t.Fatal("evictOne never reported empty")
		}
	}
	if sr.bytes != 0 {
		t.Errorf("drained series still accounts %d bytes", sr.bytes)
	}
	if delta, empty := sr.evictOne(st); delta != 0 || !empty {
		t.Errorf("evictOne on an empty series = (%d, %v)", delta, empty)
	}
}

// TestValidateDigest covers the checkpoint-restore gate.
func TestValidateDigest(t *testing.T) {
	good := hourly(0, stats(1, 2, 3, 4), stats(5, 6, 7, 8), 12)
	tests := []struct {
		name string
		mut  func(*Digest)
		tier int
		ok   bool
	}{
		{"valid", func(*Digest) {}, TierHourly, true},
		{"wrong tier", func(*Digest) {}, TierDaily, false},
		{"zero start", func(d *Digest) { d.Start = time.Time{} }, TierHourly, false},
		{"non-utc", func(d *Digest) { d.Start = d.Start.In(time.FixedZone("x", 3600)) }, TierHourly, false},
		{"end before start", func(d *Digest) { d.End = d.Start.Add(-time.Hour) }, TierHourly, false},
		{"empty window", func(d *Digest) { d.End = d.Start }, TierHourly, false},
		{"zero samples", func(d *Digest) { d.Samples = 0 }, TierHourly, false},
		{"negative restarts", func(d *Digest) { d.Restarts = -1 }, TierHourly, false},
		{"negative coalesced", func(d *Digest) { d.Coalesced = -1 }, TierHourly, false},
		{"unordered stats", func(d *Digest) { d.CPU = stats(9, 2, 3, 4) }, TierHourly, false},
		{"NaN throttle", func(d *Digest) { d.ThrottleRatio = math.NaN() }, TierHourly, false},
		{"throttle above one", func(d *Digest) { d.ThrottleRatio = 1.5 }, TierHourly, false},
		{"throttle at one", func(d *Digest) { d.ThrottleRatio = 1 }, TierHourly, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := good
			tc.mut(&d)
			err := validateDigest(d, tc.tier)
			if (err == nil) != tc.ok {
				t.Errorf("validateDigest = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
