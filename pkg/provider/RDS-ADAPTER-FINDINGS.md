# Q4 — the live RDS SDK adapter: a field copy, and the places a field copy is not enough

`pkg/rds` shipped a complete, tested, read-only collector whose only missing
piece was the translation between an AWS SDK struct and a struct with the same
field names. `cmd/WIRING-FINDINGS.md` §6.1 named the gap and named the blocker:
`github.com/aws/aws-sdk-go-v2/service/rds` and `.../service/cloudwatch` were not
in `go.mod`. This unit adds those two modules and writes the translation.

Everything else was already proven. `--rds-fixture` drives the REAL collector —
real window clamp, real `GetMetricData` batching and ID routing, real pagination
across three pages, real truncation — so the value of this unit is precisely
that it adds no judgement. What it could not avoid adding is a decision about
what an *unset* SDK field means. §4 records every one; five of them change what
a report says, and two of those five change a verdict.

**What landed**

| File | What |
|---|---|
| `pkg/provider/rdsapi.go` | `RDSAPI` over `*rds.Client`. Implements `rds.InventoryAPI`, `rds.CommitmentAPI` and `rds.ModificationEnvelopeAPI` — six read operations. `IsAccessDenied`, `noteSet`. |
| `pkg/provider/cloudwatchapi.go` | `CloudWatchAPI` over `*cloudwatch.Client`. Implements `rds.MetricsAPI` — one read operation. |
| `pkg/provider/rdsapi_test.go` | 26 tests against a faked SDK client. No credential, no `~/.aws`, no `AWS_*`, no socket. |
| `pkg/provider/cloudwatchapi_test.go` | 11 tests, same rule, including the whole collector driven end to end through both adapters into `rds.Domain.Report`. |
| `go.mod` / `go.sum` | `service/rds v1.124.5`, `service/cloudwatch v1.67.1`, and the version bumps those two force on lines already present. Nothing else added, nothing removed. |

---

## 1. The seams, and every operation on them

`pkg/rds` declares four interfaces. `RDSAPI` implements three of them and
`CloudWatchAPI` the fourth. One concrete type covers the three `rds:` seams
because one credential answers all three — exactly as `rds.Fixture` does — and
they stay separate *interfaces* at the pkg/rds boundary for the reason pkg/rds
gives: a caller may hold one permission and not another, and the right
behaviour then is a degraded report, not a missing one.

| Interface | Method | AWS API | Paginated | IAM action | Required? |
|---|---|---|---|---|---|
| `rds.InventoryAPI` | `DescribeDBInstances` | `rds:DescribeDBInstances` | Marker | `rds:DescribeDBInstances` | **yes** |
| | `DescribeDBClusters` | `rds:DescribeDBClusters` | Marker | `rds:DescribeDBClusters` | **yes** (degrades, §3) |
| | `ListTagsForResource` | `rds:ListTagsForResource` | no | `rds:ListTagsForResource` | **yes** |
| `rds.MetricsAPI` | `GetMetricData` | `cloudwatch:GetMetricData` | NextToken | `cloudwatch:GetMetricData` | optional |
| `rds.CommitmentAPI` | `DescribeReservedDBInstances` | `rds:DescribeReservedDBInstances` | Marker | `rds:DescribeReservedDBInstances` | optional |
| `rds.ModificationEnvelopeAPI` | `DescribeValidDBInstanceModifications` | same | no | `rds:DescribeValidDBInstanceModifications` | optional (U13) |
| | `DescribeEvents` | `rds:DescribeEvents` | Marker | `rds:DescribeEvents` | optional (U13) |

`ModificationEnvelopeAPI` **does** exist — it landed in `pkg/rds/parity_envelope.go`
with U13, after `pkg/rds/FINDINGS.md` §7.5 said it would not — so it is
implemented here rather than deferred. U13 reads the envelope through this seam
and a missing envelope reaches the caller as "not answered": see §4.4, which is
the single most consequential line in this unit.

Seven operations, all GETs. `TestNoMutatingSDKSurface` walks both SDK
interfaces by reflection and fails on any method whose name begins with a
mutating verb, so `rds:ModifyDBInstance` cannot arrive here by accident any more
than it can arrive in `pkg/rds`.

### 1.1 Pagination is per-call, not internal

