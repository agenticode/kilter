package rds

// The provisioning envelope, read live, because AWS's own page contradicts
// itself about it.
//
// §2.4:
//
//	AWS's own page contradicts itself on the SQL Server gp3 ceiling — the gp3
//	table says 3,000–80,000 IOPS while the comparison table on the same page
//	says "Maximum IOPS: 64,000 (16,000 on RDS for SQL Server)". [unverified:
//	which is current]. Do not hardcode either; read the envelope from
//	rds:DescribeValidDBInstanceModifications and refuse when it is unavailable.
//
// So this file is a fourth READ seam beside U11's three, shaped exactly like
// them: SDK-free structs, one interface, a recorded fixture, real pagination
// behaviour, and a collector that degrades instead of breaking. Nothing here
// writes; `DescribeValidDBInstanceModifications` and `DescribeEvents` are both
// read-only operations, and TestNoMutatingAPISurface still holds.
//
// The envelope is collected ONCE, into an immutable snapshot, and the parity
// arithmetic then runs pure. That ordering is not incidental: [StorageParity]
// is a pure interface with no context and no error, because a decision path
// that can perform I/O is a decision path that can fail halfway through a
// report.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// --- The seam --------------------------------------------------------------

// ValidStorageOptionRecord is one entry of the `ValidStorageOptions` list
// `rds:DescribeValidDBInstanceModifications` returns, reduced to the fields a
// storage-performance decision reads.
//
// AWS returns ranges; this reduces each to its overall minimum and maximum,
// which is the only shape a proposal can be checked against. A record whose
// ranges are empty is an UNKNOWN envelope, not a zero one — see
// [StorageEnvelope.Known].
type ValidStorageOptionRecord struct {
	StorageType string `json:"storageType"`
	// MinIOPS and MaxIOPS bound `--iops`.
	MinIOPS int32 `json:"minIOPS,omitempty"`
	MaxIOPS int32 `json:"maxIOPS,omitempty"`
	// MinStorageThroughputMBps and MaxStorageThroughputMBps bound
	// `--storage-throughput`.
	MinStorageThroughputMBps int32 `json:"minStorageThroughputMBps,omitempty"`
	MaxStorageThroughputMBps int32 `json:"maxStorageThroughputMBps,omitempty"`
	// MinAllocatedStorageGiB and MaxAllocatedStorageGiB are carried for
	// completeness and are never used to propose a size: allocated storage is
	// a one-way ratchet (trap 8) and this package does not name it.
	MinAllocatedStorageGiB int64 `json:"minAllocatedStorageGiB,omitempty"`
	MaxAllocatedStorageGiB int64 `json:"maxAllocatedStorageGiB,omitempty"`
}

// DescribeValidDBInstanceModificationsInput asks for one instance's envelope.
type DescribeValidDBInstanceModificationsInput struct {
	DBInstanceIdentifier string `json:"dbInstanceIdentifier"`
}

// DescribeValidDBInstanceModificationsOutput is that instance's envelope.
type DescribeValidDBInstanceModificationsOutput struct {
	ValidStorageOptions []ValidStorageOptionRecord `json:"validStorageOptions,omitempty"`
}

// EventRecord is one `rds:DescribeEvents` entry, reduced to what the
// four-modifications-per-24-hours limit needs.
type EventRecord struct {
	SourceIdentifier string    `json:"sourceIdentifier"`
	SourceType       string    `json:"sourceType,omitempty"`
	Message          string    `json:"message,omitempty"`
	Categories       []string  `json:"categories,omitempty"`
	Date             time.Time `json:"date,omitzero"`
}

// DescribeEventsInput is the paginating event request. StartTime is passed by
// the caller — this package reads no clock.
type DescribeEventsInput struct {
	SourceIdentifier string    `json:"sourceIdentifier,omitempty"`
	SourceType       string    `json:"sourceType,omitempty"`
	StartTime        time.Time `json:"startTime,omitzero"`
	EndTime          time.Time `json:"endTime,omitzero"`
	Marker           string    `json:"marker,omitempty"`
}

// DescribeEventsOutput is one page.
type DescribeEventsOutput struct {
	Events []EventRecord `json:"events,omitempty"`
	Marker string        `json:"marker,omitempty"`
}

