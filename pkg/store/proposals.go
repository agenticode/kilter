package store

// The proposals bucket — pkg/api/WHATIFROUTES-FINDINGS.md §5.3's "a brain
// restart loses every filed proposal and its audit trail".
//
// whatif.Store.Snapshot() and whatif.Load() already move the whole proposal
// store as bytes; §5.3 said where those bytes go and that pkg/store was out of
// scope that cycle. This is that bucket. It stores ONE value, like
// bucketEvidence and for the same reason: a whatif.Store is fleet-wide (its
// proposals carry their own target cluster, and `GET /api/v1/proposals?cluster=`
// filters the one store rather than selecting between several), so a scope key
// here would be a parameter every caller had to invent a value for.
//
// # Persisting a proposal is not approving one
//
// The blob is opaque to this package and stays opaque. pkg/store does not
// import pkg/whatif, does not parse a record, and has no method that
// transitions one — the state machine is pkg/whatif's security property and
// splitting it across a persistence layer would be the way to lose it.
//
// What that buys is precise: this bucket is a byte pipe, so a proposal comes
// back in exactly the gate state it was written in, and a blob that arrives
// from anywhere else — an edited file, a truncated write, an attacker with the
// db — is still checked by whatif.Record.UnmarshalJSON, which recomputes the
// content fingerprint and re-verifies that an approved record carries a live
// approval bound to that fingerprint and that verdict by a human who is not
// the author. A forged "state":"approved" fails to load; it does not load as
// approved. persist_ext_test.go forges one and asserts exactly that.
//
// The direction of the remaining risk is worth stating: the worst this bucket
// can do is lose proposals or refuse to load them. It cannot manufacture an
// approval, and nothing here or downstream of here makes one executable —
// pkg/ec2 and pkg/rds's actuators remain unreachable from the binary.

// bucketProposals holds the single framed whatif.Store snapshot.
var bucketProposals = []byte("proposals")

// proposalsScope is the key the fleet-wide proposal store is filed under.
// A constant, for evidenceScope's reason.
const proposalsScope = "brain"

// ProposalsEnvelopeVersion is the framing version this build writes. It is
// independent of whatif's own storeVersion, which travels inside the blob: the
// question this version answers is "how is the blob framed", not "what is in
// it".
const ProposalsEnvelopeVersion = 1

// MaxProposalsCheckpointBytes bounds the DECODED snapshot, at write and again
// at read.
//
// The arithmetic is taken from pkg/whatif's own bounds rather than from a
// measurement of today's data — a cap chosen from what a producer CAN emit
// survives the producer getting busier. One record's free text is bounded by
// maxEvidenceIDs × maxEvidenceIDLen (64 × 512 = 32,768 B), the rationale
// (2,048 B: Spec.normalize checks maxRationale = 4096 and then runs
// sanitizeNote, whose maxNote = 2048 refuses first, so the larger constant
// never binds), and one note per human-supplied transition over the longest
// legal path draft→gated→approved→applied (2 × maxNote) plus the approval's
// own note — 6,144 B. TestOneRecordIsWellUnderItsShareOfTheCap builds exactly
// that record and measures 45,278 B, which with the one further note the
// longest path adds is ~47,326 B, call it 46 KiB. whatif caps itself at
// maxRecords = 1000:
//
//	46 KiB × 1,000 ≈ 45.1 MiB
//
// 64 MiB covers that with ~40% headroom, and matches
// MaxEvidenceCheckpointBytes, which was chosen for the constraint that binds
// here too: a bbolt value is materialized whole on both sides of a write.
//
// What 64 MiB does NOT cover, stated rather than left to be discovered:
// encoding/json escapes '<', '>' and '&' to six bytes each, so a store whose
// every rationale, citation and note is '<' frames to roughly 240 MiB and is
// REFUSED AT WRITE. That is deliberate, and it is the evidence precedent's
// choice — refuse loudly and name the cap, rather than write a value that
// cannot be read back within a sane allocation. The failure is safe in the
// only direction that matters: the in-memory store keeps working, nothing is
// corrupted, and a lost proposal is not an approved one. A realistic record is
// ~6.5 KiB — the structure above plus short prose — so a realistic full store
// is ~6.5 MiB and this is ~10× production headroom.
const MaxProposalsCheckpointBytes = 64 << 20

var proposalsKind = checkpointKind{
	name:       "proposals",
	bucket:     bucketProposals,
	version:    ProposalsEnvelopeVersion,
	max:        MaxProposalsCheckpointBytes,
	fixedScope: proposalsScope,
}

// SaveProposals stores the proposal store's bytes, replacing the previous
// snapshot. blob is whatever whatif.Store.Snapshot() returned; this package
// does not look inside it.
//
// Callers hang this off the housekeeping timer, immediately after the
// whatif.Store.Sweep that moves lapsed approvals to expired — sweeping after
// snapshotting would persist a store one tick staler than the one in memory.
// PERSIST-FINDINGS.md §5 has the loop.
func (s *Store) SaveProposals(blob []byte) error {
	return proposalsKind.save(s, "", blob)
}

// LoadProposals returns the stored bytes for whatif.Load.
//
// A cold store is an ERROR — errors.Is(err, ErrNoCheckpoint) — not a nil blob.
// The distinction is the point: "no proposals have ever been filed" and "there
// were proposals and I cannot read them" must not arrive in the same shape,
// because a caller that treats the second as the first starts an empty store
// over an audit trail it just failed to read.
func (s *Store) LoadProposals() ([]byte, error) {
	return proposalsKind.load(s, "")
}

// ProposalSnapshotter is the seam whatif.Store satisfies. It is declared here,
// structurally, so pkg/store keeps its place at the bottom of the dependency
// order and never imports the state machine — the same arrangement
// EvidenceCheckpointStore has with pkg/evidence. persist_ext_test.go asserts
// the satisfaction at compile time, so a signature drift in pkg/whatif breaks
// this package's test build rather than some caller's.
type ProposalSnapshotter interface {
	Snapshot() ([]byte, error)
}

// SaveProposalsFrom snapshots and stores in one call, which is the whole of
// what a housekeeping tick needs to do. A snapshot that cannot be produced is
// returned unwrapped in a CheckpointError, because the failure is the
// producer's, not this bucket's.
func (s *Store) SaveProposalsFrom(p ProposalSnapshotter) error {
	if p == nil {
		return proposalsKind.errf(proposalsScope, ErrEmptyCheckpoint, "nil snapshotter")
	}
	blob, err := p.Snapshot()
	if err != nil {
		return proposalsKind.errf(proposalsScope, ErrCorruptCheckpoint, "snapshotting: %v", err)
	}
	return s.SaveProposals(blob)
}
