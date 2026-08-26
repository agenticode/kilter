// Package store_test holds the assertions that pkg/store satisfies the two
// seams other packages named for it, without pkg/store importing either of
// them in production code.
//
// The dependency direction is the point. pkg/evidence documents that "the
// substrate itself never opens a file, a bbolt handle or a socket", and
// pkg/backtest's SnapshotSource sits above the recommender and the planner.
// Both interfaces are satisfied structurally; asserting it here means a
// signature drift is a compile error in this package's own test run, while
// production still has pkg/store importing nothing above pkg/model.
package store_test

import (
	"testing"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/store"
)

var (
	_ backtest.SnapshotSource  = (*store.Store)(nil)
	_ evidence.CheckpointStore = store.EvidenceCheckpointStore{}
	_ evidence.CheckpointStore = (*store.Store)(nil).EvidenceCheckpoints()
)

// TestSeamsAreSatisfied exists so `go test` reports this file's contract
// rather than only failing to build it.
func TestSeamsAreSatisfied(t *testing.T) {
	var src backtest.SnapshotSource = (*store.Store)(nil)
	if src == nil {
		t.Fatal("store.Store no longer satisfies backtest.SnapshotSource")
	}
	var cs evidence.CheckpointStore = store.EvidenceCheckpointStore{}
	if cs == nil {
		t.Fatal("store.EvidenceCheckpointStore no longer satisfies evidence.CheckpointStore")
	}
}
