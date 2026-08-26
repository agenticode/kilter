# Q2 — the evidence substrate and snapshot history

Two shipped features were unreachable and both were blocked on the same
missing thing, so this was one job rather than two:

- `kilter backtest --cluster` refused because `pkg/store` kept only the
  **latest** snapshot per cluster (cmd/WIRING-FINDINGS.md §6.2).
- The `why-cost` and `explain` API routes could not be served because
  `api.Brain` held **no evidence substrate at all** — `grep 'evidence\.'
  pkg/api/brain.go` returned nothing — and `Verify` needs *the same store that
  produced the answer* to re-serve every citation (§6.3).

All four steps landed.

```
pkg/store/history.go          time-keyed snapshot history + the SnapshotSource adapter
pkg/store/evidence.go         the evidence checkpoint blob + its framing contract
pkg/api/substrate.go          api.Brain's *evidence.Memory, populated from Ingest
pkg/api/backtest.go           Brain.Backtest — replay, and the refusal that must survive it
pkg/api/explainroutes.go      GET …/why-cost and GET …/explain, both gated on Verify
```

| | |
|---|---|
| New production code | 1,714 lines across `pkg/store` and `pkg/api` |
| New tests | 44 test functions |
| Coverage | `pkg/store` **88.9 %**, `pkg/api` **85.2 %** |

