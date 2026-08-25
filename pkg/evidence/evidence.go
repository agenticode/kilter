// Package evidence is Kilter's L0 evidence substrate: an append-mostly,
// bounded, queryable record of what happened to every subject the engine
// watches — deploys, HPA actions, OOMKills, node pressure, throttling,
// Kilter's own actuation steps — plus tiered usage digests compact enough
// for 50k workloads and a decision journal for backtesting.
//
// Design reference: docs/design/reasoning-engine.md §3. Three rules govern
// every type here:
//
//   - Bounded memory is a hard invariant. Every store is ring-capped per
//     subject AND byte-budgeted globally, with deterministic eviction.
//     Nothing grows without a bound the tests assert.
//   - Determinism. No time.Now() in any code path — callers pass clocks.
//     No map-iteration order leaks into any output: queries and checkpoints
//     sort with documented total orders.
//   - No silent failure. Garbage input is rejected with an error or
//     normalized by documented rules; every eviction and drop is counted in
//     Stats.
//
// The package depends only on pkg/model and the standard library (§8
// dependency direction: evidence sits below everything).
package evidence

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agenticode/kilter/pkg/model"
)

// Subject kinds. SubjectRef.Kind is open-ended (a later unit adds
// "instance" for plain EC2); these are the vocabulary used today.
const (
	SubjectContainer = "container"
	SubjectWorkload  = "workload"
	SubjectNode      = "node"
	SubjectCluster   = "cluster"
)

// SubjectRef generalizes "what the evidence is about". Cluster scopes the
// subject (it is the <cluster> segment of the evt/<cluster>/<kind>/<key>
// storage key in the design) and may be empty for fleet-level subjects such
// as pricing-catalog changes.
type SubjectRef struct {
	Cluster string `json:"cluster,omitempty"`
	Kind    string `json:"kind"` // "container" | "workload" | "node" | "cluster"
	Key     string `json:"key"`  // ContainerKey.String() | WorkloadRef.String() | node name | cluster id
}

func (s SubjectRef) String() string {
	return s.Cluster + "/" + s.Kind + "/" + s.Key
}

// less is the total order used everywhere subjects are enumerated
// (checkpoints, Subjects()): by Cluster, then Kind, then Key.
func (s SubjectRef) less(o SubjectRef) bool {
	if s.Cluster != o.Cluster {
		return s.Cluster < o.Cluster
	}
	if s.Kind != o.Kind {
		return s.Kind < o.Kind
	}
	return s.Key < o.Key
}

// ClusterSubject refers to a cluster as a whole.
func ClusterSubject(cluster string) SubjectRef {
	return SubjectRef{Cluster: cluster, Kind: SubjectCluster, Key: cluster}
}

// NodeSubject refers to one node.
func NodeSubject(cluster, node string) SubjectRef {
	return SubjectRef{Cluster: cluster, Kind: SubjectNode, Key: node}
}

// WorkloadSubject refers to one workload (controller).
func WorkloadSubject(cluster string, ref model.WorkloadRef) SubjectRef {
	return SubjectRef{Cluster: cluster, Kind: SubjectWorkload, Key: ref.String()}
}

// ContainerSubject refers to one container template within a workload.
func ContainerSubject(cluster string, key model.ContainerKey) SubjectRef {
	return SubjectRef{Cluster: cluster, Kind: SubjectContainer, Key: key.String()}
}

// Event kinds. EvidenceEvent.Kind is open-ended; these are the enumerated
// vocabulary collectors emit today (§3.2 signal inventory).
const (
	EventDeploy           = "deploy"             // spec change: image, resources, replicas, generation
	EventHPAScale         = "hpa-scale"          // HPA scale action or min/max/target change
	EventOOMKill          = "oomkill"            // exact OOMKill with container + timestamp
	EventThrottleHigh     = "throttle-high"      // sustained CFS throttling above threshold
	EventNodePressure     = "node-pressure"      // node condition (MemoryPressure, DiskPressure, ...)
	EventEvicted          = "evicted"            // pod evicted
	EventFailedScheduling = "failed-scheduling"  // pod could not schedule
	EventProbeFailure     = "probe-failure"      // liveness/readiness probe failure
	EventImagePullBackOff = "image-pull-backoff" // image pull failure
	EventSpotInterrupt    = "spot-interrupt"     // spot interruption / rebalance recommendation
	EventPricingChange    = "pricing-change"     // pricing catalog sync delta
	EventKilterAction     = "kilter-action"      // an applied Kilter action (mirrors the ledger)
	EventActuationStep    = "actuation-step"     // one executed plan step
	EventRegimeChange     = "regime-change"      // changepoint detector fired (later unit emits)
	EventFinding          = "finding"            // operator-pinned investigation finding
)

