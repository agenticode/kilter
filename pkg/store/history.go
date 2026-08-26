package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/agenticode/kilter/pkg/model"
)

// Time-keyed snapshot history — the persistence `kilter backtest --cluster`
// was refused for (cmd/WIRING-FINDINGS.md §6.2).
//
// SaveSnapshot/LoadSnapshot keep exactly one snapshot per cluster, keyed by
// cluster. Replay needs topology OVER TIME, because recommend.ObserveSnapshot
// and plan.Build both take a *model.ClusterSnapshot, so this file adds a
// second, time-keyed bucket beside it.
//
// # Why it is not simply "one row per ingest"
//
// At the 5-minute ingest cadence a 30-day window is 8,640 snapshots per
// cluster, and a snapshot of a few hundred nodes and a few thousand pods is
// megabytes of JSON. Storing every tick verbatim is tens of gigabytes per
// cluster: a disk bomb dressed as a feature. Three bounds apply instead, and
// all three are enforced inside the same write transaction:
//
//  1. CADENCE THINNING. At most one snapshot is retained per Cadence bucket
//     (default 1h). Density above that buys nothing a replay can use —
//     backtest's default DecisionInterval is 24h — while span is exactly what
//     a replay is short of, so the cheapest byte is the one not written.
//  2. A COUNT CAP per cluster (default 768 ≈ 32 days at the default cadence).
//  3. A BYTE BUDGET per cluster (default 32 MiB), which is the bound that
//     actually holds: snapshot size varies by three orders of magnitude
//     between a ten-node cluster and a thousand-node one, so a count cap
//     alone bounds rows, not disk.
//
// Records are stored gzip-framed (see snapshotMagic), which is roughly a 10x
// reduction on snapshot JSON and therefore buys ~10x the retained span for
// the same budget. See pkg/api/SUBSTRATE-FINDINGS.md for the worked
// worst-case arithmetic.

// bucketSnapHistory holds "cluster/timestamp" → framed snapshot record. It is
// a separate bucket from bucketSnapshots on purpose: the latest-snapshot
// bucket is read on every brain start and must not have to skip 768 rows per
// cluster to find one.
var bucketSnapHistory = []byte("snapshot-history")

// snapTimeFormat renders the timestamp half of a history key. Fixed width for
// the reason planTimeFormat is fixed width: bbolt orders keys as bytes, and
// time.RFC3339Nano trims trailing zeros, which makes byte order disagree with
// chronological order. Unlike plans, history keys have never been written in
// any other form, so readers may rely on byte order — but they parse anyway,
// because parsing is also what isolates cluster ids containing '/'.
const snapTimeFormat = planTimeFormat

// snapshotMagic frames a stored history record: 4 magic bytes then gzip'd
// JSON. A magic number rather than a JSON envelope because base64-ing the
// compressed bytes into a JSON string would give back a third of what the
// compression won. An unrecognized prefix is an explicit error — a record
// written by a future encoding is never guessed at.
const snapshotMagic = "KSN1"

// maxSnapshotRecordBytes bounds one stored (compressed) record. A snapshot
// bigger than this is refused at the door rather than written, because a
// single record that cannot be read back within a sane allocation is a row
// that poisons every later scan of the cluster.
const maxSnapshotRecordBytes = 8 << 20

// maxSnapshotDecodedBytes bounds decompression. Without it a hand-crafted
// 8 MiB record could inflate to gigabytes — the classic zip bomb, aimed at a
// process that is meant to run unattended for months.
const maxSnapshotDecodedBytes = 256 << 20

// SnapshotRetention is the history's bound. Every field is a hard limit, not
// a hint, and SetSnapshotRetention rejects a configuration that would let the
// history grow without one.
type SnapshotRetention struct {
	// Cadence is the minimum spacing between retained snapshots. Snapshots
	// landing in a bucket that already holds one are dropped, unless they
	// carry the exact timestamp of the stored one (in which case they replace
	// it, so a replay of the same history is idempotent). Zero retains every
	// distinct timestamp — bounded only by the two caps below.
	Cadence time.Duration
	// MaxPerCluster caps retained snapshots per cluster.
	MaxPerCluster int
	// MaxBytesPerCluster caps the stored (compressed) bytes per cluster.
	// The newest snapshot is always retained even when it alone exceeds the
	// budget: an empty history is a worse answer than an over-budget one, and
	// maxSnapshotRecordBytes already caps how far over it can go.
	MaxBytesPerCluster int64
	// MaxWindow caps the span Snapshots will answer for. A caller asking for
	// ten years is refused by name rather than served a scan whose cost it
	// did not intend.
	MaxWindow time.Duration
}

