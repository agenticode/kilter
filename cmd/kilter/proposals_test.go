package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/whatif"
)

// These tests drive pkg/whatif's REAL Store, state machine and codec through
// the REAL CLI entry point. Every timestamp is supplied with --now, so the
// store's bytes are a function of the arguments alone.

const proposalNow = "2026-08-26T12:00:00Z"

func runProposalsOK(t *testing.T, args ...string) string {
	t.Helper()
	var b strings.Builder
	if err := runProposalsTo(&b, args); err != nil {
		t.Fatalf("kilter proposals %s: %v\n%s", strings.Join(args, " "), err, b.String())
	}
	return b.String()
}

func runProposalsErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var b strings.Builder
	err := runProposalsTo(&b, args)
	if err == nil {
		t.Fatalf("kilter proposals %s was accepted:\n%s", strings.Join(args, " "), b.String())
	}
	return b.String(), err
}

// fileAProposal runs `kilter whatif --propose` into a fresh store and returns
// the store path and the proposal id.
func fileAProposal(t *testing.T, extra ...string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proposals.json")
	args := append(append([]string{}, acceptedArgs...),
		"--propose", "--store", path, "--now", proposalNow,
		"--rationale", "cpu headroom at the bottom of the envelope is cheaper on this trace")
	args = append(args, extra...)
	out := runWhatIfOK(t, args...)
	id := proposalIDFrom(t, out)
	return path, id
}

func proposalIDFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "filed proposal "); ok {
			return strings.Fields(rest)[0]
		}
	}
	t.Fatalf("no proposal id in:\n%s", out)
	return ""
}

// TestWhatIfProposeFilesAGatedProposalTheStoreCanRead is the round trip the
// whole unit exists for: replay → gate → proposal → a record a human reads.
func TestWhatIfProposeFilesAGatedProposalTheStoreCanRead(t *testing.T) {
	path, id := fileAProposal(t)

	raw := runProposalsOK(t, "list", "--store", path, "--json")
	var recs []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		t.Fatalf("decode list: %v\n%s", err, raw)
	}
	if len(recs) != 1 {
		t.Fatalf("listed %d records, want 1", len(recs))
	}

	shown := runProposalsOK(t, "show", "--store", path, "--json", id)
	var rec struct {
		ID       string          `json:"id"`
		State    string          `json:"state"`
		Proposal whatif.Proposal `json:"proposal"`
		Approval json.RawMessage `json:"approval"`
		History  []struct {
			From, To string
			By       whatif.Actor
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(shown), &rec); err != nil {
		t.Fatalf("decode record: %v\n%s", err, shown)
	}
	if rec.ID != id {
		t.Errorf("record id %q, want %q", rec.ID, id)
	}
	if rec.State != string(whatif.StateGated) {
		t.Errorf("state %q, want gated", rec.State)
	}
	if rec.Approval != nil {
		t.Errorf("a freshly gated proposal carries an approval: %s", rec.Approval)
	}
	if !rec.Proposal.Gate.Passed {
		t.Errorf("a gated proposal whose gate did not pass: %v", rec.Proposal.Gate.Reasons)
	}
	if rec.Proposal.Kind != whatif.KindPolicyChange {
		t.Errorf("kind %q, want %q", rec.Proposal.Kind, whatif.KindPolicyChange)
	}
	if rec.Proposal.Rationale == "" {
		t.Error("--rationale did not reach the proposal")
	}
	// The audit trail reads as a lifecycle, and the gate is its own actor.
	if len(rec.History) != 2 ||
		rec.History[0].To != string(whatif.StateDraft) ||
		rec.History[1].To != string(whatif.StateGated) {
		t.Fatalf("history = %+v, want (new)→draft→gated", rec.History)
	}
	if rec.History[1].By.Kind != whatif.ActorSystem {
		t.Errorf("the gating transition was attributed to %v, not the system", rec.History[1].By)
	}

	// The human form prints the proposal verbatim plus the audit trail.
	human := runProposalsOK(t, "show", "--store", path, id)
	for _, want := range []string{id, "gated", "audit trail", "→ gated", `"kind": "policy-change"`} {
		if !strings.Contains(human, want) {
			t.Errorf("show does not print %q:\n%s", want, human)
		}
	}
}

