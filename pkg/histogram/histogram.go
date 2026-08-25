// Package histogram implements exponentially-decaying, exponentially-bucketed
// histograms — the memory of Kilter's recommender. The design follows the
// battle-tested approach of Kubernetes VPA: sample weights grow exponentially
// with time relative to a reference point, which makes old observations decay
// with a configurable half-life without ever rescanning the data.
package histogram

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Options fixes the bucket layout and decay behavior of a histogram.
// Histograms with different Options are incompatible for merging.
type Options struct {
	// FirstBucketSize is the width of bucket 0 (e.g. 10 milliCPU, 10 MiB).
	FirstBucketSize float64
	// Ratio is the geometric growth of bucket widths (>1, e.g. 1.05).
	Ratio float64
	// NumBuckets bounds the value range; the last bucket absorbs everything above.
	NumBuckets int
	// HalfLife is the time for a sample's relative weight to halve.
	HalfLife time.Duration
	// Epsilon is the minimum relative (to total) bucket weight considered
	// non-empty. Must be in (0, 1); keep Epsilon*NumBuckets well below 1 or
	// every bucket of a flat distribution counts as negligible.
	Epsilon float64
}

// MaxNumBuckets bounds NumBuckets so that a corrupt or hostile checkpoint
// cannot drive an arbitrarily large allocation in New. 2^20 buckets (8 MiB
// of weights) is orders of magnitude beyond any sane layout (defaults: 240).
const MaxNumBuckets = 1 << 20

// DefaultCPUOptions covers 10m .. ~23,000 cores with 5% resolution, 24h half-life.
func DefaultCPUOptions() Options {
	return Options{FirstBucketSize: 10, Ratio: 1.05, NumBuckets: 240, HalfLife: 24 * time.Hour, Epsilon: 1e-4}
}

// DefaultMemoryOptions covers 10Mi .. ~22TiB with 5% resolution, 24h half-life.
func DefaultMemoryOptions() Options {
	return Options{FirstBucketSize: 10 * (1 << 20), Ratio: 1.05, NumBuckets: 240, HalfLife: 24 * time.Hour, Epsilon: 1e-4}
}

func (o Options) validate() error {
	// Positive-form comparisons so NaN in any field fails validation
	// (e.g. NaN <= 0 is false, but !(NaN > 0) is true).
	if !(o.FirstBucketSize > 0) || math.IsInf(o.FirstBucketSize, 0) ||
		!(o.Ratio > 1) || math.IsInf(o.Ratio, 0) ||
		o.NumBuckets < 2 || o.NumBuckets > MaxNumBuckets ||
		o.HalfLife <= 0 ||
		!(o.Epsilon > 0) || !(o.Epsilon < 1) {
		return fmt.Errorf("invalid histogram options: %+v", o)
	}
	return nil
}

// bucketStart returns the lower boundary of bucket i:
// firstBucketSize * (ratio^i - 1) / (ratio - 1), so bucket 0 starts at 0.
func (o Options) bucketStart(i int) float64 {
	return o.FirstBucketSize * (math.Pow(o.Ratio, float64(i)) - 1) / (o.Ratio - 1)
}

// findBucket locates the bucket for a value in O(1) via the closed-form inverse.
func (o Options) findBucket(value float64) int {
	if value < o.FirstBucketSize {
		return 0
	}
	// Clamp in the float domain BEFORE converting to int: for huge values
	// value*(Ratio-1) can overflow to +Inf, and Go's float->int conversion of
	// out-of-range values is implementation-dependent (MinInt64 on amd64,
	// saturating on arm64) — converting first would yield a negative index
	// and an out-of-range panic on amd64.
	fi := math.Log(value*(o.Ratio-1)/o.FirstBucketSize+1) / math.Log(o.Ratio)
	if fi >= float64(o.NumBuckets) {
		return o.NumBuckets - 1
	}
	i := int(fi)
	// Float-precision guard: at exact boundaries the closed form can land a
	// bucket or two off in either direction. Step until the invariant
	// start(i) <= v < start(i+1) holds; the loops are bounded by the actual
	// error, which is tiny, and cannot oscillate (the second loop only runs
	// when the first did not).
	for i+1 < o.NumBuckets && value >= o.bucketStart(i+1) {
		i++
	}
	for i > 0 && o.bucketStart(i) > value {
		i--
	}
	return i
}

// maxDecayExponent bounds the reference-time weight multiplier before we
// renormalize, keeping float64 arithmetic far from overflow.
const maxDecayExponent = 100

