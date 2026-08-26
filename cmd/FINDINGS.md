# J1 — wiring: eight domain packages become reachable from the binary

`kilter domains` runs observe → recommend → report across every compute domain
built in the last four waves, and `kilter analyze --fargate` reaches the ninth.
Before this unit, a user who built `kilter` got none of U2–U6, U8 or U9: the
decision logic was in the repo and the binary could not call it.

**Status:** `gofmt`, `go vet ./...`, `go build ./...` and `go test -race -short
./...` are green. `go.mod` and `go.sum` are **unchanged** — every import added
is stdlib or intra-repo, and `go list -deps ./cmd/kilter/... | grep aws-sdk`
finds nothing new on the domain path. No existing test was modified or
weakened.

| | |
|---|---|
| New/changed production code | ~2,990 lines across `pkg/domain/` and `cmd/` |
| Tests | ~3,290 lines, 62 tests |
| Coverage | `pkg/domain` **89.0 %**, `pkg/domain/ecs` **84.8 %**, `pkg/domain/lambda` **86.7 %**, `pkg/domain/ec2` 63.7 % (its remaining paths are covered through the CLI integration tests, which count against `cmd/kilter`) |

```
pkg/domain/actuation.go     actuator table + Execute/Revert — the gate a domain cannot write to
pkg/domain/composite.go     one Kind, several halves: the pkg/ebs collision resolution
pkg/domain/refusal.go       Refusal + Refuser: targets that produced no recommendation
pkg/domain/plan.go          Plan + BuildPlan: a refusal is the product, with a code
pkg/domain/report.go        Summarize/Report/Validate/WriteText: the cross-domain roll-up
pkg/domain/snapshot.go      + Snapshot.Payload (the opaque per-domain payload U9 asked for)
pkg/domain/registry.go      + step validation on the way OUT of a domain
pkg/domain/ledger.go        + Inventor: hand pkg/ec2 the inventory it needs
pkg/domain/ec2/             the `ec2` domain: pkg/ec2 adapter + pkg/ebs wrapper + composite
pkg/domain/ecs/             pkg/ecs adapter (one snapshot per cluster, one domain per account)
pkg/domain/lambda/          pkg/lambda adapter (payload ingest + refusals)
cmd/kilter/domains.go       kilter domains list | report | plan
cmd/kilter/analyze.go       + --fargate: the U3 crossover report
```

---

## 1. What is reachable now, and what it says

One command, over recorded snapshots, no cloud call:

```
$ kilter domains report --now 2026-08-26T12:00:00Z \
    --snapshot ec2-instances.json --snapshot ec2-volumes.json \
    --snapshot ecs-services.json --snapshot lambda-functions.json \
    --kube-snapshot cluster.json

  DOMAIN         STATE     ACTUATION   TARGETS    RECS REFUSED    CLAIMABLE        GROSS
  ec2            ready     report-only       7       3       4      $214.06      $262.24
  ecs-fargate    ready     report-only       2       5       1      $419.50      $419.50
  k8s-fargate    ready     report-only       1       1       0       $32.80       $32.80
  lambda         ready     report-only       2       2       0        $0.00        $0.00
  TOTAL                                             11       5      $666.36      $714.54
```

That `$32.80` is pkg/domain/fargate's own §4.1.1 figure — the pod that requests
1 vCPU / 8 GiB, is billed 2 vCPU / 9 GB because Fargate adds 256 MiB before
rounding, and drops a whole vCPU when the request is shaved under the tier
boundary. It survives the wiring unchanged, which is what
`TestFargateOverheadCliffSurvivesTheWiring` pins. The same fixture's Fargate VM
reports 96 vCPU / 384 GB; if that shell had reached node math (§7 trap 7) the
number would be nonsense.

Add the commitment inventory and one line of that table changes:

```
$ kilter domains report … --commitments commitments.json
  ec2            ready     report-only       7       1       6        $3.82        $3.82
  …
  What kilter declined to do, and why
    commitment-neutral                  1 refused
```

The $210.24 EC2 recommendation did not get smaller and did not disappear. It
became a refusal with a stable code and an expiry date. That gap — between the
number a naive optimizer reports and the number that reaches an invoice — is
§7 trap 1, and `TestCommitmentWaterfallSuppressesAGrossSaving` asserts the
aggregate falls by *exactly* that recommendation's claim and by nothing else.

