package decision

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	"time"
)

// cpStart is the fixed timestamp the synthetic traces start at, and cpStep
// the 5-minute scrape cadence the parameter rationale is written against.
var cpStart = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

const cpStep = 5 * time.Minute

// analyticCPConfig is the config every analytically-solved trace uses.
// Alpha is deliberately tiny (0.001) so the EWMA baseline barely moves over
// the ~10 samples a detection takes: that makes the standardized residual z
// stay ~= the injected shift delta, so the detection delay is exactly the
// closed form documented on ChangepointConfig,
//
//	delay = floor(H / (min(delta, ZClamp) - DriftK)) + 1
//
// (strictly-greater-than threshold, hence the +1). Warmup is the minimum
// legal 8 to keep traces short.
func analyticCPConfig() ChangepointConfig {
	return ChangepointConfig{
		Alpha:        0.001,
		DriftK:       0.5,
		ThresholdH:   5,
		ZClamp:       4,
		Warmup:       8,
		MinSigmaFrac: 0.05,
	}
}

// analyticDelay is the closed form above.
func analyticDelay(cfg ChangepointConfig, deltaSigma float64) int {
	eff := math.Min(math.Abs(deltaSigma), cfg.ZClamp) - cfg.DriftK
	if !(eff > 0) {
		return -1 // inside the drift allowance: never accumulates
	}
	return int(math.Floor(cfg.ThresholdH/eff)) + 1
}

type fireEvent struct {
	index int // 1-based index within the fed slice
	dir   int
	det   Detection
}

// feed pushes vals at cpStep intervals and records every firing.
func feed(t *testing.T, c *Changepoint, vals []float64) []fireEvent {
	t.Helper()
	var out []fireEvent
	for i, v := range vals {
		ts := cpStart.Add(time.Duration(i) * cpStep)
		fired, dir := c.Add(ts, v)
		if fired != (dir != 0) {
			t.Fatalf("sample %d: fired=%v but direction=%d (must agree)", i, fired, dir)
		}
		if fired {
			det, ok := c.Last()
			if !ok {
				t.Fatalf("sample %d: fired but Last() reports never fired", i)
			}
			if !det.At.Equal(ts) {
				t.Fatalf("sample %d: Detection.At = %v, want %v", i, det.At, ts)
			}
			out = append(out, fireEvent{index: i + 1, dir: dir, det: det})
		}
	}
	return out
}

