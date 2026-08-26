// Package ecs adapts pkg/ecs to the [domain.Domain] seam.
//
// pkg/ecs (U8) stopped one file short of the seam on purpose: it is stateless
// — CloudWatch is the history store, so one collection pass carries the whole
// window and there is no cross-tick histogram to fold — and its FINDINGS §3.1
// records that the adapter a later unit writes is thin. This is that file.
//
// Thin, but not empty. Three things belong here rather than in pkg/ecs:
//
//  1. ONE COLLECTOR PER CLUSTER. pkg/ecs.Snapshot covers a single ECS cluster,
//     while a [domain.Kind] covers an account. So this adapter keeps a
//     snapshot per cluster and merges the per-cluster reports, in sorted
//     cluster order, into one domain-level answer.
//  2. Freshness. A stateless sizer has no opinion about how old its input is;
//     a domain must, or it will happily recommend against last month's
//     metrics.
//  3. Actuation availability. pkg/ecs ships a complete, tested actuator, and
//     whether one is WIRED is a fact about cmd/, not about the package.
//
// No AWS SDK is linked here. The SDK adapter that fills a snapshot belongs in
// cmd/ and is not in this unit.
package ecs

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	kecs "github.com/agenticode/kilter/pkg/ecs"
)

// Kind is the compute domain this adapter serves.
const Kind = domain.ECSFargate

// DefaultStaleAfter is how old the newest snapshot may be before the domain
// stops being ready. ECS service metrics publish at 60 s, but a collection
// pass covers a 14-day window, so a one-hour-old pass is still a good
// description of the fleet.
const DefaultStaleAfter = time.Hour

// Config wires the domain.
type Config struct {
	// Scope is the accountID/region this domain covers; snapshots override it.
	Scope string
	// Sizer tunes pkg/ecs. Zero value ⇒ kecs.DefaultConfig().
	Sizer kecs.Config
	// StaleAfter is the freshness horizon. Zero ⇒ DefaultStaleAfter.
	StaleAfter time.Duration
	// ActuationAvailable reports that an ECS actuator is wired. FALSE BY
	// DEFAULT: forgetting to wire credentials can never read as permission to
	// register a task definition.
	ActuationAvailable bool
}

// Domain is the [domain.Domain] adapter over pkg/ecs.
type Domain struct {
	sizer      *kecs.Sizer
	scope      string
	staleAfter time.Duration
	actuatable bool

	mu sync.RWMutex
	// snaps is keyed by cluster; iteration is always over sorted keys, so no
	// output can depend on map order.
	snaps map[string]*kecs.Snapshot
}

// New builds the domain.
func New(cfg Config) (*Domain, error) {
	sc := cfg.Sizer
	if sc.CPUPercentile <= 0 && sc.CPUHeadroom <= 0 && sc.MinWindow <= 0 {
		// pkg/ecs fills a partially-set config from its own defaults one field
		// at a time, so only the wholly-unset case needs help here.
		sc = kecs.DefaultConfig()
	}
	d := &Domain{
		sizer:      kecs.NewSizer(sc),
		scope:      cfg.Scope,
		staleAfter: cfg.StaleAfter,
		actuatable: cfg.ActuationAvailable,
		snaps:      map[string]*kecs.Snapshot{},
	}
	if d.staleAfter <= 0 {
		d.staleAfter = DefaultStaleAfter
	}
	if d.sizer == nil {
		return nil, fmt.Errorf("domain/ecs: sizer could not be built")
	}
	return d, nil
}

// Kind implements [domain.Domain].
func (d *Domain) Kind() domain.Kind { return Kind }

// Learn stores the cluster snapshot carried in the generic envelope's Payload.
//
// A snapshot with no payload is a no-op, not an error: an ECS-domain snapshot
// may legitimately arrive carrying only inventory the generic shape can hold.
// A payload that will not decode IS an error — a malformed snapshot must never
// be mistaken for an empty cluster.
func (d *Domain) Learn(snap *domain.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Domain != "" && snap.Domain != Kind {
		return fmt.Errorf("%w: %q delivered to %q", domain.ErrWrongDomain, snap.Domain, Kind)
	}
	if len(snap.Payload) == 0 {
		return nil
	}
	var native kecs.Snapshot
	if err := json.Unmarshal(snap.Payload, &native); err != nil {
		return fmt.Errorf("domain/ecs: decode payload: %w", err)
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
		native.StaleReason = joinReason(native.StaleReason, snap.StaleReason)
	}
	return d.Observe(&native)
}

// Observe is the native ingest path: one snapshot per ECS cluster, replacing
// whatever that cluster last delivered.
func (d *Domain) Observe(snap *kecs.Snapshot) error {
	if snap == nil {
		return nil
	}
	if snap.Cluster == "" {
		return fmt.Errorf("domain/ecs: snapshot has no cluster")
	}
	cp := *snap
	d.mu.Lock()
	if d.snaps == nil {
		d.snaps = map[string]*kecs.Snapshot{}
	}
	d.snaps[cp.Cluster] = &cp
	d.mu.Unlock()
	return nil
}

func joinReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case a == b:
		return a
	}
	return a + "; " + b
}

// clusters returns the learned cluster names in sorted order.
func (d *Domain) clusters() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.snaps))
	for c := range d.snaps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (d *Domain) snapshot(cluster string) *kecs.Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.snaps[cluster]
}

