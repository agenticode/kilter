// Package crossover answers the highest-value question an EKS user can ask
// Kilter: should this pod set run on Fargate, or on EC2 nodes?
//
// It computes two bills for the same pod set and compares them.
//
//	F(P) = Σ_p rates.Cost(Quantize(p))   — Fargate, per pod, quantized (§4.1)
//	E(P) = price of binpack.PlanNodes(P) — the cheapest feasible node set
//
// Neither is a formula. F runs every pod through the U1 quantizer, so it
// carries the +256 MiB overhead and the tier cliffs that make naive Fargate
// estimates fiction. E runs the real bin-packing simulator, so it carries the
// three overheads that make an EC2 estimate honest: a DaemonSet copy on every
// node, the kubelet/system reservation carved out of every node's capacity,
// and the fact that a node one pod short of full is billed whole.
//
// # Advisory only
//
// This package produces a [Report]. It has no Step type, no actuator, and no
// import of pkg/plan, pkg/actuate, pkg/safety or pkg/provider — asserted by
// TestPackageIsPureAndAdvisory, which parses this directory's imports. Domain
// migration is a human decision; Kilter supplies the arithmetic and the
// blockers, never a button.
//
// # The non-price gates are hard blocks
//
// Several properties make Fargate impossible at any price (§4.3). They are
// gates, not penalties: a blocked pod set is reported as blocked, with the
// reason, and the verdict is never [VerdictFargate]. See gates.go.
//
// # Money convention
//
// Hourly USD in float64, monthly via [pricing.HoursPerMonth] (730) — the same
// convention as pkg/pricing and pkg/pricing/commit. Money is never float32 and
// never compared with ==; equality goes through [moneyEqual], whose tolerance
// matches commit.Eps.
//
// # Determinism
//
// Analyze is a pure function of (now, pod set, options). It takes `now` from
// the caller, holds no package-level state, and never iterates a map to
// produce output: every list in a [Report] is sorted by an intrinsic key, so
// shuffling the input pods cannot change a single byte of the result. Pinned
// by TestReportIsShuffleInvariant.
package crossover

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/binpack"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

// moneyEps is the tolerance for money equality. It matches
// pkg/pricing/commit.Eps: small enough to separate real hourly rates (Fargate
// bills to ~1e-8 USD/GB-s), large enough to absorb float64 summation error.
const moneyEps = 1e-9

// CloseMarginFraction is the relative gap below which the two bills are too
// close for this model to call. It does not change the verdict — the cheaper
// side still wins — it sets [Report.Close], so a reader knows a 1 % difference
// is inside the error bars of an embedded price table.
const CloseMarginFraction = 0.05

// DefaultProvider is the cloud whose instance types the EC2 side is priced
// from when Options names neither candidates nor a provider. EKS Fargate is an
// AWS product, so comparing it against a GCP node set would be nonsense.
const DefaultProvider = "aws"

// Pod is one pod under comparison: its spec, and the non-price facts §4.3
// gates on. A zero Facts blocks — see gates.go rule 2.
type Pod struct {
	Spec  model.PodSpec `json:"spec"`
	Facts Facts         `json:"facts"`
}

// PodSet is the workload being compared, plus the cluster context E(P) needs.
type PodSet struct {
	// Pods is the workload under comparison. DaemonSet pods do not belong
	// here: they are per-node overhead, not workload.
	Pods []Pod `json:"pods"`
	// DaemonSets is one template per DaemonSet that would run on the planned
	// nodes. Each is replicated onto every planned node that it fits, which is
	// what makes E(P) honest — and it also raises GateDaemonSet, because a
	// DaemonSet serving this workload cannot follow it to Fargate.
	DaemonSets []model.PodSpec `json:"daemonSets,omitempty"`
}

