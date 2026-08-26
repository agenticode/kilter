# U2 — the `pkg/domain` seam + the k8s-fargate domain

Implements unit **U2** of `docs/design/compute-domains.md` §6: the core types
and registry of §5.2–§5.3, and `k8s-fargate` as their first real user —
tier-move and boundary-shave recommendations priced by U1's quantizer.

Files, all new, all under `pkg/domain/`:

| File | What |
|---|---|
| `domain.go` | §5.2 core types: `Kind`, `TargetRef`, `Spec`, `ActionClass`, `Recommendation`, `Evidence`, plus `Step`/`StepKey`/`Fingerprint` from §5.6 |
| `snapshot.go` | `Snapshot`/`Target`/`Sample`, the `Collector`/`Domain`/`Actuator` interfaces, `Health`, `Guard`, the error set |
| `registry.go` | `Registry` — explicit wiring, canonical ordering, report-only enforcement |
| `ledger.go` | `Netter` seam + `Ledger`, the account-wide splice into `pkg/pricing/commit` |
| `fargate/spec.go` | Fargate `Spec` encoding: billed tier in `Resources`, per-container requests in `Attrs` |
| `fargate/fargate.go` | `Config`, learned state, `Learn`/`Health`/`PlanSteps`/`RecordApplied`/`Checkpoint`/`Restore` |
| `fargate/recommend.go` | the projection: tier move, boundary shave, revert |

`go.mod` and `go.sum` are untouched; the only non-test dependencies added are
intra-repo (`model`, `guard`, `decision`, `recommend`, `safety`, `plan`,
`pricing`, `pricing/commit`). No SDK, no client-go, no clock, no I/O.

`gofmt`, `go vet ./...`, `go build ./...`, `go test -race -count=1
./pkg/domain/...` and `go test -race -short ./...` are all green. Statement
coverage: **95.6 %** (`pkg/domain`), **92.2 %** (`pkg/domain/fargate`).

---

## 1. Why the seam, in one paragraph

Kilter's loop optimizes by packing containers onto cheaper machines. A Fargate
pod has no machine to pack — it is its own single-pod VM, and AWS bills the
quantized tier it rounds the pod's requests up to. So the only two levers are
"land the pod on a cheaper tier" and "drop the request just under a tier
boundary", neither of which the node-centric pipeline can express, and both of
which are worth exactly $0 unless a tier boundary is crossed. `pkg/domain` is
the thin contract that lets one engine serve both billable shapes; `k8s-fargate`
is the proof that the contract is the right shape.

---

## 2. Invariants, and exactly where each is tested

### The core (`pkg/domain`)