// Reports runs the sizer over every learned cluster, in sorted cluster order.
// The per-cluster report is pkg/ecs's own shape and carries the two totals a
// UI must keep apart: ClaimableMonthlyUSD and AdvisoryMonthlyUSD.
func (d *Domain) Reports(now time.Time, ledger domain.Netter) []*kecs.Report {
	clusters := d.clusters()
	out := make([]*kecs.Report, 0, len(clusters))
	for _, c := range clusters {
		snap := d.snapshot(c)
		if snap == nil {
			continue
		}
		if rep := d.sizer.Report(snap, now, ledger); rep != nil {
			out = append(out, rep)
		}
	}
	return out
}

// Recommend projects every cluster's report into the generic shape.
//
// Suppressed proposals and advisories are included, because pkg/ecs includes
// them and both must stay visible: an advisory is [domain.ActionAdvisory] and
// claims exactly $0, so it can never become a step or a saving.
func (d *Domain) Recommend(now time.Time, ledger domain.Netter) []domain.Recommendation {
	var out []domain.Recommendation
	for _, rep := range d.Reports(now, ledger) {
		out = append(out, rep.Recommendations()...)
	}
	for i := range out {
		out[i].Target.Domain = Kind
	}
	domain.SortRecommendations(out)
	return out
}

// Refusals implements [domain.Refuser]: every service the sizer looked at and
// declined to resize, with the code that stopped it.
//
// This is most of the output on a healthy fleet, and pkg/ecs's own FINDINGS
// says so: "Refusals are first-class UI content, not an error state." A
// service with no proposal produces no recommendation, so without this the
// only honest answer — "we assessed 40 services and here is why we changed
// none of them" — would be invisible.
func (d *Domain) Refusals(now time.Time, ledger domain.Netter) []domain.Refusal {
	var out []domain.Refusal
	for _, rep := range d.Reports(now, ledger) {
		for _, a := range rep.Assessments {
			if a.Proposal != nil {
				continue
			}
			code, reason, validFrom := "unstated",
				"the sizer produced neither a proposal nor a reason", time.Time{}
			if len(a.Suppressions) > 0 {
				s := a.Suppressions[0]
				code, reason, validFrom = s.Code, s.Reason, s.ValidFrom
			}
			ref := a.Ref
			ref.Domain = Kind
			out = append(out, domain.Refusal{
				Target: ref, Code: code, Reason: reason, ValidFrom: validFrom,
			})
		}
	}
	domain.SortRefusals(out)
	return out
}

// PlanSteps hands the applicable recommendations to pkg/ecs's planner, which
// enforces the three rules with teeth: no advisory becomes a step, no
// suppressed recommendation becomes a step, and every ECS task-size change is
// [domain.ActionRolling] because an ECS revision means a new deployment.
//
// It refuses outright when no actuator is wired. [domain.Registry] refuses
// first, on Health; this refuses again, so a caller holding the domain
// directly hits the same wall.
func (d *Domain) PlanSteps(recs []domain.Recommendation, g domain.Guard) ([]domain.Step, error) {
	if !d.actuatable {
		return nil, fmt.Errorf("%w: %s: no actuator is wired", domain.ErrReportOnly, Kind)
	}
	if h := d.Health(g.Now); h.ReportOnly {
		return nil, fmt.Errorf("%w: %s: %s", domain.ErrReportOnly, Kind, h.Reason)
	}
	return kecs.PlanSteps(recs, g)
}

// Health reports what the domain can currently do.
func (d *Domain) Health(now time.Time) domain.Health {
	h := domain.Health{Kind: Kind, ReportOnly: true}
	clusters := d.clusters()
	if len(clusters) == 0 {
		h.Reason = "no snapshot has been learned: report-only until a collector delivers one"
		return h
	}
	var newest time.Time
	var stale []string
	for _, c := range clusters {
		snap := d.snapshot(c)
		if snap == nil {
			continue
		}
		h.Targets += len(snap.Services)
		if snap.Timestamp.After(newest) {
			newest = snap.Timestamp
		}
		if snap.Stale {
			stale = append(stale, c+": "+snap.StaleReason)
		}
	}
	h.LastSnapshot = newest
	age := now.Sub(newest)
	if !now.IsZero() && age > d.staleAfter {
		h.Reason = fmt.Sprintf("newest snapshot is %s old (limit %s)",
			age.Round(time.Second), d.staleAfter)
		return h
	}
	h.Ready = true
	h.ReportOnly = !d.actuatable
	var reasons []string
	if !d.actuatable {
		reasons = append(reasons, "no actuator is wired")
	}
	reasons = append(reasons, stale...)
	h.Reason = strings.Join(reasons, "; ")
	return h
}

// ecsCheckpoint is the persisted form: snapshots sorted by cluster, so the
// bytes are stable regardless of the order clusters were collected in.
type ecsCheckpoint struct {
	Version   int              `json:"version"`
	Snapshots []*kecs.Snapshot `json:"snapshots,omitempty"`
}

// Checkpoint persists every learned cluster snapshot.
func (d *Domain) Checkpoint() ([]byte, error) {
	cp := ecsCheckpoint{Version: 1}
	for _, c := range d.clusters() {
		if snap := d.snapshot(c); snap != nil {
			cp.Snapshots = append(cp.Snapshots, snap)
		}
	}
	return json.Marshal(cp)
}

// Restore reloads a checkpoint.
func (d *Domain) Restore(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var cp ecsCheckpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return fmt.Errorf("domain/ecs: restore: %w", err)
	}
	next := make(map[string]*kecs.Snapshot, len(cp.Snapshots))
	for _, s := range cp.Snapshots {
		if s == nil || s.Cluster == "" {
			continue
		}
		next[s.Cluster] = s
	}
	d.mu.Lock()
	d.snaps = next
	d.mu.Unlock()
	return nil
}
