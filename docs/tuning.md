# Tuning Kilter

The defaults are deliberately conservative: they favor "missed savings" over
"paged SRE". This page lists the dials that matter, in the order most teams
turn them.

## 1. Pricing accuracy

Savings math is only as good as node prices. Resolution order per node:

1. `kilter.dev/hourly-cost` node annotation (exact, highest priority)
2. Catalog lookup by `node.kubernetes.io/instance-type` + provider + spot
3. Fallback unit economics ($0.033/vCPU-h + $0.0044/GiB-h)

For on-prem or negotiated pricing, ship a custom catalog:

```json
{"instances": [
  {"provider": "onprem", "name": "rack-std", "milliCPU": 32000,
   "memoryBytes": 137438953472, "hourlyUSD": 0.35}
]}
```

and pass `--catalog my-prices.json` to the brain (or `kilter analyze`).
Spot nodes are auto-detected from karpenter/EKS/GKE/AKS labels.

## 2. Rightsizing aggressiveness

Brain-side (`recommend.Config`, surfaced as flags in a future release —
defaults shown):

| Dial | Default | Notes |
|---|---|---|
| CPU percentile / headroom | p95 × 1.15 | raise headroom for latency-critical services |
| Memory percentile / headroom | p99 × 1.20, floored at observed peak | memory is unforgiving; don't lower |
| OOM bump | 1.5× | floor applied after any OOMKill |
| Min window / samples | 6h / 30 | how much history before recommending |
| Min change | 10% | churn suppression |

`kilter analyze --watch 30m` uses relaxed thresholds (window/2, 5 samples) —
good for a first look, not for automated apply.

## 3. Consolidation appetite

| Dial | Default | Effect of raising |
|---|---|---|
| `MinNodeUtilization` | 0.5 | more nodes become candidates |
| `MaxNodeRemovals` | 3/plan | bigger bites per reconcile |
| `MinClusterHeadroom` | 0.10 | *lowering* packs tighter, riskier |
| `--max-evictions-per-hour` | 20 | overall churn ceiling |

Spiky clusters (batch jobs, cron storms): keep headroom ≥ 0.15 and let the
spike detector do its job — volatile workloads already get lower confidence.

## 4. Multi-cluster

Run one brain; point each cluster's agent/controller at it with a unique
`--cluster-id`. Learning, plans and metrics are fully isolated per cluster
(recommender state is keyed per cluster). The brain is a single process with
an embedded DB — a small VM handles hundreds of clusters at 60s intervals;
ingest is ~10ms per snapshot at the 23-pod demo scale and O(pods) beyond.

## 5. Kubernetes version notes

- **≥1.33**: keep `--in-place-resize=true` (default). Resizes land without pod
  restarts when the kubelet supports it; otherwise the rollout from the
  template patch converges anyway.
- **<1.27**: disable in-place resize; everything else works.
- `metrics.k8s.io` (metrics-server) is required for usage learning; without
  it Kilter still prices nodes and consolidates on *requested* capacity.
