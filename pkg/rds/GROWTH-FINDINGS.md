# T4 — allocated-storage growth: a refusal that finally has a size

`pkg/rds/FINDINGS.md` §7.4 deferred trend detection over the allocated-storage
ratchet and named its seam — `Target.PriorAllocatedStorageGiB` plus
`AttributeStorageGrowth`. `cmd/RDSLIVE-FINDINGS.md` §5.2 independently listed
`AttributeStorageGrowth` as unreachable, needing "cross-run checkpoint
persistence `kilter domains` has not got". Both were describing the same hole.
This unit fills it: `growth.go` + `growth_test.go`, **979 / 943 lines, 19 tests
including one fuzz target, 5 new refusal codes and 1 advisory code.**

Green under `gofmt -l ./pkg/rds`, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/rds/...` and `go test -race -short ./...`
(36 packages, all `ok`). `go.mod` and `go.sum` are untouched; `growth.go`
imports `fmt`, `sort`, `time` and `pkg/domain`. No existing test was edited.

**What was NOT touched, deliberately:** `actuate*.go` and `parity*.go` are
byte-identical. This unit adds no mutating identifier, no seam an actuator
could be reached through, and no argument for making one reachable. Storage
growth is an observation and its output is a refusal; if anything it *narrows*
the case for actuation, because the thing it measures is the one RDS quantity
that provably cannot be actuated in the direction anyone would want.

---

## 1. Why this is not a shrink proposal, structurally rather than by intent

U11's central finding is that `MaxAllocatedStorage` is a **ratchet, not
headroom**. Measuring how fast the ratchet turned is therefore not a step
toward turning it back, and this unit is built so that reading it as one takes
active effort:

| Guard | Where |
|---|---|
| The finding IS a refusal. A measured growth emits `storage-growth-is-not-reclaimable`, not an advisory with a refusal attached. | `AssessStorageGrowth` |
| No type here has a field matching `saving`, `reclaim`, `shrink`, `reduc` or `recover`. Asserted by reflection over four structs. | `TestGrowthNamesNoSaving` |
| The growth's dollar is `MonthlyUSD` on `GrowthMeasurement` — a **cost already being paid**, on the same footing as `StorageVerdict.UnusedMonthlyUSD`. There is no signed delta. | `GrowthMeasurement` |
| Nothing in `growth.go` writes `Assessment.Proposal`. The report's proposal count and savings totals stay 0 over arbitrary histories. | `FuzzGrowthNeverClaimsASaving`, `TestGrowthReachesTheReportAsARefusalAndNeverAsAProposal` |
| The refusal prose ends on AWS's own sentence, and a test rejects opportunity-shaped words in it. | `ratchetClause`, `TestGrowthIsARefusalWithASize` |

The existing refusals are unweakened and still fire beside it:
`allocated-storage-cannot-shrink` and `storage-autoscaling-ratchet` are
asserted on the same assessment that carries the growth finding.

## 2. The two bars, with the arithmetic

Both are refusals with their own codes, and they are **separate codes because
they have separate remedies**: one is fixed by checkpointing more often, the
other only by waiting.

### 2.1 `DefaultGrowthMinObservations = 4` → `storage-growth-insufficient-observations`

n observations bound **n−1 intervals**:

| n | intervals | what it can distinguish |
|---|---|---|
| 2 | 1 | nothing — a difference, not a trend |
| 3 | 2 | a second interval exists, but one anomalous interval is **half** the evidence and a 1–1 split has no tiebreak |
| 4 | 3 | 2-of-3: repeated behaviour outvotes a single anomaly |

4 is the smallest n at which "it did this more than once" beats "it did this
once". Cross-check, not derivation: it equals `AutoscaleMaxModificationsPer24h`
— AWS's documented ceiling of four storage modifications per 24 hours — so
below four observations a maximally active day cannot even in principle be
resolved into separate events.

Cross-check against the substrate: at `pkg/store`'s default 1 h cadence a
14-day span retains up to 336 snapshots, against a count cap of 768. So 4 never
binds on an hourly-checkpointing deployment; it binds exactly on a sparse one,
which is when it should. **[unverified: that `cmd/` will checkpoint at ingest
cadence rather than once per manual invocation — §6 is not written yet.]**

### 2.2 `DefaultGrowthMinSpan = 14 days` → `storage-growth-span-too-short`

A database's write volume has a **weekly** shape, and this package already
encodes that belief: `DefaultMinWindow = 72h` is documented as deliberately
*not* enough to contain a weekly batch job. One weekly cycle is one event, so
two is the minimum at which "it happened again" is a statement rather than an
assumption: **2 × 7 = 14 days**.

It coincides with `DefaultFullWindow`, the span at which this package already
attaches no window caveat to a metric verdict. That coincidence is wanted: a
growth finding never claims more confidence than the CPU and memory verdicts
printed on the same line.

What the two bars mean together, by checkpoint cadence:

| cadence | 4 observations at | 14-day span at | first verdict | which code holds last |
|---|---|---|---|---|
| hourly | +3 h | +14 d | day 14 | span |
| daily | +3 d | +14 d | day 14 | span |
| weekly | +21 d | +14 d | day 21 | count |
| monthly | +90 d | +14 d | day 90 | count |

`TestHistorySurvivesTheCheckpointLoopAndReachesAVerdict` walks the daily row
one day at a time and asserts which code is present on each.

### 2.3 `DefaultGrowthMinSteps = 2` → `storage-growth-rate-not-projectable`

This one gates the **rate only**, never the size. Storage autoscaling adds
discrete steps; one step is one event, and a per-day rate over one event
asserts a recurrence the evidence does not contain — the exact failure the
brief calls "a slope that predicts an absurd future". Two is the minimum at
which recurrence is *observed*.

The size is stated anyway, because the size is a measured fact about the past
and does not depend on this gate at all.
`TestSingleStepGrowthStatesTheSizeAndWithholdsTheRate` pins both halves: 200
GiB reported, no `Projection`.

### 2.4 `DefaultGrowthMaxGapFraction = 0.5` → `storage-growth-rate-not-projectable`

If the largest interval between two retained observations exceeds half the
span, most of the span is a single blind spot and the whole increase could have
landed inside it. At `MinObservations` evenly spaced the largest gap is
span/3 ≈ 33.3 %, so an evenly-sampled history never trips this; it trips
exactly when the retained instants are clustered.

A policy may make every bar **stricter and never looser** —
`GrowthPolicy.normalized()` clamps up, so a caller cannot configure its way to
a two-sample trend. `TestGrowthPolicyCannotBeLoosened`.

## 3. Every code added, and the test that pins it

| Code | Fires when | Test |
|---|---|---|
| `storage-growth-insufficient-observations` | 0 observations (`GrowthNoHistory`) or fewer than `MinObservations` (`GrowthTooFewObservations`) | `TestGrowthRefusesTooFewObservations` |
| `storage-growth-span-too-short` | enough observations, span below `MinSpan` | `TestGrowthRefusesTooShortASpan` |
| `storage-growth-history-inconsistent` | an allocation decreased, or one instant carries two allocations | `TestGrowthRefusesAnInconsistentHistory` |
| `storage-growth-is-not-reclaimable` | **the finding**: a measured, permanent increase, with its size | `TestGrowthIsARefusalWithASize` |
| `storage-growth-rate-not-projectable` | measured and sized; the rate is withheld (single step, or irregular sampling) | `TestSingleStepGrowthStatesTheSizeAndWithholdsTheRate`, `TestIrregularSamplingWithholdsTheRateAndIsReported` |
| `allocated-storage-grew` (advisory) | alongside the refusal, carrying the permanent monthly cost | `TestGrowthReachesTheReportAsARefusalAndNeverAsAProposal` |

`TestGrowthCodesAreDistinctFromEveryExistingCode` re-runs U11's
`TestReasonCodesAreDistinct` discipline across the union of the old 31 codes
and the new 6 — two codes sharing a value silently merge two findings.

**Two states share one code, on purpose.** `GrowthNoHistory` and
`GrowthTooFewObservations` both emit
`storage-growth-insufficient-observations`. The *code* is the refusal ("I will
not state a growth"); the *state* is the diagnosis ("your persistence is not
wired" vs. "your persistence works and is young"). Those are different
sentences to a human and the same decision to a caller filtering by code.

## 4. "Not enough history" is not "flat", and cannot be collapsed into it

This is the trap the unit is shaped around, and it is closed structurally
rather than by documentation:

- `GrowthVerdict.Measurement` is a **pointer**, `nil` on every refusal state.
  A short history carries no number, so there is no zero to misread as flat.
- `GrowthState.Measured()` is the only correct test, and
  `Measurement != nil ⟺ State.Measured()` is a biconditional pinned over a
  9-case table (`TestGrowthMeasurementExistsExactlyWhenMeasured`) and re-checked
  on every fuzz input.
- `GrewGiB()` and `Rate()` return `(value, ok)`. A caller that drops the
  boolean gets 0 — and had to drop it deliberately.
- The zero value `GrowthUnevaluated` is a **third** state: "the question was
  never put", which is what an excluded instance carries. Three states, no two
  reachable from one another by reading a number.

`TestNotEnoughHistoryIsNotFlat` asserts the three are pairwise distinct and
that only the measured one answers `GrewGiB`. This is the same rule the
substrate applies at 422 rather than 500: a request that cannot be answered is
not answered with a default.

## 5. Irregular sampling: reported, not smoothed

`pkg/store` retains at most one snapshot per cadence bucket and then prunes by
count and by bytes, so the retained instants are irregular **by construction**.
Nothing here divides by an observation count:

- **Span** is `Last − First` from the actual instants. `SpanDays` is that in
  days, and the projected `GiBPerDay` is `GrewGiB ÷ SpanDays`.
  `TestGrowthRateUsesTheActualInstants` builds a front-loaded history where
  growth÷count and growth÷span differ, and asserts the rate equals the second
  and **not** the first.
- **`GrowthSampling`** ships `MinGap`, `MaxGap`, `MeanGap` and
  `LargestGapFraction` on every verdict, refusals included. `MeanGap` is
  present precisely so the distance between it and `MinGap`/`MaxGap` is
  visible; no arithmetic reads it.
- **`Truncated`** says when the retention bound dropped older observations, so
  a reader knows `Span` is the retained history and not the instance's
  lifetime.
- The prose says it out loud: every rendered sampling line contains *"the
  instants are thinned by the store's retention policy and are not evenly
  spaced — gaps run X to Y against a Z mean, and the largest single gap is N%
  of the span"*.

Above `MaxGapFraction` the rate is withheld rather than caveated, because past
that point the number is arithmetic rather than evidence.

## 6. What `cmd/` must do — exactly two calls, plus the persistence

`cmd/RDSLIVE-FINDINGS.md` §5.2 is right that this needs cross-run persistence,
and right that `cmd/` cannot reach it alone today. What it needs turned out to
be smaller than a new store: the history is a field on `Target`, and
`Domain.Checkpoint`/`Restore` already serialize `Target` whole. **No new bucket,
no new key, no new codec.**

The one obstacle is that `Domain.Observe` *replaces* a target wholesale (it
must — the collector re-queries a window each tick and accumulating would
double-count), so a restored history would be discarded by the next collection.
Two additive helpers close that without touching `domain.go`:

```go
d, _ := krds.NewDomain(sc)                 // sc.Growth defaults; no flag needed
if blob, err := st.LoadRDSCheckpoint(scope); err == nil {
    _ = d.Restore(blob)                    // ← the missing read side
}

