package rds

import (
	"fmt"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// Allocated-storage growth — FINDINGS.md §7.4's "the one genuinely
// time-series-shaped RDS finding", built on the seam that section names:
// [AttributeStorageGrowth], applied across a series of observations instead of
// across a pair.
//
// # What this produces, and what it does not
//
// The product is A REFUSAL WITH A SIZE. "This instance's allocated storage
// grew 240 GiB over 21 days, here is the evidence, and no reduction is
// available." [ReasonGrowthIsNotReclaimable] is that sentence. Trap 8 is
// unchanged and unweakened by any of it: MaxAllocatedStorage is a RATCHET, not
// headroom, so measuring how fast the ratchet turned is not a step toward
// turning it back. There is no such step. [GrowthMeasurement] therefore has no
// field named after a saving, a reduction or a reclaim, and
// TestGrowthNamesNoSaving asserts that by reflection rather than by comment.
//
// # Two samples are not a trend, and this file is mostly about that
//
// Storage autoscaling adds "the greater of 10 GiB, 10 % of the current
// allocation, or the growth predicted for the next 7 hours" [verified, see
// [AutoscalingTrigger]] in DISCRETE STEPS, up to four times in 24 hours. A
// single scale-up inside the observation window is one event; a slope fitted
// through it predicts that the event recurs, which is exactly the claim one
// event cannot support. So the gates below come first and the arithmetic
// second:
//
//   - fewer than [GrowthPolicy.MinObservations] instants ⇒
//     [ReasonGrowthInsufficientObservations], and NOTHING is measured;
//   - a span below [GrowthPolicy.MinSpan] ⇒ [ReasonGrowthSpanTooShort],
//     and nothing is measured;
//   - an allocation that DECREASED, or two contradicting observations at one
//     instant ⇒ [ReasonGrowthHistoryInconsistent] — no RDS API reduces
//     allocated storage, so such a history is two instances under one
//     identifier and is not averaged into a trend;
//   - measured, but the growth arrived in a single step or behind one
//     unobserved gap covering most of the span ⇒ the SIZE is stated and the
//     RATE is withheld with [ReasonGrowthRateNotProjectable].
//
// "Not enough history" and "measured, and flat" are different facts and this
// file makes them structurally different values, not two readings of one zero.
// See [GrowthVerdict.Measurement].
//
// # The retained history is thinned, so instants are not evenly spaced
//
// pkg/store retains at most one snapshot per cadence bucket and prunes by
// count and by bytes, so a history is a sequence of IRREGULAR instants and a
// slope computed as (last−first)/count would be a fiction. Everything here
// works from the actual instants: the span is Last−First and the gaps between
// consecutive observations are reported as [GrowthSampling] rather than
// smoothed into a mean. [GrowthSampling.LargestGapFraction] is the number that
// says how much of the "observed" span nobody actually observed.
//
// # A rate is a claim about the future
//
// [GrowthProjection] carries no dollar field of any kind, its Claimable method
// returns false unconditionally, and nothing in this file writes to
// [Assessment.Proposal]. A growth projection cannot become a cost claim
// through a side door because there is no door: [Report.Validate] gates
// proposals, and this file produces none.

// Growth policy defaults. Each is a POLICY choice, not an AWS fact, and the
// arithmetic behind each is in GROWTH-FINDINGS.md §2.
const (
	// DefaultGrowthMinObservations is the fewest distinct instants this
	// package will state anything from. n observations bound n−1 intervals:
	// 2 gives one interval, which is a difference rather than a trend; 3
	// gives two, where a single anomalous interval is half the evidence and
	// ties cannot be broken; 4 gives three, the smallest n at which repeated
	// behaviour outvotes one anomaly. It is also
	// [AutoscaleMaxModificationsPer24h], the documented ceiling on storage
	// modifications in a day — below four observations a maximally active day
	// cannot even in principle be resolved into separate events.
	DefaultGrowthMinObservations = 4
	// DefaultGrowthMinSpan is the shortest history this package will state
	// anything over. A database's write volume has a WEEKLY shape — this
	// package already encodes that belief in [DefaultMinWindow], documented as
	// deliberately not long enough to contain a weekly batch job — and one
	// weekly cycle is one event, so two are the minimum at which "it happened
	// again" is a statement rather than an assumption. 2 × 7 days. It
	// coincides with [DefaultFullWindow], the span at which this package
	// already attaches no window caveat, so a growth finding never claims more
	// confidence than the metric verdicts printed beside it.
	DefaultGrowthMinSpan = 14 * 24 * time.Hour
	// DefaultGrowthMinSteps is the fewest strictly-increasing transitions
	// before a RATE is projected from them. One step is one event; a rate
	// derived from one event asserts that the event recurs, which is the one
	// thing a single event contains no evidence for. Two is the minimum at
	// which recurrence is observed. The size of the growth is reported either
	// way — it is a measured fact and does not depend on this gate.
	DefaultGrowthMinSteps = 2
	// DefaultGrowthMaxGapFraction bounds how much of the span may sit inside a
	// single unobserved interval before the rate is withheld. At
	// DefaultGrowthMinObservations evenly spaced, the largest gap is span/3 ≈
	// 0.333, so an evenly-sampled history never trips this; it trips exactly
	// when the retained instants are clustered and the "span" is mostly blind.
	// Above half the span the whole growth could have arrived in one interval
	// nobody looked at, and a per-day rate over it is arithmetic rather than
	// evidence.
	DefaultGrowthMaxGapFraction = 0.5
	// DefaultGrowthMaxObservations bounds how much history is read. It is
	// pkg/store's DefaultSnapshotRetention().MaxPerCluster, so this package
	// refuses to hold more than the substrate beneath it retains — the two
	// bounds are stated independently because pkg/rds imports nothing and must
	// not learn pkg/store's number by importing it.
	DefaultGrowthMaxObservations = 768
)

// SourceCheckpointHistory names where a growth observation came from: not one
// CloudWatch call and not one DescribeDBInstances call, but a sequence of them
// persisted across runs. Evidence carrying this source is evidence that
// depends on the caller's persistence being wired — see GROWTH-FINDINGS.md §6.
const SourceCheckpointHistory = "checkpointed-observation-history"

// Growth reason codes. All five are refusals: this domain is modelling the
// instance and declining to state something about it. None is an exclusion.
const (
	// ReasonGrowthInsufficientObservations: the history holds fewer distinct
	// instants than [GrowthPolicy.MinObservations]. Two samples are not a
	// trend and neither are three. NOTHING is measured — the verdict carries
	// no [GrowthMeasurement] at all, so "we have not looked long enough"
	// cannot be misread as "we looked and it is flat".
	ReasonGrowthInsufficientObservations = "storage-growth-insufficient-observations"

	// ReasonGrowthSpanTooShort: enough observations arrived, but they cover
	// less than [GrowthPolicy.MinSpan]. Four snapshots an hour apart describe
	// an hour, and a database's growth shape is weekly. Distinct from
	// [ReasonGrowthInsufficientObservations] because the remedies differ: one
	// needs a denser checkpoint cadence, the other needs time to pass.
	ReasonGrowthSpanTooShort = "storage-growth-span-too-short"

	// ReasonGrowthHistoryInconsistent: the history is not one instance's.
	// Either an allocation DECREASED — which no RDS API can do, so the two
	// observations are a replaced instance reusing an identifier — or one
	// instant carries two different allocations. Averaging such a history into
	// a trend would turn a data fault into a confident number, so the whole
	// history is refused rather than repaired. See [StorageShrankImpossible].
	ReasonGrowthHistoryInconsistent = "storage-growth-history-inconsistent"

	// ReasonGrowthIsNotReclaimable: the finding itself, stated as a refusal
	// because that is what it is. The allocation grew by a measured amount
	// over a measured span, that increase is permanent, and no RDS API and no
	// proposal in this package can return it. "You can't reduce the amount of
	// storage for a DB instance after storage has been allocated" [verified].
	// A reader who takes this line as an opportunity has read it backwards.
	ReasonGrowthIsNotReclaimable = "storage-growth-is-not-reclaimable"

	// ReasonGrowthRateNotProjectable: the growth was measured and sized, and
	// the per-day RATE is withheld. Either every GiB arrived in a single step
	// (fewer than [GrowthPolicy.MinSteps] increases) or more than
	// [GrowthPolicy.MaxGapFraction] of the span sits inside one unobserved
	// interval. A rate is a claim about the future; these histories do not
	// contain one, and the size is reported without it.
	ReasonGrowthRateNotProjectable = "storage-growth-rate-not-projectable"
)

// AdvisoryStorageGrew reports a measured, permanent allocated-storage
// increase and its monthly cost. Like every advisory in this package the
// MonthlyUSD is a MAGNITUDE — here, the recurring cost the fleet acquired
// while nobody was asked — and never a saving.
const AdvisoryStorageGrew = "allocated-storage-grew"

// GrowthState is the typed answer to "what does the history support?".
//
// The refusal states and the measured states are deliberately not
// distinguishable by a zero: a caller cannot fall into reading
// [GrowthTooFewObservations] as [GrowthFlat] by testing a number against zero,
// because a non-measured verdict carries no number to test. Use
// [GrowthState.Measured].
type GrowthState string

const (
	// GrowthUnevaluated is the zero value: the growth question was never put.
	// A target that carried no history at all is [GrowthNoHistory], not this.
	GrowthUnevaluated GrowthState = ""
	// GrowthNoHistory: not one observation was supplied. Operationally this
	// means the caller is not persisting checkpoints between runs, which is a
	// property of the WIRING and not of the instance — so it is recorded on
	// the assessment and, alone among these states, adds no suppression. See
	// [Sizer.growthFindings].
	GrowthNoHistory GrowthState = "no-history"
	// GrowthTooFewObservations: some history, below MinObservations.
	GrowthTooFewObservations GrowthState = "too-few-observations"
	// GrowthSpanTooShort: enough observations, too little elapsed time.
	GrowthSpanTooShort GrowthState = "span-too-short"
	// GrowthInconsistent: the observations are not one instance's history.
	GrowthInconsistent GrowthState = "inconsistent-history"
	// GrowthFlat: MEASURED, and the allocation never moved. This is a finding,
	// not an absence of one.
	GrowthFlat GrowthState = "flat"
	// GrowthGrew: MEASURED, and the allocation ratcheted up.
	GrowthGrew GrowthState = "grew"
)

// Measured reports whether a number was produced. It is the only correct way
// to tell "flat" from "we could not say", and [GrowthVerdict.Measurement] is
// non-nil for exactly the states it answers true for.
func (s GrowthState) Measured() bool { return s == GrowthFlat || s == GrowthGrew }

// GrowthPolicy is the evidence bar. Every field is a refusal threshold; see
// the Default* constants for the arithmetic behind each.
type GrowthPolicy struct {
	MinObservations int           `json:"minObservations"`
	MinSpan         time.Duration `json:"minSpan"`
	MinSteps        int           `json:"minSteps"`
	MaxGapFraction  float64       `json:"maxGapFraction"`
	MaxObservations int           `json:"maxObservations"`
}

// DefaultGrowthPolicy returns the shipped bar.
func DefaultGrowthPolicy() GrowthPolicy {
	return GrowthPolicy{
		MinObservations: DefaultGrowthMinObservations,
		MinSpan:         DefaultGrowthMinSpan,
		MinSteps:        DefaultGrowthMinSteps,
		MaxGapFraction:  DefaultGrowthMaxGapFraction,
		MaxObservations: DefaultGrowthMaxObservations,
	}
}

// normalized clamps a policy to the shipped floor. A caller may make the bar
// STRICTER and may not make it looser: a MinObservations of 2 would ship the
// exact claim §7.4 deferred this finding to avoid, so it is clamped up rather
// than honoured.
func (p GrowthPolicy) normalized() GrowthPolicy {
	if p.MinObservations < DefaultGrowthMinObservations {
		p.MinObservations = DefaultGrowthMinObservations
	}
	if p.MinSpan < DefaultGrowthMinSpan {
		p.MinSpan = DefaultGrowthMinSpan
	}
	if p.MinSteps < DefaultGrowthMinSteps {
		p.MinSteps = DefaultGrowthMinSteps
	}
	if p.MaxGapFraction <= 0 || p.MaxGapFraction > DefaultGrowthMaxGapFraction {
		p.MaxGapFraction = DefaultGrowthMaxGapFraction
	}
	if p.MaxObservations <= 0 || p.MaxObservations > DefaultGrowthMaxObservations {
		p.MaxObservations = DefaultGrowthMaxObservations
	}
	return p
}

// StorageObservation is one instant's view of an instance's allocated storage:
// what [DescribeDBInstances] reported, and when it reported it.
type StorageObservation struct {
	At           time.Time `json:"at"`
	AllocatedGiB int64     `json:"allocatedGiB"`
	// MaxAllocatedGiB is the autoscaling ceiling at that instant, carried so a
	// reader can see the ceiling move too. It takes no part in the arithmetic.
	MaxAllocatedGiB int64 `json:"maxAllocatedGiB,omitempty"`
}

// StorageHistory is one instance's observations, oldest first. It is the
// STATE this finding introduces — see GROWTH-FINDINGS.md §5 for what that
// costs — and it is carried on [Target] so it round-trips through the domain
// checkpoint with everything else.
type StorageHistory []StorageObservation

// Append folds one observation into the history and returns the result,
// oldest first, bounded to max observations by dropping the oldest.
//
// Re-appending an instant already present REPLACES it, which is the same
// idempotence pkg/store's SaveSnapshotAt gives: re-ingesting a history a
// second time is a no-op rather than a doubling. An observation with a zero
// instant or a non-positive allocation is DROPPED — "we did not look" is not
// an observation of zero storage, and admitting it would put a fake step into
// every history whose first record predates the field.
//
// max ≤ 0 means [DefaultGrowthMaxObservations]. The receiver is not mutated.
func (h StorageHistory) Append(obs StorageObservation, max int) StorageHistory {
	if max <= 0 || max > DefaultGrowthMaxObservations {
		max = DefaultGrowthMaxObservations
	}
	out := make(StorageHistory, 0, len(h)+1)
	replaced := false
	for _, o := range h {
		if o.At.IsZero() || o.AllocatedGiB <= 0 {
			continue
		}
		if o.At.Equal(obs.At) {
			if obs.At.IsZero() || obs.AllocatedGiB <= 0 {
				continue
			}
			out = append(out, normalizeObs(obs))
			replaced = true
			continue
		}
		out = append(out, normalizeObs(o))
	}
	if !replaced && !obs.At.IsZero() && obs.AllocatedGiB > 0 {
		out = append(out, normalizeObs(obs))
	}
	sortHistory(out)
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// normalizeObs puts an observation's instant in UTC so two histories that
// travelled through different time zones compare and sort identically.
func normalizeObs(o StorageObservation) StorageObservation {
	o.At = o.At.UTC()
	if o.MaxAllocatedGiB < 0 {
		o.MaxAllocatedGiB = 0
	}
	return o
}

// sortHistory orders by instant, then by allocation so a same-instant
// contradiction has one canonical order and is DETECTED rather than resolved
// by whichever record happened to arrive first.
func sortHistory(h StorageHistory) {
	sort.SliceStable(h, func(i, j int) bool {
		if !h[i].At.Equal(h[j].At) {
			return h[i].At.Before(h[j].At)
		}
		return h[i].AllocatedGiB < h[j].AllocatedGiB
	})
}

// clean returns the history in canonical form and reports whether two
// observations at one instant disagreed.
//
// Identical (instant, allocation) pairs collapse silently — that is a replayed
// checkpoint, not a fault. Two different allocations at one instant do NOT
// collapse: that is a contradiction, and a contradiction is the caller's to
// explain rather than this package's to average away.
func (h StorageHistory) clean(max int) (StorageHistory, bool) {
	if max <= 0 || max > DefaultGrowthMaxObservations {
		max = DefaultGrowthMaxObservations
	}
	out := make(StorageHistory, 0, len(h))
	for _, o := range h {
		if o.At.IsZero() || o.AllocatedGiB <= 0 {
			continue
		}
		out = append(out, normalizeObs(o))
	}
	sortHistory(out)
	conflict := false
	dedup := out[:0]
	for i, o := range out {
		if i > 0 && o.At.Equal(dedup[len(dedup)-1].At) {
			if o.AllocatedGiB != dedup[len(dedup)-1].AllocatedGiB {
				conflict = true
			}
			continue
		}
		dedup = append(dedup, o)
	}
	if len(dedup) > max {
		dedup = dedup[len(dedup)-max:]
	}
	return dedup, conflict
}

// GrowthSampling describes the SHAPE of the retained history, and is filled
// for every state including the refusals: a refusal that does not say how
// short the history was is not actionable.
//
// The gaps are reported rather than averaged. pkg/store thins its history to
// at most one snapshot per cadence bucket and prunes by count and by bytes, so
// the retained instants are irregular by construction and a mean gap would
// describe a history that was never recorded.
type GrowthSampling struct {
	Observations int       `json:"observations"`
	First        time.Time `json:"first,omitzero"`
	Last         time.Time `json:"last,omitzero"`
	// Span is Last−First, from the ACTUAL instants. It is not
	// Observations × any assumed cadence.
	Span time.Duration `json:"span,omitempty"`
	// MinGap and MaxGap bound the intervals between consecutive observations.
	MinGap time.Duration `json:"minGap,omitempty"`
	MaxGap time.Duration `json:"maxGap,omitempty"`
	// MeanGap is Span ÷ (Observations−1): the interval an evenly-spaced
	// history WOULD have had. It is reported beside MinGap/MaxGap precisely so
	// the distance between them is visible, and no arithmetic here uses it.
	MeanGap time.Duration `json:"meanGap,omitempty"`
	// LargestGapFraction is MaxGap ÷ Span — the share of the span that sits
	// inside a single interval nobody observed. The headline irregularity
	// number, and the one [ReasonGrowthRateNotProjectable] tests.
	LargestGapFraction float64 `json:"largestGapFraction,omitempty"`
	// Regular is LargestGapFraction ≤ the policy's MaxGapFraction.
	Regular bool `json:"regular,omitempty"`
	// Truncated reports that the supplied history was longer than the policy
	// reads, so Span is the retained span and not the instance's lifetime.
	Truncated bool `json:"truncated,omitempty"`
}

// Sampling measures a history's shape under a policy, without judging it.
func (h StorageHistory) Sampling(p GrowthPolicy) GrowthSampling {
	p = p.normalized()
	clean, _ := h.clean(p.MaxObservations)
	s := GrowthSampling{Observations: len(clean), Truncated: len(clean) < countUsable(h)}
	if len(clean) == 0 {
		return s
	}
	s.First, s.Last = clean[0].At, clean[len(clean)-1].At
	s.Span = s.Last.Sub(s.First)
	if len(clean) < 2 {
		return s
	}
	s.MinGap, s.MaxGap = s.Span, 0
	for i := 1; i < len(clean); i++ {
		gap := clean[i].At.Sub(clean[i-1].At)
		if gap < s.MinGap {
			s.MinGap = gap
		}
		if gap > s.MaxGap {
			s.MaxGap = gap
		}
	}
	s.MeanGap = s.Span / time.Duration(len(clean)-1)
	if s.Span > 0 {
		s.LargestGapFraction = float64(s.MaxGap) / float64(s.Span)
	}
	s.Regular = s.LargestGapFraction <= p.MaxGapFraction
	return s
}

// countUsable is how many observations survive the "we did not look is not a
// zero" filter, before the retention bound is applied.
func countUsable(h StorageHistory) int {
	n := 0
	for _, o := range h {
		if !o.At.IsZero() && o.AllocatedGiB > 0 {
			n++
		}
	}
	return n
}

// GrowthProjection is the per-day rate, and it is a CLAIM ABOUT THE FUTURE.
//
// It has no dollar field, no saving, and no reclaim, and Claimable is a method
// returning false unconditionally so no serialized form and no future struct
// literal can say otherwise. TestGrowthProjectionCarriesNoMoney asserts the
// absence by reflection. The growth's monthly COST lives on
// [GrowthMeasurement], where it describes money already being spent rather
// than money predicted.
type GrowthProjection struct {
	// GiBPerDay is the measured growth divided by the ACTUAL span in days —
	// Last−First, never Observations × an assumed cadence. [unverified]: it
	// extrapolates a step function whose steps are driven by write volume this
	// package does not observe.
	GiBPerDay float64 `json:"giBPerDay"`
	// Steps and SpanDays are the two numbers the rate was divided out of, so a
	// reader can reconstruct it rather than trust it.
	Steps    int     `json:"steps"`
	SpanDays float64 `json:"spanDays"`
}

// Claimable is false for every projection, always. A growth rate may motivate
// a question and may never become a number in a savings column.
func (GrowthProjection) Claimable() bool { return false }

// String renders the projection with its unverified marker attached, so the
// marker cannot be dropped by a caller formatting the number itself.
func (g GrowthProjection) String() string {
	return fmt.Sprintf("[unverified] ~%.2f GiB/day (%d increases over %.1f days)",
		g.GiBPerDay, g.Steps, g.SpanDays)
}

// GrowthMeasurement is what the history actually showed. It exists only when
// [GrowthState.Measured] is true, which is what keeps "not enough history"
// from being readable as "flat".
//
// Read the ABSENT fields as the specification, the same way [StorageVerdict]
// does: there is a MonthlyUSD and there is no SavingMonthlyUSD, no
// ReclaimableGiB and no RecommendedGiB, because allocated storage is a monotone
// ratchet and a field named after a reduction would be a promise no RDS API
// can keep.
type GrowthMeasurement struct {
	FirstGiB int64 `json:"firstGiB"`
	LastGiB  int64 `json:"lastGiB"`
	// GrewGiB is LastGiB−FirstGiB and is never negative: a decrease is
	// [GrowthInconsistent] and never reaches here.
	GrewGiB int64 `json:"grewGiB"`
	// Steps is how many transitions strictly increased, classified by
	// [AttributeStorageGrowth] — the same ledger rule §7.4 names, applied
	// pairwise across the series. Steps == 1 means the whole growth is one
	// event.
	Steps int `json:"steps"`
	// LargestStepGiB is the biggest single increase. A growth dominated by one
	// step is a scale-up, not a trend, whatever the span says.
	LargestStepGiB int64 `json:"largestStepGiB,omitempty"`
	// MonthlyUSD is what the GROWTH now costs every month at the card's
	// storage rate. It is a magnitude and a permanent one: this is money the
	// account started spending without being asked and cannot stop spending.
	MonthlyUSD     float64        `json:"monthlyUSD,omitempty"`
	RateProvenance RateProvenance `json:"rateProvenance,omitempty"`
	// Projection is non-nil only when the rate survived the step and gap
	// gates. Nil with a positive GrewGiB is the normal, correct outcome for a
	// single autoscaling event.
	Projection *GrowthProjection `json:"projection,omitempty"`
}

// GrowthVerdict is this package's complete answer about one instance's
// allocated-storage history.
type GrowthVerdict struct {
	State    GrowthState    `json:"state"`
	Policy   GrowthPolicy   `json:"policy,omitzero"`
	Sampling GrowthSampling `json:"sampling,omitzero"`
	// Measurement is non-nil if and only if State.Measured(). That biconditional
	// is the whole defence against a caller collapsing "we have not looked long
	// enough" into "we looked and it is flat", and
	// TestGrowthMeasurementExistsExactlyWhenMeasured pins it.
	Measurement *GrowthMeasurement `json:"measurement,omitempty"`
	// Code and Reason are the refusal this verdict carries. Every state except
	// [GrowthFlat] has one — a flat, well-observed history is a measurement
	// with nothing to refuse.
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// RateCode and RateReason are the SECOND refusal a measured growth can
	// carry: the size was stated and the rate was withheld. They are separate
	// fields because a caller must be able to see that a growth was measured
	// AND that its rate was refused, which one code slot cannot say.
	RateCode   string `json:"rateCode,omitempty"`
	RateReason string `json:"rateReason,omitempty"`
}

// Measured is true when a number was produced. Prefer it to any test against a
// field: on a refusal there is no field to test.
func (v GrowthVerdict) Measured() bool { return v.State.Measured() }

// GrewGiB returns the measured increase and whether one was measured at all.
// A caller that ignores the boolean gets 0, which is indistinguishable from a
// flat instance — the two-return form exists so that mistake has to be made on
// purpose.
func (v GrowthVerdict) GrewGiB() (int64, bool) {
	if v.Measurement == nil {
		return 0, false
	}
	return v.Measurement.GrewGiB, true
}

// Rate returns the projected growth rate and whether one was projectable.
// The projection is [unverified] and carries no money; see [GrowthProjection].
func (v GrowthVerdict) Rate() (GrowthProjection, bool) {
	if v.Measurement == nil || v.Measurement.Projection == nil {
		return GrowthProjection{}, false
	}
	return *v.Measurement.Projection, true
}

// AssessStorageGrowth measures one instance's allocated-storage history.
//
// It is pure: no clock, no I/O, no package state. The instants come from the
// history the caller persisted, and every threshold comes from p. card may be
// a zero RateCard, in which case the growth is reported in GiB with no dollar
// beside it — a weaker finding, and still the right one.
//
// It never proposes anything. There is no return path to [Proposal] from here.
func AssessStorageGrowth(d DBInstance, h StorageHistory, p GrowthPolicy, card RateCard) GrowthVerdict {
	p = p.normalized()
	v := GrowthVerdict{Policy: p, Sampling: h.Sampling(p)}
	clean, conflict := h.clean(p.MaxObservations)
	name := d.DisplayName()

	if conflict {
		v.State = GrowthInconsistent
		v.Code = ReasonGrowthHistoryInconsistent
		v.Reason = fmt.Sprintf(
			"the allocated-storage history of %s records two different allocations at one instant, so it "+
				"is not one instance's history — a replaced instance reusing an identifier is the common "+
				"cause. No growth is stated from it: averaging a contradiction into a trend turns a data "+
				"fault into a confident number. Nothing here implies a reduction is available; %s",
			name, ratchetClause)
		return v
	}
	if len(clean) == 0 {
		v.State = GrowthNoHistory
		v.Code = ReasonGrowthInsufficientObservations
		v.Reason = fmt.Sprintf(
			"no allocated-storage history was supplied for %s, so no growth is stated. Storage autoscaling "+
				"moves the floor on its own and \"autoscaling operations aren't logged by AWS CloudTrail\", "+
				"so the only way to see the floor move is to compare persisted observations across runs — "+
				"which requires the caller to checkpoint this domain between invocations. This is a "+
				"property of the wiring, not of the instance", name)
		return v
	}
	if len(clean) < p.MinObservations {
		v.State = GrowthTooFewObservations
		v.Code = ReasonGrowthInsufficientObservations
		v.Reason = fmt.Sprintf(
			"%s has %d allocated-storage observation(s), below the %d this package will state a growth "+
				"from. %d observations bound %d interval(s); two samples are a difference rather than a "+
				"trend, and storage autoscaling adds lumpy discrete steps, so a slope drawn through too "+
				"few of them predicts a future the evidence does not contain. This is NOT a report that "+
				"the allocation is flat — nothing was measured. Wait for more observations",
			name, len(clean), p.MinObservations, len(clean), max(len(clean)-1, 0))
		return v
	}
	if v.Sampling.Span < p.MinSpan {
		v.State = GrowthSpanTooShort
		v.Code = ReasonGrowthSpanTooShort
		v.Reason = fmt.Sprintf(
			"%s has %d allocated-storage observations but they span only %s, below the %s minimum. A "+
				"database's write volume has a weekly shape, and one weekly cycle is one event; %s is the "+
				"shortest history in which \"it happened again\" is a statement rather than an assumption. "+
				"This is NOT a report that the allocation is flat — nothing was measured. Denser sampling "+
				"does not fix it; only elapsed time does",
			name, len(clean), fmtSpan(v.Sampling.Span), fmtSpan(p.MinSpan), fmtSpan(p.MinSpan))
		return v
	}

	// The ledger rule from trap 8, applied pairwise across the series rather
	// than across a single pair. One implementation of the rule, one test.
	m := &GrowthMeasurement{FirstGiB: clean[0].AllocatedGiB, LastGiB: clean[len(clean)-1].AllocatedGiB}
	for i := 1; i < len(clean); i++ {
		attr, why := AttributeStorageGrowth(clean[i-1].AllocatedGiB, clean[i].AllocatedGiB)
		switch attr {
		case StorageShrankImpossible:
			v.State = GrowthInconsistent
			v.Code = ReasonGrowthHistoryInconsistent
			v.Reason = fmt.Sprintf(
				"the allocated-storage history of %s is not monotone and therefore is not one instance's: "+
					"%s. No growth rate is drawn through it, and no reduction is inferred from the "+
					"decrease — %s", name, why, ratchetClause)
			return v
		case StorageGrewUnattributed:
			step := clean[i].AllocatedGiB - clean[i-1].AllocatedGiB
			m.Steps++
			if step > m.LargestStepGiB {
				m.LargestStepGiB = step
			}
		}
	}
	m.GrewGiB = m.LastGiB - m.FirstGiB
	if m.GrewGiB <= 0 {
		v.State = GrowthFlat
		v.Measurement = m
		return v
	}

	v.State = GrowthGrew
	if usd, prov, ok := card.StorageMonthlyUSD(d.StorageType, m.GrewGiB); ok {
		m.MonthlyUSD, m.RateProvenance = zeroIfNotFinite(usd), prov
	}

	// The rate, and the two ways it is withheld. The SIZE above does not
	// depend on either gate: it is a measured fact about the past.
	days := v.Sampling.Span.Hours() / 24
	switch {
	case m.Steps < p.MinSteps:
		v.RateCode = ReasonGrowthRateNotProjectable
		v.RateReason = fmt.Sprintf(
			"%s grew %d GiB across %s, and no growth RATE is stated from it: the whole increase arrived in "+
				"%d step(s), below the %d this package projects from. Storage autoscaling adds \"the "+
				"greater of %d GiB, %s of the current allocation, or the growth predicted for the next "+
				"%s\" in discrete steps, so one step is one event and a per-day rate over it asserts a "+
				"recurrence the evidence does not contain. The size above is measured; the future is not",
			name, m.GrewGiB, fmtSpan(v.Sampling.Span), m.Steps, p.MinSteps,
			AutoscaleMinIncrementGiB, fmtPct(AutoscaleIncrementFraction), AutoscalePredictionHorizon)
	case !v.Sampling.Regular:
		v.RateCode = ReasonGrowthRateNotProjectable
		v.RateReason = fmt.Sprintf(
			"%s grew %d GiB across %s, and no growth RATE is stated from it: the largest gap between two "+
				"retained observations is %s, %s of the span, above the %s bound. The retained history is "+
				"thinned rather than evenly sampled, and when most of a span sits inside one interval "+
				"nobody observed, the whole increase could have arrived inside it. The size above is "+
				"measured; the rate would be arithmetic rather than evidence",
			name, m.GrewGiB, fmtSpan(v.Sampling.Span), fmtSpan(v.Sampling.MaxGap),
			fmtPct(v.Sampling.LargestGapFraction), fmtPct(p.MaxGapFraction))
	case days > 0:
		m.Projection = &GrowthProjection{
			GiBPerDay: zeroIfNotFinite(float64(m.GrewGiB) / days),
			Steps:     m.Steps,
			SpanDays:  days,
		}
	}

	v.Code = ReasonGrowthIsNotReclaimable
	v.Reason = fmt.Sprintf(
		"the allocated storage of %s grew %d GiB — from %d GiB to %d GiB — across %s of retained history, "+
			"in %d observation(s) and %d increase(s), the largest of them %d GiB%s. This domain has no "+
			"actuator, so it was not Kilter; storage autoscaling is the usual cause and its operations are "+
			"not logged by CloudTrail, so there is no event to correlate against, and the increase is "+
			"recorded as unattributed. None of it is recoverable: %s. This is the measured size of a "+
			"permanent cost, not a reduction that is available",
		name, m.GrewGiB, m.FirstGiB, m.LastGiB, fmtSpan(v.Sampling.Span), v.Sampling.Observations,
		m.Steps, m.LargestStepGiB, growthCostClause(m), ratchetClause)
	v.Measurement = m
	return v
}

// ratchetClause is the one sentence every growth refusal ends on, quoted so
// the reader gets AWS's words rather than this package's paraphrase
// [verified: USER_PIOPS.Autoscaling.html].
const ratchetClause = "\"You can't reduce the amount of storage for a DB instance after storage has been " +
	"allocated\", and the documented alternatives are a blue/green deployment or a migration to a new " +
	"instance, neither of which Kilter performs"

func growthCostClause(m *GrowthMeasurement) string {
	if m.MonthlyUSD <= 0 {
		return ""
	}
	return fmt.Sprintf(", which now costs %s/mo at a %s storage rate", fmtUSD(m.MonthlyUSD), m.RateProvenance)
}

// fmtSpan renders a duration in days-and-hours, because "504h0m0s" is not a
// span a reader can weigh against a weekly cycle.
func fmtSpan(d time.Duration) string {
	if d <= 0 {
		return "0h"
	}
	days := int64(d / (24 * time.Hour))
	rem := d - time.Duration(days)*24*time.Hour
	hours := int64(rem / time.Hour)
	switch {
	case days == 0 && hours == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	case days == 0:
		return fmt.Sprintf("%dh", hours)
	case hours == 0:
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, hours)
}

// growthFindings turns the growth verdict into report output: evidence for the
// sampling shape, a refusal carrying the size, and an advisory carrying the
// permanent cost.
//
// The one state that produces no suppression is [GrowthNoHistory]. An empty
// history means the CALLER is not persisting checkpoints, which is one fact
// about the wiring and not N facts about N instances; stamping the same
// sentence on every row of a fleet report would bury the findings that are
// about instances. The verdict is still recorded on the assessment, typed and
// with its reason filled, so a caller can see it and GROWTH-FINDINGS.md §6
// says what to do about it.
func (s *Sizer) growthFindings(a *Assessment, t Target, snap *Snapshot) {
	v := AssessStorageGrowth(a.Instance, t.StorageHistory, s.cfg.Growth, s.cfg.Rates)
	a.Growth = v
	if v.State == GrowthNoHistory {
		return
	}

	a.Evidence = append(a.Evidence, domain.Evidence{
		Metric: "allocated-storage-history", Value: describeSampling(v.Sampling),
		Window: fmtSpan(v.Sampling.Span), Samples: v.Sampling.Observations,
		Source: SourceCheckpointHistory, At: v.Sampling.Last,
	})

	if v.Code != "" {
		a.suppress(v.Code, v.Reason)
	}
	if v.RateCode != "" {
		a.suppress(v.RateCode, v.RateReason)
	}

	m := v.Measurement
	if m == nil {
		return
	}
	if v.State == GrowthFlat {
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: "allocated-storage-growth",
			Value: fmt.Sprintf("flat: %d GiB unchanged across %s and %d observations",
				m.LastGiB, fmtSpan(v.Sampling.Span), v.Sampling.Observations),
			Window: fmtSpan(v.Sampling.Span), Samples: v.Sampling.Observations,
			Source: SourceCheckpointHistory, At: v.Sampling.Last,
		})
		return
	}

	a.Evidence = append(a.Evidence, domain.Evidence{
		Metric: "allocated-storage-growth",
		Value: fmt.Sprintf("grew %d GiB (%d → %d) in %d step(s) across %s",
			m.GrewGiB, m.FirstGiB, m.LastGiB, m.Steps, fmtSpan(v.Sampling.Span)),
		Window: fmtSpan(v.Sampling.Span), Samples: v.Sampling.Observations,
		Source: SourceCheckpointHistory, At: v.Sampling.Last,
	})
	if p, ok := v.Rate(); ok {
		a.Evidence = append(a.Evidence, domain.Evidence{
			Metric: "allocated-storage-growth-rate", Value: p.String(),
			Window: fmtSpan(v.Sampling.Span), Samples: v.Sampling.Observations,
			Source: SourceCheckpointHistory, At: v.Sampling.Last,
		})
	}

	a.advise(Advisory{
		Code: AdvisoryStorageGrew,
		Message: fmt.Sprintf("%s acquired %d GiB of allocated storage (%d → %d GiB) across %s, in %d "+
			"increase(s) nothing in this account requested and no CloudTrail event records",
			a.Instance.DisplayName(), m.GrewGiB, m.FirstGiB, m.LastGiB,
			fmtSpan(v.Sampling.Span), m.Steps),
		Caveat: "this is a permanent cost that has already been incurred, not a saving and not an " +
			"opportunity: no RDS API reduces allocated storage, so the increase cannot be undone at any " +
			"price. " + describeSampling(v.Sampling) + growthRateCaveat(v),
		MonthlyUSD:     m.MonthlyUSD,
		RateProvenance: m.RateProvenance,
	})
}

