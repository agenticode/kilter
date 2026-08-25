# pkg/evidence — Evidence substrate v1 (design §3, unit 1)

L0 substrate for Kilter's reasoning engine: a bounded, deterministic,
queryable record of *what happened to a subject and when*, plus tiered usage
digests compact enough for 50k workloads, a decision journal for
backtesting, an exact-round-trip codec, and the dossier builder.

Stdlib only. The package imports `pkg/model` and nothing else from the repo,
per the §8 dependency direction (evidence sits below everything).

```
gofmt -l pkg/evidence/                    → empty
go vet ./...                              → clean
go build ./...                            → clean
go test -race -count=1 ./pkg/evidence/... → ok, 96.0% statement coverage
go test -race -short ./...                → ok (whole repo, 19 packages)
```

80 test functions / 291 cases including subtests, 6 fuzz targets
(~12M executions run locally, no failures beyond the one recorded below).

---

## Bugs found by tests

All three of the first group are in code that existed before this run. Each
was found by an invariant assertion, not by reading — which is the point:
`subjLog.bytes` and `series.bytes` were **write-only fields**, so nothing in
the package could have noticed them drifting.

### 1. `budgetedLog.append` under-counted multi-entry evictions

`ring.push` evicts *down to* the cap and returns only its **last** eviction.
`append` treated that single return as "one entry left", so whenever the
per-subject cap shrank between appends — a checkpoint restored under a
tightened `MaxEventsPerSubject`, exactly what `FromCheckpoint` now does —
every entry beyond the first vanished from the ring while still being
counted in `l.bytes` and `l.count`.

Measured repro (cap 8 → 2 on the 9th append): ring held 2 entries,
`l.count` still said 8 and `l.bytes` leaked 600 bytes. The leak is
permanent and cumulative: `enforceBudget` then evicts live data to satisfy
a budget that phantom entries are consuming.

**Fix** (`log.go`): `append` drains the front itself, accounting each
eviction, and returns the eviction *count*. `Memory.Append` /
`RecordDecision` add that count to `Stats` instead of incrementing by one.
**Tests:** `TestLogPerSubjectCap` (rows `cap shrinks`,
`cap collapses to one`), `checkLog` invariant on every log test.

### 2. `series.ingest` double-counted every hour roll

`rollHour` applied its byte delta to `sr.bytes` *and* returned it to
`ingest`, which applied it again. Every hour boundary corrupted the
per-series byte accounting by the roll's delta (−16 bytes in the common
case, unbounded over a long-lived series). Nothing read `sr.bytes` yet, so
this was a live trap for the next reader — including the checkpoint codec
written in this same unit, which needs it to be right.

**Fix** (`series.go`): `rollHour` returns the delta and no longer applies
it; the doc comment now states that the caller applies it exactly once.
**Tests:** `TestIngestRollsHours` re-derives `sr.bytes` from ring contents
after *every* sample; `TestEvictOneOrder` does the same after every
eviction; `checkMemory` enforces it store-wide.

### 3. Dedup severity upgrades desynced the global byte budget

`Append` folds a repeat event into the stored one and upgrades its severity.
Severity strings differ in length (`info` 4 → `critical` 8) and
`eventBytes` counts them, so the stored event grew by up to 4 bytes with no
corresponding change to `subjLog.bytes` / `budgetedLog.bytes`. On a
warning-heavy cluster (probe failures, evictions — precisely the
dedup-heavy kinds) the accounted total drifts permanently below reality, so
`MaxEventBytes` stops bounding actual memory.

**Fix** (`store.go`): the upgrade re-charges the delta on the entry, the
subject and the log, then re-runs `enforceBudget`.
**Tests:** `TestAppendDedupSeverityUpgradeIsAccounted`, `TestAppendDedup`.

### 4. Dossier size cap could be exceeded by its own truncation notice

