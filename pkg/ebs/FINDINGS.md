# U6 — EBS rightsizing: gp2 → gp3 at measured parity, and the first non-Kubernetes actuator

`pkg/ebs` implements docs/design/compute-domains.md §6 **U6**: the gp2 → gp3
parity math (§4.7), the read seams that feed it, the `pkg/domain` contract so it
registers as a domain, and a `ModifyVolume` actuator with cooldown, progress
polling, an audit ledger and revert-upward.

Everything below is green under `gofmt -l`, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/ebs/...` and `go test -race -short ./...`.
`go.mod` and `go.sum` are untouched: the package links **no AWS SDK, no
client-go and no third-party module at all** — `go list -deps ./pkg/ebs` is
stdlib plus intra-repo packages only.

---

## 1. What is here

| File | Role |
|---|---|
| `parity.go` | The arithmetic. Pure: no clock, no I/O, no package state. gp2 performance model, gp3 configuration space, rate card, `Rates.PlanGP3`. |
| `collect.go` | The read seams (`DescribeVolumes` / `DescribeVolumesModifications` / `GetMetricData` shaped) and the `Collector` that turns them into a `domain.Snapshot`. |
| `ebs.go` | The `domain.Domain` implementation: `Learn`, `Recommend`, `PlanSteps`, `Health`, `Checkpoint`/`Restore`. |
| `report.go` | `Assessment`/`Report`: one verdict per volume, always with its reason. `Recommend` is this projected onto the volumes that have a proposal. |
| `actuate.go` | The `domain.Actuator`: `ModifyVolume` write seam, pre-flight, dry-run/apply symmetry, polling, ledger, `Revert`. |
| `fixture.go` | Recorded fixtures replaying all four seams, including a modifying → optimizing → completed state machine — a mocked EC2 endpoint. |
| `testdata/account-mixed.json` | A recorded four-volume account used by the end-to-end test. |

~3,300 lines of production code, ~3,350 of tests, 53 tests and 2 fuzz targets.

## 2. The parity math, and where the naive rule is wrong

gp2 performance is a function of **size**; gp3's free baseline is flat. That
single fact is the whole trap (§7 trap 6).

```
gp2 baseline IOPS   = clamp(3 × GiB, 100, 16 000)
gp2 burst IOPS      = 3 000, only for volumes ≤ 1 000 GiB
gp2 throughput      = min(ceiling, baselineIOPS × 256 KiB),
                      ceiling = 128 MiB/s below 334 GiB, 250 MiB/s at or above
