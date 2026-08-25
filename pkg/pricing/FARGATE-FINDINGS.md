# U1 — Fargate correctness fix + quantizer

Implements unit **U1** of `docs/design/compute-domains.md` §6: EKS Fargate pods
are priced by the vCPU/memory tier AWS actually bills, never by node capacity
or raw requests, and Fargate "nodes" are kept out of node math.

Files: `pkg/pricing/fargate.go` (new, ~500 lines incl. docs), a Fargate-aware
`SnapshotCost` in `pkg/pricing/pricing.go`, a minimal `pkg/model` addition
(`PodSpec.InitRequests`, `PodSpec.ProvisionedCapacity`, the `ManagedByFargate` /
`ManagedByKarpenter` / `LabelComputeType` constants and `NodeSpec.IsFargate`),
and four test files. No new dependency; `go.mod` and
`go.sum` are untouched.

---

## 1. What was wrong before this change

There was **no Fargate awareness anywhere in the repo** — `grep -ri fargate
--include='*.go'` returned nothing. Every Fargate pod in a mixed cluster was
priced as if its single-pod VM were a real machine:

| Path | Old behaviour | New behaviour |
|---|---|---|
| `Catalog.NodeHourlyCost` on a Fargate VM | priced by `kilter.dev/hourly-cost` → instance-type catalog → capacity fallback | `(0, SourceFargateNode)` — a Fargate VM has no node price |
| `Catalog.SnapshotCost` | Fargate VMs summed into `ClusterCost.Nodes` | VMs excluded; their pods priced into `ClusterCost.Fargate` |

Concrete numbers, from `TestSnapshotCostFargateMispricingRegression` — one pod
requesting 200 m / 512 Mi on a Fargate VM that reports 96 vCPU / 384 GB:

- old, via the `kilter.dev/hourly-cost` annotation: **$99.000000/h**
- old, via the capacity fallback: 96×0.0330 + 384×0.0044 = **$4.857600/h**
  (the fixture's `m5.24xlarge` is not in the embedded catalog, so an unknown
  instance type falls through to unit economics)
- correct (quantized to 0.25 vCPU / 1 GB): **$0.014565/h**

i.e. the old path overstated that pod's cost by **334×–6800×**, and every
"saving" derived from it was fiction. Even with a realistically-sized VM
(2 vCPU / 4 GB, `TestMixedClusterCostAccounting`) the fallback would have
charged $0.0836/h against a true bill of $0.0146/h — **5.7× over**.

**No existing test encoded the old Fargate behaviour**, so no existing test was
weakened, changed or deleted. The one pre-existing invariant that had to move is
documented in §5.

---

## 2. What was built

### `Quantize(req, initReq model.Resources) (FargateConfig, error)`

The heart of the unit, and the first thing written and tested.

1. Effective request = per-dimension `max(Σ long-running containers, max init
   container)` — init containers run one at a time, so they set a floor, they
   never stack (§4.1 step 1–2).
2. `+256 MiB` is added to memory **before** rounding (§4.1 step 3), with a
   saturating add so a `MaxInt64` garbage request cannot wrap into "fits".
3. Round **up** to the first entry of the complete 74-row valid-configuration
   table (§4.1 step 4). Negative garbage clamps to zero; an unspecified request
   therefore lands on 0.25 vCPU / 0.5 GB, matching §4.1 step 4's last sentence
   (256 MiB of overhead alone still fits the smallest tier).
4. Above 16 vCPU / 120 GB it returns `ErrFargateTooLarge` with the numbers in
   the message. There is no clamp-to-max fallback: such a pod cannot be
   scheduled, so pricing it would invent a bill.

### Rates as embedded data with an override seam

