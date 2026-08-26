package explain

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
)

// ID is a citation: a stable, parseable pointer at one row of the evidence
// substrate.
//
// The rule this package is built around (§5.6/§5.7): a number the reader
// cannot trace back to a stored observation must not be emitted at all. An
// uncitable term is worse than a missing one, because a missing term is
// visible in the residual while an uncitable term looks like knowledge.
// Every Term and every Driver therefore carries at least one ID, and every
// ID this package emits resolves through Resolver against the same store the
// answer was computed from.
//
// The wire format is one line of text so it can survive a JSON field, a CLI
// table, a log line and an LLM's quotation of it:
//
//	evt/<cluster>/<subjectKind>/<subjectKey>/<eventKind>@<unixNano>
//	dec/<cluster>/<subjectKind>/<subjectKey>@<unixNano>
//	dig/<tier>/<cluster>/<subjectKind>/<subjectKey>@<unixNano>   (digest window start)
//	tl/<cluster>@<unixNano>
//	act/<cluster>/<fingerprint>@<unixNano>
//
// Segments percent-escape '%', '/' and '@' (and only those), so parsing is
// exact for any subject key the substrate accepts — including the
// slash-bearing "ns/Deployment/name/container" container keys — while the
// overwhelmingly common case stays readable.
type ID string

// Citation kinds, the leading segment of an ID.
const (
	KindEvent    = "evt"
	KindDecision = "dec"
	KindDigest   = "dig"
	KindTimeline = "tl"
	KindAction   = "act"
)

// ErrBadID reports a malformed ID.
var ErrBadID = errors.New("explain: malformed evidence id")

// ErrNotFound reports a well-formed ID that no longer resolves — pruned,
// evicted, or never stored. Callers must treat it as "do not print the
// claim", not as "print it without the citation".
var ErrNotFound = errors.New("explain: evidence id does not resolve")

// escSeg percent-escapes the three delimiter bytes. Everything else is
// passed through: the substrate already strips control characters and
// invalid UTF-8 at ingest, so an ID is printable by construction.
func escSeg(s string) string {
	if !strings.ContainsAny(s, "%/@") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '%':
			b.WriteString("%25")
		case '/':
			b.WriteString("%2F")
		case '@':
			b.WriteString("%40")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// unescSeg reverses escSeg. It rejects any escape it did not produce, so
// parsing is a total inverse rather than a lenient guess.
func unescSeg(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("%w: truncated escape in %q", ErrBadID, s)
		}
		switch s[i : i+3] {
		case "%25":
			b.WriteByte('%')
		case "%2F":
			b.WriteByte('/')
		case "%40":
			b.WriteByte('@')
		default:
			return "", fmt.Errorf("%w: unknown escape %q", ErrBadID, s[i:i+3])
		}
		i += 3
	}
	return b.String(), nil
}

// stamp renders a timestamp as its UTC Unix nanoseconds — the substrate's
// own key, so an ID is literally the storage coordinate.
func stamp(t time.Time) string { return strconv.FormatInt(t.UTC().UnixNano(), 10) }

// EventID cites one evidence event.
func EventID(ev evidence.EvidenceEvent) ID {
	return ID(KindEvent + "/" + escSeg(ev.Subject.Cluster) + "/" + escSeg(ev.Subject.Kind) +
		"/" + escSeg(ev.Subject.Key) + "/" + escSeg(ev.Kind) + "@" + stamp(ev.At))
}

// DecisionID cites one journalled decision.
func DecisionID(d evidence.DecisionRecord) ID {
	return ID(KindDecision + "/" + escSeg(d.Subject.Cluster) + "/" + escSeg(d.Subject.Kind) +
		"/" + escSeg(d.Subject.Key) + "@" + stamp(d.At))
}

// DigestID cites one usage digest by its window start.
func DigestID(s evidence.SubjectRef, d evidence.Digest) ID {
	return ID(KindDigest + "/" + strconv.Itoa(d.Tier) + "/" + escSeg(s.Cluster) + "/" +
		escSeg(s.Kind) + "/" + escSeg(s.Key) + "@" + stamp(d.Start))
}