// growthRateCaveat states, inside the advisory's own caveat, whether a rate
// was projected — so the advisory cannot be read as carrying a forecast when
// it does not, or as carrying a verified one when it does.
func growthRateCaveat(v GrowthVerdict) string {
	if p, ok := v.Rate(); ok {
		return " Growth rate " + p.String() + ": a claim about the future from a step function whose steps " +
			"this package does not observe the cause of, and never a dollar figure."
	}
	return " No growth rate is projected from this history; the size above is a measured fact about the past."
}

// describeSampling renders the irregularity rather than smoothing it, because
// a reader weighing a growth number needs to know how much of the span was
// actually looked at.
func describeSampling(s GrowthSampling) string {
	if s.Observations == 0 {
		return "no retained observations"
	}
	if s.Observations == 1 {
		return fmt.Sprintf("1 retained observation at %s", s.Last.UTC().Format(time.RFC3339))
	}
	out := fmt.Sprintf("%d retained observations spanning %s (%s → %s); the instants are thinned by the "+
		"store's retention policy and are not evenly spaced — gaps run %s to %s against a %s mean, and the "+
		"largest single gap is %s of the span",
		s.Observations, fmtSpan(s.Span), s.First.UTC().Format(time.RFC3339),
		s.Last.UTC().Format(time.RFC3339), fmtSpan(s.MinGap), fmtSpan(s.MaxGap), fmtSpan(s.MeanGap),
		fmtPct(s.LargestGapFraction))
	if s.Truncated {
		out += ". Older observations were dropped by the retention bound, so the span is the retained " +
			"history and not the instance's lifetime"
	}
	return out
}

