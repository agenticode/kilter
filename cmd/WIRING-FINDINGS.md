# P2 — wiring: three shipped packages become reachable from the binary

`pkg/rds`, `pkg/backtest` and `pkg/explain` shipped last cycle and **none of
them was reachable from `kilter`**. Each deferred its wiring to a FINDINGS.md,
exactly as PR#29 (j1-wire) had to clean up once before. A user who built the
binary could not run any of them.

Four commands now do:

```
kilter domains report --rds-fixture …   observe → refuse → report, across the RDS domain
kilter backtest --demo <archetype>      score a policy against history; Gate; CI exit code
kilter explain  --workload … --container …   why the engine would resize this container
kilter why-cost --from … --to …         an additive, individually-citable cost decomposition
```

**Status:** `gofmt -l ./cmd ./pkg/domain` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./cmd/... ./pkg/domain/...` and `go test -race -short ./...`
(35 packages) all green. **`go.mod` and `go.sum` are unchanged** — every import
added is stdlib or intra-repo.

| | |
|---|---|
| New production code | 1,419 lines across `cmd/` and `pkg/domain/rds/` |
| New tests | 1,958 lines, 40 test functions |
| Coverage | `pkg/domain` **89.3 %**, `pkg/domain/rds` **89.5 %**, `cmd/kilter` 45.1 % |

```
pkg/domain/domain.go        + the RDS kind (the two-line core change §6.1 owed)
pkg/domain/rds/             the pkg/rds adapter: registration + the usage-line projection
pkg/domain/hostilerds_test.go   the adversarial suite: a domain that lies through four seams
cmd/kilter/rds.go           the three read seams over a recorded account + the rate override
cmd/kilter/backtest.go      kilter backtest: traces wired, live history refused by name
cmd/kilter/explain.go       kilter explain + kilter why-cost, both calling Verify before serving
cmd/kilter/domains.go       + rds registration, the ledger splice, and its output check
```

---

## 1. What is reachable now

### 1.1 RDS — a domain whose entire product is refusals

```
$ kilter domains report --now 2026-08-26T12:00:00Z --domain rds \
    --scope 000000000000/us-east-1 --region us-east-1 \
    --rds-fixture cmd/kilter/testdata/rds-account.json

  DOMAIN         STATE     ACTUATION   TARGETS    RECS REFUSED    CLAIMABLE        GROSS
  rds            ready     report-only       7       0      27        $0.00        $0.00

  What kilter declined to do, and why
    allocated-storage-cannot-shrink        4 refused
    instance-class-change-is-a-failover    4 refused
    no-storage-performance-model           4 refused
    unverified-rate                        4 refused
    freeable-memory-is-page-cache          2 refused
    aurora-not-supported                   1 refused
    …
```

Zero recommendations and zero claimable dollars is the **deliverable**, not a
gap. `pkg/rds/FINDINGS.md` §2: the instance class is where the money is and
changing it is a failover; allocated storage cannot shrink; `FreeableMemory` is
`MemAvailable`. The domain proposes nothing, so its whole output arrives
through the `domain.Refuser` seam — the seam j1-wire added for exactly this
case, now carrying its first domain that has nothing else to say.

`--rds-detail` additionally prints `pkg/rds`'s own report, whose layout is the
opposite of the aggregate's on purpose: refusals first, money second, because
in this domain the refusal *is* the finding and a reader who sees a dollar
figure first reads the refusal as a caveat on a recommendation that does not
exist.

### 1.2 `kilter backtest` — the falsifiability harness

```
$ kilter backtest --demo regime-change --workloads 3 --compare enforced.json

  policy 69955a2cdd2004fd
    scored 21  decisions 18  refusals 3 (good 0, idle 3)
    safety      memViolations 3  cpuStarvation 3  oomKills 0
    efficiency  oracleGap 94.5%  (applied 5.7%)  claimed/realized 0.96
    regret      $303.86  (resource $3.86 + risk $300.00)

  candidate 69955a2cdd2004fd
    scored 21  decisions 15  refusals 6 (good 0, idle 6)
    safety      memViolations 3  cpuStarvation 0  oomKills 0
    efficiency  oracleGap 129.6%  (applied 16.1%)  claimed/realized 1.00
    regret      $157.21  (resource $7.21 + risk $150.00)

  Gate
    ACCEPTED: the candidate dominates on the terms §4.6 defines
