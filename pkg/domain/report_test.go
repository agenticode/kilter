package domain

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var reportNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// TestSummarizeWithZeroDomains is invariant 1 at the report level: a core with
// no organs attached reports nothing and fails at nothing.
func TestSummarizeWithZeroDomains(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *Registry
	}{{"nil", nil}, {"empty", NewRegistry()}, {"zero value", &Registry{}}} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Summarize(reportNow, tc.reg, nil)
			if rep == nil {
				t.Fatal("Summarize returned nil")
			}
			if len(rep.Domains) != 0 || len(rep.Recommendations) != 0 || len(rep.Refusals) != 0 {
				t.Fatalf("empty registry produced %+v", rep)
			}
			if rep.Totals.ClaimableMonthlyUSD != 0 || rep.Totals.GrossMonthlyUSD != 0 {
				t.Fatalf("empty registry produced money: %+v", rep.Totals)
			}
			if err := rep.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			var b strings.Builder
			if err := rep.WriteText(&b); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(b.String(), "no domains registered") {
				t.Errorf("empty report does not say so:\n%s", b.String())
			}
		})
	}
}

// TestAggregateNeverClaimsMoreThanGross is requirement 4 at the aggregate.
//
// The trap is arithmetic, not conceptual: a domain may honestly recommend a
// change that COSTS more (a safety-driven growth), and such a recommendation
// has a negative gross with a zero claim. Summing raw gross would let that
// negative drag the total below a positive claimable sum and manufacture a
// net > gross violation out of two individually honest numbers.
func TestAggregateNeverClaimsMoreThanGross(t *testing.T) {
	growth := recFor(Lambda, "fn-under-provisioned", -40, -40)
	growth.Action = ActionAdvisory
	saving := recFor(EC2, "i-1", 100, 100)

	inst := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true},
		recs: []Recommendation{saving}}
	lam := &partDomain{kind: Lambda, prefix: "fn-", health: Health{Ready: true, ReportOnly: true},
		recs: []Recommendation{growth}}
	r := NewRegistry()
	mustRegister(t, r, inst, lam)

	rep := Summarize(reportNow, r, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := rep.Totals.ClaimableMonthlyUSD; got != 100 {
		t.Errorf("claimable = %v, want 100", got)
	}
	if got := rep.Totals.GrossMonthlyUSD; got != 100 {
		t.Errorf("gross = %v, want 100 (the loss must not net against it)", got)
	}
	if got := rep.Totals.GrossIncreaseMonthlyUSD; got != 40 {
		t.Errorf("gross increase = %v, want 40 (carried, not hidden)", got)
	}
	if rep.Totals.ClaimableMonthlyUSD > rep.Totals.GrossMonthlyUSD {
		t.Fatal("the aggregate claims more than list price")
	}

	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "would COST more") {
		t.Errorf("the cost increase is not surfaced:\n%s", b.String())
	}
}

// TestCommitmentNegativeRecommendationIsReportedNotDropped is §7 trap 1 at the
// aggregate: the change that would raise the bill stays visible, with its
// code, and claims exactly nothing.
func TestCommitmentNegativeRecommendationIsReportedNotDropped(t *testing.T) {
	blocked := recFor(EC2, "i-ri-covered", 210.24, 0)
	blocked.Suppressed = true
	blocked.SuppressCode = SuppressCommitmentNegative
	blocked.Reason = "downsizing would strand a reserved instance"
	blocked.ValidFrom = reportNow.AddDate(1, 0, 0)

	fine := recFor(EC2, "i-plain", 12, 12)
	inst := &partDomain{kind: EC2, prefix: "i-", health: Health{Ready: true},
		recs: []Recommendation{blocked, fine}}
	r := NewRegistry()
	mustRegister(t, r, inst)

	rep := Summarize(reportNow, r, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	row, ok := rep.For(EC2)
	if !ok {
		t.Fatal("no ec2 row")
	}
	if row.Recommendations != 2 || row.Applicable != 1 || row.Suppressed != 1 {
		t.Fatalf("counts = %+v", row)
	}
	if row.ClaimableMonthlyUSD != 12 {
		t.Errorf("claimable = %v, want 12: the blocked $210 must not be claimed", row.ClaimableMonthlyUSD)
	}
	if row.GrossMonthlyUSD != 222.24 {
		t.Errorf("gross = %v, want 222.24: the fantasy is still shown", row.GrossMonthlyUSD)
	}
	if len(row.SuppressedByCode) != 1 || row.SuppressedByCode[0].Code != SuppressCommitmentNegative {
		t.Fatalf("suppressed-by-code = %+v", row.SuppressedByCode)
	}

	// Still present in the recommendation list — a dropped recommendation is
	// indistinguishable from a bug.
	var found bool
	for _, rec := range rep.Recommendations {
		if rec.Target.ID == "i-ri-covered" {
			found = true
			if rec.ClaimableMonthlyUSD() != 0 {
				t.Errorf("a suppressed recommendation claims $%v", rec.ClaimableMonthlyUSD())
			}
		}
	}
	if !found {
		t.Fatal("the commitment-blocked recommendation vanished from the report")
	}

	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, SuppressCommitmentNegative) {
		t.Errorf("the suppression code is not rendered:\n%s", out)
	}
	if !strings.Contains(out, "declined to do") {
		t.Errorf("there is no refusals panel:\n%s", out)
	}
}

