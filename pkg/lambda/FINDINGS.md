# U9 — Lambda advisory domain

`pkg/lambda` observes AWS Lambda functions and reports what their memory
setting should be. It never changes one. Spec: `docs/design/compute-domains.md`
§4.8, §5.2, §6 U9, §7.

**Status:** complete for the U9 scope. `gofmt` clean, `go vet ./...` clean,
`go build ./...` clean, `go test -race -short ./...` green, 88.7 % statement
coverage in this package. No AWS SDK, no network call, no clock, no
package-level mutable state, no new module dependency, `go.mod`/`go.sum`
untouched.

---

## 1. Why this unit is mostly refusals

Lambda bills **GB-seconds**: `cost = memory × duration`. Memory is also the only
performance knob, because CPU is allocated *proportionally to memory* (≈ 1 vCPU
near 1,769 MB, §4.8). So the recommendation every naive optimizer makes —
"max memory used is 74 MB, drop 512 MB → 128 MB, save 75 %" — is wrong roughly
half the time. At a quarter of the CPU the duration can more than quadruple and
the bill goes **up**.

The duration response to memory is a property of the function's *code*. It
appears in no metric AWS publishes, it cannot be derived from a single operating
point, and no amount of statistics at one memory setting recovers it. This
package therefore has exactly one load-bearing rule, and it is encoded as a
refusal rather than as a caveat string a UI might not render:

> **No saving is claimed at a proposed memory setting without a MEASURED
> duration AT that setting.**

Three places enforce it, so it cannot be lost to a refactor:

1. `sizer.go` step (9): fewer than two adequately-measured settings ⇒
   `single-memory-point`, cost effect **UNKNOWN**, nothing claimed.
2. `sizer.go` step (11) `compare()`: only measured points are ever compared.
   Nothing is interpolated, extrapolated, or modelled.
3. `report.go` `validateProposal()`: a `Proposal` is structurally invalid unless
   `Observation.PointAt(p.MemoryMB)` exists, its warm-sample count equals
   `p.MeasuredSamples`, and its mean billed duration equals `p.MeasuredBilledMS`.
   `Report.Validate()` is called by every test, by both fuzz targets, and is the
   right thing for a later unit to call before persisting or serving a report.

## 2. What is here

| File | What it is |
|---|---|
| `lambda.go` | Package contract and core types: `Function`, `Target`, `Snapshot`, `Series`, `Suppression`, `Advisory`, `Window`, every reason code. Uses `domain.TargetRef`/`Spec`/`Evidence`/`ActionClass` directly — pkg/domain exists now, so nothing is duplicated. |
| `parse.go` | The REPORT log-line parser and its drop reason codes. |
| `stats.go` | `MemoryPoint` aggregation (one per measured memory setting) and `Observation`. |
| `cost.go` | The GB-second cost model, embedded rates, `MemoryFloorMB`, commitment usage lines. |
| `sizer.go` | The pure decision core: gates, suppressions, comparison, confidence, advisories. |
| `report.go` | `Report`, `Totals`, `Report.Validate()` (the invariant checker), `WriteText`. |
| `collect.go` | Three read-only cloud seams and the bounded `Collector`. |
| `fixture.go` | `Fixture`: recorded responses replayed through all three seams, with real pagination, truncation and empty-account behaviour, plus REPORT-line builders. |
| `domain.go` | The `domain.Domain` adapter — the read-only half of the seam. |

### The seams

```go
type InventoryAPI interface { ListFunctions(ctx, *ListFunctionsInput)   (*ListFunctionsOutput, error) }
type LogsAPI      interface { FilterLogEvents(ctx, *FilterLogEventsInput) (*FilterLogEventsOutput, error) }
type MetricsAPI   interface { GetMetricData(ctx, *GetMetricDataInput)   (*GetMetricDataOutput, error) }
```

