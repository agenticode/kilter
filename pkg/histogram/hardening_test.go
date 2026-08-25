package histogram

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// sumWeights returns the raw sum of the weights slice; the histogram must
// keep total consistent with it (within float accumulation noise) or
// percentile thresholds are computed against phantom weight.
func sumWeights(h *Histogram) float64 {
	s := 0.0
	for _, w := range h.weights {
		s += w
	}
	return s
}

func checkTotalConsistent(t *testing.T, h *Histogram) {
	t.Helper()
	s := sumWeights(h)
	if math.Abs(s-h.total) > 1e-9*math.Max(s, h.total)+1e-300 {
		t.Fatalf("total=%v inconsistent with sum(weights)=%v", h.total, s)
	}
}

func TestAddSampleGarbageWeight(t *testing.T) {
	cases := []struct {
		name   string
		weight float64
	}{
		{"nan", math.NaN()},
		{"+inf", math.Inf(1)},
		{"-inf", math.Inf(-1)},
		{"zero", 0},
		{"negative", -3},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/empty", func(t *testing.T) {
			h := newCPU(t)
			h.AddSample(100, tc.weight, t0)
			if !h.IsEmpty() {
				t.Fatalf("weight %v must be ignored", tc.weight)
			}
		})
		// The dangerous variant: garbage weight arriving AFTER real data must
		// not poison total (NaN total makes every future percentile collapse
		// to the max bucket, silently).
		t.Run(tc.name+"/poison", func(t *testing.T) {
			h := newCPU(t)
			h.AddSample(500, 1, t0)
			before := h.Percentile(0.9)
			h.AddSample(100, tc.weight, t0)
			after := h.Percentile(0.9)
			if math.IsNaN(h.total) || math.IsInf(h.total, 0) {
				t.Fatalf("total poisoned: %v", h.total)
			}
			if after != before {
				t.Fatalf("garbage weight changed p90: %v -> %v", before, after)
			}
			checkTotalConsistent(t, h)
		})
	}
}

// TestAddSampleHugeValueSafe: with Ratio > 2, value*(Ratio-1) overflows to
// +Inf for values near MaxFloat64. Go's float->int conversion of +Inf is
// implementation-dependent (MinInt64 on amd64), which turned findBucket into
// a negative index and AddSample into an out-of-range panic.
func TestAddSampleHugeValueSafe(t *testing.T) {
	opts := Options{FirstBucketSize: 1, Ratio: 3, NumBuckets: 50, HalfLife: time.Hour, Epsilon: 1e-4}
	h := MustNew(opts)
	for _, v := range []float64{math.MaxFloat64, math.MaxFloat64 / 2, 1e300, 1e30} {
		h.AddSample(v, 1, t0) // must not panic
	}
	if h.IsEmpty() {
		t.Fatal("huge values must land in the last bucket, not vanish")
	}
	if b := opts.findBucket(math.MaxFloat64); b != opts.NumBuckets-1 {
		t.Fatalf("findBucket(MaxFloat64)=%d, want last bucket %d", b, opts.NumBuckets-1)
	}
	p := h.Percentile(1)
	if math.IsNaN(p) || math.IsInf(p, 0) || p <= 0 {
		t.Fatalf("percentile over huge values = %v", p)
	}
}

