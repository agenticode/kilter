package rds

// The step ledger: what was attempted, what was sent, and what is still
// running.
//
// The ledger is not bookkeeping. It is the thing that makes a controller
// restart safe: an entry that is neither terminal nor settled names a database
// that may have a storage modification in flight right now with nobody
// watching it, and [Actuator.Unsettled] is the list a controller works through
// on startup. Every status change goes through [ActuatorConfig.Persist] where
// one is wired, and it is called BEFORE the mutating call, never after —
// the crash window then contains "we may have modified it" rather than "we
// definitely modified it and nobody knows".

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

func (a *Actuator) entry(key string) (LedgerEntry, bool) {
	if key == "" {
		return LedgerEntry{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return LedgerEntry{}, false
	}
	return *e, true
}

// Entry returns one step's ledger entry.
func (a *Actuator) Entry(key string) (LedgerEntry, bool) { return a.entry(key) }

// upsert returns the entry for a step, creating it on first sight. Caller
// holds a.mu.
func (a *Actuator) upsert(step domain.Step, as ApprovedStep, now time.Time) *LedgerEntry {
	key := step.Key
	if key == "" {
		key = domain.StepKey(step.Target, step.From, step.To)
	}
	e, ok := a.ledger[key]
	if !ok {
		e = &LedgerEntry{
			Key: key, Target: step.Target, Action: step.Action,
			From: step.From, To: step.To, StartedAt: now,
			Revert: as.undo, Origin: as.origin,
			Fingerprint: as.approval.token.Fingerprint,
			ApprovedBy:  as.approval.token.ApprovedBy,
		}
		// The claim is read from the step, never computed here. A second
		// arithmetic would be a second source of truth for the bill.
		if raw := strings.TrimSpace(step.To.Attr(AttrNetSavingsMonthlyUSD)); raw != "" {
			if v := floatOrNaN(raw); v == v {
				e.ClaimedMonthlyUSD, e.Claimed = v, true
			}
		}
		a.ledger[key] = e
		a.order = append(a.order, key)
	}
	return e
}

// record writes (or updates) the entry for a step.
func (a *Actuator) record(step domain.Step, as ApprovedStep, now time.Time, status, detail string, err error) {
	a.recordCall(step, as, now, status, "", detail, ModifyStorageInput{}, err)
}

// recordCall records a status together with the exact call the step would
// make. Dry-run uses it, which is what makes a dry-run a preview of a specific
// API call rather than a promise about one.
func (a *Actuator) recordCall(step domain.Step, as ApprovedStep, now time.Time,
	status string, stage Stage, detail string, call ModifyStorageInput, err error) {

	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.upsert(step, as, now)
	e.Mode = a.cfg.Mode
	e.Status = status
	if stage != "" {
		e.Stage = stage
	}
	if detail != "" {
		e.Detail = detail
	}
	if call.DBInstanceIdentifier != "" {
		e.Sent = call
	}
	if err != nil {
		e.Error = err.Error()
		e.RefusalCode = RefusalCode(err)
		e.ValidFrom = RefusalValidFrom(err)
	} else {
		e.Error, e.RefusalCode, e.ValidFrom = "", "", time.Time{}
	}
	if status != StatusInFlight {
		e.FinishedAt = now
	}
}

// mutate is the persist-before-act barrier.
//
// It records the intent AND the exact call, flushes the ledger through
// [ActuatorConfig.Persist], and only then lets the caller issue it. A Persist
// failure ABORTS the mutation: a storage modification nobody wrote down is
// precisely the state this unit must never reach, so failing to record is
// failing to act.
func (a *Actuator) mutate(ctx context.Context, step domain.Step, as ApprovedStep,
	now time.Time, stage Stage, detail string, call ModifyStorageInput) error {

	a.mu.Lock()
	e := a.upsert(step, as, now)
	e.Mode = a.cfg.Mode
	e.Status = StatusInFlight
	e.Stage = stage
	e.Detail = detail
	e.Sent = call
	e.Attempts++
	attempts := e.Attempts
	a.mu.Unlock()
	if attempts > 1 {
		// Not fatal, but it must be visible: a second mutating call for one
		// step spends a second of the four modifications this instance gets
		// in 24 hours.
		a.cfg.Logger.Warn("rds storage modification retried",
			"instance", step.Target.ID, "attempts", attempts, "key", step.Key)
	}
	return a.persist(ctx)
}

// persist flushes the ledger when a hook is wired.
func (a *Actuator) persist(ctx context.Context) error {
	if a.cfg.Persist == nil {
		return nil
	}
	b, err := a.LedgerJSON()
	if err != nil {
		return fmt.Errorf("rds: serialize ledger: %w", err)
	}
	if err := a.cfg.Persist(ctx, b); err != nil {
		return fmt.Errorf("rds: persist ledger before modifying: %w", err)
	}
	return nil
}

// markIssued records the moment RDS accepted the modification. That instant
// starts this instance's 24-hour window, so it is stored rather than derived.
func (a *Actuator) markIssued(key string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[key]; ok && e.IssuedAt.IsZero() {
		e.IssuedAt = at
	}
}

func (a *Actuator) setDetail(key, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[key]; ok {
		e.Detail = detail
	}
}

func (a *Actuator) addPolls(key string, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.ledger[key]; ok {
		e.Polls += n
	}
}

// finish closes an entry out.
func (a *Actuator) finish(key, status string, stage Stage, now time.Time, detail string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.ledger[key]
	if !ok {
		return
	}
	e.Status = status
	if stage != "" {
		e.Stage = stage
	}
	if detail != "" {
		e.Detail = detail
	}
	e.FinishedAt = now
	if err != nil {
		e.Error = err.Error()
		e.RefusalCode = RefusalCode(err)
		e.ValidFrom = RefusalValidFrom(err)
	} else {
		e.Error, e.RefusalCode, e.ValidFrom = "", "", time.Time{}
	}
}

// Ledger returns every recorded entry, ordered by first sight — deterministic
// for a given step sequence and independent of map iteration.
func (a *Actuator) Ledger() []LedgerEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]LedgerEntry, 0, len(a.order))
	for _, k := range a.order {
		if e, ok := a.ledger[k]; ok {
			out = append(out, *e)
		}
	}
	return out
}

