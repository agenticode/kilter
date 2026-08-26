// Package whatif turns "should we change the policy?" from an argument into a
// measured, gated, approval-bound artifact.
//
// Design reference: docs/design/reasoning-engine.md §4.5 (what-if), §4.6
// (closed-loop policy tuning) and §9 unit 5. Three things live here:
//
//   - What-if (whatif.go): replay recorded history against a candidate policy
//     through the REAL pkg/backtest paths and report the delta against the
//     incumbent. No scoring logic is reimplemented; a second implementation of
//     the measuring instrument would measure a policy that never runs.
//   - The proposal object, its store and its gate (proposal.go, store.go,
//     gate.go, approval.go): an inert record of "this policy scored better,
//     here is the evidence", plus the state machine that is the only path from
//     that record to something an operator may act on (INV-4).
//   - The nightly tuner (tuner.go): a bounded grid search, OFF BY DEFAULT,
//     whose only output is proposals. It cannot apply anything.
//
// Four properties are load-bearing, and each has tests that fail loudly if it
// is broken:
//
//   - Nothing here applies a change. There is no executor, no client, no
//     writer of cluster or config state. The terminal state a proposal can
//     reach inside this package is "approved"; StateApplied is recorded by
//     whatever actually did the work, after the fact.
//   - Self-approval is unrepresentable. Approval carries no exported fields,
//     so the only way to obtain one is (*Approver).Approve, and Approver
//     cannot be constructed by a non-human actor or used on its own author's
//     proposal. See approval.go.
//   - Independence. Both sides of every comparison are scored by pkg/backtest
//     against its policy-independent oracle, under an identical yardstick that
//     backtest.Gate re-checks field by field. Nothing in this package computes
//     a quality number of its own.
//   - Determinism. Same history plus same candidate yields a byte-identical
//     proposal. Every enumeration is sorted, every money total goes through
//     sumUSD, and no pure path calls time.Now — the clock is an argument.
//
// Dependency direction stays downward (§8): this package imports backtest,
// recommend, plan, decision, evidence, pricing and model, and nothing imports
// it yet.
package whatif