// Severities.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// severityRank orders severities for overlay capping and dedup upgrades.
// Unknown severities never occur post-sanitize.
func severityRank(s string) int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	}
	return 0
}

// EvidenceEvent is one typed observation about a subject. ~200–400 bytes
// JSON-encoded; the in-memory representation is bounded by the sanitize
// caps below.
type EvidenceEvent struct {
	At       time.Time  `json:"at"`
	Kind     string     `json:"kind"`
	Subject  SubjectRef `json:"subject"`
	Severity string     `json:"severity"`
	// Attrs carries small allowlisted key/value details (collectors own the
	// allowlist per INV-3; the substrate enforces count and length caps).
	Attrs map[string]string `json:"attrs,omitempty"`
	// Dedup collapses informer replays and repeated warnings: an appended
	// event whose (Kind, Dedup) matches a stored event within
	// Config.DedupWindow is folded into it instead of stored again.
	Dedup string `json:"dedup,omitempty"`
	// Count is the number of occurrences folded into this event beyond the
	// first: 0 means the event happened once, N>0 means 1+N occurrences.
	// Callers must leave it zero on Append; the substrate maintains it.
	Count int `json:"count,omitempty"`
}

// Decision kinds for DecisionRecord.Kind.
const (
	DecisionRecommendation = "recommendation"
	DecisionRefusal        = "refusal"
	DecisionAction         = "action"
)

// DecisionRecord journals every recommendation actually surfaced, every
// refusal, and every applied action — what backtesting scores and what
// "why did kilter say that in March" replays. The Payload is an opaque,
// size-capped JSON document owned by the producing layer (pkg/recommend or
// later pkg/decision); the substrate stores it verbatim.
type DecisionRecord struct {
	At          time.Time       `json:"at"`
	Subject     SubjectRef      `json:"subject"`
	Kind        string          `json:"kind"` // recommendation | refusal | action
	Summary     string          `json:"summary"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"` // valid JSON, ≤ Config.MaxDecisionPayloadBytes
}

// Sample is one usage observation for a subject (typically a container),
// the raw material of tier-0 series history. Restarts and OOMs are deltas
// observed since the previous sample, not cumulative counters.
type Sample struct {
	At            time.Time `json:"at"`
	MilliCPU      int64     `json:"milliCPU"`
	MemoryBytes   int64     `json:"memoryBytes"`
	ThrottleRatio float64   `json:"throttleRatio,omitempty"` // throttled/total CFS periods, 0..1
	Restarts      int64     `json:"restarts,omitempty"`
	OOMs          int64     `json:"ooms,omitempty"`
}

// TimelinePoint is one cluster-level cost/demand observation (tier 3).
// Events is filled by Timeline queries as an overlay; it must be nil on
// ObservePoint.
type TimelinePoint struct {
	At             time.Time       `json:"at"`
	CostUSDPerHour float64         `json:"costUSDPerHour"`
	Nodes          int             `json:"nodes"`
	Events         []EvidenceEvent `json:"events,omitempty"`
}

// Sanitize caps. Strings are stripped of control characters and invalid
// UTF-8, then truncated at a rune boundary to these byte lengths (§5.7:
// free-text fields are length-capped, control characters stripped at
// ingest). Every cap also bounds the byte accounting below.
const (
	maxKindLen        = 64
	maxDedupLen       = 128
	maxSubjectKindLen = 32
	maxSubjectKeyLen  = 512 // ContainerKey worst case ≈ 400 bytes
	maxClusterLen     = 128
	maxAttrs          = 16
	maxAttrKeyLen     = 64
	maxAttrValLen     = 128
	maxSummaryLen     = 256
	maxFingerprintLen = 64
	maxEventCount     = 1 << 30 // dedup Count saturates here
)

// maxSampleValue rejects absurd magnitudes the same way pkg/patterns does:
// values above it are pipeline garbage, and keeping every stored value
// below it keeps all digest arithmetic comfortably finite.
const maxSampleValue = int64(1e18)

