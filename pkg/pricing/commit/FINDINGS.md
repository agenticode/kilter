# U4 — CommitmentLedger (`pkg/pricing/commit`)

Rightsizing an instance covered by a Reserved Instance or a Savings Plan can
*raise* net cost while the optimizer reports a saving. §4.4 ex.1 of
`docs/design/compute-domains.md` documents a verified **+135 %** case. Until a
commitment waterfall exists, every savings number Kilter prints is a list-price
fantasy. This package is the fix: a pure re-implementation of AWS's documented
commitment application order, so that every savings claim can be a `Bill()`
delta.

`gofmt`, `go vet ./...`, `go build ./...`, `go test -race -count=1
./pkg/pricing/...` and the full `go test -race -short ./...` are green. No
dependency was added; `go.mod` and `go.sum` are untouched. Nothing outside
`pkg/pricing/commit/` was modified.

---

## What is here

| File | Contents |
| --- | --- |
| `commit.go` | Package doc, `UsageLine`/`Usage`, `ReservedInstance`, `SavingsPlan`, `Inventory`, validation, JSON load/save, `Active(t)` |
| `normalize.go` | Normalization-factor table, bare-metal table, size-flexibility limitations, platform/tenancy folding |
| `waterfall.go` | `Bill(Usage) Cost` — the five-stage waterfall, `Cost`, `LineCoverage`, `CommitmentUse` |
| `netsavings.go` | `NetSavings(before, after) Assessment` — signed net, suppression with reason + `ValidFrom`, rate-availability harmonization |
| `sync.go` | `CommitmentSource` / `RateSource` seams (**defined, not implemented**), `RateTable` and its offline JSON |

**Surface**: five exported functions with behaviour (`Bill`, `NetSavings`,
`Active`, `NormalizationUnits`, `InstanceUnits`), plus data types, validation
and the two sync interfaces. No goroutines, no clock, no I/O beyond explicit
`Load*File`.

### The billing model, in one line

```
bill = Σ RI charges + Σ SP commitments + on-demand remainder
```

Commitments are charged **whether or not usage absorbs them**. That is not an
approximation, it is the entire point: freeing capacity nothing else absorbs
shows up as `Cost.StrandedUSD`, not as a saving. Every §4.4 example falls out of
this one modelling choice.

### Making `Bill()` the easy path

- `NetSavings` is the only supported way to evaluate a change and returns an
  `Assessment` — a struct, not a float, so it cannot be mistaken for a rate.
- `Assessment.ClaimableHourlyUSD()` / `ClaimableMonthlyUSD()` return **0** when
  the recommendation is suppressed or the net is not positive. Claiming is a
  method call that already knows about suppression; there is no field a caller
  can read that quietly skips it.
- The list price is reachable only as `Usage.OnDemandHourlyUSD()`, whose doc
  comment says subtracting two of them is the bug this package exists to
  prevent, and as `Assessment.GrossHourlyUSD`, which sits next to
  `NetHourlyUSD` in the same struct so the two are always compared.

---

## Invariants, and exactly where each is tested

| Invariant | Enforced in | Pinned by |
| --- | --- | --- |
| `HourlyUSD == RICommittedUSD + SPCommittedUSD + OnDemandUSD` | `waterfall.go` | `assertPartition` (called by every scenario test), `FuzzWaterfall` |
| Bill never below the committed floor | model shape | `assertPartition`, `FuzzWaterfall` |
| Per line: `RIQty + EC2SPQty + ComputeSPQty + OnDemandQty == Quantity` | `waterfall.go` | `assertPartition`, `FuzzWaterfall` |
| No commitment is over-allocated (per plan, in aggregate, and in normalization units) | `applyRI`, `applySPPool` | `FuzzWaterfall` (all three checks) |
| Output is a pure function of the input *multiset* — no map iteration, intrinsic sort keys only | `newStates`, `canonicalRIs`, `canonicalSPs`, `groupEC2SPs` | `TestBillIsIndependentOfInputOrder` (200 shuffles, `reflect.DeepEqual`, bit-identical totals), `TestNetSavingsIsIndependentOfInputOrder`, `FuzzWaterfall` determinism check |
| `Bill` does not mutate the inventory; concurrent callers agree | `waterfall.go` | `TestBillIsRaceFreeAcrossGoroutines` (under `-race`) |
| Garbage clamps rather than poisoning totals; `Validate` fails loudly | `sane()`, `Validate` | `TestBillClampsGarbageInsteadOfPoisoningTotals`, `TestLoadInventoryRejectsBadData` |
| Suppressed ⇒ claims exactly 0, and carries a reason code | `Assessment` | `FuzzNetSavings`, every stranding test |
| `ValidFrom` names a real commitment expiry | `newlyStrandedExpiry` | `FuzzNetSavings` |
| Conservative fallback under-claims, never over-claims | `applySPPool` + harmonization | `TestConservativeFallbackUnderClaims`, `…AcrossShapes`, `…HarmonizesRateAvailability` |
| `HoursPerMonth` matches the rest of Kilter | — | `TestHoursPerMonthMatchesPricing` |