import (
	"fmt"
	"math"
	"time"

	"github.com/agenticode/kilter/pkg/backtest"
	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

// Policy is the config triple that defines what the engine would decide: the
// exact three structs pkg/api's brain runs with. It is deliberately the same
// grouping backtest.PolicyHash fingerprints, so a Policy and its scorecard
// can never drift apart.
type Policy struct {
	Rec      recommend.Config `json:"recommend"`
	Plan     plan.Config      `json:"plan"`
	Decision decision.Config  `json:"decision"`
}

// DefaultPolicy is the shipped production policy — the incumbent a candidate
// is measured against when the caller does not name one.
func DefaultPolicy() Policy {
	return Policy{
		Rec:      recommend.DefaultConfig(),
		Plan:     plan.DefaultConfig(),
		Decision: decision.DefaultConfig(),
	}
}

// Hash fingerprints the policy. It delegates to backtest.PolicyHash rather
// than hashing the structs here, so a policy's identity in a proposal is the
// same string that appears in Scorecard.Policy. If those two ever disagreed,
// a proposal could carry a scorecard for a different policy than the one it
// proposes — which is the single worst failure this package could have.
func (p Policy) Hash() string {
	return backtest.PolicyHash(p.Rec, p.Plan, p.Decision)
}

// IsZero reports whether the policy is entirely unset, which callers read as
// "use the default" the same way backtest.Harness does.
func (p Policy) IsZero() bool {
	return p.Rec == (recommend.Config{}) &&
		p.Plan == (plan.Config{}) &&
		p.Decision == (decision.Config{})
}

// withDefaults fills a zero triple field-group by field-group, matching
// backtest.Harness.run's own defaulting exactly. Doing it here as well means
// the Policy recorded in a proposal is the one that was scored, not a zero
// struct the harness silently expanded.
func (p Policy) withDefaults() Policy {
	if p.Rec == (recommend.Config{}) {
		p.Rec = recommend.DefaultConfig()
	}
	if p.Plan == (plan.Config{}) {
		p.Plan = plan.DefaultConfig()
	}
	if p.Decision == (decision.Config{}) {
		p.Decision = decision.DefaultConfig()
	}
	return p
}

// Validate rejects a policy the engine could not run. recommend.New and
// decision.Config.Validate are the shipped validators; using them (instead of
// a copy of their rules) means an invalid candidate fails here for exactly the
// reason it would fail at brain start. plan.Config has no exported validator,
// so the fields this package can move are checked explicitly.
func (p Policy) Validate() error {
	p = p.withDefaults()
	if _, err := recommend.New(p.Rec); err != nil {
		return fmt.Errorf("whatif: invalid recommend policy: %w", err)
	}
	if err := p.Decision.Validate(); err != nil {
		return fmt.Errorf("whatif: invalid decision policy: %w", err)
	}
	if !(p.Plan.MinConfidence >= 0) || p.Plan.MinConfidence > 1 {
		return fmt.Errorf("whatif: plan MinConfidence %v out of [0,1]", p.Plan.MinConfidence)
	}
	if !(p.Plan.MinNodeUtilization >= 0) || p.Plan.MinNodeUtilization > 1 {
		return fmt.Errorf("whatif: plan MinNodeUtilization %v out of [0,1]", p.Plan.MinNodeUtilization)
	}
	if !(p.Plan.MinClusterHeadroom >= 0) || p.Plan.MinClusterHeadroom > 1 {
		return fmt.Errorf("whatif: plan MinClusterHeadroom %v out of [0,1]", p.Plan.MinClusterHeadroom)
	}
	if p.Plan.MaxNodeRemovals < 0 {
		return fmt.Errorf("whatif: plan MaxNodeRemovals %d must be >= 0", p.Plan.MaxNodeRemovals)
	}
	return nil
}

// ---- the tunable axes ----

// Axis names the knobs this package is allowed to move. The list is closed on
// purpose: §4.6 bounds the search to "percentile ±2pts, headroom ±5%, soak
// ±2h", and an open-ended search over every config field would be a different,
// far riskier feature wearing this one's name.
//
// Adding an axis is a deliberate act: it needs a hard bound in hardEnvelope, a
// projection in (Axis).get/set, and an entry in AllAxes. The fuzz test walks
// AllAxes, so a new axis with no bound fails the build's tests immediately.
type Axis string

const (
	AxisCPUPercentile    Axis = "cpu-percentile"
	AxisMemoryPercentile Axis = "memory-percentile"
	AxisCPUHeadroom      Axis = "cpu-headroom"
	AxisMemoryHeadroom   Axis = "memory-headroom"
	AxisBaseSoak         Axis = "base-soak"
)

// AllAxes is the enumeration order every grid, report and encoding uses.
// Sorted by construction (it is a fixed literal), so no map iteration can
// reorder a proposal's contents.
var AllAxes = []Axis{
	AxisCPUPercentile,
	AxisMemoryPercentile,
	AxisCPUHeadroom,
	AxisMemoryHeadroom,
	AxisBaseSoak,
}

// isSoak reports whether the axis is expressed in time rather than a ratio.
// Durations are carried through the envelope as float64 hours so one Range
// type covers every axis; the conversion happens only here and in set/get.
func (a Axis) isSoak() bool { return a == AxisBaseSoak }

// Known reports whether the axis is one this package can move. Unknown axes
// are rejected rather than ignored: silently dropping a knob the caller asked
// to tune would produce a proposal that does not do what its rationale says.
func (a Axis) Known() bool {
	for _, k := range AllAxes {
		if k == a {
			return true
		}
	}
	return false
}

// get projects the axis out of a policy, in envelope units (ratio, or hours
// for the soak).
func (a Axis) get(p Policy) float64 {
	switch a {
	case AxisCPUPercentile:
		return p.Rec.CPUPercentile
	case AxisMemoryPercentile:
		return p.Rec.MemoryPercentile
	case AxisCPUHeadroom:
		return p.Rec.CPUHeadroom
	case AxisMemoryHeadroom:
		return p.Rec.MemoryHeadroom
	case AxisBaseSoak:
		return p.Decision.BaseSoak.Hours()
	}
	return math.NaN()
}

// set writes the axis back into a policy. Soak hours are quantized to whole
// minutes so a float round-trip cannot produce a duration whose string form
// differs run to run.
func (a Axis) set(p Policy, v float64) Policy {
	switch a {
	case AxisCPUPercentile:
		p.Rec.CPUPercentile = v
	case AxisMemoryPercentile:
		p.Rec.MemoryPercentile = v
	case AxisCPUHeadroom:
		p.Rec.CPUHeadroom = v
	case AxisMemoryHeadroom:
		p.Rec.MemoryHeadroom = v
	case AxisBaseSoak:
		p.Decision.BaseSoak = hoursToDuration(v)
	}
	return p
}

// hoursToDuration converts envelope hours to a duration quantized to whole
// minutes, clamped to a non-negative, finite range.
func hoursToDuration(h float64) time.Duration {
	if math.IsNaN(h) || h <= 0 {
		return 0
	}
	if h > maxSoakHours {
		h = maxSoakHours
	}
	return time.Duration(math.Round(h*60)) * time.Minute
}

// maxSoakHours is the absolute ceiling on the post-change soak axis. Three
// days without a decision is already an engine that has stopped working; the
// bound also keeps the hours→duration arithmetic far from overflow.
const maxSoakHours = 72
