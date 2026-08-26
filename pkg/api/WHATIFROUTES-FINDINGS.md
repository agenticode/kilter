# T2 — the what-if plane, served

`cmd/WHATIF-WIRING-FINDINGS.md` §7.1 listed the routes and said they were not
built because `pkg/api` was another agent's that cycle, and because
`GET .../whatif` was blocked on snapshot history. The history landed
(`pkg/store.Snapshots`, `api.Brain` already holding the substrate). This
closes the gap.

```
pkg/api/whatifroutes.go        the plane: one counterfactual route, five proposal routes
pkg/api/whatifroutes_test.go   18 test functions
pkg/api/brain.go               +1 line: b.registerWhatIfRoutes(mux)
```

`gofmt -l ./pkg/api` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/api/...` and `go test -race -short ./...` all
green. **`go.mod` and `go.sum` are unchanged**; the two imports added that
`pkg/api` did not already have are `pkg/whatif` — which `pkg/whatif`'s own doc
comment notes nothing imported yet, and which this is the intended direction
for — and `pkg/decision`, already inside `pkg/backtest`'s closure. Both are
intra-repo. No existing test was edited and none needed to be. Coverage for `pkg/api` is **86.5 %** (was
85.2 %).

---

## 1. The routes

Seven patterns are registered; §7.1 called them "the six routes" and listed
seven lines. Five serve, two refuse (§5).

### 1.1 `GET /api/v1/clusters/{id}/whatif` — the counterfactual

```
?from=RFC3339            required
&to=RFC3339              required
&horizon=24h             optional, default 24h
&set=<axis>=<value>      repeatable, at least one required
&enforceRefusals=true    optional, default false
```

`from`/`to` are required for the reason `why-cost` requires them: an answer
computed over a wall-clock default cannot be replayed or compared to a stored
one. There is no `interval` parameter — see §4.3.

The response is the `Simulated` envelope of §2, carrying `whatif.Result`
verbatim under `simulation`.

**This is not a projection.** It replays the cluster's own retained history
twice through `pkg/backtest` — once under the policy this brain runs, once
under the candidate — and is the only route here that computes anything.

The baseline is `Brain.IncumbentPolicy()`: this brain's `recommend.Config` and
`plan.Config` plus `decision.DefaultConfig()`, for the reason `Brain.Backtest`
scores this brain's own config. The question an operator is asking is "would
this be better than what we are running", not "better than the shipped
default". Two exported functions came out of it, `Brain.WhatIf` and
`Brain.IncumbentPolicy`, so `cmd/` and `pkg/mcp` get the refusals without
reimplementing them — the same reason `WhyCost` and `Explain` are exported.

### 1.2 `GET /api/v1/proposals?cluster=&state=` — pure projection

`{"proposals": [ <Record>, … ]}`, the array always present and never `null`.
`Store.List()` / `ListState()` in the order they arrive — already sorted by
`(CreatedAt, ID)` — **not re-sorted**. `state` outside the closed set is a 400
rather than an empty list. `draft` is not in that set: no record rests there,
so accepting it would be a filter that can only ever return nothing.

The container is keyed rather than a bare array (`{"recommendations": …}`,
`{"insights": …}` — the idiom every other list route in this package uses).
The elements inside are `whatif.Record`'s own `MarshalJSON`, byte for byte:
`TestProposalRoutesAreProjectionsOfTheStore` compares the served bytes against
`json.Marshal` of the record fetched straight from the store.

### 1.3 `GET /api/v1/proposals/{id}` — pure projection

The record whole, at the top level, unwrapped: proposal, state, approval and
audit trail, the same bytes `kilter proposals show --json` prints. §7.1 is
explicit that the wire form is the package's, so the only thing this handler
adds is the 404.

### 1.4 `POST /api/v1/proposals` — files one, gate and all

```json
{"cluster":"c1","from":"…","to":"…","horizon":"24h",
 "set":{"memory-headroom":1.05},"enforceRefusals":false,"rationale":"…"}
