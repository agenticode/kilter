# `Recommender.Verdicts` — the seam, and the verdict it refuses to invent

Closes the reachability gap named in `cmd/WIRING-FINDINGS.md` §6.4 bullet 1 and
requested by name in `pkg/backtest/backtest.go:416` / `pkg/backtest/FINDINGS.md`
§2. New code lives entirely in `pkg/recommend/verdict.go` (300 lines) and
`pkg/recommend/verdict_test.go` (695 lines). **`recommend.go` was not modified**
— `recommendOne` and `hpaCPUWorkloads` are already package-level, so the seam
needed no edit to the shipped surface at all. `go.mod` and `go.sum` unchanged;
the one new import is intra-repo.

Package coverage **95.3 %**. `gofmt`, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/recommend/...` and `go test -race -short ./...`
(36 packages) all green. No existing test was touched.

---

## 1. The finding: the production path does **not** compute a verdict

This is the first deliverable and it is a negative result.

`pkg/recommend` does not evaluate a decision-quality judgement anywhere, and
the gap is not a wiring oversight — it is structural. Evidence, four ways:

**1.1 No refusal predicate runs.** `pkg/decision` ships eight refusal codes.
Zero of them are evaluated on the production path:

| `decision.RefusalCode` | Evaluated in `recommend`? | Why not |
|---|---|---|
| `insufficient-history` | **no** — see §1.2 | a silent skip, not a refusal |
| `post-change-soak` | no | `recommend` has no change/deploy events (`Evidence.LastChange`) |
| `class-unstable` | no | `patterns.Detector` yields a class; no stability fraction, no `LastClassFlip` |
| `signal-conflict` | no | `oomCount` exists, `ThrottledInWindow` does not; nothing is compared |
| `regime-change-pending` | no | `forecast.SpikeDetector` is not a CUSUM changepoint |
| `forecast-divergence` | no | one forecaster, no remote to disagree with |
| `sla-degraded` | no | no SLO signal reaches this package |
| `quarantined` | no | no quarantine state |

**1.2 The one thing that looks like a refusal isn't one.**
`Recommendations` gates on `st.samples < cfg.MinSamples || window < cfg.MinWindow`
and, at default config, those are the same numbers `decision.Config` documents
itself as matching. That is a **coincidence of defaults between two
independently settable Configs**, not a shared computation — and a gate that
`continue`s produces no `Code`, no `Detail`, and no `Until`. A refusal is a
value you can cite; this is a branch you cannot see from outside the package.

**1.3 No `Action` is chosen.** `decision.Decide` maps confidence onto
act / recommend-only against `Config.ActConfidence`. `recommend` has no such
field. The act threshold lives in `plan.Config.MinConfidence` and is applied
later, in a different package. There is no point on the production path where
"act vs recommend-only" exists to be read out.

**1.4 Confidence is a bare float.** `Recommendation.Confidence` is
`bySamples × byWindow × (1−volatility)` — the very formula
`decision.Compose` reproduces "by construction" — but it is produced **without
a `Basis`**. There are no named terms on this path, and `pkg/explain` renders
`Confidence.Basis` terms as individually-grounded Drivers. Synthesising a
`decision.Confidence{Score: x}` with an empty basis would put a number in the
payload with nothing behind it. Worse: for churn-suppressed containers
`recommendOne` returns before `r.confidence` is ever called, so for that
disposition **no confidence value exists at all**.

**Conclusion:** the honest deliverable is §6.4's second branch. The seam
exposes the dispositions production actually reached and reports the
decision-quality verdict as a typed absence. It does not call
`decision.Evaluate`, and it never will from this file — a second evaluation
site is a second answer.

---

## 2. What production *does* reach, and now says out loud

Per container it considered, `Recommendations` takes exactly one of four
branches. All four are real; three of them were invisible outside the package.

```go
type Disposition string

DispositionRecommended         // a Recommendation was produced; Rec holds it
DispositionNeverObserved       // no observed snapshot ever contained this container
DispositionInsufficientHistory // known, but under MinSamples or MinWindow (Samples 0 included)
DispositionNoSignificantChange // sizing ran; both dimensions inside MinChangeRatio
```

`never-observed` and `insufficient-history` are deliberately distinct.
`ObserveSnapshot` registers state for every container of every pod it sees,
with or without usage — so `insufficient-history` with `Samples: 0` is the
**collector gap** (pod known, no telemetry), while `never-observed` means the
snapshot handed to `Verdicts` was never itself observed. Collapsing them would
have hidden a broken collector behind "young workload".

A `Disposition` is a report of a branch taken. **No `Disposition` is a
`decision.Refusal`**, and the type carries no `Code`/`Detail`/`Until` that
would let one be mistaken for the other.

---

## 3. The seam

```go
func (r *Recommender) Verdicts(snap *model.ClusterSnapshot) []Verdict