// ModificationEnvelopeAPI is U13's read seam. Both operations are read-only,
// and the interface names the whole of this unit's AWS surface: two GETs.
//
// It is a SEPARATE interface from [InventoryAPI] for the same reason
// [CommitmentAPI] is: a caller may hold `rds:DescribeDBInstances` without
// `rds:DescribeValidDBInstanceModifications`, and the correct behaviour then
// is a complete report in which every provisioning proposal is refused by
// name — not a missing report, and never a hardcoded ceiling.
type ModificationEnvelopeAPI interface {
	DescribeValidDBInstanceModifications(ctx context.Context,
		in *DescribeValidDBInstanceModificationsInput) (*DescribeValidDBInstanceModificationsOutput, error)
	DescribeEvents(ctx context.Context, in *DescribeEventsInput) (*DescribeEventsOutput, error)
}

// --- The collected shape ---------------------------------------------------

// StorageEnvelope is one storage type's live provisioning range for one
// instance.
//
// Known is the load-bearing field. A zero-value StorageEnvelope is "we were
// not told", and [GP3Config.Validate] refuses every provisioning
// configuration against it. There is deliberately no "assume the published
// ceiling" path: §2.4 names two published ceilings that contradict each other,
// and picking one would be picking a coin flip and calling it a fact.
type StorageEnvelope struct {
	StorageType            string `json:"storageType"`
	Known                  bool   `json:"known,omitempty"`
	MinIOPS                int32  `json:"minIOPS,omitempty"`
	MaxIOPS                int32  `json:"maxIOPS,omitempty"`
	MinThroughputMBps      int32  `json:"minThroughputMBps,omitempty"`
	MaxThroughputMBps      int32  `json:"maxThroughputMBps,omitempty"`
	MinAllocatedStorageGiB int64  `json:"minAllocatedStorageGiB,omitempty"`
	MaxAllocatedStorageGiB int64  `json:"maxAllocatedStorageGiB,omitempty"`
}

// Describe renders the envelope for a refusal or an evidence line.
func (s StorageEnvelope) Describe() string {
	if !s.Known {
		return "unknown"
	}
	return fmt.Sprintf("%d–%d IOPS, %d–%d MiB/s",
		s.MinIOPS, s.MaxIOPS, s.MinThroughputMBps, s.MaxThroughputMBps)
}

// Envelope is everything the modification seam learned about one DB instance.
//
// It is the typed surface U14 actuates against: the ranges ModifyDBInstance
// will accept, and the modification history that decides whether it will
// accept anything at all right now.
type Envelope struct {
	// Identifier is the DBInstanceIdentifier this envelope describes.
	Identifier string `json:"identifier"`
	// Storage is sorted by storage type, so any rendering of an Envelope is
	// byte-identical run to run.
	Storage []StorageEnvelope `json:"storage,omitempty"`
	// StorageModifications are the times of observed storage modifications,
	// sorted ascending. Empty means "none seen in the window read", which is
	// NOT the same as "none happened" — see [Envelope.HistoryKnown].
	StorageModifications []time.Time `json:"storageModifications,omitempty"`
	// HistoryKnown reports whether the event seam answered at all. False means
	// the cooldown could not be evaluated, and this unit then reports the
	// modification limit as an unverified precondition rather than clearing
	// it silently.
	HistoryKnown bool `json:"historyKnown,omitempty"`
}

// For returns the envelope for a storage type. The zero value it returns for
// an absent type has Known=false, which is the refusing default.
func (e Envelope) For(storageType string) StorageEnvelope {
	want := strings.ToLower(strings.TrimSpace(storageType))
	i := sort.Search(len(e.Storage), func(i int) bool { return e.Storage[i].StorageType >= want })
	if i < len(e.Storage) && e.Storage[i].StorageType == want {
		return e.Storage[i]
	}
	return StorageEnvelope{StorageType: want}
}