### Commands

| Command | What |
|---|---|
| `kilter domains list` | registered domains, health, whether an actuator is wired |
| `kilter domains report` | observe → recommend → report, per domain and across all |
| `kilter domains plan` | executable steps per domain; never executes |
| `kilter analyze --fargate` | the U3 Fargate ⇄ EC2 crossover, advisory |

Input is recorded `domain.Snapshot` JSON, routed by its own `domain` field, plus
the `model.ClusterSnapshot` format `analyze --dump-snapshot` and `simulate`
already write. `--now` fixes the decision time; **the clock is read once, in
`parseNow`, and passed down**. Nothing below that line calls `time.Now`.

---

## 2. The `pkg/ebs` Kind collision: resolved with a composite, not a new Kind

pkg/ebs/FINDINGS.md §6.3 offers two resolutions. **We took the composite.**

`domain.Composite` (`pkg/domain/composite.go`) *is* a `domain.Kind` and fans
`Learn`/`Recommend`/`PlanSteps`/`Health`/`Checkpoint` out to the halves that
compose it. `pkg/domain/ec2` builds the one that matters: pkg/ec2 (instances)
and pkg/ebs (volumes), both reporting `domain.EC2`, behind one registry entry.

**Why not add an `ebs` Kind.** It is a one-line change to a closed set, and it
would have been wrong for three reasons, in increasing order of severity:

1. **A Kind names a billable target scope, not a Go package.** §5.1 places
   volumes under the EC2 domain; both halves share the `accountID/region`
   scope; one cloud agent, one IAM role and one `DescribeVolumes`-adjacent API
   surface collect both. Splitting the Kind would make the *registry* the only
   thing that thought they were different.
2. **It would not remove the composite, only move it.** A `kilter domains
   report` still has to present one `ec2` answer, and the account-wide
   commitment baseline still has to splice instance and volume usage lines into
   one `commit.Usage` — Compute Savings Plans absorb account-wide, so a
   per-Kind ledger would over-claim (§4.4 ex.3). We would have gained a second
   registry entry and kept the merge.
3. **It would silently break the shipped EBS actuator.** `pkg/ebs` bakes
   `domain.EC2` into every `domain.StepKey` it computes — the key is
   `hash(domain, scope, id, from, to)` — and its actuator re-derives that key to
   decide whether a step is already applied. Re-labelling its steps `ebs` would
   change every idempotency key, so a plan interrupted mid-flight would resume
   by re-issuing a `ModifyVolume` it had already issued. Every alternative to
   the composite requires either editing pkg/ebs (out of scope) or a
   re-stamping layer that lies about step identity.

The composite is not free, and the cost is stated in the code: two rules had to
be made explicit rather than inferred.

- **`Part.Owns`** — which recommendations a half can execute. Supplied by the
  wiring (`pkg/domain/ec2`: volume-type attribute, ID prefix as fallback),
  never guessed by the core. `pkg/domain` must not invent knowledge about what
  its parts own.
- **`Part.Accepts`** — which snapshots a half is addressed by. This one came
  out of a real bug; see §5.1.

### The report-only edge, and why the permissive merge is safe

`Composite.Health` reports `ReportOnly` only when **every** half is
report-only — otherwise a wired EBS actuator would be unreachable because
plain-EC2 actuation does not exist. That looks like a hole, and it is closed one
level down: `Composite.PlanSteps` re-checks **each half's own** report-only
state before routing anything to it. The instance half never receives a step,
however healthy its sibling is.
`TestCompositeReportOnlyHalfNeverGetsAStep` asserts both directions, including
that the report-only half's `PlanSteps` is never called at all.

`Guard.MaxSteps` is applied **after** the merge, once. A cap applied per half
would let a two-part domain execute twice the plan the operator authorized.

---

## 3. Report-only enforcement: what the core actually guarantees

The unit's requirement is that the wiring must not create a path around U2's
report-only enforcement, and that a hostile domain claiming actuatability is
still blocked by the core. Meeting it honestly required naming what the existing
gate does and does not cover.

**What U2 already closed.** `Registry.PlanSteps` refuses to plan for a domain
whose `Health` says report-only, and refuses in the core rather than trusting
the domain's own `PlanSteps` to do it. That handles the *honest* failure: a
domain that knows its collector is gone. `TestReportOnlyDomainCannotPlanSteps`
(pre-existing, untouched) pins it.