### Money convention

`float64` USD per hour — the convention `pkg/pricing` already uses (`HourlyUSD`,
`FallbackCPUHourlyUSD`, `HoursPerMonth = 730`). Never `float32`. **Never
compared with `==`**: the package exports `Eps = 1e-9` and every internal
exhaustion test goes through it; every test assertion goes through `near(got,
want, tol, what)` and states its tolerance (`1e-9` for exact arithmetic, `5e-3`
for figures the AWS docs print rounded to the cent). `HoursPerMonth` is
duplicated rather than imported from `pkg/pricing`, so that package stays free
to depend on this one later without an import cycle;
`TestHoursPerMonthMatchesPricing` stops the duplicate drifting.

---

## AWS scenarios reproduced

All numbers below are the figures printed on the AWS pages, asserted as test
expectations — not values read back out of the implementation.

### `apply_ri.html` — `TestAWSDocApplyRIScenarios` (table-driven)

- **Scenario 1, single account** — 4 × m3.large zonal RIs cover the 4 m3.large
  instances; 4 × m4.large regional RIs (16 units) fully cover 2 × m4.xlarge
  (16 units); 1 × c4.large regional RI (4 units) covers **50 %** of one
  c4.xlarge (8 units), remainder on-demand. ✅
- **Scenario 2, normalization factor** — 1 × m3.2xlarge regional RI (16 units)
  covers both m3.large first (8 units, smallest-first), then one full
  m3.xlarge; the second m3.xlarge bills on-demand. ✅

### `sp-applying.html` — `TestAWSDocSavingsPlansScenarios` (table-driven)

The page's example hour is reproduced as a shared fixture; its documented
On-Demand total of **$59.10** is pinned separately by
`TestAWSDocSavingsPlansListPrice`.

| Scenario | Documented figures asserted | |
| --- | --- | --- |
| 1 — SP covers all usage | SP-priced usage $47.125 (the page prints its cent-rounded form, $47.13); $0 on-demand; $2.875 stranded | ✅ |
| 2 — SP covers some usage | ~2.9 r5 units covered, ~1.1 on-demand = **$1.14**; rest **$55.10**; total on-demand **$56.24** | ✅ |
| 3 — across products | commitment **$19.60** exactly met (r5 30 % → Fargate GB → Fargate vCPU, memory before compute on the lower SP rate); remainder **$32.70** | ✅ |
| 4 — SP after RIs | RI covers 2 r5.4xlarge *first*; SP then exhausts **$18.20**; remainder **$32.70** | ✅ |
| 5 — multiple SPs | EC2 Instance SP (r5/us-east-1) consumes **$2.40** of $3.00 and leaves $0.60 stranded; Compute SP then meets **$16.80** on Fargate; remainder **$32.70** | ✅ |

### Not reproduced, and why

- **`apply_ri.html` Scenarios 3 and 4** (regional and zonal RIs in *linked*
  accounts). These are consolidated-billing ordering rules — reservations apply
  to the purchasing account first, except that an unused zonal RI in another
  account is applied before a regional RI owned by the account itself. Modelling
  them needs an account dimension on every usage line and reservation plus the
  organization's payer structure, none of which Kilter observes today. The
  design doc asks for Scenarios 1–2 only. **Consequence to know about:** in a
  consolidated-billing family, this ledger prices the account it is given, so
  absorption available in a *sibling* account is invisible and stranding is
  therefore over-estimated. That direction is safe — it suppresses
  recommendations it might have allowed, never the reverse.
