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
	// Epsilon is the minimum relative (to total) bucket weight considered non-empty.
	Epsilon float64
}

// DefaultCPUOptions covers 10m .. ~1000 cores with 5% resolution, 24h half-life.
func DefaultCPUOptions() Options {
	return Options{FirstBucketSize: 10, Ratio: 1.05, NumBuckets: 240, HalfLife: 24 * time.Hour, Epsilon: 1e-4}
}

// DefaultMemoryOptions covers 10Mi .. ~1.3TiB with 5% resolution, 24h half-life.
func DefaultMemoryOptions() Options {
	return Options{FirstBucketSize: 10 * (1 << 20), Ratio: 1.05, NumBuckets: 240, HalfLife: 24 * time.Hour, Epsilon: 1e-4}
}

func (o Options) validate() error {
	if o.FirstBucketSize <= 0 || o.Ratio <= 1 || o.NumBuckets < 2 || o.HalfLife <= 0 || o.Epsilon <= 0 {
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
	i := int(math.Log(value*(o.Ratio-1)/o.FirstBucketSize+1) / math.Log(o.Ratio))
	if i >= o.NumBuckets {
		return o.NumBuckets - 1
	}
	// Guard against float edge: ensure value >= start(i); the closed form can
	// land one bucket high at exact boundaries.
	if o.bucketStart(i) > value {
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

// AddSample records value with the given base weight (>=0) observed at t.
func (h *Histogram) AddSample(value, weight float64, t time.Time) {
	if weight <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	if value < 0 {
		value = 0
	}
	w := weight * h.decayFactor(t)
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
// p-quantile, p in [0,1]. Returns 0 for an empty histogram.
func (h *Histogram) Percentile(p float64) float64 {
	if h.IsEmpty() {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	threshold := p * h.total
	cum := 0.0
	b := h.maxB
	for i := h.minB; i <= h.maxB; i++ {
		cum += h.weights[i]
		if cum >= threshold && h.weights[i] > h.total*h.opts.Epsilon {
			b = i
			break
		}
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
func (h *Histogram) Merge(other *Histogram) error {
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
		if w == 0 {
			continue
		}
		h.weights[i] += w * scale
		h.total += w * scale
	}
	if h.minB < 0 || (other.minB >= 0 && other.minB < h.minB) {
		h.minB = other.minB
	}
	if other.maxB > h.maxB {
		h.maxB = other.maxB
	}
	return nil
}

// compactRange re-derives minB/maxB skipping negligible buckets.
func (h *Histogram) compactRange() {
	h.minB, h.maxB = -1, -1
	if h.total <= 0 {
		h.total = 0
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
		h.total = 0
	}
}

// Checkpoint is a compact serializable snapshot of a histogram.
type Checkpoint struct {
	Options Options         `json:"options"`
	RefTime time.Time       `json:"refTime"`
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
	h.compactRange()
	return h, nil
}
