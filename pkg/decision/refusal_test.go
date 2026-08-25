package decision

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/patterns"
)

// refNow is the fixed decision instant every refusal test evaluates at.
// Pure functions take the clock as an argument, so the tests can too.
var refNow = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

// cleanEvidence is a subject with no grounds for refusal under
// DefaultConfig at refNow. Every predicate test starts here and breaks
// exactly one thing, so a failure names the predicate that misfired.
func cleanEvidence() Evidence {
	return Evidence{
		Samples:             100,
		Window:              48 * time.Hour,
		LastSample:          refNow,
		Class:               patterns.ClassSteady,
		ClassStabilityKnown: true,
		ClassStability:      0.95,
		ShrinkIndicated:     true,
		HPAThrashPerHour:    0.1,
		BuiltinForecast:     100,
		RemoteForecast:      105,
	}
}

func TestEvaluateCleanEvidenceHasNoGrounds(t *testing.T) {
	if r := Evaluate(cleanEvidence(), DefaultConfig(), refNow); r != nil {
		t.Fatalf("clean evidence refused: %+v", r)
	}
}

func TestSoakFor(t *testing.T) {
	base := 6 * time.Hour
	cases := []struct {
		name  string
		class patterns.Class
		base  time.Duration
		want  time.Duration
	}{
		{"steady x1", patterns.ClassSteady, base, 6 * time.Hour},
		{"unknown x1", patterns.ClassUnknown, base, 6 * time.Hour},
		{"empty class x1", patterns.Class(""), base, 6 * time.Hour},
		{"growing x1", patterns.ClassGrowing, base, 6 * time.Hour},
		{"bursty x2", patterns.ClassBursty, base, 12 * time.Hour},
		{"diurnal x4", patterns.ClassDiurnal, base, 24 * time.Hour},
		{"batch x4", patterns.ClassBatch, base, 24 * time.Hour},
		{"zero base falls back to default", patterns.ClassSteady, 0, DefaultConfig().BaseSoak},
		{"negative base falls back to default", patterns.ClassSteady, -time.Hour, DefaultConfig().BaseSoak},
		{"cap applies", patterns.ClassDiurnal, 100 * time.Hour, maxSoak},
		// A base large enough that base*4 overflows int64 nanoseconds must
		// still yield a sane, positive, capped soak — never a negative
		// duration, which would silently disable the refusal (fail-open).
		{"absurd base cannot overflow", patterns.ClassDiurnal, 100 * 365 * 24 * time.Hour, maxSoak},
		{"absurd base bursty cannot overflow", patterns.ClassBursty, 200 * 365 * 24 * time.Hour, maxSoak},
		{"max duration base cannot overflow", patterns.ClassBatch, time.Duration(math.MaxInt64), maxSoak},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SoakFor(tc.class, tc.base)
			if got != tc.want {
				t.Fatalf("SoakFor(%q, %v) = %v, want %v", tc.class, tc.base, got, tc.want)
			}
			if got <= 0 {
				t.Fatalf("SoakFor(%q, %v) = %v, must always be positive", tc.class, tc.base, got)
			}
			if got > maxSoak {
				t.Fatalf("SoakFor(%q, %v) = %v exceeds cap %v", tc.class, tc.base, got, maxSoak)
			}
		})
	}
}

// TestSoakForNeverNonPositive is the property behind the overflow cases: no
// base whatsoever may produce a non-positive or uncapped soak, because a
// non-positive soak makes RefusePostChangeSoak and RefuseRegimeChangePending
// unconditionally pass.
func TestSoakForNeverNonPositive(t *testing.T) {
	classes := []patterns.Class{
		patterns.ClassUnknown, patterns.ClassSteady, patterns.ClassDiurnal,
		patterns.ClassBursty, patterns.ClassBatch, patterns.ClassGrowing,
	}
	bases := []time.Duration{
		math.MinInt64, -time.Hour, 0, time.Nanosecond, time.Hour, 6 * time.Hour,
		maxSoak, maxSoak + 1, 1 << 60, 1 << 62, math.MaxInt64,
	}
	for _, c := range classes {
		for _, b := range bases {
			got := SoakFor(c, b)
			if !(got > 0) || got > maxSoak {
				t.Fatalf("SoakFor(%q, %v) = %v, want (0, %v]", c, b, got, maxSoak)
			}
		}
	}
}

