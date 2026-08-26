package domain

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/guard"
)

// fakeDomain is a scriptable Domain used to pin the core's behaviour without
// dragging a real domain's economics into these tests.
type fakeDomain struct {
	kind       Kind
	health     Health
	recs       []Recommendation
	learned    int
	learnErr   error
	planned    [][]Recommendation
	planErr    error
	checkpoint []byte
}

func (f *fakeDomain) Kind() Kind { return f.kind }
func (f *fakeDomain) Learn(s *Snapshot) error {
	f.learned++
	return f.learnErr
}
func (f *fakeDomain) Recommend(now time.Time, l Netter) []Recommendation {
	out := make([]Recommendation, len(f.recs))
	copy(out, f.recs)
	return out
}
func (f *fakeDomain) PlanSteps(recs []Recommendation, g Guard) ([]Step, error) {
	f.planned = append(f.planned, recs)
	if f.planErr != nil {
		return nil, f.planErr
	}
	out := make([]Step, 0, len(recs))
	for i, r := range recs {
		out = append(out, Step{Seq: i + 1, Key: StepKey(r.Target, r.Current, r.Proposed),
			Target: r.Target, Action: r.Action})
	}
	return out, nil
}
func (f *fakeDomain) Health(now time.Time) Health { return f.health }
func (f *fakeDomain) Checkpoint() ([]byte, error) { return f.checkpoint, nil }
func (f *fakeDomain) Restore(b []byte) error      { f.checkpoint = b; return nil }

func ready(k Kind) *fakeDomain {
	return &fakeDomain{kind: k, health: Health{Kind: k, Ready: true}}
}

// TestZeroDomainsRegistered is invariant 1: the core runs with nothing plugged
// in. Every entry point must be harmless on an empty and on a nil registry —
// "kilter with zero domains configured behaves exactly as today".
func TestZeroDomainsRegistered(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		r    *Registry
	}{
		{"nil registry", nil},
		{"zero value", &Registry{}},
		{"constructed but empty", NewRegistry()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.r
			if got := r.Len(); got != 0 {
				t.Errorf("Len() = %d", got)
			}
			if got := r.Kinds(); got != nil {
				t.Errorf("Kinds() = %v, want nil", got)
			}
			if _, ok := r.Get(K8sFargate); ok {
				t.Error("Get found a domain in an empty registry")
			}
			if got := r.Recommend(now, nil); got != nil {
				t.Errorf("Recommend() = %v, want nil", got)
			}
			if got := r.Health(now); got != nil {
				t.Errorf("Health() = %v, want nil", got)
			}
			if err := r.Learn(nil); err != nil {
				t.Errorf("Learn(nil) = %v, want nil", err)
			}
			// A snapshot for an unregistered domain is reported, not dropped:
			// silently discarding collected data reads exactly like a broken
			// collector.
			err := r.Learn(&Snapshot{Domain: K8sFargate})
			if !errors.Is(err, ErrNotRegistered) {
				t.Errorf("Learn(snapshot) = %v, want ErrNotRegistered", err)
			}
			if _, err := r.PlanSteps(K8sFargate, nil, Guard{Now: now}); !errors.Is(err, ErrNotRegistered) {
				t.Errorf("PlanSteps = %v, want ErrNotRegistered", err)
			}
		})
	}
	// Register on a nil registry is a wiring bug, reported rather than a panic.
	var nilReg *Registry
	if err := nilReg.Register(ready(EC2)); err == nil {
		t.Error("Register on a nil registry should error")
	}
}

func TestRegisterRejectsWiringBugs(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("Register(nil) accepted")
	}
	if err := r.Register(&fakeDomain{kind: "quantum-annealer"}); err == nil {
		t.Error("Register accepted an unknown kind")
	}
	if err := r.Register(ready(K8sFargate)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ready(K8sFargate)); err == nil {
		t.Error("Register accepted a duplicate kind")
	}
	if r.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", r.Len())
	}
}

