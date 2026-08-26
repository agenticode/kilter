package fargate

import (
	"math"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
)

var now = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// rate reprices a tier straight from the published per-hour rates, so a test
// failure distinguishes "the recommendation picked the wrong tier" from "the
// arithmetic drifted".
func rate(vcpu, memGB float64) float64 {
	return vcpu*pricing.FargateVCPUHourlyUSD + memGB*pricing.FargateGBHourlyUSD
}

// TestOverheadCliffBoundaryShave is the AWS worked example of §4.1.1 driven end
// to end through the domain: a pod asking for 1 vCPU / 8 GB is billed
// 2 vCPU / 9 GB because Fargate adds 256 MB before rounding, and shaving the
// memory request by that 256 MB drops a whole tier.
//
// This is the single highest-yield Fargate optimization and the one a
// node-centric pipeline structurally cannot see: nothing about the pod's
// utilization changed, only which side of a billing boundary it sits on.
func TestOverheadCliffBoundaryShave(t *testing.T) {
	d := newDomain(t, noPolicyMoves)
	snap := cluster(now, 300, 72*time.Hour, wl{
		name: "api", containers: []ctr{{
			name: "app", cpuReq: 1000, memReq: 8 * gib,
			cpuUse: 400, memUse: 7 * gib,
		}},
	})
	learn(t, d, snap)

	recs := d.Recommend(now, nil)
	rec := only(t, recs, wl{name: "api"}.ref())

	if got := rec.Proposed.Attr(AttrChange); got != ChangeBoundaryShave {
		t.Fatalf("change = %q, want %q", got, ChangeBoundaryShave)
	}
	if got, want := rec.Current.Attr(AttrTier), "2vCPU 9GB"; got != want {
		t.Fatalf("current tier = %q, want %q (the +256 MB cliff)", got, want)
	}
	if got, want := rec.Proposed.Attr(AttrTier), "1vCPU 8GB"; got != want {
		t.Fatalf("proposed tier = %q, want %q", got, want)
	}

	// Exact dollars, from the published rates rather than from the code.
	wantCur, wantProp := rate(2, 9), rate(1, 8)
	if math.Abs(rec.CurrentHourlyUSD-wantCur) > 1e-12 {
		t.Errorf("current = $%.9f/h, want $%.9f/h", rec.CurrentHourlyUSD, wantCur)
	}
	if math.Abs(rec.ProposedHourlyUSD-wantProp) > 1e-12 {
		t.Errorf("proposed = $%.9f/h, want $%.9f/h", rec.ProposedHourlyUSD, wantProp)
	}
	wantGross := (wantCur - wantProp) * pricing.HoursPerMonth
	if math.Abs(rec.GrossSavingsMonthlyUSD-wantGross) > 1e-9 {
		t.Errorf("gross = $%.6f/mo, want $%.6f/mo", rec.GrossSavingsMonthlyUSD, wantGross)
	}
	// The design quotes a 37 % drop; hold the code to it.
	if drop := 1 - wantProp/wantCur; math.Abs(drop-0.371) > 0.002 {
		t.Errorf("the cliff shave saves %.1f%%, the design says 37.1%%", drop*100)
	}

	// The proposed request is the LARGEST one that still lands on the cheaper
	// tier — 8 GB minus the 256 MB overhead — not what the sizing policy would
	// pick. The shave gives up the fewest bytes that buy the tier.
	cs, err := Containers(rec.Proposed)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("containers = %+v", cs)
	}
	if want := 8*gib - pricing.FargateOverheadBytes; cs[0].Requests.MemoryBytes != want {
		t.Errorf("proposed memory = %d, want %d (the tier boundary)", cs[0].Requests.MemoryBytes, want)
	}
	// CPU is never shaved: the target tier must hold the unchanged request.
	if cs[0].Requests.MilliCPU != 1000 {
		t.Errorf("proposed CPU = %dm, want the unchanged 1000m", cs[0].Requests.MilliCPU)
	}
	if rec.Risk != plan.RiskMedium {
		t.Errorf("risk = %q, want %q for a memory shave", rec.Risk, plan.RiskMedium)
	}
	// Re-quantizing the proposal must reproduce the quoted tier.
	got, err := pricing.Quantize(cs[0].Requests, snap.Pods[0].InitRequests)
	if err != nil {
		t.Fatal(err)
	}
	if got != tierOf(t, rec.Proposed) {
		t.Errorf("proposal re-quantizes to %s, not the quoted %s", got, rec.Proposed.Attr(AttrTier))
	}
}

