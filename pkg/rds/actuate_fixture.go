package rds

// StorageActuateFixture: a fake RDS account, made of maps.
//
// It exists so that EVERY test in this unit — including the ones that "modify"
// a database — runs with no SDK, no socket, no credential and no ~/.aws. That
// is a hard rule for this package (surface_test.go's TestNoForeignImports is
// the structural half), and a fixture is what makes it a rule people can
// follow rather than one they route around.
//
// It models the three RDS behaviours that make this actuator's shape
// necessary, because a fixture that returns success immediately would let
// every resumability and idempotency test pass without proving anything:
//
//  1. **Asynchrony.** A modification does not take effect on the call. The
//     instance walks available → modifying → storage-optimization → available
//     across subsequent describes, with the new values visible in
//     PendingModifiedValues first and in the top-level fields later.
//  2. **The four-per-24-hours limit.** Every accepted modification appends a
//     `DescribeEvents` record, so a fixture-backed test can drive an instance
//     into its own cooldown the way a real account would.
//  3. **Lost responses.** [StorageActuateFixture.FailAfter] fails a call AFTER
//     its effect has landed, which is the crash window the idempotency and
//     resume paths exist for.
//
// It is exported for the same reason [EnvelopeFixture] and [Fixture] are: the
// seam is the contract, and a contract nobody outside the package can exercise
// is not a contract.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Operation names the fixture counts. The VALUES name the real AWS operations
// the adapter calls, which is the honest mapping — see actuate_api.go for why
// the Go identifiers cannot.
const (
	OpDescribeInstanceState = "DescribeDBInstances"
	OpModifyStorage         = "ModifyDBInstance"
)

// modPhase is how far a fixture-side modification has walked.
type modPhase int

const (
	phaseNone modPhase = iota
	phaseAccepted
	phaseModifying
	phaseOptimizing
)

// StorageActuateFixture is a fake RDS account.
type StorageActuateFixture struct {
	// Now is the fixture's clock. Required: an accepted modification is
	// stamped with it and becomes part of the cooldown history.
	Now func() time.Time

	// SettleAfter is how many describes the instance spends in EACH transient
	// phase (accepted → modifying → storage-optimization) before advancing.
	// Zero advances on every describe, which is the fast path most tests want;
	// a larger value is how a test drives the poll budget to expiry.
	SettleAfter int

	// Envelope is the answer DescribeValidDBInstanceModifications gives, per
	// instance identifier. An identifier with no entry answers with no
	// storage options at all, which is an UNKNOWN envelope and a refusal.
	Envelope map[string][]ValidStorageOptionRecord
	// Events seeds the modification history. Modifications this fixture
	// accepts are appended to it.
	Events map[string][]EventRecord
	// PageSize splits every paginated events response; 0 means one page.
	PageSize int

	// Fail is consulted BEFORE an operation's effect; a non-nil return fails
	// the call and changes nothing.
	Fail func(op string, n int) error
	// FailAfter is consulted AFTER the effect has landed — the lost-response
	// case, which is the only interesting failure for an actuator.
	FailAfter func(op string, n int) error
	// EnvelopeErr and EventsErr fail the two read seams per instance.
	EnvelopeErr map[string]error
	EventsErr   map[string]error

	mu     sync.Mutex
	insts  map[string]*InstanceStateRecord
	phase  map[string]modPhase
	ticks  map[string]int
	tokens map[string]bool
	counts map[string]int
	ops    []string
}

// NewStorageActuateFixture builds a fixture from a set of instances. The
// records are copied, so a caller's slice cannot mutate the account behind the
// test's back.
func NewStorageActuateFixture(now func() time.Time, insts ...InstanceStateRecord) *StorageActuateFixture {
	f := &StorageActuateFixture{
		Now:      now,
		Envelope: map[string][]ValidStorageOptionRecord{},
		Events:   map[string][]EventRecord{},
		insts:    map[string]*InstanceStateRecord{},
		phase:    map[string]modPhase{},
		ticks:    map[string]int{},
		tokens:   map[string]bool{},
		counts:   map[string]int{},
	}
	for i := range insts {
		cp := insts[i]
		if cp.Tags == nil {
			cp.Tags = map[string]string{}
		}
		f.insts[cp.Identifier] = &cp
	}
	return f
}

// WithEnvelope sets one instance's provisioning envelope.
func (f *StorageActuateFixture) WithEnvelope(id string, recs ...ValidStorageOptionRecord) *StorageActuateFixture {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Envelope[id] = append([]ValidStorageOptionRecord(nil), recs...)
	return f
}