Three interfaces, three **read** operations, over plain Go structs whose field
names track the API's, so a recorded fixture reads like the response it came
from and the SDK adapter is a mechanical field copy. `logs:FilterLogEvents`
rather than `logs:StartQuery`/`GetQueryResults` on purpose: it is synchronous,
bounded by `ctx`, and leaves no running Insights query behind on cancellation.

`Tags` and `ProvisionedConcurrency` ride on `FunctionRecord` even though AWS
serves them from `lambda:ListTags` and
`lambda:ListProvisionedConcurrencyConfigs` — one seam instead of three, filled by
the adapter.

**There is no fourth seam.** `lambda:UpdateFunctionConfiguration` has no
representation anywhere in this package; `TestNoMutatingAPISurface` scans the
package's identifiers to keep it that way, and `TestNoActuationSurfaceExists`
asserts `*Domain` does not satisfy `domain.Actuator`.

## 3. Evidence intake: the REPORT line

```
REPORT RequestId: 8f5…	Duration: 12.34 ms	Billed Duration: 13 ms	Memory Size: 512 MB	Max Memory Used: 74 MB	Init Duration: 143.21 ms
```

This is a log format, not an API, and it is the *only* place AWS publishes
`Max Memory Used` — and the only place the memory setting a **past** invocation
ran at survives. That last part is what makes multi-point evidence possible at
all: a function retuned during the window, or deliberately power-tuned, yields
two measured operating points from its own log group.

`ParseReport` is written for adversarial input:

- **Longest-first label matching with consumed byte ranges.** `Duration:` is a
  substring of `Billed Duration:`, `Init Duration:` and `Restore Duration:`. A
  naive scan assigns the init time to the warm duration — a plausible, silent,
  completely wrong number. Pinned by
  `TestParseReportDoesNotConfuseDurationLabels`.
- **Tabs and spaces both work**; units may be attached (`512MB`) or separated.
- **Unknown fields are ignored, not fatal** (`XRAY TraceId`, `SegmentId`,
  `Sampled`, `Status`, and whatever AWS adds next).
- **First occurrence wins** on a duplicated label.
- **Malformed input is DROPPED with a reason code, never coerced.** Drop codes:
  `not-a-report-line`, `missing-field`, `malformed-number`,
  `implausible-memory`, `implausible-duration`, `inconsistent-record`. Drops are
  aggregated by code with one truncated sample line and surface in the report:
  a dropped line is reported, never absorbed.

Plausibility checks reject rather than clamp — a clamped value is
indistinguishable from a measured one downstream: memory outside 128–10,240 MB,
`Max Memory Used` above the configured size, any duration beyond the 900 s
platform maximum, and `Billed Duration` more than 1 ms below `Duration` (billing
rounds **up**, so it can never be materially lower).

**Init duration.** A record with `Init Duration` (or SnapStart
`Restore Duration`) is *cold*. Cold records never contribute to warm duration
statistics — `MemoryPoint.MeanBilledMS`, `P95BilledMS`, `MaxBilledMS` and
`MeanDurationMS` are warm-only; init time is aggregated separately as
`MeanInitMS`. This is not tidiness: two memory settings with different
cold-start mixes would otherwise look like they had different durations when
only the mix differed, inventing a cost difference memory had nothing to do
with. Pinned by `TestInitDurationIsSegregatedFromWarmDuration`.

**Fuzz.** `FuzzParseReport` asserts the parser never panics, that every refusal
carries a code and yields the zero record, and that any record it *does* return
is plausible: no NaN, no ±Inf, no negative duration or memory, nothing outside
the platform limits, and a finite derived `GBSeconds`. 8.3 M executions, no
crashers. `FuzzReportInvariants` fuzzes whole reports and re-asserts the core
rule as a property.

## 4. Memory floor