// TestTheProposerCannotSupplyTheVerdict.
//
// Result.Spec deliberately omits the GateResult and Store.Create runs Decide
// itself, so a caller hands over evidence and receives a judgment. Through the
// CLI: the verdict on the filed record must be the one the what-if computed,
// and a REJECTED comparison must file as rejected rather than as gated.
func TestTheProposerCannotSupplyTheVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proposals.json")
	out := runWhatIfOK(t, append(append([]string{}, rejectedArgs...),
		"--propose", "--store", path, "--now", proposalNow)...)
	if !strings.Contains(out, "REJECTED") {
		t.Fatalf("the rejected case no longer fails the gate:\n%s", out)
	}
	id := proposalIDFrom(t, out)
	if !strings.Contains(out, "the gate rejected it") {
		t.Errorf("the filing did not say the gate rejected it:\n%s", out)
	}
	shown := runProposalsOK(t, "show", "--store", path, "--json", id)
	if !strings.Contains(shown, `"state":"rejected"`) &&
		!strings.Contains(shown, `"state": "rejected"`) {
		t.Errorf("a gate-rejected proposal was not filed as rejected:\n%s", shown)
	}
	// It is still FILED: a rejected proposal is the record of a question that
	// was asked and answered, and discarding it would make a tuner that ran
	// look like one that never did.
	if listed := runProposalsOK(t, "list", "--store", path); !strings.Contains(listed, id) {
		t.Errorf("the rejected proposal was not listed:\n%s", listed)
	}
}

