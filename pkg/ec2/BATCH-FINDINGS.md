# U15a — the Batch containment selector: evidence ledger

**Unit:** U15a, `docs/design/rds-batch-assessment.md` §3.5, §5 U15a, §7 trap 14.
**Scope of this file:** the *selector decision only* — what was chosen, on what
evidence, what was refuted, and what a reader with a real AWS account must check
before trusting it. The in-package narrative (the wrong answer that was
shipping, the reason-code table, the U15b advisories, the deferrals) lives in
`FINDINGS.md` §7 and is not repeated here.
**Why a separate file:** §5 U15a made the selector a *verification* obligation,
not an implementation one — it marked the exact EC2 tag key AWS Batch applies
**[unverified]** and required the selector be verified before code depended on
it. That obligation deserves its own auditable record, because the failure mode
is silent in both directions: a wrong selector either suppresses nothing (and
the unit's own fixture hides it, since the fixture carries whatever key the
author invented) or suppresses instances a user wanted sized.

---

## 0. The answer, first

| | |
|---|---|
| **Selector** | the presence of the EC2 tag key `aws:autoscaling:groupName` (`TagASGName`, `pkg/ec2/ec2.go`) |
| **Reason code** | `asg-managed` (`ReasonASGManaged`) — deliberately **not** `batch-managed` |
| **Gate** | `pkg/ec2/sizer.go`, stage 1, third — after `k8s-tagged` and `guardrail-mode-off` |
| **Confidence, as a detector of ASG membership** | **Verified.** Two AWS-documented facts, both quoted in §2. |
| **Confidence, as a detector of AWS Batch** | **Inferential, and a deliberate superset.** Every Batch *ASG-backed* managed compute environment is caught; so is every non-Batch ASG. Batch-ness is never claimed in the reason code, only in the prose. |
| **Known residual hole** | Batch `SPOT` compute environments backed by a **Spot Fleet** rather than an ASG. Still assessed today. §4.2. |

The short version: **no EC2 tag key that identifies a Batch instance *as Batch*
is documented anywhere**, so U15a did not invent one. It took the broader
fallback §5 U15a explicitly sanctions — *"suppress ASG members outright and
route them to a template-level sizer"* — and named the reason code after what
the selector actually detects.

---

## 1. What was searched, in the tree, first

§5 U15a's instruction was to look for the answer in this working tree before
reaching for anything else. That search was run and it returns nothing usable:

```
grep -rniE 'aws:batch|ec2spot|fleet-request|containerInstanceId|aws:ecs:' pkg/ cmd/ test/ docs/
```

Every hit outside `docs/design/rds-batch-assessment.md` is an ECS **ARN** in a
test fixture (`arn:aws:ecs:...`), not a tag key. The complete set of EC2 tag
keys this repository declares, before U15a, was:

```
pkg/ec2/ec2.go:97   TagK8sClusterPrefix  "kubernetes.io/cluster/"
pkg/ec2/ec2.go:98   TagEKSCluster        "eks:cluster-name"
pkg/ec2/ec2.go:99   TagAWSEKSCluster     "aws:eks:cluster-name"
pkg/ec2/ec2.go:100  TagKilterMode        "kilter.dev/mode"
pkg/ec2/ec2.go:101  TagName              "Name"
```

All five are Kubernetes or Kilter's own. `pkg/ecs` declares only
`kilter.dev/mode` and `Name`. **There is no prior art in this tree for a Batch,
Spot Fleet or ECS container-instance tag key**, so nothing here could
corroborate a guess. No network access was available to check AWS directly.

---

## 2. The selector that shipped, and the evidence for it

`aws:autoscaling:groupName`. It rests on two verified facts, and the pairing is
the whole argument:

1. **Batch builds ASGs.** AWS Batch *"creates and manages multiple AWS resources
   on your behalf and within your account, including Amazon EC2 Launch
   Templates, **Amazon EC2 Auto Scaling Groups**, Amazon EC2 Spot Fleets, and
   Amazon ECS Clusters"* [verified,
   `managed_compute_environments.html`].
2. **Every ASG member carries the tag, and nobody can remove it.** *"The Auto
   Scaling group automatically adds a tag to instances with a key of
   `aws:autoscaling:groupName` and a value of the Auto Scaling group name"*;
   and *"Do not use the `aws:` prefix in your tag names or values, because it is
   reserved for AWS use. **You can't edit or delete tag names or values with
   this prefix**"* [verified, `ec2-auto-scaling-tagging.html`].

Fact 2 is what makes this a *better* selector than any of the refuted
candidates, and better than every other tag key in this package: it is applied
by AWS at instance creation, on **every** member, and it can be neither forged
by a workload nor stripped by an operator who would rather see a bigger number
in the savings report. `kubernetes.io/cluster/*` — the selector `k8s-tagged`
uses — has none of those properties; it is a convention.

**The honest cost of the pairing:** fact 1 is one-directional. It says Batch
implies ASG (for the EC2/SPOT-on-ASG case); it does not say ASG implies Batch.
So the gate is a superset, and the reason code says `asg-managed` because that
is the only thing it can say without lying in the one field an operator filters
on. `FINDINGS.md` §7.3 carries the full naming argument.

---

## 3. Candidates rejected, and why

All three candidates §5 U15a named were evaluated. A fourth — the tempting
shortcut — was evaluated and rejected too.

| Candidate | Verdict | Why |
|---|---|---|
| **Batch-propagated resource tags** (`ComputeResource.tags`) | **Refuted — not a selector at all.** | The field is `Required: No` and wholly operator-supplied: *"Key-value pair tags to be applied to Amazon EC2 resources that are launched in the compute environment … for example, `{ "Name": "Batch Instance - C4OnDemand" }`. This is helpful for **recognizing your** AWS Batch instances in the Amazon EC2 console"* [verified]. A tag the operator may simply never set cannot gate a suppression, and a tag the operator chooses the value of cannot be matched on. |
| **A Batch-applied system tag** (some `aws:batch:*` key) | **Not found. Nothing claimed.** | Neither the managed-compute-environment page nor the `ComputeResource` API reference documents any tag AWS Batch applies on its own behalf. This is the key §5 U15a marked **[unverified]**, and it stayed unverified. The nearest documented precedent belongs to a *different* service (`aws:ecs:containerInstanceId`, for ECS Managed Instances — [not re-verified], and not Batch). **This is precisely the guess U15a was told not to hard-code**, and it is not in the code. |
| **ECS container-instance membership** of the Batch-managed cluster | **Real, but deferred — not cheap.** | Documented: a managed compute environment *"registers [instances] with an Amazon ECS cluster"* [verified], and `ComputeEnvironmentDetail.ecsClusterArn` gives the join key. But it costs a **third** cloud seam (`ecs:ListContainerInstances` + `ecs:DescribeContainerInstances`) and two IAM actions outside U15's charter — §5 U15b scopes the optional seam to `batch:Describe*` only. This is the one candidate that would convert `asg-managed` into a true `batch-managed`. See §6. |
| **Pattern-matching the ASG *name*** (Batch names its groups `<ce-name>_Batch_<hash>`) | **Rejected.** | Batch's ASG naming is real — the fixture's `prod-batch-ce_Batch_9f2c7e1a3b` reflects it — but it is **not documented as a contract**, and matching an undocumented substring is exactly the confidently-wrong-selector failure §5 U15a warned about. Worse, it would fail *open*: a Batch CE whose naming changed would silently stop being suppressed. `Instance.AutoScalingGroup()` therefore returns the group name **unparsed**, and nothing in this package matches on it. The suppression does not need to know *which* fleet manager it is protecting, only that there is one. |

---