- **Lambda request charges.** The AWS page's illustrative rate table lists them
  as Savings-Plan-eligible at a 0 % savings rate; Kilter's §4.4 states they are
  SP-ineligible. The scenario fixture follows the AWS table so the documented
  $47.13/$59.10 reproduce exactly (the two models give identical totals, because
  a 0 %-savings rate equals the on-demand rate). The `SPIneligible` flag
  implements §4.4's rule and is pinned by
  `TestSPIneligibleUsageNeverConsumesCommitment`.
- **`r6d.metal`** in the bare-metal table looks like an AWS typo (r6i/r6id would
  fit the pattern). It is transcribed as documented rather than guessed at. An
  unlisted `.metal` type resolves to "unknown", which only denies that RI its
  size flexibility — safe, not wrong.

---

## The conservative fallback (§4.4), stated precisely

> When a usage line is Savings-Plan-eligible, a plan exists that could cover it,
> and its rate for that plan type is unknown (≤ 0), the line is billed at **zero
> marginal cost** and consumes **none** of the commitment — i.e. the commitment
> is assumed fully stranded.

Because both the before-bill and the after-bill treat such a line that way, a
change confined to those lines nets exactly **$0**. The rule can only
under-claim.

Two consequences are documented in code and tested:

1. **`Cost.HourlyUSD` is then a lower bound**, not the bill. `Cost.Fallback` and
   `Assessment.Conservative` say so, and the suppression prose appends "savings-
   plan rates were unavailable … the net is a conservative floor". Do not render
   a `Fallback` cost as an absolute number.
2. **Rate-availability harmonization.** The one way the fallback could
   over-claim is a line priced with a known rate *before* and no known rate
   *after* — it would appear to drop to zero cost and manufacture a saving out
   of a pure family swap. `NetSavings` therefore bills both sides once, unions
   the fallback line IDs, and re-bills both sides forcing every such line onto
   the fallback path. `TestConservativeFallbackHarmonizesRateAvailability`
   exhibits the phantom at the raw `Bill` level ($2.56/h out of nothing) and
   shows `NetSavings` returning $0.

Lines are matched by `UsageLine.ID`; lines with an empty ID share one bucket —
coarse, never optimistic.

---

## Deliberately deferred

