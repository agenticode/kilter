# U8 — ECS-on-Fargate domain (`pkg/ecs`)

Implements docs/design/compute-domains.md §6 **U8**: service-level observer over
ECS's default CloudWatch metrics, Fargate engine reuse, task-definition
right-sizing, Spot and ARM advisories, and the register-revision +
`UpdateService` actuator with revision rollback — all behind hard gates.

Stdlib + intra-repo imports only. No AWS SDK, no network call, no `time.Now()`,
no package-level mutable state. `go.mod`/`go.sum` untouched.

```
pkg/ecs/ecs.go       package doc, constants, reason/advisory codes, task-size parsing,
                     AbsoluteFromPercent (the conversion), mode-tag resolution
pkg/ecs/price.go     Arch × Market platform, ECS rates, RoundUpTier (quantizer reuse)
pkg/ecs/collect.go   the three read seams, Series, Observation, Snapshot, Collector
pkg/ecs/sizer.go     Config, Demand, Proposal, Assessment, Assess, advisories
pkg/ecs/report.go    Report, Validate, Recommendations projection, PlanSteps
pkg/ecs/actuate.go   MutateAPI, Refusal, Actuator (Execute / Revert)
```

---

## 1. What was built

### 1.1 The observer

Three read seams shaped after the AWS calls, over plain Go structs
(`collect.go`): `InventoryAPI` (`ListServices` / `DescribeServices` /
`DescribeTaskDefinition`) and `MetricsAPI` (`GetMetricData`). The write seam
(`MutateAPI`) is a *separate interface* so a collector can be handed credentials
that physically cannot register a task definition — the observe/actuate IAM
split of §3.3 expressed in Go.

The metrics seam queries exactly ECS's default, free, automatic service metrics:
`AWS/ECS` `CPUUtilization` and `MemoryUtilization`, dimensioned by
`ClusterName`+`ServiceName`, at the 60-second publication period, `Average`.
Container Insights is not used (v1 is service-level, per §3.4).

**The central computation.** Those metrics are a *percentage of the task
definition's reservation*, not absolute usage. Absolute demand is

```
used = percent / 100 × reserved
```

`AbsoluteFromPercent` (`ecs.go`) is the only conversion in the package, and the
`Reservation` it multiplies against carries the task-definition **revision** it
came from, so a percentage can never be silently converted against a
reservation that never applied.

### 1.2 Fargate engine reuse

`RoundUpTier` (`price.go`) does not own a tier table. It calls
`pricing.Quantize` — the shipped U1 quantizer — with the EKS-only +256 MiB
Kubernetes overhead cancelled out of the input:

```
Quantize(need − pricing.FargateOverheadBytes) ≡ roundUpToTier(need)
```

exactly for `need ≥ overhead`, and both land on the smallest tier below it. ECS
tasks have no kubelet/kube-proxy/containerd, so they have no overhead; the tier
table itself is one AWS table shared by both services, and cancelling a constant
is cheaper to keep correct than a second copy of the tiers.

Prices come from `pricing.FargateRates.Cost` (the §4.1 price function
`P(v,g) = v·rate_vcpu + g·rate_gb`), and the x86 on-demand rates come from
`pricing.DefaultFargateRates()` — so ECS and EKS cannot disagree about what an
x86 tier costs.

### 1.3 Recommendations

CPU: p95 × 1.30 headroom. Memory: observed **peak** × 1.25 headroom — a Fargate
task that exceeds its memory is killed, one that exceeds its CPU is throttled,
so the policy is asymmetric on purpose. Demand is raised to the task
definition's container-level floors, quantized, and priced; net savings run
through `pkg/pricing/commit` via `domain.Netter` exactly as the k8s-fargate
domain does.

Plus the two levers that are **illegal on EKS and legal here** (§1.4).

### 1.4 The ECS-vs-EKS feature difference, in the type system

| | EKS Fargate (`pkg/pricing`) | ECS Fargate (`pkg/ecs`) |
|---|---|---|
| Kubernetes memory overhead | +256 MiB, always | none |
| Platform type | `pricing.Platform`, **one** inhabitant (`EKSLinuxX86`) | `ecs.Platform` = `Arch` × `Market`, four |
| ARM / Graviton | no field exists in `FargateRates` | `ArmVCPUHourlyUSD` / `ArmGBHourlyUSD` |
| Fargate Spot | no field exists in `FargateRates` | `SpotDiscount` |
| Rate override file | `spotDiscount` / `armVCPUHourlyUSD` **rejected** | both are first-class |

