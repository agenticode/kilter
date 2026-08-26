package domain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// The threat model, restated for the domain that has the strongest reason to
// be trusted and must be trusted least.
//
// pkg/rds is structurally read-only: no mutating API appears anywhere in the
// package, Health is unconditionally report-only and PlanSteps refuses
// unconditionally. None of that is why an RDS step cannot run. A [Domain] is
// ordinary Go code, and the value of `rds` as a registry key is now a public
// constant that anything in-process can claim. What stops an impostor is the
// core: Health is checked BEFORE the domain is consulted, steps are validated
// on the way OUT, and execution routes through an actuator table only cmd/ can
// write to.
//
// impostorDomain is that impostor. Every field is a separate lie so each can
// be tested alone, and it lies through the two seams the RDS wiring newly
// depends on — [Refuser] and the usage-line projection — which the existing
// hostile-domain tests do not exercise.
type impostorDomain struct {
	kind Kind
	// claimsActuatable makes Health report ReportOnly:false on a domain with
	// no collector, no credentials and no actuator.
	claimsActuatable bool
	// steps is handed back unconditionally, including for recommendations it
	// was never given and targets in domains it does not own.
	steps []Step
	// recs are fabricated recommendations. netAboveGross populates the two
	// savings fields DIRECTLY, bypassing SetSavings — the one clamp that makes
	// Net ≤ Gross an invariant rather than a convention.
	recs []Recommendation
	// refusals are fabricated refusals, optionally attributed to another
	// domain's targets.
	refusals []Refusal
	// lines are fabricated account-wide usage lines.
	lines []commit.UsageLine

	planCalls int
}

func (i *impostorDomain) Kind() Kind            { return i.kind }
func (i *impostorDomain) Learn(*Snapshot) error { return nil }
func (i *impostorDomain) Recommend(time.Time, Netter) []Recommendation {
	out := make([]Recommendation, len(i.recs))
	copy(out, i.recs)
	return out
}
func (i *impostorDomain) PlanSteps([]Recommendation, Guard) ([]Step, error) {
	i.planCalls++
	return i.steps, nil
}
func (i *impostorDomain) Health(time.Time) Health {
	return Health{Kind: i.kind, Ready: true, ReportOnly: !i.claimsActuatable, Targets: 1}
}
func (i *impostorDomain) Checkpoint() ([]byte, error) { return nil, nil }
func (i *impostorDomain) Restore([]byte) error        { return nil }

// Refusals implements [Refuser].
func (i *impostorDomain) Refusals(time.Time, Netter) []Refusal {
	out := make([]Refusal, len(i.refusals))
	copy(out, i.refusals)
	return out
}

// UsageLines is the shape cmd/ reaches for when it splices the account-wide
// commitment baseline.
func (i *impostorDomain) UsageLines(time.Time, Netter) []commit.UsageLine {
	out := make([]commit.UsageLine, len(i.lines))
	copy(out, i.lines)
	return out
}

var impostorNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// TestAHostileRDSDomainCannotActuateOrBorrowAnotherDomainsActuator.
//
// Three independent claims, in increasing order of severity:
//
//  1. It says it is not report-only. The core does not grant actuation on a
//     domain's own say-so — CanActuate reads the table, not the Health.
//  2. It hands back a well-formed step for its OWN kind. There is no actuator
//     for `rds` and there cannot be one, so Execute and Revert both refuse.
//  3. It hands back a step labelled `ec2`, where a real actuator IS wired.
//     This is the hole j1-wire closed: steps are routed for execution by
//     Step.Target.Domain, so an unvalidated output would let any domain borrow
//     any other domain's actuator and its credentials.
func TestAHostileRDSDomainCannotActuateOrBorrowAnotherDomainsActuator(t *testing.T) {
	victim := &recordingActuator{kind: EC2}
	imp := &impostorDomain{kind: RDS, claimsActuatable: true}

	r := NewRegistry()
	if err := r.Register(imp); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ready(EC2)); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterActuator(victim); err != nil {
		t.Fatal(err)
	}

	// 1. Claiming actuatability grants nothing.
	if r.CanActuate(RDS) {
		t.Fatal("an rds domain granted itself actuation capability")
	}

	// 2. Its own step goes nowhere.
	own := stepFor(RDS, "db-prod-1")
	if err := r.Execute(context.Background(), Guard{Now: impostorNow}, own); !errors.Is(err, ErrReportOnly) {
		t.Errorf("Execute(rds step) = %v, want ErrReportOnly", err)
	}
	if err := r.Revert(context.Background(), Guard{Now: impostorNow}, own); !errors.Is(err, ErrReportOnly) {
		t.Errorf("Revert(rds step) = %v, want ErrReportOnly", err)
	}

	// 3. A step labelled as EC2's is refused on the way out of the domain, so
	//    it never becomes something Execute could route.
	rec := validRec()
	rec.Target.Domain = RDS
	imp.steps = []Step{stepFor(EC2, "i-victim")}
	steps, err := r.PlanSteps(RDS, []Recommendation{rec}, Guard{Now: impostorNow})
	if !errors.Is(err, ErrWrongDomain) {
		t.Fatalf("PlanSteps = (%v, %v), want ErrWrongDomain", steps, err)
	}
	if len(steps) != 0 {
		t.Fatal("a cross-domain step escaped the core")
	}
	p := r.BuildPlan(RDS, []Recommendation{rec}, Guard{Now: impostorNow})
	if p.RefusalCode != RefuseDomainError {
		t.Errorf("BuildPlan refusal = %q, want %q", p.RefusalCode, RefuseDomainError)
	}
	if len(victim.executed) != 0 || len(victim.reverted) != 0 {
		t.Fatal("the ec2 actuator was reached by a domain that does not own it")
	}
	if imp.planCalls == 0 {
		t.Fatal("the test never reached the domain's PlanSteps; it proves nothing")
	}
}

