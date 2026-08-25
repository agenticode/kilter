# pkg/patterns hardening findings

## Bugs found and fixed

1. **Idle-fraction collapse at p95 == 0 (misclassification, real money impact).**
   A batch workload idle at *exactly* 0 for >95% of samples (e.g. a cron with
   one 5-minute spike every 2h) drives `p95` to 0, so the idle test
   `v < 0.10*p95` became `v < 0` and `IdleFrac` reported **0** for the idlest
   possible series. Repro classified it **diurnal** (`idle=0%`); random-spike
   variants classify **bursty**. Both apply *more* headroom / higher
   percentile instead of batch's lower percentile — the opposite of correct.
   Fix: when `p95 == 0`, count exact zeros as idle (`patterns.go`, Analyze).
   Regression test: `TestZeroIdleBatchRegression` (fails before, passes after).

2. **`percentile` could panic on out-of-range/NaN `p`.**
   `int(p * float64(n-1))` with NaN `p` is a platform-defined conversion (in
   practice a huge negative index → out-of-range panic); `p > 1` also indexed
   past the slice. Only constants are passed today, so unreachable — but it's
   one refactor away from a crash in decision-path code. Now clamped
   (`p <= 0`/NaN → min, `p >= 1` → max) and the truncating "lower-value"
   quantile convention is documented. Tests: `TestPercentile`.

3. **Finite-features invariant was not guaranteed.**
   `Add` rejected NaN/Inf/negative but accepted any finite magnitude, so a
   garbage upstream sample (e.g. unit-conversion blowup ~1e300) overflowed
   the sum/squared-deviation aggregates and produced Inf/NaN in `Mean`, `CV`,
   `TrendPerDay`, `AutoCorr24h`. Those feed straight into arithmetic in
   pkg/recommend (`TrendPerDay` scales projected memory) and into JSON
   (`json.Marshal` errors on non-finite floats). Fix: `Add` now also rejects
   `v > maxSample` (1e18 — an exabyte of memory bytes; far beyond any real
   telemetry), which provably keeps every downstream aggregate finite, plus a
   belt-and-braces non-finite guard in `slopePerDay`. The invariant is
   documented on `Analyze` and enforced by `TestFeatureInvariantsAdversarial`
   and `FuzzAnalyze` (10.6M execs, 45s run, no violations).

## Tests added (`hardening_test.go`)

- `TestZeroIdleBatchRegression` — bug 1 repro.
- `TestAddSampleValidation` — table: NaN/±Inf/negative/−0/subnormal/cap
  boundary (`Nextafter` above cap rejected, cap accepted)/MaxFloat64.
- `TestAnalyzeSampleBoundaries` — 0, 47, 48 samples (threshold edges).
- `TestRingWrapKeepsNewestInOrder` — ring overwrite keeps newest 576 in
  strictly increasing time order.
- `TestAllZeroSeriesIsBatch` — all-zero series → batch, finite features.
- `TestPercentile` — empty/single/two-element/p0/p1/clamps/no-mutation.
- `TestSortFloatsMatchesStdlib` — shell sort vs `sort.Float64s` property
  check (duplicates, reverse-sorted, n ∈ {0,1,2,3,17,576}).
- `TestClassifyRuleTable` — pins the priority order of the rule chain
  (growth > batch > diurnal > bursty > steady; threshold edges non-inclusive).
- `TestPolicyForBounds` — bursty 0.99 cap, batch 0.85 floor, steady
  headroom floors, diurnal/unknown keep base; sane outputs for all classes.
- `TestAntiPhaseAutocorrelation` — 48h-period signal → strongly negative
  AutoCorr24h (negative clamp side), not diurnal.
- `TestFeatureInvariantsAdversarial` — cap-valued constants, cap/zero
  alternation, subnormals with one cap spike, duplicate timestamps,
  zero-value times, 1-second cadence, out-of-order times, year-9999 times,
  random walk — all must yield finite, range-correct features, no panic.
- `FuzzAnalyze` — byte-decoded (Δt, float64-bits) series through Add/Analyze
  asserting the finiteness/range invariants and no-classify-below-threshold.

## Invariants documented

- `Analyze`: all features finite; CV ≥ 0; AutoCorr24h ∈ [−1,1]; SpikeRate,
  IdleFrac, MedianRatio ∈ [0,1].
- `Detector`: samples expected in non-decreasing time order; out-of-order
  input degrades trend/autocorr quality but cannot panic or corrupt state.
- `percentile`: lower-value (truncating-index) quantile — biases slightly
  low, the safe direction for the idle threshold it feeds.
- `Policy` fields: units/ranges documented; `PolicyFor` documents that base
  values are assumed config-validated (garbage in → garbage out).
- `Features.String` now includes `med=` (MedianRatio) since the
  batch-vs-bursty rule hinges on it — explainability requirement.

## Deliberately left undone

- **`PolicyFor` does not sanitize garbage base values** (NaN/negative
  percentile or headroom propagate). Callers own config validation and other
  agents own the calling package; inventing fallback policy values here could
  mask config bugs. Documented the precondition instead.
- **Transient "growing" misclassification during ramp-up**: a strong diurnal
  signal observed for only ~1.5 periods (~36h) can have a nonzero
  least-squares slope > 10%/day and classify as growing until more history
  accumulates. This errs toward over-provisioning (safe direction) and
  self-corrects within hours as the ring fills; distinguishing it robustly
  would require seasonal decomposition, out of scope for a hardening pass.
- **`percentile` uses truncation, not linear interpolation.** Changing the
  quantile method would silently shift every existing classification
  boundary; the convention is now documented and its low bias is the safe
  direction for how it is used.
