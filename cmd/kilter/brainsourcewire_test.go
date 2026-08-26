package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/explain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/store"
)

// These tests drive the REAL api.Brain over a REAL bbolt database written by
// the REAL ingest path. Nothing is stubbed: the history under replay is the
// history SaveSnapshotAt retained, thinning and all, and the substrate the
// explanations are built from is the one Ingest populated and checkpointed.
//
// No network and no cloud call anywhere. The one HTTP comparison below uses
// httptest.NewRecorder against Brain.Handler() in-process, so not even a
// loopback socket is opened.

func testCatalog(t *testing.T) *pricing.Catalog {
	t.Helper()
	cat, err := loadCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// brainDB writes a brain database the way `kilter brain` writes one: through
// api.Brain.Ingest, which is what fills both the time-keyed snapshot history
// and the evidence substrate.
//
// checkpointEvery is explicit because it is the freshness bound on everything
// why-cost and explain can see. A brain at the default 10 has not persisted
// the last nine snapshots' evidence, so a test that ingested five and then
// read the file would see an EMPTY substrate — which is a property of the
// brain, not of this helper, and is recorded in cmd/BRAINWIRE-FINDINGS.md.
func brainDB(t *testing.T, checkpointEvery int, snaps ...*model.ClusterSnapshot) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	brain, err := api.NewBrain(api.BrainConfig{
		CheckpointEvery: checkpointEvery,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, testCatalog(t), st)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	for _, s := range snaps {
		if err := brain.Ingest(s); err != nil {
			st.Close()
			t.Fatalf("ingest %s: %v", s.Timestamp, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------- the trap

// TestBacktestOverADatabaseRefusesAHistoryThatWouldScoreNothing.
//
// THE trap this unit exists to avoid. The database below holds a REAL history:
// two retained snapshots, readable, priced, with pods and nodes, inside the
// requested window. Nothing about it is empty. It simply cannot be replayed —
// both snapshots land 30 hours into a 48-hour window, so no decision instant
// has a full 24-hour future left inside it — and backtest.Run does NOT fail on
// that. It returns a Scorecard reading `snapshots 2`, `regret $0.00`, with the
// same field names and the same confident tone as a scorecard over a month of
// history, and an operator cannot tell "nothing was replayed" from "the policy
// is perfect".
//
// api.Brain.Backtest checks backtest's OWN coverage report (Scorecard.Instants,
// not a predicate re-derived in cmd) and refuses. This asserts the refusal is
// reachable from the command line, exits non-zero, and prints NO SCORECARD.
//
// The window is deliberately WIDER than the horizon: backtest.Run has its own
// "horizon exceeds the replay window" guard, and a window narrower than the
// horizon would be caught by that instead, leaving Instants == 0 untested.
func TestBacktestOverADatabaseRefusesAHistoryThatWouldScoreNothing(t *testing.T) {
	t0 := whyCostT0
	db := brainDB(t, 10,
		fleetSnapshot(t0.Add(30*time.Hour), 4, 0, 500),
		fleetSnapshot(t0.Add(31*time.Hour), 5, 1, 700),
	)

	var b strings.Builder
	err := runBacktestTo(&b, []string{
		"--cluster", "why-cost-demo", "--db", db,
		"--from", rfc(t0), "--to", rfc(t0.Add(48 * time.Hour)),
	})
	if err == nil {
		t.Fatalf("a scorecard over an unscoreable history was accepted:\n%s", b.String())
	}
	// The refusal names what was there and what it yielded, because "not
	// enough history" without numbers is indistinguishable from a bug.
	for _, want := range []string{"refused", "2 snapshot(s)", "no decision instant", "24h"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
	assertNoScorecard(t, b.String())

	// And the typed error survives the trip through cmd: rendering it is the
	// command's job, classifying it is pkg/api's.
	var tooShort api.ErrHistoryTooShort
	if !errors.As(err, &tooShort) {
		t.Fatalf("want api.ErrHistoryTooShort, got %T: %v", err, err)
	}
	if tooShort.Snapshots != 2 || tooShort.Instants != 0 {
		t.Errorf("ErrHistoryTooShort reported snapshots=%d instants=%d, want 2 and 0",
			tooShort.Snapshots, tooShort.Instants)
	}
}

// TestBacktestOverADatabaseRefusesASingleSnapshot pins the other half of
// api.ErrHistoryTooShort: a history below the two-snapshot floor, where the
// count check is the one that fires. Scoring the one snapshot that exists is
// exactly what cmd/WIRING-FINDINGS.md §6.2 refused to do.
func TestBacktestOverADatabaseRefusesASingleSnapshot(t *testing.T) {
	t0 := whyCostT0
	db := brainDB(t, 10, fleetSnapshot(t0, 4, 0, 500))

	var b strings.Builder
	err := runBacktestTo(&b, []string{
		"--cluster", "why-cost-demo", "--db", db,
		"--from", rfc(t0), "--to", rfc(t0.Add(48 * time.Hour)),
	})
	if err == nil {
		t.Fatalf("a scorecard over one snapshot was accepted:\n%s", b.String())
	}
	for _, want := range []string{"refused", "1 snapshot(s)", "empty replay"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
	assertNoScorecard(t, b.String())
}

// TestBacktestOverADatabaseRefusesAnEmptyBrain pins api.ErrNoHistory's
// sibling: a database that exists and has never been ingested into. The
// cluster is unknown, so the history is empty, and the count check fires with
// zero — not a scorecard of zeros.
func TestBacktestOverADatabaseRefusesAClusterWithNoHistory(t *testing.T) {
	db := brainDB(t, 10)
	var b strings.Builder
	err := runBacktestTo(&b, []string{
		"--cluster", "never-ingested", "--db", db,
		"--from", rfc(whyCostT0), "--to", rfc(whyCostT0.Add(48 * time.Hour)),
	})
	if err == nil {
		t.Fatalf("a scorecard over an empty database was accepted:\n%s", b.String())
	}
	if !strings.Contains(err.Error(), "0 snapshot(s)") {
		t.Errorf("the refusal does not say how much history there was:\n%s", err)
	}
	assertNoScorecard(t, b.String())
}

// assertNoScorecard is the assertion every refusal above shares: the point is
// not that an error was returned, it is that no scorecard-shaped output
// reached the operator to be read as a verdict.
func assertNoScorecard(t *testing.T, out string) {
	t.Helper()
	for _, forbidden := range []string{"regret", "oracleGap", "flipRate", "safety"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a scorecard was printed for a refused replay (%q):\n%s", forbidden, out)
		}
	}
}

// ---------------------------------------------------------------- the wiring

// traceDB generates a realistic multi-day history with pkg/backtest's own
// trace builder and ingests it through api.Brain.Ingest, so the database holds
// what a running brain would have kept — including the hourly thinning
// pkg/store applies on the way in.
func traceDB(t *testing.T, kind backtest.TraceKind, days int) (path, cluster string, start time.Time) {
	t.Helper()
	spec := backtest.TraceSpec{
		Cluster: "replay-" + string(kind), Kind: kind,
		Start: backtestEpoch, Days: days, Workloads: 2,
		// Hourly rather than the 5-minute default: pkg/store thins the
		// history to an hour anyway, so a denser trace would cost a thousand
		// ingests to store exactly the same rows.
		Interval: time.Hour,
	}
	trace, err := spec.Build()
	if err != nil {
		t.Fatal(err)
	}
	return brainDB(t, 10, trace.Snapshots...), trace.Cluster, trace.Start
}

// TestBacktestOverADatabaseScoresTheRetainedHistory.
//
// The other side of the refusal: given a history long enough to replay, the
// command produces a real scorecard over the cluster's OWN snapshots. This is
// the capability cmd/WIRING-FINDINGS.md §6.2 refused and pkg/store's
// time-keyed bucket unblocked, asserted end to end through the CLI.
func TestBacktestOverADatabaseScoresTheRetainedHistory(t *testing.T) {
	db, cluster, start := traceDB(t, backtest.TraceRegimeChange, 6)
	args := []string{
		"--cluster", cluster, "--db", db,
		"--from", rfc(start), "--to", rfc(start.Add(6 * 24 * time.Hour)),
	}

	var b strings.Builder
	if err := runBacktestTo(&b, append(append([]string{}, args...), "--json")); err != nil {
		t.Fatalf("kilter backtest --cluster: %v\n%s", err, b.String())
	}
	var sc backtest.Scorecard
	if err := json.Unmarshal([]byte(b.String()), &sc); err != nil {
		t.Fatalf("decode scorecard: %v\n%s", err, b.String())
	}
	if sc.Cluster != cluster {
		t.Errorf("scorecard is for %q, want %q", sc.Cluster, cluster)
	}
	// The refusal's own gate, from the other direction: a scorecard only
	// escapes when something was actually replayed.
	if sc.Instants == 0 {
		t.Fatal("a scorecard escaped with zero decision instants — the refusal is not gating")
	}
	if sc.Snapshots < 2 {
		t.Errorf("replayed %d snapshots, want the retained history", sc.Snapshots)
	}
	// The history is what SaveSnapshotAt retained, not what was ingested: the
	// trace is hourly and so is the retention, so all of it survived.
	if want := 6 * 24; sc.Snapshots != want {
		t.Errorf("replayed %d snapshots, want the %d the hourly retention kept", sc.Snapshots, want)
	}

	// Byte-identical across runs, over a database: no clock reaches the
	// replay, and the history read is not order-dependent.
	var again strings.Builder
	if err := runBacktestTo(&again, append(append([]string{}, args...), "--json")); err != nil {
		t.Fatal(err)
	}
	if again.String() != b.String() {
		t.Error("two replays of the same database disagreed byte for byte")
	}

	// And the text rendering names the source, so a scorecard pasted into a
	// ticket says which history produced it.
	var text strings.Builder
	if err := runBacktestTo(&text, args); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cluster " + cluster, "regret", rfc(start)} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the scorecard header does not mention %q:\n%s", want, text.String())
		}
	}
}

// ---------------------------------------------------------------- agreement

// TestBrainAndFileSourcesAgreeOnWhyCost.
//
// The brain-backed source is a SIBLING of --kube-snapshot, and the claim that
// makes it a sibling rather than a second implementation is this one: over the
// same snapshots and the same window, the two produce the SAME ANSWER, byte
// for byte, in the form a machine reads.
//
// It is asserted over the JSON rather than the prose because JSON carries the
// evidence IDs: the terms, the residual and every citation have to match, not
// just the dollar amounts.
//
// "Where both can answer" is a real qualifier and TWO things narrow it.
//
//  1. SPACING. pkg/store thins the history to one snapshot per hour, so a
//     5-minute series would leave the brain with fewer composition edges than
//     the file series holds. Twelve hours apart, the retention keeps
//     everything and there is nothing left to disagree about.
//  2. EVENTS. The brain's substrate also records deploys and OOMKills, which
//     the file path never observes, and pkg/explain cites them where they fall
//     inside a term. The fixture here therefore holds its container sizing
//     CONSTANT — the subtest below changes it on purpose and pins what the
//     difference is.
func TestBrainAndFileSourcesAgreeOnWhyCost(t *testing.T) {
	t0 := whyCostT0
	const steadySizing = 500
	snaps := []*model.ClusterSnapshot{
		fleetSnapshot(t0, 4, 0, steadySizing),
		fleetSnapshot(t0.Add(12*time.Hour), 5, 1, steadySizing),
		fleetSnapshot(t0.Add(24*time.Hour), 4, 2, steadySizing),
	}
	fromFile, fromBrain := bothWhyCostSources(t, snaps, rfc(t0), rfc(t0.Add(25*time.Hour)))
	if fromBrain != fromFile {
		t.Errorf("the two sources disagree over the same history\n--kube-snapshot:\n%s\n--db:\n%s",
			fromFile, fromBrain)
	}

	// Not vacuous: the answer both produced is a real decomposition.
	var att explain.Attribution
	if err := json.Unmarshal([]byte(fromBrain), &att); err != nil {
		t.Fatal(err)
	}
	if len(att.Terms) == 0 {
		t.Fatal("the agreed answer has no terms, so agreeing about it proves nothing")
	}
	var sum explain.Micro
	for _, term := range att.Terms {
		sum += term.Micro
	}
	if sum+att.Residual.Micro != att.DeltaMicro {
		t.Errorf("sum(terms)=%d + residual=%d != delta=%d", sum, att.Residual.Micro, att.DeltaMicro)
	}
}

// TestTheBrainCitesMoreThanAFileCanAndTheMoneyIsUnchanged.
//
// The one way the two sources legitimately differ, pinned so it cannot drift
// into a difference of substance. A brain sees DEPLOYS — it holds the previous
// snapshot's declared sizing and diffs it — while --kube-snapshot observes
// timeline points only. Where a deploy falls inside a term, pkg/explain cites
// it.
//
// So the brain's citations are a strict SUPERSET and every amount is
// identical. If the amounts ever differed, one of the two would be wrong about
// what a cost change is; if the citations were equal, the brain would be
// throwing away evidence it holds.
func TestTheBrainCitesMoreThanAFileCanAndTheMoneyIsUnchanged(t *testing.T) {
	t0 := whyCostT0
	snaps := []*model.ClusterSnapshot{
		fleetSnapshot(t0, 4, 0, 500),
		fleetSnapshot(t0.Add(12*time.Hour), 5, 1, 700), // the workload was resized
		fleetSnapshot(t0.Add(24*time.Hour), 4, 2, 900), // and resized again
	}
	rawFile, rawBrain := bothWhyCostSources(t, snaps, rfc(t0), rfc(t0.Add(25*time.Hour)))
	if rawFile == rawBrain {
		t.Fatal("the brain recorded no deploy event, so this asserts nothing")
	}

	var file, brain explain.Attribution
	if err := json.Unmarshal([]byte(rawFile), &file); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rawBrain), &brain); err != nil {
		t.Fatal(err)
	}
	if file.DeltaMicro != brain.DeltaMicro || file.Residual.Micro != brain.Residual.Micro {
		t.Errorf("the two sources disagree about the money: delta %d vs %d, residual %d vs %d",
			file.DeltaMicro, brain.DeltaMicro, file.Residual.Micro, brain.Residual.Micro)
	}
	if len(file.Terms) != len(brain.Terms) {
		t.Fatalf("term counts differ: %d vs %d", len(file.Terms), len(brain.Terms))
	}
	var extra int
	for i := range file.Terms {
		ft, bt := file.Terms[i], brain.Terms[i]
		if ft.Kind != bt.Kind || ft.Micro != bt.Micro {
			t.Errorf("term %d: %s=%d (file) vs %s=%d (brain)", i, ft.Kind, ft.Micro, bt.Kind, bt.Micro)
		}
		extra += compareCitations(t, ft, bt)
		if len(ft.Of) != len(bt.Of) {
			t.Errorf("term %q: sub-term counts differ, %d vs %d", ft.Kind, len(ft.Of), len(bt.Of))
			continue
		}
		for j := range ft.Of {
			if ft.Of[j].Kind != bt.Of[j].Kind || ft.Of[j].Micro != bt.Of[j].Micro {
				t.Errorf("sub-term %d of %q disagrees: %s=%d vs %s=%d", j, ft.Kind,
					ft.Of[j].Kind, ft.Of[j].Micro, bt.Of[j].Kind, bt.Of[j].Micro)
			}
			extra += compareCitations(t, ft.Of[j], bt.Of[j])
		}
	}
	if extra <= 0 {
		t.Error("the brain cited nothing extra, so its deploy events reached no term")
	}
	// The extra citations are deploy events, which is the whole claim: the
	// brain saw a rollout the snapshot files never described.
	if !strings.Contains(rawBrain, "/deploy@") {
		t.Error("the brain's extra citations are not deploy events")
	}
}

// compareCitations asserts the brain kept every citation the file source
// carries and reports how many it added.
func compareCitations(t *testing.T, file, brain explain.Term) int {
	t.Helper()
	have := map[explain.ID]bool{}
	for _, id := range brain.Evidence {
		have[id] = true
	}
	for _, id := range file.Evidence {
		if !have[id] {
			t.Errorf("term %q: the brain dropped the citation %q the file source carries", file.Kind, id)
		}
	}
	return len(brain.Evidence) - len(file.Evidence)
}

// bothWhyCostSources runs the same window through both sources over the same
// snapshots: once from a brain database that ingested them, once from the JSON
// files. CheckpointEvery is 1 because the substrate has to be ON DISK for
// another process to read it, and a brain at the default 10 would have
// persisted none of a three-snapshot history — a property of the brain, not of
// this test, and recorded in cmd/BRAINWIRE-FINDINGS.md.
func bothWhyCostSources(t *testing.T, snaps []*model.ClusterSnapshot, from, to string) (file, brain string) {
	t.Helper()
	db := brainDB(t, 1, snaps...)
	dir := t.TempDir()
	fileArgs := []string{}
	for i, s := range snaps {
		fileArgs = append(fileArgs, "--kube-snapshot",
			writeSnapshot(t, dir, "snap-"+string(rune('0'+i))+".json", s))
	}
	window := []string{"--from", from, "--to", to, "--json"}
	return runWhyCostOK(t, append(append([]string{}, fileArgs...), window...)...),
		runWhyCostOK(t, append([]string{"--db", db, "--cluster", snaps[0].ClusterID}, window...)...)
}

// TestExplainOverADatabaseMatchesTheHTTPRoute.
//
// `kilter explain --db` and `GET /api/v1/clusters/{id}/explain` must be the
// same answer, because they are the same brain answering the same question —
// and the thing most likely to make them differ is the DEFAULT WINDOW.
// pkg/api's defaultExplainWindow is unexported, so cmd carries its own
// brainExplainWindow constant, and a constant copied by hand is a constant
// that drifts. This pins them to each other: both sides resolve the window
// from the latest ingested snapshot (never a clock), and if either changed the
// span or the +1s edge, the payloads would stop matching.
//
// The route is exercised through Brain.Handler() with httptest.NewRecorder, so
// no socket is opened.
func TestExplainOverADatabaseMatchesTheHTTPRoute(t *testing.T) {
	snap := loadFixtureSnapshot(t)
	db := brainDB(t, 1, snap)

	// The CLI, with NO --from/--to: the default window is what is under test.
	var cli strings.Builder
	if err := runExplainTo(&cli, []string{
		"--db", db, "--cluster", snap.ClusterID,
		"--workload", "Deployment/default/api", "--container", "api", "--json",
	}); err != nil {
		t.Fatalf("kilter explain --db: %v\n%s", err, cli.String())
	}

	// The route, over the same database, opened after the command released
	// the lock.
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	brain, err := api.NewBrain(api.BrainConfig{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, testCatalog(t), st)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	brain.Handler().ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/v1/clusters/"+snap.ClusterID+"/explain?subject=Deployment/default/api/api", nil))
	if rec.Code != 200 {
		t.Fatalf("route answered %d: %s", rec.Code, rec.Body.String())
	}

	if got, want := canonicalJSON(t, cli.String()), canonicalJSON(t, rec.Body.String()); got != want {
		t.Errorf("the command and the route disagree\ncommand: %s\nroute:   %s", got, want)
	}

	// Not vacuous: the agreed payload is a grounded explanation.
	var payload explain.Explanation
	if err := json.Unmarshal([]byte(cli.String()), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Drivers) == 0 || len(payload.Citations) == 0 {
		t.Fatal("the agreed payload has no drivers or no citations")
	}
	// The window came from the snapshot, not from a clock: it ends one second
	// after the latest ingested snapshot, exactly as the route defines it.
	if want := snap.Timestamp.Add(time.Second); !payload.To.Equal(want) {
		t.Errorf("window ends %s, want %s (latest snapshot + 1s)", payload.To, want)
	}
	if want := snap.Timestamp.Add(time.Second).Add(-brainExplainWindow); !payload.From.Equal(want) {
		t.Errorf("window starts %s, want %s", payload.From, want)
	}
}

// loadFixtureSnapshot reads the recorded cluster snapshot the file-backed
// explain tests already use, so both sources are fed identical bytes.
func loadFixtureSnapshot(t *testing.T) *model.ClusterSnapshot {
	t.Helper()
	raw, err := os.ReadFile(readFixture(t, "cluster.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snap model.ClusterSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	return &snap
}

// canonicalJSON re-encodes a payload so two encoders' whitespace choices are
// not mistaken for a disagreement about the answer.
func canonicalJSON(t *testing.T, raw string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
