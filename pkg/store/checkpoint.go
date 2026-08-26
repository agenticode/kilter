package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

// Scope-keyed checkpoint framing — the write side pkg/api's
// WHATIFROUTES-FINDINGS.md §5.3, pkg/rds's GROWTH-FINDINGS.md §6 and
// cmd/WIRING-FINDINGS.md §6.2/§6.3 all stopped at. See PERSIST-FINDINGS.md.
//
// evidence.go is the precedent and this file is its generalization in exactly
// one dimension: SaveEvidenceCheckpoint stores ONE blob for the whole brain,
// and pkg/rds needs one per scope. Everything else is deliberately identical —
// a JSON envelope carrying its own version independent of the payload's, gzip
// inside it, an unknown version refused by name rather than guessed at.
//
// # Why a scope key is the dangerous part
//
// A checkpoint served under the wrong scope is not a crash, it is a wrong
// answer delivered confidently: one region's allocated-storage history read as
// another's produces a growth verdict about a database that never grew. Two
// defences, layered, because the cheap one is not sufficient on its own:
//
//  1. The key is the scope's bytes and nothing else. There is no separator to
//     be contained, no prefix to be shared, no composition to be ambiguous —
//     unlike bucketPlans and bucketSnapHistory, whose keys are
//     "<cluster>/<timestamp>" and therefore need parsePlanTime/parseSnapTime
//     to tell "a/b" from "a" plus "b". A one-component key cannot be parsed
//     wrong because it is never parsed.
//  2. The envelope repeats the scope, and the reader refuses a record whose
//     stored scope is not the one asked for. This is the identity check
//     LoadSnapshot and LatestPlan already make against ClusterID, for the same
//     reason: even granting a key collision that (1) says is impossible, the
//     wrong history is refused rather than served.
//
// Scopes are validated at the door instead of being escaped, so that (1) can
// stay true without an encoding. validateScope is the gate.

// checkpointCodec is the only codec this build writes or reads.
//
// It is "gzip" and not evidence.go's "gzip+json" because the framing here is
// over OPAQUE BYTES: pkg/store does not parse a proposal store or an RDS
// checkpoint, does not import the packages that produce them, and must not
// start — the payload carries its own version (whatif.storeVersion,
// rds.Domain's own) and its producer owns its schema. The field exists so that
// a future codec is refused by name rather than misread, which is the same
// service the version field performs.
const checkpointCodec = "gzip"

// MaxScopeBytes bounds a scope key. The bound is not bbolt's (32 KiB): a scope
// is an identifier a human types into a config file and reads back out of an
// error message, and 512 bytes is already two orders of magnitude past
// "<account>/<region>". A caller passing a document as a scope has a bug, and
// the earlier it is named the cheaper it is.
const MaxScopeBytes = 512

// The three facts a cold read can report, kept apart on purpose.
//
// A restart that lost data must not be indistinguishable from one that never
// had any. "Nothing was found" is Tuesday — the first boot of a new brain, a
// scope observed for the first time. "Something was found and I could not read
// it" is a bug report, and a caller that starts empty on it has silently
// discarded an audit trail. So the load path returns an error in every case,
// and the caller's rule is one line: errors.Is(err, ErrNoCheckpoint) means
// start empty, ANY other error means stop and say so.
var (
	// ErrNoCheckpoint: the bucket exists and holds nothing under this scope.
	ErrNoCheckpoint = errors.New("no checkpoint is stored")
	// ErrCheckpointBucket: the bucket itself is absent. Open creates every
	// bucket, so this is not a cold start — it is a file that was truncated,
	// hand-edited, or written by something that is not this program.
	ErrCheckpointBucket = errors.New("checkpoint bucket is missing from the store file")
	// ErrFutureCheckpoint: the envelope carries a version this build does not
	// speak. Distinct from corruption because the remedy is opposite: a
	// corrupt record may be discarded, a future one must be left alone, since
	// discarding it destroys a newer build's state on a downgrade.
	ErrFutureCheckpoint = errors.New("checkpoint was written by a build this one does not understand")
	// ErrCorruptCheckpoint: a record is present and unreadable — not an
	// envelope, wrong kind, wrong scope, unknown codec, broken gzip.
	ErrCorruptCheckpoint = errors.New("stored checkpoint cannot be read")
	// ErrCheckpointTooLarge: over the size cap, at write or at read. Separate
	// from ErrCorruptCheckpoint because the bytes may be perfectly well formed
	// and simply too many, and because the remedy is a configuration change in
	// the producing package rather than a repair.
	ErrCheckpointTooLarge = errors.New("checkpoint is over this build's size cap")
	// ErrInvalidScope: a scope that cannot be stored as a key.
	ErrInvalidScope = errors.New("checkpoint scope is not storable")
	// ErrEmptyCheckpoint: a zero-length blob. Refused at write because it
	// would otherwise read back as a checkpoint carrying nothing, which is a
	// third meaning for a state that already has two.
	ErrEmptyCheckpoint = errors.New("checkpoint is empty")
)

