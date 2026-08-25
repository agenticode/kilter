package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// TierNone marks a usage summary computed from no data at all.
const TierNone = -1

// Dossier defaults and hard limits (§3.4: "size-capped (~4 KiB JSON) so it
// is a safe retrieval unit for LLM context").
const (
	DefaultDossierBytes     = 4096
	defaultDossierEvents    = 24
	defaultDossierDecisions = 8
	defaultDossierDigests   = 24
	maxDossierItems         = 256
	maxDossierBytes         = 1 << 20
	minDossierBytes         = 256
)

// Attachment is an opaque section contributed by a higher layer — the
// sizing recommendation, a refusal, the workload class, guard state, cost.
// The substrate stores and size-caps it without interpreting it: §8 fixes
// evidence below recommend/decision/patterns, so it must not import them.
// Data must be valid, compact JSON.
type Attachment struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

// UsageSummary is the p50/p95/p99/max view of a subject over the dossier
// window. Tier records where it came from: TierRaw is exact, the digest
// tiers are conservative upper bounds (element-wise max merging), and
// TierNone means the window held no usage data at all.
type UsageSummary struct {
	From          time.Time   `json:"from"`
	To            time.Time   `json:"to"`
	Tier          int         `json:"tier"`
	Samples       int64       `json:"samples"`
	Windows       int         `json:"windows"`
	CPU           DigestStats `json:"cpu"`
	Mem           DigestStats `json:"mem"`
	ThrottleRatio float64     `json:"throttleRatio"`
	Restarts      int64       `json:"restarts"`
	OOMs          int64       `json:"ooms"`
}

// Truncation records exactly what the size cap cost. Nothing is dropped
// silently: a caller can always tell whether it is looking at the whole
// picture.
type Truncation struct {
	Events      int    `json:"events,omitempty"`
	Decisions   int    `json:"decisions,omitempty"`
	Digests     int    `json:"digests,omitempty"`
	Attachments int    `json:"attachments,omitempty"`
	Reason      string `json:"reason,omitempty"` // "count-cap" | "byte-cap"
}

func (t Truncation) any() bool {
	return t.Events > 0 || t.Decisions > 0 || t.Digests > 0 || t.Attachments > 0
}

// Dossier is the bounded, deterministic case file for one subject (§3.4) —
// the retrieval unit for the UI, the API, MCP and the LLM alike. It is
// guaranteed to encode within DossierRequest.MaxBytes.
type Dossier struct {
	Subject     SubjectRef       `json:"subject"`
	From        time.Time        `json:"from,omitempty"`
	To          time.Time        `json:"to,omitempty"`
	Usage       UsageSummary     `json:"usage"`
	Events      []EvidenceEvent  `json:"events,omitempty"`    // newest first
	Decisions   []DecisionRecord `json:"decisions,omitempty"` // newest first
	Digests     []Digest         `json:"digests,omitempty"`   // oldest first
	Attachments []Attachment     `json:"attachments,omitempty"`
	Truncated   *Truncation      `json:"truncated,omitempty"`
}

// DossierRequest parameterizes the build. Zero-valued caps take defaults;
// a zero From/To means unbounded on that side.
type DossierRequest struct {
	Subject      SubjectRef
	From, To     time.Time
	MaxBytes     int // default DefaultDossierBytes
	MaxEvents    int // default 24; a negative value omits events
	MaxDecisions int // default 8; a negative value omits decisions
	MaxDigests   int // default 24; a negative value omits digests
	// DigestTier selects which stored tier appears in Digests and is
	// preferred for the usage summary. Default TierHourly.
	DigestTier int
	// Attachments are higher-layer sections, included in request order.
	Attachments []Attachment
}

func (r DossierRequest) withDefaults() DossierRequest {
	if r.MaxBytes == 0 {
		r.MaxBytes = DefaultDossierBytes
	}
	if r.MaxEvents == 0 {
		r.MaxEvents = defaultDossierEvents
	}
	if r.MaxDecisions == 0 {
		r.MaxDecisions = defaultDossierDecisions
	}
	if r.MaxDigests == 0 {
		r.MaxDigests = defaultDossierDigests
	}
	// A negative cap means "none of this section" — clamped here so the
	// count caps below are always valid slice bounds.
	if r.MaxEvents < 0 {
		r.MaxEvents = 0
	}
	if r.MaxDecisions < 0 {
		r.MaxDecisions = 0
	}
	if r.MaxDigests < 0 {
		r.MaxDigests = 0
	}
	return r
}

