# pkg/pricing hardening findings

Quality-hardening pass over `pkg/pricing` and `pkg/pricing/awssync`. Every fix
below has a test that fails on the pre-fix code (verified by reverting the
sources and re-running) and passes now. `gofmt`, `go vet`, `go build ./...`,
`go test -race ./pkg/pricing/...`, and the full `go test -race -short ./...`
are green.

## Bugs found and fixed

1. **`awssync.fetchSpot`: stale spot prices leaked into the AZ average across
   pages.** The "newest price per AZ" map was rebuilt for every pagination
   page, so any AZ whose history spanned a page boundary contributed one quote
   *per page* — including stale ones — to the average, violating the
   documented "latest, averaged across AZs" contract and skewing the synced
   spot price. Newest-per-AZ is now resolved across all pages before
   averaging. Tests: `TestFetchSpotLatestAcrossPages`,
   `TestFetchSpotNewerOnLaterPage`.

2. **`awssync.archOf`: the `a1` family (first-generation Graviton) was
   classified `amd64`.** The "g after the generation digit" heuristic misses
   the one arm64 family that predates it. An arm64 node type labeled amd64 is
   an outage risk, not just a pricing error. Now special-cased. Test:
   `TestArchOfFamilies`.

3. **`awssync`: `trn1`/`trn2` (Trainium) were flagged `Burstable`** by the
   `strings.HasPrefix(fam, "t")` check, which would make planners exclude
   sustained-use accelerated instances as if they were credit-based. Burstable
   now requires `t<digit>` (t2/t3/t3a/t4g). Test:
   `TestBurstableClassification`.

4. **`awssync.ParsePriceListEntry` accepted `"Infinity"` as a price and chose
   a random price among multiple hourly dimensions.** `strconv.ParseFloat`
   parses `"Infinity"` with a nil error, so an entry could carry
   `HourlyUSD = +Inf`; and when a SKU had several `Hrs` dimensions, Go map
   iteration order decided which price won, making repeated syncs disagree.
   Non-finite quotes are now rejected and the lowest positive hourly quote
   wins deterministically. Tests: `TestParsePriceDeterministicAcrossDimensions`,
   `FuzzParsePriceListEntry` (the Infinity seed fails pre-fix).

5. **`awssync.parseMemory`: int64 overflow on huge TiB values.** Float→int64
   conversion overflow is implementation-defined in Go (wraps negative on
   amd64, saturates on arm64), so garbage like `"99999999999 TiB"` produced
   platform-dependent nonsense capacity. Values ≥ 2^63 bytes are now rejected,
   with exact boundary tests at 8388607/8388608 TiB. Test:
   `TestParseMemoryAdversarial`, `FuzzParseMemory`.

6. **`pricing.NodeHourlyCost`: negative capacity priced nodes below free.**
   A corrupt snapshot with negative `Capacity` values made the fallback path
   return a negative hourly cost, which then poisoned `SnapshotCost` totals
   and any savings math built on them. Capacity now clamps at zero; a `+Inf`
   hourly-cost annotation is also ignored (collect guards this at its
   boundary, but snapshots can be built by other paths — the invariant
   "finite and ≥ 0" is now local to the decision code). Test:
   `TestNodeHourlyCostAdversarial`.

7. **`pricing.Load` accepted silently-corrupting catalogs.** Previously
   loadable without error: duplicate provider/name entries (double-counted in
   `Candidates` while `Lookup` saw only the last one), negative spot prices,
   and typo'd arch strings like `"x86_64"` (entries that silently never match
   any arch filter). All three now fail loudly with specific errors. Tests:
   `TestLoadValidationBoundaries`, `FuzzLoad`.

8. **`awssync.fetchOnDemand` could emit duplicate catalog entries.** The
   Pricing API can list more than one SKU matching the filters for the same
   instance type; combined with fix 7 that output would now fail to load.
   Sync dedupes by instance type keeping the cheapest quote. Tests:
   `TestSyncDedupesDuplicateSKUs`, `TestSyncInstancesDeterministic`.