// cleanString strips control characters (C0 and DEL) and invalid UTF-8,
// then truncates to at most max bytes at a rune boundary. Sanitized strings
// round-trip encoding/json byte-exactly (json replaces invalid UTF-8,
// which would break codec exactness).
func cleanString(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r < 0x20 || r == 0x7f || (r == utf8.RuneError && size == 1) {
			continue
		}
		if b.Len()+size > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cleanStringOK reports whether s is already clean under the given cap —
// the validation dual of cleanString, used when restoring checkpoints
// (restore validates, never transforms, so valid checkpoints round-trip
// byte-exactly).
func cleanStringOK(s string, max int) bool {
	return len(s) <= max && cleanString(s, max) == s
}

// sanitizeSubject normalizes a subject in place. Kind and Key must be
// non-empty after cleaning.
func sanitizeSubject(s SubjectRef) (SubjectRef, error) {
	s.Cluster = cleanString(s.Cluster, maxClusterLen)
	s.Kind = cleanString(s.Kind, maxSubjectKindLen)
	s.Key = cleanString(s.Key, maxSubjectKeyLen)
	if s.Kind == "" || s.Key == "" {
		return s, fmt.Errorf("evidence: subject needs kind and key: %q", s.String())
	}
	return s, nil
}

func validateSubject(s SubjectRef) error {
	if !cleanStringOK(s.Cluster, maxClusterLen) || !cleanStringOK(s.Kind, maxSubjectKindLen) ||
		!cleanStringOK(s.Key, maxSubjectKeyLen) || s.Kind == "" || s.Key == "" {
		return fmt.Errorf("evidence: invalid subject %q", s.String())
	}
	return nil
}

// utcTime normalizes t for storage. Zero times are the caller's bug and
// rejected by the sanitizers.
func utcTime(t time.Time) time.Time { return t.UTC() }

// timeStoredOK reports whether a checkpointed time is storage-normal:
// non-zero and in UTC (what utcTime produces and what JSON "Z" decodes to).
func timeStoredOK(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC
}

// Config bounds every store in the substrate. The zero value of any field
// means "use the default"; NewMemory fills defaults before validating.
type Config struct {
	// Event log (§3.3a): per-subject ring + age + global byte budget.
	MaxEventsPerSubject int           // default 256
	EventMaxAge         time.Duration // default 90 days; applied by Prune
	MaxEventBytes       int64         // default 256 MiB accounted bytes
	DedupWindow         time.Duration // default 1h; see EvidenceEvent.Dedup

	// Series history (§3.3b): tier 0 raw ring, tier 1 hourly, tier 2 daily.
	RawSampleCap      int           // default 576 (48h at 5-minute samples)
	RawMaxAge         time.Duration // default 48h; applied by Prune
	MaxSamplesPerHour int           // default 120; extra samples in an hour are dropped and counted
	HourlyCap         int           // default 720 digests (30d uncoalesced worst case)
	HourlyMaxAge      time.Duration // default 30 days
	DailyCap          int           // default 400 digests
	DailyMaxAge       time.Duration // default 400 days
	// CoalesceTolerance is the relative tolerance under which an hourly or
	// daily digest is "boring" relative to its predecessor and run-length
	// coalesced into it. 0 < tol < 1.
	CoalesceTolerance float64 // default 0.10
	MaxSeriesBytes    int64   // default 1 GiB accounted bytes

	// Decision journal (§3.3c).
	MaxDecisionsPerSubject  int           // default 100
	DecisionMaxAge          time.Duration // default 400 days
	MaxDecisionBytes        int64         // default 64 MiB accounted bytes
	MaxDecisionPayloadBytes int           // default 4096; larger payloads are rejected

	// Cluster timeline (tier 3).
	TimelineCap              int           // default 9600 points (400d hourly)
	TimelineMaxAge           time.Duration // default 400 days
	MaxTimelineClusters      int           // default 256
	MaxOverlayEventsPerPoint int           // default 32
}

// DefaultConfig returns the production defaults from the design (§3.3).
func DefaultConfig() Config {
	const day = 24 * time.Hour
	return Config{
		MaxEventsPerSubject: 256,
		EventMaxAge:         90 * day,
		MaxEventBytes:       256 << 20,
		DedupWindow:         time.Hour,

		RawSampleCap:      576,
		RawMaxAge:         48 * time.Hour,
		MaxSamplesPerHour: 120,
		HourlyCap:         720,
		HourlyMaxAge:      30 * day,
		DailyCap:          400,
		DailyMaxAge:       400 * day,
		CoalesceTolerance: 0.10,
		MaxSeriesBytes:    1 << 30,

		MaxDecisionsPerSubject:  100,
		DecisionMaxAge:          400 * day,
		MaxDecisionBytes:        64 << 20,
		MaxDecisionPayloadBytes: 4096,

		TimelineCap:              9600,
		TimelineMaxAge:           400 * day,
		MaxTimelineClusters:      256,
		MaxOverlayEventsPerPoint: 32,
	}
}

// maxRingCap bounds every configurable ring so a hostile config or
// checkpoint cannot demand an arbitrarily large allocation (rings grow
// lazily, but caps also gate checkpoint restore sizes).
const maxRingCap = 1 << 20

// minBudgetBytes is the floor for byte budgets: below it a single
// worst-case event (~4.5 KiB accounted) could thrash the whole store.
const minBudgetBytes = 64 << 10

// withDefaults returns c with zero fields replaced by DefaultConfig values.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MaxEventsPerSubject == 0 {
		c.MaxEventsPerSubject = d.MaxEventsPerSubject
	}
	if c.EventMaxAge == 0 {
		c.EventMaxAge = d.EventMaxAge
	}
	if c.MaxEventBytes == 0 {
		c.MaxEventBytes = d.MaxEventBytes
	}
	if c.DedupWindow == 0 {
		c.DedupWindow = d.DedupWindow
	}
	if c.RawSampleCap == 0 {
		c.RawSampleCap = d.RawSampleCap
	}
	if c.RawMaxAge == 0 {
		c.RawMaxAge = d.RawMaxAge
	}
	if c.MaxSamplesPerHour == 0 {
		c.MaxSamplesPerHour = d.MaxSamplesPerHour
	}
	if c.HourlyCap == 0 {
		c.HourlyCap = d.HourlyCap
	}
	if c.HourlyMaxAge == 0 {
		c.HourlyMaxAge = d.HourlyMaxAge
	}
	if c.DailyCap == 0 {
		c.DailyCap = d.DailyCap
	}
	if c.DailyMaxAge == 0 {
		c.DailyMaxAge = d.DailyMaxAge
	}
	if c.CoalesceTolerance == 0 {
		c.CoalesceTolerance = d.CoalesceTolerance
	}
	if c.MaxSeriesBytes == 0 {
		c.MaxSeriesBytes = d.MaxSeriesBytes
	}
	if c.MaxDecisionsPerSubject == 0 {
		c.MaxDecisionsPerSubject = d.MaxDecisionsPerSubject
	}
	if c.DecisionMaxAge == 0 {
		c.DecisionMaxAge = d.DecisionMaxAge
	}
	if c.MaxDecisionBytes == 0 {
		c.MaxDecisionBytes = d.MaxDecisionBytes
	}
	if c.MaxDecisionPayloadBytes == 0 {
		c.MaxDecisionPayloadBytes = d.MaxDecisionPayloadBytes
	}
	if c.TimelineCap == 0 {
		c.TimelineCap = d.TimelineCap
	}
	if c.TimelineMaxAge == 0 {
		c.TimelineMaxAge = d.TimelineMaxAge
	}
	if c.MaxTimelineClusters == 0 {
		c.MaxTimelineClusters = d.MaxTimelineClusters
	}
	if c.MaxOverlayEventsPerPoint == 0 {
		c.MaxOverlayEventsPerPoint = d.MaxOverlayEventsPerPoint
	}
	return c
}

