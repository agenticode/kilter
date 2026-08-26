// Package store persists the brain's learned state in a single embedded
// bbolt file: recommender histograms, the latest snapshot per cluster, and a
// bounded plan history. Everything is JSON-encoded — debuggable with one call
// to `bbolt` CLI or `kilter brain --dump`.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

var (
	bucketRecommender = []byte("recommender") // cluster → []CheckpointState
	bucketSnapshots   = []byte("snapshots")   // cluster → ClusterSnapshot
	bucketPlans       = []byte("plans")       // cluster/timestamp → Plan
)

// Four more buckets live in their own files, next to the code that owns them:
// bucketSnapHistory (history.go), bucketEvidence (evidence.go),
// bucketProposals (proposals.go) and bucketRDSCheckpoints (rdscheckpoint.go).

// PlanHistoryLimit bounds retained plans per cluster. Pruning keeps the newest
// PlanHistoryLimit by Plan.CreatedAt, not by insertion order.
const PlanHistoryLimit = 50

// planTimeFormat renders the timestamp half of a plan key: RFC3339 in UTC with
// a fixed nine-digit fraction.
//
// The width is the point. Plan keys are "<cluster>/<timestamp>" and bbolt
// orders keys as bytes, so byte order only agrees with chronological order
// when every timestamp renders to the same width. time.RFC3339Nano trims
// trailing zeros, which breaks that: "…00.5Z" sorts before "…00Z" and
// "…00.23Z" before "…00.2Z", i.e. newer plans sort older. Keys written by
// older builds use the trimmed form; readers parse rather than compare bytes
// (see parsePlanTime), so both encodings order correctly together.
const planTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Store is a bbolt-backed persistence layer. Safe for concurrent use.
type Store struct {
	db *bolt.DB
	// snapRetention bounds the time-keyed snapshot history (history.go).
	snapRetention retentionState
}

// Open creates/opens the store file, creating the buckets if needed. It fails
// rather than blocking forever when another process holds the file lock.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{
			bucketRecommender, bucketSnapshots, bucketPlans,
			bucketSnapHistory, bucketEvidence, bucketProposals, bucketRDSCheckpoints,
		} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init buckets: %w", err)
	}
	s := &Store{db: db}
	s.snapRetention.r = DefaultSnapshotRetention()
	return s, nil
}

// Close releases the file.
func (s *Store) Close() error { return s.db.Close() }

// validateClusterID rejects ids that cannot survive being stored.
//
// Cluster ids are used two ways at once: raw as the bbolt key, and JSON-encoded
// inside the record. encoding/json replaces invalid UTF-8 with U+FFFD, so a
// non-UTF-8 id would write a record whose own ClusterID no longer matches the
// key it lives under — a write that cannot be read back. Refuse it at the door
// instead, where the operator can still see which id was wrong.
func validateClusterID(cluster string) error {
	if cluster == "" {
		return fmt.Errorf("store: cluster id must not be empty")
	}
	if !utf8.ValidString(cluster) {
		return fmt.Errorf("store: cluster id %q is not valid UTF-8", cluster)
	}
	return nil
}

// bucket fetches a bucket Open is expected to have created. A missing one
// means the file was truncated or written by something else — an explicit
// error, never a nil-bucket panic inside a transaction.
func bucket(tx *bolt.Tx, name []byte) (*bolt.Bucket, error) {
	b := tx.Bucket(name)
	if b == nil {
		return nil, fmt.Errorf("store: missing bucket %q", name)
	}
	return b, nil
}

func put(tx *bolt.Tx, bkt []byte, key string, v any) error {
	b, err := bucket(tx, bkt)
	if err != nil {
		return err
	}
	// json.Marshal rejects NaN/±Inf, so a poisoned float aborts the whole
	// transaction and leaves the previously stored value intact rather than
	// half-writing one.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), raw)
}

// SaveRecommenderState persists checkpointed learning for a cluster, replacing
// any previous checkpoint. Writing is all-or-nothing: an unserializable state
// (a NaN histogram weight, say) fails the call and preserves the old value.
func (s *Store) SaveRecommenderState(cluster string, states []recommend.CheckpointState) error {
	if err := validateClusterID(cluster); err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return put(tx, bucketRecommender, cluster, states)
	}); err != nil {
		return fmt.Errorf("store: save recommender %s: %w", cluster, err)
	}
	return nil
}

