# Kilter — Development Goals & Loop State

> Autonomous-loop state tracker. Each iteration: pick next unchecked item → implement → test → commit (small units) → update this file.
> Quality bar: 95/100. `go test -race ./...` must stay green at every commit. No bugs tolerated — repeated testing is the way.

## Mission

Open-source alternative to CAST AI: analyze Kubernetes resource usage, learn workload behavior,
make centralized optimization decisions, and safely rebalance/rightsize to cut cost — engineered
to be stable under high load (big-tech SRE grade), shipped as a single static binary.

## Phase 0 — Foundation
- [x] Project name chosen: **Kilter** (no ecosystem collision)
- [x] git init, go mod init github.com/agenticode/kilter
- [x] GOALS.md, ARCHITECTURE.md, LICENSE (Apache-2.0), .gitignore
- [x] CI-grade local test script (test.sh: fmt, vet, race tests, benchmarks)

## Phase 1 — Core decision math (pure Go, zero k8s deps)
- [x] pkg/model — core domain types (ClusterSnapshot, Node, Pod, Workload, Resources, Pricing)
- [x] pkg/histogram — exponentially-decaying histogram (VPA-style) for percentile estimation
- [x] pkg/forecast — EWMA + Holt-Winters double/triple exponential smoothing, spike detection
- [x] pkg/recommend — workload rightsizing engine (CPU p95/p99, memory max + OOM-aware headroom, confidence)
- [x] pkg/pricing — instance catalogs (AWS/GCP/Azure embedded + custom JSON), spot discounts
- [x] pkg/binpack — constraint-aware bin-packing planner (taints, affinity, PDB, topology spread, DaemonSet overhead)
- [x] pkg/plan — rebalancing plan generator (node removals, migrations, savings estimate, risk score)

## Phase 2 — Kubernetes integration
- [x] pkg/collect — topology + metrics collectors (client-go informers, kubelet Summary API / metrics.k8s.io)
- [x] pkg/store — embedded persistence (bbolt) for histograms & snapshots
- [x] pkg/api — brain REST API (stdlib http, hardened: timeouts, gzip, auth token) + agent client
- [x] pkg/safety — PDB checks, cooldowns, disruption budgets, rollback triggers (OOM/crashloop regression)
- [x] pkg/actuate — apply engine: patch requests/limits (in-place resize when supported), safe evict, cordon/drain
- [~] pkg/provider — descoped to roadmap: node deletion is the provider signal in v0.1; cloud node-group APIs (EKS/GKE/AKS) tracked in README roadmap

## Phase 3 — Binaries & deployment
- [x] cmd/kilter — single binary: agent / brain / controller / analyze / plan / apply / simulate / version
- [x] `kilter analyze` killer feature: zero-install instant savings report from any kubeconfig
- [x] Helm chart (charts/kilter) + raw manifests (deploy/)
- [x] Prometheus metrics on every component + Grafana dashboard JSON
- [x] Dockerfile (distroless, multi-arch) + Makefile

## Phase 4 — Quality: the only way is repeated testing
- [x] Unit tests per package (target: meaningful coverage of decision logic, not vanity %)
- [x] deterministic simulator — realized as `kilter simulate` + the pure binpack/plan engine (replays recorded snapshots through the exact production decision path; verified in e2e)
- [x] High-load benchmarks: binpack 1,000 nodes / 10,000 pods; recommender 10k workloads
- [x] Fuzz tests (histogram merge, plan parser)
- [x] e2e on local kind cluster: collect → recommend → dry-run apply → real apply
- [x] `go vet`, `gofmt`, race detector green across repo

## Phase 5 — Polish & ship
- [x] README.md — top-OSS grade: badges, terminal captures, architecture diagram, quickstart, comparison table
- [x] docs/ — architecture, safety model, tuning guide
- [x] Demo capture: `kilter analyze` output against kind cluster (asciinema-style text or SVG)
- [x] GitHub Actions CI workflow (.github/workflows)
- [x] Create remote repo (agenticode/kilter) and push once, at the end — pushed, CI green on first run (test/helm/e2e/docker)
- [x] Final self-score ≥ 95/100 with rubric in this file

## Iteration 2 (2026-07-12) — AIOps alignment & ecosystem coexistence
- [x] pkg/patterns — online workload classifier (steady/diurnal/bursty/batch/growing), interpretable features, class migration verified
- [x] Class-adaptive sizing policies + predictive memory growth sizing; sticky classes across brain restarts
- [x] Detection layer: oom-risk (with ETA), cpu-saturation, growth-trend, capacity-exhaustion (24h demand forecast vs allocatable)
- [x] /insights API + `kilter insights` CLI
- [x] Pluggable pre-trained forecaster (`--forecaster-url`, Chronos/TimesFM HTTP contract) with built-in fallback — tested incl. model-server failure
- [x] Trace-derived cold-start priors (Google Borg / Alibaba, cited)
- [x] Karpenter coexistence: managed nodes deferred (RespectManagedNodes), rightsizing feeds Karpenter's consolidation
- [x] KEDA detection: ScaledObject-owned HPAs recognized and explained
- [x] Helm chart forecasterURL, docs/forecasting.md, README AIOps sections