9. **`pricing.Candidates`: unspecified order for price ties.** `sort.Slice`
   is not stable, so equal-price types (e.g. m5.large vs m6i.large at $0.096)
   could reorder across Go versions or catalog file edits, making plans
   non-reproducible. Ties now break by (provider, name). For the embedded
   catalog this coincides with the existing file order, so no downstream
   behavior changed (full suite verified). Test:
   `TestCandidatesDeterministicTieBreak`.

10. **`pricing.SnapshotCost` dereferenced a nil snapshot.** Now prices as an
    empty cluster. Test: `TestSnapshotCostNilAndSumInvariant`.

## Invariants documented (godoc/comments)

- `NodeHourlyCost` result is always finite and ≥ 0 for a Load-validated
  catalog; resolution order annotation → catalog → fallback.
- `Price(spot=true)` with no positive spot price falls back to on-demand
  (overstates cost rather than inventing a discount).
- `SnapshotCost.HourlyUSD` equals the sum of its per-node entries; nil
  snapshot = empty cluster.
- `Candidates` ordering is fully deterministic: (price, provider, name).
- `Load` validation contract (positive shape/price, non-negative spot, known
  arch, no duplicates) and why it fails loudly.
- `HoursPerMonth` is the 8760/12 billing-average convention.
- `fetchSpot` newest-per-AZ-across-pages requirement; `parseMemory` overflow
  guard rationale; `burstableRe` vs Trainium; `a1` Graviton exception.

## Tests added

- `pkg/pricing/hardening_test.go`: table-driven Load boundary/garbage cases,
  adversarial `NodeHourlyCost` (NaN/±Inf/negative annotation, negative/zero/
  MaxInt64 capacity), Price spot-guard probes, deterministic tie-break,
  nil/empty/sum-invariant SnapshotCost, and `FuzzLoad` (accepted catalog ⇒
  all decision invariants hold, index agrees with instance list).
- `pkg/pricing/awssync/hardening_test.go`: paginated spot fakes (stale-later
  and newer-later cases), arch/burstable classification tables, adversarial
  memory parsing with exact overflow boundaries, deterministic price pick
  under map-order shuffling (50 iterations), Sync dedupe + determinism, and
  `FuzzParsePriceListEntry`/`FuzzParseMemory`. All fuzzers also ran 10–15 s
  of active fuzzing (~7M execs total) with no failures.

## Deliberately left undone

- **Spot ≥ on-demand in custom catalogs is still accepted by `Load`.** awssync
  never emits it (it drops spot quotes ≥ on-demand), and a user's custom
  catalog may encode an unusual but intentional market. Planners treat such an
  entry as "spot saves nothing", which is self-consistent, so rejecting it
  felt like deleting a legitimate (if odd) input rather than hardening.
- **`fetchSpot` does not pass the `Families` filter to
  `DescribeSpotPriceHistory`.** It fetches all types and merges by name later
  — wasteful on the wire but correct; narrowing it needs the family→
  instance-type expansion the API filter expects, which is more surface than
  the win justifies here.
- **Nondeterministic `map[string]any` catalog rendering in `Sync`** is fine in
  practice (`encoding/json` sorts map keys), noted only for completeness.
- The embedded catalog's baseline prices were sanity-checked for internal
  consistency (spot < on-demand everywhere, no duplicates, positive shapes —
  now also enforced by `Load` + tests) but not re-verified against current AWS
  list prices; that is what `kilter pricing sync-aws` is for.

---

# Rate consolidation: one source of truth for every AWS price

Four waves of domain work each needed rates and could not edit `pkg/pricing`
without colliding with a sibling agent, so each inlined its own copy. This pass
moved the numbers here and left the domain packages' exported surface exactly
as it was. `gofmt`, `go vet ./...`, `go build ./...` and
`go test -race -short ./...` are green; `go.mod` and `go.sum` are byte-identical
and no existing `*_test.go` was touched.

The result is checkable by grep: every embedded AWS price now appears as a
literal in exactly one file.

## Findings

### 1. pkg/lambda priced requests two ways in one report (bug, fixed)

`usageLines` built the commitment usage lines for a function. The **duration**
line took its rate from the caller's `Rates`; the **requests** line took its
rate from the package constant:

```go
Unit: "GB-Seconds",         ODRate: r.GBSecond(arch)        // honours Config.Rates
Unit: "Requests-Millions",  ODRate: RequestUSDPerMillion    // ignores it
```

`Config.Rates` is the documented override point — `Rates` is a plain struct
precisely so an operator can supply their own numbers. An operator who did got
those numbers honoured by `Rates.InvocationUSD` (what the sizer prices an
invocation at) and silently ignored by `commit`'s waterfall (what the report
claims the bill is). The same requests, in the same report, at two prices.
Nothing failed, because nothing compared them.

Fixed: the line now reads `r.RequestUSD * 1e6`. Pinned by
`TestLambdaRequestLineHonoursOverriddenRates`, which drives the real Lambda
domain through a capturing `domain.Netter` and fails on the pre-fix code
(verified by reverting: `requests line ODRate = 0.2, want 0.6000000000000001`).

This is the discrepancy the unit was looking for. It is a *latent* one — the
shipped defaults agree — which is exactly why it survived four waves of
single-package tests.

### 2. One price function returns two prices on arm64 (FMA contraction)

`pricing.FargateRates.Cost` is the single implementation of
`P(v,g) = v·rate_v + g·rate_g`, and `pkg/ecs` calls it rather than
re-implementing it. Even so, an early version of
`TestFargateX86TierPriceAgreesBetweenEKSAndECS` failed:

```
1vCPU 5GB: EKS bills 0.06270500000000001/h, ECS bills 0.062705/h
```

Same function, same inputs, two results. On arm64 the Go compiler may contract
`a*b + c` into a single fused multiply-add, and whether it does depends on how
the call site inlined — so the *identical expression* evaluates one ULP apart
at two call sites. Confirmed with a standalone repro (`//go:noinline` vs
inlined: `0.062704999999999996851` vs `0.062705000000000010729`).

Nothing in production is wrong: no production code compares two independently
computed money values bitwise (checked). But it sets the rule for this kind of
test, which is the rule `pkg/pricing/commit` already documents — **money is
compared through `commit.Eps` or in cents, never with `==`**. Stated as a
property by `TestFargatePricesAgreeToTheCentNotToTheBit`, so a future
"tighten this to `==`" is a deliberate act with a documented reason to overrule
rather than an arch-dependent flake.

### 3. The request rate is exact as a constant and inexact at runtime

`RequestUSDPerMillion / 1e6` written as a Go *constant expression* is exactly
`2e-07` — constant arithmetic is arbitrary-precision. The same division
performed on `float64` values at runtime yields `2.0000000000000002e-07`.

The naive delegation (`RequestUSD: pricing.DefaultLambdaRates().RequestUSD()`)
would therefore have moved `lambda.DefaultRates().RequestUSD` by one ULP as a
side effect of a refactor that was supposed to change nothing. Caught by
`TestBaselineTablesReturnTheDeclaredConstants`.

`pricing.LambdaRequestUSD` is now a constant expression, and `pkg/lambda` reads
it, so the pre-refactor value is preserved bit-for-bit.
`LambdaRates.RequestUSD()` remains for rate tables that did not come from the
constants, and its doc says which one to prefer.

### 4. Two documented figures are rounded, and one pair does not round-trip

- `docs/design/compute-domains.md` §4.1.1 quotes the billed price of the
  overhead-cliff example as **$0.12097/h**. The exact product is
  `2·0.04048 + 9·0.004445 = $0.120965`. The test pins the exact value and says
  the doc rounds.
- §4.1 quotes the Fargate rates twice, per second and per hour, and the two do
  not round-trip: `$0.000001235/GB-s × 3600 = $0.004446`, against the per-hour
  quote of **$0.004445** (0.02 % apart). The per-hour figure is authoritative
  — `0.004445/3600 = 0.00000123472…`, which rounds to the quoted
  `0.000001235` — so this is AWS's own rounding, not an error. Pinned by
  `TestFargatePerSecondAndPerHourQuotesAreOnePrice` at the precision AWS
  quotes, so a future sync that swapped a per-second quote in as if it were
  per-hour lands outside the tolerance.

