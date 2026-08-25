# Kilter Reasoning Engine — Design

> Status: design (Iteration 5 candidate). No code in this document is built yet.
> Scope: the core reasoning/decision engine that monitors continuously and infers/judges
> across compute domains (EKS-Fargate, EKS-EC2, plain EC2), plus the agentic/LLM layer
> that makes those judgments explainable, interrogable, and enterprise-trustworthy.

This design builds **on** the existing decision math — the decaying histograms
(`pkg/histogram`), the online classifier (`pkg/patterns`), EWMA/Holt-Winters and the
remote forecaster contract (`pkg/forecast`), class-adaptive sizing with OOM floors and
HPA guards (`pkg/recommend`), the constraint-aware planner with deterministic
fingerprints (`pkg/plan`), the safety/guard envelope (`pkg/safety`, `pkg/guard`), and
the audit ledger with claimed-vs-measured savings (`pkg/api/ledger.go`). Nothing here
replaces that stack. The design adds four things:

1. an **evidence substrate** that widens what the engine can see beyond CPU/memory
   percentiles, stored compactly enough for 50k workloads;
2. **decision-quality machinery**: structured confidence with first-class refusal,
   anomaly-vs-trend discrimination, counterfactuals, and a backtesting harness that
   scores past recommendations against what actually happened;
3. an **agentic reasoning layer** (LLM) that explains, correlates, hypothesizes and
   proposes — but never sizes;
4. an **MCP surface** so enterprises can drive kilter from their own AI tooling.

Everything obeys the standing rules: every integration is an organ, not a heart; no new
dependency in the decision path; the core stays a single static air-gap-capable binary;
`go test -race ./...` green at every commit.

---

## 1. Reasoning architecture: where inference belongs

### 1.1 The four planes

```
             ┌───────────────────────────────────────────────────────────────┐
             │  L3  REASONING PLANE (optional organ — LLM)                   │
             │  explain · correlate · hypothesize · interrogate · propose    │
             │  read-only tools + proposal objects; NEVER in decision path   │
             └───────────────▲───────────────────────────┬───────────────────┘
                             │ tools (read)              │ proposals (gated by
                             │                           │ approval + backtest)
             ┌───────────────┴───────────────────────────▼───────────────────┐
             │  L2  DECISION PLANE (deterministic, replayable)               │
             │  recommend · plan · binpack · safety · guard · refusal        │
             └───────────────▲───────────────────────────────────────────────┘
                             │ features, classes, forecasts, changepoints
             ┌───────────────┴───────────────────────────────────────────────┐
             │  L1  STATISTICAL PLANE (online, O(1)/sample, in-path)         │
             │  decaying histograms · classifier · EWMA/HW · spike/changepoint│
             │  (+ optional remote TS foundation model via --forecaster-url) │
             └───────────────▲───────────────────────────────────────────────┘
                             │ snapshots, usage, events
             ┌───────────────┴───────────────────────────────────────────────┐
             │  L0  EVIDENCE SUBSTRATE (append-mostly, bounded, queryable)   │
             │  usage series · k8s events · deploys · throttling · ledger    │
             └───────────────────────────────────────────────────────────────┘
```

L0–L2 are the **decision path**: pure Go, stdlib-first, deterministic, fuzzable,
air-gap-complete. L3 is an **advisory plane**: it can read everything, decide nothing.
The remote forecaster (`--forecaster-url`) is the precedent: an optional model server
that improves one signal (long-horizon demand) with a built-in fallback and zero
availability coupling. The reasoner gets the same treatment at a bigger scope.

### 1.2 Stress-testing the prior: "an LLM must never pick a CPU number"

The prior deserves an honest adversarial pass, because "LLM picks the number" is
exactly what several 2025-era products gesture at, and if it were right we should adopt
it. Steelman for LLM-in-the-loop sizing:

- *An LLM could integrate context no scalar model sees* — "this deploy doubled the
  worker pool; the P95 history is stale" — and adjust the number accordingly.
- *An LLM could interpolate priors for cold-start workloads* from the workload's name,
  image, labels ("this is a JVM service; size the heap accordingly").
- *Frontier models are decent at numeric estimation* and the number is bounded anyway
  by limits/quota.

Why each fails as a production sizing mechanism:

| Claim | Refutation |
|---|---|
| Integrates context | True — but the *right* fix is to feed that context into the deterministic policy as evidence (deploy-aware histogram reset, §4.3), which is testable and applies uniformly to 50k workloads. An LLM applying it ad hoc is unauditable per-workload luck. |
| Cold-start priors from names/images | Names and images are **attacker-controlled input** (any pod author chooses them). A sizing path that reads them is a prompt-injection channel *into resource allocation*. Kilter already has trace-derived priors (`pkg/patterns` Borg/Alibaba constants), clearly labeled, deterministic. |
| Good at estimation | "Decent" is the problem. A histogram percentile has a definition; a sampled token has a distribution that shifts with every model revision. Two runs give two numbers; a model upgrade silently re-sizes the fleet. There is no `go test` for it, no fuzz harness, no replay. Kilter's flagship trust feature — the deterministic simulator, "every decision replayable before it touches prod" — dies. |
| Bounded blast radius | A wrong CPU request is not benign: memory too low → OOM cascade; CPU too low → CFS throttling → latency SLO breach; too high → scheduling pressure → capacity exhaustion. The bound is the incident. |
| — | **Scale/economics**: the brain re-evaluates every container on every snapshot (§`pkg/recommend` `Recommendations()` is called per API request; ingest is ns-scale per sample). 50k workloads × even one LLM call/day is ~$100–500/day of inference to replace microseconds of arithmetic that is *already correct*. The LLM adds cost and subtracts properties. |
| — | **Air-gap**: the core must produce identical-quality sizing with no model reachable. If the LLM improves the number, air-gapped users get a worse product; if it doesn't, it shouldn't be there. |

**Verdict: the prior is confirmed, and we sharpen it into an invariant** (enforced by
construction, not convention):

> **INV-1 (numeric sovereignty).** Every number that reaches a cluster — requests,
> limits, node counts, plan steps — is computed by deterministic code from the evidence
> substrate. LLM output influences those numbers through exactly one channel: a
> **policy proposal object** (bounded parameter deltas within schema-validated ranges)
> that must pass the backtest gate and the existing human approval gate
> (`kilter approve`, plan fingerprints) before it changes configuration. There is no
> code path from model output to `plan.Step` or `recommend.Recommendation` fields.

Mechanically: the reasoner package (`pkg/reason`) imports `recommend`/`plan` types to
*read* them; `recommend`/`plan`/`actuate` never import `pkg/reason`. The Go import
graph is the enforcement — a one-line CI check (`go list -deps`) keeps it true forever.

### 1.3 What the LLM plane is genuinely for

The five uses in the mission statement survive scrutiny, and each maps to a concrete
deliverable:

