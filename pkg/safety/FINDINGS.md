# pkg/safety — hardening findings

Scope: `pkg/safety/` only. No files outside this directory were touched.

Method: tests were written first, against the package exactly as it stood, then
each failure was triaged as either a real defect (fix production code) or a
wrong expectation of my own (fix the test). Every fix and every load-bearing
boundary was then mutation-checked: the behaviour was reverted or shifted in
isolation and the named test confirmed to fail without it. **25 of 25 mutants
are killed.** Three survivors from an earlier round were what drove the last
three tests below; a fourth was dead code and was deleted rather than shipped
untestable.

---

## Bugs found by tests

### 1. `RegressionDetector` could not see a crashloop it caused — the whole point of the type
**Severity: high (silent false negative in the revert path).**

`changeRecord` stored a single restart/OOM total for the *workload* and compared
it against the workload's later total. But restart counters live on the **pod**,
and a resize replaces every pod. So the baseline was the old pods' lifetime
totals while the "after" reading was fresh pods starting at zero.

A workload whose pods had accumulated 40 restarts before the change would have
to reach **43 restarts on brand-new pods** before the detector fired. The same
trap applied to OOMs: a workload that had OOMed before the change (`ooms = 1`)
could never report a fresh OOM, because the new pod's `1 > 1` is false.

Two further consequences of summing across the workload:
- a pod retiring with a large restart history *offset* another pod's genuine new
  crashloop, hiding it;
- any counter decrease (kubelet reset, container replaced) acted as credit
  against other pods' increases.

Fix: baselines are now per pod, keyed by UID (`podHealthKey`, with a
namespace/name fallback for hand-built specs). `changeRecord.sinceChange`
compares each pod against *its own* baseline; pods absent from the baseline were
created by the change, so everything they report counts; a decrease contributes
zero and can never offset another pod.

The key has to be the **UID**, not the name: a StatefulSet recreates a pod under
the same name with a new UID, so name-keying would reproduce the identical bug
one level down — comparing a fresh `web-0` against its predecessor's lifetime
total. A mutation that dropped the UID branch initially survived the suite;
`TestRegressionStatefulSetPodNameReuse` now closes it.

Tests: `TestRegressionSurvivesPodReplacement`, `TestRegressionOOMOnReplacementPod`,
`TestRegressionNotMaskedByRetiredPod`, `TestRegressionSurvivingPodCounterReset`,
`TestRegressionStatefulSetPodNameReuse`, `TestRegressionKeylessPodsAccumulate`,
`TestRegressionThresholdTable`.

### 2. Nil-dereference panic on a failed collection
**Severity: high (crashes the reconcile loop).**

`workloadHealth` did `for i := range snap.Pods` with no nil check, so
`Check(nil, now)` and `RecordChange(ref, nil, now)` panicked. The controller
calls both with a snapshot obtained from a collector that can fail.

Fix: a nil snapshot yields an empty health index — missing evidence, not a clean
bill of health. `Check` reports nothing and leaves watches armed (reverting on no
evidence is itself production disruption); watches still expire on schedule.
`RecordChange` with a nil snapshot arms **nothing**, because an empty baseline
would read every pre-existing restart as new and revert a healthy change.

Tests: `TestRegressionNilSnapshot`, `TestRegressionRecordChangeNilSnapshot`.

### 3. `PDBGuard` waved a nil pod straight through
**Severity: medium (fail-open in the disruption gate).**

PDB coverage is decided by the pod's namespace, labels and UID. `Covers(nil)`
returns false, so a nil pod matched no budget and `CanEvict`/`Reserve` returned
`true, ""` — permission to evict a pod we cannot even read. This was
inconsistent with the package-level `CanEvict`/`BlocksDrain`, which already
fail closed on nil.

Fix: both refuse with a reason. `Release(nil)` is an explicit no-op.
Test: `TestPDBGuardNilPodFailsClosed`.

