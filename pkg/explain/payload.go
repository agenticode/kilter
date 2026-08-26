package explain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/recommend"
)

// Driver kinds. One per reason the engine can give for a sizing decision;
// each is grounded in a specific class of stored observation.
const (
	DriverRefusal     = "refusal"
	DriverConfidence  = "confidence"
	DriverOOMFloor    = "oom-floor"
	DriverThrottled   = "cpu-throttled"
	DriverRecentEvent = "recent-change"
	DriverHPAGuard    = "hpa-cpu-guard"
	DriverClass       = "behavior-class"
	DriverHistory     = "usage-history"
	DriverPrior       = "prior-decision"
)

// throttleNoticeRatio is the mean CFS throttle ratio above which throttling
// is worth telling a human about. 5% of periods throttled is roughly where
// tail latency starts moving on a typical request/response service, and it is
// far above the sub-1% noise a healthy container shows. It only gates a
// *sentence*: no sizing decision is made here.
const throttleNoticeRatio = 0.05

// explainDossierBytes is the byte cap for the dossier BuildExplain composes
// from. Larger than the 4 KiB LLM-retrieval default because an explanation is
// rendered for a human or an API response, not squeezed into a context
// window; still bounded, because everything here is bounded.
const explainDossierBytes = 32 << 10

// Driver is one reason behind a decision, with the evidence that grounds it.
//
// A Driver with no evidence is never emitted. That is not tidiness: the
// narrating plane in §5 is required to cite this package, so an ungrounded
// reason is a sentence a model could repeat with nothing behind it. Drops are
// counted in [Explanation.Ungrounded] so the omission is itself visible.
type Driver struct {
	Kind     string  `json:"kind"`
	Name     string  `json:"name,omitempty"`
	Detail   string  `json:"detail"`
	Value    float64 `json:"value,omitempty"`
	Evidence []ID    `json:"evidence"`
	// EvidenceTruncated counts ids the per-driver cap dropped.
	EvidenceTruncated int `json:"evidenceTruncated,omitempty"`
}

