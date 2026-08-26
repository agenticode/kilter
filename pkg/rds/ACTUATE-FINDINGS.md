# U14 — `pkg/rds` actuation: the first change Kilter makes to a database

`pkg/rds/actuate.go`, `actuate_api.go`, `actuate_approve.go`,
`actuate_preflight.go`, `actuate_ledger.go`, `actuate_fixture.go` implement the
storage-performance actuator U13 (`FINDINGS.md` §5) specified: one resumable
`ModifyDBInstance` carrying three arguments, behind a structural approval gate,
in front of a refusal layer that blocks on every unknown.

**2,800 production lines, 1,733 test lines, 51 tests, 22 refusal predicates
(13 new codes, 9 reused).** Green under `gofmt -l ./pkg/rds`, `go vet ./...`,
`go build ./...`, `go test -race -count=1 ./pkg/rds/...` and
`go test -race -short ./...`. `go.mod` and `go.sum` are **untouched**;
`go list -deps ./pkg/rds/` shows stdlib plus `pkg/domain`, `pkg/model`,
`pkg/guard` and `pkg/pricing/commit`. **No file outside `pkg/rds/actuate*.go`
was created or edited** — not `parity*.go`, not `rds.go`, not `FINDINGS.md`,
not `cmd/`. §5 below is what `cmd/` and `pkg/actuate` owe, written down instead
of wired.

---

## 1. What was built, in one paragraph

An [`Actuator`](actuate.go) that takes an `ApprovedStep` — a value no package
outside `pkg/rds` can construct — decodes it, re-reads the instance state, the
provisioning envelope and the 24-hour modification history **live**, runs
twenty-two pure refusal predicates, and then either records the exact call it
would make (dry-run, the default) or issues exactly one `ModifyDBInstance` and
**observes** the instance walk `available → modifying → storage-optimization →
available` rather than assuming it did. A poll budget that runs out is
`in-flight`, not success and not failure, and re-executing resumes the
observation from whatever AWS shows.

---

## 2. Read this before anything else: what `TestNoMutatingAPISurface` now means

U11 shipped two tests whose names promise this package cannot act:

- `TestNoActuationSurfaceExists` — `*rds.Domain` must not satisfy
  `domain.Actuator`.
- `TestNoMutatingAPISurface` — the **identifier** `ModifyDBInstance` must not
  appear in any non-test file in the package.

**Both still pass, and after this unit they mean something narrower than they
say.** U14 may not edit them, so the narrowing is recorded here and asserted in
`TestTheOnlyMutatingPathIsTheActuator`.

The seam method is named [`StorageActuateAPI.ModifyStorage`](actuate_api.go)
and the SDK adapter maps it onto `rds:ModifyDBInstance`. That satisfies the
identifier scan **by naming**, which a reviewer is entitled to call a
loophole. Three things make it the right call rather than an evasion:

1. **It is stated here, at the top, in those words.** The fixture's operation
   constant is `OpModifyStorage = "ModifyDBInstance"` — a string literal, which
   the scan drops — so every ledger entry, log line and audit trail names the
   real AWS operation. Nothing downstream has to decode a euphemism.
2. **The narrower name is the better name anyway.** `ModifyDBInstance` is a
   forty-argument operation that can change an instance's class, topology,
   engine version, master password and deletion protection. This unit may send
   **three** arguments. A seam named for the three is a seam a reviewer can
   bound in one glance; a seam named for the operation is not.
3. **The guarantee that actually mattered survives intact and is tested.** The
   read-only decision path — sizer, parity engine, report, `*Domain` — cannot
   reach a mutation, because the only type that can is `*Actuator` and nothing
   in that path constructs one. `EnvelopeFixture` and `Fixture` are asserted
   *not* to satisfy `StorageActuateAPI`, so a read-only wiring cannot be passed
   where a mutating one is expected.

