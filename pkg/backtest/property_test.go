package backtest

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
)

var propStart = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// TestRefuseEverythingCannotDominate is the property the whole scorecard has
// to satisfy to be worth anything. If a policy can win by never deciding,
// then "kilter's judgment is good" is unfalsifiable and this package is
// decorative.
//
// The argument is made where it is airtight: on the archetypes where the
// default policy causes no violations at all. There the risk term is zero on
// both sides, so the comparison is pure efficiency and cannot be flipped by
// any choice of IncidentUSD — which the test demonstrates by sweeping it over
// four orders of magnitude.
func TestRefuseEverythingCannotDominate(t *testing.T) {
	for _, kind := range calmKinds {
		for _, refuser := range []policy{refuseByHistory(), refuseByMode()} {
			t.Run(string(kind)+"/"+refuser.name, func(t *testing.T) {
				tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 3})
				for _, incident := range []float64{0, 1, 50, 1000} {
					scoring := DefaultConfig()
					scoring.Cost.IncidentUSD = incident

					base := defaultPolicy().harness(tr)
					base.Scoring = scoring
					baseCard := mustRun(t, base, tr)

					ref := refuser.harness(tr)
					ref.Scoring = scoring
					refCard := mustRun(t, ref, tr)

					if refCard.Decisions != 0 {
						t.Fatalf("%s planned %d resizes; it is not a refuse-everything policy",
							refuser.name, refCard.Decisions)
					}
					if baseCard.Decisions == 0 {
						t.Fatalf("the default policy decided nothing; the comparison is vacuous")
					}
					if baseCard.MemViolations != 0 || baseCard.CPUStarvation != 0 {
						t.Fatalf("%s is not a calm archetype for the default policy: mem=%d cpu=%d",
							kind, baseCard.MemViolations, baseCard.CPUStarvation)
					}
					if refCard.RegretUSD <= baseCard.RegretUSD {
						t.Fatalf("incident=$%g: refusing everything scored regret $%.4f, "+
							"no worse than deciding ($%.4f) — the scoring lets abstention win",
							incident, refCard.RegretUSD, baseCard.RegretUSD)
					}
					if ok, _ := Gate(baseCard, refCard, DefaultTolerance()); ok {
						t.Fatalf("incident=$%g: the gate admitted a refuse-everything policy", incident)
					}
					// Refusing is costed, not merely uncounted.
					if refCard.RefusalsIdle == 0 {
						t.Fatalf("no refusal was recognised as idle over a boring window")
					}
					if refCard.ForgoneSavingsUSD <= 0 {
						t.Fatalf("idle refusals cost $%.4f in forgone savings, want > 0",
							refCard.ForgoneSavingsUSD)
					}
					if refCard.OracleGapPct <= baseCard.OracleGapPct {
						t.Fatalf("refusing everything reports an oracle gap of %.2f%%, "+
							"no worse than deciding (%.2f%%)", refCard.OracleGapPct, baseCard.OracleGapPct)
					}
				}
			})
		}
	}
}

// TestDecidingDominatesRefusingOnCalmHistory is the same claim from the other
// side: the default policy must actually pass the gate against a
// refuse-everything incumbent.
func TestDecidingDominatesRefusingOnCalmHistory(t *testing.T) {
	for _, kind := range calmKinds {
		t.Run(string(kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 3})
			refCard := mustRun(t, refuseByHistory().harness(tr), tr)
			baseCard := mustRun(t, defaultPolicy().harness(tr), tr)
			ok, reasons := Gate(refCard, baseCard, DefaultTolerance())
			if !ok {
				t.Fatalf("the default policy failed the gate against a do-nothing incumbent: %v", reasons)
			}
		})
	}
}