// Histogram is a decaying histogram. Not safe for concurrent use; callers
// serialize access (the brain shards by container key).
type Histogram struct {
	opts    Options
	weights []float64
	total   float64
	// refTime anchors the exponential weighting; sample weight = 2^(Δt/halfLife).
	refTime time.Time
	// first/last non-empty bucket indexes; -1 when empty.
	minB, maxB int
}

// New creates an empty histogram.
func New(opts Options) (*Histogram, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return &Histogram{
		opts:    opts,
		weights: make([]float64, opts.NumBuckets),
		minB:    -1,
		maxB:    -1,
	}, nil
}

// MustNew panics on invalid options; for use with the Default*Options.
func MustNew(opts Options) *Histogram {
	h, err := New(opts)
	if err != nil {
		panic(err)
	}
	return h
}

// IsEmpty reports whether the histogram holds no meaningful weight.
func (h *Histogram) IsEmpty() bool {
	return h.minB < 0 || h.total <= 0
}

// decayFactor returns the weight multiplier for a sample at time t.
func (h *Histogram) decayFactor(t time.Time) float64 {
	if h.refTime.IsZero() {
		h.refTime = t
	}
	exp := t.Sub(h.refTime).Seconds() / h.opts.HalfLife.Seconds()
	if exp > maxDecayExponent {
		h.shiftRef(t)
		exp = 0
	}
	return math.Exp2(exp)
}

// shiftRef moves the reference time forward, rescaling all stored weights so
// relative proportions (and therefore all percentiles) are preserved exactly.
// Both callers guarantee to >= refTime, so scale <= 1 and rescaling can only
// shrink weights, never overflow them.
func (h *Histogram) shiftRef(to time.Time) {
	if h.refTime.IsZero() {
		h.refTime = to
		return
	}
	scale := math.Exp2(-to.Sub(h.refTime).Seconds() / h.opts.HalfLife.Seconds())
	for i := range h.weights {
		h.weights[i] *= scale
	}
	h.total *= scale
	h.refTime = to
	h.compactRange()
}

// AddSample records value with the given base weight observed at t.
// Garbage in is dropped, never stored: NaN/Inf values, and weights that are
// NaN, Inf, or <= 0, are all ignored (a single NaN weight would otherwise
// poison total and every future percentile). Negative values clamp to 0.
func (h *Histogram) AddSample(value, weight float64, t time.Time) {
	// !(weight > 0) rejects zero, negatives, and NaN in one comparison.
	if !(weight > 0) || math.IsInf(weight, 1) || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	if value < 0 {
		value = 0
	}
	w := weight * h.decayFactor(t)
	// w == 0 when the sample predates refTime by so much that its relative
	// weight underflows: adding it would only smear minB/maxB over an empty
	// bucket. w == +Inf when weight*2^exp overflows: storing it would make
	// total (and all thresholds) infinite forever. Skip both.
	if !(w > 0) || math.IsInf(w, 1) {
		return
	}
	b := h.opts.findBucket(value)
	h.weights[b] += w
	h.total += w
	if h.minB < 0 || b < h.minB {
		h.minB = b
	}
	if b > h.maxB {
		h.maxB = b
	}
}

// Percentile returns a conservative estimate (upper bucket boundary) of the
// p-quantile, p in [0,1]; out-of-range and NaN p clamp to that range (NaN to
// 1, the conservative end). Returns 0 for an empty histogram. Buckets holding
// less than Epsilon of the total weight are treated as empty, so a single
// long-decayed outlier does not inflate the estimate. Values that fell in the
// unbounded last bucket are reported as its start — an underestimate; size
// NumBuckets so real workloads never land there.
func (h *Histogram) Percentile(p float64) float64 {
	if h.IsEmpty() {
		return 0
	}
	if math.IsNaN(p) || p > 1 {
		p = 1
	}
	if p < 0 {
		p = 0
	}
	threshold := p * h.total
	cum := 0.0
	// b falls back to the highest non-negligible bucket when accumulated
	// rounding (or crumbs outside [minB,maxB]) keeps cum just short of
	// threshold; falling back to maxB itself could surface a bucket holding
	// one epsilon-negligible outlier.
	b := -1
	lastSolid := -1
	for i := h.minB; i <= h.maxB; i++ {
		cum += h.weights[i]
		if h.weights[i] > h.total*h.opts.Epsilon {
			lastSolid = i
			if cum >= threshold {
				b = i
				break
			}
		}
	}
	if b < 0 {
		b = lastSolid
	}
	if b < 0 {
		b = h.maxB // pathological: no solid bucket at all (Epsilon*NumBuckets >= 1)
	}
	if b == h.opts.NumBuckets-1 {
		// Last bucket is unbounded; return its start to avoid infinity.
		return h.opts.bucketStart(b)
	}
	return h.opts.bucketStart(b + 1)
}