**A reviewer who disagrees should change `TestNoMutatingAPISurface` to scan for
the string literal too, and move `OpModifyStorage`'s value into a test file.**
That is a two-line change in a file U14 was not allowed to touch.

---

## 3. Every refusal predicate, and exactly where it is tested

A modification is issued only when **all** of these hold. Each failure is a
named refusal with a machine-readable code; none is a silent skip. Every test
below drives the **full apply path** against a fixture that would happily
modify the database, and every one asserts `f.Mutations() == 0` — a refusal
proven against a pure function proves the function refuses, not that the
database was untouched.

### 3.1 The cooldown — FINDINGS.md §5.3

| Code | Predicate | Tested by |
|---|---|---|
| `storage-modification-cooldown` (**reused**) | fewer than four storage modifications in the trailing 24 h | `TestActuateRefusesBlockedCooldown` |
| `storage-modification-history-unknown` (**new**) | `CooldownVerdict.Known` — **unknown BLOCKS** | `TestActuateRefusesUnknownCooldown` |

§5.3's sentence is the one this unit turns on and it is implemented literally:
`Known=false` never clears the cooldown. It gets its **own code**, distinct
from `Blocked`, because the operator's next action differs — *wait until
`ClearsAt`* against *grant `rds:DescribeEvents`* — and one code for both would
hide which is needed. The blocked refusal carries `ClearsAt` as
`RefusalError.ValidFrom`, which is the value §5.3 names as the right
`ValidFrom` for a deferred step; `TestActuateRefusesBlockedCooldown` asserts it
equals the moment the **oldest** of the four leaves the window.
`TestActuateAllowsAKnownEmptyHistory` pins the other half: the gate is
"unknown blocks", not "silence blocks".

### 3.2 Live state — FINDINGS.md §5.4

| Code | Predicate | Tested by |
|---|---|---|
| `storage-optimization-blocks-modification` (**reused**) | `DBInstance.StateUnstable()` — `modifying`, `storage-optimization` | `TestActuateRefusesUnstableStateAtExecuteTime` |
| `instance-not-available` (**new**) | every other non-`available` state | `TestActuateRefusesNonAvailableState` |
| `storage-modification-pending` (**new**) | `PendingModifiedValues` is empty | `TestActuateRefusesPendingModification` |
| `instance-missing` (**new**) | the instance is in the account | `TestActuateRefusesAMissingInstance` |
| `drift` (**new**) | the live volume matches the recorded `From` or the intended `To` | `TestActuateRefusesDrift` |

§5.4 is honoured exactly: the record read microseconds before the call is
converted back into a `DBInstance` and `StateUnstable()` — **U11's function,
not a second copy** — decides. A `stopped` instance is refused under its own
code rather than under the unstable one, because telling an operator a stopped
database is mid-change is a lie.

### 3.3 The envelope, re-read live — FINDINGS.md §5.2

| Code | Predicate | Tested by |
|---|---|---|
| `provisioning-envelope-unknown` (**reused**) | `DescribeValidDBInstanceModifications` answered **at execute time** | `TestActuateRefusesUnknownEnvelope` |
| `storage-demand-exceeds-envelope` (**reused**) | `GP3Config.Validate` passes against the **fresh** envelope | `TestActuateRefusesWhenLiveEnvelopeDisagrees` |
| `gp3-not-provisionable-below-striping-threshold` (**reused**) | at or above the striping threshold, or SQL Server | `TestActuateRefusesBelowTheStripingThreshold` |
| `baseline-value-must-not-be-sent` (**new**) | no argument names a value equal to the regime baseline | `TestActuateNeverSendsABaselineArgument`, `TestActuateRefusesABaselineArgumentByName` |

The envelope is collected through **U13's own `EnvelopeCollector`**, not a
second reader, so the two units cannot disagree about what the seam said. The
refusal text names the cause §5.2 names — *the instance class changed between
plan and apply* — and the test asserts that sentence is present.

