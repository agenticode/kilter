# `pkg/confidence` — the differential first, the lift second

`pkg/rds/FINDINGS.md` §7.3 asks for this package, and describes the situation
as *"the three shipped domains each wrote their own"* confidence model. That
premise is wrong in a way that matters, and finding out how was most of this
unit.

**There are not three copies of one model. There are two models with two users
each.** `pkg/ec2` and `pkg/lambda` share an *additive* model — score starts at
zero and each factor adds `weight × earned`. `pkg/ecs` and
`pkg/domain/fargate` share a *multiplicative* one, `decision.Compose`, which
already lives in a shared package (`pkg/decision`) and reproduces the
Kubernetes recommender's historical formula by construction.

So the fourth copy §7.3 warns about is real, but it is the third copy of the
*additive* model. That is what this package holds. `pkg/ecs` is not touched and
must not be.

---

## 1. The factor-by-factor differential

`✅` = genuinely identical. `❌` = genuinely different.

### 1.1 Model shape

| | `pkg/ec2` | `pkg/ecs` | `pkg/lambda` | |
|---|---|---|---|---|
| Composition | `Σ wᵢ·eᵢ` | `∏ vᵢ^wᵢ` | `Σ wᵢ·eᵢ` | ❌ ecs |
| Public type | `Confidence{Score, Factors}` | bare `float64` | `Confidence{Score, Factors}` | ❌ ecs |
| Rounding | none | `Round(s·100)/100` | none | ❌ ecs |
| Zero evidence | 0 | 0 | 0 | ✅ |
| Weights sum to | 1 (asserted) | n/a (exponents, all 1) | 1 (asserted) | ❌ ecs |
| Reads a clock | no | **yes** (`now`, for freshness) | no | ❌ ecs |
| `weakestFactor` | yes | **absent** | yes | ❌ ecs |
| Term list retained | yes | **discarded** (`return c.Score`) | yes | ❌ ecs |

Between `ec2` and `lambda` the shape agrees on every row, and
`ConfidenceFactor` / `Confidence` were byte-identical declarations including
JSON tags. `weakestFactor` was byte-identical in both.

### 1.2 The accumulator (`add`)

| earned | `pkg/ec2` | `pkg/lambda` | |
|---|---|---|---|
| `< 0` | → 0 | → 0 | ✅ |
| `> 1` | → 1 | → 1 | ✅ |
| `-Inf` | → 0 | → 0 | ✅ |
| **`NaN`** | **passes through, poisons `Score`** | **→ 0** | ❌ |
| **`+Inf`** | **→ 1** | **→ 0** | ❌ |

`pkg/ec2` clamps by comparison alone; NaN satisfies neither `< 0` nor `> 1`.
`pkg/lambda` guards with `finite()` first.

### 1.3 The factors themselves

| Factor | ec2 | lambda | |
|---|---|---|---|
| `window` — earned | `w.Seconds()/min.Seconds()`, 0 if `min ≤ 0` | identical | ✅ |
| `window` — prose | `…against a %s minimum` | identical format | ✅ |
| `window` — prose rounding | `Round(time.Hour)` | `Round(time.Minute)` | ❌ |
| `window` — weight | 0.20 | 0.15 | ❌ |
| coverage | `sample-coverage`, 0.30, pass-through `obs.Coverage` | `report-coverage`, 0.25, pass-through `obs.ReportCoverage` | ❌ name/weight/prose, ✅ arithmetic |
| everything else | `memory-signal` 0.20, `metric-resolution` 0.15, `burst-evidence` 0.15 | `measured-points` 0.30, `warm-share` 0.15, `memory-headroom` 0.15 | ❌ disjoint |

---

## 2. The rulings

### 2.1 `pkg/ecs` stays where it is — DOMAIN FACT, not accidental

Not lifted, and `pkg/ecs/sizer.go` is unmodified. Four independent reasons, any
one of which is sufficient:

1. **The number would move.** A product is not a sum. ECS also rounds to two
   decimals inside `decision.Compose`; the additive model does not round at
   all. Every shipped ECS confidence value would change.