// GrowthTotals is the fleet-level roll-up of this finding, for a caller that
// wants the headline without walking every assessment.
//
// Note what is counted and what is not: there is a MonthlyUSD naming a cost
// and there is no saving, because none of this is recoverable. Refused counts
// the instances this package DECLINED to measure — the number that tells an
// operator their history is too young, which no total of measured growth
// could.
type GrowthTotals struct {
	// Measured is instances with enough history to state something.
	Measured int `json:"measured"`
	// Grew and Flat partition Measured. Both are findings.
	Grew int `json:"grew"`
	Flat int `json:"flat"`
	// Refused is instances with a history that did not clear the bar. It
	// excludes instances with no history at all, which are NoHistory.
	Refused int `json:"refused"`
	// NoHistory is instances for which nothing was persisted — a count of the
	// wiring gap, not of the fleet.
	NoHistory int `json:"noHistory"`
	// RateRefused is instances measured as having grown whose per-day rate was
	// withheld. A subset of Grew.
	RateRefused int `json:"rateRefused"`

	GrewGiB int64 `json:"grewGiB"`
	// UnrecoverableGrowthMonthlyUSD is what the fleet's measured growth now
	// costs every month, permanently. It is a cost and never a saving.
	UnrecoverableGrowthMonthlyUSD float64 `json:"unrecoverableGrowthMonthlyUSD"`
}

