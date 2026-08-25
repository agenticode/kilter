package decision

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/patterns"
)

func TestDefaultConfigValidates(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig must validate: %v", err)
	}
}

// TestDefaultConfigMatchesSiblingPackages pins the couplings the field
// comments claim. If recommend or plan moves, this fails and someone has to
// decide deliberately rather than drift apart silently.
func TestDefaultConfigMatchesSiblingPackages(t *testing.T) {
	c := DefaultConfig()
	if c.MinSamples != 30 {
		t.Errorf("MinSamples = %d; the comment claims parity with recommend.Config.MinSamples (30)", c.MinSamples)
	}
	if c.MinWindow != 6*time.Hour {
		t.Errorf("MinWindow = %v; the comment claims parity with recommend.Config.MinWindow (6h)", c.MinWindow)
	}
	if c.ActConfidence != 0.6 {
		t.Errorf("ActConfidence = %v; the comment claims parity with plan.Config.MinConfidence (0.6)", c.ActConfidence)
	}
	// The forecast tolerance rationale: it must exceed the standard memory
	// headroom excess plus the minimum change ratio, or ordinary headroom
	// could not absorb a disagreement of that size.
	if !(c.MaxForecastDivergence > 0.20+0.10) {
		t.Errorf("MaxForecastDivergence = %v must exceed MemoryHeadroom-1 (0.20) + MinChangeRatio (0.10)", c.MaxForecastDivergence)
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"default", func(c *Config) {}, false},
		{"minSamples 1 is the floor", func(c *Config) { c.MinSamples = 1 }, false},
		{"minSamples 0", func(c *Config) { c.MinSamples = 0 }, true},
		{"minSamples negative", func(c *Config) { c.MinSamples = -1 }, true},
		{"minWindow zero", func(c *Config) { c.MinWindow = 0 }, true},
		{"minWindow negative", func(c *Config) { c.MinWindow = -time.Hour }, true},
		{"baseSoak zero", func(c *Config) { c.BaseSoak = 0 }, true},
		{"baseSoak negative", func(c *Config) { c.BaseSoak = -1 }, true},
		{"classFlipWindow zero", func(c *Config) { c.ClassFlipWindow = 0 }, true},
		{"minClassStability 0 is allowed", func(c *Config) { c.MinClassStability = 0 }, false},
		{"minClassStability 1 is allowed", func(c *Config) { c.MinClassStability = 1 }, false},
		{"minClassStability above 1", func(c *Config) { c.MinClassStability = 1.01 }, true},
		{"minClassStability negative", func(c *Config) { c.MinClassStability = -0.01 }, true},
		{"minClassStability NaN", func(c *Config) { c.MinClassStability = math.NaN() }, true},
		{"maxHPAThrash zero", func(c *Config) { c.MaxHPAThrashPerHour = 0 }, true},
		{"maxHPAThrash negative", func(c *Config) { c.MaxHPAThrashPerHour = -1 }, true},
		{"maxHPAThrash NaN", func(c *Config) { c.MaxHPAThrashPerHour = math.NaN() }, true},
		{"maxHPAThrash Inf", func(c *Config) { c.MaxHPAThrashPerHour = math.Inf(1) }, true},
		{"maxForecastDivergence zero", func(c *Config) { c.MaxForecastDivergence = 0 }, true},
		{"maxForecastDivergence 1", func(c *Config) { c.MaxForecastDivergence = 1 }, true},
		{"maxForecastDivergence NaN", func(c *Config) { c.MaxForecastDivergence = math.NaN() }, true},
		{"actConfidence zero", func(c *Config) { c.ActConfidence = 0 }, true},
		{"actConfidence 1 is allowed", func(c *Config) { c.ActConfidence = 1 }, false},
		{"actConfidence above 1", func(c *Config) { c.ActConfidence = 1.01 }, true},
		{"actConfidence NaN", func(c *Config) { c.ActConfidence = math.NaN() }, true},
		{"zero value config", func(c *Config) { *c = Config{} }, true},
		// An absurd but positive soak base must be rejected at startup
		// rather than relied on to be clamped later.
		{"baseSoak beyond the cap", func(c *Config) { c.BaseSoak = 100 * 365 * 24 * time.Hour }, true},
		{"classFlipWindow absurd", func(c *Config) { c.ClassFlipWindow = 100 * 365 * 24 * time.Hour }, true},
		{"minWindow absurd", func(c *Config) { c.MinWindow = 100 * 365 * 24 * time.Hour }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr=%v (cfg %+v)", err, tc.wantErr, cfg)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "decision: ") {
				t.Fatalf("error %q is not package-prefixed", err)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	cfg := DefaultConfig() // ActConfidence 0.6
	conf := func(score float64) Confidence {
		return Confidence{Score: score, Basis: []ConfidenceTerm{{Name: "t", Value: score, Weight: 1}}}
	}

	cases := []struct {
		name   string
		mutate func(*Evidence)
		score  float64
		want   Action
	}{
		{"clean and confident acts", func(e *Evidence) {}, 0.9, ActionAct},
		{"exactly at threshold acts", func(e *Evidence) {}, 0.6, ActionAct},
		{"just below threshold recommends only", func(e *Evidence) {}, 0.59, ActionRecommendOnly},
		{"zero confidence recommends only", func(e *Evidence) {}, 0, ActionRecommendOnly},
		{"refusal beats high confidence", func(e *Evidence) { e.Quarantined = true }, 1.0, ActionRefuse},
		{"refusal beats low confidence", func(e *Evidence) { e.Samples = 0 }, 0.1, ActionRefuse},
		// Fail-safe: a NaN score must never clear a threshold.
		{"NaN confidence recommends only", func(e *Evidence) {}, math.NaN(), ActionRecommendOnly},
		{"negative confidence recommends only", func(e *Evidence) {}, -1, ActionRecommendOnly},
		{"+Inf confidence acts", func(e *Evidence) {}, math.Inf(1), ActionAct},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := cleanEvidence()
			tc.mutate(&ev)
			got := Decide(ev, conf(tc.score), cfg, refNow)
			if got.Action != tc.want {
				t.Fatalf("Action = %q, want %q (refusal %+v)", got.Action, tc.want, got.Refusal)
			}
			// The confidence is carried through verbatim in every case,
			// including refusals: a refusal still reports what was known.
			if !(got.Confidence.Score == tc.score) && !math.IsNaN(tc.score) {
				t.Fatalf("Confidence.Score = %v, want %v", got.Confidence.Score, tc.score)
			}
			if len(got.Confidence.Basis) != 1 {
				t.Fatalf("basis dropped: %+v", got.Confidence)
			}
			// A refusal is present exactly when the action is refuse.
			if (got.Refusal != nil) != (got.Action == ActionRefuse) {
				t.Fatalf("Action=%q but Refusal=%+v", got.Action, got.Refusal)
			}
		})
	}
}