// LoadRecommenderState returns nil, nil when the cluster has no saved state,
// and an error when the stored checkpoint is unreadable. Individual entries are
// not validated here — recommend.Restore skips the ones it cannot trust.
func (s *Store) LoadRecommenderState(cluster string) ([]recommend.CheckpointState, error) {
	var out []recommend.CheckpointState
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketRecommender)
		if err != nil {
			return err
		}
		raw := b.Get([]byte(cluster))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return nil, fmt.Errorf("store: load recommender %s: %w", cluster, err)
	}
	return out, nil
}

// SaveSnapshot stores a cluster's snapshot, replacing the previous one. Last
// write wins: Timestamp is not compared, so replaying an older snapshot does
// overwrite a newer one. That mirrors the brain's in-memory lastSnap, which
// the restored value has to agree with.
func (s *Store) SaveSnapshot(snap *model.ClusterSnapshot) error {
	if snap == nil {
		return fmt.Errorf("store: nil snapshot")
	}
	if err := validateClusterID(snap.ClusterID); err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return put(tx, bucketSnapshots, snap.ClusterID, snap)
	}); err != nil {
		return fmt.Errorf("store: save snapshot %s: %w", snap.ClusterID, err)
	}
	return nil
}

// LoadSnapshot returns nil, nil when the cluster has no snapshot, and an error
// when the stored record is unreadable or names a different cluster than the
// key it was filed under. It never returns a partially decoded snapshot.
func (s *Store) LoadSnapshot(cluster string) (*model.ClusterSnapshot, error) {
	var out *model.ClusterSnapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapshots)
		if err != nil {
			return err
		}
		raw := b.Get([]byte(cluster))
		if raw == nil {
			return nil
		}
		out = &model.ClusterSnapshot{}
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
		// SaveSnapshot keys on snap.ClusterID, so key and payload always agree
		// when written by this package. A disagreement means the record was
		// truncated, hand-edited or JSON "null"; returning it would hand the
		// caller another cluster's topology under the name it asked for.
		if out.ClusterID != cluster {
			return fmt.Errorf("record holds cluster %q", out.ClusterID)
		}
		return nil
	})
	if err != nil {
		// A half-decoded snapshot must not escape as if it were whole.
		return nil, fmt.Errorf("store: load snapshot %s: %w", cluster, err)
	}
	return out, nil
}

