# The verdict bridge, and the boundary between "thin" and "absent"

Two payloads in `pkg/explain` sounded more certain than their inputs. Both are
closed. New code lives in `pkg/explain/verdict.go` (132 lines),
`pkg/explain/grounding.go` (190 lines) and 180 added lines in `payload.go`;
tests in `verdict_test.go` (527) and `grounding_test.go` (369). **No existing
test file was touched and no golden fixture was regenerated** — `testdata/`
is byte-identical, which is a deliberate design constraint discharged in §4.3.

`gofmt`, `go vet ./...`, `go build ./...`, `go test -race -count=1
./pkg/explain/...` and `go test -race -short ./...` (37 packages) all green.
Package coverage **92.0 %**. `go.mod` and `go.sum` are byte-identical
(`shasum` before and after); the two new intra-repo imports were already
imported by `payload.go`.

**Actuators.** Nothing here moves toward them. `Action` can now hold a real
value, and that value is a *string in a payload*: this package still has no
reference to `pkg/ec2` or `pkg/rds`, no writer of any kind, and
`TestNoSecondEvaluationInThisPackage` (§1.3) fails on any call into another
decision plane, which is the same fence one notch tighter.

---

## 1. Is the verdict bridge buildable without a second evaluation? **Yes — and it is built. It is just not a *verdict* yet.**

### 1.1 The seam does not carry one today, and that is checkable, not assumed

`(*Recommender).Verdicts(snap)` returns `[]recommend.Verdict`. The
`decision.Verdict` inside each one is unexported and reachable only through
`Decision() (decision.Verdict, bool)`, whose `ok` is false for every verdict
the recommender constructs — `Verdicts` sets `state: VerdictNotComputed` on
every element and no exported constructor can set anything else
(`pkg/recommend/VERDICT-FINDINGS.md` §7). So the answer to "did the
operational path produce a verdict?" is a boolean this package can *read*
rather than infer, which is the whole reason the bridge is honest.

Both branches are implemented and both are tested:

- **`ok == true`** → the `decision.Verdict` is copied into the payload
  unchanged and `Action`, `Confidence` and `Refusal` come from it. Nothing is
  re-derived. Reachable today only through the seam's own
  `UnmarshalJSON` (which is how `TestComputedVerdictIsCopiedNotRecomputed`
  reaches it); reachable from production the moment
  `pkg/recommend/VERDICT-FINDINGS.md` §7 step 4 lands, with **no further
  change in this package**.
- **`ok == false`** → the typed "not computed" state, §2. This is what every
  production readout produces today.

### 1.2 The proof that it is a copy and not a recomputation

Three independent proofs, because "we don't recompute" is exactly the claim a
reader should not take on trust.

1. **Behavioural, and adversarial.** `TestComputedVerdictIsCopiedNotRecomputed`
   hands the payload a readout whose verdict says `act`, over a store whose
   evidence a second evaluation refuses. The test *computes the second
   opinion itself* — `decision.Evaluate` over the same subject's evidence,
   assembled the way a caller filling the gap would assemble it — and
   `t.Fatal`s if that second opinion happens to agree, so the test cannot
   quietly stop being a test. The payload must still say `act`, with the
   readout's `Confidence` structurally equal (`reflect.DeepEqual`) to the one
   supplied. A recomputation flips it to `refuse` and the test says so.
2. **Structural, and fail-closed.** `TestNoSecondEvaluationInThisPackage`
   parses every non-test file in the package and fails on any call whose
   receiver is the `decision` or `recommend` package, except an allowlist of
   two type conversions. A *new* `decision.X(...)` call fails the test until
   someone justifies it — the list is of what is permitted, not of what is
   forbidden, so a function that does not exist yet is already covered. It
   also fails if the scan finds no selectors at all, which is how a scan
   silently stops looking.
3. **Arithmetic, by absence.** This package computes no confidence, no
   refusal predicate and no action. `Confidence` is assigned from
   `verdict.Confidence` and from nowhere else.

### 1.3 Where the seam is *not* closed, named exactly

