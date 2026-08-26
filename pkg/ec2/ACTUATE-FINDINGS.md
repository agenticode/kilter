# U7 — EC2 instance / ASG actuation behind approval

`pkg/ec2`'s actuation half. Spec: `docs/design/compute-domains.md` §3.3, §6 U7,
§7 traps 1 and 4.

**This is the first Kilter unit that stops production instances.** Everything
before it was advisory or online-reversible (U6's `ModifyVolume` degrades a
volume at worst). A bug here is downtime or destroyed data. The whole design is
organised around one property, and the fuzz target is named after it:

> An EC2 instance is never left **stopped and forgotten**.

**Status:** complete for the U7 scope. `gofmt` clean, `go vet ./...` clean,
`go build ./...` clean, `go test -race -short ./...` green, `pkg/ec2` at
**89.6 %** statement coverage. No AWS SDK, no network call, no clock of its
own, no new module dependency, **`go.mod`/`go.sum` untouched**. 6.8k lines
across 14 new files, all `pkg/ec2/actuate*`. Nothing outside that prefix was
edited.

Two fuzz targets were run beyond their seed corpora during development:
`FuzzStopStartNeverStrandsAnInstance` (2.37M execs, 30 s) and
`FuzzPreflightNeverApprovesAForbiddenChange` (3.60M execs, 26 s). Both clean;
no corpus files were committed because nothing failed.

---

## 1. What is here

| File | What it is |
|---|---|
| `actuate_api.go` | The seam. `PreflightAPI` (read-only) / `InstanceActuateAPI` / `ASGPreflightAPI` / `ASGActuateAPI`, over plain Go structs shaped after the AWS request/response types. |
| `actuate_approve.go` | `ApprovalToken`, `Approval`, `ApprovedStep`, `PlanFingerprint`. The human-in-the-loop gate, made structural. |
| `actuate_preflight.go` | Every refusal predicate. Pure: no I/O, no clock, no reachable mutation. |
| `actuate.go` | `Actuator`, `ActuatorConfig`, `Mode`, `Stage`, statuses, the ledger, `Summarize`, `BoundActuator`. |
| `actuate_instance.go` | The standalone stop → modify → start machine, its resume logic and its rollback. |
| `actuate_asg.go` | Launch-template version + instance refresh, and the refresh's own rollback semantics. |
| `actuate_fixture.go` | `ActuateFixture`: a fake EC2 + Auto Scaling account with AWS's own state rules and two fault-injection hooks. |
| `actuate_*_test.go` | 56 tests + 2 fuzz targets. |

### The seams

```go
type PreflightAPI interface {           // read-only: nothing here changes anything
    DescribeInstanceDetail(...)         // ec2:DescribeInstances + ec2:DescribeInstanceAttribute
    DescribeInstanceType(...)           // ec2:DescribeInstanceTypes
    DescribeImage(...)                  // ec2:DescribeImages
}
type InstanceActuateAPI interface {     // PreflightAPI + the three §3.3 mutations
    PreflightAPI
    StopInstances(...); StartInstances(...); ModifyInstanceAttribute(...)
}
type ASGActuateAPI interface {          // ASGPreflightAPI + the three ASG mutations
    ASGPreflightAPI
    CreateLaunchTemplateVersion(...); ModifyLaunchTemplate(...); StartInstanceRefresh(...)
}
```

Read and write are separate interfaces on purpose: a read-only wiring does not
satisfy `InstanceActuateAPI`, so it cannot be passed where an actuator is
expected and compile. `ec2:TerminateInstances` and
`autoscaling:TerminateInstanceInAutoScalingGroup` have no method anywhere in
this package — §3.3 forbids termination, and the way to enforce that is to make
it unrepresentable rather than to check for it.

`ModifyInstanceAttributeInput` has exactly two fields (`InstanceID`,
`InstanceType`). The real API can also change user data, IAM profile,
shutdown behavior and security groups; none of that is representable, so no bug
here can reach it. `StopInstancesInput.Force` and `.Hibernate` exist, are always
false, and the fixture fails the call if either is set.

---

## 2. Approval is structural, not procedural

Every other guardrail in Kilter is a function somebody has to remember to call.
This one is built out of the type system:

1. `*Actuator` has **no method that takes a bare `domain.Step` and acts**. Its
   execution entry points are `Execute(ctx, ApprovedStep)` and
   `Revert(ctx, ApprovedStep)`.
2. `ApprovedStep` has **only unexported fields**. `ec2.ApprovedStep{step: s}`
   does not compile outside this package. The only constructor is
   `Approval.Authorize`.
3. Go always permits a zero value, so the zero `ApprovedStep` *is*
   constructible — and is inert. `Execute` refuses it with `ErrNotApproved`
   before any cloud call.
