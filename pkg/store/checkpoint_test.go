package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// A checkpoint blob is opaque to pkg/store, so the tests in this file frame
// stand-in documents: what is under test is the FRAMING and the KEYING, not
// any producer's schema. persist_ext_test.go does the other half with a real
// whatif.Store.

// smallKind is a checkpoint family with a tiny cap, so the bound-at-read tests
// assert the same code path the 64 MiB kinds use without allocating 64 MiB to
// do it. The two real kinds are exercised against their real caps in
// TestRealKindsRefuseOversizeAtWrite.
func smallKind(t *testing.T, s *Store, name string, max, maxScopes int) checkpointKind {
	t.Helper()
	k := checkpointKind{
		name:      name,
		bucket:    []byte("test-" + name),
		version:   1,
		max:       max,
		maxScopes: maxScopes,
	}
	if maxScopes == 0 {
		k.fixedScope = "brain"
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(k.bucket)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return k
}

// putCheckpointRaw stores bytes this package would not itself write, so the
// reader can be tested against records other builds — or an attacker — produce.
func putCheckpointRaw(t *testing.T, s *Store, k checkpointKind, key string, raw []byte) {
	t.Helper()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(k.bucket).Put([]byte(key), raw)
	}); err != nil {
		t.Fatal(err)
	}
}

func putEnvelope(t *testing.T, s *Store, k checkpointKind, key string, env checkpointEnvelope) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	putCheckpointRaw(t, s, k, key, raw)
}

// gzipOf frames a payload the way encode does, for hand-built envelopes.
func gzipOf(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// storedKeys returns the raw bbolt keys of a bucket. The collision tests
// assert over these rather than over the API, because the claim being made is
// about the key encoding, not about what happens to survive a round trip.
func storedKeys(t *testing.T, s *Store, k checkpointKind) []string {
	t.Helper()
	var out []string
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(k.bucket).ForEach(func(key, _ []byte) error {
			out = append(out, string(key))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// rdsBlob builds a stand-in RDS checkpoint with an irregular history: uneven
// gaps, a ratchet step, and a scope-specific marker so a cross-scope leak is
// visible in the bytes rather than only in a length.
func rdsBlob(scope string, days []int) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"scope":%q,"targets":[{"id":"db-1","history":[`, scope)
	for i, d := range days {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"day":%d,"allocatedGiB":%d}`, d, 100+10*i)
	}
	b.WriteString(`]}]}`)
	return b.Bytes()
}

// ---- the three facts a load can report ----

// TestColdScopeIsNotAnEmptyBlob prevents the failure the whole error taxonomy
// exists for: a caller that cannot tell "never had a checkpoint" from "had one
// and could not read it" silently restarts an observation window over an audit
// trail it just failed to read.
func TestColdScopeIsNotAnEmptyBlob(t *testing.T) {
	s := open(t)
	blob, err := s.LoadRDSCheckpoint("prod/us-east-1")
	if blob != nil {
		t.Fatalf("a cold scope returned %q as well as an error", blob)
	}
	if !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("a cold scope reported %v, want ErrNoCheckpoint", err)
	}
	// And it is not any of the other three facts.
	for _, other := range []error{ErrCorruptCheckpoint, ErrFutureCheckpoint, ErrCheckpointBucket} {
		if errors.Is(err, other) {
			t.Fatalf("a cold scope also matched %v", other)
		}
	}
	var ce *CheckpointError
	if !errors.As(err, &ce) || ce.Scope != "prod/us-east-1" || ce.Kind != "rds" {
		t.Fatalf("error does not carry kind and scope: %#v", ce)
	}
}

// TestMissingBucketIsNotAColdStart: a truncated or foreign file must not read
// as a brain that has never checkpointed. Open creates every bucket, so a
// bucket that is gone is a statement about the FILE, and wiping and starting
// over on it destroys whatever else is in there.
func TestMissingBucketIsNotAColdStart(t *testing.T) {
	s := open(t)
	if err := s.SaveRDSCheckpoint("prod", rdsBlob("prod", []int{0, 3})); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket(bucketRDSCheckpoints)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.LoadRDSCheckpoint("prod")
	if !errors.Is(err, ErrCheckpointBucket) {
		t.Fatalf("a missing bucket reported %v, want ErrCheckpointBucket", err)
	}
	if errors.Is(err, ErrNoCheckpoint) {
		t.Fatal("a missing bucket collapsed into a cold start")
	}
	if _, err := s.LoadProposals(); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("deleting one bucket changed another's answer: %v", err)
	}
}

// TestFutureVersionIsRefusedByNameAndIsNotCorruption: the remedy for the two
// is opposite. A corrupt record may be discarded; a record written by a newer
// build must be left alone, because discarding it destroys that build's state
// the moment someone rolls back.
func TestFutureVersionIsRefusedByNameAndIsNotCorruption(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "future", 1024, 4)
	putEnvelope(t, s, k, "prod", checkpointEnvelope{
		Version: k.version + 1, Kind: k.name, Scope: "prod",
		Codec: checkpointCodec, Blob: gzipOf(t, []byte("payload")),
	})
	_, err := k.load(s, "prod")
	if !errors.Is(err, ErrFutureCheckpoint) {
		t.Fatalf("a future envelope reported %v, want ErrFutureCheckpoint", err)
	}
	if errors.Is(err, ErrCorruptCheckpoint) || errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("a future envelope also matched another fact: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "version 2") || !strings.Contains(msg, "speaks 1") {
		t.Fatalf("error does not name both versions: %v", err)
	}
	// A version BELOW this build's is corruption, not the future: this build
	// has never written another, so there is no older layout to be compatible
	// with and guessing would be inventing one.
	putEnvelope(t, s, k, "old", checkpointEnvelope{
		Version: 0, Kind: k.name, Scope: "old",
		Codec: checkpointCodec, Blob: gzipOf(t, []byte("payload")),
	})
	if _, err := k.load(s, "old"); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("a version-0 envelope reported %v, want ErrCorruptCheckpoint", err)
	}
}

