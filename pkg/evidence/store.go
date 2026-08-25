package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Store is the deterministic query surface over the substrate (§3.4).
// Implementations: *Memory here; the bbolt-backed store in pkg/store
// satisfies it in a later unit. All windows are half-open [from, to); a
// zero from means "since forever", a zero to means "until forever".
type Store interface {
	Append(ev EvidenceEvent) error
	Events(s SubjectRef, from, to time.Time, kinds ...string) ([]EvidenceEvent, error)
	Digests(s SubjectRef, from, to time.Time, tier int) ([]Digest, error)
	Timeline(cluster string, from, to time.Time) ([]TimelinePoint, error) // cost + node count + events overlay
	Decisions(s SubjectRef, from, to time.Time) ([]DecisionRecord, error)
}

// Sink is the ingestion seam collectors write into. pkg/collect gets a
// Sink in a later unit (see FINDINGS.md for the exact wiring); pkg/api
// records decisions and timeline points after each ingest.
type Sink interface {
	Append(ev EvidenceEvent) error
	ObserveSample(s SubjectRef, smp Sample) error
	ObservePoint(cluster string, p TimelinePoint) error
	RecordDecision(d DecisionRecord) error
}

// ErrOutOfOrder rejects samples and timeline points older than the newest
// stored one for their subject/cluster: series are time-ordered by
// construction so digest windows stay well-defined.
var ErrOutOfOrder = errors.New("evidence: observation older than newest stored")

// Stats counts what the substrate holds and everything it ever dropped —
// budgets are measured, not hoped (§3.3). Gauges are recomputed on call;
// counters accumulate since construction (they reset on checkpoint
// restore).
type Stats struct {
	// Gauges.
	EventSubjects    int // subjects holding at least one event
	Events           int
	EventBytes       int64
	SeriesSubjects   int
	SeriesBytes      int64
	RawSamples       int
	HourlyDigests    int
	DailyDigests     int
	Decisions        int
	DecisionBytes    int64
	TimelineClusters int
	TimelinePoints   int
	TimelineBytes    int64

	// Counters.
	CoalescedEvents        uint64
	EvictedEventsCap       uint64
	EvictedEventsBudget    uint64
	CoalescedHourly        uint64
	CoalescedDaily         uint64
	DroppedSamples         uint64 // out-of-order or per-hour cap overflow
	EvictedSeriesItems     uint64
	EvictedDecisionsCap    uint64
	EvictedDecisionsBudget uint64
	EvictedTimeline        uint64
	PrunedEvents           uint64
	PrunedDecisions        uint64
	PrunedSeriesItems      uint64
	PrunedTimeline         uint64
}

// timeline is one cluster's tier-3 point ring.
type timeline struct {
	points ring[TimelinePoint]
	lastAt time.Time
}

// Memory is the in-memory evidence substrate. Safe for concurrent use.
// Persistence goes through Checkpoint/FromCheckpoint (codec.go); the
// bbolt wiring belongs to pkg/store in a later unit.
type Memory struct {
	mu  sync.RWMutex
	cfg Config
	// seq is the global arrival counter: the deterministic eviction order
	// and the query tie-break for equal timestamps.
	seq uint64

	events    *budgetedLog[EvidenceEvent]
	decisions *budgetedLog[DecisionRecord]

	series      map[SubjectRef]*series
	seriesHeap  minHeap[*series]
	seriesBytes int64

	timelines map[string]*timeline

	stats Stats
}

var (
	_ Store = (*Memory)(nil)
	_ Sink  = (*Memory)(nil)
)

// NewMemory builds an empty substrate. Zero-valued Config fields take
// their defaults (DefaultConfig).
func NewMemory(cfg Config) (*Memory, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	m := &Memory{
		cfg:       cfg,
		events:    newBudgetedLog[EvidenceEvent](),
		decisions: newBudgetedLog[DecisionRecord](),
		series:    map[SubjectRef]*series{},
		timelines: map[string]*timeline{},
	}
	m.seriesHeap = minHeap[*series]{
		less:   func(a, b *series) bool { return a.lastSeq < b.lastSeq },
		setIdx: func(s *series, i int) { s.hIdx = i },
	}
	return m, nil
}

// Config returns the effective (defaulted) configuration.
func (m *Memory) Config() Config { return m.cfg }