The two `Platform` types are not convertible, so neither domain can borrow the
other's levers by accident. Both advisories carry a caveat naming the
precondition Kilter *cannot* observe (Spot: interruption tolerance + the
capacity-provider migration; ARM: arm64 image manifests and compiled
dependencies), are `domain.ActionAdvisory`, are always `Suppressed`, and their
estimates are reported in a **separate** `Report.AdvisoryMonthlyUSD` total that
is never added to `ClaimableMonthlyUSD`.

Advisory eligibility is enforced, not assumed: no ARM for Windows tasks, for
tasks already on arm64, or for services pinned below platform version 1.4.0; no
Spot for Windows tasks or for a service already on `FARGATE_SPOT`.

### 1.5 The actuator

`Execute` = `RegisterTaskDefinition` (a **read-modify-write** of the current
revision — only `cpu`/`memory` change, everything else is sent back verbatim,
because an ECS revision is an immutable whole and a reconstructed one silently
drops log config, secrets and volumes) then `UpdateService`.

`Revert` registers **nothing**: the step's `From` spec carries the full
task-definition ARN *including the revision*, and rolling back is a single
`UpdateService` onto that known-good revision.

`UpdateServiceInput` has three fields — cluster, service, task definition. There
is no desired-count field, so §3.4's "never change desired count" is
unrepresentable rather than merely forbidden.

### 1.6 Refusals

Every observed service yields exactly one `Assessment`, and an assessment with
no proposal always carries a `Suppression` with a stable code. The gates, in
order: mode tag → Fargate launch type → readable task size → `awsvpc` →
deployment-in-progress → idle → metric window / partial metrics / revision drift
/ window / samples → confidence → Fargate ceiling → platform version → container
limits → quantization ($0) → undersized → commitment waterfall → mode=recommend.

---

## 2. Invariants and exactly where they are tested