`FargateVCPUHourlyUSD = 0.04048`, `FargateGBHourlyUSD = 0.004445` (us-east-1,
Linux/x86, from the per-second rates on the AWS pricing page). Exposed as
`DefaultFargateRates()`, overridable through `LoadFargateRates(io.Reader)` /
`LoadFargateRatesFile(path)` / `Catalog.WithFargateRates`, mirroring how
`Load`/`LoadFile` handle `catalog.json`. Embedded as Go constants rather than an
embedded `fargate.json` only because file creation was outside this unit's
scope; the parse/validate/override path is identical.

### `SplitFargate(*model.ClusterSnapshot) (*model.ClusterSnapshot, []FargatePod)`

Removes Fargate VMs and their pods from a snapshot before any node math sees
them. Always allocates fresh `Nodes`/`Pods` slices (no aliasing footgun);
carries `Workloads`, `PDBs` and `Usage` through unchanged, because
`pkg/recommend` is reused verbatim for Fargate containers and must keep their
histories; deterministic (snapshot order).

### Fargate-aware cost resolution

`FargatePodConfig` implements the §4.1.2 precedence: `ProvisionedCapacity`
(the AWS-stamped `CapacityProvisioned` annotation) → `Quantize(requests)` →
never node capacity. `SnapshotCost` splits Fargate VMs out of `Nodes`, prices
their pods into `ClusterCost.Fargate` / `FargateHourlyUSD`, and reports every
data-integrity problem in `ClusterCost.Warnings`.

`ParseCapacityProvisioned("0.25vCPU 0.5GB")` and `FargateConfig.String()`
round-trip, which is what lets U2 run §4.1.2's production validation.

---

## 3. Invariants, and exactly where each is tested

| Invariant | Test |
|---|---|
| The tier table is exactly the §4.1 table: 74 rows, canonical order, no duplicates, and `FargateConfigs()` hands out a copy | `TestFargateConfigTableMatchesSpec` (`fargate_quantize_test.go`) |
| Every documented tier boundary rounds as AWS documents, in both directions | `TestQuantizeTierBoundaries` (41 literal cases), `TestQuantizeEveryTierIsReachableAndTight` (sweeps all 74 rows: largest fitting request hits the row exactly, +1 byte lands strictly higher) |
| The +256 MB overhead is added before rounding — the §4.1.1 cliff | `TestOverheadCliffWorkedExample`: 1 vCPU / 8 GB → **2 vCPU / 9 GB**, $0.07604 → $0.120965/h, **+59.08 %**; the 7.75 GB boundary shave returns it to 1 vCPU / 8 GB, **−37.1 %** |
| §4.1.3 worked example, exact dollars | `TestSmallPodWorkedExample`: 200 m / 512 Mi → 0.25 vCPU / 1 GB → **$0.014565/h**, **$10.632/mo** (asserted to 1e-12), plus §4.3's "6 pods ≈ $64/mo" |
| Init-container max rule (per dimension, never additive) | `TestQuantizeInitContainerMaxRule` (5 cases incl. "init is not additive"), `TestQuantizeSumsLongRunningContainers` for the contrast |
| \>16 vCPU / >120 GB is rejected explicitly, and the ceiling itself is reachable | `TestQuantizeCeilingRejectionIsExplicit`, plus 4 ceiling cases in `TestQuantizeTierBoundaries` |
| **A request change that does not cross a tier boundary saves exactly $0** | `TestRequestChangeWithinTierSavesExactlyZero` — 5 cases, asserted as bit-exact `saving != 0`, not a tolerance; each case also asserts it really is within-tier, so it cannot silently rot into a different test |
| Round-up is also cost-minimal, so quantization can stay rate-independent | `TestQuantizeRoundUpIsAlsoCheapest` (16×16 grid), `TestQuantizeIsRateIndependent`, and invariant 4 of `FuzzQuantize` |
| Output is always a valid tier, always ≥ request + overhead, always the smallest and cheapest such tier, monotonic in each dimension, deterministic, and rejected only when the ceiling genuinely cannot hold it | `FuzzQuantize` (6 int64 inputs, 7 seeds; ~680 k execs clean) and `FuzzQuantizeMonotonicDownward` (~4.2 M execs clean) |
| EKS has no Fargate Spot and no ARM — unrepresentable, not discouraged | `TestEKSFargateHasNoSpotAndNoArm`: reflection over `FargateRates` fails the build-out of any field named `*spot*`/`*arm*`/`*graviton*`; `Platform` has no exported field so no caller can mint another platform; `TestFargateRatesOverrideSeam` proves an override file containing `spotDiscount` or `armVCPUHourlyUSD` is **rejected**, not ignored |
| Snapshot split: partition, order, no aliasing, learning state preserved | `TestSplitFargate`, `TestSplitFargateNoFargateAndNil` |
| Cost precedence: provisioned → quantized → never node capacity | `TestSnapshotCostFargatePrecedence`, `TestFargatePodConfigPrecedenceUnit`, `TestNodeHourlyCostRefusesFargateNodes` (incl. a control case proving the guard, not the fixture, is what makes the cost 0) |
| Nothing is silently absorbed: invalid `ProvisionedCapacity`, quantizer/AWS disagreement, over-ceiling pod | `TestSnapshotCostFargateWarnings`, `TestSnapshotCostFargatePrecedence` (mismatch warning) |
| Determinism (no map-iteration order, no clock) | `reflect.DeepEqual` of two `SnapshotCost` runs in `TestSnapshotCostFargatePrecedence`; determinism check in `FuzzQuantize`; `SplitFargate` and `SnapshotCost` iterate slices, never maps (maps are membership-only) |
| **A mixed cluster produces no Fargate node-removal steps** | `TestMixedClusterPlansNoFargateNodeSteps` (`fargate_regression_test.go`, `package pricing_test` so it can import `pkg/plan`), 4 sub-tests; `TestMixedClusterCostAccounting` asserts the plan claims exactly one m5.xlarge of savings and not a cent of Fargate |

