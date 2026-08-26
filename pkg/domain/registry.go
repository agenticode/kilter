package domain

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Registry is the explicit wiring point for domains — `cmd/` constructs one and
// registers whatever its flags configure. It is not global state, and there is
// no init-time self-registration: a domain exists in a process because someone
// wrote the line that put it there.
//
// A zero-value Registry, a nil *Registry, and a Registry with nothing
// registered all behave identically and harmlessly: no recommendations, no
// steps, no errors. That is the "organ, not heart" rule made mechanical.
type Registry struct {
	mu      sync.RWMutex
	domains map[Kind]Domain
	// actuators is the actuation-capability table. It is deliberately a
	// SECOND map, written only by cmd/: see the note in actuation.go.
	actuators map[Kind]Actuator
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a domain. Registering two domains for the same kind, a nil
// domain, or a domain reporting an unknown kind is a wiring bug and is
// reported, never silently resolved.
func (r *Registry) Register(d Domain) error {
	if r == nil {
		return fmt.Errorf("domain: Register on a nil registry")
	}
	if d == nil {
		return fmt.Errorf("domain: Register(nil)")
	}
	k := d.Kind()
	if !k.Valid() {
		return fmt.Errorf("domain: Register: unknown kind %q", k)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.domains[k]; dup {
		return fmt.Errorf("domain: Register: %q already registered", k)
	}
	if r.domains == nil {
		r.domains = map[Kind]Domain{}
	}
	r.domains[k] = d
	return nil
}

// Len reports how many domains are registered.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.domains)
}

// Kinds returns the registered kinds in canonical order. Every iteration over
// the registry goes through this, so no output can depend on map order.
func (r *Registry) Kinds() []Kind {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.domains) == 0 {
		return nil
	}
	out := make([]Kind, 0, len(r.domains))
	for k := range r.domains {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Get returns the domain registered for a kind.
func (r *Registry) Get(k Kind) (Domain, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.domains[k]
	return d, ok
}

// Learn routes a snapshot to its domain. A nil snapshot is a no-op. A snapshot
// for a kind nobody registered returns ErrNotRegistered: with zero domains
// registered nothing is collected in the first place, so this can only mean a
// collector is running that the brain was not told about.
func (r *Registry) Learn(snap *Snapshot) error {
	if snap == nil {
		return nil
	}
	d, ok := r.Get(snap.Domain)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotRegistered, snap.Domain)
	}
	return d.Learn(snap)
}

// Recommend collects recommendations from every registered domain, in canonical
// kind order, sorted. With zero domains registered it returns nil.
//
// Recommendations from report-only domains ARE included — report-only means
// report — and are made unplannable by PlanSteps, not by hiding them. The
// registry stamps each recommendation's target domain from the domain that
// produced it, so a domain cannot emit recommendations attributed to another.
func (r *Registry) Recommend(now time.Time, ledger Netter) []Recommendation {
	var out []Recommendation
	for _, k := range r.Kinds() {
		d, ok := r.Get(k)
		if !ok {
			continue
		}
		recs := d.Recommend(now, ledger)
		for i := range recs {
			recs[i].Target.Domain = k
		}
		out = append(out, recs...)
	}
	SortRecommendations(out)
	return out
}

// Health reports every registered domain's readiness, in canonical kind order.
// With zero domains registered it returns nil — there is nothing unhealthy
// about a core with no organs attached.
func (r *Registry) Health(now time.Time) []Health {
	var out []Health
	for _, k := range r.Kinds() {
		d, ok := r.Get(k)
		if !ok {
			continue
		}
		h := d.Health(now)
		h.Kind = k
		out = append(out, h)
	}
	return out
}

// PlanSteps builds executable steps for one domain.
//
// This is where report-only is enforced, and it is enforced by the core rather
// than by trusting each domain: a domain whose collector is absent, whose
// credentials are missing, or which is configured advisory-only never gets the
// chance to produce a step, however confident its recommendations are.
// Suppressed recommendations and recommendations belonging to another domain
// are filtered out before the domain sees them.
func (r *Registry) PlanSteps(k Kind, recs []Recommendation, g Guard) ([]Step, error) {
	d, ok := r.Get(k)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotRegistered, k)
	}
	if h := d.Health(g.Now); h.ReportOnly {
		return nil, fmt.Errorf("%w: %s: %s", ErrReportOnly, k, h.Reason)
	}
	if err := g.Allow(); err != nil {
		return nil, err
	}
	applicable := make([]Recommendation, 0, len(recs))
	for _, rec := range recs {
		if rec.Suppressed || rec.Target.Domain != k {
			continue
		}
		applicable = append(applicable, rec)
	}
	if len(applicable) == 0 {
		return nil, nil
	}
	SortRecommendations(applicable)
	steps, err := d.PlanSteps(applicable, g)
	if err != nil {
		return nil, err
	}
	if err := validateSteps(k, steps); err != nil {
		return nil, err
	}
	return steps, nil
}

// validateSteps checks what a domain handed back before the core will carry
// it any further.
//
// Filtering the INPUT is not enough. A domain is ordinary Go code: it can
// return a step for a target it was never given, in a domain it does not own.
// Without this check, [Registry.Execute] would route that step by
// Step.Target.Domain straight into ANOTHER domain's actuator — a domain
// escaping its own boundary through the core that exists to hold it there. So
// every step is required to name the domain that produced it, to carry a
// valid action class (the executor's disruption accounting keys off it), and
// to carry an idempotency key (without one, a resumed plan re-executes).
func validateSteps(k Kind, steps []Step) error {
	for i, s := range steps {
		switch {
		case s.Target.Domain != k:
			return fmt.Errorf("domain: %s returned step %d for another domain (%q): %w",
				k, i, s.Target.Domain, ErrWrongDomain)
		case !s.Action.Valid():
			return fmt.Errorf("domain: %s returned step %d (%s) with invalid action %q",
				k, i, s.Target, s.Action)
		case s.Key == "":
			return fmt.Errorf("domain: %s returned step %d (%s) with no idempotency key",
				k, i, s.Target)
		}
	}
	return nil
}
