package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// A checkpoint blob is opaque to pkg/store, so these tests use a stand-in
// document rather than a real evidence.Checkpoint: what is under test is the
// FRAMING contract, not the substrate's own schema (which pkg/evidence's own
// codec tests own, and which carries its own independent version).
var sampleCheckpoint = []byte(`{"version":1,"config":{},"seq":42,"subjects":[]}`)

func TestEvidenceCheckpointRoundtrips(t *testing.T) {
	s := open(t)
	if err := s.SaveEvidenceCheckpoint(sampleCheckpoint); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadEvidenceCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sampleCheckpoint) {
		t.Fatalf("roundtrip returned %q", got)
	}
}

func TestColdEvidenceStoreIsNotAnError(t *testing.T) {
	s := open(t)
	got, err := s.LoadEvidenceCheckpoint()
	if err != nil {
		t.Fatalf("a cold store reported corruption: %v", err)
	}
	if got != nil {
		t.Fatalf("a cold store returned %q", got)
	}
}

// TestOldPlainJSONEnvelopeStillLoads is the backward half of the
// compatibility contract: a checkpoint written under the uncompressed codec
// must load into this build unchanged. The envelope is hand-built rather than
// produced by this package precisely because this package no longer writes
// it — a round-trip through the current writer would not test compatibility
// with anything.
func TestOldPlainJSONEnvelopeStillLoads(t *testing.T) {
	s := open(t)
	old := evidenceEnvelope{Version: 1, Codec: "json", Payload: json.RawMessage(sampleCheckpoint)}
	writeEnvelope(t, s, old)
	got, err := s.LoadEvidenceCheckpoint()
	if err != nil {
		t.Fatalf("an old plain-JSON checkpoint did not load: %v", err)
	}
	if string(got) != string(sampleCheckpoint) {
		t.Fatalf("old checkpoint loaded as %q", got)
	}
}

// TestFutureEnvelopeVersionIsRejectedByName is the forward half: a checkpoint
// written by a later build is refused with an error naming both versions,
// never partially decoded. Silently misreading it would serve citations that
// resolve against the wrong observations.
func TestFutureEnvelopeVersionIsRejectedByName(t *testing.T) {
	s := open(t)
	future := evidenceEnvelope{Version: EvidenceEnvelopeVersion + 1, Codec: "json", Payload: json.RawMessage(sampleCheckpoint)}
	writeEnvelope(t, s, future)
	_, err := s.LoadEvidenceCheckpoint()
	if err == nil {
		t.Fatal("a future envelope version was read anyway")
	}
	msg := err.Error()
	if !strings.Contains(msg, "version 2") || !strings.Contains(msg, "want 1") {
		t.Fatalf("error does not name both versions: %v", err)
	}
}

func TestUnknownCodecIsRejected(t *testing.T) {
	s := open(t)
	writeEnvelope(t, s, evidenceEnvelope{Version: 1, Codec: "zstd+cbor", Payload: json.RawMessage(sampleCheckpoint)})
	_, err := s.LoadEvidenceCheckpoint()
	if err == nil {
		t.Fatal("an unknown codec was decoded anyway")
	}
	if !strings.Contains(err.Error(), "zstd+cbor") {
		t.Fatalf("error does not name the codec: %v", err)
	}
}

func TestGarbageEvidenceRecordIsAnErrorNotEmptyState(t *testing.T) {
	s := open(t)
	putRaw(t, s, []byte("not json at all"))
	if _, err := s.LoadEvidenceCheckpoint(); err == nil {
		t.Fatal("a corrupt record was reported as a cold start")
	}
}

// writeEnvelope stores an envelope this package would not itself write, so
// the reader can be tested against framings other builds produce.
func writeEnvelope(t *testing.T, s *Store, env evidenceEnvelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	putRaw(t, s, raw)
}

func putRaw(t *testing.T, s *Store, raw []byte) {
	t.Helper()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketEvidence)
		if err != nil {
			return err
		}
		return b.Put([]byte(evidenceScope), raw)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOversizeCheckpointIsRefusedAtWrite(t *testing.T) {
	s := open(t)
	// One byte over the cap, so the boundary itself is what is asserted.
	big := make([]byte, MaxEvidenceCheckpointBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	err := s.SaveEvidenceCheckpoint(big)
	if err == nil {
		t.Fatal("an oversize checkpoint was stored")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("refusal does not name the cap: %v", err)
	}
	// And nothing was written: a refused save must leave a cold store cold.
	got, err := s.LoadEvidenceCheckpoint()
	if err != nil || got != nil {
		t.Fatalf("refused save left %q (err %v)", got, err)
	}
}

func TestEmptyCheckpointIsRefused(t *testing.T) {
	s := open(t)
	if err := s.SaveEvidenceCheckpoint(nil); err == nil {
		t.Fatal("an empty checkpoint was stored, which would read back as a cold start")
	}
}

func TestEvidenceCheckpointStoreHonoursContextCancellation(t *testing.T) {
	s := open(t)
	cs := s.EvidenceCheckpoints()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cs.SaveCheckpoint(ctx, sampleCheckpoint); err == nil {
		t.Fatal("a cancelled save was performed")
	}
	if _, err := cs.LoadCheckpoint(ctx); err == nil {
		t.Fatal("a cancelled load was performed")
	}
	if err := cs.SaveCheckpoint(context.Background(), sampleCheckpoint); err != nil {
		t.Fatal(err)
	}
	got, err := cs.LoadCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sampleCheckpoint) {
		t.Fatalf("adapter returned %q", got)
	}
}