| Invariant | Test |
|---|---|
| **Zero domains registered ⇒ today's behaviour.** Nil registry, zero-value registry and empty registry all return nil from `Kinds`/`Recommend`/`Health`, no-op on `Learn(nil)`, and never panic | `TestZeroDomainsRegistered` (3 sub-tests × 7 entry points) |
| A snapshot for an unregistered domain is **reported, not dropped** — silently discarding collected data is indistinguishable from a broken collector | `TestZeroDomainsRegistered` (`ErrNotRegistered`) |
| Wiring bugs are refused: nil domain, unknown kind, duplicate kind | `TestRegisterRejectsWiringBugs` |
| **Report-only is enforced by the core**, not by trusting the domain: a degraded domain still recommends and can never produce a step, and the domain's own `PlanSteps` is never even called | `TestReportOnlyDomainCannotPlanSteps` |
| Suppressed and foreign recommendations never reach a domain's `PlanSteps` | `TestPlanStepsFiltersSuppressedAndForeign` |
| Output order is independent of registration order and of map order; the registry stamps the producing domain so no domain can emit under another's name | `TestRegistryRecommendIsDeterministicAndAttributed` (100 shuffled registrations), `TestSortRecommendationsIsDeterministicUnderShuffle` (500 shuffles) |
| `Spec` comparison, rendering and hashing never depend on map iteration order | `TestSpecAttrsAreOrderIndependent` (200 iterations), `TestStepKeyIsStableAndContentAddressed` |
| `Spec.WithAttr` copies rather than aliases, so a Spec recorded in a Step cannot mutate later | `TestSpecWithAttrCopiesTheMap` |
| **`Net ≤ Gross`, always** — enforced at the single chokepoint `SetSavings`, with NaN/±Inf mapped to 0 | `TestSetSavingsEnforcesNetLessThanGross` (9 cases), `FuzzSetSavings` (3.7 M execs clean) |
| A recommendation with no evidence is a bug; so is one that proposes no change, has an invalid action class, or claims net > gross | `TestRecommendationValidate` (9 cases) |
| Guardrails refuse before any step is built: freeze > breaker > change window | `TestGuardAllow` |
| `Ledger` degrades to net == gross with no inventory, splices by line ID, never mutates the baseline, and is order-independent | `TestNilLedgerNetEqualsGross`, `TestSpliceReplacesByIDAndAppendsAnonymous`, `TestSpliceIsOrderIndependent` (100 shuffles) |
| An account-wide view never under-reports absorption relative to a per-line view | `TestLedgerSplicesIntoAccountWideUsage` |
| `commit` and `domain` suppression codes are the same strings, so they can be forwarded without translation | `TestLedgerSuppressesCommitmentStranding` |

### The k8s-fargate domain (`pkg/domain/fargate`)