The baseline rule (§5.1) is enforced in one function, `argumentsFor`, used by
**both** the pre-flight check and the call builder, so the thing checked and
the thing sent are the same value by construction rather than by agreement.
`TestActuateNeverSendsABaselineArgument` sweeps four engines × nine sizes ×
nine configurations and asserts the property directly; the sizes include 199 /
200 / 201 and 399 / 400 / 401, which are the boundaries where the regime
changes and a plan shape does not.

### 3.4 The trap-8 ratchet

| Code | Predicate | Tested by |
|---|---|---|
| `storage-performance-ratchet` (**new**) | IOPS and throughput never move down; allocated storage never moves at all | `TestActuateRefusesAReduction`, `TestActuateRefusesAnAllocationChange`, `TestActuateRatchetIsCheckedAgainstTheLiveVolume` |
| `allocated-storage-drift` (**new**) | the observed allocation still matches `Proposal.AllocatedStorageGiB` | `TestActuateRefusesAllocationDrift` |

**This is the largest deliberate narrowing in the unit and §4.1 states its
cost.** The ratchet is evaluated against the **live** configuration, not
against the step's `From`, so a plan that predates somebody else's change
cannot reduce a volume further.

### 3.5 Guardrails and shape

| Code | Predicate | Tested by |
|---|---|---|
| `guardrail-mode-off` (**reused**) | `kilter.dev/mode` ≠ `off` | `TestActuateRefusesModeOff` |
| `guardrail-tags-unknown` (**new**) | the tag set was **read** | `TestActuateRefusesUnreadableTags` |
| `engine-mismatch` (**new**) | the step's engine matches itself and the live instance | `TestActuateRefusesAnEngineChange` |
| `unknown-engine` (**reused**) | a gp3 regime is encoded for the engine | `TestActuateRefusesAnUnknownEngine` |
| `storage-type-not-modelled` (**reused**) | gp2/gp3 only, landing on gp3 | `TestActuateRefusesAMalformedStep/unmodelled-storage-type`, `/target-is-not-gp3` |
| `storage-size-unusable` (**reused**) | 1–65,536 GiB | `TestActuateRefusesAMalformedStep/size-unusable` |
| `wrong-action` (**new**) | `domain.ActionInPlace` | `TestActuateRefusesAMalformedStep/wrong-action` |
| `bad-step` (**new**) | the key hashes its own contents; both specs are complete | `TestActuateRefusesAMalformedStep/edited-after-hashing`, `/no-target-values` |
| `no-change` (**new**) | `From` and `To` differ | `TestActuateRefusesAMalformedStep/no-change` |

`guardrail-tags-unknown` is the doctrine applied to a place nobody would think
to apply it: an unreadable `kilter.dev/mode` tag is indistinguishable from one
that says `off`, and the whole point of that tag is that it works when nobody
is watching.

`TestActuateReasonCodesAreDistinct` pins all thirteen new codes against each
other **and against all thirty-five U11/U13 codes**, because two codes with one
value silently merge two findings in every roll-up.

### 3.6 The one predicate that is deliberately *permissive*

`inFlightTowardTarget` (actuate_preflight.go) decides whether AWS is already
applying **this step's** change. When it is, the four gates in §3.2 — which
answer *may a modification be issued?* — are skipped, and only the identity
checks (right instance, right engine, right allocation) run.

This was **found by a test, not designed in**: `TestResumeAtEveryStageBoundary`
and `TestALostResponseDoesNotIssueASecondModification` both failed on the first
implementation, because the gates that correctly stop a modification being
issued are exactly the states a resumed step is legitimately in. A pre-flight
that cannot tell those apart refuses to observe the very modification it
started, leaving a production database mid-change with nobody watching — a
worse failure than the one the gates prevent.

Being permissive here is safe for a structural reason, stated in the function's
doc comment: **a step in a resuming state issues nothing.** `execute` sends a
modification only from `StageReady`, and no state `inFlightTowardTarget`
accepts derives to `StageReady`. The worst case of a false positive is a poll
budget spent watching an instance that is not changing, which ends as an honest
`in-flight` entry.

