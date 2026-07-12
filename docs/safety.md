# Kilter safety model

Kilter mutates production clusters, so its safety rules are structural — they
live in the type system and the plan format, not in operator discipline.

## The envelope

Every mutation passes through all of these gates, in order:

1. **Mode gate.** The actuator is constructed in `dry-run` or `apply` mode.
   Dry-run performs zero API writes (verified by test: the fake client records
   no write actions).
2. **Plan-time proof.** A node is only planned for removal when the scheduling
   simulator (`pkg/binpack`) proves every displaced pod fits elsewhere under
   full constraint semantics: resources, nodeSelector, required affinity,
   taints/tolerations, workload anti-affinity, topology spread, pod limits.
3. **PDB reservations.** Plan building *reserves* disruptions against each
   PodDisruptionBudget (`pkg/safety.PDBGuard`), so a single plan can never
   schedule more evictions than budgets allow — even across multiple nodes.
   PDB coverage uses exact pod-UID sets computed with full Kubernetes selector
   semantics (matchExpressions included).
4. **Headroom floor.** After a simulated removal the surviving cluster must
   keep `MinClusterHeadroom` (default 10%) free in *both* CPU and memory.
   Kilter refuses to pack a cluster to the brim.
5. **Execution-time enforcement.** Evictions go through the eviction API, so
   the apiserver re-checks PDBs with live state. `429 TooManyRequests` is
   retried with backoff, then the plan aborts.
6. **Budgets and cooldowns.** A sliding eviction budget (default 20/hour)
   bounds cluster-wide churn; per-workload and per-node cooldowns (1h) prevent
   flapping; removals per plan are capped (default 3).
7. **Abort semantics.** Failure in a node-surgery step aborts the remainder of
   the plan. A node is deleted only after polling confirms nothing but
   DaemonSet/mirror/completed pods remain.

## Pods Kilter refuses to move

| Condition | Effect |
|---|---|
| `kilter.dev/do-not-evict: "true"` | pins pod and its node |
| `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"` | same (drop-in compat) |
| Bare pod (no controller) | pins its node |
| Local storage (emptyDir/hostPath) | pins its node |
| DaemonSet pod | never evicted (dies with its node, by design) |
| Control-plane node | never a consolidation candidate |
| Jobs / CronJobs / bare pods | never rightsized |

## Rightsizing guards

- **Memory never shrinks below evidence.** Target = max(p99 × headroom,
  observed peak), and any OOMKill raises a floor of 1.5× the level that OOMed.
  Floors also apply to limits.
- **HPA interplay.** Workloads whose HPA scales on CPU utilization keep their
  CPU request untouched (changing it silently reshapes HPA math); memory is
  still optimized.
- **Churn suppression.** Changes under 10% are not proposed; recommendations
  carry confidence scores derived from sample depth, window length and
  volatility, and the planner ignores low-confidence ones.

## Regression revert

After the controller applies a resize it records the workload's restart/OOM
baseline. For the next 30 minutes, every reconcile re-checks:

- any new OOMKill, or restarts rising by more than 2 → **automatic revert to
  the previous requests/limits** and a 24-hour quarantine during which no new
  recommendation touches that workload.

## What apply mode can never do

- Delete a node that still runs non-DaemonSet pods.
- Evict beyond what PDBs or the sliding budget allow.
- Touch control-plane nodes.
- Resize a workload kind it doesn't understand (only Deployments and
  StatefulSets are mutated).