// TestDecideNeverActsOnGarbageConfig is the fail-safe the package doc
// promises: "if a garbage config does reach Evaluate/Decide anyway, the
// comparisons degrade toward refusal / recommend-only, never toward act."
// A config with a non-positive or NaN act threshold must not turn a
// zero-confidence subject into an action.
func TestDecideNeverActsOnGarbageConfig(t *testing.T) {
	badThresholds := []float64{0, -1, -0.5, math.NaN(), math.Inf(-1)}
	scores := []float64{0, 0.01, 0.3, 0.59, 0.6, 1, math.NaN(), -1}

	for _, th := range badThresholds {
		for _, score := range scores {
			cfg := DefaultConfig()
			cfg.ActConfidence = th
			got := Decide(cleanEvidence(), Confidence{Score: score}, cfg, refNow)
			if got.Action == ActionAct {
				t.Fatalf("ActConfidence=%v score=%v produced %q; an unvalidated threshold must never license action",
					th, score, got.Action)
			}
		}
	}
}

// TestDecideZeroConfigRefuses: the fully zero Config is the worst realistic
// misuse (a caller that forgot DefaultConfig). It must refuse, not act.
func TestDecideZeroConfigRefuses(t *testing.T) {
	for _, score := range []float64{0, 0.5, 1} {
		got := Decide(cleanEvidence(), Confidence{Score: score}, Config{}, refNow)
		if got.Action == ActionAct {
			t.Fatalf("zero Config with score %v produced %q", score, got.Action)
		}
		if got.Action == ActionRefuse && got.Refusal == nil {
			t.Fatal("refuse without a Refusal")
		}
	}
}

