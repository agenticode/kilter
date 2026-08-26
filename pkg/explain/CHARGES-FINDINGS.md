# pkg/explain — the `Charges` dimension (why-cost, second dimension)

`cmd/WIRING-FINDINGS.md` §6.4 bullet 3: *"`why-cost` prices Fargate into the
residual… correct and coarse; the natural extension is a second `CostBasis`
dimension (`Charges []Charge`) decomposed by the same chain."* This is that
extension.

```
gofmt -l pkg/explain/ pkg/pricing/      → empty
go vet ./...                            → clean
go build ./...                          → clean
go test -race -count=1 ./pkg/explain/... ./pkg/pricing/...  → ok, 91.8% statements in pkg/explain
go test -race -short ./...              → ok (whole repo)
go.mod / go.sum                         → unchanged
```

19 new test functions + 1 new fuzz target. Fuzzing run locally: **6.0 M execs
`FuzzChargeInvariants`** clean, plus **3.1 M execs `FuzzWhyCostInvariants`**
re-run to prove the existing property still holds. No existing test was
edited, weakened or deleted; `testdata/golden/whycost_base.json` and
`whycost_base_prose.txt` are byte-identical (every new payload field is
`omitempty`, and no note fires on an input with no charges).

`pkg/pricing` was **not touched.** The rule was "a pricing-side accessor only
if the decomposition genuinely needs one"; it does not. Everything the wiring
below needs — `Catalog.FargateRates`, `FargateRates.Cost`,
`FargateConfig.String`, `ClusterCost.Fargate` — is already exported, and
`pkg/explain` must not import `pkg/pricing` anyway (money.go duplicates
`HoursPerMonth` rather than take that dependency).

---

## What the dimension is

```go
type Charge struct {
    Kind           string   // "fargate", "ebs", … — a *policy* grouping
    Class          string   // "1vCPU/2GB", "gp3", … — a *shape* inside the kind
    Units          int64    // pods, GiB, invocations
    UnitUSDPerHour float64  // the price of ONE unit
    Evidence       []ID     // required, and must parse
}

type CostBasis struct {
    …
    Charges      []Charge
    ChargesKnown bool
}
```

`Charge` is deliberately the same shape as `NodeGroup` — a count and a unit
price — because that is what lets the *same chain* decompose it. `Cost = Σ u·r`
is the identity on both sides; only the names of the two grouping dimensions
differ (lifecycle/instance-type there, kind/class here).

**`ChargesKnown` is a flag, not a nil check.** This is bug 1 of the original
unit applied to the second dimension: an empty slice has to be able to mean
"we looked, there were none", which is a different and more useful claim than
"we did not look". Charges are attributed **only when both edges set it**. A
dimension stated at one edge alone is refused with a note and the whole move
stays in the residual — reading the silent edge as zero would report a
cluster's entire Fargate bill as having appeared out of nothing, which is
exactly the large, confident, wrong number this package exists to not print.
`TestChargesAtOneEdgeAreRefusedNotZeroed` pins it, and the fuzzer asserts that
a half-known dimension leaves no trace in any payload field.

## The decomposition chain and its order

```
charge-volume → charge-kind-mix → charge-class-mix → charge-rate
```

echoed into every payload as `Attribution.ChargeOrder`, and into the prose:

```
Attribution order: node-count → spot-ratio → instance-mix → pricing-catalog → charges
Charge attribution order: charge-volume → charge-kind-mix → charge-class-mix → charge-rate
```

- **`charge-volume` first**, at the window's **starting** mix and **starting**
  rates. It is the one charge term an operator can verify independently: the
  unit count (Fargate pods) is a number they already know, and the reference
  per-unit rate is printed as the `referenceUnitUSDPerHour` fact.
- **`charge-kind-mix` before `charge-class-mix`.** A charge *kind* is a
  placement policy someone chose ("run this on Fargate"). A charge *class* is
  largely a consequence — which configuration a pod lands on is AWS's rounding
  rule applied to its requests (`pkg/pricing` §4.1). Giving the interaction to
  the consequence keeps the policy term equal to "what moving that workload
  off Fargate would have cost at the old shape mix".
  `TestChargeKindMixIsThePolicyTerm` pins it.
- **`charge-rate` last**, weighted at end-of-window units (Paasche). A rate
  change you did not cause belongs against the units you actually run now. A
  line that appears mid-window borrows the other edge's rate, so "rates rose"
  can never mean "you bought something" (`TestChargeRateMeansRatesMoved`), and
  a supplied row with **zero units** states a rate nobody was paying and gets
  no rate term (`TestZeroUnitChargeLineHasNoObservedRate`) — the same rule
  `priceAt` applies to node groups.