4. `Approval` can only be built by `NewApproval(steps, token, now)`, which
   **recomputes the plan fingerprint from the steps in hand** and refuses a
   token that approves anything else, a token with no approver or expiry, a
   step whose key does not hash its own contents, a step outside the token's
   account scope, and duplicate keys.
5. **`*Actuator` deliberately does not satisfy `domain.Actuator`.** It cannot be
   handed to `domain.Registry.RegisterActuator` at all. Only `*BoundActuator` —
   an actuator with an approval already attached, via `a.Bind(approval)` — does.
   So the registry's `Execute(ctx, Step)` path physically cannot exist without a
   token having been presented first.
6. The approval's expiry is **re-read at every step**, not once at the top of a
   plan: a stop, a modify and a start take minutes, and a token that lapsed
   halfway through does not authorize the rest.

Proven at runtime by `TestZeroApprovedStepCannotAct` (zero value and a
hand-forged in-package value, both refused, account untouched),
`TestActuatorIsNotRegistrableWithoutApproval` (asserts `any(a).(domain.Actuator)`
fails — this test breaks the moment somebody adds a convenience
`Execute(ctx, Step)` to `*Actuator`), `TestBoundActuatorRefusesForeignSteps`,
`TestAuthorizeRefusesStepsOutsideThePlan`,
`TestNewApprovalRefusesEverythingItCannotVerify` and
`TestExpiredApprovalStopsExecutionMidPlan`.

`Preflight(ctx, step)` needs **no** token, because it cannot act. Separating
"may I look?" from "may I act?" is what lets the gate stay absolute without
making the tool opaque.

### Undo and the gate

`Revert(ctx, approvedStep)` takes the **original** approved step and inverts it
internally. It does not require a second signature, and the inverse step is
exempt from the coverage and expiry checks (`ApprovedStep.check`). Reasons: the
inverse key was never in the approved plan and cannot be; and an approval that
lapsed while the plan ran is exactly when an undo is most needed.
`domain.Registry.Revert` takes the same position about freeze/breaker/window.
An undo still runs every **safety** predicate — `TestRevertStillRefusesUnsafeInstances`
shows an undo refused because somebody attached instance-store storage in the
meantime.

---

## 3. The refusal matrix, and exactly where each predicate is tested

Every refusal is a `*RefusalError` carrying a stable `Code`, matched with
`errors.Is(err, ErrRefused)` and read with `RefusalCode(err)`. Codes the
read-only sizer already defines are **reused, not redefined**, so an operator
filtering on `memory-blind` sees both halves of the domain under one code.

Every test below asserts the **specific code** *and* — via `wantRefusal` —
that the account received **zero mutating calls**.

### The six §6 U7 / §7 predicates

| Code | Fires when | Test |
|---|---|---|
| `instance-store` | Any of: `InstanceStoreVolumes > 0`; a block-device mapping with a `virtualName` (`ephemeral*`); `rootDeviceType == instance-store`; or `DescribeInstanceTypes` says the **current** type carries instance storage. Stopping destroys that data permanently (§3.3 never). | `TestRefusesInstanceStoreVolumes` (3 sub-cases), `TestRefusesInstanceStoreDeclaredByTheCatalog` |
| `ena-unsupported` | Target's `ENASupport == "required"` and either the instance's `enaSupport` attribute or the AMI's is false — **or** `DescribeInstanceTypes` reported no ENA level at all. | `TestRefusesMissingENAPrerequisite` (3 sub-cases) |
| `nvme-unsupported` | Target's `NVMeSupport == "required"` and the **current** type's is not `required` — or no NVMe level was reported. See §3.1 for why this is the honest test. | `TestRefusesMissingNVMePrerequisite` (3 sub-cases, incl. the passing Nitro→Nitro case) |
| `shutdown-behavior` | `instanceInitiatedShutdownBehavior` is `terminate`, empty, or unrecognized. Absence of evidence refuses (§3.3 "on any doubt the step is advisory"). | `TestRefusesUnsafeShutdownBehavior` (3 sub-cases) |
| `storage-shrink` | `To`'s declared storage < `From`'s, or either side does not declare it. §3.3: never reduce storage. | `TestRefusesStorageReduction`, `TestRefusesUndeclaredStorage` (4 sub-cases), `TestASGRefusesAShrunkenTemplateVersion` |
| `commitment-negative` | The step's attested net bill delta through the commitment waterfall is ≤ 0 (§7 trap 1, §4.4 ex.1–3). | `TestRefusesCommitmentNegativeRecommendation` (2 sub-cases; also asserts the account was never even **read**) |
| `commitment-unchecked` | No commitment-check timestamp, a check older than `CommitmentMaxAge` (30 d), a future-dated check, an unparseable/non-finite net or gross, or net > gross. | `TestRefusesUncheckedCommitments` (9 sub-cases) |
| `memory-blind` | `To.MemoryBytes < From.MemoryBytes` and the memory-signal attestation is not `cwagent` (§7 trap 4). | `TestRefusesMemoryReductionWithoutAMemorySignal` (5 sub-cases, incl. both passing cases) |