Found by `FuzzDossierSizeCap` in code written during this run.
`shrinkTo` measured the dossier *before* `Truncated` was attached, then the
builder attached it — so a dossier that shrank to exactly the cap
overflowed it by ~20 bytes. A 4096-byte request returned 4116 bytes. For a
structure whose entire purpose is being a safe fixed-size retrieval unit,
that is the one bug that matters.

**Fix** (`dossier.go`): `shrinkTo` attaches the current notice before each
measurement. **Test:** `TestDossierSizeCap`, `FuzzDossierSizeCap`.

### 5. Negative dossier count caps panicked

`FuzzDossierSizeCap` seed `MaxEvents=-16` → `slice bounds out of range`.
Validation checked only the upper bound. **Fix:** negative caps are
clamped to zero and documented as "omit this section". The crasher is kept
as a regression seed in `testdata/fuzz/FuzzDossierSizeCap/`.

---

## What is here

| File | Contents |
|---|---|
| `evidence.go` | Types (`SubjectRef`, `EvidenceEvent`, `DecisionRecord`, `Sample`, `TimelinePoint`), the event/severity vocabulary, sanitize caps and `Config` + validation |
| `ring.go` | Lazily-growing bounded FIFO. Sparse subjects pay only for what they hold |
| `heap.go` | Min-heap with index write-back — O(log n) re-key and remove, keyed on unique sequence numbers so eviction order is total |
| `log.go` | `budgetedLog`: per-subject ring + global byte budget, globally-oldest-first eviction |
| `series.go` | Tiered digests: raw ring → hourly → daily, run-length coalescing, per-series shedding |
| `store.go` | `Memory`: `Store` + `Sink` implementations, `Prune`, `Stats` |
| `codec.go` | `Checkpoint` / `FromCheckpoint` / `Save` / `Load` + the `CheckpointStore` seam |
| `dossier.go` | `BuildDossier` — the §3.4 composition function, size-capped and deterministic |

## Invariants and where they are tested

| Invariant | Enforced in | Tested by |
|---|---|---|
| Every store is ring-capped per subject | `budgetedLog.append`, `series` rings | `TestLogPerSubjectCap`, `TestAppendPerSubjectCapAndBudget`, `checkSeriesShape` |
| Every store is byte-budgeted globally, never exceeded mid-ingest | `enforceBudget`, `enforceSeriesBudget` | `TestEventBudgetBoundsUnderLoad`, `TestSeriesBudgetBoundsUnderLoad`, `TestTimelineAndDecisionBounds` |
| Accounted bytes equal the bytes actually held | all mutators | `checkLog` / `checkMemory`, called by every mutating test |
| Budget eviction is globally-oldest-first; series shedding is coldest-first | seq-keyed heaps | `TestLogBudgetEvictsGloballyOldest`, `TestSeriesBudgetBoundsUnderLoad` |
| Empty subjects release their overhead | `dropSubject` | `TestLogBudgetDropsEmptySubjects`, `TestPruneEmptiesEverything` |
| Live heap stays within a small multiple of the configured budget at 50k subjects | — | `TestScaleSoakHeapBounded` (real `runtime.MemStats`; skipped under `-short`) |
| No map iteration order reaches any output | sorted queries, `sortedRefs` | `TestLogSortedRefsDeterministic`, `TestSubjectsSorted`, `TestEventsTieBreakIsArrivalOrder`, `TestTimelineOverlay`, `TestCheckpointDeterministic`, `TestDossierDeterministic` |
| No `time.Now()` anywhere; `Prune` takes the caller's clock | — | every test builds from the fixed `t0`; `Prune(zero)` errors |
| Digest stats stay ordered, finite, non-negative through any merge | `DigestStats.merge`, `foldInto` | `TestDigestStatsMergePreservesOrder`, `TestFoldInto`, **`FuzzDigestFold`** |
| Coalescing never erases restarts or OOMs, never bridges a gap | `canCoalesce` | `TestCanCoalesce`, `TestIngestInterestingHoursDoNotCoalesce` |
| Garbage is rejected or normalized by documented rules, never stored | `sanitize*` | `TestSanitizeSample`, `TestSanitizeEvent`, `TestObserveSampleRejectsGarbage`, `TestRecordDecisionRejects`, **`FuzzSanitizeEvent`** |
| Sanitize is a fixed point (what makes exact restore possible) | `cleanString` idempotence | **`FuzzCleanString`**, **`FuzzSanitizeEvent`** |
| Stored state never aliases caller memory, in either direction | `copyEvent`, `copyDecision`, `sanitizeAttrs` | `TestEventsReturnsDefensiveCopies`, `TestRecordDecisionPayload`, `TestDossierAttachments`, `TestCheckpointDoesNotAliasStore` |
| Checkpoints round-trip byte-exactly; restore is a fixed point | `Checkpoint` / `FromCheckpoint` | `TestCheckpointRoundTripExact`, **`FuzzCheckpointDecode`** |
| Restore validates, never transforms; corrupt input is refused whole | `validate*` | `TestFromCheckpointRejects` (33 corruption cases) |
| Restoring under a tightened budget sheds rather than exceeds | `FromCheckpoint` | `TestFromCheckpointTightenedBudget` |
| Dossiers always fit their byte cap, or the request is refused | `shrinkTo` | `TestDossierSizeCap`, `TestDossierImpossibleCapFails`, **`FuzzDossierSizeCap`** |
| Nothing is dropped silently — every eviction/drop is in `Stats` or `Truncated` | throughout | `TestDossierTruncationIsReported`, `TestStatsGauges` |
| Safe for concurrent use | `sync.RWMutex` | `TestConcurrentUse` under `-race` |
| Any interleaving of operations preserves all of the above | — | **`FuzzMemoryIngest`** (arbitrary opcode streams against tiny budgets) |

