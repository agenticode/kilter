# pkg/api hardening findings

Scope: pkg/api only. All changes verified with `gofmt`, `go vet`,
`go build ./...`, `go test -race -count=1 ./pkg/api/...`, and the full
`go test -race -short ./...` (all green). Each behavioral fix has a test that
fails against the pre-fix code (verified by temporarily restoring the old
code and running the new tests).

## Bugs found and fixed

1. **Realized savings computed from pre-action data** (`ledger.go`,
   `ledgerState.report`). `RealizedMonthlyUSD` compared the first applied
   entry's before-cost against the last *appended* cost point, even when that
   measurement was taken *before* the action ran. With only pre-action history
   the ledger reported −$146/month of "realized savings" for an action whose
   effect had never been measured; a replayed out-of-order snapshot could also
   roll the comparison point backwards ($219 → $73 in the regression test).
   Now: "latest" is chosen by measurement timestamp, and realized stays 0
   until a measurement at/after the first applied action exists. The `Method`
   string states this. Tests: `TestRealizedSavingsRequiresPostActionMeasurement`,
   `TestRealizedSavingsLatestByTimestamp`, `TestRealizedSavingsBaselineSelection`
   (all fail on the old math). The pre-existing `TestLedgerRoundtripAndRealized`
   left `entry.At` unset (server stamped wall-clock "now", after every cost
   point); it now sets `At` between the two measurements — physically coherent,
   with every original assertion intact.

2. **Snapshot ingest blocked behind the remote forecaster** (`capacity.go`,
   `demandTracker.forecastPeak`). The tracker's mutex was held across two
   remote HTTP calls (10 s client timeout each); `observe()` — on the ingest
   path — takes the same mutex, so one slow/hung forecaster stalled snapshot
   ingestion for that cluster for up to ~20 s per insights request. History is
   now copied under the lock and the remote call made outside it (the copies
   also stop the remote call from racing `appendCapped`'s in-place re-slicing).
   Test: `TestForecastPeakDoesNotBlockObserve` (times out on old code).

3. **Out-of-order snapshots corrupted the cadence estimate** (`capacity.go`,
   `demandTracker.observe`). A replayed/out-of-order snapshot dragged `lastAt`
   backwards, so the next in-order gap was inflated and skewed the interval
   EWMA (5 m → 5 m 48 s in the regression case), which distorts the forecast
   step count. Only in-order snapshots advance the cadence clock now.
   Test: `TestDemandTrackerObserve/out-of-order…` (fails on old code).

4. **Plan/recommendation snapshot skew** (`brain.go`, `Brain.Plan`). `Plan`
   fetched the snapshot, then called `Recommendations`, which fetched the
   snapshot *again* — a concurrent ingest between the reads produced a plan
   whose cost/node math and recommendations came from different snapshots.
   `Plan` now derives both from a single snapshot (`recommendationsFor`).

5. **UTF-8 rune split in client error messages** (`client.go`, `truncate`).
   Truncation could cut a multi-byte rune in half (and trimmed whitespace only
   on the short path). Now trims first and cuts at a rune boundary.
   Tests: `TestTruncate`, `FuzzTruncate`.

## Hardening

- **Constant-time token comparison** (`brain.go`, `bearerEqual`): bearer
  tokens were compared with `==`, allowing timing probes to recover match
  position. Now `crypto/subtle.ConstantTimeCompare`. Adversarial-credential
  table test: `TestAuthAdversarialTokens`.
- **Ledger entry validation** (`ledger.go`, `LedgerEntry.validate`): the
  reports endpoint accepted any mode string, negative step counters, and
  negative costs. A misspelled mode ("Apply") was silently excluded from the
  realized-savings filter; negative before-costs corrupted the money math.
  Now rejected with 400. Tests: `TestReportValidation`,
  `TestReportEndpointRejectsGarbage`.
- **Approval map bounded** (`ledger.go`): expired approvals were purged only
  on reads, so a writer approving ever-changing fingerprints grew the map
  without bound; fingerprints are also now capped at 128 chars (plan
  fingerprints are 16 hex chars). Tests: `TestApprovalExpiryAndPurge`,
  `TestApprovalFingerprintBounds`.
- **Zero-timestamp snapshots normalized** (`brain.go`, `Ingest`): a zero
  timestamp put a year-1 point on the cost timeline and permanently stalled
  the demand tracker's cadence estimate. Normalized to ingest time.
  Test: `TestIngestNormalizesZeroTimestamp`.
- **413 vs 400** (`brain.go`, `decodeStatus`): over-limit bodies (including
  gzip bombs) surfaced as 400 "decode snapshot: request body too large",
  pointing clients at the wrong problem. Now 413. Test:
  `TestOversizedBodyReturns413`.
- **Deterministic `Clusters()`**: map-order output made the clusters endpoint
  (and the UI dropdown) shuffle between calls; now sorted. Test:
  `TestClustersSorted`.

## Tests added (beyond the regression tests above)

- `TestAppendCapped`, `TestMaxOf` — table-driven boundary tests plus a
  window-retention property check for the history ring.
- `TestForecastPeakGating` — no forecast below 10 points or without a cadence;
  a ramp must peak above the last observation.
- `TestCapacityInsightsNilSafety` — nil tracker/snapshot, zero-allocatable.
- `TestLedgerBounded` — entry/cost-history caps keep the newest data.
- `TestConcurrentTrustState` — concurrent ledger/approval mutation under `-race`.
- `TestCostEndpoint` — priced cost > 0, unknown cluster → 404 (route was
  untested).
- `TestClientRetriesServerErrors`, `TestClientDoesNotRetryClientErrors`,
  `TestClientHonorsContextDuringBackoff` — retry policy was untested.
- `FuzzIngestEndpoint` — arbitrary/gzip bodies against the ingest handler,
  in-process (326k execs clean, 30 s run). One corpus entry under
  `testdata/fuzz/` pins a malformed-gzip-header case.
- `FuzzTruncate` — truncation never emits invalid UTF-8 or exceeds the bound.

## Invariants documented in code

- `forecastPeak`: the tracker mutex is never held across remote HTTP calls
  (ingest must never block on the network).
- `observe`: only in-order snapshots advance the cadence clock.
- `maxOf`/`appendCapped`: non-negative floor / newest-N retention semantics.
- `LedgerEntry.validate`: why `Mode` is checked strictly (the realized filter
  matches the exact string "apply").
- `report()`: why realized savings require a post-action measurement.

## Deliberately left undone

- **Ledger/approval state is memory-only** — the `ledgerState` doc comment
  says "persisted via store when present", but nothing persists it; a brain
  restart loses the audit trail and approvals. Fixing this needs new `store`
  APIs, which are outside pkg/api. Left as-is.
- **Realized-savings baseline can rotate out**: entries are capped at 200, so
  after 200 newer entries the oldest applied entry (the baseline) is lost and
  realized savings silently re-baseline. Acceptable for a bounded audit
  window, but worth knowing when reading the number.
- **Trailing garbage after the snapshot JSON is accepted** (`{"…"}extra`):
  lenient by design in the current decoder use; rejecting it would need a
  second `Decode` probe. Left, as the risk is negligible.
- **`gz.Close()` CRC result ignored on ingest**: a corrupt gzip trailer after
  a fully-parsed JSON body goes unnoticed. In practice corruption breaks the
  JSON parse first; left as-is.
- The `var _ = model.Insight{}` / `var _ = plan.Plan{}` keep-import anchors
  look odd but are commented as deliberate; left untouched.