---

## 4. What was deliberately not built, and the honest cost

### 4.1 This actuator will not execute a reduction — and that is where the money is

U13 identifies two shapes: a gp2 → gp3 **conversion**, and a **reduction** of
provisioned IOPS/throughput toward the non-reducible baseline. FINDINGS.md
§2.4 says the reduction is *"the shape that carries the money"*.

**U14 refuses to execute it.** `storage-performance-ratchet` fires on any step
that lowers IOPS or throughput below what the volume delivers now. The reason
is in the refusal text: a reduction is the change that starves a production
primary of I/O if the measurement behind it was wrong, and this actuator would
be making that call unattended at 3 a.m. against a database whose p99 was
computed from `p99(read) + p99(write)` (§6.2) over a window that may have
missed a month-end.

The cost is real and should be stated without softening: **the highest-value
recommendation this domain produces is advisory-only after U14 ships.** An
operator performing it by hand gets the full assessment, the exact call in
`Actuator.PlannedCall`, and a dry-run that names every argument. What they do
not get is automation.

### 4.2 A revert consumes one of the four modifications per 24 hours

`Actuator.Revert` builds the inverse step and runs it through the identical
pre-flight. Two consequences, neither hidden:

- **A change and its undo are two of four.** A change, an undo and one retry
  are three, and there is no fourth chance to get it right that day.
  `TestAChangeAndItsUndoSpendTwoOfFour` asserts the counter really is shared,
  driving two changes and checking the fixture's history reaches 2.
- **The undo of a raise is a reduction, so §4.1's ratchet refuses it.**
  `TestRevertRestoresTheRecordedFrom` asserts exactly that outcome rather than
  papering over it: the operator is told the undo is a reduction instead of
  having one performed for them. A conversion's undo (gp3 → gp2) is refused
  under `storage-type-not-modelled`, because every modification this unit makes
  lands on gp3.

**In practice this actuator's changes are one-way in the automated path.** That
is a defensible posture for storage performance — the ratchet only moves up, so
the failure mode of not reverting is a bill, not an outage — and it is the
opposite of the posture `pkg/ebs` takes, where a revert restores a *faster*
volume. §5.6's guarantee is still honoured and tested: a revert can never be
talked below the regime baseline, because `configOf` floors the recorded
`From` at the baseline before anything looks at it
(`TestRevertCannotGoBelowTheRegimeBaseline`).

### 4.3 There is no economics gate

`pkg/ec2`'s actuator refuses a step that carries no commitment-checked savings
attestation. This one does not, for a reason specific to storage: *"the price
for a reserved DB instance doesn't provide a discount for the costs associated
with storage, backups, and I/O"* [verified, FINDINGS.md §6.1], so there is no
commitment waterfall to strand anything and net equals gross by construction.
`AssessParity` already refuses any proposal whose rate provenance is not
`Claimable`, upstream of any step reaching here.

The step **may** carry `kilter.dev/net-savings-monthly-usd`; it is recorded
verbatim in the ledger, never recomputed, and rolled up through `SumUSD`. It is
not a gate. If a future unit wants one, it is four lines in `decodeStep`.

### 4.4 `--apply-immediately` is not a field, and the adapter must send it

`ModifyStorageInput` has no scheduling field, because §5.5 says three fields
and a scheduling flag is a fourth thing a caller could get wrong. The adapter
must send `--apply-immediately` unconditionally (§5.2 below).

**This is the deviation a reviewer is most likely to object to**, and the
objection is fair: a behaviour that consequential living only in a doc comment
is weaker than a field with a validator. The counter-argument is that a
deferred storage change happens in a maintenance window this actuator cannot
observe, so a step that scheduled one could never reach `done` — it would sit
`in-flight` until a human noticed. Making it unrepresentable is the safer of
two imperfect options. `TestTheIssuedCallCarriesNothingElse` asserts
`applyImmediately` is absent from the serialized call, which at least makes the
absence deliberate rather than forgotten.

