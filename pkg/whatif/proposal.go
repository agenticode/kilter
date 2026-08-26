package whatif

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
)

// fingerprintLen is the hex width of every content hash in this package,
// matching plan fingerprints and backtest.PolicyHash. Sixteen hex chars is 64
// bits — collision-free for any realistic proposal population, short enough
// for a human to read back over the phone.
const fingerprintLen = 16

// Kind is what a proposal proposes. §5.2 enumerates two proposal tools; this
// unit implements the policy one. The annotation kind is declared, and
// rejected by Validate, so that the later unit adds a case rather than
// inventing a parallel artifact — see FINDINGS.md.
type Kind string

const (
	// KindPolicyChange proposes a new (recommend, plan, decision) triple.
	KindPolicyChange Kind = "policy-change"
	// KindAnnotationChange proposes a kilter.dev/* annotation edit. Declared,
	// not implemented: its gate is not a backtest scorecard, so accepting one
	// here would mean a proposal whose GateResult means nothing.
	KindAnnotationChange Kind = "annotation-change"
)

// maxRationale bounds the rationale text. The rationale is the one field an
// LLM writes freely (§5.2), so it is bounded and sanitized like any other
// untrusted string.
const maxRationale = 4096

// maxEvidenceIDs bounds the citation set attached to a proposal, matching the
// spirit of §5.4's hard result caps.
const maxEvidenceIDs = 64

// maxEvidenceIDLen bounds one citation.
const maxEvidenceIDLen = 512

// Target is what a policy change applies to. Cluster is required; Namespace
// and Class narrow it, per §4.6's "per-class policy".
type Target struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	Class     string `json:"class,omitempty"`
}

// Validate rejects an unusable target.
func (t Target) Validate() error {
	if strings.TrimSpace(t.Cluster) == "" {
		return errors.New("whatif: proposal target needs a cluster")
	}
	for _, f := range []struct{ name, v string }{
		{"cluster", t.Cluster}, {"namespace", t.Namespace}, {"class", t.Class},
	} {
		if len(f.v) > maxActorID {
			return fmt.Errorf("whatif: target %s is %d bytes, over the %d limit",
				f.name, len(f.v), maxActorID)
		}
		if clean, err := sanitizeNote(f.v); err != nil || clean != f.v {
			return fmt.Errorf("whatif: target %s %q contains characters that cannot be audited", f.name, f.v)
		}
	}
	return nil
}

// String is the stable rendering used in fingerprints and CLI output.
func (t Target) String() string {
	s := t.Cluster
	if t.Namespace != "" {
		s += "/" + t.Namespace
	}
	if t.Class != "" {
		s += "#" + t.Class
	}
	return s
}

// Change is one axis moving. Human-readable From/To strings sit alongside the
// numbers so a CLI and an API response render identically without either
// re-deriving the unit of a soak.
type Change struct {
	Axis Axis    `json:"axis"`
	From float64 `json:"from"`
	To   float64 `json:"to"`
	Text string  `json:"text"`
}

// changesBetween enumerates the axes that differ, in AllAxes order. Axes the
// envelope does not declare are still reported when they differ: a proposal
// whose candidate moved a knob nobody was searching over is exactly the thing
// an approver must be able to see.
func changesBetween(base, cand Policy) []Change {
	base, cand = base.withDefaults(), cand.withDefaults()
	var out []Change
	for _, a := range AllAxes {
		from, to := a.get(base), a.get(cand)
		if from == to {
			continue
		}
		out = append(out, Change{
			Axis: a,
			From: from,
			To:   to,
			Text: fmt.Sprintf("%s %s → %s", a, formatAxis(a, from), formatAxis(a, to)),
		})
	}
	return out
}

