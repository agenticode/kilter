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