The bridge is buildable; the **verdict** is not, and that limit is
`pkg/recommend`'s, not this package's. `pkg/recommend/VERDICT-FINDINGS.md` §1
establishes with a predicate-by-predicate table that none of `pkg/decision`'s
eight refusal predicates runs on the production path, no `Action` is chosen
(the act threshold lives in `plan.Config.MinConfidence`), and the confidence
that does exist is a bare float with no `Basis` — so there is nothing to read
out. This package re-confirms that from its own side rather than trusting it:
`rv.Decision()` returns `ok == false` for the readout every fixture builds
straight from `recommend.Verdict`.

**So `kilter explain` still prints `unknown`, and that is the correct
answer.** What changed is that it can now say *which* unknown, and why.

---

## 2. "Refused", "not computed" and "unknown" — the type-level difference

These are three facts and the payload previously had two shapes for them.
Every field below is a field a consumer can switch on, in Go and on the wire.

| | `Action` | `Refusal` | `Confidence` | `VerdictOrigin` | `VerdictState()` | `Refused()` |
|---|---|---|---|---|---|---|
| **refused** | `"refuse"` | non-nil, with `Code`/`Detail`/`Until` | non-nil | nil | `computed` | **true** |
| **not computed** | `"unknown"` | **nil** | **nil** | non-nil, `State: "not-computed"`, `Disposition` set | `not-computed` | false |
| **unknown** | `"unknown"` | nil | nil | **nil** | `unknown` | false |

The distinction that carries the weight is the second row against the first.
A `recommend.Disposition` is a report of a branch the recommender took; a
`decision.Refusal` is a judgement with a code you can cite and a time it
clears. `recommend.DispositionInsufficientHistory` and
`decision.CodeInsufficientHistory` share a name and, at default config, the
same two numbers — and they are still two thresholds in two independently
settable `Config`s (`pkg/recommend/VERDICT-FINDINGS.md` §1.2). Rendering one
as the other would be a refusal nobody issued, on a subject nobody judged.

**Prose distinguishes all three**, because the no-model path is what most
operators actually read:

```
Verdict: refuse (post-change-soak) — deploy 6h ago… Clears no earlier than …
Verdict: not computed — the recommendation path reported disposition
         "insufficient-history" (12 samples over 3h0m0s). That is an absent
         verdict, not a negative one.
Verdict: none recorded.
```

### The tests pinning each

| Claim | Test |
|---|---|
| a readout with no verdict is not-computed, not refused, and carries the disposition, samples and window | `TestReadoutWithoutAVerdictIsNotComputedNotRefused` |
| **no** disposition — all four walked — produces a `Refusal`, a refusal `Driver`, a non-unknown `Action`, or a `"refusal"` key on the wire | `TestNoDispositionIsEverRenderedAsARefusalCode` |
| the three states differ in `Action`/`Refusal`/`VerdictOrigin`/`Refused()` **and render three distinct prose lines** (a set is built and asserted to have size 3) | `TestTheThreeStatesAreDistinguishable` |
| the two absences survive JSON as different documents; a decoded not-computed payload does not read as refused | `TestVerdictStateSurvivesJSON` |
| a computed verdict is reported verbatim, not re-derived | `TestComputedVerdictIsCopiedNotRecomputed` |
| the readout's `Recommendation` is the payload's sizing | `TestReadoutSuppliesTheServedRecommendation` |

Five mutations were applied to the shipped files, the suite run, then
reverted. All were caught by name:

| Mutation | Caught by |
|---|---|
| `originOf` reports `VerdictComputed` | 3 tests, incl. the prose-distinctness set |
| a disposition is mapped onto `ActionRefuse` + `CodeInsufficientHistory` | `TestNoDispositionIsEverRenderedAsARefusalCode` (all 4 subtests) |
| the copied verdict replaced by a `decision.Decide` call | `TestComputedVerdictIsCopiedNotRecomputed` **and** the structural scan |
| two verdict sources silently accepted | `TestTwoVerdictSourcesAreRefused` |
| a readout about another container accepted | `TestReadoutMustBeAboutTheSubject` |