`go test -race -count=1 ./pkg/pricing/... ./pkg/model/...` and
`go test -race -short ./...` are green; `gofmt`, `go vet ./...`, `go build
./...` clean. Statement coverage of `fargate.go` is 100 % except
`LoadFargateRatesFile` (60 %: the `os.Open` success path is exercised only
through `LoadFargateRates`).

---

## 4. Finding: `ManagedBy="fargate"` alone does **not** close trap 7

Design §5.4 says the mispricing fix "rides an existing mechanism" — plan.go's
`RespectManagedNodes` skip of `ManagedBy != ""`. That is **half true**, and the
regression test caught the other half:

- `RespectManagedNodes` removes Fargate VMs from *removal candidacy*, so no
  Fargate node is cordoned, drained or deleted. ✅
- It does **not** remove them from `binpack.ClusterState`. On a snapshot with
  `ManagedBy="fargate"` but no taint, the consolidation simulator happily
  placed an evicted pod onto a Fargate VM:
  `step 2 (evict-pod) … evict default/c1 (sim target: fargate-2)` — a
  placement that cannot happen in reality, and one that makes the whole node
  removal it justifies unsound.

In a live cluster this is masked because EKS taints every Fargate VM
`eks.amazonaws.com/compute-type=fargate:NoSchedule`, which `binpack.fits`
honours. But that is a coincidence of the fixture, not a guarantee.
**`SplitFargate` before any node math is the only structural fix**, exactly as
§7 trap 7 says. The regression test asserts all three states separately
(ManagedBy-only → no removals; realistic ManagedBy+taint → fully clean;
bare label-only snapshot through `SplitFargate` → fully clean) and asserts that
the ManagedBy and split paths produce an identical plan fingerprint.

---

## 5. Deliberate deviations and deferrals, with reasons

1. **`Quantize` returns `error`, not `ok bool`** (design §5.5 sketched `bool`).
   "No silent failure" needs a reason attached, and callers need
   `errors.Is(err, ErrFargateTooLarge)` to distinguish "too big for Fargate"
   from any future rejection.