**What it did not close.** A `Domain` is ordinary Go code. Its `Health` can
return `{ReportOnly: false}` on a domain with no collector, no credentials and
no actuator. Nothing a domain says about itself can be the last word on whether
it may touch a cloud account.

**What this unit added.** Three gates, none of which a domain can write to:

1. **The actuator table** (`pkg/domain/actuation.go`). An `Actuator` gets in
   exactly one way: someone in `cmd/` wrote the line, having first built the SDK
   client it needs. A domain never sees the `Registry`.
   `Registry.Execute`/`Revert` refuse a step whose kind has no actuator. A
   hostile domain can fabricate recommendations, lie about its Health and hand
   back steps; the steps go nowhere.
2. **Output validation** (`validateSteps`, `pkg/domain/registry.go`). This
   closed a real hole. `Registry.PlanSteps` filtered its *input* — suppressed
   and foreign recommendations never reached a domain — but did not check the
   *output*. A step is routed for execution by `Step.Target.Domain`, so a domain
   returning a step naming a **different** domain would be borrowing that
   domain's actuator and its credentials. Steps are now required to name their
   producer, to carry a valid action class (the executor's disruption accounting
   keys off it) and to carry an idempotency key (without one, a resumed plan
   re-applies). `TestDomainCannotEmitAStepForAnotherDomain` builds exactly that
   attack and asserts the victim's actuator is never reached.
3. **Execution-time re-evaluation.** `Registry.Execute` re-checks
   registration, report-only *as of `g.Now`*, the actuator, and the guardrails.
   A plan legal when built is refused when executed late.
   `Registry.Revert` deliberately skips the change window, freeze and the
   breaker: a revert undoes a change Kilter already made, and those switches are
   usually on *because* something is wrong, which is when a revert is most
   needed.

**A plan is still built for a lying domain, on purpose.** `BuildPlan` produces
reviewable steps and sets `Actuatable: false`; the renderer prints
`NOT RUNNABLE: no actuator is wired`. Refusing to show the plan would hide the
problem rather than contain it, and nothing runs either way.

---

## 4. Money: Net ≤ Gross through the CLI

`Report.Totals.ClaimableMonthlyUSD` is the only figure the CLI presents as a
saving. Every term in it is a `commit.Bill()` delta, because
`Recommendation.SetSavings` is the only supported way to populate the field and
it clamps net to gross; the `pkg/domain/ec2` adapter re-asserts it at the seam
rather than trusting a number that crossed a package boundary.

The account-wide ledger is built in `cmd/kilter/domains.go:buildLedger`, in two
passes. The first asks each domain that can for its priced inventory as usage
lines — a figure that does not depend on any commitment, because it is what is
running today at on-demand rates. The second nets every domain's proposed change
against that whole picture. A per-domain baseline would understate absorption
and overstate the saving (§4.4 ex.3).

**One aggregation subtlety worth stating**, because it is where a naive roll-up
breaks the invariant. Gross sums `max(0, gross)` per recommendation, not raw
gross. A domain may honestly recommend a change that *costs* more — a
safety-driven growth, an under-provisioned Lambda — and such a recommendation
carries a negative gross and a zero claim. Summing raw gross lets one negative
drag the total below a positive claimable sum and manufacture a `net > gross`
violation out of two individually honest numbers. The clamp makes the aggregate
invariant a consequence of the per-recommendation one;
`GrossIncreaseMonthlyUSD` carries the other side so the clamp hides nothing, and
`WriteText` prints it. `TestAggregateNeverClaimsMoreThanGross` is the case.

`Report.Validate()` checks Net ≤ Gross at the recommendation, the domain and the
totals; the CLI calls it before printing and **fails loudly** rather than
printing a number somebody might put in a business case.
`TestReportValidateCatchesEachViolation` corrupts a valid report fifteen ways
and requires each to be caught.

### Refusals are the product

Three levels, all rendered:

| Level | Carrier | Example |
|---|---|---|
| Domain | `Health.ReportOnly` + `Reason` | "no actuator is wired" |
| Recommendation | `Suppressed` + `SuppressCode` | `fargate-spot`, `mode-recommend` |
| Target | `domain.Refusal` (**new**) | `single-memory-point`, `not-gp2`, `memory-blind` |

