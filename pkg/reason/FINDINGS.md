# U6 — the reasoning core, with no model in it

`pkg/reason` is unit 6's core: the read-only tool registry, the reasoning loop,
the audit trail, the budgets and the injection defenses. There is **no provider
implementation** — no Anthropic SDK, no openai-compat client, no HTTP client to
a model endpoint, not behind a build tag. `Provider` is an interface; the only
implementation in this tree is a scripted fake in `helpers_test.go`.

```
reason.go     package doc, version pins, the system prompt
clock.go      the time seam (pkg/whatif's idiom, unchanged)
sanitize.go   scrubText / scrubJSON / stripExternalLinks — the display half of §5.7
refusal.go    Refusal, the code→detail table, Clamp
schema.go     Param/Schema/Args — clamp-or-refuse, per parameter, by type
tool.go       Reader, roStore, Tool, Input, Result — the capability boundary
registry.go   Registry.Run: validate → time-box → scrub → cap → cite
tools.go      the five read-only tools
provider.go   the model seam and its wire types
budget.go     Budget/spend — bounds a loop, not a call
audit.go      the hash-chained trail
finding.go    terminal states, Finding, the strict output schema
loop.go       Investigator.Run, the citation ledger, the publish gate
context.go    deterministic seed selection at 50k subjects
```

**Status:** `gofmt -l ./pkg/reason` empty; `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/reason/...` and `go test -race -short ./...` all
green. **`go.mod` and `go.sum` are byte-identical** (sha256 `9a21b19b…` /
`e21c013b…` before and after). No file outside `pkg/reason/` was touched.

| | |
|---|---|
| Production code | 3,842 lines across 14 files |
| Tests | 72 test functions + 1 benchmark, 2,748 lines across 8 files |
| Coverage, `pkg/reason` | **86.8 %** |
| Module dependencies added | **0** |

---

## 1. Read-only by construction, and what would defeat it

Five things hold, in descending order of how much weight they carry:

1. **No other package can build a `Tool`.** Every field is unexported *and*
   the constructor `readOnlyTool` is unexported. `pkg/api`, `cmd/` and a future
   `pkg/mcp` cannot put a tool in the registry at all — not a write tool, not a
   read tool. The §5.2 surface is enumerated in `tools.go` and nowhere else.
2. **The zero `Tool` is the one value another package can produce, and it is
   inert.** `Registry.Register` refuses anything not stamped, which is exactly
   the zero value (`TestTheZeroToolCannotBeRegistered`). This mirrors
   `pkg/rds`'s `ApprovedStep` deliberately, including the "the zero value is
   representable, so pin it at runtime" half.
3. **A tool body's only handle on the world is `Input`.** It carries a
   `Reader` — `evidence.Store`'s four query methods, not its `Append` — plus a
   sorted subject snapshot and two bound capabilities. `evidence.Sink` is not
   reachable from anything a tool receives.
4. **The narrowed store's write method is hostile, not absent.** `roStore` has
   to satisfy `evidence.Store` because `evidence.BuildDossier` and
   `explain.BuildExplain` take one. Rather than hand either a live writer, the
   value they get answers `ErrReadOnly` to `Append`, and its wrapped store is
   an unexported field so no type assertion recovers it
   (`TestTheNarrowedStoreRefusesToWrite`).
5. **Composite literals are confined by test.** `TestOnlyToolGoConstructsATool`
   parses every file in the package and fails if a `Tool{...}` literal appears
   outside `tool.go`. Inside the package the type system alone would not stop
   `Tool{readOnly: true, run: writeSomething}`.

### What would defeat it

- **A closure.** A body compiled inside this package can capture a writer from
  its enclosing scope, and no type prevents that.
  `TestNoActuatorSymbolIsReachableFromThisPackage` is the cover: it derives the
  forbidden identifier set from `pkg/ec2`'s and `pkg/rds`'s own `actuate*.go`
  sources (the `cmd/BRAINWIRE-FINDINGS.md` §6 technique), checks three canaries
  so an empty scan cannot pass, and fails on both an import of an actuator
  package and a named actuation verb. It is a *narrower* check than cmd's,
  because this package imports neither actuator package and never should — the
  same test fails on the import itself, before it looks at any identifier.
- **Exporting the constructor.** The moment `readOnlyTool` becomes
  `ReadOnlyTool`, any package can register a read-only-stamped tool whose body
  does whatever it likes. Unit 8's proposal tools are the pressure here; see §7.