### 4. Unbalanced `Release` minted disruption budget that does not exist
**Severity: medium (defeats the type's stated purpose).**

`Release` incremented `DisruptionsAllowed` unconditionally, with no ceiling. The
guard exists "so a single plan can't overspend what the API would later refuse",
but three `Release` calls against a budget of 1 left the ledger at 4. This is
reachable: `pkg/plan.tryRemove` installs a `rollback()` closure invoked from
several failure paths, and the guard deliberately outlives one call.

Fix: `NewPDBGuard` records the collected allowance per PDB; `Release` restores
toward it and never past it. Balanced reserve/release round trips are unchanged.

Tests: `TestPDBGuardReleaseCannotMintBudget`, `TestPDBGuardReserveReleaseRoundTrip`,
`FuzzPDBGuardLedger`.

### 5. `Cooldowns.Remaining` contradicted `Cooldowns.Allow` on a garbage clock
**Severity: low (wrong operator-facing number; a livelock if a caller trusts it).**

`Remaining` computed `interval - now.Sub(t)`. `time.Time.Sub` saturates at the
`Duration` bounds, so a zero-value `time.Time` (a miswired caller) or a
far-past clock makes that subtraction overflow to a negative, and `Remaining`
returned **0 — "go ahead"** — while `Allow` denied. A caller that polls
`Remaining` and waits would spin forever.

Fix: `Remaining` now mirrors `Allow`'s predicate exactly and clamps instead of
wrapping. The invariant `Remaining > 0 ⟺ Allow denies` is asserted directly.

Tests: `TestCooldownsRemainingAgreesWithAllow`, `FuzzCooldownsInvariants`.

### 6. A corrupt negative PDB allowance stuck permanently below its own ceiling
**Severity: low (state hygiene; found by `FuzzPDBGuardLedger`, not by hand).**

The API never reports a negative `DisruptionsAllowed`, but a corrupt snapshot
could. Refusal behaviour was already correct, but the ledger carried a nonsense
number that also became its own `Release` ceiling after fix #4 — pinning it
negative forever. Normalised to zero at construction, in the guard's private
copy only; the caller's slice is not rewritten.

Tests: `TestPDBGuardNegativeBudgetRefuses`, `TestPDBGuardNormalisationDoesNotTouchCaller`.

### 7. `Check` returned regressions in Go's map order
**Severity: low (non-determinism in a decision-critical output).**

`Check` ranged over a map and returned the slice as built, so the order of
reverts, and of the log lines an operator reads during an incident, varied per
run. Now sorted by workload.

Test: `TestRegressionOrderIsDeterministic` (20 runs must agree).

---

## Not bugs — verified and pinned

Behaviours that looked suspicious, were checked, found correct, and now have a
test so they cannot drift:

- **Sliding-window arithmetic.** I initially wrote a `Budget` expectation that
  disagreed with the code; recomputing by hand showed the *code* was right and
  my expectation was wrong. `FuzzBudgetInvariants` now checks `Budget` against
  an independent reference model (keep every grant, filter on read) that cannot
  share a bug with the prune-in-place implementation. Clean over ~5M executions.
- **Window boundaries are half-open.** An event exactly on the cutoff has aged
  out; a cooldown expires exactly at `t+interval`; a regression watch is still
  live exactly at `t+window`; a quarantine is still in force exactly at `until`.
  All four pinned.
- **`Reserve` is atomic.** It checks every covering budget before decrementing
  any, so a refusal cannot half-spend a healthy budget covering another pod.
  Pinned by `TestPDBGuardReserveIsAtomic` (mutation-checked).
- **`NewPDBGuard`'s copy protects the caller.** `plan.go` passes `snap.PDBs`
  directly; reservations must not drain the snapshot later phases read.
  Pinned by `TestPDBGuardDoesNotMutateCallerSlice`.
- **The DaemonSet rule outranks `DoNotEvict` in `BlocksDrain`.** A DaemonSet pod
  cannot hold a node hostage — it does not survive the node either way — so the
  annotation does not pin it. Pinned as a table row.
- **`BlocksDrain` and `CanEvict` agree** for every non-DaemonSet pod, reason text
  included (`TestBlocksDrainAgreesWithCanEvict`, exhaustive over kind × flags).
- **`Budget`'s event log is a window, not a journal**, so a long-lived actuator
  cannot leak memory one eviction at a time (100k events, capacity asserted).

## Added functionality

- **`Cooldowns.Prune(now) int`.** The controller keys cooldowns by workload and
  node and keeps one tracker for the process lifetime, so every deleted workload
  and scaled-in node stayed resident forever. `Prune` changes no answer the
  tracker gives — to `Allow` and `Remaining` a lapsed key and an unknown key are
  already identical — it only bounds the map. Its predicate is asserted to be
  the same predicate `Remaining` uses.
  Tests: `TestCooldownsPrune`, `TestCooldownsPruneAgreesWithRemaining`.
- **Lapsed quarantines are swept inside `Check`.** `Quarantined` dropped an
  expired entry only when asked about that specific workload, and a controller
  only asks about workloads it still intends to touch — so the map grew without
  bound. `Check` now sweeps — and sweeps *only* the lapsed ones, since releasing
  a workload still serving out its quarantine would let Kilter touch it again
  (`TestRegressionCheckKeepsLiveQuarantines`).

## Invariants now documented in the code

`PDBGuard`'s ledger range `[0, collected]` and why `Release` is a rollback and
not a credit line; the shallow-copy contract of `NewPDBGuard`; why `Reserve`
validates before it spends; why `Remaining` must not be written as a plain
subtraction; why regression baselines must be per-pod; what a nil snapshot means
in each of `RecordChange` and `Check`; and `restartTolerance`, previously the
bare literal `2` inside a comparison.

## Deliberately left undone

- **An unset `Workload.Kind` is still evictable.** `CanEvict` tests for
  `KindBarePod`/`KindDaemonSet`, so a zero-value `Kind` falls through to
  "evictable". Treating unknown provenance as movable is a fail-open, and I
  would rather it refused. I did not change it: collectors always populate
  `Kind` (an unowned pod becomes `KindBarePod`), so it is unreachable in
  practice, and tightening it would silently change `pkg/plan`'s node-removal
  and spot-report behaviour — a package outside my scope and being edited
  concurrently. Pinned as a table row documenting current behaviour so the
  choice is visible rather than accidental.
- **A pod that OOMed both before and after a change reads as no new OOM.**
  `LastOOMKilled` is a boolean snapshot of the last termination reason, not a
  counter, so a repeat OOM on a *surviving* pod is genuinely indistinguishable
  from the original one. The restart delta catches it once it crosses
  `restartTolerance`. Fixing it properly needs a termination timestamp or an OOM
  count on `ContainerSpec`, which lives in `pkg/model` — outside this scope.
- **`Budget` and `Cooldowns` trust the caller's clock.** Both take `now` as a
  parameter, which is what makes them testable; neither can detect a caller
  passing a non-monotonic clock. They are hardened not to produce nonsense
  (no overflow, no negative durations, no contradiction between methods) but a
  wrong `now` still yields a wrong decision. Documented rather than defended
  against.

## Mutation coverage

Beyond the fixes, these load-bearing behaviours were each shifted in isolation
and confirmed to break a test: `Reserve`'s check-all-then-spend ordering; the
guard's defensive copy of the caller's slice; `Cooldowns.Allow`'s strict `<`;
`Prune`'s predicate; `Budget`'s half-open window edge; the OOM zero-tolerance
rule and the `restartTolerance` threshold; the regression-window and
quarantine-expiry boundaries; and `Check` consuming the watch when it delivers a
verdict (so a regression is reported exactly once).

## Verification

```
gofmt -l pkg/safety/                     # empty
go vet ./...                             # clean
go build ./...                           # clean
go test -race -count=1 ./pkg/safety/...  # PASS
go test -race -short ./...               # PASS (whole repo)
```

Each fuzz target was additionally run for 30s (~28M executions total across the
four) with no crashers and no corpus entries added to `testdata/`.
