package ec2

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// fuzzTypes are the shapes the fuzzer may start from, spanning burstable and
// fixed, small and large, memory-dense and CPU-dense.
var fuzzTypes = []string{
	"m5.large", "m5.xlarge", "m5.2xlarge", "c5.large", "c5.xlarge",
	"r5.large", "r5.xlarge", "t3.medium", "t3.large", "t3.xlarge",
	"x1e.32xlarge", // deliberately absent from the catalog
}

// FuzzSizerNeverUndersizes is the package's load-bearing property: whatever
// the metrics say, a proposal may never be smaller than something we actually
// observed. It also asserts every other invariant [Report.Validate] encodes —
// the memory-blind floor, net-positive claims, no proposal on a throttled
// instance — and that every assessment states a reason for whatever it did.
func FuzzSizerNeverUndersizes(f *testing.F) {
	f.Add(uint8(1), uint8(0), []byte{5, 6, 7, 90}, []byte{40, 42})
	f.Add(uint8(8), uint8(0b0111), []byte{200, 3, 3, 3}, []byte{})
	f.Add(uint8(3), uint8(0b1010), []byte{255}, []byte{255})
	f.Add(uint8(10), uint8(0b1111), []byte{0, 0, 0}, []byte{0})
	f.Add(uint8(5), uint8(0b0101), []byte{}, []byte{128, 250})

	f.Fuzz(func(t *testing.T, typeSel, flags uint8, cpuSeed, memSeed []byte) {
		if len(cpuSeed) == 0 {
			cpuSeed = []byte{0}
		}
		itype := fuzzTypes[int(typeSel)%len(fuzzTypes)]
		detailedMon := flags&1 != 0
		withMem := flags&2 != 0 && len(memSeed) > 0
		withCredits := flags&4 != 0
		withRI := flags&8 != 0

		period := basic
		r := rec("i-fuzz", itype)
		if detailedMon {
			period = detailP
			r.MonitoringState = "enabled"
		}
		switch (flags >> 4) & 3 {
		case 1:
			r.CPUCredits = CreditModeStandard
		case 2:
			r.CPUCredits = CreditModeUnlimited
		}

		// Ten days at the instance's own publication period, so the window and
		// coverage gates are satisfied and the interesting branches are reached.
		count := int((time.Duration(windowDays) * 24 * time.Hour) / period)
		metrics := []RecordedSeries{
			SyntheticSeries("i-fuzz", MetricCPUUtilization, testNow, period, count, percents(cpuSeed)),
		}
		if withMem {
			metrics = append(metrics,
				SyntheticSeries("i-fuzz", memAgent, testNow, period, count, percents(memSeed)))
		}
		if withCredits {
			metrics = append(metrics,
				SyntheticSeries("i-fuzz", MetricCPUCreditBalance, testNow, period, count, credits(cpuSeed)),
				SyntheticSeries("i-fuzz", MetricCPUSurplusCreditsCharged, testNow, period, count,
					credits(memSeed)),
				SyntheticSeries("i-fuzz", MetricCPUSurplusCreditBalance, testNow, period, count,
					credits(memSeed)),
				SyntheticSeries("i-fuzz", MetricCPUCreditUsage, testNow, period, count, credits(cpuSeed)),
			)
		}

		var inv *commit.Inventory
		if withRI {
			inv = &commit.Inventory{RIs: []commit.ReservedInstance{{
				ID: "ri-fuzz", Count: 1, InstanceType: itype, Region: "us-east-1",
				Platform: commit.PlatformLinux, Tenancy: commit.TenancyDefault,
				EffectiveHourlyUSD: 0.05, Expires: testNow.AddDate(0, 6, 0),
			}}}
		}

		fx := &Fixture{
			InventoryPages: []DescribeInstancesOutput{{Reservations: []Reservation{{
				Instances: []InstanceRecord{r}}}}},
			Metrics: metrics,
		}
		c, err := NewCollector(fx, fx, CollectorConfig{
			Scope: "acct/us-east-1", Region: "us-east-1",
			Window: (windowDays + 1) * 24 * time.Hour, CollectMemory: true,
		})
		if err != nil {
			t.Fatalf("collector: %v", err)
		}
		snap, err := c.Collect(context.Background(), testNow)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		s, err := NewSizer(testCatalog(t), DefaultConfig())
		if err != nil {
			t.Fatalf("sizer: %v", err)
		}
		rep := s.Assess(testNow, snap, inv)

		// Every invariant, on every input.
		if err := rep.Validate(); err != nil {
			t.Fatalf("invariant violated (%s, flags %04b, cpu %v, mem %v): %v",
				itype, flags, cpuSeed, memSeed, err)
		}
		if len(rep.Assessments) != 1 {
			t.Fatalf("got %d assessments, want 1", len(rep.Assessments))
		}
		a := rep.Assessments[0]
		if a.Proposal == nil && len(a.Suppressions) == 0 {
			t.Fatal("no proposal and no reason")
		}
		if a.Proposal == nil {
			return
		}
		// Restated here, independently of Validate, because this is the
		// property the unit is judged on.
		if a.Proposal.Spec.Resources.MilliCPU < a.Observation.PeakCPUMilli {
			t.Fatalf("proposed %d mCPU below observed peak %d mCPU (%s → %s)",
				a.Proposal.Spec.Resources.MilliCPU, a.Observation.PeakCPUMilli,
				itype, a.Proposal.InstanceType)
		}
		if a.Observation.MemoryBlind {
			if a.Proposal.Spec.Resources.MemoryBytes < a.Current.Resources.MemoryBytes {
				t.Fatalf("memory-blind proposal cut memory %s → %s",
					gib(a.Current.Resources.MemoryBytes), gib(a.Proposal.Spec.Resources.MemoryBytes))
			}
		} else if a.Proposal.Spec.Resources.MemoryBytes < a.Observation.PeakMemoryBytes {
			t.Fatalf("proposed %s memory below observed peak %s",
				gib(a.Proposal.Spec.Resources.MemoryBytes), gib(a.Observation.PeakMemoryBytes))
		}
		if a.Proposal.NetSavingsMonthlyUSD <= 0 {
			t.Fatalf("proposal claims %v/mo", a.Proposal.NetSavingsMonthlyUSD)
		}
	})
}