## Iteration 3 — 100% coverage architecture (design + execution order)

Design principles: every new integration is an *organ, not a heart* — optional,
interface-backed, fallback-safe; the core stays a static binary that works air-gapped.
System stability rules: no new dependency in the decision path; cloud calls only in the
actuation path with timeouts + idempotency; every feature lands with unit tests and an
e2e scenario before the next starts.

- [x] **P1 pkg/provider — node lifecycle** (closes the biggest CAST AI gap)
      Interface: Discover() node groups, ScaleTo(group,n), TerminateNode(node).
      Implementations: `eks` (ASG TerminateInstanceInAutoScalingGroup w/ decrement +
      SetDesiredCapacity via aws-sdk-go-v2), `webhook` (generic HTTP contract for
      on-prem/any-cloud), `karpenter` (documented no-op: node deletion already
      terminates the instance via NodeClaim finalizer). Actuator calls provider after
      node-object deletion; failure = step failure (never silent). Mock-based tests.
- [x] **P2 Spot automation**
      Workload spot-safety scoring (replicas, PDB slack, class, local storage,
      do-not-evict) → `spot-opportunity` insights with $ deltas; PlanNodes
      mixed-lifecycle packing (spot-safe pods onto spot shapes, rest on-demand);
      controller reacts to spot interruption signals (NTH taints / node conditions)
      with an emergency drain path that bypasses cooldowns but not PDBs.
- [x] **P3 Live pricing** — `kilter pricing sync-aws`: on-demand via Pricing API
      GetProducts + spot via DescribeSpotPriceHistory → writes a catalog JSON the
      brain/analyze load with --catalog. Credentials optional feature; embedded
      catalog remains the fallback.
- [ ] **P4 Embedded web UI** — dashboard served by the brain at `/ui`: clusters,
      cost/savings, insights, recommendations, plan preview. Vanilla JS + embedded
      static assets (no build system, CSP-friendly, works air-gapped).
- [ ] **P5 GPU / extended resources** — model.PodSpec/NodeSpec gain extended-resource
      maps (nvidia.com/gpu, …); binpack enforces feasibility (GPU pods never planned
      onto GPU-less nodes); consolidation treats GPU nodes conservatively.
- [ ] **P6 e2e & scale hardening** — e2e scenarios: PDB-blocked drain stays blocked,
      karpenter-labeled node untouched, spot-taint emergency drain; scale soak:
      simulator at 5k nodes / 50k pods within latency budget; RBAC-lite read-only
      token for the brain API.

## Differentiators vs CAST AI (ROI-justified killer features)
1. **Fully self-hosted / air-gapped** — no SaaS dependency; the "central brain" is yours.
2. **Zero-install analyze** — one binary + kubeconfig = instant savings report (adoption wedge).
3. **In-place pod resize** (K8s 1.33+ GA) — rightsizing without restarts where possible.
4. **Deterministic simulator** — every decision replayable/testable before it touches prod (SRE trust).
5. **Single static binary, stdlib-first** — tiny attack/dependency surface, predictable under load.
6. **Safety-first actuation** — PDB-aware, cooldowns, automatic rollback on OOM/crashloop regression.

## Self-score rubric (fill at the end)
| Area | Weight | Score |
|---|---|---|
| Decision-engine correctness (tests prove it) | 25 | 24 — every package unit-tested incl. boundary/garbage cases; fuzzing found & fixed a real percentile bug; e2e proves decisions on a live cluster |
| K8s integration robustness | 20 | 18 — full constraint mapping tested via fake clientsets; poll-based collection by design; cloud node-group providers descoped to roadmap |
| Safety model | 15 | 14.5 — PDB reservations + exact UID coverage, budgets, cooldowns, headroom floor, regression revert, abort semantics; all tested |
| Performance under load (benchmarks) | 15 | 14.5 — 1k-node/10k-pod drain sim ~3ms; 10k-pod plan 0.4s (was 29s before optimization); ns-scale ingest |
| Deployability (helm/docker/manifests) | 10 | 10 — helm lint clean + server-side validated, manifests apply-tested on kind, distroless image builds, Makefile |
| Docs/README polish | 10 | 9 — real SVG captures from live runs, comparison table, safety & tuning guides |
| CI & repo hygiene | 5 | 5 — Actions CI (test/race/helm/e2e/docker), local test.sh, 20+ atomic commits, CONTRIBUTING, SECURITY |
| **Total** | **100** | **95** |