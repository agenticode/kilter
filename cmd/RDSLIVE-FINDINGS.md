# S3 — the live RDS collector and the storage-parity seam become reachable

Two shipped, tested, read-only features could not be run from the binary.

`cmd/WIRING-FINDINGS.md` **§6.1**: the live RDS collector was blocked on
`go.mod`, and that blocker is gone — `pkg/provider.NewRDSAPI` and
`NewCloudWatchAPI` landed with PR#45 and implement all four `pkg/rds` seams.
**Nothing called them.** `cmd/kilter/rds.go` could only be driven by
`--rds-fixture`.

`cmd/WIRING-FINDINGS.md` **§6.4**, last bullet: `pkg/rds`'s `StorageParity`
seam was "still nil, as U11 shipped it — `--rds-fixture` cannot enable it and
no flag pretends to". `pkg/rds/parity_assess.go` implements it and is merged.
**Nothing constructed it.**

Both now work:

```
kilter domains report --domain rds --rds-region us-east-1 --scope 000000000000/us-east-1
kilter domains report --domain rds --rds-fixture account.json --rds-parity
```

**Status:** `gofmt -l ./cmd` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./cmd/...` and `go test -race -short ./...` all green.
**`go.mod` and `go.sum` are unchanged** — every module this unit imports was
already required. No existing test was weakened, edited or deleted; the
committed `testdata/rds-account.json` is byte-identical and
`TestWriteRDSFixture` still passes.

| | |
|---|---|
| New production code | `cmd/kilter/rdslive.go` (507 lines) + 145 added lines across `rds.go` and `domains.go` |
| New tests | 971 lines, **19 test functions**, zero AWS calls |

```
cmd/kilter/rdslive.go            the live seams, the one retry, the parity holder, the rate loader
cmd/kilter/rds.go                rdsFixtureFile grows the U13 seam; collectRDS splits
cmd/kilter/domains.go            three flags, one constructor call, one new loop
cmd/kilter/rdslive_test.go       the live path: degradation, denial, tags, notes, determinism
cmd/kilter/rdsliveparity_test.go the parity seam: off, on-without-envelope, on-with-envelope
```

---

## 1. The exact IAM policy a user now needs

Two statements, because the required/optional split is the whole point of the
seam split. Grant the first and withhold the second and you get a **complete
report with fewer dimensions and a note on each**; withhold anything in the
first and you get either a loud failure or a report that quietly disobeys an
operator.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "KilterRDSRequired",
      "Effect": "Allow",
      "Action": [
        "rds:DescribeDBInstances",
        "rds:DescribeDBClusters",
        "rds:ListTagsForResource"
      ],
      "Resource": "*"
    },
    {
      "Sid": "KilterRDSOptionalEachBuysOneDimension",
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

Seven actions, **all of them GETs**. This policy grants nothing that can change
an account: no `rds:Modify*`, no `rds:Reboot*`, no `rds:Create*`, no
`rds:Delete*`. `pkg/provider`'s `TestNoMutatingSDKSurface` walks both SDK
interfaces by reflection and fails on any mutating verb, and
`TestNoActuatorBecomesReachableThroughTheLivePath` asserts the same thing from
the CLI end: a live, parity-enabled `rds` domain still produces
`actuatable=false`, zero steps and `RefuseReportOnly`.

### 1.1 Required — what each one is required *for*

| Action | Why it is required |
|---|---|
| `rds:DescribeDBInstances` | The one **hard** dependency. With no inventory there is nothing to report on, and a report that silently covered fewer databases than the account holds is worse than no report. Denial fails the run, loudly, naming the action. |
| `rds:DescribeDBClusters` | Required-but-degrades. It is the only way to tell an **Aurora** cluster from a **Multi-AZ DB cluster** without inferring it from a member's engine string, and calling a MySQL Multi-AZ cluster "Aurora" would be a false statement in a report whose entire value is that its statements are true. Denial warns and falls back to the more cautious `cluster-member-not-supported`. |
| `rds:ListTagsForResource` | Required, and not as a nicety. The `kilter.dev/mode=off` opt-out lives in a tag. Without this action an operator who tagged a database to be left alone is **not obeyed**. See §2.2 — this is the degradation with the worst failure mode in the set. |

### 1.2 Optional — what each one buys, and what you get without it

| Action | Buys | Without it |
|---|---|---|
| `cloudwatch:GetMetricData` | Every measurement-derived finding: idle detection, memory verdicts, unused storage, and the four I/O series `--rds-parity` needs. **The only action here that costs money at scale** — `pkg/rds` issues `len(rds.CollectedMetrics()) == 11` queries per instance, batched 500 per request. Not resource-scopable; `"Resource": "*"` is the only form CloudWatch accepts. | A **complete inventory** in which every instance refuses with `no-metric-evidence`, plus a warning. No idle verdict is ever drawn from the silence. |
| `rds:DescribeReservedDBInstances` | Net-of-commitment savings. | Complete report, `net == gross`. That **under**-claims, and a warning says so. |
| `rds:DescribeValidDBInstanceModifications` | *(only read under `--rds-parity`)* The live provisioning envelope — the IOPS and throughput ranges AWS will actually accept for this instance. | Every provisioning proposal refused by name with `provisioning-envelope-unknown`. **No published ceiling is assumed**: §2.4 names two AWS ceilings that contradict each other and `pkg/rds` hardcodes neither. |
| `rds:DescribeEvents` | *(only read under `--rds-parity`)* The storage-modification history, i.e. the four-per-24-hours limit. | Per-instance warning; `HistoryKnown=false`, so the limit is reported **unverified rather than cleared**. |

Both `--rds-parity` actions can be narrowed to
`arn:aws:rds:<region>:<account>:db:*`, as can `rds:ListTagsForResource` — they
act on a single instance. Whether the `Describe*` **list** actions accept
resource-level ARNs varies by action and has changed over time
[unverified], so `"Resource": "*"` is what is published here.

---

## 2. Every degradation path, and how it is reported

The table `cmd/` had to read before deciding what to wire is **not symmetric**,
and getting it wrong is exactly how an optional permission becomes a failed
run. Every row below is tested from the CLI.

| Failure | Result | Where it is said | Test |
|---|---|---|---|
| `DescribeDBInstances` denied/fails | **Hard failure**, non-zero exit | the error, naming the action | `TestAMissingRequiredPermissionFailsLoudly` |
| `DescribeDBClusters` denied | complete report; members excluded under `cluster-member-not-supported` | `snap.Warnings` → `rt.Warnings` | `TestOptionalPermissionsDegradeToAWarningAndNeverToAFailure` |
| `ListTagsForResource` denied | complete report; guardrail **unevaluated** for that instance | warning naming the **ARN** and `kilter.dev/mode` | `TestAnUnreadableTagIsNotAnAbsentTag` |
| `GetMetricData` **denied** | one retry with the seam dropped ⇒ complete report, every instance `no-metric-evidence` | warning naming `cloudwatch:GetMetricData` | `TestAFailingMetricsSeamIsNotTheSameAsAnAbsentOne` |
| `GetMetricData` throttled/timed out | **hard failure** — not a degradation | the error | `TestAThrottleIsNotMistakenForAMissingPermission` |
| `DescribeReservedDBInstances` denied | complete report; `net == gross` | warning saying it "under-claims" | `TestOptionalPermissionsDegradeToAWarningAndNeverToAFailure` |
| `DescribeValidDBInstanceModifications` absent/denied | complete report; `provisioning-envelope-unknown` per instance | `Envelopes.Warnings` → `rt.Warnings` | `TestParityWithoutTheEnvelopeSeamRefusesByName…` |
| `DescribeEvents` denied | complete report; `HistoryKnown=false` | warning: "unverified rather than cleared" | `TestADeniedDescribeEventsLeavesTheLimitUnverifiedRatherThanCleared` |
| SDK facts with no seam field | complete report | `RDSAPI.Notes()` / `CloudWatchAPI.Notes()`, rendered beside `snap.Warnings` | `TestTheAdaptersNotesAreRenderedBesideTheSnapshotWarnings` |
| window > CloudWatch retention | clamped by `pkg/rds` | warning naming the **observed** window | `TestTheLiveWindowIsClampedByPkgRDSAndTheClampIsSaidOutLoud` |

### 2.1 The one degradation `cmd/` had to implement itself

`RDS-ADAPTER-FINDINGS.md` §3 puts the asymmetry in bold and it is the entire
content of this unit's error handling: **a nil `MetricsAPI` degrades, a failing
one does not.** `readMetrics` returns `fmt.Errorf("rds: get metric data: %w",
err)` and `Collect` propagates it, so a credential that lacks
`cloudwatch:GetMetricData` and is wired anyway produces `AccessDeniedException`
and **no report at all** where the design promises the documented degraded one.

So there is exactly one retry, and it is gated on two conditions, both
load-bearing:

```go
func isMetricsAccessDenied(err error) bool {
	return err != nil && provider.IsAccessDenied(err) &&
		strings.Contains(err.Error(), "get metric data")
}
```

- **`provider.IsAccessDenied`** matches only permission denials. A throttle or
  a timeout stays an error, because swallowing it would turn a transient fault
  into a permanently degraded report claiming the credential lacks a permission
  it actually holds — and the operator would then go and grant a permission
  that was never missing.
- **the message test** keeps the retry pointed at the metrics seam. Without it,
  an `AccessDenied` on the **required** `DescribeDBInstances` would be caught by
  the metrics branch and downgraded into "no CloudWatch", producing a
  confident, complete-looking report over **zero databases**.

Deleting either half makes a test fail. Replacing `err != nil &&
isMetricsAccessDenied(err)` with `false` makes
`TestAFailingMetricsSeamIsNotTheSameAsAnAbsentOne` fail with the raw
`AccessDeniedException`, which is the behaviour it exists to prevent.

Every other optional seam is wired **unconditionally**, because `pkg/rds`
already degrades it internally. Adding a second retry layer for
`DescribeReservedDBInstances` would be dead code guarding a call that cannot
fail the collection.

### 2.2 "I could not look" is not "I looked and there was nothing"

The single most dangerous class of bug in this codebase, and the reason
`TestAnUnreadableTagIsNotAnAbsentTag` asserts **two** things rather than one.

`db-legacy` carries `kilter.dev/mode=off`. `provider.RDSAPI.ListTagsForResource`
returns the error rather than an empty `TagList`; `pkg/rds`'s `readTags`
returns `(nil, warning)` and the collector appends a warning naming the ARN and
the tag key. The wiring passes both through untouched. What it must never do —
and does not — is fold an `AccessDenied` into an empty tag map, because
`DBInstance.ModeOff()` reads a missing key as "not opted out".

The test asserts the control (tag readable ⇒ `guardrail-mode-off` fires, no
warning) **and** the subject (tag denied ⇒ warning naming the ARN, and
`guardrail-mode-off` does **not** fire). The disappearance of the refusal is
what makes the warning load-bearing: assert only the warning and the test still
passes on a wiring that honours the tag anyway; assert only the refusal and the
test cannot tell "unreadable" from "absent".

**A finding for `pkg/rds`, unfixed here and out of scope:** an unreadable tag
adds a warning but does **not** set `Snapshot.Stale`, and there is no refusal
code for it — so an instance whose guardrail could not be evaluated is
assessed, priced and reported exactly like one whose guardrail was read and
found empty. The warning is the only thing distinguishing them. A
`ReasonTagsUnreadable` exclusion — the same shape as `ReasonModeOff` — would
make "could not look" a *verdict* rather than a footnote, and would be the
conservative direction: excluding an instance can only ever under-claim.

### 2.3 A failed collection still says what it learned

`buildRuntime` returns `nil` on error, so anything appended to
`runtime.Warnings` on the way to a failure was discarded. That is how an
operator ends up staring at one `AccessDeniedException` with no idea that the
run had *already* fallen back from a denied `GetMetricData` before dying of
something else. `rdsFailure(err, warnings)` appends them to the error, which is
the only channel that survives an aborted build
(`TestAFailedCollectionSaysWhatItHadAlreadyLearned`).

### 2.4 One region fails ⇒ the whole run fails

`--rds-region` is repeatable and one collector runs per region. If any region
fails hard, the run fails. That is deliberate: continuing would produce a
report covering three regions out of four, with the same shape, the same field
names and the same confident tone as a complete one. The alternative — a
partial report plus a warning — is defensible, but a warning is not the right
weight for "a quarter of your fleet is missing from these totals".

---

## 3. The parity seam: how it is enabled, and how its absence is surfaced

### 3.1 Enabling it

```
--rds-parity              read the modification envelope and run pkg/rds's parity engine
--rds-parity-rates PATH   verified provisioned-IOPS / provisioned-throughput rates
```

`--rds-parity` works on **both** sources. On `--rds-region` the envelope seam is
the same `*provider.RDSAPI` as the inventory seam. On `--rds-fixture` it is
`rds.EnvelopeFixture`, driven from three new `omitempty` fields on
`rdsFixtureFile` — `storageOptions`, `events`, `noEnvelopeAPI` — so the seam is
fully exercisable without an AWS account. `TestParityIsReachableOnTheLivePathToo`
asserts the two sources produce a byte-identical report over the same account.

The event window is **48 h**, hardcoded with the reason: the question the
events answer is "have there been four storage modifications in the last 24
hours", and a window that cannot contain 24 hours cannot rule one out —
`EnvelopeCollector` warns if you hand it a shorter one, and
`TestTheStorageModificationCooldownIsReadFromRecordedEvents` asserts that
warning is absent.

### 3.2 The ordering problem, and the holder that solves it

`rds.Config.Parity` is fixed when the domain is **constructed**; the parity
engine needs the envelopes, which can only be read once the inventory names the
instances — which happens after the domain is registered. So the domain is
registered holding `*rdsParitySeam`, an indirection, and every collected source
folds its envelopes in through `observe()`. Envelopes accumulate across sources
(`rds.NewEnvelopes` sorts and de-duplicates by identifier) and the engine is
rebuilt, because one domain can be fed several fixtures and several live
regions while `AssessParity` is called once, at report time, after all of them.

**The holder's nil branch returns a suppression, not `ok=false`.** This is the
subtlety worth recording. `StorageParity`'s third result means "I declined to
look at all", and the sizer reads it literally: `ok=false` emits **neither** a
proposal **nor** a suppression, and it does not fall back to the
`no-storage-performance-model` refusal either — that lives in the *else* branch
of `cfg.Parity != nil`. A holder that returned `ok=false` while unfilled would
therefore produce a report with a missing dimension and **no line saying so**,
which is precisely the failure this seam exists to prevent.

### 3.3 How its absence is surfaced

Three states, three different reports, and none of them is silence.

| State | What the report says |
|---|---|
| **no `--rds-parity`** | `rds.Config.Parity` is nil, and `pkg/rds`'s sizer refuses every assessed instance's storage with **`no-storage-performance-model`** — 4 of the shipped fixture's 27 refusals. A report that did not assess parity says so on every line it would have assessed. |
| **`--rds-parity`, no envelope** | `no-storage-performance-model` is gone and **`provisioning-envelope-unknown`** takes its place, plus a warning naming `DescribeValidDBInstanceModifications`. No ceiling is assumed. |
| **`--rds-parity` + envelope** | Real verdicts: `no-io-measurement`, `gp2-band-unpublished`, `storage-parity-not-cheaper`, `provisioned-performance-floors-at-baseline`, `storage-modification-cooldown`, or a proposal. |

`TestParityIsOptInAndItsAbsenceIsVisible` asserts all three transitions,
including that the flag is not decorative: with `--rds-parity` the
not-evaluated refusal must be **gone** and at least one parity-only code must
be **present**. Concretely, on the shipped account:

```
                                 without --rds-parity        with --rds-parity
  no-storage-performance-model    4 refused                   —
  no-io-measurement               —                           4 refused
  provisioning-envelope-unknown   —                           2 refused
  gp2-band-unpublished            —                           1 refused
  provisioned-performance-floors  —                           1 refused
