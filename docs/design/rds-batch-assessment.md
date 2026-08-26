# RDS/Aurora and AWS Batch: the U10 decision

**Status:** decision checkpoint (docs only; no code in this change).
**Answers:** `docs/design/compute-domains.md` §6 **U10** — "revisit RDS/Batch after
U5–U8 field feedback." U5 (`pkg/ec2`), U6 (`pkg/ebs`), U8 (`pkg/ecs`) and U9
(`pkg/lambda`) have all shipped. This is that revisit.
**Verification:** AWS behaviour marked **[verified]** was fetched from the cited
URL on **2026-08-26** and is quoted where the wording is load-bearing. Items
marked **[not re-verified]** are documented behaviour taken from a secondary
route (search corroboration of an AWS page). Items marked **[unverified]** must
be confirmed before any code depends on them. Every line count and test name in
§1 was read out of this working tree on the same date.
**Prior verdicts this document overturns:** §3.4 deferred RDS ("high blast
radius, modest ROI") and declined Batch ("covered transitively"). The Batch
verdict survives and gets sharper. The RDS verdict was right about the blast
radius and wrong about which part of RDS carries it.

---

## 0. The answer, first

| Subject | Verdict | Unit | Effort |
|---|---|---|---|
| RDS **commitment modelling** (Reserved DB Instances in `pkg/pricing/commit`) | **Build** — and it gates everything else | U12 | M |
| RDS **read-only observation + refusal report** | **Build**, structurally advisory-only | U11 | M–L |
| RDS **storage-performance rightsizing** (gp2→gp3, gp3 IOPS/throughput) | **Build** | U13 | M |
| RDS storage-performance **actuation** (`ModifyDBInstance` on IOPS/throughput only) | **Build**, behind approval | U14 | S–M |
| RDS **instance-class actuation** (the failover) | **Decline** for this horizon | — | — |
| RDS **allocated-storage reduction** | **Decline** — the API cannot do it | — | — |
| **Aurora** | **Defer** — it is a third billing model wearing RDS's name | — | — |
| **AWS Batch as a domain** | **Decline** | — | — |
| **AWS Batch containment** (one suppression + three insights) | **Build** | U15 | S + S |

The short version: **the resizable thing in RDS and the safe thing in RDS are
not the same thing.** Instance class is where the money is and it is a failover;
storage performance is online, reversible and structurally identical to the
volume work U6 already shipped. Batch has no money of its own at all — but it
has a live footgun aimed at `pkg/ec2`, and closing it is the only Batch work
worth doing.

---

## 1. What the seam actually cost to extend

§2 of `compute-domains.md` predicted that the expensive parts were already
built: "*Everything else — decaying histograms (`pkg/histogram`), forecasting
(`pkg/forecast`), behavior classes (`pkg/patterns`), confidence scoring,
guardrails, approvals, the ledger — is reusable as-is because those packages are
already key-agnostic pure Go.*" Four domains later, that prediction is
measurably wrong for cloud domains and right only for Kubernetes-shaped ones.
This section is the measurement, because a forecast for RDS that is not
calibrated against the four shipped domains is worth nothing.

### 1.1 The line counts

Production Go (non-`_test.go`) and test Go per shipped domain, this tree,
2026-08-26:

| Package | Prod | Test | Tests | Fuzz | Reason codes declared |
|---|---|---|---|---|---|
| `pkg/ec2` (U5) | 3,055 | 2,132 | 60 | 2 | 13 + 4 advisories |
| `pkg/ecs` (U8) | 3,018 | 2,805 | 65 | 2 | 20 + 3 advisories |
| `pkg/lambda` (U9) | 3,869 | 2,037 | 63 | 2 | 14 + 5 advisories |
| `pkg/ebs` (U6) | 4,100 | 2,575 | 53 | 2 | 13 |
| **four domains** | **14,042** | **9,549** | **241** | **8** | |
| `pkg/domain` (the seam) | 870 | 880 | 25 | 1 | |
| `pkg/pricing/commit` (U4) | 1,359 | 1,664 | 32 | 2 | |
| **shared substrate** | **2,229** | **2,544** | | | |

`pkg/ec2`, `pkg/lambda` and `pkg/ebs` carry their replay harness in a
non-`_test.go` file (`fixture.go`: 337 / 208 / 536 lines) so it can be imported
by later units; `pkg/ecs` keeps it in `fixtures_test.go`. Netting that out, the
four domains are **12,961 lines of shipping code** against **2,229 lines of
shared substrate** — about **5.8:1**. The seam is real leverage, and it is
leverage on roughly one line in six.

### 1.2 Where those lines went, per domain

| Package | Domain-specific economics | Cloud seam + replay harness | Actuator | Types/contract + report |
|---|---|---|---|---|
| `pkg/ec2` | 1,281 (`sizer.go` 905, `burst.go` 376) | 1,027 (`collect.go` 690, `fixture.go` 337) | 0 | 747 (`ec2.go` 487, `report.go` 260) |
| `pkg/ecs` | 1,003 (`sizer.go` 768, `price.go` 235) | 970 (`collect.go`) | 411 (`actuate.go`) | 634 (`ecs.go` 392, `report.go` 242) |
| `pkg/lambda` | 1,831 (`sizer.go` 844, `parse.go` 491, `stats.go` 303, `cost.go` 193) | 757 (`collect.go` 549, `fixture.go` 208) | 0 | 1,281 (`lambda.go` 522, `report.go` 321, `domain.go` 438) |
| `pkg/ebs` | **594** (`parity.go`) | 1,416 (`collect.go` 880, `fixture.go` 536) | 818 (`actuate.go`) | 1,272 (`ebs.go` 803, `report.go` 469) |

`pkg/ebs` is the sharpest reading. The entire novel arithmetic — the gp2/gp3
regime table, the refusal band, the floor rule, the thing no other tool gets
right — is **594 lines, 14 % of the package**. The other 86 % is collector,
actuator, report, refusal plumbing and fixtures. **The economics is the cheap
part. The honesty machinery around it is the expensive part**, and none of it
was reusable.

### 1.3 What was actually reused (production imports only)

| Package | `ec2` | `ecs` | `lambda` | `ebs` | `domain/fargate` |
|---|---|---|---|---|---|
| `pkg/pricing/commit` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `pkg/domain` | ❌ | ✅ | ✅ | ✅ | ✅ |
| `pkg/pricing` | ✅ | ✅ | ❌ | ❌ | ✅ |
| `pkg/plan` (risk constants) | ❌ | ✅ | ❌ | ✅ | ✅ |
| `pkg/guard` | ❌ | ❌¹ | ❌ | ✅ | ✅ |
| `pkg/decision` | ❌ | ✅ | ❌ | ❌ | ✅ |
| `pkg/model` | ✅ | ✅ | ❌ | ❌ | ✅ |
| **`pkg/histogram`** | ❌ | ❌ | ❌ | ❌ | ❌ |
| **`pkg/forecast`** | ❌ | ❌ | ❌ | ❌ | ❌ |
| **`pkg/patterns`** | ❌ | ❌ | ❌ | ❌ | ❌ |
| **`pkg/recommend`** | ❌ | ❌ | ❌ | ❌ | ✅ |
| **`pkg/safety`** | ❌ | ❌ | ❌ | ❌ | ✅ |
| `pkg/binpack` | ❌ | ❌ | ❌ | ❌ | ❌ |

¹ `pkg/ecs` declares its own `modeOff`/`modeRecommend`/`modeApply` constants and
pins them equal to `pkg/guard`'s with `TestModeVocabularyMatchesGuard` rather
than importing it.

**Zero of the four cloud domains import `histogram`, `forecast`, `patterns`,
`recommend` or `safety`.** Only `pkg/domain/fargate` — the Kubernetes-shaped
domain, whose targets really are containers — reuses them. `pkg/ec2/FINDINGS.md`
§5 states the reason without hedging: *"The sizer computes percentiles directly
from the delivered series rather than folding them into `pkg/histogram`. Correct
for a stateless read-only report … That is a `Domain.Learn`/`Checkpoint`
concern, and `Domain` does not exist yet."* `pkg/ecs/FINDINGS.md` §3.1 gives the
structural version: *"unlike Kubernetes, CloudWatch **is** the history store, so
a collection pass carries the whole window and no cross-tick histogram is
needed."*

That is not a mistake to correct. It is the actual shape of the problem: a
CloudWatch-backed domain gets a *window* per collection, not a *sample* per
tick, so the online-histogram machinery has nothing to be online about. **Any
estimate for RDS that assumes the L1 statistical plane comes for free is wrong
by about 800–1,800 lines per domain.**

### 1.4 The thing four domains each rebuilt: the CloudWatch seam

`MetricDataQuery`, `MetricDataResult`, `GetMetricDataInput` and `MetricsAPI` are
declared **four times**, once per domain:

- `pkg/ec2/collect.go:80,115,91,131`
- `pkg/ebs/collect.go:211,244,222,260`
- `pkg/ecs/collect.go:345,378,356,394`
- `pkg/lambda/collect.go:90,119,100,133`

`pkg/ecs/FINDINGS.md` §3.8 anticipated this and drew the line in the right
place: *"The two query shapes, periods and status handling differ, and coupling
two domain packages to lift ~40 lines seemed worse than the duplication. **If a
third domain needs it, lift a `pkg/cloudwatch` seam then.**"* That threshold has
now been crossed twice — `lambda` and `ebs` both shipped their own copy after
`ecs` wrote that sentence.

The 40 lines of struct are not the cost. The cost is that each domain
independently re-derived and independently re-tested the same four CloudWatch
truths:

| Truth | ec2 | ecs | ebs | lambda |
|---|---|---|---|---|
| Batch at ≤ 500 series/call | `TestCollectBatchesWithinTheGetMetricDataLimit` | `MaxSeriesPerCall` | ✅ | ✅ |
| Route results by query **ID**, never position | `TestCollectDiscardsUnknownQueryIDs` | `TestCollectorRoutesMetricsByQueryID` | ✅ | "Copy `Id` verbatim in BOTH directions" |
| A *missing* result is truncation, not "no usage" | `TestCollectDetectsTruncatedMetricResponse` | `TestCollectorMarksAMissingResultTruncated` | `TestCollectorTruncatedResponse` | ✅ |
| Clamp the window to CloudWatch retention / publication granularity | `TestCollectClampsPeriodToPublicationGranularity` | `TestCollectorClampsTheWindowToCloudWatchRetention` | ✅ | ✅ |

An RDS domain would be the **fifth** independent derivation, and it needs every
one of the four (RDS publishes at 60 s with 15-day 1-minute retention — §2.1, so
the retention clamp `pkg/ecs` already wrote applies verbatim).

Three domains also wrote three independent confidence models
(`pkg/ec2/sizer.go:827`, `pkg/ecs/sizer.go:646`, `pkg/lambda/sizer.go:737`),
two of them with the same "earned, not lost" structure and the same
`weakestFactor()` helper (`pkg/ec2/sizer.go:867`, `pkg/lambda/sizer.go:784`).

### 1.5 The seam gap `pkg/ec2` paid for, and the one nobody has paid yet

`pkg/ec2` does not import `pkg/domain` at all — it was built in parallel and
declares its own `TargetRef`, `Spec`, `Evidence` and `Snapshot`
(`pkg/ec2/ec2.go:146` and following), 487 lines, with an adapter still owed
(`pkg/ec2/FINDINGS.md` §6.1). That cost is now sunk and is **not** a cost RDS
would pay: `pkg/domain` exists, and `pkg/lambda` (438 lines in `domain.go`) and
`pkg/ebs` (`ebs.go`) show what the adapter costs when the seam is there —
call it 400–800 lines including `Learn`/`Checkpoint`/`Restore`/`Health`.

The gap nobody has paid: **`pkg/pricing/commit` has a closed usage-kind set.**

```go
// pkg/pricing/commit/commit.go:87
KindEC2     Kind = "ec2"
KindFargate Kind = "fargate"
KindLambda  Kind = "lambda"
```

and `Usage.Validate()` (`commit.go:176`) *rejects* anything else:
`"unknown kind %q"`. `pkg/ebs` got in by declaring EBS storage as
`Kind: commit.KindEC2, SPIneligible: true` with no instance type
(`pkg/ebs/ebs.go:759-768`) — legitimate, because no SP or RI covers EBS, and
pinned by `TestEBSIsNeverAbsorbedByACommitment`.

RDS cannot take that route, because RDS *does* have a commitment product and it
is not any of the three the waterfall knows. **U12 is therefore the first unit
in this whole line of work that must edit a shipped core package.** Every
domain from U5 to U9 landed with `go.mod`, `go.sum` and every existing package
untouched; the RDS work breaks that streak by exactly one file plus one branch.
That is worth stating plainly because it is the single largest structural
difference between this wave and the last one.

*One thing that is **not** broken today, stated so nobody fixes a non-bug:*
Compute Savings Plans cover EC2, Fargate and Lambda
(`pkg/pricing/commit/waterfall.go:241`) and **not** RDS, so omitting RDS spend
from the account-wide baseline does not distort any existing domain's absorption
math. There is no latent wrong number waiting in the tree. There is only a
missing capability.

### 1.6 The calibrated estimate for a new cloud domain

From four data points, a new CloudWatch-backed AWS domain of RDS's complexity
costs:

- **3,000–4,100 lines** of production Go, of which **14–47 %** is the actual
  economics and the balance is collector, refusal machinery, report and
  fixtures;
- **2,000–2,800 lines** of tests, **53–65** test functions, **2** fuzz targets;
- **13–20** locally-declared reason codes plus the shared commitment codes;
- **zero** reuse of `histogram`/`forecast`/`patterns`/`recommend`/`safety`;
- **one more** private copy of the CloudWatch seam unless it is lifted first.

RDS is at the top of that band, not the middle, because it adds three axes none
of the four shipped domains had: a per-engine semantic layer (§2.2), a
deployment-topology multiplier (§2.5), and a commitment product that does not
exist yet (§2.6).

---

## 2. RDS / Aurora

### 2.1 Where the signal is — and it is better than plain EC2

This is the one place §3.4's deferral was too pessimistic. RDS's default
telemetry is strictly better than the plain-EC2 telemetry `pkg/ec2` had to work
with:

- **1-minute publication, no paid feature.** *"By default, Amazon RDS
  automatically sends metric data to CloudWatch in 1-minute periods … Data
  points with a period of 60 seconds (1 minute) are available for 15 days."*
  [verified: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/monitoring-cloudwatch.html]
  Plain EC2 gets 300 s unless the customer pays for detailed monitoring, which
  is why `pkg/ec2` carries a 25 % `CoarseResolutionHeadroom` it openly calls
  *"a stated safety margin, not a derived truth"* (`pkg/ec2/FINDINGS.md` §3.2).
  **On RDS that entire compensation disappears**, and with it the confidence
  penalty (`pkg/ec2` loses 60 % of a 0.15 weight to resolution).
- **A memory metric exists by default.** `FreeableMemory`, no CloudWatch agent,
  no Enhanced Monitoring. `pkg/ec2`'s dominant limitation — memory-blind mode,
  confidence capped at 0.71 against a 0.65 floor — has no analogue here.
- **A storage-fill metric exists**: `FreeStorageSpace`, *"The amount of
  available storage space."* `pkg/ec2` declares disk space a permanent blind
  spot because EBS metrics are I/O, not fill; RDS publishes fill directly.
- **Connection count**: `DatabaseConnections`, with an explicit caveat that it
  under-counts sessions (engine-internal, parallel-execution and scheduler
  sessions are excluded) — which is the safe direction for an *idle* test.
- **Credit metrics for `db.t2`/`db.t3`/`db.t4g`**: `CPUCreditBalance`,
  `CPUSurplusCreditsCharged` and friends, with the same semantics `pkg/ec2`
  already models — *"CPU credit metrics are available at a five-minute frequency
  only."*
- **`BurstBalance`** (*"The percent of General Purpose SSD (gp2) burst-bucket
  I/O credits available"*) and **`EBSIOBalance%`/`EBSByteBalance%`**, the
  instance-level EBS burst buckets, *"available for basic monitoring only"* and
  *"based on the throughput of all volumes, including the root volume."*

  [all verified: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-metrics.html]

The 15-day 1-minute retention means the window is capped at 15 days — the same
clamp `pkg/ecs` already implements and tests.

### 2.2 Where the signal lies: `FreeableMemory` is `MemAvailable`

The metrics reference is precise, and the precision is the problem:

> `FreeableMemory` — The amount of available random access memory. **For
> MariaDB, MySQL, Oracle, and PostgreSQL DB instances, this metric reports the
> value of the `MemAvailable` field of `/proc/meminfo`.**
> [verified: rds-metrics.html]

`MemAvailable` is the kernel's estimate of memory obtainable for a new
allocation *without swapping* — and it **counts reclaimable page cache as
available**. The consequence splits by engine:

- **PostgreSQL** deliberately leans on the OS page cache; `shared_buffers` is
  typically a fraction of RAM and the rest of the working set lives in cache
  that `MemAvailable` reports as free. A well-tuned PostgreSQL instance with a
  hot 200 GiB working set fully cached reports *large* `FreeableMemory` — and
  downsizing on that evidence evicts the cache, converts memory hits into
  storage reads, and moves the cost from the instance line to the I/O and
  latency lines. The metric says "spare"; the truth is "in use, productively."
- **MySQL/InnoDB** holds its buffer pool as anonymous memory, which
  `MemAvailable` does **not** count as available. Here `FreeableMemory` is close
  to real headroom — but the buffer pool is sized from the parameter group as a
  fraction of instance memory **[unverified: the RDS default parameter group's
  `innodb_buffer_pool_size` formula]**, so shrinking the class shrinks the pool,
  which changes the I/O profile, which changes CPU. The memory signal is honest
  and the *effect* of acting on it is not linear.
- **SQL Server and Oracle** have their own memory managers and their own
  license-linked class constraints (§2.8).

This is a **trap class §7 does not currently contain**. Trap 4 (memory-blind EC2
downsizing) is about a signal being *absent*. This is a signal being *present
and engine-dependently misleading* — which is worse, because the existing rule
("never propose less memory than current without a memory signal") is satisfied
on paper while being violated in spirit. `pkg/ec2`'s memory-blind refusal would
not fire. Nothing would fire.

**The only honest handling is an engine-keyed policy**, and it must default to
refusal for engines whose semantics are not encoded, exactly as `pkg/ec2`
defaults `t2` to `unknown` rather than guessing its baselines
(`pkg/ec2/FINDINGS.md` §5: *"Rather than guess, `burstFamilies` recognizes t2 as
credit-based while `burstBaselines` has no row for it, so every t2 instance
lands in `unknown` and is refused."*).

### 2.3 Storage: the floor only ever ratchets up

Three verified sentences settle this entirely:

> *"Autoscaling can't decrease the allocated storage. **You can't reduce the
> amount of storage for a DB instance after storage has been allocated.**"*
>
> *"After a DB instance has been autoscaled, its allocated storage can't be
> reduced."*
>
> *"For storage, **you can't manually reduce the allocated storage of a DB
> instance using the `modify-db-instance` command.**"* — the documented
> alternatives are a blue/green deployment or a manual migration to a new
> instance.
>
> [verified: https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html]

So RDS allocated storage is a **monotone ratchet**, and Kilter's existing rule
("never recommend volume shrink"; `pkg/ebs`: *"ModifyVolume can only grow;
growing is not rightsizing"*) transfers unchanged.

What is *new* is the autoscaler that moves the ratchet on its own. Storage
autoscaling fires when [verified, same page]:

- *"Free available space is less than or equal to 10 percent of the allocated
  storage."*
- *"The low-storage condition lasts at least five minutes."*
- *"Storage optimization has completed on the instance for the previous storage
  modification, and fewer than four storage modifications have occurred in the
  past 24 hours."*

and adds the **greater** of 10 GiB, 10 % of current allocation, or *"Predicted
storage growth exceeding the current allocated storage size in the next 7 hours
based on the `FreeStorageSpace` metrics from the past hour."*

Two consequences Kilter must encode:

1. **`AllocatedStorage` observed today is an upper bound that is also a floor.**
   A 4 TiB instance holding 300 GiB of data is paying for 4 TiB forever. That is
   a real, large, *reportable* number — and it is **not a recommendation**,
   because the only remediation is a blue/green cutover or a migration, both of
   which are outside anything Kilter does or should do. This is the clearest
   "quantify it, refuse to act on it" finding in the whole RDS surface.
2. **Autoscaling operations are invisible to the change log.** *"Autoscaling
   operations aren't logged by AWS CloudTrail."* [verified, same page] So a
   ledger entry that says "we changed nothing and storage grew" has no
   corroborating event anywhere. Any RDS ledger must treat an allocated-storage
   increase between two snapshots as **unattributed by default**, never as
   Kilter's doing and never as a regression.

### 2.4 Storage performance: RDS's gp3 is not EBS's gp3

This is where the money that *is* safely reachable lives, and it is also where
`pkg/ebs` would be confidently wrong if reused.

RDS **stripes across four volumes** above an engine-dependent threshold, and the
baselines step at that threshold [verified:
https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/CHAP_Storage.html]:

| Engine | Striping threshold | gp3 baseline below | gp3 baseline at/above | gp3 provisionable range at/above |
|---|---|---|---|---|
| Db2, MariaDB, MySQL, PostgreSQL | 400 GiB | 3,000 IOPS / 125 MiB/s | 12,000 IOPS / 500 MiB/s | 12,000–64,000 IOPS; 500–4,000 MiB/s |
| Oracle | 200 GiB | 3,000 / 125 | 12,000 / 500 | 12,000–64,000; 500–4,000 |
| SQL Server | never (1 volume) | 3,000 / 125 | — | 3,000–80,000 IOPS; 125–2,000 MiB/s |

Below the threshold the provisioning columns read literally **"N/A"** — *"For
every DB engine except RDS for SQL Server, you can provision additional IOPS and
storage throughput **when storage size is at or above the threshold value**."*

And RDS gp2 is a completely different animal from EC2 gp2 [verified, same page]:

| Engine / size | Baseline IOPS | Baseline throughput | Burst IOPS |
|---|---|---|---|
| MariaDB/MySQL/PostgreSQL 5–399 GiB | 100–1,197 | 128–250 MiB/s | 3,000 |
| … 400–1,335 GiB | 1,200–4,005 | **512–1,000 MiB/s** | **12,000** |
| … 1,336–3,999 GiB | 4,008–11,997 | 1,000 MiB/s | 12,000 |
| … 4,000–65,536 GiB | 12,000–64,000 | 1,000 MiB/s | N/A (baseline exceeds burst) |
| SQL Server 334–999 GiB | 1,002–2,997 | 250 MiB/s | 3,000 |

Compare `pkg/ebs/parity.go`'s model, which is correct for a raw EBS volume:
`gp2 burst = 3,000, only for volumes ≤ 1,000 GiB`; `throughput ceiling 128 MiB/s
below 334 GiB, 250 MiB/s at or above`. **Every one of those three constants is
wrong for RDS**: burst reaches 12,000 in the striped regime, throughput reaches
1,000 MiB/s, and the regime boundary is 400/200/never rather than 334/1,000.

The falsifiable consequence: a **500 GiB MySQL** instance on gp2 delivers
1,500 baseline IOPS and (per the band) 512–1,000 MiB/s. Converting it to gp3
lands on the 12,000 IOPS / 500 MiB/s striped baseline — IOPS goes *up* roughly
8×, and **throughput goes down**, from at least 512 MiB/s to 500 MiB/s, with the
only remedy being provisioned throughput that costs money. That is precisely the
shape of `pkg/ebs`'s 334–375 GiB refusal band, relocated to a different size
range by a different table. A parity checker that reuses `pkg/ebs`'s numbers
would claim a saving in the band where RDS refuses and refuse in the band where
RDS converts cleanly.

Three more constraints that shape the unit:

- **Reducing provisioned performance is bounded below by the baseline.** In the
  striped regime the range *starts* at 12,000 / 500 — you can reduce a
  20,000-IOPS instance to 12,000 and no further. Below the threshold you cannot
  provision at all, so there is nothing to reduce. **The addressable opportunity
  is only the over-provisioned tail above the baseline**, which is a genuinely
  smaller pool than EBS's gp2→gp3 conversion. Anyone sizing the ROI of U13
  should size it on that tail, not on total RDS storage spend.
- **Storage modifications are online but rate-limited.** *"Downtime doesn't
  occur during this change. Performance might be degraded during the change"*
  (allocated storage) and *"Downtime doesn't occur during this change"*
  (Provisioned IOPS; storage throughput) — but *"You can perform a maximum of
  four storage modifications on a DB instance within any 24-hour period"* and
  *"You can't modify allocated storage if the DB instance status is
  **storage-optimization**."* [verified:
  https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_ModifyInstance.Settings.html]
  `pkg/ebs` already has the cooldown-plus-in-flight-state machinery
  (`TestPollingAcrossIncompleteModification`,
  `TestResumeOfMatchingInFlightModification`); the shape transfers, the
  constants do not.
- **AWS's own page contradicts itself on the SQL Server gp3 ceiling** — the gp3
  table says 3,000–80,000 IOPS while the comparison table on the same page says
  "Maximum IOPS: 64,000 (16,000 on RDS for SQL Server)". **[unverified: which is
  current]**. Do not hardcode either; read the envelope from
  `rds:DescribeValidDBInstanceModifications` and refuse when it is unavailable.

### 2.5 Multi-AZ and replica topology: the multiplier and the fan-out

A Multi-AZ DB instance is *"a synchronous standby replica in a different
Availability Zone"* and *"**You can't use a standby replica to serve read
traffic**"* [verified:
https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZSingleStandby.html].

The clean billing evidence is the reserved-instance normalized-unit table, which
prices a Multi-AZ deployment at exactly **twice** the Single-AZ units of the same
size, and a Multi-AZ **cluster** (one writer, two readers) at three times
[verified:
https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_WorkingWithReservedDBInstances.html]:

| size | Single-AZ | Multi-AZ instance | Multi-AZ cluster |
|---|---|---|---|
| micro | 0.5 | 1 | 1.5 |
| small | 1 | 2 | 3 |
| medium | 2 | 4 | 6 |
| large | 4 | 8 | 12 |
| xlarge | 8 | 16 | 24 |
| 2xlarge | 16 | 32 | 48 |
| … | … | … | … |
| 32xlarge | 256 | 512 | 768 |

Three implications:

1. **Every instance-class delta is doubled (or tripled) before it is a dollar.**
   A domain that prices a `db.r6i.xlarge` at the Single-AZ rate under-reports a
   Multi-AZ instance's cost — and its saving — by half. This is the mirror image
   of §7 trap 2 (Fargate quantization blindness): the price function is not the
   catalog lookup.
2. **Read replicas are separate billable instances that Kilter must attribute,
   not resize.** A replica's own CPU/memory series describes the *replica's*
   workload; its size, however, is usually chosen to match the primary so it can
   be promoted. Recommending a smaller replica on utilization evidence proposes
   an instance that cannot absorb a failover — a correctness claim about an
   availability property Kilter cannot observe. **Replica downsizing is
   structurally advisory, permanently.** The exception that *is* safe to report:
   a replica with zero `DatabaseConnections` across the window is either unused
   or a pure standby, and either way that is a fact worth stating with a dollar
   attached.
3. **The Multi-AZ toggle itself is off-limits.** *"Downtime doesn't occur during
   this change"* [verified: USER_ModifyInstance.Settings.html] — it is
   mechanically cheap, and it is a deliberate availability posture. Turning off
   Multi-AZ halves a bill and halves an SLA. Kilter must not represent it, on
   the same principle that makes `ecs.UpdateServiceInput` carry no desired-count
   field: *"§3.4's 'never change desired count' is unrepresentable rather than
   merely forbidden"* (`pkg/ecs/FINDINGS.md` §1.5).

### 2.6 Reserved DB Instances are not Reserved Instances

Verbatim, and every clause changes the arithmetic
[all verified: USER_WorkingWithReservedDBInstances.html]:

- *"Discounts for reserved DB instances are tied to instance type and AWS
  Region."*
- *"Size-flexible reserved DB instances are available for DB instances with the
  same AWS Region and database engine. **Size-flexible reserved DB instances can
  only scale in their instance class type.** For example, a reserved DB instance
  for a `db.r6i.large` can apply to a `db.r6i.xlarge`, but **not** to a
  `db.r6id.large` or `db.r7g.large`, because `db.r6id.large` and `db.r7g.large`
  are different instance class types."*
- *"**Reserved DB instance benefits apply to both Multi-AZ and Single-AZ
  configurations.** … you can move from a Single-AZ deployment running on one
  large DB instance (four normalized units per hour) to a Multi-AZ deployment
  running on two medium DB instances (2+2 = 4 normalized units per hour)."*
- *"**Size flexibility does not apply to RDS for SQL Server and RDS for Oracle
  License Included.**"* (It does apply to Db2, MariaDB, MySQL, PostgreSQL and
  Oracle BYOL.)
- *"The price for a reserved DB instance **doesn't provide a discount for the
  costs associated with storage, backups, and I/O.** It provides a discount only
  on the hourly, on-demand instance usage."*
- *"**You can't cancel a reserved DB instance.**"*

And the thing that is *not* in that page but is decisive: **Compute Savings
Plans do not cover RDS.** The waterfall's Compute-SP branch
(`pkg/pricing/commit/waterfall.go:241`) admits exactly `KindEC2`, `KindFargate`,
`KindLambda`. RDS therefore needs a **fourth commitment product**, not a fourth
usage kind riding an existing one.

Mapping §4.4's three worked examples onto RDS:

- **Example 1 (family migration off an RI, +135 %)** reappears as
  `db.r6i.2xlarge → db.r7g.2xlarge` (Graviton). The r6i reservation strands
  entirely; the r7g bills on-demand. Same trap, different table.
- **Example 2 (downsizing inside the family, claimed 50 % / realized 0 %)**
  reappears as `db.r6i.xlarge → db.r6i.large` under a `db.r6i.xlarge` Multi-AZ
  reservation: 16 units reserved, 8 used, 8 stranded, bill unchanged.
- **Example 3 (SP under-commitment)** has **no RDS analogue** — there is no
  account-wide RDS commitment to under-consume, only per-family-type
  reservations. That makes RDS's netting *simpler* than EC2's in one respect and
  strictly harder in another: the size-flexibility rule is engine-gated, so the
  same downsize is partially absorbed on PostgreSQL and 100 % stranded on SQL
  Server.

**Until U12 lands, an RDS domain cannot produce a net-savings number at all** —
not a conservative one, not an under-claiming one. `Usage.Validate()` would
reject the line. That is the honest failure mode (loud, not wrong), and it is
why U12 is a hard prerequisite rather than a nice-to-have.

### 2.7 A resize is a failover, and a failover is a DNS change

| Modification | Downtime, verbatim | Class |
|---|---|---|
| **DB instance class** | *"Downtime occurs during this change."* | disruptive |
| Allocated storage | *"Downtime doesn't occur during this change. Performance might be degraded during the change."* | online |
| Provisioned IOPS | *"Downtime doesn't occur during this change."* | online |
| Storage throughput (gp3) | *"Downtime doesn't occur during this change."* | online |
| Storage type (SSD ↔ SSD) | no downtime row; only magnetic conversions carry *"a brief downtime while the process starts"* | online |
| Multi-AZ toggle | *"Downtime doesn't occur during this change."* | online, but out of scope (§2.5) |

[all verified: USER_ModifyInstance.Settings.html]

For Multi-AZ, the mechanism is a failover:

- Failover reason table entry: *"**The RDS instance was modified by customer.** —
  An RDS DB instance modification triggered a failover."*
- *"Failover times are typically 60–120 seconds. However, large transactions or a
  lengthy recovery process can increase failover time."*
- *"The failover mechanism automatically changes the Domain Name System (DNS)
  record of the DB instance to point to the standby DB instance. **As a result,
  you need to re-establish any existing connections to your DB instance.**"* —
  with an explicit warning that a JVM whose `networkaddress.cache.ttl` is
  unbounded *"can't use that resource until you manually restart the JVM."*

  [all verified:
  https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZ.Failover.html]

Operations are applied to the standby first and the primary is then failed over
**[not re-verified: search corroboration of Concepts.MultiAZSingleStandby.html —
confirm the exact sentence before relying on it]**, which is what makes Multi-AZ
resizes *shorter* than Single-AZ ones, not free.

`domain.ActionClass` has four members and none of them describes this.
`ActionStopStart` — *"requires downtime: a plain-EC2 instance resize"* — is the
closest and it understates the blast radius in a way that matters: a stop/start
returns the same endpoint to a client that will retry; a failover changes a DNS
record and drops **every** pooled connection, and whether the application
recovers depends on client-side DNS caching and pool configuration that Kilter
cannot see from any API. Modelling it as `ActionStopStart` would let an executor
account for it with the same disruption budget as an EC2 resize, and
`ActionClass.Disruptive()` feeds exactly that accounting — `pkg/domain`'s own
comment says *"a domain must never understate it."*

That is the load-bearing argument for **declining instance-class actuation**:
not that it is dangerous (Kilter has change windows, approvals and freeze for
dangerous things), but that Kilter has no way to *represent* the danger
truthfully, and inventing a fifth action class to represent one action in one
domain is a core change with a worse cost/benefit than the report it enables.

### 2.8 Licensing and engine edition break the pure price model

§3.4 flagged this and it holds. A `db.r6i.xlarge` running SQL Server Enterprise
license-included and one running PostgreSQL are the same hardware at different
prices, and the reservation rules differ too (size flexibility applies to one and
not the other, §2.6). The current catalog has no place to put this: it is 36 rows
of `{provider, name, family, arch, burstable, milliCPU, memoryBytes, hourlyUSD,
spotHourlyUSD}` with 22 AWS entries and no `db.*` class at all
(`pkg/pricing/catalog.json`). `pkg/ec2` already refuses anything the catalog
cannot price (`unknown-instance-type`, *"no price ⇒ no bill delta ⇒ nothing to
claim"*), which is the right default and means an RDS domain shipped against
today's catalog would refuse **every** instance.

So U11 needs an embedded RDS rate table keyed by
`(class, engine, edition, license model, deployment)` — the `fargate.json`
pattern (`pkg/pricing/fargate.go`), not the `catalog.json` pattern. Sizing that
table is real work and the price axis is wide; the honest v1 is *"ship the
engines whose rates we have, refuse the rest by name."*

### 2.9 Aurora is a third billing model wearing RDS's name

Aurora shares `DescribeDBInstances` and most of the CloudWatch namespace, which
makes it look like one domain. It is not:

- **Aurora Serverless v2 has no instance class to resize.** Capacity is ACUs,
  adjusted in 0.5-ACU steps, *"measured on a per-second basis"*, and it can
  *"scale to zero ACUs with automatic pause and resume."* [verified:
  https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-serverless-v2.html]
  The only levers are the min and max ACU settings — and a min-ACU floor is the
  Aurora analogue of Batch's `minvCpus` (§3.2): a cost that is a configuration
  choice, not a demand signal.
- **Aurora storage is cluster-managed**, not an `AllocatedStorage` the operator
  sets, so §2.3's ratchet analysis does not apply and a different one is needed
  **[unverified: current Aurora storage auto-shrink behaviour]**.
- **Aurora I/O-Optimized** is a cluster-level pricing mode that trades a higher
  instance/storage rate for zero I/O charges — a genuine, quantifiable,
  *reversible* optimization with a documented switching cadence
  **[unverified: rates and the switch-back cooldown]**. It is arguably the
  single highest-value Aurora recommendation and it is nothing like instance
  rightsizing.

Bundling Aurora into an RDS unit would produce a domain that silently applies
provisioned-instance reasoning to serverless clusters. **Aurora gets its own
assessment or its own unit; it does not get folded in.** U11 must detect Aurora
(`Engine` prefix `aurora-`, or a non-empty `DBClusterIdentifier`) and refuse it
by name with a stable code.

### 2.10 What could ever be safe, and what is structurally advisory

**Could be safe (online, reversible, evidence-checkable):**

| Recommendation | Why it can be safe |
|---|---|
| gp3 provisioned-IOPS reduction toward the baseline | *"Downtime doesn't occur"*; reversible upward; measured against `ReadIOPS`+`WriteIOPS` p99 with a floor, exactly `pkg/ebs`'s discipline |
| gp3 provisioned-throughput reduction toward the baseline | same, against `ReadThroughput`+`WriteThroughput` |
| gp2 → gp3 at measured throughput parity | same, subject to the §2.4 refusal band |

**Structurally advisory, permanently:**

| Recommendation | Why it can never auto-apply |
|---|---|
| Instance-class change | failover + DNS + connection-pool reset (§2.7); no action class can represent it honestly |
| Graviton (`db.r6i` → `db.r7g`) | same as §4.5 — portability is unobservable — *plus* total RI stranding (§2.6) |
| Memory-driven downsize | `FreeableMemory` semantics are engine-dependent (§2.2) |
| Replica downsizing | a replica's size is a failover-capacity decision (§2.5) |
| Multi-AZ removal | an availability posture, not a cost setting (§2.5) |
| Allocated-storage reduction | the API cannot do it; blue/green or migrate (§2.3) |
| Idle-instance shutdown | RDS instances are data-bearing; §1 invariant 5 forbids it outright |

**Not representable at all** (must be absent from the write seam, not merely
forbidden — the `pkg/ecs` precedent): `--db-instance-class`,
`--allocated-storage`, `--multi-az`, `--engine-version`, anything that deletes.

---

## 3. AWS Batch

### 3.1 The shape, and where the dollars are

*"There is no additional charge for AWS Batch."* [verified:
https://aws.amazon.com/batch/pricing/] Every dollar is an EC2 instance-hour, a
Fargate task-second, or an EKS pod — substrates that `pkg/ec2`, `pkg/ecs` and
the `k8s-nodes` domain already own or are chartered to own. §3.4's original
reasoning ("covered transitively") is correct at the billing layer and this
assessment does not disturb it.

The structural facts [verified:
https://docs.aws.amazon.com/batch/latest/APIReference/API_ComputeResource.html
and
https://docs.aws.amazon.com/batch/latest/userguide/managed_compute_environments.html]:

- A compute environment is `EC2`, `SPOT`, `FARGATE`, `FARGATE_SPOT` or
  `ECS_MANAGED_INSTANCES`.
- *"AWS Batch creates and manages multiple AWS resources on your behalf and
  within your account, including Amazon EC2 Launch Templates, Amazon EC2 Auto
  Scaling Groups, Amazon EC2 Spot Fleets, and Amazon ECS Clusters. … **Manually
  modifying these AWS Batch-managed resources … can result in unexpected
  behavior, including `INVALID` compute environments, suboptimal instance
  scaling behavior, delayed workload processing, or unexpected costs.**"*
- *"**AWS Batch assumes full control of the compute resources in a managed
  compute environment and can terminate instances, stop tasks, or scale the
  cluster at any time.**"*

### 3.2 An idle compute environment and a correctly-sized one

`desiredvCpus` is *"The desired number of vCPUS in the compute environment. AWS
Batch modifies this value between the minimum and maximum values based on job
queue demand."* The compute environment **is already an autoscaler**. There is
nothing for a rightsizer to size.

The one number that is a standing cost is:

> `minvCpus` — *"The minimum number of vCPUs that a compute environment should
> maintain **(even if the compute environment is `DISABLED`)**."*

That parenthetical is the entire Batch cost finding. `minvCpus > 0` buys
instances that run whether or not any job exists, and it survives disabling the
environment. The difference between an idle CE and a correctly-sized one is
**one integer field**, readable from one API call, priced against the catalog
Kilter already ships.

That is an **insight**, not a domain. It has no target to resize, no histogram
to learn, no plan step to sequence, and one honest caveat Kilter cannot resolve:
`minvCpus` buys job *start latency*, and Kilter has no measurement of how much
that latency is worth to the operator. The correct output is "this floor costs
$X/month; it exists to avoid cold starts; here is how often the queue was empty
during the window" — and then stop.

Two adjacent one-field findings of the same shape:

- `allocationStrategy: BEST_FIT` is the default and the docs describe its own
  cost: *"This allocation strategy keeps costs lower but can limit scaling"* and
  *"`BEST_FIT` isn't supported when updating compute environments"* — so a
  `BEST_FIT` CE cannot receive infrastructure updates at all. Reportable, never
  changeable by Kilter (changing it is an infrastructure update to a resource
  AWS says not to touch outside the Batch API).
- `bidPercentage`: *"The maximum percentage that a Spot Instance price can be
  when compared with the On-Demand price … If you leave this field empty, the
  default value is 100% of the On-Demand price. **For most use cases, we
  recommend leaving this field empty.**"* A non-empty, low `bidPercentage` is a
  common cause of a queue that mysteriously never scales. That is an
  availability insight with a cost consequence, and AWS's own recommendation is
  the remediation.

### 3.3 Spot economics

Batch has real Spot support and picks pools by allocation strategy [verified:
https://docs.aws.amazon.com/batch/latest/userguide/allocation-strategies.html]:

- `SPOT_CAPACITY_OPTIMIZED` — *"Instance types that are less likely to be
  interrupted are preferred."*
- `SPOT_PRICE_CAPACITY_OPTIMIZED` — *"looks at both price and capacity to select
  the Spot Instance pools that are the least likely to be interrupted and have
  the lowest possible price. … **We recommend that you use
  `SPOT_PRICE_CAPACITY_OPTIMIZED` rather than `SPOT_CAPACITY_OPTIMIZED` in most
  instances.**"*
- `BEST_FIT_PROGRESSIVE_ORDERED` and `SPOT_CAPACITY_OPTIMIZED_PRIORITIZED` carry
  AWS's own warning in bold: *"Placing large instance types at the top of the
  list may result in **over-provisioning** for small jobs."*

So the Spot decision Kilter would make — "prefer stable, diversified pools" — is
a decision AWS already makes, better, with fleet telemetry Kilter cannot have.
`landscape.md` §4 item 16 says the same thing about EC2 Spot generally:
*"CAST's 85 % prediction needs fleet telemetry kilter can't have."* On Batch the
gap is wider, because the choice is a single enum whose optimum AWS publishes.

A batch (lower-case) queue is also the *ideal* Spot workload — retryable,
interruption-tolerant, deadline-flexible — which means an `EC2` (on-demand) CE
serving a retry-configured queue is a genuine finding. But it is again **one
enum**, and flipping it is an infrastructure update to a Batch-managed resource.

### 3.4 Why job-level metrics rarely reach a billable conclusion

The rightsizable unit in Batch is the job definition's `vcpus`/`memory`. Three
things stand between measuring it and claiming a dollar:

1. **The measurement is a paid feature.** AWS Batch publishes no per-job CPU or
   memory utilization. Job-level metrics come from **CloudWatch Container
   Insights**, in the `ECS/ContainerInsights` namespace, and `MemoryReserved`
   and `MemoryUtilized` are *collected only for jobs with a defined memory
   reservation in their job definition* [not re-verified: search corroboration
   of
   https://docs.aws.amazon.com/batch/latest/userguide/cloudwatch-container-insights.html].
   §3.4 already scoped `pkg/ecs` to service level *because* per-task detail is
   paid; on ECS that was a v1 simplification, because service-level percentages
   are per-task by construction. **On Batch it is fatal**, because job-level is
   the *only* level.
2. **The evidence gates cannot be satisfied by short jobs.** Every shipped
   domain requires a window and a sample floor before it will claim anything:
   `pkg/ec2` 7 days plus 50 % coverage, `pkg/ebs` 7 days plus 288 samples,
   `pkg/lambda` 24 hours plus 1,000 invocations. A job that runs for four
   minutes produces four 1-minute datapoints. The per-*job* series is
   structurally too short; only a per-*job-definition* aggregate over many runs
   would clear a gate — which is a different data model (many short series
   grouped by definition) than any of the four shipped collectors implements.
3. **The conversion from vCPU-minutes to instance-hours is a packing problem.**
   Suppose a job definition is measured at 40 % memory utilization and its
   reservation is halved. The bill does not fall by half. It falls by whatever
   the allocation strategy's packing decides, over a future queue whose
   composition Kilter cannot observe, against `maxvCpus` — and AWS notes it *"might
   need to exceed `maxvCpus` … never exceeds `maxvCpus` by more than a single
   instance."* Kilter owns a constraint-complete bin-packer (§4.3 uses it for the
   Fargate/EC2 crossover), so this is *simulatable* — but only against a
   hypothetical queue, which makes the output a scenario, not a claim. `pkg/lambda`
   already established the correct handling for exactly this epistemics:
   *"No saving is claimed at a proposed memory setting without a MEASURED
   duration AT that setting."* The Batch analogue would be "no saving claimed
   without a measured instance-hour count at the proposed reservation," and
   nothing publishes that.

### 3.5 The live footgun, which is the actual Batch deliverable

`pkg/ec2` recognizes exactly three ownership tag keys, all Kubernetes
(`pkg/ec2/ec2.go:97-99`): `kubernetes.io/cluster/`, `eks:cluster-name`,
`aws:eks:cluster-name`. `Assessment.Excluded()` (`pkg/ec2/sizer.go:262`) is
`ReasonK8sTagged || ReasonModeOff`. **A Batch managed compute environment's
container instances are not excluded.** They are ordinary EC2 instances in an
ASG that `DescribeInstances` returns, and `pkg/ec2` assesses them as standalone
instances — against resources AWS documents as *"AWS Batch assumes full control
… and can terminate instances, stop tasks, or scale the cluster at any time."*

`pkg/ec2/FINDINGS.md` §5 already names the adjacent gap:
*"`aws:autoscaling:groupName` is **not** currently an exclusion — a follow-up
should decide whether ASG members are suppressed here or routed to a
template-level sizer."*

Today the damage is bounded: U5 is report-only, and most Batch instances are
short-lived enough to fail the 7-day `MinWindow` gate and land in
`insufficient-window`. **But that protection is an accident with exactly the
wrong exception.** The Batch instances that live long enough to clear a 7-day
window are precisely the `minvCpus > 0` floor — instances kept alive by
configuration with an empty queue and therefore near-zero CPU. Those clear every
evidence gate, look unambiguously oversized, and would produce a confident
`stop-start` downsize proposal for U7 to execute. The recommendation would be to
**shrink the idle floor instead of removing it** — a proposal that is wrong in
kind, not just in degree, and that AWS explicitly warns can produce *"unexpected
costs."*

That is the whole Batch verdict: **one suppression and three insights, no
domain.**

---

## 4. What these domains add to §7

Numbered continuing from §7's existing seven. Each states the wrong
recommendation it produces, the fix, and a falsifiable check.

**8. Storage floors that only ratchet up (RDS).** A DB instance holding 300 GiB
of data on 4 TiB of allocated storage looks like a 92 % over-provision. There is
no API that reduces it: *"You can't reduce the amount of storage for a DB
instance after storage has been allocated"* [verified]. Worse, storage
autoscaling raises the floor on its own and *"autoscaling operations aren't
logged by AWS CloudTrail"* [verified], so a Kilter ledger sees the floor move
with no attributable event. *Fix:* allocated storage is a reported fact with a
dollar attached and an explicit "the only remediation is blue/green or
migration" caveat; storage growth between snapshots is unattributed by default.
*Check:* `Report.Validate()` rejects any proposal whose `allocatedStorage` is
below the observed value; a fuzz target re-asserts it.

**9. `FreeableMemory` is `MemAvailable`, and `MemAvailable` counts the page
cache (RDS).** *"For MariaDB, MySQL, Oracle, and PostgreSQL DB instances, this
metric reports the value of the `MemAvailable` field of `/proc/meminfo`"*
[verified]. A PostgreSQL instance whose working set is fully page-cached reports
large freeable memory; downsizing on it evicts the cache and moves cost to I/O
and latency. §7 trap 4 does not catch this — the signal is *present*, so no
memory-blind refusal fires. *Fix:* memory policy is keyed by engine, and an
engine with no encoded semantics refuses (the `t2`-baseline precedent). *Check:*
one fixture, two engines, identical `FreeableMemory` series, and the test
asserts the two verdicts **differ**.

**10. Multi-AZ is a 2× multiplier on the instance line only (RDS).** Pricing a
Multi-AZ instance at its Single-AZ rate halves both its cost and its saving —
and RI coverage is measured in the doubled units (large = 4 Single-AZ, 8
Multi-AZ) [verified]. The multiplier applies to the instance hours and **not**
to storage, backups or I/O [verified: *"doesn't provide a discount for the costs
associated with storage, backups, and I/O"* — the same line separation applies
to the multiplier]. *Fix:* deployment mode is part of the price function, not a
label. *Check:* a Multi-AZ instance's `CurrentHourlyUSD` is exactly twice the
Single-AZ price of the same class, asserted to 1e-12, and a class change moves
both halves.

**11. RDS's storage tables are not EBS's (RDS).** RDS stripes across four
volumes at 400 GiB (200 GiB for Oracle, never for SQL Server); gp2 burst reaches
12,000 rather than 3,000 and throughput reaches 1,000 MiB/s rather than 250; gp3
cannot be provisioned *at all* below the striping threshold [all verified].
Reusing `pkg/ebs/parity.go`'s constants claims a saving in the band where RDS
loses throughput and refuses in the band where RDS converts cleanly. *Fix:* a
separate, engine-keyed table, plus the live envelope from
`DescribeValidDBInstanceModifications`. *Check:* a test that runs a 500 GiB MySQL
volume through both models, asserts they **disagree**, and asserts this one
matches the published RDS table by name.

**12. A resize is a failover, and a failover is a DNS change (RDS).** *"The RDS
instance was modified by customer — An RDS DB instance modification triggered a
failover"*; *"Failover times are typically 60–120 seconds"*; *"you need to
re-establish any existing connections to your DB instance"* [all verified].
`ActionStopStart` accounts for this with the same disruption budget as an EC2
resize, which understates it: an EC2 restart returns the same endpoint, a
failover changes DNS and drops every pooled connection, and client-side DNS TTL
is unobservable from any API. *Fix:* instance-class changes are
`ActionAdvisory`, and the write seam does not contain `--db-instance-class`.
*Check:* reflection over the mutate input struct asserts the field does not
exist (the `TestUpdateServiceCannotChangeDesiredCount` pattern).

**13. Reserved DB Instances are not Reserved Instances (RDS).** Size flexibility
is scoped to the *instance class type* (`db.r6i.large` → `db.r6i.xlarge` yes;
`db.r6id.large` or `db.r7g.large` no) and is **absent** for SQL Server and
Oracle License Included [verified]. Compute Savings Plans do not cover RDS at
all. A domain that reuses `commit.NormalizationUnits` and the EC2 waterfall
would over-report absorption on SQL Server by 100 % and mis-scope every
generation migration. *Fix:* U12 — a distinct commitment product with its own
unit table, its own Multi-AZ multiplier and an engine gate on size flexibility.
*Check:* the same downsize under MySQL and under SQL Server must produce opposite
net-savings outcomes.

**14. A Batch compute environment's idle floor looks exactly like an oversized
instance (Batch).** `minvCpus` maintains instances *"even if the compute
environment is `DISABLED`"* [verified], so a `minvCpus > 0` CE with an empty
queue presents 30 days of near-zero CPU on a long-lived instance — clearing every
evidence gate `pkg/ec2` has. The resulting recommendation shrinks the floor
instead of questioning it, on a resource AWS says it *"can terminate … at any
time"* and warns produces *"unexpected costs"* when touched. *Fix:* U15 — a
`batch-managed` ownership suppression in `pkg/ec2` plus a priced `minvCpus`
insight. *Check:* a fixture instance from a `minvCpus > 0` CE with 30 days of
2 % CPU produces exactly one suppression and **no** proposal.

**15. Batch job-level savings do not convert to instance-hours (Batch).** Job
CPU/memory requires paid Container Insights and is *"collected only for jobs
with a defined memory reservation"* [not re-verified]; per-job series are too
short for any shipped evidence gate; and the reservation → instance-hour
conversion runs through an allocation strategy packing a queue whose future
composition is unobservable. A "cut this job definition's memory 50 %, save
50 %" claim is `pkg/lambda`'s GB-second trap in a different currency.
*Fix:* decline; if ever built, the `pkg/lambda` rule transfers verbatim — no
saving claimed without a measured instance-hour count at the proposed
reservation.

**16. Aurora is not RDS (Aurora).** An Aurora Serverless v2 "instance" has no
class to resize: capacity is ACUs in 0.5 steps, billed per second, able to scale
to zero [verified]. Applying provisioned-instance reasoning to it produces
recommendations about a field that does not control the bill; the actual levers
are min/max ACU and the I/O-Optimized cluster mode. *Fix:* U11 detects Aurora by
`Engine` prefix / cluster membership and refuses by name. *Check:* an Aurora
fixture yields a stable `aurora-not-supported` refusal and zero proposals.

---

## 5. Recommendation, unit-carved

Effort is calibrated to §1.6 and to the §6 scale where U5 (`pkg/ec2`, 3,055 prod
lines) was **M–L** and U9 (`pkg/lambda`, 3,869) was **S**. On that scale U9 was
under-called; the estimates below use §1.6's measurements, not §6's labels.

Units land in the shipped style: `go test -race ./...` green, `gofmt`/`go vet`
clean, no AWS SDK inside `pkg/`, no clock (callers pass `now`), no
package-level mutable state, a shuffle-invariance test, a `Validate()` invariant
checker with a "catches each violation" test, and at least one fuzz target
asserting the load-bearing property.

### U12 — Reserved DB Instances in `pkg/pricing/commit` (**M**) — *prerequisite*

**Owns:** `pkg/pricing/commit/rds.go`, `pkg/pricing/commit/rds_test.go`; plus
one constant block in `commit.go` (`KindRDS`), one clause in `Usage.Validate()`,
and one branch in `waterfall.go`. **Nothing else in the repo.**

Adds a `ReservedDBInstance` product and `RDSNormalizationUnits(size,
deployment)` encoding the verified table (micro 0.5 … 32xlarge 256; Multi-AZ
instance ×2; Multi-AZ cluster ×3). Size flexibility gated on engine. RDS lines
are absorbed by RDS reservations only — never by an EC2 RI, an EC2 Instance SP
or a Compute SP.

*Acceptance:*
- `TestRDSNormalizationTableMatchesPublishedUnits` — all 14 sizes × 3 deployment
  columns, transcribed independently of the implementation (the
  `TestBurstTableMatchesPublishedRates` pattern).
- `TestComputeSPNeverAbsorbsRDS` — a fully-utilized Compute SP leaves an RDS
  line at on-demand; mirrors `TestEBSIsNeverAbsorbedByACommitment`.
- `TestReservedDBInstanceCoversMultiAZAsTwoUnits` — reproduces the doc's own
  example: one Single-AZ large (4 units) ↔ two Multi-AZ mediums (2+2).
- `TestSizeFlexibilityIsEngineGated` — identical downsize, PostgreSQL vs SQL
  Server, opposite outcomes; the SQL Server case strands 100 %.
- `TestClassTypeChangeStrandsEntirely` — a `db.r6i.large` reservation against
  `db.r6id.large` and `db.r7g.large` usage yields zero coverage (AWS's own
  counter-example), and the resulting suppression carries
  `ValidFrom` = reservation end.
- `TestExistingKindsAreUnaffectedByRDS` — every pre-existing
  `pkg/pricing/commit` test passes unchanged; widening the closed set alters no
  EC2/Fargate/Lambda outcome.
- `FuzzRDSWaterfall` — net ≤ gross, finite, order-independent, over arbitrary
  reservation inventories.

### U11 — `pkg/rds`, read-only observation and the refusal report (**M–L**)

**Owns:** `pkg/rds/**` except `parity*.go` and `actuate*.go`. Depends on U12
for any dollar; can be developed against a nil ledger and wired last.

Three read seams shaped after the API (`InventoryAPI`: `DescribeDBInstances`,
`DescribeDBClusters`, `ListTagsForResource`; `MetricsAPI`: `GetMetricData`;
`CommitmentAPI`: `DescribeReservedDBInstances`), a recorded `Fixture`, an
engine-keyed memory policy, deployment-topology pricing, an embedded RDS rate
table in the `fargate.json` style, and a `Report` whose refusals are the
product. Every assessment is `domain.ActionAdvisory`; `PlanSteps` returns
`domain.ErrReportOnly` unconditionally and `Health.ReportOnly` is always true —
the `pkg/lambda` shape, enforced by the core rather than by good manners.

*Acceptance:*
- `TestFreeableMemoryIsNotHeadroom` — trap 9: identical series, PostgreSQL vs
  MySQL, verdicts must differ; an engine with no encoded semantics refuses.
- `TestMultiAZBillsTwice` — trap 10, asserted to 1e-12 against the Single-AZ
  rate.
- `TestStorageFloorNeverProposesShrink` + `FuzzRDSNeverProposesLessStorage` —
  trap 8.
- `TestAutoscaledStorageIsAFloorNotAChoice` — an instance with
  `MaxAllocatedStorage > AllocatedStorage` reports the ratchet, quotes the
  10 %-free/5-minute/4-per-24 h trigger as evidence, and never reads
  `FreeStorageSpace` headroom as a saving.
- `TestIdleInstanceIsReportedNeverResized` — `DatabaseConnections` ≡ 0 across the
  window yields an `idle-instance` advisory carrying the full monthly cost and
  no class proposal; and a *replica* with the same signature is reported
  distinctly.
- `TestSQLServerAndOracleLIAreExactMatchOnly` — trap 13, end to end through the
  U12 ledger.
- `TestAuroraIsRefusedByName` — trap 16.
- `TestNoActuationSurfaceExists` — `*Domain` does not satisfy `domain.Actuator`;
  an identifier scan finds no `ModifyDBInstance` (the `pkg/lambda`
  `TestNoMutatingAPISurface` pattern).
- `TestUnknownEngineOrClassRefusesByName`, `TestEveryAssessmentStatesAReason`,
  `TestReportIsShuffleInvariant`, `TestValidateCatchesEachViolation`,
  `TestCollectorClampsWindowTo15DayRetention`.

### U13 — RDS storage-performance parity, read-only (**M**)

**Owns:** `pkg/rds/parity.go`, `pkg/rds/parity_test.go`, plus one call site U11
reserves for it. **Sequential after U11** (same package); file-disjoint from U12
and U15, which may run concurrently.

The engine-keyed gp2/gp3 regime tables, the striping thresholds, measured-parity
conversion, provisioned IOPS/throughput reduction toward the (non-reducible)
baseline, and the refusal band. Provisioning envelopes read from
`DescribeValidDBInstanceModifications`, refusing when absent rather than
hardcoding the contradictory published ceilings (§2.4).

*Acceptance:*
- `TestRDSGP2ModelIsNotTheEBSModel` — trap 11; the test names both numbers for a
  500 GiB MySQL volume and asserts this one matches the published table.
- `TestStripingThresholdIsEngineDependent` — 400 GiB / 200 GiB / never.
- `TestGP3IsNotProvisionableBelowTheThreshold` — any proposal to provision there
  is a `Validate()` violation.
- `TestGP3ReductionFloorsAtTheBaseline` — 12,000 / 500 is a hard floor in the
  striped regime.
- `TestThroughputParityRefusalBand` — the RDS analogue of `pkg/ebs`'s
  `TestParityRefusalBand`, asserted to fire in a **different** size band, which
  is what proves the tables are not silently shared.
- `TestFourModificationsPer24HoursIsACooldown` and
  `TestStorageOptimizationStateBlocks`.
- `FuzzRDSParityNeverUnderProvisions` — the `pkg/ebs` load-bearing property.

### U14 — RDS storage-performance actuation (**S–M**), behind approval

**Owns:** `pkg/rds/actuate.go`, `pkg/rds/actuate_test.go`. Sequential after U13.

`ModifyDBInstance` restricted to `--iops`, `--storage-throughput` and
`--storage-type`. `ActionInPlace` (verified downtime-free, §2.7), revert-upward,
cooldown, in-flight-modification resume, ledger, `kilter.dev/mode` tag re-read
live at execution. `pkg/ebs/actuate.go` is the template.

*Acceptance:*
- `TestMutateInputCannotChangeClassStorageOrAZ` — reflection: the fields do not
  exist (trap 12's structural fix).
- `TestExecuteRefusesDuringStorageOptimization`,
  `TestExecuteRefusesModifyingState`, `TestExecuteRefusesGuardrails`,
  `TestExecuteRefusesModeOff`.
- `TestDryRunAndApplyAgree`, `TestIdempotencyAcrossRestart`,
  `TestRevertRestoresRecordedFrom`, `TestRevertRefusesToDegrade`,
  `TestApplyRecordsFailures`, `TestContextCancellation`.

### U15 — Batch containment: one suppression, three insights (**S** + **S**)

**U15a owns:** `pkg/ec2/ec2.go` (one constant block), `pkg/ec2/sizer.go` (one
gate in stage 1), `pkg/ec2/fixture.go` + `pkg/ec2/testdata/` (one fixture), and
a new `pkg/ec2/batch_test.go`. Adds a `batch-managed` ownership suppression
beside `k8s-tagged`, on a **verified** selector. Candidates to confirm before
implementing — **[unverified: the exact EC2 tag key AWS Batch applies to managed
compute-environment instances]** — are the Batch-propagated resource tags, the
Batch-created ASG name via `aws:autoscaling:groupName`, and ECS container-instance
membership of the Batch-managed cluster. If none can be verified cheaply, the
correct fallback is the broader decision `pkg/ec2/FINDINGS.md` §5 already
defers: suppress ASG members outright and route them to a template-level sizer.

*Acceptance:*
- `TestBatchManagedInstanceIsSuppressedFiresAlone` — the `fires alone` discipline
  every `pkg/ec2` suppression already follows.
- `TestMinVCpusFloorIsNotReadAsIdleDemand` — trap 14: 30 days of 2 % CPU on a
  long-lived instance from a `minvCpus > 0` CE produces one suppression and no
  proposal. **This test fails on today's tree**, which is the point.

**U15b owns:** `pkg/ec2/batchenrich.go` + tests, and one optional seam
(`batch:DescribeComputeEnvironments`, `batch:DescribeJobQueues`) that
`NewCollector` accepts as nil. Emits three `Advisory`-class findings — priced
`minvCpus` floor, `BEST_FIT` default, non-empty `bidPercentage` — into the
existing EC2 report. **No new `domain.Kind`, no `domain.Domain`, no actuator.**

*Acceptance:*
- `TestMinVCpusIdleFloorIsPricedAsAnInsight` — quotes the monthly cost of the
  floor and carries the caveat that removing it trades cost for job start
  latency, which Kilter does not measure; `Advisory.Actuatable()` returns false.
- `TestBestFitAndBidPercentageAreReportedNeverChanged`.
- `TestBatchEnrichmentIsOptional` — a nil seam degrades the report, never breaks
  it.

### Sequencing and disjointness

```
U12  (pkg/pricing/commit)      ──┐
U15a (pkg/ec2 suppression)     ──┼── concurrent, file-disjoint
U15b (pkg/ec2 enrichment)      ──┘   (U15a → U15b sequential: same package)
                                 │
U11  (pkg/rds core)  ────────────┴──▶ U13 (pkg/rds/parity) ──▶ U14 (pkg/rds/actuate)
```

U12 is the only unit touching a shipped package other than `pkg/ec2`, and the
only one in this line of work since U4 to do so. U11 blocks on U12 for its
dollars, not for its structure — it can be built and tested against a nil ledger
(net == gross) exactly as `pkg/lambda` and `pkg/ebs` do today.

**Total: ~4,500–6,000 production lines and ~4,000–5,000 test lines across five
units** — roughly one wave, and roughly 1.5× the cost of U5 alone.

---

## 6. What would change my mind

Stated as evidence, not as opinion. Each item, if true, flips a specific verdict.

**"Decline RDS instance-class actuation" flips to build if:**
1. `domain.ActionClass` gains a member that honestly represents a failover — one
   whose `Disruptive()` accounting the executor treats as a connection-pool
   reset rather than a restart — *and* it is justified by a second domain that
   needs it, not by RDS alone. A one-domain core change is the wrong trade; a
   two-domain one is not.
2. A Multi-AZ resize is shown, on a real fleet, to complete inside a change
   window with a measured connection-recovery time, and the ledger's
   claimed-vs-measured comparison confirms the saving. §1's whole argument is
   that predictions not grounded in shipped behaviour are worthless; that cuts
   both ways.

**"Build RDS storage-performance rightsizing" (U13/U14) flips to decline if:**
3. The addressable pool is empty. The lever only exists **above** the
   12,000 IOPS / 500 MiB/s striped baseline (§2.4). If a survey of real accounts
   shows that provisioned-above-baseline gp3 is rare — most RDS storage sitting
   below the striping threshold where nothing is provisionable — then U13
   optimizes a tail and U11's report alone is the deliverable. **This is the
   single cheapest check in this document and it should be run before U13
   starts:** one `DescribeDBInstances` pass, count instances where `Iops` or
   `StorageThroughput` exceeds the engine's baseline.
4. RDS gp3 provisioned-IOPS pricing turns out to be low enough that a plausible
   reduction is worth less than `pkg/ebs`'s `MinSavings` floor. The RDS rates
   are **[unverified]** — the pricing tables are JavaScript-rendered and were not
   retrievable this session. Fetch them before committing to the unit.

**"Build U12" flips to defer if:**
5. No target account holds Reserved DB Instances. U12 exists to prevent
   over-claiming; with no reservations, `Ledger` already degrades to net ==
   gross correctly. But note the asymmetry: without U12, `Usage.Validate()`
   *rejects* RDS lines outright, so U11 cannot produce a number at all. Deferring
   U12 means deferring every RDS dollar figure, not just the netted ones.

**"Decline Batch as a domain" flips to build if:**
6. AWS begins publishing per-job CPU/memory in a free namespace, or Container
   Insights becomes default-on for Batch. That removes obstacle 1 of §3.4's
   three; obstacles 2 and 3 remain, so this is necessary and not sufficient.
7. A real fleet is found where Batch spend is dominated by long-running jobs
   (hours, not minutes) under stable job definitions, so per-definition
   aggregates clear a 7-day evidence gate and the reservation→instance-hour
   conversion is near-linear because each job occupies a whole instance. That
   makes obstacles 2 and 3 fall together, and the verdict should flip. **The
   check:** distribution of job durations and jobs-per-instance from
   `batch:ListJobs` over a month.
8. `pkg/ec2` grows an ASG-template sizer (the `pkg/ec2/FINDINGS.md` §5 deferral).
   At that point a Batch compute environment *is* an ASG with a launch template,
   and "Batch support" collapses to "do not size this particular ASG's template;
   report its `minvCpus` instead" — which is U15 restated, but inside a unit that
   would exist anyway.

**"Defer Aurora" flips to build if:**
9. Aurora I/O-Optimized's rates and switch-back cadence are verified and the
   break-even is computable from `VolumeReadIOPs`/`VolumeWriteIOPs` alone. That
   would be a single, reversible, cluster-level recommendation with an exact
   arithmetic answer — structurally the best-shaped recommendation in this entire
   document, and it belongs in its own S-effort unit, not inside U11.

**§1's cost model itself is falsifiable.** It predicts a new CloudWatch-backed
domain costs 3,000–4,100 production lines with zero reuse of
`histogram`/`forecast`/`patterns`/`recommend`/`safety`. If U11 lands materially
under 3,000 lines, or materially reuses any of those five, the model in §1.6 is
wrong and the estimates in §5 should be re-derived from U11's actual shape before
U13 is scheduled.

**One structural recommendation that is independent of every verdict above.**
The CloudWatch seam has now been written four times (§1.4) and would be written a
fifth by U11. `pkg/ecs/FINDINGS.md` §3.8 set the threshold at three. Lifting
`pkg/cloudwatch` — the four types, the 500-series batching, ID-based routing, the
truncation-vs-absence rule, the retention and publication clamps, and their
tests — is an **S** unit that pays for itself once and is owed regardless of what
happens to RDS and Batch. It is deliberately **not** listed as U11–U15 because it
is not part of this decision; it is a debt this decision made visible.

---

## 7. Source table

| Fact | Source (fetched 2026-08-26) |
|---|---|
| RDS 1-minute default publication; 60 s datapoints retained 15 days | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/monitoring-cloudwatch.html |
| `FreeableMemory` = `MemAvailable`; `FreeStorageSpace`, `DatabaseConnections`, `BurstBalance`, `EBSIOBalance%`/`EBSByteBalance%`, `ReadIOPS`/`WriteIOPS`/`ReadThroughput`/`WriteThroughput`, `CPUCredit*` at 5-min only | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/rds-metrics.html |
| Storage cannot be reduced; autoscaling trigger (10 % free / 5 min / storage-optimization complete / < 4 modifications per 24 h); increment rule; autoscaling not logged by CloudTrail; blue-green or migrate to shrink | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIOPS.Autoscaling.html |
| Per-setting downtime table: DB instance class *"Downtime occurs"*; allocated storage / Provisioned IOPS / storage throughput *"Downtime doesn't occur"*; four storage modifications per 24 h; `storage-optimization` blocks | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_ModifyInstance.Settings.html |
| gp3 baselines and striping thresholds (400 / 200 / never); gp3 provisioning ranges; gp2 baseline/burst/throughput tables per engine; striping volume counts; SSD-modification `optimizing` semantics | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/CHAP_Storage.html |
| Reserved DB Instance size flexibility (class type, engine-gated); normalized-unit table incl. Multi-AZ ×2 and Multi-AZ cluster ×3; RI covers instance hours only; cannot cancel | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_WorkingWithReservedDBInstances.html |
| Multi-AZ: synchronous standby, standby cannot serve reads | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZSingleStandby.html |
| Failover reasons incl. *"The RDS instance was modified by customer"*; 60–120 s; DNS record change requires reconnect; JVM DNS TTL caveat | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Concepts.MultiAZ.Failover.html |
| Aurora Serverless v2: 0.5-ACU steps, per-second measurement, scale to zero with pause/resume | https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-serverless-v2.html |
| Batch `minvCpus` *"even if the compute environment is `DISABLED`"*; `desiredvCpus` managed between min and max; `bidPercentage` default 100 % with "leave it empty" recommendation; CE types | https://docs.aws.amazon.com/batch/latest/APIReference/API_ComputeResource.html |
| Batch allocation strategies; `SPOT_PRICE_CAPACITY_OPTIMIZED` recommended; `BEST_FIT` limits scaling and blocks infrastructure updates; over-provisioning warnings on the *_ORDERED/_PRIORITIZED strategies; may exceed `maxvCpus` by one instance | https://docs.aws.amazon.com/batch/latest/userguide/allocation-strategies.html |
| Batch manages ASGs / Spot Fleets / ECS clusters on your behalf; manual modification causes `INVALID` CEs and *"unexpected costs"*; *"assumes full control … can terminate instances … at any time"* | https://docs.aws.amazon.com/batch/latest/userguide/managed_compute_environments.html |
| *"There is no additional charge for AWS Batch."* | https://aws.amazon.com/batch/pricing/ |
| Line counts, import graph, test names, reason codes, `commit.Kind` closed set, `pkg/ec2` ownership tags | this working tree, `wave/j3-rds`, 2026-08-26 |

**[not re-verified]** (documented behaviour reached by search corroboration, not
by reading the primary page this session): Multi-AZ modifications are applied to
the standby first and then failed over; AWS Batch job-level CPU/memory requires
CloudWatch Container Insights in the `ECS/ContainerInsights` namespace and is
collected only for jobs with a defined memory reservation.

**[unverified] — do not build on without confirming:** RDS on-demand instance
rates and Multi-AZ price ratio (the pricing tables are JS-rendered and were not
retrievable); RDS gp3 storage, provisioned-IOPS and provisioned-throughput
rates; RDS gp2 $/GiB-month (the $0.115 figure appears only in a doc example AWS
itself labels *"sample prices"*); the SQL Server gp3 IOPS ceiling (two tables on
the same AWS page disagree: 80,000 vs 64,000/16,000); the RDS default parameter
group's `innodb_buffer_pool_size` formula; Aurora storage auto-shrink behaviour;
Aurora I/O-Optimized rates and switch-back cadence; the EC2 tag key AWS Batch
applies to managed compute-environment instances; the CloudWatch `GetMetricData`
per-metric retrieval charge that applies to every domain Kilter ships.