// percents maps fuzz bytes onto the 0..100 range CloudWatch reports.
func percents(b []byte) []float64 {
	out := make([]float64, len(b))
	for i, v := range b {
		out[i] = float64(v) * 100 / 255
	}
	return out
}

// credits maps fuzz bytes onto a plausible credit-balance range, including
// zero (the depleted case the sizer must refuse to size from).
func credits(b []byte) []float64 {
	if len(b) == 0 {
		return []float64{0}
	}
	out := make([]float64, len(b))
	for i, v := range b {
		out[i] = float64(v) * 4
	}
	return out
}

// FuzzFixtureRoundTrip asserts the recorded-fixture loader survives arbitrary
// bytes: a fixture is test infrastructure, and infrastructure that panics on
// bad input hides the failures it was built to surface.
func FuzzFixtureRoundTrip(f *testing.F) {
	f.Add([]byte(`{"inventoryPages":[{"reservations":[]}]}`))
	f.Add([]byte(`{"metrics":[{"instanceId":"i-1","metric":"CPUUtilization"}]}`))
	f.Add([]byte(`{"metricPageSize":-1}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		fx, err := LoadFixture(bytesReader(data))
		if err != nil {
			return
		}
		c, err := NewCollector(fx, fx, CollectorConfig{Window: time.Hour})
		if err != nil {
			t.Fatalf("collector: %v", err)
		}
		snap, err := c.Collect(context.Background(), testNow)
		if err != nil {
			return // an error is a fine outcome; a panic is not
		}
		if snap == nil {
			t.Fatal("nil snapshot with nil error")
		}
		s, err := NewSizer(testCatalog(t), DefaultConfig())
		if err != nil {
			t.Fatalf("sizer: %v", err)
		}
		if err := s.Assess(testNow, snap, nil).Validate(); err != nil {
			t.Fatalf("invariant violated on fixture-derived snapshot: %v", err)
		}
	})
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