// TestCorruptRecordIsNotAColdStart — a bug report must not read as Tuesday.
func TestCorruptRecordIsNotAColdStart(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "corrupt", 1024, 4)
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"not json", []byte("not json at all")},
		{"json but not an envelope", []byte(`{"hello":"world"}`)},
		{"truncated", []byte(`{"version":1,"kind":"corrupt",`)},
		{"empty value", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			putCheckpointRaw(t, s, k, "prod", tc.raw)
			_, err := k.load(s, "prod")
			if !errors.Is(err, ErrCorruptCheckpoint) {
				t.Fatalf("reported %v, want ErrCorruptCheckpoint", err)
			}
			if errors.Is(err, ErrNoCheckpoint) {
				t.Fatal("a corrupt record was reported as a cold start")
			}
		})
	}
}

// TestBrokenGzipIsCorruptNotEmpty: the envelope parses, the payload does not.
func TestBrokenGzipIsCorruptNotEmpty(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "gz", 1024, 4)
	putEnvelope(t, s, k, "prod", checkpointEnvelope{
		Version: 1, Kind: k.name, Scope: "prod", Codec: checkpointCodec,
		Blob: []byte("this is not gzip"),
	})
	if _, err := k.load(s, "prod"); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("reported %v, want ErrCorruptCheckpoint", err)
	}
	// Half a gzip stream: the header is valid, the body is truncated.
	full := gzipOf(t, bytes.Repeat([]byte("payload"), 40))
	putEnvelope(t, s, k, "half", checkpointEnvelope{
		Version: 1, Kind: k.name, Scope: "half", Codec: checkpointCodec,
		Blob: full[:len(full)-6],
	})
	if _, err := k.load(s, "half"); !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("a truncated gzip reported %v, want ErrCorruptCheckpoint", err)
	}
}

