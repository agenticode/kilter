# U5 — plain-EC2 domain, read-only

`pkg/ec2` observes non-Kubernetes EC2 instances and reports what they should
be. It never changes one. Spec: `docs/design/compute-domains.md` §3.3, §4.4,
§4.5, §4.6, §5.2, §6 U5, §7.

**Status:** complete for the U5 scope, `go test -race ./...` green,
92.7 % statement coverage. No AWS SDK, no network call, no clock, no
package-level mutable state, no new module dependency, `go.sum` untouched.

---

## 1. What is here

| File | What it is |
|---|---|
| `ec2.go` | Package contract and core types: `TargetRef`, `Spec`, `Evidence`, `Suppression`, `Series`, `Instance`, `Target`, `Snapshot`, every reason code. Mirrors §5.2 field-for-field. |
| `collect.go` | The two cloud seams (`InventoryAPI`, `MetricsAPI`) and the `Collector` that turns them into a `Snapshot`. |
| `fixture.go` | `Fixture`: recorded API responses replayed through both seams, with real pagination, truncation and empty-account behavior. |
| `burst.go` | T-family credit analysis (§4.6): baselines, accrual, throttling vs surplus. |
| `sizer.go` | The pure decision core: demand, memory-blind rule, suppressions, confidence, Graviton advisory. |
| `report.go` | `Report`, `Totals`, `Report.Validate()` (the invariant checker), `WriteText`. |

### The seams

Two interfaces, two read operations, over plain Go structs:

```go
type InventoryAPI interface { DescribeInstances(ctx, *DescribeInstancesInput) (*DescribeInstancesOutput, error) }
type MetricsAPI   interface { GetMetricData(ctx, *GetMetricDataInput) (*GetMetricDataOutput, error) }
```

`pkg/provider` isolates aws-sdk-go-v2 behind an `asgAPI`-style interface and is
wired in `cmd/`. This package goes one step further and does not import the SDK
at all, because its decision path has to link into the air-gapped binary. The
struct fields track the API's field names, so a recorded fixture reads like the
response it came from and the SDK adapter is a mechanical field copy.

**There is no actuation surface.** No `Actuator`, no mutating method, no flag
that would enable one. `ActionClass` and `Risk` on a `Proposal` are labels for
U7 to read; nothing in this package dispatches on them.

---

## 2. Suppressions, and exactly where each one is tested

Every observed instance produces exactly one `Assessment`. An `Assessment` with
no `Proposal` always carries at least one `Suppression` — `Report.Validate()`
rejects a report where one does not. Each test below builds a fixture where
**exactly one** suppression fires, and `only()` fails if a second one does.

