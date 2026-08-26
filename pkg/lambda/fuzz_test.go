package lambda

import (
	"math"
	"testing"
	"time"
)

// The REPORT line is a log format, not an API, and this package's evidence
// intake is the first thing an attacker-shaped or simply corrupt log reaches.
// The property: ParseReport never panics, and any record it DOES return is
// plausible — no NaN, no infinity, no negative duration or memory, nothing
// outside the documented platform limits. A wrong number here becomes a wrong
// memory floor, and a wrong memory floor is an out-of-memory error.
func FuzzParseReport(f *testing.F) {
	f.Add(warmLine)
	f.Add(coldLine)
	f.Add("REPORT RequestId: a Duration: 1.20 ms Billed Duration: 2 ms Memory Size: 128 MB " +
		"Max Memory Used: 74 MB")
	f.Add("REPORT RequestId: a\tDuration: 1e400 ms\tBilled Duration: 2 ms\tMemory Size: 128 MB\t" +
		"Max Memory Used: 74 MB\t")
	f.Add("REPORT Duration: -0 ms Billed Duration: -0 ms Memory Size: -128 MB Max Memory Used: -1 MB")
	f.Add("REPORT Duration: Duration: Duration: ms Memory Size: Memory Size: MB")
	f.Add("REPORT RequestId: \t\t\t Init Duration: NaN ms Restore Duration: +Inf ms")
	f.Add("REPORT\tMax Memory Used: 10 MB\tMemory Size: 128 MB\tBilled Duration: 1 ms\tDuration: 1 ms")
	f.Add("")
	f.Add("REPORT")

	f.Fuzz(func(t *testing.T, line string) {
		rec, err := ParseReport(line)
		if err != nil {
			// A refusal must always carry a code, so no drop is ever silent.
			pe, ok := err.(*ParseError)
			if !ok || pe.Code == "" {
				t.Fatalf("parse failure without a reason code: %v", err)
			}
			if rec != (ReportRecord{}) {
				t.Fatalf("a dropped line must yield the zero record, got %+v", rec)
			}
			return
		}

		for name, v := range map[string]float64{
			"Duration":        rec.DurationMS,
			"BilledDuration":  rec.BilledDurationMS,
			"InitDuration":    rec.InitDurationMS,
			"RestoreDuration": rec.RestoreDurationMS,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s is not finite (%v) for %q", name, v, line)
			}
			if v < 0 {
				t.Fatalf("%s is negative (%v) for %q", name, v, line)
			}
			if v > float64(MaxTimeoutSeconds)*1000 {
				t.Fatalf("%s exceeds the platform maximum (%v) for %q", name, v, line)
			}
		}
		if rec.MemorySizeMB < MinMemoryMB || rec.MemorySizeMB > MaxMemoryMB {
			t.Fatalf("MemorySizeMB %d is outside the platform range for %q", rec.MemorySizeMB, line)
		}
		if rec.MaxMemoryUsedMB < 0 || rec.MaxMemoryUsedMB > rec.MemorySizeMB {
			t.Fatalf("MaxMemoryUsedMB %d is implausible against %d for %q",
				rec.MaxMemoryUsedMB, rec.MemorySizeMB, line)
		}
		// The derived quantities the cost model consumes must be finite too.
		if gb := GBSeconds(rec.MemorySizeMB, rec.BilledDurationMS); math.IsNaN(gb) || math.IsInf(gb, 0) || gb < 0 {
			t.Fatalf("GBSeconds derived %v from %+v", gb, rec)
		}
	})
}

// Whatever a log group contains, a report built from it must satisfy every
// invariant this package documents — above all: no proposal without a
// measurement at the proposed memory setting.
func FuzzReportInvariants(f *testing.F) {
	f.Add(uint8(1), uint16(700), uint16(700), uint8(100), uint8(150), uint8(40), uint8(0))
	f.Add(uint8(4), uint16(0), uint16(1500), uint8(10), uint8(255), uint8(255), uint8(3))
	f.Add(uint8(0), uint16(300), uint16(0), uint8(0), uint8(0), uint8(0), uint8(0))
	f.Add(uint8(7), uint16(900), uint16(900), uint8(250), uint8(100), uint8(200), uint8(5))

	f.Fuzz(func(t *testing.T, memSel uint8, nA, nB uint16, billedA, billedB, usedPct, coldEvery uint8) {
		mems := []int64{128, 256, 512, 768, 1024, 1536, 2048, 3008}
		curMem := mems[int(memSel)%len(mems)]
		altMem := mems[(int(memSel)+3)%len(mems)]
		usedA := curMem * int64(usedPct) / 255
		usedB := altMem * int64(usedPct) / 255

		pts := []point{
			{memoryMB: curMem, maxUsedMB: usedA, billedMS: float64(billedA) + 1,
				n: int(nA % 2000), coldEvery: int(coldEvery), initMS: 300},
			{memoryMB: altMem, maxUsedMB: usedB, billedMS: float64(billedB) + 1,
				n: int(nB % 2000), coldEvery: int(coldEvery), initMS: 300},
		}
		tgt := target(fn(curMem), events(testSpan, pts...))
		s, err := NewSizer(DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		rep := s.Assess(testNow, snapOf(testSpan, tgt), nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("report invariants violated: %v", err)
		}
		a, ok := rep.For(tgt.Ref.ID)
		if !ok {
			t.Fatalf("no assessment produced")
		}
		// Silence is never an output.
		if a.Proposal == nil && len(a.Suppressions) == 0 {
			t.Fatalf("neither a proposal nor a reason")
		}
		if a.Proposal == nil {
			return
		}
		// THE RULE, restated as a property.
		p, measured := a.Observation.PointAt(a.Proposal.MemoryMB)
		if !measured || p.Warm < DefaultConfig().MinSamplesPerPoint {
			t.Fatalf("proposed %s without an adequate measurement there", fmtMB(a.Proposal.MemoryMB))
		}
		if a.Proposal.MemoryMB < a.Observation.MemoryFloorMB {
			t.Fatalf("proposed %s below the %s floor",
				fmtMB(a.Proposal.MemoryMB), fmtMB(a.Observation.MemoryFloorMB))
		}
		if a.Proposal.NetSavingsMonthlyUSD <= 0 {
			t.Fatalf("proposal claims %v", a.Proposal.NetSavingsMonthlyUSD)
		}
	})
}

// A degenerate window must not divide by zero or invent a rate.
func TestZeroWindowProducesNoRate(t *testing.T) {
	tgt := target(fn(1024), events(testSpan, point{memoryMB: 1024, maxUsedMB: 400, billedMS: 100, n: 700}))
	snap := snapOf(0, tgt)
	snap.Window = Window{Start: testNow, End: testNow.Add(-time.Hour)} // End before Start
	s, err := NewSizer(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	rep := s.Assess(testNow, snap, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("report invariants violated: %v", err)
	}
}