// Options tunes the EC2 side. The Fargate side has nothing to tune: EKS
// Fargate is x86 Linux on-demand and nothing else (pricing.Platform has
// exactly one value), which is why there is no Spot or Arm option here that
// could be mistaken for one.
type Options struct {
	// Catalog supplies instance types and Fargate rates. Nil uses the
	// embedded baseline catalog.
	Catalog *pricing.Catalog
	// Candidates overrides the instance types the EC2 side may buy. Nil
	// derives them from Catalog, Provider and Arch.
	Candidates []pricing.InstanceType
	// Provider filters catalog candidates; "" means DefaultProvider.
	Provider string
	// Arch filters catalog candidates; "" allows every architecture,
	// including Graviton — which the EC2 side may use and EKS Fargate cannot.
	Arch string
	// Spot prices the EC2 side at spot rates where the catalog knows one.
	// There is deliberately no Fargate equivalent: EKS has no Fargate Spot.
	Spot bool
	// AllowBurstable admits credit-based shapes (t3, B-series) to the EC2
	// side. Off by default, matching pkg/binpack: their sticker price is only
	// real if you do not use the CPU.
	AllowBurstable bool
	// SystemReservedFraction is the kubelet/system reservation carved out of
	// every planned node's capacity. Zero uses pkg/binpack's default (0.08).
	SystemReservedFraction float64
	// MaxPodsPerNode caps pods per planned node. Zero uses the kubelet
	// default (110).
	MaxPodsPerNode int
	// MaxNodes caps the plan size. Zero uses pkg/binpack's default.
	MaxNodes int
	// NodeLabels are extra labels every planned node carries.
	NodeLabels map[string]string
}

// Verdict is the report's answer. It is deliberately not a boolean: "Fargate
// is impossible" and "EC2 is cheaper" are different answers with different
// remedies, and collapsing them loses the reason.
type Verdict string

const (
	// VerdictFargate: no gate blocks, and Fargate bills less.
	VerdictFargate Verdict = "fargate"
	// VerdictEC2: no gate blocks, and the node set bills less.
	VerdictEC2 Verdict = "ec2"
	// VerdictTie: no gate blocks and the two bills are equal.
	VerdictTie Verdict = "tie"
	// VerdictFargateBlocked: at least one §4.3 gate blocks. The pod set stays
	// on EC2 because Fargate cannot run it, not because of a price.
	VerdictFargateBlocked Verdict = "fargate-blocked"
	// VerdictUndecided: neither side can be priced or neither can run the pod
	// set. Nothing is recommended and nothing is claimed.
	VerdictUndecided Verdict = "undecided"
)

// FargateSide is the Fargate bill for the pod set.
type FargateSide struct {
	// Eligible is false when any gate blocks, or when any pod exceeds the
	// Fargate ceiling. When it is false, HourlyUSD is arithmetic, not advice:
	// no verdict may be derived from it.
	Eligible   bool    `json:"eligible"`
	Pods       int     `json:"pods"`
	HourlyUSD  float64 `json:"hourlyUSD"`
	MonthlyUSD float64 `json:"monthlyUSD"`
	// Configs is the billed-tier histogram, in canonical tier order.
	Configs []ConfigCount `json:"configs,omitempty"`
	// Unpriced lists pods with no Fargate configuration at all (above the
	// 16 vCPU / 120 GB ceiling), sorted. They also raise GateSizeCeiling.
	Unpriced []string `json:"unpriced,omitempty"`
}

// ConfigCount is one billed Fargate tier and how many pods land on it.
type ConfigCount struct {
	Config     pricing.FargateConfig `json:"config"`
	Pods       int                   `json:"pods"`
	HourlyUSD  float64               `json:"hourlyUSD"`
	MonthlyUSD float64               `json:"monthlyUSD"`
}