### What a different order would have said

`TestChargeAttributionOrderIsTheDocumentedOne` pins the choice against the
equally defensible alternative, on a fixture where the two disagree:

| | reference rate | `charge-volume` |
|---|---|---|
| **chosen: volume at the START class mix** | `$0.200000 / 10 units` = **$0.020000**/unit-h | 2 × $0.020000 = **+$0.040000/h** |
| alternative: volume at the END class mix, start rates | `(4×$0.0125 + 8×$0.05) / 12` = $0.037500/unit-h | 2 × $0.037500 = **+$0.075000/h** |

Nearly a 2× difference on the same input, and both are defensible — which is
precisely why the convention is asserted rather than assumed. The test refuses
to pass if the two conventions ever coincide, because a fixture that cannot
tell them apart proves nothing.

## The exact arithmetic identity, and where it is enforced

```
sum(Terms) + Residual  ==  Delta                                  (unchanged)
charges.Micro          ==  ChargeDeltaMicro                       (new)
                       ==  Σ(end u·r) − Σ(start u·r), exact int64
sum(charges.Of)        ==  charges.Micro                          (new)
```

All three are re-verified on the finished payload in **`Attribution.check`** —
the first two by `checkCharges`, the third by the sub-term loop that already
existed. `WhyCost` returns an error rather than ship a payload that violates
any of them.

**The invariant got stronger, not weaker.** `sum(Terms)+Residual == Delta` is
untouched: the `charges` term is an ordinary member of `Attribution.Terms`, so
`Residual` is still computed **last**, as the remainder, and can still grow.
What is new is that the charges term is not free to be any number: it must
equal the exact repriced difference of the two supplied dimensions. Without
that clause the third identity would be vacuous — `charge-unattributed` would
dutifully absorb whatever the parent claimed, which is the failure mode this
whole package refuses.

`FuzzChargeInvariants` proves no rounding path can break either identity, and
adds the two assertions that carry the actual weight, since identities that
close by construction cannot fail on their own:

1. **`charge-unattributed` ≤ 2 µUSD/h, always.** `charge-volume`,
   `charge-kind-mix` and `charge-class-mix` are each quantized exactly once
   from float share arithmetic (< ½ µUSD each); `charge-rate` and the parent
   are exact integer arithmetic. Three half-µUSD errors cannot exceed 1.5, so
   anything above 2 is a modelling error, not rounding. This is the assertion
   that would catch a chain that failed to explain a real charge move.
2. **A closed model in *both* dimensions leaves only quantization in the
   residual** (≤ 2 µUSD/h) — the two decompositions compose, rather than one
   quietly eating the other's error.

Plus, every exec: charge terms are citable and every id parses; reversing
every input slice (charge rows included) is byte-identical; the emitted term
order equals the stated `Order`; a half-known dimension populates nothing.

### A bug the fuzzer found — in the test, on the first run

`FuzzChargeInvariants` failed in 0.05 s on `[]byte("0102020100000")`: a
"closed" case where only the *end* edge stated its charges. The code was
right — it refused the one-sided dimension and left $23.04/h in the residual,
which is the designed behaviour — and the *test* was wrong to call that input
closed. A model built on evidence the package is designed to decline is not a
complete model. The closed branch now states both edges; the asymmetric shape
is still generated and still asserted, in the open branch.

### Determinism

Float sums go through `sumSorted` over canonically-ordered slices
(`ckey` sorts by `(Kind, Class)`); no map is ever iterated into output.
`TestChargeShuffleIsIdentical` explodes every line into single-unit rows,
shuffles them 64 ways, and requires byte-identical JSON — which exercises the
duplicate-row merge rule at the same time. Duplicate `(kind, class)` rows
merge by **unit-weighted average rate**, mirroring `newComp`, because two rows
for one line is a collector detail and must not become two terms
(`TestDuplicateChargeRowsMergeNotDouble`).

## Citations: a `Charge` that cannot be cited cannot reach a term

- **`NewCharge` is the constructor and it refuses**: no evidence, or evidence
  that does not `Parse`, returns `ErrUncitableCharge` and a zero `Charge`.
- **A hand-built uncitable `Charge` fails the whole answer.**
  `CostBasis.validate` → `validateCharges` rejects it, so `WhyCost` returns an
  error. It is *not* dropped: silently dropping a line would understate the
  charge total and hand the difference to a term that did not earn it.
- **Rows without the flag are an error too**, not a silent skip — money that
  vanishes from both the terms and the operator's attention.
- `Evidence` is an exported field so a `CostBasis` still round-trips through
  JSON (the API layer builds these). The trade-off is deliberate and is the
  same one `Term.Evidence` already makes: enforcement lives at the boundary
  and in `check`, not in an unexported field that would break the wire format.

