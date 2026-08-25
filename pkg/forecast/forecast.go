// Package forecast provides lightweight online time-series models used by the
// brain to anticipate cluster demand: EWMA baselines with variance bands,
// Holt-Winters triple exponential smoothing for seasonal workloads, and a
// spike detector that guards scale-down decisions against volatile workloads.
//
// All models are online (O(1) per sample, bounded memory) so the brain can
// track hundreds of thousands of series under high load.
//
// Models are not safe for concurrent use; callers serialize access per series.
package forecast

import (
	"fmt"
	"math"
)

// maxAbsSample bounds the magnitude of an accepted observation. Real resource
// quantities (millicores, bytes, dollars) sit far below 1e150; anything larger
// is sensor garbage, and admitting it would let intermediate products such as
// delta² or level+trend overflow float64 and permanently poison the online
// state with ±Inf/NaN. With inputs bounded here every intermediate stays
// finite: |delta| ≤ 2e150, so delta² ≤ 4e300 < MaxFloat64.
const maxAbsSample = 1e150

// usable reports whether v may enter a model. The range test is also false
// for NaN and ±Inf, so a single comparison covers all garbage classes.
func usable(v float64) bool {
	return v >= -maxAbsSample && v <= maxAbsSample
}

// EWMA is an exponentially weighted moving average with online variance
// (EWMVar), suitable as a cheap baseline + band model.
type EWMA struct {
	alpha float64
	mean  float64
	vari  float64
	n     int
}

// NewEWMA creates an EWMA with smoothing factor alpha in (0,1].
// Larger alpha reacts faster; 2/(N+1) approximates an N-sample window.
func NewEWMA(alpha float64) (*EWMA, error) {
	if alpha <= 0 || alpha > 1 || math.IsNaN(alpha) {
		return nil, fmt.Errorf("forecast: alpha %v out of (0,1]", alpha)
	}
	return &EWMA{alpha: alpha}, nil
}

// Add feeds one observation. Garbage samples (NaN, ±Inf, or magnitude beyond
// maxAbsSample) are ignored and do not count toward N.
func (e *EWMA) Add(v float64) {
	if !usable(v) {
		return
	}
	e.n++
	if e.n == 1 {
		e.mean = v
		return
	}
	delta := v - e.mean
	incr := e.alpha * delta
	e.mean += incr
	// West's EWM variance update.
	e.vari = (1 - e.alpha) * (e.vari + e.alpha*delta*delta)
}

// N returns the number of samples observed.
func (e *EWMA) N() int { return e.n }

// Mean returns the current smoothed mean.
func (e *EWMA) Mean() float64 { return e.mean }

// StdDev returns the current smoothed standard deviation.
func (e *EWMA) StdDev() float64 { return math.Sqrt(e.vari) }

// UpperBound returns mean + k*stddev, floored at the mean: a negative or NaN
// k can never produce a bound below the current mean.
func (e *EWMA) UpperBound(k float64) float64 {
	b := e.mean + k*e.StdDev()
	if math.IsNaN(b) || b < e.mean {
		return e.mean
	}
	return b
}

// HoltWinters is additive triple exponential smoothing. With Gamma=0 and
// SeasonLen=0 it degrades gracefully to double exponential smoothing
// (level + trend), which is the right default for series without a known
// season (resource demand often has daily seasonality → SeasonLen = samples/day).
type HoltWinters struct {
	alpha, beta, gamma float64
	seasonLen          int

	level, trend float64
	seasonal     []float64

	n       int
	initBuf []float64
}

// NewHoltWinters validates parameters. seasonLen == 0 disables seasonality.
func NewHoltWinters(alpha, beta, gamma float64, seasonLen int) (*HoltWinters, error) {
	// NaN fails every range comparison below, so it must be rejected
	// explicitly; ±Inf is already caught by the range checks.
	if math.IsNaN(alpha) || math.IsNaN(beta) || math.IsNaN(gamma) ||
		alpha <= 0 || alpha > 1 || beta < 0 || beta > 1 || gamma < 0 || gamma > 1 {
		return nil, fmt.Errorf("forecast: smoothing params out of range a=%v b=%v g=%v", alpha, beta, gamma)
	}
	if seasonLen < 0 || seasonLen == 1 {
		return nil, fmt.Errorf("forecast: seasonLen %d invalid", seasonLen)
	}
	if seasonLen > 0 && gamma == 0 {
		return nil, fmt.Errorf("forecast: seasonal model requires gamma > 0")
	}
	return &HoltWinters{alpha: alpha, beta: beta, gamma: gamma, seasonLen: seasonLen}, nil
}

// Ready reports whether the model has enough history to forecast.
func (hw *HoltWinters) Ready() bool {
	if hw.seasonLen == 0 {
		return hw.n >= 2
	}
	return hw.n >= 2*hw.seasonLen
}

// N returns the number of samples observed.
func (hw *HoltWinters) N() int { return hw.n }

