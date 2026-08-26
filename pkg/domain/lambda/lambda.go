// Package lambda wires pkg/lambda's advisory domain into the seam, and closes
// the one gap that package could not close itself.
//
// pkg/lambda/FINDINGS.md §8 states it plainly: `domain.Snapshot` cannot carry
// a REPORT record. A [domain.Sample] is one (metric, float64, timestamp)
// triple; a REPORT record is four numbers whose CORRELATION is the entire
// point — the memory setting an invocation ran at, and the duration it took
// THERE. Split one record into three samples and the correlation is gone, and
// with it every multi-memory-point comparison in that package, which is to say
// every cost claim. So pkg/lambda grew two ingest paths, said the fix belonged
// in pkg/domain, and stopped.
//
// The fix landed: [domain.Snapshot] now carries an opaque per-domain Payload.
// This adapter decodes it and feeds the NATIVE path, so the brain has one
// ingest path again and a function is no longer refused with
// `no-report-evidence` merely because its evidence had to cross a package
// boundary.
//
// Everything else is pass-through. The domain stays advisory: PlanSteps
// refuses unconditionally, Health is always report-only, and there is no
// actuator to register.
package lambda

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	klambda "github.com/agenticode/kilter/pkg/lambda"
)

// Kind is the compute domain this adapter serves.
const Kind = domain.Lambda

// Config wires the domain.
type Config struct {
	// Sizer tunes pkg/lambda. Zero value ⇒ klambda.DefaultConfig().
	Sizer klambda.Config
}

// Domain is pkg/lambda's domain plus payload-aware ingest and refusals.
type Domain struct {
	*klambda.Domain
}

// New builds the domain.
func New(cfg Config) (*Domain, error) {
	sc := cfg.Sizer
	if sc.MemHeadroom <= 0 && sc.MinWindow <= 0 && sc.MinSamplesPerPoint <= 0 {
		def := klambda.DefaultConfig()
		def.Scope, def.Region = sc.Scope, sc.Region
		if sc.Rates != (klambda.Rates{}) {
			def.Rates = sc.Rates
		}
		sc = def
	}
	d, err := klambda.NewDomain(sc)
	if err != nil {
		return nil, fmt.Errorf("domain/lambda: %w", err)
	}
	return &Domain{Domain: d}, nil
}

// Learn routes the generic envelope to the ingest path that preserves the
// evidence.
//
// A Payload is the native snapshot and goes to Observe, which keeps the REPORT
// records intact. No payload falls through to pkg/lambda's own Learn, which
// takes what the generic shape genuinely carries — inventory, tags,
// provisioned concurrency, aggregate CloudWatch samples — and lets every
// affected function refuse honestly with `no-report-evidence`. Both paths are
// correct; only one can produce a cost claim, and the caller no longer has to
// know which it is holding.
func (d *Domain) Learn(snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q delivered to %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	if len(snap.Payload) == 0 {
		return d.Domain.Learn(snap)
	}
	var native klambda.Snapshot
	if err := json.Unmarshal(snap.Payload, &native); err != nil {
		return fmt.Errorf("domain/lambda: decode payload: %w", err)
	}
	if native.Domain != "" && native.Domain != Kind {
		return nil
	}
	if native.Scope == "" {
		native.Scope = snap.Scope
	}
	if native.Timestamp.IsZero() {
		native.Timestamp = snap.Timestamp
	}
	if snap.Stale {
		native.Stale = true
	}
	return d.Observe(&native)
}

// Refusals implements [domain.Refuser].
//
// On a fleet that has never been power-tuned this is the ENTIRE output: every
// function refuses with `single-memory-point`, no recommendation exists, and
// pkg/lambda's own FINDINGS calls that "the correct output". Without this
// projection a user would see an empty report and conclude the tool found
// nothing — a materially different claim from "we measured 200 functions at
// one memory setting each and nobody, including this tool, knows what a second
// setting would cost".
func (d *Domain) Refusals(now time.Time, ledger domain.Netter) []domain.Refusal {
	rep := d.Report(now, ledger)
	if rep == nil {
		return nil
	}
	out := make([]domain.Refusal, 0, len(rep.Assessments))
	for _, a := range rep.Assessments {
		if _, ok := a.Recommendation(); ok {
			continue
		}
		code, reason, validFrom := "unstated",
			"the sizer produced neither a proposal nor a reason", time.Time{}
		if len(a.Suppressions) > 0 {
			s := a.Suppressions[0]
			code, reason, validFrom = s.Code, s.Reason, s.ValidFrom
		}
		ref := a.Target
		ref.Domain = Kind
		out = append(out, domain.Refusal{
			Target: ref, Code: code, Reason: reason, ValidFrom: validFrom,
		})
	}
	domain.SortRefusals(out)
	return out
}