// DefaultSnapshotRetention is the production bound: hourly snapshots, 32 days
// or 32 MiB per cluster, whichever binds first, over a window no wider than
// 400 days (pkg/evidence's longest retention, so the two substrates can be
// queried over the same window).
func DefaultSnapshotRetention() SnapshotRetention {
	return SnapshotRetention{
		Cadence:            time.Hour,
		MaxPerCluster:      768,
		MaxBytesPerCluster: 32 << 20,
		MaxWindow:          400 * 24 * time.Hour,
	}
}

func (r SnapshotRetention) validate() error {
	if r.Cadence < 0 {
		return fmt.Errorf("store: snapshot retention Cadence=%v must not be negative", r.Cadence)
	}
	if r.MaxPerCluster < 1 || r.MaxPerCluster > 1<<20 {
		return fmt.Errorf("store: snapshot retention MaxPerCluster=%d outside [1, %d]", r.MaxPerCluster, 1<<20)
	}
	if r.MaxBytesPerCluster < 1024 {
		return fmt.Errorf("store: snapshot retention MaxBytesPerCluster=%d below the 1024-byte floor", r.MaxBytesPerCluster)
	}
	if r.MaxWindow <= 0 {
		return fmt.Errorf("store: snapshot retention MaxWindow=%v must be positive", r.MaxWindow)
	}
	return nil
}

// retentionState guards the mutable retention setting. It is a separate mutex
// from bbolt's own locking because it protects a plain struct field, not the
// file.
type retentionState struct {
	mu sync.RWMutex
	r  SnapshotRetention
}

func (s *Store) retention() SnapshotRetention {
	s.snapRetention.mu.RLock()
	defer s.snapRetention.mu.RUnlock()
	return s.snapRetention.r
}

// SnapshotRetention returns the history bound currently in force.
func (s *Store) SnapshotRetention() SnapshotRetention { return s.retention() }

// SetSnapshotRetention replaces the history bound. It does not retroactively
// prune: a tightened bound takes effect on the next SaveSnapshotAt for a
// cluster, which is the same moment every other bound in this package is
// applied.
func (s *Store) SetSnapshotRetention(r SnapshotRetention) error {
	if err := r.validate(); err != nil {
		return err
	}
	s.snapRetention.mu.Lock()
	defer s.snapRetention.mu.Unlock()
	s.snapRetention.r = r
	return nil
}

func snapKey(cluster string, ts time.Time) []byte {
	return []byte(cluster + "/" + ts.UTC().Format(snapTimeFormat))
}

// parseSnapTime decodes the timestamp half of a history key. Requiring the
// suffix to be exactly one instant is what isolates cluster ids containing
// '/' (EKS ARNs do): scanning cluster "a/b" from prefix "a/" leaves the
// suffix "b/<ts>", which is not an instant, so a sibling's history is neither
// read nor pruned by its parent.
func parseSnapTime(suffix []byte) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339Nano, string(suffix))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// forEachSnap visits a cluster's history keys in stored byte order, skipping
// keys whose suffix is not a timestamp. k and v alias bbolt's mmap and are
// only valid inside the enclosing transaction.
func forEachSnap(b *bolt.Bucket, cluster string, fn func(k, v []byte, ts time.Time) error) error {
	prefix := []byte(cluster + "/")
	c := b.Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		ts, ok := parseSnapTime(k[len(prefix):])
		if !ok {
			continue
		}
		if err := fn(k, v, ts); err != nil {
			return err
		}
	}
	return nil
}