// CheckpointError carries which checkpoint failed and why. Callers match the
// cause with errors.Is and, when they want to log the scope rather than parse
// it out of a string, reach the fields with errors.As.
type CheckpointError struct {
	// Kind is the checkpoint family: "proposals" or "rds".
	Kind string
	// Scope is the key that was asked for. Empty for a single-scope kind.
	Scope string
	// Detail names the specific failure, e.g. both version numbers.
	Detail string
	// Err is one of the sentinels above.
	Err error
}

func (e *CheckpointError) Error() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "store: %s checkpoint", e.Kind)
	if e.Scope != "" {
		fmt.Fprintf(&b, " %q", e.Scope)
	}
	fmt.Fprintf(&b, ": %v", e.Err)
	if e.Detail != "" {
		fmt.Fprintf(&b, ": %s", e.Detail)
	}
	return b.String()
}

func (e *CheckpointError) Unwrap() error { return e.Err }

// checkpointEnvelope frames one stored checkpoint.
//
// Scope and Kind are stored as well as implied by the key and the bucket. That
// is deliberate redundancy: a record can only be misfiled once, and a reader
// that checks what it was handed against what it asked for turns "misfiled"
// from a wrong answer into a refusal.
type checkpointEnvelope struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Scope   string `json:"scope"`
	Codec   string `json:"codec"`
	// Blob is gzip of the caller's bytes, which encoding/json renders as
	// base64.
	Blob []byte `json:"blob"`
}

// checkpointKind is one bucket's contract: its name, its framing version, its
// size ceiling, and how many scopes it will hold. Both buckets in this package
// share every line of the code below; they differ only in these five values.
type checkpointKind struct {
	name    string
	bucket  []byte
	version int
	// max bounds the DECODED blob, at write and again at read.
	max int
	// maxScopes bounds distinct keys. Zero means the kind is single-scope and
	// scopeless: fixedScope is the only key it will ever use.
	maxScopes int
	// fixedScope is the key a single-scope kind stores under.
	fixedScope string
}

func (k checkpointKind) errf(scope string, sentinel error, format string, args ...any) error {
	return &CheckpointError{Kind: k.name, Scope: scope, Detail: fmt.Sprintf(format, args...), Err: sentinel}
}

func (k checkpointKind) err(scope string, sentinel error) error {
	return &CheckpointError{Kind: k.name, Scope: scope, Err: sentinel}
}

// validateScope rejects a scope that cannot survive being a key.
//
// The reasoning is validateClusterID's, one step further. A scope is used two
// ways at once — raw as the bbolt key, and JSON-encoded inside the envelope —
// so a scope that does not survive encoding/json unchanged would write a
// record whose own Scope no longer matches the key it lives under, which the
// reader would then (correctly) refuse as corrupt. An unreadable write is
// worse than a refused one, so it is refused here, where the operator can
// still see which scope was wrong.
//
// Control characters are refused for whatif.sanitizeNote's reason rather than
// an encoding one: this string is echoed into error messages, log lines and
// `bbolt` CLI dumps, and a scope carrying a newline can forge a log line.
func validateScope(scope string) error {
	if scope == "" {
		return errors.New("a scope must not be empty; bbolt has no empty key")
	}
	if len(scope) > MaxScopeBytes {
		return fmt.Errorf("scope is %d bytes, over the %d-byte cap", len(scope), MaxScopeBytes)
	}
	if !utf8.ValidString(scope) {
		return errors.New("scope is not valid UTF-8; encoding/json would rewrite it to U+FFFD and the key and the record would stop agreeing")
	}
	for _, r := range scope {
		if unicode.IsControl(r) {
			return fmt.Errorf("scope contains the control character %q, which cannot be audited", r)
		}
	}
	return nil
}

