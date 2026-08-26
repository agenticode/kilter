package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/whatif"
)

// `kilter proposals` — the closed-loop proposal record, made readable.
//
// A proposal is an inert artifact: "this policy scored better than the
// incumbent over this window, here is the evidence, here is the gate's
// verdict". This command lists them, shows one whole, and rejects one. It does
// NOT approve and it does NOT record an apply, and both refusals are by name —
// see proposalsApproveRefusal and proposalsAppliedRefusal. They are the point
// of the command, not gaps in it.
//
// # Where the bytes live
//
// pkg/whatif owns no file, no bucket and no schema migration on purpose:
// Store.Snapshot() and whatif.Load() move bytes and cmd/ decides where they
// go. --store names a local JSON file. That is a LOCAL artifact and says so;
// the fleet's proposals belong in pkg/store's bbolt file under a `proposals`
// bucket, which pkg/api owns and has not built (cmd/WHATIF-WIRING-FINDINGS.md).
//
// The file is not trusted on the way back in. whatif.Load re-validates every
// record: the ID must be the fingerprint the contents actually hash to, an
// approved record must carry an approval bound to that fingerprint and that
// verdict granted by a human who is not the author, and unknown fields are
// rejected. A hand-edited store fails to load rather than loading a lie, which
// is exactly the property that makes a plain file an acceptable home for this.

const proposalsUsage = `kilter proposals — the closed-loop policy proposals and their audit trail

Usage:
  kilter proposals list    --store PATH [--cluster ID] [--state STATE] [--json]
  kilter proposals show    --store PATH [--json] <id>
  kilter proposals reject  --store PATH [--reason "..."] [--now RFC3339] <id>
  kilter proposals approve --store PATH <id>          (refused: see below)
  kilter proposals applied --store PATH <id>          (refused: see below)

States: gated | approved | rejected | applied | expired

Flags:
  --store PATH     the proposal store file written by kilter whatif --propose
  --cluster ID     list only proposals for this cluster
  --state STATE    list only proposals in this state
  --json           emit the record(s) verbatim instead of the table
  --reason TEXT    why the proposal is being rejected
  --now RFC3339    audit timestamp (default: the wall clock)

approve and applied are REFUSED by this command, by name. Approving mints a
capability that only an authenticated human may hold, and a local CLI cannot
authenticate one; recording an apply asserts that a config change landed, and
nothing in this binary writes config. Run either to read the full reason.
`

func runProposals(args []string) error { return runProposalsTo(os.Stdout, args) }

// runProposalsTo is the testable entry point: everything printed goes to w.
func runProposalsTo(w io.Writer, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(w, proposalsUsage)
		return fmt.Errorf("proposals: a subcommand is required (list|show|reject|approve|applied)")
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "list":
		return proposalsList(w, rest)
	case "show":
		return proposalsShow(w, rest)
	case "reject":
		return proposalsReject(w, rest)
	case "approve":
		return proposalsApproveRefusal(rest)
	case "applied":
		return proposalsAppliedRefusal(rest)
	case "help", "-h", "--help":
		fmt.Fprint(w, proposalsUsage)
		return nil
	default:
		fmt.Fprint(w, proposalsUsage)
		return fmt.Errorf("proposals: unknown subcommand %q", verb)
	}
}

// ---------------------------------------------------------------- list