## Design notes worth knowing

- **`Sample.ThrottleRatio` is clamped, not rejected**, at the `[0,1]`
  boundaries — kubelet occasionally reports `throttled > total` across a
  counter reset. Out-of-range *magnitudes* elsewhere are rejected outright,
  matching `pkg/patterns`.
- **Coalesced percentiles are element-wise maxima**, i.e. a conservative
  upper bound, never an underestimate. Over-sizing from a coalesced digest
  is safe; under-sizing is an outage. `Max` is always exact.
- **Decision payloads are compacted at ingest.** `encoding/json` compacts
  `json.RawMessage` on the way out, so storing a producer's whitespace would
  break the byte-exact checkpoint round-trip. Restore *rejects* a
  non-compact payload rather than silently rewriting it.
- **`Digests(…, TierRaw)` returns point-in-time digests** (`Start == End`,
  `Samples == 1`). They are query projections and deliberately do not
  satisfy `validateDigest`, which governs *stored* digests only.
- **Series eviction under budget pressure can produce two digests for one
  hour** if the pending accumulator is shed mid-hour and the subject keeps
  writing. Both are valid, non-overlapping-by-construction consumers of the
  same window; the alternative (evicting finalized history first) loses
  strictly more information. Called out here rather than hidden.

## Deliberately deferred, with reasons

- **Collectors (`pkg/collect`).** Out of scope by instruction — that package
  belongs to another agent. The seam is `Sink` (below); nothing here needs
  to change when it lands.
- **bbolt persistence (`pkg/store`).** Same reason. `CheckpointStore` is the
  seam; the codec is complete and fuzz-tested against it.
- **`Dossier.Class` / `Features` / `Sizing` / `Refusal` / `Guards` /
  `CostMonthly`** from the §3.4 sketch. Those types live in `pkg/patterns`,
  `pkg/recommend` and the not-yet-existing `pkg/decision`; importing them
  would invert the §8 dependency direction. They are carried instead as
  `[]Attachment` — opaque, compact JSON contributed by the layer that owns
  the type. That layer fills them in unit 3/4 with no change here.
