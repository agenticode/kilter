# U11 — `pkg/rds`: read-only observation, and a report whose refusals are the product

`pkg/rds` implements `docs/design/rds-batch-assessment.md` §5 **U11**: three read
seams shaped after the RDS and CloudWatch APIs, a recorded `Fixture`, an
engine-keyed memory policy, deployment-topology pricing, an embedded RDS rate
table in the `fargate.json` style, and a `Report` in which the refusals are the
finding rather than a caveat on one.

Everything below is green under `gofmt -l`, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/rds/...` and `go test -race -count=1 ./...`
(32 packages, all `ok`). `go.mod` and `go.sum` are **untouched**:
`go list -deps ./pkg/rds` is stdlib plus intra-repo packages only — no AWS SDK,
no client-go, no third-party module at all.

**4,980 lines of production Go, 3,359 of tests, 56 tests, 3 fuzz targets,
24 reason codes and 7 advisory codes** — inside §1.6's calibrated band for a
new CloudWatch-backed AWS domain (3,000–4,100 prod / 2,000–2,800 test /
53–65 tests / 13–20 codes), at the top of it, which §1.6 predicted RDS would be.

---

## 1. What is here

| File | Role |
|---|---|
| `rds.go` | Types, constants, the 24 reason codes and 7 advisory codes, `Window`/`ClampWindow`, `Series` statistics, `DBInstance`, `Snapshot`, `SpecFor`. |
| `engine.go` | **Trap 9.** Engine identity (`ParseEngine`) and the engine-keyed memory policy (`MemorySemantics`, `AssessMemory`). |
| `rates.go` | **Trap 10.** The embedded rate table, `RateProvenance`, `PriceBand`, `RateCard.HourlyUSD` (the price *function*, with the deployment multiplier in it), `LoadRates`/`Merge`, class shapes. |
| `storage.go` | **Trap 8.** `StorageVerdict`, `AssessStorage`, the documented autoscaling trigger, `AttributeStorageGrowth`. |
| `collect.go` | The three read seams and the `Collector`: `DescribeDBInstances`/`DescribeDBClusters`/`ListTagsForResource`, `GetMetricData`, `DescribeReservedDBInstances`. |
| `sizer.go` | The decision path. `Config`, `Assessment`, `Proposal`, the exclusion/refusal ordering, the commitment arithmetic, and the reserved `StorageParity` seam for U13. |
| `report.go` | `Report`, `Totals`, `Validate` (the gate), `Refusals()` projection, `WriteText`. |
| `domain.go` | The `domain.Domain` + `domain.Refuser` implementation: `Learn`/`Observe`, `Recommend`, `Refusals`, `PlanSteps` (unconditional refusal), `Health`, `Checkpoint`/`Restore`, `Snapshot.Generic`. |
| `fixture.go` | Recorded fixtures replaying all three seams with real pagination, real truncation and real empty-account behaviour. |

**Not here, and reserved:** `parity.go`/`parity_test.go` (U13) and
`actuate.go`/`actuate_test.go` (U14). U11 leaves exactly one documented,
currently-unreachable call site for U13 — the `StorageParity` interface in
`sizer.go` and the `if s.cfg.Parity != nil` branch in `Sizer.assessTarget`.
`TestStoragePerformanceIsRefusedNotBorrowed` pins that the shipped default
leaves it nil.

## 2. Why this domain proposes nothing

`Report.Totals.Proposals` is 0 in every report this unit can produce, and that
is the deliverable rather than an omission. Four independent facts each close
the door on a different half of RDS:

| What is expensive | Why it is not proposed |
|---|---|
| The DB instance class | Changing it is a **failover**: *"The RDS instance was modified by customer — An RDS DB instance modification triggered a failover"*; *"Failover times are typically 60–120 seconds"*; *"The failover mechanism automatically changes the Domain Name System (DNS) record … you need to re-establish any existing connections"* [all verified]. `domain.ActionClass` has four members and none describes it — `ActionStopStart` would budget it like an EC2 restart, which returns the same endpoint. `pkg/domain`'s own comment says a domain "must never understate it". |
| Allocated storage | *"You can't reduce the amount of storage for a DB instance after storage has been allocated"* [verified]. There is no API. |
| Memory headroom | `FreeableMemory` is `MemAvailable`, which counts reclaimable page cache as available (§4 below). |
| Multi-AZ | Halving the bill halves the SLA. An availability posture, not a cost setting. |

So the class is **unrepresentable**, not merely forbidden: `Proposal` has no
field naming a class, a topology, an engine version or a deletion, and
`TestProposalCannotNameAnInstanceClass` asserts that by reflection. This is the
`pkg/ecs` precedent — *"§3.4's 'never change desired count' is unrepresentable
rather than merely forbidden"*.

What the domain produces instead is a priced refusal per instance: what it
costs, what was observed, and the exact measurement or API that does not exist.

## 3. Every reason code, and exactly where it is tested

Exclusions fire **alone** — an instance this domain does not model is not also
told things in a vocabulary that does not describe it. `Report.Validate`
enforces that, and `TestValidateCatchesEachViolation/exclusion does not fire
alone` proves the enforcement.

| Code | Kind | Tested by |
|---|---|---|
| `aurora-not-supported` | exclusion | `TestAuroraIsRefusedByName` (trap 16) |
| `cluster-member-not-supported` | exclusion | `TestAuroraIsRefusedByName`, `TestOptionalSeamsDegradeRatherThanBreak` |
| `guardrail-mode-off` | exclusion | `TestModeOffExcludesEverything` |
| `unknown-engine` | exclusion | `TestUnknownEngineOrClassRefusesByName` |
| `unknown-deployment` | exclusion | `TestDeploymentMultiplierMatchesTheReservationTable`, `FuzzRDSPriceFunction` |
| `unknown-instance-class` | refusal | `TestUnknownEngineOrClassRefusesByName` |
| `engine-not-priced` | refusal | `TestUnknownEngineOrClassRefusesByName`, `TestPriceBandSeparatesLicensedEngines` |
| `unverified-rate` | refusal | `TestUnverifiedRatesNeverBecomeASaving`, `TestEveryShippedRateIsUnverified` |
| `instance-class-change-is-a-failover` | refusal | `TestEveryAssessmentStatesAReason` |
| `freeable-memory-is-page-cache` | refusal | `TestFreeableMemoryIsNotHeadroom` (trap 9) |
| `buffer-pool-scales-with-class` | refusal | `TestFreeableMemoryIsNotHeadroom` (trap 9) |
| `engine-memory-semantics-unencoded` | refusal | `TestFreeableMemoryIsNotHeadroom`, `TestMemorySemanticsDefaultToRefusal` |
| `allocated-storage-cannot-shrink` | refusal | `TestStorageFloorNeverProposesShrink` (trap 8) |
| `storage-autoscaling-ratchet` | refusal | `TestAutoscaledStorageIsAFloorNotAChoice` (trap 8) |
| `replica-is-failover-capacity` | refusal | `TestIdleInstanceIsReportedNeverResized` |
| `multi-az-is-availability-posture` | refusal | `TestMultiAZBillsTwice` |
| `insufficient-window` | refusal | `TestShortWindowIsRefusedNotAbsorbed` |
| `no-metric-evidence` | refusal | `TestTruncatedMetricsNeverProduceAnIdleVerdict` |
| `truncated-metric-response` | refusal | `TestTruncatedMetricsNeverProduceAnIdleVerdict` |
| `size-flexibility-excluded` | refusal | `TestSQLServerAndOracleLIAreExactMatchOnly` (trap 13) |
| `instance-state-unstable` | refusal | `TestEveryAssessmentStatesAReason` |
| `no-storage-performance-model` | refusal | `TestStoragePerformanceIsRefusedNotBorrowed` (trap 11) |
| `commitment-negative` / `commitment-neutral` | refusal | re-exported from the U12 waterfall; `TestSQLServerAndOracleLIAreExactMatchOnly` |

Advisories (`idle-instance`, `idle-read-replica`, `allocated-storage-floor`,
`storage-autoscaling-enabled`, `multi-az-doubles-the-instance-line`,
`unverified-rate-magnitude`, `reservation-would-strand`) each carry a mandatory
`Caveat` — `Report.Validate` rejects one without it, because an advisory
stripped of its caveat reads as an actionable saving.

`TestReasonCodesAreDistinct` pins that no two codes share a value, which would
silently merge two findings in every roll-up.

## 4. Trap 9 in detail, because it is the one nothing else in the tree catches

> `FreeableMemory` — The amount of available random access memory. **For
> MariaDB, MySQL, Oracle, and PostgreSQL DB instances, this metric reports the
> value of the `MemAvailable` field of `/proc/meminfo`.** [verified]

§7 trap 4 (memory-blind EC2 downsizing) does not catch this. There the signal is
*absent* and a refusal fires. Here the signal is *present* and
engine-dependently misleading, so `pkg/ec2`'s "never propose less memory
without a memory signal" rule is satisfied on paper while being violated in
spirit. **Nothing would fire.**

The policy is a three-row table with a refusing default:

| Engine | Semantics | Verdict |
|---|---|---|
| PostgreSQL | `page-cache-dominant` | `Readable = false`. The series is **not converted into a headroom number at all** — `MinFreeBytes` and `FreeFraction` stay zero, because a populated field named after headroom is an invitation to use it. |
| MySQL, MariaDB | `anonymous-buffer-pool` | `Readable = true`, low-water mark reported — and the downsize still refused, because the buffer pool is a fraction of instance memory, so a smaller class means more disk I/O and more CPU. |
| everything else | `unencoded` | Refuses. The `t2`-baseline precedent: *"Rather than guess, `burstFamilies` recognizes t2 as credit-based while `burstBaselines` has no row for it, so every t2 instance lands in `unknown` and is refused"* (`pkg/ec2/FINDINGS.md` §5). |

`TestFreeableMemoryIsNotHeadroom` drives one fixture through three engines,
asserts point-for-point that the `FreeableMemory` series is **identical**, and
asserts the verdicts **differ**. Oracle is deliberately absent from the encoded
table even though `rds-metrics.html` names it in the `MemAvailable` sentence:
knowing which `/proc` field the metric reports is not the same as knowing what
the SGA and PGA do with it.

## 5. Decisions a reviewer should know about

**5.1 Every shipped rate is `unverified`, and that is a refusal rather than a
caveat.** §7 records that RDS on-demand rates, the Multi-AZ price ratio and the
gp2/gp3 `$/GiB-month` figures could not be retrieved, and that the `$0.115` gp2
figure appears only in an example AWS itself labels *"sample prices"*. So
`ClassRate` carries a `RateProvenance`, an unverified rate may **size a
reported fact** and may **never become a claimed saving**, and
`Report.Validate` rejects a proposal that tries. `TestEveryShippedRateIsUnverified`
stops a future edit from quietly promoting a row. The intended path to a number
Kilter will stand behind is `LoadRates`, which stamps every row `operator-supplied`.
A **mixed** card — verified class rates over the shipped `$/GiB-month` — still
refuses, because `Assessment.WorstRateProvenance()` takes the weakest rate
behind *any* dollar on the target.

**5.2 The three open-source engines share one price band.** That is a *modelled
equivalence*, stated in `PriceBand` rather than hidden in a table so it can be
disagreed with in one place. It is contained because the whole band is
unverified and therefore cannot produce a claim. SQL Server, Oracle and Db2 ship
**no rows at all** and are refused by name with `engine-not-priced` — §2.8's
honest v1, *"ship the engines whose rates we have, refuse the rest by name"*.

**5.3 A Multi-AZ DB cluster member is refused under its own name, not
Aurora's.** The design doc says to detect Aurora by *"`Engine` prefix `aurora-`,
or a non-empty `DBClusterIdentifier`"*. Taken literally that labels a PostgreSQL
Multi-AZ DB cluster "Aurora", which would be a false statement in a report whose
entire value is that its statements are true. So `DescribeDBClusters` is read
(a third inventory operation) and the *cluster's* engine decides:
`aurora-not-supported` when it is Aurora, `cluster-member-not-supported`
otherwise. Both are exclusions, so the outcome the doc asked for — refuse, zero
proposals — is unchanged. If `DescribeDBClusters` fails, the member still
excludes, under the more cautious name, and the report says it could not tell
which kind of cluster it was.

**5.4 The Db2 reservation identity carries no licence marker, deliberately.**
`Engine.Licensed()` (the *price* axis) includes Db2; `licenceMarkedInReservations()`
(the *reservation-matching* axis) does not. AWS's reserved-DB-instance page names
licence models for SQL Server and Oracle only, and `commit.NormalizeRDSEngine`
folds exactly those spellings. Appending `(byol)` to a Db2 usage line would stop
it matching a Db2 reservation whose product description carries none, turning
every Db2 reservation into apparent stranding. Both sides are built from the
same unverified spelling instead, which neither over- nor under-matches.

**5.5 `DatabaseConnections` uses `Maximum`, `FreeableMemory` and
`FreeStorageSpace` use `Minimum`.** Each statistic is chosen for the direction
the verdict must be safe in: the idle test must fail if *any* point was
non-zero, and the storage/memory floors are set by the worst moment, not the
average. The idle verdict additionally requires CPU below `IdleCPUPercent`,
because non-zero CPU on a connection-less database is real work — autovacuum,
replication apply, backups — and calling it idle would be false.

**5.6 An open question this unit implemented to spec and did not resolve.**
§4 trap 10 says the Multi-AZ multiplier *"applies to the instance hours and
**not** to storage, backups or I/O"*, reasoning from the reservation discount's
line separation. `RateCard.StorageMonthlyUSD` therefore does **not** apply the
multiplier, and `TestMultiAZBillsTwice` asserts the storage line does not move.
Whether AWS bills Multi-AZ *storage* at 1× or 2× was not verifiable from the
cited pages. If it is 2×, this domain under-states Multi-AZ storage cost by
half — the conservative direction, and a one-line change in
`StorageMonthlyUSD`. **This is the single highest-value thing to verify next.**

**5.7 The window is clamped at construction, not at query time.** A snapshot
that claims a 45-day window and contains 15 days of data is a lie told by
omission, and every downstream "insufficient window" gate reads the claim
rather than the data. `NewCollector` clamps and warns; `Collector.Window()`
returns what will actually be observed.

**5.8 The fifth copy of the CloudWatch seam.** §1.4 counts four independent
derivations of `MetricDataQuery`/`MetricDataResult`/`GetMetricDataInput`/`MetricsAPI`
and says *"If a third domain needs it, lift a `pkg/cloudwatch` seam then"* —
a threshold now crossed three times. This unit adds the fifth copy rather than
lifting the seam, because lifting it edits shared packages U11 may not touch.
The four truths every copy has had to re-derive are asserted here, and they are
the specification a `pkg/cloudwatch` would have to satisfy:

| Truth | Test |
|---|---|
| Batch at ≤ 500 series per call | `TestCollectorBatchesWithinTheGetMetricDataLimit` |
| Route results by query **ID**, never by position | `TestCollectorRoutesByQueryIDAndMarksMissingResultsTruncated` |
| A *missing* result is truncation, not "no usage" | same test, plus `TestTruncatedMetricsNeverProduceAnIdleVerdict` |
| Clamp the window to retention / publication granularity | `TestCollectorClampsWindowTo15DayRetention` |

RDS adds a fifth that the other four did not need: a **per-metric publication
floor**. Most RDS metrics publish at 60 s, but *"CPU credit metrics are
available at a five-minute frequency only"*, so `metricSpec.minPeriodSeconds`
raises those queries to 300 s and the delivered `Series.PeriodSeconds` records
what was actually asked for.

## 6. Exact wiring a later unit must do — this is what `cmd/` needs

U11 deliberately does **no** wiring. Everything below is owed by whoever wires
the binary.

### 6.1 `pkg/domain` must learn the RDS kind — one constant and one slice entry

`domain.Kind` is a closed set (`pkg/domain/domain.go`) and `Registry.Register`
rejects anything outside it. `pkg/rds` declares `const Kind = domain.Kind("rds")`
and therefore **cannot be registered today**. The change is two lines in
`pkg/domain/domain.go`:

```go
RDS Kind = "rds" // in the const block beside EC2, ECSFargate, K8sFargate, K8sNodes, Lambda
var kinds = []Kind{EC2, ECSFargate, K8sFargate, K8sNodes, Lambda, RDS} // canonical order
```

This is the same shape of core edit U12 made to `pkg/pricing/commit` and, like
that one, it is the only edit needed. `TestKindIsHonestAboutRegistration` passes
**both before and after** it lands: it asserts the property that must hold
either way (registered ⇒ the core still refuses to plan steps; not registered ⇒
`Register` fails with `unknown kind` and the standalone `Report` path works),
so nobody has to remember to update a test when they make the change.

Until it lands, `cmd/` can drive the domain directly through `Report()` /
`Refusals()`, which is the whole output anyway.

### 6.2 The SDK adapter — three seams, five read operations

`cmd/` must implement three interfaces over `*rds.Client` and
`*cloudwatch.Client`. No SDK type may enter `pkg/rds`.

| Interface | Operation | AWS API | IAM action |
|---|---|---|---|
| `rds.InventoryAPI` | `DescribeDBInstances` | `rds:DescribeDBInstances` | required |
| | `DescribeDBClusters` | `rds:DescribeDBClusters` | required (see §5.3) |
| | `ListTagsForResource` | `rds:ListTagsForResource` | required for the `kilter.dev/mode` guardrail |
| `rds.MetricsAPI` | `GetMetricData` | `cloudwatch:GetMetricData` | optional; nil ⇒ every instance refuses with `no-metric-evidence` |
| `rds.CommitmentAPI` | `DescribeReservedDBInstances` | `rds:DescribeReservedDBInstances` | optional; nil ⇒ net == gross, which under-claims |

Field mapping is one-to-one with the SDK's own names
(`DBInstanceRecord`, `DBClusterRecord`, `ReservedDBInstanceRecord`), so the
adapter is a field copy with no interpretation in it. Two conversions are
already done **inside** this package and must not be redone in `cmd/`:

- Reservation amortization (`EffectiveHourly = UsagePrice + FixedPrice ÷ term
  hours`) and the `active`/`payment-pending` filter — `reservationFromRecord`.
- Deployment topology (`MultiAZ` → `commit.RDSMultiAZInstance`) —
  `DBInstance.Deployment`.

`rds.Fixture` implements all three interfaces, so the adapter can be tested
against a recorded account before it ever sees a credential.

### 6.3 The collection loop

```go
w := rds.Window{Start: now.Add(-14 * 24 * time.Hour), End: now}
cfg := rds.DefaultCollectorConfig(w)
cfg.Scope, cfg.Region = accountID+"/"+region, region
c, err := rds.NewCollector(invAdapter, cwAdapter, resAdapter, cfg)
snap, err := c.Collect(ctx)          // rds.Snapshot
```

Note `c.Window()` may be **shorter** than the window passed in — 1-minute
CloudWatch datapoints live 15 days. Render `c.Window()`, not the request.

### 6.4 The brain side

```go
d, err := rds.NewDomain(rds.Config{Scope: scope, Region: region, Rates: card})
d.Observe(snap)                       // native path, lossless
rep := d.Report(now, ledger)          // the product
refusals := rep.Refusals()            // []domain.Refusal for the aggregate report
rep.WriteText(os.Stdout)
```

Or, over the generic seam, `d.Learn(snap.Generic())` — which carries the native
snapshot in `domain.Snapshot.Payload` and is equally lossless
(`TestGenericSnapshotIsLosslessThroughPayload`). **Do not** hand this domain a
generic snapshot with `Payload` stripped: `domain.Sample` has no truncation
flag, so every series rebuilt from samples is marked partial and no idle verdict
can fire. That is honest, and it is a much weaker report.

`Recommend()` returns an **empty slice**, always. Render `Refusals()` — for this
domain that is not a debug view, it is the answer.

### 6.5 The ledger

RDS lines are `commit.KindRDS` and are absorbed by `commit.ReservedDBInstance`
and by nothing else. Build the account-wide baseline with `rds.UsageLines(...)`
per instance and splice it into whatever `pkg/ec2` and the other domains
contribute, then `domain.NewLedger(inv, baseline)`. Passing a **partial**
baseline is safe but pessimistic; passing none gives net == gross.

`snap.Reservations` is already `[]commit.ReservedDBInstance` and can go straight
into `commit.Inventory.ReservedDBs`. `snap.Generic()` also attaches it to
`domain.Snapshot.Commitments`.

### 6.6 Rates

Ship `rds.DefaultRates()` and expose a `--rds-rates <file>` flag wired to
`rds.LoadRatesFile`. Until an operator supplies one, every RDS dollar in the
report is marked `unverified` and no saving is claimable from it — which is the
intended, loud failure mode, not a bug to route around.

The override format (unknown fields rejected):

```json
{
  "region": "us-east-1",
  "classes": {
    "open-source|db.r6i.xlarge":     {"singleAZHourlyUSD": 0.48},
    "sqlserver-ee-li|db.r6i.xlarge": {"singleAZHourlyUSD": 3.20}
  },
  "storage": {"gp2GiBMonthUSD": 0.115, "gp3GiBMonthUSD": 0.115}
}
```

There is deliberately **no** `multiAZHourlyUSD` column and an override that adds
one fails to parse: the ×1/×2/×3 multiplier is a property of the price function
(trap 10), and a rate file must not be able to contradict it.

Use `rds.DefaultRates().Merge(loaded)` to add the licensed-engine rows without
restating the open-source ones.

## 7. Deliberately deferred

**7.1 Storage-performance parity is U13's, and is refused rather than
borrowed.** `pkg/ebs/parity.go`'s constants are wrong for RDS in all three ways
trap 11 names: RDS stripes across four volumes at 400 GiB (200 GiB for Oracle,
never for SQL Server), gp2 burst reaches 12,000 IOPS rather than 3,000 and
throughput 1,000 MiB/s rather than 250, and gp3 cannot be provisioned *at all*
below the striping threshold. Reusing them would claim a saving in the band
where RDS loses throughput and refuse in the band where it converts cleanly. So
every instance carries `no-storage-performance-model` and the `StorageParity`
seam waits, typed and reviewed, in `sizer.go`.

**7.2 Aurora.** Refused by name, per trap 16. The refusal already names what an
Aurora unit would have to start from — min/max ACU and the I/O-Optimized cluster
mode — and `ClusterInfo.ServerlessV2MinACU` is collected so that unit has the
number waiting.

**7.3 No confidence model.** The three shipped domains each wrote their own
(`pkg/ec2/sizer.go:827`, `pkg/ecs/sizer.go:646`, `pkg/lambda/sizer.go:737`).
This one proposes nothing, so there is nothing to be confident *about*;
`Proposal.Confidence` exists and is validated for U13's sake. When U13 lands, the
"earned, not lost" structure plus `weakestFactor()` from `pkg/ec2`/`pkg/lambda`
is the shape to copy — and it is the fourth copy, which is its own argument for
lifting it.

**7.4 No histogram, forecast, patterns, recommend or safety.** Consistent with
all four shipped cloud domains (§1.3): zero of them import those packages. A
stateless read-only report has nothing to decay and nothing to forecast. If a
later unit wants trend detection over the allocated-storage ratchet — which is
the one genuinely time-series-shaped RDS finding — that is a `Learn`/`Checkpoint`
change, and `Target.PriorAllocatedStorageGiB` plus `AttributeStorageGrowth` is
the seam it would grow from.

**7.5 `DescribeValidDBInstanceModifications` is not read.** §2.4 says the
provisioning envelope must come from it rather than from AWS's two
contradictory published SQL Server gp3 ceilings (80,000 vs 64,000/16,000). It
belongs to U13, which is the first unit with anything to provision.

**7.6 One region per collector.** Same as every shipped domain. Multi-region is
`cmd/` running one collector per region and merging reports.

## 8. Things that would falsify parts of this unit

Stated as evidence, in the style §6 of the design doc uses.

1. **If AWS bills Multi-AZ storage at 2×**, §5.6's implementation under-states
   Multi-AZ storage cost by half. One line in `RateCard.StorageMonthlyUSD`.
2. **If `DescribeReservedDBInstances` returns a Db2 product description that
   carries a licence marker**, §5.4's decision inverts and
   `licenceMarkedInReservations` should include Db2.
3. **If AWS publishes distinct MariaDB / MySQL / PostgreSQL rates that differ**,
   §5.2's shared `open-source` band should split into three. The band key is a
   single function, and the whole table is unverified today, so nothing built on
   it has to move.
4. **If `pkg/domain` grows a fifth `ActionClass` that can honestly represent a
   failover** — DNS change, pooled-connection reset, client-side TTL
   unobservable — then §2's central argument for declining instance-class
   actuation weakens, and U10's "decline" verdict is worth revisiting. Note that
   the *availability* arguments (replica promotion capacity, Multi-AZ posture)
   and the *evidence* argument (trap 9) survive it independently.

---

# U13 — storage-performance parity: the tables are the deliverable

`pkg/rds/parity.go`, `parity_envelope.go`, `parity_assess.go` implement
`docs/design/rds-batch-assessment.md` §5 **U13**: the engine-keyed gp2/gp3
regime tables, the 400 / 200 / never striping thresholds, measured-parity
conversion, provisioned IOPS and throughput reduction toward the non-reducible
baseline, the refusal band, and the provisioning envelope read live from
`rds:DescribeValidDBInstanceModifications`.

**2,008 production lines, 1,596 test lines, 18 tests, 1 fuzz target, 13 new
reason codes.** Green under `gofmt -l ./pkg/rds`, `go vet ./...`,
`go build ./...`, `go test -race -count=1 ./pkg/rds/...` and
`go test -race -short ./...` (34 packages, all `ok`). `go.mod` and `go.sum` are
**untouched**; the only non-stdlib import anywhere in the unit is intra-repo
(`pkg/domain` in production, `pkg/ebs` in the trap-11 test).

**`sizer.go` was not edited.** U11 typed the seam correctly: `StorageParity`
takes exactly the four arguments this unit needs, the `if s.cfg.Parity != nil`
branch is the whole wiring, and `DefaultConfig().Parity` is still nil — so
`TestStoragePerformanceIsRefusedNotBorrowed` passes unchanged and the parity
engine is opt-in. `AssessParity` always returns `ok=true` and at least one
suppression, which is what makes the reserved `else` branch unnecessary rather
than merely unused.

## 1. What is here

| File | Role |
|---|---|
| `parity.go` | The tables. Striping thresholds, the published gp2 band table, the gp3 regimes, `GP3Config` + its `Validate` gate, `Demand`, `PerformanceRates`, and `SumUSD`. Pure: no clock, no I/O, no package-level `var` at all. |
| `parity_envelope.go` | The fourth read seam. `ModificationEnvelopeAPI` (`DescribeValidDBInstanceModifications` + `DescribeEvents`), `Envelope`/`Envelopes`, `CooldownVerdict`, `EnvelopeCollector`, and `EnvelopeFixture`. |
| `parity_assess.go` | The decision path. `Parity` (the `StorageParity` implementation), `MeasureIO`, `PlanParity`, `ParityPlan`, and the earned-not-lost `ParityConfidence`. |

There is **no** `actuate.go`. U14 owns it, and §5 below is what it inherits.

## 2. The tables, and where each number came from

Every table below is transcribed a **second** time, by hand, in
`TestRDSGP2TableMatchesPublishedBands`, `TestStripingThresholdIsEngineDependent`
and `TestGP3IsNotProvisionableBelowTheThreshold` — the
`TestRDSNormalizationTableMatchesPublishedUnits` discipline, because a test that
reads the constant it is checking proves nothing.

### 2.1 Striping thresholds [verified: CHAP_Storage.html, via §2.4]

| Engine | Threshold | Encoded as |
|---|---|---|
| Db2, MariaDB, MySQL, PostgreSQL | 400 GiB | `StripingThresholdGiB` |
| Oracle | 200 GiB | `StripingThresholdOracleGiB` |
| SQL Server | never (one volume) | `NeverStripes` = −1 |

`NeverStripes` is −1 rather than 0 deliberately: zero would read as "threshold
of zero", which puts every SQL Server instance in the *striped* regime — the
exact inversion of the published fact.

### 2.2 The gp3 regimes [verified, same page]

| | below threshold | at/above threshold | SQL Server, any size |
|---|---|---|---|
| baseline | 3,000 IOPS / 125 MiB/s | 12,000 / 500 | 3,000 / 125 |
| provisionable | **no** | yes | **yes** |

The `Provisionable` column is the load-bearing one and comes from one sentence:
*"For every DB engine except RDS for SQL Server, you can provision additional
IOPS and storage throughput when storage size is at or above the threshold
value."* SQL Server is the exception in both directions — it never stripes and
it can always provision — and that asymmetry is why a single "RDS gp3 baseline"
constant cannot exist.

### 2.3 The published gp2 band table [verified, same page]

Transcribed verbatim into `gp2Bands`:

| Engine / size | Baseline IOPS | Throughput | Burst |
|---|---|---|---|
| MariaDB/MySQL/PostgreSQL 5–399 GiB | 100–1,197 | 128–250 | 3,000 |
| … 400–1,335 GiB | 1,200–4,005 | 512–1,000 | 12,000 |
| … 1,336–3,999 GiB | 4,008–11,997 | 1,000 | 12,000 |
| … 4,000–65,536 GiB | 12,000–64,000 | 1,000 | N/A |
| SQL Server 334–999 GiB | 1,002–2,997 | 250 | 3,000 |

Three decisions inside that transcription, each of which a reviewer should be
able to disagree with in one place:

**(a) Baseline IOPS is interpolated at 3 per GiB inside a band; throughput is
not interpolated at all.** Every published IOPS endpoint is exactly 3× its size
edge (399×3 = 1,197; 400×3 = 1,200; 3,999×3 = 11,997; 4,000×3 = 12,000), which
is the corroboration that makes interpolation a reproduction of the table rather
than an extension of it. Throughput has no such structure — the table gives a
range per band and never says which size gets which number — so both endpoints
are carried and nothing between them is invented.

**(b) Parity is measured against the band's throughput MAXIMUM.** §2.4's own
falsifiable example reasons from the *floor* ("at least 512 MiB/s"), and the
floor is the right number for describing what a volume is guaranteed. It is the
wrong number for deciding what a conversion must preserve: the table permits the
volume to be delivering the top of its band, and provisioning below that is
exactly the silent degradation this unit exists to prevent. So
`GP2PerformanceRDS` carries `MinThroughputMBps` for the report and
`ParityThroughputMBps` (= the band maximum) for the arithmetic. **This is the
single most consequential judgement call in the unit** — it is what puts the
refusal band at 400–1,739 GiB rather than at 400–1,057 — and §7 below states
what would falsify it.

**(c) Oracle and Db2 have NO gp2 row, and neither does SQL Server outside
334–999 GiB, so they refuse.** They appear in the striping table and not in the
gp2 table. Borrowing MySQL's bands for an engine that stripes at 200 GiB is trap
11 committed against a second engine, so `ReasonParityGP2BandUnpublished` fires
by name. `TestRDSGP2TableMatchesPublishedBands` asserts the absence
structurally.

### 2.4 What is deliberately NOT transcribed

The two contradictory SQL Server gp3 ceilings (80,000 vs 64,000/16,000).
`TestProvisioningEnvelopeIsReadNeverHardcoded` asserts this **structurally**: it
re-parses every non-test file in the package and fails if the literal `80000` or
`16000` appears anywhere outside a comment. The ceiling comes from
`DescribeValidDBInstanceModifications` or the proposal is refused.

## 3. Trap 11, as a number

`TestRDSGP2ModelIsNotTheEBSModel` runs one 500 GiB MySQL volume through both
models and logs both answers:

```
500 GiB MySQL: pkg/ebs says 1500 baseline IOPS, 3000 burst IOPS, 250 MiB/s;
               pkg/rds says 1500 baseline IOPS, 12000 burst IOPS, 512–1000 MiB/s
