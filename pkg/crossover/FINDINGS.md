# U3 — Fargate ⇄ EC2 crossover report

Implements unit **U3** of `docs/design/compute-domains.md` §6: two computed
bills for the same pod set — `F(P)` through the U1 quantizer, `E(P)` through the
bin-packing simulator — the §4.3 non-price gates as hard blocks, and a
break-even a human can act on. Advisory only.

Files (new package, nothing outside it touched; `go.mod` and `go.sum`
unmodified):

| File | Lines | What |
|---|---|---|
| `crossover.go` | 707 | `Analyze`, `F(P)`, `E(P)`, density/break-even, verdict, `FromSnapshot` |
| `gates.go` | 300 | `Fact`/`Facts`/`Gate`/`Block`, the ten §4.3 gates, fact derivation |
| `report.go` | 174 | `Headline`, `Summary`, `Insight` — rendering only |
| 5 test files | 1 587 | goldens, per-gate isolation, degenerate matrix, determinism, purity, fuzz |

`gofmt`, `go vet ./...`, `go build ./...`, `go test -race -count=1
./pkg/crossover/...` and `go test -race -short ./...` are all green. Statement
coverage of the package is **98.8 %**. `FuzzReportNeverRecommendsBlockedFargate`
ran 207 403 executions clean.

---

## 1. What was built

### `F(P)` — the Fargate bill

`Σ_p rates.Cost(pricing.FargatePodConfig(p))`, which is U1's §4.1.2 precedence:
the AWS-stamped `CapacityProvisioned` annotation when the pod carries one, else
`Quantize(requests, initRequests)` — so the +256 MiB overhead and the tier
cliffs are in the number, never a `vCPU × rate` approximation. A pod above the
16 vCPU / 120 GB ceiling is **excluded from the sum and named** in
`FargateSide.Unpriced`; it is never clamped to the ceiling and billed, because
such a pod would never be scheduled.

The result carries a tier histogram (`Configs`) in canonical tier order, which
is what makes the boundary-shave opportunities of §4.1.1 visible in the same
report.

### `E(P)` — the EC2 bill

`binpack.PlanNodes` over the workload pods, priced at the plan's node prices,
with the three overheads that make it honest:

- **DaemonSet copies per node** — `PlanOptions.DaemonSetPods`, one template per
  DaemonSet (three observed `fluentbit` pods are one per-node overhead, not
  three: `TestDaemonSetPodsCollapseToTemplates`).
- **kubelet/system reserved** — `PlanOptions.SystemReservedFraction`, default
  0.08, carved out of every candidate's capacity before anything is packed.
  `Density.SystemReservedFraction` reports it back, asserted at 0.08 in
  `TestGoldenSparseWinsFargate`.
- **Whole nodes** — a node one pod short of full is billed whole. The dense
  golden buys 7 nodes for 40 pods and pays for all 7.

`EC2Side` also reports `Purchased`, `Allocatable` and `WorkloadRequests`, so
the gap between what you buy and what you use is in the report rather than
implied.

### The report