func TestRefuseInsufficientHistory(t *testing.T) {
	cfg := DefaultConfig() // MinSamples 30, MinWindow 6h
	cases := []struct {
		name      string
		mutate    func(*Evidence)
		wantRef   bool
		wantUntil time.Time
	}{
		{"enough history", func(e *Evidence) {}, false, time.Time{}},
		{"exactly at both minimums", func(e *Evidence) {
			e.Samples, e.Window = 30, 6*time.Hour
		}, false, time.Time{}},
		{"one sample short", func(e *Evidence) {
			e.Samples, e.Window = 29, 6*time.Hour
			e.LastSample = time.Time{}
		}, true, time.Time{}},
		{"one nanosecond short of window", func(e *Evidence) {
			e.Window = 6*time.Hour - 1
			e.LastSample = time.Time{}
		}, true, time.Time{}},
		{"negative window is insufficient", func(e *Evidence) {
			e.Window = -time.Hour
			e.LastSample = time.Time{}
		}, true, time.Time{}},
		{"zero samples", func(e *Evidence) {
			e.Samples, e.Window = 0, 0
			e.LastSample = time.Time{}
		}, true, time.Time{}},
		// Until arithmetic: window short by 2h, samples fine.
		{"until from window shortfall", func(e *Evidence) {
			e.Samples, e.Window = 100, 4*time.Hour
			e.LastSample = refNow
		}, true, refNow.Add(2 * time.Hour)},
		// 11 samples over 10h => 1h/sample; 19 more needed => 19h,
		// which dominates the 0h window shortfall.
		{"until from sample rate", func(e *Evidence) {
			e.Samples, e.Window = 11, 10*time.Hour
			e.LastSample = refNow
		}, true, refNow.Add(19 * time.Hour)},
		{"single sample gives no rate estimate", func(e *Evidence) {
			e.Samples, e.Window = 1, 10*time.Hour
			e.LastSample = refNow
		}, true, time.Time{}},
		{"no last sample means no until", func(e *Evidence) {
			e.Samples, e.Window = 5, time.Hour
			e.LastSample = time.Time{}
		}, true, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			tc.mutate(&ev)
			got := RefuseInsufficientHistory(ev, cfg, refNow)
			if (got != nil) != tc.wantRef {
				t.Fatalf("refusal = %+v, want refusal=%v", got, tc.wantRef)
			}
			if got == nil {
				return
			}
			if got.Code != CodeInsufficientHistory {
				t.Fatalf("code = %q", got.Code)
			}
			if !got.Until.Equal(tc.wantUntil) {
				t.Fatalf("Until = %v, want %v", got.Until, tc.wantUntil)
			}
		})
	}
}

// TestRefuseInsufficientHistoryUntilIsCapped: an absurd MinSamples must not
// project a clearing time beyond the documented 30-day cap (and must not
// overflow time arithmetic).
func TestRefuseInsufficientHistoryUntilIsCapped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSamples = 1 << 40
	ev := cleanEvidence()
	ev.Samples, ev.Window, ev.LastSample = 3, time.Hour, refNow

	got := RefuseInsufficientHistory(ev, cfg, refNow)
	if got == nil {
		t.Fatal("want refusal")
	}
	if got.Until.After(refNow.Add(untilProjectionCap)) {
		t.Fatalf("Until = %v exceeds cap %v past now", got.Until, untilProjectionCap)
	}
	if got.Until.Before(refNow) {
		t.Fatalf("Until = %v is before now %v", got.Until, refNow)
	}
}