### Ownership (§3.3 "Never")

| Code | Fires when | Test |
|---|---|---|
| `k8s-tagged` | `kubernetes.io/cluster/*`, `eks:cluster-name` or `aws:eks:cluster-name` on the instance or the ASG. Belongs to `k8s-nodes`. | `TestRefusesInstancesThisDomainDoesNotOwn`, `TestASGRefusalMatrix` |
| `guardrail-mode-off` | `kilter.dev/mode=off` on the instance or the ASG. | same |
| `asg-managed` | The instance carries `aws:autoscaling:groupName`. §3.3: never touch ASG instances directly. | `TestRefusesInstancesThisDomainDoesNotOwn` |

Ownership is evaluated against **live tags**, not against what the plan
recorded: a plan built before an operator tagged an instance `mode=off` must not
run after.

### Boot prerequisites and shapes this unit will not resize

| Code | Test |
|---|---|
| `arch-mismatch`, `virtualization-unsupported`, `root-device-unsupported`, `image-missing`, `unknown-instance-type`, `bare-metal`, `spot-instance`, `hibernation-configured`, `tenancy` | `TestRefusesInstancesThisUnitDoesNotResize` (10 sub-cases) |
| `wrong-action`, `bad-step`, `no-change` | `TestRefusesMalformedSteps` (8 sub-cases) |
| `instance-missing`, `instance-state`, `drift` | `TestRefusesInstancesThatAreNotThere`, `TestRefusesDriftedInstance` |

### Auto Scaling — the silent-no-op family

The dangerous ASG failure is not a crash, it is a **silent no-op**: several
legal group shapes make "edit the launch template's instanceType" change
nothing while the refresh churns the whole fleet. Each is refused
(`TestASGRefusalMatrix`, 16 sub-cases; plus `TestASGMissingGroupIsRefused`):

`asg-mixed-instances-policy` (overrides, not the template, pick the types) ·
`asg-instance-requirements` (attribute-based selection ignores `instanceType`) ·
`asg-version-pinned` (a numeric version reference means the new version is never
launched, and repointing the group is `autoscaling:UpdateAutoScalingGroup`,
which this unit does not hold) · `asg-launch-configuration` ·
`asg-no-launch-template` · `asg-suspended-processes` (Launch or Terminate
suspended ⇒ the refresh stalls forever) · `asg-empty` ·
`asg-refresh-in-progress` (a refresh toward another configuration) ·
`asg-missing`.

### Dry-run and apply refuse identically

`TestDryRunAndApplyRefuseIdentically` runs six predicates in both modes and
fails if the codes differ. Both modes share one code path; the only difference
is that apply issues the calls. An apply that can do something dry-run never
showed is the bug class `pkg/actuate` already hit once.

---

## 4. The stop-start machine

```
ec2:StopInstances → ec2:ModifyInstanceAttribute(instanceType) → ec2:StartInstances
```

Three transitions, two crash windows. Four rules:

1. **The stage is observed, never remembered.** `stageOf(live, from, to)`
   derives `ready | stopping | stopped | modified | starting | running | gone |
   unknown` from the live instance's state **and** its type compared against
   *both* the step's From and To. A ledger that says "stopping" and an account
   that says "running as the new type" disagree; the account wins.
2. **Gates run once, at the top, while the instance is still up.** Past the
   stop the machine may only move forward (target, running) or back (original,
   running). It is never allowed to conclude "this is no longer worth doing" and
   stop — that conclusion with a stopped instance underneath it *is* the outage.
3. **A failed modify rolls forward into a start.** If `ModifyInstanceAttribute`
   fails on a stopped instance, the machine starts it again at its **original**
   type and records `rolled-back`. If the start also fails, the entry stays
   non-terminal so the next run resumes. If the instance comes back as the
   *target* type, the "failure" was a lost response and the step is recorded
   `done` — reporting a rollback there would be a lie.
4. **Persist before mutate.** `ActuatorConfig.Persist` is called with the
   serialized ledger **before** every mutating call. A `Persist` that returns an
   error **aborts the mutation**: an unrecorded stop is precisely the state this
   unit must never reach. (`TestPersistFailureAbortsBeforeTheStop`,
   `TestPersistRecordsIntentBeforeEachMutation`.)

`maxStageVisits = 3` bounds the loop: an instance somebody else keeps restarting
ends as a reported `ErrStuck` failure rather than an infinite loop against a
billed API (`TestMachineGivesUpRatherThanLooping`).

### Resumability, at every stage boundary