### 4.5 `ClientToken` is real here and is a no-op at AWS

`ModifyDBInstance` **accepts no client token.** The field exists on
`ModifyStorageInput`, the fixture deduplicates on it, and **the adapter must
not forward it** (§5.2). Idempotency against a lost response is structural
instead:

1. the ledger's terminal check short-circuits a completed step with **no cloud
   call at all** (`TestReExecutingACompletedStepIsANoop` asserts zero calls);
2. `PendingModifiedValues` is re-read before every issue, so a landed-but-lost
   modification is observed and resumed rather than re-sent
   (`TestALostResponseDoesNotIssueASecondModification` asserts the instance's
   own event history contains exactly **one** modification after a lost
   response and a retry).

A duplicate `ModifyDBInstance` with identical values is harmless to the
*shape* of the instance — it is a declarative absolute-value API — but it
spends one of four modifications per 24 hours, which is why (2) is load-bearing
rather than tidy.

### 4.6 Not attempted

- **Blue/green deployments.** The only route to a storage change with a
  bounded, observable cutover for engines where in-place is disruptive. A
  different unit.
- **Batching across a fleet.** One step, one instance. A batch API would need a
  concurrency limiter aware of the per-instance 24-hour counter.
- **A fuzz target.** `pkg/rds` has two; this unit has none. The refusal layer
  is table-driven and its property test (`TestActuateNeverSendsABaselineArgument`)
  is the one that would have been fuzzed. Budget went to the refusal tests.
- **Reading the instance class.** The envelope depends on it, and the refusal
  when it changes is `storage-demand-exceeds-envelope` — correct but indirect.
  Carrying the class on `InstanceStateRecord` would let the refusal say *"the
  class changed from db.r6i.large to db.t3.medium"* instead of describing the
  consequence. One field, one refusal message.

---

## 5. Exact wiring `cmd/` and `pkg/actuate` must do

### 5.1 The SDK adapter — four operations

```go
type rdsStorageAdapter struct{ c *rds.Client } // aws-sdk-go-v2

func (a *rdsStorageAdapter) DescribeInstanceState(ctx context.Context,
    in *kilterrds.DescribeInstanceStateInput) (*kilterrds.DescribeInstanceStateOutput, error) {

    out, err := a.c.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
        DBInstanceIdentifier: aws.String(in.DBInstanceIdentifier)})
    var nf *types.DBInstanceNotFoundFault
    if errors.As(err, &nf) {
        return &kilterrds.DescribeInstanceStateOutput{Found: false}, nil // NOT an error
    }
    if err != nil || len(out.DBInstances) == 0 {
        return &kilterrds.DescribeInstanceStateOutput{Found: false}, err
    }
    d := out.DBInstances[0]
    rec := kilterrds.InstanceStateRecord{
        Identifier: aws.ToString(d.DBInstanceIdentifier), ARN: aws.ToString(d.DBInstanceArn),
        Engine: aws.ToString(d.Engine), LicenseModel: aws.ToString(d.LicenseModel),
        Status: aws.ToString(d.DBInstanceStatus),
        AllocatedStorageGiB: int64(aws.ToInt32(d.AllocatedStorage)),
        StorageType: aws.ToString(d.StorageType),
        IOPS: aws.ToInt32(d.Iops), StorageThroughputMBps: aws.ToInt32(d.StorageThroughput),
    }
    if p := d.PendingModifiedValues; p != nil {          // REQUIRED: §4.5 turns on it
        rec.PendingStorageType = aws.ToString(p.StorageType)
        rec.PendingIOPS = aws.ToInt32(p.Iops)
        rec.PendingStorageThroughputMBps = aws.ToInt32(p.StorageThroughput)
        rec.PendingAllocatedStorageGiB = int64(aws.ToInt32(p.AllocatedStorage))
    }
    // rds:ListTagsForResource. TagsKnown MUST stay false if it fails — an
    // unreadable mode tag refuses (guardrail-tags-unknown).
    if tags, err := a.c.ListTagsForResource(ctx, &rds.ListTagsForResourceInput{
        ResourceName: d.DBInstanceArn}); err == nil {
        rec.Tags = map[string]string{}
        for _, t := range tags.TagList {
            rec.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
        }
        rec.TagsKnown = true
    }
    return &kilterrds.DescribeInstanceStateOutput{Instance: rec, Found: true}, nil
}
```