2. **The type would move.** `Assessment.Confidence` is `float64`
   (`pkg/ecs/sizer.go:187`), not a struct. Changing it violates the
   additive-only rule on exported signatures.
3. **ECS's lineage is the recommender, not EC2.** `decision.Compose`'s own doc
   says it reproduces the recommender's historical formula "by construction".
   `pkg/domain/fargate/recommend.go:423` composes the same way. Moving ECS to
   the additive model would break its agreement with the two things it is
   actually meant to agree with, to gain agreement with two things it was never
   meant to agree with.
4. **ECS has a factor EC2 and Lambda deliberately do not.** `TermFreshness`
   takes `now`. The additive model is pure over a closed observation window and
   reads no clock; adding freshness to it would make it clock-dependent, which
   is a different guarantee.

§7.3's own wording already knew this: it names the shape to copy as *"from
`pkg/ec2`/`pkg/lambda`"* and does not name `pkg/ecs`.

### 2.2 The `NaN` / `+Inf` divergence — ACCIDENTAL, and preserved anyway

`pkg/lambda` is right and `pkg/ec2` is older. A confidence factor that scores
NaN as evidence is a bug in waiting, and `finite()` is the fix.

**It is still not this unit's fix to make.** Flattening it would change
`pkg/ec2`'s output on non-finite input — which is exactly the failure mode this
unit exists to prevent, and it would be invisible in a diff that reads as a
refactor. So both spellings survive as
[`Confidence.Add`](confidence.go) (guarded) and `Confidence.AddBounded`
(comparison-only), and the equivalence proof pins each domain to the one it
shipped with.

`pkg/ec2` adopting `Add` is now a one-word diff in one file, and
`TestConfidenceKeepsItsNaNPropagation` is the test that will fail loudly when
someone makes it. That is the whole point: the divergence went from *invisible
and duplicated* to *visible and named*.

Reachability, for whoever makes that call: `obs.Coverage` is
`math.Min(1, float64(int)/float64(int))` behind an `ExpectedSamples > 0` guard
(`pkg/ec2/sizer.go:520`), so NaN is **[unverified] believed unreachable in
production today**. The proof does not depend on that belief — it pins the
behaviour over the whole `float64` domain either way.

### 2.3 The `window` prose rounding — DOMAIN FACT

`Round(time.Hour)` vs `Round(time.Minute)` is not a style difference. An EC2
minimum window is quoted in days (`DefaultConfig().MinWindow` = 7 days); a
Lambda log window is a slice of hours (24h).

Note carefully: **at both shipped defaults the two spellings print the same
string**, because 7 days and 24 h are both whole hours. The divergence only
bites when `MinWindow` is set to something that is not — and `MinWindow` is a
caller-settable `Config` field, so that is a supported configuration, not a
hypothetical. Under the EC2 spelling a 6h20m Lambda minimum prints as
`"6h0m0s"`: a wrong number, in a line whose entire job is to tell an operator
how much more window they need.

This is the case for the dense sweep rather than a handful of realistic
fixtures. A proof built only from default configs would have found the two
spellings interchangeable and flattened them. It survives as the `round`
parameter of `WindowFactor`, never as a constant.

### 2.4 The weights — DOMAIN FACT

Both budgets sum to 1 and each domain has a test asserting it, but `window` is
worth 0.20 to EC2 and 0.15 to Lambda, and no other factor name is even shared.
These are claims about what each domain's evidence is worth. They stay in their
domains. This package holds no weights.

---

## 3. What was lifted, and what was left behind

**Lifted** (`confidence.go`, 138 lines): `Factor`, `Confidence`, `Add`,
`AddBounded`, `WeakestFactor`, `FactorWindow`, `WindowFactor`.

`pkg/ec2` and `pkg/lambda` now alias the types rather than redeclare them, so
the two reports cannot drift apart in JSON. Net effect on the two domains:
**−47 lines**, and the `add`/`weakestFactor` bodies exist once.

**Left behind, deliberately:**