`MemoryFloorMB(maxUsed, headroom, step)` = `ceil(maxUsed × 1.25)` rounded up to
a 64 MB step and clamped to `[128, 10240]`. Lambda accepts 1 MB steps;
recommending 137 MB is technically valid and operationally absurd, so the
default is coarser than the platform. `TestMemoryFloorMath` covers the grid
including clamping in both directions and garbage input.

The peak is the max **across every measured setting**, so a measurement taken at
a lower setting still raises the floor — the safe direction
(`TestMemoryFloorComesFromTheREPORTFixtures`).

**A function whose `Max Memory Used` sits at its configured ceiling may have
been TRUNCATED, not fitted.** The platform cannot report a value above the
limit, so that measurement is a *lower bound* on demand, not the demand. Sizing
down from a truncated peak is an out-of-memory error with a savings estimate
attached. Handling:

- Per-record: `ReportRecord.AtCeiling(ratio)`, default ratio 0.98.
- Per-setting: `MemoryPoint.AtCeiling` — such a point is never a proposal target
  (`validateProposal` rejects it).
- Per-function: `Observation.AtCeiling` (the *current* setting was truncated) ⇒
  the `memory-at-ceiling` refusal plus the `memory-possibly-truncated` advisory.
- Truncated measurements still feed the floor, because a lower bound can only
  push the floor up.

## 5. Every suppression, and exactly where it is tested

Every observed function produces exactly one `Assessment`. An `Assessment` with
no `Proposal` always carries at least one `Suppression` — `Report.Validate()`
rejects a report where one does not. Each test below builds a fixture where
**exactly one** suppression fires; `onlySuppression()` fails if a second one
does, and asserts the code and that its prose is non-empty.

| Reason code | Fires when | Test |
|---|---|---|
| `guardrail-mode-off` | `kilter.dev/mode=off` tag, mirroring the Kubernetes annotation guardrail. | `TestSuppressionModeOffFiresAlone` (also asserts no advisories leak out) |
| `unknown-configuration` | Configured memory outside 128–10,240 MB: no current bill to compare against, no valid setting to propose. | `TestSuppressionUnknownConfigurationFiresAlone` |
| `provisioned-concurrency` | Provisioned concurrency configured. A **different billing model** — per-GB-hour for kept-warm environments plus a discounted duration rate — so this package's on-demand GB-second arithmetic does not describe the bill at all. | `TestSuppressionProvisionedConcurrencyFiresAlone`; detected from the metric as well in `TestProvisionedConcurrencyIsAlsoDetectedFromTheMetric` |
| `no-report-evidence` | No REPORT line survived parsing. Max-memory-used exists nowhere else; CloudWatch's `Duration` metric cannot substitute because it says nothing about memory. The reason distinguishes *dropped* from *absent*. | `TestSuppressionNoReportEvidenceFiresAlone`, `TestNoReportEvidenceDistinguishesDroppedFromAbsent`, `TestCollectorDegradesWhenLogsAreUnreadable`, `TestSnapshotGenericIsLossyAndSaysSo` |
| `insufficient-window` | Surviving records span less than `MinWindow` (24 h). A slice of a log group has not seen the memory peak. | `TestSuppressionInsufficientWindowFiresAlone` |
| `insufficient-invocations` | Fewer than `MinInvocations` (1,000) over the window. A handful of samples is an anecdote. | `TestSuppressionInsufficientInvocationsFiresAlone` |
| `cold-start-dominated` | Cold share above `MaxColdShare` (20 %). Warm statistics stop describing the bill, and a memory change moves init time too — in a direction this data cannot show. | `TestSuppressionColdStartDominatedFiresAlone` |
| `memory-at-ceiling` | The current setting's max-memory-used reached the configured memory: possible truncation. | `TestSuppressionMemoryAtCeilingFiresAlone` |
| **`single-memory-point`** | Fewer than two settings carry ≥ `MinSamplesPerPoint` (200) warm invocations. **The core refusal:** floor and risk are reported, the cost effect is UNKNOWN, nothing is claimed. | `TestSingleMemoryPointIsARefusalNotACaveat`, `TestRecommendationsCarryTheSuppressionAndNoMoney` |
| `no-measurement-at-current-setting` | ≥ 2 settings measured, but not the one the function runs on now: the **current bill** is unmeasured, so every delta from it would be a guess. | `TestSuppressionNoMeasurementAtCurrentFiresAlone` |
| **`lower-memory-costs-more`** | A smaller setting *was* measured and measured **more expensive** — the GB-second trap. Refused as a downsize, reported as a cost increase. | `TestLoweringMemoryThatRaisesTheBillIsRefusedAndReportedAsAnIncrease` |
| `cheapest-measurement-below-floor` | A cheaper measured setting exists but is below the memory floor. A cheaper setting the function cannot fit in is not a saving. | `TestMemoryFloorComesFromTheREPORTFixtures` |
| `no-cheaper-measured-setting` | Nothing measured beats the current setting. | `TestSuppressionNoCheaperMeasurementFiresAlone`, `TestRecommendProducesNothingWhenNothingWasContemplated` |
| `low-confidence` | Earned confidence below `MinConfidence`; the refusal names the weakest factor. | `TestSuppressionLowConfidenceFiresAlone` — the fixture parses 1,400 REPORT lines against 2 M CloudWatch invocations, so `report-coverage` earns ≈ 0, and the test raises the gate to 0.75 to put that loss on the wrong side of it |
| `commitment-negative` / `commitment-neutral` | The commitment waterfall says the GB-second reduction strands committed spend (§4.4, §7 trap 1). Compute Savings Plans cover Lambda duration account-wide. | `TestCommitmentWaterfallCanSuppressAMeasuredSaving` (asserts the same change *is* proposed with no ledger) |

