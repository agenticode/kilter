# pkg/histogram hardening findings

Session scope: `pkg/histogram` only. Every fix below has a regression test that
was verified to FAIL against the pre-fix code (by swapping the original
`histogram.go` back in) and pass after.

## Bugs found and fixed

1. **NaN/±Inf weight poisons the histogram forever** (`AddSample`).
   `weight <= 0` is false for NaN, so `AddSample(v, NaN, t)` flowed a NaN into
   `weights[b]` and `total`. From then on every `Percentile` collapsed to the
   max bucket (all comparisons against a NaN threshold fail) with no error —
   silent, permanent over-recommendation. `+Inf` weight likewise made `total`
   infinite. Fixed with NaN-safe rejection; also drop `w == 0` (decay
   underflow) and `w == +Inf` (decay overflow) after applying the decay
   factor. Test: `TestAddSampleGarbageWeight`, `FuzzAddSample`.

2. **Out-of-range panic in `findBucket` for huge values (amd64)**.
   With `Ratio > 2`, `value*(Ratio-1)` overflows to `+Inf` for values near
   `MaxFloat64`; `int(+Inf)` is implementation-dependent — `MinInt64` on
   amd64 — producing a negative bucket index and an index-out-of-range panic
   in `AddSample`. (arm64 saturates to `MaxInt64`, accidentally masking it.)
   Fixed by clamping in the float domain before the int conversion. Test:
   `TestAddSampleHugeValueSafe`.

3. **`validate` accepted NaN/Inf options** — NaN fails every `<=` comparison,
   so `Options{FirstBucketSize: NaN, ...}` passed validation and produced a
   histogram that panics (negative bucket on amd64) or silently drops data.
   `+Inf` FirstBucketSize made every percentile `+Inf`. Fixed with
   positive-form (NaN-safe) comparisons, explicit Inf rejection, `Epsilon < 1`
   (at `Epsilon >= 1` every bucket is "negligible" and compactRange wipes the
   histogram), and a `MaxNumBuckets` cap (2^20) so a corrupt/hostile
   checkpoint cannot demand a multi-GiB allocation through `FromCheckpoint`.
   Tests: `TestValidateRejectsGarbageOptions`,
   `TestFromCheckpointRejectsOversizedLayout`.

4. **`Merge(nil)` panicked** with a nil dereference. Now returns an error.
   Test: `TestMergeNil`.

5. **`h.Merge(h)` double-counted every sample** (weights and total doubled).
   Percentiles happened to survive (proportions preserved) but checkpointed
   totals were wrong. Self-merge is now a documented no-op. Test:
   `TestMergeSelf`.

6. **`Merge` could put weight outside `[minB, maxB]`**. It copied every
   nonzero source bucket (including sub-epsilon crumbs that decay had pushed
   outside the source's own range) but took range bounds from the source's
   `minB/maxB`. Weight inside the destination but invisible to the percentile
   scan skews `cum` against `total`. The loop now tracks the buckets actually
   written. Test: `TestMergeRangeCoversCrumbs`.

7. **Declared-empty histograms kept ghost weights** (`compactRange`). When
   every bucket fell below the epsilon threshold, `total` was zeroed but the
   bucket weights were left in place. `Checkpoint` exports every nonzero
   bucket and `FromCheckpoint` recomputes `total` from them, so decayed-away
   weight resurrected on restore (restored total ≠ live total). Reachable
   with valid-but-coarse options (`Epsilon*NumBuckets >= 1`); impossible with
   the defaults, but the state machine should not depend on that. Declaring
   empty now clears the weights. Test: `TestMergeGhostWeights`.

8. **`FromCheckpoint` silently emptied on total overflow**. Individually
   finite bucket weights can sum past `MaxFloat64`; the infinite total made
   every epsilon threshold infinite and `compactRange` wiped the histogram —
   valid-looking, empty, no error. Now rejected explicitly. Test:
   `TestFromCheckpointTotalOverflow`.

9. **`Max`/high percentiles tracked a single negligible outlier**. When the
   threshold crossing never "stuck" (trailing sub-epsilon buckets, or rounding
   keeping `cum` just under `threshold`), `Percentile` fell back to `maxB`,
   which `AddSample` extends without any epsilon check — so one long-decayed
   spike dictated `Max()` forever, contradicting its documented
   "non-negligible" contract and inflating recommendations. The fallback is
   now the highest non-negligible bucket. Test:
   `TestMaxIgnoresNegligibleOutlier`.

10. **Underflowed ancient samples smeared `minB`** — a sample predating
    `refTime` by ~1100 half-lives decays to weight exactly 0 but still
    extended `minB/maxB` over an empty bucket. Now skipped. Test:
    `TestAncientSampleUnderflowIgnored`.

## Behavior pinned down (not bugs, now specified + tested)

- `Percentile(NaN)` clamps to 1 (the conservative end) instead of silently
  hitting the fallback path (`TestPercentileNaN`).
- Values beyond the last bucket's start are reported as that start — a finite
  underestimate, documented on `Percentile` (`TestLastBucketClamp`).
- Exact bucket-boundary values (and their float neighbors) land in the right
  bucket across adversarial layouts, including `Ratio=1.001`
  (`TestFindBucketInvariantAcrossOptions`); the boundary correction in
  `findBucket` is now a bounded loop rather than a single step.
- Bucketized percentiles match a brute-force weighted quantile exactly at the
  bucket level (`TestPercentileBruteForce`); percentiles are monotone in `p`
  across decay and merges (`TestPercentileMonotone`).
- Merging into an empty histogram reproduces the source's percentiles exactly
  (`TestMergeIntoEmpty`).
- New fuzz target `FuzzAddSample`: arbitrary sample pairs keep `total` finite,
  consistent with the bucket sum, and percentiles finite and monotone.

## Docs corrected

- `DefaultCPUOptions` claimed "10m .. ~1000 cores"; the layout actually
  reaches ~23,000 cores (`bucketStart(239) ≈ 2.3e7` mCPU). `DefaultMemoryOptions`
  claimed "~1.3TiB"; actual ≈ 22 TiB. Comments now state the real ranges
  (the layout constants themselves were left untouched — changing them would
  rebucket existing checkpoints).
- Documented: `Total` in `Checkpoint` is informational (recomputed on
  restore); `shiftRef` only ever scales down; why `compactRange` clears
  weights; the `Epsilon`-vs-`NumBuckets` interaction on `Options`.

## Deliberately left undone

- **`Merge` can still overflow `total` to `+Inf`** if two histograms each
  hold ~1e308 of weight. Guarding it would require rolling back a partially
  applied merge; the state to reach it is unreachable through `AddSample`
  (which caps per-sample weight growth) plus `FromCheckpoint` (which rejects
  infinite totals), so I left it and note it here instead.
- **`Epsilon*NumBuckets >= 1` is still accepted** by `validate`. Rejecting it
  would be the principled fix for the ghost-weight class (bug 7), but it
  could refuse to restore a previously written checkpoint, which is a worse
  failure mode for an operator. The state machine is instead hardened to
  behave correctly under such options.
- **`FromCheckpoint` accepts a zero `RefTime` with nonzero buckets**; the
  first subsequent sample re-anchors the decay clock, effectively treating
  restored weight as current. This matches upstream VPA semantics and only
  arises from hand-crafted checkpoints, so behavior was kept.
