# T1 — three stale refusals retired, one seam at a time

`cmd/WIRING-FINDINGS.md` §6.2 and §6.3 refused two things because the substrate
did not exist. It does now (`pkg/api/SUBSTRATE-FINDINGS.md`), so those refusals
were stale — they told an operator to go and build something that was already
built. This unit retires them.

**Nothing here deletes a refusal.** The one in §6.2 moved into `pkg/api` as
`ErrNoHistory` and `ErrHistoryTooShort`, and the whole job was making it
reachable from a command line with a non-zero exit rather than making it go
away.

```
cmd/kilter/brainsource.go        open-the-brain-from---db, written once for three verbs
cmd/kilter/backtest.go           --cluster reaches Brain.Backtest; --from/--to now required
cmd/kilter/explain.go            --db/--cluster added to BOTH verbs, beside --kube-snapshot
cmd/kilter/brainsourcewire_test.go   the refusals, the replay, and the two agreements
cmd/kilter/sourcewire_test.go        source selection and every flag refused by name
cmd/kilter/actuatorwire_test.go      the actuators are still unreachable, asserted structurally
```

**Status:** `gofmt -l ./cmd` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./cmd/...` and `go test -race -short ./...` all green.
**`go.mod` and `go.sum` are unchanged** — every import added is stdlib or
intra-repo, and no file outside `cmd/kilter/` was touched.

| | |
|---|---|
| New production code | 207 lines (`brainsource.go`), plus the rewiring in two files |
| New tests | 16 test functions, 1,017 lines across three files |
| Coverage, `cmd/kilter` | **58.1 %**, from 45.1 % |

---

## 1. What each verb reaches now, and over which flag

| Verb | Flag | Reaches | Refuses via |
|---|---|---|---|
| `backtest --cluster ID --db P --from T --to T` | `--db` | `api.Brain.Backtest` over `store.Snapshots` | `api.ErrNoHistory`, `api.ErrHistoryTooShort` |
| `why-cost --db P --cluster ID --from T --to T` | `--db` | `api.Brain.WhyCost` (calls `Verify`) | unknown cluster; `notEnoughEvidence`; `unverifiable` |
| `explain --db P --cluster ID --workload W --container C` | `--db` | `api.Brain.Explain` (calls `Verify`) | unknown cluster; `notEnoughEvidence`; `unverifiable` |

Every one of these is a **sibling** of the file-backed flag, which is
unchanged: `--kube-snapshot` (repeatable) and `--ledger` still answer without a
database, still need no brain, and are still the only way to explain snapshots
that were never ingested anywhere. §3 of the substrate findings is followed as
written; the three places this unit went beyond it are in §5 below and the two
places it declined to are in §6.

The shared open-the-brain helper is `brainsource.go`, used by all three verbs.
It carries four operator-facing facts that are properties of bbolt and of the
brain, not of these commands, and that a reader of the code cannot see:

1. **The database must already exist.** `bolt.Open` *creates* the file it is
   given, so a mistyped `--db` would otherwise make an empty database and
   answer "no history for that cluster" — a true statement about the file it
   had just created, and a completely misleading one about the cluster. All
   three verbs `os.Stat` first and refuse by name, and
   `TestReadVerbsRefuseAMissingDatabaseRatherThanCreatingOne` asserts the file
   is still absent afterwards.
2. **The lock is exclusive, so these verbs cannot read a running brain's
   database.** `store.Open` takes bbolt's file lock with a 5-second timeout.
   The failure surfaces as `timeout`, which does not say why, so the wrapper
   offers the cause without asserting it (a permission or corruption failure
   arrives at the same line). The operational recipe is: stop the brain, or
   point `--db` at a copy.
3. **Opening writes.** `store.Open` creates missing buckets inside an `Update`
   transaction, so even these read-only verbs touch the file. Nothing here
   ingests, plans or actuates.
4. **The freshness of the two substrates differs, and one of them lags.** See
   §2 — this is the finding most likely to cost somebody an afternoon.

## 2. The two substrates do not go stale together, and it shows

`SaveSnapshotAt` runs inside **every** `Ingest`, so `backtest --cluster` sees
everything the retention kept, up to the last snapshot the brain received.
`evidence.Memory` is persisted only every `BrainConfig.CheckpointEvery`
snapshots (default 10) and on graceful shutdown, so `why-cost --db` and
`explain --db` see the substrate **as of the last checkpoint**.

Measured, not reasoned about — a brain given three snapshots at the default
checkpoint cadence, then read by the binary:

```
$ kilter why-cost --db brain.db --cluster prod --from … --to …
kilter why-cost: why-cost --cluster prod: the evidence substrate holds 0 timeline
point(s) for "prod" inside […); ΔCost needs two observations to be a measurement.

