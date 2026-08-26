package whatif

import (
	"fmt"
	"math"
	"sort"
)

// Range is an inclusive [Min, Max] bound on one axis, in envelope units.
type Range struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Validate rejects a range that is not a usable interval. Positive form
// throughout, so a NaN bound fails rather than disabling the check — a NaN
// Max would make every `v > r.Max` comparison false and silently turn the
// bound into "anything goes", which is precisely the failure mode an envelope
// exists to prevent.
func (r Range) Validate() error {
	if math.IsNaN(r.Min) || math.IsNaN(r.Max) {
		return fmt.Errorf("range [%v,%v] has a NaN bound", r.Min, r.Max)
	}
	if math.IsInf(r.Min, 0) || math.IsInf(r.Max, 0) {
		return fmt.Errorf("range [%v,%v] has an infinite bound", r.Min, r.Max)
	}
	if !(r.Max >= r.Min) {
		return fmt.Errorf("range [%v,%v] is inverted", r.Min, r.Max)
	}
	return nil
}

// Contains reports membership. NaN is never contained.
func (r Range) Contains(v float64) bool { return v >= r.Min && v <= r.Max }

// Clamp pulls a value into the range. A NaN input clamps to Min rather than
// propagating: the caller asked for a value on this axis, and the envelope's
// job is to produce one that is inside it.
func (r Range) Clamp(v float64) float64 {
	if math.IsNaN(v) || v < r.Min {
		return r.Min
	}
	if v > r.Max {
		return r.Max
	}
	return v
}

// within reports whether r is a sub-interval of outer.
func (r Range) within(outer Range) bool {
	return r.Min >= outer.Min && r.Max <= outer.Max
}

func (r Range) String() string { return fmt.Sprintf("[%g,%g]", r.Min, r.Max) }

// Envelope is the declared search space: the hard statement of how far any
// automated proposal is allowed to move the policy, independent of what the
// history says. §4.6's closed loop is safe because the search is bounded, and
// this is the boundary — not a convention, a value that is validated once and
// then enforced on every candidate.
//
// Axes absent from Bounds are pinned: the tuner may not move them at all. An
// empty Envelope therefore searches nothing, which is the correct reading of
// "no envelope was declared".
type Envelope struct {
	// Bounds is keyed by Axis. It is a map for ergonomics at the config
	// boundary; every enumeration of it in this package goes through
	// AllAxes or a sorted key list, so no output can depend on Go's map
	// iteration order.
	Bounds map[Axis]Range `json:"bounds"`
}

// hardEnvelope is the ceiling on what any Envelope may declare. These are not
// tuning defaults — they are the values past which the shape of the decision
// math stops being trustworthy, and no config file, tuner, agent or API caller
// may widen them:
//
//   - percentiles stay strictly inside (0,1] and above the point where the
//     "size for the tail" premise stops holding (a p50 CPU request is not a
//     conservative estimate with a smaller safety margin, it is a different
//     and unstated policy);
//   - headroom multipliers stay at or above 1 (below 1 is deliberate
//     undersizing, which the engine must never propose to itself) and below 2
//     (doubling every request is not rightsizing);
//   - the soak stays inside [0, 72h] — see maxSoakHours.
var hardEnvelope = map[Axis]Range{
	AxisCPUPercentile:    {Min: 0.50, Max: 0.999},
	AxisMemoryPercentile: {Min: 0.90, Max: 0.9999},
	AxisCPUHeadroom:      {Min: 1.00, Max: 2.00},
	AxisMemoryHeadroom:   {Min: 1.00, Max: 2.00},
	AxisBaseSoak:         {Min: 0, Max: maxSoakHours},
}

// HardBounds returns a copy of the absolute limits, so callers (and the CLI's
// help text) can show them without being able to change them.
func HardBounds() map[Axis]Range {
	out := make(map[Axis]Range, len(hardEnvelope))
	for a, r := range hardEnvelope {
		out[a] = r
	}
	return out
}

