# Compute Domains: Rightsizing Beyond the Node-Backed Cluster

**Status:** design proposal (no code in this change)
**Scope:** extend Kilter's observe → learn → decide → act loop from "EC2-backed
Kubernetes nodes" to distinct *compute domains*: EKS-on-Fargate, EKS-on-EC2
(existing), plain EC2 (non-Kubernetes), and an assessment of ECS-on-Fargate,
Lambda, RDS/Aurora, and Batch.
**Verification:** every price, quota, and API behavior marked **[verified]** was
fetched from the cited URL on 2026-08-25. Items marked **[not re-verified]** are
documented AWS behavior we did not re-fetch this session; items marked
**[unverified]** must be confirmed during implementation. Prices are us-east-1,
Linux, USD, and drift over time — implementation reads them from synced
catalogs, never from constants in this doc.

---

## 1. Design invariants

These restate `GOALS.md` Iteration-3 rules as hard requirements for every new
domain. A domain that cannot satisfy them does not ship.

1. **Organ, not heart.** Every domain is optional, interface-backed, and
   fallback-safe. `kilter` with zero domains configured behaves exactly as
   today. The core remains a single static binary that works air-gapped.
2. **No new dependency in the decision path.** Learning, recommending, pricing,
   and planning are pure Go over data already collected. Cloud SDK calls exist
   only in (a) collectors that *feed* snapshots and (b) actuators that *execute*
   approved steps — both with bounded contexts, timeouts, and idempotency.
3. **Embedded economics.** Fargate tier tables, EBS rates, and burstable
   baselines ship embedded (like `pkg/pricing/catalog.json`), overridable by a
   synced or hand-written file. Relative math works offline; exact billing
   belongs to the invoice.
4. **Dry-run by default, ledger always.** Every domain reuses the trust
   package: `kilter.dev/mode` semantics, change windows, freeze, circuit
   breaker, plan fingerprints + approval, audit ledger with claimed-vs-measured
   savings, and undo where reversible.
5. **Never silent, never destructive.** A failed actuation step fails the plan
   loudly. No domain ever deletes data-bearing resources, purchases or modifies
   commitments, or performs an irreversible action without an explicit human
   gate.

## 2. The shape of the problem

Kilter's loop is domain-independent; only two things vary per domain:

- **The target**: what unit is observed and resized (a container, a Fargate
  pod, an EC2 instance, an EBS volume, an ECS service, a Lambda function).
- **The price function**: how a spec maps to dollars (node price via catalog;
  Fargate quantized vCPU/GB rates; instance + commitment waterfall; GB-month +
  IOPS; GB-second × invocations).

Everything else — decaying histograms (`pkg/histogram`), forecasting
(`pkg/forecast`), behavior classes (`pkg/patterns`), confidence scoring,
guardrails, approvals, the ledger — is reusable as-is because those packages
are already key-agnostic pure Go. The design therefore introduces a thin
`pkg/domain` seam (§5) rather than a rewrite.

A second structural fact drives the architecture: **bin-packing exists only in
the node domain.** Fargate, plain EC2, ECS, and Lambda have no shared node to
pack; each target is priced independently. So `pkg/binpack` and the
consolidation half of `pkg/plan` are *bypassed* for node-less domains, while
the rightsizing half (`pkg/recommend`) is *reused* wherever the target is a
container (both Kubernetes domains) and *re-implemented per target type*
elsewhere (instances, volumes, functions) behind a common `Rightsizer`
contract.

---

## 3. Per-domain agent roles (Part A)

### 3.1 Domain `k8s-nodes` — EKS on EC2 / managed node groups / self-managed ASG / Karpenter (existing)

This is today's Kilter; listed for contrast and for the deltas this design
adds.

- **Observes:** cluster topology via client-go informers; usage via
  `metrics.k8s.io` (fallback kubelet Summary API) at ~1-min resolution. Node
  prices via embedded catalog / `kilter pricing sync-aws` (AWS Pricing API
  `GetProducts`, `ec2:DescribeSpotPriceHistory`).
- **Decides:** per-container requests (CPU p95 × headroom, memory peak +
  OOM-aware floors), cheapest safe node set (binpack), spot opportunity,
  node removals.
- **Actuates:** patch/resize/evict/cordon/drain via the Kubernetes API;
  instance termination via `pkg/provider` (`autoscaling:SetDesiredCapacity`,
  `autoscaling:TerminateInstanceInAutoScalingGroup`).
- **Never:** touches Karpenter-managed nodes (deferred by default), violates
  PDBs, exceeds eviction budgets, acts under freeze/circuit-breaker.
- **HITL gate:** `kilter.dev/mode`, change windows, `--require-approval` +
  plan fingerprints, undo.
- **Deltas from this design:**
  1. Fargate nodes (label `eks.amazonaws.com/compute-type=fargate`) must be
     **excluded from consolidation and node pricing** and routed to the
     `k8s-fargate` domain (§3.2). Today they are mispriced (their reported
     node capacity is often larger than the pod's billed capacity —
     [verified], §4.1) and could be fed into binpack as removal candidates.
  2. Node-plan savings become **commitment-aware** (§4.4): claimed savings are
     netted against RI/SP stranding when commitment inventory is available.

### 3.2 Domain `k8s-fargate` — EKS on Fargate

Fargate runs each pod in its own single-pod VM ("node"); there is nothing to
bin-pack or drain, and billing is per-pod by a *quantized* vCPU/memory
configuration.

