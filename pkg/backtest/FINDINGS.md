# pkg/backtest — findings

Unit 2 of `docs/design/reasoning-engine.md` §9 (§4.4 is the specification):
the backtesting harness, scorecard, oracle and policy gate.

**Nothing outside `pkg/backtest/` was touched.** `go.mod`/`go.sum` are
unchanged; the package imports only the standard library and
`pkg/{model,evidence,recommend,plan,decision,pricing,guard,patterns}`. There is
no clock, no randomness and no environment read in any non-test source — a
test parses the sources and asserts it (`TestNoAmbientInputsInTheScoringPath`).

CLI wiring (`kilter backtest`) is **not** in this unit; it is specified in
[CLI surface](#cli-surface-what-a-later-unit-must-add) below.

| | |
|---|---|
| production files / lines | 6 / 2,073 |
| test files / lines | 10 / 2,398 |
| top-level tests | 70 |
| subtests | 122 |
| fuzz targets | 2 |
| statement coverage | 95.0% |
| `go test -race ./pkg/backtest` | green |

---

## Why this unit exists, and what it now says

Everything shipped before this unit — twelve domains, an evidence substrate, a
decision layer — asserted its own quality. This package turns that into a
number and, on its very first run, **falsified the shipped engine**:

```
regime-change trace, 7 days, 3 workloads, default policy
  decisions 18   memViolations 3   cpuStarvation 3   regretUSD $303.86
```

Three of eighteen container-days would have been sized from pre-shift history
and starved after a level shift. That is not a harness bug; it is the engine,
measured. See [The first real result](#the-first-real-result-wiring-pkgdecision-is-worth-half-the-regret).

---

## Surface

```go
type SnapshotSource interface {
    Snapshots(cluster string, from, to time.Time) ([]*model.ClusterSnapshot, error)
}
type SliceSource []*model.ClusterSnapshot   // in-memory implementation

type Harness struct {
    Evidence evidence.Store   // optional: events + OOM ground truth
    History  SnapshotSource   // required: the replay stream
    Rec      recommend.Config // ┐
    Plan     plan.Config      // ├ the policy under test
    Decision decision.Config  // ┘
    EnforceDecisionRefusals bool
    Catalog  *pricing.Catalog // nil → pricing.Embedded()
    Scoring  Config           // measurement knobs, not policy
}
func (h *Harness) Run(cluster string, from, to time.Time, horizon time.Duration) (*Scorecard, error)

func Gate(current, candidate *Scorecard, tol Tolerance) (ok bool, reasons []string)
func PolicyHash(recommend.Config, plan.Config, decision.Config) string
func CostModelFromCatalog(*pricing.Catalog, *model.ClusterSnapshot, CostModel) (CostModel, error)

type TraceSpec struct{ /* … */ }          // synthetic histories with known oracles
func (TraceSpec) Build() (*Trace, error)
```

`Scorecard` carries every field §4.4 names (`Policy`, `Window`, `Decisions`,
`Refusals` by code, `MemViolations`, `CPUStarvation`, `OracleGapPct`,
`ClaimedVsRealized`, `FlipRate`, `RegretUSD`) plus the terms that make them
auditable: `PolicyCostUSD`/`OracleCostUSD`, `ResourceRegretUSD`/`RiskRegretUSD`,
`ClaimedSavingsUSD`/`RealizedSavingsUSD`, `ForgoneSavingsUSD`,
`RefusalsGood`/`RefusalsIdle`, `MemOOMKills`, `Flips` and a `Skipped` block
that accounts for every pair the harness declined to score.

---

## The four traps, and what was done about each

### 1. The oracle is computed independently of the policy

Two structural guarantees, not conventions:

* The oracle is a pure function of `(future usage samples, the violation
  predicates, Config.StarvationFactor)`. No `recommend.Config`, `plan.Config`
  or `decision.Config` value can reach `oracle.go`. In particular the CPU
  percentile the oracle inverts is a package constant (`cpuOraclePercentile =
  0.95`, fixed by the Scorecard's own definition) and deliberately **not**
  `recommend.Config.CPUPercentile` — letting the policy pick the yardstick it
  is measured with is the trap in its purest form.
* The **set** of scored pairs is fixed before any policy runs: every eligible
  container at every decision instant is scored, whether the engine
  recommended, refused, or was filtered by the planner. A policy therefore
  cannot change *which* oracles exist, only its distance from them.

`TestOracleIsIndependentOfThePolicyUnderTest` runs five very different
policies over all four archetypes and demands byte-identical oracle sequences,
identical `OracleCostUSD` and identical `Scored`.

`TestOracleIsALowerBound` asserts the property everything rests on: no
zero-violation sizing is ever cheaper than the oracle.

### 2. Refusing everything cannot win

A refusal is not an absence of an outcome; it is the outcome "the current
sizing stays in force", and it is scored on exactly the same terms as a
resize. Refuse-everything therefore banks no savings and still pays for
whatever the unchanged sizing did.

`TestRefuseEverythingCannotDominate` proves this on the three archetypes where
the default policy causes **zero** violations, so the risk term is zero on both
sides and the comparison is pure efficiency — which means it cannot be flipped
by any choice of `IncidentUSD`. The test sweeps `IncidentUSD` over
`{$0, $1, $50, $1000}` to make that explicit, and asserts:

* the refuser plans nothing, and the default plans something (no vacuous pass);
* `refuser.RegretUSD > default.RegretUSD`;
* `Gate(default, refuser)` rejects;
* `RefusalsIdle > 0` and `ForgoneSavingsUSD > 0` — refusals are *costed*, not
  merely uncounted;
* `refuser.OracleGapPct > default.OracleGapPct`.

Two independent refuse-everything policies are tested, both realistic:
history-starved (`MinSamples` unreachable) and cluster-wide advisory mode
(`plan.DefaultMode = "recommend"`). `TestDecidingDominatesRefusingOnCalmHistory`
runs the same claim from the other side. `TestUndersizingIsPunished` is the
mirror property — a policy that saves money by taking risk shows the risk, and
the gate rejects it.

**A refusal over a turbulent window is credited**, per §4.4: `RefusalsGood`
counts refusals whose window carried a violation or a real adverse event
(OOMKill, throttle, regime change from the substrate); `RefusalsIdle` counts
the rest and prices them in `ForgoneSavingsUSD`.

### 3. Same code path

`Run` drives `recommend.Recommender.ObserveSnapshot` over every snapshot in
order and then, on the snapshot at the decision instant, calls
`Recommendations` and `plan.Build` — the identical observe-then-ask sequence
`pkg/api`'s `Ingest`/`Plan` runs, against one long-lived recommender. The
outcome sizing is read from the plan's `resize-workload` steps, so a
recommendation the planner filters is scored as "nothing happened", which is
what would really have happened.

`TestTheEngineNeverSeesTheFuture` is the leakage test: two histories identical
up to day 5 and wildly different after must produce identical decisions at
every earlier instant, and must differ afterwards (so the test cannot pass
vacuously).

### 4. Determinism

Same history plus same policy ⇒ byte-identical scorecard.

* `sumSorted` canonicalises every float sum by sorting the multiset first —
  float addition is not associative, which is the exact bug `pkg/ecs` shipped
  this week. `TestSumSortedIsOrderIndependent` also asserts that a *naive* sum
  over the same data really does vary with order, so the test is guarding a
  live hazard rather than a hypothetical one.
* Every enumeration is sorted: container keys, records (by instant then key),
  snapshots (by timestamp, with duplicate timestamps rejected as
  `ErrDuplicateSnapshot` — a tie has no defined replay order).
* `Refusals` is the one map, and `encoding/json` emits map keys sorted;
  `TestEncodeSortsRefusalCodes` pins it.
* Three determinism tests: eight repeat runs in one process (Go randomises map
  iteration on every `range`), five shuffled histories (snapshot sequence, pod
  order, usage order and workload order all permuted), and forty shuffled
  record multisets fed straight to `score()`. Plus four golden scorecards under
  `testdata/` (`go test ./pkg/backtest -update` regenerates).

---

## The first real result: wiring `pkg/decision` is worth half the regret

`pkg/decision` shipped as unit 3, but **`pkg/recommend` does not import it** —
verified, not assumed. Today a refusal predicate cannot stop a recommendation
from being planned; the decision layer is a library nothing calls in the
decision path.

`Harness.EnforceDecisionRefusals` models exactly that pending wiring, using
the shipped predicates (`decision.Evaluate`), not a copy of them. The A/B on
the regime-change trace (7 days, 3 workloads, default policy, default costs):

| | decisions | refusals | memViolations | cpuStarvation | oracle gap | resource regret | risk regret | **regret** |
|---|---|---|---|---|---|---|---|---|
| as shipped | 18 | `insufficient-history:3` | 3 | 3 | 94.5% | $3.86 | $300 | **$303.86** |
| refusals enforced | 15 | `+ post-change-soak:3` | 3 | **0** | 129.6% | $7.21 | $150 | **$157.21** |

Reading: three post-change-soak refusals remove **all** CPU starvation and
halve total regret, bought with $3.35 of extra idle headroom. `Gate` admits it.
The three memory violations survive in both columns and are *unavoidable* —
they are the window that straddles the level shift, where no policy could have
known. That distinction is exactly what the instrument is for.

`TestWiringTheDecisionLayerIsAnImprovement` pins this as a regression test,
including the assertion that the win is *paid for* (resource regret must rise),
so the scorecard reports both halves of the trade rather than only the win.

---

## Definitions that deviate from a literal reading of §4.4, and why

### Memory violations are measured against the **request**, not the limit

§4.4 writes `MemViolations // future max usage > recommended limit`. This
package uses the request. Three reasons, all load-bearing:

1. It is the **stricter** predicate. `pkg/recommend` guarantees an emitted
   limit is never below the emitted request, so request ≤ limit always.
2. It cannot be **gamed**. A policy that hid a peak behind a generous limit
   while under-reserving would score free memory it never paid for. The cluster
   would meet that peak as unreserved node memory — a node-pressure eviction
   rather than a container OOMKill, but an incident either way.
3. It matches what kilter's own policy already does:
   `targetMem = max(p99×headroom, peak)` — the request covers the peak.

The literal OOMKill is not lost: `MemOOMKills` counts the OOMKill events the
substrate actually recorded inside scoring windows. It is ground truth, cannot
move with the policy, and `Gate` rejects two scorecards that disagree on it as
"did not replay the same history".

### `ClaimedVsRealized` has no ledger to join against in a backtest

§4.4 defines it "from ledger joins where actions were applied". A backtest
applies nothing, so there is no ledger. The honest analogue used here:

* **claimed** = `cost(current) − cost(target)`, over the horizon, summed over
  planned resizes — what the engine promised at decision time;
* **realized** = the same saving measured against `max(target, oracle)`
  per dimension — the part hindsight says could have been kept, with any
  under-sizing given back.

Both are reported in dollars alongside the ratio, so the number is auditable
rather than a bare fraction. When actions *have* been applied, the ledger join
is strictly better evidence and should be reported next to this, not instead of
it — see [Deferred](#deferred-and-why).

### `FlipRate` is measured target-to-target, not target-versus-current

A backtest never applies its own advice, so the current request never moves:
measuring direction against it would label every recommendation the same
one-way change forever and report a flip rate of zero on every history. A flip
is therefore a *material move that reverses the previous material move* for the
same container within `FlipWindow` — "shrink to 229m, then grow back to 825m
two days later", which is the churn an operator actually feels. On the
regime-change golden this reports `flipRate 0.167` (the overshoot-and-correct
after the shift); under the current-request definition it would have been 0.

### The gate does not double-count efficiency

`Gate`'s rules are: comparability, ground truth agreement, no safety
regression, regret within tolerance, flip rate within tolerance — and the
oracle gap **only when regret did not strictly improve**. The gap measures the
same dollars regret already prices; gating on it unconditionally blocks exactly
the trade an operator most wants to make (a large safety win for a small,
priced amount of waste). The A/B above is precisely that case: it improves
regret 2× while the gap rises from 94.5% to 129.6%. §4.6's own dominance rule
("better regret, no safety regression, flip-rate not worse") contains no
oracle-gap term; here it is the tie-breaker. `TestGateLetsSafetyBuyEfficiency`
pins both directions.

---

## Tunable constants and why they have that value

| Constant | Value | Rationale |
|---|---|---|
| `Config.DecisionInterval` | 24h | Makes the scoring unit the container-day the oracle is defined on in §4.4. |
| `Config.StarvationFactor` | 1.0 | A violation is `future p95 > request × factor`; 1.0 means the request must cover the future p95. >1 tolerates burst, <1 demands explicit headroom. |
| `Config.FlipWindow` | 7 days | §4.4's "reversed within 7 days". |
| `cpuOraclePercentile` | 0.95 | Fixed by the Scorecard's definition of starvation. Not a policy knob — see trap 1. |
| `CostModel.CPUUSDPerCoreHour` | $0.024 | m5.large in the embedded catalog: $0.096/h for 2 vCPU + 8 GiB, split 50/50 across dimensions (the greenfield estimator's convention — neither dimension is privileged because which one binds depends on the workload mix). `TestDefaultCostModelMatchesItsStatedDerivation` fails if the catalog moves, so the comment cannot quietly become a lie. |
| `CostModel.MemUSDPerGiBHour` | $0.006 | Same derivation. |
| `CostModel.IncidentUSD` | $50 | **The load-bearing knob.** It is the exchange rate between "wasted money" and "broke the service" — the trade every rightsizing policy makes. At the rates above it prices one avoidable OOM against ~90 container-days of one wasted core. It is echoed into every `Scorecard` and `Gate` rejects a comparison across different cost models, so no scorecard can silently assume a different one. |
| `Tolerance.MaxOracleGapIncreasePct` | 2 pts | The gap is a mean over a finite sample; a fraction of a point is noise. |
| `Tolerance.MaxFlipRateIncrease` | 0.05 | One extra reversal per twenty decisions. |
| `burstEvery` (traces) | 24 samples | 12 spikes/day = 4.17% of a 288-sample window: inside the 5% tail the CPU oracle ignores, above the histogram's 1e-4 negligible-weight floor so the memory peak estimator still sees it. This is what makes the bursty archetype exhibit the CPU/memory asymmetry. |

---

## Synthetic traces: the acceptance evidence

Four archetypes whose oracles are known in closed form at the default
5-minute cadence and 24h horizon (288 samples per window):

| archetype | composition | CPU oracle | memory oracle |
|---|---|---|---|
| steady | every sample Base | Base | BaseMem |
| diurnal | 144 Peak / 144 Base | Peak | PeakMem |
| bursty | 12 Peak / 276 Base (4.17%) | **Base** | **PeakMem** |
| regime-change | Base, then Peak from the midpoint | per window | per window |

The bursty row is the point: a spike narrower than the 5% tail leaves the CPU
oracle at the baseline (a throttle is survivable) while the memory oracle must
cover the peak (an OOM is not) — the exact asymmetry `pkg/recommend`'s policy
is built on, now measurable.

`TestAnalyticOracles` asserts the harness recovers each closed form at every
instant. `TestTraceCompositionIsWhatTheOracleClaims` verifies the generator's
half of the contract (the sample mix, and that each archetype's peak share
falls on the correct side of the 5% tail), so the two tests cannot agree by
being the same mistake twice.

Per §4.4 and §10 ("clean synthetic evals overstate quality"), the fixtures are
not only clean: `NoisePct` adds deterministic ±N% jitter (a hash of
`(seed, workload, sample)`, so a sample's value depends only on where it is,
never on how many were produced before it or on iteration order), and
`DeployAt`/`OOMAt` inject real substrate events. The determinism, dominance and
lower-bound tests all run on noisy traces. The regime-change archetype lands
its shift exactly on a decision instant so the post-change-soak refusal and the
first violation are attributable to a nameable instant.

Golden scorecards for all four archetypes are in `testdata/`.

### What the goldens currently say about the shipped engine

7 days, 2 workloads, default policy, default costs:

| archetype | decisions | mem | cpu | gap (applied) | claimed/realized | flip rate | regret |
|---|---|---|---|---|---|---|---|
| steady | 12 | 0 | 0 | 17.3% | 1.00 | 0 | $2.74 |
| diurnal | 12 | 0 | 0 | 40.5% | 1.00 | 0 | $4.02 |
| bursty | 12 | 0 | 0 | 23.4% | 1.00 | 0 | $2.90 |
| regime-change | 12 | 2 | 2 | 5.7% | 0.96 | 0.17 | $202.57 |

The engine is safe and moderately tight on stationary workloads (17–40% above
hindsight-optimal, all of it deliberate headroom), and it is *unsafe across
regime changes* until the decision layer is wired in.

---

## Seams this unit needed and did not find (ordered by value)

### 1. Snapshot history persistence — the reason `SnapshotSource` exists

§9 unit 1 lists "snapshot-history persistence" in the evidence substrate's
contents, but `pkg/evidence` stores per-subject usage series and events, not
pod/node topology, and `pkg/store` keeps only the **latest** snapshot per
cluster (`SaveSnapshot`/`LoadSnapshot` are keyed by cluster, not by time).
`recommend.ObserveSnapshot` and `plan.Build` both take a
`*model.ClusterSnapshot`, so a replay that goes through the production code
path needs topology over time.

Reconstructing topology from digests would be a parallel reimplementation of
exactly the thing this package must not reimplement, so the harness takes a
`SnapshotSource` instead. **What is needed:** a time-keyed snapshot bucket in
`pkg/store` (`SaveSnapshotAt(snap)` / `Snapshots(cluster, from, to)`), bounded
the way everything else in the substrate is bounded, plus an adapter
implementing `backtest.SnapshotSource`. Until it lands, `kilter backtest` can
only run against traces and recorded fixtures, not against a live cluster's
own history — which is the headline user-facing feature of §4.4.

Storage note for whoever builds it: at a 5-minute cadence a 30-day window is
8,640 snapshots per cluster. Full snapshots are far too large; the realistic
shape is a keyframe-plus-delta encoding (topology changes rarely; usage does
not), or a reduced "replay snapshot" carrying only what `recommend` and `plan`
read.

### 2. `Recommender.Verdicts(snap)` — "which containers did you consider?"

`recommend.Recommendations` returns only the containers it decided to
recommend. It never says which it considered and skipped, so the harness
duplicates the *eligibility filter* (Running pods, excluding bare pods and
Job/CronJob, deduplicated by container key) in `eligibleContainers`. It
duplicates a filter, not a decision — no percentile, threshold or policy value
is involved — and `TestScoredSetMatchesRecommenderEligibility` pins the two
together. It is still duplication, and it will drift.

**What is needed:** `func (r *Recommender) Verdicts(snap) []decision.Verdict`
(§8 already sketches the type), one entry per considered container, carrying
the recommendation *or* the refusal. That single seam removes both this
duplication and the next two items.

### 3. `pkg/decision` is not wired into `pkg/recommend`

Consequence: refusal codes cannot be read out of the production path, so the
harness evaluates `decision.Evaluate` itself — the shipped predicates, but at a
call site production does not have. When `Verdicts` lands, `refusalCode` and
`EnforceDecisionRefusals` should both collapse into "read the verdict".

The A/B above is the business case for doing it, with a number.

### 4. `decision.Evidence` fields the harness cannot honestly fill

Filled today: `Samples`, `Window`, `LastSample` (harness bookkeeping),
`Class` (from the recommendation), `ShrinkIndicated` (a fact about the
recommendation), and `LastChange`, `OOMsInWindow`, `ThrottledInWindow`,
`LastChangepoint` (from the substrate).

Left zero, i.e. "signal absent, no grounds to refuse":
`ClassStabilityKnown`/`ClassStability`, `LastClassFlip`, `HPAThrashPerHour`,
`BuiltinForecast`/`RemoteForecast`, `SLODegraded`, `Quarantined`. Four refusal
codes — `class-unstable`, `signal-conflict` (partially), `forecast-divergence`,
`sla-degraded`, `quarantined` — therefore **cannot fire in a backtest today**.
They need collectors (unit 1 follow-ups) or the `Verdicts` seam. Any scorecard
whose `Refusals` map lacks these codes is showing a gap in instrumentation, not
an engine that never doubts.

### 5. `learnState` mirrors the recommender's history counters

`observe` re-derives sample counts and first/last timestamps, duplicating (and
depending on) `ObserveSnapshot`'s garbage guards. Same fix: `Verdicts`, or a
small `func (r *Recommender) History(key) (samples int, first, last time.Time)`.

---

## CLI surface: what a later unit must add

Not built here (out of scope), specified so it can be built without
re-deriving anything. Wiring lives in `cmd/`, not in this package.

```
kilter backtest --cluster <id> [--from 30d|RFC3339] [--to RFC3339]
                [--horizon 24h] [--interval 24h] [--starvation 1.0]
                [--incident-usd 50] [--derive-costs]
                [--policy default|<file.json>] [--compare <file.json>]
                [--enforce-refusals] [--json] [--fail-on-regression]
```

* `--from 30d` parses a duration relative to the newest snapshot in the store,
  **never** `time.Now()` — the replay window must come from the history, or two
  runs over the same data disagree.
* `--policy` / `--compare` load `{recommend, plan, decision}` config triples.
  With `--compare`, print both scorecards side by side plus `Gate`'s verdict
  and reasons.
* `--json` writes `Scorecard.Encode()` verbatim (byte-stable, CI-diffable).
  Exit non-zero with `--fail-on-regression` when `Gate` rejects — that is the
  CI gate §4.4 asks for.
* `--derive-costs` calls `CostModelFromCatalog` on the newest snapshot instead
  of the built-in rates.
* The command needs the snapshot-history seam above. Until it lands, ship
  `kilter backtest --demo <archetype>` over `TraceSpec` so the output format,
  the gate and the exit codes can be integrated and reviewed against known
  numbers.
* `pkg/api`: `GET /api/v1/clusters/{id}/backtest?from=&horizon=` returning the
  same JSON, and the proposal object of §8 gains its `Scorecard` here.

---

## Invariants and where they are tested

| Invariant | Test |
|---|---|
| The oracle does not depend on the policy | `TestOracleIsIndependentOfThePolicyUnderTest` |
| The scored set does not depend on the policy | same test (`Scored` and record count) |
| No safe sizing is cheaper than the oracle | `TestOracleIsALowerBound` |
| The oracle is exactly on the safe-set boundary | `TestOracleRequestInvertsThePredicates` |
| The harness recovers the analytic oracle | `TestAnalyticOracles` (+ `TestTraceCompositionIsWhatTheOracleClaims`) |
| Refuse-everything never dominates | `TestRefuseEverythingCannotDominate`, `TestDecidingDominatesRefusingOnCalmHistory` |
| Undersizing is detected and priced | `TestUndersizingIsPunished` |
| A refusal over a turbulent window is credited | `TestRefusalOverATurbulentWindowIsCredited` |
| The engine never sees the future | `TestTheEngineNeverSeesTheFuture` |
| The scoring window excludes the decision instant | `TestScoringWindowExcludesTheDecisionInstant` |
| Decisions + refusals = scored, every refusal named | `TestEveryScoredPairIsAccountedFor`, `FuzzScoreInvariants` |
| Every refusal path is reachable and distinct | `TestRefusalCodeTaxonomy`, `TestEnforcedRefusalsSurfaceDecisionCodes` |
| Byte-identical across runs / shuffled history / shuffled records | three tests in `determinism_test.go` |
| Sums do not depend on addend order | `TestSumSortedIsOrderIndependent` |
| No clock, randomness or environment in the scoring path | `TestNoAmbientInputsInTheScoringPath` (AST scan) |
| Truncated horizons are skipped, not scored | `TestDecisionInstantsSnapForwardAndRequireAFullHorizon` |
| Duplicate snapshot timestamps are rejected | `TestRunRejectsDuplicateSnapshotTimestamps` |
| Only rightsizable workloads are scored | `TestScoredSetMatchesRecommenderEligibility` |
| The gate fails closed and cannot be widened by garbage | `TestGateFailsClosedOnNil`, `TestGateToleranceIsHonouredAndCannotBeWidenedByGarbage` |
| Incomparable scorecards are never compared metric-by-metric | `TestGateStopsAtIncomparability` |
| No scorecard field is ever NaN or ±Inf | `FuzzScoreInvariants` |
| The trace generator is total and pure | `FuzzTraceSpecBuild`, `TestTraceIsAPureFunctionOfItsSpec` |

---

## Deferred, and why

* **Ledger join for `ClaimedVsRealized`.** `pkg/api/ledger.go` holds
  claimed-vs-measured for actions that were actually applied. Joining it needs
  a `cmd/`-side wiring this unit may not touch, and the oracle-based analogue
  above is well defined without it. When it lands, report both — they answer
  different questions ("could the saving have been kept" vs "was it").
* **`kilter whatif` (unit 5).** It is this harness with a config override and a
  scorecard diff; `Gate` and `PolicyHash` are already the pieces it needs.
* **Recorded kind-cluster fixtures.** §4.4 asks for them alongside the
  synthetic traces. They need the snapshot-history seam to record through, and
  a fixture format decision that belongs with it.
* **Checkpoint restore as a replay accelerator.** §4.4 mentions "or restores a
  checkpoint". `recommend.Checkpoint`/`Restore` exist, but a full replay is
  already O(snapshots) with one long-lived recommender — 2,016 snapshots score
  in ~20ms. Checkpoints matter for resuming a very long replay, not for speed
  here, and every checkpoint boundary is a place replay fidelity could silently
  differ from production. Not worth the risk until a real workload demands it.
* **A `--parallel` replay.** Deliberately not done. The replay is inherently
  sequential (state at *t* depends on everything before it), and the one place
  parallelism would help — independent clusters — is better solved by running
  the command once per cluster.

---

## Known tension (not a bug)

`OracleGapPct` includes refusals, `OracleGapPctApplied` does not. §4.4's
one-line comment (`mean( (recommended − oracle) / oracle )`) reads like the
latter, but a gap computed over recommendations only is trivially gamed by
refusing the hard cases — the same hole as trap 2, one metric down. Both are
reported: read `OracleGapPct` for "how good is this policy", and
`OracleGapPctApplied` for "how good are the sizes it picks". `Gate` uses the
former.