func TestUnknownCheckpointCodecIsRefusedByName(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "codec", 1024, 4)
	putEnvelope(t, s, k, "prod", checkpointEnvelope{
		Version: 1, Kind: k.name, Scope: "prod", Codec: "zstd+cbor",
		Blob: gzipOf(t, []byte("payload")),
	})
	_, err := k.load(s, "prod")
	if !errors.Is(err, ErrCorruptCheckpoint) || !strings.Contains(err.Error(), "zstd+cbor") {
		t.Fatalf("an unknown codec was not refused by name: %v", err)
	}
}

// ---- the key encoding ----

// TestScopeKeysAreTheScopeBytes pins the encoding the collision argument rests
// on. If a future change introduces an encoding — an escape, a prefix, a
// hash — this fails, and whoever makes that change has to re-argue collision
// freedom rather than inherit it.
func TestScopeKeysAreTheScopeBytes(t *testing.T) {
	s := open(t)
	scopes := []string{"prod", "prod/us-east-1", "café", "a b", "9"}
	for _, sc := range scopes {
		if err := s.SaveRDSCheckpoint(sc, rdsBlob(sc, []int{0, 1})); err != nil {
			t.Fatal(err)
		}
	}
	got := storedKeys(t, s, rdsKind)
	if len(got) != len(scopes) {
		t.Fatalf("%d scopes wrote %d keys: %q", len(scopes), len(got), got)
	}
	for _, sc := range scopes {
		found := false
		for _, k := range got {
			if k == sc {
				found = true
			}
		}
		if !found {
			t.Fatalf("scope %q is not stored under its own bytes; keys are %q", sc, got)
		}
	}
}

// TestScopesContainingTheSeparatorCannotCollide is the failure GROWTH-FINDINGS
// §7 names: two scopes that collapse into one key serve one region's storage
// history as another's, and the resulting growth verdict is about a database
// that never grew. A composite key — the "<cluster>/<timestamp>" shape
// bucketPlans and bucketSnapHistory use — would make "prod" + "us-east-1"
// indistinguishable from "prod/us-east-1". This bucket's key has one
// component, so there is nothing to compose and nothing to parse back.
func TestScopesContainingTheSeparatorCannotCollide(t *testing.T) {
	s := open(t)
	scopes := []string{
		"prod", "us-east-1", "prod/us-east-1", "prod/us-east-1/extra",
		"prod/", "/us-east-1", "//", "prod//us-east-1",
	}
	for i, sc := range scopes {
		if err := s.SaveRDSCheckpoint(sc, rdsBlob(sc, []int{0, i + 1, i + 9})); err != nil {
			t.Fatalf("scope %q: %v", sc, err)
		}
	}
	if got := len(storedKeys(t, s, rdsKind)); got != len(scopes) {
		t.Fatalf("%d scopes collapsed into %d keys", len(scopes), got)
	}
	for i, sc := range scopes {
		got, err := s.LoadRDSCheckpoint(sc)
		if err != nil {
			t.Fatalf("scope %q: %v", sc, err)
		}
		want := rdsBlob(sc, []int{0, i + 1, i + 9})
		if !bytes.Equal(got, want) {
			t.Fatalf("scope %q served\n %s\nwant\n %s", sc, got, want)
		}
	}
}

// TestUnicodeScopesAreDistinctKeys: two spellings of the same grapheme are two
// scopes. Normalising them together would be a silent merge of two accounts'
// histories, which is the collision this bucket refuses to have; normalising
// them apart is what storing the bytes already does.
func TestUnicodeScopesAreDistinctKeys(t *testing.T) {
	s := open(t)
	// The same grapheme, composed and decomposed: two different byte strings.
	nfc, nfd := "caf\u00e9", "cafe\u0301"
	scopes := []string{nfc, nfd, "東京/prod", "prod-🌍"}
	for i, sc := range scopes {
		if err := s.SaveRDSCheckpoint(sc, rdsBlob(sc, []int{i, i + 4})); err != nil {
			t.Fatalf("scope %q: %v", sc, err)
		}
	}
	if got := len(storedKeys(t, s, rdsKind)); got != len(scopes) {
		t.Fatalf("%d unicode scopes collapsed into %d keys", len(scopes), got)
	}
	for i, sc := range scopes {
		got, err := s.LoadRDSCheckpoint(sc)
		if err != nil {
			t.Fatalf("scope %q: %v", sc, err)
		}
		if want := rdsBlob(sc, []int{i, i + 4}); !bytes.Equal(got, want) {
			t.Fatalf("scope %q served another scope's checkpoint: %s", sc, got)
		}
	}
}

