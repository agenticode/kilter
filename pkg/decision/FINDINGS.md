# pkg/decision — findings

Unit 3 of `docs/design/reasoning-engine.md` §9, decision-quality core only.
Self-contained library: **nothing outside `pkg/decision/` was touched**, and in
particular `pkg/recommend` is untouched — the wiring a later unit must do is
specified in [Wiring](#wiring-what-a-later-unit-must-change-in-pkgrecommend).

Stdlib only. No `time.Now()` in any logic (callers pass `now`), no package-level
mutable state, no map iteration on any output path.

---

## What this run did

The package existed but 1,561 lines were covered by a single test file
(`confidence_test.go`); `changepoint`, `floors`, `refusal` and `shift` had no
tests at all. This run wrote the tests **first, against the code as it stood**,
and then fixed production code wherever a test proved an invariant did not hold.
No file was deleted and rewritten.

| | before | after |
|---|---|---|
| production lines | 1,250 | 1,378 |
| test lines | 311 | 2,829 |
| test files | 1 | 6 |
| top-level tests | 11 | 77 |
| subtests | 60 | 373 |
| fuzz targets | 0 | 2 |

The only pre-existing coverage was `confidence.go`. `changepoint.go`,
`decision.go`, `floors.go`, `refusal.go` and `shift.go` had none.

---

## Bugs found by tests

Every item below was found by writing a test against the existing code, watching
it fail, and fixing the **production** code. Each has a named regression test.

### 1. `SoakFor` overflowed into a negative duration — two refusals silently deleted (fail-open)

`refusal.go`. `SoakFor` multiplied the configured base by the class multiplier
(×4 for diurnal/batch) *before* capping. `Config.Validate` only required
`BaseSoak > 0`, so a base above ~73 years passed validation and `base*4` wrapped
`int64` nanoseconds negative:

```
SoakFor("diurnal", 876000h) = -1620095h34m33.709551616s
```

Both `RefusePostChangeSoak` and `RefuseRegimeChangePending` gate on
`now.Sub(event) >= soak`, which is **trivially true for a negative soak**. The
two refusals that protect against sizing on post-deploy and post-regime-change
data would have vanished without a trace. This is the worst class of bug in this
package: a safety check that silently stops existing.

*Fixed* by clamping the base to `maxSoak` before multiplying (product is then
bounded by `4·maxSoak ≈ 5e15 ns`), and by bounding `BaseSoak`, `MinWindow` and
`ClassFlipWindow` in `Config.Validate` so the config cannot express the state at
all.
*Tests:* `TestSoakFor` (three overflow cases), `TestSoakForNeverNonPositive`
(property over the whole `int64` duration range × every class),
`TestConfigValidate` (`baseSoak beyond the cap`, `classFlipWindow absurd`,
`minWindow absurd`).

### 2. `Decide` licensed action on zero evidence when `ActConfidence` was unset

`decision.go`. `Decide` compared `conf.Score >= cfg.ActConfidence` without
guarding the threshold. A `Config` with `ActConfidence = 0` — the zero value, i.e.
a caller who built a `Config` literal and forgot the field — turned
`0 >= 0` into **`ActionAct` on a subject with a confidence of exactly zero**.
That directly contradicts the package doc's promise that a garbage config
"degrades toward refusal / recommend-only, never toward act".

*Fixed:* a threshold that is not a usable probability (`!(x > 0 && x <= 1)`,
which also catches NaN) now yields `recommend-only` unconditionally. Unknown
operator intent authorizes nothing.
*Test:* `TestDecideNeverActsOnGarbageConfig` (5 bad thresholds × 8 scores),
`TestDecideZeroConfigRefuses`.

### 3. One extreme sample blinded the CUSUM detector to the next real regime change

`changepoint.go`. `ZClamp` winsorized the residual *for accumulation only*; the
EWMA baseline was then updated with the **raw** value. One in-range-but-extreme
sample (`1e9` on a `1e3` series — well inside `maxAbsSample`, so not garbage)
inflated the variance estimate by ~1e12. `sigma` then swamped every subsequent
residual, and a genuine sustained 4σ regime change that followed **never fired**,
for roughly `1/Alpha` samples (~4 hours at defaults, ~3.5 days at `Alpha=0.001`).

The detector failed silent in the direction that matters: it kept reporting "no
regime change" while the workload had visibly moved.

*Fixed:* the baseline update is winsorized with the same clamped residual
(`baseV = mean + z·sigma`), then bounded to the accepted-sample domain
`[0, maxAbsSample]` so the level can never wander negative. During warmup the
raw value is still used — that phase is *establishing* the level and has no
trustworthy scale to clamp against.
*Test:* `TestChangepointSpikeDoesNotBlindDetector` (spikes of 1e6/1e9/1e15, each
followed by a real 4σ step that must be detected within the analytic delay + 3).

### 4. `ShiftFacts{}` zero value classified every exceedance as `spike`

`shift.go`. `RevertedWithin >= 0 && <= 3` meant the struct's **zero value**
scored as `spike`. `spike` is the one verdict with a shrinking consequence — per
§4.3 its samples are excluded from the memory peak term — so a caller who forgot
to populate the field would have systematically under-sized memory, silently.

"Reverted in zero samples" is not physically meaningful anyway: an excursion must
last at least one sample before it can revert.

*Fixed:* the spike window is now `1..spikeRevertMaxSamples`; `0` and negatives
both mean "has not reverted", making the zero value `indeterminate` (the safe
"nothing is resolved yet" state). This follows the `ClassStabilityKnown` pattern
already established in `Evidence`: a default must be earned, not fallen into.
*Tests:* `TestClassifyShift` (`zero means not reverted`, `zero value facts`,
`unpopulated facts never yield spike`).

### 5. A refusal sentence rendered `NaN` to the operator

`refusal.go`. A tracked-but-unusable class stability produced the user-facing
string `class stability NaN is below the required 0.70`, and a NaN configured
threshold produced `... below the required NaN`. These sentences are surfaced
verbatim in `/insights`, the dossier and the UI. §4.2's whole argument is that a
refusal must be an *honest, actionable* answer; "NaN" is neither.

*Fixed:* an unusable stability fraction (NaN, ±Inf, outside `[0,1]`) now gets its
own distinct sentence saying the classifier could not report its agreement, and
an unusable configured threshold falls back to the documented default rather than
being formatted into the message. The two failure modes now read differently, so
an operator can tell "the classifier disagrees with itself" from "the classifier
reported nothing".
*Tests:* `TestRefuseClassUnstableSentenceNeverLeaksGarbage` (9 cases),
`TestRefuseClassUnstableUnusableStabilityIsDistinct`, and the
`TestRefusalShapeInvariant` corpus which asserts no refusal detail from any
predicate ever contains `NaN` or a `%!` formatting artifact.

### 6. `±Inf` statistics manufactured a "no action needed" verdict

`shift.go`. The doc claimed "garbage statistics can never manufacture a season or
a trend", but only NaN was actually excluded: `AutoCorr24h = +Inf` satisfied
`>= 0.5` and returned `seasonal`, and `±Inf` trend returned `trend`. Both of
those verdicts mean *no regime handling is needed* — the permissive direction.
Autocorrelation is mathematically confined to `[-1,1]` (see `patterns.Features`),
so anything outside that band is a broken producer, not a season.

*Fixed:* both comparisons are now bounded on **both** sides and in positive form,
so NaN and ±Inf fall through to `indeterminate`.
*Tests:* `TestClassifyShift` (`+Inf autocorr is not a season`, `autocorr above 1
is not a season`, `±Inf trend is not a trend`, `large but finite trend is still a
trend`).

### 7. `Dropped` was lost across a checkpoint restore

`changepoint.go`. `ChangepointCheckpoint` carried every state field except
`dropped`. An operator watching the rejected-garbage counter as a data-quality
metric would see it reset to zero on every brain restart, which reads as "the
pipeline got clean" rather than "we forgot".

*Fixed:* `Dropped` is part of the checkpoint and is validated (`>= 0`) on restore.
*Tests:* `TestChangepointCheckpointRoundTrip` (feeds NaN and a negative so the
counter is non-zero), `TestChangepointFromCheckpointRejectsCorruptState`
(`negative dropped`).

### 8. (hardening, not a live bug) implementation-defined `float64 → int64` in the OOM floor

`floors.go` computed the decayed floor as an absolute `float64` and converted with
`int64(math.Ceil(eff))`. Converting an out-of-range `float64` to `int64` is
**implementation-defined** in Go: arm64 saturates to `MaxInt64`, amd64 yields
`MinInt64`. `float64(floorBytes)` rounds *up* past `MaxInt64` for a floor near the
int64 ceiling, and on amd64 the wrapped `MinInt64` would then hit the
`out < observedBytes` clamp and collapse the floor down to `observedBytes` — a
silently *lower* memory floor than the OOM evidence supports, which is the one
direction a memory floor must never fail.

**I could not construct an input that reaches it.** The `age <= Hold` early
return keeps `factor` strictly below 1, which keeps the product strictly below
2⁶³; a search over floors near the int64 ceiling × 8 observed values × 8 ages
found zero reaching combinations. Reported as a latent hazard, not a live bug.
It was fixed anyway because removing the implementation-defined conversion costs
nothing: the decay now runs on the *gap* in integer space, where
`observedBytes + add` is provably in range.

That rewrite changed one documented semantic, which a test caught: ceiling the
gap alone left a permanent `observed + 1` asymptote instead of relaxing to
`observed`. The gap now rounds to zero once the remaining excess is below one
byte, restoring "relaxes toward the observed-peak term".
*Tests:* `TestEffectiveOOMFloorHugeArmedFloor`, `TestEffectiveOOMFloorBounds`
(8 floors × 7 observed × 8 ages × 3 configs), `TestEffectiveOOMFloor`
(`ancient OOM relaxes to observed`).

### Two failures that were my test's fault, not the code's

Recorded for honesty:

- `TestVerdictJSONShape/zero until is omitted` asserted on raw JSON bytes, but the
  refusal *sentence* legitimately contains the word "until". `omitzero` was
  working correctly. The test now inspects the decoded object's keys.
- `TestRefuseEverythingIsNeverTheBestPolicy` drew defects from `uniform()*9` with
  6 defective branches, making two thirds of the synthetic fleet broken. The
  engine's 204/600 act rate was correct behaviour on a fleet that bad. Fixed the
  generator, not the engine.

---

## Surface

| file | contents |
|---|---|
| `decision.go` | `Config` + `Validate`, `Action`, `Verdict`, `Decide` |
| `confidence.go` | `Confidence`, `ConfidenceTerm`, `Compose`, 8 `Term*` constructors |
| `refusal.go` | `RefusalCode`, `Refusal`, `Evidence`, `SoakFor`, 8 predicates, `Evaluate` |
| `changepoint.go` | `ChangepointConfig`, `Changepoint` (online CUSUM), checkpointing |
| `floors.go` | `FloorConfig`, `EffectiveOOMFloor`, `SustainedPeak`, `RobustPeak` |
| `shift.go` | `ShiftKind`, `ShiftFacts`, `ClassifyShift` |

`Compose` is a weighted product, so every term can only *reduce* the score — the
conservative direction for a gate that acts above a threshold — and a zero-valued
term vetoes outright. Terms are a slice in caller order, never a map, so JSON
output is byte-stable. `TestComposeLegacyBackCompat` pins that the three legacy
terms reproduce `recommend.confidence()` exactly, so adopting this is a no-op for
existing `MinConfidence` filtering.

---

## Tunable constants and why they have that value

**`Config`** (`decision.go`) — `MinSamples 30` / `MinWindow 6h` match
`recommend.Config` so the refusal surfaces exactly where the recommender used to
skip silently. `BaseSoak 6h` covers a deploy warm-up transient at 5-min sampling.
`ClassFlipWindow 24h` is one full daily cycle. `MinClassStability 0.7` is far
above chance agreement across ~5 classes while tolerating steady↔diurnal flicker.
`MaxHPAThrashPerHour 2.0` — a healthy diurnal HPA reverses ~0.1/h; 2/h means
replicas oscillate every ≤30 min and per-replica usage mixes regimes.
`MaxForecastDivergence 0.35` exceeds `MemoryHeadroom−1 (0.20) + MinChangeRatio
(0.10)`, i.e. a disagreement standard headroom could not absorb — asserted by
`TestDefaultConfigMatchesSiblingPackages`. `ActConfidence 0.6` matches
`plan.Config.MinConfidence`, so "act" means "the planner would accept this today".
`maxConfigWindow 30d` / `maxSoak 7d` bound the duration knobs (bug 1).

**Soak multipliers** (`refusal.go`) — steady/unknown/growing ×1, bursty ×2
(heavy tails need more coverage), diurnal ×4 and batch ×4 (24h at the 6h base:
one full cycle, or one run of a nightly job).

**`ChangepointConfig`** (`changepoint.go`) — `Alpha 0.02` (~100-sample memory,
≈8h at 5-min samples); the baseline must adapt slower than the CUSUM accumulates
or shifts are absorbed before they can fire. `DriftK 0.5` is the classical
`k = δ*/2` for fastest detection of 1σ shifts; shifts below it belong to
`patterns.TrendPerDay`, not regime detection. `ThresholdH 5` gives ~10-sample
(~50 min) detection of a true 1σ shift. `ZClamp 4` winsorizes accumulation **and**
the baseline update (bug 3). `Warmup 24` (2h) is both settle time and the re-arm
holdoff. `MinSigmaFrac 0.05` stops quantization wiggle on a flat series from
standardizing to infinity.

`Validate` enforces `ThresholdH > ZClamp − DriftK`, which is what makes
"one balloon is an anomaly, never a regime" a structural guarantee rather than a
hope — tested both as a rejected config and as behaviour
(`TestChangepointOneSampleExcursionNeverFires`).

**`FloorConfig`** (`floors.go`) — `Hold 14d` = two weekly business cycles OOM-free
before an OOM-derived constraint may relax. `DecayPerWeek 0.10` halves the excess
in ~6.6 weeks, slow enough that a monthly OOM pattern re-arms it long before it
fades. `TestDefaultFloorConfig` asserts the half-life claim is arithmetically true.

**`shift.go`** — `spikeRevertMaxSamples 3` (15 min), `regimeSustainMinSamples 6`
(30 min), `seasonalAutoCorrMin 0.5` and `trendPerDayMin 0.10` deliberately equal
`pkg/patterns`' diurnal and growing thresholds; `TestShiftThresholdsMatchPatterns`
fails if they drift apart, because an exceedance called "seasonal" here while the
classifier calls the workload "steady" is a contradiction the operator would see.

---

## Invariants and where they are tested

| invariant | test |
|---|---|
| Every refusal carries a machine-readable code **and** a human sentence, with no `NaN`/`%!` artifacts | `TestRefusalShapeInvariant` |
| Refusal precedence is exactly the §4.2 table order, first match wins | `TestEvaluateOrder` |
| Every documented refusal code is reachable (no dead safety net) | `TestEveryRefusalCodeIsReachable` |
| One test per refusal predicate | `TestRefuse*` (8 tests) |
| Adding a defect can only move a subject toward abstention | `TestRefusalIsMonotonicInEvidenceQuality` (12 defects × 12) |
| Refuse-everything never scores as the best policy | `TestRefuseEverythingIsNeverTheBestPolicy` |
| `SoakFor` ∈ (0, maxSoak] for every base and class | `TestSoakForNeverNonPositive` |
| The soak refusal and the soak confidence term never disagree | `TestSoakForIsConsistentWithTermPostChangeSoak` |
| CUSUM detection delay = `⌊H/(min(δ,ZClamp)−K)⌋+1`, direction and magnitude correct | `TestChangepointStepDetectionDelay` |
| Sub-`DriftK` shifts never fire, over 5,000 samples | `TestChangepointSubDriftShiftNeverFires` |
| No single sample can fire the detector | `TestChangepointOneSampleExcursionNeverFires` |
| A spike cannot blind the detector to the next real shift | `TestChangepointSpikeDoesNotBlindDetector` |
| Garbage samples never fire, never move the baseline, never advance warmup | `TestChangepointGarbageSamplesAreDropped` |
| Timestamps are recorded, never used for math | `TestChangepointTimestampsAreRecordedNotUsed` |
| Restore is behaviourally identical to the original detector | `TestChangepointCheckpointRoundTrip` |
| A live detector's state is always restorable | `FuzzChangepointAdd` (25M execs clean) |
| OOM floor ∈ `[observed, armed]`, monotonically non-increasing in age | `TestEffectiveOOMFloorBounds`, `…IsMonotonicInAge` |
| Garbage `FloorConfig` yields exactly the default-config result | `TestEffectiveOOMFloorGarbageConfigFallsBackToDefaults` |
| `SustainedPeak` is non-increasing in `run` and always a level the series reached | `…IsNonIncreasingInRun`, `FuzzSustainedPeak` |
| `ClassifyShift` is total: every input → one of 5 kinds + a reason | `TestClassifyShiftIsTotalAndDeterministic` (~1.2M combos) |
| `Compose` score ∈ [0,1] for any garbage terms | `TestComposeScoreAlwaysInRange` |
| Legacy confidence reproduced exactly | `TestComposeLegacyBackCompat` |
| `Decide`/`Evaluate` are pure and deterministic | `TestDecideIsDeterministicAndPure`, `TestEvaluateDoesNotMutateEvidence` |

Fuzz targets: `FuzzChangepointAdd`, `FuzzSustainedPeak`. Both were run well past
the seed corpus (25M and 6.4M executions) with no failures; no crashers were
written to `testdata/`.

---

## Wiring: what a later unit must change in `pkg/recommend`

**Not done here** — `pkg/recommend` is owned by another agent right now, and this
package deliberately does not import it (dependency direction is one-way).
Line numbers are against the current `main`.

1. **`recommend.go:315` and `recommend.go:319`** — the two silent `continue`s
   (`st.samples < r.cfg.MinSamples`, `window < r.cfg.MinWindow`). This is
   §4.2's headline case. Replace with `decision.Evaluate(...)`; on a non-nil
   `*Refusal`, emit a `Recommendation` carrying the `Verdict` instead of
   dropping the subject. `decision.DefaultConfig()` already mirrors these two
   thresholds, so behaviour is unchanged apart from the subject now being visible.

2. **`recommend.go:455` `confidence()`** — replace the body with
   `decision.Compose(TermHistoryDepth(st.samples, cfg.MinSamples*4),
   TermWindowSpan(window, 2*cfg.MinWindow), TermVolatility(st.spikes.SpikeRate()))`
   and keep `.Score` for the existing float field. `TestComposeLegacyBackCompat`
   proves this is numerically identical, so `plan.Build`'s `MinConfidence`
   filtering is unaffected. Then add `class-stability`, `post-change-soak`,
   `freshness` and `forecast-agreement` terms as unit 1's substrate supplies them.

3. **`recommend.go:132` `Confidence float64`** — add
   `Verdict *decision.Verdict \`json:"verdict,omitempty"\`` alongside it. Keep the
   float for one release so the API and UI are not broken in the same change.

4. **`recommend.go:349` `memPeak := st.mem.Max()`** (and the identical
   `insights.go:45`) — this is the "one 30-second balloon sets sizing forever"
   path. Replace with
   `decision.RobustPeak(st.mem.Percentile(0.999), decision.SustainedPeak(recent, k))`.
   Needs a bounded recent-sample ring on `containerState`, which `pkg/histogram`
   does not retain today — that ring is unit 1's job.

5. **`recommend.go:362` and `recommend.go:406`** — `st.oomFloorBytes` is applied
   as a permanent constraint. Wrap both in
   `decision.EffectiveOOMFloor(st.oomFloorBytes, st.lastOOM, now, memPeak, cfg)`.
   **Requires a new `lastOOM time.Time` field** on `containerState`, persisted in
   `containerSnapshot` next to `OOMFloor` (`recommend.go:494`, restored at
   `recommend.go:544`). Without it the floor cannot decay, because its age is
   unknown. `recommend.go:212` (where the floor is armed) is where `lastOOM`
   should be stamped.

6. **New per-container `*decision.Changepoint`**, fed alongside `st.memDet` /
   `st.cpuDet`. Its `Detection.At` becomes `Evidence.LastChangepoint`, and
   `ChangepointCheckpoint` must join `containerSnapshot` so detector state
   survives a restart. `Changepoint` is **not** safe for concurrent use; the
   recommender's existing per-key sharding under `r.mu` already provides the
   required serialization.

---

## Deferred, and why

- **The `pkg/recommend` wiring itself** — out of scope by instruction; that
  package is being edited concurrently. Specified above instead.
- **Fast-forward histogram decay on regime change** (§4.3.2) — the `shiftRef`
  math lives in `pkg/histogram`, outside this package's write scope.
  `ClassifyShift` returning `ShiftRegime` is the intended trigger.
- **Deploy-aware learning** (§4.3.3, image-change vs replica-change) — needs
  unit 1's deploy events. `Evidence.LastChange` is the seam and is already
  consumed by `RefusePostChangeSoak`.
- **Populating `Evidence`** — every field is filled by the caller from unit 1's
  substrate. This package deliberately owns no collection.
- **A `Verdict` scorer / backtest integration** (§9 unit 2) — the
  refuse-everything property is tested here against a synthetic fleet under an
  explicit value model, but scoring against real cluster history is unit 2's job.
- **`SustainedPeak` is O(n·run)**, not the O(n) monotonic-deque version. At the
  intended scale (a bounded recent ring, `run` ≈ 6) the deque's bookkeeping costs
  more than it saves, and the naive form is obviously correct. If a caller ever
  passes a long series, revisit — `TestSustainedPeakIsNonIncreasingInRun` and the
  fuzz target pin the semantics, so the swap would be safe.

## Known design tension (not a bug)

`ClassifyShift` ranks **seasonal above regime**: a diurnal workload with
`AutoCorr24h ≥ 0.5` is called seasonal even when the CUSUM fired and the level
held. That is deliberate — a diurnal workload fires the CUSUM on its own daily
ramp every single day, and the decaying histogram already covers seasons, so the
opposite precedence would make the engine re-learn once per day forever. The cost
is that a genuine regime change *on a strongly diurnal workload* is attributed to
the season until the autocorrelation itself decays. Resolving it properly needs a
seasonally-adjusted residual (subtract the daily profile before the CUSUM), which
needs unit 1's per-hour profile storage. Tested as an explicit expectation in
`TestClassifyShift/season_beats_regime`.