func TestRefusePostChangeSoak(t *testing.T) {
	cfg := DefaultConfig() // BaseSoak 6h
	cases := []struct {
		name    string
		class   patterns.Class
		change  time.Time
		wantRef bool
	}{
		{"no change event", patterns.ClassSteady, time.Time{}, false},
		{"change just now", patterns.ClassSteady, refNow, true},
		{"inside steady soak", patterns.ClassSteady, refNow.Add(-5 * time.Hour), true},
		{"exactly at steady soak", patterns.ClassSteady, refNow.Add(-6 * time.Hour), false},
		{"beyond steady soak", patterns.ClassSteady, refNow.Add(-7 * time.Hour), false},
		{"diurnal needs 24h: 7h is not enough", patterns.ClassDiurnal, refNow.Add(-7 * time.Hour), true},
		{"diurnal beyond 24h", patterns.ClassDiurnal, refNow.Add(-25 * time.Hour), false},
		{"bursty needs 12h", patterns.ClassBursty, refNow.Add(-11 * time.Hour), true},
		{"batch needs 24h", patterns.ClassBatch, refNow.Add(-23 * time.Hour), true},
		{"future change (clock skew) still refuses", patterns.ClassSteady, refNow.Add(time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			ev.Class, ev.LastChange = tc.class, tc.change
			got := RefusePostChangeSoak(ev, cfg, refNow)
			if (got != nil) != tc.wantRef {
				t.Fatalf("refusal = %+v, want refusal=%v", got, tc.wantRef)
			}
			if got == nil {
				return
			}
			if got.Code != CodePostChangeSoak {
				t.Fatalf("code = %q", got.Code)
			}
			if want := tc.change.Add(SoakFor(tc.class, cfg.BaseSoak)); !got.Until.Equal(want) {
				t.Fatalf("Until = %v, want %v", got.Until, want)
			}
			if !got.Until.After(refNow) {
				t.Fatalf("Until %v must be after now %v while the soak is active", got.Until, refNow)
			}
		})
	}
}

func TestRefuseClassUnstable(t *testing.T) {
	cfg := DefaultConfig() // ClassFlipWindow 24h, MinClassStability 0.7
	cases := []struct {
		name      string
		mutate    func(*Evidence)
		wantRef   bool
		wantUntil bool
	}{
		{"stable", func(e *Evidence) {}, false, false},
		{"flip just now", func(e *Evidence) { e.LastClassFlip = refNow }, true, true},
		{"flip 23h ago", func(e *Evidence) { e.LastClassFlip = refNow.Add(-23 * time.Hour) }, true, true},
		{"flip exactly 24h ago clears", func(e *Evidence) { e.LastClassFlip = refNow.Add(-24 * time.Hour) }, false, false},
		{"flip 25h ago clears", func(e *Evidence) { e.LastClassFlip = refNow.Add(-25 * time.Hour) }, false, false},
		{"future flip (skew) refuses", func(e *Evidence) { e.LastClassFlip = refNow.Add(time.Hour) }, true, true},
		{"stability below minimum", func(e *Evidence) { e.ClassStability = 0.5 }, true, false},
		{"stability exactly at minimum passes", func(e *Evidence) { e.ClassStability = 0.7 }, false, false},
		{"untracked stability is not grounds", func(e *Evidence) {
			e.ClassStabilityKnown, e.ClassStability = false, 0
		}, false, false},
		// Fail-safe: claiming to track stability and producing NaN is
		// grounds for distrust, not a free pass.
		{"tracked NaN stability refuses", func(e *Evidence) { e.ClassStability = math.NaN() }, true, false},
		{"tracked negative stability refuses", func(e *Evidence) { e.ClassStability = -1 }, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			tc.mutate(&ev)
			got := RefuseClassUnstable(ev, cfg, refNow)
			if (got != nil) != tc.wantRef {
				t.Fatalf("refusal = %+v, want refusal=%v", got, tc.wantRef)
			}
			if got == nil {
				return
			}
			if got.Code != CodeClassUnstable {
				t.Fatalf("code = %q", got.Code)
			}
			if tc.wantUntil && !got.Until.After(refNow) {
				t.Fatalf("Until %v must be after now while the flip window is open", got.Until)
			}
			if !tc.wantUntil && !got.Until.IsZero() {
				t.Fatalf("Until = %v, want zero (clearing time unknowable)", got.Until)
			}
		})
	}
}