Order matters and is deliberate — ownership, then billing model, then evidence
existence, then evidence quality, then truncation, then the cost rule, then
confidence, then the bill. Each gate returns, which is what makes "fires alone"
achievable and each reason legible.

### Advisories (reported, never actuated, always caveated)

`Report.Validate()` rejects an advisory with no `Caveat`, and `Advisory.Actuatable()`
is a method returning `false` so no serialized form or struct literal can claim
otherwise.

| Code | What it says | Test |
|---|---|---|
| `arm-migration` | arm64 bills ~20 % less per GB-second. The figure is a **RATE delta at UNCHANGED duration**, never a saving: Graviton is a different CPU, the duration there is unmeasured, and duration is half of a GB-second bill. Caveat names the portability blockers (native modules, vendored binaries; for `PackageType: Image`, the entire image). | `TestARMAdvisoryIsARateDeltaNotASaving`, `TestARMAdvisoryStrengthensTheCaveatForContainerImages`, `TestARMAdvisoryIsAbsentForFunctionsAlreadyOnGraviton` |
| `power-tuning-trial` | The honest output of a single-point observation: not a recommendation, an experiment. Names the memory to publish, the sample count to reach, and states that nobody — including this tool — knows what it costs until it runs there. | `TestSingleMemoryPointIsARefusalNotACaveat` |
| `memory-possibly-truncated` | The measurement is a LOWER BOUND on demand. | `TestSuppressionMemoryAtCeilingFiresAlone` |
| `measured-cost-increase` | The naive recommendation, measured and refuted. Carries a **negative** delta by construction. | `TestLoweringMemoryThatRaisesTheBillIsRefusedAndReportedAsAnIncrease` |
| `under-provisioned-memory` | The floor exceeds the configured memory. Raising memory costs money, and spending it is not this unit's call. | `TestUnderProvisionedMemoryIsAdvisoryOnly` |

Advisory money is totalled separately (`Totals.AdvisoryRateDeltaMonthlyUSD`) and
never summed with `NetSavingsMonthlyUSD`. An advisory is not a plan.

## 6. What the GB-second model genuinely cannot tell you from one memory setting