// TestRefusalsSurfaceTargetsThatProducedNoRecommendation is requirement 2's
// sharp end. A domain that assessed forty volumes and proposed nothing must
// not look like a domain that found nothing.
func TestRefusalsSurfaceTargetsThatProducedNoRecommendation(t *testing.T) {
	vols := &partDomain{kind: EC2, prefix: "vol-", health: Health{Ready: true, ReportOnly: true},
		refs: []Refusal{
			{Target: TargetRef{Scope: "acct", ID: "vol-2"}, Code: "no-cheaper-config",
				Reason: "parity costs more than the storage saving in the 334-375 GiB band"},
			{Target: TargetRef{Scope: "acct", ID: "vol-1"}, Code: "not-gp2", Reason: "io2 is advisory"},
			{Target: TargetRef{Scope: "acct", ID: "vol-3"}, Code: "not-gp2", Reason: "gp3 already"},
		}}
	r := NewRegistry()
	mustRegister(t, r, vols)

	rep := Summarize(reportNow, r, nil)
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(rep.Recommendations) != 0 {
		t.Fatal("this domain proposes nothing")
	}
	if rep.Totals.Refused != 3 {
		t.Fatalf("refused = %d, want 3", rep.Totals.Refused)
	}
	// Most frequent first, then alphabetical: deterministic, never map order.
	want := []CodeCount{{Code: "not-gp2", Count: 2}, {Code: "no-cheaper-config", Count: 1}}
	if len(rep.Totals.RefusedByCode) != len(want) {
		t.Fatalf("refused-by-code = %+v", rep.Totals.RefusedByCode)
	}
	for i := range want {
		if rep.Totals.RefusedByCode[i] != want[i] {
			t.Fatalf("refused-by-code = %+v, want %+v", rep.Totals.RefusedByCode, want)
		}
	}
	// Refusals sorted by target, not by arrival.
	for i, id := range []string{"vol-1", "vol-2", "vol-3"} {
		if rep.Refusals[i].Target.ID != id {
			t.Fatalf("refusals are not canonically ordered: %v", rep.Refusals)
		}
	}
	var b strings.Builder
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no-cheaper-config") {
		t.Errorf("a refusal code is missing from the rendering:\n%s", b.String())
	}
}