// CooldownVerdict is the four-modifications-per-24-hours arithmetic.
//
// "You can perform a maximum of four storage modifications on a DB instance
// within any 24-hour period" [verified]. This is a read-only verdict in U13
// and a hard gate in U14; both read the same function so they cannot disagree.
type CooldownVerdict struct {
	// Known is false when the event history could not be read. An unknown
	// history never clears the cooldown: silence is not evidence of quiet, the
	// same rule U11 applies to every CloudWatch series.
	Known bool `json:"known"`
	// Recent is how many storage modifications fell inside the trailing
	// [StorageModificationWindow].
	Recent int `json:"recent"`
	// Blocked is Recent >= MaxStorageModificationsPer24h.
	Blocked bool `json:"blocked,omitempty"`
	// ClearsAt is when the oldest of those modifications leaves the window,
	// i.e. the earliest moment a fifth would be accepted.
	ClearsAt time.Time `json:"clearsAt,omitzero"`
}

// Cooldown evaluates the modification limit at `now`. The clock is an
// argument: this package reads no clock (TestNoClockReads).
func (e Envelope) Cooldown(now time.Time) CooldownVerdict {
	v := CooldownVerdict{Known: e.HistoryKnown}
	if !v.Known {
		return v
	}
	cut := now.Add(-StorageModificationWindow)
	var oldest time.Time
	for _, at := range e.StorageModifications {
		if at.After(cut) && !at.After(now) {
			v.Recent++
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
	}
	if v.Recent >= MaxStorageModificationsPer24h {
		v.Blocked = true
		v.ClearsAt = oldest.Add(StorageModificationWindow)
	}
	return v
}

// --- The collector ---------------------------------------------------------

// EnvelopeCollectorConfig bounds one envelope collection.
type EnvelopeCollectorConfig struct {
	// Window is the period of event history to read. Callers pass it; the
	// collector reads no clock. A window shorter than
	// [StorageModificationWindow] cannot answer the cooldown question and the
	// collector says so with a warning.
	Window Window
	// MaxPages bounds pagination per instance. Zero means [DefaultMaxPages].
	MaxPages int
}

// EnvelopeCollector reads the modification envelope for a set of instances.
//
// A nil [ModificationEnvelopeAPI] is legal and yields an empty, entirely
// unknown set — every provisioning proposal then refuses with
// [ReasonParityEnvelopeUnknown], which is the intended loud failure mode.
type EnvelopeCollector struct {
	api ModificationEnvelopeAPI
	cfg EnvelopeCollectorConfig
}

// NewEnvelopeCollector builds a collector. api may be nil.
func NewEnvelopeCollector(api ModificationEnvelopeAPI, cfg EnvelopeCollectorConfig) *EnvelopeCollector {
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = DefaultMaxPages
	}
	return &EnvelopeCollector{api: api, cfg: cfg}
}

// Envelopes is an immutable, sorted set of collected envelopes.
//
// It is a slice rather than a map so that anything derived from it — a report
// line, an evidence string, a checksum — is produced in one fixed order and
// never depends on Go's randomized map iteration.
type Envelopes struct {
	items []Envelope
	// Warnings records seams that declined to answer, in a stable order.
	Warnings []string
}

// Len is how many instances were described.
func (e Envelopes) Len() int { return len(e.items) }

// All returns the envelopes in identifier order. The slice is a copy: an
// Envelopes value handed to two goroutines must not be mutable through either.
func (e Envelopes) All() []Envelope { return append([]Envelope(nil), e.items...) }

// Get returns the envelope for an instance identifier, and a wholly unknown
// one when the instance was not described.
func (e Envelopes) Get(identifier string) Envelope {
	id := strings.TrimSpace(identifier)
	i := sort.Search(len(e.items), func(i int) bool { return e.items[i].Identifier >= id })
	if i < len(e.items) && e.items[i].Identifier == id {
		return e.items[i]
	}
	return Envelope{Identifier: id}
}

// NewEnvelopes builds an immutable set from collected envelopes, sorting and
// de-duplicating by identifier. Exported so a caller with its own source —
// a cached file, a test — can build one without the collector.
func NewEnvelopes(in []Envelope, warnings ...string) Envelopes {
	items := make([]Envelope, 0, len(in))
	for _, e := range in {
		e.Identifier = strings.TrimSpace(e.Identifier)
		if e.Identifier == "" {
			continue
		}
		e.Storage = normalizeStorageEnvelopes(e.Storage)
		e.StorageModifications = sortedTimes(e.StorageModifications)
		items = append(items, e)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Identifier < items[j].Identifier })
	// Last write wins on a duplicate identifier, deterministically.
	out := items[:0]
	for i, e := range items {
		if i+1 < len(items) && items[i+1].Identifier == e.Identifier {
			continue
		}
		out = append(out, e)
	}
	return Envelopes{items: out, Warnings: sortWarnings(warnings)}
}