// TestFindBucketInvariantAcrossOptions replays the boundary-consistency
// property over layouts far from the defaults, including a ratio close to 1
// where the closed-form inverse is numerically at its worst.
func TestFindBucketInvariantAcrossOptions(t *testing.T) {
	layouts := []Options{
		{FirstBucketSize: 1e-6, Ratio: 1.001, NumBuckets: 5000, HalfLife: time.Hour, Epsilon: 1e-4},
		{FirstBucketSize: 1e9, Ratio: 2, NumBuckets: 64, HalfLife: time.Hour, Epsilon: 1e-4},
		{FirstBucketSize: 0.5, Ratio: 10, NumBuckets: 30, HalfLife: time.Hour, Epsilon: 1e-4},
	}
	for _, o := range layouts {
		if err := o.validate(); err != nil {
			t.Fatalf("layout should be valid: %v", err)
		}
		// Exact bucket boundaries are the adversarial inputs: the closed form
		// is most likely to land one off exactly there.
		for i := 0; i < o.NumBuckets; i++ {
			start := o.bucketStart(i)
			for _, v := range []float64{start, math.Nextafter(start, math.Inf(1)), math.Nextafter(start, math.Inf(-1))} {
				if v < 0 {
					continue
				}
				b := o.findBucket(v)
				if b < 0 || b >= o.NumBuckets {
					t.Fatalf("layout %+v: bucket %d out of range for %v", o, b, v)
				}
				if o.bucketStart(b) > v {
					t.Fatalf("layout %+v: start(%d)=%v > value %v", o, b, o.bucketStart(b), v)
				}
				if b < o.NumBuckets-1 && v >= o.bucketStart(b+1) {
					t.Fatalf("layout %+v: value %v >= start(%d)=%v", o, v, b+1, o.bucketStart(b+1))
				}
			}
		}
	}
}

func TestValidateRejectsGarbageOptions(t *testing.T) {
	base := DefaultCPUOptions()
	mutate := func(f func(*Options)) Options { o := base; f(&o); return o }
	bad := []struct {
		name string
		o    Options
	}{
		{"nan first bucket", mutate(func(o *Options) { o.FirstBucketSize = math.NaN() })},
		{"inf first bucket", mutate(func(o *Options) { o.FirstBucketSize = math.Inf(1) })},
		{"nan ratio", mutate(func(o *Options) { o.Ratio = math.NaN() })},
		{"inf ratio", mutate(func(o *Options) { o.Ratio = math.Inf(1) })},
		{"nan epsilon", mutate(func(o *Options) { o.Epsilon = math.NaN() })},
		{"epsilon one", mutate(func(o *Options) { o.Epsilon = 1 })},
		{"epsilon above one", mutate(func(o *Options) { o.Epsilon = 1.5 })},
		{"num buckets over cap", mutate(func(o *Options) { o.NumBuckets = MaxNumBuckets + 1 })},
	}
	for _, tc := range bad {
		if _, err := New(tc.o); err == nil {
			t.Errorf("%s: expected error for %+v", tc.name, tc.o)
		}
	}
	good := []Options{
		mutate(func(o *Options) { o.Epsilon = 0.999 }),
		mutate(func(o *Options) { o.NumBuckets = MaxNumBuckets }),
		mutate(func(o *Options) { o.Ratio = 1.0001 }),
	}
	for i, o := range good {
		if _, err := New(o); err != nil {
			t.Errorf("good case %d: unexpected error: %v", i, err)
		}
	}
}

func TestPercentileNaN(t *testing.T) {
	h := newCPU(t)
	h.AddSample(100, 1, t0)
	h.AddSample(900, 1, t0)
	p := h.Percentile(math.NaN())
	if math.IsNaN(p) || math.IsInf(p, 0) {
		t.Fatalf("Percentile(NaN) = %v", p)
	}
	if p != h.Max() {
		t.Fatalf("NaN p should clamp to the conservative end: got %v, max %v", p, h.Max())
	}
}

// TestMaxIgnoresNegligibleOutlier: one epsilon-negligible sample far above the
// real distribution must not set the reported Max — Max documents "highest
// non-negligible bucket", and a rightsizer keying off a single decayed outlier
// over-provisions forever.
func TestMaxIgnoresNegligibleOutlier(t *testing.T) {
	h := newCPU(t)
	h.AddSample(100, 1e6, t0)
	h.AddSample(100000, 0.01, t0) // 1e-8 of total: far below Epsilon=1e-4
	if got := h.Max(); got > 200 {
		t.Fatalf("Max=%v tracks a negligible outlier; want the ~100 bucket bound", got)
	}
	if got := h.Percentile(0.999); got > 200 {
		t.Fatalf("p999=%v tracks a negligible outlier", got)
	}
}

func TestMergeNil(t *testing.T) {
	h := newCPU(t)
	h.AddSample(100, 1, t0)
	if err := h.Merge(nil); err == nil {
		t.Fatal("merging nil must return an error, not panic")
	}
}

