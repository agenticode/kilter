package whatif

import "time"

// Clock is the only way this package learns the time. It is an argument, never
// an ambient call: a proposal that embedded time.Now() would be a different
// artifact on every run over identical history, and the determinism tests
// (and any future golden file) could not exist.
//
// The nil Clock is not defaulted to time.Now anywhere. A caller that forgets
// to supply one gets an error, not a silently non-reproducible proposal.
type Clock func() time.Time

// FixedClock returns a Clock that always reports t — the form tests and
// replays use.
func FixedClock(t time.Time) Clock { return func() time.Time { return t } }

// now reads the clock and normalizes to UTC. A zero or nil clock is reported
// to the caller by the validation at each entry point, not papered over here.
func (c Clock) now() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c().UTC()
}