// TestUndersizingIsPunished is the mirror property: a policy that saves money
// by taking risk must show the risk. Without this, "cheapest wins" would be
// the whole scorecard and safety would be advisory.
func TestUndersizingIsPunished(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceDiurnal, Start: propStart, Days: 7, Workloads: 3})
	base := mustRun(t, defaultPolicy().harness(tr), tr)
	agg := mustRun(t, starving().harness(tr), tr)

	if agg.CPUStarvation <= base.CPUStarvation {
		t.Fatalf("sizing CPU from the median caused %d starvation events vs the default's %d; "+
			"the safety predicate is not detecting undersizing", agg.CPUStarvation, base.CPUStarvation)
	}
	if agg.RiskRegretUSD <= 0 {
		t.Fatalf("starvation was counted but priced at $%.4f", agg.RiskRegretUSD)
	}
	if agg.ResourceRegretUSD >= base.ResourceRegretUSD {
		t.Fatalf("the starving policy should at least be cheaper on resources: $%.4f vs $%.4f",
			agg.ResourceRegretUSD, base.ResourceRegretUSD)
	}
	if ok, reasons := Gate(base, agg, DefaultTolerance()); ok {
		t.Fatalf("the gate admitted a policy that starves CPU (reasons: %v)", reasons)
	}
	// And the claimed-vs-realized join must notice that some of the promised
	// saving could not have been kept.
	if !(agg.ClaimedVsRealized < 1) {
		t.Fatalf("an undersizing policy realized %.4f of its claim; want < 1", agg.ClaimedVsRealized)
	}
}

// TestOracleIsIndependentOfThePolicyUnderTest is the structural guarantee
// stated in oracle.go: the reference sizing, and the set of pairs it is
// computed for, are fixed before any policy runs. Five very different
// policies must therefore agree on both, to the byte.
func TestOracleIsIndependentOfThePolicyUnderTest(t *testing.T) {
	for _, kind := range allKinds {
		t.Run(string(kind), func(t *testing.T) {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 3,
				NoisePct: 0.1, NoiseSeed: 7})
			var (
				wantOracles []string
				wantCost    float64
				wantScored  int
				from        string
			)
			for _, p := range namedPolicies() {
				recs, err := p.harness(tr).records(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				got := make([]string, 0, len(recs))
				for _, r := range recs {
					got = append(got, r.At.Format(time.RFC3339)+" "+r.Key.String()+" "+r.Oracle.String())
				}
				sc := mustRun(t, p.harness(tr), tr)
				if wantOracles == nil {
					wantOracles, wantCost, wantScored, from = got, sc.OracleCostUSD, sc.Scored, p.name
					continue
				}
				if len(got) != len(wantOracles) {
					t.Fatalf("policy %q scored %d pairs, %q scored %d: the scored set moved with the policy",
						p.name, len(got), from, len(wantOracles))
				}
				for i := range got {
					if got[i] != wantOracles[i] {
						t.Fatalf("policy %q oracle %d = %q, %q got %q: the oracle moved with the policy",
							p.name, i, got[i], from, wantOracles[i])
					}
				}
				if sc.OracleCostUSD != wantCost {
					t.Fatalf("policy %q reports oracle cost $%.6f, %q reports $%.6f",
						p.name, sc.OracleCostUSD, from, wantCost)
				}
				if sc.Scored != wantScored {
					t.Fatalf("policy %q scored %d pairs, %q scored %d", p.name, sc.Scored, from, wantScored)
				}
			}
		})
	}
}

// TestEveryScoredPairIsAccountedFor: a backtest that silently drops decisions
// reads as a clean scorecard. Decisions plus refusals must equal the scored
// set exactly, and every refusal must carry a reason.
func TestEveryScoredPairIsAccountedFor(t *testing.T) {
	for _, kind := range allKinds {
		for _, p := range namedPolicies() {
			tr := mustTrace(t, TraceSpec{Kind: kind, Start: propStart, Days: 7, Workloads: 2,
				DeployAt: []time.Duration{0}, OOMAt: []time.Duration{50 * time.Hour}})
			h := p.harness(tr)
			h.Evidence = mustStore(t, tr)
			sc := mustRun(t, h, tr)

			total := sc.Decisions
			for code, n := range sc.Refusals {
				if code == "" {
					t.Fatalf("%s/%s: %d refusals with no reason", kind, p.name, n)
				}
				if n <= 0 {
					t.Fatalf("%s/%s: refusal code %q counted %d times", kind, p.name, code, n)
				}
				total += n
			}
			if total != sc.Scored {
				t.Fatalf("%s/%s: decisions(%d) + refusals(%d) = %d, but %d pairs were scored",
					kind, p.name, sc.Decisions, total-sc.Decisions, total, sc.Scored)
			}
			if sc.RefusalsGood+sc.RefusalsIdle != total-sc.Decisions {
				t.Fatalf("%s/%s: refusal quality split %d+%d does not cover %d refusals",
					kind, p.name, sc.RefusalsGood, sc.RefusalsIdle, total-sc.Decisions)
			}
		}
	}
}