$ kilter backtest --cluster prod --db brain.db --from … --to …     # SAME database
kilter backtest — cluster prod, replayed from brain.db
  policy 69955a2cdd2004fd
    scored 12  decisions 4 …
```

Both answers are correct and they are about the same database. The failure mode
to be aware of is that "not enough evidence" reads as a statement about the
cluster and is partly a statement about **when the brain was last stopped**.
`kilter brain` exposes no `--checkpoint-every`, so the mitigation is a graceful
shutdown (`Serve` checkpoints on `ctx.Done()`), and the tests that need a
persisted substrate build their brain with `CheckpointEvery: 1` for exactly
this reason.

This is deliberately **not** pinned by a `cmd/` test. The cadence is
`pkg/api`'s policy; a cmd test asserting it would fail the day `pkg/api`
changed its own default, which is not a fact about the command line.

## 3. The refusals that survived, and the test pinning each

| Refusal | Where it lives | Pinned by |
|---|---|---|
| `ErrHistoryTooShort`, **`Instants == 0`** — a real history that replays nothing | `pkg/api/backtest.go` | `TestBacktestOverADatabaseRefusesAHistoryThatWouldScoreNothing` |
| `ErrHistoryTooShort`, **count < 2** — the one-snapshot case §6.2 was written about | `pkg/api/backtest.go` | `TestBacktestLiveHistoryRefusesRatherThanScoringOneSnapshot` (re-pointed), `TestBacktestOverADatabaseRefusesASingleSnapshot` |
| the same, with **zero** snapshots — a database that never saw the cluster | `pkg/api/backtest.go` | `TestBacktestOverADatabaseRefusesAClusterWithNoHistory` |
| `--from`/`--to` required for `--cluster` | `cmd/kilter/backtest.go` | `TestBacktestClusterRequiresAFixedWindow` |
| unknown cluster, the CLI form of the routes' 404 | `cmd/kilter/brainsource.go` | `TestBrainBackedVerbsRefuseAnUnknownCluster` |
| a missing `--db` is not created | `cmd/kilter/brainsource.go` | `TestReadVerbsRefuseAMissingDatabaseRatherThanCreatingOne` |
| two sources at once | `cmd/kilter/explain.go` | `TestTheTwoSourcesAreExclusiveAndSaySo` |
| a flag the source cannot honour | `cmd/kilter/brainsource.go` | `TestFlagsThatCannotBeHonouredAreRefusedNotIgnored` |

Every refusal test asserts **no scorecard was printed** (`assertNoScorecard`
checks for `regret`, `oracleGap`, `flipRate` and `safety` in the output), not
merely that an error came back. The error is the easy half; the trap is output
that reads as a verdict.

### 3.1 Building a history that is real and still unscoreable

The `Instants == 0` case took three attempts to reach and the two dead ends are
worth recording, because a future reader will otherwise write the same test and
believe it.

- **Two snapshots two hours apart, window `[t0, t0+2h)`.** The window is
  half-open, so the snapshot at exactly `t0+2h` is outside it and only one
  snapshot is in range: this hits the *count* branch, not the coverage branch.
- **Widening to `[t0, t0+3h)` with a 24h horizon.** Now `backtest.Run`'s own
  guard fires first — `horizon 24h0m0s exceeds the replay window 3h0m0s` —
  and `Instants` is never computed.
- **What works:** a window WIDER than the horizon whose snapshots all land too
  late in it. Two snapshots at `t0+30h` and `t0+31h` in `[t0, t0+48h)` with a
  24h horizon: `decisionInstants` snaps every grid point forward to the first
  snapshot at or after it, then rejects it because `snapshot + horizon > to`.
  Two retained snapshots, zero instants, `Scorecard.Snapshots == 2`.

That third shape is not contrived. It is a brain that started recording late in
the window an operator asked about — the single most likely way to hit this in
production, which is precisely why a count check would have been the wrong
gate.

## 4. Where the two sources are proven to agree

`TestBrainAndFileSourcesAgreeOnWhyCost` runs the same three snapshots through
both sources over the same window and requires the `--json` attribution to be
**byte-identical**. JSON rather than prose, because JSON carries the evidence
IDs: the terms, the residual and every citation have to match, not just the
dollar amounts.

"Where both can answer" is a real qualifier and **two** things narrow it. Both
were found by running the test, not by reading the code:

- **Spacing.** `pkg/store` thins the history to one snapshot per hour, so a
  5-minute series leaves the brain with fewer composition edges than the file
  series holds. The fixture is spaced 12 hours apart, where the retention keeps
  everything.
- **Events.** The brain's substrate also records deploys and OOMKills; the file
  path observes timeline points only. With a workload whose requests change
  across the window, the brain's answer carries two extra citations
  (`evt/…/deploy@…`) that the file's cannot.

That second one is pinned rather than hidden, by
`TestTheBrainCitesMoreThanAFileCanAndTheMoneyIsUnchanged`: every term kind and
every amount is identical, the brain's citations are a strict **superset**, and
at least one of the extras is a deploy event. If the amounts ever diverged, one
of the two sources would be wrong about what a cost change is; if the citations
were equal, the brain would be discarding evidence it holds.

A third agreement is asserted for `explain`, against a different oracle:
`TestExplainOverADatabaseMatchesTheHTTPRoute` compares
`kilter explain --db` with `GET /api/v1/clusters/{id}/explain` over the same
database, through `Brain.Handler()` and an `httptest.NewRecorder` (no socket is
opened). This exists because `pkg/api`'s `defaultExplainWindow` is unexported
and `cmd` therefore carries its own `brainExplainWindow` constant — a constant
copied by hand is a constant that drifts. The test pins the span *and* the
one-second edge to the route rather than to a comment.

## 5. Where this went beyond §3, and why

§3.1's sketch is `api.NewBrain(api.BrainConfig{}, catalog, st)` and
`return err`. That is what the code does. Three things it does not mention had
to be decided:

1. **The demo-only flags are refused by name on the `--cluster` path**
   (`--days`, `--workloads`, `--noise`, `--derive-costs`, `--policy`,
   `--compare`, `--enforce-refusals`, `--fail-on-regression`), and the live
   flags (`--db`, `--from`, `--to`) are refused on the `--demo` path. Silently
   ignoring them was the alternative, and `loadPolicy` already records why that
   is unacceptable: a knob that is dropped produces a report for a
   configuration nobody ran. `flag.FlagSet.Visit` is what makes this exact — it
   distinguishes an explicit `--days 7` from an unmentioned `--days`.
2. **`--policy`/`--compare` are refused rather than plumbed.**
   `Brain.Backtest` scores *this brain's* policy, which is the question a live
   replay answers; an A/B needs a policy argument `pkg/api` deliberately does
   not take. The seam, if it is ever wanted, is `api.BrainConfig`'s `Recommend`
   and `Plan` fields, and the reason it was not taken here is that
   `Brain.Backtest` builds its harness with no `Decision` config at all, so
   `--enforce-refusals` — the flag behind `pkg/backtest`'s headline result —
   could not be honoured either way. Half an A/B is worse than none.
   *Related fact, checked:* `kilter brain` passes only `Token`, `ReadToken` and
   `ForecasterURL` to `BrainConfig`, so `BrainConfig{}` here scores the same
   recommender and planner a served brain runs. That equivalence breaks the day
   `kilter brain` grows a policy flag.
3. **The unknown-cluster check is in `cmd`, for two verbs and not the third.**
   `why-cost` and `explain` call `requireCluster` (the CLI form of the routes'
   404 — "never ingested" and "ingested but thin" are different facts).
   `backtest --cluster` deliberately does **not**: its refusals belong to
   `pkg/api`, and a pre-check would shadow `ErrHistoryTooShort` — including in
   the re-pointed test, whose fixture is a database with exactly one snapshot.

Nothing in §3 was diverged from. §3.2's "nothing to do in `cmd/`" was read as a
statement about the *routes*, which are indeed complete; the same paragraph
then says `cmd` should use `Brain.WhyCost`/`Brain.Explain` rather than
duplicate them, which is what the `--db` flags do.

### 5.1 `cmd/kilter/whycost.go` was not created

The scope named it; it does not exist and did not before. `kilter why-cost`
lives in `explain.go` beside `kilter explain`, and the brain-backed path was
added next to the file-backed one it must agree with. Splitting one branch of
one verb into its own file would put the two answers to the same question in
two files, which is the arrangement most likely to let them drift.

## 6. What is still unreachable after this unit

- **The actuators.** `pkg/ec2/actuate*.go` and `pkg/rds/actuate*.go` remain
  unreachable and this unit added no path toward them. Asserting that is
  awkward, because both packages are *already linked into the binary* for their
  collectors (`cmd/kilter/rds.go`, `cmd/kilter/rdslive.go`), so an import-graph
  check answers the wrong question. `TestNoActuatorIsReachableFromTheBinary`
  instead derives the forbidden set from the source — every exported identifier
  declared in an `actuate*.go` file of either package, 212 of them today — and
  parses every non-test file in `cmd/kilter` for a selector naming one. A new
  actuator entry point is covered the moment it is written, with no list to
  maintain; three canaries (`NewActuator`, `Actuator`, `ActuatorConfig`) fail
  the test if the scan ever finds nothing. `TestNoCloudSDKReachesTheNewSources`
  adds the narrower structural check on the three files this unit wrote or
  rewired.
- **A workload-level `explain` subject.** `Brain.Explain` accepts
  `Kind/namespace/name` as well as the four-segment container form, but
  `kilter explain` has required `--workload` AND `--container` since it
  shipped, and making `--container` optional would change what an existing
  command line means. The workload subject stays reachable over HTTP only.
- **An A/B against live history.** See §5, item 2. Needs a policy argument on
  `Brain.Backtest`, and needs `pkg/api` to accept a `Decision` config before
  the comparison worth running (`--enforce-refusals`) is expressible.
- **`Explanation.Verdict` is still nil, so `Action` is `unknown`.** Unchanged
  and for the unchanged reason: `pkg/recommend` does not import
  `pkg/decision`, so production has no verdict to read out
  (cmd/WIRING-FINDINGS.md §6.4, SUBSTRATE-FINDINGS.md §6). This is visible on
  the `--db` path exactly as it is on the `--kube-snapshot` path — the prose
  reads `Verdict: none recorded.`
- **The live RDS collector** (§6.1) and **`pkg/rds`'s `StorageParity` seam**
  (§6.4) are untouched: both are `go.mod` questions and this unit changed no
  module.

### 6.1 One observed gap in §3.2's status taxonomy

§3.2 says a subject with no history is a **422**. It is not, in the case a
typo produces: `explain.BuildExplain` returns a *payload* for an unknown
subject rather than an error, carrying the note "no evidence is stored for this
subject in this window; the payload states the decision but grounds none of
it". `Brain.Explain` passes it through, the route answers 200, and
`kilter explain --db` prints it and **exits 0**.

Observed directly against the built binary:

```
$ kilter explain --db brain.db --cluster prod \
    --workload Deployment/default/app-0 --container app     # real name is app-00
