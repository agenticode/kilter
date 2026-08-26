package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/store"
)

// Answering from a brain's own database.
//
// `kilter backtest --cluster`, `kilter why-cost` and `kilter explain` all
// needed the same thing and none of them had it: the snapshot history and the
// evidence substrate a running brain accumulates. pkg/store now keeps a
// time-keyed history (SaveSnapshotAt/Snapshots) and api.Brain now holds an
// *evidence.Memory, so the three verbs open the database the brain writes and
// ask the brain itself. Written once, here, because three independent
// definitions of "open the brain" is three places for them to disagree.
//
// # This is a SIBLING of the file-backed sources, not a replacement
//
// --kube-snapshot (a recorded snapshot series) and --ledger (a recorded
// ledger) keep working exactly as before and are still the only source that
// needs no database at all. A user with recorded snapshots and no brain is
// unaffected by everything in this file. The two sources answer the same
// question from different substrates and are proven to agree where both can
// answer (TestBrainAndFileSourcesAgreeOnWhyCost).
//
// # Four things an operator has to know, all of them consequences of bbolt
//
//  1. THE DATABASE MUST ALREADY EXIST. bolt.Open creates the file it is given,
//     so a mistyped --db would otherwise produce an empty database and an
//     answer that reads as "this cluster has no history yet" rather than "that
//     path is wrong". openBrainSource stats first and refuses by name.
//  2. THE LOCK IS EXCLUSIVE. bbolt takes a file lock for the process lifetime,
//     and pkg/store opens with a 5-second timeout, so these verbs cannot read
//     the database of a brain that is currently running. That failure is
//     wrapped with what it means rather than surfaced as "timeout".
//  3. OPENING WRITES. store.Open creates missing buckets in an Update
//     transaction, so even these read-only verbs touch the file. Nothing here
//     ingests, plans or actuates.
//  4. THE EVIDENCE IS AS FRESH AS THE LAST CHECKPOINT. The substrate is
//     persisted every BrainConfig.CheckpointEvery snapshots (default 10) and
//     on graceful shutdown; a brain killed between checkpoints loses the tail.
//     The snapshot HISTORY has no such lag — SaveSnapshotAt runs inside every
//     Ingest — so `backtest --cluster` sees everything the retention kept
//     while `why-cost`/`explain` may not see the newest few snapshots' usage.
//
// # No mutating path is reachable from here
//
// This file imports pkg/api, pkg/store, pkg/pricing and pkg/model. It reaches
// no cloud SDK and no actuator: pkg/ec2's and pkg/rds's actuators stay
// unreachable from the binary, which TestNoActuatorIsReachableFromTheBinary
// asserts over the whole command package rather than trusting this comment.

// brainSource is an opened brain database plus the pricing catalog to read it
// with.
//
// The store is held open for the lifetime of the command and the brain is
// built separately, because the two answer different questions: "is there a
// database, and does it know this cluster" is a store read costing one bbolt
// lookup, while restoring a brain replays every cluster's recommender state
// and the whole evidence checkpoint. A mistyped --cluster should not pay for
// the second to be told about the first.
type brainSource struct {
	verb    string
	path    string
	st      *store.Store
	catalog *pricing.Catalog
}

// brainExplainWindow is the trailing span `kilter explain --db` resolves to
// when no --from/--to is given. It mirrors pkg/api's own defaultExplainWindow
// (unexported there) and is resolved against the LATEST INGESTED SNAPSHOT,
// never against a clock. TestExplainOverADatabaseMatchesTheHTTPRoute pins the
// two together by comparing this command's payload against the route's, so a
// change to either side that made them disagree fails rather than drifts.
const brainExplainWindow = 24 * time.Hour

