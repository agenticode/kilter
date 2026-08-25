# pkg/model hardening findings

## Bugs found and fixed

1. **int64 wraparound in `Resources.Add`/`Sub`, `PodSpec.Requests`, and
   `PodSpec.ExtendedRequests`** — summing container requests past `MaxInt64`
   wrapped negative. This composes into a real capacity-minting hole: binpack's
   `requestsOf` clamps negative requests to zero (deliberately, to stop negative
   requests inflating `Free`), so a garbage/adversarial snapshot whose containers
   sum past `MaxInt64` turned an impossibly large pod into one that "fits
   anywhere". Fixed with saturating arithmetic (`satAdd64`/`satSub64`): sums
   clamp at the int64 bounds, so an absurd pod stays absurd and fails `Fits`
   everywhere. Verified: `TestResourcesSaturation`, `TestPodRequestsOverflowSafe`,
   and `TestExtendedRequests/saturates_instead_of_wrapping` all fail against the
   pre-fix code (wrapped-negative results confirmed by running them against
   `HEAD`'s model.go) and pass now.

2. **`PodsOnNode("")` returned all unscheduled pods** — pending pods carry an
   empty `NodeName`, so querying a node whose name is accidentally empty
   silently returned every pending pod, inflating per-node utilization math.
   No real node is named `""`; the method now returns nil for it.
   (`TestPodsOnNodeEmptyName` fails before / passes after.)

3. **nil-deref panic in `PDB.Covers`** — a nil receiver or nil pod crashed the
   eviction-safety path (`pkg/safety` calls `Covers` per pod per PDB). Both now
   return false, which is semantically correct (no PDB / no pod ⇒ nothing to
   cover), not just fail-safe. (`TestPDBCovers` panicked before the fix.)

## Tests added

- `hardening_test.go` — table-driven adversarial tests: saturation contract for
  Add/Sub, overflow-safe `Requests`/`ExtendedRequests`, `PDB.Covers` precedence
  (UID list beats labels both directions, namespace gate, empty-list fallback,
  nil safety), `PodsOnNode` empty-name guard, `NodesByName` duplicate/aliasing
  semantics, invalid-toleration-operator behavior, `String` format pinning, and
  a full `ClusterSnapshot` JSON round-trip test guarding the agent→brain wire
  contract against future tag collisions or dropped fields. Also a compile-time
  pin that `WorkloadRef`/`ContainerKey` stay comparable (they key maps across
  recommend/plan).
- `fuzz_test.go` — two differential fuzz targets, each run ~6–7M execs clean:
  - `FuzzResourcesArithmetic`: Add/Sub vs an arbitrary-precision `math/big`
    reference clamped to int64, plus Max commutativity/idempotence/upper-bound
    and Fits properties.
  - `FuzzTolerates`: `Toleration.Tolerates` vs an independently structured
    reference (operator-first vs key-first), plus "unknown operator never
    tolerates".

## Invariants documented

- Why `Add`/`Sub` saturate and why wraparound (not overflow per se) is the
  dangerous failure mode.
- `Limits()` understates the pod when any container has a zero (= unlimited)
  limit; callers enforcing ceilings must check for zero-limit containers.
  Behavior unchanged (existing test pins the sum) — only unused by consumers
  today, so documentation is the honest fix.
- `PDB.Matches` empty-selector-matches-nothing (deliberate inversion of the
  Kubernetes empty-selects-all rule, so a zero-value PDB can't freeze a
  namespace) and how genuine select-all PDBs still work via `CoveredPodUIDs`.
- `NodesByName` values alias `s.Nodes` (mutation visible); duplicates keep the
  last entry.
- `WorkloadRef`/`ContainerKey` must remain comparable (map keys engine-wide).
- `Resources.String` truncates memory toward zero (sub-MiB prints `0Mi`).

## Deliberately left undone

- **`Limits()` zero-means-unlimited semantics unchanged**: making the sum
  return "unbounded" when any container is unlimited would be more correct for
  pod-level capping, but the existing test pins the current sum, no consumer
  calls `Limits()` yet, and changing a wire-adjacent helper under other agents'
  feet is riskier than documenting the caveat sharply.
- **`Toleration.Tolerates` empty-key + Equal returns false** (raw Kubernetes
  `ToleratesTaint` would compare values): kept as-is — K8s API validation makes
  such tolerations unrepresentable, the existing test pins it, and "does not
  tolerate" is the conservative direction for placement simulation.
- **Init containers are not modeled**: `pkg/collect` folds them (incl. sidecar
  statuses) before building `PodSpec`, so the model-level sum-of-containers is
  correct by construction; noted here rather than adding unused fields.