func TestUnstorableScopesAreRefusedAtTheDoor(t *testing.T) {
	s := open(t)
	for _, tc := range []struct{ name, scope string }{
		{"empty", ""},
		{"10 KiB", strings.Repeat("s", 10<<10)},
		{"one over the cap", strings.Repeat("s", MaxScopeBytes+1)},
		{"newline", "prod\nus-east-1"},
		{"NUL", "prod\x00"},
		{"not utf-8", string([]byte{0xff, 0xfe, 0x00})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.SaveRDSCheckpoint(tc.scope, []byte(`{"x":1}`))
			if !errors.Is(err, ErrInvalidScope) {
				t.Fatalf("save reported %v, want ErrInvalidScope", err)
			}
			if _, err := s.LoadRDSCheckpoint(tc.scope); !errors.Is(err, ErrInvalidScope) {
				t.Fatalf("load reported %v, want ErrInvalidScope", err)
			}
			// Nothing escaped into the bucket, under any key.
			if keys := storedKeys(t, s, rdsKind); len(keys) != 0 {
				t.Fatalf("a refused scope wrote keys %q", keys)
			}
		})
	}
	// Exactly at the cap is storable: the boundary belongs to the legal side.
	atCap := strings.Repeat("s", MaxScopeBytes)
	if err := s.SaveRDSCheckpoint(atCap, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("a scope of exactly %d bytes was refused: %v", MaxScopeBytes, err)
	}
}

// TestARecordMovedToAnotherKeyIsRefused is the second collision defence, the
// one that does not depend on the key encoding being right. It fires when
// something other than this package files a record under the wrong scope.
func TestARecordMovedToAnotherKeyIsRefused(t *testing.T) {
	s := open(t)
	if err := s.SaveRDSCheckpoint("eu-west-1", rdsBlob("eu-west-1", []int{0, 2, 30})); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		raw = append(raw, tx.Bucket(bucketRDSCheckpoints).Get([]byte("eu-west-1"))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	putCheckpointRaw(t, s, rdsKind, "us-east-1", raw)
	_, err := s.LoadRDSCheckpoint("us-east-1")
	if !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("one region's history was served as another's: %v", err)
	}
	if !strings.Contains(err.Error(), "eu-west-1") {
		t.Fatalf("refusal does not name the scope it actually holds: %v", err)
	}
}

// TestACheckpointFromTheOtherBucketIsRefused: a proposals envelope filed in
// the rds bucket is not an rds checkpoint, and handing its bytes to
// rds.Domain.Restore would be a decode error at best.
func TestACheckpointFromTheOtherBucketIsRefused(t *testing.T) {
	s := open(t)
	if err := s.SaveProposals([]byte(`{"version":1,"records":[]}`)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.db.View(func(tx *bolt.Tx) error {
		raw = append(raw, tx.Bucket(bucketProposals).Get([]byte(proposalsScope))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	putCheckpointRaw(t, s, rdsKind, "brain", raw)
	_, err := s.LoadRDSCheckpoint("brain")
	if !errors.Is(err, ErrCorruptCheckpoint) || !strings.Contains(err.Error(), `"proposals"`) {
		t.Fatalf("a proposals envelope was accepted as an rds one: %v", err)
	}
}

// ---- bounds ----

// TestRealKindsRefuseOversizeAtWrite asserts the boundary on the two shipped
// caps, one byte over each, and that a refused save leaves the previous
// checkpoint intact rather than half-writing one.
func TestRealKindsRefuseOversizeAtWrite(t *testing.T) {
	s := open(t)
	prev := []byte(`{"version":1,"records":[]}`)
	if err := s.SaveProposals(prev); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), MaxProposalsCheckpointBytes+1)
	err := s.SaveProposals(big)
	if !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("an oversize proposal store was stored: %v", err)
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("refusal does not name the cap: %v", err)
	}
	got, err := s.LoadProposals()
	if err != nil || !bytes.Equal(got, prev) {
		t.Fatalf("a refused save damaged the previous checkpoint: %q %v", got, err)
	}
	big = nil

	rdsBig := bytes.Repeat([]byte("x"), MaxRDSCheckpointBytes+1)
	if err := s.SaveRDSCheckpoint("prod", rdsBig); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("an oversize rds checkpoint was stored: %v", err)
	}
	if _, err := s.LoadRDSCheckpoint("prod"); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("a refused save left something behind: %v", err)
	}
}