// encodeSnapshot frames a snapshot: magic, then gzip'd JSON.
func encodeSnapshot(snap *model.ClusterSnapshot) ([]byte, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(snapshotMagic)
	// Default compression, default (zero) header fields: gzip output is a
	// function of the input alone, so the same snapshot always frames to the
	// same bytes and a re-save is a no-op write rather than a diff.
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeSnapshot reverses encodeSnapshot under a decompression bound.
func decodeSnapshot(rec []byte) (*model.ClusterSnapshot, error) {
	if len(rec) < len(snapshotMagic) || string(rec[:len(snapshotMagic)]) != snapshotMagic {
		return nil, fmt.Errorf("record is not a %s snapshot frame", snapshotMagic)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec[len(snapshotMagic):]))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	// +1 so an input of exactly the cap is distinguishable from one over it.
	raw, err := io.ReadAll(io.LimitReader(zr, maxSnapshotDecodedBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSnapshotDecodedBytes {
		return nil, fmt.Errorf("record decompresses beyond the %d-byte bound", maxSnapshotDecodedBytes)
	}
	var snap model.ClusterSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// SaveSnapshotAt appends a snapshot to the cluster's time-keyed history and
// applies the retention bound in the same transaction.
//
// Three behaviours are worth stating because callers depend on them:
//
//   - Cadence thinning is SILENT and deliberate: a snapshot dropped because
//     its bucket is taken is not an error, it is the policy working. The
//     ingest path calls this every five minutes and must not log a failure
//     eleven times an hour.
//   - Re-saving the exact stored timestamp REPLACES it. Timestamps are unique
//     per cluster by construction, which is what backtest.SnapshotSource
//     requires ("a time series with two values at one instant has no defined
//     replay order").
//   - Saving a snapshot older than every retained one, with the history
//     already full, prunes the snapshot just written. Same rule as SavePlan:
//     the bound is over the history, not over the arrival order.
func (s *Store) SaveSnapshotAt(snap *model.ClusterSnapshot) error {
	if snap == nil {
		return fmt.Errorf("store: nil snapshot")
	}
	if err := validateClusterID(snap.ClusterID); err != nil {
		return err
	}
	if snap.Timestamp.IsZero() {
		// A zero timestamp is not a point on a timeline. Refusing beats
		// substituting a clock: this package has none, and a history whose
		// keys depend on when the process happened to run is not replayable.
		return fmt.Errorf("store: snapshot history %s: a snapshot needs a timestamp", snap.ClusterID)
	}
	ts := snap.Timestamp.UTC()
	key := snapKey(snap.ClusterID, ts)
	if got, ok := parseSnapTime(key[len(snap.ClusterID)+1:]); !ok || !got.Equal(ts) {
		return fmt.Errorf("store: snapshot history %s: timestamp %v is not representable as a key",
			snap.ClusterID, snap.Timestamp)
	}
	rec, err := encodeSnapshot(snap)
	if err != nil {
		return fmt.Errorf("store: snapshot history %s: %w", snap.ClusterID, err)
	}
	if len(rec) > maxSnapshotRecordBytes {
		return fmt.Errorf("store: snapshot history %s: record is %d bytes, over the %d-byte cap",
			snap.ClusterID, len(rec), maxSnapshotRecordBytes)
	}
	ret := s.retention()
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapHistory)
		if err != nil {
			return err
		}
		if ret.Cadence > 0 {
			taken, err := bucketTaken(b, snap.ClusterID, ts, ret.Cadence)
			if err != nil {
				return err
			}
			if taken {
				return nil
			}
		}
		if err := b.Put(key, rec); err != nil {
			return err
		}
		return pruneSnapHistory(b, snap.ClusterID, ret)
	}); err != nil {
		return fmt.Errorf("store: save snapshot history %s: %w", snap.ClusterID, err)
	}
	return nil
}

// bucketTaken reports whether the cadence bucket containing ts already holds
// a retained snapshot at a DIFFERENT instant. An exact-timestamp match is not
// "taken": it is a replay of a snapshot already stored, and replacing it in
// place keeps re-ingesting the same history idempotent.
func bucketTaken(b *bolt.Bucket, cluster string, ts time.Time, cadence time.Duration) (bool, error) {
	start := ts.Truncate(cadence)
	end := start.Add(cadence)
	taken := false
	err := forEachSnap(b, cluster, func(_, _ []byte, got time.Time) error {
		if got.Before(start) || !got.Before(end) {
			return nil
		}
		if !got.Equal(ts) {
			taken = true
		}
		return nil
	})
	return taken, err
}