kilter explain — Deployment/default/app-0/app over [2026-01-09T23:00:01Z, …]
  note: no evidence is stored for this subject in this window; the payload
        states the decision but grounds none of it
$ echo $?
0
```

The payload is honest — it says it grounds nothing — so this is a question of
whether "you asked about a subject that does not exist" deserves a non-zero
exit, not a question of a misleading answer. It is left alone because the fix
belongs in `pkg/explain` or `pkg/api`, both outside this unit, and because the
CLI's job here is to be faithful to the route: it is, byte for byte (§4).

## 7. Determinism

- **No clock in any answer.** `backtestEpoch` still fixes the `--demo` window;
  `--from`/`--to` are now required for `--cluster`, so a live scorecard's
  window is an argument rather than a drifting default; `explain --db`'s
  default window is resolved from the **latest ingested snapshot** in the
  database, never `time.Now()`. `TestBacktestOverADatabaseScoresTheRetainedHistory`
  asserts two replays of the same database are byte-identical.
- **One renderer per artefact.** `writeBacktestReport` and
  `renderAttribution`/`renderExplanation` are shared by both sources, so the
  same answer prints the same bytes whichever substrate produced it — which is
  what makes the agreement tests in §4 a comparison of answers rather than of
  layouts. Only the header line differs, and it names the source.
- **No new ordering.** Every sort that matters (`basisFrom`'s groups and
  namespaces, `ledgerActions`' `(At, Fingerprint)`, `writeScorecard`'s refusal
  codes) is unchanged and now runs inside `pkg/api` for the `--db` path, where
  SUBSTRATE-FINDINGS §4 covers it.
- **No network, no cloud call, no credential** in any test. The one HTTP
  comparison uses `httptest.NewRecorder` against `Brain.Handler()` in-process,
  so not even a loopback socket is opened. Databases are built in `t.TempDir()`
  through the real `api.Brain.Ingest`.