// TestDecompressionBombIsRefusedAtRead is the read-side half of the ceiling.
// The write side cannot help here: this record was never written through
// SaveRDSCheckpoint. A few hundred bytes of gzip in a bbolt value expand
// without limit, and this process is meant to run unattended for months.
func TestDecompressionBombIsRefusedAtRead(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "bomb", 4096, 4)
	bomb := gzipOf(t, bytes.Repeat([]byte{0}, k.max*64))
	if len(bomb) > 2048 {
		t.Fatalf("test bomb is not compressed (%d bytes), it proves nothing", len(bomb))
	}
	putEnvelope(t, s, k, "prod", checkpointEnvelope{
		Version: 1, Kind: k.name, Scope: "prod", Codec: checkpointCodec, Blob: bomb,
	})
	_, err := k.load(s, "prod")
	if !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("a decompression bomb was expanded: %v", err)
	}
	if !strings.Contains(err.Error(), "decompresses past") {
		t.Fatalf("refusal does not name what happened: %v", err)
	}
	// A payload of exactly the cap still loads: the bound is not off by one.
	putEnvelope(t, s, k, "atcap", checkpointEnvelope{
		Version: 1, Kind: k.name, Scope: "atcap", Codec: checkpointCodec,
		Blob: gzipOf(t, bytes.Repeat([]byte{'a'}, k.max)),
	})
	got, err := k.load(s, "atcap")
	if err != nil || len(got) != k.max {
		t.Fatalf("a payload of exactly the cap was refused: %d bytes, %v", len(got), err)
	}
}

// TestOversizeStoredRecordIsRefusedBeforeItIsCopied bounds the compressed side
// too: without it, a hostile value is copied out of the mmap in full before
// anything looks at it.
func TestOversizeStoredRecordIsRefusedBeforeItIsCopied(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "stored", 1024, 4)
	putCheckpointRaw(t, s, k, "prod", bytes.Repeat([]byte("x"), k.maxStored()+1))
	if _, err := k.load(s, "prod"); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("an oversize stored record was read: %v", err)
	}
}

func TestEmptyCheckpointIsRefusedAtWrite(t *testing.T) {
	s := open(t)
	if err := s.SaveProposals(nil); !errors.Is(err, ErrEmptyCheckpoint) {
		t.Fatalf("an empty proposal snapshot was stored: %v", err)
	}
	if err := s.SaveRDSCheckpoint("prod", []byte{}); !errors.Is(err, ErrEmptyCheckpoint) {
		t.Fatalf("an empty rds checkpoint was stored: %v", err)
	}
	// It must stay a cold start, not become a checkpoint carrying nothing.
	if _, err := s.LoadProposals(); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("a refused empty save left state: %v", err)
	}
}