| Reason code | Fires when | Test |
|---|---|---|
| `k8s-tagged` | `kubernetes.io/cluster/*`, `eks:cluster-name` or `aws:eks:cluster-name` tag. The instance is a cluster node and belongs to the k8s-nodes pipeline, which sizes it against pod requests and eviction guardrails this domain cannot see. | `TestSuppressionK8sTaggedFiresAlone` (all three tag forms) |
| `guardrail-mode-off` | `kilter.dev/mode=off`, mirroring the Kubernetes annotation guardrail. | `TestSuppressionModeOffFiresAlone` |
| `memory-blind` | No memory series **and** the current-memory floor is what stopped a cheaper choice. See §3. | `TestMemoryBlindRefusesRatherThanShrinks`; contrast in `TestMemorySignalUnlocksTheDownsize`; non-firing case in `TestMemoryBlindDoesNotFireWhenItChangesNothing` |
| `commitment-negative` | `commit.Inventory.NetSavings` says the bill goes **up** (§4.4 ex.1, the +135 % case). Carries `ValidFrom` = the blocking commitment's expiry. | `TestSuppressionCommitmentNegativeFiresAlone` (asserts the date, and that the same change *is* proposed with no commitments) |
| `commitment-neutral` | The list-price saving is entirely absorbed by stranded commitment (§4.4 ex.2/ex.3). | `TestSuppressionCommitmentNeutralFiresAlone` |
| `burst-credit-depleted` | Standard-mode T instance with an exhausted credit balance. Observed CPU is a throttling ceiling, not demand. | `TestCreditDepletedInstanceIsNotDownsized` |
| `burst-evidence-missing` | Burstable, but no `CPUCreditBalance`, no encoded baseline, or an unknown credit mode. §7 trap 5 requires credit evidence in *both* directions. | `TestBurstEvidenceMissingFiresAlone`, `TestBurstUnknownCreditModeFiresAlone`, `TestUnverifiedBurstFamilyIsUnknownNotAssumed` |
| `burst-surplus-charged` | Unlimited-mode T instance already paying surplus, where the cheapest candidate is a *smaller burstable* shape — which lowers the baseline and buys more surplus. | `TestBurstSurplusIsPricedAndSmallerBurstableRefused` |
| `no-metrics` | No `CPUUtilization` datapoints at all. | `TestSuppressionNoMetricsFiresAlone` |
| `partial-metrics` | Any series incomplete: a non-`Complete` GetMetricData status, a truncated response, or an exhausted page budget. | `TestSuppressionPartialMetricsFiresAlone` |
| `insufficient-window` | Observed span < `MinWindow` (7 d). | `TestSuppressionInsufficientWindowFiresAlone` |
| `insufficient-samples` | Delivered datapoints < `MinSampleCoverage` (50 %) of what the window and period imply. Gaps are unobserved time, not idle time. | `TestSuppressionInsufficientSamplesFiresAlone` |
| `unknown-instance-type` | Not in the pricing catalog: no price ⇒ no bill delta ⇒ nothing to claim. | `TestSuppressionUnknownInstanceTypeFiresAlone` |
| `low-confidence` | Earned confidence below `MinConfidence`. The reason names the weakest factor, i.e. what would fix it. | `TestSuppressionLowConfidenceFiresAlone` |
| `undersized` | Demand already exceeds the current shape. Reported, never proposed — growing an instance costs money and that is not this unit's call. | `TestSuppressionUndersizedFiresAlone` |
| `no-cheaper-candidate` | Nothing was wrong and nothing was cheaper. Recorded so that "no output" is never silent. | `TestMemoryBlindDoesNotFireWhenItChangesNothing` |

Ordering is deliberate: ownership → priceability → evidence completeness →
evidence sufficiency → burst class → candidate search → confidence → the bill.
Each stage returns early, which is what lets every suppression fire alone.

### Collector-side tests

`TestCollectEmptyAccount` (an empty account is a normal answer, not an error
and not a nil snapshot), `TestCollectPaginatedInventoryAndMetrics` (3 inventory
pages × paged metrics, reassembled and time-sorted),
`TestCollectDetectsTruncatedMetricResponse`,
`TestCollectPropagatesPartialDataStatus`, `TestCollectRejectsNonAdvancingPager`,
`TestCollectPageBudgetDegradesToStale`, `TestCollectPropagatesTransportErrors`,
`TestCollectDeduplicatesAcrossPages`,
`TestCollectBatchesWithinTheGetMetricDataLimit` (620 instances → ≥ 3 calls, none
over 500 queries; the fixture rejects an over-large call so a batching
regression cannot pass quietly), `TestCollectDiscardsUnknownQueryIDs`,
`TestCollectRejectsMalformedResults`, `TestCollectHonorsContextCancellation`.

Recorded fixtures live in `testdata/`: `account-empty.json`,
`account-paginated.json` (5 instances across 3 pages: basic and detailed
monitoring, a stopped instance, a cluster-tagged node, an opted-out node),
`account-truncated.json`.

### Property and contract tests

- `FuzzSizerNeverUndersizes` — the load-bearing property: a proposal is never
  below an observed peak. Also asserts every `Report.Validate()` invariant on
  every input. 65 k executions clean in a 25 s smoke run.
- `TestValidateCatchesEachViolation` — 15 hand-corrupted reports, one per
  invariant, each required to be caught. A checker that passes everything
  proves nothing.
- `TestReportIsShuffleInvariant` — reverses target order, series order, tag
  order, catalog row order and commitment order; requires byte-identical JSON.
- `TestCollectorIsPageGroupingInvariant`, `TestAssessIsRepeatable`.
- `TestBurstTableMatchesPublishedRates` — AWS's published T3 credit rates,
  transcribed independently, against this package's *derived* values.