// EC2Side is the node-set bill for the pod set.
type EC2Side struct {
	// Feasible is false when the packer could not place every pod. HourlyUSD
	// then prices a partial plan and must not be compared against Fargate.
	Feasible   bool    `json:"feasible"`
	Nodes      int     `json:"nodes"`
	HourlyUSD  float64 `json:"hourlyUSD"`
	MonthlyUSD float64 `json:"monthlyUSD"`
	// NodeTypes is the purchased shape histogram, sorted by instance name.
	NodeTypes []NodeTypeCount `json:"nodeTypes,omitempty"`
	// Purchased is the total capacity bought — what is billed, including the
	// part the kubelet reserves and the part nothing fits into.
	Purchased model.Resources `json:"purchased"`
	// Allocatable is Purchased minus the system reservation.
	Allocatable model.Resources `json:"allocatable"`
	// WorkloadRequests is what the workload pods actually asked for.
	// DaemonSet copies are not counted here — they are overhead, and the gap
	// between Allocatable and WorkloadRequests is where they live.
	WorkloadRequests model.Resources `json:"workloadRequests"`
	// DaemonSetTemplates is how many DaemonSets were replicated per node.
	DaemonSetTemplates int `json:"daemonSetTemplates,omitempty"`
	// Unschedulable lists pods that fit no candidate instance type, sorted.
	Unschedulable []string `json:"unschedulable,omitempty"`
}

// NodeTypeCount is one purchased instance type and how many were bought.
type NodeTypeCount struct {
	Provider   string  `json:"provider"`
	Name       string  `json:"name"`
	Arch       string  `json:"arch,omitempty"`
	Spot       bool    `json:"spot,omitempty"`
	Nodes      int     `json:"nodes"`
	HourlyUSD  float64 `json:"hourlyUSD"`
	MonthlyUSD float64 `json:"monthlyUSD"`
}

// Density is the break-even, expressed in the dimension §4.3 itself uses:
// effective node density u — what fraction of the capacity you buy the
// workload actually requests, after the system reservation, the DaemonSet
// copies and packing fragmentation have taken their share.
//
// Density is chosen over pod count and duty cycle because it is the only one
// of the three that (a) falls out of a single pack with no extra simulation,
// (b) needs no assumption the snapshot cannot support, and (c) decomposes into
// levers a human can actually pull: buy different shapes, drop a DaemonSet,
// right-size requests. See FINDINGS.md §3.
//
// The identity that makes it exact:
//
//	u  = value(WorkloadRequests) / value(Purchased)
//	u* = u · E/F                                    ⇒  u/u* = F/E
//
// so "you pack at u, break-even is u*" is the same statement as "E vs F",
// re-expressed on an axis with units a human recognizes. At u = 1 — a perfect
// pack with no reservation and no fragmentation — u* reduces exactly to §4.3's
// screening ratio P_ec2_bundle / P_fargate_bundle.
type Density struct {
	// Defined is false when there is nothing to divide (no nodes, no pods, or
	// an unpriceable side). Every other field is then zero and means nothing.
	Defined bool `json:"defined"`
	// Achieved is u: workload requests ÷ purchased capacity.
	Achieved float64 `json:"achieved"`
	// BreakEven is u*: the density at which the two bills tie. Below it,
	// Fargate is cheaper; above it, the node set is.
	BreakEven float64 `json:"breakEven"`
	// SystemReservedFraction is the share of purchased capacity the kubelet
	// reserve takes before any pod is scheduled.
	SystemReservedFraction float64 `json:"systemReservedFraction"`
	// UnusedAllocatableFraction is the share of purchased capacity that is
	// allocatable but not requested by workload pods: DaemonSet copies plus
	// packing fragmentation. Achieved + SystemReservedFraction +
	// UnusedAllocatableFraction = 1.
	UnusedAllocatableFraction float64 `json:"unusedAllocatableFraction"`
}

