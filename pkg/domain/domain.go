// Package domain generalizes Kilter's observe → learn → decide → act loop over
// compute domains: EKS on EC2 nodes (today's pipeline), EKS on Fargate, plain
// EC2, and whatever comes next.
//
// A domain is an organ, not a heart. Three rules hold, and each is pinned by a
// test rather than by convention:
//
//  1. The core runs with zero domains registered. A [Registry] with nothing in
//     it learns nothing, recommends nothing, plans nothing, and never fails —
//     bit-for-bit today's behaviour.
//  2. Every domain degrades to report-only when its collector or its
//     credentials are absent. Report-only is enforced *by the core*
//     ([Registry.PlanSteps] refuses), not by trusting each domain to police
//     itself, so a buggy domain cannot talk the executor into acting on data
//     it does not have.
//  3. Nothing here does I/O or reads the clock. Learn and Recommend are pure
//     over data a collector already delivered; callers pass `now`. Only
//     [Collector] and [Actuator] implementations may hold SDK clients.
//
// The types are §5.2 of docs/design/compute-domains.md. Two fields are added
// to Recommendation beyond that sketch — Suppressed and SuppressCode — because
// §5.7 requires a commitment-blocked recommendation to stay *visible* with its
// reason instead of vanishing; see [Recommendation].
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/model"
)

// Kind names a compute domain.
type Kind string

const (
	K8sNodes   Kind = "k8s-nodes"   // existing pipeline, adapted
	K8sFargate Kind = "k8s-fargate" // EKS on Fargate
	EC2        Kind = "ec2"         // plain EC2, non-Kubernetes
	ECSFargate Kind = "ecs-fargate"
	Lambda     Kind = "lambda"
	// RDS is managed relational database: a billable domain of its own, not a
	// flavour of EC2. pkg/rds declares `const Kind = domain.Kind("rds")` and
	// could not be registered until this line existed; pkg/rds/FINDINGS.md §6.1
	// specifies the change and pkg/rds's TestKindIsHonestAboutRegistration
	// asserts the property that holds either side of it.
	RDS Kind = "rds"
)

// kinds is the closed set of known domains, in canonical (sorted) order.
var kinds = []Kind{EC2, ECSFargate, K8sFargate, K8sNodes, Lambda, RDS}

// Kinds returns a copy of the known domain kinds in canonical order.
func Kinds() []Kind {
	out := make([]Kind, len(kinds))
	copy(out, kinds)
	return out
}

// Valid reports whether k is a known domain kind.
func (k Kind) Valid() bool {
	for _, c := range kinds {
		if c == k {
			return true
		}
	}
	return false
}

// TargetRef identifies one billable, independently resizable unit: a workload
// on Fargate, an instance, a volume, a function.
type TargetRef struct {
	Domain Kind   `json:"domain"`
	Scope  string `json:"scope"`          // clusterID | accountID/region
	ID     string `json:"id"`             // workload key | instance ID | volume ID | ARN
	Name   string `json:"name,omitempty"` // human label; never an identity component
}

// String renders the reference as a stable path. Name is deliberately excluded:
// two refs that differ only in their display name are the same target.
func (t TargetRef) String() string { return string(t.Domain) + "/" + t.Scope + "/" + t.ID }

// Compare orders references totally and deterministically.
func (t TargetRef) Compare(o TargetRef) int {
	if c := strings.Compare(string(t.Domain), string(o.Domain)); c != 0 {
		return c
	}
	if c := strings.Compare(t.Scope, o.Scope); c != 0 {
		return c
	}
	if c := strings.Compare(t.ID, o.ID); c != 0 {
		return c
	}
	return strings.Compare(t.Name, o.Name)
}