// TestBoundaryShavePerTierClass walks every vCPU class in the §4.1 table. Each
// case is an over-the-boundary pod whose observed peak allows exactly one tier
// drop, and the dollars are asserted against the published rates.
func TestBoundaryShavePerTierClass(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cpuReq, memReq    int64
		peak              int64
		wantCur, wantProp string
		curVCPU, curGB    float64
		propVCPU, propGB  float64
	}{
		{
			name:   "0.25 vCPU class: 2GB → 1GB",
			cpuReq: 200, memReq: 1500 * mib, peak: 300 * mib,
			wantCur: "0.25vCPU 2GB", wantProp: "0.25vCPU 1GB",
			curVCPU: 0.25, curGB: 2, propVCPU: 0.25, propGB: 1,
		},
		{
			name:   "0.5 vCPU class: 4GB → 3GB",
			cpuReq: 400, memReq: 3800 * mib, peak: 2200 * mib,
			wantCur: "0.5vCPU 4GB", wantProp: "0.5vCPU 3GB",
			curVCPU: 0.5, curGB: 4, propVCPU: 0.5, propGB: 3,
		},
		{
			name:   "1 vCPU class: 8GB → 7GB",
			cpuReq: 900, memReq: 7800 * mib, peak: 6000 * mib,
			wantCur: "1vCPU 8GB", wantProp: "1vCPU 7GB",
			curVCPU: 1, curGB: 8, propVCPU: 1, propGB: 7,
		},
		{
			name:   "2 vCPU class: 16GB → 14GB",
			cpuReq: 1800, memReq: 15800 * mib, peak: 12000 * mib,
			wantCur: "2vCPU 16GB", wantProp: "2vCPU 14GB",
			curVCPU: 2, curGB: 16, propVCPU: 2, propGB: 14,
		},
		{
			name:   "4 vCPU class: 30GB → 25GB",
			cpuReq: 3500, memReq: 29800 * mib, peak: 23000 * mib,
			wantCur: "4vCPU 30GB", wantProp: "4vCPU 25GB",
			curVCPU: 4, curGB: 30, propVCPU: 4, propGB: 25,
		},
		{
			// 8 vCPU steps in 4 GB, not 1 GB — a shave has to clear a whole
			// 4 GB step to be worth anything.
			name:   "8 vCPU class, 4GB steps: 60GB → 52GB",
			cpuReq: 7000, memReq: 59800 * mib, peak: 46000 * mib,
			wantCur: "8vCPU 60GB", wantProp: "8vCPU 52GB",
			curVCPU: 8, curGB: 60, propVCPU: 8, propGB: 52,
		},
		{
			// 16 vCPU steps in 8 GB.
			name:   "16 vCPU class, 8GB steps: 120GB → 104GB",
			cpuReq: 15000, memReq: 119500 * mib, peak: 92000 * mib,
			wantCur: "16vCPU 120GB", wantProp: "16vCPU 104GB",
			curVCPU: 16, curGB: 120, propVCPU: 16, propGB: 104,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDomain(t, noPolicyMoves)
			w := wl{name: "api", containers: []ctr{{
				name: "app", cpuReq: tc.cpuReq, memReq: tc.memReq,
				cpuUse: tc.cpuReq / 2, memUse: tc.peak,
			}}}
			learn(t, d, cluster(now, 300, 72*time.Hour, w))
			rec := only(t, d.Recommend(now, nil), w.ref())

			if got := rec.Current.Attr(AttrTier); got != tc.wantCur {
				t.Fatalf("current tier = %q, want %q", got, tc.wantCur)
			}
			if got := rec.Proposed.Attr(AttrTier); got != tc.wantProp {
				t.Fatalf("proposed tier = %q, want %q", got, tc.wantProp)
			}
			if got := rec.Proposed.Attr(AttrChange); got != ChangeBoundaryShave {
				t.Fatalf("change = %q, want a boundary shave", got)
			}
			wantCur, wantProp := rate(tc.curVCPU, tc.curGB), rate(tc.propVCPU, tc.propGB)
			if math.Abs(rec.CurrentHourlyUSD-wantCur) > 1e-12 {
				t.Errorf("current = $%.9f/h, want $%.9f/h", rec.CurrentHourlyUSD, wantCur)
			}
			if math.Abs(rec.ProposedHourlyUSD-wantProp) > 1e-12 {
				t.Errorf("proposed = $%.9f/h, want $%.9f/h", rec.ProposedHourlyUSD, wantProp)
			}
			if want := (wantCur - wantProp) * pricing.HoursPerMonth; math.Abs(rec.GrossSavingsMonthlyUSD-want) > 1e-9 {
				t.Errorf("gross = $%.6f/mo, want $%.6f/mo", rec.GrossSavingsMonthlyUSD, want)
			}
			// With no ledger there are no commitments, so net == gross.
			if rec.NetSavingsMonthlyUSD != rec.GrossSavingsMonthlyUSD {
				t.Errorf("net $%v != gross $%v with no ledger",
					rec.NetSavingsMonthlyUSD, rec.GrossSavingsMonthlyUSD)
			}
		})
	}
}

