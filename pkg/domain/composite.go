package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A [Registry] holds exactly one [Domain] per [Kind], and that is the right
// rule: a Kind names a billable target scope, and two domains claiming one
// scope is a wiring bug the registry should refuse rather than resolve by
// coin-flip.
//
// It collides with reality in exactly one place. `ec2` is a single Kind, but
// two packages implement halves of it: pkg/ec2 sizes instances and pkg/ebs
// sizes the volumes attached to them. Both report [EC2] from Kind(), because
// both are correct — §5.1 places volumes under the EC2 domain, both share the
// `accountID/region` scope, and one cloud agent collects both.
//
// Composite is the resolution pkg/ebs/FINDINGS.md §6.3 asks for: a domain that
// IS the Kind, and fans every seam method out to the halves that compose it.
// The alternative — adding an `ebs` Kind — was rejected; cmd/FINDINGS.md §2
// records why in full, but the short version is that pkg/ebs bakes
// domain.EC2 into [StepKey], so re-labelling its steps would change their
// idempotency keys and silently break the already-shipped, already-tested EBS
// actuator's resume path.
//
// Routing is explicit. Each [Part] declares which recommendations it owns, and
// the wiring supplies that predicate — the composite never guesses from an ID
// prefix it does not control.
type Composite struct {
	kind  Kind
	parts []Part
}

// Part is one half of a composite domain.
type Part struct {
	// Name identifies the part in health reasons and checkpoints. It must be
	// unique within the composite and stable across releases: a checkpoint
	// written under one name is restored by that name.
	Name string
	// Domain is the half. Its Kind() must equal the composite's.
	Domain Domain
	// Owns reports whether a recommendation belongs to this part. It is
	// consulted only on the planning path, where a recommendation must reach
	// the half that can execute it. Exactly one part should own any given
	// recommendation; a recommendation no part claims is not planned, and one
	// two parts claim is planned by the first in registration order.
	Owns func(Recommendation) bool
	// Accepts reports whether a snapshot is addressed to this part. Nil means
	// "every snapshot for this kind", which is the right default for a half
	// that can tell on its own what it is looking at.
	//
	// It exists because several snapshots arrive per Kind — one per collector
	// — and a half handed a snapshot meant for its sibling may not be able to
	// distinguish "this is not for me" from "my collector found nothing". A
	// half that draws the second conclusion degrades itself, and then the
	// domain's answer depends on the order the snapshots happened to be fed
	// in. cmd/FINDINGS.md §5.1 records the concrete case.
	Accepts func(*Snapshot) bool
}

// NewComposite builds a composite domain for one kind. Every part must report
// that kind: a part that disagrees is a wiring bug, not something to paper
// over, because the composite's whole job is to make one Kind mean one thing.
func NewComposite(k Kind, parts ...Part) (*Composite, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("domain: NewComposite: unknown kind %q", k)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("domain: NewComposite(%s): no parts", k)
	}
	seen := make(map[string]bool, len(parts))
	for i, p := range parts {
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("domain: NewComposite(%s): part %d has no name", k, i)
		case seen[p.Name]:
			return nil, fmt.Errorf("domain: NewComposite(%s): duplicate part %q", k, p.Name)
		case p.Domain == nil:
			return nil, fmt.Errorf("domain: NewComposite(%s): part %q is nil", k, p.Name)
		case p.Owns == nil:
			return nil, fmt.Errorf("domain: NewComposite(%s): part %q has no ownership predicate", k, p.Name)
		case p.Domain.Kind() != k:
			return nil, fmt.Errorf("domain: NewComposite(%s): part %q reports kind %q",
				k, p.Name, p.Domain.Kind())
		}
		seen[p.Name] = true
	}
	out := &Composite{kind: k, parts: make([]Part, len(parts))}
	copy(out.parts, parts)
	return out, nil
}

// Kind implements [Domain].
func (c *Composite) Kind() Kind { return c.kind }

// PartNames returns the part names in registration order.
func (c *Composite) PartNames() []string {
	out := make([]string, len(c.parts))
	for i, p := range c.parts {
		out[i] = p.Name
	}
	return out
}