// Spec is a resource specification in a domain's own vocabulary, with the
// canonical dimensions filled when they apply. Attrs carry domain axes
// (instanceType, volumeType, iops, tier, per-container breakdowns…).
//
// Attrs is a map, so nothing may range over it on an output path: use
// [Spec.AttrKeys], which sorts.
type Spec struct {
	Resources model.Resources   `json:"resources,omitzero"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

// Attr reads one attribute; a missing attribute reads as "".
func (s Spec) Attr(k string) string { return s.Attrs[k] }

// AttrKeys returns the attribute keys in sorted order. Every path that renders,
// hashes or compares a Spec goes through this, so no output can depend on Go's
// randomized map order.
func (s Spec) AttrKeys() []string {
	if len(s.Attrs) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Attrs))
	for k := range s.Attrs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WithAttr returns a copy of s with one attribute set. The Attrs map is copied,
// never shared: a Spec recorded in a Step must not mutate when its source does.
func (s Spec) WithAttr(k, v string) Spec {
	out := Spec{Resources: s.Resources, Attrs: make(map[string]string, len(s.Attrs)+1)}
	for ak, av := range s.Attrs {
		out.Attrs[ak] = av
	}
	out.Attrs[k] = v
	return out
}

// Canonical renders the spec as a deterministic string, used for hashing and
// for human-readable step detail.
func (s Spec) Canonical() string {
	var b strings.Builder
	b.WriteString(s.Resources.String())
	for _, k := range s.AttrKeys() {
		b.WriteString(";")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(s.Attrs[k])
	}
	return b.String()
}

// Equal compares two specs by value, independent of map iteration order.
func (s Spec) Equal(o Spec) bool {
	if s.Resources != o.Resources || len(s.Attrs) != len(o.Attrs) {
		return false
	}
	for k, v := range s.Attrs {
		if ov, ok := o.Attrs[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

// ActionClass tells the executor — and the human approving the plan — what
// applying a recommendation costs.
type ActionClass string

const (
	// ActionInPlace is online with no disruption: EBS ModifyVolume, Kubernetes
	// in-place pod resize.
	ActionInPlace ActionClass = "in-place"
	// ActionRolling recreates the workload behind its controller: a Fargate
	// resize, an ECS deployment, an ASG instance refresh.
	ActionRolling ActionClass = "rolling"
	// ActionStopStart requires downtime: a plain-EC2 instance resize.
	ActionStopStart ActionClass = "stop-start"
	// ActionAdvisory is never auto-applied: Graviton moves, Lambda tuning,
	// cross-domain migrations.
	ActionAdvisory ActionClass = "advisory"
)

var actionClasses = []ActionClass{ActionAdvisory, ActionInPlace, ActionRolling, ActionStopStart}

// Valid reports whether a is a known action class.
func (a ActionClass) Valid() bool {
	for _, c := range actionClasses {
		if c == a {
			return true
		}
	}
	return false
}

// Disruptive reports whether applying the action restarts or stops the target.
// The executor's disruption accounting (eviction budgets, PDB reservations)
// keys off this, which is why a domain must never understate it — see
// [ActionRolling] and the Fargate rule in pkg/domain/fargate.
func (a ActionClass) Disruptive() bool { return a == ActionRolling || a == ActionStopStart }

// Suppression codes. A suppressed recommendation is reported, never applied.
// They are stable strings meant to be stored and matched on, unlike
// [Recommendation.Reason], which is prose.
const (
	// SuppressCommitmentNegative: applying the change would raise the bill
	// because committed capacity would be stranded (§4.4 ex.1).
	SuppressCommitmentNegative = "commitment-negative"
	// SuppressCommitmentNeutral: the list-price saving is entirely absorbed by
	// a commitment, so the change buys risk for nothing (§4.4 ex.2-3).
	SuppressCommitmentNeutral = "commitment-neutral"
	// SuppressModeRecommend: the target carries kilter.dev/mode=recommend.
	SuppressModeRecommend = "mode-recommend"
	// SuppressQuarantined: the target regressed after a recent change.
	SuppressQuarantined = "quarantined"
)

// Recommendation is the domain-generic sizing decision. The Kubernetes
// container recommendation (recommend.Recommendation) is projected into this
// shape at the domain boundary.
//
// Savings discipline: GrossSavingsMonthlyUSD is the on-demand delta — the
// number a naive optimizer reports. NetSavingsMonthlyUSD is the bill delta
// after the commitment waterfall, and it is what a plan or a ledger entry may
// claim. Net ≤ Gross always; the invariant is enforced in [Recommendation.SetSavings],
// which is the only supported way to populate the two fields.
type Recommendation struct {
	Target   TargetRef `json:"target"`
	Current  Spec      `json:"current"`
	Proposed Spec      `json:"proposed"`

	CurrentHourlyUSD  float64 `json:"currentHourlyUSD"`
	ProposedHourlyUSD float64 `json:"proposedHourlyUSD"`
	// GrossSavingsMonthlyUSD is the on-demand delta; negative means the change
	// costs more (a safety-driven growth, for instance).
	GrossSavingsMonthlyUSD float64 `json:"grossSavingsMonthlyUSD"`
	// NetSavingsMonthlyUSD is the delta after the commitment waterfall (§4.4).
	// Never greater than gross.
	NetSavingsMonthlyUSD float64 `json:"netSavingsMonthlyUSD"`

	Action     ActionClass `json:"action"`
	Risk       string      `json:"risk"`       // plan.RiskLow | RiskMedium | RiskHigh
	Confidence float64     `json:"confidence"` // 0..1, same semantics as recommend
	Evidence   []Evidence  `json:"evidence"`
	Reason     string      `json:"reason"`

	// Suppressed marks a recommendation that must never be applied. It stays
	// visible — with SuppressCode and Reason — because a silently dropped
	// recommendation is indistinguishable from a bug, and §5.7 requires the
	// commitment-blocked case to be explainable to a human. [Registry.PlanSteps]
	// and every domain's PlanSteps skip suppressed recommendations.
	Suppressed   bool   `json:"suppressed,omitempty"`
	SuppressCode string `json:"suppressCode,omitempty"`

	// ValidFrom defers a recommendation blocked by a commitment term (§4.4
	// ex.1): the date the blocking commitment expires and the arithmetic
	// changes.
	ValidFrom time.Time `json:"validFrom,omitzero"`
}

// finite maps NaN and ±Inf to 0 and leaves every other value alone. Garbage
// arithmetic must not be able to travel into a savings claim.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// SetSavings records the gross (on-demand) and net (post-commitment) monthly
// deltas, enforcing the one invariant this package exists to protect:
//
//	Net ≤ Gross, always.
//
// Commitments can only ever make a change worth *less* than its list price
// suggests: the committed spend bills whether usage absorbs it or not. A net
// above gross is therefore arithmetically impossible, and any caller producing
// one has a bug — so it is clamped here rather than trusted. Non-finite inputs
// become 0 for the same reason.
func (r *Recommendation) SetSavings(grossMonthly, netMonthly float64) {
	gross := finite(grossMonthly)
	net := finite(netMonthly)
	if net > gross {
		net = gross
	}
	r.GrossSavingsMonthlyUSD = gross
	r.NetSavingsMonthlyUSD = net
}

// ClaimableMonthlyUSD is the only number a plan, an API response or a ledger
// entry may present as a saving: the net, and only when the recommendation is
// actually applicable.
func (r Recommendation) ClaimableMonthlyUSD() float64 {
	if r.Suppressed || r.NetSavingsMonthlyUSD <= 0 {
		return 0
	}
	return r.NetSavingsMonthlyUSD
}

// Validate reports the first structural problem with a recommendation. It is
// the contract every domain's output must satisfy; the domains assert it on
// themselves and the tests assert it on them.
func (r Recommendation) Validate() error {
	switch {
	case r.Target.Domain == "":
		return fmt.Errorf("domain: recommendation has no target domain")
	case r.Target.ID == "":
		return fmt.Errorf("domain: recommendation %s has no target ID", r.Target)
	case !r.Action.Valid():
		return fmt.Errorf("domain: recommendation %s has invalid action %q", r.Target, r.Action)
	case len(r.Evidence) == 0:
		// "Every recommendation states its evidence; a rec with none is a bug."
		return fmt.Errorf("domain: recommendation %s has no evidence", r.Target)
	case r.Confidence < 0 || r.Confidence > 1 || math.IsNaN(r.Confidence):
		return fmt.Errorf("domain: recommendation %s has out-of-range confidence %v", r.Target, r.Confidence)
	case r.NetSavingsMonthlyUSD > r.GrossSavingsMonthlyUSD:
		return fmt.Errorf("domain: recommendation %s claims net $%v > gross $%v",
			r.Target, r.NetSavingsMonthlyUSD, r.GrossSavingsMonthlyUSD)
	case r.Suppressed && r.SuppressCode == "":
		return fmt.Errorf("domain: recommendation %s is suppressed without a code", r.Target)
	case r.Current.Equal(r.Proposed):
		return fmt.Errorf("domain: recommendation %s proposes no change", r.Target)
	}
	return nil
}

// Evidence is one observable fact backing a recommendation — a metric window, a
// percentile, a source system.
type Evidence struct {
	Metric  string    `json:"metric"` // cpu-p95 | mem-peak | capacity-provisioned | …
	Value   string    `json:"value"`
	Window  string    `json:"window,omitempty"`
	Samples int       `json:"samples,omitempty"`
	Source  string    `json:"source"` // metrics.k8s.io | cloudwatch | annotation | …
	At      time.Time `json:"at,omitzero"`
}

// Evidence sources.
const (
	SourceMetricsAPI  = "metrics.k8s.io"
	SourceAnnotation  = "annotation"
	SourceRecommender = "recommend"
	SourceQuantizer   = "pricing.Quantize"
)

// SortRecommendations orders recommendations totally and deterministically, so
// two runs over the same data — in any input order — render identically.
func SortRecommendations(recs []Recommendation) {
	sort.SliceStable(recs, func(i, j int) bool {
		a, b := recs[i], recs[j]
		if c := a.Target.Compare(b.Target); c != 0 {
			return c < 0
		}
		if a.Action != b.Action {
			return a.Action < b.Action
		}
		if a.Proposed.Canonical() != b.Proposed.Canonical() {
			return a.Proposed.Canonical() < b.Proposed.Canonical()
		}
		return a.Reason < b.Reason
	})
}

// Step is the domain-neutral executable unit (§5.6). Non-K8s domains reuse the
// existing trust machinery — fingerprints, approval, ledger — through it.
type Step struct {
	Seq    int         `json:"seq"`
	Key    string      `json:"key"` // idempotency key: hash(domain, target, from, to)
	Target TargetRef   `json:"target"`
	Action ActionClass `json:"action"`
	From   Spec        `json:"from"` // recorded for Revert and the ledger
	To     Spec        `json:"to"`
	Risk   string      `json:"risk"`
	Detail string      `json:"detail"`
}

// StepKey is the idempotency key for a step: a content hash over the domain,
// the target and both specs. Re-executing a completed step is a no-op, so the
// key must not depend on anything that varies between runs — hence the sorted
// attribute rendering in [Spec.Canonical] and the omission of Seq.
func StepKey(t TargetRef, from, to Spec) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s",
		t.Domain, t.Scope, t.ID, from.Canonical(), to.Canonical())
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Fingerprint is the deterministic content hash of an ordered step list, the
// same approach `kilter approve` already uses for node plans.
func Fingerprint(steps []Step) string {
	h := sha256.New()
	for _, s := range steps {
		fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\n", s.Seq, s.Key, s.Action, s.Risk)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