func (r DossierRequest) validate() error {
	if r.MaxBytes < minDossierBytes || r.MaxBytes > maxDossierBytes {
		return fmt.Errorf("evidence: dossier MaxBytes=%d outside [%d, %d]",
			r.MaxBytes, minDossierBytes, maxDossierBytes)
	}
	for _, b := range []struct {
		name string
		v    int
	}{{"MaxEvents", r.MaxEvents}, {"MaxDecisions", r.MaxDecisions}, {"MaxDigests", r.MaxDigests}} {
		if b.v > maxDossierItems {
			return fmt.Errorf("evidence: dossier %s=%d over the hard limit %d", b.name, b.v, maxDossierItems)
		}
	}
	switch r.DigestTier {
	case TierRaw, TierHourly, TierDaily:
	default:
		return fmt.Errorf("evidence: dossier DigestTier=%d unknown", r.DigestTier)
	}
	if len(r.Attachments) > maxDossierItems {
		return fmt.Errorf("evidence: dossier carries %d attachments, limit %d",
			len(r.Attachments), maxDossierItems)
	}
	for _, a := range r.Attachments {
		if a.Name == "" || !cleanStringOK(a.Name, maxKindLen) {
			return fmt.Errorf("evidence: dossier attachment name %q invalid", a.Name)
		}
		if len(a.Data) == 0 {
			return fmt.Errorf("evidence: dossier attachment %q has no data", a.Name)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, a.Data); err != nil {
			return fmt.Errorf("evidence: dossier attachment %q is not valid JSON: %w", a.Name, err)
		}
		if !bytes.Equal(buf.Bytes(), a.Data) {
			return fmt.Errorf("evidence: dossier attachment %q must be compact JSON", a.Name)
		}
	}
	return nil
}

// Dossier assembles the case file for one subject over a window.
func (m *Memory) Dossier(req DossierRequest) (*Dossier, error) {
	return BuildDossier(m, req)
}

// BuildDossier composes a dossier from any Store — the in-memory substrate
// today, the bbolt-backed one later — using only the §3.4 query surface.
//
// The result is deterministic: it depends solely on the store's contents
// and the request, never on map iteration or wall-clock time. It is also
// guaranteed to encode within req.MaxBytes; whatever had to go to get there
// is reported in Truncated.
func BuildDossier(st Store, req DossierRequest) (*Dossier, error) {
	if st == nil {
		return nil, fmt.Errorf("evidence: dossier needs a store")
	}
	req = req.withDefaults()
	subject, err := sanitizeSubject(req.Subject)
	if err != nil {
		return nil, err
	}
	req.Subject = subject
	if err := req.validate(); err != nil {
		return nil, err
	}
	if err := checkWindow(req.From, req.To); err != nil {
		return nil, err
	}
	from, to := utcTime(req.From), utcTime(req.To)

	d := &Dossier{Subject: subject, From: from, To: to}
	var trunc Truncation

	events, err := st.Events(subject, from, to)
	if err != nil {
		return nil, err
	}
	reverseEvents(events)
	if len(events) > req.MaxEvents {
		trunc.Events += len(events) - req.MaxEvents
		trunc.Reason = "count-cap"
		events = events[:req.MaxEvents]
	}
	d.Events = events

	decisions, err := st.Decisions(subject, from, to)
	if err != nil {
		return nil, err
	}
	reverseDecisions(decisions)
	if len(decisions) > req.MaxDecisions {
		trunc.Decisions += len(decisions) - req.MaxDecisions
		trunc.Reason = "count-cap"
		decisions = decisions[:req.MaxDecisions]
	}
	d.Decisions = decisions

	if req.MaxDigests > 0 {
		digests, err := st.Digests(subject, from, to, req.DigestTier)
		if err != nil {
			return nil, err
		}
		if len(digests) > req.MaxDigests {
			// Keep the most recent windows: they describe the subject now.
			trunc.Digests += len(digests) - req.MaxDigests
			trunc.Reason = "count-cap"
			digests = digests[len(digests)-req.MaxDigests:]
		}
		d.Digests = digests
	}

	d.Usage, err = summarizeUsage(st, subject, from, to, req.DigestTier)
	if err != nil {
		return nil, err
	}
	if len(req.Attachments) > 0 {
		d.Attachments = append([]Attachment(nil), req.Attachments...)
	}

	if err := d.shrinkTo(req.MaxBytes, &trunc); err != nil {
		return nil, err
	}
	return d, nil
}