| Invariant | Test |
|---|---|
| `used = percent/100 × reserved`; **fails if inverted**, if `/100` is dropped, or if the operands are swapped | `TestAbsoluteFromPercentIsNotInverted` (`sizer_test.go`) — checks the three wrong formulas by name |
| The same, end to end: 30 % of 4 vCPU/8 GB ⇒ demand 1560m/3 GiB ⇒ tier **2vCPU 4GB**; the inverted answer is asserted to be a *different* tier and the missing-÷100 answer to be unrepresentable | `TestReservedVsUsedMathFromUtilizationPercentages` |
| Quantizer reuse: `RoundUpTier` == round-up at **every** tier and **every** ±1-byte boundary; the ceiling errors; `pricing.Quantize` still adds +256 MiB | `TestRoundUpTierIsQuantizeMinusOverhead` (`price_test.go`) |
| AWS-documented tier examples (§4.1.1 1 vCPU/8 GB; §4.1.3 200m/512 MiB; the 4-GB step at 8 vCPU; the 8-GB step at 16 vCPU), priced against the published rates | `TestQuantizerReuseAgainstAWSDocumentedTiers` |
| ECS x86 on-demand price == EKS price for every tier (shared engine, not a fork) | `TestX86OnDemandCostMatchesTheEKSEngine` |
| **A request change that crosses no tier boundary claims exactly $0** — demand strictly below reservation in both dimensions, `ClaimableMonthlyUSD() == 0` exactly, report total 0, reason says "$0.00" | `TestNoTierChangeClaimsExactlyZero` |
| **Spot and ARM are legal here**: both priced, both with caveats, both `ActionAdvisory`, both claim $0, neither ever becomes a step | `TestSpotAndARMAdvisoriesAreLegalHere`, `TestSpotAndARMArePriceableHere` |
| **The EKS-Fargate domain still refuses them**: `pricing.Platform` has one inhabitant, `FargateRates` has no ARM/Spot field (reflection), a rate override naming either is rejected, and the two `Platform` types are non-convertible | `TestEKSFargateStillRefusesSpotAndARM` |
| Advisory eligibility (Windows, already-arm64, platform < 1.4.0, already-Spot) | `TestARMAdvisoryRespectsItsOwnEligibility`, plus assertions inside `FuzzSizerInvariants` |
| **Revision rollback restores the recorded `From`** — reverting re-points at the exact recorded revision ARN, registers nothing, and a second revert is a no-op | `TestRevertRestoresTheRecordedRevision` |
| A step with no recorded revision is `domain.ErrIrreversible`, not a guess | `TestRevertWithoutARecordedRevisionIsIrreversible` |
| **The deployments-in-progress gate refuses** — sizer side (4 shapes of in-progress) and actuator side (planned healthy, executed mid-rollout), for both `Execute` and `Revert`, with nothing sent to the cloud | `TestDeploymentInProgressRefuses`, `TestExecuteRefusesDeploymentInProgress` |
| Change window / freeze / breaker refuse; a plan legal when built is refused when executed late; inside the window it succeeds | `TestExecuteRefusesGuardrails` |
| `kilter.dev/mode=off` refuses at actuation time (tag re-read live) | `TestExecuteRefusesModeOff` |
| Execute is idempotent per `Step.Key`: re-running registers no second revision | `TestExecuteIsIdempotent`, `TestStepKeysAreStableAcrossRuns` |
| Revision drift at actuation time refuses rather than clobbering | `TestExecuteRefusesRevisionDrift` |
| `awsvpc`, container floors and platform version are re-checked live | `TestExecuteRefusesShapeConstraints` |
| Same three, at decision time | `TestShapeConstraintsRefuse` |
| `UpdateService` cannot carry a desired count (reflection over the struct) | `TestUpdateServiceCannotChangeDesiredCount` |
| No metric window / partial / truncated / short window / too few samples all refuse, with evidence still attached | `TestEvidenceGatesRefuse` |
| Revision drift trims pre-rollout datapoints, then refuses if too few remain and proceeds (with a note) if enough do | `TestRevisionDriftTrimsThenRefuses` |
| Undersizing is reported as an advisory, never proposed | `TestUndersizedIsReportedNeverProposed` |
| Net ≤ Gross through the real commitment waterfall; net == gross with no ledger | `TestCommitmentNettingNeverOverClaims` |
| Every assessment proposes or states a reason; every recommendation passes `domain.Recommendation.Validate` | `TestEveryAssessmentStatesAReason`, `Report.Validate` |
| **Determinism**: shuffling services, tags, deployments and container definitions changes not one byte of the report; shuffled recommendations produce an identical plan and fingerprint | `TestReportIsShuffleInvariant`, `TestRecommendationsAndStepsAreShuffleInvariant` |
| Metric results are routed by query ID, never by position (a reversed CloudWatch response must not cross-wire two services' utilization) | `TestCollectorRoutesMetricsByQueryID` |
| A *missing* `GetMetricData` result is `Truncated`, not "no usage", and the sizer refuses on it | `TestCollectorMarksAMissingResultTruncated` |
| Window clamped to CloudWatch's 15-day 1-minute retention, with the clamp reported | `TestCollectorClampsTheWindowToCloudWatchRetention` |
| Transport errors surface; per-service failures degrade the snapshot and are named | `TestCollectorSurfacesTransportErrors`, `TestCollectorReportsDescribeFailures` |
| Task-size strings parse both AWS spellings and *error* rather than guess; proposals round-trip through the strings `RegisterTaskDefinition` takes | `TestParseTaskSizes`, `TestFormatTaskSizeRoundTrips` |
| Mode vocabulary identical to `pkg/guard`'s | `TestModeVocabularyMatchesGuard` |
| Garbage config/rates fall back per dimension; an invalid platform prices as the most *expensive* option | `TestGarbageConfigFallsBackToDefaults`, `TestRatesGarbageFallsBackPerDimension` |
| **Fuzz, sizer**: arbitrary task definitions × utilization × drift × variants — proposal-or-reason, real tiers, never the current tier, gross > 0, net ≤ gross, finite, claim 0 when suppressed, no advisory becomes a step | `FuzzSizerInvariants` |
| **Fuzz, quantizer reuse**: arbitrary demand always yields a real tier that holds it, or `ErrFargateTooLarge`; a need that is already a tier rounds to itself (no overhead crept in) | `FuzzRoundUpTier` |

A real bug was caught by the shuffle test and fixed: report totals were being
accumulated in arrival order, and float addition is not associative, so two runs
over the same data differed in the last bits. Totals are now summed after
sorting (`report.go`).

Fuzz runs: `FuzzRoundUpTier` 4.6 M execs, `FuzzSizerInvariants` 4.9 M execs, no
failures. Only the seed corpus is committed; nothing was written under
`testdata/`.

---

## 3. Deliberately deferred

1. **`domain.Domain` (Learn / Recommend / PlanSteps / Health / Checkpoint /
   Restore) is not implemented.** This package is stateless by design: unlike
   Kubernetes, CloudWatch *is* the history store, so a collection pass carries
   the whole window and no cross-tick histogram is needed — the same shape
   `pkg/ec2` (U5) uses. `Report.Recommendations()` already returns
   `[]domain.Recommendation`, so the adapter a later unit writes is thin.
   `Actuator` *does* implement `domain.Actuator` today.
2. **Container Insights / per-task metrics.** §3.4 scopes v1 to service level;
   per-task detail is a paid feature. Service-level `Average` is per-task demand
   because the percentage is already relative to the per-task reservation.
3. **Ephemeral storage.** Beyond the free 20 GB, Fargate bills
   ~$0.081/GB-month. The task definition's `ephemeralStorageGiB` is collected but
   not priced or optimized.
4. **Actually flipping a service to Spot or to arm64.** Both stay advisory: the
   preconditions are not observable from metrics, and §3.4 says never flip a
   service to Spot without approval. The specs carry `arch` / `market`
   attributes so a later approval-gated unit has what it needs.
5. **Fargate Savings-Plan rates.** As in `pkg/domain/fargate`, `ComputeSPRate`
   is left unknown, which `pkg/pricing/commit` treats as full stranding —
   under-claiming rather than inventing savings.
6. **Windows tasks** are priced with the Linux rates and get neither advisory.
   Windows has a 5-minute billing minimum and license fees (§4.1); the numbers
   for a Windows service are therefore a floor, not a bill.
7. **Post-change regression watch / quarantine.** `pkg/domain/fargate` has one
   via `pkg/safety`; the ECS equivalent needs deployment-health polling (a
   stateful controller-side concern) and is left to the wiring unit. The rollback
   *mechanism* is complete and tested — only the automatic trigger is missing.
8. **The CloudWatch seam is duplicated**, not shared with `pkg/ec2`'s. The two
   query shapes, periods and status handling differ, and coupling two domain
   packages to lift ~40 lines seemed worse than the duplication. If a third
   domain needs it, lift a `pkg/cloudwatch` seam then.

---

## 4. Exact wiring a later unit must do

### 4.1 `cmd/` — SDK adapters (the only place aws-sdk-go-v2 may appear)

Implement three interfaces against `*ecs.Client` and `*cloudwatch.Client`. They
are plain struct conversions; no logic belongs in the adapter.

```go
// pkg/ecs seams → aws-sdk-go-v2
type awsECS struct{ c *ecs.Client }
func (a awsECS) ListServices(ctx, in *ecs.ListServicesInput) (*ecs.ListServicesOutput, error)
func (a awsECS) DescribeServices(ctx, in *ecs.DescribeServicesInput) (*ecs.DescribeServicesOutput, error)
func (a awsECS) DescribeTaskDefinition(ctx, in *ecs.DescribeTaskDefinitionInput) (*ecs.DescribeTaskDefinitionOutput, error)
func (a awsECS) RegisterTaskDefinition(...)  // separate credentials/role
func (a awsECS) UpdateService(...)
type awsCW struct{ c *cloudwatch.Client }
func (a awsCW) GetMetricData(ctx, in *ecs.GetMetricDataInput) (*ecs.GetMetricDataOutput, error)
```

Adapter notes that will otherwise bite:

- `DescribeServices` must be called with `Include: []types.ServiceField{"TAGS"}`,
  or `kilter.dev/mode` is invisible and every service reads as `apply`.
- `DescribeTaskDefinition` returns `cpu`/`memory` as strings that may be either
  `"1024"` or `"1 vCPU"`. Pass them through verbatim; `ParseTaskCPU` /
  `ParseTaskMemory` handle both and error on anything else.
- Copy `deployments[].createdAt` and `rolloutState` — the revision-drift trim and
  the deployment gate both depend on them.
- `RegisterTaskDefinition` needs every field of the source revision. The seam's
  `TaskDefinitionRecord` is a *subset*; the adapter should keep the original SDK
  task definition alongside and apply only `cpu`/`memory` from the record. If it
  round-trips through `TaskDefinitionRecord` alone it will drop log
  configuration, secrets, volumes and IAM roles. **This is the one place the
  adapter is not a mechanical conversion.**
- Batch `DescribeServices` at 10 (`MaxServicesPerDescribe`) and `GetMetricData`
  at 500 (`MaxSeriesPerCall`) — the collector already does; the adapter must not
  re-batch.

### 4.2 `cmd/` — collector and brain wiring

```go
col, _ := ecs.NewCollector(awsECS{c}, awsCW{cw}, ecs.CollectorConfig{
    Cluster: *clusterFlag, Scope: accountID + "/" + region,
})
snap, err := col.Collect(ctx, now)             // caller supplies now
rep := ecs.NewSizer(ecs.Config{Region: region}).Report(snap, now, ledger)
recs := rep.Recommendations()                  // []domain.Recommendation
steps, err := ecs.PlanSteps(recs, domain.Guard{Now: now, Windows: w, Freeze: f, BreakerOpen: b})
```

`ledger` is the account-wide `*domain.Ledger` the brain already builds for the
other domains — pass the same one; Compute Savings Plans absorb Fargate
account-wide, so a per-domain ledger would over-claim.

Suggested flags: `--ecs-cluster` (repeatable, one collector per cluster),
`--ecs-window` (default 14d, clamped at 15d), `--ecs-spot-advisory` /
`--ecs-arm-advisory` (default on).

### 4.3 `cmd/` — controller wiring

```go
act, _ := ecs.NewActuator(awsECS{c}, awsECSWrite{c}, ecs.ActuatorConfig{
    Guard: g,             // SAME guard the plan was built under; Guard.Now required
    DefaultMode: guard.ModeApply,
})
for _, s := range steps { err := act.Execute(ctx, s) }   // ledger-record each
```

The actuator's `Guard` is **per-plan configuration**, because `domain.Actuator`
has no room for a `now` argument and this package has no clock. Build one
`Actuator` per plan execution. Every `Execute` re-reads the live service, so
gates are evaluated against reality, not against the plan's beliefs.

Record `Step.From` in the ledger: it is the rollback target, and `act.Revert(ctx,
step)` needs nothing else.

### 4.4 IAM

```json
[
  {"Sid": "KilterECSObserve", "Effect": "Allow",
   "Action": ["ecs:ListClusters", "ecs:ListServices", "ecs:DescribeServices",
              "ecs:DescribeTaskDefinition", "ecs:ListTagsForResource",
              "cloudwatch:GetMetricData"],
   "Resource": "*"},
  {"Sid": "KilterECSActuate", "Effect": "Allow",
   "Action": ["ecs:RegisterTaskDefinition", "ecs:UpdateService"],
   "Resource": "*",
   "Condition": {"StringNotEquals": {"aws:ResourceTag/kilter.dev/mode": "off"}}}
]
```

Two roles, matching the two interface sets: the collector gets the first, the
controller the second.

### 4.5 API / store / UI (§5.8, additive)

- bbolt bucket `domain/ecs-fargate/<scope>/…`; existing buckets untouched.
- `GET /v1/domains/ecs-fargate/recommendations` serves
  `Report.Recommendations()`; the report itself serves a per-domain card.
- The UI card must render **two** totals: `claimableMonthlyUSD` and
  `advisoryMonthlyUSD`, separately and labelled. Summing them would present
  Spot/ARM estimates — whose preconditions Kilter cannot verify — as savings,
  which is the exact dishonesty §7 trap 3 is about.
- Refusals are first-class UI content, not an error state: `Suppression.Code` is
  a stable string, `Suppression.Reason` is the prose to show, and
  `Suppression.ValidFrom` (commitment-dated blocks) means "re-check on this
  date".
