package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The rules that keep two sources of history from becoming one ambiguous one.
//
// Every assertion here is about a REFUSAL, and every refusal exists because
// the alternative is a plausible-looking answer: a database created by a typo
// answering "no history", a flag silently dropped, a window that moves with
// the wall clock, or two substrates resolved by an undocumented precedence.

type verbFn func(io.Writer, []string) error

func verbs() map[string]verbFn {
	return map[string]verbFn{
		"backtest": runBacktestTo,
		"why-cost": runWhyCostTo,
		"explain":  runExplainTo,
	}
}

// dbArgs is a minimal, otherwise-valid command line for each verb pointed at
// dbPath, so the only thing under test is the database.
func dbArgs(verb, dbPath, cluster string) []string {
	t0 := rfc(whyCostT0)
	t1 := rfc(whyCostT0.Add(48 * time.Hour))
	switch verb {
	case "backtest":
		return []string{"--cluster", cluster, "--db", dbPath, "--from", t0, "--to", t1}
	case "why-cost":
		return []string{"--db", dbPath, "--cluster", cluster, "--from", t0, "--to", t1}
	default:
		return []string{"--db", dbPath, "--cluster", cluster,
			"--workload", "Deployment/default/api", "--container", "api"}
	}
}

// TestReadVerbsRefuseAMissingDatabaseRatherThanCreatingOne.
//
// bolt.Open CREATES the file it is given. Left alone, `--db /tpm/kilter.db`
// would therefore succeed, make an empty database, and answer "no history for
// that cluster" — a true statement about the file it had just created and a
// completely misleading one about the cluster. All three verbs stat first and
// refuse, and the file must still not exist afterwards.
func TestReadVerbsRefuseAMissingDatabaseRatherThanCreatingOne(t *testing.T) {
	for verb, run := range verbs() {
		t.Run(verb, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "typo.db")
			var b strings.Builder
			err := run(&b, dbArgs(verb, missing, "prod"))
			if err == nil {
				t.Fatalf("a missing database was accepted:\n%s", b.String())
			}
			if !strings.Contains(err.Error(), "no such database") {
				t.Errorf("error = %q, want it to name the missing database", err)
			}
			if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
				t.Errorf("a read verb created %s", missing)
			}
		})
	}
}

