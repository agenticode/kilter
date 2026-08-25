package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// CheckpointVersion is the on-disk format version. FromCheckpoint refuses
// anything it does not recognize rather than guessing at a layout.
const CheckpointVersion = 1

// ErrNoCheckpoint is returned by Load when the backing store holds nothing
// yet — a cold start, not a failure.
var ErrNoCheckpoint = errors.New("evidence: no checkpoint stored")

// CheckpointStore is the persistence seam. pkg/store satisfies it in a
// later unit (see FINDINGS.md); the substrate itself never opens a file, a
// bbolt handle or a socket — it only produces and consumes bytes.
type CheckpointStore interface {
	SaveCheckpoint(ctx context.Context, data []byte) error
	LoadCheckpoint(ctx context.Context) ([]byte, error)
}

// Checkpoint is the serializable whole-substrate state. Every slice is in a
// documented total order, so encoding the same substrate twice yields
// byte-identical output: map iteration never reaches the wire.
type Checkpoint struct {
	Version   int             `json:"version"`
	Config    Config          `json:"config"`
	Seq       uint64          `json:"seq"`
	Subjects  []SubjectState  `json:"subjects,omitempty"`  // sorted by (Cluster, Kind, Key)
	Timelines []TimelineState `json:"timelines,omitempty"` // sorted by cluster
}

// SubjectState is everything the substrate holds about one subject.
type SubjectState struct {
	Subject   SubjectRef       `json:"subject"`
	Events    []StoredEvent    `json:"events,omitempty"`    // oldest arrival first
	Decisions []StoredDecision `json:"decisions,omitempty"` // oldest arrival first
	Series    *SeriesState     `json:"series,omitempty"`
}

// StoredEvent pairs an event with its arrival sequence — the sequence is
// the deterministic eviction and tie-break key, so it must survive a
// restart or query order would change across restarts.
type StoredEvent struct {
	Seq   uint64        `json:"seq"`
	Event EvidenceEvent `json:"event"`
}

// StoredDecision pairs a decision with its arrival sequence.
type StoredDecision struct {
	Seq      uint64         `json:"seq"`
	Decision DecisionRecord `json:"decision"`
}

// SeriesState is one subject's tiered history, including the partially
// accumulated hour and day — dropping those would lose up to 24h of
// history on every restart.
type SeriesState struct {
	LastAt  time.Time    `json:"lastAt"`
	LastSeq uint64       `json:"lastSeq"`
	Raw     []Sample     `json:"raw,omitempty"`
	Hourly  []Digest     `json:"hourly,omitempty"`
	Daily   []Digest     `json:"daily,omitempty"`
	Pending *PendingHour `json:"pending,omitempty"`
	Day     *Digest      `json:"day,omitempty"`
}

// PendingHour is the not-yet-finalized hour accumulator.
type PendingHour struct {
	Hour     time.Time `json:"hour"`
	CPU      []int64   `json:"cpu,omitempty"`
	Mem      []int64   `json:"mem,omitempty"`
	Throttle []float64 `json:"throttle,omitempty"`
	Restarts int64     `json:"restarts,omitempty"`
	OOMs     int64     `json:"ooms,omitempty"`
	Dropped  int64     `json:"dropped,omitempty"`
}

// TimelineState is one cluster's tier-3 point ring.
type TimelineState struct {
	Cluster string          `json:"cluster"`
	Points  []TimelinePoint `json:"points,omitempty"`
}

// Checkpoint captures the substrate's state. The result shares no memory
// with the store: it is safe to encode after the store resumes writing.
func (m *Memory) Checkpoint() *Checkpoint {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cp := &Checkpoint{Version: CheckpointVersion, Config: m.cfg, Seq: m.seq}

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

	cp.Subjects = make([]SubjectState, 0, len(refs))
	for _, ref := range refs {
		st := SubjectState{Subject: ref}
		if s := m.events.subs[ref]; s != nil {
			st.Events = make([]StoredEvent, 0, s.ring.len())
			for i := 0; i < s.ring.len(); i++ {
				e := s.ring.at(i)
				st.Events = append(st.Events, StoredEvent{Seq: e.seq, Event: copyEvent(e.v)})
			}
		}
		if s := m.decisions.subs[ref]; s != nil {
			st.Decisions = make([]StoredDecision, 0, s.ring.len())
			for i := 0; i < s.ring.len(); i++ {
				e := s.ring.at(i)
				st.Decisions = append(st.Decisions, StoredDecision{Seq: e.seq, Decision: copyDecision(e.v)})
			}
		}
		if sr := m.series[ref]; sr != nil {
			st.Series = seriesState(sr)
		}
		cp.Subjects = append(cp.Subjects, st)
	}

	clusters := make([]string, 0, len(m.timelines))
	for c := range m.timelines {
		clusters = append(clusters, c)
	}
	sort.Strings(clusters)
	cp.Timelines = make([]TimelineState, 0, len(clusters))
	for _, c := range clusters {
		tl := m.timelines[c]
		ts := TimelineState{Cluster: c, Points: make([]TimelinePoint, 0, tl.points.len())}
		for i := 0; i < tl.points.len(); i++ {
			p := *tl.points.at(i)
			p.Events = nil // the overlay is query-computed, never stored
			ts.Points = append(ts.Points, p)
		}
		cp.Timelines = append(cp.Timelines, ts)
	}
	return cp
}