// TestScopeCapRefusesNewScopesAndKeepsUpdatingOldOnes: the cap must fall on
// the newcomer. A cap that stopped the fleet already being observed correctly
// would turn a mis-derived scope into a total outage of the growth finding.
func TestScopeCapRefusesNewScopesAndKeepsUpdatingOldOnes(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "cap", 4096, 3)
	for i := 0; i < 3; i++ {
		if err := k.save(s, fmt.Sprintf("scope-%d", i), []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	err := k.save(s, "scope-3", []byte(`{"v":1}`))
	if !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("the scope cap did not hold: %v", err)
	}
	if !strings.Contains(err.Error(), "cap is 3") {
		t.Fatalf("refusal does not name the cap: %v", err)
	}
	// Existing scopes keep working.
	if err := k.save(s, "scope-1", []byte(`{"v":2}`)); err != nil {
		t.Fatalf("an existing scope stopped updating at the cap: %v", err)
	}
	got, err := k.load(s, "scope-1")
	if err != nil || string(got) != `{"v":2}` {
		t.Fatalf("existing scope: %q %v", got, err)
	}
	// Freeing a slot lets the newcomer in.
	if err := k.remove(s, "scope-0"); err != nil {
		t.Fatal(err)
	}
	if err := k.save(s, "scope-3", []byte(`{"v":1}`)); err != nil {
		t.Fatalf("a freed slot was not reusable: %v", err)
	}
}

func TestDeleteReportsWhetherItFreedASlot(t *testing.T) {
	s := open(t)
	if err := s.SaveRDSCheckpoint("prod", rdsBlob("prod", []int{0, 1})); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRDSCheckpoint("prod"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRDSCheckpoint("prod"); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("deleting an absent scope reported %v, want ErrNoCheckpoint", err)
	}
	if _, err := s.LoadRDSCheckpoint("prod"); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("a deleted scope: %v", err)
	}
}

// ---- round trip and framing ----

func TestRDSCheckpointsRoundTripBitExactAcrossScopes(t *testing.T) {
	s := open(t)
	// Irregular histories: uneven gaps, differing lengths, one single-sample.
	want := map[string][]byte{
		"111122223333/us-east-1": rdsBlob("111122223333/us-east-1", []int{0, 1, 4, 5, 19, 20, 41}),
		"111122223333/eu-west-1": rdsBlob("111122223333/eu-west-1", []int{0, 13}),
		"999988887777/us-east-1": rdsBlob("999988887777/us-east-1", []int{7}),
		"sandbox":                rdsBlob("sandbox", []int{0, 2, 3, 3, 9, 30, 31, 32, 90}),
	}
	for scope, blob := range want {
		if err := s.SaveRDSCheckpoint(scope, blob); err != nil {
			t.Fatal(err)
		}
	}
	for scope, blob := range want {
		got, err := s.LoadRDSCheckpoint(scope)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, blob) {
			t.Fatalf("scope %q round-tripped to\n %s\nwant\n %s", scope, got, blob)
		}
	}
	scopes, err := s.RDSCheckpointScopes()
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != len(want) {
		t.Fatalf("scopes %q", scopes)
	}
	for i := 1; i < len(scopes); i++ {
		if scopes[i-1] >= scopes[i] {
			t.Fatalf("scopes are not in ascending order: %q", scopes)
		}
	}
	// Replacing one scope leaves the others byte-identical.
	next := rdsBlob("sandbox", []int{0, 2, 3, 3, 9, 30, 31, 32, 90, 91})
	if err := s.SaveRDSCheckpoint("sandbox", next); err != nil {
		t.Fatal(err)
	}
	for scope, blob := range want {
		if scope == "sandbox" {
			blob = next
		}
		got, err := s.LoadRDSCheckpoint(scope)
		if err != nil || !bytes.Equal(got, blob) {
			t.Fatalf("scope %q after a neighbour's rewrite: %s (%v)", scope, got, err)
		}
	}
}

// TestBinaryAndNonUTF8BlobsSurviveIntact: the blob is opaque bytes, not a
// string. A framing that round-tripped it through a string type would replace
// invalid UTF-8 with U+FFFD and hand the producer back something it did not
// write.
func TestBinaryAndNonUTF8BlobsSurviveIntact(t *testing.T) {
	s := open(t)
	blob := make([]byte, 256)
	for i := range blob {
		blob[i] = byte(i)
	}
	if err := s.SaveRDSCheckpoint("bin", blob); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRDSCheckpoint("bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("a binary blob came back changed: %x", got)
	}
}