- **A `Reader` implementation that writes.** `RegistryConfig.Store` is an
  `evidence.Store` supplied by the caller. Nothing stops a caller passing a
  store whose `Events` has side effects. This is not a hole a type can close —
  the substrate is the caller's — and it is the reason INV-3 (collector-side
  allowlist) lives where it does.
- **A tool that returns a `Result` built from something other than what it
  read.** `Result` has no exported constructor, so only this package can, but
  within the package a tool could cite an ID it fabricated. The loop's
  re-resolution (§3) makes that fail closed rather than fail open.

---

## 2. The hostile corpus, and what each entry attacks

`injection_test.go` carries nine entries. Each is run through three argument
paths — as a `subject_key`, as a tool *name*, and as an undeclared *argument
name* — and, concatenated into one subject name and one set of event
attributes, through `rawStore` as substrate content that reaches the model.

| Entry | Attacks | Result |
|---|---|---|
| `instructions` | the model's instruction-following: prose shaped like a system message | reaches the model **as data**, inside a JSON string under `data`, labelled untrusted; refused as an argument (not in the universe) |
| `instructions-in-a-refusal` | the refusal path itself — the payload rides out on the error explaining the rejection | refusal carries a code, a field name and a bound, all ours; no free text |
| `ansi-c0` | the operator's terminal, classic `ESC [` | stripped at ingest by `pkg/evidence`; stripped again here |
| `ansi-c1` | the operator's terminal via **U+009B**, the single-byte CSI | **survives `pkg/evidence` ingest** (see §2.1); stripped here |
| `zero-width` | identity: two subjects that render identically | stripped here; subject is still reachable by `subject_index` |
| `bidi-override` | identity: a name that renders as its own reverse | stripped here |
| `valid-json` | the transcript's structure: a name that parses as a message object | arrives as a string field; `TestAJSONShapedNameCannotEscapeItsField` decodes the envelope strictly and finds it still nested |
| `ten-kib` | the context budget and every parser below it | refused as `arguments-too-large` before any parse |
| `budget-raise` | the loop's own accounting, delivered as a tool **result** | budget unmoved; `TestATranscriptCannotRaiseItsOwnBudget` |

Two whole-run assertions sit on top:
`TestNoHostileByteReachesTheTranscriptThroughAResult` rebuilds everything the
model was shown on the final turn and checks that no 16-byte fragment of any
corpus entry survives; `TestEveryRecordedRefusalDetailComesFromTheTable` checks
that every refusal produced across the corpus has the table's detail, verbatim.

### 2.1 A measured gap in `pkg/evidence`, not a hypothetical one

`evidence.cleanString` tests `r < 0x20 || r == 0x7f`. That is C0 and DEL. The
**C1 block (U+0080–U+009F) passes it**, and U+009B is the CSI introducer that
xterm-family terminals honour exactly like `ESC [` — so a workload named
`"<U+009B>2K"` clears the operator's line without ever containing an ESC.
Zero-width (U+200B–U+200F), bidi (U+202A–U+202E, U+2066–U+2069) and the BOM
pass it too.

This is stated as an observation about the substrate, not a complaint: ingest
sanitization is the load-bearing pass and it does its job. `pkg/reason` closes
the remainder for its own output, because it must also declaw strings that
never went through ingest (an operator's question, a model's answer, a future
bbolt-backed store, a checkpoint restored from an older build). **Closing it in
`pkg/evidence` would be strictly better** and is out of this unit's scope: the
one-line change is in `cleanString`, and it needs `pkg/evidence`'s codec tests
to be re-golden'd because stored bytes would change.

### 2.2 The anti-echo rule, and where it costs something

The rule as implemented: **a name is repeated back to the model only if it is
this package's own vocabulary.**

- `Refusal.Detail` is `refusalDetail[Code]`, a constant. There is no formatting
  site where an argument could be interpolated.
  `TestOnlyRefusalGoConstructsARefusal` parses the package and fails if a
  `Refusal{...}` literal appears outside `refusal.go`.
- `Refusal.Field` is a schema parameter name **this package declared**. For an
  *undeclared* argument the field is left empty — the name was chosen by
  whoever wrote the call.
- An unregistered tool's name is dropped from both the result envelope and the
  transcript message that carries it (`Outcome.known`). This closes three
  paths: unknown-tool, over-the-per-turn-cap, and the message's own `tool`
  field.