// LedgerJSON serializes the ledger for pkg/store. Entries are emitted in key
// order so the bytes are stable across processes.
func (a *Actuator) LedgerJSON() ([]byte, error) {
	entries := a.Ledger()
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return json.Marshal(entries)
}

// RestoreLedger reloads a serialized ledger, so a restarted controller knows
// which steps it already finished — and, more importantly, which ones it
// started and has not.
func (a *Actuator) RestoreLedger(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var entries []LedgerEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("rds: restore ledger: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ledger = make(map[string]*LedgerEntry, len(entries))
	a.order = a.order[:0]
	for i := range entries {
		e := entries[i]
		if e.Key == "" {
			continue
		}
		if _, dup := a.ledger[e.Key]; dup {
			continue
		}
		a.ledger[e.Key] = &e
		a.order = append(a.order, e.Key)
	}
	return nil
}

// Unsettled returns the entries describing work that is neither finished nor
// safely at rest, in key order.
//
// A controller calls this on startup. Every key it returns is a database that
// may have a storage modification running right now, and re-executing its step
// re-observes AWS rather than re-issuing anything: [Actuator.execute] derives
// the stage from a live read, finds StageAccepted / StageModifying /
// StageOptimizing, and resumes the observation.
func (a *Actuator) Unsettled() []LedgerEntry {
	entries := a.Ledger()
	out := make([]LedgerEntry, 0, len(entries))
	for _, e := range entries {
		if !e.Settled() {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// --- aggregates -------------------------------------------------------------

// LedgerSummary is the roll-up a report renders. Every field is produced from
// a sorted input, so the same entries in a different order give byte-identical
// output — TestActuateLedgerSummaryIsShuffleInvariant permutes and compares.
type LedgerSummary struct {
	Entries int `json:"entries"`
	// ByStatus and ByRefusal are ordered by descending count, then by code.
	ByStatus  []domain.CodeCount `json:"byStatus,omitempty"`
	ByRefusal []domain.CodeCount `json:"byRefusal,omitempty"`
	// ClaimedMonthlyUSD is the sum of the attested savings of the entries
	// that COMPLETED. It is summed through [SumUSD] — sorted by name, then
	// added — for the same reason U13 sums a bill that way: floating-point
	// addition is not associative, and a total that depends on arrival order
	// is a total that changes between two runs over the same data.
	ClaimedMonthlyUSD float64 `json:"claimedMonthlyUSD,omitempty"`
	// Unclaimed counts completed entries carrying no attestation, so a total
	// can never quietly mean "everything".
	Unclaimed int `json:"unclaimed,omitempty"`
	// InFlight is the number an operator must never see stuck: modifications
	// issued and not observed to completion.
	InFlight int `json:"inFlight,omitempty"`
	// NextClears is the earliest moment a dated refusal lapses, so a
	// scheduler has one number to sleep until.
	NextClears time.Time `json:"nextClears,omitzero"`
}

// Summarize rolls a ledger up deterministically.
func Summarize(entries []LedgerEntry) LedgerSummary {
	sorted := make([]LedgerEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	out := LedgerSummary{Entries: len(sorted)}
	statuses := make([]string, 0, len(sorted))
	refusals := make([]string, 0, len(sorted))
	parts := make([]CostPart, 0, len(sorted))
	for _, e := range sorted {
		if e.Status != "" {
			statuses = append(statuses, e.Status)
		}
		if e.RefusalCode != "" {
			refusals = append(refusals, e.RefusalCode)
		}
		if e.Status == StatusInFlight {
			out.InFlight++
		}
		if e.Status == StatusDone {
			if e.Claimed {
				parts = append(parts, CostPart{Name: e.Key, USD: e.ClaimedMonthlyUSD})
			} else {
				out.Unclaimed++
			}
		}
		if !e.ValidFrom.IsZero() && (out.NextClears.IsZero() || e.ValidFrom.Before(out.NextClears)) {
			out.NextClears = e.ValidFrom
		}
	}
	out.ClaimedMonthlyUSD = SumUSD(parts)
	out.ByStatus = tallyCodes(statuses)
	out.ByRefusal = tallyCodes(refusals)
	return out
}

// tallyCodes counts codes into a canonically ordered slice: descending count,
// then code. It never ranges over a map on an output path without sorting
// after.
func tallyCodes(codes []string) []domain.CodeCount {
	if len(codes) == 0 {
		return nil
	}
	counts := make(map[string]int, len(codes))
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			counts[c]++
		}
	}
	out := make([]domain.CodeCount, 0, len(counts))
	for c, n := range counts {
		out = append(out, domain.CodeCount{Code: c, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// --- the registrable form ---------------------------------------------------

// BoundActuator is an [Actuator] with an approval already attached. It is the
// ONLY form that satisfies [domain.Actuator], and therefore the only form
// [domain.Registry.RegisterActuator] will take.
//
// That is the structural half of the approval gate seen from the wiring side:
// cmd/ cannot register a bare actuator and let the registry drive it, because
// a bare actuator has no Execute(ctx, Step) method to satisfy the interface.
// It must first obtain an approval — which requires a token, which requires a
// human — and bind it to a specific plan fingerprint.
type BoundActuator struct {
	a  *Actuator
	ap Approval
}

// Bind attaches an approval, producing the registrable form.
func (a *Actuator) Bind(ap Approval) (*BoundActuator, error) {
	if !ap.Valid() {
		return nil, fmt.Errorf("%w: Bind needs an approval from NewApproval", ErrNotApproved)
	}
	return &BoundActuator{a: a, ap: ap}, nil
}

// Domain implements domain.Actuator.
func (b *BoundActuator) Domain() domain.Kind { return Kind }

// Fingerprint is the plan this actuator is bound to.
func (b *BoundActuator) Fingerprint() string { return b.ap.Fingerprint() }

// Execute implements domain.Actuator. A step the bound approval does not cover
// is refused here, so binding one plan does not authorize another.
func (b *BoundActuator) Execute(ctx context.Context, step domain.Step) error {
	as, err := b.ap.Authorize(step)
	if err != nil {
		b.a.record(step, ApprovedStep{}, b.a.cfg.Now(), StatusRefused, "", err)
		return err
	}
	return b.a.Execute(ctx, as)
}

// Revert implements domain.Actuator.
func (b *BoundActuator) Revert(ctx context.Context, step domain.Step) error {
	as, err := b.ap.Authorize(step)
	if err != nil {
		return err
	}
	return b.a.Revert(ctx, as)
}

// Ledger exposes the underlying actuator's ledger.
func (b *BoundActuator) Ledger() []LedgerEntry { return b.a.Ledger() }

// LedgerSummary rolls the underlying ledger up.
func (b *BoundActuator) LedgerSummary() LedgerSummary { return Summarize(b.a.Ledger()) }

// floatOrNaN parses a money attribute, returning NaN for anything unusable —
// including a non-finite literal — so a garbage value can never pass for zero.
// "No claim" and "claims exactly $0" are different statements and only one of
// them is a bug.
func floatOrNaN(raw string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return math.NaN()
	}
	return v
}

// That *BoundActuator satisfies domain.Actuator — and *Actuator does not — is
// asserted in actuate_test.go rather than with the usual
// `var _ domain.Actuator = ...` line, because TestNoUnexpectedPackageState
// forbids package-level vars in this package, including blank ones.