func TestRefuseSignalConflict(t *testing.T) {
	cfg := DefaultConfig() // MaxHPAThrashPerHour 2.0
	cases := []struct {
		name     string
		mutate   func(*Evidence)
		wantRef  bool
		wantWord string
	}{
		{"no conflict", func(e *Evidence) {}, false, ""},
		{"shrink with OOM", func(e *Evidence) {
			e.ShrinkIndicated, e.OOMsInWindow = true, 2
		}, true, "OOMKill"},
		{"shrink with throttling", func(e *Evidence) {
			e.ShrinkIndicated, e.ThrottledInWindow = true, true
		}, true, "throttling"},
		{"OOM prioritized over throttling in the sentence", func(e *Evidence) {
			e.ShrinkIndicated, e.OOMsInWindow, e.ThrottledInWindow = true, 1, true
		}, true, "OOMKill"},
		// No shrink indicated: OOM/throttle alone is not a contradiction,
		// it is just a workload that needs more, which sizing handles.
		{"OOM without shrink is not a conflict", func(e *Evidence) {
			e.ShrinkIndicated, e.OOMsInWindow = false, 5
		}, false, ""},
		{"throttling without shrink is not a conflict", func(e *Evidence) {
			e.ShrinkIndicated, e.ThrottledInWindow = false, true
		}, false, ""},
		{"thrash below threshold", func(e *Evidence) { e.HPAThrashPerHour = 1.9 }, false, ""},
		{"thrash exactly at threshold refuses", func(e *Evidence) { e.HPAThrashPerHour = 2.0 }, true, "thrashing"},
		{"thrash above threshold", func(e *Evidence) { e.HPAThrashPerHour = 4 }, true, "thrashing"},
		// Positive-form comparison: garbage thrash must refuse, not pass.
		{"NaN thrash refuses", func(e *Evidence) { e.HPAThrashPerHour = math.NaN() }, true, "thrashing"},
		{"Inf thrash refuses", func(e *Evidence) { e.HPAThrashPerHour = math.Inf(1) }, true, "thrashing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			tc.mutate(&ev)
			got := RefuseSignalConflict(ev, cfg, refNow)
			if (got != nil) != tc.wantRef {
				t.Fatalf("refusal = %+v, want refusal=%v", got, tc.wantRef)
			}
			if got == nil {
				return
			}
			if got.Code != CodeSignalConflict {
				t.Fatalf("code = %q", got.Code)
			}
			if !strings.Contains(got.Detail, tc.wantWord) {
				t.Fatalf("detail %q does not mention %q", got.Detail, tc.wantWord)
			}
		})
	}
}

// TestRefuseSignalConflictNaNConfigFailsSafe: a NaN threshold in the config
// must refuse rather than let every thrash score through.
func TestRefuseSignalConflictNaNConfigFailsSafe(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxHPAThrashPerHour = math.NaN()
	if got := RefuseSignalConflict(cleanEvidence(), cfg, refNow); got == nil {
		t.Fatal("NaN MaxHPAThrashPerHour must refuse, not pass")
	}
}