```

The IOPS baselines agree — 3 IOPS/GiB is the one thing the two products share —
and everything that decides a dollar disagrees. The test then asserts the
consequence §2.4 states: converting lands on 12,000 / 500, so IOPS rises ~8×
and throughput **falls** below what gp2 guaranteed.

`TestThroughputParityRefusalBand` is the same fact at fleet scale. Sweeping
300–2,000 GiB of MySQL gp2 at the nameplate floor:

| size | pkg/ebs | pkg/rds |
|---|---|---|
| 334–375 GiB | refuses (`no-cheaper-config`) | refuses — `gp3-not-provisionable-below-striping-threshold` |
| 300–399 GiB | converts (outside its band) | refuses — throughput cannot be bought below 400 GiB **at any price** |
| **400–1,739 GiB** | converts | **refuses — `storage-parity-not-cheaper`** |
| 1,740+ GiB | converts | converts |

The two `no-cheaper-config` bands — 334–375 and 400–1,739 — are asserted
disjoint. At 1,000 GiB the test logs the trap in its own words: *pkg/ebs
converts at parity; RDS needs 12,000 IOPS / 1,000 MiB/s costing $132.00/mo
against gp2's $115.00/mo and refuses.* That is "claims a saving in the band
where RDS loses money", reproduced.

The band's edges are recomputed in the test from the rate arithmetic
(`0.115·G − (0.092·G + 500×0.08) > 0`) rather than read from the model, so a
change to either side fails.

## 4. Every refusal predicate, and where it is tested

A proposal is produced only when **all** of these hold. Each failure is a named
refusal; none is a silent skip.

| Code | Predicate | Tested by |
|---|---|---|
| `storage-type-not-modelled` | gp2 or gp3 | `TestParityRefusesWhatItDoesNotModel`, `TestParityReportIsShuffleInvariant` |
| `storage-size-unusable` | 1–65,536 GiB | `TestParityRefusesWhatItDoesNotModel`, `FuzzRDSParityNeverUnderProvisions` |
| `gp2-band-unpublished` | the engine/size has a published nameplate | `TestRDSGP2TableMatchesPublishedBands`, `TestParityRefusesWhatItDoesNotModel` |
| `gp3-not-provisionable-below-striping-threshold` | provisioning only at/above the threshold (or SQL Server) | `TestGP3IsNotProvisionableBelowTheThreshold`, `TestThroughputParityRefusalBand` |
| `provisioning-envelope-unknown` | `DescribeValidDBInstanceModifications` was read | `TestProvisioningEnvelopeIsReadNeverHardcoded` |
| `storage-demand-exceeds-envelope` | the config is inside the LIVE envelope | `TestProvisioningEnvelopeIsReadNeverHardcoded` |
| `storage-parity-not-cheaper` | strictly cheaper than today | `TestThroughputParityRefusalBand`, `TestShippedParityRatesCannotClaim` |
| `provisioned-performance-floors-at-baseline` | the tail above 12,000/500 is non-empty | `TestGP3ReductionFloorsAtTheBaseline` |
| `no-io-measurement` | all four I/O series delivered in full, and usable arithmetic | `TestNoMeasurementFallsBackToNameplateAndNeverToZero`, `TestParityRefusesWhatItDoesNotModel` |
| `io-window-too-short` | the series span clears `MinWindow` | `TestParityConfidenceIsEarnedNotLost` |
| `storage-optimization-blocks-modification` | not `storage-optimization` / `modifying` | `TestStorageOptimizationStateBlocks` |
| `storage-modification-cooldown` | fewer than four modifications in 24 h | `TestFourModificationsPer24HoursIsACooldown` |
| `storage-parity-low-confidence` | confidence ≥ 0.6, earned | `TestParityConfidenceIsEarnedNotLost` |
| `unverified-rate` (**U11's, reused**) | the rate behind the saving is claimable | `TestShippedParityRatesCannotClaim` |

`TestParityReasonCodesAreDistinct` pins all thirteen new codes against each
other **and against all twenty-two U11 codes**, because two codes with one value
silently merge two findings in every roll-up.

Two of these fire **alone**: `storage-optimization-blocks-modification` and
`storage-modification-cooldown`. A change AWS will reject is the whole finding,
and pricing it alongside would present an impossibility as an opportunity.

## 5. What U14 inherits — the typed surface it actuates against

This is the contract. Nothing below needs to be re-derived.

**5.1 The proposal.** `Assessment.Proposal` carries `StorageType` (always
`gp3`), `IOPS` and `StorageThroughputMBps` as **EFFECTIVE TOTALS**, and
`AllocatedStorageGiB` set to the *observed* allocation as the trap-8 ratchet
guard. Effective totals, not deltas, because `ModifyDBInstance` takes absolute
values. U14 decides whether to *send* `--iops` / `--storage-throughput` by
calling `GP3RegimeFor(engine, sizeGiB)`: a value equal to `BaselineIOPS` /
`BaselineThroughputMBps` is what the volume delivers for free and needs no API
argument, and below the threshold sending one at all is an error
(`GP3Config.Validate` states it).

**5.2 The envelope, re-read live.** `ModificationEnvelopeAPI` is the seam;
`EnvelopeCollector.Collect(ctx, identifiers)` returns an immutable, sorted
`Envelopes`. U14 must re-read it at execution time and re-validate with
`GP3Config.Validate(regime, env.For("gp3"))` — the envelope in a stored plan is
a snapshot, and an instance whose class changed between plan and apply has a
different envelope. `EnvelopeFixture` implements the seam for tests.

**5.3 The cooldown, re-checked live.** `Envelope.Cooldown(now)` returns
`CooldownVerdict{Known, Recent, Blocked, ClearsAt}`. `Known=false` (the event
seam did not answer) **never clears** the cooldown — U14 must treat unknown as
blocking, not as permitting, which is the opposite of what a zero count would
suggest. `ClearsAt` is when the oldest of the four leaves the window and is the
right value for a `ValidFrom` on a deferred step.

**5.4 The in-flight gate.** `DBInstance.StateUnstable()` (U11) already covers
`storage-optimization` and `modifying`; this unit refuses on the same two
statuses in `AssessParity`. U14 re-checks at execute time, because the state
U13 observed is minutes to hours old.

**5.5 The mutate input.** U14's `TestMutateInputCannotChangeClassStorageOrAZ`
has three fields to expose and no more: `iops`, `storageThroughput`,
`storageType`. Nothing in this unit produces a class, a topology, an engine
version or a shrunk allocation, so a mutate struct with only those three fields
can express every proposal U13 can make. `Proposal` already has no field for the
others (U11, `TestProposalCannotNameAnInstanceClass`).

**5.6 Revert.** `ParityPlan.Current` is the configuration observed before the
change, expressed in the same `GP3Config` shape as `ParityPlan.Config`. It is
the recorded `from` a revert restores, and `Current.IOPS`/`ThroughputMBps` are
already floored at the regime baseline, so a revert can never be talked below
one.

**5.7 Downtime.** *"Downtime doesn't occur during this change"* holds for
Provisioned IOPS and storage throughput [verified], which is why every proposal
here is `domain.ActionAdvisory` and U14's is `ActionInPlace`. The four-per-24-h
limit and the storage-optimization state are the two things that make it fail,
and both are already typed above.

## 6. Decisions a reviewer should know about

**6.1 Storage net == gross, by construction.** *"The price for a reserved DB
instance doesn't provide a discount for the costs associated with storage,
backups, and I/O"* [verified], so no Reserved DB Instance can absorb any part of
a storage-line saving. `AssessParity` therefore sets `Net = Gross` rather than
leaving a ledger unapplied, and `TestParityWiresIntoTheReservedSeam` asserts the
equality. This is why the seam needs no `domain.Netter`.

**6.2 Demand sums two percentiles rather than two series.** `MeasureIO` takes
p99 of `ReadIOPS` and p99 of `WriteIOPS` and adds them, and likewise for
throughput. That **over-states** demand — p99(read) + p99(write) ≥ p99(read +
write) — and over-stating is the only safe direction here. A point-by-point
join would be more accurate and would require the two series to share
timestamps, which CloudWatch does not guarantee.

**6.3 All four series or none.** A demand figure missing its write half is not
a smaller demand, it is an unknown one, so `IOMeasurement.Known` is false unless
all four arrived complete. Without measurement the arithmetic falls back to the
nameplate floor (which cannot degrade anything) and **no reduction is considered
at all**: cutting a number nobody measured is a guess wearing a decimal point.

**6.4 A fifth read seam, and a second CloudWatch-shaped one.** §5.8 above
records that this package is the fifth independent copy of the CloudWatch seam.
`ModificationEnvelopeAPI` is not that — it is `rds:`-side — but it does repeat
the pagination idiom for a third time inside this package. Lifting
`offsetOf`/`paginate` was possible and was not done: they are eight lines and
they live in `fixture.go`, which this unit may not edit.

**6.5 `DescribeEvents` is a judgement call.** §2.4 names the four-per-24-hours
limit and no API for observing it. `rds:DescribeEvents` is the only read
operation that reports storage modifications, and `IsStorageModificationEvent`
matches **broadly** on purpose: over-counting delays a proposal by hours,
under-counting proposes a change AWS rejects, and only one of those is visible
to an operator as a failure. The recogniser is exported so U14 and a future
verification pass can tighten it in one place.

**6.6 No package-level `var`.** `TestNoUnexpectedPackageState` carries an
allowlist this unit may not edit, so every table here is a function returning a
fresh value (`gp2Bands`, `DefaultPerformanceRates`). That is stricter than the
test requires and is the better property anyway: no caller can mutate another's
numbers.

**6.7 Money is summed in name order.** `SumUSD` sorts `[]CostPart` by name
before adding. `TestParityCostSumIsShuffleInvariant` walks all 24 permutations
of a four-part bill, asserts one bit-identical total, and asserts the premise —
that naive accumulation really does produce several distinct totals over the
same 24 permutations. `TestParityReportIsShuffleInvariant` does the same end to
end: instances, metric datapoints and collected envelopes all permuted, four
proposals totalling $639.05/mo, report bytes identical.

## 7. Deliberately deferred, and what would falsify this unit

**7.1 The band-maximum parity floor (§2.3(b)).** If AWS publishes a per-size
gp3/gp2 throughput *formula* rather than a per-band range, this unit is
conservative by up to the width of a band and the 400–1,739 GiB refusal band
narrows. One constant moves: `ParityThroughputMBps` becomes the interpolated
value instead of `Band.MaxThroughputMBps`. **This is the highest-value thing to
verify next in this unit.**

**7.2 The gp3 IOPS:throughput coupling is not modelled.** EBS gp3 caps
throughput at 0.25 MiB/s per provisioned IOPS; §2.4 states no such rule for RDS,
and the published RDS ranges (12,000–64,000 IOPS against 500–4,000 MiB/s) are
not in that ratio. Inventing it would refuse configurations AWS sells. The live
envelope is the only ceiling applied. If RDS does enforce a ratio, a proposal
here can be rejected at apply time — U14's `TestApplyRecordsFailures` is where
that surfaces, and the fix is one clause in `GP3Config.Validate`.

**7.3 io1/io2 → gp3 is not modelled.** A different product with a different
price function; `ReasonParityStorageTypeNotModelled` refuses by name. `pkg/ebs`
defers the same conversion for the same reason.

**7.4 Allocated-storage growth is never proposed.** The envelope carries
`MinAllocatedStorageGiB`/`MaxAllocatedStorageGiB` and nothing reads them for a
proposal. Growing storage to reach a striping threshold is a *permanent* cost
increase (trap 8: the floor only ratchets up) traded for a *reversible*
performance one, and this package does not make that trade on an operator's
behalf.

**7.5 The performance rates are unverified, like every other RDS rate.**
`DefaultPerformanceRates()` ships $0.02/IOPS-month and $0.08/MiB/s-month as
`RateUnverified`. On the **shipped** card gp2 and gp3 cost the same per GiB, so
no conversion is ever cheaper and every reduction refuses with
`unverified-rate` — `TestShippedParityRatesCannotClaim` pins both. The refusal
still *sizes* the opportunity in dollars, which is the whole point of U11 §5.1's
rule. There is no `LoadPerformanceRates` file loader: `PerformanceRates` is a
three-field struct a caller fills directly, and adding a JSON loader for three
numbers before anyone has the numbers would be the wrong order.

**7.6 Wiring is not done.** §6 above still describes everything `cmd/` owes, and
this unit adds exactly one more thing to that list:

```go
ec := rds.NewEnvelopeCollector(envAdapter, rds.EnvelopeCollectorConfig{Window: w})
envs, err := ec.Collect(ctx, identifiersFrom(snap))     // rds:DescribeValidDBInstanceModifications
par, err := rds.NewParity(rds.ParityConfig{             // + rds:DescribeEvents
    Now: now, Envelopes: envs, Performance: perfRates,
})
cfg := rds.DefaultConfig()
cfg.Rates, cfg.Parity = card, par                       // the seam is OPT-IN
```

Two IAM actions are added: `rds:DescribeValidDBInstanceModifications` and
`rds:DescribeEvents`. Both are optional — a nil `ModificationEnvelopeAPI` is
legal and yields a report in which every provisioning proposal is refused by
name, which is the intended loud failure mode. Leaving `cfg.Parity` nil keeps
U11's `no-storage-performance-model` refusal, unchanged.