### 2.1 Three fabrications the request now refuses outright

A bridge that accepts contradictory inputs invents an answer by choosing one.
`ExplainRequest.validate` therefore rejects, with the reason in the error:

- **Both `Verdict` and `RecVerdict`.** Two sources for one disposition; the
  payload would have to pick and picking silently is the failure.
- **A readout about a different container**, or a container readout on a
  workload subject. Attributing one container's disposition to another
  subject is the same fabrication as inventing it.
- **`Rec` disagreeing with `RecVerdict.Rec`** — either a different sizing, or
  a `Rec` supplied alongside a disposition that carries none. The sizing and
  the disposition must come from the same answer. Identical sizings pass.

---

## 3. The absent-vs-thin boundary, and its arithmetic

### 3.1 The old rule was `len(Citations) == 0`, and it was wrong in both directions

That test conflates "the store holds nothing about this subject" with "the
payload could not cite what the store does hold". A **demonstrated**
false positive, now a regression test
(`TestUsageOutsideTheRequestedTierIsThinNotAbsent`): the dossier's usage
summary queries *every* stored tier, while the `usage-history` driver may only
cite digests of the **requested** tier. Two samples that have not yet rolled
into an hourly digest therefore produce a payload carrying that subject's
usage — `"samples": 2`, real percentiles — under the note *"no evidence is
stored for this subject in this window"*. The payload contradicted itself two
fields apart.

### 3.2 The rule

The state is computed from what the **store returned for the subject**, never
from what the payload managed to cite:

```
own  :=  Digests + Events + Decisions + UsageWindows + Samples + Withheld     (Grounding.Any)
                                                    ── ParentEvents excluded ──

absent    ⟺  own == 0
thin      ⟺  own  > 0  ∧  len(Citations) == 0
grounded  ⟺  own  > 0  ∧  len(Citations)  > 0
unknown   ⟺  no Grounding report on the payload ∧ len(Citations) == 0
```

`GroundingError()` returns a `*NoEvidenceError` (wrapping `ErrNoEvidence`) for
**`absent` and nothing else**. `thin` keeps its payload and no error, which is
the requirement that made this a boundary problem rather than a deletion.

Three terms in that sum are load-bearing and each has a test:

- **`UsageWindows`/`Samples` is the witness a caller cannot suppress.**
  `BuildDossier` computes the usage summary through a query of its own, not
  gated by `MaxDigests`, falling through every tier. So "no digest in the
  requested tier" can never be read as "no usage data" (§3.1's bug), and
  digest absence needs no separate proof.
- **`Withheld` is what makes absence honest under a caller's own caps.**
  `MaxEvents < 0` empties the events section — but the dossier still reports
  how many it dropped, and a drop is proof of existence. Without this term, a
  caller who asked for nothing is told the subject does not exist:
  `TestAbsenceIsNeverClaimedAboutASectionNobodyAskedFor` sets all three caps
  negative over an events-only store (usage deliberately empty, so the
  suppressed section is the *only* evidence) and requires `thin`.
- **`ParentEvents` is excluded on purpose.** `BuildExplain` borrows the parent
  workload's deploys and HPA actions so a container's post-change refusal can
  be explained at all. Those describe the *workload*. A container that never
  ran, under a workload that deploys weekly, would otherwise be declared
  grounded by its parent's history — which is precisely the mistyped-container
  case. `TestParentEventsDoNotGroundTheSubject` builds it: the payload cites
  the borrowed deploy, `Any()` is false, the state is `absent`, the error
  fires, and the note says the citations are borrowed rather than repeating
  "grounds none of it", which would be false in that one case.

`GroundingUnknown` exists so a hand-built or re-decoded payload that never
computed absence cannot assert it. It does **not** produce the error: absence
is a computed fact and only a computed fact may be claimed
(`TestGroundingStateNeverSilentlyClaimsAbsence`).

### 3.3 What was left alone, deliberately

