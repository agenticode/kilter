package crossover

import (
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// maxFuzzPods bounds the packing work per execution so the fuzzer explores
// input shapes rather than one enormous bin-packing run.
const maxFuzzPods = 48

// decodePods turns fuzz bytes into a pod set: 6 bytes per pod give a request
// shape and a fact vector, so both the pricing paths and every gate are
// reachable from the corpus.
func decodePods(data []byte) []Pod {
	var pods []Pod
	for i := 0; i+5 < len(data) && len(pods) < maxFuzzPods; i += 6 {
		mcpu := int64(data[i])<<4 | int64(data[i+1]) // 0 … ~4335 m
		mem := int64(data[i+2]) << 4                 // 0 … 4080 MiB
		if data[i+3]&0x80 != 0 {                     // occasionally, something absurd
			mcpu = math.MaxInt64 / 4
			mem = math.MaxInt64 / 4 >> 20
		}
		p := mkPod("", mcpu, mem)
		p.Spec.Name = string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		p.Spec.UID = p.Spec.Name
		p.Facts = decodeFacts(data[i+4], data[i+5])
		if data[i+3]&0x01 != 0 {
			p.Spec.Workload.Kind = model.KindDaemonSet
		}
		if data[i+3]&0x02 != 0 {
			p.Spec.Phase = "Succeeded"
		}
		pods = append(pods, p)
	}
	return pods
}

// decodeFacts maps two bytes onto the nine gate facts, two bits each, so the
// corpus reaches Unknown, Absent and Present on every gate.
func decodeFacts(a, b byte) Facts {
	bits := uint32(a) | uint32(b)<<8
	next := func(i uint) Fact {
		switch (bits >> (2 * i)) & 0x3 {
		case 0:
			return Unknown
		case 1, 2:
			return Absent
		default:
			return Present
		}
	}
	// Eight fact fields come from the 16 bits; the ninth follows the parity of
	// the pair so it is neither constant nor correlated with a single bit.
	ninth := Absent
	if (a^b)&0x01 != 0 {
		ninth = Present
	}
	return Facts{
		DaemonSet: next(0), ExtendedResource: next(1), EBSVolume: next(2),
		Privileged: next(3), HostPath: next(4), HostNetwork: next(5),
		HostPort: next(6), NoPrivateSubnet: next(7), EvictionIntolerant: ninth,
	}
}

// FuzzReportNeverRecommendsBlockedFargate is the safety property of this whole
// unit: whatever the input, a pod set with a standing §4.3 block is never
// recommended for Fargate, never carries a saving, and never reads as though
// Fargate were the cheaper choice.
func FuzzReportNeverRecommendsBlockedFargate(f *testing.F) {
	f.Add([]byte{0x0C, 0x80, 0x20, 0x00, 0x55, 0x55, 0x0C, 0x80, 0x20, 0x00, 0x55, 0x55}, uint8(0), false)
	f.Add([]byte{0x3E, 0x80, 0x00, 0x00, 0xFF, 0xFF}, uint8(2), false)
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, uint8(0), true)
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0x80, 0xAA, 0xAA}, uint8(1), false)
	f.Add([]byte{}, uint8(3), false)
	f.Add([]byte{0x40, 0x00, 0x40, 0x02, 0x55, 0x55, 0x40, 0x00, 0x40, 0x01, 0x55, 0x55}, uint8(0), false)

	f.Fuzz(func(t *testing.T, data []byte, nDS uint8, spot bool) {
		pods := decodePods(data)
		var ds []model.PodSpec
		for i := 0; i < int(nDS%4); i++ {
			ds = append(ds, model.PodSpec{
				UID: "ds" + string(rune('a'+i)), Name: "ds" + string(rune('a'+i)), Namespace: "kube-system",
				Workload:   model.WorkloadRef{Kind: model.KindDaemonSet, Namespace: "kube-system", Name: "ds" + string(rune('a'+i))},
				Containers: []model.ContainerSpec{{Requests: model.Resources{MilliCPU: int64(50 * (i + 1)), MemoryBytes: int64(64*(i+1)) * mib}}},
			})
		}
		set := PodSet{Pods: pods, DaemonSets: ds}
		opts := Options{Candidates: []pricing.InstanceType{m5large, m5xlarge, m52xlarge, c5large, m7g2xlarge}, Spot: spot}
		rep := Analyze(testNow, set, opts)

		// 1. The safety property.
		if len(rep.Blocks) > 0 {
			if rep.Verdict == VerdictFargate {
				t.Fatalf("blocked pod set recommended for Fargate: %+v", rep.Blocks)
			}
			if rep.Fargate.Eligible {
				t.Fatalf("blocked pod set marked eligible: %+v", rep.Blocks)
			}
			if rep.MonthlySavingsUSD != 0 || rep.SavingsFraction != 0 || rep.Close {
				t.Fatalf("blocked pod set claimed savings %v (%v, close=%v)",
					rep.MonthlySavingsUSD, rep.SavingsFraction, rep.Close)
			}
			if h := rep.Headline(); strings.Contains(h, "cheaper") {
				t.Fatalf("blocked headline reads as price advice: %q", h)
			}
		}
		// 2. …and its converse: a Fargate verdict implies nothing blocked.
		if rep.Verdict == VerdictFargate && (len(rep.Blocks) > 0 || !rep.Fargate.Eligible) {
			t.Fatalf("Fargate verdict with blocks=%d eligible=%v", len(rep.Blocks), rep.Fargate.Eligible)
		}
		// 3. Every gate reported is a gate that exists, in AllGates order.
		order := map[Gate]int{}
		for i, g := range AllGates() {
			order[g] = i
		}
		prev := -1
		for _, b := range rep.Blocks {
			idx, ok := order[b.Gate]
			if !ok {
				t.Fatalf("unknown gate %q in report", b.Gate)
			}
			if idx < prev {
				t.Fatalf("blocks out of order: %q after index %d", b.Gate, prev)
			}
			prev = idx
			if b.Reason == "" {
				t.Fatalf("gate %q blocked without a reason", b.Gate)
			}
			if b.Kind != BlockViolation && b.Kind != BlockUnverified {
				t.Fatalf("gate %q has kind %q", b.Gate, b.Kind)
			}
		}
		// 4. Money is finite, non-negative, and never claimed backwards.
		for name, v := range map[string]float64{
			"fargate hourly": rep.Fargate.HourlyUSD, "fargate monthly": rep.Fargate.MonthlyUSD,
			"ec2 hourly": rep.EC2.HourlyUSD, "ec2 monthly": rep.EC2.MonthlyUSD,
			"savings": rep.MonthlySavingsUSD, "savings fraction": rep.SavingsFraction,
			"achieved density": rep.Density.Achieved, "break-even density": rep.Density.BreakEven,
		} {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				t.Fatalf("%s = %v: money and ratios must be finite and non-negative", name, v)
			}
		}
		// 5. Accounting: every pod is either on a tier or explicitly unpriced.
		priced := 0
		for _, c := range rep.Fargate.Configs {
			priced += c.Pods
		}
		if priced+len(rep.Fargate.Unpriced) != rep.Fargate.Pods {
			t.Fatalf("pod accounting: %d priced + %d unpriced != %d pods",
				priced, len(rep.Fargate.Unpriced), rep.Fargate.Pods)
		}
		// 6. Rendering never panics, and always states the verdict.
		if s := rep.Summary(); !strings.Contains(s, rep.Headline()) {
			t.Fatalf("summary omits its own headline")
		}
		if in := rep.Insight(); in.Kind != InsightKind || in.At != testNow || in.Severity != "info" {
			t.Fatalf("insight = %+v, want an informational %s at the caller's clock", in, InsightKind)
		}
		// 7. Determinism under shuffling.
		shuffled := append([]Pod(nil), pods...)
		rng := rand.New(rand.NewSource(int64(len(data)*31 + int(nDS))))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		again := Analyze(testNow, PodSet{Pods: shuffled, DaemonSets: ds}, opts)
		if !reflect.DeepEqual(again, rep) {
			t.Fatalf("shuffling the input changed the report")
		}
	})
}