### 5. No rate disagreement between domains, and that is now checked

Cross-checked and found consistent: EKS vs ECS x86 Fargate across all 74 tiers;
the ARM Fargate deltas against §4.5 (−20.0 % vCPU, −19.9 % GB); the Lambda ARM
delta against §4.5 (−20.0 %); the EBS worked examples against §4.7 ($50→$40 at
500 GiB, $372.50 at 4,000 GiB parity); the t3.large/m5.large breakeven against
§4.6 (u ≈ 43 %, a joint fact about `catalog.json` and the surplus rate);
`HoursPerMonth` across five packages.

## What moved

| From | To |
|---|---|
| `lambda.RequestUSDPerMillion`, `X86GBSecondUSD`, `ARMGBSecondUSD` | `pricing.LambdaRates` + constants; new exact `pricing.LambdaRequestUSD` |
| `lambda.FreeEphemeralStorageMB` | `pricing.LambdaFreeEphemeralStorageMB` (a `GlobalFacts` allowance, not a rate) |
| `ecs.ARMVCPUHourlyUSD`, `ARMGBHourlyUSD` | `pricing.FargateARMRates` + constants |
| the six money fields of `ebs.DefaultRates()` | `pricing.EBSRates` + constants |
| `ec2.SurplusCreditUSDPerVCPUHour` | `pricing.EC2SurplusCreditUSDPerVCPUHour` |
| `ec2.CreditsPerVCPUHour` | `pricing.CreditsPerVCPUHour` (it is also the `/60` in `SurplusUSDPerCredit`; a divisor that drifted between the accrual model and the price would misreport both) |
| `ebs.HoursPerMonth`, `ec2.HoursPerMonth` | defined from `pricing.HoursPerMonth`; the existing assertions now hold structurally |

Every domain constant kept its name **and stayed a constant**, so
`const x = lambda.X86GBSecondUSD` still compiles and `cmd/` needs no change.

`pricing.FargateRates` gained a `Region` field (`json:"-"`), so every rate type
in the package is uniformly region-keyed. It is safe against the two existing
reflection tests, which ban `arm`/`spot`/`graviton` field names — the §7 trap 3
guarantee is untouched, and `TestARMFargateRatesAreSingleSourcedWithoutReachingEKS`
re-asserts it after the move.

## Region-awareness

Every AWS price here varies by region, so every rate type carries a `Region`
and is reached through a `...RatesFor(Region) (T, bool)` accessor. The embedded
tables hold `us-east-1` only — this package is air-gapped and ships what was
verified. Inventing per-region numbers would be worse than the duplication this
pass removed, so instead: a lookup for an unknown region returns `ok == false`
and a table whose `Region` field says `us-east-1`, and a report can label it
honestly. `TestUnknownRegionIsReportedNotFabricated` covers every accessor.

The inputs that genuinely do not vary by region — the Fargate memory overhead,
Lambda's free ephemeral storage, the billing-average month — are `GlobalFacts`,
which has **no** `Region` field and no per-region accessor. That absence is the
statement, and `TestGlobalFactsIsNotRegionKeyed` keeps it.

## What did NOT move, and why

- **`ecs.DefaultSpotDiscount = 0.50`** — not an AWS rate. AWS advertises "up to
  70 %" and publishes no number; the realized discount varies by region,
  architecture and moment. It is a deliberately conservative policy default an
  operator overrides. Filing it beside the rate card would give it a
  provenance it does not have.
- **`ebs.GP3BaseIOPS` / `GP3BaseThroughputMBps`** (and therefore
  `Rates.GP3FreeIOPS` / `GP3FreeThroughputMBps`) — device capability, not
  price. gp3's *included* allowance happens to equal its *delivered* baseline;
  moving the number here would have created a second copy rather than removed
  one. `TestEBSRatesAreSingleSourced` pins that the rate table's free
  allowances still equal the device baseline.
- **`ebs.GP2Performance`** (3 IOPS/GiB, the burst bucket, the 334 GiB
  throughput step) — performance, not money.