gp3 free baseline   = 3 000 IOPS + 125 MiB/s, then $0.005/IOPS-mo and $0.06/MBps-mo
gp3 limits          = IOPS ≤ min(16 000, max(3 000, 500 × GiB)); MiB/s ≤ min(1 000, IOPS ÷ 4)
```

### The regime table

| Size | gp2 delivers | What the naive "convert to gp3 baseline" rule does | What this package does |
|---|---|---|---|
| ≤ 166 GiB | ≤ 500 IOPS, ≤ 125 MiB/s | correct by accident | converts; Δ = exactly `0.02 × G` |
| 167 – 333 GiB | ≤ 999 IOPS, 125–128 MiB/s | loses up to 3 MiB/s | converts, paying ≤ $0.18/mo to keep the throughput |
| **334 – 375 GiB** | up to 1 125 IOPS, **250 MiB/s** | claims a **20 % saving** while **halving sustained throughput** | **refuses** (`no-cheaper-config`): parity costs $7.50/mo of throughput against ≤ $7.50/mo of storage saving |
| 376 – 1 000 GiB | ≤ 3 000 IOPS, 250 MiB/s | loses 125 MiB/s | converts; gp3's *sustained* 3 000 IOPS beats gp2's *credit-funded* burst |
| **> 1 000 GiB** | 3 × GiB IOPS, **no burst bucket** | **divides sustained IOPS by up to 5.33×** (16 000 → 3 000 at 5 334 GiB) | provisions to measured p99 × headroom, floored at gp2's delivered baseline when the window is short |
| ≥ 5 334 GiB | 16 000 IOPS (capped) | as above, worst case | same, and refuses when measured demand exceeds gp3's envelope |

Two consequences worth stating plainly:

1. **§4.7's headline example is optimistic.** "500 GB gp2 $50/mo → gp3 $40/mo
   (−20 %)" holds only if measurement shows the volume never needed gp2's
   250 MiB/s. At full nameplate parity the same volume is **$47.50/mo (−5 %)**.
   Both numbers are produced and tested (`TestDesignDocExamples`); which one is
   reported depends on the evidence, never on optimism.
2. **§4.7's Δ formula is continuous; ModifyVolume provisions integers.** Kilter
   rounds provisioning **up**, so its saving is the document's minus at most one
   IOPS and one MiB/s of rounding — never more.
   `TestNameplateDeltaMatchesDocFormula` asserts `kilter Δ ≤ doc Δ` and that the
   gap never exceeds one rounding unit, for every size from 1 GiB to 16 TiB.

### The floor rule (§4.7's minimum window)

Provisioning **below** gp2's delivered baseline is a real performance decision,
so it requires evidence: a window ≥ `MinWindow` (7 days, shipped) **and**
datapoint coverage ≥ `MinCoverage` (0.8) **and** ≥ `MinSamples` (288) points.
Otherwise the proposal is floored at gp2's delivered baseline
(`Floor = FloorGP2Baseline`), which means a thin observation can only ever
produce a same-or-better volume. Below `MinSamples` the volume is simply
**unmeasured** and gets no proposal at all.

Burst is deliberately **not** the floor. A burst bucket is credit-funded, so
promising 3 000 IOPS forever because a volume once reached it would be the same
class of error in the other direction. `BurstBalance` is collected and reported
as evidence.

## 3. Invariants, and exactly where each is tested

| Invariant | Test |
|---|---|
| A proposed gp3 configuration never clears less than measured demand — any size, any measurement, any floor | `FuzzParityNeverUnderProvisions` (~247 k execs clean in a 25 s burst), `TestParityGridNeverDegrades` (37 sizes × 15 IOPS × 10 throughputs × 2 floors) |
| The gp2 performance model is right at every regime boundary (100-IOPS floor, 334 GiB throughput step, 1 TiB burst cutoff, 16 000 IOPS ceiling) | `TestGP2PerformanceGrid` |
| §4.7's stated numbers reproduce exactly | `TestDesignDocExamples`, `TestNameplateDeltaMatchesDocFormula` |
| gp3 is **not** always cheaper: the 334–375 GiB band is refused | `TestParityRefusalBand` |
| The naive rule claims a saving exactly where parity refuses | `TestNaiveRuleClaimsASavingWhereParityRefuses` |
| The naive configuration is never the proposal, and its shortfall is reported | `TestNaiveMigrationLosesPerformance`, `TestNaiveDowngradeIsNeverProposed`, fuzz check (5) |
| Demand gp3 cannot meet is refused, not clamped | `TestNaiveMigrationLosesPerformance`, `TestNaiveDowngradeIsNeverProposed` |
| Fractional demand rounds **up**; throughput demand pulls IOPS up with it (0.25 MiB/s per IOPS) | `TestConfigForRoundsUp`, `TestThroughputImpliesIOPS` |
| Short window ⇒ floored at gp2 baseline, never below | `TestShortWindowFloorsAtGP2Baseline`, `TestEndToEndUnderShippedDefaults` |
| An unmeasured volume gets no parity claim | `TestRefusalMatrix` (`vol-unmeasured`, `vol-thin`) |
| Every volume yields exactly one verdict; silence is never an output | `Report.Validate`, asserted on every report through the `only`/`wantRefusal` helpers |
| Savings are `Bill()` deltas through the commitment path, never list-price deltas | `TestSavingsGoThroughTheCommitmentPath` |
| No RI or SP ever absorbs an EBS line (they cannot, in reality) | `TestEBSIsNeverAbsorbedByACommitment` |
| A suppressed recommendation stays visible and claims $0 | `TestModeRecommendStaysVisible` |
| Guardrails: freeze, breaker, change window, mode=off, mode=recommend | `TestPlanStepsGuardrails`, `TestRefusalMatrix`, `TestModeRecommendStaysVisible` |
| Report-only is enforced by the core, not by the domain's good manners | `TestReportOnlyIsEnforcedByTheCore` |
| Dry-run and apply reach the identical pre-flight verdict, and dry-run never mutates | `TestDryRunAndApplyAgree` (14 scenarios) |
| Re-executing a completed step is a no-op — in-process, after a restart, and after a ledger reload | `TestIdempotency` |
| An incomplete modification is *in-flight*, not done, and resumes without issuing a second modification | `TestPollingAcrossIncompleteModification`, `TestResumeOfMatchingInFlightModification` |
| Revert restores the recorded `From`, and is upward | `TestRevertRestoresRecordedFrom` |
| A revert that would degrade the live volume is refused | `TestRevertRefusesToDegrade` |
| Failures fail loudly and are recorded | `TestApplyRecordsFailures` |
| Every call is bounded by the caller's context | `TestContextCancellation` |
| A truncated metric response is a fact about the response, not "no I/O" | `TestCollectorTruncatedResponse` |
| A collector failure degrades the domain, never breaks the brain | `TestCollectorFailureModes`, `TestCollectorSurvivesBrokenPager`, `TestHealthStates` |
| Output is independent of input order (volumes, samples, tags, map iteration) | `TestOutputIsShuffleInvariant` (8 shuffled rounds, byte-identical reports) |
| Checkpoint is deterministic and round-trips | `TestCheckpointRoundTrip` |
| Overlapping snapshots do not inflate sample counts | `TestSamplesAreDeduplicatedByTimestamp` |
| An instance-bearing snapshot does not make this domain forget its volumes | `TestLearnIgnoresNonVolumeTargets` |
| The whole loop converges: apply, re-observe, nothing left to do | `TestEndToEndConvertsAndRecords` |
| The shipped defaults (7-day window, 288 samples) work end to end | `TestEndToEndUnderShippedDefaults` |
| No clock anywhere: callers pass `now` | `TestCollectorNeedsCallerClock`, `TestActuatorWiring`; `grep -rn 'time.Now()' pkg/ebs` is empty |

## 4. Decisions a reviewer should know about

**This domain registers as `domain.EC2`.** `domain.Kind` is a closed set with no
`ebs` member, and `Registry.Register` refuses unknown kinds — correctly. EBS
volumes are EC2 resources sharing the EC2 scope (`accountID/region`), which is
also how §5.1 places them (`pkg/domain/ec2`). The consequence is a wiring
constraint, addressed in §6 below.

**The audit ledger carries no money.** A ledger entry records the step, the
recorded `From`, the mode, the status, attempts, polls and timestamps. The
claimed saving belongs to the recommendation that produced the step (net,
post-commitment); duplicating a number in the ledger would create a second
source of truth for the bill, and the second one would be list price. Join by
`Step.Key`.

**EBS usage lines are `commit.KindEC2` with `SPIneligible: true`.** No Savings
Plan and no Reserved Instance covers EBS storage. The line is still handed to
the ledger, because the ledger prices the *whole account* before and after, and
because a later unit that changes an instance and its volume in one plan must
see both lines. `TestEBSIsNeverAbsorbedByACommitment` pins that a Compute SP
cannot silently zero an EBS saving.

**`Recommend` cannot express a refusal** (`domain.Recommendation` requires a
proposal that differs from the current spec), so refusals live in `Report`, the
shape `pkg/ec2` already established. `Recommend` is `Assess` projected. Anything
rendering EBS results should render the `Report`, not just the recommendations.

**Two bugs the tests caught during development**, both worth keeping in mind:

1. A gp2 spec legitimately carries the size-derived IOPS `DescribeVolumes`
   reports, so the first version of the "a gp2 target must not carry
   provisioned numbers" rule refused every *revert*. The rule now accepts the
   size-derived baseline and rejects any other number (`validTransition`).
2. Comparing whole `Spec`s for drift is wrong: a spec also carries state,
   attachment and modification attributes that move on their own, so a
   detached volume would read as drift. Identity is `volumeConfig`
   (type, size, IOPS, throughput), and gp2 identity ignores IOPS because AWS may
   round the size-derived number differently than this model does.

## 5. Deliberately deferred

- **io1/io2 → gp3.** §4.7 calls it advisory and latency-sensitive; refused with
  a stated reason (`not-gp2`), not silently dropped. It needs a latency-class
  signal (tags/attachment/workload) this unit does not collect.
- **gp3 → gp3 re-tuning.** An over-provisioned gp3 volume is a real saving and a
  natural follow-on, but it is a *reduction* on a volume with no gp2 baseline to
  floor against, so it wants its own evidence rule. Refused today as `not-gp2`.
- **Any size change.** ModifyVolume can only grow; growing is not rightsizing.
  `validTransition` refuses a step whose size differs, and the request never
  carries `SizeGiB`.
- **st1/sc1/standard**, snapshot costs, unattached-volume deletion (never: data
  bearing), multi-region/multi-account fan-out (one `Collector` = one
  account/region), 1-minute detailed EBS metrics, Cost Explorer corroboration of
  claimed-vs-measured.
- **BurstBalance-driven advisories.** Collected and reported as evidence
  (`burst-balance-min-pct`); no recommendation is derived from it yet.
- **Unverified AWS behaviours** are configuration, not constants: the 6-hour
  cooldown (`Config.Cooldown`, §4.9 lists it as unverified) and the gp2 burst
  mechanics (named constants in one block in `parity.go`, so a verification pass
  changes one place and re-runs the grid test).
- **gp3's "1 000 MiB/s needs ≥ 8 GiB" rule** is not modelled; only the 0.25
  MiB/s-per-IOPS ratio and the 1 000 MiB/s cap are. It cannot bite here: gp2
  never delivers more than 250 MiB/s, so no parity proposal reaches that corner.

## 6. Exact wiring a later unit must do

**1. SDK adapter (in `cmd/`, the only place that may link aws-sdk-go-v2).**
Implement three interfaces over the SDK — no type in this package is an SDK
type, so the adapter is field copying:

| Interface | Methods | AWS call |
|---|---|---|
| `ebs.InventoryAPI` | `DescribeVolumes`, `DescribeVolumesModifications` | `ec2:DescribeVolumes`, `ec2:DescribeVolumesModifications` |
| `ebs.MetricsAPI` | `GetMetricData` | `cloudwatch:GetMetricData` (namespace `AWS/EBS`, dimension `VolumeId`) |
| `ebs.ModifyAPI` | the above plus `ModifyVolume` | `ec2:ModifyVolume` |

IAM: `ec2:DescribeVolumes`, `ec2:DescribeVolumesModifications`,
`cloudwatch:GetMetricData` for read-only; add `ec2:ModifyVolume` only where
actuation is wired. `ModifyAPI` is deliberately not satisfied by a read-only
wiring.

**2. Clocks.** This package has none. Pass `time.Now`:

```go
col, _ := ebs.NewCollector(sdk, sdk, ebs.CollectorConfig{Scope: acct + "/" + region, Region: region})
dc, _ := ebs.NewDomainCollector(col, time.Now)                       // satisfies domain.Collector
act, _ := ebs.NewActuator(sdk, ebs.ActuatorConfig{Mode: ebs.ModeApply, Now: time.Now})
```

**3. Registration — and the one collision to plan for.**

```go
d, _ := ebs.New(ebs.Config{Scope: scope, Region: region, ActuationAvailable: actuatorWired})
reg := domain.NewRegistry()
reg.Register(d)   // registers under domain.EC2
```

A registry holds **one** domain per kind, so when U5's plain-EC2 instance domain
(`pkg/ec2`) grows its `domain.Domain` adapter, `cmd/` must register a composite
that fans `Learn`/`Recommend`/`PlanSteps` out to both halves rather than
registering both. Two facts make that cheap: volume IDs (`vol-…`) and instance
IDs (`i-…`) do not collide, and this domain already ignores non-volume targets
in a shared snapshot (`isVolumeTarget`, `TestLearnIgnoresNonVolumeTargets`).
Alternatively, add an `ebs` kind to `pkg/domain` — a one-line change to a closed
set that is not this unit's to make.

**4. Persistence (`pkg/store`).** `Domain.Checkpoint`/`Restore` for learned
state (deterministic bytes; version-tagged). `Actuator.LedgerJSON`/
`RestoreLedger` for the audit ledger — **restore it before executing**, or a
completed step costs one `DescribeVolumes` to rediscover it is a no-op (it is
still a no-op; it just costs a call).

**5. The controller must call `Domain.RecordApplied(step, now)`** for every step
it completes. That is what holds the cooldown before a collector has observed
the modification record.

**6. Guardrails.** Build `domain.Guard` from the existing freeze / breaker /
change-window configuration; nothing EBS-specific is needed. Per-volume mode
comes from the `kilter.dev/mode` **tag** on the volume, with the same
`off | recommend | apply` semantics as the Kubernetes annotation.

**7. Rates.** `ebs.DefaultRates()` ships embedded (us-east-1). A synced or
hand-written override goes through `ebs.LoadRates(io.Reader)` into
`Config.Rates`; unknown fields and non-positive rates are rejected rather than
half-applied.

**8. UI / `analyze` / `/insights`.** Render `Domain.Assess(now, ledger)`:

- one row per volume, `Assessment.Refusal.Code` + prose for the refusals (they
  are stable strings safe to store, group and filter on);
- `Report.ClaimableMonthlyUSD` is the only number that may be presented as a
  saving; `Report.GrossMonthlyUSD` is the list price and must be labelled as
  such;
- `Assessment.Parity.NaiveDegrades` / `NaiveIOPSShortfall` is the "what a naive
  tool would have done to this volume" column, which is the single most useful
  thing this domain can show an operator;
- `Assessment.Observed.Floor` tells the operator whether a proposal is floored
  at gp2's baseline (and therefore whether waiting for a full week would save
  more).

---

## 7. K3 — `Learn` could not tell "not addressed to me" from "my collector found nothing"

`cmd/FINDINGS.md` §5.1, fixed here. One production change (`Learn` gains an
addressing test), one new test file, `go.mod`/`go.sum` untouched, no existing
test modified.

### 7.1 Root cause

`Kind` is `domain.EC2`, and the ec2 kind covers instances as well as volumes.
More than one collector therefore feeds it: `pkg/domain/ec2` composes an
instance half beside this one, and that half ships its evidence as an opaque
`Snapshot.Payload` rather than as generic targets.

`Learn` had exactly one rule for a snapshot with nothing in `Targets` or
`Samples`:

```go
if len(snap.Targets) == 0 && len(snap.Samples) == 0 {
	d.stale = true
	d.staleReason = "collector delivered no volumes"     // ← a claim about OUR collector
	return nil
}
```

That is right under this package's own contract — one collector, one domain,
one snapshot — and wrong inside a composite. Handed the sibling's snapshot the
domain concluded that **its** collector had found nothing and degraded itself.
Health is last-write-wins over `d.stale`, so whichever snapshot arrived last
decided it. The bug is not in the arithmetic; it is that the package treated
*absence of its own data* as *evidence about its own collector*, without first
asking whether the snapshot was addressed to it.

### 7.2 The fix

A new `addressedHere(snap)`, consulted before `Learn` touches any state:

> A snapshot that carries content, none of it ours, was not addressed to us and
> is a no-op. A snapshot that carries **nothing** is our own collector reporting
> an empty account.

"Ours" is `hasVolumeEvidence`: any target `isVolumeTarget` accepts, or any
sample `isVolumeSample` accepts (`isVolumeSample` is the metric-side twin of
the existing `isVolumeTarget` — one of this domain's three metric names, or a
`vol-` target ID). Nothing decodes `Payload`; it stays opaque, which it must,
and this package never writes one.

**The distinction that matters is preserved, deliberately.** An EBS snapshot
with genuinely zero volumes carries no payload, no targets and no samples, so
it is still *addressed*, still degrades the domain, and still says
`collector delivered no volumes` — distinct from the never-collected reason
`no volume inventory learned yet`. Collapsing "we looked and found none" into
"we never looked" would have been worse than the bug, and
`TestEmptyAccountStaysDistinguishableFromNoCollection` pins both branches
against each other so a later simplification cannot quietly merge them.

The no-op is a full no-op, which matters: the old code wrote `d.scope`,
`d.stale`, `d.staleReason`, `d.learned` and `d.lastAt` from the sibling's
snapshot before it ever looked at a volume.

### 7.3 Failing-first evidence

The three tests in `addressing_test.go` were written and **run against the
unfixed tree first**. All three failed. `TestSiblingSnapshotDoesNotDecideHealth`
reported the two answers verbatim:

```
volumes first:   {"ready":false,"reportOnly":true,
                  "reason":"partial collection: collector delivered no volumes","targets":1}
                 refused: domain: report-only ...: partial collection: collector delivered no volumes
