# pkg/plan hardening findings

Scope: `pkg/plan` only (`plan.go`, `spot.go`). All changes verified with
`gofmt`, `go vet`, `go build ./...`, `go test -race -count=1 ./pkg/plan/...`,
plus ~470k fuzz executions of `FuzzBuildInvariants` with zero failures.

## Bugs found and fixed

1. **PDB reservation leak on aborted removals** (`tryRemove`). The old flow
   dry-checked pods individually (`CanEvict`), then reserved at commit time.
   Two pods on one node sharing a single-disruption budget pass the dry check
   individually, then the second `Reserve` fails — aborting the removal but
   leaving the first reservation consumed in the plan-scoped guard. Later,
   legitimate removals in the same plan were then silently blocked.
   Fix: reserve the whole eviction set up front and roll back on *every*
   failure path (reserve/schedule/headroom). Test: `TestPDBReservationRollback`
   (fails before, passes after), `TestPDBGroupBudgetRespected`.

2. **Terminal pods distorted planning** (`Build`, `EmergencyDrainPlan`). The
   collector ships all pods; Succeeded/Failed pods (completed Jobs, failed
   pods pending GC) were counted toward node utilization, force-placed in the
   binpack sim (consuming capacity), given evict steps, and — worst — a
   completed bare pod or local-storage pod permanently pinned its node via
   `BlocksDrain`, hiding real savings. The scheduler and kubectl drain both
   ignore terminal pods. Fix: filter them from `Build`'s working set and from
   emergency-drain evictions (`isTerminal`). Tests:
   `TestTerminalPodsDoNotDistortPlan`, `TestTerminalBarePodDoesNotPinNode`,
   `TestEmergencyDrainSkipsTerminalPods` (all fail before, pass after).

3. **NaN config disabled the headroom guard** (`Build`). Every float
   comparison with NaN is false, so `MinClusterHeadroom = NaN` made
   `free/alloc < cfg` never trigger — waving arbitrarily aggressive packing
   through; NaN `MinNodeUtilization` made *every* node a candidate. Fix:
   positive-range defaulting (`!(x > 0)`), same idiom as
   `binpack.PlanOptions.withDefaults`. Test: `TestNaNConfigFallsBackToDefaults`
   (fails before, passes after).

4. **Negative spot "savings" from garbage catalogs** (`typicalSpotDiscount`).
   A catalog row with `spotHourlyUSD >= hourlyUSD` contributed a negative
   discount to the average (e.g. spot 0.5 vs on-demand 0.1 → −400%), which
   could turn the whole `EstMonthlySavingsUSD` negative. Fix: only rows with
   a real discount (0 < spot < on-demand) count; otherwise fall back to 0.65.
   Test: `TestSpotDiscountIgnoresGarbageCatalog` (fails before, passes after).

5. **Nil-pointer panics in report paths** (`BuildSpotReport`,
   `EmergencyDrainPlan`, `InterruptedSpotNodes`). `Build` guards nil inputs;
   these didn't (confirmed panic under the old code). Their signatures return
   no error (callers in `pkg/api` and `cmd/` — out of scope to change), so
   they now degrade gracefully: empty report / empty plan / nil. Test:
   `TestSpotReportNilTolerant` (panics before, passes after).

6. **Stale recommendations emitted dead resize steps** (`Build`). A rec whose
   workload matched no pod container *and* is absent from `snap.Workloads`
   (workload deleted or container renamed between learning and planning)
   still produced a resize step — a guaranteed actuation failure — and
   entered the fingerprint, so approval could bind to an unexecutable plan.
   Scaled-to-zero workloads keep their steps via the `snap.Workloads` check.
   Dropped recs are surfaced in `Notes`. Test: `TestStaleRecommendationDropped`
   (fails before, passes after).

## Hardening (defense in depth; prior behavior was saved by later layers)

- **Negative requests clamped** in `utilizationMap` and the spot report
  (`clampedRequests`, mirroring `binpack.requestsOf`): a buggy collector
  can no longer deflate utilization to fake consolidation candidates
  (previously caught downstream by the schedule step) or push spot savings
  negative. Tests: `TestNegativeRequestsDoNotFakeCandidates`,
  `TestSpotReportNegativeRequestsClamped`.
- **Non-positive allocatable reads as fully utilized** (was `== 0`): a node
  reporting negative allocatable is broken, not empty.
  Test: `TestZeroAllocatableNodeNeverCandidate` (failed before).
- **Eviction-target nodes are pinned** for the rest of the plan: a later
  round can no longer delete a node an earlier step moved pods onto, which
  would make the plan self-contradictory (its delete step could only fail at
  actuation — `WaitNodeEmpty` fails closed — and the pod would be double-
  evicted, double-spending PDB budget). I could not construct a natural repro
  (best-fit sends evictees to the fullest fitting node, which essentially
  never remains a candidate), so this is enforced as a structural invariant
  rather than a demonstrated bug. Tests: `TestEvictionTargetsSurvive` plus
  fuzz invariants (no removed target, no double eviction).
- **Emergency drain plans are now fingerprinted** for the audit ledger
  (they bypass approval, not audit), and their step sequence stays contiguous.

## Tests added

- `hardening_test.go`: 14 tests covering the above plus contract pins:
  `TestFingerprintContract` (fingerprint ignores from-values, is
  order-sensitive, changes with targets) and `TestMaxKeyDeterministic`
  (lexicographic tie-break so `dominantProviderArch` can't flap).
- `fuzz_test.go`: `FuzzBuildInvariants` derives adversarial clusters
  (unready/cordoned/control-plane/karpenter/spot/shrunk nodes; DS, bare,
  terminal, pinned, pending, local-storage pods; a PDB with 0–2 allowed
  disruptions) and asserts: no error, snapshot unmutated, deterministic
  fingerprint, removal bound respected, `0 ≤ projected ≤ current`, savings
  arithmetic consistent, seq contiguous, untouchable nodes untouched, evict
  steps only for live movable pods, targets survive the plan, no double
  eviction, PDB never overspent. ~470k execs clean.

## Invariants documented in code

- `tryRemove`: every failure path must release reserved PDB disruptions
  (the guard is plan-scoped and accumulates across removals).
- `Build`: terminal pods are treated as absent, matching scheduler/drain
  semantics; targeted nodes are pinned; NaN-tolerant config defaulting.
- `utilizationMap`: non-positive allocatable ⇒ utilization 1.

## Deliberately left undone

- **`BuildSpotReport` PDB check samples only `pods[0]`** per workload. With
  `CoveredPodUIDs`-based PDBs, other replicas may be covered while pod 0 is
  not, letting a workload score spot-safe despite an exhausted budget. It is
  an advisory report (no actuation), and the honest fix (group-aware
  coverage) duplicates PDBGuard internals; noted instead of half-fixed.
- **`SpotSafety.Replicas` counts observed pods**, not spec replicas; a
  deployment scaled to 3 with 1 running pod reads as 1 replica (conservative
  direction: fewer workloads marked safe).
- **Greenfield floor collects DaemonSet templates from the evolved pod set**;
  a DaemonSet whose only pod sat on a consolidated node drops out of the
  floor's per-node overhead. Informational metric, error is small and
  optimistic-only; a fix needs the pre-consolidation DS set threaded through.
- **`Build` still trusts `snap.Timestamp`** as `CreatedAt` (zero time if the
  collector omits it) — plausibly intentional (plans replay deterministically
  from snapshots), so left alone.
- `go.sum` picked up incidental `/go.mod` graph-pruning lines from running
  `go build ./...`; reverted — out of scope.