// sanitizeEvent validates and normalizes an event for storage.
func sanitizeEvent(ev EvidenceEvent) (EvidenceEvent, error) {
	if ev.At.IsZero() {
		return ev, fmt.Errorf("evidence: event needs a timestamp")
	}
	ev.At = utcTime(ev.At)
	var err error
	if ev.Subject, err = sanitizeSubject(ev.Subject); err != nil {
		return ev, err
	}
	ev.Kind = cleanString(ev.Kind, maxKindLen)
	if ev.Kind == "" {
		return ev, fmt.Errorf("evidence: event needs a kind")
	}
	switch ev.Severity {
	case "":
		ev.Severity = SeverityInfo
	case SeverityInfo, SeverityWarning, SeverityCritical:
	default:
		return ev, fmt.Errorf("evidence: unknown severity %q", ev.Severity)
	}
	ev.Dedup = cleanString(ev.Dedup, maxDedupLen)
	ev.Attrs = sanitizeAttrs(ev.Attrs)
	ev.Count = 0 // maintained by the substrate, never by callers
	return ev, nil
}

// sanitizeAttrs cleans keys and values, drops empties, and caps the count
// deterministically (the lexicographically-first maxAttrs keys survive).
// The result is always a fresh map — stored state never aliases caller
// memory.
func sanitizeAttrs(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	cleaned := make(map[string]string, len(in))
	for k, v := range in {
		ck := cleanString(k, maxAttrKeyLen)
		if ck == "" {
			continue
		}
		cleaned[ck] = cleanString(v, maxAttrValLen)
	}
	if len(cleaned) == 0 {
		return nil
	}
	if len(cleaned) > maxAttrs {
		keys := make([]string, 0, len(cleaned))
		for k := range cleaned {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[maxAttrs:] {
			delete(cleaned, k)
		}
	}
	return cleaned
}

// validateEvent is the restore-side dual of sanitizeEvent: checks without
// transforming, so valid checkpoints round-trip byte-exactly.
func validateEvent(ev EvidenceEvent) error {
	if !timeStoredOK(ev.At) {
		return fmt.Errorf("evidence: event time not storage-normal")
	}
	if err := validateSubject(ev.Subject); err != nil {
		return err
	}
	if ev.Kind == "" || !cleanStringOK(ev.Kind, maxKindLen) || !cleanStringOK(ev.Dedup, maxDedupLen) {
		return fmt.Errorf("evidence: event kind/dedup invalid")
	}
	switch ev.Severity {
	case SeverityInfo, SeverityWarning, SeverityCritical:
	default:
		return fmt.Errorf("evidence: unknown severity %q", ev.Severity)
	}
	if len(ev.Attrs) > maxAttrs {
		return fmt.Errorf("evidence: event has %d attrs, cap %d", len(ev.Attrs), maxAttrs)
	}
	for k, v := range ev.Attrs {
		if k == "" || !cleanStringOK(k, maxAttrKeyLen) || !cleanStringOK(v, maxAttrValLen) {
			return fmt.Errorf("evidence: event attr %q invalid", k)
		}
	}
	if ev.Count < 0 || ev.Count > maxEventCount {
		return fmt.Errorf("evidence: event count %d invalid", ev.Count)
	}
	return nil
}

// Append records one event. Events whose (Kind, Dedup) matches a stored
// event of the same subject within Config.DedupWindow are folded into it:
// the stored timestamp advances to the newer one, Count increments, and
// severity upgrades to the higher of the two. First-occurrence attrs win.
func (m *Memory) Append(ev EvidenceEvent) error {
	ev, err := sanitizeEvent(ev)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if ev.Dedup != "" {
		if s := m.events.subs[ev.Subject]; s != nil {
			for i := s.ring.len() - 1; i >= 0; i-- {
				e := s.ring.at(i)
				if e.v.Kind != ev.Kind || e.v.Dedup != ev.Dedup {
					continue
				}
				if absDuration(ev.At.Sub(e.v.At)) > m.cfg.DedupWindow {
					continue
				}
				if ev.At.After(e.v.At) {
					e.v.At = ev.At
				}
				if severityRank(ev.Severity) > severityRank(e.v.Severity) {
					// Severity strings differ in length, so an upgrade changes
					// the entry's accounted cost: re-charge it, or the global
					// budget drifts away from the bytes actually held.
					e.v.Severity = ev.Severity
					nb := eventBytes(&e.v)
					delta := nb - e.b
					e.b = nb
					s.bytes += delta
					m.events.bytes += delta
				}
				if e.v.Count < maxEventCount {
					e.v.Count++
				}
				m.stats.CoalescedEvents++
				m.stats.EvictedEventsBudget += uint64(m.events.enforceBudget(m.cfg.MaxEventBytes))
				return nil
			}
		}
	}

	m.seq++
	m.stats.EvictedEventsCap += uint64(m.events.append(ev.Subject, ev, eventBytes(&ev), m.seq, m.cfg.MaxEventsPerSubject))
	m.stats.EvictedEventsBudget += uint64(m.events.enforceBudget(m.cfg.MaxEventBytes))
	return nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// sanitizeDecision validates and normalizes a decision record.
func sanitizeDecision(d DecisionRecord, maxPayload int) (DecisionRecord, error) {
	if d.At.IsZero() {
		return d, fmt.Errorf("evidence: decision needs a timestamp")
	}
	d.At = utcTime(d.At)
	var err error
	if d.Subject, err = sanitizeSubject(d.Subject); err != nil {
		return d, err
	}
	d.Kind = cleanString(d.Kind, maxKindLen)
	if d.Kind == "" {
		return d, fmt.Errorf("evidence: decision needs a kind")
	}
	d.Summary = cleanString(d.Summary, maxSummaryLen)
	d.Fingerprint = cleanString(d.Fingerprint, maxFingerprintLen)
	if len(d.Payload) > 0 {
		if len(d.Payload) > maxPayload {
			return d, fmt.Errorf("evidence: decision payload %dB exceeds cap %dB", len(d.Payload), maxPayload)
		}
		// Compact at ingest rather than storing the producer's whitespace:
		// encoding/json compacts json.RawMessage on the way out, so an
		// uncompacted stored payload would not survive a checkpoint
		// round-trip byte-exactly. Compacting also validates.
		var buf bytes.Buffer
		if err := json.Compact(&buf, d.Payload); err != nil {
			return d, fmt.Errorf("evidence: decision payload is not valid JSON: %w", err)
		}
		d.Payload = json.RawMessage(buf.Bytes()) // fresh bytes, never aliases the caller
	} else {
		d.Payload = nil
	}
	return d, nil
}

// validateDecision is the restore-side dual of sanitizeDecision.
func validateDecision(d DecisionRecord, maxPayload int) error {
	if !timeStoredOK(d.At) {
		return fmt.Errorf("evidence: decision time not storage-normal")
	}
	if err := validateSubject(d.Subject); err != nil {
		return err
	}
	if d.Kind == "" || !cleanStringOK(d.Kind, maxKindLen) ||
		!cleanStringOK(d.Summary, maxSummaryLen) || !cleanStringOK(d.Fingerprint, maxFingerprintLen) {
		return fmt.Errorf("evidence: decision strings invalid")
	}
	if len(d.Payload) > maxPayload {
		return fmt.Errorf("evidence: decision payload invalid")
	}
	if len(d.Payload) > 0 {
		// Stored payloads are compact (sanitizeDecision compacts at ingest);
		// a non-compact payload in a checkpoint would not re-encode
		// byte-identically, so reject it rather than silently rewrite it.
		var buf bytes.Buffer
		if err := json.Compact(&buf, d.Payload); err != nil {
			return fmt.Errorf("evidence: decision payload invalid")
		}
		if !bytes.Equal(buf.Bytes(), d.Payload) {
			return fmt.Errorf("evidence: decision payload is not compact")
		}
	}
	return nil
}

// RecordDecision journals one surfaced recommendation, refusal, or action.
func (m *Memory) RecordDecision(d DecisionRecord) error {
	d, err := sanitizeDecision(d, m.cfg.MaxDecisionPayloadBytes)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.stats.EvictedDecisionsCap += uint64(m.decisions.append(d.Subject, d, decisionBytes(&d), m.seq, m.cfg.MaxDecisionsPerSubject))
	m.stats.EvictedDecisionsBudget += uint64(m.decisions.enforceBudget(m.cfg.MaxDecisionBytes))
	return nil
}

// ObserveSample ingests one usage sample for a subject. Samples must
// arrive in non-decreasing time order per subject (equal timestamps are
// accepted); older samples return ErrOutOfOrder and are counted.
func (m *Memory) ObserveSample(s SubjectRef, smp Sample) error {
	s, err := sanitizeSubject(s)
	if err != nil {
		return err
	}
	smp, err = sanitizeSample(smp)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	sr := m.series[s]
	if sr == nil {
		sr = &series{ref: s, hIdx: -1}
		m.series[s] = sr
		m.seriesBytes += subjectOverheadBytes
		m.seriesHeap.push(sr)
	}
	if smp.At.Before(sr.lastAt) {
		m.stats.DroppedSamples++
		return fmt.Errorf("%w: sample %v < %v for %s", ErrOutOfOrder, smp.At, sr.lastAt, s)
	}
	m.seq++
	m.seriesBytes += sr.ingest(smp, m.seq, &m.cfg, &m.stats)
	m.seriesHeap.fix(sr.hIdx) // lastSeq advanced: series got warmer
	m.enforceSeriesBudget()
	return nil
}

// enforceSeriesBudget sheds items from the coldest series (smallest
// lastSeq) — raw samples first, then hourly, then daily digests — until
// the budget fits. Coldest-first keeps full fidelity on active subjects
// and degrades idle ones gracefully.
func (m *Memory) enforceSeriesBudget() {
	for m.seriesBytes > m.cfg.MaxSeriesBytes && m.seriesHeap.len() > 0 {
		sr := m.seriesHeap.peek()
		delta, empty := sr.evictOne(&m.stats)
		m.seriesBytes += delta
		if empty {
			m.seriesHeap.removeAt(sr.hIdx)
			delete(m.series, sr.ref)
			m.seriesBytes -= subjectOverheadBytes
		}
	}
}

// ObservePoint ingests one cluster timeline point (tier 3). Points must
// arrive in non-decreasing time order per cluster; Events must be nil (the
// overlay is computed by Timeline queries, never stored).
func (m *Memory) ObservePoint(cluster string, p TimelinePoint) error {
	cluster = cleanString(cluster, maxClusterLen)
	if cluster == "" {
		return fmt.Errorf("evidence: timeline point needs a cluster")
	}
	if p.Events != nil {
		return fmt.Errorf("evidence: timeline point events are query-computed, must be nil")
	}
	if err := validatePointValues(p); err != nil {
		return err
	}
	p.At = utcTime(p.At)
	m.mu.Lock()
	defer m.mu.Unlock()
	tl := m.timelines[cluster]
	if tl == nil {
		if len(m.timelines) >= m.cfg.MaxTimelineClusters {
			return fmt.Errorf("evidence: timeline cluster budget (%d) exhausted", m.cfg.MaxTimelineClusters)
		}
		tl = &timeline{}
		m.timelines[cluster] = tl
	}
	if p.At.Before(tl.lastAt) {
		return fmt.Errorf("%w: point %v < %v for cluster %s", ErrOutOfOrder, p.At, tl.lastAt, cluster)
	}
	if _, evicted := tl.points.push(p, m.cfg.TimelineCap); evicted {
		m.stats.EvictedTimeline++
	}
	tl.lastAt = p.At
	return nil
}

// validatePointValues checks the numeric fields of a timeline point.
func validatePointValues(p TimelinePoint) error {
	if p.At.IsZero() {
		return fmt.Errorf("evidence: timeline point needs a timestamp")
	}
	if !(p.CostUSDPerHour >= 0) || p.CostUSDPerHour > 1e12 { // NaN-safe positive form
		return fmt.Errorf("evidence: timeline cost %v outside [0, 1e12]", p.CostUSDPerHour)
	}
	if p.Nodes < 0 || p.Nodes > 1<<20 {
		return fmt.Errorf("evidence: timeline node count %d invalid", p.Nodes)
	}
	return nil
}

// inWindow reports t ∈ [from, to), where zero bounds mean unbounded.
func inWindow(t, from, to time.Time) bool {
	if !from.IsZero() && t.Before(from) {
		return false
	}
	if !to.IsZero() && !t.Before(to) {
		return false
	}
	return true
}

func checkWindow(from, to time.Time) error {
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return fmt.Errorf("evidence: window [%v, %v) is inverted", from, to)
	}
	return nil
}

// copyEvent returns a defensive copy (fresh attrs map).
func copyEvent(ev EvidenceEvent) EvidenceEvent {
	if len(ev.Attrs) > 0 {
		attrs := make(map[string]string, len(ev.Attrs))
		for k, v := range ev.Attrs {
			attrs[k] = v
		}
		ev.Attrs = attrs
	}
	return ev
}

// copyDecision returns a defensive copy (fresh payload bytes).
func copyDecision(d DecisionRecord) DecisionRecord {
	if len(d.Payload) > 0 {
		d.Payload = append(json.RawMessage(nil), d.Payload...)
	}
	return d
}

// Events returns the subject's events in [from, to), oldest first, ordered
// by (At, arrival). With kinds given, only those kinds are returned.
func (m *Memory) Events(s SubjectRef, from, to time.Time, kinds ...string) ([]EvidenceEvent, error) {
	s, err := sanitizeSubject(s)
	if err != nil {
		return nil, err
	}
	if err := checkWindow(from, to); err != nil {
		return nil, err
	}
	from, to = utcTime(from), utcTime(to)
	var kindSet map[string]bool
	if len(kinds) > 0 {
		kindSet = make(map[string]bool, len(kinds))
		for _, k := range kinds {
			kindSet[k] = true
		}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sub := m.events.subs[s]
	if sub == nil {
		return nil, nil
	}
	type seqEv struct {
		ev  EvidenceEvent
		seq uint64
	}
	var out []seqEv
	for i := 0; i < sub.ring.len(); i++ {
		e := sub.ring.at(i)
		if !inWindow(e.v.At, from, to) {
			continue
		}
		if kindSet != nil && !kindSet[e.v.Kind] {
			continue
		}
		out = append(out, seqEv{copyEvent(e.v), e.seq})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ev.At.Equal(out[j].ev.At) {
			return out[i].ev.At.Before(out[j].ev.At)
		}
		return out[i].seq < out[j].seq
	})
	evs := make([]EvidenceEvent, len(out))
	for i, se := range out {
		evs[i] = se.ev
	}
	return evs, nil
}

// Decisions returns the subject's decision records in [from, to), oldest
// first, ordered by (At, arrival).
func (m *Memory) Decisions(s SubjectRef, from, to time.Time) ([]DecisionRecord, error) {
	s, err := sanitizeSubject(s)
	if err != nil {
		return nil, err
	}
	if err := checkWindow(from, to); err != nil {
		return nil, err
	}
	from, to = utcTime(from), utcTime(to)
	m.mu.RLock()
	defer m.mu.RUnlock()
	sub := m.decisions.subs[s]
	if sub == nil {
		return nil, nil
	}
	type seqDec struct {
		d   DecisionRecord
		seq uint64
	}
	var out []seqDec
	for i := 0; i < sub.ring.len(); i++ {
		e := sub.ring.at(i)
		if !inWindow(e.v.At, from, to) {
			continue
		}
		out = append(out, seqDec{copyDecision(e.v), e.seq})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].d.At.Equal(out[j].d.At) {
			return out[i].d.At.Before(out[j].d.At)
		}
		return out[i].seq < out[j].seq
	})
	ds := make([]DecisionRecord, len(out))
	for i, sd := range out {
		ds[i] = sd.d
	}
	return ds, nil
}