- `FuzzFixtureRoundTrip` — arbitrary bytes into the fixture loader.

---

## 3. What CloudWatch genuinely cannot tell you

### 3.1 Memory — there is none

Without the CloudWatch agent there is **no memory metric for EC2 at all**
(§3.3, [verified]). This is the trap the unit exists for (§7 trap 4): shrinking
a memory-bound instance on CPU evidence invites an OOM that the optimizer will
never be blamed for.

What we do:

1. `Observation.MemoryBlind` is a named state, set by the collector's declared
   blind spots and re-derived by the sizer.
2. In that state the memory floor is the instance's **current** memory. Only
   same-or-more-memory moves are eligible. `Report.Validate()` rejects any
   report where a memory-blind proposal reduces memory — the fuzz target
   re-asserts it independently.
3. The refusal is *counterfactual*, which is what makes it legible: the sizer
   also computes what it would have chosen with **no** memory floor, and when
   that is cheaper, it emits `memory-blind` naming the instance type it
   declined and the memory that choice would have removed. It does not quietly
   emit a smaller recommendation.
4. Confidence loses its whole 0.20 memory weight, and the report says how to
   fix it (install the agent, `mem_used_percent`).

Disk *space* is invisible for the same reason — EBS metrics are I/O, not fill —
so `"disk-space"` is a permanently declared blind spot. EBS is U6's problem;
this package never reasons about volume capacity.

### 3.2 Resolution — a peak you cannot see

Basic (free) EC2 monitoring publishes **one datapoint per 300 s**; 1-minute
datapoints require paid detailed monitoring ([verified] §3.3). The limit is
publication granularity, not the statistic: requesting `Maximum` at a 300 s
period buys nothing when only one datapoint exists per window.

**The rule this package applies, stated verbatim in every report**
(`ResolutionNote`, echoed into `Observation.ResolutionNote` and into
`WriteText`):

> 300-second datapoints (basic monitoring): CloudWatch publishes one value per
> 300 seconds, so a burst shorter than that is averaged away and CANNOT be
> recovered from this data — requesting the Maximum statistic does not help,
> because there is only one datapoint per window. Demand is inflated by 25 % to
> compensate and confidence is reduced; enable detailed monitoring for
> 60-second datapoints.

The 25 % (`CoarseResolutionHeadroom`) is a **stated safety margin, not a derived
truth**: a 5-minute average can hide an arbitrarily higher 1-minute peak, and no
multiplier recovers it. Confidence loses 60 % of its 0.15 resolution weight.

`TestMetricResolutionChangesTheObservedPeak` holds the underlying workload
fixed — 1 minute of 100 % CPU in every 20 — and feeds it both ways. At
1-minute resolution the peak is 100 %; averaged into 5-minute buckets it is
28 %, and the sizer, reasoning honestly from what it was given, proposes an
instance *half* the size. The test asserts the coarse proposal is below the
peak that really happened, and that the report says so. That is the cost of
basic monitoring, made visible rather than assumed away.

The collector enforces the same truth on the request side: an instance without
detailed monitoring is **always** queried at 300 s, even when the config asks
for 60 s, because a 60 s request against 300 s publication returns mostly-empty
buckets that read as a coverage failure rather than as the resolution limit
they are (`TestCollectClampsPeriodToPublicationGranularity`).

### 3.3 Truncation vs absence

GetMetricData answers *every* query it accepts, with an empty value list when
the metric has no data. So a query with **no result at all** means the response
was truncated — and the two must never be conflated, because absence of data is
evidence (that is exactly how memory-blind is detected) while absence of an
answer is not. The collector tracks answered query IDs, synthesizes an
explicitly `Truncated` series for the rest, marks the snapshot stale, and the
sizer refuses on `partial-metrics` rather than reading a dropped series as
"nothing was using it".

### 3.4 Throttling looks exactly like idleness

A T-family instance whose credit balance has hit zero is throttled to its
baseline. Its low CPU is a ceiling AWS imposed, not demand the workload
expressed, and sizing down from it lowers the baseline and throttles harder.
`AnalyzeBurst` separates the cases:

- `throttled` — standard mode, balance at/below 2 % of the accrual cap for
  ≥ 10 % of the window ⇒ **refuse**, plus an advisory pointing *upward*.
- `surplus` — unlimited mode with realized `CPUSurplusCreditsCharged` ⇒ the
  sticker is not the price; effective cost is reported and burstable downsizes
  are refused.
- `healthy` — below baseline with credits intact ⇒ the sticker covers it, and a
  downsize is allowed.
- `unknown` — no credit evidence, no encoded baseline, or unknown mode ⇒
  **refuse**.

`TestCreditDepletedInstanceIsNotDownsized` runs two t3.large instances with
*identical* CPU series and opposite credit balances: the healthy one downsizes
to t3.medium, the depleted one is refused. The credit balance, not the CPU, is
what decides.

Credit economics are **derived**, not transcribed: earn rate = baseline ×
vCPUs × 60 credits/h, accrual cap = 24 × earn rate, surplus = $0.05 per
vCPU-hour = $0.05/60 per credit. `TestBurstTableMatchesPublishedRates` checks
the derivation against AWS's published table for all six t3 sizes.

### 3.5 What no metric can tell you: architecture portability

Graviton is **advisory only** (§4.5). Binary, AMI, container-image and vendored
dependency compatibility is not observable from any metric, so no amount of
price evidence makes it applicable. Structurally enforced:

- `Advisory.Actuatable()` is a **method returning false**, not a field — no
  serialized form and no struct literal can claim otherwise.
- `Report.Validate()` rejects any advisory with an empty `Caveat`.
- Advisory money is totalled separately (`Totals.AdvisoryNetSavingsMonthlyUSD`)
  and never folded into `NetSavingsMonthlyUSD`.
- The $ delta is netted through the commitment waterfall, because family-scoped
  commitments do not follow an instance across families. Under an m5 RI the
  caveat says the list-price delta is **NOT a saving**
  (`TestGravitonAdvisoryStatesCommitmentCaveat`).

---

## 4. Money

Gross savings are the on-demand list-price delta — carried only so a UI can
show the fantasy beside the fact. The only number presented as a saving is
`commit.Assessment.ClaimableMonthlyUSD()`: a bill delta through
`pkg/pricing/commit`'s waterfall.

The before/after usage handed to the waterfall is **account-wide** — every
priced instance in the snapshot, not just the one being changed — because
Compute Savings Plans absorb usage account-wide and a partial view overstates
savings (§4.4 ex.3). Instances the catalog cannot price are omitted rather than
priced at zero, and the report warns.

Confidence is **earned, not lost**: it starts at 0 and adds sample coverage
(0.30) + window (0.20) + memory signal (0.20) + resolution (0.15) + burst
evidence (0.15). A memory-blind instance on basic monitoring tops out at 0.71
against a 0.65 floor — enough for a same-or-more-memory move, and nothing more.
Every factor carries the prose explaining what it earned and why.

---

## 5. Deliberately deferred