`TestResumesFromEveryStageBoundary` and `TestResumesThroughAPersistedLedger`
each run **nine** boundaries. After the interruption both assert the safety
invariant, then resume — the first with a **fresh actuator holding no ledger at
all** (the harshest restart there is), the second through a persisted-and-
restored ledger.

| Boundary | Injection |
|---|---|
| before the stop is issued | fail `StopInstances` |
| the stop's response is lost | apply, then fail `StopInstances` |
| while it is stopping | stall the transition; poll budget = 1 |
| before the modify is issued | fail `ModifyInstanceAttribute` → rollback path |
| the modify's response is lost | apply, then fail `ModifyInstanceAttribute` |
| before the start is issued | fail `StartInstances` |
| the start's response is lost | apply, then fail `StartInstances` |
| while it is starting | stall the transition; poll budget = 1 |
| the driving describe fails | fail `DescribeInstances` |

Statuses: `done` / `no-op` / `rolled-back` are **terminal** (never redone);
`in-flight` / `failed` / `dry-run` are **not**. `rolled-back` is terminal on
purpose — the machine already restored the instance, and retrying automatically
would stop it again for a change that just failed. A human decides.
`Actuator.Unsettled()` is what a controller reads on startup: every key it
returns may be an instance that is down right now.

### The fuzz target

`FuzzStopStartNeverStrandsAnInstance` drives fault injection from a fuzzed byte
script (fail-before-effect and fail-after-effect, on every describe and every
mutation), runs eight executions with a fresh actuator each time, and after each
one asserts:

> the instance is running, **or** the actuator holds a non-terminal ledger entry
> for the step.

A terminal entry with a stopped instance fails the test — that is "stopped and
forgotten". A second phase clears the faults and requires the machine to
actually converge to a running instance, so an actuator that satisfied safety by
failing forever would not pass.

---

## 5. The ASG path

```
ec2:CreateLaunchTemplateVersion → [ec2:ModifyLaunchTemplate] → autoscaling:StartInstanceRefresh
```

- **A group's instances are never touched.** No stop, no start, no modify, no
  terminate-in-ASG. `assertNoInstanceMutations` asserts it in every ASG test.
- `StartInstanceRefresh` always requests `AutoRollback: true`, a min-healthy
  percentage (default 90) and an instance warmup, and pins
  `DesiredConfigurationVersion` to the version this step created — an unpinned
  refresh follows whatever `$Latest` becomes while it runs. There is no
  `SkipMatching`, no `MaxHealthyPercentage`, no checkpoints: a refresh this unit
  starts is always the slow, safe kind.
- **The refresh's own rollback is respected.** A terminal refresh has three
  outcomes and all three are recorded honestly:
  `Successful → done`, `RollbackSuccessful|RollbackFailed → rolled-back` (**not**
  a success; the group is back on the old configuration), `Failed|Cancelled →
  failed`. A rolled-back or failed refresh is **not retried automatically** —
  churning a whole fleet a second time is a human's decision, and a restarted
  controller with no ledger re-reports it instead of re-refreshing.
  (`TestASGRefreshRollbackIsNotSuccess` ×2, `TestASGRefreshFailureIsReportedNotRetried`.)
- **A pointed template is not a migrated fleet.** `asgStage` distinguishes
  "template launches the target type" from "the instances are the target type";
  only a completed refresh means the latter. Conflating them would report a
  resize as done the moment the template was edited.
- **Undo** (`TestASGRevertPointsTheTemplateBack`): §3.3's "point template back".
  The ledger records `PriorVersion` and `PriorDefaultVersion` the moment they are
  known; the undo issues a single `ModifyLaunchTemplate` back to the recorded
  default and refreshes again — no version chain. A `$Latest` group has no
  default to repoint, so its undo creates a version carrying the original type
  (`TestASGRevertOnALatestGroupCreatesAVersion`).