// Digests returns the subject's stored digests of one tier overlapping
// [from, to), oldest first. Tier 0 maps each raw sample to a point-in-time
// digest (Start == End == At, Samples == 1). Pending, not-yet-finalized
// hour/day accumulators are not visible — the raw tier covers that window.
func (m *Memory) Digests(s SubjectRef, from, to time.Time, tier int) ([]Digest, error) {
	s, err := sanitizeSubject(s)
	if err != nil {
		return nil, err
	}
	if err := checkWindow(from, to); err != nil {
		return nil, err
	}
	from, to = utcTime(from), utcTime(to)
	m.mu.RLock()
	defer m.mu.RUnlock()
	sr := m.series[s]
	if sr == nil {
		return nil, nil
	}
	var out []Digest
	switch tier {
	case TierRaw:
		for i := 0; i < sr.raw.len(); i++ {
			smp := sr.raw.at(i)
			if !inWindow(smp.At, from, to) {
				continue
			}
			v := DigestStats{P50: float64(smp.MilliCPU), P95: float64(smp.MilliCPU), P99: float64(smp.MilliCPU), Max: float64(smp.MilliCPU)}
			mem := DigestStats{P50: float64(smp.MemoryBytes), P95: float64(smp.MemoryBytes), P99: float64(smp.MemoryBytes), Max: float64(smp.MemoryBytes)}
			out = append(out, Digest{
				Start: smp.At, End: smp.At, Tier: TierRaw, Samples: 1,
				CPU: v, Mem: mem, ThrottleRatio: smp.ThrottleRatio,
				Restarts: smp.Restarts, OOMs: smp.OOMs,
			})
		}
	case TierHourly, TierDaily:
		r := &sr.hourly
		if tier == TierDaily {
			r = &sr.daily
		}
		for i := 0; i < r.len(); i++ {
			d := r.at(i)
			if !from.IsZero() && !d.End.After(from) {
				continue
			}
			if !to.IsZero() && !d.Start.Before(to) {
				continue
			}
			out = append(out, *d)
		}
	default:
		return nil, fmt.Errorf("evidence: unknown digest tier %d", tier)
	}
	return out, nil
}