Given `cost = requests × (request_charge + memory × duration × rate)` and a
window of observations at exactly one `memory`:

| Question | Answerable from one point? | What this package does |
|---|---|---|
| What does this function cost today? | **Yes** — memory, billed duration and invocation count are all measured. | Reported as `CurrentHourlyUSD`/`CurrentMonthlyUSD`, gated on `CostKnown`. |
| How much memory does it need? | **A lower bound**, from max-memory-used + headroom. | `MemoryFloorMB`, reported as evidence and as the power-tuning candidate. |
| Is the memory measurement even valid? | Only if it is not at the ceiling. | `memory-at-ceiling` refusal + truncation advisory. |
| What would it cost at a *different* memory? | **No.** Duration at that setting is a property of the code: CPU-bound work speeds up roughly linearly (GB-seconds flat or falling — more memory can be *free speed*), I/O-bound work does not (cost rises linearly with memory), and most real functions are somewhere between, with a plateau where the runtime stops parallelising. Nothing in CloudWatch distinguishes these. | `single-memory-point` refusal. No number is emitted — not a range, not an estimate, not a "likely". |
| Would Graviton be cheaper? | **No** — the rate is 20 % lower but the duration is unmeasured. | `arm-migration` advisory, explicitly a rate delta, excluded from claimed savings. |
| Is the saving real after commitments? | Only with an account-wide view. | `domain.Netter` / `pkg/pricing/commit` waterfall; `commitment-*` suppressions. |

