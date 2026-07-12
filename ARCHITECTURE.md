# Kilter Architecture

Kilter keeps Kubernetes clusters "in kilter": continuously observed, learned, and rebalanced
onto the cheapest safe shape. It is a self-hosted alternative to commercial optimizers
(CAST AI, ScaleOps, PerfectScale): same control loop, no SaaS.

```
                        ┌────────────────────────────────────────────┐
                        │                 BRAIN (central)            │
   snapshots            │  ┌──────────┐  ┌───────────┐  ┌─────────┐  │   plans
  ┌──────────┐  push    │  │  store   │→ │ recommend │→ │  plan   │  │  ┌────────────┐
  │  AGENT   │ ───────► │  │ (bbolt)  │  │ forecast  │  │ binpack │  │─►│ CONTROLLER │
  │ (per     │  REST    │  └──────────┘  └───────────┘  └─────────┘  │  │ (actuator) │
  │ cluster) │          │        savings API · metrics · web        │  └─────┬──────┘
  └────┬─────┘          └────────────────────────────────────────────┘        │ evict /
       │ watch+scrape                                                         │ patch /
       ▼                                                                      ▼ resize
  ┌─────────────────────────────  Kubernetes cluster  ─────────────────────────────┐
```

One static binary, four roles: `kilter agent|brain|controller|analyze`.

## Data flow

1. **Agent** (per cluster) uses client-go informers to watch topology (nodes, pods, workloads,
   PDBs, HPAs, priority classes) and scrapes usage from `metrics.k8s.io` (fallback: kubelet
   Summary API). Every interval it ships a compact `ClusterSnapshot` to the brain.
2. **Brain** ingests snapshots into decaying histograms per container (CPU, memory), persisted
   in bbolt. The recommender derives requests (CPU p95, memory max+headroom with OOM bumps);
   the forecaster (Holt-Winters) projects cluster demand; the bin-packer computes the cheapest
   node set satisfying every scheduling constraint; the planner emits a `RebalancePlan` with
   per-step risk and $ savings.
3. **Controller** executes plans under a safety envelope: PDB-aware eviction, cooldowns,
   in-place resize when the cluster supports it, cordon/drain orchestration, and automatic
   rollback when a workload regresses (OOMKill / CrashLoop after a change).

`kilter analyze` embeds agent+brain in one process for a zero-install, read-only savings
report from any kubeconfig.

## Package map

| Package | Responsibility | Deps |
|---|---|---|
| `pkg/model` | Domain types shared by all components | none |
| `pkg/histogram` | Exponentially-decaying histograms, checkpointable | none |
| `pkg/forecast` | EWMA, Holt-Winters, spike detection | none |
| `pkg/recommend` | Container/workload rightsizing + confidence | model, histogram |
| `pkg/pricing` | Instance catalogs (embedded AWS/GCP/Azure + custom), spot | model |
| `pkg/binpack` | Constraint-aware bin-packing (taints, affinity, PDB, spread) | model, pricing |
| `pkg/plan` | Rebalance plan generation, savings & risk scoring | binpack, recommend |
| `pkg/collect` | Cluster topology + metrics collection | client-go |
| `pkg/store` | bbolt persistence for histograms/snapshots | bbolt |
| `pkg/api` | Brain REST API + client (stdlib http) | model |
| `pkg/safety` | PDB math, cooldowns, disruption budget, regression detect | model |
| `pkg/actuate` | Patch/resize/evict/drain execution | client-go, safety |
| `pkg/provider` | Node-group scale interface (cloud stubs + manual) | model |
| `pkg/sim` | Deterministic cluster simulator for decision testing | model, binpack |

Dependency rule: Phase-1 packages (model→plan) are pure Go — no Kubernetes imports —
so the whole decision engine is unit-testable and fuzzable in milliseconds.

## Safety model

- **Dry-run by default.** Mutations require `--mode=apply` plus per-action guards.
- **Disruption budget**: max concurrent evictions, per-workload cooldown, PDB always honored.
- **Two-phase rebalance**: provision-before-drain; never removes capacity before replacements are Ready.
- **Regression rollback**: controller watches changed workloads; OOMKill or CrashLoopBackOff
  within the observation window reverts the change and quarantines the recommendation.
- **Idempotent, resumable plans**: every step records state; controller restart resumes safely.

## High-load engineering rules

- stdlib-first; heavy deps only where unavoidable (client-go, bbolt, prometheus client).
- All servers: explicit ReadHeaderTimeout/IdleTimeout, bounded request bodies, graceful shutdown.
- All hot paths allocation-benchmarked; binpack target: 1k nodes / 10k pods well under a second.
- Everything race-tested; informer callbacks never block; channels bounded.