// Add feeds one observation (fixed sampling interval assumed). Garbage
// samples (NaN, ±Inf, or magnitude beyond maxAbsSample) are ignored: they do
// not count toward N and do not shift the seasonal phase.
func (hw *HoltWinters) Add(v float64) {
	if !usable(v) {
		return
	}
	hw.n++
	if hw.seasonLen == 0 {
		hw.addTrendOnly(v)
		return
	}
	if hw.n <= 2*hw.seasonLen {
		hw.initBuf = append(hw.initBuf, v)
		if hw.n == 2*hw.seasonLen {
			hw.initialize()
		}
		return
	}
	si := (hw.n - 1) % hw.seasonLen
	prevLevel := hw.level
	hw.level = hw.alpha*(v-hw.seasonal[si]) + (1-hw.alpha)*(hw.level+hw.trend)
	hw.trend = hw.beta*(hw.level-prevLevel) + (1-hw.beta)*hw.trend
	hw.seasonal[si] = hw.gamma*(v-hw.level) + (1-hw.gamma)*hw.seasonal[si]
}

func (hw *HoltWinters) addTrendOnly(v float64) {
	switch hw.n {
	case 1:
		hw.level = v
	case 2:
		hw.trend = v - hw.level
		hw.level = hw.alpha*v + (1-hw.alpha)*(hw.level+hw.trend)
	default:
		prevLevel := hw.level
		hw.level = hw.alpha*v + (1-hw.alpha)*(hw.level+hw.trend)
		hw.trend = hw.beta*(hw.level-prevLevel) + (1-hw.beta)*hw.trend
	}
}

// initialize seeds level/trend/seasonal from the first two full seasons.
func (hw *HoltWinters) initialize() {
	L := hw.seasonLen
	mean1, mean2 := 0.0, 0.0
	for i := 0; i < L; i++ {
		mean1 += hw.initBuf[i]
		mean2 += hw.initBuf[L+i]
	}
	mean1 /= float64(L)
	mean2 /= float64(L)

	hw.level = mean2
	hw.trend = (mean2 - mean1) / float64(L)
	hw.seasonal = make([]float64, L)
	for i := 0; i < L; i++ {
		hw.seasonal[i] = ((hw.initBuf[i] - mean1) + (hw.initBuf[L+i] - mean2)) / 2
	}
	hw.initBuf = nil
}

// Forecast projects h steps ahead (h >= 1). Results are clamped at 0 because
// resource demand cannot be negative. Returns 0 if not Ready.
func (hw *HoltWinters) Forecast(h int) float64 {
	if !hw.Ready() || h < 1 {
		return 0
	}
	v := hw.level + float64(h)*hw.trend
	if hw.seasonLen > 0 {
		// Reduce h modulo the season before adding: (hw.n - 1 + h) can
		// overflow int for large horizons, and a negative index into
		// seasonal would panic.
		si := ((hw.n-1)%hw.seasonLen + h%hw.seasonLen) % hw.seasonLen
		v += hw.seasonal[si]
	}
	// Demand cannot be negative, and a degenerate model must never report
	// unbounded demand; 0 is the package-wide "no usable forecast" sentinel.
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// DefaultDemand returns a Holt-Winters tuned for cluster demand sampled every
// 5 minutes with daily seasonality (288 samples/day).
func DefaultDemand() *HoltWinters {
	hw, err := NewHoltWinters(0.4, 0.05, 0.15, 288)
	if err != nil {
		panic(err)
	}
	return hw
}

// SpikeDetector flags observations that exceed the EWMA band. The brain uses
// it to veto aggressive scale-down for workloads with bursty history.
type SpikeDetector struct {
	baseline *EWMA
	k        float64
	spikes   int
	total    int
}

// NewSpikeDetector builds a detector; k is the band width in stddevs (e.g. 3).
func NewSpikeDetector(alpha, k float64) (*SpikeDetector, error) {
	e, err := NewEWMA(alpha)
	if err != nil {
		return nil, err
	}
	// NaN fails k <= 0, and an infinite k yields a NaN band (Inf*0 when the
	// baseline variance is 0) that would silently never flag a spike.
	if math.IsNaN(k) || math.IsInf(k, 0) || k <= 0 {
		return nil, fmt.Errorf("forecast: k %v must be finite and > 0", k)
	}
	return &SpikeDetector{baseline: e, k: k}, nil
}

// Observe feeds a sample and reports whether it is a spike. Garbage samples
// (NaN, ±Inf, or magnitude beyond maxAbsSample) are ignored entirely: they
// are never spikes, do not move the baseline, and do not dilute SpikeRate.
// Warm-up (first 10 samples) never reports spikes.
func (s *SpikeDetector) Observe(v float64) bool {
	if !usable(v) {
		return false
	}
	s.total++
	warm := s.baseline.N() >= 10
	spike := false
	if warm {
		bound := s.baseline.UpperBound(s.k)
		// Guard tiny baselines: below-noise-floor values never spike.
		if v > bound && v-s.baseline.Mean() > 1e-9 {
			spike = true
			s.spikes++
		}
	}
	s.baseline.Add(v)
	return spike
}

// SpikeRate returns the fraction of observed samples flagged as spikes.
func (s *SpikeDetector) SpikeRate() float64 {
	if s.total == 0 {
		return 0
	}
	return float64(s.spikes) / float64(s.total)
}