// WithEvents seeds one instance's modification history.
func (f *StorageActuateFixture) WithEvents(id string, evs ...EventRecord) *StorageActuateFixture {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Events[id] = append([]EventRecord(nil), evs...)
	return f
}

// Instance returns a copy of one instance's live state.
func (f *StorageActuateFixture) Instance(id string) (InstanceStateRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.insts[id]
	if !ok {
		return InstanceStateRecord{}, false
	}
	cp := *d
	return cp, true
}

// SetInstance overwrites one instance's live state — how a test makes somebody
// else change a database mid-plan.
func (f *StorageActuateFixture) SetInstance(r InstanceStateRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := r
	f.insts[r.Identifier] = &cp
}

// Remove deletes an instance, so a test can make one vanish mid-modification.
func (f *StorageActuateFixture) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.insts, id)
}

// Count returns how many times one operation was called.
func (f *StorageActuateFixture) Count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[op]
}

// Mutations counts the mutating calls this fixture accepted. It is the number
// every idempotency test asserts, because the four-per-24-hours limit makes a
// duplicate modification expensive in a way a duplicate read is not.
func (f *StorageActuateFixture) Mutations() int { return f.Count(OpModifyStorage) }

// Ops returns the operation names in call order.
func (f *StorageActuateFixture) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *StorageActuateFixture) enter(ctx context.Context, op string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.counts[op]++
	n := f.counts[op]
	f.ops = append(f.ops, op)
	fail := f.Fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(op, n); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (f *StorageActuateFixture) leave(op string, n int) error {
	f.mu.Lock()
	after := f.FailAfter
	f.mu.Unlock()
	if after == nil {
		return nil
	}
	return after(op, n)
}

func (f *StorageActuateFixture) now() time.Time {
	if f.Now == nil {
		return time.Time{}
	}
	return f.Now()
}

// --- the read seam ----------------------------------------------------------

// DescribeInstanceState implements [StorageActuateAPI]. It ALSO advances a
// pending modification one phase per SettleAfter+1 describes, which is what
// makes the polling path in actuate.go real rather than decorative.
func (f *StorageActuateFixture) DescribeInstanceState(ctx context.Context,
	in *DescribeInstanceStateInput) (*DescribeInstanceStateOutput, error) {

	n, err := f.enter(ctx, OpDescribeInstanceState)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	rec, ok := f.insts[in.DBInstanceIdentifier]
	if ok {
		f.advance(in.DBInstanceIdentifier, rec)
	}
	var out DescribeInstanceStateOutput
	if ok {
		cp := *rec
		cp.Tags = copyTags(rec.Tags)
		out = DescribeInstanceStateOutput{Instance: cp, Found: true}
	}
	f.mu.Unlock()
	if err := f.leave(OpDescribeInstanceState, n); err != nil {
		return nil, err
	}
	return &out, nil
}

// advance walks one instance's in-flight modification. Caller holds f.mu.
//
// The order of the walk is the RDS one and it matters: the new configuration
// becomes visible in the top-level fields when the instance enters
// storage-optimization, NOT when it returns to available. An actuator that
// waited for `available` before believing the values would be right; one that
// believed them at `modifying` would be wrong. This fixture makes the
// difference observable.
func (f *StorageActuateFixture) advance(id string, rec *InstanceStateRecord) {
	ph := f.phase[id]
	if ph == phaseNone {
		return
	}
	f.ticks[id]++
	if f.ticks[id] <= f.SettleAfter {
		return
	}
	f.ticks[id] = 0
	switch ph {
	case phaseAccepted:
		f.phase[id] = phaseModifying
		rec.Status = StatusModifying
	case phaseModifying:
		f.phase[id] = phaseOptimizing
		rec.Status = StatusStorageOptimization
		// The change lands here.
		if rec.PendingStorageType != "" {
			rec.StorageType = rec.PendingStorageType
		}
		if rec.PendingIOPS > 0 {
			rec.IOPS = rec.PendingIOPS
		}
		if rec.PendingStorageThroughputMBps > 0 {
			rec.StorageThroughputMBps = rec.PendingStorageThroughputMBps
		}
		rec.PendingStorageType, rec.PendingIOPS, rec.PendingStorageThroughputMBps = "", 0, 0
	case phaseOptimizing:
		f.phase[id] = phaseNone
		rec.Status = StatusAvailable
	}
}

// --- the write seam ---------------------------------------------------------

// ModifyStorage implements [StorageActuateAPI].
//
// It enforces the two RDS rules an actuator can get wrong: an instance that is
// not `available` rejects the call, and an instance with four modifications in
// the trailing 24 hours rejects it too. A fixture that accepted everything
// would let a broken pre-flight pass every test in this unit.
func (f *StorageActuateFixture) ModifyStorage(ctx context.Context,
	in *ModifyStorageInput) (*ModifyStorageOutput, error) {

	n, err := f.enter(ctx, OpModifyStorage)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	rec, ok := f.insts[in.DBInstanceIdentifier]
	if !ok {
		f.mu.Unlock()
		return nil, fmt.Errorf("rds fixture: DBInstanceNotFound: %s", in.DBInstanceIdentifier)
	}
	// The client token AWS does not have. Deduplicating on it here is what
	// lets a test prove that a retried step issues at most one EFFECTIVE
	// modification even when the response to the first was lost — see
	// ModifyStorageInput.ClientToken for what the adapter must do with it.
	if in.ClientToken != "" && f.tokens[in.ClientToken] {
		cp := *rec
		f.mu.Unlock()
		if err := f.leave(OpModifyStorage, n); err != nil {
			return nil, err
		}
		return &ModifyStorageOutput{Instance: cp}, nil
	}
	if st := strings.ToLower(strings.TrimSpace(rec.Status)); st != StatusAvailable {
		f.mu.Unlock()
		return nil, fmt.Errorf("rds fixture: InvalidDBInstanceState: %s is %s",
			in.DBInstanceIdentifier, rec.Status)
	}
	at := f.now()
	if f.recentModifications(in.DBInstanceIdentifier, at) >= MaxStorageModificationsPer24h {
		f.mu.Unlock()
		return nil, fmt.Errorf("rds fixture: InvalidDBInstanceState: %s has had %d storage modifications "+
			"in 24 hours", in.DBInstanceIdentifier, MaxStorageModificationsPer24h)
	}
	if in.ClientToken != "" {
		f.tokens[in.ClientToken] = true
	}
	rec.PendingStorageType = in.StorageType
	rec.PendingIOPS = in.IOPS
	rec.PendingStorageThroughputMBps = in.StorageThroughputMBps
	f.phase[in.DBInstanceIdentifier] = phaseAccepted
	f.ticks[in.DBInstanceIdentifier] = 0
	f.Events[in.DBInstanceIdentifier] = append(f.Events[in.DBInstanceIdentifier], EventRecord{
		SourceIdentifier: in.DBInstanceIdentifier,
		SourceType:       EventSourceDBInstance,
		Message:          "Finished applying modification to allocated storage",
		Categories:       []string{EventCategoryConfigurationChange},
		Date:             at,
	})
	cp := *rec
	f.mu.Unlock()
	if err := f.leave(OpModifyStorage, n); err != nil {
		return nil, err
	}
	return &ModifyStorageOutput{Instance: cp}, nil
}

// recentModifications counts storage modifications inside the trailing
// window. Caller holds f.mu.
func (f *StorageActuateFixture) recentModifications(id string, now time.Time) int {
	cut := now.Add(-StorageModificationWindow)
	count := 0
	for _, ev := range f.Events[id] {
		if IsStorageModificationEvent(ev) && ev.Date.After(cut) && !ev.Date.After(now) {
			count++
		}
	}
	return count
}

// --- the envelope seam ------------------------------------------------------

// DescribeValidDBInstanceModifications implements [ModificationEnvelopeAPI].
func (f *StorageActuateFixture) DescribeValidDBInstanceModifications(ctx context.Context,
	in *DescribeValidDBInstanceModificationsInput) (*DescribeValidDBInstanceModificationsOutput, error) {

	if _, err := f.enter(ctx, "DescribeValidDBInstanceModifications"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.EnvelopeErr[in.DBInstanceIdentifier]; err != nil {
		return nil, err
	}
	return &DescribeValidDBInstanceModificationsOutput{
		ValidStorageOptions: append([]ValidStorageOptionRecord(nil), f.Envelope[in.DBInstanceIdentifier]...),
	}, nil
}

// DescribeEvents implements [ModificationEnvelopeAPI], with real pagination.
func (f *StorageActuateFixture) DescribeEvents(ctx context.Context,
	in *DescribeEventsInput) (*DescribeEventsOutput, error) {

	if _, err := f.enter(ctx, "DescribeEvents"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.EventsErr[in.SourceIdentifier]; err != nil {
		return nil, err
	}
	all := append([]EventRecord(nil), f.Events[in.SourceIdentifier]...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date.Before(all[j].Date) })
	start, err := offsetOf(in.Marker)
	if err != nil {
		return nil, err
	}
	end, next := paginate(len(all), start, f.PageSize)
	return &DescribeEventsOutput{Events: all[min(start, len(all)):end], Marker: next}, nil
}
