# pkg/explain — Deterministic explain + cost attribution (design §4.5, unit 4)

The explanation plane: the full explain payload behind one recommendation,
and `why-cost` — an additive, individually-citable decomposition of a
cluster's hourly cost change over a window.

No model, no clock, no network. Stdlib plus `pkg/model`, `pkg/evidence`,
`pkg/recommend`, `pkg/decision`. **Not** `pkg/api` — the audit ledger lives
above every decision package, so ledger entries arrive through the local
`LedgerAction` projection ([mapping below](#1-pkgapi--project-ledgerentry-into-ledgeraction)).

```
gofmt -l pkg/explain/                     → empty
go vet ./pkg/explain/                     → clean
go build ./...                            → clean
go test -race -count=1 ./pkg/explain/...  → ok, 92.7% statement coverage
go test -race -short ./...                → ok (whole repo, 32 packages)
go.mod / go.sum                           → unchanged; nothing outside pkg/explain touched
```

73 test functions + 3 fuzz targets, 126 subtests. Fuzzing run locally:
~6.0M execs `FuzzWhyCostInvariants`, ~7.3M `FuzzParseID`, ~8.1M `FuzzMoney`.

---

## The invariant, and why it is not vacuous

```
sum(Terms) + Residual == Delta        exactly, always
sum(Term.Of)          == Term.Micro   exactly, for every term with children
```

Every amount is a `Micro` — an int64 count of millionths of a dollar. Integer
addition is associative and exact, so the sum is order-independent *by
construction* rather than by luck. `Residual` is computed last, as `Delta`
minus the terms, and is reported as residual. Nothing in the package is
allowed to make the identity true by adjusting a term; `Attribution.check()`
re-verifies it on the finished payload and returns an error rather than ship a
wrong audit record.

Computing the residual as the remainder does make the *first* identity true by
definition, which on its own would be a decomposition that can never be wrong.
So the property that carries the weight is the second one, asserted in
`FuzzWhyCostInvariants`:

> when the supplied composition fully prices the observed cost (a **closed
> basis**), the residual must be at most **2 µUSD/h** — quantization only.

Three of the four terms are quantized once from float share arithmetic, each
losing under half a µUSD; anything above 2 is a modelling error, not rounding.
That is the assertion that would have caught a genuinely incomplete
decomposition — and it did, on the fuzzer's first run (see
[Bugs found by tests](#bugs-found-by-tests)).

The complementary direction is `TestResidualIsReportedNotAbsorbed`: the
composition says the fleet costs $1.00/h at both edges, the meter says the
bill doubled, and the whole unexplained dollar shows up as residual with every
term at zero. A decomposition that quietly enlarged its biggest term instead
would be a lie with a number attached.

| Invariant | Enforced in | Tested by |
|---|---|---|
| `sum(terms) + residual == ΔCost`, exactly | `WhyCost`, `check` | `checkSums` (called by every attribution test), **`FuzzWhyCostInvariants`** |
| `sum(Of) == parent`, exactly, one level deep | `attributeNodeCount`, `check` | `checkSums`, `TestCheckCatchesEveryWayTheInvariantCanBreak`, **`FuzzWhyCostInvariants`** |
| A closed basis leaves only quantization | the term algebra | **`FuzzWhyCostInvariants`** (≤ 2 µUSD/h), `TestWhyCostWorkedExample` (exactly 0) |
| Unexplained cost is reported, never absorbed | residual is the remainder | `TestResidualIsReportedNotAbsorbed` |
| No term, sub-term, residual or driver ships uncited | `check`, `push` | `checkCitable`, `TestEveryDriverIsGrounded`, **`FuzzWhyCostInvariants`** |
| Every emitted citation resolves against the store | `Resolver` | `TestCitationsResolveAgainstTheStore`, `TestVerifyFailsAgainstAnEmptyStore`, `TestAttributionVerifyChecksSubTermsToo` |
| The answer never depends on input order | fixed-point money, `sumSorted`, canonical key order | `TestShuffleIsIdentical` (64 shuffles), `TestExplainIsDeterministic`, **`FuzzWhyCostInvariants`** (reversal), `TestSumSortedIsOrderIndependent` |
| The documented attribution order is the one used | `chainOrder` | `TestAttributionOrderIsTheDocumentedOne`, `TestSubAttributionOrderTakesFactsBeforeInferences` |
| Money arithmetic errors instead of saturating | `add`/`sub`/`mul`/`sumMicro` | `TestMoneyArithmeticRefusesToSaturate`, `TestExtremePricesRejectedNotSaturated`, **`FuzzMoney`** |
| Ids are an exactly invertible language | `escSeg`/`unescSeg`/`Parse` | `TestIDRoundTrip`, `TestIDSurvivesHostileNames`, `TestParseRejects`, **`FuzzParseID`** |
| Nothing is dropped silently | `EvidenceTruncated`, `Ungrounded`, `Notes` | `TestEvidenceIDsAreCapped`, `TestUngroundedDriversAreDroppedAndCounted` |
| No `time.Now()` anywhere; the window is an argument | — | every fixture is built from the fixed `t0`; a zero `From`/`To` is a validation error |

## Attribution order — the decision, stated once

```
node-count → spot-ratio → instance-mix → pricing-catalog
```

Node count, instance mix, spot ratio and unit prices overlap: buying five more
nodes *of a cheaper type* is simultaneously a volume and a mix change, and
whichever factor moves first collects the interaction. There is no order-free
answer, only a chosen one. The full argument lives in the package doc
(`explain.go`); the short form:

- **node-count first**, at the window's **starting** mix and prices — it is
  the one term an operator can verify independently, against a node count they
  already remember and a reference price printed in the term's facts.
- **spot-ratio before instance-mix** — capacity type is a *policy* someone
  chose; instance mix is largely a *consequence* the autoscaler produced.
  Giving the interaction to the consequence keeps the policy term equal to
  "what flipping that policy would have cost at the old fleet shape".
- **pricing-catalog last**, weighted at **end-of-window** node counts (Paasche)
  — a price change you did not cause belongs against the fleet you run now. A
  group that appeared mid-window borrows the other edge's price, so "catalog"
  always means *prices moved*, never *the fleet moved*.

`TestAttributionOrderIsTheDocumentedOne` pins it with a fixture where the
alternative convention gives a different, also-defensible number ($0.20/h vs
$0.16/h), and refuses to pass if the two ever coincide — a fixture that cannot
tell the orders apart proves nothing. `Attribution.Order` echoes the
convention into every stored payload, so an answer states the convention it
was computed under.

**Within node-count**, the order is `kilter-action → {workload-set,
workload-scaling} → unattributed`: Kilter's node actions are *counted* from
the ledger, the demand split is a *proportional inference*, and facts come out
before inferences.

### Why workload-set and kilter-action are children, not siblings

The design lists six additive terms. Emitted as siblings, two of them
double-count: a new namespace's nodes are *already inside* the node-count
term, and so are the nodes Kilter deleted. Neither is a factor of the price
identity `Cost = Σ nodes × price` — both are *drivers of* the node count. They
are therefore sub-attributions in `Term.Of`, with their own exact invariant and
their own explicit `unattributed` remainder. All six named terms appear, each
grounded, each additive within its level, with a documented parent. Nesting is
exactly one level deep and `check()` rejects anything deeper.

### The one number deliberately not attributed

`kilter-action` carries a fact, `observedAcrossActionWindowsUSDPerHour`: what
the meter actually did across the action's execution window. It is never the
term's value. Correlating a cost move with whatever ran at the same time is
the classic attribution lie (§10's cautionary tale: a deploy blamed, the real
cause a 24h cron). The term's value is the number that *is* attributable —
nodes the ledger records Kilter as having removed, at a stated reference price
— and the fact ships with its own caveat string.
`TestObservedActionWindowIsContextNotAttribution` fails if the fixture cannot
tell the two apart.

## Guards that refuse rather than fabricate

- **Demand-split cancellation guard.** When added and surviving demand move in
  opposite directions their net can be near zero while the parts are huge;
  dividing by it turns rounding noise into a confident enormous attribution.
  Above `|δ_added| + |δ_surviving| > 4 × |net|` the split is refused, the whole
  remainder becomes `unattributed`, and a note says so
  (`TestWorkloadSplitRefusedOnCancellation`).
- **Dominant dimension.** Node count is driven by whichever of CPU or memory
  binds, and millicores cannot be added to bytes. The split uses the dimension
  with the larger *relative* total move, ties to CPU — a documented rule, not a
  per-cluster heuristic — and reports its choice as the `demandDimension` fact.
- **Money range.** Prices, node counts, group counts and namespace counts are
  bounded, and `add`/`sub`/`mul` return `ErrRange` rather than saturating.
- **Two observations minimum.** ΔCost is a measurement; one timeline point is
  not a change.

## Units

The decomposition is over the **hourly run rate**, not integrated spend. The
timeline stores a rate, a rate is what a fleet change moves, and integrating it
would silently assume the timeline has no gaps. Monthly figures are derived per
term (×730) for readability and never summed. Decomposing integrated spend is
[deferred](#deliberately-deferred-with-reasons).

---

## Bugs found by tests

### 1. An *empty* fleet was mistaken for a *missing* fleet (found by `FuzzWhyCostInvariants`, first run)

`CostBasis.present()` was `b != nil && len(b.Groups) > 0`, which conflates two
different claims: "no composition was supplied" and "the fleet was empty at
this edge" — the latter being exactly what a cluster created inside the window
looks like.

Measured repro (now `TestEmptyBasisIsSuppliedNotMissing`, and kept as the
regression seed `testdata/fuzz/FuzzWhyCostInvariants/a4afbbfd86330cfe`): a
cluster starting empty and ending at 48 × m5.large @ $0.48 produced

```
node-count   +$0.000000/h   (measured against an "unknown" fleet)
residual    +$23.040000/h   ← the entire cost of the cluster
```

The whole $23.04/h — a fully explainable "you created 48 nodes" — was dumped
into the residual, and a note claimed no composition had been supplied. Worse
than a wrong number: a *silently degraded* answer that looks like honest
uncertainty.

**Fix** (`whycost.go`): `supplied()` is `b != nil`. nil means unsupplied; an
empty group list is a claim, and one the `a.nodes == 0` branch already knew how
to explain in full. **Tests:** `TestEmptyBasisIsSuppliedNotMissing` (which also
asserts the *reverse* is not conflated: a nil basis still degrades and says so),
`FuzzWhyCostInvariants`.

### 2. Container explanations could not see their own workload's deploys

Found by `TestExplainRefusalGetsChangeEvidence` while writing it. Deploys and
HPA actions are recorded against the **workload** subject; rightsizing
explanations are built for the **container** subject. A `post-change-soak`
refusal — literally "a deploy happened recently" — therefore had no deploy to
cite, and the ungrounded-driver rule would have dropped the sentence that
explains the refusal.

**Fix** (`payload.go`): `parentWorkload` maps a container subject to its owning
workload, and `mergeEvents` folds the parent's change events in under their own
bound (at most `MaxEvents` extra, newest first, total order `(At desc, id asc)`).
**Tests:** `TestExplainRefusalGetsChangeEvidence`, `TestParentWorkload`,
`TestMergeEventsIsBoundedAndOrdered`.

### 3. Terms priced a brand-new lifecycle at zero

Not a wrong total — the identity holds for *any* finite value of the missing
average, because it cancels between the spot and mix terms — but a badly wrong
*story*. With no spot nodes at the window start, `qA[spot]` was 0, so the
spot-ratio term was charged the full on-demand price of every migrated node
(−$0.40/h) and instance-mix was handed the offsetting "spot nodes cost
something" (+$0.16/h). Neither number described anything a human did.

**Fix** (`decomposeComposition`): a lifecycle absent at one edge borrows the
other edge's within-lifecycle average, still at start prices. The worked
example now reads spot-ratio −$0.24/h and instance-mix $0.00 — which is what
actually happened. **Tests:** `TestWhyCostWorkedExample` pins both numbers;
`TestSumInvariantAcrossShapes/whole fleet swaps type and lifecycle` covers the
degenerate case.

### 4. `mul` used the unsound overflow idiom

`p := m * n; if p/n != m` computes the wrapped product before checking it,
which is undefined-in-practice at the int64 extremes (`MinInt64 * -1` divides
back to itself). For arithmetic that is load-bearing for an audit claim, "the
check usually works" is not a property. **Fix:** the bound is checked by
division *before* multiplying. **Test:**
`TestMoneyArithmeticRefusesToSaturate` (including the `MinInt64, -1` case),
**`FuzzMoney`**.

### 5. `FuzzMoney`'s own tolerance was wrong (test-only)

Recorded because the corpus entries look like crashers and are not.
Round-half-away-from-zero bounds the round-trip drift at exactly 5e-7 USD, and
inputs landing precisely on the tie (`-21.7078125` → `-21707812.5` µUSD) hit
that bound *plus* the one ulp that dividing by 1e6 costs on the way back. The
assertion now scales its slack with magnitude and says why. Production
behaviour was correct throughout; the two seeds are kept because the exact-tie
boundary is worth re-running.

---

## What is here

| File | Contents |
|---|---|
| `explain.go` | Package doc: the invariant, the attribution order and its rationale, the children-not-siblings argument, units, dependency direction |
| `money.go` | `Micro` fixed-point µUSD, range-checked arithmetic, `sumSorted` |
| `id.go` | The `ID` citation scheme, exact escaping/parsing, `Resolver`, per-term id caps |
| `whycost.go` | `Input`/`CostBasis`/`LedgerAction`, the four-term chain, node-count sub-attribution, guards, `Attribution` + `check` |
| `payload.go` | `BuildExplain`, `Driver` grounding rules, `Verify` publish gates |
| `prose.go` | Deterministic template rendering (§5.9's no-model path) |
| `testdata/golden/` | Frozen JSON + prose for the worked example and the explain payload (`-update` to regenerate) |

---

## Exact wiring a later unit must do

Everything below is **out of scope for this unit by instruction** and touches
files this unit did not.

### 1. `pkg/api` — project `LedgerEntry` into `LedgerAction`

`pkg/explain` must not import `pkg/api` (dependency direction). The API layer
builds the projection:

```go
func toAction(cluster string, e api.LedgerEntry) explain.LedgerAction {
    a := explain.LedgerAction{
        At:                  e.At,
        Cluster:             cluster,               // ledger entries carry it too; prefer e.Cluster
        Fingerprint:         e.Fingerprint,
        Mode:                e.Mode,
        Risk:                e.Risk,
        Applied:             e.Mode == "apply" && e.Done > 0,
        CostBeforeHourlyUSD: e.CostBeforeHourlyUSD,
        ProjectedHourlyUSD:  e.ProjectedHourlyUSD,
    }
    for _, s := range e.Steps {
        if s.Status != actuate.StatusDone {   // NOT StatusDryRun: a preview moved nothing
            continue
        }
        switch s.Step.Type {
        case plan.StepDeleteNode:
            a.NodesRemoved++
        case plan.StepResizeWorkload:
            a.Resizes++
        }
    }
    return a
}
```

- `Finished` has no ledger field today. Either add `Finished time.Time` to
  `LedgerEntry` (it is already in `actuate.Report`) or leave it zero — a zero
  `Finished` falls back to `At`, which `TestZeroFinishedTimeFallsBackToStart`
  covers.
- `NodesAdded` stays 0 until a plan type that provisions nodes exists.
- **`Applied` must be exact.** Counting a dry-run would attribute money to a
  plan that moved none; `TestDryRunActionsAreNotAttributed` is the assertion,
  but it can only test what this side is given.

### 2. `pkg/api` — build the `CostBasis` pair for a window

`Input.Timeline` is `evidence.Store.Timeline(cluster, from, to)` verbatim (its
event overlay is picked up automatically as term evidence). The compositions
come from the cluster snapshots at the window edges:

```go
func basisFrom(snap *model.ClusterSnapshot, cat *pricing.Catalog) *explain.CostBasis {
    b := &explain.CostBasis{At: snap.Timestamp}
    type key struct{ t string; spot bool }
    agg := map[key]*explain.NodeGroup{}
    for i := range snap.Nodes {
        n := &snap.Nodes[i]
        if n.IsFargate() {           // billed per quantized pod, never per node shape
            continue
        }
        hourly, _ := cat.NodeHourlyCost(n)
        k := key{n.InstanceType, n.Spot}
        g := agg[k]
        if g == nil {
            g = &explain.NodeGroup{InstanceType: n.InstanceType, Spot: n.Spot, UnitUSDPerHour: hourly}
            agg[k] = g
        }
        g.Nodes++
    }
    // ... append in any order: duplicate rows merge by node-weighted average
    //     (TestSplitGroupRowsMergeNotDouble), and the package sorts internally.
    // Namespaces: sum clampedRequests(pod) per pod.Namespace.
    return b
}
```

Three things the wiring must get right, each with a test on this side already
waiting for it:

1. **Fargate nodes must be excluded** from `Groups` — they are not shareable
   machines and pricing them per node shape would inflate the fleet total. The
   resulting gap between modelled and observed cost lands in the residual with
   an explanatory note, which is the correct behaviour but a worse answer than
   pricing Fargate separately (see [deferred](#deliberately-deferred-with-reasons)).
2. **An empty fleet is `&CostBasis{At: t}`, not `nil`.** See bug 1. `nil` means
   "I could not determine the composition".
3. **Namespace demand is *requested* capacity**, not usage — requests are what
   force node count. The existing definition to mirror is `clampedRequests` in `pkg/plan/plan.go` (unexported; either export it or duplicate its clamping rules — do not invent a third).

### 3. `cmd/` — `kilter why-cost` and `kilter explain`

```
kilter why-cost --cluster <id> --from <RFC3339> --to <RFC3339> [--json]
kilter explain  --cluster <id> --workload <Kind/ns/name> --container <c>
                [--from --to] [--json]
```

- `--from`/`--to` are **required** for `why-cost` (no default window: the
  window is an argument, and a wall-clock default would make stored answers
  unreplayable). `explain` may default to a trailing window, but the CLI must
  resolve it to concrete timestamps *before* calling and echo them in the
  output.
- Human output is `Attribution.Prose()` / `Explanation.Prose()` verbatim.
  `--json` is the payload. There is no third rendering.
- Exit non-zero on error; a decomposition that cannot be computed is an error,
  not an empty table.

### 4. `pkg/api` — routes

```
GET /api/v1/clusters/{id}/why-cost?from=&to=      → *explain.Attribution
GET /api/v1/clusters/{id}/explain?subject=        → *explain.Explanation
```

**Call `Verify(Resolver{Store: mem, Actions: actions})` before serving.** It is
the publish gate for §5.7's "citations must resolve to ids the session actually
fetched": an answer with a dangling citation must 500, not render. Both
`Verify` methods exist and are tested; nothing else enforces this.

### 5. `pkg/reason` / `pkg/mcp` (units 6–7)

Expose `get_recommendation_explain` and `why_cost` as read-only tools
returning these payloads. The narrating model may quote `Term.Label`,
`Term.Facts`, `Driver.Detail` and the µUSD amounts, and must cite `Term.Evidence`
/ `Explanation.Citations`. Reject any answer whose citations are not a subset of
the payload's — `Attribution.Citations()` returns exactly that set.

---

## Deliberately deferred, with reasons

- **Integrated spend (`$ over the window`) rather than run-rate.** Would
  require assuming the timeline has no gaps, and `evidence` explicitly evicts
  under budget pressure. Doing it honestly means a gap-aware integrator with
  its own coverage term — a unit of work, not a field. The run-rate answer is
  exact and is what a fleet change actually moves.
- **Fargate, EBS, Lambda and other non-node cost in the composition.** They
  are priced by other packages with other shapes; today they land in the
  residual with a note naming the gap, which is honest but coarse. The natural
  extension is a second `CostBasis` dimension (`Charges []Charge`) decomposed
  by the same chain — additive, so the invariant is unaffected.
- **Per-workload attribution.** The split stops at namespace granularity
  because that is what `NamespaceDemand` and the deploy-event subjects support
  without a pod→node map. Going finer needs placement data the substrate does
  not keep.
- **`whatif` counterfactuals.** §4.5's other half, and explicitly unit 5.
- **Prometheus metrics.** Wiring a registry means touching `pkg/api`.
- **A `Timeline`-driven multi-segment decomposition** (attributing each
  consecutive pair of points and summing). It would localise causes in time,
  but every intermediate point needs its own composition, which no store keeps
  today. The two-edge decomposition is what the available evidence supports.