The third level is new and it is most of the output on a real account. A
`Recommendation` cannot express it — `Validate` rejects one whose `Proposed`
equals its `Current`, correctly — so every domain had put its refusals in a
package-local report type that the generic seam dropped on the floor. A fleet
that has never been power-tuned yields *nothing but* `single-memory-point`
refusals, and rendering only recommendations would let a reader conclude the
tool found nothing. That is a different claim from "the tool declined to guess,
and here is what it needs". `domain.Refuser` is the optional seam;
`pkg/domain/ec2`, `pkg/domain/ecs` and `pkg/domain/lambda` implement it by
projecting each package's own report.

---

## 5. Findings in other packages

Reported, not fixed — every one is outside this unit's scope.

### 5.1 `pkg/ebs.Domain.Learn` cannot tell "not mine" from "my collector found nothing" — **real bug, worked around**

Found by `TestOutputIsByteIdenticalAcrossRuns`, not by reading the code.

Two collectors feed the `ec2` kind: pkg/ebs ships the generic
`Targets`/`Samples` shape and pkg/ec2 ships an opaque `Payload`. Handed the
instance snapshot, `pkg/ebs.Domain.Learn` saw zero volume targets and concluded
its collector had delivered no volumes, degrading itself with
`partial collection: collector delivered no volumes`. Whichever snapshot
arrived last decided the domain's health, so **the same inputs in a different
order produced a different report** — a different `ec2` health line, and a
different plan refusal.

That conclusion is correct for pkg/ebs's own contract (one collector, one
domain, one snapshot). It is wrong inside a composite. Worked around with
`Part.Accepts`, deliberately narrow: reject only a snapshot whose *entire*
content is another half's opaque payload. An EBS snapshot with no volumes is
still accepted, because an empty account is a real answer and "we looked and
found none" must stay distinguishable from "we never looked".

**The fix belongs in pkg/ebs**: `Learn` should distinguish a snapshot addressed
to it and empty from a snapshot that is not addressed to it at all — most simply
by treating a snapshot carrying a foreign `Payload` and no volume targets as a
no-op rather than as evidence about its collector.

### 5.2 `pkg/ec2` reports a commitment block as a refusal; every other domain reports it as a suppressed recommendation

Same condition, two shapes. pkg/ebs, pkg/ecs and pkg/lambda keep a *suppressed
`Recommendation`* with `SuppressCode: commitment-negative`; pkg/ec2 clears
`Assessment.Proposal` and records a `Suppression`, which projects to a
`domain.Refusal`. Both stay visible with prose and `ValidFrom`, so §5.7 is
satisfied either way — but the same block lands in
`Totals.RefusedByCode` for one domain and `Totals.SuppressedByCode` for the
others, and a UI grouping by suppression code will under-count EC2.

Not fixed here: making them agree means changing which of the two a package
emits, and both are defensible. A later unit should pick one and say so in
`pkg/domain`'s doc comment. In the meantime the CLI's
`TestCommitmentWaterfallSuppressesAGrossSaving` accepts either.

### 5.3 `pkg/lambda`'s two ingest paths can now be collapsed

pkg/lambda/FINDINGS.md §8 asked for "an opaque per-domain payload on Snapshot,
or a typed Evidence union" and said a later unit should add it. `Snapshot.Payload`
is that field, and `pkg/domain/lambda.Learn` routes a payload to
`Observe`. `TestPayloadPreservesREPORTEvidence` drives the *same* snapshot both
ways and asserts the payload path makes a measured two-point claim while the
generic path honestly refuses with `no-report-evidence`.

**pkg/lambda can now collapse `Learn`/`Observe` into one method** and delete the
limitation section. The adapter in `pkg/domain/lambda` exists only to bridge the
gap and should shrink to nothing when it does.

### 5.4 `pkg/ec2` needs `*commit.Inventory`, the seam offers `domain.Netter`

Recorded in pkg/ec2/FINDINGS.md §6.5 as a contract note. pkg/ec2 constructs the
account-wide before/after usage itself — which is *correct*, not a shortcut —
and needs the inventory to do it. Bridged with `domain.Inventor`, an optional
interface on `Netter` that `*domain.Ledger` implements. A `Netter` that does not
implement it means "no inventory to share", and the domain degrades to
net == gross: under-claiming, never inventing.