// Report is the crossover answer. It is advisory: there is no step in it, and
// nothing in this package can produce one.
type Report struct {
	// At is the caller-supplied evaluation time. This package never reads a
	// clock.
	At      time.Time   `json:"at"`
	Verdict Verdict     `json:"verdict"`
	Fargate FargateSide `json:"fargate"`
	EC2     EC2Side     `json:"ec2"`
	// Blocks lists every §4.3 gate that refused, in AllGates order. Non-empty
	// ⇒ Verdict is never VerdictFargate and MonthlySavingsUSD is 0.
	Blocks []Block `json:"blocks,omitempty"`
	// MonthlySavingsUSD is the monthly gap between the two bills, in favour of
	// the winning side. It is a comparison, not a claim about the current
	// invoice: this report does not know where the pods run today. Zero
	// whenever nothing is recommended.
	MonthlySavingsUSD float64 `json:"monthlySavingsUSD"`
	// SavingsFraction is MonthlySavingsUSD over the losing side's bill.
	SavingsFraction float64 `json:"savingsFraction"`
	// Close marks a gap below CloseMarginFraction: the cheaper side still
	// wins, but not by more than the price tables' own uncertainty.
	Close   bool    `json:"close,omitempty"`
	Density Density `json:"density"`
	// Assumptions are the modelling choices behind the two numbers, in fixed
	// order. They are part of the answer, not a footnote.
	Assumptions []string `json:"assumptions,omitempty"`
	// Warnings records inputs that did not add up. Nothing is silently
	// absorbed.
	Warnings []string `json:"warnings,omitempty"`
}

// FromSnapshot builds a pod set from a cluster snapshot.
//
// Fargate-hosted pods are separated with pricing.SplitFargate (§7 trap 7:
// their single-pod VMs must never reach node math) and carry
// [CompatibleFacts] — AWS already ran them there. Node-hosted pods carry
// [FactsFromPodSpec], which leaves five properties Unknown, so the EC2 → Fargate
// direction blocks until a collector fills them; that is the intended,
// conservative state and the report says exactly which observations are
// missing. DaemonSet pods become per-node overhead templates, one per
// DaemonSet, rather than workload.
//
// A nil snapshot yields an empty pod set.
func FromSnapshot(snap *model.ClusterSnapshot) PodSet {
	if snap == nil {
		return PodSet{}
	}
	nodeSide, fargatePods := pricing.SplitFargate(snap)
	out := PodSet{}
	seenDS := make(map[model.WorkloadRef]bool)
	for i := range nodeSide.Pods {
		p := nodeSide.Pods[i]
		if p.Workload.Kind == model.KindDaemonSet {
			if !seenDS[p.Workload] {
				seenDS[p.Workload] = true
				out.DaemonSets = append(out.DaemonSets, p)
			}
			continue
		}
		out.Pods = append(out.Pods, Pod{Spec: p, Facts: FactsFromPodSpec(&p)})
	}
	for _, fp := range fargatePods {
		p := fp.Pod
		out.Pods = append(out.Pods, Pod{Spec: p, Facts: CompatibleFacts()})
	}
	return out
}