`gofmt -l ./pkg/store ./pkg/api` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/store/... ./pkg/api/...` and
`go test -race -short ./...` all green. **`go.mod` and `go.sum` are
unchanged** — every import added is stdlib or intra-repo.

---

## 1. Retention, and the arithmetic behind it

### 1.1 The policy

Three bounds, all enforced inside the same write transaction, so the bound is
never observable as violated:

| Bound | Default | What it stops |
|---|---|---|
| **Cadence thinning** | 1 hour | one row per ingest tick |
| **Count cap** | 768 rows/cluster (≈32 days hourly) | unbounded rows |
| **Byte budget** | 32 MiB/cluster | unbounded *disk* — the bound that actually binds |

Plus two record-level guards: a framed record over **8 MiB** is refused at
write, and decompression is bounded at **256 MiB** (a stored record is
otherwise a zip bomb aimed at a process meant to run unattended for months).

Records are `"KSN1"` + gzip(JSON). A magic prefix rather than a JSON envelope
because base64-ing the compressed bytes would give back a third of what the
compression won.

**Why thinning and not "store every tick".** At the 5-minute ingest cadence a
30-day window is 8,640 snapshots per cluster. Density above hourly buys a
replay nothing — `backtest`'s default `DecisionInterval` is 24h — while
**span** is exactly what a replay is short of. The cheapest byte is the one
not written.

**Why a byte budget and not just a count.** Snapshot size varies by three
orders of magnitude between a ten-node cluster and a thousand-node one. A
count cap bounds rows; only a byte budget bounds disk.

### 1.2 Worst-case bytes per cluster

**40 MiB.** The 32 MiB budget, plus at most one record that exceeds it alone
(the newest is always retained — an empty history reads as "nothing happened",
which is worse than one over-budget row), which the 8 MiB record cap bounds.
Nothing else in the history grows.

`TestFramedRecordSizeIsWellUnderTheRecordCap` measures the table rather than
asserting a guessed number, because a quoted figure nobody recomputes goes
stale the first time `model.ClusterSnapshot` grows a field:

| Cluster | raw JSON | framed | ratio | 768 rows | **actually retained** |
|---|---|---|---|---|---|
| 10 nodes / 60 pods | 53 KiB | 2.4 KiB | 22× | 1.8 MiB | 1.8 MiB — **768 rows, 32 days** |
| 50 nodes / 500 pods | 431 KiB | 14 KiB | 30× | 10.8 MiB | 10.8 MiB — **768 rows, 32 days** |
| 200 nodes / 3 000 pods | 2.5 MiB | 83 KiB | 30× | 62.5 MiB | 32 MiB — **392 rows, 16 days** |
| 1 000 nodes / 15 000 pods | 12.4 MiB | 408 KiB | 31× | 306 MiB | 32 MiB — **80 rows, 3.3 days** |

The trade-off is stated rather than hidden: **a big cluster gets less history
than a small one.** That is what a byte budget means. The alternative — a
uniform 30-day span — costs the 200-node cluster 8,640 × 2.5 MiB ≈ **22 GB**,
which is the disk bomb §6.2 warned about.

The compression ratios above are measured on structured synthetic fixtures and
are optimistic; real snapshots with high-entropy pod names and UIDs compress
nearer 10×, which moves the two large rows in the table down proportionally
and leaves the two small ones unchanged (they are count-capped, not
byte-capped).

**Both bounds are tested by writing past them and asserting eviction** —
`TestSnapshotHistoryEvictsPastTheCountCap`,
`TestSnapshotHistoryEvictsPastTheByteBudget`,
`TestSingleOversizeRecordIsRetainedAlone`,
`TestDefaultRetentionThinsToItsCadence`.

### 1.3 The query bound

`Snapshots(cluster, from, to)` **refuses** a window wider than
`MaxWindow` (400 days, matching `pkg/evidence`'s longest retention) rather
than clamping. Retention already bounds the answer, so this is not an OOM
guard: it is a guard against a caller who typed the wrong unit getting a
plausible-looking short answer instead of being told the query was nonsense.
The explanation routes apply the same 400-day cap for the same reason —
the substrate is bounded, and a wider window would return a bounded answer
that reads as a complete one. Tested by
`TestSnapshotWindowWiderThanTheCapIsRefused` and
`TestExplainRoutesRefuseAnUnboundedWindow`.

### 1.4 Behaviours callers depend on

- **Cadence thinning is silent and deliberate.** A dropped snapshot is the
  policy working, not an error: the ingest path calls `SaveSnapshotAt` every
  five minutes and must not log a failure eleven times an hour.
- **Re-saving the exact stored timestamp replaces it.** Timestamps are unique
  per cluster by construction, which is `backtest.SnapshotSource`'s
  requirement ("a time series with two values at one instant has no defined
  replay order"). Re-ingesting a recorded history is idempotent.
- **Saving a snapshot older than every retained one, with the history full,
  prunes the row just written.** The same rule `SavePlan` already follows: the
  bound is over the history, not over the arrival order.
- **Cluster ids containing `/` are isolated.** EKS ARNs do. A key's suffix
  must parse as exactly one instant, so scanning cluster `a` never reads or
  prunes cluster `a/b`'s rows (`TestSnapshotHistoryIsolatesClusterIDsContainingSlashes`).

---

## 2. The checkpoint compatibility contract

A stored evidence checkpoint is a **JSON envelope with its own version**,
independent of `evidence.CheckpointVersion`:

```json
{"version":1,"codec":"gzip+json","blob":"<base64 gzip of checkpoint JSON>"}
{"version":1,"codec":"json","payload":{ …checkpoint JSON… }}
```

The contract, in four clauses:

1. **Both codecs are read; only `gzip+json` is written.** Checkpoint JSON is
   highly repetitive and this value is rewritten on every checkpoint tick.
2. **An unknown envelope version is rejected by name**, naming the version
   found and the version this build speaks. Never guessed at.
3. **An unknown codec is rejected by name.**
4. **The decoded checkpoint is bounded at 64 MiB.** `pkg/evidence`'s own
   default budgets allow well over a gigabyte in memory, and a bbolt value
   must be materialized whole on both sides of a write, so persistence applies
   a much smaller bound and says so rather than truncating.

Below the envelope, `evidence.FromCheckpoint` applies its own version check,
so a *substrate* written by a future build is refused there.

**Both directions are tested.** `TestOldPlainJSONEnvelopeStillLoads`
hand-builds the uncompressed form — deliberately not produced by the current
writer, because a round-trip through the current writer tests compatibility
with nothing — and asserts it loads. `TestFutureEnvelopeVersionIsRejectedByName`,
`TestUnknownCodecIsRejected` and `TestGarbageEvidenceRecordIsAnErrorNotEmptyState`
cover the refusals.

**A checkpoint that cannot be understood fails `NewBrain`.** It does not start
empty. Starting empty after a failed restore is indistinguishable from a cold
boot, and the brain would then serve explanations over a substrate missing
exactly the history the operator restarted to look at
(`TestUnreadableCheckpointFailsTheBrainRatherThanStartingEmpty`).

---

## 3. What `cmd/` must now do

Both refusals are removable. Neither should be removed by deleting the
refusal — in both cases the refusal moved into `pkg/api` and must stay called.

### 3.1 Removing `backtest --cluster`'s refusal

Delete `backtestLiveRefusal` in `cmd/kilter/backtest.go` and route
`--cluster` to **`Brain.Backtest`**:

```go
// cmd/kilter/backtest.go, replacing the `case bf.cluster != "":` arm
st, err := store.Open(dbPath)                       // the brain's --db path
brain, err := api.NewBrain(api.BrainConfig{}, catalog, st)
sc, err := brain.Backtest(bf.cluster, from, to, horizon, scoring)
if err != nil {
    return err          // ← already the refusal, already non-zero exit
}
writeScorecard(w, sc, …)  // unchanged; --json and Gate paths unchanged
```

`--from`/`--to` become required for `--cluster` for the same reason
`why-cost` requires them: a replay window that drifts with wall-clock time
makes two runs over the same configuration disagree. `backtestEpoch` stays as
it is for `--demo`.

**The refusal is still there and must stay reachable.** `Brain.Backtest`
refuses in two cases, both typed so the caller can render them without string
matching:

- `ErrNoHistory` — the brain has no `--db`, so no history is kept at all.
- `ErrHistoryTooShort` — fewer than two retained snapshots in the window, **or**
  `Scorecard.Instants == 0`.

The second condition is the one a count check alone would miss, and it is why
this lives in `pkg/api` rather than in `cmd/`. `backtest.Run` over a history
with no scoreable instant does **not** fail: it returns a `Scorecard` with the
same shape, the same field names and the same confident tone as a real one —
`snapshots 2`, `regret $0.00` — and an operator reading that cannot tell it
means "nothing was replayed" rather than "the policy is perfect". The check
uses backtest's **own** coverage report (`sc.Instants`), not a re-derived
predicate that could drift from it.
`TestBacktestRefusesWhenNothingWouldBeScored` and
`TestBacktestRefusesAHistoryTooShortToReplay` assert no scorecard escapes.

`cmd/kilter/backtest_test.go`'s
`TestBacktestLiveHistoryRefusesRatherThanScoringOneSnapshot` still passes
unchanged today (nothing has been ingested into a fresh store), and after the
wiring above it should be re-pointed at the `ErrHistoryTooShort` message
rather than deleted.

### 3.2 Removing the "routes not built" gap

Nothing to do in `cmd/` — the routes are registered by `Brain.Handler()` and
served by `kilter brain`:

```
GET /api/v1/clusters/{id}/why-cost?from=&to=      → *explain.Attribution
GET /api/v1/clusters/{id}/explain?subject=[&from=&to=]  → *explain.Explanation
```

`subject` is `Kind/namespace/name/container` (a container template) or
`Kind/namespace/name` (the workload). `from`/`to` are RFC3339 and **required
for `why-cost`**; `explain` defaults to a 24-hour window ending one second
after the **latest ingested snapshot** — never a clock — and echoes the
resolved instants in the payload
(`TestExplainDefaultWindowIsResolvedFromHistoryNotAClock`).

Two things `cmd/` gets for free and should use rather than duplicate:

- **`Brain.WhyCost(cluster, from, to)`** and **`Brain.Explain(cluster,
  subject, from, to)`** are exported, so `kilter why-cost` and
  `kilter explain` can be re-pointed at a running brain's own substrate
  instead of a hand-fed `--kube-snapshot` list. Both call `Verify` internally;
  neither can return an unverified payload.
- **The `LedgerEntry` projection is now in `pkg/api`** (`Brain.ledgerActions`),
  where pkg/explain/FINDINGS.md §1 said it belonged. `cmd`'s
  `loadLedgerActions` remains correct for the `--ledger` *file* path and was
  lifted, not re-derived: same field mapping, same `StatusDone`-only rule,
  same `(At, Fingerprint)` sort.

Status codes are three, because the three failures are different facts:
**404** the cluster was never ingested; **422** the substrate cannot support
an answer (fewer than two timeline points in the window, no history for the
subject) — this is the case a never-populated brain hits and it must read as
"not enough evidence", not as a fault; **500** `Verify` failed, which is a
defect in the process and the one case worth paging on.

---

## 4. Determinism

- **No `time.Now()` in any decision path added here.** The history's keys come
  from `snap.Timestamp`; a snapshot with no timestamp is **refused** rather
  than stamped with a clock, because a history whose keys depend on when the
  process happened to run is not replayable. The explain window resolves
  against the latest ingested snapshot.
- **No map iteration reaches output.** `basisFrom` sorts node groups and
  namespaces before appending (they are summed downstream); `ledgerActions`
  sorts by `(At, Fingerprint)`; the substrate's events are sorted by
  `(Kind, Subject)` before `Append`, because arrival sequence is the
  substrate's eviction and tie-break key **and is checkpointed** — an
  unsorted append would make a restored substrate differ from the one that
  produced it.
- **Usage samples are fed oldest-first.** A snapshot carries a whole window of
  rows in collector order; feeding them unsorted would make every row older
  than the newest a dropped sample, and the substrate would hold a sparse,
  arrival-order-dependent subset of a series it was handed whole.
- **The shuffle test.** `TestWhyCostIsIndependentOfIngestOrderAndRepeats`
  permutes node and usage order inside every snapshot three ways and requires
  a byte-identical attribution, repeating each permutation three times *in one
  process* (Go randomizes map iteration on every `range`, so in-process
  repetition is the real test). `TestSnapshotHistoryIsUnaffectedByQueryOrder`
  does the same for the history read.
- **During a rollout, "the" container spec is the newest pod's** — ordered by
  `(CreatedAt, UID)`. Old and new pods coexist with different specs, so
  "current sizing" is otherwise ambiguous and would be decided by pod order.

---

## 5. Bounded everything

| Thing | Bound | Test |
|---|---|---|
| Snapshot history rows | 768/cluster | `TestSnapshotHistoryEvictsPastTheCountCap` |
| Snapshot history bytes | 32 MiB/cluster (+ ≤8 MiB overhang) | `TestSnapshotHistoryEvictsPastTheByteBudget` |
| One stored record | 8 MiB framed, 256 MiB decompressed | `TestFramedRecordSizeIsWellUnderTheRecordCap` |
| Query window | 400 days, refused above | `TestSnapshotWindowWiderThanTheCapIsRefused` |
| Route window | 400 days, refused above | `TestExplainRoutesRefuseAnUnboundedWindow` |
| Evidence checkpoint | 64 MiB decoded | `TestOversizeCheckpointIsRefusedAtWrite` |
| Evidence substrate | `evidence.Config`, unchanged | pkg/evidence's own suite |
| OOM/deploy detector state | rebuilt per ingest from the current snapshot | `TestSubstrateStateDoesNotGrowWithPodChurn` |

The last row is the only new unbounded-*looking* state. Pods churn forever, so
the detector's two maps are **rebuilt** on every ingest, carrying forward only
keys the current snapshot still mentions: their size is the size of the
cluster, not of its history.

---

## 6. Deliberately deferred, with reasons

- **Keyframe-plus-delta encoding.** §6.2 named it as one realistic shape.
  Cadence thinning plus gzip framing gets a measured 22–31× on the same
  problem for a tenth of the machinery, and — unlike a delta chain — a single
  corrupt record costs one snapshot rather than every snapshot after it. If
  the 200-node cluster's 16-day window proves too short, the next lever is a
  *reduced replay snapshot* carrying only the fields `recommend` and `plan`
  read; that changes what a replay sees, so it needs its own equivalence test
  against the full snapshot and was not worth guessing at here.

- **A deploy that changed only the image is invisible.**
  `model.ClusterSnapshot` carries no image and no generation, so a deploy is
  detected from what a snapshot *does* carry: declared requests/limits and
  replica count. Inventing a deploy event from a pod restart would claim a
  spec change nobody observed.

- **`Sample.ThrottleRatio`, `Restarts` and `OOMs` stay zero.** `model.Usage`
  carries CPU, memory and a window and nothing else. Those signals reach the
  substrate as events from other collectors; a zero throttle ratio invented by
  the wiring would be a claim nobody measured, and "signal absent" is a state
  `pkg/decision` already knows how to read. This is `cmd/`'s `observeUsage`
  finding, unchanged and for the same reason.

- **An OOMKill event is stamped with the snapshot instant, not the kill
  instant.** The snapshot does not carry the termination time. The attr
  `observedAt=snapshot` says so on every such event, so a reader is never
  misled about the precision of a citation.

- **`Explanation.Verdict` is still nil, so `Action` is `unknown`.**
  `pkg/recommend` does not import `pkg/decision`, so production has no verdict
  to read out. Calling `decision.Evaluate` in the route would answer a
  question production never asked — a *different* answer wearing this one's
  clothes. This is cmd/WIRING-FINDINGS.md §6.4, unchanged; it needs the
  `Recommender.Verdicts(snap)` seam `pkg/backtest` asked for.

- **Fargate is still priced into the residual.** `pricing.SnapshotCost`
  includes Fargate pods; `basisFrom` excludes Fargate nodes, so the difference
  is reported as residual with a note. Correct and coarse, and unchanged by
  this unit — the fix is pkg/explain's second `CostBasis` dimension.

- **`why-cost`'s composition edges are the first and last retained snapshots
  in the window, which may not be the same instants as the first and last
  timeline points.** The history is thinned to hourly while timeline points
  land on every ingest. `explain.WhyCost` already detects and *notes* the
  disagreement ("start composition holds N nodes but the observed timeline
  point holds M; the difference lands in the residual"), so the answer stays
  additive and the approximation stays visible. Making the two agree exactly
  means either observing timeline points only at the thinning cadence — which
  would coarsen ΔCost, the measurement the whole decomposition explains — or
  keeping a snapshot per timeline point, which is the disk bomb. The note is
  the honest third option.

- **Recommender state is still checkpointed per cluster while the evidence
  substrate is checkpointed whole.** `evidence.Memory` is fleet-wide (its
  subjects carry their own cluster), so it is one blob under one key. At the
  `CheckpointEvery` default of 10 snapshots this re-marshals the whole
  substrate roughly every 50 minutes per cluster. That is fine at the sizes
  measured above and will not be at ten times them; the seam for fixing it is
  `evidence.Checkpoint`'s already-sorted `Subjects` slice, which could be
  written per-subject under separate keys without changing the format.

- **The number of clusters is not itself bounded.** `SaveSnapshotAt` bounds
  each cluster's history; nothing bounds how many clusters may report, which
  is the pre-existing behaviour of every other bucket in `pkg/store`. Worst
  case is therefore 40 MiB × clusters. `evidence.Config.MaxTimelineClusters`
  (256) bounds the substrate side already.