**Observes.**
- Topology + usage through the **existing Kilter agent, unchanged**: every
  Fargate pod's VM runs a kubelet and registers as a node, so
  `metrics.k8s.io` serves Fargate pod usage exactly like EC2-backed pods.
  No node access exists (no SSH, no host OS, no DaemonSets, no IMDS) —
  [verified: EKS Fargate considerations,
  https://docs.aws.amazon.com/eks/latest/userguide/fargate.html] — but Kilter
  never needed node access: its collectors are API-level. **Blind spots:**
  no node-exporter-style host metrics (irrelevant — the pod is the host); no
  DaemonSet collectors can run there (Kilter doesn't use any).
- **Billed capacity** from the pod annotation `CapacityProvisioned`
  (e.g. `0.25vCPU 0.5GB`), which "determines the cost of your Pod running on
  Fargate" — [verified:
  https://docs.aws.amazon.com/eks/latest/userguide/fargate-pod-configuration.html].
  The agent copies this annotation into the snapshot; it is the ground truth
  the quantizer (§4.1) is validated against in production.
- Fargate profile membership (which namespaces/labels land on Fargate) is
  visible cluster-side; optionally enriched via `eks:ListFargateProfiles` +
  `eks:DescribeFargateProfile` for what-if placement recommendations.
- Resolution: same 1-min usage cadence as today. Memory metrics exist (kubelet
  reports them); no CloudWatch agent involved.

**Decides.**
1. Per-container requests exactly as today (`pkg/recommend`, unchanged — same
   histograms, classes, OOM floors).
2. **Tier placement:** map recommended pod requests through the Fargate
   quantizer (§4.1) and pick the cheapest valid configuration. The
   recommendation unit is the *pod tier delta*, priced with real Fargate
   rates, including the 256 MB overhead and rounding cliffs.
3. **Boundary shaving:** when a pod's requests sit just above a tier boundary
   (including the +256 MB overhead), recommend the sub-boundary request that
   halves the bill (worked example §4.1.3) — only when the learned usage
   supports it.
4. **Crossover advice:** compute Fargate-vs-EC2 economics both directions
   (§4.3) and emit insights: "these Fargate pods would cost X on EC2 node
   groups" / "these EC2-hosted workloads would be cheaper on Fargate".
   Placement *migration* between domains is advisory-only in this design.
5. Job hygiene: completed/failed Job pods left running on Fargate keep
   billing — [verified: fargate.html] — emit an insight recommending
   `ttlSecondsAfterFinished`.

**Actuates.**
- Exactly one mechanism: **patch workload requests/limits via the Kubernetes
  API** — the same actuator path as today. On Fargate the resize is *never
  in-place*: changing requests recreates the pod on a new Fargate VM (AWS
  documents VPA must use `Auto`/`Recreate` there — [verified: fargate.html]).
  The controller therefore treats every Fargate resize as a
  restart-class change: rolling, PDB-aware, regression-watched, undoable.
- **No AWS API calls at all in this domain's actuation path.** This is the
  cleanest possible organ: zero new IAM for actuation.
- Fargate pods run Guaranteed QoS; requests must equal limits — [verified:
  fargate-pod-configuration.html: "the requested CPU and memory must be equal
  to the limit for all of the containers"]. The actuator sets both.

**Never.**
- Never cordon/drain/consolidate Fargate "nodes" (each is one pod; eviction
  is just killing the pod).
- Never recommend Fargate Spot on EKS: "Amazon EKS doesn't support Fargate
  Spot" — [verified: fargate.html].
- Never recommend ARM/Graviton Fargate on EKS: "Can run workloads that
  require Arm processors: No" — [verified: fargate.html comparison table;
  open request aws/containers-roadmap#1629].
- Never assume GPU, EBS, privileged, hostPort/hostNetwork, or DaemonSet
  features on Fargate pods ([verified: fargate.html]); never move a pod that
  needs any of these *to* Fargate in crossover advice.

**HITL gate.** Identical to today: `kilter.dev/mode` per workload/namespace,
change windows, approval fingerprints, ledger + undo (a resize is reverted by
restoring the From-values). Domain-to-domain migration advice never
auto-applies.

**Least-privilege IAM (all optional; observation is Kubernetes-only).**

```json
{
  "Sid": "KilterFargateOptionalEnrichment",
  "Effect": "Allow",
  "Action": [
    "eks:DescribeCluster",
    "eks:ListFargateProfiles",
    "eks:DescribeFargateProfile",
    "pricing:GetProducts"
  ],
  "Resource": "*"
}
```

(`pricing:GetProducts` only for live Fargate rate sync; the embedded rate
table is the fallback. The Price List `serviceCode` under which Fargate SKUs
live must be confirmed at implementation time — **[unverified]**, candidates
`AmazonECS`/`AmazonEKS`.)

### 3.3 Domain `ec2` — plain EC2 (non-Kubernetes): instances, ASGs, fleets

A new collector role (`kilter cloud-agent --domain ec2 --region …`) ships
`DomainSnapshot`s to the brain on the same push contract the K8s agent uses.

**Observes.**
- **Inventory:** `ec2:DescribeInstances` (type, platform, tenancy, AZ, tags,
  instance-store presence, EBS mappings), `ec2:DescribeInstanceTypes`
  (vCPU/memory/arch/network/EBS baseline of candidates),
  `ec2:DescribeVolumes` (type, size, provisioned IOPS/throughput),
  `autoscaling:DescribeAutoScalingGroups` (+ launch templates via
  `ec2:DescribeLaunchTemplateVersions`).
- **Metrics:** `cloudwatch:GetMetricData` (batched, up to 500 series/call).
  Default (free) EC2 metrics are **5-minute** resolution; **1-minute requires
  paid detailed monitoring**; and there is **no memory metric at all without
  the CloudWatch agent** — [verified:
  https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/viewing_metrics_with_cloudwatch.html
  + search corroboration]. Series per instance: `CPUUtilization`,
  `NetworkIn/Out`, per-volume `VolumeReadOps/VolumeWriteOps/
  VolumeReadBytes/VolumeWriteBytes`, and for T-family `CPUCreditUsage`,
  `CPUCreditBalance`, `CPUSurplusCreditBalance`,
  `CPUSurplusCreditsCharged`.
  If the customer runs the CW agent, memory series (`CWAgent` namespace,
  `mem_used_percent`) are ingested when present.
- **Blind spots (must shape decisions):**
  - **Memory-blind mode**: without the CW agent there is no memory signal.
    Rule: *never recommend a spec whose memory is below the current
    instance's memory* in memory-blind mode; only same-or-more-memory moves
    (e.g. m5.xlarge → m7g.xlarge, or gp2 → gp3) are eligible, confidence is
    capped, and the report says why. (AWS Compute Optimizer has the same
    constraint.)
  - 5-min CPU averages hide sub-minute bursts: recommendations use p95/p99 of
    the 5-min series *plus* T-family credit-burn corroboration, and headroom
    defaults are higher than in the K8s domain.
  - Disk *space* is invisible without the agent (EBS metrics are I/O, not
    fill). Volume *shrink* is impossible on EBS anyway; Kilter never
    recommends it.
- **Commitments:** `ec2:DescribeReservedInstances`,
  `savingsplans:DescribeSavingsPlans`, and (optional, $0.01/request —
  [not re-verified: aws.amazon.com/aws-cost-management/aws-cost-explorer/pricing/])
  `ce:GetSavingsPlansUtilization`, `ce:GetReservationUtilization` for
  pool-utilization corroboration. SP rates via
  `savingsplans:DescribeSavingsPlansOfferingRates` — **[unverified: exact
  action name to confirm]**.
- **Cross-check (optional):** `compute-optimizer:GetEC2InstanceRecommendations`,
  `GetEBSVolumeRecommendations`, `GetAutoScalingGroupRecommendations` —
  [verified action names:
  https://docs.aws.amazon.com/compute-optimizer/latest/APIReference/API_Operations.html].
  Kilter never *depends* on Compute Optimizer (14-day lookback, opt-in), but
  when present, disagreement lowers confidence and is reported as evidence.

**Decides.**
- **Size/family migration** within the same architecture: target the cheapest
  instance type whose vCPU ≥ CPU p95 × headroom and memory ≥ current (or ≥
  learned peak when memory data exists), honoring network/EBS baseline
  floors from `DescribeInstanceTypes`.
- **Graviton migration** (advisory only): flag x86 instances whose family has
  an ARM twin ~15–20 % cheaper (§4.5). OS/binary compatibility cannot be
  verified from metrics, so this never auto-applies.
- **Burstable analysis** (§4.6): classify T-family instances by 24 h average
  CPU vs baseline; compute realized surplus charges from
  `CPUSurplusCreditsCharged`; recommend T→M when surplus erases the discount,
  M→T when sustained CPU sits far below a T baseline, and flag
  standard-mode throttling risk (credit balance trending to zero).
- **EBS rightsizing** (§4.7): gp2→gp3 with measured-IOPS/throughput parity;
  over-provisioned io1/io2/gp3 IOPS reduction.
- **Commitment interplay** (§4.4): every recommendation's savings are
  computed **net of the commitment waterfall**; recs whose net savings ≤ 0
  are suppressed with the reason attached ("covered by m5 RI until
  2027-03-01; rightsizing would strand 8 normalized units").
- ASG-level: same math against the launch template's instance type; the
  target is the template, not individual instances.

**Actuates** (each step idempotent, timeout-bounded, ledger-recorded).
- **EBS modification (lowest risk, online):** `ec2:ModifyVolume`
  (type/size/IOPS/throughput) + `ec2:DescribeVolumesModifications` to await
  `optimizing`/`completed`. Constraint: one modification per volume per
  ~6 hours — [not re-verified: EBS ModifyVolume docs] — enforced as a
  cooldown.
- **Stopped-resize for standalone instances:** `ec2:StopInstances` →
  `ec2:ModifyInstanceAttribute` (instanceType) → `ec2:StartInstances`.
  This is downtime; it only ever runs inside a change window *and* behind
  explicit approval (below). Pre-flight refuses instances with
  instance-store volumes (data loss on stop), unknown shutdown behavior, or
  missing newer-generation prerequisites (ENA/NVMe) — pre-flight reads
  `DescribeImages`/`DescribeInstances` attributes; on any doubt the step is
  advisory.
- **ASG migration:** `ec2:CreateLaunchTemplateVersion` (new instance type) →
  `autoscaling:StartInstanceRefresh` (rolling, honors min-healthy). Rollback
  = point template back + refresh again; the ledger stores the prior version.
- Spot for fleets/ASGs (mixed-instances policy changes): advisory in v1.

**Never.**
- Never `ec2:TerminateInstances`. Termination stays a human/provider concern.
- Never stop an instance with instance-store data, never resize storage down,
  never change tenancy/platform.
- Never touch instances carrying `kubernetes.io/cluster/*` tags (they belong
  to the `k8s-nodes` domain), `aws:autoscaling:groupName` unless acting on
  the ASG itself, or a `kilter.dev/mode=off` tag (tag guardrails mirror the
  K8s annotations).
- Never purchase, exchange, or modify RIs/Savings Plans. Commitment data is
  read-only input.
- Never act on CPU alone when a decision needs memory (memory-blind rule).

**HITL gate.** Instance stop/resize and ASG refresh require an approved plan
fingerprint (`kilter approve`) — no auto-apply default, ever, in v1. EBS
modifications may run under `mode=apply` + change window without per-plan
approval (online, reversible upward). Tag-based guardrails mirror the K8s
annotation semantics.

**Least-privilege IAM.**

```json
[
  {
    "Sid": "KilterEC2Observe",
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeInstances", "ec2:DescribeInstanceTypes",
      "ec2:DescribeVolumes", "ec2:DescribeVolumesModifications",
      "ec2:DescribeImages", "ec2:DescribeLaunchTemplateVersions",
      "autoscaling:DescribeAutoScalingGroups",
      "cloudwatch:GetMetricData", "cloudwatch:ListMetrics",
      "ec2:DescribeReservedInstances", "savingsplans:DescribeSavingsPlans",
      "ec2:DescribeSpotPriceHistory", "pricing:GetProducts"
    ],
    "Resource": "*"
  },
  {
    "Sid": "KilterEC2ObserveOptional",
    "Effect": "Allow",
    "Action": [
      "ce:GetReservationUtilization", "ce:GetSavingsPlansUtilization",
      "compute-optimizer:GetEC2InstanceRecommendations",
      "compute-optimizer:GetEBSVolumeRecommendations",
      "compute-optimizer:GetAutoScalingGroupRecommendations"
    ],
    "Resource": "*"
  },
  {
    "Sid": "KilterEC2Actuate",
    "Effect": "Allow",
    "Action": [
      "ec2:ModifyVolume",
      "ec2:StopInstances", "ec2:StartInstances",
      "ec2:ModifyInstanceAttribute",
      "ec2:CreateLaunchTemplateVersion", "ec2:ModifyLaunchTemplate",
      "autoscaling:StartInstanceRefresh"
    ],
    "Resource": "*",
    "Condition": {
      "StringNotEquals": { "aws:ResourceTag/kilter.dev/mode": "off" }
    }
  }
]
```

Split observe/actuate into two roles; the brain/collector gets only the first,
the controller only the second. Actuation is further scoped by tag condition.

### 3.4 Assessed domains — include or defer

| Domain | Verdict | Rationale |
|---|---|---|
| **ECS on Fargate** | **Include (after EKS Fargate + EC2)** | Reuses the entire Fargate economics engine (§4.1–4.3) byte-for-byte. Observation is easy: ECS publishes per-service `CPUUtilization`/`MemoryUtilization` (percent of *reserved*, 1-min, free, automatic for Fargate services) — [verified: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/cloudwatch-metrics.html]. Per-task detail needs paid Container Insights; v1 works service-level. Actuation is clean and rolling: `ecs:RegisterTaskDefinition` (new revision with corrected cpu/memory) + `ecs:UpdateService`; rollback = point back to prior revision. Uniquely, ECS Fargate has **Spot (up to 70 % off)** and **ARM (~20 % cheaper, incl. Spot since 2024)** — [verified: fargate/pricing + whats-new 2024/09] — so capacity-provider and architecture advice lands here, not on EKS. IAM: `ecs:ListClusters/ListServices/DescribeServices/DescribeTaskDefinition` + `cloudwatch:GetMetricData` (observe); `ecs:RegisterTaskDefinition`, `ecs:UpdateService` (actuate). Never: change desired count (that's their autoscaler's job), never flip a service to Spot without approval. |
| **Lambda** | **Include as advisory-only, low priority** | Memory is the only knob and it also scales CPU (§4.8). Observe: `lambda:ListFunctions`/`GetFunctionConfiguration`, CloudWatch `Duration`/`Invocations`, and max-memory-used from CloudWatch Logs `REPORT` lines (`logs:StartQuery`/`GetQueryResults`) or `compute-optimizer:GetLambdaFunctionRecommendations` [verified action name]. Decide: raise memory floor over measured max (OOM protection), flag ARM migration (−20 % GB-s), and propose *stepwise* memory trials — duration response to memory is workload-specific and cannot be predicted from metrics, so Kilter must not claim savings it can't compute. Actuate (later, gated): `lambda:UpdateFunctionConfiguration` with published versions + alias for instant rollback. |
| **RDS / Aurora** | **Defer; report-only if requested** | Stateful, resize implies failover/downtime, storage cannot shrink, licensing (Windows/Oracle) breaks the pure price model, and memory signals (`FreeableMemory`) need careful interpretation vs page cache. High blast radius, modest ROI for Kilter's audience v1. A future `rds` domain fits the same abstraction (`DescribeDBInstances` + CW metrics → advisory recs). |
| **Batch** | **Defer as a domain; covered transitively** | AWS Batch runs on ECS/Fargate/EC2/EKS compute environments — rightsizing those substrates (already in scope) captures most of the money. A Batch-specific optimizer (job-queue shaping, allocation strategies) is scheduler work, not rightsizing. |

---

## 4. Economics, correct (Part B)

### 4.1 Fargate pricing model

**Rates (us-east-1, Linux) — [verified: https://aws.amazon.com/fargate/pricing/]:**

| Dimension | x86 | ARM (Graviton) |
|---|---|---|
| per vCPU-second | $0.000011244 | $0.0000089944 |
| per GB-second | $0.000001235 | $0.0000009889 |
| ⇒ per vCPU-hour | $0.04048 | $0.03238 |
| ⇒ per GB-hour | $0.004445 | $0.00356 |

- Billing is **per-second with a 1-minute minimum** (Linux; Windows has a
  5-minute minimum and license fees, and Windows is ECS-only). [verified]
- **Ephemeral storage:** 20 GB free per pod/task; beyond that
  $0.0000000308/GB-s (≈ $0.081/GB-month). EKS pods can raise it to 175 GiB
  via `ephemeral-storage` requests. [verified: pricing page +
  fargate-pod-configuration.html]
- **Fargate Spot:** up to ~70 % discount, **ECS only** ("Amazon EKS doesn't
  support Fargate Spot"). [verified]
- **ARM on Fargate:** ECS yes; **EKS no** (docs comparison table; open
  request containers-roadmap#1629). [verified]
- **Savings Plans:** Compute Savings Plans cover Fargate "up to 50 %"
  [verified: pricing page]; EC2 *Instance* Savings Plans cover Fargate **not
  at all** (§4.4).
- EKS itself adds a per-cluster control-plane fee on top of pod pricing —
  [not re-verified: aws.amazon.com/eks/pricing/] — irrelevant to per-pod
  deltas, relevant to whole-platform comparisons.

**Valid configurations — [verified: fargate-pod-configuration.html]:**

| vCPU | Memory options |
|---|---|
| 0.25 | 0.5, 1, 2 GB |
| 0.5 | 1, 2, 3, 4 GB |
| 1 | 2–8 GB, 1-GB steps |
| 2 | 4–16 GB, 1-GB steps |
| 4 | 8–30 GB, 1-GB steps |
| 8 | 16–60 GB, **4-GB** steps |
| 16 | 32–120 GB, **8-GB** steps |

**How EKS sizes a pod (the quantization function) — [verified, same page]:**

1. `initReq` = max over init containers' requests;
   `runReq` = sum over long-running containers' requests.
2. Effective request per dimension = max(initReq, runReq).
3. **Fargate adds 256 MB to the memory request** for kubelet/kube-proxy/
   containerd.
4. Round *up* to the closest valid configuration from the table (both
   dimensions must fit). Unspecified requests get the smallest config
   (0.25 vCPU / 0.5 GB).
5. The chosen config is stamped on the pod as `CapacityProvisioned` and is
   what you pay for.

Price of a config: `P(v, g) = v·rate_vcpu + g·rate_gb` per hour.
For x86: `P(v,g) = 0.04048·v + 0.004445·g`.

#### 4.1.1 The overhead cliff (AWS's own example)

Request 1 vCPU / 8 GB. Fargate adds 256 MB → 8.25 GB; no 1-vCPU config offers
9 GB (1 vCPU caps at 8 GB), so the pod is provisioned **2 vCPU / 9 GB** —
[verified: the exact example in the AWS docs].

- Intended price: P(1, 8) = 0.04048 + 0.03556 = **$0.07604/h** ($55.5/mo)
- Billed price: P(2, 9) = 0.08096 + 0.04001 = **$0.12097/h** ($88.3/mo)
- Penalty: **+59 %** for 256 MB of overhead.

Kilter's boundary-shave rec: if learned memory peak allows requests ≤
7.75 GB (7.75 + 0.25 = 8 GB exactly), the pod lands on 1 vCPU / 8 GB and the
bill drops 37 %. This single rule is the highest-yield Fargate optimization
that node-centric tools cannot see.

#### 4.1.2 Quantizer contract

Pure function, exhaustively table-tested (§6 U1), validated in production by
comparing its output against the `CapacityProvisioned` annotation on every
observed pod — any mismatch is surfaced as a bug-grade insight, never
silently absorbed.

#### 4.1.3 Small-pod example

Pod requests 200 m CPU / 512 Mi. Effective: 0.2 vCPU, 0.5 GiB + 0.25 GiB =
0.75 GiB → config **0.25 vCPU / 1 GB** = 0.01012 + 0.004445 =
**$0.014565/h** ($10.6/mo). Note memory quantization skipped 0.75 → 1 GB
because 0.25 vCPU has no 0.75 GB option.

### 4.2 What discounts Fargate on EKS actually has

Only two levers exist: (1) request less (quantization-aware rightsizing —
Kilter's job), and (2) Compute Savings Plans (a purchasing decision Kilter
observes but never makes). No Spot, no ARM, no RIs on EKS Fargate. Any
optimizer claiming "move EKS pods to Fargate Spot" is wrong — this is a trap
Kilter's recommendations must structurally avoid (the domain type simply has
no spot dimension).

### 4.3 Fargate vs EC2: the crossover

**Exact method (Kilter's differentiator).** Kilter already owns a
constraint-complete bin-packing simulator. So the comparison is not a
formula, it is two *computed* bills for the same pod set `P`:

- `F(P) = Σ_p P(quantize(p))` — Fargate, per-pod, quantized (x86 rates on
  EKS).
- `E(P)` = price of `binpack.PlanNodes(P)` — cheapest feasible node set,
  including DaemonSet overhead, system reserve, and fragmentation, at
  on-demand / spot / commitment-adjusted node prices.

Both are pure and run in `kilter analyze` offline.

**Screening formula (for intuition and for insights when a full pack isn't
warranted).** For the m5 bundle (1 vCPU + 4 GB):

- EC2 m5: $0.048/bundle-h (m5.xlarge $0.192 ÷ 4 — [verified: vantage
  m5.xlarge]).
- Fargate x86: P(1,4) = $0.05826/bundle-h → **21.4 % premium at 100 %
  utilization**.

Fargate wins when *effective node utilization* `u` (requested ÷ purchased,
after system reserve, DaemonSets, and packing fragmentation) satisfies:

```
u < u* = P_ec2_bundle / P_fargate_bundle
```

| EC2 alternative | Bundle $/h | Crossover u* |
|---|---|---|
| m5 on-demand (x86) | 0.0480 | **82 %** |
| m7g on-demand (ARM, $0.1632/4 — [verified: vantage]) | 0.0408 | **70 %** |
| m5 spot (≈$0.070/instance — [verified: vantage, volatile]) | 0.0175 | **30 %** |
| m5 + max 3-yr Compute SP (−66 %) vs Fargate + same SP (−50 %) | 0.0163 vs 0.0291 | **56 %** |

Readings:

- Well-packed clusters (Kilter's own output typically reaches > 82 %
  requested-utilization on the packed subset) beat Fargate on x86 on-demand —
  *before* Graviton, Spot, or commitments, each of which EKS Fargate cannot
  use. For steady, dense fleets, **EC2 wins decisively**.
- Sparse/small clusters lose to Fargate: 6 pods of (200 m, 512 Mi) cost
  6 × $0.014565 = $0.0874/h ≈ $64/mo on Fargate vs one m5.large $70/mo (and
  realistically two nodes for HA, $140/mo) — **Fargate wins ~2×**, more when
  the cluster idles (per-second billing, no idle nodes).
- Bursty/scale-to-zero (CronJobs, dev environments, spiky queues with
  minutes-scale duty cycles): Fargate's per-second billing beats paying for
  idle node floors; Kilter classifies these via `pkg/patterns`
  (batch/bursty classes) and biases crossover advice accordingly.
- Worked dense example: 40 pods × (1 vCPU, 4 GB). Fargate: 40 × 0.05826 =
  **$2.33/h**. EC2: 40 vCPU/160 GB at ~3.6 vCPU usable per m5.xlarge → 12
  nodes = **$2.30/h** on-demand x86 (≈ tie); m7g **$1.96/h** (−16 %); spot
  **$0.84/h** (−64 %). The tie at OD-x86 is why naive "Fargate is 20 % more
  expensive" claims mislead — fragmentation eats the premium; the *real* EC2
  advantage is Graviton/Spot/commitments.

**Non-price gates** the crossover report must apply before suggesting a move
to Fargate: no DaemonSets serving the workload, no EBS/GPU/privileged/
hostNetwork needs, private-subnet availability, tolerance for
no-in-place-resize and Fargate patching evictions [all verified:
fargate.html].

### 4.4 EC2 purchase options and the commitment trap

**Option landscape (us-east-1, m5.xlarge, Linux — [verified: vantage]):**
on-demand $0.192/h; 1-yr standard RI (no upfront) ≈ $0.121/h effective
(−37 %); spot ≈ $0.070/h (−64 %, interruptible, volatile). Savings Plans —
[verified: https://aws.amazon.com/savingsplans/compute-pricing/ +
search corroboration]: **Compute SP** up to 66 %, applies to EC2 *any*
family/size/region *plus Fargate and Lambda*; **EC2 Instance SP** up to 72 %,
locked to one family in one region, **never covers Fargate/Lambda**. Lambda
request charges and Fargate ephemeral storage are SP-ineligible.

**How AWS applies commitments — [verified:
https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/apply_ri.html and
https://docs.aws.amazon.com/savingsplans/latest/userguide/sp-applying.html]:**

1. **Zonal RIs**: exact match (AZ, type, platform, tenancy).
2. **Regional RIs**: AZ-flexible; *size-flexible* within the instance family
   via normalization units (nano 0.25 … large 4, xlarge 8, 2xlarge 16 …),
   applied smallest-instance-first. Size flexibility **only** for regional +
   Linux/Unix + default tenancy (not Windows/RHEL/SUSE, not zonal, not
   dedicated, not G/P/Inf families).
3. **EC2 Instance SPs** (before Compute SPs, narrower applicability).
4. **Compute SPs**: applied to the *highest savings-percentage* usage first
   (tie → lowest SP rate), until the hourly commitment is exhausted; the
   commitment is use-it-or-lose-it *per hour*.
5. Remainder bills on-demand.

**The trap, modeled explicitly.** A rightsizing rec changes *usage*; the
*commitment* keeps billing regardless. Gross savings (OD delta) ≠ net savings
(bill delta). Kilter computes:

```
NetSavings(rec) = Bill(usage) − Bill(apply(rec, usage))
```

where `Bill` is a pure re-implementation of the waterfall above
(`CommitmentLedger`, §5.5) over the account's RI/SP inventory.

*Worked example 1 — family migration off an RI.*
One m5.xlarge, 100 % covered by a 1-yr no-upfront standard regional RI
(effective ≈ $0.121/h, committed until expiry). Naive rec: "migrate to
m7g.xlarge $0.1632/h, save 15 % vs $0.192 OD." Reality with no other m5
usage to absorb the freed RI: new bill = $0.1632 (m7g OD) + $0.121 (stranded
RI) = **$0.2842/h vs $0.121/h before — a 135 % increase**. Kilter suppresses
the rec until RI expiry (the rec resurfaces with a `validFrom` = expiry
date), or reports it as *conditional*: "net-positive only if the freed 8
normalization units are absorbed by other m5 usage (currently: none)".
Convertible RIs add an exchange path — Kilter may *note* it, never execute
it.

*Worked example 2 — downsizing inside an RI family.*
m5.xlarge (8 units) under a regional Linux m5.xlarge RI, rightsize →
m5.large (4 units). Bill before = RI rate; bill after = same RI rate (4
units used, 4 stranded). **Claimed 50 %, realized 0 %** — unless other m5
usage absorbs the 4 freed units, which the ledger checks against observed
uncovered m5 usage. The audit ledger's claimed-vs-measured comparison
(already shipped) would expose this after the fact; the CommitmentLedger
prevents it before the fact.

*Worked example 3 — Compute SP under-commitment.*
$2.00/h Compute SP, fully utilized. Rightsizing shrinks SP-eligible spend
below $2.00/h → the delta is paid anyway (per-hour, non-carryover —
[verified: sp-applying.html]). Net savings = max(0, eligible_spend −
commitment) delta, not the OD delta. Because Compute SPs also cover Fargate
and Lambda, absorption is account-wide — the ledger nets across *all*
domains Kilter observes, which is exactly why commitments are modeled once,
centrally, not per-domain.

**Data inputs:** `ec2:DescribeReservedInstances` (inventory, term, offering
class, scope), `savingsplans:DescribeSavingsPlans` (commitment $/h, type,
region/family for EC2-Instance SPs), optional `ce:GetReservationUtilization`
/ `ce:GetSavingsPlansUtilization` to corroborate pool utilization. SP
*rates* per usage type come from `DescribeSavingsPlansOfferingRates`
**[unverified action name]**; v1 falls back to a conservative bound (treat
SP-covered usage as zero-marginal-cost, i.e. assume full stranding) when
rates are unavailable — conservative means Kilter under-promises savings,
never over-promises.

### 4.5 Graviton economics

- EC2: m5.xlarge $0.192 → m7g.xlarge $0.1632 (same 4 vCPU/16 GiB) = **−15 %**
  on-demand, typically with higher per-core performance — [verified: vantage;
  performance claims not benchmarked here]. Spot m7g.xlarge ≈ $0.085.
- Fargate ARM: −20 % vCPU / −19.9 % GB vs x86 (§4.1 table) — **ECS only**.
- Lambda ARM: $0.0000133334 vs $0.0000166667 per GB-s = **−20 %** —
  [verified: lambda/pricing].
- Constraint everywhere: binary/AMI/image compatibility is not observable
  from metrics ⇒ **Graviton recs are always advisory** with the $ delta
  attached. In the K8s domain, multi-arch node groups are a
  Karpenter/provisioner concern; Kilter feeds the price signal.
- Commitment caveat: family-scoped commitments (m5 RIs, EC2-Instance SPs) do
  **not** follow to m7g/m6g — the §4.4 waterfall prices this automatically.

### 4.6 Burstable (T-family) credit analysis

Model — [verified: aws.amazon.com/ec2/instance-types/t3/ + EC2 unlimited-mode
docs + search corroboration]:

- T3 launches in **unlimited mode by default**. Baseline utilization:
  t3.micro 10 %, t3.small/medium 20 %, t3.large 30 %, t3.xlarge/2xlarge 40 %.
- Below-baseline 24 h average ⇒ sticker price covers everything.
- Sustained above baseline ⇒ surplus credits billed at **$0.05 per
  vCPU-hour** (Linux).

Effective hourly cost as a function of sustained utilization `u`:

```
cost(u) = sticker + max(0, u − baseline) × vCPUs × $0.05
```

t3.large ($0.0832/h, 2 vCPU, 30 % — [verified: vantage + t3 page]):

- vs m5.large ($0.096): breakeven at `0.0832 + (u−0.3)×0.1 = 0.096` →
  **u ≈ 43 %**. Above 43 % sustained CPU, the "cheap" t3.large is the
  expensive choice.
- vs m7g.large ($0.0816 — [not re-verified, from m7g family pricing]):
  t3.large *sticker* already exceeds it — for ARM-compatible sustained
  workloads T-family has no price case at all.

Signals: `CPUSurplusCreditsCharged` (realized $ — ground truth),
`CPUCreditBalance` trend (standard-mode throttling risk: balance → 0 pins
CPU at baseline — an *availability* insight, not just cost), 24 h/14 d CPU
average vs baseline. Decisions: T→fixed when surplus ≥ family delta;
fixed→T when p99 CPU ≪ baseline (with the class detector confirming
steady-low, not bursty-high); credit-mode change recommendations are
report-only. This generalizes the existing catalog `Burstable` flag — the
planner's "exclude burstable for sustained workloads" default stays, now
with per-instance evidence.

### 4.7 EBS rightsizing

Rates (us-east-1, from the pricing page's own examples — [verified:
https://aws.amazon.com/ebs/pricing/]): gp2 $0.10/GB-mo; gp3 $0.08/GB-mo with
3,000 IOPS + 125 MB/s free, then $0.005/prov-IOPS-mo and $0.06/prov-MBps-mo;
io1/io2 $0.125/GB-mo (+ IOPS charges).

gp2 performance model — [not re-verified: EBS gp2 docs]: 3 IOPS/GiB (min
100), burst to 3,000 below ~1 TB; throughput up to 250 MB/s.

**gp2 → gp3 at full performance parity is always ≥ 0.** For size `G` GiB:

```
Δ = 0.02·G − max(0, 3G − 3000)·0.005 − max(0, T_gp2(G) − 125)·0.06
```

- G ≤ 1000: Δ = 0.02·G and gp3's *sustained* 3,000 IOPS beats gp2's *burst*
  3,000 — strictly cheaper and faster. Example: 500 GB gp2 $50/mo → gp3
  $40/mo (−20 %).
- G = 4000 (12,000 gp2 IOPS, 250 MB/s): $400 → $320 + $45 IOPS + $7.50
  throughput = $372.50 (−6.9 %) at full parity; with *measured* p99 IOPS of
  4,000 → $320 + $5 + throughput-as-measured ≈ **−18 %**. Kilter provisions
  to measured p99 + headroom (CW `VolumeReadOps+VolumeWriteOps` per 5-min ⇒
  avg IOPS; floor at gp2's *delivered* baseline when the observation window
  is short, so parity is never silently degraded).
- io1/io2 → gp3 when measured IOPS ≤ 16,000 and latency class permits:
  storage alone is −36 %; flagged advisory when the volume looks
  latency-critical (databases detected by tags/attachment).

Execution: `ModifyVolume` is online; size can only grow; ~6 h cooldown per
volume [not re-verified]. Risk note: gp2 burst masks true steady-state needs
— Kilter requires a ≥ 7-day window covering business peaks before parity-
reduction recs (same MinWindow machinery as `pkg/recommend`).

### 4.8 Lambda memory→cost/latency curve

Facts — [verified: https://aws.amazon.com/lambda/pricing/]: $0.20/1M
requests; x86 $0.0000166667/GB-s, ARM $0.0000133334/GB-s; memory 128 MB –
10,240 MB in 1-MB steps; 1-ms billing granularity. CPU scales linearly with
memory; ≈ 1 full vCPU near 1,769 MB — [not re-verified:
docs.aws.amazon.com/lambda/latest/dg/configuration-memory.html].

Cost per invocation = `req_charge + duration_s × (MB/1024) × rate`. For
CPU-bound functions, duration ≈ k/memory until the function stops
parallelizing/saturates ⇒ GB-s (and thus cost) is *flat* while latency
drops — memory increases are often free speed. For I/O-bound functions,
duration is memory-invariant ⇒ cost rises linearly with memory. The optimum
is function-specific and **cannot be derived from a single operating point**
— which is why Kilter's Lambda domain is advisory: it (a) fixes memory
floors from measured max-memory-used (+ headroom) to prevent OOMs, (b)
quantifies the ARM −20 % opportunity, and (c) proposes *measured* power
tuning (one step at a time, or an offline AWS Power-Tuning-style sweep the
operator runs), rather than fabricating a curve it hasn't observed.

### 4.9 Source table

| Fact | Source (fetched 2026-08-25) |
|---|---|
| Fargate rates, per-second billing/1-min minimum, Spot −70 % (ECS), storage, SP up to 50 % | https://aws.amazon.com/fargate/pricing/ |
| Fargate config table, sizing/rounding, +256 MB, `CapacityProvisioned`, 1→2 vCPU example, ephemeral 175 GiB | https://docs.aws.amazon.com/eks/latest/userguide/fargate-pod-configuration.html |
| EKS Fargate: no Spot, no ARM, no DaemonSets/GPU/EBS/privileged, Guaranteed QoS, VPA Recreate, patching, Job billing | https://docs.aws.amazon.com/eks/latest/userguide/fargate.html |
| EKS Fargate ARM open request | https://github.com/aws/containers-roadmap/issues/1629 |
| RI application, size flexibility scope, normalization units | https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/apply_ri.html |
| SP application order (RI→EC2-SP→Compute-SP, highest-%-first, per-hour) | https://docs.aws.amazon.com/savingsplans/latest/userguide/sp-applying.html |
| Compute SP vs EC2 Instance SP coverage/percentages | https://aws.amazon.com/savingsplans/compute-pricing/ |
| EBS gp2/gp3/io rates and free baselines | https://aws.amazon.com/ebs/pricing/ |
| Lambda rates, granularity, memory range | https://aws.amazon.com/lambda/pricing/ |
| EC2 CW: 5-min basic / 1-min detailed / no memory without agent | https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/viewing_metrics_with_cloudwatch.html |
| ECS default service metrics incl. Fargate memory | https://docs.aws.amazon.com/AmazonECS/latest/developerguide/cloudwatch-metrics.html |
| ECS Graviton Spot on Fargate | https://aws.amazon.com/about-aws/whats-new/2024/09/amazon-ecs-graviton-based-spot-compute-fargate/ |
| Compute Optimizer API action names | https://docs.aws.amazon.com/compute-optimizer/latest/APIReference/API_Operations.html |
| m5.xlarge / m7g.xlarge / t3.large spot & OD spot-check | https://instances.vantage.sh/aws/ec2/{m5.xlarge,m7g.xlarge,t3.large} |
| T3 unlimited $0.05/vCPU-h, baselines | https://aws.amazon.com/ec2/instance-types/t3/ (+ EC2 unlimited-mode docs) |

**Explicitly unverified (do not build on without confirming):** Price List
`serviceCode` for Fargate SKUs; `DescribeSavingsPlansOfferingRates` exact
name/shape; EBS ModifyVolume 6-h cooldown; gp2 3-IOPS/GiB burst mechanics;
Lambda 1,769 MB = 1 vCPU; m7g.large price; Cost Explorer $0.01/request; EKS
control-plane fee tiers.

---

## 5. Unified Go abstraction (Part C)

### 5.1 Placement in the package graph

```
pkg/domain            (new, pure)   — Target/Spec/Recommendation/Plan + interfaces
pkg/domain/fargate    (new, pure)   — quantizer, tier table, k8s-fargate domain
pkg/domain/ec2        (new)         — plain-EC2 domain: pure decision core +
                                      collect/ (aws-sdk) + actuate/ (aws-sdk)
pkg/pricing/commit    (new, pure)   — CommitmentLedger (RI/SP waterfall)
pkg/pricing           (extended)    — Fargate + EBS rate tables (embedded JSON)
```

Dependency rules preserved: `pkg/domain*` decision code imports only `model`,
`histogram`, `forecast`, `patterns`, `recommend`, `pricing` — no SDK, no
client-go. AWS SDK imports live only under `collect/` and `actuate/`
subpackages, mirroring how `pkg/provider` already isolates aws-sdk-go-v2, and
they are wired in `cmd/` — the brain binary decision path never links a cloud
call. `pkg/model` gains only optional fields (§5.3).

### 5.2 Core types

```go
// Package domain generalizes Kilter's loop over compute domains. A domain is
// an organ: the core runs with zero domains registered, and every domain
// degrades to report-only when its collector or credentials are absent.
package domain

type Kind string

const (
    K8sNodes   Kind = "k8s-nodes"   // existing pipeline, adapted
    K8sFargate Kind = "k8s-fargate"
    EC2        Kind = "ec2"
    ECSFargate Kind = "ecs-fargate"
    Lambda     Kind = "lambda"
)

// TargetRef identifies one billable, independently resizable unit.
type TargetRef struct {
    Domain Kind   `json:"domain"`
    Scope  string `json:"scope"` // clusterID | accountID/region
    ID     string `json:"id"`    // workload key | instance ID | volume ID | ARN
    Name   string `json:"name,omitempty"`
}

// Spec is a resource specification in a domain's own vocabulary, with the
// canonical dimensions filled when they apply. Attrs carry domain axes
// (instanceType, volumeType, iops, throughputMBps, memoryMB, arch, tier…).
type Spec struct {
    Resources model.Resources   `json:"resources,omitempty"`
    Attrs     map[string]string `json:"attrs,omitempty"`
}

// ActionClass tells the executor (and the human) what applying costs.
type ActionClass string

const (
    ActionInPlace   ActionClass = "in-place"   // online, no disruption (ModifyVolume, K8s in-place resize)
    ActionRolling   ActionClass = "rolling"    // recreate behind a controller (Fargate resize, ECS deploy, ASG refresh)
    ActionStopStart ActionClass = "stop-start" // downtime (plain-EC2 instance resize)
    ActionAdvisory  ActionClass = "advisory"   // never auto-applied (Graviton, Lambda tuning, domain moves)
)

// Recommendation is the domain-generic sizing decision. The K8s container
// recommendation (recommend.Recommendation) is embedded via Evidence and
// projected into this shape at the domain boundary.
type Recommendation struct {
    Target   TargetRef `json:"target"`
    Current  Spec      `json:"current"`
    Proposed Spec      `json:"proposed"`

    CurrentHourlyUSD  float64 `json:"currentHourlyUSD"`
    ProposedHourlyUSD float64 `json:"proposedHourlyUSD"`
    // Gross = on-demand delta. Net = after the commitment waterfall (§4.4);
    // Net ≤ Gross always, and Net is what plans and the ledger claim.
    GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
    NetSavingsMonthlyUSD   float64 `json:"netSavingsMonthlyUSD"`

    Action     ActionClass `json:"action"`
    Risk       string      `json:"risk"`       // reuses plan.RiskLow/Medium/High
    Confidence float64     `json:"confidence"` // 0..1, same semantics as recommend
    Evidence   []Evidence  `json:"evidence"`
    Reason     string      `json:"reason"`
    // ValidFrom defers recs blocked by a commitment term (§4.4 ex.1).
    ValidFrom time.Time `json:"validFrom,omitempty"`
}

// Evidence is one observable fact backing a recommendation — metric window,
// percentile, source system. Every recommendation states its evidence; a
// rec with none is a bug.
type Evidence struct {
    Metric  string    `json:"metric"` // cpu-p95 | mem-peak | surplus-credits-usd | capacity-provisioned | …
    Value   string    `json:"value"`
    Window  string    `json:"window,omitempty"`
    Samples int       `json:"samples,omitempty"`
    Source  string    `json:"source"` // metrics.k8s.io | cloudwatch | annotation | compute-optimizer
    At      time.Time `json:"at,omitempty"`
}
```

### 5.3 Interfaces: Collector / Domain / Actuator

The split mirrors agent / brain / controller and keeps rule 2 (§1) literal:
`Domain` is pure; only `Collector` and `Actuator` may hold SDK clients.

```go
// Snapshot is the domain-generic unit a collector ships to the brain —
// the analogue of model.ClusterSnapshot for non-K8s domains.
type Snapshot struct {
    Domain    Kind      `json:"domain"`
    Scope     string    `json:"scope"`
    Timestamp time.Time `json:"timestamp"`
    Targets   []Target  `json:"targets"`
    Samples   []Sample  `json:"samples,omitempty"`
    // Commitments piggyback on EC2-domain snapshots; the brain owns one
    // account-wide ledger regardless of which domain delivered it.
    Commitments *commit.Inventory `json:"commitments,omitempty"`
}

type Target struct {
    Ref     TargetRef         `json:"ref"`
    Spec    Spec              `json:"spec"`
    Labels  map[string]string `json:"labels,omitempty"` // tags; guardrail source
    Blind   []string          `json:"blind,omitempty"`  // e.g. "memory": declared blind spots
}

// Sample is one metric observation for a target. Non-K8s domains feed these
// into the same decaying-histogram store keyed by (TargetRef, Metric).
type Sample struct {
    Ref           TargetRef `json:"ref"`
    Metric        string    `json:"metric"` // cpu-mcores | mem-bytes | iops | throughput-mbps | surplus-usd | duration-ms
    Value         float64   `json:"value"`
    Timestamp     time.Time `json:"timestamp"`
    WindowSeconds int32     `json:"windowSeconds"` // 300 for CW basic — resolution is data, not config
}

// Collector runs agent-side. Implementations own SDK clients and budgets;
// every call is bounded by ctx. A collector failure yields a stale-marked
// domain, never a broken brain.
type Collector interface {
    Domain() Kind
    Collect(ctx context.Context) (*Snapshot, error)
}

// Domain runs brain-side and is PURE: Learn and Recommend perform no I/O.
type Domain interface {
    Kind() Kind
    // Learn folds a snapshot into learned state (histograms, floors, classes).
    Learn(snap *Snapshot) error
    // Recommend derives current recommendations; ledger nets commitments.
    Recommend(now time.Time, ledger *commit.Ledger) []Recommendation
    // PlanSteps orders approved recommendations into executable steps under
    // guardrails. Returns steps in the shared step envelope (§5.6).
    PlanSteps(recs []Recommendation, g guard.Config) ([]Step, error)
    // Checkpoint/Restore integrate with pkg/store (bbolt) like the
    // recommender's existing state does.
    Checkpoint() ([]byte, error)
    Restore([]byte) error
}

// Actuator runs controller-side; one per domain that supports execution.
// Execute must be idempotent per Step.Key: re-running a completed step is a
// no-op, matching the resumable-plan contract.
type Actuator interface {
    Domain() Kind
    Execute(ctx context.Context, step Step) error
    // Revert undoes a step from its recorded From state where the action
    // class permits; irreversible steps return ErrIrreversible (honest undo).
    Revert(ctx context.Context, step Step) error
}
```

The **registry** is explicit wiring in `cmd/`, not global state:

```go
type Registry struct{ domains map[Kind]Domain }

func (r *Registry) Register(d Domain) { r.domains[d.Kind()] = d }
```

The brain iterates registered domains on its existing tick; `analyze` embeds
whichever domains its flags configure. Nothing registered ⇒ today's behavior,
bit-for-bit.

### 5.4 Reuse vs bypass, package by package

| Package | k8s-fargate | ec2 | ecs-fargate | lambda |
|---|---|---|---|---|
| `histogram`, `forecast`, `patterns` | reused as-is | reused as-is (keyed by TargetRef+metric) | reused | reused |
| `recommend` (container policy) | **reused verbatim** — same containers, same histograms | bypassed (targets aren't containers; per-target sizers implement the policy analogue: p95+headroom CPU, peak+floor memory) | partially (service-level util % → reserved units) | bypassed |
| `binpack` | **bypassed** (nothing to pack) — but *reused* by the crossover report to price the EC2 alternative | bypassed | bypassed | bypassed |
| `plan` (node consolidation) | bypassed | bypassed | bypassed | bypassed |
| `plan` (fingerprint/approval/risk envelope) | reused via shared step envelope | reused | reused | reused |
| `safety` (cooldowns, regression watch) | reused (pod-level, unchanged) | generalized cooldowns per TargetRef | reused (deployment health) | n/a (advisory) |
| `actuate` (K8s patch/evict) | **reused verbatim** — the only actuator this domain needs | not used | not used | not used |
| `provider` | untouched | untouched (provider stays node-lifecycle-only; EC2 actuation is a Domain Actuator, not a Provider — different contract, different blast radius) | untouched | untouched |
| `store`, `api`, UI, ledger | extended with a domain dimension in keys/routes (additive) | same | same | same |

The K8s pipeline itself becomes the `k8s-nodes` domain via a thin adapter,
and one new pure function splits mixed clusters:

```go
// SplitFargate partitions a cluster snapshot into the node-backed cluster
// and the Fargate pod set. Fargate "nodes" (label
// eks.amazonaws.com/compute-type=fargate) never enter binpack or node
// pricing; their pods are priced by the quantizer instead.
func SplitFargate(s *model.ClusterSnapshot) (nodes *model.ClusterSnapshot, fargate []FargatePod)
```

`model` changes (all optional, backward-compatible JSON):

- `PodSpec.InitRequests model.Resources` — max over init containers; needed
  by the quantizer (collect fills it; zero value = no init containers).
- `PodSpec.ProvisionedCapacity model.Resources` — parsed
  `CapacityProvisioned` annotation (ground truth for Fargate billing).
- `NodeSpec.ManagedBy` gains value `"fargate"` (set from the compute-type
  label), which existing RespectManagedNodes logic already excludes from
  consolidation — the mispricing fix rides an existing mechanism.

### 5.5 Pricing extensions (pure, embedded, overridable)

```go
// pkg/pricing/fargate.go — embedded fargate.json mirrors catalog.json.
type FargateRates struct {
    VCPUHourlyUSD, GBHourlyUSD           float64 // x86
    ArmVCPUHourlyUSD, ArmGBHourlyUSD     float64 // ECS only; 0 ⇒ unavailable
    SpotDiscount                         float64 // ECS only; 0 ⇒ unavailable
    StorageGBHourlyUSD                   float64
    FreeStorageGB                        int
}

// Config is one valid Fargate compute configuration.
type Config struct{ MilliCPU int64; MemoryGB float64 }

// Quantize maps effective pod requests (max(init, Σ long-running)) to the
// cheapest valid configuration, adding the 256MB Kubernetes overhead.
// ok=false ⇒ the pod exceeds 16 vCPU / 120 GB and cannot run on Fargate.
func Quantize(req, initReq model.Resources) (Config, bool) {
    eff := req.Max(initReq)
    needCPU := eff.MilliCPU
    needMem := eff.MemoryBytes + 256<<20
    best, ok := Config{}, false
    for _, c := range configs { // ~130 entries, generated from the §4.1 table
        if c.MilliCPU >= needCPU && int64(c.MemoryGB*float64(1<<30)) >= needMem {
            if !ok || rates.Cost(c) < rates.Cost(best) {
                best, ok = c, true
            }
        }
    }
    return best, ok
}

func (r FargateRates) Cost(c Config) float64 {
    return float64(c.MilliCPU)/1000*r.VCPUHourlyUSD + c.MemoryGB*r.GBHourlyUSD
}
```

```go
// pkg/pricing/commit — the commitment waterfall (§4.4), pure and
// unit-tested against the AWS documentation scenarios.
package commit

type Inventory struct {
    RIs          []ReservedInstance // zonal + regional, with family units
    SavingsPlans []SavingsPlan      // type, $/h commitment, family/region scope
    FetchedAt    time.Time
}

type Usage struct { // one hour of billable usage, pre-commitment
    Lines []UsageLine // {family, size-units, region, platform, tenancy, ODRate, SPRate, kind: ec2|fargate|lambda}
}

// Bill applies zonal RIs → regional RIs (small→large by normalization
// units) → EC2-Instance SPs → Compute SPs (highest-savings-% first) → OD,
// per the verified AWS application order, and returns the hourly bill.
func (inv *Inventory) Bill(u Usage) float64

// Net evaluates a recommendation: the bill delta, not the OD delta.
// Missing SP rates degrade conservatively: covered usage is treated as
// zero-marginal-cost, so Net under-reports savings rather than inventing them.
func (inv *Inventory) Net(before, after Usage) float64
```

`NormalizationUnits(size string) float64` encodes the verified table
(nano 0.25 … 112xlarge 896, metal per-family) with an exhaustive test.

### 5.6 Shared step envelope, approval, ledger

Non-K8s domains reuse the trust machinery through a domain-neutral step:

```go
type Step struct {
    Seq    int         `json:"seq"`
    Key    string      `json:"key"` // idempotency key: hash(domain, target, from, to)
    Target TargetRef   `json:"target"`
    Action ActionClass `json:"action"`
    From   Spec        `json:"from"` // recorded for Revert and the ledger
    To     Spec        `json:"to"`
    Risk   string      `json:"risk"`
    Detail string      `json:"detail"`
}
```

- **Fingerprints**: the existing deterministic content-hash approach applies
  unchanged over `(domain, steps)`; `kilter approve` needs no new UX.
- **Ledger**: entries gain a `domain` field. Measured savings per domain:
  K8s domains keep today's formula; cloud domains can later corroborate via
  Cost Explorer (optional organ; absent ⇒ claimed-only, labeled as such).
- **Guardrails**: change windows/freeze/circuit breaker are already
  domain-agnostic (time + global switches). Mode guardrails come from tags
  (`kilter.dev/mode` tag on instances/volumes/services) with identical
  semantics to the K8s annotations.
- **Timeouts/idempotency**: every actuator call takes a deadline from the
  step executor; `Step.Key` makes re-execution after controller restart a
  no-op, matching the resumable-plan contract.

### 5.7 EC2 domain internals (sketch)

```go
// pkg/domain/ec2 — pure decision core.
type sizer struct {
    cpu  *histogram.Histogram // p95/p99 over CW CPUUtilization × vCPUs
    mem  *histogram.Histogram // nil ⇒ memory-blind (rule: never shrink memory)
    burst creditState          // surplus $, credit balance trend
}

func (d *ec2Domain) recommendInstance(t Target, s *sizer, cat *pricing.Catalog,
    led *commit.Ledger) *domain.Recommendation {

    needCPU := int64(s.cpu.Percentile(0.95) * headroom)
    minMem := t.Spec.Resources.MemoryBytes // memory-blind floor
    if s.mem != nil {
        minMem = int64(s.mem.Max() * memHeadroom)
    }
    best := cheapestInstance(cat, needCPU, minMem, t.archConstraint(), t.perfFloors())
    if best == nil || best.Name == t.Spec.Attrs["instanceType"] {
        return nil
    }
    rec := buildRec(t, best)                    // gross savings from catalog
    rec.NetSavingsMonthlyUSD = led.NetMonthly(t, best) // §4.4 waterfall
    if rec.NetSavingsMonthlyUSD <= 0 {
        return suppressed(rec, led.Explain(t))  // visible, with reason + ValidFrom
    }
    return &rec
}
```

`collect/` batches `GetMetricData` (500 series/call, 5-min period), tags each
target with declared blind spots, and never blocks the brain: a failed region
poll ships a stale-marked snapshot. `actuate/` wraps the §3.3 flows with
pre-flight checks and per-step deadlines.

### 5.8 API/store surface (additive)

- bbolt: new buckets `domain/<kind>/<scope>/…` for domain state; existing
  buckets untouched.
- REST: `GET /v1/domains`, `GET /v1/domains/{kind}/recommendations`,
  `POST /v1/domains/{kind}/snapshots` (collector push, same auth). Existing
  routes unchanged; UI gains a per-domain savings card fed by the same
  endpoints.
- CLI: `kilter analyze --fargate` (crossover + tier report from kubeconfig
  alone), `kilter cloud-agent --domain ec2`, `kilter pricing sync-commitments`.

---

## 6. Implementation plan (Part D)

Ordered by ROI-per-effort. Every unit lands with `go test -race ./...` green,
unit tests listed, and an e2e scenario before the next unit starts. Fargate
can't run under kind, so Fargate e2e uses the simulator + labeled-node
fixtures through the exact production decision path (the same trick the
existing e2e uses for decisions), plus contract tests that validate the
quantizer against recorded `CapacityProvisioned` values.

**U1 — Fargate correctness fix + quantizer (S effort, high value).**
`pricing/fargate.go` (rates + configs + Quantize), `SplitFargate`,
`NodeSpec.ManagedBy="fargate"`, Fargate-aware `SnapshotCost` (price pods by
`ProvisionedCapacity`, else quantized requests — never by node capacity).
Fixes today's silent mispricing and stops binpack from planning Fargate node
removals. *Tests:* table-driven quantizer covering every tier boundary, the
+256 MB cliff, init-container max rule, >16 vCPU rejection; snapshot-split;
cost-resolution precedence; sim scenario: mixed cluster produces no
Fargate-node steps.

**U2 — `pkg/domain` seam + k8s-fargate recommendations (M).**
Core types (§5.2), registry, k8s-fargate Domain reusing `recommend` +
quantizer; tier-move and boundary-shave recs in `analyze`, `/insights`, UI.
Actuation via the existing resize path flagged `ActionRolling` (never
in-place on Fargate). *Tests:* rec math per class; boundary-shave only above
confidence + min-window; e2e (sim): overprovisioned Fargate pod →
tier-drop rec with exact $ delta; regression-revert path on a simulated
post-resize OOM.

**U3 — Fargate ⇄ EC2 crossover report (S–M).**
`F(P)` vs `E(P)` (reusing binpack) + non-price gates (§4.3); ships in
`analyze --fargate` and as insights. Advisory only. *Tests:* golden
scenarios (sparse wins Fargate, dense wins EC2, DaemonSet/EBS gates block).

**U4 — CommitmentLedger (M, unlocks honesty everywhere).**
`pkg/pricing/commit` waterfall + normalization table;
`kilter pricing sync-commitments` (DescribeReservedInstances,
DescribeSavingsPlans → JSON, credentials optional); `NetSavings` wired into
k8s-nodes plan claims and all domain recs. *Tests:* reproduce the verified
AWS doc scenarios (apply_ri Scenarios 1–2, sp-applying Scenarios 1–5) as
unit tests; stranding examples §4.4 ex.1–3; conservative fallback when SP
rates absent.

**U5 — Plain-EC2 domain, read-only (M–L).**
`cloud-agent --domain ec2` collector (inventory + GetMetricData), sizer with
memory-blind rule, burstable analytics (§4.6), Graviton advisory, report/UI.
No actuation yet. *Tests:* collector against recorded API fixtures
(mock asgAPI-style seams as in `provider`); sizer suppressions
(memory-blind, commitment-negative, K8s-tagged exclusion); resolution
handling (5-min windows).

**U6 — EBS rightsizing, read-only → online actuation (S–M).**
gp2→gp3 parity math (§4.7) on measured IOPS/throughput; then the first
non-K8s actuator: `ModifyVolume` with cooldown, progress polling, ledger,
revert-upward. Gated by mode tag + change window. *Tests:* parity formula
across the size/IOPS grid incl. the >1 TB regime; actuator idempotency
(re-execute completed step = no-op); e2e: mocked EC2 endpoint executes and
records a gp2→gp3 plan.

**U7 — EC2 instance/ASG actuation behind approval (M).**
Stop/modify/start with pre-flight refusals (instance-store, ENA/NVMe),
launch-template + instance refresh for ASGs, approval-mandatory, undo via
recorded From. *Tests:* pre-flight matrix; approval enforcement; refresh
rollback; step resumability across controller restart.

**U8 — ECS-on-Fargate domain (M).**
Service-level observer (default CW metrics), Fargate engine reuse, task-def
revision + UpdateService actuator, Spot/ARM advisories (ECS has both).
*Tests:* reserved-vs-used math from utilization %, revision rollback, gate
on deployments-in-progress.

**U9 — Lambda advisory domain (S).**
Memory-floor + ARM recs from CW/logs evidence; no actuation. *Tests:* floor
math from REPORT fixtures; cost-curve honesty (no claimed savings without
measured duration at the proposed memory).

**U10 — RDS/Batch assessment revisit.** Documented decision checkpoint, not
code: revisit after U5–U8 field feedback.

---

## 7. Top traps that produce wrong recommendations

1. **Commitment stranding.** Rightsizing or migrating an RI/SP-covered
   instance can *raise* net cost (up to +135 % in §4.4 ex.1) while the
   optimizer reports savings. Fix: all claims are `Bill()` deltas through the
   CommitmentLedger; commitment-negative recs are suppressed-with-reason and
   dated `ValidFrom` expiry.
2. **Fargate quantization blindness.** Pricing Fargate by node capacity or
   raw requests ignores the +256 MB overhead and tier rounding — the AWS
   docs' own example bills +59 % over the naive estimate; savings computed
   without `Quantize()` are fiction, and request changes that don't cross a
   tier boundary save exactly $0. Fix: quantizer everywhere, validated
   against `CapacityProvisioned`.
3. **Assuming ECS Fargate features on EKS.** No Fargate Spot and no
   ARM/Graviton on EKS Fargate (both verified) — a "move to Fargate
   Spot/ARM" rec on EKS is unactionable. The domain type encodes the
   difference.
4. **Memory-blind EC2 downsizing.** Default CloudWatch has *no* memory
   metric; shrinking an instance on CPU evidence alone invites OOM. Fix:
   never propose less memory than current without a memory signal.
5. **Burstable sticker-price mirage.** T-family "savings" invert above
   baseline (t3.large loses to m5.large past ~43 % sustained CPU; surplus
   credits bill $0.05/vCPU-h) — and a too-small fixed→T move can throttle at
   the baseline. Fix: credit-metric evidence required both directions.
6. **gp2→gp3 without performance parity.** Large gp2 volumes carry 3×GiB
   IOPS and burst; converting at baseline gp3 (3,000/125) silently degrades
   them. Fix: measured-p99 provisioning with gp2-delivered floors.
7. **One-pod-per-node confusion.** Fargate "nodes" fed to binpack yield
   nonsense consolidation plans and double-counted savings. Fix:
   `SplitFargate` before any node math.