// validate rejects nonsensical configs. Positive-form comparisons so NaN in
// CoalesceTolerance fails validation (NaN > 0 is false).
func (c Config) validate() error {
	intBounds := []struct {
		name string
		v    int
	}{
		{"MaxEventsPerSubject", c.MaxEventsPerSubject},
		{"RawSampleCap", c.RawSampleCap},
		{"MaxSamplesPerHour", c.MaxSamplesPerHour},
		{"HourlyCap", c.HourlyCap},
		{"DailyCap", c.DailyCap},
		{"MaxDecisionsPerSubject", c.MaxDecisionsPerSubject},
		{"TimelineCap", c.TimelineCap},
		{"MaxTimelineClusters", c.MaxTimelineClusters},
		{"MaxOverlayEventsPerPoint", c.MaxOverlayEventsPerPoint},
	}
	for _, b := range intBounds {
		if b.v < 1 || b.v > maxRingCap {
			return fmt.Errorf("evidence: config %s=%d outside [1, %d]", b.name, b.v, maxRingCap)
		}
	}
	durs := []struct {
		name string
		v    time.Duration
	}{
		{"EventMaxAge", c.EventMaxAge},
		{"DedupWindow", c.DedupWindow},
		{"RawMaxAge", c.RawMaxAge},
		{"HourlyMaxAge", c.HourlyMaxAge},
		{"DailyMaxAge", c.DailyMaxAge},
		{"DecisionMaxAge", c.DecisionMaxAge},
		{"TimelineMaxAge", c.TimelineMaxAge},
	}
	for _, b := range durs {
		if b.v <= 0 {
			return fmt.Errorf("evidence: config %s=%v must be positive", b.name, b.v)
		}
	}
	if c.MaxEventBytes < minBudgetBytes || c.MaxSeriesBytes < minBudgetBytes || c.MaxDecisionBytes < minBudgetBytes {
		return fmt.Errorf("evidence: byte budgets must be at least %d bytes", minBudgetBytes)
	}
	if !(c.CoalesceTolerance > 0) || !(c.CoalesceTolerance < 1) || math.IsInf(c.CoalesceTolerance, 0) {
		return fmt.Errorf("evidence: CoalesceTolerance=%v outside (0, 1)", c.CoalesceTolerance)
	}
	if c.MaxDecisionPayloadBytes < 2 || c.MaxDecisionPayloadBytes > 1<<20 {
		return fmt.Errorf("evidence: MaxDecisionPayloadBytes=%d outside [2, %d]", c.MaxDecisionPayloadBytes, 1<<20)
	}
	return nil
}
