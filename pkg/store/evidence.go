package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"

	bolt "go.etcd.io/bbolt"
)

// Evidence-substrate checkpoint persistence.
//
// pkg/evidence produces and consumes bytes and never opens a file — its
// CheckpointStore interface is the seam, and this file is the implementation
// it named. The substrate's own Checkpoint carries a version and
// FromCheckpoint refuses a version it does not recognize, so the interesting
// compatibility question is not "what is inside the blob" but "how is the
// blob framed", and that is what the envelope below answers.
//
// # The compatibility contract
//
// A stored checkpoint is a JSON envelope with its own version, independent of
// the substrate's:
//
//	{"version":1,"codec":"gzip+json","blob":"<base64 gzip of checkpoint JSON>"}
//	{"version":1,"codec":"json","payload":<checkpoint JSON>}
//
// Both codecs are READ. Only "gzip+json" is written — checkpoint JSON is
// highly repetitive and compresses roughly tenfold, which matters because
// this value is rewritten on every checkpoint tick.
//
// An envelope version this build does not know is REJECTED BY NAME, never
// guessed at, and so is an unknown codec. That is the whole rule: an old
// checkpoint either loads, or fails with an error naming the version it
// carries and the version this build speaks. It is never silently misread —
// a half-understood evidence substrate would serve citations that resolve to
// the wrong observations, which is the exact failure the substrate exists to
// prevent.

// bucketEvidence holds scope → framed evidence checkpoint.
var bucketEvidence = []byte("evidence")

// evidenceScope is the key the brain's substrate is filed under. It is a
// constant rather than a cluster id because evidence.Memory is fleet-wide:
// its subjects carry their own cluster, and one checkpoint covers them all.
const evidenceScope = "brain"

// EvidenceEnvelopeVersion is the framing version this build writes.
const EvidenceEnvelopeVersion = 1

// Codecs. Both are read; the first is written.
const (
	evidenceCodecGzipJSON = "gzip+json"
	evidenceCodecJSON     = "json"
)

// MaxEvidenceCheckpointBytes bounds the DECODED checkpoint. pkg/evidence's own
// default budgets allow well over a gigabyte in memory, and a bbolt value must
// be materialized whole on both sides of a write, so the persistence layer
// applies its own, much smaller bound and refuses rather than tries. A brain
// whose substrate has outgrown this is a brain whose evidence.Config needs
// tightening, and an explicit error says so where a truncated write would not.
const MaxEvidenceCheckpointBytes = 64 << 20

type evidenceEnvelope struct {
	Version int    `json:"version"`
	Codec   string `json:"codec"`
	// Payload carries codec "json": the checkpoint JSON, inline.
	Payload json.RawMessage `json:"payload,omitempty"`
	// Blob carries codec "gzip+json": gzip of the checkpoint JSON, which
	// encoding/json renders as base64.
	Blob []byte `json:"blob,omitempty"`
}

// SaveEvidenceCheckpoint stores the substrate checkpoint bytes, replacing any
// previous one. data is opaque here — it is whatever
// evidence.Memory.MarshalCheckpoint produced.
func (s *Store) SaveEvidenceCheckpoint(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("store: evidence checkpoint is empty")
	}
	if len(data) > MaxEvidenceCheckpointBytes {
		return fmt.Errorf("store: evidence checkpoint is %d bytes, over the %d-byte cap; tighten evidence.Config",
			len(data), MaxEvidenceCheckpointBytes)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return fmt.Errorf("store: evidence checkpoint: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("store: evidence checkpoint: %w", err)
	}
	env := evidenceEnvelope{
		Version: EvidenceEnvelopeVersion,
		Codec:   evidenceCodecGzipJSON,
		Blob:    buf.Bytes(),
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return put(tx, bucketEvidence, evidenceScope, env)
	}); err != nil {
		return fmt.Errorf("store: save evidence checkpoint: %w", err)
	}
	return nil
}

// LoadEvidenceCheckpoint returns the stored checkpoint bytes, or nil, nil when
// nothing has been checkpointed yet — a cold start, which callers treat as
// "start empty" rather than as corruption.
func (s *Store) LoadEvidenceCheckpoint() ([]byte, error) {
	var raw []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketEvidence)
		if err != nil {
			return err
		}
		v := b.Get([]byte(evidenceScope))
		if v == nil {
			return nil
		}
		// bbolt values alias the mmap; copy before the transaction ends.
		raw = append([]byte(nil), v...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: load evidence checkpoint: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	out, err := decodeEvidenceEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("store: load evidence checkpoint: %w", err)
	}
	return out, nil
}

func decodeEvidenceEnvelope(raw []byte) ([]byte, error) {
	var env evidenceEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("stored record is not an evidence envelope: %w", err)
	}
	if env.Version != EvidenceEnvelopeVersion {
		return nil, fmt.Errorf("envelope version %d, want %d; this build will not guess at a layout it does not know",
			env.Version, EvidenceEnvelopeVersion)
	}
	switch env.Codec {
	case evidenceCodecJSON:
		if len(env.Payload) == 0 {
			return nil, fmt.Errorf("envelope codec %q carries no payload", env.Codec)
		}
		if len(env.Payload) > MaxEvidenceCheckpointBytes {
			return nil, fmt.Errorf("checkpoint is %d bytes, over the %d-byte cap",
				len(env.Payload), MaxEvidenceCheckpointBytes)
		}
		return append([]byte(nil), env.Payload...), nil
	case evidenceCodecGzipJSON:
		if len(env.Blob) == 0 {
			return nil, fmt.Errorf("envelope codec %q carries no blob", env.Codec)
		}
		zr, err := gzip.NewReader(bytes.NewReader(env.Blob))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		// +1 so an input of exactly the cap is distinguishable from one over
		// it; without the bound a small blob could inflate without limit.
		out, err := io.ReadAll(io.LimitReader(zr, MaxEvidenceCheckpointBytes+1))
		if err != nil {
			return nil, err
		}
		if len(out) > MaxEvidenceCheckpointBytes {
			return nil, fmt.Errorf("checkpoint decompresses beyond the %d-byte cap", MaxEvidenceCheckpointBytes)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("envelope codec %q is not supported by this build", env.Codec)
	}
}

// EvidenceCheckpoints returns the context-taking view of the two calls above,
// which is exactly evidence.CheckpointStore. It is a separate type rather than
// two more methods on Store because "SaveCheckpoint" alone would be ambiguous
// next to SaveRecommenderState, which also stores a thing called a checkpoint.
//
// The interface is satisfied structurally: pkg/store does not import
// pkg/evidence, so the dependency order (evidence sits below everything)
// is preserved. store_ext_test.go asserts the satisfaction at compile time.
func (s *Store) EvidenceCheckpoints() EvidenceCheckpointStore {
	return EvidenceCheckpointStore{s: s}
}

// EvidenceCheckpointStore adapts Store to pkg/evidence's persistence seam.
type EvidenceCheckpointStore struct{ s *Store }

// SaveCheckpoint stores the substrate bytes. ctx is accepted for the
// interface; a bbolt write is not cancellable once begun, and pretending
// otherwise would be a lie about what an interrupted checkpoint leaves behind.
func (e EvidenceCheckpointStore) SaveCheckpoint(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.s.SaveEvidenceCheckpoint(data)
}

// LoadCheckpoint returns the stored bytes. A cold store returns nil, nil;
// evidence.UnmarshalCheckpoint turns that into ErrNoCheckpoint, which is the
// "start empty" signal its own Load documents.
func (e EvidenceCheckpointStore) LoadCheckpoint(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.s.LoadEvidenceCheckpoint()
}