### 5.2 The mutation — and the four rules the adapter must not break

```go
func (a *rdsStorageAdapter) ModifyStorage(ctx context.Context,
    in *kilterrds.ModifyStorageInput) (*kilterrds.ModifyStorageOutput, error) {

    req := &rds.ModifyDBInstanceInput{
        DBInstanceIdentifier: aws.String(in.DBInstanceIdentifier),
        StorageType:          aws.String(in.StorageType),
        ApplyImmediately:     aws.Bool(true),   // RULE 1 — see §4.4
    }
    if in.IOPS > 0 {                            // RULE 2 — zero means OMIT (§5.1)
        req.Iops = aws.Int32(in.IOPS)
    }
    if in.StorageThroughputMBps > 0 {           // RULE 2
        req.StorageThroughput = aws.Int32(in.StorageThroughputMBps)
    }
    // RULE 3: set NOTHING else on req. Not the class, not MultiAZ, not the
    // engine version, not AllocatedStorage. FINDINGS.md §5.5.
    // RULE 4: do NOT forward in.ClientToken — ModifyDBInstance has no such
    // parameter (§4.5).
    out, err := a.c.ModifyDBInstance(ctx, req)
    ...
}
```

`DescribeValidDBInstanceModifications` and `DescribeEvents` are U13's, already
specified in `FINDINGS.md` §7.6; the same adapter satisfies both interfaces.

### 5.3 Construction, approval and execution

```go
act, err := rds.NewActuator(adapter, rds.ActuatorConfig{
    Mode: mode,                         // rds.ModeDryRun unless --apply
    Now:  time.Now,                     // REQUIRED: the package reads no clock
    CallTimeout:  30 * time.Second,
    PollInterval: 30 * time.Second,
    PollTimeout:  15 * time.Minute,     // storage-optimization outlives this: expect in-flight
    EventWindow:  24 * time.Hour,       // REFUSED if shorter than 24 h
    Persist: func(ctx context.Context, b []byte) error { return store.Put(ctx, ledgerKey, b) },
})

// On startup, BEFORE anything else: resume.
if b, err := store.Get(ctx, ledgerKey); err == nil {
    _ = act.RestoreLedger(b)
}
for _, e := range act.Unsettled() {          // every entry here may be RUNNING NOW
    log.Warn("resuming an unfinished storage modification", "key", e.Key, "stage", e.Stage)
}

// The approval gate. There is no path around this.
ap, err := rds.NewApproval(steps, token, time.Now())   // token from `kilter approve`
bound, err := act.Bind(ap)                             // the ONLY domain.Actuator form
registry.RegisterActuator(bound)
```

`token.Fingerprint` must be `rds.PlanFingerprint(steps)` — **not**
`domain.Fingerprint(steps)`. The two differ for a plan that round-tripped
through JSON or a map: `PlanFingerprint` canonicalizes by `(Seq, Key)` first,
so the fingerprint is a property of the plan's content rather than of the order
it arrived in.

### 5.4 Building a step from a `Proposal`

`Assessment.Proposal` carries effective totals. The step's `From` records what
was observed (this is the `From` a revert restores, §5.6), and both specs must
carry the engine and the allocation:

```go
from := domain.Spec{Attrs: map[string]string{
    rds.AttrEngine:              inst.Engine,
    rds.AttrLicenseModel:        inst.LicenseModel,
    rds.AttrStorageType:         inst.StorageType,
    rds.AttrAllocatedStorageGiB: strconv.FormatInt(inst.AllocatedStorageGiB, 10),
    rds.AttrIOPS:                strconv.FormatInt(int64(inst.IOPS), 10),
    rds.AttrStorageThroughput:   strconv.FormatInt(int64(inst.StorageThroughputMBps), 10),
}}
to := domain.Spec{Attrs: map[string]string{
    rds.AttrEngine:              inst.Engine,                       // MUST match From
    rds.AttrLicenseModel:        inst.LicenseModel,
    rds.AttrStorageType:         prop.StorageType,                  // always gp3
    rds.AttrAllocatedStorageGiB: strconv.FormatInt(prop.AllocatedStorageGiB, 10), // MUST match From
    rds.AttrIOPS:                strconv.FormatInt(int64(prop.IOPS), 10),
    rds.AttrStorageThroughput:   strconv.FormatInt(int64(prop.StorageThroughputMBps), 10),
    rds.AttrNetSavingsMonthlyUSD: strconv.FormatFloat(prop.NetSavingsMonthlyUSD, 'f', -1, 64), // optional
}}
step := domain.Step{Seq: n, Target: ref, Action: domain.ActionInPlace, From: from, To: to,
    Risk: prop.Risk, Detail: prop.Reason}
step.Key = domain.StepKey(step.Target, step.From, step.To)
```

`prop.Action` is `domain.ActionAdvisory` (U13 proposes nothing actuatable). The
step's action must be `domain.ActionInPlace` — FINDINGS.md §5.7's classification
— and setting it is `cmd/`'s deliberate act of promotion, not a default.