// TestWithinTierChangeIsNeverRecommended is U1's $0 rule at the domain level: a
// request change that does not cross a tier boundary is a rolling restart for
// nothing, so it must not be emitted at all.
func TestWithinTierChangeIsNeverRecommended(t *testing.T) {
	d := newDomain(t)
	// Requests 2 vCPU / 8 GB → billed 2 vCPU / 9 GB. Usage is far lower, so the
	// sizing policy wants a big shrink, but memory can only fall to the peak
	// and CPU stays inside the 2-vCPU row... unless it crosses a boundary.
	w := wl{name: "steady", containers: []ctr{{
		name: "app", cpuReq: 2000, memReq: 8 * gib, cpuUse: 1900, memUse: 8*gib - 300*mib,
	}}}
	learn(t, d, cluster(now, 300, 72*time.Hour, w))
	for _, rec := range d.Recommend(now, nil) {
		if rec.Target.ID != targetID(w.ref()) {
			continue
		}
		cur, prop := tierOf(t, rec.Current), tierOf(t, rec.Proposed)
		if cur == prop {
			t.Fatalf("emitted a recommendation that does not change the tier: %s", rec.Reason)
		}
		if rec.GrossSavingsMonthlyUSD == 0 {
			t.Fatalf("emitted a $0 recommendation: %s", rec.Reason)
		}
	}
}

