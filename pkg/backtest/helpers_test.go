package backtest

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/recommend"
)

// allKinds is every synthetic archetype, in a fixed order so table-driven
// tests report failures in a stable sequence.
var allKinds = []TraceKind{TraceSteady, TraceDiurnal, TraceBursty, TraceRegimeChange}

// calmKinds are the archetypes on which the shipped default policy causes no
// violations, so the efficiency argument can be made without the risk term
// entering the comparison at all.
var calmKinds = []TraceKind{TraceSteady, TraceDiurnal, TraceBursty}

func mustTrace(t *testing.T, spec TraceSpec) *Trace {
	t.Helper()
	tr, err := spec.Build()
	if err != nil {
		t.Fatalf("building %s trace: %v", spec.Kind, err)
	}
	return tr
}

func mustStore(t *testing.T, tr *Trace) *evidence.Memory {
	t.Helper()
	st, err := tr.Store()
	if err != nil {
		t.Fatalf("seeding evidence: %v", err)
	}
	return st
}

func mustRun(t *testing.T, h *Harness, tr *Trace) *Scorecard {
	t.Helper()
	sc, err := h.Run(tr.Cluster, tr.Start, tr.End, 24*time.Hour)
	if err != nil {
		t.Fatalf("running backtest: %v", err)
	}
	return sc
}

// policy names one configuration under test. The refuse-everything variants
// exist to prove they cannot win; the aggressive one to prove undersizing is
// punished; conservative to prove the curve has two sides.
type policy struct {
	name string
	rec  recommend.Config
	pl   plan.Config
	dec  decision.Config
}

func (p policy) harness(tr *Trace) *Harness {
	return &Harness{History: tr.Source(), Rec: p.rec, Plan: p.pl, Decision: p.dec}
}

func defaultPolicy() policy {
	return policy{name: "default", rec: recommend.DefaultConfig(),
		pl: plan.DefaultConfig(), dec: decision.DefaultConfig()}
}

// refuseByHistory never has "enough" history, so the recommender is silent at
// every instant — the purest refuse-everything policy.
func refuseByHistory() policy {
	p := defaultPolicy()
	p.name = "refuse-all-history"
	p.rec.MinSamples = 1 << 30
	p.dec.MinSamples = 1 << 30
	return p
}

// refuseByMode recommends normally but runs the whole cluster in
// advisory mode — the real, popular way an operator refuses everything.
func refuseByMode() policy {
	p := defaultPolicy()
	p.name = "refuse-all-mode"
	p.pl.DefaultMode = guard.ModeRecommend
	return p
}

// starving sizes CPU from a low percentile with no headroom. It is
// deliberately unsafe: its job in the suite is to be caught.
func starving() policy {
	p := defaultPolicy()
	p.name = "starving"
	p.rec.CPUPercentile = 0.25
	p.rec.CPUHeadroom = 1.0
	return p
}

// conservative buys a lot of headroom it will mostly not use.
func conservative() policy {
	p := defaultPolicy()
	p.name = "conservative"
	p.rec.CPUPercentile = 0.99
	p.rec.CPUHeadroom = 2.0
	p.rec.MemoryHeadroom = 2.0
	return p
}

func namedPolicies() []policy {
	return []policy{defaultPolicy(), refuseByHistory(), refuseByMode(), starving(), conservative()}
}