Every list operation paginates, and the adapter's share of that is to propagate
the token faithfully in **both** directions: the caller's `Marker` in, AWS's
`Marker` out. The page **loop** stays in `pkg/rds`, which owns it along with
`CollectorConfig.MaxPages`, the per-page `ctx.Err()` check and the warning that
says an inventory was truncated. An adapter that looped internally would defeat
all three and would return one enormous page that no budget bounds.

`TestPaginationIsPropagatedThroughEverySeam` drives the real collector over a
7-instance / 5-reservation fake at `pageSize: 3` and asserts 3 and 2 pages
respectively, with nothing lost.
`TestSwallowingAMarkerWouldTruncateSilently` is its negative: it shows that a
dropped `Marker` turns a partial inventory into a report that reads as complete.

### 1.2 Every call carries a timeout

`DefaultRDSCallTimeout = 30s`, `DefaultMetricsCallTimeout = 60s` — the metrics
call is longer because `pkg/rds` batches up to 500 queries into one request.
Both are `context.WithTimeout` on the caller's context, so the bound only ever
shortens the deadline and a cancelled parent still cancels
(`TestParentCancellationStillPropagates`). `SetCallTimeout` overrides; a
non-positive value restores the default rather than disabling the bound.
`TestEveryCallCarriesADeadline` asserts all six `rds:` calls arrive with
`ctx.Deadline()` set.

---

## 2. The exact least-privilege IAM policy