- **A binary codec.** JSON is ~2× the size of a packed encoding. The
  checkpoint is written on a timer, not per-event, and JSON buys diffable,
  debuggable, version-tolerant checkpoints. Revisit only if a measurement
  says so.
- **`EventRegimeChange` emission.** The type exists in the vocabulary; the
  CUSUM detector that fires it is unit 3.
- **Prometheus metrics.** `Stats()` exposes every gauge and counter a
  collector needs; wiring it to a registry means touching `pkg/api`, which
  is out of scope.

## Exact wiring a later unit must do

### `pkg/collect` — implement the `Sink` seam

```go
type Sink interface {
    Append(ev EvidenceEvent) error
    ObserveSample(s SubjectRef, smp Sample) error
    ObservePoint(cluster string, p TimelinePoint) error
    RecordDecision(d DecisionRecord) error
}
```

`*evidence.Memory` already satisfies it. The collector work is:

1. **Workload informer** — on spec-hash change emit `EventDeploy` on
   `WorkloadSubject(cluster, ref)` with attrs from a **fixed allowlist**
   (`image`, `replicas`, `generation`, `cpuRequest`, `memRequest`). Per
   **INV-3** attrs must never carry env vars, args or log text; the
   allowlist test belongs on the collector side. The substrate caps count
   (16) and value length (128B) but cannot know provenance.
2. **HPA informer** — `EventHPAScale` on the workload subject; set
   `Dedup` to `hpa/<name>` so replays fold instead of flooding the ring.
3. **Event informer (Warning only)** — `EventOOMKill`,
   `EventEvicted`, `EventFailedScheduling`, `EventProbeFailure`,
   `EventImagePullBackOff` on `ContainerSubject`/`WorkloadSubject`. Set
   `Dedup` to `<kind>/<involvedObject.uid>`; `DedupWindow` (1h) then
   collapses informer replays into a single event with a `Count`.
4. **Node informer** — `EventNodePressure` on `NodeSubject`, `Dedup` set to
   `<condition>`.
5. **Usage sampling** — add `throttledPeriods/totalPeriods` to the existing
   cAdvisor scrape and call `ObserveSample` per container per interval.
   Samples must be **non-decreasing in time per subject**; an older sample
   returns `ErrOutOfOrder` and is counted, not stored. `Restarts`/`OOMs` are
   **deltas since the previous sample**, not cumulative counters.
6. **Ingest tail** — after the existing recommender observe, call
   `ObservePoint(cluster, TimelinePoint{At, CostUSDPerHour, Nodes})`.
   `Events` must be nil; the overlay is computed by `Timeline` queries.
7. **Retention** — call `Prune(now)` on the existing housekeeping ticker.
   It is the only clock-dependent path and the clock is the caller's.
   Ring caps and byte budgets are enforced on write and need no ticker.

### `pkg/store` — implement `CheckpointStore`

```go
type CheckpointStore interface {
    SaveCheckpoint(ctx context.Context, data []byte) error
    LoadCheckpoint(ctx context.Context) ([]byte, error)
}
```

One bbolt bucket, one key, one opaque blob:

- `SaveCheckpoint` → `bucketEvidence.Put([]byte("checkpoint"), data)`.
- `LoadCheckpoint` → `Get`; return `(nil, nil)` when absent. `Load` maps
  that to `ErrNoCheckpoint`, which the brain treats as a cold start.
- Brain startup: `evidence.Load(ctx, store)`; on `ErrNoCheckpoint` call
  `evidence.NewMemory(cfg.Evidence)`.
- Brain shutdown and a periodic ticker: `evidence.Save(ctx, store, mem)`.
- Do **not** split the checkpoint across keys. Restore validates the
  document as a whole and refuses a partial one; a half-applied checkpoint
  is the failure mode this design exists to prevent.

`BrainConfig` gains `Evidence evidence.Config`; every zero field takes its
production default from `DefaultConfig()`.