func repeat(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func newCP(t *testing.T, cfg ChangepointConfig) *Changepoint {
	t.Helper()
	c, err := NewChangepoint(cfg)
	if err != nil {
		t.Fatalf("NewChangepoint: %v", err)
	}
	return c
}

// settleSamples is how many constant samples are fed before a step, so the
// detector is out of warmup with a known baseline (mean = level, vari = 0).
const settleSamples = 12

func TestChangepointConfigValidate(t *testing.T) {
	ok := DefaultChangepointConfig()
	if err := ok.Validate(); err != nil {
		t.Fatalf("DefaultChangepointConfig must validate: %v", err)
	}
	if err := analyticCPConfig().Validate(); err != nil {
		t.Fatalf("analyticCPConfig must validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ChangepointConfig)
	}{
		{"alpha zero", func(c *ChangepointConfig) { c.Alpha = 0 }},
		{"alpha negative", func(c *ChangepointConfig) { c.Alpha = -0.1 }},
		{"alpha above half", func(c *ChangepointConfig) { c.Alpha = 0.51 }},
		{"alpha NaN", func(c *ChangepointConfig) { c.Alpha = math.NaN() }},
		{"alpha Inf", func(c *ChangepointConfig) { c.Alpha = math.Inf(1) }},
		{"driftK too small", func(c *ChangepointConfig) { c.DriftK = 0.04 }},
		{"driftK too large", func(c *ChangepointConfig) { c.DriftK = 5.1 }},
		{"driftK NaN", func(c *ChangepointConfig) { c.DriftK = math.NaN() }},
		{"zclamp below 2", func(c *ChangepointConfig) { c.ZClamp = 1.9 }},
		{"zclamp above 100", func(c *ChangepointConfig) { c.ZClamp = 101 }},
		{"zclamp not above driftK", func(c *ChangepointConfig) { c.DriftK, c.ZClamp = 3, 3 }},
		{"zclamp NaN", func(c *ChangepointConfig) { c.ZClamp = math.NaN() }},
		{"threshold below driftK", func(c *ChangepointConfig) { c.ThresholdH = 0.4 }},
		{"threshold above 100", func(c *ChangepointConfig) { c.ThresholdH = 101 }},
		{"threshold NaN", func(c *ChangepointConfig) { c.ThresholdH = math.NaN() }},
		// A single winsorized sample contributes at most ZClamp-DriftK, so
		// H must exceed that or one balloon fires the detector alone.
		{"threshold allows single-sample fire", func(c *ChangepointConfig) {
			c.ZClamp, c.DriftK, c.ThresholdH = 8, 0.5, 7
		}},
		{"warmup too small", func(c *ChangepointConfig) { c.Warmup = 7 }},
		{"warmup negative", func(c *ChangepointConfig) { c.Warmup = -1 }},
		{"warmup too large", func(c *ChangepointConfig) { c.Warmup = 100001 }},
		{"minSigmaFrac zero", func(c *ChangepointConfig) { c.MinSigmaFrac = 0 }},
		{"minSigmaFrac above 1", func(c *ChangepointConfig) { c.MinSigmaFrac = 1.1 }},
		{"minSigmaFrac NaN", func(c *ChangepointConfig) { c.MinSigmaFrac = math.NaN() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultChangepointConfig()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
			if c, err := NewChangepoint(cfg); err == nil || c != nil {
				t.Fatalf("NewChangepoint accepted invalid config: c=%v err=%v", c, err)
			}
		})
	}
}

// TestChangepointStepDetectionDelay is the analytic core: a clean level step
// of exactly delta sigmas must fire at exactly the closed-form delay, in the
// right direction, with a magnitude estimate that recovers delta (saturating
// at ZClamp, which is the documented limit of a winsorized estimator).
func TestChangepointStepDetectionDelay(t *testing.T) {
	cfg := analyticCPConfig()
	const level = 1000.0
	// vari is 0 on a constant series, so sigma is the MinSigmaFrac floor.
	sigma := cfg.MinSigmaFrac * level // 50

	cases := []struct {
		name    string
		delta   float64
		wantDir int
		wantMag float64
	}{
		{"2 sigma up", 2, +1, 2},
		{"2 sigma down", -2, -1, 2},
		{"1 sigma up", 1, +1, 1},
		{"3 sigma down", -3, -1, 3},
		{"8 sigma up saturates at ZClamp", 8, +1, 4},
		{"20 sigma down saturates at ZClamp", -20, -1, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCP(t, cfg)
			vals := repeat(level, settleSamples)
			vals = append(vals, repeat(level+tc.delta*sigma, 60)...)
			fires := feed(t, c, vals)

			if len(fires) == 0 {
				t.Fatalf("a %v-sigma step never fired", tc.delta)
			}
			first := fires[0]
			gotDelay := first.index - settleSamples
			wantDelay := analyticDelay(cfg, tc.delta)
			// The EWMA baseline creeps toward the new level, shrinking z
			// slightly, so the observed delay can only be >= the closed
			// form; allow a small slack above it.
			if gotDelay < wantDelay {
				t.Fatalf("fired after %d samples, faster than the analytic bound %d", gotDelay, wantDelay)
			}
			if gotDelay > wantDelay+2 {
				t.Fatalf("fired after %d samples, want ~%d", gotDelay, wantDelay)
			}
			if first.dir != tc.wantDir {
				t.Fatalf("direction = %d, want %d", first.dir, tc.wantDir)
			}
			if math.Abs(first.det.MagnitudeSigma-tc.wantMag) > 0.15 {
				t.Fatalf("MagnitudeSigma = %.3f, want ~%.1f", first.det.MagnitudeSigma, tc.wantMag)
			}
		})
	}
}

// TestChangepointSubDriftShiftNeverFires: a sustained shift smaller than the
// drift allowance is by construction invisible to CUSUM — it belongs to the
// trend machinery (patterns.TrendPerDay), not regime detection. This is the
// documented meaning of DriftK and must hold for an unbounded run.
func TestChangepointSubDriftShiftNeverFires(t *testing.T) {
	cfg := analyticCPConfig()
	const level = 1000.0
	sigma := cfg.MinSigmaFrac * level

	for _, delta := range []float64{0.1, 0.25, 0.4, 0.49, -0.4, -0.49} {
		t.Run(fmt.Sprintf("%.2f sigma", delta), func(t *testing.T) {
			c := newCP(t, cfg)
			vals := repeat(level, settleSamples)
			vals = append(vals, repeat(level+delta*sigma, 5000)...)
			if fires := feed(t, c, vals); len(fires) != 0 {
				t.Fatalf("%.2f-sigma shift fired %d time(s); DriftK=%v must absorb it",
					delta, len(fires), cfg.DriftK)
			}
		})
	}
}

// TestChangepointConstantSeriesNeverFires: a perfectly flat series has zero
// variance; without the MinSigmaFrac floor every residual would standardize
// to infinity and the detector would fire on quantization wiggle.
func TestChangepointConstantSeriesNeverFires(t *testing.T) {
	for _, level := range []float64{0, 1, 1000, 1e9, maxAbsSample} {
		c := newCP(t, DefaultChangepointConfig())
		if fires := feed(t, c, repeat(level, 3000)); len(fires) != 0 {
			t.Fatalf("constant series at %v fired %d time(s)", level, len(fires))
		}
	}
}

// TestChangepointOneSampleExcursionNeverFires is the invariant the ZClamp /
// ThresholdH relationship exists to guarantee: however extreme, a single
// sample contributes at most ZClamp-DriftK < ThresholdH, so one balloon is
// an anomaly, never a regime.
func TestChangepointOneSampleExcursionNeverFires(t *testing.T) {
	for _, cfg := range []ChangepointConfig{DefaultChangepointConfig(), analyticCPConfig()} {
		for _, spike := range []float64{2000, 1e6, 1e12, maxAbsSample, 0} {
			c := newCP(t, cfg)
			vals := repeat(1000.0, settleSamples+cfg.Warmup)
			vals = append(vals, spike)
			vals = append(vals, repeat(1000.0, 5)...)
			if fires := feed(t, c, vals); len(fires) != 0 {
				t.Fatalf("single %v excursion fired: %+v", spike, fires)
			}
		}
	}
}

// TestChangepointSpikeDoesNotBlindDetector: one in-range but extreme sample
// must not destroy the variance baseline. If it does, sigma explodes, every
// later residual standardizes to ~0, and a genuine regime change that
// follows is invisible — the detector fails silent, which is exactly what
// the package contract forbids.
func TestChangepointSpikeDoesNotBlindDetector(t *testing.T) {
	cfg := analyticCPConfig()
	const level = 1000.0
	sigma := cfg.MinSigmaFrac * level

	for _, spike := range []float64{1e6, 1e9, 1e15} {
		t.Run(fmt.Sprintf("spike %.0e", spike), func(t *testing.T) {
			c := newCP(t, cfg)
			vals := repeat(level, settleSamples)
			vals = append(vals, spike)                        // one balloon
			vals = append(vals, repeat(level, 6)...)          // reverts
			stepAt := len(vals)                               //
			vals = append(vals, repeat(level+4*sigma, 60)...) // genuine 4-sigma regime change

			fires := feed(t, c, vals)
			if len(fires) == 0 {
				ck := c.Checkpoint()
				t.Fatalf("spike of %v blinded the detector: the following 4-sigma regime change never fired (mean=%v vari=%v)",
					spike, ck.Mean, ck.Vari)
			}
			if got := fires[0].index; got <= stepAt {
				t.Fatalf("fired at sample %d, before the step at %d", got, stepAt)
			}
			if fires[0].dir != +1 {
				t.Fatalf("direction = %d, want +1", fires[0].dir)
			}
			if delay := fires[0].index - stepAt; delay > analyticDelay(cfg, 4)+3 {
				t.Fatalf("regime change took %d samples to detect, want ~%d", delay, analyticDelay(cfg, 4))
			}
		})
	}
}

// TestChangepointRearmHoldoff: after firing, the detector re-enters warmup so
// it re-learns the new level instead of firing repeatedly on the same shift.
func TestChangepointRearmHoldoff(t *testing.T) {
	cfg := analyticCPConfig()
	const level = 1000.0
	sigma := cfg.MinSigmaFrac * level

	c := newCP(t, cfg)
	vals := repeat(level, settleSamples)
	vals = append(vals, repeat(level+8*sigma, 200)...)
	fires := feed(t, c, vals)

	if len(fires) == 0 {
		t.Fatal("step never fired")
	}
	for i := 1; i < len(fires); i++ {
		if gap := fires[i].index - fires[i-1].index; gap <= cfg.Warmup {
			t.Fatalf("fires %d and %d are %d samples apart, holdoff is %d",
				fires[i-1].index, fires[i].index, gap, cfg.Warmup)
		}
	}
	// One sustained step is one regime change, not a stream of them.
	if len(fires) > 3 {
		t.Fatalf("a single sustained step fired %d times: %+v", len(fires), fires)
	}
}

func TestChangepointGarbageSamplesAreDropped(t *testing.T) {
	garbage := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), -1, -1e9,
		maxAbsSample * 10, math.MaxFloat64,
	}
	for _, g := range garbage {
		c := newCP(t, DefaultChangepointConfig())
		// Establish a real baseline first.
		clean := repeat(1000.0, settleSamples+DefaultChangepointConfig().Warmup)
		feed(t, c, clean)
		before := c.Checkpoint()

		for i := 0; i < 100; i++ {
			fired, dir := c.Add(cpStart, g)
			if fired || dir != 0 {
				t.Fatalf("garbage %v fired the detector", g)
			}
		}
		after := c.Checkpoint()

		if c.Dropped() != 100 {
			t.Fatalf("garbage %v: Dropped = %d, want 100", g, c.Dropped())
		}
		if after.N != before.N {
			t.Fatalf("garbage %v advanced N: %d -> %d", g, before.N, after.N)
		}
		if after.Mean != before.Mean || after.Vari != before.Vari {
			t.Fatalf("garbage %v moved the baseline: %v/%v -> %v/%v",
				g, before.Mean, before.Vari, after.Mean, after.Vari)
		}
		if after.WarmupLeft != before.WarmupLeft {
			t.Fatalf("garbage %v advanced warmup: %d -> %d", g, before.WarmupLeft, after.WarmupLeft)
		}
		if after.CusumPos != before.CusumPos || after.CusumNeg != before.CusumNeg {
			t.Fatalf("garbage %v moved a CUSUM", g)
		}
	}
}