// StorageGrowth rolls the growth verdicts up across a report. It walks
// Assessments, which [Sizer.Assess] has already sorted, so the result does not
// depend on map order or on collection order.
func (r *Report) StorageGrowth() GrowthTotals {
	var t GrowthTotals
	if r == nil {
		return t
	}
	for _, a := range r.Assessments {
		v := a.Growth
		switch {
		case v.State == GrowthUnevaluated:
			continue
		case v.State == GrowthNoHistory:
			t.NoHistory++
			continue
		case !v.Measured():
			t.Refused++
			continue
		}
		t.Measured++
		if v.State == GrowthFlat {
			t.Flat++
			continue
		}
		t.Grew++
		if v.RateCode != "" {
			t.RateRefused++
		}
		if m := v.Measurement; m != nil {
			t.GrewGiB += m.GrewGiB
			t.UnrecoverableGrowthMonthlyUSD += m.MonthlyUSD
		}
	}
	return t
}

// --- The persistence seam, and the only two calls cmd/ has to make ----------

// StorageHistories returns every instance's allocated-storage history as this
// domain currently holds it, keyed by target ID.
//
// It exists because [Domain.Observe] REPLACES a target wholesale — the
// collector re-queries a window each tick and accumulating would double-count
// the overlap — so a history restored from a checkpoint would be discarded by
// the next collection unless the caller carries it across. That is deliberate
// on Observe's part and it is why the history is the CALLER's to fold in; this
// method plus [RecordStorageHistory] are the two halves of doing so.
//
// The returned map and its slices are copies: a caller cannot reach into the
// domain's state through them. Ranging over the map is not deterministic, so
// use it as a lookup table and never as an output order.
func (d *Domain) StorageHistories() map[string]StorageHistory {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]StorageHistory, len(d.targets))
	for id, t := range d.targets {
		if len(t.StorageHistory) == 0 {
			continue
		}
		out[id] = append(StorageHistory(nil), t.StorageHistory...)
	}
	return out
}