- **Cross-account / consolidated billing** — see above.
- **Convertible RI exchange.** `OfferingClass` is recorded and deliberately does
  not affect billing (matching AWS: "the offering class … does not affect how
  the billing discount is applied"). §4.4 says Kilter may *note* an exchange
  path, never execute one; noting it is a recommendation-surface concern, not a
  ledger one.
- **Capacity reservations** as an availability concept. Zonal RIs reserve
  capacity; this package models only their billing effect.
- **Utilization corroboration** via `ce:GetReservationUtilization` /
  `ce:GetSavingsPlansUtilization` (§4.4 calls these optional). The ledger
  computes utilization from observed usage; corroborating it against Cost
  Explorer belongs with the audit ledger, after the fact.
- **Per-RI stranding attribution within a pool.** AWS documents no consumption
  order among interchangeable reservations, and the order cannot change the
  bill. Kilter consumes **soonest-expiring first**, so stranding lands on the
  longest-lived reservation and a `ValidFrom` is dated by the blocker that
  actually outlasts the others. If two pooled reservations have different
  effective rates, the *total* stranded dollars depend on this choice; the bill
  and the net saving do not.
- **"Net ≤ Gross always" is not enforced.** §5.2 states it, and it holds for
  ordinary rightsizing. It is false in general and deliberately not clamped: a
  recommendation that moves usage *onto* an under-used reservation is genuinely
  worth more than its list-price delta, and clamping would hide a real saving.
  `TestStrandingExample2…/other-m5-usage-absorbs-the-freed-units` shows the
  absorbing case.

---

## Wiring a later unit must do

The sync seams are **defined only** (`sync.go`); nothing here calls AWS.

1. **`kilter pricing sync-commitments`** — implement `commit.CommitmentSource`
   in a new package that may link the SDK (mirror `pkg/pricing/awssync` and
   `pkg/provider`), backed by `ec2:DescribeReservedInstances` and
   `savingsplans:DescribeSavingsPlans`. Paginate to exhaustion — a truncated
   inventory reads as "less commitment than we have", understates stranding and
   re-opens the trap. Write with `commit.WriteInventory`. Optionally implement
   `commit.RateSource` over `DescribeSavingsPlansOfferingRates`
   (**[unverified action name]**, §4.4) and write with `commit.WriteRateTable`;
   an implementation that cannot resolve a rate must **omit** it, never
   substitute the on-demand rate — the fallback is only safe when absence is
   honest.
2. **Loading** — `commit.LoadInventoryFile(path)` and
   `commit.LoadRateTableFile(path)` behind a flag (`--commitments`,
   `--sp-rates`). Both are optional: a `nil *Inventory` prices everything at
   on-demand, which is exactly right for an account holding no commitments
   (`TestNoCommitmentsMeansNetEqualsGross`). Per §5.3 the inventory also arrives
   on EC2-domain snapshots as `Snapshot.Commitments *commit.Inventory`; the
   brain keeps **one** account-wide ledger regardless of which domain delivered
   it.
3. **Build one account-wide `Usage`** per hour, from *every* domain Kilter
   observes — k8s-nodes, ec2, fargate, lambda. Compute Savings Plans absorb
   usage account-wide, so a partial `Usage` understates absorption and
   overstates the saving. `UsageLine.ID` must be the stable target identity
   (instance ID / workload key / ARN) and must be the **same on both sides** of
   a comparison — the harmonization keys on it.
4. **`pkg/plan`** — replace list-price deltas with:
   ```go
   as := inv.Active(now).NetSavings(before, after)
   if as.Suppressed { /* drop the step; surface as.Reason and as.ValidFrom */ }
   step.SavingsMonthlyUSD = as.ClaimableMonthlyUSD()
   ```
   `Recommendation.GrossSavingsMonthlyUSD = as.GrossMonthlyUSD` and
   `NetSavingsMonthlyUSD = as.ClaimableMonthlyUSD()` (§5.2), and
   `Recommendation.ValidFrom = as.ValidFrom` (§4.4 ex.1). Because
   `inv.Active(now)` drops expired commitments, a suppression lapses on its own
   with no stored state to expire — pinned by the post-expiry half of
   `TestStrandingExample1FamilyMigrationOffAnRI`.
5. **`pkg/api`** — publish `netSavingsMonthlyUSD` from
   `ClaimableMonthlyUSD()`; publish `grossSavingsMonthlyUSD` only labelled as
   list price. Surface `suppressed`, `reasonCode`, `reason`, `validFrom` and
   `conservative` so a user can see *why* a rec vanished. If `Cost.Fallback` is
   set, do not render the absolute bill.
6. **Audit ledger** — the claimed-vs-measured comparison should now compare
   against the `Bill()` delta. §4.4 ex.2 is precisely the case where the audit
   ledger would have caught the error after the fact; this package prevents it
   before the fact.

---

## Test inventory

`awsdoc_test.go` (AWS scenarios + SP scope/eligibility) · `stranding_test.go`
(§4.4 ex.1–3 end to end, plus the absorbing variants and the no-commitment
case) · `fallback_test.go` (conservative fallback, harmonization, garbage
clamping) · `normalize_test.go` (exhaustive normalization table, metal table,
size-flexibility limitation matrix, zonal/regional matching) ·
`determinism_test.go` (200-permutation shuffle, `NetSavings` shuffle,
concurrency) · `fuzz_test.go` (`FuzzWaterfall`, `FuzzNetSavings`) ·
`inventory_test.go` (JSON round-trip and byte-stability, validation table,
`Active`, `RateTable`, sync-seam satisfiability offline).

Both fuzz targets were also run actively (`-fuzz`, ~1M+ execs each) beyond the
seed corpus. The one failure they produced was a defect in the *test's* unit
attribution helper, not the waterfall: two generated lines shared an ID and
quantity but not their size, and the helper attributed the larger size's units
to the smaller line. The helper now index-aligns against the canonical order and
asserts that alignment; the failing input is kept in
`testdata/fuzz/FuzzWaterfall/` as a regression seed.

---
---

# U12 — Reserved DB Instances (`pkg/pricing/commit/rds.go`)

`Usage.Validate()` used to reject an RDS line outright, so until this landed an
RDS domain could not produce a single dollar figure — netted or not. U12 adds
the fourth commitment product so U11/U13 have a ledger to bill against.

`gofmt`, `go vet ./...`, `go build ./...`, `go test -race -short ./...` and
`go test ./test/scale/` are green. `go.mod` and `go.sum` are untouched; `rds.go`
imports `fmt`, `math`, `sort`, `strconv`, `strings`, `time` and nothing else. No
existing test was modified, weakened or skipped.

## Scope: what was touched outside `rds.go` / `rds_test.go`

The unit brief allows one constant block in `commit.go`, one clause in
`Usage.Validate()` and one branch in `waterfall.go`. Four edits go slightly
past that letter. Each is listed here because a scope note that hides an edit
is worse than the edit.

| File | Edit | Why it was not avoidable |
| --- | --- | --- |
| `commit.go` | `KindRDS` in the `Kind` block | as specified |
| `commit.go` | one clause in `Usage.Validate()`, plus `KindRDS` added to the existing closed-set check | as specified; the clause is **last** in the `switch` on purpose — a Go case does not fall through, so an earlier RDS case would have skipped the quantity/rate checks that apply to RDS too |
| `waterfall.go` | one stage in `bill()` (+ two doc-comment corrections) | as specified |
| `commit.go` | `UsageLine.Engine`, `UsageLine.Deployment` | size flexibility is **engine-gated** and topology is a **unit multiplier**. Neither fact can be represented without carrying them on the line, and Go cannot add a field to a struct from another file. Overloading `Platform` as the engine was considered and rejected: `NormalizePlatform("")` returns `Linux/UNIX`, so an engine-less line would have silently acquired a nonsense engine identity |
| `commit.go` | `UsageLine.canonicalKey()` gains the two fields | the key's contract is "two lines with the same key are indistinguishable in every output field". Without this, two lines differing only in engine sort by input position and `Bill` stops being order-independent. Both fields are folded through `NormalizeRDSEngine` / `NormalizeRDSDeployment`, so spelling variants still collapse |
| `commit.go` | `Inventory.ReservedDBs`, one line in `Active()`, one line in `Validate()` | there is nowhere else to hold the product. The two one-liners delegate to `rds.go`. `Active()` is load-bearing, not cosmetic: it is the entire mechanism by which an RDS suppression lapses without stored state |

## What was verified

All quotes are from
`https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_WorkingWithReservedDBInstances.html`
via §2.5–2.6 of `docs/design/rds-batch-assessment.md`.

| Claim | Encoded in | Pinned by |
| --- | --- | --- |
| Normalized-unit table, 14 sizes × 3 deployment columns (micro 0.5 … 32xlarge 256; Multi-AZ instance ×2; Multi-AZ cluster ×3) | `rdsSizeUnits`, `RDSDeployment.Multiplier` | `TestRDSNormalizationTableMatchesPublishedUnits` — **hand-transcribed a second time**, 14 rows × 3 columns, never reading `rdsSizeUnits`; also asserts the map's row count and that the table is *not* the EC2 one |
| "can only scale in their instance class type… not to a `db.r6id.large` or `db.r7g.large`" | `RDSClassType` | `TestRDSClassTypeIsNotTheFamily`, `TestClassTypeChangeStrandsEntirely` |
| "Size flexibility does not apply to RDS for SQL Server and RDS for Oracle License Included" | `RDSSizeFlexibleEngine` | `TestRDSEngineGateMatchesTheDocumentedExclusions`, `TestSizeFlexibilityIsEngineGated` |
| "benefits apply to both Multi-AZ and Single-AZ… one large (4 units) ↔ two medium (2+2 = 4)" | `RDSNormalizationUnits` | `TestReservedDBInstanceCoversMultiAZAsTwoUnits` (both directions, plus the half-covered and ×3 cases) |
| "tied to instance type and AWS Region"; "same AWS Region and database engine" | `applyReservedDB` eligibility | `FuzzRDSWaterfall`, `TestSizeFlexibilityIsEngineGated` |
| "no discount for storage, backups, and I/O" | `UsageLine.validateRDS` requires a DB instance class | `TestRDSStorageIsNotExpressibleAsCoveredUsage` |
| "You can't cancel a reserved DB instance" | stranding is permanent until `Expires`; that date is the `ValidFrom` | `TestClassTypeChangeStrandsEntirely` (incl. the post-expiry lapse) |
| **Compute SPs do not cover RDS** — nor do EC2 Instance SPs or EC2 RIs | the `Kind` allowlists in `waterfall.go` already excluded it; `applyReservedDB` touches `KindRDS` only | `TestComputeSPNeverAbsorbsRDS` (a *fully exhausted* Compute SP + EC2 SP + an `r6i` EC2 RI, all unable to reach the DB line), `FuzzRDSWaterfall` |
| Widening the closed set changes no EC2/Fargate/Lambda outcome | — | `TestExistingKindsAreUnaffectedByRDS` (`reflect.DeepEqual` per coverage row and per commitment, in three configurations, plus an EC2 `NetSavings` verdict), and the pre-existing suite unchanged |

Both fuzz targets that existed before were re-run actively after the
`canonicalKey` change (≈3.5M and ≈3.1M execs). `FuzzRDSWaterfall` was run
actively for ≈1.6M+ execs. No failures, so no new seed was added to
`testdata/fuzz/`.

## `[unverified]`

1. **Seven of the fourteen table rows.** §2.5 prints `micro, small, medium,
   large, xlarge, 2xlarge, … , 32xlarge` — the ellipsis hides
   `4xlarge, 6xlarge, 8xlarge, 10xlarge, 12xlarge, 16xlarge, 24xlarge`. Those
   seven are reconstructed from the U12 acceptance criterion's "all 14 sizes"
   plus the 8×N rule every printed row obeys. The reconstruction is exact if and
   only if the elided set is that one; the row *count* is asserted, so a wrong
   set fails loudly rather than silently. **Closing it:** read the AWS page and
   confirm the seven names.
2. **Non-flexible engines and the Multi-AZ ⇄ Single-AZ boundary.** AWS documents
   that SQL Server / Oracle-LI have no *size* flexibility. It does not say
   whether a Single-AZ SQL Server reservation may cover a Multi-AZ SQL Server
   instance — a conversion AWS otherwise expresses through the same normalized
   units. `applyReservedDB` requires an exact topology match on the
   non-flexible path. That strands more, never less.
3. **Engine vocabulary.** `NormalizeRDSEngine` folds only the licence marker
   (`(license-included)` → `(li)`, `bring-your-own-license` → `byol`) and
   `postgres` → `postgresql`. It deliberately preserves the edition, so
   `sqlserver-se` ≠ `sqlserver-ee`. Whether `DescribeReservedDBInstances`'s
   `ProductDescription` and `DescribeDBInstances`'s `Engine` agree on every
   other spelling (Db2 editions in particular) is not verified; a mismatch
   denies coverage, which over-states stranding.
4. **Aurora.** Its billing model is a third thing wearing RDS's name and nothing
   about it is verified here, so `aurora-*` is not a size-flexible engine and
   `db.serverless` is not a recognizable class. Aurora usage therefore gets
   exact-match coverage at best. U11 refuses Aurora by name anyway.
5. **Consumption order within a pool.** As for EC2, AWS documents no order.
   Soonest-expiring reservation first, smallest instance first within one
   reservation. Neither choice can change a total — the units absorbed are
   `min(capacity, demand)` either way — only per-line and per-reservation
   attribution, and therefore which date a suppression carries.
6. **`OfferingType`** (All/Partial/No Upfront) is recorded and does not affect
   billing; the amortization is already in `EffectiveHourlyUSD`. Same treatment
   as `ReservedInstance.OfferingClass`.

## Design decisions worth arguing with

**An unset `Deployment` is unknown, not Single-AZ** — the reverse of how
`Platform` treats an empty string, and the asymmetry is deliberate. Guessing
Linux for an unknown platform only ever grants coverage AWS would also have
granted. Guessing Single-AZ for an unknown topology *halves* the units a line
needs, so a reservation appears to cover twice the usage it really covers — an
error in the exact direction this package exists to prevent. `Usage.Validate`
rejects it loudly; `Bill`, which never fails, leaves the line uncovered.
`TestRDSDeploymentIsNotGuessedWhenUnset`.

**RDS committed dollars join `RICommittedUSD`, not a new `Cost` field.** They
are reservation dollars, charged whether absorbed or not, so the documented
partition `HourlyUSD == RICommittedUSD + SPCommittedUSD + OnDemandUSD` keeps
holding verbatim and every existing `assertPartition` call stays valid. The
price: a UI cannot yet say "of the reserved spend, this much is RDS". Adding
`RDSCommittedUSD` later is a one-line change plus a revisit of
`assertPartition`, which is a test this unit was not allowed to touch.

**`SPIneligible` on an RDS line is an error, not a no-op.** No Savings Plan
covers RDS at all, so the flag can only mean the caller believes one might.
Saying so beats letting a wrong mental model through silently.

**Net ≤ Gross is asserted only where it is true.** `FuzzRDSWaterfall` bounds
`0 ≤ net ≤ gross` on a *single-line* shrink over arbitrary inventories, and
`TestRDSNetCanExceedGrossWhenFreedUnitsAreAbsorbed` exhibits the multi-line
counter-example: four `db.r6i.medium` instances hold all 8 units of a
`db.r6i.xlarge` reservation (smallest-first), leaving a dear-per-unit
`db.r6i.large` on demand; switching the mediums off frees units the large
absorbs, so the invoice falls **$1.00/h against a $0.20/h list-price delta**.
That is a real saving and clamping it would hide one. This matches the existing
"'Net ≤ Gross always' is not enforced" section above — RDS just supplies a
cleaner witness for it.

**RDS can never be `Conservative`.** There is no account-wide RDS commitment
(§4.4 ex.3 has no RDS analogue) and therefore no Savings-Plan rate to be
missing. An RDS-only assessment is exact given the inventory, never a floor.
Asserted in `FuzzRDSWaterfall`.

## What RDS exposed in the existing waterfall

### 1. A negative `Count` produces a negative bill *(defect, not fixed — out of scope)*

`applyRI` clamps the reservation count when it computes capacity
(`float64(max(ri.Count, 0)) * perRI`) but `bill()` does **not** clamp it when it
computes money:

```go
committed := float64(ri.Count) * sane(ri.EffectiveHourlyUSD)   // waterfall.go
```

`sane()` guards the rate and not the count, so an unvalidated inventory holding
`{Count: -5, EffectiveHourlyUSD: 0.10}` yields `RICommittedUSD = -0.50` and
`HourlyUSD = -0.404` — a *negative* bill. That contradicts the package's stated
contract ("garbage clamps rather than poisoning totals") and the "never below
the committed floor" invariant becomes vacuous. `FuzzWaterfall` cannot reach it:
`synth` generates `Count: 1 + s.mod(5)`.

Reachable in production only through a hand-edited or third-party inventory
file, since `Inventory.Validate` rejects `Count <= 0` and `LoadInventory` calls
it — but `Bill` is documented as total, and here it is not.

**Fix:** wrap the two `float64(ri.Count)` occurrences in `max(…, 0)`. The RDS
stage already does exactly that, which is how the asymmetry surfaced. Left
unmade because the brief allows one *new* branch in `waterfall.go`, not edits to
the RI loop; a `synth` that can emit a negative count belongs with it, and
`fuzz_test.go` is likewise off-limits.

### 2. `CommitmentUse.Kind` is an unchecked free-form string

`newlyStrandedExpiry` keys stranding deltas on `Kind + "/" + ID`. Adding a
fourth product meant inventing `"reserved-db-instance"` with no compile-time
check that it is distinct from `"reserved-instance"`. A typo would silently
merge two products' stranding and mis-date every `ValidFrom` built from them.
It wants to be a typed constant set the way `Kind` and `SavingsPlanType` are.

### 3. The eligible-product set is written out three times

`Usage.Validate`, the EC2-Instance-SP predicate and the Compute-SP predicate
each open-code the closed set of `Kind`s. Forgetting `Validate` is safe (the
line is rejected); forgetting a *predicate* is not — a new kind accidentally
listed there silently gains Savings-Plan coverage it does not have. One
`func (k Kind) computeSPEligible() bool` would make it a single source of truth.
`TestComputeSPNeverAbsorbsRDS` and `FuzzRDSWaterfall` pin the RDS half of it, so
the property is guarded even though the structure is not.

### 4. `UsageLine.Family()` and `SizeOf()` answer confidently and wrongly for RDS

`FamilyOf("db.r6i.large")` is `"db"` and `SizeOf` is `"r6i.large"` — plausible
strings, not errors. Any future code that reaches for them on an RDS line gets a
wrong answer silently, and `"db"` collapses *every* DB class into one family,
which is exactly the merge AWS's `db.r6i` / `db.r6id` counter-example forbids.
`TestRDSClassTypeIsNotTheFamily` asserts the collapse, so a change to `FamilyOf`
that would make the trap look safe fails the test instead.

### 5. Under-specified: reservation identity is per-product

`Inventory.Validate` namespaces duplicate-ID detection as `ri/…` and `sp/…`;
`validateReservedDBs` keeps its own set. A Reserved DB Instance may therefore
share an ID with a Reserved Instance. That is harmless today — `CommitmentUse`
carries the product in `Kind` and `newlyStrandedExpiry` keys on both — but it is
an invariant nobody stated, and finding #2 is what makes it load-bearing.

### 6. Under-specified: an empty engine matches an empty engine

`Bill` never fails, so an engine-less reservation and an engine-less line both
normalize to `""` and match each other on the exact-class path. `Usage.Validate`
and `Inventory.Validate` both reject an empty engine, so this is only reachable
by skipping validation — the same shape as finding #1, and worth the same
scepticism about how total `Bill` really is.

## Test inventory

`rds_test.go`: `TestRDSNormalizationTableMatchesPublishedUnits` ·
`TestRDSDeploymentMultiplierAndNormalization` ·
`TestRDSNormalizationUnitsExtrapolatesAndRefuses` ·
`TestRDSClassTypeIsNotTheFamily` ·
`TestRDSEngineGateMatchesTheDocumentedExclusions` ·
`TestComputeSPNeverAbsorbsRDS` · `TestReservedDBInstanceCoversMultiAZAsTwoUnits` ·
`TestSizeFlexibilityIsEngineGated` · `TestClassTypeChangeStrandsEntirely` ·
`TestRDSDeploymentIsNotGuessedWhenUnset` ·
`TestRDSStorageIsNotExpressibleAsCoveredUsage` ·
`TestRDSNetCanExceedGrossWhenFreedUnitsAreAbsorbed` ·
`TestExistingKindsAreUnaffectedByRDS` ·
`TestReservedDBInstanceJSONRoundTripAndValidation` ·
`TestRDSUsageLineValidation` · `TestActiveDropsExpiredReservedDBInstances` ·
`FuzzRDSWaterfall`.

## What U11/U13 must do with this

1. Emit **one `UsageLine` per DB instance** with `Kind: KindRDS`,
   `InstanceType` = the DB instance class, `Engine` = the product description,
   and `Deployment` set explicitly — never left empty, or the line silently
   loses every reservation it was entitled to.
2. Emit storage, backup and I/O as **separate lines that are not `KindRDS`**, or
   keep them out of `Usage` entirely the way EBS is. A reserved DB instance
   never discounts them, and `Usage.Validate` will reject them as RDS lines.
3. Read replicas are **separate billable instances**, each its own line. Their
   own reservations apply; their resizing does not (§2.5.2 — permanently
   advisory).
4. Populate `Inventory.ReservedDBs` from `rds:DescribeReservedDBInstances`,
   paginated to exhaustion. A truncated inventory reads as "less commitment than
   we have", understates stranding and re-opens the trap.
5. Do **not** set `ComputeSPRate`, `EC2SPRate` or `SPIneligible` on an RDS line.
   No plan covers RDS; the last of those three is now a validation error.