- **Resumability:** `TestASGResumesFromEveryStageBoundary` covers seven
  boundaries and asserts that resuming produces **no second launch template
  version** (client-token idempotency plus "reuse the oldest version newer than
  source that already carries the target type") and **no second refresh**.

---

## 6. AWS API calls and the IAM policy the operator must grant

### Calls this unit makes

**Read (pre-flight):** `ec2:DescribeInstances`, **`ec2:DescribeInstanceAttribute`**
(`instanceInitiatedShutdownBehavior`), `ec2:DescribeInstanceTypes`,
`ec2:DescribeImages`, `autoscaling:DescribeAutoScalingGroups`,
`ec2:DescribeLaunchTemplateVersions`, **`autoscaling:DescribeInstanceRefreshes`**.

**Write:** `ec2:StopInstances`, `ec2:ModifyInstanceAttribute`,
`ec2:StartInstances`, `ec2:CreateLaunchTemplateVersion`,
`ec2:ModifyLaunchTemplate`, `autoscaling:StartInstanceRefresh`.

### Two actions are missing from the §3.3 policy block (line ~341)

The design's `KilterEC2Observe` statement does **not** include
`ec2:DescribeInstanceAttribute` or `autoscaling:DescribeInstanceRefreshes`.
Both are load-bearing:

- Without `ec2:DescribeInstanceAttribute` the shutdown behavior reads as empty
  and **every** stop-resize is refused with `shutdown-behavior`. That is the
  correct failure (absence of evidence refuses) but it makes the whole unit a
  no-op, so the operator needs to be told.
- Without `autoscaling:DescribeInstanceRefreshes` no refresh can be polled or
  resumed, and `asg-refresh-in-progress` can never be detected.

### The policy to grant

```json
[
  {
    "Sid": "KilterEC2ActuateObserve",
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeInstances",
      "ec2:DescribeInstanceAttribute",
      "ec2:DescribeInstanceTypes",
      "ec2:DescribeImages",
      "ec2:DescribeLaunchTemplateVersions",
      "autoscaling:DescribeAutoScalingGroups",
      "autoscaling:DescribeInstanceRefreshes"
    ],
    "Resource": "*"
  },
  {
    "Sid": "KilterEC2ActuateInstances",
    "Effect": "Allow",
    "Action": [
      "ec2:StopInstances",
      "ec2:StartInstances",
      "ec2:ModifyInstanceAttribute"
    ],
    "Resource": "arn:aws:ec2:*:<account>:instance/*",
    "Condition": {
      "StringNotEquals": { "aws:ResourceTag/kilter.dev/mode": "off" }
    }
  },
  {
    "Sid": "KilterEC2ActuateASG",
    "Effect": "Allow",
    "Action": [
      "ec2:CreateLaunchTemplateVersion",
      "ec2:ModifyLaunchTemplate",
      "autoscaling:StartInstanceRefresh"
    ],
    "Resource": "*",
    "Condition": {
      "StringNotEquals": { "aws:ResourceTag/kilter.dev/mode": "off" }
    }
  }
]
```

Notes on this being **narrower** than §3.3's block:

- `ec2:ModifyVolume` is not here; it is U6's, and it belongs to a different
  (online, no-approval) risk class.
- The instance statement is scoped to `instance/*` ARNs, not `"*"`.
  `ec2:ModifyInstanceAttribute` on `"*"` also grants attribute writes on
  security groups and network interfaces, which this unit never performs.
- **`ec2:TerminateInstances` must never be granted.** The package has no method
  that could call it; the policy should say the same thing.
- Split observe from actuate into two roles: the brain/collector gets the
  observe statement, the controller gets all three. §3.3 already says this.
- The tag condition mirrors §3.3. It is defence in depth, not the guardrail —
  `guardrail-mode-off` is enforced in code against live tags, before any call.

### Idempotency tokens and timeouts

`CreateLaunchTemplateVersion`, `ModifyLaunchTemplate` and `StartInstanceRefresh`
carry a `ClientToken` derived deterministically from the step key
(`kilter-<op>-<stepKey>`), so a retry after a lost response cannot create a
second version. `StopInstances` / `StartInstances` / `ModifyInstanceAttribute`
have no client-token parameter in the real API and are naturally idempotent;
this unit's own per-key ledger plus live re-observation covers them. Every call
is wrapped in `context.WithTimeout(ctx, cfg.CallTimeout)` (default 30 s), so no
single hung API call can hold a stopped instance hostage.

---

## 7. Deliberate deviations from `pkg/ebs`'s actuator shape

U6 is the idiom this unit follows: dry-run/apply symmetry over one code path,
per-`Step.Key` idempotency, `From` recorded for `Revert`, in-memory ledger with
`LedgerJSON`/`RestoreLedger`, `Now` injected, `Sleep` injected, poll-timeout
reported as in-flight rather than success or failure. Four things differ.

1. **`ApprovedStep` instead of `domain.Step` on the acting methods, and
   `*Actuator` not satisfying `domain.Actuator`.** U6's actuator implements
   `domain.Actuator` directly, which is right for an online, reversible-upward
   `ModifyVolume` that §3.3 explicitly allows under `mode=apply` without
   per-plan approval. §3.3 requires the opposite for instance stop/resize and
   ASG refresh: "no auto-apply default, ever, in v1". A type that accepts a bare
   `Step` cannot express that. `BoundActuator` restores registry compatibility
   at the point where an approval exists.

2. **The savings and memory-signal attestations ride in `Spec.Attrs` under a
   `kilter.dev/` prefix.** `domain.StepKey` hashes every attribute, so the
   attestation is part of the idempotency key and therefore part of the plan
   fingerprint a human approved. Editing a savings claim between approval and
   execution changes the key and the approval stops covering the step
   (`TestRefusesMalformedSteps/a step edited after it was built`). An
   attestation carried outside the spec would be a number anyone could rewrite.
   The `kilter.dev/` prefix keeps it visibly distinct from the resource axes
   (`instanceType`, `arch`, `tenancy`) beside it.
   - `kilter.dev/net-savings-monthly-usd`, `kilter.dev/gross-savings-monthly-usd`
   - `kilter.dev/commitment-checked-at` (RFC3339; ≤ 30 days old)
   - `kilter.dev/memory-signal` (`cwagent` | `none`)
   - `kilter.dev/storage-gib`

3. **A multi-stage machine instead of issue-then-poll.** `ModifyVolume` is one
   call plus polling. This is three calls with two crash windows, so the stage
   is re-derived from the account and the rollback branch exists.

4. **`Persist`-before-mutate.** U6 does not need it: an interrupted
   `ModifyVolume` leaves a working volume. An interrupted stop leaves an outage.

Unchanged on purpose: **the ledger carries no money.** The claimed saving
belongs to the recommendation that produced the step; a second copy here would
become a second source of truth for the bill, and the second one would be list
price. Join by `Key`. The ledger *does* carry downtime, because downtime is a
cost only this unit can measure.

### Determinism

No `time.Now`, no map iteration on any output path, no package-level mutable
state. `Summarize` sorts by key before summing and is pinned by
`TestSummaryIsShuffleInvariant` (50 shuffles, byte-compared JSON).
`PlanFingerprint` canonicalizes by `(Seq, Key)` before hashing — `domain.Fingerprint`
hashes the slice as given, which is correct for a plan the core just built and
wrong for one that round-tripped through JSON or a merge — pinned by
`TestPlanFingerprintIsShuffleInvariant` (40 shuffles).

Honest note on the PR#27 float hazard: the only quantity summed here is
`time.Duration`, an `int64`, whose addition **is** associative. The sort is
therefore not what makes *this* total correct — it makes the rendered order
stable and it means a future float field added to `LedgerSummary` lands in a
function that already sorts. There is no float aggregate in this unit to get
wrong, by the deliberate choice above to keep money out of the ledger.

---

## 8. Deliberately deferred, with the honest reason

- **`autoscaling:UpdateAutoScalingGroup`.** Would let the unit repoint a
  version-pinned group at a new template version. Not held: it is also the API
  that changes desired capacity, min and max, and granting it to reach one field
  hands a resizer the ability to scale a fleet to zero. Version-pinned groups are
  refused with `asg-version-pinned` instead.
- **NVMe driver detection beyond the running-type inference.** No AWS API
  reports whether an AMI carries NVMe drivers. The only sound offline evidence is
  "this AMI is running right now on a type whose NVMe support is `required`".
  The cost is refusing some safe migrations (a Xen-era AMI that does have the
  driver); the alternative is a stopped instance that never boots. An explicit
  operator attestation attribute would lift this and is not in scope for U7.
- **Root-volume / block-device changes on the ASG path.** A new template version
  copies the source's mappings verbatim and this unit never edits them. The
  shrink check exists only to refuse a *pre-existing* target version that
  shrinks one.
- **Mixed-instances policies and Spot fleets.** §3.3 says advisory in v1.
  Refused here rather than partially handled.
- **Batch-managed ASGs** are not detected as such. Per U15's finding, no
  documented EC2 or ASG tag identifies an instance *as Batch*, and this unit does
  not pattern-match undocumented group names. The `asg-managed` instance-level
  refusal covers the instance side; a Batch-created *group* passed to the ASG
  path would be treated as an ordinary group. **Operators running AWS Batch
  should not point U7 at Batch compute-environment groups.**
- **A change-window gate inside the actuator.** §3.3 says stop-resize "only ever
  runs inside a change window *and* behind explicit approval". The approval half
  is here. The window half is `domain.Guard` / `pkg/guard`, already
  domain-agnostic, and belongs in the wiring — see §9.4. **This unit does not
  enforce a change window on its own.**
- **Concurrency across steps.** The actuator is safe for concurrent use, but
  nothing here limits how many instances may be down at once. A disruption
  budget belongs with the planner, not the executor.

---

## 9. Exactly what `cmd/` and `pkg/actuate` must do

Nothing outside `pkg/ec2/actuate*` was touched. The following is the whole
wire-up.

### 9.1 An SDK adapter (new file under `cmd/kilter/`)

One struct implementing `ec2.ASGActuateAPI` over `aws-sdk-go-v2`. It is a
mechanical field copy; the field names track the API's. Two notes:

- `DescribeInstanceDetail` is **two** SDK calls: `DescribeInstances` for
  everything, then `DescribeInstanceAttribute(Attribute:
  "instanceInitiatedShutdownBehavior")` for `ShutdownBehavior`. If the second
  call is denied or errors, leave `ShutdownBehavior` **empty** — do not default
  it to `"stop"`. Empty refuses, which is the designed behaviour.
- `InstanceDetail.InstanceStoreVolumes` must count ephemeral block-device
  mappings **and** any instance-store volumes the type carries. Over-counting is
  safe here; under-counting destroys data.

### 9.2 `pkg/domain/ec2` — replace the `PlanSteps` stub

`pkg/domain/ec2/ec2.go:492` currently refuses unconditionally with an accurate
comment ("pkg/ec2 has no actuation surface … because U7 was never built") that
is now out of date. `Instances.PlanSteps` should emit, for each non-suppressed
`Assessment` with a `Proposal`:

```go
ref  := domain.TargetRef{Domain: domain.EC2, Scope: a.Target.Scope, ID: a.Target.ID, Name: a.Target.Name}
from := specOf(a.Current)                      // carries instanceType, arch, platform, tenancy
to   := specOf(p.Spec).
    WithAttr(ec2.AttrStorageGiB,             storageGiBOf(a)).            // and on `from` too
    WithAttr(ec2.AttrMemorySignal,           memorySignal(a.Observation)). // cwagent | none
    WithAttr(ec2.AttrNetSavingsMonthlyUSD,   money(p.NetSavingsMonthlyUSD)).
    WithAttr(ec2.AttrGrossSavingsMonthlyUSD, money(p.GrossSavingsMonthlyUSD)).
    WithAttr(ec2.AttrCommitmentCheckedAt,    inv.FetchedAt.UTC().Format(time.RFC3339))
step := domain.Step{Seq: n, Target: ref, Action: domain.ActionStopStart, From: from, To: to, Risk: p.Risk}
step.Key = domain.StepKey(ref, from, to)
```

`memorySignal` is `ec2.MemorySignalCWAgent` **iff**
`!a.Observation.MemoryBlind`, and `ec2.MemorySignalNone` otherwise — do not
re-derive the rule, read the flag the sizer already set. `money` must render
with a fixed precision (`strconv.FormatFloat(v, 'f', 4, 64)`) so the step key is
stable across runs. Both `From` and `To` need `AttrStorageGiB`; a step that
cannot state its storage is refused.

`Instances.Health` should stop reporting
`"instance actuation is not implemented (design §6 U7)"`, and should report
report-only when no approval is currently held.

**Also update** `pkg/domain/ec2/ec2_test.go`'s
`TestInstanceHalfIsStructurallyReportOnly` and its assertion that `*Instances`
does not satisfy `domain.Actuator` — the latter stays **true** and should stay
(the actuator is `*ec2.BoundActuator`, a different type), but the surrounding
"there is no actuation surface" prose is now wrong.

### 9.3 `cmd/kilter` — the approval flow

`kilter approve` already exists (`cmd/kilter/trust.go`, `pkg/api/ledger.go`) and
produces `api.Approval{Fingerprint, ApprovedAt, ExpiresAt}` with a 24-hour TTL.
Map it straight across:

```go
tok := ec2.ApprovalToken{
    Fingerprint: ap.Fingerprint,           // must be ec2.PlanFingerprint(plan.Steps)
    Scope:       accountID + "/" + region,
    ApprovedBy:  whoApproved,              // NEW: api.Approval has no approver field
    ApprovedAt:  ap.ApprovedAt,
    ExpiresAt:   ap.ExpiresAt,
}
approval, err := ec2.NewApproval(plan.Steps, tok, time.Now())
bound, err   := actuator.Bind(approval)
registry.RegisterActuator(bound)
```

Two required changes to the existing approval machinery:

1. **`api.Approval` must carry an approver.** `ApprovalToken.ApprovedBy` is
   mandatory (`NewApproval` refuses without it) so the ledger can answer "who
   said yes to this stop". Add the field to `pkg/api`'s `Approval` and to
   `Brain.approve`.
2. **The fingerprint must be `ec2.PlanFingerprint`, not `domain.Fingerprint`.**
   `domain.BuildPlan` sets `Plan.Fingerprint = domain.Fingerprint(steps)`, which
   hashes the slice in the order given. That equals `PlanFingerprint` whenever
   steps are already in `Seq` order — which `BuildPlan` produces — so today they
   agree. `NewApproval` re-derives with `PlanFingerprint` deliberately, so a plan
   that round-tripped through the store or an API still matches. If the two ever
   disagree, `NewApproval` fails closed with `ErrFingerprintMismatch`.

### 9.4 `cmd/kilter/controller.go` — the run loop

```go
a, err := ec2.NewActuator(sdkAdapter, sdkAdapter, ec2.ActuatorConfig{
    Mode:                 ec2.Mode(*mode),        // dry-run unless --mode=apply
    Now:                  time.Now,
    Persist:              func(ctx context.Context, b []byte) error { return store.Put(ctx, "ec2-actuate-ledger", b) },
    MinHealthyPercentage: 90,
    InstanceWarmup:       5 * time.Minute,
    Logger:               log,
})
// On startup, BEFORE anything else:
if b, err := store.Get(ctx, "ec2-actuate-ledger"); err == nil { a.RestoreLedger(b) }
for _, e := range a.Unsettled() {
    log.Warn("resuming an interrupted EC2 resize", "instance", e.Target.ID, "stage", e.Stage)
    // re-authorize the step from the persisted plan and Execute it
}
```

Non-negotiable ordering: **restore the ledger and drain `Unsettled()` before
planning anything new.** Each entry it returns may be an instance that is down
right now.

The controller must also apply the §3.3 change-window rule that this unit does
not: gate `ActionStopStart` and `ActionRolling` steps on
`guard.InWindow(rc.windows, time.Now())`. `domain.Registry.Execute` already
calls `Guard.Allow()`, so routing through the registry gets it for free —
**but** `Registry.Revert` deliberately does not, which is correct.

### 9.5 `pkg/actuate` — nothing

`pkg/actuate` is the Kubernetes executor. It needs no change; the EC2 path goes
through `domain.Registry`. The only cross-package concern is that
`cmd/kilter/domains_test.go:355` currently asserts *no* domain claims an
actuator in this build. That assertion must be updated when the wire-up lands —
its own comment already anticipates it.

### 9.6 A doc comment that is now wrong and could not be fixed here

`pkg/ec2/ec2.go:6-14` states: *"This package contains no actuation and no
mutating call of any kind, not behind a flag, not behind an interface … Instance
resize, stop/start and ASG refresh are a later unit (§6 U7); nothing here can
reach them."* U7 is that later unit and `ec2.go` was out of scope for this job.
**The wire-up must rewrite that paragraph**, pointing at the split between the
read-only seams (`InventoryAPI`, `MetricsAPI`) and the actuation seams
(`InstanceActuateAPI`, `ASGActuateAPI`), and at the approval gate. Leaving it is
worse than a stale comment: it is a false safety claim in the first thing a
reader sees.

---

## 10. What I am NOT confident is safe to run against a real account

Stated plainly, because this is the unit where that matters.

1. **Nothing here has ever run against AWS.** Every seam is a Go interface over
   structs I wrote from the API documentation. The fixture enforces the rules I
   believe AWS enforces. Field-name drift, an error code I did not model, or an
   API that returns a state I did not enumerate all land as
   `unknown`/`drift`/`instance-missing` — which refuse — but I cannot claim the
   adapter will map cleanly until somebody writes and exercises it. **First
   contact should be `--mode=dry-run` against a real account, comparing
   `Preflight` refusals with what an operator expects.**
2. **`ec2:DescribeInstanceAttribute` semantics are modelled, not verified.** If
   the adapter returns `"stop"` on error instead of empty, the single most
   important data-loss gate silently opens. This is the highest-risk line in the
   whole wire-up.
3. **Instance-store detection depends on the adapter.** `InstanceStoreVolumes`
   and the ephemeral block-device mapping are both checked, plus the catalog's
   `InstanceStorageSupported`, but if the adapter populates none of them for an
   instance that has ephemeral disks, the gate does not fire and stopping
   destroys data. I consider this and (2) the two places where an adapter bug is
   unrecoverable.
4. **`MinHealthyPercentage` default 90 and warmup 5 min are AWS's documented
   defaults, not measured for any real fleet.** A group of 2 with min-healthy 90
   may make no progress. An operator should set these per group; this unit has
   no per-group configuration.
5. **The `asg-refresh-in-progress` / "is this refresh ours?" test is heuristic
   when the ledger is lost.** With a recorded `RefreshID` it is exact. Without
   one, it infers ownership from the template already carrying the target type.
   A concurrent refresh started by another tool in that exact window could be
   adopted as ours and polled to completion. It is never *started* twice — AWS
   rejects that — so the failure mode is a mis-attributed status, not a double
   churn.
6. **A stop-resize is downtime, and this unit never asks whether the workload
   can take it.** Nothing here knows about connection draining, load-balancer
   deregistration, or an in-flight job. §3.3 puts that on the human and the
   change window; the change window is not enforced here (§8).
7. **Batch-managed ASGs are not detected** (§8). Pointing U7 at one is a manual
   modification of a fleet AWS documents itself as fully controlling.
8. **The 30-day `CommitmentMaxAge` is a judgement call, not a documented
   bound.** It is what keeps an old inventory from authorizing a resize whose
   net has since inverted; a shorter window would be safer and noisier.