- **`lambda.MinMemoryMB` / `MaxMemoryMB` / `MinBilledMS`** — platform limits,
  region-independent, and they live in `lambda.go`, which is not a rate-table
  file and was out of this unit's scope. They would fit `GlobalFacts`; left as
  a follow-up rather than widened into.
- **`commit.HoursPerMonth`** — duplicated on purpose, so `pkg/pricing` stays
  free to depend on `pkg/pricing/commit` later without an import cycle. That
  reasoning still holds, so it stays duplicated and stays asserted, now by
  `TestHoursPerMonthAgreesAcrossEveryDomain` as well as its own test. `pkg/ebs`
  and `pkg/ec2` had no such reason — both already import `pkg/pricing` — so
  theirs were converted from asserted-equal to structurally-equal.
- **`ecs.MinMoveMonthlyUSD = 0.10`** — a policy floor that happens to equal the
  gp2 GB-month rate. Recorded in the `notARate` allowlist with its reason,
  which is where the price/policy distinction is now written down.
- **`catalog.json`** — already single-sourced here; untouched.

## Tests

New, all in `pkg/pricing`:

- **`duplication_test.go`** — the "exactly one place" proof, as a source scan
  over `pkg/{lambda,ecs,ebs,ec2}`.
  `TestNoRateLiteralsInDomainPackages` fails if a price this package owns
  reappears as a literal in a money-named binding;
  `TestNoMoneyLiteralsInDomainPackages` fails on a money-named literal this
  package has never heard of, catching the *next* rate born in the wrong place;
  `TestCanonicalRatesCoversEveryEmbeddedRate` stops a new rate here from
  silently escaping the scan; `TestRateLiteralCheckActuallyFires` is the
  scan's own smoke alarm and prunes stale allowlist entries. Verified to fail
  by re-inlining a rate and re-running.
- **`crossdomain_test.go`** (`package pricing_test`, the only place all four
  domains are visible at once) — every §4.1 tier priced through both Fargate
  domains; the same workload sized by both, proving the +59 % cliff is
  attributable entirely to quantization and not to drifted rates; the ARM
  single-sourcing that must not put ARM on EKS; the Lambda override
  disagreement above; the non-monotone Lambda cost curve (lowering memory
  raises the bill; a linear speedup is flat; a −20 % rate at a +25 % duration
  costs *more*); the §4.7 EBS worked examples; the §4.6 burstable breakeven;
  `HoursPerMonth` across five packages; and the overhead-cancellation identity
  between `ecs.RoundUpTier` and `pricing.Quantize` at every tier.
- **`rates_test.go`** — the region contract (unknown region ⇒ told, never
  fabricated), every rate type carries its `Region`, `GlobalFacts` carries
  none, the baselines return the declared constants, every embedded rate is
  positive and finite with the rate card's internal orderings (gp3 < gp2,
  io1/io2 > gp2, arm < x86), the per-second/per-hour pin, JSON round-trip
  including the region label, and that a loaded override file carries no
  region claim.

## Deliberately left undone

- **`awssync` does not sync the new tables.** It syncs the instance catalog
  only; there is no Price List fetch for Lambda, EBS or surplus-credit SKUs,
  and §4.9 already flags the Fargate `serviceCode` as unverified. The
  region-keyed accessors are the seam that work fills; writing a fetcher
  against an unverified `serviceCode` would ship guesses.
- **`cmd/` is not wired to pass a region in.** Another agent owns `cmd/` this
  round, and the scope forbade touching it. `LambdaRatesFor` and friends are
  therefore exported-and-unused by production code until that wiring lands —
  deliberately, so the wiring has something to call.
- **No `LoadLambdaRates` / `LoadEBSRates` here.** `pkg/ebs` already has its own
  `LoadRates`, and `pkg/lambda`'s override point is `Config.Rates`. Adding
  parallel loaders in this package would have created a *third* way to set the
  same numbers — the shape this pass exists to remove.
- **`pricing.FargateRates` still has no `Load`-settable region.** An override
  file states rates, not provenance, so loaded rates carry the empty `Region`
  and a file that tries to declare one is rejected like any other unknown
  field. If per-region override files are ever wanted, that is a format
  decision, not a default.