| Invariant | Test |
|---|---|
| **The §4.1.1 overhead cliff, end to end.** 1 vCPU / 8 GB requested → billed 2 vCPU / 9 GB; shaving 256 MB lands 1 vCPU / 8 GB. Exact dollars from the published rates: $0.120965/h → $0.07604/h, **$32.795/mo**, **−37.1 %** | `TestOverheadCliffBoundaryShave` |
| **Recommendation math per tier class** — every vCPU row of the §4.1 table (0.25 / 0.5 / 1 / 2 / 4 / 8 / 16), including the 4-GB steps at 8 vCPU and the 8-GB steps at 16 vCPU. Prices asserted to 1e-12 against `0.04048·v + 0.004445·g`, gross to 1e-9 | `TestBoundaryShavePerTierClass` (7 cases) |
| A change that does not cross a tier boundary is **never emitted** — U1's $0 rule at the domain level | `TestWithinTierChangeIsNeverRecommended` |
| **Every Fargate resize is `ActionRolling`**, is accounted as disruptive, and `PlanSteps` refuses a plan containing anything else | `TestEveryFargateResizeIsRolling`, `FuzzPlanStepsNeverEmitsInPlace`, and an assertion inside `FuzzRecommendNetNeverExceedsGross` |
| A tier move is `pkg/recommend`'s decision projected onto the tier table, priced per pod × replicas, and re-quantizes to the tier it quotes | `TestTierMoveProjectsTheSizingPolicy` |
| **Boundary-shave gates, each vetoing on its own** against a positive control: sample count, window span, composed confidence, freshness, any OOM ever, the noise band, an init-container floor, the dollar floor, a stricter threshold, a wider band | `TestBoundaryShaveGates` (12 cases, each naming the number that vetoes it) |
| A shave never lands inside the noise band and never raises a request, swept across a tier boundary | `TestShaveNeverProposesBelowThePeakPlusBand` (52 peaks) |
| The noise band is `max(10 %·peak, 3σ)` — volatility widens it | `TestNoiseBandWidensWithVolatility` |
| The observed peak holds for 14 days then relaxes toward the recent level, never below it (reusing `decision.EffectiveOOMFloor`) | `TestObservedPeakRelaxesWithAge` |
| Broken telemetry (negative memory, zero timestamps) is dropped, not clamped, so it can never authorize a shave | `TestGarbageUsageCannotLowerTheFloor` |
| **A simulated post-resize OOM drives the revert**: revert to the exact recorded prior spec, rolling, low risk, confidence 1, negative savings, claimable $0; the workload is quarantined; the revert survives being asked for twice and is consumed by executing it | `TestPostResizeOOMDrivesTheRevertPath` |
| A quarantined workload gets no new shave, and the quarantine lapses on schedule | `TestQuarantineBlocksNewShaves` |
| Kilter only reverts changes Kilter made | `TestRegressionWithoutARecordedChangeIsNotOurs` |
| A pending revert survives a controller restart | `TestPendingRevertSurvivesARestart` |
| **Degraded ⇒ report-only**, in all five ways it can happen: never learned, no cluster payload, partial collection, stale by age, actuation not wired — plus "a default-constructed domain may not act" | `TestDegradesToReportOnly` (6 sub-tests) |
| A payload-less or nil snapshot degrades the domain and returns nil; only a wrong-domain snapshot is an error | `TestLearnInputHandling` |
| Fargate VMs are never priced as nodes (the fixture's VMs report 96 vCPU / 384 GB) | `TestFargateNodesNeverPricedAsNodes` |
| `kilter.dev/mode`: `off` produces nothing, `recommend` produces a **visible suppressed** recommendation that claims $0 and yields no step, `apply` plans | `TestModeGuardrails` |
| Steps are sequenced, keyed, target-ordered, capped by `MaxSteps`, carry the exact From/To for revert, and are refused under freeze/breaker | `TestPlanStepsShape` |
| **No output depends on input order or map order** — recommendations byte-identical under 50 shuffles of pods/usage/nodes, plan fingerprint stable across 20 rebuilds | `TestOutputIsDeterministicUnderShuffle` |
| Checkpoints are byte-stable, and restore-then-learn is indistinguishable from learn-then-learn; a restored domain stays report-only until a collector feeds it | `TestCheckpointRoundTripAndDeterminism` |
| The Spec encoding round-trips, sorts by container name, and rejects malformed breakdowns rather than half-patching | `TestSpecEncodingRoundTrip` |
| A garbage `Config` falls back to the conservative defaults, never to a disabled guard | `TestConfigDefaultsAreConservative` |
| **`Net ≤ Gross` for every recommendation the domain can emit**, under arbitrary pod shapes, replica counts, init requests, sample counts and commitment inventories — plus finiteness, non-negative prices, rolling action, claim ≤ max(0, gross), valid tiers on both sides, and shave monotonicity | `FuzzRecommendNetNeverExceedsGross` (**1.42 M execs clean**) |

---

## 3. Findings

### 3.1 Two real determinism bugs, caught by the shuffle tests

Both were in code that looked obviously deterministic.

1. **`PeakAt` was set by arrival order.** The peak tracker used `mem > peak`,
   so with a flat usage series the *first sample ingested* stamped the peak's
   timestamp. Shuffling the usage slice changed the timestamp, which changed
   the emitted evidence and — because `decision.EffectiveOOMFloor` ages the
   peak from that timestamp — could eventually change a decision. Fixed by
   taking the **latest** time the peak level was reached: order-independent,
   and the conservative direction (the peak holds full strength longer).
2. **`pkg/recommend.Checkpoint()` returns its states in map order.** Embedding
   it verbatim made this domain's checkpoint non-byte-stable, which defeats any
   store that compares bytes before writing. Fixed here by sorting the embedded
   slice by container key. *This is a latent issue in `pkg/recommend` itself*
   for any other caller that persists it — not fixed there, because that
   package is out of scope this round.

### 3.2 The boundary shave deliberately proposes less memory than `pkg/recommend` would

This is the sharpest design decision in the unit and deserves to be stated
plainly. `pkg/recommend`'s memory target is `max(p99 × headroom, peak)` with a
trend projection on top. The shave's floor is `peak + max(10 %·peak, 3σ)`. In
the §4.1.1 fixture the policy wants 8.4 GiB while the shave proposes 7.75 GiB —
**less** memory than the sizing policy asks for.

That is the point of the lever: on Fargate, 650 MiB of extra headroom costs a
whole vCPU and 37 % of the bill, because it pushes the pod over a tier boundary.
Buying that headroom is only rational if it is actually used, and the peak says
it is not. The trade is only acceptable because the shave is gated far harder
than the policy is — confidence ≥ 0.80 *and* ≥ 120 samples *and* ≥ 24 h of
window *and* never after an OOM *and* a dollar floor *and* a noise band that
widens with observed volatility — and because a wrong call is caught by the
regression watch and reverted.

Two structural consequences follow, both encoded:

- **CPU is never shaved.** The target tier must hold the *unchanged* CPU
  request, exactly as in the AWS worked example where the vCPU drops as a
  *consequence* of the memory shave while the pod keeps asking for 1 vCPU.
  Shaving CPU would trade a throttle for a dollar, which is a worse bargain
  than trading an OOM for a dollar.
- **Tier moves keep the policy's headroom** and use it as their floor
  (`max(policy target, observed peak)`); only the shave uses the bare
  peak-plus-band. At most one recommendation is emitted per workload, and when
  both levers fire the cheaper wins with ties going to the tier move.

### 3.3 `pkg/pricing/commit` needed a seam, not a change

`Inventory.NetSavings(before, after Usage)` requires the **account-wide** usage
on both sides and documents that passing only the affected lines is a
correctness bug: Compute Savings Plans absorb usage account-wide, so a partial
view understates absorption and overstates the saving. A domain knows only its
own targets and cannot construct that view.

Rather than edit `commit`, `pkg/domain` defines `Netter` (what a domain needs:
"assess replacing these lines with those lines") and ships `Ledger`, which
splices the domain's lines into an account-wide baseline by `UsageLine.ID` and
calls `NetSavings` correctly. **No change to `pkg/pricing/commit` is required.**
`TestLedgerSplicesIntoAccountWideUsage` shows the partial view never
over-claiming relative to the full one.

**One honest limitation.** Kilter has no Fargate Savings-Plan rates, so the
domain emits `ComputeSPRate: 0` and `commit`'s documented fallback applies: the
covered pod is treated as free at the margin and the commitment as fully
stranded. Consequence — **when the account holds any Compute Savings Plan,
every Fargate saving nets to $0 and is suppressed as `commitment-neutral`.**
That under-claims by construction and can never invent a saving, which is the
right way to be wrong about somebody's bill, but it does mean U4 (or whoever
wires `DescribeSavingsPlansOfferingRates`) unlocks real Fargate net savings by
populating that one field. Pinned by `TestFargateWithoutSPRateNetsZero` and
`TestLedgerRoutingNeverExceedsGross` so the day it changes, the tests say so.

