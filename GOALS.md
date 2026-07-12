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
- [ ] GOALS.md, ARCHITECTURE.md, LICENSE (Apache-2.0), .gitignore
- [ ] CI-grade local test script (test.sh: fmt, vet, race tests, benchmarks)

## Phase 1 — Core decision math (pure Go, zero k8s deps)
- [ ] pkg/model — core domain types (ClusterSnapshot, Node, Pod, Workload, Resources, Pricing)
- [ ] pkg/histogram — exponentially-decaying histogram (VPA-style) for percentile estimation
- [ ] pkg/forecast — EWMA + Holt-Winters double/triple exponential smoothing, spike detection
- [ ] pkg/recommend — workload rightsizing engine (CPU p95/p99, memory max + OOM-aware headroom, confidence)
- [ ] pkg/pricing — instance catalogs (AWS/GCP/Azure embedded + custom JSON), spot discounts
- [ ] pkg/binpack — constraint-aware bin-packing planner (taints, affinity, PDB, topology spread, DaemonSet overhead)
- [ ] pkg/plan — rebalancing plan generator (node removals, migrations, savings estimate, risk score)

## Phase 2 — Kubernetes integration
- [ ] pkg/collect — topology + metrics collectors (client-go informers, kubelet Summary API / metrics.k8s.io)
- [ ] pkg/store — embedded persistence (bbolt) for histograms & snapshots
- [ ] pkg/api — brain REST API (stdlib http, hardened: timeouts, gzip, auth token) + agent client
- [ ] pkg/safety — PDB checks, cooldowns, disruption budgets, rollback triggers (OOM/crashloop regression)
- [ ] pkg/actuate — apply engine: patch requests/limits (in-place resize when supported), safe evict, cordon/drain
- [ ] pkg/provider — node group scaling interface (manual/generic + cloud stubs)

## Phase 3 — Binaries & deployment
- [ ] cmd/kilter — single binary: agent / brain / controller / analyze / plan / apply / simulate / version
- [ ] `kilter analyze` killer feature: zero-install instant savings report from any kubeconfig
- [ ] Helm chart (charts/kilter) + raw manifests (deploy/)
- [ ] Prometheus metrics on every component + Grafana dashboard JSON
- [ ] Dockerfile (distroless, multi-arch) + Makefile

## Phase 4 — Quality: the only way is repeated testing
- [ ] Unit tests per package (target: meaningful coverage of decision logic, not vanity %)
- [ ] pkg/sim — deterministic cluster simulator + scenario replay tests
- [ ] High-load benchmarks: binpack 1,000 nodes / 10,000 pods; recommender 10k workloads
- [ ] Fuzz tests (histogram merge, plan parser)
- [ ] e2e on local kind cluster: collect → recommend → dry-run apply → real apply
- [ ] `go vet`, `gofmt`, race detector green across repo

## Phase 5 — Polish & ship
- [ ] README.md — top-OSS grade: badges, terminal captures, architecture diagram, quickstart, comparison table
- [ ] docs/ — architecture, safety model, tuning guide
- [ ] Demo capture: `kilter analyze` output against kind cluster (asciinema-style text or SVG)
- [ ] GitHub Actions CI workflow (.github/workflows)
- [ ] Create remote repo (agenticode/kilter) and push once, at the end
- [ ] Final self-score ≥ 95/100 with rubric in this file

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
| Decision-engine correctness (tests prove it) | 25 | |
| K8s integration robustness | 20 | |
| Safety model | 15 | |
| Performance under load (benchmarks) | 15 | |
| Deployability (helm/docker/manifests) | 10 | |
| Docs/README polish | 10 | |
| CI & repo hygiene | 5 | |