func TestRefuseRegimeChangePending(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name    string
		class   patterns.Class
		cp      time.Time
		wantRef bool
	}{
		{"no changepoint", patterns.ClassSteady, time.Time{}, false},
		{"changepoint just now", patterns.ClassSteady, refNow, true},
		{"inside soak", patterns.ClassSteady, refNow.Add(-3 * time.Hour), true},
		{"exactly at soak clears", patterns.ClassSteady, refNow.Add(-6 * time.Hour), false},
		{"beyond soak clears", patterns.ClassSteady, refNow.Add(-9 * time.Hour), false},
		{"diurnal soak is 24h", patterns.ClassDiurnal, refNow.Add(-10 * time.Hour), true},
		{"future changepoint (skew) refuses", patterns.ClassSteady, refNow.Add(time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			ev.Class, ev.LastChangepoint = tc.class, tc.cp
			got := RefuseRegimeChangePending(ev, cfg, refNow)
			if (got != nil) != tc.wantRef {
				t.Fatalf("refusal = %+v, want refusal=%v", got, tc.wantRef)
			}
			if got == nil {
				return
			}
			if got.Code != CodeRegimeChangePending {
				t.Fatalf("code = %q", got.Code)
			}
			if want := tc.cp.Add(SoakFor(tc.class, cfg.BaseSoak)); !got.Until.Equal(want) {
				t.Fatalf("Until = %v, want %v", got.Until, want)
			}
		})
	}
}

func TestRefuseForecastDivergence(t *testing.T) {
	cfg := DefaultConfig() // MaxForecastDivergence 0.35
	cases := []struct {
		name            string
		builtin, remote float64
		wantRef         bool
	}{
		{"identical", 100, 100, false},
		{"10% apart", 90, 100, false},
		{"exactly at tolerance passes", 65, 100, false},
		{"just beyond tolerance refuses", 64, 100, true},
		{"wildly apart", 10, 100, true},
		{"symmetric in argument order", 100, 10, true},
		{"remote unavailable is not grounds", 100, 0, false},
		{"builtin unavailable is not grounds", 0, 100, false},
		{"NaN is not grounds", math.NaN(), 100, false},
		{"+Inf is not grounds", 100, math.Inf(1), false},
		{"-Inf is not grounds", math.Inf(-1), 100, false},
		{"negative is not grounds", -10, 100, false},
		{"beyond maxAbsSample is not grounds", 1e300, 100, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			ev.BuiltinForecast, ev.RemoteForecast = tc.builtin, tc.remote
			got := RefuseForecastDivergence(ev, cfg, refNow)
			if (got != nil) != tc.wantRef {
				t.Fatalf("refusal = %+v, want refusal=%v", got, tc.wantRef)
			}
			if got != nil && got.Code != CodeForecastDivergence {
				t.Fatalf("code = %q", got.Code)
			}
		})
	}
}

// TestRefuseForecastDivergenceNaNConfigFailsSafe: a NaN tolerance must refuse
// when both forecasts are present, not wave the disagreement through.
func TestRefuseForecastDivergenceNaNConfigFailsSafe(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxForecastDivergence = math.NaN()
	ev := cleanEvidence()
	ev.BuiltinForecast, ev.RemoteForecast = 100, 101
	if got := RefuseForecastDivergence(ev, cfg, refNow); got == nil {
		t.Fatal("NaN MaxForecastDivergence must refuse, not pass")
	}
}

func TestRefuseSLADegraded(t *testing.T) {
	cfg := DefaultConfig()
	t.Run("healthy SLO is not grounds", func(t *testing.T) {
		if got := RefuseSLADegraded(cleanEvidence(), cfg, refNow); got != nil {
			t.Fatalf("refusal = %+v", got)
		}
	})
	t.Run("degraded SLO refuses", func(t *testing.T) {
		ev := cleanEvidence()
		ev.SLODegraded = true
		got := RefuseSLADegraded(ev, cfg, refNow)
		if got == nil || got.Code != CodeSLADegraded {
			t.Fatalf("got %+v, want %q refusal", got, CodeSLADegraded)
		}
	})
}

