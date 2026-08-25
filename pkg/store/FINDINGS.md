# pkg/store hardening findings

Session scope: `pkg/store` only. Every fix below has a regression test that was
verified to FAIL against the pre-fix `store.go` (by swapping the original file
back in) and pass after.

## Bugs found and fixed

1. **`LatestPlan` returned a stale plan whenever timestamps shared a second.**
   Plan keys are `"<cluster>/<timestamp>"` and the timestamp was rendered with
   `time.RFC3339Nano`, which *trims trailing zeros* — so keys are not
   fixed-width and their byte order (which is the only order bbolt has) does
   not match chronological order:

   | instants | keys | byte order |
   |---|---|---|
   | `00.0` < `00.5` | `…00Z`, `…00.5Z` | `…00.5Z` < `…00Z` ✗ |
   | `00.2` < `00.23` | `…00.2Z`, `…00.23Z` | `…00.23Z` < `…00.2Z` ✗ |
   | `…780` < `…785` | `….12345678Z`, `….123456785Z` | reversed ✗ |

   `LatestPlan` took the last key in the cursor scan, so it served an older
   plan as the current one. This is not an edge case: `Plan.CreatedAt` is the
   snapshot timestamp, i.e. `time.Now().UTC()` with nanosecond resolution, and
   roughly one nanosecond value in ten has a trailing zero to trim. The
   consequence is a `/api/…/plan` response, a `SavingsMonthlyUSD` metric and an
   executable step list taken from a superseded decision.
   Tests: `TestLatestPlanChronologicalWithSubSecondTimestamps`,
   `TestLatestPlanChronologicalAcrossWholeSecond`.

2. **Pruning deleted the newest plans and kept the oldest** — same root cause.
   `SavePlan` deleted `keys[0]` of the byte-ordered scan until the history fit
   `PlanHistoryLimit`. With 55 plans one millisecond apart, the pre-fix code
   retained the *first* plan written and pruned five newer ones (verified
   directly against the original file). A cluster saving plans faster than once
   a second would keep an arbitrary, mostly-stale window of its audit history.
   Test: `TestPruneDropsOldestByTimeNotByBytes` (asserts the exact retained
   set by reading raw keys), `TestBackdatedPlanDoesNotEvictNewerHistory`.

   Fix for 1 and 2: keys are now written with a fixed nine-digit fraction
   (`planTimeFormat`) so on-disk order is chronological and a raw `bbolt` dump
   reads correctly, **and** every reader decodes the timestamp out of the key
   (`parsePlanTime`) instead of trusting byte order. Decoding is what makes the
   change backward compatible — a history that mixes old trimmed keys with new
   padded ones still orders and prunes correctly.
   Test: `TestLegacyTrimmedKeysOrderWithNewKeys`.

3. **A cluster id containing `/` read and pruned its neighbour's history.**
   Prefix-scanning cluster `"a"` with `"a/"` also matches every key of cluster
   `"a/b"`. EKS ids are routinely ARNs (`arn:aws:eks:…:cluster/prod`), so this
   was reachable with ordinary input: the parent id's `LatestPlan` could return
   the child's plan — one cluster's cordon/evict/delete-node list served under
   another cluster's name — and its `SavePlan` prune loop could delete the
   child's plans. Fixed by requiring the key suffix to parse as exactly one
   RFC3339 instant: `"a/b/<ts>"` leaves `"b/<ts>"`, which is not an instant, so
   it is skipped by reads, counts and pruning alike.
   Tests: `TestPlanClusterIDContainingSlashIsIsolated`,
   `TestPlanHistoriesAreMutuallyIsolated`, `FuzzPlanKeyClusterIsolation`.

4. **A non-UTF-8 cluster id wrote a record that could never be read back.**
   Found by `FuzzClusterIDRoundtrip`. The id is used twice — raw as the bbolt
   key and JSON-encoded inside the record — and `encoding/json` rewrites
   invalid UTF-8 as U+FFFD, so key and payload silently disagreed. Now rejected
   at the door by `validateClusterID`, with the id in the error.
   Test: `TestNonUTF8ClusterIDIsRejectedNotMangled`.

5. **A `CreatedAt` outside years 0000–9999 wrote an orphan key.** It renders
   wider than the key layout and parses back as nothing, so the entry was
   invisible to `LatestPlan` and immune to pruning — it would sit in the file
   forever. `SavePlan` now refuses any `CreatedAt` that does not round-trip
   through its own key.
   Test: `TestSavePlanRejectsUnrepresentableCreatedAt`.

6. **Nil-bucket dereference.** `put`, `LoadSnapshot`, `Clusters`, `LatestPlan`
   and `PlanCount` all did `tx.Bucket(...)` and used the result unchecked. A
   file missing a bucket — truncated, or written by another tool — panicked
   inside a transaction instead of returning an error. Now routed through
   `bucket()`.
   Test: `TestMissingBucketsAreErrorsNotPanics`.