func TestDecideIsDeterministicAndPure(t *testing.T) {
	ev := cleanEvidence()
	ev.LastChange = refNow.Add(-2 * time.Hour)
	conf := Compose(TermHistoryDepth(100, 120), TermVolatility(0.01))
	before := ev

	first := Decide(ev, conf, DefaultConfig(), refNow)
	for i := 0; i < 50; i++ {
		got := Decide(ev, conf, DefaultConfig(), refNow)
		if got.Action != first.Action || got.Confidence.Score != first.Confidence.Score {
			t.Fatalf("call %d diverged: %+v vs %+v", i, got, first)
		}
		if (got.Refusal == nil) != (first.Refusal == nil) {
			t.Fatalf("call %d refusal presence diverged", i)
		}
		if got.Refusal != nil && *got.Refusal != *first.Refusal {
			t.Fatalf("call %d refusal diverged: %+v vs %+v", i, *got.Refusal, *first.Refusal)
		}
	}
	if ev != before {
		t.Fatal("Decide mutated its Evidence argument")
	}
}

// TestVerdictJSONShape: the Verdict crosses the API boundary into /insights
// and the UI, so its wire shape is part of the contract.
func TestVerdictJSONShape(t *testing.T) {
	t.Run("act omits refusal", func(t *testing.T) {
		v := Decide(cleanEvidence(), Confidence{Score: 0.9}, DefaultConfig(), refNow)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "refusal") {
			t.Fatalf("act verdict carries a refusal key: %s", b)
		}
		if !strings.Contains(string(b), `"action":"act"`) {
			t.Fatalf("unexpected shape: %s", b)
		}
	})
	t.Run("refusal carries code, detail and until", func(t *testing.T) {
		ev := cleanEvidence()
		ev.LastChange = refNow.Add(-time.Hour)
		v := Decide(ev, Confidence{Score: 0.9}, DefaultConfig(), refNow)
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var back Verdict
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.Action != ActionRefuse || back.Refusal == nil {
			t.Fatalf("round trip lost the refusal: %s", b)
		}
		if back.Refusal.Code != CodePostChangeSoak || back.Refusal.Detail == "" {
			t.Fatalf("round trip lost code/detail: %+v", back.Refusal)
		}
		if !back.Refusal.Until.Equal(v.Refusal.Until) {
			t.Fatalf("Until = %v, want %v", back.Refusal.Until, v.Refusal.Until)
		}
	})
	t.Run("zero until is omitted", func(t *testing.T) {
		ev := cleanEvidence()
		ev.SLODegraded = true
		v := Decide(ev, Confidence{Score: 0.9}, DefaultConfig(), refNow)
		b, _ := json.Marshal(v)
		// Inspect the keys, not the bytes: the human sentence legitimately
		// contains the word "until".
		var raw struct {
			Refusal map[string]json.RawMessage `json:"refusal"`
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw.Refusal["until"]; ok {
			t.Fatalf("unknowable clearing time serialized an until key: %s", b)
		}
		if _, ok := raw.Refusal["code"]; !ok {
			t.Fatalf("refusal lost its code: %s", b)
		}
	})
}

// --- properties ------------------------------------------------------------

