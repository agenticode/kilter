package model

import (
	"math"
	"math/big"
	"testing"
)

// clampedBig computes op(a, b) in arbitrary precision and clamps to int64 —
// the reference semantics for saturating Resources arithmetic.
func clampedBig(a, b int64, op func(z, x, y *big.Int) *big.Int) int64 {
	z := op(new(big.Int), big.NewInt(a), big.NewInt(b))
	if z.IsInt64() {
		return z.Int64()
	}
	if z.Sign() > 0 {
		return math.MaxInt64
	}
	return math.MinInt64
}

func FuzzResourcesArithmetic(f *testing.F) {
	f.Add(int64(500), int64(1<<30), int64(250), int64(1<<29))
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(1), int64(1))
	f.Add(int64(math.MinInt64), int64(math.MinInt64), int64(-1), int64(-1))
	f.Add(int64(0), int64(0), int64(math.MinInt64), int64(math.MinInt64))
	f.Fuzz(func(t *testing.T, aCPU, aMem, bCPU, bMem int64) {
		a := Resources{MilliCPU: aCPU, MemoryBytes: aMem}
		b := Resources{MilliCPU: bCPU, MemoryBytes: bMem}

		wantAdd := Resources{
			MilliCPU:    clampedBig(aCPU, bCPU, (*big.Int).Add),
			MemoryBytes: clampedBig(aMem, bMem, (*big.Int).Add),
		}
		if got := a.Add(b); got != wantAdd {
			t.Fatalf("Add(%+v, %+v) = %+v, want %+v", a, b, got, wantAdd)
		}
		if got := b.Add(a); got != wantAdd {
			t.Fatalf("Add not commutative: %+v vs %+v", b.Add(a), wantAdd)
		}

		wantSub := Resources{
			MilliCPU:    clampedBig(aCPU, bCPU, (*big.Int).Sub),
			MemoryBytes: clampedBig(aMem, bMem, (*big.Int).Sub),
		}
		if got := a.Sub(b); got != wantSub {
			t.Fatalf("Sub(%+v, %+v) = %+v, want %+v", a, b, got, wantSub)
		}

		// Max is a commutative element-wise upper bound both inputs fit into.
		mx := a.Max(b)
		if mx != b.Max(a) {
			t.Fatalf("Max not commutative: %+v vs %+v", mx, b.Max(a))
		}
		if !mx.Fits(a) || !mx.Fits(b) {
			t.Fatalf("Max(%+v, %+v) = %+v must contain both inputs", a, b, mx)
		}
		if mx != mx.Max(a) {
			t.Fatalf("Max not idempotent: %+v", mx)
		}
	})
}

// refTolerates restates the documented toleration semantics in a different
// shape (operator-first instead of key-first) so the fuzzer can catch a
// refactor that drifts from the contract.
func refTolerates(tol Toleration, taint Taint) bool {
	if tol.Effect != "" && tol.Effect != taint.Effect {
		return false
	}
	switch tol.Operator {
	case "Exists":
		return tol.Key == "" || tol.Key == taint.Key
	case "Equal", "":
		return tol.Key != "" && tol.Key == taint.Key && tol.Value == taint.Value
	default:
		return false
	}
}

func FuzzTolerates(f *testing.F) {
	f.Add("dedicated", "Equal", "gpu", "NoSchedule", "dedicated", "gpu", "NoSchedule")
	f.Add("", "Exists", "", "", "any", "any", "NoExecute")
	f.Add("k", "Bogus", "v", "", "k", "v", "NoSchedule")
	f.Fuzz(func(t *testing.T, tKey, tOp, tVal, tEff, key, val, eff string) {
		tol := Toleration{Key: tKey, Operator: tOp, Value: tVal, Effect: tEff}
		taint := Taint{Key: key, Value: val, Effect: eff}
		got := tol.Tolerates(taint)
		if want := refTolerates(tol, taint); got != want {
			t.Fatalf("Tolerates(%+v, %+v) = %v, reference says %v", tol, taint, got, want)
		}
		// Operators outside the contract must never tolerate anything.
		switch tOp {
		case "Exists", "Equal", "":
		default:
			if got {
				t.Fatalf("unknown operator %q tolerated %+v", tOp, taint)
			}
		}
	})
}
