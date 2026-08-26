package fargate

import (
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// FuzzRecommendNetNeverExceedsGross drives the whole projection — quantizer,
// sizing policy, shave gates, commitment waterfall — with arbitrary pod shapes
// and arbitrary commitment inventories, and asserts the invariants that must
// hold for EVERY recommendation this domain can ever emit:
//
//	Net ≤ Gross            — commitments only ever make a change worth less;
//	Net, Gross finite      — garbage arithmetic never reaches a savings claim;
//	Action == rolling      — Fargate cannot resize in place;
//	claim ≤ max(0, Gross)  — nothing is claimed above the list-price delta;
//	Validate() == nil      — evidence present, no null change, ranges sane;
//	the proposed tier is a real Fargate configuration.
func FuzzRecommendNetNeverExceedsGross(f *testing.F) {
	f.Add(int64(1000), int64(8<<30), int64(400), int64(7<<30), int64(0), 1, 300, 0.0)
	f.Add(int64(200), int64(512<<20), int64(50), int64(100<<20), int64(0), 3, 300, 10.0)
	f.Add(int64(16000), int64(120<<30), int64(1), int64(1), int64(0), 1, 300, 0.5)
	f.Add(int64(0), int64(0), int64(0), int64(0), int64(0), 1, 0, 0.0)
	f.Add(int64(-5), int64(-5), int64(-5), int64(-5), int64(-5), -1, -1, -1.0)
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64),
		int64(math.MaxInt64), 100, 1000, math.MaxFloat64)
	f.Add(int64(2000), int64(9<<30), int64(1900), int64(9<<30), int64(4<<30), 2, 200, 1e6)

	f.Fuzz(func(t *testing.T, cpuReq, memReq, cpuUse, memUse, initMem int64,
		replicas, samples int, spCommit float64) {

		replicas = 1 + abs(replicas)%4
		samples = abs(samples) % 400
		w := wl{
			name:     "fuzz",
			replicas: replicas,
			init:     model.Resources{MemoryBytes: initMem},
			containers: []ctr{
				{name: "app", cpuReq: cpuReq, memReq: memReq, cpuUse: cpuUse, memUse: memUse},
				{name: "side", cpuReq: cpuReq / 4, memReq: memReq / 4, cpuUse: cpuUse / 4, memUse: memUse / 4},
			},
		}
		d := newDomain(t)
		snap := cluster(now, samples, 72*time.Hour, w)
		if err := d.Learn(&domain.Snapshot{Domain: Kind, Timestamp: now, Cluster: snap}); err != nil {
			t.Fatalf("Learn: %v", err)
		}

		var led domain.Netter
		if spCommit > 0 && !math.IsInf(spCommit, 0) {
			led = domain.NewLedger(&commit.Inventory{SavingsPlans: []commit.SavingsPlan{
				{ID: "sp", Type: commit.SPCompute, CommitmentUSDPerHour: spCommit},
			}}, commit.Usage{})
		}

		for _, r := range d.Recommend(now, led) {
			if err := r.Validate(); err != nil {
				t.Fatalf("invalid recommendation: %v (%+v)", err, r)
			}
			if r.NetSavingsMonthlyUSD > r.GrossSavingsMonthlyUSD {
				t.Fatalf("net $%v exceeds gross $%v", r.NetSavingsMonthlyUSD, r.GrossSavingsMonthlyUSD)
			}
			for name, v := range map[string]float64{
				"gross": r.GrossSavingsMonthlyUSD, "net": r.NetSavingsMonthlyUSD,
				"currentHourly": r.CurrentHourlyUSD, "proposedHourly": r.ProposedHourlyUSD,
			} {
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("%s is non-finite: %v", name, v)
				}
			}
			if r.CurrentHourlyUSD < 0 || r.ProposedHourlyUSD < 0 {
				t.Fatalf("negative price: %v → %v", r.CurrentHourlyUSD, r.ProposedHourlyUSD)
			}
			if r.Action != domain.ActionRolling {
				t.Fatalf("action %q; every Fargate resize is rolling", r.Action)
			}
			if c := r.ClaimableMonthlyUSD(); c > math.Max(0, r.GrossSavingsMonthlyUSD) {
				t.Fatalf("claims $%v against a gross of $%v", c, r.GrossSavingsMonthlyUSD)
			}
			// Both specs name real Fargate tiers, and the priced Resources
			// agree with the tier attribute.
			for _, s := range []domain.Spec{r.Current, r.Proposed} {
				cfg, err := pricing.ParseCapacityProvisioned(s.Attr(AttrTier))
				if err != nil {
					t.Fatalf("spec tier %q is not a Fargate configuration: %v", s.Attr(AttrTier), err)
				}
				if cfg.Resources() != s.Resources {
					t.Fatalf("spec Resources %s disagree with tier %s", s.Resources, cfg)
				}
			}
			// A shave never raises a request and never proposes more than the
			// tier boundary allows.
			if r.Proposed.Attr(AttrChange) == ChangeBoundaryShave {
				cs, err := Containers(r.Proposed)
				if err != nil {
					t.Fatalf("proposal is undecodable: %v", err)
				}
				cur, err := Containers(r.Current)
				if err != nil {
					t.Fatal(err)
				}
				for i := range cs {
					if cs[i].Requests.MilliCPU != cur[i].Requests.MilliCPU {
						t.Fatalf("a boundary shave changed CPU: %+v → %+v", cur[i], cs[i])
					}
					if cs[i].Requests.MemoryBytes > cur[i].Requests.MemoryBytes {
						t.Fatalf("a boundary shave raised memory: %+v → %+v", cur[i], cs[i])
					}
				}
				if r.ProposedHourlyUSD >= r.CurrentHourlyUSD {
					t.Fatalf("a boundary shave did not save money: %v → %v",
						r.CurrentHourlyUSD, r.ProposedHourlyUSD)
				}
			}
		}
	})
}

// FuzzPlanStepsNeverEmitsInPlace: whatever recommendations reach PlanSteps, the
// executor either gets rolling steps or an error — never a step that
// understates its own disruption.
func FuzzPlanStepsNeverEmitsInPlace(f *testing.F) {
	f.Add(uint8(0), false, false, 0)
	f.Add(uint8(3), true, false, 2)
	f.Add(uint8(1), false, true, 1)
	f.Fuzz(func(t *testing.T, action uint8, freeze, breaker bool, maxSteps int) {
		classes := []domain.ActionClass{
			domain.ActionRolling, domain.ActionInPlace, domain.ActionStopStart,
			domain.ActionAdvisory, domain.ActionClass("nonsense"),
		}
		d := newDomain(t, noPolicyMoves)
		learn(t, d, cluster(now, 300, 72*time.Hour, cliff()))
		recs := d.Recommend(now, nil)
		if len(recs) == 0 {
			t.Skip("fixture produced nothing")
		}
		recs[0].Action = classes[int(action)%len(classes)]

		steps, err := d.PlanSteps(recs, domain.Guard{
			Now: now, Freeze: freeze, BreakerOpen: breaker, MaxSteps: abs(maxSteps) % 5})
		if err != nil {
			return // refused, which is always an acceptable outcome
		}
		for _, s := range steps {
			if s.Action != domain.ActionRolling {
				t.Fatalf("plan contains a %q step", s.Action)
			}
			if s.Key == "" {
				t.Fatal("step has no idempotency key")
			}
		}
	})
}

func abs(i int) int {
	if i < 0 {
		if i == math.MinInt {
			return math.MaxInt
		}
		return -i
	}
	return i
}