| Not lifted | Why |
|---|---|
| The coverage factor | The arithmetic is a pass-through — a lifted helper would be `func(x) x`. Only the name, weight and prose differ, and those are the domain's claims. Lifting it would move code without removing a decision. |
| `memory-signal`, `metric-resolution`, `burst-evidence`, `measured-points`, `warm-share`, `memory-headroom` | Disjoint. There is no second implementation to deduplicate. |
| The weight constants | §2.4. |
| Any `Score`-gating helper | `MinConfidence` comparison plus a refusal string is three lines and each domain words its refusal differently. |
| `pkg/ecs`'s model | §2.1. |

`WindowFactor` is the only *judgement* that moved, and it moved because both
domains computed it identically — including the detail below, which is the
reason it was worth moving at all rather than leaving as six duplicated lines.

**The division is `observed.Seconds() / minimum.Seconds()`, not
`float64(observed) / float64(minimum)`.** These are not the same number.
`Duration.Seconds()` is `float64(d/1e9) + float64(d%1e9)/1e9`, not a single
division, and the two disagree in the last bit. Measured, by mutating the
implementation and running the proof: for a 3h7m13.456789123s window against a
3h20m minimum, `Seconds()/Seconds()` gives `0.9361213990935833`
(`0x3fedf4b4dd462aa2`) and `float64/float64` gives `0.9361213990935834`
(`0x3fedf4b4dd462aa3`). One ULP — and a shipped confidence value. A
reimplementation from the doc comment would have gotten this wrong.

---

## 4. The equivalence proof

`pkg/ec2/confidence_equiv_test.go` and `pkg/lambda/confidence_equiv_test.go`
each carry the **pre-lift implementation verbatim** — the old struct, the old
`add`, the old `confidence` body, the old `weakestFactor` — and assert the
shipped path reproduces it over a dense input sweep.

**Compared by raw bits** (`math.Float64bits`), not by tolerance: `Score`, and
for every factor its `Name`, `Weight`, `Earned` and `Why`. A confidence that
moves in the last bit is still a confidence that moved.

| | inputs swept | combinations |
|---|---|---|
| `pkg/ec2` | 6 minimum windows × 8 windows × 11 coverages × 7 periods × 7 burst classes × memory-blind | **56,448** |
| `pkg/lambda` | 5 minimum windows × 7 windows × 9 coverages × 7 cold shares × 9 point sets × 4 current-indices × warm | **158,760** |

Coverage inputs include `NaN`, `±Inf`, `-0.0`, `MaxFloat64` and
`0.9999999999999999`; windows include a span whose `Seconds()` is not exactly
representable; burst classes include `""` and an unknown future class; Lambda's
point sets cover every branch of `measured-points` (0/1/2/3+) and of
`memory-headroom` (absent, zero-memory, at-ceiling, margins either side of the
0.25 saturation, and a negative margin).

**`weakestFactor` is pinned in the same assertion**, on every one of those
combinations — the string, not just the score. It is a reporting surface: an
operator reads it to know what to go measure, so a change in *which* factor is
named is a behaviour change even when the number is unchanged.
`TestWeakestFactorNamesTheLargestLossAndBreaksTiesByOrder` additionally pins
the tie-break (earliest factor wins, via strict `>`), which is what makes the
result a function of call order alone.

### 4.1 The proof was checked against mutants

A proof that cannot fail proves nothing. Each of these was applied to the
shipped code, confirmed to fail the proof, and reverted:

| Mutation | Caught by |
|---|---|
| `AddBounded` → `Add` in `pkg/ec2` (the "harmless cleanup") | `score: legacy NaN != lifted 0.5` |
| `Round(time.Hour)` → `Round(time.Minute)` in `pkg/ec2` | `factor 1 (window) why` |
| `Round(time.Minute)` → `Round(time.Hour)` in `pkg/lambda` | `factor 3 (window) why` |
| `Seconds()/Seconds()` → `float64/float64` in `WindowFactor` | `factor 1 (window) earned`, 1 ULP |
| `WeakestFactor` tie-break `>` → `>=` | `weakestFactor` string, both domains |

### 4.2 What the proof does *not* cover, stated plainly

