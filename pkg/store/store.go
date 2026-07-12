// Package store persists the brain's learned state in a single embedded
// bbolt file: recommender histograms, the latest snapshot per cluster, and a
// bounded plan history. Everything is JSON-encoded — debuggable with one call
// to `bbolt` CLI or `kilter brain --dump`.
package store

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

var (
	bucketRecommender = []byte("recommender") // cluster → []CheckpointState
	bucketSnapshots   = []byte("snapshots")   // cluster → ClusterSnapshot
	bucketPlans       = []byte("plans")       // cluster/RFC3339Nano → Plan
)

// PlanHistoryLimit bounds retained plans per cluster.
const PlanHistoryLimit = 50

// Store is a bbolt-backed persistence layer. Safe for concurrent use.
type Store struct {
	db *bolt.DB
}

// Open creates/opens the store file.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketRecommender, bucketSnapshots, bucketPlans} {
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
	return &Store{db: db}, nil
}

// Close releases the file.
func (s *Store) Close() error { return s.db.Close() }

func put(tx *bolt.Tx, bucket []byte, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(key), raw)
}

// SaveRecommenderState persists checkpointed learning for a cluster.
func (s *Store) SaveRecommenderState(cluster string, states []recommend.CheckpointState) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return put(tx, bucketRecommender, cluster, states)
	})
}

// LoadRecommenderState returns nil, nil when the cluster has no saved state.
func (s *Store) LoadRecommenderState(cluster string) ([]recommend.CheckpointState, error) {
	var out []recommend.CheckpointState
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketRecommender).Get([]byte(cluster))
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

// SaveSnapshot stores the latest snapshot for its cluster.
func (s *Store) SaveSnapshot(snap *model.ClusterSnapshot) error {
	if snap == nil || snap.ClusterID == "" {
		return fmt.Errorf("store: snapshot must have a cluster id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return put(tx, bucketSnapshots, snap.ClusterID, snap)
	})
}

// LoadSnapshot returns nil, nil when the cluster has no snapshot.
func (s *Store) LoadSnapshot(cluster string) (*model.ClusterSnapshot, error) {
	var out *model.ClusterSnapshot
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketSnapshots).Get([]byte(cluster))
		if raw == nil {
			return nil
		}
		out = &model.ClusterSnapshot{}
		return json.Unmarshal(raw, out)
	})
	if err != nil {
		return nil, fmt.Errorf("store: load snapshot %s: %w", cluster, err)
	}
	return out, nil
}

// Clusters lists cluster ids that have snapshots.
func (s *Store) Clusters() ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSnapshots).ForEach(func(k, _ []byte) error {
			out = append(out, string(k))
			return nil
		})
	})
	return out, err
}

func planKey(cluster string, ts time.Time) []byte {
	return []byte(cluster + "/" + ts.UTC().Format(time.RFC3339Nano))
}

// SavePlan appends a plan to the cluster's history and prunes old entries.
func (s *Store) SavePlan(p *plan.Plan) error {
	if p == nil || p.ClusterID == "" {
		return fmt.Errorf("store: plan must have a cluster id")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPlans)
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if err := b.Put(planKey(p.ClusterID, p.CreatedAt), raw); err != nil {
			return err
		}
		// Prune: keep the newest PlanHistoryLimit for this cluster.
		prefix := []byte(p.ClusterID + "/")
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for len(keys) > PlanHistoryLimit {
			if err := b.Delete(keys[0]); err != nil {
				return err
			}
			keys = keys[1:]
		}
		return nil
	})
}

// LatestPlan returns nil, nil when the cluster has no plans.
func (s *Store) LatestPlan(cluster string) (*plan.Plan, error) {
	var out *plan.Plan
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPlans)
		prefix := []byte(cluster + "/")
		c := b.Cursor()
		var lastVal []byte
		for k, v := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, v = c.Next() {
			lastVal = v
		}
		if lastVal == nil {
			return nil
		}
		out = &plan.Plan{}
		return json.Unmarshal(lastVal, out)
	})
	if err != nil {
		return nil, fmt.Errorf("store: latest plan %s: %w", cluster, err)
	}
	return out, nil
}

// PlanCount returns how many plans are retained for a cluster.
func (s *Store) PlanCount(cluster string) (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		prefix := []byte(cluster + "/")
		c := tx.Bucket(bucketPlans).Cursor()
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			n++
		}
		return nil
	})
	return n, err
}

func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}