No change to pkg/ec2 is required. If a later unit unifies the two, the
`Inventor` assertion in `pkg/domain/ec2` is the single line to delete.

### 5.5 Smaller notes

- **`pkg/ecs` FINDINGS §3.1** predicted the `domain.Domain` adapter would be
  thin. It is ~300 lines, and three things genuinely belong in it rather than in
  pkg/ecs: one snapshot per *cluster* vs one domain per *account*, freshness
  (a stateless sizer has no opinion about how old its input is), and actuation
  availability (a fact about `cmd/`).
- **`pkg/ecs.Sizer` confidence needs more than the minimums.** Confidence
  composes history depth against `2 × MinSamples` and window span against
  `2 × MinWindow`, so a fixture that merely clears `MinSamples`/`MinWindow`
  scores below `MinConfidence` and refuses. Not a bug — confidence is earned —
  but it is a sharp edge for anyone building a fixture, and it cost time here.
  Recorded in the generator's comments.
- **`pkg/recommend.Checkpoint()` returns states in map order** (pkg/domain's own
  FINDINGS §3.1 flagged it as latent). Still latent; `Composite.Checkpoint`
  sorts its parts, so nothing here depends on it.

---

## 6. Deliberately deferred

The single largest item, and the reason `kilter domains plan` refuses for every
domain today:

1. **No SDK adapters, therefore no collectors and no actuators.** Every domain
   package defines read/write seams over plain Go structs and documents the
   mechanical field copy that binds them to `aws-sdk-go-v2`
   (pkg/ebs §6.1, pkg/ecs §4.1, pkg/lambda §10, pkg/ec2 §6.2). Writing them is
   several hundred lines of adapter per domain that **cannot be tested
   air-gapped**, and this unit's budget went to the wiring and its tests
   instead. Consequence, stated plainly: `cmd/` registers **no actuator**, so
   `CanActuate` is false everywhere, `Health` is report-only everywhere, and
   `domains plan` prints a refusal per domain.
   `TestNoDomainCanPlanAStepInThisBuild` asserts exactly that — it is meant to
   fail, and be updated, the day an actuator is wired.
   `Registry.Execute`/`Revert` are implemented and tested against fakes, so the
   execution path is ready for the first real actuator.
   One cheap intermediate step: `pkg/ebs.Fixture` already satisfies
   `ebs.ModifyAPI`, so a `--ebs-fixture` flag could wire a dry-run actuator over
   a recorded EC2 endpoint and exercise plan → execute end to end offline.
2. **`k8s-nodes` is not a domain.** The existing pipeline
   (`collect` → `recommend` → `binpack` → `plan` → `actuate`) predates the seam
   and does not implement `domain.Domain`; adapting it means deciding whether
   `plan.Plan` becomes a list of `domain.Step`s, which is a design question, not
   wiring. `--domain k8s-nodes` is refused with "known but not wired into this
   binary" rather than silently ignored.
3. **No `domains apply` verb.** There is nothing to apply. Adding the verb
   before an actuator exists would put an unreachable code path in front of the
   most dangerous operation the binary has.
4. **No persistence.** `Domain.Checkpoint`/`Restore` are implemented on every
   adapter (and `Composite.Checkpoint` is byte-stable and order-independent),
   but nothing writes them to `pkg/store` under the §5.8
   `domain/<kind>/<scope>/…` buckets. Every CLI invocation starts cold. That is
   correct for a report command and wrong for the brain: pkg/ebs's cooldown and
   pkg/domain/fargate's pending reverts both live in a checkpoint, and losing
   one strands a pod at the size that broke it.
5. **No API routes.** §5.8's `GET /v1/domains`,
   `GET /v1/domains/{kind}/recommendations` and
   `POST /v1/domains/{kind}/snapshots` are not added; `pkg/api` is outside this
   unit's scope. `domain.Summarize` and `Registry.Health` are shaped to serve
   them directly.
