package decision

import (
	"fmt"
	"math"
	"time"
)

// ChangepointConfig tunes the two-sided winsorized CUSUM detector. All
// parameters are in standardized (sigma) units against an EWMA baseline, so
// one config works for CPU millicores and memory bytes alike.
//
// Sensitivity model (documented so tuning is engineering, not vibes):
// after a sustained level shift of δ sigmas, each sample adds
// min(δ, ZClamp) − DriftK to the CUSUM, so detection delay ≈
// ⌈ThresholdH / (min(δ, ZClamp) − DriftK)⌉ samples. Shifts below DriftK
// sigmas are inside the drift allowance and never accumulate — they are the
// domain of the trend machinery (patterns.TrendPerDay), not regime
// detection.
type ChangepointConfig struct {
	// Alpha is the EWMA smoothing for the level/variance baseline.
	// Range (0, 0.5]; default 0.02 (~100-sample memory, ≈8h at 5-min
	// samples). The baseline must adapt slower than the CUSUM accumulates
	// or shifts get absorbed before they can fire; α > 0.5 chases every
	// sample and blinds the detector.
	Alpha float64
	// DriftK is the per-sample slack in sigmas (classical CUSUM k).
	// Range [0.05, 5], and < ZClamp; default 0.5, which targets fastest
	// detection of ~1σ sustained shifts (k = δ*/2).
	DriftK float64
	// ThresholdH is the decision threshold in accumulated sigmas.
	// Range (DriftK, 100], and > ZClamp−DriftK so that no single sample —
	// however extreme — can fire the detector alone (a one-sample
	// excursion is an anomaly, not a regime). Default 5: with k=0.5 a true
	// 1σ shift fires in ~10 samples (~50min) while stationary noise
	// virtually never accumulates 5 sigmas.
	ThresholdH float64
	// ZClamp winsorizes each standardized residual to ±ZClamp before it is
	// accumulated *and* before it moves the EWMA baseline. Range [2, 100];
	// default 4. This is what makes single balloons harmless in both
	// directions: an arbitrarily large sample contributes at most
	// ZClamp−DriftK to the CUSUM and moves the level/variance estimate by
	// at most ZClamp sigmas, so one balloon can neither fire the detector
	// nor blind it to the next real shift.
	ZClamp float64
	// Warmup is the number of accepted samples that must pass before
	// detection can fire — the variance estimate needs to settle. Also the
	// re-arm holdoff after each fire, while the baseline re-learns the new
	// level. Range [8, 100000]; default 24 (2h at 5-min samples).
	Warmup int
	// MinSigmaFrac floors the standardization sigma at this fraction of
	// the baseline mean, so quantization wiggle on a near-constant series
	// does not standardize to infinity. Range [1e-9, 1]; default 0.05
	// (5% of level — roughly scrape noise on a flat series).
	MinSigmaFrac float64
}

// DefaultChangepointConfig returns the documented defaults.
func DefaultChangepointConfig() ChangepointConfig {
	return ChangepointConfig{
		Alpha:        0.02,
		DriftK:       0.5,
		ThresholdH:   5,
		ZClamp:       4,
		Warmup:       24,
		MinSigmaFrac: 0.05,
	}
}

// Validate rejects out-of-range and NaN parameters (positive-form
// comparisons throughout).
func (c ChangepointConfig) Validate() error {
	if !(c.Alpha > 0) || !(c.Alpha <= 0.5) {
		return fmt.Errorf("decision: changepoint Alpha %v out of (0,0.5]", c.Alpha)
	}
	if !(c.DriftK >= 0.05) || !(c.DriftK <= 5) {
		return fmt.Errorf("decision: changepoint DriftK %v out of [0.05,5]", c.DriftK)
	}
	if !(c.ZClamp >= 2) || !(c.ZClamp <= 100) || !(c.ZClamp > c.DriftK) {
		return fmt.Errorf("decision: changepoint ZClamp %v out of [2,100] or <= DriftK", c.ZClamp)
	}
	if !(c.ThresholdH > c.DriftK) || !(c.ThresholdH <= 100) {
		return fmt.Errorf("decision: changepoint ThresholdH %v out of (DriftK,100]", c.ThresholdH)
	}
	if !(c.ThresholdH > c.ZClamp-c.DriftK) {
		return fmt.Errorf("decision: changepoint ThresholdH %v must exceed ZClamp-DriftK %v (single-sample fire)", c.ThresholdH, c.ZClamp-c.DriftK)
	}
	if c.Warmup < 8 || c.Warmup > 100000 {
		return fmt.Errorf("decision: changepoint Warmup %d out of [8,100000]", c.Warmup)
	}
	if !(c.MinSigmaFrac >= 1e-9) || !(c.MinSigmaFrac <= 1) {
		return fmt.Errorf("decision: changepoint MinSigmaFrac %v out of [1e-9,1]", c.MinSigmaFrac)
	}
	return nil
}

