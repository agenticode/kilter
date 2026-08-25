package decision

import (
	"fmt"
	"math"
	"testing"
	"time"
)

var floorNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

const (
	week = 7 * 24 * time.Hour
	day  = 24 * time.Hour
)

func TestDefaultFloorConfig(t *testing.T) {
	c := DefaultFloorConfig()
	if c.Hold != 14*day {
		t.Fatalf("Hold = %v, want 14d", c.Hold)
	}
	if c.DecayPerWeek != 0.10 {
		t.Fatalf("DecayPerWeek = %v, want 0.10", c.DecayPerWeek)
	}
	// The documented "halves its excess in ~6.6 weeks" claim must be true.
	halfLife := math.Log(0.5) / math.Log(1-c.DecayPerWeek)
	if halfLife < 6.3 || halfLife > 6.9 {
		t.Fatalf("excess half-life is %.2f weeks, doc claims ~6.6", halfLife)
	}
}

func TestEffectiveOOMFloor(t *testing.T) {
	cfg := DefaultFloorConfig() // Hold 14d, 10%/week
	const floor = 2000
	const observed = 1000

	// After Hold, the 1000-byte gap decays at 10%/week, ceil-rounded.
	afterWeeks := func(w float64) int64 {
		return int64(math.Ceil(observed + (floor-observed)*math.Pow(0.9, w)))
	}

	cases := []struct {
		name     string
		floorB   int64
		lastOOM  time.Time
		observed int64
		want     int64
	}{
		{"no floor armed", 0, floorNow.Add(-time.Hour), observed, 0},
		{"negative floor", -5, floorNow.Add(-time.Hour), observed, 0},
		{"no OOM ever", floor, time.Time{}, observed, 0},
		{"fresh OOM holds full floor", floor, floorNow, observed, floor},
		{"mid-hold holds full floor", floor, floorNow.Add(-7 * day), observed, floor},
		{"exactly at hold boundary", floor, floorNow.Add(-14 * day), observed, floor},
		{"one week past hold", floor, floorNow.Add(-14*day - week), observed, afterWeeks(1)},
		{"two weeks past hold", floor, floorNow.Add(-14*day - 2*week), observed, afterWeeks(2)},
		{"half a week past hold decays continuously", floor, floorNow.Add(-14*day - week/2), observed, afterWeeks(0.5)},
		{"ten weeks past hold", floor, floorNow.Add(-14*day - 10*week), observed, afterWeeks(10)},
		{"ancient OOM relaxes to observed", floor, floorNow.Add(-10 * 365 * day), observed, observed},
		{"floor already below observed is inert", 500, floorNow.Add(-100 * day), observed, 500},
		{"floor equal to observed is inert", observed, floorNow.Add(-100 * day), observed, observed},
		{"clock skew (OOM in the future) holds full floor", floor, floorNow.Add(48 * time.Hour), observed, floor},
		{"negative observed clamps to zero", floor, floorNow.Add(-14*day - week), -500,
			int64(math.Ceil(0 + float64(floor)*math.Pow(0.9, 1)))},
		{"zero observed decays toward zero", floor, floorNow.Add(-14*day - week), 0,
			int64(math.Ceil(float64(floor) * math.Pow(0.9, 1)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveOOMFloor(tc.floorB, tc.lastOOM, floorNow, tc.observed, cfg)
			if got != tc.want {
				t.Fatalf("EffectiveOOMFloor = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveOOMFloorGarbageConfigFallsBackToDefaults(t *testing.T) {
	good := DefaultFloorConfig()
	const floor, observed = 2000, 1000
	lastOOM := floorNow.Add(-14*day - week)
	want := EffectiveOOMFloor(floor, lastOOM, floorNow, observed, good)

	garbage := []FloorConfig{
		{},                                          // zero
		{Hold: -time.Hour, DecayPerWeek: 0.10},      // negative hold
		{Hold: 14 * day, DecayPerWeek: 0},           // no decay
		{Hold: 14 * day, DecayPerWeek: 1},           // total decay
		{Hold: 14 * day, DecayPerWeek: 1.5},         // >1
		{Hold: 14 * day, DecayPerWeek: -0.5},        // negative
		{Hold: 14 * day, DecayPerWeek: math.NaN()},  // NaN
		{Hold: 14 * day, DecayPerWeek: math.Inf(1)}, // +Inf
		{Hold: -1, DecayPerWeek: math.NaN()},        // both bad
	}
	for i, cfg := range garbage {
		t.Run(fmt.Sprintf("garbage/%d", i), func(t *testing.T) {
			got := EffectiveOOMFloor(floor, lastOOM, floorNow, observed, cfg)
			if got != want {
				t.Fatalf("garbage config %+v gave %d, want the default-config result %d", cfg, got, want)
			}
		})
	}
}

// TestEffectiveOOMFloorBounds is the safety invariant: whatever the inputs,
// the returned floor is either 0 (disarmed) or inside [max(observed,0),
// floorBytes]. A floor that escaped below the observed peak would silently
// under-provision memory after an OOM.
func TestEffectiveOOMFloorBounds(t *testing.T) {
	floors := []int64{0, -1, 1, 1 << 20, 1 << 40, math.MaxInt64, math.MaxInt64 - 1, math.MinInt64}
	observeds := []int64{-1 << 40, -1, 0, 1, 1 << 20, 1 << 40, math.MaxInt64}
	ages := []time.Duration{
		-1000 * day, 0, time.Hour, 14 * day, 14*day + week,
		365 * day, 100 * 365 * day, time.Duration(math.MaxInt64),
	}
	cfgs := []FloorConfig{DefaultFloorConfig(), {}, {Hold: time.Nanosecond, DecayPerWeek: 0.999}}

	for _, f := range floors {
		for _, o := range observeds {
			for _, age := range ages {
				for _, cfg := range cfgs {
					lastOOM := floorNow.Add(-age)
					got := EffectiveOOMFloor(f, lastOOM, floorNow, o, cfg)
					if f <= 0 {
						if got != 0 {
							t.Fatalf("disarmed floor %d returned %d", f, got)
						}
						continue
					}
					lo := o
					if lo < 0 {
						lo = 0
					}
					if got > f {
						t.Fatalf("floor=%d observed=%d age=%v cfg=%+v: got %d > armed floor",
							f, o, age, cfg, got)
					}
					if got < lo && got != f {
						t.Fatalf("floor=%d observed=%d age=%v cfg=%+v: got %d < observed floor %d",
							f, o, age, cfg, got, lo)
					}
				}
			}
		}
	}
}

// TestEffectiveOOMFloorIsMonotonicInAge: the floor may only relax as the OOM
// recedes. A non-monotonic schedule would make memory sizing oscillate.
func TestEffectiveOOMFloorIsMonotonicInAge(t *testing.T) {
	cfg := DefaultFloorConfig()
	const floor, observed = 4 << 30, 1 << 30
	prev := int64(math.MaxInt64)
	for h := 0; h <= 200*24; h += 3 {
		age := time.Duration(h) * time.Hour
		got := EffectiveOOMFloor(floor, floorNow.Add(-age), floorNow, observed, cfg)
		if got > prev {
			t.Fatalf("floor rose from %d to %d as age reached %v", prev, got, age)
		}
		prev = got
	}
	if prev >= floor {
		t.Fatalf("floor never relaxed below the armed value %d after 200 days", floor)
	}
}

// TestEffectiveOOMFloorHugeArmedFloor: an armed floor near the int64 ceiling
// must not lose precision through the decay arithmetic into a *lower* floor
// than the evidence supports. Memory floors must fail conservative (high),
// never low. This guards the integer-gap formulation against regressing to a
// float64 absolute-floor computation, whose out-of-range int64 conversion is
// implementation-defined (arm64 saturates high, amd64 wraps to MinInt64).
func TestEffectiveOOMFloorHugeArmedFloor(t *testing.T) {
	cfg := DefaultFloorConfig()
	const observed = 1 << 30
	for _, f := range []int64{math.MaxInt64, math.MaxInt64 - 1, 1 << 62, (1 << 62) + 12345} {
		// Just past Hold: essentially no decay has happened yet, so the
		// answer must still be almost the whole armed floor.
		lastOOM := floorNow.Add(-14*day - time.Minute)
		got := EffectiveOOMFloor(f, lastOOM, floorNow, observed, cfg)
		if got > f {
			t.Fatalf("floor=%d: got %d above the armed floor", f, got)
		}
		if got < f/2 {
			t.Fatalf("floor=%d one minute past hold collapsed to %d (want ~%d); "+
				"float64 conversion lost the floor", f, got, f)
		}
	}
}

func TestSustainedPeak(t *testing.T) {
	cases := []struct {
		name string
		vals []float64
		run  int
		want float64
	}{
		{"empty", nil, 3, 0},
		{"all garbage", []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300}, 3, 0},
		{"single value run 1", []float64{7}, 1, 7},
		{"run 1 is plain max", []float64{1, 9, 2}, 1, 9},
		// The headline property: a one-sample balloon cannot set the peak.
		{"one-sample balloon rejected", []float64{10, 10, 999, 10, 10, 10}, 3, 10},
		{"two-sample balloon rejected at run 3", []float64{10, 10, 999, 999, 10, 10}, 3, 10},
		{"three-sample plateau accepted at run 3", []float64{10, 10, 999, 999, 999, 10}, 3, 999},
		{"plateau at the start", []float64{50, 50, 50, 1, 1}, 3, 50},
		{"plateau at the end", []float64{1, 1, 50, 50, 50}, 3, 50},
		{"window min is the level held", []float64{100, 80, 90}, 3, 80},
		{"run longer than series shrinks to whole-series min", []float64{5, 9, 7}, 99, 5},
		{"run zero behaves as 1", []float64{1, 9, 2}, 0, 9},
		{"run negative behaves as 1", []float64{1, 9, 2}, -5, 9},
		{"negatives clamp to zero", []float64{-5, -5, -5}, 2, 0},
		{"garbage dropped, neighbors join", []float64{10, math.NaN(), 10, math.NaN(), 10}, 3, 10},
		{"absurd magnitude dropped", []float64{10, 10, 1e300, 10, 10}, 3, 10},
		{"exact maxAbsSample is accepted", []float64{maxAbsSample, maxAbsSample}, 2, maxAbsSample},
		{"all zeros", []float64{0, 0, 0}, 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SustainedPeak(tc.vals, tc.run); got != tc.want {
				t.Fatalf("SustainedPeak(%v, %d) = %v, want %v", tc.vals, tc.run, got, tc.want)
			}
		})
	}
}

// TestSustainedPeakRunOneEqualsMax pins the run==1 degenerate case against an
// independent implementation.
func TestSustainedPeakRunOneEqualsMax(t *testing.T) {
	rng := newLCG(0xBEEF)
	for trial := 0; trial < 200; trial++ {
		n := 1 + trial%40
		vals := make([]float64, n)
		want := 0.0
		for i := range vals {
			vals[i] = rng.uniform()*2000 - 500 // includes negatives
			clamped := vals[i]
			if clamped < 0 {
				clamped = 0
			}
			if clamped > want {
				want = clamped
			}
		}
		if got := SustainedPeak(vals, 1); got != want {
			t.Fatalf("trial %d: SustainedPeak(run=1) = %v, want max %v", trial, got, want)
		}
	}
}

// TestSustainedPeakIsNonIncreasingInRun: demanding a longer plateau can only
// lower (never raise) the verified sustained peak.
func TestSustainedPeakIsNonIncreasingInRun(t *testing.T) {
	rng := newLCG(0xF00D)
	for trial := 0; trial < 100; trial++ {
		vals := make([]float64, 60)
		for i := range vals {
			vals[i] = rng.uniform() * 1000
		}
		prev := math.Inf(1)
		for run := 1; run <= 70; run++ {
			got := SustainedPeak(vals, run)
			if got > prev {
				t.Fatalf("trial %d: run %d gave %v, higher than run %d's %v", trial, run, got, run-1, prev)
			}
			prev = got
		}
	}
}

// TestSustainedPeakNeverExceedsMax: whatever the input, the answer is a level
// the series actually reached (or 0).
func TestSustainedPeakNeverExceedsMax(t *testing.T) {
	rng := newLCG(0xABCD)
	garbage := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300, -1e300, maxAbsSample * 2}
	for trial := 0; trial < 300; trial++ {
		n := trial%50 + 1
		vals := make([]float64, n)
		max := 0.0
		for i := range vals {
			if int(rng.uniform()*4) == 0 {
				vals[i] = garbage[int(rng.uniform()*float64(len(garbage)))%len(garbage)]
				continue
			}
			vals[i] = rng.uniform() * 1e6
			if vals[i] > max {
				max = vals[i]
			}
		}
		for _, run := range []int{-1, 0, 1, 2, 5, n, n + 10} {
			got := SustainedPeak(vals, run)
			if !(got >= 0) || got > max {
				t.Fatalf("trial %d run %d: SustainedPeak = %v, outside [0,%v]", trial, run, got, max)
			}
		}
	}
}

func TestSustainedPeakDoesNotMutateInput(t *testing.T) {
	vals := []float64{-3, math.NaN(), 5, 1e300, 7}
	before := append([]float64(nil), vals...)
	SustainedPeak(vals, 2)
	for i := range vals {
		if math.IsNaN(before[i]) && math.IsNaN(vals[i]) {
			continue
		}
		if vals[i] != before[i] {
			t.Fatalf("input mutated at %d: %v -> %v", i, before[i], vals[i])
		}
	}
}

func TestRobustPeak(t *testing.T) {
	cases := []struct {
		name       string
		hist, sust float64
		want       float64
	}{
		{"histogram wins", 500, 300, 500},
		{"sustained wins", 300, 500, 500},
		{"equal", 400, 400, 400},
		{"both zero", 0, 0, 0},
		{"NaN histogram contributes zero", math.NaN(), 300, 300},
		{"NaN sustained contributes zero", 300, math.NaN(), 300},
		{"both NaN", math.NaN(), math.NaN(), 0},
		{"+Inf contributes zero", math.Inf(1), 300, 300},
		{"-Inf contributes zero", math.Inf(-1), 300, 300},
		{"negative contributes zero", -900, 300, 300},
		{"absurd magnitude contributes zero", 1e300, 300, 300},
		{"exact maxAbsSample is kept", maxAbsSample, 300, maxAbsSample},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RobustPeak(tc.hist, tc.sust); got != tc.want {
				t.Fatalf("RobustPeak(%v, %v) = %v, want %v", tc.hist, tc.sust, got, tc.want)
			}
		})
	}
}

// TestRobustPeakIsNeverGarbage: the output feeds a memory target, so it must
// always be a usable finite non-negative number.
func TestRobustPeakIsNeverGarbage(t *testing.T) {
	vals := []float64{
		math.NaN(), math.Inf(1), math.Inf(-1), -1e300, -1, 0, 1, 1e6,
		maxAbsSample, maxAbsSample * 2, math.MaxFloat64, math.SmallestNonzeroFloat64,
	}
	for _, a := range vals {
		for _, b := range vals {
			got := RobustPeak(a, b)
			if !(got >= 0 && got <= maxAbsSample) {
				t.Fatalf("RobustPeak(%v, %v) = %v, outside [0,%v]", a, b, got, maxAbsSample)
			}
		}
	}
}

// FuzzSustainedPeak: no byte sequence may make the spike-robust peak return a
// non-finite, negative, or impossible value, or panic.
func FuzzSustainedPeak(f *testing.F) {
	f.Add([]byte{}, 3)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 1)
	f.Add(make([]byte, 64), 0)
	f.Fuzz(func(t *testing.T, data []byte, run int) {
		const maxVals = 512
		vals := make([]float64, 0, len(data)/8)
		for i := 0; i+8 <= len(data) && len(vals) < maxVals; i += 8 {
			var u uint64
			for k := 0; k < 8; k++ {
				u |= uint64(data[i+k]) << (8 * k)
			}
			vals = append(vals, math.Float64frombits(u))
		}
		// Keep run inside a sane band; the caller's own contract is a
		// sample count, and an unbounded one only burns fuzz time.
		if run > maxVals+8 {
			run = maxVals + 8
		}
		if run < -8 {
			run = -8
		}

		got := SustainedPeak(vals, run)
		if !(got >= 0 && got <= maxAbsSample) {
			t.Fatalf("SustainedPeak(%d vals, run=%d) = %v, outside [0,%v]", len(vals), run, got, maxAbsSample)
		}
		// The answer must be a level some accepted sample actually reached.
		if got > 0 {
			seen := false
			for _, v := range vals {
				if v == got {
					seen = true
					break
				}
			}
			if !seen {
				t.Fatalf("SustainedPeak returned %v, which is not any input sample", got)
			}
		}
		if rp := RobustPeak(got, got); !(rp >= 0 && rp <= maxAbsSample) {
			t.Fatalf("RobustPeak(%v,%v) = %v", got, got, rp)
		}
	})
}