The deliberate consequence: on a fleet that has never been power-tuned, this
domain proposes **nothing** and refuses everything with `single-memory-point`.
That is the correct output, and the report says so in a headline count
(`Totals.SinglePoint`, rendered by `WriteText` as "*N* of *M* functions have
only one measured memory setting: their cost effect is UNKNOWN"). The unit earns
its keep the moment a second setting exists — from a retune, a rollback, or a
deliberate trial — and then the claim is a comparison of two *observed* bills.

## 7. Determinism

- No clock anywhere: callers pass `now`; `TestNoClockReads` scans the source for
  `time.Now(`/`time.Since(`/`time.Until(`.
- No package-level mutable state: `TestNoUnexpectedPackageState` walks the AST
  and allows exactly `reportLabels` and `collectedMetrics`, both fixed tables.
- Every iteration order is sorted by an intrinsic key.
  `TestReportIsShuffleInvariant` reverses targets, log events, metric series,
  metric datapoints and warnings, renders both JSON and text, and requires
  byte-identical output — running the forward case twice, because Go randomizes
  map order within a single process.
- `TestCheckpointIsDeterministic` pins the persisted form.

## 8. The domain seam, and one honest limitation

`*Domain` implements `domain.Domain`. `PlanSteps` returns `domain.ErrReportOnly`
**unconditionally** — not a stub, a structural refusal — and `Health.ReportOnly`
is always true. `TestPlanStepsAlwaysRefuses` tries three guard configurations;
`TestRegistryRefusesStepsForThisDomain` goes through `domain.Registry`.

`Recommend` projects assessments into `domain.Recommendation` with
`ActionAdvisory`. Suppressed recommendations stay **visible** with their
`SuppressCode` (§5.7) and claim exactly zero. A refusal with no specific
alternative on the table produces no `Recommendation` at all — one whose
`Proposed` equals its `Current` is not a recommendation and
`domain.Recommendation.Validate()` says so — and lives in `Domain.Report()`,
which is the complete record.

**The limitation, stated plainly.** There are two ingest paths:

- `Domain.Observe(*lambda.Snapshot)` — the native path, carrying per-invocation
  REPORT records. This is the decision path.
- `Domain.Learn(*domain.Snapshot)` — the generic seam.

`domain.Snapshot` **cannot carry REPORT records**. A `domain.Sample` is one
`(metric, float64, timestamp)` triple; a REPORT record is four numbers whose
*correlation* is the entire point — the memory setting an invocation ran at, and
the duration it took **there**. Splitting one record into three samples discards
that correlation, and without it every multi-point comparison in this package —
which is to say every cost claim — becomes impossible.

So `Learn` ingests what the generic seam genuinely carries (inventory, tags,
provisioned concurrency, aggregate CloudWatch samples) and a function learned
only that way refuses with `no-report-evidence`. It never erases REPORT evidence
a prior `Observe` delivered (`TestLearnDoesNotEraseNativeEvidence`).
`Snapshot.Generic()` projects the other direction for the brain's account-wide
inventory view and declares the blind spot it creates in `Target.Blind`.

The fix belongs in `pkg/domain` — an opaque per-domain payload on `Snapshot`,
or a typed `Evidence` union — and U9 may not edit that package. **A later unit
should add it and collapse the two paths into one.**

## 9. Deliberately deferred

- **Actuation.** `lambda:UpdateFunctionConfiguration` with published versions and
  an alias for instant rollback is the U9 spec's "later, gated" item. Nothing
  here reaches it.
- **Automated power-tuning.** The `power-tuning-trial` advisory *names* the
  experiment; running it means publishing a version, shifting traffic and
  waiting — actuation plus a traffic-management surface, both out of scope.
- **Ephemeral storage (`/tmp`) above 512 MB**, which bills separately.
  `Function.EphemeralStorageMB` is collected and carried so the report can say
  the line exists; it is not priced. Adding it without measuring its use would
  put an unmeasured term in a claimed bill.
- **Free tier** (1 M requests + 400,000 GB-s/month). Not modelled: it is
  account-wide, shared across every function, and applying it per-function would
  under-report every bill. Relative deltas are unaffected.
- **Lambda@Edge and SnapStart pricing.** SnapStart `Restore Duration` is parsed
  and classified as cold, but its separate restore/cache charges are not priced.
- **`compute-optimizer:GetLambdaFunctionRecommendations`.** A second opinion
  worth cross-checking against, but it is a network call and a new IAM grant;
  the seam for it belongs with the SDK adapter.
- **Per-alias / per-version analysis.** Everything is aggregated per function.
  A function whose alias split traffic across two memory settings is exactly the
  multi-point case this package wants, and it already reads it correctly from
  `Memory Size`; attributing points to aliases is cosmetic and was skipped.
- **`Fixture` JSON round-trip** (pkg/ec2 has one). The fixture is built in Go by
  the tests; recording real account responses to `testdata/` is the natural
  next step and needs no code change.

## 10. Exact wiring a later unit must do

### `cmd/` — the SDK adapter (the only place that links aws-sdk-go-v2)

Implement the three seams over the SDK. Every struct field name tracks the API's,
so each method is a mechanical copy:

```go
// cmd/kilter/lambdaaws.go (new file, aws-sdk-go-v2 lives here and nowhere else)
type awsLambda struct {
    fns  *awslambda.Client
    logs *cwlogs.Client
    cw   *cloudwatch.Client
}

func (a *awsLambda) ListFunctions(ctx context.Context, in *lambda.ListFunctionsInput) (*lambda.ListFunctionsOutput, error)
    // → awslambda.ListFunctions (paginate with Marker/NextMarker), then for each
    //   function: awslambda.ListTags (→ Tags) and
    //   awslambda.ListProvisionedConcurrencyConfigs (→ ProvisionedConcurrency,
    //   summed across aliases/versions), and GetFunctionConfiguration for
    //   EphemeralStorage when ListFunctions omits it.

func (a *awsLambda) FilterLogEvents(ctx context.Context, in *lambda.FilterLogEventsInput) (*lambda.FilterLogEventsOutput, error)
    // → cwlogs.FilterLogEvents{LogGroupName, StartTime/EndTime as epoch millis,
    //   FilterPattern: lambda.ReportFilterPattern, NextToken}. Map
    //   OutputLogEvent.Timestamp (millis) → LogEvent.Timestamp,
    //   .Message → .Message. Do NOT pre-filter or trim the message.

func (a *awsLambda) GetMetricData(ctx context.Context, in *lambda.GetMetricDataInput) (*lambda.GetMetricDataOutput, error)
    // → cloudwatch.GetMetricData. Copy Id verbatim in BOTH directions: the
    //   collector routes results by ID, never by position or label.
```

Least-privilege IAM (read-only; no mutating action is needed or wanted):

```json
{
  "Effect": "Allow",
  "Action": [
    "lambda:ListFunctions",
    "lambda:GetFunctionConfiguration",
    "lambda:ListTags",
    "lambda:ListProvisionedConcurrencyConfigs",
    "logs:FilterLogEvents",
    "cloudwatch:GetMetricData"
  ],
  "Resource": "*"
}
```

`logs` and `cloudwatch` are optional: `NewCollector` accepts `nil` for either,
and every affected function refuses honestly
(`TestCollectorRequiresAnInventoryAndAWindow`).

Then wire the loop — note `now` and the window are passed in, never read here:

```go
now := clock.Now()
win := lambda.Window{Start: now.Add(-14 * 24 * time.Hour), End: now}

ccfg := lambda.DefaultCollectorConfig(win)
ccfg.Scope, ccfg.Region = accountID+"/"+region, region
coll, err := lambda.NewCollector(adapter, adapter, adapter, ccfg)

dcfg := lambda.DefaultConfig()
dcfg.Scope, dcfg.Region = ccfg.Scope, region
dom, err := lambda.NewDomain(dcfg)
_ = registry.Register(dom)            // domain.Registry, explicit wiring

snap, err := coll.Collect(ctx)        // the ONLY call that touches the network
_ = dom.Observe(snap)                 // native path — carries REPORT records
                                      // (Learn(snap.Generic()) is the lossy path; do not use it here)

rep := dom.Report(now, ledger)        // ledger is a *domain.Ledger or nil
_ = rep.Validate()                    // fail loudly rather than serve a broken report
_ = rep.WriteText(os.Stdout)
```

Persistence uses the seam's own contract: `dom.Checkpoint()` into `pkg/store`
under a `lambda/<scope>` key, `dom.Restore(blob)` on start.

Rates live in this package as constants
(`RequestUSDPerMillion`, `X86GBSecondUSD`, `ARMGBSecondUSD`) only because U9 may
not edit `pkg/pricing`. **A later unit should move them into
`pkg/pricing/catalog.json` (or a sibling `lambda.json`) beside the Fargate rate
table**, add a `pricing.LambdaRates()` accessor, and have `cmd/` pass
`Config.Rates` from it so `kilter pricing sync-aws` keeps them current. The
override point already exists: `Config.Rates` is a plain struct.

### UI / `/insights`

The report is already shaped for it. Three rules the UI must not break:

1. **Never show `AdvisoryRateDeltaMonthlyUSD` in the same total as
   `NetSavingsMonthlyUSD`.** They are separate fields for a reason: one is
   measured, one is a rate delta at an unmeasured duration.
2. **Render `Advisory.Caveat` wherever `Advisory.Message` is rendered.** A
   caveat that is one click away reads as an actionable saving; `Validate()`
   guarantees the caveat exists, the UI must guarantee it is seen.
3. **Show refusals.** `Totals.SuppressedByCode` drives a "what we declined to do,
   and why" panel without walking every assessment;
   `Totals.SinglePoint` is the headline for a fleet that has never been tuned,
   and the `power-tuning-trial` advisory is the call to action beside it.

`Assessment.CandidateMemoryMB` is what makes a refusal legible: it is the
alternative the sizer actually contemplated, present even when nothing was
proposed, so the UI can say "we looked at 512 MB and here is why we will not
call it cheaper."

Every function is `ActionAdvisory`. There is no "Apply" button to render.