### 3.4 Deviations from §5.2/§5.3, and why

1. **`Recommendation` gains `Suppressed` + `SuppressCode`.** §5.7 requires a
   commitment-blocked recommendation to stay visible with its reason and
   `ValidFrom`; a bare `[]Recommendation` cannot express "report this, never
   apply it". `PlanSteps` skips suppressed recommendations in both the registry
   and the domain.
2. **`Domain` gains `Health(now) Health`.** §5.3 has no readiness method, but
   "every domain degrades to report-only when its collector or credentials are
   absent" is unenforceable without one, and enforcing it in the core (rather
   than trusting each domain) is what makes it an invariant instead of a
   convention.
3. **`Snapshot` gains `Cluster *model.ClusterSnapshot`, `Stale`, `StaleReason`.**
   The Kubernetes collector already ships a `ClusterSnapshot`; re-projecting it
   into generic `Targets`/`Samples` so the brain could re-derive it would be a
   lossy round trip for no gain. `Targets`/`Samples` remain for non-Kubernetes
   domains (U5+). `Stale` is how §5.7's "a failed region poll ships a
   stale-marked snapshot" is actually expressed.
4. **`PlanSteps(recs, g Guard)` instead of `guard.Config`.** `pkg/guard` has no
   `Config` type; `domain.Guard` carries the genuinely domain-neutral half
   (windows via `guard.Window`/`guard.InWindow`, freeze, breaker, cap). Per-target
   mode is *not* in it — mode is a property of the target, so each domain
   resolves it during `Recommend` (via `guard.ModeFor`) and marks the affected
   recommendations suppressed, which keeps a `mode=recommend` workload in the
   report instead of vanishing between `Recommend` and `PlanSteps`.