func TestChangepointZeroIsAValidSample(t *testing.T) {
	c := newCP(t, DefaultChangepointConfig())
	feed(t, c, repeat(0.0, 50))
	if c.N() != 50 || c.Dropped() != 0 {
		t.Fatalf("N=%d Dropped=%d, want 50/0: zero usage is real, not garbage", c.N(), c.Dropped())
	}
}

// TestChangepointCheckpointRoundTrip: a restored detector must be
// indistinguishable from the original — same fires, same state — or a brain
// restart silently changes the engine's judgment.
func TestChangepointCheckpointRoundTrip(t *testing.T) {
	cfg := analyticCPConfig()
	const level = 1000.0
	sigma := cfg.MinSigmaFrac * level

	prefix := repeat(level, settleSamples)
	prefix = append(prefix, repeat(level+3*sigma, 20)...)
	prefix = append(prefix, math.NaN(), -5) // ensure Dropped is non-zero
	suffix := repeat(level+3*sigma, 40)
	suffix = append(suffix, repeat(level, 60)...)

	original := newCP(t, cfg)
	feed(t, original, prefix)

	ck := original.Checkpoint()
	restored, err := ChangepointFromCheckpoint(ck)
	if err != nil {
		t.Fatalf("round trip rejected its own checkpoint: %v", err)
	}
	if restored.N() != original.N() {
		t.Fatalf("N = %d, want %d", restored.N(), original.N())
	}
	if restored.Dropped() != original.Dropped() {
		t.Fatalf("Dropped = %d, want %d (observability counter lost across restart)",
			restored.Dropped(), original.Dropped())
	}
	od, ook := original.Last()
	rd, rok := restored.Last()
	if od != rd || ook != rok {
		t.Fatalf("Last() = %+v/%v, want %+v/%v", rd, rok, od, ook)
	}

	// Continuing both must produce byte-identical behavior.
	wantFires := feed(t, original, suffix)
	gotFires := feed(t, restored, suffix)
	if len(wantFires) != len(gotFires) {
		t.Fatalf("fire count %d != %d after restore", len(gotFires), len(wantFires))
	}
	for i := range wantFires {
		if gotFires[i] != wantFires[i] {
			t.Fatalf("fire %d: %+v, want %+v", i, gotFires[i], wantFires[i])
		}
	}
	if original.Checkpoint() != restored.Checkpoint() {
		t.Fatalf("final state diverged:\n got %+v\nwant %+v", restored.Checkpoint(), original.Checkpoint())
	}
}