// TestBrainBackedVerbsRefuseAnUnknownCluster.
//
// The command-line form of the routes' 404. "never ingested" and "ingested,
// but there is not enough evidence" are different facts, and merging them
// sends an operator to look for the wrong problem — so the refusal names the
// clusters the database does hold, because the usual cause is a typo or a
// disagreement about what the agent calls this cluster.
//
// backtest is deliberately absent: its refusals belong to api.Brain.Backtest,
// and a pre-check in cmd would shadow ErrHistoryTooShort.
func TestBrainBackedVerbsRefuseAnUnknownCluster(t *testing.T) {
	db := brainDB(t, 1, fleetSnapshot(whyCostT0, 4, 0, 500))
	for _, verb := range []string{"why-cost", "explain"} {
		t.Run(verb, func(t *testing.T) {
			var b strings.Builder
			err := verbs()[verb](&b, dbArgs(verb, db, "not-this-one"))
			if err == nil {
				t.Fatalf("an unknown cluster was accepted:\n%s", b.String())
			}
			for _, want := range []string{"no snapshot for that cluster", "Known clusters", "why-cost-demo"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestTheTwoSourcesAreExclusiveAndSaySo.
//
// A precedence rule would be worse than a refusal here: a recorded snapshot
// series and a brain's thinned history can legitimately disagree, and the
// operator who passed both has no way to learn which one answered.
func TestTheTwoSourcesAreExclusiveAndSaySo(t *testing.T) {
	db := brainDB(t, 1, fleetSnapshot(whyCostT0, 4, 0, 500))
	snapPath := writeSnapshot(t, t.TempDir(), "snap.json", fleetSnapshot(whyCostT0, 4, 0, 500))
	window := []string{"--from", rfc(whyCostT0), "--to", rfc(whyCostT0.Add(48 * time.Hour))}

	for _, tc := range []struct {
		name, verb string
		args       []string
		want       string
	}{
		{"why-cost both sources", "why-cost",
			append([]string{"--kube-snapshot", snapPath, "--db", db, "--cluster", "why-cost-demo"}, window...),
			"two different substrates"},
		{"why-cost cluster without db", "why-cost",
			append([]string{"--kube-snapshot", snapPath, "--cluster", "why-cost-demo"}, window...),
			"it needs --db"},
		{"why-cost db without cluster", "why-cost",
			append([]string{"--db", db}, window...),
			"--db needs --cluster"},
		{"why-cost no source", "why-cost", window,
			"a source of history is required"},
		{"explain both sources", "explain", []string{
			"--kube-snapshot", snapPath, "--db", db, "--cluster", "why-cost-demo",
			"--workload", "Deployment/shop/web", "--container", "web"},
			"two different substrates"},
		{"explain db without cluster", "explain", []string{
			"--db", db, "--workload", "Deployment/shop/web", "--container", "web"},
			"--db needs --cluster"},
		{"explain no source", "explain", []string{
			"--workload", "Deployment/shop/web", "--container", "web"},
			"a source of history is required"},
		{"backtest both sources", "backtest", []string{
			"--cluster", "why-cost-demo", "--demo", "steady", "--db", db},
			"different sources of history"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			if err := verbs()[tc.verb](&b, tc.args); err == nil {
				t.Fatalf("accepted:\n%s", b.String())
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestBacktestClusterRequiresAFixedWindow.
//
// The second trap. `--demo` gets its window from backtestEpoch, a constant,
// because a replay window that drifts with wall-clock time makes two runs over
// the same configuration disagree. `--cluster` has no such constant available
// — its history is whatever was retained — so the window becomes an argument,
// exactly as why-cost already requires. Defaulting it to "the last 30 days"
// would make a scorecard's headline number depend on the day it was run, and
// comparability is the entire value of a scorecard.
func TestBacktestClusterRequiresAFixedWindow(t *testing.T) {
	db := brainDB(t, 1, fleetSnapshot(whyCostT0, 4, 0, 500))
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no window", []string{"--cluster", "why-cost-demo", "--db", db}},
		{"only from", []string{"--cluster", "why-cost-demo", "--db", db, "--from", rfc(whyCostT0)}},
		{"only to", []string{"--cluster", "why-cost-demo", "--db", db, "--to", rfc(whyCostT0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			err := runBacktestTo(&b, tc.args)
			if err == nil {
				t.Fatalf("a drifting replay window was accepted:\n%s", b.String())
			}
			if !strings.Contains(err.Error(), "--from and --to are required") {
				t.Errorf("error = %q", err)
			}
			assertNoScorecard(t, b.String())
		})
	}
	// And --db is required before the window is even parsed: there is no
	// history without a database.
	var b strings.Builder
	err := runBacktestTo(&b, []string{"--cluster", "why-cost-demo",
		"--from", rfc(whyCostT0), "--to", rfc(whyCostT0.Add(48 * time.Hour))})
	if err == nil || !strings.Contains(err.Error(), "--db is required") {
		t.Errorf("error = %v, want --db to be required", err)
	}
}

// TestFlagsThatCannotBeHonouredAreRefusedNotIgnored.
//
// loadPolicy already refuses an unknown key in a policy file, and states why:
// a knob that is silently dropped produces a report for a configuration nobody
// ran. A flag belonging to the other source is the same failure with a
// friendlier face, so each source refuses the other's flags by name.
func TestFlagsThatCannotBeHonouredAreRefusedNotIgnored(t *testing.T) {
	db := brainDB(t, 1, fleetSnapshot(whyCostT0, 4, 0, 500))
	live := []string{"--cluster", "why-cost-demo", "--db", db,
		"--from", rfc(whyCostT0), "--to", rfc(whyCostT0.Add(48 * time.Hour))}
	policyPath := writePolicy(t, `{"recommend":{"cpuHeadroom":1.5}}`)

	for _, tc := range []struct {
		name, verb string
		args       []string
		want       string
	}{
		{"live rejects trace shape", "backtest", append(live, "--days", "3"), "--days"},
		{"live rejects noise", "backtest", append(live, "--noise", "0.1"), "--noise"},
		{"live rejects a policy file", "backtest", append(live, "--policy", policyPath), "--policy"},
		{"live rejects a comparison", "backtest", append(live, "--compare", policyPath), "--compare"},
		{"live rejects refusal enforcement", "backtest", append(live, "--enforce-refusals"), "--enforce-refusals"},
		{"live rejects derived costs", "backtest", append(live, "--derive-costs"), "--derive-costs"},
		{"demo rejects a database", "backtest",
			[]string{"--demo", "steady", "--db", db}, "--db"},
		{"demo rejects a window", "backtest",
			[]string{"--demo", "steady", "--from", rfc(whyCostT0)}, "--from"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			if err := verbs()[tc.verb](&b, tc.args); err == nil {
				t.Fatalf("a flag that cannot be honoured was accepted:\n%s", b.String())
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			assertNoScorecard(t, b.String())
		})
	}
}

// TestWhyCostOverADatabaseRefusesALedgerFile.
//
// A brain attributes actions from the audit ledger it kept itself
// (api.Brain.ledgerActions, the projection lifted from loadLedgerActions).
// Splicing a file-supplied ledger in as well would give one attribution two
// answers to "which actions moved money" and no rule for reconciling them.
func TestWhyCostOverADatabaseRefusesALedgerFile(t *testing.T) {
	db := brainDB(t, 1, fleetSnapshot(whyCostT0, 4, 0, 500))
	ledger := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(ledger, []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	err := runWhyCostTo(&b, append(dbArgs("why-cost", db, "why-cost-demo"), "--ledger", ledger))
	if err == nil {
		t.Fatalf("accepted:\n%s", b.String())
	}
	if !strings.Contains(err.Error(), "--ledger") {
		t.Errorf("error = %q, want it to name --ledger", err)
	}
}

// TestTheFileBackedSourcesStillNeedNoDatabase.
//
// The compatibility claim, asserted rather than assumed: a user with recorded
// snapshots and no database keeps working exactly as before. Nothing about
// --kube-snapshot opens, creates or requires a bbolt file, and the working
// directory is untouched — a --db that defaulted to "kilter.db" would have
// made that false without changing a single command line.
func TestTheFileBackedSourcesStillNeedNoDatabase(t *testing.T) {
	dir := t.TempDir()
	snaps := []string{
		"--kube-snapshot", writeSnapshot(t, dir, "a.json", fleetSnapshot(whyCostT0, 4, 0, 500)),
		"--kube-snapshot", writeSnapshot(t, dir, "b.json", fleetSnapshot(whyCostT0.Add(12*time.Hour), 6, 0, 500)),
	}
	out := runWhyCostOK(t, append(append([]string{}, snaps...),
		"--from", rfc(whyCostT0), "--to", rfc(whyCostT0.Add(13*time.Hour)))...)
	if !strings.Contains(out, "node-count") {
		t.Errorf("the file-backed why-cost stopped explaining:\n%s", out)
	}

	var b strings.Builder
	if err := runExplainTo(&b, []string{
		"--kube-snapshot", readFixture(t, "cluster.json"),
		"--workload", "Deployment/default/api", "--container", "api",
	}); err != nil {
		t.Fatalf("the file-backed explain stopped working: %v\n%s", err, b.String())
	}
	if !strings.Contains(b.String(), "usage-history") {
		t.Errorf("the file-backed explain lost its drivers:\n%s", b.String())
	}

	// No database was created anywhere the commands could have put one.
	for _, dir := range []string{dir, "."} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".db") {
				t.Errorf("a file-backed run created %s", filepath.Join(dir, e.Name()))
			}
		}
	}
}