// TestReportOnlyDomainCannotPlanSteps is invariant 2, and it is enforced by the
// core rather than by trusting the domain: a domain whose collector or
// credentials are absent never gets the chance to produce a step.
func TestReportOnlyDomainCannotPlanSteps(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	rec := validRec()
	rec.Target.Domain = K8sFargate

	degraded := &fakeDomain{
		kind: K8sFargate,
		health: Health{Kind: K8sFargate, Ready: false, ReportOnly: true,
			Reason: "no cluster snapshot learned yet"},
		recs: []Recommendation{rec},
	}
	r := NewRegistry()
	if err := r.Register(degraded); err != nil {
		t.Fatal(err)
	}

	// Report-only means REPORT: recommendations still surface.
	if got := r.Recommend(now, nil); len(got) != 1 {
		t.Fatalf("report-only domain produced %d recommendations, want 1", len(got))
	}
	// ...but they can never become steps.
	steps, err := r.PlanSteps(K8sFargate, []Recommendation{rec}, Guard{Now: now})
	if !errors.Is(err, ErrReportOnly) {
		t.Fatalf("PlanSteps = (%v, %v), want ErrReportOnly", steps, err)
	}
	if steps != nil {
		t.Fatal("a report-only domain produced steps")
	}
	if len(degraded.planned) != 0 {
		t.Fatal("the domain's PlanSteps was called despite report-only")
	}
	// The reason travels with the error: no silent failure.
	if !contains(err.Error(), "no cluster snapshot learned yet") {
		t.Errorf("error %q drops the domain's reason", err)
	}

	// Flipping to ready lets the same recommendation plan.
	degraded.health = Health{Kind: K8sFargate, Ready: true}
	steps, err = r.PlanSteps(K8sFargate, []Recommendation{rec}, Guard{Now: now})
	if err != nil || len(steps) != 1 {
		t.Fatalf("PlanSteps after recovery = (%v, %v)", steps, err)
	}
}

