# pkg/recommend hardening findings

## Bugs found and fixed

1. **Existing limit silently dropped when the request was unset** (`recommendOne`).
   The limits policy required `curLim > 0 && curReq > 0` to compute a ratio; a
   container with a limit but no request got `TargetLimit = 0`, i.e. the
   recommendation removed an existing limit — changing QoS class and node OOM
   behavior. Now a limit with an undefined ratio is carried forward unchanged.
   Test: `TestLimitPreservedWhenRequestUnset`.

2. **Emitted limit could sit below the emitted request.** With garbage snapshot
   data (limit < request, ratio < 1) the target limit came out below the target
   request — a spec the API server rejects, so actuation would fail. Both
   dimensions now clamp `TargetLimit >= TargetRequest`.
   Test: `TestLimitNeverBelowRequest`, plus the property test.

3. **OOM floor lost to integer overflow on amd64.** `int64(float64(oomedAt) *
   OOMBumpRatio)` overflows for huge limits; out-of-range float→int64
   conversion is implementation-specific in Go — arm64 saturates, amd64 wraps
   to MinInt64. On amd64 the bumped floor went negative and was silently
   discarded, exactly when an OOM had just been observed. All float→int64
   target conversions now go through a saturating `ceilInt64` helper.
   Test: `TestOOMBumpSaturatesInsteadOfOverflowing`, `TestCeilInt64`.

4. **Garbage usage samples were learned.** Negative usage values were clamped
   to zero by the histogram (dragging percentiles toward an unsafe shrink) and
   a zero timestamp anchored `firstSample` at year 1, inflating the observation
   window and therefore the confidence score. Such samples are now skipped
   entirely. Test: `TestGarbageUsageSkipped`.

5. **Unbounded sample weight from `WindowSeconds`.** A single sample claiming a
   huge averaging window (e.g. MaxInt32 seconds → weight ~35M) drowned the
   entire learned distribution; the real peak became epsilon-negligible and
   both targets collapsed toward the garbage value. Weight is now capped at one
   hour's worth (60×). Test: `TestHugeWindowSecondsCapped`.

6. **`Config.validate` accepted degenerate gates.** `MinSamples=0` /
   `MinWindow=0` allowed recommendations from empty histograms (target = bare
   floors → shrink-to-nothing) and produced NaN confidence (0/0), which also
   breaks JSON marshaling of the recommendation. Validation now requires
   `MinSamples >= 1`, `MinWindow > 0`, non-negative floors. All in-repo callers
   already satisfy this. Test: `TestConfigValidate`.

7. **`Restore` trusted self-inconsistent checkpoint scalars.** Negative
   samples/OOM counters/floor, a backwards time range, or a one-sided zero
   timestamp restored cleanly and inflated the window and confidence — acting
   confidently on corrupt state. Such entries are now skipped, matching the
   documented "corrupt entries are skipped" contract.
   Test: `TestRestoreRejectsCorruptEntries`.

8. **Nil-snapshot panics.** `ObserveSnapshot`/`Recommendations`/`Insights`
   dereferenced a nil snapshot. Now no-ops. Test: `TestNilSnapshotSafe`.

Cleanups: `insights.go` compared the learned class against the string literal
`"growing"` instead of `patterns.ClassGrowing`; dead `_ = key` in the prune
loop removed. The OOM bump now rounds up instead of truncating (one byte more
conservative).

All fixes were verified fail-before/pass-after by temporarily reverting the
production changes and re-running the new tests (all failed as expected).

## Tests added

- Table-driven: config validation, `significant` boundaries (exactly-at-ratio),
  `ceilInt64` saturation, corrupt-checkpoint rejection (incl. mixed batches),
  GC cutoff boundary (exact timestamp kept), HPA variants (keda owner in
  reason, memory-based HPA not skipped, `SkipCPUForHPA=false`).
- Adversarial: garbage usage, huge `WindowSeconds`, limit-without-request,
  inverted limit/request, near-MaxInt64 OOM, zero-request floors, usage-only
  keys (tracked but never recommended), insight severity ordering.
- Property/fuzz: `runInvariantScenario` checks decision-safety invariants
  (targets ≥ floors, memory target ≥ observed peak, OOM floor honored, limits
  never invented/dropped/below request, HPA CPU byte-identical, confidence a
  real number in [0,1]) across 150 seeded scenarios per run
  (`TestRecommendationInvariantsProperty`) and a fuzz target
  (`FuzzRecommendationInvariants`, 140k execs clean in a 15s burst).

## Invariants documented in code

- First sight of a pod only seeds the restart baseline (pre-existing restart
  counts never bump the OOM floor).
- Emitted limits are never below emitted requests; existing limits are never
  dropped; missing limits are never invented.
- Why garbage samples are skipped rather than clamped.
- Why `ceilInt64` saturates (platform-specific conversion semantics).
- Why `MinSamples`/`MinWindow` must be positive.

## Deliberately left undone

- **Predicted-OOM horizon can be absurd**: with peak already ≥90% of the limit
  and a negligible positive slope, the warning reports horizons like ~9000h and
  attributes the risk to "growth". The trigger itself is defensible (workload
  sits near its limit); fixing the message/threshold is a detection-behavior
  change downstream layers may depend on, so left as-is.
- **Checkpoint does not persist pattern-detector rings, spike-detector state,
  or per-pod restart baselines.** After a restore the volatility penalty is 0
  (slightly optimistic confidence) and an OOM spanning the restart can be
  missed once. Needs a checkpoint format change; out of scope for this pass.
- **`Restore` cannot detect swapped CPU/Memory checkpoints** (both restore
  cleanly under their own Options). Rejecting unexpected Options would break
  operators running custom histogram options, so not enforced.
- **Mid-rollout replicas with divergent sizing**: `currents` keeps whichever
  replica iterates last; the delta is computed against an arbitrary replica.
  Harmless churn-wise (suppression re-evaluates next cycle) but worth a
  deterministic rule eventually.
- The memory growth projection multiplies the *peak* by the *mean-normalized*
  trend, overestimating growth for peaky series. The error is in the safe
  (larger) direction, so left.