// Timeline returns the cluster's timeline points in [from, to), oldest
// first, with the cluster's evidence events overlaid: each event in the
// window attaches to the latest point at or before it (events before the
// first point attach to the first). At most Config.MaxOverlayEventsPerPoint
// events attach per point, keeping the highest-severity, most recent ones.
// Without stored points the result is empty — there is nothing to overlay
// onto.
func (m *Memory) Timeline(cluster string, from, to time.Time) ([]TimelinePoint, error) {
	cluster = cleanString(cluster, maxClusterLen)
	if cluster == "" {
		return nil, fmt.Errorf("evidence: timeline needs a cluster")
	}
	if err := checkWindow(from, to); err != nil {
		return nil, err
	}
	from, to = utcTime(from), utcTime(to)
	m.mu.RLock()
	defer m.mu.RUnlock()
	tl := m.timelines[cluster]
	if tl == nil {
		return nil, nil
	}
	var points []TimelinePoint
	for i := 0; i < tl.points.len(); i++ {
		p := tl.points.at(i)
		if inWindow(p.At, from, to) {
			cp := *p
			cp.Events = nil
			points = append(points, cp)
		}
	}
	if len(points) == 0 {
		return nil, nil
	}

	// Overlay: collect the cluster's events in the window. Map iteration
	// order is neutralized by the (At, seq) sort below.
	type seqEv struct {
		ev  EvidenceEvent
		seq uint64
	}
	var overlay []seqEv
	for _, sub := range m.events.subs {
		if sub.ref.Cluster != cluster {
			continue
		}
		for i := 0; i < sub.ring.len(); i++ {
			e := sub.ring.at(i)
			if inWindow(e.v.At, from, to) {
				overlay = append(overlay, seqEv{copyEvent(e.v), e.seq})
			}
		}
	}
	sort.Slice(overlay, func(i, j int) bool {
		if !overlay[i].ev.At.Equal(overlay[j].ev.At) {
			return overlay[i].ev.At.Before(overlay[j].ev.At)
		}
		return overlay[i].seq < overlay[j].seq
	})
	// Attach each event to the latest point at or before it.
	buckets := make([][]seqEv, len(points))
	for _, se := range overlay {
		idx := sort.Search(len(points), func(i int) bool { return points[i].At.After(se.ev.At) }) - 1
		if idx < 0 {
			idx = 0
		}
		buckets[idx] = append(buckets[idx], se)
	}
	for i, b := range buckets {
		if len(b) > m.cfg.MaxOverlayEventsPerPoint {
			// Keep the highest severity, then the most recent arrivals.
			sort.SliceStable(b, func(x, y int) bool {
				rx, ry := severityRank(b[x].ev.Severity), severityRank(b[y].ev.Severity)
				if rx != ry {
					return rx > ry
				}
				return b[x].seq > b[y].seq
			})
			b = b[:m.cfg.MaxOverlayEventsPerPoint]
			sort.Slice(b, func(x, y int) bool {
				if !b[x].ev.At.Equal(b[y].ev.At) {
					return b[x].ev.At.Before(b[y].ev.At)
				}
				return b[x].seq < b[y].seq
			})
		}
		if len(b) > 0 {
			evs := make([]EvidenceEvent, len(b))
			for j, se := range b {
				evs[j] = se.ev
			}
			points[i].Events = evs
		}
	}
	return points, nil
}