func TestPlanStepsFiltersSuppressedAndForeign(t *testing.T) {
	now := time.Now().UTC()
	fd := ready(K8sFargate)
	r := NewRegistry()
	if err := r.Register(fd); err != nil {
		t.Fatal(err)
	}

	keep := validRec()
	suppressed := validRec()
	suppressed.Target.ID = "Deployment/default/suppressed"
	suppressed.Suppressed, suppressed.SuppressCode = true, SuppressCommitmentNegative
	foreign := validRec()
	foreign.Target = TargetRef{Domain: EC2, Scope: "acct", ID: "i-1"}

	steps, err := r.PlanSteps(K8sFargate, []Recommendation{keep, suppressed, foreign}, Guard{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Target.ID != keep.Target.ID {
		t.Fatalf("steps = %+v, want only %s", steps, keep.Target.ID)
	}
	if len(fd.planned) != 1 || len(fd.planned[0]) != 1 {
		t.Fatalf("domain saw %v, want exactly the applicable recommendation", fd.planned)
	}
	// Nothing applicable is nil steps and no error, not an empty-slice surprise.
	steps, err = r.PlanSteps(K8sFargate, []Recommendation{suppressed}, Guard{Now: now})
	if err != nil || steps != nil {
		t.Fatalf("all-suppressed plan = (%v, %v), want (nil, nil)", steps, err)
	}
}

func TestRegistryRecommendIsDeterministicAndAttributed(t *testing.T) {
	now := time.Now().UTC()
	mk := func(k Kind, ids ...string) *fakeDomain {
		d := ready(k)
		for _, id := range ids {
			rec := validRec()
			// Deliberately wrong/blank attribution: the registry stamps the
			// truth, so a domain cannot emit under another domain's name.
			rec.Target = TargetRef{Domain: "", Scope: "s", ID: id}
			d.recs = append(d.recs, rec)
		}
		return d
	}
	rng := rand.New(rand.NewSource(11))
	var want []Recommendation
	for i := 0; i < 100; i++ {
		r := NewRegistry()
		doms := []*fakeDomain{
			mk(K8sFargate, "Deployment/ns/b", "Deployment/ns/a"),
			mk(EC2, "i-2", "i-1"),
			mk(Lambda, "fn-1"),
		}
		rng.Shuffle(len(doms), func(a, b int) { doms[a], doms[b] = doms[b], doms[a] })
		for _, d := range doms {
			if err := r.Register(d); err != nil {
				t.Fatal(err)
			}
		}
		got := r.Recommend(now, nil)
		if len(got) != 5 {
			t.Fatalf("got %d recommendations", len(got))
		}
		for _, rec := range got {
			if !rec.Target.Domain.Valid() {
				t.Fatalf("unattributed recommendation %+v", rec.Target)
			}
		}
		if want == nil {
			want = got
			continue
		}
		for j := range got {
			if got[j].Target != want[j].Target {
				t.Fatalf("registration order changed the output at %d: %v vs %v",
					j, got[j].Target, want[j].Target)
			}
		}
	}
	// Kind order is canonical regardless of registration order.
	if want[0].Target.Domain != EC2 || want[len(want)-1].Target.Domain != Lambda {
		t.Fatalf("kinds not in canonical order: %v … %v",
			want[0].Target.Domain, want[len(want)-1].Target.Domain)
	}
}

func TestRegistryHealthAndLearnRouting(t *testing.T) {
	now := time.Now().UTC()
	fg, ec := ready(K8sFargate), ready(EC2)
	ec.health.ReportOnly = true
	ec.health.Reason = "no credentials"
	r := NewRegistry()
	for _, d := range []*fakeDomain{fg, ec} {
		if err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	hs := r.Health(now)
	if len(hs) != 2 || hs[0].Kind != EC2 || hs[1].Kind != K8sFargate {
		t.Fatalf("health not in canonical kind order: %+v", hs)
	}
	if !hs[0].ReportOnly || hs[0].Reason != "no credentials" {
		t.Fatalf("health lost the reason: %+v", hs[0])
	}
	if err := r.Learn(&Snapshot{Domain: EC2}); err != nil {
		t.Fatal(err)
	}
	if ec.learned != 1 || fg.learned != 0 {
		t.Fatalf("snapshot routed wrong: ec2=%d fargate=%d", ec.learned, fg.learned)
	}
}

func TestGuardAllow(t *testing.T) {
	inWindow, err := guard.ParseWindows("Mon-Sun 00:00-23:59")
	if err != nil {
		t.Fatal(err)
	}
	outWindow, err := guard.ParseWindows("Mon-Sun 03:00-03:30")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) // Wednesday noon
	for _, tc := range []struct {
		name string
		g    Guard
		want error
	}{
		{"clean", Guard{Now: now}, nil},
		{"freeze beats everything", Guard{Now: now, Freeze: true, BreakerOpen: true}, ErrFrozen},
		{"breaker", Guard{Now: now, BreakerOpen: true}, ErrBreakerOpen},
		{"inside the window", Guard{Now: now, Windows: inWindow}, nil},
		{"outside the window", Guard{Now: now, Windows: outWindow}, ErrOutsideWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.g.Allow()
			if tc.want == nil && err != nil {
				t.Fatalf("Allow() = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Allow() = %v, want %v", err, tc.want)
			}
		})
	}
	// Guardrails are checked before the domain is asked to plan.
	fd := ready(K8sFargate)
	r := NewRegistry()
	if err := r.Register(fd); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PlanSteps(K8sFargate, []Recommendation{validRec()},
		Guard{Now: now, Freeze: true}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen PlanSteps = %v", err)
	}
	if len(fd.planned) != 0 {
		t.Error("the domain planned under a freeze")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
