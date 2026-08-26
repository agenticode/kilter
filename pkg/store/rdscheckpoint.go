package store

// The RDS checkpoint bucket — pkg/rds/GROWTH-FINDINGS.md §6's
// "SaveRDSCheckpoint/LoadRDSCheckpoint do not exist", and the persistence
// cmd/WIRING-FINDINGS.md §6.2/§6.3 named as the shared prerequisite.
//
// §6 asked for the shape SaveEvidenceCheckpoint has — "a scope-keyed,
// gzip-framed, version-refusing envelope" — and the one word in that sentence
// the precedent does not actually implement is the first. Evidence stores one
// blob for the brain; an RDS checkpoint is per scope, because
// Target.StorageHistory is the state and a target only means anything inside
// the account and region it was collected from. checkpoint.go is the
// generalization; this file is its five parameters.
//
// # What a scope is, and what happens if two of them collapse
//
// A scope is the caller's name for the collection boundary — in practice
// "<account>/<region>", whatever cmd/ derives from the credentials and the
// endpoint it queried. pkg/store does not parse it and does not need to: the
// key is the scope's bytes verbatim, so "prod", "us-east-1" and
// "prod/us-east-1" are three keys that cannot become each other.
//
// The failure this prevents is not a crash. GROWTH-FINDINGS §7 spells it out:
// "a checkpoint restored from another scope, or a history attached to a reused
// identifier, produces a decrease" — and a decrease is refused by
// storage-growth-history-inconsistent, so the FIRST-ORDER outcome of a
// collision is a refusal, not a lie. The dangerous case is the collision that
// looks consistent: two same-named databases in two regions whose histories
// interleave into a plausible upward slope, producing a growth verdict about a
// database that never grew. That verdict would carry a rate, a projection and
// a confident tone. Hence a key encoding with nothing to collide and a stored
// scope the reader re-checks.

// bucketRDSCheckpoints holds scope → framed RDS domain checkpoint.
var bucketRDSCheckpoints = []byte("rds-checkpoints")

// RDSEnvelopeVersion is the framing version this build writes. Independent of
// whatever version pkg/rds's own Domain.Checkpoint carries inside the blob.
const RDSEnvelopeVersion = 1

// MaxRDSCheckpointBytes bounds one scope's DECODED checkpoint, at write and
// again at read.
//
// The arithmetic is GROWTH-FINDINGS §7's, taken at its own stated bound rather
// than re-derived: DefaultGrowthMaxObservations = 768 observations per
// instance at ~40 bytes each is ~30 KiB per instance, and §7 sizes the worst
// realistic account at 1,000 instances:
//
//	768 × 40 B = 30,720 B per instance
//	30,720 B × 1,000 instances ≈ 29.3 MiB of history
//
// The rest of each Target — identifier, class, engine, the sizing inputs — is
// small next to its history but not free, and JSON keys are repeated per
// observation, so double it: ~59 MiB, and the cap is 64 MiB. Same value as
// MaxEvidenceCheckpointBytes, same binding constraint (a bbolt value is
// materialized whole on both sides of a write), and the same remedy in the
// refusal: an account that has outgrown this needs a tighter
// Config.Growth.MaxObservations, which §7 already names as the knob.
//
// [unverified: no 1,000-instance account has been measured; §7 flags the same
// figure as unverified and this cap inherits that.]
const MaxRDSCheckpointBytes = 64 << 20

// MaxRDSCheckpointScopes bounds distinct scopes.
//
// A scope-keyed bucket with no cap on scopes is not a ceiling, it is a growth
// curve: a caller deriving a scope from something accidentally per-run — a
// session id, a timestamp, a hostname — writes a new key every tick and the
// file grows until the disk does not. The count cap is what makes the total a
// number that can be stated: 64 scopes × 64 MiB is a 4 GiB absolute ceiling,
// and at the ~10× compression this framing gets on repetitive observation JSON
// [unverified: not measured for RDS checkpoints; the figure is history.go's
// for snapshot JSON] a realistic full fleet is a few hundred MiB on disk.
//
// 64 is chosen as "more account/region pairs than one brain should be watching
// serially". Past it, a NEW scope is refused by name while existing scopes
// keep updating — the failure lands on the newcomer, not on the fleet that is
// already being observed correctly. DeleteRDSCheckpoint is how an operator
// reclaims a slot whose scope no longer exists.
const MaxRDSCheckpointScopes = 64

var rdsKind = checkpointKind{
	name:      "rds",
	bucket:    bucketRDSCheckpoints,
	version:   RDSEnvelopeVersion,
	max:       MaxRDSCheckpointBytes,
	maxScopes: MaxRDSCheckpointScopes,
}

// SaveRDSCheckpoint stores one scope's checkpoint, replacing the previous one.
// blob is whatever rds.Domain.Checkpoint() produced; this package does not
// look inside it.
//
// Callers write this at the end of each collection tick, after Observe. See
// PERSIST-FINDINGS.md §5 for the sequence, which is GROWTH-FINDINGS §6's
// snippet with the two missing calls filled in.
func (s *Store) SaveRDSCheckpoint(scope string, blob []byte) error {
	return rdsKind.save(s, scope, blob)
}

// LoadRDSCheckpoint returns one scope's checkpoint bytes for
// rds.Domain.Restore.
//
// A scope observed for the first time is errors.Is(err, ErrNoCheckpoint), and
// that is the only error a caller may proceed through: the domain then starts
// with no history and every instance reports GrowthNoHistory, which is §6's
// intended cold-start behaviour. Every other error means a checkpoint exists
// and could not be read, and proceeding through THAT silently restarts a
// 14-day observation window while reporting nothing wrong — the growth finding
// would simply never appear, and no one would know why.
func (s *Store) LoadRDSCheckpoint(scope string) ([]byte, error) {
	return rdsKind.load(s, scope)
}

// RDSCheckpointScopes lists the stored scopes in ascending key order, so an
// operator can see what the file is holding and which slots the scope cap is
// spending.
func (s *Store) RDSCheckpointScopes() ([]string, error) {
	return rdsKind.scopes(s)
}

// DeleteRDSCheckpoint drops one scope's checkpoint and frees its slot.
//
// It exists because MaxRDSCheckpointScopes is otherwise a one-way door: an
// account that is renamed or decommissioned would hold its slot forever. The
// deletion is real and unrecoverable — the growth history goes with it, and
// the next report for that scope is GrowthNoHistory until a new window fills.
// That is the safe direction (a finding is lost, never manufactured), but it
// is a loss, so an absent scope reports ErrNoCheckpoint rather than quietly
// succeeding.
func (s *Store) DeleteRDSCheckpoint(scope string) error {
	return rdsKind.remove(s, scope)
}