// TestAnHonestRDSDomainIsAlsoRefused is the control, and it is the point of
// the whole arrangement: the outcome does not depend on the domain's manners.
//
// pkg/rds's real Health is report-only, so the core refuses one step EARLIER —
// before PlanSteps is called at all. Both a liar and an honest domain end at
// the same wall; only the number of walls they hit differs.
func TestAnHonestRDSDomainIsAlsoRefused(t *testing.T) {
	honest := &impostorDomain{kind: RDS, claimsActuatable: false}
	r := NewRegistry()
	if err := r.Register(honest); err != nil {
		t.Fatal(err)
	}
	rec := validRec()
	rec.Target.Domain = RDS
	if _, err := r.PlanSteps(RDS, []Recommendation{rec}, Guard{Now: impostorNow}); !errors.Is(err, ErrReportOnly) {
		t.Fatalf("PlanSteps = %v, want ErrReportOnly", err)
	}
	if honest.planCalls != 0 {
		t.Error("a report-only domain's PlanSteps was called; the core deferred to the domain")
	}
}

// TestAHostileDomainCannotAttributeFindingsToAnotherDomain.
//
// The RDS wiring is the first that depends on [Refuser] for its entire output,
// which makes the seam worth attacking. A domain that returned refusals
// pointing at another domain's targets would put words in that domain's mouth
// — a `not-gp2` line under `ec2` that pkg/ebs never said, in a report a human
// is asked to act on. The registry re-stamps the producing kind onto every
// refusal for the same reason Recommend re-stamps it onto every
// recommendation.
func TestAHostileDomainCannotAttributeFindingsToAnotherDomain(t *testing.T) {
	imp := &impostorDomain{kind: RDS, refusals: []Refusal{
		{Target: TargetRef{Domain: EC2, Scope: "acct/us-east-1", ID: "i-victim"},
			Code: "not-gp2", Reason: "a finding pkg/ebs never made"},
		{Target: TargetRef{Domain: Lambda, Scope: "acct/us-east-1", ID: "fn-victim"},
			Code: "single-memory-point", Reason: "nor did pkg/lambda"},
	}}
	r := NewRegistry()
	if err := r.Register(imp); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ready(EC2)); err != nil {
		t.Fatal(err)
	}

	got := r.Refusals(RDS, impostorNow, nil)
	if len(got) != 2 {
		t.Fatalf("got %d refusals, want 2", len(got))
	}
	for _, ref := range got {
		if ref.Target.Domain != RDS {
			t.Errorf("refusal for %s is attributed to %q; the producing domain must own its output",
				ref.Target.ID, ref.Target.Domain)
		}
	}
	// And the aggregate agrees: EC2's row counts none of them.
	rep := Summarize(impostorNow, r, nil)
	ec2Row, ok := rep.For(EC2)
	if !ok {
		t.Fatal("ec2 is missing from the report")
	}
	if ec2Row.Refused != 0 {
		t.Errorf("ec2 was charged %d refusals it never made", ec2Row.Refused)
	}
}

// TestAFabricatedSavingIsCaughtByReportValidate.
//
// Recommendation.SetSavings clamps Net ≤ Gross, which is what makes the
// invariant mechanical — but a domain can assign the fields directly and skip
// the clamp. Nothing in Summarize re-checks it, by design: the aggregate is a
// projection, and Report.Validate is the gate. The CLI calls Validate before
// printing and fails loudly rather than print a number somebody might put in a
// business case, so this asserts the gate rather than the projection.
func TestAFabricatedSavingIsCaughtByReportValidate(t *testing.T) {
	rec := validRec()
	rec.Target.Domain = RDS
	// The clamp, bypassed.
	rec.GrossSavingsMonthlyUSD = 1
	rec.NetSavingsMonthlyUSD = 100_000

	imp := &impostorDomain{kind: RDS, recs: []Recommendation{rec}}
	r := NewRegistry()
	if err := r.Register(imp); err != nil {
		t.Fatal(err)
	}
	rep := Summarize(impostorNow, r, nil)
	if err := rep.Validate(); err == nil {
		t.Fatalf("a report claiming net $%v on gross $%v validated",
			rec.NetSavingsMonthlyUSD, rec.GrossSavingsMonthlyUSD)
	}
}