```

Unknown fields are rejected. Response: 200 and the `Record`.

**The body is the QUESTION, not the answer, and this is one place the built
route is narrower than §7.1's sketch.** §7.1 says "body: Spec-ish" and insists
the body carry no verdict, because `Store.Create` runs `Decide` itself. It
does not carry the **scorecards** either: the brain replays its own retained
history to produce them, through the same `Brain.WhatIf` the GET route serves.
A caller-supplied scorecard is a measurement of unknown provenance, and a
proposal is a document whose entire value is that its numbers came from
somewhere checkable. The cost is real and is stated in §6.3.

Three more things the body cannot carry, each tested by name in
`TestTheProposerCannotSupplyTheVerdict`:

* **the verdict** — `gate`, and `state`/`approval` for good measure;
* **the author** — §4.3 of the CLI findings called a `{"by":{"kind":"human"}}`
  body field "`--author-id` pointed the dangerous way". Every proposal filed
  here is `system:kilter-brain-api`, which names the FUNNEL: the write token is
  held by every controller and agent that ingests, so the brain cannot tell one
  holder from another, and inventing a name for an indistinguishable caller
  puts an unbacked fact in an audit trail;
* **the yardstick** — `envelope` and `tolerance` are the shipped defaults. A
  proposer that could widen the tolerance would be supplying its own verdict
  with extra steps.

`Target` carries only the cluster, and it comes from the *result* rather than
from the request, so a proposal cannot name a cluster other than the one that
was replayed. `EvidenceIDs` are empty rather than filled with plausible-looking
strings.

**200, never 201.** Proposals are content-addressed and `CreatedAt` is outside
the fingerprint, so filing the same one twice returns the record that already
exists — that is what makes a nightly tuner idempotent rather than a duplicate
factory, and a 201 would claim a document was created that was not.
`TestFilingTheSameProposalTwiceIsOneProposal` files the same candidate on two
different nights and requires one record with one id.

### 1.5 `POST /api/v1/proposals/{id}/rejections`

`{"reason":"…"}`, optional; an absent body is a rejection with no reason, a
malformed one is a 400. Response: 200 and the `Record`.

This is the only write here, and it is safe by construction: `pkg/whatif` lets
**any** actor reject, because refusing to make a change needs no capability.
That asymmetry is the whole reason this route exists and approvals do not.

---

## 2. Simulation and observation, made structurally different

A what-if answer carries two `backtest.Scorecard`s — the same field names, the
same units and the same confident tone as a scorecard measured over the policy
that really ran. Served bare, `regret $2.90` from a policy that never existed
is indistinguishable from `regret $2.90` that someone paid.

The envelope is `Simulated`, and the distinction is structural in both
directions rather than documentary in either:

* **On the wire**, the whole `whatif.Result` sits under a `simulation` key,
  beside `observed: false`, `applied: false` and a statement saying the policy
  was never in force. A client that drops the envelope does not get a
  plausible measurement — it gets **nothing**: there is no scorecard, no
  regret and no delta at the top level to mis-read.
  `TestWhatIfRouteServesASimulationNotAMeasurement` decodes the response into a
  bare `whatif.Result` and requires every field to come back zero, and pins the
  top-level key set exactly.
* **In Go**, `Simulated` has no exported field and no exported constructor.
  `observed` is not something a caller can set, here or in `pkg/mcp` later; it
  is a constant inside `MarshalJSON`. This is `whatif.Approval`'s discipline
  applied to a payload: not a rule that gets checked, a value that cannot be
  built. The test asserts the type has no exported field, so adding one fails
  the build's tests.

`SimulationBasis` rides along because "is this a simulation" is not the only
question a reader has: it names how many retained snapshots were replayed, how
many instants were scored, both policy hashes, and whether the refusal
predicates were part of the yardstick.

**The proposal routes are deliberately NOT wrapped.** A `Record` is an
observation *of a document* — this proposal exists, in this state, with this
audit trail — and its counterfactual numbers already sit under
`proposal.gate` and `proposal.delta`, shapes no measurement produces. Wrapping
it would also have meant re-deriving the wire form §7.1 says to lift.

---

## 3. Status codes: every failure found, and the test that pins it

`SUBSTRATE-FINDINGS.md` §3.2's three, extended by the two a request can be
wrong in. The classes are types (`badRequest`, `notEnoughEvidence`,
`notIngested`, `conflict`, `defect`), so the status is a property of the
failure and not of its wording.

| Status | Failure | Test |
|---|---|---|
| **404** | the cluster was never ingested (`snapshotFor` is nil) | `TestWhatIfStatusCodesAreThreeDifferentFacts/404…` |
| **404** | no such proposal (`whatif.ErrNotFound`, on GET and on rejections) | `TestProposalRoutesAreProjectionsOfTheStore`, `TestRejectionMovesTheRecordAndConflictIsNotAnError404` |
| **422** | the brain has no persistent store at all (`ErrNoHistory`) | `…/422 a brain with no store at all` |
| **422** | fewer than 2 retained snapshots in the window (`ErrHistoryTooShort`) | `…/422 the substrate holds too little history` |
| **422** | snapshots exist but no instant can be scored (`Instants == 0`) | `TestWhatIfRefusesTwoEmptyReplaysRatherThanComparingThem` |
| **400** | window absent, half-specified, empty, inverted or over the cap | `TestWhatIfRefusesAnUnboundedWindow` |
| **400** | no axis named, unknown axis, non-finite, outside the hard bounds, given twice, unparseable value, candidate == incumbent, `base-soak` without `enforceRefusals`, unparseable horizon, horizon wider than the window | `TestWhatIfRejectsAQuestionItCannotAnswer`, `TestBaseSoakWithoutEnforcedRefusalsIsRefused` |
| **400** | request body with an unknown field (a verdict, an author, a scorecard, a tolerance) | `TestTheProposerCannotSupplyTheVerdict` |
| **409** | the record's state machine refuses the transition (rejecting a rejected proposal) | `TestRejectionMovesTheRecordAndConflictIsNotAnError404` |
| **413 / 400** | oversized or malformed body (`decodeStatus`, reused) | — [unverified: inherited from the existing helper, not re-tested here] |
| **500** | the retained history will not read back | `…/500 only for a defect in this process` |
| **501** | approvals and applied | `TestApprovalsAndAppliedAreRefusedByName` |

Three decisions worth the words:

**422 and not 500, for an empty brain.** A brain that was started yesterday and
asked about last month is not broken. `Brain.Backtest`'s two error types are
reused rather than re-derived, so "not enough history" has one definition and
one wording across the backtest path and this one.

**422 and not 200-with-zeros.** This is the refusal `kilter whatif --cluster`
was written around, and cmd's §3.1 argument is stronger here than for a plain
backtest, not weaker: a backtest over nothing prints one suspicious scorecard,
while a what-if over nothing prints a *comparison* in which every delta is
0.00, the regret change is `$0.0000`, and the gate says "short of the required
margin" — a considered-sounding negative verdict about a measurement that never
happened. Both the count check and `backtest`'s own `Instants` report gate it,
and `assertNoVerdict` requires that no refusal body contains `regret`,
`oracleGap`, `gate`, `delta` or `simulation`. A refusal that also prints
numbers is a refusal a reader reads past.

**500 for exactly one thing.** The window was bounded, the cluster came from
this brain's own ingest, and the candidate was validated before anything ran —
so a failure from `store.Snapshots` is the retained history not reading back,
which is a defect in this process and the one case worth paging on. It is
provoked in the test by closing the bolt store under a populated brain, and the
test additionally requires the 500 body **not** to read as an evidence
shortfall: an operator who reads "not enough history" at 3am goes back to bed.

`whatIfStatus`'s default is **500**, the opposite of `explainStatus`'s 422.
Every error this file returns is classified where it is created, so an
unclassified one means a path was added without deciding what kind of failure
it is — a hole in this file, not a fact about the caller's evidence.
`TestEveryWhatIfFailureIsClassified` pins the whole table including that
default.

One status this plane does **not** use: 403. A caller holding the write token
is not "forbidden" from approving — nobody can approve here (§5.1), so the
answer is 501 and the reason.

---

## 4. Bounding the query

### 4.1 The window: 400 days, refused, not truncated

`checkExplainWindow` is reused rather than re-derived, so this route honours
the bound `pkg/store`'s `Snapshots` and both explanation routes honour.
`TestWhatIfRefusesAnUnboundedWindow` asserts the constant **equals**
`store.DefaultSnapshotRetention().MaxWindow` — a second number here would be
free to drift from the one the store enforces — and that a window one day
inside the cap is still answered, so the bound is a cap and not a rounding.

The arithmetic, and why refusing beats truncating: retention already bounds the
answer at 768 rows / 32 MiB per cluster (`SUBSTRATE-FINDINGS.md` §1.1), so a
400-year request cannot return 400 years of anything. It would return a
retention-bounded slice that reads exactly like a complete answer to the
question asked. This is §1.3's point — the guard is against a caller who typed
the wrong unit getting a plausible-looking short answer instead of being told
the query was nonsense — and it matters more for a what-if than for a raw
read, because the plausible-looking short answer here comes with a **verdict**
attached.

What the bound does *not* have to do is stop an OOM, and the numbers say why.
The window is materialised once per replay, so twice, plus once in the count
check — and the count check drops its slice before the replays start, so the
peak live set is one window's worth of decoded snapshots. For the largest
cluster shape in §1.2's table (1 000 nodes / 15 000 pods) retention holds ~80
rows of ~12.4 MiB raw, so roughly a gigabyte decoded per pass and about two
passes of CPU over it. That is bounded by *retention*, not by the window, which
is exactly why the window bound can be a correctness guard rather than a
capacity one. [unverified: the per-row raw sizes are §1.2's measured fixture
table, not re-measured here.]

### 4.2 The horizon

Bounded to `(0, 400 days]` and additionally required not to exceed the window
span. `pkg/backtest` refuses the second case itself; checking it here is what
keeps it a 400 — the caller asked for a scoring window that does not fit inside
the replay window — instead of a replay failure this file would have had to
call a defect. That distinction was found by the test, which first saw a 500.

### 4.3 The instant count, bounded by not adding a knob

`Scoring` is `backtest.DefaultConfig()`; there is no `interval` parameter. With
the interval fixed at 24h and the window capped at 400 days, a request can ask
for at most 400 decision instants. A caller-supplied `interval=1m` over the
same window would be 576 000 instants of replay work per side — a denial of
service with a plausible-looking query string. The knob is omitted rather than
bounded because nothing yet needs it, and an unbounded one is worse than none.

---

## 5. What §7.1 asked for that is not built, and why

### 5.1 `POST /api/v1/proposals/{id}/approvals` — 501, by name

§7.1: *"the token tier must be one the MCP server (§6) and the reasoner (§5) do
not hold, and the actor must come from the token, never from the body"*.

`BrainConfig` has two tiers, `Token` and `ReadToken`, and **the write token is
held by every controller and agent that ingests a snapshot**. Deriving
`Actor{Kind: ActorHuman}` from it would hand the approval capability to exactly
the actor the human-only rule exists to exclude, silently, in the direction
that grants capability. `pkg/whatif`'s guarantee — self-approval is not a rule
that gets checked but a value that cannot be constructed — would become a
convention that one bearer token walks around.

A human tier is a new `BrainConfig` field and a startup-time decision about who
holds it. `brain.go` was in scope for route registration only, so that
configuration change is not this unit's, and a route that cannot authenticate
an approver must not pretend to. The route is therefore **registered and
refuses**, for the same reason `kilter whatif --auto-tune=apply` is a flag that
refuses rather than an unknown flag: a 404 from the mux teaches a caller
nothing, and here the reason is the answer.

The refusal is not just text. `TestNoCodePathInThisPackageApprovesOrApplies`
walks the package's AST and fails on any call to `NewApprover`, `Approve` or
`MarkApplied`, so the claim cannot rot into a comment about code that changed.
`TestNoAPIPathReachesApprovedOrApplied` states it over the bytes: after a
session of every request this surface accepts, `Store.Snapshot()` contains no
`"approved"`, no `"applied"`, no `"approval"` and no `"human"` — and still
loads, which is `pkg/whatif`'s own tamper check.

**What is needed to build it** (not built here): a `BrainConfig.HumanToken`
distinct from `Token`, an `authHuman` guard beside `auth`/`authWrite`, and an
actor derived from that token alone. `Store.Approve` then needs nothing else —
`NewApprover` is the only constructor and it refuses every non-human kind.

### 5.2 `POST /api/v1/proposals/{id}/applied` — 501, by name

Two independent reasons, and the second settles it.

Nothing in `pkg/api` writes a policy config, and §4.6 requires a `LedgerEntry`
in the same breath as the applied transition — proposal id, both policy hashes,
the claimed `Delta.ProjectedMonthlyUSD` — because that entry is what later lets
the existing claimed-vs-measured join score the tuner itself. Recording
"applied" from a process that wrote neither the config nor the entry would put
an unbacked fact into an audit trail whose whole value is that its facts are
backed. (`api.LedgerEntry` is a different record — one executed *plan*, with
`Steps` and a `Mode` — so it is not the entry §4.6 is asking for either.)

And it is unreachable: only an approved proposal can be applied, and no
approval can be minted here. A route that could only ever answer *"proposal X
is gated; only an approved proposal can be applied"* is better replaced by the
reason it can never do anything else.

This is also the line the actuator prohibition draws. A what-if plane is a
hypothetical; `applied` is the one verb in §7.1 that asserts something happened
to a real cluster.

### 5.3 Persistence (§7.2) — the proposal store is in memory

`whatif.Store.Snapshot()` / `whatif.Load()` move the whole store as bytes and
§7.2 says where they go: a `proposals` bucket in `pkg/store`'s bbolt file. That
is a `pkg/store` change and `pkg/store` was out of scope, so the store here is
created by `registerWhatIfRoutes` and lives as long as the handler.

What that costs, stated plainly rather than left to be discovered: **a brain
restart loses every filed proposal and its audit trail.** Nothing is corrupted
and nothing is lied about — a lost proposal is not an approved one — but a
gated proposal awaiting a human does not survive a deploy. The store is bounded
by `pkg/whatif`'s own 1 000-record cap, so the leak is a ceiling and not a
growth curve.

Two lines and a `Sweep` call are what it takes when `pkg/store` is in scope:
load the bucket's bytes through `whatif.Load` at `NewBrain`, write
`Snapshot()` on the same housekeeping timer that would call
`store.Sweep(clock)` so expired approvals move to `expired` rather than
lingering. `MarkApplied` checks expiry independently, so an unswept store is
safe, just untidy.

`Handler()` builds one surface per call. `Serve` calls `Handler()` once, and so
does every test; a caller that built two handlers from one brain would get two
independent proposal stores. With a bucket behind it that stops being true, so
it is a property of the gap rather than a design choice. [unverified: no test
asserts the two-handler behaviour, because nothing in the tree does it.]

### 5.4 The nightly tuner (§7.3)

Not built, and out of scope by the brief. What this unit leaves ready:
`Brain.IncumbentPolicy()` is the baseline a tuner needs, `Brain.WhatIf` is the
scenario runner with the refusals already attached, and the surface's
`whatif.Clock` is the seam a nightly loop would drive. §7.3's own advice — that
`NewTuner` be constructed unconditionally at brain start so a disabled-but-
invalid config is a startup error rather than a 3am surprise — is a
`BrainConfig` change, so it belongs with §5.1's.

### 5.5 `KindAnnotationChange`

Not offered anywhere, by any parameter. `Spec.normalize` rejects it with a
reason; an annotation proposal's gate is not a backtest scorecard, so a route
that accepted one would file a proposal whose `GateResult` means nothing.

---

## 6. Adapters and mismatches, each with what forced it

### 6.1 The axis projection, restated for the third time

`whatif.Axis.get`/`set` are unexported, so — like `cmd/kilter`'s
`applyAxisSets` before it — this package restates the five-field mapping from
an axis onto a policy. That is the kind of duplication that fails silently:
set `MemoryHeadroom` when the caller said `cpu-headroom` and everything
downstream is internally consistent and wrong.

The cross-check is `pkg/whatif`'s own projection.
`TestSetMovesTheAxisPkgWhatifThinksItMoves` drives every axis through the HTTP
route and requires `Result.Changes` — computed by `changesBetween`, which uses
`Axis.get` — to name the axis the caller named and carry the value it carried.
A mis-mapped field fails the build's tests rather than quietly tuning the wrong
knob under the right name. Values are additionally checked against
`whatif.HardBounds()` before anything is replayed, and the gate re-checks the
candidate against the narrower declared envelope; producer and checker being
different code is the package's own argument.

The seam that would delete this file's copy: an exported
`whatif.Axis.Apply(Policy, float64) (Policy, error)`.

### 6.2 `base-soak` without `enforceRefusals` is refused

`decision.Config` can only change what a replay **does** when
`EnforceDecisionRefusals` is set: that flag is what lets a predicate veto a
change before the planner is consulted. Without it, `backtest` reads the config
in two harmless places — the policy hash, and `refusalCode`'s fallback, which
borrows a predicate to NAME a refusal that had already happened for another
reason. Neither moves a decision or a dollar. So a soak what-if without the
flag scores two policies that decide identically: every delta is zero, the gate
says "regret improves by $0.0000", and the answer reads as *"we tested a longer
soak and it changed nothing"*. That is a what-if of a policy nobody ran, which
is the artefact this plane exists to prevent. Measured before it was refused,
on a fixture with no refusals to re-label: baseline and candidate regret came
back `0.253347 → 0.253347`.

It is cmd's §3.6 seen from the other side. There, `enforceDecisionRefusals` in
a *policy file* is refused because it is the yardstick; here the flag is
accepted, applies to **both** replays, and is echoed in the basis — and the
axis that needs it is refused without it.

### 6.3 The cost of not accepting scorecards (§1.4)

A client that computed a what-if elsewhere — the CLI's `--demo` traces, say —
cannot file the result through this route. That is deliberate: those scorecards
are of a synthetic trace, and a fleet's proposal store should hold documents
about the fleet. But it does mean `POST /proposals` is only usable against a
cluster this brain has retained history for, which is the same precondition
`GET .../whatif` has, with the same 404/422 answers when it does not hold.

---

## 7. Determinism

* **No `time.Now()` in anything computed.** The window comes from the request,
  the horizon from the request, the baseline from the brain's config. The wall
  clock is read once, at the HTTP edge, and passed inward as a `whatif.Clock`;
  it reaches `CreatedAt` and the audit transitions and nothing else. Tests fix
  it, so a proposal's bytes are a function of its inputs.
* **No map iteration reaches output.** Axis moves arrive as a map and are
  applied in `AllAxes` order; a request with several bad axes is validated in
  sorted order so the first complaint does not depend on iteration; `List()`
  arrives sorted and is not re-sorted.
  `TestWhatIfAnswerIsByteIdenticalAcrossRepeatedRequests` repeats one request
  four times **in one process** — Go randomizes map iteration on every range,
  and a scorecard carries a refusal map — and requires identical bytes.
* **No network and no socket to anything real.** Every handler test is
  `httptest` over an in-process mux; the brain's store is a temp-dir bolt file.

---

## 8. Smaller notes

* **`Brain.WhatIf` refuses in the same order the statuses mean things**: 404
  before 422, because "that cluster was never ingested" and "that cluster has
  too little history" are different facts an operator acts on differently.
* **`ErrNoHistory` and `ErrHistoryTooShort` are reused, not re-derived.** The
  what-if path and the backtest path give the same words for the same
  shortfall, so an operator who has read one has read both.
* **The count check drops its snapshot slice before replaying.** Each replay
  re-reads and re-decodes the window itself; holding a third copy for the
  duration of two replays is hundreds of megabytes on the largest retained
  history, for nothing.
* **The actuator claim got narrower when it was tested, and that is the
  finding.** "pkg/api cannot reach an actuator" is **false** as stated:
  `pkg/api → pkg/actuate → pkg/provider → pkg/rds`, because `ledger.go` and
  `explainroutes.go` import `pkg/actuate` for `actuate.StatusDone`. That
  predates this plane and is not this unit's to remove, so
  `TestTheAPIPackageCannotReachAnActuator` pins what is actually true and
  useful: the what-if plane's own transitive closure reaches **neither**
  actuator; `pkg/api` never reaches `pkg/ec2` at all; the `pkg/rds`
  reachability runs **only** through `pkg/actuate` (proved by recomputing the
  closure with `pkg/actuate` as a leaf and watching it disappear); and no file
  in `pkg/api` imports either actuator directly, which is what makes an
  `Actuator` un-nameable — and therefore un-callable — in this package. The
  test also asserts `pkg/ec2/actuate.go` and `pkg/rds/actuate.go` still declare
  `NewActuator` and `Execute`, so it cannot pass by naming something that moved.