// Clusters lists, in ascending id order, the cluster ids that have snapshots.
// Clusters known only through plans or recommender state are not included.
func (s *Store) Clusters() ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketSnapshots)
		if err != nil {
			return err
		}
		return b.ForEach(func(k, _ []byte) error {
			out = append(out, string(k))
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	return out, nil
}

func planKey(cluster string, ts time.Time) []byte {
	return []byte(cluster + "/" + ts.UTC().Format(planTimeFormat))
}

// parsePlanTime decodes the timestamp half of a plan key. It accepts both the
// fixed-width form written today and the trimmed time.RFC3339Nano form written
// by older builds.
//
// Requiring the suffix to be exactly one instant is also what isolates cluster
// ids that contain '/' — EKS ARNs do. Scanning cluster "a/b" from prefix "a/"
// leaves the suffix "b/<ts>", which is not an instant, so the sibling's plans
// are neither read nor pruned by its parent.
func parsePlanTime(suffix []byte) (time.Time, bool) {
	ts, err := time.Parse(time.RFC3339Nano, string(suffix))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// forEachPlan visits the plan keys belonging to cluster in stored byte order,
// skipping any whose suffix is not a timestamp. k and v alias bbolt's mmap and
// are only valid inside the enclosing transaction — copy what outlives it.
func forEachPlan(b *bolt.Bucket, cluster string, fn func(k, v []byte, ts time.Time) error) error {
	prefix := []byte(cluster + "/")
	c := b.Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		ts, ok := parsePlanTime(k[len(prefix):])
		if !ok {
			continue
		}
		if err := fn(k, v, ts); err != nil {
			return err
		}
	}
	return nil
}

// SavePlan appends a plan to the cluster's history and prunes the oldest
// entries beyond PlanHistoryLimit.
//
// The key is derived from Plan.CreatedAt, so re-saving a plan with an already
// stored CreatedAt replaces it rather than adding a second entry; and saving a
// plan older than every retained one, with the history already full, prunes
// the plan just written.
func (s *Store) SavePlan(p *plan.Plan) error {
	if p == nil {
		return fmt.Errorf("store: nil plan")
	}
	if err := validateClusterID(p.ClusterID); err != nil {
		return err
	}
	key := planKey(p.ClusterID, p.CreatedAt)
	// Refuse to write a key we cannot read back. A year outside [0000,9999]
	// renders wider than planTimeFormat and would not parse, making the plan
	// invisible to LatestPlan and immune to pruning — a silent leak.
	if ts, ok := parsePlanTime(key[len(p.ClusterID)+1:]); !ok || !ts.Equal(p.CreatedAt) {
		return fmt.Errorf("store: plan %s: createdAt %v is not representable as a key", p.ClusterID, p.CreatedAt)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketPlans)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if err := b.Put(key, raw); err != nil {
			return err
		}
		// Prune by CreatedAt, not by key bytes: histories written by older
		// builds mix key widths, and the just-written plan must be counted.
		type entry struct {
			key []byte
			ts  time.Time
		}
		var entries []entry
		if err := forEachPlan(b, p.ClusterID, func(k, _ []byte, ts time.Time) error {
			entries = append(entries, entry{append([]byte(nil), k...), ts})
			return nil
		}); err != nil {
			return err
		}
		if len(entries) <= PlanHistoryLimit {
			return nil
		}
		sort.Slice(entries, func(i, j int) bool {
			if !entries[i].ts.Equal(entries[j].ts) {
				return entries[i].ts.Before(entries[j].ts)
			}
			return bytes.Compare(entries[i].key, entries[j].key) < 0
		})
		// Keys were copied above, so deleting after the scan is safe.
		for _, e := range entries[:len(entries)-PlanHistoryLimit] {
			if err := b.Delete(e.key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("store: save plan %s: %w", p.ClusterID, err)
	}
	return nil
}

// LatestPlan returns the retained plan with the greatest CreatedAt, or
// nil, nil when the cluster has no plans. It errors rather than return a plan
// whose stored ClusterID disagrees with the key it was filed under.
func (s *Store) LatestPlan(cluster string) (*plan.Plan, error) {
	var out *plan.Plan
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketPlans)
		if err != nil {
			return err
		}
		var bestKey, bestVal []byte
		var bestTS time.Time
		var found bool
		if err := forEachPlan(b, cluster, func(k, v []byte, ts time.Time) error {
			// Equal instants can still be two keys when a history spans an
			// encoding change; break the tie on key bytes so the winner is
			// deterministic across calls. Track found separately from bestVal:
			// an externally written empty value must still count as a record,
			// and surface as a decode error rather than as "no plans".
			if !found || ts.After(bestTS) ||
				(ts.Equal(bestTS) && bytes.Compare(k, bestKey) > 0) {
				bestKey, bestVal, bestTS, found = k, v, ts, true
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			return nil
		}
		out = &plan.Plan{}
		if err := json.Unmarshal(bestVal, out); err != nil {
			return err
		}
		// Same identity check as LoadSnapshot, and it matters more here: a plan
		// is a list of evictions and node deletions, so serving one cluster's
		// plan under another cluster's name is an outage, not a bad number.
		if out.ClusterID != cluster {
			return fmt.Errorf("record holds cluster %q", out.ClusterID)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: latest plan %s: %w", cluster, err)
	}
	return out, nil
}

// PlanCount returns how many plans are retained for a cluster. Keys that do not
// decode as this cluster's — a sibling id that merely shares a prefix, or a
// corrupt key — are not counted.
func (s *Store) PlanCount(cluster string) (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		b, err := bucket(tx, bucketPlans)
		if err != nil {
			return err
		}
		return forEachPlan(b, cluster, func(_, _ []byte, _ time.Time) error {
			n++
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("store: plan count %s: %w", cluster, err)
	}
	return n, nil
}
