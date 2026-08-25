# pkg/forecast hardening findings

Session scope: pkg/forecast only. Every bug below was demonstrated with a test
that failed against the original code and passes after the fix.

## Bugs found and fixed

1. **Seasonal `Forecast` panic on large horizons** (`forecast.go`).
   `si := (hw.n - 1 + h) % hw.seasonLen` overflows int for large `h`, producing
   a negative index into `seasonal` → runtime panic. Fixed by reducing both
   terms modulo the season length before adding. Test:
   `TestForecastHugeHorizonNoPanic` (panicked before; also checks modular
   phase equivalence against `Forecast(h % L)`).

2. **`NewHoltWinters` accepted NaN smoothing parameters**. NaN fails every
   range comparison (`alpha <= 0`, `alpha > 1`, …), so `NewHoltWinters(NaN, …)`
   succeeded and every forecast became NaN — a NaN reaching the capacity
   planner via `forecastPeak`. Fixed with explicit `IsNaN` checks (±Inf was
   already caught by the range checks). Test: `TestNewHoltWintersRejectsNaNParams`.

3. **`NewSpikeDetector` accepted NaN/±Inf band width `k`**. `NaN <= 0` is
   false, so a NaN `k` passed validation and the resulting NaN bound meant the
   detector *silently never fired* — the scale-down veto guard disabled with no
   error. `k = +Inf` similarly yields a NaN band (`Inf*0`) once variance is 0.
   Fixed validation. Test: `TestNewSpikeDetectorRejectsBadK`.

4. **Finite-but-huge samples permanently poisoned model state**. Inputs like
   `1e308` are finite (so they passed the NaN/Inf filter) but overflow
   `delta*delta` and `level+trend`, driving EWMA mean/variance and
   Holt-Winters level/trend to ±Inf/NaN forever; `Forecast` then returned NaN
   (the `v < 0` clamp doesn't catch NaN) and `SpikeDetector` stopped firing.
   Fixed by widening the garbage filter to `|v| <= 1e150` (`maxAbsSample`),
   chosen so every intermediate product stays finite; real resource
   quantities are nowhere near this bound. Tests: `TestEWMAPoisonResistance`,
   `TestHoltWintersPoisonResistance`, `TestEWMAAcceptsBoundaryMagnitude`.
   Defense-in-depth: `Forecast` now also maps any non-finite result to the
   package's 0 "no usable forecast" sentinel.

5. **`SpikeDetector.Observe` mishandled garbage inputs**. `+Inf` counted as a
   spike, and NaN/Inf incremented `total`, diluting `SpikeRate` — the
   volatility penalty in pkg/recommend shrinks as garbage arrives, weakening
   the scale-down veto exactly when the data is least trustworthy. Garbage is
   now ignored entirely (no spike, no baseline update, no rate dilution).
   Test: `TestSpikeDetectorIgnoresGarbage`.

6. **`RemoteForecaster.Forecast` did not enforce its documented length
   contract**. The contract promises exactly `horizon` points but the code
   only checked `len > 0`. `forecastPeak` takes `maxOf()` over the response,
   so a short response silently under-covers the horizon → under-provisioning
   risk. Length mismatch is now an error (callers fall back to built-in
   models). Test: `TestRemoteForecastLengthContract`.

7. **`EWMA.UpperBound` doc/code mismatch**. Doc said "floored at the mean" but
   a negative or NaN `k` returned a bound below the mean (or NaN). The floor
   is now implemented. Test: `TestEWMAUpperBoundFloor`.

## Smaller hardening in remote.go

- Input series is validated for NaN/±Inf before marshalling, with the index in
  the error (json.Marshal would fail anyway, but without saying where).
- Response values now also reject ±Inf (previously only NaN/negative);
  invalid-value errors include the point index.
- Non-200 responses drain a bounded amount of body so keep-alive connections
  can be reused.
- The 8 MiB response cap is a named, documented constant (`maxResponseBytes`).

## Tests added

- `hardening_test.go` — boundary/adversarial tables: constructor validation,
  overflow poisoning, garbage-vs-clean state equivalence for seasonal HW
  (garbage must not shift seasonal phase), huge/zero/negative horizons,
  Ready() boundary at exactly 2L samples, single-sample EWMA, alpha=1 edge,
  warm-up boundary (10th vs 11th observation), empty SpikeRate, DefaultDemand
  sanity, and a property test that the EWMA mean never escapes the input
  envelope (convex combination invariant).
- `remote_test.go` — httptest coverage for the previously untested client:
  URL validation table, happy path with payload verification, length-contract
  violations, bad response bodies (negative, empty, malformed, wrong type),
  HTTP 500, invalid inputs short-circuiting before any HTTP call, cancelled
  context, and a >8 MiB oversized response.
- `fuzz_test.go` — three fuzz targets (EWMA, HoltWinters trend+seasonal,
  SpikeDetector) decoding raw bytes into float64 streams with adversarial
  seeds (1e308 alternation, NaN/Inf, subnormals). Invariants: state and
  forecasts always finite and non-negative, `UpperBound(3) >= Mean`,
  `SpikeRate` in [0,1], no panics — including `Forecast(math.MaxInt)` after
  every sample. Each target ran ~15s locally (4.5M+ execs total), no crashes.

## Invariants documented

- Package doc: models are not goroutine-safe; callers serialize per series.
- `maxAbsSample` (1e150): why the bound exists and why intermediates stay
  finite under it.
- Garbage-sample semantics on `EWMA.Add`, `HoltWinters.Add`,
  `SpikeDetector.Observe` (ignored; do not count toward N; do not shift
  seasonal phase; do not dilute SpikeRate).
- `Forecast`'s 0 return as the package-wide "no usable forecast" sentinel.
- Seasonal index overflow reasoning at the fixed line.
- Remote client: exact-length contract, 10s timeout, response size cap.

## Considered and deliberately left

- **`SpikeRate` denominator includes the 10 warm-up samples**, slightly
  deflating the rate early on. This matches the documented "fraction of
  observed samples" and pkg/recommend's tuning consumes it as-is; changing
  the denominator would silently retune the volatility penalty, so I left it.
- **Spikes are absorbed into the EWMA baseline** (`Observe` adds `v` even when
  flagged). This inflates the band after a spike — a deliberate-looking
  design (repeated similar values shouldn't re-flag), not a bug; changing it
  would alter detector sensitivity downstream.
- **`Forecast(0)` cannot be distinguished from a genuine zero-demand
  forecast.** A signature change (error or ok-bool return) would touch
  callers outside this package, which is out of scope for this session.