// Parts returns a copy of the parts, in registration order. It exists so a
// caller can reach a half's package-native surface — the account-wide usage
// lines the brain needs for its commitment baseline, a domain-specific report
// — without the composite having to re-export every method its parts have.
// The slice is copied; the Domain values inside are the live parts.
func (c *Composite) Parts() []Part {
	out := make([]Part, len(c.parts))
	copy(out, c.parts)
	return out
}

// Part returns one part by name.
func (c *Composite) Part(name string) (Part, bool) {
	for _, p := range c.parts {
		if p.Name == name {
			return p, true
		}
	}
	return Part{}, false
}

// Learn hands the snapshot to every part that accepts it (see [Part.Accepts]).
//
// Errors are joined rather than short-circuited: a part that cannot read a
// snapshot must not stop the others from reading it, because a degraded half
// is an operational condition and a dead brain is not.
func (c *Composite) Learn(snap *Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != c.kind {
		return fmt.Errorf("%w: %q delivered to %q", ErrWrongDomain, snap.Domain, c.kind)
	}
	var errs []error
	for _, p := range c.parts {
		if p.Accepts != nil && !p.Accepts(snap) {
			continue
		}
		if err := p.Domain.Learn(snap); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Recommend concatenates every part's recommendations, stamps the composite's
// kind on each, and sorts. Output order is therefore independent of part
// order and of anything inside a part.
func (c *Composite) Recommend(now time.Time, ledger Netter) []Recommendation {
	var out []Recommendation
	for _, p := range c.parts {
		recs := p.Domain.Recommend(now, ledger)
		for i := range recs {
			recs[i].Target.Domain = c.kind
		}
		out = append(out, recs...)
	}
	SortRecommendations(out)
	return out
}

// Refusals collects the target-level refusals of every part that can explain
// them (see [Refuser]), sorted.
func (c *Composite) Refusals(now time.Time, ledger Netter) []Refusal {
	var out []Refusal
	for _, p := range c.parts {
		r, ok := p.Domain.(Refuser)
		if !ok {
			continue
		}
		refs := r.Refusals(now, ledger)
		for i := range refs {
			refs[i].Target.Domain = c.kind
		}
		out = append(out, refs...)
	}
	SortRefusals(out)
	return out
}

// Health merges the parts' health.
//
// The two merges are deliberately asymmetric:
//
//   - Ready is true when ANY part is ready. The domain can say something
//     useful about the account even if one half's collector is down, and
//     saying it is the point.
//   - ReportOnly is true only when EVERY part is report-only. If one half can
//     act, the domain can act — and [Composite.PlanSteps] then plans steps for
//     that half ALONE, because it re-checks each part's own report-only state
//     before routing anything to it. A part that cannot act never gets a step,
//     however healthy its sibling is.
//
// Reason names every part that is degraded, so "ec2 is report-only" never
// reaches a human without saying which half and why.
func (c *Composite) Health(now time.Time) Health {
	out := Health{Kind: c.kind, ReportOnly: true}
	reasons := make([]string, 0, len(c.parts))
	for _, p := range c.parts {
		h := p.Domain.Health(now)
		if h.Ready {
			out.Ready = true
		}
		if !h.ReportOnly {
			out.ReportOnly = false
		}
		out.Targets += h.Targets
		if h.LastSnapshot.After(out.LastSnapshot) {
			out.LastSnapshot = h.LastSnapshot
		}
		if !h.Ready || h.ReportOnly {
			reasons = append(reasons, p.Name+": "+describeHealth(h))
		}
	}
	out.Reason = strings.Join(reasons, "; ")
	return out
}

// describeHealth renders one part's degradation in prose, never empty.
func describeHealth(h Health) string {
	reason := h.Reason
	if reason == "" {
		switch {
		case !h.Ready && h.ReportOnly:
			reason = "not ready, report-only"
		case !h.Ready:
			reason = "not ready"
		default:
			reason = "report-only"
		}
	}
	return reason
}

// PlanSteps routes each recommendation to the part that owns it, and plans
// only for parts that may act.
//
// A part whose own Health says report-only is skipped: its recommendations
// stay visible in the report (they were never hidden) but cannot become steps.
// This is why the composite's permissive ReportOnly merge is safe — the
// composite being actuatable never makes a report-only HALF actuatable.
//
// Steps are re-sequenced across parts so Seq is dense and ordered, and capped
// by g.MaxSteps after the merge — a cap that applied per part would let a
// two-part domain execute twice the plan the operator authorized.
func (c *Composite) PlanSteps(recs []Recommendation, g Guard) ([]Step, error) {
	if err := g.Allow(); err != nil {
		return nil, err
	}
	routed := make([][]Recommendation, len(c.parts))
	for _, rec := range recs {
		if rec.Suppressed {
			continue
		}
		for i, p := range c.parts {
			if p.Owns(rec) {
				routed[i] = append(routed[i], rec)
				break
			}
		}
	}
	var out []Step
	var errs []error
	for i, p := range c.parts {
		if len(routed[i]) == 0 {
			continue
		}
		if h := p.Domain.Health(g.Now); h.ReportOnly {
			continue
		}
		sub := g
		sub.MaxSteps = 0 // capped once, after the merge
		steps, err := p.Domain.PlanSteps(routed[i], sub)
		if err != nil {
			// A report-only half is not an error — it is the product — but a
			// half that fails for any other reason must be reported.
			if errors.Is(err, ErrReportOnly) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			continue
		}
		if err := validateSteps(c.kind, steps); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name, err))
			continue
		}
		out = append(out, steps...)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := out[i].Target.Compare(out[j].Target); c != 0 {
			return c < 0
		}
		return out[i].Key < out[j].Key
	})
	if g.MaxSteps > 0 && len(out) > g.MaxSteps {
		out = out[:g.MaxSteps]
	}
	for i := range out {
		out[i].Seq = i + 1
	}
	return out, nil
}