Two statements, because the two required-vs-optional halves are the whole point
of the seam split. Split them into separate policies if you want to grant the
required half and withhold the rest.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "KilterRDSInventoryRequired",
      "Effect": "Allow",
      "Action": [
        "rds:DescribeDBInstances",
        "rds:DescribeDBClusters",
        "rds:ListTagsForResource"
      ],
      "Resource": "*"
    },
    {
      "Sid": "KilterRDSOptionalDegradesWithoutThese",
      "Effect": "Allow",
      "Action": [
        "cloudwatch:GetMetricData",
        "rds:DescribeReservedDBInstances",
        "rds:DescribeValidDBInstanceModifications",
        "rds:DescribeEvents"
      ],
      "Resource": "*"
    }
  ]
}
```

Notes on the policy, and what is deliberately not claimed:

- **`rds:ListTagsForResource` is required, not a nicety.** The `kilter.dev/mode`
  opt-out lives in a tag (`pkg/rds/rds.go:184`, `ModeOff`). Without this action
  `readTags` fails, the collector emits the warning "the `kilter.dev/mode`
  guardrail cannot be evaluated for it, so an opt-out tag on that instance would
  not be honoured", and an operator who tagged a database to be left alone is
  not obeyed. Granting the two describes without this one buys a report that
  quietly ignores opt-outs.
- **`cloudwatch:GetMetricData` is not resource-scopable.** CloudWatch has no
  resource-level permissions for it; `"Resource": "*"` is the only form that
  works. It is also the only action here that can cost money at scale — one
  `GetMetricData` request per 500 queries, and `pkg/rds` issues
  `len(CollectedMetrics()) == 11` queries per instance. `rds.CollectedMetrics()`
  is exported so an operator can size that bill without reading the source.
- `rds:ListTagsForResource` and `rds:DescribeValidDBInstanceModifications` act on
  a single instance and can be narrowed to
  `arn:aws:rds:<region>:<account>:db:*`. Whether the *list* actions accept
  resource-level ARNs varies by action and has changed over time
  [unverified: which of the `rds:Describe*` actions accept resource-level ARNs
  in the current IAM reference]. `"Resource": "*"` is correct for all of them,
  so it is what is published here.
- No `rds:Modify*`, no `rds:Reboot*`, no `rds:Create*`, no `rds:Delete*`. This
  policy grants nothing that can change an account. `TestNoMutatingSDKSurface`
  is the code-side half of the same statement.

---

## 3. What degrades, and what fails

This is the table cmd/ has to read before it decides which seams to wire, and
it is not symmetric. Getting it wrong is how an optional permission becomes a
failed run.

| Missing / failing | Behaviour today | Where |
|---|---|---|
| `DescribeDBInstances` fails | **Collection fails.** The one hard dependency: with no inventory there is nothing to report on. | `collect.go` `describeInstances` |
| `DescribeDBClusters` fails | Warning; members are still excluded, but under the more cautious `cluster-member-not-supported` rather than `aurora-not-supported`. | `describeClusters` |
| `ListTagsForResource` fails | Warning naming the instance; the `kilter.dev/mode` guardrail is unevaluated for it. | `readTags` |
| **`MetricsAPI` is nil** | Complete report; every instance refuses with `no-metric-evidence`. | `readMetrics` |
| **`GetMetricData` *fails* mid-run** | **Collection fails.** Not the same as nil. | `collect.go` `readMetrics`: `return fmt.Errorf("rds: get metric data: %w", err)` |
| `CommitmentAPI` is nil | Complete report; net == gross. | `describeReservations` |
| `DescribeReservedDBInstances` fails | Warning: "net savings equal gross in this report, which under-claims". | same |
| `ModificationEnvelopeAPI` is nil | Every provisioning proposal refuses with `provisioning-envelope-unknown`. | `parity_envelope.go` `Collect` |
| `DescribeValidDBInstanceModifications` fails | Per-instance warning; that instance's envelope stays unknown; its proposals refuse. | same |
| `DescribeEvents` fails | Per-instance warning; `HistoryKnown=false`; the four-per-24-hours limit is reported unverified rather than cleared. | `history` |

**The asymmetry that matters: a nil `MetricsAPI` degrades, a failing one does
not.** `pkg/rds/FINDINGS.md` §6.2 calls `cloudwatch:GetMetricData` optional, and
it is — but only in the nil form. A credential that lacks the permission and is
wired anyway produces `AccessDeniedException` from the first call and
`Collect` returns an error, so the operator gets no report at all rather than
the documented degraded one.

That is why `IsAccessDenied` is exported. It matches **only** permission
denials — `AccessDenied`, `AccessDeniedException`, `UnauthorizedOperation`,
`AuthorizationError`, `AuthFailure`, `NotAuthorized`, `Forbidden`. A throttle, a
timeout or a malformed request stays an error, because swallowing those would
turn a transient fault into a permanently degraded report that claims the
credential lacks a permission it actually holds
(`TestIsAccessDenied` pins both halves).

§6.3 gives cmd/ the retry that turns the failure into the degradation.

### 3.1 `Notes()` — degradations the seam has no field for

Three facts the adapter learns cannot be expressed through the seam's return
types, because the seam structs have no field for them, and all three are
silent in `pkg/rds`: an instance with neither ARN nor identifier (dropped by
`recordID`), a tag with no key, an event with no date. Both adapters therefore
carry a deduplicated, sorted, mutex-guarded `Notes() []string`, and **cmd/ must
render it beside `Snapshot.Warnings`** (§6.2). A degradation nobody can see is a
degradation that did not happen.

`Notes()` returns `nil` on a clean run, so wiring it costs nothing when there is
nothing to say.

---

## 4. Every nilable field, and what its absence was decided to mean

The rule: an unset SDK pointer is *unknown*, not zero. Where the destination
field can carry that distinction, it does. Where it cannot — and `pkg/rds`'s
records are deliberately plain scalars — the decision is stated, its direction
is argued, and it is tested.

### 4.1 `DBInstance` — `rds:DescribeDBInstances`

| SDK field | Type | nil ⇒ | Why that is the safe reading |
|---|---|---|---|
| `DBInstanceIdentifier`, `DBInstanceArn` | `*string` | `""` | With both empty the record is unaddressable and `pkg/rds` skips it. Faithful copy **plus a note**, because pkg/rds's skip is silent. |
| `DBInstanceClass`, `DBInstanceStatus`, `Engine`, `EngineVersion`, `LicenseModel`, `AvailabilityZone`, `StorageType` | `*string` | `""` | `pkg/rds` already refuses an unknown engine (`unknown-engine`) and an empty class by name. Empty is the refusing value, not a plausible one. |
| `MultiAZ` | `*bool` | `false` | **The one that moves money.** `DBInstance.Deployment()` maps false → `RDSSingleAZ`, a ×1 rather than ×2 multiplier. The destination is a `bool` and cannot hold "unknown". False *under*-states the instance line, and an under-stated line can only under-state a saving derived from it — the direction this package is built to fail in. `TestNilMultiAZUnderStatesRatherThanOverStates`. |
| `DBClusterIdentifier` | `*string` | `""` | Empty means "not a cluster member", which is what AWS's omission means. |
| `ReadReplicaSourceDBInstanceIdentifier` | `*string` | `""` | Empty on a primary, per the seam's own doc comment. |
| `ReadReplicaDBInstanceIdentifiers` | `[]string` | `nil`, blanks dropped | Carrying `""` forward would invent a replica named `""`. |
| `AllocatedStorage`, `MaxAllocatedStorage` | `*int32` | `0`, widened to `int64` | A nil `MaxAllocatedStorage` is AWS's own encoding of "storage autoscaling is off", which is exactly what 0 means to `pkg/rds`. The widening is lossless: no value RDS issues approaches the int32 ceiling. |
| `Iops`, `StorageThroughput` | `*int32` | `0` | Absent on gp2/standard because they are **not provisionable** there. 0 is the honest reading of that absence, and `pkg/rds` reads 0 as "not provisioned", never as "measured zero". |
| `InstanceCreateTime` | `*time.Time` | zero time | `pkg/rds` uses it only for an age display. |
| `TagList` | `[]Tag` | `nil` | An empty map and a nil map both trigger `pkg/rds`'s `ListTagsForResource` fallback, which is the behaviour you want. |
| `Tag.Key` | `*string` | entry dropped **+ note** | A tag named `""` is not a tag. The note names the instance and says out loud that if the dropped tag was `kilter.dev/mode`, the opt-out is not honoured. |
| `Tag.Value` | `*string` | `""` | A nil value is AWS's encoding of a legal empty-valued tag, and `""` is not `"off"`, so the guardrail reads it correctly. |

`TestEveryNilableInstanceFieldHasADecision` asserts by reflection that every
field this adapter reads still exists on the SDK struct — so an upstream rename
breaks the test instead of silently zeroing a column — and then feeds an
entirely unset `DBInstance` through and requires the exact zero record plus the
unaddressable-instance note.

### 4.2 `DBCluster` — `rds:DescribeDBClusters`

`ServerlessV2ScalingConfiguration` is a nil **struct pointer** on every cluster
that is not Serverless v2, so `ServerlessV2MinCapacity`/`MaxCapacity` stay 0.
That is free: `ClusterInfo.ServerlessV2MinACU` is carried so the Aurora refusal
can *name* the lever a future unit would look at, and `pkg/rds` never does
arithmetic with it. `DBClusterMembers` entries with a nil
`DBInstanceIdentifier` are dropped for the same reason blank replica
identifiers are.

### 4.3 `ReservedDBInstance` — `rds:DescribeReservedDBInstances`

`FixedPrice`, `UsagePrice`, `Duration` and `MultiAZ` are copied **raw**. The
amortization (`EffectiveHourly = UsagePrice + FixedPrice ÷ term hours`), the
`active`/`payment-pending` filter and the topology mapping all live in
`pkg/rds.reservationFromRecord` and are not repeated here — that is
`FINDINGS.md` §6.2's explicit instruction, and
`TestReservationFieldsAreCopiedRawAndAmortizedOnlyByPkgRDS` proves the
arithmetic runs exactly once by asserting a raw `FixedPrice: 8760, Duration:
31536000` becomes `EffectiveHourlyUSD == 1.05`, not `2.05` and not `1.00`.

Three nils get notes rather than repair, because in each case `pkg/rds` has
already chosen and its choice is defensible — but invisible:

| nil | `pkg/rds` does | Note says |
|---|---|---|
| `DBInstanceCount` | `count <= 0` ⇒ the reservation is **dropped**, not guessed at 1 | "it is dropped rather than counted as one, so it contributes no coverage to this report" |
| `State` | `case "", "active", "payment-pending"` ⇒ treated as **billing** | "a retired reservation counted as live can only make a saving smaller, never larger" |
| `Duration` with a non-zero `FixedPrice` | keeps the usage price alone; the upfront is unamortizable and dropped | "under-stating this reservation's cost and therefore under-stating stranding" |

`Duration` is `*int32` **seconds** in the SDK and `int64` in the record; the
longest RDS term is three years (94,608,000 s), so the widening is lossless.

### 4.4 `ValidStorageOptions` — `rds:DescribeValidDBInstanceModifications`

**This is the decision that changes a verdict, and it is the classic
nil-becomes-zero bug in its most expensive form.**

AWS answers in ranges — `ProvisionedIops []Range` of `{From, To, Step}`, all
`*int32`. `rds.ValidStorageOptionRecord` carries one overall minimum and
maximum per dimension, and its own doc comment says that reduction is what an
adapter should perform ("AWS returns ranges; this reduces each to its overall
minimum and maximum"). `reduceRanges` does exactly that, skipping nil bounds
rather than reading them as 0.

The trap is what happens next. `pkg/rds/parity.go:487` enforces the ceiling as:

```go
if env.MaxIOPS > 0 && c.IOPS > env.MaxIOPS { ... refuse ... }
```

so a `StorageEnvelope` that is `Known` with `MaxIOPS == 0` has **no ceiling**,
not an unknown one. And `envelopesFromRecords` sets `Known = true` for any
record carrying a non-empty storage type. Emitting a record whose ranges AWS
never filled would therefore convert *"AWS did not tell us the ceiling"* into
*"this instance has no ceiling"* — and an 80,000-IOPS proposal would pass
validation on an instance capped at 16,000, which is precisely the contradiction
`FINDINGS.md` §2.4 refuses to resolve by guessing.

So: **a storage type is emitted only when AWS named a positive upper bound for
both provisionable dimensions.** Otherwise the record is omitted, `Envelope.For`
returns `Known=false`, and the proposal is refused under its own name,
`provisioning-envelope-unknown`. `StorageEnvelope`'s doc comment asks for
exactly this — "A record whose ranges are empty is an UNKNOWN envelope, not a
zero one" — and omission is the only way the seam lets an adapter say it.

This over-refuses for storage types where nothing is provisionable in the first
place (gp2, standard, and io1's throughput), which costs nothing: `pkg/rds` only
ever looks up gp3 (`env.For(StorageGP3)` at `parity_assess.go:472` and `:563`).
Every omission carries a note.

`TestEnvelopeWithNoReadableCeilingStaysUnknown` covers four shapes — no ranges,
a nil upper bound, an absent throughput list, and `To: 0` — and asserts through
the real `EnvelopeCollector` that `Known` stays false in all four. Deleting the
gate makes all four fail.

`Range.Step` has nowhere to go: the record carries a min and a max and nothing
else. A step larger than 1 is noted, because a value inside the range but off
the step passes this package's check and is rejected by AWS at apply time.

An answer with a nil `ValidDBInstanceModificationsMessage` returns an **empty
output plus a note**, not an error: empty leaves every envelope unknown, which
is the refusing default.

### 4.5 `MetricDataResult.StatusCode` — `cloudwatch:GetMetricData`

The same trap in string form. `pkg/rds/collect.go` reads:

```go
ser.Status = r.StatusCode
if ser.Status == "" { ser.Status = StatusComplete }
ser.Partial = ser.Status != StatusComplete
```

so an unset status becomes **Complete** and the series is treated as whole
evidence. Passing `""` through would turn "CloudWatch did not vouch for this
series" into "this series is complete", and a complete-looking
`DatabaseConnections` series is exactly what an idle verdict is made of.

**Decision: an unset `StatusCode` is reported as `PartialData`**, plus a note.
This is not an invention — it is the same reading `pkg/rds` itself applies to a
result that never arrived at all (`StatusTruncated`: "a MISSING result means the
response was truncated, which is a fact about the response, not about the
metric"). `Complete`, `PartialData`, `InternalError` and `Forbidden` all pass
through verbatim; only the empty string is resolved, and it is resolved away
from evidence. `TestUnsetStatusBecomesPartialNotComplete` checks both the seam
value and the resulting `Series.Partial` through the real collector.

`MetricDataResult.Id` nil is copied as `""` **plus a note**: `pkg/rds` routes by
ID, so an unidentified result lands in a slot no query owns and the real query
stays unanswered — which produces a `Truncated`, refusing series. Safe, and
invisible without the note.

### 4.6 `Event.Date` — `rds:DescribeEvents`

Nil becomes the zero time, which `Envelope.Cooldown` cannot place inside the
trailing 24-hour window, so an **undated storage modification does not count**
toward the four-per-24-hours limit. `pkg/rds` calls under-counting the worse of
the two errors by name — "over-counting delays a proposal by hours,
under-counting proposes a change AWS will reject" — so the adapter cannot repair
it (it has no date to supply) but it does say so. See §7.3 for the pkg/rds fix.

### 4.7 Outbound nils

Symmetric, and for the same reason: an empty seam field must stay unset on the
wire rather than become an explicit empty filter.

- `Marker`/`NextToken`/`SourceIdentifier` empty ⇒ `nil`.
- `MaxRecords == 0` ⇒ `nil`. Sending 0 would have AWS reject a request the
  caller never made — the valid range is 20–100 (`TestMaxRecordsIsOnlySentWhenAsked`).
- `StartTime`/`EndTime` zero ⇒ `nil` on `DescribeEvents`; sending a zero
  `time.Time` means the year 1 and fails the whole request.
- `GetMetricData` **requires** both times, so a zero or inverted window is
  refused client-side with a message that names the window, rather than sent
  and answered with a validation error that reads like a permissions problem.
  A zero `PeriodSeconds` is refused the same way.
- An empty query list makes **no call at all** (CloudWatch rejects it) and
  returns an empty output. Nothing was asked, so nothing was answered.
- `MetricStat.Unit` is deliberately left unset. Naming a unit *filters*
  datapoints to those published with it, so a wrong guess returns an empty
  series — which reads as a quiet database.
- Dimensions are sorted by name. Go randomizes map iteration, and an unsorted
  dimension list makes two runs over the same account send two different
  requests, which defeats every replay and diff in this tree
  (`TestDimensionsAreOrderedSoTwoIdenticalCollectionsIssueIdenticalRequests`
  runs 25 iterations).

---

## 5. What this adapter deliberately does not do

- **No amortization.** §4.3.
- **No topology derivation.** `MultiAZ` in, `MultiAZ` out;
  `DBInstance.Deployment()` decides.
- **No engine normalization.** `ProductDescription` is copied raw;
  `commit.NormalizeRDSEngine` runs inside `pkg/rds`.
- **No retry, no backoff.** The SDK's own standard retryer handles throttling.
  Adding a second layer here would multiply against the per-call timeout.
- **No `ScanBy`, no `MaxDatapoints`.** Both are left at CloudWatch's defaults —
  see §7.1, where the reason is that choosing either would be answering a
  `pkg/rds` question from the wrong side of the seam.
- **No live call anywhere in the test suite.** Every test drives a fake client.
  `NewRDSAPI`/`NewCloudWatchAPI` reject a blank region *before* calling
  `LoadDefaultConfig`, so even the constructor tests read no credential.

---

## 6. The exact wiring `cmd/` must do

`cmd/kilter/rds.go` currently holds `collectRDS(ctx, path, …)` over
`--rds-fixture`, and `cmd/kilter/domains.go:317` loops over `df.rdsFixtures`.
The live path is a sibling, not a replacement: **keep `--rds-fixture`**. It is
how the collector is tested without an account, and `cmd/kilter/rds_test.go`
depends on it.

### 6.1 One new flag, one new function

```go
// cmd/kilter/domains.go, beside --rds-fixture
fs.Var(&df.rdsRegions, "rds-region",
    "collect RDS live from this region (requires AWS credentials; repeatable)")
