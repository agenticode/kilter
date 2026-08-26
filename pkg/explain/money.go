package explain

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Micro is a money amount in millionths of a US dollar (µUSD).
//
// Every number this package adds, subtracts or compares is a Micro, and the
// reason is the package's central invariant: sum(terms) + residual == ΔCost,
// asserted bit-exactly. Integer addition is associative and exact, so that
// identity cannot depend on the order terms happen to be produced in.
// float64 dollars have neither property — `pkg/ecs` shipped a real
// nondeterminism bug this week because a total depended on arrival order —
// so floats live only inside a single term's arithmetic, where they are
// summed in a sorted order (see sumSorted) and quantized to Micro exactly
// once, at the end.
//
// µUSD is fine enough that quantization is invisible against real money
// (1 µUSD/h is $0.00073/month) and coarse enough that every amount in a
// 5000-node cluster fits int64 with ten orders of magnitude to spare.
type Micro int64

// MicroPerUSD is the fixed-point scale.
const MicroPerUSD = 1_000_000

// hoursPerMonth is the billing-average month, matching pricing.HoursPerMonth.
// It is duplicated rather than imported so pkg/explain's decomposition stays
// free of the cloud SDKs pkg/pricing pulls in; TestHoursPerMonthMatchesPricing
// fails if the two ever drift.
const hoursPerMonth = 730

// maxMicro bounds every amount the package accepts or produces: ±$1e9 per
// hour, roughly a thousand times the largest cloud bill on earth. The bound
// is not decoration — it is what makes the overflow checks below provably
// sufficient for the products and sums the decomposition performs.
const maxMicro = Micro(1e15)

// MaxUSD is maxMicro in dollars, for error messages and validation.
const MaxUSD = float64(maxMicro) / MicroPerUSD

// ErrRange reports an amount that is not finite, is outside ±MaxUSD, or
// would overflow int64 mid-arithmetic. The decomposition returns it rather
// than saturating: a saturated total is a wrong answer wearing the costume
// of a right one, and this package's whole claim is that its numbers add up.
var ErrRange = errors.New("explain: money amount out of range")

// MicroFromUSD converts dollars to µUSD, rounding halves away from zero.
func MicroFromUSD(v float64) (Micro, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: %v is not finite", ErrRange, v)
	}
	scaled := math.Round(v * MicroPerUSD)
	if scaled > float64(maxMicro) || scaled < -float64(maxMicro) {
		return 0, fmt.Errorf("%w: %v USD outside ±%.0f USD", ErrRange, v, MaxUSD)
	}
	return Micro(scaled), nil
}

// quantize converts an amount already expressed in µUSD (typically the
// float64 result of one term's share arithmetic) to Micro, rounding halves
// away from zero. The rounding error it introduces — strictly under 0.5 µUSD
// per term — is absorbed by the residual, never by another term.
func quantize(microFloat float64) (Micro, error) {
	if math.IsNaN(microFloat) || math.IsInf(microFloat, 0) {
		return 0, fmt.Errorf("%w: %v µUSD is not finite", ErrRange, microFloat)
	}
	r := math.Round(microFloat)
	if r > float64(maxMicro) || r < -float64(maxMicro) {
		return 0, fmt.Errorf("%w: %v µUSD outside ±%d µUSD", ErrRange, microFloat, int64(maxMicro))
	}
	return Micro(r), nil
}

// USD returns the amount in dollars. Derived, never authoritative: JSON
// consumers that need exactness read the µUSD field.
func (m Micro) USD() float64 { return float64(m) / MicroPerUSD }

// MonthlyUSD projects an hourly rate to a billing-average month.
func (m Micro) MonthlyUSD() float64 { return m.USD() * hoursPerMonth }

// add returns a+b, or ErrRange if the sum leaves ±maxMicro. Checking against
// maxMicro rather than int64 bounds keeps every intermediate far from
// wraparound, so a later multiply cannot overflow silently.
func add(a, b Micro) (Micro, error) {
	s := a + b
	// a and b are each within ±maxMicro (1e15), so a+b is within ±2e15 and
	// cannot wrap int64; the range check below is therefore exact.
	if s > maxMicro || s < -maxMicro {
		return 0, fmt.Errorf("%w: %d + %d µUSD", ErrRange, a, b)
	}
	return s, nil
}

// sub returns a-b, or ErrRange on overflow.
func sub(a, b Micro) (Micro, error) { return add(a, -b) }

// mul returns m*n (a unit price times a node count), or ErrRange. The
// bound is checked by division *before* multiplying, so no intermediate ever
// wraps int64 — the after-the-fact `p/n != m` idiom is unsound at the
// int64 extremes and this arithmetic is load-bearing for an audit claim.
func mul(m Micro, n int64) (Micro, error) {
	if m == 0 || n == 0 {
		return 0, nil
	}
	if m > maxMicro || m < -maxMicro {
		return 0, fmt.Errorf("%w: %d µUSD", ErrRange, m)
	}
	a, b := abs64(int64(m)), abs64(n)
	if b == 0 || a > int64(maxMicro)/b {
		return 0, fmt.Errorf("%w: %d µUSD × %d", ErrRange, m, n)
	}
	return Micro(int64(m) * n), nil
}

// abs64 is |v| with the one input that has no absolute value rejected by
// the callers' range checks; it saturates rather than wrapping so a stray
// math.MinInt64 cannot turn into a negative "magnitude".
func abs64(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}

// sumMicro adds amounts left to right with an overflow check at every step.
// No sorting: int64 addition is exact, so the result is order-independent
// already — which is precisely why the terms are Micro and not float64.
func sumMicro(vals []Micro) (Micro, error) {
	var total Micro
	for _, v := range vals {
		var err error
		if total, err = add(total, v); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// sumSorted adds float64 values in ascending order.
//
// Float addition is not associative, so a sum over a map, or over whatever
// order the caller's slice happened to arrive in, is not a function of its
// inputs — it is a function of the iteration. Sorting first makes the sum a
// pure function of the multiset, which is the property every golden test in
// this package depends on. Determinism is the whole reason; any total order
// would do, and ascending is merely the cheapest one to state.
//
// Callers pass only finite values (inputs are validated at the boundary), so
// the ordering is total.
func sumSorted(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := make([]float64, len(vals))
	copy(s, vals)
	sort.Float64s(s)
	total := 0.0
	for _, v := range s {
		total += v
	}
	return total
}