// summarizeUsage folds the finest usage data available in the window into
// one summary, preferring the requested tier and falling back through the
// coarser ones so a dossier is never usage-blind just because the requested
// tier happens to be empty.
func summarizeUsage(st Store, s SubjectRef, from, to time.Time, prefer int) (UsageSummary, error) {
	sum := UsageSummary{From: from, To: to, Tier: TierNone}
	order := []int{prefer}
	for _, t := range []int{TierRaw, TierHourly, TierDaily} {
		if t != prefer {
			order = append(order, t)
		}
	}
	for _, tier := range order {
		digests, err := st.Digests(s, from, to, tier)
		if err != nil {
			return sum, err
		}
		if len(digests) == 0 {
			continue
		}
		sum.Tier = tier
		sum.Windows = len(digests)
		acc := digests[0]
		for i := 1; i < len(digests); i++ {
			next := digests[i]
			foldInto(&acc, &next)
		}
		sum.Samples = acc.Samples
		sum.CPU, sum.Mem = acc.CPU, acc.Mem
		sum.ThrottleRatio = acc.ThrottleRatio
		sum.Restarts, sum.OOMs = acc.Restarts, acc.OOMs
		return sum, nil
	}
	return sum, nil
}

// shrinkTo drops content until the dossier encodes within max bytes.
//
// The ladder is fixed and documented so two callers with the same store and
// request always get the same dossier: coarse history goes first (digests,
// oldest first), then decisions (oldest), then events (oldest), then
// attachments (last-listed first). The subject, window and usage summary
// are the irreducible skeleton — if that alone does not fit, the request is
// rejected rather than a misleading fragment returned.
func (d *Dossier) shrinkTo(max int, trunc *Truncation) error {
	for {
		// The truncation notice is part of what is encoded, so it must be
		// attached before measuring: sizing the dossier without it and
		// adding it afterwards is exactly how a size cap gets exceeded.
		if trunc.any() {
			t := *trunc
			d.Truncated = &t
		} else {
			d.Truncated = nil
		}
		data, err := json.Marshal(d)
		if err != nil {
			return fmt.Errorf("evidence: encoding dossier: %w", err)
		}
		if len(data) <= max {
			return nil
		}
		if !d.dropOne(trunc) {
			return fmt.Errorf("evidence: dossier for %s needs %d bytes, cap is %d",
				d.Subject, len(data), max)
		}
		trunc.Reason = "byte-cap"
	}
}

// dropOne removes the least valuable remaining item; reports false when
// only the skeleton is left.
func (d *Dossier) dropOne(trunc *Truncation) bool {
	switch {
	case len(d.Digests) > 0:
		d.Digests = d.Digests[1:] // oldest window first
		trunc.Digests++
	case len(d.Decisions) > 0:
		d.Decisions = d.Decisions[:len(d.Decisions)-1] // newest-first slice: drop the oldest
		trunc.Decisions++
	case len(d.Events) > 0:
		d.Events = d.Events[:len(d.Events)-1]
		trunc.Events++
	case len(d.Attachments) > 0:
		d.Attachments = d.Attachments[:len(d.Attachments)-1]
		trunc.Attachments++
	default:
		return false
	}
	return true
}

// Encode renders the dossier as JSON. For a dossier from BuildDossier the
// result is within the requested byte cap.
func (d *Dossier) Encode() ([]byte, error) { return json.Marshal(d) }

func reverseEvents(s []EvidenceEvent) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func reverseDecisions(s []DecisionRecord) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