func TestMergeSelf(t *testing.T) {
	h := newCPU(t)
	for i := 0; i < 100; i++ {
		h.AddSample(float64(10*i), 1, t0)
	}
	total, p90 := h.total, h.Percentile(0.9)
	if err := h.Merge(h); err != nil {
		t.Fatal(err)
	}
	if h.total != total {
		t.Fatalf("self-merge changed total: %v -> %v (double count)", total, h.total)
	}
	if h.Percentile(0.9) != p90 {
		t.Fatalf("self-merge changed p90: %v -> %v", p90, h.Percentile(0.9))
	}
}

func TestMergeIntoEmpty(t *testing.T) {
	full := newCPU(t)
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 3000; i++ {
		full.AddSample(rng.Float64()*1000, 1, t0.Add(time.Duration(i)*time.Minute))
	}
	empty := newCPU(t)
	if err := empty.Merge(full); err != nil {
		t.Fatal(err)
	}
	for _, p := range []float64{0, 0.5, 0.9, 0.99, 1} {
		if empty.Percentile(p) != full.Percentile(p) {
			t.Fatalf("p%v: merged-into-empty %v != source %v", p, empty.Percentile(p), full.Percentile(p))
		}
	}
	checkTotalConsistent(t, empty)
}

// TestMergeRangeCoversCrumbs: after decay renormalization the source holds
// sub-epsilon crumb weights outside its own [minB,maxB]. Merge copies every
// nonzero bucket, so the destination's range must cover the buckets actually
// written — weight inside the range but invisible to the percentile scan
// would skew cum against total.
func TestMergeRangeCoversCrumbs(t *testing.T) {
	src := newCPU(t)
	src.AddSample(50, 1, t0)                         // 1e-6 of total: sub-epsilon crumb
	src.AddSample(1000, 1e6, t0)                     // dominant mass in a much higher bucket
	src.AddSample(1000, 1, t0.Add(101*24*time.Hour)) // 101 half-lives: forces shiftRef+compactRange, dropping the crumb from src's range
	dst := newCPU(t)
	dst.AddSample(500, 1, t0.Add(101*24*time.Hour))
	if err := dst.Merge(src); err != nil {
		t.Fatal(err)
	}
	for i, w := range dst.weights {
		if w > 0 && (i < dst.minB || i > dst.maxB) {
			t.Fatalf("bucket %d weight %v outside [minB=%d, maxB=%d]", i, w, dst.minB, dst.maxB)
		}
	}
	checkTotalConsistent(t, dst)
}

// TestMergeGhostWeights: with coarse-but-valid options (Epsilon*NumBuckets>=1)
// a flat distribution can be declared entirely negligible during the shiftRef
// inside Merge. The declared-empty histogram must actually drop that weight;
// leaving it in the buckets while zeroing total makes Checkpoint export a
// state whose recomputed total disagrees with the live histogram.
func TestMergeGhostWeights(t *testing.T) {
	opts := Options{FirstBucketSize: 10, Ratio: 1.05, NumBuckets: 10, HalfLife: time.Hour, Epsilon: 0.4}
	h := MustNew(opts)
	// Three equal buckets, each 1/3 < Epsilon of total: all "negligible".
	h.AddSample(5, 1, t0)
	h.AddSample(15, 1, t0)
	h.AddSample(30, 1, t0)
	other := MustNew(opts)
	other.AddSample(50, 1, t0.Add(time.Second)) // forces h.shiftRef -> compactRange
	if err := h.Merge(other); err != nil {
		t.Fatal(err)
	}
	checkTotalConsistent(t, h)
	c := h.Checkpoint()
	h2, err := FromCheckpoint(c)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(h2.total-h.total) > 1e-9*h.total {
		t.Fatalf("checkpoint resurrection: restored total %v != live total %v", h2.total, h.total)
	}
}