func TestChangepointFromCheckpointRejectsCorruptState(t *testing.T) {
	base := func() ChangepointCheckpoint {
		c := newCP(t, analyticCPConfig())
		feed(t, c, repeat(1000.0, settleSamples+5))
		return c.Checkpoint()
	}
	if _, err := ChangepointFromCheckpoint(base()); err != nil {
		t.Fatalf("clean checkpoint rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ChangepointCheckpoint)
	}{
		{"invalid config", func(c *ChangepointCheckpoint) { c.Config.Alpha = 0 }},
		{"negative n", func(c *ChangepointCheckpoint) { c.N = -1 }},
		{"negative warmupLeft", func(c *ChangepointCheckpoint) { c.WarmupLeft = -1 }},
		{"warmupLeft above configured warmup", func(c *ChangepointCheckpoint) { c.WarmupLeft = c.Config.Warmup + 1 }},
		{"NaN mean", func(c *ChangepointCheckpoint) { c.Mean = math.NaN() }},
		{"Inf mean", func(c *ChangepointCheckpoint) { c.Mean = math.Inf(1) }},
		{"negative mean", func(c *ChangepointCheckpoint) { c.Mean = -1 }},
		{"absurd mean", func(c *ChangepointCheckpoint) { c.Mean = maxAbsSample * 10 }},
		{"NaN vari", func(c *ChangepointCheckpoint) { c.Vari = math.NaN() }},
		{"negative vari", func(c *ChangepointCheckpoint) { c.Vari = -1 }},
		{"Inf vari", func(c *ChangepointCheckpoint) { c.Vari = math.Inf(1) }},
		{"cusumPos above threshold", func(c *ChangepointCheckpoint) { c.CusumPos = c.Config.ThresholdH + 0.1 }},
		{"cusumNeg above threshold", func(c *ChangepointCheckpoint) { c.CusumNeg = c.Config.ThresholdH + 0.1 }},
		{"negative cusumPos", func(c *ChangepointCheckpoint) { c.CusumPos = -1 }},
		{"NaN cusumNeg", func(c *ChangepointCheckpoint) { c.CusumNeg = math.NaN() }},
		{"negative posRun", func(c *ChangepointCheckpoint) { c.PosRun = -1 }},
		{"negative negRun", func(c *ChangepointCheckpoint) { c.NegRun = -1 }},
		{"fired with zero direction", func(c *ChangepointCheckpoint) {
			c.Fired, c.Last = true, Detection{Direction: 0, MagnitudeSigma: 1}
		}},
		{"fired with bogus direction", func(c *ChangepointCheckpoint) {
			c.Fired, c.Last = true, Detection{Direction: 7, MagnitudeSigma: 1}
		}},
		{"fired with NaN magnitude", func(c *ChangepointCheckpoint) {
			c.Fired, c.Last = true, Detection{Direction: 1, MagnitudeSigma: math.NaN()}
		}},
		{"fired with negative magnitude", func(c *ChangepointCheckpoint) {
			c.Fired, c.Last = true, Detection{Direction: 1, MagnitudeSigma: -1}
		}},
		{"negative dropped", func(c *ChangepointCheckpoint) { c.Dropped = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := base()
			tc.mutate(&ck)
			got, err := ChangepointFromCheckpoint(ck)
			if err == nil {
				t.Fatalf("accepted corrupt checkpoint %+v", ck)
			}
			if got != nil {
				t.Fatal("returned a detector alongside an error")
			}
		})
	}
}

// TestChangepointDeterminism: the same trace fed to two detectors must
// produce identical fires and identical final state. No clock, no map order,
// no hidden state.
func TestChangepointDeterminism(t *testing.T) {
	cfg := DefaultChangepointConfig()
	rng := newLCG(0xC0FFEE)
	vals := make([]float64, 4000)
	for i := range vals {
		lvl := 1000.0
		if i > 2000 {
			lvl = 1600.0
		}
		vals[i] = lvl + 100*rng.normal()
		if vals[i] < 0 {
			vals[i] = 0
		}
	}
	a, b := newCP(t, cfg), newCP(t, cfg)
	fa, fb := feed(t, a, vals), feed(t, b, vals)
	if len(fa) != len(fb) {
		t.Fatalf("fire counts differ: %d vs %d", len(fa), len(fb))
	}
	for i := range fa {
		if fa[i] != fb[i] {
			t.Fatalf("fire %d differs: %+v vs %+v", i, fa[i], fb[i])
		}
	}
	if a.Checkpoint() != b.Checkpoint() {
		t.Fatal("final state differs across identical runs")
	}
}

// TestChangepointRegimeChangeInNoiseIsFound: the realistic end-to-end case —
// a doubled memory level buried in 10% noise must be caught promptly, and
// the pre-change stretch must stay quiet enough to be usable.
func TestChangepointRegimeChangeInNoiseIsFound(t *testing.T) {
	cfg := DefaultChangepointConfig()
	const changeAt = 1500
	rng := newLCG(0x5EED)
	vals := make([]float64, 3000)
	for i := range vals {
		lvl := 1000.0
		if i >= changeAt {
			lvl = 2000.0
		}
		vals[i] = math.Max(0, lvl+0.10*lvl*rng.normal())
	}
	c := newCP(t, cfg)
	fires := feed(t, c, vals)

	var firstAfter *fireEvent
	falseAlarms := 0
	for i := range fires {
		if fires[i].index <= changeAt {
			falseAlarms++
			continue
		}
		if firstAfter == nil {
			f := fires[i]
			firstAfter = &f
		}
	}
	if firstAfter == nil {
		t.Fatal("a 10-sigma sustained doubling was never detected")
	}
	if firstAfter.dir != +1 {
		t.Fatalf("direction = %d, want +1", firstAfter.dir)
	}
	// ZClamp caps per-sample accumulation, so even an enormous shift needs
	// ceil(H/(ZClamp-DriftK)) samples; allow generous slack for noise.
	if delay := firstAfter.index - changeAt; delay > 20 {
		t.Fatalf("detected %d samples after the change, want <= 20", delay)
	}
	// A stationary stretch may false-alarm (that is CUSUM's ARL0), but a
	// rate this high would make the signal useless. 1500 samples is ~5
	// days at 5-min cadence.
	if falseAlarms > 8 {
		t.Fatalf("%d false alarms in %d stationary samples", falseAlarms, changeAt)
	}
}

// TestChangepointTimestampsAreRecordedNotUsed: t is carried into Detection
// but never used for math, so out-of-order timestamps cannot corrupt state.
func TestChangepointTimestampsAreRecordedNotUsed(t *testing.T) {
	cfg := analyticCPConfig()
	const level = 1000.0
	sigma := cfg.MinSigmaFrac * level
	vals := repeat(level, settleSamples)
	vals = append(vals, repeat(level+8*sigma, 30)...)

	ordered := newCP(t, cfg)
	shuffled := newCP(t, cfg)
	for i, v := range vals {
		ordered.Add(cpStart.Add(time.Duration(i)*cpStep), v)
		// Deliberately pathological clock: backwards, zero, far future.
		var ts time.Time
		switch i % 3 {
		case 0:
			ts = cpStart.Add(-time.Duration(i) * cpStep)
		case 1:
			ts = time.Time{}
		default:
			ts = cpStart.Add(1 << 40)
		}
		shuffled.Add(ts, v)
	}
	a, b := ordered.Checkpoint(), shuffled.Checkpoint()
	a.Last.At, b.Last.At = time.Time{}, time.Time{} // At is the only difference allowed
	if a != b {
		t.Fatalf("timestamp order changed detector state:\n got %+v\nwant %+v", b, a)
	}
}

// --- deterministic noise source -------------------------------------------

// lcg is a deterministic PRNG so the noise traces above are reproducible on
// every machine and every run. Nothing here feeds production code.
type lcg struct{ s uint64 }

func newLCG(seed uint64) *lcg { return &lcg{s: seed} }

func (l *lcg) uniform() float64 {
	l.s = l.s*6364136223846793005 + 1442695040888963407
	return float64(l.s>>11) / float64(uint64(1)<<53)
}

// normal returns an approximately standard-normal deviate (Irwin-Hall).
func (l *lcg) normal() float64 {
	sum := 0.0
	for i := 0; i < 12; i++ {
		sum += l.uniform()
	}
	return sum - 6
}

// --- fuzz ------------------------------------------------------------------

// fuzzCPConfigs are the parameter sets the fuzzer rotates through, spanning
// the validated ranges (fast/slow baseline, tight/loose threshold).
var fuzzCPConfigs = []ChangepointConfig{
	DefaultChangepointConfig(),
	analyticCPConfig(),
	{Alpha: 0.5, DriftK: 0.05, ThresholdH: 100, ZClamp: 100, Warmup: 8, MinSigmaFrac: 1},
	{Alpha: 0.5, DriftK: 5, ThresholdH: 5.1, ZClamp: 10, Warmup: 8, MinSigmaFrac: 1e-9},
	{Alpha: 0.01, DriftK: 0.25, ThresholdH: 8, ZClamp: 6, Warmup: 100, MinSigmaFrac: 0.01},
}

// FuzzChangepointAdd asserts the detector's state invariants survive any
// byte sequence reinterpreted as float64 samples: no panic, no non-finite
// state, CUSUMs inside [0,H], a coherent fired/direction pair, and — the
// strongest one — that a live detector's Checkpoint is always accepted back
// by ChangepointFromCheckpoint. If that ever fails, a brain restart would
// reject its own persisted state.
func FuzzChangepointAdd(f *testing.F) {
	seed := func(vals ...float64) []byte {
		b := make([]byte, 0, 8*len(vals))
		for _, v := range vals {
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
			b = append(b, buf[:]...)
		}
		return b
	}
	f.Add(seed(1000, 1000, 1000, 1000, 2000, 2000, 2000))
	f.Add(seed(math.NaN(), math.Inf(1), math.Inf(-1), -1, 0))
	f.Add(seed(maxAbsSample, 0, maxAbsSample, 0, maxAbsSample))
	f.Add(seed(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1e-300))
	f.Add([]byte{})
	f.Add([]byte{0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := fuzzCPConfigs[0]
		if len(data) > 0 {
			cfg = fuzzCPConfigs[int(data[0])%len(fuzzCPConfigs)]
		}
		c, err := NewChangepoint(cfg)
		if err != nil {
			t.Fatalf("preset config %d rejected: %v", int(data[0])%len(fuzzCPConfigs), err)
		}

		const maxSamples = 4096
		adds := 0
		for i := 0; i+8 <= len(data) && adds < maxSamples; i += 8 {
			v := math.Float64frombits(binary.LittleEndian.Uint64(data[i : i+8]))
			fired, dir := c.Add(cpStart.Add(time.Duration(adds)*cpStep), v)
			adds++

			if fired != (dir != 0) {
				t.Fatalf("fired=%v but direction=%d", fired, dir)
			}
			if dir != 0 && dir != 1 && dir != -1 {
				t.Fatalf("direction = %d, want -1, 0 or +1", dir)
			}

			ck := c.Checkpoint()
			if !(ck.Mean >= 0 && ck.Mean <= maxAbsSample) {
				t.Fatalf("mean escaped [0,%v]: %v (after v=%v)", maxAbsSample, ck.Mean, v)
			}
			if !(ck.Vari >= 0) || math.IsInf(ck.Vari, 0) {
				t.Fatalf("vari not finite and non-negative: %v (after v=%v)", ck.Vari, v)
			}
			if !(ck.CusumPos >= 0 && ck.CusumPos <= cfg.ThresholdH) {
				t.Fatalf("cusumPos escaped [0,%v]: %v", cfg.ThresholdH, ck.CusumPos)
			}
			if !(ck.CusumNeg >= 0 && ck.CusumNeg <= cfg.ThresholdH) {
				t.Fatalf("cusumNeg escaped [0,%v]: %v", cfg.ThresholdH, ck.CusumNeg)
			}
			if ck.PosRun < 0 || ck.NegRun < 0 || ck.N < 0 || ck.Dropped < 0 {
				t.Fatalf("negative counter in %+v", ck)
			}
			if ck.WarmupLeft < 0 || ck.WarmupLeft > cfg.Warmup {
				t.Fatalf("warmupLeft %d escaped [0,%d]", ck.WarmupLeft, cfg.Warmup)
			}
			if ck.N+ck.Dropped != adds {
				t.Fatalf("N(%d)+Dropped(%d) != Add calls(%d)", ck.N, ck.Dropped, adds)
			}
			if ck.Fired {
				if ck.Last.Direction != 1 && ck.Last.Direction != -1 {
					t.Fatalf("fired detection has direction %d", ck.Last.Direction)
				}
				if !(ck.Last.MagnitudeSigma >= 0) || math.IsInf(ck.Last.MagnitudeSigma, 0) {
					t.Fatalf("fired detection has magnitude %v", ck.Last.MagnitudeSigma)
				}
			}
			// The state a live detector reports must always be restorable.
			if _, err := ChangepointFromCheckpoint(ck); err != nil {
				t.Fatalf("live state rejected by its own restorer: %v (%+v)", err, ck)
			}
		}
	})
}