// openBrainSource opens an existing brain database for reading.
//
// verb is the command name, so every refusal below names the command the
// operator actually typed.
func openBrainSource(verb, dbPath string, catalog *pricing.Catalog) (*brainSource, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("%s: --db is required to answer from a brain's own history", verb)
	}
	// Stat before open: bolt.Open would CREATE this path, and a database
	// created by a typo answers "no history for that cluster" — which is true
	// of the empty file it just made and says nothing about the cluster.
	fi, err := os.Stat(dbPath)
	switch {
	case os.IsNotExist(err):
		return nil, fmt.Errorf("%s --db %s: no such database. This verb reads a brain's database and "+
			"does not create one; the brain writes it (kilter brain --db %s). An empty database created "+
			"here would answer \"no history\" for every cluster, which would be a fact about the file "+
			"rather than about the cluster", verb, dbPath, dbPath)
	case err != nil:
		return nil, fmt.Errorf("%s --db %s: %w", verb, dbPath, err)
	case fi.IsDir():
		return nil, fmt.Errorf("%s --db %s: is a directory, not a bbolt database", verb, dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		// The overwhelmingly likely cause is the file lock, and "timeout" on
		// its own does not say so. It is offered rather than asserted because
		// a permission or corruption failure arrives here too, and claiming
		// the wrong cause is worse than naming the likely one.
		return nil, fmt.Errorf("%w\n\nIf that is a timeout: a bbolt database is locked by one process "+
			"at a time, so this cannot read the database of a brain that is currently running. Stop the "+
			"brain, or point --db at a copy of the file", err)
	}
	return &brainSource{verb: verb, path: dbPath, st: st, catalog: catalog}, nil
}

// Close releases the database lock.
func (s *brainSource) Close() error { return s.st.Close() }

// brain restores a brain over the open store.
//
// cfg carries whatever the caller needs to set; the logger is always replaced.
// A restore logs "brain restored" at INFO and the persistence paths log at
// ERROR, and neither belongs on the stderr of a command whose entire output is
// one report — a CLI that prints a log line before its answer teaches the
// operator to ignore log lines.
func (s *brainSource) brain(cfg api.BrainConfig) (*api.Brain, error) {
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := api.NewBrain(cfg, s.catalog, s.st)
	if err != nil {
		return nil, fmt.Errorf("%s --db %s: %w", s.verb, s.path, err)
	}
	return b, nil
}

// requireCluster refuses a cluster id the database has never seen, and returns
// the latest snapshot ingested for it.
//
// This is the command-line form of the 404 the HTTP routes answer with
// (pkg/api/SUBSTRATE-FINDINGS.md §3.2): "never ingested" and "ingested but
// there is not enough evidence" are different facts, and a single message
// covering both sends an operator to look for the wrong problem. The known ids
// are listed because the overwhelmingly common cause is a typo or a
// disagreement about what the agent calls this cluster.
//
// backtest --cluster deliberately does NOT call this: its refusals are
// api.ErrNoHistory and api.ErrHistoryTooShort, which pkg/api owns, and a
// pre-check here would shadow them.
func (s *brainSource) requireCluster(cluster string) (*model.ClusterSnapshot, error) {
	if strings.TrimSpace(cluster) == "" {
		return nil, fmt.Errorf("%s: --cluster is required with --db", s.verb)
	}
	snap, err := s.st.LoadSnapshot(cluster)
	if err != nil {
		return nil, fmt.Errorf("%s --cluster %s: %w", s.verb, cluster, err)
	}
	if snap == nil {
		known, kerr := s.st.Clusters()
		if kerr != nil {
			return nil, fmt.Errorf("%s --cluster %s: %w", s.verb, cluster, kerr)
		}
		list := "none — nothing has been ingested into this database"
		if len(known) > 0 {
			list = strings.Join(known, ", ")
		}
		return nil, fmt.Errorf("%s --cluster %s: no snapshot for that cluster in %s. Known clusters: %s",
			s.verb, cluster, s.path, list)
	}
	return snap, nil
}

// setFlagNames reports which flags were named on the command line, as opposed
// to left at their default value. flag.FlagSet.Visit walks exactly the flags
// that were set, which is the only way to tell an explicit `--days 7` from an
// unmentioned --days — and telling them apart is what lets a command refuse a
// flag it cannot honour instead of ignoring it.
func setFlagNames(fs *flag.FlagSet) map[string]bool {
	out := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { out[f.Name] = true })
	return out
}

// refuseUnusable refuses the flags a source cannot honour, by name.
//
// The alternative is to ignore them, and pkg/backtest's policy loader already
// records why that is unacceptable: a knob that is silently dropped produces a
// report for a configuration nobody ran. The same argument applies to a flag
// that belongs to a different source of history.
func refuseUnusable(verb, why string, set map[string]bool, names ...string) error {
	var named []string
	for _, n := range names {
		if set[n] {
			named = append(named, "--"+n)
		}
	}
	if len(named) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s %s", verb, strings.Join(named, ", "), why)
}