// TestProposalsApproveIsRefusedByName.
//
// pkg/whatif made self-approval unrepresentable in the type system. A CLI that
// minted Actor{Kind: ActorHuman} from $USER would reintroduce it, because
// anything in that session inherits the identity — including unit 8's
// reasoner, which is the exact actor the human-only rule exists to exclude.
func TestProposalsApproveIsRefusedByName(t *testing.T) {
	path, id := fileAProposal(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	out, err := runProposalsErr(t, "approve", "--store", path, id)
	msg := err.Error()
	for _, want := range []string{
		id, "refused", "authenticate", "NewApprover", "$USER",
		"/api/v1/proposals/{id}/approvals", "HUMAN token tier",
		"kilter proposals reject",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(out, "approved") {
		t.Errorf("approve printed something that reads as success:\n%s", out)
	}
	// Nothing was written, and the record did not move.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("the refused approve rewrote the store")
	}
	if shown := runProposalsOK(t, "show", "--store", path, "--json", id); !strings.Contains(
		shown, string(whatif.StateGated)) {
		t.Errorf("the record moved out of gated:\n%s", shown)
	}
}

// TestProposalsAppliedIsRefusedByName. Recording an apply asserts that a
// config change landed and owes §4.6 a ledger entry; this binary writes
// neither. It is also unreachable, since nothing here can approve.
func TestProposalsAppliedIsRefusedByName(t *testing.T) {
	path, id := fileAProposal(t)
	_, err := runProposalsErr(t, "applied", "--store", path, id)
	msg := err.Error()
	for _, want := range []string{
		id, "refused", "LedgerEntry", "claimed-vs-measured",
		"/api/v1/proposals/{id}/applied", "only an approved proposal",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// TestNoCLIPathReachesApprovedOrApplied is the property behind the two
// refusals above, stated directly: INV-4's terminal states are unreachable
// from this binary, whatever sequence of commands is run.
func TestNoCLIPathReachesApprovedOrApplied(t *testing.T) {
	path, id := fileAProposal(t)
	for _, args := range [][]string{
		{"approve", "--store", path, id},
		{"applied", "--store", path, id},
		{"approve", "--store", path, "--note", "please", id},
		{"list", "--store", path, "--state", "approved"},
	} {
		var b strings.Builder
		_ = runProposalsTo(&b, args)
		if strings.Contains(b.String(), `"state": "approved"`) ||
			strings.Contains(b.String(), `"state": "applied"`) {
			t.Errorf("%v produced an approved/applied record:\n%s", args, b.String())
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"approved"`, `"applied"`, `"approval"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("the store on disk contains %s after a CLI-only session:\n%s", forbidden, raw)
		}
	}
	// And the author is never a human, so the audit trail never claims a
	// person filed what a command did.
	if strings.Contains(string(raw), `"kind":"human"`) ||
		strings.Contains(string(raw), `"kind": "human"`) {
		t.Errorf("the CLI synthesized a human identity:\n%s", raw)
	}
	_ = id
}

// TestAuthorIDCanOnlyRestrict.
//
// whatif.sameIdentity compares actor IDs and deliberately IGNORES Kind, so an
// operator who names themselves with --author-id is BLOCKED from later
// approving that proposal through the authenticated funnel. That direction is
// the safe one, which is why the AUTHOR identity is a flag and the APPROVER
// identity is not.
func TestAuthorIDCanOnlyRestrict(t *testing.T) {
	path, id := fileAProposal(t, "--author-id", "alice")
	shown := runProposalsOK(t, "show", "--store", path, "--json", id)
	var rec struct {
		Proposal struct {
			Author whatif.Actor `json:"author"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(shown), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Proposal.Author.ID != "alice" {
		t.Errorf("author id = %q, want alice", rec.Proposal.Author.ID)
	}
	if rec.Proposal.Author.Kind == whatif.ActorHuman {
		t.Error("--author-id minted a human actor; a CLI cannot authenticate one")
	}

	// The restriction is real: an approver with the same identity is refused
	// by pkg/whatif itself, whatever the kind.
	store, err := requireProposalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := whatif.NewApprover(whatif.Actor{Kind: whatif.ActorHuman, ID: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(ap, id, "mine", whatif.FixedClock(mustTime(t, proposalNow))); err == nil {
		t.Fatal("the author approved their own proposal")
	} else if !strings.Contains(err.Error(), "cannot be approved by its author") {
		t.Errorf("err = %v, want the self-approval refusal", err)
	}
	// A different human is not blocked — the rule is author≠approver, not
	// "nobody may approve".
	other, err := whatif.NewApprover(whatif.Actor{Kind: whatif.ActorHuman, ID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Approve(other, id, "reviewed", whatif.FixedClock(mustTime(t, proposalNow))); err != nil {
		t.Fatalf("a non-author human could not approve: %v", err)
	}
}

// TestProposalsRejectIsTheOneWriteTheCLIMakes. Any actor may reject: refusing
// to make a change is always safe, so it needs no capability — which is
// exactly why it is here and approve is not.
func TestProposalsRejectIsTheOneWriteTheCLIMakes(t *testing.T) {
	path, id := fileAProposal(t)
	out := runProposalsOK(t, "reject", "--store", path, "--reason", "not this quarter",
		"--now", "2026-08-27T09:00:00Z", id)
	if !strings.Contains(out, "rejected") {
		t.Errorf("reject did not report the new state:\n%s", out)
	}
	shown := runProposalsOK(t, "show", "--store", path, id)
	if !strings.Contains(shown, "not this quarter") {
		t.Errorf("the reason did not reach the audit trail:\n%s", shown)
	}
	if !strings.Contains(shown, "gated") || !strings.Contains(shown, "rejected") {
		t.Errorf("the transition is not in the audit trail:\n%s", shown)
	}
	// Rejection is terminal: there is no way back.
	if _, err := runProposalsErr(t, "reject", "--store", path, id); !strings.Contains(
		err.Error(), "cannot go rejected") {
		t.Errorf("a second rejection was not refused: %v", err)
	}
	// And the store still loads, with every invariant revalidated.
	if _, err := requireProposalStore(path); err != nil {
		t.Errorf("the store no longer loads after a reject: %v", err)
	}
}

// TestATamperedProposalStoreDoesNotLoad.
//
// A file is bytes, and this one is the only record that a proposal was gated.
// whatif.Load revalidates everything on the way in; this asserts the CLI goes
// through that door rather than around it. Without it, "state": "approved" is
// three keystrokes away from a policy change nobody approved.
func TestATamperedProposalStoreDoesNotLoad(t *testing.T) {
	path, id := fileAProposal(t)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"assert approved", `"state": "gated"`, `"state": "approved"`},
		{"flip the verdict", `"passed": true`, `"passed": false`},
		{"improve the numbers", `"candidateRegretUSD"`, `"candidateRegretUSD_x"`},
		{"invent a state", `"state": "gated"`, `"state": "blessed"`},
		{"add a field", `"records": [`, `"records": [{"override": true},`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := strings.Replace(string(good), tc.from, tc.to, 1)
			if tampered == string(good) {
				t.Fatalf("the edit %q did not apply; the store format changed", tc.from)
			}
			if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runProposalsErr(t, "list", "--store", path)
			if !strings.Contains(err.Error(), "--store") {
				t.Errorf("err = %v, want the store to be named", err)
			}
			if strings.Contains(out, id) {
				t.Errorf("a tampered store still listed a record:\n%s", out)
			}
		})
	}
}

// TestProposalStoreRoundTripIsByteStable. Snapshot() is byte-identical for
// identical state, so re-reading and rewriting an unchanged store must not
// move a byte — which is what makes the file diffable and a re-file
// idempotent rather than a duplicate factory.
func TestProposalStoreRoundTripIsByteStable(t *testing.T) {
	path, id := fileAProposal(t)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		store, err := requireProposalStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := saveProposalStore(path, store); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("round trip %d moved bytes", i)
		}
	}
	// Filing the identical proposal again is idempotent: proposals are
	// content-addressed, so the same evidence is the same proposal.
	args := append(append([]string{}, acceptedArgs...),
		"--propose", "--store", path, "--now", "2026-09-01T00:00:00Z",
		"--rationale", "cpu headroom at the bottom of the envelope is cheaper on this trace")
	again := runWhatIfOK(t, args...)
	if got := proposalIDFrom(t, again); got != id {
		t.Errorf("re-filing produced a second id %s (was %s); CreatedAt must not be in "+
			"the fingerprint", got, id)
	}
	listed := runProposalsOK(t, "list", "--store", path, "--json")
	var recs []json.RawMessage
	if err := json.Unmarshal([]byte(listed), &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Errorf("re-filing grew the store to %d records", len(recs))
	}
}

// TestProposalsListIsDeterministicAndFiltered. Store.List is already sorted by
// (CreatedAt, ID); the CLI prints in that order and does not re-sort.
func TestProposalsListIsDeterministicAndFiltered(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proposals.json")
	file := func(demo, set, now string) string {
		out := runWhatIfOK(t, "--demo", demo, "--set", set,
			"--propose", "--store", path, "--now", now)
		return proposalIDFrom(t, out)
	}
	first := file("bursty", "cpu-headroom=1.05", "2026-08-26T12:00:00Z")
	second := file("diurnal", "cpu-headroom=1.05", "2026-08-26T13:00:00Z")
	third := file("regime-change", "cpu-headroom=1.30", "2026-08-26T14:00:00Z")

	base := runProposalsOK(t, "list", "--store", path)
	for i := 0; i < 5; i++ {
		if got := runProposalsOK(t, "list", "--store", path); got != base {
			t.Fatalf("list run %d differs", i)
		}
	}
	if a, b, c := strings.Index(base, first), strings.Index(base, second),
		strings.Index(base, third); !(a < b && b < c) {
		t.Errorf("list is not in (CreatedAt, ID) order: %d %d %d\n%s", a, b, c, base)
	}

	// --state and --cluster narrow it, and an unknown state is refused rather
	// than silently matching nothing.
	gated := runProposalsOK(t, "list", "--store", path, "--state", "gated")
	if strings.Contains(gated, third) {
		t.Errorf("the rejected proposal was listed as gated:\n%s", gated)
	}
	if !strings.Contains(gated, first) || !strings.Contains(gated, second) {
		t.Errorf("a gated proposal was missing from --state gated:\n%s", gated)
	}
	byCluster := runProposalsOK(t, "list", "--store", path, "--cluster", "demo-diurnal")
	if !strings.Contains(byCluster, second) || strings.Contains(byCluster, first) {
		t.Errorf("--cluster did not filter:\n%s", byCluster)
	}
	if _, err := runProposalsErr(t, "list", "--store", path, "--state", "blessed"); !strings.Contains(
		err.Error(), "unknown state") {
		t.Errorf("an unknown --state was accepted: %v", err)
	}
	// An empty result is `[]`, never `null`.
	empty := runProposalsOK(t, "list", "--store", path, "--cluster", "nowhere", "--json")
	if strings.TrimSpace(empty) != "[]" {
		t.Errorf("an empty JSON listing is %q, want []", strings.TrimSpace(empty))
	}
}

// TestProposalsRefusesAMissingStoreRatherThanShowingNothing.
//
// "0 proposals" and "you named a file that is not there" render identically as
// an empty list, and the first reads as a fact about the fleet.
func TestProposalsRefusesAMissingStoreRatherThanShowingNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	for _, args := range [][]string{
		{"list", "--store", missing},
		{"show", "--store", missing, "abc"},
		{"reject", "--store", missing, "abc"},
	} {
		out, err := runProposalsErr(t, args...)
		if !strings.Contains(err.Error(), "--store") {
			t.Errorf("%v: err = %v", args, err)
		}
		if strings.Contains(out, "(none)") {
			t.Errorf("%v printed an empty listing for a missing file:\n%s", args, out)
		}
	}
	// No --store at all names where the brain's proposals belong.
	_, err := runProposalsErr(t, "list")
	for _, want := range []string{"--store PATH is required", "pkg/store", "proposals", "bucket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
	// --propose without a store refuses rather than printing an id for a
	// record nobody kept.
	_, werr := runWhatIfErr(t, append(append([]string{}, acceptedArgs...), "--propose")...)
	for _, want := range []string{"--store PATH is required", "bbolt", "proposals"} {
		if !strings.Contains(werr.Error(), want) {
			t.Errorf("the --propose refusal does not mention %q:\n%s", want, werr)
		}
	}
}

// TestProposalsUnknownSubcommand.
func TestProposalsUnknownSubcommand(t *testing.T) {
	out, err := runProposalsErr(t, "frobnicate")
	if !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out, "kilter proposals list") {
		t.Errorf("the usage text was not printed:\n%s", out)
	}
	if _, err := runProposalsErr(t); !strings.Contains(err.Error(), "subcommand is required") {
		t.Errorf("no subcommand: err = %v", err)
	}
	if _, err := runProposalsErr(t, "show", "--store", "x"); !strings.Contains(
		err.Error(), "id is required") {
		t.Errorf("show with no id: err = %v", err)
	}
	// A refused verb still names the proposal the caller meant, not the
	// --store value.
	path, id := fileAProposal(t)
	_, err = runProposalsErr(t, "approve", "--store", path, id)
	if !strings.Contains(err.Error(), "approve "+id) {
		t.Errorf("the refusal quoted the wrong id:\n%s", err)
	}
}

// TestProposalsShowUnknownID.
func TestProposalsShowUnknownID(t *testing.T) {
	path, _ := fileAProposal(t)
	if _, err := runProposalsErr(t, "show", "--store", path, "0000000000000000"); !strings.Contains(
		err.Error(), "no such proposal") {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := parseNowFlag(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