There is no sample-count or window threshold anywhere in this file. "Weak"
is a policy and this package owns no policy — it prices nothing and will not
guess, and `recommend.Config.MinSamples` is not its constant to read. The
boundary above is purely structural: it asks *does a record exist*, never
*is the record good enough*. A caller wanting a stricter notion of thin has
`Usage.Samples`, `Usage.Windows` and the whole `Grounding` block to apply its
own threshold to. `TestThinHistoryStaysGrounded` pins the consequence: five
samples — thin by the recommender's own gate of thirty — is `grounded`, has
no error, and ships.

### 3.4 The mutations

| Mutation | Caught by |
|---|---|
| `absent` collapses into `thin` | 4 tests, incl. the arithmetic table |
| borrowed parent events count as the subject's own | `TestParentEventsDoNotGroundTheSubject` + the table |
| a cap-suppressed section reads as absence (`Withheld` zeroed) | `TestAbsenceIsNeverClaimedAboutASectionNobodyAskedFor` |
| the error widens to cover `thin` (deleting the thin payload) | 4 tests |

---

## 4. Blast radius

### 4.1 Signatures changed: **none**

`BuildExplain(ExplainRequest) (*Explanation, error)` is untouched, and so is
every other exported function. This was a constraint, not an accident:
`pkg/api/explainroutes.go:205` and `cmd/kilter/explain.go:538` are the only
two call sites in the repo and both are being edited by other agents right
now. The typed signal is a method on the payload
(`(*Explanation).GroundingError`), so a caller that has not adopted it
compiles and behaves exactly as before. The cost is that a caller can forget
to call it; §5 gives both call sites verbatim, and both were compiled and
tested here before being reverted.

### 4.2 One behaviour change inside an unchanged signature

`BuildExplain` now returns an error for a `Verdict` whose `Action` is not one
of `pkg/decision`'s three. The zero `decision.Verdict` has `Action: ""`,
which used to reach `Prose` and render as a blank verdict — a payload
asserting a disposition that is not one. `decision.Decide` cannot produce it,
so it is a bug at the call site and now says so.

**Blast radius, enumerated:** every in-repo caller that passes a
`*decision.Verdict`. There are two `BuildExplain` call sites
(`pkg/api/explainroutes.go:205`, `cmd/kilter/explain.go:538`) and **both pass
`Verdict: nil` today**, so neither is affected. `go build ./...` and
`go test -race -short ./...` across 37 packages confirm it. The only two
`explain.ExplainRequest` literals outside this package are those same two call
sites (`cmd/kilter/explain.go:531`, `pkg/api/explainroutes.go:205`); no test
in any other package builds one.

### 4.3 Wire compatibility, and why the goldens did not move

`Explanation` gained two fields, both pointers with `omitempty`, both
populated **only when the payload is not in the good state** —
`VerdictOrigin` only for `not-computed`, `Grounding` only for `thin`/`absent`.
A grounded payload carrying a verdict serializes to the same bytes it did
before, which is why `testdata/golden/explain_recommendation.json` and its
prose fixture are unmodified and pass unregenerated.
`TestGroundedPayloadCarriesNoNewBytes` pins that as an intended property
rather than a coincidence, so a future field cannot silently break the
fixtures either.

The trade-off, stated because it is a real one: on the wire, the *absence* of
`"grounding"` means grounded and the absence of `"verdictOrigin"` means the
verdict was computed. That is the inverse of the pattern
`recommend.Verdict.MarshalJSON` uses (always emit `verdictState`), and it is
weaker. Two things make it safe here. `Action` is a total function of the
verdict's existence — only a supplied `decision.Verdict` can set it to
anything but `unknown`, now that §4.2 rejects the third possibility — so
`VerdictState()` reconstructs `computed` from a field that is always present.
And `GroundingState()` falls back to `GroundingUnknown`, never to
`GroundingGrounded`, when a payload has neither a report nor citations. Both
fallbacks are tested against decoded documents, not just constructed ones.

### 4.4 Callers that read `Explanation.Notes` by string

