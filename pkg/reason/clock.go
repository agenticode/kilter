package reason

import "time"

// Clock is the only way this package learns the time, and it is an argument,
// never an ambient call. The audit trail must be byte-identical for a
// replayed transcript (§5.5/§5.6); one time.Now() anywhere in the loop makes
// that impossible and makes the determinism tests unwritable.
//
// The idiom is pkg/whatif's, deliberately unchanged: a nil Clock is not
// defaulted to time.Now, it is refused at the entry point that needed one.
type Clock func() time.Time

// FixedClock returns a Clock that always reports t — the form tests, replays
// and golden files use.
func FixedClock(t time.Time) Clock { return func() time.Time { return t } }

// StepClock returns a Clock that advances by step on every read, starting at
// start. Loop tests need successive stamps that are still a pure function of
// the transcript: with it, "the audit trail is byte-identical across runs" and
// "records are ordered in time" are both testable at once.
func StepClock(start time.Time, step time.Duration) Clock {
	n := int64(-1)
	return func() time.Time {
		n++
		return start.Add(time.Duration(n) * step)
	}
}

// now reads the clock and normalizes to UTC. A nil clock yields the zero
// time; every entry point validates for one before it gets here.
func (c Clock) now() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c().UTC()
}