// scopeOf resolves the key a call operates on: the caller's for a scope-keyed
// kind, the fixed one for a single-scope kind.
func (k checkpointKind) scopeOf(scope string) (string, error) {
	if k.maxScopes == 0 {
		return k.fixedScope, nil
	}
	if err := validateScope(scope); err != nil {
		return "", k.errf(scope, ErrInvalidScope, "%s", err.Error())
	}
	return scope, nil
}

// encode frames a blob. gzip is written with default settings and no header
// fields, so the framing is a function of the input alone: the same state
// always produces the same bytes, and re-saving unchanged state is a no-op
// write rather than a diff. encodeSnapshot relies on the same property.
func (k checkpointKind) encode(scope string, blob []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(blob); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return json.Marshal(checkpointEnvelope{
		Version: k.version,
		Kind:    k.name,
		Scope:   scope,
		Codec:   checkpointCodec,
		Blob:    buf.Bytes(),
	})
}

// maxStored bounds the stored record. gzip of incompressible input can exceed
// it slightly and encoding/json base64s the result, so a legitimate record can
// be about 4/3 of the decoded cap; 3/2 plus the envelope's own bytes is that
// with room. The point is to refuse a hostile value BEFORE copying it out of
// the mmap, not to second-guess the compressor.
func (k checkpointKind) maxStored() int { return k.max + k.max/2 + 4096 }

// save writes one checkpoint, replacing any previous one for the scope.
func (k checkpointKind) save(s *Store, scope string, blob []byte) error {
	key, err := k.scopeOf(scope)
	if err != nil {
		return err
	}
	if len(blob) == 0 {
		return k.err(key, ErrEmptyCheckpoint)
	}
	if len(blob) > k.max {
		return k.errf(key, ErrCheckpointTooLarge, "%d bytes, over the %d-byte cap", len(blob), k.max)
	}
	raw, err := k.encode(key, blob)
	if err != nil {
		return k.errf(key, ErrCorruptCheckpoint, "framing: %v", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(k.bucket)
		if b == nil {
			return k.err(key, ErrCheckpointBucket)
		}
		// The scope cap is enforced inside the write transaction, where bbolt
		// has already serialized writers, so two concurrent saves cannot both
		// observe the last free slot and both take it.
		if k.maxScopes > 0 && b.Get([]byte(key)) == nil {
			if n := countKeys(b); n >= k.maxScopes {
				return k.errf(key, ErrCheckpointTooLarge,
					"the bucket already holds %d scopes, the cap is %d; delete a scope that no longer exists",
					n, k.maxScopes)
			}
		}
		return b.Put([]byte(key), raw)
	})
}

// load returns one checkpoint's bytes. Every outcome other than success is an
// error naming which of the failures it was; nothing returns a nil blob and a
// nil error, because that is the shape in which "lost" reads as "new".
func (k checkpointKind) load(s *Store, scope string) ([]byte, error) {
	key, err := k.scopeOf(scope)
	if err != nil {
		return nil, err
	}
	var raw []byte
	var loadErr error
	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(k.bucket)
		if b == nil {
			loadErr = k.err(key, ErrCheckpointBucket)
			return nil
		}
		v := b.Get([]byte(key))
		if v == nil {
			loadErr = k.err(key, ErrNoCheckpoint)
			return nil
		}
		if len(v) > k.maxStored() {
			loadErr = k.errf(key, ErrCheckpointTooLarge,
				"stored record is %d bytes, over the %d-byte stored cap", len(v), k.maxStored())
			return nil
		}
		// bbolt values alias the mmap; copy before the transaction ends.
		raw = append([]byte(nil), v...)
		return nil
	}); err != nil {
		return nil, k.errf(key, ErrCorruptCheckpoint, "reading: %v", err)
	}
	if loadErr != nil {
		return nil, loadErr
	}
	return k.decode(key, raw)
}