func proposalsList(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("proposals list", flag.ContinueOnError)
	fs.SetOutput(w)
	storePath := fs.String("store", "", "proposal store file")
	cluster := fs.String("cluster", "", "filter by cluster")
	state := fs.String("state", "", "filter by state")
	jsonOut := fs.Bool("json", false, "emit the records as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := requireProposalStore(*storePath)
	if err != nil {
		return err
	}

	// List() is already sorted by (CreatedAt, ID). The spec is explicit: print
	// in order and do not re-sort — a second sort here would be a second
	// definition of "the order proposals are read in".
	recs := store.List()
	if s := strings.TrimSpace(*state); s != "" {
		want := whatif.State(s)
		if !knownProposalState(want) {
			return fmt.Errorf("proposals list --state %q: unknown state (gated|approved|rejected|applied|expired)", s)
		}
		recs = store.ListState(want)
	}
	if c := strings.TrimSpace(*cluster); c != "" {
		filtered := make([]*whatif.Record, 0, len(recs))
		for _, r := range recs {
			if r.Proposal().Cluster == c {
				filtered = append(filtered, r)
			}
		}
		recs = filtered
	}

	if *jsonOut {
		// A JSON array, always — an empty store is `[]`, never `null`, so a
		// consumer cannot confuse "no proposals" with "no answer".
		if recs == nil {
			recs = []*whatif.Record{}
		}
		return writeJSON(w, recs)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kilter proposals — %d record(s) in %s\n\n", len(recs), *storePath)
	if len(recs) == 0 {
		b.WriteString("  (none)\n")
		_, err := io.WriteString(w, b.String())
		return err
	}
	t := &table{header: []string{"ID", "STATE", "CLUSTER", "REGRET", "PROJECTED/MO", "CHANGES", "CREATED", "AUTHOR"}}
	for _, r := range recs {
		p := r.Proposal()
		changes := make([]string, 0, len(p.Changes))
		for _, c := range p.Changes {
			changes = append(changes, string(c.Axis))
		}
		t.add(
			r.ID(),
			string(r.State()),
			p.Cluster,
			fmt.Sprintf("%s → %s", usd(p.BaselineRegret), usd(p.CandidateRegret)),
			signedUSD(p.Delta.ProjectedMonthlyUSD),
			strings.Join(changes, ","),
			p.CreatedAt.UTC().Format(time.RFC3339),
			p.Author.String(),
		)
	}
	b.WriteString(t.render("  "))
	_, err = io.WriteString(w, b.String())
	return err
}

func knownProposalState(s whatif.State) bool {
	switch s {
	case whatif.StateGated, whatif.StateApproved, whatif.StateRejected,
		whatif.StateApplied, whatif.StateExpired:
		return true
	}
	return false
}

// ---------------------------------------------------------------- show

func proposalsShow(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("proposals show", flag.ContinueOnError)
	fs.SetOutput(w)
	storePath := fs.String("store", "", "proposal store file")
	jsonOut := fs.Bool("json", false, "emit the record as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(fs.Arg(0))
	if id == "" {
		return fmt.Errorf("proposals show: a proposal id is required")
	}
	store, err := requireProposalStore(*storePath)
	if err != nil {
		return err
	}
	rec, ok := store.Get(id)
	if !ok {
		return fmt.Errorf("proposals show %s: %w in %s", id, whatif.ErrNotFound, *storePath)
	}
	if *jsonOut {
		// The whole Record — proposal, state, approval and audit history. This
		// is the same shape GET /api/v1/proposals/{id} must serve.
		return writeJSON(w, rec)
	}

	var b strings.Builder
	// Proposal().Encode() verbatim, per the spec: the proposal is the
	// evidence, and reformatting it here would be a second rendering of one
	// artifact that could drift from the one the API serves.
	raw, err := rec.Proposal().Encode()
	if err != nil {
		return err
	}
	fmt.Fprintf(&b, "proposal %s — %s\n\n", rec.ID(), rec.State())
	b.Write(raw)

	b.WriteString("\naudit trail\n")
	for _, tr := range rec.History() {
		from := string(tr.From)
		if from == "" {
			from = "(new)"
		}
		fmt.Fprintf(&b, "  %s  %-9s → %-9s by %-24s %s\n",
			tr.At.UTC().Format(time.RFC3339), from, tr.To, tr.By.String(), tr.Note)
	}
	if ap, ok := rec.Approval(); ok {
		b.WriteString("\napproval\n")
		fmt.Fprintf(&b, "  by %s at %s, expires %s\n",
			ap.By().String(), ap.At().UTC().Format(time.RFC3339),
			ap.ExpiresAt().UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "  bound to proposal %s, verdict %s\n", ap.Fingerprint(), ap.Verdict())
		if n := ap.Note(); n != "" {
			fmt.Fprintf(&b, "  note: %s\n", n)
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// ---------------------------------------------------------------- reject

// proposalsReject is the one write this command performs, and it is safe by
// construction: pkg/whatif lets ANY actor reject, because refusing to make a
// change needs no capability. That asymmetry is why reject is implemented here
// and approve is not — the thing a CLI cannot supply is an authenticated
// human, and rejection does not need one.
//
// The actor is Actor{Kind: ActorSystem, ID: "kilter-cli"} rather than a
// human: the command cannot prove a person ran it, and an audit trail that
// says "human:alice rejected this" when a script did is a worse record than
// one that says where the rejection came from.
func proposalsReject(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("proposals reject", flag.ContinueOnError)
	fs.SetOutput(w)
	storePath := fs.String("store", "", "proposal store file")
	reason := fs.String("reason", "", "why the proposal is being rejected")
	nowFlag := fs.String("now", "", "audit timestamp as RFC3339 (default: now)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(fs.Arg(0))
	if id == "" {
		return fmt.Errorf("proposals reject: a proposal id is required")
	}
	store, err := requireProposalStore(*storePath)
	if err != nil {
		return err
	}
	now, err := parseNowFlag(*nowFlag)
	if err != nil {
		return err
	}
	by := whatif.Actor{Kind: whatif.ActorSystem, ID: "kilter-cli"}
	rec, err := store.Reject(by, id, *reason, whatif.FixedClock(now))
	if err != nil {
		return fmt.Errorf("proposals reject %s: %w", id, err)
	}
	if err := saveProposalStore(*storePath, store); err != nil {
		return err
	}
	fmt.Fprintf(w, "proposal %s is now %s (by %s)\n", rec.ID(), rec.State(), by)
	return nil
}

// ---------------------------------------------------------------- refusals

// proposalsApproveRefusal is the sharpest line in this wiring.
//
// pkg/whatif made self-approval UNREPRESENTABLE rather than merely forbidden:
// Approval has no exported fields, no UnmarshalJSON, and the only route to one
// is Store.Approve, which needs an *Approver, which NewApprover will not build
// for a non-human actor. That is a type-system guarantee. It rests on exactly
// one thing the type system cannot supply — authentication — and pkg/whatif's
// FINDINGS names it: "the actor must come from the authenticated caller, not
// from a flag ... authentication is the API layer's job and is the one thing
// that can undo the guarantee".
//
// A local CLI has nothing to authenticate with. The identities within reach —
// $USER, os/user.Current, the uid — describe the SESSION, not the presence of
// a person, and everything running in that session inherits them, including
// unit 8's reasoner, which is precisely the actor the human-only rule exists
// to exclude. Minting Actor{Kind: ActorHuman} from any of them would convert a
// structural guarantee into a convention that `sh -c` walks around.
func proposalsApproveRefusal(args []string) error {
	id := refusedProposalID("proposals approve", args)
	return fmt.Errorf(`proposals approve %s: refused — a local CLI cannot authenticate a human approver.

whatif.NewApprover requires an Actor{Kind: human}, and pkg/whatif's guarantee is
that self-approval is not a rule that gets checked but a value that cannot be
constructed. The guarantee rests on one input the type system cannot supply:
proof that a human is on the other end.

This process has none. $USER, os/user.Current and the uid describe the SESSION,
not a person — anything running in that session inherits them, including unit
8's reasoner, which is the exact actor the human-only rule exists to exclude.
Minting Actor{Kind: ActorHuman, ID: $USER} here would turn a type-system
guarantee into a convention that a shell command walks around, and it would do
it silently, in the direction that grants capability.

Approval belongs behind the authenticated funnel, at a token tier the MCP
server (§6) and the reasoner (§5) do not hold:

  POST /api/v1/proposals/{id}/approvals    (authWrite, HUMAN token tier only)

See pkg/whatif/FINDINGS.md, "Brain wiring (pkg/api)" and "Approval is
structural, not procedural", and cmd/WHATIF-WIRING-FINDINGS.md.

What this command can do, because it needs no capability:
  kilter proposals reject %s --reason "..." --store PATH`, id, id)
}

// proposalsAppliedRefusal explains why the after-the-fact record is not here.
//
// Two independent reasons, and the second is the one that matters: even if the
// record were harmless, nothing in this binary can produce an APPROVED
// proposal to apply, because approve refuses. A verb that could only ever
// return "proposal X is gated; only an approved proposal can be applied" is
// better replaced by the reason it can never do anything else.
func proposalsAppliedRefusal(args []string) error {
	id := refusedProposalID("proposals applied", args)
	return fmt.Errorf(`proposals applied %s: refused — nothing in this binary applies a policy change.

MarkApplied records, AFTER THE FACT, that a config change actually landed, and
§4.6 requires a ledger entry written in the same breath — the proposal ID, both
policy hashes and the claimed Delta.ProjectedMonthlyUSD — because that entry is
what later lets the existing claimed-vs-measured join score the tuner itself.
This command writes no config and no ledger entry, so recording "applied" from
here would put an unbacked fact into an audit trail whose whole value is that
its facts are backed.

It is also unreachable: only an approved proposal can be applied, and nothing
in this CLI can produce one (kilter proposals approve %s explains why).

Where it belongs, next to the writer:
  POST /api/v1/proposals/{id}/applied      (authWrite, system)
plus the LedgerEntry. See pkg/whatif/FINDINGS.md, "Brain wiring (pkg/api)".`, id, id)
}

// refusedProposalID recovers the id from a refused verb's arguments so the
// refusal can name the proposal the caller meant. The same flags the working
// verbs accept are parsed and discarded, because `--store PATH <id>` must not
// report PATH as the id — a refusal that quotes the wrong thing back reads as
// a parsing bug rather than as a decision.
func refusedProposalID(verb string, args []string) string {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("store", "", "proposal store file")
	fs.String("note", "", "approver's note")
	fs.String("now", "", "audit timestamp as RFC3339")
	if err := fs.Parse(args); err == nil {
		if id := strings.TrimSpace(fs.Arg(0)); id != "" {
			return id
		}
	}
	return "<id>"
}

// ---------------------------------------------------------------- the file

// requireProposalStore loads an EXISTING store, or refuses.
//
// A missing file is an error rather than an empty store on purpose: "0
// proposals" and "you named a file that is not there" render identically as an
// empty list, and the first reads as a fact about the fleet.
func requireProposalStore(path string) (*whatif.Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New(`proposals: --store PATH is required.

pkg/whatif owns no file by design — Snapshot() and Load() move bytes and cmd/
decides where they live — so the store has to be named. It is written by
` + "`kilter whatif --propose --store PATH`" + `.

That file is a LOCAL artifact. The fleet's proposals belong in pkg/store's
bbolt file under a ` + "`proposals`" + ` bucket (pkg/whatif/FINDINGS.md, "Brain
wiring"), which is pkg/api's to add and is not wired yet.`)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--store: %w", err)
	}
	store, err := whatif.Load(raw)
	if err != nil {
		// whatif.Load re-validates every invariant, so this branch is also
		// the tamper detector: a hand-edited "state": "approved" fails here.
		return nil, fmt.Errorf("--store %s: %w", path, err)
	}
	return store, nil
}

// openProposalStore loads the store at path, or returns an empty one when the
// file does not exist yet — the `kilter whatif --propose` case, where creating
// the file is the point. Any other read error is reported: silently starting
// fresh on a permissions error would discard a pending approval.
func openProposalStore(path string) (*whatif.Store, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return whatif.NewStore(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("--store: %w", err)
	}
	store, err := whatif.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("--store %s: %w", path, err)
	}
	return store, nil
}

// saveProposalStore writes the store back.
//
// Snapshot() is byte-identical for identical state, so rewriting an unchanged
// store produces an unchanged file. The write goes to a temporary file in the
// same directory and is renamed over the target, because a store truncated
// half-way through a write is a store that no longer loads — and this file is
// the only record that a proposal was ever gated.
func saveProposalStore(path string, store *whatif.Store) error {
	raw, err := store.Snapshot()
	if err != nil {
		return fmt.Errorf("--store %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dirOf(path), ".kilter-proposals-*")
	if err != nil {
		return fmt.Errorf("--store %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("--store %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("--store %s: %w", path, err)
	}
	// 0600: the file carries approvals and an audit trail.
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("--store %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("--store %s: %w", path, err)
	}
	return nil
}

// dirOf is filepath.Dir with an empty path meaning the working directory.
func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// compile-time assertion that the record shape the CLI prints is the one
// pkg/whatif marshals: Record has MarshalJSON, so writeJSON emits the
// package's own wire form rather than a cmd/-side projection of it.
var _ json.Marshaler = (*whatif.Record)(nil)