## 4. Fail-closed analysis

§5 U15a required that unverified selectors fail **closed** — that an absent or
unreadable tag set must not silently mean "not Batch". Stated honestly, in both
directions:

### 4.1 Where the gate fails closed (by construction)

- **The superset direction is the safe one.** Because the selector is broader
  than Batch rather than narrower, an ASG-backed Batch compute environment
  cannot escape it by being unrecognisable *as Batch*. There is no Batch-shaped
  input that reaches a proposal through the ASG path.
- **An empty tag *value* is still membership.** `AutoScalingGroup()` returns
  `("(unnamed)", true)` for `aws:autoscaling:groupName=""` rather than treating
  an empty string as absence — pinned by
  `TestFleetSelectorIsTheReservedAWSTagOnly`. That is the one place the
  read could have degraded into a false negative, and it does not.
- **Tags cannot partially fail.** EC2 returns tags **inline in the
  `DescribeInstances` response** — the same call, the same response object that
  produced the instance (`pkg/ec2/collect.go:41`, `:434`). There is no separate
  tag read, no second permission, and no partial-tag failure mode for the
  suppression to mistake for absence. An instance with no tags is an instance
  that genuinely has no tags. **The fail-closed requirement is satisfied by the
  API's shape here, not by defensive code** — which is worth stating plainly so
  that nobody later "hardens" it against a failure mode that does not exist.
  Truncation in this collector drops whole *instances* (§3.3 of `FINDINGS.md`),
  never an instance's tags, and a dropped instance produces no proposal at all.

### 4.2 Where it fails OPEN — the one real residual

**A Batch `SPOT` compute environment backed by a Spot Fleet rather than an Auto
Scaling group is still assessed today.** AWS Batch manages *"Amazon EC2 Spot
Fleets"* alongside ASGs [verified], and a Spot Fleet member need not carry
`aws:autoscaling:groupName`. Trap 14 is therefore **closed for ASG-backed
compute environments and open for Spot-Fleet-backed ones.**

This was not closed because closing it requires a *verified* Spot-Fleet-applied
instance tag, and none could be verified from this tree with no network access.
The obvious candidate (`aws:ec2spot:fleet-request-id`) is plausible and
widely repeated, **but plausible is the standard §5 U15a specifically
rejected** — so it is neither matched nor named as fact in the code. §5 of this
file tells a user how to check it against a real account in one command, and
§6 says what to do with the answer.

### 4.3 On the "configurable predicate" option, and why it did not ship

§5 U15a offers a documented, configurable predicate as the fallback when no
selector can be verified. It did not ship, and the reason is that the shipped
selector **is** verified, which dominates the alternative at both possible
default settings:

- A configurable predicate defaulting to **empty** suppresses nothing. It would
  be a silent no-op that this unit's own tests could not catch, because the
  fixture would carry whatever key the author configured. That is the exact
  failure mode §5 U15a describes.
- A configurable predicate defaulting to **guessed keys** ships the guess with
  the product and merely relocates the blame to a config file.

A verified, unforgeable, always-applied AWS tag beats both. The place where
configurability *would* genuinely earn its keep is narrow and specific — letting
an operator opt in to a Spot-Fleet key (§4.2) that AWS has not documented — and
that is a change to make **after** a user has confirmed the key against a real
account (§5, check 2), not before.

---

## 5. What a user must verify against a real AWS account

None of the following could be run here — there is no network access and no
account. Each check is one command and states what a failure means.