func (k checkpointKind) decode(scope string, raw []byte) ([]byte, error) {
	var env checkpointEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, k.errf(scope, ErrCorruptCheckpoint, "stored record is not a checkpoint envelope: %v", err)
	}
	// Version first: a future envelope may legitimately carry fields whose
	// meaning this build would misjudge, so nothing below is trusted until the
	// layout is one this build wrote.
	if env.Version != k.version {
		if env.Version > k.version {
			return nil, k.errf(scope, ErrFutureCheckpoint,
				"envelope version %d, this build speaks %d; refusing to guess at a layout it does not know",
				env.Version, k.version)
		}
		return nil, k.errf(scope, ErrCorruptCheckpoint,
			"envelope version %d, this build speaks %d and has never written another",
			env.Version, k.version)
	}
	if env.Kind != k.name {
		return nil, k.errf(scope, ErrCorruptCheckpoint,
			"envelope holds a %q checkpoint, not a %q one", env.Kind, k.name)
	}
	// The second collision defence. The key encoding makes this unreachable
	// for records this package wrote; it fires for a record moved between keys
	// by anything else, and it is what makes "served under the wrong scope"
	// structurally impossible rather than merely unlikely.
	if env.Scope != scope {
		return nil, k.errf(scope, ErrCorruptCheckpoint,
			"envelope holds scope %q; refusing to serve it under %q", env.Scope, scope)
	}
	if env.Codec != checkpointCodec {
		return nil, k.errf(scope, ErrCorruptCheckpoint, "codec %q is not supported by this build", env.Codec)
	}
	if len(env.Blob) == 0 {
		return nil, k.errf(scope, ErrCorruptCheckpoint, "envelope carries no blob")
	}
	zr, err := gzip.NewReader(bytes.NewReader(env.Blob))
	if err != nil {
		return nil, k.errf(scope, ErrCorruptCheckpoint, "gzip: %v", err)
	}
	defer zr.Close()
	// The cap is applied again HERE, not only at write. A stored value is a
	// few hundred kilobytes of base64 that can inflate without limit, and this
	// process is meant to run unattended for months; a decompression bomb in a
	// bbolt value is still a decompression bomb. +1 so an input of exactly the
	// cap is distinguishable from one over it.
	out, err := io.ReadAll(io.LimitReader(zr, int64(k.max)+1))
	if err != nil {
		return nil, k.errf(scope, ErrCorruptCheckpoint, "gzip: %v", err)
	}
	if len(out) > k.max {
		return nil, k.errf(scope, ErrCheckpointTooLarge,
			"checkpoint decompresses past the %d-byte cap", k.max)
	}
	return out, nil
}

// remove deletes one scope's checkpoint. Deleting an absent scope is
// ErrNoCheckpoint, not success: a caller freeing a slot needs to know whether
// it freed one.
func (k checkpointKind) remove(s *Store, scope string) error {
	key, err := k.scopeOf(scope)
	if err != nil {
		return err
	}
	var missing error
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(k.bucket)
		if b == nil {
			return k.err(key, ErrCheckpointBucket)
		}
		if b.Get([]byte(key)) == nil {
			missing = k.err(key, ErrNoCheckpoint)
			return nil
		}
		return b.Delete([]byte(key))
	}); err != nil {
		return err
	}
	return missing
}

// scopes lists stored scopes in ascending key order. The keys ARE the scopes —
// see the file comment — so no decoding is involved and a scope that cannot be
// listed is a scope that was never written.
func (k checkpointKind) scopes(s *Store) ([]string, error) {
	var out []string
	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(k.bucket)
		if b == nil {
			return k.err("", ErrCheckpointBucket)
		}
		return b.ForEach(func(key, _ []byte) error {
			out = append(out, string(key))
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func countKeys(b *bolt.Bucket) int {
	n := 0
	c := b.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		n++
	}
	return n
}