// compositeCheckpoint is the persisted form: one entry per part, sorted by
// name so the bytes are stable regardless of registration order.
type compositeCheckpoint struct {
	Version int                  `json:"version"`
	Kind    Kind                 `json:"kind"`
	Parts   []compositePartState `json:"parts,omitempty"`
}

type compositePartState struct {
	Name string `json:"name"`
	Blob []byte `json:"blob,omitempty"`
}

// Checkpoint serializes every part, deterministically.
func (c *Composite) Checkpoint() ([]byte, error) {
	cp := compositeCheckpoint{Version: 1, Kind: c.kind}
	for _, p := range c.parts {
		blob, err := p.Domain.Checkpoint()
		if err != nil {
			return nil, fmt.Errorf("domain: checkpoint %s/%s: %w", c.kind, p.Name, err)
		}
		cp.Parts = append(cp.Parts, compositePartState{Name: p.Name, Blob: blob})
	}
	sort.Slice(cp.Parts, func(i, j int) bool { return cp.Parts[i].Name < cp.Parts[j].Name })
	return json.Marshal(cp)
}

// Restore reloads every part. An entry for a part this composite does not have
// is an error: it means the checkpoint was written by a different wiring, and
// silently discarding half a domain's learned state is how a resized volume
// loses its cooldown.
func (c *Composite) Restore(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var cp compositeCheckpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return fmt.Errorf("domain: restore %s: %w", c.kind, err)
	}
	if cp.Kind != "" && cp.Kind != c.kind {
		return fmt.Errorf("%w: checkpoint for %q restored into %q", ErrWrongDomain, cp.Kind, c.kind)
	}
	byName := make(map[string]Domain, len(c.parts))
	for _, p := range c.parts {
		byName[p.Name] = p.Domain
	}
	var errs []error
	for _, ps := range cp.Parts {
		d, ok := byName[ps.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("checkpoint names unknown part %q", ps.Name))
			continue
		}
		if err := d.Restore(ps.Blob); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ps.Name, err))
		}
	}
	return errors.Join(errs...)
}