`Verdict` is five-valued on purpose. `fargate-blocked` ("stay on EC2 because
Fargate cannot run this") and `ec2` ("stay on EC2 because it is cheaper") are
different answers with different remedies; collapsing them into a boolean would
lose the reason, which is the only part a human can act on. `undecided` is a
real answer too — an empty pod set, or a pod set neither side can hold.

`Close` marks a gap under 5 %: the cheaper side still wins, but the reader is
told the margin is inside an embedded price table's own uncertainty. It is not
a verdict, so it never suppresses one.

---

## 2. Money convention

Hourly USD in `float64`, monthly via `pricing.HoursPerMonth` (730) — the same
convention as `pkg/pricing` and `pkg/pricing/commit`. **Money is never
`float32` and never compared with `==`.** Equality goes through `moneyEqual`,
whose tolerance (`moneyEps = 1e-9`) matches `commit.Eps`: an absolute floor for
near-zero amounts and a relative band above it, so float64 summation error over
a 5 000-pod cluster cannot manufacture a verdict. NaN is never equal to
anything, including itself. Pinned by `TestMoneyIsNeverComparedWithEquals`;
`VerdictTie` is proved to mean *equal*, not *close*, by
`TestTieIsExactNotApproximate`.

Ratios (`SavingsFraction`, both densities) are float64 in `[0, ∞)` and are
asserted finite and non-negative on every fuzz execution.

---

## 3. The break-even dimension: **effective node density `u`**

`Density.Achieved` = `value(workload requests) / value(purchased capacity)`,
after the system reserve, the DaemonSet copies and packing fragmentation have
taken their share. `Density.BreakEven` = `u* = u · E/F`: the density at which
the two bills tie.

**Why density and not pod count or duty cycle.**

1. It is §4.3's own axis. The design doc states the crossover as `u < u* =
   P_ec2_bundle / P_fargate_bundle` and tabulates `u*` per EC2 alternative.
   Reporting anything else would make Kilter's output incomparable with its own
   design.
2. It falls out of **one** pack. Pod count requires re-packing the set at many
   scale factors (7+ extra `PlanNodes` runs on a set that grows with the
   factor), and replicating a workload with anti-affinity or topology-spread
   constraints is not even semantically well defined. Duty cycle requires a
   runtime model this snapshot cannot support — see §5.
3. It **decomposes into levers**. `Summary` prints where every dollar of node
   capacity went: requested by workload pods, reserved for kubelet/system, or
   lost to DaemonSet copies and fragmentation. That is what a human changes.

The identity `u/u* = F/E` (asserted in `TestBreakEvenReducesToTheScreening-
Ratio`) is deliberate: the density framing carries no information beyond the
two bills, it re-expresses them on an axis with units. That also makes it
scalarization-independent — `resourceValueUSD` collapses the 2-D shape with a
fixed exchange rate (the same one `binpack` uses for packing efficiency), and
changing that rate moves `u` and `u*` together.

At `u = 1` — a perfect pack, no reservation, no fragmentation — `u*` reduces
exactly to §4.3's screening ratio. Which produced the first finding.

---

## 4. Findings

### 4.1 §4.3's screening table overstates the crossover by 6 points

§4.3 gives the m5 crossover as `u* = 0.0480 / 0.05826 = 82.4 %`, pricing the
Fargate bundle as `P(1 vCPU, 4 GB)`. **That bundle does not exist.** A pod
requesting 1 vCPU / 4 GiB is billed at 1 vCPU / **5** GB, because Fargate adds
256 MiB before rounding and the 1-vCPU row steps in whole gigabytes:

```
P(1,4) = 0.04048 + 4×0.004445 = $0.058260/h   ← the screening formula's bundle
P(1,5) = 0.04048 + 5×0.004445 = $0.062705/h   ← the bill
```

Computed through the quantizer, the m5 crossover is **76.55 %**, not 82.4 %.
The error is in the expensive direction — the screening formula makes Fargate
look better than it is, i.e. it would talk you *onto* Fargate at densities
between 76.5 % and 82.4 % where the node set is actually cheaper.
`TestBreakEvenReducesToTheScreeningRatio` asserts both numbers and shows the
overhead is the sole difference: recomputing `u*` against the overhead-free
bundle reproduces 0.823893 to 1e-6.

Same cause, same direction, in §4.3's worked dense example: 40 × (1 vCPU, 4 GB)
is quoted as "$2.33/h Fargate ≈ $2.30/h EC2, a tie". The real Fargate bill is
**$2.5082/h** against **$2.304/h**, and EC2 wins by 8.1 %
(`TestGoldenDenseWinsEC2`, arithmetic in the comment).

### 4.2 DaemonSets do not move the break-even line — they move you across it

`u* = value(W) · (E / value(C)) / F`, and `E / value(C)` is the price per unit
of capacity of the shape you buy. So for a fixed winning instance type, **the
break-even density is a pure price ratio and DaemonSet overhead cannot move
it.** What DaemonSet overhead moves is `u`: the same demand now needs more
nodes, so achieved density falls and you cross a stationary line.

`TestDaemonSetOverheadMovesTheAnswer` asserts exactly this, with hand-checked
arithmetic: one 200 m / 256 Mi DaemonSet cuts an m5.large from 6 workload pods
to 5, forcing a second node — `E` doubles from $0.096/h to $0.192/h, `u` halves
from 71.7 % to 35.9 %, and `u*` is **unchanged to 1e-9**.

The line *does* move when the overhead changes *which shape wins the pack*,
because that changes the price per unit of capacity.
`TestDaemonSetOverheadCanMoveTheBreakEvenLine` builds that case: a 2 GiB
DaemonSet evicts the memory-poor c5.large from the pack in favour of m5.large,
and `u*` **falls** from 0.49839 to 0.46499 — a cheaper-per-capacity shape needs
less density to beat Fargate. Direction asserted in both tests.

### 4.3 The two DaemonSet effects point in opposite directions, and the block wins

The same DaemonSet that makes the node set more expensive also makes Fargate
impossible (§4.3 gate: no DaemonSets serving the workload). So the report ends
up saying: *the node set now costs twice as much, it now costs 30 % more than
Fargate would, and you cannot have Fargate.* `TestDaemonSetOverheadMovesTheAnswer` asserts the
verdict goes `ec2` → **`fargate-blocked`**, never `fargate`, and that no saving
is claimed. This is the most expensive-to-get-wrong shape in the whole unit: it
is exactly the workload a price-only optimizer recommends moving.

### 4.4 "Not observed" blocks, and that is the honest default

A property nobody checked is not a property observed absent. `Fact` is
tri-state with `Unknown` as the **zero value**, so a zero `Facts` — a pod nobody
looked at — is blocked on all nine observable gates and says which observations
are missing (`TestZeroFactsBlocksOnEveryGate`). `BlockUnverified` is reported
distinctly from `BlockViolation`, so "impossible" and "unknown" never read the
same.

The practical consequence, stated plainly: **`FromSnapshot` on a live cluster
blocks every EC2-hosted pod today**, because `pkg/model` carries no volume,
security-context or subnet information (§6 lists the five missing observations
and where to fill them). The EC2 → Fargate direction is therefore honest-but-
unhelpful until the collector lands; the Fargate → EC2 direction works fully
today, because a pod AWS already runs on Fargate is compatible by observation
(`CompatibleFacts`, wired in `FromSnapshot`, asserted in
`TestFromSnapshotSplitsAndClassifies`).

That is the correct trade. The alternative — guessing, e.g. "StatefulSets use
EBS" — produces exactly the confident-and-wrong recommendation the gates exist
to prevent.

### 4.5 The EC2 side can buy Graviton; the Fargate side structurally cannot

With `Arch` unset the packer may buy arm64 shapes, which on the dense golden
takes `E` from $2.304/h to $2.0160/h (5 × m7g.2xlarge + 2 × m5.xlarge). EKS Fargate has no ARM
(`pricing.Platform` has exactly one value and no constructor), so this is a real
advantage of the node side — but binary compatibility is not observable from
metrics. The report states it in `Assumptions` **only when the pack actually
bought one** (`TestGravitonIsCalledOutOnlyWhenTheEC2SideUsesIt`), so the
assumption list describes this run rather than being a generic disclaimer.

---

## 5. Advisory only — enforced structurally

- No `Step` type, no actuator, no `error`-returning apply path. The only thing
  this package hands the rest of Kilter is a `model.Insight` (a finding) and a
  string.
- `TestPackageIsPureAndAdvisory` parses every non-test file in the directory
  and fails on: an import outside a 8-entry allowlist (`fmt`, `math`, `sort`,
  `strings`, `time`, `pkg/binpack`, `pkg/model`, `pkg/pricing`) — so a future
  edit importing `pkg/plan`, `pkg/actuate`, `pkg/safety`, `pkg/provider` or an
  AWS SDK fails here; any `time.Now()` in package logic; any package-level
  `var` (there are none — even the gate table is a function returning a fresh
  slice, `TestAllGatesIsACopy`); and any declaration named `*Step*`,
  `*Actuat*`, `*Execute*`, `*Apply*`, `*Patch*`, `*Revert*`, `*Approve*`.
- Determinism: `Analyze` is a pure function of `(now, pod set, options)`.
  Every output list is sorted by an intrinsic key and no map is iterated to
  produce output. `TestReportIsShuffleInvariant` (25 shuffles, `reflect.DeepEqual`
  on the whole report), `TestShuffleInvariantWithUnnamedPods`, and a shuffle
  check on **every** fuzz execution.

---

## 6. Every gate, and exactly where it is tested

Ten gates, each encoded separately so each can be tested alone.
`TestEachGateBlocksIndependently` runs a table over all nine `Facts` gates ×
{Present, Unknown}: it changes **one property of one pod** on a pod set that
otherwise wins on Fargate, and requires the report to come back
`fargate-blocked` by exactly that gate, naming exactly that pod, with zero
savings and a headline that never contains "cheaper".
`TestControlCaseWinsFargateSoGateTestsMeanSomething` proves the base fixture is
an unblocked Fargate win, so a passing gate test cannot be an artefact of an
unwinnable scenario.

| Gate | Blocks when | Derivable from `pkg/model` today? | Tested by |
|---|---|---|---|
| `daemonset` | a DaemonSet pod is in the set, or DaemonSets run beside it | ✅ `Workload.Kind` | `TestEachGateBlocksIndependently/daemonset/*`; set-level: `TestDaemonSetTemplatesBlockAtSetLevel`; interaction: `TestDaemonSetOverheadMovesTheAnswer` |
| `extended-resource` (GPU) | any `nvidia.com/gpu`-class request | ✅ `ContainerSpec.Extended` | `.../extended-resource/*`; derivation: `TestFactsFromPodSpec` |
| `ebs-volume` | an EBS-backed volume is attached | ❌ **Unknown** | `.../ebs-volume/*` |
| `privileged` | a privileged container / added capability | ❌ **Unknown** | `.../privileged/*` |
| `host-path` | hostPath or node-local volume | ✅ `HasLocalStorage` | `.../host-path/*`; derivation: `TestFactsFromPodSpec` |
| `host-network` | `hostNetwork: true` | ❌ **Unknown** | `.../host-network/*` |
| `host-port` | a container binds a host port | ❌ **Unknown** | `.../host-port/*` |
| `private-subnet` | no private subnet for a Fargate profile | ❌ **Unknown** | `.../private-subnet/*` |
| `eviction-intolerant` | pod cannot tolerate recreation (no in-place resize, platform patching evictions) | ✅ `DoNotEvict` | `.../eviction-intolerant/*`; derivation: `TestFactsFromPodSpec` |
| `size-ceiling` | > 16 vCPU / 120 GB after the 256 MiB overhead | ✅ computed by `Quantize` | `TestSizeCeilingGateBlocksAlone` |

Cross-cutting gate tests: `TestZeroFactsBlocksOnEveryGate` (unknown blocks, in
`AllGates` order), `TestBlockPodListIsCappedAndCounted` (a 5 000-pod cluster
does not print 5 000 lines, and the dropped count is never lost),
`TestCompatibleFactsPassesEveryGate` (reflection over every `Facts` field, so a
field added without a gate fails the build-out), and
`FuzzReportNeverRecommendsBlockedFargate` — which asserts, on every execution,
that a blocked pod set is never `VerdictFargate`, never `Eligible`, never
carries a saving, and never produces a headline containing "cheaper".

Degenerate-input matrix (`TestDegenerateInputs`, 11 sub-tests): empty pod set,
nil pod slice, one pod above both the Fargate ceiling and every node, one pod
Fargate can hold that no node can, a mixed set with one unschedulable pod, an
empty candidate list, a catalog with no instances for the provider, negative
requests (clamp, never mint capacity), `MaxInt64` requests (saturate and reject,
never wrap into "fits the smallest tier"), terminal-pods-only, and duplicate
UIDs.

---

## 7. Deliberately deferred, with reasons

1. **Duty cycle.** `F(P)` and `E(P)` are both steady-state, 730 h/month. Fargate
   bills per second with a 1-minute minimum, so for CronJobs, batch and
   scale-to-zero workloads this report *understates Fargate*, sometimes by an
   order of magnitude. Modelling it needs a runtime/idle model from
   `pkg/patterns` plus a window the snapshot alone cannot supply, and a wrong
   duty cycle multiplies the answer rather than nudging it. The assumption is
   printed on **every** report (`Assumptions[0]`), including the direction of
   the error. This is the single highest-value follow-up.
2. **Commitments.** Both sides are priced at list. `pkg/pricing/commit` (U4)
   landed this round and can net an RI/SP-covered node side; wiring it needs a
   `commit.Inventory` on the call, which is a signature this unit should not
   invent before the caller exists. Stated in `Assumptions[2]`, including the
   direction: a commitment already covering the node side makes a move away
   from it *worse* than shown (§4.4 ex.1, +135 %).
3. **Per-workload verdicts.** The report answers "this pod set", not "each
   workload". Splitting a set into per-workload sub-reports is a caller-side
   loop over `Analyze`; doing it inside would have to invent a policy for
   DaemonSet overhead attribution.
4. **Ephemeral storage** (20 GB free, then $0.081/GB-month) is not priced:
   `pkg/model` has no `ephemeral-storage` request field, and adding one is
   outside this unit's scope. Same deferral as U1 §5.5. Stated in
   `Assumptions[4]`.
5. **Windows / OS gate.** EKS Fargate is Linux-only, and a Windows workload
   moved to it is unschedulable. `pkg/model` carries no pod OS, so this is not
   a gate here; the collector should surface `kubernetes.io/os` and it becomes
   a tenth `Facts` field.
6. **Fargate-side node affinity / topology spread.** Fargate has no nodes, so
   pod-level topology constraints silently stop applying on a move. This is
   arguably an eleventh gate; it was left out because it needs a policy call
   (is losing zone spread a block or a risk?) that belongs with whoever owns
   the availability story.
7. **Fargate profile matching** (namespace/label selectors deciding *which*
   pods can land on Fargate at all) is not modelled; `eks:ListFargateProfiles`
   is an enrichment the collector does not do yet (§3.2).

---

## 8. Exact wiring the next unit must do

### 8.1 `kilter analyze --fargate` (`cmd/kilter/analyze.go`)

```go
fargate := fs.Bool("fargate", false, "compare this cluster's pods against EKS Fargate pricing")
...
if *fargate {
    rep := crossover.Analyze(time.Now(), crossover.FromSnapshot(snap), crossover.Options{
        Catalog: catalog,
        // Provider/Arch default to aws/any; set Arch to "amd64" to price the
        // node side without Graviton.
    })
    if *jsonOut {
        json.NewEncoder(os.Stdout).Encode(rep)
    } else {
        fmt.Print(rep.Summary())
    }
}
```

`analyze` is the natural home: `FromSnapshot` needs nothing but the snapshot
the collector already builds, and both bills are pure. Note the call passes
`time.Now()` — this package never reads a clock, so `analyze` (and the brain,
and every test) supplies it.

**`FromSnapshot` is also the fix for §7 trap 7 in this path**: it calls
`pricing.SplitFargate` first, so a Fargate VM's 96 vCPU / 384 GB shell can never
enter the pack. `TestFromSnapshotSplitsAndClassifies` asserts it does not.

### 8.2 `/insights` (`pkg/api/brain.go`, `Brain.Insights`)

Beside the existing `plan.BuildSpotReport` block, which is the same shape
(advisory finding with a savings threshold):

```go
if ps := crossover.FromSnapshot(snap); len(ps.Pods) > 0 {
    rep := crossover.Analyze(time.Now(), ps, crossover.Options{Catalog: b.catalog})
    switch rep.Verdict {
    case crossover.VerdictFargate, crossover.VerdictEC2:
        if rep.MonthlySavingsUSD >= 10 && !rep.Close {
            out = append(out, rep.Insight())
        }
    case crossover.VerdictFargateBlocked:
        // Optional: surface only when the blocks are BlockViolation, not
        // BlockUnverified — an unverified block is a gap in Kilter's
        // collector, not a finding about the user's cluster.
    }
}
```

`rep.Insight()` returns a `model.Insight{Kind: "fargate-crossover", Severity:
"info"}` whose `Message` is `rep.Headline()` verbatim. It is a finding, not an
action: nothing in `pkg/plan` or `pkg/actuate` consumes `Insight`, and
`TestPackageIsPureAndAdvisory` keeps it that way.

### 8.3 The five observations that unblock the EC2 → Fargate direction

`FactsFromPodSpec` fills four of nine gate facts. To make the EC2 → Fargate
direction useful, `pkg/collect` must fill the other five — and `pkg/model` must
carry them. In priority order (most workloads blocked first):

| `Facts` field | Kubernetes source | `pkg/model` addition |
|---|---|---|
| `EBSVolume` | pod volumes → PVC → StorageClass provisioner `ebs.csi.aws.com` (or `awsElasticBlockStore`) | `PodSpec.BlockVolumes []string` or a `HasBlockStorage bool` |
| `HostNetwork` | `pod.Spec.HostNetwork` | `PodSpec.HostNetwork bool` |
| `Privileged` | any `container.SecurityContext.Privileged` / added capabilities | `ContainerSpec.Privileged bool` |
| `HostPort` | any `container.Ports[].HostPort != 0` | `PodSpec.HostPorts []int32` |
| `NoPrivateSubnet` | `eks:DescribeFargateProfile` + subnet route tables, cluster-level | not a `PodSpec` field — set it on every pod from cluster context |

Each is a pure additive `pkg/model` field plus one line in `collect`'s pod
conversion. When one lands, flip it from `Unknown` to `factOf(...)` in
`FactsFromPodSpec` and update `TestFactsFromPodSpec`, which asserts the exact
Unknown set and will fail loudly.

### 8.4 What must NOT be wired

- **No actuation.** Domain migration is advisory in this design (§3.2). A pod
  does not move to Fargate by a Kubernetes patch — it moves by a Fargate
  profile change plus a recreate — and none of that belongs to Kilter.
- **No spot or ARM Fargate side.** `pricing.Platform` has exactly one value and
  no constructor; `Options` deliberately has no field that could be mistaken
  for one.
- **No savings claim from `MonthlySavingsUSD` into the ledger.** It is the gap
  between two *modelled* bills for the same pods, not a delta against the
  current invoice, and it is not netted against commitments (§7.2). The ledger
  claims plan deltas; this is a report.