// Analyze computes both bills, applies the §4.3 gates, and reports which side
// wins, by how much, and at what density the answer flips.
//
// It never fails: a pod set nothing can run, a catalog with no candidates and
// an empty input all produce a report that says so. now is the caller's clock.
func Analyze(now time.Time, ps PodSet, opts Options) Report {
	rep := Report{At: now, Verdict: VerdictUndecided}
	catalog := opts.Catalog
	if catalog == nil {
		catalog = pricing.Embedded()
	}
	pods, notes := canonicalize(ps.Pods)
	rep.Warnings = append(rep.Warnings, notes...)
	dsPods, dsNotes := canonicalDaemonSets(ps.DaemonSets)
	rep.Warnings = append(rep.Warnings, dsNotes...)

	// --- Fargate side: F(P) -------------------------------------------------
	rep.Fargate = fargateSide(pods, catalog)

	// --- Non-price gates ----------------------------------------------------
	rep.Blocks = evaluateGates(pods)
	if len(dsPods) > 0 {
		// A DaemonSet serving this workload cannot follow it to Fargate: there
		// is no node for it to run on. This is a property of the whole set, so
		// it is reported without a pod list.
		rep.Blocks = append(rep.Blocks, Block{
			Gate: GateDaemonSet, Kind: BlockViolation,
			Reason: fmt.Sprintf("%s: %d DaemonSet(s) run beside this workload on its nodes and cannot follow it to Fargate, which supports none",
				GateDaemonSet, len(dsPods)),
		})
	}
	if len(rep.Fargate.Unpriced) > 0 {
		rep.Blocks = append(rep.Blocks, Block{
			Gate: GateSizeCeiling, Kind: BlockViolation,
			Reason: string(GateSizeCeiling) + ": " + fargateCeilingReason,
			Pods:   capPods(append([]string(nil), rep.Fargate.Unpriced...)),
		})
	}
	rep.Blocks = sortBlocks(rep.Blocks)
	rep.Fargate.Eligible = len(rep.Blocks) == 0 && len(pods) > 0

	// --- EC2 side: E(P) -----------------------------------------------------
	rep.EC2 = ec2Side(pods, dsPods, catalog, opts, &rep.Warnings)

	// --- Verdict, savings, break-even --------------------------------------
	rep.Density = density(rep.EC2, rep.Fargate)
	decide(&rep, len(pods))
	rep.Assumptions = assumptions(opts, rep.EC2)
	return rep
}

// fargateCeilingReason explains GateSizeCeiling.
const fargateCeilingReason = "the pod exceeds the Fargate maximum configuration " +
	"(16 vCPU / 120 GB) once the 256 MiB Kubernetes overhead is added, so AWS has no tier to bill it at"

// fargateSide prices every pod through the U1 quantizer.
func fargateSide(pods []Pod, catalog *pricing.Catalog) FargateSide {
	rates := catalog.FargateRates()
	out := FargateSide{Pods: len(pods)}
	byConfig := make(map[pricing.FargateConfig]int)
	for i := range pods {
		cfg, _, err := pricing.FargatePodConfig(&pods[i].Spec)
		if err != nil {
			// No tier means no bill. Inventing one — clamping to the ceiling —
			// would price a pod that can never be scheduled.
			out.Unpriced = append(out.Unpriced, podKey(&pods[i].Spec))
			continue
		}
		byConfig[cfg]++
		out.HourlyUSD += rates.Cost(cfg)
	}
	sort.Strings(out.Unpriced)
	// Emit in canonical tier order (vCPU, then memory) — never map order.
	for _, cfg := range pricing.FargateConfigs() {
		n, ok := byConfig[cfg]
		if !ok {
			continue
		}
		h := rates.Cost(cfg) * float64(n)
		out.Configs = append(out.Configs, ConfigCount{
			Config: cfg, Pods: n, HourlyUSD: h, MonthlyUSD: h * pricing.HoursPerMonth,
		})
	}
	out.MonthlyUSD = out.HourlyUSD * pricing.HoursPerMonth
	return out
}

