package reason

import (
	"context"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/explain"
)

// The read-only tools of §5.2 that this unit implements. Each is a bounded
// projection of the deterministic substrate; none of them can write, and none
// of them accepts a free-text query — no PromQL, no SQL, no label selector, no
// path. Every argument is a name from a closed set, a bounded integer, or an
// instant, which is what makes the schema in schema.go a complete description
// of the attack surface rather than a first line of it.
//
// FINDINGS.md §7 lists the §5.2 tools that are NOT here and what each one
// needs from a package above this one.
const (
	ToolListSubjects    = "list_subjects"
	ToolGetDossier      = "get_dossier"
	ToolQueryEvidence   = "query_evidence"
	ToolClusterTimeline = "get_cluster_timeline"
	ToolExplain         = "get_recommendation_explain"
)

// Bounds the tools enforce. These are the §5.2 numbers: limit ≤ 50, window
// ≤ 90 days, dossier ~4 KiB.
const (
	maxRowLimit      = 50
	maxEvidenceSpan  = 90 * 24 * time.Hour
	maxTimelineRows  = 48
	maxDossierEvents = 24
	maxDrivers       = 8
	maxDriverCites   = 4
)

// builtinTools constructs the registry's tool set. It returns them in
// declaration order; the registry sorts.
func builtinTools() ([]Tool, error) {
	subjectKind := Enum("subject_kind", "which kind of subject subject_key names", false,
		evidence.SubjectContainer, evidence.SubjectWorkload, evidence.SubjectNode, evidence.SubjectCluster)
	subjectKey := OptIdent("subject_key",
		"the subject's key exactly as list_subjects reported it; compared byte for byte and never normalized. "+
			"Use subject_index instead when list_subjects reported a key you cannot reproduce exactly.")
	subjectIndex := Quantity("subject_index",
		"the index list_subjects reported for this subject. Preferred: it selects a subject without "+
			"repeating a cluster-authored name, and it is the only way to reach a subject whose name "+
			"carries characters that are stripped for display.", -1, 1<<20, -1)
	from := Instant("from", "RFC3339 start of the window; clamped into the investigation's scope")
	to := Instant("to", "RFC3339 end of the window, exclusive; clamped into the investigation's scope")

	listSchema, err := NewSchema(
		Enum("kind", "restrict to one subject kind", false,
			evidence.SubjectContainer, evidence.SubjectWorkload, evidence.SubjectNode, evidence.SubjectCluster),
		OptIdent("key_prefix", "return only subjects whose key starts with this literal prefix"),
		Quantity("limit", "how many rows to return", 1, maxRowLimit, 20),
		Quantity("offset", "how many rows to skip; pagination is a stable total order over (kind, key)", 0, 1<<20, 0),
	)
	if err != nil {
		return nil, err
	}

	dossierSchema, err := NewSchema(subjectKind, subjectKey, subjectIndex, from, to,
		Quantity("events", "how many recent events to include", 0, maxDossierEvents, 12),
	)
	if err != nil {
		return nil, err
	}

	evidenceSchema, err := NewSchema(subjectKind, subjectKey, subjectIndex, from, to,
		IdentList("kinds", "restrict to these event kinds; empty means every kind", 8),
		Quantity("limit", "how many events to return, newest first", 1, maxRowLimit, 20),
	)
	if err != nil {
		return nil, err
	}

	timelineSchema, err := NewSchema(from, to,
		Quantity("points", "how many timeline points to return, evenly spaced across the window", 2, maxTimelineRows, 24),
	)
	if err != nil {
		return nil, err
	}

	explainSchema, err := NewSchema(subjectKind, subjectKey, subjectIndex, from, to)
	if err != nil {
		return nil, err
	}

	out := make([]Tool, 0, 5)
	for _, spec := range []struct {
		name, desc string
		schema     Schema
		run        ToolFunc
	}{
		{ToolListSubjects,
			"List the subjects the substrate holds for this cluster, in a stable (kind, key) order. " +
				"The entry point: every other tool takes a key this one reported.",
			listSchema, runListSubjects},
		{ToolGetDossier,
			"The bounded case file for one subject: usage percentiles, recent events, recent decisions and " +
				"usage digests over the window. The retrieval unit; roughly 4 KiB.",
			dossierSchema, runGetDossier},
		{ToolQueryEvidence,
			"Typed evidence events for one subject over a window of at most 90 days, newest first. " +
				"Each row carries the citation id that grounds it.",
			evidenceSchema, runQueryEvidence},
		{ToolClusterTimeline,
			"The cluster's cost and node-count timeline over the window, evenly sampled.",
			timelineSchema, runClusterTimeline},
		{ToolExplain,
			"The deterministic explanation behind one subject's sizing decision: every input to the number, " +
				"the confidence basis, any refusal, and the drivers with their evidence. " +
				"This is the only sanctioned source for explanation prose — do not reconstruct a number from raw events.",
			explainSchema, runExplain},
	} {
		t, err := readOnlyTool(spec.name, spec.desc, spec.schema, DefaultToolTimeout, spec.run)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// subjectOf resolves the subject arguments against the scope and the
// enumerable universe. A call selects a subject one of two ways, and exactly
// one of them.
//
// # subject_index, and why it is the preferred selector
//
// A subject key is a workload name somebody with kubectl wrote. Everything
// this package shows a model is scrubbed of control, zero-width and bidi
// runes — which means the key the model reads back from list_subjects is not
// always the key the substrate holds. Accepting the scrubbed form would
// resolve one name to a different object; refusing it outright would make a
// hostile-named workload permanently uninvestigatable, which is a denial of
// service an attacker arranges with one annotation.
//
// The index closes both: it is a bounded integer over the same total order
// list_subjects enumerates, it carries no cluster-authored bytes at all, and
// it can only name a subject the model has already enumerated.
//
// # Membership is enforced for keys
//
// A key that is not in the universe is refused rather than answered with an
// empty result. "No events for that subject" is indistinguishable from "no
// such subject", and only one of those is worth an operator's attention.
func subjectOf(in Input) (evidence.SubjectRef, *Refusal) {
	idx := in.Args.Int("subject_index")
	key := in.Args.Str("subject_key")
	kind := in.Args.Str("subject_kind")

	if idx >= 0 && (key != "" || kind != "") {
		return evidence.SubjectRef{}, refuse(CodeAmbiguousSubject, "subject_index")
	}
	if idx >= 0 {
		scoped := scopedSubjects(in)
		if int(idx) >= len(scoped) {
			return evidence.SubjectRef{}, refuse(CodeOutOfScope, "subject_index")
		}
		return scoped[idx], nil
	}
	if key == "" || kind == "" {
		return evidence.SubjectRef{}, refuse(CodeMissingArgument, "subject_index")
	}

	s := evidence.SubjectRef{Cluster: in.Scope.Cluster, Kind: kind, Key: key}
	if len(in.Subjects) == 0 {
		return s, nil
	}
	i := sort.Search(len(in.Subjects), func(i int) bool {
		o := in.Subjects[i]
		if o.Cluster != s.Cluster {
			return o.Cluster >= s.Cluster
		}
		if o.Kind != s.Kind {
			return o.Kind >= s.Kind
		}
		return o.Key >= s.Key
	})
	if i < len(in.Subjects) && in.Subjects[i] == s {
		return s, nil
	}
	return evidence.SubjectRef{}, refuse(CodeOutOfScope, "subject_key")
}

// scopedSubjects is the cluster's slice of the universe, in the one order
// list_subjects enumerates and subject_index counts.
func scopedSubjects(in Input) []evidence.SubjectRef {
	out := make([]evidence.SubjectRef, 0, len(in.Subjects))
	for _, s := range in.Subjects {
		if s.Cluster == in.Scope.Cluster {
			out = append(out, s)
		}
	}
	return out
}

type subjectRow struct {
	// Index is this subject's position in the cluster's total order — the
	// value subject_index takes. It counts the whole universe, not the
	// filtered page, so a filtered listing still yields usable indices.
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	Key   string `json:"key"`
}

type listSubjectsOut struct {
	Cluster  string       `json:"cluster"`
	Matched  int          `json:"matched"`
	Offset   int          `json:"offset"`
	Returned int          `json:"returned"`
	More     bool         `json:"more"`
	Subjects []subjectRow `json:"subjects"`
}

func runListSubjects(_ context.Context, in Input) (Result, error) {
	kind := in.Args.Str("kind")
	prefix := in.Args.Str("key_prefix")
	limit := int(in.Args.Int("limit"))
	offset := int(in.Args.Int("offset"))

	out := listSubjectsOut{Cluster: in.Scope.Cluster, Offset: offset, Subjects: []subjectRow{}}
	for i, s := range scopedSubjects(in) { // already in (cluster, kind, key) order
		if kind != "" && s.Kind != kind {
			continue
		}
		if prefix != "" && !hasPrefix(s.Key, prefix) {
			continue
		}
		out.Matched++
		if out.Matched <= offset || len(out.Subjects) >= limit {
			continue
		}
		out.Subjects = append(out.Subjects, subjectRow{Index: i, Kind: s.Kind, Key: s.Key})
	}
	out.Returned = len(out.Subjects)
	out.More = out.Matched > offset+out.Returned
	return result(out, nil, nil)
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

type dossierOut struct {
	Subject evidence.SubjectRef `json:"subject"`
	Dossier *evidence.Dossier   `json:"dossier"`
}

func runGetDossier(_ context.Context, in Input) (Result, error) {
	s, ref := subjectOf(in)
	if ref != nil {
		return Result{}, ref
	}
	from, to, clamps, ref := clampWindow(in, "from", "to", 0)
	if ref != nil {
		return Result{}, ref
	}
	dos, err := evidence.BuildDossier(in.ro, evidence.DossierRequest{
		Subject:      s,
		From:         from,
		To:           to,
		MaxBytes:     evidence.DefaultDossierBytes,
		MaxEvents:    int(in.Args.Int("events")),
		MaxDecisions: 8,
		MaxDigests:   24,
		DigestTier:   evidence.TierHourly,
	})
	if err != nil {
		return Result{}, err
	}
	return result(dossierOut{Subject: s, Dossier: dos}, dossierCitations(s, dos), clamps)
}

// dossierCitations is what the dossier just showed the model. Digests are
// cited by window start, events and decisions by their exact nanosecond —
// the same coordinates pkg/explain resolves.
func dossierCitations(s evidence.SubjectRef, d *evidence.Dossier) []explain.ID {
	if d == nil {
		return nil
	}
	ids := make([]explain.ID, 0, len(d.Events)+len(d.Decisions)+len(d.Digests))
	for _, ev := range d.Events {
		ids = append(ids, explain.EventID(ev))
	}
	for _, dec := range d.Decisions {
		ids = append(ids, explain.DecisionID(dec))
	}
	for _, dg := range d.Digests {
		ids = append(ids, explain.DigestID(s, dg))
	}
	return ids
}

type eventRow struct {
	ID       explain.ID `json:"id"`
	At       time.Time  `json:"at"`
	Kind     string     `json:"kind"`
	Severity string     `json:"severity"`
	Count    int        `json:"count,omitempty"`
	Attrs    []kv       `json:"attrs,omitempty"`
}

type queryEvidenceOut struct {
	Subject  evidence.SubjectRef `json:"subject"`
	From     time.Time           `json:"from"`
	To       time.Time           `json:"to"`
	Matched  int                 `json:"matched"`
	Returned int                 `json:"returned"`
	Events   []eventRow          `json:"events"`
}

func runQueryEvidence(_ context.Context, in Input) (Result, error) {
	s, ref := subjectOf(in)
	if ref != nil {
		return Result{}, ref
	}
	from, to, clamps, ref := clampWindow(in, "from", "to", maxEvidenceSpan)
	if ref != nil {
		return Result{}, ref
	}
	evs, err := in.Read.Events(s, from, to, in.Args.List("kinds")...)
	if err != nil {
		return Result{}, err
	}
	limit := int(in.Args.Int("limit"))
	out := queryEvidenceOut{Subject: s, From: from, To: to, Matched: len(evs), Events: []eventRow{}}
	ids := make([]explain.ID, 0, limit)
	// Newest first: the substrate returns oldest first, and the useful end of
	// an event list under a row cap is the recent end.
	for i := len(evs) - 1; i >= 0 && len(out.Events) < limit; i-- {
		ev := evs[i]
		id := explain.EventID(ev)
		out.Events = append(out.Events, eventRow{
			ID:       id,
			At:       ev.At,
			Kind:     ev.Kind,
			Severity: ev.Severity,
			Count:    ev.Count,
			Attrs:    sortedKV(ev.Attrs, maxDisplayText),
		})
		ids = append(ids, id)
	}
	out.Returned = len(out.Events)
	return result(out, ids, clamps)
}

type timelineRow struct {
	ID             explain.ID `json:"id"`
	At             time.Time  `json:"at"`
	CostUSDPerHour float64    `json:"costUSDPerHour"`
	Nodes          int        `json:"nodes"`
	Events         int        `json:"events,omitempty"`
}

type timelineOut struct {
	Cluster  string        `json:"cluster"`
	From     time.Time     `json:"from"`
	To       time.Time     `json:"to"`
	Stored   int           `json:"stored"`
	Returned int           `json:"returned"`
	Points   []timelineRow `json:"points"`
}

func runClusterTimeline(_ context.Context, in Input) (Result, error) {
	from, to, clamps, ref := clampWindow(in, "from", "to", 0)
	if ref != nil {
		return Result{}, ref
	}
	pts, err := in.Read.Timeline(in.Scope.Cluster, from, to)
	if err != nil {
		return Result{}, err
	}
	want := int(in.Args.Int("points"))
	out := timelineOut{Cluster: in.Scope.Cluster, From: from, To: to, Stored: len(pts), Points: []timelineRow{}}
	ids := make([]explain.ID, 0, want)
	for _, i := range evenIndices(len(pts), want) {
		p := pts[i]
		id := explain.TimelineID(in.Scope.Cluster, p)
		out.Points = append(out.Points, timelineRow{
			ID:             id,
			At:             p.At,
			CostUSDPerHour: p.CostUSDPerHour,
			Nodes:          p.Nodes,
			Events:         len(p.Events),
		})
		ids = append(ids, id)
	}
	out.Returned = len(out.Points)
	return result(out, ids, clamps)
}

// evenIndices picks at most want indices out of n, always including the first
// and the last, evenly spaced and strictly increasing.
//
// Sampling rather than truncating: a cost timeline read from one end tells
// the model what happened recently and nothing about the shape of the window
// it was asked about, which is how "cost rose on the 14th" becomes invisible.
func evenIndices(n, want int) []int {
	if n <= 0 || want <= 0 {
		return nil
	}
	if n <= want {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	if want == 1 {
		return []int{0}
	}
	out := make([]int, 0, want)
	last := -1
	for i := 0; i < want; i++ {
		idx := int(int64(i) * int64(n-1) / int64(want-1))
		if idx > last {
			out = append(out, idx)
			last = idx
		}
	}
	return out
}

type driverOut struct {
	Kind     string       `json:"kind"`
	Name     string       `json:"name,omitempty"`
	Detail   string       `json:"detail"`
	Value    float64      `json:"value,omitempty"`
	Evidence []explain.ID `json:"evidence"`
}

type explainOut struct {
	Subject    evidence.SubjectRef     `json:"subject"`
	From       time.Time               `json:"from"`
	To         time.Time               `json:"to"`
	Action     string                  `json:"action"`
	Sizing     *explain.Sizing         `json:"sizing,omitempty"`
	Refusal    any                     `json:"refusal,omitempty"`
	Usage      evidence.UsageSummary   `json:"usage"`
	Drivers    []driverOut             `json:"drivers"`
	Notes      []string                `json:"notes,omitempty"`
	Truncated  *evidence.Truncation    `json:"truncated,omitempty"`
	Citations  []explain.ID            `json:"citations"`
	Confidence *explainConfidenceScore `json:"confidence,omitempty"`
}

// explainConfidenceScore is the engine's own confidence, projected flat and
// labelled. It is kept separate from the model's self-reported confidence in
// the finding for the reason §5.3 gives: two numbers called "confidence" that
// mean different things must never be rendered as one.
type explainConfidenceScore struct {
	Score float64 `json:"score"`
	Terms int     `json:"terms"`
}

func runExplain(_ context.Context, in Input) (Result, error) {
	s, ref := subjectOf(in)
	if ref != nil {
		return Result{}, ref
	}
	from, to, clamps, ref := clampWindow(in, "from", "to", 0)
	if ref != nil {
		return Result{}, ref
	}
	if in.explain == nil {
		return Result{}, refuse(CodeToolFailed, "")
	}
	ex, err := in.explain(s, from, to)
	if err != nil {
		return Result{}, err
	}
	out := explainOut{
		Subject:   ex.Subject,
		From:      ex.From,
		To:        ex.To,
		Action:    ex.Action,
		Sizing:    ex.Sizing,
		Usage:     ex.Usage,
		Notes:     ex.Notes,
		Truncated: ex.Truncated,
		Citations: ex.Citations,
		Drivers:   []driverOut{},
	}
	if ex.Refusal != nil {
		out.Refusal = ex.Refusal
	}
	if ex.Confidence != nil {
		out.Confidence = &explainConfidenceScore{Score: ex.Confidence.Score, Terms: len(ex.Confidence.Basis)}
	}
	for i, d := range ex.Drivers {
		if i >= maxDrivers {
			break
		}
		cites := d.Evidence
		if len(cites) > maxDriverCites {
			cites = cites[:maxDriverCites]
		}
		out.Drivers = append(out.Drivers, driverOut{
			Kind:     d.Kind,
			Name:     d.Name,
			Detail:   d.Detail,
			Value:    d.Value,
			Evidence: cites,
		})
	}
	return result(out, ex.Citations, clamps)
}