```

That is `pkg/backtest/FINDINGS.md`'s headline A/B, reproduced field for field
from the binary: wiring `pkg/decision` into the recommendation path removes
**all** CPU starvation and halves regret, bought with $3.35 of extra idle
headroom. `TestWiringTheDecisionLayerIsAnImprovementThroughTheCLI` asserts the
win *and* that it was paid for — a scorecard reporting only the win would be an
advertisement. The four shipped goldens (steady $2.74, diurnal $4.02, bursty
$2.90, regime-change $202.57) are pinned by
`TestBacktestReproducesTheShippedGoldens`.

### 1.3 `kilter explain` and `kilter why-cost`

```
$ kilter explain --kube-snapshot cluster.json \
    --workload Deployment/default/api --container api

kilter explain — Deployment/default/api/api over [2026-08-23T12:15:00Z, 2026-08-26T12:00:01Z]

Sizing: api request 1000m/8192Mi → 324m/7802Mi, limit 0m/0Mi → 0m/0Mi.
Because:
  behavior-class behavior class "steady" selects the sizing policy applied [dig/1/…@1787486400000000000]
  usage-history  287 samples over 71h45m0s: cpu p95 300m / max 300m, memory p95 6.5GiB / max 6.5GiB [dig/1/…]
```

Both commands **call `Verify` before serving**, and treat a failure as an error
rather than rendering an answer with a dangling citation.
`pkg/explain/FINDINGS.md` §4 is explicit that nothing else enforces §5.7's
"citations must resolve to ids the session actually fetched"; `Verify` exists
and someone has to call it. That someone is now `runWhyCostTo` and
`runExplainTo`.

---

## 2. The core change, and the two test fixtures it made stale

`pkg/domain/domain.go` grew the row `pkg/rds/FINDINGS.md` §6.1 specified:

```go
RDS Kind = "rds"
var kinds = []Kind{EC2, ECSFargate, K8sFargate, K8sNodes, Lambda, RDS}
```

**§6.1's claim was checked, not trusted.** `TestKindIsHonestAboutRegistration`
does pass both before and after: it asserts the property that holds either way
(registered ⇒ the core still refuses to plan steps; not registered ⇒ `Register`
fails with `unknown kind` and the standalone `Report` path works). Verified by
running it on both sides of the edit.

What §6.1 did **not** anticipate is that five other tests used the literal
string `"rds"` as their stand-in for *a kind outside the closed set*. Those
placeholders went stale the moment the kind became real. Every assertion is
unchanged; only the sample string moved, to `"quantum-annealer"`:

| File | What it asserts (unchanged) |
|---|---|
| `pkg/domain/domain_test.go:26` | unknown kinds do not validate |
| `pkg/domain/composite_test.go:159,162` | a composite cannot be built for an unknown kind |
| `pkg/domain/registry_test.go:112` | `Register` rejects an unknown kind |
| `pkg/domain/report_test.go:238` | `Report.Validate` rejects an unknown kind |
| `cmd/kilter/domains_test.go:467` | `--domain` rejects an unknown domain |

One further count moved: `TestNoDomainCanPlanAStepInThisBuild` asserted
`len(plans) == 4`, "one per wired domain". There are five wired domains now, so
it reads `5`. The loop body — every plan refuses, none is actuatable, none has
steps — is untouched, and it is where the test's meaning lives. No expectation
was weakened; a count of wired domains cannot survive wiring a domain.

---

## 3. Report-only enforcement: what the core guarantees for `rds`

`pkg/rds` is structurally read-only — no mutating API appears anywhere in the
package, `Health` is unconditionally report-only, `PlanSteps` refuses
unconditionally. **None of that is why an RDS step cannot run**, and the
distinction matters now that `rds` is a public registry key anything in-process
can claim.

Three walls, in the order a step meets them, none of which a domain can write to:

1. **`Registry.PlanSteps` checks `Health` before the domain is consulted.** An
   honest report-only domain's `PlanSteps` is never called at all
   (`TestAnHonestRDSDomainIsAlsoRefused`).
2. **`validateSteps` checks the output.** A domain registered as `rds` that
   hands back a step labelled `ec2` — where a real actuator *is* wired — is
   stopped with `ErrWrongDomain`, and the victim's actuator is never reached.
3. **`Registry.Execute` routes through the actuator table.** There is no `rds`
   row and there cannot be one. `Execute` and `Revert` both return
   `ErrReportOnly`.

`TestAHostileRDSDomainCannotActuateOrBorrowAnotherDomainsActuator` builds a
domain that lies about all three and asserts each wall independently, including
that the test actually reached the domain's `PlanSteps` (an adversarial test
that never reaches the attack proves nothing).

Two seams the RDS wiring newly *depends* on were attacked as well, because both
are new attack surface:

- **`Refuser`.** This is the first domain whose entire output is refusals. A
  domain returning refusals pointed at another domain's targets would put words
  in that domain's mouth — a `not-gp2` line filed under `ec2` that `pkg/ebs`
  never said, in a report a human is asked to act on. The registry re-stamps
  the producing kind, and `TestAHostileDomainCannotAttributeFindingsToAnotherDomain`
  asserts the victim's row counts none of them.
- **`SetSavings`, bypassed.** A domain can assign `Net`/`Gross` directly and
  skip the clamp that makes `Net ≤ Gross` mechanical. Nothing in `Summarize`
  re-checks it, by design — `Report.Validate` is the gate, and the CLI calls it
  before printing. `TestAFabricatedSavingIsCaughtByReportValidate` pins that.

### 3.1 A real hole this unit found and closed: the usage-line seam

`cmd/`'s account-wide commitment baseline is built from `UsageLines(now,
ledger)` — a **domain-supplied output that lands in a structure every other
domain's net savings are computed against**. Until this unit nothing checked
it. That is the same shape as the hole j1-wire closed one level down: inputs
filtered, outputs trusted.

Three ways it goes wrong, all silent, all in the *overstating* direction:

- an **empty ID** never matches in `domain.Ledger`'s splice and is always
  appended, so two anonymous lines for one resource double-count the usage
  available to absorb a commitment;
- a **duplicate ID** from a second domain *replaces* the first domain's line
  rather than adding to it, so one domain silently rewrites another's
  contribution;
- a **non-positive or non-finite rate or quantity** prices real usage at
  nothing, which again makes a commitment look more absorbed than it is.

More apparent absorption means a larger claimed saving. `badBaselineLine`
(`cmd/kilter/domains.go`) drops such a line and records a collection warning —
dropping is the conservative direction, because less baseline usage means more
apparent stranding and a *smaller* claim.
`TestPoisonedUsageLinesNeverReachTheAccountWideBaseline` builds all five
variants; `TestTheShippedDomainsContributeCleanBaselineLines` is the control
that the gate is a no-op for every domain actually wired, so it is not silently
shrinking a real baseline.

---

## 4. Every adapter written, and the mismatch that forced it

### 4.1 `rdsFixtureFile` — because `rds.Fixture` has fields JSON cannot carry

`rds.Fixture` implements all three read seams and is exported precisely so
`cmd/` can test against a recorded account. It cannot be decoded from JSON
directly: it carries four `error` fields (`InstancesErr`, `ClustersErr`,
`TagsErr`, `MetricsErr`, `ReservationsErr`) and a `Calls` counter struct, none
of which have a JSON representation. Decoding straight into it would produce a
file format with five fields that silently cannot be set and one that pretends
to be input.

`rdsFixtureFile` carries the data half with explicit json tags, and turns the
two seam-absence cases into the booleans the §6.2 IAM table actually describes:
`noMetricsAPI` (no `cloudwatch:GetMetricData` ⇒ every instance refuses with
`no-metric-evidence`, which is a complete report) and `noCommitmentAPI` (no
`rds:DescribeReservedDBInstances` ⇒ net == gross, which under-claims).

### 4.2 `pkg/domain/rds.Domain.UsageLines` — because a domain must not build an account-wide view

`pkg/rds` ships `UsageLines` as a pure function over one instance and stops
there on purpose; `pkg/domain/ledger.go`'s argument is that a domain knows only
its own targets and must not be tempted to construct the account-wide picture.
The adapter projects a whole `Report` into baseline lines, contributing only
the assessments `pkg/rds` could **price**. An excluded instance (Aurora, a
cluster member, an unknown engine, `mode=off`) never reaches the price step, so
`CostKnown` is false and it contributes nothing — which is right, because a
zero-rate baseline line makes a Reserved DB Instance look like it is absorbing
usage that costs nothing.

### 4.3 `policyFile` — because the three policy configs have no json tags

None of `recommend.Config`, `plan.Config` or `decision.Config` carries json
tags, so decoding into them gives a file whose keys are Go field names and
whose durations are integer nanoseconds. Worse, **omitted fields decode as
zero**: a policy file where leaving out `cpuHeadroom` silently means "headroom
1.0" is a footgun with a business consequence. Every field in `policyFile` is
therefore a **pointer** overlaid onto the package defaults, so absent means
default and present means present. Unknown fields are rejected (a knob
misspelled and silently ignored produces a scorecard for a policy nobody ran),
and a bare number where a duration belongs is rejected by name because Go would
read it as nanoseconds and nobody ever meant that.

`enforceDecisionRefusals` is modelled as part of the **policy**, not the
scoring knobs, because it is pending production wiring rather than a yardstick.
That is what turns "should we wire the decision layer in?" into an A/B through
`Gate` instead of an opinion.

### 4.4 `basisFrom` — and the three things §2 said the wiring must get right

All three are implemented and each has a note in the code saying why:

1. **Fargate nodes are excluded** from `Groups`. A Fargate "node" is a
   single-pod VM billed per quantized pod; pricing it per node shape would
   inflate the fleet total and put the error *inside a term*. Excluding it moves
   the gap to the residual, where it is visible.
   `TestFargateIsExcludedFromTheCompositionAndLandsInTheResidual` asserts the
   residual is non-zero, every priced term is zero, and a note is attached.
2. **An empty fleet is `&CostBasis{At: t}`, not `nil`.** `nil` is returned only
   for a snapshot that does not exist.
3. **Namespace demand is *requested* capacity.** `clampedPodRequests` mirrors
   `pkg/plan`'s unexported `clampedRequests` exactly (negatives clamped to
   zero), duplicated rather than re-derived because §2 is explicit that a third
   definition of "requested capacity" is the thing to avoid, and `pkg/plan` is
   not this unit's to change.

### 4.5 `loadLedgerActions` — §1's projection, field for field

Both things §1 insists on are enforced and tested. `Applied` is exact: only
`Mode == "apply"` with `Done > 0`, and only `actuate.StatusDone` steps are
counted — `StatusDryRun` is deliberately excluded even though `actuate.Report`
counts it as done for its own purposes, because a preview moved no money and
attributing a cost change to it is the classic attribution lie with a plan
attached. `NodesAdded` stays 0 because no plan type provisions a node today.
`Finished` has no ledger field, so it is left zero and `pkg/explain` falls back
to `At`.

---

## 5. Bugs found while wiring

### 5.1 `Scorecard.OracleGapPct` is already scaled — the doc comment says otherwise

`scorecard.go:371` computes `round6(meanSorted(gapsAll) * 100)`, while the field's
doc comment states the unscaled ratio `(cost(outcome) − cost(oracle)) / cost(oracle)`.
The first draft of the renderer multiplied by 100 again and printed a
**9,445 % oracle gap** for a trace whose real gap is 94.5 %. Caught by
comparing the rendered output against `pkg/backtest/FINDINGS.md`'s published
goldens, which is the reason those goldens are worth publishing.
`TestOracleGapIsRenderedInTheUnitsTheScorecardUses` is the regression.

**Reported, not fixed** — the fix belongs in `pkg/backtest`, and it is a doc
comment, not the arithmetic: the field name says `Pct` and the value is a
percentage. `Tolerance.MaxOracleGapIncreasePct`'s "2 pts" default confirms the
percentage reading.

### 5.2 The half-open window has to be honoured on both sides of the seam

`explain.WhyCost` filters its timeline to `[From, To)`, matching
`pkg/evidence`'s convention. The first draft observed timeline points over the
**closed** interval, so a snapshot at exactly `--to` was recorded and then
ignored — the CLI's own "needs two observations" check passed while `WhyCost`
failed with a confusing message about one timeline point. Both sides now use
`[from, to)`, the usage text says `--to` is exclusive, and the error message
says so too.

A related correctness fix: the two `CostBasis` edges are the **first and last
snapshots that produced a timeline point in the window**, not the newest
snapshot on either side of the requested boundary. A composition describing
`t+24h` against a measurement taken at `t+12h` would push a real, explainable
fleet change into the residual and call it unexplained.

---

## 6. What is still NOT reachable, and why

### 6.1 The live RDS collector — blocked on `go.mod`

`pkg/rds/FINDINGS.md` §6.2 owes `cmd/` an SDK adapter over `*rds.Client` and
`*cloudwatch.Client`. It **cannot be written in this build**:
`github.com/aws/aws-sdk-go-v2/service/rds` and `.../service/cloudwatch` are not
in `go.mod`, and adding them is a `go.mod`/`go.sum` change this unit may not
make. (`go.mod` has `service/ec2`, `service/pricing` and `service/autoscaling`
only.)

What replaces it is not a stub. `--rds-fixture` drives the **real collector**:
`rds.NewCollector` over the recorded account, `Collect`, the real window clamp,
the real `GetMetricData` batching and ID routing, real pagination (the shipped
fixture uses `pageSize: 3`, so it spans three pages), real truncation. Every
line of `pkg/rds/collect.go` a live credential would exercise is exercised
here. What is missing is the field copy between an SDK struct and a struct with
the same field names — which §6.2 describes as "a field copy with no
interpretation in it".

**Next job:** add the two modules, then write ~200 lines of mechanical
adapter per seam. The IAM actions are `rds:DescribeDBInstances`,
`rds:DescribeDBClusters`, `rds:ListTagsForResource` (required — the
`kilter.dev/mode` guardrail is unreachable without it),
`cloudwatch:GetMetricData` and `rds:DescribeReservedDBInstances` (both
optional, both with a documented degraded report). Two conversions are already
done inside `pkg/rds` and **must not be redone**: reservation amortization
(`reservationFromRecord`) and deployment topology (`DBInstance.Deployment`).

### 6.2 `kilter backtest --cluster` — blocked on snapshot-history persistence

Refused by name, with the seam spelled out and a non-zero exit:

```
$ kilter backtest --cluster prod
kilter backtest: backtest --cluster prod: refused — snapshot history is not persisted.