7. **`Clusters` returned a partially filled slice alongside its error**, and
   the write paths returned bare `json`/bbolt errors with no `store:` prefix or
   cluster name. Both fixed; errors from every path now name the operation and
   the cluster.

## Hardening added

- **Record identity check.** `LoadSnapshot` and `LatestPlan` now verify that
  the decoded `ClusterID` matches the key the record was filed under. This
  package always writes them in agreement, so a mismatch means corruption, a
  hand-edit, or a literal JSON `null` (which decodes without error into a zero
  value). Returning such a record would mean acting on one cluster's topology
  or eviction list under another cluster's name.
  Tests: `TestRecordClusterIdentityIsVerified`,
  `TestCorruptRecordsReturnErrorsNotZeroValues`.
- `LatestPlan` tracks "found" separately from the value pointer, so an
  externally written zero-length value surfaces as a decode error rather than
  as "this cluster has no plans".
- Ties on equal `CreatedAt` (possible when a history spans the key-format
  change) resolve on key bytes, so `LatestPlan` is deterministic across calls.

## Invariants now documented in the code

- Why `planTimeFormat` must be fixed-width, with the concrete byte-order
  counterexamples, and why readers parse instead of comparing bytes.
- Why `parsePlanTime` requiring an exact instant is what isolates cluster ids
  containing `/`.
- That `k`/`v` from `forEachPlan` alias bbolt's mmap and only live as long as
  the transaction.
- That `SavePlan` keys on `CreatedAt`: re-saving the same instant *replaces*
  the entry, and a backdated save into a full history prunes itself.
- That `SaveSnapshot` is last-write-wins and does **not** compare `Timestamp`
  — it deliberately mirrors the brain's in-memory `lastSnap`.
- That `json.Marshal` rejects NaN/±Inf, which makes a poisoned float abort the
  whole transaction and leave the prior value intact rather than half-written.
- That `Clusters` is ascending-ordered and lists snapshot owners only.
- That `PlanHistoryLimit` is applied by `CreatedAt`, not insertion order.

## Tests added

26 new tests: `hardening_test.go` (22) and `fuzz_test.go` (4). Coverage of
`pkg/store` is 95.2% of statements; the remainder is bbolt-internal failure
paths (a `Put` that fails after validation, `CreateBucketIfNotExists` failing)
that cannot be provoked without faking the DB.

Beyond the regression tests listed above: adversarial cluster ids (empty, bare
slashes, NUL, newline, unicode, timestamp-shaped, past bbolt's 32 KiB key
limit); NaN/±Inf floats in both plans and snapshots, asserting the prior value
survives the failed write; corrupt and `null` records; missing buckets;
persistence across close/reopen; `Open` on an empty path, a directory and a
missing parent; `Open` timing out on a locked file rather than hanging
(skipped under `-short`); concurrent `SavePlan` holding the history limit; nil
arguments; unknown-cluster reads.

The four fuzz targets cover the invariants the scheme rests on: key/time
round-trip and byte-order-equals-time-order (`FuzzPlanKeyOrderMatchesTime`),
cross-cluster key attribution (`FuzzPlanKeyClusterIsolation`), end-to-end
history bounds and latest-selection (`FuzzPlanHistoryInvariants`), and
arbitrary cluster-id round-trip (`FuzzClusterIDRoundtrip`, which is what found
bug 4). Each was run for 45–90s beyond the seed corpus with no failures.

## Deliberately left undone

- **The plan-key format change is forward-only, not a migration.** Old trimmed
  keys are read, ordered and pruned correctly, but they are not rewritten, so a
  raw `bbolt` dump of a pre-upgrade file still shows mixed widths. Rewriting
  keys on open would need a migration path (and a version marker) that is
  larger than this package's scope and riskier than the read-side fix.
- **A far-future `CreatedAt` still poisons `LatestPlan`.** An agent whose clock
  reads 2099 writes a plan that stays "latest" forever and never prunes.
  Rejecting it needs a wall-clock skew bound, which belongs at ingest —
  `pkg/api` already normalises a zero `Timestamp` and is the right place to
  bound a future one. Adding a clock dependency to the persistence layer would
  also make every test here time-relative. Flagged, not fixed; out of scope.
- **No delete/GC API.** Snapshots and up to 50 plans per cluster persist
  forever, including for clusters that stopped reporting years ago. That is the
  existing design, not a defect, but the file has no upper bound in the number
  of clusters. A `Forget(cluster)` would be an API addition, not a fix.
- **`SaveSnapshot` still lets an older snapshot overwrite a newer one.** The
  doc comment now says so explicitly. Enforcing monotonicity here alone would
  make the persisted value disagree with the brain's in-memory `lastSnap`,
  which is a cross-package decision (`pkg/api` is owned by another session).
- **`SaveRecommenderState` is still all-or-nothing per cluster.** One container
  with an unserializable histogram weight fails the whole cluster's checkpoint.
  Dropping the bad entry silently would be worse (learning would vanish with no
  signal), and filtering belongs where the states are produced, in
  `recommend.Checkpoint`. The failure is at least loud and non-destructive: the
  previous checkpoint survives.