func TestRefuseQuarantined(t *testing.T) {
	cfg := DefaultConfig()
	t.Run("not quarantined is not grounds", func(t *testing.T) {
		if got := RefuseQuarantined(cleanEvidence(), cfg, refNow); got != nil {
			t.Fatalf("refusal = %+v", got)
		}
	})
	t.Run("quarantined without reason", func(t *testing.T) {
		ev := cleanEvidence()
		ev.Quarantined = true
		got := RefuseQuarantined(ev, cfg, refNow)
		if got == nil || got.Code != CodeQuarantined {
			t.Fatalf("got %+v", got)
		}
		if strings.Contains(got.Detail, "()") {
			t.Fatalf("empty reason leaked an empty parenthetical: %q", got.Detail)
		}
	})
	t.Run("quarantined with reason surfaces it", func(t *testing.T) {
		ev := cleanEvidence()
		ev.Quarantined, ev.QuarantineReason = true, "p95 latency +40%"
		got := RefuseQuarantined(ev, cfg, refNow)
		if got == nil || !strings.Contains(got.Detail, "p95 latency +40%") {
			t.Fatalf("detail %q does not carry the reason", got)
		}
	})
}

// TestEvaluateOrder locks the documented first-match-wins precedence from
// design §4.2. Each case sets up every earlier condition plus the one under
// test and asserts the earliest one wins.
func TestEvaluateOrder(t *testing.T) {
	cfg := DefaultConfig()
	// Mutators, in the same order as the predicate table.
	steps := []struct {
		code   RefusalCode
		mutate func(*Evidence)
	}{
		{CodeInsufficientHistory, func(e *Evidence) { e.Samples = 1 }},
		{CodePostChangeSoak, func(e *Evidence) { e.LastChange = refNow }},
		{CodeClassUnstable, func(e *Evidence) { e.LastClassFlip = refNow }},
		{CodeSignalConflict, func(e *Evidence) { e.HPAThrashPerHour = 99 }},
		{CodeRegimeChangePending, func(e *Evidence) { e.LastChangepoint = refNow }},
		{CodeForecastDivergence, func(e *Evidence) { e.BuiltinForecast, e.RemoteForecast = 1, 100 }},
		{CodeSLADegraded, func(e *Evidence) { e.SLODegraded = true }},
		{CodeQuarantined, func(e *Evidence) { e.Quarantined = true }},
	}
	// Applying steps i..n-1 must always surface steps[i].code.
	for i := range steps {
		t.Run(string(steps[i].code), func(t *testing.T) {
			ev := cleanEvidence()
			for j := i; j < len(steps); j++ {
				steps[j].mutate(&ev)
			}
			got := Evaluate(ev, cfg, refNow)
			if got == nil {
				t.Fatal("want a refusal")
			}
			if got.Code != steps[i].code {
				t.Fatalf("code = %q, want %q (precedence violated)", got.Code, steps[i].code)
			}
		})
	}
	// And each condition alone surfaces itself.
	for i := range steps {
		t.Run("alone/"+string(steps[i].code), func(t *testing.T) {
			ev := cleanEvidence()
			steps[i].mutate(&ev)
			got := Evaluate(ev, cfg, refNow)
			if got == nil || got.Code != steps[i].code {
				t.Fatalf("got %+v, want %q", got, steps[i].code)
			}
		})
	}
}