// Sizing is the numeric core of an explanation: what the engine would change
// and by how much.
type Sizing struct {
	Container      string          `json:"container"`
	CurrentRequest model.Resources `json:"currentRequest"`
	TargetRequest  model.Resources `json:"targetRequest"`
	CurrentLimit   model.Resources `json:"currentLimit"`
	TargetLimit    model.Resources `json:"targetLimit"`
	// DeltaRequest is current − target: positive means the request shrinks.
	DeltaRequest model.Resources `json:"deltaRequest"`
	Class        string          `json:"class,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Samples      int             `json:"samples"`
	WindowHours  float64         `json:"windowHours"`
	OOMCount     int             `json:"oomCount,omitempty"`
	CPUSkipped   bool            `json:"cpuSkipped,omitempty"`
	// SavingsMonthlyUSD is the caller's priced value of applying this
	// change. Present only when the caller supplied one — this package
	// prices nothing itself and will not guess.
	SavingsMonthlyUSD *float64 `json:"savingsMonthlyUSD,omitempty"`
}

// Explanation is the deterministic explain payload for one subject: the
// decision, the numbers, the drivers, and a citation set that resolves.
type Explanation struct {
	Cluster string              `json:"cluster"`
	Subject evidence.SubjectRef `json:"subject"`
	From    time.Time           `json:"from"`
	To      time.Time           `json:"to"`
	Action  string              `json:"action"` // act | recommend-only | refuse | unknown
	Sizing  *Sizing             `json:"sizing,omitempty"`
	// Confidence is the scored basis behind the verdict; each of its terms
	// becomes a Driver below, individually grounded.
	Confidence *decision.Confidence `json:"confidence,omitempty"`
	Refusal    *decision.Refusal    `json:"refusal,omitempty"`

	Usage     evidence.UsageSummary     `json:"usage"`
	Drivers   []Driver                  `json:"drivers"`
	Events    []evidence.EvidenceEvent  `json:"events,omitempty"`
	Decisions []evidence.DecisionRecord `json:"decisions,omitempty"`

	// Citations is every ID the payload leans on, sorted and de-duplicated.
	// Verify re-resolves all of them.
	Citations []ID `json:"citations"`
	// Ungrounded counts drivers dropped for want of an evidence id. Nothing
	// vanishes silently.
	Ungrounded int `json:"ungrounded,omitempty"`
	// Truncated mirrors the substrate's own truncation report when the
	// dossier the payload was built from hit a cap.
	Truncated *evidence.Truncation `json:"truncated,omitempty"`
	Notes     []string             `json:"notes,omitempty"`

	// VerdictOrigin says which kind of absence Action == unknown is, and is
	// present only when the verdict was not computed. See [VerdictState].
	VerdictOrigin *VerdictOrigin `json:"verdictOrigin,omitempty"`
	// Grounding is the evidence arithmetic behind the payload, present only
	// when the payload is not grounded — i.e. exactly when a caller needs to
	// act on it. See [Explanation.GroundingState] and
	// [Explanation.GroundingError].
	Grounding *Grounding `json:"grounding,omitempty"`
}

// ActionUnknown is the Action of an explanation built without a Verdict —
// the payload still explains the sizing and its evidence, it simply has no
// disposition to report.
const ActionUnknown = "unknown"

// ExplainRequest is everything BuildExplain may look at. As with WhyCost the
// window is an argument: there is no clock in this package.
type ExplainRequest struct {
	Cluster string
	Subject evidence.SubjectRef
	// From and To bound the evidence window. Both are required: an
	// explanation whose window drifts with wall-clock time is not replayable,
	// and replayability is the point.
	From, To time.Time
	Store    evidence.Store

	Rec *recommend.Recommendation
	// Verdict is the decision-quality verdict the operational path reached,
	// copied into the payload unchanged. This package never computes one:
	// see verdict.go's file comment for why a second evaluation here would
	// be a fabricated audit trail.
	Verdict *decision.Verdict
	// RecVerdict is the production recommendation path's readout for this
	// subject — one element of recommend.Recommender.Verdicts. It supplies
	// the verdict when the path reached one (Decision()'s comma-ok, copied
	// verbatim) and the typed "not computed" state when it did not, which is
	// every readout the recommender produces today.
	//
	// Supply this OR Verdict, never both: two sources for one disposition is
	// how a payload ends up reporting a verdict nobody reached.
	RecVerdict *recommend.Verdict

	// SavingsMonthlyUSD is priced by the caller (pkg/plan owns that math).
	// Reported only when SavingsKnown is set — an explicit flag beats a zero
	// sentinel, because zero savings is a real and interesting answer.
	SavingsMonthlyUSD float64
	SavingsKnown      bool

	MaxEvents    int // default 12; negative omits events
	MaxDecisions int // default 4;  negative omits decisions
	MaxDigests   int // default 24; negative omits digests
	DigestTier   int // default evidence.TierHourly
}

func (r *ExplainRequest) withDefaults() {
	if r.MaxEvents == 0 {
		r.MaxEvents = 12
	}
	if r.MaxDecisions == 0 {
		r.MaxDecisions = 4
	}
	if r.MaxDigests == 0 {
		r.MaxDigests = 24
	}
	if r.DigestTier == 0 {
		r.DigestTier = evidence.TierHourly
	}
}

func (r *ExplainRequest) validate() error {
	if r.Store == nil {
		return fmt.Errorf("explain: BuildExplain needs an evidence store")
	}
	if r.Subject.Kind == "" || r.Subject.Key == "" {
		return fmt.Errorf("explain: BuildExplain needs a subject")
	}
	if r.From.IsZero() || r.To.IsZero() {
		return fmt.Errorf("explain: BuildExplain needs a bounded window (the window is an argument, never a clock)")
	}
	if !r.To.After(r.From) {
		return fmt.Errorf("explain: window [%v, %v) is empty or inverted", r.From, r.To)
	}
	if r.SavingsKnown && (math.IsNaN(r.SavingsMonthlyUSD) || math.IsInf(r.SavingsMonthlyUSD, 0)) {
		return fmt.Errorf("explain: savings %v is not a usable amount", r.SavingsMonthlyUSD)
	}
	return r.validateVerdict()
}

// validateVerdict enforces that the payload has exactly one source for its
// disposition, and that the source is one a verdict could actually come from.
// Every rule here refuses a fabrication rather than a typo.
func (r *ExplainRequest) validateVerdict() error {
	if r.Verdict != nil && r.RecVerdict != nil {
		return fmt.Errorf("explain: Verdict and RecVerdict are two sources for one disposition; " +
			"supply exactly one, because the payload cannot report both and must not pick")
	}
	if v := r.Verdict; v != nil {
		switch v.Action {
		case decision.ActionAct, decision.ActionRecommendOnly, decision.ActionRefuse:
		default:
			// A zero decision.Verdict has Action "", which used to render as
			// a blank verdict — a payload asserting a disposition that is
			// not one. decision.Decide cannot produce it, so this is a bug
			// at the call site and says so rather than degrading quietly.
			return fmt.Errorf("explain: verdict action %q is not one of %q, %q or %q; "+
				"a verdict with no action is not a verdict",
				v.Action, decision.ActionAct, decision.ActionRecommendOnly, decision.ActionRefuse)
		}
	}
	rv := r.RecVerdict
	if rv == nil {
		return nil
	}
	// The readout names a container. Attributing one container's disposition
	// to another subject is the same fabrication as inventing it.
	if r.Subject.Kind != evidence.SubjectContainer {
		return fmt.Errorf("explain: RecVerdict is a container readout but the subject is %q; "+
			"a workload has no single disposition to report", r.Subject.Kind)
	}
	if got := rv.Key.String(); got != r.Subject.Key {
		return fmt.Errorf("explain: RecVerdict is about %s but the subject is %s; "+
			"a readout may only explain the container it is about", got, r.Subject.Key)
	}
	if r.Rec == nil {
		return nil
	}
	if rv.Rec == nil {
		return fmt.Errorf("explain: RecVerdict reports disposition %q, which carries no recommendation, "+
			"but Rec was supplied; the sizing and the disposition would come from different answers",
			rv.Disposition)
	}
	if *r.Rec != *rv.Rec {
		return fmt.Errorf("explain: Rec and RecVerdict.Rec size %s differently; "+
			"one of them is stale and the payload must not choose", rv.Key)
	}
	return nil
}

// BuildExplain assembles the deterministic explain payload for one subject.
//
// It reads the substrate through the same bounded, size-capped dossier
// builder the UI and MCP use, so an explanation can never be a different view
// of history than the one everything else sees.
func BuildExplain(req ExplainRequest) (*Explanation, error) {
	req.withDefaults()
	if err := req.validate(); err != nil {
		return nil, err
	}
	subject := req.Subject
	if subject.Cluster == "" {
		subject.Cluster = req.Cluster
	}
	dos, err := evidence.BuildDossier(req.Store, evidence.DossierRequest{
		Subject:      subject,
		From:         req.From,
		To:           req.To,
		MaxBytes:     explainDossierBytes,
		MaxEvents:    req.MaxEvents,
		MaxDecisions: req.MaxDecisions,
		MaxDigests:   req.MaxDigests,
		DigestTier:   req.DigestTier,
	})
	if err != nil {
		return nil, err
	}

	// Deploys and HPA actions are recorded against the *workload*, not the
	// container template inside it, so a container's case file is missing
	// exactly the events that explain a post-change refusal. Pull the parent
	// workload's change events in as well, under their own bound.
	// ownEvents is fixed before the parent workload's events are folded in:
	// everything after this point can tell the subject's own record from a
	// borrowed one, which is what makes "absent" a statement about the
	// subject rather than about its workload. See grounding.go.
	events := dos.Events
	ownEvents := len(dos.Events)
	if parent, ok := parentWorkload(subject); ok && req.MaxEvents > 0 {
		pe, err := req.Store.Events(parent, req.From, req.To,
			evidence.EventDeploy, evidence.EventHPAScale, evidence.EventRegimeChange)
		if err != nil {
			return nil, err
		}
		events = mergeEvents(events, pe, req.MaxEvents)
	}

	ex := &Explanation{
		Cluster:   req.Cluster,
		Subject:   subject,
		From:      req.From.UTC(),
		To:        req.To.UTC(),
		Action:    ActionUnknown,
		Usage:     dos.Usage,
		Events:    events,
		Decisions: dos.Decisions,
		Truncated: dos.Truncated,
	}
	// The verdict. There is exactly one source for it and this package is
	// never that source: whichever of the two inputs the caller supplied, the
	// decision.Verdict below is COPIED, never derived. Nothing in this
	// function calls decision.Evaluate, decision.Decide or decision.Compose.
	verdict := req.Verdict
	rec := req.Rec
	if rv := req.RecVerdict; rv != nil {
		if d, ok := rv.Decision(); ok {
			// The production path reached a verdict. Report that one.
			verdict = &d
		} else {
			// It did not. "Not computed" is the fact, and it is a different
			// fact from a refusal — Action stays unknown, Refusal stays nil,
			// and the origin says which of the four branches produced the
			// silence.
			ex.VerdictOrigin = originOf(rv)
			ex.Notes = append(ex.Notes, ex.VerdictOrigin.notComputedNote())
		}
		if rec == nil {
			// Byte-for-byte the Recommendation the production path served
			// for this container, or nil for the three silent dispositions.
			rec = rv.Rec
		}
	}
	if verdict != nil {
		ex.Action = string(verdict.Action)
		ex.Refusal = verdict.Refusal
		conf := verdict.Confidence
		ex.Confidence = &conf
	}
	if rec != nil {
		ex.Sizing = sizingOf(rec, req.SavingsMonthlyUSD, req.SavingsKnown)
	}

	// Citation pools, computed once and shared by the drivers below.
	digestIDs := digestCitations(subject, dos.Digests)
	oomIDs := concatIDs(eventIDs(events, evidence.EventOOMKill),
		digestsWhere(subject, dos.Digests, func(d evidence.Digest) bool { return d.OOMs > 0 }))
	throttleIDs := concatIDs(eventIDs(events, evidence.EventThrottleHigh),
		digestsWhere(subject, dos.Digests, func(d evidence.Digest) bool { return d.ThrottleRatio >= throttleNoticeRatio }))
	changeIDs := eventIDs(events, evidence.EventDeploy, evidence.EventHPAScale, evidence.EventRegimeChange)
	hpaIDs := eventIDs(events, evidence.EventHPAScale)
	decisionIDs := decisionCitations(dos.Decisions)

	var drivers []Driver
	push := func(d Driver, pool ...[]ID) {
		var all []ID
		for _, p := range pool {
			all = append(all, p...)
		}
		ids, dropped := dedupeIDs(all, maxEvidencePerTerm)
		if len(ids) == 0 {
			ex.Ungrounded++
			return
		}
		d.Evidence, d.EvidenceTruncated = ids, dropped
		drivers = append(drivers, d)
	}

	if ex.Refusal != nil {
		detail := ex.Refusal.Detail
		if detail == "" {
			detail = string(ex.Refusal.Code)
		}
		push(Driver{Kind: DriverRefusal, Name: string(ex.Refusal.Code), Detail: detail},
			refusalCitations(ex.Refusal.Code, digestIDs, changeIDs, oomIDs, throttleIDs, decisionIDs))
	}
	if ex.Confidence != nil {
		for _, t := range ex.Confidence.Basis {
			detail := t.Note
			if detail == "" {
				detail = fmt.Sprintf("confidence term %q contributes %s at weight %s",
					t.Name, strconv.FormatFloat(t.Value, 'f', 2, 64), strconv.FormatFloat(t.Weight, 'f', 2, 64))
			}
			push(Driver{Kind: DriverConfidence, Name: t.Name, Detail: detail, Value: t.Value},
				confidenceCitations(t.Name, digestIDs, changeIDs, decisionIDs))
		}
	}
	if rec != nil {
		if rec.OOMCount > 0 {
			push(Driver{Kind: DriverOOMFloor, Detail: fmt.Sprintf(
				"%d OOMKill%s in the window %s the memory request at or above its floor",
				rec.OOMCount, pluralS(rec.OOMCount),
				pluralVerb(rec.OOMCount, "holds", "hold"))}, oomIDs)
		}
		if rec.CPUSkipped {
			push(Driver{Kind: DriverHPAGuard,
				Detail: "an HPA scales this workload on CPU, so the CPU request is left alone"}, hpaIDs, decisionIDs)
		}
		if rec.Class != "" {
			push(Driver{Kind: DriverClass, Name: string(rec.Class), Detail: fmt.Sprintf(
				"behavior class %q selects the sizing policy applied", rec.Class)}, digestIDs)
		}
	}
	if dos.Usage.Samples > 0 {
		push(Driver{Kind: DriverHistory, Detail: fmt.Sprintf(
			"%d sample%s over %s: cpu p95 %.0fm / max %.0fm, memory p95 %s / max %s",
			dos.Usage.Samples, pluralS64(dos.Usage.Samples),
			req.To.Sub(req.From).Round(time.Minute),
			dos.Usage.CPU.P95, dos.Usage.CPU.Max,
			humanBytes(dos.Usage.Mem.P95), humanBytes(dos.Usage.Mem.Max)),
			Value: dos.Usage.CPU.P95}, digestIDs)
	}
	if dos.Usage.ThrottleRatio >= throttleNoticeRatio {
		push(Driver{Kind: DriverThrottled, Value: dos.Usage.ThrottleRatio, Detail: fmt.Sprintf(
			"CFS throttling averaged %s of periods; shrinking CPU here would make it worse",
			formatRatio(dos.Usage.ThrottleRatio))}, throttleIDs)
	}
	if len(changeIDs) > 0 {
		push(Driver{Kind: DriverRecentEvent, Detail: fmt.Sprintf(
			"%d change event%s (deploy, HPA or regime change) %s inside the window",
			len(changeIDs), pluralS(len(changeIDs)),
			pluralVerb(len(changeIDs), "falls", "fall"))}, changeIDs)
	}
	if len(dos.Decisions) > 0 {
		push(Driver{Kind: DriverPrior, Detail: fmt.Sprintf(
			"%d earlier decision%s for this subject %s on record",
			len(dos.Decisions), pluralS(len(dos.Decisions)),
			pluralVerb(len(dos.Decisions), "is", "are"))}, decisionIDs)
	}

	sort.SliceStable(drivers, func(i, j int) bool {
		if drivers[i].Kind != drivers[j].Kind {
			return driverRank(drivers[i].Kind) < driverRank(drivers[j].Kind)
		}
		return drivers[i].Name < drivers[j].Name
	})
	ex.Drivers = drivers

	var all []ID
	for _, d := range drivers {
		all = append(all, d.Evidence...)
	}
	ex.Citations, _ = dedupeIDs(all, -1)
	if ex.Ungrounded > 0 {
		ex.Notes = append(ex.Notes, fmt.Sprintf(
			"%d driver%s were computed but dropped for want of a resolvable evidence id; an ungrounded reason is worse than a missing one",
			ex.Ungrounded, pluralS(ex.Ungrounded)))
	}
	ex.noteGrounding(groundingOf(dos, ownEvents, len(events), len(ex.Citations)))
	return ex, nil
}

// groundingOf counts what the store returned for the subject and resolves the
// state from it. The counts are witnesses, not an inventory: digests appear
// in both Digests and UsageWindows, because the usage summary runs its own
// query across every stored tier and is the one section a caller's caps
// cannot suppress.
func groundingOf(dos *evidence.Dossier, ownEvents, allEvents, citations int) *Grounding {
	g := &Grounding{
		Digests:      len(dos.Digests),
		Events:       ownEvents,
		Decisions:    len(dos.Decisions),
		UsageWindows: dos.Usage.Windows,
		Samples:      dos.Usage.Samples,
		ParentEvents: allEvents - ownEvents,
		Citations:    citations,
	}
	// A cap that dropped records is proof records exist. Without this the
	// caller who asks for MaxEvents < 0 gets told the subject does not exist.
	if t := dos.Truncated; t != nil {
		g.Withheld = t.Digests + t.Events + t.Decisions
	}
	g.State = g.stateFor(citations)
	return g
}

// noteGrounding attaches the report and the sentence that goes with it. The
// report is dropped for a grounded payload: Citations already prove it, and
// every field would be redundant with the drivers above.
func (e *Explanation) noteGrounding(g *Grounding) {
	switch g.State {
	case GroundingGrounded:
		return
	case GroundingAbsent:
		// The leading clause is the same sentence this payload has always
		// carried for an empty store, because it is still exactly true.
		note := "no evidence is stored for this subject in this window; the payload states the decision but grounds none of it"
		if g.Citations > 0 {
			// It cites something anyway: the parent workload's change
			// events, which describe the workload and not this subject.
			note = fmt.Sprintf("no evidence is stored for this subject in this window; "+
				"the %d citation%s below %s the parent workload's change event%s, which describe%s the workload, not this subject",
				g.Citations, pluralS(g.Citations), pluralVerb(g.Citations, "is", "are"),
				pluralS(g.ParentEvents), pluralVerb(g.ParentEvents, "s", ""))
		}
		e.Notes = append(e.Notes, note)
	case GroundingThin:
		e.Notes = append(e.Notes, fmt.Sprintf(
			"evidence is stored for this subject in this window (%d digest%s, %d event%s, %d decision%s, %d usage window%s) "+
				"but no driver could be grounded in it; the payload states the decision and cites nothing",
			g.Digests, pluralS(g.Digests), g.Events, pluralS(g.Events),
			g.Decisions, pluralS(g.Decisions), g.UsageWindows, pluralS(g.UsageWindows)))
	}
	e.Grounding = g
}

// parentWorkload maps a container subject to the workload subject that owns
// it: ContainerKey renders as "Kind/namespace/name/container", so the parent
// is the key minus its last segment.
func parentWorkload(s evidence.SubjectRef) (evidence.SubjectRef, bool) {
	if s.Kind != evidence.SubjectContainer {
		return evidence.SubjectRef{}, false
	}
	i := strings.LastIndexByte(s.Key, '/')
	if i <= 0 {
		return evidence.SubjectRef{}, false
	}
	return evidence.SubjectRef{Cluster: s.Cluster, Kind: evidence.SubjectWorkload, Key: s.Key[:i]}, true
}

// mergeEvents folds the parent workload's change events into the subject's
// own, newest first, keeping at most extra of the parent's. Both inputs are
// already bounded, so the result is bounded by construction; the total order
// is (newest first, then citation id) so the merge cannot depend on which
// query returned first.
func mergeEvents(own, parent []evidence.EvidenceEvent, extra int) []evidence.EvidenceEvent {
	if len(parent) == 0 {
		return own
	}
	sort.SliceStable(parent, func(i, j int) bool {
		if !parent[i].At.Equal(parent[j].At) {
			return parent[i].At.After(parent[j].At)
		}
		return EventID(parent[i]) < EventID(parent[j])
	})
	if len(parent) > extra {
		parent = parent[:extra]
	}
	out := make([]evidence.EvidenceEvent, 0, len(own)+len(parent))
	out = append(out, own...)
	out = append(out, parent...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return EventID(out[i]) < EventID(out[j])
	})
	return out
}

// Verify re-resolves every citation against a store. It is the publish gate:
// an explanation that does not verify must not be rendered, exported, or
// handed to a narrating model.
func (e *Explanation) Verify(r Resolver) error {
	for _, id := range e.Citations {
		if _, err := r.Resolve(id); err != nil {
			return fmt.Errorf("explain: citation %s does not resolve: %w", id, err)
		}
	}
	return nil
}

// Verify re-resolves every citation in the attribution, including sub-terms.
func (a *Attribution) Verify(r Resolver) error {
	for _, t := range append(append([]Term(nil), a.Terms...), a.Residual) {
		for _, sub := range append([]Term{t}, t.Of...) {
			for _, id := range sub.Evidence {
				if _, err := r.Resolve(id); err != nil {
					return fmt.Errorf("explain: citation %s on term %q does not resolve: %w", id, sub.Kind, err)
				}
			}
		}
	}
	return nil
}

// Citations returns every id the attribution leans on, sorted and
// de-duplicated — the set a narrating session is allowed to quote.
func (a *Attribution) Citations() []ID {
	var all []ID
	for _, t := range append(append([]Term(nil), a.Terms...), a.Residual) {
		all = append(all, t.Evidence...)
		for _, s := range t.Of {
			all = append(all, s.Evidence...)
		}
	}
	ids, _ := dedupeIDs(all, -1)
	return ids
}

func sizingOf(r *recommend.Recommendation, savings float64, known bool) *Sizing {
	s := &Sizing{
		Container:      r.Key.Container,
		CurrentRequest: r.CurrentRequest,
		TargetRequest:  r.TargetRequest,
		CurrentLimit:   r.CurrentLimit,
		TargetLimit:    r.TargetLimit,
		DeltaRequest:   r.Delta(),
		Class:          string(r.Class),
		Reason:         r.Reason,
		Samples:        r.Samples,
		WindowHours:    r.WindowHours,
		OOMCount:       r.OOMCount,
		CPUSkipped:     r.CPUSkipped,
	}
	if known {
		v := savings
		s.SavingsMonthlyUSD = &v
	}
	return s
}

// driverRank fixes the display order: what stopped the decision, then what
// scored it, then what the numbers were, then context.
func driverRank(kind string) int {
	switch kind {
	case DriverRefusal:
		return 0
	case DriverOOMFloor:
		return 1
	case DriverThrottled:
		return 2
	case DriverHPAGuard:
		return 3
	case DriverConfidence:
		return 4
	case DriverClass:
		return 5
	case DriverHistory:
		return 6
	case DriverRecentEvent:
		return 7
	case DriverPrior:
		return 8
	}
	return 9
}

// refusalCitations picks the evidence class that actually justifies each
// refusal code. A refusal grounded in "some digest exists" would technically
// resolve while explaining nothing, so codes about change cite change events,
// codes about signals cite the signals, and so on.
func refusalCitations(code decision.RefusalCode, digests, changes, ooms, throttles, decisions []ID) []ID {
	switch code {
	case decision.CodeInsufficientHistory:
		return digests
	case decision.CodePostChangeSoak, decision.CodeRegimeChangePending, decision.CodeClassUnstable:
		return concatIDs(changes, digests)
	case decision.CodeSignalConflict:
		return concatIDs(ooms, throttles, digests)
	case decision.CodeQuarantined, decision.CodeSLADegraded:
		return concatIDs(decisions, changes, digests)
	}
	return concatIDs(digests, changes, decisions)
}

// confidenceCitations maps a confidence term to the evidence behind it by
// name, using the term names pkg/decision actually produces.
func confidenceCitations(name string, digests, changes, decisions []ID) []ID {
	switch {
	case strings.Contains(name, "soak"), strings.Contains(name, "change"), strings.Contains(name, "class"):
		return concatIDs(changes, digests)
	case strings.Contains(name, "agreement"):
		return concatIDs(decisions, digests)
	default:
		return digests
	}
}

func concatIDs(pools ...[]ID) []ID {
	var out []ID
	for _, p := range pools {
		out = append(out, p...)
	}
	return out
}

func digestCitations(s evidence.SubjectRef, ds []evidence.Digest) []ID {
	out := make([]ID, 0, len(ds))
	for _, d := range ds {
		out = append(out, DigestID(s, d))
	}
	return out
}

func digestsWhere(s evidence.SubjectRef, ds []evidence.Digest, pred func(evidence.Digest) bool) []ID {
	var out []ID
	for _, d := range ds {
		if pred(d) {
			out = append(out, DigestID(s, d))
		}
	}
	return out
}

func decisionCitations(ds []evidence.DecisionRecord) []ID {
	out := make([]ID, 0, len(ds))
	for _, d := range ds {
		out = append(out, DecisionID(d))
	}
	return out
}

func pluralS(n int) string {
	if n == 1 || n == -1 {
		return ""
	}
	return "s"
}

// pluralVerb agrees a verb with its count. Template prose is the only thing
// a no-model deployment ever renders (§5.9), so it may as well read like a
// sentence.
func pluralVerb(n int, singular, plural string) string {
	if n == 1 || n == -1 {
		return singular
	}
	return plural
}

func pluralS64(n int64) string {
	if n == 1 || n == -1 {
		return ""
	}
	return "s"
}

// humanBytes renders a byte count with a fixed unit ladder so two payloads
// over the same number are byte-identical.
func humanBytes(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return "n/a"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + units[i]
}

// Prose renders the explanation as deterministic text with inline citations
// — the no-model path of §5.9.
func (e *Explanation) Prose() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Subject %s over %s → %s.\n", e.Subject.String(),
		e.From.Format("2006-01-02 15:04Z"), e.To.Format("2006-01-02 15:04Z"))
	switch {
	case e.Refusal != nil:
		fmt.Fprintf(&b, "Verdict: refuse (%s) — %s", e.Refusal.Code, e.Refusal.Detail)
		if !e.Refusal.Until.IsZero() {
			fmt.Fprintf(&b, " Clears no earlier than %s.", e.Refusal.Until.UTC().Format("2006-01-02 15:04Z"))
		}
		b.WriteString("\n")
	case e.Action == ActionUnknown:
		// Two absences, two sentences. "None recorded" for a payload nobody
		// told anything; "not computed" for one where the engine considered
		// the subject and reached no verdict. Neither may read as a refusal.
		if o := e.VerdictOrigin; o != nil && o.State == VerdictNotComputed {
			fmt.Fprintf(&b, "Verdict: not computed — the recommendation path reported disposition %q "+
				"(%d sample%s over %s). That is an absent verdict, not a negative one.\n",
				o.Disposition, o.Samples, pluralS(o.Samples), o.Window)
			break
		}
		b.WriteString("Verdict: none recorded.\n")
	default:
		fmt.Fprintf(&b, "Verdict: %s", e.Action)
		if e.Confidence != nil {
			fmt.Fprintf(&b, " at confidence %s", strconv.FormatFloat(e.Confidence.Score, 'f', 2, 64))
		}
		b.WriteString(".\n")
	}
	if s := e.Sizing; s != nil {
		fmt.Fprintf(&b, "Sizing: %s request %s → %s, limit %s → %s.\n", s.Container,
			s.CurrentRequest.String(), s.TargetRequest.String(), s.CurrentLimit.String(), s.TargetLimit.String())
		if s.SavingsMonthlyUSD != nil {
			fmt.Fprintf(&b, "Priced by the caller at %s/mo.\n",
				"$"+strconv.FormatFloat(*s.SavingsMonthlyUSD, 'f', 2, 64))
		}
	}
	b.WriteString("Because:\n")
	for _, d := range e.Drivers {
		fmt.Fprintf(&b, "  %-14s %s [%s]\n", d.Kind, d.Detail, citeList(d.Evidence))
	}
	for _, n := range e.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	return b.String()
}