// TimelineID cites one cluster cost/demand observation.
func TimelineID(cluster string, p evidence.TimelinePoint) ID {
	return ID(KindTimeline + "/" + escSeg(cluster) + "@" + stamp(p.At))
}

// ActionID cites one ledger entry.
func ActionID(a LedgerAction) ID {
	return ID(KindAction + "/" + escSeg(a.Cluster) + "/" + escSeg(a.Fingerprint) + "@" + stamp(a.At))
}

// Ref is a parsed ID: the storage coordinate it names.
type Ref struct {
	Kind      string // KindEvent | KindDecision | KindDigest | KindTimeline | KindAction
	Cluster   string
	Subject   evidence.SubjectRef // zero for tl/ and act/
	EventKind string              // evt/ only
	Tier      int                 // dig/ only
	Token     string              // act/ only: the plan fingerprint
	At        time.Time           // UTC
}

// Parse decodes an ID. It is the exact inverse of the *ID constructors: any
// ID this package emits parses back to the coordinate it was built from,
// and nothing else parses at all.
func Parse(id ID) (Ref, error) {
	s := string(id)
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return Ref{}, fmt.Errorf("%w: no timestamp in %q", ErrBadID, s)
	}
	nanos, err := strconv.ParseInt(s[at+1:], 10, 64)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: bad timestamp in %q", ErrBadID, s)
	}
	head := strings.Split(s[:at], "/")
	seg := func(i int) (string, error) { return unescSeg(head[i]) }
	r := Ref{At: time.Unix(0, nanos).UTC()}
	switch {
	case len(head) == 5 && head[0] == KindEvent:
		r.Kind = KindEvent
		for i, dst := range []*string{nil, &r.Cluster, &r.Subject.Kind, &r.Subject.Key, &r.EventKind} {
			if dst == nil {
				continue
			}
			if *dst, err = seg(i); err != nil {
				return Ref{}, err
			}
		}
		r.Subject.Cluster = r.Cluster
	case len(head) == 4 && head[0] == KindDecision:
		r.Kind = KindDecision
		for i, dst := range []*string{nil, &r.Cluster, &r.Subject.Kind, &r.Subject.Key} {
			if dst == nil {
				continue
			}
			if *dst, err = seg(i); err != nil {
				return Ref{}, err
			}
		}
		r.Subject.Cluster = r.Cluster
	case len(head) == 5 && head[0] == KindDigest:
		r.Kind = KindDigest
		if r.Tier, err = strconv.Atoi(head[1]); err != nil {
			return Ref{}, fmt.Errorf("%w: bad tier in %q", ErrBadID, s)
		}
		for i, dst := range []*string{nil, nil, &r.Cluster, &r.Subject.Kind, &r.Subject.Key} {
			if dst == nil {
				continue
			}
			if *dst, err = seg(i); err != nil {
				return Ref{}, err
			}
		}
		r.Subject.Cluster = r.Cluster
	case len(head) == 2 && head[0] == KindTimeline:
		r.Kind = KindTimeline
		if r.Cluster, err = seg(1); err != nil {
			return Ref{}, err
		}
	case len(head) == 3 && head[0] == KindAction:
		r.Kind = KindAction
		if r.Cluster, err = seg(1); err != nil {
			return Ref{}, err
		}
		if r.Token, err = seg(2); err != nil {
			return Ref{}, err
		}
	default:
		return Ref{}, fmt.Errorf("%w: unrecognised shape %q", ErrBadID, s)
	}
	return r, nil
}

// Citation is a resolved ID: enough of the underlying record to render the
// claim next to its source, without handing the caller the whole substrate.
type Citation struct {
	ID      ID                  `json:"id"`
	Kind    string              `json:"kind"`
	At      time.Time           `json:"at"`
	Subject evidence.SubjectRef `json:"subject,omitzero"`
	Summary string              `json:"summary"`
}

// Resolver turns IDs back into records. It is the enforcement point for
// "citations must resolve to IDs the session actually fetched" (§5.7): the
// same store that produced an answer must be able to re-serve every ID in
// it, or the answer is not publishable.
type Resolver struct {
	Store   evidence.Store
	Actions []LedgerAction
}