Mutating `pkg/lambda`'s window factor from `Add` to `AddBounded` **does not**
fail the proof. That is not a gap in the sweep; it is a theorem, and
`TestWindowFactorIsAlwaysFiniteAndNonNegative` states it: a `time.Duration` is
an int64 of nanoseconds, so a positive minimum is at least 1e-9 s and any span
is at most ~9.2e9 s, making the quotient at most ~9.2e18 — large, finite, and
clamped to 1 by both spellings. The two clamps are provably interchangeable for
this one factor. Recorded here so the absence of a failing mutant reads as a
proof rather than as missing coverage.

Also not covered: the proof pins `(*Sizer).confidence` and `weakestFactor`. It
does not pin the *call sites* — that `a.Confidence.Score` still feeds
`Proposal.Confidence` and the `low-confidence` refusal. Those are covered by
the pre-existing `TestSuppressionLowConfidenceFiresAlone` in both packages,
which assert the refusal names `metric-resolution` (ec2) and `report-coverage`
(lambda) respectively. **No existing test was modified**; 37 packages pass
`go test -race -short ./...`.

### 4.3 The frozen copies

The legacy implementations in the two `_test.go` files are frozen and are not
maintained alongside the real code. When a deliberate change to a confidence
model lands, this proof's job is to **fail**; the new behaviour is then
re-pinned by editing the frozen copy in the same commit that changes the model,
so the diff shows the number moving. Deleting the copies to make the test pass
deletes the only thing standing between a refactor and a silent recalibration.

---

## 5. What `pkg/rds` should call

When U13 grows a confidence model, it writes **factors and weights only**. The
machinery is:

```go
import "github.com/agenticode/kilter/pkg/confidence"

type ConfidenceFactor = confidence.Factor
type Confidence = confidence.Confidence

func (s *Sizer) confidence(obs Observation) Confidence {
    var c Confidence
    c.Add("...", weightSomething, earned, why)   // Add, never AddBounded

    // RDS quotes a multi-day CloudWatch window: round the prose to the hour.
    windowEarned, windowWhy := confidence.WindowFactor(
        obs.Window.Duration(), s.cfg.MinWindow, obs.Window.String(), time.Hour)
    c.Add(confidence.FactorWindow, weightWindow, windowEarned, windowWhy)
    return c
}
```

and the refusal is
`fmt.Sprintf("... %s", confidence.WeakestFactor(a.Confidence))`, with
`Proposal.Confidence = a.Confidence.Score` — still a `float64`, unchanged in
type and meaning.

Four things `pkg/rds` must **not** do:

1. **Do not use `AddBounded`.** It exists only to hold `pkg/ec2` still. A new
   domain has no shipped numbers to preserve and should reject non-finite
   evidence.
2. **Do not copy `pkg/ec2`'s or `pkg/lambda`'s weights.** They are claims about
   CPU/memory metrics and Lambda REPORT lines. RDS's evidence is different —
   most obviously, `FreeableMemory` is `MemAvailable` (`pkg/rds/FINDINGS.md`
   §4), so a memory factor there means something else entirely and should
   probably be a refusal rather than a weight.
3. **Do not reach for `decision.Compose` as well.** Pick one model. A domain
   with both would produce two incomparable confidences for the same target.
4. **Do not add a `window` factor with a different name.** `FactorWindow` is a
   constant because operators and tests read it out of refusals.

Once RDS lands, the additive model has three users and one implementation —
which was the whole ask.

---

## 6. Left for someone else

- **`pkg/ec2` adopting `Add`.** §2.2. One word, one file, one test to re-pin.
  Deliberately not bundled into a refactor.
- **`pkg/ecs` keeping its `Basis`.** `pkg/ecs/sizer.go:646` calls
  `decision.Compose` and immediately discards everything but `.Score`, so its
  `low-confidence` refusal can only say *that* confidence was low, never
  *which* term cost it — the thing `WeakestFactor` gives the other two domains
  for free. `decision.Confidence.Basis` is already populated. A
  `decision.WeakestTerm` would be the multiplicative model's equivalent and
  belongs in `pkg/decision`, not here. Out of scope this wave.
- **Reconciling the two models.** Not obviously desirable. Recorded as a real
  fork with four reasons (§2.1) rather than as debt, so that anyone proposing
  to merge them has to argue against those four rather than discover them.