| Use | Why an LLM (and not more Go) | Deliverable |
|---|---|---|
| (a) Trustworthy explanations | The engine already emits machine reasons (`class=bursty (cv=1.8 ...)`). Platform teams must defend a resize to an app team in *their* language: "we're cutting your request 40% because 30 days of history show p95=210m; your HPA is untouched; here's the rollback path." Template prose can't adapt to audience or evidence shape. | `explain` tool + API: narrative over the deterministic explain payload (§5.4), never over raw metrics. |
| (b) Correlating heterogeneous evidence | "Cost rose 18% on the 14th" has candidate causes across domains: a deploy, an HPA max bump, a spot→on-demand fallback, a pricing sync, a new namespace. Deterministic code enumerates and quantifies the candidates (§4.5 cost attribution); ranking and narrating a *coherent causal account* across them is judgment. | `investigate` loop over read-only tools; output cites evidence IDs. |
| (c) Hypothesis generation | "Why is this workload bursty at 03:00 UTC?" — the classifier says *that*, not *why*. Cross-referencing deploy history, cron-shaped siblings in the namespace, and the business calendar to propose "nightly batch kicked off by X" is abduction, the LLM's comparative advantage. Hypotheses are labeled as such and are never inputs to sizing. | hypothesis section of investigations; feeds humans, not the planner. |
| (d) Natural-language interrogation | "What would break if we moved payments to spot?" decomposes into tool calls kilter already answers (spot-safety score, PDB slack, replica count). The LLM is a **query planner over enumerated tools**, not an oracle. | `kilter ask` / `POST /ask` + the same tools over MCP. |
| (e) Proposing policy, not applying it | Closed-loop tuning (§4.6) produces candidate parameter deltas mechanically; the LLM can also draft them from investigation findings ("payments-api regressed twice after resize; propose `kilter.dev/mode: recommend` + memory headroom 1.3 for this namespace"). Both funnel into the same gated proposal object. | `propose_policy_change` tool → proposal → backtest gate → approval gate. |

### 1.4 The safety argument, stated once

Trust for an autonomous optimizer decomposes into four properties; the boundary
preserves all four with the LLM present, absent, wrong, or actively hostile:

1. **Determinism where it counts.** Same snapshot + same learned state + same config →
   same plan, same fingerprint (`plan.fingerprint()` at `pkg/plan/plan.go:532`). The
   LLM cannot touch this because of INV-1's import-graph enforcement.
2. **Bounded authority.** The reasoner's write authority is *zero*. Its proposals enter
   the same funnel as a human's config change: schema validation → backtest gate →
   fingerprinted approval with TTL. Prompt injection therefore caps out at "a bad
   paragraph or a bad proposal a human must approve" — annoying, not an incident.
3. **Full auditability.** Every investigation is a recorded artifact (§5.6): inputs,
   every tool call and result hash, model ID, token spend, output. The ledger answers
   "what did kilter do"; the investigation log answers "what did the AI see and say."
4. **Fail-static.** Model server down/slow/gone ⇒ decisions continue at full quality;
   only prose degrades (template reasons remain). Identical to the forecaster fallback
   behavior in `pkg/api/capacity.go:90-97`.

---

## 2. Compute domains: one engine, three actuation dialects

The reasoning engine is domain-agnostic; domains differ in *what is actionable*, which
is encoded in the evidence and honored by the decision plane:

| Domain | What kilter optimizes | Decision-plane differences |
|---|---|---|
| **EKS on EC2** (today's core) | requests/limits, node consolidation, spot mix, node lifecycle via `pkg/provider` | full engine as shipped |
| **EKS on Fargate** | requests/limits only — Fargate bills the *pod* by rounded-up vCPU/GB combos | binpack/consolidation are no-ops; pricing maps pod size → Fargate combo table; rightsizing savings are computed per-pod, not per-node. Detection: `eks.amazonaws.com/compute-type: fargate` / Fargate node labels → `NodeSpec.Domain=fargate`. New: recommendation *snapping* to the Fargate vCPU/memory lattice so a 0.3 vCPU target doesn't silently bill as 0.5. |
| **Plain EC2 / ASG (non-K8s)** (roadmap) | instance rightsizing, family migration, spot | same L0/L1 machinery over a different collector (CloudWatch/agent series instead of kubelet); `ContainerKey` generalizes to `SubjectRef{Kind: instance}`. The decision plane grows an instance-recommendation policy reusing histograms/classifier verbatim — they are already resource-agnostic float series. |

Design consequence now (cheap), implementation later: `model.NodeSpec` gains a
`Domain` field, pricing gains a Fargate combo table, and the evidence substrate keys on
a `SubjectRef` that is a superset of today's `ContainerKey`/node name. Nothing else in
this document is domain-specific.

---

## 3. Evidence substrate (`pkg/evidence`)

### 3.1 What's missing today

The brain currently learns from usage samples plus a handful of in-band signals
(restarts, `LastOOMKilled`, HPA presence, PDBs). It cannot see *why* behavior changed
— and several trust-critical judgments (post-deploy soak, throttling-aware CPU floors,
HPA thrash, spike-vs-regime-change) are impossible without more signals. The store
keeps only the **latest** snapshot per cluster (`pkg/store/store.go`, `bucketSnapshots`
is keyed by cluster only), so there is no history to backtest against. Both gaps are
fixed here.

### 3.2 Signal inventory

All signals are collected by the existing agent (client-go informers + kubelet), stay
inside the cluster→brain path, and are **structured** — kilter never ingests log lines
or env vars (that is a security stance, not a limitation; see §5.7).

| Signal | Source | Cost | Feeds |
|---|---|---|---|
| Deploy/rollout events (image, replicas, resources, generation) | workload informers (spec hash diff) | free (already watched) | deploy-aware decay, post-change soak, cost attribution, regression detector baselines |
| HPA scale events + min/max/target changes; thrash score (direction flips/hour EWMA) | HPA informer | free | refusal ("HPA thrashing — sizing unreliable"), insight `hpa-thrash` |
| CFS throttling (`throttled_periods/total_periods`) | kubelet cAdvisor metrics (already scraped endpoint family) | one more field per container sample | CPU floor guard: never shrink a throttling container; insight `cpu-throttled`; backtest scoring |
| OOMKill events (exact, with container + timestamp) | K8s Events + status (today: restart-count heuristic) | free | OOM floor with decay (§4.3), regression detect |
| Pod lifecycle warnings (FailedScheduling, Evicted, ProbeFailure, ImagePullBackOff) | K8s Events informer (Warning only, deduped) | bounded ring | breaker context, investigation evidence |
| Node pressure conditions + allocatable changes | node informer | free | breaker, capacity insights |
| Spot interruptions / rebalance recommendations | existing NTH-taint path | free | already used; recorded as evidence now |
| Pricing changes (catalog sync deltas) | `kilter pricing sync-*` | free | cost attribution ("your cost rose because spot price rose") |
| Request latency / SLO, queue depth | **optional organ**: `--metrics-url` Prometheus HTTP API, operator-declared queries per workload (annotation `kilter.dev/slo-query`) | opt-in | SLO-aware refusal ("p99 latency degraded after last resize"), never required |
| Business calendar (freeze dates, known peaks: Black Friday…) | static config file / ConfigMap | free | seasonality context for forecasts + refusal near declared peaks |
| Kilter's own actions | ledger (exists) | free | closed loop; joined into the same timeline |

### 3.3 Storage model — compact by construction

Three stores, all bbolt (no new deps), all bounded:

**(a) Event log.** Append-mostly, per-subject ring.

```
key:   evt/<cluster>/<subjectKind>/<subjectKey>/<unixNano>
value: EvidenceEvent (JSON, ~200-400B)
bound: last 256 events per subject AND 90 days, pruned on write
```

```go
// SubjectRef generalizes "what the evidence is about".
type SubjectRef struct {
    Kind string // "container" | "workload" | "node" | "cluster"
    Key  string // ContainerKey.String() | WorkloadRef.String() | node name | cluster id
}

type EvidenceEvent struct {
    At       time.Time         `json:"at"`
    Kind     string            `json:"kind"`     // deploy | hpa-scale | oomkill | throttle-high | evicted | spot-interrupt | pricing-change | kilter-action | ...
    Subject  SubjectRef        `json:"subject"`
    Severity string            `json:"severity"` // info | warning | critical
    Attrs    map[string]string `json:"attrs,omitempty"` // small, allowlisted keys; values length-capped
    // Dedup collapses informer replays and repeated warnings.
    Dedup string `json:"dedup,omitempty"`
}
```

Worst case 50k containers × 256 × 400B ≈ 5 GB *only if every subject is saturated*;
real clusters are sparse (most workloads emit near-zero events). A global byte budget
(default 1 GiB) triggers oldest-first eviction, surfaced as a metric.

**(b) Series history (backtest fuel).** Today's "latest snapshot only" becomes an
RRD-style tiered digest, storing *aggregates*, not raw snapshots:

```
tier 0: per-container 5-min usage samples ......... 48h   (already in detector rings, now persisted)
tier 1: per-container hourly digest ............... 30d   {p50,p95,p99,max,samples,throttleRatio,restarts,ooms}
tier 2: per-container daily digest ................ 400d  same shape
tier 3: cluster-level cost/demand timeline ........ 400d  extends ledger costHist beyond 1 week
```

An hourly digest is ~64B binary-encoded; 50k containers × 24 × 30 ≈ 2.3 GB/month at
tier 1 — too fat. Fix: tier 1 stores digests **only for containers whose day was not
"boring"** (a digest within tolerance of the previous one is run-length-coalesced).
Steady workloads — the majority — compress to a handful of digests per week. Budgeted
like the event log; measured, not hoped: the scale soak test (5k nodes/50k pods) gains
a storage-size assertion.

**(c) Decision journal.** Every recommendation actually *surfaced* (not each internal
recompute) and every refusal, keyed like events. This is what backtesting scores and
what "why did kilter say that in March" replays. Bounded: last 100 per container.

### 3.4 Query surface

Deterministic accessors (no query language, no new deps):

```go
type Store interface {
    Append(ev EvidenceEvent) error
    Events(s SubjectRef, from, to time.Time, kinds ...string) ([]EvidenceEvent, error)
    Digests(s SubjectRef, from, to time.Time, tier int) ([]Digest, error)
    Timeline(cluster string, from, to time.Time) ([]TimelinePoint, error) // cost + node count + events overlay
    Decisions(s SubjectRef, from, to time.Time) ([]DecisionRecord, error)
}
```

On top of it, one composition function is the workhorse for humans, the UI, the LLM,
and MCP alike — the **dossier**:

```go
// Dossier is the bounded, serializable case file for one subject.
// Size-capped (~4 KiB JSON) so it is a safe retrieval unit for LLM context.
type Dossier struct {
    Subject     SubjectRef            `json:"subject"`
    Class       patterns.Class        `json:"class"`
    Features    patterns.Features     `json:"features"`
    Usage       UsageSummary          `json:"usage"`      // p50/p95/p99/max cpu+mem, window, samples
    Sizing      *recommend.Recommendation `json:"sizing,omitempty"`
    Refusal     *decision.Refusal     `json:"refusal,omitempty"`
    Events      []EvidenceEvent       `json:"events"`     // last N, newest first
    Decisions   []DecisionRecord      `json:"decisions"`  // recent recommendation/action history
    Guards      GuardSummary          `json:"guards"`     // mode, windows, quarantine, HPA/PDB facts
    CostMonthly float64               `json:"costMonthlyUSD"`
}
```

---

## 4. Decision-quality machinery

### 4.1 Structured confidence

Today confidence is one float (`recommend.confidence()`, blending samples/window/
volatility) filtered by `MinConfidence` in `plan.Build`. It becomes a value with a
basis, so every number can say why it is believed:

```go
type Confidence struct {
    Score float64           `json:"score"` // 0..1, same semantics as today (back-compat)
    Basis []ConfidenceTerm  `json:"basis"` // each: name, value, weight, note
}
// terms: history-depth, window-span, volatility, class-stability,
//        post-change-soak, signal-agreement, forecast-agreement
```

`class-stability` (fraction of recent re-classifications agreeing) and
`post-change-soak` (time since last deploy/resize vs required soak) are new and
computable from the substrate. Plan filtering keeps using `Score`; nothing downstream
breaks.

### 4.2 Refusal is a first-class output

The engine currently *silently omits* recommendations below thresholds (`nil` return
in `recommendOne`). Silence is the wrong shape for trust: "no recommendation" and "we
refuse to recommend, because X, until Y" are different facts. New:

```go
type Refusal struct {
    Code   string    `json:"code"`   // enumerated below
    Detail string    `json:"detail"` // human-readable, deterministic
    Until  time.Time `json:"until,omitempty"` // when the condition likely clears
}
```

Refusal codes (each a tested predicate, evaluated in order, first match wins):

| Code | Predicate |
|---|---|
| `insufficient-history` | samples < MinSamples or window < MinWindow (today's silent skip) |
| `post-change-soak` | deploy/resize/scale event within soak window (default 6h; class-scaled) |
| `class-unstable` | classifier flip within last 24h or `class-stability < 0.7` |
| `signal-conflict` | e.g. shrink indicated but OOM/throttle events in window; or HPA thrash score high |
| `regime-change-pending` | changepoint detected (§4.3), post-changepoint window still short |
| `forecast-divergence` | remote and built-in forecasters disagree beyond tolerance on the horizon that matters |
| `sla-degraded` | optional SLO signal breached since kilter's last change |
| `quarantined` | existing regression quarantine (`safety.RegressionDetector`) surfaced as refusal rather than silence |

Refusals appear in `/insights`, in the dossier, in `kilter recommend` output, and in
the UI. They are also the honest answer the LLM relays; the agent never papers over an
abstention. **A system that can say "I don't know yet" earns the right to be believed
when it says "I know."**

### 4.3 Anomaly vs. trend vs. regime change

Failure mode being closed: a one-off spike (backfill job, incident retry-storm)
permanently inflates sizing; or a genuine regime change (new feature doubles memory) is
treated as an outlier and under-provisioned.

Mechanisms, all L1 (online, O(1)):

1. **Changepoint detector** per series alongside the pattern detector: two-sided CUSUM
   (Page-Hinkley) on the sample stream with drift/threshold scaled by the series'
   EWMA variance. Emits `regime-change` evidence events with direction and magnitude.
2. **Classification of exceedances.** A sample far above baseline is scored against
   three hypotheses with cheap statistics: *spike* (isolated, reverts within k
   samples), *new season* (recurs at ~24h/7d lag — checked against the existing
   autocorrelation machinery), *regime change* (CUSUM fires and level holds).
   Consequences:
   - *spike* → excluded from the **memory peak** term (`st.mem.Max()` in
     `recommendOne`) via a spike-robust peak: `max(p99.9-of-decayed-histogram,
     verified-sustained-peak)`, so one 30-second balloon no longer sets sizing forever.
     The OOM floor is unaffected (an OOM is an OOM).
   - *regime change (up)* → histograms get a **fast-forward decay** (one-off extra
     half-life application to pre-changepoint weight, the same math as
     `shiftRef`), so the engine re-learns in hours, not days; refusal
     `regime-change-pending` guards the interim.
   - *regime change (down)* (e.g. rollback, feature flag off) → same fast-forward,
     but shrink recommendations additionally require the post-changepoint window to
     exceed the class soak.
3. **Deploy-aware learning.** A deploy event with an image change triggers the same
   fast-forward decay per affected container (spec changed ⇒ old distribution is
   stale-biased). A deploy with *only* replica change does not (per-replica behavior
   is what we model).
4. **OOM floor decay.** `oomFloorBytes` is currently permanent
   (`pkg/recommend/recommend.go:88-91`). It becomes evidence-backed: the floor holds
   for 14 days since the *last* OOM, then relaxes 10%/week toward the observed-peak
   term, and any new OOM re-arms it. The floor, its age, and its schedule are in the
   dossier.

### 4.4 Backtesting harness (`pkg/backtest`) — first-class

The claim "kilter's judgment is good" must be a number computed from the user's own
cluster history, not marketing. The harness replays stored history through the exact
production code path (the same property the simulator already provides for plans) and
scores decisions against what actually happened afterward.

```go
type Harness struct {
    Evidence evidence.Store
    Rec      recommend.Config     // policy under test
    Plan     plan.Config
}

// Run replays [from,to) in snapshot order. At each decision instant t it:
//   1. reconstructs learned state from evidence up to t (or restores a checkpoint),
//   2. asks the engine for recommendations/refusals with policy P,
//   3. scores them against evidence in (t, t+horizon].
func (h *Harness) Run(cluster string, from, to time.Time, horizon time.Duration) (*Scorecard, error)

type Scorecard struct {
    Policy         string  // hash of the Config pair (like plan fingerprints)
    Window         [2]time.Time
    Decisions      int
    Refusals       map[string]int // by code
    // Safety (would the recommendation have hurt?)
    MemViolations  int     // future max usage > recommended limit (would-OOM)
    CPUStarvation  int     // future p95 > recommended request × starvation factor (would-throttle)
    // Efficiency (did it save?)
    OracleGapPct   float64 // mean( (recommended − oracle) / oracle ), oracle = min safe size in hindsight
    ClaimedVsRealized float64 // from ledger joins where actions were applied
    // Stability
    FlipRate       float64 // recommendations reversed within 7 days / decisions
    // Overall regret in dollars: cost above oracle + risk events priced at a configured incident cost
    RegretUSD      float64
}
```

Key properties:

- **The oracle is defined**, not vibes: for each container-day, the cheapest
  request/limit that would have produced zero violations in hindsight over the
  horizon. Regret = actual policy cost − oracle cost, plus a priced penalty per
  violation. This makes "conservative vs aggressive" a curve you can plot, not an
  argument.
- **Refusals are scored too**: a refusal whose window contained a violation is a *good*
  refusal (counted as avoided risk); a refusal over a boring window is costed as
  foregone savings. Refusing everything cannot game the scorecard.
- **Same code path**: the harness calls `recommend.Recommender` and `plan.Build`
  exactly as the brain does — a policy that scores well *is* the policy that will run.
- CLI: `kilter backtest --cluster c --from 30d --policy default \
  [--compare candidate.json]` prints scorecards side by side; JSON output for CI.
- The harness is also the **gate**: no policy change (human, closed-loop, or
  LLM-proposed) is applied unless its scorecard is ≥ current policy on safety metrics
  and within tolerance on efficiency. The gate result is attached to the proposal
  object the approver sees.

Test strategy for the harness itself: synthetic evidence generators (steady/diurnal/
bursty/regime-change traces with known oracles — asserting the harness recovers the
analytically-known optimum) plus recorded kind-cluster fixtures; golden scorecards
under `testdata/`.

### 4.5 Counterfactuals and cost attribution

Two deterministic query engines the LLM (and humans) lean on:

- **What-if** (`kilter whatif`): run the engine over stored history with a modified
  input — policy knobs, a pinned workload, spot enabled, a Fargate lattice — and diff
  scorecards/costs against baseline. Implementation is the backtest harness with a
  config override; zero new decision logic.
- **Cost attribution** (`kilter why-cost --from --to`): decompose ΔCost over a window
  into additive terms computed from the timeline + event log: node count change ×
  price, instance-mix change, spot/on-demand ratio change, pricing-catalog change,
  workload-set change (new/removed namespaces), and kilter's own actions (from the
  ledger, with realized deltas). Residual is reported as residual — honesty again.
  This directly answers "why did my cluster cost go up" with evidence IDs, and it is
  the tool the LLM must cite when narrating cost stories (§5).

### 4.6 Closed-loop policy tuning

The ledger already measures claimed vs realized. The loop closes conservatively:

1. Nightly, the tuner enumerates a small candidate grid around current per-class
   policy (percentile ±2pts, headroom ±5%, soak ±2h — bounded, schema-validated).
2. Each candidate is scored by the backtest harness over the trailing 30d.
3. A candidate that dominates (better regret, no safety regression, flip-rate not
   worse) becomes a **proposal object** — the same artifact an LLM or a human would
   produce — waiting in `kilter proposals` with its scorecard diff.
4. `--auto-tune=apply` (off by default) lets dominated-in-all-metrics proposals
   auto-apply within hard bounds; default is approval-gated. Either way the ledger
   records the policy change like any other action, so tuning itself is auditable and
   revertible.

No reinforcement learning, no online mutation, no cleverness in the loop — the search
is grid + gate, because the gate (backtest) is where the intelligence lives.

---

## 5. Agentic layer (`pkg/reason`)

### 5.1 Posture

The agent is a **read-only investigator with a proposal pen**, running server-side in
the brain (organ, optional), speaking through four fronts: `kilter ask/explain/
investigate` CLI, brain REST endpoints, the embedded UI, and the MCP server (§6). One
tool registry backs all four.

### 5.2 Tool surface — explicitly enumerated

Tools are Go functions over the substrate and engines; schemas are strict JSON Schema
(`additionalProperties: false`); every call and result is audit-logged. **Read-only
tools can never mutate; the two proposal tools produce inert objects.** There is no
bash, no kubectl, no arbitrary query passthrough, no log access.

| Tool | Args (validated) | Returns | Notes |
|---|---|---|---|
| `list_clusters` | — | ids + cost summaries | |
| `get_cluster_summary` | cluster | aggregates: cost, node mix, insight counts, breaker state, top-N savings/risk subjects | the entry point; bounded size |
| `search_workloads` | cluster, filters (class, namespace, minCostUSD, hasRefusal, sort, limit≤50) | subject list w/ one-line stats | deterministic pagination |
| `get_dossier` | subject | `Dossier` (§3.4) | the retrieval unit; ~4 KiB cap |
| `query_evidence` | subject, window, kinds | events | window ≤ 90d, result capped |
| `get_recommendation_explain` | subject | deterministic explain payload: every input to the number (percentiles, class policy, floors, guards, confidence basis, refusal) | the *only* sanctioned source for explanation prose |
| `get_plan` / `get_plan_step` | cluster / fingerprint | plan JSON | |
| `get_ledger` | cluster, window | entries + cost timeline + realized math | |
| `cost_attribution` | cluster, window | §4.5 decomposition | |
| `run_whatif` | cluster, bounded config override | scorecard diff | CPU-bounded, queued; override schema is the same bounded grid as tuning |
| `run_backtest` | cluster, window, policyRef | scorecard | cached by (cluster,window,policy-hash) |
| `get_pricing` | provider, filters | catalog rows | |
| `get_calendar` | cluster | business calendar entries | |
| `propose_policy_change` | target (cluster/ns/class), bounded deltas, rationale, evidence IDs | proposal ID (state: `draft`) | **gated**: schema bounds → backtest gate → human approval |
| `propose_annotation_change` | workload, one of the `kilter.dev/*` annotations, value, rationale | proposal ID | e.g. suggest `mode: recommend` for a flapping workload; applied only by a human/GitOps |

Deliberate exclusions, recorded as policy: raw metrics passthrough (unbounded tokens),
PromQL/SQL strings (injection surface), pod logs and env (secrets), cluster mutation
of any kind (breaks INV-1's spirit at the annotation layer — kilter *proposes*
annotations; the operator's GitOps applies them).

### 5.3 Reasoning loop

A plain, owned loop (stdlib-style control; the Anthropic Go SDK's tool runner is
acceptable since it exposes per-turn iteration, but the loop must be ours so that
audit, budget, and clamps are structural):

```
Investigation(question, scope):
  budget := TokenBudget(cfg)                     // hard cap, default 150k tokens
  msgs   := [system, scopedContext(question)]    // context assembled deterministically
  for i := 0; i < cfg.MaxTurns; i++ {            // default 12
      resp := provider.Chat(msgs, tools)         // streaming; effort per tier (§7)
      audit.Record(reqHash, resp)                // full transcript, hashed chain
      if budget.Exceeded() → finalize("budget")
      for call in resp.ToolCalls {
          out := registry.Run(call)              // schema-validate, clamp, time-box (2s default, whatif/backtest queued)
          audit.RecordTool(call, hash(out))
          msgs.append(toolResult(out))
      }
      if resp.Final() → break
  }
  return Finding{Answer, EvidenceRefs, Hypotheses, Proposals, Confidence}
```

`Finding` is a structured output (strict tool / `output_config.format`), not free
prose: `answer` (markdown), `evidence` (list of event/decision IDs actually retrieved
— the harness verifies each cited ID appeared in a tool result this session, so the
model cannot cite what it did not read), `hypotheses` (each labeled speculative),
`proposals` (IDs), `confidence` (the model's own, clearly separated from engine
confidence).

### 5.4 Memory & context at 50k workloads

The context strategy is **deterministic retrieval, then drill-down** — the LLM never
sees the cluster, it sees case files:

1. **Scoping.** Every investigation binds to a scope (subject | namespace | cluster
   question). The seed context is computed by ranking, not sampling: top-K subjects by
   `max(savingsUSD, riskScore, anomalyScore)` relevant to the question kind, K sized
   to a context budget (default 24 dossier stubs ≈ 12 KiB).
2. **Drill-down.** The model pulls full dossiers/evidence via tools. Tool result caps
   guarantee context can't blow up: worst case turns × cap ≈ 12 × 8 KiB = manageable.
3. **No cross-investigation memory by default.** Investigations are stateless
   case-files (reproducibility beats chattiness). Optional *operator-curated* memory:
   accepted findings can be pinned as `evidence.Kind="finding"` events on the subject
   — so next time the dossier itself carries "2026-08: bursty due to nightly
   reindex (finding #123)". Memory thus lives in the substrate, versioned and
   auditable, not in a vector store. (No embeddings, no new deps; if semantic search
   over 50k names/annotations is ever needed, it becomes another organ.)
4. **Prompt caching.** System prompt + tool schemas are stable-by-construction
   (sorted, versioned) and marked as a cache prefix with 1h TTL; per-cluster summary
   goes next; volatile question last. With the Claude API this puts the ~10-15 KiB
   fixed prefix at ~0.1× input price on every turn after the first (cache minimums:
   512 tokens on `claude-opus-5`, 1024 on `claude-sonnet-5` — our prefix clears both).

### 5.5 Determinism & reproducibility

LLM sampling is not bit-reproducible; the design does not pretend otherwise. What is
guaranteed:

- **Inputs are replayable.** Context assembly is deterministic (ranking has total
  order with documented tie-breaks); tool results are hashed into the audit trail; the
  substrate is append-only. "Show me exactly what the AI saw" is a query.
- **Outputs are inert or gated.** Nondeterminism in prose is cosmetic; nondeterminism
  in proposals is neutralized by the backtest+approval gate, which evaluates the
  proposal *content* deterministically.
- **Pinning.** Model ID, system-prompt version, tool-registry version recorded per
  investigation. A model upgrade is a visible config change, not silent drift.

### 5.6 Auditability

`bucketInvestigations` in bbolt: one record per investigation — question, scope,
initiator (token identity), model + versions, every request/response hash, every tool
call with args and result hash, token usage in/out + computed USD, finding, and
proposal IDs. Hash-chained (each record carries the previous record's hash) so
tampering is evident. Surfaced: `kilter investigations`, `GET
/api/v1/clusters/{id}/investigations`, UI panel next to the ledger. Retention
budgeted like events. The API-token identity of the caller is recorded — MCP callers
(§6) are just another identity.

### 5.7 Prompt injection & exfiltration defenses

Threat model: (1) anyone who can create a workload controls names/annotations/labels
that flow into dossiers; (2) the operator's question itself may be adversarial (MCP
exposure); (3) the model may be induced to leak whatever it can read.

Defense-in-depth, in order of load-bearing-ness:

1. **Capability firewall (structural).** All tools read-only; proposals inert and
   double-gated. The worst achievable outcome by construction is wrong prose or a
   proposal that a human sees with its (attacker-uninfluenceable) backtest scorecard.
2. **No secrets in the substrate (structural).** The collectors never ship env vars,
   log lines, ConfigMap/Secret contents, or command args. What the model can read is
   what any read-token dashboard user can read. Exfiltration therefore has nothing
   privileged to exfiltrate — and the model has no network tools anyway; its only
   egress is the answer to the caller.
3. **Data/instruction separation (prompt-level).** Evidence reaches the model only as
   JSON tool results; the system prompt declares all field values untrusted data.
   Free-text fields (names, annotation values, event notes) are length-capped (128
   chars), control-characters stripped at ingest.
4. **Output discipline.** Findings are schema-validated; markdown links to
   non-kilter URLs are stripped by the renderer (defeats "click here" exfil vectors
   in UI); evidence citations must resolve to IDs the session actually fetched.
5. **Rate/budget clamps.** Per-token-identity investigation quotas; global daily USD
   cap; both metrics exported.

### 5.8 LLM cost control — and self-accounting

A cost optimizer must account for its own inference spend. Every investigation's token
usage is priced (provider rates in config) and written to the audit record; the UI's
savings card shows **realized savings vs kilter's own AI spend** on one line. Controls:
per-investigation budget, daily org budget, tier routing (§7), prompt caching, and the
Batch API for the one bulk workload (nightly explanation refresh of top-N
recommendations — 50% batch discount and no latency requirement). Default posture ships
conservative: reasoner off unless configured, budgets low, caching on.

### 5.9 Offline / air-gapped degradation

| Capability | No model configured | Self-hosted model | Anthropic API |
|---|---|---|---|
| Sizing, plans, safety, guardrails, ledger, approvals | **full** | full | full |
| Deterministic explain payloads (`get_recommendation_explain`) | **full** (JSON + template prose, as today) | full | full |
| Backtest, what-if, cost attribution | **full** | full | full |
| Narrative explanations | template only | yes (quality = model) | yes |
| NL interrogation / investigations | unavailable (clear error) | yes | yes |
| MCP server (tools) | **full** — MCP exposes the deterministic tools regardless; only kilter-side narration needs a model | full | full |

The MCP row is the subtle one: an air-gapped enterprise with its *own* internal LLM
gateway can point that at kilter's MCP server and get the full interrogation
experience with zero model configuration inside kilter.

---

## 6. MCP server surface

Enterprises increasingly drive infra from their own AI tooling (Claude, IDE agents,
internal copilots). Kilter meets them with a first-class MCP server rather than
forcing its own chat UI.

- **Transport/impl:** Streamable HTTP endpoint on the brain (`/mcp`) plus `kilter mcp
  --stdio` for local/desktop clients (spawns against a brain URL). MCP is JSON-RPC
  2.0 with a modest lifecycle — implemented on stdlib (`net/http`, `encoding/json`),
  consistent with the no-new-deps-in-core rule; conformance is covered by golden
  transcripts of the spec's initialize/list/call flows in tests.
- **Auth:** same bearer tokens as the REST API; read token ⇒ read tools only; write
  token additionally unlocks the two proposal tools and `approve` remains
  human/CLI-only. Per-token rate limits shared with §5.7.
- **Tools:** the registry of §5.2, 1:1 — one registry, two frontends (internal loop +
  MCP). Names/schemas are the contract; versioned via `tools/list` metadata.
- **Resources:** read-only URIs for the artifacts clients like to pin as context:
  `kilter://clusters/{id}/summary`, `.../ledger`, `.../plan`,
  `kilter://subjects/{key}/dossier`.
- **Prompts:** shipped prompt templates for the two marquee flows:
  `explain-recommendation(subject)` — expands to instructions + the explain payload +
  dossier; `why-did-cost-change(cluster, window)` — instructions + cost attribution +
  timeline. These encode the citation discipline (must cite evidence IDs; must relay
  refusals) so *external* agents inherit kilter's honesty norms, not just its data.
- **Safety note:** MCP exposes exactly what the REST API already exposes to the same
  token — no new authority. Kilter-side proposal gating is unchanged regardless of how
  clever the client agent is.

---

## 7. Model strategy

Provider-neutral by contract; Anthropic-based deployment first-class by
implementation quality.

### 7.1 Pluggability — reuse the forecaster precedent

```go
// pkg/reason/provider.go — the neutral seam. Implementations are organs.
type Provider interface {
    // Chat runs one model turn with tool definitions; implementations map to
    // their native wire format. Streaming internally; returns the final turn.
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
    // Name/Model/pricing metadata for the audit trail and self-accounting.
    Info() ProviderInfo
}
```

Selection by config, mirroring `--forecaster-url`:

```
--reasoner-provider anthropic --reasoner-model claude-opus-5        # first-class
--reasoner-provider openai-compat --reasoner-url http://vllm:8000   # air-gap / any local
--reasoner-provider anthropic-compat --reasoner-url <gateway>       # enterprise LLM gateways
(unset)                                                             # reasoner off; core unaffected
```

`openai-compat` (chat-completions with tools) covers vLLM/Ollama/llama.cpp and most
enterprise gateways with one adapter, which is what makes the air-gap story real.

### 7.2 Anthropic deployment (first-class)

Implementation: the official Go SDK, `github.com/anthropics/anthropic-sdk-go`
(`anthropic.NewClient()`, `client.Messages.New` / `NewStreaming`; tools via
`ToolParam` with `Strict: anthropic.Bool(true)` for the proposal/finding schemas).
The SDK is pure Go (static binary preserved) and sits strictly outside the decision
path, so the no-new-deps-in-decision-path rule holds; the neutral `Provider` seam
keeps it swappable. Bedrock/Vertex/Foundry variants exist in the same SDK family for
enterprises with cloud-committed spend.

Tier routing (defaults; all overridable):

| Job | Model | Why | Config |
|---|---|---|---|
| Deep investigations, RCA, policy-proposal drafting | **Claude Opus 5 — `claude-opus-5`** ($5 in / $25 out per MTok, 1M context) | strongest agentic/long-horizon reasoning; investigations are low-volume, high-stakes | adaptive thinking (default-on), `effort: high` (`xhigh` for scheduled deep audits) |
| Interactive Q&A (`kilter ask`), on-demand explanations | **Claude Sonnet 5 — `claude-sonnet-5`** ($3/$15 per MTok; intro $2/$10 through 2026-08-31) | near-Opus quality on tool-driven Q&A at lower latency/cost | `effort: medium` |
| Bulk/nightly: triage summaries, explanation refresh for top-N recommendations | **Claude Haiku 4.5 — `claude-haiku-4-5`** ($1/$5 per MTok, 200K context) via the **Message Batches API** | high-volume, template-adjacent prose; batch pricing halves it and latency is irrelevant overnight | `effort` n/a (pre-4.6 thinking rules); plain calls |
| Optional ceiling for the hardest cross-cluster audits | Claude Fable 5 — `claude-fable-5` ($10/$50 per MTok) | above-Opus capability where correctness of a deep audit outweighs cost | opt-in only; requires 30-day retention orgs; handle `stop_reason: "refusal"` |

Mechanics adopted from the API (verified against current docs at design time):

- **Prompt caching:** cache breakpoints after tool schemas + system prompt (stable,
  versioned) with 1h TTL: reads ≈ 0.1× input price, writes 1.25×/2× (5m/1h TTL) —
  the economics that make a 12-turn tool loop cheap. Cache minimums (512 tokens
  Opus 5 / 1024 Sonnet 5 / 4096 Haiku 4.5) are cleared by our fixed prefix.
- **Strict/structured outputs** for `Finding` and proposal objects — schema-guaranteed
  parses, no regex repair.
- **Adaptive thinking + effort** as the depth dial per tier — no `budget_tokens`
  plumbing (removed on current models); no sampling params (likewise removed).
- **Refusal handling:** branch on `stop_reason == "refusal"` (present on 4.5+ models
  and mandatory hygiene on Opus 5/Fable 5); investigations surface it honestly as
  "model declined" rather than an empty finding.

Order-of-magnitude self-cost (defaults, 200-workload cluster): nightly Haiku batch
explanation refresh of the top 50 recommendations (~50 × 3K in/500 out tokens ≈ 0.2M
in/25K out/day) ≈ **$4–6/month**; a weekly Opus 5 deep investigation (~150K in with
~80% cached / 8K out) ≈ **$3–5/month**. Total well under $15/month against typical
three-to-four-figure monthly realized savings — and the self-accounting line (§5.8)
proves it per-org rather than asserting it.

### 7.3 What we deliberately do not do

- No fine-tuning, no embeddings store, no vector DB in core — retrieval is
  deterministic ranking + drill-down (§5.4); revisit only with evidence it's
  insufficient.
- No LLM in the collectors or actuators, ever.
- No streaming partial findings into automation — findings are complete artifacts.

---

## 8. Go interfaces (implementation sketches)

New packages — dependency direction strictly downward; `recommend`/`plan`/`actuate`
never import any of these:

```
pkg/evidence   (model)                      — L0 substrate
pkg/decision   (model, patterns, forecast)  — confidence, refusal, changepoint  [used BY recommend]
pkg/backtest   (evidence, recommend, plan)  — harness, scorecard, oracle, gate
pkg/explain    (evidence, recommend, plan)  — deterministic explain payload + cost attribution
pkg/reason     (evidence, backtest, explain)— tool registry, loop, audit, providers
pkg/mcp        (reason)                     — MCP frontend over the registry
```

```go
// pkg/decision — replaces the bare float without breaking it.
type Verdict struct {
    Recommendation *recommend.Recommendation // nil when refused
    Confidence     Confidence
    Refusal        *Refusal
}

// pkg/decision/changepoint.go — online CUSUM, O(1)/sample, checkpointable.
type Changepoint struct{ /* level EWMA, cusumPos, cusumNeg, threshold, drift */ }
func (c *Changepoint) Add(t time.Time, v float64) (fired bool, direction int)

// pkg/evidence — see §3.3/§3.4 for Store, EvidenceEvent, Dossier.

// pkg/backtest — see §4.4 for Harness, Scorecard. Plus the gate:
func Gate(current, candidate *Scorecard, tol Tolerance) (ok bool, reasons []string)

// pkg/reason — the registry both the loop and MCP mount.
type Tool struct {
    Name, Description string
    Schema            json.RawMessage // strict JSON Schema
    ReadOnly          bool
    Timeout           time.Duration
    Run               func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}
type Registry struct{ /* ordered tools, per-token limiter, clamps */ }

type Investigator struct {
    Provider Provider
    Registry *Registry
    Audit    *Audit          // bbolt-backed, hash-chained
    Budget   BudgetConfig    // tokens, turns, USD/day
}
func (iv *Investigator) Run(ctx context.Context, q Question) (*Finding, error)

// pkg/reason/proposal.go — the only bridge from L3 toward config, always gated.
type Proposal struct {
    ID        string          // content fingerprint, same idea as plan fingerprints
    Target    ProposalTarget  // cluster | namespace | class | workload-annotation
    Delta     json.RawMessage // schema-bounded parameter deltas
    Rationale string
    Evidence  []string        // event/decision IDs
    Scorecard *backtest.Scorecard // filled by the gate before human review
    State     string          // draft | gated | approved | rejected | applied | reverted
}
```

Brain wiring: `BrainConfig` gains `Evidence evidence.Config`, `Reasoner
reason.Config`; `Ingest` appends evidence and digests after the existing recommender
observe; new routes `GET /api/v1/clusters/{id}/explain/{subject}`, `POST .../ask`,
`GET/POST .../proposals`, `GET .../investigations`, `POST/GET /mcp`.

---

## 9. Implementation plan — independently shippable units, ROI-ordered

Each unit lands with unit tests + an e2e scenario before the next starts; `go test
-race ./...` green at every commit; no unit depends on a later one.

| # | Unit | Contents | ROI / effort | Test strategy |
|---|---|---|---|---|
| 1 | **Evidence substrate v1** | `pkg/evidence`: event log, deploy/HPA/OOM/pressure collectors (informer diffs), throttling field in usage samples, tiered digests + snapshot-history persistence, dossier builder; storage budgets + metrics | **Very high / M** — unblocks everything; immediate UI value (event timeline) | unit: ring/budget/prune invariants, digest coalescing, dossier size cap; fuzz digest merge; scale-soak storage assertion; e2e: deploy on kind → event visible via API |
| 2 | **Backtest harness + scorecard** | `pkg/backtest` over unit 1's history; oracle; `kilter backtest`; CI-friendly JSON | **Very high / M** — the proof-the-engine-is-good feature; also the gate everything later needs | synthetic traces with analytically-known oracles; golden scorecards; property: refusal-everything never dominates; determinism: same history ⇒ byte-identical scorecard |
| 3 | **Decision quality: refusal + changepoint + floors** | `pkg/decision`; wire into `recommend` (Verdicts), spike-robust peak, OOM-floor decay, deploy-aware fast-forward, post-change soak; refusals in insights/API/UI | **High / M** — visibly smarter + safer engine; measured by unit 2 before/after | unit per refusal predicate; CUSUM synthetic regime tests; backtest A/B in CI proving no safety regression; fuzz changepoint |
| 4 | **Deterministic explain + cost attribution** | `pkg/explain`: full explain payload, `why-cost` decomposition, `kilter explain/why-cost`; API routes | **High / S** — trust feature with zero model dependency; substrate for LLM + MCP | golden explain payloads; attribution terms sum to ΔCost ± residual invariant; e2e on kind with a forced node change |
| 5 | **What-if + closed-loop proposals** | backtest-driven `whatif`; proposal object + store + gate; nightly tuner (off by default); `kilter proposals` | **Med-high / S** (leverages 2) | gate property tests (dominance rules); tuner bounds fuzz; e2e: proposal → approve → config applied → ledger entry |
| 6 | **Reasoner core + Anthropic provider** | `pkg/reason`: registry (read-only tools first), loop, audit trail, budgets, injection defenses; providers: anthropic (SDK), openai-compat; `kilter ask`, `POST /ask`; self-cost accounting | **High / L** — the flagship differentiator, safe to ship because 1–5 bound it | registry: schema clamp + timeout tests; loop against a scripted fake Provider (deterministic transcripts); injection corpus tests (hostile names/annotations → assert no tool-arg echo, citations resolve); budget-exhaustion paths; live smoke behind env-gated tag |
| 7 | **MCP server** | `pkg/mcp` over the registry; streamable HTTP + stdio; resources + shipped prompts | **Med-high / S** (registry exists) | golden JSON-RPC conformance transcripts; auth-tier tests (read vs write token); e2e: real MCP client script drives explain-recommendation |
| 8 | **Proposal tools for the agent + batch explanations** | `propose_*` tools wired to unit 5's gate; nightly Haiku batch explanation refresh; UI investigation panel | **Med / S** | end-to-end: hostile prompt attempts to self-approve → impossible by construction (asserted); batch pipeline golden outputs |
| 9 | **Fargate domain + SubjectRef generalization** | `Domain` on NodeSpec, Fargate lattice pricing + snapping, per-pod savings | **Med / M** — opens the second domain with ~no reasoning-engine change | lattice snapping table tests; plan e2e asserting no node steps on Fargate |

Units 1–5 ship a dramatically stronger *deterministic* engine — worth the iteration
even if the LLM work stopped there, which is exactly the property that keeps the LLM
an organ and not a heart.

---

## 10. How this compares to the 2025–2026 agentic AIOps field

Survey performed against primary sources (docs, engineering blogs, repos) in August
2026. The short version: every serious system converged on the same boundary this
design draws — deterministic core, LLM shell, reads free, writes gated — and the
field's documented failures are precisely the ones §1.2 and §5 defend against.

| System | Where the LLM sits | What kilter takes from it |
|---|---|---|
| **k8sgpt** (k8sgpt-ai) | Deterministic *analyzers* detect 100% of issues; LLM only translates findings on `--explain`; results persisted as K8s CRs; optional anonymization; ships an MCP server. <https://github.com/k8sgpt-ai/k8sgpt>, <https://docs.k8sgpt.ai/reference/operator/overview/> | Confirms explain-over-deterministic-payload (§1.3a). Its anonymization gap ("does not currently apply to events") is a cautionary tale for partial redaction — kilter's answer is structural (no secrets collected at all, §5.7) rather than masking. |
| **HolmesGPT** (robusta-dev) | Agentic investigation loop over 75+ read-only, RBAC-scoped toolsets; writes are explicit opt-in toolsets; per-tool memory limits, output transformers and token budgeting for large payloads; 150+ eval scenarios. <https://github.com/robusta-dev/holmesgpt>, <https://holmesgpt.dev/latest/> | Validates the tool-registry-with-clamps shape (§5.2–5.4) and evals-as-product (our backtest + scripted-provider transcripts). Kilter goes further: *no* write toolsets at all — proposals instead. |
| **Kagent** (CNCF) | Agents/tools as CRDs; tools are MCP servers; per-tool human approval pauses execution; OTel-traced. <https://kagent.dev/docs/kagent/concepts/architecture>, <https://www.solo.io/blog/bringing-agentic-ai-to-kubernetes-contributing-kagent-to-cncf> | Confirms MCP as the standard tool surface (§6) and approval-gated tools. Kilter differs: approval gates *proposals with backtest scorecards*, not individual kubectl calls. |
| **Sedai** | Numbers come from ML/RL (baselines, seasonality, causal inference, deep RL); the LLM ("Sed") is only a chat layer; graduated autonomy Datapilot→Copilot→Autopilot; acts only above confidence thresholds. <https://sedai.io/platform>, <https://www.sedai.io/blog/automated-vs-autonomous-why-the-difference-matters-for-modern-cloud-operations> | The strongest commercial confirmation of INV-1: even a fully autonomous optimizer keeps the LLM out of sizing. Kilter's mode annotations (off/recommend/apply) are the same graduation, already shipped. |
| **Cleric AI** | Layered knowledge graph — deterministic high-confidence layers below, "fuzzy" inferred layers above; parallel hypothesis testing; compound confidence "heavily favor[ing] deterministic signals"; explicit abstention ("when we're genuinely uncertain, we simply say so"). <https://cleric.ai/blog/the-hidden-complexity-of-building-an-ai-sre> | Mirrors §4.1–4.2 (deterministic-weighted confidence, first-class refusal). Their correlation-trap postmortem (deploy blamed; real cause a 24h cron) is why kilter's cost attribution enumerates candidates deterministically before the LLM narrates (§4.5). |
| **Datadog Bits AI SRE** | Hypothesis-driven loop; key negative result: naive tool use scaled input tokens linearly and *degraded* reasoning — fixed by fetching only telemetry causally relevant to the current hypothesis; eval platform of labeled incidents + world-snapshots with injected noise. <https://www.datadoghq.com/blog/building-bits-ai-sre/>, <https://www.datadoghq.com/blog/engineering/bits-ai-eval-platform/> | Directly motivates dossier-based retrieval with hard result caps (§5.4) and noisy synthetic traces in the backtest fixtures (§4.4). |
| **Dynatrace Davis AI** | The explicit split this design generalizes: *causal AI* (deterministic fault-tree RCA) + *predictive AI* + *generative AI* (CoPilot) — because LLMs are probabilistic, "a pure generative AI approach renders use cases that require precision impossible"; data flows causal→generative, never the reverse. <https://www.dynatrace.com/news/blog/hypermodal-ai-dynatrace-expands-davis-ai-with-davis-copilot/> | Kilter's L1/L2→L3 one-way flow (§1.1) is the same architecture with an OSS, replayable core. |
| **OpenCost / Kubecost MCP** | OpenCost's official MCP server exposes exactly three read-only cost tools and deliberately no write surface. <https://opencost.io/blog/opencost-mcp-server/>, <https://opencost.io/docs/integrations/mcp/> | Precedent for a read-heavy MCP cost surface; kilter's adds explain/backtest/attribution depth those tools lack. |

Field-wide anti-patterns this design explicitly engineers against:

- **LLM-fabricated numbers** — models with data access "learned which numerical ranges
  looked plausible and began generating convincing — but fabricated — outputs"
  (<https://towardsdatascience.com/hybrid-ai-combining-deterministic-analytics-with-llm-reasoning/>);
  answered by INV-1.
- **Unbounded tool calling → context bloat → worse reasoning** (Datadog); answered by
  dossier caps, turn/token budgets (§5.3–5.4).
- **Correlation-as-causation and single-hypothesis tunnel vision** (Cleric); answered
  by deterministic candidate enumeration (§4.5) and hypotheses-labeled-as-hypotheses
  (§1.3c).
- **Guardrail-model-only injection defense is insufficient**
  (<https://blogs.cisco.com/ai/prompt-injection-is-the-new-sql-injection-and-guardrails-arent-enough>,
  <https://www.datadoghq.com/blog/llm-guardrails-best-practices/>); answered by the
  structural capability firewall + no-secrets substrate (§5.7), with prompt-level
  hygiene as the *last* layer, not the first.
- **Clean synthetic evals overstating quality** (Datadog); backtest fixtures include
  noise, unrelated churn, and adversarial traces by requirement (§4.4, §9 unit 2).

Where kilter is differentiated rather than convergent: (1) the **backtest harness as
a user-facing trust product** — none of the surveyed systems let the customer replay
*their own* history against the engine and read a regret/oracle scorecard; (2)
**refusal as a typed, scored output** rather than UX copy; (3) a fully **air-gapped
agentic story** via the provider seam + MCP-without-model (§5.9), which none of the
SaaS agents can offer.

---

## Appendix A — invariants (CI-enforceable)

- **INV-1** Numeric sovereignty (§1.2): no import path from `pkg/reason`/`pkg/mcp`
  into decision output types' producers; checked via `go list -deps` in CI.
- **INV-2** Fail-static: brain start, ingest, recommend, plan, actuate paths contain
  no call into `pkg/reason` (build succeeds with reasoner config nil; e2e runs with
  no model configured).
- **INV-3** Substrate hygiene: collectors never populate evidence attrs from env,
  args, or log text; enforced by a collectors-side allowlist test.
- **INV-4** Proposal funnel: the only state transition to `applied` goes through
  `backtest.Gate` + `Approved` (unit-tested state machine; no other writer).
- **INV-5** Every investigation record carries model ID, prompt/tool versions, token
  usage, and a resolvable citation set.
