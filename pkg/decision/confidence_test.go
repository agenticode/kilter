package decision

import (
	"math"
	"testing"
	"time"
)

func TestTermHistoryDepth(t *testing.T) {
	cases := []struct {
		name       string
		samples    int
		saturation int
		want       float64
	}{
		{"zero samples", 0, 120, 0},
		{"half", 60, 120, 0.5},
		{"saturated", 120, 120, 1},
		{"beyond saturation clamps", 500, 120, 1},
		{"negative samples clamp to 0", -5, 120, 0},
		{"zero saturation is invalid", 60, 0, 0},
		{"negative saturation is invalid", 60, -1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TermHistoryDepth(tc.samples, tc.saturation)
			if got.Name != "history-depth" || got.Weight != 1 {
				t.Fatalf("bad term shape: %+v", got)
			}
			if got.Value != tc.want {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermWindowSpan(t *testing.T) {
	cases := []struct {
		name               string
		window, saturation time.Duration
		want               float64
	}{
		{"zero window", 0, 12 * time.Hour, 0},
		{"half", 6 * time.Hour, 12 * time.Hour, 0.5},
		{"saturated", 12 * time.Hour, 12 * time.Hour, 1},
		{"beyond clamps", 100 * time.Hour, 12 * time.Hour, 1},
		{"negative window clamps to 0", -time.Hour, 12 * time.Hour, 0},
		{"zero saturation invalid", 6 * time.Hour, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TermWindowSpan(tc.window, tc.saturation)
			if got.Name != "window-span" {
				t.Fatalf("bad name %q", got.Name)
			}
			if got.Value != tc.want {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermVolatility(t *testing.T) {
	cases := []struct {
		name string
		rate float64
		want float64
	}{
		{"no spikes", 0, 1},
		{"low rate", 0.02, 0.9},
		{"penalty caps at 0.5", 0.1, 0.5},
		{"beyond cap stays 0.5", 0.9, 0.5},
		{"NaN scores 0", math.NaN(), 0},
		{"negative scores 0", -0.1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TermVolatility(tc.rate)
			if math.Abs(got.Value-tc.want) > 1e-12 {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermClassStability(t *testing.T) {
	cases := []struct {
		name string
		frac float64
		want float64
	}{
		{"full agreement", 1, 1},
		{"partial", 0.7, 0.7},
		{"zero", 0, 0},
		{"above 1 clamps", 1.5, 1},
		{"NaN scores 0", math.NaN(), 0},
		{"negative scores 0", -0.5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TermClassStability(tc.frac); got.Value != tc.want {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermPostChangeSoak(t *testing.T) {
	cases := []struct {
		name            string
		since, required time.Duration
		want            float64
	}{
		{"just changed", 0, 6 * time.Hour, 0},
		{"half soaked", 3 * time.Hour, 6 * time.Hour, 0.5},
		{"fully soaked", 6 * time.Hour, 6 * time.Hour, 1},
		{"beyond clamps", 60 * time.Hour, 6 * time.Hour, 1},
		{"future change (skew) scores 0", -time.Hour, 6 * time.Hour, 0},
		{"no soak required is neutral", 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TermPostChangeSoak(tc.since, tc.required); got.Value != tc.want {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermFreshness(t *testing.T) {
	cases := []struct {
		name          string
		since, maxAge time.Duration
		want          float64
	}{
		{"fresh", 0, 2 * time.Hour, 1},
		{"half stale", time.Hour, 2 * time.Hour, 0.5},
		{"fully stale", 2 * time.Hour, 2 * time.Hour, 0},
		{"beyond clamps at 0", 10 * time.Hour, 2 * time.Hour, 0},
		{"future sample (skew) is fresh", -time.Hour, 2 * time.Hour, 1},
		{"invalid max age scores 0", time.Hour, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TermFreshness(tc.since, tc.maxAge); got.Value != tc.want {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermSignalAgreement(t *testing.T) {
	cases := []struct {
		name            string
		agreeing, total int
		want            float64
	}{
		{"all agree", 3, 3, 1},
		{"partial", 1, 2, 0.5},
		{"none agree", 0, 4, 0},
		{"no signals is neutral", 0, 0, 1},
		{"negative total is neutral", 2, -1, 1},
		{"agreeing above total clamps", 9, 3, 1},
		{"negative agreeing clamps to 0", -2, 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TermSignalAgreement(tc.agreeing, tc.total); got.Value != tc.want {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

func TestTermForecastAgreement(t *testing.T) {
	cases := []struct {
		name            string
		builtin, remote float64
		want            float64
	}{
		{"identical", 100, 100, 1},
		{"20% apart", 80, 100, 0.8},
		{"builtin unavailable is neutral", 0, 100, 1},
		{"remote unavailable is neutral", 100, 0, 1},
		{"NaN unavailable is neutral", math.NaN(), 100, 1},
		{"Inf unavailable is neutral", 100, math.Inf(1), 1},
		{"negative unavailable is neutral", -5, 100, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TermForecastAgreement(tc.builtin, tc.remote)
			if math.Abs(got.Value-tc.want) > 1e-12 {
				t.Fatalf("value = %v, want %v", got.Value, tc.want)
			}
		})
	}
}

// TestComposeLegacyBackCompat locks in that the three legacy terms reproduce
// the recommender's historical confidence formula:
// min(1, samples/(4·minSamples)) · min(1, window/(2·minWindow)) · (1 − min(0.5, 5·spikeRate)),
// rounded to 2 decimals.
func TestComposeLegacyBackCompat(t *testing.T) {
	const minSamples = 30
	minWindow := 6 * time.Hour
	for _, samples := range []int{0, 10, 30, 60, 120, 500} {
		for _, window := range []time.Duration{0, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 48 * time.Hour} {
			for _, rate := range []float64{0, 0.01, 0.05, 0.1, 0.5} {
				legacy := math.Min(1, float64(samples)/float64(minSamples*4)) *
					math.Min(1, window.Hours()/(2*minWindow.Hours())) *
					(1 - math.Min(0.5, rate*5))
				legacy = math.Round(legacy*100) / 100

				got := Compose(
					TermHistoryDepth(samples, minSamples*4),
					TermWindowSpan(window, 2*minWindow),
					TermVolatility(rate),
				)
				if got.Score != legacy {
					t.Fatalf("samples=%d window=%v rate=%v: Score=%v, legacy=%v",
						samples, window, rate, got.Score, legacy)
				}
			}
		}
	}
}

func TestComposeEdges(t *testing.T) {
	t.Run("no terms is zero confidence", func(t *testing.T) {
		if c := Compose(); c.Score != 0 {
			t.Fatalf("Score = %v, want 0", c.Score)
		}
	})
	t.Run("zero term vetoes", func(t *testing.T) {
		c := Compose(
			ConfidenceTerm{Name: "a", Value: 1, Weight: 1},
			ConfidenceTerm{Name: "b", Value: 0, Weight: 1},
		)
		if c.Score != 0 {
			t.Fatalf("Score = %v, want 0", c.Score)
		}
	})
	t.Run("NaN value clamps to 0 and vetoes", func(t *testing.T) {
		c := Compose(ConfidenceTerm{Name: "a", Value: math.NaN(), Weight: 1})
		if c.Score != 0 || c.Basis[0].Value != 0 {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("value above 1 clamps", func(t *testing.T) {
		c := Compose(ConfidenceTerm{Name: "a", Value: 7, Weight: 1})
		if c.Score != 1 || c.Basis[0].Value != 1 {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("zero-weight term is recorded but ignored", func(t *testing.T) {
		c := Compose(
			ConfidenceTerm{Name: "a", Value: 0.9, Weight: 1},
			ConfidenceTerm{Name: "b", Value: 0, Weight: 0},
		)
		if c.Score != 0.9 || len(c.Basis) != 2 {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("NaN weight clamps to 0", func(t *testing.T) {
		c := Compose(
			ConfidenceTerm{Name: "a", Value: 0.8, Weight: 1},
			ConfidenceTerm{Name: "b", Value: 0.1, Weight: math.NaN()},
		)
		if c.Score != 0.8 {
			t.Fatalf("Score = %v, want 0.8", c.Score)
		}
	})
	t.Run("only zero-weight terms is zero confidence", func(t *testing.T) {
		c := Compose(ConfidenceTerm{Name: "a", Value: 1, Weight: 0})
		if c.Score != 0 {
			t.Fatalf("Score = %v, want 0", c.Score)
		}
	})
	t.Run("weight above cap clamps", func(t *testing.T) {
		c := Compose(ConfidenceTerm{Name: "a", Value: 0.5, Weight: 1e9})
		want := math.Round(math.Pow(0.5, maxTermWeight)*100) / 100
		if c.Score != want {
			t.Fatalf("Score = %v, want %v", c.Score, want)
		}
	})
	t.Run("basis preserves input order", func(t *testing.T) {
		c := Compose(
			ConfidenceTerm{Name: "z", Value: 1, Weight: 1},
			ConfidenceTerm{Name: "a", Value: 1, Weight: 1},
			ConfidenceTerm{Name: "m", Value: 1, Weight: 1},
		)
		if c.Basis[0].Name != "z" || c.Basis[1].Name != "a" || c.Basis[2].Name != "m" {
			t.Fatalf("order not preserved: %+v", c.Basis)
		}
	})
}

// TestComposeScoreAlwaysInRange: no combination of garbage terms can push
// the score outside [0,1] or make it non-finite.
func TestComposeScoreAlwaysInRange(t *testing.T) {
	garbage := []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0, 0.5, 1, 2, 1e300, -1e300}
	for _, v := range garbage {
		for _, w := range garbage {
			c := Compose(ConfidenceTerm{Name: "g", Value: v, Weight: w},
				ConfidenceTerm{Name: "ok", Value: 0.9, Weight: 1})
			if !(c.Score >= 0 && c.Score <= 1) {
				t.Fatalf("value=%v weight=%v → Score=%v out of [0,1]", v, w, c.Score)
			}
		}
	}
}