- `Message.Calls` **does** carry the model's raw arguments, because every wire
  format needs the assistant's tool-use block replayed. That is not the echo
  §5.7 forbids: the rule is that a *result's* bytes never become a subsequent
  call's arguments, and the only path from model output to a tool is
  `Schema.Validate`. The test excludes that field explicitly and says why.

What it costs: a model that sends an undeclared argument is told "the schema
declares no such argument" without being told which. This is defensible — the
model holds the schema, and `additionalProperties: false` enumerates every
legal name — but it is a real ergonomic loss and someone will want to undo it.
The audit trail keeps the scrubbed name for the operator, who is not the
attacker's target.

### 2.3 `subject_index`, which the trap forced

The corpus produced a design change rather than just a test. Everything shown
to a model is scrubbed, so the key the model reads back from `list_subjects` is
**not** the key the substrate holds when the name carries unsafe runes.
Accepting the scrubbed form would resolve one name to a different object.
Refusing it makes a hostile-named workload permanently uninvestigatable — a
denial of service an attacker arranges with one annotation.

So subject-taking tools accept `subject_index`: a bounded integer over the same
total order `list_subjects` enumerates. It carries no cluster-authored bytes at
all and can only name a subject the model has already enumerated. `subject_key`
remains for the readable case; supplying both is refused
(`subject-selected-two-ways`).

---

## 3. Citations: re-resolution, handles, and why discarding beats annotating

Three gates, in order:

1. **At the source.** A tool declares the IDs its result showed. The explain
   tool verifies its payload through `explain.Explanation.Verify` *before*
   returning, so a tool never hands a model a citation that does not resolve
   (`TestEveryCitationATooLReturnsResolves`).
2. **Session-fetched.** The model cites **handles** (`e1`, `e2`, …), not IDs.
   A handle is issued on first appearance and lives in the session ledger. An
   unissued handle is not in the ledger and there is no ID to guess, which
   makes "the model cannot cite what it did not read" exact rather than
   probabilistic (`TestAModelCannotCiteWhatItDidNotRead`).
3. **Re-resolution.** Every handle's ID is resolved again, through
   `explain.Resolver`, against the same narrowed substrate the answer was
   computed from. Shown-once-gone-now fails here
   (`TestACitationThatStopsResolvingDiscardsTheAnswer`).

Handles exist because an evidence ID **embeds the subject key** —
`evt/<cluster>/<kind>/<key>/<eventKind>@<nanos>` — and a subject key is a
workload name somebody with kubectl wrote. The citation channel has to survive
a round trip through the model byte-for-byte, which rules out scrubbing it; and
not scrubbing it would hand a rendering exploit to whoever reads the answer.
Handles carry no cluster bytes at all, and cost ~2 tokens per citation.
`Finding.Evidence` carries the real IDs, mapped back after verification, so the
artifact §5.3 specifies is unchanged.
`TestTheModelNeverSeesAnEvidenceIDContainingClusterBytes` pins both halves.

### Why discard, not annotate

The tempting alternative is to publish with "3 of 7 citations could not be
verified" attached. It fails for reasons that have nothing to do with models:
an annotation is a *second* thing to read and the answer is the first. A
platform engineer forwards the paragraph to an app team and the annotation does
not travel with it. A dashboard renders the answer in a card and the warning in
a tooltip. A summary quotes the conclusion.

Worse, the annotation is most likely to be ignored in exactly the case it
matters: a confident, fluent, well-structured answer whose citations happen not
to resolve is what a *successful* injection produces. Publishing it with a
caveat converts a structural defense into a UX one, and §5.7 puts
presentation-level defenses last for that reason.

So `Finding.Answer` is emptied, `Outcome` becomes `discarded`, and the audit
records the counts and the digests of the rejected citations. An uncited answer
is discarded too, by default (`Config.AllowUncitedAnswer` exists, defaults
false, and the doc comment says to write down who turned it on).

---

## 4. Budgets: the terminal states

Exhaustion is neither a success nor an error. `Investigator.Run` returns an
error **only** when the investigation could not start — no provider, or a
malformed question. Everything else is a state on the finding:

| `Finding.Outcome` | Partial | `Answer` | Meaning |
|---|---|---|---|
| `answered` | no | set | the only publishable state |
| `turn-limit` | **yes** | empty | hit `MaxTurns`; the evidence read so far is reported, resolved |
| `budget-exhausted-tokens` | **yes** | empty | could not fund another turn |
| `budget-exhausted-usd` | **yes** | empty | priced budget spent |
| `discarded` | no | empty | an answer arrived and was thrown away; nothing partial about it |
| `malformed-finding` | no | empty | not a finding, or stopped without producing one |
| `provider-failed` | no | empty | the model server failed; every deterministic answer is unaffected |

The budget bounds the **loop**, three ways: the accounting is cumulative;
`spend.exhausted()` is checked *before* a turn is spent, not after; and the
per-turn cap handed to the provider is `min(MaxOutputTokensPerTurn, remaining)`
— `TestTheTurnCapIsLoweredToWhatRemains` asserts it falls monotonically. A
per-call cap the caller cannot lower is a speed limit, not a budget.

Partial work is reported as partial: `Finding.Evidence` and `Citations` carry
everything the session actually read, re-resolved, so the operator gets the
same grounded material the model had. `Finding.Notes` says why it stopped
(constants, never substrate text). Cost is in **micro-USD integers**, priced as
`tokens × USDPerMillion`, so two replays compare byte-for-byte.

Per-turn fan-out is capped separately and the extras are **refused, not
dropped**: twelve calls against a cap of four produce four results and eight
recorded refusals (`TestAFanOutTurnIsCappedAndTheExtrasAreRefusedNotDropped`).

---

## 5. Determinism seams

The same scripted transcript produces a **byte-identical** audit trail
(`TestAScriptedTranscriptProducesAByteIdenticalAuditTrail`, four runs).

- **`Clock`** is the only way this package learns the time — `pkg/whatif`'s
  idiom, including "a nil Clock is refused at the entry point, never defaulted
  to `time.Now`". `New` fails without one.
- **No durations are recorded.** A tool's elapsed time is real and varies;
  recording it would put a stopwatch reading in a hash chain.
- **The one wall-clock read** in the package is the per-tool
  `context.WithTimeout` deadline. It bounds work and is never recorded.
- **Nothing is emitted from a map range.** Args are recorded as sorted
  key/value pairs (`sortedKV`), citations are sorted (`dedupeCites`), tools are
  in name order, and `scrubJSON` rebuilds documents through `map[string]any`
  precisely because `encoding/json` sorts map keys on the way out.
- **`scrubJSON` uses `json.Number`**, so `123456789012345678` does not become
  `123456789012345680` on the way through.
- **Post-scrub key collisions** resolve by a total rule (keep the
  lexicographically smaller encoding), not by whichever the map yielded first.
- **The seed context is a function of the candidate *set*.** Ranking is score
  descending, then `(cluster, kind, key)` ascending — total, so no two distinct
  candidates compare equal. 50,000 candidates and a deterministic permutation
  of the same 50,000 produce identical bytes; selection is a bounded insert,
  2.3 ms on an M4 (`BenchmarkSeedAtFiftyThousand`).
- **The trail is hash-chained.** Each record hashes itself with its own `Hash`
  field empty, plus the previous record's hash. Rewriting the question after
  the fact — the single most useful edit for somebody covering their tracks —
  fails `Verify` (`TestTamperingWithTheAuditTrailIsEvident`).
- **Versions are pinned** into the opening record and the finding:
  `RegistryVersion`, `PromptVersion`, the model and provider names, the system
  prompt's digest, the seed's digest, and `Registry.Digest()` — a hash of every
  tool name, description and schema, so a replay can distinguish "the model
  behaved differently" from "the model was offered a different surface".

---

## 6. Exactly what `cmd/` and `pkg/api` must call

No CLI surface and no route was added — `kilter ask` and `POST /ask` are a
later job. This is what they call. Copy-pasteable.