// minAbsSigma is the absolute sigma floor for an all-zero baseline (an idle
// series has mean 0, so the relative floor vanishes). Any tiny positive
// value works; activity on a truly idle series should standardize huge and
// be caught by the clamp.
const minAbsSigma = 1e-12

// Detection describes the most recent regime change.
type Detection struct {
	At time.Time `json:"at"`
	// Direction is +1 (level shifted up) or -1 (down).
	Direction int `json:"direction"`
	// MagnitudeSigma estimates the shift size in baseline sigmas
	// (average accumulated excess per sample plus the drift allowance).
	MagnitudeSigma float64 `json:"magnitudeSigma"`
}

// Changepoint is an online two-sided winsorized CUSUM (Page) detector over
// an EWMA level/variance baseline. O(1) memory and time per sample,
// deterministic, checkpointable. Not safe for concurrent use; owners
// serialize access (the recommender shards by container key).
type Changepoint struct {
	cfg ChangepointConfig

	n          int // accepted samples ever
	warmupLeft int // samples until detection may fire

	mean, vari float64 // EWMA level and West's EWM variance

	cusumPos, cusumNeg float64
	posRun, negRun     int // samples since the respective cusum was last zero

	last    Detection
	fired   bool // whether any detection has ever fired
	dropped int  // rejected garbage samples (for observability)
}

// NewChangepoint validates the config and returns a detector.
func NewChangepoint(cfg ChangepointConfig) (*Changepoint, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Changepoint{cfg: cfg, warmupLeft: cfg.Warmup}, nil
}

// N returns the number of accepted samples.
func (c *Changepoint) N() int { return c.n }

// Dropped returns the number of rejected garbage samples.
func (c *Changepoint) Dropped() int { return c.dropped }

// Last returns the most recent detection and whether one has ever fired.
func (c *Changepoint) Last() (Detection, bool) { return c.last, c.fired }

// Add ingests one sample and reports whether a regime change fired at this
// sample and in which direction (+1 up, -1 down; 0 when not fired).
//
// Garbage samples — NaN, ±Inf, negative (usage series are non-negative by
// contract), or beyond maxAbsSample — are counted in Dropped and otherwise
// ignored: they never fire, never move the baseline, and never advance
// warmup. Samples are expected in non-decreasing timestamp order; t is only
// recorded in the Detection, never used for math, so out-of-order times
// cannot corrupt state.
//
// The baseline update is winsorized to ±ZClamp sigmas (see ZClamp), so a
// single extreme-but-in-range sample can move the level and variance
// estimate only by a bounded amount.
//
// On fire the detector re-seeds its level at the firing sample, keeps the
// variance estimate (the old regime's noise scale is the best available
// guess for the new one), resets both CUSUMs, and re-enters warmup as a
// re-arm holdoff.
func (c *Changepoint) Add(t time.Time, v float64) (fired bool, direction int) {
	if !(v >= 0 && v <= maxAbsSample) { // catches NaN, ±Inf, negatives
		c.dropped++
		return false, 0
	}
	c.n++
	if c.n == 1 {
		c.mean = v
		c.warmupLeft--
		return false, 0
	}
	if c.warmupLeft > 0 {
		c.warmupLeft--
		c.updateBaseline(v)
		return false, 0
	}

	sigma := math.Sqrt(c.vari)
	if floor := c.cfg.MinSigmaFrac * c.mean; sigma < floor {
		sigma = floor
	}
	if sigma < minAbsSigma {
		sigma = minAbsSigma
	}
	// Winsorize the residual, and winsorize the *baseline update* with it.
	// Clamping only the accumulation still leaves the EWMA variance fully
	// exposed to a single balloon: one 1e9 sample on a 1e3 series inflates
	// vari by ~1e12, after which sigma swamps every later residual and the
	// detector goes silently blind to genuine regime changes for roughly
	// 1/Alpha samples. baseV is the sample as far as the baseline is
	// allowed to see it — at most ZClamp sigmas from the current level.
	z := (v - c.mean) / sigma
	baseV := v
	if z > c.cfg.ZClamp {
		z = c.cfg.ZClamp
		baseV = c.mean + z*sigma
	} else if z < -c.cfg.ZClamp {
		z = -c.cfg.ZClamp
		baseV = c.mean + z*sigma
	}
	// Keep the baseline inside the accepted-sample domain so the level can
	// never wander negative or beyond maxAbsSample (mean - ZClamp*sigma is
	// negative whenever sigma is large relative to the level).
	if !(baseV > 0) { // catches a clamp that undershot zero
		baseV = 0
	} else if baseV > maxAbsSample {
		baseV = maxAbsSample
	}

	c.cusumPos += z - c.cfg.DriftK
	if !(c.cusumPos > 0) {
		c.cusumPos, c.posRun = 0, 0
	} else {
		c.posRun++
	}
	c.cusumNeg += -z - c.cfg.DriftK
	if !(c.cusumNeg > 0) {
		c.cusumNeg, c.negRun = 0, 0
	} else {
		c.negRun++
	}

	if c.cusumPos > c.cfg.ThresholdH {
		c.fire(t, v, +1, c.cusumPos, c.posRun)
		return true, +1
	}
	if c.cusumNeg > c.cfg.ThresholdH {
		c.fire(t, v, -1, c.cusumNeg, c.negRun)
		return true, -1
	}

	c.updateBaseline(baseV)
	return false, 0
}