// Resolve looks an ID up. A well-formed ID for a record that has since been
// pruned returns ErrNotFound — a fact worth surfacing, not papering over.
func (r Resolver) Resolve(id ID) (Citation, error) {
	ref, err := Parse(id)
	if err != nil {
		return Citation{}, err
	}
	c := Citation{ID: id, Kind: ref.Kind, At: ref.At, Subject: ref.Subject}
	// Half-open [At, At+1ns) selects exactly the nanosecond the ID names.
	lo, hi := ref.At, ref.At.Add(time.Nanosecond)
	switch ref.Kind {
	case KindEvent:
		if r.Store == nil {
			return Citation{}, fmt.Errorf("%w: no store", ErrNotFound)
		}
		evs, err := r.Store.Events(ref.Subject, lo, hi, ref.EventKind)
		if err != nil {
			return Citation{}, err
		}
		for _, ev := range evs {
			if ev.Kind == ref.EventKind && ev.At.Equal(ref.At) {
				c.Summary = ev.Kind + " (" + ev.Severity + ")"
				return c, nil
			}
		}
	case KindDecision:
		if r.Store == nil {
			return Citation{}, fmt.Errorf("%w: no store", ErrNotFound)
		}
		ds, err := r.Store.Decisions(ref.Subject, lo, hi)
		if err != nil {
			return Citation{}, err
		}
		for _, d := range ds {
			if d.At.Equal(ref.At) {
				c.Summary = d.Kind + ": " + d.Summary
				return c, nil
			}
		}
	case KindDigest:
		if r.Store == nil {
			return Citation{}, fmt.Errorf("%w: no store", ErrNotFound)
		}
		ds, err := r.Store.Digests(ref.Subject, lo, hi, ref.Tier)
		if err != nil {
			return Citation{}, err
		}
		for _, d := range ds {
			if d.Start.Equal(ref.At) {
				c.Summary = fmt.Sprintf("usage digest tier %d, %d samples", d.Tier, d.Samples)
				return c, nil
			}
		}
	case KindTimeline:
		if r.Store == nil {
			return Citation{}, fmt.Errorf("%w: no store", ErrNotFound)
		}
		pts, err := r.Store.Timeline(ref.Cluster, lo, hi)
		if err != nil {
			return Citation{}, err
		}
		for _, p := range pts {
			if p.At.Equal(ref.At) {
				c.Summary = fmt.Sprintf("%d nodes at %.6f USD/h", p.Nodes, p.CostUSDPerHour)
				return c, nil
			}
		}
	case KindAction:
		for _, a := range r.Actions {
			if a.Cluster == ref.Cluster && a.Fingerprint == ref.Token && a.At.UTC().Equal(ref.At) {
				c.Summary = "ledger " + a.Mode + " " + a.Fingerprint
				return c, nil
			}
		}
	}
	return Citation{}, fmt.Errorf("%w: %s", ErrNotFound, id)
}

// ResolveAll resolves every ID, returning the citations in the order given
// and the first failure. Used by tests and by any caller that must not
// publish an answer with a dangling citation.
func (r Resolver) ResolveAll(ids []ID) ([]Citation, error) {
	out := make([]Citation, 0, len(ids))
	for _, id := range ids {
		c, err := r.Resolve(id)
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, nil
}

// maxEvidencePerTerm caps how many IDs one term or driver carries. Citations
// exist to be checked by a human or quoted by a model with a context budget;
// an unbounded list is neither. Truncation is always reported in Notes.
const maxEvidencePerTerm = 16

// dedupeIDs sorts and de-duplicates, then truncates to at most max, keeping
// the lexicographically smallest — a total order, so the kept subset is a
// function of the set and not of the discovery order. It reports how many
// were dropped so nothing vanishes silently.
func dedupeIDs(ids []ID, max int) (kept []ID, dropped int) {
	if len(ids) == 0 {
		return nil, 0
	}
	s := make([]ID, len(ids))
	copy(s, ids)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	out := s[:0]
	for i, id := range s {
		if i == 0 || id != s[i-1] {
			out = append(out, id)
		}
	}
	if max >= 0 && len(out) > max {
		return out[:max], len(out) - max
	}
	return out, 0
}
