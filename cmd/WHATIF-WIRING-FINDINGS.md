# Q3 — wiring: `pkg/whatif` becomes reachable from the binary

`pkg/whatif` (reasoning-engine unit 5) shipped with a complete CLI and API
specification and **zero reachability**: a user who built `kilter` could not run
a what-if, could not see a proposal, and could not find out that either
existed. That is the third time a unit has shipped its wiring as a FINDINGS
note — PR#29 (j1-wire) and PR#41 (p2-wire2) each cleaned up one round of it.
This closes the third.

Two commands now do:

```
kilter whatif    --demo <archetype> --set <axis>=<value>   replay → delta → gate → proposal
kilter proposals list | show | reject                      the proposal record and its audit trail
```

**Status:** `gofmt -l ./cmd` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./cmd/...` and `go test -race -short ./...` all green.
**`go.mod` and `go.sum` are unchanged**; every import added is stdlib or
intra-repo. Nothing under `pkg/**`, `docs/` or `deploy/` was touched.

| | |
|---|---|
| New production code | 1,322 lines (`cmd/kilter/whatif.go`, `cmd/kilter/proposals.go`) |
| New tests | 1,229 lines, 30 test functions |
| New fixture | `cmd/kilter/testdata/whatif-bursty.json` (5,220 bytes, golden) |
| Core edit | `cmd/kilter/main.go`: two `case` arms, two usage lines, two doc lines |
| Coverage | `cmd/kilter` 45.1 % → **54.2 %** |

---

## 1. What is reachable now

### 1.1 `kilter whatif` — the counterfactual

```
$ kilter whatif --demo bursty --set cpu-headroom=1.05

kilter whatif — demo-bursty, bursty trace, 7 days, 2 workloads
window 2026-01-05T00:00:00Z .. 2026-01-12T00:00:00Z  horizon 24h0m0s  interval 24h0m0s
set: cpu-headroom=1.05

  what changed
    cpu-headroom 1.15 → 1.05

  baseline 69955a2cdd2004fd
    scored 14  decisions 12  refusals 2 (good 0, idle 2)
    safety      memViolations 0  cpuStarvation 0  oomKills 0
    efficiency  oracleGap 92.9%  (applied 23.4%)  claimed/realized 1.00
    stability   flipRate 0.000  flips 0
    regret      $2.90  (resource $2.90 + risk $0.00)

  candidate f8e7e12e930bfae8
    …
    regret      $2.77  (resource $2.77 + risk $0.00)

  delta (candidate − baseline; negative is better everywhere except decisions)
    safety      memViolations 0  cpuStarvation 0
    regret      -$0.13  (-4.4%)  resource -$0.13  risk +$0.00
    efficiency  oracleGap -4.1 pts  (applied -4.8 pts)  forgone +$0.00
    behaviour   decisions 0  refusals 0 (idle 0)  flipRate +0.000
    projected   -$0.56/month, extrapolated from 168.0h of history

  Gate
    ACCEPTED: the candidate dominates on the terms §4.6 defines
      win: regret $2.9026 → $2.7736
      win: oracle gap 92.890% → 88.761%
    required regret improvement $0.03
```

Two replays of one recorded history through the **real** `pkg/backtest`
harness, differenced, and put through §4.6's seven dominance rules. `--json`
writes `Result.Encode()` verbatim, which is what
`testdata/whatif-bursty.json` pins.

Implemented as specified in `pkg/whatif/FINDINGS.md` → "CLI and API surface":
`--from 30d|RFC3339`, `--to`, `--horizon`, `--interval`, `--starvation`,
`--policy`, `--candidate`, repeatable `--set`, `--enforce-refusals`,
`--derive-costs`, `--json`, `--propose`, `--rationale`, and the CI exit code
(spelled `--fail-on-no-improvement`, the name the spec uses). Three flags are
additions and each has a reason below: `--store`, `--author-id`, `--now`.

### 1.2 `kilter proposals` — the record and its audit trail

```
$ kilter proposals list --store ./proposals.json

kilter proposals — 1 record(s) in ./proposals.json

  ID                STATE  CLUSTER      REGRET         PROJECTED/MO  CHANGES       CREATED               AUTHOR
  ────────────────  ─────  ───────────  ─────────────  ────────────  ────────────  ────────────────────  ─────────────────
  db58c9f1f46cec54  gated  demo-bursty  $2.90 → $2.77  -$0.56        cpu-headroom  2026-08-26T12:00:00Z  system:kilter-cli

$ kilter proposals show --store ./proposals.json db58c9f1f46cec54
proposal db58c9f1f46cec54 — gated
{ … Proposal.Encode() verbatim … }

audit trail
  2026-08-26T12:00:00Z  (new)     → draft     by system:kilter-cli   filed
  2026-08-26T12:00:00Z  draft     → gated     by system:gate         gate passed
```

`list` prints `Store.List()` / `ListState()` in the order they arrive — already
sorted by `(CreatedAt, ID)` — and does not re-sort. `show` prints
`record.Proposal().Encode()` verbatim plus `record.History()` and
`record.Approval()` when present. `--json` on either emits the package's own
`Record` wire form, which is the same shape `GET /api/v1/proposals[/{id}]` must
serve.

---

## 2. The core change, and the test edits it forced: **none**

`cmd/kilter/main.go` grew two `case` arms, two lines of `rootUsage` and two
lines of package doc. **No existing test changed, and none needed to.**

That was checked rather than assumed. The two counts the brief warned about:

| Assertion | Where | Why it did not move |
|---|---|---|
| `len(env.Plans) == 5`, "one per wired domain" | `cmd/kilter/domains_test.go:352` | counts **domains**, not subcommands; `whatif` and `proposals` register no domain |
| the closed set of `--domain` values | `cmd/kilter/domains_test.go:465-468` | `kilter domains`' subcommands, a different dispatcher |

Nothing in `cmd/kilter` asserts over `rootUsage` or over the set of top-level
verbs (`grep -rn "rootUsage" cmd/kilter/*_test.go` is empty). So the honest
statement is: this wiring was additive at the dispatcher and the existing suite
is untouched, not loosened.

---

## 3. What refuses, and why

Five refusals. Each names the seam, exits non-zero, and prints **no** scorecard
— the failure mode this whole file exists to avoid is a number that looks fine.

### 3.1 `whatif --cluster` — snapshot history is not persisted

Same seam `kilter backtest --cluster` refuses on: `pkg/store` keeps only the
LATEST snapshot per cluster (`SaveSnapshot`/`LoadSnapshot` are keyed by
cluster, not by time), and `whatif.Scenario.History` is a
`backtest.SnapshotSource`.

**The refusal matters more here than it does for `backtest`, not less**, and
that is the argument for repeating it rather than deferring to the other
command. `kilter backtest` over an empty replay prints one bad scorecard.
`kilter whatif` prints a *comparison*: two runs over an empty replay agree on
every field, so every delta is `0.00`, `regret $0.00` reads as "perfect", and
the gate's `regret improves by $0.0000, short of the $0.0100 margin` reads as
*"we measured, and the candidate is not better"*. A refusal that produces a
considered-sounding negative verdict is worse than one that produces a
suspicious zero.

`TestWhatIfLiveHistoryRefusesRatherThanComparingTwoEmptyReplays` asserts the
refusal names `pkg/store`, `SaveSnapshotAt`, `Snapshots(cluster, from, to)` and
`backtest.SnapshotSource`, and that none of `regret`, `Gate`, `delta` or
`oracleGap` is printed.

**Wired instead:** `--demo <archetype>` over `backtest.TraceSpec`, exactly as
`pkg/whatif/FINDINGS.md` → "The snapshot-history seam" suggests, so the output
format, the gate and the exit codes can be integrated against known numbers.
The trace start is `backtestEpoch`, the same package constant `kilter backtest`
uses — a replay window that drifts with the wall clock would additionally make
a proposal's fingerprint (which covers the window) change every night.

### 3.2 `whatif --auto-tune=apply` — refused by name, on principle

`pkg/whatif` deferred auto-apply explicitly "not because of budget": apply
needs a **writer**, and a writer is what breaks INV-4's single funnel. The flag
exists — rather than being absent, where `flag provided but not defined` would
teach a reader nothing — and refuses, naming where apply belongs: `pkg/api`, as
a caller that reads a gated proposal, mints an approval as a **configured
operator identity distinct from the tuner**, writes the config, and posts
`applied` with the §4.6 ledger entry.

It is checked **first, before any replay**, so the output cannot read as though
the request was honoured and merely printed instead of applied.
`TestAutoTuneApplyIsRefusedByName` asserts the refusal text and that no
scorecard is emitted.

`--auto-tune=propose` is refused too, for a different reason: the nightly loop
is brain wiring (`NewTuner` at brain start, `Run` on the nightly timer with
`historyEnd` from the newest snapshot, once per cluster), not a CLI verb — and
it needs the same snapshot-history seam §3.1 refuses on. `--auto-tune=off` is
the default and a no-op.

### 3.3 `proposals approve` — a local CLI cannot authenticate a human

This is the sharpest line in the unit, and the one the spec is most explicit
about: *"the actor must come from the authenticated caller, not from a flag …
authentication is the API layer's job and is the one thing that can undo the
guarantee."*

`pkg/whatif` made self-approval **unrepresentable**: `Approval` has no exported
fields, no `UnmarshalJSON`, and the only route to one is `Store.Approve`, which
needs an `*Approver`, which `NewApprover` will not build for a non-human actor.
That is a type-system guarantee resting on exactly one input the type system
cannot supply — proof that a human is on the other end.

A local CLI process has none. The identities within reach — `$USER`,
`os/user.Current`, the uid — describe the **session**, not the presence of a
person, and *everything running in that session inherits them, including unit
8's reasoner*, which is precisely the actor the human-only rule exists to
exclude. An agent with shell access running `kilter proposals approve <id>`
would mint `Actor{Kind: ActorHuman, ID: $USER}`, and because `sameIdentity`
compares IDs, an agent-authored proposal (`agent:kilter-reasoner`) would clear
the author≠approver check and be approved. **That is self-approval with two
extra steps**, and it converts a structural guarantee into a convention that
`sh -c` walks around.

So `approve` refuses, points at
`POST /api/v1/proposals/{id}/approvals (authWrite, HUMAN token tier only)`, and
names the thing the CLI *can* do (`reject`).
`TestProposalsApproveIsRefusedByName` additionally asserts the refused command
**did not rewrite the store** and the record is still `gated`.

### 3.4 `proposals applied` — nothing here writes config or a ledger entry

`MarkApplied` records, after the fact, that a change landed, and §4.6 requires
a `LedgerEntry` in the same breath (proposal ID, both policy hashes, the
claimed `Delta.ProjectedMonthlyUSD`) — that entry is what later lets the
existing claimed-vs-measured join score the tuner itself. This binary writes
neither, so recording `applied` here would put an unbacked fact into an audit
trail whose whole value is that its facts are backed.

It is also **unreachable**: only an approved proposal can be applied, and §3.3
means nothing in the CLI can produce one. A verb that could only ever return
*"proposal X is gated; only an approved proposal can be applied"* is better
replaced by the reason it can never do anything else.

### 3.5 `--propose` / `proposals *` without `--store PATH`

`Store.Create` is what runs the gate and mints the ID, so a proposal that is
not stored is a receipt for a document that does not exist. And a *missing*
store file is an error rather than an empty listing, because "0 proposals" and
"you named a file that is not there" render identically and the first reads as
a fact about the fleet.

Both refusals say the file is a **local artefact** and that the fleet's
proposals belong in `pkg/store`'s bbolt file under a `proposals` bucket.

### 3.6 One more, which is a genuine mismatch between two commands

`enforceDecisionRefusals` **in a policy file is refused by name** for
`kilter whatif`, and the reason is worth recording because the same key is
legal for `kilter backtest`:

* For `kilter backtest` it is part of the **policy** (`cmd/kilter/backtest.go`'s
  `policy.EnforceRefusals`), deliberately — there the question is "should we
  wire `pkg/decision` in?" and an A/B through `Gate` is the honest way to ask.
* For `kilter whatif` it is part of the **yardstick**:
  `whatif.Scenario.EnforceDecisionRefusals` is a *scenario* field shared by both
  replays, precisely so the two sides cannot be scored under different rules.

A policy file that set it would therefore be **silently ignored**, and a
what-if of a policy nobody ran is exactly the artefact this unit exists to
prevent. So the loader refuses it and names `--enforce-refusals`, which applies
to both runs. `TestEnforceRefusalsIsTheYardstickNotThePolicy` covers both flags
and additionally asserts that `--enforce-refusals` reaches **both** sides and
moves **neither** policy hash.

---

## 4. Not letting the CLI become a self-approval path

Hard rule 5 has two halves, and both are tested.

### 4.1 The evaluation path cannot be pointed at the policy under test

`TestTheEvaluationPathCannotBePointedAtThePolicyUnderTest`:

1. **No flag scores a policy against itself.** `--set cpu-headroom=1.15` (the
   shipped value) and `--candidate default` are both refused by
   `Scenario.validate` *before anything is replayed*, and the test asserts no
   scorecard, delta or gate line is printed.
2. **The yardstick is shared, end to end.** The two most different policies the
   envelope allows (p80/1.05× vs p99/1.50× on both dimensions) are run through
   the CLI, and `OracleCostUSD`, `Scored`, `Instants`, `Snapshots`,
   `MemOOMKills`, `StarvationFactor` and the whole `CostModel` must be
   **identical** across the baseline and candidate scorecards, and the oracle
   identical across the two invocations. If the evaluation ever started tracing
   back to the thing under test, the oracle would move with it.

There is no CLI-side scoring code at all: every number printed is
`pkg/backtest`'s output or `whatif.Delta`'s arithmetic over two of its
scorecards.

### 4.2 The proposer cannot supply the verdict

`Result.Spec` deliberately omits the `GateResult` and `Store.Create` runs
`Decide` itself. `TestTheProposerCannotSupplyTheVerdict` files a candidate the
gate rejects and asserts it lands in `rejected` — and that it is **still
filed**, because a rejected proposal is the record of a question that was asked
and answered, and discarding it would make a loop that ran look like one that
never did.

### 4.3 The author is never a human, and `--author-id` can only restrict

`pkg/whatif/FINDINGS.md` says the CLI actor is `{human, <authenticated
identity>}` and, in the same breath, "**never** synthesize a human identity for
an automated caller". Since §3.3 establishes the CLI has no authenticated
identity, the first half is unavailable and the second half decides it:
`--propose` files as `Actor{Kind: ActorSystem, ID: "kilter-cli"}`.

`--author-id` overrides the **ID only**, and it is a flag while the approver
identity is not, for a reason that is structural rather than stylistic:
`whatif.sameIdentity` compares IDs and **ignores `Kind`** (deliberately —
otherwise `agent:alice` could file what `human:alice` approves). So an operator
who names themselves as author is *blocked* from later approving that proposal
through the authenticated funnel. **Naming yourself can only ever remove a
capability.** A flag whose worst case is a denial is safe in a way a flag that
grants one never is.

`TestAuthorIDCanOnlyRestrict` proves both directions: `human:Alice` is refused
on the proposal filed by `system:alice` (case-insensitively, across kinds), and
`human:bob` is not.

`TestNoCLIPathReachesApprovedOrApplied` states the aggregate property: after
any sequence of CLI commands, the store on disk contains no `"approved"`, no
`"applied"`, no `"approval"` and no `"kind": "human"`.

---

## 5. Determinism

* **No clock in anything computed.** The replay window comes from the history:
  `backtestEpoch` is a constant, `--to` defaults to the end of the recorded
  history, and a relative `--from 30d` is measured back from the **newest
  snapshot**, never `time.Now()` — with the anchor printed, so a reader can see
  which it was. `whatif.Scenario` takes no clock at all, so resolving this
  window is the CLI's whole job here.
* **`--now` exists and touches nothing computed.** It feeds `CreatedAt` and the
  audit transitions and nothing else. The wall clock is read once, at the edge
  of the program, and passed inward as a `whatif.Clock`; `pkg/whatif` never
  calls `time.Now` itself and errors rather than defaulting.
* **A window past either end of the history is clamped, and the clamp is
  reported.** A run claiming a 30-day window over a 7-day trace is a lie by
  omission — the same discipline `kilter domains --rds-fixture` uses for its
  CloudWatch retention clamp. Asserted by
  `TestWhatIfWindowComesFromTheHistoryNotTheWallClock`.
* **Byte-identical across repeated runs in ONE process.** Go randomizes map
  iteration on every `range`, so in-process repetition is the real test:
  `TestWhatIfOutputIsByteIdenticalAcrossRuns` repeats a noisy 3-workload
  comparison six times in text and six in JSON. The same test permutes the
  `--set` flags and requires everything except the echoed `set:` line to be
  identical — the echo records what the operator typed; nothing computed from
  it may.
* **The golden file.** `testdata/whatif-bursty.json` pins `Result.Encode()`
  byte for byte, with the same `-update-fixtures` idiom `TestWriteRDSFixture`
  and `TestWriteDomainFixtures` use. It additionally asserts the pinned
  comparison is an **accepted** one, so a regression in the gate is visible
  here rather than silently flipping a rejection to a different rejection.
* **The store file round-trips byte-stably.** `Store.Snapshot()` is
  byte-identical for identical state; `TestProposalStoreRoundTripIsByteStable`
  loads and rewrites four times and requires no byte to move, then re-files the
  identical proposal at a *different* `--now` and asserts the store does not
  grow — proposals are content-addressed and `CreatedAt` is outside the
  fingerprint, so a nightly loop that re-derives the same candidate is
  idempotent rather than a duplicate factory.
* **No map iteration in any output.** `whatifUsage()` walks `whatif.AllAxes`,
  `writeScorecard` sorts refusal codes (reused from `backtest.go`), `List()`
  arrives sorted and is not re-sorted, and `Result.Changes` is in `AllAxes`
  order. `TestWhatIfHelpQuotesTheEnforcedBounds` asserts the help text is
  byte-stable across five renders and in `AllAxes` order.
* **No network, no cluster, no credential** on any path, including tests.

---

## 6. Adapters written, and the mismatch that forced each

### 6.1 `applyAxisSets` — because `Axis.get`/`Axis.set` are unexported

`whatif.Axis` is exported, `Known()` is exported, `HardBounds()` is exported —
but the projection from an axis onto a policy field is not. So `cmd/` restates
the five-field mapping, which is exactly the kind of duplication that goes
wrong silently: set `MemoryHeadroom` when the caller said `cpu-headroom` and
everything downstream is internally consistent and wrong.

**The cross-check is `pkg/whatif`'s own projection.**
`TestSetMovesTheAxisPkgWhatifThinksItMoves` runs each axis through the CLI and
asserts `Result.Changes` — computed by `changesBetween`, which uses `Axis.get`
— names the axis the flag named and carries the value the flag carried. Then
all five at once, asserting exactly five moved, in `AllAxes` order. A mis-mapped
field fails the build's tests rather than quietly tuning the wrong knob under
the right name.

`--set` values are additionally checked against `whatif.HardBounds()` at the
CLI, and the gate re-checks the candidate against the (narrower) declared
envelope. Producer and checker are different code on purpose, which is the
package's own argument for checking the envelope at acceptance and not only at
generation.

### 6.2 `loadWhatIfPolicy` — reusing `backtest`'s loader, refusing one field

The policy-file format is `cmd/kilter/backtest.go`'s `policyFile`: pointer
fields overlaid onto package defaults (so absent means default, not zero),
`DisallowUnknownFields`, and a bare number where a duration belongs rejected by
name. Reusing it rather than writing a second format is deliberate — a policy
file should mean the same thing to both commands.

Detecting whether the file *set* `enforceDecisionRefusals` (§3.6) is done by
loading twice with opposite defaults: if the file pinned the field both agree,
if it left the field out they differ. That is cheaper and less brittle than a
second parser for one boolean, and it is commented as such at the call site.

### 6.3 The proposal store is a file, and it is not trusted

`pkg/whatif` owns no file, no bucket and no schema migration by design:
`Snapshot()` and `Load()` move bytes and "cmd/ decides where they live". So
`--store PATH` is a local JSON file — the choice the package explicitly
delegates, and one that touches neither `pkg/store` nor `pkg/api`.

The file is **not** trusted on the way back in. `whatif.Load` revalidates every
record: the ID must be the fingerprint the contents actually hash to, an
approved record must carry an approval bound to that fingerprint *and* that
verdict granted by a human who is not the author, a gated record must actually
have passed the gate, and unknown fields are rejected.
`TestATamperedProposalStoreDoesNotLoad` runs five concrete edits an attacker
with write access would make — assert `"state": "approved"`, flip the gate
verdict, corrupt the regret numbers, invent a state, splice in an extra record
— and each fails to load with the store named. That property is what makes a
plain file an acceptable home for this artefact at all.

Writes go to a temp file in the same directory and are renamed over the target:
a store truncated half-way through a write is a store that no longer loads, and
this file is the only record that a proposal was ever gated. Mode `0600`,
because it carries approvals and an audit trail.

---

## 7. What the API-route unit must still do

Not built here. `pkg/api` and `pkg/store` are another agent's this cycle, and
the six routes plus the nightly tuner wiring are explicitly out of scope. What
this unit learned that whoever builds them will need:

### 7.1 The six routes

```
GET  /api/v1/clusters/{id}/whatif?from=&to=&horizon=&candidate=<policy-ref>   → Result JSON
GET  /api/v1/proposals?cluster=&state=                                        → []Record
GET  /api/v1/proposals/{id}                                                   → Record
POST /api/v1/proposals                    (authWrite) body: Spec-ish          → Record
POST /api/v1/proposals/{id}/approvals     (authWrite, HUMAN token tier only)  → Record
POST /api/v1/proposals/{id}/rejections    (authWrite)                         → Record
POST /api/v1/proposals/{id}/applied       (authWrite, system)                 → Record
```

* **`GET .../whatif` is blocked on the same seam as `--cluster`.** It cannot be
  served over the one snapshot `pkg/store` keeps, for the reason in §3.1. It is
  blocked on nothing else: `whatif.Scenario` is a struct literal and `Run()` is
  one call.
* **The two GETs on `/proposals` are pure projections** of `Store.List()` /
  `Get()`. `Record` has `MarshalJSON`, so the wire form is the package's, not a
  hand-rolled one — `cmd/kilter/proposals.go` emits it verbatim and asserts as
  much with a `var _ json.Marshaler = (*whatif.Record)(nil)`. Lift the
  rendering, do not re-derive it.
* **`POST /proposals` must not accept a verdict.** The body is a `Spec`, which
  carries scorecards and no `GateResult`; `Store.Create` runs `Decide` itself.
  If the handler ever grows a "gate" field in its request body, that is the
  regression.
* **`POST .../approvals` is the whole security boundary.** Everything the CLI
  refuses in §3.3 is available to an API that authenticates. The token tier
  must be one the MCP server (§6) and the reasoner (§5) do not hold, and the
  actor must come from the token, never from the body — a `{"by": {"kind":
  "human"}}` field in a request body is `--author-id` pointed the dangerous
  way. `whatif.NewApprover` is the only constructor; if any handler builds an
  `Actor{Kind: ActorHuman}` from request-supplied data, the guarantee is gone.
* **`POST .../applied` owes a `LedgerEntry`** in the same transaction: proposal
  ID, both policy hashes, the claimed `Delta.ProjectedMonthlyUSD`. Without it
  the claimed-vs-measured join can never score the tuner itself, which is the
  only mechanism that would ever catch a tuner that is confidently wrong.

### 7.2 Persistence

`Store.Snapshot()` / `whatif.Load()` move the whole store as bytes, and both
are already round-trip and byte-identity tested inside the package. The bbolt
`proposals` bucket is a one-key-per-cluster (or one-blob) write of those bytes;
the store is capped at 1,000 records, so size is bounded by construction. The
CLI's file store is the same bytes — a `--store` file can be read by
`whatif.Load` and vice versa, so the two are interchangeable for testing.

Call `store.Sweep(clock)` on the brain's existing housekeeping timer so expired
approvals move to `expired` rather than lingering as `approved`.
`MarkApplied` checks expiry independently, so an unswept store is safe, just
untidy.

### 7.3 Two prerequisites shared with earlier units

* **Snapshot history** (§3.1) is the same seam `kilter backtest --cluster` and
  the explain/why-cost routes are waiting on — `cmd/WIRING-FINDINGS.md` §6.2
  and §6.3. One piece of work unblocks four features.
* **The nightly tuner** needs a `Scenario` pre-filled with `History`,
  `Evidence`, `Catalog`, `Scoring` and `Cluster`, run once per cluster, with
  `historyEnd` from the newest snapshot — so it needs snapshot history too.
  Construct `whatif.NewTuner(cfg, store)` **unconditionally** at brain start so
  a disabled-but-invalid config is a startup error rather than a 3am surprise,
  and surface `TunerReport.Truncated` wherever the operator looks: a search
  that silently covered half the grid reads exactly like one that covered all
  of it.

---

## 8. Smaller notes

* **`--propose` files `Target{Cluster: …}` only.** `Target` carries `Namespace`
  and `Class` and they are inside the fingerprint, but nothing narrows the
  *replay* to them — `backtest.Harness` scores a whole cluster. Until scoping
  the harness lands (a `pkg/backtest` change), a narrowed target would be
  documentation printed on top of a cluster-wide measurement, which is why the
  tuner only ever files a cluster target and why this command does the same.
  No flag pretends otherwise.
* **`--propose` supplies no evidence IDs.** `Spec.EvidenceIDs` is the citation
  set, and the CLI has no dossier to cite from — the trace is synthetic.
  Passing the trace's own identifiers would be a citation that resolves to
  nothing an operator can look up. Left empty rather than filled with
  plausible-looking strings.
* **The envelope and tolerance are the shipped defaults.**
  `whatif.DefaultEnvelope()` (narrower than the hard bounds on every axis) and
  `DefaultTolerance()`. There is no `--envelope` flag: `Envelope.Validate`
  errors rather than clamping, so a file-supplied envelope is a config surface
  that wants the same "declared once at brain start" treatment the tuner's
  does, not a per-invocation flag.
* **`KindAnnotationChange` is not reachable and no flag offers it.**
  `Spec.normalize` rejects it with a reason; an annotation proposal's gate is
  not a backtest scorecard.
* **Coverage is 54.2 % for `cmd/kilter` as a whole.** The two new files are
  exercised end to end; the untested remainder is the pre-existing collectors
  and the `brain`/`agent`/`controller` long-running paths.