func seriesState(sr *series) *SeriesState {
	st := &SeriesState{LastAt: sr.lastAt, LastSeq: sr.lastSeq}
	for i := 0; i < sr.raw.len(); i++ {
		st.Raw = append(st.Raw, *sr.raw.at(i))
	}
	for i := 0; i < sr.hourly.len(); i++ {
		st.Hourly = append(st.Hourly, *sr.hourly.at(i))
	}
	for i := 0; i < sr.daily.len(); i++ {
		st.Daily = append(st.Daily, *sr.daily.at(i))
	}
	if sr.acc != nil {
		st.Pending = &PendingHour{
			Hour:     sr.acc.hour,
			CPU:      append([]int64(nil), sr.acc.cpu...),
			Mem:      append([]int64(nil), sr.acc.mem...),
			Throttle: append([]float64(nil), sr.acc.throttle...),
			Restarts: sr.acc.restarts,
			OOMs:     sr.acc.ooms,
			Dropped:  sr.acc.dropped,
		}
	}
	if sr.day != nil {
		d := *sr.day
		st.Day = &d
	}
	return st
}

// MarshalCheckpoint encodes the substrate as JSON. Encoding the same state
// twice is byte-identical (Checkpoint sorts everything; encoding/json sorts
// map keys), which is what makes checkpoints diffable and comparable.
func (m *Memory) MarshalCheckpoint() ([]byte, error) {
	return json.Marshal(m.Checkpoint())
}

// UnmarshalCheckpoint decodes without applying. Use FromCheckpoint to
// validate and build a store.
func UnmarshalCheckpoint(data []byte) (*Checkpoint, error) {
	if len(data) == 0 {
		return nil, ErrNoCheckpoint
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("evidence: decoding checkpoint: %w", err)
	}
	return &cp, nil
}

// FromCheckpoint rebuilds a substrate. Every record is validated, never
// transformed: a checkpoint that would need normalizing is rejected rather
// than silently rewritten, so a restored store re-checkpoints byte-exactly.
//
// The checkpoint's own Config is used. If it holds more than the config's
// byte budgets allow (a budget tightened between runs), the excess is
// evicted oldest-first and counted in Stats — restoring never exceeds a
// bound just because a previous run did.
func FromCheckpoint(cp *Checkpoint) (*Memory, error) {
	if cp == nil {
		return nil, ErrNoCheckpoint
	}
	if cp.Version != CheckpointVersion {
		return nil, fmt.Errorf("evidence: checkpoint version %d, want %d", cp.Version, CheckpointVersion)
	}
	m, err := NewMemory(cp.Config)
	if err != nil {
		return nil, fmt.Errorf("evidence: checkpoint config: %w", err)
	}
	cfg := m.cfg

	var lastRef *SubjectRef
	for i := range cp.Subjects {
		st := &cp.Subjects[i]
		if err := validateSubject(st.Subject); err != nil {
			return nil, err
		}
		if lastRef != nil && !lastRef.less(st.Subject) {
			return nil, fmt.Errorf("evidence: checkpoint subjects out of order at %q", st.Subject)
		}
		ref := st.Subject
		lastRef = &ref
		if err := m.restoreEvents(st, cfg); err != nil {
			return nil, err
		}
		if err := m.restoreDecisions(st, cfg); err != nil {
			return nil, err
		}
		if err := m.restoreSeries(st, cfg); err != nil {
			return nil, err
		}
	}

	lastCluster := ""
	for i := range cp.Timelines {
		ts := &cp.Timelines[i]
		if !cleanStringOK(ts.Cluster, maxClusterLen) || ts.Cluster == "" {
			return nil, fmt.Errorf("evidence: checkpoint timeline cluster %q invalid", ts.Cluster)
		}
		if i > 0 && ts.Cluster <= lastCluster {
			return nil, fmt.Errorf("evidence: checkpoint timelines out of order at %q", ts.Cluster)
		}
		lastCluster = ts.Cluster
		if len(ts.Points) > cfg.TimelineCap {
			return nil, fmt.Errorf("evidence: checkpoint cluster %q holds %d points, cap %d",
				ts.Cluster, len(ts.Points), cfg.TimelineCap)
		}
		if len(m.timelines) >= cfg.MaxTimelineClusters {
			return nil, fmt.Errorf("evidence: checkpoint holds more than %d timeline clusters",
				cfg.MaxTimelineClusters)
		}
		tl := &timeline{}
		for j := range ts.Points {
			p := ts.Points[j]
			if p.Events != nil {
				return nil, fmt.Errorf("evidence: checkpoint timeline point carries a stored overlay")
			}
			if err := validatePointValues(p); err != nil {
				return nil, err
			}
			if !timeStoredOK(p.At) {
				return nil, fmt.Errorf("evidence: checkpoint timeline point time not storage-normal")
			}
			if j > 0 && p.At.Before(tl.lastAt) {
				return nil, fmt.Errorf("evidence: checkpoint timeline %q points out of order", ts.Cluster)
			}
			tl.points.push(p, cfg.TimelineCap)
			tl.lastAt = p.At
		}
		if tl.points.len() > 0 {
			m.timelines[ts.Cluster] = tl
		}
	}

	if cp.Seq < m.seq {
		return nil, fmt.Errorf("evidence: checkpoint seq %d below the sequences it stores", cp.Seq)
	}
	m.seq = cp.Seq
	m.stats.EvictedEventsBudget += uint64(m.events.enforceBudget(cfg.MaxEventBytes))
	m.stats.EvictedDecisionsBudget += uint64(m.decisions.enforceBudget(cfg.MaxDecisionBytes))
	m.enforceSeriesBudget()
	return m, nil
}