The absent case's note text is **unchanged, character for character**, and
the new thin-case note deliberately does *not* contain the substring
`"no evidence is stored"`. Anything matching on that phrase keeps working and
stops matching the cases where the phrase was false.

---

## 5. Caller changes specified but NOT made

Both were applied in this worktree, compiled, run against their existing
suites (`pkg/api` and `cmd/kilter`, both green), verified end to end against
the §6.1 scenario, and then **reverted**. `pkg/api` and `cmd/` are other
agents' scope.

### 5.1 `pkg/api/explainroutes.go` — the 422

In `(*Brain).Explain`, after the `Verify` gate (~line 219):

```go
	// §5.7's publish gate, before anything is shown.
	if err := payload.Verify(explain.Resolver{Store: b.mem}); err != nil {
		return nil, unverifiable{err}
	}
	// A subject the substrate holds no record of is a 422, not a 200 carrying
	// a payload that grounds nothing (§3.2; cmd/BRAINWIRE-FINDINGS.md §6.1).
	// Thin evidence is NOT this case and still returns its payload.
	if err := payload.GroundingError(); err != nil {
		return nil, notEnoughEvidence{err}
	}
	return payload, nil
```

`explainStatus` already maps `notEnoughEvidence` to
`http.StatusUnprocessableEntity`, so this is the whole change: no new error
type, no route edit. Verified: `go test ./pkg/api/...` green with it applied
— no existing test asserts a 200 for an unknown subject.

**A choice left to that package:** `writeErr` discards the payload, so the 422
carries a message rather than the (honest, window-stating) document. Serving
the payload *with* a 422 status is a one-line alternative in the route and is
arguably better for a UI. Not decided here.

### 5.2 `cmd/kilter/explain.go` — the non-zero exit

At the end of `runExplainTo` (~line 545), replacing the tail call:

```go
	if err := renderExplanation(w, key, start, end, payload, *jsonOut); err != nil {
		return err
	}
	// The payload is printed first — it is honest and it names the window —
	// and then the command fails, because the subject the operator asked
	// about has no record in the substrate.
	//
	// Returned unwrapped: the error already names the package, the subject
	// and the window, and a second "explain:" prefix reads as a stutter.
	if err := payload.GroundingError(); err != nil {
		return err
	}
	return nil
```

`explainFromBrain` (the `--db` path) needs **no edit**: it returns whatever
`brain.Explain` returns, so §5.1 gives it the non-zero exit for free — at the
cost of not printing the payload, which is the same trade-off §5.1 names.

Verified end to end against `cmd/BRAINWIRE-FINDINGS.md` §6.1's exact
scenario, using the repo's own `cluster.json` fixture and a one-character
container typo:

```
kilter explain — Deployment/default/api/ap over [2026-08-23T12:15:00Z, 2026-08-26T12:00:01Z]

Subject prod-eks/container/Deployment/default/api/ap over 2026-08-23 12:15Z → 2026-08-26 12:00Z.
Verdict: none recorded.
Because:
  note: no evidence is stored for this subject in this window; the payload states the decision but grounds none of it

$ echo $?   # was 0
1
error: explain: no evidence is stored for subject
       prod-eks/container/Deployment/default/api/ap in [2026-08-23T12:15:00Z, 2026-08-26T12:00:01Z)
```

The real container (`api`) still exits 0 in the same run — asserted in the
same throwaway test, which was deleted with the reverts.

### 5.3 `cmd/kilter/explain.go` — the verdict readout

This is `pkg/recommend/VERDICT-FINDINGS.md` §5's wiring, updated for what
this package now offers. That document's advice was to pass
`Verdict: nil` plus a hand-built `Note` appended after `BuildExplain`; do
**not** do that any more. Pass the readout and let the payload type carry it,
so the disposition lands in a typed field instead of a string a consumer has
to parse.

Replace the `Recommendations` scan at `cmd/kilter/explain.go:523–529` and the
request literal that follows it (`:531`):