6. **`kilter pricing sync-commitments`** (U4's CLI half) is not implemented.
   `--commitments` reads a `commit.Inventory` JSON file, which is the same
   format that command would write.
7. **The crossover is not netted against commitments.** pkg/crossover
   FINDINGS §7.2 states it, `Assumptions[2]` prints it on every report, and the
   direction of the error is stated there too. `analyze --fargate` renders the
   report as-is; wiring `commit.Inventory` into it changes a signature this unit
   should not invent.
8. **`analyze --fargate` blocks every EC2-hosted pod today.** Five of nine gate
   facts are not derivable from `pkg/model` (pkg/crossover FINDINGS §8.3), so
   the EC2 → Fargate direction is honest-but-unhelpful until `pkg/collect` fills
   them. The Fargate → EC2 direction works fully.
9. **`--kube-snapshot` feeds only `k8s-fargate`.** A cluster snapshot could also
   drive the crossover from `domains report`; it is one call and was left out to
   keep the two commands' outputs from overlapping.

---

## 7. Fixtures, and why they are generated

`cmd/kilter/testdata/` holds five recorded snapshots plus a commitment
inventory, ~1.7 MB. They are **generated by `cmd/kilter/domainfixtures_test.go`
and committed**, and `TestWriteDomainFixtures` asserts the committed bytes still
match what the generator produces:

```
go test ./cmd/kilter -run TestWriteDomainFixtures -update-fixtures
```

Two reasons for generating rather than hand-writing. A 7-day window of
5-minute datapoints is 2,016 objects per metric per target — nobody reviews
that, and the evidence gates in every domain are *specifically* about window
span and sample coverage, so a fixture thin enough to read by hand exercises
only the refusal paths. And the EC2 and EBS fixtures run through those
packages' **real collectors**, so the committed bytes are a contract test on
the collector: a regeneration that changes a file means a collector changed,
which is exactly when a reviewer should look.

The fixtures are chosen so that each is the interesting case rather than the
easy one:

| Target | Why it is there |
|---|---|
| `i-…a` m5.2xlarge with a memory signal | the only instance that can be proposed — and the only m5 in the account, which is what makes the reservation strand |
| `i-…b` r5.xlarge with **no** memory series | §7 trap 4: CloudWatch has no memory metric by default; the counterfactual refusal names the shape it declined |
| `i-…c` t3.large, credits at zero | §7 trap 5: low CPU is a ceiling AWS imposed, not demand |
| `i-…d` c5.large tagged `kubernetes.io/cluster/prod` | belongs to the k8s-nodes pipeline, not here |
| `vol-…1` 200 GiB gp2 | converts to gp3, floored at gp2's delivered baseline |
| `vol-…2` 350 GiB gp2 | the 334–375 GiB band where gp3 is **not** cheaper at parity |
| `vol-…3` gp3 | `not-gp2` |
| ECS `web` | a real task-size drop, 4 vCPU/8 GB → 1 vCPU/3 GB |
| ECS `migrator` | mid-deployment: refused |
| Lambda `thumbnailer` | measured at **two** memory settings — the only shape that can carry a claim |
| Lambda `webhook` | one setting: `single-memory-point`, the fleet-wide honest default |
| Fargate pod | the §4.1.1 overhead cliff, on a VM that reports 96 vCPU / 384 GB |
| RI `m5.2xlarge` | size-flexible, with no second m5 to move to |

That last row is the subtle one. A regional Linux/default-tenancy RI is
size-flexible: its normalization units follow the family anywhere in the region,
so with a second m5 in the account the discount simply moves and the saving is
**real**. `pkg/pricing/commit` gets that right, and a fixture that ignored it
would prove nothing. The account holds exactly one m5, so twelve of sixteen
units have nowhere to go.

---

## 8. Determinism

- No map iteration reaches output. `Registry.Kinds`, `ActuatableKinds`,
  `Composite.PartNames`, `countCodes`, `mergeCodeCounts` and every refusal and
  recommendation list sort by an intrinsic key.
- Float sums accumulate in canonical order — recommendations are sorted before
  anything is added up — because float addition is not associative and two runs
  over the same data must agree to the last bit.
- `TestOutputIsByteIdenticalAcrossRuns` runs each subcommand nine times **in one
  process** (Go randomizes map order within a process, so repeating in-process is
  the real test) and once more with the snapshots supplied in a different order.
  It is the test that found §5.1.
- `TestSummarizeIsDeterministicUnderShuffle` shuffles registration order forty
  times and requires byte-identical text.
- `Composite.Checkpoint` sorts parts by name, so the persisted bytes do not
  depend on wiring order — asserted against a composite built with the parts
  swapped.
- `grep -n 'time.Now()' cmd/kilter/domains.go` returns exactly one line, inside
  `parseNow`.