5. **`Recommend(now, ledger Netter)` instead of `*commit.Ledger`.** No such
   type exists; see §3.3.
6. **`TargetRef.ID` is `Kind/namespace/name`** (`model.WorkloadRef.String()`),
   parsed back by `parseTargetID`. The billable unit is the pod, but the
   *resizable* unit is the workload's container template, and every replica of
   a workload shares a tier.
7. **`Spec.Resources` holds the billed tier, not the requests.** For Fargate the
   tier is what costs money, so `Current.Resources`/`Proposed.Resources`
   correspond exactly to `CurrentHourlyUSD`/`ProposedHourlyUSD`. The
   per-container requests an actuator must patch live in `Attrs` under
   `container/<name>/{milliCPU,memoryBytes}` and decode through
   `fargate.Containers(spec)`, which is strict — a malformed breakdown is an
   error, never a partial patch.
8. **`Config.DefaultMode` defaults to `guard.ModeApply`**, matching
   `plan.DefaultConfig`. Two different defaults for one annotation would be
   worse than either choice. Actuation is instead gated by
   `ActuationAvailable`, which defaults to **false**: forgetting to wire
   credentials can never read as permission to act.

### 3.5 Deliberate deferrals

- **No `Actuator` implementation.** The k8s-fargate actuator is the existing
  `pkg/actuate` patch path, which is out of scope for this unit. The interface,
  `ErrIrreversible`, and the `Step` envelope are defined; §4 below is the
  wiring.
- **`ClusterCost.Warnings` are not yet surfaced as insights.** U1's item 5.
  The domain drops pods it cannot price (over the 16 vCPU / 120 GB ceiling,
  invalid `CapacityProvisioned`) rather than inventing a bill for them; routing
  those warnings into `/insights` belongs with the API wiring in §4.
- **Jobs, CronJobs and bare pods are excluded**, matching `pkg/recommend`'s
  exclusions. U1's "terminal Fargate pods keep billing" insight (its item 6) is
  a *hygiene* recommendation with a different shape (delete/TTL, not resize) and
  is not modelled here.
- **Ephemeral storage is not priced.** `pkg/model` has no
  `ephemeral-storage` field; unchanged from U1's item 5.
- **`OOMSeen` never decays.** One observed OOM disqualifies a container
  template from being shaved, permanently, for as long as its learned state
  lives. An OOM is evidence that the memory samples do not capture demand,
  which is exactly the assumption the shave rests on. A decaying version
  (`decision.EffectiveOOMFloor` over the OOM floor) is the obvious refinement;
  it was deferred because "reluctant" is the correct failure direction for this
  lever and the simple rule is trivially auditable.
- **No crossover report.** That is U3.

---

## 4. Exact wiring a later unit must do

Nothing outside `pkg/domain/` was touched, so the seam is inert until wired.

### 4.1 `cmd/kilter` (analyze) and the brain

```go
// once, at startup:
reg := domain.NewRegistry()
fg, err := fargate.New(fargate.Config{
    Scope:              clusterID,
    Region:             region,
    Recommend:          recommendCfg,       // the same policy the node domain uses
    ActuationAvailable: kubeClient != nil,  // false ⇒ report-only, by design
    DefaultMode:        planCfg.DefaultMode,
})
if err != nil { return err }
if err := reg.Register(fg); err != nil { return err }
```

