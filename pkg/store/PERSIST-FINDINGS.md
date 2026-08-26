# U1 — the write side three units stopped at

Three agents reached the same wall in three packages and each wrote down that
`pkg/store` was out of scope: `pkg/api/WHATIFROUTES-FINDINGS.md` §5.3 (a brain
restart loses every filed proposal and its audit trail),
`pkg/rds/GROWTH-FINDINGS.md` §6 (`SaveRDSCheckpoint`/`LoadRDSCheckpoint` do not
exist), `cmd/WIRING-FINDINGS.md` §6.2/§6.3 (the same write side, named as the
shared prerequisite). Both buckets are built.

```
pkg/store/checkpoint.go       the shared envelope: keying, framing, bounds, typed errors
pkg/store/proposals.go        the `proposals` bucket           (§5.3)
pkg/store/rdscheckpoint.go    the `rds-checkpoints` bucket     (§6)
pkg/store/checkpoint_test.go      30 test functions, package store
pkg/store/persist_ext_test.go      5 test functions, package store_test — real whatif.Store
pkg/store/store.go            +2 buckets in Open's creation list, +1 comment
```

`gofmt -l ./pkg/store` empty, `go vet ./...`, `go build ./...`,
`go test -race -count=1 ./pkg/store/...` and `go test -race -short ./...` all
green. **`go.mod` and `go.sum` are byte-identical** — `shasum` before and after
matches, and the only imports added are stdlib (`compress/gzip`, `errors`,
`unicode`) plus, in the external test package alone,
`pkg/whatif` and `pkg/backtest`. Coverage for `pkg/store` is **90.0 %** (was
88.9 %). No existing test file was touched and no existing assertion changed.

---

## 1. The key encoding, and why a collision has no representation

The trap named in the brief is real and it is the whole design: **the evidence
precedent is not scope-keyed.** `SaveEvidenceCheckpoint` stores one blob under
a constant, so it has no key to get wrong. §6 needs one per scope, and two
scopes that collapse into one key serve one region's storage history as
another's.

**The key is the scope's bytes and nothing else.** No escape, no prefix, no
hash, no composition. `TestScopeKeysAreTheScopeBytes` pins that over the raw
bbolt keys, so a later change that introduces an encoding fails here and has to
re-argue the property rather than inherit it.

That is not the obvious choice — the two neighbouring buckets don't do it — and
the contrast is the argument. `bucketPlans` and `bucketSnapHistory` key on
`"<cluster>/<timestamp>"`, a genuinely composite key, and they pay for it:
`parsePlanTime`/`parseSnapTime` exist because an EKS ARN contains `/`, and
scanning cluster `a/b` from prefix `a/` would otherwise sweep up a sibling's
rows. That cost is worth paying when a key really has two components. A
checkpoint key has one. Composing it with anything — a kind tag, a version, a
timestamp — would import the entire ambiguity class for no benefit:
`"prod"` + `"us-east-1"` would become indistinguishable from
`"prod/us-east-1"`. **A one-component key cannot be parsed wrong because it is
never parsed.**

`TestScopesContainingTheSeparatorCannotCollide` files eight scopes chosen to
break a composite scheme — `prod`, `us-east-1`, `prod/us-east-1`,
`prod/us-east-1/extra`, `prod/`, `/us-east-1`, `//`, `prod//us-east-1` — and
asserts eight distinct keys and eight distinct payloads read back.
`TestUnicodeScopesAreDistinctKeys` does the same for `café` composed vs
decomposed (they are two scopes, and merging them would be exactly the silent
account merge this bucket refuses to have), `東京/prod` and `prod-🌍`.

Injectivity of the identity map is not an argument that needs making, which is
the point of choosing it. What still needs making is the argument about what
gets in:

**`validateScope` refuses, at the door, four kinds of scope** — the reasoning
is `validateClusterID`'s, one step further, because a scope is used two ways at
once (raw as the bbolt key, JSON-encoded inside the envelope):