// TestRefuseEverythingIsNeverTheBestPolicy is the design's stated property
// (§9 unit 2): a policy that abstains on everything must not score as well
// as the real engine. Scored against a synthetic fleet under an explicit
// value model — acting correctly saves, acting on a bad subject costs
// several times more than one save is worth, abstaining scores zero — the
// real Evaluate must beat both degenerate policies. If refusing everything
// ever won, the refusal predicates would have become a way to game the
// scorecard rather than a safety mechanism.
func TestRefuseEverythingIsNeverTheBestPolicy(t *testing.T) {
	// An unsafe action costs 5x what a safe one saves: the standard
	// asymmetry between "saved some money" and "caused an incident".
	const savePerGoodAct = 1.0
	const costPerBadAct = 5.0

	type subject struct {
		ev   Evidence
		safe bool // whether acting on this subject is actually correct
	}

	rng := newLCG(0x1234ABCD)
	fleet := make([]subject, 0, 600)
	for i := 0; i < 600; i++ {
		ev := cleanEvidence()
		safe := true
		// Two thirds of the fleet is healthy; the rest has exactly one
		// genuine defect that makes acting unsafe.
		switch int(rng.uniform() * 18) {
		case 0:
			ev.Samples, ev.Window = 3, 20*time.Minute
			safe = false
		case 1:
			ev.LastChange = refNow.Add(-time.Duration(rng.uniform()*5) * time.Hour)
			safe = false
		case 2:
			ev.LastClassFlip = refNow.Add(-time.Duration(rng.uniform()*20) * time.Hour)
			safe = false
		case 3:
			ev.ShrinkIndicated, ev.OOMsInWindow = true, 1+int(rng.uniform()*3)
			safe = false
		case 4:
			ev.LastChangepoint = refNow.Add(-time.Duration(rng.uniform()*5) * time.Hour)
			safe = false
		case 5:
			ev.BuiltinForecast, ev.RemoteForecast = 100, 20
			safe = false
		default:
			// healthy: vary the benign fields so the fleet is not uniform
			ev.HPAThrashPerHour = rng.uniform() * 1.5
			ev.ClassStability = 0.7 + rng.uniform()*0.3
			ev.Window = time.Duration(24+rng.uniform()*100) * time.Hour
			ev.Samples = 40 + int(rng.uniform()*400)
		}
		fleet = append(fleet, subject{ev: ev, safe: safe})
	}

	score := func(act func(Evidence) bool) float64 {
		total := 0.0
		for _, s := range fleet {
			if !act(s.ev) {
				continue
			}
			if s.safe {
				total += savePerGoodAct
			} else {
				total -= costPerBadAct
			}
		}
		return total
	}

	cfg := DefaultConfig()
	engine := score(func(ev Evidence) bool { return Evaluate(ev, cfg, refNow) == nil })
	refuseAll := score(func(Evidence) bool { return false })
	actAll := score(func(Evidence) bool { return true })

	if refuseAll != 0 {
		t.Fatalf("refuse-everything must score exactly zero, got %v", refuseAll)
	}
	if !(engine > refuseAll) {
		t.Fatalf("engine scored %v, no better than refusing everything (%v): "+
			"the refusal predicates are abstaining on subjects that are safe to act on", engine, refuseAll)
	}
	if !(engine > actAll) {
		t.Fatalf("engine scored %v, no better than acting on everything (%v): "+
			"the refusal predicates are not catching the unsafe subjects", engine, actAll)
	}
	// And the engine must actually act on a healthy majority, not squeak
	// past by acting on three subjects.
	acted := 0
	for _, s := range fleet {
		if Evaluate(s.ev, cfg, refNow) == nil {
			acted++
		}
	}
	if acted < len(fleet)/2 {
		t.Fatalf("engine acted on only %d/%d subjects; abstention is not free", acted, len(fleet))
	}
}