func (m *Memory) restoreEvents(st *SubjectState, cfg Config) error {
	if len(st.Events) > cfg.MaxEventsPerSubject {
		return fmt.Errorf("evidence: checkpoint subject %q holds %d events, cap %d",
			st.Subject, len(st.Events), cfg.MaxEventsPerSubject)
	}
	prevSeq := uint64(0)
	for i := range st.Events {
		se := st.Events[i]
		if err := validateEvent(se.Event); err != nil {
			return err
		}
		if se.Event.Subject != st.Subject {
			return fmt.Errorf("evidence: checkpoint event filed under %q but carries %q",
				st.Subject, se.Event.Subject)
		}
		if se.Seq == 0 || se.Seq <= prevSeq {
			return fmt.Errorf("evidence: checkpoint subject %q event sequences not increasing", st.Subject)
		}
		prevSeq = se.Seq
		e := se.Event
		e.Attrs = copyEvent(e).Attrs
		m.events.append(st.Subject, e, eventBytes(&e), se.Seq, cfg.MaxEventsPerSubject)
		if se.Seq > m.seq {
			m.seq = se.Seq
		}
	}
	return nil
}

func (m *Memory) restoreDecisions(st *SubjectState, cfg Config) error {
	if len(st.Decisions) > cfg.MaxDecisionsPerSubject {
		return fmt.Errorf("evidence: checkpoint subject %q holds %d decisions, cap %d",
			st.Subject, len(st.Decisions), cfg.MaxDecisionsPerSubject)
	}
	prevSeq := uint64(0)
	for i := range st.Decisions {
		sd := st.Decisions[i]
		if err := validateDecision(sd.Decision, cfg.MaxDecisionPayloadBytes); err != nil {
			return err
		}
		if sd.Decision.Subject != st.Subject {
			return fmt.Errorf("evidence: checkpoint decision filed under %q but carries %q",
				st.Subject, sd.Decision.Subject)
		}
		if sd.Seq == 0 || sd.Seq <= prevSeq {
			return fmt.Errorf("evidence: checkpoint subject %q decision sequences not increasing", st.Subject)
		}
		prevSeq = sd.Seq
		d := copyDecision(sd.Decision)
		m.decisions.append(st.Subject, d, decisionBytes(&d), sd.Seq, cfg.MaxDecisionsPerSubject)
		if sd.Seq > m.seq {
			m.seq = sd.Seq
		}
	}
	return nil
}

