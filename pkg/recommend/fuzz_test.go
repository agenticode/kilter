package recommend

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/agenticode/kilter/pkg/model"
)

// runInvariantScenario builds a pseudo-random workload from seed and checks
// the decision-safety invariants that must hold for ANY input:
//
//   - targets never fall below configured floors
//   - the memory target covers every observed sample (never size below peak)
//   - after an OOM the memory target honors the bumped floor
//   - an existing limit is never dropped and never sits below the request
//   - a missing limit is never invented
//   - HPA-on-CPU leaves the CPU request byte-identical
//   - confidence is a real number in [0,1]
func runInvariantScenario(t *testing.T, seed int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	cfg := DefaultConfig()
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ref := deployRef(fmt.Sprintf("fuzz-%d", seed&0xffff))

	curReq := model.Resources{MilliCPU: rng.Int63n(4001), MemoryBytes: rng.Int63n(8 << 30)}
	var curLim model.Resources
	switch rng.Intn(3) {
	case 0: // no limits
	case 1: // sane limits: 1–3× the request
		curLim = model.Resources{
			MilliCPU:    curReq.MilliCPU * (1 + rng.Int63n(3)),
			MemoryBytes: curReq.MemoryBytes * (1 + rng.Int63n(3)),
		}
	case 2: // garbage limits, possibly below the request
		curLim = model.Resources{MilliCPU: rng.Int63n(4001), MemoryBytes: rng.Int63n(8 << 30)}
	}

	hours := 7 + rng.Intn(41)
	baseCPU := rng.Int63n(2000)
	baseMem := rng.Int63n(4 << 30)
	var maxMem int64
	snap := mkSnap(ref, curReq, curLim, hours,
		func(i int) int64 { return baseCPU + rng.Int63n(500) },
		func(i int) int64 {
			v := baseMem + rng.Int63n(1<<30)
			if v > maxMem {
				maxMem = v
			}
			return v
		},
	)
	hpaOnCPU := rng.Intn(4) == 0
	if hpaOnCPU {
		snap.Workloads = []model.WorkloadInfo{{Ref: ref, HasHPA: true, HPATargetsCPU: true}}
	}
	r.ObserveSnapshot(snap)

	var oomFloor int64
	if rng.Intn(3) == 0 && (curLim.MemoryBytes > 0 || curReq.MemoryBytes > 0) {
		snap.Pods[0].Containers[0].RestartCount = 1
		snap.Pods[0].Containers[0].LastOOMKilled = true
		r.ObserveSnapshot(snap)
		oomedAt := curLim.MemoryBytes
		if oomedAt == 0 {
			oomedAt = curReq.MemoryBytes
		}
		oomFloor = ceilInt64(float64(oomedAt) * cfg.OOMBumpRatio)
	}

	for _, rec := range r.Recommendations(snap) {
		if math.IsNaN(rec.Confidence) || rec.Confidence < 0 || rec.Confidence > 1 {
			t.Fatalf("seed %d: confidence %v out of [0,1]", seed, rec.Confidence)
		}
		if rec.CPUSkipped {
			if rec.TargetRequest.MilliCPU != curReq.MilliCPU {
				t.Fatalf("seed %d: HPA-on-CPU changed cpu request %d → %d",
					seed, curReq.MilliCPU, rec.TargetRequest.MilliCPU)
			}
		} else if rec.TargetRequest.MilliCPU < cfg.MinMilliCPU {
			t.Fatalf("seed %d: cpu target %dm below floor", seed, rec.TargetRequest.MilliCPU)
		}
		if rec.TargetRequest.MemoryBytes < cfg.MinMemoryBytes {
			t.Fatalf("seed %d: mem target below floor: %d", seed, rec.TargetRequest.MemoryBytes)
		}
		if rec.TargetRequest.MemoryBytes < maxMem {
			t.Fatalf("seed %d: mem target %d below observed peak %d — would OOM on replay",
				seed, rec.TargetRequest.MemoryBytes, maxMem)
		}
		if rec.TargetRequest.MemoryBytes < oomFloor {
			t.Fatalf("seed %d: mem target %d below OOM floor %d",
				seed, rec.TargetRequest.MemoryBytes, oomFloor)
		}
		if curLim.MilliCPU == 0 && rec.TargetLimit.MilliCPU != 0 {
			t.Fatalf("seed %d: invented a cpu limit %d", seed, rec.TargetLimit.MilliCPU)
		}
		if curLim.MemoryBytes == 0 && rec.TargetLimit.MemoryBytes != 0 {
			t.Fatalf("seed %d: invented a mem limit %d", seed, rec.TargetLimit.MemoryBytes)
		}
		if curLim.MilliCPU > 0 && rec.TargetLimit.MilliCPU < rec.TargetRequest.MilliCPU {
			t.Fatalf("seed %d: cpu limit %d below request %d",
				seed, rec.TargetLimit.MilliCPU, rec.TargetRequest.MilliCPU)
		}
		if curLim.MemoryBytes > 0 && rec.TargetLimit.MemoryBytes < rec.TargetRequest.MemoryBytes {
			t.Fatalf("seed %d: mem limit %d below request %d",
				seed, rec.TargetLimit.MemoryBytes, rec.TargetRequest.MemoryBytes)
		}
		if rec.Samples < cfg.MinSamples {
			t.Fatalf("seed %d: emitted with %d samples < MinSamples", seed, rec.Samples)
		}
		if rec.WindowHours < cfg.MinWindow.Hours() {
			t.Fatalf("seed %d: emitted with window %vh < MinWindow", seed, rec.WindowHours)
		}
	}
}

// TestRecommendationInvariantsProperty exercises the invariants across many
// deterministic random scenarios on every plain `go test` run.
func TestRecommendationInvariantsProperty(t *testing.T) {
	for seed := int64(0); seed < 150; seed++ {
		runInvariantScenario(t, seed)
	}
}

func FuzzRecommendationInvariants(f *testing.F) {
	for _, seed := range []int64{1, 42, 1<<40 + 7, -9} {
		f.Add(seed)
	}
	f.Fuzz(runInvariantScenario)
}