instances first: {"ready":true,"reportOnly":false,"targets":1}
                 2 steps
```

Same two snapshots, same clock, same fixture — a different health line and a
different plan, decided by arrival order. That is §5.1 reproduced inside the
package that owns it.

| Test | What it holds |
|---|---|
| `TestSiblingSnapshotDoesNotDecideHealth` | the two-snapshot case, both orders, compared on health + report + plan; and pins *which* answer is correct by comparing against the volume snapshot alone |
| `TestEmptyAccountStaysDistinguishableFromNoCollection` | an empty EBS collection still degrades and still says why; the sibling's snapshot can fake neither state |
| `TestSiblingSnapshotsAreInertUnderShuffle` | 24 seeded shuffles of one volume snapshot against six sibling snapshots (one of them `Stale`) spread either side of it in time — every interleaving must equal the volume snapshot alone |
| `FuzzSnapshotArrivalOrder` | arbitrary permutations, plus the degenerate shapes a shuffle test would not build: a nil snapshot, and an instance-only snapshot with no payload. 1.1 M executions, no failures |

The oracle is `stateOf`: health JSON + report JSON + the plan (step count, or
the refusal string). Comparing only the report would have missed this bug — the
report was *identical* in both orders. Only health and the plan differed.

### 7.4 Survey: the same class of defect elsewhere in `pkg/ebs`

Looked for anywhere the package infers something about its collector from the
absence of its own data.

**(a) `d.lastAt` and `d.learned` advanced on a foreign snapshot — same class,
also fixed, and NOT covered by `Part.Accepts`.** The composite guard is
`len(s.Payload) == 0 || len(s.Targets) > 0`, so an instance-only snapshot with
no payload passes it. Probed against the unfixed tree:

```
PROBE-A (before): learned=true lastAt=2026-08-06 scope="other-scope"
                  health={Ready:true ReportOnly:false Reason: Targets:0}