// TestReportValidateCatchesEachViolation. A checker that passes everything
// proves nothing.
func TestReportValidateCatchesEachViolation(t *testing.T) {
	base := func() *Report {
		return &Report{
			At: reportNow,
			Domains: []DomainReport{{
				Kind: EC2, Recommendations: 1, Applicable: 1,
				ClaimableMonthlyUSD: 10, GrossMonthlyUSD: 10,
			}},
			Recommendations: []Recommendation{recFor(EC2, "i-1", 10, 10)},
			Totals: Totals{
				Domains: 1, Recommendations: 1, Applicable: 1,
				ClaimableMonthlyUSD: 10, GrossMonthlyUSD: 10,
			},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the positive control does not validate: %v", err)
	}
	for _, tc := range []struct {
		name   string
		break_ func(*Report)
	}{
		{"claims more than gross", func(r *Report) { r.Totals.ClaimableMonthlyUSD = 11 }},
		{"domain claims more than gross", func(r *Report) { r.Domains[0].ClaimableMonthlyUSD = 99 }},
		{"NaN money", func(r *Report) { r.Totals.GrossMonthlyUSD = math.NaN() }},
		{"infinite money", func(r *Report) { r.Domains[0].GrossMonthlyUSD = math.Inf(1) }},
		{"negative money", func(r *Report) { r.Totals.ClaimableMonthlyUSD = -1 }},
		{"counts disagree", func(r *Report) { r.Domains[0].Applicable = 5 }},
		{"totals disagree", func(r *Report) { r.Totals.Applicable = 5 }},
		{"total recs disagree", func(r *Report) { r.Totals.Recommendations = 2; r.Totals.Applicable = 2 }},
		{"domain count disagrees", func(r *Report) { r.Totals.Domains = 7 }},
		{"duplicate domain", func(r *Report) { r.Domains = append(r.Domains, r.Domains[0]); r.Totals.Domains = 2 }},
		{"unknown kind", func(r *Report) { r.Domains[0].Kind = "quantum-annealer" }},
		{"invalid recommendation", func(r *Report) { r.Recommendations[0].Evidence = nil }},
		{"refusal with no code", func(r *Report) {
			r.Refusals = []Refusal{{Target: TargetRef{ID: "x"}, Reason: "why"}}
			r.Totals.Refused = 1
		}},
		{"refusal with no reason", func(r *Report) {
			r.Refusals = []Refusal{{Target: TargetRef{ID: "x"}, Code: "c"}}
			r.Totals.Refused = 1
		}},
		{"refusal count disagrees", func(r *Report) { r.Totals.Refused = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.break_(r)
			if err := r.Validate(); err == nil {
				t.Error("Validate accepted a broken report")
			}
		})
	}
	var nilRep *Report
	if err := nilRep.Validate(); err == nil {
		t.Error("Validate accepted a nil report")
	}
}

// TestSummarizeIsDeterministicUnderShuffle: domains registered in any order,
// recommendations delivered in any order, identical bytes out. Float addition
// is not associative, so this is a money check as much as a rendering one.
func TestSummarizeIsDeterministicUnderShuffle(t *testing.T) {
	build := func(order []Kind) *Registry {
		r := NewRegistry()
		for _, k := range order {
			d := &partDomain{kind: k, health: Health{Ready: true}}
			for i := 0; i < 6; i++ {
				d.recs = append(d.recs, recFor(k, string(k)+"-"+string(rune('a'+i)),
					float64(i)*1.1+0.07, float64(i)*1.1+0.07))
			}
			d.refs = []Refusal{{Target: TargetRef{ID: string(k) + "-refused"},
				Code: "low-confidence", Reason: "not enough evidence"}}
			if err := r.Register(d); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	order := []Kind{EC2, ECSFargate, K8sFargate, Lambda}
	var want string
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 40; i++ {
		rng.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })
		rep := Summarize(reportNow, build(order), nil)
		if err := rep.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		var b strings.Builder
		if err := rep.WriteText(&b); err != nil {
			t.Fatal(err)
		}
		got := b.String()
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("output changed with registration order\n--- want ---\n%s\n--- got ---\n%s", want, got)
		}
	}
}

