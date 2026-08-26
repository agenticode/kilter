package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing/commit"
)

// Errors the seam defines. Callers distinguish them with errors.Is.
var (
	// ErrReportOnly: the domain has no live collector, no credentials, or is
	// configured advisory-only. It still recommends; it cannot plan steps.
	ErrReportOnly = errors.New("domain: report-only (no collector or actuation unavailable)")
	// ErrNotRegistered: a snapshot arrived for a domain nobody registered.
	// Reported rather than dropped — silently discarding collected data is
	// indistinguishable from a broken collector.
	ErrNotRegistered = errors.New("domain: not registered")
	// ErrWrongDomain: a snapshot was handed to the wrong domain.
	ErrWrongDomain = errors.New("domain: snapshot belongs to another domain")
	// ErrIrreversible: the step's action class cannot be undone.
	ErrIrreversible = errors.New("domain: step is irreversible")
	// ErrFrozen, ErrBreakerOpen, ErrOutsideWindow: guardrails refused.
	ErrFrozen        = errors.New("domain: freeze is active")
	ErrBreakerOpen   = errors.New("domain: circuit breaker is open")
	ErrOutsideWindow = errors.New("domain: outside the change window")
)

// Metric names carried by [Sample] and echoed in [Evidence].
const (
	MetricCPUMillicores = "cpu-mcores"
	MetricMemoryBytes   = "mem-bytes"
)