PROBE-A (after):  learned=false lastAt=zero scope="123456789012/us-east-1"
                  health={Ready:false Reason:no volume inventory learned yet ...}
```

A domain tracking **zero volumes** reported itself freshly and cleanly
collected, and rewrote `d.scope` — which is `Report.Scope` — to the other
half's. This is the optimistic mirror image of §5.1: the reported bug made a
healthy domain look broken; this made a blind one look healthy, which is the
more dangerous direction because `PlanSteps` gates on `Health`. `addressedHere`
closes it because it asks for *volume* evidence, not merely for targets.

**(b) `Checkpoint`/`Restore` drops `stale`/`staleReason` — same shape, reported,
not fixed.** `Restore` restores `learned` but not the degradation, so `Health`
falls past `case d.stale` to the freshness check and reports ready. Probed, and
still true on this tree:

```
PROBE-B before checkpoint: {Ready:false ReportOnly:true Reason:partial collection: us-east-1c timed out}
PROBE-B after  restore:    {Ready:true  ReportOnly:false Reason:}
```

A partial collection is forgotten by a process restart, and the absence of the
persisted flag is read as "the collection was clean" — the same missing-field-
becomes-a-positive-claim shape as §7.1. The fix is two fields on the versioned
`checkpoint` struct; old blobs unmarshal them as `false`, i.e. exactly today's
behaviour, so no `checkpointVersion` bump is needed. Left out because it is a
persisted-format change and not the ordering bug this unit was scoped to.

**(c) The collector layer is clean, and is the model for the rest.**
`collectMetrics` records an unanswered CloudWatch query as `StatusTruncated`
rather than as an empty series, and `Collect` marks a volume type it never
metered `Blind` — both exist precisely so the domain cannot read silence as
"this volume does no I/O". `Collect` also returns a transport error on the
first inventory call instead of an empty snapshot, "because a failed call is not
evidence of an empty account". That sentence is the invariant §7.1 violated one
layer up.

**(d) `observe` refuses per volume, which is the right shape.** No IOPS series
produces an `unmeasured` refusal on *that volume*, not a claim about the
collector. Same for `insufficient-samples` / `insufficient-window`. A per-target
refusal with a stable code is the honest way to say "we were not shown enough".

**(e) Cooldown is permissive on absence, by design.** `lastModification`
returning `ok == false` means no cooldown applies, so an unobserved modification
does not block a step. That is why `RecordApplied`/`d.applied` exists (§6 item
5), and it is documented rather than inferred. Noted, not a defect.

### 7.5 Is `Part.Accepts` now redundant?

**For this pairing, yes — strictly.** `volumeSnapshot` rejects exactly
`len(Payload) > 0 && len(Targets) == 0`; `addressedHere` no-ops on that and on
strictly more (see (a): payload-less instance targets, which `Accepts` lets
through). Every snapshot `Accepts` rejects, `Learn` would now ignore anyway.

**It should still stay.** It is a guard at a different layer and it is not this
unit's to remove: `Part.Accepts` is `pkg/domain`'s answer for *any* half that
cannot tell on its own what it is looking at, and only one of the two halves
that exist today has been taught to. Deleting it would make the composite's
correctness depend on every present and future part having done what `pkg/ebs`
just did. If a later unit that owns `pkg/domain` does drop it, `volumeSnapshot`
and the `Accepts` field on the volumes part in `pkg/domain/ec2/ec2.go` are the
two things to delete, and `TestSiblingSnapshotsAreInertUnderShuffle` is the test
that then carries the property alone.
