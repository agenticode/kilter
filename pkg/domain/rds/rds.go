// Package rds wires pkg/rds's read-only observation domain into the seam, and
// contributes the one thing that package deliberately left to the wiring: the
// account-wide commitment usage baseline.
//
// pkg/rds is the first domain whose entire product is refusals. It proposes
// nothing — the DB instance class is unrepresentable in [rds.Proposal], not
// merely forbidden — so [rds.Domain.Recommend] returns an empty slice and
// [rds.Domain.Refusals] returns everything. The adapter therefore adds no
// recommendation plumbing; the seam already renders refusals beside
// recommendations, which is exactly what this domain needed to exist.
//
// # What the adapter adds, and why it could not live in pkg/rds
//
// One method: [Domain.UsageLines]. pkg/rds ships [rds.UsageLines] as a pure
// function over one instance because the *sizer* needs it to build a
// before/after pair, and it stops there on purpose — a domain knows only its
// own targets and must not be tempted to construct an account-wide view
// (pkg/domain/ledger.go's argument for [domain.Netter]). Projecting a whole
// report into baseline lines is the brain's job, and the brain reaches for it
// through the `UsageLines(now, ledger)` shape cmd/ already uses for pkg/ec2
// and pkg/ebs.
//
// # Report-only is not this package's promise to keep
//
// pkg/rds's Health is unconditionally report-only and its PlanSteps refuses
// unconditionally, and neither fact is why nothing can be actuated here. The
// core refuses first: [domain.Registry.PlanSteps] checks Health before the
// domain is consulted, and [domain.Registry.Execute] routes through an
// actuator table only cmd/ can write to, which has no `rds` row and cannot
// grow one — there is no mutating RDS API anywhere in the tree.
// TestAHostileRDSDomainCannotActuateOrBorrowAnotherDomainsActuator in
// pkg/domain asserts that against a domain built to lie about all three.
package rds

import (
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/pricing/commit"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// Kind is the compute domain this adapter serves. It is the same value
// pkg/rds declares, and it became registrable when pkg/domain's closed set
// grew the row (pkg/rds/FINDINGS.md §6.1).
const Kind = domain.RDS

// Config wires the domain.
type Config struct {
	// Scope is the accountID/region this collector covers.
	Scope string
	// Region labels commitment usage lines.
	Region string
	// Rates prices instance-hours and storage. The zero value means
	// [rds.DefaultRates], every row of which is `unverified` and therefore
	// able to size a reported fact and unable to become a claimed saving.
	Rates krds.RateCard
}

// Domain is pkg/rds's read-only domain plus the account-wide usage projection.
type Domain struct {
	*krds.Domain
}

// New builds the domain.
func New(cfg Config) (*Domain, error) {
	sc := krds.DefaultConfig()
	sc.Scope, sc.Region = cfg.Scope, cfg.Region
	if len(cfg.Rates.Classes) > 0 {
		sc.Rates = cfg.Rates
	}
	d, err := krds.NewDomain(sc)
	if err != nil {
		return nil, fmt.Errorf("domain/rds: %w", err)
	}
	return &Domain{Domain: d}, nil
}

// UsageLines projects every priced DB instance into the account-wide
// commitment baseline, as `cmd/`'s two-pass ledger build expects.
//
// # Which instances contribute, and why that is the right set
//
// Exactly the assessments pkg/rds could price. An EXCLUDED instance — Aurora,
// a Multi-AZ DB cluster member, an unknown engine, an unreadable topology,
// `kilter.dev/mode=off` — never reaches the price step in
// `Sizer.assessTarget`, so its CostKnown is false and it contributes nothing.
// That is the honest direction: a baseline line for an instance nobody could
// price would carry a rate of zero, and a zero-rate line in the baseline makes
// a Reserved DB Instance look like it is absorbing usage that costs nothing,
// which OVERSTATES absorption and therefore overstates every other domain's
// saving. Under-claiming is the only safe way to be wrong here.
//
// The ledger argument is accepted to satisfy the shape `cmd/` calls through
// and is deliberately unused: the baseline is what the account costs today at
// on-demand rates, which is what a commitment is applied *to*. Netting it
// against the commitments while computing it would be circular.
func (d *Domain) UsageLines(now time.Time, _ domain.Netter) []commit.UsageLine {
	rep := d.Report(now, nil)
	if rep == nil {
		return nil
	}
	out := make([]commit.UsageLine, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		if !a.CostKnown {
			continue
		}
		// Same call the sizer's own before/after pair is built from
		// (pkg/rds/sizer.go), with the DEPLOYMENT-ADJUSTED hourly rate — so a
		// Multi-AZ instance's line costs twice a Single-AZ one's exactly as it
		// consumes twice the normalized units. The two halves of trap 10 stay
		// in step because they come from one multiplier.
		out = append(out, krds.UsageLines(a.Target, a.Instance, a.Engine, a.Deployment, a.CurrentHourlyUSD)...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