// TestAncientSampleUnderflowIgnored: a sample so far before refTime that its
// relative weight underflows to zero must be dropped entirely — pre-fix it
// contributed nothing but still smeared minB over an empty bucket.
func TestAncientSampleUnderflowIgnored(t *testing.T) {
	h := newCPU(t)
	h.AddSample(1000, 1, t0)
	minB, maxB, total := h.minB, h.maxB, h.total
	h.AddSample(5, 1, t0.Add(-1100*24*time.Hour)) // 2^-1100 underflows to 0
	if h.minB != minB || h.maxB != maxB || h.total != total {
		t.Fatalf("zero-weight sample disturbed state: minB %d->%d maxB %d->%d total %v->%v",
			minB, h.minB, maxB, h.maxB, total, h.total)
	}
}

// TestPercentileBruteForce checks the bucketized percentile exactly against a
// brute-force weighted quantile: the result must be the upper boundary of the
// bucket holding the true quantile value.
func TestPercentileBruteForce(t *testing.T) {
	o := DefaultCPUOptions()
	for _, seed := range []int64{1, 2, 3} {
		h := newCPU(t)
		rng := rand.New(rand.NewSource(seed))
		values := make([]float64, 2000)
		for i := range values {
			values[i] = rng.Float64() * 2000
			h.AddSample(values[i], 1, t0) // same timestamp: no decay in play
		}
		sort.Float64s(values)
		for _, p := range []float64{0.01, 0.25, 0.5, 0.9, 0.95, 0.99, 1} {
			// Smallest value whose cumulative weight reaches p*total.
			idx := int(math.Ceil(p*float64(len(values)))) - 1
			if idx < 0 {
				idx = 0
			}
			vq := values[idx]
			want := o.bucketStart(o.findBucket(vq) + 1)
			if got := h.Percentile(p); got != want {
				t.Fatalf("seed %d p%v: got %v, want %v (true quantile %v)", seed, p, got, want, vq)
			}
		}
	}
}

// TestPercentileMonotone: p1 <= p2 must imply Percentile(p1) <= Percentile(p2),
// including across decay and merges.
func TestPercentileMonotone(t *testing.T) {
	h := newCPU(t)
	rng := rand.New(rand.NewSource(23))
	for i := 0; i < 5000; i++ {
		h.AddSample(math.Exp(rng.Float64()*10), 1+rng.Float64()*4, t0.Add(time.Duration(i)*time.Minute))
	}
	other := newCPU(t)
	other.AddSample(3000, 100, t0.Add(200*24*time.Hour))
	if err := h.Merge(other); err != nil {
		t.Fatal(err)
	}
	prev := -1.0
	for p := 0.0; p <= 1.0; p += 0.01 {
		got := h.Percentile(p)
		if got < prev {
			t.Fatalf("Percentile(%v)=%v < Percentile(%v)=%v", p, got, p-0.01, prev)
		}
		prev = got
	}
}

// TestLastBucketClamp: values beyond the last bucket's start are reported as
// that start — a documented, finite underestimate rather than +Inf.
func TestLastBucketClamp(t *testing.T) {
	o := DefaultCPUOptions()
	h := newCPU(t)
	h.AddSample(1e9, 1, t0) // ~1M cores: far beyond the ~23k-core layout
	want := o.bucketStart(o.NumBuckets - 1)
	for _, p := range []float64{0.5, 1} {
		if got := h.Percentile(p); got != want {
			t.Fatalf("p%v=%v, want last bucket start %v", p, got, want)
		}
	}
}

func TestFromCheckpointRejectsOversizedLayout(t *testing.T) {
	c := Checkpoint{
		Options: Options{FirstBucketSize: 10, Ratio: 1.05, NumBuckets: 1 << 30, HalfLife: time.Hour, Epsilon: 1e-4},
		RefTime: t0,
	}
	if _, err := FromCheckpoint(c); err == nil {
		t.Fatal("a checkpoint demanding a gigabucket allocation must be rejected")
	}
}

func TestFromCheckpointTotalOverflow(t *testing.T) {
	c := Checkpoint{
		Options: DefaultCPUOptions(),
		RefTime: t0,
		Buckets: map[int]float64{1: math.MaxFloat64, 2: math.MaxFloat64, 3: math.MaxFloat64},
	}
	if _, err := FromCheckpoint(c); err == nil {
		t.Fatal("finite buckets summing to +Inf must be rejected, not silently emptied")
	}
}
