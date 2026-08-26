package explain

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/agenticode/kilter/pkg/pricing"
)

// TestHoursPerMonthMatchesPricing keeps the locally-declared constant honest.
// pkg/explain does not import pkg/pricing in production code (it would drag
// the cloud SDKs into the decomposition), so this test is the only thing
// stopping the two from silently disagreeing about what a month is.
func TestHoursPerMonthMatchesPricing(t *testing.T) {
	if hoursPerMonth != pricing.HoursPerMonth {
		t.Fatalf("explain.hoursPerMonth = %d but pricing.HoursPerMonth = %d", hoursPerMonth, pricing.HoursPerMonth)
	}
}

func TestMicroFromUSD(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want Micro
		err  bool
	}{
		{"zero", 0, 0, false},
		{"one cent", 0.01, 10_000, false},
		{"typical node hour", 0.0928, 92_800, false},
		{"negative", -1.5, -1_500_000, false},
		{"half rounds away from zero", 0.0000005, 1, false},
		{"negative half rounds away from zero", -0.0000005, -1, false},
		{"sub-micro truncates", 0.0000004, 0, false},
		{"at the ceiling", MaxUSD, maxMicro, false},
		{"over the ceiling", MaxUSD * 1.001, 0, true},
		{"under the floor", -MaxUSD * 1.001, 0, true},
		{"NaN", math.NaN(), 0, true},
		{"+Inf", math.Inf(1), 0, true},
		{"-Inf", math.Inf(-1), 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MicroFromUSD(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("MicroFromUSD(%v) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MicroFromUSD(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("MicroFromUSD(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestMicroConversionsAreDerived(t *testing.T) {
	m := Micro(123_456)
	if got := m.USD(); got != 0.123456 {
		t.Errorf("USD() = %v, want 0.123456", got)
	}
	if got := m.MonthlyUSD(); math.Abs(got-90.122880) > 1e-9 {
		t.Errorf("MonthlyUSD() = %v, want 90.12288", got)
	}
}

func TestMoneyArithmeticRefusesToSaturate(t *testing.T) {
	if _, err := add(maxMicro, 1); err == nil {
		t.Error("add past the ceiling must error, not saturate")
	}
	if _, err := add(-maxMicro, -1); err == nil {
		t.Error("add past the floor must error, not saturate")
	}
	if _, err := sub(-maxMicro, 1); err == nil {
		t.Error("sub past the floor must error")
	}
	if _, err := mul(maxMicro, 2); err == nil {
		t.Error("mul past the ceiling must error")
	}
	if _, err := mul(math.MinInt64, -1); err == nil {
		t.Error("mul of the int64 minimum must error rather than wrap to itself")
	}
	if got, err := mul(0, math.MaxInt64); err != nil || got != 0 {
		t.Errorf("mul(0, huge) = %d, %v; want 0 with no error", got, err)
	}
	if got, err := mul(1_000, 0); err != nil || got != 0 {
		t.Errorf("mul(x, 0) = %d, %v; want 0 with no error", got, err)
	}
	if _, err := sumMicro([]Micro{maxMicro, maxMicro}); err == nil {
		t.Error("sumMicro past the ceiling must error")
	}
}

func TestQuantize(t *testing.T) {
	cases := []struct {
		in   float64
		want Micro
		err  bool
	}{
		{0.4, 0, false},
		{0.5, 1, false},
		{-0.5, -1, false},
		{-0.4, 0, false},
		{1e14, 100_000_000_000_000, false},
		{math.NaN(), 0, true},
		{math.Inf(-1), 0, true},
		{float64(maxMicro) * 2, 0, true},
	}
	for _, tc := range cases {
		got, err := quantize(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("quantize(%v) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("quantize(%v): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("quantize(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestSumSortedIsOrderIndependent is the direct regression for the trap this
// package's fixed-point design exists to avoid: an unsorted float sum is a
// function of arrival order, not of its inputs.
func TestSumSortedIsOrderIndependent(t *testing.T) {
	vals := []float64{1e16, 1.0, -1e16, 0.5, 3.25, -2.75, 1e-9, 7e15, -7e15}
	want := sumSorted(vals)
	rng := rand.New(rand.NewSource(7))
	naiveDiffered := false
	for i := 0; i < 500; i++ {
		shuffled := append([]float64(nil), vals...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := sumSorted(shuffled); got != want {
			t.Fatalf("sumSorted changed with input order: %v vs %v", got, want)
		}
		naive := 0.0
		for _, v := range shuffled {
			naive += v
		}
		if naive != want {
			naiveDiffered = true
		}
	}
	if !naiveDiffered {
		t.Fatal("fixture is useless: naive left-to-right summation never disagreed, so it cannot demonstrate the hazard")
	}
}

func TestSumSortedDoesNotMutateItsInput(t *testing.T) {
	in := []float64{3, 1, 2}
	sumSorted(in)
	if !sort.SliceIsSorted(in, func(i, j int) bool { return i < j }) || in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Errorf("sumSorted mutated its argument: %v", in)
	}
}

func TestSumSortedEmpty(t *testing.T) {
	if got := sumSorted(nil); got != 0 {
		t.Errorf("sumSorted(nil) = %v, want 0", got)
	}
}

func TestSumMicroIsOrderIndependentByConstruction(t *testing.T) {
	vals := []Micro{5, -3, 1_000_000, -999_999, 42}
	want, err := sumMicro(vals)
	if err != nil {
		t.Fatalf("sumMicro: %v", err)
	}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 200; i++ {
		s := append([]Micro(nil), vals...)
		rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
		got, err := sumMicro(s)
		if err != nil {
			t.Fatalf("sumMicro: %v", err)
		}
		if got != want {
			t.Fatalf("sumMicro changed with order: %d vs %d", got, want)
		}
	}
}