```go
import (
    "github.com/agenticode/kilter/pkg/evidence"
    "github.com/agenticode/kilter/pkg/explain"
    "github.com/agenticode/kilter/pkg/reason"
)

// 1. One registry per (cluster, window). The window is an ARGUMENT: pick it
//    from the request, or from the caller's clock — never from inside here.
reg, err := reason.NewRegistry(reason.RegistryConfig{
    Scope: reason.Scope{
        Cluster: clusterID,
        From:    from,             // required, non-zero
        To:      to,               // required, after From
        Subject: subjectRef,       // optional; advisory, carried into the audit
    },
    Store:    ev,                  // evidence.Store — brain's substrate for this cluster
    Subjects: ev.Subjects(),       // the enumerable universe; a SNAPSHOT, not a live query
    Actions:  ledgerProjection,    // []explain.LedgerAction, for act/ citations; may be nil
    // MaxResultBytes: defaults to reason.DefaultMaxResultBytes (8 KiB)
})

// 2. One investigator. A nil Provider is legal and is the shipped default.
iv, err := reason.New(reason.Config{
    Provider: prov,                // nil ⇒ ask is unavailable; nothing else changes
    Registry: reg,
    Clock:    func() time.Time { return time.Now() },  // the brain's clock, injected
    Budget:   reason.DefaultBudget(),                  // 12 turns / 150k tokens / $2
    Seed:     candidates,          // []reason.Candidate, ranked by the deterministic engine
})

// 3. Availability is a first-class question, asked before the work.
if !iv.Available() {
    // §5.9: NL interrogation is UNAVAILABLE, not degraded. HTTP 501 (or 503 if
    // a provider is configured but unreachable); CLI exit 1 with reason.ErrNoProvider's
    // text. Do NOT fall back to template prose and call it an answer.
    return reason.ErrNoProvider
}

f, err := iv.Run(ctx, reason.Question{
    Text:      question,           // ≤ 4096 bytes
    Scope:     reg.Scope(),        // or a Scope whose Cluster matches
    Initiator: tokenIdentity,      // §5.6: the API-token identity, recorded
})
if err != nil {
    // ONLY "could not start": no provider, empty/oversize question, wrong cluster.
    return err
}

// 4. The outcome is the answer. Do not infer success from err == nil.
switch f.Outcome {
case reason.OutcomeAnswered:            // 200; render f.Answer + f.Citations
case reason.OutcomeTurnLimit,
     reason.OutcomeBudgetTokens,
     reason.OutcomeBudgetUSD:           // 200 with f.Partial set, or CLI exit 3.
                                        // Render f.Evidence/f.Citations and f.Notes.
                                        // NEVER render an empty Answer as "no problems found".
case reason.OutcomeDiscarded:           // 502-ish / CLI exit 1. Say the answer failed
                                        // verification. Do NOT render f.Answer (it is empty).
case reason.OutcomeMalformed,
     reason.OutcomeProviderFailed:      // 502 / CLI exit 1.
}

// 5. The audit trail is not optional. §5.6 wants one bbolt record per
//    investigation in bucketInvestigations, hash-chained to the previous one.
trail, err := f.Audit().Encode()        // canonical JSON, []AuditRecord
_ = f.Audit().Verify()                  // re-derives every hash; cheap, do it on read
_ = f.AuditHead                         // the chain head; store it beside the finding
```

Three things the caller owns that this package deliberately does not:

1. **`Subjects` is a snapshot.** A subject list that changes mid-investigation
   makes the transcript unreplayable, and `subject_index` would name different
   things on two turns. Take it once, per investigation.
2. **`Seed` is the deterministic engine's ranking**, not this package's.
   `Candidate.Score` should be §5.4's `max(savingsUSD, riskScore, anomalyScore)`
   for the question kind. `pkg/reason` ranks what it is given and computes no
   number of its own — inventing one here would be the exact inversion §1.2
   forbids.
3. **Persisting the trail.** `Audit.Encode` produces the bytes; the bucket, the
   retention budget and the `GET /api/v1/clusters/{id}/investigations` route
   are `pkg/api`'s.

For §6's MCP frontend: mount `Registry.Tools()` as `tools/list` (the
descriptors are 1:1 with MCP's shape, `input_schema` included) and
`Registry.Run` as `tools/call`, rendering `Outcome.JSON()` — which uses **real
evidence IDs** rather than session handles, because an MCP client holds its own
session and can resolve them. `Registry.Digest()` is the version for
`tools/list` metadata. The registry needs no provider, which is §5.9's subtle
row: MCP serves the deterministic tools with no model configured at all.

---

## 7. What this unit did NOT build, in priority order

1. **Any `Provider` implementation.** Deliberate and load-bearing: it is the
   air-gap invariant. An `anthropic` organ belongs in its own package
   (`pkg/reason/anthropic`, or an organ repo) importing this one, so
   `go list -deps ./pkg/reason/` stays stdlib + intra-repo. When it lands, the
   one contract worth restating: **a provider that cannot report token usage
   must report an estimate, never zero** — a zero-usage provider makes the
   loop's budget unenforceable, which is the single failure a cost optimizer
   cannot ship.