```

```go
// cmd/kilter/rds.go
func collectRDSLive(ctx context.Context, scope, region string,
    now time.Time, span time.Duration) (*krds.Snapshot, []string, error) {

    inv, err := provider.NewRDSAPI(ctx, region)
    if err != nil {
        return nil, nil, err
    }
    cw, err := provider.NewCloudWatchAPI(ctx, region)
    if err != nil {
        return nil, nil, err
    }

    // pkg/rds/FINDINGS.md §6.3. The window is clamped by the collector, so
    // c.Window() is what was observed and the request is not.
    cfg := krds.DefaultCollectorConfig(krds.Window{Start: now.Add(-span), End: now})
    cfg.Scope, cfg.Region = scope, inv.Region()   // the SAME region the client talks to

    collect := func(metrics krds.MetricsAPI) (*krds.Snapshot, error) {
        // inv is passed twice: once as InventoryAPI, once as CommitmentAPI.
        // A denied DescribeReservedDBInstances already degrades to a warning
        // inside pkg/rds, so the commitment seam never needs to be dropped.
        c, err := krds.NewCollector(inv, metrics, inv, cfg)
        if err != nil {
            return nil, err
        }
        return c.Collect(ctx)
    }

    var warnings []string
    snap, err := collect(cw)
    // The ONE seam that must be dropped rather than retried: a nil MetricsAPI
    // degrades, a denied one fails the whole collection (§3).
    if err != nil && provider.IsAccessDenied(err) &&
        strings.Contains(err.Error(), "get metric data") {

        warnings = append(warnings, region+": no cloudwatch:GetMetricData; every instance is "+
            "reported without CloudWatch evidence and refuses with no-metric-evidence")
        snap, err = collect(nil)
    }
    if err != nil {
        return nil, nil, fmt.Errorf("rds %s: %w", region, err)
    }

    if got := c.Window(); got != cfg.Window {
        warnings = append(warnings, fmt.Sprintf(
            "%s: observation window clamped to %s (1-minute CloudWatch datapoints live %s)",
            region, got.String(), krds.RetentionAtOneMinute))
    }
    // §6.2 — the adapters' own notes, which the seam structs cannot carry.
    for _, n := range append(inv.Notes(), cw.Notes()...) {
        warnings = append(warnings, region+": "+n)
    }
    for _, w := range snap.Warnings {
        warnings = append(warnings, region+": "+w)
    }
    return snap, warnings, nil
}
```

(Hoist the `*krds.Collector` out of the closure if you want `c.Window()` as
written; the fixture path in `collectRDS` already does exactly that.)

Everything downstream is unchanged: `rdsDomain.Observe(snap)`,
`inv.ReservedDBs = append(inv.ReservedDBs, snap.Reservations...)`, and
`rep.Refusals()` — `cmd/kilter/domains.go:317` already does all three for the
fixture path, and a live snapshot is the same type.

### 6.2 Render `Notes()`

Not optional. Three degradations — an unaddressable instance, a keyless tag, an
undated modification — are **silent in `pkg/rds`** because its seam structs have
no field for them (§3.1). `Notes()` is where they surface, it returns `nil` when
there is nothing to say, and the snippet above folds it into `rt.Warnings`
alongside `snap.Warnings`.

### 6.3 The access-denied retry, and why only one seam needs it

The two optional seams degrade differently, and the difference is the whole
reason this section exists.

**`rds:DescribeReservedDBInstances` needs nothing.** `describeReservations`
returns a warning string and never an error, so a denied read already produces
a complete report with "net savings equal gross in this report, which
under-claims". Wire it unconditionally.

**`cloudwatch:GetMetricData` needs the retry.** It is optional *in the nil form
only*: `readMetrics` returns `fmt.Errorf("rds: get metric data: %w", err)` and
`Collect` propagates it, so a credential that lacks the permission and is wired
anyway produces no report at all rather than the documented degraded one. The
single retry in §6.1 converts that into the promised behaviour — every instance
refusing with `no-metric-evidence` — and `IsAccessDenied` is what keeps a
throttle or a timeout from being mistaken for a missing permission.

The alternative is to let the operator declare what they hold —
`--rds-no-metrics` — which costs one fewer wasted collection and makes the
degradation an explicit statement rather than an inference. Either is honest.
**What is not acceptable is wiring the seam and letting the run die**, because
the operator then sees `AccessDeniedException` where the design promises a
complete report.

`rds:DescribeDBClusters` needs no treatment either: `pkg/rds` already degrades
it to a warning.

### 6.4 U13's envelope, when a proposal path wants it

```go
ec := krds.NewEnvelopeCollector(inv, krds.EnvelopeCollectorConfig{
    Window: krds.Window{Start: now.Add(-48 * time.Hour), End: now},
})
envs, err := ec.Collect(ctx, identifiersFrom(snap))
```

`inv` is the same `*provider.RDSAPI`. A window shorter than 24 h cannot answer
the cooldown question and the collector warns about it. Passing `nil` instead of
`inv` is legal and refuses every provisioning proposal by name.

### 6.5 One collector per region

Unchanged from every shipped domain: `cmd/` runs one `RDSAPI` +
`CloudWatchAPI` + `Collector` per region and merges the reports. Pair the two
adapters on the **same** region — a metric is published in the region its
database lives in, and a cross-region pairing returns empty series for every
instance, which reads as an account full of idle databases. Both constructors
require an explicit region for this reason and reject a blank one.

---

## 7. Findings for `pkg/rds` — decisions that belong on the other side of the seam

Out of scope for this unit by construction. Each is stated as the failure it
produces, in the style §8 of `pkg/rds/FINDINGS.md` uses.

### 7.1 `GetMetricData` continuation results are dropped, not merged

`collect.go`'s page loop is first-write-wins per query ID:

```go
for _, r := range res.Results {
    if _, dup := got[r.ID]; dup { continue }
    got[r.ID] = r
}
```

CloudWatch caps a response at 100,800 datapoints (`MaxDatapoints`) across all
queries and pages the remainder. `pkg/rds` issues 11 queries per instance; at
60 s over 14 days each series is ~20,160 datapoints, so **one response holds
about five series** — half an instance. Every subsequent page repeats IDs
already seen, and `continue` discards their datapoints.

The failure: a query whose data spans a page boundary keeps only the datapoints
from the page it first appeared on, and — because that page's `StatusCode` is
`Complete` — the resulting `Series` is **not marked partial**. A truncated
`DatabaseConnections` series that looks complete is the exact shape
`StatusTruncated`'s doc comment exists to prevent.

The fix is one branch: append `Timestamps`/`Values` on a duplicate ID instead of
dropping, and the existing `sort.SliceStable` by timestamp already handles the
ordering. Until it lands, this adapter leaves `ScanBy` and `MaxDatapoints` at
CloudWatch's defaults — choosing a scan order would be picking *which* half of
a split series survives, which is a `pkg/rds` question and not an adapter's to
answer. [unverified: whether CloudWatch marks the results of a paginated
response `PartialData`; if it does, the series is at least honestly partial and
only the datapoints are lost.]

### 7.2 `StorageEnvelope` cannot distinguish "no ceiling" from "unknown ceiling"

`parity.go:487`'s `env.MaxIOPS > 0 &&` guard resolves the ambiguity toward
*permissiveness*, while `StorageEnvelope.Known` resolves it toward refusal. The
two disagree, and this adapter has to pick omission (§4.4) to keep the safe one.

The consequence of omission is a refusal message that reads "…needs the range
`rds:DescribeValidDBInstanceModifications` reports, and it was not read. …
Supply the seam to `rds.NewEnvelopeCollector` to unblock it" — which is
**misleading** when the seam *was* supplied and AWS simply returned a partial
envelope. A `MaxIOPSKnown bool` beside `MaxIOPS` (or a sentinel of −1) would let
the adapter say "AWS answered, and named no ceiling" and let the refusal say so
too.

### 7.3 `Envelope.Cooldown` silently ignores undated events

An `EventRecord` with a zero `Date` is recognised by
`IsStorageModificationEvent` and then filtered out by `Cooldown`'s window test,
so it does not count. That inverts the recogniser's own stated bias toward
over-counting. An undated storage-modification event should set
`HistoryKnown = false` — "we cannot rule the limit out from this" — rather than
be quietly dropped.

### 7.4 An empty `StatusCode` defaults to `Complete`

`collect.go`'s `if ser.Status == "" { ser.Status = StatusComplete }` is the
string form of the nil-pointer trap. This adapter resolves it on its side
(§4.5), but the default belongs on the other side: every future CloudWatch
adapter — and there are four other domains that will each write one — has to
re-derive the same fix. Defaulting to `StatusPartialData` in `pkg/rds` would
make the safe reading the one you get for free.

### 7.5 An unaddressable instance is dropped without a warning

`describeInstances` skips a record whose `recordID` is empty, alongside caps and
page budgets that each emit a warning naming what is absent. This one does not,
so an instance can vanish from a report that otherwise says out loud when it is
incomplete. One `warns = append(...)` line.

---

## 8. Things that would falsify parts of this unit

1. **If AWS begins returning `MultiAZ` as nil on real instances**, §4.1's
   under-stating fallback stops being a theoretical guard and starts halving
   real instance lines. The honest fix is a `MultiAZKnown` field and a
   `unknown-deployment` refusal, which `pkg/rds` already has a reason code for
   (`ReasonUnknownDeployment`) but cannot reach through the record.
2. **If `DescribeValidDBInstanceModifications` returns a gp3 entry with
   `ProvisionedIops` filled and `ProvisionedStorageThroughput` empty on a real
   instance**, §4.4's both-dimensions gate over-refuses on a case that matters,
   and the gate should narrow to IOPS alone. Nothing in the AWS documentation
   settles this [unverified], so the conservative gate is what shipped.
3. **If CloudWatch marks paginated continuation results `PartialData`**, §7.1's
   failure is reduced from "silent truncation" to "lost datapoints on a
   correctly-flagged partial series", which is much less serious and lowers the
   priority of the `pkg/rds` fix.
4. **If a future `pkg/rds` release adds a fifth seam that writes**,
   `TestNoMutatingSDKSurface` fails here first, which is the intended order.