// TestBuildPlanRefusesWithACode: every way a plan can be empty carries a
// distinguishable code, because "no steps" and "no answer" must not look the
// same to a caller.
func TestBuildPlanRefusesWithACode(t *testing.T) {
	rec := recFor(ECSFargate, "prod/web", 10, 10)
	suppressed := recFor(ECSFargate, "prod/api", 10, 0)
	suppressed.Suppressed, suppressed.SuppressCode = true, SuppressModeRecommend

	for _, tc := range []struct {
		name  string
		setup func() (*Registry, []Recommendation, Guard)
		want  string
	}{
		{"not registered", func() (*Registry, []Recommendation, Guard) {
			return NewRegistry(), []Recommendation{rec}, Guard{Now: reportNow}
		}, RefuseNotRegistered},
		{"report-only", func() (*Registry, []Recommendation, Guard) {
			r := NewRegistry()
			mustRegister(t, r, &partDomain{kind: ECSFargate,
				health: Health{Ready: true, ReportOnly: true, Reason: "no actuator is wired"}})
			return r, []Recommendation{rec}, Guard{Now: reportNow}
		}, RefuseReportOnly},
		{"frozen", func() (*Registry, []Recommendation, Guard) {
			r := NewRegistry()
			mustRegister(t, r, &partDomain{kind: ECSFargate, health: Health{Ready: true}})
			return r, []Recommendation{rec}, Guard{Now: reportNow, Freeze: true}
		}, RefuseGuardrail},
		{"everything suppressed", func() (*Registry, []Recommendation, Guard) {
			r := NewRegistry()
			mustRegister(t, r, &partDomain{kind: ECSFargate, health: Health{Ready: true}})
			return r, []Recommendation{suppressed}, Guard{Now: reportNow}
		}, RefuseNothingToDo},
		{"nothing applicable", func() (*Registry, []Recommendation, Guard) {
			r := NewRegistry()
			mustRegister(t, r, &partDomain{kind: ECSFargate, health: Health{Ready: true}})
			return r, nil, Guard{Now: reportNow}
		}, RefuseNothingToDo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, recs, g := tc.setup()
			p := reg.BuildPlan(ECSFargate, recs, g)
			if p.RefusalCode != tc.want {
				t.Fatalf("RefusalCode = %q (%s), want %q", p.RefusalCode, p.Refusal, tc.want)
			}
			if p.Refusal == "" {
				t.Error("a refusal code with no prose is not an explanation")
			}
			if len(p.Steps) != 0 || p.Fingerprint != "" {
				t.Error("a refused plan carries steps or a fingerprint")
			}
		})
	}

	// The positive control, so the table above means something.
	r := NewRegistry()
	mustRegister(t, r, &partDomain{kind: ECSFargate, health: Health{Ready: true}})
	p := r.BuildPlan(ECSFargate, []Recommendation{rec, suppressed}, Guard{Now: reportNow})
	if p.RefusalCode != "" {
		t.Fatalf("the control refused: %s (%s)", p.RefusalCode, p.Refusal)
	}
	if len(p.Steps) != 1 || p.Fingerprint == "" {
		t.Fatalf("plan = %+v", p)
	}
	if p.Considered != 1 || p.Suppressed != 1 {
		t.Errorf("Considered=%d Suppressed=%d, want 1 and 1", p.Considered, p.Suppressed)
	}
	if p.Fingerprint != Fingerprint(p.Steps) {
		t.Error("the fingerprint does not cover the steps")
	}

	// BuildPlans covers every registered kind, refusals included.
	mustRegister(t, r, &partDomain{kind: Lambda,
		health: Health{Ready: true, ReportOnly: true, Reason: "advisory only"}})
	plans := r.BuildPlans([]Recommendation{rec}, Guard{Now: reportNow})
	if len(plans) != 2 {
		t.Fatalf("BuildPlans returned %d plans, want 2", len(plans))
	}
	if plans[0].Kind != ECSFargate || plans[1].Kind != Lambda {
		t.Fatalf("BuildPlans is not canonically ordered: %v", plans)
	}
	if plans[1].RefusalCode != RefuseReportOnly {
		t.Errorf("the report-only domain was omitted rather than explained: %+v", plans[1])
	}
}

// TestKindsOfNoticesUnregisteredAttribution.
func TestKindsOfNoticesUnregisteredAttribution(t *testing.T) {
	got := KindsOf([]Recommendation{
		recFor(Lambda, "fn", 1, 1), recFor(EC2, "i-1", 1, 1), recFor(EC2, "i-2", 1, 1),
	})
	if len(got) != 2 || got[0] != EC2 || got[1] != Lambda {
		t.Fatalf("KindsOf = %v", got)
	}
}

func mustRegister(t *testing.T, r *Registry, ds ...Domain) {
	t.Helper()
	for _, d := range ds {
		if err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}
}