// Subjects returns every subject known to any store, sorted by
// (Cluster, Kind, Key).
func (m *Memory) Subjects() []SubjectRef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[SubjectRef]bool{}
	for ref := range m.events.subs {
		seen[ref] = true
	}
	for ref := range m.decisions.subs {
		seen[ref] = true
	}
	for ref := range m.series {
		seen[ref] = true
	}
	refs := make([]SubjectRef, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].less(refs[j]) })
	return refs
}

// Prune applies age-based retention relative to the caller's clock now:
// events older than EventMaxAge, decisions older than DecisionMaxAge, raw
// samples older than RawMaxAge, hourly/daily digests whose window ended
// before their retention, and timeline points older than TimelineMaxAge.
// Byte budgets and ring caps are enforced on write and never depend on a
// clock — Prune is the only clock-dependent bound, and the clock is the
// caller's. Returns the number of items removed.
func (m *Memory) Prune(now time.Time) (int, error) {
	if now.IsZero() {
		return 0, fmt.Errorf("evidence: prune needs a clock")
	}
	now = utcTime(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0

	evCut := now.Add(-m.cfg.EventMaxAge)
	n := m.events.filterAll(func(_ SubjectRef, e *entry[EvidenceEvent]) bool {
		return !e.v.At.Before(evCut)
	})
	m.stats.PrunedEvents += uint64(n)
	removed += n

	decCut := now.Add(-m.cfg.DecisionMaxAge)
	n = m.decisions.filterAll(func(_ SubjectRef, e *entry[DecisionRecord]) bool {
		return !e.v.At.Before(decCut)
	})
	m.stats.PrunedDecisions += uint64(n)
	removed += n

	removed += m.pruneSeries(now)
	removed += m.pruneTimelines(now)
	return removed, nil
}

func (m *Memory) pruneSeries(now time.Time) int {
	rawCut := now.Add(-m.cfg.RawMaxAge)
	hourCut := now.Add(-m.cfg.HourlyMaxAge)
	dayCut := now.Add(-m.cfg.DailyMaxAge)
	removed := 0
	for _, sr := range m.series {
		var delta int64
		n := 0
		for sr.raw.len() > 0 && sr.raw.at(0).At.Before(rawCut) {
			sr.raw.dropFront()
			delta -= sampleBytes
			n++
		}
		for sr.hourly.len() > 0 && !sr.hourly.at(0).End.After(hourCut) {
			sr.hourly.dropFront()
			delta -= digestBytes
			n++
		}
		for sr.daily.len() > 0 && !sr.daily.at(0).End.After(dayCut) {
			sr.daily.dropFront()
			delta -= digestBytes
			n++
		}
		if sr.acc != nil && !sr.acc.hour.Add(time.Hour).After(hourCut) {
			delta -= sr.acc.bytes()
			sr.acc = nil
			n++
		}
		if sr.day != nil && !sr.day.End.After(dayCut) {
			delta -= digestBytes
			sr.day = nil
			n++
		}
		sr.bytes += delta
		m.seriesBytes += delta
		removed += n
		m.stats.PrunedSeriesItems += uint64(n)
		if sr.raw.len() == 0 && sr.hourly.len() == 0 && sr.daily.len() == 0 && sr.acc == nil && sr.day == nil {
			m.seriesHeap.removeAt(sr.hIdx)
			delete(m.series, sr.ref)
			m.seriesBytes -= subjectOverheadBytes
		}
	}
	return removed
}

func (m *Memory) pruneTimelines(now time.Time) int {
	cut := now.Add(-m.cfg.TimelineMaxAge)
	removed := 0
	for cluster, tl := range m.timelines {
		for tl.points.len() > 0 && tl.points.at(0).At.Before(cut) {
			tl.points.dropFront()
			removed++
			m.stats.PrunedTimeline++
		}
		if tl.points.len() == 0 {
			delete(m.timelines, cluster)
		}
	}
	return removed
}

// Stats returns current gauges and lifetime counters.
func (m *Memory) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := m.stats
	st.EventSubjects = len(m.events.subs)
	st.Events = m.events.count
	st.EventBytes = m.events.bytes
	st.SeriesSubjects = len(m.series)
	st.SeriesBytes = m.seriesBytes
	for _, sr := range m.series {
		st.RawSamples += sr.raw.len()
		st.HourlyDigests += sr.hourly.len()
		st.DailyDigests += sr.daily.len()
	}
	st.Decisions = m.decisions.count
	st.DecisionBytes = m.decisions.bytes
	st.TimelineClusters = len(m.timelines)
	for _, tl := range m.timelines {
		st.TimelinePoints += tl.points.len()
	}
	st.TimelineBytes = int64(st.TimelinePoints)*pointBytes +
		int64(st.TimelineClusters)*subjectOverheadBytes
	return st
}