| Deferred | Why, and what would close it |
|---|---|
| **All actuation.** | U7. Nothing here can stop, modify or start anything. `Proposal.Action`/`Risk` and the instance-store `RiskHigh` flag are the pre-flight facts U7 needs; the pre-flight itself (ENA/NVMe, shutdown behavior, `DescribeImages`) is not implemented. |
| **EBS.** | U6 owns volumes: `DescribeVolumes`, gp2→gp3 parity, IOPS. This package requests no volume metrics and never reasons about storage. |
| **ASG-level targets.** | §3.3 sizes the launch template, not the instance. The collector does not read `DescribeAutoScalingGroups`/`DescribeLaunchTemplateVersions`; ASG members are assessed individually today, which is honest but not the right target. `aws:autoscaling:groupName` is *not* currently an exclusion — a follow-up should decide whether ASG members are suppressed here or routed to a template-level sizer. |
| **`DescribeInstanceTypes`.** | Instance shape comes from the pricing catalog, so a type absent from the catalog is a refusal (`unknown-instance-type`) rather than a live lookup. Consequently the **network and EBS baseline floors** §3.3 asks for are not enforced: the catalog carries no such fields. Adding them is a catalog schema change plus one filter in `Sizer.cheapest`. |
| **t2 and `*.nano` baselines.** | Only the t3 baselines are [verified] in §4.6. t2 uses a *different* table and defaults to standard mode. Rather than guess, `burstFamilies` recognizes t2 as credit-based while `burstBaselines` has no row for it, so every t2 instance lands in `unknown` and is refused. Closing it: one row in `t3Baselines`-style form plus one row in `TestBurstTableMatchesPublishedRates`. |
| **fixed→T recommendations.** | `Config.AllowFixedToBurstable` exists and defaults **off**. §4.6 requires the `pkg/patterns` class detector to confirm steady-low (not bursty-high) first, and this unit does not run it. |
| **Surplus-aware net savings.** | Whether a Savings Plan absorbs surplus credit charges is not documented in the sources §4.9 cites. Rather than guess in either direction, realized surplus is reported as an advisory with the effective-cost arithmetic and an explicit caveat that it is **not** netted through the waterfall. This under-claims; it never over-claims. |
| **Compute Optimizer cross-check.** | §3.3's optional disagreement signal. Would slot in as a third seam and a confidence factor. |
| **Spot.** | Advisory in v1 per §3.3; not implemented. |
| **Histogram/forecast/patterns reuse.** | The sizer computes percentiles directly from the delivered series rather than folding them into `pkg/histogram`. Correct for a stateless read-only report; a brain that *accumulates* across ticks should key `histogram.Histogram` by (`TargetRef`, metric) as §5.4 describes. That is a `Domain.Learn`/`Checkpoint` concern, and `Domain` does not exist yet. |
| **`pkg/domain` types.** | `pkg/domain` was being built in parallel and does not exist in this tree. Local types mirror §5.2 field-for-field so the adapter is mechanical — see §6. |

---

## 6. Exact wiring for a later unit

### 6.1 `pkg/domain` adapter

`TargetRef`, `Spec`, `Evidence`, `ActionClass` and `Snapshot` are §5.2's shapes
field-for-field. When `pkg/domain` lands, add `pkg/ec2/domain.go` (nothing in
this package needs to change):

```go
func (r TargetRef) ToDomain() domain.TargetRef {
    return domain.TargetRef{Domain: domain.EC2, Scope: r.Scope, ID: r.ID, Name: r.Name}
}

func (a Assessment) ToDomain() domain.Recommendation {
    rec := domain.Recommendation{
        Target: a.Target.ToDomain(), Current: a.Current.ToDomain(),
        CurrentHourlyUSD: a.CurrentHourlyUSD,
        Confidence:       a.Confidence.Score,
        Evidence:         toDomainEvidence(a.Evidence),
        Action:           domain.ActionAdvisory,      // U5 is report-only
    }
    if p := a.Proposal; p != nil {
        rec.Proposed, rec.ProposedHourlyUSD = p.Spec.ToDomain(), p.ProposedHourlyUSD
        rec.GrossSavingsMonthlyUSD = p.GrossSavingsMonthlyUSD
        rec.NetSavingsMonthlyUSD   = p.NetSavingsMonthlyUSD
        rec.Risk, rec.Reason       = p.Risk, p.Reason
    }
    if len(a.Suppressions) > 0 {
        rec.Reason    = a.Suppressions[0].Reason
        rec.ValidFrom = a.Suppressions[0].ValidFrom
    }
    return rec
}
```

`domain.Domain` is satisfiable by wrapping `Sizer`: `Kind()` → `domain.EC2`,
`Recommend(now, ledger)` → `Assess`, `Learn` → store the snapshot,
`Checkpoint`/`Restore` → JSON of the last snapshot. `PlanSteps` must return
**zero steps** until U7 exists — this domain has no executable action.

### 6.2 `kilter cloud-agent --domain ec2 --region <r>`

`cmd/` owns the SDK; this package must never import it.