// TestEveryFargateResizeIsRolling is the rule with the sharpest consequence:
// an in-place resize is impossible on Fargate, and a step claiming one would
// understate disruption to the executor's eviction budget and PDB accounting.
func TestEveryFargateResizeIsRolling(t *testing.T) {
	d := newDomain(t)
	snap := cluster(now, 300, 72*time.Hour,
		wl{name: "cliff", containers: []ctr{{name: "app", cpuReq: 1000, memReq: 8 * gib, cpuUse: 400, memUse: 7 * gib}}},
		wl{name: "fat", containers: []ctr{{name: "app", cpuReq: 4000, memReq: 16 * gib, cpuUse: 200, memUse: 512 * mib}}},
		wl{name: "multi", replicas: 3, containers: []ctr{
			{name: "app", cpuReq: 2000, memReq: 6 * gib, cpuUse: 300, memUse: 1 * gib},
			{name: "sidecar", cpuReq: 500, memReq: 2 * gib, cpuUse: 50, memUse: 256 * mib},
		}},
	)
	learn(t, d, snap)
	recs := d.Recommend(now, nil)
	if len(recs) == 0 {
		t.Fatal("fixture produced no recommendations")
	}
	for _, r := range recs {
		if r.Action != domain.ActionRolling {
			t.Errorf("%s: action %q, want %q", r.Target.ID, r.Action, domain.ActionRolling)
		}
		if r.Action == domain.ActionInPlace {
			t.Errorf("%s claims an in-place resize, which Fargate cannot do", r.Target.ID)
		}
		if !r.Action.Disruptive() {
			t.Errorf("%s: a Fargate resize must be accounted as disruptive", r.Target.ID)
		}
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", r.Target.ID, err)
		}
	}

	// PlanSteps refuses to build a plan from anything that is not rolling, so a
	// future domain change cannot smuggle an in-place claim past the executor.
	bad := recs[0]
	bad.Action = domain.ActionInPlace
	if _, err := d.PlanSteps([]domain.Recommendation{bad}, domain.Guard{Now: now}); err == nil {
		t.Fatal("PlanSteps accepted an in-place Fargate resize")
	}
}

// TestTierMoveProjectsTheSizingPolicy: a badly over-provisioned pod is moved by
// pkg/recommend's own decision, projected onto the tier table.
func TestTierMoveProjectsTheSizingPolicy(t *testing.T) {
	d := newDomain(t)
	w := wl{name: "over", replicas: 4, containers: []ctr{{
		name: "app", cpuReq: 2000, memReq: 8 * gib, cpuUse: 150, memUse: 400 * mib,
	}}}
	learn(t, d, cluster(now, 300, 72*time.Hour, w))
	rec := only(t, d.Recommend(now, nil), w.ref())

	if got := rec.Proposed.Attr(AttrChange); got != ChangeTierMove {
		t.Fatalf("change = %q, want %q", got, ChangeTierMove)
	}
	cur, prop := tierOf(t, rec.Current), tierOf(t, rec.Proposed)
	if cur.MilliCPU <= prop.MilliCPU && cur.MemoryMiB <= prop.MemoryMiB {
		t.Fatalf("tier did not shrink: %s → %s", cur, prop)
	}
	rates := pricing.DefaultFargateRates()
	// Billing is per pod: four replicas save four times over.
	if want := rates.Cost(cur) * 4; math.Abs(rec.CurrentHourlyUSD-want) > 1e-12 {
		t.Errorf("current = $%v/h, want $%v/h for 4 replicas", rec.CurrentHourlyUSD, want)
	}
	if want := rates.Cost(prop) * 4; math.Abs(rec.ProposedHourlyUSD-want) > 1e-12 {
		t.Errorf("proposed = $%v/h, want $%v/h for 4 replicas", rec.ProposedHourlyUSD, want)
	}
	if got := rec.Proposed.Attr(AttrReplicas); got != "4" {
		t.Errorf("replicas attr = %q, want 4", got)
	}
	// The proposal is self-consistent: re-quantizing the per-container requests
	// reproduces the quoted tier and therefore the quoted price.
	cs, err := Containers(rec.Proposed)
	if err != nil {
		t.Fatal(err)
	}
	var sum = cs[0].Requests
	for _, c := range cs[1:] {
		sum = sum.Add(c.Requests)
	}
	got, err := pricing.Quantize(sum, modelZero)
	if err != nil {
		t.Fatal(err)
	}
	if got != prop {
		t.Fatalf("proposal re-quantizes to %s, not the quoted %s", got, prop)
	}
	// Never below what was actually observed.
	if cs[0].Requests.MemoryBytes < 400*mib {
		t.Errorf("proposed memory %d is below the observed peak %d", cs[0].Requests.MemoryBytes, 400*mib)
	}
	if rec.Confidence <= 0 || rec.Confidence > 1 {
		t.Errorf("confidence = %v", rec.Confidence)
	}
}

// modelZero is an empty init-container request.
var modelZero model.Resources