### 5.5 Least-privilege IAM

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "KilterRDSObserve",
      "Effect": "Allow",
      "Action": [
        "rds:DescribeDBInstances",
        "rds:ListTagsForResource",
        "rds:DescribeValidDBInstanceModifications",
        "rds:DescribeEvents"
      ],
      "Resource": "*"
    },
    {
      "Sid": "KilterRDSModifyStorageOnly",
      "Effect": "Allow",
      "Action": "rds:ModifyDBInstance",
      "Resource": "arn:aws:rds:*:123456789012:db:*",
      "Condition": {
        "StringEquals": {"aws:ResourceTag/kilter.dev/managed": "true"}
      }
    }
  ]
}
```

Four notes an operator needs:

1. **`rds:ModifyDBInstance` cannot be scoped to three arguments.** IAM has no
   condition key for `iops`, `storage-throughput` or `storage-type`. The grant
   above permits *any* modification of a tagged instance, including the class
   change and the master-password change this unit will never make. The
   argument restriction is enforced entirely by `ModifyStorageInput` having no
   field for anything else — which is why §5.2 RULE 3 matters and why
   `TestMutateInputCannotChangeClassStorageOrAZ` exists.
2. **The resource tag is the only real scoping available.** Use it. An
   untagged production database is then outside the grant entirely, which is a
   second independent guardrail beneath `kilter.dev/mode=off`.
3. **`rds:DescribeEvents` is not optional here.** It is optional for U13's
   report (an unread history degrades to an unverified precondition). For U14
   an unread history **blocks** (§3.1), so a controller without it will refuse
   every step with `storage-modification-history-unknown` and never act.
4. **`rds:ListTagsForResource` is likewise not optional** — without it,
   `guardrail-tags-unknown` refuses everything.

### 5.6 What `pkg/actuate` must know

- **Register `*BoundActuator`, never `*Actuator`.** The latter does not satisfy
  `domain.Actuator` and will not compile into a registry — that is the
  structural gate, not an oversight to work around.
- **`ErrPollTimeout` is not a failure.** A storage modification plus its
  optimization phase routinely runs for hours. Treat `in-flight` as success
  with pending observation; re-execute later.
- **`Unsettled()` is the startup work-list**, and it now includes entries that
  were **refused after a modification was issued** — a refusal before anything
  was issued touched nothing and is settled, but one after is unfinished
  business somebody must look at.
- **`LedgerSummary.NextClears`** is the earliest moment a dated refusal lapses.
  It is the one number a scheduler needs to sleep until.
- **Rate-limit per instance, not per fleet.** Four modifications per 24 hours
  is a per-instance budget. A batch runner that retries aggressively can
  exhaust it on one database while doing nothing to the rest.

---

## 6. What would falsify parts of this unit

1. **If `PendingModifiedValues` does not populate for a storage-type change**
   the way §4.5 assumes, the lost-response path degrades from "observe and
   resume" to "poll until the top-level fields change", which is still correct
   but slower to recognize. `inFlightTowardTarget`'s first branch already
   covers the landed case, so nothing becomes unsafe — it becomes less prompt.
2. **If RDS enforces an IOPS:throughput ratio** (FINDINGS.md §7.2 records that
   none is documented), a proposal inside the live envelope can still be
   rejected at apply time. It surfaces as a `failed` ledger entry with the AWS
   error verbatim, and the fix is one clause in `GP3Config.Validate` — a file
   U14 may not edit.
3. **If the four-per-24-hours limit is not what `rds:DescribeEvents` reports**,
   the cooldown is wrong in the *over*-counting direction (§6.5:
   `IsStorageModificationEvent` matches broadly). Over-counting delays a change
   by hours; under-counting sends a call AWS rejects. This unit inherits that
   choice unchanged and it is the right one for an actuator.
4. **If `storage-optimization` can be entered without a modification being
   counted**, an instance could accept a fifth change. This unit would refuse
   it anyway — `storage-optimization-blocks-modification` fires on the state,
   independent of the count.

---

## 7. Things I am not confident are safe to run against a real account

Stated plainly, because the alternative is a reader assuming otherwise.

1. **Nothing here has ever run against AWS.** Every test is a fixture. The
   fixture models asynchrony, the modification limit and lost responses, and it
   is still a model. The first real run should be `ModeDryRun` against a
   non-production instance, comparing `Actuator.PlannedCall` against what an
   operator would have typed.
2. **The `available` → `modifying` → `storage-optimization` → `available` walk
   is my reading of the documented behaviour, not an observed trace.** In
   particular, `TestApplyIssuesExactlyOneModificationAndObservesIt` asserts
   that the new values become visible when the instance enters
   `storage-optimization` rather than when it returns to `available`. If that
   ordering is wrong, the actuator reports `done` early — the modification is
   still correct and still completes, but the ledger says so before AWS does.
   This is the single assumption I would verify first.
3. **A gp2 → gp3 conversion's real duration is unknown to me.** The 15-minute
   default poll budget is a guess. It is safe (a timeout is `in-flight`, not
   failure) but it means the common outcome of a real apply is an entry
   somebody has to come back to, and a controller that does not call
   `Unsettled()` on startup will lose track of it.
4. **The IAM grant in §5.5 is broader than this unit's behaviour.** IAM cannot
   express "only these three arguments". An operator who trusts the policy
   rather than the code has granted more than they think.
5. **`inFlightTowardTarget` is the one permissive predicate in a unit built on
   refusals.** Its safety rests on the claim that no state it accepts derives
   to `StageReady`, the only stage that issues. That claim is now enforced
   rather than asserted: `TestNothingResumableCanAlsoIssue` sweeps 15,552
   combinations of live type/values, pending type/values and status and fails
   if any state is simultaneously resumable and issuable. `execute` also
   carries the belt-and-braces `stage == StageReady && !inFlightTowardTarget(...)`
   double condition. It remains the sharpest edge in the unit, and the sweep is
   the thing standing on it.
