package api

import (
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
)

// Replaying a cluster's own history.
//
// The time-keyed snapshot bucket in pkg/store is only half of what
// `kilter backtest --cluster` was refused for. The other half is this
// function, and it exists because a populated bucket does NOT by itself make
// the refusal safe to delete.
//
// backtest.Run over an empty or too-short history does not fail. It returns a
// Scorecard with the same shape, the same field names and the same confident
// tone as a real one — `snapshots 0`, `regret $0.00` — and an operator reading
// that has no way to tell it means "nothing was replayed" rather than "the
// policy is perfect". That is strictly worse than the refusal it replaces, so
// the precondition is checked here, once, using backtest's OWN report of how
// much it scored rather than a re-derived predicate that could drift from it.

// MinReplaySnapshots is the floor below which there is no history to replay.
// Two is the arithmetic minimum for anything to have changed; Instants below
// is the real gate.
const MinReplaySnapshots = 2

// ErrNoHistory reports a brain with no persistent store: without one there is
// no snapshot history at all, only the single in-memory latest snapshot.
type ErrNoHistory struct{ Cluster string }

func (e ErrNoHistory) Error() string {
	return fmt.Sprintf("backtest %s: this brain has no persistent store, so no snapshot history is kept; "+
		"start it with a --db path and let it ingest", e.Cluster)
}

// ErrHistoryTooShort reports a history that exists but cannot support a
// replay. It names what is there and what was needed, because "not enough
// history" without numbers is indistinguishable from a bug.
type ErrHistoryTooShort struct {
	Cluster   string
	From, To  time.Time
	Snapshots int
	Instants  int
	Interval  time.Duration
	Horizon   time.Duration
}

func (e ErrHistoryTooShort) Error() string {
	span := e.To.Sub(e.From)
	if e.Snapshots < MinReplaySnapshots {
		return fmt.Sprintf("backtest %s: refused — the retained history holds %d snapshot(s) in [%s, %s); "+
			"scoring that would produce a scorecard shaped exactly like a real one, and its zeros would read "+
			"as a verdict rather than as an empty replay",
			e.Cluster, e.Snapshots, e.From.UTC().Format(time.RFC3339), e.To.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("backtest %s: refused — %d snapshot(s) span %v, which yields no decision instant at a "+
		"%v interval with a %v horizon. Nothing would be replayed, and a scorecard over nothing reads as a "+
		"perfect policy. Widen the window or shorten --interval",
		e.Cluster, e.Snapshots, span.Round(time.Minute), e.Interval, e.Horizon)
}

// Backtest replays a cluster's own retained history through the production
// decision path and scores it.
//
// The policy under test is THIS BRAIN'S policy: the recommender and planner
// configs it actually runs with, so the scorecard answers "how good is what
// is running here" rather than "how good is the default". The evidence
// substrate is this brain's too, which is what lets the harness score real
// OOMKills rather than only counterfactual memory violations.
//
// It refuses rather than returns an empty scorecard. See the file comment.
func (b *Brain) Backtest(cluster string, from, to time.Time, horizon time.Duration, scoring backtest.Config) (*backtest.Scorecard, error) {
	if b.st == nil {
		return nil, ErrNoHistory{Cluster: cluster}
	}
	if err := checkExplainWindow(from, to); err != nil {
		return nil, err
	}
	snaps, err := b.st.Snapshots(cluster, from, to)
	if err != nil {
		return nil, err
	}
	if len(snaps) < MinReplaySnapshots {
		return nil, ErrHistoryTooShort{
			Cluster: cluster, From: from, To: to, Snapshots: len(snaps),
			Interval: scoring.DecisionInterval, Horizon: horizon,
		}
	}
	h := &backtest.Harness{
		Evidence: b.mem,
		// The store is the SnapshotSource: satisfied structurally, so the
		// harness replays the same rows the bucket retained, thinning and all.
		History: b.st,
		Rec:     b.cfg.Recommend,
		Plan:    b.cfg.Plan,
		Catalog: b.catalog,
		Scoring: scoring,
	}
	sc, err := h.Run(cluster, from, to, horizon)
	if err != nil {
		return nil, err
	}
	// backtest's own coverage report, not a predicate reimplemented here: if
	// it scored no instant, there is no scorecard to show.
	if sc.Instants == 0 {
		return nil, ErrHistoryTooShort{
			Cluster: cluster, From: from, To: to,
			Snapshots: sc.Snapshots, Instants: sc.Instants,
			Interval: time.Duration(sc.DecisionIntervalHours * float64(time.Hour)),
			Horizon:  horizon,
		}
	}
	return sc, nil
}