// Proposal is the inert artifact §4.6 and §5.2 both describe: "this policy
// scored better than the incumbent over this window, here is the evidence,
// here is the gate's verdict". It has no method that applies anything, no
// reference to a cluster client, and no state field — state lives in the
// Record, which only a Store can transition. A hostile caller holding a
// Proposal holds a document.
//
// Every field is content; CreatedAt is the single exception and is excluded
// from the fingerprint, so replaying the same history against the same
// candidate on a different day yields the same proposal identity.
type Proposal struct {
	Kind   Kind   `json:"kind"`
	Author Actor  `json:"author"`
	Target Target `json:"target"`

	Baseline  Policy   `json:"baseline"`
	Candidate Policy   `json:"candidate"`
	Changes   []Change `json:"changes"`

	// Gate is the verdict, computed by this package from the two scorecards.
	// It is not supplied by the proposer: Store.Create runs Decide itself,
	// precisely so a caller cannot hand in a flattering verdict.
	Gate GateResult `json:"gate"`
	// Delta is the scorecard arithmetic an approver reads.
	Delta Delta `json:"delta"`

	// Evidence: what was replayed, and the scorecards' own identities. The
	// full scorecards are large; a proposal carries their hashes plus the
	// window, and the CLI re-runs or re-reads them on demand.
	Cluster         string       `json:"cluster"`
	Window          [2]time.Time `json:"window"`
	HorizonHours    float64      `json:"horizonHours"`
	BaselineScore   string       `json:"baselineScore"`
	CandidateScore  string       `json:"candidateScore"`
	BaselineRegret  float64      `json:"baselineRegretUSD"`
	CandidateRegret float64      `json:"candidateRegretUSD"`

	Rationale   string   `json:"rationale,omitempty"`
	EvidenceIDs []string `json:"evidenceIDs,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

// scorecardHash fingerprints a scorecard by its canonical encoding, so a
// proposal is bound to the exact numbers it was gated on. Re-running the
// backtest and getting different bytes changes this hash, which changes the
// proposal fingerprint, which invalidates any approval — the correct outcome.
func scorecardHash(sc *backtest.Scorecard) (string, error) {
	if sc == nil {
		return "", errors.New("whatif: cannot hash a nil scorecard")
	}
	b, err := sc.Encode()
	if err != nil {
		return "", fmt.Errorf("whatif: encoding scorecard: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:fingerprintLen], nil
}

// Fingerprint is the proposal's content-addressed identity.
//
// Fields are written explicitly, in a fixed order, rather than reflected over
// or JSON-marshalled: adding a field to Proposal then becomes a compile-time
// -visible decision about whether it changes identity. This is the discipline
// plan.fingerprint and backtest.PolicyHash both use.
//
// CreatedAt is excluded. Everything else is included — in particular the
// author, so an approval bound to this fingerprint cannot be replayed onto a
// byte-identical proposal filed by somebody else (which, if that somebody were
// the approver, would be self-approval by replay).
func (p Proposal) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "kind|%s\n", p.Kind)
	fmt.Fprintf(h, "author|%s\n", p.Author)
	fmt.Fprintf(h, "target|%s\n", p.Target)
	fmt.Fprintf(h, "policy|%s|%s\n", p.Baseline.Hash(), p.Candidate.Hash())
	for i, c := range p.Changes {
		fmt.Fprintf(h, "change|%d|%s|%v|%v\n", i, c.Axis, c.From, c.To)
	}
	fmt.Fprintf(h, "verdict|%s\n", verdictHash(p.Gate))
	fmt.Fprintf(h, "cluster|%s\n", p.Cluster)
	fmt.Fprintf(h, "window|%d|%d|%v\n",
		p.Window[0].UTC().UnixNano(), p.Window[1].UTC().UnixNano(), p.HorizonHours)
	fmt.Fprintf(h, "scores|%s|%s\n", p.BaselineScore, p.CandidateScore)
	fmt.Fprintf(h, "regret|%v|%v\n", p.BaselineRegret, p.CandidateRegret)
	fmt.Fprintf(h, "rationale|%s\n", p.Rationale)
	for i, id := range p.EvidenceIDs {
		fmt.Fprintf(h, "evidence|%d|%s\n", i, id)
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintLen]
}

// ID is the fingerprint — proposals are content-addressed, like plans. Two
// identical proposals are one proposal, which makes a nightly tuner that
// re-derives the same candidate idempotent rather than a source of duplicates.
func (p Proposal) ID() string { return p.Fingerprint() }

// Encode renders the proposal as indented JSON with a trailing newline: the
// golden-file, API and CLI form. Byte-identical for identical inputs.
func (p Proposal) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Spec is what a proposer submits. It carries evidence and intent; it does
// NOT carry a verdict, a state, or an approval, because those are the three
// things a proposer must not be able to assert about its own proposal.
type Spec struct {
	Kind   Kind
	Target Target

	Baseline  Policy
	Candidate Policy
	// BaselineScore and CandidateScore are pkg/backtest's output for the two
	// policies over one window. Store.Create gates on them.
	BaselineScore  *backtest.Scorecard
	CandidateScore *backtest.Scorecard

	Envelope  Envelope
	Tolerance Tolerance

	Rationale   string
	EvidenceIDs []string
}

// normalize validates the spec and returns it with defaults applied and free
// text sanitized. Evidence IDs are sorted and de-duplicated so a proposal's
// bytes cannot depend on the order a caller happened to collect citations in.
func (s Spec) normalize() (Spec, error) {
	switch s.Kind {
	case "":
		s.Kind = KindPolicyChange
	case KindPolicyChange:
	case KindAnnotationChange:
		return Spec{}, fmt.Errorf(
			"whatif: proposal kind %q is not implemented here: its gate is not a backtest scorecard",
			s.Kind)
	default:
		return Spec{}, fmt.Errorf("whatif: unknown proposal kind %q", s.Kind)
	}
	if err := s.Target.Validate(); err != nil {
		return Spec{}, err
	}
	if s.BaselineScore == nil || s.CandidateScore == nil {
		return Spec{}, errors.New("whatif: a proposal needs both scorecards")
	}
	if s.BaselineScore.Cluster != s.Target.Cluster {
		return Spec{}, fmt.Errorf("whatif: scorecards are for cluster %q, target is %q",
			s.BaselineScore.Cluster, s.Target.Cluster)
	}
	s.Baseline = s.Baseline.withDefaults()
	s.Candidate = s.Candidate.withDefaults()
	if s.Envelope.Bounds == nil {
		s.Envelope = DefaultEnvelope()
	}
	s.Tolerance = s.Tolerance.withDefaults()

	if len(s.Rationale) > maxRationale {
		return Spec{}, fmt.Errorf("whatif: rationale is %d bytes, over the %d limit",
			len(s.Rationale), maxRationale)
	}
	clean, err := sanitizeNote(s.Rationale)
	if err != nil {
		return Spec{}, err
	}
	s.Rationale = clean

	ids, err := normalizeEvidenceIDs(s.EvidenceIDs)
	if err != nil {
		return Spec{}, err
	}
	s.EvidenceIDs = ids
	return s, nil
}

// normalizeEvidenceIDs sorts, de-duplicates and bounds a citation set.
// Sorting is not cosmetic: the IDs are inside the fingerprint, so an unsorted
// set would give the same proposal two identities depending on collection
// order.
func normalizeEvidenceIDs(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if len(id) > maxEvidenceIDLen {
			return nil, fmt.Errorf("whatif: evidence id is %d bytes, over the %d limit",
				len(id), maxEvidenceIDLen)
		}
		clean, err := sanitizeNote(id)
		if err != nil {
			return nil, err
		}
		if clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	sort.Strings(out)
	if len(out) > maxEvidenceIDs {
		return nil, fmt.Errorf("whatif: %d evidence ids, over the %d cap", len(out), maxEvidenceIDs)
	}
	return out, nil
}

// build turns a normalized spec plus an author and a clock into a Proposal,
// running the gate itself. It is unexported: Store.Create is the only way in,
// so no proposal can exist without a gate verdict this package computed.
func build(author Actor, s Spec, now time.Time) (Proposal, error) {
	baseHash, err := scorecardHash(s.BaselineScore)
	if err != nil {
		return Proposal{}, err
	}
	candHash, err := scorecardHash(s.CandidateScore)
	if err != nil {
		return Proposal{}, err
	}
	gate := Decide(GateInput{
		Baseline:       s.Baseline,
		Candidate:      s.Candidate,
		BaselineScore:  s.BaselineScore,
		CandidateScore: s.CandidateScore,
		Envelope:       s.Envelope,
		Tolerance:      s.Tolerance,
	})
	return Proposal{
		Kind:            s.Kind,
		Author:          author,
		Target:          s.Target,
		Baseline:        s.Baseline,
		Candidate:       s.Candidate,
		Changes:         changesBetween(s.Baseline, s.Candidate),
		Gate:            gate,
		Delta:           Diff(s.BaselineScore, s.CandidateScore),
		Cluster:         s.BaselineScore.Cluster,
		Window:          s.BaselineScore.Window,
		HorizonHours:    s.BaselineScore.HorizonHours,
		BaselineScore:   baseHash,
		CandidateScore:  candHash,
		BaselineRegret:  round6(s.BaselineScore.RegretUSD),
		CandidateRegret: round6(s.CandidateScore.RegretUSD),
		Rationale:       s.Rationale,
		EvidenceIDs:     s.EvidenceIDs,
		CreatedAt:       now.UTC(),
	}, nil
}