func (c *Changepoint) fire(t time.Time, v float64, dir int, cusum float64, run int) {
	mag := c.cfg.ZClamp // degenerate run==0 cannot happen (cusum>H>0 ⇒ run≥1), but stay safe
	if run > 0 {
		mag = cusum/float64(run) + c.cfg.DriftK
	}
	c.last = Detection{At: t, Direction: dir, MagnitudeSigma: mag}
	c.fired = true
	c.cusumPos, c.cusumNeg = 0, 0
	c.posRun, c.negRun = 0, 0
	c.mean = v // jump the level to the new regime; re-learn variance shape
	c.warmupLeft = c.cfg.Warmup
}

func (c *Changepoint) updateBaseline(v float64) {
	delta := v - c.mean
	c.mean += c.cfg.Alpha * delta
	c.vari = (1 - c.cfg.Alpha) * (c.vari + c.cfg.Alpha*delta*delta)
}

// ChangepointCheckpoint is the serializable state of a detector.
type ChangepointCheckpoint struct {
	Config     ChangepointConfig `json:"config"`
	N          int               `json:"n"`
	WarmupLeft int               `json:"warmupLeft"`
	Mean       float64           `json:"mean"`
	Vari       float64           `json:"vari"`
	CusumPos   float64           `json:"cusumPos"`
	CusumNeg   float64           `json:"cusumNeg"`
	PosRun     int               `json:"posRun"`
	NegRun     int               `json:"negRun"`
	Last       Detection         `json:"last"`
	Fired      bool              `json:"fired"`
	// Dropped is the rejected-garbage counter. It is part of the
	// checkpoint because an operator watching it as a data-quality metric
	// must not see it reset to zero on every brain restart.
	Dropped int `json:"dropped"`
}

// Checkpoint exports the detector state.
func (c *Changepoint) Checkpoint() ChangepointCheckpoint {
	return ChangepointCheckpoint{
		Config: c.cfg, N: c.n, WarmupLeft: c.warmupLeft,
		Mean: c.mean, Vari: c.vari,
		CusumPos: c.cusumPos, CusumNeg: c.cusumNeg,
		PosRun: c.posRun, NegRun: c.negRun,
		Last: c.last, Fired: c.fired,
		Dropped: c.dropped,
	}
}

// ChangepointFromCheckpoint restores a detector, rejecting corrupt state:
// non-finite or out-of-range fields would otherwise poison every future
// sample, and a stored CUSUM already beyond the threshold would fire
// spuriously on the first post-restore sample.
func ChangepointFromCheckpoint(ck ChangepointCheckpoint) (*Changepoint, error) {
	if err := ck.Config.Validate(); err != nil {
		return nil, err
	}
	if ck.N < 0 || ck.WarmupLeft < 0 || ck.WarmupLeft > ck.Config.Warmup {
		return nil, fmt.Errorf("decision: corrupt changepoint checkpoint: n=%d warmupLeft=%d", ck.N, ck.WarmupLeft)
	}
	if !(ck.Mean >= 0 && ck.Mean <= maxAbsSample) || !(ck.Vari >= 0) || math.IsInf(ck.Vari, 0) {
		return nil, fmt.Errorf("decision: corrupt changepoint checkpoint: mean=%v vari=%v", ck.Mean, ck.Vari)
	}
	if !(ck.CusumPos >= 0 && ck.CusumPos <= ck.Config.ThresholdH) ||
		!(ck.CusumNeg >= 0 && ck.CusumNeg <= ck.Config.ThresholdH) {
		return nil, fmt.Errorf("decision: corrupt changepoint checkpoint: cusums %v/%v out of [0,H]", ck.CusumPos, ck.CusumNeg)
	}
	if ck.PosRun < 0 || ck.NegRun < 0 || ck.Dropped < 0 {
		return nil, fmt.Errorf("decision: corrupt changepoint checkpoint: runs %d/%d dropped=%d", ck.PosRun, ck.NegRun, ck.Dropped)
	}
	if ck.Fired && (ck.Last.Direction != 1 && ck.Last.Direction != -1 ||
		!(ck.Last.MagnitudeSigma >= 0) || math.IsInf(ck.Last.MagnitudeSigma, 0)) {
		return nil, fmt.Errorf("decision: corrupt changepoint checkpoint: detection %+v", ck.Last)
	}
	return &Changepoint{
		cfg: ck.Config, n: ck.N, warmupLeft: ck.WarmupLeft,
		mean: ck.Mean, vari: ck.Vari,
		cusumPos: ck.CusumPos, cusumNeg: ck.CusumNeg,
		posRun: ck.PosRun, negRun: ck.NegRun,
		last: ck.Last, fired: ck.Fired,
		dropped: ck.Dropped,
	}, nil
}