// TestEveryRefusalCodeIsReachable guards against a predicate that can never
// fire (dead code masquerading as a safety net).
func TestEveryRefusalCodeIsReachable(t *testing.T) {
	all := []RefusalCode{
		CodeInsufficientHistory, CodePostChangeSoak, CodeClassUnstable,
		CodeSignalConflict, CodeRegimeChangePending, CodeForecastDivergence,
		CodeSLADegraded, CodeQuarantined,
	}
	if len(predicates) != len(all) {
		t.Fatalf("predicate count %d != documented code count %d", len(predicates), len(all))
	}
	seen := map[RefusalCode]bool{}
	mutators := []func(*Evidence){
		func(e *Evidence) { e.Samples = 1 },
		func(e *Evidence) { e.LastChange = refNow },
		func(e *Evidence) { e.LastClassFlip = refNow },
		func(e *Evidence) { e.ClassStability = 0.1 },
		func(e *Evidence) { e.HPAThrashPerHour = 99 },
		func(e *Evidence) { e.ShrinkIndicated, e.OOMsInWindow = true, 1 },
		func(e *Evidence) { e.LastChangepoint = refNow },
		func(e *Evidence) { e.BuiltinForecast, e.RemoteForecast = 1, 100 },
		func(e *Evidence) { e.SLODegraded = true },
		func(e *Evidence) { e.Quarantined = true },
	}
	for _, m := range mutators {
		ev := cleanEvidence()
		m(&ev)
		if r := Evaluate(ev, DefaultConfig(), refNow); r != nil {
			seen[r.Code] = true
		}
	}
	for _, c := range all {
		if !seen[c] {
			t.Errorf("refusal code %q is unreachable through Evaluate", c)
		}
	}
}

// TestRefusalShapeInvariant: every refusal from every predicate carries a
// machine-readable code and a non-empty human sentence. This is the §4.2
// contract; a refusal without a reason is silence with extra steps.
func TestRefusalShapeInvariant(t *testing.T) {
	known := map[RefusalCode]bool{
		CodeInsufficientHistory: true, CodePostChangeSoak: true,
		CodeClassUnstable: true, CodeSignalConflict: true,
		CodeRegimeChangePending: true, CodeForecastDivergence: true,
		CodeSLADegraded: true, CodeQuarantined: true,
	}
	// A corpus that trips each predicate plus a pile of garbage values.
	corpus := []Evidence{
		{},
		cleanEvidence(),
		{Samples: -1, Window: -time.Hour, LastSample: refNow},
		{Samples: 99, Window: 99 * time.Hour, LastSample: refNow, LastChange: refNow},
		{Samples: 99, Window: 99 * time.Hour, LastClassFlip: refNow.Add(-time.Hour)},
		{Samples: 99, Window: 99 * time.Hour, ClassStabilityKnown: true, ClassStability: math.NaN()},
		{Samples: 99, Window: 99 * time.Hour, HPAThrashPerHour: math.Inf(1)},
		{Samples: 99, Window: 99 * time.Hour, ShrinkIndicated: true, ThrottledInWindow: true},
		{Samples: 99, Window: 99 * time.Hour, LastChangepoint: refNow},
		{Samples: 99, Window: 99 * time.Hour, BuiltinForecast: 1, RemoteForecast: 1e9},
		{Samples: 99, Window: 99 * time.Hour, SLODegraded: true},
		{Samples: 99, Window: 99 * time.Hour, Quarantined: true, QuarantineReason: "regressed"},
	}
	cfgs := []Config{DefaultConfig(), {}}
	for _, cfg := range cfgs {
		for i, ev := range corpus {
			for _, p := range predicates {
				r := p(ev, cfg, refNow)
				if r == nil {
					continue
				}
				if !known[r.Code] {
					t.Errorf("corpus[%d]: unknown refusal code %q", i, r.Code)
				}
				if strings.TrimSpace(r.Detail) == "" {
					t.Errorf("corpus[%d]: refusal %q has no human sentence", i, r.Code)
				}
				if strings.Contains(r.Detail, "NaN") || strings.Contains(r.Detail, "%!") {
					t.Errorf("corpus[%d]: refusal %q leaked a formatting artifact: %q", i, r.Code, r.Detail)
				}
			}
		}
	}
}