**Check 1 — do your Batch instances actually carry the ASG tag?**
This is the load-bearing assumption. Resolve a compute environment's ECS
cluster, list its container instances, and inspect their tags.
```sh
aws batch describe-compute-environments \
  --query 'computeEnvironments[?computeResources.type==`EC2`||computeResources.type==`SPOT`].[computeEnvironmentName,ecsClusterArn,computeResources.type,computeResources.minvCpus]' --output table

CE_CLUSTER=<ecsClusterArn from above>
IDS=$(aws ecs list-container-instances --cluster "$CE_CLUSTER" --query 'containerInstanceArns' --output text)
aws ecs describe-container-instances --cluster "$CE_CLUSTER" --container-instances $IDS \
  --query 'containerInstances[].ec2InstanceId' --output text
# then, for those instance ids:
aws ec2 describe-instances --instance-ids <ids> \
  --query 'Reservations[].Instances[].[InstanceId,Tags]' --output json
```
*Expected:* every one carries `aws:autoscaling:groupName`, valued like
`<ce-name>_Batch_<hash>`. **If any does not, U15a does not protect that compute
environment** and §6's ECS join becomes required rather than optional.

**Check 2 — do you run any Spot-Fleet-backed Batch compute environment?**
This is the known hole (§4.2).
```sh
aws ec2 describe-instances \
  --filters "Name=tag-key,Values=aws:ec2spot:fleet-request-id" \
  --query 'Reservations[].Instances[].[InstanceId,InstanceType,LaunchTime]' --output table
```
*Expected, for the hole to be absent:* empty, **or** every instance returned
also carries `aws:autoscaling:groupName`. If this returns Spot instances that
lack the ASG tag, they are being sized today and should not be — and the command
returning them is simultaneously the confirmation that
`aws:ec2spot:fleet-request-id` is a real applied key, which is the evidence
§4.2 lacked.

**Check 3 — how wide is the blast radius on your account?**
The gate refuses **every** ASG member, not only Batch's. That is a deliberate,
documented cost (`FINDINGS.md` §7.3), and its size is account-specific.
```sh
aws ec2 describe-instances --filters "Name=tag-key,Values=aws:autoscaling:groupName" \
  --query 'length(Reservations[].Instances[])'
```
*Expected:* a number you find acceptable to see reported as `asg-managed`
instead of individually sized. If it is large and mostly non-Batch, the right
follow-up is the template-level sizer `FINDINGS.md` §5 defers — not loosening
this gate.

**Check 4 — is there actually a floor to find?**
```sh
aws batch describe-compute-environments \
  --query 'computeEnvironments[?computeResources.minvCpus>`0`].[computeEnvironmentName,computeResources.minvCpus,computeResources.allocationStrategy,computeResources.bidPercentage]' --output table
```
*Expected:* the compute environments U15b will price. `minvCpus` maintains
instances *"even if the compute environment is `DISABLED`"* [verified] — a
non-zero value here with an empty queue is trap 14 live in your account.

---

## 6. What U15b inherits

U15b (`batchenrich.go`, already landed alongside this unit) inherits three
consequences of the selector decision:

1. **No instance→compute-environment mapping exists.** §3 rejected the ECS join
   as out of scope, so this package cannot tell you *which* instances hold a
   given floor. That is the structural reason U15b's findings are
   **report-scope** (`Report.Advisories`) rather than per-assessment: they
   describe a compute environment's configuration, which no EC2 instance
   carries. Closing §3's ECS candidate is what would let the floor advisory name
   the instances actually holding it.
2. **The suppression and the insight are deliberately not joined.** An
   `asg-managed` instance carries no advisories at all (`Excluded()` includes
   the code, and `Report.Validate()` enforces the emptiness). The floor is
   priced against the compute environment instead. The two halves of U15 meet in
   the report, not in the assessment.
3. **The residual Spot Fleet hole is U15a's, not U15b's.** U15b reads
   `batch:DescribeComputeEnvironments`, which already sees `SPOT` compute
   environments and prices their floors — so a Spot-Fleet-backed CE gets a
   correct *advisory* while its instances still get a wrong *proposal*. That
   asymmetry is the sharpest reason to close §4.2, and it is visible in the
   report today.