// ec2Side packs the pod set onto the cheapest feasible node set and prices it.
func ec2Side(pods []Pod, dsPods []model.PodSpec, catalog *pricing.Catalog, opts Options, warnings *[]string) EC2Side {
	out := EC2Side{DaemonSetTemplates: len(dsPods)}
	candidates := opts.Candidates
	if candidates == nil {
		provider := opts.Provider
		if provider == "" {
			provider = DefaultProvider
		}
		candidates = catalog.Candidates(provider, opts.Arch)
		if len(candidates) == 0 {
			*warnings = append(*warnings, fmt.Sprintf(
				"ec2 side unpriced: the catalog holds no %s instance types for arch %q",
				provider, opts.Arch))
		}
	}
	if len(pods) == 0 {
		return out
	}
	specs := make([]*model.PodSpec, 0, len(pods))
	for i := range pods {
		specs = append(specs, &pods[i].Spec)
	}
	plan := binpack.PlanNodes(specs, candidates, binpack.PlanOptions{
		SystemReservedFraction: opts.SystemReservedFraction,
		MaxPodsPerNode:         opts.MaxPodsPerNode,
		DaemonSetPods:          dsPods,
		NodeLabels:             opts.NodeLabels,
		Spot:                   opts.Spot,
		AllowBurstable:         opts.AllowBurstable,
		MaxNodes:               opts.MaxNodes,
	})
	out.Nodes = len(plan.Nodes)
	out.HourlyUSD = plan.TotalHourlyUSD
	out.MonthlyUSD = plan.TotalHourlyUSD * pricing.HoursPerMonth
	type typeKey struct {
		provider, name string
	}
	counts := make(map[typeKey]*NodeTypeCount)
	for _, n := range plan.Nodes {
		out.Purchased = out.Purchased.Add(n.Type.Resources())
		out.Allocatable = out.Allocatable.Add(n.Allocatable)
		out.WorkloadRequests = out.WorkloadRequests.Add(n.Used)
		k := typeKey{n.Type.Provider, n.Type.Name}
		c := counts[k]
		if c == nil {
			c = &NodeTypeCount{Provider: n.Type.Provider, Name: n.Type.Name, Arch: n.Type.Arch, Spot: n.Spot}
			counts[k] = c
		}
		c.Nodes++
		c.HourlyUSD += n.HourlyUSD
	}
	for _, c := range counts {
		c.MonthlyUSD = c.HourlyUSD * pricing.HoursPerMonth
		out.NodeTypes = append(out.NodeTypes, *c)
	}
	sort.Slice(out.NodeTypes, func(i, j int) bool {
		a, b := out.NodeTypes[i], out.NodeTypes[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Name < b.Name
	})
	for _, u := range plan.Unschedulable {
		if u.Pod == nil {
			continue
		}
		out.Unschedulable = append(out.Unschedulable, podKey(u.Pod)+": "+strings.Join(u.Reasons, "; "))
	}
	sort.Strings(out.Unschedulable)
	out.Unschedulable = capPods(out.Unschedulable)
	out.Feasible = len(plan.Unschedulable) == 0 && out.Nodes > 0
	return out
}

// density computes u and u*. Both sides must be real numbers for u* to mean
// anything; an unpriceable side leaves Density.Defined false rather than
// producing a break-even out of a zero.
func density(e EC2Side, f FargateSide) Density {
	purchased := resourceValueUSD(e.Purchased)
	if !finitePositive(purchased) || e.Nodes == 0 {
		return Density{}
	}
	d := Density{
		Defined:  true,
		Achieved: resourceValueUSD(e.WorkloadRequests) / purchased,
	}
	if alloc := resourceValueUSD(e.Allocatable); finiteNonNegative(alloc) {
		d.SystemReservedFraction = (purchased - alloc) / purchased
		d.UnusedAllocatableFraction = (alloc - resourceValueUSD(e.WorkloadRequests)) / purchased
	}
	if finitePositive(f.HourlyUSD) && finitePositive(e.HourlyUSD) {
		d.BreakEven = d.Achieved * e.HourlyUSD / f.HourlyUSD
	}
	if !finiteNonNegative(d.Achieved) || !finiteNonNegative(d.BreakEven) {
		return Density{}
	}
	return d
}

// decide sets the verdict and the savings. It is the only place a Fargate
// recommendation can be produced, and it cannot produce one while a block
// stands.
func decide(rep *Report, podCount int) {
	blocked := len(rep.Blocks) > 0
	fargateOK := rep.Fargate.Eligible && finiteNonNegative(rep.Fargate.HourlyUSD)
	ec2OK := rep.EC2.Feasible && finiteNonNegative(rep.EC2.HourlyUSD)

	// The ordering of this switch is the enforcement of the unit's central
	// rule: every blocked case is decided before any case that compares
	// prices, so no execution path exists on which a block and a Fargate
	// recommendation coexist. (Fargate.Eligible is independently false while a
	// block stands, so it is guarded twice.) FuzzReportNeverRecommendsBlocked-
	// Fargate asserts the property from the outside.
	switch {
	case podCount == 0:
		rep.Verdict = VerdictUndecided
		rep.Warnings = append(rep.Warnings, "empty pod set: nothing to compare")
	case blocked && ec2OK:
		rep.Verdict = VerdictFargateBlocked
	case blocked:
		rep.Verdict = VerdictUndecided
		rep.Warnings = append(rep.Warnings,
			"neither side can run this pod set: Fargate is blocked and the EC2 candidates cannot hold it")
	case fargateOK && !ec2OK:
		rep.Verdict = VerdictFargate
		rep.Warnings = append(rep.Warnings,
			"the EC2 side could not hold this pod set, so Fargate wins on feasibility, not on price: no saving is claimed")
	case fargateOK && ec2OK:
		f, e := rep.Fargate.HourlyUSD, rep.EC2.HourlyUSD
		switch {
		case moneyEqual(f, e):
			rep.Verdict = VerdictTie
		case f < e:
			rep.Verdict = VerdictFargate
			rep.MonthlySavingsUSD = (e - f) * pricing.HoursPerMonth
			rep.SavingsFraction = (e - f) / e
		default:
			rep.Verdict = VerdictEC2
			rep.MonthlySavingsUSD = (f - e) * pricing.HoursPerMonth
			rep.SavingsFraction = (f - e) / f
		}
		rep.Close = rep.SavingsFraction < CloseMarginFraction
	default:
		rep.Verdict = VerdictUndecided
	}
	if blocked {
		// Nothing above can have set these, but stating it once makes the
		// invariant local: a blocked report claims no saving, ever.
		rep.MonthlySavingsUSD, rep.SavingsFraction, rep.Close = 0, 0, false
	}
}

// assumptions states, in fixed order, what the two numbers do and do not model.
func assumptions(opts Options, e EC2Side) []string {
	out := []string{
		"steady state: both sides are billed for " + fmt.Sprintf("%d", int(pricing.HoursPerMonth)) +
			" h/month of continuously running pods. Fargate's per-second billing advantage for bursty, batch and scale-to-zero workloads is NOT modelled, so this report understates Fargate for those classes",
		"EKS Fargate is x86 Linux on-demand only: no Fargate Spot, no Graviton, no Reserved Instances (both verified, §4.2)",
		"commitments are not netted: both sides are priced at list. A Reserved Instance or Savings Plan already covering the node side makes a move away from it worse than shown (§4.4)",
		"the EKS control-plane fee is identical on both sides and is excluded",
		"Fargate ephemeral storage is not priced: 20 GB per pod is free and pkg/model carries no ephemeral-storage request",
	}
	if opts.Spot {
		out = append(out, "the EC2 side is priced at spot rates, which are interruptible and volatile; Fargate on EKS has no spot equivalent")
	}
	for _, t := range e.NodeTypes {
		if t.Arch == "arm64" {
			out = append(out, "the EC2 side buys Graviton (arm64) shapes, which EKS Fargate cannot offer; binary compatibility is not observable from metrics and is the reader's to verify (§4.5)")
			break
		}
	}
	return out
}

// canonicalize returns the pod set in a deterministic order with unique UIDs.
//
// Order is by (UID, namespace, name, requests) rather than by input position,
// so a shuffled snapshot produces a byte-identical report. Terminal pods are
// dropped from both sides — they hold no node capacity — with a note, because
// on Fargate a completed Job pod left running does keep billing (§3.2) and
// that is a different finding, not this one.
func canonicalize(in []Pod) (pods []Pod, notes []string) {
	kept := make([]Pod, 0, len(in))
	terminal := 0
	for _, p := range in {
		if isTerminal(&p.Spec) {
			terminal++
			continue
		}
		kept = append(kept, p)
	}
	sort.SliceStable(kept, func(i, j int) bool { return lessPod(kept[i].Spec, kept[j].Spec) })
	seen := make(map[string]bool, len(kept))
	dropped := 0
	pods = make([]Pod, 0, len(kept))
	for i := range kept {
		if kept[i].Spec.UID == "" {
			kept[i].Spec.UID = fmt.Sprintf("crossover:unidentified:%06d", i)
		}
		if seen[kept[i].Spec.UID] {
			dropped++
			continue
		}
		seen[kept[i].Spec.UID] = true
		pods = append(pods, kept[i])
	}
	if terminal > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d terminal pod(s) excluded from both sides; note that on Fargate a completed pod keeps billing until it is deleted (§3.2, ttlSecondsAfterFinished)", terminal))
	}
	if dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d pod(s) dropped: duplicate UID", dropped))
	}
	return pods, notes
}