// TestFramingIsDeterministic: identical state must frame to identical bytes,
// or every housekeeping tick rewrites the same page for no reason and no
// caller can compare two checkpoints without decoding them. encodeSnapshot
// relies on the same property.
func TestFramingIsDeterministic(t *testing.T) {
	s := open(t)
	blob := rdsBlob("prod", []int{0, 5, 9})
	var first []byte
	for i := 0; i < 3; i++ {
		if err := s.SaveRDSCheckpoint("prod", blob); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err := s.db.View(func(tx *bolt.Tx) error {
			raw = append(raw, tx.Bucket(bucketRDSCheckpoints).Get([]byte("prod"))...)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = raw
			continue
		}
		if !bytes.Equal(first, raw) {
			t.Fatal("re-saving identical state produced different stored bytes")
		}
	}
}

func TestProposalsAndRDSBucketsAreIndependent(t *testing.T) {
	s := open(t)
	props := []byte(`{"version":1,"ttlSeconds":86400,"records":[]}`)
	if err := s.SaveProposals(props); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRDSCheckpoint("brain", rdsBlob("brain", []int{0, 1})); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadProposals()
	if err != nil || !bytes.Equal(got, props) {
		t.Fatalf("the rds bucket disturbed the proposals bucket: %q %v", got, err)
	}
	// The two kinds share a key name here on purpose: same key, different
	// bucket, and neither can see the other.
	gotRDS, err := s.LoadRDSCheckpoint("brain")
	if err != nil || !bytes.Equal(gotRDS, rdsBlob("brain", []int{0, 1})) {
		t.Fatalf("rds under the proposals key: %q %v", gotRDS, err)
	}
}

func TestCheckpointsSurviveReopen(t *testing.T) {
	s := open(t)
	path := s.db.Path()
	props := []byte(`{"version":1,"ttlSeconds":86400,"records":[]}`)
	blob := rdsBlob("prod/us-east-1", []int{0, 3, 17})
	if err := s.SaveProposals(props); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRDSCheckpoint("prod/us-east-1", blob); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.LoadProposals()
	if err != nil || !bytes.Equal(got, props) {
		t.Fatalf("proposals did not survive a restart: %q %v", got, err)
	}
	gotRDS, err := reopened.LoadRDSCheckpoint("prod/us-east-1")
	if err != nil || !bytes.Equal(gotRDS, blob) {
		t.Fatalf("rds checkpoint did not survive a restart: %q %v", gotRDS, err)
	}
}

// TestConcurrentCheckpointAccess runs under -race. pkg/store is used from a
// running brain: the housekeeping timer writes while an HTTP handler reads,
// and the collector loop writes a different scope at the same time. The
// concurrency contract is the precedent's — bbolt serializes writers, these
// methods hold no state of their own — and the assertion that matters is that
// a reader never sees a blend of two writes.
func TestConcurrentCheckpointAccess(t *testing.T) {
	s := open(t)
	scopes := []string{"a", "b", "a/b", "café"}
	versions := make([][]byte, 8)
	for i := range versions {
		versions[i] = rdsBlob("shared", []int{0, i, i * 3})
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				scope := scopes[(i+j)%len(scopes)]
				if err := s.SaveRDSCheckpoint(scope, rdsBlob(scope, []int{0, j})); err != nil {
					t.Errorf("save %q: %v", scope, err)
					return
				}
				if err := s.SaveRDSCheckpoint("shared", versions[j%len(versions)]); err != nil {
					t.Errorf("save shared: %v", err)
					return
				}
				got, err := s.LoadRDSCheckpoint("shared")
				if err != nil {
					t.Errorf("load shared: %v", err)
					return
				}
				whole := false
				for _, v := range versions {
					if bytes.Equal(got, v) {
						whole = true
					}
				}
				if !whole {
					t.Errorf("a concurrent read saw a blend of two writes: %s", got)
					return
				}
				if err := s.SaveProposals(rdsBlob("props", []int{j})); err != nil {
					t.Errorf("save proposals: %v", err)
					return
				}
				if _, err := s.LoadProposals(); err != nil {
					t.Errorf("load proposals: %v", err)
					return
				}
				if _, err := s.RDSCheckpointScopes(); err != nil {
					t.Errorf("scopes: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestNilSnapshotterIsRefused — SaveProposalsFrom is the one call that takes an
// interface, and a nil one must not panic inside a bbolt transaction.
func TestNilSnapshotterIsRefused(t *testing.T) {
	s := open(t)
	if err := s.SaveProposalsFrom(nil); err == nil {
		t.Fatal("a nil snapshotter was accepted")
	}
}

// failingSnapshotter stands in for a producer whose own Snapshot fails.
type failingSnapshotter struct{}

func (failingSnapshotter) Snapshot() ([]byte, error) { return nil, errors.New("boom") }

func TestSnapshotterFailureIsNotStoredAsAnEmptyCheckpoint(t *testing.T) {
	s := open(t)
	prev := []byte(`{"version":1,"records":[]}`)
	if err := s.SaveProposals(prev); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProposalsFrom(failingSnapshotter{}); err == nil {
		t.Fatal("a failed snapshot was stored")
	}
	got, err := s.LoadProposals()
	if err != nil || !bytes.Equal(got, prev) {
		t.Fatalf("a failed snapshot damaged the stored one: %q %v", got, err)
	}
}

// TestEnvelopeWithNoBlobIsRefused: a well-formed envelope carrying nothing is
// the one shape that could still read back as "a checkpoint exists and it is
// empty". SaveProposals/SaveRDSCheckpoint refuse an empty blob at write, and
// the reader refuses one that arrived any other way, so an empty checkpoint
// has no representation at either end.
func TestEnvelopeWithNoBlobIsRefused(t *testing.T) {
	s := open(t)
	k := smallKind(t, s, "noblob", 1024, 4)
	putEnvelope(t, s, k, "prod", checkpointEnvelope{
		Version: 1, Kind: k.name, Scope: "prod", Codec: checkpointCodec,
	})
	_, err := k.load(s, "prod")
	if !errors.Is(err, ErrCorruptCheckpoint) {
		t.Fatalf("an empty envelope reported %v, want ErrCorruptCheckpoint", err)
	}
	if errors.Is(err, ErrNoCheckpoint) {
		t.Fatal("an envelope carrying no blob was reported as a cold start")
	}
}

// TestNothingHereApprovesOrApplies states the actuator prohibition over this
// package's own AST rather than over a comment about it.
//
// pkg/ec2's and pkg/rds's actuators are deliberately unreachable from the
// binary. Persisting a proposal is not approving one, and a persistence layer
// is exactly where that line would erode first — a "SaveApproval", a
// "MarkApplied" convenience, an import of the state machine so a record could
// be "repaired" on load. None of those exist, and this fails if one appears.
func TestNothingHereApprovesOrApplies(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := pkgs["store"]
	if !ok {
		t.Fatal("package store did not parse")
	}
	// Names that would mean this package had grown a verb.
	banned := []string{"approve", "apply", "actuate", "execute", "resize", "reboot", "modify"}
	// Packages whose actuators must stay out of this one's import graph, plus
	// the state machine itself: pkg/store frames bytes and must never be able
	// to construct, transition or repair a proposal record.
	bannedImports := []string{
		`"github.com/agenticode/kilter/pkg/ec2"`,
		`"github.com/agenticode/kilter/pkg/rds"`,
		`"github.com/agenticode/kilter/pkg/whatif"`,
	}
	for name, file := range pkg.Files {
		for _, imp := range file.Imports {
			for _, bad := range bannedImports {
				if imp.Path.Value == bad {
					t.Errorf("%s imports %s", name, bad)
				}
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			lower := strings.ToLower(fn.Name.Name)
			for _, b := range banned {
				if strings.Contains(lower, b) {
					t.Errorf("%s declares %s, which reads as a verb this package must not have", name, fn.Name.Name)
				}
			}
		}
	}
}