2. **Nine of §5.2's read-only tools.** `list_clusters`,
   `get_cluster_summary`, `get_plan`/`get_plan_step`, `get_ledger`,
   `cost_attribution`, `run_whatif`, `run_backtest`, `get_pricing`,
   `get_calendar`. Each needs data from a package *above* this one, and the
   shape to use is the `Reader` pattern — a narrow read interface this package
   declares and `pkg/api` implements — **not** an exported registration hook.
   Exporting the tool constructor would end the by-construction argument in §1.
   `cost_attribution` is the cheapest and highest-value next one:
   `explain.WhyCost` is already an intra-repo import away, it is entirely
   deterministic, and §1.3(b) makes it the flagship narration case.
3. **`search_workloads` as §5.2 specifies it.** What shipped is
   `list_subjects`: cluster, kind, key-prefix, limit, offset, stable
   pagination. It has no `class`, `minCostUSD`, `hasRefusal` or `sort`, because
   those live in `pkg/recommend`/`pkg/decision` output that the substrate does
   not hold. Same fix as (2).
4. **Per-identity quotas and the global daily USD cap** (§5.7 item 5, §5.8).
   `Budget` bounds one investigation. `Question.Initiator` is recorded but
   nothing rate-limits on it, and there is no cross-investigation ledger. That
   belongs beside `pkg/api`'s token store, with the two Prometheus metrics §5.7
   asks for.
5. **Prompt caching markers** (§5.4). The prefix is stable by construction —
   `SystemPrompt` is a constant, the scope header is derived, and the tool
   block is name-sorted, `TestTheEmittedSchemaIsStrictAndStable` pins schema
   stability — but nothing marks a cache breakpoint, because that is a
   provider-wire concept and there is no provider here.
6. **`propose_policy_change` / `propose_annotation_change`** (unit 8). Not
   mine, and admitting them needs a *visible* change here:
   `Registry.Register` refuses anything not stamped read-only, so a proposal
   tool means editing that predicate and `tool.go`'s stamp, in a diff a
   reviewer sees. Do not relax it into a `Capability` enum with a
   `CapabilityPropose` member and no other change — the point of the stamp is
   that widening it is conspicuous.
7. **Cross-investigation memory** (§5.4 item 3). Investigations are stateless
   case files, as specified. The operator-curated path (`evidence.Kind =
   "finding"` events pinned on a subject) needs a writer, and the writer must
   not be reachable from a tool — it is an operator action through `pkg/api`,
   not a model action.
8. **A `Store`-backed `Subjects()`.** `RegistryConfig.Subjects` takes a
   snapshot because `evidence.Store` has no enumeration method (`*Memory` does,
   the interface does not). If the bbolt store grows one, the config field
   should stay — the snapshot is a replayability property, not a workaround.

### Smaller things a reader will trip over

- **Tool results cap individual strings at 512 bytes** (`maxDisplayIdent`) and
  attribute values at 128. This is `Outcome.Scrubbed`-counted, never silent,
  but it means the byte cap (`result-too-large`) is provoked by *structure*,
  not by one enormous string. `TestAnOversizeResultIsRefusedRatherThanTruncated`
  builds 500 rows for exactly this reason.
- **`get_recommendation_explain` reports `action: "unknown"`** and no sizing,
  because `explain.BuildExplain` is called with `Rec`/`Verdict` nil — this
  package cannot reach `pkg/recommend`'s output without a seam from above. Same
  fix as (2); the same gap is recorded in `cmd/BRAINWIRE-FINDINGS.md` §6.
- **A turn with neither an answer nor a tool call ends the loop** as
  `malformed-finding`. Prose without the structured output has no citation
  list, so there is nothing to verify and nothing that may be published. A
  future nudge-once-then-fail would be a reasonable refinement; it was not
  worth the extra terminal state here.
- **`handleLines` sorts handles as strings**, so `e10` precedes `e2` in the
  audit record's citation list. Deterministic, and ugly. If it matters, sort by
  the issue ordinal.
- **`Args`/`Result`/`Tool`/`Input` are exported types with no exported
  constructor.** That is intentional (it is the §1 argument) and will look like
  an oversight to anyone who tries to build one from outside.

*Unverified claims are marked `[unverified]`; there are none in this document —
every number above was measured by a test or a command in this working tree,
and the C1/zero-width gap in §2.1 was measured against
`pkg/evidence/evidence.go`'s `cleanString` rather than inferred from its
comment.*