func normalizeStorageEnvelopes(in []StorageEnvelope) []StorageEnvelope {
	out := make([]StorageEnvelope, 0, len(in))
	for _, s := range in {
		s.StorageType = strings.ToLower(strings.TrimSpace(s.StorageType))
		if s.StorageType == "" {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StorageType < out[j].StorageType })
	return out
}

func sortedTimes(in []time.Time) []time.Time {
	if len(in) == 0 {
		return nil
	}
	out := append([]time.Time(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// Collect reads the envelope and the recent storage-modification history for
// each instance, in identifier order.
//
// It never fails the whole collection for one instance: an instance whose
// envelope could not be read gets an unknown envelope and a warning, and every
// provisioning proposal for it is then refused by name. That is the same
// degradation rule U11's optional seams follow.
func (c *EnvelopeCollector) Collect(ctx context.Context, identifiers []string) (Envelopes, error) {
	ids := make([]string, 0, len(identifiers))
	seen := map[string]bool{}
	for _, id := range identifiers {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)

	if c.api == nil {
		return NewEnvelopes(nil, "rds: no DescribeValidDBInstanceModifications seam was supplied; every "+
			"storage-performance proposal is refused because AWS publishes two contradictory gp3 ceilings "+
			"and this package hardcodes neither"), nil
	}

	var (
		out      []Envelope
		warnings []string
	)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return Envelopes{}, err
		}
		env := Envelope{Identifier: id}
		res, err := c.api.DescribeValidDBInstanceModifications(ctx,
			&DescribeValidDBInstanceModificationsInput{DBInstanceIdentifier: id})
		switch {
		case err != nil:
			warnings = append(warnings, fmt.Sprintf(
				"rds: DescribeValidDBInstanceModifications(%s) failed (%v); this instance's provisioning "+
					"envelope is unknown and every provisioning proposal for it is refused", id, err))
		case res != nil:
			env.Storage = envelopesFromRecords(res.ValidStorageOptions)
		}
		mods, ok, warn := c.history(ctx, id)
		env.StorageModifications, env.HistoryKnown = mods, ok
		if warn != "" {
			warnings = append(warnings, warn)
		}
		out = append(out, env)
	}
	if c.cfg.Window.Duration() > 0 && c.cfg.Window.Duration() < StorageModificationWindow {
		warnings = append(warnings, fmt.Sprintf(
			"rds: the event window is %s, shorter than the %s storage-modification period; the "+
				"four-modifications-per-24-hours limit cannot be ruled out from it",
			c.cfg.Window.String(), StorageModificationWindow))
	}
	return NewEnvelopes(out, warnings...), nil
}

// history reads one instance's storage-modification events.
func (c *EnvelopeCollector) history(ctx context.Context, id string) ([]time.Time, bool, string) {
	var (
		out   []time.Time
		token string
	)
	for page := 0; page < c.cfg.MaxPages; page++ {
		res, err := c.api.DescribeEvents(ctx, &DescribeEventsInput{
			SourceIdentifier: id, SourceType: EventSourceDBInstance,
			StartTime: c.cfg.Window.Start, EndTime: c.cfg.Window.End, Marker: token,
		})
		if err != nil {
			return nil, false, fmt.Sprintf(
				"rds: DescribeEvents(%s) failed (%v); the storage-modification history is unknown, so the "+
					"four-per-24-hours limit is reported as unverified rather than cleared", id, err)
		}
		if res == nil {
			break
		}
		for _, ev := range res.Events {
			if IsStorageModificationEvent(ev) {
				out = append(out, ev.Date)
			}
		}
		if res.Marker == "" {
			break
		}
		token = res.Marker
	}
	return sortedTimes(out), true, ""
}

// EventSourceDBInstance is the `SourceType` a DB instance's events carry.
const EventSourceDBInstance = "db-instance"

// EventCategoryConfigurationChange is the category AWS files storage
// modifications under.
const EventCategoryConfigurationChange = "configuration change"

// IsStorageModificationEvent reports whether an event is one of the four a DB
// instance is allowed per 24 hours.
//
// The match is deliberately BROAD: over-counting delays a proposal by hours,
// under-counting proposes a change AWS will reject. Of the two errors only one
// is visible to the operator as a failure, so the recogniser errs toward
// counting.
func IsStorageModificationEvent(ev EventRecord) bool {
	msg := strings.ToLower(ev.Message)
	if !strings.Contains(msg, "storage") && !strings.Contains(msg, "iops") &&
		!strings.Contains(msg, "throughput") {
		return false
	}
	for _, c := range ev.Categories {
		if strings.EqualFold(strings.TrimSpace(c), EventCategoryConfigurationChange) {
			return true
		}
	}
	// A message that names storage and a modification is counted even when the
	// category list is absent: some recorded events carry no categories, and a
	// missing category must not read as "this was not a modification".
	return strings.Contains(msg, "modif") || strings.Contains(msg, "applying") ||
		strings.Contains(msg, "optimization")
}

func envelopesFromRecords(in []ValidStorageOptionRecord) []StorageEnvelope {
	out := make([]StorageEnvelope, 0, len(in))
	for _, r := range in {
		st := strings.ToLower(strings.TrimSpace(r.StorageType))
		if st == "" {
			continue
		}
		out = append(out, StorageEnvelope{
			StorageType: st, Known: true,
			MinIOPS: r.MinIOPS, MaxIOPS: r.MaxIOPS,
			MinThroughputMBps: r.MinStorageThroughputMBps, MaxThroughputMBps: r.MaxStorageThroughputMBps,
			MinAllocatedStorageGiB: r.MinAllocatedStorageGiB,
			MaxAllocatedStorageGiB: r.MaxAllocatedStorageGiB,
		})
	}
	return normalizeStorageEnvelopes(out)
}

// --- The recorded fixture --------------------------------------------------

// EnvelopeFixture replays recorded answers through [ModificationEnvelopeAPI],
// with real pagination and real per-instance failure, so the seam can be
// exercised end to end without an AWS SDK, a network or a credential.
//
// It is the same shape as [Fixture], and exported for the same reason: the
// seam is the contract, and a contract nobody outside the package can exercise
// is not a contract.
type EnvelopeFixture struct {
	// Options maps a DBInstanceIdentifier to its ValidStorageOptions answer.
	Options map[string][]ValidStorageOptionRecord
	// Events maps a DBInstanceIdentifier to its recorded events.
	Events map[string][]EventRecord
	// PageSize splits every paginated events response; 0 means one page.
	PageSize int

	// OptionsErr fails DescribeValidDBInstanceModifications for one instance.
	OptionsErr map[string]error
	// EventsErr fails DescribeEvents for one instance.
	EventsErr map[string]error

	// Calls counts seam invocations so a test can assert the bounds bound
	// something.
	Calls struct {
		DescribeValidDBInstanceModifications int
		DescribeEvents                       int
	}
}

// DescribeValidDBInstanceModifications implements [ModificationEnvelopeAPI].
func (f *EnvelopeFixture) DescribeValidDBInstanceModifications(ctx context.Context,
	in *DescribeValidDBInstanceModificationsInput) (*DescribeValidDBInstanceModificationsOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.DescribeValidDBInstanceModifications++
	if err := f.OptionsErr[in.DBInstanceIdentifier]; err != nil {
		return nil, err
	}
	return &DescribeValidDBInstanceModificationsOutput{
		ValidStorageOptions: f.Options[in.DBInstanceIdentifier]}, nil
}

// DescribeEvents implements [ModificationEnvelopeAPI].
func (f *EnvelopeFixture) DescribeEvents(ctx context.Context,
	in *DescribeEventsInput) (*DescribeEventsOutput, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.Calls.DescribeEvents++
	if err := f.EventsErr[in.SourceIdentifier]; err != nil {
		return nil, err
	}
	all := f.Events[in.SourceIdentifier]
	start, err := offsetOf(in.Marker)
	if err != nil {
		return nil, err
	}
	end, next := paginate(len(all), start, f.PageSize)
	return &DescribeEventsOutput{Events: all[min(start, len(all)):end], Marker: next}, nil
}