```go
	var readout *recommend.Verdict
	for _, v := range rec.Verdicts(series[len(series)-1]) {
		if v.Key == key {
			readout = &v
			break
		}
	}

	req := explain.ExplainRequest{
		Cluster: cluster,
		Subject: evidence.ContainerSubject(cluster, key),
		From:    start, To: end,
		Store:      store,
		RecVerdict: readout, // supplies Rec too; do not also set Rec
	}
```

Three rules, all load-bearing:

1. **Do not set `Rec` as well.** `RecVerdict.Rec` is byte-for-byte the
   `Recommendation` production served for that key on that snapshot, and
   `BuildExplain` takes it. Setting both is accepted only if they are
   identical and is an error otherwise (§2.1) — passing just the readout
   makes the question moot.
2. **`Action` stays `unknown`, and that is the win.** `Decision()` is
   `ok == false` for every readout the recommender produces today, so the
   payload says `not computed` and names the branch, instead of a bare
   `unknown` that could mean anything.
3. **Never map a `Disposition` onto a `decision.Action` or a
   `decision.Refusal`.** The type gives you nowhere to put it, which is on
   purpose: `VerdictOrigin.Disposition` is a separate field precisely so it
   cannot be mistaken for a refusal code.

`readout = &v` is safe under Go 1.22+ loop semantics (`go 1.26.4` in
`go.mod`), where each iteration has its own `v`.

Verified: applied here, `go build ./...` and `go test ./cmd/...` green, then
reverted. The user-visible change on the repo's own fixture, which is what an
operator running `explain` is actually asking for:

```
- Verdict: none recorded.
+ Verdict: not computed — the recommendation path reported disposition
+          "recommended" (288 samples over 71h45m0s). That is an absent
+          verdict, not a negative one.
```

Note what that says: the recommender *did* produce a recommendation for this
container — the sizing is right there in the payload — and still reached no
decision-quality verdict, because nothing on that path evaluates one. A bare
`unknown` could not tell those two things apart.

The same change applies to `pkg/api`'s `(*Brain).Explain`, which today scans
`b.Recommendations(cluster)` for the key: `b.rec.Verdicts(snap)` yields the
readout and the recommendation together, so the two can no longer come from
two different reads of the recommender. **[unverified]** — unlike §5.1 and
§5.2 this variant was not compiled here, because it needs a snapshot argument
that `Brain.Explain` does not currently hold in scope.

---

## 6. What would make `Action` real, and what this package will need

Nothing, in this package. When `pkg/recommend/VERDICT-FINDINGS.md` §7 step 4
sets `state = VerdictComputed`, `rv.Decision()` starts returning `ok == true`,
`BuildExplain` copies the verdict, `Action` becomes `act` /`recommend-only` /
`refuse`, the `Confidence.Basis` terms become individually-grounded
`Driver`s through the code that already exists, and `VerdictOrigin` stops
being emitted. The path is exercised today by
`TestComputedVerdictIsCopiedNotRecomputed` — the branch is not speculative
code, it is tested code waiting for a caller.

One thing that will need attention when it lands, flagged now: a refusal
`Driver` must be citable, and `refusalCitations` grounds each code in the
evidence class that justifies it. A refusal arriving for a subject whose
grounding state is `thin` will have its driver dropped and counted in
`Ungrounded`. That is correct behaviour — an ungrounded reason is worse than
a missing one — but it means a refusal can be *reported in `Action` and
`Refusal`* while having no `Driver`. The payload already says so via
`Ungrounded` and its note. **[unverified]**: no fixture exercises a refusal
over a thin store today, because no production path produces a refusal at
all.

## 7. Determinism

No clock and no map iteration is reachable from any new code. Both new fields
are computed from integer counts already in the dossier, and the dossier is
itself deterministic. `TestAbsentPayloadIsDeterministic` marshals the absent
payload 16 times over independently built stores and requires byte equality;
the pre-existing `TestExplainIsDeterministic` still covers the grounded one.
The `Window` in `VerdictOrigin` is copied from the readout, never derived from
`From`/`To`, so it cannot drift with the requested window.