type Verdict struct {
    Key            model.ContainerKey
    Disposition    Disposition
    CurrentRequest model.Resources
    CurrentLimit   model.Resources
    Samples        int
    Window         time.Duration
    FirstSample    time.Time
    LastSample     time.Time
    Rec            *Recommendation // non-nil iff Disposition == DispositionRecommended

    state VerdictState     // unexported
    dec   *decision.Verdict // unexported
}

func (v Verdict) State() VerdictState                  // "not-computed" | "computed"
func (v Verdict) Decision() (decision.Verdict, bool)   // the ONLY way in
```

- **Coverage** = `Recommendations`' eligibility filter exactly: Running pods
  (empty phase counts as running), excluding `KindBarePod`/`KindJob`/`KindCronJob`,
  deduplicated by container key. Ineligible containers are absent, because the
  recommender never looked at them.
- **Order** is sorted by `Key.String()`. Deterministic: no map iteration
  reaches the output, no `time.Now()` anywhere — everything derives from the
  snapshot and the learned state. `Verdicts(nil)` returns nil.
- **Locking**: one `r.mu` acquisition for the whole walk, so no concurrent
  `ObserveSnapshot` can split the answer across containers.
- `pkg/recommend` now imports `pkg/decision`, which is the direction
  `pkg/decision`'s own package comment sanctions ("pkg/recommend imports this
  package; this package never imports pkg/recommend"). No cycle.

### 3.1 Why it is not `[]decision.Verdict`

`pkg/backtest` asked for `func (r *Recommender) Verdicts(snap) []decision.Verdict`.
Returning that type would require every element to *be* a verdict, and §1 says
none exists. The returned `recommend.Verdict` **contains** an optional
`decision.Verdict` instead. When the evidence inputs arrive, the element type
does not change — only `state` and `dec` start getting set, in this one place.

### 3.2 How the two absences are kept apart

"No verdict was computed" and "a verdict was computed and it is a refusal" are
different facts, and the type is built so a caller cannot merge them:

- The `decision.Verdict` is **unexported**. `v.Decision.Action` does not
  compile; `Decision()`'s comma-ok is the only route.
- A caller who discards `ok` still cannot land anywhere: the zero
  `decision.Verdict` has `Action: ""`, which equals none of `ActionAct`,
  `ActionRecommendOnly` or `ActionRefuse`. Any `switch` on it falls through to
  default. `TestVerdictNotComputedIsNotARefusal` asserts this explicitly.
- The zero `Verdict` (what a lookup miss yields) reports `not-computed`, not
  an empty string.
- **On the wire**: `MarshalJSON` always emits `"verdictState"` and omits the
  `"verdict"` object entirely when absent, so a JSON consumer reading
  `.verdict.action` finds nothing rather than a falsy disposition. There is no
  top-level `action`, `refusal` or `confidence` key to misread.
- `UnmarshalJSON` re-derives the state from whether a verdict object is
  actually present, so a truncated or hand-edited document claiming
  `"verdictState":"computed"` still decodes to `not-computed`. Tested.

---

## 4. How divergence is made impossible

`Verdicts` and `Recommendations` reach their answer through the **same
`recommendOne` call on the same locked state**, and the contract is that
`Disposition == DispositionRecommended` **iff** `Recommendations` reports that
key on the same snapshot, with `*v.Rec` equal to the served `Recommendation`
value.

`checkNoDisagreement` (verdict_test.go) enforces the full biconditional on
every scenario — both directions, both call orders, no duplicates, and
critically **the silent side**: every non-recommended disposition must carry
`Rec == nil` *and* be absent from `Recommendations`. It also asserts that no
verdict ever claims a `decision.Verdict`.

It runs over three corpora:

1. `TestVerdictsAndRecommendationsCannotDisagree` — 200 seeded scenarios
   (multi-workload, mixed kinds and phases, HPA-on-CPU, OOM restarts, sparse
   and dense history). Exercised on the last run: **recommended 276,
   insufficient-history 401, no-significant-change 103, never-observed 72** —
   and the test *fails* if any disposition count is zero, so the proof can
   never quietly stop covering the silent cases.
2. `TestVerdictsAgreeAtEveryGateBoundary` — each gate walked across its exact
   threshold, because a property corpus samples the space but does not land on
   boundaries, and off-by-one drift is how two paths actually come apart:
   samples ∈ {0, 1, 28, 29, 30, 31}; window ∈ {MinWindow−1ns, MinWindow,
   MinWindow+1ns, 2×MinWindow} (exact spans, asserted exact); and a 101-step
   sweep of the current request through the churn-suppression boundary, which
   must observe both sides.
3. `TestVerdictsAgreeAfterMutation` — agreement survives state changing
   underneath: fresh samples, an OOM bumping the memory floor, and a `GC` that
   drops every learned container.

### 4.1 The proof was mutation-tested

Each mutation was applied to `verdict.go`, the suite run, then reverted. All
eight were caught, by name:

| Mutation | Caught by |
|---|---|
| sample gate off by one (`MinSamples-1`) | boundary sweep, `samples=29` |
| window gate off by one nanosecond | boundary sweep, `span=5h59m59.999999999s` |
| eligibility drift (CronJob considered) | property corpus seed 9 + eligible-set test |
| phase filter dropped (Pending considered) | property corpus seed 1 + eligible-set test |
| HPA-on-CPU guard inverted | recommendation value mismatch, seed 1 |
| churn mislabelled as `recommended` | property corpus seed 4 + disposition test |
| `never-observed` collapsed into `insufficient-history` | disposition-coverage assertion |
| fabricated `decision.Verdict` from `Decision()` | 3 tests, incl. the JSON absence test |

The first two are the important ones: they are precisely the *silent* drift
that a happy-path agreement test would have missed. The corpus alone did **not**
catch the sample-gate mutation — that gap is why the boundary sweep exists.

### 4.2 The one structural gap left, named

`Recommendations` still has its own copy of the eligibility walk and the two
history gates; `Verdicts` does not delegate to it (and could not, without
either a second lock acquisition — which reintroduces the split-answer race —
or rewriting `Recommendations`' body, which is outside this unit's additive-only
scope). Agreement is therefore enforced by test, not by construction.

**The finishing edit, for whoever next owns `recommend.go`** — five lines,
behaviour-preserving, and it makes divergence structurally impossible:

```go
func (r *Recommender) Recommendations(snap *model.ClusterSnapshot) []Recommendation {
	var out []Recommendation
	for _, v := range r.Verdicts(snap) {
		if v.Rec != nil {
			out = append(out, *v.Rec)
		}
	}
	return out
}
```

Note this also makes `Recommendations`' order deterministic (it currently
ranges a map). Nothing in the repo depends on that order — **this edit was
applied and verified here, not merely asserted**: `go build ./...` and
`go test -race -count=1 ./...` (36 packages, full not `-short`) both pass with
it in place. It was then reverted, because rewriting a shipped method's body
is outside this unit's additive-only scope while other agents build against
`pkg/recommend`.

One further invariant `Verdicts` leans on, stated so it cannot rot silently:
**`recommendOne` returns nil exactly at the churn-suppression check and
nowhere else.** A new early return added there must add a `Disposition` with
it. This is documented on `DispositionNoSignificantChange`.

---

## 5. What `cmd/kilter/explain.go` and `pkg/explain` must now do

`pkg/explain` needs **no change**. `ExplainRequest` already has `Verdict
*decision.Verdict` (payload.go:130), `Explanation` already has `Action`,
`Confidence`, `Refusal` and `Notes` (payload.go:89–110), and `ActionUnknown`
already exists for exactly this case. The whole wiring is in `cmd/`.

In `cmd/kilter/explain.go`, add the `github.com/agenticode/kilter/pkg/decision`
import (the file does not import it today — only a comment at line 449 mentions
it) and replace the `Recommendations` scan at lines 400–406 with a `Verdicts`
scan:

```go
var found *recommend.Recommendation
var verdict *decision.Verdict
var note string
for _, v := range rec.Verdicts(series[len(series)-1]) {
	if v.Key != key {
		continue
	}
	found = v.Rec // nil unless DispositionRecommended — same value as before
	if d, ok := v.Decision(); ok {
		verdict = &d
	} else {
		// Do NOT synthesise one. Say which branch production took.
		note = "no decision verdict was computed on the production path; " +
			"the recommender's disposition for this container was " + string(v.Disposition)
	}
	break
}
req := explain.ExplainRequest{ …, Rec: found, Verdict: verdict}
```

`ExplainRequest` has no note input, so attach it to the built payload — after
`BuildExplain`, before `Verify`. That is safe: `Explanation.Verify`
(payload.go:399) re-resolves `Citations` only and does not read `Notes`, and
`Prose` already renders notes (payload.go:622):

```go
if note != "" {
	payload.Notes = append(payload.Notes, note)
}
```

Three rules for that wiring, all of them load-bearing:

1. **`Action` stays `unknown` today.** `Decision()` returns `ok == false` for
   every verdict this package currently produces, so `Verdict` stays nil and
   `BuildExplain` keeps `ActionUnknown`. That is correct and it is the point:
   `unknown` is true, and `refuse` would not be. The payload gets *better*
   because it can now say **why** it is unknown.
2. **Never map a `Disposition` onto a `decision.Action` or a
   `decision.Refusal`.** `insufficient-history` the disposition is not
   `CodeInsufficientHistory` the refusal (§1.2). Surface the disposition as a
   `Note` — `Explanation.Notes` exists — or as its own field. Do not put it in
   `Refusal`.
3. **`Refusal` stays nil until a verdict exists.** `payload.Verify` is the
   publish gate, and a refusal with no evidence behind it has nothing to cite.

The user-visible win now: `kilter explain` on a container the engine said
nothing about stops printing bare `unknown` and starts printing *which of the
four things happened*, with the sample count and window that caused it —
which is what a user running `explain` is asking for. Filling `Action` and
`Refusal` for real needs the evidence inputs in §6.

---

## 6. What `pkg/backtest` can stop working around

Three of its four open findings close against this seam. **All of them are
`pkg/backtest` edits and none was made here** — that package is another
agent's scope.

- **§2 `eligibleContainers` (backtest.go:471–500) deletes.** `Verdicts` returns
  exactly the eligible set, sorted by `Key.String()` — the same order
  `eligibleContainers` sorts into. `TestScoredSetMatchesRecommenderEligibility`
  becomes redundant with `TestVerdictsCoverExactlyTheEligibleSet`; keep
  whichever, but the duplication that "will drift" is gone.
- **§5 `learnState` (backtest.go:414–422) deletes.** `Verdict.Samples`,
  `.FirstSample` and `.LastSample` are the recommender's own counters, past its
  own garbage guards, rather than a mirror that has to reproduce them. The
  alternative `History(key)` accessor floated there is not needed.
- **§2's third consequence: the harness now knows which containers were
  *considered and skipped*, and why** — `decide` can attribute
  `NoSnapshot`/`NoHorizon` style skips against a real disposition instead of
  inferring silence.
- **§3 does *not* close.** `EnforceDecisionRefusals` and `refusalCode` must
  keep calling `decision.Evaluate` themselves, because §1 says there is still
  no refusal on the production path to read. What changes is that the harness
  can now state this precisely: its refusals are the harness's, not
  production's, and `Verdict.State() == VerdictNotComputed` is the machine-
  readable proof. **A scorecard must not present harness-evaluated refusals as
  engine behaviour** — that is the same lie §6.4 refuses, one layer up.
- §4 (evidence fields nobody can fill) is unchanged: it needs collectors.

---

## 7. What would make `VerdictComputed` real

Nothing in this unit sets `state = VerdictComputed`; there is no exported
constructor that can, so the package cannot lie about it. The remaining work,
in order:

1. **Give the recommender its evidence inputs.** `decision.Evidence` needs
   `LastChange`, `LastChangepoint`, `ThrottledInWindow`, `HPAThrashPerHour`,
   `ClassStability`/`LastClassFlip`, the two forecasts, `SLODegraded`,
   `Quarantined`. `recommend` today holds only `Samples`, `Window`,
   `LastSample`, `Class`, `OOMsInWindow` and `ShrinkIndicated`. The rest come
   from the evidence substrate, so the shape is an optional evidence source on
   the `Recommender`, populated by whoever constructs it.
2. **Move confidence to `decision.Compose`.** The three legacy terms
   (`history-depth`, `window-span`, volatility) reproduce today's float
   exactly, so this is back-compatible by construction — and it is what gives
   `pkg/explain` a `Basis` to turn into grounded Drivers.
3. **Give `recommend` an act threshold** (or have `Verdicts` take a
   `decision.Config`), so `Decide` can pick an `Action`. It must be the same
   threshold `pkg/plan` uses, or `explain` will say "act" about something the
   planner declines.
4. **Then, and only then**, `Verdicts` calls `decision.Decide` — once, on the
   production path, inside the same lock — and sets `state`/`dec`. Every
   consumer above already handles both states, so nothing downstream changes.

Until step 4, `kilter explain` reports `unknown` — and now says which of four
things production actually did to get there.