func (m *Memory) restoreSeries(st *SubjectState, cfg Config) error {
	ss := st.Series
	if ss == nil {
		return nil
	}
	if len(ss.Raw) > cfg.RawSampleCap || len(ss.Hourly) > cfg.HourlyCap || len(ss.Daily) > cfg.DailyCap {
		return fmt.Errorf("evidence: checkpoint subject %q series exceeds its ring caps", st.Subject)
	}
	sr := &series{ref: st.Subject, hIdx: -1, lastAt: ss.LastAt, lastSeq: ss.LastSeq}
	var prev time.Time
	for _, smp := range ss.Raw {
		if err := validateSample(smp); err != nil {
			return err
		}
		if !prev.IsZero() && smp.At.Before(prev) {
			return fmt.Errorf("evidence: checkpoint subject %q raw samples out of order", st.Subject)
		}
		prev = smp.At
		sr.raw.push(smp, cfg.RawSampleCap)
	}
	if len(ss.Raw) > 0 && ss.LastAt.Before(prev) {
		return fmt.Errorf("evidence: checkpoint subject %q lastAt precedes its newest sample", st.Subject)
	}
	for _, tier := range []struct {
		digests []Digest
		want    int
		r       *ring[Digest]
		capN    int
	}{
		{ss.Hourly, TierHourly, &sr.hourly, cfg.HourlyCap},
		{ss.Daily, TierDaily, &sr.daily, cfg.DailyCap},
	} {
		var end time.Time
		for _, d := range tier.digests {
			if err := validateDigest(d, tier.want); err != nil {
				return err
			}
			if !end.IsZero() && d.Start.Before(end) {
				return fmt.Errorf("evidence: checkpoint subject %q tier-%d digests overlap",
					st.Subject, tier.want)
			}
			end = d.End
			tier.r.push(d, tier.capN)
		}
	}
	if p := ss.Pending; p != nil {
		if !timeStoredOK(p.Hour) || !p.Hour.Equal(p.Hour.Truncate(time.Hour)) {
			return fmt.Errorf("evidence: checkpoint subject %q pending hour %v invalid", st.Subject, p.Hour)
		}
		if len(p.CPU) != len(p.Mem) || len(p.CPU) != len(p.Throttle) {
			return fmt.Errorf("evidence: checkpoint subject %q pending hour arrays disagree", st.Subject)
		}
		if len(p.CPU) == 0 || len(p.CPU) > cfg.MaxSamplesPerHour {
			return fmt.Errorf("evidence: checkpoint subject %q pending hour holds %d samples, cap %d",
				st.Subject, len(p.CPU), cfg.MaxSamplesPerHour)
		}
		if p.Restarts < 0 || p.OOMs < 0 || p.Dropped < 0 {
			return fmt.Errorf("evidence: checkpoint subject %q pending hour counts invalid", st.Subject)
		}
		for i := range p.CPU {
			if err := validateSample(Sample{
				At: p.Hour, MilliCPU: p.CPU[i], MemoryBytes: p.Mem[i], ThrottleRatio: p.Throttle[i],
			}); err != nil {
				return fmt.Errorf("evidence: checkpoint subject %q pending sample %d: %w", st.Subject, i, err)
			}
		}
		sr.acc = &hourAcc{
			hour:     p.Hour,
			cpu:      append([]int64(nil), p.CPU...),
			mem:      append([]int64(nil), p.Mem...),
			throttle: append([]float64(nil), p.Throttle...),
			restarts: p.Restarts,
			ooms:     p.OOMs,
			dropped:  p.Dropped,
		}
	}
	if ss.Day != nil {
		if err := validateDigest(*ss.Day, TierDaily); err != nil {
			return err
		}
		d := *ss.Day
		sr.day = &d
	}
	if sr.raw.len() == 0 && sr.hourly.len() == 0 && sr.daily.len() == 0 && sr.acc == nil && sr.day == nil {
		return nil // an empty series is never retained
	}
	if !timeStoredOK(ss.LastAt) {
		return fmt.Errorf("evidence: checkpoint subject %q series lastAt not storage-normal", st.Subject)
	}
	sr.bytes = int64(sr.raw.len())*sampleBytes +
		int64(sr.hourly.len()+sr.daily.len())*digestBytes
	if sr.acc != nil {
		sr.bytes += sr.acc.bytes()
	}
	if sr.day != nil {
		sr.bytes += digestBytes
	}
	m.series[st.Subject] = sr
	m.seriesBytes += sr.bytes + subjectOverheadBytes
	m.seriesHeap.push(sr)
	if ss.LastSeq > m.seq {
		m.seq = ss.LastSeq
	}
	return nil
}

// Save encodes the substrate and hands the bytes to cs.
func Save(ctx context.Context, cs CheckpointStore, m *Memory) error {
	data, err := m.MarshalCheckpoint()
	if err != nil {
		return fmt.Errorf("evidence: encoding checkpoint: %w", err)
	}
	return cs.SaveCheckpoint(ctx, data)
}

// Load reads, decodes and validates a checkpoint from cs. A cold store
// (no bytes yet) yields ErrNoCheckpoint, which callers treat as "start
// empty", not as corruption.
func Load(ctx context.Context, cs CheckpointStore) (*Memory, error) {
	data, err := cs.LoadCheckpoint(ctx)
	if err != nil {
		return nil, err
	}
	cp, err := UnmarshalCheckpoint(data)
	if err != nil {
		return nil, err
	}
	return FromCheckpoint(cp)
}