snap, err := collector.Collect(ctx)
rds.RecordStorageHistory(snap, d.StorageHistories(), sc.Growth)   // ← call 1
_ = d.Observe(snap)                                               // unchanged

blob, _ := d.Checkpoint()
_ = st.SaveRDSCheckpoint(scope, blob)      // ← the missing write side
```

`RecordStorageHistory` is idempotent — re-recording an instant replaces it
rather than doubling it, the same rule `SaveSnapshotAt` applies — so a retried
run does not manufacture a step. `StorageHistories` returns copies, so a caller
cannot reach into the domain through them. Both are asserted in
`TestHistorySurvivesTheCheckpointLoopAndReachesAVerdict`, which runs the full
Restore → Record → Observe → Checkpoint loop for twenty simulated days with two
autoscaling events in it, and never constructs a store.

Three things `cmd/` owns and this unit deliberately did not do:

1. **`SaveRDSCheckpoint`/`LoadRDSCheckpoint` do not exist.** `pkg/store` is out
   of scope here. `SaveEvidenceCheckpoint`/`LoadEvidenceCheckpoint` is the
   shape to copy — a scope-keyed, gzip-framed, version-refusing envelope — and
   `WIRING-FINDINGS.md` §6.2/§6.3 want the same write side, so the three jobs
   still share most of their work exactly as §5.2 predicted.
2. **The instant is the snapshot's**, falling back to `Window.End`. `cmd/`
   must set `Snapshot.Timestamp`; the collector already does. A snapshot with
   neither is skipped rather than stamped, because this package reads no clock
   and a history keyed by when the process happened to run is not replayable.
3. **No CLI surface.** There is no `--rds-growth` flag and no way to loosen the
   policy from a file, which is the `--rds-parity-rates` precedent inverted:
   provenance is the gate between a magnitude and a claim, and a two-sample
   trend is exactly the claim §7.4 deferred. A caller may pass a stricter
   `Config.Growth` in code.

Until that persistence lands, every modelled instance carries
`Assessment.Growth.State == GrowthNoHistory` with its reason filled, and **no
suppression** — see §7.

## 7. Statefulness: what was traded, and what it costs

§7.4's argument for a stateless package was "a stateless read-only report has
nothing to decay and nothing to forecast". That is now false by one field, and
the honest accounting:

**What stayed pure.** `AssessStorageGrowth` is a pure function: no clock, no
I/O, no package state. `TestNoClockReads`, `TestNoForeignImports` and
`TestNoUnexpectedPackageState` all still pass unmodified. The *sizer* is still
pure — the state is on the input, not in the package.

**What became stateful.** `Target.StorageHistory` is a growing slice that must
survive between runs for the finding to exist. Concretely:

- **Report size.** Each measured assessment gains a `GrowthVerdict`, including
  an echoed `GrowthPolicy` (~120 bytes/instance of duplication against
  `Report.Config`). Kept anyway, for the reason `Report.Config` is echoed at
  all: a verdict handed around alone must be able to state the bar it failed,
  and a UI needs "3 of 4" numerically, not in prose.
- **Checkpoint size.** Bounded at `DefaultGrowthMaxObservations = 768`
  observations per instance — `pkg/store`'s `MaxPerCluster`, stated
  independently because `pkg/rds` imports nothing. At ~40 bytes per observation
  that is ~30 KiB per instance worst case, against `pkg/store`'s 32 MiB
  per-cluster budget. A 1,000-instance account at the bound is ~30 MiB of
  checkpoint. **[unverified: no such account has been measured.]** A tighter
  `Config.Growth.MaxObservations` is the knob.
- **A new failure mode.** The finding now depends on the caller's persistence
  being correct. A checkpoint restored from another scope, or a history
  attached to a reused identifier, produces a decrease — which is why
  `storage-growth-history-inconsistent` refuses the whole history rather than
  repairing it. Data faults surface as refusals, not as confident numbers.
- **What did not decay.** Nothing here forecasts a workload or ages a
  confidence. The only forward-looking value is `GiBPerDay`, it carries
  `[unverified]` in its own `String()`, and it carries no money — see §8.

**What was reused rather than reinvented.** `AttributeStorageGrowth` is the
engine: it is called pairwise across the series, and its three outcomes drive
`Steps`, `LargestStepGiB` and the inconsistency refusal. §7.4's ledger rule
still has one implementation; this unit gave it a second arity, not a second
copy.

## 8. The rate is never money

The fourth trap, closed three ways:

1. `GrowthProjection` has **no** field matching `usd`, `cost`, `price`,
   `saving`, `dollar` or `monthly`. Reflection test.
2. `Claimable()` is a **method** returning `false` unconditionally — the
   `Advisory.Actuatable()` precedent — so no serialized form and no future
   struct literal can claim otherwise.
3. `GrowthProjection.String()` carries the literal `[unverified]` marker, so a
   caller formatting the number gets the marker whether it wanted it or not.

The only dollar the finding produces is `GrowthMeasurement.MonthlyUSD`: the
monthly cost of storage **already allocated**, priced by the same
`RateCard.StorageMonthlyUSD` that prices the floor, carrying the same
`RateProvenance`. It flows into `Assessment.WorstRateProvenance` and so into
`unverified-rate` exactly like every other RDS dollar, and into
`Report.StorageGrowth().UnrecoverableGrowthMonthlyUSD` — a field named after a
cost — and never into a savings column.
`Report.Validate` needed no change: it gates proposals, and this unit produces
none.

## 9. Things that would falsify parts of this unit

1. **If a real fleet's growth is dominated by single large steps**, `MinSteps`
   makes `storage-growth-rate-not-projectable` the common case and the rate is
   near-dead weight. That is the *intended* failure mode — the size still ships
   — but if it holds universally, the projection should be deleted rather than
   defended.
2. **If `cmd/` ends up checkpointing only on manual invocation**, a weekly
   operator waits 21 days for a first verdict (§2.2 table) and `MinObservations`
   binds where `MinSpan` was meant to. The fix is the cadence, not the bar.
3. **If AWS ever ships an API that reduces allocated storage**, §1's entire
   argument inverts, `storage-growth-is-not-reclaimable` becomes a false
   statement, and this file is the first thing to delete. Nothing about that is
   subtle, which is why the refusal is named after the claim it makes.
4. **If `MaxGapFraction = 0.5` proves too permissive** — a history with two
   clusters 45 %/55 % apart passes and shouldn't — the constant moves and
   nothing else does. It is read in exactly one place.