// TestRefusalOverATurbulentWindowIsCredited: a refusal whose window carried a
// real adverse event is a good refusal, not an idle one. Without this the
// scorecard would treat caution and inertia as the same thing.
func TestRefusalOverATurbulentWindowIsCredited(t *testing.T) {
	// An OOMKill lands inside the window that follows the day-2 instant.
	tr := mustTrace(t, TraceSpec{Kind: TraceSteady, Start: propStart, Days: 7, Workloads: 1,
		OOMAt: []time.Duration{60 * time.Hour}})
	h := refuseByHistory().harness(tr)
	h.Evidence = mustStore(t, tr)
	sc := mustRun(t, h, tr)

	if sc.MemOOMKills != 1 {
		t.Fatalf("the substrate's OOMKill was counted %d times, want 1", sc.MemOOMKills)
	}
	if sc.RefusalsGood != 1 {
		t.Fatalf("got %d good refusals, want exactly the one over the OOM window", sc.RefusalsGood)
	}
	if sc.RefusalsIdle != sc.Scored-1 {
		t.Fatalf("got %d idle refusals, want the remaining %d", sc.RefusalsIdle, sc.Scored-1)
	}

	// Ground truth is ground truth: it cannot move with the policy.
	h2 := defaultPolicy().harness(tr)
	h2.Evidence = mustStore(t, tr)
	if got := mustRun(t, h2, tr).MemOOMKills; got != sc.MemOOMKills {
		t.Fatalf("observed OOMKills changed with the policy: %d vs %d", got, sc.MemOOMKills)
	}
}

// TestWiringTheDecisionLayerIsAnImprovement demonstrates the harness doing
// the job it exists for: scoring a proposed policy change. pkg/decision
// shipped, but pkg/recommend does not consult it, so today a refusal
// predicate cannot stop a resize. Enforcing the shipped predicates turns the
// regime-change trace's CPU starvation off entirely — and the gate says so.
func TestWiringTheDecisionLayerIsAnImprovement(t *testing.T) {
	tr := mustTrace(t, TraceSpec{Kind: TraceRegimeChange, Start: propStart, Days: 7, Workloads: 3})
	store := mustStore(t, tr)

	asShipped := defaultPolicy().harness(tr)
	asShipped.Evidence = store
	current := mustRun(t, asShipped, tr)

	proposed := defaultPolicy().harness(tr)
	proposed.Evidence = store
	proposed.EnforceDecisionRefusals = true
	candidate := mustRun(t, proposed, tr)

	if current.CPUStarvation == 0 {
		t.Fatalf("the regime-change trace no longer starves the shipped policy; the A/B is vacuous")
	}
	if candidate.CPUStarvation >= current.CPUStarvation {
		t.Fatalf("enforcing refusals left CPU starvation at %d (was %d)",
			candidate.CPUStarvation, current.CPUStarvation)
	}
	if candidate.Refusals[string(decision.CodePostChangeSoak)] == 0 {
		t.Fatalf("no post-change-soak refusal fired; refusals were %v", candidate.Refusals)
	}
	if candidate.RegretUSD >= current.RegretUSD {
		t.Fatalf("enforcing refusals scored regret $%.4f, no better than $%.4f",
			candidate.RegretUSD, current.RegretUSD)
	}
	if ok, reasons := Gate(current, candidate, DefaultTolerance()); !ok {
		t.Fatalf("the gate rejected a strictly safer, cheaper policy: %v", reasons)
	}
	// The saved resources are paid for with foregone savings, and the
	// scorecard must show both halves of that trade rather than only the win.
	if candidate.ResourceRegretUSD <= current.ResourceRegretUSD {
		t.Fatalf("refusing during the soak should cost resource regret: $%.4f vs $%.4f",
			candidate.ResourceRegretUSD, current.ResourceRegretUSD)
	}
}