// pruneSnapHistory enforces the count cap and the byte budget, oldest first.
// It runs inside the caller's write transaction so the bound is never
// observable as violated.
func pruneSnapHistory(b *bolt.Bucket, cluster string, ret SnapshotRetention) error {
	type entry struct {
		key   []byte
		ts    time.Time
		bytes int64
	}
	var entries []entry
	var total int64
	if err := forEachSnap(b, cluster, func(k, v []byte, ts time.Time) error {
		n := int64(len(k) + len(v))
		entries = append(entries, entry{append([]byte(nil), k...), ts, n})
		total += n
		return nil
	}); err != nil {
		return err
	}
	// Prune by timestamp, not by key bytes: the two agree for every key this
	// package writes, but sorting explicitly means a hand-edited or
	// externally written key cannot reorder the eviction.
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].ts.Equal(entries[j].ts) {
			return entries[i].ts.Before(entries[j].ts)
		}
		return bytes.Compare(entries[i].key, entries[j].key) < 0
	})
	drop := 0
	for drop < len(entries) && len(entries)-drop > ret.MaxPerCluster {
		total -= entries[drop].bytes
		drop++
	}
	// Always leave one: a byte budget smaller than a single record must yield
	// the newest snapshot, not an empty history.
	for drop < len(entries)-1 && total > ret.MaxBytesPerCluster {
		total -= entries[drop].bytes
		drop++
	}
	for _, e := range entries[:drop] {
		if err := b.Delete(e.key); err != nil {
			return err
		}
	}
	return nil
}

// Snapshots returns every retained snapshot for a cluster whose Timestamp
// lies in [from, to), oldest first.
//
// It is the adapter backtest.SnapshotSource asks for — the signature is that
// interface's, satisfied structurally so pkg/store keeps its place at the
// bottom of the dependency order and never imports the scoring harness. The
// half-open window and the uniqueness of timestamps are both that interface's
// contract.
//
// A window wider than the retention's MaxWindow is REFUSED. Retention already
// bounds the answer, so this is not an OOM guard; it is a guard against a
// caller who typed the wrong unit getting a plausible-looking empty answer
// back instead of being told the query was nonsense.
func (s *Store) Snapshots(cluster string, from, to time.Time) ([]*model.ClusterSnapshot, error) {
	if err := validateClusterID(cluster); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, fmt.Errorf("store: snapshot window [%s, %s) is empty or inverted",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	}
	ret := s.retention()
	if span := to.Sub(from); span > ret.MaxWindow {
		return nil, fmt.Errorf("store: snapshot window spans %v, over the %v cap; ask for a narrower window",
			span, ret.MaxWindow)
	}
	fromUTC, toUTC := from.UTC(), to.UTC()
	var out []*model.ClusterSnapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapHistory)
		if err != nil {
			return err
		}
		return forEachSnap(b, cluster, func(_, v []byte, ts time.Time) error {
			if ts.Before(fromUTC) || !ts.Before(toUTC) {
				return nil
			}
			snap, err := decodeSnapshot(v)
			if err != nil {
				return fmt.Errorf("at %s: %w", ts.Format(time.RFC3339Nano), err)
			}
			// The same identity check LoadSnapshot makes, for the same
			// reason: a record whose ClusterID disagrees with the key it was
			// filed under would hand a replay another cluster's topology
			// under the name it asked for.
			if snap.ClusterID != cluster {
				return fmt.Errorf("at %s: record holds cluster %q", ts.Format(time.RFC3339Nano), snap.ClusterID)
			}
			out = append(out, snap)
			return nil
		})
	})
	if err != nil {
		// A partially decoded history must not escape as if it were whole: a
		// backtest over "the snapshots that happened to parse" is exactly the
		// confident-looking wrong answer this substrate exists to prevent.
		return nil, fmt.Errorf("store: snapshots %s: %w", cluster, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

// SnapshotHistoryCount returns how many snapshots are retained for a cluster.
// Keys that do not decode as this cluster's — a sibling id sharing a prefix,
// or a corrupt key — are not counted.
func (s *Store) SnapshotHistoryCount(cluster string) (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapHistory)
		if err != nil {
			return err
		}
		return forEachSnap(b, cluster, func(_, _ []byte, _ time.Time) error {
			n++
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("store: snapshot history count %s: %w", cluster, err)
	}
	return n, nil
}

// SnapshotHistoryBytes returns the stored bytes (keys plus framed values) a
// cluster's history occupies — the quantity MaxBytesPerCluster bounds, so a
// caller can assert the bound rather than trust it.
func (s *Store) SnapshotHistoryBytes(cluster string) (int64, error) {
	var n int64
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapHistory)
		if err != nil {
			return err
		}
		return forEachSnap(b, cluster, func(k, v []byte, _ time.Time) error {
			n += int64(len(k) + len(v))
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("store: snapshot history bytes %s: %w", cluster, err)
	}
	return n, nil
}