Charge terms cite **the lines that moved**, not merely something that
resolves: `charge-volume` cites lines whose unit count changed,
`charge-kind-mix` lines in kinds whose share moved, `charge-class-mix` lines
whose within-kind share moved, `charge-rate` lines whose rate moved plus any
`pricing-change` event. All are additionally anchored to the two timeline
points, which is the same standard the node terms hold themselves to and no
stronger.

**`Verify` needed no new code**, and that is most of the argument for making
the charge chain a sub-attribution rather than a parallel top-level list:
`Attribution.Verify`, `Citations()`, `Sum()`, `check()` and `Prose()` all walk
`Term.Of` already, so the new dimension inherits the publish gate instead of
duplicating it — and an invariant enforced in two places is an invariant
enforced in neither. `TestVerifyCoversChargeCitations` proves it is real by
breaking a citation on the parent and on a child.

## What still lands in the residual, and honestly why

- **Charges stated at one edge only.** Refused by design (above). The whole
  move stays in the residual under a note.
- **Any cost from a kind nobody supplied a line for.** The dimension can only
  decompose what it is given; a charge class the collector does not know about
  is invisible here and is in the residual by construction, exactly as before.
- **The gap between the modelled level and the meter.** The composition notes
  now compare *fleet + charges* against the observed cost, so supplying
  charges no longer produces a note claiming an unpriced gap that the charges
  dimension just priced. What remains genuinely unpriced still lands in the
  residual and still says so.
- **Node-side quantization**, ≤ 2 µUSD/h, as before.
- **Sub-µUSD disagreement with `pricing.ClusterCost`.** `SnapshotCost` sums a
  float per *pod*; a `Charge` is `Units × MicroFromUSD(rate)`. The two can
  differ by under 1 µUSD per line. That difference lands in the residual, not
  in a term.
