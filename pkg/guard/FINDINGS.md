# pkg/guard hardening findings

## Bugs found and fixed

1. **`ParseWindows` accepted garbage via `fmt.Sscanf`.** `"Mon 22:00-06:00:30"`,
   `"Mon 22:00-06:00x"`, and `"Mon +2:00-06:00"` all parsed silently — Sscanf
   ignores trailing text and accepts signs, so an operator's typo produced a
   *different* change window instead of a startup error. Replaced with a strict
   digits-only `parseHHMM` (single colon, no signs, no trailing text, full
   consumption of the range via `strings.Cut`).
2. **`ParseWindows` accepted ends past 24:00.** `"00:00-24:30"` passed the old
   range check (`eh <= 24 && em <= 59`) and produced `End=1470`, violating the
   minutes-in-`[0,1440]` invariant `InWindow` relies on. Ends are now capped at
   24:00 exactly (`eh == 24 → em == 0`).
3. **NaN `MaxNotReadyFraction` silently disabled the node-health breaker.**
   The old default guard was `<= 0`; NaN fails every comparison, so it slipped
   through and `ratio > NaN` was always false — a garbage config turned off the
   circuit breaker with no error. `withDefaults` now uses `!(x > 0)`.
4. **Nil-snapshot panics.** `ModeFor(nil, …)` and `Breaker(nil, …)` both
   dereferenced nil. Both now fail safe: `ModeFor` returns `ModeRecommend`
   (annotations — including opt-outs — are invisible without a snapshot, so
   never act), and `Breaker` opens with reason "no cluster snapshot".
5. **Breaker failed open on an empty node list.** A snapshot with zero nodes
   (failed collection, vanished cluster) reported "healthy" because the
   NotReady check was guarded by `total > 0` and nothing else fired. The
   breaker now opens with "snapshot has no nodes".

Each fix is covered by a test in `hardening_test.go` that fails against the
previous implementation (verified by restoring `git show HEAD:pkg/guard/guard.go`
and re-running: the parser cases fail, `TestModeForHardening` segfaults).

## Tests added (`hardening_test.go`)

- `TestParseWindowsStrict` — 33-case table of valid/garbage/adversarial specs:
  overflow digit runs, non-ASCII digits, dangling/doubled day ranges, trailing
  commas/fields, minute 60, hour 24/25, missing separators.
- `TestParseWindowsInvariants` — week-wrapping day range `Fri-Mon`,
  end-at-midnight `22:00-00:00`, parsed minutes stay in documented ranges.
- `TestInWindowMatchesReference` — differential test: sweeps all 10,080
  minutes of a week against an independent interval-expansion reference, for
  eight window shapes (cross-midnight, 24 h wrap `10:00-10:00`, full-week
  `Sun-Sat 00:00-00:00`, overlapping multi-window sets).
- `TestInWindowEdges` — hand-built window with zero days never matches;
  sub-minute times just inside/outside a boundary.
- `TestModeForHardening` — nil snapshot, duplicate refs (first-valid-wins,
  now documented and locked in), invalid-annotation skip, namespace-mode
  isolation, invalid-default fallback.
- `TestBreakerHardening` — NaN config, exact-threshold boundaries (1/5 vs 2/5
  NotReady at 0.2; 10 vs 11 pending at 10), fraction ≥ 1 tolerating 100%
  NotReady, freeze short-circuit yielding exactly one reason, nil/empty
  snapshots.
- `FuzzParseWindows` — no panics; every parsed window covers ≥ 1 day and has
  `Start ∈ [0,1439]`, `End ∈ [0,1440]`; `InWindow` total on all results.
  A 30 s local run (~13 M execs) found nothing.

## Invariants documented in godoc

- `Window`: `Start ∈ [0,1439]` inclusive, `End ∈ [0,1440]` exclusive
  (24:00 == 1440); `End <= Start` crosses midnight; `Start == End` is a full
  24 h window.
- `InWindow`: compares wall clock in `t`'s location — the caller chooses the
  timezone the windows are written in.
- `ModeFor`: precedence chain, invalid-annotation skipping, first-valid-wins
  for duplicate refs, `ModeApply` fallback, nil-snapshot behavior.
- `BreakerConfig`: zero/negative/NaN fraction → default 0.2; fraction ≥ 1
  disables the node-health check; `MaxPendingPods <= 0` → default 10.
- `Breaker`: degenerate-snapshot fail-safe; freeze short-circuits with a
  single reason without evaluating other health signals.

## Deliberately left undone

- **No way to configure "trip on any NotReady node"** — a zero
  `MaxNotReadyFraction` means "use the default", so the strictest expressible
  setting is a tiny positive epsilon. Fixing this needs a sentinel or pointer
  field, i.e. an API change beyond hardening scope; documented instead.
- **Trailing commas in window specs still error.** Lenient skipping of empty
  segments would be friendlier, but failing at startup is the safer behavior
  for a guard, so the strictness was kept.
- **`ModeFor` is a linear scan per call** and `plan.go` calls it per pod
  (O(pods × workloads)). Fine at realistic snapshot sizes; an index would
  change the API or require caching, not worth it here.
- **Duplicate `WorkloadRef` entries resolve first-valid-wins**, not
  most-restrictive-wins. Changing that would alter behavior on real (if
  malformed) snapshots, so the existing rule was documented and test-locked
  instead.