2. **Quantization is rate-independent** — the §5.5 sketch picked the
   cost-minimal fitting config, which would let an overridden rate file change
   which tier a pod is billed at. Tier choice is an AWS scheduling fact, so this
   implementation takes the first fit in canonical order (AWS's documented
   "round up") and *proves* it coincides with the cost-minimal one
   (`TestQuantizeRoundUpIsAlsoCheapest`, `FuzzQuantize` invariant 4). If AWS
   ever adds a row where they diverge, that test fails loudly.
3. **`FargateConfig.MemoryMiB int64`, not `MemoryGB float64`.** Tier identity is
   exact integer comparison; `MemoryGB()` is a derived accessor. Memory "GB" in
   the AWS table is binary (the 0.5 GB tier is 512 MiB), consistent with §4.1.3
   treating 512 Mi + 256 MB as 0.75 GiB.
4. **`ClusterCost` invariant changed** (additively): `HourlyUSD` is now the sum
   of `Nodes` **plus** `Fargate`, and `FargateHourlyUSD` is the latter. The
   existing `TestSnapshotCostNilAndSumInvariant` still passes unchanged because
   it uses no Fargate nodes. All new fields carry `omitempty`, so the
   `/cost` API response is backward compatible.
5. **Ephemeral storage is not priced.** 20 GB is free per pod and `model` has no
   `ephemeral-storage` request field; adding one exceeded "minimum edit to
   `pkg/model`". Deferred to whoever adds that field.
6. **Terminal Fargate pods are priced.** §3.2 documents that completed Job pods
   left on Fargate keep billing, so they are included; `plan.isTerminal` drops
   them from *packing*, which is a different question. U2's job-hygiene insight
   (`ttlSecondsAfterFinished`) reads `PodSpec.Phase` to find them.
7. **`plan.greenfieldFloor` still repacks Fargate pods** onto catalog nodes when
   handed an unsplit snapshot, making `GreenfieldHourlyUSD` an apples-to-oranges
   figure on mixed clusters. Fixing it means editing `pkg/plan` (out of scope);
   the `SplitFargate` wiring in §6 fixes it for free.
8. **No crossover math, no recommendations, no `pkg/domain`.** Those are U2/U3
   by design.

---

## 6. Exact wiring U2/U3 must do next

1. **`pkg/collect/collect.go` `ConvertNode`** — one branch beside the existing
   karpenter one:
   ```go
   if n.Labels[model.LabelComputeType] == "fargate" {
       out.ManagedBy = model.ManagedByFargate
   }
   ```
2. **`pkg/collect`, pod conversion** — fill the two new `model.PodSpec` fields:
   `InitRequests` = element-wise max over init containers, and
   `ProvisionedCapacity` = `pricing.ParseCapacityProvisioned(
   pod.Annotations["CapacityProvisioned"])` (`.Resources()`), ignoring the
   annotation when the parse fails and surfacing it as an insight.
3. **Call `pricing.SplitFargate` before node math** in the three places that own
   a snapshot: `plan.Build` (before `binpack.NewClusterState`),
   `cmd/kilter/analyze.go`, and `pkg/api/brain.go`. This is the structural fix
   for §4 above; without it, protection depends on the EKS taint being present
   in the snapshot.
4. **U2 recommendations** must price *both* sides through `Quantize` and emit
   nothing when `rates.Cost(before) == rates.Cost(after)` — the $0 case pinned
   by `TestRequestChangeWithinTierSavesExactlyZero`. The boundary-shave rec of
   §4.1.1 is `Quantize` of the current request vs `Quantize` of the largest
   request that stays on the cheaper tier.
5. **U2 must surface `ClusterCost.Warnings`** as bug-grade insights (§4.1.2);
   they are currently only carried, not routed.
6. **U3 crossover** gets `F(P) = Σ rates.Cost(Quantize(p))` from this package
   and `E(P)` from `binpack.PlanNodes` over the *split* node-side snapshot.
   It must not invent a spot or ARM Fargate side: `pricing.Platform` has exactly
   one value and no constructor.