```

### 3.4 The money gate

`pkg/rds` does the whole arithmetic either way — the **magnitude** is reported
whatever the rate says. What provenance decides is whether that magnitude may
be called a *saving*. `pkg/rds/FINDINGS.md` §7 could not retrieve the RDS gp3
storage or provisioned-performance rates from AWS, so every shipped figure is
`unverified` and the proposal is refused under `unverified-rate` with the
dollar figure attached — which is a strictly better report than
`no-storage-performance-model`, because it sizes the opportunity and names what
would unblock it.

Supplying `--rds-rates` (storage) **and** `--rds-parity-rates` (the two gp3
knobs) stamps both `operator-supplied` and unblocks the claim.
`TestParityReachesAProposalOnlyWithClaimableRates` runs both halves and asserts
the proposal that appears is advisory, never shrinks allocated storage, never
proposes below the striped regime's 12,000 IOPS / 500 MiB/s floor, and never
leaks into the actuatable recommendation stream.

**`--rds-parity-rates` gives the file no way to name its own provenance.**
`rds.LoadRates` stamps every loaded row `operator-supplied` and offers no
`provenance` key, for the reason that provenance is the single gate between
"this sizes an opportunity" and "this goes in a business case". `PerformanceRates`
*does* carry a `provenance` json tag, so decoding straight into it would let a
file promote a guess to a claim by typing the word `verified`. The cmd/-side
projection omits the field and `DisallowUnknownFields` rejects it
(`TestTheParityRatesFileCannotDeclareItsOwnProvenance`).

---

## 4. No live AWS call in any test

Nineteen new test functions. **Not one** reads a credential, opens `~/.aws`,
sets an `AWS_*` variable or touches a socket. Every live test installs a seam
set built from `pkg/rds`'s own `Fixture` and `EnvelopeFixture` through
`newRDSLiveSeams`, a package-level constructor seam restored by `t.Cleanup`, so
the collector, the pagination, the window clamp, the access-denied retry and
the envelope collection all run for real while the SDK is never constructed.

`provider.IsAccessDenied` matches a `smithy.APIError` rather than a string, so
the denial fakes implement that interface (`smithy-go` is already a direct
`go.mod` requirement; no module was added).

One test — `TestABlankLiveRegionIsRefusedBeforeAnyCredentialIsRead` — reaches
the **real** `dialRDS`, and it is deliberately the one case that provably
returns before `LoadDefaultConfig`: `NewRDSAPI` rejects a blank region first,
because `CollectorConfig.Region` stamps `DBInstance.Region` and therefore
selects the rate-card row that prices every instance. It clears the `AWS_*`
environment for the duration anyway.

A test that would try to reach the network on a developer laptop with a stale
profile is not a slow test; it is a failed unit.

---

## 5. What is still NOT reachable in `pkg/rds`, and why

### 5.1 The actuator — deliberately, and this unit must not change that

`pkg/rds/actuate*.go` is 2,800 lines of merged, tested code:
`StorageActuateAPI`, `NewActuator`, `ApprovedStep`, `NewApproval`,
`PlanFingerprint`, `BoundActuator`, `LedgerEntry`, `Summarize`, the preflight
and the refusal types. **None of it is reachable from the binary and none of it
was made reachable here.** This unit wires read-only paths; making an actuator
reachable is a separate, separately-approved decision.

Three independent walls stand in front of it and this unit added to none of
them and removed from none:

1. `Registry.PlanSteps` checks `Health` before the domain is consulted, and
   `pkg/rds`'s `Health` is unconditionally report-only.
2. `validateSteps` rejects a step labelled with another domain's kind.
3. `Registry.Execute` routes through the actuator table, which has no `rds` row.

There is also **no SDK adapter for the mutating side**. `pkg/provider`'s
`rdsSDK` interface is six GETs, and `TestNoMutatingSDKSurface` fails on any
method whose name begins with a mutating verb. So even a wiring mistake in
`cmd/` could not reach `rds:ModifyDBInstance` — there is nothing to reach.
`TestNoActuatorBecomesReachableThroughTheLivePath` asserts the CLI end of this
for a live, parity-enabled domain.

### 5.2 `AttributeStorageGrowth` — needs persistence between runs, which `kilter domains` has not got

Trap 8's ledger rule compares an instance's allocated storage against the
**last** snapshot's, because storage autoscaling moves the floor on its own and
leaves no CloudTrail event. `Target.PriorAllocatedStorageGiB` is filled by
`Domain.Observe` from the domain's remembered previous observation — but
`kilter domains` builds a fresh registry every invocation and never calls the
domain's `Checkpoint`/`Restore`. So `PriorAllocatedStorageGiB` is always 0, and
`AttributeStorageGrowth` always answers `StorageUnchanged`.

**Next job:** persist the RDS domain's checkpoint through `pkg/store` between
runs, keyed by scope. That is the same missing write side `WIRING-FINDINGS.md`
§6.2 and §6.3 both need, so the three jobs share most of their work. Wiring it
from `cmd/` alone is not possible: nothing in `kilter domains` writes state
today, and adding a store to it would be a new persistence surface rather than
a wiring change.

### 5.3 Collector knobs with no CLI surface

`CollectorConfig.MaxPages` and `PeriodSeconds`,
`EnvelopeCollectorConfig.MaxPages`, and `RDSAPI.SetCallTimeout` /
`CloudWatchAPI.SetCallTimeout` all take package defaults and no flag exposes
them. Defensible — each has a documented default with a reason — but an account
large enough to hit `DefaultMaxPages = 50` gets a truncation warning and no way
to raise the bound without rebuilding.

`rds.CollectedMetrics()` is exported "so an operator can size that bill without
reading the source", and nothing in the CLI prints it. One line in
`--rds-detail` would close that.

### 5.4 `pkg/domain/rds.Config` has no `Parity` field

Because of it, `newRDSDomain` builds the domain through `rds.NewDomain`
directly and re-wraps the result as `&domrds.Domain{Domain: d}` so
`UsageLines` still contributes to the account-wide commitment baseline. That
works and is tested, but it duplicates four lines of config mapping and would
silently skip any state a future `domrds.Domain` grew.

**One-field fix, in `pkg/domain/rds` (outside this unit's scope):** add
`Parity rds.StorageParity` to `domrds.Config` and assign it in `New`. `cmd/`
then calls `domrds.New` on both paths and the wrapper reconstruction goes away.

### 5.5 The `pkg/rds` decisions this wiring inherits and cannot fix

Carried forward from `RDS-ADAPTER-FINDINGS.md` §7, restated because they are
now reachable from the binary and therefore now affect real reports:

- **§7.1 `GetMetricData` continuation results are dropped, not merged.**
  `collect.go`'s page loop is first-write-wins per query ID, so a query whose
  data spans a page boundary keeps only the first page's datapoints — and,
  because that page's `StatusCode` is `Complete`, the series is **not** marked
  partial. At 11 queries per instance and a 100,800-datapoint response cap,
  one response holds about five series, so this is reachable on any account
  with more than a handful of databases. This is the highest-value `pkg/rds`
  fix on the list and it is one branch: append on a duplicate ID.
- **§7.2 `StorageEnvelope` cannot distinguish "no ceiling" from "unknown
  ceiling".** The adapter picks omission to keep the safe reading, so a
  partially-answered envelope produces a refusal that says the seam "was not
  read" when it *was* supplied. Misleading, in the conservative direction. A
  `MaxIOPSKnown bool` would fix it.
- **§7.3 `Envelope.Cooldown` silently ignores undated events**, which inverts
  the recogniser's own stated bias toward over-counting.
- **§7.4 an empty `StatusCode` defaults to `Complete`.** The adapter resolves
  it to `PartialData` on its side; the default belongs on the other side, or
  every future CloudWatch adapter re-derives the same fix.
- **§7.5 an unaddressable instance is dropped without a warning.** The adapter
  covers it with a `Note()` — which this unit renders — but the drop itself is
  still silent inside `pkg/rds`.

### 5.6 Smaller gaps this unit chose not to close

- **`--scope` is one string for every `--rds-region`.** Scope is
  `accountID/region`, so a two-region run stamps both regions' targets with one
  scope. Correct for the single-region case that is the norm; wrong-looking for
  a multi-region run. Deriving the scope per region needs the account ID, which
  means `sts:GetCallerIdentity` — a new IAM action, and not one this unit will
  add to a policy it is publishing as seven GETs.
- **No `--rds-no-metrics`.** `RDS-ADAPTER-FINDINGS.md` §6.3 offers it as the
  alternative to the retry: it costs one fewer wasted collection and makes the
  degradation an explicit statement rather than an inference. The retry was
  chosen because it needs nothing from the operator and produces the documented
  behaviour for a credential nobody described correctly. Both are honest; only
  letting the run die is not.
- **The parity engine's thresholds are not exposed.** `MinWindow`, `Headroom`,
  `MinConfidence` and `Percentile` stay at `pkg/rds`'s policy. A second set of
  thresholds in `cmd/` would be a second answer to a question that must have
  one.