// canonicalDaemonSets deduplicates overhead templates: one per DaemonSet,
// sorted, because each is replicated onto every planned node.
func canonicalDaemonSets(in []model.PodSpec) (out []model.PodSpec, notes []string) {
	kept := make([]model.PodSpec, 0, len(in))
	terminal := 0
	for _, p := range in {
		if isTerminal(&p) {
			terminal++
			continue
		}
		kept = append(kept, p)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if a, b := kept[i].Workload.String(), kept[j].Workload.String(); a != b {
			return a < b
		}
		return lessPod(kept[i], kept[j])
	})
	seen := make(map[model.WorkloadRef]bool, len(kept))
	collapsed := 0
	for i := range kept {
		ref := kept[i].Workload
		if ref != (model.WorkloadRef{}) {
			if seen[ref] {
				collapsed++
				continue
			}
			seen[ref] = true
		}
		out = append(out, kept[i])
	}
	if terminal > 0 {
		notes = append(notes, fmt.Sprintf("%d terminal DaemonSet pod(s) ignored as overhead", terminal))
	}
	if collapsed > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d DaemonSet pod(s) collapsed into per-DaemonSet templates: a DaemonSet is replicated once per planned node, not once per observed pod", collapsed))
	}
	return out, notes
}

func lessPod(a, b model.PodSpec) bool {
	if a.UID != b.UID {
		return a.UID < b.UID
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	ar, br := a.Requests(), b.Requests()
	if ar.MilliCPU != br.MilliCPU {
		return ar.MilliCPU < br.MilliCPU
	}
	return ar.MemoryBytes < br.MemoryBytes
}

// isTerminal mirrors plan.isTerminal: a Succeeded or Failed pod holds no node
// resources and cannot be disrupted, so planning treats it as absent.
func isTerminal(p *model.PodSpec) bool {
	return p.Phase == "Succeeded" || p.Phase == "Failed"
}

// resourceValueUSD collapses a two-dimensional shape into one number using the
// same fixed exchange rate pkg/binpack uses for packing efficiency. It is only
// ever a ratio's numerator and denominator, never reported as spend: changing
// the rate moves u and u* together and leaves u/u* = F/E untouched.
func resourceValueUSD(r model.Resources) float64 {
	return float64(max(r.MilliCPU, 0))/1000*pricing.FallbackCPUHourlyUSD +
		float64(max(r.MemoryBytes, 0))/(1<<30)*pricing.FallbackGiBHourlyUSD
}

// moneyEqual compares money without ==: absolute for near-zero amounts,
// relative for real ones.
func moneyEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	diff := math.Abs(a - b)
	if diff <= moneyEps {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return diff <= moneyEps*scale
}

func finitePositive(v float64) bool    { return v > 0 && !math.IsInf(v, 1) && !math.IsNaN(v) }
func finiteNonNegative(v float64) bool { return v >= 0 && !math.IsInf(v, 1) && !math.IsNaN(v) }