- **Fargate pods AWS refused to price** (`ClusterCost.Warnings`, "left
  unpriced"). They are absent from `SnapshotCost.HourlyUSD` *and* must be
  absent from `Charges` — a `Charge` with units and a zero rate would claim
  they are free. Nothing is double-counted; the warning is the answer.

`TestResidualStillGrowsWhenChargesCannotExplainIt` is the test that keeps this
section honest: supplied charges are flat, the meter says the bill rose
$0.29/h anyway, and the assertion is that every charge term is zero and the
residual holds the whole amount. A decomposition that can only ever shrink the
residual is not measuring, it is asserting.

The complementary direction is `TestChargesShrinkTheResidualTheyUsedToBe`:
same cluster, same meter, run with and without the dimension supplied. It
asserts that the µUSD leaving the residual **equals** the charges term, and
that no node term moved to make room. A dollar may only leave the residual by
landing in a term that claims it.

---

## Exact wiring `cmd/kilter` must do

Out of scope for this unit by instruction. `cmd/kilter/explain.go`'s
`basisFrom` already skips Fargate nodes (`if n.IsFargate() { continue }`) with
a comment pointing here; it needs the second dimension filled in.

### 1. Build `Charges` from `pricing.SnapshotCost`

```go
func chargesFrom(snap *model.ClusterSnapshot, cat *pricing.Catalog, cite explain.ID) ([]explain.Charge, error) {
    cost := cat.SnapshotCost(snap)             // already includes Fargate pods
    rates := cat.FargateRates()
    type row struct{ units int64 }
    agg := map[pricing.FargateConfig]*row{}    // FargateConfig is comparable
    for _, p := range cost.Fargate {           // NOTE: snapshot order — do not
        r := agg[p.Config]                     // append straight to the slice
        if r == nil { r = &row{}; agg[p.Config] = r }
        r.units++
    }
    cfgs := make([]pricing.FargateConfig, 0, len(agg))
    for c := range agg { cfgs = append(cfgs, c) }
    sort.Slice(cfgs, func(i, j int) bool {     // map iteration must not reach output
        if cfgs[i].MilliCPU != cfgs[j].MilliCPU { return cfgs[i].MilliCPU < cfgs[j].MilliCPU }
        return cfgs[i].MemoryMiB < cfgs[j].MemoryMiB
    })
    out := make([]explain.Charge, 0, len(cfgs))
    for _, c := range cfgs {
        // c.String() renders "0.25vCPU 0.5GB" — a stable label, which is all
        // Class has to be; the chain never parses it.
        ch, err := explain.NewCharge(explain.ChargeKindFargate, c.String(),
            agg[c].units, rates.Cost(c), cite)   // NewCharge, never a literal
        if err != nil { return nil, err }
        out = append(out, ch)
    }
    return out, nil
}
```

and on the basis itself:

```go
b.Charges, err = chargesFrom(snap, cat, explain.TimelineID(cluster, pointAt(snap.Timestamp)))
b.ChargesKnown = true      // a snapshot always knows its pod set
```

Four things the wiring must get right:

1. **`ChargesKnown = true` at BOTH edges or neither.** Setting it at one edge
   is not a half-answer, it is a refused answer — you get a note and the old
   coarse residual. If one window edge has no usable snapshot, set it at
   neither.
2. **`ChargesKnown = true` even when there are no Fargate pods.** That is the
   claim "we looked, there were none", and it is what makes the charges term
   say `no non-node charges at either window edge` instead of vanishing. Rows
   without the flag are rejected outright.
3. **`cite` must resolve against the store the answer is served from.** The
   timeline point for the window edge is the minimum and is what the node
   terms already use; `Verify` will 500 on anything that does not re-serve.
   Once the substrate keeps per-pod Fargate evidence, cite that instead — the
   `Charge.Evidence` field is a slice for exactly that reason.
4. **Sort before emitting.** `cost.Fargate` is in snapshot order and the map
   above is a map. `pkg/explain` re-sorts internally, so a wrong order cannot
   change the *answer* — but it can change `cmd`'s own JSON, and §7's
   determinism contract is repo-wide.

### 2. Surface the dimension in `kilter why-cost`

- **Human output stays `Attribution.Prose()` verbatim.** The charges term and
  its chain already render through the existing `Term.Of` path, and the charge
  order line is already printed. There is nothing to add:

```
Between … the hourly cost of cluster "prod-eks-1" rose by +$0.290000/h (+$211.70/mo), from $1.200000 to $1.490000.
Attribution order: node-count → spot-ratio → instance-mix → pricing-catalog → charges (see package doc).
Charge attribution order: charge-volume → charge-kind-mix → charge-class-mix → charge-rate (see charges.go).
  charges            +$0.290000/h    +$211.70/mo  non-node charges (fargate) cost more [tl/… tl/…]
      of which charge-volume      +$0.040000/h     +$29.20/mo  2 more charged units at the window's starting average rate of $0.020000/unit-hour […]
      of which charge-class-mix   +$0.210000/h    +$153.30/mo  charged units shifted toward more expensive configurations […]
      of which charge-rate        +$0.040000/h     +$29.20/mo  charge rates rose for 1 line […]
  …
  residual           +$0.000000/h      +$0.00/mo  nothing unexplained [tl/… tl/…]
```

- **`--json` gains** `chargesKnown`, `chargeFromMicroUSDPerHour`,
  `chargeToMicroUSDPerHour`, `chargeDeltaMicroUSDPerHour` and `chargeOrder`,
  all `omitempty`, plus the `charges` entry in `terms`. A consumer that does
  not know about the dimension sees no change on a cluster without one.
- **Update §6.4 bullet 3** of `cmd/WIRING-FINDINGS.md`: it currently says
  Fargate is priced into the residual. After the wiring above it is priced
  into `charges`, and only what the collector could not see is residual.
- **Keep `countPricedNodes`.** `cmd/kilter/explain.go` already counts only
  node-backed nodes into the timeline point, so the existing "composition
  holds N nodes but the timeline holds M" note does not fire spuriously on a
  Fargate cluster. Nothing in this unit changes that, and nothing in the
  wiring above should: `Charges` accounts for Fargate *money*, not Fargate
  *nodes*, and the node dimension must keep excluding them.

### 3. `pkg/api`

`basisFrom` in §2 of `FINDINGS.md` grows the same two lines. Nothing else
changes: `Verify(Resolver{…})` already covers charge citations.

---

## Deliberately not built here

- **A `pkg/pricing` accessor.** Not needed (top of this file). The aggregation
  above is four lines of `cmd/` glue over already-exported API, and putting it
  in `pkg/pricing` would give that package an opinion about `pkg/explain`'s
  shape that it should not have.
- **EBS / Lambda / data-transfer kinds.** The type is open — `Kind` is any
  string and the chain treats it as a policy grouping, which
  `TestChargeKindMixIsThePolicyTerm` exercises with a second kind — but no
  collector produces them today, so wiring one would mean inventing the
  numbers.
- **Per-workload charge attribution.** A `Charge` has no namespace, so the
  charges chain has no analogue of `workload-set`/`workload-scaling`. Adding
  one needs a pod→charge map the substrate does not keep, which is the same
  wall `FINDINGS.md` records for per-workload node attribution.
- **A golden fixture for the charges payload.** The existing goldens are
  deliberately untouched so a diff there still means someone changed the node
  chain. A charges golden belongs with the `cmd/` wiring that will render it.
