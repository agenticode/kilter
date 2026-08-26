# pkg/whatif — reasoning-engine unit 5

What-if, the proposal object, its store, its gate, and the nightly tuner.

Design reference: `docs/design/reasoning-engine.md` §4.5 (counterfactuals), §4.6
(closed-loop policy tuning), §5.2/§5.8 (the posture an agent must find when it
gets here in unit 8), §9 unit 5, and Appendix A **INV-4**.

Verified at every step with
`gofmt -w ./pkg/whatif && go vet ./... && go build ./... && go test -race -count=1 ./pkg/whatif/...`
and, at the end, the full `go test -race -short ./...` — all packages green.
Package coverage 92.0% of statements. Stdlib only; `go.mod`/`go.sum` untouched.

CLI and API wiring is **not** in this unit. It is specified in
[CLI and API surface](#cli-and-api-surface-what-a-later-unit-must-do) below.

---

## What is here

| File | Contents |
|---|---|
| `policy.go` | `Policy` (the recommend/plan/decision triple), the closed `Axis` set, projections onto each axis |
| `envelope.go` | `Range`, `Envelope`, the **hard bounds** no config may widen, clamp/contains/violations |
| `gate.go` | `Tolerance`, `GateInput`, `Rule`, `GateResult`, **`Decide`** — §4.6's dominance rules |
| `whatif.go` | `Scenario` → `Result`: two replays through the real `pkg/backtest`, plus the delta and the verdict |
| `delta.go` | `Diff`: candidate-minus-baseline on every scorecard axis; the monthly projection |
| `money.go` | `sumUSD` (sorted, total), `round6`, `pctChange` |
| `proposal.go` | `Kind`, `Target`, `Change`, **`Proposal`**, its fingerprint, `Spec` |
| `approval.go` | `Actor`, **`Approval`** (no exported fields), **`Approver`** (human-only capability) |
| `store.go` | `State`, the transition table, `Record`, **`Store`**, the persistence seam |
| `tuner.go` | `TunerConfig` (**off by default**), the bounded grid, `Tuner`, `TunerReport` |
| `clock.go` | `Clock` — the only way this package learns the time |

~2,000 lines of implementation, ~4,000 of tests, 84 tests and 4 fuzz targets.

### What is deliberately *not* here

No executor, no Kubernetes client, no config writer, no file, no bucket, no
network call. The furthest state a proposal reaches inside this package is
`approved`; `applied` is a fact something else reports after the event. If a
future change adds a writer here, it has broken the unit.

---

## The dominance rules, as implemented

`Decide(GateInput) GateResult` evaluates seven rules in a fixed order, so the
reason list is deterministic. Each is a hard gate: there is no scoring, no
weighting, and no way for a large win on one axis to buy a regression on
another. That asymmetry is the point — a policy change is a one-way door onto a
production cluster and the loss function is not symmetric.

| # | Rule | What it requires | Where it is tested |
|---|---|---|---|
| 1 | `RuleWellFormed` | Both scorecards present; envelope validates; candidate policy is one the engine could actually run | `TestGateFailsClosedOnNilScorecards`, `TestGateRejectsAnInvalidEnvelope`, `TestGateRejectsAnUnrunnableCandidatePolicy` |
| 2 | `RuleEnvelope` | The candidate lies inside the declared search space — checked at **acceptance**, not only at generation | `TestEnvelopeIsEnforcedAtAcceptanceNotOnlyAtGeneration`, `FuzzTunerStaysInsideItsEnvelope` |
| 3 | `RuleIdentity` | Each scorecard is the scorecard *of* the policy it is offered for, and the candidate is not the incumbent restated | `TestScorecardMustBelongToTheProposedPolicy` (3 sub-cases) |
| 4 | `RuleAdmissible` | Delegated to `backtest.Gate` with `AllowSafetyRegression` pinned false: comparable yardsticks, agreeing ground truth, **no rise in `MemViolations` or `CPUStarvation`**, no rise in regret, no rise in flip rate | `TestSafetyIsNotForSaleAtAnyPrice`, `FuzzSafetyForEfficiencyIsNeverAccepted`, `TestToleranceHasNoSafetyEscapeHatch`, `TestGateStopsAtIncomparability` |
| 5 | `RuleCoverage` | The two runs scored the same number of pairs over the same number of instants | `TestCoverageMismatchVoidsTheComparison` |
| 6 | `RuleNotRefusalCollapse` | A candidate that decides nothing, where the incumbent decided something, is rejected outright | `TestRefuseEverythingCannotWin`, `FuzzRefuseEverythingCannotWin` |
| 7 | `RuleStrictImprovement` | Regret must improve by `max(MinRegretImprovementUSD, |baseline regret| × MinRegretImprovementPct)` | `TestTiesAreNotProposals`, `TestGateRejectsAnImprovementBelowTheNoiseMargin`, `TestGateNeverReportsANonFiniteMargin` |

Rules 5–7 are skipped when 4 failed: `backtest.Gate` stops at the first
incomparability, so once the yardsticks disagree every metric below is
comparing numbers that were never on the same scale.
`TestGateIsDeterministic` asserts the reason list is byte-stable across 50 runs
on a candidate that breaks several rules at once.

### The two rules the brief singles out

**"Refuse everything" cannot win.** `pkg/backtest` already prices refusals — a
refusal leaves the current sizing in force and is charged for it — so a
decide-nothing policy on an over-provisioned cluster loses on regret without
any help from here. That is the obvious case, and it is not what rule 6 is for.
Rule 6 exists for the *subtle* case, which `TestRefuseEverythingCannotWin`
constructs explicitly: a window where the incumbent's resizes really were
harmful, so doing nothing is safer, cheaper **and** stabler, and every
admissibility rule is satisfied. The test asserts `backtest.Gate` *would*
have admitted it, then asserts `Decide` rejects it — so the test proves this
package adds something rather than restating `pkg/backtest`. The reasoning:
"the engine improves by switching itself off" is a real finding that a human
should read, not a policy tweak to auto-file at 3am. `FuzzRefuseEverything
CannotWin` generalizes it over scorecard shapes and tolerances.

**A safety-for-efficiency trade is never silently accepted.** The structural
answer is that `whatif.Tolerance` **has no field that can disable a rule**.
`backtest.Tolerance` carries `AllowSafetyRegression` — deliberately, for an
operator plotting the risk/cost curve by hand with `kilter backtest` — and this
package does not re-export it. `Tolerance.backtestTolerance()` hardcodes
`AllowSafetyRegression: false` and pins the regret slack at zero.
`TestToleranceHasNoSafetyEscapeHatch` asserts by reflection that `Tolerance`
has grown no bool field, and that no value of the four float fields (including
NaN, ±Inf and negatives) can flip the delegated flag or widen the delegated
slacks. `FuzzSafetyForEfficiencyIsNeverAccepted` then fuzzes the tolerance
against a candidate made irresistible on every other axis (regret −$1M, zero
oracle gap, zero flip rate) and one container worse on safety: it must be
rejected, on `RuleAdmissible`, every time.

A rejected proposal still reports `Wins`. An approver reading a rejection needs
to see what the candidate was buying, or every rejection looks identical —
that is `TestSafetyIsNotForSaleAtAnyPrice`'s last assertion.

---

## Independence from the policy under test, structurally

The brief's sharpest constraint: *if your evaluation path can be traced back to
the policy under test, the number is worthless.* Four separate mechanisms, none
of which is a convention:

**1. No scoring code exists here.** This package computes no quality number of
its own. Every number in a `Result`, a `Delta` and a `GateResult` is either
`pkg/backtest`'s output verbatim or arithmetic over two of its scorecards.
`pkg/backtest`'s oracle is computed from future usage alone (`oracle.go`), the
scored set is fixed before any policy runs, and both properties have tests in
that package (`TestOracleIsIndependentOfThePolicyUnderTest`,
`TestOracleIsALowerBound`). Reimplementing scoring here would have measured a
policy that never runs; §4.5 says the same thing — "zero new decision logic".

**2. One harness template, parameterized only by the policy.**
`Scenario.harness(Policy)` is a single struct literal. Every field except
`Rec`, `Plan` and `Decision` is copied from the `Scenario`, so the two replays
differ in exactly the three configs that *are* the policy under test, and in
nothing else — same snapshots, same evidence store, same price catalog, same
horizon, same decision cadence, same starvation factor, same refusal
enforcement. `TestBothSidesAreScoredByTheSameYardstick` blanks the policy
triple out of both harnesses and `DeepEqual`s the remainder, and additionally
asserts `backtest.Harness` still has exactly 8 fields — so a new field added
upstream forces a deliberate decision about whether it is yardstick or policy,
instead of silently becoming a way to score the two sides differently.

**3. The gate re-checks the yardstick from the scorecards.** Even if the
harness construction were wrong, `backtest.Gate` rule 1 compares cluster,
window, horizon, cadence, starvation factor and cost model field by field, and
rule 2 requires the ground-truth `MemOOMKills` counters to agree. The producer
and the checker are different code with different bugs, on purpose.

**4. An end-to-end assertion on real traces.**
`TestTheOracleIsIndependentOfThePolicyUnderTest` runs the two most different
policies the envelope allows (aggressive: p80/1.05× headroom; conservative:
p99/1.50×) over a bursty trace and asserts `OracleCostUSD`, `Scored`,
`MemOOMKills`, `Snapshots` and `Instants` are all *identical* while the policy
hashes differ. If the evaluation ever started tracing back to the thing under
test, the oracle would move with it and this fails.

**And the proposer never supplies the verdict.** `Spec` carries scorecards, not
a `GateResult`. `Store.Create` runs `Decide` itself and sets the state from its
own answer (`TestWhatIfResultFilesAsAProposal` asserts the store's verdict
matches the what-if's, since they are the same function over the same
evidence). A hostile caller — including unit 8's LLM — hands over evidence and
receives a judgment; it cannot hand over a judgment.

---

## Approval is structural, not procedural

The brief: *construct the types so that self-approval is unrepresentable, and
prove it with a test that tries and fails.* The construction, in layers:

1. **`Approval` has no exported fields.** `whatif.Approval{...}` does not
   compile outside this package. Asserted by reflection in
   `TestApprovalHasNoExportedFields`, so a later "convenience" export fails the
   build's tests rather than quietly removing the property.
2. **`Approval` has `MarshalJSON` but deliberately no `UnmarshalJSON`.** The
   obvious forgery — `json.Unmarshal(hostileBytes, &ap)` — therefore produces
   the *zero* Approval, not a valid one. `TestSelfApprovalIsUnrepresentable/an
   approval cannot be forged by json` asserts exactly that, and that a zero
   approval is never `Live`.
3. **`reflect` cannot set an unexported field.** Asserted (`CanSet() == false`)
   in the same test.
4. **Minting requires a capability that a non-human cannot obtain.**
   `NewApprover` returns `ErrNotAnApprover` for `ActorAgent`, `ActorTuner` and
   `ActorSystem`. Unit 8's "hostile prompt attempts to self-approve" therefore
   terminates at a constructor, not at a policy check somewhere downstream.
5. **The only exported route to an Approval is `Store.Approve`,** which
   requires an `*Approver`. `TestOnlyStoreApproveMintsAnApproval` parses the
   package's own AST and fails if any other exported function returns an
   `Approval`. Consequence: an approval that is not in an audit history cannot
   exist.
6. **Author ≠ approver, ignoring `Kind`.** `sameIdentity` compares IDs
   case-insensitively and *does not* compare `Kind`, because otherwise
   `agent:alice` could file what `human:alice` approves — self-approval in a
   costume. Tested directly (`the author changes kind and approves`).
7. **The approval is bound to the proposal's fingerprint and to the gate
   verdict hash.** The fingerprint covers the author, so an approval cannot be
   replayed onto a byte-identical proposal filed by somebody else — which, if
   that somebody were the approver, would be self-approval by replay.
8. **The state machine has no other edge.** `transitions` is a table, not a
   scatter of ifs. `TestOnlyGatedAndApprovedReachesApplied` walks all 36
   (state × state) pairs and then states the property directly: nothing but
   `approved` reaches `applied`, nothing but `gated` reaches `approved`. That
   is INV-4, enforced in one place with no second writer.
9. **The tuner cannot author as a human.** `NewTuner` rejects a human `Author`
   — otherwise a person could approve what the loop wrote and rule 6 would be
   satisfied by a lie (`TestTunerCannotAuthorAsAHuman`).

### The file is bytes, so there is a second lock

Go's type system does not protect a persisted store. `Record.UnmarshalJSON`
therefore revalidates everything: the ID must be the fingerprint the contents
actually hash to; an `approved`/`applied`/`expired` record must carry an
approval bound to that fingerprint *and* that verdict, granted by a human who
is not the author; a `gated` record must actually have passed the gate and
carry no approval; unknown fields are rejected (`DisallowUnknownFields`); the
momentary `draft` state is not storable at all.
`TestATamperedStoreDoesNotLoad` runs seven concrete edits an attacker with
write access would make (swap the author, swap the approver, demote the
approver to an agent, flip the verdict, improve the regret numbers, add an
`"override": true` field, invent a state) and
`TestAnApprovedStateCannotBeAssertedInStorage` simply writes `"approved"` over
`"gated"`. All eight fail to load.

**Residual hole, stated rather than pretended away:** `unsafe` can write an
unexported field, and code *inside* this package can construct an `Approval`
directly — which is what its own tests do. Neither is reachable by an API
caller, a CLI user or an LLM agent, and neither is within any Go type system's
reach. If that ever needs closing it wants a signature (an HMAC over the
approval wire with a key the reasoner cannot read), not a bigger type.

---

## Determinism

Same history + same candidate ⇒ byte-identical proposal.

* **No clock in the pure logic.** `Clock` is an argument, never an ambient
  call, and it is never defaulted to `time.Now` — a caller who forgets one gets
  an error, not a silently non-reproducible proposal.
  `TestNoAmbientInputsInThePureLogic` walks the package AST and fails on any
  use of `time.Now/Since/Until/Tick/After`, any `rand`, or `os.Getenv/
  LookupEnv/Environ/ReadFile/Open` in a non-test file.
* **The replay window comes from the history, never the wall clock.**
  `Tuner.Run` takes `historyEnd` explicitly and rejects a zero value
  (`TestTunerNeedsHistoryNotAWallClock`). `Scenario` takes no clock at all;
  `TestWhatIfIgnoresTheWallClock` runs the same scenario at two wall-clock
  instants and demands identical bytes.
* **The fingerprint excludes `CreatedAt` and nothing else.** Filing the same
  proposal three days later is the same proposal, so an approval bound to the
  fingerprint stays meaningful; but nine separate content mutations each change
  it (`TestTheFingerprintIgnoresTheClockButNothingElse`).
* **No map iteration in any output.** `AllAxes` is a fixed literal; envelope
  axes enumerate through it; refusal codes are summed over sorted keys; the
  store's `List` sorts by `(CreatedAt, ID)`; evidence IDs are sorted and
  de-duplicated (`TestEvidenceIDsAreSortedAndDeduped` asserts two different
  citation orders produce *one* proposal identity); candidates are sorted by
  policy hash.
* **Sort before summing money.** `sumUSD` sorts before adding, so a total is a
  function of the multiset alone. `TestShufflingHistoryDoesNotChangeThe
  Proposal` is the shuffle test the brief asks for: six deterministic
  permutations of a 6-day noisy regime-change trace (4 workloads, OOM and
  deploy events), each re-run through the whole replay → gate → proposal path,
  each demanding the identical bytes. `TestTunerReportTotalsAreOrder
  Independent` does the same at the aggregate level with magnitudes chosen so
  naive left-to-right addition actually disagrees.

### A bug the fuzzer found

`FuzzSumUSDIsOrderIndependent` produced `+Inf, −Inf, 1, −1` and got `NaN`. Two
things were wrong: the total was neither order-stable *nor* JSON-encodable —
`encoding/json` refuses `NaN` and `±Inf` outright, so a single one would have
turned `Result.Encode` and `Store.Snapshot` into errors, i.e. a proposal that
cannot be written down. `sumUSD` now drops non-finite inputs and saturates a
finite-input overflow to `±math.MaxFloat64` rather than letting `±Inf` escape.
A saturated total is visibly absurd and blows past every gate margin, which is
the right outcome for arithmetic that has already lost its meaning. The failing
input is kept as a corpus seed under `testdata/fuzz/`.

---

## The tuner: off by default, bounded by construction

`DefaultTunerConfig().Enabled` is `false`, and `Enabled` is the zero value —
so *every* `TunerConfig{}` anywhere in the codebase is a disabled tuner
(`TestTunerIsOffByDefault`). `Tuner.Run` returns `ErrTunerDisabled`, a distinct
error so a caller can tell "we looked and found nothing" from "we never looked".

The search is §4.6's grid verbatim: percentile ±2pts, headroom ±5%, soak ±2h.
Coordinate-wise by default (2n+1 candidates); `FullFactorial` is available and
truncated at `MaxCandidates` with the dropped count **reported in the
`TunerReport`, never silently**.

Bounds are enforced in three layers, each of which the fuzz test exercises:

1. `hardEnvelope` — absolute limits no config file, tuner, agent or API caller
   may widen. `Envelope.Validate` *errors* rather than clamping, because an
   operator who wrote `cpu-headroom: [0.5, 3.0]` has asked for something this
   package will not do, and quietly narrowing it would leave them believing a
   search ran that never did. `HardBounds()` returns a copy —
   `TestHardBoundsCannotBeWidenedByTheCaller` mutates the returned map and
   asserts the real limits are untouched.
2. `Envelope.Clamp` at candidate generation. The concrete corner this exists
   for: memory percentile 0.99 + a 0.02 step is 1.01, which `recommend.New`
   rejects outright (`TestMemoryPercentileCannotWalkPastOne`).
3. `Envelope.Contains` again at acceptance, inside `Decide` — because the
   producer of a candidate is untrusted, and a proposal that arrived by any
   route at all must still be in-envelope.

`FuzzTunerStaysInsideItsEnvelope` fuzzes the base policy (including absurd
values like `1e9` and `MaxFloat64`), the three step sizes, the envelope shape
and the factorial flag, and asserts on every emitted candidate: inside the
envelope, inside the hard bounds, runnable by `recommend.New`, and under the
64-candidate cap. 25s of fuzzing, ~390k execs, clean. Config errors (a step
wider than its axis, a degenerate envelope) are refusals, never silent
widenings — `TestTunerConfigRejectsAnUnboundedSearch` covers eleven of them and
additionally asserts a *disabled* tuner with a broken config also fails at
construction, so nobody discovers it on the night they enable the loop.

`TestTunerRunIsDeterministicAndIdempotent` runs two independent nightly cycles
over one trace and byte-compares the resulting stores, then re-runs the same
night against one store and asserts it does not grow — proposals are
content-addressed, so a nightly loop that re-derives the same candidate is
idempotent rather than a duplicate factory.

---

## CLI and API surface: what a later unit must do

Not built here (out of scope — `cmd/` is owned by a concurrent job). Specified
so it can be built without re-deriving anything.

### `kilter whatif`

```
kilter whatif --cluster <id> [--from 30d|RFC3339] [--to RFC3339]
              [--horizon 24h] [--interval 24h] [--starvation 1.0]
              [--policy default|<file.json>] --candidate <file.json>
              [--set cpu-headroom=1.20 --set base-soak=8h]   # repeatable
              [--enforce-refusals] [--derive-costs]
              [--json] [--propose] [--rationale "..."]
```

* Build a `whatif.Scenario`, call `Run()`, print `Result`. `--json` writes
  `Result.Encode()` verbatim (byte-stable, CI-diffable).
* `--from 30d` parses relative to the **newest snapshot in the store**, never
  `time.Now()` — same rule `pkg/backtest`'s FINDINGS states, same reason: a
  window derived from the wall clock makes two runs over identical data
  disagree. `Scenario` takes no clock, so this is the CLI's whole job here.
* `--set <axis>=<value>` is the ergonomic override. The axis names are exactly
  `whatif.AllAxes` (`cpu-percentile`, `memory-percentile`, `cpu-headroom`,
  `memory-headroom`, `base-soak`); reject anything else rather than ignoring
  it. `whatif.HardBounds()` is the source for `--help` text.
* Exit non-zero when `!result.Improved()` under a `--fail-on-no-improvement`
  flag, for CI use.
* `--propose` calls `result.Spec(...)` then `store.Create(actor, spec, clock)`
  and prints the proposal ID. The actor is `{human, <authenticated identity>}`
  for a CLI invocation; **never** synthesize a human identity for an automated
  caller.

### `kilter proposals`

```
kilter proposals list   [--cluster c] [--state gated|approved|rejected|applied|expired] [--json]
kilter proposals show   <id> [--json]
kilter proposals approve <id> [--note "..."]
kilter proposals reject  <id> [--reason "..."]
kilter proposals applied <id> [--note "..."]      # record, after the fact
```

* `list` → `store.List()` / `store.ListState()`; already sorted, so print in
  order and do not re-sort.
* `show` → `record.Proposal().Encode()`, plus `record.History()` as the audit
  trail and `record.Approval()` when present.
* `approve` → `whatif.NewApprover(actor)` then `store.Approve(...)`. **The
  actor must come from the authenticated caller, not from a flag.** This
  package guarantees authorization structure (only a human, never the author);
  *authentication* is the API layer's job and is the one thing that can undo
  the guarantee. In particular the reasoner must never be able to mint an
  `Actor{Kind: ActorHuman}`.
* `applied` → `store.MarkApplied(...)`. Call it *after* the config change has
  actually landed, and record a ledger entry in the same breath (below).

### Brain wiring (`pkg/api`)

```
GET  /api/v1/clusters/{id}/whatif?from=&to=&horizon=&candidate=<policy-ref>   → Result JSON
GET  /api/v1/proposals?cluster=&state=                                        → []Record
GET  /api/v1/proposals/{id}                                                   → Record
POST /api/v1/proposals                    (authWrite) body: Spec-ish          → Record (gated|rejected)
POST /api/v1/proposals/{id}/approvals     (authWrite, HUMAN token tier only)  → Record
POST /api/v1/proposals/{id}/rejections    (authWrite)                         → Record
POST /api/v1/proposals/{id}/applied       (authWrite, system)                 → Record
```

* `Record` marshals with its ID and state; `Store.Snapshot()` / `whatif.Load()`
  move the whole store as bytes. **Persist those bytes in `pkg/store`'s bbolt
  file under a new `proposals` bucket** (one key per cluster, or one blob —
  the store is capped at 1,000 records). This package deliberately owns no
  file, no bucket and no schema migration; it hands you a byte slice that
  round-trips (`TestSnapshotRoundTrips`) and is byte-identical for identical
  state (`TestSnapshotIsByteIdenticalForIdenticalState`).
* Call `store.Sweep(clock)` on the brain's existing housekeeping timer so
  expired approvals move to `expired` rather than lingering as `approved`.
  `MarkApplied` checks expiry independently, so a store that is never swept is
  safe — just untidy.
* **Ledger entry on apply.** §4.6 requires the policy change be recorded like
  any other action, so tuning is auditable and revertible. When `applied` is
  posted, write a `LedgerEntry` with the proposal ID, both policy hashes and
  the claimed `Delta.ProjectedMonthlyUSD` — that is what lets the existing
  claimed-vs-measured join score the tuner itself later.
* **Approval token tier.** `POST .../approvals` must require a token tier that
  the MCP server (§6) and the reasoner (§5) do not hold. Everything else on
  this list is safe for a read tier except the two write posts.

### Nightly tuner wiring

```
--auto-tune=off|propose        # default off; `apply` is NOT implemented — see below
--auto-tune-envelope <file>    # optional; DefaultEnvelope() otherwise
--auto-tune-trailing 30d
```

* Construct `whatif.NewTuner(cfg, store)` unconditionally at brain start (a
  disabled-but-invalid config is then a startup error, not a 3am surprise) and
  call `Run(basePolicy, scenario, historyEnd, clock)` on the nightly timer,
  where `historyEnd` is the newest snapshot's timestamp.
* `Run` needs a `Scenario` pre-filled with `History`, `Evidence`, `Catalog`,
  `Scoring` and `Cluster`; it overwrites the window, horizon, baseline,
  envelope and tolerance itself. Run it once per cluster.
* Surface `TunerReport.Truncated` in whatever the operator sees. A search that
  silently covered half the grid reads exactly like one that covered all of it.

### The snapshot-history seam

`whatif.Scenario.History` is `backtest.SnapshotSource`, which
`pkg/backtest`'s FINDINGS already flags as waiting on snapshot-history
persistence (`pkg/store` keeps only the latest snapshot per cluster). Until
that lands, wire `kilter whatif --demo <archetype>` over `backtest.TraceSpec`
exactly as that unit suggests for `kilter backtest`, so the output format, the
gate and the exit codes can be integrated against known numbers. Nothing else
in this unit is blocked by it.

---

## Deferred, and why

* **`--auto-tune=apply`.** §4.6 item 4 offers an auto-apply mode for
  dominated-in-all-metrics proposals "within hard bounds". Not implemented,
  and not because of budget: auto-apply needs a writer, and this package having
  a writer is the thing that would break INV-4's single funnel. When it is
  built it belongs in `cmd/`/`pkg/api` as a caller that (a) reads a
  `gated` proposal, (b) mints an approval as a *configured operator identity*
  distinct from the tuner, (c) writes the config, (d) posts `applied`. Every
  step is already representable; none of it may live here.
* **`KindAnnotationChange` (§5.2's second proposal tool).** The type is
  declared and `Spec.normalize` rejects it with a reason. An annotation
  proposal's gate is not a backtest scorecard, so accepting one here would mean
  a proposal carrying a `GateResult` that means nothing. Unit 8 should add a
  case with its own evidence type, not reuse this one.
* **Per-namespace and per-class targets.** `Target` carries `Namespace` and
  `Class` and they are inside the fingerprint, but nothing narrows the *replay*
  to them: `backtest.Harness` scores a whole cluster. Scoping the harness is a
  `pkg/backtest` change, which is not this unit's to make. Until then a
  narrowed target is documentation on a cluster-wide measurement, which is why
  the tuner only ever files `Target{Cluster: …}`.
* **Golden proposal files under `testdata/`.** The determinism tests assert
  byte-identity across runs, shuffles and clocks, which is the property that
  matters. A checked-in golden would additionally catch *intentional* format
  changes — worth adding once `cmd/` has settled the rendering, and cheap then
  because `Proposal.Encode()` already produces the bytes.
* **A signed approval.** See the residual `unsafe` hole above. Not worth a key
  management story until there is a threat model that includes in-process
  attackers.
* **Multi-cluster tuning in one run.** `Tuner.Run` handles one cluster. The
  parallelism that would help is across clusters, and — same conclusion
  `pkg/backtest` reached about `--parallel` — that is better solved by calling
  it once per cluster than by making the loop concurrent.

---

## Known tensions (not bugs)

**Rule 6 can reject a true finding.** If the incumbent policy is genuinely
harmful over the replayed window, "decide nothing" may be the correct answer
and the gate refuses to auto-file it. That is deliberate: the operator still
sees the number via `kilter whatif`, nothing is hidden, and the alternative —
a nightly loop that proposes switching the engine off — is a worse default.

**`RuleStrictImprovement` uses regret alone.** Regret already prices the
efficiency/risk trade-off in dollars, and gating additionally on the oracle gap
would double-count the same dollars (`backtest.Gate` documents the same
reasoning and consults the gap only as a tie-break). The consequence is that a
candidate which is *much* safer for a *slightly* higher cost cannot pass this
gate, because regret is the scalar and it went up. That trade is exactly the
one §4.4's `MaxOracleGapIncreasePct` band exists to let an operator make by
hand with `kilter backtest --compare`; it is not one an unattended nightly loop
should make for them.

**`ProjectedMonthlyUSD` assumes next month resembles last month.** It is
labelled a projection in the field comment and in the type doc, and
`TunerReport.TotalProjectedMonthlyUSD` is documented as an upper bound over
*alternatives* — the accepted candidates do not compose, and summing them is
not a claim that they do.