// DefaultEnvelope is the shipped search space: narrower than the hard bounds
// on every axis, because the tuner should explore a neighbourhood of the
// shipped policy, not the whole of what is merely survivable.
func DefaultEnvelope() Envelope {
	return Envelope{Bounds: map[Axis]Range{
		AxisCPUPercentile:    {Min: 0.80, Max: 0.99},
		AxisMemoryPercentile: {Min: 0.95, Max: 0.999},
		AxisCPUHeadroom:      {Min: 1.05, Max: 1.50},
		AxisMemoryHeadroom:   {Min: 1.05, Max: 1.50},
		AxisBaseSoak:         {Min: 2, Max: 24},
	}}
}

// Axes returns the declared axes in AllAxes order — the enumeration order for
// every grid and every encoding. Unknown axes are not returned (Validate has
// already rejected them, so this only matters for an unvalidated Envelope).
func (e Envelope) Axes() []Axis {
	out := make([]Axis, 0, len(e.Bounds))
	for _, a := range AllAxes {
		if _, ok := e.Bounds[a]; ok {
			out = append(out, a)
		}
	}
	return out
}

// Validate rejects an envelope that is not a usable, in-bounds search space.
// It errors rather than silently clamping: an operator who writes
// `cpu-headroom: [0.5, 3.0]` has asked for something this package will not do,
// and quietly narrowing it to [1.0, 2.0] would leave them believing a search
// ran that never did.
func (e Envelope) Validate() error {
	if len(e.Bounds) == 0 {
		return fmt.Errorf("whatif: envelope declares no axes")
	}
	// Sorted so the first error reported for a multiply-broken envelope is
	// the same one on every run.
	keys := make([]string, 0, len(e.Bounds))
	for a := range e.Bounds {
		keys = append(keys, string(a))
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := Axis(k)
		r := e.Bounds[a]
		if !a.Known() {
			return fmt.Errorf("whatif: envelope names unknown axis %q", k)
		}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("whatif: envelope axis %s: %w", k, err)
		}
		hard := hardEnvelope[a]
		if !r.within(hard) {
			return fmt.Errorf("whatif: envelope axis %s %s exceeds hard bounds %s",
				k, r, hard)
		}
	}
	return nil
}

// Contains reports whether a policy sits inside the envelope on every
// declared axis. It is the predicate the tuner's fuzz test asserts on every
// candidate it can generate, and the predicate the gate re-checks before a
// proposal may be accepted — enforced twice on purpose, because the producer
// and the checker are allowed to be different code with different bugs.
func (e Envelope) Contains(p Policy) bool {
	return len(e.Violations(p)) == 0
}

// Violations lists, in AllAxes order, the axes on which a policy sits outside
// the envelope. Deterministic and human-readable; empty means contained.
func (e Envelope) Violations(p Policy) []string {
	p = p.withDefaults()
	var out []string
	for _, a := range e.Axes() {
		r := e.Bounds[a]
		v := a.get(p)
		if !r.Contains(v) {
			out = append(out, fmt.Sprintf("%s=%s outside %s", a, formatAxis(a, v), r))
		}
	}
	return out
}

// Clamp pulls a policy onto the envelope, axis by axis, leaving undeclared
// axes untouched. Candidate generation clamps; the gate does not — generation
// is allowed to be forgiving about arithmetic that walks off the edge of the
// space, while acceptance must be strict.
func (e Envelope) Clamp(p Policy) Policy {
	p = p.withDefaults()
	for _, a := range e.Axes() {
		p = a.set(p, e.Bounds[a].Clamp(a.get(p)))
	}
	return p
}

// formatAxis renders an axis value for a human, with the unit that axis is
// actually measured in.
func formatAxis(a Axis, v float64) string {
	if a.isSoak() {
		return hoursToDuration(v).String()
	}
	return fmt.Sprintf("%g", v)
}
