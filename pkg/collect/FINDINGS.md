# pkg/collect — hardening session findings

Scope: `pkg/collect` only. All changes verified with
`gofmt && go vet && go build ./... && go test -race -count=1 ./pkg/collect/...`
and the full `go test -race -short ./...`. Every behavioral fix below was
verified to **fail against the pre-fix code** (stash + shim run) and pass after.

## Bugs found and fixed

1. **Hourly-cost annotation accepted `+Inf`** (`ConvertNode`).
   `strconv.ParseFloat("Inf", 64)` returns `+Inf` with a *nil* error, and
   `+Inf > 0` passed the guard — one annotated node poisoned every downstream
   cost aggregate (sums, deltas, savings math all become `Inf`/`NaN`). Now only
   finite positive values are honored. Locked by `TestHourlyCostAnnotation`
   (18 boundary cases) and `FuzzHourlyCostAnnotation` (invariant: finite,
   non-negative).

2. **Silent topology-list failures produced lying snapshots** (`Snapshot`).
   The doc contract said "topology errors abort", but ReplicaSet, Job,
   Deployment, StatefulSet, DaemonSet, HPA, PDB, and Namespace list errors
   were silently swallowed. Consequences by resource: RS/Job → every pod
   misattributed (Deployment pods become anonymous ReplicaSets); HPA → the
   brain resizes CPU under an autoscaler it can't see; PDB → evictions planned
   with no disruption budgets; **Namespaces → `Frozen=false` while the
   operator's `kilter.dev/freeze` kill switch is set** (the worst one: the
   controller keeps automating through a declared emergency). All topology
   lists now abort the snapshot; callers already log-and-skip the cycle, which
   is fail-safe. Shipped RBAC (chart + deploy manifest) grants all these list
   verbs, so no supported configuration regresses. Metrics remain
   degrade-gracefully (locked by `TestMetricsErrorsDegrade`); `ServerVersion`
   remains best-effort (cosmetic). Locked by
   `TestSnapshotAbortsOnTopologyListErrors` (10-resource table).

3. **`int32(pm.Window.Duration.Seconds())` was platform-dependent garbage for
   out-of-range windows** (`collectUsage`). Go float→int conversion of an
   out-of-range value is implementation-defined (MinInt32 on amd64, saturation
   on arm64). `recommend` weights samples by `WindowSeconds/60`, so a corrupt
   window became either a negative or a ~35-million-sample weight. Replaced
   with `clampWindowSeconds` (integer math, clamps to `[0, MaxInt32]`,
   negative → 0 = unknown). Locked by `TestClampWindowSeconds` and the
   `TestCollectUsageGarbage` end-to-end case.

4. **Negative usage readings were recorded verbatim** (`collectUsage`).
   Samples feed percentile histograms that size requests; a corrupt negative
   reading argues for shrinking a workload that never used less (recording it
   as zero would too). Negative CPU/memory samples are now dropped. Locked by
   `TestCollectUsageGarbage`.

5. **`HPATargetsCPU` missed `ContainerResource` CPU metrics**
   (`collectWorkloads`). Only `Resource`-type CPU metrics were detected, but a
   `ContainerResource` CPU HPA has the identical hazard: shrinking CPU
   requests raises utilization % and the HPA scales out to compensate. Both
   shapes are now detected (nil-guarded; typed constants instead of a string
   literal). Locked by `TestHPATargetsCPUShapes`, including malformed
   nil-source specs.

6. **Native sidecars (restartable init containers) were invisible**
   (`ConvertPod`). Init containers with `restartPolicy: Always` (K8s ≥1.28 —
   e.g. istio-proxy in sidecar mode) run for the pod's whole life and count
   toward the scheduler's request sum, but `ConvertPod` ignored
   `spec.initContainers` entirely — `PodSpec.Requests()` undercounted the
   pod's node footprint and binpacking overcommitted nodes. Sidecars are now
   converted like app containers (statuses resolved from
   `InitContainerStatuses`, so their OOM/restart history feeds recommend too).
   Locked by `TestSidecarContainers`.

7. **`hugepages-*` requests were dropped from the model**
   (`isExtendedResource`). Hugepages gate scheduling separately from memory,
   but the vendor-`/` heuristic excluded them, so a hugepages pod looked like
   it fits on any node. Now classified as extended resources symmetrically on
   nodes (allocatable pool) and pods (requests). Locked by
   `TestIsExtendedResource` and `TestHugepagesCollected`.

## Other hardening

- `DaemonSetTemplates(nil)` no longer panics (returns nil; documented).
- Godoc: per-constant docs for `AnnoHourlyCost`/`AnnoDoNotEvict`/
  `AnnoCASafeEvict` (units + accepted values); `Collector` documented as
  stateless/concurrency-safe; `Snapshot`'s abort-vs-degrade contract now
  explains *why* it is a safety property.
- New coverage for previously untested behavior: the freeze kill switch
  (`TestFreezeSwitch`, including freeze on a non-kube-system namespace being
  ignored), provider-ID boundary cases (`TestProviderFromIDBoundaries`,
  `FuzzProviderFromID`), and DaemonSet template dedup (`TestDaemonSetTemplates`).
- Both fuzz targets ran ~600k execs clean beyond their seed corpora.

## Deliberately left undone (with reasons)

- **RuntimeClass pod `Overhead` is not collected.** The scheduler adds
  `spec.overhead` to the pod's request sum (Kata/gVisor pods), so packing math
  underestimates such pods. Fixing it needs an `Overhead` field on
  `model.PodSpec` — outside this package's allowed scope. A synthetic
  pseudo-container would smuggle it in but would surface as a resizable
  container to the recommend engine, which is worse.
- **Plain (run-to-completion) init containers remain excluded** from
  `PodSpec.Containers`. Scheduler semantics are
  `max(max(init), sum(app + sidecars))`; the model is sum-based and cannot
  express the max without a model change. Excluding them undercounts only
  pods whose init burst exceeds steady state, and only during scheduling.
  Documented at the exclusion site.
- **No LIST pagination.** Full-cluster LISTs are the package's stated design
  (poll-based, self-consistent snapshots); chunking would be an availability
  optimization for very large clusters, not a correctness fix, and touches
  the package's core design assumption.
- **PDB `LabelSelectorAsSelector` errors are still skipped silently** inside
  `collectPDBs`. The apiserver validates selectors at admission, so the error
  path is unreachable with real API objects; aborting on it would add an
  untestable branch.