// TestEvaluateIsDeterministic: identical inputs must produce identical
// output, including across repeated calls (no hidden state, no map order).
func TestEvaluateIsDeterministic(t *testing.T) {
	ev := cleanEvidence()
	ev.LastChange = refNow.Add(-time.Hour)
	first := Evaluate(ev, DefaultConfig(), refNow)
	for i := 0; i < 100; i++ {
		got := Evaluate(ev, DefaultConfig(), refNow)
		if got == nil || first == nil {
			t.Fatal("expected a refusal on every call")
		}
		if *got != *first {
			t.Fatalf("call %d differs: %+v vs %+v", i, *got, *first)
		}
	}
}

// TestEvaluateDoesNotMutateEvidence: predicates are pure.
func TestEvaluateDoesNotMutateEvidence(t *testing.T) {
	ev := cleanEvidence()
	ev.HPAThrashPerHour = 99
	before := ev
	_ = Evaluate(ev, DefaultConfig(), refNow)
	if ev != before {
		t.Fatalf("Evaluate mutated its Evidence argument:\n got %+v\nwant %+v", ev, before)
	}
}

// TestRefuseClassUnstableSentenceNeverLeaksGarbage: the human sentence is
// shown verbatim in /insights and the UI. An unusable stability fraction —
// or an unusable configured threshold — must produce a sentence an operator
// can act on, not "class stability NaN is below the required NaN".
func TestRefuseClassUnstableSentenceNeverLeaksGarbage(t *testing.T) {
	cases := []struct {
		name      string
		stability float64
		threshold float64
	}{
		{"NaN stability", math.NaN(), 0.7},
		{"+Inf stability", math.Inf(1), 0.7},
		{"-Inf stability", math.Inf(-1), 0.7},
		{"negative stability", -0.5, 0.7},
		{"stability above 1", 1.5, 0.7},
		{"NaN threshold with good stability", 0.95, math.NaN()},
		{"NaN threshold with bad stability", 0.10, math.NaN()},
		{"threshold above 1 with good stability", 0.95, 4},
		{"NaN both", math.NaN(), math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.MinClassStability = tc.threshold
			ev := cleanEvidence()
			ev.ClassStabilityKnown, ev.ClassStability = true, tc.stability

			got := RefuseClassUnstable(ev, cfg, refNow)
			if got == nil {
				// Only legal when stability is a usable fraction that clears
				// the fallback threshold.
				if !(tc.stability >= DefaultConfig().MinClassStability && tc.stability <= 1) {
					t.Fatalf("stability=%v threshold=%v: want a refusal", tc.stability, tc.threshold)
				}
				return
			}
			for _, bad := range []string{"NaN", "Inf", "%!", "-0.50", "1.50"} {
				if strings.Contains(got.Detail, bad) {
					t.Fatalf("sentence leaked %q: %q", bad, got.Detail)
				}
			}
		})
	}
}

// TestRefuseClassUnstableUnusableStabilityIsDistinct: an un-computable
// stability gets its own sentence, not the numeric-comparison one, so the
// operator can tell "the classifier disagrees with itself" from "the
// classifier could not report agreement at all".
func TestRefuseClassUnstableUnusableStabilityIsDistinct(t *testing.T) {
	cfg := DefaultConfig()

	low := cleanEvidence()
	low.ClassStabilityKnown, low.ClassStability = true, 0.2
	lowRef := RefuseClassUnstable(low, cfg, refNow)

	bad := cleanEvidence()
	bad.ClassStabilityKnown, bad.ClassStability = true, math.NaN()
	badRef := RefuseClassUnstable(bad, cfg, refNow)

	if lowRef == nil || badRef == nil {
		t.Fatalf("both must refuse: low=%+v bad=%+v", lowRef, badRef)
	}
	if lowRef.Detail == badRef.Detail {
		t.Fatalf("unusable stability reuses the numeric sentence: %q", badRef.Detail)
	}
}