// Max returns the upper boundary of the highest non-negligible bucket.
func (h *Histogram) Max() float64 {
	return h.Percentile(1)
}

// Merge folds other into h. Both must share identical Options.
// Merging h into itself is a no-op (folding would double-count every sample).
func (h *Histogram) Merge(other *Histogram) error {
	if other == nil {
		return errors.New("histogram: cannot merge nil histogram")
	}
	if h == other {
		return nil
	}
	if h.opts != other.opts {
		return errors.New("histogram: cannot merge different options")
	}
	if other.IsEmpty() {
		return nil
	}
	// Align reference times: rescale the earlier one forward.
	if other.refTime.After(h.refTime) {
		h.shiftRef(other.refTime)
	}
	scale := 1.0
	if h.refTime.After(other.refTime) {
		scale = math.Exp2(-h.refTime.Sub(other.refTime).Seconds() / h.opts.HalfLife.Seconds())
	}
	for i, w := range other.weights {
		ws := w * scale
		if ws == 0 { // zero weight, or nonzero underflowed by a huge refTime gap
			continue
		}
		h.weights[i] += ws
		h.total += ws
		// Track the actual buckets touched, not other's [minB,maxB]: other may
		// carry sub-epsilon crumbs outside its own range, and weight outside
		// h's [minB,maxB] would count in total but never in a percentile scan.
		if h.minB < 0 || i < h.minB {
			h.minB = i
		}
		if i > h.maxB {
			h.maxB = i
		}
	}
	return nil
}

// compactRange re-derives minB/maxB skipping negligible buckets. When it
// declares the histogram empty it also clears the weights: leaving crumbs
// behind with total == 0 would let them leak back in through Checkpoint
// (which exports every nonzero bucket and recomputes total on restore),
// resurrecting weight that decay had already written off.
func (h *Histogram) compactRange() {
	h.minB, h.maxB = -1, -1
	if h.total <= 0 {
		h.clear()
		return
	}
	eps := h.total * h.opts.Epsilon
	for i, w := range h.weights {
		if w > eps {
			if h.minB < 0 {
				h.minB = i
			}
			h.maxB = i
		}
	}
	if h.minB < 0 {
		h.clear()
	}
}

// clear resets the histogram to the empty state (weights, total, range).
func (h *Histogram) clear() {
	for i := range h.weights {
		h.weights[i] = 0
	}
	h.total = 0
	h.minB, h.maxB = -1, -1
}

// Checkpoint is a compact serializable snapshot of a histogram.
type Checkpoint struct {
	// Options must match on restore; they define the bucket layout.
	Options Options `json:"options"`
	// RefTime anchors the decay weighting of the stored bucket weights.
	RefTime time.Time `json:"refTime"`
	// Total is informational only; FromCheckpoint recomputes it from Buckets.
	Total   float64         `json:"total"`
	Buckets map[int]float64 `json:"buckets"` // sparse: only non-zero
}

// Checkpoint exports the histogram state.
func (h *Histogram) Checkpoint() Checkpoint {
	c := Checkpoint{Options: h.opts, RefTime: h.refTime, Total: h.total, Buckets: map[int]float64{}}
	for i, w := range h.weights {
		if w > 0 {
			c.Buckets[i] = w
		}
	}
	return c
}

// FromCheckpoint restores a histogram.
func FromCheckpoint(c Checkpoint) (*Histogram, error) {
	h, err := New(c.Options)
	if err != nil {
		return nil, err
	}
	h.refTime = c.RefTime
	for i, w := range c.Buckets {
		if i < 0 || i >= len(h.weights) || w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			return nil, fmt.Errorf("histogram: corrupt checkpoint bucket %d=%v", i, w)
		}
		h.weights[i] = w
	}
	// Recompute total from buckets — never trust a stored aggregate.
	h.total = 0
	for _, w := range h.weights {
		h.total += w
	}
	// Individually-finite weights can still sum past MaxFloat64; an infinite
	// total would make every epsilon threshold infinite and silently empty
	// the histogram. Reject instead.
	if math.IsInf(h.total, 0) {
		return nil, errors.New("histogram: corrupt checkpoint: total weight overflows")
	}
	h.compactRange()
	return h, nil
}