// RecordStorageHistory folds each target's currently-observed allocated
// storage into the history that target carried before, and writes the result
// back onto the snapshot.
//
// Call it on a freshly collected snapshot BEFORE [Domain.Observe] or
// [Domain.Learn], with prior taken from [Domain.StorageHistories] after
// [Domain.Restore]. The history then rides the existing checkpoint — it is a
// field on [Target], which [Domain.Checkpoint] already serializes whole — so
// persisting this finding needs no new store, no new key and no new codec.
//
// The observation instant is the SNAPSHOT's, falling back to the end of its
// window: this package reads no clock, and a history keyed by when the process
// happened to run is not replayable. A snapshot with neither is skipped rather
// than stamped with a zero instant, because "we did not look" is not an
// observation.
//
// It is idempotent. Re-recording a snapshot already folded in replaces that
// instant rather than doubling it, the same rule pkg/store's SaveSnapshotAt
// applies, so a retried run does not manufacture a step.
func RecordStorageHistory(snap *Snapshot, prior map[string]StorageHistory, p GrowthPolicy) {
	if snap == nil {
		return
	}
	at := snap.Timestamp
	if at.IsZero() {
		at = snap.Window.End
	}
	if at.IsZero() {
		return
	}
	max := p.normalized().MaxObservations
	for i := range snap.Targets {
		t := &snap.Targets[i]
		h := t.StorageHistory
		if len(h) == 0 {
			h = prior[t.Ref.ID]
		}
		t.StorageHistory = h.Append(StorageObservation{
			At:              at,
			AllocatedGiB:    t.Instance.AllocatedStorageGiB,
			MaxAllocatedGiB: t.Instance.MaxAllocatedStorageGiB,
		}, max)
	}
}