// Snapshot is the domain-generic unit a collector ships to the brain — the
// analogue of model.ClusterSnapshot for non-Kubernetes domains.
//
// Kubernetes-backed domains (k8s-nodes, k8s-fargate) carry the cluster
// snapshot in Cluster and leave Targets/Samples empty: their collector is the
// existing Kilter agent, and re-projecting a cluster into generic targets
// before the brain re-derives it would be a lossy round trip for no gain.
type Snapshot struct {
	Domain    Kind      `json:"domain"`
	Scope     string    `json:"scope"`
	Timestamp time.Time `json:"timestamp"`
	Targets   []Target  `json:"targets,omitempty"`
	Samples   []Sample  `json:"samples,omitempty"`

	// Cluster is the Kubernetes snapshot for Kubernetes-backed domains.
	Cluster *model.ClusterSnapshot `json:"cluster,omitempty"`

	// Payload is a domain-native snapshot the generic Targets/Samples shape
	// cannot carry without loss. It is OPAQUE to the core: only the domain
	// named by Domain may decode it, and nothing here inspects it.
	//
	// It exists because pkg/lambda/FINDINGS.md §8 proved the generic shape
	// insufficient: a Lambda REPORT record is four numbers whose CORRELATION
	// is the entire point — the memory setting an invocation ran at and the
	// duration it took THERE — and splitting one record into three
	// [Sample]s discards exactly that. Every cost claim in that domain rests
	// on the correlation, so a domain forced through the lossy path can only
	// refuse. pkg/ec2 and pkg/ecs have the same problem for a different
	// reason: their evidence is per-target series with per-series status
	// (truncated vs. empty), and "absence of an answer" must stay
	// distinguishable from "absence of data".
	//
	// Payload does not weaken the seam: a domain still cannot see another
	// domain's payload, [Registry.Learn] still routes by Kind, and a domain
	// that cannot decode what it was handed must degrade to report-only
	// rather than fail the brain.
	Payload json.RawMessage `json:"payload,omitempty"`

	// Commitments piggyback on EC2-domain snapshots; the brain owns one
	// account-wide ledger regardless of which domain delivered it.
	Commitments *commit.Inventory `json:"commitments,omitempty"`

	// Stale marks a partial collection — a region poll that timed out, an API
	// that throttled. The brain still learns from what arrived, and the domain
	// says so in its Health. A failed collection must degrade the domain, never
	// break the brain.
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"staleReason,omitempty"`
}

// Target is one billable unit as the collector found it.
type Target struct {
	Ref    TargetRef         `json:"ref"`
	Spec   Spec              `json:"spec"`
	Labels map[string]string `json:"labels,omitempty"` // tags; guardrail source
	Blind  []string          `json:"blind,omitempty"`  // declared blind spots, e.g. "memory"
}

// Sample is one metric observation for a target.
type Sample struct {
	Ref           TargetRef `json:"ref"`
	Metric        string    `json:"metric"`
	Value         float64   `json:"value"`
	Timestamp     time.Time `json:"timestamp"`
	WindowSeconds int32     `json:"windowSeconds"` // 300 for CloudWatch basic
}

// Health is a domain's own account of what it can currently do. A domain that
// cannot see its targets must say so; the core then refuses to plan steps for
// it. There is no third state where a domain quietly acts on stale data.
type Health struct {
	Kind Kind `json:"kind"`
	// Ready is true when the domain has usable, fresh learned state.
	Ready bool `json:"ready"`
	// ReportOnly is true when the domain may recommend but must not act.
	ReportOnly bool `json:"reportOnly"`
	// Reason explains !Ready or ReportOnly in prose. Empty iff both are clean.
	Reason string `json:"reason,omitempty"`
	// LastSnapshot is the timestamp of the newest snapshot learned from.
	LastSnapshot time.Time `json:"lastSnapshot,omitzero"`
	// Targets is how many billable units the domain currently tracks.
	Targets int `json:"targets"`
}

// Collector runs agent-side. Implementations own SDK clients and budgets; every
// call is bounded by ctx. A collector failure yields a stale-marked snapshot,
// never a broken brain.
type Collector interface {
	Domain() Kind
	Collect(ctx context.Context) (*Snapshot, error)
}

// Domain runs brain-side and is PURE: Learn, Recommend, PlanSteps and Health
// perform no I/O and read no clock.
type Domain interface {
	Kind() Kind

	// Learn folds a snapshot into learned state (histograms, floors, classes).
	// A nil snapshot is a no-op. A snapshot missing its payload degrades the
	// domain to report-only rather than returning an error: an unreachable
	// collector is an operational condition, not a programming error.
	Learn(snap *Snapshot) error

	// Recommend derives current recommendations. ledger nets commitments and
	// may be nil, in which case net savings equal gross (no known commitment
	// can strand anything).
	Recommend(now time.Time, ledger Netter) []Recommendation

	// PlanSteps orders applicable recommendations into executable steps under
	// guardrails. It must return ErrReportOnly when the domain cannot act, and
	// must skip suppressed recommendations.
	PlanSteps(recs []Recommendation, g Guard) ([]Step, error)

	// Health reports readiness as of now.
	Health(now time.Time) Health

	// Checkpoint/Restore integrate with pkg/store the way the recommender's
	// state already does. Checkpoint output must be deterministic.
	Checkpoint() ([]byte, error)
	Restore([]byte) error
}

// Actuator runs controller-side; one per domain that supports execution.
// Execute must be idempotent per Step.Key: re-running a completed step is a
// no-op, matching the resumable-plan contract.
type Actuator interface {
	Domain() Kind
	Execute(ctx context.Context, step Step) error
	// Revert undoes a step from its recorded From spec where the action class
	// permits; irreversible steps return ErrIrreversible (honest undo).
	Revert(ctx context.Context, step Step) error
}

// Guard is the guardrail context a plan is built under. It is the domain-neutral
// half of pkg/guard: change windows, freeze and the circuit breaker are already
// domain-agnostic (time plus global switches), so they are passed here rather
// than re-derived per domain.
//
// Per-target mode (kilter.dev/mode) is NOT here: it is a property of the target,
// so each domain resolves it during Recommend and marks the affected
// recommendations suppressed. That keeps a mode=recommend workload visible in
// the report instead of vanishing between Recommend and PlanSteps.
type Guard struct {
	// Now is the decision time. Callers pass it; nothing here calls time.Now.
	Now time.Time
	// Windows, when non-empty, restrict disruptive steps to a change window.
	Windows []guard.Window
	// Freeze halts all actuation.
	Freeze bool
	// BreakerOpen reports the cluster-health circuit breaker.
	BreakerOpen bool
	// MaxSteps caps the plan; 0 means unlimited.
	MaxSteps int
}

// Allow reports whether actuation is permitted at all, most-severe reason first.
func (g Guard) Allow() error {
	switch {
	case g.Freeze:
		return ErrFrozen
	case g.BreakerOpen:
		return ErrBreakerOpen
	case len(g.Windows) > 0 && !guard.InWindow(g.Windows, g.Now):
		return fmt.Errorf("%w at %s", ErrOutsideWindow, g.Now.UTC().Format(time.RFC3339))
	}
	return nil
}