// TestRefusalIsMonotonicInEvidenceQuality: adding a defect to a subject can
// only move it toward abstention, never toward action. A predicate set where
// breaking something makes the engine *more* willing to act would be
// incoherent.
func TestRefusalIsMonotonicInEvidenceQuality(t *testing.T) {
	cfg := DefaultConfig()
	defects := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"fewer samples", func(e *Evidence) { e.Samples = 2 }},
		{"shorter window", func(e *Evidence) { e.Window = time.Minute }},
		{"recent change", func(e *Evidence) { e.LastChange = refNow }},
		{"recent class flip", func(e *Evidence) { e.LastClassFlip = refNow }},
		{"low class stability", func(e *Evidence) { e.ClassStability = 0.1 }},
		{"OOM against a shrink", func(e *Evidence) { e.ShrinkIndicated, e.OOMsInWindow = true, 3 }},
		{"throttled against a shrink", func(e *Evidence) { e.ShrinkIndicated, e.ThrottledInWindow = true, true }},
		{"HPA thrash", func(e *Evidence) { e.HPAThrashPerHour = 9 }},
		{"recent changepoint", func(e *Evidence) { e.LastChangepoint = refNow }},
		{"forecast divergence", func(e *Evidence) { e.BuiltinForecast, e.RemoteForecast = 100, 5 }},
		{"SLO degraded", func(e *Evidence) { e.SLODegraded = true }},
		{"quarantined", func(e *Evidence) { e.Quarantined = true }},
	}
	for _, d := range defects {
		t.Run(d.name, func(t *testing.T) {
			healthy := cleanEvidence()
			if Evaluate(healthy, cfg, refNow) != nil {
				t.Fatal("baseline is not healthy")
			}
			broken := healthy
			d.mutate(&broken)
			if Evaluate(broken, cfg, refNow) == nil {
				t.Fatalf("%s did not move the subject toward abstention", d.name)
			}
			// Piling this defect on top of every other defect must also
			// still refuse.
			for _, other := range defects {
				both := healthy
				other.mutate(&both)
				d.mutate(&both)
				if Evaluate(both, cfg, refNow) == nil {
					t.Fatalf("%s + %s produced no refusal", other.name, d.name)
				}
			}
		})
	}
}

// TestConfidenceTermsCoverTheDesignedSet: §4.1 enumerates the terms the
// basis must be able to carry. A term quietly dropped or renamed would break
// the dossier and the explain payload downstream.
func TestConfidenceTermsCoverTheDesignedSet(t *testing.T) {
	c := Compose(
		TermHistoryDepth(100, 120),
		TermWindowSpan(24*time.Hour, 12*time.Hour),
		TermVolatility(0.02),
		TermClassStability(0.9),
		TermPostChangeSoak(12*time.Hour, 6*time.Hour),
		TermSignalAgreement(3, 3),
		TermForecastAgreement(100, 105),
		TermFreshness(5*time.Minute, time.Hour),
	)
	want := []string{
		"history-depth", "window-span", "volatility", "class-stability",
		"post-change-soak", "signal-agreement", "forecast-agreement", "freshness",
	}
	if len(c.Basis) != len(want) {
		t.Fatalf("basis has %d terms, want %d", len(c.Basis), len(want))
	}
	for i, name := range want {
		if c.Basis[i].Name != name {
			t.Fatalf("basis[%d] = %q, want %q (order is part of the contract)", i, c.Basis[i].Name, name)
		}
		if c.Basis[i].Note == "" {
			t.Errorf("term %q carries no note; every term must say why it scored what it did", name)
		}
	}
	if !(c.Score > 0 && c.Score <= 1) {
		t.Fatalf("Score = %v, want (0,1] for an all-healthy basis", c.Score)
	}
}

// TestSoakForIsConsistentWithTermPostChangeSoak: the refusal and the
// confidence term must agree about how long a soak is, or a subject can be
// simultaneously "soaked enough to score 1.0" and "still refused".
func TestSoakForIsConsistentWithTermPostChangeSoak(t *testing.T) {
	cfg := DefaultConfig()
	classes := []patterns.Class{
		patterns.ClassSteady, patterns.ClassDiurnal, patterns.ClassBursty,
		patterns.ClassBatch, patterns.ClassGrowing, patterns.ClassUnknown,
	}
	for _, class := range classes {
		soak := SoakFor(class, cfg.BaseSoak)
		for _, frac := range []float64{0, 0.25, 0.5, 0.99, 1, 1.5} {
			since := time.Duration(float64(soak) * frac)
			ev := cleanEvidence()
			ev.Class, ev.LastChange = class, refNow.Add(-since)
			refused := RefusePostChangeSoak(ev, cfg, refNow) != nil
			term := TermPostChangeSoak(since, soak)

			if refused && term.Value >= 1 {
				t.Fatalf("class %q at %.0f%% of soak: refused while the term scores a full %v",
					class, frac*100, term.Value)
			}
			if !refused && term.Value < 1 {
				t.Fatalf("class %q at %.0f%% of soak: not refused while the term scores only %v",
					class, frac*100, term.Value)
			}
		}
	}
}