U1 item 3 already asks the three snapshot owners (`plan.Build`,
`cmd/kilter/analyze.go`, `pkg/api/brain.go`) to call `pricing.SplitFargate`
before node math. At the same three points, feed this domain:

```go
_ = reg.Learn(&domain.Snapshot{
    Domain: fargate.Kind, Scope: snap.ClusterID,
    Timestamp: snap.Timestamp, Cluster: snap,
    Stale: collectPartial, StaleReason: collectErr,   // never break the brain
})
recs := reg.Recommend(now, ledger)                    // `now` from the caller
```

`analyze --fargate` renders `recs` plus `reg.Health(now)`; a report-only domain
must be labelled as such in the output, not hidden.

### 4.2 `pkg/api` (`/insights`, and the additive §5.8 routes)

- `GET /v1/domains` → `reg.Health(now)`.
- `GET /v1/domains/{kind}/recommendations` → `reg.Recommend(now, ledger)`
  filtered by kind. The response must render `Suppressed`, `SuppressCode`,
  `Reason` and `ValidFrom`: a suppressed recommendation that looks like an
  applicable one is worse than no recommendation at all.
- `/insights` should surface, as bug-grade insights: (a)
  `pricing.FargatePodWarnings` for every Fargate pod (U1 §4.1.2's production
  validation of the quantizer), and (b) any domain whose `Health.ReportOnly` is
  true, with its `Reason`.
- Savings roll-ups must sum `Recommendation.ClaimableMonthlyUSD()`, never
  `GrossSavingsMonthlyUSD`. Gross is there so a UI can show the list-price
  fantasy beside the fact.

### 4.3 The commitment ledger

```go
inv, _ := commit.LoadInventoryFile(path)       // optional; nil is fine
baseline := commit.Usage{Lines: accountWideLines}  // every domain's usage
ledger := domain.NewLedger(inv, baseline)
```

Passing `nil` is legal and means "no known commitments", i.e. net == gross. See
§3.3 for the Fargate SP-rate limitation.

### 4.4 `pkg/actuate` — the missing 100 lines

Implement `domain.Actuator` for `k8s-fargate` over the existing patch path:

```go
func (a *fargateActuator) Execute(ctx context.Context, step domain.Step) error {
    changes, err := fargate.Containers(step.To)     // strict decode
    if err != nil { return err }
    // patch spec.containers[i].resources.requests for each change, then let the
    // controller roll the pods. Idempotent per step.Key: if the workload
    // already matches step.To, return nil.
}
func (a *fargateActuator) Revert(ctx context.Context, step domain.Step) error {
    // same, from step.From — a rolling resize is reversible, so this must not
    // return domain.ErrIrreversible.
}
```

Three obligations on the controller, none of them optional:

1. **`Action` is `ActionRolling` for every Fargate step.** Feed it into the
   existing eviction-budget and PDB accounting as a pod recreation. Fargate
   cannot resize in place, and `PlanSteps` refuses to emit anything else.
2. **Call `fargate.Domain.RecordApplied(step, now)` after every completed
   step.** Without it the post-change regression watch is never armed, so a bad
   shave is never detected and never reverted. This is the single most
   load-bearing line of the wiring.
3. **Persist `Checkpoint()` and `Restore()` through `pkg/store`** under the
   §5.8 `domain/<kind>/<scope>/…` buckets. The checkpoint carries pending
   reverts; losing it strands a pod at the size that broke it
   (`TestPendingRevertSurvivesARestart`).

Approval and the ledger reuse the existing machinery unchanged:
`domain.Fingerprint(steps)` over the ordered step list, `Step.Key` as the
idempotency key, and the ledger entry gaining a `domain` field per §5.6.