pkg/store keeps only the LATEST snapshot per cluster (SaveSnapshot/LoadSnapshot
are keyed by cluster, not by time) …
What it needs: a time-keyed snapshot bucket in pkg/store — SaveSnapshotAt(snap)
and Snapshots(cluster, from, to) — bounded the way the rest of the substrate is,
plus an adapter implementing backtest.SnapshotSource.
```

**`pkg/store` was deliberately left untouched.** The additive interface was in
scope, and it was still the wrong thing to add: nothing writes to a time-keyed
bucket today (`pkg/api`'s ingest path is where a write would have to go, and
`pkg/api` is not this unit's), so the bucket would be dead code and
`backtest --cluster` would go from an honest refusal to a scorecard over an
empty history. The refusal is the better artefact until the write side lands.

The failure mode this refusal exists to prevent is worth stating plainly:
scoring the one snapshot that *does* exist would produce a `Scorecard` with the
same shape, the same field names and the same confident tone as a real one.
`regret $0.00` reads as "the policy is perfect", not as "nothing was replayed".
`TestBacktestLiveHistoryRefusesRatherThanScoringOneSnapshot` asserts no
scorecard is printed.

Storage note for whoever builds it, from §"Seams this unit needed": at a
5-minute cadence a 30-day window is 8,640 snapshots per cluster. Full snapshots
are far too large; the realistic shape is keyframe-plus-delta, or a reduced
replay snapshot carrying only what `recommend` and `plan` read.

### 6.3 The explain/why-cost API routes — blocked on `pkg/api`

`pkg/explain/FINDINGS.md` §4 asks for:

```
GET /api/v1/clusters/{id}/why-cost?from=&to=   → *explain.Attribution
GET /api/v1/clusters/{id}/explain?subject=     → *explain.Explanation
```

**Not built.** `pkg/api` is outside this unit's scope, and the routes cannot be
added from `cmd/` by wrapping `Brain.Handler()` either — which is worth
recording, because that wrap *looks* available and is not:

- **`api.Brain` holds no evidence substrate.** `grep 'evidence\.' pkg/api/brain.go`
  returns nothing. Both entry points need one — `WhyCost` needs
  `Store.Timeline`, `BuildExplain` reads the substrate through the dossier
  builder — and, critically, `Verify` needs *the same store that produced the
  answer* to re-serve every citation. A route served from a store the brain
  never populated would 500 on every request, which is worse than no route.
- **Brain's internals are unexported.** `Handler()`, `Ledger()`, `Plan()`,
  `Recommendations()` and `Clusters()` are the whole surface; the recommender,
  the last snapshot and the pricing catalog are not reachable.

**Next job**, in order: give `api.Brain` an `*evidence.Memory` and populate it
from `Ingest` (samples, a timeline point per snapshot, and the deploy/OOM
events the collectors already carry); persist it through
`evidence.Memory.MarshalCheckpoint`/`FromCheckpoint` into `pkg/store`; then add
the two routes, projecting `LedgerEntry` with the mapping in
`cmd/kilter/explain.go`'s `loadLedgerActions` (which is that projection, written
and tested — lift it rather than re-derive it) and calling `Verify` before
serving. That evidence store is the same prerequisite §6.2 needs for its write
side, so the two jobs share most of their work.

### 6.4 Smaller gaps

- **`kilter explain` fills `Rec` and leaves `Verdict` nil.** `pkg/recommend`
  does not import `pkg/decision` (`pkg/backtest`'s finding, still true), so
  there is no verdict to read out of the production path. The payload's
  `Action` is therefore `unknown` and `Refusal` is nil. Filling them means
  either the `Recommender.Verdicts(snap)` seam `pkg/backtest` asked for, or
  evaluating `decision.Evaluate` at a call site production does not have —
  which would be a *different* answer from the one production gives, so it is
  deliberately not done.
- **`observeUsage` leaves throttle, restart and OOM at zero.** `model.Usage`
  carries CPU, memory and a window and nothing else. Those signals reach the
  substrate as evidence events from other collectors; inventing a zero throttle
  ratio would be a claim nobody measured, and "signal absent" is a state
  `pkg/decision` already knows how to read.
- **`why-cost` prices Fargate into the residual.** `pkg/pricing.SnapshotCost`
  includes Fargate pods; the composition excludes Fargate nodes, so the
  difference is reported as residual with a note. Correct and coarse; the
  natural extension is §"Deliberately deferred"'s second `CostBasis` dimension
  (`Charges []Charge`) decomposed by the same chain.
- **No `--policy` for `kilter domains`, no `kilter rds` verb.** RDS is reached
  through `kilter domains --domain rds`, like every other domain.
- **`pkg/rds`'s `StorageParity` seam is still nil**, as U11 shipped it —
  `--rds-fixture` cannot enable it and no flag pretends to. U13 owns it.

---

## 7. Determinism

- **Sort before summing money, proved by shuffle.**
  `TestRDSOutputIsShuffleInvariantAndByteIdentical` permutes the recorded
  account's instance pages and cluster list three ways and requires a
  byte-identical report, then repeats the same run eight times *in one process*
  (Go randomizes map iteration on every `range`, so in-process repetition is the
  real test). `TestWhyCostIsOrderAndRepeatIndependent` does the same for
  snapshot order on the command line.
  `TestBacktestOutputIsByteIdenticalAcrossRuns` covers the scorecard, text and
  JSON, on a noisy trace.
- **No map iteration reaches output.** `basisFrom` sorts its node groups and
  namespaces before appending — they are summed downstream. `writeScorecard`
  sorts the refusal codes. `loadLedgerActions` sorts by `(At, Fingerprint)`,
  because a ledger read in file order would make the attribution depend on the
  order entries happened to be appended.
- **No clock in any decision.** `--now` in `kilter domains`;
  `backtestEpoch` is a package constant, because a replay window that drifts
  with wall-clock time makes two runs over the same configuration disagree;
  `--from`/`--to` are required for `why-cost` and resolved to concrete
  timestamps before anything is computed for `explain`, then echoed in the
  output.
- **`loadSnapshotSeries` rejects duplicate timestamps**, for the reason
  `pkg/backtest` rejects them: a tie has no defined replay order.
- **No live AWS or cluster call anywhere**, including tests. `--rds-fixture`
  replays a recorded account through the real seams; `--kube-snapshot` reads
  recorded cluster snapshots; `--demo` generates a trace. No credential is read
  and `~/.aws` is never opened.

## 8. Fixtures

`cmd/kilter/testdata/rds-account.json` (39 KB) is generated by
`cmd/kilter/rdsfixture_test.go` and committed, and `TestWriteRDSFixture` asserts
the committed bytes still match the generator — the same contract-test idiom
`TestWriteDomainFixtures` already uses, so a diff means `pkg/rds`'s collector
changed and a reviewer should look.

```
go test ./cmd/kilter -run TestWriteRDSFixture -update-fixtures
```

Each of the seven instances is the interesting case, not the easy one:

| Instance | Why it is there |
|---|---|
| `db-pg-primary` | PostgreSQL, 500 GiB gp2, autoscaling on, 80 % never used — trap 9 (page cache) and trap 8 (a floor with a dollar and no API) |
| `db-mysql-multiaz` | the same-shaped `FreeableMemory` series on MySQL, where it IS readable and the downsize is still refused — the trap nothing else in the tree catches |
| `db-pg-replica` | zero connections across the whole window: the one replica finding safe to state |
| `db-mssql` | SQL Server EE license-included — refused **by name** rather than quoted an open-source rate |
| `db-aurora` | Aurora, refused by name (trap 16) |
| `db-mysql-cluster` | a Multi-AZ DB **cluster** member on MySQL, refused under its own name and not Aurora's (§5.3 — the reason `DescribeDBClusters` is read at all) |
| `db-legacy` | `kilter.dev/mode=off` — the guardrail, reachable only because `ListTagsForResource` is wired |

The metric cadence is deliberately coarse (one point per 6 hours). Every
evidence gate in `pkg/rds` is about window **span** and delivery completeness,
not sample count, so a dense series would add a megabyte of JSON and prove
nothing extra. The `why-cost` and `explain` fixtures are generated into
`t.TempDir()` rather than committed, for the same reason in reverse: they are
small, and the domain fixtures already carry the contract-test role.
