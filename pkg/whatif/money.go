package whatif

import (
	"math"
	"sort"
)

// sumUSD sums a multiset of dollar amounts in a canonical order.
//
// Float addition is not associative: summing the same values in a different
// sequence can change the last bits, and a total that depends on the order
// records happened to arrive in is a non-deterministic output. PR#27 shipped
// exactly that bug, and pkg/backtest and pkg/explain both carry the same
// sorted-sum discipline. Sorting first makes the result a function of the
// multiset alone.
//
// Non-finite inputs are dropped rather than propagated, and the reason is
// concrete rather than fastidious: encoding/json cannot marshal NaN or ±Inf at
// all, so a single one would turn Result.Encode and Store.Snapshot into errors
// — a proposal that cannot be written down. Worse, +Inf and −Inf in the same
// multiset sum to NaN, so keeping infinities would not even be order-stable in
// the way this function exists to guarantee (FuzzSumUSDIsOrderIndependent
// found exactly that). Every producer in this package funnels through
// pkg/backtest metrics that are already guaranteed finite, so a dropped term
// is a bug upstream, not a rounding decision here.
func sumUSD(v []float64) float64 {
	s := make([]float64, 0, len(v))
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			continue
		}
		s = append(s, x)
	}
	sort.Float64s(s)
	total := 0.0
	for _, x := range s {
		total += x
	}
	// Finite terms can still overflow to ±Inf. Saturate rather than let it
	// escape: the total has to be encodable, and a saturated one is visibly
	// absurd — it will blow past every gate margin and be rejected, which is
	// the right outcome for arithmetic that has already lost its meaning.
	// The sorted order makes even this deterministic.
	switch {
	case math.IsNaN(total):
		return 0
	case math.IsInf(total, 1):
		return math.MaxFloat64
	case math.IsInf(total, -1):
		return -math.MaxFloat64
	}
	return total
}

// round6 quantizes a reported float to six decimals — a tenth of a cent, well
// below any meaningful difference — matching backtest.round6 so a delta and
// the scorecards it was computed from are quantized on the same grid. Without
// the match, `candidate.RegretUSD - baseline.RegretUSD` and the reported
// delta could differ in the last digit and make a golden file flap.
func round6(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*1e6) / 1e6
}

// pctChange is (candidate − baseline) / |baseline| as a percentage, reported
// as zero when the baseline is zero: "infinitely better than nothing" is not
// a number an approver can act on, and it must not become ±Inf in JSON.
func pctChange(baseline, candidate float64) float64 {
	if baseline == 0 || math.IsNaN(baseline) || math.IsNaN(candidate) {
		return 0
	}
	return round6((candidate - baseline) / math.Abs(baseline) * 100)
}