1. **Adapter** (`cmd/kilter/ec2aws.go` or a new `pkg/ec2/collect` subpackage —
   either is fine, as long as the SDK import does not land in `pkg/ec2`):

   ```go
   type awsInventory struct{ c *ec2sdk.Client }
   func (a awsInventory) DescribeInstances(ctx context.Context, in *kec2.DescribeInstancesInput) (*kec2.DescribeInstancesOutput, error)
   ```

   - `DescribeInstances` → map `Reservations[].Instances[]` onto
     `kec2.InstanceRecord`. Required fields: `InstanceId`, `InstanceType`,
     `Architecture`, `Platform`/`PlatformDetails`, `Placement.Tenancy`,
     `Placement.AvailabilityZone`, `State.Name`, `LaunchTime`, `Tags`,
     `Monitoring.State` → `MonitoringState`,
     `len(BlockDeviceMappings ephemeral)` → `InstanceStoreVolumes`.
   - `CPUCredits` needs a second call:
     `ec2:DescribeInstanceCreditSpecifications` for the T-family instance IDs.
     **Leave it empty rather than defaulting to `unlimited`** — an unknown mode
     is a refusal, and a wrong default inverts the throttling decision.
   - `GetMetricData` → map `MetricDataQuery`/`MetricDataResult` one-to-one;
     `StatusCode` maps straight across. Copy the message list into
     `GetMetricDataOutput.Messages`.

2. **Collector**:

   ```go
   c, err := kec2.NewCollector(awsInventory{ec2c}, awsMetrics{cwc}, kec2.CollectorConfig{
       Scope: accountID + "/" + region, Region: region,
       Window: 14 * 24 * time.Hour,
       PreferredPeriodSeconds: kec2.PeriodDetailedSeconds, // clamped per instance
       CollectMemory: true,                                // absent ⇒ memory-blind, which is the point
   })
   snap, err := c.Collect(ctx, time.Now())   // the agent owns the clock
   ```

   IAM: the `KilterEC2Observe` statement in §3.3, minus the EBS and ASG actions
   this unit does not use. **Do not grant `KilterEC2Actuate`** — nothing here
   can use it.

3. **Push** `snap` to the brain on the same contract the K8s agent uses
   (`POST /v1/domains/ec2/snapshots`, §5.8). The snapshot is plain JSON.

4. **Brain side**:

   ```go
   inv, _ := commit.LoadInventoryFile(commitmentsPath)   // may be nil
   s, _  := kec2.NewSizer(catalog, kec2.DefaultConfig())
   rep   := s.Assess(now, snap, inv)
   if err := rep.Validate(); err != nil { /* a bug in pkg/ec2 — fail loudly */ }
   ```

   Call `Validate()` before persisting or serving. It is cheap and it is the
   difference between a bug and a wrong invoice.

### 6.3 Storage and API (§5.8, additive)

- bbolt bucket `domain/ec2/<scope>/report` ← `json.Marshal(rep)`. Existing
  buckets untouched.
- `GET /v1/domains/ec2/recommendations` ← `rep.Assessments`.
  `POST /v1/domains/ec2/snapshots` ← collector push.

### 6.4 UI

The savings card reads `Totals` and must keep three numbers **visually
distinct**, because collapsing them is how an optimizer lies:

- `NetSavingsMonthlyUSD` — claimable, actuatable (once U7 exists).
- `GrossSavingsMonthlyUSD` — the list-price fantasy, shown beside it.
- `AdvisoryNetSavingsMonthlyUSD` — real money that needs a human decision
  (an architecture port, a credit-mode change). Never added to the first.

`Totals.SuppressedByCode` drives a "what we declined to do, and why" panel —
the refusals are the most useful thing in this report, not a footnote.
`Totals.MemoryBlind` / `Totals.CoarseResolution` are the two "install this and
we can tell you more" calls to action. `Report.WriteText` is the reference
rendering for the CLI.

### 6.5 Two contract notes for the integrator

- `Collector.Collect(ctx, now)` takes `now`; §5.3 sketches `Collect(ctx)`. This
  package has no clock by rule, so the caller supplies one. The `domain.Collector`
  adapter closes over the agent's clock.
- The `Domain.Recommend(now, ledger)` signature in §5.3 takes a `*commit.Ledger`.
  This package takes a `*commit.Inventory` (which is what `pkg/pricing/commit`
  actually exports) and does the account-wide before/after construction itself.