Whoever closes §4.2 should note that the ECS join in §3 closes both it and the
naming problem at once: joining `ecsClusterArn` → `ecs:ListContainerInstances` →
`ec2InstanceId` identifies Batch instances regardless of whether an ASG or a
Spot Fleet launched them, and would justify a genuine `batch-managed` code
sitting *before* `asg-managed` in stage 1.

---

## 7. Test-first record

§5 U15a required that `TestMinVCpusFloorIsNotReadAsIdleDemand` **fail on the
tree before the change**, and that the failure be observed rather than assumed.
It was, and it was re-confirmed on **2026-08-26** by disabling only the stage-1
ownership gate in `pkg/ec2/sizer.go` and re-running the two acceptance tests:

```
--- FAIL: TestBatchManagedInstanceIsSuppressedFiresAlone
    batch_test.go:60: i-0batchfloor: suppression = "memory-blind", want "asg-managed"
--- FAIL: TestMinVCpusFloorIsNotReadAsIdleDemand
    batch_test.go:106: i-0batchfloor: suppression = "memory-blind", want "asg-managed"
```

The gate was then restored and the file verified byte-identical by checksum.

The wrong answer the gate stops is not hypothetical and is not a stale claim in
a commit message — it is **re-derived on every test run**. The counterfactual
half of `TestMinVCpusFloorIsNotReadAsIdleDemand` builds an untagged twin of the
same instance with the same 30 days of ~2 % CPU and asserts it still clears the
window, coverage, confidence and net-savings gates:

```
batch_test.go:142: without the fleet tag kilter would propose m5.2xlarge → r5.xlarge
                   at net $96.3600/mo, confidence 0.71 — the recommendation this
                   suppression exists to stop
```

That construction matters more than the one-time observation: it means the test
fails loudly if the ownership gate ever becomes the *only* thing standing
between the fixture and a recommendation — i.e. if some future evidence gate
starts doing the work by accident, the way the 7-day `MinWindow` gate
accidentally protected short-lived Batch instances before this unit
(`FINDINGS.md` §7.1).

The third acceptance test — an idle-looking instance with **no** fleet signal is
still sized normally — is the `i-2plain` control inside
`TestBatchManagedInstanceIsSuppressedFiresAlone`. Without it, that test would
still pass if the gate suppressed the entire account, which is the failure mode
a broad ownership suppression is most likely to have.

---

## 8. Claim ledger

Every load-bearing claim in this file, with its verification status. Anything
marked below **[unverified]** is not depended on by code.

| Claim | Status |
|---|---|
| Batch creates and manages EC2 Auto Scaling Groups | **[verified]** — `managed_compute_environments.html` |
| Batch also manages EC2 Spot Fleets | **[verified]** — same page (this is what makes §4.2 a real hole) |
| ASGs tag members `aws:autoscaling:groupName` automatically | **[verified]** — `ec2-auto-scaling-tagging.html` |
| The `aws:` tag prefix is reserved and cannot be edited or deleted | **[verified]** — same page |
| `minvCpus` maintains instances even when the CE is `DISABLED` | **[verified]** — `API_ComputeResource.html` |
| Batch assumes full control and may terminate instances at any time | **[verified]** — `managed_compute_environments.html` |
| `ComputeResource.tags` is optional and operator-supplied | **[verified]** — `API_ComputeResource.html` |
| A managed CE registers instances with an ECS cluster | **[verified]** — `managed_compute_environments.html` |
| Some `aws:batch:*` EC2 tag key exists | **[unverified — not found, not claimed, not matched]** |
| `aws:ec2spot:fleet-request-id` is applied to Spot Fleet members | **[unverified — not matched; §5 check 2 is how to settle it]** |
| Batch ASGs are named `<ce-name>_Batch_<hash>` | **[unverified as a contract — observed shape only, deliberately not matched]** |
| `aws:ecs:containerInstanceId` exists for ECS Managed Instances | **[not re-verified — irrelevant to Batch either way]** |