| refused | why | test |
|---|---|---|
| empty | bbolt has no empty key; the failure would otherwise surface as `ErrKeyRequired` from three frames down | `TestUnstorableScopesAreRefusedAtTheDoor/empty` |
| over `MaxScopeBytes` = 512 | a 10 KiB scope is a caller passing a document as an identifier | `/10_KiB`, `/one_over_the_cap` |
| not valid UTF-8 | `encoding/json` rewrites it to U+FFFD, so the envelope's own `scope` would stop matching the key it lives under — an unreadable write | `/not_utf-8` |
| control characters | this string is echoed into error messages, log lines and `bbolt` CLI dumps; a scope carrying `\n` forges a log line (`whatif.sanitizeNote`'s reason, not an encoding one) | `/newline`, `/NUL` |

Each of those also asserts that nothing was written under any key. A scope of
exactly 512 bytes stores: the boundary belongs to the legal side.

### 1.1 The second defence, which does not depend on the first

The envelope repeats the scope, and the reader refuses a record whose stored
scope is not the one asked for. This is the identity check `LoadSnapshot` and
`LatestPlan` already make against `ClusterID`. For records this package wrote it
is unreachable. It fires for a record moved between keys by anything else — a
restore from a mis-transcribed backup, a hand-edit, a bug in a future migration
— and it is what turns "served under the wrong scope" from unlikely into
*impossible*: `TestARecordMovedToAnotherKeyIsRefused` copies `eu-west-1`'s
value under `us-east-1` and asserts the load fails naming `eu-west-1`, rather
than handing back a growth history for a database in another region.

The envelope also carries its `kind`, so a `proposals` envelope filed in the
rds bucket is refused instead of being handed to `rds.Domain.Restore`
(`TestACheckpointFromTheOtherBucketIsRefused`).

The first-order consequence of a collision is worth stating because it is the
reason this got two defences rather than one. GROWTH-FINDINGS §7 already
observes that a cross-scope restore "produces a decrease", and a decrease is
refused by `storage-growth-history-inconsistent`. So the *common* collision
self-reports. The dangerous one is the collision that looks consistent: two
same-named databases in two regions whose histories interleave into a plausible
upward slope. That produces a growth verdict, with a rate and a projection, for
a database that never grew — a wrong answer delivered confidently, which is the
failure mode this whole repo is organised against.

---

## 2. The size ceiling, with its arithmetic, at write and at read

Both caps are applied **twice**: on the caller's bytes before compressing, and
on the decompressed output at read. The read-side bound is not redundancy. A
stored value never passed through `Save` — it can arrive from a backup, an
edit, or a future bug — and a few hundred bytes of gzip in a bbolt value expand
without limit. A decompression bomb in a bbolt value is still a decompression
bomb, and this process is meant to run unattended for months.

### 2.1 `MaxProposalsCheckpointBytes = 64 MiB`

Chosen from what `pkg/whatif` *can* emit, not from what it emits today. Per
record, at every free-text field's limit:

```
evidence citations   maxEvidenceIDs × maxEvidenceIDLen = 64 × 512 =  32,768 B
rationale                                                            2,048 B
human notes on the longest path draft→gated→approved→applied
                                          2 × maxNote =               4,096 B
the approval's own note                                              2,048 B
structure: two full policies, gate verdict, delta, keys, indent  ≈   6,366 B
                                                                 ------------
                                                                 ≈  47,326 B  ≈ 46 KiB
× maxRecords (1000)                                              ≈  45.1 MiB
```

`TestOneRecordIsWellUnderItsShareOfTheCap` builds that record for real and
measures **45,278 bytes** (the table's figure plus the one further note the
longest path adds); it asserts `perRecord × 1000 ≤ cap` so that a later change
to `pkg/whatif` that grows a record past its share fails *here*, in this
package, rather than at 3am in a brain whose proposals silently stopped
persisting. 64 MiB leaves ~40 % headroom and matches
`MaxEvidenceCheckpointBytes`, chosen for the constraint that binds here too: a
bbolt value is materialized whole on both sides of a write.

One correction to the arithmetic anyone else would derive from reading
`pkg/whatif`: **`maxRationale = 4096` never binds.** `Spec.normalize` checks it
and then calls `sanitizeNote`, whose `maxNote = 2048` refuses first. The
effective rationale bound is 2,048 bytes. That is `pkg/whatif`'s business, not
a defect this unit may fix, but a cap derived from the larger constant would be
overstated by 2 KiB per record.

**What 64 MiB does not cover, stated rather than left to be discovered.**
`encoding/json` escapes `<`, `>` and `&` to six bytes each, and
`sanitizeNote` permits all three. A store whose every citation, rationale and
note is `<` frames to roughly **240 MiB** and is refused at write. That is
deliberate and it is the evidence precedent's choice — refuse loudly and name
the cap, rather than write a value that cannot be read back within a sane
allocation. The failure direction is the safe one: the in-memory store keeps
working, nothing is corrupted, and a lost proposal is not an approved one. A
realistic record is ~6.5 KiB — the 6,366 B of structure above plus short
prose — so a realistic full store is ~6.5 MiB and this is ~10× production
headroom.

### 2.2 `MaxRDSCheckpointBytes = 64 MiB`, `MaxRDSCheckpointScopes = 64`

GROWTH-FINDINGS §7's own numbers, taken at its stated bound:

```
DefaultGrowthMaxObservations 768 × ~40 B  =  30,720 B per instance
× 1,000 instances (§7's worst realistic account)  ≈  29.3 MiB of history
× 2 for the rest of each Target and repeated JSON keys ≈  58.6 MiB
                                                  cap =  64 MiB
```

`[unverified: no 1,000-instance account has been measured; §7 flags the same
figure as unverified and this cap inherits that.]` The refusal names
`Config.Growth.MaxObservations` as the knob, which is §7's own answer.

The scope count cap is the bound §6 did not ask for and needs. **A scope-keyed
bucket with no cap on scopes is not a ceiling, it is a growth curve**: a caller
deriving a scope from something accidentally per-run — a session id, a
hostname, a timestamp — writes a new key every tick until the disk fills. 64
scopes × 64 MiB is a 4 GiB absolute ceiling, and at the ~10× compression this
framing gets `[unverified for RDS checkpoints; the ratio is history.go's, for
snapshot JSON]` a realistic full fleet is a few hundred MiB on disk.

The cap falls on the **newcomer**: past 64 scopes a new scope is refused by
name while every existing scope keeps updating. A cap that stopped the fleet
already being observed correctly would turn one mis-derived scope into a total
outage of the growth finding. `DeleteRDSCheckpoint` is how a slot is reclaimed,
because otherwise the cap is a one-way door — a renamed account would hold its
slot forever. Both halves are in
`TestScopeCapRefusesNewScopesAndKeepsUpdatingOldOnes`.

### 2.3 The third bound: the stored record

`maxStored() = max + max/2 + 4096` bounds the *compressed* value, checked
before it is copied out of the mmap. gzip of incompressible input can exceed
its input and `encoding/json` base64s the result, so a legitimate record can
reach ~4/3 of the decoded cap; 3/2 plus the envelope's own bytes is that with
room. Without it, a hostile value is fully copied before anything looks at it
(`TestOversizeStoredRecordIsRefusedBeforeItIsCopied`).

---

## 3. Missing, future-version and corrupt are three facts, and they arrive as three errors

> "Nothing was found" and "something was found and I could not read it" must
> never collapse: the second is a bug report, the first is Tuesday.

**Every load returns an error rather than a nil blob.** The caller's rule is one
line: `errors.Is(err, ErrNoCheckpoint)` means start empty; *any* other error
means stop and say so. `pkg/api` already writes exactly this switch for
`evidence.ErrNoCheckpoint` in `restoreEvidence`, so the shape is not new to the
consumer.

| sentinel | the fact | the remedy it protects | test |
|---|---|---|---|
| `ErrNoCheckpoint` | the bucket exists, nothing is stored under this scope | start empty — this is a first boot, or a scope observed for the first time | `TestColdScopeIsNotAnEmptyBlob` |
| `ErrCheckpointBucket` | the bucket itself is gone. `Open` creates every bucket, so this is not a cold start: the file was truncated, hand-edited, or written by something that is not this program | do not wipe and reinitialise — there is other state in that file | `TestMissingBucketIsNotAColdStart` |
| `ErrFutureCheckpoint` | the envelope's version is above this build's | **leave it alone.** Discarding it destroys a newer build's state the moment someone rolls back — the opposite remedy to corruption, which is why it cannot share an error with it | `TestFutureVersionIsRefusedByNameAndIsNotCorruption` |
| `ErrCorruptCheckpoint` | a record is present and unreadable: not an envelope, wrong kind, wrong scope, unknown codec, broken or truncated gzip, version *below* this build's | a bug report | `TestCorruptRecordIsNotAColdStart`, `TestBrokenGzipIsCorruptNotEmpty`, `TestUnknownCheckpointCodecIsRefusedByName`, `TestEnvelopeWithNoBlobIsRefused` |
| `ErrCheckpointTooLarge` | over a cap, at write or at read | a configuration change in the producing package | §2's tests |
| `ErrInvalidScope` | §1's table | fix the caller | `TestUnstorableScopesAreRefusedAtTheDoor` |
| `ErrEmptyCheckpoint` | a zero-length blob at write | — | `TestEmptyCheckpointIsRefusedAtWrite` |

Each test asserts the *negative* as well: a future envelope must not also match
`ErrCorruptCheckpoint`, a corrupt record must not match `ErrNoCheckpoint`, a
missing bucket must not collapse into a cold start.

Two decisions inside that table are worth their own sentence:

- **A version *below* this build's is corruption, not compatibility.** The
  evidence precedent reads two codecs because it had a predecessor to be
  compatible with. This framing has never written another layout, so there is
  no older form to accept and guessing at one would be inventing it. The error
  says so.
- **An envelope carrying no blob is refused at read**, not returned as an empty
  checkpoint. Combined with the write-side refusal of an empty blob, "a
  checkpoint that exists and is empty" has no representation at either end —
  which matters because it would be a *fourth* meaning competing with the three
  above.

`CheckpointError` carries `Kind`, `Scope` and `Detail` alongside the sentinel,
so a caller logs the scope with `errors.As` instead of parsing it back out of a
message.

---

## 4. Concurrency

The precedent's contract, matched exactly: `Store` is "safe for concurrent
use", bbolt serializes writers and snapshots readers, and these methods hold no
state of their own — no new mutex, and none needed. The one read-modify-write
is the scope cap, and it runs **inside** the write transaction, where bbolt has
already serialized writers, so two concurrent saves cannot both observe the
last free slot and both take it.

`TestConcurrentCheckpointAccess` runs eight goroutines × 25 iterations under
`-race`, writing four scopes plus one contended scope, reading it back, and
driving both buckets at once. The assertion that matters is not "no data race"
— it is that a reader never sees a blend of two writes: every read of the
contended scope must equal one of the eight known payloads exactly.

Framing is deterministic (`TestFramingIsDeterministic`): gzip with default
settings and no header fields, so identical state produces identical stored
bytes. `encodeSnapshot` relies on the same property, and it is what makes a
housekeeping tick with nothing to report a no-op write rather than a diff
against itself.

---

## 5. The exact call sequence, for each of the three waiting consumers

### 5.1 `pkg/api` — proposals (§5.3)

**Where the load goes.** `newWhatIfSurface` (`pkg/api/whatifroutes.go:134`)
does `props: whatif.NewStore()`. That becomes:

```go
func newWhatIfSurface(b *Brain) (*whatIfSurface, error) {
	props := whatif.NewStore()
	if b.st != nil {
		blob, err := b.st.LoadProposals()
		switch {
		case errors.Is(err, store.ErrNoCheckpoint):
			// First boot. Tuesday.
		case err != nil:
			// A proposal plane that exists and cannot be read is a bug report,
			// not an empty plane. Do not start over it.
			return nil, fmt.Errorf("api: restore proposal store: %w", err)
		default:
			loaded, err := whatif.Load(blob)
			if err != nil {
				return nil, fmt.Errorf("api: restore proposal store: %w", err)
			}
			props = loaded
		}
	}
	return &whatIfSurface{b: b, props: props, now: func() time.Time { return time.Now().UTC() }}, nil
}
```

**Where the tick hangs.** §5.3 says "the same housekeeping timer that would call
`store.Sweep(clock)`". Stated precisely, because it is not quite there yet:
**`pkg/api` has no housekeeping timer today** — `grep -n 'time.NewTicker'
pkg/api/*.go` is empty. What it has instead is a checkpoint *cadence*, and that
is the right hook, because it is already the place the brain decides it is time
to write learned state to disk:

```go
// brain.go:271, inside Ingest — beside the existing recommender/evidence writes:
if b.st != nil && count%b.cfg.CheckpointEvery == 0 {
	_ = b.st.SaveRecommenderState(snap.ClusterID, r.Checkpoint())
	b.saveEvidence()
	b.saveProposals()          // ← add
}

// brain.go:563, inside Serve's shutdown block, beside b.saveEvidence():
b.saveProposals()              // ← add

// substrate.go, beside saveEvidence, same error policy (logged, not returned:
// losing a checkpoint costs history, failing an ingest costs the observation):
func (b *Brain) saveProposals() {
	if b.st == nil || b.whatif == nil {
		return
	}
	// Sweep FIRST: snapshotting before sweeping persists a store one tick
	// staler than the one in memory, so a lapsed approval would be written
	// back as still-approved and only expire after the next restart+tick.
	if _, err := b.whatif.props.Sweep(b.whatif.now); err != nil {
		b.cfg.Logger.Error("sweep proposals", "err", err)
	}
	if err := b.st.SaveProposalsFrom(b.whatif.props); err != nil {
		b.cfg.Logger.Error("persist proposals", "err", err)
	}
}
```

`SaveProposalsFrom` takes `store.ProposalSnapshotter`, which `*whatif.Store`
satisfies structurally — `persist_ext_test.go` asserts that at compile time, so
a signature drift in `pkg/whatif` breaks *this* package's test build rather
than `pkg/api`'s. `TestHousekeepingTickShape` runs the sweep-then-save loop
above across a TTL boundary and asserts a tick with nothing to sweep does not
rewrite the snapshot, and that a tick that expires the live approval does reach
the store.

**One blocker this unit found and cannot fix, because `pkg/api` is not its
scope.** `registerWhatIfRoutes` is `newWhatIfSurface(b).register(mux)` — it
drops the pointer on the floor. Nothing on `Brain` can reach the proposal
store, so `saveProposals` above has no `b.whatif` to read. The surface has to
be held on `Brain` and built once in `NewBrain`, not per `Handler()` call.
That also settles §5.3's open question: with a bucket behind it, two handlers
built from one brain must not be two independent stores that alternately
overwrite each other's snapshot.

### 5.2 `pkg/rds` + `cmd/` — the growth checkpoint (§6)

GROWTH-FINDINGS §6's snippet, with the two missing calls filled in and the cold
start handled the way §3 requires:

```go
d, _ := krds.NewDomain(sc)

blob, err := st.LoadRDSCheckpoint(scope)          // ← the missing read side
switch {
case errors.Is(err, store.ErrNoCheckpoint):
	// No history yet. Every instance reports GrowthNoHistory with its reason
	// filled — §6's intended cold start.
case err != nil:
	// A checkpoint exists and could not be read. Proceeding here silently
	// restarts a 14-day observation window: the growth finding would simply
	// never appear and no one would know why.
	return fmt.Errorf("rds: restore growth history for %s: %w", scope, err)
default:
	if err := d.Restore(blob); err != nil {
		return fmt.Errorf("rds: restore growth history for %s: %w", scope, err)
	}
}

snap, err := collector.Collect(ctx)
rds.RecordStorageHistory(snap, d.StorageHistories(), sc.Growth)
_ = d.Observe(snap)

blob, err = d.Checkpoint()
if err != nil {
	return err
}
if err := st.SaveRDSCheckpoint(scope, blob); err != nil {   // ← the missing write side
	return err
}
```

`scope` is `cmd/`'s to derive and this package does not parse it —
`"<account>/<region>"` is the obvious value and §1 shows that a `/` in it is
inert. Two rules for whoever picks it: it must be **stable across runs** (a
scope derived from anything per-run spends a slot per tick and hits
`MaxRDSCheckpointScopes`), and it must be **as specific as the data** (one
scope per account *and* region; collapsing regions is precisely the merge §1
exists to prevent, and this package cannot detect it because both halves would
be legitimate writes under the same key).

`RDSCheckpointScopes()` lists what is stored, in key order, so an operator can
see what the file holds and which slots the cap is spending.

### 5.3 `cmd/WIRING-FINDINGS.md` §6.2/§6.3

§6.2's blocker — a time-keyed snapshot bucket — was closed by an earlier unit
(`SaveSnapshotAt`/`Snapshots` in `history.go`). §6.3's remaining blocker is the
evidence substrate on `api.Brain`, and `SaveEvidenceCheckpoint` already exists.
Neither needs anything from this unit; §6.3's "that evidence store is the same
prerequisite §6.2 needs" is now satisfied on both sides, and what is left in
both is `pkg/api` work.

---

## 6. Persisting a proposal is not approving one

`pkg/store` does not import `pkg/whatif`, does not parse a record, and has no
method that transitions one. The bucket is a **byte pipe**, and that is a
security property, not laziness: `whatif.Record.UnmarshalJSON` is the second
lock on the door, recomputing the content fingerprint and re-verifying that an
approved record carries a live approval bound to that fingerprint and that
verdict by a human who is not the author. Splitting any of that across a
persistence layer would be the way to lose it.

`TestAForgedApprovalDoesNotComeBackApproved` hand-forges four blobs from a real
snapshot and asserts a pair of properties for each:

1. the pipe hands back **exactly** what it was given — no repair, no rewriting,
   no laundering; and
2. what it was given **fails to load**. Not "loads without the approval", not
   "loads as gated" — does not load.

The four: a gated record relabelled `"state": "approved"`; an approved record
whose approval object is deleted while its state is kept; an approver rewritten
to be the author (self-approval by edit); a gate verdict flipped from `false`
to `true`. Each fails at a different one of `pkg/whatif`'s checks, which is the
useful part — the refusals are independent, so no single edit clears them.

`TestProposalStoreRoundTripsBitExactWithEveryGateState` is the positive half: a
store holding one record in **every state a stored record can rest in** —
gate-rejected, human-rejected, gated, approved-and-live, expired, applied —
round-trips byte for byte, reloads, and comes back in exactly those six states.
A restart may not promote anything. (`StateDraft` is absent and cannot be
produced: `whatif` refuses a stored draft, because no record rests there.)

`TestNothingHereApprovesOrApplies` states the prohibition over this package's
own AST: no exported or unexported function whose name contains `approve`,
`apply`, `actuate`, `execute`, `resize`, `reboot` or `modify`, and no import of
`pkg/ec2`, `pkg/rds` or `pkg/whatif`. A persistence layer is exactly where that
line erodes first — a `SaveApproval`, a `MarkApplied` convenience, an import of
the state machine so a record could be "repaired" on load — and the test fails
if one appears rather than the claim rotting into a comment.

The residual risk, stated plainly: **the worst these buckets can do is lose a
checkpoint or refuse to load one.** They cannot manufacture an approval, and
nothing downstream of them makes one executable — `pkg/ec2/actuate*.go` and
`pkg/rds/actuate*.go` remain unreachable from the binary and this unit touched
neither.

---

## 7. What I did NOT build

- **No consumer wiring.** Not one line outside `pkg/store/`. §5's sequences are
  written to be pasted, and `TestHousekeepingTickShape` executes the proposals
  one so the documented sequence is one that compiles and runs — but
  `pkg/api`, `pkg/rds` and `cmd/` are other people's, and the surface-on-`Brain`
  blocker in §5.1 is `pkg/api`'s to fix.
- **No `proposals` scope key.** The bucket stores one value under a constant,
  like `bucketEvidence`, because a `whatif.Store` is fleet-wide — its proposals
  carry their own target cluster and `GET /api/v1/proposals?cluster=` filters
  one store rather than selecting between several. A scope here would be a
  parameter every caller had to invent a value for. If a later unit runs
  per-cluster proposal stores, the scope-keyed machinery is already in
  `checkpoint.go` and the change is five constants.
- **No second codec, and no migration path.** Only `gzip` is written and only
  `gzip` is read. The evidence envelope reads a plain-JSON codec because it had
  a predecessor; this one has none, and shipping an unused codec would be
  speculative compatibility with a format that has never existed. The `codec`
  and `version` fields exist so the *next* one is refused by name.
- **No retention or pruning.** Both buckets are last-write-wins, one value per
  key. There is no history of checkpoints, so there is nothing to prune —
  unlike `history.go`, whose whole subject is retention. The only unbounded
  dimension is the scope count, and §2.2 bounds it.
- **No compaction, and no delete for proposals.** `DeleteRDSCheckpoint` exists
  because `MaxRDSCheckpointScopes` would otherwise be a one-way door. Proposals
  have a single key that is always overwritten, so a delete would only be a way
  to throw away an audit trail.
- **No `context.Context` on these calls.** `EvidenceCheckpointStore` takes one
  because `evidence.CheckpointStore` demands it, and its own doc comment says
  what it is worth: a bbolt write is not cancellable once begun. Neither §5.3
  nor §6 named an interface requiring one, so adding a parameter that can only
  be checked before the work starts would be a lie about what an interrupted
  checkpoint leaves behind. If a consumer needs the seam, copy
  `EvidenceCheckpointStore`.
- **The proposals cap is not enforced against `whatif`'s record count.** This
  package cannot count records in an opaque blob, and parsing one to check
  would undo §6. `whatif.Load` enforces `maxRecords` itself on the way back in;
  the byte cap here is the independent, cheaper bound.
- **No fuzz targets.** `fuzz_test.go` fuzzes the plan-key encoding because that
  encoding is where the ambiguity lives. The checkpoint key has no encoding to
  fuzz — §1 is the reason — and the envelope decoder's inputs are covered by
  the hand-built adversarial records in `checkpoint_test.go`. A fuzzer over
  `decode` would be reasonable future work; it was not the best use of the
  remaining budget against writing the forged-blob tests.
- **`[unverified]` claims carried forward**, both from GROWTH-FINDINGS §7 and
  both marked at their use site: the ~30 KiB-per-instance checkpoint size (no
  1,000-instance account has been measured) and the ~10× compression ratio (the
  measured figure is `history.go`'s, for snapshot JSON, not for RDS
  checkpoints).
